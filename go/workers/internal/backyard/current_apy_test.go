package backyard

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

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
