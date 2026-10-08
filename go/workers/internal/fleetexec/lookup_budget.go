package fleetexec

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

type LookupBudget struct {
	MaximumLamports int64
	RollingWindow   time.Duration
}

// ReserveLookupBudget uses the existing source cluster advisory lock and both
// the reusable and legacy-cleanup ledgers. Per-operation max(actual,reserved)
// prevents double charging while overlapping fencing reservations still add.
func (s *Store) ReserveLookupBudget(ctx context.Context, operation LookupOperation, policy LookupBudget, fee, rent uint64) (bool, error) {
	if policy.MaximumLamports <= 0 || policy.RollingWindow < time.Second || policy.RollingWindow > 365*24*time.Hour || policy.RollingWindow%time.Second != 0 || fee > math.MaxInt64 || rent > math.MaxInt64 || fee > math.MaxInt64-rent {
		return false, errors.New("lookup budget policy/accounting invalid")
	}
	requested := int64(fee + rent)
	approved := false
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-budget:'||$1,0))`, operation.Intent.Cluster); err != nil {
			return err
		}
		paused, err := lookupControlLock(ctx, tx, operation.Intent, operation.Lease.Owner)
		if err != nil {
			return err
		}
		source, err := lookupLockSource(ctx, tx, operation.Intent, operation.Lease)
		if err != nil {
			return err
		}
		if paused || !lookupFamilyAllows(source.familyState, operation.Intent.Kind) {
			return ErrLookupPaused
		}
		if source.state != "leased" {
			return errors.New("lookup budget is for a fresh unsigned source operation")
		}
		var unsigned bool
		if err = tx.QueryRow(ctx, `SELECT transaction_signature IS NULL AND message_hash IS NULL AND recent_blockhash IS NULL AND last_valid_block_height IS NULL FROM loyal_yield.lookup_table_operations WHERE id=$1`, operation.Intent.OperationID).Scan(&unsigned); err != nil {
			return err
		}
		if !unsigned {
			return errors.New("lookup retained source signature requires reconciliation before any new budget or key use")
		}
		if err = lookupSourceMembership(ctx, tx, operation.Intent); err != nil {
			return err
		}
		if err = lookupUnsignedGuards(ctx, tx, operation.Intent, source); err != nil {
			return err
		}
		var owner string
		var oldFee, oldRent int64
		var live bool
		err = tx.QueryRow(ctx, `SELECT lease_owner,estimated_fee_lamports,estimated_rent_lamports,reserved_until>clock_timestamp() FROM loyal_yield.lookup_table_cluster_budget_reservations WHERE operation_id=$1 AND fencing_token=$2`, operation.Intent.OperationID, operation.Lease.FencingToken).Scan(&owner, &oldFee, &oldRent, &live)
		if err == nil {
			if owner != operation.Lease.Owner || oldFee != int64(fee) || oldRent != int64(rent) {
				return errors.New("lookup budget fence replay changed accounting")
			}
			approved = live
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var charged, subjectReserved, subjectActual int64
		err = tx.QueryRow(ctx, `WITH reusable AS (
 SELECT 'operation'::text kind,o.id subject,COALESCE(sum(r.reserved_lamports),0)::bigint reserved,
 (COALESCE(o.actual_fee_lamports,0)+COALESCE(o.actual_rent_lamports,0))::bigint actual
 FROM loyal_yield.lookup_table_cluster_budget_reservations r JOIN loyal_yield.lookup_table_operations o ON o.id=r.operation_id
 WHERE r.cluster=$1 AND r.reserved_until>clock_timestamp() AND o.operation_state<>'cancelled'
 GROUP BY o.id,o.actual_fee_lamports,o.actual_rent_lamports
 ),legacy AS (
 SELECT 'legacy_cleanup'::text kind,a.id subject,COALESCE(sum(r.reserved_lamports),0)::bigint reserved,0::bigint actual
 FROM loyal_yield.lookup_table_legacy_cleanup_budget_reservations r JOIN loyal_yield.lookup_table_legacy_cleanup_attempts a ON a.id=r.legacy_cleanup_attempt_id
 WHERE r.cluster=$1 AND r.reserved_until>clock_timestamp() GROUP BY a.id
 ),subjects AS (SELECT * FROM reusable UNION ALL SELECT * FROM legacy)
 SELECT COALESCE(sum(GREATEST(reserved,actual)),0)::bigint,COALESCE(max(reserved) FILTER(WHERE kind='operation' AND subject=$2),0)::bigint,COALESCE(max(actual) FILTER(WHERE kind='operation' AND subject=$2),0)::bigint FROM subjects`, operation.Intent.Cluster, operation.Intent.OperationID).Scan(&charged, &subjectReserved, &subjectActual)
		if err != nil {
			return err
		}
		if subjectReserved > math.MaxInt64-requested {
			return errors.New("lookup budget reservation overflow")
		}
		previous := max(subjectReserved, subjectActual)
		next := max(subjectReserved+requested, subjectActual)
		if charged > math.MaxInt64-(next-previous) {
			return errors.New("lookup budget aggregate overflow")
		}
		if charged+next-previous > policy.MaximumLamports {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_cluster_budget_reservations(cluster,operation_id,fencing_token,lease_owner,estimated_fee_lamports,estimated_rent_lamports,reserved_lamports,reserved_until) VALUES($1,$2,$3,$4,$5,$6,$7,clock_timestamp()+$8::bigint*interval '1 second')`, operation.Intent.Cluster, operation.Intent.OperationID, operation.Lease.FencingToken, operation.Lease.Owner, int64(fee), int64(rent), requested, int64(policy.RollingWindow/time.Second))
		approved = err == nil
		return err
	})
	return approved, err
}
