package admin

import (
	"net/url"
	"testing"
)

func TestSafeNextPathRefusesProtocolRelative(t *testing.T) {
	base := &url.URL{Scheme: "https", Host: "admin.example"}
	if got, ok := SafeNextPath("/.//evil.com", base); ok {
		t.Fatalf("got %q, want refused", got)
	}
}
