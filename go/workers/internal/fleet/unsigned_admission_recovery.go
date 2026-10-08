package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// RecoverUnsignedExecutionAdmissions releases admissions abandoned before a
// signed journal existed. Any persisted signature, including a terminal one,
// belongs to signed-route recovery and cannot be reclaimed by lease expiry.
// Each opportunity commits separately so a batch never holds several reserve
// frontiers in different orders. The caller receives only committed recoveries.
func (s *Store) RecoverUnsignedExecutionAdmissions(ctx context.Context, cluster string, limit int) (int64, error) {
	if s == nil || s.pool == nil || cluster == "" || cluster != strings.TrimSpace(cluster) || limit < 1 || limit > 1000 {
		return 0, errors.New("unsigned admission recovery requires canonical cluster and limit in 1..=1000")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var recovered int64
	for range limit {
		found := false
		err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			var opportunityID, epochID, token int64
			var target, mint string
			err := tx.QueryRow(ctx, `
SELECT o.id,o.optimizer_epoch_id,o.fencing_token,o.target_reserve,o.liquidity_mint
FROM loyal_yield.rebalance_opportunities o
WHERE o.cluster=$1 AND o.decision_id IS NULL
 AND (o.opportunity_state='stale' OR (o.opportunity_state='leased' AND o.lease_kind='execute'
  AND o.lease_expires_at<=clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions signed WHERE signed.opportunity_id=o.id)
 AND EXISTS(SELECT 1 FROM loyal_yield.target_capacity_reservations reservation
  WHERE reservation.opportunity_id=o.id AND reservation.cluster=o.cluster
   AND reservation.target_reserve=o.target_reserve AND reservation.liquidity_mint=o.liquidity_mint
   AND reservation.reservation_state='active' AND reservation.signed_submission_id IS NULL AND reservation.decision_id IS NULL)
ORDER BY o.lease_expires_at NULLS FIRST,o.id FOR UPDATE OF o SKIP LOCKED LIMIT 1`, cluster).Scan(&opportunityID, &epochID, &token, &target, &mint)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			// Signed publication uses the same opportunity -> frontier lock order.
			// The opportunity lock also fences concurrent FK attachment of a journal.
			var locked int
			if err := tx.QueryRow(ctx, `SELECT 1 FROM loyal_yield.target_capacity_frontiers
WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3 FOR UPDATE`, cluster, target, mint).Scan(&locked); err != nil {
				return fmt.Errorf("unsigned admission capacity frontier: %w", err)
			}
			if err := tx.QueryRow(ctx, `SELECT 1 FROM loyal_yield.optimizer_epochs
WHERE id=$1 AND cluster=$2 FOR SHARE`, epochID, cluster).Scan(&locked); err != nil {
				return fmt.Errorf("unsigned admission epoch identity: %w", err)
			}
			// Recheck signed ownership after acquiring the admission frontier. Never
			// infer no effect from a terminal state or an absent decision link.
			var signed bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions WHERE opportunity_id=$1)`, opportunityID).Scan(&signed); err != nil {
				return err
			}
			if signed {
				return errors.New("unsigned admission acquired signed ownership")
			}
			tag, err := tx.Exec(ctx, `UPDATE loyal_yield.target_capacity_reservations
SET reservation_state='released',released_at=clock_timestamp(),release_reason='unsigned_execution_lease_expired',
 state_version=state_version+1,updated_at=clock_timestamp()
WHERE opportunity_id=$1 AND cluster=$2 AND target_reserve=$3 AND liquidity_mint=$4
 AND reservation_state='active' AND signed_submission_id IS NULL AND decision_id IS NULL`, opportunityID, cluster, target, mint)
			if err != nil || tag.RowsAffected() != 1 {
				if err != nil {
					return err
				}
				return errors.New("unsigned admission reservation changed")
			}
			tag, err = tx.Exec(ctx, `WITH recovery_clock AS MATERIALIZED (SELECT clock_timestamp() AS observed_at)
UPDATE loyal_yield.rebalance_opportunities o
SET opportunity_state=CASE WHEN o.opportunity_state<>'stale' AND o.expires_at>recovery_clock.observed_at+interval '60 seconds'
 AND epoch.expires_at>recovery_clock.observed_at+interval '60 seconds' THEN 'revalidate' ELSE 'stale' END,
 terminal_reason=CASE WHEN o.opportunity_state='stale' THEN o.terminal_reason
 WHEN o.expires_at>recovery_clock.observed_at+interval '60 seconds'
 AND epoch.expires_at>recovery_clock.observed_at+interval '60 seconds' THEN NULL ELSE 'optimizer_epoch_expired' END,
 lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL,fencing_token=o.fencing_token+1,updated_at=clock_timestamp()
FROM loyal_yield.optimizer_epochs epoch,recovery_clock
WHERE o.id=$1 AND o.cluster=$2 AND o.optimizer_epoch_id=$3 AND epoch.id=$3 AND epoch.cluster=$2
 AND o.fencing_token=$4 AND o.decision_id IS NULL
 AND (o.opportunity_state='stale' OR (o.opportunity_state='leased' AND o.lease_kind='execute'
  AND o.lease_expires_at<=clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions signed WHERE signed.opportunity_id=o.id)`, opportunityID, cluster, epochID, token)
			if err != nil || tag.RowsAffected() != 1 {
				if err != nil {
					return err
				}
				return errors.New("unsigned admission opportunity fence changed")
			}
			if _, err := tx.Exec(ctx, `DELETE FROM loyal_yield.route_account_conflict_leases
WHERE opportunity_id=$1 AND cluster=$2 AND submission_id IS NULL`, opportunityID, cluster); err != nil {
				return err
			}
			found = true
			return nil
		})
		if err != nil {
			return recovered, fmt.Errorf("recover unsigned execution admission: %w", err)
		}
		if !found {
			break
		}
		recovered++
	}
	return recovered, nil
}
