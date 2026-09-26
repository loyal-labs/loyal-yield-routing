package backyardrwa

import (
	"math"
	"testing"
	"time"
)

// This file is the bounded independent review of the manifest-funded AUTO
// selection under non-par debt/collateral prices. It drives the real
// SelectOpportunityWithLanes loop with observed BudgetPrice evidence from the
// established fixtures and asserts, at selection level:
//
//   - the liability is valued at the observed UPPER bound (a cheaper 0.9x debt
//     price strictly raises the candidate benefit; a dearer 1.1x price lowers
//     it and can reject the borrow outright where raw-unit parity would admit
//     it),
//   - the redeposited proceeds are valued at the observed LOWER bound of the
//     independently stated collateral price,
//   - a stale or window-violating price on either side never samples
//     persistence nor selects, and a stale tick cannot bridge a persistence
//     window,
//   - withdrawal handling precedes any candidate economics.
//
// Capacity holds (entry_closed / pair_capacity_unknown / deferred) and the
// missing-debt-price hold at selection level are already pinned by
// TestAutoCandidateFullAndUnavailableCapacityHoldWithoutPersistence and are
// only cited here. Quote-level identity and margin direction are pinned by
// TestNonUSDCDebtQuoteRequiresBoundPriceEvidence and
// TestBudgetPriceMarginsBracketParity; the economics unit discipline directly
// on pilotQuoteEconomics is TestOffPegDebtKeepsReserveAPRAndMovesUSDCEconomics.
// None of that is repeated.

const (
	nonParBorrowReceive = 5_000_000 // raw PYUSD principal
	nonParBorrowFee     = 10_000    // raw PYUSD origination fee
	nonParBorrowRaw     = nonParBorrowReceive + nonParBorrowFee
	nonParRedeposit     = nonParBorrowReceive // fee is consumed, principal redeploys
	nonParCost          = 10_000              // USDC movement cost
)

// nonParReviewFixture is the priced AUTO candidate at a chosen equity: a
// pilot-sized Maple→AUTO one-pass entry whose quote carries real observed
// PYUSD debt and AUTO collateral price evidence over the exact quote window.
func nonParReviewFixture(t *testing.T, debt, collateral *BudgetPrice, equity int64) SelectorInput {
	t.Helper()
	in := selectorFixture()
	advanceSelectorFixture(&in, time.Now().UTC().Sub(in.Now))
	in.Snapshot.PilotActive = true
	in.Snapshot.Slot = 42
	in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 100_000_000, 100_000_000
	market := in.Markets[0]
	market.Lane = testAutoLane
	market.EntryCapacity = Capacity{Known: true, Raw: equity}
	in.Markets = []LaneEconomics{market}
	in.Quotes = []MoveQuote{{
		MinimumIdleRaw: uint64(in.Snapshot.TotalVaultNAVRaw), BorrowReceiveRaw: nonParBorrowReceive, BorrowFeeRaw: nonParBorrowFee,
		DebtPrice: copyDebtPrice(debt), CollateralAssetPrice: copyDebtPrice(collateral), RedepositCollateralRaw: nonParRedeposit,
		SourceLane: in.Snapshot.RouteLane, DestinationLane: testAutoLane, ObservationID: in.Snapshot.ObservationID,
		EquityRaw: equity, CostRaw: nonParCost, ObservedAt: in.Now,
		EvidenceID: sha256Bytes([]byte("auto-nonpar-review")), SampleSlot: in.Snapshot.Slot, ValidThroughSlot: in.Snapshot.Slot + budgetMaxObservationLagSlots,
	}}
	return in
}

// Local authority closures over the reviewed manifest, independent of the
// funded-path test helpers.
func nonParLaneAllowed(m RouteManifest) func(string) bool {
	return func(lane string) bool { return selectorDestinationLaneAuthorized(m, lane) }
}

func nonParFundingAllowed(m RouteManifest) func(string) bool {
	return func(lane string) bool { return m.selectorEntryFundingLane(lane, false) }
}

func nonParCandidate(t *testing.T, result SelectorResult) *CandidateForecast {
	t.Helper()
	for i := range result.Candidates {
		if result.Candidates[i].Lane == testAutoLane {
			return &result.Candidates[i]
		}
	}
	t.Fatal("candidate missing from", result.Candidates)
	return nil
}

// nonParWantBenefit reassembles the candidate benefit from independently
// stated bounds: liability at the observed UPPER side, redeposited principal
// at the observed LOWER side, reserve APR projected from RAW debt units (the
// fixture curve is flat at 400 bps across the 0–8000 bps utilization region
// the borrow lands in, so the interpolation is exact). NativeAPY .15,
// SupplyAPY 0, UncertaintyBPS 0 and no exposed position fix every other term.
func nonParWantBenefit(t *testing.T, debtUpper, proceedsFloor int64, equity int64) float64 {
	t.Helper()
	invested := float64(equity - nonParCost)
	years := DefaultSelectorPolicy().Horizon.Hours() / (365.25 * 24)
	growth := math.Expm1((math.Log1p(0.15) + math.Log1p(0)) * years)
	interest := float64(debtUpper) * math.Expm1(0.04*years)
	return (invested+float64(proceedsFloor))*growth - interest - float64(nonParCost)
}

func nonParBounds(t *testing.T, debt *BudgetPrice, collateral *BudgetPrice, route RuntimeRoute, validThrough int64) (int64, int64) {
	t.Helper()
	debtUpper, err := debt.valueUpper(nonParBorrowRaw, route.Kamino.DebtMint, route.DebtTokenProgram, validThrough)
	if err != nil || debtUpper <= 0 {
		t.Fatalf("debt upper: %d %v", debtUpper, err)
	}
	proceeds, err := collateral.valueLower(nonParRedeposit, route.Kamino.CollateralMint, route.CollateralTokenProgram, validThrough)
	if err != nil || proceeds <= 0 {
		t.Fatalf("proceeds floor: %d %v", proceeds, err)
	}
	return debtUpper, proceeds
}

// TestAutoNonParSelectionUsesObservedPriceBounds varies the PYUSD debt price
// and the AUTO collateral price and asserts the selection's benefit is exactly
// the conservative reassembly of the observed interval ends — and that the
// priced UPPER bound rejects an equity that raw-unit parity would admit.
func TestAutoNonParSelectionUsesObservedPriceBounds(t *testing.T) {
	route, price09, _ := autoDebtPriceFixture(t, 900_000)
	_, price11, _ := autoDebtPriceFixture(t, 1_100_000)
	coll10 := autoCollateralPriceFixture(t, 1_000_000)
	manifest := autoInitializerFixtureManifest(t)
	allowed, funding := nonParLaneAllowed(manifest), nonParFundingAllowed(manifest)
	validThrough := int64(42 + budgetMaxObservationLagSlots)

	upper09, proceeds := nonParBounds(t, &price09, &coll10, route, validThrough)
	upper11, _ := nonParBounds(t, &price11, &coll10, route, validThrough)
	if upper11 <= upper09 {
		t.Fatalf("fixture insensitive to price scale: 0.9x upper %d, 1.1x upper %d", upper09, upper11)
	}
	// The discriminator band: an equity raw-unit parity admits (borrow raw fits)
	// that the 1.1x conservative upper bound exceeds, while the 0.9x observed
	// upper bound still fits inside it.
	if upper09 >= nonParBorrowRaw || nonParBorrowRaw >= upper11 {
		t.Fatalf("price margins left no parity discriminator band: %d/%d around raw %d", upper09, upper11, nonParBorrowRaw)
	}
	band := nonParBorrowRaw + (upper11-nonParBorrowRaw)/2
	if band <= nonParBorrowRaw || band >= upper11 || band < upper09 {
		t.Fatalf("band %d not between raw %d and 1.1x upper %d above the 0.9x upper %d", band, nonParBorrowRaw, upper11, upper09)
	}

	// At 1.1x the priced liability exceeds this equity: the candidate is held
	// with no economics and no persistence, though raw parity would admit it.
	in := nonParReviewFixture(t, &price11, &coll10, band)
	rejected := selectOpportunityWithLanes(in, SelectorState{}, allowed, funding)
	if rejected.Action != "KEEP" || rejected.Reason != "no_worthwhile_executable_move" {
		t.Fatal("rejected borrow changed the top-level hold", rejected)
	}
	if c := nonParCandidate(t, rejected); c.CostsKnown || c.BlockedReason != "bounded_borrow_unavailable" {
		t.Fatal("unaffordable priced liability was not held", c)
	}
	if len(rejected.State.Advantages) != 0 {
		t.Fatal("rejected borrow sampled persistence", rejected.State)
	}

	// The same equity at 0.9x: the observed upper bound fits, the candidate is
	// priced, profitable, and opens its persistence window.
	in = nonParReviewFixture(t, &price09, &coll10, band)
	admitted := selectOpportunityWithLanes(in, SelectorState{}, allowed, funding)
	if admitted.Action != "KEEP" || admitted.Reason != "advantage_not_yet_persistent" {
		t.Fatal("discounted borrow did not start persistence", admitted)
	}
	c := nonParCandidate(t, admitted)
	if !c.CostsKnown || c.BlockedReason != "" || c.BenefitRaw <= 0 {
		t.Fatal("discounted borrow lost its economics", c)
	}
	if window, ok := admitted.State.Advantages[testAutoLane]; !ok || !window.Since.Equal(in.Now) {
		t.Fatal("priced candidate did not open its window", admitted.State)
	}
	if want := nonParWantBenefit(t, upper09, proceeds, band); c.BenefitRaw != want {
		t.Fatalf("benefit %v is not the conservative reassembly %v", c.BenefitRaw, want)
	}

	// A dearer debt price strictly lowers the benefit of an admitted candidate;
	// a dearer collateral price strictly raises it through the proceeds floor.
	// Both reassemble exactly from the observed interval ends.
	bigEquity := upper11 + 1_000_000
	run := func(debt *BudgetPrice, collateral *BudgetPrice) (float64, float64) {
		t.Helper()
		in := nonParReviewFixture(t, debt, collateral, bigEquity)
		result := selectOpportunityWithLanes(in, SelectorState{}, allowed, funding)
		if result.Action != "KEEP" || result.Reason != "advantage_not_yet_persistent" {
			t.Fatal("admitted equity did not start persistence", result)
		}
		c := nonParCandidate(t, result)
		if !c.CostsKnown || c.BlockedReason != "" || c.BorrowAPR != 0.04 {
			t.Fatal("admitted candidate lost its priced economics", c)
		}
		debtUpper, proceedsFloor := nonParBounds(t, debt, collateral, route, validThrough)
		return c.BenefitRaw, nonParWantBenefit(t, debtUpper, proceedsFloor, bigEquity)
	}
	for name, tc := range map[string]struct {
		debt       *BudgetPrice
		collateral *BudgetPrice
	}{
		"debt_0.9x_coll_1.0x": {&price09, &coll10},
		"debt_1.1x_coll_1.0x": {&price11, &coll10},
	} {
		got, want := run(tc.debt, tc.collateral)
		if got != want {
			t.Fatalf("%s benefit %v is not the conservative reassembly %v", name, got, want)
		}
	}
	cheapDebt, _ := run(&price09, &coll10)
	dearDebt, _ := run(&price11, &coll10)
	if cheapDebt <= dearDebt {
		t.Fatalf("liability upper bound did not lower the benefit: %v vs %v", cheapDebt, dearDebt)
	}
	collLow := autoCollateralPriceFixture(t, 980_000)
	collHigh := autoCollateralPriceFixture(t, 1_020_000)
	lowAsset, _ := run(&price09, &collLow)
	highAsset, _ := run(&price09, &collHigh)
	if lowAsset >= highAsset {
		t.Fatalf("collateral proceeds floor did not raise the benefit: %v vs %v", lowAsset, highAsset)
	}
}

// TestAutoNonParStalePricesNeverSampleNorSelect proves, at selection level,
// that stale or window-violating evidence on either price side blocks the
// candidate without persistence, and that a stale tick cannot bridge a
// persistence window that healthy ticks built.
func TestAutoNonParStalePricesNeverSampleNorSelect(t *testing.T) {
	route, price09, _ := autoDebtPriceFixture(t, 900_000)
	coll10 := autoCollateralPriceFixture(t, 1_000_000)
	manifest := autoInitializerFixtureManifest(t)
	allowed, funding := nonParLaneAllowed(manifest), nonParFundingAllowed(manifest)
	upper09, _ := nonParBounds(t, &price09, &coll10, route, 42+budgetMaxObservationLagSlots)
	equity := upper09 + 1_000_000

	staleDebt, windowDebt, staleAsset := price09, price09, coll10
	staleDebt.ObservedSlot, staleDebt.ValidThroughSlot = 41, 73   // observed before the quote's sample slot
	windowDebt.ObservedSlot, windowDebt.ValidThroughSlot = 42, 73 // quote window outlives the price window
	staleAsset.ObservedSlot, staleAsset.ValidThroughSlot = 41, 73 // asset side, liability evidence untouched

	for name, tc := range map[string]struct {
		debt, collateral *BudgetPrice
	}{
		"stale_debt_price":        {&staleDebt, &coll10},
		"short_debt_price_window": {&windowDebt, &coll10},
		"stale_collateral_price":  {&price09, &staleAsset},
	} {
		in := nonParReviewFixture(t, tc.debt, tc.collateral, equity)
		result := selectOpportunityWithLanes(in, SelectorState{}, allowed, funding)
		if result.Action != "KEEP" || result.Reason != "no_worthwhile_executable_move" {
			t.Fatalf("%s changed the top-level hold: %+v", name, result)
		}
		c := nonParCandidate(t, result)
		if c.BlockedReason != "bounded_borrow_unavailable" {
			t.Fatalf("%s was not held on its borrow evidence: %+v", name, c)
		}
		if c.BorrowAPR != 0 || c.GainRaw != 0 || c.BenefitRaw != 0 || c.GrossGainRaw != 0 {
			t.Fatalf("%s carried forecast economics without a priceable borrow: %+v", name, c)
		}
		if len(result.State.Advantages) != 0 {
			t.Fatalf("%s sampled persistence: %+v", name, result.State)
		}
	}

	// A stale tick between two healthy ones cannot bridge persistence: the
	// window built at tick 1 is gone at tick 2 (stale evidence sampled
	// nothing), so tick 3 restarts from scratch and must hold for the full
	// persistence again before any entry.
	in := nonParReviewFixture(t, &price09, &coll10, equity)
	first := selectOpportunityWithLanes(in, SelectorState{}, allowed, funding)
	if first.Reason != "advantage_not_yet_persistent" || len(first.State.Advantages) != 1 {
		t.Fatal("healthy tick 1 did not open the window", first)
	}
	advanceSelectorFixture(&in, time.Minute)
	// Tick 2's evidence goes stale; the input is discarded after this tick.
	in.Quotes[0].DebtPrice = copyDebtPrice(&staleDebt)
	blocked := selectOpportunityWithLanes(in, first.State, allowed, funding)
	if c := nonParCandidate(t, blocked); c.BlockedReason != "bounded_borrow_unavailable" || len(blocked.State.Advantages) != 0 {
		t.Fatal("stale tick kept economics or persistence", blocked, blocked.State)
	}
	// Tick 3 evaluates healthy evidence again: the window restarts from
	// scratch and must hold the full persistence before any entry.
	fresh := nonParReviewFixture(t, &price09, &coll10, equity)
	advanceSelectorFixture(&fresh, 2*time.Minute)
	third := selectOpportunityWithLanes(fresh, blocked.State, allowed, funding)
	if third.Action != "KEEP" || third.Reason != "advantage_not_yet_persistent" {
		t.Fatal("selection crossed persistence after a stale gap", third)
	}
	if window, ok := third.State.Advantages[testAutoLane]; !ok || !window.Since.Equal(fresh.Now) {
		t.Fatal("window did not restart after the stale gap", third.State)
	}
	advanceSelectorFixture(&fresh, time.Minute)
	if entered := selectOpportunityWithLanes(fresh, third.State, allowed, funding); entered.Action != "ENTER" || entered.DestinationLane != testAutoLane || entered.SelectedQuote == nil {
		t.Fatal("healthy persistence after the stale gap did not select", entered)
	}
}

// TestAutoNonParWithdrawalPriorityPrecedesCandidates pins the source-side
// priority gate: with a withdrawal demand the selection holds before any
// candidate economics or persistence exists, even with a profitable priced
// candidate present. The same-lane keep baseline and the safe-unwind switch
// for a funded AUTO source are covered by
// TestAutoSourceKeepsEconomicsAndSelectsSafeUnwind and are not repeated.
func TestAutoNonParWithdrawalPriorityPrecedesCandidates(t *testing.T) {
	route, price09, _ := autoDebtPriceFixture(t, 900_000)
	coll10 := autoCollateralPriceFixture(t, 1_000_000)
	manifest := autoInitializerFixtureManifest(t)
	upper09, _ := nonParBounds(t, &price09, &coll10, route, 42+budgetMaxObservationLagSlots)
	in := nonParReviewFixture(t, &price09, &coll10, upper09+1_000_000)
	in.Snapshot.WithdrawalDemandRaw = 1
	result := selectOpportunityWithLanes(in, SelectorState{}, nonParLaneAllowed(manifest), nonParFundingAllowed(manifest))
	if result.Action != "KEEP" || result.Reason != "withdrawal_unwind_or_accounting_first" {
		t.Fatal("withdrawal demand did not hold the selection", result)
	}
	if len(result.Candidates) != 0 || len(result.State.Advantages) != 0 {
		t.Fatal("withdrawal hold produced candidate economics", result)
	}
}
