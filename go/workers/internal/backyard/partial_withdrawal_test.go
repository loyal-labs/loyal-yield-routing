package backyard

import (
	"testing"
)

// Live 09-29 shape: 1.5x AUTO, 2463.48 AUTO @ 1.0209 ($2,514.97), debt
// 837.77 PYUSD, equity ~$1,677; Vlad withdraws $470 with Voltr idle 0.
func livePartialSnapshot() Snapshot {
	s := base()
	s.RouteLane, s.StrategyKey, s.HasPosition = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, true
	s.LeverageTargetLevel = 1.5
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 2_463_480_000, 2_514_967_000
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = 837_770_000, 837_770_000, 837_900_000
	s.StrategyNAVRaw, s.TotalVaultNAVRaw = 1_677_197_000, 1_677_197_000
	s.LTVBPS = 3331
	s.WithdrawalDemandRaw = 470_000_000
	return s
}

// Simulate each leg at the live prices (AUTO $1.0209, PYUSD $1, 1% swap
// loss) and return the sequence of decisions.
func runPartialWithdrawal(t *testing.T, s Snapshot, price float64) (Snapshot, []Decision) {
	t.Helper()
	var legs []Decision
	for step := 0; step < 40; step++ {
		d := Decide(s)
		legs = append(legs, d)
		// Restart safety: deciding again on the same snapshot gives the same leg.
		if again := Decide(s); !decisionsEqual(again, d) {
			t.Fatalf("stateless decide differs: %+v vs %+v", d, again)
		}
		// Prepare sizes the exact wire from the snapshot (stable decisions).
		if wire, sized := partialWithdrawalWireAmount(s, d); sized {
			d.AmountRaw = wire
		}
		switch {
		case d.Reason == partialReleaseReason:
			value := int64(float64(d.AmountRaw) * price)
			s.PositionCollateralRaw -= d.AmountRaw
			s.PositionCollateralValueRaw -= value
			s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = s.CollateralIdleRaw+d.AmountRaw, s.PrimeIdleRaw+d.AmountRaw, s.CollateralIdleValueRaw+value
			s.LTVBPS = s.PositionDebtValueRaw * 10_000 / max(s.PositionCollateralValueRaw, 1)
			if s.LTVBPS > leverageExitReleaseCeilingBPS {
				t.Fatalf("release lifted LTV to %d (> ceiling)", s.LTVBPS)
			}
			if hard := s; true {
				hard.LTVBPS = 6000
				if got := Decide(hard); got.Reason == partialSwapToDebtReason || got.Reason == partialReleaseReason {
					t.Fatalf("hard LTV did not preempt mid-chain: %+v", got)
				}
			}
		case d.Reason == partialSwapToDebtReason:
			value := int64(float64(d.AmountRaw) * price)
			s.CollateralIdleRaw -= d.AmountRaw
			s.PrimeIdleRaw = s.CollateralIdleRaw
			s.CollateralIdleValueRaw = int64(float64(s.CollateralIdleRaw) * price)
			setDebtCash(&s, debtCashOf(s)+value*99/100)
		case d.Reason == exitPartialRepayReason:
			setDebtCash(&s, debtCashOf(s)-d.AmountRaw)
			s.PositionDebtRaw -= d.AmountRaw
			s.PositionDebtValueRaw, s.PayoffDebtRaw = s.PositionDebtRaw, s.PositionDebtRaw+130_000
			s.LTVBPS = s.PositionDebtValueRaw * 10_000 / max(s.PositionCollateralValueRaw, 1)
		case d.Reason == partialSwapToUSDCReason:
			value := int64(float64(d.AmountRaw) * price)
			s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 0, 0
			s.SquadsIdleRaw += value * 99 / 100
		case d.Reason == partialDebtToUSDCReason:
			s.DebtIdleRaw, s.SquadsIdleRaw = 0, s.SquadsIdleRaw+d.AmountRaw*99/100
		case d.Reason == partialStageReason:
			s.SquadsIdleRaw -= d.AmountRaw
			s.VoltrStrategyIdleRaw += d.AmountRaw
			s.StagedAmountRaw, s.StagedAmountKnown, s.StageTransient = s.VoltrStrategyIdleRaw, true, true
		case d.Reason == "withdrawal_staged":
			s.VoltrStrategyIdleRaw -= d.AmountRaw
			s.VoltrIdleRaw += d.AmountRaw
			s.StagedAmountRaw, s.StageTransient = 0, false
		default:
			return s, legs
		}
	}
	t.Fatalf("partial withdrawal did not settle: %+v", legs)
	return s, legs
}

func debtCashOf(s Snapshot) int64 {
	if sharedUSDCDebt(s.RouteLane) {
		return s.SquadsIdleRaw
	}
	return s.DebtIdleRaw
}

func reasons(legs []Decision) []string {
	var out []string
	for _, d := range legs {
		out = append(out, d.Reason)
	}
	return out
}

func TestPartialWithdrawalLive15xAUTO(t *testing.T) {
	s, legs := runPartialWithdrawal(t, livePartialSnapshot(), 1.0209)
	want := []string{partialReleaseReason, partialSwapToDebtReason, exitPartialRepayReason, partialSwapToUSDCReason}
	got := reasons(legs)
	for i, r := range want {
		if i >= len(got) || got[i] != r {
			t.Fatalf("chain %v, want prefix %v", got, want)
		}
	}
	if s.VoltrIdleRaw < s.WithdrawalDemandRaw {
		t.Fatalf("withdrawal not covered: idle %d < demand %d (%v)", s.VoltrIdleRaw, s.WithdrawalDemandRaw, got)
	}
	final := legs[len(legs)-1]
	if final.Reason != "withdrawal_covered" && final.Reason != "withdrawal_covered_nav_due" {
		t.Fatalf("end %+v (%v)", final, got)
	}
	if !s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionDebtRaw <= 0 {
		t.Fatal("position closed")
	}
	if s.LTVBPS < 3200 || s.LTVBPS > 3450 {
		t.Fatalf("remaining LTV %d, want ~3333 (%v)", s.LTVBPS, got)
	}
	for _, d := range legs {
		if d.Reason == "withdrawal_release_repayment_collateral" || d.Reason == "withdrawal_withdraw_collateral" || d.Reason == "withdrawal_repay_debt" {
			t.Fatalf("full-exit leg in a partial withdrawal: %v", got)
		}
	}
	t.Logf("chain %v; remaining collateral %d debt %d LTV %d idle %d", got, s.PositionCollateralRaw, s.PositionDebtRaw, s.LTVBPS, s.VoltrIdleRaw)
}

func TestPartialWithdrawalDebtFree1xAndOnRe15x(t *testing.T) {
	s := livePartialSnapshot()
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS, s.LeverageTargetLevel = 0, 0, 0, 0, 1
	after, legs := runPartialWithdrawal(t, s, 1.0209)
	got := reasons(legs)
	if got[0] != partialReleaseReason || got[1] != partialSwapToUSDCReason || got[2] != partialStageReason || after.VoltrIdleRaw < after.WithdrawalDemandRaw || after.PositionCollateralRaw <= 0 || after.PositionDebtRaw != 0 {
		t.Fatalf("1x chain %v", got)
	}
	onre := livePartialSnapshot()
	onre.RouteLane, onre.StrategyKey = onreONycUSDC, onreONycUSDC
	after, legs = runPartialWithdrawal(t, onre, 1.0209)
	got = reasons(legs)
	if got[0] != partialReleaseReason || got[1] != partialSwapToUSDCReason || got[2] != exitPartialRepayReason || after.VoltrIdleRaw < after.WithdrawalDemandRaw || after.LTVBPS < 3200 || after.LTVBPS > 3450 || after.PositionDebtRaw <= 0 {
		t.Fatalf("OnRe chain %v LTV %d", got, after.LTVBPS)
	}
}

func TestPartialWithdrawalExclusionsNeverAuthorizeFullExit(t *testing.T) {
	// >= 90% of equity: hold, never a proof of full-exit necessity.
	s := livePartialSnapshot()
	s.WithdrawalDemandRaw = 1_600_000_000
	if d := Decide(s); d.Reason != "withdrawal_full_exit_unproven" {
		t.Fatalf("large demand: %+v", d)
	}
	// A sub-$50 remainder is allowed when the existing safe partial fits.
	s = livePartialSnapshot()
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 100_000_000, 102_090_000
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = 34_000_000, 34_000_000, 34_100_000
	s.WithdrawalDemandRaw = 30_000_000
	if d := Decide(s); d.Reason != partialReleaseReason {
		t.Fatalf("safe small remainder was excluded: %+v", d)
	}
	// Demand covered by Voltr idle: withdrawal_covered, unchanged.
	s = livePartialSnapshot()
	s.VoltrIdleRaw = 500_000_000
	if d := Decide(s); d.Reason != "withdrawal_covered" && d.Reason != "withdrawal_covered_nav_due" {
		t.Fatalf("covered: %+v", d)
	}
	// Explicit unwind keeps its separately gated full chain; unsupported Maple holds.
	s = livePartialSnapshot()
	s.Unwind = true
	if d := Decide(s); d.Reason == partialReleaseReason {
		t.Fatal("unwind went partial")
	}
	s = livePartialSnapshot()
	s.RouteLane, s.StrategyKey = SelectedRouteID, SelectedRouteID
	if d := Decide(s); d.Reason == partialReleaseReason {
		t.Fatal("Maple went partial")
	}
	// Hard LTV preempts.
	s = livePartialSnapshot()
	s.LTVBPS = 6000
	if d := Decide(s); d.Reason == partialReleaseReason {
		t.Fatal("hard LTV did not preempt")
	}
}

// Review: withdrawals of 28-89% of equity at 1.5x (AUTO and OnRe) keep every
// intermediate LTV under the release ceiling, run rounds as needed, end at
// the level with the position kept; 1x debt-free has no ceiling issue.
func TestPartialWithdrawalRoundsKeepLTVUnderTheCeiling(t *testing.T) {
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		for _, pct := range []int64{28, 39, 50, 75, 89} {
			s := livePartialSnapshot()
			s.RouteLane, s.StrategyKey = lane, lane
			s.WithdrawalDemandRaw = 1_677_197_000 * pct / 100
			if !partialWithdrawalFitsRounds(s, s.WithdrawalDemandRaw, 1_677_197_000) {
				// Exceeding the partial round cap is not full-exit authority.
				if d := Decide(s); d.Reason != "withdrawal_full_exit_unproven" {
					t.Fatalf("%s %d%%: beyond the round cap: %+v", lane, pct, d)
				}
				t.Logf("%s %d%%: hold (beyond %d rounds)", lane, pct, partialWithdrawalMaxRounds)
				continue
			}
			after, legs := runPartialWithdrawal(t, s, 1.0209)
			rounds := 0
			for _, d := range legs {
				if d.Reason == partialReleaseReason {
					rounds++
				}
				if d.Reason == "withdrawal_release_repayment_collateral" || d.Reason == "withdrawal_withdraw_collateral" {
					t.Fatalf("%s %d%%: full-exit leg %v", lane, pct, reasons(legs))
				}
			}
			if rounds > partialWithdrawalMaxRounds {
				t.Fatalf("%s %d%%: %d rounds", lane, pct, rounds)
			}
			if after.VoltrIdleRaw < after.WithdrawalDemandRaw || !after.HasPosition || after.PositionDebtRaw <= 0 || after.LTVBPS < 3200 || after.LTVBPS > 3450 {
				t.Fatalf("%s %d%%: idle %d LTV %d debt %d (%v)", lane, pct, after.VoltrIdleRaw, after.LTVBPS, after.PositionDebtRaw, reasons(legs))
			}
			t.Logf("%s %d%%: %d rounds, %d legs, final LTV %d", lane, pct, rounds, len(legs), after.LTVBPS)
		}
	}
	for _, pct := range []int64{28, 50, 89} {
		s := livePartialSnapshot()
		s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS, s.LeverageTargetLevel = 0, 0, 0, 0, 1
		s.WithdrawalDemandRaw = 2_514_967_000 * pct / 100
		after, legs := runPartialWithdrawal(t, s, 1.0209)
		if after.VoltrIdleRaw < after.WithdrawalDemandRaw || after.PositionDebtRaw != 0 || after.PositionCollateralRaw <= 0 || reasons(legs)[0] != partialReleaseReason {
			t.Fatalf("1x %d%%: %v", pct, reasons(legs))
		}
	}
}

func TestSmallFiveDollarWithdrawalPreservesResidualDebt(t *testing.T) {
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		s := livePartialSnapshot()
		s.RouteLane, s.StrategyKey = lane, lane
		s.PositionCollateralRaw, s.PositionCollateralValueRaw = 30_000_000, 30_627_000
		s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = 10_200_000, 10_200_000, 10_300_000
		s.WithdrawalDemandRaw = 5_000_000
		after, legs := runPartialWithdrawal(t, s, 1.0209)
		if after.VoltrIdleRaw < s.WithdrawalDemandRaw || after.PositionDebtRaw <= 0 || after.PositionCollateralRaw <= 0 {
			t.Fatalf("$5 did not safely preserve residual position: %+v %v", after, reasons(legs))
		}
	}
}
