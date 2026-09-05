package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"math"
	"math/big"
	"time"
)

// Check the enforced quote minimum against debt through swap -> NAV -> payoff,
// not merely against principal or optimistic quote output. Called again when
// pricing the persisted actual swap at final send. No future template is signed.
func validatePayoffFunding(ctx context.Context, rpc *RPCClient, request JupiterSwapRequest, effects ExpectedEffects, slot, steps int64) (KaminoPayoffBound, []ConfirmedAccount, error) {
	if !request.FullPayoffFunding || request.Action != SwapCollateralToDebtStep {
		return KaminoPayoffBound{}, nil, budgetHold("invalid_full_payoff_funding_intent")
	}
	if _, err := MeasureExecutableDebit(request, effects); err != nil {
		return KaminoPayoffBound{}, nil, err
	}
	route, err := runtimeRoute(request.RouteLane)
	if err != nil {
		return KaminoPayoffBound{}, nil, err
	}
	bound, accounts, err := observeKaminoPayoffWindow(ctx, rpc, route, slot, steps)
	if err != nil {
		return bound, nil, err
	}
	return validatePayoffFundingAccounts(request, effects, bound, accounts, route)
}

func validatePayoffFundingAccounts(request JupiterSwapRequest, effects ExpectedEffects, bound KaminoPayoffBound, accounts []ConfirmedAccount, route RuntimeRoute) (KaminoPayoffBound, []ConfirmedAccount, error) {
	if !request.FullPayoffFunding || request.Action != SwapCollateralToDebtStep || request.RouteLane != route.Lane {
		return bound, nil, budgetHold("invalid_full_payoff_funding_intent")
	}
	if _, err := MeasureExecutableDebit(request, effects); err != nil {
		return bound, nil, err
	}
	if len(effects.Accounts) != 2 {
		return bound, nil, budgetHold("funding_custody_mismatch")
	}
	for i, identity := range []struct{ address, mint string }{{route.CollateralCustody, route.Kamino.CollateralMint}, {route.DebtCustody, route.Kamino.DebtMint}} {
		e := effects.Accounts[i]
		a := accountAt(accounts, identity.address)
		mint, _ := decodeBase58PublicKey(identity.mint)
		authority, _ := decodeBase58PublicKey(bridgeVault)
		custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, authority)
		if err != nil || a.Executable || a.Lamports == 0 || e.Address != identity.address || e.Mint != identity.mint || e.Owner != a.Owner || e.Authority != bridgeVault || e.BeforeRaw != custody.Raw {
			return bound, nil, budgetHold("funding_custody_changed")
		}
	}
	source, destination := effects.Accounts[0], effects.Accounts[1]
	// The legacy wire carries quoted output and slippage, not the JSON
	// threshold. Derive its lower bound with wide arithmetic. Use a floor for
	// funding even though Jupiter rounds its actual minimum up.
	binding, err := catalogJupiterBindingForRoute(request.Action, request.RouteLane)
	if err != nil {
		return bound, nil, err
	}
	wire, err := base64.StdEncoding.Strict().DecodeString(request.Instruction.Data)
	if err != nil {
		return bound, nil, err
	}
	slippage := binary.LittleEndian.Uint16(wire[binding.SlippageOffset:])
	n := new(big.Int).Mul(new(big.Int).SetUint64(request.QuotedOutputRaw), new(big.Int).SetUint64(uint64(10_000-slippage)))
	floor := new(big.Int).Quo(new(big.Int).Set(n), big.NewInt(10_000)).Uint64()
	ceiling := n.Add(n, big.NewInt(9_999)).Quo(n, big.NewInt(10_000)).Uint64()
	minimum := min(request.MinimumOutputRaw, floor)
	if source.BeforeRaw != request.AmountRaw || source.AfterRaw != 0 || request.MinimumOutputRaw > math.MaxInt64 || request.MinimumOutputRaw > ceiling || destination.BeforeRaw > math.MaxInt64-request.MinimumOutputRaw ||
		destination.MinimumAfterRaw == nil || *destination.MinimumAfterRaw != destination.BeforeRaw+request.MinimumOutputRaw ||
		destination.AfterRaw != *destination.MinimumAfterRaw || destination.BeforeRaw+minimum < bound.UpperDebtRaw {
		return bound, nil, budgetHold("funding_quote_cannot_cover_full_payoff")
	}
	return bound, accounts, nil
}

// Covers the actual funding swap, its preceding NAV, and the NAV after funding.
// Reserve payoff, remaining collateral withdrawal, all residue and bridge/NAV
// steps together. All projected balances below are cost-only, never RPC writes.
func observePhase3FundingAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, observation Observation, decision Decision, request any, effects ExpectedEffects) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s := observation.Snapshot
	original := s
	if !s.Fresh || s.Slot <= 0 || s.RouteKind != RouteKind || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission ||
		!s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionDebtRaw <= 0 || s.PositionDebtValueRaw <= 0 ||
		s.RouteLane != s.StrategyKey || s.RouteLane != decision.StrategyKey || !catalogJupiterRoute(s.RouteLane) ||
		s.CollateralIdleRaw < 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || s.DebtIdleRaw < 0 || s.VoltrStrategyIdleRaw != 0 || s.SquadsIdleRaw < 0 || s.VoltrIdleRaw < 0 {
		return phase3BridgeAdmission{}, budgetHold("complete_funding_return_unavailable")
	}
	var funding *JupiterExecutionEvidence
	var release *KaminoExecutionEvidence
	var releaseBound KaminoReleaseBound
	var releaseAccounts []ConfirmedAccount
	var currentSwap bool
	steps := int64(2) // current NAV -> payoff
	switch r := request.(type) {
	case KaminoPrimeUSDCRequest:
		if !r.RepaymentRelease || r.Action != DeleverRouteStep || decision.Action != r.Action || decision.Reason != "withdrawal_release_repayment_collateral" || r.RouteLane != s.RouteLane || s.CollateralIdleRaw != 0 || r.AmountRaw >= uint64(s.PositionCollateralRaw) || r.ReleaseDebtIdleRaw != uint64(s.DebtIdleRaw) {
			return phase3BridgeAdmission{}, budgetHold("release_return_intent_mismatch")
		}
		var err error
		releaseBound, releaseAccounts, err = validateRepaymentReleaseRequest(ctx, rpc, r, effects, s.Slot)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		route, _ := runtimeRoute(s.RouteLane)
		obligation, err := decodeKaminoObligation(accountAt(releaseAccounts, route.Kamino.Obligation), route.Kamino)
		if err != nil || obligation.collateralDepositedRaw != uint64(s.PositionCollateralRaw) {
			return phase3BridgeAdmission{}, budgetHold("release_position_snapshot_changed")
		}
		debit, err := MeasureExecutableDebit(r, effects)
		if err != nil || debit.Raw > math.MaxInt64 || effects.Accounts[1].BeforeRaw != 0 {
			return phase3BridgeAdmission{}, budgetHold("release_custody_snapshot_changed")
		}
		release = &KaminoExecutionEvidence{r, effects}
		s.PositionCollateralRaw -= int64(r.AmountRaw)
		s.CollateralIdleRaw, s.PrimeIdleRaw = int64(debit.Raw), int64(debit.Raw)
		// This is cost-only position metadata, not a reportable NAV.
		remainingValue := new(big.Int).Mul(big.NewInt(s.PositionCollateralValueRaw), big.NewInt(s.PositionCollateralRaw))
		s.PositionCollateralValueRaw = remainingValue.Quo(remainingValue, big.NewInt(original.PositionCollateralRaw)).Int64()
		quote, err := prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, Decision{Action: SwapCollateralToDebtStep, AmountRaw: s.CollateralIdleRaw, StrategyKey: s.RouteLane}, uint64(s.CollateralIdleRaw), uint64(s.DebtIdleRaw), releaseBound.Payoff.ObservedSlot)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		quote.Request.FullPayoffFunding = true
		funding, steps = &quote, 5
	case JupiterSwapRequest:
		if !r.FullPayoffFunding || r.Action != SwapCollateralToDebtStep || decision.Action != r.Action || r.RouteLane != s.RouteLane || decision.AmountRaw != s.CollateralIdleRaw || r.AmountRaw != uint64(s.CollateralIdleRaw) {
			return phase3BridgeAdmission{}, budgetHold("funding_return_intent_mismatch")
		}
		funding, currentSwap, steps = &JupiterExecutionEvidence{r, effects}, true, 3
	case BridgeBuildRequest:
		if r.Action != ReportNAV || decision.Action != ReportNAV || r.AmountRaw != 0 || decision.AmountRaw != 0 || r.Report.ObservedSlot != uint64(s.Slot) || r.Report.Sequence != uint64(s.Slot) {
			return phase3BridgeAdmission{}, budgetHold("funding_nav_intent_mismatch")
		}
		if s.CollateralIdleRaw > 0 {
			quote, err := prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, Decision{Action: SwapCollateralToDebtStep, AmountRaw: s.CollateralIdleRaw, StrategyKey: s.RouteLane}, uint64(s.CollateralIdleRaw), uint64(s.DebtIdleRaw), s.Slot)
			if err != nil {
				return phase3BridgeAdmission{}, err
			}
			quote.Request.FullPayoffFunding = true
			funding, steps = &quote, 4 // NAV -> swap -> NAV -> payoff
		}
	default:
		return phase3BridgeAdmission{}, budgetHold("funding_return_intent_mismatch")
	}
	route, _ := runtimeRoute(s.RouteLane)
	var bound KaminoPayoffBound
	var accounts []ConfirmedAccount
	var err error
	if release != nil {
		bound = releaseBound.Payoff
		accounts = append([]ConfirmedAccount(nil), releaseAccounts...)
		for i, a := range accounts {
			if a.Address == route.CollateralCustody {
				accounts[i].Data = append([]byte(nil), a.Data...)
				binary.LittleEndian.PutUint64(accounts[i].Data[64:72], uint64(s.CollateralIdleRaw))
			}
		}
		bound, accounts, err = validatePayoffFundingAccounts(funding.Request, funding.ExpectedEffects, bound, accounts, route)
	} else if funding != nil {
		bound, accounts, err = validatePayoffFunding(ctx, rpc, funding.Request, funding.ExpectedEffects, s.Slot, steps)
	} else {
		bound, accounts, err = observeKaminoPayoffWindow(ctx, rpc, route, s.Slot, steps)
	}
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if bound.ObservedDebtRaw != uint64(s.PositionDebtRaw) {
		return phase3BridgeAdmission{}, budgetHold("funding_debt_snapshot_changed")
	}
	upperCash := uint64(s.DebtIdleRaw)
	if funding != nil {
		if funding.ExpectedEffects.Accounts[1].BeforeRaw != upperCash {
			return phase3BridgeAdmission{}, budgetHold("funding_debt_custody_changed")
		}
		upper, err := withdrawalUSDCExitEstimate(funding.Request.QuotedOutputRaw)
		if err != nil || upper > math.MaxInt64-upperCash {
			return phase3BridgeAdmission{}, budgetHold("funding_output_estimate_overflow")
		}
		upperCash += upper
	}
	if upperCash < bound.UpperDebtRaw {
		return phase3BridgeAdmission{}, budgetHold("funding_payoff_cash_insufficient")
	}
	// Validate actual debt custody even for the NAV after a reconciled swap.
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	a := accountAt(accounts, route.DebtCustody)
	mint, _ := decodeBase58PublicKey(route.Kamino.DebtMint)
	authority, _ := decodeBase58PublicKey(bridgeVault)
	custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, authority)
	if err != nil || a.Executable || a.Lamports == 0 || custody.Raw != uint64(s.DebtIdleRaw) {
		return phase3BridgeAdmission{}, budgetHold("funding_debt_custody_changed")
	}
	projected := append([]ConfirmedAccount(nil), accounts...)
	for i, a := range projected {
		if a.Address == route.DebtCustody {
			projected[i].Data = append([]byte(nil), a.Data...)
			binary.LittleEndian.PutUint64(projected[i].Data[64:72], upperCash)
		}
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	payoff, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegRepay, bound.UpperDebtRaw, blockhash, s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	payoff.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	// Cost-only future wire deliberately has no FullPayoff assertion. Actual
	// repayment must be rebuilt from observed custody and re-admitted as such.
	payoffEffects, err := boundedKaminoRepaymentEffects(projected, source, destination, bound.ObservedDebtRaw, bound.UpperDebtRaw)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	_, policies, err := rpc.GetMultipleAccounts(ctx, []string{payoff.Policy}, bound.ObservedSlot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	p := accountAt(policies, payoff.Policy)
	if p.Owner != bridgeSquadsProgram || p.Executable || p.Lamports == 0 || sha256Bytes(p.Data) != payoff.PolicyAccountDataSHA256 {
		return phase3BridgeAdmission{}, budgetHold("funding_payoff_policy_drift")
	}
	post := observation
	post.Snapshot = s
	post.Snapshot.PositionDebtRaw, post.Snapshot.PositionDebtValueRaw, post.Snapshot.PayoffDebtRaw = 0, 0, 0
	post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = 0, 0
	post.Snapshot.DebtIdleRaw = int64(upperCash - bound.ObservedDebtRaw)
	plan, err := pricePhase3PositionReturnAfterFunding(ctx, rpc, client, manifest, post, decision, request, effects, true, funding, release)
	if err != nil {
		return plan, err
	}
	payoffCost, err := observePhase3KnownBuildCost(ctx, rpc, payoff, payoffEffects)
	if err != nil {
		return plan, err
	}
	encoded, err := jsonMarshalExpectedEffects(payoffEffects)
	if err != nil {
		return plan, err
	}
	plan.PayoffRepayment, err = encodePhase3BuildInput(payoff, encoded)
	if err != nil {
		return plan, err
	}
	prefix := []phase3BridgeExitCost{}
	if release != nil {
		prefix = append(prefix, phase3BridgeExitCost{Action: ReportNAV, Cost: plan.Exit[0].Cost})
	}
	if funding != nil {
		policySlot, err := observeWithdrawalExitPolicies(ctx, rpc, manifest, s.RouteLane, s.Slot, []Action{SwapCollateralToDebtStep})
		if err != nil {
			return plan, err
		}
		cost := plan.CurrentCost
		if !currentSwap {
			costRequest := funding.Request
			if release != nil {
				costRequest.FullPayoffFunding = false
			} // future custody; never the persisted current wire
			cost, err = observePhase3KnownBuildCost(ctx, rpc, costRequest, funding.ExpectedEffects)
			if err != nil {
				return plan, err
			}
		}
		encoded, err := jsonMarshalExpectedEffects(funding.ExpectedEffects)
		if err != nil {
			return plan, err
		}
		input, err := encodePhase3BuildInput(funding.Request, encoded)
		if err != nil {
			return plan, err
		}
		plan.FundingSwap = &phase3QuotedExit{Input: input, QuotedOutputRaw: funding.Request.QuotedOutputRaw, EstimatedUpperOutputRaw: upperCash - uint64(s.DebtIdleRaw), ProofLevel: "COST_ONLY_FUNDING_AND_RESIDUE_ESTIMATE_NOT_EXECUTION"}
		if !currentSwap {
			prefix = append(prefix, phase3BridgeExitCost{Action: SwapCollateralToDebtStep, Amount: funding.Request.AmountRaw, Cost: cost})
		}
		prefix = append(prefix, phase3BridgeExitCost{Action: ReportNAV, Cost: plan.Exit[0].Cost})
		plan.ValidThroughSlot = min(plan.ValidThroughSlot, cost.ValidThroughSlot)
		if policySlot > plan.ValidThroughSlot {
			return plan, budgetHold("stale_funding_exit_admission")
		}
	}
	prefix = append(prefix, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: payoff.AmountRaw, Cost: payoffCost})
	plan.Exit = append(prefix, plan.Exit...)
	plan.ExitAfterMicros = 0
	for _, step := range plan.Exit {
		plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros, step.Cost.TotalMicros)
		if err != nil {
			return plan, err
		}
	}
	plan.Snapshot, plan.Payoff = original, &bound
	plan.ValidThroughSlot = min(plan.ValidThroughSlot, payoffCost.ValidThroughSlot, bound.ObservedSlot+budgetMaxObservationLagSlots)
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil || slot > plan.ValidThroughSlot {
		return plan, budgetHold("stale_funding_exit_admission")
	}
	return plan, nil
}

func (d *Database) admitPhase3Funding(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, operationID string, observation Observation, decision Decision, request any, effects ExpectedEffects) error {
	plan, err := observePhase3FundingAdmission(ctx, rpc, client, manifest, observation, decision, request, effects)
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmission(ctx, rpc, operationID, observation, decision, plan)
}
