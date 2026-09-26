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

// selectorEntryLane is the reviewed new-entry scope for this rollout: Maple
// (syrupUSDC/USDC) only. It is deliberately narrower than selectorLane, which
// keeps deferred lanes (Prime/PRIME/USDC, OnRe/ONyc/USDC) fully observable,
// validatable, and exitable. Only fresh entry or rotation authority is
// withheld; widening it back to a selector lane is a reviewed change.
func selectorEntryLane(lane string) bool {
	return lane == SelectedRouteID
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
	return e.validateWithLane(now, p, selectorLane)
}

// validateWithLane is the identical economics validation with the lane
// authority parameterized, so the reviewed manifest's funded-selection path
// admits its candidate lane's evidence through the same validated autoPolicy
// binding that prices and admits it. Every rate, freshness and debt check is
// shared verbatim.
func (e LaneEconomics) validateWithLane(now time.Time, p SelectorPolicy, laneAllowed func(string) bool) error {
	if !laneAllowed(e.Lane) || e.EvidenceID == "" || !freshAt(now, e.ObservedAt, p.MarketMaxAge) || !freshAt(now, e.NativeObservedAt, p.NativeMaxAge) {
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
	// 30-day horizon (Vlad OK 09-26): over 7 days a move had to repay its
	// cost plus the margin within one week (~5%/yr APY edge), so even a
	// clearly better lane (AUTO 9.2% vs Maple 3.7%) never passed.
	// MaxSampleGap 10 min (Vlad OK 09-26): samples skip while a NAV report or
	// a failed quote runs, so live gaps reached ~6 min and a 2-min gap reset
	// the 30-min window every few minutes; every sample taken must still lead.
	return SelectorPolicy{Horizon: 30 * 24 * time.Hour, Persistence: 30 * time.Minute, MaxSampleGap: 10 * time.Minute, MarketMaxAge: 90 * time.Second, NativeMaxAge: 2 * time.Hour, QuoteMaxAge: 30 * time.Second, MinimumBenefitRaw: 250_000, UncertaintyBPS: 10, IdleBufferRaw: 0}
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
// selectorExitBound refers to gross debits in the existing exit reservation.
// It is separate from movement expense and creates no new spending authority.
type selectorExitBound struct {
	MaxCollateralRaw int64 `json:"maxCollateralRaw"`
	MaxDebtRaw       int64 `json:"maxDebtRaw"`
	GrossMicros      int64 `json:"grossMicros"`
}

type MoveQuote struct {
	SourceExit *selectorExitBound `json:"sourceExit,omitempty"`
	// Whole-vault USDC available after the source exit at enforced swap minima.
	MinimumIdleRaw uint64 `json:"minimumIdleRaw"`
	// Exact one-pass borrow sized from conservative initial collateral. Execution
	// may receive more collateral, but may not silently increase this borrow.
	// Quantities stay in raw debt-mint units; only DebtPrice gives them a USDC
	// meaning, recomputed from the bound evidence at every use.
	BorrowReceiveRaw uint64 `json:"borrowReceiveRaw"`
	BorrowFeeRaw     uint64 `json:"borrowFeeRaw"`
	// DebtPrice is nil for USDC-debt lanes (raw==USDC parity, old JSON decodes
	// unchanged). A non-USDC debt quote without this evidence is invalid:
	// missing price evidence is rejected, never waivered. The observation is
	// copied immutably at compose time and its window bounds the quote's own.
	// It prices the LIABILITY side only.
	DebtPrice *BudgetPrice `json:"debtPrice,omitempty"`
	// CollateralAssetUSDCRaw is the doc-12 asset-side correction for non-USDC
	// debt lanes: the two bounded collateral purchase outputs (entry and
	// leverage swap minima) valued at the observed collateral price LOWER
	// bound. The debt price never lifts the asset side, and nil stays the
	// USDC-parity meaning of every raw USDC field.
	CollateralAssetUSDCRaw *uint64 `json:"collateralAssetUsdcRaw,omitempty"`
	// CollateralAssetPrice is the retained observed collateral price evidence
	// behind CollateralAssetUSDCRaw; RedepositCollateralRaw is the bounded
	// leverage swap minimum it revalues for production economics. Its window
	// intersects the quote's exactly like the debt price's.
	CollateralAssetPrice   *BudgetPrice `json:"collateralAssetPrice,omitempty"`
	RedepositCollateralRaw uint64       `json:"redepositCollateralRaw,omitempty"`
	SourceLane             string       `json:"sourceLane"`
	DestinationLane        string       `json:"destinationLane"`
	ObservationID          string       `json:"observationId"`
	EquityRaw              int64        `json:"equityRaw"`
	CostRaw                int64        `json:"costRaw"`
	// ExpectedCostRaw is the forecast economic expense at central observed
	// prices; nil on quotes predating the forecast. Every admission gate,
	// reservation and spending bound keeps the conservative CostRaw upper
	// exposure bound.
	ExpectedCostRaw *int64    `json:"expectedCostRaw,omitempty"`
	ObservedAt      time.Time `json:"observedAt"`
	EvidenceID      string    `json:"evidenceId"`
	// SampleSlot is captured before constructing any recipe input. Fresh fee
	// observations cannot extend older quote/reserve evidence past this window.
	SampleSlot       int64 `json:"sampleSlot"`
	ValidThroughSlot int64 `json:"validThroughSlot"`
}

func (q MoveQuote) currentAtSlot(slot int64) bool {
	return q.SampleSlot > 0 && q.ValidThroughSlot >= q.SampleSlot &&
		q.ValidThroughSlot-q.SampleSlot <= budgetMaxObservationLagSlots &&
		slot >= q.SampleSlot && slot <= q.ValidThroughSlot
}

// selectorEconomicCostRaw is the expected economic expense when a forecast is
// present; old or opaque quotes fall back conservatively to the bounded
// CostRaw. Only the net-yield comparison consumes this — admission, spending
// bounds and history stay on CostRaw.
func (q MoveQuote) selectorEconomicCostRaw() int64 {
	if q.ExpectedCostRaw != nil && *q.ExpectedCostRaw >= 0 && *q.ExpectedCostRaw <= q.CostRaw {
		return *q.ExpectedCostRaw
	}
	return q.CostRaw
}

type SelectorInput struct {
	canaryRequest *pilotCanaryEntryRequest
	// canaryPriorEntry is the persisted selector entry, read under the route lock.
	canaryPriorEntry *SelectorEntry
	Now              time.Time
	Snapshot         Snapshot
	Markets          []LaneEconomics
	Quotes           []MoveQuote
	Policy           SelectorPolicy
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
	// The exact quote selected for ENTER/SWITCH, copied from the validated
	// candidate. Net equity alone cannot identify its gross input or costs.
	SelectedQuote *MoveQuote `json:"selectedQuote,omitempty"`
}

// A funded entry must finish before its temporary holdings become an economic
// KEEP baseline. Safety and withdrawal handling remain separate priorities.
func selectorTrancheInProgress(s Snapshot) bool {
	if !s.PilotActive || !hasWorkingCapital(s) {
		return false
	}
	return s.PositionDebtRaw <= 0 || s.SquadsIdleRaw > 0 || s.DebtIdleRaw > 0 ||
		(s.CollateralIdleRaw > 0 && (s.MinimumCollateralDepositRaw <= 0 || s.CollateralIdleRaw >= s.MinimumCollateralDepositRaw))
}

// sameLaneReinvestmentEligible reports when the funded current lane itself may
// compete as a SWITCH destination instead of staying a keep-only baseline.
// Later deposits otherwise strand: with the funded lane unquotable, extra idle
// cash can never buy a strictly larger position in the reviewed lane it sits
// beside. It binds to the reviewed Maple entry lane only, and only for a
// positively funded, settled one-pass position (collateral, debt and NAV all
// present) whose tranche loop has completed. Eligibility gates pricing only —
// capacity, the complete exit+entry quote, persistence and net-benefit
// admission below are unchanged, and a same-lane move still commits the full
// debt unwind before its fresh entry.
func sameLaneReinvestmentEligible(s Snapshot, p SelectorPolicy) bool {
	return sameLaneReinvestmentEligibleWithLane(s, p, selectorEntryLane)
}

// sameLaneReinvestmentEligibleWithLane is the identical eligibility check with
// the current-lane entry authority parameterized: installed selector lanes
// under the public wrapper, and on the manifest path also the candidate lane
// as the funded production source, so an existing AUTO allocation can reenter
// or grow exactly like an installed one. Eligibility gates pricing only —
// capacity, the complete exit+entry quote, persistence and net-benefit
// admission below are unchanged, and a same-lane move still commits the full
// debt unwind before its fresh entry.
func sameLaneReinvestmentEligibleWithLane(s Snapshot, p SelectorPolicy, laneAllowed func(string) bool) bool {
	if !s.PilotActive || !laneAllowed(s.RouteLane) || s.VoltrIdleRaw <= p.IdleBufferRaw {
		return false
	}
	if !s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionDebtRaw <= 0 || s.StrategyNAVRaw <= 0 {
		return false
	}
	return !selectorTrancheInProgress(s)
}

func forecastGain(collateral, supplied, debt float64, e LaneEconomics, borrowAPR, years float64) float64 {
	// Native yield on all owned collateral; lending yield only on supplied units.
	return (collateral-supplied)*math.Expm1(math.Log1p(e.NativeAPY)*years) + supplied*math.Expm1((math.Log1p(e.NativeAPY)+math.Log1p(e.SupplyAPY))*years) - debt*math.Expm1(borrowAPR*years)
}

// pilotEconomics is one bounded move's projected economics. DebtRaw stays in
// raw debt-mint units; Debt and Proceeds are USDC valuations of the same
// borrowing on opposite sides of the bound price interval.
type pilotEconomics struct {
	DebtRaw, Debt, Proceeds, APR, Gain float64
}

// pilotQuoteEconomics projects a candidate from its bound quote. Reserve
// utilization is projected in RAW debt units, while the forecast values the
// liability on the conservative upper side of the quote's price evidence and
// the redeposited proceeds on the lower side of the same independently
// observed interval. One price never serves both directions, and raw debt
// units never pass for USDC. A non-empty second return blocks the candidate.
func pilotQuoteEconomics(pilot bool, quote MoveQuote, m LaneEconomics, invested, years float64) (pilotEconomics, string) {
	e := pilotEconomics{DebtRaw: invested * (singlePassLeverage - 1)}
	e.Debt, e.Proceeds = e.DebtRaw, e.DebtRaw
	if pilot {
		raw, rawOK := quote.borrowRawWithFee()
		borrowed, borrowedOK := quote.borrowDebtUSDCRaw()
		redeployed, proceedsOK := quote.borrowProceedsUSDCRaw()
		if !rawOK || !borrowedOK || !proceedsOK {
			return pilotEconomics{}, "bounded_borrow_unavailable"
		}
		e.DebtRaw = float64(raw)
		e.Debt = float64(borrowed)
		e.Proceeds = float64(redeployed)
	}
	apr, err := projectedBorrowAPR(m, e.DebtRaw)
	if err != nil {
		return pilotEconomics{}, err.Error()
	}
	e.APR = apr
	collateral := invested + e.Proceeds
	e.Gain = forecastGain(collateral, collateral, e.Debt, m, apr, years) - float64(quote.selectorEconomicCostRaw())
	return e, ""
}

// SelectOpportunity never creates a transaction. The serialized worker must
// apply fresh policy/exit admission before committing an unwind or entry.
func SelectOpportunity(in SelectorInput, previous SelectorState) SelectorResult {
	return selectOpportunityWithLanes(in, previous, selectorLane, selectorEntryLane)
}

// selectOpportunityWithLanes is the identical pure selection with two lane
// authorities parameterized. laneAllowed is the source-observation and market
// evidence authority: installed selector lanes under the public wrapper, and
// on the manifest path the candidate lane is also admitted as the funded
// production source, so an existing AUTO allocation keeps its current
// economics and can unwind safely. fundingAllowed is the new-funding
// authority for same-lane reinvestment: the installed selectorEntryLane set
// under the public wrapper, plus only the reviewed candidate lane on the
// manifest path — deferred lanes stay keep-only baselines. Every persistence,
// capacity, quote and benefit rule is shared verbatim.
func selectOpportunityWithLanes(in SelectorInput, previous SelectorState, laneAllowed func(string) bool, fundingAllowed func(string) bool) SelectorResult {
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
	// Pilot execution deploys one bounded tranche. Forecast the same amount;
	// idle vault principal must not earn the destination's modeled yield.
	allocation := equity
	if s.PilotActive {
		// The source route lane is entry authority too: installed selector
		// lanes under the public wrapper, and on the manifest path also the
		// candidate lane as the funded production source. An unauthorized
		// source keeps the installed hold.
		if !laneAllowed(s.RouteLane) {
			return hold("pilot_lane_unavailable")
		}
		allocation = min(allocation, workingTrancheCap(s))
	}
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
		if !ok || m.validateWithLane(in.Now, p, laneAllowed) != nil {
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
	quotes := make(map[string]MoveQuote)
	for _, lane := range lanes {
		m := markets[lane]
		c := CandidateForecast{Lane: lane}
		if err := m.validateWithLane(in.Now, p, laneAllowed); err != nil {
			c.BlockedReason = err.Error()
			out.Candidates = append(out.Candidates, c)
			continue
		}
		if exposed && lane == s.RouteLane && !sameLaneReinvestmentEligibleWithLane(s, p, fundingAllowed) {
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
		//
		// The feed's debt room and the gross display forecast compare RAW debt
		// units with a USDC allocation — exact only for USDC-debt lanes, so the
		// feed-level window sample and gross display stay USDC-debt-only. A
		// non-USDC debt lane carries no feed-level price; its persistence is
		// sampled from the priced per-quote evidence below — the bound debt
		// price entry admission itself requires — at the same MinimumBenefit
		// threshold and the identical window hysteresis. No feed price oracle
		// is added and no raw-unit parity is assumed.
		route, routeErr := runtimeRoute(lane)
		unpricedDebt := routeErr != nil || route.Kamino.DebtMint != bridgeUSDC
		amount, known := m.EntryCapacity.amount(allocation)
		economicAmount := math.Min(float64(allocation), (m.DebtSupplyRaw-m.DebtBorrowRaw)/(singlePassLeverage-1))
		if known && amount > 0 {
			economicAmount = math.Min(economicAmount, float64(amount))
		}
		fullDebt := economicAmount * (singlePassLeverage - 1)
		fullAPR, fullErr := projectedBorrowAPR(m, fullDebt)
		if fullErr == nil && !unpricedDebt {
			gross := forecastGain(economicAmount*singlePassLeverage, economicAmount*singlePassLeverage, fullDebt, m, fullAPR, years)
			if finite(gross) && gross-out.KeepGainRaw > float64(p.MinimumBenefitRaw)+float64(equity)*float64(p.UncertaintyBPS)/10_000 {
				sampleAdvantageWindow(&out, previous, lane, in.Now, p)
			}
		}
		displayAmount := amount
		if !known {
			displayAmount = allocation
		}
		if displayAmount > 0 && !unpricedDebt {
			apr, err := projectedBorrowAPR(m, float64(displayAmount)*(singlePassLeverage-1))
			if err == nil {
				c.BorrowAPR = apr
				c.GrossGainRaw = forecastGain(float64(displayAmount)*singlePassLeverage, float64(displayAmount)*singlePassLeverage, float64(displayAmount)*(singlePassLeverage-1), m, apr, years)
			}
		}
		if !known {
			// An already-labeled market keeps its own refusal code on the
			// candidate — a whole-collection outage must not lose its sanitized
			// reason to the generic unknown-capacity label. The label changes no
			// admission: unknown capacity is never enterable either way.
			c.BlockedReason = "pair_capacity_unknown"
			if m.EntryBlockedReason != "" {
				c.BlockedReason = m.EntryBlockedReason
			}
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
			quoteAmount := amount
			if s.PilotActive {
				if q.MinimumIdleRaw == 0 || q.MinimumIdleRaw > math.MaxInt64 {
					continue
				}
				quoteAmount = min(amount, max(int64(0), int64(q.MinimumIdleRaw)-p.IdleBufferRaw))
			}
			if q.SourceLane == s.RouteLane && q.DestinationLane == lane && q.EquityRaw == quoteAmount && q.ObservationID == s.ObservationID && freshAt(in.Now, q.ObservedAt, p.QuoteMaxAge) && q.currentAtSlot(s.Slot) && q.CostRaw >= 0 && q.EvidenceID != "" {
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
		amount = quote.EquityRaw
		if quote.CostRaw >= amount {
			c.BlockedReason = "cost_exceeds_allocation"
			out.Candidates = append(out.Candidates, c)
			continue
		}
		if s.PilotActive && !quote.validBorrow() {
			c.BlockedReason = "bounded_borrow_unavailable"
			out.Candidates = append(out.Candidates, c)
			continue
		}
		quotes[lane] = *quote
		c.CostsKnown = true
		c.InvestedRaw = amount - quote.CostRaw
		c.IdleRaw = s.TotalVaultNAVRaw - amount
		// A same-lane move only pays for its full exit+entry round trip when the
		// net reinvestment is strictly larger than the funded position it unwinds.
		if lane == s.RouteLane && c.InvestedRaw <= s.StrategyNAVRaw {
			c.BlockedReason = "same_lane_reinvestment_not_larger"
			out.Candidates = append(out.Candidates, c)
			continue
		}
		// Every pilot move fully unwinds the existing debt before the new entry,
		// even when reentering the same reserve; it does not lever existing debt
		// twice.
		economics, blocked := pilotQuoteEconomics(s.PilotActive, *quote, m, float64(c.InvestedRaw), years)
		if blocked != "" {
			c.BlockedReason = blocked
			out.Candidates = append(out.Candidates, c)
			continue
		}
		c.BorrowAPR = economics.APR
		c.GainRaw = economics.Gain
		c.BenefitRaw = c.GainRaw - out.KeepGainRaw - float64(equity)*float64(p.UncertaintyBPS)/10_000
		if !finite(c.GainRaw) || !finite(c.BenefitRaw) {
			c.BlockedReason = "invalid_candidate_forecast"
		}
		// A non-USDC debt lane's persistence sample comes from the priced quote
		// economics above — its only USDC-denominated signal, the same bound
		// debt and collateral prices entry admission requires. A full or
		// unavailable market never reaches a priced quote here and so never
		// accumulates persistence; an unprofitable tick drops the window
		// exactly as the feed-level path does.
		if unpricedDebt && s.PilotActive && c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) {
			sampleAdvantageWindow(&out, previous, lane, in.Now, p)
		}
		out.Candidates = append(out.Candidates, c)
		if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) && (best < 0 || c.BenefitRaw > out.Candidates[best].BenefitRaw) {
			best = len(out.Candidates) - 1
		}
	}
	if selectorTrancheInProgress(s) {
		out.Reason = "complete_current_tranche_first"
		return out
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
	quote := quotes[chosen.Lane]
	out.SelectedQuote = &quote
	out.Action = "ENTER"
	out.Reason = "persistent_net_benefit"
	if exposed {
		out.Action = "SWITCH"
	}
	return out
}

// sampleAdvantageWindow records one persistence sample for lane with the
// installed hysteresis: a fresh window starts at now, and a prior window keeps
// its Since while the last sample stayed inside MaxSampleGap. The USDC-debt
// feed-level forecast and the non-USDC priced per-quote forecast share it, so
// both persistence signals carry identical window semantics.
func sampleAdvantageWindow(out *SelectorResult, previous SelectorState, lane string, now time.Time, p SelectorPolicy) {
	window := AdvantageWindow{Since: now, LastSample: now}
	old, ok := previous.Advantages[lane]
	if ok && !old.Since.IsZero() && !old.Since.After(old.LastSample) && !old.LastSample.After(now) && now.Sub(old.LastSample) <= p.MaxSampleGap {
		window.Since = old.Since
	}
	out.State.Advantages[lane] = window
}

func (q MoveQuote) validBorrow() bool {
	if q.EquityRaw <= 0 || q.BorrowReceiveRaw <= 0 {
		return false
	}
	route, err := runtimeRoute(q.DestinationLane)
	if err != nil {
		return false
	}
	if route.Kamino.DebtMint == bridgeUSDC {
		// USDC debt: raw units are USDC by identity, exactly as every persisted
		// pre-revision quote assumed. A carried price never relaxes this.
		return q.BorrowReceiveRaw <= uint64(q.EquityRaw) && q.BorrowFeeRaw <= uint64(q.EquityRaw)-q.BorrowReceiveRaw
	}
	// Non-USDC debt: raw units have no USDC meaning. The bound price evidence
	// is required, must cover the quote's whole window, and must value
	// receive+fee inside the same equity.
	debt, ok := q.borrowDebtUSDCRaw()
	return ok && debt <= uint64(q.EquityRaw)
}

// borrowDebtUSDCRaw values BorrowReceiveRaw+BorrowFeeRaw in USDC. USDC-debt
// quotes keep the exact raw identity every existing consumer relied on;
// otherwise the bound BudgetPrice is revalidated against the route identity
// and the conversion recomputed at its conservative upper bound.
func (q MoveQuote) borrowDebtUSDCRaw() (uint64, bool) {
	receive, ok := q.borrowRawWithFee()
	if !ok {
		return 0, false
	}
	route, err := runtimeRoute(q.DestinationLane)
	if err != nil {
		return 0, false
	}
	if route.Kamino.DebtMint == bridgeUSDC {
		return receive, true
	}
	if q.DebtPrice == nil || q.DebtPrice.ObservedSlot < q.SampleSlot {
		return 0, false
	}
	value, err := q.DebtPrice.valueUpper(receive, route.Kamino.DebtMint, route.DebtTokenProgram, q.ValidThroughSlot)
	if err != nil || value <= 0 {
		return 0, false
	}
	return uint64(value), true
}

// borrowProceedsUSDCRaw is the borrowed principal's USDC-value floor. Only
// the principal is redeposited (the origination fee is consumed), and the
// actual redeposit stays bound to the leverage swap's enforced output
// minimum; this floor feeds the economics forecast only. On a non-USDC debt
// lane the redeposit is COLLATERAL raw, so it is valued on the lower side of
// the retained collateral asset price over that actual bounded purchase
// output — the debt price never lifts the asset side (doc 12).
func (q MoveQuote) borrowProceedsUSDCRaw() (uint64, bool) {
	route, err := runtimeRoute(q.DestinationLane)
	if err != nil {
		return 0, false
	}
	if route.Kamino.DebtMint == bridgeUSDC {
		receive, ok := q.borrowRawWithFee()
		return receive, ok
	}
	if q.DebtPrice == nil || q.DebtPrice.ObservedSlot < q.SampleSlot || q.BorrowFeeRaw > q.BorrowReceiveRaw ||
		q.CollateralAssetPrice == nil || q.CollateralAssetPrice.ObservedSlot < q.SampleSlot || q.RedepositCollateralRaw == 0 {
		return 0, false
	}
	value, err := q.CollateralAssetPrice.valueLower(q.RedepositCollateralRaw, route.Kamino.CollateralMint, route.CollateralTokenProgram, q.ValidThroughSlot)
	if err != nil || value < 0 {
		return 0, false
	}
	return uint64(value), true
}

func (q MoveQuote) borrowRawWithFee() (uint64, bool) {
	if q.BorrowFeeRaw > math.MaxUint64-q.BorrowReceiveRaw {
		return 0, false
	}
	return q.BorrowReceiveRaw + q.BorrowFeeRaw, true
}
