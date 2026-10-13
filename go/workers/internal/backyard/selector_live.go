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

// selectorLadderBudget stops pricing smaller ladder sizes once this much of
// the quote's 32-slot (~9-13 s) window has passed since the observation; the
// largest size is always priced. Every lane waits for the slowest ladder, and
// the AUTO ladder alone took ~6 s of a ~9 s window on 2026-09-25, so accepted
// entries reached the worker with too few slots left to allocate.
const selectorLadderBudget = 3 * time.Second

// collectSelectorQuotes prices a flat vault's entry into each candidate
// lane (priceEntry), at most selectorLadderRungs exact sizes per lane. It
// prices only onlyLane when it is set: while an operator canary is installed
// the pure selector's economic choice is always suppressed, so other lanes'
// quotes are never used and only delay the canary's own quote. Those lanes
// keep their market economics and are published as blocked without a quote.
// Only a flat vault is quoted: held lanes keep their capital.
func collectSelectorQuotes(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, onlyLane string, canaryMaximum ...uint64) ([]LaneEconomics, []MoveQuote, error) {
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
	if s.TotalVaultNAVRaw <= policy.IdleBufferRaw {
		return out, nil, budgetHold("selector_live_snapshot_unavailable")
	}
	if laneExposure(s) {
		return out, nil, budgetHold("held_lanes_keep_their_capital")
	}
	// The entry's cash is the flat vault's: Voltr idle and the Squads cash an
	// entry already allocated, on a settled book.
	if !s.Fresh || s.ObservationID == "" || s.Slot <= 0 || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.ManualReason != "" ||
		s.WithdrawalDemandRaw != 0 || s.VoltrStrategyIdleRaw != 0 || s.VoltrIdleRaw < 0 || s.SquadsIdleRaw < 0 {
		return out, nil, budgetHold("selector_source_unavailable")
	}
	idle := uint64(s.VoltrIdleRaw) + uint64(s.SquadsIdleRaw)
	if idle <= uint64(policy.IdleBufferRaw) {
		return out, nil, budgetHold("selector_move_has_no_entry_cash")
	}
	// The policies and the SOL price every quote of this sample is priced
	// through: one read each, at the sample's slot.
	pricer, err := newLanePricer(ctx, rpc, view, client, manifest, s.Slot)
	if err != nil {
		return out, nil, err
	}
	maximum := min(strategyTwoBridgeLegCapRaw, uint64(s.TotalVaultNAVRaw-policy.IdleBufferRaw), idle-uint64(policy.IdleBufferRaw))
	if len(canaryMaximum) > 0 {
		maximum = min(maximum, canaryMaximum[0])
	}
	laddered := make([][]MoveQuote, len(out))
	var wg sync.WaitGroup
	for i := range out {
		market := out[i]
		if !selectorDestinationLane(market.Lane) {
			out[i].EntryCapacity = Capacity{Known: true}
			out[i].EntryBlockedReason = overflowLaneBlockedReason
			continue
		}
		if onlyLane != "" && market.Lane != onlyLane {
			if market.EntryBlockedReason == "" {
				out[i].EntryCapacity = Capacity{Known: true}
				out[i].EntryBlockedReason = "operator_canary_lane_only"
			}
			continue
		}
		if market.validate(time.Now().UTC(), policy) != nil || market.EntryBlockedReason != "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			price := func(size uint64) (MoveQuote, string, bool, error) {
				q, err := pricer.priceEntry(ctx, o, out[i], size)
				if err == nil {
					return q, "", false, nil
				}
				_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: selector entry quote unavailable lane=%s size=%d: %v\n", out[i].Lane, size, err)
				// Cost beyond equity is an economic outcome smaller sizes can
				// repair; every other refusal is terminal for this lane.
				var hold *BudgetHold
				if errors.As(err, &hold) && holdCause(hold) == "selector_move_cost_exceeds_equity" {
					return q, "selector_move_cost_exceeds_equity", true, err
				}
				return q, "complete_entry_quote_unavailable", false, err
			}
			// Biggest-first sizing ladder: when the largest size cannot clear
			// the selector benefit under its expected cost, price at most two
			// smaller exact sizes, inside the collection deadline.
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
				q, unavailable, economic, err := price(size)
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
				if j < selectorLadderRungs-1 && selectorQuoteSizeSufficient(o, markets, out[i].Lane, policy, q) {
					sufficient = true
					break
				}
				// The quote may be clamped below size (deposit room): the next
				// rung steps down from what was actually priced.
				size = selectorNextLadderSize(min(size, uint64(q.EquityRaw)), uint64(max(q.Cost.EntrySwapLossMicros, 0)))
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
	// Keep the best quote per lane; the pure selector owns cross-lane
	// competition and persistence. Without any profitable size, the best
	// valid quote stays published for evidence: SelectOpportunity then
	// decides KEEP from its own math.
	result := make([]MoveQuote, 0, len(out))
	for i := range out {
		if len(laddered[i]) == 0 {
			continue
		}
		best, bestProfitable, bestBenefit := 0, false, math.Inf(-1)
		for j, q := range laddered[i] {
			benefit, admissible := selectorMoveQuoteBenefit(o, markets, out[i].Lane, policy, q)
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
func selectorQuoteSizeSufficient(o Observation, markets []LaneEconomics, lane string, policy SelectorPolicy, q MoveQuote) bool {
	benefit, admissible := selectorMoveQuoteBenefit(o, markets, lane, policy, q)
	return admissible && benefit > float64(policy.MinimumBenefitRaw)
}

// selectorMoveQuoteBenefit reuses the pure SelectOpportunity evaluator on one
// exact quote rather than duplicating any financial formula. It reports the
// candidate's BenefitRaw and whether the quote is admissible at all.
func selectorMoveQuoteBenefit(o Observation, markets []LaneEconomics, lane string, policy SelectorPolicy, q MoveQuote) (float64, bool) {
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
	"held_lanes_keep_their_capital":      true,
	"invalid_selector_market_set":        true,
	"selector_live_snapshot_unavailable": true,
	"selector_move_has_no_entry_cash":    true,
	"selector_source_unavailable":        true,
}

func (d *Database) evaluateSelector(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, markets []LaneEconomics, identity func(context.Context) (programIdentityObservation, error), policy SelectorPolicy) (SelectorResult, error) {
	result, _, err := d.evaluateSelectorObserved(ctx, rpc, view, manifest, markets, identity, policy)
	return result, err
}

// evaluateSelectorObserved also returns the observation the result was
// decided from, so the B2 leverage decision uses the same snapshot and
// planning inputs.
func (d *Database) evaluateSelectorObserved(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, markets []LaneEconomics, identity func(context.Context) (programIdentityObservation, error), policy SelectorPolicy) (SelectorResult, Observation, error) {
	o, err := observeSelectorShadow(ctx, d, rpc, view, manifest, identity)
	if err != nil {
		return SelectorResult{}, Observation{}, err
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
	}
	enriched, quotes, quoteErr := collectSelectorQuotes(ctx, rpc, view, productionJupiter, manifest, o, markets, policy, onlyLane, maximum...)
	if quoteErr != nil {
		// No fabricated executable capacity on an outage. Current economic evidence
		// can still maintain persistence, while pure selection cannot enter.
		// Each lane keeps one sanitized refusal code: the collector's own closed
		// gate codes echo, and every deeper producer, transport or database
		// failure collapses to the generic source-quote code — never the raw
		// error text.
		enriched, quotes = append([]LaneEconomics(nil), markets...), nil
		reason := "selector_source_quote_unavailable"
		var hold *BudgetHold
		if errors.As(quoteErr, &hold) && selectorQuoteCollectionHoldCodes[holdCause(hold)] {
			reason = holdCause(hold)
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
	result, err := d.RecordSelectorEvaluation(ctx, productionRouteKey, SelectorInput{Now: time.Now().UTC(), Snapshot: o.Snapshot, Markets: enriched, Quotes: quotes, Policy: policy, canaryRequest: request}, slot)
	return result, o, err
}
