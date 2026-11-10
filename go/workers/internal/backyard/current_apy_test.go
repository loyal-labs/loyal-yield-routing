package backyard

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestCurrentAPYReportsLeverageDecisionChangesWithoutARateChange(t *testing.T) {
	now := time.Now()
	s := base()
	s.RouteLane, s.HasPosition, s.PositionCollateralValueRaw = autoAUTOPYUSD.Lane, true, 1_624e6
	o := Observation{Snapshot: s, planning: &routePlanningState{}}
	markets := []LaneEconomics{{Lane: s.RouteLane, NativeAPY: .1069}}
	var throttle currentAPYThrottle
	var writes []CurrentAPY
	write := func(_ context.Context, value CurrentAPY) error {
		writes = append(writes, value)
		return nil
	}
	logf := func(format string, args ...any) { t.Fatalf(format, args...) }
	decision := &LeverageDecisionSummary{TargetLevel: 1, Reason: leverageReasonNotWorthCost, NetBenefitRaw: -1_065_583, CostRaw: 8_425_697, HorizonDays: 90}
	publishCurrentAPY(t.Context(), now, &throttle, o, markets, decision, write, logf)
	if len(writes) != 1 || writes[0].LeverageDecision == nil || writes[0].LeverageDecision.Reason != leverageReasonNotWorthCost {
		t.Fatal("cost refusal missing from the published route", writes)
	}
	publishCurrentAPY(t.Context(), now.Add(time.Minute), &throttle, o, markets, decision, write, logf)
	if len(writes) != 1 {
		t.Fatal("unchanged decision bypassed the reporting throttle")
	}
	approved := *decision
	approved.TargetLevel, approved.Reason = 1.5, leverageReasonApproved
	publishCurrentAPY(t.Context(), now.Add(2*time.Minute), &throttle, o, markets, &approved, write, logf)
	if len(writes) != 2 || writes[1].LeverageDecision.Reason != leverageReasonApproved || writes[0].APYBPS != writes[1].APYBPS {
		t.Fatal("unchanged APY hid the new leverage approval", writes)
	}
	publishCurrentAPY(t.Context(), now.Add(3*time.Minute), &throttle, o, markets, nil, write, logf)
	if len(writes) != 3 || writes[2].LeverageDecision != nil {
		t.Fatal("unavailable evaluation retained the old decision", writes)
	}
	o.Snapshot.HasPosition = false
	publishCurrentAPY(t.Context(), now.Add(4*time.Minute), &throttle, o, markets, decision, write, logf)
	if len(writes) != 4 || !writes[3].Flat || writes[3].LeverageDecision != nil {
		t.Fatal("closed position retained a leverage decision", writes)
	}
}

// Live shape (09-29): AUTO collateral value $2,111.06, debt $904.01, equity
// $1,207.05 (1.749x). The published APY matches the leverage watch's
// "apy 1.75x=" figure for the same economics within rounding.
func TestCurrentAPYMatchesTheLeverageWatchFigure(t *testing.T) {
	t.Parallel()
	s := base()
	s.RouteLane, s.StrategyKey, s.HasPosition = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, true
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw = 2_112_337_500, 905_287_500
	apr := 0.068
	m := LaneEconomics{Lane: autoAUTOPYUSD.Lane, NativeAPY: .0948, SupplyAPY: .002, CurrentBorrowAPY: math.Expm1(apr),
		BorrowCurve: []BorrowCurvePoint{{0, apr * 10_000}, {10_000, apr * 10_000}}, DebtSupplyRaw: 1e15, DebtBorrowRaw: 1e14, DebtRawPerUSDCRaw: 1.0002}
	got, ok := currentPositionAPY(s, []LaneEconomics{m})
	if !ok || got.Level != 1.75 || got.Flat {
		t.Fatalf("%+v ok=%t", got, ok)
	}
	// The exact figure the watch summary prints for this lane at 1.75x.
	armLeverageCapacityFixture(&s)
	var watch leverageWatch
	lines := watch.observe([]LaneEconomics{m}, autoAUTOPYUSD.Lane, 1_207_050_000, true, rwa.AdminPerformanceFeeBPS, &s)
	want := fmt.Sprintf("1.75x=%.2f", float64(got.APYBPS)/100)
	found := false
	for _, line := range lines {
		found = found || strings.Contains(line, want)
	}
	if !found {
		t.Fatalf("published %d bps not in the watch line %q", got.APYBPS, lines)
	}
	// Debt-free: the 1x figure (native + supply).
	s.PositionDebtValueRaw = 0
	got, _ = currentPositionAPY(s, []LaneEconomics{m})
	if got.APYBPS != 767 || got.Level != 1 {
		t.Fatalf("debt-free %+v", got)
	}
}

func TestCurrentAPYWriteThrottle(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000_000, 0)
	var th currentAPYThrottle
	v := CurrentAPY{Lane: autoAUTOPYUSD.Lane, APYBPS: 1500}
	if !th.due(now, v) {
		t.Fatal("first write")
	}
	th.wrote(now, v)
	for _, c := range []struct {
		after time.Duration
		bps   int64
		flat  bool
		want  bool
	}{
		{time.Minute, 1504, false, false},     // < 5 bps
		{time.Minute, 1505, false, true},      // >= 5 bps
		{time.Minute, 1495, false, true},      // -5 bps
		{9 * time.Minute, 1500, false, false}, // same, < 10 min
		{10 * time.Minute, 1500, false, true}, // max age
		{time.Minute, 1500, true, true},       // became flat
	} {
		if got := th.due(now.Add(c.after), CurrentAPY{Lane: autoAUTOPYUSD.Lane, APYBPS: c.bps, Flat: c.flat}); got != c.want {
			t.Fatalf("%+v: due=%t", c, got)
		}
	}
	// An hour of 15 s samples with a stable APY writes at most 6 times.
	th = currentAPYThrottle{}
	writes := 0
	for i := 0; i < 240; i++ {
		at := now.Add(time.Duration(i) * 15 * time.Second)
		value := CurrentAPY{Lane: autoAUTOPYUSD.Lane, APYBPS: 1500 + int64(i%3)}
		if th.due(at, value) {
			th.wrote(at, value)
			writes++
		}
	}
	if writes > 7 {
		t.Fatalf("%d writes in an hour", writes)
	}
}

func TestLeverageWatchMergeKeepsMissingLanes(t *testing.T) {
	t.Parallel()
	old := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	now := old.Add(time.Hour)
	previous := &LeverageWatchSummary{ObservedAt: old, Lanes: []leverageWatchLane{
		{Lane: autoAUTOPYUSD.Lane, ObservedAt: old, SpreadBPS: watchSpreadFixture(100)},
		{Lane: onreONycUSDC, ObservedAt: old, SpreadBPS: watchSpreadFixture(200)},
	}}
	merged := mergeLeverageWatch(previous, LeverageWatchSummary{ObservedAt: now, Lanes: []leverageWatchLane{{Lane: autoAUTOPYUSD.Lane, ObservedAt: now, SpreadBPS: watchSpreadFixture(150)}}})
	if len(merged.Lanes) != 2 || !merged.ObservedAt.Equal(now) {
		t.Fatalf("%+v", merged)
	}
	for _, lane := range merged.Lanes {
		switch lane.Lane {
		case autoAUTOPYUSD.Lane:
			if (lane.SpreadBPS == nil || *lane.SpreadBPS != 150) || !lane.ObservedAt.Equal(now) {
				t.Fatalf("updated lane %+v", lane)
			}
		case onreONycUSDC:
			if (lane.SpreadBPS == nil || *lane.SpreadBPS != 200) || !lane.ObservedAt.Equal(old) {
				t.Fatalf("missing lane not kept with its older observedAt: %+v", lane)
			}
		}
	}
}

func watchSpreadFixture(n int64) *int64 { return &n }

func portfolioFixture() (Observation, []LaneEconomics) {
	lanes := []Snapshot{
		{RouteLane: autoAUTOPYUSD.Lane, PositionCollateralValueRaw: 1_500e6, PositionDebtValueRaw: 500e6},
		{RouteLane: MapleSyrupUSDCLaneID, PositionCollateralValueRaw: 100e6},
	}
	for i := range lanes {
		lanes[i].Slot, lanes[i].TotalVaultNAVRaw, lanes[i].VoltrIdleRaw = 77, 2_000e6, 900e6
		lanes[i].HeldLanes = autoAUTOPYUSD.Lane + "," + MapleSyrupUSDCLaneID
		lanes[i].net = fixtureSnapshotCertificate(lanes[i])
	}
	return Observation{Snapshot: lanes[1], lanes: lanes}, []LaneEconomics{
		{Lane: autoAUTOPYUSD.Lane, NativeAPY: .1, SupplyAPY: .01, CurrentBorrowAPY: .06},
		{Lane: MapleSyrupUSDCLaneID, NativeAPY: .05},
	}
}

func TestPortfolioAPYIncludesEveryPositionDebtAndIdleCash(t *testing.T) {
	o, markets := portfolioFixture()
	got := currentPortfolioAPY(o, markets, time.Now())
	// AUTO earns $135 after interest, Maple $5, idle $0: $140 / $2,000.
	if got == nil || got.APYBPS != 700 || got.IdleUSDCRaw != 900e6 || len(got.Positions) != 2 || *got.Positions[0].APYBPS != 1350 || *got.Positions[1].APYBPS != 500 {
		t.Fatalf("portfolio: %+v", got)
	}
	// A different working lane must not change the portfolio return.
	o.Snapshot = o.lanes[0]
	if p := currentPortfolioAPY(o, markets, time.Now()); p == nil || p.APYBPS != got.APYBPS {
		t.Fatal(p)
	}
	o.Snapshot.AdminPerformanceFeeBPS, o.Snapshot.ApprovedAdminPerformanceFeeBPS = 2000, 2000
	o.Snapshot.net = fixtureSnapshotCertificate(o.Snapshot)
	want := int64(math.Round(performanceFeeForecast(.07, 2000) * 10_000))
	if p := currentPortfolioAPY(o, markets, time.Now()); p == nil || p.APYBPS != want {
		t.Fatalf("fee: %+v want %d", p, want)
	}
}

func TestPortfolioAPYCountsSharedCollateralCustodyOnce(t *testing.T) {
	o, markets := portfolioFixture()
	o.lanes[1].RouteLane = autoAUTOWYLDSTemplate.Lane
	markets[1] = markets[0]
	markets[1].Lane = o.lanes[1].RouteLane
	for i := range o.lanes {
		o.lanes[i].HeldLanes = o.lanes[0].RouteLane + "," + o.lanes[1].RouteLane
		o.lanes[i].CollateralIdleValueRaw = 50e6
		o.lanes[i].net = fixtureSnapshotCertificate(o.lanes[i])
	}
	o.Snapshot = o.lanes[0]
	p := currentPortfolioAPY(o, markets, time.Now())
	// $135 + $11 from deposits, plus $5 native yield on the single $50 custody.
	if p == nil || p.APYBPS != 755 || p.Positions[0].CollateralValueRaw+p.Positions[1].CollateralValueRaw != 1_650e6 {
		t.Fatalf("shared custody: %+v", p)
	}
}

func TestPortfolioAPYRejectsIncompleteOrUnpricedHoldings(t *testing.T) {
	for name, mutate := range map[string]func(*Observation, *[]LaneEconomics){
		"missing lane":    func(o *Observation, _ *[]LaneEconomics) { o.lanes = o.lanes[1:] },
		"duplicate lane":  func(o *Observation, _ *[]LaneEconomics) { o.lanes = append(o.lanes, o.lanes[0]) },
		"missing rate":    func(_ *Observation, m *[]LaneEconomics) { *m = (*m)[1:] },
		"invalid rate":    func(_ *Observation, m *[]LaneEconomics) { (*m)[0].CurrentBorrowAPY = math.NaN() },
		"invalid value":   func(o *Observation, _ *[]LaneEconomics) { o.lanes[0].PositionDebtValueRaw = -1 },
		"different bank":  func(o *Observation, _ *[]LaneEconomics) { o.lanes[0].Slot++ },
		"stale valuation": func(o *Observation, _ *[]LaneEconomics) { o.lanes[0].net.validUntil = time.Now().Add(-time.Second) },
		"unsupported fees": func(o *Observation, _ *[]LaneEconomics) {
			o.Snapshot.ManagerManagementFeeBPS = 100
			o.Snapshot.net = fixtureSnapshotCertificate(o.Snapshot)
		},
	} {
		t.Run(name, func(t *testing.T) {
			o, markets := portfolioFixture()
			mutate(&o, &markets)
			if got := currentPortfolioAPY(o, markets, time.Now()); got != nil {
				t.Fatalf("misleading portfolio: %+v", got)
			}
		})
	}
	// An entirely idle, observed vault has a known 0% return without market rates.
	s := Snapshot{RouteLane: autoAUTOPYUSD.Lane, Slot: 77, TotalVaultNAVRaw: 2_000e6, VoltrIdleRaw: 2_000e6}
	s.net = fixtureSnapshotCertificate(s)
	if p := currentPortfolioAPY(Observation{Snapshot: s, lanes: []Snapshot{s}}, nil, time.Now()); p == nil || p.APYBPS != 0 || len(p.Positions) != 0 {
		t.Fatal(p)
	}
}

func TestPortfolioAPYThrottlePublishesAllocationsAndInvalidation(t *testing.T) {
	o, markets := portfolioFixture()
	now := time.Now()
	v := CurrentAPY{Portfolio: currentPortfolioAPY(o, markets, now)}
	var th currentAPYThrottle
	th.wrote(now, v)
	if th.due(now.Add(time.Minute), v) {
		t.Fatal("unchanged portfolio rewritten")
	}
	next := v
	p := *v.Portfolio
	p.IdleUSDCRaw -= 1_000e6
	next.Portfolio = &p
	if !th.due(now.Add(time.Minute), next) {
		t.Fatal("allocation not published")
	}
	next.Portfolio = nil
	if !th.due(now.Add(time.Minute), next) {
		t.Fatal("unavailable portfolio retained")
	}
}
