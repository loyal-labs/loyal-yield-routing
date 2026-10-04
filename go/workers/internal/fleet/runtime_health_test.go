package fleet

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

func TestRuntimeReporterClosedOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &Worker{config: Config{Mode: ModeShadow, PollInterval: time.Hour}}
	reports := 0
	w.SetRuntimeReporter(func(ready bool, slot uint64) {
		reports++
		if ready {
			t.Fatal("canceled runtime reported ready")
		}
	})
	_ = w.Run(ctx)
	if reports < 2 {
		t.Fatal("startup and joined exit did not both close readiness")
	}
}

// These are real registered-schema custody states, not transaction-effect proof.
func TestRuntimeCensusDetectsExpiredUnsignedCustody(t *testing.T) {
	store, ctx := unsignedRecoveryStore(t)
	fixture := seedUnsignedAdmission(t, ctx, store, "")
	w := &Worker{store: store, config: Config{Cluster: fixture.cluster}}
	if err := w.runtimeRecoveryCensus(ctx); err != nil {
		t.Fatal(err)
	}
	expireUnsignedLease(t, ctx, store, fixture)
	if err := w.runtimeRecoveryCensus(ctx); err == nil {
		t.Fatal("expired unsigned custody reported healthy")
	}
	if n, err := store.RecoverUnsignedExecutionAdmissions(ctx, fixture.cluster, 100); err != nil || n != 1 {
		t.Fatalf("source recovery %d %v", n, err)
	}
	if err := w.runtimeRecoveryCensus(ctx); err != nil {
		t.Fatal(err)
	}
}

type failedHealthEpoch struct{}

func (failedHealthEpoch) LoadImmutableMarketEpoch(context.Context) (ImmutableMarketEpoch, error) {
	return ImmutableMarketEpoch{}, errors.New("https://rpc.invalid/test-secret")
}
func TestRuntimeCycleClosesReadinessAndSanitizesErrors(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	w := &Worker{config: Config{Mode: ModeShadow}, marketEvidence: failedHealthEpoch{}}
	reports := 0
	w.SetRuntimeReporter(func(ready bool, slot uint64) {
		reports++
		if ready {
			t.Fatal("failed evidence cycle reported ready")
		}
	})
	w.runtimeCycle(context.Background())
	if reports != 1 || !strings.Contains(output.String(), "cycle_or_recovery_health") || strings.Contains(output.String(), "test-secret") || strings.Contains(output.String(), "rpc.invalid") {
		t.Fatalf("unsafe/missing cycle report %q", output.String())
	}
}
