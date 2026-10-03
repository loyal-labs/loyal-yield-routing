package backyard

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

// OwnerScope is the Backyard deployment scope in the platform-neutral lease
// owner identity. It is fixed: a Backyard engine instance never shares owner
// text with a retail engine instance.
const OwnerScope = "backyard"

// instanceIDPattern and immutableReleasePattern deliberately match the shared
// engine instance identity rules so one deployment never produces two owner
// dialects. Authority still comes from the database fencing token; this text
// only identifies the holder for stale-owner rejection.
var instanceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)
var immutableReleasePattern = regexp.MustCompile(`^sha-[0-9a-f]{40}$`)

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

// RuntimeConfig carries the Backyard-only process configuration. It is
// deliberately separate from retail configuration: separately deployed engine
// instances have independent credentials, and neither instance reads the
// other's database URL, RPC URL, or signing material.
type RuntimeConfig struct {
	DatabaseURL, RPCURL, RouteKey string
	InstanceID, ImageVersion      string
}

func RuntimeConfigFromEnvironment() RuntimeConfig {
	instance := os.Getenv("LOYAL_WORKER_INSTANCE")
	if instance == "" {
		// A Render deployment ID remains a unique instance during the cutover.
		// Only the owner text below becomes platform neutral; the lease
		// generation and stale-owner rejection semantics are unchanged.
		instance = os.Getenv("RENDER_SERVICE_ID")
	}
	return RuntimeConfig{
		DatabaseURL:  os.Getenv("NEON_DATABASE_URL"),
		RPCURL:       os.Getenv("SOLANA_RPC_URL"),
		RouteKey:     os.Getenv("BACKYARD_RWA_ROUTE_KEY"),
		InstanceID:   instance,
		ImageVersion: os.Getenv("LOYAL_IMAGE_VERSION"),
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

// LeaseOwner is the platform-neutral deployment identity: the fixed Backyard
// scope, a unique process instance, and an immutable release. It replaces the
// previous Render-only owner text without touching lease acquisition,
// generation increments, or exact owner/token release fencing.
func (c RuntimeConfig) LeaseOwner() (string, error) {
	if !instanceIDPattern.MatchString(c.InstanceID) {
		return "", fmt.Errorf("Backyard worker requires a unique instance identity")
	}
	if !immutableReleasePattern.MatchString(c.ImageVersion) {
		return "", fmt.Errorf("Backyard worker requires an immutable release version")
	}
	return "worker:" + OwnerScope + ":" + c.InstanceID + ":" + c.ImageVersion, nil
}

func (c Config) validateLease() error {
	if c.PollInterval <= 0 || c.LeaseTTL < 30*time.Millisecond || c.LeaseRefreshInterval <= 0 ||
		c.LeaseRefreshInterval > c.LeaseTTL/3 {
		return fmt.Errorf("invalid bounded route lease configuration")
	}
	return nil
}
