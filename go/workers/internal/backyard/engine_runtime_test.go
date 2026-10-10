package backyard

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// restartJournalFixture is the reviewable signed-unsent restart journal kept in
// go/workers/testdata/backyard. It models the persisted state a crashed process
// hands to its successor: an immutable signed wire whose send may or may not
// have landed, plus both owner dialects accepted during the cutover.
type restartJournalFixture struct {
	Schema      string `json:"schema"`
	RouteKey    string `json:"routeKey"`
	LeaseOwner  string `json:"leaseOwner"`
	LegacyOwner string `json:"legacyOwner"`
	Operation   struct {
		OperationID             string `json:"operationId"`
		Status                  string `json:"status"`
		Action                  string `json:"action"`
		StrategyKey             string `json:"strategyKey"`
		IdempotencyKey          string `json:"idempotencyKey"`
		SignedWireBase64        string `json:"signedWireBase64"`
		SignedWireSHA256        string `json:"signedWireSha256"`
		TransactionSignature    string `json:"transactionSignature"`
		RecentBlockhash         string `json:"recentBlockhash"`
		LastValidBlockHeight    int64  `json:"lastValidBlockHeight"`
		BroadcastIntentRecorded bool   `json:"broadcastIntentRecorded"`
	} `json:"operation"`
}

func loadRestartJournal(t *testing.T) restartJournalFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "backyard", "signed_unsent_restart_operation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var journal restartJournalFixture
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	wire, err := base64.StdEncoding.DecodeString(journal.Operation.SignedWireBase64)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(wire)
	if hex.EncodeToString(digest[:]) != journal.Operation.SignedWireSHA256 {
		t.Fatalf("restart journal wire hash is incoherent: %s", journal.Operation.SignedWireSHA256)
	}
	// Drain-and-swap: the Go engine never adopts the Render-era owner dialect.
	if !ValidLeaseOwner(journal.LeaseOwner) || ValidLeaseOwner(journal.LegacyOwner) {
		t.Fatalf("restart journal owner identities drifted: %q %q", journal.LeaseOwner, journal.LegacyOwner)
	}
	if journal.RouteKey != productionRouteKey {
		t.Fatalf("restart journal left the fixed production route: %s", journal.RouteKey)
	}
	return journal
}

func journalOperation(journal restartJournalFixture, status OperationStatus) PersistedOperation {
	return PersistedOperation{
		Operation: Operation{ID: journal.Operation.OperationID, RouteKey: journal.RouteKey},
		Status:    status,
	}
}

// runtimeEvents records engine boundary crossings so tests can assert
// financial ordering, such as recovery strictly preceding fresh decisions.
type runtimeEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *runtimeEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

func (e *runtimeEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

func TestRestartJournalRecoveryPrecedesFreshDecisions(t *testing.T) {
	journal := loadRestartJournal(t)
	manifest := readyWorkerManifest(t)

	// Phase 1: the restarted process finds the signed operation and its send is
	// ambiguous. The ambiguous send must stop the tick without observing or
	// journaling any fresh decision.
	ambiguous := &runtimeEvents{}
	phase1 := &Worker{routeKey: journal.RouteKey, interval: time.Millisecond, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			ambiguous.add("load")
			operation := journalOperation(journal, Signed)
			return &operation, nil
		},
		advance: func(context.Context, PersistedOperation) error {
			ambiguous.add("recover-signed")
			return errors.New("ambiguous send after durable broadcast intent: connection reset")
		},
		observe: func(context.Context) (Observation, error) {
			ambiguous.add("observe")
			return Observation{}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			ambiguous.add("journal")
			return DecisionRecord{}, nil
		},
	}}
	err := phase1.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ambiguous send after durable broadcast intent") {
		t.Fatalf("ambiguous signed send did not stop the tick: %v", err)
	}
	if got := strings.Join(ambiguous.snapshot(), ","); got != "load,recover-signed" {
		t.Fatalf("restart with ambiguous send touched fresh decisions: %v", got)
	}

	// Phase 2: the next restart finds the same operation with its durable
	// broadcast intent. Recovery advances it, the journal clears, and only then
	// is a fresh observation permitted on the following tick.
	recovered := &runtimeEvents{}
	loads := 0
	phase2 := &Worker{routeKey: journal.RouteKey, interval: time.Millisecond, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			loads++
			recovered.add("load")
			if loads == 1 {
				operation := journalOperation(journal, BroadcastIntent)
				return &operation, nil
			}
			return nil, nil
		},
		advance: func(context.Context, PersistedOperation) error {
			recovered.add("recover-intent")
			return nil
		},
		observe: func(context.Context) (Observation, error) {
			recovered.add("observe")
			return tickObservation(Snapshot{ObservationID: "post-recovery", Slot: 11, RouteKind: RouteKind, Fresh: true}), nil
		},
		recordDecision: func(_ context.Context, _ string, _ Observation, decision Decision, _ string) (DecisionRecord, error) {
			recovered.add("journal")
			if decision.Action != Hold {
				t.Fatalf("post-recovery tick journaled an executable decision: %+v", decision)
			}
			return DecisionRecord{Status: Held}, nil
		},
	}}
	if err := phase2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := phase2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(recovered.snapshot(), ","); got != "load,recover-intent,load,observe,journal" {
		t.Fatalf("fresh decision preceded or replaced intent recovery: %s", got)
	}
}

func TestNewEngineFailsClosedWithoutInjectedDependencies(t *testing.T) {
	commit := strings.Repeat("a", 40)
	validOwner := "worker:backyard:backyard-eu-1:sha-" + commit
	unpinned := ed25519.NewKeyFromSeed([]byte(bytes32(1)))
	base := EngineConfig{
		Database:    &Database{pool: &pgxpool.Pool{}},
		RPC:         &chain.Client{},
		View:        &View{},
		Credentials: Credentials{PolicyKey: unpinned},
		Config:      DefaultConfig(),
		Owner:       validOwner,
	}
	// A complete injected runtime still requires the production-pinned
	// delegated executor capability; an offline test cannot own one, so the
	// only constructible EngineConfig is one that fails the capability pin.
	if _, err := NewEngine(base); err == nil || !strings.Contains(err.Error(), "pinned delegated executor") {
		t.Fatalf("injected runtime accepted an unpinned signing capability: %v", err)
	}
	// Each mutation names the check that must reject it, so no case passes
	// merely because the offline capability is unpinned.
	for name, tc := range map[string]struct {
		mutate func(*EngineConfig)
		want   string
	}{
		"missing database":      {func(c *EngineConfig) { c.Database = nil }, ""},
		"missing rpc":           {func(c *EngineConfig) { c.RPC = nil }, ""},
		"missing view":          {func(c *EngineConfig) { c.View = nil }, ""},
		"missing credentials":   {func(c *EngineConfig) { c.Credentials = Credentials{} }, ""},
		"truncated capability":  {func(c *EngineConfig) { c.Credentials = Credentials{PolicyKey: unpinned[:ed25519.SeedSize]} }, ""},
		"selector without feed": {func(c *EngineConfig) { c.Selector = SelectorLive }, "Timescale economic feed"},
		"unknown selector":      {func(c *EngineConfig) { c.Selector = "both" }, "unknown Backyard selector mode"},
		"caller lease owner":    {func(c *EngineConfig) { c.Owner = "developer-laptop" }, "platform-neutral lease owner"},
		"retail lease owner":    {func(c *EngineConfig) { c.Owner = "worker:retail:backyard-eu-1:sha-" + commit }, "platform-neutral lease owner"},
		"invalid lease config":  {func(c *EngineConfig) { c.Config = Config{PollInterval: 0} }, ""},
	} {
		config := base
		tc.mutate(&config)
		_, err := NewEngine(config)
		if err == nil || tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s did not fail closed on its own check: %v", name, err)
		}
	}
	if _, err := NewEngine(EngineConfig{}); err == nil {
		t.Fatal("NewEngine accepted an uninjected runtime")
	}
}

func bytes32(value byte) []byte {
	return append([]byte(nil), repeat(value, 32)...)
}

func repeat(value byte, count int) []byte {
	out := make([]byte, count)
	for index := range out {
		out[index] = value
	}
	return out
}

// directEngine mirrors how worker tests construct the concrete controller: the
// production constructor requires the pinned delegated executor secret, which
// offline tests deliberately never hold.
func directEngine(t *testing.T, leasing *fakeRouteLeaseRuntime, owner string) *Engine {
	t.Helper()
	manifest := readyWorkerManifest(t)
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, manifest: manifest}
	return &Engine{worker: worker, leases: leasing, owner: owner, out: io.Discard, config: Config{
		PollInterval: time.Millisecond, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond,
	}}
}

func holdTickRuntime(events *runtimeEvents, cancel context.CancelFunc) tickRuntime {
	return tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			events.add("tick")
			cancel()
			return nil, nil
		},
		observe: func(context.Context) (Observation, error) {
			events.add("observe")
			return tickObservation(Snapshot{ObservationID: "engine-hold", Slot: 10, RouteKind: RouteKind, Fresh: true}), nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			events.add("journal")
			return DecisionRecord{Status: Held}, nil
		},
	}
}

func TestEngineRunsInjectedRuntimeAndJoinsOnCancellation(t *testing.T) {
	journal := loadRestartJournal(t)
	leasing := &fakeRouteLeaseRuntime{}
	out := &strings.Builder{}
	ctx, cancel := context.WithCancel(context.Background())
	events := &runtimeEvents{}
	engine := directEngine(t, leasing, journal.LeaseOwner)
	engine.out = out
	engine.worker.runtime = holdTickRuntime(events, cancel)
	if err := engine.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("engine did not join on cancellation: %v", err)
	}
	banner := out.String()
	if !strings.Contains(banner, "lease_owner="+journal.LeaseOwner) || !strings.Contains(banner, "route="+productionRouteKey) {
		t.Fatalf("startup banner lost the injected identity: %q", banner)
	}
	leaseEvents := leasing.snapshotEvents()
	if len(leaseEvents) != 2 || !strings.HasPrefix(leaseEvents[0], "acquire:") || leaseEvents[1] != "release" {
		t.Fatalf("engine lease lifecycle drifted: %v", leaseEvents)
	}
	if got := strings.Join(events.snapshot(), ","); got != "tick,observe,journal" {
		t.Fatalf("engine tick sequence drifted: %s", got)
	}
}

func TestEngineRestartsKeepOneOwnerDialectForTheSameFence(t *testing.T) {
	journal := loadRestartJournal(t)
	leasing := &fakeRouteLeaseRuntime{}
	events := &runtimeEvents{}
	engine := directEngine(t, leasing, journal.LeaseOwner)
	for generation := 0; generation < 2; generation++ {
		ctx, cancel := context.WithCancel(context.Background())
		engine.worker.runtime = holdTickRuntime(events, cancel)
		if err := engine.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("restart generation %d did not join: %v", generation, err)
		}
	}
	leaseEvents := leasing.snapshotEvents()
	if len(leaseEvents) != 4 {
		t.Fatalf("restart lifecycle drifted: %v", leaseEvents)
	}
	firstOwner := strings.TrimPrefix(leaseEvents[0], "acquire:"+productionRouteKey+":")
	secondOwner := strings.TrimPrefix(leaseEvents[2], "acquire:"+productionRouteKey+":")
	if firstOwner != journal.LeaseOwner || secondOwner != journal.LeaseOwner {
		t.Fatalf("restart forked the owner identity: %q then %q", firstOwner, secondOwner)
	}
	if leaseEvents[1] != "release" || leaseEvents[3] != "release" {
		t.Fatalf("a restart reacquired before releasing the previous fence: %v", leaseEvents)
	}
}

func TestLeaseLossCancelsInFlightEngineWork(t *testing.T) {
	owner := "worker:backyard:backyard-lease-loss:sha-" + strings.Repeat("b", 40)
	leasing := &fakeRouteLeaseRuntime{refreshErr: ErrRouteLeaseLost, refreshCalls: make(chan struct{}, 1)}
	engine := directEngine(t, leasing, owner)
	engine.worker.runtime = tickRuntime{
		loadNonterminal: func(ctx context.Context, _ string) (*PersistedOperation, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	if err := engine.Run(context.Background()); !errors.Is(err, ErrRouteLeaseLost) {
		t.Fatalf("lease loss did not stop the engine: %v", err)
	}
	select {
	case <-leasing.refreshCalls:
	default:
		t.Fatal("lease was never refreshed before the loss")
	}
}
