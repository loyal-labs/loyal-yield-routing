package backyard

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
)

// kaminoAdditionalDebtRoom is a fee-inclusive ADDITIONAL liability limit.
// Existing obligation debt is never subtracted from this room or treated as a
// target. All reserve limits and the chain clock must come from one batch.
func kaminoAdditionalDebtRoom(accounts []ConfirmedAccount, route RuntimeRoute) (uint64, error) {
	collateralAccount, debtAccount := accountAt(accounts, route.Kamino.CollateralReserve), accountAt(accounts, route.Kamino.DebtReserve)
	collateral, err := decodeKaminoReserve(collateralAccount, route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return 0, err
	}
	debt, err := decodeKaminoReserve(debtAccount, route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return 0, err
	}
	clock := accountAt(accounts, budgetClockAddress)
	if clock.Owner != "Sysvar1111111111111111111111111111111111111" || clock.Executable || len(clock.Data) != 40 {
		return 0, budgetHold("borrow_capacity_clock_invalid")
	}
	slot := int64(binary.LittleEndian.Uint64(clock.Data[:8]))
	if err := validateKaminoReserveHealth(slot, false, decodedKaminoObligation{}, collateral, debt); err != nil {
		return 0, err
	}
	if err := validateKaminoOracleAge(clockUnixTimestamp(accounts), collateral, debt); err != nil {
		return 0, err
	}
	obligation := accountAt(accounts, route.Kamino.Obligation)
	if obligation.Address != route.Kamino.Obligation {
		return 0, fmt.Errorf("pair_capacity_obligation_missing")
	}
	if obligation.Lamports != 0 {
		if _, err := decodeKaminoObligation(obligation, route.Kamino); err != nil {
			return 0, err
		}
		if obligation.Data[kaminoObligationElevationGroupOffset] != 0 || !allZero(obligation.Data[2288:2320]) {
			return 0, nil
		}
	}
	if collateralAccount.Data[kaminoDisableCrossCollateralOffset] != 0 ||
		collateralAccount.Data[kaminoReserveStatusOffset] != 0 || collateralAccount.Data[kaminoReserveEmergencyModeOffset] != 0 {
		return 0, nil
	}
	return kaminoDebtReserveRoom(debtAccount, debt, clockUnixTimestamp(accounts))
}

// kaminoDebtReserveRoom is the debt reserve's own share of the additional
// debt room: what any obligation could still borrow from it at chain time
// now (borrowing disabled or unmodelled, queued liquidity, weighted debt,
// the outside-elevation-group and borrow limits, the utilization limit, the
// daily net-borrow cap, available liquidity, status). It reads one account,
// so the account view can recompute it on every update of the reserve.
func kaminoDebtReserveRoom(debtAccount ConfirmedAccount, debt decodedKaminoReserve, now int64) (uint64, error) {
	if debtAccount.Data[kaminoReserveConfigOffset+7] != 0 || !allZero(debtAccount.Data[kaminoReserveConfigOffset+920:kaminoReserveConfigOffset+936]) {
		return 0, nil
	}
	// Newer KLend consumes the first trailing padding u64 for queued collateral.
	// Until the pinned decoder models freely available liquidity, decline new
	// borrowing when any liquidity is queued. Old zero-filled padding is safe.
	if binary.LittleEndian.Uint64(debtAccount.Data[kaminoQueuedCollateralOffset:]) != 0 {
		return 0, nil
	}
	factor := binary.LittleEndian.Uint64(debtAccount.Data[kaminoBorrowFactorOffset:])
	// KLend clamps the effective factor to at least 100%, including old
	// configurations whose serialized value is zero.
	factor = max(factor, 100)
	if factor != 100 {
		// Pilot risk observation uses unweighted LTV. A weighted debt lane needs
		// consistent risk/unwind admission before accepting new capital.
		return 0, nil
	}
	feeRate := binary.LittleEndian.Uint64(debtAccount.Data[kaminoReserveConfigOffset+40:])
	if feeRate >= uint64(1)<<60 {
		return 0, nil
	}
	outsideLimit := binary.LittleEndian.Uint64(debtAccount.Data[kaminoOutsideBorrowLimitOffset:])
	outsideUsed := binary.LittleEndian.Uint64(debtAccount.Data[kaminoOutsideBorrowCounterOffset:])
	if outsideUsed >= outsideLimit || debt.borrowedRaw >= debt.borrowLimitRaw {
		return 0, nil
	}
	utilization, err := utilizationBorrowHeadroomRaw(debt)
	if err != nil {
		return 0, err
	}
	daily, err := kaminoNetBorrowHeadroom(debtAccount.Data[kaminoDebtWithdrawalCapOffset:kaminoDebtWithdrawalCapOffset+32], now)
	if err != nil {
		return 0, err
	}
	available := binary.LittleEndian.Uint64(debtAccount.Data[224:232])
	debtRoom := min(available, outsideLimit-outsideUsed, debt.borrowLimitRaw-debt.borrowedRaw, utilization, daily)
	if debtAccount.Data[kaminoReserveStatusOffset] != 0 || debtAccount.Data[kaminoReserveEmergencyModeOffset] != 0 {
		return 0, nil
	}
	return min(debtRoom, math.MaxInt64), nil
}

// Fee-safe floor, including KLend's minimum-one rounding. Never overflows and
// never rounds up into the room. Exact fees are rechecked for the fixed wire.
func kaminoReceiveWithinRoom(room, rate uint64) uint64 {
	if room == 0 || rate >= uint64(1)<<60 {
		return 0
	}
	room = min(room, math.MaxInt64)
	if rate == 0 {
		return room
	}
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	n := new(big.Int).Mul(new(big.Int).SetUint64(room-1), one)
	n.Quo(n, new(big.Int).Add(one, new(big.Int).SetUint64(rate)))
	if n.Uint64() < 2 {
		return 0
	}
	return n.Uint64()
}

func capacitySizedBorrow(p KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute, level int64) (uint64, error) {
	room, err := kaminoAdditionalDebtRoom(accounts, route)
	if err != nil {
		return 0, err
	}
	rate := binary.LittleEndian.Uint64(accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+40:])
	receive := kaminoReceiveWithinRoom(room, rate)
	if receive < leverageMinimumBorrowRaw {
		return 0, nil
	}
	desired, err := p.leverageUpBorrowRaw(level)
	if err != nil {
		return 0, nil
	}
	depositRoom, err := unleveredEntryCapacityDebtRaw(p, accounts, route)
	if err != nil {
		return 0, err
	}
	return leverageStepWithinRisk(p, accounts, route, min(receive, desired, depositRoom), rate)
}

// leverageStepWithinRisk caps a receive at the lane's protocol collateral LTV
// (which can be stricter than pilot 50%) and at 50% instant LTV including the
// borrow fee; zero below the minimum borrow.
func leverageStepWithinRisk(p KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute, receive, rate uint64) (uint64, error) {
	half, err := p.targetLTVBorrowRaw()
	if err != nil {
		return 0, err
	}
	ltv := uint64(accountAt(accounts, route.Kamino.CollateralReserve).Data[kaminoLoanToValueOffset])
	protocol := new(big.Int).Mul(new(big.Int).SetUint64(half), new(big.Int).SetUint64(ltv))
	protocol.Quo(protocol, big.NewInt(50))
	if !protocol.IsUint64() || protocol.Uint64() <= p.DebtRaw {
		return 0, nil
	}
	receive = min(receive, kaminoReceiveWithinRoom(protocol.Uint64()-p.DebtRaw, rate))
	receive, err = leverageUpCapFee(p, receive, func(n uint64) (uint64, error) { return kaminoBorrowFeeAtRate(rate, n) })
	if err != nil {
		return 0, nil
	}
	return receive, nil
}

// desiredLeverageBorrow is the whole step to level the position wants, inside
// its own risk limits but not bounded by the debt reserve's room or the
// collateral reserve's deposit room. It is what an up move is priced at: no
// executable borrow is larger, so none pays a higher rate. The worker borrows
// this tick's capacity-sized part of it.
func desiredLeverageBorrow(p KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute, level int64) (uint64, error) {
	rate := binary.LittleEndian.Uint64(accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+40:])
	if rate >= uint64(1)<<60 {
		return 0, nil
	}
	desired, err := p.leverageUpBorrowRaw(level)
	if err != nil {
		return 0, nil
	}
	return leverageStepWithinRisk(p, accounts, route, desired, rate)
}

func applyBorrowCapacity(s *Snapshot, p KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute) {
	if !earnActiveLane(route.Lane) {
		return
	}
	room, err := kaminoAdditionalDebtRoom(accounts, route)
	if err != nil {
		return
	} // new borrowing holds; urgent exits remain live
	a, err := capacitySizedBorrow(p, accounts, route, 150)
	if err != nil {
		return
	}
	b, err := capacitySizedBorrow(p, accounts, route, 175)
	if err != nil {
		return
	}
	wanted150, err := desiredLeverageBorrow(p, accounts, route, 150)
	if err != nil {
		return
	}
	wanted175, err := desiredLeverageBorrow(p, accounts, route, 175)
	if err != nil {
		return
	}
	debtReserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return
	}
	utilizationHeadroom, err := utilizationBorrowHeadroomRaw(debtReserve)
	if err != nil {
		return
	}
	reference, config := route.Kamino.DebtReserve, route.Kamino
	if route.Kamino.DebtMint != bridgeUSDC {
		config, err = pinnedKaminoObservationConfig()
		if err != nil {
			return
		}
		reference = config.DebtReserve
	}
	usdc, err := decodeKaminoReserve(accountAt(accounts, reference), bridgeUSDC, config)
	if err != nil || usdc.mintDecimals != 6 || littleInt(usdc.marketPriceSF[:]).Sign() <= 0 {
		return
	}
	s.BorrowDebtPriceSF, s.BorrowUSDCPriceSF, s.BorrowDebtDecimals = p.DebtPriceSF, usdc.marketPriceSF, p.DebtDecimals
	s.BorrowFeeRate = binary.LittleEndian.Uint64(accountAt(accounts, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+40:])
	s.BorrowCapacityKnown, s.AdditionalDebtRoomRaw = true, room
	s.LeverageBorrow150Raw, s.LeverageBorrow175Raw = a, b
	s.LeverageDesired150Raw, s.LeverageDesired175Raw = wanted150, wanted175
	s.BorrowUtilizationHeadroomRaw = utilizationHeadroom
}

// leverageRateDebtRaw is the extra debt an up move is rate-priced at: the
// whole step, but never past the debt reserve's utilization borrow limit.
// KLend refuses a borrow that would cross it, so every executable part of the
// step borrows at or below the curve rate there.
func leverageRateDebtRaw(s Snapshot, debtRaw uint64) uint64 {
	return min(debtRaw, s.BorrowUtilizationHeadroomRaw)
}

// leverageDesiredRaw is the whole step to level (see desiredLeverageBorrow);
// zero when capacity is unknown.
func leverageDesiredRaw(s Snapshot, level float64) uint64 {
	if !s.BorrowCapacityKnown {
		return 0
	}
	if level == 1.5 {
		return s.LeverageDesired150Raw
	}
	if level == 1.75 {
		return s.LeverageDesired175Raw
	}
	return 0
}

func leverageBorrowCeiling(s Snapshot, level float64) uint64 {
	if !s.BorrowCapacityKnown {
		return 0
	}
	if level == 1.5 {
		return s.LeverageBorrow150Raw
	}
	if level == 1.75 {
		return s.LeverageBorrow175Raw
	}
	return 0
}

func validateCapacityBorrow(accounts []ConfirmedAccount, route RuntimeRoute, receive uint64) error {
	room, err := kaminoAdditionalDebtRoom(accounts, route)
	if err != nil {
		return err
	}
	fee, err := kaminoBorrowFee(accounts, route, receive)
	if err != nil {
		return err
	}
	if receive > room || fee > room-receive {
		return budgetHold("borrow_capacity_shrank")
	}
	// Reconstruct the CURRENT position, not the prepared observation or a
	// borrow-only simulation: neither proves fresh room for the redeposit.
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return err
	}
	debt, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return err
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		return err
	}
	if err = validateKaminoRefresh(obligation, collateral, debt); err != nil {
		return err
	}
	underlying, err := collateral.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	if err != nil {
		return err
	}
	owed, err := obligation.debtAtReserveRate(debt)
	if err != nil {
		return err
	}
	position := KaminoPosition{CollateralDepositedRaw: obligation.collateralDepositedRaw, RedeemablePrimeRaw: underlying, DebtRaw: owed, CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals, CollateralPriceSF: collateral.marketPriceSF, DebtPriceSF: debt.marketPriceSF}
	// The desired ceiling cannot exceed 1.75x. This shared reject-only bound
	// covers initial loans and B2, including fee/instant/protocol LTV and the
	// current collateral deposit room converted into debt raw units.
	ceiling, err := capacitySizedBorrow(position, accounts, route, 175)
	if err != nil {
		return err
	}
	if receive > ceiling {
		return budgetHold("borrow_redeposit_or_risk_capacity_shrank")
	}
	extra, err := valueBetweenTokenRaw(receive, position.DebtDecimals, position.CollateralDecimals, position.DebtPriceSF, position.CollateralPriceSF, false)
	if err != nil {
		return err
	}
	extra = new(big.Int).Quo(new(big.Int).Mul(new(big.Int).SetUint64(extra), big.NewInt(10_000-leverageSwapLossBPS)), big.NewInt(10_000)).Uint64()
	if position.DebtRaw, err = budgetSumU64(position.DebtRaw, receive+fee); err != nil {
		return err
	}
	if position.RedeemablePrimeRaw, err = budgetSumU64(position.RedeemablePrimeRaw, extra); err != nil {
		return err
	}
	after, err := observedLTVBPS(position)
	if err != nil || after > leverageMaxLTVBPS {
		return budgetHoldFrom(err, "borrow_loop_risk_capacity_shrank")
	}
	return nil
}

// leverageBorrowReceive is this tick's leverage_up borrow: the stored
// approval, sized down to this tick's capacity (room, deposit room, risk), so
// room that opens is used on the tick that sees it, not after the next
// selector sample. Zero below the minimum borrow.
func leverageBorrowReceive(s Snapshot, level float64) uint64 {
	if !leverageApprovalLive(s) {
		return 0
	}
	n := min(s.LeverageApprovedBorrowRaw, leverageBorrowCeiling(s, level))
	if n < leverageMinimumBorrowRaw {
		return 0
	}
	return n
}

// leverageApprovalLive: an unconsumed approval decided from this position's debt.
func leverageApprovalLive(s Snapshot) bool {
	return s.LeverageBorrowOperationID == "" && s.LeverageApprovedBorrowRaw >= leverageMinimumBorrowRaw && s.PositionDebtRaw >= 0 && borrowDebtMatches(uint64(s.PositionDebtRaw), s.LeverageSourceDebtRaw)
}

// capacityBorrowCentralValue values debt raw in USDC at the snapshot's central
// reserve prices, without capacityBorrowValue's margins: an expectation for
// the income forecast, never a bound. liability rounds up.
func capacityBorrowCentralValue(s Snapshot, amount uint64, liability bool) (uint64, error) {
	if _, err := runtimeRoute(s.RouteLane); err != nil {
		return 0, err
	}
	return valueBetweenTokenRaw(amount, s.BorrowDebtDecimals, 6, s.BorrowDebtPriceSF, s.BorrowUSDCPriceSF, liability)
}

// Match ObserveBudgetTokenPrice: bridge USDC is par; non-USDC debt and
// reference prices use the existing budgetPriceMarginBPS interval on each
// side. This is valuation evidence, not another transaction-expense charge.
func capacityBorrowValue(s Snapshot, amount uint64, liability bool) (uint64, error) {
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return 0, err
	}
	token, usdc := s.BorrowDebtPriceSF, s.BorrowUSDCPriceSF
	if route.Kamino.DebtMint != bridgeUSDC {
		if liability {
			token, err = UpperPriceMargin(token, budgetPriceMarginBPS)
			if err != nil {
				return 0, err
			}
			usdc, err = lowerPriceMargin(usdc, budgetPriceMarginBPS)
		} else {
			token, err = lowerPriceMargin(token, budgetPriceMarginBPS)
			if err != nil {
				return 0, err
			}
			usdc, err = UpperPriceMargin(usdc, budgetPriceMarginBPS)
		}
		if err != nil {
			return 0, err
		}
	}
	return valueBetweenTokenRaw(amount, s.BorrowDebtDecimals, 6, token, usdc, liability)
}
