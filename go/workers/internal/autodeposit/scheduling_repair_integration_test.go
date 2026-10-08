package autodeposit

import (
	"context"
	"encoding/base64"
	"testing"
	"time"
)

func seedRepairPosition(t *testing.T, s *Store, target int64, mint string) {
	t.Helper()
	_, err := s.pool.Exec(t.Context(), `INSERT INTO loyal_yield.user_yield_positions (
wallet_address,smart_account_address,settings,vault_index,vault_pubkey,policy_id,policy_account,policy_seed,
initial_reserve,initial_market,initial_liquidity_mint,deposit_mint,principal_amount_raw,current_reserve,current_market,current_liquidity_mint,current_amount_raw,current_observed_slot,current_observed_at,first_deposit_signature,last_deposit_signature,last_confirmed_slot,status,created_at,updated_at)
SELECT wallet,vault_pubkey,settings,vault_index,vault_pubkey,7,policy_account,7,'repair-reserve','repair-market',$2,$2,1,'repair-reserve','repair-market',$2,1,1,now(),'repair-existing','repair-existing',1,'active',now(),now() FROM loyal_yield.balance_sweep_targets WHERE id=$1`, target, mint)
	if err != nil {
		t.Fatal(err)
	}
	seedTargetLiveReserve(t, s, target, "repair-reserve", "repair-market", 1, time.Now())
}

func TestUnsignedRepairNeverReopensPersistedWire(t *testing.T) {
	for _, state := range []string{"prepared", "unknown", "failed", "expired"} {
		t.Run(state, func(t *testing.T) {
			s := integrationStore(t)
			target, claim, slot := selectedReleaseClaim(t, s, "repair-wire-"+state)
			seedRepairPosition(t, s, target.TargetID, USDCMint)
			wire := base64.StdEncoding.EncodeToString([]byte("retained repair wire"))
			_, err := s.PersistPreparedAttempt(t.Context(), PreparedAttempt{ClaimToken: claim, TargetID: target.TargetID, ScheduledSlotID: slot, OperationKind: OperationPull, AmountRaw: 5_000_000, SourcePreBalanceRaw: 9_000_000, ProtectionFloorRaw: ptrInt64(4_000_000), Signature: "repair-wire-" + state, SignedTransactionBase64: wire, SignedTransactionSHA256: wireSHA256(wire), RecentBlockhash: "repair-hash", LastValidBlockHeight: 500}, "lease-current")
			if err != nil {
				t.Fatal(err)
			}
			// Inject a stale scheduler's erroneous release while retaining the
			// immutable packet. Even terminal status alone is not no-effect proof.
			if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_transaction_attempts SET attempt_state=$2 WHERE claim_token=$1`, claim, state); err != nil {
				t.Fatal(err)
			}
			if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_lot_claims SET status='released',autodeposit_executor_lease_token=NULL,autodeposit_executor_lease_expires_at=NULL WHERE claim_token=$1`, claim); err != nil {
				t.Fatal(err)
			}
			if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_scheduled_slots SET status='failed',claim_token=NULL WHERE id=$1`, slot); err != nil {
				t.Fatal(err)
			}
			if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_surplus_lots SET status='open',remaining_amount_raw=original_amount_raw WHERE target_id=$1`, target.TargetID); err != nil {
				t.Fatal(err)
			}
			w, err := NewWorker(WorkerDependencies{Store: s, Executor: &scriptedExecutor{}, Facts: testFacts(), FeePayer: fundedPayer{}})
			if err != nil {
				t.Fatal(err)
			}
			report, err := w.Tick(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Dispatched) != 0 {
				t.Fatalf("persisted %s wire produced fresh spend: %+v", state, report.Dispatched)
			}
			var slotState, actualWire string
			if err = s.pool.QueryRow(t.Context(), `SELECT slot.status::text,a.signed_transaction_base64 FROM loyal_yield.balance_sweep_scheduled_slots slot JOIN loyal_yield.balance_sweep_transaction_attempts a ON a.scheduled_slot_id=slot.id WHERE slot.id=$1`, slot).Scan(&slotState, &actualWire); err != nil {
				t.Fatal(err)
			}
			if slotState != "failed" || actualWire != wire {
				t.Fatalf("held wire changed: %s %s", slotState, actualWire)
			}
		})
	}
}

func TestWorkerRepairsUnsignedUnresolvedReserveWithoutAppReads(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	target := seedIntegrationTarget(t, s, "repair-controller")
	seedProjectedSurplus(t, s, target, 1, 9_000_000)
	// Two live holdings leave no destination to trust.
	seedTargetLiveReserve(t, s, target.TargetID, "ambiguous-a", "market-a", 3_000_000, time.Now())
	seedTargetLiveReserve(t, s, target.TargetID, "ambiguous-b", "market-b", 4_000_000, time.Now())
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation='repair-delegation' WHERE id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	wallet, custody := "itest-wallet-usdc-repair-controller", "itest-vault-usdc-repair-controller"
	pull, topup := "itest-controller-pull-sig-repair", "itest-controller-topup-sig-repair"
	chain := &scriptedControllerChain{balances: map[string]int64{wallet: 9_000_000, custody: 0}, observations: map[string]AttemptObservation{
		pull: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870001)}, topup: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870002)}}, receipts: map[string]ReceiptEvidence{
		pull:  {Signature: pull, Slot: 870001, Effects: []ReceiptEffect{{TokenAccount: wallet, Mint: USDCMint, PreRaw: 9_000_000, PostRaw: 4_000_000}, {TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: 5_000_000}}},
		topup: {Signature: topup, Slot: 870002, Effects: []ReceiptEffect{{TokenAccount: custody, Mint: USDCMint, PreRaw: 5_000_000, PostRaw: 0}, {TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 10, PostRaw: 5_000_010}}}}, positions: map[string][2]int64{"repair-reserve": {5_000_001, 870002}}}
	wires := &scriptedControllerWires{suffix: "-repair"}
	controller, err := NewController(ControllerDependencies{Store: s, Chain: chain, Wires: wires, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWorker(WorkerDependencies{Store: s, Executor: controller, Facts: testFacts(), FeePayer: fundedPayer{}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Dispatched) != 1 || len(wires.built) != 0 {
		t.Fatalf("unresolved reserve did not fail before signing: %+v %v", first, wires.built)
	}
	second, err := w.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Dispatched) != 0 {
		t.Fatalf("unresolved reserve retried: %+v", second)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE loyal_yield.vault_reserve_positions_current SET amount_raw=0,has_value=false WHERE reserve IN('ambiguous-a','ambiguous-b')`); err != nil {
		t.Fatal(err)
	}
	// Released lots wait out the TS pre-send cadence, and the repaired slot
	// inherits their deadline, instead of being claimed, released and alerted
	// again on every poll.
	seedRepairPosition(t, s, target.TargetID, USDCMint)
	if waiting, err := w.Tick(ctx); err != nil || len(waiting.Dispatched) != 0 {
		t.Fatalf("released work retried before its delay: %+v %v", waiting, err)
	}
	var waits bool
	if err = s.pool.QueryRow(ctx, `SELECT bool_and(eligible_after > now() + interval '4 minutes') FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id=$1 AND status='scheduled'`, target.TargetID).Scan(&waits); err != nil || !waits {
		t.Fatalf("repaired slot is eligible before the retry delay: %v %v", waits, err)
	}
	elapseReleaseDelay(t, s, target.TargetID)
	third, err := w.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third.Outcome.ExecutionsCompleted != 1 || len(wires.built) != 2 {
		t.Fatalf("resolved reserve did not complete autonomous retry: %+v %v", third, wires.built)
	}
	var completed, attempts int64
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM loyal_yield.balance_sweep_executions WHERE target_id=$1),(SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts WHERE target_id=$1)`, target.TargetID).Scan(&completed, &attempts); err != nil {
		t.Fatal(err)
	}
	if completed != 1 || attempts != 2 {
		t.Fatalf("duplicate spend: executions=%d attempts=%d", completed, attempts)
	}
}

func TestUnsignedRepairPreservesDeadlineRequestsAndUnknownFloor(t *testing.T) {
	for _, kind := range []string{"deadline", "requested", "null-floor", "paused", "live-lease"} {
		t.Run(kind, func(t *testing.T) {
			s := integrationStore(t)
			target := seedIntegrationTarget(t, s, "repair-"+kind)
			seedProjectedSurplus(t, s, target, 1, 9_000_000)
			seedRepairPosition(t, s, target.TargetID, USDCMint)
			var slot int64
			if err := s.pool.QueryRow(t.Context(), `SELECT id FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id=$1`, target.TargetID).Scan(&slot); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(30 * time.Minute).Truncate(time.Microsecond)
			if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_scheduled_slots SET status='failed',eligible_after=$2,request_source='web_execute_now',requested_at=now() WHERE id=$1`, slot, deadline); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "requested":
				_, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_scheduled_slots SET status='requested' WHERE id=$1`, slot)
				if err != nil {
					t.Fatal(err)
				}
			case "null-floor":
				_, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=NULL WHERE id=$1`, target.TargetID)
				if err != nil {
					t.Fatal(err)
				}
			case "paused":
				_, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, target.TargetID)
				if err != nil {
					t.Fatal(err)
				}
			case "live-lease":
				_, err := s.pool.Exec(t.Context(), `INSERT INTO loyal_yield.balance_sweep_lot_claims(claim_token,target_id,amount_raw,stale_check_event_id,status,autodeposit_executor_lease_token,autodeposit_executor_lease_expires_at) VALUES('repair-held',$1,1,1,'released','other-owner',now()+interval '1 minute')`, target.TargetID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RepairUnsignedSchedules(t.Context(), 100); err != nil {
				t.Fatal(err)
			}
			var state, source string
			var actual time.Time
			if err := s.pool.QueryRow(t.Context(), `SELECT status::text,eligible_after,request_source FROM loyal_yield.balance_sweep_scheduled_slots WHERE id=$1`, slot).Scan(&state, &actual, &source); err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if kind == "deadline" {
				want = "scheduled"
			}
			if kind == "requested" {
				want = "requested"
			}
			if state != want || !actual.Equal(deadline) || source != "web_execute_now" {
				t.Fatalf("repair changed protected schedule: %s %s %s", state, actual, source)
			}
		})
	}
}

func TestUnsignedStaleRepairClearsOnlyUnheldRows(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	target := seedIntegrationTarget(t, s, "stale-ui")
	seedProjectedSurplus(t, s, target, 1, 9_000_000)
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_scheduled_slots SET status='failed' WHERE target_id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_wallet_balances_current SET amount_raw=4000000,observed_slot=2 WHERE target_id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	w, err := NewWorker(WorkerDependencies{Store: s, Executor: &scriptedExecutor{}, Facts: testFacts(), FeePayer: fundedPayer{}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := w.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Dispatched) != 0 {
		t.Fatal("depleted wallet dispatched")
	}
	var state, lotState string
	if err = s.pool.QueryRow(ctx, `SELECT slot.status::text,lot.status::text FROM loyal_yield.balance_sweep_scheduled_slots slot JOIN loyal_yield.balance_sweep_surplus_lots lot ON lot.scheduled_slot_id=slot.id WHERE slot.target_id=$1`, target.TargetID).Scan(&state, &lotState); err != nil {
		t.Fatal(err)
	}
	if state != "canceled" || lotState != "suppressed" {
		t.Fatalf("stale pending schedule retained: %s/%s", state, lotState)
	}
}

// elapseReleaseDelay moves a target's released work to the end of its retry
// delay, as waiting PreSendRetryDelay would.
func elapseReleaseDelay(t *testing.T, s *Store, targetID int64) {
	t.Helper()
	for _, table := range []string{"balance_sweep_surplus_lots", "balance_sweep_scheduled_slots"} {
		if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.`+table+` SET eligible_after = LEAST(eligible_after, now()) WHERE target_id=$1`, targetID); err != nil {
			t.Fatal(err)
		}
	}
}
