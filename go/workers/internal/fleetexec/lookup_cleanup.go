package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

type lookupCleanupCandidate struct {
	intent       LookupIntent
	shard        int32
	deactivation *int64
}

func (s *Store) nextLookupCleanup(ctx context.Context, cluster string) (*lookupCleanupCandidate, error) {
	var c lookupCleanupCandidate
	err := s.pool.QueryRow(ctx, `SELECT f.cluster,f.id,t.id,t.table_address,f.provisioning_authority,f.payer,t.generation,t.mutation_epoch,t.shard_ordinal,t.deactivated_slot,t.desired_state,
 COALESCE((SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=t.id),'{}'::text[])
 FROM loyal_yield.route_lookup_tables t JOIN loyal_yield.lookup_table_families f ON f.id=t.family_id
 WHERE f.cluster=$1 AND f.desired_state IN ('active','retiring') AND t.desired_state IN ('standby','retiring','deactivated')
 AND NOT t.accepting_allocations AND t.last_verified_slot IS NOT NULL
 AND (t.rollback_until IS NULL OR t.rollback_until<=clock_timestamp()) AND (f.rollback_until IS NULL OR f.rollback_until<=clock_timestamp())
 AND f.previous_generation IS DISTINCT FROM t.generation
 AND (f.active_generation IS DISTINCT FROM t.generation OR (t.allocation_kind IN ('vault_shard','dedicated_vault') AND t.desired_state IN ('retiring','deactivated')))
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_vault_bindings WHERE route_lookup_table_id=t.id AND (lifecycle_state IN ('preparing','warming','active','standby','retiring') OR rollback_until>clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=t.id AND operation_state NOT IN ('complete','permanent_failure','cancelled'))
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations o WHERE route_lookup_table_id=t.id AND operation_kind IN ('deactivate','close') AND operation_state='permanent_failure' AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_terminal_repair_operations WHERE operation_id=o.id))
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_usage_leases WHERE route_lookup_table_id=t.id AND released_at IS NULL AND expires_at>clock_timestamp())
 AND NOT COALESCE((SELECT paused FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster=$1),false)
 ORDER BY t.last_verified_at NULLS FIRST,t.updated_at,t.id LIMIT 1`, cluster).Scan(&c.intent.Cluster, &c.intent.FamilyID, &c.intent.TableID, &c.intent.TableAddress, &c.intent.Authority, &c.intent.Payer, &c.intent.Generation, &c.intent.MutationEpoch, &c.shard, &c.deactivation, &c.intent.Kind, &c.intent.Prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c.intent.Kind == "deactivated" {
		c.intent.Kind = LookupClose
		c.intent.Recipient = c.intent.Authority
	} else {
		c.intent.Kind = LookupDeactivate
	}
	return &c, nil
}

func (s *Store) queueLookupCleanup(ctx context.Context, c lookupCleanupCandidate, snapshot LookupSnapshot) (int64, error) {
	i := c.intent
	if snapshot.Slot <= 0 || snapshot.Absent || snapshot.Address != i.TableAddress || snapshot.Owner != lookupProgram || snapshot.Authority != i.Authority || snapshot.LastExtendedSlot >= uint64(snapshot.Slot) || !lookupSameAddresses(snapshot.Addresses, i.Prefix) {
		return 0, errors.New("lookup cleanup has no exact mature account readback")
	}
	if i.Kind == LookupDeactivate {
		if snapshot.DeactivationSlot != math.MaxUint64 {
			return 0, errors.New("lookup unsigned deactivate already has unattributed effect")
		}
	} else {
		if c.deactivation == nil || *c.deactivation < 0 || snapshot.DeactivationSlot != uint64(*c.deactivation) {
			return 0, errors.New("lookup close deactivation receipt identity changed")
		}
		if lookupProducedSlot(snapshot.SlotHashes, snapshot.DeactivationSlot) {
			// Preserve the actual observed bank while cooldown holds. The
			// selector orders by verification time so another eligible table
			// can progress without inventing skipped SlotHashes entries.
			_, err := s.pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables t SET last_verified_slot=$4,last_verified_at=clock_timestamp() WHERE id=$1 AND table_address=$2 AND mutation_epoch=$3 AND desired_state='deactivated' AND deactivated_slot=$5 AND address_hash=$6 AND (last_verified_slot IS NULL OR last_verified_slot<=$4)`, i.TableID, i.TableAddress, i.MutationEpoch, snapshot.Slot, c.deactivation, lookupOrderedAddressHash(i.Prefix))
			if err != nil {
				return 0, err
			}
			return 0, nil
		}
		if snapshot.DeactivationSlot >= uint64(snapshot.Slot) {
			return 0, errors.New("lookup close bank does not follow deactivation")
		}
	}
	var id int64
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		paused, err := lookupControlLock(ctx, tx, i, "go-lookup-cleanup")
		if err != nil {
			return err
		}
		if paused {
			return ErrLookupPaused
		}
		var state, kind string
		var cluster, authority, payer string
		err = tx.QueryRow(ctx, `SELECT desired_state,kind,cluster,provisioning_authority,payer FROM loyal_yield.lookup_table_families WHERE id=$1 FOR SHARE`, i.FamilyID).Scan(&state, &kind, &cluster, &authority, &payer)
		if err != nil {
			return err
		}
		if !lookupFamilyAllows(state, i.Kind) || cluster != i.Cluster || authority != i.Authority || payer != i.Payer || authority != payer {
			return errors.New("lookup cleanup family no longer eligible")
		}
		var tableState string
		var exact bool
		err = tx.QueryRow(ctx, `SELECT desired_state,cluster=$2 AND family_id=$3 AND table_address=$4 AND authority=$5 AND payer=$5 AND generation=$6 AND mutation_epoch=$7 AND address_count=$8 AND address_hash=$9 AND last_verified_slot<=$10 AND (deactivated_slot IS NOT DISTINCT FROM $11::bigint) FROM loyal_yield.route_lookup_tables WHERE id=$1 FOR UPDATE`, i.TableID, i.Cluster, i.FamilyID, i.TableAddress, i.Authority, i.Generation, i.MutationEpoch, len(i.Prefix), lookupOrderedAddressHash(i.Prefix), snapshot.Slot, c.deactivation).Scan(&tableState, &exact)
		if err != nil {
			return err
		}
		if !exact {
			return errors.New("lookup cleanup physical identity changed")
		}
		contextBytes, err := json.Marshal(map[string]any{"expectedAuthority": i.Authority, "expectedAddressHash": lookupOrderedAddressHash(i.Prefix), "expectedMutationEpoch": i.MutationEpoch, "expectedAddressCount": len(i.Prefix), "closeRecipient": i.Authority, "cluster": i.Cluster, "table": i.TableAddress})
		if err != nil {
			return err
		}
		id, err = enqueueLookupTx(ctx, tx, lookupQueuedOperation{intent: i, shard: c.shard, desiredHash: lookupOrderedAddressHash(i.Prefix), context: contextBytes})
		if err != nil {
			return err
		}
		i.OperationID = id
		if err = lookupSourceMembership(ctx, tx, i); err != nil {
			return err
		}
		return lookupUnsignedGuards(ctx, tx, i, lookupLockedSource{familyState: state, familyKind: kind, tableState: tableState, context: contextBytes})
	})
	return id, err
}
