package backyard

import (
	"context"
	"testing"
	"time"
)

// stubLeverageJournal is the production journal surface a construction
// refresh sees: no planning state rides the refreshed observation, so every
// durable planning input must come from these readers.
type stubLeverageJournal struct {
	stubProductionJournal
	target *LeverageTarget
}

func (s *stubLeverageJournal) LoadLeverageTarget(context.Context, string) (*LeverageTarget, error) {
	return s.target, nil
}

// Live 2026-09-29: the tick decided leverage_up from the planning-state
// observation (stored target 1.5x); the construction refresh re-merged the
// journal without planning, lost the target (0 -> Hold
// leverage_target_required), and every tick failed decisionsEqual.
func TestConstructionRefreshKeepsTheStoredLeverageTarget(t *testing.T) {
	t.Parallel()
	target := &LeverageTarget{Lane: autoAUTOPYUSD.Lane, Level: 1.5, BorrowRaw: 499_500_000, SpreadBPS: 300, DecidedAt: time.Now().UTC()}
	journal := &stubLeverageJournal{target: target}
	state := productionObserveState{manifest: readyWorkerManifest(t), routeKey: productionRouteKey, journal: journal, identity: pinnedIdentityObservation}
	refreshed := Observation{Snapshot: leverageSnapshot(1)} // no planning: the refresh path
	refreshed.Snapshot.LeverageTargetLevel = 0
	if err := state.mergeJournal(context.Background(), &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.Snapshot.LeverageTargetLevel != 1.5 {
		t.Fatalf("refresh lost the stored target: %v", refreshed.Snapshot.LeverageTargetLevel)
	}
	// The outer (planning) observation and the refresh now decide the same.
	outer := Observation{Snapshot: leverageSnapshot(1), planning: &routePlanningState{generation: 1, leverage: target}}
	outer.Snapshot.LeverageTargetLevel = 0
	if err := state.mergeJournal(context.Background(), &outer); err != nil {
		t.Fatal(err)
	}
	for _, o := range []*Observation{&outer, &refreshed} {
		o.Snapshot.SelectorEntryPaused = false
	}
	a, b := Decide(outer.Snapshot), Decide(refreshed.Snapshot)
	if a.Reason != leverageUpReason || !decisionsEqual(a, b) {
		t.Fatalf("outer %+v refreshed %+v", a, b)
	}
}
