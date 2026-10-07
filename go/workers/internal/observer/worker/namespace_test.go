package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/solanarpc"
)

func TestObserverRefusesForeignRPCBeforeOpeningWriters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"foreign-genesis"}`)
	}))
	defer server.Close()
	_, err := New(context.Background(), config.Config{Cluster: "mainnet-beta", SolanaRPCURL: server.URL, NeonDatabaseURL: "unusable-yield"}, nil, nil)
	if err == nil || err.Error() != "observer watch RPC is not Solana mainnet" {
		t.Fatalf("foreign endpoint reached writer initialization: %v", err)
	}
}

func TestWatchRefreshRechecksActualNamespace(t *testing.T) {
	var genesis atomic.Value
	genesis.Store("5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%q}`, genesis.Load())
	}))
	defer server.Close()
	runtime := &Runtime{cfg: config.Config{Cluster: "mainnet-beta"}, rpc: solanarpc.New(server.URL, time.Second)}
	if err := validateWatchNamespace(context.Background(), runtime.cfg.Cluster, runtime.rpc); err != nil {
		t.Fatal(err)
	}
	genesis.Store("foreign-genesis")
	// No loader/store is installed: reaching either would panic. Namespace
	// failure must retain the old watch without reading or mutating projections.
	if set, targets, err := runtime.load(context.Background()); err == nil || set != nil || targets != nil {
		t.Fatalf("changed RPC admitted watch refresh: %v %v %v", set, targets, err)
	}
}
