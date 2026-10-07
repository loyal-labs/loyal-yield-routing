package backyard

import "testing"

// Step 5: 1.75x -> 1.5x = one cycle sized to the 1.5x LTV: release
// R = (3D - C)/2, swap it, repay the proceeds, stop. Never the whole debt,
// never below 1.5x by more than the swap loss.
func TestDownMove175To15IsOneSizedCycle(t *testing.T) {
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		s := leverageSnapshot(1.75) // C 1750, D 750 (value = raw)
		s.RouteLane, s.StrategyKey, s.LeverageTargetLevel = lane, lane, 1.5
		action, reason, marker, ok := leverageDownPartialStepAt(s, true)
		receipts := leverageDownPartialReceipts(s) // sized in prepare
		if !ok || action != DeleverRouteStep || reason != leverageDownPartialReleaseReason || marker != 1 || receipts != 250_000_000 {
			t.Fatalf("%s release %s %s %d", lane, action, reason, receipts)
		}
		// After the release: 250 idle collateral -> swap (LTV 50%).
		s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.LTVBPS = 1_500_000_000, 1_500_000_000, 5000
		s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 250_000_000, 250_000_000, 250_000_000
		if action, reason, amount, _ := leverageDownPartialStepAt(s, true); action != SwapCollateralToDebtStep || reason != exitCycleSwapReason || amount != 250_000_000 {
			t.Fatalf("%s swap %s %s %d", lane, action, reason, amount)
		}
		// After the swap (1% loss): 247.5 debt cash -> partial repay, not the whole debt.
		s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 0, 0
		setDebtCash(&s, 247_500_000)
		action, reason, _, _ = leverageDownPartialStepAt(s, true)
		amount := exitPartialRepayWireRaw(s) // sized in prepare
		if action != DeleverRouteStep || reason != exitPartialRepayReason || amount != 247_500_000 || amount >= s.PositionDebtRaw {
			t.Fatalf("%s repay %s %s %d", lane, action, reason, amount)
		}
		// After the repay: C 1500, D 502.5 -> LTV 33.5% = 1.5x; the step stops.
		setDebtCash(&s, 0)
		s.PositionDebtRaw, s.PositionDebtValueRaw, s.LTVBPS = 502_500_000, 502_500_000, 3350
		if _, _, _, ok := leverageDownPartialStepAt(s, true); ok || currentLeverageBand(s) != 1.5 {
			t.Fatalf("%s did not stop at 1.5x", lane)
		}
		if got := Decide(s); got.Action != Hold {
			t.Fatalf("%s after the move: %+v", lane, got)
		}
	}
	// Inert while live levels stop at 1.5x; Maple never.
	s := leverageSnapshot(1.75)
	s.LeverageTargetLevel = 1.5
	if _, _, _, ok := leverageDownPartialStep(s); ok != leverageDownPartialEnabled {
		t.Fatal("down-partial gate")
	}
	s.RouteLane, s.StrategyKey = SelectedRouteID, SelectedRouteID
	if _, _, _, ok := leverageDownPartialStepAt(s, true); ok {
		t.Fatal("Maple de-levered")
	}
}
