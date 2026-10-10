package backyard

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// marketTestPrice builds an observation the way decodeBudgetTokenPrice does:
// ±budgetPriceMarginBPS sides around the token and USDC market prices, or the
// market prices themselves when unmargined (a USDC debit).
func marketTestPrice(mint, program string, decimals uint8, token, usdc uint64, margined bool) BudgetPrice {
	var tokenSF, usdcSF [16]byte
	binary.LittleEndian.PutUint64(tokenSF[:8], token)
	binary.LittleEndian.PutUint64(usdcSF[:8], usdc)
	p := BudgetPrice{Credit: &BudgetCreditBounds{tokenSF, usdcSF}, Mint: mint, TokenProgram: program, Decimals: decimals, TokenUpperSF: tokenSF, USDCLowerSF: usdcSF, ObservedSlot: 42, ValidThroughSlot: 74, EvidenceSHA256: strings.Repeat("a", 64)}
	if margined {
		p.TokenUpperSF, _ = UpperPriceMargin(tokenSF, budgetPriceMarginBPS)
		p.USDCLowerSF, _ = lowerPriceMargin(usdcSF, budgetPriceMarginBPS)
		p.Credit.TokenLowerSF, _ = lowerPriceMargin(tokenSF, budgetPriceMarginBPS)
		p.Credit.USDCUpperSF, _ = UpperPriceMargin(usdcSF, budgetPriceMarginBPS)
	}
	return p
}

const (
	swapTestInputRaw   uint64 = 100_000_000_000 // 100,000 USDC
	swapTestMinimumRaw uint64 = 94_714_050_000  // Jupiter quote 95,190 AUTO less 50 bps
	swapTestOutputRaw  uint64 = 95_190_000_000  // finalized credit
	// AUTO at $1.05: 95,190 AUTO is $99,949.50, so the swap lost $50.50; the
	// 5,000-lamport fee at $150/SOL adds $0.00075.
	swapTestRealizedMicros int64 = 50_500_750
)

// A 100k USDC -> AUTO swap, priced and finalized as the send fence and the
// reconciler persist it.
func swapRealizedFixture(t *testing.T) (phase3OperationAuthorization, []byte) {
	t.Helper()
	autoMint := autoAUTOPYUSD.Kamino.CollateralMint
	minimum := swapTestMinimumRaw
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap", Accounts: []ExpectedAccountEffect{
		{Address: bridgeSquadsATA, Owner: classicTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault, BeforeRaw: swapTestInputRaw},
		{Address: autoAUTOPYUSD.CollateralCustody, Owner: classicTokenProgram, Mint: autoMint, Authority: bridgeVault, AfterRaw: minimum, MinimumAfterRaw: &minimum},
	}}
	encoded, err := json.Marshal(effects)
	if err != nil {
		t.Fatal(err)
	}
	usdc := marketTestPrice(bridgeUSDC, classicTokenProgram, 6, 1_000_000_000_000, 1_000_000_000_000, false)
	auto := marketTestPrice(autoMint, classicTokenProgram, 6, 1_050_000_000_000, 1_000_000_000_000, true)
	send := ValuedTransactionCost{Fee: MessageFeeObservation{Lamports: 5000}, TokenPrice: &usdc, NativePrice: marketTestPrice(nativeSOLBudgetAsset, "11111111111111111111111111111111", 9, 150_000_000_000_000, 1_000_000_000_000, true), ObservationSlot: 42, ExecutionCost: &PilotExecutionCost{CreditPrice: &auto}}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, PilotAuthorityID: pilotBudgetAuthorityID, IntentSHA256: sha256Bytes([]byte("swap")), BuildInput: &phase3BuildInput{Kind: "jupiter", Effects: encoded}, SendKnownCost: &send}
	reconciled, err := json.Marshal(map[string]any{"schema": "loyal-backyard-rwa-reconciled-effects/v1", "accounts": []string{
		fmt.Sprintf("%s:%s:%s:%s:%d:%d", bridgeSquadsATA, classicTokenProgram, bridgeUSDC, bridgeVault, swapTestInputRaw, 0),
		fmt.Sprintf("%s:%s:%s:%s:%d:%d", autoAUTOPYUSD.CollateralCustody, classicTokenProgram, autoMint, bridgeVault, 0, swapTestOutputRaw),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return auth, reconciled
}

// The admitted bound of the same swap: the debit at the upper price less the
// guaranteed minimum at the lower credit price, plus the fee at the upper SOL
// price (classifyPilotExecutionCost's formula), about $2,519.55.
func swapBoundMicros(t *testing.T, auth phase3OperationAuthorization) int64 {
	t.Helper()
	cost := auth.SendKnownCost
	principal, err := cost.TokenPrice.valueUpper(swapTestInputRaw, bridgeUSDC, classicTokenProgram, 42)
	if err != nil {
		t.Fatal(err)
	}
	minimum, err := cost.ExecutionCost.CreditPrice.valueLower(swapTestMinimumRaw, autoAUTOPYUSD.Kamino.CollateralMint, classicTokenProgram, 42)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := cost.NativePrice.valueUpper(5000, nativeSOLBudgetAsset, "11111111111111111111111111111111", 42)
	if err != nil {
		t.Fatal(err)
	}
	return principal - minimum + fee
}

func TestRealizedSwapCostIsMidPriceLossPlusFee(t *testing.T) {
	auth, reconciled := swapRealizedFixture(t)
	realized, err := realizedPilotExecutionCost(auth, reconciled)
	if err != nil || realized != swapTestRealizedMicros {
		t.Fatalf("realized swap cost %d, want %d: %v", realized, swapTestRealizedMicros, err)
	}
	if bound := swapBoundMicros(t, auth); bound < 2_500_000_000 || bound > 2_550_000_000 {
		t.Fatalf("fixture bound %d is not the ~$2.5k worst case", bound)
	}
	// Unproven evidence is refused, never guessed.
	missing := auth
	missing.SendKnownCost = nil
	_, err = realizedPilotExecutionCost(missing, reconciled)
	assertBudgetHold(t, err, "realized_execution_cost_evidence_missing")
	_, err = realizedPilotExecutionCost(auth, []byte(`{"accounts":[]}`))
	assertBudgetHold(t, err, "realized_swap_cost_evidence_missing")
}

func TestMidValuationIsTheUnmarginedMarketPrice(t *testing.T) {
	for _, market := range []uint64{3, 99, 1_234_567, 1_152_921_504_606_846_977} {
		margined := marketTestPrice("mint", classicTokenProgram, 6, market, 1_000_000_000_000_000_003, true)
		plain := marketTestPrice("mint", classicTokenProgram, 6, market, 1_000_000_000_000_000_003, false)
		for _, liability := range []bool{false, true} {
			mid, err := margined.valueMid(987_654_321, "mint", classicTokenProgram, 42, liability)
			want, wantErr := valueBetweenTokenRaw(987_654_321, 6, 6, plain.TokenUpperSF, plain.USDCLowerSF, liability)
			if err != nil || wantErr != nil || mid != int64(want) {
				t.Fatalf("market %d: mid %d want %d: %v %v", market, mid, want, err, wantErr)
			}
		}
	}
}

func TestSettlementBooksRealizedCostAndGrossUpperBound(t *testing.T) {
	auth, reconciled := swapRealizedFixture(t)
	realized, err := realizedPilotExecutionCost(auth, reconciled)
	if err != nil {
		t.Fatal(err)
	}
	b := pilotTestBudget(t)
	b.Families["Maple"] = FamilyBudget{SpentMicros: 319_360_000, ExecutionCostSpentMicros: 319_360_000}
	r := BudgetReservation{OperationID: "swap", Family: "Maple", IntentSHA256: auth.IntentSHA256, UpperMicros: 100_000_000_766, ExecutionCostUpperMicros: swapBoundMicros(t, auth), ExitAfterMicros: 100_000_000_000}
	if err = b.Admit(r); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(b)
	assertBudgetHold(t, b.Settle(r.OperationID, r.IntentSHA256, r.UpperMicros, r.ExecutionCostUpperMicros+1), "realized_execution_cost_exceeds_bound")
	assertBudgetHold(t, b.Settle(r.OperationID, r.IntentSHA256, r.UpperMicros-1, realized), "pilot_settlement_requires_full_admitted_bound")
	if after, _ := json.Marshal(b); string(after) != string(before) {
		t.Fatal("refused settlement changed the budget")
	}
	if err = b.Settle(r.OperationID, r.IntentSHA256, r.UpperMicros, realized); err != nil {
		t.Fatal(err)
	}
	got := b.Families["Maple"]
	if len(b.Reservations) != 0 || got.SpentMicros != 319_360_000+r.UpperMicros || got.ExecutionCostSpentMicros != 319_360_000+swapTestRealizedMicros {
		t.Fatalf("settlement booked %+v", got)
	}
}

func TestPilotCapAdmitsWorstCaseBoundWithinThreeThousandDollars(t *testing.T) {
	auth, _ := swapRealizedFixture(t)
	for _, tc := range []struct {
		spent int64
		hold  string
	}{{319_360_000, ""}, {500_000_000, "pilot_entry_execution_cost_cap_exhausted"}} {
		b := pilotTestBudget(t)
		b.Families["Maple"] = FamilyBudget{SpentMicros: tc.spent, ExecutionCostSpentMicros: tc.spent}
		r := BudgetReservation{OperationID: "swap", Family: "Maple", IntentSHA256: auth.IntentSHA256, UpperMicros: 100_000_000_766, ExecutionCostUpperMicros: swapBoundMicros(t, auth), ExitAfterMicros: 100_000_000_000}
		err := b.Admit(r)
		if tc.hold == "" {
			if err != nil {
				t.Fatalf("spent %d: %v", tc.spent, err)
			}
			continue
		}
		assertBudgetHold(t, err, tc.hold)
	}
}
