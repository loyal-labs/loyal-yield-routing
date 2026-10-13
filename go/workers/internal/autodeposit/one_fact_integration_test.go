package autodeposit

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

// A requested slot is a "Deposit now" being executed, not a place for new
// lots: an inflow opens the target's scheduled slot beside it and leaves the
// request's due date alone, so the request is neither pushed nor timed out.
func TestNewLotBesideARequestedSlotOpensTheScheduledSlot(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	seeded := seedIntegrationTarget(t, s, "requested-beside")
	seedProjectedSurplus(t, s, seeded, 9_810_001, 9_000_000)
	requested := openSlot(t, s, seeded.TargetID)
	due := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_scheduled_slots SET status='requested',request_source='web_execute_now',requested_at=now(),eligible_after=$2 WHERE id=$1`, requested, due); err != nil {
		t.Fatal(err)
	}
	s.insertIntegrationEvent(t, seeded.TargetID, 9_810_002, 11_000_000, ptrInt64(2_000_000), time.Now())
	if _, err := s.ProjectSurplusLotsOnce(ctx, 100); err != nil {
		t.Fatal(err)
	}
	scheduled := openSlot(t, s, seeded.TargetID)
	var status string
	var stillDue time.Time
	var lotSlot int64
	if err := s.pool.QueryRow(ctx, `SELECT status::text, eligible_after, (SELECT scheduled_slot_id FROM loyal_yield.balance_sweep_surplus_lots WHERE source_event_id=9810002) FROM loyal_yield.balance_sweep_scheduled_slots WHERE id=$1`, requested).Scan(&status, &stillDue, &lotSlot); err != nil {
		t.Fatal(err)
	}
	if scheduled == requested || status != "requested" || !stillDue.Equal(due) || lotSlot != scheduled {
		t.Fatalf("requested %d (%s, due %s) scheduled %d lot on %d", requested, status, stillDue, scheduled, lotSlot)
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

// Two writers that each need the target's scheduled slot (the projector
// filing a lot, a release returning its slot) take turns on the target row and
// end on one slot; neither fails on the unique index and rolls back its batch.
func TestConcurrentScheduledSlotWritersShareOneSlot(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	race := func(t *testing.T, targetID int64, first func(pgx.Tx) (int64, error)) {
		t.Helper()
		a, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Rollback(ctx)
		want, err := first(a)
		if err != nil {
			t.Fatal(err)
		}
		type result struct {
			id  int64
			err error
		}
		done := make(chan result, 1)
		go func() {
			b, err := s.pool.Begin(ctx)
			if err != nil {
				done <- result{err: err}
				return
			}
			id, err := openSlotFor(ctx, b, targetID, time.Now())
			if err == nil {
				err = b.Commit(ctx)
			} else {
				_ = b.Rollback(ctx)
			}
			done <- result{id, err}
		}()
		select {
		case r := <-done:
			t.Fatalf("second writer did not wait for the first: %+v", r)
		case <-time.After(200 * time.Millisecond):
		}
		if err := a.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if r.err != nil || r.id != want {
			t.Fatalf("second writer got slot %d (%v), want the first writer's %d", r.id, r.err, want)
		}
		var scheduled int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id=$1 AND status='scheduled'`, targetID).Scan(&scheduled); err != nil || scheduled != 1 {
			t.Fatalf("scheduled slots=%d %v", scheduled, err)
		}
	}
	t.Run("two projector inserts", func(t *testing.T) {
		target := seedIntegrationTarget(t, s, "race-insert")
		s.insertIntegrationEvent(t, target.TargetID, 9_820_001, 9_000_000, nil, time.Now())
		// The first projector files its lot on the slot it opens.
		race(t, target.TargetID, func(tx pgx.Tx) (int64, error) {
			id, err := openSlotFor(ctx, tx, target.TargetID, time.Now())
			if err != nil {
				return 0, err
			}
			_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_surplus_lots (target_id, source_event_id, original_amount_raw, remaining_amount_raw, classification, eligible_after, status, confidence, reason, scheduled_slot_id)
VALUES ($1, 9820001, 5000000, 5000000, 'unknown', now(), 'open', 'derived', 'race', $2)`, target.TargetID, id)
			return id, err
		})
	})
	t.Run("release against a projector insert", func(t *testing.T) {
		target, claim, slot := selectedReleaseClaim(t, s, "race-release", 9_000_000)
		race(t, target.TargetID, func(tx pgx.Tx) (int64, error) {
			released, err := releaseClaimCustody(ctx, tx, claim, 0)
			if err == nil && !released {
				err = errors.New("claim was not released")
			}
			return slot, err
		})
	})
}

// A released "Deposit now" slot becomes the target's scheduled slot, so it is
// automatic work again: the user's request does not outlive its execution and
// a withdrawn user's skip ends it.
func TestReleasedRequestIsAutomaticWorkAgain(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	_, claim, slot := selectedReleaseClaim(t, s, "released-request", 9_000_000)
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_scheduled_slots SET request_source='web_execute_now', requested_at=now() WHERE id=$1`, slot); err != nil {
		t.Fatal(err)
	}
	if outcome, err := s.ReleaseClaimOnce(ctx, claim, "lease-current"); err != nil || outcome.Status != ClaimReleased {
		t.Fatalf("release=%+v %v", outcome, err)
	}
	var status string
	var source *string
	var requestedAt *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT status::text, request_source, requested_at FROM loyal_yield.balance_sweep_scheduled_slots WHERE id=$1`, slot).Scan(&status, &source, &requestedAt); err != nil {
		t.Fatal(err)
	}
	if status != "scheduled" || source != nil || requestedAt != nil {
		t.Fatalf("released slot %s source=%v requested_at=%v, want automatic scheduled work", status, source, requestedAt)
	}
	if skipped, err := s.SkipWithdrawnScheduledSlot(ctx, slot); err != nil || !skipped {
		t.Fatalf("withdrawn skip=%v %v", skipped, err)
	}
}
