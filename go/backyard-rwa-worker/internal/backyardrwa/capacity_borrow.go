package backyardrwa

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
	if debtAccount.Data[kaminoReserveConfigOffset+7] != 0 || !allZero(debtAccount.Data[kaminoReserveConfigOffset+920:kaminoReserveConfigOffset+936]) {
		return 0, nil
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
	if collateralAccount.Data[kaminoDisableCrossCollateralOffset] != 0 {
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
	daily, err := kaminoNetBorrowHeadroom(debtAccount.Data[kaminoDebtWithdrawalCapOffset:kaminoDebtWithdrawalCapOffset+32], clockUnixTimestamp(accounts))
	if err != nil {
		return 0, err
	}
	available := binary.LittleEndian.Uint64(debtAccount.Data[224:232])
	debtRoom := min(available, outsideLimit-outsideUsed, debt.borrowLimitRaw-debt.borrowedRaw, utilization, daily)
	if collateralAccount.Data[kaminoReserveStatusOffset] != 0 || debtAccount.Data[kaminoReserveStatusOffset] != 0 ||
		collateralAccount.Data[kaminoReserveEmergencyModeOffset] != 0 || debtAccount.Data[kaminoReserveEmergencyModeOffset] != 0 {
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
	receive = min(receive, desired, depositRoom)
	half, err := p.targetLTVBorrowRaw()
	if err != nil {
		return 0, err
	}
	// The lane's own protocol collateral LTV can be stricter than pilot 50%.
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

func applyBorrowCapacity(s *Snapshot, p KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute) {
	if !leverageLane(route.Lane) {
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
		return budgetHold("borrow_loop_risk_capacity_shrank")
	}
	return nil
}

func leverageBorrowReceive(s Snapshot, level float64) uint64 {
	n := s.LeverageApprovedBorrowRaw
	if s.LeverageBorrowOperationID != "" || n < leverageMinimumBorrowRaw || s.PositionDebtRaw < 0 || !borrowDebtMatches(uint64(s.PositionDebtRaw), s.LeverageSourceDebtRaw) || n > leverageBorrowCeiling(s, level) {
		return 0
	}
	return n
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
