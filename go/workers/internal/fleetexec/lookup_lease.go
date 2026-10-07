package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// LeaseLookupOperation preserves the existing source owner and economic order.
// Recovery and read-only verification remain claimable while creation is paused.
func (s *Store) LeaseLookupOperation(ctx context.Context, cluster, owner string, ttl time.Duration, recoverOnly bool) (*LookupOperation, error) {
	if cluster == "" || owner == "" || ttl < 10*time.Second || ttl > 5*time.Minute || ttl%time.Second != 0 {
		return nil, errors.New("lookup operation lease configuration invalid")
	}
	var result *LookupOperation
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `WITH candidate AS (
 SELECT o.id FROM loyal_yield.lookup_table_operations o
 JOIN loyal_yield.lookup_table_families f ON f.id=o.family_id
 LEFT JOIN loyal_yield.lookup_table_provisioner_controls control ON control.cluster=f.cluster
 LEFT JOIN loyal_yield.lookup_table_provisioning_requests request ON request.id=CASE WHEN o.operation_context->>'request_id' ~ '^[0-9]{1,18}$' THEN (o.operation_context->>'request_id')::bigint ELSE NULL END
 LEFT JOIN LATERAL (SELECT COALESCE(sum(p.annual_yield_gain_usd_micros),0)::numeric yield,COALESCE(sum(p.economic_priority),0)::numeric priority,count(*) consumers
 FROM loyal_yield.lookup_table_provisioning_request_consumers c JOIN loyal_yield.rebalance_opportunities p ON p.id=c.opportunity_id
 WHERE c.provisioning_request_id=request.id AND p.opportunity_state='waiting_alt' AND p.expires_at>clock_timestamp()) live ON request.id IS NOT NULL
 WHERE f.cluster=$1 AND o.operation_state NOT IN ('complete','permanent_failure','cancelled')
 AND (o.lease_expires_at IS NULL OR o.lease_expires_at<=clock_timestamp())
 AND (o.next_attempt_at IS NULL OR o.next_attempt_at<=clock_timestamp())
 AND ((o.transaction_signature IS NOT NULL OR o.message_hash IS NOT NULL OR o.recent_blockhash IS NOT NULL OR o.last_valid_block_height IS NOT NULL) OR o.operation_kind='verify' OR (NOT $4 AND NOT COALESCE(control.paused,false) AND (f.desired_state='active' OR (f.desired_state='retiring' AND o.operation_kind IN ('deactivate','close')))))
 AND ((o.transaction_signature IS NOT NULL OR o.message_hash IS NOT NULL OR o.recent_blockhash IS NOT NULL OR o.last_valid_block_height IS NOT NULL) OR o.operation_kind='verify' OR NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_usage_leases u WHERE u.route_lookup_table_id=o.route_lookup_table_id AND u.released_at IS NULL AND u.expires_at>clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations predecessor WHERE predecessor.route_lookup_table_id=o.route_lookup_table_id AND predecessor.id<>o.id AND predecessor.operation_state NOT IN ('complete','permanent_failure','cancelled') AND (predecessor.created_at,predecessor.id)<(o.created_at,o.id))
 ORDER BY CASE o.operation_state WHEN 'needs_reconcile' THEN 0 WHEN 'signed' THEN 1 WHEN 'submitted' THEN 2 WHEN 'confirmed' THEN 3 WHEN 'finalized' THEN 4 WHEN 'reconciled' THEN 5 WHEN 'leased' THEN 6 WHEN 'retry_wait' THEN 7 ELSE 8 END,
 CASE WHEN f.kind='shared_market' THEN 0 ELSE 1 END,
 COALESCE(live.yield,0)/GREATEST(1,COALESCE(request.desired_shared_address_count,0)+COALESCE(request.desired_vault_address_count,0)) DESC,COALESCE(live.priority,0) DESC,COALESCE(live.consumers,0) DESC,o.created_at,o.id
 FOR UPDATE OF o SKIP LOCKED LIMIT 1
 ) UPDATE loyal_yield.lookup_table_operations o SET operation_state=CASE WHEN operation_state IN ('queued','retry_wait','leased') THEN 'leased' ELSE operation_state END,
 lease_owner=$2,lease_expires_at=clock_timestamp()+$3::bigint*interval '1 second',fencing_token=fencing_token+1,attempt_count=attempt_count+1,updated_at=clock_timestamp()
 FROM candidate WHERE o.id=candidate.id RETURNING o.id`, cluster, owner, int64(ttl/time.Second), recoverOnly).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		op, err := loadLeasedLookup(ctx, tx, id)
		if err == nil {
			result = &op
		}
		return err
	})
	return result, err
}

func loadLeasedLookup(ctx context.Context, tx pgx.Tx, id int64) (LookupOperation, error) {
	var op LookupOperation
	err := tx.QueryRow(ctx, `SELECT f.cluster,o.id,f.id,t.id,o.operation_kind,t.table_address,f.provisioning_authority,f.payer,t.generation,o.mutation_epoch,
 o.operation_state,o.lease_owner,o.fencing_token,o.lease_expires_at,f.desired_state,f.kind,t.desired_state,o.manifest_id,o.binding_id,o.operation_context,
 o.transaction_signature,o.message_hash,o.recent_blockhash,o.last_valid_block_height,
 COALESCE((SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=t.id),'{}'::text[]),
 COALESCE((SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_operation_addresses WHERE operation_id=o.id),'{}'::text[])
 FROM loyal_yield.lookup_table_operations o JOIN loyal_yield.lookup_table_families f ON f.id=o.family_id JOIN loyal_yield.route_lookup_tables t ON t.id=o.route_lookup_table_id WHERE o.id=$1`, id).Scan(
		&op.Intent.Cluster, &op.Intent.OperationID, &op.Intent.FamilyID, &op.Intent.TableID, &op.Intent.Kind, &op.Intent.TableAddress, &op.Intent.Authority, &op.Intent.Payer, &op.Intent.Generation, &op.Intent.MutationEpoch,
		&op.State, &op.Lease.Owner, &op.Lease.FencingToken, &op.Lease.ExpiresAt, &op.FamilyState, &op.FamilyKind, &op.TableState, &op.ManifestID, &op.BindingID, &op.Context,
		&op.Signature, &op.MessageHash, &op.Blockhash, &op.LastValidBlockHeight, &op.Intent.Prefix, &op.Intent.Extension)
	if err != nil {
		return op, err
	}
	var reservation struct {
		RecentSlot *uint64 `json:"recent_slot"`
		Alias      *uint64 `json:"recentSlot"`
	}
	if err = json.Unmarshal(op.Context, &reservation); err != nil {
		return op, errors.New("lookup operation context is invalid")
	}
	if op.Intent.Kind == LookupCreate || op.Intent.Kind == LookupRollover {
		if reservation.RecentSlot != nil && reservation.Alias != nil && *reservation.RecentSlot != *reservation.Alias {
			return op, errors.New("lookup reserved recent slot aliases differ")
		}
		op.Intent.RecentSlot = reservation.RecentSlot
		if op.Intent.RecentSlot == nil {
			op.Intent.RecentSlot = reservation.Alias
		}
		if op.Intent.RecentSlot == nil {
			return op, errors.New("lookup create has no reserved source slot")
		}
	}
	if op.Intent.Kind == LookupClose {
		op.Intent.Recipient = op.Intent.Authority
	}
	return op, nil
}

func (s *Store) RenewLookupLease(ctx context.Context, op LookupOperation, ttl time.Duration) error {
	if ttl < 10*time.Second || ttl > 5*time.Minute || ttl%time.Second != 0 {
		return errors.New("lookup renewal duration invalid")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET lease_expires_at=clock_timestamp()+$4::bigint*interval '1 second',updated_at=clock_timestamp() WHERE id=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp() AND operation_state NOT IN ('complete','permanent_failure','cancelled')`, op.Intent.OperationID, op.Lease.Owner, op.Lease.FencingToken, int64(ttl/time.Second))
	if err == nil && tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return err
}

// Unsigned deferral never clears retained source signatures or owned packets.
func (s *Store) deferLookupUnsigned(ctx context.Context, op LookupOperation, reason string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET operation_state='retry_wait',next_attempt_at=clock_timestamp()+interval '5 seconds',lease_owner=NULL,lease_expires_at=NULL,error_detail=$4,updated_at=clock_timestamp()
 WHERE id=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp() AND transaction_signature IS NULL AND message_hash IS NULL AND recent_blockhash IS NULL AND last_valid_block_height IS NULL`, op.Intent.OperationID, op.Lease.Owner, op.Lease.FencingToken, reason)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return err
}
