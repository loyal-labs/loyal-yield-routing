package backyardrwa

import (
	"bytes"
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
func isPayoffFundingAction(action Action) bool {
	return action == SwapCollateralToDebtStep || action == SwapUSDCToDebtStep
}

// validatePayoffFunding keeps the explicit reviewed manifest so an AUTO
// funding swap measures through the same binding that produced it; existing
// lanes resolve identically through either manifest.
func validatePayoffFunding(ctx context.Context, rpc *RPCClient, manifest RouteManifest, request JupiterSwapRequest, effects ExpectedEffects, slot, steps int64, refreshedBasis bool) (KaminoPayoffBound, []ConfirmedAccount, error) {
	if !request.FullPayoffFunding || !isPayoffFundingAction(request.Action) {
		return KaminoPayoffBound{}, nil, budgetHold("invalid_full_payoff_funding_intent")
	}
	if _, err := manifest.measureExecutableDebit(request, effects); err != nil {
		return KaminoPayoffBound{}, nil, err
	}
	route, err := runtimeRoute(request.RouteLane)
	if err != nil {
		return KaminoPayoffBound{}, nil, err
	}
	var additional []string
	if request.Action == SwapUSDCToDebtStep {
		additional = []string{bridgeSquadsATA}
	}
	bound, accounts, err := observePayoffWindowOnSnapshotBasis(ctx, rpc, route, slot, steps, refreshedBasis, additional...)
	if err != nil {
		return bound, nil, err
	}
	return validatePayoffFundingAccounts(manifest, request, effects, bound, accounts, route)
}

// observePayoffWindowOnSnapshotBasis prices the payoff window on the same
// reserve basis the decision snapshot used. The route observer derives
// Snapshot.PositionDebtRaw from the unsigned reserve-refresh simulation bank
// whenever raw reserves are health-stale, so a strict comparison against a
// raw re-capture would conflate the reserves' accrued rate basis with a real
// obligation mutation and refuse funding after ordinary accrual crossed one
// whole-unit ceil boundary.
func observePayoffWindowOnSnapshotBasis(ctx context.Context, rpc *RPCClient, route RuntimeRoute, minimumSlot, steps int64, refreshedBasis bool, additional ...string) (KaminoPayoffBound, []ConfirmedAccount, error) {
	if refreshedBasis {
		return observeKaminoPayoffWindowOnSnapshotBasis(ctx, rpc, route, minimumSlot, steps, additional...)
	}
	return observeKaminoPayoffWindowAccounts(ctx, rpc, route, minimumSlot, steps, additional...)
}

func validatePayoffFundingAccounts(manifest RouteManifest, request JupiterSwapRequest, effects ExpectedEffects, bound KaminoPayoffBound, accounts []ConfirmedAccount, route RuntimeRoute) (KaminoPayoffBound, []ConfirmedAccount, error) {
	if !request.FullPayoffFunding || !isPayoffFundingAction(request.Action) || request.RouteLane != route.Lane {
		return bound, nil, budgetHold("invalid_full_payoff_funding_intent")
	}
	if _, err := manifest.measureExecutableDebit(request, effects); err != nil {
		return bound, nil, err
	}
	if len(effects.Accounts) != 2 {
		return bound, nil, budgetHold("funding_custody_mismatch")
	}
	sourceMint, _, sourceATA, _, err := jupiterEdgeForRoute(request.Action, request.RouteLane)
	if err != nil {
		return bound, nil, err
	}
	for i, identity := range []struct{ address, mint string }{{sourceATA, sourceMint}, {route.DebtCustody, route.Kamino.DebtMint}} {
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
	wire, err := base64.StdEncoding.Strict().DecodeString(request.Instruction.Data)
	if err != nil {
		return bound, nil, err
	}
	// Compile validates this lane's actual Jupiter dialect and policy boundaries.
	// Basic USDC edges use the same physical collateral/USDC swap for funding.
	offset := len(wire) - 3
	if request.RouteLane == autoAUTOPYUSD.Lane {
		// The reviewed AUTO binding authorizes only the legacy
		// SharedAccountsRoute dialect — structurally validated here through the
		// explicit reviewed binding — so its slippage byte stays at len-3. The
		// historical catalog entry describes the retired AUTO policy layout
		// and is never consulted for the candidate lane.
		binding, err := manifest.jupiterPolicyForRoute(request.Action, request.RouteLane)
		if err != nil {
			return bound, nil, err
		}
		if _, err := binding.constraintIndex(request.Instruction); err != nil {
			return bound, nil, err
		}
	} else if catalogJupiterRoute(request.RouteLane) {
		binding, err := catalogJupiterBindingForRoute(request.Action, request.RouteLane)
		if err != nil {
			return bound, nil, err
		}
		offset = binding.SlippageOffset
	} else if len(wire) >= 8 && bytes.Equal(wire[:8], jupiterSharedAccountsRouteV2) {
		offset = 25
	}
	if offset < 0 || offset+2 > len(wire) {
		return bound, nil, budgetHold("funding_slippage_encoding_invalid")
	}
	slippage := binary.LittleEndian.Uint16(wire[offset:])
	if slippage > jupiterMaxSlippageBPS {
		return bound, nil, budgetHold("funding_slippage_exceeds_policy")
	}
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
	// The snapshot's position debt is only comparable to a payoff window
	// derived from the same reserve basis it was priced on.
	refreshedBasis := observation.ValuationSource == routeRefreshValuationSource
	if !s.Fresh || s.Slot <= 0 || s.RouteKind != RouteKind || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission ||
		!s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionDebtRaw <= 0 || s.PositionDebtValueRaw <= 0 ||
		s.RouteLane != s.StrategyKey || s.RouteLane != decision.StrategyKey || !positionReturnRoute(s.RouteLane) ||
		s.CollateralIdleRaw < 0 || s.CollateralIdleValueRaw < 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || debtCashRaw(s) < 0 || s.VoltrStrategyIdleRaw != 0 || s.SquadsIdleRaw < 0 || s.VoltrIdleRaw < 0 {
		return phase3BridgeAdmission{}, budgetHold("complete_funding_return_unavailable")
	}
	var funding *JupiterExecutionEvidence
	var release *KaminoExecutionEvidence
	var releaseBound KaminoReleaseBound
	var releaseAccounts []ConfirmedAccount
	var currentSwap bool
	var futureRelease bool
	steps := int64(2) // current NAV -> payoff
	switch r := request.(type) {
	case KaminoPrimeUSDCRequest:
		if !r.RepaymentRelease || r.Action != DeleverRouteStep || decision.Action != r.Action || decision.Reason != "withdrawal_release_repayment_collateral" || r.RouteLane != s.RouteLane || r.AmountRaw >= uint64(s.PositionCollateralRaw) || r.ReleaseDebtIdleRaw != uint64(debtCashRaw(s)) {
			return phase3BridgeAdmission{}, budgetHold("release_return_intent_mismatch")
		}
		var err error
		releaseBound, releaseAccounts, err = manifest.validateRepaymentReleaseRequest(ctx, rpc, r, effects, s.Slot)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		route, _ := runtimeRoute(s.RouteLane)
		obligation, err := decodeKaminoObligation(accountAt(releaseAccounts, route.Kamino.Obligation), route.Kamino)
		if err != nil || obligation.collateralDepositedRaw != uint64(s.PositionCollateralRaw) {
			return phase3BridgeAdmission{}, budgetHold("release_position_snapshot_changed")
		}
		debit, err := manifest.measureExecutableDebit(r, effects)
		if err != nil || debit.Raw > uint64(math.MaxInt64-s.CollateralIdleRaw) || effects.Accounts[1].BeforeRaw != uint64(s.CollateralIdleRaw) {
			return phase3BridgeAdmission{}, budgetHold("release_custody_snapshot_changed")
		}
		release = &KaminoExecutionEvidence{r, effects}
		steps = 5
	case JupiterSwapRequest:
		amount := s.CollateralIdleRaw
		if r.Action == SwapUSDCToDebtStep {
			amount = s.SquadsIdleRaw
		}
		if !r.FullPayoffFunding || !isPayoffFundingAction(r.Action) || decision.Action != r.Action || r.RouteLane != s.RouteLane || amount <= 0 || decision.AmountRaw != amount || r.AmountRaw != uint64(amount) {
			return phase3BridgeAdmission{}, budgetHold("funding_return_intent_mismatch")
		}
		funding, currentSwap, steps = &JupiterExecutionEvidence{r, effects}, true, 3
	case BridgeBuildRequest:
		if r.Action != ReportNAV || decision.Action != ReportNAV || r.AmountRaw != 0 || decision.AmountRaw != 0 || r.Report.ObservedSlot != uint64(s.Slot) || r.Report.Sequence != uint64(s.Slot) {
			return phase3BridgeAdmission{}, budgetHold("funding_nav_intent_mismatch")
		}
		route, _ := runtimeRoute(s.RouteLane)
		// The candidate AUTO pilot release risk model reads the lending
		// market, which the payoff window captures only for installed
		// selector lanes; the reviewed candidate lane rides the same window
		// request so every decode keeps one coherent slot.
		payoffAdditional := []string(nil)
		if route.Lane == autoAUTOPYUSD.Lane {
			payoffAdditional = append(payoffAdditional, route.Kamino.Market)
		}
		future, rows, err := observePayoffWindowOnSnapshotBasis(ctx, rpc, route, s.Slot, 6, refreshedBasis, payoffAdditional...)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		action, amount := payoffFundingSource(s, future.UpperDebtRaw)
		if uint64(debtCashRaw(s)) < future.UpperDebtRaw && amount == 0 {
			// NAV -> release -> NAV -> funding -> NAV -> payoff. This is a
			// future cost template; the release will be rebuilt and admitted
			// from actual custody after NAV, never signed from this projection.
			releaseBound, rows, err = manifest.observeRawRepaymentRelease(ctx, rpc, route, s.Slot, s.PilotActive)
			if err != nil {
				return phase3BridgeAdmission{}, err
			}
			blockhash, err := rpc.LatestBlockhash(ctx)
			if err != nil {
				return phase3BridgeAdmission{}, err
			}
			req, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, releaseBound.ReceiptRaw, blockhash, s.RouteLane)
			if err != nil {
				return phase3BridgeAdmission{}, err
			}
			req.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
			req.RepaymentRelease, req.ReleaseDebtIdleRaw = true, uint64(debtCashRaw(s))
			req.PilotRepaymentRelease = s.PilotActive
			source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
			e, err := exactKaminoTokenEffects(rows, source, destination, releaseBound.LiquidityRaw)
			if err != nil {
				return phase3BridgeAdmission{}, err
			}
			release, releaseAccounts, futureRelease, steps = &KaminoExecutionEvidence{req, e}, rows, true, 6
		}
		if amount > 0 {
			quote, err := prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, Decision{Action: action, AmountRaw: amount, StrategyKey: s.RouteLane}, uint64(amount), uint64(debtCashRaw(s)), s.Slot)
			if err != nil {
				return phase3BridgeAdmission{}, err
			}
			quote.Request.FullPayoffFunding = true
			funding, steps = &quote, 4 // NAV -> swap -> NAV -> payoff
		}
	default:
		return phase3BridgeAdmission{}, budgetHold("funding_return_intent_mismatch")
	}
	if release != nil {
		debit, err := manifest.measureExecutableDebit(release.Request, release.ExpectedEffects)
		if err != nil || debit.Raw > uint64(math.MaxInt64-s.CollateralIdleRaw) || release.ExpectedEffects.Accounts[1].BeforeRaw != uint64(s.CollateralIdleRaw) || release.Request.AmountRaw >= uint64(s.PositionCollateralRaw) {
			return phase3BridgeAdmission{}, budgetHold("release_custody_snapshot_changed")
		}
		s.PositionCollateralRaw -= int64(release.Request.AmountRaw)
		s.CollateralIdleRaw += int64(debit.Raw)
		s.PrimeIdleRaw = s.CollateralIdleRaw
		// Cost-only metadata, not a reportable NAV.
		remainingValue := new(big.Int).Mul(big.NewInt(s.PositionCollateralValueRaw), big.NewInt(s.PositionCollateralRaw))
		s.PositionCollateralValueRaw = remainingValue.Quo(remainingValue, big.NewInt(original.PositionCollateralRaw)).Int64()
		quote, err := prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, Decision{Action: SwapCollateralToDebtStep, AmountRaw: s.CollateralIdleRaw, StrategyKey: s.RouteLane}, uint64(s.CollateralIdleRaw), uint64(debtCashRaw(s)), releaseBound.Payoff.ObservedSlot)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		quote.Request.FullPayoffFunding = true
		funding = &quote
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
		bound, accounts, err = validatePayoffFundingAccounts(manifest, funding.Request, funding.ExpectedEffects, bound, accounts, route)
	} else if funding != nil {
		bound, accounts, err = validatePayoffFunding(ctx, rpc, manifest, funding.Request, funding.ExpectedEffects, s.Slot, steps, refreshedBasis)
	} else {
		bound, accounts, err = observePayoffWindowOnSnapshotBasis(ctx, rpc, route, s.Slot, steps, refreshedBasis)
	}
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if !sameAccruingDebt(bound, s.PositionDebtRaw) {
		return phase3BridgeAdmission{}, budgetHold("funding_debt_snapshot_changed")
	}
	upperCash := uint64(debtCashRaw(s))
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
	if err != nil || a.Executable || a.Lamports == 0 || custody.Raw != uint64(debtCashRaw(s)) {
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
	if funding != nil && funding.Request.Action == SwapCollateralToDebtStep {
		post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw, post.Snapshot.CollateralIdleValueRaw = 0, 0, 0
	}
	if funding != nil && funding.Request.Action == SwapUSDCToDebtStep {
		// Current source custody was checked above. Only the cost-only tail sees
		// USDC consumed by funding; the original snapshot/current wire stay intact.
		post.Snapshot.SquadsIdleRaw = 0
	}
	setDebtCashRaw(&post.Snapshot, int64(upperCash-bound.ObservedDebtRaw))
	plan, err := pricePhase3PositionReturnAfterFunding(ctx, rpc, client, manifest, post, decision, request, effects, true, funding, release)
	if err != nil {
		return plan, err
	}
	payoffCost, err := manifest.observePhase3KnownBuildCost(ctx, rpc, payoff, payoffEffects)
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
		if futureRelease {
			cost, err := manifest.observePhase3KnownBuildCost(ctx, rpc, release.Request, release.ExpectedEffects)
			if err != nil {
				return plan, err
			}
			encoded, err := jsonMarshalExpectedEffects(release.ExpectedEffects)
			if err != nil {
				return plan, err
			}
			input, err := encodePhase3BuildInput(release.Request, encoded)
			if err != nil {
				return plan, err
			}
			plan.FundingRelease = input
			prefix = append(prefix, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: release.Request.AmountRaw, Cost: cost, Template: input})
			plan.ValidThroughSlot = min(plan.ValidThroughSlot, cost.ValidThroughSlot)
		}
		prefix = append(prefix, phase3BridgeExitCost{Action: ReportNAV, Cost: plan.Exit[0].Cost, Template: plan.Exit[0].Template})
	}
	if funding != nil {
		policySlot, err := observeWithdrawalExitPolicies(ctx, rpc, manifest, s.RouteLane, s.Slot, []Action{funding.Request.Action})
		if err != nil {
			return plan, err
		}
		cost := plan.CurrentCost
		if !currentSwap {
			costRequest := funding.Request
			if release != nil {
				costRequest.FullPayoffFunding = false
			} // future custody; never the persisted current wire
			cost, err = manifest.observePhase3KnownBuildCost(ctx, rpc, costRequest, funding.ExpectedEffects)
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
		plan.FundingSwap = &phase3QuotedExit{Input: input, QuotedOutputRaw: funding.Request.QuotedOutputRaw, EstimatedUpperOutputRaw: upperCash - uint64(debtCashRaw(s)), ProofLevel: "COST_ONLY_FUNDING_AND_RESIDUE_ESTIMATE_NOT_EXECUTION"}
		if !currentSwap {
			prefix = append(prefix, phase3BridgeExitCost{Action: funding.Request.Action, Amount: funding.Request.AmountRaw, Cost: cost, Template: input})
		}
		prefix = append(prefix, phase3BridgeExitCost{Action: ReportNAV, Cost: plan.Exit[0].Cost, Template: plan.Exit[0].Template})
		plan.ValidThroughSlot = min(plan.ValidThroughSlot, cost.ValidThroughSlot)
		if policySlot > plan.ValidThroughSlot {
			return plan, budgetHold("stale_funding_exit_admission")
		}
	}
	prefix = append(prefix, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: payoff.AmountRaw, Cost: payoffCost, Template: plan.PayoffRepayment})
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
