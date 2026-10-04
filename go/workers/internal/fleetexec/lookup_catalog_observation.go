package fleetexec

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

func (s *Store) lookupCatalogTables(ctx context.Context, c lookupCatalog, generation int32) ([]lookupCatalogPhysical, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.id,t.mutation_epoch,t.table_address,t.desired_state,t.shard_ordinal,COALESCE((SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=t.id),'{}'::text[]),(SELECT count(*) FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=t.id AND operation_state NOT IN ('complete','permanent_failure','cancelled')),t.last_verified_slot,t.last_extended_slot FROM loyal_yield.route_lookup_tables t WHERE family_id=$1 AND generation=$2 AND allocation_kind='shared_market' AND desired_state NOT IN ('closed','deactivated','failed') ORDER BY shard_ordinal,id LIMIT 17`, c.familyID, generation)
	if err != nil {
		return nil, err
	}
	var tables []lookupCatalogPhysical
	for rows.Next() {
		var t lookupCatalogPhysical
		if err = rows.Scan(&t.id, &t.epoch, &t.address, &t.state, &t.shard, &t.confirmed, &t.nonterminal, &t.verified, &t.lastExtended); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(tables) > 16 {
		return nil, errors.New("lookup catalog observation exceeds bounded sixteen shards")
	}
	return tables, nil
}

// One coherent finalized bundle read proves every shard from the same bank.
func (p *LookupPlanner) observeLookupCatalog(ctx context.Context, c lookupCatalog, generation int32, minSlot int64, reportDrift bool) (bool, uint64, error) {
	tables, err := p.store.lookupCatalogTables(ctx, c, generation)
	if err != nil {
		return false, 0, err
	}
	if len(tables) == 0 {
		return false, uint64(minSlot), nil
	}
	addresses := make([]string, 0, len(tables))
	for _, t := range tables {
		addresses = append(addresses, t.address)
		if t.verified != nil {
			minSlot = max(minSlot, *t.verified)
		}
	}
	observations, err := p.chain.lookupCatalogObservations(ctx, addresses, minSlot)
	if err != nil {
		return false, 0, err
	}
	bank := uint64(observations[0].snapshot.Slot)
	shards, err := lookupCatalogShards(c.addresses, c.capacity)
	if err != nil {
		return false, bank, err
	}
	var snapshots []LookupSnapshot
	ready := len(tables) == len(shards)
	for n, t := range tables {
		o := observations[n]
		s := o.snapshot
		exact := o.reason == "" && !s.Absent && s.Authority == c.authority && s.DeactivationSlot == math.MaxUint64 && s.LastExtendedSlot < uint64(s.Slot) && t.lastExtended != nil && s.LastExtendedSlot == uint64(*t.lastExtended) && lookupSameAddresses(t.confirmed, s.Addresses) && t.shard >= 0 && int(t.shard) < len(shards) && lookupSameAddresses(t.confirmed, shards[t.shard])
		if reportDrift && !exact {
			if t.nonterminal != 0 {
				return false, bank, errors.New("lookup active catalog drift overlaps owned source mutation; reconcile first")
			}
			if o.reason == "" {
				o.reason = "finalized_shared_table_identity_or_membership_drift"
				if s.LastExtendedSlot >= uint64(s.Slot) {
					o.reason = "finalized_shared_table_not_warm"
				}
			}
			if err = p.store.reportLookupCatalogDrift(ctx, c, t, o, p.config.Owner); err != nil {
				return false, bank, err
			}
			return true, bank, nil
		}
		ready = ready && exact && t.nonterminal == 0 && t.verified != nil && (t.state == "active" || t.state == "standby")
		snapshots = append(snapshots, s)
	}
	if !ready {
		return false, bank, nil
	}
	if err = p.store.ActivateLookupCatalog(ctx, c.cluster, c.familyID, c.revisionID, generation, snapshots); err != nil {
		return false, bank, err
	}
	return c.state != "active" || c.active == nil || *c.active != generation, bank, nil
}

func (p *LookupPlanner) reconcileLookupCatalog(ctx context.Context, minSlot int64) (bool, uint64, error) {
	c, err := p.store.loadLookupCatalog(ctx, p.config.Cluster)
	if err != nil || c == nil {
		return false, uint64(minSlot), err
	}
	worked := false
	bank := uint64(minSlot)
	if c.state == "active" && c.active != nil && c.target != nil && *c.active == *c.target {
		changed, observed, e := p.observeLookupCatalog(ctx, *c, *c.active, minSlot, true)
		bank = max(bank, observed)
		if e != nil {
			return worked, bank, e
		}
		worked = worked || changed
		if changed {
			c, err = p.store.loadLookupCatalog(ctx, p.config.Cluster)
			if err != nil || c == nil {
				return worked, bank, err
			}
		}
	}
	if !p.config.ReconcileOnly {
		proof, e := p.planningBank(ctx, c.authority, int64(bank))
		if e != nil {
			return worked, bank, e
		}
		bank = max(bank, uint64(proof.slot))
		_, changed, e := p.store.planLookupCatalog(ctx, *c, proof)
		if e != nil {
			return worked, bank, e
		}
		worked = worked || changed
		c, err = p.store.loadLookupCatalog(ctx, p.config.Cluster)
		if err != nil || c == nil {
			return worked, bank, err
		}
		// Planning changes the source head state; activation rechecks its exact
		// immutable revision and every source physical projection under locks.
	}
	if c.target != nil && c.state != "failed" {
		changed, observed, e := p.observeLookupCatalog(ctx, *c, *c.target, int64(bank), false)
		bank = max(bank, observed)
		if e != nil {
			return worked, bank, e
		}
		worked = worked || changed
	}
	return worked, bank, nil
}

func (s *Store) reportLookupCatalogDrift(ctx context.Context, c lookupCatalog, t lookupCatalogPhysical, o lookupCatalogObservation, owner string) error {
	snapshot := o.snapshot
	if snapshot.Slot <= 0 || snapshot.Address != t.address || o.reason == "" || owner == "" || (t.verified != nil && snapshot.Slot < *t.verified) {
		return errors.New("lookup catalog drift lacks exact finalized frontier")
	}
	var authority *string
	var extended *int64
	active, warm := false, false
	if o.reason != "finalized_shared_table_missing" && o.reason != "finalized_shared_table_owner_drift" && o.reason != "finalized_shared_table_decode_drift" {
		if snapshot.LastExtendedSlot > math.MaxInt64 {
			return errors.New("lookup drift metadata exceeds source range")
		}
		if snapshot.Authority != "" {
			v := snapshot.Authority
			authority = &v
		}
		e := int64(snapshot.LastExtendedSlot)
		extended = &e
		active = snapshot.DeactivationSlot == math.MaxUint64
		warm = snapshot.LastExtendedSlot < uint64(snapshot.Slot)
	}
	observedAddresses := append([]string{}, snapshot.Addresses...)
	observedHash := lookupOrderedAddressHash(observedAddresses)
	observedAuthority := ""
	if authority != nil {
		observedAuthority = *authority
	}
	observedExtended := ""
	if extended != nil {
		observedExtended = strconv.FormatInt(*extended, 10)
	}
	values := []string{c.cluster, strconv.FormatInt(c.revisionID, 10), strconv.FormatInt(c.familyID, 10), strconv.FormatInt(t.id, 10), strconv.FormatInt(t.epoch, 10), t.address, c.authority, strconv.FormatInt(snapshot.Slot, 10), strconv.FormatBool(!snapshot.Absent), observedAuthority, strconv.FormatBool(active), observedExtended, strconv.FormatBool(warm), observedHash, o.reason, owner}
	values = append(values, observedAddresses...)
	hash := lookupHashValues(values...)
	addressesJSON, err := json.Marshal(observedAddresses)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('shared-alt-drift:'||$1,0))`, c.cluster); err != nil {
			return err
		}
		var exact bool
		err := tx.QueryRow(ctx, `SELECT h.catalog_revision_id=$2 AND h.readiness_state='active' AND f.active_generation IS NOT NULL AND f.active_generation=h.target_generation FROM loyal_yield.lookup_table_shared_market_catalog_heads h JOIN loyal_yield.lookup_table_families f ON f.id=h.family_id WHERE h.family_id=$1 FOR UPDATE OF h,f`, c.familyID, c.revisionID).Scan(&exact)
		if err != nil {
			return err
		}
		if !exact {
			return errors.New("lookup drift lost active catalog fence")
		}
		var membership []string
		err = tx.QueryRow(ctx, `SELECT cluster=$2 AND family_id=$3 AND table_address=$4 AND authority=$5 AND mutation_epoch=$6 AND generation=$7 AND last_verified_slot<=$8 AND last_extended_slot IS NOT DISTINCT FROM $9::bigint,COALESCE((SELECT array_agg(address ORDER BY ordinal) FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=t.id),'{}'::text[]) FROM loyal_yield.route_lookup_tables t WHERE id=$1 AND allocation_kind='shared_market' FOR UPDATE`, t.id, c.cluster, c.familyID, t.address, c.authority, t.epoch, c.active, snapshot.Slot, t.lastExtended).Scan(&exact, &membership)
		if err != nil {
			return err
		}
		if !exact || !lookupSameAddresses(membership, t.confirmed) {
			return errors.New("lookup drift source physical incarnation changed")
		}
		var pending bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=$1 AND operation_state NOT IN ('complete','permanent_failure','cancelled')) OR EXISTS(SELECT 1 FROM loyal_yield.lookup_table_signed_attempts WHERE route_lookup_table_id=$1 AND attempt_state NOT IN ('reconciled','failed','expired'))`, t.id).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return errors.New("lookup drift overlaps unresolved source custody")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_physical_drifts(evidence_hash,cluster,family_id,catalog_revision_id,route_lookup_table_id,expected_mutation_epoch,expected_table_address,expected_authority,observed_slot,observed_table_present,observed_authority,observed_active,observed_last_extended_slot,observed_warm,observed_address_hash,observed_addresses,reason,reported_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) ON CONFLICT(evidence_hash) DO NOTHING`, hash, c.cluster, c.familyID, c.revisionID, t.id, t.epoch, t.address, c.authority, snapshot.Slot, !snapshot.Absent, authority, active, extended, warm, observedHash, addressesJSON, o.reason, owner); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.lookup_table_shared_market_catalog_heads SET readiness_state='provisioning',activated_at=NULL,updated_at=clock_timestamp() WHERE family_id=$1 AND catalog_revision_id=$2`, c.familyID, c.revisionID)
		return err
	})
}
