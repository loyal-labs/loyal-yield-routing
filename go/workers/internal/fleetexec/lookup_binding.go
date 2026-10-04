package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// ActivateLookupBinding publishes only the newest desired revision, after an
// actual mature account read. Other vaults using an unchanged packed physical
// table do not conflict with this logical-head publication.
func (s *Store) ActivateLookupBinding(ctx context.Context, bindingID int64, snapshot LookupSnapshot) error {
	if bindingID <= 0 || snapshot.Slot <= 0 || snapshot.Absent || snapshot.Owner != lookupProgram || snapshot.DeactivationSlot != math.MaxUint64 || snapshot.LastExtendedSlot >= uint64(snapshot.Slot) {
		return errors.New("lookup binding has no actual mature ALT proof")
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var familyID, tableID, manifestID, vaultID int64
		var ordinal int32
		if err := tx.QueryRow(ctx, `SELECT family_id,route_lookup_table_id,manifest_id,vault_id,binding_ordinal FROM loyal_yield.lookup_table_vault_bindings WHERE id=$1`, bindingID).Scan(&familyID, &tableID, &manifestID, &vaultID, &ordinal); err != nil {
			return err
		}
		var cluster, authority, payer string
		if err := tx.QueryRow(ctx, `SELECT cluster FROM loyal_yield.lookup_table_families WHERE id=$1`, familyID).Scan(&cluster); err != nil {
			return err
		}
		paused, err := lookupControlLock(ctx, tx, LookupIntent{Cluster: cluster, FamilyID: familyID}, "go-lookup-binding")
		if err != nil {
			return err
		}
		if paused {
			return ErrLookupPaused
		}
		var activeGeneration *int32
		if err := tx.QueryRow(ctx, `SELECT cluster,provisioning_authority,payer,active_generation FROM loyal_yield.lookup_table_families WHERE id=$1 AND kind='vault_shards' AND desired_state='active' FOR SHARE`, familyID).Scan(&cluster, &authority, &payer, &activeGeneration); err != nil {
			return err
		}
		if authority != snapshot.Authority || payer != authority {
			return errors.New("lookup binding authority changed")
		}
		var revision int64
		var desiredManifest int64
		if err := tx.QueryRow(ctx, `SELECT manifest_id,desired_revision FROM loyal_yield.lookup_table_vault_desired_heads WHERE family_id=$1 AND vault_id=$2 AND binding_ordinal=$3 FOR UPDATE`, familyID, vaultID, ordinal).Scan(&desiredManifest, &revision); err != nil {
			return err
		}
		if desiredManifest != manifestID {
			return errors.New("lookup binding desired manifest superseded")
		}
		rows, err := tx.Query(ctx, `SELECT id,route_lookup_table_id,lifecycle_state,manifest_id,desired_head_revision FROM loyal_yield.lookup_table_vault_bindings WHERE family_id=$1 AND vault_id=$2 AND binding_ordinal=$3 AND lifecycle_state IN ('preparing','warming','active') ORDER BY id FOR UPDATE`, familyID, vaultID, ordinal)
		if err != nil {
			return err
		}
		var predecessorID *int64
		var affected = []int64{tableID}
		var newest int64
		var candidate bool
		for rows.Next() {
			var id, physical, manifest, rev int64
			var state string
			if err = rows.Scan(&id, &physical, &state, &manifest, &rev); err != nil {
				rows.Close()
				return err
			}
			if state == "active" {
				if id == bindingID {
					rows.Close()
					return errors.New("lookup binding is already active")
				}
				if predecessorID != nil || id >= bindingID {
					rows.Close()
					return errors.New("lookup logical head is not monotonic")
				}
				copy := id
				predecessorID = &copy
				if physical != tableID {
					affected = append(affected, physical)
				}
			}
			if state != "active" && manifest == manifestID && rev == revision {
				newest = max(newest, id)
				if id == bindingID && physical == tableID {
					candidate = true
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if !candidate || newest != bindingID {
			return errors.New("lookup binding is not newest desired candidate")
		}
		// Lock the predecessor/candidate physical rows in stable order.
		rows, err = tx.Query(ctx, `SELECT id FROM loyal_yield.route_lookup_tables WHERE id=ANY($1) AND family_id=$2 ORDER BY id FOR UPDATE`, affected, familyID)
		if err != nil {
			return err
		}
		locked := 0
		for rows.Next() {
			locked++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if locked != len(affected) {
			return errors.New("lookup binding physical identity missing")
		}
		var physicalAddress, hash, state, status, kind string
		var generation int32
		var count, usable, reserved int
		var durable bool
		var verified *int64
		var projection json.RawMessage
		err = tx.QueryRow(ctx, `SELECT t.table_address,t.address_hash,t.desired_state,t.status,t.allocation_kind,t.generation,t.address_count,t.usable_address_count,b.reserved_capacity,t.durable,t.last_verified_slot,t.addresses FROM loyal_yield.route_lookup_tables t JOIN loyal_yield.lookup_table_vault_bindings b ON b.route_lookup_table_id=t.id WHERE b.id=$1 AND b.family_id=$2 AND b.manifest_id=$3 AND b.vault_id=$4 AND b.binding_ordinal=$5 AND b.desired_head_revision=$6 AND t.cluster=$7 AND t.authority=$8 AND t.payer=$8 AND (b.allocation_mode='packed_shard' AND t.allocation_kind='vault_shard' OR b.allocation_mode='dedicated' AND t.allocation_kind='dedicated_vault')`, bindingID, familyID, manifestID, vaultID, ordinal, revision, cluster, authority).Scan(&physicalAddress, &hash, &state, &status, &kind, &generation, &count, &usable, &reserved, &durable, &verified, &projection)
		if err != nil {
			return err
		}
		var projected []string
		if physicalAddress != snapshot.Address || activeGeneration == nil || *activeGeneration != generation || state != "active" || status != "usable" || !durable || verified == nil || *verified > snapshot.Slot || usable != count || len(snapshot.Addresses) != count || json.Unmarshal(projection, &projected) != nil || !lookupSameAddresses(projected, snapshot.Addresses) || lookupOrderedAddressHash(projected) != hash {
			return errors.New("lookup binding physical projection is not exact current verified head")
		}
		var membership []string
		var exact bool
		err = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(address ORDER BY ordinal),'{}'::text[]),COALESCE(min(ordinal)=0 AND max(ordinal)=count(*)-1 AND bool_and(usable_after_slot<=$2 AND last_verified_slot<=$2),true) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1`, tableID, snapshot.Slot).Scan(&membership, &exact)
		if err != nil {
			return err
		}
		if !exact || !lookupSameAddresses(membership, projected) {
			return errors.New("lookup binding ordered usable membership changed")
		}
		var covered bool
		err = tx.QueryRow(ctx, `SELECT m.address_count<=$4 AND count(a.address)=m.address_count AND COALESCE(bool_and(a.address=ANY($5) AND a.ordinal>=0),true) FROM loyal_yield.lookup_table_manifests m LEFT JOIN loyal_yield.lookup_table_manifest_addresses a ON a.manifest_id=m.id WHERE m.id=$1 AND m.family_id=$2 AND m.vault_id=$3 AND m.subject_kind='vault' AND m.sealed_at IS NOT NULL GROUP BY m.address_count`, manifestID, familyID, vaultID, reserved, projected).Scan(&covered)
		if err != nil {
			return err
		}
		if !covered {
			return errors.New("lookup binding sealed full manifest is not covered")
		}
		var protected bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_usage_leases WHERE route_lookup_table_id=ANY($1) AND released_at IS NULL AND expires_at>clock_timestamp() AND (vault_id=$2 OR binding_id=$3 OR vault_id IS NULL OR binding_id IS NULL)) OR EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=$4 AND operation_state NOT IN ('complete','permanent_failure','cancelled')) OR EXISTS(SELECT 1 FROM loyal_yield.lookup_table_signed_attempts WHERE route_lookup_table_id=ANY($1) AND attempt_state NOT IN ('reconciled','failed','expired')) OR EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_opportunities p ON p.id=s.opportunity_id WHERE s.cluster=$5 AND s.submission_state NOT IN ('reconciled','failed','expired') AND (jsonb_typeof(s.alt_mutation_epochs->'tables') IS DISTINCT FROM 'array' OR (p.vault_id=$2 AND EXISTS(SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(s.alt_mutation_epochs->'tables')='array' THEN s.alt_mutation_epochs->'tables' ELSE '[]'::jsonb END) entry WHERE (entry->>'tableId')::text=ANY(SELECT x::text FROM unnest($1::bigint[]) x)))))`, affected, vaultID, predecessorID, tableID, cluster).Scan(&protected)
		if err != nil {
			return err
		}
		if protected {
			return errors.New("lookup binding has logical usage or unresolved packet custody")
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_vault_bindings SET lifecycle_state='failed',deactivated_at=COALESCE(deactivated_at,clock_timestamp()),updated_at=clock_timestamp() WHERE family_id=$1 AND vault_id=$2 AND binding_ordinal=$3 AND id<>$4 AND lifecycle_state IN ('preparing','warming')`, familyID, vaultID, ordinal, bindingID); err != nil {
			return err
		}
		if predecessorID != nil {
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_vault_bindings SET lifecycle_state='standby',active_until_slot=$2,rollback_until=clock_timestamp()+interval '24 hours',updated_at=clock_timestamp() WHERE id=$1 AND lifecycle_state='active'`, *predecessorID, snapshot.Slot); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_vault_bindings SET lifecycle_state='active',active_from_slot=$2,predecessor_binding_id=COALESCE($3,predecessor_binding_id),updated_at=clock_timestamp() WHERE id=$1 AND lifecycle_state IN ('preparing','warming')`, bindingID, snapshot.Slot, predecessorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}
