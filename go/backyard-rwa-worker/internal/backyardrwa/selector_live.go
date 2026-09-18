package backyardrwa

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// collectSelectorQuotes prices a source once, then at most three independent
// destinations. Nothing here writes a journal row or signs a transaction.
func collectSelectorQuotes(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, canaryMaximum ...uint64) ([]LaneEconomics, []MoveQuote, error) {
	ctx, cancel := context.WithDeadline(ctx, o.ObservedAt.Add(8*time.Second))
	defer cancel()
	if err := policy.validate(); err != nil {
		return nil, nil, err
	}
	out := append([]LaneEconomics(nil), markets...)
	seen := map[string]bool{}
	for _, market := range out {
		if !selectorLane(market.Lane) || seen[market.Lane] {
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
	source, err := observeSelectorSource(ctx, rpc, client, manifest, o)
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
	quotes := make([]*MoveQuote, len(out))
	var wg sync.WaitGroup
	for i := range out {
		market := out[i]
		// Deferred rollout lanes keep their economics observable for KEEP
		// baselines and valuation, but receive no executable destination quote,
		// so no ENTER, SWITCH, or canary can select them.
		if !selectorEntryLane(market.Lane) {
			if market.EntryBlockedReason == "" {
				out[i].EntryCapacity = Capacity{Known: true}
				out[i].EntryBlockedReason = "lane_entry_deferred"
			}
			continue
		}
		// Current deployed capital is the KEEP baseline, never close/reopen merely
		// to quote the same destination. Idle ownership can enter that lane normally.
		if (hasWorkingCapital(s) && market.Lane == s.RouteLane) || market.validate(time.Now().UTC(), policy) != nil || market.EntryBlockedReason != "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			destination, err := observeSelectorDestinationSize(ctx, rpc, client, manifest, out[i].Lane, maximum, s.Slot, true)
			if err != nil {
				out[i].EntryCapacity = Capacity{Known: true}
				out[i].EntryBlockedReason = "complete_entry_quote_unavailable"
				return
			}
			q, err := composeSelectorMove(ctx, rpc, o, source, destination)
			if err != nil {
				out[i].EntryCapacity = Capacity{Known: true}
				out[i].EntryBlockedReason = "complete_move_quote_unavailable"
				return
			}
			// Capacity means this exact executable equity, not aggregate liquidity.
			out[i].EntryCapacity = Capacity{Known: true, Raw: q.EquityRaw}
			quotes[i] = &q
		}(i)
	}
	wg.Wait()
	result := make([]MoveQuote, 0, len(quotes))
	for _, q := range quotes {
		if q != nil {
			result = append(result, *q)
		}
	}
	return out, result, nil
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
	request, err := readPilotCanaryEntryRequest(time.Now().UTC())
	if err != nil {
		return SelectorResult{}, err
	}
	var maximum []uint64
	if request != nil {
		maximum = []uint64{uint64(request.EquityRaw)}
	}
	enriched, quotes, quoteErr := collectSelectorQuotes(ctx, rpc, productionJupiterClient(), manifest, o, markets, policy, maximum...)
	if quoteErr != nil {
		// No fabricated executable capacity on an outage. Current economic evidence
		// can still maintain persistence, while pure selection cannot enter/switch.
		enriched, quotes = append([]LaneEconomics(nil), markets...), nil
		for i := range enriched {
			enriched[i].EntryCapacity = Capacity{}
		}
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return SelectorResult{}, err
	}
	return d.RecordSelectorEvaluation(ctx, productionRouteKey, SelectorInput{Now: time.Now().UTC(), Snapshot: o.Snapshot, Markets: enriched, Quotes: quotes, Policy: policy, canaryRequest: request}, slot, version)
}
