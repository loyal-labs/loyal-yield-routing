package backyardrwa

import (
	"context"
	"testing"
	"time"
)

func TestSelectorMovePricesIdleEntryAndRejectsWholeRecipeNativeShortfall(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixture(t)
	s := Snapshot{PilotActive: true, Fresh: true, Slot: 42, ObservationID: "idle-move", RouteLane: SelectedRouteID, StrategyKey: SelectedRouteID, VoltrIdleRaw: 100_000_000}
	o := tickObservation(s)
	o.ObservedAt = time.Now().UTC()
	q, err := observeSelectorMove(context.Background(), rpc, client, m, o, SelectedRouteID, 10_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if q.EquityRaw != 10_000_000 || q.MinimumIdleRaw != 100_000_000 || q.BorrowReceiveRaw == 0 || q.CostRaw <= 0 || !sha256Pattern.MatchString(q.EvidenceID) || !q.currentAtSlot(42) {
		t.Fatal("incomplete move", q)
	}
	source, err := observeSelectorSource(context.Background(), rpc, client, m, o)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := observeSelectorDestination(context.Background(), rpc, client, m, SelectedRouteID, 10_000_000, 42)
	if err != nil {
		t.Fatal(err)
	}
	// Each component alone fits the observed fee payer; their sum must fit too.
	// A bounded synthetic source cost isolates this composition check. Actual
	// source graph production is covered separately by the full-loop test.
	source.Recipe.NetworkLamports = 1_000_000_000
	if _, err = composeSelectorMove(context.Background(), rpc, o, source, destination); err == nil {
		t.Fatal("combined native fee shortfall accepted")
	}
	// Refuse a destination that exceeds the guaranteed source return.
	source.Recipe.NetworkLamports = 0
	source.MinimumIdleRaw = 9_999_999
	_, err = composeSelectorMove(context.Background(), rpc, o, source, destination)
	assertBudgetHold(t, err, "invalid_selector_move")
}

func TestPilotSelectorSizesFromMinimumReturnedCash(t *testing.T) {
	in := selectorFixture()
	in.Snapshot.PilotActive = true
	in.Snapshot.TotalVaultNAVRaw, in.Snapshot.VoltrIdleRaw = 10_000_000, 10_000_000
	q := &in.Quotes[0]
	q.EquityRaw = 9_900_000
	q.MinimumIdleRaw = 9_900_000
	q.BorrowReceiveRaw = 4_900_000
	result := SelectOpportunity(in, SelectorState{})
	if len(result.Candidates) != 1 || !result.Candidates[0].CostsKnown || result.Candidates[0].InvestedRaw != q.EquityRaw-q.CostRaw {
		t.Fatal("unexecutable pre-exit equity forced", result)
	}
	q.EquityRaw = 10_000_000
	if got := SelectOpportunity(in, SelectorState{}).Candidates[0]; got.CostsKnown {
		t.Fatal("optimistic equity accepted", got)
	}
}

func TestPilotSelectorRetainsBufferAfterSourceExitLoss(t *testing.T) {
	in := selectorFixture()
	in.Snapshot.PilotActive = true
	in.Snapshot.TotalVaultNAVRaw, in.Snapshot.VoltrIdleRaw = 10_000_000, 10_000_000
	in.Policy.IdleBufferRaw = 2_000_000
	q := &in.Quotes[0]
	q.MinimumIdleRaw = 7_900_000
	q.EquityRaw = 5_900_000
	q.BorrowReceiveRaw = 2_900_000
	c := SelectOpportunity(in, SelectorState{}).Candidates[0]
	if !c.CostsKnown {
		t.Fatal("correct buffered amount unavailable", c)
	}
	q.EquityRaw = 7_900_000
	if c = SelectOpportunity(in, SelectorState{}).Candidates[0]; c.CostsKnown {
		t.Fatal("exit loss consumed buffer", c)
	}
	m, rpc, client, _ := selectorDestinationFixture(t)
	s := Snapshot{PilotActive: true, Fresh: true, Slot: 42, ObservationID: "buffer", RouteLane: SelectedRouteID, StrategyKey: SelectedRouteID, VoltrIdleRaw: 7_900_000}
	o := tickObservation(s)
	o.ObservedAt = time.Now().UTC()
	got, err := observeSelectorMove(context.Background(), rpc, client, m, o, SelectedRouteID, 8_000_000, 2_000_000)
	if err != nil || got.EquityRaw != 5_900_000 {
		t.Fatal("producer buffer disagrees with selector", got, err)
	}
	_, err = observeSelectorMove(context.Background(), rpc, client, m, o, SelectedRouteID, 8_000_000, 7_900_000)
	assertBudgetHold(t, err, "selector_move_has_no_entry_cash")
}
