package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/prometheus/client_golang/prometheus"
)

// Disabled read models (the Apps crons' tables) produce no lane: no RPC, no
// schema check, and no read_models failure that could page or hold readiness.
func TestDisabledReadModelsStartNoWriterAndReportNoFailure(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	registry := prometheus.NewRegistry()
	facts := engine.NewFacts(registry)
	facts.Own(engine.FamilyObserver)
	runtime := &Runtime{cfg: config.Config{Cluster: "mainnet-beta", SolanaRPCURL: server.URL}, rpc: chainClient(t, server.URL, time.Second), facts: facts}
	lane, err := runtime.NewMaintenance(context.Background())
	if err != nil || lane != nil {
		t.Fatalf("disabled read models produced lane %v, %v", lane, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("disabled read models made %d RPC calls", calls.Load())
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "loyal_family_failed_total" && len(family.GetMetric()) != 0 {
			t.Fatalf("disabled read models reported failures: %v", family.GetMetric())
		}
	}
	// Enabled, the same runtime does build the lane (and here fails on the
	// unavailable endpoint), proving the switch is the only difference.
	runtime.cfg.ReadModelsEnabled = true
	if _, err := runtime.NewMaintenance(context.Background()); !errors.Is(err, chain.ErrUnavailable) || calls.Load() == 0 {
		t.Fatalf("enabled read models did not reach the endpoint: %v (calls %d)", err, calls.Load())
	}
}
