package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"
)

const testAutoLane = "AUTO/AUTO/PYUSD"

// autoDebtPriceFixture observes a real BudgetPrice for the AUTO lane's PYUSD
// debt through the established budget valuation path, at tokenPriceScale
// millionths times parity (1e6 == observed at exact 1 PYUSD/USDC).
func autoDebtPriceFixture(t *testing.T, tokenPriceScale int64) (RuntimeRoute, BudgetPrice, *RPCClient) {
	t.Helper()
	route, err := runtimeRoute(testAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	scaled := new(big.Int).Mul(one, big.NewInt(tokenPriceScale))
	scaled.Quo(scaled, big.NewInt(1_000_000))
	debtReserve := reserveFixture(t, route.Kamino.DebtReserve, route.Kamino.DebtMint, 42, scaled, 1_000_000, 1_000_000)
	putKey(t, debtReserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(debtReserve.Data[264:272], 1000)
	mint := ConfirmedAccount{Address: route.Kamino.DebtMint, Owner: token2022Program, Lamports: 1, Data: make([]byte, 82)}
	mint.Data[44], mint.Data[45] = 6, 1
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, []ConfirmedAccount{debtReserve, mint})
	price, err := ObserveBudgetTokenPrice(context.Background(), rpc, route.Lane, ExecutableDebit{Mint: route.Kamino.DebtMint, TokenProgram: token2022Program, Raw: 1_000}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if price.Mint != route.Kamino.DebtMint || price.TokenProgram != token2022Program || price.Decimals != 6 ||
		price.ObservedSlot != 42 || price.ValidThroughSlot != 42+budgetMaxObservationLagSlots ||
		price.Source != "confirmed-chain-accounts" || len(price.EvidenceSHA256) != 64 || price.Credit == nil {
		t.Fatalf("price identity: %+v", price)
	}
	return route, price, rpc
}

// autoCollateralPriceFixture observes a real BudgetPrice for the AUTO lane's
// classic-SPL collateral through the established budget valuation path, at
// tokenPriceScale millionths times parity (1e6 == observed at exact
// 1 AUTO/USDC).
func autoCollateralPriceFixture(t *testing.T, tokenPriceScale int64) BudgetPrice {
	t.Helper()
	route, err := runtimeRoute(testAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	scaled := new(big.Int).Mul(one, big.NewInt(tokenPriceScale))
	scaled.Quo(scaled, big.NewInt(1_000_000))
	reserve := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, scaled, 1_000_000, 1_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	mint := ConfirmedAccount{Address: route.Kamino.CollateralMint, Owner: classicTokenProgram, Lamports: 1, Data: make([]byte, 82)}
	mint.Data[44], mint.Data[45] = 6, 1
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, []ConfirmedAccount{reserve, mint})
	price, err := ObserveBudgetTokenPrice(context.Background(), rpc, route.Lane, ExecutableDebit{Mint: route.Kamino.CollateralMint, TokenProgram: classicTokenProgram, Raw: 1_000}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if price.Mint != route.Kamino.CollateralMint || price.TokenProgram != classicTokenProgram || price.Decimals != 6 || price.ObservedSlot != 42 || price.Credit == nil {
		t.Fatalf("collateral price identity: %+v", price)
	}
	return price
}

// debtPriceEqual compares observed evidence by value. BudgetPrice carries a
// nested Credit pointer, so == would compare pointer identity, not the
// interval the evidence asserts.
func debtPriceEqual(a, b BudgetPrice) bool {
	if a.Credit == nil || b.Credit == nil {
		return a.Credit == b.Credit
	}
	return a.Source == b.Source && a.Mint == b.Mint && a.TokenProgram == b.TokenProgram && a.Decimals == b.Decimals &&
		a.TokenUpperSF == b.TokenUpperSF && a.USDCLowerSF == b.USDCLowerSF &&
		a.ObservedSlot == b.ObservedSlot && a.ValidThroughSlot == b.ValidThroughSlot && a.EvidenceSHA256 == b.EvidenceSHA256 &&
		a.Credit.TokenLowerSF == b.Credit.TokenLowerSF && a.Credit.USDCUpperSF == b.Credit.USDCUpperSF
}

func TestSelectorDestinationDebtPriceWiring(t *testing.T) {
	route, price, rpc := autoDebtPriceFixture(t, 1_000_000)
	got, err := selectorDestinationDebtPrice(context.Background(), rpc, route, 1_000, 42)
	if err != nil || got == nil || !debtPriceEqual(*got, price) {
		t.Fatalf("wiring: %+v %v", got, err)
	}
	usdc, _ := runtimeRoute(SelectedRouteID)
	if p, err := selectorDestinationDebtPrice(context.Background(), rpc, usdc, 1_000, 42); err != nil || p != nil {
		t.Fatal("USDC debt lane must not carry price evidence", p, err)
	}
	if _, err := selectorDestinationDebtPrice(context.Background(), rpc, route, 0, 42); err == nil {
		t.Fatal("zero borrow ceiling accepted")
	}
	// A classic-program PYUSD mint fails the strict Token-2022 parser.
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	wrongProgram := reserveFixture(t, route.Kamino.DebtReserve, route.Kamino.DebtMint, 42, one, 1_000_000, 1_000_000)
	binary.LittleEndian.PutUint64(wrongProgram.Data[264:272], 1000)
	classicMint := ConfirmedAccount{Address: route.Kamino.DebtMint, Owner: classicTokenProgram, Lamports: 1, Data: make([]byte, 82)}
	classicMint.Data[44], classicMint.Data[45] = 6, 1
	wrongRPC := budgetBuildRPCWithAccounts(t, 5000, 42, []ConfirmedAccount{wrongProgram, classicMint})
	if _, err := ObserveBudgetTokenPrice(context.Background(), wrongRPC, route.Lane, ExecutableDebit{Mint: route.Kamino.DebtMint, TokenProgram: token2022Program, Raw: 1_000}, 42); err == nil {
		t.Fatal("mint program mismatch accepted")
	}
}

func TestBudgetPriceMarginsBracketParity(t *testing.T) {
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)
	upper, err := price.valueUpper(1_000_000, price.Mint, price.TokenProgram, 42)
	if err != nil || upper <= 1_000_000 {
		t.Fatalf("upper=%d err=%v", upper, err)
	}
	lower, err := price.valueLower(1_000_000, price.Mint, price.TokenProgram, 42)
	if err != nil || lower >= 1_000_000 || upper-lower < 19_000 {
		t.Fatalf("lower=%d upper=%d err=%v", lower, upper, err)
	}
	if _, err := price.valueUpper(1, "wrong", price.TokenProgram, 42); err == nil {
		t.Fatal("foreign mint valued")
	}
	if _, err := price.valueUpper(1, price.Mint, price.TokenProgram, price.ValidThroughSlot+1); err == nil {
		t.Fatal("stale slot valued")
	}
}

// autoMoveQuote is the canonical priced AUTO entry quote: equity 1 USDC,
// half borrowed as PYUSD at receive 499_000_000 + fee 1_000_000 raw. The
// asset side carries its own observed collateral price evidence over the
// bounded leverage purchase output, exactly as doc-12 production economics
// requires; the debt price never lifts it.
func autoMoveQuote(t *testing.T, price *BudgetPrice) MoveQuote {
	t.Helper()
	asset := autoCollateralPriceFixture(t, 1_000_000)
	q := MoveQuote{MinimumIdleRaw: 1_000_000_000, BorrowReceiveRaw: 499_000_000, BorrowFeeRaw: 1_000_000, SourceLane: SelectedRouteID, DestinationLane: testAutoLane, ObservationID: "obs", EquityRaw: 1_000_000_000, CostRaw: 10_000, SampleSlot: 42, ValidThroughSlot: 74, DebtPrice: copyDebtPrice(price)}
	q.CollateralAssetPrice = copyDebtPrice(&asset)
	q.RedepositCollateralRaw = 499_000_000
	route, err := runtimeRoute(testAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	assetValue, err := asset.valueLower(q.RedepositCollateralRaw, route.Kamino.CollateralMint, route.CollateralTokenProgram, q.ValidThroughSlot)
	if err != nil || assetValue <= 0 || assetValue >= int64(q.RedepositCollateralRaw) {
		t.Fatalf("asset estimate: %d %v", assetValue, err)
	}
	assetRaw := uint64(assetValue)
	q.CollateralAssetUSDCRaw = &assetRaw
	return q
}

// pricedQuoteRun evaluates the production pilotQuoteEconomics candidate path
// on a priced quote: raw receive+fee for reserve utilization,
// valueUpper(receive+fee) for the liability, valueLower(receive) for the
// redeposited proceeds. The SelectOpportunity loop itself cannot reach a
// non-USDC lane while it is gated out of selectorLanes (see
// TestGatedAutoLaneNeverReachesFeedEconomics), so the shared production
// function is called directly — any unit regression in it fails here.
type pricedRun struct {
	receive, raw, debt, proceeds uint64
	apr, gain, invested, years   float64
}

func evaluatePricedQuote(t *testing.T, q MoveQuote, m LaneEconomics) pricedRun {
	t.Helper()
	if !q.validBorrow() {
		t.Fatal("priced quote rejected")
	}
	out := pricedRun{receive: q.BorrowReceiveRaw, invested: float64(q.EquityRaw - q.CostRaw), years: float64((7 * 24 * time.Hour).Hours()) / (365.25 * 24)}
	e, blocked := pilotQuoteEconomics(true, q, m, out.invested, out.years)
	if blocked != "" {
		t.Fatal(blocked)
	}
	out.raw, out.debt, out.proceeds = uint64(e.DebtRaw), uint64(e.Debt), uint64(e.Proceeds)
	out.apr, out.gain = e.APR, e.Gain
	return out
}

func pricedQuoteRun(t *testing.T, m LaneEconomics, price *BudgetPrice) pricedRun {
	t.Helper()
	return evaluatePricedQuote(t, autoMoveQuote(t, price), m)
}

// pricedAssetRun evaluates the same production economics on a quote whose
// purchased collateral output and observed collateral price evidence are
// stated independently of the debt price, so the asset side can be moved
// without touching the liability. It also returns the exact proceeds lower
// bound the quote's asset evidence asserts.
func pricedAssetRun(t *testing.T, m LaneEconomics, price *BudgetPrice, redepositRaw uint64, assetScale int64) (pricedRun, uint64) {
	t.Helper()
	asset := autoCollateralPriceFixture(t, assetScale)
	q := autoMoveQuote(t, price)
	q.RedepositCollateralRaw = redepositRaw
	q.CollateralAssetPrice = copyDebtPrice(&asset)
	route, err := runtimeRoute(testAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	assetValue, err := asset.valueLower(q.RedepositCollateralRaw, route.Kamino.CollateralMint, route.CollateralTokenProgram, q.ValidThroughSlot)
	if err != nil || assetValue <= 0 || assetValue >= int64(redepositRaw) {
		t.Fatalf("asset estimate: %d %v", assetValue, err)
	}
	assetRaw := uint64(assetValue)
	q.CollateralAssetUSDCRaw = &assetRaw
	return evaluatePricedQuote(t, q, m), assetRaw
}

func TestOffPegDebtKeepsReserveAPRAndMovesUSDCEconomics(t *testing.T) {
	_, parityPrice, _ := autoDebtPriceFixture(t, 1_000_000)
	_, highPrice, _ := autoDebtPriceFixture(t, 1_050_000)
	// Sloped in the utilization region the borrow lands in (pinned flat in the
	// second fixture): raw versus USDC-valued debt must produce different
	// projected APRs under the sloped curve. The reserve APR is a RAW quantity,
	// so a debt-only repricing can move the USDC liability and everything
	// derived from it — never the raw projection or the asset side.
	for name, tc := range map[string]struct {
		market        LaneEconomics
		loU, loB, hiU float64
		hiB           float64
	}{
		"sloped_utilization_curve": {LaneEconomics{Lane: testAutoLane, NativeAPY: .15, SupplyAPY: 0, CurrentBorrowAPY: .04, BorrowCurve: []BorrowCurvePoint{{0, 100}, {2000, 800}, {4000, 3000}, {8000, 6000}, {10000, 10000}}, DebtSupplyRaw: 2e9, DebtBorrowRaw: 0}, 2000, 800, 4000, 3000},
		"flat_utilization_curve":   {LaneEconomics{Lane: testAutoLane, NativeAPY: .15, SupplyAPY: 0, CurrentBorrowAPY: .04, BorrowCurve: []BorrowCurvePoint{{0, 100}, {2500, 100}, {10000, 10000}}, DebtSupplyRaw: 2e9, DebtBorrowRaw: 0}, 0, 100, 2500, 100},
	} {
		t.Run(name, func(t *testing.T) {
			m := tc.market
			base := pricedQuoteRun(t, m, &parityPrice)
			repriced := pricedQuoteRun(t, m, &highPrice)
			// Identical raw borrowing in both runs, and the reserve utilization
			// behind the APR is a RAW quantity: interpolated by hand here, not
			// via projectedBorrowAPR, so the unit discipline is asserted
			// independently of the code under test.
			if repriced.raw != base.raw || repriced.receive != base.receive {
				t.Fatalf("raw borrowing moved with price: %+v %+v", base, repriced)
			}
			u := (0.0 + float64(base.raw)) / m.DebtSupplyRaw * 10_000
			wantAPR := (tc.loB + (tc.hiB-tc.loB)*(u-tc.loU)/(tc.hiU-tc.loU) + 0.0) / 10_000.0
			if base.apr != wantAPR || repriced.apr != wantAPR {
				t.Fatalf("APR=%v/%v want raw-unit %v", base.apr, repriced.apr, wantAPR)
			}
			// The fixture must genuinely distinguish: had the candidate block
			// fed the USDC-valued liability into the projection, the APR here
			// would differ, so this regression fails on the old bug.
			if buggy, err := projectedBorrowAPR(m, float64(base.debt)); err != nil || buggy == wantAPR {
				t.Fatalf("fixture insensitive to USDC-valued APR: buggy=%v err=%v", buggy, err)
			}
			// The purchased collateral output and its observed asset price are
			// independent evidence: a debt-only repricing leaves the redeposited
			// proceeds lower bound exactly where it was — never a
			// liability-margin windfall on the asset side. Only the priced
			// liability moves with the token price.
			if repriced.proceeds != base.proceeds {
				t.Fatalf("debt repricing moved asset proceeds: base=%d repriced=%d", base.proceeds, repriced.proceeds)
			}
			if base.debt <= base.raw || base.proceeds >= base.receive || repriced.debt <= base.debt {
				t.Fatalf("priced bounds: base=%d/%d repriced debt=%d", base.debt, base.proceeds, repriced.debt)
			}
			// Through the liability, the priced interest rises and the net gain
			// falls — unconditionally, because the asset side is pinned above.
			interest := func(run pricedRun) float64 { return float64(run.debt) * math.Expm1(run.apr*run.years) }
			if interest(repriced) <= interest(base) {
				t.Fatalf("interest did not rise with the debt price: %v -> %v", interest(base), interest(repriced))
			}
			if repriced.gain >= base.gain {
				t.Fatalf("gain did not fall with the repriced interest: %v -> %v", base.gain, repriced.gain)
			}
			// Net gain reassembled from independently stated principal asset
			// value and debt interest value, for each price.
			for _, run := range []pricedRun{base, repriced} {
				assetValue := run.invested + float64(run.proceeds)
				want := assetValue*math.Expm1((math.Log1p(m.NativeAPY)+math.Log1p(m.SupplyAPY))*run.years) - interest(run) - 10_000.0
				if run.gain != want {
					t.Fatalf("gain=%v want asset %v minus interest %v", run.gain, assetValue, interest(run))
				}
			}
		})
	}
	// Asset economics move only with actual collateral output or the asset's
	// own price evidence, and never touch the priced liability.
	t.Run("asset_evidence_moves_proceeds_not_liability", func(t *testing.T) {
		m := LaneEconomics{Lane: testAutoLane, NativeAPY: .15, SupplyAPY: 0, CurrentBorrowAPY: .04, BorrowCurve: []BorrowCurvePoint{{0, 100}, {2000, 800}, {4000, 3000}, {8000, 6000}, {10000, 10000}}, DebtSupplyRaw: 2e9, DebtBorrowRaw: 0}
		base, baseWant := pricedAssetRun(t, m, &parityPrice, 499_000_000, 1_000_000)
		moreOutput, moreWant := pricedAssetRun(t, m, &parityPrice, 520_000_000, 1_000_000)
		dearerAsset, dearerWant := pricedAssetRun(t, m, &parityPrice, 499_000_000, 1_020_000)
		for _, tc := range []struct {
			name     string
			run      pricedRun
			want     uint64
			rawUpper uint64
		}{{"base", base, baseWant, 499_000_000}, {"more_output", moreOutput, moreWant, 520_000_000}, {"dearer_asset", dearerAsset, dearerWant, 499_000_000}} {
			if tc.run.proceeds != tc.want || tc.run.proceeds >= tc.rawUpper {
				t.Fatalf("%s proceeds=%d want=%d upper=%d", tc.name, tc.run.proceeds, tc.want, tc.rawUpper)
			}
			if tc.run.raw != base.raw || tc.run.apr != base.apr {
				t.Fatalf("%s moved raw economics: %+v vs %+v", tc.name, tc.run, base)
			}
		}
		// The liability is priced from the borrow side alone: changed output or
		// asset price evidence leaves it byte-identical.
		if moreOutput.debt != base.debt || dearerAsset.debt != base.debt {
			t.Fatalf("asset evidence moved the liability: %d/%d vs %d", moreOutput.debt, dearerAsset.debt, base.debt)
		}
		if moreOutput.proceeds <= base.proceeds || dearerAsset.proceeds <= base.proceeds {
			t.Fatalf("asset evidence did not move proceeds: %d -> %d/%d", base.proceeds, moreOutput.proceeds, dearerAsset.proceeds)
		}
		if moreOutput.gain <= base.gain || dearerAsset.gain <= base.gain {
			t.Fatalf("asset evidence did not move the gain: %v -> %v/%v", base.gain, moreOutput.gain, dearerAsset.gain)
		}
		for _, run := range []pricedRun{base, moreOutput, dearerAsset} {
			assetValue := run.invested + float64(run.proceeds)
			interestValue := float64(run.debt) * math.Expm1(run.apr*run.years)
			want := assetValue*math.Expm1((math.Log1p(m.NativeAPY)+math.Log1p(m.SupplyAPY))*run.years) - interestValue - 10_000.0
			if run.gain != want {
				t.Fatalf("gain=%v want asset %v minus interest %v", run.gain, assetValue, interestValue)
			}
		}
	})
}

func TestNonUSDCDebtQuoteRequiresBoundPriceEvidence(t *testing.T) {
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)
	quote := autoMoveQuote(t, &price)
	if !quote.validBorrow() {
		t.Fatal("evidence-backed quote rejected")
	}
	// Missing non-USDC price evidence is rejected, never waivered.
	naked := quote
	naked.DebtPrice = nil
	if naked.validBorrow() {
		t.Fatal("unpriced non-USDC borrow accepted")
	}
	if _, ok := naked.borrowDebtUSDCRaw(); ok {
		t.Fatal("unpriced debt valued")
	}
	if _, ok := naked.borrowProceedsUSDCRaw(); ok {
		t.Fatal("unpriced proceeds valued")
	}
	// Identity is bound through the destination lane's route.
	foreign := quote
	foreignPrice := *quote.DebtPrice
	foreignPrice.Mint = bridgeUSDC
	foreign.DebtPrice = &foreignPrice
	if foreign.validBorrow() {
		t.Fatal("foreign mint identity accepted")
	}
	wrongProgram := quote
	wrongPrice := *quote.DebtPrice
	wrongPrice.TokenProgram = classicTokenProgram
	wrongProgram.DebtPrice = &wrongPrice
	if wrongProgram.validBorrow() {
		t.Fatal("foreign program identity accepted")
	}
	// The price observation cannot predate the quote's sample.
	stale := quote
	stalePrice := *quote.DebtPrice
	stalePrice.ObservedSlot = 9
	stale.DebtPrice = &stalePrice
	if stale.validBorrow() {
		t.Fatal("price predating the sample accepted")
	}
	// The quote may not outlive the price window.
	expired := quote
	expiredPrice := *quote.DebtPrice
	expired.ValidThroughSlot = expiredPrice.ValidThroughSlot + 1
	expired.DebtPrice = &expiredPrice
	if expired.validBorrow() {
		t.Fatal("quote outliving its price window accepted")
	}
	// Equity bounds the priced liability, not raw units.
	oversized := quote
	oversized.EquityRaw = int64(quote.BorrowReceiveRaw - 1)
	if oversized.validBorrow() {
		t.Fatal("priced liability above equity accepted")
	}
	// USDC lanes keep exact legacy semantics without evidence.
	usdc, _ := runtimeRoute(SelectedRouteID)
	legacy := MoveQuote{DestinationLane: usdc.Lane, EquityRaw: 2_000_000, BorrowReceiveRaw: 900_000, BorrowFeeRaw: 1_000}
	if !legacy.validBorrow() {
		t.Fatal("legacy USDC quote rejected")
	}
	if d, ok := legacy.borrowDebtUSDCRaw(); !ok || d != 901_000 {
		t.Fatalf("legacy identity: %d %v", d, ok)
	}
	if p, ok := legacy.borrowProceedsUSDCRaw(); !ok || p != 901_000 {
		t.Fatalf("legacy proceeds: %d %v", p, ok)
	}
	over := legacy
	over.BorrowReceiveRaw = 2_000_001
	if over.validBorrow() {
		t.Fatal("legacy equity bound relaxed")
	}
	feeBound := legacy
	feeBound.BorrowFeeRaw = 1_100_001
	if feeBound.validBorrow() {
		t.Fatal("legacy fee bound relaxed")
	}
}

func TestGatedAutoLaneNeverReachesFeedEconomics(t *testing.T) {
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)
	route, _ := runtimeRoute(testAutoLane)
	in := selectorFixture()
	in.Snapshot.PilotActive = true
	in.Snapshot.Slot = 42
	in.Markets = []LaneEconomics{{Lane: route.Lane, EvidenceID: "rates", ObservedAt: in.Now, NativeObservedAt: in.Now, NativeAPY: .15, SupplyAPY: 0, CurrentBorrowAPY: .04, BorrowCurve: []BorrowCurvePoint{{0, 400}, {8000, 400}, {10000, 10000}}, DebtSupplyRaw: 1e15, DebtBorrowRaw: 1e14, EntryCapacity: Capacity{Known: true, Unlimited: true}}}
	in.Quotes = []MoveQuote{autoMoveQuote(t, &price)}
	// While the lane is not in selectorLanes, market validation rejects it
	// before any economics: no advantage window, no display forecast, no
	// borrow projection. That is the staging that keeps mixed-unit feed math
	// unreachable; the priced per-quote composition is covered directly in
	// TestOffPegDebtKeepsReserveAPRAndMovesUSDCEconomics until entry
	// enablement lands.
	first := SelectOpportunity(in, SelectorState{})
	if len(first.Candidates) != 1 {
		t.Fatalf("candidates: %+v", first.Candidates)
	}
	if c := first.Candidates[0]; c.Lane != route.Lane || c.BlockedReason != "economic_evidence_unavailable" || c.BorrowAPR != 0 || c.GrossGainRaw != 0 {
		t.Fatalf("gated lane produced economics: %+v", c)
	}
	if len(first.State.Advantages) != 0 {
		t.Fatalf("gated lane sampled a persistence window: %+v", first.State.Advantages)
	}
	advanceSelectorFixture(&in, 31*time.Second)
	second := SelectOpportunity(in, first.State)
	if len(second.State.Advantages) != 0 {
		t.Fatalf("gated lane accumulated persistence: %+v", second.State.Advantages)
	}
	// USDC lanes keep sampling windows from the same feed math.
	usdcSample := SelectOpportunity(selectorFixture(), SelectorState{})
	if _, sampled := usdcSample.State.Advantages["OnRe/ONyc/USDC"]; !sampled {
		t.Fatalf("USDC persistence regressed: %+v", usdcSample.State)
	}
}

func TestAutoPairCapacityFailsClosedUntilLaneAdmission(t *testing.T) {
	route, err := runtimeRoute(testAutoLane)
	if err != nil {
		t.Fatal(err)
	}
	// kaminoPairEntryCapacity returns DEBT-denominated equity for admitted
	// lanes (twice the borrow headroom in raw debt-mint units). AUTO is not a
	// selector lane yet, so its capacity must fail closed rather than be read
	// as USDC; admission is the explicit enablement step.
	if got, err := kaminoPairEntryCapacity(KaminoPosition{EntryCapacityRaw: 6600}, nil, route); err == nil || got != 0 || !strings.Contains(err.Error(), "pair_capacity_lane_unreviewed") {
		t.Fatalf("unadmitted lane capacity: %d %v", got, err)
	}
}

func TestComposedDebtPriceIsAnImmutableCopy(t *testing.T) {
	original := BudgetPrice{Mint: "mint", TokenProgram: "program", Decimals: 6, TokenUpperSF: [16]byte{1}, USDCLowerSF: [16]byte{2}, ObservedSlot: 1, ValidThroughSlot: 2, EvidenceSHA256: strings.Repeat("a", 64), Credit: &BudgetCreditBounds{TokenLowerSF: [16]byte{3}, USDCUpperSF: [16]byte{4}}}
	copied := copyDebtPrice(&original)
	original.Mint = "mutated"
	original.TokenUpperSF[0] = 9
	original.USDCLowerSF[0] = 9
	original.Credit.TokenLowerSF[0] = 9
	original.Credit.USDCUpperSF[0] = 9
	original.Credit = nil
	if copied.Mint != "mint" || copied.TokenUpperSF[0] != 1 || copied.USDCLowerSF[0] != 2 {
		t.Fatal("composed price copied by reference")
	}
	if copied.Credit == nil || copied.Credit.TokenLowerSF[0] != 3 || copied.Credit.USDCUpperSF[0] != 4 {
		t.Fatal("nested Credit copied by reference")
	}
	if copyDebtPrice(nil) != nil {
		t.Fatal("USDC lanes must not grow evidence")
	}
}

func TestComposeRejectsForeignDebtPrice(t *testing.T) {
	_, price, _ := autoDebtPriceFixture(t, 1_000_000)
	route, _ := runtimeRoute(testAutoLane)
	foreign := price
	foreign.Mint = bridgeUSDC
	destination := selectorDestinationQuote{Lane: route.Lane, EquityRaw: 1, DebtPrice: &foreign, Recipe: selectorRecipe{CostRaw: 1, ValidThroughSlot: 74, EvidenceID: strings.Repeat("a", 64)}}
	source := selectorSourceQuote{Lane: route.Lane, ObservationID: "o", MinimumIdleRaw: 1, Recipe: selectorRecipe{CostRaw: 1, ValidThroughSlot: 74, EvidenceID: strings.Repeat("a", 64)}}
	s := base()
	s.Slot = 42
	_, err := composeSelectorMove(context.Background(), nil, Observation{Snapshot: s}, source, destination)
	assertBudgetHold(t, err, "selector_debt_price_identity_invalid")
	// A price observed before the sample is equally rejected.
	early := price
	early.ObservedSlot = 9
	destination.DebtPrice = &early
	_, err = composeSelectorMove(context.Background(), nil, Observation{Snapshot: s}, source, destination)
	assertBudgetHold(t, err, "selector_debt_price_identity_invalid")
}
