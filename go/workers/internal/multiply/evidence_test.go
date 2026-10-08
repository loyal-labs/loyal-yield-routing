package multiply

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
)

// The anchors digest keeps its own shape while the row encoding of
// ExpectedEffects carries Rust's explicit nulls.
func TestFinancialAnchorsDigestIsIndependentOfRowEncoding(t *testing.T) {
	effects := ExpectedEffects{TokenDeltas: []TokenDelta{{Account: "a", Mint: "m", RawDelta: -5}}}
	digest, err := financialAnchorsHash(effects)
	if err != nil {
		t.Fatal(err)
	}
	legacy := sha256.Sum256([]byte(`{"tokenAmountsBefore":[],"tokenDeltas":[{"account":"a","mint":"m","rawDelta":-5}]}`))
	if digest != hexEncode(legacy[:]) {
		t.Fatalf("anchors digest drifted: %s", digest)
	}
	row, err := json.Marshal(effects)
	if err != nil || !strings.Contains(string(row), `"obligationDelta":null`) {
		t.Fatalf("row encoding must carry Rust's explicit nulls: %s %v", row, err)
	}
}
