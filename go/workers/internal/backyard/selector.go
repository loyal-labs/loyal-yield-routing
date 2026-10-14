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
// are omitted until eligibility and recurrence can be proven. NativeAPY is the
// collateral's realized market price growth over the trailing 30 days
// (parseRealizedYield); FeedNativeAPY is Kamino's yield feed for the same
// token, display and comparison only (nil when the feed did not report it),
// and no decision reads it.
type LaneEconomics struct {
	Lane             string             `json:"lane"`
	EvidenceID       string             `json:"evidenceId"`
	ObservedAt       time.Time          `json:"observedAt"`
	NativeObservedAt time.Time          `json:"nativeObservedAt"`
	NativeAPY        float64            `json:"nativeApy"`
	FeedNativeAPY    *float64           `json:"feedNativeApy,omitempty"`
	SupplyAPY        float64            `json:"supplyApy"`
	CurrentBorrowAPY float64            `json:"currentBorrowApy"`
	BorrowCurve      []BorrowCurvePoint `json:"borrowCurve"`
	HostBorrowBPS    float64            `json:"hostBorrowBps"`
	DebtSupplyRaw    float64            `json:"debtSupplyUsdcRaw"`
	DebtBorrowRaw    float64            `json:"debtBorrowUsdcRaw"`
	// DebtRawPerUSDCRaw converts a USDC value into a non-USDC debt lane's raw
	// debt units at the verified debt and USDC reserve prices of the same feed
	// read; 0 when unknown and on USDC debt, whose raw units are USDC. Forecast
	// only (projected utilization); money never moves at this rate.
	DebtRawPerUSDCRaw  float64  `json:"debtRawPerUsdcRaw,omitempty"`
	EntryCapacity      Capacity `json:"entryCapacity"`
	EntryBlockedReason string   `json:"entryBlockedReason,omitempty"`
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
	// Borrowed may exceed total supply: KLend's total supply nets accumulated
	// protocol and referrer fees out of available liquidity, so a fully
	// borrowed reserve whose fees exceed its available cash reports
	// utilization above 100% (Prime USDS on 2026-10-10).
	if e.SupplyAPY < 0 || e.CurrentBorrowAPY < 0 || !finite(e.HostBorrowBPS) || e.HostBorrowBPS < 0 || e.HostBorrowBPS > 100_000 || !finite(e.DebtSupplyRaw) || !finite(e.DebtBorrowRaw) || e.DebtSupplyRaw <= 0 || e.DebtBorrowRaw < 0 ||
		!finite(e.DebtRawPerUSDCRaw) || e.DebtRawPerUSDCRaw < 0 {
		return fmt.Errorf("invalid_debt_market")
	}
	_, err := projectedBorrowAPR(e, 0)
	return err
}

// projectedBorrowAPR is the KLend borrow curve plus host rate at the reserve's
// utilization after additionalDebt raw debt units. Like KLend's
// get_borrow_rate, utilization above 100% prices at the terminal point; this
// prices the rate only, never whether the borrow can be funded.
func projectedBorrowAPR(e LaneEconomics, additionalDebt float64) (float64, error) {
	if !finite(additionalDebt) || additionalDebt < 0 || e.DebtSupplyRaw <= 0 {
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
	u := min((e.DebtBorrowRaw+additionalDebt)/e.DebtSupplyRaw*10_000, 10_000)
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

// MoveQuote is a flat vault's entry into one lane, priced by priceEntry from
// the transactions the executor would send: what it enters, the up step it
// levers with (none at 1x) and what that costs the vault's capital. It
// creates no spending authority: target() reads only its lane, equity and
// window, and every leg is built and simulated again when it executes.
type MoveQuote struct {
	// MinimumIdleRaw is the flat cash the entry draws on: Voltr idle plus the
	// Squads cash an entry already allocated.
	MinimumIdleRaw uint64 `json:"minimumIdleRaw"`
	// BorrowReceiveRaw and BorrowFeeRaw are the up step the entry levers with,
	// in raw debt units: the capacity-sized borrow of the funded position.
	BorrowReceiveRaw uint64 `json:"borrowReceiveRaw"`
	BorrowFeeRaw     uint64 `json:"borrowFeeRaw"`
	// Unlevered is a 1x entry: no up step (receive and fee are zero), scored
	// at the lane's 1x yield and persisted in its own advantage window. It is
	// quoted while the debt reserve has no room for the minimum borrow, or
	// (SpreadUnlevered) while the lane's spread at that borrow is below the
	// live leverage rule's 1.5x minimum.
	Unlevered       bool `json:"unlevered,omitempty"`
	SpreadUnlevered bool `json:"spreadUnlevered,omitempty"`
	// DebtRoomUSDCRaw is the debt reserve's borrow room in USDC: display only.
	DebtRoomUSDCRaw *uint64 `json:"debtRoomUsdcRaw,omitempty"`
	// BorrowValueRaw and ProceedsValueRaw are the up step valued at the lane
	// batch's prices: the liability (receive plus fee) and the principal the
	// step converts into collateral.
	BorrowValueRaw   uint64 `json:"borrowValueRaw,omitempty"`
	ProceedsValueRaw uint64 `json:"proceedsValueRaw,omitempty"`
	SourceLane       string `json:"sourceLane"`
	// RotateFrom is a rotation's source: the held lane whose release funds
	// the entry, in place of cash. Empty on a cash entry.
	RotateFrom      string `json:"rotateFrom,omitempty"`
	DestinationLane string `json:"destinationLane"`
	ObservationID   string `json:"observationId"`
	EquityRaw       int64  `json:"equityRaw"`
	// CostRaw is Cost's total: what the entry and its unwind are expected to
	// cost the vault's capital.
	CostRaw    int64     `json:"costRaw"`
	Cost       entryCost `json:"cost"`
	ObservedAt time.Time `json:"observedAt"`
	EvidenceID string    `json:"evidenceId"`
	// SampleSlot is the sample's batch slot; the quote is current through
	// ValidThroughSlot, one observation window later.
	SampleSlot       int64 `json:"sampleSlot"`
	ValidThroughSlot int64 `json:"validThroughSlot"`
}

// storedWindowValid checks a PERSISTED quote's own slot window against the
// fixed ceiling, not the runtime-measured observation lag: the lag is
// re-measured (48/49 slots) and a quote stored under 49 must stay a valid
// record when it later reads 48 (live 2026-09-29: the worker exited on
// invalid_selector_entry). Freshness for new spending is still
// currentAtSlot at the current slot, in target() and the canary.
func (q MoveQuote) storedWindowValid() bool {
	return q.SampleSlot > 0 && q.ValidThroughSlot >= q.SampleSlot && q.ValidThroughSlot-q.SampleSlot <= budgetMaxObservationLagCeilingSlots
}

func (q MoveQuote) currentAtSlot(slot int64) bool {
	return q.SampleSlot > 0 && q.ValidThroughSlot >= q.SampleSlot &&
		q.ValidThroughSlot-q.SampleSlot <= observationLagSlots() &&
		slot >= q.SampleSlot && slot <= q.ValidThroughSlot
}

type SelectorInput struct {
	canaryRequest *pilotCanaryEntryRequest
	Now           time.Time
	Snapshot      Snapshot
	// Lanes is every lane's view of the same batch (Observation.lanes): the
	// held lanes a rotation releases from.
	Lanes   []Snapshot
	Markets []LaneEconomics
	Quotes  []MoveQuote
	Policy  SelectorPolicy
}

// Only economic persistence lives here; forecasts do not manufacture
// authorization or a second cash ledger.
type AdvantageWindow struct {
	Since      time.Time `json:"since"`
	LastSample time.Time `json:"lastSample"`
}

// Advantages are keyed by the move: a cash entry by its lane, a rotation by
// source->destination (rotationAdvantageKey), a 1x quote with "|1x".
type SelectorState struct {
	Advantages map[string]AdvantageWindow `json:"advantages,omitempty"`
}

type CandidateForecast struct {
	// SourceLane is a rotation's source; empty for cash entering Lane.
	SourceLane  string `json:"sourceLane,omitempty"`
	Lane        string `json:"lane"`
	InvestedRaw int64  `json:"investedRaw"`
	// CostRaw is the priced move's whole cost (its quote's), and KeepGainRaw
	// what a rotation's equity earns where it is over the horizon.
	CostRaw       int64   `json:"costRaw,omitempty"`
	KeepGainRaw   float64 `json:"keepGainRaw,omitempty"`
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
	// Display only: the lane's realized native APY the forecast used and
	// the yield feed's figure for the same token.
	NativeAPY     float64  `json:"nativeApy,omitempty"`
	FeedNativeAPY *float64 `json:"feedNativeApy,omitempty"`
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
	// The exact quote selected for ENTER or ROTATE, copied from the
	// validated candidate. Net equity alone cannot identify its gross input
	// or costs.
	SelectedQuote *MoveQuote `json:"selectedQuote,omitempty"`
	// SampledAt is when the selector decided this result: a rotation
	// releases only while it is within the policy's MaxSampleGap.
	SampledAt time.Time `json:"sampledAt,omitempty"`
	// SourceCollateralRaw is the rotation source's deposited collateral the
	// release is sized from. A release that lands changes it, so one result
	// releases at most once; the next sample decides the next tranche.
	SourceCollateralRaw int64 `json:"sourceCollateralRaw,omitempty"`
}

func forecastGain(collateral, supplied, debt float64, e LaneEconomics, borrowAPR, years float64) float64 {
	// Native yield on all owned collateral; lending yield only on supplied units.
	return (collateral-supplied)*math.Expm1(math.Log1p(e.NativeAPY)*years) + supplied*math.Expm1((math.Log1p(e.NativeAPY)+math.Log1p(e.SupplyAPY))*years) - debt*math.Expm1(borrowAPR*years)
}

// pilotEconomics is one entry's projected economics. DebtRaw stays in raw
// debt-mint units (the reserve's projected utilization); Debt and Proceeds
// are the same borrow valued in USDC at the lane batch's prices.
type pilotEconomics struct {
	DebtRaw, Debt, Proceeds, APR, Gain    float64
	PositiveIncome, InitialNAV, EndingNAV float64
}

// pilotQuoteEconomics projects a candidate from its quote: the liability the
// up step owes (receive plus fee) and the principal it converts into
// collateral, at the quote's central values. Its cost is charged once, in
// Gain. A non-empty second return blocks the candidate.
func pilotQuoteEconomics(quote MoveQuote, m LaneEconomics, invested, years float64) (pilotEconomics, string) {
	if !quote.validBorrow() {
		return pilotEconomics{}, "bounded_borrow_unavailable"
	}
	if quote.Unlevered {
		// 1x entry: all invested equity is supplied collateral, no debt.
		return pilotForecastEconomics(pilotEconomics{}, invested, m, years, quote.CostRaw), ""
	}
	e := pilotEconomics{DebtRaw: float64(quote.BorrowReceiveRaw) + float64(quote.BorrowFeeRaw), Debt: float64(quote.BorrowValueRaw), Proceeds: float64(quote.ProceedsValueRaw)}
	apr, err := projectedBorrowAPR(m, e.DebtRaw)
	if err != nil {
		return pilotEconomics{}, err.Error()
	}
	e.APR = apr
	return pilotForecastEconomics(e, invested, m, years, quote.CostRaw), ""
}

// selectorSampleLine is the held-sample diagnostic: every candidate with its
// executable leverage, net APY after the performance fee, realized native APY
// beside the yield feed's and their gap, borrow open or
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
			lev, net = fmt.Sprintf("%.2fx", c.Leverage), fmt.Sprintf("%.2f%%", c.NetAPY*100)
			benefit = fmt.Sprintf("%s size=%s cost=%s gain=%s keep=%s", usd(c.BenefitRaw), usd(float64(c.InvestedRaw+c.CostRaw)), usd(float64(c.CostRaw)), usd(c.GainRaw), usd(c.KeepGainRaw))
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
		name := c.Lane
		if c.SourceLane != "" {
			name = rotationAdvantageKey(c.SourceLane, c.Lane)
		}
		keys = append(keys, fmt.Sprintf("%s:%s:%s:%s:%t", name, reason, borrow, lev, roomOpen))
		parts = append(parts, fmt.Sprintf("%s(lev=%s net=%s %s borrow=%s room=%s benefit=%s reason=%s)", name, lev, net, nativeFeedText(c.NativeAPY, c.FeedNativeAPY), borrow, room, benefit, reason))
	}
	return strings.Join(keys, "|"), fmt.Sprintf("backyard-rwa-worker: selector sample action=%s reason=%s source=%s candidates=%s", result.Action, result.Reason, result.SourceLane, strings.Join(parts, " "))
}

// nativeFeedText prints a lane's realized native APY beside the yield feed's
// and the gap (realized minus feed), in percent points.
func nativeFeedText(native float64, feed *float64) string {
	if feed == nil {
		return fmt.Sprintf("native=%.2f%% feed=unavailable", native*100)
	}
	return fmt.Sprintf("native=%.2f%% feed=%.2f%% gap=%+.2f", native*100, *feed*100, (native-*feed)*100)
}

// quoteLeverageAndNetAPY is a quote's executable leverage and the position's
// annual net APY at it after the performance fee, move costs excluded:
// display only.
func quoteLeverageAndNetAPY(e pilotEconomics, invested float64, m LaneEconomics, feeBPS int64) (float64, float64) {
	equity := invested + e.Proceeds - e.Debt
	if invested <= 0 || equity <= 0 {
		return 0, 0
	}
	collateral := invested + e.Proceeds
	gross := (collateral*math.Expm1(math.Log1p(m.NativeAPY)+math.Log1p(m.SupplyAPY)) - e.Debt*math.Expm1(e.APR)) / equity
	return collateral / equity, performanceFeeForecast(gross, feeBPS)
}

// All candidate collateral is supplied. A nonnegative asset exponential minus
// a nonnegative borrow exponential has its minimum at an endpoint when the
// initial collateral equity is positive. Reject the other case economically.
//
// invested is the collateral the move leaves earning, already net of the
// move's expense: the expense is money the move loses before anything earns.
// So the post-move NAV is collateral minus debt, and the expense is charged
// once, in Gain, which is measured against the NAV before the move.
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
	income := incomeAt(years)
	e.Gain = income - float64(expense)
	e.InitialNAV = collateral - e.Debt
	e.EndingNAV = e.InitialNAV + income
	return e
}

// SelectOpportunity never creates a transaction. It chooses where the
// vault's money goes next, and target() reads its latest result:
//
//   - cash (Voltr idle and Squads cash) enters the lane with the best
//     persistent net benefit (ENTER), a held lane included;
//   - a held lane's equity rotates to another lane (ROTATE) when a tranche's
//     forward gain there, net of the move's full round trip, beats what it
//     earns where it is, over the same horizon and persistence as an entry.
//
// Cash outranks a rotation: it needs no release. Markets are the active
// registry lanes the selector scores.
func SelectOpportunity(in SelectorInput, previous SelectorState) SelectorResult {
	s, p := in.Snapshot, in.Policy
	out := SelectorResult{Action: "KEEP", Reason: "no_worthwhile_move", SourceLane: s.RouteLane, SampledAt: in.Now}
	hold := func(reason string) SelectorResult { out.Reason = reason; out.State = SelectorState{}; return out }
	if err := p.validate(); err != nil || in.Now.IsZero() {
		return hold("invalid_selector_policy")
	}
	if s.MonitorsArmed && !selectorFeeBaselineKnown(s) {
		return hold("fee_hwm_baseline_unavailable")
	}
	base := Decide(s)
	if base.Action == RecoverTransaction || base.Action == HoldManualRecovery || s.Nonterminal != "" {
		return hold("execution_recovery_or_safety_first")
	}
	if s.WithdrawalDemandRaw > 0 || s.VoltrStrategyIdleRaw > 0 {
		return hold("withdrawal_or_accounting_first")
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
	lanes := make([]string, 0, len(markets))
	for lane := range markets {
		lanes = append(lanes, lane)
	}
	sort.Strings(lanes)
	out.State.Advantages = map[string]AdvantageWindow{}
	// sample records a priced candidate's persistence: its executable
	// quote's benefit, at the same MinimumBenefit threshold; an unprofitable
	// sample drops the window. A 1x quote persists in its own window: a
	// leveraged advantage never counts toward it, nor the reverse.
	sample := func(c CandidateForecast, move string, q MoveQuote) {
		if c.BlockedReason == "fee_forecast_unavailable" {
			delete(out.State.Advantages, move)
			delete(out.State.Advantages, unleveredAdvantageKey(move))
		}
		if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) {
			key := move
			if q.Unlevered {
				key = unleveredAdvantageKey(move)
			}
			sampleAdvantageWindow(&out, previous, key, in.Now, p)
		}
	}
	best := -1
	quotes := make(map[int]MoveQuote)
	for _, lane := range lanes {
		m := markets[lane]
		c := CandidateForecast{Lane: lane, NativeAPY: m.NativeAPY, FeedNativeAPY: m.FeedNativeAPY}
		if err := m.validate(in.Now, p); err != nil {
			c.BlockedReason = err.Error()
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
		l, known := findEarnLane(lane)
		unpricedDebt := !known || l.route.Kamino.DebtMint != bridgeUSDC
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
			if q.RotateFrom != "" || q.MinimumIdleRaw == 0 || q.MinimumIdleRaw > math.MaxInt64 {
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
		// Cash earns nothing where it is (KeepGainRaw).
		scoreMove(&c, s, p, m, *quote, out.KeepGainRaw, years)
		sample(c, lane, *quote)
		out.Candidates = append(out.Candidates, c)
		quotes[len(out.Candidates)-1] = *quote
		if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) && (best < 0 || c.BenefitRaw > out.Candidates[best].BenefitRaw) {
			best = len(out.Candidates) - 1
		}
	}
	// Held capital: every held lane's equity is scored against every other
	// destination, one tranche at a time.
	bestRotation, sources := -1, map[int]Snapshot{}
	held := s.heldLanes()
	for _, source := range in.Lanes {
		if !rotationSource(held, source) {
			continue
		}
		ms, sourceKnown := markets[source.RouteLane]
		for _, lane := range lanes {
			if !rotationPair(source.RouteLane, lane) {
				continue
			}
			m := markets[lane]
			key := rotationAdvantageKey(source.RouteLane, lane)
			c := CandidateForecast{SourceLane: source.RouteLane, Lane: lane, NativeAPY: m.NativeAPY, FeedNativeAPY: m.FeedNativeAPY}
			if !sourceKnown || ms.validate(in.Now, p) != nil || m.validate(in.Now, p) != nil {
				c.BlockedReason = "economic_evidence_unavailable"
				carryAdvantageWindows(&out, previous, key, c.BlockedReason)
				out.Candidates = append(out.Candidates, c)
				continue
			}
			var quote *MoveQuote
			for i := range in.Quotes {
				q := &in.Quotes[i]
				if q.RotateFrom == source.RouteLane && q.DestinationLane == lane && q.ObservationID == s.ObservationID && freshAt(in.Now, q.ObservedAt, p.QuoteMaxAge) && q.currentAtSlot(s.Slot) && q.CostRaw >= 0 && q.EvidenceID != "" &&
					q.EquityRaw <= laneReleaseLimitRaw(source) {
					if quote != nil {
						return hold("duplicate_move_quote")
					}
					quote = q
				}
			}
			if quote == nil {
				// The collector prices a pair only when the destination's
				// forward APY beats the source's: below it no quote could pay.
				c.BlockedReason = "bounded_move_cost_unavailable"
				if !rotationForwardEdge(source, ms, m) {
					c.BlockedReason = "forward_apy_not_higher"
				}
				carryAdvantageWindows(&out, previous, key, c.BlockedReason)
				out.Candidates = append(out.Candidates, c)
				continue
			}
			keep, ok := heldTrancheGain(source, ms, quote.EquityRaw, years)
			if !ok {
				c.BlockedReason = "held_forecast_unavailable"
				out.Candidates = append(out.Candidates, c)
				continue
			}
			scoreMove(&c, s, p, m, *quote, keep, years)
			sample(c, key, *quote)
			out.Candidates = append(out.Candidates, c)
			quotes[len(out.Candidates)-1], sources[len(out.Candidates)-1] = *quote, source
			if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) && (bestRotation < 0 || c.BenefitRaw > out.Candidates[bestRotation].BenefitRaw) {
				bestRotation = len(out.Candidates) - 1
			}
		}
	}
	// A lane whose economics are missing from this sample (feed gap) keeps
	// its windows unchanged too; MaxSampleGap bounds how long.
	for key, window := range previous.Advantages {
		if _, present := markets[advantageKeyLane(key)]; !present {
			if _, sampled := out.State.Advantages[key]; !sampled {
				out.State.Advantages[key] = window
			}
		}
	}
	if base.Action == ReportNAV {
		out.Reason = "accounting_first"
		return out
	}
	if best < 0 && bestRotation < 0 {
		out.Reason = "no_worthwhile_executable_move"
		return out
	}
	persistent := func(i int, key string) bool {
		if quotes[i].Unlevered {
			key = unleveredAdvantageKey(key)
		}
		window, stable := out.State.Advantages[key]
		return stable && in.Now.Sub(window.Since) >= p.Persistence
	}
	out.Reason = "advantage_not_yet_persistent"
	if best >= 0 {
		chosen := out.Candidates[best]
		out.DestinationLane = chosen.Lane
		out.EquityRaw = chosen.InvestedRaw
		if persistent(best, chosen.Lane) {
			quote := quotes[best]
			out.SelectedQuote = &quote
			out.Action = "ENTER"
			out.Reason = "persistent_net_benefit"
			return out
		}
	}
	if bestRotation >= 0 {
		chosen := out.Candidates[bestRotation]
		if persistent(bestRotation, rotationAdvantageKey(chosen.SourceLane, chosen.Lane)) {
			quote, source := quotes[bestRotation], sources[bestRotation]
			out.Action, out.Reason = "ROTATE", "persistent_net_benefit"
			out.SourceLane, out.DestinationLane, out.EquityRaw = source.RouteLane, chosen.Lane, quote.EquityRaw
			out.SourceCollateralRaw, out.SelectedQuote = source.PositionCollateralRaw, &quote
		}
	}
	return out
}

// scoreMove prices one executable quote into m's lane: the capital its
// cost leaves earning there over the horizon, with the performance fee
// reserved (selectorFeeReservedGain), against keepGain, what the same
// equity earns where it is, less the uncertainty share of the equity moved.
func scoreMove(c *CandidateForecast, s Snapshot, p SelectorPolicy, m LaneEconomics, quote MoveQuote, keepGain, years float64) {
	amount := quote.EquityRaw
	if quote.CostRaw >= amount {
		c.BlockedReason = "cost_exceeds_allocation"
		return
	}
	if !quote.validBorrow() {
		c.BlockedReason = "bounded_borrow_unavailable"
		return
	}
	c.CostsKnown, c.CostRaw, c.KeepGainRaw = true, quote.CostRaw, keepGain
	// The capital the move loses before anything earns.
	c.InvestedRaw = amount - quote.CostRaw
	c.IdleRaw = s.TotalVaultNAVRaw - amount
	economics, blocked := pilotQuoteEconomics(quote, m, float64(c.InvestedRaw), years)
	if blocked != "" {
		c.BlockedReason = blocked
		return
	}
	c.BorrowAPR = economics.APR
	c.Leverage, c.NetAPY = quoteLeverageAndNetAPY(economics, float64(c.InvestedRaw), m, s.ApprovedAdminPerformanceFeeBPS)
	c.BorrowBlocked, c.DebtRoomUSDCRaw = quote.Unlevered && !quote.SpreadUnlevered, quote.DebtRoomUSDCRaw
	var feeKnown bool
	c.GainRaw, feeKnown = selectorFeeReservedGain(s, p.Horizon, economics, float64(c.IdleRaw))
	// The candidate pays the repeated-fee reserve; KEEP gets its gross
	// return. Deduct uncertainty after that money edge.
	c.BenefitRaw = c.GainRaw - keepGain - float64(amount)*float64(p.UncertaintyBPS)/10_000
	if !feeKnown {
		c.BlockedReason = "fee_forecast_unavailable"
	} else if !finite(c.GainRaw) || !finite(c.BenefitRaw) {
		c.BlockedReason = "invalid_candidate_forecast"
	}
}

// rotationSource reports a held lane whose equity the selector may rotate:
// a positioned registry lane that can release a tranche.
func rotationSource(held []string, s Snapshot) bool {
	return containsLane(held, s.RouteLane) && s.HasPosition && earnHeldLane(s.RouteLane) && laneReleaseLimitRaw(s) > 0
}

// rotationPair reports a destination a source's equity may rotate into:
// another lane the selector enters (never an overflow lane) with its own
// collateral. Lanes sharing a collateral custody hold one collateral, and
// an overflow lane's moves with its source are overflow moves.
func rotationPair(source, destination string) bool {
	if source == destination || !selectorDestinationLane(destination) {
		return false
	}
	return !sameCollateralCustody(source, destination)
}

// sameCollateralCustody reports two registry lanes whose collateral the
// vault holds in one account: the same mint under the same token program.
// An unknown lane shares with everything.
func sameCollateralCustody(a, b string) bool {
	x, okA := findEarnLane(a)
	y, okB := findEarnLane(b)
	return !okA || !okB || x.route.Kamino.CollateralMint == y.route.Kamino.CollateralMint && x.route.CollateralTokenProgram == y.route.CollateralTokenProgram
}

// rotationForwardEdge is the market-level test a pair is priced on: the
// destination's forward APY at its entry leverage (1.5x where the spread
// rule levers it, else 1x) beats the source position's at its own leverage,
// both before fees. Below it no quote could pay its cost.
func rotationForwardEdge(source Snapshot, ms, md LaneEconomics) bool {
	equity := laneEquityRaw(source)
	if equity <= 0 {
		return false
	}
	held, ok := leverageLevelAPY(ms, float64(source.PositionCollateralValueRaw)/float64(equity), equity, true, 0, source)
	level := 1.0
	if _, levered := leverageUpAllowed(md, 1.5, 0); levered {
		level = 1.5
	}
	entered, okD := leverageLevelAPY(md, level, laneReleaseLimitRaw(source), false, 0)
	return ok && okD && entered > held
}

// heldTrancheGain is what equity of the source's position earns where it is
// over the horizon: its share of the position's collateral and debt at the
// source's current borrow rate, before fees (KEEP's upper return).
func heldTrancheGain(source Snapshot, m LaneEconomics, equity int64, years float64) (float64, bool) {
	total := laneEquityRaw(source)
	if total <= 0 || equity <= 0 || equity > total || source.PositionDebtValueRaw < 0 {
		return 0, false
	}
	debt := float64(source.PositionDebtValueRaw) * float64(equity) / float64(total)
	apr, err := projectedBorrowAPR(m, 0)
	if err != nil {
		return 0, false
	}
	e := pilotForecastEconomics(pilotEconomics{Debt: debt, Proceeds: debt, APR: apr}, float64(equity), m, years, 0)
	return e.Gain, finite(e.Gain)
}

// selectorRotates reports a result that rotates a held lane's equity.
func selectorRotates(r SelectorResult) bool {
	return r.Action == "ROTATE" || r.Action == "CANARY_ROTATE"
}

// rotationAdvantageKey is a rotation's persistence window.
func rotationAdvantageKey(source, destination string) string { return source + "->" + destination }

// advantageKeyLane is the destination lane a persistence window is for.
func advantageKeyLane(key string) string {
	key = strings.TrimSuffix(key, "|1x")
	if i := strings.Index(key, "->"); i >= 0 {
		return key[i+2:]
	}
	return key
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
	"bounded_move_cost_unavailable":      true,
	"pair_capacity_unknown":              true,
	"selector_source_quote_unavailable":  true,
	"selector_source_unavailable":        true,
	"selector_live_snapshot_unavailable": true,
}

// carryAdvantageWindows keeps a move's leveraged and 1x windows (a lane's,
// or a rotation's) exactly as they were (same Since and LastSample) when
// this sample could not price it for an availability reason.
func carryAdvantageWindows(out *SelectorResult, previous SelectorState, move, reason string) {
	if !selectorAvailabilityReasons[reason] {
		return
	}
	for _, key := range []string{move, unleveredAdvantageKey(move)} {
		if _, sampled := out.State.Advantages[key]; sampled {
			continue
		}
		if window, ok := previous.Advantages[key]; ok {
			out.State.Advantages[key] = window
		}
	}
}

// unleveredAdvantageKey is the persistence window of a move's 1x entry.
func unleveredAdvantageKey(move string) string { return move + "|1x" }

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

// validBorrow: a 1x quote borrows nothing; a levered one borrows within the
// equity it enters with, and converts no more principal than it owes.
func (q MoveQuote) validBorrow() bool {
	if q.EquityRaw <= 0 || !earnActiveLane(q.DestinationLane) {
		return false
	}
	if q.Unlevered {
		return q.BorrowReceiveRaw == 0 && q.BorrowFeeRaw == 0 && q.BorrowValueRaw == 0
	}
	return q.BorrowReceiveRaw > 0 && q.BorrowValueRaw > 0 && q.BorrowValueRaw <= uint64(q.EquityRaw) && q.ProceedsValueRaw <= q.BorrowValueRaw
}
