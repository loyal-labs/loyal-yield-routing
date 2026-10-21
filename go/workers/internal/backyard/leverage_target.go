package backyard

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	leverageReasonSpreadRule             = "spread_rule"
	leverageReasonCapacityUnknown        = "borrow_capacity_unknown"
	leverageReasonStepBelowMinimum       = "leverage_step_below_minimum"
	leverageReasonRateUnavailable        = "leverage_rate_unavailable"
	leverageReasonFeeTermsUnavailable    = "fee_terms_unavailable"
	leverageReasonFeeForecastUnavailable = "fee_forecast_unavailable"
	leverageReasonCostUnavailable        = "leverage_cost_unavailable"
	leverageReasonNotWorthCost           = "up_move_not_worth_cost"
	leverageReasonApproved               = "up_move_approved"
)

type leverageDecision struct {
	Lane          string
	Current, Next float64
	SpreadBPS     int64
	GainRaw       float64
	Reason        string
	BorrowRaw     uint64
	RoomRaw       uint64
	Cost          leverageStepCost
	// TermDays is the lane's days to its term debt's maturity, when it owes term debt (decide_term.go).
	TermDays  float64
	TermKnown bool
}

func (d leverageDecision) held(reason string) leverageDecision {
	d.Next, d.Reason = d.Current, reason
	return d
}

func leverageSettled(s Snapshot, selector SelectorResult) bool {
	return earnActiveLane(s.RouteLane) && s.HasPosition && s.PositionCollateralRaw > 0 &&
		selector.Action == "KEEP" && s.Nonterminal == "" && withdrawalShortfallRaw(s) == 0 &&
		s.SquadsIdleRaw == 0 && s.VoltrStrategyIdleRaw == 0
}

// decideLeverageTarget sets the level target() reads for one settled lane.
// An up move needs the level's spread (leverageLadder) and a positive gain over KEEP after the move's priced cost; an unpriced step is leverage_cost_unavailable, never free.
func decideLeverageTarget(s Snapshot, selector SelectorResult, markets []LaneEconomics, p SelectorPolicy, price leverageStepPricer) (leverageDecision, bool) {
	out := leverageDecision{Lane: s.RouteLane, RoomRaw: s.AdditionalDebtRoomRaw}
	out.TermDays, out.TermKnown = termDays(s)
	market := economicsOf(markets, s.RouteLane)
	if !leverageSettled(s, selector) || market == nil || s.PositionCollateralValueRaw-s.PositionDebtValueRaw <= 0 {
		return out, false
	}
	maxLevel := laneMaxLevel(s.RouteLane)
	out.Current = min(currentLeverageBand(s), maxLevel)
	if s.LeverageTargetLevel > 0 {
		out.Current = min(s.LeverageTargetLevel, maxLevel)
	}
	currentSpread := market.NativeAPY + market.SupplyAPY - market.CurrentBorrowAPY
	spreadAt := func(level float64) (float64, bool) {
		if level <= out.Current {
			return currentSpread, finite(currentSpread) && market.CurrentBorrowAPY >= 0
		}
		debt, ok := heldRungDebt(s, level)
		if !ok {
			return 0, false
		}
		return leverageBorrowSpread(*market, debt)
	}
	out.Next = nextLeverageLevel(out.Current, maxLevel, spreadAt)
	out.SpreadBPS = int64(currentSpread * 10_000)
	out.Reason = leverageReasonSpreadRule
	if out.Next < out.Current {
		return out, true
	}
	candidateSnapshot := s
	candidateSnapshot.LeverageTargetLevel = out.Next
	level := leverageUpLevel(candidateSnapshot)
	if level > 0 && level < out.Next {
		out.Next = max(level, out.Current)
	}
	raw := leverageDesiredRaw(s, level)
	if raw < leverageMinimumBorrowRaw {
		if level > 0 && !s.BorrowCapacityKnown {
			out.Reason = leverageReasonCapacityUnknown
		} else if level > 0 {
			out.Reason = leverageReasonStepBelowMinimum
		}
		return out, true
	}
	return out.priceUpStep(s, *market, level, raw, p, price), true
}

func (out leverageDecision) priceUpStep(s Snapshot, market LaneEconomics, level float64, raw uint64, p SelectorPolicy, price leverageStepPricer) leverageDecision {
	fee, err := kaminoBorrowFeeAtRate(s.BorrowFeeRate, raw)
	proceeds, priceErr := capacityBorrowExpectedUSDC(s, raw, false)
	liability, debtErr := capacityBorrowExpectedUSDC(s, raw+fee, true)
	rateDebt := float64(leverageRateDebtRaw(s, raw+fee))
	apr, rateErr := projectedBorrowAPR(market, rateDebt)
	spread, allowed := leverageUpAllowed(market, level, rateDebt)
	if err != nil || priceErr != nil || debtErr != nil || rateErr != nil {
		return out.held(leverageReasonRateUnavailable)
	}
	if !allowed {
		out.Next = out.Current
		return out
	}
	out.SpreadBPS = int64(spread * 10_000)
	if !s.MonitorsArmed || !approvedVoltrFeeTerms(s) {
		return out.held(leverageReasonFeeTermsUnavailable)
	}
	if p.validate() != nil || s.PositionDebtValueRaw < 0 || !finite(market.CurrentBorrowAPY) || market.CurrentBorrowAPY < 0 {
		return out.held(leverageReasonFeeForecastUnavailable)
	}
	if price == nil {
		return out.held(leverageReasonCostUnavailable)
	}
	stepCost, costErr := price(raw)
	if costErr != nil || stepCost.TotalMicros < 0 || stepCost.TotalMicros > 1<<53 {
		return out.held(leverageCostOutageReason(costErr))
	}
	out.Cost = stepCost
	years := p.Horizon.Hours() / (365.25 * 24)
	out.GainRaw = upStepGain(market, float64(s.PositionCollateralValueRaw), float64(s.PositionDebtValueRaw), float64(liability), float64(proceeds),
		math.Log1p(market.CurrentBorrowAPY), apr, float64(stepCost.TotalMicros), years, s.ApprovedAdminPerformanceFeeBPS)
	if !finite(out.GainRaw) {
		out = out.held(leverageReasonFeeForecastUnavailable)
		out.GainRaw = 0
		return out
	}
	if out.GainRaw <= 0 {
		return out.held(leverageReasonNotWorthCost)
	}
	out.Reason = leverageReasonApproved
	out.BorrowRaw = raw
	return out
}

// upStepGain is the live rule's gain of one up step over the horizon: a
// position of collateral and debt (USDC) borrowing liability more, proceeds
// of it converted into collateral, every debt at apr after the step, against
// KEEP's gross income with its debt at keepAPR; the performance fee is taken
// on a positive difference (a vault below its high-water mark pays less,
// never more), then the step's cost. The step pays when it is positive.
func upStepGain(m LaneEconomics, collateral, debt, liability, proceeds, keepAPR, apr, cost, years float64, feeBPS int64) float64 {
	keepGross := forecastGain(collateral, collateral, debt, m, keepAPR, years)
	// The held collateral is not reduced by the move; its cost is. Pass the
	// collateral the move leaves earning, net of that cost; the candidate's
	// income is its gain before the cost is charged.
	cost = math.Ceil(cost)
	candidate := pilotForecastEconomics(pilotEconomics{Debt: debt + liability, Proceeds: proceeds, APR: apr}, collateral-cost, m, years, int64(cost))
	extra := candidate.Gain + cost - keepGross
	if extra > 0 {
		extra *= 1 - float64(feeBPS)/10_000
	}
	return extra - cost
}

func (d leverageDecision) level() laneLevel {
	return laneLevel{Level: d.Next, UpApproved: d.Next > 1 && d.BorrowRaw >= leverageMinimumBorrowRaw}
}

func (d leverageDecision) outage() bool {
	switch d.Reason {
	case leverageReasonCapacityUnknown, leverageReasonRateUnavailable, leverageReasonFeeForecastUnavailable, leverageReasonCostUnavailable:
		return true
	}
	return strings.HasPrefix(d.Reason, leverageReasonCostUnavailable+"(")
}

func (d leverageDecision) storeChanges(s Snapshot) bool {
	if d.outage() {
		return false
	}
	return d.level() != laneLevel{Level: s.LeverageTargetLevel, UpApproved: s.LeverageUpApproved}
}

func decideOverflowLevel(source Snapshot, markets []LaneEconomics, p SelectorPolicy, now time.Time) (string, laneLevel, bool) {
	lane, ok := overflowLaneOf(source.RouteLane)
	if !ok || !source.HasPosition || !overflowSourceBorrowClosed(source) || source.OverflowRoom == nil || containsLane(source.heldLanes(), lane) {
		return "", laneLevel{}, false
	}
	maxLevel := laneMaxLevel(lane)
	e := overflowEquityRaw(source, maxLevel)
	if e <= 0 {
		return "", laneLevel{}, false
	}
	for _, m := range markets {
		if m.Lane != lane || m.validate(now, p) != nil || m.DebtRawPerUSDCRaw <= 0 {
			continue
		}
		_, allowed := leverageUpAllowed(m, maxLevel, wholeStepBorrowDebtRaw(e, maxLevel, m))
		return lane, laneLevel{Level: maxLevel, UpApproved: allowed}, true
	}
	return "", laneLevel{}, false
}

func wholeStepBorrowDebtRaw(equityRaw int64, maxLevel float64, m LaneEconomics) float64 {
	return float64(equityRaw) * (maxLevel - 1) * m.DebtRawPerUSDCRaw
}

func (d leverageDecision) logLine() string {
	term := "-"
	if d.TermKnown {
		term = fmt.Sprintf("%.2f", d.TermDays)
	}
	return fmt.Sprintf("backyard-rwa-worker: leverage decision lane=%s %.2fx->%.2fx spread=%.2f gain=%.0f cost=%.0f (fee=%d in=%d out=%d network=%d) borrow=%d room=%d term_days=%s reason=%s",
		d.Lane, d.Current, d.Next, float64(d.SpreadBPS)/100, d.GainRaw, float64(d.Cost.TotalMicros), d.Cost.BorrowFeeMicros, d.Cost.EntrySwapCostMicros, d.Cost.ExitSwapCostMicros, d.Cost.NetworkMicros, d.BorrowRaw, d.RoomRaw, term, d.Reason)
}

type leverageDecisionLog struct {
	printed map[string]time.Time
	last    map[string]leverageDecisionKey
}

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
