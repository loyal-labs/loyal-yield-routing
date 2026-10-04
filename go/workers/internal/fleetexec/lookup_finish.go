package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

type lookupEffectReadback struct {
	Address, Owner, Authority string
	ObservedSlot              int64 `json:"observed_slot"`
	Absent                    bool
	Addresses                 []string `json:"ordered_addresses"`
	LastExtendedSlot          uint64   `json:"last_extended_slot"`
}
type lookupEffectReceipt struct {
	Fee       uint64 `json:"fee_lamports"`
	TablePre  uint64 `json:"table_pre_lamports"`
	TablePost uint64 `json:"table_post_lamports"`
}

// commitLookupProof resolves packet ownership before source projections in the
// same short transaction. Terminal source flags never produce this proof.
func (s *Store) commitLookupProof(ctx context.Context, operation LookupOperation, attempt LookupAttempt, proof *lookupProof) error {
	if proof == nil || proof.binding != lookupProofBinding(attempt) || proof.readbackSlot < attempt.SigningContextSlot || (proof.state != LookupReconciled && proof.state != LookupFailed && proof.state != LookupExpired) {
		return errors.New("lookup terminal proof is missing or belongs to another packet")
	}
	var readback lookupEffectReadback
	if err := json.Unmarshal(proof.readback, &readback); err != nil {
		return err
	}
	if readback.Address != attempt.Intent.TableAddress || readback.ObservedSlot != proof.readbackSlot {
		return errors.New("lookup readback proof identity changed")
	}
	var receipt lookupEffectReceipt
	if proof.state != LookupExpired {
		if err := json.Unmarshal(proof.receipt, &receipt); err != nil {
			return err
		}
	}
	if receipt.Fee > math.MaxInt64 || receipt.TablePre > math.MaxInt64 || receipt.TablePost > math.MaxInt64 {
		return errors.New("lookup actual SOL accounting exceeds durable range")
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := lookupLockSource(ctx, tx, attempt.Intent, operation.Lease)
		if err != nil {
			return err
		}
		current, err := scanLookupAttempt(tx.QueryRow(ctx, `SELECT `+lookupAttemptColumns+` FROM loyal_yield.lookup_table_signed_attempts WHERE id=$1 FOR UPDATE`, attempt.ID))
		if err != nil {
			return err
		}
		if lookupProofBinding(current) != proof.binding {
			return errors.New("lookup proof no longer matches durable packet")
		}
		if current.State == LookupReconciled || current.State == LookupFailed || current.State == LookupExpired {
			return errors.New("lookup packet already resolved")
		}
		proofKind := "effect"
		if proof.state == LookupFailed {
			proofKind = "failed_receipt"
		}
		if proof.state == LookupExpired {
			proofKind = "no_effect"
		}
		var finalized, history, height *int64
		if proof.finalizedSlot > 0 {
			finalized = &proof.finalizedSlot
		}
		if proof.historySlot > 0 {
			history = &proof.historySlot
		}
		if proof.blockHeight > 0 {
			height = &proof.blockHeight
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_signed_attempts SET attempt_state=$2,finalized_slot=$3,readback_slot=$4,history_context_slot=$5,history_complete=$6,observed_block_height=$7,proof_kind=$8,readback_evidence=$9,receipt_evidence=$10,last_status_checked_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, attempt.ID, proof.state, finalized, proof.readbackSlot, history, proof.historyComplete, height, proofKind, proof.readback, proof.receipt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		i := attempt.Intent
		rent, reclaimed := uint64(0), uint64(0)
		if proof.state == LookupReconciled {
			if i.Kind == LookupCreate || i.Kind == LookupRollover || i.Kind == LookupExtend {
				if readback.Owner != lookupProgram || readback.Authority != i.Authority || readback.Absent || !lookupSameAddresses(readback.Addresses, append(append([]string{}, i.Prefix...), i.Extension...)) || readback.LastExtendedSlot >= uint64(proof.readbackSlot) {
					return errors.New("lookup exact warmed growth proof changed")
				}
				if err = lookupSourceMembership(ctx, tx, i); err != nil {
					return err
				}
				for n, address := range i.Extension {
					_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_addresses(route_lookup_table_id,address,ordinal,added_operation_id,added_slot,usable_after_slot,last_verified_slot,last_verified_at) VALUES($1,$2,$3,$4,$5::bigint,$5::bigint+1,$6,clock_timestamp())`, i.TableID, address, len(i.Prefix)+n, i.OperationID, int64(readback.LastExtendedSlot), proof.readbackSlot)
					if err != nil {
						return err
					}
				}
				if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_addresses SET last_verified_slot=$2,last_verified_at=clock_timestamp() WHERE route_lookup_table_id=$1 AND last_verified_slot<=$2`, i.TableID, proof.readbackSlot); err != nil {
					return err
				}
				addresses, err := json.Marshal(readback.Addresses)
				if err != nil {
					return err
				}
				tag, err = tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET address_count=$3,usable_address_count=$3,address_hash=$4,addresses=$5,mutation_epoch=mutation_epoch+1,last_extended_slot=$6,last_verified_slot=$7,last_verified_at=clock_timestamp(),desired_state=CASE WHEN desired_state IN ('preparing','warming') THEN 'active' ELSE desired_state END,status='usable',updated_at=clock_timestamp() WHERE id=$1 AND mutation_epoch=$2 AND (last_verified_slot IS NULL OR last_verified_slot<=$7)`, i.TableID, i.MutationEpoch, len(readback.Addresses), lookupOrderedAddressHash(readback.Addresses), addresses, int64(readback.LastExtendedSlot), proof.readbackSlot)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 1 {
					return errors.New("lookup warmed projection is stale")
				}
				rent = receipt.TablePost - receipt.TablePre
			} else if i.Kind == LookupDeactivate || i.Kind == LookupClose {
				state := "deactivated"
				if i.Kind == LookupClose {
					state = "closed"
					reclaimed = receipt.TablePre
				}
				tag, err = tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET desired_state=$3,status=$3,accepting_allocations=false,usable_address_count=0,last_verified_slot=$4,last_verified_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND mutation_epoch=$2 AND (last_verified_slot IS NULL OR last_verified_slot<=$4)`, i.TableID, i.MutationEpoch, state, proof.readbackSlot)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 1 {
					return errors.New("lookup cleanup projection is stale")
				}
			} else {
				return errors.New("lookup signed proof kind is not a mutation")
			}
		}
		state := "complete"
		permitState := "reconciled"
		if proof.state == LookupFailed {
			state = "permanent_failure"
			permitState = "failed"
		}
		if proof.state == LookupExpired {
			state = "retry_wait"
			permitState = "expired"
		}
		tag, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state=$2,finalized_slot=COALESCE($3,finalized_slot),finalized_at=CASE WHEN $3::bigint IS NOT NULL THEN clock_timestamp() ELSE finalized_at END,reconciled_slot=CASE WHEN $2='complete' THEN $4 ELSE reconciled_slot END,reconciled_at=CASE WHEN $2='complete' THEN clock_timestamp() ELSE reconciled_at END,completed_at=CASE WHEN $2='complete' THEN clock_timestamp() ELSE completed_at END,actual_fee_lamports=COALESCE(actual_fee_lamports,0)+$5,actual_rent_lamports=COALESCE(actual_rent_lamports,0)+$6,reclaimed_rent_lamports=COALESCE(reclaimed_rent_lamports,0)+$7,transaction_signature=CASE WHEN $2='retry_wait' THEN NULL ELSE transaction_signature END,message_hash=CASE WHEN $2='retry_wait' THEN NULL ELSE message_hash END,recent_blockhash=CASE WHEN $2='retry_wait' THEN NULL ELSE recent_blockhash END,last_valid_block_height=CASE WHEN $2='retry_wait' THEN NULL ELSE last_valid_block_height END,next_attempt_at=CASE WHEN $2='retry_wait' THEN clock_timestamp()+interval '5 seconds' ELSE NULL END,lease_owner=NULL,lease_expires_at=NULL,error_code=NULL,error_detail=NULL,updated_at=clock_timestamp() WHERE id=$1 AND lease_owner=$8 AND fencing_token=$9 AND lease_expires_at>clock_timestamp()`, i.OperationID, state, finalized, proof.readbackSlot, int64(receipt.Fee), int64(rent), int64(reclaimed), operation.Lease.Owner, operation.Lease.FencingToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_broadcast_permits SET permit_state=$2,resolution_detail='owned finalized packet and coherent effect proof',resolved_at=clock_timestamp(),updated_at=clock_timestamp() WHERE operation_id=$1 AND transaction_signature=$3 AND resolved_at IS NULL`, i.OperationID, permitState, attempt.Wire.TransactionSignature)
		return err
	})
}
