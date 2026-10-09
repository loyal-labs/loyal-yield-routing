package autodeposit

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

func TestMain(m *testing.M) {
	landResendEvery, landWindow = time.Millisecond, 200*time.Millisecond
	os.Exit(m.Run())
}

func testFacts() *engine.Facts { return engine.NewFacts(prometheus.NewRegistry()) }

func svmWire(t *testing.T, attempt DurableAttempt) []byte {
	t.Helper()
	wire, err := base64.StdEncoding.DecodeString(attempt.SignedTransactionBase64)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// svmObserve reads one signature through the landing chain as an attempt state.
func svmObserve(ctx context.Context, cluster chain.LandChain, attempt DurableAttempt) (AttemptObservation, error) {
	state, err := cluster.SignatureState(ctx, attempt.Signature)
	if err != nil || !state.Found || state.Commitment < chain.Confirmed {
		return AttemptObservation{State: AttemptUnknown}, err
	}
	if state.Err != "" {
		return AttemptObservation{State: AttemptFailed}, nil
	}
	slot := int64(state.Slot)
	return AttemptObservation{State: AttemptConfirmed, ConfirmedSlot: &slot}, nil
}
