package backyard

import "testing"

func TestTransitions(t *testing.T) {
	t.Parallel()
	if !CanTransition(Signed, BroadcastIntent) || !CanTransition(Confirmed, Reconciling) ||
		CanTransition(Confirmed, Reconciled) || CanTransition(Held, Decided) ||
		CanTransition(Failed, Decided) {
		t.Fatal("invalid transition rules")
	}
}

func TestExpiredAbsentSubmissionCanTerminateWithoutResend(t *testing.T) {
	t.Parallel()
	if !CanTransition(BroadcastIntent, Failed) {
		t.Fatal("expired absent submission cannot reach its migration-backed terminal state")
	}
	if CanTransition(Confirmed, Failed) || CanTransition(Reconciling, Failed) {
		t.Fatal("landed operations must not use the expired-absent terminal path")
	}
}

func TestNonterminalSetExcludesPersistedHolds(t *testing.T) {
	t.Parallel()
	for _, status := range []OperationStatus{Decided, Built, Simulated, Signed, BroadcastIntent, Confirmed, Reconciling} {
		if !IsNonterminal(status) {
			t.Fatalf("expected %s to be nonterminal", status)
		}
	}
	for _, status := range []OperationStatus{Held, ManualRecovery, Reconciled, Failed} {
		if IsNonterminal(status) {
			t.Fatalf("expected %s to be terminal", status)
		}
	}
}
