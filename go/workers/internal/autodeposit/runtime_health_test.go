package autodeposit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// This is transport evidence: the real RPCChain performs getSlot with confirmed
// commitment, rather than deriving progress from queue length or account work.
func TestRuntimeConfirmedSlotUsesActualRPC(t *testing.T) {
	for _, result := range []int64{87321, 0} {
		t.Run(time.Duration(result).String(), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Method string `json:"method"`
					Params []struct {
						Commitment string `json:"commitment"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Method != "getSlot" || len(request.Params) != 1 || request.Params[0].Commitment != "confirmed" {
					t.Errorf("unexpected chain frontier request: %+v", request)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
			}))
			defer server.Close()
			chain, err := NewRPCChain(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			slot, err := chain.ConfirmedSlot(context.Background())
			if result > 0 && (err != nil || slot != result) {
				t.Fatalf("slot=%d error=%v", slot, err)
			}
			if result == 0 && err == nil {
				t.Fatal("zero RPC result manufactured readiness progress")
			}
		})
	}
}

func TestRuntimeConfirmedSlotCancellationJoins(t *testing.T) {
	entered, left := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		close(entered)
		<-r.Context().Done()
		close(left)
	}))
	defer server.Close()
	chain, err := NewRPCChain(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := chain.ConfirmedSlot(ctx); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe never entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("probe error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not join")
	}
	select {
	case <-left:
	case <-time.After(time.Second):
		t.Fatal("RPC request survived cancellation")
	}
}

type runtimeControlReader func(context.Context, ControlTarget, int64) (ControlObservation, error)

func (f runtimeControlReader) ObserveControl(ctx context.Context, target ControlTarget, slot int64) (ControlObservation, error) {
	return f(ctx, target, slot)
}

func TestRuntimeCancellationReportsInitialAndExitHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w, err := NewWorker(WorkerDependencies{Store: &Store{}, Executor: &scriptedExecutor{}})
	if err != nil {
		t.Fatal(err)
	}
	var reports int
	w.SetRuntimeReporter(func(ready bool, slot uint64) {
		reports++
		if ready || slot != 0 {
			t.Errorf("canceled runtime reported ready=%v slot=%d", ready, slot)
		}
	})
	if err := w.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("worker exit=%v", err)
	}
	if reports < 2 {
		t.Fatalf("missing initial/exit hold: %d", reports)
	}
	r := &ControlReconciler{Store: &Store{}, Reader: runtimeControlReader(func(context.Context, ControlTarget, int64) (ControlObservation, error) {
		t.Error("canceled control performed work")
		return ControlObservation{}, nil
	})}
	reports = 0
	r.SetRuntimeReporter(func(ready bool, slot uint64) {
		reports++
		if ready || slot != 0 {
			t.Errorf("control ready=%v slot=%d", ready, slot)
		}
	})
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("control exit=%v", err)
	}
	if reports < 2 {
		t.Fatalf("missing control initial/exit hold: %d", reports)
	}
}
