package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/stream"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// recordingConnector records each session's from_slot and keeps it open.
type recordingConnector struct {
	mu    sync.Mutex
	froms []uint64
}

func (c *recordingConnector) Open(ctx context.Context, request *pb.SubscribeRequest) (stream.OpenStream, error) {
	c.mu.Lock()
	c.froms = append(c.froms, request.GetFromSlot())
	c.mu.Unlock()
	return &idleStream{ctx: ctx}, nil
}

type idleStream struct{ ctx context.Context }

func (s *idleStream) Recv() (*pb.SubscribeUpdate, error) { <-s.ctx.Done(); return nil, s.ctx.Err() }
func (*idleStream) Send(*pb.SubscribeRequest) error      { return nil }
func (*idleStream) CloseSend() error                     { return nil }
func (*idleStream) Close() error                         { return nil }

// slotRPC serves getSlot and an account read for every requested address.
func slotRPC(t *testing.T, current uint64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var result any = current
		if body.Method == "getMultipleAccounts" {
			var addresses []string
			_ = json.Unmarshal(body.Params[0], &addresses)
			values := make([]map[string]any, len(addresses))
			for index := range addresses {
				values[index] = map[string]any{"lamports": 1, "owner": squads.ProgramID.String(), "data": []string{"AA==", "base64"}, "executable": false, "rentEpoch": 0}
			}
			result = map[string]any{"context": map[string]any{"slot": current}, "value": values}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(server.Close)
	return server
}

// One poisoned update kills every session right after it starts. Each
// reconnect must wait, and keep doubling while no session gets past the
// failure; progress past it restarts at the base delay. The old reconnect
// waited only when a start itself failed: 55 reconnects in 30 s.
func TestEveryReconnectBacksOffUntilAFrontierPassesTheFailure(t *testing.T) {
	const current = 449_073_607
	server := slotRPC(t, current)
	connector := &recordingConnector{}
	runtime := &Runtime{cfg: config.Config{ProgressTimeout: 10 * time.Second, ReplayOverlapSlots: 32}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), rpc: chainClient(t, server.URL, 5*time.Second), connector: connector, handler: &DurableHandler{}}
	set := &watch.Set{}
	targets := []kamino.Target{{Reserve: "11111111111111111111111111111111"}}
	var backoff reconnectBackoff
	reconnect := func(frontier uint64) time.Duration {
		started := time.Now()
		manager, _, err := runtime.resumeSession(context.Background(), set, targets, frontier, &backoff, "unused", current-1_000)
		if err != nil {
			t.Fatal(err)
		}
		manager.Close()
		return time.Since(started)
	}
	poisoned := uint64(current - 100)
	if waited := reconnect(poisoned); waited < reconnectBaseDelay {
		t.Fatalf("first reconnect after a started session waited %s", waited)
	}
	if waited := reconnect(poisoned); waited < 2*reconnectBaseDelay {
		t.Fatalf("second reconnect at the same failure waited %s, want doubled backoff", waited)
	}
	if waited := reconnect(poisoned + 50); waited < reconnectBaseDelay || waited >= 2*reconnectBaseDelay {
		t.Fatalf("reconnect after progress waited %s, want the base delay", waited)
	}
	for range 8 {
		backoff.next(poisoned + 50)
	}
	if backoff.delay != reconnectMaxDelay {
		t.Fatalf("backoff grew to %s, want the %s ceiling", backoff.delay, reconnectMaxDelay)
	}
}
