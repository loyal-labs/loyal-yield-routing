package backyard

import (
	"context"
	"math"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// Quote outputs are estimates, not enforceable maxima. Reserve the existing
// two-sided price margin around the quoted USDC output for later full-custody
// transfers. Every actual leg is reobserved/repriced; an out-of-bound poststate
// is a recovery HOLD, never an instruction to truncate the full restoration.
func withdrawalUSDCExitEstimate(quoted uint64) (uint64, error) {
	if quoted == 0 {
		return 0, budgetHold("empty_withdrawal_exit_quote")
	}
	n := new(big.Int).Mul(new(big.Int).SetUint64(quoted), big.NewInt(int64(10_000+budgetPriceMarginBPS)))
	d := int64(10_000 - budgetPriceMarginBPS)
	n.Add(n, big.NewInt(d-1)).Quo(n, big.NewInt(d))
	if !n.IsUint64() || n.Uint64() > math.MaxInt64 {
		return 0, budgetHold("withdrawal_exit_estimate_overflow")
	}
	return n.Uint64(), nil
}

// observeDisarmedReportTicket reads the report ticket disarmed at slot or
// later and returns that read's slot.
func observeDisarmedReportTicket(ctx context.Context, view *View, slot int64) (int64, error) {
	observed, accounts, _, err := view.read(ctx, []string{reportTicketPDA}, slot)
	if err != nil {
		return 0, budgetHold("report_ticket_observation_unavailable")
	}
	ticket, err := decodeObservedReportTicket(accountAt(accounts, reportTicketPDA))
	if err != nil || ticket.Armed {
		return 0, budgetHold("withdrawal_exit_ticket_unavailable")
	}
	return observed, nil
}

// Price a complete debt-free withdrawal -> NAV -> collateral/USDC conversion
// -> NAV -> staging -> NAV -> full restoration -> NAV return. This is recovery
// admission, not a substitute for pricing entry, borrowing or debt repayment.
// Future packets are cost templates only; none become the persisted current wire.
func observePhase3WithdrawalAdmission(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, observation Observation, decision Decision, evidence KaminoExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, r := observation.Snapshot, evidence.Request
	plan := phase3BridgeAdmission{Snapshot: s, Decision: decision}
	if !s.Fresh || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.RouteKind != RouteKind ||
		s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.RouteLane != s.StrategyKey ||
		s.RouteLane != decision.StrategyKey || s.RouteLane != r.RouteLane || !fundedLane(s.RouteLane) ||
		decision.Action != DeleverRouteStep || r.Action != decision.Action || !s.HasPosition || s.PositionCollateralRaw <= 0 ||
		s.PositionDebtRaw != 0 || s.PositionDebtValueRaw != 0 || s.CollateralIdleRaw < 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || s.DebtIdleRaw < 0 ||
		s.VoltrIdleRaw < 0 || s.SquadsIdleRaw < 0 || s.VoltrStrategyIdleRaw != 0 || r.AmountRaw != uint64(s.PositionCollateralRaw) {
		return plan, budgetHold("complete_position_exit_admission_unavailable")
	}
	_, leg, err := kaminoPrimeUSDCInstruction(r)
	if err != nil || leg != kaminoLegWithdraw {
		return plan, budgetHold("withdrawal_admission_intent_mismatch")
	}
	debit, err := manifest.measureExecutableDebit(r, evidence.ExpectedEffects)
	if err != nil {
		return plan, err
	}
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return plan, err
	}
	if debit.Raw == 0 || debit.Raw > uint64(math.MaxInt64-s.CollateralIdleRaw) {
		return plan, budgetHold("withdrawal_admission_amount_unavailable")
	}
	// Include both withdrawal proceeds and collateral already in custody (for
	// example deposit rounding residue) in the complete conversion and return.
	returnRaw := uint64(s.CollateralIdleRaw) + debit.Raw
	var destinationOK bool
	for _, effect := range evidence.ExpectedEffects.Accounts {
		if effect.Address == route.CollateralCustody && effect.Authority == bridgeVault && effect.Mint == route.Kamino.CollateralMint &&
			effect.BeforeRaw == uint64(s.CollateralIdleRaw) && effect.AfterRaw == returnRaw && effect.MinimumAfterRaw == nil {
			destinationOK = true
		}
	}
	if !destinationOK {
		return plan, budgetHold("withdrawal_admission_custody_mismatch")
	}
	return pricePhase3CollateralReturn(ctx, rpc, view, client, manifest, observation, decision, r, evidence.ExpectedEffects, returnRaw, true, nil)
}

// Continue the same return after the withdrawal has reconciled. Both the
// intervening NAV and full collateral/debt-residue-to-USDC swaps use the same
// estimator. Outstanding position debt still requires separate repayment proof.
func observePhase3CollateralReturnAdmission(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, observation Observation, decision Decision, request any, effects ExpectedEffects) (phase3BridgeAdmission, error) {
	s := observation.Snapshot
	// A payoff residue converted beside a debt-free position (plan B3) keeps
	// that position; its complete return is priced after the swap below.
	residue := decision.Reason == debtResidueSwapReason && decision.Action == SwapDebtToUSDCStep && s.HasPosition &&
		s.PositionCollateralRaw > 0 && s.PositionDebtRaw == 0 && s.PositionDebtValueRaw == 0 && s.CollateralIdleRaw == 0 &&
		s.WithdrawalDemandRaw == 0 && !s.Unwind && !s.CutoverDrain
	if !s.Fresh || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.RouteKind != RouteKind ||
		s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.RouteLane != s.StrategyKey || decision.StrategyKey != s.RouteLane ||
		!fundedLane(s.RouteLane) || (!residue && (s.HasPosition || s.PositionCollateralRaw != 0 || s.PositionDebtRaw != 0 ||
		s.PositionCollateralValueRaw != 0 || s.PositionDebtValueRaw != 0)) || s.DebtIdleRaw < 0 || s.CollateralIdleRaw < 0 ||
		(s.CollateralIdleRaw == 0 && s.DebtIdleRaw == 0) ||
		s.PrimeIdleRaw != s.CollateralIdleRaw || s.VoltrStrategyIdleRaw != 0 || s.SquadsIdleRaw < 0 || s.VoltrIdleRaw < 0 {
		return phase3BridgeAdmission{}, budgetHold("complete_collateral_return_admission_unavailable")
	}
	var currentSwap *JupiterExecutionEvidence
	switch r := request.(type) {
	case BridgeBuildRequest:
		if r.Action != ReportNAV || decision.Action != r.Action || decision.AmountRaw != 0 || r.AmountRaw != 0 ||
			r.Report.ObservedSlot != uint64(s.Slot) || r.Report.Sequence != uint64(s.Slot) {
			return phase3BridgeAdmission{}, budgetHold("collateral_return_intent_mismatch")
		}
	case JupiterSwapRequest:
		amount := s.CollateralIdleRaw
		if r.Action == SwapDebtToUSDCStep && s.CollateralIdleRaw == 0 {
			amount = s.DebtIdleRaw
		} else if r.Action != SwapCollateralToStableStep {
			return phase3BridgeAdmission{}, budgetHold("collateral_return_intent_mismatch")
		}
		if amount <= 0 || decision.Action != r.Action || decision.AmountRaw != amount ||
			r.RouteLane != s.RouteLane || r.AmountRaw != uint64(amount) {
			return phase3BridgeAdmission{}, budgetHold("collateral_return_intent_mismatch")
		}
		source, destination, sourceATA, destinationATA, err := jupiterEdgeForRoute(r.Action, r.RouteLane)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		var sourceOK, destinationOK bool
		for _, e := range effects.Accounts {
			if e.Address == sourceATA && e.Mint == source && e.Authority == bridgeVault && e.BeforeRaw == r.AmountRaw && e.AfterRaw == 0 {
				sourceOK = true
			}
			if e.Address == destinationATA && e.Mint == destination && e.Authority == bridgeVault && e.BeforeRaw == uint64(s.SquadsIdleRaw) {
				destinationOK = true
			}
		}
		if !sourceOK || !destinationOK {
			return phase3BridgeAdmission{}, budgetHold("collateral_return_custody_mismatch")
		}
		currentSwap = &JupiterExecutionEvidence{r, effects}
		if residue {
			return pricePhase3DebtResidueSwapReturn(ctx, rpc, view, client, manifest, observation, decision, r, effects)
		}
	default:
		return phase3BridgeAdmission{}, budgetHold("collateral_return_intent_mismatch")
	}
	if residue {
		return phase3BridgeAdmission{}, budgetHold("collateral_return_intent_mismatch")
	}
	return pricePhase3CollateralReturn(ctx, rpc, view, client, manifest, observation, decision, request, effects, uint64(s.CollateralIdleRaw), false, currentSwap)
}

// The residue swap keeps the debt-free position. Reserve the NAV after the
// swap plus the complete position return from the post-swap custody: the
// quoted USDC (with the two-sided margin) joins bridge cash, debt custody is
// empty. Cost-only; the swap itself is the only current wire.
func pricePhase3DebtResidueSwapReturn(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, observation Observation, decision Decision, r JupiterSwapRequest, effects ExpectedEffects) (phase3BridgeAdmission, error) {
	s := observation.Snapshot
	if r.AmountRaw != uint64(s.DebtIdleRaw) || r.FullPayoffFunding || r.EntryReturnReserved || r.PositionReturnReserved {
		return phase3BridgeAdmission{}, budgetHold("collateral_return_intent_mismatch")
	}
	upper, err := withdrawalUSDCExitEstimate(r.QuotedOutputRaw)
	if err != nil || upper > uint64(math.MaxInt64-s.SquadsIdleRaw) {
		return phase3BridgeAdmission{}, budgetHold("withdrawal_exit_estimate_overflow")
	}
	post := observation
	post.Snapshot.DebtIdleRaw = 0
	post.Snapshot.SquadsIdleRaw += int64(upper)
	plan, err := pricePhase3PositionReturn(ctx, rpc, view, client, manifest, post, decision, r, effects, true)
	if err != nil {
		return plan, err
	}
	plan.Snapshot = s
	return plan, nil
}

func pricePhase3CollateralReturn(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, observation Observation, decision Decision, request any, effects ExpectedEffects, collateralRaw uint64, reportBeforeSwap bool, currentSwap *JupiterExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s := observation.Snapshot
	plan := phase3BridgeAdmission{Snapshot: s, Decision: decision}
	if rpc == nil || (client == nil && currentSwap == nil) {
		return plan, budgetHold("withdrawal_exit_valuation_unavailable")
	}
	conversions := []Action{}
	if collateralRaw > 0 {
		conversions = append(conversions, SwapCollateralToStableStep)
	}
	if s.DebtIdleRaw > 0 {
		conversions = append(conversions, SwapDebtToUSDCStep)
	}
	if len(conversions) == 0 {
		return plan, budgetHold("empty_custody_return")
	}
	policySlot, err := observeDisarmedReportTicket(ctx, view, s.Slot)
	if err != nil {
		return plan, err
	}
	var swaps []JupiterExecutionEvidence
	var upperUSDC uint64
	for _, action := range conversions {
		amount := collateralRaw
		if action == SwapDebtToUSDCStep {
			amount = uint64(s.DebtIdleRaw)
		}
		var swap JupiterExecutionEvidence
		if currentSwap != nil && currentSwap.Request.Action == action {
			swap = *currentSwap
		} else {
			if client == nil {
				return plan, budgetHold("withdrawal_exit_quote_unavailable")
			}
			d := Decision{Action: action, AmountRaw: int64(amount), StrategyKey: s.RouteLane}
			swap, err = prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, observation.policies, d, amount, uint64(s.SquadsIdleRaw)+upperUSDC, policySlot)
			if err != nil {
				return plan, budgetHold("withdrawal_exit_quote_unavailable")
			}
		}
		estimate, err := withdrawalUSDCExitEstimate(swap.Request.QuotedOutputRaw)
		if err != nil || estimate > uint64(math.MaxInt64-s.SquadsIdleRaw)-upperUSDC {
			return plan, budgetHold("withdrawal_exit_estimate_overflow")
		}
		upperUSDC += estimate
		swaps = append(swaps, swap)
	}
	post := observation
	post.Snapshot.HasPosition = false
	post.Snapshot.PositionCollateralRaw, post.Snapshot.PositionCollateralValueRaw = 0, 0
	post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = 0, 0
	post.Snapshot.DebtIdleRaw = 0
	post.Snapshot.CutoverDrain = false // cost-only bridge state, not a queue change
	post.Snapshot.SquadsIdleRaw += int64(upperUSDC)
	post.Snapshot.StrategyNAVRaw = post.Snapshot.SquadsIdleRaw
	tailDecision := Decision{Action: ReportNAV, StrategyKey: s.RouteLane}
	tailEffects, _, _, err := bridgeExpectedEffects(tailDecision, uint64(s.VoltrIdleRaw), 0, uint64(post.Snapshot.SquadsIdleRaw))
	if err != nil {
		return plan, err
	}
	tailEffects.Kind = "bridge"
	tailEffects.ReturnData = expectedAdaptorReturnData(uint64(post.Snapshot.SquadsIdleRaw))
	var blockhash string
	var height int64
	switch r := request.(type) {
	case KaminoPrimeUSDCRequest:
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case BridgeBuildRequest:
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case JupiterSwapRequest:
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	}
	navPolicy, err := observation.policies.account(policyKey{action: ReportNAV})
	if err != nil {
		return plan, err
	}
	tailRequest := BridgeBuildRequest{Action: ReportNAV, Policy: navPolicy, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings,
		Report:          BridgeReport{Sequence: uint64(s.Slot), ObservedSlot: uint64(s.Slot), NAVAfterRaw: uint64(post.Snapshot.SquadsIdleRaw), SnapshotDigest: s.ReportSnapshotDigest},
		RecentBlockhash: blockhash, LastValidBlockHeight: height}
	// The tail NAV, the current wire and every swap leg are priced by
	// independent reads: run them at once.
	var tail phase3BridgeAdmission
	var current ValuedTransactionCost
	swapCosts := make([]ValuedTransactionCost, len(swaps))
	reads := []func(context.Context) error{func(ctx context.Context) (err error) {
		tail, err = observePhase3BridgeTemplateAdmission(ctx, rpc, view, post, tailDecision, BridgeExecutionEvidence{tailRequest, tailEffects})
		return err
	}, func(ctx context.Context) (err error) {
		current, err = manifest.observePhase3KnownBuildCost(ctx, rpc, view, request, effects)
		return err
	}}
	for i, swap := range swaps {
		reads = append(reads, func(ctx context.Context) (err error) {
			swapCosts[i], err = manifest.observePhase3KnownBuildCost(ctx, rpc, view, swap.Request, swap.ExpectedEffects)
			return err
		})
	}
	if err = concurrentReads(ctx, reads...); err != nil {
		return plan, err
	}
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return plan, err
	}
	plan.Input, err = encodePhase3BuildInput(request, encoded)
	if err != nil {
		return plan, err
	}
	plan.CurrentCost = current
	// NAV's compiled account graph and fixed-width payload have the same fee
	// before and after the swap. This is fee equivalence, not a NAV value proof.
	if reportBeforeSwap {
		plan.Exit = append(plan.Exit, phase3BridgeExitCost{Action: ReportNAV, Cost: tail.CurrentCost, Template: tail.Input})
	}
	plan.ValidThroughSlot = min(tail.ValidThroughSlot, current.ValidThroughSlot)
	for i, swap := range swaps {
		swapCost := swapCosts[i]
		swapEffects, err := jsonMarshalExpectedEffects(swap.ExpectedEffects)
		if err != nil {
			return plan, err
		}
		swapInput, err := encodePhase3BuildInput(swap.Request, swapEffects)
		if err != nil {
			return plan, err
		}
		if currentSwap == nil || currentSwap.Request.Action != swap.Request.Action {
			plan.Exit = append(plan.Exit, phase3BridgeExitCost{Action: swap.Request.Action, Amount: swap.Request.AmountRaw, Cost: swapCost, Template: swapInput})
		}
		plan.Exit = append(plan.Exit, phase3BridgeExitCost{Action: ReportNAV, Cost: tail.CurrentCost, Template: tail.Input})
		plan.ValidThroughSlot = min(plan.ValidThroughSlot, swapCost.ValidThroughSlot)
	}
	plan.Exit = append(plan.Exit, tail.Exit...)
	for _, step := range plan.Exit {
		plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros, step.Cost.TotalMicros)
		if err != nil {
			return plan, err
		}
	}
	slot, err := view.slot(ctx)
	if err != nil || slot > plan.ValidThroughSlot {
		return plan, budgetHold("stale_withdrawal_exit_admission")
	}
	return plan, nil
}
