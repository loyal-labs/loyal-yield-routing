package backyard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// trancheCollection is one sample's tranche quotes and why the rest are
// missing: a reason per (source>destination) pair, or one for the whole set.
type trancheCollection struct {
	Quotes    []MoveQuote
	Refusals  map[string]string
	SetReason string
}

// Tranche collection reasons. Outages carry a pair's persistence window
// (selectorAvailabilityReasons); a destination without room drops it.
const (
	trancheReasonPlanningUnavailable = "tranche_planning_unavailable"
	trancheReasonInProgress          = "tranche_in_progress"
	trancheReasonSourceQuote         = "tranche_source_quote_unavailable"
	trancheReasonNoRungFitsSource    = "tranche_no_rung_within_source_partial"
	trancheReasonDestinationQuote    = "tranche_destination_quote_unavailable"
	trancheReasonMoveQuote           = "tranche_move_quote_unavailable"
	trancheReasonDeadline            = "tranche_quote_deadline"
	trancheReasonRoomClosed          = "tranche_destination_room_closed"
	trancheReasonTopupUnsupported    = "tranche_destination_topup_unsupported"
)

// collectTrancheQuotes composes (working source, destination, E) tranche
// quotes from what the sample's quote collection already priced: its
// full-exit source quote scaled to a partial exit of E, plus every
// destination recipe its ladders priced at a size the source can free. A
// held destination is priced as a top-up of its own. Every early return and
// every refused pair records its reason.
func collectTrancheQuotes(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, capture *selectorQuoteCapture) trancheCollection {
	s := o.Snapshot
	out := trancheCollection{Refusals: map[string]string{}}
	maxE := int64(0)
	switch {
	case o.planning == nil:
		out.SetReason = trancheReasonPlanningUnavailable
	case o.planning.tranche != nil:
		out.SetReason = trancheReasonInProgress
	case !earnActiveLane(s.RouteLane):
		out.SetReason = "tranche_source_full_exit_only"
	case !trancheSourceSettled(s):
		out.SetReason = "tranche_finish_current_work_first"
	case !selectorDestinationLane(s.RouteLane):
		out.SetReason = "tranche_source_overflow_lane"
	case capture == nil || capture.source.Lane != s.RouteLane || capture.source.ObservationID != s.ObservationID || !sha256Pattern.MatchString(capture.source.Recipe.EvidenceID):
		out.SetReason = trancheReasonSourceQuote
	default:
		if maxE = min(trancheSourceMaxRaw(s), int64(strategyTwoBridgeLegCapRaw)); maxE < topupMinimumRaw {
			out.SetReason = "tranche_source_partial_unproven"
		}
	}
	if out.SetReason != "" {
		return out
	}
	source := capture.source
	held := s.heldLanes()
	heldDebt := map[string]int64{}
	for _, f := range o.heldFacts {
		heldDebt[f.Lane] = f.DebtValueRaw
	}
	capture.mu.Lock()
	destinations := append([]selectorDestinationQuote(nil), capture.destinations...)
	capture.mu.Unlock()
	best := map[string]MoveQuote{}
	bestBenefit := map[string]float64{}
	keep := func(q MoveQuote) {
		benefit := trancheQuoteBenefit(o, markets, policy, q)
		if _, seen := best[q.DestinationLane]; !seen || benefit > bestBenefit[q.DestinationLane] {
			best[q.DestinationLane], bestBenefit[q.DestinationLane] = q, benefit
		}
	}
	refuse := func(lane, reason string) {
		key := trancheWindowKey(s.RouteLane, lane)
		if _, priced := best[lane]; !priced && out.Refusals[key] == "" {
			out.Refusals[key] = reason
		}
	}
	// Flat destinations: every rung the whole-move ladders priced.
	rungs := map[string]int{}
	for _, d := range destinations {
		if d.Lane == s.RouteLane || containsLane(held, d.Lane) {
			continue
		}
		rungs[d.Lane]++
		if d.EquityRaw == 0 || d.EquityRaw > uint64(maxE) {
			continue
		}
		q, err := composeTrancheMove(ctx, view, o, source, d)
		if err != nil {
			var hold *BudgetHold
			if errors.As(err, &hold) && hold.Reason == "selector_move_cost_exceeds_equity" {
				refuse(d.Lane, "cost_exceeds_allocation")
			} else {
				refuse(d.Lane, trancheReasonMoveQuote)
			}
			continue
		}
		keep(q)
	}
	for lane, count := range rungs {
		if count > 0 {
			refuse(lane, trancheReasonNoRungFitsSource)
		}
	}
	// Held destinations: the whole-move ladder never prices a held lane, so
	// the top-up is priced here, in its own bounded window.
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	started := time.Now()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, market := range markets {
		lane := market.Lane
		if lane == s.RouteLane || !containsLane(held, lane) || !selectorDestinationLane(lane) || market.validate(time.Now().UTC(), policy) != nil || siblingLanes(s.RouteLane, lane) {
			continue
		}
		if heldDebt[lane] > 0 && sharedUSDCDebt(lane) {
			// Squads cash beside USDC debt is borrowed cash, so a top-up
			// cannot run there (topupStep): the lane takes no tranche.
			refuse(lane, trancheReasonTopupUnsupported)
			continue
		}
		wg.Add(1)
		go func(market LaneEconomics) {
			defer wg.Done()
			reason := ""
			size := uint64(maxE)
			for j := 0; j < selectorLadderRungs && size >= uint64(topupMinimumRaw); j++ {
				if j > 0 && time.Since(started) > selectorLadderBudget {
					break
				}
				destination, err := observeSelectorTopupForecast(ctx, rpc, view, client, manifest, o.policies, market, size, s.Slot)
				var hold *BudgetHold
				switch {
				case err == nil:
				case ctx.Err() != nil:
					reason = trancheReasonDeadline
				case errors.As(err, &hold) && (hold.Reason == trancheReasonRoomClosed || hold.Reason == trancheReasonTopupUnsupported):
					reason = hold.Reason
				default:
					reason = trancheReasonDestinationQuote
				}
				if err != nil {
					break
				}
				q, err := composeTrancheMove(ctx, view, o, source, destination)
				if err != nil {
					if errors.As(err, &hold) && hold.Reason == "selector_move_cost_exceeds_equity" {
						reason, size = "cost_exceeds_allocation", size/10
						continue
					}
					reason = trancheReasonMoveQuote
					break
				}
				mu.Lock()
				keep(q)
				mu.Unlock()
				if destination.EquityRaw < size {
					break // already clamped to the destination's room
				}
				size = selectorNextLadderSize(size, destinationEntrySwapLoss(destination.Recipe))
			}
			if reason != "" {
				mu.Lock()
				refuse(market.Lane, reason)
				mu.Unlock()
			}
		}(market)
	}
	wg.Wait()
	for _, q := range best {
		out.Quotes = append(out.Quotes, q)
	}
	sort.Slice(out.Quotes, func(i, j int) bool { return out.Quotes[i].DestinationLane < out.Quotes[j].DestinationLane })
	for key := range out.Refusals {
		lane := strings.TrimPrefix(key, s.RouteLane+">")
		if _, priced := best[lane]; priced {
			delete(out.Refusals, key)
		}
	}
	return out
}

// trancheQuoteBenefit evaluates one tranche quote through the pure selector.
func trancheQuoteBenefit(o Observation, markets []LaneEconomics, policy SelectorPolicy, q MoveQuote) float64 {
	selection := selectTranche(SelectorInput{Now: time.Now().UTC(), Snapshot: o.Snapshot, Markets: markets, Quotes: []MoveQuote{q}, Policy: policy}, nil)
	for _, c := range selection.Candidates {
		if c.Destination == q.DestinationLane && c.CostsKnown && c.BlockedReason == "" {
			return c.BenefitRaw
		}
	}
	return math.Inf(-1)
}

// composeTrancheMove composes a tranche quote: the source's priced full exit
// scaled to a partial exit of the destination's E, with no exit bound (the
// source keeps the rest of its position), plus the destination recipe.
func composeTrancheMove(ctx context.Context, view *View, o Observation, source selectorSourceQuote, destination selectorDestinationQuote) (MoveQuote, error) {
	if destination.EquityRaw == 0 || destination.EquityRaw > math.MaxInt64 {
		return MoveQuote{}, budgetHold("invalid_selector_move")
	}
	bound, expected, err := trancheSourcePartialCost(source.Recipe, o.Snapshot, int64(destination.EquityRaw))
	if err != nil {
		return MoveQuote{}, err
	}
	partial := source
	partial.ExitBound = nil
	partial.MinimumIdleRaw = destination.EquityRaw
	partial.Recipe.CostRaw, partial.Recipe.ExpectedCostRaw = bound, &expected
	q, err := composeSelectorMove(ctx, view, o, partial, destination)
	if err != nil {
		return q, err
	}
	q.Tranche, q.SourceExit = true, nil
	raw, err := json.Marshal(struct {
		Quote          MoveQuote
		SourceEvidence string
	}{q, source.Recipe.EvidenceID})
	if err != nil {
		return q, err
	}
	q.EvidenceID = sha256Bytes(raw)
	return q, nil
}

// observeSelectorTopupForecast prices a tranche top-up of a held destination
// with the same legs the plan B3 top-up executes: allocate E, swap it to
// collateral, deposit, and the reports between. The top-up is debt-free
// (1x on the increment); the lane then levers toward its own stored target
// under the leverage decision's own cost check.
func observeSelectorTopupForecast(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, m RouteManifest, policies installedPolicies, market LaneEconomics, equity uint64, sampleSlot int64) (selectorDestinationQuote, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lane := market.Lane
	out := selectorDestinationQuote{Lane: lane, EquityRaw: equity, Unlevered: true}
	if rpc == nil || client == nil || !earnActiveLane(lane) || equity == 0 || equity > strategyTwoBridgeLegCapRaw || sampleSlot <= 0 || sampleSlot > math.MaxInt64-budgetMaxObservationLagCeilingSlots {
		return out, budgetHold("invalid_selector_destination")
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return out, err
	}
	slot, accounts, position, err := observeSelectorDestinationBatch(ctx, rpc, view, route, sampleSlot)
	if err != nil {
		return out, err
	}
	if !position.HasPosition || position.CollateralDepositedRaw == 0 {
		return out, budgetHold("tranche_destination_not_held")
	}
	if position.DebtRaw > 0 && !debtTopupLane(lane) {
		return out, budgetHold("tranche_destination_topup_unsupported")
	}
	room, err := topupDepositRoomUSDC(accounts, route)
	if err != nil {
		return out, err
	}
	equity = min(equity, room)
	if equity < uint64(topupMinimumRaw) {
		return out, budgetHold("tranche_destination_room_closed")
	}
	out.EquityRaw = equity
	out.AccountSlot, out.AccountsSHA256 = slot, hashConfirmedAccounts(accounts)
	blockhash, err := latestBlockhash(ctx, rpc)
	if err != nil {
		return out, err
	}
	var inputs []*phase3BuildInput
	appendInput := func(request any, effects ExpectedEffects) error {
		raw, err := jsonMarshalExpectedEffects(effects)
		if err != nil {
			return err
		}
		input, err := encodePhase3BuildInput(request, raw)
		if err != nil {
			return err
		}
		inputs = append(inputs, input)
		return nil
	}
	// Fixed-width report fields are fee templates, never NAV evidence.
	report := BridgeReport{Sequence: uint64(slot), ObservedSlot: uint64(slot), NAVAfterRaw: equity, SnapshotDigest: out.AccountsSHA256}
	appendBridge := func(action Action, amount, idle, strategy, squads uint64) error {
		e, _, _, err := bridgeExpectedEffects(Decision{Action: action, AmountRaw: int64(amount)}, idle, strategy, squads)
		if err != nil {
			return err
		}
		e.Kind, e.ReturnData = "bridge", expectedAdaptorReturnData(equity)
		policy, err := policies.account(policyKey{action: action})
		if err != nil {
			return err
		}
		return appendInput(BridgeBuildRequest{Action: action, AmountRaw: amount, Report: report, Policy: policy, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight}, e)
	}
	if err = appendBridge(VoltrAllocateToSquads, equity, equity, 0, 0); err != nil {
		return out, err
	}
	if err = appendBridge(ReportNAV, 0, 0, 0, equity); err != nil {
		return out, err
	}
	swap, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, policies, Decision{Action: SwapStableToCollateralStep, AmountRaw: int64(equity), StrategyKey: lane}, equity, 0, slot)
	if err != nil {
		return out, err
	}
	observationFloor, err := selectorSwapObservationFloor(swap.Request, sampleSlot, slot)
	if err != nil {
		return out, err
	}
	if err = appendInput(swap.Request, swap.ExpectedEffects); err != nil {
		return out, err
	}
	if err = appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
		return out, err
	}
	out.InitialCollateralMinimumRaw = swap.Request.MinimumOutputRaw
	rounding, err := selectorReceiptRounding(accounts, route, slot)
	if err != nil {
		return out, err
	}
	if swap.Request.MinimumOutputRaw <= rounding+1 {
		return out, budgetHold("selector_destination_below_deposit_minimum")
	}
	amount := swap.Request.MinimumOutputRaw
	liquidity := binary.LittleEndian.Uint64(accountAt(accounts, route.CollateralLiquiditySupply).Data[64:72])
	if amount > math.MaxUint64-liquidity {
		return out, budgetHold("selector_destination_amount_overflow")
	}
	deposit, err := m.kaminoPacketForRoute(policies, OpenRouteStep, kaminoLegDeposit, amount, blockhash, lane)
	if err != nil {
		return out, err
	}
	deposit.ObligationReserves = []string{route.Kamino.CollateralReserve}
	if position.DebtRaw > 0 {
		deposit.ObligationReserves = append(deposit.ObligationReserves, route.Kamino.DebtReserve)
	}
	if err = appendInput(deposit, ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-deposit", Conserved: true, Deposit: &ExpectedDeposit{amount - rounding, amount}, Accounts: []ExpectedAccountEffect{
		{Address: route.CollateralCustody, Owner: route.CollateralTokenProgram, Mint: route.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: amount, AfterRaw: 0},
		{Address: route.CollateralLiquiditySupply, Owner: route.CollateralTokenProgram, Mint: route.Kamino.CollateralMint, Authority: route.Kamino.MarketAuthority, BeforeRaw: liquidity, AfterRaw: liquidity + amount},
	}}); err != nil {
		return out, err
	}
	if err = appendBridge(ReportNAV, 0, 0, 0, 0); err != nil {
		return out, err
	}
	out.PayoffSwap = swap
	out.Recipe, err = m.priceSelectorRecipeWithFloor(ctx, rpc, view, lane, inputs, sampleSlot, observationFloor)
	if err != nil {
		return out, err
	}
	for _, funding := range []struct {
		address string
		amount  uint64
	}{{bridgeDelegate, out.Recipe.NetworkLamports}, {bridgeVault, out.Recipe.SetupLamports}} {
		a := accountAt(accounts, funding.address)
		if a.Owner != "11111111111111111111111111111111" || a.Executable || len(a.Data) != 0 || a.Lamports < funding.amount {
			return out, budgetHold("selector_destination_native_funding_unavailable")
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	out.Recipe.EvidenceID = sha256Bytes(raw)
	return out, nil
}
