package fleetexec

import (
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
)

func TestMain(m *testing.M) {
	resendEvery, lookupResendEvery = time.Millisecond, time.Millisecond
	os.Exit(m.Run())
}

func testFacts() *engine.Facts { return engine.NewFacts(prometheus.NewRegistry()) }
