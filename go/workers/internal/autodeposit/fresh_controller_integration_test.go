package autodeposit

import (
	"context"
	"errors"
	"testing"
)

// seedFreshControllerTarget seeds one projected 5,000,000 claimable slot on a
// target with a delegation and an active yield position, and returns its slot.
func seedFreshControllerTarget(t *testing.T, store *Store, name string) (integrationTarget, int64) {
	t.Helper()
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, name)
	var previousOffset int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(last_event_id),0) FROM loyal_yield.projection_offsets WHERE consumer_name=$1`, ConsumerName).Scan(&previousOffset); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `UPDATE loyal_yield.projection_offsets SET last_event_id=$2 WHERE consumer_name=$1`, ConsumerName, previousOffset)
	})
	seedProjectedSurplus(t, store, seeded, previousOffset+1, 9_000_000)
	if _, err := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation='itest-fresh-delegation' WHERE id=$1`, seeded.TargetID); err != nil {
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
 7,policy_account,7,'fresh-reserve','fresh-market',$2,
 $2,1,'fresh-reserve','fresh-market',$2,1,1,now(),
 'fresh-existing-deposit','fresh-existing-deposit',1,'active',now(),now()
FROM loyal_yield.balance_sweep_targets WHERE id=$1`, seeded.TargetID, USDCMint); err != nil {
		t.Fatal(err)
	}
	var slot int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id=$1`, seeded.TargetID).Scan(&slot); err != nil {
		t.Fatal(err)
	}
	return seeded, slot
}

// Exercises a fresh claim through both transaction legs against the registered
// SQL schema. RPC effects are explicit immutable receipts, not a live cluster.
func TestControllerFreshClaimCompletesWithoutAppRepair(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, slot := seedFreshControllerTarget(t, store, "fresh-controller")
	wallet, custody := "itest-wallet-usdc-fresh-controller", "itest-vault-usdc-fresh-controller"
	pull, topup := "itest-controller-pull-sig-fresh", "itest-controller-topup-sig-fresh"
	chain := &scriptedControllerChain{
		balances: map[string]int64{wallet: 9_000_000, custody: 0},
		observations: map[string]AttemptObservation{
			pull:  {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_001)},
			topup: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870_002)},
		},
		receipts: map[string]ReceiptEvidence{
			pull: {Signature: pull, Slot: 870_001, Effects: []ReceiptEffect{
				{TokenAccount: wallet, Mint: USDCMint, PreRaw: 9_000_000, PostRaw: 4_000_000},
				{TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: 5_000_000},
			}},
			topup: {Signature: topup, Slot: 870_002, Effects: []ReceiptEffect{
				{TokenAccount: custody, Mint: USDCMint, PreRaw: 5_000_000, PostRaw: 0},
				{TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 10, PostRaw: 5_000_010},
			}},
		},
		positions: map[string][2]int64{"fresh-reserve": {5_000_001, 870_002}},
	}
	wires := &scriptedControllerWires{suffix: "-fresh"}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: wires, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	code, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slot})
	if err != nil || code != ResultCompleted {
		t.Fatalf("fresh controller outcome=%v error=%v", code, err)
	}
	if len(wires.built) != 2 || wires.built[0] != "pull" || wires.built[1] != "top_up" {
		t.Fatalf("transaction legs = %v", wires.built)
	}
	var state string
	var walletPre, walletPost, custodyPre, custodyPost, completed, principal, position int64
	if err := store.pool.QueryRow(ctx, `
SELECT claim.status::text, execution.source_pre_balance_raw, execution.source_post_balance_raw,
 execution.destination_pre_balance_raw, execution.destination_post_balance_raw,
 (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
  WHERE attempt.claim_token=claim.claim_token AND attempt.attempt_state='confirmed' AND attempt.broadcast_count=1),
 yp.principal_amount_raw, yp.current_amount_raw
FROM loyal_yield.balance_sweep_lot_claims AS claim
JOIN loyal_yield.balance_sweep_executions AS execution ON execution.id=claim.execution_id
JOIN loyal_yield.user_yield_positions AS yp ON yp.id=execution.yield_position_id
WHERE claim.target_id=$1`, seeded.TargetID).Scan(&state, &walletPre, &walletPost, &custodyPre, &custodyPost, &completed, &principal, &position); err != nil {
		t.Fatal(err)
	}
	if state != "executed" || walletPre != 9_000_000 || walletPost != 4_000_000 || custodyPre != 0 || custodyPost != 5_000_000 || completed != 2 || principal != 5_000_001 || position != 5_000_001 {
		t.Fatalf("incorrect finalization: %s wallet=%d/%d custody=%d/%d completed=%d principal=%d position=%d", state, walletPre, walletPost, custodyPre, custodyPost, completed, principal, position)
	}
}

// KLend refuses a deposit that mints no collateral. A claim below the
// reserve's minimum deposit is released before the pull, so the wallet never
// funds custody that can never be deposited (production target 7702 pulled 1
// raw unit and its top-up failed with InvalidAmount on every retry).
func TestControllerReleasesClaimBelowMinimumDepositBeforePull(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded, slot := seedFreshControllerTarget(t, store, "below-minimum")
	chain := &scriptedControllerChain{balances: map[string]int64{
		"itest-wallet-usdc-below-minimum": 9_000_000, "itest-vault-usdc-below-minimum": 0,
	}}
	wires := &scriptedControllerWires{suffix: "-below-minimum", minimumDeposit: 5_000_001}
	controller, err := NewController(ControllerDependencies{Store: store, Chain: chain, Wires: wires, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	code, err := controller.Execute(ctx, ExecutableTarget{TargetID: seeded.TargetID, ScheduledSlotID: slot})
	if code != ResultPreflightBlocked || !errors.Is(err, ErrRouteNotExecutable) {
		t.Fatalf("outcome=%v error=%v, want a preflight block before the pull", code, err)
	}
	if len(wires.built) != 0 {
		t.Fatalf("built %v before refusing the amount", wires.built)
	}
	var status string
	var attempts int64
	if err := store.pool.QueryRow(ctx, `
SELECT claim.status::text,
 (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts AS attempt WHERE attempt.claim_token=claim.claim_token)
FROM loyal_yield.balance_sweep_lot_claims AS claim WHERE claim.target_id=$1`, seeded.TargetID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "released" || attempts != 0 {
		t.Fatalf("claim %s with %d attempts, want released with none", status, attempts)
	}
}
