package backyard

import (
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

var productionRWAPolicy = rwa.selectorPolicy()

func TestMain(m *testing.M) {
	chain.LandResendEvery = time.Millisecond
	// The rwa fixtures (live state, replays) were recorded under its 20% fee
	// and a zero issuance fee.
	rwa.AdminPerformanceFeeBPS, rwa.IssuanceFeeBPS = 2000, 0
	// Retained October 12 fixtures also predate the shorter payback horizon.
	rwa.EconomicHorizon = selectorHorizon
	os.Exit(m.Run())
}
