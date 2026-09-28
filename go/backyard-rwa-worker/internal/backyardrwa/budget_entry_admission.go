package backyardrwa

import (
	"context"
	"math"
	"time"
)

// A prospective reverse quote is not evidence of current custody. Validate only
// the persisted current entry against one fresh account batch, also at final
// send. No entry is allowed to adopt an unaccounted open position or balance.
func validateEntrySwap(ctx context.Context, rpc *RPCClient, request JupiterSwapRequest, effects ExpectedEffects, slot int64) (int64, error) {
	if !request.EntryReturnReserved || request.FullPayoffFunding || request.Action != SwapStableToCollateralStep || len(effects.Accounts) != 2 {
		return 0, budgetHold("entry_swap_intent_mismatch")
	}
	if _, err := MeasureExecutableDebit(request, effects); err != nil {
		return 0, err
	}
	if effects.Accounts[1].AfterRaw != request.MinimumOutputRaw || effects.Accounts[1].MinimumAfterRaw == nil || *effects.Accounts[1].MinimumAfterRaw != request.MinimumOutputRaw {
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
	if err != nil || obligation.collateralDepositedRaw != 0 || obligation.debtRaw != 0 {
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
			if custody.Raw != 0 {
				return 0, budgetHold("entry_swap_debt_custody_changed")
			}
			continue
		}
		e := effects.Accounts[i]
		if e.Address != identity.address || e.Mint != identity.mint || e.Owner != a.Owner || e.Authority != bridgeVault || e.BeforeRaw != custody.Raw || (i == 1 && custody.Raw != 0) {
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
	if rpc == nil || client == nil || !s.Fresh || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagSlots || s.RouteKind != RouteKind ||
		s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.CutoverDrain ||
		s.RouteLane != s.StrategyKey || s.RouteLane != decision.StrategyKey || s.RouteLane != r.RouteLane ||
		phase3BudgetFamilyForLane(s.RouteLane) == "" || s.HasPosition || s.PositionCollateralRaw != 0 || s.PositionDebtRaw != 0 ||
		s.PositionCollateralValueRaw != 0 || s.PositionDebtValueRaw != 0 || s.CollateralIdleRaw != 0 || s.PrimeIdleRaw != 0 || s.DebtIdleRaw != 0 ||
		s.VoltrStrategyIdleRaw != 0 || s.VoltrIdleRaw < 0 || s.SquadsIdleRaw <= 0 || decision.AmountRaw <= 0 || decision.AmountRaw > s.SquadsIdleRaw ||
		decision.Action != SwapStableToCollateralStep || r.Action != decision.Action || r.AmountRaw != uint64(decision.AmountRaw) || !r.EntryReturnReserved {
		return phase3BridgeAdmission{}, budgetHold("complete_entry_swap_return_unavailable")
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
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	post := observation
	post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = int64(upper), int64(upper)
	post.Snapshot.SquadsIdleRaw -= int64(r.AmountRaw)
	plan, err := pricePhase3CollateralReturn(ctx, rpc, client, manifest, post, decision, r, evidence.ExpectedEffects, upper, true, nil)
	if err != nil {
		return plan, err
	}
	plan.Snapshot = s
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
	return d.persistPhase3ExitAdmission(ctx, rpc, operationID, observation, decision, plan)
}
