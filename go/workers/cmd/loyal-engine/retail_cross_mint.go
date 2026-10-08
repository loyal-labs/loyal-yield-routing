package main

import (
	"context"
	"errors"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleetexec"
)

func composeRetailCrossMint(ctx context.Context, cfg retailConfig, owner string, store *fleetexec.Store, revalidator *fleet.Revalidator, adapter *fleetexec.RPCAdapter, market *fleet.MarketEvidenceStore, facts *engine.Facts) (*fleetexec.CrossMintRuntime, error) {
	if market == nil {
		return nil, errors.New("cross-mint fallback requires the actual planner market evidence")
	}
	capabilities, err := fleetexec.NewRevalidatorCrossMint(revalidator)
	if err != nil {
		return nil, err
	}
	controller, err := fleetexec.NewCrossMintController(store, capabilities, fleetexec.DelegateSigner{FeePayer: cfg.delegate}, adapter, "mainnet-beta", owner, 30*time.Second, cfg.crossMintEnabled)
	if err != nil {
		return nil, err
	}
	controller.SetMarketEpochSource(market)
	runtime, err := fleetexec.NewCrossMintRuntime(ctx, fleetexec.Config{Cluster: "mainnet-beta", Owner: owner, LeaseTTL: 30 * time.Second, BatchSize: 20, TickInterval: 750 * time.Millisecond, SlotDuration: cfg.slotDuration, Facts: facts}, store, controller, adapter, capabilities)
	if err != nil {
		return nil, err
	}
	runtime.SetActivationSource(capabilities)
	return runtime, nil
}
