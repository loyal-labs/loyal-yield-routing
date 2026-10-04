package autodeposit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Pinned Apps495 floor CTE rendered using its Drizzle physical column names
// and scalar PostgreSQL placeholders. This executes the existing writer's SQL
// contract; authentication remains in its retained HTTP/mobile handlers.
// Repository source SHA256: 9caac874f3d919b8959fab616e311f48b7c09fb5c9486acdf36a32891e26af6d
const retainedAppsFloorSQL = `WITH locked_target AS (
      SELECT loyal_yield.balance_sweep_targets.id
      FROM loyal_yield.balance_sweep_targets
      WHERE loyal_yield.balance_sweep_targets.id = $1
        AND loyal_yield.balance_sweep_targets.policy_account = $2
        AND loyal_yield.balance_sweep_targets.settings = $3
        AND loyal_yield.balance_sweep_targets.wallet = $4
        AND loyal_yield.balance_sweep_targets.vault_index = $5
        AND loyal_yield.balance_sweep_targets.desired_active = true
        AND loyal_yield.balance_sweep_targets.chain_status = 'active'
        AND loyal_yield.balance_sweep_targets.recurring_delegation = $6
      FOR UPDATE
    ),
    updated_target AS (
      UPDATE loyal_yield.balance_sweep_targets
      SET wallet_balance_floor_raw = $7
      WHERE loyal_yield.balance_sweep_targets.id IN (SELECT id FROM locked_target)
      RETURNING
        loyal_yield.balance_sweep_targets.id,
        loyal_yield.balance_sweep_targets.wallet,
        loyal_yield.balance_sweep_targets.wallet_token_ata,
        loyal_yield.balance_sweep_targets.wallet_usdc_ata
    ),
    suppressed_lots AS (
      UPDATE loyal_yield.balance_sweep_surplus_lots
      SET
        status = 'suppressed'::loyal_yield.balance_sweep_surplus_lot_status,
        updated_at = $8
      WHERE loyal_yield.balance_sweep_surplus_lots.target_id IN (SELECT id FROM updated_target)
        AND loyal_yield.balance_sweep_surplus_lots.status = 'open'::loyal_yield.balance_sweep_surplus_lot_status
        AND loyal_yield.balance_sweep_surplus_lots.remaining_amount_raw > 0
      RETURNING loyal_yield.balance_sweep_surplus_lots.id
    ),
    projection AS (
      SELECT current_balance.*
      FROM loyal_yield.balance_sweep_wallet_balances_current current_balance
      INNER JOIN updated_target
        ON updated_target.id = current_balance.target_id
    ),
    inserted_event AS (
      INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
        event_id,
        target_id,
        wallet,
        wallet_usdc_ata,
        wallet_token_ata,
        mint,
        previous_amount_raw,
        amount_raw,
        delta_amount_raw,
        observed_slot,
        observed_at,
        source,
        source_commitment,
        txn_signature,
        account_data_hash,
        raw_evidence,
        projected_at
      )
      SELECT
        nextval('loyal_yield.balance_sweep_floor_rebaseline_event_id_seq'::regclass)::bigint,
        projection.target_id,
        projection.wallet,
        projection.wallet_usdc_ata,
        projection.wallet_token_ata,
        projection.mint,
        projection.amount_raw,
        projection.amount_raw,
        0,
        projection.observed_slot,
        projection.observed_at,
        'app_autodeposit_floor_rebaseline',
        projection.source_commitment,
        NULL,
        projection.account_data_hash,
        jsonb_build_object(
          'floorRebaseline', true,
          'previousWalletBalanceFloorRaw', $9::text,
          'walletBalanceFloorRaw', $7::text,
          'suppressedOpenLotCount', (SELECT COUNT(*) FROM suppressed_lots)
        ),
        $8
      FROM projection
      WHERE projection.amount_raw > $7
      RETURNING event_id
    ),
    candidate_slot AS (
      SELECT slot.id
      FROM loyal_yield.balance_sweep_scheduled_slots AS slot
      INNER JOIN projection
        ON projection.target_id = slot.target_id
      CROSS JOIN inserted_event
      WHERE slot.token_mint = projection.mint
        AND slot.status = 'scheduled'
      ORDER BY slot.eligible_after ASC, slot.id ASC
      LIMIT 1
    ),
    updated_slot AS (
      UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
      SET eligible_after = GREATEST(slot.eligible_after, $10),
          updated_at = $8
      WHERE slot.id IN (SELECT id FROM candidate_slot)
      RETURNING
        slot.id,
        slot.eligible_after,
        slot.status::text AS status
    ),
    inserted_slot AS (
      INSERT INTO loyal_yield.balance_sweep_scheduled_slots (
        target_id,
        token_mint,
        eligible_after,
        status,
        created_at,
        updated_at
      )
      SELECT
        projection.target_id,
        projection.mint,
        $10,
        'scheduled',
        $8,
        $8
      FROM projection
      CROSS JOIN inserted_event
      WHERE NOT EXISTS (SELECT 1 FROM updated_slot)
      RETURNING
        id,
        eligible_after,
        status::text AS status
    ),
    scheduled_slot AS (
      SELECT id, eligible_after, status FROM updated_slot
      UNION ALL
      SELECT id, eligible_after, status FROM inserted_slot
      LIMIT 1
    ),
    inserted_lot AS (
      INSERT INTO loyal_yield.balance_sweep_surplus_lots (
        target_id,
        scheduled_slot_id,
        source_event_id,
        source_signature,
        original_amount_raw,
        remaining_amount_raw,
        classification,
        eligible_after,
        status,
        confidence,
        reason,
        created_at,
        updated_at
      )
      SELECT
        projection.target_id,
        scheduled_slot.id,
        inserted_event.event_id,
        NULL,
        projection.amount_raw - $7,
        projection.amount_raw - $7,
        'floor_rebaseline'::loyal_yield.balance_sweep_surplus_classification,
        $10,
        'open'::loyal_yield.balance_sweep_surplus_lot_status,
        'confirmed_projection',
        'Autodeposit floor update rebaseline',
        $8,
        $8
      FROM projection
      CROSS JOIN inserted_event
      CROSS JOIN scheduled_slot
      RETURNING
        classification::text AS "lotClassification",
        confidence AS "lotConfidence",
        eligible_after AS "lotEligibleAfter",
        id AS "lotId",
        scheduled_slot_id AS "lotSlotId",
        original_amount_raw AS "lotOriginalAmountRaw",
        reason AS "lotReason",
        remaining_amount_raw AS "lotRemainingAmountRaw",
        (SELECT status FROM scheduled_slot) AS "lotStatus"
    )
    SELECT
      projection.amount_raw AS "projectionAmountRaw",
      inserted_lot."lotClassification",
      inserted_lot."lotConfidence",
      inserted_lot."lotEligibleAfter",
      inserted_lot."lotId",
      inserted_lot."lotSlotId",
      inserted_lot."lotOriginalAmountRaw",
      inserted_lot."lotReason",
      inserted_lot."lotRemainingAmountRaw",
      inserted_lot."lotStatus",
      CASE
        WHEN projection.target_id IS NULL THEN 'wallet_balance_projection_missing'
        WHEN projection.amount_raw <= $7 THEN 'wallet_balance_at_or_below_floor'
        ELSE NULL
      END AS "skippedReason"
    FROM updated_target
    LEFT JOIN projection ON true
    LEFT JOIN inserted_lot ON true`

func applyRetainedAppsFloor(ctx context.Context, s *Store, t ControlTarget, floor, previous int64, now, due time.Time) error {
	_, err := s.pool.Exec(ctx, retainedAppsFloorSQL, t.TargetID, t.Policy, t.Settings, t.Wallet, int16(1), t.RecurringDelegation, floor, now, fmt.Sprint(previous), due)
	return err
}

func TestAppsFloorWriterCoexistsWithGoGenerationBootstrap(t *testing.T) {
	s := integrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	target := seedControlTarget(t, s, "apps-floor")
	start := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Second)
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET start_timestamp=$2 WHERE id=$1`, target.TargetID, start.Unix()); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadControlTarget(ctx, target.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	target = *loaded
	observation := ControlObservation{Target: target, ObservedSlot: 990001, PolicyExists: true, DelegationExists: true, PolicyValid: true, AuthorityValid: true, DelegationValid: true, TokenDelegateValid: true, WalletBalanceRaw: 9000000, WalletAccountDataSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	request := claimControlRequest(t, s, target.TargetID, 990001, "apps-control-first")
	if err := s.ApplyControlObservation(ctx, request, "apps-control-first", observation); err != nil {
		t.Fatal(err)
	}
	var generation, bootstrap, activationEvent, originalSlot int64
	if err := s.pool.QueryRow(ctx, `SELECT t.setup_generation,t.bootstrap_generation,e.event_id,l.scheduled_slot_id FROM loyal_yield.balance_sweep_targets t JOIN loyal_yield.balance_sweep_surplus_lots l ON l.target_id=t.id JOIN loyal_yield.balance_sweep_wallet_balance_events e ON e.event_id=l.source_event_id WHERE t.id=$1`, target.TargetID).Scan(&generation, &bootstrap, &activationEvent, &originalSlot); err != nil {
		t.Fatal(err)
	}
	if activationEvent < -2999999999999 || activationEvent > -2000000000000 {
		t.Fatal("Go activation escaped registered event range")
	}
	// An old reader's current projection participates in the same row-locked
	// writer. Two floor writes coalesce into the source scheduled slot and never
	// change user enablement/lifecycle. The source suppression predicate only
	// targets open lots; selected custody is tested independently below.
	now := time.Now().UTC()
	if err := applyRetainedAppsFloor(ctx, s, target, 6000000, 4000000, now, start); err != nil {
		t.Fatal(err)
	}
	if err := applyRetainedAppsFloor(ctx, s, target, 5000000, 6000000, now.Add(time.Minute), start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var active bool
	var status string
	var newGeneration, newBootstrap, floor int64
	if err := s.pool.QueryRow(ctx, `SELECT desired_active,chain_status,setup_generation,bootstrap_generation,wallet_balance_floor_raw FROM loyal_yield.balance_sweep_targets WHERE id=$1`, target.TargetID).Scan(&active, &status, &newGeneration, &newBootstrap, &floor); err != nil {
		t.Fatal(err)
	}
	if !active || status != "active" || newGeneration != generation || newBootstrap != bootstrap || floor != 5000000 {
		t.Fatalf("floor changed unrelated owner state active=%v status=%s generation=%d bootstrap=%d floor=%d", active, status, newGeneration, newBootstrap, floor)
	}
	var slots, openLots, suppressed int
	var due time.Time
	var amount int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='open'),count(*) FILTER(WHERE status='suppressed'),sum(remaining_amount_raw) FILTER(WHERE status='open') FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=$1`, target.TargetID).Scan(&openLots, &suppressed, &amount); err != nil {
		t.Fatal(err)
	}
	if openLots != 1 || suppressed != 2 || amount != 4000000 {
		t.Fatalf("source floor baseline lots open=%d suppressed=%d amount=%d", openLots, suppressed, amount)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*),max(eligible_after) FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id=$1 AND status='scheduled'`, target.TargetID).Scan(&slots, &due); err != nil || slots != 1 || due.Before(start.Add(time.Minute)) {
		t.Fatalf("scheduled start/coalescing slots=%d due=%s error=%v", slots, due, err)
	}
	var floorEvents int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balance_events WHERE target_id=$1 AND source='app_autodeposit_floor_rebaseline' AND event_id BETWEEN -1999999999999 AND -1000000000000`, target.TargetID).Scan(&floorEvents); err != nil || floorEvents != 2 {
		t.Fatalf("App producer range %d %v", floorEvents, err)
	}
	// A subsequent source-generation control pass cannot bootstrap again over
	// the floor writer's exact new baseline or regress the projection frontier.
	request = claimControlRequest(t, s, target.TargetID, 990002, "apps-control-after-floor")
	observation.ObservedSlot = 990002
	if err := s.ApplyControlObservation(ctx, request, "apps-control-after-floor", observation); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='open'),sum(remaining_amount_raw) FILTER(WHERE status='open') FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=$1`, target.TargetID).Scan(&openLots, &amount); err != nil || openLots != 1 || amount != 4000000 {
		t.Fatalf("Go recreated obsolete bootstrap %d %d %v", openLots, amount, err)
	}
	// Actual Apps SQL and native control contend on the same target lock. The
	// order may vary; neither ordering may revive generation-3 bootstrap lots.
	request = claimControlRequest(t, s, target.TargetID, 990003, "apps-control-concurrent")
	observation.ObservedSlot = 990003
	begin := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-begin
		results <- s.ApplyControlObservation(ctx, request, "apps-control-concurrent", observation)
	}()
	go func() {
		<-begin
		results <- applyRetainedAppsFloor(ctx, s, target, 6000000, 5000000, now.Add(2*time.Minute), start.Add(2*time.Minute))
	}()
	close(begin)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='open'),sum(remaining_amount_raw) FILTER(WHERE status='open') FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=$1`, target.TargetID).Scan(&openLots, &amount); err != nil || openLots != 1 || amount != 3000000 {
		t.Fatalf("concurrent owners revived obsolete baseline %d %d %v", openLots, amount, err)
	}
	// A pre-existing selected lot is durable custody, not a source floor baseline
	// to suppress. This tests state compatibility, not receipt/financial success.
	var selectedID, selectedRemaining int64
	if err := s.pool.QueryRow(ctx, `UPDATE loyal_yield.balance_sweep_surplus_lots SET status='selected' WHERE target_id=$1 AND status='open' RETURNING id,remaining_amount_raw`, target.TargetID).Scan(&selectedID, &selectedRemaining); err != nil {
		t.Fatal(err)
	}
	if err := applyRetainedAppsFloor(ctx, s, target, 7000000, 6000000, now.Add(3*time.Minute), start.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var selectedStatus string
	var remaining int64
	if err := s.pool.QueryRow(ctx, `SELECT status::text,remaining_amount_raw FROM loyal_yield.balance_sweep_surplus_lots WHERE id=$1`, selectedID).Scan(&selectedStatus, &remaining); err != nil || selectedStatus != "selected" || remaining != selectedRemaining {
		t.Fatalf("floor writer rewrote selected custody %s %d %v", selectedStatus, remaining, err)
	}

}
