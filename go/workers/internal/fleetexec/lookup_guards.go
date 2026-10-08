package fleetexec

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

func lookupFamilyAllows(state string, kind LookupKind) bool {
	return state == "active" || (state == "retiring" && (kind == LookupDeactivate || kind == LookupClose))
}

// Control, shared head, family, table, operation is the writer lock order.
// Recovery takes no desired-state gate: changing controls cannot revoke a packet.
func lookupControlLock(ctx context.Context, tx pgx.Tx, i LookupIntent, owner string) (bool, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioner_controls(cluster,paused,reason,updated_by,control_epoch) VALUES($1,false,'implicit unpaused provisioner control',$2,0) ON CONFLICT(cluster) DO NOTHING`, i.Cluster, owner); err != nil {
		return false, err
	}
	var paused bool
	if err := tx.QueryRow(ctx, `SELECT paused FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster=$1 FOR UPDATE`, i.Cluster).Scan(&paused); err != nil {
		return false, err
	}
	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM loyal_yield.lookup_table_families WHERE id=$1 AND cluster=$2`, i.FamilyID, i.Cluster).Scan(&kind); err != nil {
		return false, err
	}
	if kind == "shared_market" {
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT catalog_revision_id FROM loyal_yield.lookup_table_shared_market_catalog_heads WHERE family_id=$1 FOR UPDATE`, i.FamilyID).Scan(&revision); err != nil {
			return false, errors.New("lookup shared catalog head is unavailable")
		}
	}
	return paused, nil
}
func lookupUnsignedGuards(ctx context.Context, tx pgx.Tx, i LookupIntent, source lookupLockedSource) error {
	var protected bool
	err := tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM loyal_yield.lookup_table_usage_leases WHERE route_lookup_table_id=$1 AND released_at IS NULL AND expires_at>clock_timestamp()) OR
 EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions WHERE cluster=$2 AND submission_state NOT IN ('reconciled','failed','expired') AND (alt_mutation_epochs @> jsonb_build_object('tables',jsonb_build_array(jsonb_build_object('tableId',$1::bigint))) OR jsonb_typeof(alt_mutation_epochs->'tables') IS DISTINCT FROM 'array')) OR
 EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations predecessor JOIN loyal_yield.lookup_table_operations current ON current.id=$3 WHERE predecessor.route_lookup_table_id=$1 AND predecessor.id<>$3 AND predecessor.operation_state NOT IN ('complete','permanent_failure','cancelled') AND (predecessor.created_at,predecessor.id)<(current.created_at,current.id))`, i.TableID, i.Cluster, i.OperationID).Scan(&protected)
	if err != nil {
		return err
	}
	if protected {
		return errors.New("lookup mutation has source usage, signed route custody, or predecessor")
	}
	var context struct {
		RecentSlot            *uint64 `json:"recent_slot"`
		RecentSlotAlias       *uint64 `json:"recentSlot"`
		ExpectedAuthority     string
		ExpectedAddressHash   string
		ExpectedMutationEpoch *int64
		ExpectedAddressCount  *int64
		CloseRecipient        string
	}
	if err = json.Unmarshal(source.context, &context); err != nil {
		return errors.New("lookup source operation context is malformed")
	}
	if i.Kind == LookupCreate || i.Kind == LookupRollover {
		recent := context.RecentSlot
		if recent == nil {
			recent = context.RecentSlotAlias
		}
		if recent == nil || i.RecentSlot == nil || *recent != *i.RecentSlot || (context.RecentSlot != nil && context.RecentSlotAlias != nil && *context.RecentSlot != *context.RecentSlotAlias) {
			return errors.New("lookup source create slot reservation changed")
		}
	}
	if source.familyKind == "shared_market" && (i.Kind == LookupCreate || i.Kind == LookupRollover || i.Kind == LookupExtend) {
		if err = lookupSharedHeadGuard(ctx, tx, i); err != nil {
			return err
		}
	}
	if i.Kind != LookupDeactivate && i.Kind != LookupClose {
		return nil
	}
	if context.ExpectedAuthority != i.Authority || context.ExpectedAddressHash != lookupOrderedAddressHash(i.Prefix) || context.ExpectedMutationEpoch == nil || *context.ExpectedMutationEpoch != i.MutationEpoch || context.ExpectedAddressCount == nil || *context.ExpectedAddressCount != int64(len(i.Prefix)) || (i.Kind == LookupClose && context.CloseRecipient != "" && context.CloseRecipient != i.Authority) {
		return errors.New("lookup queued cleanup identity changed")
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(t.accepting_allocations OR t.last_verified_slot IS NULL OR t.rollback_until>clock_timestamp() OR f.rollback_until>clock_timestamp()
 OR f.previous_generation=t.generation
 OR (f.active_generation=t.generation AND NOT(t.allocation_kind IN ('vault_shard','dedicated_vault') AND NOT t.accepting_allocations AND t.desired_state IN ('retiring','deactivated') AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_vault_bindings WHERE route_lookup_table_id=t.id AND lifecycle_state IN ('preparing','warming','active','standby','retiring'))))
 OR EXISTS(SELECT 1 FROM loyal_yield.lookup_table_vault_bindings WHERE route_lookup_table_id=t.id AND (lifecycle_state IN ('preparing','warming','active','standby','retiring') OR rollback_until>clock_timestamp()))
 OR EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=t.id AND id<>$2 AND operation_state NOT IN ('complete','permanent_failure','cancelled')),false)
 FROM loyal_yield.route_lookup_tables t JOIN loyal_yield.lookup_table_families f ON f.id=t.family_id WHERE t.id=$1`, i.TableID, i.OperationID).Scan(&protected)
	if err != nil {
		return err
	}
	if protected {
		return errors.New("lookup cleanup became protected by allocation, generation, rollback, binding, or pending operation")
	}
	if (i.Kind == LookupDeactivate && source.tableState != "active" && source.tableState != "standby" && source.tableState != "retiring") || (i.Kind == LookupClose && source.tableState != "deactivated") {
		return errors.New("lookup cleanup lifecycle changed")
	}
	return nil
}
func lookupSharedHeadGuard(ctx context.Context, tx pgx.Tx, i LookupIntent) error {
	var desired []string
	var count, highwater, shard int
	var valid bool
	err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id=r.manifest_id),'{}'::text[]),r.address_count,t.allocation_high_water,t.shard_ordinal,
 o.manifest_id=r.manifest_id AND h.target_generation=t.generation AND t.allocation_kind='shared_market' AND (o.operation_kind NOT IN ('create','rollover') OR (o.target_generation=t.generation AND o.target_shard_ordinal=t.shard_ordinal)) AND t.address_count=$3
 FROM loyal_yield.lookup_table_shared_market_catalog_heads h JOIN loyal_yield.lookup_table_shared_market_catalog_revisions r ON r.id=h.catalog_revision_id
 JOIN loyal_yield.route_lookup_tables t ON t.family_id=h.family_id JOIN loyal_yield.lookup_table_operations o ON o.id=$1 AND o.family_id=h.family_id WHERE t.id=$2`, i.OperationID, i.TableID, len(i.Prefix)).Scan(&desired, &count, &highwater, &shard, &valid)
	if err != nil {
		return err
	}
	if !valid || len(desired) != count || highwater <= 0 || highwater > 256 || shard < 0 || shard > len(desired)/highwater {
		return errors.New("lookup shared catalog generation/manifest identity changed")
	}
	start := shard * highwater
	end := start + highwater
	if end > len(desired) {
		end = len(desired)
	}
	combined := append(append([]string(nil), i.Prefix...), i.Extension...)
	if len(combined) > end-start || len(i.Extension) == 0 || !lookupSameAddresses(combined, desired[start:start+len(combined)]) {
		return errors.New("lookup mutation is not the exact next ordered catalog shard suffix")
	}
	return nil
}
