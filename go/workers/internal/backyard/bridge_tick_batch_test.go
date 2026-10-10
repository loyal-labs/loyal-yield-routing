package backyard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestBridgeTickBatchSkipsAccountReadsAndRejectsStaleOrTamperedEvidence(t *testing.T) {
	t.Parallel()
	m := readyWorkerManifest(t)
	accounts := append(routeNAVFixture(t, 77), exactReportTicketAccount(t, 4))
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], 0)
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: Snapshot{ObservationID: "tick-batch", Slot: 77, RouteKind: RouteKind, RouteLane: RouteID, StrategyKey: RouteID, Fresh: true, VoltrIdleRaw: 11, VoltrStrategyIdleRaw: 0, SquadsIdleRaw: 6, LastReportAgeSeconds: 3600}}
	o.routeBatch = &routeObservationBatch{Slot: 77, ObservationID: o.Snapshot.ObservationID, ManifestSHA256: m.SHA256, Accounts: accounts}
	decision := Decide(o.Snapshot)
	if decision.Action != ReportNAV {
		t.Fatalf("fixture expected report: %+v", decision)
	}
	client, view := newFakeChain(t, nil), accountView(t, 78, nil)
	requests := map[string]int{}
	uninstalled := false
	rpcOf(client).Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests[request.Method]++
		switch request.Method {
		case "getProgramAccounts":
			result := capturedPolicyProgramAccounts(78)
			if uninstalled {
				result["value"] = []any{}
			}
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
			return response(string(raw)), nil
		case "getLatestBlockhash":
			return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":78},"value":{"blockhash":%q,"lastValidBlockHeight":999}}}`, bridgeVault)), nil
		default:
			t.Fatalf("same-tick preparation reread %s", request.Method)
			return nil, fmt.Errorf("unexpected read")
		}
	})
	prepared, evidence, err := prepareBridgeFromTickObservation(context.Background(), client, view, m, decision, o)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.routeBatch != o.routeBatch || evidence.Request.Action != ReportNAV || len(evidence.ExpectedEffects.Accounts) == 0 || requests["getProgramAccounts"] != 1 || requests["getLatestBlockhash"] != 1 {
		t.Fatal("lost shared batch or unexpected preparation reads", requests)
	}
	withBatch, _ := json.Marshal(o)
	without := o
	without.routeBatch = nil
	withoutBatch, _ := json.Marshal(without)
	if string(withBatch) != string(withoutBatch) {
		t.Fatal("tick-local batch leaked into persisted JSON")
	}
	for _, change := range []func(*Observation){func(x *Observation) { x.routeBatch = nil }, func(x *Observation) { x.ObservedAt = x.ObservedAt.Add(-31 * time.Second) }, func(x *Observation) { x.Snapshot.Slot++ }, func(x *Observation) { x.Snapshot.ObservationID = "changed" }} {
		changed := o
		change(&changed)
		if _, _, err := prepareBridgeFromTickObservation(context.Background(), client, view, m, decision, changed); !errors.Is(err, errConfirmedObservationUnavailable) {
			t.Fatal("bad batch not held", err)
		}
	}
	if _, _, err := prepareBridgeFromTickObservation(context.Background(), client, accountView(t, 110, nil), m, decision, o); !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatal("expired slot admitted", err)
	}
	changed := decision
	changed.AmountRaw++
	if _, _, err := prepareBridgeFromTickObservation(context.Background(), client, view, m, changed, o); !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatal("ordinary decision drift is not retryable", err)
	}
	uninstalled = true
	_, _, err = prepareBridgeFromTickObservation(context.Background(), client, view, m, decision, o)
	assertBudgetHold(t, err, "REPORT_NAV policy not installed")
}

func TestSelectorWakeWaitsForActiveTickAndCoalesces(t *testing.T) {
	t.Parallel()
	w := Worker{interval: time.Hour, wake: make(chan struct{}, 1)}
	w.notifySelectorCommit("KEEP")
	if len(w.wake) != 0 {
		t.Fatal("non-actionable selector woke worker")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var calls atomic.Int32
	go func() {
		done <- w.runTicks(ctx, make(chan error), func(context.Context) error {
			n := calls.Add(1)
			if n == 1 {
				close(started)
				<-release
			}
			if n == 2 {
				cancel()
			}
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first tick absent")
	}
	for _, action := range []string{"ENTER", "CANARY_ENTER", "SWITCH"} {
		w.notifySelectorCommit(action)
	}
	if calls.Load() != 1 || len(w.wake) != 1 {
		t.Fatal("wake started concurrent tick or failed to coalesce")
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 2 {
			t.Fatal("wake did not trigger one immediate serialized tick", err, calls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("wake waited for hourly poll")
	}
}
