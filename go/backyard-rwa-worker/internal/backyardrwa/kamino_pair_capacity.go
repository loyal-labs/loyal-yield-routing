package backyardrwa

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
)

// These offsets were obtained from the installed KLend SDK's Borsh layouts,
// including the 8-byte account discriminator. Cross-mode (elevation group 0)
// is the only recipe the pilot initializes. A different group needs its exact
// market/paired-collateral cap model; it must not inherit cross-mode liquidity.
const (
	kaminoObligationElevationGroupOffset = 2285
	kaminoOutsideBorrowCounterOffset     = 6704
	kaminoOutsideBorrowLimitOffset       = 5504
	kaminoDisableCrossCollateralOffset   = 5500
	kaminoDebtWithdrawalCapOffset        = 5448
	kaminoBorrowFactorOffset             = 5008
	kaminoLoanToValueOffset              = 4872
	kaminoQueuedCollateralOffset         = 6968
)

// Constrain the reserve-wide 1.5x equity bound by every cross-mode borrowing
// cap. This is a protocol size ceiling, not policy, swap-depth or exit admission.
// Inputs come from the same confirmed batch as the independently observed NAV.
func kaminoPairEntryCapacity(position KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute) (uint64, error) {
	if !selectorLane(route.Lane) || route.Kamino.DebtMint != bridgeUSDC {
		return 0, fmt.Errorf("pair_capacity_lane_unreviewed")
	}
	if position.EntryCapacityRaw == 0 {
		return 0, nil
	}
	collateralAccount, debtAccount := accountAt(accounts, route.Kamino.CollateralReserve), accountAt(accounts, route.Kamino.DebtReserve)
	collateral, err := decodeKaminoReserve(collateralAccount, route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return 0, err
	}
	debt, err := decodeKaminoReserve(debtAccount, bridgeUSDC, route.Kamino)
	if err != nil {
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
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	if new(big.Int).SetUint64(feeRate).Cmp(one) >= 0 {
		return 0, nil
	}
	// The first borrow is against the initial collateral deposit, before the
	// borrowed proceeds are redeposited. Include origination in that interim LTV.
	needed := new(big.Int).Mul(big.NewInt(TargetLTVBPS), new(big.Int).SetUint64(factor))
	needed.Mul(needed, new(big.Int).Add(new(big.Int).Set(one), new(big.Int).SetUint64(feeRate)))
	allowed := new(big.Int).Mul(new(big.Int).SetUint64(uint64(collateralAccount.Data[kaminoLoanToValueOffset])*10_000), one)
	if needed.Cmp(allowed) > 0 {
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
	if debtRoom == 0 {
		return 0, nil
	}
	// Reserve one raw unit for fee rounding. This is deliberately conservative
	// to KLend's max(1, principal*fee).round(); execution reprices exact fees.
	receive := new(big.Int).SetUint64(debtRoom)
	if feeRate != 0 {
		receive.Sub(receive, big.NewInt(1)).Mul(receive, one)
		receive.Quo(receive, new(big.Int).Add(new(big.Int).Set(one), new(big.Int).SetUint64(feeRate)))
		if receive.Cmp(big.NewInt(2)) < 0 {
			return 0, nil
		}
	}
	equity := new(big.Int).Mul(receive, big.NewInt(2))
	if equity.Cmp(new(big.Int).SetUint64(position.EntryCapacityRaw)) > 0 {
		equity.SetUint64(position.EntryCapacityRaw)
	}
	// The reserve-wide collateral bound still includes both initial deposit and
	// redeposit; retain it independently of the borrowing caps above.
	reserveBound, err := entryCapacityDebtRaw(collateral, debt)
	if err != nil {
		return 0, err
	}
	if equity.Cmp(new(big.Int).SetUint64(reserveBound)) > 0 {
		equity.SetUint64(reserveBound)
	}
	if !equity.IsUint64() || equity.Uint64() > math.MaxInt64 {
		return 0, fmt.Errorf("pair_capacity_exceeds_decision_range")
	}
	// The maximum must itself be viable after minimum-one fee rounding. This
	// ceiling is not a substitute for repricing the actual quoted tranche;
	// smaller inputs can have a worse fee-to-collateral ratio.
	borrow := equity.Uint64() / 2
	fee, err := kaminoBorrowFeeAtRate(feeRate, borrow)
	if err != nil {
		return 0, nil
	}
	weightedDebt := new(big.Int).Mul(new(big.Int).SetUint64(borrow+fee), big.NewInt(100))
	maxDebt := new(big.Int).Mul(new(big.Int).Set(equity), new(big.Int).SetUint64(uint64(collateralAccount.Data[kaminoLoanToValueOffset])))
	if weightedDebt.Cmp(maxDebt) > 0 {
		return 0, nil
	}
	return equity.Uint64(), nil
}

func kaminoNetBorrowHeadroom(data []byte, now int64) (uint64, error) {
	if len(data) != 32 || now <= 0 {
		return 0, fmt.Errorf("pair_capacity_clock_or_cap_invalid")
	}
	capacity, current := int64(binary.LittleEndian.Uint64(data[:8])), int64(binary.LittleEndian.Uint64(data[8:16]))
	start, interval := binary.LittleEndian.Uint64(data[16:24]), binary.LittleEndian.Uint64(data[24:32])
	if interval == 0 {
		return math.MaxUint64, nil
	}
	if start > uint64(now) {
		return 0, fmt.Errorf("pair_capacity_net_borrow_cap_invalid")
	}
	if capacity < 0 {
		return 0, nil
	}
	// KLend withdrawal_cap_operations::remaining_withdrawal_caps_amount resets
	// at elapsed >= interval. Negative counters reflect net repayments.
	if uint64(now)-start >= interval {
		current = 0
	}
	room := new(big.Int).Sub(big.NewInt(capacity), big.NewInt(current))
	if room.Sign() <= 0 {
		return 0, nil
	}
	if !room.IsUint64() {
		return 0, fmt.Errorf("pair_capacity_net_borrow_overflow")
	}
	return room.Uint64(), nil
}
