package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	testVault  = "3ZvESShPULHcJSWHaMPv4GKzH2zebR6AEWKwDShmPfFs"
	testSource = "4vJ9JU1bJJE96FWSJKvHsmmFADCg4gpZQff4P3bkLKi"
	testTarget = "swqrv48gsrwpBFbftEwnP2vB4jckpvfGJfXkwaniLCC"
	testMarket = "8qbHbw2BbbTHBW1sbeqakYXVKRQM8Ne7pLK7m6CVfeR"
	testMint   = "3S5e9qmNHjhA2G1Ghkk5UWnTniaFFHiX7gzd6gcZtzcT"
	testPolicy = "GgBaCs3NCBuZN12kCJgAW63ydqohFkHEdfdEXBPzLHq"
	testALT    = "ws91DX9HBAAxGW77BZs5FogRDwpRtcUpiLBpKdPTfWu"
)

func TestPlanFleetCapacityAwareWave(t *testing.T) {
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Slot: 100, ObservedAt: now, Hash: strings.Repeat("a", 64), Reserves: map[string]ReserveState{
		testSource: {ReserveIdentity: ReserveIdentity{testSource, testMarket, USDCMint}, Slot: 100, SupplyAPYBPS: 100, TotalSupplyUSDMicros: 1_000_000_000_000, EconomicLifetimeMillis: 120000},
		testTarget: {ReserveIdentity: ReserveIdentity{testTarget, testALT, USDCMint}, Slot: 100, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 1_000_000_000_000, EconomicLifetimeMillis: 120000},
		testPolicy: {ReserveIdentity: ReserveIdentity{testPolicy, testVault, USDCMint}, Slot: 100, SupplyAPYBPS: 500, TotalSupplyUSDMicros: 1_000_000_000_000, EconomicLifetimeMillis: 120000},
	}}
	vaults := make([]FleetVault, 3)
	for i := range vaults {
		p := VaultPosition{VaultID: int64(i + 1), SnapshotID: int64(i + 10), VaultPubkey: testVault, PolicyAccount: testPolicy, SourceReserve: testSource, Market: testMarket, Mint: USDCMint, AmountRaw: 9_000_000_000, SourceCollateralAmountRaw: 9_000_000_000, SourceAmountSemantics: amountSemanticsRedeemableLiquidity}
		vaults[i] = FleetVault{Position: p, AllowedTargets: []string{testTarget, testPolicy}}
	}
	plan, err := PlanFleet(snapshot, vaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Opportunities) != 2 || plan.Rejections[3] != "target_capacity_exhausted" {
		t.Fatalf("unexpected capacity wave: %#v", plan)
	}
	if plan.Opportunities[0].IdempotencyKey == plan.Opportunities[1].IdempotencyKey || len(plan.Opportunities[0].IdempotencyKey) != 64 {
		t.Fatal("opportunity identities are not canonical")
	}
	for i := range vaults {
		vaults[i].CommittedInflows = map[string]int64{testTarget: 10_000_000_000}
		vaults[i].CommittedOutflows = map[string]int64{}
	}
	withCommitted, err := PlanFleet(snapshot, vaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(withCommitted.Opportunities) != 1 {
		t.Fatalf("existing committed capacity was ignored: %#v", withCommitted)
	}
}

func testRoute() KaminoSameMintRoute {
	account := InstructionAccount{testSource, false, true}
	refresh := []byte{33, 132, 147, 228, 151, 192, 72, 89}
	return KaminoSameMintRoute{Public: []RouteInstruction{
		{"kamino_refresh_obligation", KLendProgram, []InstructionAccount{account}, append([]byte(nil), refresh...)},
		{"kamino_refresh_obligation", KLendProgram, []InstructionAccount{{testTarget, false, true}}, append([]byte(nil), refresh...)},
	}, Protected: []RouteInstruction{{"withdraw", KLendProgram, []InstructionAccount{{testVault, true, true}, account}, []byte{1, 2, 3}}, {"deposit", KLendProgram, []InstructionAccount{{testVault, true, true}, {testTarget, false, true}}, []byte{4, 5, 6}}}}
}
func TestFreshPolicyWrapALTAndExactV0(t *testing.T) {
	route := testRoute()
	policyBytes, err := BuildExactPolicyFixture(testMarket, testVault, 0, route.Protected)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	hash := strings.Repeat("a", 64)
	accounts := []FreshAccount{{"vault", testVault, SquadsProgram, hash, 100, false, true}, {"reserve", testSource, KLendProgram, hash, 100, false, true}, {"reserve", testTarget, KLendProgram, hash, 100, false, true}, {"obligation", testPolicy, KLendProgram, hash, 100, false, true}, {"obligation", testMarket, KLendProgram, hash, 100, false, true}, {"token_account", testMint, "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", hash, 100, false, true}, {"farm", testALT, farmsProgram, hash, 100, false, true}, {"farm", testSource, farmsProgram, hash, 100, false, true}, {"policy", testPolicy, SquadsProgram, hash, 100, false, true}}
	e := FreshRouteEvidence{ObservedAt: now, Slot: 100, OpportunityID: 1, OpportunityKey: hash, EpochID: 2, EpochFingerprint: strings.Repeat("b", 64), Accounts: accounts, PolicyData: policyBytes}
	decoded, err := ValidateFreshRouteEvidence(e, now, hash, strings.Repeat("b", 64), testVault, route.Protected)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.AccountIndex != 0 || len(decoded.InstructionData) != 2 {
		t.Fatal("policy decode incomplete")
	}
	wrongPermissions := append([]byte(nil), policyBytes...)
	wrongPermissions[101] = 0xff
	e.PolicyData = wrongPermissions
	if _, err = ValidateFreshRouteEvidence(e, now, hash, strings.Repeat("b", 64), testVault, route.Protected); err == nil {
		t.Fatal("accepted noncanonical policy signer permissions")
	}
	e.PolicyData = policyBytes
	all := []string{testSource, testTarget, testVault, testPolicy, testMarket, testMint}
	sim := func(w []byte) (SimulationEvidence, error) {
		h := sha256.Sum256(w)
		return SimulationEvidence{Slot: 101, Succeeded: true, UnitsConsumed: 200000, WireSHA256: hex.EncodeToString(h[:])}, nil
	}
	prep, err := PrepareRoute(route, testPolicy, testVault, 0, []uint8{0, 1}, []LookupTable{{testALT, all, true, 99, 100}}, testMarket, 5000, 400000, sim)
	if err != nil {
		t.Fatal(err)
	}
	if prep.Transaction.Message[0] != 0x80 || prep.Transaction.PacketBytes != len(prep.Transaction.UnsignedWire) || prep.Transaction.PacketBytes > SolanaPacketLimit {
		t.Fatal("not an exact bounded v0 transaction")
	}
	if !strings.Contains(string(prep.ExecutionPlan), "unsigned_transaction_base64") {
		t.Fatal("exact bytes not durably handoff-ready")
	}
	if _, err := PrepareRoute(route, testPolicy, testVault, 0, []uint8{0, 1}, nil, testMarket, 5000, 400000, sim); err == nil {
		t.Fatal("missing ALT was accepted")
	}
	oversized := route
	oversized.Public = append([]RouteInstruction(nil), route.Public...)
	oversized.Public[0].Data = make([]byte, 1400)
	if _, err := PrepareRoute(oversized, testPolicy, testVault, 0, []uint8{0, 1}, []LookupTable{{testALT, all, true, 99, 100}}, testMarket, 5000, 400000, sim); err == nil || !strings.Contains(err.Error(), "packet") {
		t.Fatalf("oversized packet accepted: %v", err)
	}
	failSim := func(w []byte) (SimulationEvidence, error) {
		h := sha256.Sum256(w)
		return SimulationEvidence{Slot: 101, Succeeded: false, UnitsConsumed: 1, WireSHA256: hex.EncodeToString(h[:])}, nil
	}
	if _, err := PrepareRoute(route, testPolicy, testVault, 0, []uint8{0, 1}, []LookupTable{{testALT, all, true, 99, 100}}, testMarket, 5000, 400000, failSim); err == nil {
		t.Fatal("simulation failure accepted")
	}
	e.OpportunityKey = "changed"
	if _, err := ValidateFreshRouteEvidence(e, now, hash, strings.Repeat("b", 64), testVault, route.Protected); err == nil {
		t.Fatal("changed opportunity accepted")
	}
	e.OpportunityKey = hash
	e.EpochFingerprint = "changed"
	if _, err := ValidateFreshRouteEvidence(e, now, hash, strings.Repeat("b", 64), testVault, route.Protected); err == nil {
		t.Fatal("changed epoch accepted")
	}
}

func TestVerifyLookupTablesChunksFreshRPCAtSolanaLimit(t *testing.T) {
	const tableCount = 205
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var addresses []string
		if body.Method != "getMultipleAccounts" || len(body.Params) < 1 || json.Unmarshal(body.Params[0], &addresses) != nil {
			t.Errorf("unexpected RPC request: %+v", body)
			return
		}
		calls++
		if len(addresses) == 0 || len(addresses) > 100 {
			t.Errorf("getMultipleAccounts batch size=%d", len(addresses))
			return
		}
		values := make([]map[string]any, len(addresses))
		for index := range addresses {
			member, err := decodeBase58(testIdentity(byte(index + calls)))
			if err != nil {
				t.Error(err)
				return
			}
			data := make([]byte, 56+32)
			binary.LittleEndian.PutUint32(data[:4], 1)
			binary.LittleEndian.PutUint64(data[4:12], ^uint64(0))
			copy(data[56:], member)
			values[index] = map[string]any{"owner": altProgram, "lamports": 1, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]any{"slot": 500}, "value": values}})
	}))
	defer server.Close()

	tables := make([]LookupTable, tableCount)
	for index := range tables {
		call := index/100 + 1
		member := testIdentity(byte(index%100 + call))
		tables[index] = LookupTable{Address: testIdentity(byte(index + 1)), Addresses: []string{member}}
	}
	revalidator := &Revalidator{rpc: NewRPCClient(server.URL)}
	verified, err := revalidator.verifyLookupTables(context.Background(), tables, 499)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(verified) != tableCount {
		t.Fatalf("fresh ALT verification calls=%d tables=%d", calls, len(verified))
	}
}

func TestLoadReusableLookupTablesScopesStaleCandidatesBeforeRPC(t *testing.T) {
	databaseURL := os.Getenv("FLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cluster := fmt.Sprintf("alt-scope-%d", time.Now().UnixNano())
	var familyID int64
	if err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_families(cluster,logical_name,kind,planner_version,catalog_version,active_generation,provisioning_authority,payer,hard_capacity,largest_atomic_expansion,safety_margin,allocation_high_water) VALUES($1,'shared','shared_market','test','test',0,$2,$3,256,1,1,254) RETURNING id`, cluster, testPolicy, testVault).Scan(&familyID); err != nil {
		t.Fatal(err)
	}
	const globalTables = 106
	for index := 0; index < globalTables; index++ {
		tableAddress := testIdentity(byte(index + 100))
		member := testIdentity(byte(index + 1))
		if index == globalTables-1 {
			member = testTarget
		}
		var tableID int64
		if err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_lookup_tables(cluster,scope,table_address,authority,payer,status,durable,address_count,address_hash,addresses,last_extended_slot,warmup_slot,family_id,allocation_kind,generation,shard_ordinal,desired_state,accepting_allocations,allocation_high_water,reserved_address_count,usable_address_count,last_verified_slot,last_verified_at,mutation_epoch) VALUES($1,$2,$3,$4,$5,'active',TRUE,1,$6,jsonb_build_array($7),10,11,$8,'shared_market',0,$9,'active',TRUE,254,1,1,10,clock_timestamp(),0) RETURNING id`, cluster, fmt.Sprintf("shared-%d", index), tableAddress, testPolicy, testVault, strings.Repeat("a", 64), member, familyID, index).Scan(&tableID); err != nil {
			t.Fatalf("seed table %d: %v", index, err)
		}
		if _, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_addresses(route_lookup_table_id,address,ordinal,added_slot,usable_after_slot,last_verified_slot,last_verified_at) VALUES($1,$2,0,9,11,10,clock_timestamp())`, tableID, member); err != nil {
			t.Fatalf("seed membership %d: %v", index, err)
		}
	}
	tables, err := store.LoadReusableLookupTables(ctx, cluster, 1, 1_000, []string{testTarget})
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 1 || len(tables[0].Addresses) != 1 || tables[0].Addresses[0] != testTarget || tables[0].LastVerifiedSlot != 10 {
		t.Fatalf("stale relevant ALT was not scoped for fresh verification: %+v", tables)
	}
}

func TestRustCompatibleComputePadding(t *testing.T) {
	cases := map[uint64]uint64{0: 100_000, 1: 100_000, 200_000: 240_000, ^uint64(0): defaultComputeLimit}
	for measured, want := range cases {
		if got := paddedComputeUnits(measured); got != want {
			t.Fatalf("paddedComputeUnits(%d)=%d want %d", measured, got, want)
		}
	}
}

func TestRecomputeReservationEconomicsUsesLockedCommittedInflight(t *testing.T) {
	plan := json.RawMessage(`{"observed_target_apy_bps":919,"source_apy_bps":81,"confidence_ppm":950000,"holding_horizon_seconds":2592000,"estimated_execution_cost_usd_micros":100000}`)
	lease := RevalidationLease{PrincipalUSDMicros: 9_000_000_000, ExecutionPlan: plan}
	withoutCommitted, err := recomputeReservationEconomics(lease, lease.ExecutionPlan, 1_000_000_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	withCommitted, err := recomputeReservationEconomics(lease, lease.ExecutionPlan, 1_000_000_000_000, 20_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if withCommitted.ProjectedTargetAPYBPS >= withoutCommitted.ProjectedTargetAPYBPS {
		t.Fatalf("committed inflight was not priced: without=%+v with=%+v", withoutCommitted, withCommitted)
	}
	if withCommitted.ObservedTargetAPYBPS != 919 || withCommitted.ProjectedTargetAPYBPS == withCommitted.ObservedTargetAPYBPS {
		t.Fatalf("observed/projected APY evidence was not kept distinct: %+v", withCommitted)
	}
}

func TestRecomputeReservationEconomicsRejectsEdgeLostUnderLock(t *testing.T) {
	plan := json.RawMessage(`{"observed_target_apy_bps":100,"source_apy_bps":99,"confidence_ppm":950000,"holding_horizon_seconds":2592000,"estimated_execution_cost_usd_micros":100000}`)
	_, err := recomputeReservationEconomics(RevalidationLease{PrincipalUSDMicros: 9_000_000_000, ExecutionPlan: plan}, plan, 1_000_000_000, 20_000_000)
	if err == nil {
		t.Fatal("economics made ineligible by committed inflight were accepted")
	}
}

func TestRevalidationStoreIntegrationFusedExecuteIsAtomic(t *testing.T) {
	databaseURL := os.Getenv("FLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	cluster := "revalidation-" + suffix
	market := testIdentity(42)
	source := ReserveIdentity{Address: testIdentity(43), Market: market, Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(44), Market: market, Mint: USDCMint}
	vaultID := seedWorkerVault(t, ctx, store, suffix, market, source.Address)
	position, err := store.LoadVaultPosition(ctx, cluster, vaultID, source, target)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Slot: 1000, ObservedAt: now, Hash: "revalidation-" + suffix, Reserves: map[string]ReserveState{source.Address: {ReserveIdentity: source, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 100, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("a", 64)}, target.Address: {ReserveIdentity: target, Slot: 1000, LastUpdateSlot: 1000, SupplyAPYBPS: 900, TotalSupplyUSDMicros: 1_000_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("b", 64)}}}
	epoch := testImmutableMarketEpoch(t, snapshot, source, target)
	decision := Plan(snapshot, position, source.Address, target.Address)
	if !decision.Eligible {
		t.Fatalf("fixture ineligible: %s", decision.Reason)
	}
	published, err := store.Publish(ctx, cluster, epoch, position, decision)
	if err != nil || !published.Inserted {
		t.Fatalf("publish: %+v %v", published, err)
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.target_capacity_frontiers(cluster,target_reserve,liquidity_mint,observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros,telemetry_version) VALUES($1,$2,$3,$4,$5,$6,1)`, cluster, target.Address, USDCMint, int64(1_000_000_000_000_000), 1000, int64(20_000_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, false, false)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	wireHash := strings.Repeat("f", 64)
	prepared := &RoutePreparation{RouteFingerprint: strings.Repeat("1", 64), RequirementsFingerprint: strings.Repeat("2", 64), ExecutionPlan: json.RawMessage(`{"message_base64":"AQ==","unsigned_wire_base64":"Ag==","simulation":{"succeeded":true}}`), Transaction: PreparedTransaction{Message: []byte{1}, UnsignedWire: []byte{2}, WireSHA256: wireHash, PacketBytes: 1, FeeLamports: 1, ComputeLimit: 1}, Simulation: SimulationEvidence{Slot: 1001, Succeeded: true, UnitsConsumed: 1, WireSHA256: wireHash}}
	if err := preserveCanonicalPlan(lease.ExecutionPlan, prepared, "prepared_transaction"); err != nil {
		t.Fatal(err)
	}
	commit := RevalidationCommit{Disposition: "fused_execute", Preparation: prepared, ConflictKeys: []string{"vault:" + position.VaultPubkey, "source-reserve:" + source.Address}, ExpectedEpochFingerprint: epoch.Fingerprint, ExpectedOpportunityKey: lease.IdempotencyKey, FreshEconomics: true, ObservedSourceAPYBPS: 100, ObservedTargetAPYBPS: 900, TargetObservedSupplyUSDMicros: 1_000_000_000_000_000, TargetObservedSlot: 1000}
	if err = store.CommitRevalidation(ctx, *lease, commit); err != nil {
		t.Fatal(err)
	}
	var state, kind string
	var reservations int
	var observedTargetAPY, projectedTargetAPY, admittedSourceAPY int64
	var persisted []byte
	if err = store.pool.QueryRow(ctx, `SELECT opportunity_state,lease_kind,execution_plan FROM loyal_yield.rebalance_opportunities WHERE id=$1`, lease.OpportunityID).Scan(&state, &kind, &persisted); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(ctx, `SELECT count(*),min(admitted_observed_target_apy_bps),min(admitted_projected_target_apy_bps),min(admitted_source_apy_bps) FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1 AND reservation_state='active'`, lease.OpportunityID).Scan(&reservations, &observedTargetAPY, &projectedTargetAPY, &admittedSourceAPY); err != nil {
		t.Fatal(err)
	}
	if state != "leased" || kind != "execute" || reservations != 1 || observedTargetAPY != 900 || projectedTargetAPY >= observedTargetAPY || admittedSourceAPY != 100 || !bytes.Contains(persisted, []byte("unsigned_wire_base64")) {
		t.Fatalf("incomplete atomic handoff state=%s kind=%s reservations=%d observed=%d projected=%d source=%d plan=%s", state, kind, reservations, observedTargetAPY, projectedTargetAPY, admittedSourceAPY, persisted)
	}
	if err = store.CommitRevalidation(ctx, *lease, commit); err == nil {
		t.Fatal("lost revalidation lease was accepted after fused handoff")
	}
	// A live reservation remains the authoritative source/target commitment
	// after its opportunity leaves the active queue. Another vault must see it
	// exactly once rather than omitting it or double-counting queue intent.
	otherVaultID := seedWorkerVault(t, ctx, store, suffix+"-reservation-frontier", market, source.Address)
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='stale',lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL,terminal_reason='test_terminal_with_live_reservation' WHERE id=$1`, lease.OpportunityID); err != nil {
		t.Fatal(err)
	}
	frontierFleet, err := store.LoadMigratedFleet(ctx, cluster, epoch, FleetLoadOptions{OptimizerEpochID: lease.OptimizerEpochID})
	if err != nil {
		t.Fatal(err)
	}
	var frontierVault *FleetVault
	for index := range frontierFleet {
		if frontierFleet[index].Position.VaultID == otherVaultID {
			frontierVault = &frontierFleet[index]
			break
		}
	}
	if frontierVault == nil || frontierVault.CommittedInflows[target.Address] != lease.PrincipalUSDMicros || frontierVault.CommittedOutflows[source.Address] != lease.PrincipalUSDMicros {
		t.Fatalf("live reservation was not counted exactly once after terminal queue state: vault=%+v principal=%d", frontierVault, lease.PrincipalUSDMicros)
	}
	// Exercise restart-safe waiting_alt -> leased/revalidate -> ready using the
	// same durable opportunity after clearing the synthetic execute handoff.
	for _, statement := range []string{`DELETE FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=$1`, `DELETE FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='revalidate',lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`} {
		if _, err = store.pool.Exec(ctx, statement, lease.OpportunityID); err != nil {
			t.Fatal(err)
		}
	}
	waitingLease, err := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, false, false)
	if err != nil || waitingLease == nil {
		t.Fatalf("waiting claim: %+v %v", waitingLease, err)
	}
	waiting := waitingALTPreparation(testRoute(), []string{testTarget}, 400_000)
	if err := preserveCanonicalPlan(waitingLease.ExecutionPlan, &waiting, "alt_readiness"); err != nil {
		t.Fatal(err)
	}
	if err = store.CommitRevalidation(ctx, *waitingLease, RevalidationCommit{Disposition: "waiting_alt", Preparation: &waiting, MissingAddresses: []string{testTarget}, ExpectedEpochFingerprint: epoch.Fingerprint, ExpectedOpportunityKey: waitingLease.IdempotencyKey}); err != nil {
		t.Fatal(err)
	}
	var requestStatus, requestAddress string
	var requestSealed bool
	if err = store.pool.QueryRow(ctx, `SELECT opportunity.opportunity_state,request.request_status,request.sealed_at IS NOT NULL,address.address FROM loyal_yield.rebalance_opportunities opportunity JOIN loyal_yield.lookup_table_provisioning_request_consumers consumer ON consumer.opportunity_id=opportunity.id JOIN loyal_yield.lookup_table_provisioning_requests request ON request.id=consumer.provisioning_request_id JOIN loyal_yield.lookup_table_provisioning_request_addresses address ON address.request_id=request.id WHERE opportunity.id=$1`, lease.OpportunityID).Scan(&state, &requestStatus, &requestSealed, &requestAddress); err != nil || state != "waiting_alt" || requestStatus != "requested" || !requestSealed || requestAddress != testTarget {
		t.Fatalf("waiting_alt durable request: state=%s status=%s sealed=%t address=%s err=%v", state, requestStatus, requestSealed, requestAddress, err)
	}
	blockedLease, err := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, true, false)
	if err != nil || blockedLease != nil {
		t.Fatalf("waiting_alt was reclaimable before satisfaction: %+v %v", blockedLease, err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests request SET request_status='satisfied',satisfied_at=clock_timestamp(),updated_at=clock_timestamp() FROM loyal_yield.lookup_table_provisioning_request_consumers consumer WHERE consumer.provisioning_request_id=request.id AND consumer.opportunity_id=$1`, lease.OpportunityID); err != nil {
		t.Fatal(err)
	}
	blockedLease, err = store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, true, false)
	if err != nil || blockedLease != nil {
		t.Fatalf("ALT satisfaction bypassed planner readmission: %+v %v", blockedLease, err)
	}
	readmitted, err := store.Publish(ctx, cluster, epoch, position, decision)
	if err != nil || readmitted.Reason != "alt_readmitted" || readmitted.OpportunityID != lease.OpportunityID {
		t.Fatalf("planner ALT readmission: %+v %v", readmitted, err)
	}
	readyLease, err := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, true, false)
	if err != nil || readyLease == nil {
		t.Fatalf("ready claim after planner readmission: %+v %v", readyLease, err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.target_capacity_frontiers SET maximum_inflight_usd_micros=$4 WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3`, cluster, target.Address, USDCMint, readyLease.PrincipalUSDMicros-1); err != nil {
		t.Fatal(err)
	}
	readyCommit := RevalidationCommit{Disposition: "ready", Preparation: prepared, ExpectedEpochFingerprint: epoch.Fingerprint, ExpectedOpportunityKey: readyLease.IdempotencyKey}
	if err = store.CommitRevalidation(ctx, *readyLease, readyCommit); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("exhausted capacity accepted: %v", err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.target_capacity_frontiers SET maximum_inflight_usd_micros=$4 WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3`, cluster, target.Address, USDCMint, int64(20_000_000_000_000)); err != nil {
		t.Fatal(err)
	}
	if err = store.CommitRevalidation(ctx, *readyLease, readyCommit); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(ctx, `SELECT opportunity_state,COALESCE(lease_kind,'') FROM loyal_yield.rebalance_opportunities WHERE id=$1`, lease.OpportunityID).Scan(&state, &kind); err != nil || state != "ready" || kind != "" {
		t.Fatalf("ready transition: %s/%s %v", state, kind, err)
	}
	if nonFusedLease, claimErr := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, false, false); claimErr != nil || nonFusedLease != nil {
		t.Fatalf("non-fused revalidator stole executor-ready work: %+v %v", nonFusedLease, claimErr)
	}
	finalLease, err := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, true, false)
	if err != nil || finalLease == nil {
		t.Fatalf("ready recovery claim: %+v %v", finalLease, err)
	}
	if err = store.CommitRevalidation(ctx, *finalLease, RevalidationCommit{Disposition: "fused_execute", Preparation: prepared, ConflictKeys: []string{"vault:" + position.VaultPubkey}, ExpectedEpochFingerprint: epoch.Fingerprint, ExpectedOpportunityKey: finalLease.IdempotencyKey}); err != nil {
		t.Fatal(err)
	}
	// Go must claim the cross-mint lane only when explicitly enabled. This is
	// the durable ownership fence that replaces the Rust route revalidator.
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=$1`, []any{lease.OpportunityID}},
		{`DELETE FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, []any{lease.OpportunityID}},
		{`UPDATE loyal_yield.route_policies SET route_modes=array_append(route_modes,'cross_mint_jupiter'),cluster=$2,source_commitment='finalized',finalized_eligible=true WHERE id=$1 AND NOT ('cross_mint_jupiter'=ANY(route_modes))`, []any{position.PolicyID, cluster}},
		{`UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='revalidate',lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL,source_liquidity_mint=$2,target_liquidity_mint=$3,liquidity_mint=$3,execution_plan=jsonb_set(jsonb_set(jsonb_set(execution_plan,'{kind}',to_jsonb('cross_mint_jupiter'::text)),'{route_kind}',to_jsonb('cross_mint_jupiter'::text)),'{policy_bindings}',jsonb_build_object('delegated_signer','test-signer','withdraw',jsonb_build_object('policy_account',$4::text))) WHERE id=$1`, []any{lease.OpportunityID, USDCMint, USDTMint, position.PolicyAccount}},
	} {
		if _, err = store.pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if disabled, claimErr := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, false, false); claimErr != nil || disabled != nil {
		t.Fatalf("disabled Go revalidator claimed cross-mint work: %+v %v", disabled, claimErr)
	}
	crossLease, claimErr := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, false, true)
	if claimErr != nil || crossLease == nil || crossLease.RouteKind != "cross_mint_jupiter" || crossLease.SourceLiquidityMint != USDCMint || crossLease.TargetLiquidityMint != USDTMint {
		t.Fatalf("enabled Go revalidator did not claim cross-mint work: %+v %v", crossLease, claimErr)
	}
	// Preserve the verifier's retained-executor handoff artifact after this
	// ownership assertion; the cross-mint claim itself was already observed.
	if _, err = store.pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='leased',lease_kind='execute' WHERE id=$1`, lease.OpportunityID); err != nil {
		t.Fatal(err)
	}
}
