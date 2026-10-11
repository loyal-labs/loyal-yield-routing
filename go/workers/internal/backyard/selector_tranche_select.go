package backyard

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"
)

// TrancheCandidate is one (held source, destination, E) move, priced on the
// margin: the destination's gain on E at its projected utilization, minus
// the income the source stops earning on E, minus the tranche's expected
// cost and its fee reserve.
type TrancheCandidate struct {
	Source             string  `json:"source"`
	Destination        string  `json:"destination"`
	EquityRaw          int64   `json:"equityRaw"`
	CostRaw            int64   `json:"costRaw"`
	DestinationGainRaw float64 `json:"destinationGainRaw"`
	SourceIncomeRaw    float64 `json:"sourceIncomeRaw"`
	FeeReserveRaw      float64 `json:"feeReserveRaw"`
	BenefitRaw         float64 `json:"benefitRaw"`
	CostsKnown         bool    `json:"costsKnown"`
	BlockedReason      string  `json:"blockedReason,omitempty"`
}

// heldLaneFact is one non-working held lane's position value, for the
// worst-yield ordering of held lanes.
type heldLaneFact struct {
	Lane               string
	CollateralValueRaw int64
	DebtValueRaw       int64
}

type trancheSelection struct {
	Candidates []TrancheCandidate
	Windows    map[string]AdvantageWindow
	Chosen     *TrancheCandidate
	Quote      *MoveQuote
	Reason     string
}

func trancheWindowKey(source, destination string) string { return source + ">" + destination }

// trancheSourceSettled: the working lane is a quiet funded source with no
// cash in flight anywhere, so a tranche can start from it.
func trancheSourceSettled(s Snapshot) bool {
	return trancheSettledView(s) && s.WithdrawalDemandRaw == 0 && !s.Unwind && !s.CutoverDrain && !s.UnwindRefreshRequired &&
		s.PartialWithdrawalOperationID == "" && !s.OtherLanesCustodyHeld && laneTransientClear(s, true) &&
		!selectorTrancheInProgress(s) && s.HasPosition && s.PositionCollateralRaw > 0
}

// trancheSourceEquityRaw is the source position's own equity.
func trancheSourceEquityRaw(s Snapshot) int64 {
	return s.PositionCollateralValueRaw + s.CollateralIdleValueRaw - s.PositionDebtValueRaw
}

// trancheSourceMaxRaw is the largest E the debt-preserving partial
// withdrawal frees from this source: strictly below the planner's 90%
// exclusion and within its bounded release rounds. A tranche never empties
// a lane; emptying is the full-exit path and its debt-clear confirmation.
func trancheSourceMaxRaw(s Snapshot) int64 {
	equity := trancheSourceEquityRaw(s)
	if equity <= 0 {
		return 0
	}
	hi := (equity*partialWithdrawalFullExitBPS - 1) / 10_000
	if hi <= 0 {
		return 0
	}
	if partialWithdrawalFitsRounds(s, hi, equity) {
		return hi
	}
	lo := int64(0)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if partialWithdrawalFitsRounds(s, mid, equity) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// trancheReleaseRounds is how many release rounds freeing E takes at the
// source's level; each round repeats the fixed per-transaction costs.
func trancheReleaseRounds(s Snapshot, e int64) int64 {
	equity := trancheSourceEquityRaw(s)
	for rounds := int64(1); rounds <= partialWithdrawalMaxRounds; rounds++ {
		if partialWithdrawalFitsRoundsWithin(s, e, equity, int(rounds)) {
			return rounds
		}
	}
	return partialWithdrawalMaxRounds
}

// trancheSourcePartialCost scales the source's priced full exit to a
// partial exit of E: every transaction's network and protocol-rounding cost
// is fixed and repeats per release round, while swap loss scales with the
// size moved. Expected expense scales the same way and stays under the bound.
func trancheSourcePartialCost(recipe selectorRecipe, s Snapshot, e int64) (bound, expected int64, err error) {
	equity := trancheSourceEquityRaw(s)
	if equity <= 0 || e <= 0 || e > equity {
		return 0, 0, budgetHold("tranche_source_cost_unavailable")
	}
	var fixed, variable int64
	for _, cost := range recipe.Costs {
		if cost.ExecutionCost == nil {
			// Unclassified: the whole step counts as fixed, conservatively.
			if fixed, err = budgetSum(fixed, cost.TotalMicros); err != nil {
				return 0, 0, err
			}
			continue
		}
		if fixed, err = budgetSum(fixed, cost.ExecutionCost.NetworkMicros, cost.ExecutionCost.ProtocolRoundingMicros); err != nil {
			return 0, 0, err
		}
		if variable, err = budgetSum(variable, cost.ExecutionCost.SwapLossMicros); err != nil {
			return 0, 0, err
		}
	}
	rounds := trancheReleaseRounds(s, e)
	scale := func(raw int64) int64 {
		v := new(big.Int).Mul(big.NewInt(raw), big.NewInt(e))
		v.Add(v, big.NewInt(equity-1)).Quo(v, big.NewInt(equity))
		return v.Int64()
	}
	if fixed > math.MaxInt64/rounds {
		return 0, 0, budgetHold("tranche_source_cost_overflow")
	}
	if bound, err = budgetSum(fixed*rounds, scale(variable)); err != nil {
		return 0, 0, err
	}
	fullExpected := recipe.CostRaw
	if recipe.ExpectedCostRaw != nil && *recipe.ExpectedCostRaw >= 0 && *recipe.ExpectedCostRaw <= recipe.CostRaw {
		fullExpected = *recipe.ExpectedCostRaw
	}
	expectedFixed := min(fixed, fullExpected)
	if expected, err = budgetSum(expectedFixed*rounds, scale(fullExpected-expectedFixed)); err != nil {
		return 0, 0, err
	}
	return bound, min(expected, bound), nil
}

// trancheFeeReserve re-derives the selector's fee reserve for one tranche of
// a repeated program. Routine reports happen with or without the tranche,
// so only the tranche's own fee-bearing steps (each release round's legs
// plus the destination recipe) add rounding and dilution units, and only its
// marginal income can crystallize a new performance fee. Repeated tranches
// each reserve their own steps; none reuses another's.
func trancheFeeReserve(s Snapshot, marginal float64, steps int64) (float64, bool) {
	if !finite(marginal) || steps <= 0 || s.TotalVaultNAVRaw <= 0 {
		return 0, false
	}
	fee := max(marginal, 0) * float64(approvedAdminPerformanceFeeBPS) / 10_000
	if !s.MonitorsArmed {
		return fee, true // projection only
	}
	if !selectorFeeBaselineKnown(s) {
		return 0, false
	}
	maxNAV := math.Ceil(float64(s.TotalVaultNAVRaw) + max(marginal, 0))
	minNAV := math.Floor(min(float64(s.TotalVaultNAVRaw), float64(s.VoltrTotalValueRaw)))
	if !finite(maxNAV) || maxNAV > 1<<53 || minNAV <= 0 {
		return 0, false
	}
	// Per report: <=1 asset raw of fee rounding, <=1 LP valued at maxNAV per
	// effective LP, and HWM truncation dust (see selectorFeeReservedGain).
	unit := maxNAV/float64(s.LPSupplyInclFeesRaw) + 1 +
		2*float64(s.LPSupplyInclFeesRaw)*maxNAV/(minNAV*float64(uint64(1)<<48))*float64(approvedAdminPerformanceFeeBPS)/10_000
	reserve := math.Ceil((fee + unit*float64(steps)) * maxNAV / minNAV)
	if !finite(reserve) || 2*reserve >= minNAV {
		return 0, false
	}
	return reserve, true
}

// trancheSteps is the fee-bearing step bound of one tranche: every release
// round of the source half repeats its legs, plus one destination recipe and
// the handoff reports.
func trancheSteps(rounds int64) int64 {
	return rounds*8 + maxSelectorRecipeSteps + 2
}

// selectTranche is the pure tranche selector. The working lane is the
// source; every other active lane is a destination. A destination that
// would make a second sibling (Prime) lane held, or a fourth held lane, is
// refused; sibling-to-sibling moves keep the full-exit path. Persistence is
// per (source, destination), sampled only from an executable quote's priced
// benefit at the same MinimumBenefit threshold, so a closed reserve never
// accrues.
func selectTranche(in SelectorInput, previous map[string]AdvantageWindow) trancheSelection {
	s, p := in.Snapshot, in.Policy
	out := trancheSelection{Windows: map[string]AdvantageWindow{}, Reason: "no_worthwhile_tranche"}
	carry := func(key, reason string) {
		if !selectorAvailabilityReasons[reason] {
			return
		}
		if window, ok := previous[key]; ok {
			out.Windows[key] = window
		}
	}
	held := s.heldLanes()
	markets := map[string]LaneEconomics{}
	for _, m := range in.Markets {
		markets[m.Lane] = m
	}
	lanes := make([]string, 0, len(markets))
	for lane := range markets {
		if lane != s.RouteLane {
			lanes = append(lanes, lane)
		}
	}
	sort.Strings(lanes)
	sourceReason := ""
	switch {
	case p.validate() != nil || in.Now.IsZero():
		sourceReason = "invalid_selector_policy"
	case !earnActiveLane(s.RouteLane):
		sourceReason = "tranche_source_full_exit_only"
	case !trancheSourceSettled(s):
		sourceReason = "tranche_finish_current_work_first"
	case s.MonitorsArmed && !selectorFeeBaselineKnown(s):
		sourceReason = "fee_hwm_baseline_unavailable"
	}
	sourceMarket, sourceOK := markets[s.RouteLane]
	if sourceReason == "" && (!sourceOK || sourceMarket.validate(in.Now, p) != nil) {
		sourceReason = "current_lane_economics_unavailable"
	}
	maxE := int64(0)
	if sourceReason == "" {
		maxE = min(trancheSourceMaxRaw(s), int64(strategyTwoBridgeLegCapRaw))
		if maxE <= 0 {
			sourceReason = "tranche_source_partial_unproven"
		}
	}
	years := p.Horizon.Hours() / (365.25 * 24)
	equity := trancheSourceEquityRaw(s)
	sourceGross := 0.0
	if sourceReason == "" {
		collateral := float64(s.PositionCollateralValueRaw) + float64(s.CollateralIdleValueRaw)
		sourceGross = forecastGain(collateral, float64(s.PositionCollateralValueRaw), float64(s.PositionDebtValueRaw), sourceMarket, math.Log1p(sourceMarket.CurrentBorrowAPY), years)
		if !finite(sourceGross) {
			sourceReason = "invalid_keep_forecast"
		}
	}
	best := -1
	quotes := map[string]MoveQuote{}
	for _, lane := range lanes {
		c := TrancheCandidate{Source: s.RouteLane, Destination: lane}
		key := trancheWindowKey(c.Source, lane)
		block := func(reason string) {
			c.BlockedReason = reason
			carry(key, reason)
			out.Candidates = append(out.Candidates, c)
		}
		if sourceReason != "" {
			block(sourceReason)
			continue
		}
		m := markets[lane]
		if err := m.validate(in.Now, p); err != nil {
			block(err.Error())
			continue
		}
		destinationHeld := containsLane(held, lane)
		if siblingLanes(s.RouteLane, lane) {
			block("tranche_sibling_uses_full_exit")
			continue
		}
		sibling := false
		for _, other := range held {
			sibling = sibling || (other != lane && siblingLanes(other, lane))
		}
		if sibling {
			block("tranche_sibling_lane_already_held")
			continue
		}
		if !destinationHeld && len(held) >= maxHeldLanes {
			block("tranche_held_lane_limit")
			continue
		}
		if reason := in.trancheRefusals[key]; reason != "" {
			block(reason)
			continue
		}
		var quote *MoveQuote
		matches := 0
		for i := range in.Quotes {
			q := &in.Quotes[i]
			if q.Tranche && q.SourceLane == s.RouteLane && q.DestinationLane == lane && q.ObservationID == s.ObservationID &&
				freshAt(in.Now, q.ObservedAt, p.QuoteMaxAge) && q.currentAtSlot(s.Slot) && q.CostRaw >= 0 && q.EvidenceID != "" {
				quote, matches = q, matches+1
			}
		}
		if matches > 1 {
			block("duplicate_move_quote")
			continue
		}
		if quote == nil {
			block("bounded_move_cost_unavailable")
			continue
		}
		c.EquityRaw, c.CostRaw = quote.EquityRaw, quote.selectorEconomicCostRaw()
		if c.EquityRaw > maxE {
			block("tranche_source_partial_unproven")
			continue
		}
		if quote.CostRaw >= c.EquityRaw || !quote.validBorrow() {
			block("cost_exceeds_allocation")
			continue
		}
		invested := float64(c.EquityRaw - c.CostRaw)
		economics, blocked := pilotQuoteEconomics(*quote, m, invested, years)
		if blocked != "" {
			block(blocked)
			continue
		}
		c.CostsKnown = true
		// Destination income on E before the move's expense; the expense is
		// charged once below.
		c.DestinationGainRaw = economics.Gain + float64(c.CostRaw)
		c.SourceIncomeRaw = sourceGross * float64(c.EquityRaw) / float64(equity)
		marginal := c.DestinationGainRaw - c.SourceIncomeRaw
		reserve, known := trancheFeeReserve(s, marginal, trancheSteps(trancheReleaseRounds(s, c.EquityRaw)))
		c.FeeReserveRaw = reserve
		c.BenefitRaw = marginal - reserve - float64(c.CostRaw) - float64(c.EquityRaw)*float64(p.UncertaintyBPS)/10_000
		switch {
		case !known:
			c.BlockedReason = "fee_forecast_unavailable"
		case !finite(c.BenefitRaw):
			c.BlockedReason = "invalid_candidate_forecast"
		}
		if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) {
			sampleTrancheWindow(out.Windows, previous, key, in.Now, p)
			quotes[key] = *quote
		}
		out.Candidates = append(out.Candidates, c)
		if c.BlockedReason == "" && c.BenefitRaw > float64(p.MinimumBenefitRaw) && (best < 0 || c.BenefitRaw > out.Candidates[best].BenefitRaw) {
			best = len(out.Candidates) - 1
		}
	}
	if sourceReason != "" {
		out.Reason = sourceReason
		return out
	}
	if best < 0 {
		return out
	}
	chosen := out.Candidates[best]
	window, stable := out.Windows[trancheWindowKey(chosen.Source, chosen.Destination)]
	if !stable || in.Now.Sub(window.Since) < p.Persistence {
		out.Reason = "tranche_advantage_not_yet_persistent"
		return out
	}
	quote := quotes[trancheWindowKey(chosen.Source, chosen.Destination)]
	out.Chosen, out.Quote, out.Reason = &chosen, &quote, "persistent_tranche_benefit"
	return out
}

func sampleTrancheWindow(windows, previous map[string]AdvantageWindow, key string, now time.Time, p SelectorPolicy) {
	window := AdvantageWindow{Since: now, LastSample: now}
	old, ok := previous[key]
	if ok && !old.Since.IsZero() && !old.Since.After(old.LastSample) && !old.LastSample.After(now) && now.Sub(old.LastSample) <= p.MaxSampleGap {
		window.Since = old.Since
	}
	windows[key] = window
}

// laneNetYield is a held lane's current net yield on its equity at its
// markets, nil-safe: unknown is +Inf, never chosen as the worst.
func laneNetYield(m LaneEconomics, collateral, debt int64) float64 {
	equity := float64(collateral - debt)
	if equity <= 0 {
		return math.Inf(1)
	}
	gross := float64(collateral)*(m.NativeAPY+m.SupplyAPY) - float64(debt)*m.CurrentBorrowAPY
	return gross / equity
}

// worstYieldHeldLane orders held active lanes by net yield: the worst is the
// next tranche source, so capital leaves it first. It returns "" unless a
// lane at least minimumGapBPS below the working lane's yield exists.
func worstYieldHeldLane(s Snapshot, facts []heldLaneFact, markets []LaneEconomics, now time.Time, p SelectorPolicy) string {
	byLane := map[string]LaneEconomics{}
	for _, m := range markets {
		if m.validate(now, p) == nil {
			byLane[m.Lane] = m
		}
	}
	current, ok := byLane[s.RouteLane]
	if !ok || !earnActiveLane(s.RouteLane) {
		return ""
	}
	working := laneNetYield(current, s.PositionCollateralValueRaw, s.PositionDebtValueRaw)
	worst, worstYield := "", working-float64(trancheRotationGapBPS)/10_000
	for _, f := range facts {
		m, ok := byLane[f.Lane]
		if !ok || f.Lane == s.RouteLane || !earnActiveLane(f.Lane) {
			continue
		}
		if y := laneNetYield(m, f.CollateralValueRaw, f.DebtValueRaw); y < worstYield {
			worst, worstYield = f.Lane, y
		}
	}
	return worst
}

// trancheRotationGapBPS keeps the working lane unless another held lane
// yields clearly less: a rotation moves no money but bumps the route
// generation, so near-equal lanes must not flap.
const trancheRotationGapBPS = 10

// trancheSampleSuffix renders the held lanes and every tranche candidate
// for the selector sample line; key is its shape without drifting numbers.
func trancheSampleSuffix(heldLanes string, candidates []TrancheCandidate) (key, line string) {
	usd := func(raw float64) string { return fmt.Sprintf("$%.2f", raw/1e6) }
	keys := []string{heldLanes}
	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		e, benefit, reason := "-", "-", c.BlockedReason
		if c.CostsKnown {
			e, benefit = usd(float64(c.EquityRaw)), usd(c.BenefitRaw)
		}
		if reason == "" {
			reason = "-"
		}
		keys = append(keys, fmt.Sprintf("%s>%s:%s", c.Source, c.Destination, reason))
		parts = append(parts, fmt.Sprintf("%s>%s(E=%s benefit=%s reason=%s)", c.Source, c.Destination, e, benefit, reason))
	}
	held := heldLanes
	if held == "" {
		held = "-"
	}
	return strings.Join(keys, "|"), fmt.Sprintf(" held=%s tranches=%s", held, strings.Join(parts, " "))
}

// applyTrancheSelection folds the tranche program into one locked selector
// sample. With a tranche in progress every other move waits, except the
// destination half's entry into a flat destination; without one, a
// persistent tranche replaces KEEP, or the worst-yielding held lane becomes
// the working lane. It returns the tranche to start, if any.
func applyTrancheSelection(in SelectorInput, result SelectorResult, previous map[string]AdvantageWindow, active *trancheIntent, generation int64, now time.Time) (SelectorResult, *trancheIntent) {
	s := in.Snapshot
	result.HeldLanes = s.HeldLanes
	keep := func(reason string) {
		result.Action, result.Reason, result.DestinationLane, result.SelectedQuote = "KEEP", reason, "", nil
		result.EquityRaw = s.TotalVaultNAVRaw
	}
	if active != nil {
		result.State.Tranches = previous
		switch {
		case !active.SourceFreed:
			keep("tranche_source_in_progress")
		case s.RouteLane == active.Destination && !hasWorkingCapital(s) && s.VoltrIdleRaw > 0:
			// The destination half enters the flat destination with the
			// reserved idle cash on a fresh exact quote; no persistence, the
			// tranche itself was the persistent decision.
			var chosen *MoveQuote
			for i := range in.Quotes {
				q := in.Quotes[i]
				if !q.Tranche && q.SourceLane == s.RouteLane && q.DestinationLane == active.Destination && q.ObservationID == s.ObservationID &&
					q.EquityRaw > 0 && q.EquityRaw <= min(active.EquityRaw, s.VoltrIdleRaw) && freshAt(in.Now, q.ObservedAt, in.Policy.QuoteMaxAge) &&
					q.currentAtSlot(s.Slot) && q.CostRaw >= 0 && q.CostRaw < q.EquityRaw && q.validBorrow() && (chosen == nil || q.EquityRaw > chosen.EquityRaw) {
					chosen = &q
				}
			}
			if chosen == nil {
				keep("tranche_destination_quote_unavailable")
				break
			}
			result.Action, result.Reason, result.DestinationLane, result.EquityRaw, result.SelectedQuote = "ENTER", "tranche_destination_entry", active.Destination, chosen.EquityRaw, chosen
		default:
			keep("tranche_destination_in_progress")
		}
		return result, nil
	}
	selection := selectTranche(in, previous)
	result.Tranches, result.State.Tranches = selection.Candidates, selection.Windows
	if result.Action != "KEEP" {
		return result, nil
	}
	if selection.Chosen != nil && selection.Quote != nil {
		c := selection.Chosen
		tranche := &trancheIntent{Source: c.Source, Destination: c.Destination, EquityRaw: c.EquityRaw, TargetIdleRaw: s.VoltrIdleRaw + c.EquityRaw,
			Generation: generation, ObservationID: s.ObservationID, EvidenceID: selection.Quote.EvidenceID, CreatedAt: now}
		if tranche.validate() != nil {
			result.Reason = "tranche_intent_invalid"
			return result, nil
		}
		result.Action, result.Reason, result.DestinationLane, result.EquityRaw, result.SelectedQuote = "TRANCHE", selection.Reason, c.Destination, c.EquityRaw, selection.Quote
		return result, tranche
	}
	if trancheSourceSettled(s) && len(s.otherHeldLanes()) > 0 {
		if worst := worstYieldHeldLane(s, in.heldFacts, in.Markets, in.Now, in.Policy); worst != "" {
			result.WorkingLane, result.Reason = worst, "working_lane_to_worst_yield"
		}
	}
	return result, nil
}
