package backyardrwa

import (
	"context"
	"encoding/binary"
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
		s.PositionDebtValueRaw <= 0 || debtCashRaw(s) < 0 || uint64(debtCashRaw(s)) < request.AmountRaw ||
		decision.AmountRaw != s.PositionDebtRaw || evidence.ExpectedEffects.Repayment == nil {
		return phase3BridgeAdmission{}, budgetHold("complete_funded_payoff_admission_unavailable")
	}
	bound, err := validateFullPayoffRequest(ctx, rpc, request, evidence.ExpectedEffects, s.Slot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if bound.ObservedDebtRaw != uint64(s.PositionDebtRaw) || evidence.ExpectedEffects.Accounts[0].BeforeRaw != uint64(debtCashRaw(s)) {
		return phase3BridgeAdmission{}, budgetHold("payoff_admission_snapshot_changed")
	}
	post := observation
	post.Snapshot.PositionDebtRaw, post.Snapshot.PositionDebtValueRaw = 0, 0
	setDebtCashRaw(&post.Snapshot, debtCashRaw(s)-int64(evidence.ExpectedEffects.Repayment.MinimumDebitRaw))
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
	return pricePhase3PositionReturnAfterFunding(ctx, rpc, client, manifest, post, decision, request, effects, afterPayoff, nil, nil)
}

func pricePhase3PositionReturnAfterFunding(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, post Observation, decision Decision, request any, effects ExpectedEffects, afterPayoff bool, funding *JupiterExecutionEvidence, release *KaminoExecutionEvidence) (phase3BridgeAdmission, error) {
	s := post.Snapshot
	if rpc == nil || !s.Fresh || s.Slot <= 0 || s.RouteKind != RouteKind || s.ManualReason != "" ||
		s.Nonterminal != "" || s.HasAmbiguousSubmission || s.RouteLane != s.StrategyKey || decision.StrategyKey != s.RouteLane ||
		!s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionDebtRaw != 0 || s.PositionDebtValueRaw != 0 ||
		s.CollateralIdleRaw < 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || debtCashRaw(s) < 0 || s.VoltrStrategyIdleRaw != 0 ||
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
	var releasedReceipt uint64
	if release != nil {
		if !afterPayoff || funding == nil || !release.Request.RepaymentRelease {
			return phase3BridgeAdmission{}, budgetHold("invalid_release_return_projection")
		}
		releasedReceipt = release.Request.AmountRaw
	}
	if err != nil || obligation.collateralDepositedRaw != uint64(s.PositionCollateralRaw)+releasedReceipt || (!afterPayoff && obligation.debtRaw != 0) {
		return phase3BridgeAdmission{}, budgetHold("payoff_return_position_changed")
	}
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if release != nil {
		debit, err := MeasureExecutableDebit(release.Request, release.ExpectedEffects)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		reserve, err = projectKaminoReleaseReserve(reserve, releasedReceipt, debit.Raw)
		if err != nil {
			return phase3BridgeAdmission{}, err
		}
		accounts = append([]ConfirmedAccount(nil), accounts...)
		for i, a := range accounts {
			if a.Address == route.CollateralLiquiditySupply {
				if len(a.Data) < 72 || binary.LittleEndian.Uint64(a.Data[64:72]) != release.ExpectedEffects.Accounts[0].BeforeRaw {
					return phase3BridgeAdmission{}, budgetHold("release_supply_changed")
				}
				accounts[i].Data = append([]byte(nil), a.Data...)
				binary.LittleEndian.PutUint64(accounts[i].Data[64:72], release.ExpectedEffects.Accounts[0].AfterRaw)
			}
		}
	}
	amount, err := reserve.redeemLiquidityRaw(uint64(s.PositionCollateralRaw))
	if err != nil || amount == 0 || amount > math.MaxInt64 {
		return phase3BridgeAdmission{}, budgetHold("payoff_return_withdrawal_amount_unavailable")
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	withdrawal, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, uint64(s.PositionCollateralRaw), blockhash, s.RouteLane)
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
	if funding != nil {
		if !afterPayoff || !isPayoffFundingAction(funding.Request.Action) || funding.Request.RouteLane != s.RouteLane ||
			(funding.Request.Action == SwapUSDCToDebtStep && (release != nil || s.SquadsIdleRaw != 0)) {
			return phase3BridgeAdmission{}, budgetHold("invalid_funding_return_projection")
		}
	}
	if funding != nil && funding.Request.Action == SwapCollateralToDebtStep {
		if s.CollateralIdleRaw != 0 {
			return phase3BridgeAdmission{}, budgetHold("invalid_funding_return_projection")
		}
		// Only this cost template sees post-swap empty collateral. Check the
		// actual pre-swap custody first; never mutate RPC data or a current wire.
		accounts = append([]ConfirmedAccount(nil), accounts...)
		for i, account := range accounts {
			if account.Address == route.CollateralCustody {
				mint, _ := decodeBase58PublicKey(route.Kamino.CollateralMint)
				authority, _ := decodeBase58PublicKey(bridgeVault)
				custody, err := DecodeTokenCustody(account.Owner, account.Data, mint, authority)
				expectedBefore := funding.Request.AmountRaw
				if release != nil {
					expectedBefore = release.ExpectedEffects.Accounts[1].BeforeRaw
					if release.ExpectedEffects.Accounts[1].AfterRaw != funding.Request.AmountRaw {
						return phase3BridgeAdmission{}, budgetHold("release_funding_amount_changed")
					}
				}
				if err != nil || custody.Raw != expectedBefore {
					return phase3BridgeAdmission{}, budgetHold("funding_collateral_custody_changed")
				}
				accounts[i].Data = append([]byte(nil), account.Data...)
				binary.LittleEndian.PutUint64(accounts[i].Data[64:72], 0)
			}
		}
	}
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
