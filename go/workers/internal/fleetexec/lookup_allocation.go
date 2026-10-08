package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

type lookupReservation struct {
	address    string
	recentSlot uint64
}
type lookupPlanningBank struct {
	slot         int64
	authority    string
	reservations []lookupReservation
	snapshots    map[string]LookupSnapshot
}
type LookupVaultPlan struct {
	ManifestID, BindingID, TableID, OperationID int64
	SharedManifestID, CatalogRevisionID         int64
	NotRequired                                 bool
}
type lookupPlanningFamily struct {
	id                     int64
	name, authority, payer string
	activeGeneration       int32
	policy                 LookupPackingPolicy
}

// PlanLookupVaultRequest seals the complete cohort and reserves source capacity
// atomically under the existing request lease and per-vault advisory lock. The
// caller obtained bounded PDA absence/SlotHashes evidence before taking a lease.
func (s *Store) planLookupVaultRequest(ctx context.Context, request LookupPlanningRequest, policy LookupPackingPolicy, bank lookupPlanningBank) (LookupVaultPlan, error) {
	var result LookupVaultPlan
	if bank.slot <= 0 || len(bank.reservations) > 16 {
		return result, errors.New("lookup planning bank invalid")
	}
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioner_controls(cluster,paused,reason,updated_by,control_epoch) VALUES($1,false,'implicit unpaused provisioner control',$2,0) ON CONFLICT(cluster) DO NOTHING`, request.Cluster, request.Lease.Owner); err != nil {
			return err
		}
		var paused bool
		if err := tx.QueryRow(ctx, `SELECT paused FROM loyal_yield.lookup_table_provisioner_controls WHERE cluster=$1 FOR UPDATE`, request.Cluster).Scan(&paused); err != nil {
			return err
		}
		if paused {
			return ErrLookupPaused
		}
		manifest, err := sealLookupVaultDemandTx(ctx, tx, request, bank.slot)
		if err != nil {
			return err
		}
		result.ManifestID = manifest.ID
		result.SharedManifestID, result.CatalogRevisionID, err = sealLookupSharedRequestTx(ctx, tx, request, bank.slot)
		if err != nil {
			return err
		}
		if err = lookupRequestCatalogGuard(ctx, tx, request); err != nil {
			return err
		}
		var f lookupPlanningFamily
		var water int
		err = tx.QueryRow(ctx, `SELECT id,logical_name,provisioning_authority,payer,COALESCE(active_generation,0),hard_capacity,largest_atomic_expansion,safety_margin,allocation_high_water FROM loyal_yield.lookup_table_families WHERE id=$1 AND kind='vault_shards' AND desired_state='active' FOR SHARE`, manifest.FamilyID).Scan(&f.id, &f.name, &f.authority, &f.payer, &f.activeGeneration, &f.policy.HardCapacity, &f.policy.LargestAtomicExpansion, &f.policy.SafetyMargin, &water)
		if err != nil {
			return err
		}
		if bank.authority != f.authority || f.payer != f.authority || policy.HardCapacity != f.policy.HardCapacity || policy.LargestAtomicExpansion != f.policy.LargestAtomicExpansion || policy.SafetyMargin != f.policy.SafetyMargin || water != policy.HardCapacity-policy.LargestAtomicExpansion-policy.SafetyMargin {
			return errors.New("lookup durable family planner policy or authority changed")
		}
		var revision int64
		err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_vault_desired_heads(family_id,vault_id,binding_ordinal,manifest_id,desired_revision) VALUES($1,$2,0,$3,1) ON CONFLICT(family_id,vault_id,binding_ordinal) DO UPDATE SET manifest_id=EXCLUDED.manifest_id,desired_revision=CASE WHEN lookup_table_vault_desired_heads.manifest_id=EXCLUDED.manifest_id THEN lookup_table_vault_desired_heads.desired_revision ELSE lookup_table_vault_desired_heads.desired_revision+1 END,updated_at=CASE WHEN lookup_table_vault_desired_heads.manifest_id=EXCLUDED.manifest_id THEN lookup_table_vault_desired_heads.updated_at ELSE clock_timestamp() END RETURNING desired_revision`, f.id, request.VaultID, manifest.ID).Scan(&revision)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_vault_bindings SET lifecycle_state='failed',deactivated_at=COALESCE(deactivated_at,clock_timestamp()),updated_at=clock_timestamp() WHERE family_id=$1 AND vault_id=$2 AND binding_ordinal=0 AND lifecycle_state IN ('preparing','warming') AND (manifest_id<>$3 OR desired_head_revision<>$4)`, f.id, request.VaultID, manifest.ID, revision); err != nil {
			return err
		}
		if len(manifest.Addresses) == 0 {
			result.NotRequired = true
			return finishLookupPlanningRequestTx(ctx, tx, request, result, bank)
		}
		var currentID, currentTable, currentManifest, currentRevision int64
		var currentReserve int
		var currentState string
		err = tx.QueryRow(ctx, `SELECT id,route_lookup_table_id,manifest_id,desired_head_revision,reserved_capacity,lifecycle_state FROM loyal_yield.lookup_table_vault_bindings WHERE family_id=$1 AND vault_id=$2 AND binding_ordinal=0 AND lifecycle_state IN ('active','preparing','warming') ORDER BY CASE WHEN lifecycle_state='active' THEN 1 ELSE 0 END,id DESC LIMIT 1 FOR UPDATE`, f.id, request.VaultID).Scan(&currentID, &currentTable, &currentManifest, &currentRevision, &currentReserve, &currentState)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			currentID, currentTable, currentReserve = 0, 0, 0
		}
		// A terminal attribution is retained for operator repair, never silently
		// replaced with another packet or another reservation.
		if currentID != 0 && currentManifest == manifest.ID && currentRevision == revision {
			var failed bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations o WHERE binding_id=$1 AND manifest_id=$2 AND operation_state='permanent_failure' AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_terminal_repair_operations WHERE operation_id=o.id))`, currentID, manifest.ID).Scan(&failed); err != nil {
				return err
			}
			if failed {
				return errors.New("lookup binding has terminal operation requiring source repair")
			}
		}
		rows, err := tx.Query(ctx, `SELECT t.id,t.family_id,t.generation,t.shard_ordinal,t.reserved_address_count,t.allocation_high_water,(SELECT count(DISTINCT vault_id) FROM loyal_yield.lookup_table_vault_bindings WHERE route_lookup_table_id=t.id AND lifecycle_state IN ('preparing','warming','active','standby','retiring')),t.accepting_allocations AND (t.allocation_kind='vault_shard' OR t.id=$2),t.desired_state,COALESCE((SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=t.id),'{}'::text[]),COALESCE((SELECT array_agg(DISTINCT a.address ORDER BY a.address) FROM loyal_yield.lookup_table_operations o JOIN loyal_yield.lookup_table_operation_addresses a ON a.operation_id=o.id WHERE o.route_lookup_table_id=t.id AND o.operation_state NOT IN ('complete','permanent_failure','cancelled')),'{}'::text[]) FROM loyal_yield.route_lookup_tables t WHERE family_id=$1 AND allocation_kind IN ('vault_shard','dedicated_vault') AND desired_state IN ('preparing','warming','active','standby') AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations o WHERE o.route_lookup_table_id=t.id AND o.operation_kind IN ('create','rollover') AND o.operation_state='permanent_failure' AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_terminal_repair_operations WHERE operation_id=o.id)) ORDER BY t.id FOR UPDATE LIMIT 257`, f.id, currentTable)
		if err != nil {
			return err
		}
		var candidates []LookupShardCandidate
		for rows.Next() {
			var c LookupShardCandidate
			if err = rows.Scan(&c.TableID, &c.FamilyID, &c.Generation, &c.ShardOrdinal, &c.ReservedCount, &c.HighWater, &c.BoundVaults, &c.Accepting, &c.Lifecycle, &c.Confirmed, &c.Pending); err != nil {
				rows.Close()
				return err
			}
			if c.TableID == currentTable && currentState == "active" && (c.Generation != f.activeGeneration || c.Lifecycle != "active") {
				c.Lifecycle = "failed"
				c.Accepting = false
			}
			candidates = append(candidates, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(candidates) > 256 {
			return errors.New("lookup physical planning set exceeds bounded 256 tables")
		}
		addresses := make([]string, 0, len(manifest.Addresses))
		for _, a := range manifest.Addresses {
			addresses = append(addresses, a.Address)
		}
		var nextOrdinal int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE(max(shard_ordinal)::bigint+1,0) FROM loyal_yield.route_lookup_tables WHERE family_id=$1 AND generation=$2`, f.id, f.activeGeneration).Scan(&nextOrdinal); err != nil {
			return err
		}
		if nextOrdinal > math.MaxInt32 {
			return errors.New("lookup shard ordinal exhausted")
		}
		allocation, err := allocateLookupVault(LookupVaultDemand{Addresses: addresses, CurrentTableID: currentTable, CurrentReservedCapacity: currentReserve, NextGeneration: f.activeGeneration, NextShardOrdinal: int32(nextOrdinal)}, candidates, policy)
		if err != nil {
			return err
		}
		kind := LookupExtend
		contextValue := map[string]any{"request_id": request.ID, "requirementsFingerprint": request.Fingerprint}
		if allocation.TableID == 0 {
			if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, f.authority); err != nil {
				return err
			}
			var chosen *lookupReservation
			for _, r := range bank.reservations {
				derived, err := lookupDerivedAddress(f.authority, r.recentSlot)
				if err != nil || derived != r.address || r.recentSlot > uint64(bank.slot) {
					return errors.New("lookup actual PDA reservation proof changed")
				}
				var occupied bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.route_lookup_tables WHERE authority=$1 AND table_address=$2)`, f.authority, r.address).Scan(&occupied); err != nil {
					return err
				}
				if !occupied {
					copy := r
					chosen = &copy
					break
				}
			}
			if chosen == nil {
				return errors.New("lookup bounded actual-bank PDA reservations unavailable")
			}
			allocationKind := "vault_shard"
			if allocation.Dedicated {
				allocationKind = "dedicated_vault"
			}
			scope := fmt.Sprintf("reusable:%s:g%d:s%d", f.name, allocation.Generation, allocation.ShardOrdinal)
			err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.route_lookup_tables(cluster,scope,table_address,authority,payer,status,durable,address_count,address_hash,addresses,family_id,allocation_kind,generation,shard_ordinal,desired_state,accepting_allocations,allocation_high_water,reserved_address_count,usable_address_count,mutation_epoch) VALUES($1,$2,$3,$4,$4,'warming',true,0,'','[]'::jsonb,$5,$6,$7,$8,'preparing',$9,$10,0,0,0) RETURNING id`, request.Cluster, scope, chosen.address, f.authority, f.id, allocationKind, allocation.Generation, allocation.ShardOrdinal, !allocation.Dedicated, allocation.HighWater).Scan(&allocation.TableID)
			if err != nil {
				return err
			}
			allocation.FamilyID = f.id
			kind = LookupCreate
			contextValue["recent_slot"] = chosen.recentSlot
			contextValue["dedicated"] = allocation.Dedicated
		}
		result.TableID = allocation.TableID
		mode := "packed_shard"
		var tableAddress, tableKind string
		var epoch int64
		var generation, shard int32
		err = tx.QueryRow(ctx, `SELECT table_address,allocation_kind,mutation_epoch,generation,shard_ordinal FROM loyal_yield.route_lookup_tables WHERE id=$1`, allocation.TableID).Scan(&tableAddress, &tableKind, &epoch, &generation, &shard)
		if err != nil {
			return err
		}
		if tableKind == "dedicated_vault" {
			mode = "dedicated"
		}
		if currentID != 0 && currentTable == allocation.TableID && currentManifest == manifest.ID && currentRevision == revision {
			result.BindingID = currentID
		} else {
			var predecessor *int64
			if currentID != 0 && currentState == "active" {
				predecessor = &currentID
			}
			err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_vault_bindings(vault_id,family_id,route_lookup_table_id,manifest_id,binding_ordinal,desired_head_revision,allocation_mode,reserved_capacity,predecessor_binding_id,lifecycle_state) VALUES($1,$2,$3,$4,0,$5,$6,$7,$8,'preparing') RETURNING id`, request.VaultID, f.id, allocation.TableID, manifest.ID, revision, mode, len(addresses)+policy.GrowthReservation, predecessor).Scan(&result.BindingID)
			if err != nil {
				return err
			}
		}
		if err = tx.QueryRow(ctx, `SELECT COALESCE(min(id),0) FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=$1 AND operation_state NOT IN ('complete','permanent_failure','cancelled')`, allocation.TableID).Scan(&result.OperationID); err != nil {
			return err
		}
		if result.OperationID == 0 && len(allocation.Missing) > 0 {
			contextBytes, err := json.Marshal(contextValue)
			if err != nil {
				return err
			}
			intent := LookupIntent{Cluster: request.Cluster, FamilyID: f.id, TableID: allocation.TableID, Kind: kind, TableAddress: tableAddress, Authority: f.authority, Payer: f.payer, Generation: generation, MutationEpoch: epoch, Extension: allocation.Missing[:min(lookupMaximumChunk, len(allocation.Missing))]}
			result.OperationID, err = enqueueLookupTx(ctx, tx, lookupQueuedOperation{intent: intent, shard: shard, manifestID: &manifest.ID, bindingID: &result.BindingID, desiredHash: manifest.Hash, context: contextBytes})
			if err != nil {
				return err
			}
		}
		if result.OperationID == 0 && len(allocation.Missing) == 0 {
			var verified *int64
			var usable, count int
			var prefixHash string
			if err = tx.QueryRow(ctx, `SELECT last_verified_slot,usable_address_count,address_count,address_hash FROM loyal_yield.route_lookup_tables WHERE id=$1`, allocation.TableID).Scan(&verified, &usable, &count, &prefixHash); err != nil {
				return err
			}
			if verified == nil || usable != count {
				frontier := int64(-1)
				if verified != nil {
					frontier = *verified
				}
				key := lookupHashValues("vault-active-physical-verify", request.Cluster, fmt.Sprint(allocation.TableID), fmt.Sprint(epoch), fmt.Sprint(result.BindingID), fmt.Sprint(revision), fmt.Sprint(frontier), prefixHash)
				intent := LookupIntent{Cluster: request.Cluster, FamilyID: f.id, TableID: allocation.TableID, Kind: LookupVerify, TableAddress: tableAddress, Authority: f.authority, Payer: f.payer, Generation: generation, MutationEpoch: epoch}
				result.OperationID, err = enqueueLookupTx(ctx, tx, lookupQueuedOperation{intent: intent, shard: shard, manifestID: &manifest.ID, bindingID: &result.BindingID, desiredHash: manifest.Hash, context: []byte(`{}`), key: key})
				if err != nil {
					return err
				}
			}
		}
		return finishLookupPlanningRequestTx(ctx, tx, request, result, bank)
	})
	return result, err
}

func lookupRequestCatalogGuard(ctx context.Context, tx pgx.Tx, r LookupPlanningRequest) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_shared_market_catalog_heads h JOIN loyal_yield.lookup_table_families f ON f.id=h.family_id WHERE f.cluster=$1 AND f.kind='shared_market' AND f.desired_state='active') AND count(*)=$2 AND COALESCE(bool_and(c.address IS NOT NULL AND (NOT a.is_writable OR c.is_writable) AND array_remove(string_to_array(a.account_role,','),'') <@ array_remove(string_to_array(c.account_role,','),'')),true) FROM loyal_yield.lookup_table_provisioning_request_addresses a LEFT JOIN loyal_yield.lookup_table_shared_market_catalog_heads h ON h.family_id=(SELECT id FROM loyal_yield.lookup_table_families WHERE cluster=$1 AND kind='shared_market' AND desired_state='active') LEFT JOIN loyal_yield.lookup_table_shared_market_catalog_revisions rev ON rev.id=h.catalog_revision_id LEFT JOIN loyal_yield.lookup_table_manifest_addresses c ON c.manifest_id=rev.manifest_id AND c.address=a.address WHERE a.request_id=$3 AND a.semantic_class='shared_market'`, r.Cluster, r.SharedCount, r.ID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("lookup route shared roles/writability drift from authoritative catalog")
	}
	return nil
}

func finishLookupPlanningRequestTx(ctx context.Context, tx pgx.Tx, r LookupPlanningRequest, p LookupVaultPlan, bank lookupPlanningBank) error {
	var sameHead bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_shared_market_catalog_heads h JOIN loyal_yield.lookup_table_families f ON f.id=h.family_id WHERE f.cluster=$1 AND f.kind='shared_market' AND f.desired_state='active' AND h.catalog_revision_id=$2)`, r.Cluster, p.CatalogRevisionID).Scan(&sameHead); err != nil {
		return err
	}
	if !sameHead {
		return errors.New("lookup authoritative catalog changed during vault allocation")
	}
	satisfied, err := lookupPlanningSatisfiedTx(ctx, tx, r, p, bank)
	if err != nil {
		return err
	}
	status := "queued"
	if satisfied {
		status = "satisfied"
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET vault_manifest_id=$4,shared_manifest_id=$5,request_status=$6,satisfied_at=CASE WHEN $6='satisfied' THEN COALESCE(satisfied_at,clock_timestamp()) ELSE satisfied_at END,next_attempt_at=CASE WHEN $6='satisfied' THEN NULL ELSE clock_timestamp()+interval '5 seconds' END,lease_owner=NULL,lease_expires_at=NULL,error_code=NULL,error_detail=NULL,updated_at=clock_timestamp() WHERE id=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp() AND request_status='planning'`, r.ID, r.Lease.Owner, r.Lease.FencingToken, p.ManifestID, p.SharedManifestID, status)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return err
}
