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

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// This is a finalized, exact-wire failed transaction receipt, not an estimate
// of effects from logs. Failed instruction execution rolls back atomically;
// additionally verify the complete native/token balance vector, including the
// fee payer's sole permitted debit. The signed legacy envelope excludes nonce
// advancement and pins the worker's Squads execution program.
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

func validateFinalizedFailureReceipt(receipt finalizedFailureReceipt, wire []byte, signature, wireHash, messageHash string, expectedDelegate publicKey, effects ExpectedEffects) error {
	fail := func() error { return budgetHold("unproven_finalized_failure_receipt") }
	if receipt.Slot <= 0 || receipt.Meta == nil || receipt.Meta.Fee == nil || *receipt.Meta.Fee == 0 ||
		len(receipt.Transaction) != 2 || receipt.Transaction[1] != "base64" || sha256Bytes(wire) != wireHash {
		return fail()
	}
	landed, err := base64.StdEncoding.Strict().DecodeString(receipt.Transaction[0])
	if err != nil || !bytes.Equal(landed, wire) {
		return fail()
	}
	sig, message, _, signer, err := decodeExactLegacyWire(wire)
	if err != nil || !ed25519.Verify(signer[:], message, sig) || encodeBase58(sig) != signature || sha256Bytes(message) != messageHash || signer != expectedDelegate {
		return fail()
	}
	var instructionError struct {
		InstructionError []json.RawMessage `json:"InstructionError"`
	}
	if json.Unmarshal(receipt.Meta.Err, &instructionError) != nil || len(instructionError.InstructionError) != 2 {
		return fail()
	}
	// Every account key in the exact legacy wire must have balance metadata.
	offset := 3
	count, err := decodeShortVec(message, &offset)
	if err != nil || count <= 0 || len(receipt.Meta.PreBalances) != count || len(receipt.Meta.PostBalances) != count {
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
	if len(effects.Accounts) != 3 || len(receipt.Meta.PreTokenBalances) != 3 || len(receipt.Meta.PostTokenBalances) != 3 {
		return fail()
	}
	canonical := map[string]string{bridgeIdleATA: bridgeIdleAuthority, bridgeStrategyATA: bridgeStrategyAuth, bridgeSquadsATA: bridgeVault}
	want := make(map[int]ExpectedAccountEffect, 3)
	for _, effect := range effects.Accounts {
		if canonical[effect.Address] == "" || effect.Authority != canonical[effect.Address] || effect.Owner != bridgeTokenProgram || effect.Mint != bridgeUSDC || effect.BeforeRaw != effect.AfterRaw {
			return fail()
		}
		delete(canonical, effect.Address)
		found := -1
		for i := 0; i < count; i++ {
			if keyString(message[offset+i*32:offset+(i+1)*32]) == effect.Address {
				found = i
				break
			}
		}
		if found < 0 {
			return fail()
		}
		want[found] = effect
	}
	for _, raw := range receipt.Meta.PreTokenBalances {
		var token struct {
			AccountIndex int    `json:"accountIndex"`
			Mint         string `json:"mint"`
			Owner        string `json:"owner"`
			ProgramID    string `json:"programId"`
			UI           struct {
				Amount   string `json:"amount"`
				Decimals int    `json:"decimals"`
			} `json:"uiTokenAmount"`
		}
		if json.Unmarshal(raw, &token) != nil {
			return fail()
		}
		effect, ok := want[token.AccountIndex]
		if !ok || token.Mint != effect.Mint || token.Owner != effect.Authority || token.ProgramID != effect.Owner || token.UI.Decimals != 6 || token.UI.Amount != strconv.FormatUint(effect.BeforeRaw, 10) {
			return fail()
		}
		delete(want, token.AccountIndex)
	}
	if len(want) != 0 {
		return fail()
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

// settleFinalizedReportFailure terminates one classified report-bearing
// failure with the exact finalized receipt: the landed wire is byte-identical
// to the persisted wire and moved no capital, and the terminal `failed` row
// lets the next tick re-observe. Both
// report-bearing bridge wires are admitted: the report-only NAV refresh in
// every retryable state, and the VOLTR_RESTORE_IDLE wire only for the
// adaptor's report-age refusal — in the automatic walk's broadcast states and,
// through the scoped operator entrypoint (manualRecovery=true), in the
// explicit manual state for rows the previous binary's action gate forced
// into manual recovery with the unclassified marker. Same receipt proof, one
// guarded status transition per source state.
func (d *Database) settleFinalizedReportFailure(ctx context.Context, operation PersistedOperation, receipt finalizedFailureReceipt, reason string, manualRecovery bool) error {
	if d == nil || d.pool == nil || operation.ID == "" {
		return fmt.Errorf("invalid finalized failure settlement")
	}
	if manualRecovery {
		// The operator path admits exactly the restore rows this package
		// itself forced into manual recovery (recoverConfirmedFailure's old
		// action gate); recovery_reason is re-verified from the row below.
		if operation.Status != ManualRecovery || operation.Decision.Action != VoltrRestoreIdle {
			return fmt.Errorf("invalid manual report failure settlement")
		}
	} else if operation.Status != Submitted && operation.Status != BroadcastIntent {
		return fmt.Errorf("invalid finalized failure settlement")
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = d.lockOperationLease(ctx, tx, operation.ID); err != nil {
		return err
	}
	auth, err := readPhase3AuthorizationTx(ctx, tx, operation.ID)
	if err != nil {
		return err
	}
	var wire []byte
	var signature, wireHash, messageHash, action, status, recoveryReason string
	if err = tx.QueryRow(ctx, `SELECT signed_wire,transaction_signature,signed_wire_sha256,message_sha256,action,status,COALESCE(recovery_reason,'') FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operation.ID).Scan(&wire, &signature, &wireHash, &messageHash, &action, &status, &recoveryReason); err != nil {
		return err
	}
	if status != string(operation.Status) || signature != operation.TransactionSignature {
		return budgetHold("failed_settlement_journal_changed")
	}
	// Only a row the old binary stamped with the bare unclassified marker —
	// a classified retryable refusal that the action gate then forced manual —
	// may leave manual recovery through this settlement. Any other recovery
	// reason is a different incident and stays manual.
	if manualRecovery && recoveryReason != unclassifiedTransactionErrReason {
		return budgetHold("failed_settlement_requires_exact_report")
	}
	request, effects, message, err := auth.BuildInput.decode()
	if err != nil {
		return err
	}
	bridge, ok := request.(BridgeBuildRequest)
	// Scope fee settlement to the two report-bearing bridge wires: the
	// report-only NAV refresh, and the restore — the latter only through the
	// operator path and only for the adaptor's report-age refusal. The
	// compiled request must be the same action the journal row records, and
	// its compiled message must hash to the persisted message digest.
	restore := ok && bridge.Action == VoltrRestoreIdle
	// The restore is admitted from the automatic walk's broadcast states and
	// the explicit manual state alike — in both cases only for the ReportSlot
	// refusal, whose receipt proves the report was refused before the Voltr
	// CPI and therefore that no capital moved.
	if !ok || action != string(bridge.Action) || sha256Bytes(message) != messageHash ||
		(bridge.Action != ReportNAV && !restore) ||
		(restore && reason != adaptorReportSlotRefusedReason) {
		return budgetHold("failed_settlement_requires_exact_report")
	}
	// The restore's persisted effects predict successful capital movement
	// (custody down, idle up). The receipt must instead prove the exact wire
	// rolled back, so validate against their rollback form — every AfterRaw
	// replaced by its own BeforeRaw — and bind the compiled message to the
	// saved exact wire bytes. The report-only validator itself is unchanged.
	receiptEffects := effects
	if restore {
		if len(wire) <= ed25519.SignatureSize+1 || wire[0] != 1 || !bytes.Equal(message, wire[ed25519.SignatureSize+1:]) {
			return budgetHold("failed_settlement_requires_exact_report")
		}
		rollback := make([]ExpectedAccountEffect, len(effects.Accounts))
		for index, account := range effects.Accounts {
			account.AfterRaw = account.BeforeRaw
			rollback[index] = account
		}
		receiptEffects.Accounts = rollback
	}
	if err = validateFinalizedFailureReceipt(receipt, wire, signature, wireHash, messageHash, mustKey(bridgeDelegate), receiptEffects); err != nil {
		return err
	}
	intent, err := Phase3IntentDigest(request, auth.BuildInput.Effects)
	if err != nil || intent != auth.IntentSHA256 || auth.SignedWireSHA256 != wireHash {
		return budgetHold("failed_settlement_intent_changed")
	}
	proof := finalizedFailureSettlement{Schema: "backyard-finalized-failure/v1", Signature: signature, SignedWireSHA256: wireHash, MessageSHA256: messageHash, Slot: receipt.Slot, Reason: reason, AtomicNoCapitalMovement: true, FeeLamports: *receipt.Meta.Fee, Receipt: receipt}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed',confirmation_status='finalized',confirmed_slot=$3,reconciliation_sha256=$4,reconciled_effects=$5::jsonb,recovery_reason=$6,updated_at=clock_timestamp() WHERE operation_id=$1 AND status=$2`, operation.ID, status, receipt.Slot, sha256Bytes(encoded), string(encoded), reason)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("failed settlement lost serialization")
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	d.eventFailedAfterSend(ctx, operation.ID, reason)
	return nil
}
