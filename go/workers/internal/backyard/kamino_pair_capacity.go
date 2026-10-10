package backyard

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

// kaminoPairEntryCapacity is an active registry lane's entry equity ceiling:
// the collateral reserve's deposit room (B2: collateral equity is retained
// independently of debt room, which bounds only the borrow), after the
// borrowing evidence itself validates — unknown is not a zero loan.
//
// The returned equity is DEBT-denominated raw units. That equals USDC equity
// only when the debt mint is bridgeUSDC. For any other debt lane the caller
// must convert it explicitly through the established BudgetPrice valuation
// (selectorDestinationDebtPrice / ObserveBudgetTokenPrice) — same decimals
// never imply price parity.
func kaminoPairEntryCapacity(position KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute) (uint64, error) {
	if !earnActiveLane(route.Lane) {
		return 0, fmt.Errorf("pair_capacity_lane_unreviewed")
	}
	if _, err := kaminoAdditionalDebtRoom(accounts, route); err != nil {
		return 0, err
	}
	return unleveredEntryCapacityDebtRaw(position, accounts, route)
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
