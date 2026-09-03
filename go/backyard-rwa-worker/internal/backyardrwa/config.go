package backyardrwa

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"time"
)

const (
	RouteKind      = "backyard_rwa_v1"
	RouteID        = "PRIME/USDC"
	PhaseOneLaneID = "Prime/PRIME/USDC"
	// Phase 2 freezes one additional installed representative. This is a
	// compile-time lane, never caller input or runtime route selection.
	SelectedRouteID   = "Maple/syrupUSDC/USDC"
	RuntimeRouteCount = 2
	// The Phase 2 authorization envelope permits at most 1 USDC-equivalent per
	// money-moving transaction. Selected-lane decisions are clamped before they
	// are journaled, quoted, signed, or broadcast.
	Phase2TransactionCapRaw int64 = 1_000_000
	OneNonterminalInvariant       = "one_nonterminal_operation_per_route"
	FixedCollateral               = "PRIME"
	FixedDebt                     = "USDC"
	TargetLTVBPS                  = int64(5000)
)

var renderServiceIDPattern = regexp.MustCompile(`^srv-[a-z0-9]+$`)

// Config is deliberately fixed for the MVP; route selection is not configurable.
type Config struct {
	PollInterval         time.Duration
	LeaseTTL             time.Duration
	LeaseRefreshInterval time.Duration
}

func DefaultConfig() Config {
	return Config{
		PollInterval:         5 * time.Second,
		LeaseTTL:             30 * time.Second,
		LeaseRefreshInterval: 10 * time.Second,
	}
}

type RuntimeConfig struct {
	DatabaseURL, RPCURL, RouteKey string
	RenderServiceID, ImageVersion string
}

func RuntimeConfigFromEnvironment() RuntimeConfig {
	return RuntimeConfig{
		DatabaseURL:     os.Getenv("NEON_DATABASE_URL"),
		RPCURL:          os.Getenv("SOLANA_RPC_URL"),
		RouteKey:        os.Getenv("BACKYARD_RWA_ROUTE_KEY"),
		RenderServiceID: os.Getenv("RENDER_SERVICE_ID"),
		ImageVersion:    os.Getenv("LOYAL_IMAGE_VERSION"),
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

func (c RuntimeConfig) LeaseOwner() (string, error) {
	if !renderServiceIDPattern.MatchString(c.RenderServiceID) {
		return "", fmt.Errorf("Backyard worker requires a Render service ID")
	}
	if !immutableImageVersionPattern.MatchString(c.ImageVersion) {
		return "", fmt.Errorf("Backyard worker requires an immutable image version")
	}
	return "render:" + c.RenderServiceID + ":" + c.ImageVersion, nil
}

func (c Config) validateLease() error {
	if c.PollInterval <= 0 || c.LeaseTTL < 30*time.Millisecond || c.LeaseRefreshInterval <= 0 ||
		c.LeaseRefreshInterval > c.LeaseTTL/3 {
		return fmt.Errorf("invalid bounded route lease configuration")
	}
	return nil
}
