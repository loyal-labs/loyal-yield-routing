package backyardrwa

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

// Source forecasts reuse the finite production exit graph. They do not commit
// an unwind, reserve a budget or assert that prospective balances exist.
type selectorSourceQuote struct {
	Lane           string         `json:"lane"`
	ObservationID  string         `json:"observationId"`
	MinimumIdleRaw uint64         `json:"minimumIdleRaw"`
	Recipe         selectorRecipe `json:"recipe"`
}

func observeSelectorSource(ctx context.Context, rpc *RPCClient, client *jupiterClient, m RouteManifest, o Observation) (selectorSourceQuote, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s := o.Snapshot
	out := selectorSourceQuote{Lane: s.RouteLane, ObservationID: s.ObservationID}
	if rpc == nil || client == nil || !s.PilotActive || !selectorLane(s.RouteLane) || s.RouteLane != s.StrategyKey || !s.Fresh || s.ObservationID == "" || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagSlots || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.ManualReason != "" || s.Unwind || s.CutoverDrain || s.WithdrawalDemandRaw != 0 || s.VoltrIdleRaw < 0 || s.VoltrStrategyIdleRaw < 0 || s.SquadsIdleRaw < 0 || s.CollateralIdleRaw < 0 || s.PositionDebtRaw < 0 || s.PositionCollateralRaw < 0 || s.DebtIdleRaw != 0 {
		return out, budgetHold("selector_source_unavailable")
	}
	if !hasWorkingCapital(s) {
		out.MinimumIdleRaw = uint64(s.VoltrIdleRaw)
		out.Recipe.ValidThroughSlot = s.Slot + budgetMaxObservationLagSlots
		raw, err := json.Marshal(struct {
			Kind     string
			Snapshot Snapshot
		}{"OBSERVED_IDLE_NO_EXIT", s})
		if err != nil {
			return out, err
		}
		out.Recipe.EvidenceID = sha256Bytes(raw)
		return out, nil
	}
	floor, err := observeWithdrawalExitPolicies(ctx, rpc, m, s.RouteLane, s.Slot, nil)
	if err != nil {
		return out, err
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return out, err
	}
	d := Decision{Action: ReportNAV, StrategyKey: s.RouteLane}
	e, _, _, err := bridgeExpectedEffects(d, uint64(s.VoltrIdleRaw), uint64(s.VoltrStrategyIdleRaw), uint64(s.SquadsIdleRaw))
	if err != nil {
		return out, err
	}
	if s.StrategyNAVRaw < 0 || !sha256Pattern.MatchString(s.ReportSnapshotDigest) {
		return out, budgetHold("selector_source_nav_unavailable")
	}
	e.Kind, e.ReturnData = "bridge", expectedAdaptorReturnData(uint64(s.StrategyNAVRaw))
	r := BridgeBuildRequest{Action: ReportNAV, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight,
		Report: BridgeReport{Sequence: uint64(s.Slot), ObservedSlot: uint64(s.Slot), NAVAfterRaw: uint64(s.StrategyNAVRaw), SnapshotDigest: s.ReportSnapshotDigest}}
	var plan phase3BridgeAdmission
	switch {
	case s.PositionDebtRaw > 0:
		plan, err = observePhase3FundingAdmission(ctx, rpc, client, m, o, d, r, e)
	case s.HasPosition || s.PositionCollateralRaw > 0:
		plan, err = pricePhase3PositionReturn(ctx, rpc, client, m, o, d, r, e, false)
	case s.CollateralIdleRaw > 0:
		plan, err = observePhase3CollateralReturnAdmission(ctx, rpc, client, m, o, d, r, e)
	default:
		plan, err = observePhase3BridgeAdmission(ctx, rpc, o, d, BridgeExecutionEvidence{Request: r, ExpectedEffects: e})
	}
	if err != nil {
		return out, err
	}
	return priceSelectorSourcePlan(ctx, rpc, plan, floor)
}

func priceSelectorSourcePlan(ctx context.Context, rpc *RPCClient, plan phase3BridgeAdmission, observationFloor int64) (selectorSourceQuote, error) {
	s := plan.Snapshot
	out := selectorSourceQuote{Lane: s.RouteLane, ObservationID: s.ObservationID}
	if !selectorLane(s.RouteLane) || !s.PilotActive || s.VoltrIdleRaw < 0 || s.VoltrStrategyIdleRaw < 0 || s.SquadsIdleRaw < 0 || s.DebtIdleRaw != 0 || plan.Input == nil || len(plan.Exit) == 0 {
		return out, budgetHold("selector_source_recipe_unavailable")
	}
	inputs := []*phase3BuildInput{plan.Input}
	costs := []ValuedTransactionCost{plan.CurrentCost}
	for _, step := range plan.Exit {
		if step.Template == nil {
			return out, budgetHold("selector_source_template_missing")
		}
		inputs = append(inputs, step.Template)
		costs = append(costs, step.Cost)
	}
	// Project minimum USDC independently from reservation upper balances. The
	// funding tail's optimistic residue and Stage/Restore amounts are NOT cash.
	cash := uint64(s.SquadsIdleRaw)
	add := func(amount uint64) error {
		if amount > math.MaxUint64-cash {
			return budgetHold("selector_source_cash_overflow")
		}
		cash += amount
		return nil
	}
	restored := false
	for i, input := range inputs {
		if costs[i].ObservationSlot < s.Slot {
			return out, budgetHold("selector_recipe_observation_expired")
		}
		observationFloor = max(observationFloor, costs[i].ObservationSlot)
		if observationFloor > s.Slot+budgetMaxObservationLagSlots {
			return out, budgetHold("selector_recipe_observation_expired")
		}
		request, effects, message, err := input.decode()
		if err != nil {
			return out, err
		}
		debit, err := MeasureExecutableDebit(request, effects)
		if err != nil {
			return out, err
		}
		if sha256Bytes(message) != costs[i].MessageSHA256 || debit != costs[i].Debit {
			return out, budgetHold("selector_source_template_mismatch")
		}
		var action Action
		var amount uint64
		switch r := request.(type) {
		case BridgeBuildRequest:
			action, amount = r.Action, r.AmountRaw
			if action != ReportNAV && action != StageSquadsToVoltr && action != VoltrRestoreIdle {
				return out, budgetHold("selector_source_not_an_exit")
			}
			if action == VoltrRestoreIdle {
				restored = true
			}
		case JupiterSwapRequest:
			action, amount = r.Action, r.AmountRaw
			if r.RouteLane != s.RouteLane || len(effects.Accounts) != 2 || effects.Accounts[0].Mint == bridgeUSDC || effects.Accounts[1].Mint != bridgeUSDC || (r.Action != SwapCollateralToStableStep && r.Action != SwapCollateralToDebtStep) {
				return out, budgetHold("selector_source_not_an_exit")
			}
			if err = add(r.MinimumOutputRaw); err != nil {
				return out, err
			}
			observationFloor, err = selectorSwapObservationFloor(r, s.Slot, observationFloor)
			if err != nil {
				return out, err
			}
		case KaminoPrimeUSDCRequest:
			action, amount = r.Action, r.AmountRaw
			_, leg, err := kaminoPrimeUSDCInstruction(r)
			if err != nil {
				return out, err
			}
			if r.RouteLane != s.RouteLane {
				return out, budgetHold("selector_source_not_an_exit")
			}
			switch leg {
			case kaminoLegRepay:
				if r.AmountRaw > cash {
					return out, budgetHold("selector_source_minimum_cannot_pay_debt")
				}
				cash -= r.AmountRaw
			case kaminoLegWithdraw:
			default:
				return out, budgetHold("selector_source_not_an_exit")
			}
		default:
			return out, budgetHold("selector_source_not_an_exit")
		}
		if i > 0 && (plan.Exit[i-1].Action != action || plan.Exit[i-1].Amount != amount) {
			return out, budgetHold("selector_source_template_mismatch")
		}
	}
	if !restored {
		return out, budgetHold("selector_source_idle_return_missing")
	}
	if err := add(uint64(s.VoltrIdleRaw)); err != nil {
		return out, err
	}
	if err := add(uint64(s.VoltrStrategyIdleRaw)); err != nil {
		return out, err
	}
	out.MinimumIdleRaw = cash
	var err error
	out.Recipe, err = priceSelectorRecipeWithFloor(ctx, rpc, s.RouteLane, inputs, s.Slot, observationFloor)
	if err != nil {
		return out, err
	}
	out.Recipe.ValidThroughSlot = min(out.Recipe.ValidThroughSlot, plan.ValidThroughSlot)
	for _, cost := range out.Recipe.Costs {
		observationFloor = max(observationFloor, cost.ObservationSlot)
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return out, err
	}
	if slot < observationFloor || slot > out.Recipe.ValidThroughSlot {
		return out, budgetHold("selector_recipe_observation_expired")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	out.Recipe.EvidenceID = sha256Bytes(raw)
	return out, nil
}
