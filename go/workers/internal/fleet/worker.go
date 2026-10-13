package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// ReserveCatalog lists the routable reserves.
type ReserveCatalog interface {
	LoadCatalog(context.Context) ([]ReserveIdentity, error)
}

type Worker struct {
	config   Config
	store    *Store
	rpc      *chain.Client
	catalog  ReserveCatalog
	facts    *engine.Facts
	capacity capacityLog
}

func NewWorker(config Config, store *Store, client *chain.Client, catalog ReserveCatalog, facts *engine.Facts) (*Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if store == nil || client == nil || catalog == nil || facts == nil {
		return nil, fmt.Errorf("store, RPC client, reserve catalog and facts are required")
	}
	return &Worker{config: config, store: store, rpc: client, catalog: catalog, facts: facts}, nil
}

// runtimeCycle plans once; a completed cycle is fleet progress and the
// planner lane's success. The executor also marks fleet progress every tick,
// so only the lane fact shows a planner failing every cycle.
func (w *Worker) runtimeCycle(ctx context.Context) {
	if err := w.planningCycle(ctx); err != nil {
		logEvent(map[string]any{"event": "kamino_fleet_planner_cycle_failed", "errorCategory": "cycle", "error": LogErrorText(err)})
		return
	}
	w.facts.Progress(engine.FamilyFleet)
	w.facts.LaneSucceeded(engine.FamilyFleet, "planner")
}

func (w *Worker) Run(ctx context.Context) error {
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

// planningCycle is one tick: read the registry, read the chain once, plan,
// and make the queue the plan.
func (w *Worker) planningCycle(ctx context.Context) error {
	if err := w.store.RegisterFleetPlanningCluster(ctx, w.config.Cluster); err != nil {
		return err
	}
	catalog, err := w.catalog.LoadCatalog(ctx)
	if err != nil {
		return err
	}
	options := FleetLoadOptions{DelegatedSigner: w.config.DelegatedSigner, EnableCrossMint: w.config.CrossMintEnabled, CrossMintMaxValueLossBPS: w.config.CrossMintMaxValueLossBPS}
	registry, err := w.store.LoadRegistry(ctx, w.config.Cluster, options)
	if err != nil {
		return err
	}
	slotsPerSecond, err := w.rpc.SlotsPerSecond(ctx)
	if err != nil {
		return fmt.Errorf("measured slot rate: %w", err)
	}
	snapshot, err := ReadSnapshot(ctx, w.rpc, SnapshotRequest{Catalog: catalog, Vaults: SnapshotVaults(registry), SlotsPerSecond: slotsPerSecond})
	if err != nil {
		return fmt.Errorf("read chain snapshot: %w", err)
	}
	if len(snapshot.Reserves) < 2 {
		return fmt.Errorf("snapshot has %d routable reserves; unroutable: %v", len(snapshot.Reserves), snapshot.Unroutable)
	}
	inflow, outflow, err := InflightFlows(ctx, w.store.pool, w.config.Cluster, snapshot.Slot, 0)
	if err != nil {
		return err
	}
	vaults, err := PlanningVaults(snapshot, catalog, registry, inflow, outflow, options)
	if err != nil {
		return err
	}
	record, err := recordOf(snapshot)
	if err != nil {
		return err
	}
	market := snapshot.Market(record.Key)
	fleetPlan, err := PlanFleet(market, vaults)
	if err != nil {
		return err
	}
	summary, err := w.store.PublishPlan(ctx, w.config.Cluster, snapshot, fleetPlan)
	if err != nil {
		return err
	}
	if err := w.store.HeartbeatFleetPlanningCluster(ctx, w.config.Cluster); err != nil {
		return err
	}
	// Every routable reserve is set each cycle, zero included, so capital
	// that stops being movable reads 0 rather than its last value.
	for address := range snapshot.Reserves {
		w.facts.FleetMovableCapital(address, float64(fleetPlan.Movable[address])/1e6)
	}
	if report := capacityReport(market, vaults, inflow, fleetPlan); w.capacity.due(time.Now(), report) {
		logEvent(map[string]any{"event": "kamino_fleet_capacity", "cluster": w.config.Cluster, "slot": snapshot.Slot, "reserves": report})
	}
	logEvent(map[string]any{"event": "kamino_fleet_planner_cycle", "cluster": w.config.Cluster, "slot": snapshot.Slot, "snapshotKey": record.Key, "catalogReserveCount": len(catalog), "routableReserveCount": len(snapshot.Reserves), "unroutableReserves": snapshot.Unroutable, "registeredVaultCount": len(registry), "holdingCount": len(vaults), "selectedMoveCount": len(fleetPlan.Opportunities), "publishedCount": summary.Published, "keptCount": summary.Kept, "supersededCount": summary.Superseded, "rejectedVaultCount": len(fleetPlan.Rejections), "rejectionCounts": rejectionCounts(fleetPlan.Rejections)})
	return nil
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

var (
	logURLPattern    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	logSecretPattern = regexp.MustCompile(`(?i)(password|api[-_]?key|token|secret)=\S+`)
)

// LogErrorText is an error's cause for the fleet family's logs: engine
// ErrorText reduces a *url.Error to its operation and cause, and any other
// URL- or credential-shaped text an error carries is replaced, because RPC
// URLs carry their API key. The text is bounded.
func LogErrorText(err error) string {
	if err == nil {
		return ""
	}
	text := logURLPattern.ReplaceAllString(engine.ErrorText(err), "<url>")
	text = logSecretPattern.ReplaceAllString(text, "$1=<redacted>")
	if len(text) > 500 {
		text = text[:500]
	}
	return text
}

func logEvent(event map[string]any) {
	encoded, err := json.Marshal(event)
	if err != nil {
		log.Print("kamino_fleet_planner_log_encoding_failed")
		return
	}
	log.Print(string(encoded))
}
