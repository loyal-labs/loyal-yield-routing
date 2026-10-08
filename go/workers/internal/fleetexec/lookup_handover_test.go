package fleetexec

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/mr-tron/base58"
)

func TestLookupBytesFromBeforeARustResignAreNotLanded(t *testing.T) {
	// Go -> Rust -> Go: Rust's retry re-signs the operation and writes new
	// signature columns but leaves our signedTransaction key behind. Those
	// bytes are a dead packet; proving them against the new signature failed
	// on every tick while Rust's packet waited unbound.
	old := append([]byte{1}, make([]byte, 64)...)
	old[1] = 7
	context, _ := json.Marshal(map[string]any{"signedTransaction": base64.StdEncoding.EncodeToString(append(old, 0x80)), "signingContextSlot": 90, "broadcastCount": 3})
	rustSignature := base58.Encode(make([]byte, 64))
	hash, blockhash, height := "m", "b", int64(500)
	attempt, err := lookupAttemptOf(LookupOperation{Signature: &rustSignature, MessageHash: &hash, Blockhash: &blockhash, LastValidBlockHeight: &height, Context: context})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempt.Wire.SignedTransaction) != 0 || attempt.BroadcastCount != 0 || attempt.Wire.TransactionSignature != rustSignature || attempt.Wire.LastValidBlockHeight != height {
		t.Fatalf("attempt %+v; Rust's packet must be landed by its signature alone", attempt)
	}
}
