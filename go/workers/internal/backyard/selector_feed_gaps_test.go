package backyard

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"
)

// leveredTestMarket is lane economics whose spread clears every live leverage
// minimum, so a destination fixture keeps its levered one-pass quote.
func leveredTestMarket(lane string) LaneEconomics {
	return leverageMarket(lane, 0.20, math.Log1p(0.05))
}

func feedReserveFixture(reserve, mint, market string, now time.Time, price float64) verifiedEconomicReserve {
	f := func(v float64) *float64 { return &v }
	i := func(v int) *int { return &v }
	b := false
	return verifiedEconomicReserve{Reserve: reserve, Market: market, Mint: mint, ObservedAt: now.Add(-time.Second), Slot: 1, Hash: strings.Repeat("a", 64), Commitment: "confirmed",
		SupplyAPY: f(0.01), BorrowAPY: f(0.05), BorrowAPR: f(0.05), SupplyRaw: f(1e12), BorrowRaw: f(5e11), Status: i(0), Emergency: &b,
		Curve: []BorrowCurvePoint{{0, 463}, {8000, 517}, {9000, 517}, {9500, 568}, {10_000, 738}, {10_000, 738}}, HostBPS: f(0), Schema: i(2), PriceUSD: f(price), Decimals: i(6)}
}

// Prime/PRIME/USDS: the USDS reserve is fully borrowed and its accumulated
// fees exceed its available cash, so borrowed > total supply (prod
// 2026-10-10: 926,398,351,359 borrowed vs 926,393,022,185 supply). KLend
// prices that at the terminal curve point; the lane keeps its economics.
func TestFeedScoresLaneWhoseDebtReserveBorrowedExceedsSupply(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	usds, pyusd := primePRIMEUSDS, primePRIMEPYUSD
	reserves := map[string]verifiedEconomicReserve{
		kaminoDebtReserve:              feedReserveFixture(kaminoDebtReserve, bridgeUSDC, kaminoMarket, now, 0.9999),
		usds.Kamino.CollateralReserve:  feedReserveFixture(usds.Kamino.CollateralReserve, usds.Kamino.CollateralMint, usds.Kamino.Market, now, 1.05),
		usds.Kamino.DebtReserve:        feedReserveFixture(usds.Kamino.DebtReserve, usds.Kamino.DebtMint, usds.Kamino.Market, now, 1),
		pyusd.Kamino.DebtReserve:       feedReserveFixture(pyusd.Kamino.DebtReserve, pyusd.Kamino.DebtMint, pyusd.Kamino.Market, now, 0.9998),
		pyusd.Kamino.CollateralReserve: feedReserveFixture(pyusd.Kamino.CollateralReserve, pyusd.Kamino.CollateralMint, pyusd.Kamino.Market, now, 1.05),
	}
	over := reserves[usds.Kamino.DebtReserve]
	supply, borrowed := 926_393_022_185.062, 926_398_351_359.7037
	over.SupplyRaw, over.BorrowRaw = &supply, &borrowed
	reserves[usds.Kamino.DebtReserve] = over
	yields := map[string]nativeYield{usds.Lane: {0.05, now.Add(-time.Minute), "y"}, pyusd.Lane: {0.05, now.Add(-time.Minute), "y"}}
	got := map[string]LaneEconomics{}
	for _, e := range combineEconomics([]RuntimeRoute{usds, pyusd}, reserves, yields, now, DefaultSelectorPolicy()) {
		got[e.Lane] = e
	}
	e, ok := got[usds.Lane]
	if !ok {
		t.Fatal("over-utilized USDS lane dropped from the feed")
	}
	if want := math.Expm1(0.0738); math.Abs(e.CurrentBorrowAPY-want) > 1e-12 {
		t.Fatalf("over-utilized borrow APY %.6f, want terminal %.6f", e.CurrentBorrowAPY, want)
	}
	// Any further borrow also prices at the terminal point, never errors.
	if apr, err := projectedBorrowAPR(e, 1e9); err != nil || math.Abs(apr-0.0738) > 1e-12 {
		t.Fatalf("projected terminal APR %v %v", apr, err)
	}
	if math.Abs(e.DebtRawPerUSDCRaw-0.9999) > 1e-12 {
		t.Fatalf("USDS conversion %.6f, want USDC/USDS price 0.9999", e.DebtRawPerUSDCRaw)
	}
	if p, ok := got[pyusd.Lane]; !ok || math.Abs(p.DebtRawPerUSDCRaw-0.9999/0.9998) > 1e-12 {
		t.Fatalf("PYUSD lane conversion %+v", p)
	}
	// Without a verified USDC reference the conversion stays unknown; the
	// lane's 1x economics remain.
	delete(reserves, kaminoDebtReserve)
	for _, e := range combineEconomics([]RuntimeRoute{usds}, reserves, yields, now, DefaultSelectorPolicy()) {
		if e.DebtRawPerUSDCRaw != 0 {
			t.Fatalf("conversion without a USDC reference: %+v", e)
		}
	}
}

// Prime/PRIME/PYUSD is not the vault's lane, so no observed snapshot carries
// its debt price. Its spread is priced off its own feed economics, and equals
// the spread computed for the same economics when it is the source lane.
func TestLeverageWatchPricesSpreadForNonSourcePYUSDLane(t *testing.T) {
	t.Parallel()
	prime := leverageMarket(primePRIMEPYUSD.Lane, 0.10, math.Log1p(0.06))
	prime.DebtRawPerUSDCRaw = 1.0001
	auto := leverageMarket(autoAUTOPYUSD.Lane, 0.10, math.Log1p(0.06))
	auto.DebtRawPerUSDCRaw = 1.0001
	source := leverageSnapshot(1.5) // the vault holds AUTO
	var w leverageWatch
	w.observe([]LaneEconomics{auto, prime}, autoAUTOPYUSD.Lane, 1_000_000_000, true, source)
	spreads := map[string]*int64{}
	for _, lane := range w.summary {
		spreads[lane.Lane] = lane.SpreadBPS
	}
	if spreads[primePRIMEPYUSD.Lane] == nil {
		t.Fatal("non-source PYUSD lane spread unavailable")
	}
	if want := int64(math.Round((0.10 - 0.06) * 10_000)); *spreads[primePRIMEPYUSD.Lane] != want {
		t.Fatalf("Prime/PYUSD spread %d bps, want %d", *spreads[primePRIMEPYUSD.Lane], want)
	}
	if spreads[autoAUTOPYUSD.Lane] == nil {
		t.Fatal("source PYUSD lane spread unavailable")
	}
	// A non-USDC lane with no verified price has no levered column.
	prime.DebtRawPerUSDCRaw = 0
	if _, ok := leverageSpread(prime, 1.5, 1_000_000_000, false); ok {
		t.Fatal("unpriced non-USDC spread reported")
	}
}

// The entry quote levers to 1.5x only where the live leverage rule would: a
// negative-spread lane (Maple on 2026-10-10: -4.43%) is a 1x candidate, as is
// one below the 1% minimum; at the minimum it levers.
func TestSelectorDestinationLeversOnlyAtTheLiveSpreadMinimum(t *testing.T) {
	t.Parallel()
	quote := func(market LaneEconomics, closeBorrowing bool) selectorDestinationQuote {
		t.Helper()
		m, rpc, client, accounts := selectorDestinationFixture(t)
		if closeBorrowing {
			binary.LittleEndian.PutUint64(accountAt(accounts, mapleSyrupUSDCUSDC.Kamino.DebtReserve).Data[kaminoOutsideBorrowLimitOffset:], 0)
		}
		q, err := observeSelectorDestinationForecast(context.Background(), rpc, fixtureView(t, rpc), client, m, capturedTestPolicies(), market, 100_000_000, 42, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	for _, tc := range []struct {
		name       string
		yield, apr float64
		levered    bool
	}{
		{"negative spread", 0.04, math.Log1p(0.0843), false},
		{"below 1 pt", 0.10, math.Log1p(0.095), false},
		{"just above 1 pt", 0.10, math.Log1p(0.0899), true},
	} {
		q := quote(leverageMarket(SelectedRouteID, tc.yield, tc.apr), false)
		if tc.levered {
			if q.Unlevered || q.SpreadUnlevered || q.BorrowReceiveRaw == 0 {
				t.Fatalf("%s: not levered: %+v", tc.name, q)
			}
			continue
		}
		if !q.Unlevered || !q.SpreadUnlevered || q.BorrowReceiveRaw != 0 || q.BorrowFeeRaw != 0 {
			t.Fatalf("%s: levered quote: unlevered=%t spread=%t borrow=%d", tc.name, q.Unlevered, q.SpreadUnlevered, q.BorrowReceiveRaw)
		}
	}
	// A closed debt reserve is still 1x because borrowing is blocked, not by
	// the spread rule: the sample line keeps saying borrow=blocked.
	if q := quote(leveredTestMarket(SelectedRouteID), true); !q.Unlevered || q.SpreadUnlevered {
		t.Fatalf("blocked borrowing labelled as a spread choice: %+v", q)
	}
}

// A small levered USDC tranche whose expected move cost exceeds half its
// amount: invested is already amount - cost, so the post-move NAV is
// amount - cost (+ proceeds - debt), never amount - 2*cost. With the cost
// counted twice the forecast's InitialNAV went negative and the fee forecast
// became unavailable (Maple, 2026-10-10).
func TestSelectorForecastCountsMoveCostOnce(t *testing.T) {
	t.Parallel()
	const amount, expected = int64(100_000_000), int64(60_000_000)
	exp := expected
	q := MoveQuote{DestinationLane: SelectedRouteID, EquityRaw: amount, CostRaw: 90_000_000, ExpectedCostRaw: &exp, BorrowReceiveRaw: 20_000_000, BorrowFeeRaw: 20_000}
	market := leverageMarket(SelectedRouteID, 0.12, math.Log1p(0.06))
	years := DefaultSelectorPolicy().Horizon.Hours() / (365.25 * 24)
	invested := float64(amount - q.selectorEconomicCostRaw())
	e, blocked := pilotQuoteEconomics(q, market, invested, years)
	if blocked != "" {
		t.Fatal(blocked)
	}
	if want := float64(amount-expected) + e.Proceeds - e.Debt; e.InitialNAV != want {
		t.Fatalf("InitialNAV %.0f, want post-move NAV %.0f", e.InitialNAV, want)
	}
	income := e.Gain + float64(expected)
	if math.Abs(e.EndingNAV-(e.InitialNAV+income)) > 1e-6 {
		t.Fatalf("EndingNAV %.0f, want InitialNAV + income %.0f", e.EndingNAV, e.InitialNAV+income)
	}
	s := base()
	s.TotalVaultNAVRaw, s.StrategyNAVRaw = 1_000_000_000, 1_000_000_000
	armFeeAuthorityFixture(t, &s)
	if _, known := selectorFeeReservedGain(s, DefaultSelectorPolicy().Horizon, e, float64(s.TotalVaultNAVRaw-amount)); !known {
		t.Fatal("fee forecast unavailable for a tranche whose cost exceeds half its amount")
	}
}
