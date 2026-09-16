package backyardrwa

import (
	"context"
	"testing"
)

func TestSelectorSourceCashReturnCountsEveryReportWithoutChargingPrincipal(t *testing.T) {
	o, d, e := bridgeAdmissionFixture(t, ReportNAV, 0, 90_000_000, 0, 10_000_000)
	o.Snapshot.RouteLane, o.Snapshot.StrategyKey = SelectedRouteID, SelectedRouteID
	o.Snapshot.PilotActive = true
	d.StrategyKey = SelectedRouteID
	rpc := budgetBuildRPC(t, 5000, 42)
	plan, err := observePhase3BridgeAdmission(context.Background(), rpc, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	q, err := priceSelectorSourcePlan(context.Background(), rpc, plan, 42)
	if err != nil {
		t.Fatal(err)
	}
	if q.MinimumIdleRaw != 100_000_000 || q.Recipe.NetworkLamports != 25_000 || q.Recipe.CostRaw <= 0 || q.Recipe.CostRaw >= 10_000 || len(q.Recipe.Inputs) != 5 {
		t.Fatal("principal/fees lost", q)
	}
	var network int64
	for _, c := range q.Recipe.Costs {
		network += c.NetworkFeeMicros
	}
	if q.Recipe.CostRaw != network {
		t.Fatal("returned principal charged", q.Recipe.CostRaw, network)
	}
	// Settle the actual report in the budget model, then prove that unchanged
	// source pricing fits exactly its remaining reservation, without requiring
	// another already-completed report fee. This is accounting, not chain proof.
	prior := emptyTestBudget()
	budget, err := activatePilotBudget(prior, pilotTestAuthority(prior))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := Phase3IntentDigest(e.Request, plan.Input.Effects)
	if err != nil {
		t.Fatal(err)
	}
	r := BudgetReservation{OperationID: "source-nav", Family: "Maple", IntentSHA256: intent, UpperMicros: plan.CurrentCost.TotalMicros, ExecutionCostUpperMicros: plan.CurrentCost.NetworkFeeMicros, ExitAfterMicros: plan.ExitAfterMicros}
	if err = budget.Admit(r); err != nil {
		t.Fatal(err)
	}
	if err = budget.Settle(r.OperationID, intent, r.UpperMicros); err != nil {
		t.Fatal(err)
	}
	if Decide(plan.Snapshot).Action == ReportNAV || q.ExitBound == nil || q.ExitBound.GrossMicros != budget.Families["Maple"].ExitMicros {
		t.Fatal("settled NAV charged again against exit reservation", q.ExitBound, budget.Families["Maple"])
	}
	// NAV copies can share bytes. They are still separately executed messages.
	plan.Exit = append(plan.Exit, plan.Exit[len(plan.Exit)-1])
	repeated, err := priceSelectorSourcePlan(context.Background(), rpc, plan, 42)
	if err != nil || repeated.Recipe.NetworkLamports != 30_000 {
		t.Fatal("repeated report deduplicated", repeated, err)
	}
	plan.Exit[0].Template = nil
	_, err = priceSelectorSourcePlan(context.Background(), rpc, plan, 42)
	assertBudgetHold(t, err, "selector_source_template_missing")
}

func TestSelectorSourcePricesFundedPositionAndUsesMinimumResidue(t *testing.T) {
	o, m, rpc, client, _ := usdcReturnFixture(t)
	o.Snapshot.PilotActive = true
	o.Snapshot.WithdrawalDemandRaw = 0
	o.Snapshot.ReportSnapshotDigest = sha256Bytes([]byte("controlled-source-nav"))
	q, err := observeSelectorSource(context.Background(), rpc, client, m, o)
	if err != nil {
		t.Fatal(err)
	}
	cash := uint64(o.Snapshot.VoltrIdleRaw + o.Snapshot.SquadsIdleRaw)
	var swaps, payoffs int
	var upperBridge uint64
	for _, input := range q.Recipe.Inputs {
		r, _, _, err := input.decode()
		if err != nil {
			t.Fatal(err)
		}
		switch r := r.(type) {
		case JupiterSwapRequest:
			cash += r.MinimumOutputRaw
			swaps++
		case KaminoPrimeUSDCRequest:
			_, leg, _ := kaminoPrimeUSDCInstruction(r)
			if leg == kaminoLegRepay {
				cash -= r.AmountRaw
				payoffs++
			}
		case BridgeBuildRequest:
			if r.Action == VoltrRestoreIdle {
				upperBridge = r.AmountRaw
			}
		}
	}
	if swaps != 1 || payoffs != 1 || q.MinimumIdleRaw != cash || q.MinimumIdleRaw >= uint64(o.Snapshot.VoltrIdleRaw)+upperBridge || q.Recipe.CostRaw <= 0 {
		t.Fatal("optimistic reservation residue treated as minimum proceeds", q.MinimumIdleRaw, cash, upperBridge, swaps, payoffs)
	}
}

func TestSelectorSourceIdleNeedsNoExitAndPendingWorkPrecedesEconomics(t *testing.T) {
	m, rpc, client, _ := selectorDestinationFixture(t)
	s := Snapshot{PilotActive: true, Fresh: true, Slot: 42, ObservationID: "idle", RouteLane: SelectedRouteID, StrategyKey: SelectedRouteID, VoltrIdleRaw: 10_000_000}
	q, err := observeSelectorSource(context.Background(), rpc, client, m, tickObservation(s))
	if err != nil || q.MinimumIdleRaw != 10_000_000 || q.Recipe.CostRaw != 0 || len(q.Recipe.Inputs) != 0 {
		t.Fatal(q, err)
	}
	for _, change := range []func(*Snapshot){func(s *Snapshot) { s.Nonterminal = Signed }, func(s *Snapshot) { s.WithdrawalDemandRaw = 1 }, func(s *Snapshot) { s.Unwind = true }, func(s *Snapshot) { s.ManualReason = "recovery" }} {
		changed := s
		change(&changed)
		_, err = observeSelectorSource(context.Background(), rpc, client, m, tickObservation(changed))
		assertBudgetHold(t, err, "selector_source_unavailable")
	}
}

func TestSelectorSourcePricesFullTenUSDCLoopWithRepaymentRelease(t *testing.T) {
	m, rpc, client, accounts := selectorDestinationFixture(t)
	route, _ := runtimeRoute(SelectedRouteID)
	obligation := obligationFixture(t, 42, 15_000_000, 5_000_000)
	putKey(t, obligation.Data[32:64], route.Kamino.Market)
	putKey(t, obligation.Data[96:128], route.Kamino.CollateralReserve)
	putKey(t, obligation.Data[1208:1240], route.Kamino.DebtReserve)
	copy(accountAt(accounts, route.Kamino.Obligation).Data, obligation.Data)
	s := Snapshot{PilotActive: true, Fresh: true, Slot: 42, ObservationID: "full-loop", RouteKind: RouteKind, RouteLane: route.Lane, StrategyKey: route.Lane, VoltrIdleRaw: 90_000_000,
		HasPosition: true, PositionCollateralRaw: 15_000_000, PositionCollateralValueRaw: 15_000_000, PositionDebtRaw: 5_000_000, PositionDebtValueRaw: 5_000_000, StrategyNAVRaw: 10_000_000, TotalVaultNAVRaw: 100_000_000, ReportSnapshotDigest: sha256Bytes([]byte("full-loop-nav"))}
	before := hashConfirmedAccounts(accounts)
	q, err := observeSelectorSource(context.Background(), rpc, client, m, tickObservation(s))
	if err != nil {
		t.Fatal(err)
	}
	if before != hashConfirmedAccounts(accounts) || q.MinimumIdleRaw <= 99_000_000 || q.MinimumIdleRaw >= 100_000_000 || q.Recipe.CostRaw >= 1_000_000 {
		t.Fatal("full-loop minimum or accounting invalid", q)
	}
	var swaps, withdrawals, payoffs int
	for _, input := range q.Recipe.Inputs {
		request, _, _, err := input.decode()
		if err != nil {
			t.Fatal(err)
		}
		switch r := request.(type) {
		case JupiterSwapRequest:
			swaps++
		case KaminoPrimeUSDCRequest:
			_, leg, _ := kaminoPrimeUSDCInstruction(r)
			if leg == kaminoLegWithdraw {
				withdrawals++
			}
			if leg == kaminoLegRepay {
				payoffs++
			}
		}
	}
	if q.ExitBound == nil || q.ExitBound.MaxCollateralRaw != s.PositionCollateralRaw || q.ExitBound.MaxDebtRaw < s.PositionDebtRaw || q.ExitBound.GrossMicros <= q.Recipe.CostRaw {
		t.Fatal("source exit did not retain receipts, payoff ceiling and gross reservation", q.ExitBound)
	}
	if swaps != 2 || withdrawals != 2 || payoffs != 1 {
		t.Fatal("release/funding/payoff/full-return incomplete", swaps, withdrawals, payoffs)
	}
}

func TestSelectorSourceRetainedObservationCannotMoveBackwards(t *testing.T) {
	o, d, e := bridgeAdmissionFixture(t, ReportNAV, 0, 0, 0, 1_000_000)
	o.Snapshot.RouteLane, o.Snapshot.StrategyKey = SelectedRouteID, SelectedRouteID
	o.Snapshot.PilotActive = true
	d.StrategyKey = SelectedRouteID
	rpc := budgetBuildRPC(t, 5000, 42)
	plan, err := observePhase3BridgeAdmission(context.Background(), rpc, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	plan.Exit[0].Cost.ObservationSlot = 70
	if _, err = priceSelectorSourcePlan(context.Background(), rpc, plan, 42); err == nil {
		t.Fatal("later retained sample forgotten during repricing")
	}
	plan.Exit[0].Cost.ObservationSlot = 75
	_, err = priceSelectorSourcePlan(context.Background(), rpc, plan, 42)
	assertBudgetHold(t, err, "selector_recipe_observation_expired")
}
