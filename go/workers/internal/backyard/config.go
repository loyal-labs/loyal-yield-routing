package backyard

import (
	"fmt"
	"net/url"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

const (
	RouteKind = "backyard_rwa_v1"
	// The Prime/PRIME/USDC and Maple lanes' names (route_catalog.go).
	PrimeUSDCLaneID               = "Prime/PRIME/USDC"
	MapleSyrupUSDCLaneID          = "Maple/syrupUSDC/USDC"
	Phase2TransactionCapRaw int64 = 1_000_000
	OneNonterminalInvariant       = "one_nonterminal_operation_per_route"
)

const (
	navDriftToleranceBPS     = int64(50)
	navDriftFloorRaw         = int64(1_000)
	routineNAVReportInterval = time.Hour
	feeAccumulatorWarningBPS = int64(100)
)

type Config struct {
	PollInterval time.Duration
}

func DefaultConfig() Config {
	return Config{PollInterval: 5 * time.Second}
}

type RuntimeConfig struct {
	DatabaseURL, RPCURL string
	Jupiter             *jupiter.Client
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
