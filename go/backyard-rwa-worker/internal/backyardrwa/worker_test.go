package backyardrwa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func readyWorkerManifest(t *testing.T) RouteManifest {
	t.Helper()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.Status = "ready"
	manifest.Unresolved = nil
	manifest.PolicyCatalog.AddressesResolved = true
	packing := int64(8)
	manifest.PolicyCatalog.PackingRung = &packing
	manifest.PolicyCatalog.PolicyAccounts = []string{bridgeAllocationPolicy}
	commit := strings.Repeat("1", 40)
	digest := "sha256:" + strings.Repeat("2", 64)
	service := "loyal-backyard-rwa-worker"
	manifest.Deployment.SourceCommit = &commit
	manifest.Deployment.ImageDigest = &digest
	manifest.Deployment.SingleWriterService = &service
	for index := range manifest.RuntimeBindings.BridgePolicies {
		hash := strings.Repeat(string(rune('a'+index)), 64)
		manifest.RuntimeBindings.BridgePolicies[index].DataSHA256 = &hash
	}
	manifest.RuntimeBindings.PrimeUSDC.Packets = make([]struct {
		Action                  Action                  `json:"action"`
		Policy                  string                  `json:"policy"`
		PolicyAccountDataSHA256 string                  `json:"policyAccountDataSha256"`
		PolicyConstraintIndex   byte                    `json:"policyConstraintIndex"`
		Accounts                KaminoPrimeUSDCAccounts `json:"accounts"`
		DataBase64              string                  `json:"dataBase64"`
	}, 4)
	manifest.RuntimeBindings.PrimeUSDC.SwapPolicies = make([]JupiterPolicyBinding, 2)
	if blocker := manifest.executionBlocker(); blocker != nil {
		t.Fatalf("ready test manifest remained blocked: %v", blocker)
	}
	return manifest
}

func tickObservation(snapshot Snapshot) Observation {
	return Observation{Snapshot: snapshot, ObservedAt: time.Unix(1, 0).UTC()}
}

func TestTickRecordsBeforeBridgeBuildAndDispatchesExactAction(t *testing.T) {
	manifest := readyWorkerManifest(t)
	observation := tickObservation(Snapshot{
		ObservationID: "allocate", Slot: 10, RouteKind: RouteKind, Fresh: true,
		VoltrIdleRaw: 7,
	})
	decision := Decide(observation.Snapshot)
	if decision.Action != VoltrAllocateToSquads {
		t.Fatalf("unexpected test decision: %+v", decision)
	}
	order := []string{}
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return observation, nil },
		prepareBridge: func(_ context.Context, _ RouteManifest, got Decision) (Observation, BridgeExecutionEvidence, error) {
			order = append(order, "prepare")
			if got != decision {
				t.Fatalf("prepared wrong decision: %+v", got)
			}
			return observation, BridgeExecutionEvidence{Request: BridgeBuildRequest{Action: got.Action}}, nil
		},
		recordDecision: func(_ context.Context, route string, _ Observation, got Decision, _, _ string) (DecisionRecord, error) {
			order = append(order, "record")
			if route != productionRouteKey || got != decision {
				t.Fatalf("recorded route or decision drifted")
			}
			return DecisionRecord{OperationID: "operation", Status: Decided}, nil
		},
		buildBridge: func(_ context.Context, operationID string, evidence BridgeExecutionEvidence) error {
			order = append(order, "build")
			if operationID != "operation" || evidence.Request.Action != VoltrAllocateToSquads {
				t.Fatalf("bridge dispatch drifted")
			}
			return nil
		},
	}}
	if err := worker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "prepare,record,build" {
		t.Fatalf("decision was not persisted before build: %s", got)
	}
}

func TestTickPreservesHoldJournalWhileManifestIsBlocked(t *testing.T) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	observation := tickObservation(Snapshot{ObservationID: "hold", Slot: 10, RouteKind: RouteKind, Fresh: true})
	recorded := false
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return observation, nil },
		recordDecision: func(_ context.Context, _ string, _ Observation, decision Decision, _, _ string) (DecisionRecord, error) {
			recorded = true
			if decision.Action != Hold {
				t.Fatalf("expected terminal HOLD, got %s", decision.Action)
			}
			return DecisionRecord{Status: Held}, nil
		},
	}}
	if err := worker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Fatal("blocked manifest discarded the HOLD journal")
	}
}

func TestTickDispatchesKaminoAndReobservesAfterReconciliation(t *testing.T) {
	manifest := readyWorkerManifest(t)
	openObservation := tickObservation(Snapshot{
		ObservationID: "open", Slot: 10, RouteKind: RouteKind, Fresh: true,
		PrimeIdleRaw: 5, CapacityRaw: 5, PolicyLimitRaw: 5,
		MaxTargetLTVEntryRaw: 5, PolicyReady: true, ExitBuildable: true,
		LiquidationThresholdBPS: 8000,
	})
	order := []string{}
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return openObservation, nil },
		prepareKamino: func(_ context.Context, _ RouteManifest, decision Decision) (Observation, KaminoExecutionEvidence, error) {
			order = append(order, "prepare-kamino")
			if decision.Action != OpenPrimeUSDCStep {
				t.Fatalf("wrong Kamino action: %s", decision.Action)
			}
			return openObservation, KaminoExecutionEvidence{Request: KaminoPrimeUSDCRequest{Action: decision.Action}}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
			order = append(order, "record")
			return DecisionRecord{OperationID: "kamino", Status: Decided}, nil
		},
		buildKamino: func(_ context.Context, id string, evidence KaminoExecutionEvidence) error {
			order = append(order, "build-kamino")
			if id != "kamino" || evidence.Request.Action != OpenPrimeUSDCStep {
				t.Fatal("wrong Kamino evidence dispatch")
			}
			return nil
		},
	}}
	if err := worker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "prepare-kamino,record,build-kamino" {
		t.Fatalf("wrong Kamino dispatch order: %s", got)
	}

	loads := 0
	reobserved := false
	worker.runtime = tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			loads++
			if loads == 1 {
				return &PersistedOperation{Operation: Operation{ID: "confirmed"}, Status: Reconciling}, nil
			}
			return nil, nil
		},
		advance: func(context.Context, PersistedOperation) error { return nil },
		observe: func(context.Context) (Observation, error) { reobserved = true; return openObservation, nil },
	}
	if err := worker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reobserved {
		t.Fatal("reconciled confirmed mutation was not immediately reobserved")
	}
}

func TestNewWorkerRejectsRouteOverride(t *testing.T) {
	if _, err := NewWorker(&Database{}, &RPCClient{}, "caller-selected-route", DefaultConfig()); err == nil {
		t.Fatal("worker accepted a route override")
	}
}

func TestExecutionGateKeepsPhaseTwoCatalogOutOfFirstRelease(t *testing.T) {
	manifest := readyWorkerManifest(t)
	manifest.PolicyCatalog.AddressesResolved = false
	manifest.PolicyCatalog.PackingRung = nil
	manifest.PolicyCatalog.PolicyAccounts = nil
	manifest.Deployment.SourceCommit = nil
	manifest.Deployment.ImageDigest = nil
	manifest.Deployment.SingleWriterService = nil
	manifest.Unresolved = append(manifest.Unresolved, struct {
		Code            string `json:"code"`
		ResumeCondition string `json:"resumeCondition"`
	}{Code: "UNRESOLVED_CURRENT_POLICY_GRAPH"})
	if blocker := manifest.executionBlocker(); blocker != nil {
		t.Fatalf("Phase 2 catalog disabled fixed PRIME/USDC: %v", blocker)
	}
	manifest.Unresolved = append(manifest.Unresolved, struct {
		Code            string `json:"code"`
		ResumeCondition string `json:"resumeCondition"`
	}{Code: "UNRESOLVED_PRIME_USDC_PACKETS"})
	if blocker := manifest.executionBlocker(); blocker == nil {
		t.Fatal("Phase 1 packet blocker was ignored")
	}
}

type fakeRouteLeaseRuntime struct {
	mu            sync.Mutex
	events        []string
	acquireErr    error
	acquireErrors []error
	acquireCalls  int
	acquireBlocks bool
	refreshErr    error
	releaseErr    error
	refreshCalls  chan struct{}
}

func (f *fakeRouteLeaseRuntime) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *fakeRouteLeaseRuntime) AcquireRouteLease(ctx context.Context, routeKey, owner string, _ time.Duration) (RouteLease, error) {
	f.record("acquire:" + routeKey + ":" + owner)
	f.mu.Lock()
	f.acquireCalls++
	err := f.acquireErr
	if index := f.acquireCalls - 1; index < len(f.acquireErrors) {
		err = f.acquireErrors[index]
	}
	f.mu.Unlock()
	if f.acquireBlocks {
		<-ctx.Done()
		return RouteLease{}, ctx.Err()
	}
	if err != nil {
		return RouteLease{}, err
	}
	return RouteLease{RouteKey: routeKey, Owner: owner, FencingToken: 1, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *fakeRouteLeaseRuntime) RefreshRouteLease(context.Context, time.Duration) (RouteLease, error) {
	f.record("refresh")
	if f.refreshCalls != nil {
		select {
		case f.refreshCalls <- struct{}{}:
		default:
		}
	}
	if f.refreshErr != nil {
		return RouteLease{}, f.refreshErr
	}
	return RouteLease{FencingToken: 1, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *fakeRouteLeaseRuntime) ReleaseRouteLease(context.Context) (bool, error) {
	f.record("release")
	return f.releaseErr == nil, f.releaseErr
}

func (f *fakeRouteLeaseRuntime) snapshotEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func TestLeasedWorkerAcquiresBeforeTickAndReleasesOnCleanShutdown(t *testing.T) {
	manifest := readyWorkerManifest(t)
	ctx, cancel := context.WithCancel(context.Background())
	leasing := &fakeRouteLeaseRuntime{}
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			leasing.record("tick")
			cancel()
			return nil, nil
		},
		observe: func(context.Context) (Observation, error) {
			return tickObservation(Snapshot{ObservationID: "lease-hold", Slot: 10, RouteKind: RouteKind, Fresh: true}), nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
			return DecisionRecord{Status: Held}, nil
		},
	}}
	config := Config{PollInterval: time.Millisecond, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond}
	err := worker.Run(ctx, leasing, "render:srv-test:sha-"+strings.Repeat("a", 40), config)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected clean cancellation, got %v", err)
	}
	events := leasing.snapshotEvents()
	if len(events) != 3 || !strings.HasPrefix(events[0], "acquire:") || events[1] != "tick" || events[2] != "release" {
		t.Fatalf("wrong lease lifecycle order: %v", events)
	}
}

func TestLeasedWorkerRetriesObservationWithoutDroppingTheFence(t *testing.T) {
	manifest := readyWorkerManifest(t)
	ctx, cancel := context.WithCancel(context.Background())
	leasing := &fakeRouteLeaseRuntime{}
	observations := 0
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe: func(context.Context) (Observation, error) {
			observations++
			if observations == 1 {
				return Observation{}, fmt.Errorf("%w: minimum context slot has not been reached", errConfirmedObservationUnavailable)
			}
			return tickObservation(Snapshot{ObservationID: "retry-hold", Slot: 10, RouteKind: RouteKind, Fresh: true}), nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
			cancel()
			return DecisionRecord{Status: Held}, nil
		},
	}}
	config := Config{PollInterval: time.Millisecond, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond}
	err := worker.Run(ctx, leasing, "render:srv-test:sha-"+strings.Repeat("e", 40), config)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected clean cancellation, got %v", err)
	}
	if observations != 2 {
		t.Fatalf("observation attempts=%d", observations)
	}
	events := leasing.snapshotEvents()
	if len(events) != 2 || !strings.HasPrefix(events[0], "acquire:") || events[1] != "release" {
		t.Fatalf("observation retry dropped or reacquired the route fence: %v", events)
	}
}

func TestLeasedWorkerRetriesPreparationBeforeRecordingOrBuilding(t *testing.T) {
	manifest := readyWorkerManifest(t)
	ctx, cancel := context.WithCancel(context.Background())
	leasing := &fakeRouteLeaseRuntime{}
	observations, preparations, records, builds := 0, 0, 0, 0
	actionable := base()
	actionable.ObservationID = "retry-preparation"
	actionable.VoltrIdleRaw = 10
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe: func(context.Context) (Observation, error) {
			observations++
			return tickObservation(actionable), nil
		},
		prepareBridge: func(context.Context, RouteManifest, Decision) (Observation, BridgeExecutionEvidence, error) {
			preparations++
			if preparations == 1 {
				return Observation{}, BridgeExecutionEvidence{}, confirmedObservationUnavailable(errors.New("confirmed reads advanced"))
			}
			return tickObservation(actionable), BridgeExecutionEvidence{}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
			records++
			return DecisionRecord{Status: Decided, OperationID: "operation"}, nil
		},
		buildBridge: func(context.Context, string, BridgeExecutionEvidence) error {
			builds++
			cancel()
			return nil
		},
	}}
	config := Config{PollInterval: time.Millisecond, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond}
	err := worker.Run(ctx, leasing, "render:srv-test:sha-"+strings.Repeat("f", 40), config)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected clean cancellation, got %v", err)
	}
	if observations != 2 || preparations != 2 || records != 1 || builds != 1 {
		t.Fatalf("observations=%d preparations=%d records=%d builds=%d", observations, preparations, records, builds)
	}
	events := leasing.snapshotEvents()
	if len(events) != 2 || !strings.HasPrefix(events[0], "acquire:") || events[1] != "release" {
		t.Fatalf("preparation retry dropped or reacquired the route fence: %v", events)
	}
}

func TestLeasedWorkerDoesNotRetryDatabaseObservationFailure(t *testing.T) {
	databaseErr := errors.New("position snapshot constraint failed")
	leasing := &fakeRouteLeaseRuntime{}
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return Observation{}, databaseErr },
	}}
	err := worker.Run(context.Background(), leasing, "render:srv-test:sha-"+strings.Repeat("9", 40), Config{
		PollInterval: time.Millisecond, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond,
	})
	if !errors.Is(err, databaseErr) {
		t.Fatalf("database observation failure was hidden: %v", err)
	}
	events := leasing.snapshotEvents()
	if len(events) != 2 || !strings.HasPrefix(events[0], "acquire:") || events[1] != "release" {
		t.Fatalf("database failure did not terminate the leased worker: %v", events)
	}
}

func TestLeasedWorkerFailsClosedWhenRefreshLosesFence(t *testing.T) {
	leasing := &fakeRouteLeaseRuntime{refreshErr: ErrRouteLeaseLost, refreshCalls: make(chan struct{}, 1)}
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, runtime: tickRuntime{
		loadNonterminal: func(ctx context.Context, _ string) (*PersistedOperation, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}}
	config := Config{PollInterval: time.Millisecond, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond}
	err := worker.Run(context.Background(), leasing, "render:srv-test:sha-"+strings.Repeat("b", 40), config)
	if !errors.Is(err, ErrRouteLeaseLost) {
		t.Fatalf("refresh loss did not fence the active tick: %v", err)
	}
	select {
	case <-leasing.refreshCalls:
	default:
		t.Fatal("lease was never refreshed")
	}
	events := leasing.snapshotEvents()
	if strings.Join(events, ",") != "acquire:"+productionRouteKey+":render:srv-test:sha-"+strings.Repeat("b", 40)+",refresh,release" {
		t.Fatalf("unexpected lost-lease lifecycle: %v", events)
	}
}

func TestLeasedWorkerPrefersRefreshFailureOverIdleTimerCancellation(t *testing.T) {
	refreshErr := errors.New("refresh database failure")
	leasing := &fakeRouteLeaseRuntime{refreshErr: refreshErr, refreshCalls: make(chan struct{}, 1)}
	worker := &Worker{routeKey: productionRouteKey, interval: time.Hour, manifest: readyWorkerManifest(t), runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe: func(context.Context) (Observation, error) {
			return tickObservation(Snapshot{ObservationID: "idle-refresh", Slot: 10, RouteKind: RouteKind, Fresh: true}), nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
			return DecisionRecord{Status: Held}, nil
		},
	}}
	err := worker.Run(context.Background(), leasing, "render:srv-test:sha-"+strings.Repeat("a", 40), Config{PollInterval: time.Hour, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond})
	if !errors.Is(err, refreshErr) || errors.Is(err, context.Canceled) {
		t.Fatalf("idle timer race hid refresh error: %v", err)
	}
	select {
	case <-leasing.refreshCalls:
	default:
		t.Fatal("refresh was not attempted")
	}
}

func TestLeasedWorkerDoesNotRunOrReleaseAfterFailedAcquisition(t *testing.T) {
	leasing := &fakeRouteLeaseRuntime{acquireErr: ErrRouteLeaseUnavailable}
	ticked := false
	now := time.Unix(1_700_000_000, 0)
	worker := &Worker{routeKey: productionRouteKey, leaseHandoff: startupLeaseHandoffRuntime{
		now:  func() time.Time { return now },
		wait: func(context.Context, time.Duration) error { now = now.Add(time.Hour); return nil },
	}, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			ticked = true
			return nil, nil
		},
	}}
	err := worker.Run(context.Background(), leasing, "render:srv-test:sha-"+strings.Repeat("c", 40), DefaultConfig())
	if !errors.Is(err, ErrRouteLeaseUnavailable) || ticked {
		t.Fatalf("failed acquisition did not fail closed: err=%v ticked=%v", err, ticked)
	}
	if events := leasing.snapshotEvents(); len(events) != 1 || !strings.HasPrefix(events[0], "acquire:") {
		t.Fatalf("failed acquisition released or ran work: %v", events)
	}
}

func TestLeasedWorkerWaitsForRollingDeployLeaseHandoffBeforeFirstTick(t *testing.T) {
	manifest := readyWorkerManifest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leasing := &fakeRouteLeaseRuntime{acquireErrors: []error{ErrRouteLeaseUnavailable, nil}}
	now := time.Unix(1_700_000_000, 0)
	waits := 0
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, manifest: manifest,
		leaseHandoff: startupLeaseHandoffRuntime{
			now:  func() time.Time { return now },
			wait: func(ctx context.Context, delay time.Duration) error { waits++; now = now.Add(delay); return nil },
		},
		runtime: tickRuntime{
			loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
				leasing.record("tick")
				cancel()
				return nil, nil
			},
			observe: func(context.Context) (Observation, error) {
				return tickObservation(Snapshot{ObservationID: "handoff", Slot: 10, RouteKind: RouteKind, Fresh: true}), nil
			},
			recordDecision: func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
				return DecisionRecord{Status: Held}, nil
			},
		},
	}
	err := worker.Run(ctx, leasing, "render:srv-test:sha-"+strings.Repeat("d", 40), Config{PollInterval: time.Millisecond, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond})
	if !errors.Is(err, context.Canceled) || waits != 1 {
		t.Fatalf("err=%v waits=%d", err, waits)
	}
	events := leasing.snapshotEvents()
	if len(events) != 4 || !strings.HasPrefix(events[0], "acquire:") || !strings.HasPrefix(events[1], "acquire:") || events[2] != "tick" || events[3] != "release" {
		t.Fatalf("handoff ran before acquisition or missed release: %v", events)
	}
}

func TestLeasedWorkerLeaseHandoffTimeoutDoesNotTickOrRelease(t *testing.T) {
	leasing := &fakeRouteLeaseRuntime{acquireErr: ErrRouteLeaseUnavailable}
	now := time.Unix(1_700_000_000, 0)
	ticked := false
	worker := &Worker{routeKey: productionRouteKey,
		leaseHandoff: startupLeaseHandoffRuntime{
			now:  func() time.Time { return now },
			wait: func(ctx context.Context, delay time.Duration) error { now = now.Add(delay); return nil },
		},
		runtime: tickRuntime{loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { ticked = true; return nil, nil }},
	}
	err := worker.Run(context.Background(), leasing, "render:srv-test:sha-"+strings.Repeat("c", 40), Config{PollInterval: time.Millisecond, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond})
	if !errors.Is(err, ErrRouteLeaseUnavailable) || ticked {
		t.Fatalf("timeout did not fail closed: err=%v ticked=%v", err, ticked)
	}
	for _, event := range leasing.snapshotEvents() {
		if event == "release" {
			t.Fatalf("timed out handoff released an unacquired lease: %v", leasing.snapshotEvents())
		}
	}
}

func TestLeasedWorkerLeaseHandoffBoundsBlockingAcquire(t *testing.T) {
	leasing := &fakeRouteLeaseRuntime{acquireBlocks: true}
	ticked := false
	worker := &Worker{routeKey: productionRouteKey, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { ticked = true; return nil, nil },
	}}
	started := time.Now()
	err := worker.Run(context.Background(), leasing, "render:srv-test:sha-"+strings.Repeat("f", 40), Config{PollInterval: time.Millisecond, LeaseTTL: 30 * time.Millisecond, LeaseRefreshInterval: 10 * time.Millisecond})
	if !errors.Is(err, ErrRouteLeaseUnavailable) || ticked {
		t.Fatalf("blocking acquire escaped handoff bound: err=%v ticked=%v", err, ticked)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("blocking acquire exceeded bounded startup window: %s", elapsed)
	}
	if events := leasing.snapshotEvents(); len(events) != 1 || !strings.HasPrefix(events[0], "acquire:") {
		t.Fatalf("blocking handoff retried, released, or ticked: %v", events)
	}
}

func TestLeasedWorkerLeaseHandoffHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leasing := &fakeRouteLeaseRuntime{acquireErr: ErrRouteLeaseUnavailable}
	ticked := false
	worker := &Worker{routeKey: productionRouteKey, leaseHandoff: startupLeaseHandoffRuntime{
		now:  func() time.Time { return time.Unix(1_700_000_000, 0) },
		wait: func(context.Context, time.Duration) error { cancel(); return context.Canceled },
	}, runtime: tickRuntime{loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { ticked = true; return nil, nil }}}
	err := worker.Run(ctx, leasing, "render:srv-test:sha-"+strings.Repeat("e", 40), Config{PollInterval: time.Millisecond, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond})
	if !errors.Is(err, context.Canceled) || ticked {
		t.Fatalf("canceled handoff ticked or hid cancellation: err=%v ticked=%v", err, ticked)
	}
	if events := leasing.snapshotEvents(); len(events) != 1 || !strings.HasPrefix(events[0], "acquire:") {
		t.Fatalf("canceled handoff released or retried unexpectedly: %v", events)
	}
}

func TestLeasedWorkerRejectsCallerSuppliedOwnerOutsideDeploymentConvention(t *testing.T) {
	leasing := &fakeRouteLeaseRuntime{}
	worker := &Worker{routeKey: productionRouteKey}
	if err := worker.Run(context.Background(), leasing, "developer-laptop", DefaultConfig()); err == nil {
		t.Fatal("worker accepted an arbitrary lease owner")
	}
	if events := leasing.snapshotEvents(); len(events) != 0 {
		t.Fatalf("invalid owner reached the database: %v", events)
	}
}

func TestLeasedWorkerSurfacesReleaseFailureOnCleanShutdown(t *testing.T) {
	releaseErr := errors.New("database unavailable during release")
	leasing := &fakeRouteLeaseRuntime{releaseErr: releaseErr}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) {
			cancel()
			return nil, nil
		},
		observe: func(context.Context) (Observation, error) {
			return tickObservation(Snapshot{ObservationID: "release-hold", Slot: 10, RouteKind: RouteKind, Fresh: true}), nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
			return DecisionRecord{Status: Held}, nil
		},
	}}
	err := worker.Run(ctx, leasing, "render:srv-test:sha-"+strings.Repeat("f", 40), Config{
		PollInterval: time.Millisecond, LeaseTTL: 60 * time.Millisecond, LeaseRefreshInterval: 20 * time.Millisecond,
	})
	if !errors.Is(err, releaseErr) {
		t.Fatalf("clean shutdown hid the lease release failure: %v", err)
	}
}

func TestRuntimeLeaseOwnerUsesExactRenderAndImmutableImageIdentity(t *testing.T) {
	commit := strings.Repeat("d", 40)
	config := RuntimeConfig{RenderServiceID: "srv-c123abc", ImageVersion: "sha-" + commit}
	owner, err := config.LeaseOwner()
	if err != nil {
		t.Fatal(err)
	}
	if owner != "render:srv-c123abc:sha-"+commit {
		t.Fatalf("unexpected lease owner: %s", owner)
	}
	for _, invalid := range []RuntimeConfig{
		{RenderServiceID: "loyal-backyard-rwa-worker", ImageVersion: "sha-" + commit},
		{RenderServiceID: "srv-c123abc", ImageVersion: "latest"},
	} {
		if _, err := invalid.LeaseOwner(); err == nil {
			t.Fatalf("accepted invalid deployment identity: %+v", invalid)
		}
	}
}
