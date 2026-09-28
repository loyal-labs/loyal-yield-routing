package backyardrwa

import "math/big"

// B2 up moves. A stored target above the position's level borrows one level
// up under reason leverage_up, sized from the target (never a selector entry
// quote): debt after the whole loop = (level-1) x equity, i.e. borrow
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
			if level > s.LeverageTargetLevel {
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
	return OpenRouteStep, leverageUpReason, int64(level * 100)
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
	return OpenRouteStep, leverageUpReason, int64(level * 100), true
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
func leverageUpBypassesEntryFence(request any, journaledReason string, unwinding bool, operationLane string, rawTarget []byte) (bool, error) {
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
		targetErr != nil || target == nil || target.Lane != operationLane || target.Level <= 1 {
		return false, budgetHold("leverage_up_authority_mismatch")
	}
	return true, nil
}

// leverageLoopInProgress: an AUTO/OnRe position with debt whose borrowed or
// collateral cash still has to finish the loop.
func leverageLoopInProgress(s Snapshot) bool {
	return leverageLane(s.RouteLane) && s.HasPosition && s.PositionDebtRaw > 0 && (debtCashRaw(s) > 0 || s.CollateralIdleRaw > 0)
}
