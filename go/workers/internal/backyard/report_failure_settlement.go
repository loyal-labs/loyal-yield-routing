package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// This is a finalized, exact-wire failed transaction receipt, not an estimate
// of effects from logs. Failed execution rolls back atomically; additionally
// verify the complete native/token balance vector, including the fee payer's
// sole permitted debit.
type finalizedFailureReceipt struct {
	Slot        int64                 `json:"slot"`
	Transaction []string              `json:"transaction"`
	Meta        *finalizedFailureMeta `json:"meta"`
}

type finalizedFailureMeta struct {
	Err               json.RawMessage   `json:"err"`
	Fee               *uint64           `json:"fee"`
	PreBalances       []uint64          `json:"preBalances"`
	PostBalances      []uint64          `json:"postBalances"`
	PreTokenBalances  []json.RawMessage `json:"preTokenBalances"`
	PostTokenBalances []json.RawMessage `json:"postTokenBalances"`
	LogMessages       []string          `json:"logMessages"`
}

// readFailureReceipt reads a failed signature's finalized receipt in
// getTransaction's own shape, the form the settlement proof validates and
// persists. A receipt the cluster does not have yet, or one without an error,
// is an unavailable observation: the next tick lands the wire again.
func readFailureReceipt(ctx context.Context, rpc *chain.Client, signature string) (finalizedFailureReceipt, error) {
	r, err := finalizedReceipt(ctx, rpc, signature)
	if err != nil {
		return finalizedFailureReceipt{}, err
	}
	if r.Err == nil {
		return finalizedFailureReceipt{}, confirmedObservationUnavailable(fmt.Errorf("finalized receipt of a failed signature has no error"))
	}
	errJSON, err := json.Marshal(r.Err)
	if err != nil {
		return finalizedFailureReceipt{}, err
	}
	rows := func(balances map[solana.PublicKey]chain.TokenBalance) ([]json.RawMessage, error) {
		out := make([]json.RawMessage, 0, len(balances))
		for index, key := range r.Keys {
			balance, ok := balances[key]
			if !ok {
				continue
			}
			row, err := json.Marshal(map[string]any{"accountIndex": index, "mint": balance.Mint.String(), "owner": balance.Owner.String(), "programId": balance.Program.String(),
				"uiTokenAmount": map[string]any{"amount": strconv.FormatUint(balance.Amount, 10), "decimals": balance.Decimals}})
			if err != nil {
				return nil, err
			}
			out = append(out, row)
		}
		return out, nil
	}
	fee := r.Fee
	meta := &finalizedFailureMeta{Err: errJSON, Fee: &fee, PreBalances: r.PreLamports, PostBalances: r.PostLamports, LogMessages: r.Logs}
	if meta.PreTokenBalances, err = rows(r.Pre); err != nil {
		return finalizedFailureReceipt{}, err
	}
	if meta.PostTokenBalances, err = rows(r.Post); err != nil {
		return finalizedFailureReceipt{}, err
	}
	return finalizedFailureReceipt{Slot: int64(r.Slot), Transaction: []string{base64.StdEncoding.EncodeToString(r.Wire), "base64"}, Meta: meta}, nil
}

type finalizedFailureSettlement struct {
	Schema                  string                  `json:"schema"`
	Signature               string                  `json:"signature"`
	SignedWireSHA256        string                  `json:"signedWireSha256"`
	MessageSHA256           string                  `json:"messageSha256"`
	Slot                    int64                   `json:"slot"`
	Reason                  string                  `json:"reason"`
	AtomicNoCapitalMovement bool                    `json:"atomicNoCapitalMovement"`
	FeeLamports             uint64                  `json:"feeLamports"`
	Receipt                 finalizedFailureReceipt `json:"receipt"`
}

// validateFinalizedFailureReceipt proves a finalized failed receipt moved
// nothing but the fee: it is the exact persisted wire, signed by the delegate
// at key 0 under the persisted hashes; it carries an error; the fee payer was
// debited exactly the fee; every other account's lamports are unchanged; and
// the complete token balance rows after equal those before. It holds for any
// action and any error: a landed failure rolls back every instruction effect,
// and the balances are the evidence.
func validateFinalizedFailureReceipt(receipt finalizedFailureReceipt, wire []byte, signature, wireHash, messageHash string, expectedDelegate publicKey) error {
	fail := func() error { return budgetHold("unproven_finalized_failure_receipt") }
	if receipt.Slot <= 0 || receipt.Meta == nil || receipt.Meta.Fee == nil || *receipt.Meta.Fee == 0 ||
		len(receipt.Meta.Err) == 0 || string(receipt.Meta.Err) == "null" ||
		len(receipt.Transaction) != 2 || receipt.Transaction[1] != "base64" || sha256Bytes(wire) != wireHash {
		return fail()
	}
	landed, err := base64.StdEncoding.Strict().DecodeString(receipt.Transaction[0])
	if err != nil || !bytes.Equal(landed, wire) {
		return fail()
	}
	message, sig, err := splitSignedWire(wire)
	if err != nil {
		return fail()
	}
	m, err := decodeV1Message(message)
	if err != nil || m.Header.NumRequiredSignatures != 1 || len(m.AccountKeys) == 0 {
		return fail()
	}
	signer := publicKey(m.AccountKeys[0])
	if signer != expectedDelegate || !ed25519.Verify(signer[:], message, sig) || encodeBase58(sig) != signature || sha256Bytes(message) != messageHash {
		return fail()
	}
	// Every account key of the exact wire (a v1 message loads no others) has
	// balance metadata.
	count := len(m.AccountKeys)
	if len(receipt.Meta.PreBalances) != count || len(receipt.Meta.PostBalances) != count {
		return fail()
	}
	for i, pre := range receipt.Meta.PreBalances {
		post := receipt.Meta.PostBalances[i]
		if i == 0 {
			if pre < *receipt.Meta.Fee || post != pre-*receipt.Meta.Fee {
				return fail()
			}
		} else if pre != post {
			return fail()
		}
	}
	// Compare parsed complete rows, so ordering of JSON object keys is harmless,
	// while owner, mint, account index, token amount and decimals all stay bound.
	var pre, post any
	a, _ := json.Marshal(receipt.Meta.PreTokenBalances)
	b, _ := json.Marshal(receipt.Meta.PostTokenBalances)
	if json.Unmarshal(a, &pre) != nil || json.Unmarshal(b, &post) != nil || !reflect.DeepEqual(pre, post) {
		return fail()
	}
	return nil
}

// settleFinalizedFailure terminates one failed broadcast with its exact
// finalized receipt: when the receipt proves the persisted wire moved nothing
// but its fee, the row ends `failed` with that proof and the next tick
// re-observes. It reports false, changing nothing, when the receipt does not
// prove it. The automatic walk settles a row in its broadcast states; the
// operator's settle-manual-restore settles a manual row this package latched
// for a landed failure before the proof alone decided. delegate is the
// wire's signer: the pinned bridgeDelegate in production.
func (d *Database) settleFinalizedFailure(ctx context.Context, operation PersistedOperation, receipt finalizedFailureReceipt, reason string, delegate publicKey) (bool, error) {
	if d == nil || d.pool == nil || operation.ID == "" || reason == "" ||
		operation.Status != Submitted && operation.Status != BroadcastIntent && operation.Status != ManualRecovery {
		return false, fmt.Errorf("invalid finalized failure settlement")
	}
	var wire []byte
	var signature, wireHash, messageHash, status, recoveryReason string
	if err := d.pool.QueryRow(ctx, `SELECT signed_wire,COALESCE(transaction_signature,''),COALESCE(signed_wire_sha256,''),COALESCE(message_sha256,''),status,COALESCE(recovery_reason,'') FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operation.ID).Scan(&wire, &signature, &wireHash, &messageHash, &status, &recoveryReason); err != nil {
		return false, err
	}
	if status != string(operation.Status) || signature != operation.TransactionSignature {
		return false, budgetHold("failed_settlement_journal_changed")
	}
	// A manual row leaves through this settlement only when this package
	// latched it for a landed failure; any other recovery reason is a
	// different incident and stays manual.
	if status == string(ManualRecovery) && recoveryReason != unclassifiedTransactionErrReason {
		return false, budgetHold("failed_settlement_requires_landed_failure")
	}
	if validateFinalizedFailureReceipt(receipt, wire, signature, wireHash, messageHash, delegate) != nil {
		return false, nil
	}
	proof := finalizedFailureSettlement{Schema: "backyard-finalized-failure/v1", Signature: signature, SignedWireSHA256: wireHash, MessageSHA256: messageHash, Slot: receipt.Slot, Reason: reason, AtomicNoCapitalMovement: true, FeeLamports: *receipt.Meta.Fee, Receipt: receipt}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return false, err
	}
	result, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed',confirmation_status='finalized',confirmed_slot=$3,reconciliation_sha256=$4,reconciled_effects=$5::jsonb,recovery_reason=$6,updated_at=clock_timestamp() WHERE operation_id=$1 AND status=$2`, operation.ID, status, receipt.Slot, sha256Bytes(encoded), string(encoded), reason)
	if err != nil {
		return false, err
	}
	if result.RowsAffected() != 1 {
		return false, fmt.Errorf("failed settlement found no %s operation", status)
	}
	d.eventFailedAfterSend(ctx, operation.ID, reason)
	return true, nil
}
