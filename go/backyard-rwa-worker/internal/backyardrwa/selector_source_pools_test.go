package backyardrwa

import (
	"math"
	"testing"
)

// Amounts throughout are deliberately asymmetric — no parity, no round
// numbers — so any accidental doubling, halving or peg assumption shows up
// as an exact-value mismatch rather than hiding inside a round figure.

// selectorSourceAutoLane keeps this file self-contained: it never depends on
// another test file for the candidate lane identity.
const selectorSourceAutoLane = "AUTO/AUTO/PYUSD"

func TestSelectorSourcePoolsTrackDebtFundingSeparately(t *testing.T) {
	route, err := runtimeRoute(selectorSourceAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	pools := newSelectorSourcePools(123_457, route)
	// Bound collateral→debt funding: raw PYUSD minimum output lands in the
	// funding pool, never in cash.
	if err := pools.creditSwap(SwapCollateralToDebtStep, route.Kamino.CollateralMint, route.Kamino.DebtMint, 987_654_321, 987_654_321); err != nil {
		t.Fatal(err)
	}
	if pools.cash != 123_457 || pools.debtFunding != 987_654_321 {
		t.Fatalf("funding leaked into cash: %+v", pools)
	}
	// Repay consumes debt-mint funding only.
	if err := pools.repay(500_000_001); err != nil {
		t.Fatal(err)
	}
	if pools.cash != 123_457 || pools.debtFunding != 487_654_320 {
		t.Fatalf("repay moved the wrong pool: %+v", pools)
	}
	// Only a bound debt→USDC quote converts the guaranteed remainder: it
	// consumes exactly the funding it sells and credits exactly its enforced
	// minimum output, never a price-arithmetic conversion of raw units.
	if err := pools.creditSwap(SwapDebtToUSDCStep, route.Kamino.DebtMint, bridgeUSDC, 44_321, 44_099); err != nil {
		t.Fatal(err)
	}
	if pools.cash != 167_556 || pools.debtFunding != 487_609_999 {
		t.Fatalf("residue conversion mismatched: %+v", pools)
	}
	if err := pools.creditCash(12_345); err != nil {
		t.Fatal(err)
	}
	if pools.cash != 179_901 {
		t.Fatalf("cash: %d", pools.cash)
	}
}

func TestSelectorSourcePoolsRejectDuplicateAndOversizedResidueConversion(t *testing.T) {
	route, err := runtimeRoute(selectorSourceAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	pools := newSelectorSourcePools(500, route)
	if err := pools.creditSwap(SwapCollateralToDebtStep, route.Kamino.CollateralMint, route.Kamino.DebtMint, 1_000, 1_000); err != nil {
		t.Fatal(err)
	}
	if err := pools.creditSwap(SwapDebtToUSDCStep, route.Kamino.DebtMint, bridgeUSDC, 600, 590); err != nil {
		t.Fatal(err)
	}
	if pools.cash != 1_090 || pools.debtFunding != 400 {
		t.Fatalf("first conversion: %+v", pools)
	}
	// Re-converting residue that was already sold fails closed, and so does
	// any conversion above the guaranteed recipe-produced remainder.
	assertBudgetHold(t, pools.creditSwap(SwapDebtToUSDCStep, route.Kamino.DebtMint, bridgeUSDC, 600, 590), "selector_source_residue_unfunded")
	assertBudgetHold(t, pools.creditSwap(SwapDebtToUSDCStep, route.Kamino.DebtMint, bridgeUSDC, 401, 1), "selector_source_residue_unfunded")
	// State is unchanged on every rejected conversion.
	if pools.cash != 1_090 || pools.debtFunding != 400 {
		t.Fatalf("rejected conversion mutated pools: %+v", pools)
	}
}

func TestSelectorSourcePoolsBindSellToRouteCollateral(t *testing.T) {
	route, err := runtimeRoute(selectorSourceAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	foreign := primePRIMEPYUSD.Kamino.CollateralMint
	pools := newSelectorSourcePools(1_000, route)
	// Collateral legs sell exactly the route collateral — no other token
	// becomes cash or funding by passing a USDC output check.
	assertBudgetHold(t, pools.creditSwap(SwapCollateralToStableStep, foreign, bridgeUSDC, 1_000, 900), "selector_source_not_an_exit")
	assertBudgetHold(t, pools.creditSwap(SwapCollateralToDebtStep, foreign, route.Kamino.DebtMint, 1_000, 900), "selector_source_not_an_exit")
	// Action and pair are bound together.
	assertBudgetHold(t, pools.creditSwap(SwapCollateralToStableStep, route.Kamino.CollateralMint, route.Kamino.DebtMint, 1_000, 900), "selector_source_not_an_exit")
	assertBudgetHold(t, pools.creditSwap(SwapDebtToUSDCStep, foreign, bridgeUSDC, 100, 90), "selector_source_not_an_exit")
	// USDC is never an exit input; identical sides are never a swap; zero
	// amounts are never a bound leg.
	assertBudgetHold(t, pools.creditSwap(SwapCollateralToStableStep, bridgeUSDC, bridgeUSDC, 1_000, 900), "selector_source_not_an_exit")
	assertBudgetHold(t, pools.creditSwap(SwapCollateralToStableStep, route.Kamino.CollateralMint, bridgeUSDC, 0, 0), "selector_source_not_an_exit")
	if pools.cash != 1_000 || pools.debtFunding != 0 {
		t.Fatalf("rejected legs mutated pools: %+v", pools)
	}
	// The one bound collateral leg still works.
	if err := pools.creditSwap(SwapCollateralToStableStep, route.Kamino.CollateralMint, bridgeUSDC, 1_000, 900); err != nil {
		t.Fatal(err)
	}
	if pools.cash != 1_900 || pools.debtFunding != 0 {
		t.Fatalf("bound collateral leg: %+v", pools)
	}
}

func TestSelectorSourcePoolsRepayFailsClosedWithoutFunding(t *testing.T) {
	route, err := runtimeRoute(selectorSourceAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	pools := newSelectorSourcePools(0, route)
	if err := pools.creditSwap(SwapCollateralToDebtStep, route.Kamino.CollateralMint, route.Kamino.DebtMint, 100, 100); err != nil {
		t.Fatal(err)
	}
	// No cash ever covers a non-USDC debt: repayment needs produced funding.
	assertBudgetHold(t, pools.repay(101), "selector_source_minimum_cannot_pay_debt")
	if pools.debtFunding != 100 || pools.cash != 0 {
		t.Fatalf("rejected repay mutated pools: %+v", pools)
	}
	if err := pools.repay(100); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, pools.repay(1), "selector_source_minimum_cannot_pay_debt")
	if pools.debtFunding != 0 {
		t.Fatalf("over-repay mutated pools: %+v", pools)
	}
}

func TestSelectorSourcePoolsKeepMapleUSDCRepayFromCash(t *testing.T) {
	usdc, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	pools := newSelectorSourcePools(900_000, usdc)
	if err := pools.creditSwap(SwapCollateralToStableStep, usdc.Kamino.CollateralMint, bridgeUSDC, 1_000_000, 400_000); err != nil {
		t.Fatal(err)
	}
	if pools.cash != 1_300_000 {
		t.Fatalf("cash: %d", pools.cash)
	}
	// USDC-debt lanes repay from cash exactly as the persisted exits assumed.
	if err := pools.repay(400_001); err != nil {
		t.Fatal(err)
	}
	if pools.cash != 899_999 || pools.debtFunding != 0 {
		t.Fatalf("maple repay: %+v", pools)
	}
	// Collateral→debt is the same pair on a USDC lane and credits cash.
	if err := pools.creditSwap(SwapCollateralToDebtStep, usdc.Kamino.CollateralMint, bridgeUSDC, 100_000, 100); err != nil {
		t.Fatal(err)
	}
	if pools.cash != 900_099 {
		t.Fatalf("maple collateral→debt credit: %d", pools.cash)
	}
	assertBudgetHold(t, pools.repay(900_100), "selector_source_minimum_cannot_pay_debt")
	// A USDC-debt lane can never run the residue conversion: USDC is not an
	// exit input, so the debt→USDC pair is degenerate there.
	assertBudgetHold(t, pools.creditSwap(SwapDebtToUSDCStep, bridgeUSDC, bridgeUSDC, 100, 90), "selector_source_not_an_exit")
}

func TestSelectorSourcePoolsOverflowFailsClosed(t *testing.T) {
	route, err := runtimeRoute(selectorSourceAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	pools := selectorSourcePools{cash: math.MaxUint64, debtFunding: 1_000, collateralMint: route.Kamino.CollateralMint, debtMint: route.Kamino.DebtMint}
	assertBudgetHold(t, pools.creditSwap(SwapCollateralToStableStep, route.Kamino.CollateralMint, bridgeUSDC, 100, 1), "selector_source_cash_overflow")
	// The residue conversion checks the cash bound before mutating funding.
	assertBudgetHold(t, pools.creditSwap(SwapDebtToUSDCStep, route.Kamino.DebtMint, bridgeUSDC, 1_000, math.MaxUint64), "selector_source_cash_overflow")
	if pools.debtFunding != 1_000 || pools.cash != math.MaxUint64 {
		t.Fatalf("rejected conversion mutated pools: %+v", pools)
	}
	pools.debtFunding = math.MaxUint64
	assertBudgetHold(t, pools.creditSwap(SwapCollateralToDebtStep, route.Kamino.CollateralMint, route.Kamino.DebtMint, 100, 1), "selector_source_funding_overflow")
	assertBudgetHold(t, pools.creditCash(1), "selector_source_cash_overflow")
	if pools.cash != math.MaxUint64 || pools.debtFunding != math.MaxUint64 {
		t.Fatalf("overflow mutated pools: %+v", pools)
	}
}

// Raw sold and output amounts live in different mints: above-peg debt, a
// collateral worth more than one USDC per raw unit, and differing decimals
// all legitimately credit more (or fewer) raw units than were sold. The
// helper accepts them all — only the bound pairs, the input pool and the
// output overflow are safety checks.
func TestSelectorSourcePoolsNeverCompareRawAmountsAcrossMints(t *testing.T) {
	autoRoute, err := runtimeRoute(selectorSourceAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	// PYUSD above peg: converting 1_000 raw debt units can legitimately
	// guarantee 1_001 USDC of minimum output.
	abovePeg := newSelectorSourcePools(10_000, autoRoute)
	if err := abovePeg.creditSwap(SwapCollateralToDebtStep, autoRoute.Kamino.CollateralMint, autoRoute.Kamino.DebtMint, 1_000, 1_000); err != nil {
		t.Fatal(err)
	}
	if err := abovePeg.creditSwap(SwapDebtToUSDCStep, autoRoute.Kamino.DebtMint, bridgeUSDC, 1_000, 1_001); err != nil {
		t.Fatal(err)
	}
	if abovePeg.cash != 11_001 || abovePeg.debtFunding != 0 {
		t.Fatalf("above-peg conversion: %+v", abovePeg)
	}
	// Maple-style collateral worth more than one USDC per raw unit.
	usdc, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	syrupLike := newSelectorSourcePools(0, usdc)
	if err := syrupLike.creditSwap(SwapCollateralToStableStep, usdc.Kamino.CollateralMint, bridgeUSDC, 500, 501); err != nil {
		t.Fatal(err)
	}
	if syrupLike.cash != 501 {
		t.Fatalf("rich collateral credit: %d", syrupLike.cash)
	}
	// A synthetic route whose collateral carries 9 decimals and whose debt
	// carries 2: raw comparisons are meaningless in both directions.
	synthetic := RuntimeRoute{Lane: "Synthetic/NineTwoDecimals/USDC", Kamino: KaminoObservationConfig{
		CollateralMint: "SynthCollateral11111111111111111111111111",
		DebtMint:       "SynthDebt111111111111111111111111111111",
	}}
	pools := newSelectorSourcePools(7, synthetic)
	// Nine-decimal collateral sold for a single raw USDC unit.
	if err := pools.creditSwap(SwapCollateralToStableStep, synthetic.Kamino.CollateralMint, bridgeUSDC, 1_000_000_000, 1); err != nil {
		t.Fatal(err)
	}
	// Two-decimal debt funding whose bound minimum dwarfs the sold amount.
	if err := pools.creditSwap(SwapCollateralToDebtStep, synthetic.Kamino.CollateralMint, synthetic.Kamino.DebtMint, 1, 100_000_000); err != nil {
		t.Fatal(err)
	}
	if pools.cash != 8 || pools.debtFunding != 100_000_000 {
		t.Fatalf("differing-decimals pools: %+v", pools)
	}
	if err := pools.repay(100_000_000); err != nil {
		t.Fatal(err)
	}
}

func TestSelectorRepayMintIdentityBound(t *testing.T) {
	route, err := runtimeRoute(selectorSourceAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	bound := ExpectedEffects{Accounts: []ExpectedAccountEffect{
		{Address: route.DebtCustody, Mint: route.Kamino.DebtMint, BeforeRaw: 500_000, AfterRaw: 0},
		{Address: route.DebtLiquiditySupply, Mint: route.Kamino.DebtMint, BeforeRaw: 1_000, AfterRaw: 501_000},
	}}
	if !selectorRepayMintBound(bound, route.Kamino.DebtMint) {
		t.Fatal("bound route repay rejected")
	}
	// A repay whose effects move any foreign mint never consumes funding.
	foreign := ExpectedEffects{Accounts: []ExpectedAccountEffect{
		{Address: route.DebtCustody, Mint: route.Kamino.DebtMint, BeforeRaw: 500, AfterRaw: 0},
		{Address: route.CollateralCustody, Mint: route.Kamino.CollateralMint, BeforeRaw: 0, AfterRaw: 700},
	}}
	if selectorRepayMintBound(foreign, route.Kamino.DebtMint) {
		t.Fatal("foreign repay mint accepted")
	}
	if selectorRepayMintBound(ExpectedEffects{Accounts: []ExpectedAccountEffect{{Address: route.DebtCustody}}}, route.Kamino.DebtMint) {
		t.Fatal("repay without token effects accepted")
	}
}
