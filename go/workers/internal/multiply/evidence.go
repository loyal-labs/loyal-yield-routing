package multiply

import (
	"crypto/sha256"
	"encoding/json"
)

// financialAnchorsHash digests the anchors in a fixed shape of their own
// (absent obligation fields omitted), so the row encoding of ExpectedEffects
// can match Rust's explicit nulls without changing the digest.
func financialAnchorsHash(effects ExpectedEffects) (string, error) {
	anchors := struct {
		TokenAmountsBefore []TokenAmountBefore `json:"tokenAmountsBefore"`
		TokenDeltas        []TokenDelta        `json:"tokenDeltas"`
		ObligationBefore   *ObligationBefore   `json:"obligationBefore,omitempty"`
		ObligationDelta    *ObligationDelta    `json:"obligationDelta,omitempty"`
	}{effects.TokenAmountsBefore, effects.TokenDeltas, effects.ObligationBefore, effects.ObligationDelta}
	if anchors.TokenAmountsBefore == nil {
		anchors.TokenAmountsBefore = []TokenAmountBefore{}
	}
	if anchors.TokenDeltas == nil {
		anchors.TokenDeltas = []TokenDelta{}
	}
	raw, err := json.Marshal(anchors)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hexEncode(digest[:]), nil
}
