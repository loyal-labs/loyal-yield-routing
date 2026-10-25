package backyard

import (
	"context"
	"fmt"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

type shadowJournal struct{ db *Database }

func (r shadowJournal) PostMutationNAVRequired(ctx context.Context, key string) (bool, error) {
	var required bool
	err := r.db.pool.QueryRow(ctx, PostMutationNAVRequiredSQL, key).Scan(&required)
	return required, err
}
func (r shadowJournal) ReconciledBridgeJournal(ctx context.Context, key string) (ReconciledBridgeJournalState, error) {
	return r.db.readReconciledBridgeJournal(ctx, key)
}

func observeSelectorShadow(ctx context.Context, database *Database, rpc *chain.Client, view *View, manifest RouteManifest, vault *vaultState, identity func(context.Context) (programIdentityObservation, error)) (Observation, error) {
	v := manifest.vault
	planning, err := database.readRoutePlanningState(ctx, v.RouteKey)
	if err != nil {
		return Observation{}, err
	}
	observation, _, err := observeQuotingRouteSnapshot(ctx, rpc, view, manifest, vault, planning.landedSlot, planning.inputs, database.exits[v.RouteKey])
	if err != nil {
		return Observation{}, fmt.Errorf("shadow confirmed observation unavailable: %w", err)
	}
	observation.planning = planning
	if err = enrichObservation(ctx, shadowJournal{database}, v.RouteKey, identity, &observation); err != nil {
		return Observation{}, fmt.Errorf("shadow journal or identity unavailable: %w", err)
	}
	var pending string
	err = database.pool.QueryRow(ctx, `SELECT COALESCE((SELECT status FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`) ORDER BY created_at,operation_id LIMIT 1),'')`, v.RouteKey).Scan(&pending)
	if err != nil {
		return Observation{}, fmt.Errorf("shadow pending transaction unavailable")
	}
	observation.Snapshot.Nonterminal = OperationStatus(pending)

	if _, latched, err := database.ManualRecoveryLatch(ctx, v.RouteKey); err != nil {
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
