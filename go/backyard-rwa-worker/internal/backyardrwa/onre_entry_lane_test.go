package backyardrwa

import (
	"testing"
	"time"
)

// B4: OnRe is an entry lane under the unchanged selector rule; Prime is not.
// The live quote collector quotes only selectorEntryFundingLane destinations
// (others get lane_entry_deferred), so the funding scope is the fence.
func TestOnReIsAnEntryLaneUnderTheUnchangedSelectorRule(t *testing.T) {
	if !selectorEntryLane(onreONycUSDC) || !selectorEntryLane(SelectedRouteID) || selectorEntryLane(PhaseOneLaneID) || selectorEntryLane(autoAUTOPYUSD.Lane) {
		t.Fatal("entry lanes must be exactly Maple and OnRe (AUTO stays manifest-gated)")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.selectorEntryFundingLane(onreONycUSDC, false) || manifest.selectorEntryFundingLane(PhaseOneLaneID, false) {
		t.Fatal("funding scope does not follow the entry lanes")
	}
	// The unchanged rule: an OnRe advantage must persist for the policy's
	// persistence window before the selector enters.
	in := selectorFixture() // Maple source, OnRe destination market
	in.Snapshot.PilotActive = true
	first := selectOpportunityWithLanes(in, SelectorState{}, selectorLane, selectorEntryLane)
	if first.Action != "KEEP" || first.Reason != "advantage_not_yet_persistent" {
		t.Fatalf("OnRe entered before its advantage persisted: %+v", first)
	}
	advanceSelectorFixture(&in, in.Policy.Persistence+time.Second)
	entered := selectOpportunityWithLanes(in, first.State, selectorLane, selectorEntryLane)
	if entered.Action != "ENTER" || entered.DestinationLane != onreONycUSDC {
		t.Fatalf("persistent OnRe advantage did not enter: %+v", entered)
	}
	// A funded OnRe position may price its own same-lane reinvestment.
	same := sameLaneSelectorFixture()
	same.Snapshot.RouteLane, same.Snapshot.StrategyKey = onreONycUSDC, onreONycUSDC
	if !sameLaneReinvestmentEligible(same.Snapshot, same.Policy) {
		t.Fatal("funded OnRe position cannot reinvest in its own lane")
	}
	same.Snapshot.RouteLane, same.Snapshot.StrategyKey = PhaseOneLaneID, PhaseOneLaneID
	if sameLaneReinvestmentEligible(same.Snapshot, same.Policy) {
		t.Fatal("dropped Prime lane may reinvest")
	}
}
