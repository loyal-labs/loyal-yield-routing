package worker

import (
	"context"
	"errors"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer"
)

// These eight market identities are the retained loyal-actions Medium preset:
// actions.rs:680-715 and ids.rs:81-89. The runtime owns/joins the maintenance
// lane and supplies its existing pools; no extra database pool or signer exists.
var mediumMainnetMarkets = []string{
	"7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF",
	"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
	"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
	"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
	"BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF",
	"DxXdAyU3kCjnyggvHmY5nAwg5cRbbmdyX3npfDMjjMek",
	"GMqmFygF5iSm5nkckYU6tieggFcR42SyjkkhK5rswFRs",
	"CF32kn7AY8X1bW7ZkGcHc4X9ZWTxqKGCJk6QwrQkDcdw",
}

// NewMaintenance returns the read-model lane, or nil when read models are not
// enabled: the Apps hourly crons own those tables until the Phase 2 handover
// (config.Config.ReadModelsEnabled), so a disabled observer opens no price
// RPC, checks no read-model schema and reports no read_models failures.
func (r *Runtime) NewMaintenance(ctx context.Context) (*observer.Maintenance, error) {
	if r != nil && !r.cfg.ReadModelsEnabled {
		return nil, nil
	}
	if r == nil || r.cfg.Cluster != "mainnet-beta" {
		return nil, errors.New("fixed product read models require mainnet-beta namespace")
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
	maintenance, err := observer.NewMaintenance(r.neon, r.timescale, observer.MaintenanceConfig{Cluster: r.cfg.Cluster, MediumMarkets: mediumMainnetMarkets, PriceRPC: r.rpc, Logger: r.logger, ValidateNamespace: r.validateMaintenanceNamespace, OnError: onError})
	return maintenance, err
}

// validateMaintenanceNamespace checks the actual RPC endpoint before a pass.
// It deliberately does not classify custody rows: the Apps recorders these
// passes replace (earn-fleet-allocation, reserve-share-price and forecast
// crons) select every active vault_index=1 managed vault with no cluster test,
// so legacy route_policies.cluster='unknown' rows and positions without a
// managed vault are part of the published product universe, not a reason to
// stop publishing it.
func (r *Runtime) validateMaintenanceNamespace(ctx context.Context) error {
	return validateWatchNamespace(ctx, r.cfg.Cluster, r.rpc)
}
