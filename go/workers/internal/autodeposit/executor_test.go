package autodeposit

import "testing"

func TestClaimHoldingAttemptStates(t *testing.T) {
	// A confirmed pull has already moved funds out of the wallet, so the claim
	// must survive a restart and a restarted worker resumes it.
	for _, state := range []AttemptState{AttemptPrepared, AttemptSubmitted, AttemptConfirmed} {
		if !AttemptHoldsClaim(state) || !containsState(ClaimHoldingPullAttemptStates, string(state)) {
			t.Fatalf("attempt state %q must hold the claim", state)
		}
	}
	for _, state := range []AttemptState{AttemptFailed, AttemptExpired} {
		if AttemptHoldsClaim(state) {
			t.Fatalf("conclusive state %q must not hold the claim", state)
		}
		if !AttemptAllowsSafeRequeue(state) {
			t.Fatalf("conclusive state %q must allow a fresh attempt", state)
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
