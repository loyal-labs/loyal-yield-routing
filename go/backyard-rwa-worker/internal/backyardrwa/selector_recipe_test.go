package backyardrwa

import (
	"context"
	"encoding/binary"
	"math/big"
	"testing"
)

func selectorRecipeInput(t *testing.T, request any, effects ExpectedEffects) *phase3BuildInput {
	t.Helper()
	raw, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	input, err := encodePhase3BuildInput(request, raw)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

// This is controlled RPC/compiler evidence, not a completed on-chain loop.
// The transport refuses simulation, signing and sends, and the actual price
// decoders and expense classifier value every supplied message.
func TestSelectorRecipePricesRepeatedReportsWithoutChargingPrincipalOrRent(t *testing.T) {
	_, _, allocate := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 1_000_000, 1_000_000, 0, 0)
	_, _, report := bridgeAdmissionFixture(t, ReportNAV, 0, 0, 0, 1_000_000)
	init, _ := initializationReconcileFixture(t)
	inputs := []*phase3BuildInput{
		selectorRecipeInput(t, *init.Initialization, init),
		selectorRecipeInput(t, allocate.Request, allocate.ExpectedEffects),
		selectorRecipeInput(t, report.Request, report.ExpectedEffects),
		selectorRecipeInput(t, report.Request, report.ExpectedEffects),
	}
	got, err := priceSelectorRecipe(context.Background(), budgetBuildRPC(t, 5000, 42), init.Initialization.RouteLane, inputs, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Costs) != 4 || got.NetworkLamports != 20_000 || got.SetupLamports != init.Initialization.RentLamports || got.CostRaw <= 0 || got.CostRaw >= 10_000 || !sha256Pattern.MatchString(got.EvidenceID) {
		t.Fatalf("principal/rent charged or repeated report omitted: %+v", got)
	}
	var network, gross int64
	for _, cost := range got.Costs {
		network += cost.NetworkFeeMicros
		gross += cost.TotalMicros
	}
	if got.CostRaw != network || gross <= 1_000_000 || got.ValidThroughSlot != 74 {
		t.Fatal("gross movement mixed with execution expense", got.CostRaw, network, gross)
	}
	changed, err := priceSelectorRecipe(context.Background(), budgetBuildRPC(t, 6000, 42), init.Initialization.RouteLane, inputs[1:], 42)
	if err != nil || changed.CostRaw <= 0 || changed.EvidenceID == got.EvidenceID {
		t.Fatal("fee evidence not bound", err)
	}
	_, err = priceSelectorRecipe(context.Background(), budgetBuildRPC(t, init.Initialization.MaximumFeeLamports+1, 42), init.Initialization.RouteLane, inputs, 42)
	assertBudgetHold(t, err, "initializer_fee_changed")
	staleRPC := budgetBuildRPC(t, 5000, 75)
	_, _ = staleRPC.ConfirmedSlot(context.Background())
	_, err = priceSelectorRecipe(context.Background(), staleRPC, init.Initialization.RouteLane, inputs, 42)
	if err == nil {
		t.Fatal("stale prices admitted")
	}
	_, err = priceSelectorRecipe(context.Background(), budgetBuildRPC(t, 5000, 42), PhaseOneLaneID, inputs, 42)
	if init.Initialization.RouteLane != PhaseOneLaneID {
		assertBudgetHold(t, err, "selector_recipe_lane_mismatch")
	}
	_, err = priceSelectorRecipe(context.Background(), budgetBuildRPC(t, 5000, 42), PhaseOneLaneID, []*phase3BuildInput{nil}, 42)
	assertBudgetHold(t, err, "missing_persisted_build_input")
}

func TestSelectorRecipeValuesActualBasicSwapMinimum(t *testing.T) {
	lane := SelectedRouteID
	route, _ := runtimeRoute(lane)
	request, _ := basicJupiterRequestFromExport(t, lane, "USDC->syrupUSDC")
	// This recorded leg fits the legacy message; no synthetic lookup account
	// or execution-state projection is needed for a fee-only forecast.
	request.Instruction.LookupTableAddresses = nil
	minimum := request.MinimumOutputRaw
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap", Accounts: []ExpectedAccountEffect{
		{Address: bridgeSquadsATA, Owner: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault, BeforeRaw: request.AmountRaw, AfterRaw: 0},
		{Address: route.CollateralCustody, Owner: classicTokenProgram, Mint: route.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: minimum, MinimumAfterRaw: &minimum},
	}}
	sf := new(big.Int).Lsh(big.NewInt(1), 60)
	reserve := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, sf, 1, 1)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	mint := ConfirmedAccount{Address: route.Kamino.CollateralMint, Owner: classicTokenProgram, Data: make([]byte, 82)}
	mint.Data[44], mint.Data[45] = 6, 1
	input := selectorRecipeInput(t, request, effects)
	got, err := priceSelectorRecipe(context.Background(), budgetBuildRPCWithAccounts(t, 5000, 42, []ConfirmedAccount{reserve, mint}), lane, []*phase3BuildInput{input}, 42)
	if err != nil {
		t.Fatal(err)
	}
	price := got.Costs[0].ExecutionCost.CreditPrice
	lower, err := price.valueLower(minimum, route.Kamino.CollateralMint, classicTokenProgram, 42)
	if err != nil {
		t.Fatal(err)
	}
	want := max(0, got.Costs[0].PrincipalMicros-lower) + got.Costs[0].NetworkFeeMicros
	if got.CostRaw != want || got.Costs[0].ExecutionCost.SwapLossMicros <= 0 {
		t.Fatal("swap omitted floor output/margin", got.CostRaw, want)
	}
	effects.Accounts[1].AfterRaw++
	_, err = priceSelectorRecipe(context.Background(), budgetBuildRPCWithAccounts(t, 5000, 42, []ConfirmedAccount{reserve, mint}), lane, []*phase3BuildInput{selectorRecipeInput(t, request, effects)}, 42)
	if err == nil {
		t.Fatal("unenforced optimistic credit admitted")
	}
}
