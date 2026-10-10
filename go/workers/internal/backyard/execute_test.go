package backyard

import "testing"

func TestTransitions(t *testing.T) {
	t.Parallel()
	if !CanTransition(Signed, BroadcastIntent) || !CanTransition(Confirmed, Reconciling) ||
		CanTransition(Confirmed, Reconciled) || CanTransition(Held, Decided) ||
		CanTransition(Failed, Decided) || CanTransition(Submitted, Signed) {
		t.Fatal("invalid transition rules")
	}
}

func TestExpiredAbsentSubmissionCanTerminateWithoutResend(t *testing.T) {
	t.Parallel()
	if !CanTransition(BroadcastIntent, Failed) || !CanTransition(Submitted, Failed) {
		t.Fatal("expired absent submission cannot reach its migration-backed terminal state")
	}
	if CanTransition(Confirmed, Failed) || CanTransition(Reconciling, Failed) {
		t.Fatal("landed operations must not use the expired-absent terminal path")
	}
}

func TestNonterminalSetExcludesPersistedHolds(t *testing.T) {
	t.Parallel()
	for _, status := range []OperationStatus{Decided, Built, Simulated, Signed, BroadcastIntent, Submitted, Confirmed, Reconciling} {
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

func TestWithdrawalPreemptsEveryOpenPreBroadcastState(t *testing.T) {
	t.Parallel()
	for _, status := range []OperationStatus{Decided, Built, Simulated, Signed} {
		if !WithdrawalPreemptsOpenLoop(OpenPrimeUSDCStep, status, 1) {
			t.Fatalf("withdrawal did not preempt OPEN in %s", status)
		}
	}
	if WithdrawalPreemptsOpenLoop(OpenPrimeUSDCStep, Submitted, 1) {
		t.Fatal("ambiguous submitted OPEN must recover its exact signature")
	}
	if WithdrawalPreemptsOpenLoop(DeleverPrimeUSDCStep, Signed, 1) {
		t.Fatal("withdrawal must not cancel a risk-reducing action")
	}
}
