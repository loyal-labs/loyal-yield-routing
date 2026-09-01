package backyardrwa

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

func TestDecodeSolanaKeypairMaterialMatchesRepoFormats(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	expected := ed25519.NewKeyFromSeed(seed)
	for _, encoded := range []string{hex.EncodeToString(seed), encodeBase58(seed), "[7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7]"} {
		decoded, err := decodeSolanaKeypairMaterial(encoded)
		if err != nil || !bytes.Equal(decoded, expected) {
			t.Fatalf("valid signer format rejected: %v", err)
		}
	}
	if _, err := decodeSolanaKeypairMaterial("not a key"); err == nil {
		t.Fatal("invalid key material accepted")
	}
}
