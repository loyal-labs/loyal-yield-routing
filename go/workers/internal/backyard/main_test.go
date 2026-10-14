package backyard

import (
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	landResendEvery = time.Millisecond
	// The rwa fixtures (live state, replays) were recorded under its 20% fee.
	rwa.AdminPerformanceFeeBPS = 2000
	os.Exit(m.Run())
}
