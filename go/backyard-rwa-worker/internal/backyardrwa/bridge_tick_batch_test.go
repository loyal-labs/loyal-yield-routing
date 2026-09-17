package backyardrwa

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
	m := readyWorkerManifest(t)
	accounts := append(routeNAVFixture(t, 77), exactReportTicketAccount(t, 4))
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], 0)
	for i := range m.RuntimeBindings.BridgePolicies {
		p := &m.RuntimeBindings.BridgePolicies[i]
		data := []byte("controlled-bridge-policy-" + string(p.Action))
		p.NormalizedDigest = sha256Bytes(data)
		p.MaskedByteRanges = nil
		accounts = append(accounts, ConfirmedAccount{Address: p.Account, Owner: bridgeSquadsProgram, Lamports: 1, Data: data})
	}
	o := Observation{ObservedAt: time.Now().UTC(), Snapshot: Snapshot{ObservationID: "tick-batch", Slot: 77, RouteKind: RouteKind, RouteLane: RouteID, StrategyKey: RouteID, Fresh: true, VoltrIdleRaw: 11, VoltrStrategyIdleRaw: 0, SquadsIdleRaw: 6, LastReportAgeSeconds: 60}}
	o.routeBatch = &routeObservationBatch{Slot: 77, ObservationID: o.Snapshot.ObservationID, ManifestSHA256: m.SHA256, Accounts: accounts}
	decision := Decide(o.Snapshot)
	if decision.Action != ReportNAV {
		t.Fatalf("fixture expected report: %+v", decision)
	}
	client, _ := NewRPCClient("https://rpc.invalid")
	slot := int64(78)
	requests := map[string]int{}
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests[request.Method]++
		switch request.Method {
		case "getSlot":
			return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%d}`, slot)), nil
		case "getLatestBlockhash":
			return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":78},"value":{"blockhash":%q,"lastValidBlockHeight":999}}}`, bridgeVault)), nil
		default:
			t.Fatalf("same-tick preparation reread %s", request.Method)
			return nil, fmt.Errorf("unexpected read")
		}
	})
	prepared, evidence, err := prepareBridgeFromTickObservation(context.Background(), client, m, decision, o)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.routeBatch != o.routeBatch || evidence.Request.Action != ReportNAV || len(evidence.ExpectedEffects.Accounts) == 0 || requests["getSlot"] != 1 || requests["getLatestBlockhash"] != 1 {
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
		if _, _, err := prepareBridgeFromTickObservation(context.Background(), client, m, decision, changed); !errors.Is(err, errConfirmedObservationUnavailable) {
			t.Fatal("bad batch not held", err)
		}
	}
	slot = 110
	if _, _, err := prepareBridgeFromTickObservation(context.Background(), client, m, decision, o); !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatal("expired slot admitted", err)
	}
	slot = 78
	changed := decision
	changed.AmountRaw++
	if _, _, err := prepareBridgeFromTickObservation(context.Background(), client, m, changed, o); !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatal("ordinary decision drift is not retryable", err)
	}
	badAccounts := append([]ConfirmedAccount(nil), accounts...)
	pin, _ := m.bridgePolicy(ReportNAV)
	for i := range badAccounts {
		if badAccounts[i].Address == pin.Account {
			badAccounts[i].Owner = bridgeTokenProgram
		}
	}
	tampered := o
	copyBatch := *o.routeBatch
	copyBatch.Accounts = badAccounts
	tampered.routeBatch = &copyBatch
	if _, _, err := prepareBridgeFromTickObservation(context.Background(), client, m, decision, tampered); err == nil {
		t.Fatal("tampered policy bypassed shared constructor")
	}
}

func TestSelectorWakeWaitsForActiveTickAndCoalesces(t *testing.T) {
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
