package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"
)

func TestLookupPlannerQueuesRealRetiringCleanupAndExactCloseRefund(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Second)
	defer cancel()
	store, op := seedLookupSource(t, ctx, pool, f)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32))
	keys := 0
	workerConfig := LookupWorkerConfig{Cluster: "localnet", Owner: "cleanup-writer", LeaseTTL: time.Minute, TickDeadline: 25 * time.Second, PollInterval: time.Second, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, Facts: testFacts()}
	worker, err := NewLookupWorker(store, svm.rpc, workerConfig, func(context.Context, string) (ed25519.PrivateKey, error) { keys++; return key, nil })
	if err != nil {
		t.Fatal(err)
	}
	runWriter := func(w *LookupWorker) {
		t.Helper()
		if _, err := w.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	due := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE family_id=$1 AND operation_state NOT IN ('complete','permanent_failure','cancelled')`, op.Intent.FamilyID); err != nil {
			t.Fatal(err)
		}
	}
	runWriter(worker)
	if err = svm.direct("advanceSlot", []any{1001}, nil); err != nil {
		t.Fatal(err)
	}
	due()
	runWriter(worker)
	// Explicit source retirement intent preserves all generation/reference
	// protection; the planner cannot retire a current shared-market head.
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET desired_state='retiring' WHERE id=$1`, op.Intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET desired_state='retiring',accepting_allocations=false WHERE id=$1`, op.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	planner, err := NewLookupPlanner(store, svm.rpc, LookupPlannerConfig{Cluster: "localnet", Owner: "cleanup-plan", LeaseTTL: time.Minute, TickDeadline: 25 * time.Second, PollInterval: time.Second, CatalogInterval: time.Minute, GrowthReservation: 8, MaximumVaultCohort: 16, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	runPlanner := func() {
		t.Helper()
		if _, err := planner.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runPlanner()
	runWriter(worker)
	if err = svm.direct("advanceSlot", []any{1002}, nil); err != nil {
		t.Fatal(err)
	}
	due()
	runWriter(worker)
	runPlanner()
	var closes int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=$1 AND operation_kind='close'`, op.Intent.TableID).Scan(&closes); err != nil || closes != 0 || keys != 2 {
		t.Fatal("cooldown queued close or requested signer", closes, keys, err)
	}
	// Every advanced fixture bank contributes its own actual hash. Numeric
	// age alone never establishes absence from SlotHashes or authorizes close.
	for slot := 1003; slot <= 1513; slot++ {
		if err = svm.direct("advanceSlot", []any{slot}, nil); err != nil {
			t.Fatal(err)
		}
	}
	runPlanner()
	snapshot, err := svm.rpc.LookupSnapshot(ctx, f.Table, 1513)
	if err != nil || lookupProducedSlot(snapshot.SlotHashes, 1001) {
		t.Fatal("actual deactivation bank retained", err)
	}
	before, err := svm.rpc.LookupBalance(ctx, f.Manager)
	if err != nil {
		t.Fatal(err)
	}
	runWriter(worker)
	if keys != 3 {
		t.Fatal("close key ownership", keys)
	}
	// Restart after actual close IO. Retired/paused controls cannot erase the
	// existing immutable packet or force another signing key lookup.
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET desired_state='retired' WHERE id=$1`, op.Intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_controls SET paused=true,control_epoch=control_epoch+1 WHERE cluster='localnet'`); err != nil {
		t.Fatal(err)
	}
	workerConfig.Owner = "cleanup-restart"
	workerConfig.ReconcileOnly = true
	restart, err := NewLookupWorker(store, svm.rpc, workerConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	due()
	runWriter(restart)
	after, err := svm.rpc.LookupBalance(ctx, f.Manager)
	if err != nil {
		t.Fatal(err)
	}
	if after != before+snapshot.Lamports-5000 {
		t.Fatal("close credited another recipient or inferred fee from rent", before, after, snapshot.Lamports)
	}
	var state string
	var fee, reclaimed, epoch int64
	if err = pool.QueryRow(ctx, `SELECT t.desired_state,o.actual_fee_lamports,o.reclaimed_rent_lamports,t.mutation_epoch FROM loyal_yield.route_lookup_tables t JOIN loyal_yield.lookup_table_operations o ON o.route_lookup_table_id=t.id AND o.operation_kind='close' WHERE t.id=$1`, op.Intent.TableID).Scan(&state, &fee, &reclaimed, &epoch); err != nil || state != "closed" || fee != 5000 || reclaimed != int64(snapshot.Lamports) || epoch != 1 {
		t.Fatal("close durable receipt identity", state, fee, reclaimed, epoch, err)
	}
}
