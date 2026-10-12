package backyard

import "math/big"

// B2 up moves. The desired target bounds a same-batch capacity-sized loan.
// One economically approved raw receive is journal-bound to one operation.
// The uncapped desired amount is
// (L-1)*C - L*D in debt units (1x->1.5x: C/2; 1.5x->1.75x: equity/4). The
// borrow plus its fee never takes the instant LTV above 50%, and the loop
// must land at or below 45% even after a 1% swap loss.
const (
	leverageUpReason = "leverage_up"
	// A position within 2 LTV points of a level counts as at that level.
	leverageUpNearBPS = 200
	// Smallest leverage_up borrow in debt raw units (six-decimal stables).
	leverageMinimumBorrowRaw = 10_000_000
	leverageSwapLossBPS      = 100
	// leverageMaxLiveLevel caps live levels.
	leverageMaxLiveLevel = 1.75
)

// leverageUpLevel is the next level above the position (at most one step),
// when the stored target allows it; 0 means no up move.
func leverageUpLevel(s Snapshot) float64 {
	if !earnActiveLane(s.RouteLane) || s.LeverageTargetLevel <= 1 {
		return 0
	}
	ltv := int64(0)
	if s.PositionDebtRaw > 0 {
		if s.LTVBPS <= 0 {
			return 0 // debt without an observed LTV: never lever up
		}
		ltv = s.LTVBPS
	}
	for _, level := range leverageLevels[1:] {
		if leverageLevelLTVBPS(level) > ltv+leverageUpNearBPS {
			if level > s.LeverageTargetLevel || level > leverageMaxLiveLevel {
				return 0
			}
			return level
		}
	}
	return 0
}

// leverageBorrowStep is the B2 up step (an atomic borrow, conversion and
// deposit) for a funded active registry position. It runs before the
// selector-entry pause (an up move adds to the current lane; it is not a new
// entry). Collateral residue beside the position needs no leg of its own: the
// step deposits it with the conversion's output.
func leverageBorrowStep(s Snapshot, hard int64) (Action, string, int64, bool) {
	if !earnActiveLane(s.RouteLane) || !s.HasPosition || s.PositionCollateralRaw <= 0 || hard <= TargetLTVBPS {
		return "", "", 0, false
	}
	if s.PositionDebtRaw == 0 {
		action, reason, amount := leverageDebtFreeStep(s)
		return action, reason, amount, true
	}
	return leverageUpStepWithDebt(s)
}

// leverageDebtFreeStep replaces the installed first-loop borrow on AUTO and
// OnRe: a funded debt-free position borrows only toward a stored target.
func leverageDebtFreeStep(s Snapshot) (Action, string, int64) {
	switch {
	case s.BorrowUtilizationBlocked:
		return Hold, "debt_reserve_utilization_blocks_borrow", 0
	case s.LeverageTargetLevel == 0:
		// Never the stored selector entry quote (live 2026-09-28: a 09-26
		// quote would have borrowed $189.87 against $1,676, ~1.11x).
		return Hold, "leverage_target_required", 0
	case s.LeverageTargetLevel == 1:
		return Hold, "leverage_target_1x", 0
	}
	level := leverageUpLevel(s)
	if level == 0 {
		return Hold, "leverage_target_required", 0
	}
	receive := leverageBorrowReceive(s, level)
	if receive < leverageMinimumBorrowRaw {
		return Hold, leverageUpHoldReason(s), 0
	}
	return OpenRouteStep, leverageUpReason, int64(receive)
}

// leverageUpHoldReason names why a position below its stored target does not
// borrow this tick: no live approval from the selector's economics, or no
// capacity for the minimum borrow.
func leverageUpHoldReason(s Snapshot) string {
	if !leverageApprovalLive(s) {
		return "leverage_up_awaiting_approval"
	}
	return "leverage_up_no_borrow_room"
}

// leverageUpStepWithDebt is the up move on a settled leveraged position below
// its stored target; ok=false (target reached) keeps the installed
// single_loop_position_ready hold.
func leverageUpStepWithDebt(s Snapshot) (Action, string, int64, bool) {
	level := leverageUpLevel(s)
	if level == 0 {
		return "", "", 0, false
	}
	if s.BorrowUtilizationBlocked {
		return Hold, "debt_reserve_utilization_blocks_borrow", 0, true
	}
	receive := leverageBorrowReceive(s, level)
	if receive < leverageMinimumBorrowRaw {
		return Hold, leverageUpHoldReason(s), 0, true
	}
	return OpenRouteStep, leverageUpReason, int64(receive), true
}

// leverageUpBorrowRaw sizes the borrow toward levelHundredths (150 or 175)
// before the fee cap; see leverageUpCapFee.
func (p KaminoPosition) leverageUpBorrowRaw(levelHundredths int64) (uint64, error) {
	if levelHundredths != 150 && levelHundredths != 175 {
		return 0, budgetHold("invalid_leverage_up_level")
	}
	half, err := p.targetLTVBorrowRaw()
	if err != nil {
		return 0, err
	}
	c := new(big.Int).Lsh(new(big.Int).SetUint64(half), 1)
	b := new(big.Int).Mul(c, big.NewInt(levelHundredths-100))
	b.Sub(b, new(big.Int).Mul(new(big.Int).SetUint64(p.DebtRaw), big.NewInt(levelHundredths)))
	b.Quo(b, big.NewInt(100))
	if b.Sign() <= 0 || !b.IsUint64() {
		return 0, budgetHold("leverage_up_level_already_reached")
	}
	return b.Uint64(), nil
}

// leverageUpCapFee keeps debt + receive + fee within 50% of collateral and
// refuses a borrow below the minimum.
func leverageUpCapFee(p KaminoPosition, receive uint64, fee func(uint64) (uint64, error)) (uint64, error) {
	half, err := p.targetLTVBorrowRaw()
	if err != nil {
		return 0, err
	}
	if half <= p.DebtRaw {
		return 0, budgetHold("leverage_up_no_instant_ltv_room")
	}
	// 0.1% margin keeps the instant LTV under 50% across the
	// prepare-to-simulation price refresh.
	room := half - p.DebtRaw - half/1000
	if room == 0 || room > half {
		return 0, budgetHold("leverage_up_no_instant_ltv_room")
	}
	receive = min(receive, room)
	f, err := fee(receive)
	if err != nil {
		return 0, err
	}
	if receive+f > room {
		if f >= room {
			return 0, budgetHold("leverage_up_no_instant_ltv_room")
		}
		receive = room - f
	}
	if receive < leverageMinimumBorrowRaw {
		return 0, budgetHold("leverage_up_below_minimum_borrow")
	}
	return receive, nil
}

// borrowDebtMatches compares a re-read obligation debt with the snapshot's:
// exact for a debt-free position (installed behaviour), within 0.1% + 1 raw
// of accrual otherwise.
func borrowDebtMatches(observed, snapshot uint64) bool {
	if snapshot == 0 {
		return observed == 0
	}
	tolerance := snapshot/1000 + 1
	return observed+tolerance >= snapshot && observed <= snapshot+tolerance
}

// leverageUpBypassesEntryFence: a leverage_up borrow is authorized by the
// stored level target of the same lane, not by a selector entry. Every other
// borrow keeps the entry fence.
func leverageUpBypassesEntryFence(request any, journaledReason string, unwinding bool, operationLane string, rawTarget []byte, operationID ...string) (bool, error) {
	if journaledReason != leverageUpReason {
		return false, nil
	}
	r, ok := stepBorrow(request)
	if !ok {
		return false, budgetHold("leverage_up_authority_mismatch")
	}
	_, leg, err := kaminoPrimeUSDCInstruction(r)
	target, targetErr := decodeLeverageTarget(rawTarget)
	if err != nil || leg != kaminoLegBorrow || unwinding || r.RouteLane != operationLane || !earnActiveLane(operationLane) ||
		targetErr != nil || target == nil || target.Lane != operationLane || target.Level <= 1 || target.BorrowRaw < leverageMinimumBorrowRaw || r.AmountRaw != target.BorrowRaw || len(operationID) != 1 || target.OperationID != operationID[0] || target.OperationID == "" {
		return false, budgetHold("leverage_up_authority_mismatch")
	}
	return true, nil
}

// B2 down moves, each an atomic down step (withdraw, convert, repay): to 1x
// (a stored 1x target, or a term lane it cannot keep alive, decide_term.go)
// the step repays the whole debt, sized on its payoff bound, and repeats while
// a withdrawal limit leaves debt; 1.75x -> 1.5x repays to the 1.5x LTV.
func leverageDownPending(s Snapshot) bool {
	return earnActiveLane(s.RouteLane) && (s.LeverageTargetLevel == 1 || termDeleverDue(s)) && s.HasPosition && s.PositionDebtRaw > 0
}

func leverageDownStep(s Snapshot) (Action, string, int64, bool) {
	if !leverageDownPending(s) {
		return "", "", 0, false
	}
	return DeleverRouteStep, leverageDownReason, stepDownMarker, true
}

// leverageDownPartialStep is the 1.75x -> 1.5x step: a stored 1.5x target
// below a position above the 1.5x band (an unknown LTV never moves).
func leverageDownPartialStep(s Snapshot) (Action, string, int64, bool) {
	band := leverageLevelLTVBPS(1.5)
	if !earnActiveLane(s.RouteLane) || s.LeverageTargetLevel != 1.5 || !s.HasPosition || s.PositionDebtRaw <= 0 || s.LTVBPS <= band+leverageUpNearBPS {
		return "", "", 0, false
	}
	if stepDebtToLTVRaw(s, band) <= 0 {
		return "", "", 0, false
	}
	return DeleverRouteStep, leverageDownPartialReason, stepDownMarker, true
}

// hardLTVTargetBPS is the LTV a hard-LTV step brings the position to: its
// stored level's, between 1.5x and the live cap.
func hardLTVTargetBPS(s Snapshot) int64 {
	return leverageLevelLTVBPS(min(max(s.LeverageTargetLevel, 1.5), leverageMaxLiveLevel))
}
