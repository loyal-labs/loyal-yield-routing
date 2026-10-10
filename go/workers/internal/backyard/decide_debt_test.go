package backyard

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"testing"
	"time"
)

// Controlled snapshots exercise production decisions, not executed transfers.
func TestNonUSDCDrainFundsInterestShortfallBeforePayoff(t *testing.T) {
	_, _, _, _, _, _, accounts := payoffAdmissionFixture(t, 20_000)
	bound, err := decodeKaminoPayoffBound(accounts, ethenaUSDePYUSD, 42)
	if err != nil || bound.UpperDebtRaw != 1_001 {
		t.Fatal(bound, err)
	}
	s := base()
	s.RouteLane, s.CutoverDrain, s.HasPosition = ethenaUSDePYUSD.Lane, true, true
	s.PositionCollateralRaw, s.PositionDebtRaw = 100_000_000, int64(bound.ObservedDebtRaw)
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw = 100_000, 2_000
	s.PayoffDebtRaw, s.DebtIdleRaw = int64(bound.UpperDebtRaw), 1_000
	for _, cash := range []int64{1, 999, 1_000} {
		s.DebtIdleRaw = cash
		got := Decide(s)
		if got.Action != DeleverRouteStep || got.Reason != "withdrawal_release_repayment_collateral" {
			t.Fatal("insufficient buffer selected a repeated/dust repayment", cash, got)
		}
	}
	s.SquadsIdleRaw = 10
	if got := Decide(s); got.Action != SwapUSDCToDebtStep {
		t.Fatal("available USDC funding ignored", got)
	}
	// AUTO top-up cash beside debt (live Oct 9: 10k allocated) cannot fund the
	// payoff: the policy has no USDC->PYUSD edge, so collateral is released.
	auto := s
	auto.RouteLane, auto.SquadsIdleRaw = autoAUTOPYUSD.Lane, 10_000
	if got := Decide(auto); got.Action != DeleverRouteStep || got.Reason != "withdrawal_release_repayment_collateral" {
		t.Fatal("AUTO payoff funded through a missing USDC->PYUSD edge", got)
	}
	if action, _ := payoffFundingSource(auto, uint64(auto.PayoffDebtRaw)); action != "" {
		t.Fatal("NAV exit pricing chose a missing AUTO edge", action)
	}
	s.CollateralIdleRaw = 10
	s.CollateralIdleValueRaw = 10
	if got := Decide(s); got.Action != SwapCollateralToDebtStep {
		t.Fatal("available collateral funding ignored", got)
	}
	s.DebtIdleRaw = 1_001
	if got := Decide(s); got.Action != DeleverRouteStep || got.Reason != "withdrawal_repay_debt" || got.AmountRaw != 1_000 {
		t.Fatal("funded payoff not selected", got)
	}
	s.PayoffDebtRaw = -1
	if got := Decide(s); got.Action != HoldManualRecovery {
		t.Fatal("invalid payoff accepted", got)
	}
}

func TestNonUSDCLifecycleDecisionsKeepDebtAndBridgeCashSeparate(t *testing.T) {
	for _, lane := range []string{"AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD"} {
		t.Run(lane, func(t *testing.T) {
			s := base()
			s.RouteLane = lane
			s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 100000000, 100000000, 100000000
			check := func(action Action, amount int64) {
				t.Helper()
				got := Decide(s)
				if got.Action != action || got.AmountRaw != amount || got.StrategyKey != lane || got.Validate() != nil {
					t.Fatalf("wanted %s %d, got %+v", action, amount, got)
				}
			}
			s.VoltrIdleRaw, s.SelectorEntryEquityRaw = 100000000, 100000000
			check(VoltrAllocateToSquads, 100000000)
			s.VoltrIdleRaw, s.SquadsIdleRaw = 0, 100000000
			check(SwapStableToCollateralStep, 100000000)
			s.SquadsIdleRaw, s.CollateralIdleRaw = 0, 90000000
			check(OpenRouteStep, 90000000)
			s.CollateralIdleRaw, s.PositionCollateralRaw, s.HasPosition = 0, 90000000, true
			s.PositionCollateralValueRaw = 90_000_000
			if leverageLane(lane) {
				check(Hold, 0) // B2: leverage_target_required
				s.LeverageTargetLevel = 1.5000000
				armLeverageCapacityFixture(&s)
				check(OpenRouteStep, int64(s.LeverageApprovedBorrowRaw))
			} else {
				check(OpenRouteStep, 1)
			}
			s.PositionDebtRaw, s.DebtIdleRaw = 40000000, 40000000
			s.PositionDebtValueRaw = 80000000
			// An unrelated USDC residue cannot be treated as borrowed PYUSD.
			s.SquadsIdleRaw = 3000000
			check(SwapDebtToCollateralStep, 40000000)
			s.DebtIdleRaw, s.CollateralIdleRaw = 0, 35000000
			check(OpenRouteStep, 35000000)
			s.CollateralIdleRaw, s.PositionCollateralRaw = 0, 125000000
			if debtTopupLane(lane) {
				// Plan B3 beside debt: the USDC residue tops the position up
				// as collateral; it never repays or joins the PYUSD debt cash.
				check(SwapStableToCollateralStep, 3000000)
			} else {
				check(Hold, 0)
			}
			// Even ample Voltr USDC must not short-circuit a canary drain.
			s.CutoverDrain, s.WithdrawalDemandRaw, s.VoltrIdleRaw = true, 1, 200000000
			check(DeleverRouteStep, 1) // 3000000 USDC raw cannot fund 40000000 debt raw at $2
			s.SquadsIdleRaw, s.DebtIdleRaw = 0, 2
			check(DeleverRouteStep, 1)
			s.DebtIdleRaw, s.PositionDebtRaw = 0, 38000000
			check(DeleverRouteStep, 1)
			s.CollateralIdleRaw = 45000000
			s.CollateralIdleValueRaw = 100000000
			check(SwapCollateralToDebtStep, 45000000)
			s.CollateralIdleRaw, s.DebtIdleRaw = 0, 39000000
			check(DeleverRouteStep, 38000000)
			s.PositionDebtRaw, s.DebtIdleRaw = 0, 1
			check(DeleverRouteStep, 0)
			s.PositionCollateralRaw, s.HasPosition, s.CollateralIdleRaw = 0, false, 80000000
			check(SwapCollateralToStableStep, 80000000)
			s.CollateralIdleRaw, s.SquadsIdleRaw = 0, 75000000
			check(SwapDebtToUSDCStep, 1)
			s.DebtIdleRaw, s.SquadsIdleRaw = 0, 76000000
			check(StageSquadsToVoltr, 76000000)
			s.SquadsIdleRaw, s.VoltrStrategyIdleRaw = 0, 76000000
			s.StagedAmountKnown, s.StagedAmountRaw = true, 76000000
			check(VoltrRestoreIdle, 76000000)
			s.VoltrStrategyIdleRaw, s.PriorReportedNAVRaw = 0, 76000000
			check(ReportNAV, 0)
			s.PriorReportedNAVRaw = 0
			check(Hold, 0)
			if Decide(s).Reason != "canary_flat_nav_current" {
				t.Fatal("missing explicit flat terminal decision")
			}
		})
	}
}

func TestNonUSDCLifecycleSafetyPrecedence(t *testing.T) {
	s := base()
	s.RouteLane = "AUTO/AUTO/PYUSD"
	s.HasPosition, s.PositionCollateralRaw, s.PositionDebtRaw = true, 100, 40
	s.LTVBPS, s.DebtIdleRaw, s.SquadsIdleRaw, s.PostMutationNAVRequired = 6000, 5, 100, true
	if got := Decide(s); got.Action != DeleverRouteStep || got.AmountRaw != 5 {
		t.Fatal(got)
	}
	// AUTO's policy has no USDC->PYUSD edge: Squads cash is no repayment buffer.
	s.DebtIdleRaw = 0
	if got := Decide(s); got.Action != HoldManualRecovery || got.Reason != "hard_ltv_without_repayment_buffer" {
		t.Fatal(got)
	}
	s.RouteLane = ethenaUSDePYUSD.Lane
	if got := Decide(s); got.Action != SwapUSDCToDebtStep || got.AmountRaw != 100 {
		t.Fatal(got)
	}
	s.RouteLane, s.LTVBPS = "AUTO/AUTO/PYUSD", 4000
	if got := Decide(s); got.Action != ReportNAV {
		t.Fatal(got)
	}
	s.Nonterminal, s.HasAmbiguousSubmission, s.Fresh = Submitted, true, false
	if got := Decide(s); got.Action != RecoverTransaction || got.Reason != "recover_ambiguous_submission" {
		t.Fatal(got)
	}
	s.Nonterminal = ""
	if got := Decide(s); got.Action != HoldManualRecovery {
		t.Fatal(got)
	}
	s = base()
	s.RouteLane, s.VoltrIdleRaw, s.DebtIdleRaw = "AUTO/AUTO/PYUSD", 100, -1
	if got := Decide(s); got.Action != HoldManualRecovery {
		t.Fatal(got)
	}
	for _, action := range []Action{SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
		if WithdrawalPreemptsOpenLoop(action, Signed, 1) {
			t.Fatal("withdrawal cancelled its own exit conversion")
		}
	}
	for _, status := range []OperationStatus{Decided, Built, Simulated, Signed} {
		if !WithdrawalPreemptsOpenLoop(SwapDebtToCollateralStep, status, 1) {
			t.Fatal("withdrawal did not preempt debt reinvestment")
		}
	}
	if WithdrawalPreemptsOpenLoop(SwapDebtToCollateralStep, BroadcastIntent, 1) {
		t.Fatal("ambiguous wire must retain recovery")
	}
}

func TestFixedAccountObservationPreservesDecimalsAndUSDCEntryCapacity(t *testing.T) {
	route, accounts := nonUSDCDebtNAVFixture(t)
	// Feed the production fixed-account decoder a configured oracle, and 9/6
	// decimal assets. At 1.5/2 prices, 20,000 collateral raw units support
	// floor(20,000/1e9 * 1.5/2 * .5 * 1e6) = 7 debt raw units.
	putKey(t, accountAt(accounts, route.Kamino.CollateralReserve).Data[5112:5144], kaminoScopePrices)
	position, err := observeKaminoFromFixedAccounts(context.Background(), func(_ context.Context, addresses []string, slot int64) (int64, []ConfirmedAccount, error) {
		if len(addresses) != 1 || addresses[0] != kaminoScopePrices {
			t.Fatal("unexpected oracle graph")
		}
		return slot, []ConfirmedAccount{{Address: kaminoScopePrices, Lamports: 1, Data: []byte{1}}}, nil
	}, 77, append(append([]ConfirmedAccount{}, accounts...), clockFixture()), route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	borrow, err := position.targetLTVBorrowRaw()
	if err != nil || borrow != 7 {
		t.Fatalf("lost decimal scale in real observation: borrow=%d err=%v", borrow, err)
	}
	ltv, err := observedLTVBPS(position)
	if err != nil || ltv != 4667 {
		t.Fatalf("LTV lost decimal scale or rounded risk down: %d %v", ltv, err)
	}
	if _, _, err = withdrawExcessForRepayment(position); err == nil {
		t.Fatal("released collateral above the unwind LTV")
	}
	position.DebtRaw = 5
	leg, receipts, collateral, err := selectKaminoLeg(Decision{Action: DeleverRouteStep,
		StrategyKey: "AUTO/AUTO/PYUSD", Reason: "withdrawal_release_repayment_collateral", AmountRaw: 1}, position)
	if err != nil || leg != kaminoLegWithdraw || receipts != 2000 || collateral != 4000 {
		t.Fatalf("cross-decimal exit exceeded safe collateral release: leg=%d receipt=%d collateral=%d err=%v", leg, receipts, collateral, err)
	}
	position.EntryCapacityRaw = 123
	capacity, err := routeEntryCapacityUSDC(position, accounts, route)
	if err != nil || capacity != 246 {
		t.Fatalf("debt capacity inherited a USDC peg: %d %v", capacity, err)
	}
	position.DebtDecimals, position.EntryCapacityRaw = 9, 1234
	capacity, err = routeEntryCapacityUSDC(position, accounts, route)
	if err != nil || capacity != 2 {
		t.Fatalf("entry capacity did not floor across decimals: %d %v", capacity, err)
	}
	nav, err := ComputeRouteNAVForRoute(77, accounts, readyWorkerManifest(t), nil, route)
	if err != nil {
		t.Fatal(err)
	}
	s := base()
	s.Slot = 77
	if err = applyRouteNAVSnapshot(&s, nav, time.Unix(1_700_000_010, 0)); err != nil || s.DebtIdleRaw != 9 || s.CollateralIdleValueRaw != int64(nav.PrimeIdleValueRaw) {
		t.Fatal("debt custody lost before planning", err)
	}
	s.StrategyNAVRaw = int64(nav.StrategyNAVRaw)
	s.VoltrIdleRaw, s.VoltrStrategyIdleRaw, s.SquadsIdleRaw = int64(nav.Custodies.VoltrIdleRaw), int64(nav.Custodies.StrategyUSDCraw), int64(nav.Custodies.SquadsUSDCraw)
	s.HasPosition, s.PositionCollateralRaw, s.PositionDebtRaw = true, int64(position.CollateralDepositedRaw), 7
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw, s.LTVBPS = int64(nav.PositionCollateralValue), int64(nav.PositionDebtValue), ltv
	projection, err := newRouteObservationProjection(Observation{Snapshot: s, ObservedAt: time.Unix(1_700_000_010, 0)})
	if err != nil || projection.DebtIdleRaw != "9" || projection.CollateralIdleValueRaw != fmt.Sprint(nav.PrimeIdleValueRaw) {
		t.Fatal("debt custody lost from durable projection", err)
	}
	nav.Custodies.SquadsDebtRaw = math.MaxUint64
	if err = applyRouteNAVSnapshot(&s, nav, time.Unix(1_700_000_010, 0)); err == nil {
		t.Fatal("debt custody overflow accepted")
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, kaminoDebtReserve).Data[272:280], 9)
	if _, err = routeEntryCapacityUSDC(position, accounts, route); err == nil {
		t.Fatal("wrong USDC reference decimals accepted")
	}
}
