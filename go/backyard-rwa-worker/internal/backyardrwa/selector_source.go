package backyardrwa

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

// Source forecasts reuse the finite production exit graph. They do not commit
// an unwind, reserve a budget or assert that prospective balances exist.
type selectorSourceQuote struct {
	Lane           string             `json:"lane"`
	ObservationID  string             `json:"observationId"`
	MinimumIdleRaw uint64             `json:"minimumIdleRaw"`
	Recipe         selectorRecipe     `json:"recipe"`
	ExitBound      *selectorExitBound `json:"exitBound,omitempty"`
}

// selectorSourcePools is the per-mint ledger behind a full exit's guaranteed
// proceeds. USDC cash and raw route-debt funding are separate pools: route
// debt-mint units never become cash by arithmetic, so cash — the only pool
// MinimumIdleRaw reports — can grow only through a bound swap's enforced
// minimum output. Residue a recipe produces stays tracked by that recipe's
// own legs; nothing here attributes residue from a custody address alone.
type selectorSourcePools struct {
	cash           uint64
	debtFunding    uint64
	collateralMint string
	debtMint       string
}

func newSelectorSourcePools(cash uint64, route RuntimeRoute) selectorSourcePools {
	return selectorSourcePools{cash: cash, collateralMint: route.Kamino.CollateralMint, debtMint: route.Kamino.DebtMint}
}

// creditCash adds guaranteed returned USDC.
func (p *selectorSourcePools) creditCash(amount uint64) error {
	if amount > math.MaxUint64-p.cash {
		return budgetHold("selector_source_cash_overflow")
	}
	p.cash += amount
	return nil
}

// creditSwap validates one exit swap against the route's bound mint pairs and
// credits its enforced minimum output into the pool of the output mint.
// Sold amounts are exact: collateral legs must sell the route's own
// collateral, and the residue conversion must sell funding this recipe
// actually produced — it consumes that funding before crediting cash, so a
// repeated or oversized conversion cannot fabricate proceeds. A foreign pair,
// identical sides, USDC as the sold side, or a degenerate amount fails
// closed: USDC cash is never an exit input, and raw route debt is never
// renamed into cash by arithmetic. Raw sold and output amounts are never
// compared to each other — mints differ in decimals and price, the minimum
// output is already bound by upstream swap evidence, and this helper
// invents no price or parity restriction of its own.
func (p *selectorSourcePools) creditSwap(action Action, sell, buy string, soldAmount, minimumOutput uint64) error {
	bound := (action == SwapCollateralToStableStep && sell == p.collateralMint && buy == bridgeUSDC) ||
		(action == SwapCollateralToDebtStep && sell == p.collateralMint && buy == p.debtMint) ||
		(action == SwapDebtToUSDCStep && sell == p.debtMint && buy == bridgeUSDC)
	if !bound || sell == buy || soldAmount == 0 || minimumOutput == 0 {
		return budgetHold("selector_source_not_an_exit")
	}
	if action == SwapDebtToUSDCStep {
		if soldAmount > p.debtFunding {
			return budgetHold("selector_source_residue_unfunded")
		}
		if minimumOutput > math.MaxUint64-p.cash {
			return budgetHold("selector_source_cash_overflow")
		}
		p.debtFunding -= soldAmount
		p.cash += minimumOutput
		return nil
	}
	if buy == bridgeUSDC {
		return p.creditCash(minimumOutput)
	}
	if minimumOutput > math.MaxUint64-p.debtFunding {
		return budgetHold("selector_source_funding_overflow")
	}
	p.debtFunding += minimumOutput
	return nil
}

// selectorRepayMintBound requires a repay step's token effects to move only
// the route's own debt mint; a repay against any other mint must never
// consume this lane's funding pool.
func selectorRepayMintBound(effects ExpectedEffects, debtMint string) bool {
	seen := false
	for _, effect := range effects.Accounts {
		if effect.Mint == "" {
			continue
		}
		if effect.Mint != debtMint {
			return false
		}
		seen = true
	}
	return seen
}

// repay consumes the route debt's own funding pool. USDC-debt lanes repay
// from cash exactly as every persisted Maple exit assumed; a non-USDC debt
// lane must have produced bound debt-mint funding first — raw route debt is
// never converted to cash to make room. The conservative funding remainder
// after repayment stays debt-mint units tracked by the recipe; anything
// larger than that remainder needs refreshed observation and a newly bound
// continuation before it can ever be cash.
func (p *selectorSourcePools) repay(amount uint64) error {
	if p.debtMint == bridgeUSDC {
		if amount > p.cash {
			return budgetHold("selector_source_minimum_cannot_pay_debt")
		}
		p.cash -= amount
		return nil
	}
	if amount > p.debtFunding {
		return budgetHold("selector_source_minimum_cannot_pay_debt")
	}
	p.debtFunding -= amount
	return nil
}

func observeSelectorSource(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation) (selectorSourceQuote, error) {
	return observeReviewedSelectorSource(ctx, rpc, client, m, o, false)
}

// observeAutoSelectorSource is the internal candidate source producer for the
// AUTO lane. It runs the identical coherence gates and the identical finite
// producer chain through an explicit reviewed manifest: public production
// gates stay unchanged, the live selector routes every AUTO source quote —
// idle and funded — through this entry over the durable planning observation
// manifest, and no lane list or global selector state is touched.
func observeAutoSelectorSource(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation) (selectorSourceQuote, error) {
	return observeReviewedSelectorSource(ctx, rpc, client, m, o, true)
}

// selectorSourceLaneAuthorized keeps the public selector-lane gate untouched
// and admits exactly one candidate lane: the AUTO lane through an explicit
// manifest whose reviewed autoPolicy binding resolves and whose activation
// selects that lane.
func selectorSourceLaneAuthorized(m RouteManifest, lane string, candidate bool) bool {
	if selectorLane(lane) {
		return true
	}
	if !candidate || lane != autoAUTOPYUSD.Lane {
		return false
	}
	if _, err := m.autoPolicyBinding(); err != nil {
		return false
	}
	active, err := m.activeRuntimeRoute()
	return err == nil && active.Lane == lane
}

func observeReviewedSelectorSource(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation, candidate bool) (selectorSourceQuote, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s := o.Snapshot
	out := selectorSourceQuote{Lane: s.RouteLane, ObservationID: s.ObservationID}
	if rpc == nil || client == nil || !s.PilotActive || !selectorSourceLaneAuthorized(m, s.RouteLane, candidate) || s.RouteLane != s.StrategyKey || !s.Fresh || s.ObservationID == "" || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagSlots || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.ManualReason != "" || s.Unwind || s.CutoverDrain || s.WithdrawalDemandRaw != 0 || s.VoltrIdleRaw < 0 || s.VoltrStrategyIdleRaw < 0 || s.SquadsIdleRaw < 0 || s.CollateralIdleRaw < 0 || s.PositionDebtRaw < 0 || s.PositionCollateralRaw < 0 || s.DebtIdleRaw != 0 {
		return out, budgetHold("selector_source_unavailable")
	}
	if !hasWorkingCapital(s) {
		out.MinimumIdleRaw = uint64(s.VoltrIdleRaw)
		out.Recipe.ValidThroughSlot = s.Slot + budgetMaxObservationLagSlots
		raw, err := json.Marshal(struct {
			Kind     string
			Snapshot Snapshot
		}{"OBSERVED_IDLE_NO_EXIT", s})
		if err != nil {
			return out, err
		}
		out.Recipe.EvidenceID = sha256Bytes(raw)
		return out, nil
	}
	floor, err := observeWithdrawalExitPolicies(ctx, rpc, m, s.RouteLane, s.Slot, nil)
	if err != nil {
		return out, err
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return out, err
	}
	d := Decision{Action: ReportNAV, StrategyKey: s.RouteLane}
	e, _, _, err := bridgeExpectedEffects(d, uint64(s.VoltrIdleRaw), uint64(s.VoltrStrategyIdleRaw), uint64(s.SquadsIdleRaw))
	if err != nil {
		return out, err
	}
	if s.StrategyNAVRaw < 0 || !sha256Pattern.MatchString(s.ReportSnapshotDigest) {
		return out, budgetHold("selector_source_nav_unavailable")
	}
	e.Kind, e.ReturnData = "bridge", expectedAdaptorReturnData(uint64(s.StrategyNAVRaw))
	r := BridgeBuildRequest{Action: ReportNAV, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight,
		Report: BridgeReport{Sequence: uint64(s.Slot), ObservedSlot: uint64(s.Slot), NAVAfterRaw: uint64(s.StrategyNAVRaw), SnapshotDigest: s.ReportSnapshotDigest}}
	var plan phase3BridgeAdmission
	switch {
	case s.PositionDebtRaw > 0:
		plan, err = observePhase3FundingAdmission(ctx, rpc, client, m, o, d, r, e)
	case s.HasPosition || s.PositionCollateralRaw > 0:
		plan, err = pricePhase3PositionReturn(ctx, rpc, client, m, o, d, r, e, false)
	case s.CollateralIdleRaw > 0:
		plan, err = observePhase3CollateralReturnAdmission(ctx, rpc, client, m, o, d, r, e)
	default:
		plan, err = observePhase3BridgeAdmission(ctx, rpc, o, d, BridgeExecutionEvidence{Request: r, ExpectedEffects: e})
	}
	if err != nil {
		return out, err
	}
	return priceReviewedSelectorSourcePlan(ctx, rpc, client, m, plan, floor, candidate)
}

// priceSelectorSourcePlan stays the persisted-build compatibility form: it
// prices embedded-manifest selector lanes exactly as every persisted plan
// always has, and still refuses the candidate AUTO lane outright.
func priceSelectorSourcePlan(ctx context.Context, rpc *RPCClient, plan phase3BridgeAdmission, observationFloor int64) (selectorSourceQuote, error) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return selectorSourceQuote{}, err
	}
	return manifest.priceSelectorSourcePlan(ctx, rpc, plan, observationFloor)
}

func (m RouteManifest) priceSelectorSourcePlan(ctx context.Context, rpc *RPCClient, plan phase3BridgeAdmission, observationFloor int64) (selectorSourceQuote, error) {
	return priceReviewedSelectorSourcePlan(ctx, rpc, nil, m, plan, observationFloor, false)
}

// priceReviewedSelectorSourcePlan is the manifest-aware consumer behind the
// public pricer. Retained legs decode, compile and measure against the SAME
// reviewed manifest that produced them; every identity, bound-pair, cost-hash
// and freshness check is shared with the public path unchanged.
func priceReviewedSelectorSourcePlan(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, plan phase3BridgeAdmission, observationFloor int64, candidate bool) (selectorSourceQuote, error) {
	s := plan.Snapshot
	out := selectorSourceQuote{Lane: s.RouteLane, ObservationID: s.ObservationID}
	if !selectorSourceLaneAuthorized(m, s.RouteLane, candidate) || !s.PilotActive || s.VoltrIdleRaw < 0 || s.VoltrStrategyIdleRaw < 0 || s.SquadsIdleRaw < 0 || s.DebtIdleRaw != 0 || plan.Input == nil || len(plan.Exit) == 0 {
		return out, budgetHold("selector_source_recipe_unavailable")
	}
	if candidate {
		// The producer's funding tail sells the margin-inflated upper residue.
		// Rewrite that conversion at the guaranteed funding remainder before
		// any pool credits it as cash.
		if err := requoteAutoResidueContinuation(ctx, rpc, client, m, &plan); err != nil {
			return out, err
		}
	}
	inputs := []*phase3BuildInput{plan.Input}
	costs := []ValuedTransactionCost{plan.CurrentCost}
	for _, step := range plan.Exit {
		if step.Template == nil {
			return out, budgetHold("selector_source_template_missing")
		}
		inputs = append(inputs, step.Template)
		costs = append(costs, step.Cost)
	}
	// Project minimum proceeds independently from reservation upper balances.
	// The funding tail's optimistic residue and Stage/Restore amounts are NOT
	// cash. Guaranteed USDC and raw route-debt funding are separate pools; the
	// pool helper decides where each bound leg's minimum output lands.
	route, routeErr := runtimeRoute(s.RouteLane)
	if routeErr != nil {
		return out, routeErr
	}
	pools := newSelectorSourcePools(uint64(s.SquadsIdleRaw), route)
	restored := false
	for i, input := range inputs {
		if costs[i].ObservationSlot < s.Slot {
			return out, budgetHold("selector_recipe_observation_expired")
		}
		observationFloor = max(observationFloor, costs[i].ObservationSlot)
		if observationFloor > s.Slot+budgetMaxObservationLagSlots {
			return out, budgetHold("selector_recipe_observation_expired")
		}
		request, effects, message, err := input.decodeWithManifest(m)
		if err != nil {
			return out, err
		}
		debit, err := m.measureExecutableDebit(request, effects)
		if err != nil {
			return out, err
		}
		if sha256Bytes(message) != costs[i].MessageSHA256 || debit != costs[i].Debit {
			return out, budgetHold("selector_source_template_mismatch")
		}
		var action Action
		var amount uint64
		switch r := request.(type) {
		case BridgeBuildRequest:
			action, amount = r.Action, r.AmountRaw
			if action != ReportNAV && action != StageSquadsToVoltr && action != VoltrRestoreIdle {
				return out, budgetHold("selector_source_not_an_exit")
			}
			if action == VoltrRestoreIdle {
				restored = true
			}
		case JupiterSwapRequest:
			action, amount = r.Action, r.AmountRaw
			if r.RouteLane != s.RouteLane || len(effects.Accounts) != 2 {
				return out, budgetHold("selector_source_not_an_exit")
			}
			if err := pools.creditSwap(r.Action, effects.Accounts[0].Mint, effects.Accounts[1].Mint, r.AmountRaw, r.MinimumOutputRaw); err != nil {
				return out, err
			}
			observationFloor, err = selectorSwapObservationFloor(r, s.Slot, observationFloor)
			if err != nil {
				return out, err
			}
		case KaminoPrimeUSDCRequest:
			action, amount = r.Action, r.AmountRaw
			_, leg, err := kaminoPrimeUSDCInstruction(r)
			if err != nil {
				return out, err
			}
			if r.RouteLane != s.RouteLane {
				return out, budgetHold("selector_source_not_an_exit")
			}
			switch leg {
			case kaminoLegRepay:
				if !selectorRepayMintBound(effects, route.Kamino.DebtMint) {
					return out, budgetHold("selector_source_repay_mint_foreign")
				}
				if err := pools.repay(r.AmountRaw); err != nil {
					return out, err
				}
			case kaminoLegWithdraw:
			default:
				return out, budgetHold("selector_source_not_an_exit")
			}
		default:
			return out, budgetHold("selector_source_not_an_exit")
		}
		if i > 0 && (plan.Exit[i-1].Action != action || plan.Exit[i-1].Amount != amount) {
			return out, budgetHold("selector_source_template_mismatch")
		}
	}
	if !restored {
		return out, budgetHold("selector_source_idle_return_missing")
	}
	if err := pools.creditCash(uint64(s.VoltrIdleRaw)); err != nil {
		return out, err
	}
	if err := pools.creditCash(uint64(s.VoltrStrategyIdleRaw)); err != nil {
		return out, err
	}
	out.MinimumIdleRaw = pools.cash
	out.ExitBound = &selectorExitBound{MaxCollateralRaw: s.PositionCollateralRaw, MaxDebtRaw: s.PositionDebtRaw}
	if plan.Payoff != nil {
		if plan.Payoff.UpperDebtRaw > math.MaxInt64 {
			return out, budgetHold("selector_source_debt_overflow")
		}
		out.ExitBound.MaxDebtRaw = int64(plan.Payoff.UpperDebtRaw)
	}
	var err error
	out.Recipe, err = m.priceSelectorRecipeWithFloor(ctx, rpc, s.RouteLane, inputs, s.Slot, observationFloor)
	if err != nil {
		return out, err
	}
	out.Recipe.ValidThroughSlot = min(out.Recipe.ValidThroughSlot, plan.ValidThroughSlot)
	for i, cost := range out.Recipe.Costs {
		// The producer starts from a cost-only NAV anchor. When NAV is already
		// settled, that extra report is conservative economic expense, not a
		// remaining exit debit in the durable reservation.
		anchorOnly := false
		if i == 0 && Decide(s).Action != ReportNAV {
			request, _, _, decodeErr := inputs[i].decodeWithManifest(m)
			if decodeErr != nil {
				return out, decodeErr
			}
			r, ok := request.(BridgeBuildRequest)
			anchorOnly = ok && r.Action == ReportNAV
		}
		if !anchorOnly {
			out.ExitBound.GrossMicros, err = budgetSum(out.ExitBound.GrossMicros, cost.TotalMicros)
			if err != nil {
				return out, err
			}
		}
		observationFloor = max(observationFloor, cost.ObservationSlot)
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return out, err
	}
	if slot < observationFloor || slot > out.Recipe.ValidThroughSlot {
		return out, budgetHold("selector_recipe_observation_expired")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	out.Recipe.EvidenceID = sha256Bytes(raw)
	return out, nil
}

// requoteAutoResidueContinuation walks one candidate plan over the SAME
// ledger sequence the consumer enforces — the anchor input first, then every
// exit leg in order — and fixes the funding tail's optimistic upper amounts:
//
//  1. The residue conversion is requoted at the GUARANTEED continuation: the
//     enforced funding minimum minus the repayment upper. The producer sold
//     the margin-inflated upper residue instead, which structurally exceeds
//     that remainder; the excess was never guaranteed cash. A zero remainder
//     drops the conversion entirely rather than selling unproduced funding.
//  2. Every bridge continuation template after a USDC conversion is rebuilt
//     from the guaranteed running custody, because the producer staged and
//     restored the optimistic upper squad balance. Each rebuilt leg is
//     re-measured through the same build-cost gate, so its fee, hash, debit
//     and freshness are re-proven; nothing skips a check.
//
// Rewritten legs re-enter the identical consumer walk afterwards.
func requoteAutoResidueContinuation(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, plan *phase3BridgeAdmission) error {
	if client == nil {
		return budgetHold("selector_source_residue_quote_unavailable")
	}
	s := plan.Snapshot
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return err
	}
	pools := newSelectorSourcePools(uint64(s.SquadsIdleRaw), route)
	idle, strategy, squads := uint64(s.VoltrIdleRaw), uint64(s.VoltrStrategyIdleRaw), uint64(s.SquadsIdleRaw)
	dropIndex, sawConversion := -1, false
	// rebuildBridge regenerates one cost-only bridge continuation template from
	// the guaranteed running custody, mirroring the producer's own step
	// assembly byte-for-byte, then re-measures its fee and cost.
	rebuildBridge := func(index int) error {
		old := plan.Exit[index]
		request, _, _, err := old.Template.decodeWithManifest(m)
		if err != nil {
			return err
		}
		r, ok := request.(BridgeBuildRequest)
		if !ok {
			return budgetHold("selector_source_not_an_exit")
		}
		amount := r.AmountRaw
		if r.Action == StageSquadsToVoltr {
			amount = squads
		} else if r.Action == VoltrRestoreIdle {
			amount = strategy
		}
		if amount > math.MaxInt64 {
			return budgetHold("selector_source_exit_amount_overflow")
		}
		effects, afterStrategy, afterSquads, err := bridgeExpectedEffects(Decision{Action: r.Action, AmountRaw: int64(amount)}, idle, strategy, squads)
		if err != nil {
			return err
		}
		if afterStrategy > math.MaxUint64-afterSquads {
			return budgetHold("selector_source_exit_amount_overflow")
		}
		next := r
		next.Action, next.AmountRaw = r.Action, amount
		next.Report.NAVAfterRaw = afterSquads
		effects.Kind = "bridge"
		if r.Action != StageSquadsToVoltr {
			effects.ReturnData = expectedAdaptorReturnData(next.Report.NAVAfterRaw)
		}
		if _, err = m.measureExecutableDebit(next, effects); err != nil {
			return err
		}
		if r.Action == VoltrRestoreIdle {
			idle += amount
		}
		strategy, squads = afterStrategy, afterSquads
		cost, err := m.observePhase3KnownBuildCost(ctx, rpc, next, effects)
		if err != nil {
			return err
		}
		encoded, err := jsonMarshalExpectedEffects(effects)
		if err != nil {
			return err
		}
		input, err := encodePhase3BuildInput(next, encoded)
		if err != nil {
			return err
		}
		if old.Cost.TotalMicros > plan.ExitAfterMicros {
			return budgetHold("selector_source_exit_cost_mismatch")
		}
		plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros-old.Cost.TotalMicros, cost.TotalMicros)
		if err != nil {
			return err
		}
		plan.Exit[index] = phase3BridgeExitCost{Action: next.Action, Amount: next.AmountRaw, Cost: cost, Template: input}
		plan.ValidThroughSlot = min(plan.ValidThroughSlot, cost.ValidThroughSlot)
		return nil
	}
	rewrite := func(index int, remainder uint64) (uint64, error) {
		swap, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, Decision{Action: SwapDebtToUSDCStep, AmountRaw: int64(remainder), StrategyKey: s.RouteLane}, remainder, pools.cash, plan.Exit[index].Cost.ObservationSlot)
		if err != nil {
			return 0, budgetHold("selector_source_residue_quote_unavailable")
		}
		cost, err := m.observePhase3KnownBuildCost(ctx, rpc, swap.Request, swap.ExpectedEffects)
		if err != nil {
			return 0, err
		}
		encoded, err := jsonMarshalExpectedEffects(swap.ExpectedEffects)
		if err != nil {
			return 0, err
		}
		input, err := encodePhase3BuildInput(swap.Request, encoded)
		if err != nil {
			return 0, err
		}
		old := plan.Exit[index]
		if old.Cost.TotalMicros > plan.ExitAfterMicros {
			return 0, budgetHold("selector_source_exit_cost_mismatch")
		}
		plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros-old.Cost.TotalMicros, cost.TotalMicros)
		if err != nil {
			return 0, err
		}
		plan.Exit[index] = phase3BridgeExitCost{Action: swap.Request.Action, Amount: swap.Request.AmountRaw, Cost: cost, Template: input}
		plan.ValidThroughSlot = min(plan.ValidThroughSlot, cost.ValidThroughSlot)
		if err := pools.creditSwap(SwapDebtToUSDCStep, route.Kamino.DebtMint, bridgeUSDC, remainder, swap.Request.MinimumOutputRaw); err != nil {
			return 0, err
		}
		return swap.Request.MinimumOutputRaw, nil
	}
	// Anchor input first: it shares the ledger sequence but starts no pool
	// credit, and only a bridge anchor can open a source continuation.
	if plan.Input == nil {
		return budgetHold("selector_source_recipe_unavailable")
	}
	if request, _, _, err := plan.Input.decodeWithManifest(m); err != nil {
		return err
	} else if _, ok := request.(BridgeBuildRequest); !ok {
		return budgetHold("selector_source_not_an_exit")
	}
	for i := 0; i < len(plan.Exit); i++ {
		step := plan.Exit[i]
		if step.Template == nil {
			return budgetHold("selector_source_template_missing")
		}
		request, effects, _, err := step.Template.decodeWithManifest(m)
		if err != nil {
			return err
		}
		switch r := request.(type) {
		case BridgeBuildRequest:
			if r.Action != ReportNAV && r.Action != StageSquadsToVoltr && r.Action != VoltrRestoreIdle {
				return budgetHold("selector_source_not_an_exit")
			}
			if sawConversion {
				if err := rebuildBridge(i); err != nil {
					return err
				}
			}
		case JupiterSwapRequest:
			if len(effects.Accounts) != 2 {
				return budgetHold("selector_source_not_an_exit")
			}
			if r.Action == SwapDebtToUSDCStep && r.AmountRaw > pools.debtFunding {
				remainder := pools.debtFunding
				if remainder > 0 {
					credited, err := rewrite(i, remainder)
					if err != nil {
						return err
					}
					squads += credited
					sawConversion = true
					continue
				}
				// Zero guaranteed residue: the conversion would sell funding
				// this recipe never produced. Drop the leg and its cost.
				if step.Cost.TotalMicros > plan.ExitAfterMicros {
					return budgetHold("selector_source_exit_cost_mismatch")
				}
				plan.ExitAfterMicros -= step.Cost.TotalMicros
				dropIndex = i
				sawConversion = true
				continue
			}
			if err := pools.creditSwap(r.Action, effects.Accounts[0].Mint, effects.Accounts[1].Mint, r.AmountRaw, r.MinimumOutputRaw); err != nil {
				return err
			}
			if r.Action == SwapCollateralToStableStep {
				squads += r.MinimumOutputRaw
				sawConversion = true
			}
		case KaminoPrimeUSDCRequest:
			_, leg, err := kaminoPrimeUSDCInstruction(r)
			if err != nil {
				return err
			}
			if leg == kaminoLegRepay {
				if !selectorRepayMintBound(effects, route.Kamino.DebtMint) {
					return budgetHold("selector_source_repay_mint_foreign")
				}
				if err := pools.repay(r.AmountRaw); err != nil {
					return err
				}
			}
		default:
			return budgetHold("selector_source_not_an_exit")
		}
	}
	if dropIndex >= 0 {
		plan.Exit = append(plan.Exit[:dropIndex:dropIndex], plan.Exit[dropIndex+1:]...)
	}
	return nil
}
