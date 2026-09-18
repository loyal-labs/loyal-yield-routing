package backyardrwa

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// The reviewed increase widens the code ceilings only. A budget already
// activated under the previous pilot limits keeps binding on its persisted
// Limits record: loading the new worker neither raises nor resets it, and
// only an explicitly persisted limit record reaches the reviewed envelope.
func TestPilotCeilingIncreaseKeepsPersistedLimitsBinding(t *testing.T) {
	if legacy := legacyDeploymentLimits(); legacy != (DeploymentLimits{1_000_000, 20_000_000, 60_000_000}) {
		t.Fatal("legacy envelope moved with the pilot ceilings", legacy)
	}
	oldLimits := DeploymentLimits{20_000_000, 100_000_000, 150_000_000}
	persisted := pilotTestBudget(t)
	persisted.Limits = &oldLimits
	if err := persisted.validate(); err != nil {
		t.Fatal("pre-increase persisted budget invalidated by ceiling increase:", err)
	}
	workingSize := BudgetReservation{OperationID: "working-size", Family: "Maple", IntentSHA256: sha256Bytes([]byte("working-size")), UpperMicros: PilotWorkingTrancheCapRaw, ExecutionCostUpperMicros: 500_000, ExitAfterMicros: PilotWorkingTrancheCapRaw}
	persisted.Families["Maple"] = FamilyBudget{SpentMicros: 1_000_000, ExecutionCostSpentMicros: 1_000}
	before, _ := json.Marshal(persisted)
	assertBudgetHold(t, persisted.Admit(workingSize), "transaction_cap_exceeded")
	after, _ := json.Marshal(persisted)
	if !bytes.Equal(before, after) {
		t.Fatal("refused working-size entry mutated budget")
	}
	if got := persisted.Families["Maple"]; got.SpentMicros != 1_000_000 || got.ExecutionCostSpentMicros != 1_000 {
		t.Fatal("rejected working-size entry moved Maple counters:", got)
	}
	assertBudgetHold(t, persisted.ConstrainLimits(pilotDeploymentLimits()), "deployment_limit_increase_requires_authority_transition")

	// An explicitly persisted reviewed limit record reaches the new envelope
	// with the activation authority, spend history and reservations intact.
	reviewed := pilotDeploymentLimits()
	persisted.Limits = &reviewed
	if err := persisted.validate(); err != nil {
		t.Fatal("reviewed limit record rejected:", err)
	}
	if err := persisted.Admit(workingSize); err != nil {
		t.Fatal("reviewed working size refused under recorded limits:", err)
	}
	if err := persisted.AuthorizeIntent(workingSize.OperationID, workingSize.IntentSHA256); err != nil {
		t.Fatal(err)
	}
	if err := persisted.Settle(workingSize.OperationID, workingSize.IntentSHA256, workingSize.UpperMicros); err != nil {
		t.Fatal(err)
	}
	if got := persisted.Families["Maple"]; got.SpentMicros != 1_000_000+PilotWorkingTrancheCapRaw || got.ExecutionCostSpentMicros != 1_000+500_000 || got.ExitMicros != PilotWorkingTrancheCapRaw {
		t.Fatal("limit record reset counters:", got)
	}
	if persisted.GoalID != Phase3GoalID || persisted.Pilot == nil || persisted.Pilot.AuthorityID != pilotBudgetAuthorityID {
		t.Fatal("limit record changed activation authority")
	}
	if err := persisted.ConstrainLimits(pilotDeploymentLimits()); err != nil {
		t.Fatal("recording identical limits disturbed persisted state:", err)
	}

	// Working-size boundary: exactly the reviewed allocation is valid, one
	// raw unit more is not, independent of the budget record.
	atCap := selectorEntryFixture(time.Now().UTC(), SelectedRouteID, PilotWorkingTrancheCapRaw)
	if err := atCap.validate(); err != nil {
		t.Fatal("reviewed working-size entry rejected:", err)
	}
	overCap := selectorEntryFixture(time.Now().UTC(), SelectedRouteID, PilotWorkingTrancheCapRaw+1)
	if err := overCap.validate(); err == nil {
		t.Fatal("over-cap working-size entry accepted")
	}
}
