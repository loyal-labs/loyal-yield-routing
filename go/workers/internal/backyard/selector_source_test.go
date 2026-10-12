package backyard

import (
	"context"
	"testing"
)

func TestSelectorSourceIdleNeedsNoExitAndPendingWorkPrecedesEconomics(t *testing.T) {
	t.Parallel()
	m, rpc, client, _ := selectorDestinationFixture(t)
	s := Snapshot{Fresh: true, Slot: 42, ObservationID: "idle", RouteLane: SelectedRouteID, StrategyKey: SelectedRouteID, VoltrIdleRaw: 10_000_000}
	q, err := observeSelectorSource(context.Background(), rpc, fixtureView(t, rpc), client, m, tickObservation(s))
	if err != nil || q.MinimumIdleRaw != 10_000_000 || q.Recipe.CostRaw != 0 || len(q.Recipe.Inputs) != 0 {
		t.Fatal(q, err)
	}
	// Squads cash an entry already allocated is the same flat vault's cash.
	s.VoltrIdleRaw, s.SquadsIdleRaw = 4_000_000, 6_000_000
	if q, err = observeSelectorSource(context.Background(), rpc, fixtureView(t, rpc), client, m, tickObservation(s)); err != nil || q.MinimumIdleRaw != 10_000_000 {
		t.Fatal(q, err)
	}
	for _, change := range []func(*Snapshot){func(s *Snapshot) { s.Nonterminal = Signed }, func(s *Snapshot) { s.WithdrawalDemandRaw = 1 }, func(s *Snapshot) { s.HasPosition, s.PositionCollateralRaw = true, 1 }, func(s *Snapshot) { s.ManualReason = "recovery" }} {
		changed := s
		change(&changed)
		_, err = observeSelectorSource(context.Background(), rpc, fixtureView(t, rpc), client, m, tickObservation(changed))
		assertBudgetHold(t, err, "selector_source_unavailable")
	}
}
