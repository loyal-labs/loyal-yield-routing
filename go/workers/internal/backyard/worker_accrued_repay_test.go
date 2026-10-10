package backyard

import (
	"context"
	"errors"
	"testing"
)

func TestTickAcceptsAccruedWholeDebtRepaymentAndRetriesOtherDrift(t *testing.T) {
	manifest := readyWorkerManifest(t)
	decided := liveIdleDebtSnapshot()
	refreshed := decided
	refreshed.ObservationID = "refreshed"
	refreshed.PositionDebtRaw, refreshed.PayoffDebtRaw = decided.PositionDebtRaw+3, decided.PayoffDebtRaw+3
	var recorded Decision
	var wire uint64
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         func(context.Context) (Observation, error) { return tickObservation(decided), nil },
		prepareKamino: func(_ context.Context, _ RouteManifest, d Decision) (Observation, KaminoExecutionEvidence, error) {
			if d.AmountRaw != decided.PositionDebtRaw {
				t.Fatalf("prepared the wrong decision: %+v", d)
			}
			return tickObservation(refreshed), KaminoExecutionEvidence{Request: KaminoPrimeUSDCRequest{Action: DeleverRouteStep, FullPayoff: true, AmountRaw: uint64(refreshed.PayoffDebtRaw)}}, nil
		},
		recordDecision: func(_ context.Context, _ string, _ Observation, d Decision, _ string) (DecisionRecord, error) {
			recorded = d
			return DecisionRecord{OperationID: "repay", Status: Decided}, nil
		},
		bind: func(context.Context, string, Observation, Decision, any, ExpectedEffects) error { return nil },
		buildKamino: func(_ context.Context, _ string, e KaminoExecutionEvidence) error {
			wire = e.Request.AmountRaw
			return nil
		},
	}}
	if err := worker.Tick(context.Background()); err != nil {
		t.Fatalf("accrued interest stopped the worker: %v", err)
	}
	if recorded.Reason != "idle_debt_repay" || recorded.AmountRaw != refreshed.PositionDebtRaw || wire != uint64(refreshed.PayoffDebtRaw) {
		t.Fatalf("repayment not sized on the refreshed debt: %+v wire=%d", recorded, wire)
	}
	// A prepared wire that is not the full payoff of the refreshed debt, or
	// any other drift, retries next tick without recording or exiting.
	for name, prepare := range map[string]func() (Observation, KaminoExecutionEvidence){
		"not full payoff": func() (Observation, KaminoExecutionEvidence) {
			return tickObservation(refreshed), KaminoExecutionEvidence{Request: KaminoPrimeUSDCRequest{Action: DeleverRouteStep}}
		},
		"other drift": func() (Observation, KaminoExecutionEvidence) {
			other := refreshed
			other.LTVBPS = 6000
			other.DebtIdleRaw = 1
			return tickObservation(other), KaminoExecutionEvidence{Request: KaminoPrimeUSDCRequest{Action: DeleverRouteStep, FullPayoff: true}}
		},
	} {
		recorded = Decision{}
		worker.runtime.prepareKamino = func(context.Context, RouteManifest, Decision) (Observation, KaminoExecutionEvidence, error) {
			o, e := prepare()
			return o, e, nil
		}
		err := worker.Tick(context.Background())
		if !errors.Is(err, errConfirmedObservationUnavailable) || recorded.Reason != "" {
			t.Fatalf("%s: drift was not a retry: %v %+v", name, err, recorded)
		}
	}
}

func TestFullDebtRepaymentRefreshAcceptsOnlyGrowingWholeDebt(t *testing.T) {
	s := liveIdleDebtSnapshot()
	prepared := Decide(s)
	s.PositionDebtRaw++
	if !fullDebtRepaymentRefreshed(prepared, Decide(s), s) {
		t.Fatal("accrued debt refused")
	}
	s.PositionDebtRaw -= 2
	if fullDebtRepaymentRefreshed(prepared, Decide(s), s) {
		t.Fatal("shrinking debt accepted")
	}
	partial := Decision{Action: DeleverRouteStep, Reason: "hard_ltv_repay", AmountRaw: 5, StrategyKey: s.RouteLane}
	if fullDebtRepaymentRefreshed(partial, Decision{Action: DeleverRouteStep, Reason: "hard_ltv_repay", AmountRaw: 6, StrategyKey: s.RouteLane}, s) {
		t.Fatal("partial repayment treated as whole debt")
	}
}
