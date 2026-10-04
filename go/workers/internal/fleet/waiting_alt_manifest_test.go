package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

func waitingManifestFixture(t *testing.T, settings, vault, policy string) ALTManifest {
	return waitingManifestFixtureForRoute(t, settings, vault, policy, manifestKey(5), manifestKey(6))
}

func waitingManifestFixtureForRoute(t *testing.T, settings, vault, policy, source, target string) ALTManifest {
	t.Helper()
	payer := manifestKey(90)
	input := KaminoSameMintRouteRequest{Vault: vault, Source: KaminoPositionAccounts{Market: manifestKey(4), Reserve: source, LiquidityMint: USDCMint, Obligation: manifestKey(60), VaultLiquidityATA: manifestKey(61)}}
	input.Target = input.Source
	input.Target.Reserve = target
	input.Target.Obligation = manifestKey(62)
	accounts := []InstructionAccount{{settings, false, false}, {vault, false, true}, {policy, false, false}, {input.Source.Market, false, false}, {input.Source.Reserve, false, true}, {input.Target.Reserve, false, true}, {USDCMint, false, false}, {input.Source.Obligation, false, true}, {input.Target.Obligation, false, true}, {input.Source.VaultLiquidityATA, false, true}}
	for i := byte(10); i < 28; i++ {
		key := manifestKey(i)
		input.Source.ObligationDepositReserves = append(input.Source.ObligationDepositReserves, key)
		accounts = append(accounts, InstructionAccount{key, false, i%2 == 0})
	}
	manifest, err := BuildRouteALTManifest(input, settings, policy, payer, []RouteInstruction{{Program: KLendProgram, Accounts: accounts, Data: []byte{1}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func waitingManifestPreparation(m ALTManifest) RoutePreparation {
	return RoutePreparation{RouteFingerprint: strings.Repeat("a", 64), RequirementsFingerprint: m.Fingerprint, Manifest: &m, ExecutionPlan: json.RawMessage(`{"route_kind":"same_mint","source_kind":"reserve_position"}`)}
}

func TestWaitingALTUsesCompleteTypedVectorsAndDistinctOrderContracts(t *testing.T) {
	m := waitingManifestFixture(t, manifestKey(80), manifestKey(81), manifestKey(82))
	p := waitingManifestPreparation(m)
	missing := []string{m.VaultAddresses[0].Address}
	shared, vault, err := waitingALTManifestAddresses(p, missing)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != len(m.SharedAddresses) || len(vault) != len(m.VaultAddresses) || len(vault) <= len(missing) {
		t.Fatal("missing-only demand lost full manifest")
	}
	if provisioningAddressesHash(shared) == m.Fingerprint {
		t.Fatal("provisioning row hash substituted for v1 requirements fingerprint")
	}
	var raw [][32]byte
	for i, a := range shared {
		if i > 0 && shared[i-1].Address >= a.Address {
			t.Fatal("persisted order is not base58 lexical")
		}
		key, err := decodePublicKey(a.Address)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, key)
	}
	sort.Slice(raw, func(i, j int) bool { return bytes.Compare(raw[i][:], raw[j][:]) < 0 })
	differs := false
	for i, key := range raw {
		if shared[i].Address != encodeBase58(key[:]) {
			differs = true
		}
	}
	if !differs {
		t.Fatal("fixture failed to exercise raw-key/base58 ordering difference")
	}
	for _, mutate := range []func(*RoutePreparation){
		func(p *RoutePreparation) { p.Manifest = nil },
		func(p *RoutePreparation) { p.RequirementsFingerprint = strings.Repeat("b", 64) },
		func(p *RoutePreparation) { p.Manifest.VaultAddresses[0].AccountRole = "route" },
		func(p *RoutePreparation) {
			p.Manifest.VaultAddresses[0].Writable = !p.Manifest.VaultAddresses[0].Writable
		},
		func(p *RoutePreparation) { p.Manifest.SharedAddresses[0].Ordinal = 99 },
	} {
		copy := m
		copy.SharedAddresses = append([]ALTManifestAddress(nil), m.SharedAddresses...)
		copy.VaultAddresses = append([]ALTManifestAddress(nil), m.VaultAddresses...)
		changed := waitingManifestPreparation(copy)
		mutate(&changed)
		if _, _, err := waitingALTManifestAddresses(changed, missing); err == nil {
			t.Fatal("changed or fabricated manifest accepted")
		}
	}
	for _, missing := range [][]string{nil, {m.SharedAddresses[0].Address}, {manifestKey(99)}, {m.VaultAddresses[0].Address, m.VaultAddresses[0].Address}} {
		if _, _, err := waitingALTManifestAddresses(p, missing); err == nil {
			t.Fatalf("invalid missing membership accepted: %v", missing)
		}
	}
}

func TestSameMintALTManifestIncludesActualStaticBudgetProgram(t *testing.T) {
	input := KaminoSameMintRouteRequest{Vault: manifestKey(81), Source: KaminoPositionAccounts{Reserve: manifestKey(5)}}
	instructions := []RouteInstruction{{Program: KLendProgram, Accounts: []InstructionAccount{{input.Vault, false, true}, {manifestKey(82), false, false}, {input.Source.Reserve, false, true}}}}
	build := func(ixs []RouteInstruction) ALTManifest {
		t.Helper()
		m, err := BuildRouteALTManifest(input, manifestKey(80), manifestKey(82), manifestKey(90), ixs, nil)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	baseline := build(instructions)
	waiting := build(append(computeBudgetInstructions(1_400_000, 0), instructions...))
	final := build(append(computeBudgetInstructions(350_000, 900), instructions...))
	if waiting.Fingerprint != final.Fingerprint || baseline.Fingerprint == final.Fingerprint {
		t.Fatal("raw v1 fingerprint omitted static budget program or hashed dynamic compute values")
	}
	if len(baseline.SharedAddresses) != len(final.SharedAddresses) || len(baseline.VaultAddresses) != len(final.VaultAddresses) {
		t.Fatal("static program became durable ALT demand")
	}
	for i, a := range baseline.SharedAddresses {
		if a != final.SharedAddresses[i] {
			t.Fatal("static budget changed shared demand")
		}
	}
	for i, a := range baseline.VaultAddresses {
		if a != final.VaultAddresses[i] {
			t.Fatal("static budget changed vault demand")
		}
	}
}

func waitingALTStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("FLEET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("registered disposable FLEET_TEST_DATABASE_URL is unset")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "51913" || u.User == nil || u.User.Username() != "workers_v2" || (u.Path != "/fleet" && u.Path != "/fleet_same_mint") || u.RawQuery != "" {
		t.Fatal("ALT tests require the registered disposable loopback family database")
	}
	if _, password := u.User.Password(); password {
		t.Fatal("ALT tests do not consume credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var role, database string
	if err := s.pool.QueryRow(ctx, `SELECT current_user,current_database()`).Scan(&role, &database); err != nil || role != "workers_v2" || database != strings.TrimPrefix(u.Path, "/") {
		t.Fatal("ALT fixture role/database mismatch")
	}
	return s, ctx
}

func waitingIdentity(t *testing.T, ctx context.Context, s *Store, suffix string) (RevalidationLease, ALTManifest) {
	t.Helper()
	vaultID := seedWorkerVault(t, ctx, s, suffix, manifestKey(4), manifestKey(5))
	settings, vault, policy := bindWaitingFixtureVault(t, ctx, s, vaultID, suffix)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `UPDATE loyal_yield.managed_vaults SET active=false WHERE id=$1`, vaultID)
	})
	l := RevalidationLease{Cluster: "waiting-alt-" + suffix, VaultID: vaultID, VaultPubkey: vault, VaultIndex: 0, PolicyAccount: policy, RouteKind: "same_mint", SourceReserve: manifestKey(5), TargetReserve: manifestKey(6), LiquidityMint: USDCMint, SourceLiquidityMint: USDCMint, TargetLiquidityMint: USDCMint}
	return l, waitingManifestFixture(t, settings, vault, policy)
}

func bindWaitingFixtureVault(t *testing.T, ctx context.Context, s *Store, vaultID int64, suffix string) (string, string, string) {
	t.Helper()
	settingsHash := sha256.Sum256([]byte("waiting-settings:" + suffix))
	settings := encodeBase58(settingsHash[:])
	vault, _, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), settingsHash[:], []byte("smart_account"), {0}}, solana.MustPublicKeyFromBase58(SquadsProgram))
	if err != nil {
		t.Fatal(err)
	}
	policyKey, _, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("policy"), settingsHash[:], {1, 0, 0, 0, 0, 0, 0, 0}}, solana.MustPublicKeyFromBase58(SquadsProgram))
	if err != nil {
		t.Fatal(err)
	}
	policy := policyKey.String()
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET settings=$2,vault_pubkey=$3,policy_account=$4 WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, vaultID, settings, vault.String(), policy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.managed_vaults SET settings=$2,vault_pubkey=$3 WHERE id=$1`, vaultID, settings, vault.String()); err != nil {
		t.Fatal(err)
	}
	return settings, vault.String(), policy
}

// All catalog/head/seal/physical rows use registered source guards. This
// fixture proves the SQL contract only, not chain accounts or executable ABI.
func seedWaitingCatalog(t *testing.T, ctx context.Context, s *Store, cluster string, addresses []ALTManifestAddress) (int64, []int64) {
	t.Helper()
	var family, manifest, revision int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_families(cluster,logical_name,kind,planner_version,catalog_version,active_generation,provisioning_authority,payer,hard_capacity,largest_atomic_expansion,safety_margin,allocation_high_water)VALUES($1,'waiting-shared','shared_market','v1','v1',1,$2,$2,256,239,1,16)RETURNING id`, cluster, manifestKey(90)).Scan(&family); err != nil {
		t.Fatal(err)
	}
	converted := make([]provisioningAddress, len(addresses))
	for i, a := range addresses {
		converted[i] = provisioningAddress{a.Address, a.SemanticClass, a.Ordinal, a.AccountRole, a.Writable}
	}
	hash := provisioningAddressesHash(converted)
	if err := s.pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_manifests(family_id,subject_kind,subject_key,desired_set_hash,address_count,source_slot,planner_version,catalog_version)VALUES($1,'shared_market','waiting-catalog',$2,$3,100,'v1','v1')RETURNING id`, family, hash, len(addresses)).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	for _, a := range addresses {
		if _, err := s.pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_manifest_addresses(manifest_id,address,ordinal,semantic_class,account_role,is_writable)VALUES($1,$2,$3,$4,$5,$6)`, manifest, a.Address, a.Ordinal, a.SemanticClass, a.AccountRole, a.Writable); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_manifests SET sealed_at=clock_timestamp() WHERE id=$1`, manifest); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_revisions(family_id,manifest_id,catalog_revision,catalog_version,desired_set_hash,enabled_mints_hash,reserve_set_hash,address_count,source_slot,reason,updated_by)VALUES($1,$2,1,'v1',$3,$3,$3,$4,100,'fixture','fixture')RETURNING id`, family, manifest, hash, len(addresses)).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_heads(family_id,catalog_revision_id,target_generation,readiness_state,activated_at)VALUES($1,$2,1,'active',clock_timestamp())`, family, revision); err != nil {
		t.Fatal(err)
	}
	var tables []int64
	for start := 0; start < len(addresses); start += 16 {
		end := min(start+16, len(addresses))
		shard := start / 16
		var id int64
		key := sha256.Sum256([]byte(fmt.Sprintf("waiting-table:%s:%d", cluster, shard)))
		keys := []string{}
		for _, a := range addresses[start:end] {
			keys = append(keys, a.Address)
		}
		raw, _ := json.Marshal(keys)
		if err := s.pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_lookup_tables(cluster,scope,table_address,authority,payer,status,durable,address_count,addresses,family_id,allocation_kind,generation,shard_ordinal,desired_state,accepting_allocations,allocation_high_water,reserved_address_count,usable_address_count,last_verified_slot,mutation_epoch)VALUES($1,$2,$3,$4,$4,'active',true,$5,$6::jsonb,$7,'shared_market',1,$8,'active',true,16,0,$5,101,0)RETURNING id`, cluster, fmt.Sprintf("waiting-shard-%d", shard), encodeBase58(key[:]), manifestKey(90), len(keys), raw, family, shard).Scan(&id); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, id)
		for i, a := range addresses[start:end] {
			if _, err := s.pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_addresses(route_lookup_table_id,address,ordinal,added_slot,usable_after_slot,last_verified_slot,last_verified_at)VALUES($1,$2,$3,100,101,101,clock_timestamp())`, id, a.Address, i); err != nil {
				t.Fatal(err)
			}
		}
	}
	return family, tables
}

func upsertWaitingFixture(ctx context.Context, s *Store, l RevalidationLease, p RoutePreparation, missing []string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	id, err := upsertWaitingALTRequest(ctx, tx, l, p, missing)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func TestWaitingALTRegisteredFullManifestAndRouteShapeIdempotency(t *testing.T) {
	s, ctx := waitingALTStore(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	l, m := waitingIdentity(t, ctx, s, suffix)
	seedWaitingCatalog(t, ctx, s, l.Cluster, m.SharedAddresses)
	p := waitingManifestPreparation(m)
	missing := []string{m.VaultAddresses[0].Address}
	id, err := upsertWaitingFixture(ctx, s, l, p, missing)
	if err != nil {
		t.Fatal(err)
	}
	var sharedCount, vaultCount int
	var sharedHash, vaultHash, route string
	var sealed bool
	if err := s.pool.QueryRow(ctx, `SELECT desired_shared_address_count,desired_vault_address_count,desired_shared_hash,desired_vault_hash,route_fingerprint,sealed_at IS NOT NULL FROM loyal_yield.lookup_table_provisioning_requests WHERE id=$1`, id).Scan(&sharedCount, &vaultCount, &sharedHash, &vaultHash, &route, &sealed); err != nil {
		t.Fatal(err)
	}
	shared, vault, err := waitingALTManifestAddresses(p, missing)
	if err != nil {
		t.Fatal(err)
	}
	if !sealed || sharedCount != len(shared) || vaultCount != len(vault) || sharedHash != provisioningAddressesHash(shared) || vaultHash != provisioningAddressesHash(vault) {
		t.Fatal("sealed demand omitted typed vector/hash/count")
	}
	p.RouteFingerprint = strings.Repeat("b", 64)
	second, err := upsertWaitingFixture(ctx, s, l, p, missing)
	if err != nil || second != id {
		t.Fatalf("same requirements allocated twice: id=%d error=%v", second, err)
	}
	var first string
	if err := s.pool.QueryRow(ctx, `SELECT route_fingerprint FROM loyal_yield.lookup_table_provisioning_requests WHERE id=$1`, id).Scan(&first); err != nil || first != route {
		t.Fatalf("first route audit changed: %q error=%v", first, err)
	}
	rows, err := s.pool.Query(ctx, `SELECT address,semantic_class,ordinal,account_role,is_writable FROM loyal_yield.lookup_table_provisioning_request_addresses WHERE request_id=$1 ORDER BY semantic_class,ordinal`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	expected := append(shared, vault...)
	n := 0
	for rows.Next() {
		var a provisioningAddress
		if err := rows.Scan(&a.Address, &a.SemanticClass, &a.Ordinal, &a.AccountRole, &a.Writable); err != nil {
			t.Fatal(err)
		}
		if n >= len(expected) || a != expected[n] {
			t.Fatalf("persisted role/access/order differs at %d", n)
		}
		n++
	}
	if rows.Err() != nil || n != len(expected) {
		t.Fatal("persisted full vector count differs")
	}
}

// Retained fleet-worker lib.rs durable_lookup_table_manifest_addresses filters
// externally covered accounts from durable demand, while the original typed
// requirements fingerprint and source identities remain bound to the route.
func TestWaitingALTRegisteredExternalCoverageKeepsOriginalSourceIdentity(t *testing.T) {
	s, ctx := waitingALTStore(t)
	l, original := waitingIdentity(t, ctx, s, fmt.Sprint(time.Now().UnixNano()))
	sourceShared, sourceVault, err := ALTManifestSourceAddresses(&original)
	if err != nil {
		t.Fatal(err)
	}
	covered := []string{l.SourceReserve, l.PolicyAccount, l.VaultPubkey, USDCMint}
	data := make([]byte, 56+32*len(covered))
	binary.LittleEndian.PutUint32(data[:4], 1)
	binary.LittleEndian.PutUint64(data[4:12], ^uint64(0))
	binary.LittleEndian.PutUint64(data[12:20], 100)
	for i, address := range covered {
		key, err := decodePublicKey(address)
		if err != nil {
			t.Fatal(err)
		}
		copy(data[56+32*i:], key[:])
	}
	tableAddress := manifestKey(99)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		var keys []string
		var options struct {
			Commitment string `json:"commitment"`
			Minimum    int64  `json:"minContextSlot"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Method != "getMultipleAccounts" || len(request.Params) != 2 || json.Unmarshal(request.Params[0], &keys) != nil || json.Unmarshal(request.Params[1], &options) != nil || len(keys) != 1 || keys[0] != tableAddress || options.Commitment != "finalized" || options.Minimum != 100 {
			t.Error("external coverage did not request the actual finalized table at its minimum slot")
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int{"slot": 101}, "value": []any{map[string]any{"owner": altProgram, "lamports": 1, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}}}})
	}))
	defer server.Close()
	r := &Revalidator{rpc: NewRPCClient(server.URL)}
	filtered, err := r.filterFinalizedExternalALTManifest(ctx, original, []LookupTable{{Address: tableAddress, Active: true, Addresses: covered}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Fingerprint != original.Fingerprint || len(filtered.SharedAddresses) != len(sourceShared)-2 || len(filtered.VaultAddresses) != len(sourceVault)-2 {
		t.Fatal("durable filtering changed source identity or did not remove exact external coverage")
	}
	seedWaitingCatalog(t, ctx, s, l.Cluster, filtered.SharedAddresses)
	p := waitingManifestPreparation(filtered)
	missing := []string{filtered.VaultAddresses[0].Address}
	id, err := upsertWaitingFixture(ctx, s, l, p, missing)
	if err != nil {
		t.Fatalf("legitimate source identity was inferred from its filtered durable subset: %v", err)
	}
	var persisted int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_provisioning_request_addresses WHERE request_id=$1 AND address=ANY($2)`, id, covered).Scan(&persisted); err != nil || persisted != 0 {
		t.Fatalf("externally covered accounts became durable demand: count=%d error=%v", persisted, err)
	}
	l.SourceReserve = manifestKey(88)
	if _, err := upsertWaitingFixture(ctx, s, l, p, missing); err == nil {
		t.Fatal("filtered identity anchor permitted a foreign source lease")
	}
}

func TestWaitingALTRegisteredCatalogDriftCannotPublishDemand(t *testing.T) {
	for _, kind := range []string{"missing_head", "role_drift", "generation_drift", "extra_member", "missing_member", "unverified_member"} {
		t.Run(kind, func(t *testing.T) {
			s, ctx := waitingALTStore(t)
			l, m := waitingIdentity(t, ctx, s, fmt.Sprintf("%d-%s", time.Now().UnixNano(), kind))
			if kind != "missing_head" {
				catalog := append([]ALTManifestAddress(nil), m.SharedAddresses...)
				if kind == "role_drift" {
					catalog[0].AccountRole = "infrastructure"
					catalog[0].Writable = false
				}
				family, tables := seedWaitingCatalog(t, ctx, s, l.Cluster, catalog)
				var err error
				switch kind {
				case "generation_drift":
					_, err = s.pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET active_generation=2 WHERE id=$1`, family)
				case "extra_member":
					_, err = s.pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_addresses(route_lookup_table_id,address,ordinal,added_slot,usable_after_slot,last_verified_slot,last_verified_at)VALUES($1,$2,31,100,101,101,clock_timestamp())`, tables[len(tables)-1], manifestKey(100))
				case "missing_member":
					_, err = s.pool.Exec(ctx, `DELETE FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1 AND ordinal=0`, tables[0])
				case "unverified_member":
					_, err = s.pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_addresses SET usable_after_slot=102 WHERE route_lookup_table_id=$1 AND ordinal=0`, tables[0])
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := upsertWaitingFixture(ctx, s, l, waitingManifestPreparation(m), []string{m.VaultAddresses[0].Address}); err == nil {
				t.Fatal("catalog drift published vault demand")
			}
			var count int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_provisioning_requests WHERE cluster=$1`, l.Cluster).Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed catalog fence left demand: count=%d error=%v", count, err)
			}
		})
	}
}

func TestWaitingALTRegisteredSealedLegacyVectorCollisionIsHeld(t *testing.T) {
	s, ctx := waitingALTStore(t)
	l, m := waitingIdentity(t, ctx, s, fmt.Sprint(time.Now().UnixNano()))
	seedWaitingCatalog(t, ctx, s, l.Cluster, m.SharedAddresses)
	p := waitingManifestPreparation(m)
	missing := []string{m.VaultAddresses[0].Address}
	shared, vault, err := waitingALTManifestAddresses(p, missing)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_requests(cluster,vault_id,route_fingerprint,requirements_fingerprint,desired_shared_hash,desired_vault_hash,desired_shared_address_count,desired_vault_address_count)VALUES($1,$2,$3,$4,$5,$6,$7,$8)RETURNING id`, l.Cluster, l.VaultID, p.RouteFingerprint, p.RequirementsFingerprint, provisioningAddressesHash(shared), provisioningAddressesHash(vault), len(shared), len(vault)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// Registered legacy state: declared hash/count match, but the writer put
	// the wrong access bit in one row. The database seal validates counts; the
	// Go handoff must compare actual immutable rows, not trust their metadata.
	for i, a := range append(shared, vault...) {
		if i == len(shared) {
			a.Writable = !a.Writable
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_request_addresses(request_id,address,semantic_class,ordinal,account_role,is_writable)VALUES($1,$2,$3,$4,$5,$6)`, id, a.Address, a.SemanticClass, a.Ordinal, a.AccountRole, a.Writable); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET sealed_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWaitingFixture(ctx, s, l, p, missing); err == nil {
		t.Fatal("sealed incorrect access vector was adopted from declared hashes")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_provisioning_requests WHERE cluster=$1`, l.Cluster).Scan(&count); err != nil || count != 1 {
		t.Fatalf("collision allocated another demand: count=%d error=%v", count, err)
	}
}

func TestWaitingALTRegisteredCatalogLocksFenceConcurrentMutation(t *testing.T) {
	s, ctx := waitingALTStore(t)
	l, m := waitingIdentity(t, ctx, s, fmt.Sprint(time.Now().UnixNano()))
	family, tables := seedWaitingCatalog(t, ctx, s, l.Cluster, m.SharedAddresses)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := LockSharedALTManifestCoverage(ctx, tx, l.Cluster, m.SharedAddresses); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		sql string
		id  int64
	}{
		{`UPDATE loyal_yield.lookup_table_families SET active_generation=2 WHERE id=$1`, family},
		{`UPDATE loyal_yield.lookup_table_shared_market_catalog_heads SET readiness_state='provisioning',activated_at=NULL WHERE family_id=$1`, family},
		{`UPDATE loyal_yield.route_lookup_tables SET status='warming' WHERE id=$1`, tables[0]},
		{`UPDATE loyal_yield.lookup_table_addresses SET usable_after_slot=102 WHERE route_lookup_table_id=$1 AND ordinal=0`, tables[0]},
	} {
		writer, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
		_, err := s.pool.Exec(writer, mutation.sql, mutation.id)
		if err == nil || writer.Err() != context.DeadlineExceeded {
			cancel()
			t.Fatalf("catalog publication failed to hold mutation fence: error=%v context=%v", err, writer.Err())
		}
		cancel()
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
