package fleetexec

import (
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func TestMain(m *testing.M) {
	chain.LandResendEvery = time.Millisecond
	os.Exit(m.Run())
}
