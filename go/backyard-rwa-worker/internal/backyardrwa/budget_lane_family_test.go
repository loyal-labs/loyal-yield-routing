package backyardrwa

import (
	"context"
	"testing"
)

// Audit B2 / contract test: the manifest-selected lane must reach bridge
// admission instead of cycling decided -> failed on an unavailable admission
// snapshot, and an unknown lane must still be refused with no generic fallback.
func TestSelectedLaneHasPhase3BridgeFamily(t *testing.T) {
	if phase3BudgetFamilyForLane(SelectedRouteID) != "Maple" {
		t.Fatalf("selected lane family = %q, want Maple", phase3BudgetFamilyForLane(SelectedRouteID))
	}
	if _, err := runtimeRoute(SelectedRouteID); err != nil {
		t.Fatalf("selected lane is not a runtime route: %v", err)
	}

	o, d, evidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
	o.Snapshot.RouteLane, o.Snapshot.StrategyKey = SelectedRouteID, SelectedRouteID
	d.StrategyKey = SelectedRouteID
	// Passing the family gate advances to the next admission boundary: with no
	// RPC the valuation itself is unavailable, which is a later, distinct hold.
	if _, err := observePhase3BridgeAdmission(context.Background(), nil, o, d, evidence); err == nil {
		t.Fatal("admission proceeded without an RPC client")
	} else if hold, ok := err.(*BudgetHold); !ok || hold.Reason == "bridge_admission_snapshot_unavailable" {
		t.Fatalf("selected lane was refused at the family gate: %v", err)
	}

	for _, lane := range []string{"Prime/PRIME/PYUSD", "Prime/PRIME/USDS"} {
		if phase3BudgetFamilyForLane(lane) != "" {
			t.Fatalf("sibling lane %s authorized an extra funded canary", lane)
		}
	}

	unknown, unknownDecision, unknownEvidence := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
	unknown.Snapshot.RouteLane, unknown.Snapshot.StrategyKey = "unknown/asset/debt", "unknown/asset/debt"
	unknownDecision.StrategyKey = "unknown/asset/debt"
	_, err := observePhase3BridgeAdmission(context.Background(), nil, unknown, unknownDecision, unknownEvidence)
	assertBudgetHold(t, err, "bridge_admission_snapshot_unavailable")

	// The selected lane's family is a fourth row inside the same goal envelope,
	// so the three-family canary budget keeps validating unchanged.
	budget := Phase3Budget{GoalID: Phase3GoalID, Families: map[string]FamilyBudget{}, Reservations: map[string]BudgetReservation{}}
	for _, family := range []string{"OnRe", "AUTO", "Ethena"} {
		budget.Families[family] = FamilyBudget{}
	}
	if err := budget.validate(); err != nil {
		t.Fatalf("canary budget no longer validates: %v", err)
	}
	budget.Families["Maple"] = FamilyBudget{}
	if err := budget.validate(); err != nil {
		t.Fatalf("selected-lane family budget was rejected: %v", err)
	}
	if err := (Phase3Budget{GoalID: Phase3GoalID, Families: map[string]FamilyBudget{
		"OnRe": {}, "AUTO": {}, "Ethena": {}, "Maple": {}, "Extra": {},
	}, Reservations: map[string]BudgetReservation{}}).validate(); err == nil {
		t.Fatal("an unfunded fifth family was accepted into the goal budget")
	}
}
