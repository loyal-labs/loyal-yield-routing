package backyard

import (
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func TestMain(m *testing.M) {
	chain.LandResendEvery = time.Millisecond
	// The rwa fixtures (live state, replays) were recorded under its 20% fee
	// and a zero issuance fee.
	rwa.AdminPerformanceFeeBPS, rwa.IssuanceFeeBPS = 2000, 0
	os.Exit(m.Run())
}
