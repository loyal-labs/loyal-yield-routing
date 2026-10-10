package backyard

import "math"

// B2 option 1 (docs/plans/b2-variable-leverage-design.md): the live leverage
// levels and the one-step rule the watch-only log has used since 074d6fd.
// This file holds the pure level logic; the durable target and the moves are
// built on it.

var leverageLevels = []float64{1, 1.5, 1.75}

// leverageLevelLTVBPS is the LTV a level sits at after its last redeposit:
// 1 - 1/level (1x = 0, 1.5x = 33.33%, 1.75x = 42.86%).
func leverageLevelLTVBPS(level float64) int64 {
	if level <= 1 {
		return 0
	}
	return int64(math.Floor((1 - 1/level) * 10_000))
}

// leverageMaxLTVBPS is the option-1 cap after any move (the 45% warning).
const leverageMaxLTVBPS int64 = 4500

// currentLeverageLevel is actual supplied collateral/equity; desired policy
// bands remain discrete and are resolved separately by currentLeverageBand.
func currentLeverageLevel(s Snapshot) float64 {
	equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
	if s.PositionDebtRaw <= 0 {
		return 1
	}
	if equity <= 0 {
		return 0
	}
	return float64(s.PositionCollateralValueRaw) / float64(equity)
}

func currentLeverageBand(s Snapshot) float64 {
	if s.PositionDebtRaw <= 0 {
		return 1
	}
	best, bestDiff := 1.5, int64(math.MaxInt64)
	for _, level := range leverageLevels[1:] {
		diff := s.LTVBPS - leverageLevelLTVBPS(level)
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			best, bestDiff = level, diff
		}
	}
	return best
}

// nextLiveLeverageLevel is the watch-only option-1 step, applied live: at most
// one level per decision, with the same up/down thresholds and gap.
func nextLiveLeverageLevel(current float64, spreadAt func(float64) (float64, bool)) float64 {
	return nextLeverageLevel(leverageWatchOptions["1"], current, spreadAt)
}

// leverageUpMinimumSpread is the live rule's spread an up move to level needs
// (1% to 1.5x, 2% to 1.75x); false for a level the rule never moves up to.
func leverageUpMinimumSpread(level float64) (float64, bool) {
	for _, s := range leverageWatchOptions["1"] {
		if s.to == level && s.to > s.from {
			return s.min, true
		}
	}
	return 0, false
}

// leverageBorrowSpread is the lane's token yield minus the borrow APY once
// debtRaw more raw debt units are borrowed from its debt reserve.
func leverageBorrowSpread(m LaneEconomics, debtRaw float64) (float64, bool) {
	apr, err := projectedBorrowAPR(m, debtRaw)
	if err != nil || !finite(apr) {
		return 0, false
	}
	spread := m.NativeAPY + m.SupplyAPY - math.Expm1(apr)
	return spread, finite(spread)
}

// leverageUpAllowed applies the live spread rule to borrowing debtRaw more raw
// debt units to reach level: the same test the leverage decision and the
// selector's entry quote apply.
func leverageUpAllowed(m LaneEconomics, level, debtRaw float64) (float64, bool) {
	minimum, ok := leverageUpMinimumSpread(level)
	if !ok {
		return 0, false
	}
	spread, ok := leverageBorrowSpread(m, debtRaw)
	return spread, ok && spread >= minimum
}
