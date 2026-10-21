package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

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

func (w *Worker) runtimeCycle(ctx context.Context) {
	if err := w.planningCycle(ctx); err != nil {
		logEvent(map[string]any{"event": "kamino_fleet_planner_cycle_failed", "errorCategory": "cycle", "error": engine.ErrorText(err)})
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
	snapshot, err := ReadSnapshot(ctx, w.rpc, SnapshotRequest{Catalog: catalog, Vaults: SnapshotVaults(registry), SlotRate: MeasuredSlotRate(slotsPerSecond)})
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
	for address := range snapshot.Reserves {
		w.facts.FleetMovableCapital(address, float64(fleetPlan.Movable[address])/1e6)
	}
	if report := capacityReport(market, vaults, inflow, fleetPlan); w.capacity.due(time.Now(), report) {
		logEvent(map[string]any{"event": "kamino_fleet_capacity", "cluster": w.config.Cluster, "slot": snapshot.Slot, "reserves": report})
	}
	logEvent(map[string]any{"event": "kamino_fleet_planner_cycle", "cluster": w.config.Cluster, "slot": snapshot.Slot, "snapshotKey": record.Key, "catalogReserveCount": len(catalog), "routableReserveCount": len(snapshot.Reserves), "unroutableReserves": snapshot.Unroutable, "registeredVaultCount": len(registry), "holdingCount": len(vaults), "selectedMoveCount": len(fleetPlan.Opportunities), "publishedCount": summary.Published, "keptCount": summary.Kept, "supersededCount": summary.Superseded, "rejectedVaultCount": len(fleetPlan.Rejections), "rejectionCounts": rejectionCounts(fleetPlan.Rejections)})
	return nil
}

func rejectionCounts(rejections map[int64]string) map[string]int {
	counts := make(map[string]int)
	for _, reason := range rejections {
		counts[reason]++
	}
	return counts
}

func logEvent(event map[string]any) {
	encoded, err := json.Marshal(event)
	if err != nil {
		log.Print("kamino_fleet_planner_log_encoding_failed")
		return
	}
	log.Print(string(encoded))
}
