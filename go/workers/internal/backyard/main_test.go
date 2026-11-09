package backyard

import (
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

var productionRWAInvestmentExit = rwa.CheckInvestmentExits
var productionLoopingInvestmentExit = looping.CheckInvestmentExits

var productionRWAPolicy = rwa.selectorPolicy()
var productionLoopingPolicy = looping.selectorPolicy()

func TestMain(m *testing.M) {
	chain.LandResendEvery = time.Millisecond
	// The rwa fixtures (live state, replays) were recorded under its 20% fee
	// and a zero issuance fee.
	rwa.AdminPerformanceFeeBPS, rwa.IssuanceFeeBPS = 2000, 0
	// Retained October 12 fixtures also predate the shorter payback horizon.
	rwa.EconomicHorizon = selectorHorizon
	// Historical lifecycle fixtures predate admission limits; new policy tests opt in.
	rwa.DiversifyNewCash = false
	looping.DiversifyNewCash, looping.IdleBufferBPS, looping.EconomicHorizon = false, 0, selectorHorizon
	// Historical replay fixtures retain their original trade sizes and timing.
	rwa.ProtectExecution, looping.ProtectExecution = false, false
	rwa.CheckInvestmentExits, looping.CheckInvestmentExits = false, false
	os.Exit(m.Run())
}
