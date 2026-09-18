package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPublicKeyBase58LeadingZerosMatchesSDK(t *testing.T) {
	var raw [][]byte
	var input []string
	for zeros := 0; zeros <= 32; zeros++ {
		key := make([]byte, 32)
		for i := zeros; i < 32; i++ {
			key[i] = byte(i + 1)
		}
		raw = append(raw, key)
		input = append(input, base64.StdEncoding.EncodeToString(key))
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bun", "testdata/base58-oracle.mjs")
	cmd.Stdin = bytes.NewReader(encoded)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("SDK base58 oracle: %v %s", err, output)
	}
	var expected []string
	if json.Unmarshal(output, &expected) != nil || len(expected) != len(raw) {
		t.Fatal("invalid SDK oracle output")
	}
	for i, key := range raw {
		got := encodeBase58(key)
		if got != expected[i] || strings.ContainsRune(got, 0) {
			t.Fatalf("base58 differs from SDK with %d leading zero bytes", i)
		}
		decoded, err := decodeKey(got)
		if err != nil || !bytes.Equal(decoded[:], key) {
			t.Fatalf("public key does not round trip with %d leading zeros: %v", i, err)
		}
	}
	if encodeBase58(nil) != "" || encodeBase58([]byte{0, 0, 1}) != "112" || encodeBase58([]byte{0, 255}) != "15Q" || encodeBase58(make([]byte, 64)) != strings.Repeat("1", 64) {
		t.Fatal("empty, non-key or zero-signature base58 encoding is not canonical")
	}
}
