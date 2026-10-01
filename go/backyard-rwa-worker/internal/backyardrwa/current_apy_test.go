package backyardrwa

import (
	"context"
	"encoding/json"
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
	s := base()
	s.RouteLane, s.StrategyKey, s.HasPosition = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, true
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw = 2_111_060_000, 904_010_000
	apr := 0.068
	m := LaneEconomics{Lane: autoAUTOPYUSD.Lane, NativeAPY: .0948, SupplyAPY: .002, CurrentBorrowAPY: math.Expm1(apr),
		BorrowCurve: []BorrowCurvePoint{{0, apr * 10_000}, {10_000, apr * 10_000}}, DebtSupplyRaw: 1e15, DebtBorrowRaw: 1e14}
	got, ok := currentPositionAPY(s, []LaneEconomics{m})
	if !ok || got.Level != 1.75 || got.Flat {
		t.Fatalf("%+v ok=%t", got, ok)
	}
	// The exact figure the watch summary prints for this lane at 1.75x.
	var watch leverageWatch
	lines := watch.observe([]LaneEconomics{m}, autoAUTOPYUSD.Lane, 1_207_050_000, true, func(string) bool { return true })
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
	// Flat: nothing computed, flat flag set.
	flat := base()
	flat.RouteLane = autoAUTOPYUSD.Lane
	if got, ok := currentPositionAPY(flat, []LaneEconomics{m}); ok || !got.Flat {
		t.Fatalf("flat %+v", got)
	}
}

func TestCurrentAPYWriteThrottle(t *testing.T) {
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

// A refused write (lost fence, newer state) only logs: publishCurrentAPY
// returns nothing, keeps the throttle unwritten, and retries next sample.
func TestCurrentAPYWriteFailureOnlyLogs(t *testing.T) {
	s := leverageSnapshot(1.5)
	o := Observation{Snapshot: s, planning: &routePlanningState{generation: 7}}
	m := LaneEconomics{Lane: s.RouteLane, NativeAPY: .1, CurrentBorrowAPY: .05, BorrowCurve: []BorrowCurvePoint{{0, 500}, {10_000, 500}}, DebtSupplyRaw: 1e15, DebtBorrowRaw: 1e14}
	var th currentAPYThrottle
	logged := 0
	publishCurrentAPY(context.Background(), time.Now(), &th, o, []LaneEconomics{m}, func(context.Context, CurrentAPY, int64) error {
		return budgetHold("current_apy_state_changed")
	}, func(string, ...any) { logged++ })
	if logged != 1 || !th.written.IsZero() {
		t.Fatalf("logged=%d written=%v", logged, th.written)
	}
	var stored CurrentAPY
	var version int64
	publishCurrentAPY(context.Background(), time.Now(), &th, o, []LaneEconomics{m}, func(_ context.Context, v CurrentAPY, gen int64) error {
		stored, version = v, gen
		return nil
	}, func(string, ...any) { t.Fatal("unexpected log") })
	if version != 7 || stored.APYBPS == 0 || stored.Level != 1.5 || th.written.IsZero() {
		t.Fatalf("stored %+v version %d", stored, version)
	}
	// No planning state (shadow observation): nothing is written.
	publishCurrentAPY(context.Background(), time.Now(), &currentAPYThrottle{}, Observation{Snapshot: s}, []LaneEconomics{m}, func(context.Context, CurrentAPY, int64) error {
		t.Fatal("wrote without a planning generation")
		return nil
	}, func(string, ...any) {})
}

// The fenced write against Postgres: stored under state.currentApy without
// advancing the version; a stale version or a lost lease refuses; flat keeps
// the last APY and sets flat=true.
func TestRecordCurrentAPYFencedWrite(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("current-apy-%d", time.Now().UnixNano())
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":1}',1)`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "apy-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 29, 21, 0, 0, 0, time.UTC)
	value := CurrentAPY{Lane: autoAUTOPYUSD.Lane, APYBPS: 1523, Level: 1.75, ObservedAt: now}
	if err := db.RecordCurrentAPY(ctx, key, value, 1); err != nil {
		t.Fatal(err)
	}
	read := func() (CurrentAPY, int64) {
		var raw []byte
		var version int64
		if err := db.pool.QueryRow(ctx, `SELECT state->'currentApy',state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&raw, &version); err != nil {
			t.Fatal(err)
		}
		var got CurrentAPY
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got, version
	}
	if got, version := read(); got != value || version != 1 {
		t.Fatalf("stored %+v version %d", got, version)
	}
	// Stale version: refused, nothing changed.
	if err := db.RecordCurrentAPY(ctx, key, CurrentAPY{Lane: value.Lane, APYBPS: 1, ObservedAt: now}, 0); err == nil {
		t.Fatal("stale version accepted")
	}
	// Flat: last APY kept, flat set.
	if err := db.RecordCurrentAPY(ctx, key, CurrentAPY{Lane: value.Lane, Flat: true, ObservedAt: now.Add(time.Minute)}, 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := read(); !got.Flat || got.APYBPS != 1523 || !got.ObservedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("flat write %+v", got)
	}
	// Lost lease: refused.
	db.setLease(nil)
	if err := db.RecordCurrentAPY(ctx, key, value, 1); err == nil {
		t.Fatal("write without a lease accepted")
	}
}

// The stored summary is the printed line's numbers: the fixture line is
// rebuilt from the stored lanes and must match exactly.
func TestLeverageWatchSummaryEqualsThePrintedLine(t *testing.T) {
	apr := 0.068
	auto := LaneEconomics{Lane: autoAUTOPYUSD.Lane, NativeAPY: .0948, SupplyAPY: .002, BorrowCurve: []BorrowCurvePoint{{0, apr * 10_000}, {10_000, apr * 10_000}}, DebtSupplyRaw: 1e15, DebtBorrowRaw: 1e14}
	onre := auto
	onre.Lane, onre.NativeAPY = onreONycUSDC, .1102
	var watch leverageWatch
	lines := watch.observe([]LaneEconomics{onre, auto}, autoAUTOPYUSD.Lane, 1_207_050_000, true, func(l string) bool { return l == onreONycUSDC })
	summary := lines[len(lines)-1]
	if len(watch.summary) != 2 {
		t.Fatalf("summary lanes %d", len(watch.summary))
	}
	for _, lane := range watch.summary {
		parts := []string{}
		for _, level := range leverageWatchLevels {
			key := fmt.Sprintf("%.2f", level)
			if bps, ok := lane.APYBPS[key]; ok {
				parts = append(parts, fmt.Sprintf("%.2fx=%.2f", level, float64(bps)/100))
			}
		}
		entry := "no"
		if lane.Enterable {
			entry = "yes"
		}
		rebuilt := fmt.Sprintf("%s(spread=%.2f enterable=%s apy %s levels 1/2/3=%.2f/%.2f/%.2f)", lane.Lane, float64(lane.SpreadBPS)/100, entry, strings.Join(parts, " "), lane.Levels[0], lane.Levels[1], lane.Levels[2])
		if !strings.Contains(summary, rebuilt) {
			t.Fatalf("stored lane %q not in the printed line %q", rebuilt, summary)
		}
	}
	// A non-summary observe clears it: nothing is stored between summaries.
	watch.observe([]LaneEconomics{auto}, autoAUTOPYUSD.Lane, 1_207_050_000, false, func(string) bool { return false })
	if watch.summary != nil {
		t.Fatal("stale summary kept")
	}
}

func TestLeverageWatchMergeKeepsMissingLanes(t *testing.T) {
	old := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	now := old.Add(time.Hour)
	previous := &LeverageWatchSummary{ObservedAt: old, Lanes: []leverageWatchLane{
		{Lane: autoAUTOPYUSD.Lane, ObservedAt: old, SpreadBPS: 100},
		{Lane: onreONycUSDC, ObservedAt: old, SpreadBPS: 200},
	}}
	merged := mergeLeverageWatch(previous, LeverageWatchSummary{ObservedAt: now, Lanes: []leverageWatchLane{{Lane: autoAUTOPYUSD.Lane, ObservedAt: now, SpreadBPS: 150}}})
	if len(merged.Lanes) != 2 || !merged.ObservedAt.Equal(now) {
		t.Fatalf("%+v", merged)
	}
	for _, lane := range merged.Lanes {
		switch lane.Lane {
		case autoAUTOPYUSD.Lane:
			if lane.SpreadBPS != 150 || !lane.ObservedAt.Equal(now) {
				t.Fatalf("updated lane %+v", lane)
			}
		case onreONycUSDC:
			if lane.SpreadBPS != 200 || !lane.ObservedAt.Equal(old) {
				t.Fatalf("missing lane not kept with its older observedAt: %+v", lane)
			}
		}
	}
}

func TestLeverageWatchWriteFailureOnlyLogs(t *testing.T) {
	watch := &leverageWatch{summary: []leverageWatchLane{{Lane: autoAUTOPYUSD.Lane}}}
	o := Observation{planning: &routePlanningState{generation: 3}}
	logged := 0
	publishLeverageWatch(context.Background(), time.Now(), watch, o, func(context.Context, LeverageWatchSummary, int64) error {
		return budgetHold("leverage_watch_state_changed")
	}, func(string, ...any) { logged++ })
	if logged != 1 {
		t.Fatal("refused write not logged")
	}
	publishLeverageWatch(context.Background(), time.Now(), watch, Observation{}, func(context.Context, LeverageWatchSummary, int64) error {
		t.Fatal("wrote without a planning generation")
		return nil
	}, func(string, ...any) {})
}

// Against Postgres: stored and merged per lane, fenced, no version bump.
func TestRecordLeverageWatchFencedWrite(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	key := fmt.Sprintf("leverage-watch-%d", time.Now().UnixNano())
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,'{"generation":1}',1)`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, key, "watch-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	both := LeverageWatchSummary{ObservedAt: old, Lanes: []leverageWatchLane{
		{Lane: autoAUTOPYUSD.Lane, ObservedAt: old, SpreadBPS: 100, APYBPS: map[string]int64{"1.00": 968}, Levels: []float64{1.5, 1.5, 1.5}},
		{Lane: onreONycUSDC, ObservedAt: old, SpreadBPS: 200, APYBPS: map[string]int64{"1.00": 1102}, Levels: []float64{1, 1, 1}},
	}}
	if err := db.RecordLeverageWatch(ctx, key, both, 1); err != nil {
		t.Fatal(err)
	}
	now := old.Add(time.Hour)
	if err := db.RecordLeverageWatch(ctx, key, LeverageWatchSummary{ObservedAt: now, Lanes: []leverageWatchLane{{Lane: autoAUTOPYUSD.Lane, ObservedAt: now, SpreadBPS: 150}}}, 1); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	var version int64
	if err := db.pool.QueryRow(ctx, `SELECT state->'leverageWatch',state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&raw, &version); err != nil {
		t.Fatal(err)
	}
	var stored LeverageWatchSummary
	if err := json.Unmarshal(raw, &stored); err != nil || version != 1 || len(stored.Lanes) != 2 || !stored.ObservedAt.Equal(now) {
		t.Fatalf("stored %s version %d err %v", raw, version, err)
	}
	if err := db.RecordLeverageWatch(ctx, key, both, 0); err == nil {
		t.Fatal("stale version accepted")
	}
	db.setLease(nil)
	if err := db.RecordLeverageWatch(ctx, key, both, 1); err == nil {
		t.Fatal("write without a lease accepted")
	}
}
