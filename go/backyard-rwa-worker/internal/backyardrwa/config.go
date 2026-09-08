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

// Phase 2 monitor knobs. They are code constants on purpose: loosening a
// fail-closed bound must be a reviewed change, not environment configuration.
const (
	// Program-identity pins (M6): for each pinned program the ProgramData
	// address, the last-deploy slot recorded in that account, and the sha256 of
	// the executable bytes only, ProgramData.data[45:], past the loader
	// discriminant, deploy slot, option byte, and upgrade authority. Hashing
	// past the header keeps an upgrade-authority rotation from reading as a new
	// binary and lets any external tool reproduce the pin byte for byte
	// (scripts/voltr_deploy_check.py prints the identical digest). Slots and
	// hashes were computed from a read-only mainnet fetch on 2026-09-08. Every
	// tick compares the slot; a moved slot forces a full re-hash against these
	// pins, and any absent, incoherent, or mismatched read stops the worker for
	// manual recovery instead of failing one tick.
	voltrProgramDataAddress   = "3fiAyUjktZkZf6hcbBPy6U6UdkMdEFoToS4sjtzAd5az"
	voltrProgramDataSHA256    = "bf1c1831b3d6350f4340badb942bd2e7bfaca4aa89276cb65e8480aa30d44c56"
	adaptorProgramDataAddress = "DrvzixaVmAuPVVJPtP5wykb9mvgDWqZbvZau9oiCUpHu"
	adaptorProgramDataSHA256  = "8361a469833fa17df8f62f9c4b8055aa859552fe8db7610b8fcb3af6ac6eb6d5"
	voltrProgramDeploySlot    = int64(445223838)
	adaptorProgramDeploySlot  = int64(443528877)
	// S1/S2: the observed NAV may drift from the last reported NAV by this
	// bounded relative tolerance; beyond it the book is unexplained and the
	// route stops instead of reporting.
	navDriftToleranceBPS = int64(50)
	navDriftFloorRaw     = int64(1_000)
	// M7: un-harvested LP fee accumulators are bounded as a share of the LP
	// supply Voltr actually prices against.
	feeAccumulatorMaxBPS = int64(100)
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
