package backyard

import (
	"math"
	"testing"
	"time"
)

// autoMoveQuote is an AUTO entry whose PYUSD up step is valued at debtScale
// millionths of parity (1e6 == 1 PYUSD/USDC): the batch's debt price values
// both the liability (receive plus fee) and the principal it converts.
func autoMoveQuote(debtScale int64) MoveQuote {
	q := MoveQuote{MinimumIdleRaw: 1_000_000_000, BorrowReceiveRaw: 499_000_000, BorrowFeeRaw: 1_000_000, SourceLane: SelectedRouteID, DestinationLane: testAutoLane, ObservationID: "obs", EquityRaw: 1_000_000_000, CostRaw: 10_000, SampleSlot: 42, ValidThroughSlot: 74}
	q.BorrowValueRaw = (q.BorrowReceiveRaw + q.BorrowFeeRaw) * uint64(debtScale) / 1_000_000
	q.ProceedsValueRaw = q.BorrowReceiveRaw * uint64(debtScale) / 1_000_000
	return q
}

type pricedRun struct {
	raw, debt, proceeds        uint64
	apr, gain, invested, years float64
}

func evaluatePricedQuote(t *testing.T, q MoveQuote, m LaneEconomics) pricedRun {
	t.Helper()
	if !q.validBorrow() {
		t.Fatal("priced quote rejected")
	}
	out := pricedRun{invested: float64(q.EquityRaw - q.CostRaw), years: (7 * 24 * time.Hour).Hours() / (365.25 * 24)}
	e, blocked := pilotQuoteEconomics(q, m, out.invested, out.years)
	if blocked != "" {
		t.Fatal(blocked)
	}
	out.raw, out.debt, out.proceeds = uint64(e.DebtRaw), uint64(e.Debt), uint64(e.Proceeds)
	out.apr, out.gain = e.APR, e.Gain
	return out
}

// An off-peg debt price moves the USDC economics of the same raw borrow, both
// sides of it, and never the reserve's projected APR: utilization is a raw
// quantity of the debt reserve.
func TestOffPegDebtKeepsReserveAPRAndMovesUSDCEconomics(t *testing.T) {
	t.Parallel()
	m := LaneEconomics{Lane: testAutoLane, NativeAPY: .15, SupplyAPY: 0, CurrentBorrowAPY: .04, BorrowCurve: []BorrowCurvePoint{{0, 100}, {2000, 800}, {4000, 3000}, {8000, 6000}, {10000, 10000}}, DebtSupplyRaw: 2e9, DebtBorrowRaw: 0}
	base, repriced := evaluatePricedQuote(t, autoMoveQuote(1_000_000), m), evaluatePricedQuote(t, autoMoveQuote(1_050_000), m)
	if repriced.raw != base.raw || base.raw != 500_000_000 {
		t.Fatalf("raw borrowing moved with price: %+v %+v", base, repriced)
	}
	// Interpolated by hand: 2,500 bps utilization between the 2,000 and 4,000
	// points.
	u := float64(base.raw) / m.DebtSupplyRaw * 10_000
	wantAPR := (800 + (3000-800)*(u-2000)/(4000-2000)) / 10_000.0
	if base.apr != wantAPR || repriced.apr != wantAPR {
		t.Fatalf("APR=%v/%v want raw-unit %v", base.apr, repriced.apr, wantAPR)
	}
	if buggy, err := projectedBorrowAPR(m, float64(repriced.debt)); err != nil || buggy == wantAPR {
		t.Fatalf("fixture insensitive to a USDC-valued APR: buggy=%v err=%v", buggy, err)
	}
	if repriced.debt <= base.debt || repriced.proceeds <= base.proceeds || base.debt != 500_000_000 || base.proceeds != 499_000_000 {
		t.Fatalf("priced borrow: base %d/%d repriced %d/%d", base.debt, base.proceeds, repriced.debt, repriced.proceeds)
	}
	for _, run := range []pricedRun{base, repriced} {
		assetValue := run.invested + float64(run.proceeds)
		interest := float64(run.debt) * math.Expm1(run.apr*run.years)
		want := assetValue*math.Expm1((math.Log1p(m.NativeAPY)+math.Log1p(m.SupplyAPY))*run.years) - interest - 10_000.0
		if run.gain != want {
			t.Fatalf("gain=%v want asset %v minus interest %v", run.gain, assetValue, interest)
		}
	}
}

// A levered quote's borrow is valued, within the equity it enters with, and
// converts no more principal than it owes; a 1x quote borrows nothing.
func TestQuoteBorrowIsValuedWithinEquity(t *testing.T) {
	t.Parallel()
	quote := autoMoveQuote(1_000_000)
	if !quote.validBorrow() {
		t.Fatal("valued quote rejected")
	}
	for name, change := range map[string]func(*MoveQuote){
		"unvalued":          func(q *MoveQuote) { q.BorrowValueRaw = 0 },
		"proceeds over owe": func(q *MoveQuote) { q.ProceedsValueRaw = q.BorrowValueRaw + 1 },
		"no borrow":         func(q *MoveQuote) { q.BorrowReceiveRaw = 0 },
		"inactive lane":     func(q *MoveQuote) { q.DestinationLane = ethenaUSDePYUSD.Lane },
		"1x with a borrow":  func(q *MoveQuote) { q.Unlevered = true },
	} {
		bad := quote
		change(&bad)
		if bad.validBorrow() {
			t.Fatalf("%s quote accepted", name)
		}
	}
	flat := quote
	flat.Unlevered, flat.BorrowReceiveRaw, flat.BorrowFeeRaw, flat.BorrowValueRaw, flat.ProceedsValueRaw = true, 0, 0, 0, 0
	if !flat.validBorrow() {
		t.Fatal("1x quote rejected")
	}
}

// The exit-only Ethena lane never reaches economics: its market is refused
// before any forecast, window or borrow projection.
func TestExitOnlyLaneNeverReachesFeedEconomics(t *testing.T) {
	t.Parallel()
	route, _ := runtimeRoute(ethenaUSDePYUSD.Lane)
	in := selectorFixture()
	in.Snapshot.Slot = 42
	in.Markets = []LaneEconomics{{Lane: route.Lane, EvidenceID: "rates", ObservedAt: in.Now, NativeObservedAt: in.Now, NativeAPY: .15, SupplyAPY: 0, CurrentBorrowAPY: .04, BorrowCurve: []BorrowCurvePoint{{0, 400}, {8000, 400}, {10000, 10000}}, DebtSupplyRaw: 1e15, DebtBorrowRaw: 1e14, EntryCapacity: Capacity{Known: true, Unlimited: true}}}
	in.Quotes = []MoveQuote{autoMoveQuote(1_000_000)}
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
	// An active lane with an executable quote keeps sampling its window.
	usdcSample := SelectOpportunity(selectorFixture(), SelectorState{})
	if _, sampled := usdcSample.State.Advantages["OnRe/ONyc/USDC"]; !sampled {
		t.Fatalf("active lane persistence regressed: %+v", usdcSample.State)
	}
}
