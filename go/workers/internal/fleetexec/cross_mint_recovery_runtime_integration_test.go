package fleetexec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCrossMintKeylessRuntimeCannotStartUnsignedWorkOrReportItsCustodyReady(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	movement := seedCrossMintMovement(t, ctx, pool)
	var rpcCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpcCalls.Add(1)
		t.Error("unsigned keyless custody must close readiness before RPC")
	}))
	defer server.Close()
	adapter, err := NewRPCAdapter(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewCrossMintRecoveryRuntime(ctx, Config{Cluster: movement.Cluster, Owner: "keyless-owner", LeaseTTL: time.Minute, BatchSize: 1, TickInterval: time.Second, Facts: testFacts()}, store, adapter, &runtimeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	admission := &rolloutAdmission{}
	runtime.SetActivationSource(admission)
	if n, err := runtime.Tick(ctx); err != nil || n != 0 || admission.calls != 0 {
		t.Fatalf("keyless runtime admitted fresh work: n=%d calls=%d err=%v", n, admission.calls, err)
	}
	lease, err := store.ClaimCrossMintContinuation(ctx, movement.Cluster, "keyless-owner", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("unsigned custody fixture lost source lease: %v", err)
	}
	var submissions int
	var terminal *string
	if err := pool.QueryRow(ctx, `SELECT d.terminal_outcome,(SELECT count(*) FROM loyal_yield.signed_route_submissions s WHERE s.decision_id=d.id) FROM loyal_yield.rebalance_decisions d WHERE d.id=$1`, movement.DecisionID).Scan(&terminal, &submissions); err != nil || terminal != nil || submissions != 0 {
		t.Fatalf("keyless runtime changed custody or manufactured a packet: terminal=%v attempts=%d err=%v", terminal, submissions, err)
	}
}
