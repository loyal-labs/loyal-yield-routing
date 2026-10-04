package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// ActivateLookupCatalog publishes only the same immutable head/generation which
// the caller read on chain. No transaction status or lifecycle flag substitutes
// for every full ordered shard's actual mature finalized account.
func (s *Store) ActivateLookupCatalog(ctx context.Context, cluster string, familyID, revisionID int64, generation int32, snapshots []LookupSnapshot) error {
	if cluster == "" || familyID <= 0 || revisionID <= 0 || generation < 0 {
		return errors.New("lookup catalog activation identity invalid")
	}
	byAddress := map[string]LookupSnapshot{}
	for _, snapshot := range snapshots {
		if snapshot.Address == "" || byAddress[snapshot.Address].Address != "" {
			return errors.New("lookup activation snapshot identity repeated")
		}
		byAddress[snapshot.Address] = snapshot
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		paused, err := lookupControlLock(ctx, tx, LookupIntent{Cluster: cluster, FamilyID: familyID}, "lookup-catalog")
		if err != nil {
			return err
		}
		if paused {
			return ErrLookupPaused
		}
		// The head is the catalog ownership fence. Planning and signing use this
		// same head-before-family lock order; no separate Go planning owner exists.
		var currentRevision, manifest int64
		var target *int32
		if err := tx.QueryRow(ctx, `SELECT h.catalog_revision_id,h.target_generation,r.manifest_id FROM loyal_yield.lookup_table_shared_market_catalog_heads h JOIN loyal_yield.lookup_table_shared_market_catalog_revisions r ON r.id=h.catalog_revision_id WHERE h.family_id=$1 FOR UPDATE OF h`, familyID).Scan(&currentRevision, &target, &manifest); err != nil {
			return err
		}
		if currentRevision != revisionID || target == nil || *target != generation {
			return errors.New("lookup catalog head changed before activation")
		}
		var authority, state, kind string
		var active *int32
		var highwater int
		if err := tx.QueryRow(ctx, `SELECT provisioning_authority,desired_state,kind,active_generation,allocation_high_water FROM loyal_yield.lookup_table_families WHERE id=$1 AND cluster=$2 FOR UPDATE`, familyID, cluster).Scan(&authority, &state, &kind, &active, &highwater); err != nil {
			return err
		}
		if state != "active" || kind != "shared_market" {
			return errors.New("lookup catalog family does not permit activation")
		}
		var desired []string
		var count int
		var sealed bool
		if err := tx.QueryRow(ctx, `SELECT address_count,sealed_at IS NOT NULL,COALESCE((SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id=m.id),'{}'::text[]) FROM loyal_yield.lookup_table_manifests m WHERE id=$1 AND family_id=$2 AND subject_kind='shared_market'`, manifest, familyID).Scan(&count, &sealed, &desired); err != nil {
			return err
		}
		if !sealed || count != len(desired) {
			return errors.New("lookup catalog immutable manifest is incomplete")
		}
		shards, err := lookupCatalogShards(desired, highwater)
		if err != nil {
			return err
		}
		if len(shards) == 0 {
			return errors.New("lookup catalog empty generation cannot be published")
		}
		generations := []int32{generation}
		if active != nil && *active != generation {
			generations = append(generations, *active)
		}
		rows, err := tx.Query(ctx, `SELECT id,table_address,authority,generation,shard_ordinal,desired_state,address_count,usable_address_count,address_hash,addresses,last_verified_slot FROM loyal_yield.route_lookup_tables WHERE family_id=$1 AND generation=ANY($2) AND allocation_kind='shared_market' ORDER BY id FOR UPDATE`, familyID, generations)
		if err != nil {
			return err
		}
		var ids []int64
		targetMembership := map[int64][]string{}
		targetCount := 0
		for rows.Next() {
			var id int64
			var address, owner, lifecycle, hash string
			var gen, ordinal int32
			var addressCount, usable int
			var data json.RawMessage
			var verified *int64
			if err = rows.Scan(&id, &address, &owner, &gen, &ordinal, &lifecycle, &addressCount, &usable, &hash, &data, &verified); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
			if gen != generation {
				continue
			}
			targetCount++
			if ordinal < 0 || int(ordinal) >= len(shards) || (lifecycle != "active" && lifecycle != "standby") || addressCount != len(shards[ordinal]) || usable != addressCount || verified == nil {
				rows.Close()
				return errors.New("lookup catalog target shard is not fully verified")
			}
			var projected []string
			if json.Unmarshal(data, &projected) != nil || !lookupSameAddresses(projected, shards[ordinal]) || hash != lookupOrderedAddressHash(projected) {
				rows.Close()
				return errors.New("lookup catalog target ordered projection differs from manifest")
			}
			snapshot, exists := byAddress[address]
			if !exists || snapshot.Absent || snapshot.Owner != lookupProgram || snapshot.Authority != owner || owner != authority || !lookupSameAddresses(snapshot.Addresses, projected) || snapshot.Slot < *verified || snapshot.Slot <= int64(snapshot.LastExtendedSlot) || snapshot.DeactivationSlot != math.MaxUint64 {
				rows.Close()
				return errors.New("lookup catalog lacks exact actual mature shard account")
			}
			targetMembership[id] = projected
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if targetCount != len(shards) || targetCount != len(snapshots) {
			return errors.New("lookup catalog missing or additional target shard readback")
		}
		for id, expected := range targetMembership {
			var membership []string
			var ordinals bool
			if err = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(address ORDER BY ordinal),'{}'::text[]),COALESCE(min(ordinal)=0 AND max(ordinal)=count(*)-1,true) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1`, id).Scan(&membership, &ordinals); err != nil {
				return err
			}
			if !ordinals || !lookupSameAddresses(membership, expected) {
				return errors.New("lookup catalog source membership differs from exact verified table")
			}
		}
		var protected bool
		if err = tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM loyal_yield.lookup_table_usage_leases WHERE route_lookup_table_id=ANY($1) AND released_at IS NULL AND expires_at>clock_timestamp()) OR
 EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=ANY($1) AND operation_state NOT IN ('complete','permanent_failure','cancelled')) OR
 EXISTS(SELECT 1 FROM loyal_yield.lookup_table_signed_attempts WHERE route_lookup_table_id=ANY($1) AND attempt_state NOT IN ('reconciled','failed','expired')) OR
 EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions s WHERE s.cluster=$2 AND s.submission_state NOT IN ('reconciled','failed','expired') AND (jsonb_typeof(s.alt_mutation_epochs->'tables') IS DISTINCT FROM 'array' OR EXISTS(SELECT 1 FROM unnest($1::bigint[]) id WHERE s.alt_mutation_epochs @> jsonb_build_object('tables',jsonb_build_array(jsonb_build_object('tableId',id))))))`, ids, cluster).Scan(&protected); err != nil {
			return err
		}
		if protected {
			return errors.New("lookup catalog activation is held by source usage or unresolved packet custody")
		}
		if active != nil && *active != generation {
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET desired_state='standby',accepting_allocations=false,rollback_until=clock_timestamp()+interval '24 hours',updated_at=clock_timestamp() WHERE family_id=$1 AND generation=$2 AND allocation_kind='shared_market' AND desired_state='active'`, familyID, *active); err != nil {
				return err
			}
		}
		if active == nil || *active != generation {
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET desired_state='active',status='usable',rollback_until=clock_timestamp()+interval '24 hours',updated_at=clock_timestamp() WHERE family_id=$1 AND generation=$2 AND allocation_kind='shared_market'`, familyID, generation); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET previous_generation=active_generation,active_generation=$2,rollback_until=clock_timestamp()+interval '24 hours',updated_at=clock_timestamp() WHERE id=$1`, familyID, generation); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_shared_market_physical_drifts SET resolution_state='resolved',resolution_target_generation=$3,resolved_at=clock_timestamp() WHERE family_id=$1 AND catalog_revision_id=$2 AND resolution_state='open'`, familyID, revisionID, generation); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_shared_market_catalog_heads SET readiness_state='active',activated_at=COALESCE(activated_at,clock_timestamp()),updated_at=clock_timestamp() WHERE family_id=$1 AND catalog_revision_id=$2 AND target_generation=$3`, familyID, revisionID, generation)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return err
	})
}
