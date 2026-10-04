package fleetexec

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

var ErrLookupPaused = errors.New("lookup source provisioning is paused")

// RecordLookupBroadcastIntent commits the existing 0021 exact-signature permit
// and the journal's single broadcast intent before any network IO. Once this
// returns successfully, even a crash before Send remains recovery-only.
func (s *Store) RecordLookupBroadcastIntent(ctx context.Context, operation LookupOperation, attempt LookupAttempt) error {
	if err := proveLookupWire(attempt.Intent, attempt.Wire); err != nil {
		return err
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		paused, err := lookupControlLock(ctx, tx, attempt.Intent, operation.Lease.Owner)
		if err != nil {
			return err
		}
		source, err := lookupLockSource(ctx, tx, attempt.Intent, operation.Lease)
		if err != nil {
			return err
		}
		current, err := scanLookupAttempt(tx.QueryRow(ctx, `SELECT `+lookupAttemptColumns+` FROM loyal_yield.lookup_table_signed_attempts WHERE id=$1 FOR UPDATE`, attempt.ID))
		if err != nil {
			return err
		}
		if lookupProofBinding(current) != lookupProofBinding(attempt) || current.State != LookupPrepared || current.BroadcastCount != 0 {
			return errors.New("lookup owned packet already crossed broadcast boundary")
		}
		if paused || !lookupFamilyAllows(source.familyState, attempt.Intent.Kind) {
			return ErrLookupPaused
		}
		if source.state != "signed" && source.state != "needs_reconcile" {
			return errors.New("lookup source operation is not a known signed packet")
		}
		if err = lookupUnsignedGuards(ctx, tx, attempt.Intent, source); err != nil {
			return err
		}
		if err = lookupSourceMembership(ctx, tx, attempt.Intent); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioner_broadcast_permits(cluster,operation_id,fencing_token,control_epoch,transaction_signature,message_hash) SELECT $1,$2,$3,control_epoch,$4,$5 FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster=$1 AND NOT paused`, attempt.Intent.Cluster, attempt.Intent.OperationID, operation.Lease.FencingToken, attempt.Wire.TransactionSignature, attempt.Wire.MessageHash)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_signed_attempts SET attempt_state='unknown',broadcast_count=1,last_broadcast_at=clock_timestamp(),error_detail='broadcast_intent_persisted',updated_at=clock_timestamp() WHERE id=$1 AND attempt_state='prepared' AND broadcast_count=0`, attempt.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}

// deferLookupRecovery changes only scheduling and retains every immutable
// packet field. It deliberately resolves no source permit on a send timeout.
func (s *Store) deferLookupRecovery(ctx context.Context, operation LookupOperation, attempt LookupAttempt, reason string) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := lookupLockSource(ctx, tx, attempt.Intent, operation.Lease); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_signed_attempts SET last_status_checked_at=clock_timestamp(),error_detail=$2,updated_at=clock_timestamp() WHERE id=$1 AND transaction_signature=$3 AND operation_id=$4 AND signed_transaction_sha256=$5 AND attempt_state NOT IN ('reconciled','failed','expired')`, attempt.ID, reason, attempt.Wire.TransactionSignature, operation.Intent.OperationID, attempt.Wire.SignedTransactionHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		tag, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='needs_reconcile',next_attempt_at=clock_timestamp()+interval '5 seconds',error_detail=$2,lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>clock_timestamp()`, attempt.Intent.OperationID, reason, operation.Lease.Owner, operation.Lease.FencingToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}
