package backyard

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
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

// collectSelectorQuotes prices, at most selectorLadderRungs exact sizes per
// move, every move SelectOpportunity scores: cash's entry into each candidate
// lane, a held lane included (priceEntry), and each held lane's rotation of a
// tranche into another lane whose forward APY beats it (priceMove). It
// prices only onlyLane when it is set, and only onlySource's rotation into
// it when that is set too: while an operator canary is installed the pure
// selector's economic choice is always suppressed, so other moves' quotes
// are never used and only delay the canary's own quote. Those lanes keep
// their market economics and are published as blocked without a quote.
func collectSelectorQuotes(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, onlyLane, onlySource string, canaryMaximum ...uint64) ([]LaneEconomics, []MoveQuote, error) {
	ctx, cancel := context.WithDeadline(ctx, o.ObservedAt.Add(8*time.Second))
	defer cancel()
	if err := policy.validate(); err != nil {
		return nil, nil, err
	}
	out := append([]LaneEconomics(nil), markets...)
	// Markets are the active registry lanes; any other market fails closed
	// with the set error of an unknown lane.
	seen := map[string]LaneEconomics{}
	for _, market := range out {
		if _, dup := seen[market.Lane]; !earnActiveLane(market.Lane) || dup {
			return nil, nil, budgetHold("invalid_selector_market_set")
		}
		seen[market.Lane] = market
	}
	s := o.Snapshot
	if s.TotalVaultNAVRaw <= policy.IdleBufferRaw {
		return out, nil, budgetHold("selector_live_snapshot_unavailable")
	}
	// Moves are priced on a settled book: Voltr idle beyond the withdrawal
	// queue and the Squads cash an entry already allocated, no shortfall.
	if !s.Fresh || s.ObservationID == "" || s.Slot <= 0 || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.ManualReason != "" ||
		withdrawalShortfallRaw(s) != 0 || s.VoltrStrategyIdleRaw != 0 || s.WithdrawalDemandRaw < 0 || s.VoltrIdleRaw < 0 || s.SquadsIdleRaw < 0 {
		return out, nil, budgetHold("selector_source_unavailable")
	}
	idle := uint64(s.VoltrIdleRaw-s.WithdrawalDemandRaw) + uint64(s.SquadsIdleRaw)
	// Without cash beyond the buffer no lane is entered with cash; a
	// rotation canary prices its own pair only.
	cashBlocked := ""
	if idle <= uint64(policy.IdleBufferRaw) {
		cashBlocked = "selector_move_has_no_entry_cash"
	} else if onlySource != "" {
		cashBlocked = "operator_canary_rotation_only"
	}
	// The policies and the SOL price every quote of this sample is priced
	// through: one read each, at the sample's slot.
	pricer, err := newLanePricer(ctx, rpc, view, client, manifest, s.Slot)
	if err != nil {
		return out, nil, err
	}
	canary := func(maximum uint64) uint64 {
		if len(canaryMaximum) > 0 {
			return min(maximum, canaryMaximum[0])
		}
		return maximum
	}
	type job struct {
		i       int
		source  *Snapshot
		maximum uint64
	}
	var jobs []job
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
		if cashBlocked != "" {
			out[i].EntryCapacity = Capacity{Known: true}
			out[i].EntryBlockedReason = cashBlocked
		} else {
			jobs = append(jobs, job{i: i, maximum: canary(min(strategyTwoBridgeLegCapRaw, uint64(s.TotalVaultNAVRaw-policy.IdleBufferRaw), idle-uint64(policy.IdleBufferRaw)))})
		}
		// Each held lane's tranche, released, enters this lane when it is
		// the source's best destination by forward APY (a canary prices
		// its pair anyway).
		held := s.heldLanes()
		for k := range o.lanes {
			source := o.lanes[k]
			if !rotationSource(held, source) || !rotationPair(source.RouteLane, market.Lane) || (onlySource != "" && source.RouteLane != onlySource) ||
				onlySource == "" && rotationDestination(source, seen, time.Now().UTC(), policy) != market.Lane {
				continue
			}
			jobs = append(jobs, job{i: i, source: &source, maximum: canary(min(strategyTwoBridgeLegCapRaw, uint64(laneReleaseLimitRaw(source))))})
		}
	}
	laddered := make([][]MoveQuote, len(jobs))
	refusals := make([]string, len(jobs))
	var wg sync.WaitGroup
	for j := range jobs {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			laddered[j], refusals[j] = pricer.ladder(ctx, o, markets, policy, out[jobs[j].i], jobs[j].source, jobs[j].maximum, len(canaryMaximum) > 0)
		}(j)
	}
	wg.Wait()
	// Keep the best quote per move; the pure selector owns cross-move
	// competition and persistence. Without any profitable size, the best
	// valid quote stays published for evidence: SelectOpportunity then
	// decides KEEP from its own math.
	result := make([]MoveQuote, 0, len(jobs))
	for j, job := range jobs {
		if len(laddered[j]) == 0 {
			if job.source == nil {
				out[job.i].EntryCapacity = Capacity{Known: true}
				out[job.i].EntryBlockedReason = refusals[j]
			}
			continue
		}
		best, bestProfitable, bestBenefit := 0, false, math.Inf(-1)
		for k, q := range laddered[j] {
			benefit, admissible := selectorMoveQuoteBenefit(o, markets, policy, q)
			profitable := admissible && benefit > float64(policy.MinimumBenefitRaw)
			if k == 0 || profitable && !bestProfitable || profitable == bestProfitable && benefit > bestBenefit {
				best, bestProfitable, bestBenefit = k, profitable, benefit
			}
		}
		bestQuote := laddered[j][best]
		if job.source == nil {
			out[job.i].EntryCapacity = Capacity{Known: true, Raw: bestQuote.EquityRaw}
		}
		result = append(result, bestQuote)
	}
	return out, result, nil
}

// ladder prices one move into market's lane, of cash or of source's
// tranche, at up to selectorLadderRungs exact sizes from maximum down, and
// returns the collected quotes, or the refusal that labels the move when
// none collects. An operator canary skips the benefit check, so its sizes
// are quoted whatever their gain ceiling.
func (p lanePricer) ladder(ctx context.Context, o Observation, markets []LaneEconomics, policy SelectorPolicy, market LaneEconomics, source *Snapshot, maximum uint64, canary bool) ([]MoveQuote, string) {
	from := "cash"
	if source != nil {
		from = source.RouteLane
	}
	var ms LaneEconomics
	for _, m := range markets {
		if source != nil && m.Lane == source.RouteLane {
			ms = m
		}
	}
	// A rotation's source is settled once, on the first size priced: what
	// staying earns does not depend on the tranche.
	var keep *heldSettlement
	price := func(size uint64) (MoveQuote, string, bool, error) {
		var err error
		if source != nil && keep == nil {
			var settled heldSettlement
			if settled, err = p.settleHeld(ctx, o, *source, ms); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: selector source settlement unavailable from=%s lane=%s: %v\n", from, market.Lane, err)
				return MoveQuote{}, "complete_entry_quote_unavailable", false, err
			}
			keep = &settled
		}
		q, err := p.priceMove(ctx, o, source, keep, market, size)
		if err == nil {
			return q, "", false, nil
		}
		_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: selector entry quote unavailable from=%s lane=%s size=%d: %v\n", from, market.Lane, size, err)
		// A smaller size may repair a cost beyond equity or a step quote
		// the leveraged route cannot fill at this size (exit_quote at the
		// 90% tranche, Oct 14); Jupiter's rate limit and every other
		// refusal stop the move.
		var hold *BudgetHold
		if errors.As(err, &hold) && holdCause(hold) == "selector_move_cost_exceeds_equity" {
			return q, "selector_move_cost_exceeds_equity", true, err
		}
		var status *jupiter.StatusError
		var outage *leverageCostOutage
		sized := errors.As(err, &outage) && (outage.stage == "entry_quote" || outage.stage == "exit_quote") &&
			!(errors.As(err, &status) && status.Code == http.StatusTooManyRequests)
		return q, "complete_entry_quote_unavailable", sized, err
	}
	// Biggest-first sizing ladder: when the largest size cannot clear the
	// selector benefit under its expected cost, price at most two smaller
	// exact sizes, inside the collection deadline.
	var collected []MoveQuote
	// The largest size's refusal labels the move when nothing collects:
	// economic failures keep probing smaller sizes, while network,
	// staleness and validation refusals stop the ladder immediately.
	refusal := ""
	sufficient, smallerUnavailable := false, false
	size := maximum
	for j := 0; j < selectorLadderRungs; j++ {
		if size == 0 || (j > 0 && (ctx.Err() != nil || time.Since(o.ObservedAt) > selectorLadderBudget)) {
			break
		}
		// A size whose gain ceiling cannot clear MinimumBenefit is not
		// quoted, nor is any smaller one.
		if !canary && moveGainCeiling(source, ms, market, int64(min(size, math.MaxInt64)), policy) <= float64(policy.MinimumBenefitRaw) {
			if j == 0 {
				refusal = gainBelowMinimumBenefit
			}
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
		if j < selectorLadderRungs-1 && selectorQuoteSizeSufficient(o, markets, policy, q) {
			sufficient = true
			break
		}
		// The quote may be clamped below size (deposit room): the next
		// rung steps down from what was actually priced.
		size = selectorNextLadderSize(min(size, uint64(q.EquityRaw)), uint64(max(q.Cost.EntrySwapLossMicros, 0)))
	}
	return selectorLadderOutcome(collected, sufficient, smallerUnavailable, refusal)
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
func selectorQuoteSizeSufficient(o Observation, markets []LaneEconomics, policy SelectorPolicy, q MoveQuote) bool {
	benefit, admissible := selectorMoveQuoteBenefit(o, markets, policy, q)
	return admissible && benefit > float64(policy.MinimumBenefitRaw)
}

// selectorMoveQuoteBenefit reuses the pure SelectOpportunity evaluator on one
// exact quote rather than duplicating any financial formula. It reports the
// candidate's BenefitRaw and whether the quote is admissible at all.
func selectorMoveQuoteBenefit(o Observation, markets []LaneEconomics, policy SelectorPolicy, q MoveQuote) (float64, bool) {
	eval := append([]LaneEconomics(nil), markets...)
	for i := range eval {
		if eval[i].Lane == q.DestinationLane {
			eval[i].EntryCapacity = Capacity{Known: true, Raw: q.EquityRaw}
			eval[i].EntryBlockedReason = ""
		}
	}
	result := SelectOpportunity(SelectorInput{Now: time.Now().UTC(), Snapshot: o.Snapshot, Lanes: o.lanes, Markets: eval, Quotes: []MoveQuote{q}, Policy: policy}, SelectorState{})
	for _, c := range result.Candidates {
		if c.Lane == q.DestinationLane && c.SourceLane == q.RotateFrom && c.CostsKnown {
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
	"invalid_selector_market_set":        true,
	"selector_live_snapshot_unavailable": true,
	"selector_source_unavailable":        true,
}

func (d *Database) evaluateSelector(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, vault *vaultState, markets []LaneEconomics, identity func(context.Context) (programIdentityObservation, error), policy SelectorPolicy) (SelectorResult, error) {
	result, _, err := d.evaluateSelectorObserved(ctx, rpc, view, client, manifest, vault, markets, identity, policy)
	return result, err
}

// evaluateSelectorObserved also returns the observation the result was
// decided from, so the B2 leverage decision uses the same snapshot and
// planning inputs.
func (d *Database) evaluateSelectorObserved(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, vault *vaultState, markets []LaneEconomics, identity func(context.Context) (programIdentityObservation, error), policy SelectorPolicy) (SelectorResult, Observation, error) {
	o, err := observeSelectorShadow(ctx, d, rpc, view, manifest, vault, identity)
	if err != nil {
		return SelectorResult{}, Observation{}, err
	}
	request, err := readPilotCanaryEntryRequest(time.Now().UTC())
	if err != nil {
		return SelectorResult{}, Observation{}, err
	}
	var maximum []uint64
	onlyLane, onlySource := "", ""
	if request != nil {
		maximum = []uint64{uint64(request.EquityRaw)}
		onlyLane, onlySource = request.Lane, request.SourceLane
	}
	enriched, quotes, quoteErr := collectSelectorQuotes(ctx, rpc, view, client, manifest, o, markets, policy, onlyLane, onlySource, maximum...)
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
	result, err := d.RecordSelectorEvaluation(ctx, manifest.vault.RouteKey, SelectorInput{Now: time.Now().UTC(), Snapshot: o.Snapshot, Lanes: o.lanes, Markets: enriched, Quotes: quotes, Policy: policy, canaryRequest: request}, slot)
	return result, o, err
}
