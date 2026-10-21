package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
)

func TestLookupCatalogActivationAndRevalidationCoverageDoNotDeadlock(t *testing.T) {
	registered := lookupRegisteredPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	config := registered.Config()
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `TRUNCATE loyal_yield.lookup_table_families CASCADE`); err != nil {
		t.Fatal(err)
	}
	cluster := "lock-order"
	key := func(seed byte) string { return sdk.PublicKeyFromBytes(bytes.Repeat([]byte{seed}, 32)).String() }
	authority, table := key(201), key(202)
	addresses := []string{key(11), key(12)}
	sort.Strings(addresses)
	required := []fleet.ALTManifestAddress{}
	for i, address := range addresses {
		required = append(required, fleet.ALTManifestAddress{Address: address, SemanticClass: "shared_market", Ordinal: int32(i), AccountRole: "market"})
	}
	hash := lookupOrderedAddressHash(addresses)
	var family, manifest, revision, tableID int64
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_families(cluster,logical_name,kind,planner_version,catalog_version,active_generation,provisioning_authority,payer,hard_capacity,largest_atomic_expansion,safety_margin,allocation_high_water) VALUES($1,'lock-order','shared_market','v1','v1',0,$2,$2,256,239,1,16) RETURNING id`, cluster, authority).Scan(&family); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_manifests(family_id,subject_kind,subject_key,desired_set_hash,address_count,source_slot,planner_version,catalog_version) VALUES($1,'shared_market','lock-order',$2,2,100,'v1','v1') RETURNING id`, family, hash).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	for i, address := range addresses {
		if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_manifest_addresses(manifest_id,address,ordinal,semantic_class,account_role,is_writable) VALUES($1,$2,$3,'shared_market','market',false)`, manifest, address, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_manifests SET sealed_at=clock_timestamp() WHERE id=$1`, manifest); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_revisions(family_id,manifest_id,catalog_revision,catalog_version,desired_set_hash,enabled_mints_hash,reserve_set_hash,address_count,source_slot,reason,updated_by) VALUES($1,$2,1,'v1',$3,$3,$3,2,100,'fixture','fixture') RETURNING id`, family, manifest, hash).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_heads(family_id,catalog_revision_id,target_generation,readiness_state,activated_at) VALUES($1,$2,0,'active',clock_timestamp())`, family, revision); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(addresses)
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_lookup_tables(cluster,scope,table_address,authority,payer,status,durable,address_count,addresses,address_hash,family_id,allocation_kind,generation,shard_ordinal,desired_state,accepting_allocations,allocation_high_water,reserved_address_count,usable_address_count,last_verified_slot,mutation_epoch) VALUES($1,'lock-order',$2,$3,$3,'active',true,2,$4::jsonb,$5,$6,'shared_market',0,0,'active',true,16,0,2,101,0) RETURNING id`, cluster, table, authority, raw, hash, family).Scan(&tableID); err != nil {
		t.Fatal(err)
	}
	for i, address := range addresses {
		if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_addresses(route_lookup_table_id,address,ordinal,added_slot,usable_after_slot,last_verified_slot,last_verified_at) VALUES($1,$2,$3,100,101,101,clock_timestamp())`, tableID, address, i); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := LookupSnapshot{Address: table, Owner: lookupProgram, Slot: 102, Authority: authority, Addresses: addresses, DeactivationSlot: math.MaxUint64, LastExtendedSlot: 100}

	waiting := func(want int) {
		t.Helper()
		for {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == want {
				return
			}
		}
	}
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err = gate.Exec(ctx, `SELECT 1 FROM loyal_yield.lookup_table_shared_market_catalog_heads WHERE family_id=$1 FOR UPDATE`, family); err != nil {
		t.Fatal(err)
	}
	activation := make(chan error, 1)
	go func() {
		activation <- NewStore(pool).ActivateLookupCatalog(ctx, cluster, family, revision, 0, []LookupSnapshot{snapshot})
	}()
	waiting(1)
	revalidation := make(chan error, 1)
	go func() {
		revalidation <- pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
			return fleet.LockSharedALTManifestCoverage(ctx, tx, cluster, required)
		})
	}()
	waiting(2)
	if err = gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	activated, covered := <-activation, <-revalidation
	for _, err := range []error{activated, covered} {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			t.Fatalf("lookup catalog lock order cycle: activation=%v revalidation=%v", activated, covered)
		}
	}
	if activated != nil || covered != nil {
		t.Fatalf("activation=%v revalidation=%v", activated, covered)
	}
}

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
	snapshot, err := lookupSnapshot(ctx, svm.rpc, f.Table, 1000)
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
	snapshot, err = lookupSnapshot(ctx, svm.rpc, f.Table, 1001)
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
	// The fleet routes through the active catalog: its usage must not fail
	// the planner's re-verification of the generation already active.
	if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_usage_leases(cluster,lease_kind,reference_key,route_lookup_table_id,expires_at) VALUES('localnet','prepared_transaction','active-usage',$1,clock_timestamp()+interval '1 hour')`, op.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	if err = store.ActivateLookupCatalog(ctx, "localnet", op.Intent.FamilyID, revision, 0, []LookupSnapshot{snapshot}); err != nil {
		t.Fatal("active catalog re-verification held by fleet usage:", err)
	}
}
