package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// Legacy adoption requires the actual finalized original packet, not rebuilding
// or signing a replacement. No signing bank context or 0090 attempt is invented.
func (w *LookupWorker) recoverLegacy(ctx context.Context, op LookupOperation, observedBank *uint64) error {
	if op.LegacySignature == nil || op.LegacyMessageHash == nil || op.LegacyBlockhash == nil || op.LegacyLastValidBlockHeight == nil {
		return w.store.deferLookupLegacy(ctx, op, "legacy packet identity incomplete")
	}
	status, err := w.chain.SignatureStatus(ctx, *op.LegacySignature)
	if err != nil {
		return err
	}
	if !status.Finalized {
		_, _, bank, err := w.chain.LookupBlockhash(ctx)
		if err != nil {
			return err
		}
		*observedBank = uint64(bank)
		return w.store.deferLookupLegacy(ctx, op, "legacy signature lacks finalized original receipt; retained")
	}
	receipt, err := w.chain.LookupFinalizedReceipt(ctx, *op.LegacySignature)
	if err != nil {
		return err
	}
	if receipt == nil {
		return w.store.deferLookupLegacy(ctx, op, "legacy original packet history unavailable; retained")
	}
	if op.PhysicalMutationEpoch != op.Intent.MutationEpoch {
		if receipt.Err != "" {
			return errors.New("legacy failed packet cannot explain projected membership")
		}
		original, err := lookupLegacyOriginalIntent(op)
		if err != nil {
			return err
		}
		op.Intent = original
	}
	if op.Intent.Kind == LookupClose {
		var deactivated *int64
		if err = w.store.pool.QueryRow(ctx, `SELECT deactivated_slot FROM loyal_yield.route_lookup_tables WHERE id=$1`, op.Intent.TableID).Scan(&deactivated); err != nil {
			return err
		}
		if deactivated == nil || *deactivated < 0 {
			return w.store.deferLookupLegacy(ctx, op, "legacy close has no retained original deactivation bank")
		}
		slot := uint64(*deactivated)
		op.Intent.ExpectedDeactivationSlot = &slot
	}
	hash := sha256.Sum256(receipt.Wire)
	attempt := LookupAttempt{Intent: op.Intent, Wire: WireIdentity{SignedTransaction: receipt.Wire, SignedTransactionHash: hex.EncodeToString(hash[:]), TransactionSignature: *op.LegacySignature, MessageHash: *op.LegacyMessageHash, RecentBlockhash: *op.LegacyBlockhash, LastValidBlockHeight: *op.LegacyLastValidBlockHeight}}
	if err = proveLookupWire(op.Intent, attempt.Wire); err != nil {
		return err
	}
	snapshot, err := w.chain.LookupSnapshot(ctx, op.Intent.TableAddress, receipt.Slot)
	if err != nil {
		return err
	}
	*observedBank = uint64(snapshot.Slot)
	recovery, err := recoverLookup(attempt, status, receipt, snapshot)
	if err != nil {
		return err
	}
	if recovery.proof == nil {
		return w.store.deferLookupLegacy(ctx, op, recovery.wait)
	}
	return w.store.commitLookupLegacyProof(ctx, op, attempt, recovery.proof)
}

func (s *Store) commitLookupLegacyProof(ctx context.Context, op LookupOperation, attempt LookupAttempt, proof *lookupProof) error {
	if attempt.ID != 0 || attempt.SigningContextSlot != 0 || proof == nil || proof.binding != lookupProofBinding(attempt) || (proof.state != LookupReconciled && proof.state != LookupFailed) || proof.finalizedSlot <= 0 || proof.readbackSlot < proof.finalizedSlot {
		return errors.New("legacy lookup requires exact finalized receipt/effect proof")
	}
	if err := proveLookupWire(attempt.Intent, attempt.Wire); err != nil {
		return err
	}
	var readback lookupEffectReadback
	var receipt lookupEffectReceipt
	if err := json.Unmarshal(proof.readback, &readback); err != nil {
		return err
	}
	if err := json.Unmarshal(proof.receipt, &receipt); err != nil {
		return err
	}
	if readback.Address != attempt.Intent.TableAddress || readback.ObservedSlot != proof.readbackSlot || receipt.Fee > math.MaxInt64 || receipt.TablePre > math.MaxInt64 || receipt.TablePost > math.MaxInt64 {
		return errors.New("legacy lookup proof/accounting identity invalid")
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		projected := op.PhysicalMutationEpoch == attempt.Intent.MutationEpoch+1
		physicalEpoch := attempt.Intent.MutationEpoch
		if projected {
			physicalEpoch++
		}
		if _, err := lookupLockSourceEpoch(ctx, tx, attempt.Intent, op.Lease, physicalEpoch); err != nil {
			return err
		}
		var matches bool
		err := tx.QueryRow(ctx, `SELECT transaction_signature=$2 AND message_hash=$3 AND recent_blockhash=$4 AND last_valid_block_height=$5 AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_signed_attempts WHERE operation_id=$1)
 FROM loyal_yield.lookup_table_operations WHERE id=$1`, op.Intent.OperationID, attempt.Wire.TransactionSignature, attempt.Wire.MessageHash, attempt.Wire.RecentBlockhash, attempt.Wire.LastValidBlockHeight).Scan(&matches)
		if err != nil {
			return err
		}
		if !matches {
			return errors.New("legacy lookup original packet identity changed or has owned journal")
		}
		if projected {
			if proof.state != LookupReconciled {
				return errors.New("legacy projection requires successful receipt")
			}
			if err = lookupLegacyProjectedMembership(ctx, tx, attempt.Intent, proof, readback); err != nil {
				return err
			}
		} else if err = lookupSourceMembership(ctx, tx, attempt.Intent); err != nil {
			return err
		}
		return finishLookupSourceProjectionTx(ctx, tx, op, attempt, proof, readback, receipt, &proof.finalizedSlot, projected)
	})
}

// Rust commits membership before advancing the operation lifecycle. Reconstruct
// only that single-epoch crash window; the original packet still proves intent.
func lookupLegacyOriginalIntent(op LookupOperation) (LookupIntent, error) {
	i := op.Intent
	if i.MutationEpoch == math.MaxInt64 || op.PhysicalMutationEpoch != i.MutationEpoch+1 || (i.Kind != LookupCreate && i.Kind != LookupRollover && i.Kind != LookupExtend) || len(i.Extension) == 0 || len(i.Prefix) < len(i.Extension) {
		return i, errors.New("legacy physical epoch is not the single growth receipt crash window")
	}
	end := len(i.Prefix) - len(i.Extension)
	if !lookupSameAddresses(i.Prefix[end:], i.Extension) {
		return i, errors.New("legacy projected suffix differs from original operation")
	}
	i.Prefix = append([]string{}, i.Prefix[:end]...)
	return i, nil
}

func lookupLegacyProjectedMembership(ctx context.Context, tx pgx.Tx, i LookupIntent, proof *lookupProof, readback lookupEffectReadback) error {
	full := append(append([]string{}, i.Prefix...), i.Extension...)
	projected := i
	projected.Prefix = full
	if err := lookupSourceMembership(ctx, tx, projected); err != nil {
		return err
	}
	if !lookupSameAddresses(full, readback.Addresses) || readback.LastExtendedSlot != uint64(proof.finalizedSlot) {
		return errors.New("legacy projected membership lacks original receipt bank")
	}
	var exact bool
	err := tx.QueryRow(ctx, `SELECT count(*)=$3 AND COALESCE(bool_and(added_operation_id=$2 AND added_slot=$4 AND usable_after_slot=$4::bigint+1),false) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1 AND ordinal >= $5`, i.TableID, i.OperationID, len(i.Extension), proof.finalizedSlot, len(i.Prefix)).Scan(&exact)
	if err != nil {
		return err
	}
	if !exact {
		return errors.New("legacy projected suffix provenance differs from finalized packet")
	}
	return nil
}
