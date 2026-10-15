package backyard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func readyWorkerManifest(t *testing.T) RouteManifest {
	t.Helper()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// tickObservation is an observation of snapshot that found every literal
// installed where today's Settings holds it.
// tickObservation is the observation the production observer returns for
// snapshot: its view always names a registry lane (Maple when the literal
// names none), and its target is computed, a flat vault's Voltr idle entering
// that lane under the selector's current quote.
func tickObservation(snapshot Snapshot) Observation {
	if snapshot.RouteLane == "" {
		snapshot.RouteLane, snapshot.StrategyKey = SelectedRouteID, SelectedRouteID
	}
	if snapshot.Target.Rule == "" {
		var in targetInputs
		if !snapshot.HasPosition && snapshot.VoltrIdleRaw > 0 && snapshot.WithdrawalDemandRaw == 0 {
			for _, limit := range []*int64{&snapshot.CapacityRaw, &snapshot.PolicyLimitRaw, &snapshot.MaxTargetLTVEntryRaw} {
				if *limit == 0 {
					*limit = snapshot.VoltrIdleRaw
				}
			}
			q := entryQuoteFixture(time.Unix(1, 0).UTC(), snapshot.RouteLane, min(snapshot.VoltrIdleRaw, snapshot.CapacityRaw, snapshot.PolicyLimitRaw, snapshot.MaxTargetLTVEntryRaw))
			q.SampleSlot, q.ValidThroughSlot = snapshot.Slot, snapshot.Slot
			in.Entry = &SelectorResult{Action: "ENTER", DestinationLane: snapshot.RouteLane, SelectedQuote: &q}
		}
		snapshot = targetedWith(snapshot, in)
	}
	return Observation{Snapshot: snapshot, ObservedAt: time.Unix(1, 0).UTC(), policies: capturedTestPolicies()}
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
		prepareBridge: func(_ context.Context, _ RouteManifest, got Decision, _ Observation) (Observation, BridgeExecutionEvidence, error) {
			order = append(order, "prepare")
			if got != decision {
				t.Fatalf("prepared wrong decision: %+v", got)
			}
			return observation, BridgeExecutionEvidence{Request: BridgeBuildRequest{Action: got.Action}}, nil
		},
		recordDecision: func(_ context.Context, route string, _ Observation, got Decision, _ string) (DecisionRecord, error) {
			order = append(order, "record")
			if route != productionRouteKey || got != decision {
				t.Fatalf("recorded route or decision drifted")
			}
			return DecisionRecord{OperationID: "operation", Status: Decided}, nil
		},
		build: func(_ context.Context, operationID string, request any, _ ExpectedEffects) error {
			order = append(order, "build")
			if r, ok := request.(BridgeBuildRequest); operationID != "operation" || !ok || r.Action != VoltrAllocateToSquads {
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
	// A typed build refusal stays a journaled stop, never generic restart
	// recovery or a successful build.
	rejected := &BudgetHold{Reason: holdChainRefused}
	worker.runtime.build = func(context.Context, string, any, ExpectedEffects) error { return rejected }
	journaled := false
	worker.runtime.recordBudgetHold = func(_ context.Context, id string, hold *BudgetHold) error {
		if id != "operation" || hold != rejected {
			t.Fatal("budget HOLD lost its operation or type")
		}
		journaled = true
		return nil
	}
	if err := worker.Tick(context.Background()); !errors.Is(err, rejected) || !journaled {
		t.Fatalf("budget rejection did not remain a journaled stop: %v", err)
	}
}

// A build hold taken on an endpoint that could not answer is not journaled:
// the decided operation stays decided, the run loop waits, and the next tick
// builds it.
func TestTickBuildOnUnavailableRPCLeavesTheOperationDecided(t *testing.T) {
	manifest := readyWorkerManifest(t)
	observation := tickObservation(Snapshot{ObservationID: "allocate", Slot: 10, RouteKind: RouteKind, Fresh: true, VoltrIdleRaw: 7})
	builds, journaled := 0, false
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return observation, nil },
		prepareBridge: func(_ context.Context, _ RouteManifest, d Decision, _ Observation) (Observation, BridgeExecutionEvidence, error) {
			return observation, BridgeExecutionEvidence{Request: BridgeBuildRequest{Action: d.Action}}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			return DecisionRecord{OperationID: "operation", Status: Decided}, nil
		},
		build: func(context.Context, string, any, ExpectedEffects) error {
			if builds++; builds == 1 {
				return budgetHoldFrom(fmt.Errorf("simulateTransaction: %w", chain.ErrBehind), "deposit_projection_unavailable")
			}
			return nil
		},
		recordBudgetHold: func(context.Context, string, *BudgetHold) error { journaled = true; return nil },
	}}
	if err := worker.Tick(context.Background()); !errors.Is(err, errConfirmedObservationUnavailable) || journaled {
		t.Fatalf("unavailable RPC failed the decided operation: journaled=%v %v", journaled, err)
	}
	if err := worker.Tick(context.Background()); err != nil || builds != 2 || journaled {
		t.Fatalf("next tick did not build: builds=%d journaled=%v %v", builds, journaled, err)
	}
	// A hold on anything else is still journaled.
	builds = 0
	worker.runtime.build = func(context.Context, string, any, ExpectedEffects) error {
		return budgetHoldFrom(errors.New("bad layout"), "execution_mint_layout_mismatch")
	}
	if err := worker.Tick(context.Background()); err == nil || !journaled {
		t.Fatalf("a real hold was not journaled: %v", err)
	}
}

func TestTickHoldsUtilizationWithoutPreparingBorrow(t *testing.T) {
	manifest := readyWorkerManifest(t)
	observation := tickObservation(Snapshot{
		ObservationID: "utilization-blocked", Slot: 10, RouteKind: RouteKind, Fresh: true,
		HasPosition: true, PositionCollateralRaw: 99_000_000, PositionCollateralValueRaw: 99_000_000, BorrowUtilizationBlocked: true,
		LiquidationThresholdBPS: 8000, MaxLTVBPS: 8000, LeverageTargetLevel: 1.5, LeverageUpApproved: true,
		CapacityRaw: 99_000_000, PolicyLimitRaw: 99_000_000,
	})
	recorded := false
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return observation, nil },
		prepareKamino: func(context.Context, RouteManifest, Decision, Observation) (Observation, KaminoExecutionEvidence, error) {
			t.Fatal("utilization-blocked reserve attempted to prepare another borrow")
			return Observation{}, KaminoExecutionEvidence{}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			recorded = true
			return DecisionRecord{}, nil
		},
	}}
	if err := worker.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorded || worker.holds.reason != holdNoRoom {
		t.Fatalf("utilization hold wrote a row or lost its reason: recorded=%v hold=%q", recorded, worker.holds.reason)
	}
}

func TestTickDispatchesKaminoAndReobservesAfterReconciliation(t *testing.T) {
	manifest := readyWorkerManifest(t)
	openObservation := tickObservation(Snapshot{
		ObservationID: "open", Slot: 10, RouteKind: RouteKind, Fresh: true,
		CollateralIdleRaw: 5_000, MinimumCollateralDepositRaw: 1, CapacityRaw: 5_000, PolicyLimitRaw: 5_000,
		MaxTargetLTVEntryRaw: 5_000, LiquidationThresholdBPS: 8000, MaxLTVBPS: 8000,
	})
	order := []string{}
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return openObservation, nil },
		prepareKamino: func(_ context.Context, _ RouteManifest, decision Decision, _ Observation) (Observation, KaminoExecutionEvidence, error) {
			order = append(order, "prepare-kamino")
			if decision.Action != OpenRouteStep || decision.Reason != entryDepositReason {
				t.Fatalf("wrong Kamino action: %s", decision.Action)
			}
			return openObservation, KaminoExecutionEvidence{Request: KaminoPrimeUSDCRequest{Action: decision.Action}}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			order = append(order, "record")
			return DecisionRecord{OperationID: "kamino", Status: Decided}, nil
		},
		build: func(_ context.Context, id string, request any, _ ExpectedEffects) error {
			order = append(order, "build-kamino")
			if r, ok := request.(KaminoPrimeUSDCRequest); id != "kamino" || !ok || r.Action != OpenRouteStep {
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
}

func TestNewWorkerRejectsMissingSigningCapability(t *testing.T) {
	if _, err := NewWorker(&Database{pool: &pgxpool.Pool{}}, &chain.Client{}, &View{}, nil, DefaultConfig(), Credentials{}, rwa, &vaultState{}); err == nil {
		t.Fatal("worker accepted a missing signing capability")
	}
}

func TestWorkerRetriesObservation(t *testing.T) {
	t.Parallel()
	manifest := readyWorkerManifest(t)
	ctx, cancel := context.WithCancel(context.Background())
	observations := 0
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe: func(context.Context) (Observation, error) {
			observations++
			if observations == 1 {
				return Observation{}, fmt.Errorf("%w: minimum context slot has not been reached", errConfirmedObservationUnavailable)
			}
			cancel()
			return tickObservation(Snapshot{ObservationID: "retry-hold", Slot: 10, RouteKind: RouteKind, Fresh: true}), nil
		},
	}}
	err := worker.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected clean cancellation, got %v", err)
	}
	if observations != 2 {
		t.Fatalf("observation attempts=%d", observations)
	}
}

func TestWorkerRetriesPreparationBeforeRecordingOrBuilding(t *testing.T) {
	t.Parallel()
	manifest := readyWorkerManifest(t)
	ctx, cancel := context.WithCancel(context.Background())
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
		prepareBridge: func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error) {
			preparations++
			if preparations == 1 {
				return Observation{}, BridgeExecutionEvidence{}, confirmedObservationUnavailable(errors.New("confirmed reads advanced"))
			}
			return tickObservation(actionable), BridgeExecutionEvidence{}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			records++
			return DecisionRecord{Status: Decided, OperationID: "operation"}, nil
		},
		build: func(context.Context, string, any, ExpectedEffects) error {
			builds++
			cancel()
			return nil
		},
	}}
	err := worker.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected clean cancellation, got %v", err)
	}
	if observations != 2 || preparations != 2 || records != 1 || builds != 1 {
		t.Fatalf("observations=%d preparations=%d records=%d builds=%d", observations, preparations, records, builds)
	}
}

func TestWorkerDoesNotRetryDatabaseObservationFailure(t *testing.T) {
	t.Parallel()
	databaseErr := errors.New("position snapshot constraint failed")
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return Observation{}, databaseErr },
	}}
	err := worker.Run(context.Background())
	if !errors.Is(err, databaseErr) {
		t.Fatalf("database observation failure was hidden: %v", err)
	}
}

func TestTickAdvancesOnlyItsDurablySignedWireWithoutPollDelay(t *testing.T) {
	for _, after := range []OperationStatus{Signed, BroadcastIntent} {
		t.Run(string(after), func(t *testing.T) {
			o := tickObservation(Snapshot{ObservationID: "report", Slot: 10, RouteKind: RouteKind, Fresh: true, LastReportAgeSeconds: 3600})
			d := Decide(o.Snapshot)
			if d.Action != ReportNAV {
				t.Fatalf("expected report fixture: %+v", d)
			}
			var persisted *PersistedOperation
			advanced := 0
			w := &Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
				loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return persisted, nil },
				observe:         func(context.Context) (Observation, error) { return o, nil },
				prepareBridge: func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error) {
					return o, BridgeExecutionEvidence{}, nil
				},
				recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
					return DecisionRecord{OperationID: "report-op", Status: Decided}, nil
				},
				build: func(context.Context, string, any, ExpectedEffects) error {
					persisted = &PersistedOperation{Operation: Operation{ID: "report-op"}, Status: after, SignedWire: []byte{1, 2, 3}}
					return nil
				},
				advance: func(_ context.Context, op PersistedOperation) error {
					if op.ID != "report-op" || string(op.SignedWire) != string([]byte{1, 2, 3}) {
						t.Fatal("lost durable wire")
					}
					if op.Status == Signed {
						advanced++
						persisted.Status = Submitted
					}
					return nil
				},
			}}
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 0
			if after == Signed {
				want = 1
			}
			if advanced != want {
				t.Fatalf("advanced %d times, want %d", advanced, want)
			}
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if advanced != want {
				t.Fatal("recovery tried a second signed send")
			}
		})
	}
}
