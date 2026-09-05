package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
		if err := database.RevalueAndMarkBroadcastIntent(ctx, rpc, operation); err != nil {
			var hold *BudgetHold
			if errors.As(err, &hold) {
				if journalErr := database.RecordPhase3SignedBudgetHold(ctx, operation.ID, hold); journalErr != nil {
					return errors.Join(err, journalErr)
				}
				var validated *validatedSignedBudgetHold
				if !errors.As(err, &validated) {
					return err
				}
				// A failed fresh valuation must not trap an expired, absent wire
				// in Signed forever. Release only after finalized expiry and a
				// subsequent explicit signature-absence observation; never resend.
				height, heightErr := rpc.FinalizedBlockHeight(ctx)
				if heightErr != nil {
					return errors.Join(err, heightErr)
				}
				if height > operation.LastValidBlockHeight {
					status, statusErr := rpc.SignatureStatus(ctx, operation.TransactionSignature)
					if statusErr != nil {
						return errors.Join(err, statusErr)
					}
					if status.Found {
						return database.MarkManualRecovery(ctx, operation.ID, Signed, "signed_budget_hold_signature_found")
					}
					return database.MarkExpiredAbsentFailed(ctx, operation.ID, Signed)
				}
			}
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
		height, err := rpc.FinalizedBlockHeight(ctx)
		if err != nil {
			return err
		}
		if height > operation.LastValidBlockHeight {
			// Recheck after finalized expiry. The earlier absence observation
			// may predate a last-valid-block landing. A malformed response or
			// any found signature retains the reservation and recovery fence.
			afterExpiry, err := rpc.SignatureStatus(ctx, operation.TransactionSignature)
			if err != nil {
				return err
			}
			if afterExpiry.Found {
				return nil
			}
			return database.MarkExpiredAbsentFailed(ctx, operation.ID, operation.Status)
		}
		return nil
	case Confirmed:
		return database.MarkReconciling(ctx, operation.ID)
	case Reconciling:
		status, err := rpc.SignatureStatus(ctx, operation.TransactionSignature)
		if err != nil {
			return err
		}
		if status.Failed {
			return database.MarkManualRecovery(ctx, operation.ID, Reconciling, "finalization_transaction_error")
		}
		if !status.Finalized {
			return nil
		}
		expected, err := DecodeExpectedEffects(operation.ExpectedEffects)
		if err != nil {
			return database.MarkManualRecovery(ctx, operation.ID, Reconciling, "invalid_expected_effects")
		}
		receipt, err := rpc.FinalizedTransaction(ctx, operation.TransactionSignature)
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
		return database.MarkReconciled(ctx, operation.ID, reconciliation, effects, receipt)
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
