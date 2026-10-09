package fleetexec

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

type LookupPlannerConfig struct {
	Cluster, Owner                                        string
	LeaseTTL, TickDeadline, PollInterval, CatalogInterval time.Duration
	GrowthReservation, MaximumVaultCohort                 int
	ReconcileOnly                                         bool
	Facts                                                 *engine.Facts
	OnHealth                                              func(error)
}

// LookupPlanner has no signing callback or private key. The provisioner worker
// independently owns each source operation and authorizes exact packet IO.
type LookupPlanner struct {
	store       *Store
	chain       *LookupRPC
	config      LookupPlannerConfig
	gate        chan struct{}
	nextCatalog time.Time
}

func NewLookupPlanner(store *Store, chain *LookupRPC, config LookupPlannerConfig) (*LookupPlanner, error) {
	if store == nil || store.pool == nil || chain == nil || config.Cluster == "" || config.Owner == "" || config.LeaseTTL < 10*time.Second || config.LeaseTTL > 5*time.Minute || config.LeaseTTL%time.Second != 0 || config.TickDeadline <= 0 || config.TickDeadline+5*time.Second > config.LeaseTTL || config.PollInterval <= 0 || config.PollInterval > time.Minute || config.CatalogInterval < time.Second || config.CatalogInterval > time.Hour || config.GrowthReservation < 0 || config.GrowthReservation > 256 || config.MaximumVaultCohort < 1 || config.MaximumVaultCohort > 65535 || config.Facts == nil {
		return nil, errors.New("lookup planner configuration invalid")
	}
	return &LookupPlanner{store: store, chain: chain, config: config, gate: make(chan struct{}, 1)}, nil
}

func (p *LookupPlanner) report(err error) {
	if err == nil {
		p.config.Facts.Progress(engine.FamilyLookup)
	}
	if p.config.OnHealth != nil {
		p.config.OnHealth(err)
	}
}

func (p *LookupPlanner) Run(ctx context.Context) error {
	backoff := p.config.PollInterval
	for {
		_, err := p.Tick(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			backoff = min(time.Minute, max(5*time.Second, backoff*2))
		} else {
			backoff = p.config.PollInterval
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *LookupPlanner) Tick(ctx context.Context) (worked bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, p.config.TickDeadline)
	defer cancel()
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-ctx.Done():
		p.report(ctx.Err())
		return false, ctx.Err()
	}
	defer func() { p.report(err) }()
	_, _, bank, err := p.chain.LookupBlockhash(ctx)
	if err != nil {
		return false, err
	}
	// Independent retirement gets a bounded turn before request/catalog errors.
	// A permanently malformed planning request must not strand rent forever.
	if !p.config.ReconcileOnly {
		cleanupWorked, _, e := p.cleanupTick(ctx, bank)
		worked = cleanupWorked
		if e != nil {
			return worked, e
		}
	}
	if !time.Now().Before(p.nextCatalog) {
		catalogWorked, _, e := p.reconcileLookupCatalog(ctx, bank)
		if e != nil {
			return worked, e
		}
		worked = worked || catalogWorked
		p.nextCatalog = time.Now().Add(p.config.CatalogInterval)
		if catalogWorked {
			p.nextCatalog = time.Now().Add(p.config.PollInterval)
		}
	}
	// Existing candidates may publish only through actual bank evidence. This
	// does not create a new mutation, including in reconcile-only mode.
	//
	// As in the source activate_binding_if_ready, a binding is a candidate only
	// once its table's confirmed membership covers its sealed manifest. A packed
	// binding reserved while another vault's mutation held the table has no
	// operation of its own yet; its request re-plan queues that extend once the
	// table is idle. Such a binding is not ready, and it must not stop this tick
	// from reaching the planning step that covers it.
	candidates, err := p.lookupActivationCandidates(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range candidates {
		snapshot, e := p.chain.LookupSnapshot(ctx, c.table, bank)
		if e != nil {
			return false, e
		}
		// The source defers a head still in use for its own vault; that vault
		// waits while the next candidate and request planning proceed.
		e = p.store.ActivateLookupBinding(ctx, c.binding, snapshot)
		if errors.Is(e, errLookupBindingDeferred) {
			continue
		}
		if e != nil {
			return false, e
		}
		worked = true
		break
	}
	if p.config.ReconcileOnly {
		return worked, nil
	}
	// This census takes no lease and changes no attempt counter. RPC inputs still
	// precede the actual request lease, but idle families avoid sixteen PDA reads.
	vault, e := p.store.nextLookupPlanningVault(ctx, p.config.Cluster)
	if e != nil || vault == 0 {
		return worked, e
	}
	var family lookupPlanningFamily
	err = p.store.pool.QueryRow(ctx, `SELECT id,logical_name,provisioning_authority,payer,COALESCE(active_generation,0),hard_capacity,largest_atomic_expansion,safety_margin FROM loyal_yield.lookup_table_families WHERE cluster=$1 AND kind='vault_shards' AND desired_state='active'`, p.config.Cluster).Scan(&family.id, &family.name, &family.authority, &family.payer, &family.activeGeneration, &family.policy.HardCapacity, &family.policy.LargestAtomicExpansion, &family.policy.SafetyMargin)
	if err == nil {
		family.policy.GrowthReservation = p.config.GrowthReservation
		family.policy.MaximumVaultCohort = p.config.MaximumVaultCohort
		proof, e := p.planningBank(ctx, family.authority, bank)
		if e != nil {
			return worked, e
		}
		if e = p.readPlanningReadiness(ctx, vault, &proof); e != nil {
			return worked, e
		}
		r, e := p.store.LeaseLookupPlanningRequest(ctx, p.config.Cluster, p.config.Owner, p.config.LeaseTTL)
		if e != nil {
			return worked, e
		}
		if r != nil {
			_, e = p.store.planLookupVaultRequest(ctx, *r, family.policy, proof)
			if e != nil {
				if deferErr := p.store.deferLookupPlanningRequest(ctx, *r, e.Error()); deferErr != nil {
					return worked, errors.Join(e, deferErr)
				}
				return worked, e
			}
			worked = true
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return worked, err
	}
	return worked, nil
}

type lookupActivationCandidate struct {
	binding int64
	table   string
}

// lookupActivationCandidates returns a bounded set of newest-revision bindings
// whose physical table is idle, verified and already contains every sealed
// manifest address. Rows are read fully before any RPC.
func (p *LookupPlanner) lookupActivationCandidates(ctx context.Context) ([]lookupActivationCandidate, error) {
	rows, err := p.store.pool.Query(ctx, `SELECT b.id,t.table_address FROM loyal_yield.lookup_table_vault_bindings b JOIN loyal_yield.lookup_table_families f ON f.id=b.family_id JOIN loyal_yield.route_lookup_tables t ON t.id=b.route_lookup_table_id JOIN loyal_yield.lookup_table_vault_desired_heads h ON h.family_id=b.family_id AND h.vault_id=b.vault_id AND h.binding_ordinal=b.binding_ordinal AND h.manifest_id=b.manifest_id AND h.desired_revision=b.desired_head_revision WHERE f.cluster=$1 AND f.kind='vault_shards' AND f.desired_state='active' AND b.lifecycle_state IN ('preparing','warming') AND t.generation=f.active_generation AND t.desired_state='active' AND t.status='usable' AND t.usable_address_count=t.address_count AND t.last_verified_slot IS NOT NULL AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations WHERE route_lookup_table_id=t.id AND operation_state NOT IN ('complete','permanent_failure','cancelled')) AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_manifest_addresses m WHERE m.manifest_id=b.manifest_id AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_addresses a WHERE a.route_lookup_table_id=t.id AND a.address=m.address)) ORDER BY b.updated_at,b.id LIMIT $2`, p.config.Cluster, lookupActivationBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []lookupActivationCandidate
	for rows.Next() {
		var c lookupActivationCandidate
		if err = rows.Scan(&c.binding, &c.table); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// lookupActivationBatch bounds per-tick snapshot reads when heads are deferred.
const lookupActivationBatch = 8

// cleanupTick queues deactivation/close for tables already retiring. It
// never finalizes a rollback: as in the source provisioner, retiring expired
// standby bindings is the explicit operator transition
// (--finalize-rollbacks, FinalizeLookupRollback), not a worker inference.
func (p *LookupPlanner) cleanupTick(ctx context.Context, bank int64) (worked bool, observed uint64, err error) {
	candidate, e := p.store.nextLookupCleanup(ctx, p.config.Cluster)
	if e != nil {
		return worked, observed, e
	}
	if candidate != nil {
		snapshot, e := p.chain.LookupSnapshot(ctx, candidate.intent.TableAddress, bank)
		if e != nil {
			return worked, observed, e
		}
		id, e := p.store.queueLookupCleanup(ctx, *candidate, snapshot)
		if e != nil {
			return worked, observed, e
		}
		worked = worked || id != 0
	}
	return worked, observed, nil
}

func (p *LookupPlanner) planningBank(ctx context.Context, authority string, minSlot int64) (lookupPlanningBank, error) {
	proof := lookupPlanningBank{authority: authority}
	// A coherent absent PDA read also contains the actual bank's SlotHashes.
	address, err := lookupDerivedAddress(authority, uint64(minSlot))
	if err != nil {
		return proof, err
	}
	first, err := p.chain.LookupSnapshot(ctx, address, minSlot)
	if err != nil {
		return proof, err
	}
	proof.slot = first.Slot
	for _, slot := range first.SlotHashes[:min(16, len(first.SlotHashes))] {
		if slot > uint64(first.Slot) {
			return proof, errors.New("lookup produced slot exceeds finalized bank")
		}
		address, err = lookupDerivedAddress(authority, slot)
		if err != nil {
			return proof, err
		}
		snapshot, e := p.chain.LookupSnapshot(ctx, address, first.Slot)
		if e != nil {
			return proof, e
		}
		proof.slot = max(proof.slot, snapshot.Slot)
		if snapshot.Absent && lookupProducedSlot(snapshot.SlotHashes, slot) {
			proof.reservations = append(proof.reservations, lookupReservation{address: address, recentSlot: slot})
		}
	}
	return proof, nil
}

func (s *Store) deferLookupPlanningRequest(ctx context.Context, r LookupPlanningRequest, reason string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET request_status='failed',next_attempt_at=clock_timestamp()+interval '5 seconds',lease_owner=NULL,lease_expires_at=NULL,error_code='go_planning_hold',error_detail=$4,updated_at=clock_timestamp() WHERE id=$1 AND request_status='planning' AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`, r.ID, r.Lease.Owner, r.Lease.FencingToken, reason)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return err
}
