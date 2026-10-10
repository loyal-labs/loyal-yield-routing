package backyard

import (
	"bytes"
	"strings"
	"testing"
)

func TestPublicKeyBase58LeadingZerosMatchesSDK(t *testing.T) {
	t.Parallel()
	expected := sdkVectors(t).Base58LeadingZeros.Keys
	if len(expected) != 33 {
		t.Fatal("invalid SDK base58 vectors")
	}
	for zeros := 0; zeros <= 32; zeros++ {
		key := make([]byte, 32)
		for i := zeros; i < 32; i++ {
			key[i] = byte(i + 1)
		}
		got := encodeBase58(key)
		if got != expected[zeros] || strings.ContainsRune(got, 0) {
			t.Fatalf("base58 differs from SDK with %d leading zero bytes", zeros)
		}
		decoded, err := decodeKey(got)
		if err != nil || !bytes.Equal(decoded[:], key) {
			t.Fatalf("public key does not round trip with %d leading zeros: %v", zeros, err)
		}
	}
	if encodeBase58(nil) != "" || encodeBase58([]byte{0, 0, 1}) != "112" || encodeBase58([]byte{0, 255}) != "15Q" || encodeBase58(make([]byte, 64)) != strings.Repeat("1", 64) {
		t.Fatal("empty, non-key or zero-signature base58 encoding is not canonical")
	}
}
