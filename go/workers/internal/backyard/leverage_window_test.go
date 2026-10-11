package backyard

import (
	"math"
	"testing"
)

// The AUTO position of 2026-10-10 01:00-06:00 UTC, as observed: $102.08k of
// AUTO against $908 of PYUSD (LTV 89 bps), NAV $101.17k, a stored 1.75x
// target, the PYUSD debt reserve at 89.92-89.96% utilization under its 90%
// borrow limit ($3.3k-$3.7k of room), AUTO's token yield 9.47% against the
// 5.80% plain curve (the selector's 3.45-3.54 pt spread). Every tick
// journaled HOLD single_loop_position_ready: no borrow approval was ever
// stored (leverageTargets decidedAt stayed 2026-09-29). The refusal was the
// economic gate, not capacity.
func oct10WindowSnapshot(t *testing.T, roomRaw uint64) (Snapshot, LaneEconomics) {
	t.Helper()
	s := base()
	s.RouteLane, s.StrategyKey = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane
	s.HasPosition, s.ObligationPresent, s.ObligationPresenceKnown = true, true, true
	s.PositionCollateralValueRaw, s.PositionCollateralRaw = 102_080_000_000, 102_080_000_000
	s.PositionDebtValueRaw, s.PositionDebtRaw = 908_000_000, 908_000_000
	s.LTVBPS = 89
	s.LeverageTargetLevel = 1.75
	s.TotalVaultNAVRaw, s.StrategyNAVRaw = 101_172_000_000, 101_172_000_000
	armFeeAuthorityFixture(t, &s)
	armLeverageCapacityFixture(&s)
	s.LeverageApprovedBorrowRaw, s.LeverageSourceDebtRaw = 0, 0
	s.AdditionalDebtRoomRaw, s.BorrowUtilizationHeadroomRaw = roomRaw, roomRaw
	s.LeverageBorrow150Raw = min(s.LeverageDesired150Raw, roomRaw)
	s.LeverageBorrow175Raw = min(s.LeverageDesired175Raw, roomRaw)
	if roomRaw < leverageMinimumBorrowRaw {
		s.LeverageBorrow150Raw, s.LeverageBorrow175Raw = 0, 0
	}
	m := LaneEconomics{Lane: s.RouteLane, NativeAPY: 0.0947,
		BorrowCurve:   []BorrowCurvePoint{{0, 458}, {8000, 580}, {9000, 580}, {9500, 662}, {10_000, 905}},
		DebtSupplyRaw: 4_866_000e6, DebtBorrowRaw: 4_866_000e6*0.9 - float64(roomRaw)}
	apr, err := projectedBorrowAPR(m, 0)
	if err != nil {
		t.Fatal(err)
	}
	m.CurrentBorrowAPY = math.Expm1(apr)
	return s, m
}

// Priced at its expected cost the window's step approves: the 20 bps
// haircut it used to be charged left it at break-even on a ~3.5 pt spread.
func TestOct10WindowApprovesAtTheExpectedCost(t *testing.T) {
	t.Parallel()
	s, m := oct10WindowSnapshot(t, 3_300_000_000)
	got, ok := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{m}, DefaultSelectorPolicy(), measuredStepCost(0))
	if !ok || got.Next != 1.75 || got.Reason != "up_move_approved" || got.BorrowRaw != s.LeverageDesired150Raw || got.GainRaw <= 0 {
		t.Fatalf("the Oct 10 step was not approved: %+v ok=%t", got, ok)
	}
	// Its expected cost is the AUTO->PYUSD unwind and the network: ~6.4 bps.
	if bps := got.CostRaw / float64(got.BorrowRaw) * 10_000; bps < 6.4 || bps > 6.5 {
		t.Fatalf("step cost %.2f bps", bps)
	}
}

// AUTO at its realized spread (2.88 pts, #349) with today's measured quotes
// takes both live steps toward its 1.75x target: 1.01x -> 1.5x, then
// 1.5x -> 1.75x. A spread under the 1.75x step's 2 pt rule does not, and a
// quote outage is an unknown cost.
func TestRealizedAUTOSpreadApprovesTheStepsTo1_75x(t *testing.T) {
	t.Parallel()
	realized := func(m LaneEconomics, spread float64) LaneEconomics {
		m.NativeAPY = m.CurrentBorrowAPY + spread
		return m
	}
	s, m := oct10WindowSnapshot(t, 3_300_000_000)
	got, ok := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{realized(m, 0.0288)}, DefaultSelectorPolicy(), measuredStepCost(0))
	if !ok || got.Next != 1.75 || got.Reason != "up_move_approved" || got.BorrowRaw != s.LeverageDesired150Raw || got.GainRaw <= 0 {
		t.Fatalf("1.01x -> 1.5x at 2.88 pts: %+v ok=%t", got, ok)
	}
	// The same lane at 1.5x: the step to 1.75x.
	at15 := s
	at15.PositionCollateralValueRaw, at15.PositionCollateralRaw = 151_758_000_000, 151_758_000_000
	at15.PositionDebtValueRaw, at15.PositionDebtRaw = 50_586_000_000, 50_586_000_000
	at15.LTVBPS = leverageLevelLTVBPS(1.5)
	armLeverageCapacityFixture(&at15)
	at15.LeverageApprovedBorrowRaw, at15.LeverageSourceDebtRaw = 0, 0
	at15.BorrowUtilizationHeadroomRaw = 3_300_000_000
	got, ok = decideLeverageTarget(at15, SelectorResult{Action: "KEEP"}, []LaneEconomics{realized(m, 0.0288)}, DefaultSelectorPolicy(), measuredStepCost(0))
	if !ok || got.Next != 1.75 || got.Reason != "up_move_approved" || got.BorrowRaw != at15.LeverageDesired175Raw || got.GainRaw <= 0 {
		t.Fatalf("1.5x -> 1.75x at 2.88 pts: %+v ok=%t", got, ok)
	}
	// Under the 1.75x step's 2 pt rule: no approval, whatever the cost.
	got, ok = decideLeverageTarget(at15, SelectorResult{Action: "KEEP"}, []LaneEconomics{realized(m, 0.015)}, DefaultSelectorPolicy(), measuredStepCost(0))
	if !ok || got.BorrowRaw != 0 || got.Reason != "spread_rule" {
		t.Fatalf("a spread under the rule was approved: %+v ok=%t", got, ok)
	}
	// A quote outage: the cost is unknown, never zero.
	got, ok = decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{realized(m, 0.0288)}, DefaultSelectorPolicy(), quoteOutage)
	if !ok || got.BorrowRaw != 0 || got.Reason != "leverage_cost_unavailable" || got.Next != got.Current {
		t.Fatalf("an unpriced step was decided: %+v ok=%t", got, ok)
	}
}

func TestLeverageUpApprovesTheStepAndBorrowsTheRoom(t *testing.T) {
	t.Parallel()
	const room = 3_300_000_000
	s, m := oct10WindowSnapshot(t, room)
	got, ok := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{m}, DefaultSelectorPolicy(), measuredStepCost(0))
	if !ok || got.Next != 1.75 || got.Reason != "up_move_approved" || got.BorrowRaw != s.LeverageDesired150Raw || got.GainRaw <= 0 || got.SourceDebtRaw != uint64(s.PositionDebtRaw) {
		t.Fatalf("a profitable step got no approval: %+v ok=%t", got, ok)
	}
	if !got.storeChanges(s) {
		t.Fatal("a new approval was not stored")
	}
	// The worker borrows this tick's room, not the whole approved step.
	s.LeverageApprovedBorrowRaw, s.LeverageSourceDebtRaw = got.BorrowRaw, got.SourceDebtRaw
	if d := Decide(s); d.Action != OpenRouteStep || d.Reason != leverageUpReason || d.AmountRaw != room {
		t.Fatalf("the approved step did not borrow the room: %+v", d)
	}
	// The same decision a sample later is not rewritten (no generation churn).
	if got.storeChanges(s) {
		t.Fatal("an unchanged approval was rewritten")
	}
	// The reserve fills: the approval stands (it was priced at the limit
	// rate) and the tick names the hold.
	full, _ := oct10WindowSnapshot(t, 0)
	again, _ := decideLeverageTarget(full, SelectorResult{Action: "KEEP"}, []LaneEconomics{m}, DefaultSelectorPolicy(), measuredStepCost(0))
	if again.BorrowRaw != got.BorrowRaw || again.Reason != "up_move_approved" || again.RoomRaw != 0 {
		t.Fatalf("a full reserve withdrew the approval: %+v", again)
	}
	full.LeverageApprovedBorrowRaw, full.LeverageSourceDebtRaw = got.BorrowRaw, got.SourceDebtRaw
	if d := Decide(full); d.Action != Hold || d.Reason != "leverage_up_no_borrow_room" {
		t.Fatalf("a full reserve under a live approval: %+v", d)
	}
	full.LeverageApprovedBorrowRaw = 0
	if d := Decide(full); d.Action != Hold || d.Reason != "leverage_up_awaiting_approval" {
		t.Fatalf("no approval: %+v", d)
	}
	// A consumed approval (bound to a borrow) never borrows again.
	s.LeverageBorrowOperationID = "op"
	if d := Decide(s); d.Action != Hold || d.Reason != "leverage_up_awaiting_approval" {
		t.Fatalf("a consumed approval borrowed again: %+v", d)
	}
}

// A spread that does not repay the move's cost within the horizon is still
// refused: the in-place comparison does not drop the cost.
func TestLeverageUpRefusesAThinSpread(t *testing.T) {
	t.Parallel()
	s, m := oct10WindowSnapshot(t, 3_300_000_000)
	m.NativeAPY = 0.08
	got, ok := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{m}, DefaultSelectorPolicy(), twentyBPSStepCost)
	if !ok || got.BorrowRaw != 0 || got.Reason != "up_move_not_worth_cost" || got.GainRaw > 0 {
		t.Fatalf("a thin spread was approved: %+v ok=%t", got, ok)
	}
}
