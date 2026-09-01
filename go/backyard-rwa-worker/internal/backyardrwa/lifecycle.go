package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// AdvanceNonterminal resumes the one durable operation before any new
// observation is permitted. Only Signed may create a new submission: it first
// records broadcast_intent and then sends its exact persisted wire once.
// BroadcastIntent and Submitted are recovery states and never resend.
func AdvanceNonterminal(ctx context.Context, database *Database, rpc *RPCClient, operation PersistedOperation) error {
	if database == nil || rpc == nil || !IsNonterminal(operation.Status) {
		return fmt.Errorf("invalid nonterminal recovery input")
	}
	switch operation.Status {
	case Decided, Built, Simulated:
		reason, err := preBroadcastRecoveryReason(ctx, rpc, operation)
		if err != nil {
			return err
		}
		return database.MarkPreBroadcastFailed(ctx, operation.ID, operation.Status, reason)
	case Signed:
		if WithdrawalPreemptsOpenLoop(operation.Decision.Action, Signed, 1) {
			observation, err := ObserveConfirmedBridgeSnapshot(ctx, rpc)
			if err != nil {
				return err
			}
			if WithdrawalPreemptsOpenLoop(operation.Decision.Action, Signed, observation.Snapshot.WithdrawalDemandRaw) {
				return database.MarkManualRecovery(ctx, operation.ID, Signed, "fresh_onchain_withdrawal_fence_required")
			}
		}
		wireHash := sha256Bytes(operation.SignedWire)
		if len(operation.SignedWire) == 0 || wireHash != operation.SignedWireSHA256 ||
			operation.TransactionSignature == "" || operation.RecentBlockhash == "" || operation.LastValidBlockHeight <= 0 {
			return database.MarkManualRecovery(ctx, operation.ID, Signed, "incomplete_persisted_signed_wire")
		}
		if err := database.MarkBroadcastIntent(ctx, operation.ID); err != nil {
			return err
		}
		if _, err := rpc.SendSignedTransactionOnce(ctx, operation.SignedWire, operation.TransactionSignature); err != nil {
			// The RPC response is ambiguous. Keep broadcast_intent durable and let
			// the next iteration recover the signature from chain; never resend.
			return fmt.Errorf("ambiguous send after durable broadcast intent: %w", err)
		}
		return database.MarkSubmitted(ctx, operation.ID)
	case BroadcastIntent, Submitted:
		if operation.TransactionSignature == "" || operation.LastValidBlockHeight <= 0 {
			return database.MarkManualRecovery(ctx, operation.ID, operation.Status, "incomplete_submission_identity")
		}
		status, err := rpc.SignatureStatus(ctx, operation.TransactionSignature)
		if err != nil {
			return err
		}
		if status.Failed {
			return database.MarkManualRecovery(ctx, operation.ID, operation.Status, "confirmed_transaction_error")
		}
		if status.Confirmed {
			return database.MarkConfirmed(ctx, operation.ID, operation.Status, status.ConfirmationSlot)
		}
		if status.Found {
			// A processed signature may still reach confirmed after its blockhash
			// expires. Keep observing it; expiry is only decisive when absent.
			return nil
		}
		height, err := rpc.ConfirmedBlockHeight(ctx)
		if err != nil {
			return err
		}
		if height > operation.LastValidBlockHeight {
			return database.MarkManualRecovery(ctx, operation.ID, operation.Status, "signature_absent_after_blockhash_expiry")
		}
		return nil
	case Confirmed:
		return database.MarkReconciling(ctx, operation.ID)
	case Reconciling:
		expected, err := DecodeExpectedEffects(operation.ExpectedEffects)
		if err != nil {
			return database.MarkManualRecovery(ctx, operation.ID, Reconciling, "invalid_expected_effects")
		}
		receipt, err := rpc.ConfirmedTransaction(ctx, operation.TransactionSignature)
		if err != nil {
			return err
		}
		if receipt.Slot != operation.ConfirmedSlot {
			return database.MarkManualRecovery(ctx, operation.ID, Reconciling, "confirmed_transaction_slot_mismatch")
		}
		reconciliation, effects, err := ReconcileConfirmedTransaction(expected, receipt)
		if err != nil {
			return database.MarkManualRecovery(ctx, operation.ID, Reconciling, "exact_effect_reconciliation_failed")
		}
		return database.MarkReconciled(ctx, operation.ID, reconciliation, effects)
	default:
		return fmt.Errorf("unsupported nonterminal status: %s", operation.Status)
	}
}

func preBroadcastRecoveryReason(ctx context.Context, rpc *RPCClient, operation PersistedOperation) (string, error) {
	if operation.Status != Decided && operation.Status != Built && operation.Status != Simulated {
		return "", fmt.Errorf("operation is not pre-broadcast")
	}
	if !WithdrawalPreemptsOpenLoop(operation.Decision.Action, operation.Status, 1) {
		return "prebroadcast_restart_reobserve_required", nil
	}
	observation, err := ObserveConfirmedBridgeSnapshot(ctx, rpc)
	if err != nil {
		return "", err
	}
	if WithdrawalPreemptsOpenLoop(operation.Decision.Action, operation.Status, observation.Snapshot.WithdrawalDemandRaw) {
		return "prebroadcast_withdrawal_preempted", nil
	}
	return "prebroadcast_restart_reobserve_required", nil
}

func sha256Bytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
