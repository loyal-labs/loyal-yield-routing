package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

type MarketEpochSource interface {
	LoadImmutableMarketEpoch(context.Context) (ImmutableMarketEpoch, error)
}

type Worker struct {
	config            Config
	store             *Store
	rpc               *RPCClient
	marketEvidence    MarketEpochSource
	revalidator       *Revalidator
	shadowRevalidator *Revalidator
	shadowSeen        shadowSeen
	lastConfirmedSlot int64
	facts             *engine.Facts
	voltr             *VoltrRoute
	nextVoltr         time.Time
}

func NewWorker(config Config, store *Store, rpc *RPCClient, facts *engine.Facts) (*Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if store == nil || rpc == nil || facts == nil {
		return nil, fmt.Errorf("store, RPC client and facts are required")
	}
	w := &Worker{config: config, store: store, rpc: rpc, facts: facts}
	if config.VoltrVaultID > 0 {
		route, err := LoadVoltrRoute()
		if err != nil {
			return nil, err
		}
		w.voltr = &route
	}
	return w, nil
}

func (w *Worker) SetRevalidator(revalidator *Revalidator) error {
	if revalidator == nil {
		return fmt.Errorf("revalidator is required")
	}
	if w.config.Mode != ModePublish {
		return fmt.Errorf("shadow mode cannot run durable revalidation")
	}
	w.revalidator = revalidator
	return nil
}

// SetShadowRevalidator installs the read-only shadow loop. Shadow mode only:
// it peeks, prepares, and logs; it never leases or commits.
func (w *Worker) SetShadowRevalidator(revalidator *Revalidator) error {
	if revalidator == nil {
		return fmt.Errorf("revalidator is required")
	}
	if w.config.Mode != ModeShadow {
		return fmt.Errorf("publish mode cannot run the shadow revalidator")
	}
	w.shadowRevalidator = revalidator
	return nil
}

func (w *Worker) SetMarketEvidence(source MarketEpochSource) error {
	if source == nil {
		return fmt.Errorf("market evidence source is required")
	}
	w.marketEvidence = source
	return nil
}

// runtimeCycle plans once; a completed cycle is fleet progress. The Voltr
// vault is planned on its own five-second probe, as in the Rust planner.
func (w *Worker) runtimeCycle(ctx context.Context) {
	if w.voltr != nil && w.config.Mode == ModePublish && !time.Now().Before(w.nextVoltr) {
		w.nextVoltr = time.Now().Add(5 * time.Second)
		if status, err := w.voltrCycle(ctx); err != nil {
			logEvent(map[string]any{"event": "backyard_voltr_planning_failed", "vaultId": w.config.VoltrVaultID, "errorCategory": "cycle"})
		} else {
			logEvent(map[string]any{"event": "backyard_voltr_planning", "vaultId": w.config.VoltrVaultID, "status": status})
		}
	}
	if err := w.planningCycle(ctx); err != nil {
		logEvent(map[string]any{"event": "kamino_fleet_planner_cycle_failed", "errorCategory": "cycle"})
		return
	}
	w.facts.Progress(engine.FamilyFleet)
}

// voltrCycle is run_backyard_voltr_planning_cycle: one confirmed observation
// against the durable market epoch yields zero or one manager opportunity.
// An observation that cannot be read coherently defers to the next probe.
func (w *Worker) voltrCycle(ctx context.Context) (string, error) {
	if w.marketEvidence == nil {
		return "", fmt.Errorf("complete durable market evidence is required")
	}
	epoch, err := w.marketEvidence.LoadImmutableMarketEpoch(ctx)
	if err != nil {
		return "", err
	}
	if err = epoch.Validate(); err != nil {
		return "", err
	}
	epochID, err := w.store.EnsureOptimizerEpoch(ctx, w.config.Cluster, epoch)
	if err != nil {
		return "", err
	}
	minSlot := int64(1)
	if epoch.MaximumMarketSlot != nil && *epoch.MaximumMarketSlot > 0 {
		minSlot = *epoch.MaximumMarketSlot
	}
	observation, err := ObserveVoltr(ctx, w.rpc, *w.voltr, minSlot)
	if err != nil {
		return "deferred_observation", nil
	}
	pending, lastOptimization, err := w.store.VoltrPlanningState(ctx, w.config.VoltrVaultID)
	if err != nil || pending {
		return "recovery_precedes_planning", err
	}
	opportunity, err := PlanVoltr(*w.voltr, observation, epoch, w.config.VoltrVaultID, lastOptimization, time.Now().UTC())
	if errors.Is(err, errVoltrMarketCoverage) {
		return "deferred_market_coverage", nil
	}
	if err != nil || opportunity == nil {
		return "noop", err
	}
	inserted, err := w.store.PublishVoltr(ctx, w.config.Cluster, epochID, *opportunity)
	if err != nil || !inserted {
		return "not_published", err
	}
	return "opportunity_upserted", nil
}

func (w *Worker) Run(ctx context.Context) error {
	var lanes sync.WaitGroup
	defer lanes.Wait()
	// Match the retained Rust route-revalidator service: sixteen independent
	// claim loops polling every 250ms by default. Planning remains a singleton
	// one-second loop and can never block recovery of already durable work.
	if w.revalidator != nil {
		for i := 0; i < w.config.RevalidationConcurrency; i++ {
			lanes.Add(1)
			go func(index int) {
				defer lanes.Done()
				w.runRevalidator(ctx, index, "kamino_fleet_revalidation_failed", w.revalidator.Cycle)
			}(i)
		}
	}
	if w.shadowRevalidator != nil {
		for i := 0; i < w.config.RevalidationConcurrency; i++ {
			lanes.Add(1)
			go func(index int) {
				defer lanes.Done()
				w.runRevalidator(ctx, index, "kamino_fleet_revalidation_shadow_failed", func(ctx context.Context, cluster string) (bool, error) {
					return w.shadowRevalidator.ShadowCycle(ctx, cluster, &w.shadowSeen)
				})
			}(i)
		}
	}
	poll := time.NewTicker(w.config.PollInterval)
	defer poll.Stop()
	w.runtimeCycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			w.runtimeCycle(ctx)
		}
	}
}

func (w *Worker) runRevalidator(ctx context.Context, index int, failureEvent string, cycle func(context.Context, string) (bool, error)) {
	ticker := time.NewTicker(w.config.RevalidationPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		processed, err := cycle(ctx, w.config.Cluster)
		if err != nil {
			logEvent(map[string]any{"event": failureEvent, "workerIndex": index, "errorCategory": "cycle_failed"})
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// cycle is retained as the deterministic single-step integration surface: it
// plans and then services one durable revalidation item even when no vault is
// currently plannable.
func (w *Worker) cycle(ctx context.Context) error {
	if err := w.planningCycle(ctx); err != nil {
		return err
	}
	if w.revalidator != nil {
		_, err := w.revalidator.Cycle(ctx, w.config.Cluster)
		return err
	}
	return nil
}

func (w *Worker) planningCycle(ctx context.Context) error {
	// Shadow never changes durable state. Keep Rust serving production while
	// observing Go-specific successful-cycle logs, not the shared heartbeat.
	if w.config.Mode == ModePublish {
		if err := w.store.RegisterFleetPlanningCluster(ctx, w.config.Cluster); err != nil {
			return err
		}
		if _, err := w.store.RecoverUnsignedExecutionAdmissions(ctx, w.config.Cluster, 1000); err != nil {
			return err
		}
		// Sweep before loading evidence: an RPC/Timescale outage must not leave
		// expired unstarted routes blocking vaults indefinitely.
		swept, err := w.store.SweepExpiredOpportunities(ctx, w.config.Cluster, 10_000)
		if err != nil {
			return err
		}
		if swept > 0 {
			logEvent(map[string]any{"event": "kamino_fleet_planner_expiry_sweep", "cluster": w.config.Cluster, "sweptCount": swept})
		}
	}
	if w.marketEvidence == nil {
		return fmt.Errorf("complete durable market evidence is required")
	}
	epoch, err := w.marketEvidence.LoadImmutableMarketEpoch(ctx)
	if err != nil {
		return err
	}
	if err = epoch.Validate(); err != nil {
		return err
	}
	addresses, err := epoch.RoutableReserveAddresses()
	if err != nil {
		return err
	}
	identities := make(map[string]ReserveIdentity, len(epoch.Reserves))
	for _, r := range epoch.Reserves {
		if r.Market == nil {
			return fmt.Errorf("catalog reserve %s has no market", r.Reserve)
		}
		identities[r.Reserve] = ReserveIdentity{Address: r.Reserve, Market: *r.Market, Mint: r.LiquidityMint}
	}
	minimumSlot, err := w.rpc.ConfirmedSlot(ctx)
	if err != nil {
		return fmt.Errorf("observe confirmed slot: %w", err)
	}
	if w.lastConfirmedSlot > minimumSlot {
		minimumSlot = w.lastConfirmedSlot
	}
	direct, err := w.observeConfirmedReserveCatalog(ctx, addresses, identities, minimumSlot)
	if err != nil {
		return fmt.Errorf("observe complete coherent reserve catalog: %w", err)
	}
	slot := direct.Slot
	observationDifferences := 0
	if err = epoch.VerifyDirectObservation(direct, addresses...); err != nil {
		if !tolerableObservationDifference(err) {
			return fmt.Errorf("durable market evidence not converged: %w", err)
		}
		// Rust plans from this verified immutable database epoch and never
		// re-reads reserve accounts over RPC, so it has no equivalent fence.
		// A hash difference here only means the reserve account changed after
		// Timescale captured it, which is the normal state of a busy reserve
		// (D6q6 differed in 35% of shadow cycles on 2026-09-16); it cannot say
		// which side is newer. Identity, slot, and expiry failures above stay
		// fatal in every mode. Route-level freshness is enforced later by
		// simulation against confirmed chain state before publication.
		observationDifferences = 1
		logEvent(map[string]any{"event": "kamino_fleet_planner_observation_difference", "mode": w.config.Mode, "errorCategory": "cycle_failed", "planningEvidence": "durable_verified_epoch"})
	}
	snapshot, err := marketSnapshotFromEpoch(epoch, addresses...)
	if err != nil {
		return err
	}
	snapshot.Cluster = w.config.Cluster
	// Each opportunity is fenced by the minimum complete source/target mint
	// lifetime. An epoch need not contain USDC when the migrated fleet only
	// uses another supported stablecoin.
	snapshot.ExpiresAt = epoch.ExpiresAt
	if w.config.Mode == ModePublish {
		snapshot.OptimizerEpochID, err = w.store.EnsureOptimizerEpoch(ctx, w.config.Cluster, epoch)
		if err == nil {
			err = w.store.RefreshCapacityEpoch(ctx, w.config.Cluster, epoch)
		}
	} else {
		snapshot.OptimizerEpochID, err = w.store.LookupOptimizerEpoch(ctx, w.config.Cluster, epoch.Fingerprint)
	}
	if err != nil {
		return fmt.Errorf("resolve optimizer/capacity epoch: %w", err)
	}
	vaults, err := w.store.LoadMigratedFleet(ctx, w.config.Cluster, epoch, FleetLoadOptions{DelegatedSigner: w.config.DelegatedSigner, EnableCrossMint: w.config.CrossMintEnabled, CrossMintMaxValueLossBPS: w.config.CrossMintMaxValueLossBPS, OptimizerEpochID: snapshot.OptimizerEpochID, IncludeIdleShadowSources: w.config.Mode == ModeShadow})
	if err != nil {
		return err
	}
	var idleSummary map[string]any
	var fleetPlan FleetPlan
	reserveSourceCount := len(vaults)
	if w.config.Mode == ModeShadow {
		reserveSources, summary := idleShadowChecks(snapshot, vaults)
		reserveSourceCount, idleSummary = len(reserveSources), summary
		fleetPlan, err = PlanFleetShadow(snapshot, vaults)
	} else {
		fleetPlan, err = PlanFleet(snapshot, vaults)
	}
	if err != nil {
		return err
	}
	positions := make(map[int64]VaultPosition, len(vaults))
	for _, v := range vaults {
		positions[v.Position.VaultID] = v.Position
	}
	published := 0
	for _, opportunity := range fleetPlan.Opportunities {
		result := PublishResult{Reason: "shadow"}
		if w.config.Mode == ModePublish {
			result, err = w.store.Publish(ctx, w.config.Cluster, epoch, positions[opportunity.Decision.VaultID], opportunity.Decision)
			if err != nil {
				return fmt.Errorf("publish fleet opportunity: %w", err)
			}
			if result.Inserted {
				published++
			}
		}
		logEvent(map[string]any{"event": "kamino_fleet_planner_opportunity", "mode": w.config.Mode, "vaultId": opportunity.Decision.VaultID, "sourceReserve": opportunity.Decision.SourceReserve, "targetReserve": opportunity.Decision.TargetReserve, "idempotencyKey": opportunity.IdempotencyKey, "publishReason": result.Reason, "opportunityId": result.OpportunityID})
	}
	// Advance durable health only after evidence loading, coherent RPC
	// verification, planning, and all requested publications have succeeded.
	if w.config.Mode == ModePublish {
		if err := w.store.HeartbeatFleetPlanningCluster(ctx, w.config.Cluster); err != nil {
			return err
		}
	}
	w.lastConfirmedSlot = slot
	if idleSummary != nil {
		idleSelected := 0
		for _, o := range fleetPlan.Opportunities {
			if o.Decision.RouteKind == "idle_vault_deposit" {
				idleSelected++
			}
		}
		idleSummary["candidateChecksOnly"] = false
		idleSummary["executableIdleEnabled"] = false
		idleSummary["jointAllocationChecked"] = true
		idleSummary["jointSelectedIdleCount"] = idleSelected
		idleSummary["jointSelectedReserveCount"] = len(fleetPlan.Opportunities) - idleSelected
		logEvent(idleSummary)
	}
	logEvent(map[string]any{"event": "kamino_fleet_planner_cycle", "mode": w.config.Mode, "cluster": w.config.Cluster, "slot": slot, "optimizerEpochFingerprint": epoch.Fingerprint, "catalogReserveCount": epoch.CatalogReserveCount, "routableReserveCount": len(addresses), "migratedVaultCount": reserveSourceCount, "selectedMoveCount": len(fleetPlan.Opportunities), "publishedCount": published, "observationDifferenceCount": observationDifferences, "rejectedVaultCount": len(fleetPlan.Rejections), "rejectionCounts": rejectionCounts(fleetPlan.Rejections)})
	return nil
}

func marketSnapshotFromEpoch(epoch ImmutableMarketEpoch, addresses ...string) (MarketSnapshot, error) {
	if err := epoch.Validate(); err != nil {
		return MarketSnapshot{}, err
	}
	result := MarketSnapshot{OptimizerEpochID: epoch.OptimizerEpochID, ExpiresAt: epoch.ExpiresAt, MintExpiresAt: make(map[string]time.Time), Hash: epoch.Fingerprint, ObservedAt: epoch.CapturedAt, Reserves: make(map[string]ReserveState)}
	for _, coverage := range epoch.MintCoverage {
		if coverage.Complete && coverage.ExpiresAt != nil {
			result.MintExpiresAt[coverage.Mint] = *coverage.ExpiresAt
		}
	}
	if epoch.MaximumMarketSlot != nil {
		result.Slot = *epoch.MaximumMarketSlot
	}
	for _, address := range addresses {
		reserve, ok := epoch.Reserve(address)
		if !ok || reserve.Market == nil {
			return MarketSnapshot{}, fmt.Errorf("required reserve %s is absent from immutable market epoch", address)
		}
		lifetime := reserve.EconomicExpiresAt.Sub(epoch.CapturedAt).Milliseconds()
		if lifetime < 0 {
			lifetime = 0
		}
		result.Reserves[address] = ReserveState{
			ReserveIdentity: ReserveIdentity{Address: reserve.Reserve, Market: *reserve.Market, Mint: reserve.LiquidityMint},
			Slot:            reserve.Slot, ObservedAt: reserve.ObservedAt, LastUpdateSlot: reserve.ReserveLastUpdateSlot, LastUpdateStale: reserve.ReserveLastUpdateStale,
			EconomicSlotLag: reserve.EconomicSlotLag, SupplyAPYBPS: reserve.SupplyAPYBPS,
			TotalSupplyUSDMicros: reserve.TotalSupplyUSDMicros, EconomicLifetimeMillis: lifetime, DataHash: reserve.AccountDataHash,
		}
		if reserve.ObservedAt.After(result.ObservedAt) {
			result.ObservedAt = reserve.ObservedAt
		}
	}
	return result, nil
}

// Log one count per reason, not a fleet-sized map of vault IDs every cycle.
// Keep the detailed per-vault results in FleetPlan for callers that need them.
func rejectionCounts(rejections map[int64]string) map[string]int {
	counts := make(map[string]int)
	for _, reason := range rejections {
		counts[reason]++
	}
	return counts
}

// tolerableObservationDifference is true only for a bare account-hash
// difference. Every other VerifyDirectObservation failure is fatal in every mode.
func tolerableObservationDifference(err error) bool {
	_, mismatch := err.(*DirectObservationHashMismatch)
	return mismatch
}

func logEvent(event map[string]any) {
	encoded, err := json.Marshal(event)
	if err != nil {
		log.Print("kamino_fleet_planner_log_encoding_failed")
		return
	}
	log.Print(string(encoded))
}
