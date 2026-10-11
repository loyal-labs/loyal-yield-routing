package backyard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// collectTrancheQuotes prices (working source, destination, E) tranche
// quotes: the source's priced full exit scaled to a partial exit of E, plus
// the destination's exact recipe for E (a flat entry, or a top-up of a held
// lane). E starts at the largest partial the source can free and follows
// the depth-aware ladder. Refusals that say the destination has no room are
// returned per pair; outages are not, so their windows carry.
func collectTrancheQuotes(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, o Observation, markets []LaneEconomics, policy SelectorPolicy, source selectorSourceQuote) ([]MoveQuote, map[string]string) {
	s := o.Snapshot
	refusals := map[string]string{}
	if o.planning == nil || o.planning.tranche != nil || !earnActiveLane(s.RouteLane) || !trancheSourceSettled(s) ||
		source.Lane != s.RouteLane || source.ObservationID != s.ObservationID || !sha256Pattern.MatchString(source.Recipe.EvidenceID) {
		return nil, refusals
	}
	maxE := min(trancheSourceMaxRaw(s), int64(strategyTwoBridgeLegCapRaw))
	if maxE < topupMinimumRaw {
		return nil, refusals
	}
	ctx, cancel := context.WithDeadline(ctx, o.ObservedAt.Add(8*time.Second))
	defer cancel()
	held := s.heldLanes()
	heldDebt := map[string]int64{}
	for _, f := range o.heldFacts {
		heldDebt[f.Lane] = f.DebtValueRaw
	}
	sorted := append([]LaneEconomics(nil), markets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Lane < sorted[j].Lane })
	var mu sync.Mutex
	var wg sync.WaitGroup
	var quotes []MoveQuote
	for _, market := range sorted {
		lane := market.Lane
		key := trancheWindowKey(s.RouteLane, lane)
		if lane == s.RouteLane || !earnActiveLane(lane) || market.validate(time.Now().UTC(), policy) != nil || siblingLanes(s.RouteLane, lane) {
			continue
		}
		destinationHeld := containsLane(held, lane)
		if destinationHeld && heldDebt[lane] > 0 && sharedUSDCDebt(lane) {
			// Squads cash beside USDC debt is borrowed cash, so a top-up
			// cannot run there (topupStep): the lane takes no tranche.
			refusals[key] = "tranche_destination_topup_unsupported"
			continue
		}
		wg.Add(1)
		go func(market LaneEconomics) {
			defer wg.Done()
			var best *MoveQuote
			bestBenefit := math.Inf(-1)
			size := uint64(maxE)
			for j := 0; j < selectorLadderRungs && size >= uint64(topupMinimumRaw); j++ {
				if j > 0 && (ctx.Err() != nil || time.Since(o.ObservedAt) > selectorLadderBudget) {
					break
				}
				var destination selectorDestinationQuote
				var err error
				if destinationHeld {
					destination, err = observeSelectorTopupForecast(ctx, rpc, view, client, manifest, o.policies, market, size, s.Slot)
				} else {
					destination, err = observeSelectorDestinationForecast(ctx, rpc, view, client, manifest, o.policies, market, size, s.Slot, true, nil)
				}
				var hold *BudgetHold
				if err != nil {
					if j == 0 && errors.As(err, &hold) && (hold.Reason == "selector_destination_capacity_unavailable" || hold.Reason == "tranche_destination_room_closed") {
						mu.Lock()
						refusals[key] = "tranche_destination_room_closed"
						mu.Unlock()
					}
					break
				}
				q, err := composeTrancheMove(ctx, view, o, source, destination)
				if err != nil {
					if errors.As(err, &hold) && hold.Reason == "selector_move_cost_exceeds_equity" {
						size /= 10
						continue
					}
					break
				}
				benefit := trancheQuoteBenefit(o, markets, policy, q)
				if best == nil || benefit > bestBenefit {
					best, bestBenefit = &q, benefit
				}
				if benefit > float64(policy.MinimumBenefitRaw) || destination.EquityRaw < size {
					// Profitable, or already clamped to the destination's room.
					break
				}
				size = selectorNextLadderSize(size, destinationEntrySwapLoss(destination.Recipe))
			}
			if best != nil {
				mu.Lock()
				quotes = append(quotes, *best)
				mu.Unlock()
			}
		}(market)
	}
	wg.Wait()
	sort.Slice(quotes, func(i, j int) bool { return quotes[i].DestinationLane < quotes[j].DestinationLane })
	return quotes, refusals
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
