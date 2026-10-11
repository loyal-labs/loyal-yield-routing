package backyard

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"
)

// Overflow (owner decision 2026-10-11). AUTO/AUTO/PYUSD is Earn Max's AUTO
// lane; AUTO/AUTO/wYLDS is its overflow lane, a second obligation on the same
// collateral reserve and custody that borrows wYLDS. When the source lane's
// next leverage step finds no borrow room and the overflow lane's reserve
// has room, an overflow tranche moves part of the source's AUTO into the
// overflow obligation and levers it there:
//
//  1. source half (the source is working): release collateral worth E from
//     the source obligation without repaying, which takes the source toward
//     its own target level, never past it; the AUTO waits in the shared
//     custody;
//  2. handoff: the overflow lane becomes the working lane and the AUTO in
//     the shared custody is its collateral;
//  3. destination half: initialize the overflow obligation if absent,
//     deposit the AUTO, then lever toward its own stored target with the
//     same leverage_up approval, expected cost and persistence as any lane.
//
// E is sized so the vault's whole level heads to the live cap: the source
// lands at most at its target level, and E is at most what the overflow
// reserve's room levers to that level. While the source has room its own
// step wins and no overflow starts; the overflow lane is never a selector or
// tranche destination of its own.

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
		if l.overflowOf == source && earnActiveLane(l.lane) {
			return l.lane, true
		}
	}
	return "", false
}

// overflowPair reports a source lane and its overflow lane, in either order.
func overflowPair(a, b string) bool {
	if source, ok := overflowSourceOf(b); ok && source == a {
		return true
	}
	source, ok := overflowSourceOf(a)
	return ok && source == b
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
	route, err := runtimeRoute(lane)
	return err != nil || !hastraLane(route)
}

// overflowSourceLTVBPS is the LTV an overflow source releases to: the live
// cap's level (1.75x = 42.85%), below the 45% leverage cap and the release
// ceiling, so no release round is needed.
var overflowSourceLTVBPS = leverageLevelLTVBPS(leverageMaxLiveLevel)

// overflowMinimumEquityRaw is the smallest E: one that levers to the live cap
// with at least the minimum borrow (borrow = (L-1) E).
var overflowMinimumEquityRaw = int64(math.Ceil(float64(leverageMinimumBorrowRaw) / (leverageMaxLiveLevel - 1)))

// overflowSourceSettled: the working lane is a quiet funded source with an
// overflow lane and nothing in flight.
func overflowSourceSettled(s Snapshot) bool {
	_, ok := overflowLaneOf(s.RouteLane)
	return ok && trancheSourceSettled(s) && !s.OtherLanesCustodyHeld && s.HasPosition && s.PositionCollateralValueRaw > 0
}

// overflowReleaseValueRaw is the most collateral value the source releases
// without repaying: down to the overflow LTV (debt / (collateral - R)), and
// below the 90% of its equity that keeps it a lane.
func overflowReleaseValueRaw(s Snapshot) int64 {
	if s.PositionCollateralValueRaw <= 0 || s.PositionDebtValueRaw < 0 {
		return 0
	}
	limit := s.PositionCollateralValueRaw
	if s.PositionDebtValueRaw > 0 {
		keep := new(big.Int).Mul(big.NewInt(s.PositionDebtValueRaw), big.NewInt(10_000))
		keep.Add(keep, big.NewInt(overflowSourceLTVBPS-1)).Quo(keep, big.NewInt(overflowSourceLTVBPS))
		if !keep.IsInt64() || keep.Int64() >= s.PositionCollateralValueRaw {
			return 0
		}
		limit = s.PositionCollateralValueRaw - keep.Int64()
	}
	// A tranche never empties a lane: strictly below the partial planner's
	// 90% exclusion of the source's equity.
	equity := trancheSourceEquityRaw(s)
	return max(min(limit, (equity*partialWithdrawalFullExitBPS-1)/10_000), 0)
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

// overflowTranche is the overflow tranche a quiet working source starts, or
// nil with the reason it does not. It needs the source's next leverage step
// (its stored target above the position) to find no fee-inclusive room for
// the minimum borrow, the overflow reserve to have room for it, and the
// overflow lane's spread at the live cap to clear the level's rule.
func overflowTranche(in SelectorInput, generation int64, now time.Time) (*trancheIntent, string) {
	s := in.Snapshot
	lane, ok := overflowLaneOf(s.RouteLane)
	switch {
	case !ok:
		return nil, ""
	case leverageUpLevel(s) == 0:
		return nil, "overflow_source_at_target"
	case !s.BorrowCapacityKnown:
		return nil, "overflow_source_room_unknown"
	case !s.BorrowUtilizationBlocked && kaminoReceiveWithinRoom(s.AdditionalDebtRoomRaw, s.BorrowFeeRate) >= leverageMinimumBorrowRaw:
		return nil, "overflow_source_has_room"
	case !overflowSourceSettled(s):
		return nil, "overflow_source_not_settled"
	case !s.OverflowRoomKnown:
		return nil, "overflow_room_unknown"
	case s.OverflowRoomRaw < leverageMinimumBorrowRaw:
		return nil, "overflow_room_closed"
	}
	held := s.heldLanes()
	if !containsLane(held, lane) && len(held) >= maxHeldLanes {
		return nil, "tranche_held_lane_limit"
	}
	e := overflowEquityRaw(s)
	if e <= 0 {
		return nil, "overflow_equity_below_minimum"
	}
	var market *LaneEconomics
	for i := range in.Markets {
		if in.Markets[i].Lane == lane && in.Markets[i].validate(in.Now, in.Policy) == nil {
			market = &in.Markets[i]
		}
	}
	if market == nil {
		return nil, "overflow_lane_economics_unavailable"
	}
	// The step's projected utilization: its whole borrow, in the debt
	// reserve's raw units (forecast only).
	borrowValue := float64(e) * (leverageMaxLiveLevel - 1)
	if market.DebtRawPerUSDCRaw <= 0 {
		return nil, "overflow_lane_economics_unavailable"
	}
	if _, allowed := leverageUpAllowed(*market, leverageMaxLiveLevel, borrowValue*market.DebtRawPerUSDCRaw); !allowed {
		return nil, "overflow_spread_rule"
	}
	t := &trancheIntent{Source: s.RouteLane, Destination: lane, EquityRaw: e, Generation: generation, ObservationID: s.ObservationID,
		EvidenceID: sha256Bytes([]byte(fmt.Sprintf("overflow|%s|%s|%d|%d|%d", s.ObservationID, lane, e, s.OverflowRoomRaw, s.OverflowRoomValueRaw))), CreatedAt: now}
	if t.validate() != nil {
		return nil, "tranche_intent_invalid"
	}
	return t, "overflow_tranche"
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

// overflowSourceStep is the overflow source half: one release of E, with no
// repay, while the shared custody is clear. Released collateral is not a
// leg here: it waits in the custody for the overflow lane. The release
// reuses the partial withdrawal's journaled reason, its LTV record and its
// prepare-time sizing.
func overflowSourceStep(s Snapshot) (Action, string, int64, bool) {
	if s.TrancheOverflowRaw <= 0 || !earnActiveLane(s.RouteLane) || s.WithdrawalDemandRaw != 0 || s.Unwind || s.CutoverDrain || !s.HasPosition ||
		s.CollateralIdleRaw > partialWithdrawalDustRaw || s.SquadsIdleRaw > partialWithdrawalDustRaw || debtCashRaw(s) > partialWithdrawalDustRaw || s.VoltrStrategyIdleRaw != 0 {
		return "", "", 0, false
	}
	if overflowReleaseReceipts(s, s.TrancheOverflowRaw) <= 0 {
		return "", "", 0, false
	}
	return DeleverRouteStep, partialReleaseReason, s.TrancheOverflowRaw, true
}

// overflowSourceHalf: this view is an overflow source still freeing E.
func overflowSourceHalf(s Snapshot) bool {
	return s.TrancheOverflowRaw > 0 && s.WithdrawalDemandRaw == 0 && !s.Unwind && !s.CutoverDrain
}

// overflowSourceFreed: the source released and its collateral waits in the
// shared custody, with no other cash in flight.
func overflowSourceFreed(s Snapshot) bool {
	return s.CollateralIdleRaw > partialWithdrawalDustRaw && s.SquadsIdleRaw == 0 && s.DebtIdleRaw <= partialWithdrawalDustRaw && s.VoltrStrategyIdleRaw == 0
}

// overflowSourceHoldReason labels an overflow source half with no next leg.
func overflowSourceHoldReason(s Snapshot) string {
	if overflowSourceFreed(s) {
		return "overflow_source_freed"
	}
	return "overflow_source_release_unavailable"
}

// overflowDestinationEntry: the overflow lane's view taking over the freed
// collateral. Its entry authority is the overflow tranche, not a selector
// entry quote.
func overflowDestinationEntry(s Snapshot) bool {
	return s.TrancheOverflowDestination && s.TrancheDestinationRaw > 0 && earnActiveLane(s.RouteLane)
}

// overflowInitializationReady is the overflow lane's initializer readiness:
// a quiet view of its absent obligation whose only working cash is the
// collateral the source freed into the shared custody.
func overflowInitializationReady(s Snapshot) bool {
	return overflowDestinationEntry(s) && earnInitializerLane(s.RouteLane) && s.Fresh && s.ObservationID != "" && s.Slot > 0 && s.RouteKind == RouteKind &&
		s.ObligationPresenceKnown && !s.ObligationPresent && !s.HasPosition && s.PositionCollateralRaw == 0 && s.PositionDebtRaw == 0 &&
		s.CollateralIdleRaw > 0 && s.DebtIdleRaw == 0 && s.SquadsIdleRaw == 0 && s.VoltrStrategyIdleRaw == 0 &&
		s.Nonterminal == "" && !s.HasAmbiguousSubmission && s.ManualReason == "" && !s.Unwind && !s.CutoverDrain && s.WithdrawalDemandRaw == 0
}

// applyOverflowRoom stamps, on a lane with an overflow lane, that lane's
// fee-inclusive receive room and its bridge USDC value. Nobody but us borrows
// from the overflow lane's debt reserve, so its raw reserve is usually far
// past the health age (2026-10-11: wYLDS 34GJ ~9,500 slots), while the
// borrow that would use the room refreshes it first. A stale raw verdict is
// therefore judged again on the overflow lane's own refresh simulation, kept
// apart from the NAV capture so its failure only leaves this room unknown.
// The returned error says why the room is unknown.
func applyOverflowRoom(ctx context.Context, s *Snapshot, slot int64, accounts []ConfirmedAccount, route RuntimeRoute, refresh func(context.Context, []RuntimeRoute, []string, int64) (int64, []ConfirmedAccount, error)) error {
	s.OverflowRoomRaw, s.OverflowRoomValueRaw, s.OverflowRoomKnown = 0, 0, false
	lane, ok := overflowLaneOf(route.Lane)
	if !ok {
		return nil
	}
	overflow, err := runtimeRoute(lane)
	if err != nil {
		return err
	}
	receive, value, err := overflowRoom(accounts, overflow)
	if (errors.Is(err, errKaminoReserveStale) || errors.Is(err, errKaminoOracleStale)) && refresh != nil {
		addresses, addressErr := overflowRoomAddresses(overflow)
		if addressErr != nil {
			return addressErr
		}
		_, refreshed, refreshErr := refresh(ctx, []RuntimeRoute{overflow}, addresses, slot)
		if refreshErr != nil {
			return refreshErr
		}
		receive, value, err = overflowRoom(refreshed, overflow)
	}
	if err != nil {
		return err
	}
	s.OverflowRoomRaw, s.OverflowRoomValueRaw, s.OverflowRoomKnown = receive, value, true
	return nil
}

// overflowRoomAddresses are the accounts overflowRoom reads.
func overflowRoomAddresses(overflow RuntimeRoute) ([]string, error) {
	reference, err := pinnedKaminoObservationConfig()
	if err != nil {
		return nil, err
	}
	return []string{overflow.Kamino.CollateralReserve, overflow.Kamino.DebtReserve, overflow.Kamino.Obligation, reference.DebtReserve, budgetClockAddress}, nil
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
	reference, err := pinnedKaminoObservationConfig()
	if err != nil {
		return 0, 0, err
	}
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

// termDueHeldLane is a held lane, other than the working lane, whose term
// debt is within keep-alive time of maturity (or of unknown maturity): it
// becomes the working lane so its keep-alive or delever runs.
func termDueHeldLane(s Snapshot, facts []heldLaneFact) string {
	if s.ChainUnix <= 0 {
		return ""
	}
	for _, f := range facts {
		if f.Lane == s.RouteLane || !earnHeldLane(f.Lane) || f.DebtValueRaw <= 0 || f.DebtTermEndUnix == 0 {
			continue
		}
		if f.DebtTermEndUnix == termMaturityUnknown || f.DebtTermEndUnix-s.ChainUnix < termKeepAliveSeconds {
			return f.Lane
		}
	}
	return ""
}

// workingTermDue: the working lane's own term needs keep-alive time, so no
// rotation moves the working lane away from it.
func workingTermDue(s Snapshot) bool {
	if termDebtUnknown(s) {
		return true
	}
	left, ok := termSecondsLeft(s)
	return ok && left < termKeepAliveSeconds
}

// workingLaneSettled: the working lane can hand the working role to another
// held lane: nothing in flight, nothing reserved, no transient cash.
func workingLaneSettled(s Snapshot) bool {
	return trancheSettledView(s) && s.WithdrawalDemandRaw == 0 && !s.Unwind && !s.CutoverDrain && !s.UnwindRefreshRequired &&
		s.PartialWithdrawalOperationID == "" && !s.OtherLanesCustodyHeld && laneTransientClear(s, true)
}

// overflowInitializerBypassesEntryFence: the overflow lane's initializer is
// authorized by the durable overflow tranche into it, once its source has
// freed the collateral, outside an unwind. Every other request keeps the
// entry fence.
func overflowInitializerBypassesEntryFence(request any, unwinding bool, operationLane string, rawTranche []byte) (bool, error) {
	r, ok := request.(KaminoInitializationRequest)
	if !ok {
		return false, nil
	}
	if _, overflow := overflowSourceOf(r.RouteLane); !overflow {
		return false, nil
	}
	t, err := decodeTrancheIntent(rawTranche)
	if err != nil || t == nil || !t.overflow() || !t.SourceFreed || t.Destination != r.RouteLane || r.RouteLane != operationLane || unwinding {
		return false, budgetHoldFrom(err, "overflow_initializer_authority_mismatch")
	}
	return true, nil
}
