package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleetexec"
	"github.com/mr-tron/base58"
)

// This is crates/loyal-solana-env/src/signer.rs's production ALT authority.
// Neither the route delegate nor another environment key grants ALT authority.
const retailLookupAuthority = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"

type retailLookupConfig struct {
	active  bool
	manager ed25519.PrivateKey
	budget  fleetexec.LookupBudget
}

func loadRetailLookupConfig() (retailLookupConfig, error) {
	// Recovery never invokes the budget or key capability. A positive inert
	// value satisfies the shared constructor without granting a spend budget.
	cfg := retailLookupConfig{budget: fleetexec.LookupBudget{MaximumLamports: 1, RollingWindow: 24 * time.Hour}}
	switch strings.TrimSpace(os.Getenv("RETAIL_LOOKUP_MODE")) {
	case "", "reconcile-only":
		return cfg, nil
	case "active":
		cfg.active = true
	default:
		return cfg, errors.New("RETAIL_LOOKUP_MODE must be reconcile-only or active")
	}
	maximum, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("RETAIL_LOOKUP_MAX_LAMPORTS")), 10, 64)
	if err != nil || maximum <= 0 {
		return cfg, errors.New("active lookup mode requires explicit positive RETAIL_LOOKUP_MAX_LAMPORTS")
	}
	cfg.budget.MaximumLamports = maximum
	if value := strings.TrimSpace(os.Getenv("RETAIL_LOOKUP_BUDGET_WINDOW")); value != "" {
		window, err := time.ParseDuration(value)
		if err != nil || window < time.Minute || window > 365*24*time.Hour || window%time.Second != 0 {
			return cfg, errors.New("RETAIL_LOOKUP_BUDGET_WINDOW must be whole seconds between one minute and 365 days")
		}
		cfg.budget.RollingWindow = window
	}
	material, err := engine.Credential("RETAIL_LOOKUP_MANAGER_KEYPAIR")
	if err != nil {
		return cfg, err
	}
	cfg.manager, err = parseRetailKey(material)
	if err != nil {
		return cfg, errors.New("invalid RETAIL_LOOKUP_MANAGER_KEYPAIR")
	}
	if base58.Encode(cfg.manager[ed25519.SeedSize:]) != retailLookupAuthority {
		return cfg, errors.New("RETAIL_LOOKUP_MANAGER_KEYPAIR must be the source standard policy authority")
	}
	return cfg, nil
}

// The callback accepts only the immutable operation's authority/payer. It is
// invoked by the worker after simulation and durable budget admission.
func (c retailLookupConfig) managerKey() func(context.Context, string) (ed25519.PrivateKey, error) {
	if !c.active {
		return nil
	}
	return func(ctx context.Context, authority string) (ed25519.PrivateKey, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(c.manager) != ed25519.PrivateKeySize || authority != retailLookupAuthority || base58.Encode(c.manager[ed25519.SeedSize:]) != authority {
			return nil, errors.New("lookup operation does not bind the configured manager authority")
		}
		return c.manager, nil
	}
}
