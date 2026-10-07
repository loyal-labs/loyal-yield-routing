package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
)

// A read-only census avoids acquiring or incrementing source leases while idle.
// The later lease query remains authoritative if priority or ownership changes.
func (s *Store) nextLookupPlanningVault(ctx context.Context, cluster string) (int64, error) {
	var vault int64
	err := s.pool.QueryRow(ctx, `SELECT r.vault_id `+lookupPlanningCandidates+` LIMIT 1`, cluster).Scan(&vault)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return vault, err
}

// Read published source references before taking a request lease. A different
// request may win the later economic-priority lease; unmatched evidence simply
// leaves that request queued. No source status substitutes for these accounts.
func (p *LookupPlanner) readPlanningReadiness(ctx context.Context, vault int64, bank *lookupPlanningBank) error {
	rows, err := p.store.pool.Query(ctx, `SELECT DISTINCT t.table_address FROM loyal_yield.route_lookup_tables t JOIN loyal_yield.lookup_table_families f ON f.id=t.family_id WHERE f.cluster=$1 AND f.desired_state='active' AND t.desired_state='active' AND t.generation=f.active_generation AND ((f.kind='shared_market' AND EXISTS(SELECT 1 FROM loyal_yield.lookup_table_shared_market_catalog_heads h WHERE h.family_id=f.id AND h.readiness_state='active' AND h.target_generation=f.active_generation)) OR (f.kind='vault_shards' AND EXISTS(SELECT 1 FROM loyal_yield.lookup_table_vault_bindings b JOIN loyal_yield.lookup_table_vault_desired_heads h ON h.family_id=b.family_id AND h.vault_id=b.vault_id AND h.binding_ordinal=b.binding_ordinal AND h.manifest_id=b.manifest_id AND h.desired_revision=b.desired_head_revision WHERE b.route_lookup_table_id=t.id AND b.vault_id=$2 AND b.binding_ordinal=0 AND b.lifecycle_state='active'))) ORDER BY t.table_address LIMIT 18`, p.config.Cluster, vault)
	if err != nil {
		return err
	}
	var addresses []string
	for rows.Next() {
		var address string
		if err = rows.Scan(&address); err != nil {
			rows.Close()
			return err
		}
		addresses = append(addresses, address)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(addresses) > 17 {
		return errors.New("lookup readiness exceeds sixteen shared shards and one vault table")
	}
	bank.snapshots = make(map[string]LookupSnapshot, len(addresses))
	for _, address := range addresses {
		snapshot, e := p.chain.LookupSnapshot(ctx, address, bank.slot)
		if e != nil {
			return e
		}
		bank.snapshots[address] = snapshot
		bank.slot = max(bank.slot, snapshot.Slot)
	}
	return nil
}

func lookupPlanningSatisfiedTx(ctx context.Context, tx pgx.Tx, r LookupPlanningRequest, p LookupVaultPlan, bank lookupPlanningBank) (bool, error) {
	if p.OperationID != 0 || len(bank.snapshots) == 0 {
		return false, nil
	}
	var family, manifest int64
	var generation *int32
	var state string
	// Lock the authoritative head before its family, as catalog publication does.
	if err := tx.QueryRow(ctx, `SELECT h.family_id,rev.manifest_id,h.target_generation,h.readiness_state FROM loyal_yield.lookup_table_shared_market_catalog_heads h JOIN loyal_yield.lookup_table_families f ON f.id=h.family_id JOIN loyal_yield.lookup_table_shared_market_catalog_revisions rev ON rev.id=h.catalog_revision_id WHERE f.cluster=$1 AND f.kind='shared_market' AND f.desired_state='active' AND h.catalog_revision_id=$2 FOR SHARE OF h`, r.Cluster, p.CatalogRevisionID).Scan(&family, &manifest, &generation, &state); err != nil {
		return false, err
	}
	if generation == nil || state != "active" {
		return false, nil
	}
	var active *int32
	if err := tx.QueryRow(ctx, `SELECT active_generation FROM loyal_yield.lookup_table_families WHERE id=$1 AND desired_state='active' FOR SHARE`, family).Scan(&active); err != nil {
		return false, err
	}
	if active == nil || *active != *generation {
		return false, nil
	}
	var tableIDs []int64
	rows, err := tx.Query(ctx, `SELECT id FROM loyal_yield.route_lookup_tables WHERE family_id=$1 AND generation=$2 AND allocation_kind='shared_market' ORDER BY id LIMIT 17`, family, *generation)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		tableIDs = append(tableIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(tableIDs) == 0 || len(tableIDs) > 16 {
		return false, nil
	}
	sharedTableIDs := append([]int64(nil), tableIDs...)
	if !p.NotRequired {
		var bindingValid bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_vault_bindings b JOIN loyal_yield.lookup_table_vault_desired_heads h ON h.family_id=b.family_id AND h.vault_id=b.vault_id AND h.binding_ordinal=b.binding_ordinal AND h.manifest_id=b.manifest_id AND h.desired_revision=b.desired_head_revision JOIN loyal_yield.lookup_table_families f ON f.id=b.family_id JOIN loyal_yield.route_lookup_tables t ON t.id=b.route_lookup_table_id WHERE b.id=$1 AND b.vault_id=$2 AND b.manifest_id=$3 AND b.route_lookup_table_id=$4 AND b.binding_ordinal=0 AND b.lifecycle_state='active' AND f.desired_state='active' AND t.generation=f.active_generation)`, p.BindingID, r.VaultID, p.ManifestID, p.TableID).Scan(&bindingValid)
		if err != nil || !bindingValid {
			return false, err
		}
		tableIDs = append(tableIDs, p.TableID)
	}
	rows, err = tx.Query(ctx, `SELECT t.id,t.table_address,t.authority,t.address_count,t.usable_address_count,t.address_hash,t.addresses,t.last_verified_slot,t.status,t.desired_state,t.durable FROM loyal_yield.route_lookup_tables t WHERE id=ANY($1) ORDER BY id FOR SHARE`, tableIDs)
	if err != nil {
		return false, err
	}
	physical := map[int64][]string{}
	readbackSlots := map[int64]int64{}
	valid := true
	for rows.Next() {
		var id int64
		var address, authority, hash, status, lifecycle string
		var count, usable int
		var data json.RawMessage
		var verified *int64
		var durable bool
		if err = rows.Scan(&id, &address, &authority, &count, &usable, &hash, &data, &verified, &status, &lifecycle, &durable); err != nil {
			rows.Close()
			return false, err
		}
		var projected []string
		snapshot, exists := bank.snapshots[address]
		if !exists || snapshot.Absent || snapshot.Slot <= 0 || snapshot.Owner != lookupProgram || snapshot.Authority != authority || snapshot.DeactivationSlot != math.MaxUint64 || snapshot.LastExtendedSlot >= uint64(snapshot.Slot) || verified == nil || snapshot.Slot < *verified || status != "usable" || lifecycle != "active" || !durable || count != usable || json.Unmarshal(data, &projected) != nil || len(projected) != count || !lookupSameAddresses(projected, snapshot.Addresses) || hash != lookupOrderedAddressHash(projected) {
			valid = false
		}
		physical[id] = projected
		readbackSlots[id] = snapshot.Slot
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !valid || len(physical) != len(tableIDs) {
		return false, err
	}
	for id, expected := range physical {
		var actual []string
		var exact bool
		if err = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(address ORDER BY ordinal),'{}'::text[]),COALESCE(min(ordinal)=0 AND max(ordinal)=count(*)-1 AND bool_and(last_verified_slot IS NOT NULL AND usable_after_slot IS NOT NULL AND last_verified_slot<=$2 AND usable_after_slot<=$2),true) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1`, id, readbackSlots[id]).Scan(&actual, &exact); err != nil {
			return false, err
		}
		if !exact || !lookupSameAddresses(actual, expected) {
			return false, nil
		}
	}
	var covered, unresolved bool
	err = tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_manifest_addresses a WHERE a.manifest_id=$1 AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_addresses ta WHERE ta.route_lookup_table_id=ANY($2) AND ta.address=a.address)) AND ($3 OR NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_manifest_addresses a WHERE a.manifest_id=$4 AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_addresses ta WHERE ta.route_lookup_table_id=$5 AND ta.address=a.address))), EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=ANY($6) AND operation_state NOT IN ('complete','permanent_failure','cancelled'))`, manifest, sharedTableIDs, p.NotRequired, p.ManifestID, p.TableID, tableIDs).Scan(&covered, &unresolved)
	return covered && !unresolved, err
}
