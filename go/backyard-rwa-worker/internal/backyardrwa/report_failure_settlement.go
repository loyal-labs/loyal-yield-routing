package backyardrwa

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
)

// This is a finalized, exact-wire failed transaction receipt, not an estimate
// of effects from logs. Failed instruction execution rolls back atomically;
// additionally verify the complete native/token balance vector, including the
// fee payer's sole permitted debit. The signed legacy envelope excludes nonce
// advancement and pins the worker's Squads execution program.
type finalizedFailureReceipt struct {
	Slot        int64    `json:"slot"`
	Transaction []string `json:"transaction"`
	Meta        *struct {
		Err               json.RawMessage   `json:"err"`
		Fee               *uint64           `json:"fee"`
		PreBalances       []uint64          `json:"preBalances"`
		PostBalances      []uint64          `json:"postBalances"`
		PreTokenBalances  []json.RawMessage `json:"preTokenBalances"`
		PostTokenBalances []json.RawMessage `json:"postTokenBalances"`
		LogMessages       []string          `json:"logMessages"`
	} `json:"meta"`
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
	BookedFeeMicros         int64                   `json:"bookedFeeMicros"`
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

// Charge only the independently measured fee, conservatively valued by the
// existing send authorization's SOL upper bound. Historic prices are checked
// at their original admission slot, never relabeled as a fresh observation.
func settleFailedFeeBudget(budget Phase3Budget, auth phase3OperationAuthorization, operationID string, proof *finalizedFailureSettlement) (Phase3Budget, phase3OperationAuthorization, error) {
	fail := func() (Phase3Budget, phase3OperationAuthorization, error) {
		return budget, auth, budgetHold("unproven_failed_fee_settlement")
	}
	if err := budget.validate(); err != nil {
		return budget, auth, err
	}
	r, ok := budget.Reservations[operationID]
	if !ok || auth.GoalID != Phase3GoalID || auth.ReservationReleased || auth.BookedSpentMicros != 0 || auth.BookedExecutionCostMicros != 0 || r.IntentSHA256 != auth.IntentSHA256 || auth.SignedWireSHA256 != proof.SignedWireSHA256 || auth.SendKnownCost == nil || !proof.AtomicNoCapitalMovement {
		return fail()
	}
	cost := auth.SendKnownCost
	if cost.MessageSHA256 != proof.MessageSHA256 || cost.Fee.MessageSHA256 != proof.MessageSHA256 || cost.NativePrice.Decimals != 9 || cost.Fee.Slot <= 0 || cost.Fee.Slot > cost.ObservationSlot || cost.ObservationSlot-cost.Fee.Slot > budgetMaxObservationLagSlots || proof.FeeLamports == 0 || proof.FeeLamports > cost.Fee.Lamports || proof.Slot < cost.ObservationSlot {
		return fail()
	}
	quoted, err := cost.NativePrice.valueUpper(cost.Fee.Lamports, nativeSOLBudgetAsset, "11111111111111111111111111111111", cost.ObservationSlot)
	if err != nil || quoted != cost.NetworkFeeMicros {
		return fail()
	}
	fee, err := cost.NativePrice.valueUpper(proof.FeeLamports, nativeSOLBudgetAsset, "11111111111111111111111111111111", cost.ObservationSlot)
	if err != nil || fee <= 0 || fee > r.UpperMicros || (budget.Pilot != nil && fee > r.ExecutionCostUpperMicros) {
		return fail()
	}
	row := budget.Families[r.Family]
	if row.ExitMicros != r.ExitAfterMicros {
		return fail()
	}
	row.SpentMicros, err = budgetSum(row.SpentMicros, fee)
	if err != nil {
		return fail()
	}
	if budget.Pilot != nil {
		row.ExecutionCostSpentMicros, err = budgetSum(row.ExecutionCostSpentMicros, fee)
		if err != nil {
			return fail()
		}
	}
	row.ExitMicros = r.ExitBeforeMicros
	// Do not mutate caller maps until the entire replacement budget validates.
	next := budget
	next.Families = make(map[string]FamilyBudget, len(budget.Families))
	for k, v := range budget.Families {
		next.Families[k] = v
	}
	next.Reservations = make(map[string]BudgetReservation, len(budget.Reservations))
	for k, v := range budget.Reservations {
		next.Reservations[k] = v
	}
	next.Families[r.Family] = row
	delete(next.Reservations, operationID)
	if err = next.validate(); err != nil {
		return budget, auth, err
	}
	auth.BookedSpentMicros = fee
	if budget.Pilot != nil {
		auth.BookedExecutionCostMicros = fee
	}
	proof.BookedFeeMicros = fee
	return next, auth, nil
}

func (d *Database) settleFinalizedReportFailure(ctx context.Context, rpc *RPCClient, operation PersistedOperation, reason string) error {
	// A missing detailed receipt stays ambiguous even after finalized status.
	var receipt finalizedFailureReceipt
	if err := rpc.call(ctx, "getTransaction", []any{operation.TransactionSignature, map[string]any{"commitment": "finalized", "encoding": "base64", "maxSupportedTransactionVersion": 0}}, &receipt); err != nil {
		return nil
	}
	if receipt.Meta == nil || len(receipt.Transaction) != 2 {
		return nil
	}
	if d == nil || d.pool == nil || operation.ID == "" || (operation.Status != Submitted && operation.Status != BroadcastIntent) {
		return fmt.Errorf("invalid finalized failure settlement")
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, operation.ID)
	if err != nil {
		return err
	}
	var wire []byte
	var signature, wireHash, messageHash, action, status string
	if err = tx.QueryRow(ctx, `SELECT signed_wire,transaction_signature,signed_wire_sha256,message_sha256,action,status FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operation.ID).Scan(&wire, &signature, &wireHash, &messageHash, &action, &status); err != nil {
		return err
	}
	if status != string(operation.Status) || signature != operation.TransactionSignature {
		return budgetHold("failed_settlement_journal_changed")
	}
	request, effects, message, err := auth.BuildInput.decode()
	if err != nil {
		return err
	}
	if err = validateFinalizedFailureReceipt(receipt, wire, signature, wireHash, messageHash, mustKey(bridgeDelegate), effects); err != nil {
		return err
	}
	intent, err := Phase3IntentDigest(request, auth.BuildInput.Effects)
	if err != nil || intent != auth.IntentSHA256 {
		return budgetHold("failed_settlement_intent_changed")
	}
	bridge, ok := request.(BridgeBuildRequest)
	// Scope automatic fee settlement to the report-only wire being recovered.
	// Other action families need their own exact-envelope proof before adoption.
	if !ok || bridge.Action != ReportNAV || action != string(ReportNAV) || sha256Bytes(message) != messageHash {
		return budgetHold("failed_settlement_requires_exact_report")
	}
	classification := ClassifyConfirmedReportFailure(receipt.Meta.Err, receipt.Meta.LogMessages)
	if !classification.Retryable && ReportExpiredAtLanding(int64(bridge.Report.ObservedSlot), receipt.Slot) {
		classification = ConfirmedFailureClassification{Retryable: true, Reason: "report_expired_at_landing"}
	}
	if !classification.Retryable || classification.Reason != reason {
		return budgetHold("failed_settlement_classification_changed")
	}
	proof := finalizedFailureSettlement{Schema: "backyard-finalized-failure/v1", Signature: signature, SignedWireSHA256: wireHash, MessageSHA256: messageHash, Slot: receipt.Slot, Reason: reason, AtomicNoCapitalMovement: true, FeeLamports: *receipt.Meta.Fee, Receipt: receipt}
	budget, auth, err = settleFailedFeeBudget(budget, auth, operation.ID, &proof)
	if err != nil {
		return err
	}
	if err = d.writePhase3BudgetTx(ctx, tx, operation.ID, budget, auth); err != nil {
		return err
	}
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
	return tx.Commit(ctx)
}
