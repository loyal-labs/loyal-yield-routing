package fleetexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeReporterClosedOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &Worker{config: Config{TickInterval: time.Hour}}
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

// Local JSON-RPC + the registered SQL journal exercise readiness ownership;
// they do not claim chain execution or financial receipt acceptance.
func TestRuntimeCensusRequiresAdoptionAndRejectsExpiredHeldSignature(t *testing.T) {
	store, pool := integrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	baseline := seedBaseline(t, ctx, pool, fmt.Sprint(time.Now().UnixNano()))
	wire, _ := integrationWire(t, 79)
	if _, _, err := store.PersistSignedRoute(ctx, fixturePersistInput(t, ctx, pool, baseline, wire)); err != nil {
		t.Fatal(err)
	}
	var height atomic.Int64
	height.Store(1000)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, in *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(in.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch req.Method {
		case "getSlot":
			result = 1000
		case "getBlockHeight":
			result = height.Load()
		case "getSignatureStatuses":
			result = map[string]any{"context": map[string]any{"slot": 1000}, "value": []any{map[string]any{"slot": 999, "confirmationStatus": "processed", "err": nil}}}
		default:
			t.Errorf("unexpected readiness RPC %s", req.Method)
		}
		_ = json.NewEncoder(out).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer server.Close()
	adapter, err := NewRPCAdapter(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sent := 0
	w := &Worker{config: Config{Cluster: baseline.Cluster, Owner: "health-owner", LeaseTTL: time.Minute, BatchSize: 8}, store: store, broadcast: &countingBroadcast{counter: &sent}, status: adapter}
	w.SetRuntimeReporter(func(bool, uint64) {})
	if _, err := w.runtimeRecoveryHealth(ctx); err == nil {
		t.Fatal("unadopted signed work reported ready")
	}
	if _, err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if slot, err := w.runtimeRecoveryHealth(ctx); err != nil || slot != 1000 {
		t.Fatalf("owned live recovery census %d %v", slot, err)
	}
	height.Store(wire.LastValidBlockHeight + 1)
	if _, err := w.runtimeRecoveryHealth(ctx); err == nil {
		t.Fatal("expired signed ownership reported ready")
	}
}
