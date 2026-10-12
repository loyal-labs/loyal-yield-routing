package backyard

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// leverageDecision is one B2 option-1 level decision for the funded lane.
type leverageDecision struct {
	Lane          string
	Current, Next float64
	SpreadBPS     int64
	GainRaw       float64
	CostRaw       float64
	Reason        string
	BorrowRaw     uint64
	// RoomRaw is the debt reserve's additional debt room this sample.
	RoomRaw uint64
	// Cost is the priced step's expected cost (CostRaw is its total).
	Cost leverageStepCost
	// TermDays is the lane's days to its term debt's maturity, when it owes
	// term debt (decide_term.go).
	TermDays  float64
	TermKnown bool
}

// decideLeverageTarget applies the option-1 rule to one funded lane's view,
// the level setting target() reads (backyard_lane_levels). It runs only on a
// settled lane: nothing nonterminal, no withdrawal, no custody in flight, and
// the selector itself chose KEEP. An up move needs the
// level's spread (1% to 1.5x, 2% to 1.75x) and a positive gain over KEEP
// after the move's own cost and the performance fee on the extra income. It
// does not need the selector's MinimumBenefit, which is SWITCH hysteresis
// against a full unwind, not a cost of borrowing more in place.
// DOWN/no-change never use this gate.
//
// The move is sized at the whole step to the next level (desiredLeverage
// Borrow), not at the debt reserve's momentary room, so the approval stands
// while the reserve is full. Its rate is the curve at the step's utilization,
// capped at the reserve's utilization borrow limit (leverageRateDebtRaw): no
// executable part of the step borrows above it. The worker borrows each
// tick's capacity-sized part of it the moment room opens (leverageUpReceive).
//
// Borrowing more in place changes the same vault's income by the increment
// only, so the gain compares like with like: the candidate's and KEEP's
// gross income over the horizon, the full performance fee on a positive
// difference (a vault below its high-water mark pays less, never more), then
// the move's cost. The selector's SWITCH forecast instead reserves fees on
// the whole position's income against KEEP's fee-favoured upper bound; on a
// $100k position that asymmetry alone outweighed any in-place borrow the
// debt reserve could fund (live 2026-10-10: AUTO held 1.01x under a 1.75x
// target while PYUSD had $3k-$5k of room for five hours).
//
// The cost is the step's expected cost (leverage_step_cost.go): the KLend
// origination fee, the expected swap loss in and out at the step's size from
// live quotes, and the six transactions' network fees, all at central
// prices. It was 2 x UncertaintyBPS of the borrowed amount: a haircut that
// bounds execution, not what capital loses, and at AUTO's realized spread
// (~2.9 pts) it refused every up step whatever the room. Uncertainty stays
// where it bounds execution: the swap minimums and the recipe limits. The
// income forecast values the borrow and its proceeds at central prices too.
// An unpriced step is leverage_cost_unavailable, never free.
func decideLeverageTarget(s Snapshot, selector SelectorResult, markets []LaneEconomics, p SelectorPolicy, price leverageStepPricer) (leverageDecision, bool) {
	out := leverageDecision{Lane: s.RouteLane, RoomRaw: s.AdditionalDebtRoomRaw}
	out.TermDays, out.TermKnown = termDays(s)
	if !earnActiveLane(s.RouteLane) || !s.HasPosition || s.PositionCollateralRaw <= 0 ||
		selector.Action != "KEEP" || s.Nonterminal != "" || s.WithdrawalDemandRaw != 0 ||
		s.SquadsIdleRaw != 0 || s.DebtIdleRaw != 0 || s.VoltrStrategyIdleRaw != 0 ||
		(s.CollateralIdleRaw > 0 && s.MinimumCollateralDepositRaw > 0 && s.CollateralIdleRaw >= s.MinimumCollateralDepositRaw) {
		return out, false
	}
	var market *LaneEconomics
	for i := range markets {
		if markets[i].Lane == s.RouteLane {
			market = &markets[i]
		}
	}
	equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
	if market == nil || equity <= 0 {
		return out, false
	}
	// The rule steps from the STORED target, not the position: a target the
	// position has not reached yet (borrowing blocked) is still the decided
	// level, so re-deciding from the position flipped it (live 2026-09-28:
	// 1.5x <-> 1x writes, each bumping the generation). Without a stored
	// target the position level is the base. Live levels stop at
	// leverageMaxLiveLevel (see leverage_up.go).
	positionLevel := min(currentLeverageBand(s), leverageMaxLiveLevel)
	out.Current = positionLevel
	if s.LeverageTargetLevel > 0 {
		out.Current = min(s.LeverageTargetLevel, leverageMaxLiveLevel)
	}
	// Down rules use current borrowing cost even when new capacity is unknown.
	currentSpread := market.NativeAPY + market.SupplyAPY - market.CurrentBorrowAPY
	spreadAt := func(level float64) (float64, bool) {
		if level <= out.Current {
			return currentSpread, finite(currentSpread) && market.CurrentBorrowAPY >= 0
		}
		raw := leverageDesiredRaw(s, level)
		if raw < leverageMinimumBorrowRaw {
			return 0, false
		}
		fee, err := kaminoBorrowFeeAtRate(s.BorrowFeeRate, raw)
		if err != nil {
			return 0, false
		}
		return leverageBorrowSpread(*market, float64(leverageRateDebtRaw(s, raw+fee)))
	}
	out.Next = min(nextLiveLeverageLevel(out.Current, spreadAt), leverageMaxLiveLevel)
	out.SpreadBPS = int64(currentSpread * 10_000)
	out.Reason = "spread_rule"
	if out.Next < out.Current {
		return out, true
	}
	// A stored desired ceiling may be above the actual position. Price the
	// whole remaining step, including its entire fee/cost.
	candidateSnapshot := s
	candidateSnapshot.LeverageTargetLevel = out.Next
	level := leverageUpLevel(candidateSnapshot)
	raw := leverageDesiredRaw(s, level)
	if raw < leverageMinimumBorrowRaw {
		if level > 0 && !s.BorrowCapacityKnown {
			out.Reason = "borrow_capacity_unknown"
		} else if level > 0 {
			out.Reason = "leverage_step_below_minimum"
		}
		return out, true
	}
	fee, err := kaminoBorrowFeeAtRate(s.BorrowFeeRate, raw)
	proceeds, priceErr := capacityBorrowCentralValue(s, raw, false)
	liability, debtErr := capacityBorrowCentralValue(s, raw+fee, true)
	rateDebt := float64(leverageRateDebtRaw(s, raw+fee))
	apr, rateErr := projectedBorrowAPR(*market, rateDebt)
	spread, allowed := leverageUpAllowed(*market, level, rateDebt)
	if err != nil || priceErr != nil || debtErr != nil || rateErr != nil {
		out.Next, out.Reason = out.Current, "leverage_rate_unavailable"
		return out, true
	}
	if !allowed {
		out.Next = out.Current
		return out, true
	}
	out.SpreadBPS = int64(spread * 10_000)
	moved := float64(proceeds)
	if !s.MonitorsArmed || !approvedVoltrFeeTerms(s) {
		out.Next, out.Reason = out.Current, "fee_terms_unavailable"
		return out, true
	}
	if p.validate() != nil || s.CollateralIdleRaw > 0 || s.PositionDebtValueRaw < 0 || !finite(market.CurrentBorrowAPY) || market.CurrentBorrowAPY < 0 {
		out.Next, out.Reason = out.Current, "fee_forecast_unavailable"
		return out, true
	}
	if price == nil {
		out.Next, out.Reason = out.Current, "leverage_cost_unavailable"
		return out, true
	}
	stepCost, costErr := price(raw)
	if costErr != nil || stepCost.TotalMicros <= 0 || stepCost.TotalMicros > 1<<53 {
		out.Next, out.Reason = out.Current, leverageCostOutageReason(costErr)
		return out, true
	}
	out.Cost, out.CostRaw = stepCost, float64(stepCost.TotalMicros)
	years := p.Horizon.Hours() / (365.25 * 24)
	collateral, debt := float64(s.PositionCollateralValueRaw), float64(s.PositionDebtValueRaw)
	keepGross := forecastGain(collateral, collateral, debt, *market, math.Log1p(market.CurrentBorrowAPY), years)
	// The held collateral is not reduced by the move; its cost is. Pass the
	// collateral the move leaves earning, net of that cost; the candidate's
	// income is its gain before the cost is charged.
	cost := math.Ceil(out.CostRaw)
	candidate := pilotForecastEconomics(pilotEconomics{Debt: debt + float64(liability), Proceeds: moved, APR: apr}, collateral-cost, *market, years, int64(cost))
	extra := candidate.Gain + cost - keepGross
	if !finite(extra) || !finite(keepGross) {
		out.Next, out.Reason = out.Current, "fee_forecast_unavailable"
		return out, true
	}
	if extra > 0 {
		extra *= 1 - float64(approvedAdminPerformanceFeeBPS)/10_000
	}
	out.GainRaw = extra - cost
	if out.GainRaw <= 0 {
		out.Next, out.Reason = out.Current, "up_move_not_worth_cost"
	} else {
		out.Reason = "up_move_approved"
		out.BorrowRaw = raw
	}
	return out, true
}

// level is the decision as the lane's stored level: the level, and whether
// the up step toward it is approved.
func (d leverageDecision) level() laneLevel {
	return laneLevel{Level: d.Next, UpApproved: d.Next > 1 && d.BorrowRaw >= leverageMinimumBorrowRaw}
}

// leverageOutageReasons are the decisions that are not decisions: the step
// could not be evaluated (capacity, rate or cost unknown). They carry the
// stored level and approval, as the selector carries its persistence
// windows through outages; only an economic refusal (spread_rule,
// up_move_not_worth_cost), a level change or a risk or fee-terms change
// revokes an approval.
func (d leverageDecision) outage() bool {
	switch d.Reason {
	case "borrow_capacity_unknown", "leverage_rate_unavailable", "fee_forecast_unavailable", "leverage_cost_unavailable":
		return true
	}
	return strings.HasPrefix(d.Reason, "leverage_cost_unavailable(")
}

// storeChanges reports whether d changes the lane's stored level or its
// approval; an outage changes neither.
func (d leverageDecision) storeChanges(s Snapshot) bool {
	if d.outage() {
		return false
	}
	return d.level() != laneLevel{Level: s.LeverageTargetLevel, UpApproved: s.LeverageUpApproved}
}

// decideOverflowLevel is the level an overflow lane its source cannot lever
// into takes before it holds anything: the live cap, approved while the
// overflow lane's spread at the borrow its room would make clears the
// level's rule. Once the overflow lane holds a position its own view decides.
func decideOverflowLevel(source Snapshot, markets []LaneEconomics, p SelectorPolicy, now time.Time) (string, laneLevel, bool) {
	lane, ok := overflowLaneOf(source.RouteLane)
	if !ok || !source.HasPosition || !overflowSourceBorrowClosed(source) || !source.OverflowRoomKnown || containsLane(source.heldLanes(), lane) {
		return "", laneLevel{}, false
	}
	e := overflowEquityRaw(source)
	if e <= 0 {
		return "", laneLevel{}, false
	}
	for _, m := range markets {
		if m.Lane != lane || m.validate(now, p) != nil || m.DebtRawPerUSDCRaw <= 0 {
			continue
		}
		// The step's projected utilization: its whole borrow, in the debt
		// reserve's raw units (forecast only).
		_, allowed := leverageUpAllowed(m, leverageMaxLiveLevel, float64(e)*(leverageMaxLiveLevel-1)*m.DebtRawPerUSDCRaw)
		return lane, laneLevel{Level: leverageMaxLiveLevel, UpApproved: allowed}, true
	}
	return "", laneLevel{}, false
}

func (d leverageDecision) logLine() string {
	term := "-"
	if d.TermKnown {
		term = fmt.Sprintf("%.2f", d.TermDays)
	}
	return fmt.Sprintf("backyard-rwa-worker: leverage decision lane=%s %.2fx->%.2fx spread=%.2f gain=%.0f cost=%.0f (fee=%d in=%d out=%d network=%d) borrow=%d room=%d term_days=%s reason=%s",
		d.Lane, d.Current, d.Next, float64(d.SpreadBPS)/100, d.GainRaw, d.CostRaw, d.Cost.BorrowFeeMicros, d.Cost.EntrySwapLossMicros, d.Cost.ExitSwapLossMicros, d.Cost.NetworkMicros, d.BorrowRaw, d.RoomRaw, term, d.Reason)
}

// leverageDecisionLog rate-limits the 'leverage decision' line: it prints
// when the outcome (level, reason, approved borrow) changes, otherwise at
// most once an hour (live 2026-09-28: a 1x->1.5x decision held by blocked
// borrowing printed on every 15 s selector sample). Holds print too: the
// stored target is the cap, so an approved or refused up move keeps
// Next == Current, and keying on the level alone hid every one of them
// (live 2026-10-10). Each lane keeps its own line.
type leverageDecisionLog struct {
	printed map[string]time.Time
	last    map[string]leverageDecisionKey
}

// The key leaves out the drifting numbers (approved size, room); whether an
// approval and room exist are in it.
type leverageDecisionKey struct {
	next     float64
	reason   string
	approved bool
	room     bool
}

func (l *leverageDecisionLog) due(now time.Time, d leverageDecision) bool {
	if l.printed == nil {
		l.printed, l.last = map[string]time.Time{}, map[string]leverageDecisionKey{}
	}
	key := leverageDecisionKey{d.Next, d.Reason, d.BorrowRaw > 0, d.RoomRaw >= leverageMinimumBorrowRaw}
	if key != l.last[d.Lane] || now.Sub(l.printed[d.Lane]) >= time.Hour {
		l.printed[d.Lane], l.last[d.Lane] = now, key
		return true
	}
	return false
}
