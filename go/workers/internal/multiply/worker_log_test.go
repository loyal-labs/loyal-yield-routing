package multiply

import (
	"errors"
	"testing"
)

// A tick failure must say why (Rust logged it), without leaking a DSN,
// endpoint or key material into the shipped logs.
func TestSafeErrorKeepsCauseAndRedactsSecrets(t *testing.T) {
	for _, tc := range []struct{ err, want string }{
		{"route lease is held by another owner", "route lease is held by another owner"},
		{"dial postgres://u:p@host/db: refused", "external dependency failed; inspect terminal logs"},
		{"Post \"https://rpc.example/?api-key=x\": timeout", "external dependency failed; inspect terminal logs"},
		{"invalid keypair material", "external dependency failed; inspect terminal logs"},
	} {
		if got := safeError(errors.New(tc.err)); got != tc.want {
			t.Errorf("safeError(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
