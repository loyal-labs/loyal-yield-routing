package fleetexec

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// FinalizeLookupRollback ports the explicit source rollback-finalization
// transition. It releases expired standby reservations, then makes their
// physical tables eligible for separately proved deactivation/close packets.
// Current shared heads are never retired by an inferred age or empty queue.
func (s *Store) FinalizeLookupRollback(ctx context.Context, cluster string, familyID int64) (bool, error) {
	changed := false
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		paused, err := lookupControlLock(ctx, tx, LookupIntent{Cluster: cluster, FamilyID: familyID}, "go-lookup-retirement")
		if err != nil {
			return err
		}
		if paused {
			return nil
		}
		var previous *int32
		var allowed, explicit bool
		err = tx.QueryRow(ctx, `SELECT previous_generation,desired_state IN ('active','retiring') AND (rollback_until IS NULL OR rollback_until<=clock_timestamp()),rollback_until IS NOT NULL FROM loyal_yield.lookup_table_families WHERE id=$1 AND cluster=$2 FOR UPDATE`, familyID, cluster).Scan(&previous, &allowed, &explicit)
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		}
		if previous != nil && !explicit {
			return errors.New("lookup previous generation has no explicit rollback deadline")
		}
		rows, err := tx.Query(ctx, `SELECT id,route_lookup_table_id,rollback_until IS NOT NULL AND rollback_until<=clock_timestamp() FROM loyal_yield.lookup_table_vault_bindings WHERE family_id=$1 AND lifecycle_state='standby' ORDER BY id FOR UPDATE LIMIT 257`, familyID)
		if err != nil {
			return err
		}
		var bindings, tables []int64
		for rows.Next() {
			var id, table int64
			var expired bool
			if err = rows.Scan(&id, &table, &expired); err != nil {
				rows.Close()
				return err
			}
			if !expired {
				rows.Close()
				return nil
			}
			bindings = append(bindings, id)
			tables = append(tables, table)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(bindings) > 256 {
			return errors.New("lookup standby finalization exceeds bounded 256 bindings")
		}
		if previous == nil && len(bindings) == 0 {
			return nil
		}
		rows, err = tx.Query(ctx, `SELECT id,desired_state,generation,rollback_until IS NULL OR rollback_until<=clock_timestamp() FROM loyal_yield.route_lookup_tables WHERE family_id=$1 AND (generation=$2 OR id=ANY($3)) ORDER BY id FOR UPDATE LIMIT 257`, familyID, previous, tables)
		if err != nil {
			return err
		}
		var affected, previousTables []int64
		for rows.Next() {
			var id int64
			var state string
			var generation int32
			var expired bool
			if err = rows.Scan(&id, &state, &generation, &expired); err != nil {
				rows.Close()
				return err
			}
			if !expired {
				rows.Close()
				return nil
			}
			if previous != nil && generation == *previous {
				if state != "standby" {
					rows.Close()
					return errors.New("lookup previous generation is not entirely standby")
				}
				previousTables = append(previousTables, id)
			}
			affected = append(affected, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(affected) > 256 {
			return errors.New("lookup rollback table set exceeds bounded 256")
		}
		if previous != nil && len(previousTables) == 0 {
			return errors.New("lookup previous generation has no physical tables")
		}
		var protected bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_usage_leases WHERE route_lookup_table_id=ANY($1) AND released_at IS NULL AND expires_at>clock_timestamp()) OR EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=ANY($1) AND operation_state NOT IN ('complete','permanent_failure','cancelled')) OR EXISTS(SELECT 1 FROM loyal_yield.lookup_table_signed_attempts WHERE route_lookup_table_id=ANY($1) AND attempt_state NOT IN ('reconciled','failed','expired')) OR EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions WHERE cluster=$2 AND submission_state NOT IN ('reconciled','failed','expired') AND (jsonb_typeof(alt_mutation_epochs->'tables') IS DISTINCT FROM 'array' OR EXISTS(SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(alt_mutation_epochs->'tables')='array' THEN alt_mutation_epochs->'tables' ELSE '[]'::jsonb END) e WHERE e->>'tableId'=ANY(SELECT x::text FROM unnest($1::bigint[]) x))))`, affected, cluster).Scan(&protected)
		if err != nil {
			return err
		}
		if protected {
			return nil
		}
		// A predecessor generation cannot be finalized while any live binding
		// other than the explicitly expired standby set still references it.
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_vault_bindings WHERE route_lookup_table_id=ANY($1) AND lifecycle_state IN ('preparing','warming','active','standby','retiring') AND NOT id=ANY($2))`, previousTables, bindings).Scan(&protected); err != nil {
			return err
		}
		if protected {
			return nil
		}
		if len(bindings) > 0 {
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_vault_bindings SET lifecycle_state='retired',deactivated_at=COALESCE(deactivated_at,clock_timestamp()),rollback_until=NULL,updated_at=clock_timestamp() WHERE id=ANY($1) AND lifecycle_state='standby' AND rollback_until<=clock_timestamp()`, bindings); err != nil {
				return err
			}
			changed = true
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET accepting_allocations=false,updated_at=clock_timestamp() WHERE id=ANY($1) AND allocation_kind IN ('vault_shard','dedicated_vault')`, tables); err != nil {
			return err
		}
		if previous != nil {
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET desired_state='retiring',accepting_allocations=false,rollback_until=NULL,updated_at=clock_timestamp() WHERE id=ANY($1) AND desired_state='standby'`, previousTables); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET previous_generation=NULL,rollback_until=NULL,updated_at=clock_timestamp() WHERE id=$1`, familyID); err != nil {
				return err
			}
			changed = true
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables t SET desired_state='retiring',accepting_allocations=false,rollback_until=NULL,updated_at=clock_timestamp() WHERE id=ANY($1) AND allocation_kind IN ('vault_shard','dedicated_vault') AND desired_state IN ('active','standby') AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_vault_bindings WHERE route_lookup_table_id=t.id AND lifecycle_state IN ('preparing','warming','active','standby','retiring'))`, tables); err != nil {
			return err
		}
		return nil
	})
	return changed, err
}
