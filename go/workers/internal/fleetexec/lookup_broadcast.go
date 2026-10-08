package fleetexec

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

var ErrLookupPaused = errors.New("lookup source provisioning is paused")

// RecordLookupSend counts one send of the operation's signed packet before
// the bytes leave. The first send also takes the Rust 0021 exact-signature
// permit and rechecks the source controls; resends of the same bytes only
// count, because the permit already covers that signature.
func (s *Store) RecordLookupSend(ctx context.Context, operation LookupOperation, attempt LookupAttempt) error {
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		paused, err := lookupControlLock(ctx, tx, attempt.Intent, operation.Lease.Owner)
		if err != nil {
			return err
		}
		source, err := lookupLockSource(ctx, tx, attempt.Intent, operation.Lease)
		if err != nil {
			return err
		}
		if source.state != "signed" && source.state != "submitted" && source.state != "needs_reconcile" {
			return errors.New("lookup source operation is not a known signed packet")
		}
		var permitted bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_provisioner_broadcast_permits WHERE operation_id=$1 AND transaction_signature=$2 AND resolved_at IS NULL)`, attempt.Intent.OperationID, attempt.Wire.TransactionSignature).Scan(&permitted); err != nil {
			return err
		}
		if !permitted {
			if paused || !lookupFamilyAllows(source.familyState, attempt.Intent.Kind) {
				return ErrLookupPaused
			}
			if err = lookupUnsignedGuards(ctx, tx, attempt.Intent, source); err != nil {
				return err
			}
			if err = lookupSourceMembership(ctx, tx, attempt.Intent); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioner_broadcast_permits(cluster,operation_id,fencing_token,control_epoch,transaction_signature,message_hash) SELECT $1,$2,$3,control_epoch,$4,$5 FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster=$1 AND NOT paused`, attempt.Intent.Cluster, attempt.Intent.OperationID, operation.Lease.FencingToken, attempt.Wire.TransactionSignature, attempt.Wire.MessageHash)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrLookupPaused
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state=CASE WHEN operation_state='signed' THEN 'submitted' ELSE operation_state END,submitted_at=COALESCE(submitted_at,clock_timestamp()),operation_context=jsonb_set(operation_context,'{broadcastCount}',to_jsonb(COALESCE((operation_context->>'broadcastCount')::int,0)+1),true),updated_at=clock_timestamp() WHERE id=$1 AND transaction_signature=$2 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>clock_timestamp()`, attempt.Intent.OperationID, attempt.Wire.TransactionSignature, operation.Lease.Owner, operation.Lease.FencingToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}

// deferLookupRecovery changes only scheduling and retains the signed packet,
// as Rust's poll deferral does. A pause also moves a never-sent packet to
// needs_reconcile.
func (s *Store) deferLookupRecovery(ctx context.Context, operation LookupOperation, reason string, paused bool) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	tag, err := s.pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state=CASE WHEN $5 AND operation_state='signed' THEN 'needs_reconcile' ELSE operation_state END,next_attempt_at=clock_timestamp()+interval '5 seconds',error_detail=$2,lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>clock_timestamp()`, operation.Intent.OperationID, reason, operation.Lease.Owner, operation.Lease.FencingToken, paused)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return nil
}

// markLookupDrift is Rust's NeedsManualReconcile: the packet's chain effect
// does not match its intent, so it is neither retried nor applied.
func (s *Store) markLookupDrift(ctx context.Context, operation LookupOperation, attempt LookupAttempt, reason string) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='needs_reconcile',error_code='chain_drift',error_detail=$2,updated_at=clock_timestamp() WHERE id=$1 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>clock_timestamp()`, operation.Intent.OperationID, reason, operation.Lease.Owner, operation.Lease.FencingToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioner_broadcast_permits SET permit_state='needs_reconcile',resolution_detail=$3,resolved_at=clock_timestamp(),updated_at=clock_timestamp() WHERE operation_id=$1 AND transaction_signature=$2 AND resolved_at IS NULL`, operation.Intent.OperationID, attempt.Wire.TransactionSignature, reason)
		return err
	})
}
