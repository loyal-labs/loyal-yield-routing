package backyardrwa

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// Controlled snapshots exercise production decisions, not executed transfers.
func TestNonUSDCLifecycleDecisionsKeepDebtAndBridgeCashSeparate(t *testing.T) {
	for _, lane := range []string{"AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD"} {
		t.Run(lane, func(t *testing.T) {
			s := base()
			s.RouteLane = lane
			s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 100, 100, 100
			check := func(action Action, amount int64) {
				t.Helper()
				got := Decide(s)
				if got.Action != action || got.AmountRaw != amount || got.StrategyKey != lane || got.Validate() != nil {
					t.Fatalf("wanted %s %d, got %+v", action, amount, got)
				}
			}
			s.VoltrIdleRaw = 100
			check(VoltrAllocateToSquads, 100)
			s.VoltrIdleRaw, s.SquadsIdleRaw = 0, 100
			check(SwapStableToCollateralStep, 100)
			s.SquadsIdleRaw, s.CollateralIdleRaw = 0, 90
			check(OpenRouteStep, 90)
			s.CollateralIdleRaw, s.PositionCollateralRaw, s.HasPosition = 0, 90, true
			check(OpenRouteStep, 1)
			s.PositionDebtRaw, s.DebtIdleRaw = 40, 40
			// An unrelated USDC residue cannot be treated as borrowed PYUSD.
			s.SquadsIdleRaw = 3
			check(SwapDebtToCollateralStep, 40)
			s.DebtIdleRaw, s.CollateralIdleRaw = 0, 35
			check(OpenRouteStep, 35)
			s.CollateralIdleRaw, s.PositionCollateralRaw = 0, 125
			check(Hold, 0)
			// Even ample Voltr USDC must not short-circuit a canary drain.
			s.CutoverDrain, s.WithdrawalDemandRaw, s.VoltrIdleRaw = true, 1, 200
			check(SwapUSDCToDebtStep, 3)
			s.SquadsIdleRaw, s.DebtIdleRaw = 0, 2
			check(DeleverRouteStep, 2)
			s.DebtIdleRaw, s.PositionDebtRaw = 0, 38
			check(DeleverRouteStep, 1)
			s.CollateralIdleRaw = 45
			check(SwapCollateralToDebtStep, 45)
			s.CollateralIdleRaw, s.DebtIdleRaw = 0, 39
			check(DeleverRouteStep, 38)
			s.PositionDebtRaw, s.DebtIdleRaw = 0, 1
			check(DeleverRouteStep, 0)
			s.PositionCollateralRaw, s.HasPosition, s.CollateralIdleRaw = 0, false, 80
			check(SwapCollateralToStableStep, 80)
			s.CollateralIdleRaw, s.SquadsIdleRaw = 0, 75
			check(SwapDebtToUSDCStep, 1)
			s.DebtIdleRaw, s.SquadsIdleRaw = 0, 76
			check(StageSquadsToVoltr, 76)
			s.SquadsIdleRaw, s.VoltrStrategyIdleRaw = 0, 76
			check(VoltrRestoreIdle, 76)
			s.VoltrStrategyIdleRaw, s.PriorReportedNAVRaw = 0, 76
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
	s.DebtIdleRaw = 0
	if got := Decide(s); got.Action != SwapUSDCToDebtStep || got.AmountRaw != 100 {
		t.Fatal(got)
	}
	s.LTVBPS = 4000
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
	s.RouteLane, s.VoltrIdleRaw, s.ExitBuildable = "AUTO/AUTO/PYUSD", 100, false
	if got := Decide(s); got.Action != Hold {
		t.Fatal("allocation preceded exit readiness", got)
	}
	s.ExitBuildable, s.DebtIdleRaw = true, -1
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
	}, 77, accounts, route.Kamino)
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
	if err = applyRouteNAVSnapshot(&s, nav, time.Unix(1_700_000_010, 0)); err != nil || s.DebtIdleRaw != 9 {
		t.Fatal("debt custody lost before planning", err)
	}
	s.StrategyNAVRaw = int64(nav.StrategyNAVRaw)
	s.VoltrIdleRaw, s.VoltrStrategyIdleRaw, s.SquadsIdleRaw = int64(nav.Custodies.VoltrIdleRaw), int64(nav.Custodies.StrategyUSDCraw), int64(nav.Custodies.SquadsUSDCraw)
	s.HasPosition, s.PositionCollateralRaw, s.PositionDebtRaw = true, int64(position.CollateralDepositedRaw), 7
	s.PositionCollateralValueRaw, s.PositionDebtValueRaw, s.LTVBPS = int64(nav.PositionCollateralValue), int64(nav.PositionDebtValue), ltv
	projection, err := newRouteObservationProjection(Observation{Snapshot: s, ObservedAt: time.Unix(1_700_000_010, 0)})
	if err != nil || projection.DebtIdleRaw != "9" {
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
