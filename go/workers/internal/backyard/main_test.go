package backyard

import (
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

var currentRWAForeignIdleRaw = rwa.AcknowledgedForeignIdleRaw

func TestMain(m *testing.M) {
	chain.LandResendEvery = time.Millisecond
	// The rwa fixtures (live state, replays) were recorded under its 20% fee
	// and a zero issuance fee.
	rwa.AdminPerformanceFeeBPS, rwa.IssuanceFeeBPS = 2000, 0
	// Historical captures predate the acknowledged 2026-10-30 foreign transfer.
	rwa.AcknowledgedForeignIdleRaw = 0
	os.Exit(m.Run())
}
