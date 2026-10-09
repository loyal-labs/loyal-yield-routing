package backyard

import (
	"context"
	"encoding/binary"
	"math"
	"time"
)

// A prospective reverse quote is not evidence of current custody. Validate only
// the persisted current entry against one fresh account batch, also at final
// send. No entry is allowed to adopt an unaccounted open position or balance.
func validateEntrySwap(ctx context.Context, rpc *RPCClient, request JupiterSwapRequest, effects ExpectedEffects, slot int64) (int64, error) {
	if selectorLane(request.RouteLane) && (len(effects.Accounts) == 0 || effects.Accounts[0].BeforeRaw != request.AmountRaw) {
		return 0, budgetHold("entry_swap_must_consume_working_cash")
	}
	if !request.EntryReturnReserved || request.FullPayoffFunding || request.Action != SwapStableToCollateralStep || len(effects.Accounts) != 2 {
		return 0, budgetHold("entry_swap_intent_mismatch")
	}
	if _, err := MeasureExecutableDebit(request, effects); err != nil {
		return 0, err
	}
	if effects.Accounts[1].BeforeRaw > math.MaxUint64-request.MinimumOutputRaw || effects.Accounts[1].AfterRaw != effects.Accounts[1].BeforeRaw+request.MinimumOutputRaw || effects.Accounts[1].MinimumAfterRaw == nil || *effects.Accounts[1].MinimumAfterRaw != effects.Accounts[1].AfterRaw {
		return 0, budgetHold("entry_swap_output_mismatch")
	}
	route, err := runtimeRoute(request.RouteLane)
	if err != nil {
		return 0, err
	}
	sourceMint, destinationMint, source, destination, err := jupiterEdgeForRoute(request.Action, request.RouteLane)
	if err != nil || source != bridgeSquadsATA || sourceMint != bridgeUSDC || destination != route.CollateralCustody {
		return 0, budgetHold("entry_swap_custody_mismatch")
	}
	observed, accounts, err := rpc.GetMultipleAccounts(ctx, []string{source, destination, route.DebtCustody, route.Kamino.Obligation}, slot)
	if err != nil {
		return 0, budgetHold("entry_swap_state_unavailable")
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	// Only AUTO topups may retain debt; durable admission/build/send bind
	// the original loan and exact receipt-owned custody separately.
	debtTopup := request.TopupReturnReserved && request.RouteLane == autoAUTOPYUSD.Lane
	if err != nil || (!debtTopup && obligation.debtRaw != 0) || (obligation.collateralDepositedRaw == 0) != !request.TopupReturnReserved {
		return 0, budgetHold("entry_swap_position_changed")
	}
	for i, identity := range []struct{ address, mint string }{{source, sourceMint}, {destination, destinationMint}, {route.DebtCustody, route.Kamino.DebtMint}} {
		a := accountAt(accounts, identity.address)
		mint, _ := decodeBase58PublicKey(identity.mint)
		authority, _ := decodeBase58PublicKey(bridgeVault)
		custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, authority)
		if err != nil || a.Executable || a.Lamports == 0 {
			return 0, budgetHold("entry_swap_custody_changed")
		}
		if i == 2 {
			if identity.address == source && identity.mint == sourceMint {
				continue
			}
			if custody.Raw != 0 {
				return 0, budgetHold("entry_swap_debt_custody_changed")
			}
			continue
		}
		e := effects.Accounts[i]
		if e.Address != identity.address || e.Mint != identity.mint || e.Owner != a.Owner || e.Authority != bridgeVault || e.BeforeRaw != custody.Raw || (i == 1 && custody.Raw != 0 && !debtTopup) {
			return 0, budgetHold("entry_swap_custody_changed")
		}
	}
	return observed, nil
}

// Reserve an immediate complete exit after the initial USDC/collateral swap.
// The later deposit/borrow must independently reprice and extend this reserve;
// pricing an entry conversion does not authorize those future transactions.
func observePhase3EntrySwapAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, observation Observation, decision Decision, evidence JupiterExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, r := observation.Snapshot, evidence.Request
	debtTopup := r.TopupReturnReserved && s.RouteLane == autoAUTOPYUSD.Lane && s.PositionDebtRaw > 0 && s.PositionDebtValueRaw > 0
	emergency := emergencyTopupEntryInventory(s, decision)
	if rpc == nil || client == nil || !s.Fresh || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.RouteKind != RouteKind ||
		s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.CutoverDrain ||
		s.RouteLane != s.StrategyKey || s.RouteLane != decision.StrategyKey || s.RouteLane != r.RouteLane ||
		phase3BudgetFamilyForLane(s.RouteLane) == "" || s.CollateralIdleRaw < 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || (!debtTopup && s.CollateralIdleRaw != 0) || s.DebtIdleRaw != 0 ||
		s.VoltrStrategyIdleRaw != 0 || s.VoltrIdleRaw < 0 || s.SquadsIdleRaw <= 0 || decision.AmountRaw <= 0 || decision.AmountRaw > s.SquadsIdleRaw ||
		decision.Action != SwapStableToCollateralStep || r.Action != decision.Action || r.AmountRaw != uint64(decision.AmountRaw) || !r.EntryReturnReserved ||
		r.TopupReturnReserved != (decision.Reason == topupSwapReason || emergency) {
		return phase3BridgeAdmission{}, budgetHold("complete_entry_swap_return_unavailable")
	}
	// Topups retain the funded position; ordinary entry requires a flat route.
	topup := r.TopupReturnReserved && s.HasPosition && s.PositionCollateralRaw > 0 && s.PositionCollateralValueRaw > 0 &&
		(debtTopup || (s.PositionDebtRaw == 0 && s.PositionDebtValueRaw == 0)) && (emergency || s.WithdrawalDemandRaw == 0 && !s.Unwind) && decision.AmountRaw == s.SquadsIdleRaw
	flat := !r.TopupReturnReserved && !s.HasPosition && s.PositionCollateralRaw == 0 && s.PositionDebtRaw == 0 &&
		s.PositionCollateralValueRaw == 0 && s.PositionDebtValueRaw == 0
	if !topup && !flat {
		return phase3BridgeAdmission{}, budgetHold("complete_entry_swap_return_unavailable")
	}
	if emergency {
		proof, err := verifyDebtClearEmergency(manifest, observation, decision, "topup-risk-entry", time.Now().UTC())
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		if proof == nil {
			return phase3BridgeAdmission{}, budgetHold("topup_risk_entry_fresh_risk_required")
		}
		if err := validateFreshTopupPrincipal(ctx, rpc, s.TopupTranche.Loan, s, s.Slot+observationLagSlots()); err != nil {
			return phase3BridgeAdmission{}, err
		}
	} else if debtTopup {
		if err := validateDebtTopupCapital(ctx, rpc, s, decision, s.Slot+observationLagSlots()); err != nil {
			return phase3BridgeAdmission{}, err
		}
	}
	slot, err := validateEntrySwap(ctx, rpc, r, evidence.ExpectedEffects, s.Slot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if evidence.ExpectedEffects.Accounts[0].BeforeRaw != uint64(s.SquadsIdleRaw) {
		return phase3BridgeAdmission{}, budgetHold("entry_swap_snapshot_changed")
	}
	// Reuse the existing two-sided estimate for full-custody exit costing.
	// The actual output is reobserved after execution; this is not a maximum
	// enforced by Jupiter, and excess output never permits a truncated return.
	upper, err := withdrawalUSDCExitEstimate(r.QuotedOutputRaw)
	if err != nil || upper > uint64(math.MaxInt64-s.CollateralIdleRaw) {
		return phase3BridgeAdmission{}, budgetHold("topup_swap_output_overflow")
	}
	upper += uint64(s.CollateralIdleRaw)
	post := observation
	post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = int64(upper), int64(upper)
	post.Snapshot.SquadsIdleRaw -= int64(r.AmountRaw)
	var plan phase3BridgeAdmission
	if debtTopup {
		current, costErr := manifest.observePhase3KnownBuildCost(ctx, rpc, r, evidence.ExpectedEffects)
		if costErr != nil {
			return plan, costErr
		}
		bound, accounts, readErr := observeKaminoPayoffWindowAccounts(ctx, rpc, autoAUTOPYUSD, max(slot, current.ObservationSlot), 3, autoAUTOPYUSD.Kamino.Market)
		if readErr != nil {
			return plan, readErr
		}
		if err = s.TopupTranche.Loan.validatePrincipal(accounts, autoAUTOPYUSD, bound.ObservedSlot); err != nil {
			return plan, err
		}
		// Cost-only custody projection. Keep the actual obligation and debt.
		for i, a := range accounts {
			if a.Address == autoAUTOPYUSD.CollateralCustody {
				accounts[i].Data = append([]byte(nil), a.Data...)
				binary.LittleEndian.PutUint64(accounts[i].Data[64:72], upper)
			}
		}
		plan, err = pricePhase3ProjectedPositionReturn(ctx, rpc, client, manifest, post, decision, r, evidence.ExpectedEffects, current, phase3KaminoProjection{Slot: bound.ObservedSlot, Accounts: accounts})
	} else if topup {
		// NAV, then withdraw the whole position and return it together with
		// the swapped collateral, exactly like the post-payoff return.
		plan, err = pricePhase3PositionReturn(ctx, rpc, client, manifest, post, decision, r, evidence.ExpectedEffects, true)
	} else {
		plan, err = pricePhase3CollateralReturn(ctx, rpc, client, manifest, post, decision, r, evidence.ExpectedEffects, upper, true, nil)
	}
	if err != nil {
		return plan, err
	}
	plan.Snapshot = s
	if debtTopup && !topupFullyFundedReturn(plan) {
		return plan, budgetHold("topup_risk_entry_requires_full_funding")
	}
	if slot > plan.ValidThroughSlot {
		return plan, budgetHold("stale_entry_swap_admission")
	}
	return plan, nil
}

func (d *Database) admitPhase3EntrySwap(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, operationID string, observation Observation, decision Decision, evidence JupiterExecutionEvidence) error {
	plan, err := observePhase3EntrySwapAdmission(ctx, rpc, client, manifest, observation, decision, evidence)
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmissionOnManifest(ctx, rpc, manifest, operationID, observation, decision, plan)
}

// Capital continuation consumes only finalized receipt-owned inventory. A fresh
// attempt does not replace the allocation's original principal witness.
func validateDebtTopupCapital(ctx context.Context, rpc *RPCClient, s Snapshot, d Decision, through int64) error {
	t := s.TopupTranche
	hard := min(s.LiquidationThresholdBPS-1500, int64(6000))
	if !s.Fresh || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.CutoverDrain || t == nil || t.validate() != nil || t.LastSlot > s.Slot || t.Lane != s.RouteLane || s.RouteLane != autoAUTOPYUSD.Lane || d.StrategyKey != s.RouteLane ||
		!s.PilotActive || !s.PolicyReady || !s.ExitBuildable || s.Unwind || s.UnwindRefreshRequired || s.PostMutationNAVRequired || s.WithdrawalDemandRaw != 0 ||
		hard <= TargetLTVBPS || s.LTVBPS >= hard || s.DebtIdleRaw != 0 || s.VoltrStrategyIdleRaw != 0 || s.SquadsIdleRaw < 0 || s.CollateralIdleRaw < 0 || s.PrimeIdleRaw != s.CollateralIdleRaw ||
		uint64(s.SquadsIdleRaw) != t.USDCRemainingRaw || uint64(s.CollateralIdleRaw) != t.CollateralRemainingRaw || d.AmountRaw <= 0 {
		return budgetHold("topup_capital_binding_changed")
	}
	switch d.Reason {
	case topupSwapReason:
		if d.Action != SwapStableToCollateralStep || t.Stage != topupTrancheAllocated || uint64(d.AmountRaw) != t.USDCRemainingRaw {
			return budgetHold("topup_swap_binding_changed")
		}
	case topupDepositReason:
		if d.Action != OpenRouteStep || t.Stage != topupTrancheCollateral || uint64(d.AmountRaw) != t.CollateralRemainingRaw {
			return budgetHold("topup_deposit_binding_changed")
		}
	default:
		return budgetHold("topup_capital_binding_changed")
	}
	return validateFreshTopupPrincipal(ctx, rpc, t.Loan, s, through)
}
