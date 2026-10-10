package backyard

import (
	"testing"
	"time"
)

// Every active registry lane is an entry lane under the unchanged selector
// rule (owner 2026-10-10: Prime/PRIME/USDC and the Prime siblings again, AUTO
// without a manifest gate); the exit-only Ethena lane is not.
func TestEveryActiveRegistryLaneEntersUnderTheUnchangedSelectorRule(t *testing.T) {
	t.Parallel()
	want := []string{SelectedRouteID, onreONycUSDC, autoAUTOPYUSD.Lane, PhaseOneLaneID, primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane}
	got := earnLaneIDs(false)
	if len(got) != len(want) {
		t.Fatalf("active registry %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] || !earnActiveLane(want[i]) {
			t.Fatalf("active registry %v, want %v", got, want)
		}
	}
	if earnActiveLane(ethenaUSDePYUSD.Lane) {
		t.Fatal("exit-only Ethena is an entry lane")
	}
	// The unchanged rule: an OnRe advantage must persist for the policy's
	// persistence window before the selector enters.
	in := selectorFixture() // Maple source, OnRe destination market
	first := SelectOpportunity(in, SelectorState{})
	if first.Action != "KEEP" || first.Reason != "advantage_not_yet_persistent" {
		t.Fatalf("OnRe entered before its advantage persisted: %+v", first)
	}
	advanceSelectorFixture(&in, in.Policy.Persistence+time.Second)
	entered := SelectOpportunity(in, first.State)
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
	if !sameLaneReinvestmentEligible(same.Snapshot, same.Policy) {
		t.Fatal("Prime/PRIME/USDC may not reinvest")
	}
	same.Snapshot.RouteLane, same.Snapshot.StrategyKey = ethenaUSDePYUSD.Lane, ethenaUSDePYUSD.Lane
	if sameLaneReinvestmentEligible(same.Snapshot, same.Policy) {
		t.Fatal("exit-only Ethena may reinvest")
	}
}
