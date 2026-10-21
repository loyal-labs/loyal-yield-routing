package backyard

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

func TestInjectedSignerRejectsForgedPinnedPublicHalf(t *testing.T) {
	t.Parallel()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	pinned := mustKey(backyardDelegate)
	copy(key[ed25519.SeedSize:], pinned[:])
	if _, err := (Credentials{key: key}).signer(); err == nil || !strings.Contains(err.Error(), "public half") {
		t.Fatalf("forged injected capability reached signing: %v", err)
	}
}
