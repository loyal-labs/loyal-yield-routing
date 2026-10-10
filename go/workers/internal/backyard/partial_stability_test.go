package backyard

import "testing"

// Live 2026-09-29 blocker: decide and the prepare-time refresh differed by
// a few raw units (interest, oracle), so every tick failed decisionsEqual.
// Every partial-withdrawal and exit-cycle decision now carries a stable
// amount (USDC target, custody balances, a marker); the exact wire is sized
// in prepare. Re-deciding with debt accrued (+1 raw .. +0.01%) and the
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
	// 1.75x exit cycle and down move legs.
	for _, lane := range []string{autoAUTOPYUSD.Lane, onreONycUSDC} {
		c := leverageExitSnapshot(lane)
		c.Unwind = true
		c.PositionCollateralRaw, c.PositionCollateralValueRaw, c.LTVBPS = 1_360_000_000, 1_360_000_000, 5500
		c.CollateralIdleRaw, c.PrimeIdleRaw, c.CollateralIdleValueRaw = 390_000_000, 390_000_000, 390_000_000
		legs = append(legs, leg{lane + " exit_cycle_swap", c})
		c.CollateralIdleRaw, c.PrimeIdleRaw, c.CollateralIdleValueRaw = 0, 0, 0
		setDebtCash(&c, 386_000_000)
		legs = append(legs, leg{lane + " exit_partial_repay", c})
		down := leverageSnapshot(1.75)
		down.RouteLane, down.StrategyKey, down.LeverageTargetLevel = lane, lane, 1.5
		legs = append(legs, leg{lane + " leverage_down_partial_release(gated)", down})
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
		s.PositionCollateralRaw -= d.AmountRaw
		s.PositionCollateralValueRaw -= value
		s.CollateralIdleRaw += d.AmountRaw
		s.PrimeIdleRaw = s.CollateralIdleRaw
		s.CollateralIdleValueRaw += value
		s.LTVBPS = s.PositionDebtValueRaw * 10_000 / max(s.PositionCollateralValueRaw, 1)
	case partialSwapToDebtReason:
		value := int64(float64(d.AmountRaw) * price)
		s.CollateralIdleRaw -= d.AmountRaw
		s.PrimeIdleRaw = s.CollateralIdleRaw
		s.CollateralIdleValueRaw = int64(float64(s.CollateralIdleRaw) * price)
		setDebtCash(&s, debtCashOf(s)+value*99/100)
	case exitPartialRepayReason:
		setDebtCash(&s, debtCashOf(s)-d.AmountRaw)
		s.PositionDebtRaw -= d.AmountRaw
		s.PositionDebtValueRaw, s.PayoffDebtRaw = s.PositionDebtRaw, s.PositionDebtRaw+130_000
		s.LTVBPS = s.PositionDebtValueRaw * 10_000 / max(s.PositionCollateralValueRaw, 1)
	case partialSwapToUSDCReason:
		value := int64(float64(d.AmountRaw) * price)
		s.CollateralIdleRaw, s.PrimeIdleRaw, s.CollateralIdleValueRaw = 0, 0, 0
		s.SquadsIdleRaw += value * 99 / 100
	case partialDebtToUSDCReason:
		s.DebtIdleRaw, s.SquadsIdleRaw = 0, s.SquadsIdleRaw+d.AmountRaw*99/100
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
