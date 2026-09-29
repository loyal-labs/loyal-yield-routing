package backyardrwa

import "math/big"

// B2 1.75x exit (docs/plans/b2-175x-multi-cycle-exit-design.md). One release
// at the pilot release ceiling (min(55%, maxLTV-5, hard-5)) frees
// C - D/ceiling; at 1.75x that is ~0.39 of 0.75 debt. A cycle is
// release -> swap -> PARTIAL repay; after at most leverageExitMaxCycles
// cycles one release must cover the payoff, which the existing
// release -> swap -> full payoff legs then finish.
const (
	leverageExitMaxCycles = 2
	// Swap output planning uses a 1% loss floor, like the up-move check.
	leverageExitSwapLossBPS = 100
)

// leverageExitPlan is the pure, value-level cycle plan: every amount is in
// one common value unit (USDC-equivalent at current prices).
type leverageExitPlan struct {
	Cycles []leverageExitCycle
	// FinalReleaseValue is the release that funds the full payoff.
	FinalReleaseValue *big.Int
}

type leverageExitCycle struct {
	ReleaseValue, RepayValue *big.Int
}

// planLeverageExit returns the cycles a full exit needs. collateral and debt
// are values in the same unit; ceilingBPS is the release ceiling; debt
// grows by accrualBPS per cycle window. More than leverageExitMaxCycles
// cycles holds for manual review (leverage_exit_cycles_exceeded).
func planLeverageExit(collateral, debt *big.Int, ceilingBPS, accrualBPS int64) (leverageExitPlan, error) {
	var plan leverageExitPlan
	if collateral == nil || debt == nil || collateral.Sign() <= 0 || debt.Sign() < 0 || ceilingBPS <= 0 || ceilingBPS >= 10_000 || accrualBPS < 0 {
		return plan, budgetHold("invalid_leverage_exit_plan")
	}
	c, d := new(big.Int).Set(collateral), new(big.Int).Set(debt)
	for {
		// Accrue before the release is sized, like the payoff window.
		d.Add(d, new(big.Int).Quo(new(big.Int).Mul(d, big.NewInt(accrualBPS)), big.NewInt(10_000)))
		// release = C - D * 10000 / ceiling (floored at zero)
		need := new(big.Int).Quo(new(big.Int).Mul(d, big.NewInt(10_000)), big.NewInt(ceilingBPS))
		release := new(big.Int).Sub(c, need)
		if release.Sign() <= 0 {
			return plan, budgetHold("no_safe_repayment_collateral_release")
		}
		proceeds := new(big.Int).Quo(new(big.Int).Mul(release, big.NewInt(10_000-leverageExitSwapLossBPS)), big.NewInt(10_000))
		if proceeds.Cmp(d) >= 0 {
			plan.FinalReleaseValue = release
			return plan, nil
		}
		if len(plan.Cycles) == leverageExitMaxCycles {
			return plan, budgetHold("leverage_exit_cycles_exceeded")
		}
		plan.Cycles = append(plan.Cycles, leverageExitCycle{ReleaseValue: release, RepayValue: proceeds})
		c.Sub(c, release)
		d.Sub(d, proceeds)
	}
}
