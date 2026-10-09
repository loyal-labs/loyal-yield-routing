package backyard

import (
	"context"
	"math"
)

const topupRiskEntryReason = "hard_ltv_owned_usdc_conversion"

// Classification only; coherent risk, durable authority and fresh loan/custody
// validation remain mandatory at admission, build and send.
func emergencyTopupEntryInventory(s Snapshot, d Decision) bool {
	t := s.TopupTranche
	return s.RouteLane == autoAUTOPYUSD.Lane && s.StrategyKey == s.RouteLane && d.StrategyKey == s.RouteLane &&
		d.Action == SwapStableToCollateralStep && d.Reason == topupRiskEntryReason && s.PilotActive && s.HasPosition && s.PositionDebtRaw > 0 &&
		t != nil && t.validate() == nil && t.Stage == topupTrancheAllocated && t.LastSlot <= s.Slot && t.Lane == s.RouteLane &&
		s.SquadsIdleRaw > 0 && d.AmountRaw == s.SquadsIdleRaw && uint64(s.SquadsIdleRaw) == t.USDCRemainingRaw &&
		s.CollateralIdleRaw >= 0 && s.PrimeIdleRaw == s.CollateralIdleRaw && uint64(s.CollateralIdleRaw) == t.CollateralRemainingRaw &&
		debtCashRaw(s) == 0 && t.DebtRemainingRaw == 0 && s.VoltrStrategyIdleRaw == 0 && t.StrategyRemainingRaw == 0
}

func topupFullyFundedReturn(p phase3BridgeAdmission) bool {
	return p.BorrowRelease == nil && p.ExitCycles == 0 && p.FundingSwap != nil && p.Payoff != nil && p.PayoffRepayment != nil && p.PayoffWithdrawal != nil
}

// Reserve the actual permitted conversion and whole-position recovery before
// allocation. This is cost-only; it creates neither custody nor exit authority.
func priceTopupAllocatedCashReturn(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, post Observation, d Decision, current ValuedTransactionCost, allocation BridgeExecutionEvidence, projection phase3KaminoProjection) (phase3BridgeAdmission, error) {
	s := post.Snapshot
	entryDecision := Decision{Action: SwapStableToCollateralStep, StrategyKey: s.RouteLane, AmountRaw: s.SquadsIdleRaw, Reason: topupRiskEntryReason}
	entry, err := prepareJupiterQuoteEvidence(ctx, rpc, client, m, entryDecision, uint64(s.SquadsIdleRaw), uint64(s.CollateralIdleRaw), projection.Slot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	// Unflagged cost templates compile the same wire without asserting future custody.
	entryCost, err := m.observePhase3KnownBuildCost(ctx, rpc, entry.Request, entry.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	entryCost, err = m.observePilotExecutionCost(ctx, rpc, entry.Request, entry.ExpectedEffects, entryCost)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	entryInput, err := exitLegInput(entry.Request, entry.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	upper, err := withdrawalUSDCExitEstimate(entry.Request.QuotedOutputRaw)
	if err != nil || upper > uint64(math.MaxInt64-s.CollateralIdleRaw) {
		return phase3BridgeAdmission{}, budgetHold("topup_swap_output_overflow")
	}
	upper += uint64(s.CollateralIdleRaw)
	post.Snapshot.SquadsIdleRaw = 0
	post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = int64(upper), int64(upper)
	projection.Accounts = patchConfirmedTokenRaw(projection.Accounts, autoAUTOPYUSD.CollateralCustody, upper)
	entry.Request.EntryReturnReserved, entry.Request.TopupReturnReserved = true, true
	plan, err := pricePhase3ProjectedPositionReturn(ctx, rpc, client, m, post, entryDecision, entry.Request, entry.ExpectedEffects, entryCost, projection)
	if err != nil {
		return plan, err
	}
	if !topupFullyFundedReturn(plan) {
		return plan, budgetHold("topup_allocation_requires_full_recovery_funding")
	}
	plan.Input, err = exitLegInput(allocation.Request, allocation.ExpectedEffects)
	if err != nil {
		return plan, err
	}
	plan.Decision, plan.CurrentCost = d, current
	plan.Exit = append([]phase3BridgeExitCost{{Action: SwapStableToCollateralStep, Amount: entry.Request.AmountRaw, Cost: entryCost, Template: entryInput}}, plan.Exit...)
	plan.ExitAfterMicros, err = budgetSum(entryCost.TotalMicros, plan.ExitAfterMicros)
	plan.ValidThroughSlot = min(plan.ValidThroughSlot, current.ValidThroughSlot, entryCost.ValidThroughSlot)
	return plan, err
}
