package backyard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// landResendEvery matches the fleet landing cadence.
var landResendEvery = time.Second

// AdvanceNonterminal resumes the one durable operation before any new
// observation is permitted. Only Signed may start a submission: it records
// broadcast_intent before its first send. Signed, BroadcastIntent and Submitted
// all land the exact persisted wire, resending the same bytes until it lands,
// fails on chain or expires.
func AdvanceNonterminal(ctx context.Context, database *Database, rpc *chain.Client, view *View, operation PersistedOperation) error {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	return advanceNonterminalWithManifest(ctx, manifest, database, rpc, view, operation)
}

// advanceNonterminalWithManifest is the identical recovery state machine with
// the immutable reviewed manifest explicit: only the reconciliation decode,
// initializer receipt observation, reconciliation and locked settlement resolve
// through it. Every other state transition is unchanged.
func advanceNonterminalWithManifest(ctx context.Context, manifest RouteManifest, database *Database, rpc *chain.Client, view *View, operation PersistedOperation) error {
	if database == nil || rpc == nil || !IsNonterminal(operation.Status) {
		return fmt.Errorf("invalid nonterminal recovery input")
	}
	switch operation.Status {
	case Decided, Built, Simulated:
		reason, err := preBroadcastRecoveryReason(ctx, view, operation)
		if err != nil {
			return err
		}
		return database.MarkPreBroadcastFailed(ctx, operation.ID, operation.Status, reason)
	case Signed:
		if WithdrawalPreemptsOpenLoop(operation.Decision.Action, Signed, 1) {
			observation, err := ObserveConfirmedBridgeSnapshot(ctx, view, operation)
			if err != nil {
				return err
			}
			if WithdrawalPreemptsOpenLoop(operation.Decision.Action, Signed, observation.Snapshot.WithdrawalDemandRaw) {
				return database.MarkManualRecovery(ctx, operation.ID, Signed, "fresh_onchain_withdrawal_fence_required")
			}
		}
		if !persistedWireIntact(operation) {
			return database.MarkManualRecovery(ctx, operation.ID, Signed, "incomplete_persisted_signed_wire")
		}
		markIntent, err := database.finalSend(ctx, manifest, operation)
		if err != nil {
			return database.journalSignedHold(ctx, operation.ID, err)
		}
		return database.land(ctx, rpc, operation, markIntent)
	case BroadcastIntent, Submitted:
		// Submitted is no longer written; rows left in it by an older binary
		// resume exactly like broadcast_intent.
		if !persistedWireIntact(operation) {
			return database.MarkManualRecovery(ctx, operation.ID, operation.Status, "incomplete_submission_identity")
		}
		return database.land(ctx, rpc, operation, nil)
	case Confirmed:
		return database.MarkReconciling(ctx, operation.ID)
	case Reconciling:
		status, err := signatureStatus(ctx, rpc, operation.TransactionSignature)
		if err != nil {
			return err
		}
		if status.Failed {
			return database.MarkManualRecovery(ctx, operation.ID, Reconciling, "finalization_transaction_error")
		}
		if !status.Finalized {
			return nil
		}
		expected, err := decodeExpectedEffectsWithManifest(manifest, operation.ExpectedEffects)
		if err != nil {
			return database.MarkManualRecovery(ctx, operation.ID, Reconciling, "invalid_expected_effects")
		}
		var receipt ConfirmedTransactionEvidence
		if expected.Initialization != nil {
			receipt, err = manifest.observeFinalizedKaminoInitialization(ctx, rpc, *expected.Initialization, operation)
		} else {
			receipt, err = finalizedTransaction(ctx, rpc, operation.TransactionSignature)
		}
		if err != nil {
			return err
		}
		if receipt.Slot != operation.ConfirmedSlot {
			return database.MarkManualRecovery(ctx, operation.ID, Reconciling, "confirmed_transaction_slot_mismatch")
		}
		reconciliation, effects, err := manifest.ReconcileConfirmedTransaction(expected, receipt)
		if err != nil {
			return database.MarkManualRecovery(ctx, operation.ID, Reconciling, "exact_effect_reconciliation_failed")
		}
		return database.markReconciledOnManifest(ctx, manifest, operation.ID, reconciliation, effects, receipt)
	default:
		return fmt.Errorf("unsupported nonterminal status: %s", operation.Status)
	}
}

// persistedWireIntact checks the row's wire is the bytes it hashed and signs
// the signature it names, so landing and expiry are decided for these bytes.
func persistedWireIntact(operation PersistedOperation) bool {
	wire := operation.SignedWire
	return len(wire) > 65 && sha256Bytes(wire) == operation.SignedWireSHA256 &&
		operation.TransactionSignature == encodeBase58(wire[1:65]) &&
		operation.RecentBlockhash != "" && operation.LastValidBlockHeight > 0
}

// land resends the persisted wire through chain.Land until it lands, fails on
// chain or its blockhash expires. A signed row records broadcast intent behind
// markIntent's database fences before its first send; a row that already
// recorded it resends the same bytes, which cannot spend twice. A signed row
// stays signed while a fence holds, until its expiry proves the wire absent.
func (d *Database) land(ctx context.Context, rpc *chain.Client, operation PersistedOperation, markIntent func(context.Context) error) error {
	out, err := chain.Land(ctx, rpc, chain.Attempt{
		Wire: operation.SignedWire, Signature: operation.TransactionSignature,
		LastValidBlockHeight: uint64(operation.LastValidBlockHeight), Required: chain.Confirmed,
	}, landResendEvery, func(ctx context.Context) error {
		if operation.Status != Signed {
			return nil
		}
		if err := markIntent(ctx); err != nil {
			return d.journalSignedHold(ctx, operation.ID, err)
		}
		operation.Status = BroadcastIntent
		return nil
	})
	if err != nil {
		return unavailable(err)
	}
	switch {
	case out.Kind == chain.Expired:
		return d.MarkExpiredAbsentFailed(ctx, operation.ID, operation.Status)
	case operation.Status == Signed:
		// On chain although this worker never recorded a send of it.
		return d.MarkManualRecovery(ctx, operation.ID, Signed, "signed_signature_found_before_broadcast_intent")
	case out.Kind == chain.Failed:
		// A failed Solana transaction moves no funds. Decode the failure
		// before stopping capital: an adaptor report refusal is a liveness
		// termination, everything else stays a capital stop.
		return d.recoverConfirmedFailure(ctx, rpc, operation)
	}
	return d.MarkConfirmed(ctx, operation.ID, operation.Status, int64(out.Slot))
}

// journalSignedHold keeps a signed row's hold reason on the row; the wire
// stays signed and is never sent behind a hold.
func (d *Database) journalSignedHold(ctx context.Context, operationID string, err error) error {
	var hold *BudgetHold
	if errors.As(err, &hold) {
		if journalErr := d.RecordPhase3SignedBudgetHold(ctx, operationID, hold); journalErr != nil {
			return errors.Join(err, journalErr)
		}
	}
	return err
}

func preBroadcastRecoveryReason(ctx context.Context, view *View, operation PersistedOperation) (string, error) {
	if operation.Status != Decided && operation.Status != Built && operation.Status != Simulated {
		return "", fmt.Errorf("operation is not pre-broadcast")
	}
	if !WithdrawalPreemptsOpenLoop(operation.Decision.Action, operation.Status, 1) {
		return "prebroadcast_restart_reobserve_required", nil
	}
	observation, err := ObserveConfirmedBridgeSnapshot(ctx, view, operation)
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
