package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"
)

func TestLookupBindingPublicationUsesActualBankAndLogicalUsageFence(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	store, op := seedLookupSource(t, ctx, pool, f)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET accepting_allocations=true,durable=true WHERE id=$1`, op.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET active_generation=0 WHERE id=$1`, op.Intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	seedLookupCatalogDemand(t, ctx, pool, f)
	seedLookupDemandRequest(t, ctx, pool, f, f.Addresses[:2])
	r, err := store.LeaseLookupPlanningRequest(ctx, "localnet", "binding-plan", time.Minute)
	if err != nil || r == nil {
		t.Fatal(r, err)
	}
	policy := LookupPackingPolicy{HardCapacity: 256, LargestAtomicExpansion: 20, SafetyMargin: 8, GrowthReservation: 8, MaximumVaultCohort: 16}
	p, err := store.planLookupVaultRequest(ctx, *r, policy, lookupPlanningBank{slot: 1000, authority: f.Manager})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	worker, err := NewLookupWorker(store, svm.rpc, LookupWorkerConfig{Cluster: "localnet", Owner: "binding-writer", LeaseTTL: time.Minute, TickDeadline: 25 * time.Second, PollInterval: time.Second, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, Facts: testFacts()}, func(context.Context, string) (ed25519.PrivateKey, error) {
		return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := svm.rpc.LookupSnapshot(ctx, f.Table, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ActivateLookupBinding(ctx, p.BindingID, snapshot); err == nil {
		t.Fatal("same-bank or unverified binding published")
	}
	if err = svm.direct("advanceSlot", []any{1001}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err = svm.rpc.LookupSnapshot(ctx, f.Table, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_usage_leases(cluster,lease_kind,reference_key,route_lookup_table_id,vault_id,binding_id,expires_at) VALUES('localnet','prepared_transaction','binding-hold',$1,$2,$3,clock_timestamp()+interval '1 hour')`, p.TableID, r.VaultID, p.BindingID); err != nil {
		t.Fatal(err)
	}
	if err = store.ActivateLookupBinding(ctx, p.BindingID, snapshot); err == nil {
		t.Fatal("logical vault usage bypassed")
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_usage_leases SET released_at=clock_timestamp() WHERE reference_key='binding-hold'`); err != nil {
		t.Fatal(err)
	}
	seedLookupDemandRequest(t, ctx, pool, f, f.Addresses[:2])
	other, err := store.LeaseLookupPlanningRequest(ctx, "localnet", "other-vault-plan", time.Minute)
	if err != nil || other == nil {
		t.Fatal(other, err)
	}
	otherPlan, err := store.planLookupVaultRequest(ctx, *other, policy, lookupPlanningBank{slot: 1001, authority: f.Manager})
	if err != nil || otherPlan.TableID != p.TableID || otherPlan.BindingID == p.BindingID {
		t.Fatal("shared packed table reservation", otherPlan, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_usage_leases(cluster,lease_kind,reference_key,route_lookup_table_id,vault_id,binding_id,expires_at) VALUES('localnet','prepared_transaction','other-vault-hold',$1,$2,$3,clock_timestamp()+interval '1 hour')`, p.TableID, other.VaultID, otherPlan.BindingID); err != nil {
		t.Fatal(err)
	}
	if err = store.ActivateLookupBinding(ctx, p.BindingID, snapshot); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = pool.QueryRow(ctx, `SELECT lifecycle_state FROM loyal_yield.lookup_table_vault_bindings WHERE id=$1`, p.BindingID).Scan(&state); err != nil || state != "active" {
		t.Fatal(state, err)
	}
}
