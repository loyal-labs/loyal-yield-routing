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

// commitLookupProof applies a verified finalized packet to its source
// operation and physical table in one short transaction.
func (s *Store) commitLookupProof(ctx context.Context, operation LookupOperation, attempt LookupAttempt, proof *lookupProof) error {
	if proof == nil || proof.binding != lookupProofBinding(attempt) || proof.readbackSlot < attempt.SigningContextSlot || (proof.state != LookupReconciled && proof.state != LookupFailed) {
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
	if err := json.Unmarshal(proof.receipt, &receipt); err != nil {
		return err
	}
	if receipt.Fee > math.MaxInt64 || receipt.TablePre > math.MaxInt64 || receipt.TablePost > math.MaxInt64 {
		return errors.New("lookup actual SOL accounting exceeds durable range")
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := lookupLockSource(ctx, tx, attempt.Intent, operation.Lease); err != nil {
			return err
		}
		finalized := proof.finalizedSlot
		return finishLookupSourceTx(ctx, tx, operation, attempt, proof, readback, receipt, &finalized)
	})
}

// expireLookupOperation is the Rust provisioner's retry: the expired packet's
// identity moves to operation_context.attempt_history and the operation
// returns to retry_wait for a fresh packet.
func (s *Store) expireLookupOperation(ctx context.Context, operation LookupOperation, attempt LookupAttempt) error {
	const detail = "blockhash expired without the signature landing"
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='retry_wait',next_attempt_at=clock_timestamp()+interval '5 seconds',error_code='expired_transaction_not_observed',error_detail=$5,
 operation_context=jsonb_set(operation_context,'{attempt_history}',COALESCE(operation_context->'attempt_history','[]'::jsonb)||jsonb_build_array(jsonb_strip_nulls(jsonb_build_object(
  'fencingToken',fencing_token,'transactionSignature',transaction_signature,'messageHash',message_hash,'recentBlockhash',recent_blockhash,'lastValidBlockHeight',last_valid_block_height,
  'estimatedFeeLamports',estimated_fee_lamports,'estimatedRentLamports',estimated_rent_lamports,'estimatedReclaimedRentLamports',operation_context->'signedExpectedReclaimedRentLamports',
  'submittedSlot',submitted_slot,'submittedAt',submitted_at,'confirmedSlot',confirmed_slot,'confirmedAt',confirmed_at,'finalizedSlot',finalized_slot,'finalizedAt',finalized_at,
  'reconciledSlot',reconciled_slot,'reconciledAt',reconciled_at,'archivedAt',now()))),true) - 'signedExpectedReclaimedRentLamports' - 'signedTransaction' - 'signingContextSlot' - 'broadcastCount',
 transaction_signature=NULL,message_hash=NULL,recent_blockhash=NULL,last_valid_block_height=NULL,estimated_fee_lamports=NULL,estimated_rent_lamports=NULL,submitted_slot=NULL,submitted_at=NULL,
 confirmed_slot=NULL,confirmed_at=NULL,finalized_slot=NULL,finalized_at=NULL,reconciled_slot=NULL,reconciled_at=NULL,completed_at=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND transaction_signature=$2 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>clock_timestamp()`, attempt.Intent.OperationID, attempt.Wire.TransactionSignature, operation.Lease.Owner, operation.Lease.FencingToken, detail)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_broadcast_permits SET permit_state='expired',resolution_detail=$2,resolved_at=clock_timestamp(),updated_at=clock_timestamp() WHERE operation_id=$1 AND resolved_at IS NULL`, attempt.Intent.OperationID, detail)
		return err
	})
}

func finishLookupSourceTx(ctx context.Context, tx pgx.Tx, operation LookupOperation, attempt LookupAttempt, proof *lookupProof, readback lookupEffectReadback, receipt lookupEffectReceipt, finalized *int64) error {
	var err error
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
			tag, err := tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET address_count=$3,usable_address_count=$3,address_hash=$4,addresses=$5,mutation_epoch=mutation_epoch+1,last_extended_slot=$6,last_verified_slot=$7,last_verified_at=clock_timestamp(),desired_state=CASE WHEN desired_state IN ('preparing','warming') THEN 'active' ELSE desired_state END,status='usable',updated_at=clock_timestamp() WHERE id=$1 AND mutation_epoch=$2 AND (last_verified_slot IS NULL OR last_verified_slot<=$7)`, i.TableID, i.MutationEpoch, len(readback.Addresses), lookupOrderedAddressHash(readback.Addresses), addresses, int64(readback.LastExtendedSlot), proof.readbackSlot)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("lookup warmed projection is stale")
			}
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET create_signature=CASE WHEN $2 IN ('create','rollover') THEN COALESCE(create_signature,$3) ELSE create_signature END,extend_signatures=CASE WHEN $4 AND NOT extend_signatures @> jsonb_build_array($3::text) THEN extend_signatures||jsonb_build_array($3::text) ELSE extend_signatures END,warmup_slot=$5::bigint+1 WHERE id=$1`, i.TableID, string(i.Kind), attempt.Wire.TransactionSignature, len(i.Extension) > 0, int64(readback.LastExtendedSlot)); err != nil {
				return err
			}
			rent = receipt.TablePost - receipt.TablePre
		} else if i.Kind == LookupDeactivate || i.Kind == LookupClose {
			state := "deactivated"
			if i.Kind == LookupClose {
				state = "closed"
				reclaimed = receipt.TablePre
			}
			tag, err := tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET desired_state=$3,status=$3,accepting_allocations=false,usable_address_count=0,last_verified_slot=$4,last_verified_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND mutation_epoch=$2 AND (last_verified_slot IS NULL OR last_verified_slot<=$4)`, i.TableID, i.MutationEpoch, state, proof.readbackSlot)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("lookup cleanup projection is stale")
			}
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET deactivated_slot=CASE WHEN $2='deactivate' THEN $3 ELSE deactivated_slot END,deactivate_signature=CASE WHEN $2='deactivate' THEN $4 ELSE deactivate_signature END,closed_signature=CASE WHEN $2='close' THEN $4 ELSE closed_signature END,close_recipient=CASE WHEN $2='close' THEN $5 ELSE close_recipient END,reclaimed_lamports=CASE WHEN $2='close' THEN $6 ELSE reclaimed_lamports END WHERE id=$1`, i.TableID, string(i.Kind), proof.finalizedSlot, attempt.Wire.TransactionSignature, i.Recipient, int64(reclaimed)); err != nil {
				return err
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
	// The Rust writer stores the current packet's actual accounting with
	// COALESCE(new, old), not an accumulated operation history.
	actualFee, actualRent, actualReclaimed := int64(receipt.Fee), int64(rent), int64(reclaimed)
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state=$2,finalized_slot=COALESCE($3,finalized_slot),finalized_at=CASE WHEN $3::bigint IS NOT NULL THEN clock_timestamp() ELSE finalized_at END,reconciled_slot=CASE WHEN $2='complete' THEN $4 ELSE reconciled_slot END,reconciled_at=CASE WHEN $2='complete' THEN clock_timestamp() ELSE reconciled_at END,completed_at=CASE WHEN $2='complete' THEN clock_timestamp() ELSE completed_at END,actual_fee_lamports=COALESCE($5::bigint,actual_fee_lamports),actual_rent_lamports=COALESCE($6::bigint,actual_rent_lamports),reclaimed_rent_lamports=COALESCE($7::bigint,reclaimed_rent_lamports),next_attempt_at=NULL,lease_owner=NULL,lease_expires_at=NULL,error_code=NULL,error_detail=NULL,updated_at=clock_timestamp() WHERE id=$1 AND lease_owner=$8 AND fencing_token=$9 AND lease_expires_at>clock_timestamp()`, i.OperationID, state, finalized, proof.readbackSlot, actualFee, actualRent, actualReclaimed, operation.Lease.Owner, operation.Lease.FencingToken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	_, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_broadcast_permits SET permit_state=$2,resolution_detail='finalized packet and coherent effect proof',resolved_at=clock_timestamp(),updated_at=clock_timestamp() WHERE operation_id=$1 AND transaction_signature=$3 AND resolved_at IS NULL`, i.OperationID, permitState, attempt.Wire.TransactionSignature)
	return err
}
