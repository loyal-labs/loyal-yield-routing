package backyard

import (
	"context"
	"math"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

// Plan B3 leg 1: move idle Voltr cash into Squads beside a funded debt-free
// position. The allocation wire carries its own adaptor report, so it keeps
// the 32-slot report window. The reserved exit is the complete position
// return priced with the allocated cash already in Squads: NAV, withdraw the
// whole position, swap, stage all Squads cash, restore, NAV.
func observePhase3TopupAllocationAdmission(ctx context.Context, rpc *chain.Client, client *jupiter.Client, manifest RouteManifest, observation Observation, decision Decision, evidence BridgeExecutionEvidence) (phase3BridgeAdmission, error) {
	s, r := observation.Snapshot, evidence.Request
	if rpc == nil || client == nil || !s.Fresh || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.RouteKind != RouteKind ||
		s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.CutoverDrain || s.Unwind || s.WithdrawalDemandRaw != 0 ||
		s.RouteLane != s.StrategyKey || decision.StrategyKey != s.RouteLane || phase3BudgetFamilyForLane(s.RouteLane) == "" ||
		!s.HasPosition || s.PositionCollateralRaw <= 0 || s.PositionCollateralValueRaw <= 0 || s.PositionDebtRaw != 0 || s.PositionDebtValueRaw != 0 ||
		s.SquadsIdleRaw != 0 || s.CollateralIdleRaw != 0 || s.PrimeIdleRaw != 0 || s.DebtIdleRaw != 0 || s.VoltrStrategyIdleRaw != 0 ||
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
	plan, err := pricePhase3PositionReturn(ctx, rpc, client, manifest, post, decision, r, evidence.ExpectedEffects, true)
	if err != nil {
		return plan, err
	}
	plan.Snapshot = s
	plan.ValidThroughSlot = min(plan.ValidThroughSlot, s.Slot+min(observationLagSlots(), adaptorMaxReportAgeSlots))
	slot, err := confirmedSlot(ctx, rpc)
	if err != nil || slot > plan.ValidThroughSlot {
		return plan, budgetHold("stale_topup_allocation_admission")
	}
	return plan, nil
}

func (d *Database) admitPhase3TopupAllocation(ctx context.Context, rpc *chain.Client, client *jupiter.Client, manifest RouteManifest, operationID string, observation Observation, decision Decision, evidence BridgeExecutionEvidence) error {
	plan, err := observePhase3TopupAllocationAdmission(ctx, rpc, client, manifest, observation, decision, evidence)
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmissionOnManifest(ctx, rpc, manifest, operationID, observation, decision, plan)
}
