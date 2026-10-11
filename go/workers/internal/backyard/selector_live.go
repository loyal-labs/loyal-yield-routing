package backyard

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// collectSelectorQuotes prices a source once, then at most three independent
// destinations, each with a biggest-first sizing ladder of at most three
// exact-size quotes. Nothing here writes a journal row or signs a transaction.
func collectSelectorQuotes(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, canaryMaximum ...uint64) ([]LaneEconomics, []MoveQuote, error) {
	return collectSelectorQuotesForLane(ctx, rpc, view, client, manifest, o, markets, policy, "", canaryMaximum...)
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
func collectSelectorQuotesForLane(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, onlyLane string, canaryMaximum ...uint64) ([]LaneEconomics, []MoveQuote, error) {
	return collectSelectorQuotesWithSource(ctx, rpc, view, client, manifest, o, markets, policy, onlyLane, nil, canaryMaximum...)
}

// selectorQuoteCapture is what one sample's quote collection priced beyond
// its whole-move quotes: the full-exit source quote, the policies it read,
// and every destination recipe its ladders priced. Tranche quotes are
// composed from these, so a tranche never prices a destination twice and
// never runs after the sample's own deadline.
type selectorQuoteCapture struct {
	source       selectorSourceQuote
	policies     installedPolicies
	mu           sync.Mutex
	destinations []selectorDestinationQuote
}

func (c *selectorQuoteCapture) addDestination(d selectorDestinationQuote) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.destinations = append(c.destinations, d)
}

// collectSelectorQuotesWithSource is collectSelectorQuotesForLane that also
// hands back the priced full-exit source quote, which tranche pricing scales
// to a partial exit instead of pricing the source a second time.
func collectSelectorQuotesWithSource(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, onlyLane string, capture *selectorQuoteCapture, canaryMaximum ...uint64) ([]LaneEconomics, []MoveQuote, error) {
	ctx, cancel := context.WithDeadline(ctx, o.ObservedAt.Add(8*time.Second))
	defer cancel()
	if err := policy.validate(); err != nil {
		return nil, nil, err
	}
	out := append([]LaneEconomics(nil), markets...)
	// Markets are the active registry lanes; any other market fails closed
	// with the set error of an unknown lane.
	seen := map[string]bool{}
	for _, market := range out {
		if !earnActiveLane(market.Lane) || seen[market.Lane] {
			return nil, nil, budgetHold("invalid_selector_market_set")
		}
		seen[market.Lane] = true
	}
	s := o.Snapshot
	if s.TotalVaultNAVRaw <= policy.IdleBufferRaw || !freshAt(time.Now().UTC(), o.ObservedAt, 30*time.Second) {
		return out, nil, budgetHold("selector_live_snapshot_unavailable")
	}
	if selectorTrancheInProgress(s) {
		return out, nil, budgetHold("complete_current_tranche_first")
	}
	// The policies every quote of this sample is priced through: one read,
	// at the sample's slot.
	policies, err := observeInstalledPolicies(ctx, rpc, s.Slot)
	if err != nil {
		return out, nil, err
	}
	o.policies = policies
	if capture != nil {
		capture.policies = policies
	}
	// A catalog lane's source quote (non-USDC debt) comes from the candidate
	// producer through the durable planning observation manifest: idle cash
	// prices the OBSERVED_IDLE_NO_EXIT source there, and funded capital prices
	// the same finite exit the basic lanes get, with its debt residue requoted
	// at the guaranteed funding remainder.
	observeSource := observeSelectorSource
	sourceManifest := manifest
	if catalogJupiterRoute(s.RouteLane) {
		if o.planning == nil {
			return out, nil, budgetHold("selector_source_unavailable")
		}
		sourceManifest = o.planning.observationManifest(manifest)
		observeSource = observeAutoSelectorSource
	}
	source, err := observeSource(ctx, rpc, view, client, sourceManifest, o)
	if err != nil {
		return out, nil, err
	}
	if capture != nil {
		capture.source = source
	}
	if source.MinimumIdleRaw <= uint64(policy.IdleBufferRaw) {
		return out, nil, budgetHold("selector_move_has_no_entry_cash")
	}
	maximum := min(strategyTwoBridgeLegCapRaw, uint64(s.TotalVaultNAVRaw-policy.IdleBufferRaw), source.MinimumIdleRaw-uint64(policy.IdleBufferRaw))
	if len(canaryMaximum) > 0 {
		maximum = min(maximum, canaryMaximum[0])
	}
	laddered := make([][]MoveQuote, len(out))
	var wg sync.WaitGroup
	for i := range out {
		market := out[i]
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
		if (sameLane && !sameLaneReinvestmentEligible(s, policy)) || market.validate(time.Now().UTC(), policy) != nil || market.EntryBlockedReason != "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			price := func(size uint64) (MoveQuote, uint64, string, bool, error) {
				// An eligible funded lane prices a forecast-only reentry against its
				// own full source exit binding; every other lane prices the ordinary
				// flat entry. Both yield the same complete move quote and neither
				// relaxes actual entry admission.
				var destination selectorDestinationQuote
				var err error
				if sameLane {
					destination, err = observeSelectorReentryDestinationSize(ctx, rpc, view, client, manifest, o, out[i], source, size, true)
				} else {
					destination, err = observeSelectorDestinationForecast(ctx, rpc, view, client, manifest, o.policies, out[i], size, s.Slot, true, nil)
				}
				if err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: selector entry quote unavailable lane=%s size=%d: %v\n", out[i].Lane, size, err)
					return MoveQuote{}, 0, "complete_entry_quote_unavailable", false, err
				}
				if !sameLane {
					capture.addDestination(destination)
				}
				loss := destinationEntrySwapLoss(destination.Recipe)
				q, err := composeSelectorMove(ctx, view, o, source, destination)
				if err != nil {
					// Cost beyond equity is an economic outcome smaller sizes can
					// repair; every other refusal is terminal for this lane.
					var hold *BudgetHold
					if errors.As(err, &hold) && hold.Reason == "selector_move_cost_exceeds_equity" {
						return MoveQuote{}, loss, "selector_move_cost_exceeds_equity", true, err
					}
					return MoveQuote{}, loss, "complete_move_quote_unavailable", false, err
				}
				return q, loss, "", false, nil
			}
			// Biggest-first sizing ladder: when the largest tranche cannot clear
			// the existing selector benefit under its expected expense, reprice
			// at most two smaller exact sizes. Every leg stays inside the caller's
			// existing collection deadline; no deadline is ever extended.
			var collected []MoveQuote
			// The largest size's refusal labels the lane when nothing collects:
			// economic failures keep probing smaller sizes, while network,
			// staleness and validation refusals stop the ladder immediately.
			refusal := ""
			sufficient, smallerUnavailable := false, false
			size := maximum
			for j := 0; j < selectorLadderRungs; j++ {
				if size == 0 || (j > 0 && (ctx.Err() != nil || time.Since(o.ObservedAt) > selectorLadderBudget)) {
					break
				}
				q, loss, unavailable, economic, err := price(size)
				if err != nil {
					if j == 0 {
						refusal = unavailable
					}
					if !economic {
						smallerUnavailable = j > 0
						break
					}
					size /= 10
					continue
				}
				collected = append(collected, q)
				if j < selectorLadderRungs-1 && selectorQuoteSizeSufficient(manifest, o, markets, out[i].Lane, policy, q) {
					sufficient = true
					break
				}
				size = selectorNextLadderSize(size, loss)
			}
			if collected, refusal = selectorLadderOutcome(collected, sufficient, smallerUnavailable, refusal); len(collected) > 0 {
				laddered[i] = collected
				return
			}
			out[i].EntryCapacity = Capacity{Known: true}
			out[i].EntryBlockedReason = refusal
		}(i)
	}
	wg.Wait()
	// Keep the best quote per lane; the locked pure selector owns cross-lane
	// competition and persistence. Profitable quotes are published as
	// executable capacity. Without any profitable size, the best bound-valid quote stays published
	// for evidence — pure SelectOpportunity then decides KEEP from its own math.
	result := make([]MoveQuote, 0, len(out))
	for i := range out {
		if len(laddered[i]) == 0 {
			continue
		}
		best, bestProfitable, bestBenefit := 0, false, math.Inf(-1)
		for j, q := range laddered[i] {
			benefit, admissible := selectorMoveQuoteBenefit(manifest, o, markets, out[i].Lane, policy, q)
			profitable := admissible && benefit > float64(policy.MinimumBenefitRaw)
			if j == 0 || profitable && !bestProfitable || profitable == bestProfitable && benefit > bestBenefit {
				best, bestProfitable, bestBenefit = j, profitable, benefit
			}
		}
		bestQuote := laddered[i][best]
		out[i].EntryCapacity = Capacity{Known: true, Raw: bestQuote.EquityRaw}
		result = append(result, bestQuote)
	}
	return out, result, nil
}

// selectorLadderRungs is how many exact sizes one lane's ladder prices.
const selectorLadderRungs = 3

// selectorNextLadderSize is the next, smaller ladder size: a tenth of the
// last one, unless the last rung's entry swap lost over a tenth of its size
// to the venue's depth. Then the next rung is 90% of the value the venue
// actually filled. Maple's reviewed venue (Manifest) fills about 33.6k
// syrupUSDC (~$40k): a $100k rung pays ~$61k of swap loss there, and a
// $10k rung would leave most of the fillable size unpriced.
func selectorNextLadderSize(size, entrySwapLoss uint64) uint64 {
	next := size / 10
	if entrySwapLoss <= size/10 || entrySwapLoss >= size {
		return next
	}
	if depth := (size - entrySwapLoss) / 10 * 9; depth > next {
		return depth
	}
	return next
}

// destinationEntrySwapLoss is the entry swap's bound loss in a destination
// recipe: the first swap that debits bridge USDC.
func destinationEntrySwapLoss(recipe selectorRecipe) uint64 {
	for _, cost := range recipe.Costs {
		if cost.Debit.Mint == bridgeUSDC && cost.ExecutionCost != nil && cost.ExecutionCost.SwapLossMicros > 0 {
			return uint64(cost.ExecutionCost.SwapLossMicros)
		}
	}
	return 0
}

// selectorLadderOutcome publishes a lane's collected rungs, except when none
// clears the benefit threshold and a smaller rung failed for availability:
// that smaller rung could have been the profitable one, so the lane is an
// availability gap (its persistence windows carry), never a larger rung's
// economic refusal that drops them.
func selectorLadderOutcome(collected []MoveQuote, sufficient, smallerUnavailable bool, refusal string) ([]MoveQuote, string) {
	if len(collected) > 0 && !sufficient && smallerUnavailable {
		return nil, "complete_entry_quote_unavailable"
	}
	return collected, refusal
}

// selectorQuoteSizeSufficient reports whether an already-collected quote
// clears the existing selector benefit under its expected expense, so no
// smaller ladder size is priced.
func selectorQuoteSizeSufficient(manifest RouteManifest, o Observation, markets []LaneEconomics, lane string, policy SelectorPolicy, q MoveQuote) bool {
	benefit, admissible := selectorMoveQuoteBenefit(manifest, o, markets, lane, policy, q)
	return admissible && benefit > float64(policy.MinimumBenefitRaw)
}

// selectorMoveQuoteBenefit reuses the pure SelectOpportunity evaluator on one
// exact quote rather than duplicating any financial formula. It reports the
// candidate's BenefitRaw and whether the quote is admissible at all.
func selectorMoveQuoteBenefit(manifest RouteManifest, o Observation, markets []LaneEconomics, lane string, policy SelectorPolicy, q MoveQuote) (float64, bool) {
	eval := append([]LaneEconomics(nil), markets...)
	for i := range eval {
		if eval[i].Lane == lane {
			eval[i].EntryCapacity = Capacity{Known: true, Raw: q.EquityRaw}
			eval[i].EntryBlockedReason = ""
		}
	}
	result := SelectOpportunity(SelectorInput{Now: time.Now().UTC(), Snapshot: o.Snapshot, Markets: eval, Quotes: []MoveQuote{q}, Policy: policy}, SelectorState{})
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
func (d *Database) evaluateSelector(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, markets []LaneEconomics, identity func(context.Context) (programIdentityObservation, error), policy SelectorPolicy) (SelectorResult, error) {
	result, _, err := d.evaluateSelectorObserved(ctx, rpc, view, manifest, markets, identity, policy)
	return result, err
}

// evaluateSelectorObserved also returns the observation the result was
// decided from, so the B2 leverage decision uses the same snapshot and
// planning generation.
func (d *Database) evaluateSelectorObserved(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, markets []LaneEconomics, identity func(context.Context) (programIdentityObservation, error), policy SelectorPolicy) (SelectorResult, Observation, error) {
	var version int64
	lease, err := d.currentLease()
	if err != nil {
		return SelectorResult{}, Observation{}, err
	}
	if lease.RouteKey != productionRouteKey {
		return SelectorResult{}, Observation{}, fmt.Errorf("selector_route_lease_mismatch")
	}
	if err = d.pool.QueryRow(ctx, `SELECT state_version FROM loyal_yield.multiply_route_states WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`, productionRouteKey, lease.Owner, lease.FencingToken).Scan(&version); err != nil {
		return SelectorResult{}, Observation{}, err
	}
	o, err := observeSelectorShadow(ctx, d, rpc, view, manifest, identity)
	if err != nil {
		return SelectorResult{}, Observation{}, err
	}
	if o.planning == nil || o.planning.generation != version {
		return SelectorResult{}, Observation{}, budgetHold("selector_state_changed_during_quote")
	}
	request, err := readPilotCanaryEntryRequest(time.Now().UTC())
	if err != nil {
		return SelectorResult{}, Observation{}, err
	}
	var maximum []uint64
	onlyLane := ""
	if request != nil {
		maximum = []uint64{uint64(request.EquityRaw)}
		onlyLane = request.Lane
	} else if t := o.planning.tranche; t != nil && t.SourceFreed && o.Snapshot.RouteLane == t.Destination && !hasWorkingCapital(o.Snapshot) && o.Snapshot.VoltrIdleRaw > 0 {
		// A tranche's destination half quotes only its flat destination, at
		// most the reserved E.
		maximum = []uint64{uint64(min(t.EquityRaw, o.Snapshot.VoltrIdleRaw))}
		onlyLane = t.Destination
	}
	capture := &selectorQuoteCapture{}
	enriched, quotes, quoteErr := collectSelectorQuotesWithSource(ctx, rpc, view, productionJupiter, manifest, o, markets, policy, onlyLane, capture, maximum...)
	var tranches trancheCollection
	switch {
	case onlyLane != "":
		tranches.SetReason = "tranche_collection_paused_for_single_lane_quote"
	case quoteErr != nil:
		// The full-exit source quote every tranche scales was not priced.
		tranches.SetReason = "selector_source_quote_unavailable"
		var hold *BudgetHold
		if errors.As(quoteErr, &hold) && selectorQuoteCollectionHoldCodes[hold.Reason] {
			tranches.SetReason = hold.Reason
		}
	default:
		priced := o
		priced.policies = capture.policies
		tranches = collectTrancheQuotes(ctx, rpc, view, productionJupiter, manifest, priced, markets, policy, capture)
		quotes = append(quotes, tranches.Quotes...)
	}
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
	slot, err := view.slot(ctx)
	if err != nil {
		return SelectorResult{}, Observation{}, err
	}
	result, err := d.RecordSelectorEvaluation(ctx, productionRouteKey, SelectorInput{Now: time.Now().UTC(), Snapshot: o.Snapshot, Markets: enriched, Quotes: quotes, Policy: policy, canaryRequest: request, trancheRefusals: tranches.Refusals, trancheSetReason: tranches.SetReason, heldFacts: o.heldFacts}, slot, version)
	return result, o, err
}
