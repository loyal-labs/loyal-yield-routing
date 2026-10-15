package backyard

import "math"

// B2 option 1 (docs/plans/b2-variable-leverage-design.md): the leverage
// levels a lane may hold and the one-step spread rule that moves it between
// them. This file holds the pure level logic; the durable target and the
// moves are built on it.

// leverageRung is one level of the ladder and the spread rule at it: an up
// move to level needs the spread at the step's own borrow (token yield minus
// the borrow APY once the step is borrowed) at or above up; a lane at level
// steps down one level when its current spread falls under down.
type leverageRung struct {
	level, up, down float64
}

// leverageLadder is the one list of levels (owner decision 2026-10-14), in
// APY points (0.01 = 1 pt). Each down sits at least 1 point under its up, so
// a lane never flip-flops. A lane climbs no higher than its cap
// (earnLane.maxLevel). The migration's CHECK on backyard_lane_levels lists
// the same levels.
var leverageLadder = [...]leverageRung{
	{level: 1},
	{level: 1.5, up: 0.01, down: 0},
	{level: 1.75, up: 0.02, down: 0.01},
	{level: 2, up: 0.025, down: 0.015},
	{level: 2.5, up: 0.025, down: 0.015},
	{level: 3, up: 0.025, down: 0.015},
}

// leverageRungIndex is level's place on the ladder; false off it.
func leverageRungIndex(level float64) (int, bool) {
	for i, r := range leverageLadder {
		if r.level == level {
			return i, true
		}
	}
	return 0, false
}

// leverageLevelLTVBPS is the LTV a level sits at after its last redeposit:
// 1 - 1/level (1.5x = 33.33%, 1.75x = 42.86%, 2x = 50%, 2.5x = 60%,
// 3x = 66.67%).
func leverageLevelLTVBPS(level float64) int64 {
	if level <= 1 {
		return 0
	}
	return int64(math.Floor((1 - 1/level) * 10_000))
}

// laneMaxLevel is the lane's cap: the highest level it may hold
// (route_catalog.go); 1 for a lane outside the registry.
func laneMaxLevel(lane string) float64 {
	l, ok := findEarnLane(lane)
	if !ok {
		return 1
	}
	return l.maxLevel
}

// laneCeilingLTVBPS is the most a lane borrows: its cap's LTV. Every up step
// lands within it, and the lane's hard LTV must sit above it.
func laneCeilingLTVBPS(lane string) int64 {
	return leverageLevelLTVBPS(laneMaxLevel(lane))
}

// heldLevel is the level a lane's stored setting holds it at: a stored 1
// holds 1.5 (never debt 0), 0 (none stored) leaves the position as it is,
// and nothing sits above the lane's cap.
func heldLevel(lane string, stored float64) float64 {
	if stored == 1 {
		stored = 1.5
	}
	return min(stored, laneMaxLevel(lane))
}

func currentLeverageBand(s Snapshot) float64 {
	if s.PositionDebtRaw <= 0 {
		return 1
	}
	best, bestDiff := 1.5, int64(math.MaxInt64)
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

// nextLeverageLevel applies the spread rule from current, at most one level
// per decision and never above maxLevel: up when the spread at the next
// level reaches its up threshold, else down when the spread at current falls
// under its down threshold.
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

// leverageUpAllowed applies the spread rule to borrowing debtRaw more raw
// debt units to reach level: the same test the leverage decision and the
// selector's entry quote apply.
func leverageUpAllowed(m LaneEconomics, level, debtRaw float64) (float64, bool) {
	i, ok := leverageRungIndex(level)
	if !ok || i == 0 {
		return 0, false
	}
	spread, ok := leverageBorrowSpread(m, debtRaw)
	return spread, ok && spread >= leverageLadder[i].up
}
