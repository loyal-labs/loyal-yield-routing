package backyard

import (
	"bytes"
	"context"
	"math"
)

// Plan B3 leg 1: move idle Voltr cash into Squads beside a funded
// position. The allocation wire carries its own adaptor report, so it keeps
// the 32-slot report window. The reserved exit is the complete position
// return priced with the allocated cash already in Squads. Debt-bearing AUTO
// must first prove full repayment funding through the installed swap edges;
// only then price collateral withdrawal and all remaining cash returns.
func observePhase3TopupAllocationAdmission(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, observation Observation, decision Decision, evidence BridgeExecutionEvidence) (phase3BridgeAdmission, error) {
	s, r := observation.Snapshot, evidence.Request
	debtTopup := s.RouteLane == autoAUTOPYUSD.Lane && s.PositionDebtRaw > 0 && s.PositionDebtValueRaw > 0
	if rpc == nil || client == nil || !s.Fresh || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.RouteKind != RouteKind ||
		s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.CutoverDrain || s.Unwind || s.WithdrawalDemandRaw != 0 ||
		s.RouteLane != s.StrategyKey || decision.StrategyKey != s.RouteLane || phase3BudgetFamilyForLane(s.RouteLane) == "" ||
		!s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionCollateralValueRaw <= 0 || (!debtTopup && (s.PositionDebtRaw != 0 || s.PositionDebtValueRaw != 0)) ||
		s.SquadsIdleRaw != 0 || s.CollateralIdleRaw < 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || (!debtTopup && s.CollateralIdleRaw != 0) || s.DebtIdleRaw != 0 || s.VoltrStrategyIdleRaw != 0 ||
		decision.Action != VoltrAllocateToSquads || decision.Reason != topupAllocationReason || decision.AmountRaw <= 0 || decision.AmountRaw > s.VoltrIdleRaw ||
		r.Action != decision.Action || r.AmountRaw != uint64(decision.AmountRaw) || r.Report.ObservedSlot != uint64(s.Slot) || r.Report.Sequence != uint64(s.Slot) {
		return phase3BridgeAdmission{}, budgetHold("topup_allocation_admission_unavailable")
	}
	effects, _, squadsAfter, err := bridgeExpectedEffects(decision, uint64(s.VoltrIdleRaw), 0, 0)
	if err != nil || squadsAfter != r.AmountRaw || len(evidence.ExpectedEffects.Accounts) != len(effects.Accounts) {
		return phase3BridgeAdmission{}, budgetHold("topup_allocation_intent_mismatch")
	}
	for i, account := range effects.Accounts {
		if evidence.ExpectedEffects.Accounts[i] != account {
			return phase3BridgeAdmission{}, budgetHold("topup_allocation_intent_mismatch")
		}
	}
	post := observation
	post.Snapshot.VoltrIdleRaw -= decision.AmountRaw
	post.Snapshot.SquadsIdleRaw = decision.AmountRaw
	var plan phase3BridgeAdmission
	if debtTopup {
		plan, err = pricePhase3DebtTopupAllocation(ctx, rpc, client, manifest, observation, post, decision, evidence)
	} else {
		plan, err = pricePhase3PositionReturn(ctx, rpc, client, manifest, post, decision, r, evidence.ExpectedEffects, true)
	}
	if err != nil {
		return plan, err
	}
	plan.Snapshot = s
	plan.ValidThroughSlot = min(plan.ValidThroughSlot, s.Slot+min(observationLagSlots(), adaptorMaxReportAgeSlots))
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil || slot > plan.ValidThroughSlot {
		return plan, budgetHold("stale_topup_allocation_admission")
	}
	return plan, nil
}

// The finalized loan is captured before any pricing. Only custody cash is
// projected: the loan/reserves remain actual, and exit templates are COST ONLY.
func pricePhase3DebtTopupAllocation(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, before, post Observation, d Decision, e BridgeExecutionEvidence) (phase3BridgeAdmission, error) {
	s, r := before.Snapshot, e.Request
	carry := uint64(0)
	if s.TopupTranche != nil {
		if s.TopupTranche.Stage != topupTrancheComplete || s.TopupTranche.validate() != nil || s.TopupTranche.LastSlot > s.Slot {
			return phase3BridgeAdmission{}, budgetHold("topup_allocation_binding_unavailable")
		}
		carry = s.TopupTranche.CollateralRemainingRaw
	}
	if uint64(s.CollateralIdleRaw) != carry {
		return phase3BridgeAdmission{}, budgetHold("topup_allocation_custody_changed")
	}
	hard := min(s.LiquidationThresholdBPS-1500, int64(6000))
	amount := min(s.VoltrIdleRaw-DefaultSelectorPolicy().IdleBufferRaw, workingTrancheCap(s), s.TopupDepositRoomRaw, int64(strategyTwoBridgeLegCapRaw))
	if !s.PilotActive || !s.PolicyReady || !s.ExitBuildable || hard <= TargetLTVBPS || s.LTVBPS >= hard || s.UnwindRefreshRequired || s.PostMutationNAVRequired ||
		amount < topupMinimumRaw || d.AmountRaw != amount || s.StrategyNAVRaw < 0 || s.StrategyNAVRaw > math.MaxInt64-d.AmountRaw ||
		r.Report.NAVAfterRaw != uint64(s.StrategyNAVRaw+d.AmountRaw) {
		return phase3BridgeAdmission{}, budgetHold("topup_allocation_intent_mismatch")
	}
	want, _, _, err := bridgeExpectedEffects(d, uint64(s.VoltrIdleRaw), 0, 0)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	want.Kind, want.ReturnData = "bridge", expectedAdaptorReturnData(r.Report.NAVAfterRaw)
	expected, err := jsonMarshalExpectedEffects(want)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	actual, err := jsonMarshalExpectedEffects(e.ExpectedEffects)
	if err != nil || !bytes.Equal(expected, actual) {
		return phase3BridgeAdmission{}, budgetHold("topup_allocation_intent_mismatch")
	}
	loan, err := observeTopupLoanOrigin(ctx, rpc)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	through := s.Slot + min(observationLagSlots(), adaptorMaxReportAgeSlots)
	if loan.ObservedSlot > s.Slot {
		return phase3BridgeAdmission{}, budgetHold("topup_requires_prepricing_finalized_origin")
	}
	if err = validateFreshTopupPrincipal(ctx, rpc, loan, s, through); err != nil {
		return phase3BridgeAdmission{}, err
	}
	current, err := m.observePhase3KnownBuildCost(ctx, rpc, r, e.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	bound, accounts, err := observeKaminoPayoffWindowAccounts(ctx, rpc, autoAUTOPYUSD, max(s.Slot, current.ObservationSlot), 3, autoAUTOPYUSD.Kamino.Market, bridgeIdleATA, bridgeSquadsATA, bridgeStrategyATA)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if err = loan.validatePrincipal(accounts, autoAUTOPYUSD, bound.ObservedSlot); err != nil {
		return phase3BridgeAdmission{}, err
	}
	if bound.ObservedSlot > through || !sameAccruingDebt(bound, s.PositionDebtRaw) {
		return phase3BridgeAdmission{}, budgetHold("topup_snapshot_position_changed")
	}
	for _, effect := range append(want.Accounts, ExpectedAccountEffect{Address: autoAUTOPYUSD.CollateralCustody, Owner: autoAUTOPYUSD.CollateralTokenProgram, Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: uint64(s.CollateralIdleRaw)}) {
		a := accountAt(accounts, effect.Address)
		mint, _ := decodeBase58PublicKey(effect.Mint)
		owner, _ := decodeBase58PublicKey(effect.Authority)
		cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, owner)
		if err != nil || a.Owner != effect.Owner || a.Executable || a.Lamports == 0 || cash.Raw != effect.BeforeRaw {
			return phase3BridgeAdmission{}, budgetHold("topup_snapshot_custody_changed")
		}
	}
	post.Snapshot.StrategyNAVRaw = s.StrategyNAVRaw + d.AmountRaw
	projection := phase3KaminoProjection{Slot: bound.ObservedSlot, Accounts: accounts}
	plan, err := priceTopupAllocatedCashReturn(ctx, rpc, client, m, post, d, current, e, projection)
	if err != nil {
		return plan, err
	}
	plan.topupOrigin = &loan
	plan.Snapshot = s
	plan.ValidThroughSlot = min(plan.ValidThroughSlot, through)
	if err = validateFreshTopupPrincipal(ctx, rpc, loan, s, plan.ValidThroughSlot); err != nil {
		return plan, err
	}
	return plan, nil
}

func (d *Database) admitPhase3TopupAllocation(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, operationID string, observation Observation, decision Decision, evidence BridgeExecutionEvidence) error {
	plan, err := observePhase3TopupAllocationAdmission(ctx, rpc, client, manifest, observation, decision, evidence)
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmissionOnManifest(ctx, rpc, manifest, operationID, observation, decision, plan)
}
