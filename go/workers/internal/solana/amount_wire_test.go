package solana

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestAttemptOwnsImmutableWire(t *testing.T) {
	source := []byte{1, 2, 3}
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	wire, err := OwnSignedWire(source, hash)
	if err != nil {
		t.Fatal(err)
	}
	source[0] = 9
	exported := wire.Bytes()
	exported[0] = 8
	if wire.Bytes()[0] != 1 || wire.Hash() != hash {
		t.Fatal("caller mutated owned attempt")
	}
	if _, err := OwnSignedWire([]byte{1, 2, 4}, hash); err == nil {
		t.Fatal("persisted hash discrepancy accepted")
	}
}
