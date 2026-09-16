package backyardrwa

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Capital amounts are raw USDC (six decimals). Forecasts are estimates only;
// transaction amounts, reservations and reconciliation keep integer arithmetic.
// The current executor deposits equity, borrows 50% once, then redeposits it.
const singlePassLeverage = 1 + float64(TargetLTVBPS)/10_000

var selectorLanes = []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"}

func selectorLane(lane string) bool {
	for _, allowed := range selectorLanes {
		if lane == allowed {
			return true
		}
	}
	return false
}

// Capacity distinguishes unknown, a closed entry, and an explicitly unlimited
// limit. It is equity capacity for the exact pair and execution recipe, not a
// reserve's aggregate available liquidity or the collateral's borrow limit.
type Capacity struct {
	Known     bool  `json:"known"`
	Unlimited bool  `json:"unlimited"`
	Raw       int64 `json:"raw"`
}

func (c Capacity) amount(equity int64) (int64, bool) {
	if equity < 0 || !c.Known || c.Raw < 0 || (c.Unlimited && c.Raw != 0) {
		return 0, false
	}
	if c.Unlimited {
		return equity, true
	}
	return min(equity, c.Raw), true
}

type BorrowCurvePoint struct {
	UtilizationBPS float64 `json:"utilization_rate_bps"`
	BorrowBPS      float64 `json:"borrow_rate_bps"`
}

// Economics uses effective annual rates for supply/native income and nominal
// APR for the borrow curve, including the fixed host rate separately. Rewards
// are omitted until eligibility and recurrence can be proven.
type LaneEconomics struct {
	Lane               string             `json:"lane"`
	EvidenceID         string             `json:"evidenceId"`
	ObservedAt         time.Time          `json:"observedAt"`
	NativeObservedAt   time.Time          `json:"nativeObservedAt"`
	NativeAPY          float64            `json:"nativeApy"`
	SupplyAPY          float64            `json:"supplyApy"`
	CurrentBorrowAPY   float64            `json:"currentBorrowApy"`
	BorrowCurve        []BorrowCurvePoint `json:"borrowCurve"`
	HostBorrowBPS      float64            `json:"hostBorrowBps"`
	DebtSupplyRaw      float64            `json:"debtSupplyUsdcRaw"`
	DebtBorrowRaw      float64            `json:"debtBorrowUsdcRaw"`
	EntryCapacity      Capacity           `json:"entryCapacity"`
	EntryBlockedReason string             `json:"entryBlockedReason,omitempty"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func freshAt(now, at time.Time, age time.Duration) bool {
	return !at.IsZero() && !at.After(now) && now.Sub(at) <= age
}
func (e LaneEconomics) validate(now time.Time, p SelectorPolicy) error {
	if !selectorLane(e.Lane) || e.EvidenceID == "" || !freshAt(now, e.ObservedAt, p.MarketMaxAge) || !freshAt(now, e.NativeObservedAt, p.NativeMaxAge) {
		return fmt.Errorf("economic_evidence_unavailable")
	}
	for _, rate := range []float64{e.NativeAPY, e.SupplyAPY, e.CurrentBorrowAPY} {
		if !finite(rate) || rate <= -1 || rate > 10 {
			return fmt.Errorf("invalid_economic_rate")
		}
	}
	if e.SupplyAPY < 0 || e.CurrentBorrowAPY < 0 || !finite(e.HostBorrowBPS) || e.HostBorrowBPS < 0 || e.HostBorrowBPS > 100_000 || !finite(e.DebtSupplyRaw) || !finite(e.DebtBorrowRaw) || e.DebtSupplyRaw <= 0 || e.DebtBorrowRaw < 0 || e.DebtBorrowRaw > e.DebtSupplyRaw {
		return fmt.Errorf("invalid_debt_market")
	}
	_, err := projectedBorrowAPR(e, 0)
	return err
}
func projectedBorrowAPR(e LaneEconomics, additionalDebt float64) (float64, error) {
	if !finite(additionalDebt) || additionalDebt < 0 || e.DebtSupplyRaw <= 0 || e.DebtBorrowRaw+additionalDebt > e.DebtSupplyRaw {
		return 0, fmt.Errorf("projected_debt_unavailable")
	}
	points := e.BorrowCurve
	// KLend stores eleven points and pads the tail with the identical terminal
	// 100% utilization point. Only that exact padding is redundant.
	for len(points) > 2 && points[len(points)-1].UtilizationBPS == 10_000 && points[len(points)-1] == points[len(points)-2] {
		points = points[:len(points)-1]
	}
	if len(points) < 2 || points[0].UtilizationBPS != 0 || points[len(points)-1].UtilizationBPS != 10_000 {
		return 0, fmt.Errorf("invalid_borrow_curve")
	}
	for i, v := range points {
		if !finite(v.UtilizationBPS) || !finite(v.BorrowBPS) || v.UtilizationBPS < 0 || v.UtilizationBPS > 10_000 || v.BorrowBPS < 0 || v.BorrowBPS > 1_000_000 || (i > 0 && (v.UtilizationBPS <= points[i-1].UtilizationBPS || v.BorrowBPS < points[i-1].BorrowBPS)) {
			return 0, fmt.Errorf("invalid_borrow_curve")
		}
	}
	u := (e.DebtBorrowRaw + additionalDebt) / e.DebtSupplyRaw * 10_000
	for i := 1; i < len(points); i++ {
		if u <= points[i].UtilizationBPS {
			a, b := points[i-1], points[i]
			return (a.BorrowBPS + (b.BorrowBPS-a.BorrowBPS)*(u-a.UtilizationBPS)/(b.UtilizationBPS-a.UtilizationBPS) + e.HostBorrowBPS) / 10_000, nil
		}
	}
	return 0, fmt.Errorf("projected_utilization_out_of_range")
}

type SelectorPolicy struct {
	Horizon           time.Duration `json:"horizon"`
	Persistence       time.Duration `json:"persistence"`
	MaxSampleGap      time.Duration `json:"maxSampleGap"`
	MarketMaxAge      time.Duration `json:"marketMaxAge"`
	NativeMaxAge      time.Duration `json:"nativeMaxAge"`
	QuoteMaxAge       time.Duration `json:"quoteMaxAge"`
	MinimumBenefitRaw int64         `json:"minimumBenefitRaw"`
	UncertaintyBPS    int64         `json:"uncertaintyBps"`
	IdleBufferRaw     int64         `json:"idleBufferRaw"`
}

func DefaultSelectorPolicy() SelectorPolicy {
	// Shadow starting values; activation must review observed churn and costs.
	return SelectorPolicy{Horizon: 7 * 24 * time.Hour, Persistence: 30 * time.Minute, MaxSampleGap: 2 * time.Minute, MarketMaxAge: 90 * time.Second, NativeMaxAge: 2 * time.Hour, QuoteMaxAge: 30 * time.Second, MinimumBenefitRaw: 250_000, UncertaintyBPS: 10, IdleBufferRaw: 0}
}
func (p SelectorPolicy) validate() error {
	if p.Horizon <= 0 || p.Horizon > 365*24*time.Hour || p.Persistence <= 0 || p.MaxSampleGap <= 0 || p.MarketMaxAge <= 0 || p.NativeMaxAge <= 0 || p.QuoteMaxAge <= 0 || p.MinimumBenefitRaw < 0 || p.UncertaintyBPS < 0 || p.UncertaintyBPS > 10_000 || p.IdleBufferRaw < 0 {
		return fmt.Errorf("invalid_selector_policy")
	}
	return nil
}

// MoveQuote values the entire proposed source exit + destination entry, including
// setup, swap losses, origination and transaction fees for the full recipe.
// Returned principal is not a cost: the budget separately counts gross debits.
// It is bound to actual equity, source state, destination and exact policy set.
// Remaining exit spending belongs to the existing budget, not this quote.
type MoveQuote struct {
	SourceLane      string    `json:"sourceLane"`
	DestinationLane string    `json:"destinationLane"`
	ObservationID   string    `json:"observationId"`
	EquityRaw       int64     `json:"equityRaw"`
	CostRaw         int64     `json:"costRaw"`
	ObservedAt      time.Time `json:"observedAt"`
	EvidenceID      string    `json:"evidenceId"`
}

type SelectorInput struct {
	Now      time.Time
	Snapshot Snapshot
	Markets  []LaneEconomics
	Quotes   []MoveQuote
	Policy   SelectorPolicy
}

// Only economic persistence lives here. A source exit is committed by the
// existing route state's UnwindIntent after execution admission; forecasts do
// not manufacture authorization, reservations, or a second cash ledger.
type AdvantageWindow struct {
	Since      time.Time `json:"since"`
	LastSample time.Time `json:"lastSample"`
}
type SelectorState struct {
	SourceLane string                     `json:"sourceLane,omitempty"`
	Advantages map[string]AdvantageWindow `json:"advantages,omitempty"`
}

type CandidateForecast struct {
	Lane          string  `json:"lane"`
	InvestedRaw   int64   `json:"investedRaw"`
	IdleRaw       int64   `json:"idleRaw"`
	GrossGainRaw  float64 `json:"grossGainRaw"`
	GainRaw       float64 `json:"gainRaw"`
	CostsKnown    bool    `json:"costsKnown"`
	BenefitRaw    float64 `json:"benefitRaw"`
	BorrowAPR     float64 `json:"borrowApr"`
	BlockedReason string  `json:"blockedReason,omitempty"`
}
type SelectorResult struct {
	Action          string              `json:"action"`
	Reason          string              `json:"reason"`
	SourceLane      string              `json:"sourceLane,omitempty"`
	DestinationLane string              `json:"destinationLane,omitempty"`
	EquityRaw       int64               `json:"equityRaw"`
	KeepGainRaw     float64             `json:"keepGainRaw"`
	Candidates      []CandidateForecast `json:"candidates"`
	State           SelectorState       `json:"state"`
}

func forecastGain(collateral, supplied, debt float64, e LaneEconomics, borrowAPR, years float64) float64 {
	// Native yield on all owned collateral; lending yield only on supplied units.
	return (collateral-supplied)*math.Expm1(math.Log1p(e.NativeAPY)*years) + supplied*math.Expm1((math.Log1p(e.NativeAPY)+math.Log1p(e.SupplyAPY))*years) - debt*math.Expm1(borrowAPR*years)
}

// SelectOpportunity never creates a transaction. The serialized worker must
// apply fresh policy/exit admission before committing an unwind or entry.
func SelectOpportunity(in SelectorInput, previous SelectorState) SelectorResult {
	s, p := in.Snapshot, in.Policy
	out := SelectorResult{Action: "KEEP", Reason: "no_worthwhile_move", SourceLane: s.RouteLane}
	hold := func(reason string) SelectorResult { out.Reason = reason; out.State = SelectorState{}; return out }
	if err := p.validate(); err != nil || in.Now.IsZero() {
		return hold("invalid_selector_policy")
	}
	base := Decide(s)
	if base.Action == RecoverTransaction || base.Action == HoldManualRecovery || base.Action == DeleverRouteStep || base.Action == DeleverPrimeUSDCStep || base.Reason == "hard_ltv_buffer_swap" || s.Nonterminal != "" {
		return hold("execution_recovery_or_safety_first")
	}
	if s.WithdrawalDemandRaw > 0 || s.Unwind || s.CutoverDrain || s.VoltrStrategyIdleRaw > 0 {
		return hold("withdrawal_unwind_or_accounting_first")
	}
	if !s.Fresh || s.SquadsIdleRaw < 0 || s.TotalVaultNAVRaw > 1<<53 || s.TotalVaultNAVRaw < 0 || s.PositionCollateralValueRaw < 0 || s.PositionDebtValueRaw < 0 || s.CollateralIdleValueRaw < 0 {
		return hold("invalid_holdings")
	}
	// USDC stays idle; borrowed cash is already offset by observed debt in NAV.
	equity := s.TotalVaultNAVRaw - p.IdleBufferRaw
	if equity <= 0 {
		return hold("no_investable_equity")
	}
	out.EquityRaw = equity
	markets := map[string]LaneEconomics{}
	for _, m := range in.Markets {
		if _, ok := markets[m.Lane]; ok {
			return hold("duplicate_market")
		}
		markets[m.Lane] = m
	}
	years := p.Horizon.Hours() / (365.25 * 24)
	exposed := s.HasPosition || s.PositionDebtRaw > 0 || s.PositionCollateralRaw > 0 || s.CollateralIdleRaw > 0
	if exposed {
		m, ok := markets[s.RouteLane]
		if !ok || m.validate(in.Now, p) != nil {
			return hold("current_lane_economics_unavailable")
		}
		out.KeepGainRaw = forecastGain(float64(s.PositionCollateralValueRaw)+float64(s.CollateralIdleValueRaw), float64(s.PositionCollateralValueRaw), float64(s.PositionDebtValueRaw), m, math.Log1p(m.CurrentBorrowAPY), years)
		if !finite(out.KeepGainRaw) {
			return hold("invalid_keep_forecast")
		}
	}
	lanes := make([]string, 0, len(markets))
	for lane := range markets {
		lanes = append(lanes, lane)
	}
	sort.Strings(lanes)
	out.State.SourceLane = s.RouteLane
	if previous.SourceLane != s.RouteLane {
		previous = SelectorState{}
	}
	out.State.Advantages = map[string]AdvantageWindow{}
	best := -1
	for _, lane := range lanes {
		m := markets[lane]
		c := CandidateForecast{Lane: lane}
		if err := m.validate(in.Now, p); err != nil {
			c.BlockedReason = err.Error()
			out.Candidates = append(out.Candidates, c)
			continue
		}
		if exposed && lane == s.RouteLane {
			c.BlockedReason = "current_position_is_keep_baseline"
			out.Candidates = append(out.Candidates, c)
			continue
		}
		// Economic persistence is independent of an entry-capacity opening or a
		// transient quote outage. Each lane must retain an economic advantage;
		// actual capacity, full cost and admission are checked on this tick below.
		// Positive pair capacity determines the feasible size. During an entry
		// closure, assess at available debt liquidity without claiming entry is
		// possible. Idle remainder contributes zero to the whole-vault forecast.
		amount, known := m.EntryCapacity.amount(equity)
		economicAmount := math.Min(float64(equity), (m.DebtSupplyRaw-m.DebtBorrowRaw)/(singlePassLeverage-1))
		if known && amount > 0 {
			economicAmount = math.Min(economicAmount, float64(amount))
		}
		fullDebt := economicAmount * (singlePassLeverage - 1)
		fullAPR, fullErr := projectedBorrowAPR(m, fullDebt)
		if fullErr == nil {
			gross := forecastGain(economicAmount*singlePassLeverage, economicAmount*singlePassLeverage, fullDebt, m, fullAPR, years)
			if finite(gross) && gross-out.KeepGainRaw > float64(p.MinimumBenefitRaw)+float64(equity)*float64(p.UncertaintyBPS)/10_000 {
				window := AdvantageWindow{Since: in.Now, LastSample: in.Now}
				old, ok := previous.Advantages[lane]
				if ok && !old.Since.IsZero() && !old.Since.After(old.LastSample) && !old.LastSample.After(in.Now) && in.Now.Sub(old.LastSample) <= p.MaxSampleGap {
					window.Since = old.Since
				}
				out.State.Advantages[lane] = window
			}
		}
		displayAmount := amount
		if !known {
			displayAmount = equity
		}
		if displayAmount > 0 {
			apr, err := projectedBorrowAPR(m, float64(displayAmount)*(singlePassLeverage-1))
			if err == nil {
				c.BorrowAPR = apr
				c.GrossGainRaw = forecastGain(float64(displayAmount)*singlePassLeverage, float64(displayAmount)*singlePassLeverage, float64(displayAmount)*(singlePassLeverage-1), m, apr, years)
			}
		}
		if !known {
			c.BlockedReason = "pair_capacity_unknown"
			out.Candidates = append(out.Candidates, c)
			continue
		}
		if amount == 0 || m.EntryBlockedReason != "" {
			c.BlockedReason = m.EntryBlockedReason
			if c.BlockedReason == "" {
				c.BlockedReason = "entry_closed"
			}
			out.Candidates = append(out.Candidates, c)
			continue
		}
		var quote *MoveQuote
		for i := range in.Quotes {
			q := &in.Quotes[i]
			if q.SourceLane == s.RouteLane && q.DestinationLane == lane && q.EquityRaw == amount && q.ObservationID == s.ObservationID && freshAt(in.Now, q.ObservedAt, p.QuoteMaxAge) && q.CostRaw >= 0 && q.EvidenceID != "" {
				if quote != nil {
					return hold("duplicate_move_quote")
				}
				quote = q
			}
		}
		if quote == nil {
			c.BlockedReason = "bounded_move_cost_unavailable"
			out.Candidates = append(out.Candidates, c)
			continue
		}
		if quote.CostRaw >= amount {
			c.BlockedReason = "cost_exceeds_allocation"
			out.Candidates = append(out.Candidates, c)
			continue
		}
		c.CostsKnown = true
		c.InvestedRaw = amount - quote.CostRaw
		c.IdleRaw = equity - amount
		// This pilot switches to another reserve; it does not lever existing debt twice.
		debt := float64(c.InvestedRaw) * (singlePassLeverage - 1)
		apr, err := projectedBorrowAPR(m, debt)
		if err != nil {
			c.BlockedReason = err.Error()
			out.Candidates = append(out.Candidates, c)
			continue
		}
		c.BorrowAPR = apr
		c.GainRaw = forecastGain(float64(c.InvestedRaw)*singlePassLeverage, float64(c.InvestedRaw)*singlePassLeverage, debt, m, apr, years) - float64(quote.CostRaw)
		c.BenefitRaw = c.GainRaw - out.KeepGainRaw - float64(equity)*float64(p.UncertaintyBPS)/10_000
		if !finite(c.GainRaw) || !finite(c.BenefitRaw) {
			c.BlockedReason = "invalid_candidate_forecast"
		}
		out.Candidates = append(out.Candidates, c)
		if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) && (best < 0 || c.BenefitRaw > out.Candidates[best].BenefitRaw) {
			best = len(out.Candidates) - 1
		}
	}
	if base.Action == ReportNAV {
		out.Reason = "accounting_first"
		return out
	}
	if best < 0 {
		out.Reason = "no_worthwhile_executable_move"
		return out
	}
	chosen := out.Candidates[best]
	out.DestinationLane = chosen.Lane
	out.EquityRaw = chosen.InvestedRaw
	window, stable := out.State.Advantages[chosen.Lane]
	if !stable || in.Now.Sub(window.Since) < p.Persistence {
		out.Reason = "advantage_not_yet_persistent"
		return out
	}
	out.Action = "ENTER"
	out.Reason = "persistent_net_benefit"
	if exposed {
		out.Action = "SWITCH"
	}
	return out
}
