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

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/config"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/earn"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/stream"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
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

	// Keep each batch's real SQL work inside its deadline under -race while
	// the three simulated RPC delays still exceed one whole pass deadline.
	const passTimeout = 2 * time.Second
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
			values[index] = map[string]any{"lamports": 2_039_280, "owner": squads.ProgramID.String(), "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}, "executable": false, "rentEpoch": 0}
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
	runtime := &Runtime{cfg: config.Config{ProgressTimeout: 2 * passTimeout}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), rpc: chainClient(t, server.URL, 5*time.Second), earnStore: store, earn: handler}
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

// A session that fails after an outage longer than LaserStream's retention
// must reconnect through the same plan as a first start: a replayable
// from_slot, and the skipped range recovered from confirmed state before the
// stream resumes. The old reconnect requested frontier-overlap unclamped and
// failed every attempt with OutOfRange.
func TestReconnectAfterProviderWindowClampsAndRecoversBindings(t *testing.T) {
	pool := observerFixturePool(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const current = 449_073_607
	accounts := []watch.Account{{Pubkey: solana.NewWallet().PublicKey().String(), Role: "policy"}, {Pubkey: solana.NewWallet().PublicKey().String(), Role: "smart_account"}}
	set := &watch.Set{Vaults: []watch.Vault{{Environment: "mainnet-beta", Settings: accounts[1].Pubkey, Vault: solana.NewWallet().PublicKey().String(), VaultIndex: 1, Accounts: accounts}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch body.Method {
		case "getSlot":
			result = current
		case "getMultipleAccounts":
			var addresses []string
			_ = json.Unmarshal(body.Params[0], &addresses)
			values := make([]map[string]any, len(addresses))
			for index := range addresses {
				values[index] = map[string]any{"lamports": 1, "owner": squads.ProgramID.String(), "data": []string{"AA==", "base64"}, "executable": false, "rentEpoch": 0}
			}
			result = map[string]any{"context": map[string]any{"slot": current}, "value": values}
		default:
			t.Errorf("unexpected RPC %s", body.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer server.Close()
	store := earn.NewStore(pool)
	handler := earn.NewHandler(store, "reconnect-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	watchConsumer := handler.ConsumerName() + ":watch-observation"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, handler.ConsumerName())
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name IN ($1,$2)`, handler.ConsumerName(), watchConsumer)
	})
	connector := &recordingConnector{}
	runtime := &Runtime{cfg: config.Config{ProgressTimeout: 10 * time.Second, ReplayOverlapSlots: 32}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), rpc: chainClient(t, server.URL, 5*time.Second), connector: connector, handler: &DurableHandler{}, earnStore: store, earn: handler}
	ancientFrontier := uint64(current - 1_000_000)
	manager, watchCursor, err := runtime.resumeSession(ctx, set, []kamino.Target{{Reserve: "11111111111111111111111111111111"}}, ancientFrontier, &reconnectBackoff{}, watchConsumer, ancientFrontier-5_000)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	edge := uint64(current - laserStreamReplaySlots)
	connector.mu.Lock()
	froms := append([]uint64(nil), connector.froms...)
	connector.mu.Unlock()
	if len(froms) != 1 || froms[0] != edge {
		t.Fatalf("reconnect from_slot = %v, want provider window edge %d", froms, edge)
	}
	var jobs int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, handler.ConsumerName()).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	durable, err := store.ReplayCursor(ctx, watchConsumer)
	if err != nil {
		t.Fatal(err)
	}
	if jobs != 2 || watchCursor != edge || durable != edge {
		t.Fatalf("skipped range recovered %d jobs, watch cursor %d (durable %d); want 2 at %d", jobs, watchCursor, durable, edge)
	}
}

// Recovery must not enqueue unsigned jobs the consumer can only dead-letter:
// Earn MAX claim custody (recovered by signature history instead) and a
// classic policy found closed. Every other binding is still recovered.
func TestWatchStateRecoverySkipsSignatureOnlyFacts(t *testing.T) {
	pool := observerFixturePool(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key := func() string { return solana.NewWallet().PublicKey().String() }
	maxVault := watch.Vault{Environment: "mainnet-beta", Settings: key(), Vault: key(), VaultIndex: 0, EarnMax: true}
	custody, err := watch.USDCATA(maxVault.Vault)
	if err != nil {
		t.Fatal(err)
	}
	maxPolicy, closedPolicy, classicSettings := key(), key(), key()
	maxVault.Accounts = []watch.Account{{Pubkey: custody, Role: "idle_token"}, {Pubkey: maxPolicy, Role: "policy"}}
	classic := watch.Vault{Environment: "mainnet-beta", Settings: classicSettings, Vault: key(), VaultIndex: 1, Accounts: []watch.Account{{Pubkey: closedPolicy, Role: "policy"}, {Pubkey: classicSettings, Role: "smart_account"}}}
	set := &watch.Set{Vaults: []watch.Vault{maxVault, classic}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		var addresses []string
		_ = json.Unmarshal(body.Params[0], &addresses)
		values := make([]any, len(addresses))
		for index, address := range addresses {
			if address != closedPolicy {
				values[index] = map[string]any{"lamports": 1, "owner": squads.ProgramID.String(), "data": []string{"AA==", "base64"}, "executable": false, "rentEpoch": 0}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]any{"slot": 900}, "value": values}})
	}))
	defer server.Close()
	store := earn.NewStore(pool)
	handler := earn.NewHandler(store, "signature-only-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	watchConsumer := handler.ConsumerName() + ":watch-observation"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, handler.ConsumerName())
		_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name IN ($1,$2)`, handler.ConsumerName(), watchConsumer)
	})
	runtime := &Runtime{cfg: config.Config{ProgressTimeout: 10 * time.Second}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), rpc: chainClient(t, server.URL, 5*time.Second), earnStore: store, earn: handler}
	if _, err := runtime.recoverWatchState(ctx, set, watchConsumer, 800); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT event_payload->>'account_pubkey' FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, handler.ConsumerName())
	if err != nil {
		t.Fatal(err)
	}
	recovered := map[string]bool{}
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			t.Fatal(err)
		}
		recovered[account] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if recovered[custody] || recovered[closedPolicy] {
		t.Fatalf("recovery enqueued a signature-only fact: %v", recovered)
	}
	if !recovered[maxPolicy] || !recovered[classicSettings] || len(recovered) != 2 {
		t.Fatalf("recovered bindings = %v, want the Earn MAX policy and classic smart account", recovered)
	}
}

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

// Production at swap time: Rust's earn-smart-account:mainnet cursor is fresh
// and the Go-only watch-observation cursor does not exist. Rust continued from
// its Earn cursor by replay alone; Go must do the same instead of enqueuing a
// state-recovery job for every binding (81,130 jobs in the rehearsal), and
// must record that continuity as its watch observation. A database with no
// Earn continuity at all is still recovered.
func TestContinuingRustEarnCursorReplaysWithoutStateRecovery(t *testing.T) {
	pool := observerFixturePool(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const current = 449_073_607
	if policy, err := earn.NewStore(pool).ProjectionCursor(ctx, earn.PolicyProjectionConsumer); err != nil || (policy > 0 && policy < current-100_000) {
		t.Fatalf("fixture policy cursor %d would gap this scenario: %v", policy, err)
	}
	server := slotRPC(t, current)
	accounts := []watch.Account{{Pubkey: solana.NewWallet().PublicKey().String(), Role: "policy"}, {Pubkey: solana.NewWallet().PublicKey().String(), Role: "smart_account"}}
	set := &watch.Set{Vaults: []watch.Vault{{Environment: "mainnet-beta", Settings: accounts[1].Pubkey, Vault: solana.NewWallet().PublicKey().String(), VaultIndex: 1, Accounts: accounts}}}
	targets := []kamino.Target{{Reserve: "11111111111111111111111111111111"}}
	for _, scenario := range []struct {
		name       string
		earnCursor uint64
	}{{"continues_rust", current - 900}, {"fresh_database", 0}} {
		t.Run(scenario.name, func(t *testing.T) {
			store := earn.NewStore(pool)
			handler := earn.NewHandler(store, "continue-"+strconv.FormatInt(time.Now().UnixNano(), 10))
			watchConsumer := handler.ConsumerName() + ":watch-observation"
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, handler.ConsumerName())
				_, _ = pool.Exec(context.Background(), `DELETE FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name IN ($1,$2)`, handler.ConsumerName(), watchConsumer)
			})
			if scenario.earnCursor > 0 {
				// The row Rust left: its Earn cursor, and no watch observation.
				if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.laserstream_replay_cursors(consumer_name,durable_slot) VALUES($1,$2)`, handler.ConsumerName(), int64(scenario.earnCursor)); err != nil {
					t.Fatal(err)
				}
			}
			connector := &recordingConnector{}
			runtime := &Runtime{cfg: config.Config{ProgressTimeout: 10 * time.Second, ReplayOverlapSlots: 32}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), rpc: chainClient(t, server.URL, 5*time.Second), connector: connector, handler: &DurableHandler{}, earnStore: store, earn: handler}
			manager, plan, watchCursor, err := runtime.startSession(ctx, set, targets, current-50, 0, watchConsumer, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			var jobs int64
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name=$1`, handler.ConsumerName()).Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			durable, err := store.ReplayCursor(ctx, watchConsumer)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.earnCursor > 0 {
				if jobs != 0 || plan.from != scenario.earnCursor-32 || watchCursor != scenario.earnCursor || durable != scenario.earnCursor {
					t.Fatalf("continuing Rust enqueued %d recovery jobs, from %d, watch %d (durable %d); want replay from %d and watch %d", jobs, plan.from, watchCursor, durable, scenario.earnCursor-32, scenario.earnCursor)
				}
				return
			}
			if jobs != 2 || watchCursor != plan.from || durable != plan.from {
				t.Fatalf("fresh database recovered %d jobs, watch %d (durable %d), want 2 at %d", jobs, watchCursor, durable, plan.from)
			}
		})
	}
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
