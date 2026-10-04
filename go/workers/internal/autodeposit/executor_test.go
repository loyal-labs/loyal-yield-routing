package autodeposit

import "testing"

func TestExecutorFailureContract(t *testing.T) {
	for _, test := range []struct {
		result                    ExecutorResult
		code, operation           string
		retryable, selfRecovering bool
	}{
		{ResultKaminoTopUpFailed, "kamino_top_up_failed", "top_up_autodeposit_to_kamino", false, false},
		{ResultYieldPersistenceFailed, "yield_persistence_failed", "persist_autodeposit_yield_position", false, false},
		{ResultPreflightBlocked, "autodeposit_preflight_blocked", "preflight_autodeposit_route", true, false},
		{ResultFeePayerExhausted, "autodeposit_fee_payer_exhausted", "fund_autodeposit_fee_payer", true, false},
		{ResultTransactionEffectAmbig, "autodeposit_transaction_effect_ambiguous", "reconcile_autodeposit_transaction", false, false},
		{ResultIdleHandoffFailed, "autodeposit_idle_handoff_failed", "publish_autodeposit_idle_vault_balance", true, false},
		{ResultDependencyUnavailable, "autodeposit_dependency_unavailable", "retry_autodeposit_after_dependency_recovers", true, true},
	} {
		var outcome ExecutorOutcome
		alert := outcome.RecordExecutorResult(test.result)
		if outcome.ExecutionsFailed != 1 || alert == nil || alert.Code != test.code || alert.Operation != test.operation || alert.Retryable != test.retryable || alert.SelfRecovering != test.selfRecovering {
			t.Fatalf("result %q: outcome %+v alert %+v", test.result, outcome, alert)
		}
	}
}
func TestUnknownOutcomeNeverProvesCompletion(t *testing.T) {
	for _, result := range []ExecutorResult{ResultUnknown, "future_outcome"} {
		var outcome ExecutorOutcome
		alert := outcome.RecordExecutorResult(result)
		if outcome.ExecutionsCompleted != 0 || alert == nil || alert.Code != "autodeposit_executor_failed" || outcome.ExecutionsUnknown+outcome.ExecutionsFailed != 1 {
			t.Fatalf("unknown result %q: %+v, alert %v", result, outcome, alert)
		}
	}
}

func TestClaimHoldingAndRecoverableAttemptStates(t *testing.T) {
	// A confirmed pull has already moved funds out of the wallet, so the claim
	// must survive a restart; ambiguous is claim-holding but never auto-retried.
	for _, state := range []AttemptState{AttemptPrepared, AttemptSubmitted, AttemptConfirmed, AttemptUnknown, AttemptAmbiguous} {
		if !AttemptHoldsClaim(state) {
			t.Fatalf("attempt state %q must hold the claim", state)
		}
	}
	for _, state := range []AttemptState{AttemptPrepared, AttemptSubmitted, AttemptConfirmed, AttemptUnknown} {
		if !containsState(AutomaticPullRecoveryStates, string(state)) {
			t.Fatalf("attempt state %q must be automatically recoverable after a restart", state)
		}
	}
	if containsState(AutomaticPullRecoveryStates, string(AttemptAmbiguous)) {
		t.Fatal("ambiguous broadcast must require an operator, never an automatic resend")
	}
	for _, state := range []AttemptState{AttemptFailed, AttemptExpired} {
		if AttemptHoldsClaim(state) {
			t.Fatalf("conclusive state %q must not hold the claim", state)
		}
		if !AttemptAllowsSafeRequeue(state) {
			t.Fatalf("conclusive state %q must allow a fresh attempt", state)
		}
	}
	if AlertForAttemptState(AttemptAmbiguous) == nil {
		t.Fatal("ambiguous effect must alert for reconciliation")
	}
	for _, state := range []AttemptState{AttemptConfirmed, AttemptFailed, AttemptExpired, AttemptUnknown, AttemptPrepared} {
		if AlertForAttemptState(state) != nil {
			t.Fatalf("attempt state %q must not page", state)
		}
	}
}

func containsState(states []string, want string) bool {
	for _, state := range states {
		if state == want {
			return true
		}
	}
	return false
}
