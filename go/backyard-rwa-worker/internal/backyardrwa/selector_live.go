package backyardrwa

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
)

// collectSelectorQuotes prices a source once, then at most three independent
// destinations, each with a biggest-first sizing ladder of at most three
// exact-size quotes. Nothing here writes a journal row or signs a transaction.
// entryCostRemainingRaw is advisory sizing headroom under the bounded
// execution-cost stop; negative means unknown and skips that trigger.
func collectSelectorQuotes(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, entryCostRemainingRaw int64, canaryMaximum ...uint64) ([]LaneEconomics, []MoveQuote, error) {
	return collectSelectorQuotesForLane(ctx, rpc, client, manifest, o, markets, policy, entryCostRemainingRaw, "", canaryMaximum...)
}

// selectorLadderBudget stops pricing smaller ladder sizes once this much of
// the quote's 32-slot (~9-13 s) window has passed since the observation; the
// largest size is always priced. Every lane waits for the slowest ladder, and
// the AUTO ladder alone took ~6 s of a ~9 s window on 2026-09-25, so accepted
// entries reached the worker with too few slots left to allocate.
const selectorLadderBudget = 3 * time.Second

// collectSelectorQuotesForLane prices only onlyLane when it is set: while an
// operator canary is installed the pure selector's economic choice is always
// suppressed, so other lanes' destination quotes are never used and only
// delay the canary's own quote. Those lanes keep their market economics and
// are published as blocked without a quote.
func collectSelectorQuotesForLane(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, entryCostRemainingRaw int64, onlyLane string, canaryMaximum ...uint64) ([]LaneEconomics, []MoveQuote, error) {
	ctx, cancel := context.WithDeadline(ctx, o.ObservedAt.Add(8*time.Second))
	defer cancel()
	if err := policy.validate(); err != nil {
		return nil, nil, err
	}
	out := append([]LaneEconomics(nil), markets...)
	// The market-set lane authority is the reviewed manifest's: installed
	// selector lanes always, the candidate AUTO lane only while this manifest
	// itself carries the fully validated autoPolicy binding. An AUTO market
	// under an absent or drifted binding fails closed with the same set error
	// as any unknown lane.
	laneAllowed := func(lane string) bool { return selectorDestinationLaneAuthorized(manifest, lane) }
	seen := map[string]bool{}
	for _, market := range out {
		if !laneAllowed(market.Lane) || seen[market.Lane] {
			return nil, nil, budgetHold("invalid_selector_market_set")
		}
		seen[market.Lane] = true
	}
	s := o.Snapshot
	if !s.PilotActive || s.TotalVaultNAVRaw <= policy.IdleBufferRaw || !freshAt(time.Now().UTC(), o.ObservedAt, 30*time.Second) {
		return out, nil, budgetHold("selector_live_snapshot_unavailable")
	}
	if selectorTrancheInProgress(s) {
		return out, nil, budgetHold("complete_current_tranche_first")
	}
	// An AUTO route's source quote always comes from the candidate producer
	// through the durable planning observation manifest: idle cash prices the
	// OBSERVED_IDLE_NO_EXIT source there, and funded capital prices the same
	// reviewed finite exit the installed lanes get — the candidate producer is
	// the only one whose lane authority admits the AUTO route lane. The reviewed
	// gates bound to this manifest's autoPolicy and active lane are unchanged;
	// an absent or drifted binding fails closed, and installed lanes keep the
	// reviewed producer.
	observeSource := observeSelectorSource
	sourceManifest := manifest
	if s.RouteLane == autoAUTOPYUSD.Lane {
		if o.planning == nil {
			return out, nil, budgetHold("selector_source_unavailable")
		}
		sourceManifest = o.planning.observationManifest(manifest)
		observeSource = observeAutoSelectorSource
	}
	source, err := observeSource(ctx, rpc, client, sourceManifest, o)
	if err != nil {
		return out, nil, err
	}
	if source.MinimumIdleRaw <= uint64(policy.IdleBufferRaw) {
		return out, nil, budgetHold("selector_move_has_no_entry_cash")
	}
	maximum := min(uint64(PilotWorkingTrancheCapRaw), uint64(s.TotalVaultNAVRaw-policy.IdleBufferRaw), source.MinimumIdleRaw-uint64(policy.IdleBufferRaw))
	if len(canaryMaximum) > 0 {
		maximum = min(maximum, canaryMaximum[0])
	}
	laddered := make([][]MoveQuote, len(out))
	var wg sync.WaitGroup
	for i := range out {
		market := out[i]
		// Deferred rollout lanes keep their economics observable for KEEP
		// baselines and valuation, but receive no executable destination quote,
		// so no ENTER, SWITCH, or canary can select them. The candidate AUTO
		// lane is quoted exactly when this manifest's reviewed binding admits
		// its funded allocation — the same authority the funding fence below
		// enforces; absent or drifted bindings keep it deferred.
		if !manifest.selectorEntryFundingLane(market.Lane, false) {
			if market.EntryBlockedReason == "" {
				out[i].EntryCapacity = Capacity{Known: true}
				out[i].EntryBlockedReason = "lane_entry_deferred"
			}
			continue
		}
		if onlyLane != "" && market.Lane != onlyLane {
			if market.EntryBlockedReason == "" {
				out[i].EntryCapacity = Capacity{Known: true}
				out[i].EntryBlockedReason = "operator_canary_lane_only"
			}
			continue
		}
		// Current deployed capital is the KEEP baseline, never close/reopen merely
		// to quote the same destination — unless a strictly larger same-lane
		// reinvestment is eligible. Idle ownership can enter that lane normally.
		sameLane := hasWorkingCapital(s) && market.Lane == s.RouteLane
		fundingAllowed := func(l string) bool { return manifest.selectorEntryFundingLane(l, false) }
		if (sameLane && !sameLaneReinvestmentEligibleWithLane(s, policy, fundingAllowed)) || market.validateWithLane(time.Now().UTC(), policy, laneAllowed) != nil || market.EntryBlockedReason != "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			price := func(size uint64) (MoveQuote, string, bool, error) {
				// An eligible funded lane prices a forecast-only reentry against its
				// own full source exit binding; every other lane prices the ordinary
				// flat entry. Both yield the same complete move quote and neither
				// relaxes actual entry admission.
				var destination selectorDestinationQuote
				var err error
				if sameLane {
					destination, err = observeSelectorReentryDestinationSize(ctx, rpc, client, manifest, o, source, size, true)
				} else {
					// The lane authority was resolved above; the authorized form
					// prices the identical entry graph while the candidate lane
					// goes through the same checks the public gate applies to
					// installed lanes.
					destination, err = observeSelectorDestinationForecastAuthorized(ctx, rpc, client, manifest, out[i].Lane, size, s.Slot, true, nil)
				}
				if err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: selector entry quote unavailable lane=%s size=%d: %v\n", out[i].Lane, size, err)
					return MoveQuote{}, "complete_entry_quote_unavailable", false, err
				}
				q, err := composeSelectorMoveWithLane(ctx, rpc, o, source, destination, laneAllowed)
				if err != nil {
					// Cost beyond equity is an economic outcome smaller sizes can
					// repair; every other refusal is terminal for this lane.
					var hold *BudgetHold
					if errors.As(err, &hold) && hold.Reason == "selector_move_cost_exceeds_equity" {
						return MoveQuote{}, "selector_move_cost_exceeds_equity", true, err
					}
					return MoveQuote{}, "complete_move_quote_unavailable", false, err
				}
				return q, "", false, nil
			}
			// Biggest-first sizing ladder: when the largest tranche cannot clear
			// the existing selector benefit under its expected expense, or would
			// exhaust the remaining bounded entry-cost budget, reprice at most
			// two smaller exact sizes. Every leg stays inside the caller's
			// existing collection deadline; no deadline is ever extended.
			sizes := [3]uint64{maximum, maximum / 10, maximum / 100}
			var collected []MoveQuote
			// The largest size's refusal labels the lane when nothing collects:
			// economic failures keep probing smaller sizes, while network,
			// staleness and validation refusals stop the ladder immediately.
			refusal := ""
			for j, size := range sizes {
				if size == 0 || (j > 0 && (ctx.Err() != nil || time.Since(o.ObservedAt) > selectorLadderBudget)) {
					break
				}
				q, unavailable, economic, err := price(size)
				if err != nil {
					if j == 0 {
						refusal = unavailable
					}
					if !economic {
						break
					}
					continue
				}
				collected = append(collected, q)
				if j < len(sizes)-1 && selectorQuoteSizeSufficient(manifest, o, markets, out[i].Lane, policy, entryCostRemainingRaw, q) {
					break
				}
			}
			if len(collected) > 0 {
				laddered[i] = collected
				return
			}
			out[i].EntryCapacity = Capacity{Known: true}
			out[i].EntryBlockedReason = refusal
		}(i)
	}
	wg.Wait()
	// Keep the best quote per lane; the locked pure selector owns cross-lane
	// competition and persistence. Profitable, budget-fitting quotes are
	// published as executable capacity. A positive-benefit quote beyond the
	// remaining bounded entry-cost budget is retained only as diagnostics with
	// an explicit block, so pure selection can never attempt its admission.
	// Without any profitable size, the best bound-valid quote stays published
	// for evidence — pure SelectOpportunity then decides KEEP from its own math.
	result := make([]MoveQuote, 0, len(out))
	for i := range out {
		if len(laddered[i]) == 0 {
			continue
		}
		best, bestProfitable, bestBenefit := 0, false, math.Inf(-1)
		for j, q := range laddered[i] {
			benefit, admissible := selectorMoveQuoteBenefit(manifest, o, markets, out[i].Lane, policy, q)
			withinBudget := entryCostRemainingRaw < 0 || q.CostRaw <= entryCostRemainingRaw
			profitable := admissible && withinBudget && benefit > float64(policy.MinimumBenefitRaw)
			if j == 0 || profitable && !bestProfitable || profitable == bestProfitable && benefit > bestBenefit {
				best, bestProfitable, bestBenefit = j, profitable, benefit
			}
		}
		bestQuote := laddered[i][best]
		out[i].EntryCapacity = Capacity{Known: true, Raw: bestQuote.EquityRaw}
		if !bestProfitable && entryCostRemainingRaw >= 0 && bestQuote.CostRaw > entryCostRemainingRaw {
			out[i].EntryBlockedReason = "execution_cost_budget_exhausted"
		}
		result = append(result, bestQuote)
	}
	return out, result, nil
}

// selectorQuoteSizeSufficient reports whether an already-collected quote
// clears the existing selector benefit under its expected expense inside the
// remaining bounded entry-cost budget, so no smaller ladder size is priced.
func selectorQuoteSizeSufficient(manifest RouteManifest, o Observation, markets []LaneEconomics, lane string, policy SelectorPolicy, entryCostRemainingRaw int64, q MoveQuote) bool {
	if entryCostRemainingRaw >= 0 && q.CostRaw > entryCostRemainingRaw {
		return false
	}
	benefit, admissible := selectorMoveQuoteBenefit(manifest, o, markets, lane, policy, q)
	return admissible && benefit > float64(policy.MinimumBenefitRaw)
}

// selectorMoveQuoteBenefit reuses the pure SelectOpportunity evaluator on one
// exact quote rather than duplicating any financial formula. It reports the
// candidate's BenefitRaw and whether the quote is admissible at all. The lane
// authority is the caller manifest's — the same reviewed binding that priced
// the quote — so a candidate lane's exact quote is evaluated, never silently
// dropped, while installed behavior is unchanged.
func selectorMoveQuoteBenefit(manifest RouteManifest, o Observation, markets []LaneEconomics, lane string, policy SelectorPolicy, q MoveQuote) (float64, bool) {
	eval := append([]LaneEconomics(nil), markets...)
	for i := range eval {
		if eval[i].Lane == lane {
			eval[i].EntryCapacity = Capacity{Known: true, Raw: q.EquityRaw}
			eval[i].EntryBlockedReason = ""
		}
	}
	laneAllowed := func(l string) bool { return selectorDestinationLaneAuthorized(manifest, l) }
	fundingAllowed := func(l string) bool { return manifest.selectorEntryFundingLane(l, false) }
	result := selectOpportunityWithLanes(SelectorInput{Now: time.Now().UTC(), Snapshot: o.Snapshot, Markets: eval, Quotes: []MoveQuote{q}, Policy: policy}, SelectorState{}, laneAllowed, fundingAllowed)
	for _, c := range result.Candidates {
		if c.Lane == lane && c.CostsKnown {
			return c.BenefitRaw, c.BlockedReason == ""
		}
	}
	return math.Inf(-1), false
}

// selectorQuoteCollectionHoldCodes is the closed set of BudgetHold reasons
// collectSelectorQuotes itself raises before any destination pricing. Only
// these codes survive into the no-quote lane diagnostic; deeper
// source-producer, transport and database failures collapse to the generic
// selector_source_quote_unavailable code, so nothing arbitrary ever reaches
// the persisted lane evidence.
var selectorQuoteCollectionHoldCodes = map[string]bool{
	"complete_current_tranche_first":     true,
	"invalid_selector_market_set":        true,
	"selector_live_snapshot_unavailable": true,
	"selector_move_has_no_entry_cash":    true,
	"selector_source_unavailable":        true,
}

// Capture the generation before observing accounts. Concurrent execution or
// budget changes invalidate collection in RecordSelectorEvaluation's lock.
func (d *Database) evaluateSelector(ctx context.Context, rpc *RPCClient, manifest RouteManifest, markets []LaneEconomics, identity func(context.Context) (programIdentityObservation, error), policy SelectorPolicy) (SelectorResult, error) {
	var version int64
	lease, err := d.currentLease()
	if err != nil {
		return SelectorResult{}, err
	}
	if lease.RouteKey != productionRouteKey {
		return SelectorResult{}, fmt.Errorf("selector_route_lease_mismatch")
	}
	if err = d.pool.QueryRow(ctx, `SELECT state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`, productionRouteKey, lease.Owner, lease.FencingToken).Scan(&version); err != nil {
		return SelectorResult{}, err
	}
	o, err := observeSelectorShadow(ctx, d, rpc, manifest, identity)
	if err != nil {
		return SelectorResult{}, err
	}
	if o.planning == nil || o.planning.generation != version {
		return SelectorResult{}, budgetHold("selector_state_changed_during_quote")
	}
	if !o.Snapshot.PilotActive {
		return SelectorResult{}, budgetHold("selector_requires_active_pilot")
	}
	request, err := readPilotCanaryEntryRequestOnManifest(time.Now().UTC(), manifest)
	if err != nil {
		return SelectorResult{}, err
	}
	var maximum []uint64
	onlyLane := ""
	if request != nil {
		maximum = []uint64{uint64(request.EquityRaw)}
		onlyLane = request.Lane
	}
	// Advisory sizing headroom from the same planning snapshot; the binding
	// bounded-cost stop still runs at reservation time under the record lock.
	remaining := int64(PilotEntryExecutionCostCapMicros)
	if o.planning != nil {
		remaining = o.planning.remainingExecutionCost
	}
	enriched, quotes, quoteErr := collectSelectorQuotesForLane(ctx, rpc, productionJupiterClient(), manifest, o, markets, policy, remaining, onlyLane, maximum...)
	if quoteErr != nil {
		// No fabricated executable capacity on an outage. Current economic evidence
		// can still maintain persistence, while pure selection cannot enter/switch.
		// Each lane keeps one sanitized refusal code: the collector's own closed
		// gate codes echo, and every deeper producer, transport or database
		// failure collapses to the generic source-quote code — never the raw
		// error text.
		enriched, quotes = append([]LaneEconomics(nil), markets...), nil
		reason := "selector_source_quote_unavailable"
		var hold *BudgetHold
		if errors.As(quoteErr, &hold) && selectorQuoteCollectionHoldCodes[hold.Reason] {
			reason = hold.Reason
		}
		for i := range enriched {
			enriched[i].EntryCapacity = Capacity{}
			enriched[i].EntryBlockedReason = reason
		}
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return SelectorResult{}, err
	}
	// The locked persistence resolves its market lane authority through the
	// same reviewed manifest that priced these quotes, so a candidate lane's
	// profitable quote can be recorded as an entry rather than silently
	// dropped at the installed embedded-lane gate.
	return d.recordSelectorEvaluationWithLanes(ctx, productionRouteKey, &manifest, SelectorInput{Now: time.Now().UTC(), Snapshot: o.Snapshot, Markets: enriched, Quotes: quotes, Policy: policy, canaryRequest: request}, slot, version)
}
