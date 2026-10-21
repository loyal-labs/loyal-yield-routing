package backyard

import (
	"fmt"
	"math"
	"math/big"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

func overflowSourceOf(lane string) (string, bool) {
	l, ok := findEarnLane(lane)
	return l.overflowOf, ok && l.overflowOf != ""
}

func overflowLaneOf(source string) (string, bool) {
	if source == "" {
		return "", false
	}
	for _, l := range earnLanes {
		if l.overflowOf == source && earnActiveLane(l.route.Lane) {
			return l.route.Lane, true
		}
	}
	return "", false
}

func selectorDestinationLane(lane string) bool {
	_, overflow := overflowSourceOf(lane)
	return earnActiveLane(lane) && !overflow
}

const overflowLaneBlockedReason = "overflow_lane_entered_by_overflow_only"

// debtToUSDCConvertible reports a lane whose debt leftovers may convert to
// USDC. A Hastra lane's wYLDS debt redeems to USDC only through Hastra's
// admin-settled queue (hastraConversionLeg), which Backyard never uses, so
// its leftovers stake to collateral instead.
func debtToUSDCConvertible(lane string) bool {
	l, ok := findEarnLane(lane)
	return !ok || !hastraLane(l.route)
}

func overflowSourceBorrowClosed(s Snapshot) bool {
	return s.BorrowCapacityKnown && (s.BorrowUtilizationBlocked || kaminoReceiveWithinRoom(s.AdditionalDebtRoomRaw, s.BorrowFeeRate) < leverageMinimumBorrowRaw)
}

func overflowEquityRaw(s Snapshot, level float64) int64 {
	if _, ok := overflowLaneOf(s.RouteLane); !ok || s.OverflowRoom == nil || s.OverflowRoom.FlooredUSDCValue <= 0 {
		return 0
	}
	levered := level - 1
	if levered <= 0 {
		return 0
	}
	byRoom := int64(math.Floor(float64(s.OverflowRoom.FlooredUSDCValue) / levered))
	e := min(overflowReleaseValueRaw(s), byRoom, int64(strategyTwoBridgeLegCapRaw))
	if e < int64(math.Ceil(float64(leverageMinimumBorrowRaw)/levered)) {
		return 0
	}
	return e
}

func overflowReleaseReceipts(s Snapshot, releaseValueRaw int64) (collateralReceiptsRaw int64) {
	value := min(releaseValueRaw, overflowReleaseValueRaw(s))
	if value <= 0 || s.PositionCollateralRaw <= 0 || s.PositionCollateralValueRaw <= 0 {
		return 0
	}
	receipts := new(big.Int).Mul(big.NewInt(s.PositionCollateralRaw), big.NewInt(value))
	receipts.Quo(receipts, big.NewInt(s.PositionCollateralValueRaw))
	if !receipts.IsInt64() || receipts.Sign() <= 0 || receipts.Int64() >= s.PositionCollateralRaw {
		return 0
	}
	return receipts.Int64()
}

func applyOverflowRoom(v *Vault, s *Snapshot, accounts []ConfirmedAccount, route RuntimeRoute) error {
	s.OverflowRoom = nil
	lane, ok := overflowLaneOf(route.Lane)
	if !ok || !s.HasPosition || !overflowSourceBorrowClosed(*s) {
		return nil
	}
	overflow, err := v.route(lane)
	if err != nil {
		return err
	}
	receive, value, err := overflowRoom(accounts, overflow)
	if err != nil {
		return err
	}
	s.OverflowRoom = &overflowBorrowRoom{DebtRaw: receive, FlooredUSDCValue: value, Obligation: observedObligation(accountAt(accounts, overflow.Kamino.Obligation).Lamports != 0)}
	return nil
}

// overflowRoom is the overflow lane's fee-inclusive receive room and its
// bridge USDC value, judged on accounts. A matured or unknown overflow borrow
// has no room (KLend refuses it).
func overflowRoom(accounts []ConfirmedAccount, overflow RuntimeRoute) (uint64, int64, error) {
	room, err := kaminoAdditionalDebtRoom(accounts, overflow)
	if err != nil {
		return 0, 0, err
	}
	debtAccount := accountAt(accounts, overflow.Kamino.DebtReserve)
	debt, err := decodeKaminoReserve(debtAccount, overflow.Kamino.DebtMint, overflow.Kamino)
	if err != nil {
		return 0, 0, err
	}
	receive := kaminoReceiveWithinRoom(room, debt.terms.BorrowFeeSF)
	end, err := observedDebtTermEnd(accounts, overflow)
	if err != nil {
		return 0, 0, err
	}
	if now := clockUnixTimestamp(accounts); end == termMaturityUnknown || end > 0 && end <= now {
		receive = 0
	}
	reference := usdcReference
	usdc, err := decodeKaminoReserve(accountAt(accounts, reference.DebtReserve), fleet.USDCMint, reference)
	if err != nil {
		return 0, 0, err
	}
	if usdc.mintDecimals != 6 {
		return 0, 0, fmt.Errorf("USDC reference reserve decimals drifted")
	}
	value, err := valueBetweenTokenRaw(receive, debt.mintDecimals, 6, debt.marketPriceSF, usdc.marketPriceSF, false)
	if err != nil {
		return 0, 0, err
	}
	if value > math.MaxInt64 {
		return 0, 0, fmt.Errorf("overflow room value exceeds the signed range")
	}
	return receive, int64(value), nil
}
