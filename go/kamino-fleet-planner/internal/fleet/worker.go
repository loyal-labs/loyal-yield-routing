package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

type Worker struct {
	config            Config
	store             *Store
	rpc               *RPCClient
	owner             *SnapshotOwner
	lastConfirmedSlot int64
}

func NewWorker(config Config, store *Store, rpc *RPCClient) (*Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if store == nil || rpc == nil {
		return nil, fmt.Errorf("store and RPC client are required")
	}
	owner, err := NewSnapshotOwner(config.Source, config.Target)
	if err != nil {
		return nil, err
	}
	return &Worker{config: config, store: store, rpc: rpc, owner: owner}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	poll := time.NewTicker(w.config.PollInterval)
	defer poll.Stop()
	if err := w.cycle(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			if err := w.cycle(ctx); err != nil {
				return err
			}
		}
	}
}

func (w *Worker) cycle(ctx context.Context) error {
	minimumSlot, err := w.rpc.ConfirmedSlot(ctx)
	if err != nil {
		return fmt.Errorf("observe confirmed slot: %w", err)
	}
	if w.lastConfirmedSlot > minimumSlot {
		minimumSlot = w.lastConfirmedSlot
	}
	slot, accounts, err := w.rpc.ConfirmedAccounts(ctx, []string{w.config.Source.Address, w.config.Target.Address}, minimumSlot)
	if err != nil {
		return fmt.Errorf("observe coherent reserves: %w", err)
	}
	states := make([]ReserveState, 2)
	states[0], err = DecodeKaminoReserve(accounts[0], w.config.Source, slot, w.config.SlotDuration)
	if err != nil {
		return err
	}
	states[1], err = DecodeKaminoReserve(accounts[1], w.config.Target, slot, w.config.SlotDuration)
	if err != nil {
		return err
	}
	snapshot, _, err := w.owner.Apply(slot, time.Now().UTC(), states)
	if err != nil {
		return err
	}
	position, err := w.store.LoadVaultPosition(ctx, w.config.Cluster, w.config.VaultID, w.config.Source, w.config.Target)
	if err != nil {
		return err
	}
	decision := Plan(snapshot, position, w.config.Source.Address, w.config.Target.Address)
	result := PublishResult{Reason: "shadow"}
	if decision.Eligible && w.config.Mode == ModePublish {
		result, err = w.store.Publish(ctx, w.config.Cluster, snapshot, position, decision)
		if err != nil {
			return fmt.Errorf("publish durable opportunity: %w", err)
		}
	}
	w.lastConfirmedSlot = snapshot.Slot
	logEvent(map[string]any{"event": "kamino_fleet_planner_cycle", "mode": w.config.Mode, "slot": snapshot.Slot, "snapshotHash": snapshot.Hash, "eligible": decision.Eligible, "reason": decision.Reason, "publishReason": result.Reason, "opportunityId": result.OpportunityID, "sourceApyBps": decision.SourceAPYBPS, "targetApyBps": decision.TargetAPYBPS, "edgeBps": decision.EdgeBPS})
	return nil
}

func logEvent(event map[string]any) {
	encoded, err := json.Marshal(event)
	if err != nil {
		log.Printf("kamino_fleet_planner_log_error=%q", err)
		return
	}
	log.Print(string(encoded))
}
