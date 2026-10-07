package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"
)

func TestLookupCatalogActivationNeedsActualWarmShardsAndUsageFence(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	store, op := seedLookupSource(t, ctx, pool, f, "shared_market")
	addresses := []LookupManifestAddress{{Address: f.Addresses[0], SemanticClass: "shared_market", Role: "market"}, {Address: f.Addresses[1], SemanticClass: "shared_market", Ordinal: 1, Role: "reserve"}}
	hash := lookupManifestHash(addresses)
	var manifest, revision int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_manifests(family_id,subject_kind,subject_key,desired_set_hash,address_count,source_slot,planner_version,catalog_version) VALUES($1,'shared_market','catalog:test',$2,2,1000,'v2-test','v2-test') RETURNING id`, op.Intent.FamilyID, hash).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	for _, a := range addresses {
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_manifest_addresses(manifest_id,address,semantic_class,account_role,ordinal,is_writable) VALUES($1,$2,$3,$4,$5,false)`, manifest, a.Address, a.SemanticClass, a.Role, a.Ordinal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_manifests SET sealed_at=clock_timestamp() WHERE id=$1`, manifest); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_revisions(family_id,manifest_id,catalog_revision,catalog_version,desired_set_hash,enabled_mints_hash,reserve_set_hash,address_count,source_slot,reason,updated_by) VALUES($1,$2,1,'v2-test',$3,$3,$3,2,1000,'actual ALT fixture','fixture') RETURNING id`, op.Intent.FamilyID, manifest, hash).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_heads(family_id,catalog_revision_id,target_generation,readiness_state) VALUES($1,$2,0,'provisioning')`, op.Intent.FamilyID, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET manifest_id=$2,lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.Intent.OperationID, manifest); err != nil {
		t.Fatal(err)
	}
	worker, err := NewLookupWorker(store, svm.rpc, LookupWorkerConfig{Cluster: "localnet", Owner: "catalog-fixture", LeaseTTL: time.Minute, TickDeadline: 30 * time.Second, PollInterval: time.Second, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, Facts: testFacts()}, func(context.Context, string) (ed25519.PrivateKey, error) {
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
	if err = store.ActivateLookupCatalog(ctx, "localnet", op.Intent.FamilyID, revision, 0, []LookupSnapshot{snapshot}); err == nil {
		t.Fatal("same-bank unverified catalog activated")
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
	if err = store.ActivateLookupCatalog(ctx, "localnet", op.Intent.FamilyID, revision+1, 0, []LookupSnapshot{snapshot}); err == nil {
		t.Fatal("stale catalog revision published")
	}
	if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_usage_leases(cluster,lease_kind,reference_key,route_lookup_table_id,expires_at) VALUES('localnet','prepared_transaction','catalog-hold',$1,clock_timestamp()+interval '1 hour')`, op.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	if err = store.ActivateLookupCatalog(ctx, "localnet", op.Intent.FamilyID, revision, 0, []LookupSnapshot{snapshot}); err == nil {
		t.Fatal("catalog published over live source usage")
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_usage_leases SET released_at=clock_timestamp() WHERE reference_key='catalog-hold'`); err != nil {
		t.Fatal(err)
	}
	if err = store.ActivateLookupCatalog(ctx, "localnet", op.Intent.FamilyID, revision, 0, []LookupSnapshot{snapshot}); err != nil {
		t.Fatal(err)
	}
	var active *int32
	var ready string
	if err = pool.QueryRow(ctx, `SELECT f.active_generation,h.readiness_state FROM loyal_yield.lookup_table_families f JOIN loyal_yield.lookup_table_shared_market_catalog_heads h ON h.family_id=f.id WHERE f.id=$1`, op.Intent.FamilyID).Scan(&active, &ready); err != nil || active == nil || *active != 0 || ready != "active" {
		t.Fatal("actual catalog head not published", active, ready, err)
	}
	var rollback time.Time
	if err = pool.QueryRow(ctx, `SELECT rollback_until FROM loyal_yield.lookup_table_families WHERE id=$1`, op.Intent.FamilyID).Scan(&rollback); err != nil {
		t.Fatal(err)
	}
	if err = store.ActivateLookupCatalog(ctx, "localnet", op.Intent.FamilyID, revision, 0, []LookupSnapshot{snapshot}); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err = pool.QueryRow(ctx, `SELECT rollback_until FROM loyal_yield.lookup_table_families WHERE id=$1`, op.Intent.FamilyID).Scan(&after); err != nil || !rollback.Equal(after) {
		t.Fatal("catalog polling extended rollback and prevented cleanup", rollback, after, err)
	}
}
