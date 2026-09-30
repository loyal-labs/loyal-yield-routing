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
	if got.APYBPS != int64(math.Round((m.NativeAPY+m.SupplyAPY)*10_000)) || got.Level != 1 {
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
