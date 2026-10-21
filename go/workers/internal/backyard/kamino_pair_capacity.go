package backyard

import (
	"fmt"
	"math"
	"math/big"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

func kaminoPairEntryCapacityDebtRaw(position KaminoPosition, accounts []ConfirmedAccount, route RuntimeRoute) (uint64, error) {
	if !earnActiveLane(route.Lane) {
		return 0, fmt.Errorf("pair_capacity_lane_unreviewed")
	}
	if _, err := kaminoAdditionalDebtRoom(accounts, route); err != nil {
		return 0, err
	}
	return unleveredEntryCapacityDebtRaw(position, accounts, route)
}

func kaminoNetBorrowHeadroom(withdrawalCap kamino.WithdrawalCap, now int64) (uint64, error) {
	if now <= 0 {
		return 0, fmt.Errorf("pair_capacity_clock_or_cap_invalid")
	}
	capacity, current := withdrawalCap.ConfigCapacity, withdrawalCap.CurrentTotal
	start, interval := withdrawalCap.LastIntervalStartTimestamp, withdrawalCap.IntervalLengthSeconds
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
