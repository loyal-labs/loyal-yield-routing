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

type lookupCatalog struct {
	cluster, name, authority, payer, version, hash, state string
	familyID, revisionID, manifestID                      int64
	active, target                                        *int32
	capacity                                              int
	descriptors                                           []LookupManifestAddress
	addresses                                             []string
}
type lookupCatalogPhysical struct {
	id, epoch              int64
	address, state         string
	shard                  int32
	confirmed, pending     []string
	nonterminal            int
	verified, lastExtended *int64
}

func (s *Store) loadLookupCatalog(ctx context.Context, cluster string) (*lookupCatalog, error) {
	var c lookupCatalog
	var count int
	err := s.pool.QueryRow(ctx, `SELECT f.cluster,f.logical_name,f.provisioning_authority,f.payer,f.catalog_version,r.desired_set_hash,h.readiness_state,f.id,h.catalog_revision_id,r.manifest_id,f.active_generation,h.target_generation,f.allocation_high_water,r.address_count FROM loyal_yield.lookup_table_families f JOIN loyal_yield.lookup_table_shared_market_catalog_heads h ON h.family_id=f.id JOIN loyal_yield.lookup_table_shared_market_catalog_revisions r ON r.id=h.catalog_revision_id JOIN loyal_yield.lookup_table_manifests m ON m.id=r.manifest_id WHERE f.cluster=$1 AND f.kind='shared_market' AND f.desired_state='active' AND r.family_id=f.id AND r.catalog_version=f.catalog_version AND m.sealed_at IS NOT NULL`, cluster).Scan(&c.cluster, &c.name, &c.authority, &c.payer, &c.version, &c.hash, &c.state, &c.familyID, &c.revisionID, &c.manifestID, &c.active, &c.target, &c.capacity, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if count <= 0 || count > 4096 || c.capacity <= 0 || c.capacity > 256 || c.authority != c.payer {
		return nil, errors.New("lookup bounded catalog family identity invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT address,semantic_class,account_role,ordinal,is_writable FROM loyal_yield.lookup_table_manifest_addresses WHERE manifest_id=$1 ORDER BY ordinal`, c.manifestID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a LookupManifestAddress
		if err = rows.Scan(&a.Address, &a.SemanticClass, &a.Role, &a.Ordinal, &a.Writable); err != nil {
			rows.Close()
			return nil, err
		}
		if a.SemanticClass != "shared_market" || a.Ordinal != int32(len(c.descriptors)) {
			rows.Close()
			return nil, errors.New("lookup authoritative catalog order/class changed")
		}
		c.descriptors = append(c.descriptors, a)
		c.addresses = append(c.addresses, a.Address)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(c.addresses) != count || lookupManifestHash(c.descriptors) != c.hash {
		return nil, errors.New("lookup authoritative catalog exact sealed hash changed")
	}
	return &c, nil
}

func lookupCatalogGenerationTx(ctx context.Context, tx pgx.Tx, familyID int64, generation int32) ([]lookupCatalogPhysical, int, error) {
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.route_lookup_tables WHERE family_id=$1 AND generation=$2 AND allocation_kind='shared_market'`, familyID, generation).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(ctx, `SELECT t.id,t.mutation_epoch,t.table_address,t.desired_state,t.shard_ordinal,COALESCE((SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=t.id),'{}'::text[]),COALESCE((SELECT array_agg(a.address ORDER BY o.created_at,o.id,a.ordinal) FROM loyal_yield.lookup_table_operations o JOIN loyal_yield.lookup_table_operation_addresses a ON a.operation_id=o.id WHERE o.route_lookup_table_id=t.id AND o.operation_state NOT IN ('complete','permanent_failure','cancelled')),'{}'::text[]),(SELECT count(*) FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=t.id AND operation_state NOT IN ('complete','permanent_failure','cancelled')),t.last_verified_slot,t.last_extended_slot FROM loyal_yield.route_lookup_tables t WHERE t.family_id=$1 AND t.generation=$2 AND allocation_kind='shared_market' AND desired_state NOT IN ('deactivated','closed','failed') ORDER BY t.id FOR UPDATE LIMIT 17`, familyID, generation)
	if err != nil {
		return nil, 0, err
	}
	var tables []lookupCatalogPhysical
	for rows.Next() {
		var t lookupCatalogPhysical
		if err = rows.Scan(&t.id, &t.epoch, &t.address, &t.state, &t.shard, &t.confirmed, &t.pending, &t.nonterminal, &t.verified, &t.lastExtended); err != nil {
			rows.Close()
			return nil, 0, err
		}
		tables = append(tables, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	if len(tables) > 16 {
		return nil, 0, errors.New("lookup physical catalog exceeds bounded sixteen shards")
	}
	return tables, total, nil
}

func lookupCatalogCompatible(tables []lookupCatalogPhysical, total int, shards [][]string) bool {
	if total != len(tables) || len(tables) > len(shards) {
		return false
	}
	seen := map[int32]bool{}
	for _, t := range tables {
		// A cancelled unsigned create leaves a reserved PDA with no observed
		// physical incarnation. Treat it as an obsolete partial generation;
		// selecting Extend solely because its source row exists is invalid.
		if len(t.confirmed) == 0 && t.nonterminal == 0 && t.lastExtended == nil {
			return false
		}
		if t.shard < 0 || int(t.shard) >= len(shards) || seen[t.shard] {
			return false
		}
		seen[t.shard] = true
		combined := append(append([]string{}, t.confirmed...), t.pending...)
		desired := shards[t.shard]
		if len(combined) > len(desired) || !lookupSameAddresses(combined, desired[:min(len(combined), len(desired))]) {
			return false
		}
	}
	return true
}

// The authoritative sealed head supplies demand. Route requests cannot change
// the shared catalog or cause a different shard order to be published.
func (s *Store) planLookupCatalog(ctx context.Context, c lookupCatalog, bank lookupPlanningBank) (int32, bool, error) {
	var target int32
	changed := false
	shards, err := lookupCatalogShards(c.addresses, c.capacity)
	if err != nil {
		return target, false, err
	}
	if len(shards) > 16 {
		return target, false, errors.New("lookup catalog needs more than sixteen bounded shards")
	}
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		i := LookupIntent{Cluster: c.cluster, FamilyID: c.familyID, Authority: c.authority, Payer: c.payer}
		paused, err := lookupControlLock(ctx, tx, i, "go-lookup-catalog")
		if err != nil {
			return err
		}
		if paused {
			return ErrLookupPaused
		}
		var exact bool
		if err = tx.QueryRow(ctx, `SELECT catalog_revision_id=$2 FROM loyal_yield.lookup_table_shared_market_catalog_heads WHERE family_id=$1 FOR UPDATE`, c.familyID, c.revisionID).Scan(&exact); err != nil {
			return err
		}
		if !exact {
			return errors.New("lookup catalog changed before planning")
		}
		var active *int32
		if err = tx.QueryRow(ctx, `SELECT active_generation,kind='shared_market' AND desired_state='active' AND cluster=$2 AND provisioning_authority=$3 AND payer=$3 AND catalog_version=$4 AND allocation_high_water=$5 FROM loyal_yield.lookup_table_families WHERE id=$1 FOR UPDATE`, c.familyID, c.cluster, c.authority, c.version, c.capacity).Scan(&active, &exact); err != nil {
			return err
		}
		if !exact || bank.authority != c.authority || bank.slot <= 0 {
			return errors.New("lookup catalog family/bank changed")
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations o SET operation_state='cancelled',next_attempt_at=NULL,error_code='superseded_shared_market_catalog',error_detail='unsigned source catalog superseded',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE family_id=$1 AND manifest_id IS DISTINCT FROM $2 AND operation_kind IN ('create','extend','rollover') AND (operation_state IN ('queued','retry_wait') OR operation_state='leased' AND lease_expires_at<=clock_timestamp()) AND transaction_signature IS NULL AND message_hash IS NULL AND recent_blockhash IS NULL AND last_valid_block_height IS NULL AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_signed_attempts WHERE operation_id=o.id AND attempt_state NOT IN ('reconciled','failed','expired'))`, c.familyID, c.manifestID); err != nil {
			return err
		}
		target = 0
		if active != nil {
			target = *active
		}
		tables, total, err := lookupCatalogGenerationTx(ctx, tx, c.familyID, target)
		if err != nil {
			return err
		}
		var drift bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_shared_market_physical_drifts d JOIN loyal_yield.route_lookup_tables t ON t.id=d.route_lookup_table_id WHERE d.family_id=$1 AND d.catalog_revision_id=$2 AND d.resolution_state='open' AND t.generation=$3)`, c.familyID, c.revisionID, target).Scan(&drift); err != nil {
			return err
		}
		if drift || !lookupCatalogCompatible(tables, total, shards) {
			reuse := false
			if c.target != nil && *c.target != target {
				candidate, n, e := lookupCatalogGenerationTx(ctx, tx, c.familyID, *c.target)
				if e != nil {
					return e
				}
				if lookupCatalogCompatible(candidate, n, shards) {
					target = *c.target
					tables = candidate
					reuse = true
				}
			}
			if !reuse {
				var next int64
				if err = tx.QueryRow(ctx, `SELECT COALESCE(max(generation)::bigint,-1)+1 FROM loyal_yield.route_lookup_tables WHERE family_id=$1`, c.familyID).Scan(&next); err != nil {
					return err
				}
				if next > math.MaxInt32 {
					return errors.New("lookup catalog generation exhausted")
				}
				target = int32(next)
				tables = nil
			}
		}
		for ordinal, desired := range shards {
			var table *lookupCatalogPhysical
			for n := range tables {
				if tables[n].shard == int32(ordinal) {
					table = &tables[n]
					break
				}
			}
			var confirmed, pending []string
			if table != nil {
				if table.nonterminal != 0 {
					continue
				}
				confirmed = table.confirmed
				pending = table.pending
			}
			kind, chunk, e := nextLookupCatalogMutation(table != nil, desired, confirmed, pending, lookupMaximumChunk)
			if e != nil {
				return e
			}
			if kind == "" {
				continue
			}
			contextValue := map[string]any{"catalog_revision_id": c.revisionID}
			if table == nil {
				if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, c.authority); err != nil {
					return err
				}
				var chosen *lookupReservation
				for _, r := range bank.reservations {
					derived, e := lookupDerivedAddress(c.authority, r.recentSlot)
					if e != nil || derived != r.address || r.recentSlot > uint64(bank.slot) {
						return errors.New("lookup catalog actual PDA proof changed")
					}
					var occupied bool
					if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.route_lookup_tables WHERE authority=$1 AND table_address=$2)`, c.authority, r.address).Scan(&occupied); err != nil {
						return err
					}
					if !occupied {
						copy := r
						chosen = &copy
						break
					}
				}
				if chosen == nil {
					return errors.New("lookup catalog bounded absent PDA set exhausted")
				}
				table = &lookupCatalogPhysical{address: chosen.address, shard: int32(ordinal)}
				scope := fmt.Sprintf("reusable:%s:g%d:s%d", c.name, target, ordinal)
				err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.route_lookup_tables(cluster,scope,table_address,authority,payer,status,durable,address_count,address_hash,addresses,family_id,allocation_kind,generation,shard_ordinal,desired_state,accepting_allocations,allocation_high_water,reserved_address_count,usable_address_count,mutation_epoch) VALUES($1,$2,$3,$4,$4,'warming',true,0,'','[]'::jsonb,$5,'shared_market',$6,$7,'preparing',false,$8,0,0,0) RETURNING id`, c.cluster, scope, chosen.address, c.authority, c.familyID, target, ordinal, c.capacity).Scan(&table.id)
				if err != nil {
					return err
				}
				contextValue["recent_slot"] = chosen.recentSlot
			}
			contextBytes, e := json.Marshal(contextValue)
			if e != nil {
				return e
			}
			i := LookupIntent{Cluster: c.cluster, FamilyID: c.familyID, TableID: table.id, Kind: kind, TableAddress: table.address, Authority: c.authority, Payer: c.payer, Generation: target, MutationEpoch: table.epoch, Extension: chunk}
			if _, err = enqueueLookupTx(ctx, tx, lookupQueuedOperation{intent: i, shard: int32(ordinal), manifestID: &c.manifestID, desiredHash: lookupOrderedAddressHash(desired), context: contextBytes}); err != nil {
				return err
			}
			changed = true
		}
		var failed bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations WHERE family_id=$1 AND manifest_id=$2 AND operation_state='permanent_failure')`, c.familyID, c.manifestID).Scan(&failed); err != nil {
			return err
		}
		state := "provisioning"
		if failed {
			state = "failed"
		} else if c.state == "active" && c.target != nil && *c.target == target {
			state = "active"
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_shared_market_catalog_heads SET target_generation=$3,readiness_state=$4,activated_at=CASE WHEN $4='active' THEN activated_at ELSE NULL END,updated_at=clock_timestamp() WHERE family_id=$1 AND catalog_revision_id=$2`, c.familyID, c.revisionID, target, state)
		return err
	})
	return target, changed, err
}
