package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
)

// Reuse the existing complete-return fixture, changing only the route's concrete
// identities and token program. Real compilers/admission run against controlled
// transport; no simulation, signer or live-program success is claimed here.
func usdcReturnFixture(t *testing.T) (Observation, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	old := ethenaUSDePYUSD
	route, _ := runtimeRoute(SelectedRouteID)
	o, _, _, m, oldRPC, _, existing := payoffAdmissionFixture(t, 20_000)
	addresses := []string{old.Kamino.CollateralReserve, old.Kamino.CollateralMint, old.Kamino.DebtMint, reportTicketPDA}
	for _, p := range m.RuntimeBindings.BridgePolicies {
		addresses = append(addresses, p.Account)
	}
	_, extra, err := oldRPC.GetMultipleAccounts(context.Background(), addresses, 42)
	if err != nil {
		t.Fatal(err)
	}
	existing = append(existing, extra...)
	pairs := [][2]string{
		{old.Kamino.Market, route.Kamino.Market}, {old.Kamino.MarketAuthority, route.Kamino.MarketAuthority},
		{old.Kamino.Obligation, route.Kamino.Obligation}, {old.Kamino.CollateralReserve, route.Kamino.CollateralReserve},
		{old.Kamino.DebtReserve, route.Kamino.DebtReserve}, {old.Kamino.CollateralMint, route.Kamino.CollateralMint},
		{old.Kamino.DebtMint, route.Kamino.DebtMint}, {old.CollateralCustody, route.CollateralCustody},
		{old.DebtCustody, route.DebtCustody}, {old.CollateralLiquiditySupply, route.CollateralLiquiditySupply},
		{old.DebtLiquiditySupply, route.DebtLiquiditySupply}, {old.DebtFeeReceiver, route.DebtFeeReceiver},
		{old.CollateralReceiptMint, route.CollateralReceiptMint}, {old.CollateralReceiptSupply, route.CollateralReceiptSupply},
	}
	var accounts []ConfirmedAccount
	accounts = append(accounts, marketFixture(t, route.Kamino.Market))
	for _, a := range existing {
		a.Data = append([]byte(nil), a.Data...)
		for _, p := range pairs {
			if a.Address == p[0] {
				a.Address = p[1]
			}
			from, e1 := decodeKey(p[0])
			to, e2 := decodeKey(p[1])
			if e1 == nil && e2 == nil {
				a.Data = bytes.ReplaceAll(a.Data, from[:], to[:])
			}
		}
		if a.Address == route.DebtCustody || a.Address == route.DebtLiquiditySupply || a.Address == bridgeUSDC {
			a.Owner = classicTokenProgram
		}
		accounts = append(accounts, a)
	}
	// Basic-policy hash validation remains active with controlled policy bytes.
	for _, family := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
		b, _ := basicPolicyBinding(family)
		data := []byte("controlled-basic-policy:" + string(family))
		hash := sha256Bytes(data)
		switch family {
		case BasicCollateralLifecycle:
			m.RuntimeBindings.CollateralLifecycle.DataSHA256 = &hash
		case BasicDebtLifecycle:
			m.RuntimeBindings.DebtLifecycle.DataSHA256 = &hash
		case BasicSwapRoutesA:
			m.RuntimeBindings.SwapRoutesA.DataSHA256 = &hash
		case BasicSwapRoutesB:
			m.RuntimeBindings.SwapRoutesB.DataSHA256 = &hash
		}
		accounts = append(accounts, ConfirmedAccount{Address: b.Policy, Owner: bridgeSquadsProgram, Lamports: 1, Data: data})
	}
	tables := retainedJupiterLookups(t)
	for _, leg := range []string{"syrupUSDC->USDC"} {
		req, record := basicJupiterRequestFromExport(t, route.Lane, leg)
		tables = append(tables, retainedOrReconstructedLookupTables(t, req.Instruction.LookupTableAddresses, legacyMessageKeys(t, record.MessageBase64), []string{record.PolicyAccount})...)
	}
	for _, table := range tables {
		binary.LittleEndian.PutUint64(table.Data[12:20], 41)
		accounts = append(accounts, ConfirmedAccount{Address: table.Address, Owner: table.Owner, Lamports: table.Lamports, Data: table.Data})
	}
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
	raw, err := os.ReadFile(basicJupiterFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var headers struct {
		Rows []struct {
			Key          string
			LookupTables []string
			Instruction  struct {
				ProgramID, DataBase64 string
				Accounts              []JupiterInstructionAccount
			}
		}
	}
	if err = json.Unmarshal(raw, &headers); err != nil {
		t.Fatal(err)
	}
	var instruction JupiterSwapInstruction
	client, _ := newJupiterClient("https://jupiter.invalid", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload any
		if req.Method == "GET" {
			q := req.URL.Query()
			amount, err := strconv.ParseUint(q.Get("amount"), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			key, out := "syrupUSDC->USDC", amount/1000
			if q.Get("inputMint") == bridgeUSDC {
				key, out = "USDC->syrupUSDC", amount*1000
			}
			for _, row := range headers.Rows {
				if row.Key == key {
					instruction = JupiterSwapInstruction{ProgramID: row.Instruction.ProgramID, Data: row.Instruction.DataBase64, Accounts: row.Instruction.Accounts, LookupTableAddresses: row.LookupTables}
				}
			}
			wire, err := base64.StdEncoding.DecodeString(instruction.Data)
			if err != nil || len(wire) < 28 {
				t.Fatal("missing basic quote")
			}
			binary.LittleEndian.PutUint64(wire[len(wire)-19:], amount)
			binary.LittleEndian.PutUint64(wire[len(wire)-11:], out)
			binary.LittleEndian.PutUint16(wire[len(wire)-3:], 50)
			instruction.Data = base64.StdEncoding.EncodeToString(wire)
			payload = JupiterQuote{InputMint: q.Get("inputMint"), OutputMint: q.Get("outputMint"), InAmount: fmt.Sprint(amount), OutAmount: fmt.Sprint(out), OtherAmountThreshold: fmt.Sprint(out * 9950 / 10000), SwapMode: "ExactIn", SlippageBPS: 50, RoutePlan: []json.RawMessage{json.RawMessage(`{}`)}}
		} else {
			payload = map[string]any{"swapInstruction": instruction, "addressLookupTableAddresses": instruction.LookupTableAddresses}
		}
		data, _ := json.Marshal(payload)
		return response(string(data)), nil
	})})
	o.Snapshot.RouteLane, o.Snapshot.StrategyKey = route.Lane, route.Lane
	o.Snapshot.SquadsIdleRaw, o.Snapshot.DebtIdleRaw = 11_000, 0
	o.Snapshot.PositionDebtValueRaw = 1000
	return o, m, rpc, client, accounts
}

func TestUSDCPayoffReservesCashOnceAndNoSelfSwap(t *testing.T) {
	o, m, rpc, client, accounts := usdcReturnFixture(t)
	route, _ := runtimeRoute(o.Snapshot.RouteLane)
	d := Decision{Action: DeleverRouteStep, StrategyKey: route.Lane, AmountRaw: 1000, Reason: "withdrawal_repay_debt"}
	r, err := m.kaminoPacketForRoute(d.Action, kaminoLegRepay, 1001, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.FullPayoff = true
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	effects, err := boundedKaminoRepaymentEffects(accounts, source, destination, 1000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := observePhase3PayoffAdmission(context.Background(), rpc, client, m, o, d, KaminoExecutionEvidence{r, effects})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Snapshot != o.Snapshot || len(plan.AdditionalQuotedExits) != 0 {
		t.Fatal("USDC representation changed or self-swap included")
	}
	for _, step := range plan.Exit {
		if step.Action == SwapDebtToUSDCStep {
			t.Fatal("USDC self-swap")
		}
		if step.Action == StageSquadsToVoltr && step.Amount != 10_000+plan.QuotedExit.EstimatedUpperOutputRaw {
			t.Fatal("cash counted twice", step.Amount)
		}
	}
	o.Snapshot.DebtIdleRaw = 11_000
	if _, err = observePhase3PayoffAdmission(context.Background(), rpc, client, m, o, d, KaminoExecutionEvidence{r, effects}); err == nil {
		t.Fatal("duplicate debt cash representation accepted")
	}
}

func TestUSDCEntryConsumesWorkingCashAndValidatesSharedSourceOnce(t *testing.T) {
	o, m, rpc, client, accounts := usdcReturnFixture(t)
	route, _ := runtimeRoute(o.Snapshot.RouteLane)
	clear(accountAt(accounts, route.Kamino.Obligation).Data[96:1408])
	d := Decision{Action: SwapStableToCollateralStep, StrategyKey: route.Lane, AmountRaw: 11_000}
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, 11_000, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.EntryReturnReserved = true
	if _, err = validateEntrySwap(context.Background(), rpc, e.Request, e.ExpectedEffects, 42); err != nil {
		t.Fatal(err)
	}
	partial := e.Request
	partial.AmountRaw = 10_000
	if _, err = validateEntrySwap(context.Background(), rpc, partial, e.ExpectedEffects, 42); err == nil {
		t.Fatal("partial working-cash swap accepted")
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 12_000)
	if _, err = validateEntrySwap(context.Background(), rpc, e.Request, e.ExpectedEffects, 42); err == nil {
		t.Fatal("changed shared source was accepted")
	}
}

func TestUSDCPartialCapacityKeepsRemainderInVoltr(t *testing.T) {
	for _, lane := range selectorLanes {
		s := base()
		s.RouteLane, s.StrategyKey = lane, lane
		s.VoltrIdleRaw, s.CapacityRaw, s.MaxTargetLTVEntryRaw, s.PolicyLimitRaw = 100_000, 20_000, 20_000, 100_000
		d := Decide(s)
		if d.Action != VoltrAllocateToSquads || d.AmountRaw != 20_000 {
			t.Fatalf("%s allocation: %+v", lane, d)
		}
		o, _, evidence := bridgeAdmissionFixture(t, d.Action, d.AmountRaw, s.VoltrIdleRaw, 0, 0)
		o.Snapshot.RouteLane, o.Snapshot.StrategyKey = lane, lane
		d.StrategyKey = lane
		if _, err := phase3BridgeTemplates(o.Snapshot, d, evidence); phase3BudgetFamilyForLane(lane) == "" {
			assertBudgetHold(t, err, "bridge_admission_snapshot_unavailable")
		} else if err != nil {
			t.Fatal("bounded allocation return", err)
		}
		s.VoltrIdleRaw -= d.AmountRaw
		s.SquadsIdleRaw = d.AmountRaw
		d = Decide(s)
		if d.Action != SwapStableToCollateralStep || d.AmountRaw != s.SquadsIdleRaw {
			t.Fatalf("%s working cash: %+v", lane, d)
		}
		for _, capacity := range []int64{0, 19_999} {
			changed := s
			changed.CapacityRaw = capacity
			d = Decide(changed)
			if d.Action != StageSquadsToVoltr || d.AmountRaw != s.SquadsIdleRaw {
				t.Fatalf("%s capacity shrink: %+v", lane, d)
			}
			o, _, evidence = bridgeAdmissionFixture(t, d.Action, d.AmountRaw, changed.VoltrIdleRaw, 0, changed.SquadsIdleRaw)
			o.Snapshot.RouteLane, o.Snapshot.StrategyKey = lane, lane
			if _, err := phase3BridgeTemplates(o.Snapshot, d, evidence); phase3BudgetFamilyForLane(lane) == "" {
				assertBudgetHold(t, err, "bridge_admission_snapshot_unavailable")
			} else if err != nil {
				t.Fatal("capacity return", err)
			}
		}
		s.SquadsIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 20_000, 20_000
		if d = Decide(s); d.Action != OpenRouteStep || d.Reason != "prime_collateral_ready" {
			t.Fatalf("%s finish deposit before allocating more: %+v", lane, d)
		}
		s.CollateralIdleRaw, s.PrimeIdleRaw, s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.HasPosition = 0, 0, 20_000, 20_000, true
		if d = Decide(s); d.Action != OpenRouteStep || d.Reason != "prime_collateral_requires_borrow" {
			t.Fatalf("%s finish borrow: %+v", lane, d)
		}
		s.PositionDebtRaw, s.PositionDebtValueRaw, s.SquadsIdleRaw = 10_000, 10_000, 10_000
		if d = Decide(s); d.Action != SwapDebtToCollateralStep || d.AmountRaw != 10_000 {
			t.Fatalf("%s borrowed cash: %+v", lane, d)
		}
		if err := d.Validate(); err != nil {
			t.Fatalf("%s worker rejects its borrowed-cash decision: %v", lane, err)
		}
		for _, action := range []Action{SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
			invalid := d
			invalid.Action = action
			if invalid.Validate() == nil {
				t.Fatal("USDC self-swap admitted", lane, action)
			}
		}
		s.SquadsIdleRaw, s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 10_000, 10_000
		if d = Decide(s); d.Action != OpenRouteStep || d.Reason != "single_loop_redeposit" {
			t.Fatalf("%s finish redeposit: %+v", lane, d)
		}
		s.CollateralIdleRaw, s.PrimeIdleRaw = 0, 0
		if d = Decide(s); d.Action != Hold || d.Reason != "single_loop_position_ready" || s.VoltrIdleRaw != 80_000 {
			t.Fatalf("%s idle remains separate: %+v", lane, d)
		}
	}
}

func TestUSDCHardLTVRequiresExecutablePayoff(t *testing.T) {
	s := base()
	s.RouteLane, s.StrategyKey = SelectedRouteID, SelectedRouteID
	s.HasPosition, s.PositionCollateralRaw, s.PositionDebtRaw, s.PayoffDebtRaw, s.LTVBPS = true, 200, 100, 101, 6000
	for _, cash := range []int64{1, 99, 100, 101, 102} {
		s.SquadsIdleRaw = cash
		d := Decide(s)
		if cash < 101 {
			if d.Action != HoldManualRecovery || d.Reason != "hard_ltv_partial_repayment_requires_admission" {
				t.Fatalf("unadmitted partial repayment: %+v", d)
			}
		} else if d.Action != DeleverRouteStep || d.AmountRaw != 100 {
			t.Fatalf("funded full payoff: %+v", d)
		}
	}
	for _, lane := range selectorLanes {
		s.RouteLane, s.StrategyKey = lane, lane
		s.SquadsIdleRaw = 0
		s.PositionDebtValueRaw = 100
		s.CollateralIdleRaw, s.CollateralIdleValueRaw = 200, 200
		d := Decide(s)
		if d.Action != SwapCollateralToDebtStep || d.Validate() != nil {
			t.Fatalf("worker rejects admitted risk funding for %s: %+v (%v)", lane, d, d.Validate())
		}
	}
}

func TestUSDCCanaryAllocationFitsMeasuredCompleteBridgeReturn(t *testing.T) {
	s := base()
	s.RouteLane, s.StrategyKey = SelectedRouteID, SelectedRouteID
	s.VoltrIdleRaw, s.CapacityRaw, s.MaxTargetLTVEntryRaw, s.PolicyLimitRaw = 5_000_000, 5_000_000, 5_000_000, int64(strategyTwoBridgeLegCapRaw)
	d := Decide(s)
	if d.Action != VoltrAllocateToSquads || d.AmountRaw != 500_000 {
		t.Fatalf("canary size: %+v", d)
	}
	o, _, e := bridgeAdmissionFixture(t, d.Action, d.AmountRaw, s.VoltrIdleRaw, 0, 0)
	o.Snapshot.RouteLane, o.Snapshot.StrategyKey = s.RouteLane, s.StrategyKey
	plan, err := observePhase3BridgeAdmission(context.Background(), budgetBuildRPC(t, 5000, 42), o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CurrentCost.TotalMicros >= Phase3TransactionCapMicros || len(plan.Exit) != 5 {
		t.Fatal("canary has no measured return headroom")
	}
	for _, step := range plan.Exit {
		if step.Cost.TotalMicros >= Phase3TransactionCapMicros {
			t.Fatal("return leg exceeds canary budget")
		}
	}
}
