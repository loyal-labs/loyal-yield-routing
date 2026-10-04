package fleetexec

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// BalanceAnchorEvidence is the durable receipt contract written with the
// signed wire. accountAddresses is the exact message account index->address
// resolution (static keys, then loaded writable, then loaded readonly
// addresses per selected table) captured at compile time; without it the
// receipt's balance indices cannot be interpreted.
type BalanceAnchorEvidence struct {
	AccountAddresses []string       `json:"account_addresses"`
	Anchors          []EffectAnchor `json:"anchors"`
}

// EffectAnchor demands one exact raw delta for one exact account and mint.
// A route that withdraws from a source reserve and deposits into a target
// reserve anchors the source liquidity vault at -amount and the target
// liquidity vault at +amount; quoted amounts or arbitrary balance changes
// never satisfy it.
type EffectAnchor struct {
	Account       string `json:"account"`
	Mint          string `json:"mint"`
	ExpectedDelta int64  `json:"expected_delta"`
	Decimals      uint8  `json:"decimals"`
}

// ParseBalanceAnchors decodes and validates the durable anchor evidence.
func ParseBalanceAnchors(raw json.RawMessage) (BalanceAnchorEvidence, error) {
	var evidence BalanceAnchorEvidence
	if len(raw) == 0 {
		return evidence, fmt.Errorf("missing balance anchor evidence")
	}
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return evidence, fmt.Errorf("decode balance anchors: %w", err)
	}
	if len(evidence.AccountAddresses) == 0 || len(evidence.Anchors) == 0 {
		return evidence, fmt.Errorf("incomplete balance anchor evidence")
	}
	seen := make(map[string]bool, len(evidence.Anchors))
	for _, anchor := range evidence.Anchors {
		if anchor.Account == "" || anchor.Mint == "" || anchor.ExpectedDelta == 0 {
			return evidence, fmt.Errorf("incomplete effect anchor")
		}
		if seen[anchor.Account+"|"+anchor.Mint] {
			return evidence, fmt.Errorf("duplicate anchor for %s", anchor.Account)
		}
		seen[anchor.Account+"|"+anchor.Mint] = true
	}
	return evidence, nil
}

// VerifyReceiptIdentity proves the chain receipt is exactly the durably
// persisted attempt: same message bytes, same leading signature, same slot,
// and a successful on-chain result. Anything else cannot confirm the route.
func VerifyReceiptIdentity(receipt *TransactionReceipt, record SubmissionRecord, confirmedSlot int64) error {
	if receipt == nil {
		return fmt.Errorf("receipt is missing")
	}
	if receipt.Signature != record.Signature {
		return fmt.Errorf("receipt signature %q differs from persisted identity", receipt.Signature)
	}
	if receipt.Slot != confirmedSlot {
		return fmt.Errorf("receipt slot %d differs from confirmed slot %d", receipt.Slot, confirmedSlot)
	}
	if receipt.Err != "" {
		return fmt.Errorf("receipt reports chain failure %s", receipt.Err)
	}
	message, err := base64.StdEncoding.DecodeString(receipt.MessageB64)
	if err != nil {
		return fmt.Errorf("decode receipt message: %w", err)
	}
	if len(record.SignedTransaction) == 0 || !bytes.Equal(receipt.SignedTransaction, record.SignedTransaction) {
		return fmt.Errorf("receipt transaction bytes differ from the persisted signed wire")
	}
	messageHash := sha256.Sum256(message)
	if hex.EncodeToString(messageHash[:]) != record.MessageHash {
		return fmt.Errorf("receipt message hash differs from the persisted signed wire")
	}
	return nil
}

// VerifyAnchoredEffects proves the receipt actually moved the anchored
// collateral/liquidity accounts by the exact expected raw amounts. Missing
// anchors, unknown pre-balances, decimal drift, or any off-by-one delta are
// reconciliation failures; success is never inferred from a later balance.
func VerifyAnchoredEffects(receipt *TransactionReceipt, evidence BalanceAnchorEvidence) error {
	deltas := make(map[string]TokenDelta, len(receipt.TokenDeltas))
	for _, delta := range receipt.TokenDeltas {
		deltas[delta.Account+"|"+delta.Mint] = delta
	}
	for _, anchor := range evidence.Anchors {
		delta, found := deltas[anchor.Account+"|"+anchor.Mint]
		if !found {
			return fmt.Errorf("receipt carries no %s balance evidence for anchored account %s", anchor.Mint, anchor.Account)
		}
		if delta.Decimals != anchor.Decimals {
			return fmt.Errorf("anchored account %s decimals %d differ from anchor %d", anchor.Account, delta.Decimals, anchor.Decimals)
		}
		if delta.PreRaw == nil || delta.PostRaw == nil {
			return fmt.Errorf("anchored account %s has unknown pre or post balance", anchor.Account)
		}
		// Checked arithmetic: deltas are computed in int64 space after both
		// raw balances are proven to fit.
		if *delta.PreRaw > uint64(1<<62) || *delta.PostRaw > uint64(1<<62) {
			return fmt.Errorf("anchored account %s balance exceeds checked bounds", anchor.Account)
		}
		actual := int64(*delta.PostRaw) - int64(*delta.PreRaw)
		if actual != anchor.ExpectedDelta {
			return fmt.Errorf("anchored account %s moved %d raw, expected exactly %d raw", anchor.Account, actual, anchor.ExpectedDelta)
		}
	}
	return nil
}
