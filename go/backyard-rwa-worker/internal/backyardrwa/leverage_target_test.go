package backyardrwa

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

// A flat borrow curve at the given APR and a native yield; spread = yield -
// expm1(apr).
func leverageMarket(lane string, yield, borrowAPR float64) LaneEconomics {
	bps := borrowAPR * 10_000
	return LaneEconomics{Lane: lane, NativeAPY: yield, BorrowCurve: []BorrowCurvePoint{{0, bps}, {10_000, bps}}, DebtSupplyRaw: 1e15, DebtBorrowRaw: 1e14}
}

// A settled funded AUTO position at the given level, equity ~$1,000.
func leverageSnapshot(level float64) Snapshot {
	s := base()
	s.RouteLane, s.StrategyKey, s.PilotActive = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, true
	s.HasPosition = true
	s.PositionCollateralValueRaw = int64(1_000_000_000 * level)
	s.PositionCollateralRaw = s.PositionCollateralValueRaw
	s.PositionDebtValueRaw = s.PositionCollateralValueRaw - 1_000_000_000
	s.PositionDebtRaw = s.PositionDebtValueRaw
	s.LTVBPS = leverageLevelLTVBPS(level)
	s.LeverageTargetLevel = level
	return s
}

func TestDecideLeverageTargetStepsAtEachLevel(t *testing.T) {
	keep := SelectorResult{Action: "KEEP"}
	p := DefaultSelectorPolicy()
	for _, tc := range []struct {
		name       string
		level      float64
		yield, apr float64
		want       float64
		wantReason string
	}{
		{"1x stays below 1 pt", 1, 0.10, math.Log1p(0.095), 1, "spread_rule"},
		{"1x up at >= 1 pt", 1, 0.12, math.Log1p(0.06), 1.5, "spread_rule"},
		{"1.5x stays 1-2 pt", 1.5, 0.10, math.Log1p(0.085), 1.5, "spread_rule"},
		// Live levels reach 1.75x with the multi-cycle exit; option 1 exactly.
		{"1.5x up at >= 2 pt", 1.5, 0.12, math.Log1p(0.06), 1.75, "spread_rule"},
		{"1.5x down below 0", 1.5, 0.05, math.Log1p(0.06), 1, "spread_rule"},
		{"1.75x stays in the gap", 1.75, 0.10, math.Log1p(0.085), 1.75, "spread_rule"},
		{"1.75x down below 1 pt", 1.75, 0.10, math.Log1p(0.095), 1.5, "spread_rule"},
		{"1.75x never above", 1.75, 0.20, math.Log1p(0.01), 1.75, "spread_rule"},
	} {
		s := leverageSnapshot(tc.level)
		got, ok := decideLeverageTarget(s, keep, []LaneEconomics{leverageMarket(s.RouteLane, tc.yield, tc.apr)}, p)
		if !ok || got.Current != min(tc.level, leverageMaxLiveLevel) || got.Next != tc.want || got.Reason != tc.wantReason {
			t.Fatalf("%s: %+v ok=%t", tc.name, got, ok)
		}
	}
	// Up needs the move to beat MinimumBenefit plus cost: a tiny position
	// with a qualifying spread stays put, and the numbers are logged.
	small := leverageSnapshot(1)
	small.PositionCollateralValueRaw, small.PositionCollateralRaw = 2_000_000, 2_000_000
	got, _ := decideLeverageTarget(small, keep, []LaneEconomics{leverageMarket(small.RouteLane, 0.12, math.Log1p(0.06))}, p)
	if got.Next != 1 || got.Reason != "up_move_below_minimum_benefit" || got.GainRaw <= 0 || got.CostRaw <= 0 ||
		!strings.Contains(got.logLine(), "gain=") || !strings.Contains(got.logLine(), "cost=") {
		t.Fatalf("small up move not refused or not logged: %+v %s", got, got.logLine())
	}
}

// The level decision never runs beside a selector move or any unfinished
// work, and a selector move never runs beside a pending level move: the
// selector freezes while the tranche is in progress.
func TestLeverageDecisionAndSelectorMovesExcludeEachOther(t *testing.T) {
	markets := []LaneEconomics{leverageMarket(autoAUTOPYUSD.Lane, 0.12, math.Log1p(0.06))}
	p := DefaultSelectorPolicy()
	s := leverageSnapshot(1.5)
	if _, ok := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, markets, p); !ok {
		t.Fatal("settled KEEP position gave no leverage decision")
	}
	for name, c := range map[string]struct {
		result SelectorResult
		mutate func(*Snapshot)
	}{
		"selector switch":    {SelectorResult{Action: "SWITCH"}, func(*Snapshot) {}},
		"selector enter":     {SelectorResult{Action: "ENTER"}, func(*Snapshot) {}},
		"unwind in progress": {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.Unwind = true }},
		"unwind refresh":     {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.UnwindRefreshRequired = true }},
		"nonterminal":        {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.Nonterminal = Signed }},
		"withdrawal":         {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.WithdrawalDemandRaw = 1 }},
		"idle squads cash":   {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.SquadsIdleRaw = 1 }},
		"idle debt cash":     {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.DebtIdleRaw = 1 }},
		"maple":              {SelectorResult{Action: "KEEP"}, func(s *Snapshot) { s.RouteLane, s.StrategyKey = SelectedRouteID, SelectedRouteID }},
	} {
		c2 := s
		c.mutate(&c2)
		if _, ok := decideLeverageTarget(c2, c.result, markets, p); ok {
			t.Fatalf("%s: leverage decision ran", name)
		}
	}
	// A debt-free position without a 1x target is a pending first loop: the
	// selector stays frozen, so no SWITCH can start in the middle of it.
	pending := leverageSnapshot(1)
	pending.LeverageTargetLevel = 1.5
	if !selectorTrancheInProgress(pending) {
		t.Fatal("pending level move did not freeze the selector")
	}
	pending.LeverageTargetLevel = 1
	if selectorTrancheInProgress(pending) {
		t.Fatal("finished 1x froze the selector")
	}
}

func TestLeverageTargetStateRoundTrip(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	target := LeverageTarget{Lane: onreONycUSDC, Level: 1.75, SpreadBPS: 250, DecidedAt: now}
	raw := []byte(`{"lane":"OnRe/ONyc/USDC","level":1.75,"spreadBps":250,"decidedAt":"2033-05-18T03:33:20Z"}`)
	got, err := decodeLeverageTarget(raw)
	if err != nil || got == nil || *got != target {
		t.Fatalf("decode: %+v %v", got, err)
	}
	for _, bad := range []string{`{"lane":"OnRe/ONyc/USDC","level":2,"decidedAt":"2033-05-18T03:33:20Z"}`, `{"lane":"Maple/syrupUSDC/USDC","level":1.5,"decidedAt":"2033-05-18T03:33:20Z"}`, `{"lane":"OnRe/ONyc/USDC","level":1.5}`, `{`} {
		if _, err := decodeLeverageTarget([]byte(bad)); err == nil {
			t.Fatalf("invalid target accepted: %s", bad)
		}
	}
	if got, err := decodeLeverageTarget([]byte("null")); got != nil || err != nil {
		t.Fatal("null target")
	}
	s := base()
	s.RouteLane = onreONycUSDC
	applyLeverageTarget(&s, &target)
	if s.LeverageTargetLevel != 1.75 {
		t.Fatal("target not applied to its lane")
	}
	s.RouteLane = autoAUTOPYUSD.Lane
	applyLeverageTarget(&s, &target)
	if s.LeverageTargetLevel != 0 {
		t.Fatal("target applied to another lane")
	}
}

func TestBorrowBlockedHoldLogsOncePerHour(t *testing.T) {
	var out bytes.Buffer
	l := borrowBlockedLog{out: &out}
	now := time.Unix(1_000, 0)
	hold := Decision{Action: Hold, Reason: "debt_reserve_utilization_blocks_borrow", StrategyKey: autoAUTOPYUSD.Lane}
	for i := 0; i < 240; i++ { // one hour of 15 s ticks
		l.note(now.Add(time.Duration(i)*15*time.Second), hold)
	}
	l.note(now.Add(61*time.Minute), hold)
	l.note(now.Add(62*time.Minute), Decision{Action: Hold, Reason: "other"})
	if n := strings.Count(out.String(), "borrowing blocked"); n != 2 {
		t.Fatalf("want 2 lines in 61 minutes, got %d", n)
	}
}

func TestLeverageDecisionLogPrintsOnChangeOrHourly(t *testing.T) {
	l := &leverageDecisionLog{}
	now := time.Unix(10_000, 0)
	up := leverageDecision{Current: 1, Next: 1.5}
	if !l.due(now, up, 0) {
		t.Fatal("a new target was not logged")
	}
	lines := 0
	for i := 1; i < 240; i++ { // one hour of 15 s samples, target stored, borrowing blocked
		if l.due(now.Add(time.Duration(i)*15*time.Second), up, 1.5) {
			lines++
		}
	}
	if lines != 0 {
		t.Fatalf("unchanged target logged %d times within the hour", lines)
	}
	if !l.due(now.Add(time.Hour), up, 1.5) {
		t.Fatal("hourly reminder missing")
	}
	if !l.due(now.Add(time.Hour+time.Second), leverageDecision{Current: 1.5, Next: 1}, 1.5) {
		t.Fatal("a target change was rate-limited")
	}
	if l.due(now.Add(3*time.Hour), leverageDecision{Current: 1.5, Next: 1.5}, 1.5) {
		t.Fatal("a hold decision was logged")
	}
}

// Live 2026-09-28: position at 1x (borrowing blocked), stored target 1.5x.
// When spread(1.5x) was unavailable (our borrow did not fit the pool's free
// liquidity) the rule fell back to "stay at 1x" and stored 1x; the next
// sample stored 1.5x again. Now: unavailable spread -> no decision, no write;
// available again -> the stored 1.5x stands (no flip).
func TestUnavailableSpreadNeverFlipsTheStoredTarget(t *testing.T) {
	keep := SelectorResult{Action: "KEEP"}
	p := DefaultSelectorPolicy()
	s := leverageSnapshot(1) // debt-free, ~$1,000 equity
	s.LeverageTargetLevel = 1.5
	good := leverageMarket(s.RouteLane, 0.12, math.Log1p(0.06))
	full := good
	full.DebtSupplyRaw, full.DebtBorrowRaw = 1_000_000_000, 900_000_000 // $100 free < $500 borrow
	if _, ok := leverageSpread(full, 1.5, 1_000_000_000, false); ok {
		t.Fatal("fixture: spread(1.5x) should be unavailable")
	}
	if d, ok := decideLeverageTarget(s, keep, []LaneEconomics{full}, p); ok {
		t.Fatalf("unavailable spread still decided: %+v", d)
	}
	// Stored 1.5x with a 2+ pt spread: the only change is the next step up.
	d, ok := decideLeverageTarget(s, keep, []LaneEconomics{good}, p)
	if !ok || (d.Next != 1.5 && d.Next != 1.75) || (d.Next == 1.5 && d.changesTarget(s.LeverageTargetLevel)) {
		t.Fatalf("available again: %+v ok=%t changes=%t", d, ok, d.changesTarget(s.LeverageTargetLevel))
	}
	// A dip in gain below the up gate does not undo a stored target either:
	// the rule steps from the stored level (down from 1.5x only below 0).
	weak := leverageMarket(s.RouteLane, 0.10, math.Log1p(0.095))
	if d, ok := decideLeverageTarget(s, keep, []LaneEconomics{weak}, p); !ok || d.changesTarget(1.5) {
		t.Fatalf("weak spread flipped the stored 1.5x: %+v", d)
	}
	neg := leverageMarket(s.RouteLane, 0.05, math.Log1p(0.06))
	if d, ok := decideLeverageTarget(s, keep, []LaneEconomics{neg}, p); !ok || d.Next != 1 || !d.changesTarget(1.5) {
		t.Fatalf("negative spread did not step down: %+v", d)
	}
	if (leverageDecision{Current: 1.75, Next: 1.75}).changesTarget(1.75) {
		t.Fatal("stored 1.75x rewritten")
	}
	if !(leverageDecision{Current: 1, Next: 1.5}).changesTarget(0) {
		t.Fatal("no stored target: first decision not written")
	}
}
