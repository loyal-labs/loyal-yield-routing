package backyardrwa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func pilotTestAuthority(prior Phase3Budget) pilotBudgetAuthority {
	encoded, _ := json.Marshal(prior)
	return pilotBudgetAuthority{pilotBudgetAuthoritySchema, pilotBudgetAuthorityID, false, sha256Bytes(encoded), 900, 447400000, sha256Bytes([]byte("finalized-flat-test-evidence"))}
}
func pilotTestBudget(t *testing.T) Phase3Budget {
	t.Helper()
	old := emptyTestBudget()
	old.Families["OnRe"] = FamilyBudget{SpentMicros: 7_000_000}
	next, err := activatePilotBudget(old, pilotTestAuthority(old))
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func TestPilotBudgetReusesPrincipalAcrossRotationsAndRestart(t *testing.T) {
	b := pilotTestBudget(t)
	for cycle := 0; cycle < 100; cycle++ {
		family := []string{"Prime", "Maple", "OnRe"}[cycle%3]
		for leg := 0; leg < 3; leg++ {
			id := fmt.Sprintf("rotation-%d-leg-%d", cycle, leg)
			r := BudgetReservation{OperationID: id, Family: family, IntentSHA256: sha256Bytes([]byte(id)), UpperMicros: 15_000_000, ExecutionCostUpperMicros: 10_000, ExitAfterMicros: int64(2-leg) * 15_000_000, Recovery: leg > 0}
			if err := b.Admit(r); err != nil {
				t.Fatalf("rotation %d leg%d: %v", cycle, leg, err)
			}
			before, _ := json.Marshal(b)
			if err := b.Admit(r); err != nil {
				t.Fatal("retry lost immutable reservation", err)
			}
			after, _ := json.Marshal(b)
			if !bytes.Equal(before, after) {
				t.Fatal("retry changed budget")
			}
			// Restart with the ambiguous reservation still outstanding.
			var restarted Phase3Budget
			if err := json.Unmarshal(after, &restarted); err != nil {
				t.Fatal(err)
			}
			other := r
			other.OperationID += "-other"
			assertBudgetHold(t, restarted.Admit(other), "unresolved_submission_reservation")
			if err := restarted.AuthorizeIntent(id, r.IntentSHA256); err != nil {
				t.Fatal(err)
			}
			if err := restarted.Settle(id, r.IntentSHA256, r.UpperMicros); err != nil {
				t.Fatal(err)
			}
			b = restarted
		}
	}
	var gross, expenses int64
	for _, row := range b.Families {
		gross += row.SpentMicros
		expenses += row.ExecutionCostSpentMicros
		if row.ExitMicros != 0 {
			t.Fatal("final exit reservation remains")
		}
	}
	if gross != 4_507_000_000 || expenses != 3_000_000 {
		t.Fatalf("lost gross/cost history: %d %d", gross, expenses)
	}
	if len(b.Reservations) != 0 || b.GoalID != Phase3GoalID || b.Pilot.AuthorityID != pilotBudgetAuthorityID {
		t.Fatal("authority/history changed")
	}
}
func TestPilotExecutionCostCapPreservesReservedUnwind(t *testing.T) {
	b := pilotTestBudget(t)
	first := BudgetReservation{OperationID: "entry", Family: "Prime", IntentSHA256: sha256Bytes([]byte("entry")), UpperMicros: PilotEntryExecutionCostCapMicros, ExecutionCostUpperMicros: PilotEntryExecutionCostCapMicros, ExitAfterMicros: 20_000_000}
	if err := b.Admit(first); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(first.OperationID, first.IntentSHA256, first.UpperMicros); err != nil {
		t.Fatal(err)
	}
	newEntry := BudgetReservation{OperationID: "another", Family: "Prime", IntentSHA256: sha256Bytes([]byte("another")), UpperMicros: 10_000_000, ExecutionCostUpperMicros: 1, ExitAfterMicros: 20_000_000}
	before, _ := json.Marshal(b)
	assertBudgetHold(t, b.Admit(newEntry), "pilot_entry_execution_cost_cap_exhausted")
	after, _ := json.Marshal(b)
	if !bytes.Equal(before, after) {
		t.Fatal("refused entry changed reserved withdrawal")
	}
	exit := BudgetReservation{OperationID: "exit", Family: "Prime", IntentSHA256: sha256Bytes([]byte("exit")), UpperMicros: 20_000_000, ExecutionCostUpperMicros: 20_000, Recovery: true}
	if err := b.Admit(exit); err != nil {
		t.Fatal("blocked reserved unwind", err)
	}
	if err := b.Settle(exit.OperationID, exit.IntentSHA256, exit.UpperMicros); err != nil {
		t.Fatal(err)
	}
	if b.Families["Prime"].ExitMicros != 0 || b.Families["Prime"].ExecutionCostSpentMicros != PilotEntryExecutionCostCapMicros+20_000 {
		t.Fatal("unwind lost accounting")
	}
}
func TestPilotAuthorityTransitionDoesNotEraseHistoryOrWidenImplicitly(t *testing.T) {
	for _, mutation := range []string{"closed", "reservation", "exit", "wrong-prior-hash", "missing-evidence"} {
		t.Run(mutation, func(t *testing.T) {
			old := emptyTestBudget()
			old.Families["OnRe"] = FamilyBudget{SpentMicros: 19_000_000}
			switch mutation {
			case "closed":
				old.Closed = true
			case "reservation":
				old.Reservations["old"] = BudgetReservation{OperationID: "old", Family: "OnRe", IntentSHA256: sha256Bytes([]byte("old")), UpperMicros: 1}
			case "exit":
				row := old.Families["OnRe"]
				row.ExitMicros = 1
				old.Families["OnRe"] = row
			}
			a := pilotTestAuthority(old)
			if mutation == "wrong-prior-hash" {
				a.PreviousBudgetSHA256 = sha256Bytes([]byte("other"))
			}
			if mutation == "missing-evidence" {
				a.FlatEvidenceSHA256 = ""
			}
			before, _ := json.Marshal(old)
			if _, err := activatePilotBudget(old, a); err == nil {
				t.Fatal("accepted unsafe transition")
			}
			after, _ := json.Marshal(old)
			if !bytes.Equal(before, after) {
				t.Fatal("transition mutated original")
			}
		})
	}
	b := pilotTestBudget(t)
	before, _ := json.Marshal(b)
	if _, err := activatePilotBudget(b, pilotTestAuthority(b)); err == nil {
		t.Fatal("replenished existing authority")
	}
	after, _ := json.Marshal(b)
	if !bytes.Equal(before, after) {
		t.Fatal("retry changed authority")
	}
	if err := b.ConstrainLimits(DeploymentLimits{10_000_000, 80_000_000, 120_000_000}); err != nil {
		t.Fatal(err)
	}
	assertBudgetHold(t, b.ConstrainLimits(pilotDeploymentLimits()), "deployment_limit_increase_requires_authority_transition")
	legacy := emptyTestBudget()
	if err := legacy.ConstrainLimits(pilotDeploymentLimits()); err == nil {
		t.Fatal("legacy implicit increase")
	}
	for _, r := range []BudgetReservation{
		{OperationID: "no-cost", Family: "Prime", IntentSHA256: sha256Bytes([]byte("x")), UpperMicros: 1},
		{OperationID: "excess-cost", Family: "Prime", IntentSHA256: sha256Bytes([]byte("x")), UpperMicros: 1, ExecutionCostUpperMicros: 2},
		{OperationID: "retired-family", Family: "AUTO", IntentSHA256: sha256Bytes([]byte("x")), UpperMicros: 1, ExecutionCostUpperMicros: 1},
	} {
		if err := b.Admit(r); err == nil {
			t.Fatalf("admitted missing cost or retired family: %+v", r)
		}
	}
	prime := BudgetReservation{OperationID: "prime", Family: "Prime", IntentSHA256: sha256Bytes([]byte("prime")), UpperMicros: 1}
	if err := legacy.Admit(prime); err == nil {
		t.Fatal("legacy budget adopted Prime")
	}
}

func TestPilotReservedExitPreventsLimitNarrowing(t *testing.T) {
	b := pilotTestBudget(t)
	r := BudgetReservation{OperationID: "entry", Family: "Prime", IntentSHA256: sha256Bytes([]byte("entry")), UpperMicros: 15_000_000, ExecutionCostUpperMicros: 10_000, ExitAfterMicros: 30_000_000}
	if err := b.Admit(r); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(r.OperationID, r.IntentSHA256, r.UpperMicros); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(b)
	assertBudgetHold(t, b.ConstrainLimits(DeploymentLimits{10_000_000, 80_000_000, 120_000_000}), "deployment_limits_have_reserved_exits")
	after, _ := json.Marshal(b)
	if !bytes.Equal(before, after) {
		t.Fatal("limit change disabled exit")
	}
	exit := BudgetReservation{OperationID: "exit", Family: "Prime", IntentSHA256: sha256Bytes([]byte("exit")), UpperMicros: 15_000_000, ExecutionCostUpperMicros: 10_000, ExitAfterMicros: 15_000_000, Recovery: true}
	if err := b.Admit(exit); err != nil {
		t.Fatal("reserved $15 recovery no longer fits", err)
	}
}
