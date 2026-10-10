package backyard

import (
	"context"
	"fmt"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// There is no write-capable observe method on this reader. The SQL connection
// itself also rejects writes; this deliberately does not fake an execution lease.
type shadowJournal struct{ db *Database }

func (r shadowJournal) PostMutationNAVRequired(ctx context.Context, key string) (bool, error) {
	var required bool
	err := r.db.pool.QueryRow(ctx, PostMutationNAVRequiredSQL, key).Scan(&required)
	return required, err
}
func (r shadowJournal) ReconciledBridgeJournal(ctx context.Context, key string) (ReconciledBridgeJournalState, error) {
	return r.db.readReconciledBridgeJournal(ctx, key)
}
func (r shadowJournal) RecordPositionSnapshot(context.Context, string, Observation) error {
	return fmt.Errorf("shadow_projection_writes_disabled")
}
func (r shadowJournal) LoadUnwindIntent(ctx context.Context, key string) (*UnwindIntent, error) {
	return r.db.LoadUnwindIntent(ctx, key)
}

func (r shadowJournal) SelectorEntryPaused(ctx context.Context, key string) (bool, error) {
	return r.db.SelectorEntryPaused(ctx, key)
}

// This observer enriches a separate snapshot without projecting NAV or taking
// an execution lease. Broader ownership failures stay confined to shadow output.
func observeSelectorShadow(ctx context.Context, database *Database, rpc *chain.Client, view *View, manifest RouteManifest, identity func(context.Context) (programIdentityObservation, error)) (Observation, error) {
	planning, err := database.readRoutePlanningStateOnManifest(ctx, manifest, productionRouteKey, false)
	if err != nil {
		return Observation{}, err
	}
	manifest = planning.observationManifest(manifest)
	manifest.selectorObservation = true
	observation, _, err := ObserveConfirmedRouteSnapshot(ctx, rpc, view, manifest)
	if err != nil {
		return Observation{}, fmt.Errorf("shadow confirmed observation unavailable: %w", err)
	}
	observation.planning = planning
	state := productionObserveState{routeKey: productionRouteKey, journal: shadowJournal{database}, identity: identity, manifest: manifest}
	if err = state.enrich(ctx, &observation); err != nil {
		return Observation{}, fmt.Errorf("shadow journal or identity unavailable: %w", err)
	}
	var pending string
	err = database.pool.QueryRow(ctx, `SELECT COALESCE((SELECT status FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`) ORDER BY created_at,operation_id LIMIT 1),'')`, productionRouteKey).Scan(&pending)
	if err != nil {
		return Observation{}, fmt.Errorf("shadow pending transaction unavailable")
	}
	observation.Snapshot.Nonterminal = OperationStatus(pending)

	if _, latched, err := database.ManualRecoveryLatch(ctx, productionRouteKey); err != nil {
		return Observation{}, fmt.Errorf("shadow recovery hold unavailable")
	} else if latched {
		observation.Snapshot.ManualReason = "durable_manual_recovery"
	}
	return observation, nil
}

func runSelectorSamples(ctx context.Context, interval time.Duration, sample func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		sampleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		sample(sampleCtx)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r shadowJournal) LoadSelectorEntry(ctx context.Context, key string) (*SelectorEntry, error) {
	return r.db.LoadSelectorEntry(ctx, key)
}
