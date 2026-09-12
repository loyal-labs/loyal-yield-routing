package backyardrwa

import (
	"context"
	"math"
	"math/big"
	"time"
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

func observeWithdrawalExitPolicies(ctx context.Context, rpc *RPCClient, manifest RouteManifest, lane string, slot int64, conversions []Action) (int64, error) {
	// Pins are masked digests: the bridge policies carry their volatile
	// spending-limit spans, and policies without a mask compare as the raw
	// account digest.
	type observedPolicyPin struct {
		digest string
		mask   [][2]int64
	}
	pins := map[string]observedPolicyPin{}
	addresses := []string{reportTicketPDA}
	for _, action := range conversions {
		binding, err := manifest.jupiterPolicyForRoute(action, lane)
		if err != nil {
			return 0, err
		}
		pins[binding.Policy] = observedPolicyPin{digest: binding.PolicyAccountDataSHA256}
		addresses = append(addresses, binding.Policy)
	}
	for _, action := range []Action{StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV} {
		binding, err := manifest.bridgePolicy(action)
		if err != nil {
			return 0, err
		}
		pins[binding.Account] = observedPolicyPin{digest: binding.NormalizedDigest, mask: binding.MaskedByteRanges}
		addresses = append(addresses, binding.Account)
	}
	observed, accounts, err := rpc.GetMultipleAccounts(ctx, addresses, slot)
	if err != nil {
		return 0, budgetHold("withdrawal_exit_policy_observation_unavailable")
	}
	for address, pin := range pins {
		a := accountAt(accounts, address)
		if a.Owner != bridgeSquadsProgram || a.Executable || a.Lamports == 0 || !maskedPolicyDigestMatches(a.Data, pin.mask, pin.digest) {
			return 0, budgetHold("withdrawal_exit_policy_drift")
		}
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
func observePhase3WithdrawalAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, observation Observation, decision Decision, evidence KaminoExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, r := observation.Snapshot, evidence.Request
	plan := phase3BridgeAdmission{Snapshot: s, Decision: decision}
	if !s.Fresh || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagSlots || s.RouteKind != RouteKind ||
		s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.RouteLane != s.StrategyKey ||
		s.RouteLane != decision.StrategyKey || s.RouteLane != r.RouteLane || phase3BudgetFamilyForLane(s.RouteLane) == "" ||
		decision.Action != DeleverRouteStep || r.Action != decision.Action || !s.HasPosition || s.PositionCollateralRaw <= 0 ||
		s.PositionDebtRaw != 0 || s.PositionDebtValueRaw != 0 || s.CollateralIdleRaw < 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || s.DebtIdleRaw < 0 ||
		s.VoltrIdleRaw < 0 || s.SquadsIdleRaw < 0 || s.VoltrStrategyIdleRaw != 0 || r.AmountRaw != uint64(s.PositionCollateralRaw) {
		return plan, budgetHold("complete_position_exit_admission_unavailable")
	}
	_, leg, err := kaminoPrimeUSDCInstruction(r)
	if err != nil || leg != kaminoLegWithdraw {
		return plan, budgetHold("withdrawal_admission_intent_mismatch")
	}
	debit, err := MeasureExecutableDebit(r, evidence.ExpectedEffects)
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
	return pricePhase3CollateralReturn(ctx, rpc, client, manifest, observation, decision, r, evidence.ExpectedEffects, returnRaw, true, nil)
}

// Continue the same return after the withdrawal has reconciled. Both the
// intervening NAV and full collateral/debt-residue-to-USDC swaps use the same
// estimator. Outstanding position debt still requires separate repayment proof.
func observePhase3CollateralReturnAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, observation Observation, decision Decision, request any, effects ExpectedEffects) (phase3BridgeAdmission, error) {
	s := observation.Snapshot
	if !s.Fresh || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagSlots || s.RouteKind != RouteKind ||
		s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.RouteLane != s.StrategyKey || decision.StrategyKey != s.RouteLane ||
		phase3BudgetFamilyForLane(s.RouteLane) == "" || s.HasPosition || s.PositionCollateralRaw != 0 || s.PositionDebtRaw != 0 ||
		s.PositionCollateralValueRaw != 0 || s.PositionDebtValueRaw != 0 || s.DebtIdleRaw < 0 || s.CollateralIdleRaw < 0 ||
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
	default:
		return phase3BridgeAdmission{}, budgetHold("collateral_return_intent_mismatch")
	}
	return pricePhase3CollateralReturn(ctx, rpc, client, manifest, observation, decision, request, effects, uint64(s.CollateralIdleRaw), false, currentSwap)
}

func pricePhase3CollateralReturn(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, observation Observation, decision Decision, request any, effects ExpectedEffects, collateralRaw uint64, reportBeforeSwap bool, currentSwap *JupiterExecutionEvidence) (phase3BridgeAdmission, error) {
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
	policySlot, err := observeWithdrawalExitPolicies(ctx, rpc, manifest, s.RouteLane, s.Slot, conversions)
	if err != nil {
		return plan, err
	}
	var swaps []JupiterExecutionEvidence
	var estimates []uint64
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
			swap, err = prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, d, amount, uint64(s.SquadsIdleRaw)+upperUSDC, policySlot)
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
		estimates = append(estimates, estimate)
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
	tailRequest := BridgeBuildRequest{Action: ReportNAV, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings,
		Report:          BridgeReport{Sequence: uint64(s.Slot), ObservedSlot: uint64(s.Slot), NAVAfterRaw: uint64(post.Snapshot.SquadsIdleRaw), SnapshotDigest: s.ReportSnapshotDigest},
		RecentBlockhash: blockhash, LastValidBlockHeight: height}
	tail, err := observePhase3BridgeAdmission(ctx, rpc, post, tailDecision, BridgeExecutionEvidence{tailRequest, tailEffects})
	if err != nil {
		return plan, err
	}
	current, err := observePhase3KnownBuildCost(ctx, rpc, request, effects)
	if err != nil {
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
		plan.Exit = append(plan.Exit, phase3BridgeExitCost{Action: ReportNAV, Cost: tail.CurrentCost})
	}
	plan.ValidThroughSlot = min(tail.ValidThroughSlot, current.ValidThroughSlot)
	for i, swap := range swaps {
		swapCost, err := observePhase3KnownBuildCost(ctx, rpc, swap.Request, swap.ExpectedEffects)
		if err != nil {
			return plan, err
		}
		swapEffects, err := jsonMarshalExpectedEffects(swap.ExpectedEffects)
		if err != nil {
			return plan, err
		}
		swapInput, err := encodePhase3BuildInput(swap.Request, swapEffects)
		if err != nil {
			return plan, err
		}
		quoted := phase3QuotedExit{Input: swapInput, QuotedOutputRaw: swap.Request.QuotedOutputRaw, EstimatedUpperOutputRaw: estimates[i], ProofLevel: "UNSIGNED_PROSPECTIVE_QUOTE_COST_ESTIMATE_NOT_EXECUTION_OR_OUTPUT_GUARANTEE"}
		if i == 0 {
			plan.QuotedExit = &quoted
		} else {
			plan.AdditionalQuotedExits = append(plan.AdditionalQuotedExits, quoted)
		}
		if currentSwap == nil || currentSwap.Request.Action != swap.Request.Action {
			plan.Exit = append(plan.Exit, phase3BridgeExitCost{Action: swap.Request.Action, Amount: swap.Request.AmountRaw, Cost: swapCost})
		}
		plan.Exit = append(plan.Exit, phase3BridgeExitCost{Action: ReportNAV, Cost: tail.CurrentCost})
		plan.ValidThroughSlot = min(plan.ValidThroughSlot, swapCost.ValidThroughSlot)
	}
	plan.Exit = append(plan.Exit, tail.Exit...)
	for _, step := range plan.Exit {
		plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros, step.Cost.TotalMicros)
		if err != nil {
			return plan, err
		}
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil || slot < policySlot || slot > plan.ValidThroughSlot {
		return plan, budgetHold("stale_withdrawal_exit_admission")
	}
	return plan, nil
}

func (d *Database) admitPhase3CollateralReturn(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, operationID string, observation Observation, decision Decision, request any, effects ExpectedEffects) error {
	plan, err := observePhase3CollateralReturnAdmission(ctx, rpc, client, manifest, observation, decision, request, effects)
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmission(ctx, rpc, operationID, observation, decision, plan)
}

func (d *Database) admitPhase3Withdrawal(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, operationID string, observation Observation, decision Decision, evidence KaminoExecutionEvidence) error {
	var plan phase3BridgeAdmission
	var err error
	if evidence.Request.RepaymentRelease {
		plan, err = observePhase3FundingAdmission(ctx, rpc, client, manifest, observation, decision, evidence.Request, evidence.ExpectedEffects)
	} else if evidence.Request.FullPayoff {
		plan, err = observePhase3PayoffAdmission(ctx, rpc, client, manifest, observation, decision, evidence)
	} else {
		plan, err = observePhase3WithdrawalAdmission(ctx, rpc, client, manifest, observation, decision, evidence)
	}
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmission(ctx, rpc, operationID, observation, decision, plan)
}
