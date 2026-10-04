package worker

import (
	"context"
	"errors"
	"time"

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

func (r *Runtime) NewMaintenance(ctx context.Context) (*observer.Maintenance, error) {
	if r == nil || r.cfg.Cluster != "mainnet-beta" {
		return nil, errors.New("fixed product read models require mainnet-beta namespace")
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hash, err := r.rpc.GenesisHash(startup)
	if err != nil {
		return nil, err
	}
	if hash != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return nil, errors.New("product read model RPC is not Solana mainnet")
	}
	// Legacy allocation/model tables have no cluster column. Reject mixed or
	// unknown active policy custody instead of quietly publishing partial data
	// into the shared mainnet product projections.
	validateNamespace := func(ctx context.Context) error {
		if err := validateWatchNamespace(ctx, r.cfg.Cluster, r.rpc); err != nil {
			return err
		}
		var scoped bool
		if err := r.neon.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM loyal_yield.route_policies WHERE active AND cluster IS DISTINCT FROM 'mainnet-beta')
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.user_yield_positions p WHERE p.status='active' AND NOT EXISTS(
 SELECT 1 FROM loyal_yield.managed_vaults v JOIN loyal_yield.route_policies policy ON policy.id=v.active_policy_id
 WHERE v.settings=p.settings AND v.vault_index=p.vault_index AND v.vault_pubkey=p.vault_pubkey
 AND policy.cluster='mainnet-beta'))`).Scan(&scoped); err != nil {
			return errors.New("product read model custody namespace query failed")
		}
		if !scoped {
			return errors.New("product read model namespace contains unknown or foreign active custody")
		}
		return nil
	}
	// Mutable custody can become classifiable through the observer itself.
	// Keep product readiness closed and recheck before every pass instead of
	// stopping capture before it can repair that transient state.
	rpc, err := observer.NewMaintenancePriceRPC(r.cfg.SolanaRPCURL, 30*time.Second)
	if err != nil {
		return nil, err
	}
	r.health.SetDomainReady("read_models", false)
	maintenance, err := observer.NewMaintenance(r.neon, r.timescale, observer.MaintenanceConfig{Cluster: r.cfg.Cluster, MediumMarkets: mediumMainnetMarkets, PriceRPC: rpc, Logger: r.logger, ValidateNamespace: validateNamespace, OnHealth: func(ready bool) { r.health.SetDomainReady("read_models", ready) }})
	if err != nil {
		return nil, err
	}
	if err := maintenance.RequireSchema(startup); err != nil {
		return nil, err
	}
	return maintenance, nil
}
