package backyard

import "testing"

// B2 1.5x -> 1x: a stored 1x target under a leveraged AUTO/OnRe position
// runs release -> funding swap -> full payoff under leverage_down_* reasons,
// the same legs (and admissions) as a withdrawal payoff, without an unwind.
func TestLeverageDownTo1xRunsTheReleaseSwapPayoffChain(t *testing.T) {
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		s := leverageSnapshot(1.5)
		s.RouteLane, s.StrategyKey, s.LeverageTargetLevel = lane, lane, 1
		s.PayoffDebtRaw = s.PositionDebtRaw + 10
		check := func(action Action, reason string, amount int64) {
			t.Helper()
			got := Decide(s)
			if got.Action != action || got.Reason != reason || got.AmountRaw != amount || got.Validate() != nil {
				t.Fatalf("%s: want %s %s %d, got %+v", lane, action, reason, amount, got)
			}
			if !selectorTrancheInProgress(s) {
				t.Fatalf("%s: selector not frozen during the down move", lane)
			}
		}
		check(DeleverRouteStep, leverageDownReleaseReason, 1)
		// Released collateral covers the payoff: swap it to debt.
		s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 600_000_000, 600_000_000, 600_000_000
		check(SwapCollateralToDebtStep, leverageDownSwapReason, 600_000_000)
		// Funded: repay in full.
		s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 0, 0
		if lane == onreONycUSDC {
			s.SquadsIdleRaw = s.PayoffDebtRaw + 5
		} else {
			s.DebtIdleRaw = s.PayoffDebtRaw + 5
		}
		// On AUTO the existing idle_debt_repay (same funded full payoff)
		// runs first when the buffer exceeds the debt.
		wantRepay := leverageDownRepayReason
		if lane == autoAUTOPYUSD.Lane {
			wantRepay = "idle_debt_repay"
		}
		check(DeleverRouteStep, wantRepay, s.PositionDebtRaw)
		// Withdrawals and hard LTV keep their priority.
		w := s
		w.WithdrawalDemandRaw = 1
		if got := Decide(w); got.Reason == leverageDownRepayReason {
			t.Fatalf("%s: withdrawal did not preempt", lane)
		}
		// Without a 1x target nothing is repaid.
		s.LeverageTargetLevel = 1.5
		if got := Decide(s); got.Reason == leverageDownRepayReason || got.Reason == leverageDownReleaseReason {
			t.Fatalf("%s: repaid without a 1x target: %+v", lane, got)
		}
	}
	// The down reasons ride the existing withdrawal chain's gates.
	if !repaymentReleaseReason(leverageDownReleaseReason) || !repaymentReleaseReason("withdrawal_release_repayment_collateral") || repaymentReleaseReason("leverage_up") {
		t.Fatal("release reason group")
	}
	maple := leverageSnapshot(1.5)
	maple.RouteLane, maple.StrategyKey, maple.LeverageTargetLevel = SelectedRouteID, SelectedRouteID, 1
	if _, _, _, ok := leverageDownStep(maple); ok {
		t.Fatal("Maple ran a B2 down move")
	}
}
