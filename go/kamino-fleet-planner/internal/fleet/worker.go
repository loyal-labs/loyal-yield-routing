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

func (w *Worker) SetMarketEvidence(source MarketEpochSource) error {
	if source == nil {
		return fmt.Errorf("market evidence source is required")
	}
	w.marketEvidence = source
	return nil
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
	states[0], err = DecodeKaminoSourceReserve(accounts[0], w.config.Source, slot, w.config.SlotDuration)
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
	var epoch ImmutableMarketEpoch
	if decision.Eligible && w.marketEvidence == nil && w.config.Mode == ModePublish {
		return fmt.Errorf("publish mode requires durable monitor-owned market evidence")
	}
	if decision.Eligible && w.marketEvidence != nil {
		epoch, err = w.marketEvidence.LoadImmutableMarketEpoch(ctx)
		if err != nil {
			return err
		}
		if err := epoch.VerifyDirectObservation(snapshot, w.config.Source.Address, w.config.Target.Address); err != nil {
			result.Reason = "durable_market_evidence_not_converged"
			decision.Eligible = false
			decision.Reason = result.Reason
		} else {
			epochSnapshot, snapshotErr := marketSnapshotFromEpoch(epoch, w.config.Source.Address, w.config.Target.Address)
			if snapshotErr != nil {
				return snapshotErr
			}
			decision = Plan(epochSnapshot, position, w.config.Source.Address, w.config.Target.Address)
			if decision.Eligible && w.config.Mode == ModePublish {
				result, err = w.store.Publish(ctx, w.config.Cluster, epoch, position, decision)
				if err != nil {
					return fmt.Errorf("publish durable opportunity: %w", err)
				}
			}
		}
	}
	w.lastConfirmedSlot = snapshot.Slot
	logEvent(map[string]any{
		"event": "kamino_fleet_planner_cycle", "mode": w.config.Mode,
		"slot": snapshot.Slot, "snapshotHash": snapshot.Hash,
		"optimizerEpochFingerprint": epoch.Fingerprint, "catalogReserveCount": epoch.CatalogReserveCount,
		"eligible": decision.Eligible, "reason": decision.Reason,
		"publishReason": result.Reason, "opportunityId": result.OpportunityID,
		"observedSourceApyBps": snapshot.Reserves[w.config.Source.Address].SupplyAPYBPS,
		"observedTargetApyBps": snapshot.Reserves[w.config.Target.Address].SupplyAPYBPS,
		"sourceApyBps":         decision.SourceAPYBPS, "targetApyBps": decision.TargetAPYBPS,
		"edgeBps": decision.EdgeBPS,
	})
	return nil
}

func marketSnapshotFromEpoch(epoch ImmutableMarketEpoch, addresses ...string) (MarketSnapshot, error) {
	if err := epoch.Validate(); err != nil {
		return MarketSnapshot{}, err
	}
	result := MarketSnapshot{Hash: epoch.Fingerprint, ObservedAt: epoch.CapturedAt, Reserves: make(map[string]ReserveState)}
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
			Slot:            reserve.Slot, LastUpdateSlot: reserve.ReserveLastUpdateSlot, LastUpdateStale: reserve.ReserveLastUpdateStale,
			EconomicSlotLag: reserve.EconomicSlotLag, SupplyAPYBPS: reserve.SupplyAPYBPS,
			TotalSupplyUSDMicros: reserve.TotalSupplyUSDMicros, EconomicLifetimeMillis: lifetime, DataHash: reserve.AccountDataHash,
		}
		if reserve.ObservedAt.After(result.ObservedAt) {
			result.ObservedAt = reserve.ObservedAt
		}
	}
	return result, nil
}

func logEvent(event map[string]any) {
	encoded, err := json.Marshal(event)
	if err != nil {
		log.Printf("kamino_fleet_planner_log_error=%q", err)
		return
	}
	log.Print(string(encoded))
}
