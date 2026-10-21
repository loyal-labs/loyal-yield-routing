package backyard

import (
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func TestMain(m *testing.M) {
	chain.LandResendEvery = time.Millisecond
	// The rwa fixtures (live state, replays) were recorded under its 20% fee.
	rwa.AdminPerformanceFeeBPS = 2000
	os.Exit(m.Run())
}
