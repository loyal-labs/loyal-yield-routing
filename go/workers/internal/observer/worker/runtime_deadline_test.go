package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/observability"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/solanarpc"
)

// The real RPC client has a much longer transport timeout than the pass. No SQL
// result is mocked: cancellation must stop the RPC before any store is reached.
func stalledRuntimeRPC(t *testing.T, method string) (*solanarpc.Client, <-chan struct{}) {
	t.Helper()
	stopped := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Method != method {
			t.Errorf("unexpected RPC method %q: %v", body.Method, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case <-request.Context().Done():
			stopped <- struct{}{}
		case <-time.After(2 * time.Second):
			t.Error("runtime did not cancel blocked RPC")
			w.WriteHeader(http.StatusGatewayTimeout)
		}
	}))
	t.Cleanup(server.Close)
	return solanarpc.New(server.URL, 5*time.Second), stopped
}

func requireStoppedRuntimeRPC(t *testing.T, stopped <-chan struct{}) {
	t.Helper()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("RPC request survived the bounded pass")
	}
}

func TestRuntimeStartupDeadlineHoldsWatchReadiness(t *testing.T) {
	rpc, stopped := stalledRuntimeRPC(t, "getGenesisHash")
	health := observability.NewHealth()
	health.SetDomainReady("watch", true)
	runtime := &Runtime{cfg: config.Config{Cluster: "mainnet-beta", ProgressTimeout: 100 * time.Millisecond}, rpc: rpc, health: health}
	started := time.Now()
	if err := runtime.Run(context.Background()); err == nil {
		t.Fatal("blocked startup was accepted")
	}
	if time.Since(started) > time.Second {
		t.Fatal("startup used the transport timeout instead of its pass budget")
	}
	requireStoppedRuntimeRPC(t, stopped)
	response := httptest.NewRecorder()
	health.Handler(time.Second).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var result struct {
		DomainGates map[string]bool `json:"domainGates"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.DomainGates["watch"] {
		t.Fatal("incomplete startup reopened the watch gate")
	}
}

func TestRuntimeVerifyDeadlineCancelsActualRPC(t *testing.T) {
	rpc, stopped := stalledRuntimeRPC(t, "getMultipleAccounts")
	handler := kamino.NewHandler(nil, rpc, slog.New(slog.NewTextHandler(io.Discard, nil)), 400, false)
	handler.SetTargets([]kamino.Target{{Reserve: "11111111111111111111111111111111"}})
	runtime := &Runtime{cfg: config.Config{ProgressTimeout: 100 * time.Millisecond}, kamino: handler}
	started := time.Now()
	if err := runtime.verifyPass(context.Background()); err == nil {
		t.Fatal("blocked verification was accepted")
	}
	if time.Since(started) > time.Second {
		t.Fatal("verification used the transport timeout instead of its pass budget")
	}
	requireStoppedRuntimeRPC(t, stopped)
}
