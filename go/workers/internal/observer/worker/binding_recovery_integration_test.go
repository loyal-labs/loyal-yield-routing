package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/earn"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/solanarpc"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
)

// countCommits counts actual COMMIT statements sent to the fixture.
type countCommits struct{ commits atomic.Int64 }

func (c *countCommits) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		c.commits.Add(1)
	}
	return ctx
}
func (*countCommits) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func observerFixturePool(t *testing.T, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("OBSERVER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OBSERVER_TEST_DATABASE_URL is required")
	}
	endpoint, err := url.Parse(dsn)
	if err != nil || endpoint.Hostname() != "127.0.0.1" || endpoint.User == nil || endpoint.User.Username() != "workers_v2" || endpoint.Path != "/workers_v2_observer" || endpoint.RawQuery != "" {
		t.Fatal("observer recovery test requires the isolated workers_v2 fixture")
	}
	if _, hasPassword := endpoint.User.Password(); hasPassword {
		t.Fatal("observer recovery fixture must not contain credentials")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The first production start recovered ~2k Earn vaults' bindings one
// transaction each inside the 30 s startup deadline and timed out after 5,954
// jobs; every restart re-enqueued them under a fresh slot. Recovery must commit
// one transaction per RPC batch, outlive any single pass deadline, and insert
// nothing when a restart reads the same confirmed state again.
func TestWatchStateRecoveryIsBatchedUnboundedByOnePassAndIdempotent(t *testing.T) {
	tracer := &countCommits{}
	pool := observerFixturePool(t, tracer)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	settings, vaultKey := solana.NewWallet().PublicKey().String(), solana.NewWallet().PublicKey().String()
	accounts := make([]watch.Account, 250)
	for index := range accounts {
		accounts[index] = watch.Account{Pubkey: solana.NewWallet().PublicKey().String(), Role: "policy"}
	}
	set := &watch.Set{Vaults: []watch.Vault{{Environment: "mainnet-beta", Settings: settings, Vault: vaultKey, VaultIndex: 1, Accounts: accounts}}}
	changed := accounts[17].Pubkey

	const passTimeout = 500 * time.Millisecond
	var slot atomic.Uint64
	slot.Store(400_000_000)
	var calls atomic.Int64
	var mu sync.Mutex
	changedData := []byte{1}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Method != "getMultipleAccounts" {
			t.Errorf("unexpected RPC %q: %v", body.Method, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var addresses []string
		if err := json.Unmarshal(body.Params[0], &addresses); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		// Each batch uses 40% of a pass; three batches exceed one pass.
		time.Sleep(passTimeout * 2 / 5)
		values := make([]map[string]any, len(addresses))
		for index, address := range addresses {
			data := []byte{0}
			if address == changed {
				mu.Lock()
				data = append([]byte(nil), changedData...)
				mu.Unlock()
			}
			values[index] = map[string]any{"lamports": 2_039_280, "owner": "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG", "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}, "executable": false, "rentEpoch": 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]any{"slot": slot.Add(7)}, "value": values}})
	}))
	defer server.Close()

	store := earn.NewStore(pool)
	handler := earn.NewHandler(store, "recovery-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	watchConsumer := handler.ConsumerName() + ":watch-observation"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, handler.ConsumerName())
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name IN ($1,$2)`, handler.ConsumerName(), watchConsumer)
	})
	runtime := &Runtime{cfg: config.Config{ProgressTimeout: 2 * passTimeout}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), rpc: solanarpc.New(server.URL, 5*time.Second), earnStore: store, earn: handler}
	if runtime.passTimeout() != passTimeout {
		t.Fatalf("pass timeout = %s, want %s", runtime.passTimeout(), passTimeout)
	}
	jobs := func() int64 {
		var count int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, handler.ConsumerName()).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	started := time.Now()
	inserted, err := runtime.recoverWatchState(ctx, set, watchConsumer, 399_999_000)
	if err != nil {
		t.Fatalf("recovery bounded by one pass deadline: %v", err)
	}
	if elapsed := time.Since(started); elapsed <= passTimeout {
		t.Fatalf("fixture did not exceed one pass (%s)", elapsed)
	}
	if inserted != 250 || jobs() != 250 {
		t.Fatalf("first recovery inserted %d (stored %d), want one job per binding", inserted, jobs())
	}
	if calls.Load() != 3 || tracer.commits.Load() != 3 {
		t.Fatalf("recovery used %d RPC reads and %d commits, want 3 batches", calls.Load(), tracer.commits.Load())
	}
	watchCursor, err := store.ReplayCursor(ctx, watchConsumer)
	if err != nil || watchCursor != 399_999_000 {
		t.Fatalf("watch observation = %d, %v; want recovery start 399999000", watchCursor, err)
	}

	// A restart reads identical confirmed state at newer slots.
	inserted, err = runtime.recoverWatchState(ctx, set, watchConsumer, 399_999_000)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 0 || jobs() != 250 {
		t.Fatalf("restart re-enqueued %d jobs (stored %d)", inserted, jobs())
	}

	// A binding whose state actually changed is recovered again.
	mu.Lock()
	changedData = []byte{2}
	mu.Unlock()
	inserted, err = runtime.recoverWatchState(ctx, set, watchConsumer, 399_999_000)
	if err != nil {
		t.Fatal(err)
	}
	var account string
	if err := pool.QueryRow(ctx, `SELECT event_payload->>'account_pubkey' FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1 ORDER BY id DESC LIMIT 1`, handler.ConsumerName()).Scan(&account); err != nil {
		t.Fatal(err)
	}
	if inserted != 1 || jobs() != 251 || account != changed {
		t.Fatalf("changed binding recovery inserted %d for %s (stored %d)", inserted, account, jobs())
	}
}
