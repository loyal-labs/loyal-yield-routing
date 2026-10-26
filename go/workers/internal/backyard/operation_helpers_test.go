package backyard

import (
	"errors"
	"testing"
)

// assertPreSendHold wants a hold whose reason (one of the enum) or cause is
// reason.
func assertPreSendHold(t *testing.T, err error, reason string) {
	t.Helper()
	var hold *PreSendHold
	if !errors.As(err, &hold) || string(hold.Reason) != reason && holdCause(hold) != reason {
		t.Fatalf("wanted HOLD %s; got %v", reason, err)
	}
}
