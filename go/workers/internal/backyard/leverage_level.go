package backyard

import "math"

// leverageRung is one level of the ladder and the spread rule at it: an up
// move to level needs the spread at the step's own borrow (token yield minus
// the borrow APY once the step is borrowed) at or above up; a lane at level
// steps down one level when its current spread falls under down.
type leverageRung struct {
	level, up, down float64
}

// leverageLadder is the one list of levels, in APY points (0.01 = 1 pt).
var leverageLadder = [...]leverageRung{
	{level: 1},
	{level: 1.5, up: 0.01, down: 0},
	{level: 1.75, up: 0.02, down: 0.01},
	{level: 2, up: 0.025, down: 0.015},
	{level: 2.5, up: 0.025, down: 0.015},
	{level: 3, up: 0.025, down: 0.015},
}

var firstLeveredLevel = leverageLadder[1].level

func leverageRungIndex(level float64) (int, bool) {
	for i, r := range leverageLadder {
		if r.level == level {
			return i, true
		}
	}
	return 0, false
}

func leverageLevelLTVBPS(level float64) int64 {
	if level <= 1 {
		return 0
	}
	return int64(math.Floor((1 - 1/level) * 10_000))
}

func laneMaxLevel(lane string) float64 {
	l, ok := findEarnLane(lane)
	if !ok {
		return 1
	}
	return l.maxLevel
}

func laneCeilingLTVBPS(lane string) int64 {
	return leverageLevelLTVBPS(laneMaxLevel(lane))
}

// heldLevel is the level a lane's stored setting holds it at: a stored 1
// holds 1.5 (never debt 0), 0 (none stored) leaves the position as it is,
// and nothing sits above the lane's cap.
func heldLevel(lane string, stored float64) float64 {
	if stored == 1 {
		stored = firstLeveredLevel
	}
	return min(stored, laneMaxLevel(lane))
}

func currentLeverageBand(s Snapshot) float64 {
	if s.PositionDebtRaw <= 0 {
		return 1
	}
	best, bestDiff := firstLeveredLevel, int64(math.MaxInt64)
	for _, r := range leverageLadder[1:] {
		diff := s.LTVBPS - leverageLevelLTVBPS(r.level)
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			best, bestDiff = r.level, diff
		}
	}
	return best
}

func nextLeverageLevel(current, maxLevel float64, spreadAt func(float64) (float64, bool)) float64 {
	i, ok := leverageRungIndex(current)
	if !ok {
		return current
	}
	if i+1 < len(leverageLadder) && leverageLadder[i+1].level <= maxLevel {
		if spread, known := spreadAt(leverageLadder[i+1].level); known && spread >= leverageLadder[i+1].up {
			return leverageLadder[i+1].level
		}
	}
	if i > 0 {
		if spread, known := spreadAt(current); known && spread < leverageLadder[i].down {
			return leverageLadder[i-1].level
		}
	}
	return current
}

func leverageBorrowSpread(m LaneEconomics, debtRaw float64) (float64, bool) {
	apr, err := projectedBorrowAPR(m, debtRaw)
	if err != nil || !finite(apr) {
		return 0, false
	}
	spread := m.NativeAPY + m.SupplyAPY - math.Expm1(apr)
	return spread, finite(spread)
}

func leverageUpAllowed(m LaneEconomics, level, debtRaw float64) (float64, bool) {
	i, ok := leverageRungIndex(level)
	if !ok || i == 0 {
		return 0, false
	}
	spread, ok := leverageBorrowSpread(m, debtRaw)
	return spread, ok && spread >= leverageLadder[i].up
}

func settledLevel(m LaneEconomics, from float64, debtTo func(level float64) (float64, bool), pays func(level, debt float64) bool) float64 {
	level := from
	for _, r := range leverageLadder[1:] {
		if r.level <= from {
			continue
		}
		if r.level > laneMaxLevel(m.Lane) {
			break
		}
		debt, ok := debtTo(r.level)
		if !ok {
			break
		}
		if _, up := leverageUpAllowed(m, r.level, debt); !up {
			break
		}
		if !pays(r.level, debt) {
			break
		}
		level = r.level
	}
	return level
}

func sizeSettledLevel(m LaneEconomics, equityRaw int64) float64 {
	return settledLevel(m, 1, func(level float64) (float64, bool) { return levelDebtRaw(m, level, equityRaw) }, spreadRuleOnly)
}

func spreadRuleOnly(float64, float64) bool { return true }

func economicsOf(markets []LaneEconomics, lane string) *LaneEconomics {
	var out *LaneEconomics
	for i := range markets {
		if markets[i].Lane == lane {
			out = &markets[i]
		}
	}
	return out
}

func heldRungDebt(s Snapshot, level float64) (float64, bool) {
	raw := leverageDesiredRaw(s, level)
	if raw < leverageMinimumBorrowRaw {
		return 0, false
	}
	fee, err := kaminoBorrowFeeAtRate(s.BorrowFeeRate, raw)
	if err != nil {
		return 0, false
	}
	return float64(leverageRateDebtRaw(s, raw+fee)), true
}

func heldFromLevel(s Snapshot) float64 {
	return min(currentLeverageBand(s), laneMaxLevel(s.RouteLane))
}
