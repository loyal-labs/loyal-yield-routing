package backyard

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
)

// Overflow (owner decision 2026-10-11). AUTO/AUTO/PYUSD is Earn Max's AUTO
// lane; AUTO/AUTO/wYLDS is its overflow lane, a second obligation on the same
// collateral reserve and custody that borrows wYLDS. When the source lane is
// below its level and its own debt reserve has no room for the minimum
// borrow, while the overflow lane's reserve has room and its stored level
// approves levering there, target() moves collateral worth E from the source
// obligation into the overflow obligation in one transaction (withdraw,
// deposit; nothing waits in the shared custody), and the overflow lane then
// levers toward its own level by ordinary up steps.
//
// E is sized so the vault's whole level heads to the live cap: the source
// lands at most at the live cap's LTV, and E is at most what the overflow
// reserve's room levers to that level. While the source has room its own up
// step wins and nothing moves; the overflow lane is never a selector
// destination of its own.

// overflowSourceOf is the lane an overflow lane overflows.
func overflowSourceOf(lane string) (string, bool) {
	l, ok := findEarnLane(lane)
	return l.overflowOf, ok && l.overflowOf != ""
}

// overflowLaneOf is a source lane's overflow lane.
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

// selectorDestinationLane reports a lane the selector may enter, switch to
// or move a tranche into on its own economics: every active lane except an
// overflow lane, which only an overflow tranche enters.
func selectorDestinationLane(lane string) bool {
	_, overflow := overflowSourceOf(lane)
	return earnActiveLane(lane) && !overflow
}

// overflowLaneBlockedReason marks an overflow lane in the selector's market
// set: it is entered only by an overflow tranche from its source.
const overflowLaneBlockedReason = "overflow_lane_entered_by_overflow_only"

// debtToUSDCConvertible reports a lane whose debt leftovers may convert to
// USDC. A Hastra lane's wYLDS debt redeems to USDC only through Hastra's
// admin-settled queue (hastraConversionLeg), which Backyard never uses, so
// its leftovers stake to collateral instead.
func debtToUSDCConvertible(lane string) bool {
	l, ok := findEarnLane(lane)
	return !ok || !hastraLane(l.route)
}

// overflowSourceLTVBPS is the LTV an overflow source releases to: the live
// cap's level (1.75x = 42.85%), below the 45% leverage cap and the release
// ceiling, so no release round is needed.
var overflowSourceLTVBPS = leverageLevelLTVBPS(leverageMaxLiveLevel)

// overflowMinimumEquityRaw is the smallest E: one that levers to the live cap
// with at least the minimum borrow (borrow = (L-1) E).
var overflowMinimumEquityRaw = int64(math.Ceil(float64(leverageMinimumBorrowRaw) / (leverageMaxLiveLevel - 1)))

// overflowSourceBorrowClosed is the source lane's own borrow room known and
// under the minimum borrow: the only state in which the overflow room is
// read, so the only one in which observation captures it.
func overflowSourceBorrowClosed(s Snapshot) bool {
	return s.BorrowCapacityKnown && (s.BorrowUtilizationBlocked || kaminoReceiveWithinRoom(s.AdditionalDebtRoomRaw, s.BorrowFeeRate) < leverageMinimumBorrowRaw)
}

// overflowEquityRaw sizes E: the source's release limit, what the overflow
// reserve's room levers to the live cap (room = (L-1) E), and the tranche
// cap. Zero below the minimum.
func overflowEquityRaw(s Snapshot) int64 {
	if !s.OverflowRoomKnown || s.OverflowRoomValueRaw <= 0 {
		return 0
	}
	byRoom := int64(math.Floor(float64(s.OverflowRoomValueRaw) / (leverageMaxLiveLevel - 1)))
	e := min(overflowReleaseValueRaw(s), byRoom, int64(strategyTwoBridgeLegCapRaw))
	if e < overflowMinimumEquityRaw {
		return 0
	}
	return e
}

// overflowReleaseReceipts converts the overflow source's E (bridge USDC
// value) into collateral receipts, capped at the overflow release limit.
func overflowReleaseReceipts(s Snapshot, free int64) int64 {
	value := min(free, overflowReleaseValueRaw(s))
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

// applyOverflowRoom stamps, on a lane with an overflow lane, that lane's
// fee-inclusive receive room and its bridge USDC value, judged on the tick's
// refreshed batch, which refreshes the overflow lane's reserves with the
// valued lanes' (nobody but us borrows from the wYLDS reserve, so its raw
// state is far past the health age). Only a source holding a position that
// cannot borrow reads it.
func applyOverflowRoom(v *Vault, s *Snapshot, accounts []ConfirmedAccount, route RuntimeRoute) error {
	s.OverflowRoomRaw, s.OverflowRoomValueRaw, s.OverflowRoomKnown, s.OverflowObligationPresent = 0, 0, false, false
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
	s.OverflowRoomRaw, s.OverflowRoomValueRaw, s.OverflowRoomKnown = receive, value, true
	s.OverflowObligationPresent = accountAt(accounts, overflow.Kamino.Obligation).Lamports != 0
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
	if len(debtAccount.Data) < kaminoReserveConfigOffset+48 {
		return 0, 0, fmt.Errorf("overflow debt reserve config is truncated")
	}
	receive := kaminoReceiveWithinRoom(room, binary.LittleEndian.Uint64(debtAccount.Data[kaminoReserveConfigOffset+40:]))
	end, err := observedDebtTermEnd(accounts, overflow)
	if err != nil {
		return 0, 0, err
	}
	if now := clockUnixTimestamp(accounts); end == termMaturityUnknown || end > 0 && end <= now {
		receive = 0
	}
	reference := usdcReference
	usdc, err := decodeKaminoReserve(accountAt(accounts, reference.DebtReserve), bridgeUSDC, reference)
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
