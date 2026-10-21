package backyard

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
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
		return 0, staleHold("borrow_capacity_clock_invalid")
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
		decoded, err := decodeKaminoObligation(obligation, route.Kamino)
		if err != nil {
			return 0, err
		}
		if !kaminoObligationTermOpen(decoded, debt, clockUnixTimestamp(accounts)) {
			return 0, nil
		}
		// A different elevation group must not inherit cross-mode liquidity.
		if !plainCrossModeObligation(decoded, obligation) {
			return 0, nil
		}
	}
	if collateral.terms.DisableUsageAsCollOutsideEmode || collateral.terms.Status != 0 || collateral.terms.EmergencyMode {
		return 0, nil
	}
	return kaminoDebtReserveRoom(debt, clockUnixTimestamp(accounts))
}

const parBorrowFactorPct = 100

func kaminoDebtReserveRoom(debt decodedKaminoReserve, now int64) (uint64, error) {
	if !kaminoReserveTermOpen(debt, now) {
		return 0, nil
	}
	// Newer KLend consumes the first trailing padding u64 for queued collateral.
	// Until the pinned decoder models freely available liquidity, decline new
	// borrowing when any liquidity is queued. Old zero-filled padding is safe.
	if debt.terms.QueuedCollateral != 0 {
		return 0, nil
	}
	// KLend clamps the effective factor to at least 100%, including old
	// configurations whose serialized value is zero.
	unweightedDebtOnly := max(debt.terms.BorrowFactorPct, parBorrowFactorPct) == parBorrowFactorPct
	// Risk observation uses unweighted LTV, so a weighted debt lane needs its own risk and unwind admission.
	if !unweightedDebtOnly {
		return 0, nil
	}
	feeRate := debt.terms.BorrowFeeSF
	if feeRate >= uint64(1)<<kamino.FractionBits {
		return 0, nil
	}
	outsideLimit := debt.terms.BorrowLimitOutsideElevationGroup
	outsideUsed := debt.terms.BorrowedAmountOutsideElevationGroup
	if outsideUsed >= outsideLimit || debt.borrowedRaw >= debt.terms.BorrowLimit {
		return 0, nil
	}
	utilization, err := utilizationBorrowHeadroomRaw(debt)
	if err != nil {
		return 0, err
	}
	daily, err := kaminoNetBorrowHeadroom(debt.terms.DebtWithdrawalCap, now)
	if err != nil {
		return 0, err
	}
	available := debt.terms.AvailableAmount
	debtRoom := min(available, outsideLimit-outsideUsed, debt.terms.BorrowLimit-debt.borrowedRaw, utilization, daily)
	if debt.terms.Status != 0 || debt.terms.EmergencyMode {
		return 0, nil
	}
	return min(debtRoom, math.MaxInt64), nil
}

// Fee-safe floor, including KLend's minimum-one rounding. Never overflows and
// never rounds up into the room. Exact fees are rechecked for the fixed wire.
func kaminoReceiveWithinRoom(room, rate uint64) uint64 {
	if room == 0 || rate >= uint64(1)<<kamino.FractionBits {
		return 0
	}
	room = min(room, math.MaxInt64)
	if rate == 0 {
		return room
	}
	one := new(big.Int).Lsh(big.NewInt(1), kamino.FractionBits)
	n := new(big.Int).Mul(new(big.Int).SetUint64(room-1), one)
	n.Quo(n, new(big.Int).Add(one, new(big.Int).SetUint64(rate)))
	if n.Uint64() < 2 {
		return 0
	}
	return n.Uint64()
}

func capacitySizedBorrow(p KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute, level float64) (uint64, error) {
	room, err := kaminoAdditionalDebtRoom(accounts, route)
	if err != nil {
		return 0, err
	}
	rate, err := debtBorrowFeeRate(accounts, route)
	if err != nil {
		return 0, err
	}
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

func debtBorrowFeeRate(accounts []ConfirmedAccount, route RuntimeRoute) (uint64, error) {
	debt, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return 0, err
	}
	return debt.terms.BorrowFeeSF, nil
}

func leverageStepWithinRisk(p KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute, receive, rate uint64) (uint64, error) {
	collateral, err := p.collateralValueDebtRaw()
	if err != nil {
		return 0, err
	}
	if p.MaxLTVBPS() <= 0 {
		return 0, nil
	}
	borrowLimitBeforeRedeposit := new(big.Int).Mul(new(big.Int).SetUint64(collateral), big.NewInt(p.MaxLTVBPS()))
	borrowLimitBeforeRedeposit.Quo(borrowLimitBeforeRedeposit, big.NewInt(10_000))
	refreshMargin := new(big.Int).Mul(borrowLimitBeforeRedeposit, big.NewInt(leverageUpRefreshMarginBPS))
	borrowLimitBeforeRedeposit.Sub(borrowLimitBeforeRedeposit, refreshMargin.Quo(refreshMargin, big.NewInt(10_000)))
	if !borrowLimitBeforeRedeposit.IsUint64() || borrowLimitBeforeRedeposit.Uint64() <= p.DebtRaw {
		return 0, nil
	}
	receive = min(receive, kaminoReceiveWithinRoom(borrowLimitBeforeRedeposit.Uint64()-p.DebtRaw, rate))
	receive, err = leverageUpCapFee(p, laneCeilingLTVBPS(route.Lane), receive, func(n uint64) (uint64, error) { return kaminoBorrowFeeAtRate(rate, n) })
	if err != nil {
		return 0, nil
	}
	return receive, nil
}

func desiredLeverageBorrow(p KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute, level float64, rate uint64) (uint64, error) {
	if rate >= uint64(1)<<kamino.FractionBits {
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
	debtReserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return
	}
	var borrow, desired [len(leverageLadder)]uint64
	for i, r := range leverageLadder[1:] {
		if r.level > laneMaxLevel(route.Lane) {
			break
		}
		if borrow[i+1], err = capacitySizedBorrow(p, accounts, route, r.level); err != nil {
			return
		}
		if desired[i+1], err = desiredLeverageBorrow(p, accounts, route, r.level, debtReserve.terms.BorrowFeeSF); err != nil {
			return
		}
	}
	utilizationHeadroom, err := utilizationBorrowHeadroomRaw(debtReserve)
	if err != nil {
		return
	}
	reference, config := route.Kamino.DebtReserve, route.Kamino
	if route.Kamino.DebtMint != fleet.USDCMint {
		config = usdcReference
		reference = config.DebtReserve
	}
	usdc, err := decodeKaminoReserve(accountAt(accounts, reference), fleet.USDCMint, config)
	if err != nil || usdc.mintDecimals != 6 || littleInt(usdc.terms.MarketPriceSF[:]).Sign() <= 0 {
		return
	}
	s.BorrowDebtPriceSF, s.BorrowUSDCPriceSF, s.BorrowDebtDecimals = p.DebtPriceSF, usdc.terms.MarketPriceSF, p.DebtDecimals
	s.BorrowFeeRate = debtReserve.terms.BorrowFeeSF
	s.BorrowCapacityKnown, s.AdditionalDebtRoomRaw = true, room
	s.LeverageBorrowRaw, s.LeverageDesiredRaw = borrow, desired
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
// zero when capacity is unknown or level is off the ladder.
func leverageDesiredRaw(s Snapshot, level float64) uint64 {
	i, ok := leverageRungIndex(level)
	if !s.BorrowCapacityKnown || !ok {
		return 0
	}
	return s.LeverageDesiredRaw[i]
}

func leverageBorrowCeiling(s Snapshot, level float64) uint64 {
	i, ok := leverageRungIndex(level)
	if !s.BorrowCapacityKnown || !ok {
		return 0
	}
	return s.LeverageBorrowRaw[i]
}

// liability rounds up.
func capacityBorrowExpectedUSDC(s Snapshot, amount uint64, liability bool) (uint64, error) {
	if !earnHeldLane(s.RouteLane) {
		return 0, fmt.Errorf("runtime lane %q is not installed", s.RouteLane)
	}
	return valueBetweenTokenRaw(amount, s.BorrowDebtDecimals, 6, s.BorrowDebtPriceSF, s.BorrowUSDCPriceSF, liability)
}

func capacityBorrowBoundUSDC(s Snapshot, amount uint64, liability bool) (uint64, error) {
	l, ok := findEarnLane(s.RouteLane)
	if !ok {
		return 0, fmt.Errorf("runtime lane %q is not installed", s.RouteLane)
	}
	token, usdc := s.BorrowDebtPriceSF, s.BorrowUSDCPriceSF
	var err error
	if l.route.Kamino.DebtMint != fleet.USDCMint {
		if liability {
			token, err = UpperPriceMargin(token, valuationMarginBPS)
			if err != nil {
				return 0, err
			}
			usdc, err = lowerPriceMargin(usdc, valuationMarginBPS)
		} else {
			token, err = lowerPriceMargin(token, valuationMarginBPS)
			if err != nil {
				return 0, err
			}
			usdc, err = UpperPriceMargin(usdc, valuationMarginBPS)
		}
		if err != nil {
			return 0, err
		}
	}
	return valueBetweenTokenRaw(amount, s.BorrowDebtDecimals, 6, token, usdc, liability)
}
