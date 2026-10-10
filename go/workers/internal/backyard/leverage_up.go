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
	// leverageMaxLiveLevel caps live levels. 1.75x needs the multi-cycle exit
	// (one release at 55% frees ~0.39 of 0.75 debt), priced by
	// leverage_exit_pricer.go and decided by exitCycleStep.
	leverageMaxLiveLevel = 1.75
)

// leverageUpLevel is the next level above the position (at most one step),
// when the stored target allows it; 0 means no up move.
func leverageUpLevel(s Snapshot) float64 {
	if !leverageLane(s.RouteLane) || s.LeverageTargetLevel <= 1 {
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

// leverageBorrowStep is the B2 borrow step for a settled funded AUTO/OnRe position
// with no working cash beside it. It runs before the selector-entry pause
// (an up move adds to the current lane; it is not a new entry) and replaces
// both the installed first-loop borrow and the single-loop ready hold.
func leverageBorrowStep(s Snapshot, hard int64) (Action, string, int64, bool) {
	if !leverageLane(s.RouteLane) || !s.HasPosition || s.PositionCollateralRaw <= 0 || s.DebtIdleRaw != 0 || s.SquadsIdleRaw != 0 ||
		(s.CollateralIdleRaw > 0 && (s.MinimumCollateralDepositRaw <= 0 || s.CollateralIdleRaw >= s.MinimumCollateralDepositRaw)) ||
		!s.PolicyReady || !s.ExitBuildable || hard <= TargetLTVBPS {
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
		return Hold, "leverage_capacity_settled", 0
	}
	return OpenRouteStep, leverageUpReason, int64(receive)
}

// leverageUpStepWithDebt is the 1.5x->1.75x move on a settled leveraged
// position; ok=false keeps the installed single_loop_position_ready hold.
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
		return "", "", 0, false
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

// leverageUpProjectionWithinCaps checks the simulated post-borrow position:
// instant LTV <= 50%, and <= 45% once the received debt is redeposited as
// collateral after a 1% swap loss.
func leverageUpProjectionWithinCaps(p phase3KaminoProjection, route RuntimeRoute, receive uint64) error {
	o, err := decodeKaminoObligation(accountAt(p.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return err
	}
	collateral, err := decodeKaminoReserve(accountAt(p.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return err
	}
	debt, err := decodeKaminoReserve(accountAt(p.Accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return err
	}
	redeemable, err := collateral.redeemLiquidityRaw(o.collateralDepositedRaw)
	if err != nil {
		return err
	}
	position := KaminoPosition{CollateralDepositedRaw: o.collateralDepositedRaw, RedeemablePrimeRaw: redeemable, DebtRaw: o.debtRaw,
		CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals, CollateralPriceSF: collateral.marketPriceSF, DebtPriceSF: debt.marketPriceSF}
	instant, err := observedLTVBPS(position)
	if err != nil || instant > TargetLTVBPS {
		return budgetHold("leverage_up_instant_ltv_above_50")
	}
	// Received debt as collateral units at 99%: receive*pD*10^cd / (pC*10^dd).
	extra := new(big.Int).Mul(new(big.Int).SetUint64(receive), littleInt(debt.marketPriceSF[:]))
	extra.Mul(extra, big.NewInt(10_000-leverageSwapLossBPS))
	extra.Mul(extra, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(collateral.mintDecimals)), nil))
	extra.Quo(extra, new(big.Int).Mul(new(big.Int).Mul(littleInt(collateral.marketPriceSF[:]), big.NewInt(10_000)), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(debt.mintDecimals)), nil)))
	if !extra.IsUint64() {
		return budgetHold("leverage_up_projection_overflow")
	}
	position.RedeemablePrimeRaw += extra.Uint64()
	after, err := observedLTVBPS(position)
	if err != nil || after > leverageMaxLTVBPS {
		return budgetHold("leverage_up_loop_ltv_above_45")
	}
	return nil
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
	r, ok := request.(KaminoPrimeUSDCRequest)
	if !ok {
		return false, budgetHold("leverage_up_authority_mismatch")
	}
	_, leg, err := kaminoPrimeUSDCInstruction(r)
	target, targetErr := decodeLeverageTarget(rawTarget)
	if err != nil || leg != kaminoLegBorrow || unwinding || r.RouteLane != operationLane || !leverageLane(operationLane) ||
		targetErr != nil || target == nil || target.Lane != operationLane || target.Level <= 1 || target.BorrowRaw < leverageMinimumBorrowRaw || r.AmountRaw != target.BorrowRaw || len(operationID) != 1 || target.OperationID != operationID[0] || target.OperationID == "" {
		return false, budgetHold("leverage_up_authority_mismatch")
	}
	return true, nil
}

// leverageLoopInProgress: an AUTO/OnRe position with debt whose borrowed or
// collateral cash still has to finish the loop.
func leverageLoopInProgress(s Snapshot) bool {
	return leverageLane(s.RouteLane) && s.HasPosition && s.PositionDebtRaw > 0 && (debtCashRaw(s) > 0 || s.CollateralIdleRaw > 0)
}

// B2 down move to 1x: a stored 1x target below a leveraged AUTO/OnRe
// position repays it in full through the existing release -> funding swap ->
// full payoff legs (the withdrawal-shaped chain, priced and admitted the same
// way), under its own reasons. Leftover debt/USDC cash then returns to the
// position through the plan B3 residue and top-up legs.
const (
	leverageDownReleaseReason = "leverage_down_release"
	leverageDownSwapReason    = "leverage_down_swap"
	leverageDownRepayReason   = "leverage_down_repay"
)

func leverageDownPending(s Snapshot) bool {
	return leverageLane(s.RouteLane) && s.LeverageTargetLevel == 1 && s.HasPosition && s.PositionDebtRaw > 0
}

func leverageDownStep(s Snapshot) (Action, string, int64, bool) {
	if !leverageDownPending(s) {
		return "", "", 0, false
	}
	payoff := max(s.PositionDebtRaw, s.PayoffDebtRaw)
	if debtCashRaw(s) >= payoff {
		return DeleverRouteStep, leverageDownRepayReason, s.PositionDebtRaw, true
	}
	if action, reason, amount, ok := exitCycleStep(s); ok {
		return action, reason, amount, true
	}
	switch action, amount := payoffFundingSource(s, uint64(payoff)); action {
	case SwapCollateralToDebtStep:
		return action, leverageDownSwapReason, amount, true
	case "":
		// The builder sizes the safe release; 1 is only the state marker.
		return DeleverRouteStep, leverageDownReleaseReason, 1, true
	default:
		// Bridge USDC beside a down move is not part of this chain.
		return Hold, "leverage_down_unexpected_cash", 0, true
	}
}

// Reason groups shared by the withdrawal chain and the B2 down move.
func repaymentReleaseReason(reason string) bool {
	return reason == "withdrawal_release_repayment_collateral" || reason == leverageDownReleaseReason || reason == leverageDownPartialReleaseReason
}

// exitCycleStep is the B2 1.75x exit cycle inside every full-payoff chain
// (withdrawal, unwind/SWITCH, down move): when debt cash is below the payoff
// and no more collateral can be released safely (the position already sits
// at the release ceiling after a release+swap), repay what the cash covers
// instead of releasing again. ok=false keeps the installed next leg.
// The caller has already run hard-LTV safety, which preempts every cycle.
func exitCycleStep(s Snapshot) (Action, string, int64, bool) {
	if !leverageLane(s.RouteLane) || s.PositionDebtRaw <= 1 || s.LTVBPS < leverageExitCycleLTVBPS {
		return "", "", 0, false
	}
	payoff := max(s.PositionDebtRaw, s.PayoffDebtRaw)
	cash := debtCashRaw(s)
	if cash < 0 || cash >= payoff {
		return "", "", 0, false
	}
	if s.CollateralIdleRaw > 0 {
		// Released collateral that cannot fund the whole payoff is swapped
		// for a partial repay; enough collateral keeps the full-funding swap.
		if _, amount := payoffFundingSource(s, uint64(payoff)); amount > 0 {
			return "", "", 0, false
		}
		return SwapCollateralToDebtStep, exitCycleSwapReason, s.CollateralIdleRaw, true
	}
	// Leave either no debt (the installed full payoff) or a remainder at or
	// above the residual floor, never a dust debt a later payoff or release
	// could trip on.
	// The decision carries the (stable) debt cash; prepare sizes the exact
	// repay with exitPartialRepayWireRaw.
	if min(cash, s.PositionDebtRaw-exitCycleResidualFloor(s.PositionDebtRaw)) <= 0 {
		return "", "", 0, false
	}
	return DeleverRouteStep, exitPartialRepayReason, cash, true
}

// exitCycleSwapReason swaps a cycle's released collateral to debt; its
// admission prices the rest of the multi-cycle exit.
const exitCycleSwapReason = "exit_cycle_swap"

// exitCycleResidualFloor is the smallest debt a partial repay leaves: 10% of
// the debt (at least 1 raw). A cycle repays about half of a 1.75x debt, so
// the floor never binds there; it stops cash that is just short of the
// payoff from leaving dust debt, which the installed release -> full payoff
// then clears instead.
// ponytail: proportional floor; KLend's repay path has no minimum-debt check
// in the worker's model (the market minimum-remaining value applies to
// collateral withdrawals, which the release sizing already enforces).
func exitCycleResidualFloor(debt int64) int64 {
	return max(debt/10, 1)
}

// A release leaves the position at the release ceiling (55%). Debt cash
// that cannot pay off the debt at or above this LTV is a cycle's funding.
const leverageExitCycleLTVBPS int64 = 5000

// B2 1.75x -> 1.5x down move: one exit cycle sized to land at the 1.5x LTV.
// Release R = (3D - C)/2 of value (so (D-R)/(C-R) = 1/3), swap it, repay
// the proceeds (never the whole debt), then stop: the position snaps to
// 1.5x. The swap and repay reuse the exit-cycle legs and admissions.
const leverageDownPartialReleaseReason = "leverage_down_partial_release"

// leverageDownPartialEnabled keeps the step inert until live levels reach
// 1.75x (a drifted 1.5x position is not de-levered by this path).
const leverageDownPartialEnabled = leverageMaxLiveLevel > 1.5

func leverageDownPartialStep(s Snapshot) (Action, string, int64, bool) {
	return leverageDownPartialStepAt(s, leverageDownPartialEnabled)
}

func leverageDownPartialStepAt(s Snapshot, enabled bool) (Action, string, int64, bool) {
	if !enabled || !leverageLane(s.RouteLane) || s.LeverageTargetLevel != 1.5 || !s.HasPosition || s.PositionDebtRaw <= 1 ||
		s.PositionCollateralValueRaw <= 0 || s.PositionDebtValueRaw <= 0 {
		return "", "", 0, false
	}
	cash := debtCashRaw(s)
	if cash < 0 {
		return "", "", 0, false
	}
	// Finish a started cycle first: swap released collateral, repay cash.
	// Only while the move is running: position still above the 1.5x band.
	// (an unknown LTV never starts or continues the move).
	if s.LTVBPS <= leverageLevelLTVBPS(1.5)+leverageUpNearBPS {
		return "", "", 0, false
	}
	if s.CollateralIdleRaw > 0 {
		return SwapCollateralToDebtStep, exitCycleSwapReason, s.CollateralIdleRaw, true
	}
	if cash > 0 {
		if min(cash, s.PositionDebtRaw-exitCycleResidualFloor(s.PositionDebtRaw)) <= 0 {
			return "", "", 0, false
		}
		return DeleverRouteStep, exitPartialRepayReason, cash, true
	}
	if currentLeverageBand(s) != 1.75 || s.PositionCollateralRaw <= 0 {
		return "", "", 0, false
	}
	// The decision carries a marker (1, stable); prepare sizes the release
	// from its own snapshot with leverageDownPartialReceipts.
	if leverageDownPartialReceipts(s) <= 0 {
		return "", "", 0, false
	}
	return DeleverRouteStep, leverageDownPartialReleaseReason, 1, true
}

// leverageDownPartialReceipts: R = (3D - C)/2 of value, as receipts.
func leverageDownPartialReceipts(s Snapshot) int64 {
	releaseValue := (3*s.PositionDebtValueRaw - s.PositionCollateralValueRaw) / 2
	if releaseValue <= 0 || s.PositionCollateralValueRaw <= 0 {
		return 0
	}
	receipts := new(big.Int).Mul(big.NewInt(s.PositionCollateralRaw), big.NewInt(releaseValue))
	receipts.Quo(receipts, big.NewInt(s.PositionCollateralValueRaw))
	if !receipts.IsInt64() || receipts.Sign() <= 0 || receipts.Int64() >= s.PositionCollateralRaw {
		return 0
	}
	return receipts.Int64()
}
