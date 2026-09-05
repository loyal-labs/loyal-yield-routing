package backyardrwa

import (
	"context"
	"math"
	"time"
)

// A funded full payoff uses a finite interest-window request. Reserve the
// largest possible debt residue (source balance minus minimum repayment), then
// full collateral withdrawal, both conversions, and the complete bridge return.
// Partial repayment/release-for-funding and new borrowing are different graphs.
func observePhase3PayoffAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, observation Observation, decision Decision, evidence KaminoExecutionEvidence) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, request := observation.Snapshot, evidence.Request
	if decision.Action != DeleverRouteStep || request.Action != decision.Action || !request.FullPayoff ||
		request.RouteLane != s.RouteLane || decision.StrategyKey != s.RouteLane || s.PositionDebtRaw <= 0 ||
		s.PositionDebtValueRaw <= 0 || s.DebtIdleRaw < 0 || uint64(s.DebtIdleRaw) < request.AmountRaw ||
		decision.AmountRaw != s.PositionDebtRaw || evidence.ExpectedEffects.Repayment == nil {
		return phase3BridgeAdmission{}, budgetHold("complete_funded_payoff_admission_unavailable")
	}
	bound, err := validateFullPayoffRequest(ctx, rpc, request, evidence.ExpectedEffects, s.Slot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if bound.ObservedDebtRaw != uint64(s.PositionDebtRaw) || evidence.ExpectedEffects.Accounts[0].BeforeRaw != uint64(s.DebtIdleRaw) {
		return phase3BridgeAdmission{}, budgetHold("payoff_admission_snapshot_changed")
	}
	post := observation
	post.Snapshot.PositionDebtRaw, post.Snapshot.PositionDebtValueRaw = 0, 0
	post.Snapshot.DebtIdleRaw -= int64(evidence.ExpectedEffects.Repayment.MinimumDebitRaw)
	plan, err := pricePhase3PositionReturn(ctx, rpc, client, manifest, post, decision, request, evidence.ExpectedEffects, true)
	if err != nil {
		return plan, err
	}
	plan.Snapshot, plan.Payoff = s, &bound
	plan.ValidThroughSlot = min(plan.ValidThroughSlot, bound.ThroughSlot)
	return plan, nil
}

// Used both immediately after the proposed payoff (cost-only poststate) and
// for the actual NAV following a reconciled payoff. Templates never become the
// next current instruction: withdrawal is prepared and admitted again later.
func pricePhase3PositionReturn(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, post Observation, decision Decision, request any, effects ExpectedEffects, afterPayoff bool) (phase3BridgeAdmission, error) {
	s := post.Snapshot
	if rpc == nil || !s.Fresh || s.Slot <= 0 || s.RouteKind != RouteKind || s.ManualReason != "" ||
		s.Nonterminal != "" || s.HasAmbiguousSubmission || s.RouteLane != s.StrategyKey || decision.StrategyKey != s.RouteLane ||
		!s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionDebtRaw != 0 || s.PositionDebtValueRaw != 0 ||
		s.CollateralIdleRaw != 0 || s.PrimeIdleRaw != 0 || s.DebtIdleRaw < 0 || s.VoltrStrategyIdleRaw != 0 ||
		s.SquadsIdleRaw < 0 || s.VoltrIdleRaw < 0 {
		return phase3BridgeAdmission{}, budgetHold("complete_post_payoff_return_unavailable")
	}
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	slot, accounts, err := rpc.GetMultipleAccounts(ctx, []string{route.Kamino.Obligation, route.Kamino.CollateralReserve, route.CollateralCustody, route.CollateralLiquiditySupply}, s.Slot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || obligation.collateralDepositedRaw != uint64(s.PositionCollateralRaw) || (!afterPayoff && obligation.debtRaw != 0) {
		return phase3BridgeAdmission{}, budgetHold("payoff_return_position_changed")
	}
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	amount, err := reserve.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	if err != nil || amount == 0 || amount > math.MaxInt64 {
		return phase3BridgeAdmission{}, budgetHold("payoff_return_withdrawal_amount_unavailable")
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	withdrawal, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, obligation.collateralDepositedRaw, blockhash, s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	withdrawal.ObligationReserves = []string{route.Kamino.CollateralReserve}
	_, policies, err := rpc.GetMultipleAccounts(ctx, []string{withdrawal.Policy}, slot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	policy := accountAt(policies, withdrawal.Policy)
	if policy.Owner != bridgeSquadsProgram || policy.Lamports == 0 || policy.Executable || sha256Bytes(policy.Data) != withdrawal.PolicyAccountDataSHA256 {
		return phase3BridgeAdmission{}, budgetHold("payoff_withdrawal_policy_drift")
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	withdrawalEffects, err := exactKaminoTokenEffects(accounts, source, destination, amount)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	tailDecision := Decision{Action: DeleverRouteStep, StrategyKey: s.RouteLane, Reason: "withdrawal_withdraw_collateral"}
	tail, err := observePhase3WithdrawalAdmission(ctx, rpc, client, manifest, post, tailDecision, KaminoExecutionEvidence{withdrawal, withdrawalEffects})
	if err != nil {
		return tail, err
	}
	current, err := observePhase3KnownBuildCost(ctx, rpc, request, effects)
	if err != nil {
		return tail, err
	}
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return tail, err
	}
	input, err := encodePhase3BuildInput(request, encoded)
	if err != nil {
		return tail, err
	}
	plan := tail
	plan.Snapshot, plan.Decision, plan.Input, plan.CurrentCost = s, decision, input, current
	plan.PayoffWithdrawal = tail.Input
	plan.Exit = nil
	if afterPayoff {
		if len(tail.Exit) == 0 || tail.Exit[0].Action != ReportNAV {
			return plan, budgetHold("payoff_nav_fee_unavailable")
		}
		plan.Exit = append(plan.Exit, phase3BridgeExitCost{Action: ReportNAV, Cost: tail.Exit[0].Cost})
	}
	plan.Exit = append(plan.Exit, phase3BridgeExitCost{Action: DeleverRouteStep, Amount: withdrawal.AmountRaw, Cost: tail.CurrentCost})
	plan.Exit = append(plan.Exit, tail.Exit...)
	plan.ExitAfterMicros = 0
	for _, step := range plan.Exit {
		plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros, step.Cost.TotalMicros)
		if err != nil {
			return plan, err
		}
	}
	plan.ValidThroughSlot = min(tail.ValidThroughSlot, current.ValidThroughSlot)
	return plan, nil
}

func (d *Database) admitPhase3PositionReturnNAV(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, operationID string, observation Observation, decision Decision, evidence BridgeExecutionEvidence) error {
	if decision.Action != ReportNAV || evidence.Request.Action != ReportNAV || decision.AmountRaw != 0 || evidence.Request.AmountRaw != 0 ||
		evidence.Request.Report.ObservedSlot != uint64(observation.Snapshot.Slot) || evidence.Request.Report.Sequence != uint64(observation.Snapshot.Slot) {
		return budgetHold("post_payoff_nav_intent_mismatch")
	}
	plan, err := pricePhase3PositionReturn(ctx, rpc, client, manifest, observation, decision, evidence.Request, evidence.ExpectedEffects, false)
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmission(ctx, rpc, operationID, observation, decision, plan)
}
