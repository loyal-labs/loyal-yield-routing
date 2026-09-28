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
		{"1.5x up at >= 2 pt", 1.5, 0.12, math.Log1p(0.06), 1.75, "spread_rule"},
		{"1.5x down below 0", 1.5, 0.05, math.Log1p(0.06), 1, "spread_rule"},
		{"1.75x stays in the gap", 1.75, 0.10, math.Log1p(0.085), 1.75, "spread_rule"},
		{"1.75x down below 1 pt", 1.75, 0.10, math.Log1p(0.095), 1.5, "spread_rule"},
		{"1.75x never above", 1.75, 0.20, math.Log1p(0.01), 1.75, "spread_rule"},
	} {
		s := leverageSnapshot(tc.level)
		got, ok := decideLeverageTarget(s, keep, []LaneEconomics{leverageMarket(s.RouteLane, tc.yield, tc.apr)}, p)
		if !ok || got.Current != tc.level || got.Next != tc.want || got.Reason != tc.wantReason {
			t.Fatalf("%s: %+v ok=%t", tc.name, got, ok)
		}
	}
	// Up needs the move to beat MinimumBenefit plus cost: a tiny position
	// with a qualifying spread stays put, and the numbers are logged.
	small := leverageSnapshot(1.5)
	small.PositionCollateralValueRaw, small.PositionDebtValueRaw = 3_000_000, 1_000_000
	got, _ := decideLeverageTarget(small, keep, []LaneEconomics{leverageMarket(small.RouteLane, 0.12, math.Log1p(0.06))}, p)
	if got.Next != 1.5 || got.Reason != "up_move_below_minimum_benefit" || got.GainRaw <= 0 || got.CostRaw <= 0 ||
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
