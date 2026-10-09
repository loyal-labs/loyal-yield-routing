package autodeposit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// freshScenario is one fresh, fully executable slot whose vault custody already
// holds idleRaw, with scripted receipts for both legs.
type freshScenario struct {
	store  *Store
	target integrationTarget
	slot   int64
	chain  *scriptedControllerChain
	wires  *scriptedControllerWires
	wallet string
}

func newFreshScenario(t *testing.T, suffix string, idleRaw int64) freshScenario {
	t.Helper()
	store := integrationStore(t)
	ctx := t.Context()
	seeded := seedIntegrationTarget(t, store, suffix)
	seedProjectedSurplus(t, store, seeded, 1, 9_000_000)
	reserve := suffix + "-reserve"
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation='itest-delegation' WHERE id=$1`, seeded.TargetID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.user_yield_positions (
 wallet_address, smart_account_address, settings, vault_index, vault_pubkey,
 policy_id, policy_account, policy_seed, initial_reserve, initial_market, initial_liquidity_mint,
 deposit_mint, principal_amount_raw, current_reserve, current_market, current_liquidity_mint,
 current_amount_raw, current_observed_slot, current_observed_at,
 first_deposit_signature, last_deposit_signature, last_confirmed_slot, status, created_at, updated_at)
SELECT wallet,vault_pubkey,settings,vault_index,vault_pubkey,
 7,policy_account,7,$3,'itest-market',$2,
 $2,1,$3,'itest-market',$2,1,1,now(),
 $3,$3,1,'active',now(),now()
FROM loyal_yield.balance_sweep_targets WHERE id=$1`, seeded.TargetID, USDCMint, reserve); err != nil {
		t.Fatal(err)
	}
	seedLiveVaultPosition(t, store, seeded, reserve, "itest-market", 1, time.Now())
	wallet, custody := "itest-wallet-usdc-"+suffix, "itest-vault-usdc-"+suffix
	pull, topup := "itest-controller-pull-sig-"+suffix, "itest-controller-topup-sig-"+suffix
	chain := &scriptedControllerChain{
		balances: map[string]int64{wallet: 9_000_000, custody: idleRaw},
		observations: map[string]AttemptObservation{
			pull:  {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_001)},
			topup: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_002)},
		},
		receipts: map[string]ReceiptEvidence{
			pull: {Signature: pull, Slot: 870_001, Effects: []ReceiptEffect{
				{TokenAccount: wallet, Mint: USDCMint, PreRaw: 9_000_000, PostRaw: 4_000_000},
				{TokenAccount: custody, Mint: USDCMint, PreRaw: idleRaw, PostRaw: idleRaw + 5_000_000},
			}},
			topup: {Signature: topup, Slot: 870_002, Effects: []ReceiptEffect{
				{TokenAccount: custody, Mint: USDCMint, PreRaw: idleRaw + 5_000_000, PostRaw: idleRaw},
				{TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 10, PostRaw: 5_000_010},
			}},
		},
		positions: map[string][2]int64{reserve: {5_000_001, 870_002}},
	}
	var slot int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id=$1`, seeded.TargetID).Scan(&slot); err != nil {
		t.Fatal(err)
	}
	return freshScenario{store: store, target: seeded, slot: slot, chain: chain, wires: &scriptedControllerWires{suffix: "-" + suffix}, wallet: "itest-wallet-" + suffix}
}

func (f freshScenario) worker(t *testing.T, deps ControllerDependencies, notifier *SweepNotifier) (*Worker, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	facts := engine.NewFacts(registry)
	deps.Store, deps.Chain, deps.Wires, deps.Facts = f.store, f.chain, f.wires, facts
	controller, err := NewController(deps)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorker(WorkerDependencies{Store: f.store, Executor: controller, Facts: facts, FeePayer: controller, Notifier: notifier})
	if err != nil {
		t.Fatal(err)
	}
	return worker, registry
}

// signalExecutor reports every dispatched target.
type signalExecutor struct{ dispatched chan ExecutableTarget }

func (s signalExecutor) Execute(_ context.Context, target ExecutableTarget) (ExecutorResult, error) {
	s.dispatched <- target
	return ResultNoop, nil
}

// Root cause: the Rust trigger LISTENs on loyal_yield_autodeposit_wakeup so a
// user's requested slot runs at once; the Go worker never listened and polled
// every 60s. One wakeup now: the notification, with the poll as its timeout.
func TestRequestedSlotWakesTheWorkerBeforeThePoll(t *testing.T) {
	scenario := newFreshScenario(t, "wakeup", 0)
	executor := signalExecutor{dispatched: make(chan ExecutableTarget, 4)}
	worker, err := NewWorker(WorkerDependencies{Store: scenario.store, Executor: executor, Facts: testFacts(), FeePayer: fundedPayer{}, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("run ended with %v", err)
		}
	}()
	receive := func(what string) ExecutableTarget {
		select {
		case target := <-executor.dispatched:
			return target
		case <-time.After(10 * time.Second):
			t.Fatalf("no dispatch for %s", what)
		}
		return ExecutableTarget{}
	}
	receive("the first pass")
	deadline := time.Now().Add(10 * time.Second)
	for listening := false; !listening; {
		if time.Now().After(deadline) {
			t.Fatal("worker never listened")
		}
		if err := scenario.store.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE query = 'LISTEN '||$1)`, WakeupChannel).Scan(&listening); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scenario.store.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_scheduled_slots SET status='requested' WHERE id=$1`, scenario.slot); err != nil {
		t.Fatal(err)
	}
	if woken := receive("the requested slot"); woken.ScheduledSlotID != scenario.slot {
		t.Fatalf("woke for %+v, want slot %d", woken, scenario.slot)
	}
}
