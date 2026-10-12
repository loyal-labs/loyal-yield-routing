package backyard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func initializationPlanningFixture(lane string) Observation {
	s := base()
	s.ObservationID = "initializer-planning"
	s.Slot = 42
	s.RouteLane = lane
	s.StrategyKey = lane
	s.ObligationPresenceKnown = true
	s.VoltrIdleRaw = 1_000_000
	s.Target = LaneTarget{Rule: ruleEntry, EntryRaw: 1_000_000}
	s.CapacityRaw = 1_000_000
	s.PolicyLimitRaw = 1_000_000
	s.MaxTargetLTVEntryRaw = 1_000_000
	return tickObservation(s)
}
func TestInitializationDecisionPreservesRecoveryWithdrawalAndEntryGuards(t *testing.T) {
	t.Parallel()
	for _, lane := range basicLaneIDs() {
		o := initializationPlanningFixture(lane)
		d := Decide(o.Snapshot)
		if d.Action != InitializeKaminoObligation || d.Validate() != nil {
			t.Fatalf("initializer not selected for %s: %+v", lane, d)
		}
		for _, mutate := range []func(*Snapshot){
			func(s *Snapshot) { s.ObligationPresenceKnown = false }, func(s *Snapshot) { s.ObligationPresent = true },
			func(s *Snapshot) { s.Nonterminal = Signed }, func(s *Snapshot) { s.WithdrawalDemandRaw = 1 }, func(s *Snapshot) { s.Target = LaneTarget{Rule: ruleHold} }, func(s *Snapshot) { s.Target.EntryRaw = 0 },
			func(s *Snapshot) { s.SquadsIdleRaw = 1 }, func(s *Snapshot) { s.CollateralIdleRaw = 1 }, func(s *Snapshot) { s.PositionDebtRaw = 1 }, func(s *Snapshot) { s.CapacityRaw = 0 }, func(s *Snapshot) { s.LiquidationThresholdBPS = TargetLTVBPS + 1500 },
		} {
			s := o.Snapshot
			mutate(&s)
			if got := Decide(s); got.Action == InitializeKaminoObligation {
				t.Fatalf("initializer bypassed prior work or prerequisites: %+v", s)
			}
		}
	}
}

func initializationRuntimeRPC(t *testing.T) (*chain.Client, RouteManifest, KaminoInitializationRequest, map[string]ConfirmedAccount) {
	t.Helper()
	r, accounts := initializationPrestateFixture(t)
	m := embeddedTestManifest(t)
	rpc := budgetBuildRPC(t, 5000, 42)
	base := rpcOf(rpc).Transport
	rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Method string
			Params []json.RawMessage
			ID     any
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		var result any
		switch body.Method {
		case "getMinimumBalanceForRentExemption":
			result = r.RentLamports
		case "getLatestBlockhash":
			result = map[string]any{"context": map[string]any{"slot": 42}, "value": map[string]any{"blockhash": r.RecentBlockhash, "lastValidBlockHeight": r.LastValidBlockHeight}}
		default:
			return base.RoundTrip(req)
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(out)), Header: make(http.Header)}, nil
	})
	return rpc, m, r, accounts
}

// initializationView holds the initializer's accounts beside the valuation
// accounts.
func initializationView(t *testing.T, accounts map[string]ConfirmedAccount) *View {
	t.Helper()
	return fixtureView(t, budgetBuildRPCWithAccounts(t, 5000, 42, slices.Collect(maps.Values(accounts))))
}

func TestInitializerPreparationMeasuresNativeFunding(t *testing.T) {
	t.Parallel()
	rpc, m, template, _ := initializationRuntimeRPC(t)
	o := initializationPlanningFixture(template.RouteLane)
	o.policies = testPolicies(t)
	d := Decide(o.Snapshot)
	got, r, err := prepareKaminoInitialization(context.Background(), rpc, m, d, o)
	if err != nil || !reflect.DeepEqual(got.Snapshot, o.Snapshot) || r.RentLamports != template.RentLamports || r.MaximumFeeLamports != 5000 {
		t.Fatalf("unmeasured initialization: %+v %v", r, err)
	}
}

func TestWorkerDispatchesInitializationAfterTheRecord(t *testing.T) {
	o := initializationPlanningFixture(SelectedRouteID)
	d := Decide(o.Snapshot)
	m := readyWorkerManifest(t)
	var order []string
	w := &Worker{routeKey: productionRouteKey, manifest: m, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil }, observe: func(context.Context) (Observation, error) { return o, nil },
		prepareInitialization: func(context.Context, RouteManifest, Decision, Observation) (Observation, KaminoInitializationRequest, error) {
			order = append(order, "prepare")
			return o, KaminoInitializationRequest{RouteLane: SelectedRouteID}, nil
		},
		recordDecision: func(_ context.Context, _ string, _ Observation, got Decision, _ string) (DecisionRecord, error) {
			if got != d {
				t.Fatal("decision changed")
			}
			order = append(order, "record")
			return DecisionRecord{OperationID: "init", Status: Decided}, nil
		},
		build: func(_ context.Context, _ string, request any, effects ExpectedEffects) error {
			if _, ok := request.(KaminoInitializationRequest); !ok || effects.Initialization == nil {
				t.Fatal("build received another request")
			}
			order = append(order, "build")
			return nil
		},
	}}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"prepare", "record", "build"}) {
		t.Fatal(order)
	}
	w.runtime.build = func(context.Context, string, any, ExpectedEffects) error {
		return budgetHold("initializer_native_funding_unavailable")
	}
	journaled := false
	w.runtime.recordBudgetHold = func(context.Context, string, *BudgetHold) error { journaled = true; return nil }
	assertBudgetHold(t, w.Tick(context.Background()), "initializer_native_funding_unavailable")
	if !journaled {
		t.Fatal("build hold not recorded")
	}
}

// A signed AUTO initializer must reload after a restart (live 2026-09-24 the
// then-embedded validator refused it); a lane without an initializer policy
// never restores an initializer decision.
func TestNonterminalAutoInitializerRestores(t *testing.T) {
	t.Parallel()
	d := Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: autoAUTOPYUSD.Lane, IdempotencyKey: "obs:initialize:AUTO/AUTO/PYUSD"}
	effects := []byte(`{"decision":{"reason":"multiply_obligation_missing","amountRaw":0,"strategyKey":"AUTO/AUTO/PYUSD"}}`)
	got, err := restorePersistedDecision(effects, d.Action, d.IdempotencyKey, d.StrategyKey)
	if err != nil || got != d {
		t.Fatal("restore refused the AUTO initializer", got, err)
	}
	sibling := d
	sibling.StrategyKey = primePRIMEPYUSD.Lane
	siblingEffects := []byte(`{"decision":{"reason":"multiply_obligation_missing","amountRaw":0,"strategyKey":"Prime/PRIME/PYUSD"}}`)
	if _, err := restorePersistedDecision(siblingEffects, sibling.Action, sibling.IdempotencyKey, sibling.StrategyKey); err == nil {
		t.Fatal("a lane without an initializer policy restored an initializer decision")
	}
}
