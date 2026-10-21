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
	TermDays      float64
	TermKnown     bool
}

func (d leverageDecision) held(reason string) leverageDecision {
	d.Next, d.Reason = d.Current, reason
	return d
}

func leverageSettled(s Snapshot, selector SelectorResult) bool {
	return earnActiveLane(s.RouteLane) && s.HasPosition && s.PositionCollateralRaw > 0 &&
		selector.Action == SelectorKeep && s.Nonterminal == "" && withdrawalShortfallRaw(s) == 0 &&
		s.SquadsIdleRaw == 0 && s.VoltrStrategyIdleRaw == 0
}

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
	level, up := leverageUpLevel(candidateSnapshot)
	if up && level < out.Next {
		out.Next = max(level, out.Current)
	}
	raw := leverageDesiredRaw(s, level)
	if raw < leverageMinimumBorrowRaw {
		if up && !s.BorrowCapacityKnown {
			out.Reason = leverageReasonCapacityUnknown
		} else if up {
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
	out.GainRaw = upStepGain(market, upStep{CollateralMicros: float64(s.PositionCollateralValueRaw), DebtMicros: float64(s.PositionDebtValueRaw),
		LiabilityMicros: float64(liability), ProceedsMicros: float64(proceeds), KeepAPR: math.Log1p(market.CurrentBorrowAPY), APR: apr,
		CostMicros: float64(stepCost.TotalMicros), Years: years, FeeBPS: s.ApprovedAdminPerformanceFeeBPS})
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

// upStep is one candidate up move, every amount in USDC micros: a position
// of collateral and debt borrowing liability more, proceeds of it converted
// into collateral, every debt at APR after the step, against KEEP's income
// with its debt at KeepAPR.
type upStep struct {
	CollateralMicros, DebtMicros, LiabilityMicros, ProceedsMicros float64
	KeepAPR, APR                                                  float64
	CostMicros, Years                                             float64
	FeeBPS                                                        int64
}

func upStepGain(m LaneEconomics, u upStep) float64 {
	keepGross := forecastGain(u.CollateralMicros, u.CollateralMicros, u.DebtMicros, m, u.KeepAPR, u.Years)
	cost := math.Ceil(u.CostMicros)
	candidate := pilotForecastEconomics(pilotEconomics{Debt: u.DebtMicros + u.LiabilityMicros, Proceeds: u.ProceedsMicros, APR: u.APR}, u.CollateralMicros-cost, m, u.Years, int64(cost))
	extra := candidate.Gain + cost - keepGross
	if extra > 0 {
		extra *= 1 - float64(u.FeeBPS)/10_000
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
