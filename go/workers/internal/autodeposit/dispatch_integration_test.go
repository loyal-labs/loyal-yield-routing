package autodeposit

import (
	"testing"
	"time"
)

func TestDispatchReturnsOneFreshRowPerTarget(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	seeded := seedIntegrationTarget(t, store, "one-row")
	seedProjectedSurplus(t, store, seeded, 1, 9_000_000)
	store.insertIntegrationEvent(t, seeded.TargetID, 2, 9_000_000, nil, time.Now().Add(-time.Hour))
	var requestedSlot int64
	if err := store.pool.QueryRow(ctx, `
INSERT INTO loyal_yield.balance_sweep_scheduled_slots (target_id, token_mint, eligible_after, status, requested_at)
VALUES ($1, $2, now() - interval '1 minute', 'requested', now()) RETURNING id`, seeded.TargetID, USDCMint).Scan(&requestedSlot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.balance_sweep_surplus_lots
    (target_id, source_event_id, original_amount_raw, remaining_amount_raw, classification, eligible_after, reason, scheduled_slot_id)
VALUES ($1, 2, 1000000, 1000000, 'simple_inbound', now() - interval '1 minute', 'itest', $2)`, seeded.TargetID, requestedSlot); err != nil {
		t.Fatal(err)
	}

	targets, err := store.LoadExecutableTargets(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].TargetID != seeded.TargetID || targets[0].ScheduledSlotID != requestedSlot {
		t.Fatalf("dispatch rows %+v, want the requested slot %d once for target %d", targets, requestedSlot, seeded.TargetID)
	}
}
