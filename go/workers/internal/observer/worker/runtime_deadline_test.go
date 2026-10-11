package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
)

// The real RPC client has a much longer transport timeout than the pass. No SQL
// result is mocked: cancellation must stop the RPC before any store is reached.
func stalledRuntimeRPC(t *testing.T, method string) (*chain.Client, <-chan struct{}) {
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
	return chainClient(t, server.URL, 5*time.Second), stopped
}

func chainClient(t *testing.T, url string, timeout time.Duration) *chain.Client {
	t.Helper()
	client, err := chain.New(url, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func requireStoppedRuntimeRPC(t *testing.T, stopped <-chan struct{}) {
	t.Helper()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("RPC request survived the bounded pass")
	}
}

func TestRuntimeStartupDeadlineStopsBlockedRPC(t *testing.T) {
	rpc, stopped := stalledRuntimeRPC(t, "getGenesisHash")
	runtime := &Runtime{cfg: config.Config{Cluster: "mainnet-beta", ProgressTimeout: 100 * time.Millisecond}, rpc: rpc}
	started := time.Now()
	if err := runtime.Run(context.Background()); err == nil {
		t.Fatal("blocked startup was accepted")
	}
	if time.Since(started) > time.Second {
		t.Fatal("startup used the transport timeout instead of its pass budget")
	}
	requireStoppedRuntimeRPC(t, stopped)
}
