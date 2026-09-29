package backyardrwa

import "testing"

// A 1.75x AUTO/OnRe position (C 1750, D 750, value units = raw units) as it
// moves through a full exit: release to the 55% ceiling, the released
// collateral cannot pay off the debt, so it is swapped and partly repaid
// (one cycle), then the next release pays off in full.
func leverageExitSnapshot(lane string) Snapshot {
	s := leverageSnapshot(1.75)
	s.RouteLane, s.StrategyKey, s.LeverageTargetLevel = lane, lane, 1.75
	s.PayoffDebtRaw = s.PositionDebtRaw + 1_000
	return s
}

func setDebtCash(s *Snapshot, raw int64) {
	if sharedUSDCDebt(s.RouteLane) {
		s.SquadsIdleRaw = raw
	} else {
		s.DebtIdleRaw = raw
	}
}

func TestExitCycleDecisionsOnEveryExitPath(t *testing.T) {
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		for path, mutate := range map[string]func(*Snapshot){
			"withdrawal": func(s *Snapshot) { s.WithdrawalDemandRaw, s.VoltrIdleRaw = 100_000_000, 0 },
			"unwind":     func(s *Snapshot) { s.Unwind = true },
			"down to 1x": func(s *Snapshot) { s.LeverageTargetLevel = 1 },
		} {
			s := leverageExitSnapshot(lane)
			mutate(&s)
			check := func(action Action, reason string) {
				t.Helper()
				got := Decide(s)
				if got.Action != action || (reason != "" && got.Reason != reason) {
					t.Fatalf("%s %s: want %s %s, got %+v", lane, path, action, reason, got)
				}
			}
			// 1. Nothing idle, LTV 42.9%: release (the installed first leg).
			if got := Decide(s); got.Action != DeleverRouteStep || !repaymentReleaseReason(got.Reason) {
				t.Fatalf("%s %s: first leg %+v", lane, path, got)
			}
			// 2. Released 390 (LTV 55%): cannot fund the 751 payoff -> cycle swap.
			s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.LTVBPS = 1_360_000_000, 1_360_000_000, 5500
			s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 390_000_000, 390_000_000, 390_000_000
			check(SwapCollateralToDebtStep, exitCycleSwapReason)
			// 3. Swapped: 386 debt cash < payoff -> partial repay, never the whole debt.
			s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 0, 0
			setDebtCash(&s, 386_000_000)
			got := Decide(s)
			if got.Action != DeleverRouteStep || got.Reason != exitPartialRepayReason || got.AmountRaw != 386_000_000 || got.AmountRaw >= s.PositionDebtRaw {
				t.Fatalf("%s %s: cycle repay %+v", lane, path, got)
			}
			// 4. Hard LTV preempts mid-cycle.
			hard := s
			hard.LTVBPS = 6_000
			if got := Decide(hard); got.Reason == exitPartialRepayReason || got.Reason == exitCycleSwapReason {
				t.Fatalf("%s %s: hard LTV did not preempt: %+v", lane, path, got)
			}
			// 5. After the repay (LTV ~27%) the installed release -> payoff runs.
			setDebtCash(&s, 0)
			s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS = 364_000_000, 364_000_000, 364_001_000, 2676
			if got := Decide(s); got.Reason == exitPartialRepayReason || got.Reason == exitCycleSwapReason {
				t.Fatalf("%s %s: cycle after LTV fell: %+v", lane, path, got)
			}
		}
	}
}

// 1.5x keeps its installed exit: after the release the idle collateral pays
// off the debt, so no cycle leg is ever chosen; Maple never cycles.
func TestExitCycleNeverChangesA15xOrMapleExit(t *testing.T) {
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC, SelectedRouteID} {
		s := leverageSnapshot(1.5)
		s.RouteLane, s.StrategyKey, s.Unwind = lane, lane, true
		s.PayoffDebtRaw = s.PositionDebtRaw + 1_000
		s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.LTVBPS = 910_000_000, 910_000_000, 5500
		s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 590_000_000, 590_000_000, 590_000_000
		if got := Decide(s); got.Action != SwapCollateralToDebtStep || got.Reason == exitCycleSwapReason {
			t.Fatalf("%s 1.5x exit changed: %+v", lane, got)
		}
		if lane == SelectedRouteID {
			s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 100, 100, 100
			if got := Decide(s); got.Reason == exitCycleSwapReason || got.Reason == exitPartialRepayReason {
				t.Fatalf("Maple cycled: %+v", got)
			}
		}
	}
}

// (a) Cash just below the payoff never leaves a dust debt: the repay stops at
// the residual floor, and cash within the floor of the debt repays nothing
// (the installed release -> full payoff funds it).
func TestExitCycleRepayRespectsTheResidualFloor(t *testing.T) {
	s := leverageExitSnapshot(onreONycUSDC)
	s.Unwind = true
	s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.LTVBPS = 1_360_000_000, 1_360_000_000, 5500
	floor := exitCycleResidualFloor(s.PositionDebtRaw)
	setDebtCash(&s, s.PositionDebtRaw-1)
	got := Decide(s)
	if got.Reason != exitPartialRepayReason || s.PositionDebtRaw-got.AmountRaw != floor {
		t.Fatalf("remainder %d, want the floor %d: %+v", s.PositionDebtRaw-got.AmountRaw, floor, got)
	}
	// Cash within the floor of the debt: repay stops at the floor, so the
	// remainder is never dust; the next release funds its full payoff.
	if s.PositionDebtRaw-got.AmountRaw < s.PositionDebtRaw/10 {
		t.Fatal("dust remainder")
	}
}

// (b) The live-shaped 1.5x AUTO position (2463.48 AUTO @ 1.0209 = $2,514.97,
// debt 837.77 PYUSD, LTV 33.3%): every exit chain (withdrawal, unwind, down
// to 1x) releases, then swaps enough to pay off in full; no cycle leg ever.
func TestLiveShaped15xExitsNeverCycle(t *testing.T) {
	for path, mutate := range map[string]func(*Snapshot){
		"withdrawal": func(s *Snapshot) { s.WithdrawalDemandRaw = 2_000_000_000 },
		"unwind":     func(s *Snapshot) { s.Unwind = true },
		"down to 1x": func(s *Snapshot) { s.LeverageTargetLevel = 1 },
	} {
		s := base()
		s.RouteLane, s.StrategyKey, s.PilotActive = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, true
		s.HasPosition, s.LeverageTargetLevel = true, 1.5
		s.PositionCollateralRaw, s.PositionCollateralValueRaw = 2_463_480_000, 2_514_967_000
		s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = 837_770_000, 837_770_000, 837_900_000
		s.LTVBPS = 3332
		mutate(&s)
		for step := 0; step < 4; step++ {
			got := Decide(s)
			if got.Reason == exitPartialRepayReason || got.Reason == exitCycleSwapReason {
				t.Fatalf("%s step %d: 1.5x exit cycled: %+v", path, step, got)
			}
			switch {
			case got.Action == DeleverRouteStep && repaymentReleaseReason(got.Reason):
				// Release to the 55% ceiling: frees C - D/0.55 = $991.8.
				releasedValue := s.PositionCollateralValueRaw - s.PositionDebtValueRaw*10_000/5500
				released := releasedValue * 1_000_000 / 1_020_900
				s.PositionCollateralRaw -= released
				s.PositionCollateralValueRaw -= releasedValue
				s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = released, released, releasedValue
				s.LTVBPS = 5500
			case got.Action == SwapCollateralToDebtStep:
				// Full funding swap: proceeds cover the payoff.
				s.DebtIdleRaw = s.CollateralIdleValueRaw * 99 / 100
				s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 0, 0
			case got.Action == DeleverRouteStep:
				if got.AmountRaw != s.PositionDebtRaw {
					t.Fatalf("%s: repay %d is not the full payoff %d", path, got.AmountRaw, s.PositionDebtRaw)
				}
				step = 4
			default:
				t.Fatalf("%s step %d: unexpected %+v", path, step, got)
			}
		}
	}
}
