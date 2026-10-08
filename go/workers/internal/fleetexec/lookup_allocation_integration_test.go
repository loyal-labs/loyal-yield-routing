package fleetexec

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

func seedLookupCatalogDemand(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f lookupFixture) int64 {
	t.Helper()
	var family, manifest, revision int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_families(cluster,logical_name,kind,planner_version,catalog_version,provisioning_authority,payer,largest_atomic_expansion,safety_margin,allocation_high_water) VALUES('localnet','allocation-shared','shared_market','v2-test','v2-test',$1,$1,20,8,228) RETURNING id`, f.Manager).Scan(&family); err != nil {
		t.Fatal(err)
	}
	descriptors := []LookupManifestAddress{{Address: f.Addresses[2], SemanticClass: "shared_market", Role: "market"}}
	hash := lookupManifestHash(descriptors)
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_manifests(family_id,subject_kind,subject_key,desired_set_hash,address_count,source_slot,planner_version,catalog_version) VALUES($1,'shared_market','catalog:allocation',$2,1,1000,'v2-test','v2-test') RETURNING id`, family, hash).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_manifest_addresses(manifest_id,address,semantic_class,account_role,ordinal,is_writable) VALUES($1,$2,'shared_market','market',0,false)`, manifest, f.Addresses[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_manifests SET sealed_at=clock_timestamp() WHERE id=$1`, manifest); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_revisions(family_id,manifest_id,catalog_revision,catalog_version,desired_set_hash,enabled_mints_hash,reserve_set_hash,address_count,source_slot,reason,updated_by) VALUES($1,$2,1,'v2-test',$3,$3,$3,1,1000,'allocation fixture','fixture') RETURNING id`, family, manifest, hash).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_heads(family_id,catalog_revision_id,target_generation,readiness_state) VALUES($1,$2,0,'provisioning')`, family, revision); err != nil {
		t.Fatal(err)
	}
	return family
}

func seedLookupDemandRequest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f lookupFixture, addresses []string) int64 {
	t.Helper()
	identity := fmt.Sprintf("lookup-allocation-%d", time.Now().UnixNano())
	var policy, vault, id int64
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,last_seen_slot,last_seen_signature) VALUES($1,$1,1,$1,0,$1,ARRAY[$1]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY['usdc']::text[],ARRAY[$1]::text[],ARRAY['usdc']::text[],'[]',true,1000,$1) RETURNING id`, identity).Scan(&policy); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.managed_vaults(settings,vault_index,vault_pubkey,active_policy_id) VALUES($1,0,$1,$2) RETURNING id`, identity, policy).Scan(&vault); err != nil {
		t.Fatal(err)
	}
	var descriptors []LookupManifestAddress
	for n, address := range addresses {
		descriptors = append(descriptors, LookupManifestAddress{Address: address, SemanticClass: "vault", Role: "vault", Ordinal: int32(n)})
	}
	shared := []LookupManifestAddress{{Address: f.Addresses[2], SemanticClass: "shared_market", Role: "market"}}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_requests(cluster,vault_id,route_fingerprint,requirements_fingerprint,desired_shared_hash,desired_vault_hash,desired_shared_address_count,desired_vault_address_count) VALUES('localnet',$1,$2,$2,$3,$4,1,$5) RETURNING id`, vault, identity, lookupManifestHash(shared), lookupManifestHash(descriptors), len(addresses)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_request_addresses(request_id,address,semantic_class,ordinal,account_role,is_writable) VALUES($1,$2,'shared_market',0,'market',false)`, id, f.Addresses[2]); err != nil {
		t.Fatal(err)
	}
	for n, address := range addresses {
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_request_addresses(request_id,address,semantic_class,ordinal,account_role,is_writable) VALUES($1,$2,'vault',$3,'vault',false)`, id, address, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET sealed_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLookupAtomicAllocationReusesPendingShardAndFencesStaleRequest(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	ctx := t.Context()
	store, op := seedLookupSource(t, ctx, pool, f)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET accepting_allocations=true WHERE id=$1`, op.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	seedLookupCatalogDemand(t, ctx, pool, f)
	seedLookupDemandRequest(t, ctx, pool, f, f.Addresses[:2])
	r, err := store.LeaseLookupPlanningRequest(ctx, "localnet", "atomic-planner", time.Minute)
	if err != nil || r == nil {
		t.Fatal(r, err)
	}
	policy := LookupPackingPolicy{HardCapacity: 256, LargestAtomicExpansion: 20, SafetyMargin: 8, GrowthReservation: 8, MaximumVaultCohort: 16}
	p, err := store.planLookupVaultRequest(ctx, *r, policy, lookupPlanningBank{slot: 1000, authority: f.Manager})
	if err != nil {
		t.Fatal(err)
	}
	if p.TableID != op.Intent.TableID || p.OperationID != op.Intent.OperationID || p.BindingID == 0 {
		t.Fatal("duplicate pending shard", p)
	}
	var reserved, bindings int
	var status string
	if err = pool.QueryRow(ctx, `SELECT t.reserved_address_count,(SELECT count(*) FROM loyal_yield.lookup_table_vault_bindings WHERE route_lookup_table_id=t.id),r.request_status FROM loyal_yield.route_lookup_tables t JOIN loyal_yield.lookup_table_provisioning_requests r ON r.id=$2 WHERE t.id=$1`, p.TableID, r.ID).Scan(&reserved, &bindings, &status); err != nil || reserved != 10 || bindings != 1 || status != "queued" {
		t.Fatal("capacity or readiness publication", reserved, bindings, status, err)
	}
	if _, err = store.planLookupVaultRequest(ctx, *r, policy, lookupPlanningBank{slot: 1000, authority: f.Manager}); err == nil {
		t.Fatal("expired planning identity reused")
	}
	// A collision with identical set but changed instruction order must fail;
	// source idempotency hashes the set, while wire ordering remains immutable.
	err = db.WithTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		i := op.Intent
		i.Extension = []string{f.Addresses[1], f.Addresses[0]}
		_, err := enqueueLookupTx(ctx, tx, lookupQueuedOperation{intent: i, shard: 0, context: []byte(`{}`), key: "deliberate-order-collision"})
		if err != nil {
			return err
		}
		i.Extension = []string{f.Addresses[0], f.Addresses[1]}
		_, err = enqueueLookupTx(ctx, tx, lookupQueuedOperation{intent: i, shard: 0, context: []byte(`{}`), key: "deliberate-order-collision"})
		return err
	})
	if err == nil {
		t.Fatal("idempotent source key adopted another packet order")
	}
}
