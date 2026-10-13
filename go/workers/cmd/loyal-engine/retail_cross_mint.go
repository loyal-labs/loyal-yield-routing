package main

import (
	"context"
	"errors"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleetexec"
)

func composeRetailCrossMint(ctx context.Context, cfg retailConfig, owner string, store *fleetexec.Store, revalidator *fleet.Revalidator, client *chain.Client, reserves fleet.ReserveSource, facts *engine.Facts) (*fleetexec.CrossMintRuntime, error) {
	if reserves == nil {
		return nil, errors.New("cross-mint fallback requires the planner's reserve source")
	}
	capabilities, err := fleetexec.NewRevalidatorCrossMint(revalidator)
	if err != nil {
		return nil, err
	}
	controller, err := fleetexec.NewCrossMintController(store, capabilities, fleetexec.DelegateSigner{FeePayer: cfg.delegate}, client, "mainnet-beta", owner, 30*time.Second, cfg.crossMintEnabled)
	if err != nil {
		return nil, err
	}
	controller.SetReserveSource(reserves)
	runtime, err := fleetexec.NewCrossMintRuntime(ctx, fleetexec.Config{Cluster: "mainnet-beta", Owner: owner, LeaseTTL: 30 * time.Second, BatchSize: 20, TickInterval: 750 * time.Millisecond, Facts: facts}, store, controller, client, capabilities)
	if err != nil {
		return nil, err
	}
	runtime.SetActivationSource(capabilities)
	return runtime, nil
}
