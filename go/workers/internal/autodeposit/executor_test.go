package autodeposit

import (
	"encoding/json"
	"testing"
)

type executorExitFixture struct {
	Description string `json:"description"`
	Cases       []struct {
		ExitCode *int `json:"exitCode"`
		Expect   struct {
			Result         string  `json:"result"`
			AlertCode      *string `json:"alertCode"`
			Retryable      bool    `json:"retryable"`
			SelfRecovering bool    `json:"selfRecovering"`
		} `json:"expect"`
	} `json:"cases"`
}

func TestExecutorExitContract(t *testing.T) {
	var fixture executorExitFixture
	if err := json.Unmarshal(mustReadFixture(t, "executor_exits.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		result := ExecutorResultFromExitCode(testCase.ExitCode)
		if string(result) != testCase.Expect.Result {
			t.Fatalf("exit %v classified %q, want %q", testCase.ExitCode, result, testCase.Expect.Result)
		}
		alert := ExecutorFailureAlertFor(testCase.ExitCode)
		if testCase.Expect.AlertCode == nil {
			if alert != nil {
				t.Fatalf("exit %v alerted %q, want no alert: a correct decision must not page", testCase.ExitCode, alert.Code)
			}
			continue
		}
		if alert == nil {
			t.Fatalf("exit %v produced no alert, want %q", testCase.ExitCode, *testCase.Expect.AlertCode)
		}
		if alert.Code != *testCase.Expect.AlertCode {
			t.Fatalf("exit %v alerted %q, want %q", testCase.ExitCode, alert.Code, *testCase.Expect.AlertCode)
		}
		if alert.Retryable != testCase.Expect.Retryable {
			t.Fatalf("exit %v alert retryable=%v, want %v", testCase.ExitCode, alert.Retryable, testCase.Expect.Retryable)
		}
		if alert.SelfRecovering != testCase.Expect.SelfRecovering {
			t.Fatalf("exit %v alert selfRecovering=%v, want %v", testCase.ExitCode, alert.SelfRecovering, testCase.Expect.SelfRecovering)
		}
	}
}

// An unclassified exit zero must never count as completed work: legacy
// executors exit zero while funds are still pending.
func TestUnclassifiedExitZeroIsNotCompletion(t *testing.T) {
	zero := 0
	var outcome ExecutorOutcome
	if alert := outcome.RecordExecutorExit(&zero); alert != nil {
		t.Fatalf("unclassified success alerted %q", alert.Code)
	}
	if outcome.ExecutionsCompleted != 0 {
		t.Fatalf("unclassified exit zero counted as %d completions", outcome.ExecutionsCompleted)
	}
	if outcome.ExecutionsProcessSuccessUnclassifd != 1 {
		t.Fatal("unclassified exit zero was not counted separately")
	}
}

func TestSignalTerminationStaysGenericAndActionable(t *testing.T) {
	var outcome ExecutorOutcome
	alert := outcome.RecordExecutorExit(nil)
	if alert == nil || alert.Code != "autodeposit_executor_failed" {
		t.Fatalf("signal termination alert %v must stay the generic executor failure", alert)
	}
	if outcome.ExecutionsFailed != 1 {
		t.Fatal("signal termination was not counted as a failed execution")
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
