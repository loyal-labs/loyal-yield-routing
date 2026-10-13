package autodeposit

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func requireConstraint(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("want SQLSTATE %s, got %v", code, err)
	}
}

// A delegation address without its nonce cannot be proven to be the target's
// PDA; 44 production targets held one and never deposited.
func TestDelegationWithoutNonceIsRefused(t *testing.T) {
	s := integrationStore(t)
	target := seedIntegrationTarget(t, s, "nonce-fact")
	_, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation='nonce-less' WHERE id=$1`, target.TargetID)
	requireConstraint(t, err, "23514")
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation='with-nonce',recurring_delegation_nonce=3 WHERE id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation_nonce=NULL WHERE id=$1`, target.TargetID)
	requireConstraint(t, err, "23514")
}

// A vault has one position: a second active row for the same (settings,
// vault_index) is refused whatever reserve it names.
func TestOneActivePositionPerVault(t *testing.T) {
	s := integrationStore(t)
	target := seedIntegrationTarget(t, s, "one-position")
	insert := func(reserve, status string) error {
		_, err := s.pool.Exec(t.Context(), `
INSERT INTO loyal_yield.user_yield_positions (
 wallet_address, smart_account_address, settings, vault_index, vault_pubkey,
 policy_id, policy_account, policy_seed, initial_reserve, initial_market, initial_liquidity_mint,
 deposit_mint, principal_amount_raw, current_reserve, current_market, current_liquidity_mint,
 current_amount_raw, current_observed_slot, current_observed_at,
 first_deposit_signature, last_deposit_signature, last_confirmed_slot, status, created_at, updated_at)
SELECT wallet, vault_pubkey, settings, vault_index, vault_pubkey, 7, policy_account, 7, $2, 'market', $3,
 $3, 1, $2, 'market', $3, 1, 1, now(), $2, $2, 1, $4::text::loyal_yield.yield_position_status, now(), now()
FROM loyal_yield.balance_sweep_targets WHERE id=$1`, target.TargetID, reserve, USDCMint, status)
		return err
	}
	if err := insert("reserve-a", "active"); err != nil {
		t.Fatal(err)
	}
	if err := insert("reserve-closed", "closed"); err != nil {
		t.Fatal(err)
	}
	requireConstraint(t, insert("reserve-b", "active"), "23505")
}

// A slot whose lots all drained is not reused: the next lot opens a fresh
// slot, so an old idle block or deferral never applies to new money.
func TestNewLotOnDrainedSlotOpensAFreshSlot(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	seeded := seedIntegrationTarget(t, s, "drained-slot")
	seedProjectedSurplus(t, s, seeded, 9_800_001, 9_000_000)
	old := openSlot(t, s, seeded.TargetID)
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_scheduled_slots SET idle_blocked_since=now()-interval '30 days',eligible_after=now()+interval '1 day' WHERE id=$1`, old); err != nil {
		t.Fatal(err)
	}
	// The wallet spends the surplus: the old slot's lots drain.
	s.insertIntegrationEvent(t, seeded.TargetID, 9_800_002, 4_000_000, ptrInt64(-5_000_000), time.Now())
	// A new inflow arrives.
	s.insertIntegrationEvent(t, seeded.TargetID, 9_800_003, 6_000_000, ptrInt64(2_000_000), time.Now())
	if _, err := s.ProjectSurplusLotsOnce(ctx, 100); err != nil {
		t.Fatal(err)
	}
	fresh := openSlot(t, s, seeded.TargetID)
	var oldStatus string
	var blocked *time.Time
	var lotSlot int64
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT status::text FROM loyal_yield.balance_sweep_scheduled_slots WHERE id=$1),
        (SELECT idle_blocked_since FROM loyal_yield.balance_sweep_scheduled_slots WHERE id=$2),
        (SELECT scheduled_slot_id FROM loyal_yield.balance_sweep_surplus_lots WHERE source_event_id=9800003)`, old, fresh).Scan(&oldStatus, &blocked, &lotSlot); err != nil {
		t.Fatal(err)
	}
	if fresh == old || oldStatus != "canceled" || blocked != nil || lotSlot != fresh {
		t.Fatalf("new lot inherited the drained slot: old=%d (%s) fresh=%d blocked=%v lot slot=%d", old, oldStatus, fresh, blocked, lotSlot)
	}
}

// A deferral moves the slot's due date like an idle block does, so a slot
// whose allowance cannot be read is not dispatched again every poll and the
// pass reads as settled meanwhile.
func TestDeferredSlotWaitsForItsDueDate(t *testing.T) {
	scenario := newFreshScenario(t, "deferred", 0)
	scenario.chain.allowanceErr = ErrAllowanceUnknown
	worker, _ := scenario.worker(t, ControllerDependencies{}, nil)
	first, err := worker.Tick(t.Context())
	if err != nil || len(first.Dispatched) != 1 || first.Outcome.ExecutionsDeferred != 1 || first.settled() {
		t.Fatalf("first pass: dispatched=%v outcome=%+v err=%v", first.Dispatched, first.Outcome, err)
	}
	var waits bool
	if err := scenario.store.pool.QueryRow(t.Context(), `SELECT eligible_after > now() + interval '4 minutes' FROM loyal_yield.balance_sweep_scheduled_slots WHERE id=$1`, scenario.slot).Scan(&waits); err != nil || !waits {
		t.Fatalf("deferred slot is due again before the retry delay: %v %v", waits, err)
	}
	second, err := worker.Tick(t.Context())
	if err != nil || len(second.Dispatched) != 0 || !second.settled() {
		t.Fatalf("deferred slot re-dispatched: dispatched=%v outcome=%+v err=%v", second.Dispatched, second.Outcome, err)
	}
	elapseReleaseDelay(t, scenario.store, scenario.target.TargetID)
	scenario.chain.allowanceErr = nil
	third, err := worker.Tick(t.Context())
	if err != nil || third.Outcome.ExecutionsCompleted != 1 {
		t.Fatalf("readable allowance did not complete: %+v %v", third.Outcome, err)
	}
}
