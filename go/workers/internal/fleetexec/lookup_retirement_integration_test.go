package fleetexec

import (
	"testing"
	"time"
)

// An expired rollback window is not a worker decision: as in the source
// provisioner, only the operator's explicit finalize retires the predecessor.
// Production had 861 expired standby bindings; a planner that finalized them
// itself failed every tick on the 256-binding bound and stopped planning.
func TestLookupPlannerLeavesExpiredRollbackToOperator(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx := t.Context()
	store, op := seedLookupSource(t, ctx, pool, f)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='cancelled',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET desired_state='standby',accepting_allocations=false WHERE id=$1`, op.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET active_generation=1,previous_generation=0,rollback_until=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	planner, err := NewLookupPlanner(store, svm.rpc, LookupPlannerConfig{Cluster: "localnet", Owner: "rollback-plan", LeaseTTL: time.Minute, TickDeadline: 25 * time.Second, PollInterval: time.Second, CatalogInterval: time.Minute, GrowthReservation: 8, MaximumVaultCohort: 16, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = planner.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var previous *int32
	var state string
	if err = pool.QueryRow(ctx, `SELECT f.previous_generation,t.desired_state FROM loyal_yield.lookup_table_families f JOIN loyal_yield.route_lookup_tables t ON t.id=$2 WHERE f.id=$1`, op.Intent.FamilyID, op.Intent.TableID).Scan(&previous, &state); err != nil || previous == nil || *previous != 0 || state != "standby" {
		t.Fatal("planner finalized an expired rollback", previous, state, err)
	}
	if changed, err := store.FinalizeLookupRollback(ctx, "localnet", op.Intent.FamilyID); err != nil || !changed {
		t.Fatal("explicit finalize", changed, err)
	}
}

func TestLookupRollbackRetainsUnexpiredAndLeasedGeneration(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	ctx := t.Context()
	store, op := seedLookupSource(t, ctx, pool, f)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='cancelled',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET desired_state='standby',accepting_allocations=false WHERE id=$1`, op.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET active_generation=1,previous_generation=0,rollback_until=clock_timestamp()+interval '1 hour' WHERE id=$1`, op.Intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		changed, err := store.FinalizeLookupRollback(ctx, "localnet", op.Intent.FamilyID)
		if err != nil || changed != want {
			t.Fatal("rollback protection", changed, err)
		}
	}
	check(false)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET rollback_until=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_usage_leases(cluster,lease_kind,reference_key,route_lookup_table_id,expires_at) VALUES('localnet','prepared_transaction','retirement-hold',$1,clock_timestamp()+interval '1 hour')`, op.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	check(false)
	var previous *int32
	if err := pool.QueryRow(ctx, `SELECT previous_generation FROM loyal_yield.lookup_table_families WHERE id=$1`, op.Intent.FamilyID).Scan(&previous); err != nil || previous == nil || *previous != 0 {
		t.Fatal("protected predecessor erased", previous, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_usage_leases SET released_at=clock_timestamp() WHERE reference_key='retirement-hold'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET rollback_until=clock_timestamp()+interval '1 hour' WHERE id=$1`, op.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET rollback_until=NULL WHERE id=$1`, op.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_controls SET paused=true WHERE cluster='localnet'`); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_controls SET paused=false WHERE cluster='localnet'`); err != nil {
		t.Fatal(err)
	}
	check(true)
	var state string
	if err := pool.QueryRow(ctx, `SELECT desired_state FROM loyal_yield.route_lookup_tables WHERE id=$1`, op.Intent.TableID).Scan(&state); err != nil || state != "retiring" {
		t.Fatal("unprotected predecessor not retired", state, err)
	}
	check(false)
}
