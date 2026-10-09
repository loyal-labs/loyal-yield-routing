package backyard

import (
	"fmt"
	"net/url"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

const (
	RouteKind      = "backyard_rwa_v1"
	RouteID        = "PRIME/USDC"
	PhaseOneLaneID = "Prime/PRIME/USDC"
	// Phase 2 freezes one additional installed representative. This is a
	// compile-time lane, never caller input or runtime route selection.
	SelectedRouteID   = "Maple/syrupUSDC/USDC"
	RuntimeRouteCount = 3
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
	// M7 pins the intentional admin performance fee exactly; every other
	// performance, management, issuance and redemption term remains zero.
	approvedAdminPerformanceFeeBPS = int64(2000)
	// Batch routine fee-bearing reports to avoid whole-LP rounding consuming
	// small pilot gains. Withdrawal and post-mutation reporting keep priority.
	routineNAVReportInterval = time.Hour
	// Unharvested fee LP above this share of effective supply warns only. It
	// is not a fee-rate limit, harvest trigger or permission to stop withdrawals.
	feeAccumulatorWarningBPS = int64(100)
)

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

// RuntimeConfig is the Backyard-only process configuration the one-shot
// operator commands read. Separately deployed engine instances have
// independent credentials; neither reads the other's URLs or keys.
type RuntimeConfig struct {
	DatabaseURL, RPCURL, TimescaleURL string
	// Jupiter is the swap/v1 client the selector evaluate command quotes
	// through.
	Jupiter *jupiter.Client
}

func (c RuntimeConfig) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("database URL is required")
	}
	u, e := url.Parse(c.RPCURL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("confirmed RPC URL is required")
	}
	return nil
}

func (c Config) validateLease() error {
	if c.PollInterval <= 0 || c.LeaseTTL < 30*time.Millisecond || c.LeaseRefreshInterval <= 0 ||
		c.LeaseRefreshInterval > c.LeaseTTL/3 {
		return fmt.Errorf("invalid bounded route lease configuration")
	}
	return nil
}
