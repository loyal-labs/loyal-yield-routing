package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
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
	lastConfirmedSlot int64
}

func NewWorker(config Config, store *Store, rpc *RPCClient) (*Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if store == nil || rpc == nil {
		return nil, fmt.Errorf("store and RPC client are required")
	}
	return &Worker{config: config, store: store, rpc: rpc}, nil
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

func (w *Worker) SetMarketEvidence(source MarketEpochSource) error {
	if source == nil {
		return fmt.Errorf("market evidence source is required")
	}
	w.marketEvidence = source
	return nil
}

func (w *Worker) Run(ctx context.Context) error {
	// Match the retained Rust route-revalidator service: sixteen independent
	// claim loops polling every 250ms by default. Planning remains a singleton
	// one-second loop and can never block recovery of already durable work.
	if w.revalidator != nil {
		for i := 0; i < w.config.RevalidationConcurrency; i++ {
			go w.runRevalidator(ctx, i)
		}
	}
	poll := time.NewTicker(w.config.PollInterval)
	defer poll.Stop()
	if err := w.planningCycle(ctx); err != nil {
		logEvent(map[string]any{"event": "kamino_fleet_planner_cycle_failed", "error": err.Error()})
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			if err := w.planningCycle(ctx); err != nil {
				logEvent(map[string]any{"event": "kamino_fleet_planner_cycle_failed", "error": err.Error()})
			}
		}
	}
}

func (w *Worker) runRevalidator(ctx context.Context, index int) {
	ticker := time.NewTicker(w.config.RevalidationPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		processed, err := w.revalidator.Cycle(ctx, w.config.Cluster)
		if err != nil {
			logEvent(map[string]any{"event": "kamino_fleet_revalidation_failed", "workerIndex": index, "error": err.Error()})
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
	slot, accounts, err := w.rpc.ConfirmedAccounts(ctx, addresses, minimumSlot)
	if err != nil {
		return fmt.Errorf("observe complete coherent reserve catalog: %w", err)
	}
	direct := MarketSnapshot{Slot: slot, ObservedAt: time.Now().UTC(), Reserves: make(map[string]ReserveState, len(accounts))}
	for i, account := range accounts {
		state, e := DecodeKaminoSourceReserve(account, identities[addresses[i]], slot, w.config.SlotDuration)
		if e != nil {
			return e
		}
		direct.Reserves[addresses[i]] = state
	}
	if err = epoch.VerifyDirectObservation(direct, addresses...); err != nil {
		if !shadowObservationDifference(w.config.Mode, err) {
			return fmt.Errorf("durable market evidence not converged: %w", err)
		}
		// Rust plans from this verified immutable database epoch, not the
		// subsequent RPC read. Shadow can compare that same epoch while logging
		// changing account bytes; publish mode retains the strict equality fence.
		logEvent(map[string]any{"event": "kamino_fleet_planner_observation_difference", "mode": w.config.Mode, "error": err.Error(), "planningEvidence": "durable_verified_epoch"})
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
	vaults, err := w.store.LoadMigratedFleet(ctx, w.config.Cluster, epoch, FleetLoadOptions{DelegatedSigner: w.config.DelegatedSigner, EnableCrossMint: w.config.CrossMintEnabled, CrossMintMaxValueLossBPS: w.config.CrossMintMaxValueLossBPS, OptimizerEpochID: snapshot.OptimizerEpochID})
	if err != nil {
		return err
	}
	fleetPlan, err := PlanFleet(snapshot, vaults)
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
	logEvent(map[string]any{"event": "kamino_fleet_planner_cycle", "mode": w.config.Mode, "cluster": w.config.Cluster, "slot": slot, "optimizerEpochFingerprint": epoch.Fingerprint, "catalogReserveCount": epoch.CatalogReserveCount, "routableReserveCount": len(addresses), "migratedVaultCount": len(vaults), "selectedMoveCount": len(fleetPlan.Opportunities), "publishedCount": published, "rejections": fleetPlan.Rejections})
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

func shadowObservationDifference(mode Mode, err error) bool {
	_, mismatch := err.(*DirectObservationHashMismatch)
	return mode == ModeShadow && mismatch
}

func logEvent(event map[string]any) {
	encoded, err := json.Marshal(event)
	if err != nil {
		log.Printf("kamino_fleet_planner_log_error=%q", err)
		return
	}
	log.Print(string(encoded))
}
