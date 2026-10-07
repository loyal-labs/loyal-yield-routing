package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
)

// RouteFeePayer is the fee payer of one vault's mature same-mint route: a
// pure function of the vault and the fixed fee-only payer list. Every route
// write-locks its fee payer, so one shared payer serializes the whole fleet
// within a block; spreading vaults over a fixed list removes that lock
// without changing who signs the vault's policy. With no list the policy
// signer pays, as before. The score is the Rust worker's
// fee_payer_rendezvous_score, so both choose the same payer for a vault.
func RouteFeePayer(cluster, vault, policySigner string, feeOnly []string) string {
	payer, best := policySigner, []byte(nil)
	for _, candidate := range feeOnly {
		key, err := decodePublicKey(candidate)
		if err != nil {
			continue
		}
		h := sha256.New()
		h.Write([]byte(cluster))
		h.Write([]byte{0})
		h.Write([]byte(vault))
		h.Write([]byte{0})
		h.Write(key[:])
		score := h.Sum(nil)
		if c := bytes.Compare(score, best); best == nil || c > 0 || c == 0 && candidate < payer {
			payer, best = candidate, score
		}
	}
	return payer
}

// matureReservePosition reports whether a same-mint route moves an existing
// reserve position. Rust allows a fee-only payer only for these routes: idle
// and setup work pays rent, which a fee-only key never funds.
func matureReservePosition(plan json.RawMessage) bool {
	var p struct {
		SourceKind string `json:"source_kind"`
	}
	return json.Unmarshal(plan, &p) == nil && p.SourceKind == "reserve_position"
}
