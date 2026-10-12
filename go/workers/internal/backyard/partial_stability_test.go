package backyard

import "testing"

// Live 2026-09-29 blocker: decide and the prepare-time refresh differed by
// a few raw units (interest, oracle), so every tick failed decisionsEqual.
// Every partial-withdrawal and down-step decision carries a stable amount
// (USDC target, custody balances, a marker); the exact wire is sized in
// prepare. Re-deciding with debt accrued (+1 raw .. +0.01%) and the
// collateral price moved +/-1 bp must give an equal decision.
func TestPartialAndExitDecisionsAreStableAcrossRefresh(t *testing.T) {
	t.Parallel()
	type leg struct {
		name string
		snap Snapshot
	}
	var legs []leg
	// Walk the live partial withdrawal, recording each leg's snapshot.
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		s := livePartialSnapshot()
		s.RouteLane, s.StrategyKey = lane, lane
		for step := 0; step < 40; step++ {
			d := Decide(s)
			if d.Reason == "withdrawal_covered" || d.Reason == "withdrawal_covered_nav_due" {
				break
			}
			legs = append(legs, leg{lane + " " + d.Reason, s})
			if wire, sized := partialWithdrawalWireAmount(s, d); sized {
				d.AmountRaw = wire
			}
			s = applyPartialLeg(t, s, d, 1.0209)
		}
	}
	// The down steps: hard LTV, a drain, the move to 1x and to 1.5x.
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		hard := leverageExitSnapshot(lane)
		hard.LTVBPS = 6000
		legs = append(legs, leg{lane + " hard_ltv_delever", hard})
		drain := leverageExitSnapshot(lane)
		drain.Unwind = true
		legs = append(legs, leg{lane + " withdrawal_delever", drain})
		down := leverageExitSnapshot(lane)
		down.LeverageTargetLevel = 1
		legs = append(legs, leg{lane + " leverage_down", down})
		partial := leverageSnapshot(1.75)
		partial.RouteLane, partial.StrategyKey, partial.LeverageTargetLevel = lane, lane, 1.5
		legs = append(legs, leg{lane + " leverage_down_partial", partial})
	}
	for _, l := range legs {
		d := Decide(l.snap)
		for _, drift := range []func(Snapshot) Snapshot{
			func(s Snapshot) Snapshot { s.PositionDebtRaw++; s.PositionDebtValueRaw++; s.PayoffDebtRaw++; return s },
			func(s Snapshot) Snapshot {
				s.PositionDebtRaw += s.PositionDebtRaw / 10_000
				s.PositionDebtValueRaw += s.PositionDebtValueRaw / 10_000
				s.PayoffDebtRaw += s.PayoffDebtRaw / 10_000
				return s
			},
			func(s Snapshot) Snapshot {
				s.PositionCollateralValueRaw += s.PositionCollateralValueRaw / 10_000
				s.CollateralIdleValueRaw += s.CollateralIdleValueRaw / 10_000
				return s
			},
			func(s Snapshot) Snapshot {
				s.PositionCollateralValueRaw -= s.PositionCollateralValueRaw / 10_000
				s.CollateralIdleValueRaw -= s.CollateralIdleValueRaw / 10_000
				return s
			},
		} {
			if got := Decide(drift(l.snap)); !decisionsEqual(got, d) {
				t.Fatalf("%s: refreshed decision differs: %s/%s/%d vs %s/%s/%d", l.name, d.Action, d.Reason, d.AmountRaw, got.Action, got.Reason, got.AmountRaw)
			}
		}
	}
	if len(legs) < 12 {
		t.Fatalf("only %d legs covered", len(legs))
	}
}

// applyPartialLeg advances the snapshot by one executed leg (test model).
func applyPartialLeg(t *testing.T, s Snapshot, d Decision, price float64) Snapshot {
	t.Helper()
	switch d.Reason {
	case partialReleaseReason:
		value := int64(float64(d.AmountRaw) * price)
		left := value
		if s.PositionDebtRaw > 0 {
			// One down step: the debt share (+1%) converts at a 1% loss and
			// repays; the rest of the withdrawal waits as collateral.
			share := value * s.PositionDebtValueRaw / max(s.PositionCollateralValueRaw, 1) * 101 / 100
			repay := share * 99 / 100
			s.PositionDebtRaw -= repay
			s.PositionDebtValueRaw, s.PayoffDebtRaw = s.PositionDebtRaw, s.PositionDebtRaw+130_000
			left -= share
		}
		s.PositionCollateralRaw -= d.AmountRaw
		s.PositionCollateralValueRaw -= value
		s.CollateralIdleRaw += int64(float64(left) / price)
		s.PrimeIdleRaw = s.CollateralIdleRaw
		s.CollateralIdleValueRaw += left
		s.LTVBPS = s.PositionDebtValueRaw * 10_000 / max(s.PositionCollateralValueRaw, 1)
	case partialSwapToUSDCReason:
		value := int64(float64(d.AmountRaw) * price)
		s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 0, 0
		s.SquadsIdleRaw += value * 99 / 100
	case partialStageReason:
		s.SquadsIdleRaw -= d.AmountRaw
		s.VoltrStrategyIdleRaw += d.AmountRaw
		s.StagedAmountRaw, s.StagedAmountKnown, s.StageTransient = s.VoltrStrategyIdleRaw, true, true
	case "withdrawal_staged":
		s.VoltrStrategyIdleRaw -= d.AmountRaw
		s.VoltrIdleRaw += d.AmountRaw
		s.StagedAmountRaw, s.StageTransient = 0, false
	default:
		t.Fatalf("unexpected leg %+v", d)
	}
	return s
}
