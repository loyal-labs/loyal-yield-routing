package autodeposit

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

// integrationStore opens the disposable database this package's DB tests run
// against. Root's CI provisions the schema; locally the tests stay skipped
// unless AUTODEPOSIT_TEST_DATABASE_URL is set.
func integrationStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("AUTODEPOSIT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AUTODEPOSIT_TEST_DATABASE_URL is not set; database behavior tests need a disposable schema")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") ||
		(parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") || parsed.Port() == "" ||
		parsed.User == nil || parsed.User.Username() != "workers_v2" || parsed.Path != "/workers_v2_autodeposit" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatal("AUTODEPOSIT_TEST_DATABASE_URL must identify the isolated loopback workers_v2 fixture before connection")
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		t.Fatal("disposable Autodeposit tests exclude password credentials")
	}
	store, err := OpenStore(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open integration database: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.RequireSchema(context.Background()); err != nil {
		t.Fatalf("integration schema is not migrated: %v", err)
	}
	var databaseName, role string
	if err := store.pool.QueryRow(context.Background(), `SELECT current_database(),current_user`).Scan(&databaseName, &role); err != nil || databaseName != "workers_v2_autodeposit" || role != "workers_v2" {
		t.Fatalf("fixture database identity mismatch: database=%s role=%s error=%v", databaseName, role, err)
	}
	// This database belongs exclusively to the family test fixture. Reset at
	// each test boundary: legacy RESTRICT FKs deliberately retain financial
	// evidence, so best-effort cascading target cleanup is insufficient.
	// Deposits are retained by signature without a position FK. They must be
	// included explicitly or a later test replays an old accounting identity.
	if _, err := store.pool.Exec(context.Background(), `TRUNCATE loyal_yield.balance_sweep_targets,loyal_yield.managed_vaults,loyal_yield.route_policies,loyal_yield.user_yield_positions,loyal_yield.user_yield_position_deposits,loyal_yield.projection_offsets RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset disposable family fixture: %v", err)
	}
	return store
}

type integrationTarget struct {
	TargetID       int64
	ManagedVaultID int64
}

// seedIntegrationTarget installs one active target with its managed vault and a
// same_mint_kamino policy, so observed eligibility (not just desired
// enablement) holds. Rows are unique per test and removed afterwards.
func seedIntegrationTarget(t *testing.T, store *Store, suffix string) integrationTarget {
	t.Helper()
	ctx := context.Background()
	settings := fmt.Sprintf("itest-settings-%s", suffix)
	authority := fmt.Sprintf("itest-authority-%s", suffix)
	policyAccount := fmt.Sprintf("itest-policy-%s", suffix)
	vaultPubkey := fmt.Sprintf("itest-vault-%s", suffix)

	var policyID int64
	if err := store.pool.QueryRow(ctx, `
INSERT INTO loyal_yield.route_policies
    (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
     threshold, route_modes, active, last_seen_slot, last_seen_signature, cluster)
VALUES ($1, $2, 7, $3, 1, $4, 1, ARRAY['same_mint_kamino'], true, 1, 'itest', 'mainnet-beta')
RETURNING id`, settings, authority, policyAccount, vaultPubkey).Scan(&policyID); err != nil {
		t.Fatalf("seed route policy: %v", err)
	}
	var vaultID int64
	if err := store.pool.QueryRow(ctx, `
INSERT INTO loyal_yield.managed_vaults (settings, vault_index, vault_pubkey, active_policy_id, active)
VALUES ($1, 1, $2, $3, true)
RETURNING id`, settings, vaultPubkey, policyID).Scan(&vaultID); err != nil {
		t.Fatalf("seed managed vault: %v", err)
	}
	var targetID int64
	if err := store.pool.QueryRow(ctx, `
INSERT INTO loyal_yield.balance_sweep_targets
    (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
     wallet, wallet_usdc_ata, vault_usdc_ata, wallet_token_ata, vault_token_ata, token_mint, threshold,
     max_amount_per_period, desired_active, chain_status, wallet_balance_floor_raw,
     last_seen_slot, last_seen_signature, cluster)
VALUES ($1, $2, 7, $3, 1, $4, $5, $6, $7, $6, $7, $8, 1, 1000000000, true, 'active', 4000000,
        1, 'itest-seed', 'mainnet-beta')
RETURNING id`,
		settings, authority, policyAccount, vaultPubkey,
		fmt.Sprintf("itest-wallet-%s", suffix),
		fmt.Sprintf("itest-wallet-usdc-%s", suffix),
		fmt.Sprintf("itest-vault-usdc-%s", suffix),
		USDCMint,
	).Scan(&targetID); err != nil {
		t.Fatalf("seed balance sweep target: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(),
			`DELETE FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id = $1`, targetID)
		_, _ = store.pool.Exec(context.Background(),
			`DELETE FROM loyal_yield.balance_sweep_targets WHERE id = $1`, targetID)
		_, _ = store.pool.Exec(context.Background(),
			`DELETE FROM loyal_yield.managed_vaults WHERE id = $1`, vaultID)
		_, _ = store.pool.Exec(context.Background(),
			`DELETE FROM loyal_yield.route_policies WHERE id = $1`, policyID)
	})
	return integrationTarget{TargetID: targetID, ManagedVaultID: vaultID}
}

func (s *Store) insertIntegrationEvent(t *testing.T, targetID int64, eventID int64, amountRaw int64, delta *int64, observedAt time.Time) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `
INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
    (event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata, amount_raw, delta_amount_raw, observed_slot,
     observed_at, source, source_commitment, mint, txn_signature)
VALUES ($1, $2, 'itest-wallet', 'itest-wallet-ata', 'itest-wallet-ata', $3, $4, 1, $5, 'itest', 'confirmed', $6, $7)`,
		eventID, targetID, amountRaw, delta, observedAt, USDCMint,
		fmt.Sprintf("itest-sig-%d", eventID))
	if err != nil {
		t.Fatalf("seed wallet balance event %d: %v", eventID, err)
	}
}

// TestProjectionSchedulesCoalescesAndDepletes pins the durable scheduling
// behavior the typed lot math only describes: the one-hour delay lands in the
// slot, a newer lot coalesces into the same slot without moving its deadline
// earlier, an external outflow depletes newest lots first, and replaying the
// projection is a no-op.
func TestProjectionSchedulesCoalescesAndDepletes(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, "projection")

	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	store.insertIntegrationEvent(t, seeded.TargetID, 9_100_001, 9_000_000, nil, base)
	store.insertIntegrationEvent(t, seeded.TargetID, 9_100_002, 10_000_000, ptrInt64(1_000_000), base.Add(time.Hour))
	store.insertIntegrationEvent(t, seeded.TargetID, 9_100_003, 8_000_000, ptrInt64(-2_000_000), base.Add(2*time.Hour))

	first, err := store.ProjectSurplusLotsOnce(ctx, 100)
	if err != nil {
		t.Fatalf("project first batch: %v", err)
	}
	if first.EventsScanned != 3 || first.LotsCreated != 2 || first.LastEventID != 9_100_003 {
		t.Fatalf("first pass outcome %+v, want 3 events scanned and 2 lots created", first)
	}

	var lotCount int
	if err := store.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id = $1`,
		seeded.TargetID).Scan(&lotCount); err != nil {
		t.Fatalf("count lots: %v", err)
	}
	if lotCount != 2 {
		t.Fatalf("target has %d lots, want 2", lotCount)
	}

	// One slot per target holding both lots, due no earlier than the newest
	// lot's one-hour deadline (coalesced due = max(old, new)).
	var slotID int64
	var eligibleAfter time.Time
	var lotsInSlot int
	if err := store.pool.QueryRow(ctx, `
SELECT slot.id, slot.eligible_after,
       (SELECT COUNT(*) FROM loyal_yield.balance_sweep_surplus_lots lot WHERE lot.scheduled_slot_id = slot.id)
FROM loyal_yield.balance_sweep_scheduled_slots slot
WHERE slot.target_id = $1`, seeded.TargetID).Scan(&slotID, &eligibleAfter, &lotsInSlot); err != nil {
		t.Fatalf("read scheduled slot: %v", err)
	}
	if lotsInSlot != 2 {
		t.Fatalf("slot holds %d lots, want both lots coalesced into one slot", lotsInSlot)
	}
	newestDeadline := base.Add(time.Hour + ScheduleDelay)
	if eligibleAfter.Before(newestDeadline.Add(-time.Second)) {
		t.Fatalf("slot due %v moved earlier than the newest lot's deadline %v", eligibleAfter, newestDeadline)
	}

	// Newest-first depletion: the 2M outflow consumed the newest lot (the 1M
	// surplus increase) fully and drew 1M from the oldest 5M lot.
	var oldestRemaining, newestRemaining int64
	if err := store.pool.QueryRow(ctx, `
SELECT remaining_amount_raw FROM loyal_yield.balance_sweep_surplus_lots
WHERE target_id = $1 AND source_event_id = 9100001`, seeded.TargetID).
		Scan(&oldestRemaining); err != nil {
		t.Fatalf("read oldest lot: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `
SELECT remaining_amount_raw FROM loyal_yield.balance_sweep_surplus_lots
WHERE target_id = $1 AND source_event_id = 9100002`, seeded.TargetID).
		Scan(&newestRemaining); err != nil {
		t.Fatalf("read newest lot: %v", err)
	}
	if first.OutflowAmountRaw != 2_000_000 {
		t.Fatalf("outflow totaled %d, want 2000000", first.OutflowAmountRaw)
	}
	if newestRemaining != 0 {
		t.Fatalf("newest lot kept %d after an outflow at least its size; depletion must be newest-first", newestRemaining)
	}
	if oldestRemaining != 4_000_000 {
		t.Fatalf("oldest lot remaining %d, want 4000000 after the 1M spillover", oldestRemaining)
	}

	replay, err := store.ProjectSurplusLotsOnce(ctx, 100)
	if err != nil {
		t.Fatalf("replay projection: %v", err)
	}
	if replay.EventsScanned != 0 || replay.LotsCreated != 0 {
		t.Fatalf("replayed projection reprocessed %+v; the offset must be durable", replay)
	}
}

// TestReconciliationRequestCoalescesAndClaims pins the migration-0061 contract:
// one high-water row per target, an older ask coalesced away, claim exclusivity
// under concurrency, and completion reporting what is still pending.
func TestReconciliationRequestCoalescesAndClaims(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, "reconcile")

	changed, err := store.EnqueueAutodepositReconciliationRequest(ctx, seeded.TargetID, 100)
	if err != nil || !changed {
		t.Fatalf("first enqueue changed=%v err=%v", changed, err)
	}
	changed, err = store.EnqueueAutodepositReconciliationRequest(ctx, seeded.TargetID, 50)
	if err != nil || changed {
		t.Fatalf("older ask must coalesce into the existing row: changed=%v err=%v", changed, err)
	}
	changed, err = store.EnqueueAutodepositReconciliationRequest(ctx, seeded.TargetID, 300)
	if err != nil || !changed {
		t.Fatalf("higher ask must raise the high-water mark: changed=%v err=%v", changed, err)
	}

	var requestedSlot int64
	if err := store.pool.QueryRow(ctx, `
SELECT requested_slot FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id = $1`,
		seeded.TargetID).Scan(&requestedSlot); err != nil {
		t.Fatalf("read reconciliation request: %v", err)
	}
	if requestedSlot != 300 {
		t.Fatalf("requested_slot %d, want the high-water mark 300", requestedSlot)
	}

	// One ready row, two claimers: exactly one wins, the other sees no work.
	const claimers = 2
	var wg sync.WaitGroup
	won := make(chan *ReconciliationRequest, claimers)
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			request, err := store.ClaimAutodepositReconciliationRequest(ctx, fmt.Sprintf("claimer-%d", index), 60)
			if err != nil {
				t.Errorf("claim %d: %v", index, err)
				return
			}
			won <- request
		}(i)
	}
	wg.Wait()
	close(won)
	var winners int
	for request := range won {
		if request != nil {
			winners++
			if request.TargetID != seeded.TargetID || request.RequestedSlot != 300 {
				t.Fatalf("claimed %+v, want target %d slot 300", request, seeded.TargetID)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("%d claimers won one ready row, want exactly 1", winners)
	}
}

// TestReconciliationCompleteAdvancesHighWater pins completion semantics on the
// winning owner: partial progress stays pending, full progress clears.
func TestReconciliationCompleteAdvancesHighWater(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, "complete")

	if _, err := store.EnqueueAutodepositReconciliationRequest(ctx, seeded.TargetID, 500); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	request, err := store.ClaimAutodepositReconciliationRequest(ctx, "owner", 60)
	if err != nil || request == nil {
		t.Fatalf("claim: request=%v err=%v", request, err)
	}
	if request.AttemptCount != 1 {
		t.Fatalf("first claim attempt_count %d, want 1", request.AttemptCount)
	}

	stillPending, err := store.CompleteAutodepositReconciliationRequest(ctx, seeded.TargetID, "owner", 200)
	if err != nil {
		t.Fatalf("partial complete: %v", err)
	}
	if !stillPending {
		t.Fatal("processing 200 of 500 must stay pending")
	}

	request, err = store.ClaimAutodepositReconciliationRequest(ctx, "owner", 60)
	if err != nil || request == nil {
		t.Fatalf("re-claim after partial: request=%v err=%v", request, err)
	}
	if request.RequestedSlot != 500 || request.AttemptCount != 1 {
		t.Fatalf("re-claim %+v, want slot 500 with a reset then bumped attempt count", request)
	}
	stillPending, err = store.CompleteAutodepositReconciliationRequest(ctx, seeded.TargetID, "owner", 500)
	if err != nil {
		t.Fatalf("full complete: %v", err)
	}
	if stillPending {
		t.Fatal("processing the full high-water mark must clear the request")
	}
	empty, err := store.ClaimAutodepositReconciliationRequest(ctx, "owner", 60)
	if err != nil || empty != nil {
		t.Fatalf("cleared request must not be claimable again: request=%v err=%v", empty, err)
	}
}

// TestWorkerTickDispatchesEligibleTarget runs the complete pass against real
// SQL: projection schedules the lot, housekeeping runs, and the eligible slot
// is dispatched to the executor once.
func TestWorkerTickDispatchesEligibleTarget(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	seeded := seedIntegrationTarget(t, store, "tick")

	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	store.insertIntegrationEvent(t, seeded.TargetID, 9_200_001, 9_000_000, nil, base)
	if _, err := store.pool.Exec(ctx, `
INSERT INTO loyal_yield.balance_sweep_wallet_balances_current
    (target_id, wallet, wallet_usdc_ata, wallet_token_ata, amount_raw, mint, observed_slot, source, source_commitment)
VALUES ($1, 'itest-wallet', 'itest-wallet-usdc', 'itest-wallet-usdc', 9000000, $2, 1, 'itest', 'confirmed')`,
		seeded.TargetID, USDCMint); err != nil {
		t.Fatalf("seed current wallet balance: %v", err)
	}

	executor := &scriptedExecutor{exits: []*int{exitCodePtr(ExitNoop)}}
	worker, err := NewWorker(WorkerDependencies{Store: store, Executor: executor})
	if err != nil {
		t.Fatalf("build worker: %v", err)
	}
	report, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if report.Projection.EventsScanned != 1 || report.Projection.LotsCreated != 1 {
		t.Fatalf("tick projection %+v, want the seeded event projected into one lot", report.Projection)
	}
	if report.Outcome.TargetsScanned != 1 || len(report.Dispatched) != 1 {
		t.Fatalf("tick dispatched %+v (outcome %+v), want exactly the seeded target", report.Dispatched, report.Outcome)
	}
	if report.Dispatched[0].TargetID != seeded.TargetID {
		t.Fatalf("dispatched target %d, want %d", report.Dispatched[0].TargetID, seeded.TargetID)
	}
	if report.Outcome.ExecutionsNoop != 1 || len(report.Alerts) != 0 {
		t.Fatalf("noop exit tallied %+v with alerts %v", report.Outcome, report.Alerts)
	}
	if executor.order[0].ScheduledSlotID == 0 {
		t.Fatal("executor received a target without its scheduled slot")
	}

	// A second tick re-projects nothing new and finds nothing dispatchable:
	// the lot is still there but the scan must not double-dispatch fresh work
	// (one dispatch per target comes from claim exclusivity, so an unclaimed
	// slot is legitimately re-listed; the executor's noop left no claim).
	second, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if second.Projection.EventsScanned != 0 {
		t.Fatalf("second tick reprocessed %+v", second.Projection)
	}
}
