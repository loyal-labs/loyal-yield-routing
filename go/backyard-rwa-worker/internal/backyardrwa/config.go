package backyardrwa

import (
	"fmt"
	"net/url"
	"os"
	"time"
)

const (
	RouteKind               = "backyard_rwa_v1"
	RouteID                 = "PRIME/USDC"
	OneNonterminalInvariant = "one_nonterminal_operation_per_route"
	FixedCollateral         = "PRIME"
	FixedDebt               = "USDC"
	TargetLTVBPS            = int64(5000)
)

// Config is deliberately fixed for the MVP; route selection is not configurable.
type Config struct{ PollInterval time.Duration }

func DefaultConfig() Config { return Config{PollInterval: 5 * time.Second} }

type RuntimeConfig struct{ DatabaseURL, RPCURL, RouteKey string }

func RuntimeConfigFromEnvironment() RuntimeConfig {
	return RuntimeConfig{
		DatabaseURL: os.Getenv("NEON_DATABASE_URL"),
		RPCURL:      os.Getenv("SOLANA_RPC_URL"),
		RouteKey:    os.Getenv("BACKYARD_RWA_ROUTE_KEY"),
	}
}

func (c RuntimeConfig) Validate() error {
	if c.DatabaseURL == "" || c.RouteKey == "" {
		return fmt.Errorf("database URL and route key are required")
	}
	u, e := url.Parse(c.RPCURL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("confirmed RPC URL is required")
	}
	return nil
}
