package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
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
		testSource: {ReserveIdentity: ReserveIdentity{testSource, testMarket, USDCMint}, Slot: 100, SupplyAPYBPS: 100, TotalSupplyRaw: 1_000_000_000_000, EconomicLifetimeMillis: 120000},
		testTarget: {ReserveIdentity: ReserveIdentity{testTarget, testALT, USDCMint}, Slot: 100, SupplyAPYBPS: 900, TotalSupplyRaw: 1_000_000_000_000, EconomicLifetimeMillis: 120000},
		testPolicy: {ReserveIdentity: ReserveIdentity{testPolicy, testVault, USDCMint}, Slot: 100, SupplyAPYBPS: 500, TotalSupplyRaw: 1_000_000_000_000, EconomicLifetimeMillis: 120000},
	}}
	ampleCapacity(snapshot)
	vaults := make([]FleetVault, 3)
	for i := range vaults {
		p := VaultPosition{VaultID: int64(i + 1), VaultPubkey: testVault, PolicyAccount: testPolicy, SourceReserve: testSource, Market: testMarket, Mint: USDCMint, AmountRaw: 9_000_000_000, SourceCollateralAmountRaw: 9_000_000_000, SourceAmountSemantics: amountSemanticsKaminoCollateralDeposited}
		vaults[i] = FleetVault{Position: p, AllowedTargets: []string{testTarget, testPolicy}}
	}
	plan, err := PlanFleet(snapshot, vaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Opportunities) != 3 || len(plan.Rejections) != 0 {
		t.Fatalf("unexpected capacity wave: selected=%d rejected=%v", len(plan.Opportunities), plan.Rejections)
	}
	for i, target := range []string{testTarget, testTarget, testPolicy} {
		if plan.Opportunities[i].Decision.TargetReserve != target {
			t.Fatalf("move %d did not use the remaining target capacity", i)
		}
	}
	for i := range vaults {
		vaults[i].CommittedInflows = map[string]int64{testTarget: 10_000_000_000}
		vaults[i].CommittedOutflows = map[string]int64{}
	}
	withCommitted, err := PlanFleet(snapshot, vaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(withCommitted.Opportunities) != 3 {
		t.Fatalf("remaining alternative capacity was ignored: selected=%d", len(withCommitted.Opportunities))
	}
	inflows := map[string]int64{testTarget: 10_000_000_000}
	for _, o := range withCommitted.Opportunities {
		inflows[o.Decision.TargetReserve] += o.Decision.PrincipalUSDMicros
	}
	if inflows[testTarget] != 19_000_000_000 || inflows[testPolicy] != 18_000_000_000 {
		t.Fatalf("wrong capacity allocation: %v", inflows)
	}
}

func testRoute() []RouteInstruction {
	account := InstructionAccount{testSource, false, true}
	refresh := kamino.RefreshObligationDiscriminator[:]
	return []RouteInstruction{
		{Step: "kamino_refresh_obligation", Program: kamino.ProgramID.String(), Accounts: []InstructionAccount{account}, Data: append([]byte(nil), refresh...)},
		{Step: "withdraw", Program: kamino.ProgramID.String(), Accounts: []InstructionAccount{{testVault, true, true}, account}, Data: []byte{1, 2, 3}, Protected: true},
		{Step: "kamino_refresh_obligation", Program: kamino.ProgramID.String(), Accounts: []InstructionAccount{{testTarget, false, true}}, Data: append([]byte(nil), refresh...)},
		{Step: "deposit", Program: kamino.ProgramID.String(), Accounts: []InstructionAccount{{testVault, true, true}, {testTarget, false, true}}, Data: []byte{4, 5, 6}, Protected: true},
	}
}

func TestFreshPolicyWrapALTAndExactV0(t *testing.T) {
	route := testRoute()
	policyBytes, err := buildExactPolicyFixture(testMarket, testVault, 0, []RouteInstruction{route[1], route[3]})
	if err != nil {
		t.Fatal(err)
	}
	body, policies, err := wrapSameMintRoute(route, testVault, 0, testPolicy, squadsPolicyAccount(policyBytes), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(policies.accounts) != 1 || policies.accounts[0] != testPolicy || body[1].Data[17] != 0 || body[3].Data[17] != 1 {
		t.Fatalf("withdraw and deposit not executed under their exact route constraints: %+v", policies)
	}
	wrongPermissions := append([]byte(nil), policyBytes...)
	wrongPermissions[101] = 0xff
	if _, _, err = wrapSameMintRoute(route, testVault, 0, testPolicy, squadsPolicyAccount(wrongPermissions), "", nil); err == nil {
		t.Fatal("accepted noncanonical policy signer permissions")
	}
	if _, _, err = wrapSameMintRoute(route, testVault, 1, testPolicy, squadsPolicyAccount(policyBytes), "", nil); err == nil {
		t.Fatal("accepted a policy for another vault index")
	}
	all := []string{testSource, testTarget, testVault, testPolicy, testMarket, testMint}
	tables := []LookupTable{{Address: testALT, Addresses: all, Active: true, UsableAfterSlot: 99, LastVerifiedSlot: 100}}
	sim := func(w []byte) (SimulationEvidence, error) {
		h := sha256.Sum256(w)
		return SimulationEvidence{Slot: 101, Succeeded: true, UnitsConsumed: 200000, WireSHA256: hex.EncodeToString(h[:])}, nil
	}
	prep, err := prepareRoute(body, testVault, tables, testMarket, 5000, 400000, sim, "same_mint_kamino_v0")
	if err != nil {
		t.Fatal(err)
	}
	if prep.Transaction.Message[0] != 0x80 || prep.Transaction.PacketBytes != len(prep.Transaction.UnsignedWire) || prep.Transaction.PacketBytes > SolanaPacketLimit {
		t.Fatal("not an exact bounded v0 transaction")
	}
	if !strings.Contains(string(prep.ExecutionPlan), "unsigned_transaction_base64") {
		t.Fatal("exact bytes not durably handoff-ready")
	}
	if _, err := prepareRoute(body, testVault, nil, testMarket, 5000, 400000, sim, "same_mint_kamino_v0"); err == nil {
		t.Fatal("missing ALT was accepted")
	}
	oversized := append([]RouteInstruction(nil), body...)
	oversized[0].Data = make([]byte, 1400)
	if _, err := prepareRoute(oversized, testVault, tables, testMarket, 5000, 400000, sim, "same_mint_kamino_v0"); err == nil || !strings.Contains(err.Error(), "packet") {
		t.Fatalf("oversized packet accepted: %v", err)
	}
	failSim := func(w []byte) (SimulationEvidence, error) {
		h := sha256.Sum256(w)
		return SimulationEvidence{Slot: 101, Succeeded: false, UnitsConsumed: 1, WireSHA256: hex.EncodeToString(h[:])}, nil
	}
	if _, err := prepareRoute(body, testVault, tables, testMarket, 5000, 400000, failSim, "same_mint_kamino_v0"); err == nil {
		t.Fatal("simulation failure accepted")
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
	revalidator := &Revalidator{rpc: testChain(t, server.URL)}
	verified, err := revalidator.verifyRouteLookupTables(context.Background(), tables, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(verified) != tableCount {
		t.Fatalf("fresh ALT verification calls=%d tables=%d", calls, len(verified))
	}
}

func TestComputePadding(t *testing.T) {
	cases := map[uint64]uint64{0: 100_000, 1: 100_000, 200_000: 240_000, ^uint64(0): DefaultComputeLimit}
	for measured, want := range cases {
		if got := paddedComputeUnits(measured); got != want {
			t.Fatalf("paddedComputeUnits(%d)=%d want %d", measured, got, want)
		}
	}
}

func TestAdmissionEconomicsPricesCommittedInflight(t *testing.T) {
	plan := json.RawMessage(`{"observed_target_apy_bps":919,"source_apy_bps":81,"confidence_ppm":950000,"holding_horizon_seconds":2592000,"estimated_execution_cost_usd_micros":100000}`)
	lease := RevalidationLease{PrincipalUSDMicros: 9_000_000_000, ExecutionPlan: plan}
	withoutCommitted, err := ComputeAdmissionEconomics(lease, lease.ExecutionPlan, 1_000_000_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	withCommitted, err := ComputeAdmissionEconomics(lease, lease.ExecutionPlan, 1_000_000_000_000, 20_000_000_000)
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
