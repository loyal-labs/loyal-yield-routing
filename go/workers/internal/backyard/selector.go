package backyard

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Capital amounts are raw USDC (six decimals). Forecasts are estimates only;
// transaction amounts, reservations and reconciliation keep integer arithmetic.
// An entry deposits equity, then its first up step levers it to the ladder's
// first level (1.5x); later levels are later decisions, and a destination is
// priced at the level they settle it at (settledLevel).

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

func (e LaneEconomics) validate(now time.Time, p SelectorPolicy) error {
	if !earnActiveLane(e.Lane) || e.EvidenceID == "" || !freshAt(now, e.ObservedAt, p.MarketMaxAge) || !freshAt(now, e.NativeObservedAt, p.NativeMaxAge) {
		return fmt.Errorf(economicEvidenceUnavailable)
	}
	for _, rate := range []float64{e.NativeAPY, e.SupplyAPY, e.CurrentBorrowAPY} {
		if !finite(rate) || rate <= -1 || rate > 10 {
			return fmt.Errorf("invalid_economic_rate")
		}
	}
	// Borrowed may exceed total supply: KLend's total supply nets protocol and
	// referrer fees out of available liquidity, so utilization can exceed 100%.
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
	// KLend stores eleven points padded with the terminal 100% point.
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

const selectorHorizon = 30 * 24 * time.Hour

func DefaultSelectorPolicy() SelectorPolicy {
	// Shadow starting values; activation must review observed churn and costs.
	// 30-day horizon (Vlad OK 09-26): over 7 days a move had to repay its
	// cost plus the margin within one week (~5%/yr APY edge), so even a
	// clearly better lane (AUTO 9.2% vs Maple 3.7%) never passed.
	// MaxSampleGap 10 min (Vlad OK 09-26): samples skip while a NAV report or
	// a failed quote runs, so live gaps reached ~6 min and a 2-min gap reset
	// the 30-min window every few minutes; every sample taken must still lead.
	return SelectorPolicy{Horizon: selectorHorizon, Persistence: 30 * time.Minute, MaxSampleGap: 10 * time.Minute, MarketMaxAge: 90 * time.Second, NativeMaxAge: 2 * time.Hour, QuoteMaxAge: 30 * time.Second, MinimumBenefitRaw: 250_000, UncertaintyBPS: 10, IdleBufferRaw: 0}
}
func (p SelectorPolicy) validate() error {
	if p.Horizon <= 0 || p.Horizon > 365*24*time.Hour || p.Persistence <= 0 || p.MaxSampleGap <= 0 || p.MarketMaxAge <= 0 || p.NativeMaxAge <= 0 || p.QuoteMaxAge <= 0 || p.MinimumBenefitRaw < 0 || p.UncertaintyBPS < 0 || p.UncertaintyBPS > 10_000 || p.IdleBufferRaw < 0 {
		return fmt.Errorf("invalid_selector_policy")
	}
	return nil
}

type MoveQuote struct {
	MinimumIdleRaw   uint64 `json:"minimumIdleRaw"`
	BorrowReceiveRaw uint64 `json:"borrowReceiveRaw"`
	BorrowFeeRaw     uint64 `json:"borrowFeeRaw"`
	// Unlevered is a 1x entry: no up step (receive and fee are zero), scored
	// at the lane's 1x yield and persisted in its own advantage window. It is
	// quoted while the debt reserve has no room for the minimum borrow, or
	// (SpreadUnlevered) while the lane's spread at that borrow is below the
	// live leverage rule's 1.5x minimum.
	Unlevered        bool            `json:"unlevered,omitempty"`
	SpreadUnlevered  bool            `json:"spreadUnlevered,omitempty"`
	DebtRoomUSDCRaw  *uint64         `json:"debtRoomUsdcRaw,omitempty"`
	BorrowValueRaw   uint64          `json:"borrowValueRaw,omitempty"`
	ProceedsValueRaw uint64          `json:"proceedsValueRaw,omitempty"`
	DepositValueRaw  uint64          `json:"depositValueRaw,omitempty"`
	Keep             *heldSettlement `json:"keep,omitempty"`
	SourceLane       string          `json:"sourceLane"`
	RotateFrom       string          `json:"rotateFrom,omitempty"`
	DestinationLane  string          `json:"destinationLane"`
	ObservationID    string          `json:"observationId"`
	EquityRaw        int64           `json:"equityRaw"`
	// CostRaw is Cost's total: what the entry and its unwind are expected to
	// cost the vault's capital.
	CostRaw          int64     `json:"costRaw"`
	Cost             entryCost `json:"cost"`
	ObservedAt       time.Time `json:"observedAt"`
	EvidenceID       string    `json:"evidenceId"`
	SampleSlot       int64     `json:"sampleSlot"`
	ValidThroughSlot int64     `json:"validThroughSlot"`
}

// storedWindowValid checks a PERSISTED quote's own slot window against the
// fixed ceiling, not the runtime-measured observation lag: the lag is
// re-measured (48/49 slots) and a quote stored under 49 must stay a valid
// record when it later reads 48 (live 2026-09-29: the worker exited on
// invalid_selector_entry). Freshness for new spending is still
// currentAtSlot at the current slot, in target() and the canary.
func (q MoveQuote) storedWindowValid() bool {
	return q.SampleSlot > 0 && q.ValidThroughSlot >= q.SampleSlot && q.ValidThroughSlot-q.SampleSlot <= observationLagCeilingSlots
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
	Lanes         []Snapshot
	Markets       []LaneEconomics
	Quotes        []MoveQuote
	Policy        SelectorPolicy
}

type AdvantageWindow struct {
	Since      time.Time `json:"since"`
	LastSample time.Time `json:"lastSample"`
}

type SelectorState struct {
	Advantages map[advantageKey]AdvantageWindow `json:"advantages,omitempty"`
}

type advantageKey struct {
	source, lane string
	unlevered    bool
}

func (k advantageKey) unleveredKey() advantageKey {
	k.unlevered = true
	return k
}

func (k advantageKey) String() string {
	text := k.lane
	if k.source != "" {
		text = k.source + "->" + k.lane
	}
	if k.unlevered {
		text += "|1x"
	}
	return text
}

func (k advantageKey) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

func (k *advantageKey) UnmarshalText(text []byte) error {
	rest, unlevered := strings.CutSuffix(string(text), "|1x")
	source, lane, rotation := strings.Cut(rest, "->")
	if !rotation {
		source, lane = "", rest
	}
	*k = advantageKey{source: source, lane: lane, unlevered: unlevered}
	return nil
}

type CandidateForecast struct {
	SourceLane      string   `json:"sourceLane,omitempty"`
	Lane            string   `json:"lane"`
	InvestedRaw     int64    `json:"investedRaw"`
	CostRaw         int64    `json:"costRaw,omitempty"`
	KeepGainRaw     float64  `json:"keepGainRaw,omitempty"`
	IdleRaw         int64    `json:"idleRaw"`
	GrossGainRaw    float64  `json:"grossGainRaw"`
	GainRaw         float64  `json:"gainRaw"`
	CostsKnown      bool     `json:"costsKnown"`
	BenefitRaw      float64  `json:"benefitRaw"`
	BorrowAPR       float64  `json:"borrowApr"`
	BlockedReason   string   `json:"blockedReason,omitempty"`
	Leverage        float64  `json:"leverage,omitempty"`
	NetAPY          float64  `json:"netApy,omitempty"`
	BorrowBlocked   bool     `json:"borrowBlocked,omitempty"`
	DebtRoomUSDCRaw *uint64  `json:"debtRoomUsdcRaw,omitempty"`
	NativeAPY       float64  `json:"nativeApy,omitempty"`
	FeedNativeAPY   *float64 `json:"feedNativeApy,omitempty"`
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
	SelectedQuote   *MoveQuote          `json:"selectedQuote,omitempty"`
	SampledAt       time.Time           `json:"sampledAt,omitempty"`
	SourceRelease   *SourceRelease      `json:"sourceRelease,omitempty"`
}

func forecastGain(collateral, supplied, debt float64, e LaneEconomics, borrowAPR, years float64) float64 {
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
// Gain.
func pilotQuoteEconomics(quote MoveQuote, m LaneEconomics, investedNetOfExpense, years float64) (pilotEconomics, error) {
	if !quote.validBorrow() {
		return pilotEconomics{}, errors.New("bounded_borrow_unavailable")
	}
	if quote.Unlevered {
		return pilotForecastEconomics(pilotEconomics{}, investedNetOfExpense, m, years, quote.CostRaw), nil
	}
	e := pilotEconomics{DebtRaw: float64(quote.BorrowReceiveRaw) + float64(quote.BorrowFeeRaw), Debt: float64(quote.BorrowValueRaw), Proceeds: float64(quote.ProceedsValueRaw)}
	apr, err := projectedBorrowAPR(m, e.DebtRaw)
	if err != nil {
		return pilotEconomics{}, err
	}
	e.APR = apr
	return pilotForecastEconomics(e, investedNetOfExpense, m, years, quote.CostRaw), nil
}

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
		name := advantageKey{source: c.SourceLane, lane: c.Lane}.String()
		keys = append(keys, fmt.Sprintf("%s:%s:%s:%s:%t", name, reason, borrow, lev, roomOpen))
		parts = append(parts, fmt.Sprintf("%s(lev=%s net=%s %s borrow=%s room=%s benefit=%s reason=%s)", name, lev, net, nativeFeedText(c.NativeAPY, c.FeedNativeAPY), borrow, room, benefit, reason))
	}
	return strings.Join(keys, "|"), fmt.Sprintf("backyard-rwa-worker: selector sample action=%s reason=%s source=%s candidates=%s", result.Action, result.Reason, result.SourceLane, strings.Join(parts, " "))
}

func nativeFeedText(native float64, feed *float64) string {
	if feed == nil {
		return fmt.Sprintf("native=%.2f%% feed=unavailable", native*100)
	}
	return fmt.Sprintf("native=%.2f%% feed=%.2f%% gap=%+.2f", native*100, *feed*100, (native-*feed)*100)
}

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
func pilotForecastEconomics(e pilotEconomics, investedNetOfExpense float64, m LaneEconomics, years float64, expense int64) pilotEconomics {
	collateral := investedNetOfExpense + e.Proceeds
	assetRate := math.Log1p(m.NativeAPY) + math.Log1p(m.SupplyAPY)
	incomeAt := func(t float64) float64 {
		return collateral*math.Expm1(assetRate*t) - e.Debt*math.Expm1(e.APR*t)
	}
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

const completeEntryQuoteUnavailable = "complete_entry_quote_unavailable"

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
	if withdrawalShortfallRaw(s) > 0 || s.VoltrStrategyIdleRaw > 0 {
		return hold("withdrawal_or_accounting_first")
	}
	if !s.Fresh || s.SquadsIdleRaw < 0 || s.TotalVaultNAVRaw > 1<<53 || s.TotalVaultNAVRaw < 0 || s.PositionCollateralValueRaw < 0 || s.PositionDebtValueRaw < 0 || s.CollateralIdleValueRaw < 0 {
		return hold("invalid_holdings")
	}
	equity := s.TotalVaultNAVRaw - s.WithdrawalDemandRaw - p.IdleBufferRaw
	if equity <= 0 {
		return hold("no_investable_equity")
	}
	out.EquityRaw = equity
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
	out.State.Advantages = map[advantageKey]AdvantageWindow{}
	sample := func(c CandidateForecast, move advantageKey, q MoveQuote) {
		if c.BlockedReason == "fee_forecast_unavailable" {
			delete(out.State.Advantages, move)
			delete(out.State.Advantages, move.unleveredKey())
		}
		if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) {
			key := move
			if q.Unlevered {
				key = move.unleveredKey()
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
			carryAdvantageWindows(&out, previous, advantageKey{lane: lane}, c.BlockedReason)
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
			level := sizeSettledLevel(m, displayAmount)
			apr, err := projectedBorrowAPR(m, float64(displayAmount)*(level-1))
			if err == nil {
				c.BorrowAPR = apr
				c.GrossGainRaw = forecastGain(float64(displayAmount)*level, float64(displayAmount)*level, float64(displayAmount)*(level-1), m, apr, years)
			}
		}
		if !known {
			c.BlockedReason = pairCapacityUnknown
			if m.EntryBlockedReason != "" {
				c.BlockedReason = m.EntryBlockedReason
			}
			carryAdvantageWindows(&out, previous, advantageKey{lane: lane}, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		if amount == 0 || m.EntryBlockedReason != "" {
			c.BlockedReason = m.EntryBlockedReason
			if c.BlockedReason == "" {
				c.BlockedReason = "entry_closed"
			}
			carryAdvantageWindows(&out, previous, advantageKey{lane: lane}, c.BlockedReason)
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
					return hold(duplicateMoveQuote)
				}
				quote = q
			}
		}
		if quote == nil {
			c.BlockedReason = boundedMoveCostUnavailable
			carryAdvantageWindows(&out, previous, advantageKey{lane: lane}, c.BlockedReason)
			out.Candidates = append(out.Candidates, c)
			continue
		}
		scoreMove(&c, s, p, m, *quote, equity, out.KeepGainRaw, years)
		sample(c, advantageKey{lane: lane}, *quote)
		out.Candidates = append(out.Candidates, c)
		quotes[len(out.Candidates)-1] = *quote
		if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) && (best < 0 || c.BenefitRaw > out.Candidates[best].BenefitRaw) {
			best = len(out.Candidates) - 1
		}
	}
	bestRotation, sources := -1, map[int]Snapshot{}
	held := s.heldLanes()
	for _, source := range in.Lanes {
		if !rotationSource(held, source) {
			continue
		}
		ms, sourceKnown := markets[source.RouteLane]
		destination := rotationDestination(source, markets, in.Now, p)
		for _, lane := range lanes {
			if !rotationPair(source.RouteLane, lane) {
				continue
			}
			m := markets[lane]
			key := advantageKey{source: source.RouteLane, lane: lane}
			c := CandidateForecast{SourceLane: source.RouteLane, Lane: lane, NativeAPY: m.NativeAPY, FeedNativeAPY: m.FeedNativeAPY}
			if !sourceKnown || ms.validate(in.Now, p) != nil || m.validate(in.Now, p) != nil {
				c.BlockedReason = economicEvidenceUnavailable
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
						return hold(duplicateMoveQuote)
					}
					quote = q
				}
			}
			if quote == nil {
				c.BlockedReason = boundedMoveCostUnavailable
				if _, edge := rotationForwardAPY(source, ms, m); !edge {
					c.BlockedReason = "forward_apy_not_higher"
				} else if lane != destination {
					c.BlockedReason = "forward_apy_not_best"
				} else if moveGainCeiling(&source, ms, m, min(int64(strategyTwoBridgeLegCapRaw), laneReleaseLimitRaw(source)), p) <= float64(p.MinimumBenefitRaw) {
					c.BlockedReason = gainBelowMinimumBenefit
				}
				carryAdvantageWindows(&out, previous, key, c.BlockedReason)
				out.Candidates = append(out.Candidates, c)
				continue
			}
			keep, ok := heldTrancheGain(source, ms, quote.Keep, quote.EquityRaw, years)
			if !ok {
				c.BlockedReason = "held_forecast_unavailable"
				out.Candidates = append(out.Candidates, c)
				continue
			}
			scoreMove(&c, s, p, m, *quote, quote.EquityRaw, keep, years)
			sample(c, key, *quote)
			out.Candidates = append(out.Candidates, c)
			quotes[len(out.Candidates)-1], sources[len(out.Candidates)-1] = *quote, source
			if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) && (bestRotation < 0 || c.BenefitRaw > out.Candidates[bestRotation].BenefitRaw) {
				bestRotation = len(out.Candidates) - 1
			}
		}
	}
	for key, window := range previous.Advantages {
		if _, present := markets[key.lane]; !present {
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
	persistent := func(i int, key advantageKey) bool {
		if quotes[i].Unlevered {
			key = key.unleveredKey()
		}
		window, stable := out.State.Advantages[key]
		return stable && in.Now.Sub(window.Since) >= p.Persistence
	}
	out.Reason = "advantage_not_yet_persistent"
	if best >= 0 {
		chosen := out.Candidates[best]
		out.DestinationLane = chosen.Lane
		out.EquityRaw = chosen.InvestedRaw
		if persistent(best, advantageKey{lane: chosen.Lane}) {
			quote := quotes[best]
			out.SelectedQuote = &quote
			out.Action = "ENTER"
			out.Reason = "persistent_net_benefit"
			return out
		}
	}
	if bestRotation >= 0 {
		chosen := out.Candidates[bestRotation]
		if persistent(bestRotation, advantageKey{source: chosen.SourceLane, lane: chosen.Lane}) {
			quote, source := quotes[bestRotation], sources[bestRotation]
			out.Action, out.Reason = "ROTATE", "persistent_net_benefit"
			out.SourceLane, out.DestinationLane, out.EquityRaw = source.RouteLane, chosen.Lane, quote.EquityRaw
			out.SourceRelease, out.SelectedQuote = newSourceRelease(source, quote.EquityRaw), &quote
		}
	}
	return out
}

func scoreMove(c *CandidateForecast, s Snapshot, p SelectorPolicy, m LaneEconomics, quote MoveQuote, uncertain int64, keepGain, years float64) {
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
	c.InvestedRaw = amount - quote.CostRaw
	c.IdleRaw = s.TotalVaultNAVRaw - amount
	economics, err := pilotQuoteEconomics(quote, m, float64(c.InvestedRaw), years)
	if err != nil {
		c.BlockedReason = err.Error()
		return
	}
	c.BorrowAPR = economics.APR
	c.Leverage, c.NetAPY = quoteLeverageAndNetAPY(economics, float64(c.InvestedRaw), m, s.ApprovedAdminPerformanceFeeBPS)
	c.BorrowBlocked, c.DebtRoomUSDCRaw = quote.Unlevered && !quote.SpreadUnlevered, quote.DebtRoomUSDCRaw
	var feeKnown bool
	c.GainRaw, feeKnown = selectorFeeReservedGain(s, p.Horizon, economics, float64(c.IdleRaw))
	c.BenefitRaw = c.GainRaw - keepGain - float64(uncertain)*float64(p.UncertaintyBPS)/10_000
	if !feeKnown {
		c.BlockedReason = "fee_forecast_unavailable"
	} else if !finite(c.GainRaw) || !finite(c.BenefitRaw) {
		c.BlockedReason = "invalid_candidate_forecast"
	}
}

func rotationSource(held []string, s Snapshot) bool {
	return containsLane(held, s.RouteLane) && s.HasPosition && earnHeldLane(s.RouteLane) && laneSupportsSwap(s.RouteLane, SwapCollateralToStableStep) && laneReleaseLimitRaw(s) > 0
}

func rotationPair(source, destination string) bool {
	if source == destination || !selectorDestinationLane(destination) {
		return false
	}
	return !sameCollateralCustody(source, destination)
}

func sameCollateralCustody(a, b string) bool {
	x, okA := findEarnLane(a)
	y, okB := findEarnLane(b)
	return !okA || !okB || x.route.Kamino.CollateralMint == y.route.Kamino.CollateralMint && x.route.CollateralTokenProgram == y.route.CollateralTokenProgram
}

func rotationForwardAPY(source Snapshot, ms, md LaneEconomics) (float64, bool) {
	equity := laneEquityRaw(source)
	if equity <= 0 {
		return 0, false
	}
	held, ok := leverageLevelAPY(ms, float64(source.PositionCollateralValueRaw)/float64(equity), equity, true, 0, source)
	size := laneReleaseLimitRaw(source)
	entered, okD := leverageLevelAPY(md, sizeSettledLevel(md, size), size, false, 0)
	return entered, ok && okD && entered > held
}

func rotationDestination(source Snapshot, markets map[string]LaneEconomics, now time.Time, p SelectorPolicy) string {
	ms, ok := markets[source.RouteLane]
	if !ok || ms.validate(now, p) != nil {
		return ""
	}
	best, bestAPY := "", 0.0
	for _, lane := range earnLaneIDs(true) {
		md, ok := markets[lane]
		if !ok || !rotationPair(source.RouteLane, lane) || md.validate(now, p) != nil {
			continue
		}
		if apy, edge := rotationForwardAPY(source, ms, md); edge && (best == "" || apy > bestAPY) {
			best, bestAPY = lane, apy
		}
	}
	return best
}

const gainBelowMinimumBenefit = "gain_below_minimum_benefit"

func moveGainCeiling(source *Snapshot, ms, md LaneEconomics, size int64, p SelectorPolicy) float64 {
	apy, ok := leverageLevelAPY(md, sizeSettledLevel(md, size), size, false, 0)
	if !ok {
		return math.Inf(1)
	}
	if source != nil {
		equity := laneEquityRaw(*source)
		if equity <= 0 {
			return math.Inf(1)
		}
		held, ok := leverageLevelAPY(ms, float64(source.PositionCollateralValueRaw)/float64(equity), equity, true, 0, *source)
		if !ok {
			return math.Inf(1)
		}
		apy -= held
	}
	return float64(size) * apy * p.Horizon.Hours() / (365.25 * 24)
}

func heldTrancheGain(source Snapshot, m LaneEconomics, keep *heldSettlement, equity int64, years float64) (float64, bool) {
	total := laneEquityRaw(source)
	if total <= 0 || equity <= 0 || equity > total || source.PositionDebtValueRaw < 0 {
		return 0, false
	}
	settled := heldSettlement{CollateralValueRaw: source.PositionCollateralValueRaw, DebtValueRaw: source.PositionDebtValueRaw}
	if keep != nil {
		settled = *keep
	}
	share := float64(equity) / float64(total)
	apr, err := projectedBorrowAPR(m, settled.RateDebtRaw)
	if err != nil || settled.DebtValueRaw < 0 || settled.CollateralValueRaw < settled.DebtValueRaw || settled.CostRaw < 0 {
		return 0, false
	}
	debt := float64(settled.DebtValueRaw) * share
	e := pilotForecastEconomics(pilotEconomics{Debt: debt, Proceeds: float64(settled.CollateralValueRaw)*share - float64(equity), APR: apr}, float64(equity), m, years, int64(math.Ceil(float64(settled.CostRaw)*share)))
	return e.Gain, finite(e.Gain)
}

func selectorRotates(r SelectorResult) bool {
	return r.Action == "ROTATE" || r.Action == "CANARY_ROTATE"
}

const (
	economicEvidenceUnavailable = "economic_evidence_unavailable"
	boundedMoveCostUnavailable  = "bounded_move_cost_unavailable"
	pairCapacityUnknown         = "pair_capacity_unknown"
	duplicateMoveQuote          = "duplicate_move_quote"
)

var selectorAvailabilityReasons = map[string]bool{
	economicEvidenceUnavailable:     true,
	completeEntryQuoteUnavailable:   true,
	boundedMoveCostUnavailable:      true,
	pairCapacityUnknown:             true,
	selectorSourceQuoteUnavailable:  true,
	selectorSourceUnavailable:       true,
	selectorLiveSnapshotUnavailable: true,
}

func carryAdvantageWindows(out *SelectorResult, previous SelectorState, move advantageKey, reason string) {
	if !selectorAvailabilityReasons[reason] {
		return
	}
	for _, key := range []advantageKey{move, move.unleveredKey()} {
		if _, sampled := out.State.Advantages[key]; sampled {
			continue
		}
		if window, ok := previous.Advantages[key]; ok {
			out.State.Advantages[key] = window
		}
	}
}

func sampleAdvantageWindow(out *SelectorResult, previous SelectorState, key advantageKey, now time.Time, p SelectorPolicy) {
	window := AdvantageWindow{Since: now, LastSample: now}
	old, ok := previous.Advantages[key]
	if ok && !old.Since.IsZero() && !old.Since.After(old.LastSample) && !old.LastSample.After(now) && now.Sub(old.LastSample) <= p.MaxSampleGap {
		window.Since = old.Since
	}
	out.State.Advantages[key] = window
}

func (q MoveQuote) validBorrow() bool {
	if q.EquityRaw <= 0 || !earnActiveLane(q.DestinationLane) {
		return false
	}
	if q.Unlevered {
		return q.BorrowReceiveRaw == 0 && q.BorrowFeeRaw == 0 && q.BorrowValueRaw == 0
	}
	capLevel := laneMaxLevel(q.DestinationLane)
	return q.BorrowReceiveRaw > 0 && q.BorrowValueRaw > 0 && q.ProceedsValueRaw <= q.BorrowValueRaw &&
		float64(q.BorrowValueRaw)*capLevel <= (capLevel-1)*(float64(q.DepositValueRaw)+float64(q.ProceedsValueRaw))
}
