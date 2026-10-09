package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	sdk "github.com/solana-foundation/solana-go/v2"
)

// Production Oct 8: two vaults were planned onto one packed shard seconds
// apart. The second was bound while the first vault's extend held the table,
// so it had no extend of its own. Once the first extend landed, the planner
// selected the uncovered binding for activation, failed the whole tick on the
// coverage invariant, and never reached the request re-plan that queues the
// missing extend. Both vaults must end covered and active through the real
// planner and writer ticks against the actual ALT program.
func TestLookupSecondVaultPlannedBehindInFlightExtendIsCoveredAndActivated(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	store, source := seedLookupSource(t, ctx, pool, f)
	table := source.Intent.TableID
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET accepting_allocations=true,durable=true WHERE id=$1`, table); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET active_generation=0 WHERE id=$1`, source.Intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	seedLookupCatalogDemand(t, ctx, pool, f)
	worker, err := NewLookupWorker(store, svm.rpc, LookupWorkerConfig{Cluster: "localnet", Owner: "shared-table-writer", LeaseTTL: time.Minute, TickDeadline: 25 * time.Second, PollInterval: time.Second, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, Facts: testFacts()}, func(context.Context, string) (ed25519.PrivateKey, error) {
		return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	planner, err := NewLookupPlanner(store, svm.rpc, LookupPlannerConfig{Cluster: "localnet", Owner: "shared-table-plan", LeaseTTL: time.Minute, TickDeadline: 25 * time.Second, PollInterval: time.Second, CatalogInterval: time.Minute, GrowthReservation: 8, MaximumVaultCohort: 16, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	// Vault-shard behavior only; the shared catalog is covered elsewhere.
	planner.nextCatalog = time.Now().Add(time.Hour)
	slot := int64(1000)
	advance := func() {
		t.Helper()
		slot++
		if err := svm.direct("advanceSlot", []any{slot}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET lease_expires_at=clock_timestamp()-interval '1 second',next_attempt_at=clock_timestamp()-interval '1 second' WHERE operation_state NOT IN ('complete','permanent_failure','cancelled')`); err != nil {
			t.Fatal(err)
		}
	}
	writerTick := func() {
		t.Helper()
		if _, err := worker.Tick(ctx); err != nil {
			t.Fatal("writer", err)
		}
	}
	plannerTick := func() {
		t.Helper()
		if _, err := planner.Tick(ctx); err != nil {
			t.Fatal("planner tick failed instead of treating a not-ready binding as not ready: ", err)
		}
	}
	state := func(id int64) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT lifecycle_state FROM loyal_yield.lookup_table_vault_bindings WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	pending := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=$1 AND operation_state NOT IN ('complete','permanent_failure','cancelled')`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Land the seeded create so the packed shard is active and usable.
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, source.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	writerTick()
	advance()
	writerTick()
	if pending() != 0 {
		t.Fatal("seeded create did not complete")
	}
	fresh := func() []string {
		return []string{sdk.NewWallet().PublicKey().String(), sdk.NewWallet().PublicKey().String()}
	}
	policy := LookupPackingPolicy{HardCapacity: 256, LargestAtomicExpansion: 20, SafetyMargin: 8, GrowthReservation: 8, MaximumVaultCohort: 16}
	plan := func(addresses []string) (int64, LookupVaultPlan) {
		t.Helper()
		id := seedLookupDemandRequest(t, ctx, pool, f, addresses)
		r, err := store.LeaseLookupPlanningRequest(ctx, "localnet", "shared-table-direct", time.Minute)
		if err != nil || r == nil || r.ID != id {
			t.Fatal("lease", r, err)
		}
		p, err := store.planLookupVaultRequest(ctx, *r, policy, lookupPlanningBank{slot: slot, authority: f.Manager})
		if err != nil {
			t.Fatal(err)
		}
		// The re-plan is due later, as in production's backlog flush.
		if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET next_attempt_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		return id, p
	}
	firstAddresses, secondAddresses := fresh(), fresh()
	_, first := plan(firstAddresses)
	secondRequest, second := plan(secondAddresses)
	if first.TableID != table || first.OperationID == 0 {
		t.Fatal("first vault did not queue its extend on the packed shard", first)
	}
	// Exact production shape: same shard, bound while the first extend is in
	// flight, with no extend of its own.
	if second.TableID != table || second.BindingID == first.BindingID || second.OperationID != first.OperationID {
		t.Fatal("second vault was not bound behind the in-flight extend", second)
	}
	var own int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_operations WHERE binding_id=$1`, second.BindingID).Scan(&own); err != nil || own != 0 {
		t.Fatal("fixture no longer reproduces the uncovered binding", own, err)
	}
	// Writer lands the first extend, then the next bank warms it.
	writerTick()
	advance()
	writerTick()
	if pending() != 0 {
		t.Fatal("first extend did not complete")
	}
	advance()
	// A live head for the first vault defers only that vault.
	if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_usage_leases(cluster,lease_kind,reference_key,route_lookup_table_id,vault_id,binding_id,expires_at) SELECT 'localnet','prepared_transaction','shared-table-hold',$1,vault_id,id,clock_timestamp()+interval '1 hour' FROM loyal_yield.lookup_table_vault_bindings WHERE id=$2`, table, first.BindingID); err != nil {
		t.Fatal(err)
	}
	plannerTick()
	if state(first.BindingID) != "preparing" || state(second.BindingID) != "preparing" {
		t.Fatal("logical usage or missing coverage bypassed", state(first.BindingID), state(second.BindingID))
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_usage_leases SET released_at=clock_timestamp() WHERE reference_key='shared-table-hold'`); err != nil {
		t.Fatal(err)
	}
	plannerTick()
	if state(first.BindingID) != "active" || state(second.BindingID) != "preparing" {
		t.Fatal("first covered vault did not activate alone", state(first.BindingID), state(second.BindingID))
	}
	// The second request is now due while its shard is idle and its binding is
	// uncovered: the state in which the old planner failed every tick.
	for range 6 {
		if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1 AND request_status<>'satisfied'`, secondRequest); err != nil {
			t.Fatal(err)
		}
		plannerTick()
		if state(second.BindingID) == "active" {
			break
		}
		writerTick()
		advance()
		writerTick()
		advance()
	}
	if state(first.BindingID) != "active" || state(second.BindingID) != "active" {
		t.Fatal("both vaults on the shared shard must end active", state(first.BindingID), state(second.BindingID))
	}
	var extend int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_operations WHERE binding_id=$1 AND operation_kind='extend' AND operation_state='complete'`, second.BindingID).Scan(&extend); err != nil || extend != 1 {
		t.Fatal("second vault was not covered by its own queued extend", extend, err)
	}
	snapshot, err := svm.rpc.LookupSnapshot(ctx, f.Table, slot)
	if err != nil {
		t.Fatal(err)
	}
	onChain := lookupAddressSet(snapshot.Addresses)
	for _, address := range append(append([]string{}, firstAddresses...), secondAddresses...) {
		if !onChain[address] {
			t.Fatal("actual ALT does not contain a published vault address", address)
		}
	}
	if len(snapshot.Addresses) != 6 {
		t.Fatal("actual ALT membership", snapshot.Addresses)
	}
}
