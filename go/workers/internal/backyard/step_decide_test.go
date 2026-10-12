package backyard

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Every leverage move decides one atomic step: hard LTV, a drain, the down
// moves to 1x and 1.5x and a partial withdrawal's release go down; leverage
// up goes up. Each down step's repay is sized from the decided snapshot.
func TestLeverageMovesDecideAtomicSteps(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{autoAUTOPYUSD.Lane, autoAUTOWYLDSLane, onreONycUSDC} {
		hard := leverageExitSnapshot(lane)
		hard.LTVBPS = 6000
		drain := leverageExitSnapshot(lane)
		drain.Unwind = true
		down := leverageExitSnapshot(lane)
		down.LeverageTargetLevel = 1
		partialDown := leverageSnapshot(1.75)
		partialDown.RouteLane, partialDown.StrategyKey, partialDown.LeverageTargetLevel = lane, lane, 1.5
		withdrawal := livePartialSnapshot()
		withdrawal.RouteLane, withdrawal.StrategyKey = lane, lane
		for _, c := range []struct {
			s      Snapshot
			reason string
			full   bool
		}{
			{hard, hardLTVDeleverReason, false},
			{drain, withdrawalDeleverReason, true},
			{down, leverageDownReason, true},
			{partialDown, leverageDownPartialReason, false},
			{withdrawal, partialReleaseReason, false},
		} {
			got := Decide(c.s)
			if got.Action != DeleverRouteStep || got.Reason != c.reason || got.StrategyKey != lane {
				t.Fatalf("%s %s: decided %+v", lane, c.reason, got)
			}
			if up, ok := stepDecision(c.s, got); !ok || up {
				t.Fatalf("%s %s: not a down step", lane, c.reason)
			}
			if c.reason == partialReleaseReason {
				continue
			}
			repay, full := stepDownRepay(c.s, c.reason)
			if full != c.full || repay == 0 || repay > uint64(max(c.s.PositionDebtRaw, c.s.PayoffDebtRaw)) {
				t.Fatalf("%s %s: repay %d full %t", lane, c.reason, repay, full)
			}
			if full && repay != uint64(max(c.s.PositionDebtRaw, c.s.PayoffDebtRaw)) {
				t.Fatalf("%s %s: a full step repays the payoff bound, not %d", lane, c.reason, repay)
			}
		}
	}
}

// A partial down step lands at its target LTV: repaying r of debt with r of
// collateral value leaves (D-r)/(C-r) at the target, within a raw unit.
func TestStepDebtToLTVLandsOnTheTarget(t *testing.T) {
	t.Parallel()
	s := leverageSnapshot(1.75)
	for _, target := range []int64{hardLTVTargetBPS(s), leverageLevelLTVBPS(1.5), 2000} {
		r := stepDebtToLTVRaw(s, target)
		if r <= 0 || r >= s.PositionDebtRaw {
			t.Fatalf("target %d: repay %d of %d", target, r, s.PositionDebtRaw)
		}
		after := (s.PositionDebtValueRaw - r) * 10_000 / (s.PositionCollateralValueRaw - r)
		if after > target || after < target-1 {
			t.Fatalf("target %d: landed at %d", target, after)
		}
	}
	if r := stepDebtToLTVRaw(s, 9_000); r != 0 {
		t.Fatalf("a target above the current LTV repaid %d", r)
	}
}

// The worker carries a step decision through prepareStep, record, bind (the
// StepRequest) and buildStep, never through the single-leg Kamino path.
func TestWorkerDispatchesAtomicStep(t *testing.T) {
	s := leverageExitSnapshot(autoAUTOPYUSD.Lane)
	s.LTVBPS = 6000
	o := tickObservation(s)
	want := Decide(s)
	if want.Reason != hardLTVDeleverReason {
		t.Fatal(want)
	}
	request := StepRequest{Open: KaminoPrimeUSDCRequest{RouteLane: s.RouteLane}, Close: KaminoPrimeUSDCRequest{RouteLane: s.RouteLane}}
	var order []string
	worker := &Worker{routeKey: productionRouteKey, manifest: readyWorkerManifest(t), runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return o, nil },
		prepareKamino: func(context.Context, RouteManifest, Decision, Observation) (Observation, KaminoExecutionEvidence, error) {
			t.Fatal("a step reached the single-leg Kamino path")
			return Observation{}, KaminoExecutionEvidence{}, nil
		},
		prepareStep: func(_ context.Context, _ RouteManifest, d Decision, _ Observation) (Observation, StepExecutionEvidence, error) {
			if d != want {
				t.Fatal("step identity changed")
			}
			order = append(order, "prepare")
			return o, StepExecutionEvidence{Request: request}, nil
		},
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			order = append(order, "record")
			return DecisionRecord{OperationID: "local-step", Status: Decided}, nil
		},
		bind: func(_ context.Context, id string, _ Observation, d Decision, r any, _ ExpectedEffects) error {
			if id != "local-step" || d != want || !reflect.DeepEqual(r, request) {
				t.Fatalf("bound %T", r)
			}
			order = append(order, "admit")
			return nil
		},
		buildStep: func(_ context.Context, id string, e StepExecutionEvidence) error {
			if id != "local-step" || !reflect.DeepEqual(e.Request, request) {
				t.Fatal("build drift")
			}
			order = append(order, "build")
			return nil
		},
	}}
	if err := worker.Tick(context.Background()); err != nil || strings.Join(order, ",") != "prepare,record,admit,build" {
		t.Fatal(order, err)
	}
}
