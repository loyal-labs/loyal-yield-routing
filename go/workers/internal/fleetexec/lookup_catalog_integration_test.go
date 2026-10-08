package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	sdk "github.com/gagliardetto/solana-go"
)

func TestLookupPlannerCreatesExtendsAndRollsCatalogFromActualBank(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Second)
	defer cancel()
	store, _ := seedLookupSource(t, ctx, pool, f)
	family := seedLookupCatalogDemand(t, ctx, pool, f)
	if err := svm.direct("advanceSlot", []any{1001}, nil); err != nil {
		t.Fatal(err)
	}
	config := LookupPlannerConfig{Cluster: "localnet", Owner: "catalog-plan", LeaseTTL: time.Minute, TickDeadline: 25 * time.Second, PollInterval: time.Second, CatalogInterval: time.Minute, GrowthReservation: 8, MaximumVaultCohort: 16, Facts: testFacts()}
	planner, err := NewLookupPlanner(store, svm.rpc, config)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32))
	worker, err := NewLookupWorker(store, svm.rpc, LookupWorkerConfig{Cluster: "localnet", Owner: "catalog-worker", LeaseTTL: time.Minute, TickDeadline: 25 * time.Second, PollInterval: time.Second, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, Facts: testFacts()}, func(context.Context, string) (ed25519.PrivateKey, error) { return key, nil })
	if err != nil {
		t.Fatal(err)
	}
	runPlanner := func() {
		t.Helper()
		planner.nextCatalog = time.Time{}
		if _, err := planner.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runWriter := func() {
		t.Helper()
		if _, err := worker.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	warm := func(slot int) {
		t.Helper()
		if err := svm.direct("advanceSlot", []any{slot}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE family_id=$1 AND operation_state NOT IN ('complete','permanent_failure','cancelled')`, family); err != nil {
			t.Fatal(err)
		}
		runWriter()
		runPlanner()
	}
	runPlanner()
	runWriter()
	warm(1002)
	c, err := store.loadLookupCatalog(ctx, "localnet")
	if err != nil || c == nil || c.state != "active" || c.active == nil || *c.active != 0 {
		t.Fatal("new catalog did not activate", c, err)
	}
	// Publish an append-only sealed revision. Existing ordinal-zero table
	// must receive an actual Extend, rather than a replacement generation.
	descriptors := []LookupManifestAddress{{Address: f.Addresses[2], SemanticClass: "shared_market", Role: "market"}, {Address: f.Addresses[1], SemanticClass: "shared_market", Role: "reserve", Ordinal: 1}}
	hash := lookupManifestHash(descriptors)
	var manifest, revision int64
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_manifests(family_id,subject_kind,subject_key,desired_set_hash,address_count,source_slot,planner_version,catalog_version) VALUES($1,'shared_market','catalog:append',$2,2,1002,'v2-test','v2-test') RETURNING id`, family, hash).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	for _, a := range descriptors {
		if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_manifest_addresses(manifest_id,address,semantic_class,account_role,ordinal,is_writable) VALUES($1,$2,'shared_market',$3,$4,false)`, manifest, a.Address, a.Role, a.Ordinal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_manifests SET sealed_at=clock_timestamp() WHERE id=$1`, manifest); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_revisions(family_id,manifest_id,catalog_revision,catalog_version,desired_set_hash,enabled_mints_hash,reserve_set_hash,address_count,source_slot,reason,updated_by) VALUES($1,$2,2,'v2-test',$3,$3,$3,2,1002,'append fixture','fixture') RETURNING id`, family, manifest, hash).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_shared_market_catalog_heads SET catalog_revision_id=$2,target_generation=NULL,readiness_state='pending',activated_at=NULL WHERE family_id=$1`, family, revision); err != nil {
		t.Fatal(err)
	}
	runPlanner()
	runWriter()
	warm(1003)
	c, err = store.loadLookupCatalog(ctx, "localnet")
	if err != nil || c.state != "active" || c.active == nil || *c.active != 0 {
		t.Fatal("append changed physical generation", c, err)
	}
	var extensions int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_operations WHERE family_id=$1 AND operation_kind='extend' AND operation_state='complete'`, family).Scan(&extensions); err != nil || extensions != 1 {
		t.Fatal("append never executed real extension", extensions, err)
	}
	tables, err := store.lookupCatalogTables(ctx, *c, 0)
	if err != nil || len(tables) != 1 {
		t.Fatal(tables, err)
	}
	// A real ALT-program deactivation by the same authorized manager models
	// external lifecycle drift. The planner must observe it, not trust flags.
	blockhash, _, _, err := svm.rpc.LookupBlockhash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	golden := f.Instructions["deactivate"]
	data, err := base64.StdEncoding.DecodeString(golden.Data)
	if err != nil {
		t.Fatal(err)
	}
	var metas sdk.AccountMetaSlice
	for _, account := range golden.Accounts {
		address := account.Address
		if address == f.Table {
			address = tables[0].address
		}
		metas = append(metas, &sdk.AccountMeta{PublicKey: sdk.MustPublicKeyFromBase58(address), IsSigner: account.Signer, IsWritable: account.Writable})
	}
	external, err := sdk.NewTransaction([]sdk.Instruction{sdk.NewInstruction(sdk.MustPublicKeyFromBase58(golden.Program), metas, data)}, sdk.MustHashFromBase58(blockhash), sdk.TransactionPayer(sdk.MustPublicKeyFromBase58(f.Manager)))
	if err != nil {
		t.Fatal(err)
	}
	private := sdk.PrivateKey(key)
	if _, err = external.Sign(func(sdk.PublicKey) *sdk.PrivateKey { return &private }); err != nil {
		t.Fatal(err)
	}
	wire, err := external.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err = svm.rpc.SendWire(ctx, wire, true); err != nil {
		t.Fatal(err)
	}
	if err = svm.direct("advanceSlot", []any{1004}, nil); err != nil {
		t.Fatal(err)
	}
	// Historical generation seven represents a retired partial successor.
	// A replacement must choose eight, never active+1 or the old reservation.
	historical, err := lookupDerivedAddress(f.Manager, 900)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.route_lookup_tables(cluster,scope,table_address,authority,payer,status,durable,address_count,address_hash,addresses,family_id,allocation_kind,generation,shard_ordinal,desired_state,accepting_allocations,allocation_high_water,reserved_address_count,usable_address_count,mutation_epoch) VALUES('localnet','catalog:obsolete-seven',$1,$2,$2,'closed',true,0,'','[]'::jsonb,$3,'shared_market',7,0,'closed',false,228,0,0,0)`, historical, f.Manager, family); err != nil {
		t.Fatal(err)
	}
	runPlanner()
	runWriter()
	warm(1005)
	c, err = store.loadLookupCatalog(ctx, "localnet")
	if err != nil || c.state != "active" || c.active == nil || *c.active != 8 {
		t.Fatal("rollover reused historical successor", c, err)
	}
	var drifts int
	var previous *int32
	if err = pool.QueryRow(ctx, `SELECT previous_generation,(SELECT count(*) FROM loyal_yield.lookup_table_shared_market_physical_drifts WHERE family_id=f.id AND resolution_state='resolved') FROM loyal_yield.lookup_table_families f WHERE id=$1`, family).Scan(&previous, &drifts); err != nil || previous == nil || *previous != 0 || drifts != 1 {
		t.Fatal("actual drift or rollback evidence missing", previous, drifts, err)
	}
}
