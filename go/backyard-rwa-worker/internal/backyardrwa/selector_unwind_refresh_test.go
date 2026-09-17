package backyardrwa

import (
	"context"
	"testing"
)

func TestExpiredUnwindDebtRequiresReadmissionWithoutBlockingRisk(t *testing.T) {
	s := base()
	s.RouteLane, s.StrategyKey = SelectedRouteID, SelectedRouteID
	s.PilotActive, s.HasPosition = true, true
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 150, 150
	s.PositionDebtRaw, s.PositionDebtValueRaw = 51, 51
	s.PayoffDebtRaw, s.LTVBPS = 52, 3400
	intent := UnwindIntent{SourceLane: s.RouteLane, Reason: "economic_rotation", ObservationID: s.ObservationID, MaxCollateralRaw: 150, MaxDebtRaw: 50, CostBoundRaw: 100, BudgetScope: Phase3GoalID, BudgetFamily: "Maple", EvidenceID: sha256Bytes([]byte("exit")), CreatedAt: selectorFixture().Now}
	if err := applyUnwindIntent(&s, &intent); err != nil || !s.Unwind || !s.UnwindRefreshRequired || s.ManualReason != "" {
		t.Fatal("interest created permanent latch", err, s)
	}
	if d := Decide(s); d.Action != Hold || d.Reason != "unwind_requires_fresh_admission" {
		t.Fatal(d)
	}
	s.LTVBPS, s.SquadsIdleRaw = 6000, 52
	if d := Decide(s); d.Action != DeleverRouteStep || d.Reason != "hard_ltv_repay" {
		t.Fatal("renewal blocked risk reduction", d)
	}
	s.PositionCollateralRaw++
	if err := applyUnwindIntent(&s, &intent); err == nil {
		t.Fatal("unexpected collateral growth bypassed original bound")
	}
}

func TestWorkerAttemptsRenewalBeforeJournalingOrdinaryUnwind(t *testing.T) {
	s := base()
	s.Unwind, s.UnwindRefreshRequired = true, true
	worker := &Worker{routeKey: productionRouteKey}
	worker.runtime.loadNonterminal = func(context.Context, string) (*PersistedOperation, error) { return nil, nil }
	worker.runtime.observe = func(context.Context) (Observation, error) { return tickObservation(s), nil }
	calls := 0
	worker.runtime.refreshUnwind = func(context.Context) error { calls++; return nil }
	worker.runtime.recordDecision = func(context.Context, string, Observation, Decision, string, string) (DecisionRecord, error) {
		t.Fatal("renewal also created executable operation")
		return DecisionRecord{}, nil
	}
	if err := worker.Tick(context.Background()); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	worker.runtime.loadNonterminal = func(context.Context, string) (*PersistedOperation, error) {
		return &PersistedOperation{Status: Signed}, nil
	}
	worker.runtime.advance = func(context.Context, PersistedOperation) error { return nil }
	if err := worker.Tick(context.Background()); err != nil || calls != 1 {
		t.Fatal("renewed ahead of transaction recovery", err, calls)
	}
}
