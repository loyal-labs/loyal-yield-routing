package multiply

import (
	"crypto/sha256"
	"encoding/json"
)

func financialAnchorsHash(effects ExpectedEffects) (string, error) {
	raw, err := json.Marshal(effects)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hexEncode(digest[:]), nil
}
