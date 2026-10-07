package fleetexec

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// Verification is read-only on chain, including while family controls pause
// mutations. It cannot adopt a signature or infer absent account success.
func (s *Store) finishLookupVerification(ctx context.Context, op LookupOperation, snapshot LookupSnapshot) error {
	i := op.Intent
	if i.Kind != LookupVerify || snapshot.Address != i.TableAddress || snapshot.Slot <= 0 || snapshot.Absent || snapshot.Owner != lookupProgram || snapshot.Authority != i.Authority || !lookupSameAddresses(snapshot.Addresses, i.Prefix) || len(i.Extension) != 0 || snapshot.DeactivationSlot != math.MaxUint64 || snapshot.Slot <= int64(snapshot.LastExtendedSlot) {
		return s.deferLookupUnsigned(ctx, op, "read-only verification lacks exact mature active table")
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := lookupLockSource(ctx, tx, i, op.Lease); err != nil {
			return err
		}
		if err := lookupSourceMembership(ctx, tx, i); err != nil {
			return err
		}
		var unsigned bool
		if err := tx.QueryRow(ctx, `SELECT transaction_signature IS NULL AND message_hash IS NULL AND recent_blockhash IS NULL AND last_valid_block_height IS NULL FROM loyal_yield.lookup_table_operations WHERE id=$1`, i.OperationID).Scan(&unsigned); err != nil {
			return err
		}
		if !unsigned {
			return errors.New("lookup verification cannot replace owned signed mutation")
		}
		// Source Verify publishes a new confirmed-membership projection version;
		// it is atomic with completion here, so a restart cannot advance it twice.
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET desired_state=CASE WHEN desired_state IN ('preparing','warming') THEN 'active' ELSE desired_state END,status='usable',usable_address_count=address_count,mutation_epoch=mutation_epoch+1,last_extended_slot=$4,last_extended_start_index=$5,last_verified_slot=$3,last_verified_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND mutation_epoch=$2 AND mutation_epoch<9223372036854775807 AND desired_state IN ('preparing','warming','active','standby','retiring') AND (last_verified_slot IS NULL OR last_verified_slot<=$3)`, i.TableID, i.MutationEpoch, snapshot.Slot, int64(snapshot.LastExtendedSlot), int(snapshot.LastExtendedStartIndex))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_addresses SET last_verified_slot=$2,last_verified_at=clock_timestamp() WHERE route_lookup_table_id=$1 AND (last_verified_slot IS NULL OR last_verified_slot<=$2)`, i.TableID, snapshot.Slot); err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='complete',reconciled_slot=$4,reconciled_at=clock_timestamp(),completed_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=NULL,error_code=NULL,error_detail=NULL,updated_at=clock_timestamp() WHERE id=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`, i.OperationID, op.Lease.Owner, op.Lease.FencingToken, snapshot.Slot)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return err
	})
}

func (s *Store) deferLookupLegacy(ctx context.Context, op LookupOperation, reason string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='needs_reconcile',next_attempt_at=clock_timestamp()+interval '5 seconds',lease_owner=NULL,lease_expires_at=NULL,error_detail=$4,updated_at=clock_timestamp() WHERE id=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp() AND (transaction_signature IS NOT NULL OR message_hash IS NOT NULL OR recent_blockhash IS NOT NULL OR last_valid_block_height IS NOT NULL)`, op.Intent.OperationID, op.Lease.Owner, op.Lease.FencingToken, reason)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return err
}
