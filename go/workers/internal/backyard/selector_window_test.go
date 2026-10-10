package backyard

import (
	"testing"
	"time"
)

// A PYUSD-debt AUTO candidate persists only from its priced quote. One
// sample without a quote (fee read failed, timeout) must not restart the
// 30-minute window; a longer outage (> MaxSampleGap) or an unprofitable
// priced sample still does.
func autoWindowFixture() SelectorInput {
	in := unleveredSwitchFixtureAt(3_000_000, .16)
	// Flat vault with idle cash choosing a destination (ENTER).
	s := &in.Snapshot
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.StrategyNAVRaw = false, 0, 0, 0
	s.VoltrIdleRaw, s.TotalVaultNAVRaw = 1_677_000_000, 1_677_000_000
	in.Quotes[0].SourceLane, in.Quotes[0].SourceExit = s.RouteLane, nil
	in.Markets = []LaneEconomics{in.Markets[1]}
	return in
}

func sampleWindow(t *testing.T, in SelectorInput, state SelectorState, available bool) SelectorResult {
	t.Helper()
	in.Markets = append([]LaneEconomics(nil), in.Markets...)
	if !available {
		in.Quotes = nil
		in.Markets[0].EntryCapacity = Capacity{Known: true}
		in.Markets[0].EntryBlockedReason = "complete_entry_quote_unavailable"
	}
	return selectUnlevered(in, state)
}

func TestAdvantageWindowSurvivesQuoteOutages(t *testing.T) {
	key := unleveredAdvantageKey(onreONycUSDC)
	// (a) profitable, unavailable, profitable (gaps < 10 min): Since kept,
	// ENTER once 30 minutes of the window have passed.
	in := autoWindowFixture()
	first := sampleWindow(t, in, SelectorState{}, true)
	since := first.State.Advantages[key].Since
	if since.IsZero() {
		t.Fatal("window not started")
	}
	state := first.State
	var last SelectorResult
	for i := 1; i <= 6; i++ {
		advanceSelectorFixture(&in, 5*time.Minute)
		last = sampleWindow(t, in, state, i != 2 && i != 4)
		if w := last.State.Advantages[key]; w.Since != since {
			t.Fatalf("sample %d: window restarted (%v -> %v)", i, since, w.Since)
		}
		state = last.State
	}
	if last.Action != "ENTER" {
		t.Fatalf("no ENTER after 30 min: %s %s %+v", last.Action, last.Reason, last.Candidates)
	}
	// (b) unavailable longer than MaxSampleGap: the next priced sample restarts.
	in = autoWindowFixture()
	state = sampleWindow(t, in, SelectorState{}, true).State
	for i := 0; i < 3; i++ {
		advanceSelectorFixture(&in, 5*time.Minute)
		state = sampleWindow(t, in, state, false).State
	}
	advanceSelectorFixture(&in, 5*time.Minute)
	if w := sampleWindow(t, in, state, true).State.Advantages[key]; w.Since != in.Now {
		t.Fatalf("outage > MaxSampleGap kept the window: %v", w.Since)
	}
	// (c) priced but not profitable: the window drops.
	in = autoWindowFixture()
	state = sampleWindow(t, in, SelectorState{}, true).State
	advanceSelectorFixture(&in, 5*time.Minute)
	poor := in
	poor.Quotes = append([]MoveQuote(nil), in.Quotes...)
	poor.Quotes[0].CostRaw = 1_600_000_000
	if _, ok := sampleWindow(t, poor, state, true).State.Advantages[key]; ok {
		t.Fatal("unprofitable sample kept the window")
	}
	// (d) the live 09-29 sequence (samples every ~2.5 min; quote failures at
	// 18:46:38, 18:52:23, 18:55:56): ENTER after 30 min of the window.
	in = autoWindowFixture()
	start := in.Now
	state = SelectorState{}
	failures := map[int]bool{4: true, 6: true, 7: true} // minutes 10, 15, 17.5 after start
	entered := time.Duration(0)
	for i := 0; i <= 14; i++ {
		if i > 0 {
			advanceSelectorFixture(&in, 150*time.Second)
		}
		r := sampleWindow(t, in, state, !failures[i])
		state = r.State
		if r.Action == "ENTER" {
			entered = in.Now.Sub(start)
			break
		}
	}
	if entered == 0 || entered > 35*time.Minute {
		t.Fatalf("live sequence never entered (after %v)", entered)
	}
	t.Logf("live sequence: ENTER after %v", entered)
}
