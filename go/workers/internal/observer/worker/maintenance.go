package worker

import (
	"context"
	"errors"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/kamino"
)

func (r *Runtime) NewMaintenance(ctx context.Context) (*observer.Maintenance, error) {
	if r.cfg.Cluster != "mainnet-beta" {
		return nil, nil
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hash, err := r.rpc.GenesisHash(startup)
	if err != nil {
		return nil, err
	}
	if hash != mainnetGenesis {
		return nil, errors.New("product read model RPC is not Solana mainnet")
	}
	onError := func() { r.facts.Failed(engine.FamilyObserver, "read_models") }
	maintenance, err := observer.NewMaintenance(r.neon, r.timescale, observer.MaintenanceConfig{Cluster: r.cfg.Cluster, MediumMarkets: kamino.PolicyMarkets("medium"), PriceRPC: r.rpc, Logger: r.logger, ValidateNamespace: r.validateMaintenanceNamespace, OnError: onError})
	return maintenance, err
}

func (r *Runtime) validateMaintenanceNamespace(ctx context.Context) error {
	return validateWatchNamespace(ctx, r.cfg.Cluster, r.rpc)
}
