package backyard

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Capital amounts are raw USDC (six decimals). Forecasts are estimates only;
// transaction amounts, reservations and reconciliation keep integer arithmetic.
// The current executor deposits equity, borrows 50% once, then redeposits it.
const singlePassLeverage = 1 + float64(TargetLTVBPS)/10_000

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

// validate checks one scored lane's economics: an active registry lane with
// fresh, finite rates and a coherent debt market.
func (e LaneEconomics) validate(now time.Time, p SelectorPolicy) error {
	if !earnActiveLane(e.Lane) || e.EvidenceID == "" || !freshAt(now, e.ObservedAt, p.MarketMaxAge) || !freshAt(now, e.NativeObservedAt, p.NativeMaxAge) {
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
// Returned principal is not a cost.
// It is bound to actual equity, source state, destination and exact policy set.
// selectorExitBound is the source position an unwind may clear. It creates no
// spending authority.
type selectorExitBound struct {
	MaxCollateralRaw int64 `json:"maxCollateralRaw"`
	MaxDebtRaw       int64 `json:"maxDebtRaw"`
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
	// Unlevered is a B2 1x entry quoted while the destination debt reserve
	// blocks borrowing: no borrow (receive and fee are zero), scored at the
	// lane's 1x yield, and persisted in its own advantage window.
	Unlevered bool `json:"unlevered,omitempty"`
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
	// DebtRoomUSDCRaw is the destination debt reserve's borrow room valued in
	// USDC at the quote's debt price lower bound: display only, never sizing.
	DebtRoomUSDCRaw *uint64 `json:"debtRoomUsdcRaw,omitempty"`
	SourceLane      string  `json:"sourceLane"`
	DestinationLane string  `json:"destinationLane"`
	ObservationID   string  `json:"observationId"`
	EquityRaw       int64   `json:"equityRaw"`
	CostRaw         int64   `json:"costRaw"`
	// ExpectedCostRaw is the forecast economic expense at central observed
	// prices; nil on quotes predating the forecast. The selector's equity
	// bound keeps the conservative CostRaw upper exposure bound.
	ExpectedCostRaw *int64    `json:"expectedCostRaw,omitempty"`
	ObservedAt      time.Time `json:"observedAt"`
	EvidenceID      string    `json:"evidenceId"`
	// SampleSlot is captured before constructing any recipe input. Fresh fee
	// observations cannot extend older quote/reserve evidence past this window.
	SampleSlot       int64 `json:"sampleSlot"`
	ValidThroughSlot int64 `json:"validThroughSlot"`
}

// storedWindowValid checks a PERSISTED quote's own slot window against the
// fixed ceiling, not the runtime-measured observation lag: the lag is
// re-measured (48/49 slots) and a quote stored under 49 must stay a valid
// record when it later reads 48 (live 2026-09-29: the worker exited on
// invalid_selector_entry). Freshness for new spending is still
// currentAtSlot at the current slot, in applySelectorEntry and admission.
func (q MoveQuote) storedWindowValid() bool {
	return q.SampleSlot > 0 && q.ValidThroughSlot >= q.SampleSlot && q.ValidThroughSlot-q.SampleSlot <= budgetMaxObservationLagCeilingSlots
}

func (q MoveQuote) currentAtSlot(slot int64) bool {
	return q.SampleSlot > 0 && q.ValidThroughSlot >= q.SampleSlot &&
		q.ValidThroughSlot-q.SampleSlot <= observationLagSlots() &&
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
// existing route state's UnwindIntent; forecasts do not manufacture
// authorization or a second cash ledger.
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
	// Display only, from the executable quote: its leverage, the position's
	// annual net APY after the performance fee at that leverage (move costs
	// excluded), whether the debt reserve blocks borrowing, and the reserve's
	// borrow room in USDC (nil when the quote could not value it).
	Leverage        float64 `json:"leverage,omitempty"`
	NetAPY          float64 `json:"netApy,omitempty"`
	BorrowBlocked   bool    `json:"borrowBlocked,omitempty"`
	DebtRoomUSDCRaw *uint64 `json:"debtRoomUsdcRaw,omitempty"`
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
//
// B2: a funded debt-free position is complete when 1x is the chosen level
// (LeverageTargetLevel == 1) or borrowing is blocked; otherwise debt 0 still
// means the first borrow loop is pending. Idle Squads/debt/collateral cash
// always means the tranche is in progress.
func selectorTrancheInProgress(s Snapshot) bool {
	if !hasWorkingCapital(s) {
		return false
	}
	unborrowed := s.PositionDebtRaw <= 0 && !(s.HasPosition && s.PositionCollateralRaw > 0 && (s.LeverageTargetLevel == 1 || s.BorrowUtilizationBlocked || (earnActiveLane(s.RouteLane) && s.BorrowCapacityKnown && leverageBorrowReceive(s, leverageUpLevel(s)) < leverageMinimumBorrowRaw)))
	// A pending B2 up move (target above the position, borrowing open) is
	// unfinished work too, so the selector never switches in its middle.
	_, _, _, downPartial := leverageDownPartialStep(s)
	levelPending := (leverageBorrowReceive(s, leverageUpLevel(s)) >= leverageMinimumBorrowRaw && !s.BorrowUtilizationBlocked) || leverageDownPending(s) || downPartial
	return unborrowed || levelPending || s.SquadsIdleRaw > 0 || s.DebtIdleRaw > 0 ||
		(s.CollateralIdleRaw > 0 && (s.MinimumCollateralDepositRaw <= 0 || s.CollateralIdleRaw >= s.MinimumCollateralDepositRaw))
}

// sameLaneReinvestmentEligible reports when the funded current lane itself may
// compete as a SWITCH destination instead of staying a keep-only baseline.
// Later deposits otherwise strand: with the funded lane unquotable, extra idle
// cash can never buy a strictly larger position in the reviewed lane it sits
// beside. It binds to an active registry lane only, and only for a
// positively funded, settled one-pass position (collateral, debt and NAV all
// present) whose tranche loop has completed. Eligibility gates pricing only —
// capacity, the complete exit+entry quote, persistence and net-benefit
// admission below are unchanged, and a same-lane move still commits the full
// debt unwind before its fresh entry.
func sameLaneReinvestmentEligible(s Snapshot, p SelectorPolicy) bool {
	if !earnActiveLane(s.RouteLane) || s.VoltrIdleRaw <= p.IdleBufferRaw {
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
	DebtRaw, Debt, Proceeds, APR, Gain    float64
	PositiveIncome, InitialNAV, EndingNAV float64
}

// pilotQuoteEconomics projects a candidate from its bound quote. Reserve
// utilization is projected in RAW debt units, while the forecast values the
// liability on the conservative upper side of the quote's price evidence and
// the redeposited proceeds on the lower side of the same independently
// observed interval. One price never serves both directions, and raw debt
// units never pass for USDC. A non-empty second return blocks the candidate.
func pilotQuoteEconomics(quote MoveQuote, m LaneEconomics, invested, years float64) (pilotEconomics, string) {
	if quote.Unlevered {
		// B2 1x entry: all invested equity is supplied collateral, no debt.
		if !quote.validBorrow() {
			return pilotEconomics{}, "bounded_borrow_unavailable"
		}
		return pilotForecastEconomics(pilotEconomics{}, invested, m, years, quote.selectorEconomicCostRaw()), ""
	}
	raw, rawOK := quote.borrowRawWithFee()
	borrowed, borrowedOK := quote.borrowDebtUSDCRaw()
	redeployed, proceedsOK := quote.borrowProceedsUSDCRaw()
	if !rawOK || !borrowedOK || !proceedsOK {
		return pilotEconomics{}, "bounded_borrow_unavailable"
	}
	e := pilotEconomics{DebtRaw: float64(raw), Debt: float64(borrowed), Proceeds: float64(redeployed)}
	apr, err := projectedBorrowAPR(m, e.DebtRaw)
	if err != nil {
		return pilotEconomics{}, err.Error()
	}
	e.APR = apr
	return pilotForecastEconomics(e, invested, m, years, quote.selectorEconomicCostRaw()), ""
}

// selectorSampleLine is the held-sample diagnostic: every candidate with its
// executable leverage, net APY after the performance fee, borrow open or
// blocked with the debt reserve's borrow room in USDC, its benefit in USDC and
// its refusal reason ("-" when unpriced). key is the line's shape without the
// drifting numbers, for change-only printing.
func selectorSampleLine(result SelectorResult) (key, line string) {
	usd := func(raw float64) string { return fmt.Sprintf("$%.2f", raw/1e6) }
	keys := []string{result.Action, result.Reason, result.SourceLane}
	parts := make([]string, 0, len(result.Candidates))
	for _, c := range result.Candidates {
		lev, net, borrow, room, benefit, reason := "-", "-", "-", "-", "-", c.BlockedReason
		if c.CostsKnown {
			lev, net, benefit = fmt.Sprintf("%.2fx", c.Leverage), fmt.Sprintf("%.2f%%", c.NetAPY*100), usd(c.BenefitRaw)
			borrow = "open"
			if c.BorrowBlocked {
				borrow = "blocked"
			}
		}
		if c.DebtRoomUSDCRaw != nil {
			room = usd(float64(*c.DebtRoomUSDCRaw))
		}
		if reason == "" {
			reason = "-"
		}
		roomOpen := c.DebtRoomUSDCRaw != nil && *c.DebtRoomUSDCRaw > 0
		keys = append(keys, fmt.Sprintf("%s:%s:%s:%s:%t", c.Lane, reason, borrow, lev, roomOpen))
		parts = append(parts, fmt.Sprintf("%s(lev=%s net=%s borrow=%s room=%s benefit=%s reason=%s)", c.Lane, lev, net, borrow, room, benefit, reason))
	}
	return strings.Join(keys, "|"), fmt.Sprintf("backyard-rwa-worker: selector sample action=%s reason=%s source=%s candidates=%s", result.Action, result.Reason, result.SourceLane, strings.Join(parts, " "))
}

// quoteLeverageAndNetAPY is a quote's executable leverage and the position's
// annual net APY at it after the performance fee, move costs excluded:
// display only.
func quoteLeverageAndNetAPY(e pilotEconomics, invested float64, m LaneEconomics) (float64, float64) {
	equity := invested + e.Proceeds - e.Debt
	if invested <= 0 || equity <= 0 {
		return 0, 0
	}
	collateral := invested + e.Proceeds
	gross := (collateral*math.Expm1(math.Log1p(m.NativeAPY)+math.Log1p(m.SupplyAPY)) - e.Debt*math.Expm1(e.APR)) / equity
	return collateral / equity, performanceFeeForecast(gross)
}

// All candidate collateral is supplied. A nonnegative asset exponential minus
// a nonnegative borrow exponential has its minimum at an endpoint when the
// initial collateral equity is positive. Reject the other case economically.
func pilotForecastEconomics(e pilotEconomics, invested float64, m LaneEconomics, years float64, expense int64) pilotEconomics {
	collateral := invested + e.Proceeds
	assetRate := math.Log1p(m.NativeAPY) + math.Log1p(m.SupplyAPY)
	incomeAt := func(t float64) float64 {
		return collateral*math.Expm1(assetRate*t) - e.Debt*math.Expm1(e.APR*t)
	}
	// With positive initial equity, this two-exponential NAV path is
	// monotone or has one maximum. Only its positive rise can incur fees;
	// taxing gross collateral income would also tax the borrowing expense.
	peak := years
	if assetRate > 0 && e.APR > assetRate && e.Debt > 0 {
		peak = max(0, min(years, math.Log(assetRate*collateral/(e.APR*e.Debt))/(e.APR-assetRate)))
	}
	e.PositiveIncome = max(incomeAt(peak), 0)
	e.Gain = incomeAt(years) - float64(expense)
	e.InitialNAV = collateral - e.Debt - float64(expense)
	e.EndingNAV = collateral - e.Debt + e.Gain
	return e
}

// SelectOpportunity never creates a transaction. The serialized worker must
// apply fresh policy/exit admission before committing an unwind or entry.
// The source lane may be any registry lane (exit-only ones included, so a held
// position keeps its economics and can unwind); markets are the active
// registry lanes the selector scores.
func SelectOpportunity(in SelectorInput, previous SelectorState) SelectorResult {
	s, p := in.Snapshot, in.Policy
	out := SelectorResult{Action: "KEEP", Reason: "no_worthwhile_move", SourceLane: s.RouteLane}
	hold := func(reason string) SelectorResult { out.Reason = reason; out.State = SelectorState{}; return out }
	if err := p.validate(); err != nil || in.Now.IsZero() {
		return hold("invalid_selector_policy")
	}
	if s.MonitorsArmed && !selectorFeeBaselineKnown(s) {
		return hold("fee_hwm_baseline_unavailable")
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
	// A source outside the registry holds.
	if !earnHeldLane(s.RouteLane) {
		return hold("pilot_lane_unavailable")
	}
	allocation := min(equity, int64(strategyTwoBridgeLegCapRaw))
	markets := map[string]LaneEconomics{}
	for _, m := range in.Markets {
		if _, ok := markets[m.Lane]; ok {
			return hold("duplicate_market")
		}
		markets[m.Lane] = m
	}
	years := p.Horizon.Hours() / (365.25 * 24)
	exposed := s.HasPosition || s.PositionDebtRaw > 0 || s.PositionCollateralRaw > 0 || s.CollateralIdleRaw > 0
	var keepGrossGain float64
	if exposed {
		m, ok := markets[s.RouteLane]
		if !ok || m.validate(in.Now, p) != nil {
			return hold("current_lane_economics_unavailable")
		}
		keepGrossGain = forecastGain(float64(s.PositionCollateralValueRaw)+float64(s.CollateralIdleValueRaw), float64(s.PositionCollateralValueRaw), float64(s.PositionDebtValueRaw), m, math.Log1p(m.CurrentBorrowAPY), years)
		out.KeepGainRaw = selectorKeepGainUpper(s, keepGrossGain)
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
		if err := m.validate(in.Now, p); err != nil {
			c.BlockedReason = err.Error()
			carryAdvantageWindows(&out, previous, lane, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		if exposed && lane == s.RouteLane && !sameLaneReinvestmentEligible(s, p) {
			c.BlockedReason = "current_position_is_keep_baseline"
			carryAdvantageWindows(&out, previous, lane, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		// Persistence is sampled only from an executable quote's priced benefit
		// below: the quote alone sees this tick's borrow capacity, utilization
		// block and full move cost. A cost-free market-level forecast would
		// accrue persistence for a lane nobody can enter (a debt reserve above
		// its utilization block). A transient quote or evidence outage carries
		// the lane's windows unchanged (carryAdvantageWindows); MaxSampleGap
		// still bounds how long.
		//
		// The gross display forecast compares RAW debt units with a USDC
		// allocation — exact only for USDC-debt lanes, so it stays USDC-only.
		route, routeErr := runtimeRoute(lane)
		unpricedDebt := routeErr != nil || route.Kamino.DebtMint != bridgeUSDC
		amount, known := m.EntryCapacity.amount(allocation)
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
			carryAdvantageWindows(&out, previous, lane, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		if amount == 0 || m.EntryBlockedReason != "" {
			c.BlockedReason = m.EntryBlockedReason
			if c.BlockedReason == "" {
				c.BlockedReason = "entry_closed"
			}
			carryAdvantageWindows(&out, previous, lane, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		var quote *MoveQuote
		for i := range in.Quotes {
			q := &in.Quotes[i]
			if q.MinimumIdleRaw == 0 || q.MinimumIdleRaw > math.MaxInt64 {
				continue
			}
			quoteAmount := min(amount, max(int64(0), int64(q.MinimumIdleRaw)-p.IdleBufferRaw))
			if q.SourceLane == s.RouteLane && q.DestinationLane == lane && q.EquityRaw == quoteAmount && q.ObservationID == s.ObservationID && freshAt(in.Now, q.ObservedAt, p.QuoteMaxAge) && q.currentAtSlot(s.Slot) && q.CostRaw >= 0 && q.EvidenceID != "" {
				if quote != nil {
					return hold("duplicate_move_quote")
				}
				quote = q
			}
		}
		if quote == nil {
			c.BlockedReason = "bounded_move_cost_unavailable"
			carryAdvantageWindows(&out, previous, lane, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		amount = quote.EquityRaw
		if quote.CostRaw >= amount {
			c.BlockedReason = "cost_exceeds_allocation"
			carryAdvantageWindows(&out, previous, lane, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		if !quote.validBorrow() {
			c.BlockedReason = "bounded_borrow_unavailable"
			carryAdvantageWindows(&out, previous, lane, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		quotes[lane] = *quote
		c.CostsKnown = true
		// CostRaw is the worst-case execution bound (swap minima, recipe
		// limits); the capital a move actually loses is its expected expense.
		c.InvestedRaw = amount - quote.selectorEconomicCostRaw()
		c.IdleRaw = s.TotalVaultNAVRaw - amount
		// A same-lane move only pays for its full exit+entry round trip when the
		// net reinvestment is strictly larger than the funded position it unwinds.
		if lane == s.RouteLane && c.InvestedRaw <= s.StrategyNAVRaw {
			c.BlockedReason = "same_lane_reinvestment_not_larger"
			carryAdvantageWindows(&out, previous, lane, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		// Every pilot move fully unwinds the existing debt before the new entry,
		// even when reentering the same reserve; it does not lever existing debt
		// twice.
		economics, blocked := pilotQuoteEconomics(*quote, m, float64(c.InvestedRaw), years)
		if blocked != "" {
			c.BlockedReason = blocked
			carryAdvantageWindows(&out, previous, lane, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		c.BorrowAPR = economics.APR
		c.Leverage, c.NetAPY = quoteLeverageAndNetAPY(economics, float64(c.InvestedRaw), m)
		c.BorrowBlocked, c.DebtRoomUSDCRaw = quote.Unlevered, quote.DebtRoomUSDCRaw
		var feeKnown bool
		c.GainRaw, feeKnown = selectorFeeReservedGain(s, p.Horizon, economics, float64(c.IdleRaw))
		// Candidate pays the repeated-fee reserve; KEEP gets the upper return
		// from a single terminal fee. Deduct uncertainty after that money edge.
		c.BenefitRaw = c.GainRaw - out.KeepGainRaw - float64(equity)*float64(p.UncertaintyBPS)/10_000
		if !feeKnown {
			c.BlockedReason = "fee_forecast_unavailable"
			delete(out.State.Advantages, lane)
			delete(out.State.Advantages, unleveredAdvantageKey(lane))
		} else if !finite(c.GainRaw) || !finite(c.BenefitRaw) {
			c.BlockedReason = "invalid_candidate_forecast"
		}
		// Every lane's persistence sample is its executable quote's priced
		// benefit, at the same MinimumBenefit threshold; an unprofitable tick
		// drops the window. A 1x quote persists in its own window: a leveraged
		// advantage never counts toward it, nor the reverse.
		if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) {
			key := lane
			if quote.Unlevered {
				key = unleveredAdvantageKey(lane)
			}
			sampleAdvantageWindow(&out, previous, key, in.Now, p)
		}
		out.Candidates = append(out.Candidates, c)
		if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) && (best < 0 || c.BenefitRaw > out.Candidates[best].BenefitRaw) {
			best = len(out.Candidates) - 1
		}
	}
	// A lane whose economics are missing from this sample (feed gap) keeps
	// its windows unchanged too; MaxSampleGap bounds how long.
	for key, window := range previous.Advantages {
		lane := strings.TrimSuffix(key, "|1x")
		if _, present := markets[lane]; !present {
			if _, sampled := out.State.Advantages[key]; !sampled {
				out.State.Advantages[key] = window
			}
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
	windowKey := chosen.Lane
	if quotes[chosen.Lane].Unlevered {
		windowKey = unleveredAdvantageKey(chosen.Lane)
	}
	window, stable := out.State.Advantages[windowKey]
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

// selectorAvailabilityReasons are candidate refusals that say nothing about
// the lane's economics: the quote, move cost or evidence was not available
// this sample (RPC fee read, timeout, feed gap). Such a sample carries the
// lane's persistence windows forward unchanged; MaxSampleGap still resets a
// window whose last real sample is too old. A priced lane that is not
// profitable still drops its window.
var selectorAvailabilityReasons = map[string]bool{
	"economic_evidence_unavailable":      true,
	"complete_entry_quote_unavailable":   true,
	"complete_move_quote_unavailable":    true,
	"bounded_move_cost_unavailable":      true,
	"pair_capacity_unknown":              true,
	"selector_source_quote_unavailable":  true,
	"selector_source_unavailable":        true,
	"selector_live_snapshot_unavailable": true,
}

// carryAdvantageWindows keeps the lane's leveraged and 1x windows exactly
// as they were (same Since and LastSample) when this sample could not price
// the lane for an availability reason.
func carryAdvantageWindows(out *SelectorResult, previous SelectorState, lane, reason string) {
	if !selectorAvailabilityReasons[reason] {
		return
	}
	for _, key := range []string{lane, unleveredAdvantageKey(lane)} {
		if _, sampled := out.State.Advantages[key]; sampled {
			continue
		}
		if window, ok := previous.Advantages[key]; ok {
			out.State.Advantages[key] = window
		}
	}
}

// unleveredAdvantageKey is the persistence window of a lane's 1x entry.
func unleveredAdvantageKey(lane string) string { return lane + "|1x" }

// sampleAdvantageWindow records one persistence sample for lane with the
// installed hysteresis: a fresh window starts at now, and a prior window keeps
// its Since while the last sample stayed inside MaxSampleGap.
func sampleAdvantageWindow(out *SelectorResult, previous SelectorState, lane string, now time.Time, p SelectorPolicy) {
	window := AdvantageWindow{Since: now, LastSample: now}
	old, ok := previous.Advantages[lane]
	if ok && !old.Since.IsZero() && !old.Since.After(old.LastSample) && !old.LastSample.After(now) && now.Sub(old.LastSample) <= p.MaxSampleGap {
		window.Since = old.Since
	}
	out.State.Advantages[lane] = window
}

func (q MoveQuote) validBorrow() bool {
	if q.Unlevered {
		return q.EquityRaw > 0 && q.BorrowReceiveRaw == 0 && q.BorrowFeeRaw == 0 && earnActiveLane(q.DestinationLane)
	}
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
