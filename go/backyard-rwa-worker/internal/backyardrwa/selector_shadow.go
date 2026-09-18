package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"os"
	"time"
)

// Shadow uses the production readers and pure planner, but has no signer,
// submission path, lease acquisition, or database writes. Gross forecasts remain
// visibly unexecutable until pair capacity, cost, and recreation are admitted.
func RunSelectorShadow(ctx context.Context, out io.Writer) error {
	config := RuntimeConfigFromEnvironment()
	if config.RPCURL == "" || config.DatabaseURL == "" {
		return fmt.Errorf("SOLANA_RPC_URL and NEON_DATABASE_URL are required for shadow observation")
	}
	cfg, err := pgxpool.ParseConfig(config.DatabaseURL)
	if err != nil {
		return fmt.Errorf("shadow database configuration invalid")
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	database := &Database{pool: pool}
	if err != nil {
		return fmt.Errorf("shadow journal unavailable")
	}
	defer database.Close()
	rpc, err := NewRPCClient(config.RPCURL)
	if err != nil {
		return err
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	manifest.selectorObservation = true
	feed, err := NewEconomicFeed(ctx, os.Getenv("TIMESCALEDB_URL"))
	if err != nil {
		return err
	}
	defer feed.Close()
	feedErr := feed.Refresh(ctx)
	observation, err := observeSelectorShadow(ctx, database, rpc, manifest, newProgramIdentityWatcher(rpc).observe)
	if err != nil {
		return err
	}
	markets, failure := feed.Snapshot()
	result := SelectOpportunity(SelectorInput{Now: time.Now().UTC(), Snapshot: observation.Snapshot, Markets: markets, Policy: DefaultSelectorPolicy()}, SelectorState{})
	next := Decide(observation.Snapshot)
	if next.Action == HoldManualRecovery && observation.Snapshot.ManualReason != "" {
		next.Reason = observation.Snapshot.ManualReason
	}
	report := struct {
		Mode                string          `json:"mode"`
		Snapshot            Snapshot        `json:"snapshot"`
		ObservedAt          time.Time       `json:"observedAt"`
		Slot                int64           `json:"slot"`
		ObservationID       string          `json:"observationId"`
		FeedFailure         string          `json:"feedFailure,omitempty"`
		Markets             []LaneEconomics `json:"markets"`
		NextLifecycleAction Decision        `json:"nextLifecycleAction"`
		Selection           SelectorResult  `json:"selection"`
		ActivationBlockers  []string        `json:"activationBlockers"`
	}{"read_only_shadow", observation.Snapshot, observation.ObservedAt, observation.Snapshot.Slot, observation.Snapshot.ObservationID, failure, markets, next, result, []string{"basic_usdc_execution_admission_incomplete", "automatic_obligation_recreation_not_admitted", "complete_move_cost_and_pair_capacity_not_admitted", "current_image_round_trip_canary_required"}}
	if feedErr != nil {
		report.FeedFailure = feedErr.Error()
	}
	return json.NewEncoder(out).Encode(report)
}

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

func (r shadowJournal) PilotRuntimeEnabled(ctx context.Context, key string) (bool, error) {
	return r.db.PilotRuntimeEnabled(ctx, key)
}

// The shadow observation carries the same validated activation baseline into
// its snapshot through one delegated read, so M8's baseline explanation is
// identical in shadow and production.
func (r shadowJournal) PilotRuntimeState(ctx context.Context, key string) (bool, *pilotActivationBaseline, error) {
	return r.db.PilotRuntimeState(ctx, key)
}

// This observer enriches a separate snapshot without projecting NAV or taking
// an execution lease. Broader ownership failures stay confined to shadow output.
func observeSelectorShadow(ctx context.Context, database *Database, rpc *RPCClient, manifest RouteManifest, identity func(context.Context) (programIdentityObservation, error)) (Observation, error) {
	planning, err := database.readRoutePlanningState(ctx, productionRouteKey, false)
	if err != nil {
		return Observation{}, err
	}
	manifest = planning.observationManifest(manifest)
	manifest.selectorObservation = true
	observation, err := ObserveConfirmedRouteSnapshot(ctx, rpc, manifest)
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
