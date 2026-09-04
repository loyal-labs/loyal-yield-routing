package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	e := FreshRouteEvidence{now, 100, 1, hash, 2, strings.Repeat("b", 64), accounts, policyBytes}
	decoded, err := ValidateFreshRouteEvidence(e, now, hash, strings.Repeat("b", 64), testVault, route.Protected)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.AccountIndex != 0 || len(decoded.InstructionData) != 2 {
		t.Fatal("policy decode incomplete")
	}
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

func TestRustCompatibleComputePadding(t *testing.T) {
	cases := map[uint64]uint64{0: 100_000, 1: 100_000, 200_000: 240_000, ^uint64(0): defaultComputeLimit}
	for measured, want := range cases {
		if got := paddedComputeUnits(measured); got != want {
			t.Fatalf("paddedComputeUnits(%d)=%d want %d", measured, got, want)
		}
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
	lease, err := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, false)
	if err != nil || lease == nil {
		t.Fatalf("claim: %+v %v", lease, err)
	}
	wireHash := strings.Repeat("f", 64)
	prepared := &RoutePreparation{RouteFingerprint: strings.Repeat("1", 64), RequirementsFingerprint: strings.Repeat("2", 64), ExecutionPlan: json.RawMessage(`{"message_base64":"AQ==","unsigned_wire_base64":"Ag==","simulation":{"succeeded":true}}`), Transaction: PreparedTransaction{Message: []byte{1}, UnsignedWire: []byte{2}, WireSHA256: wireHash, PacketBytes: 1, FeeLamports: 1, ComputeLimit: 1}, Simulation: SimulationEvidence{Slot: 1001, Succeeded: true, UnitsConsumed: 1, WireSHA256: wireHash}}
	if err := preserveCanonicalPlan(lease.ExecutionPlan, prepared, "prepared_transaction"); err != nil {
		t.Fatal(err)
	}
	commit := RevalidationCommit{Disposition: "fused_execute", Preparation: prepared, ConflictKeys: []string{"vault:" + position.VaultPubkey, "source-reserve:" + source.Address}, ExpectedEpochFingerprint: epoch.Fingerprint, ExpectedOpportunityKey: lease.IdempotencyKey}
	if err = store.CommitRevalidation(ctx, *lease, commit); err != nil {
		t.Fatal(err)
	}
	var state, kind string
	var reservations int
	var persisted []byte
	if err = store.pool.QueryRow(ctx, `SELECT opportunity_state,lease_kind,execution_plan FROM loyal_yield.rebalance_opportunities WHERE id=$1`, lease.OpportunityID).Scan(&state, &kind, &persisted); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1 AND reservation_state='active'`, lease.OpportunityID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if state != "leased" || kind != "execute" || reservations != 1 || !bytes.Contains(persisted, []byte("unsigned_wire_base64")) {
		t.Fatalf("incomplete atomic handoff state=%s kind=%s reservations=%d plan=%s", state, kind, reservations, persisted)
	}
	if err = store.CommitRevalidation(ctx, *lease, commit); err == nil {
		t.Fatal("lost revalidation lease was accepted after fused handoff")
	}
	// Exercise restart-safe waiting_alt -> leased/revalidate -> ready using the
	// same durable opportunity after clearing the synthetic execute handoff.
	for _, statement := range []string{`DELETE FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=$1`, `DELETE FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='revalidate',lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`} {
		if _, err = store.pool.Exec(ctx, statement, lease.OpportunityID); err != nil {
			t.Fatal(err)
		}
	}
	waitingLease, err := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, false)
	if err != nil || waitingLease == nil {
		t.Fatalf("waiting claim: %+v %v", waitingLease, err)
	}
	waiting := waitingALTPreparation(testRoute(), []string{testTarget}, 400_000)
	if err := preserveCanonicalPlan(waitingLease.ExecutionPlan, &waiting, "alt_readiness"); err != nil {
		t.Fatal(err)
	}
	if err = store.CommitRevalidation(ctx, *waitingLease, RevalidationCommit{Disposition: "waiting_alt", Preparation: &waiting, ExpectedEpochFingerprint: epoch.Fingerprint, ExpectedOpportunityKey: waitingLease.IdempotencyKey}); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(ctx, `SELECT opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id=$1`, lease.OpportunityID).Scan(&state); err != nil || state != "waiting_alt" {
		t.Fatalf("waiting_alt transition: %s %v", state, err)
	}
	readyLease, err := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, true)
	if err != nil || readyLease == nil {
		t.Fatalf("ready claim: %+v %v", readyLease, err)
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
	if nonFusedLease, claimErr := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, false); claimErr != nil || nonFusedLease != nil {
		t.Fatalf("non-fused revalidator stole executor-ready work: %+v %v", nonFusedLease, claimErr)
	}
	finalLease, err := store.ClaimRevalidation(ctx, cluster, "go-revalidator", time.Minute, true)
	if err != nil || finalLease == nil {
		t.Fatalf("ready recovery claim: %+v %v", finalLease, err)
	}
	if err = store.CommitRevalidation(ctx, *finalLease, RevalidationCommit{Disposition: "fused_execute", Preparation: prepared, ConflictKeys: []string{"vault:" + position.VaultPubkey}, ExpectedEpochFingerprint: epoch.Fingerprint, ExpectedOpportunityKey: finalLease.IdempotencyKey}); err != nil {
		t.Fatal(err)
	}
}
