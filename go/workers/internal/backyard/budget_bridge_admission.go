package backyard

import (
	"bytes"
	"context"
	"math"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Exit templates are fee/debit estimates, not future signable instructions.
// Their fixed-width report fields must be regenerated from the actual poststate
// before execution. No template is promoted to BuildInput or sent on recovery.
type phase3BridgeExitCost struct {
	Action Action                `json:"action"`
	Amount uint64                `json:"amountRaw"`
	Cost   ValuedTransactionCost `json:"cost"`
	// Cost-only compiled template; never an executable future operation.
	Template *phase3BuildInput `json:"template,omitempty"`
}

type phase3BridgeAdmission struct {
	Snapshot         Snapshot               `json:"snapshot"`
	Decision         Decision               `json:"decision"`
	Input            *phase3BuildInput      `json:"input"`
	CurrentCost      ValuedTransactionCost  `json:"currentCost"`
	Exit             []phase3BridgeExitCost `json:"exit"`
	ExitAfterMicros  int64                  `json:"exitAfterMicros"`
	ValidThroughSlot int64                  `json:"validThroughSlot"`
	Payoff           *KaminoPayoffBound     `json:"payoff,omitempty"`
	PayoffRepayment  *phase3BuildInput      `json:"payoffRepayment,omitempty"`
}

// This first admission shape is deliberately closed over existing USDC bridge
// custody with no selected-lane collateral/debt. It cannot price a position or
// a swap exit, initialize an account, adopt historical exposure, or reset funds.
func phase3BridgeTemplates(policies installedPolicies, s Snapshot, decision Decision, evidence BridgeExecutionEvidence) ([]BridgeExecutionEvidence, error) {
	r := evidence.Request
	if !s.Fresh || s.Slot <= 0 || s.Slot > math.MaxInt64-budgetMaxObservationLagCeilingSlots || s.RouteKind != RouteKind ||
		s.ManualReason != "" || s.HasAmbiguousSubmission || s.Nonterminal != "" || s.CutoverDrain ||
		s.StrategyKey != s.RouteLane || decision.StrategyKey != s.RouteLane || !fundedLane(s.RouteLane) {
		return nil, budgetHold("bridge_admission_snapshot_unavailable")
	}
	if _, err := runtimeRoute(s.RouteLane); err != nil {
		return nil, budgetHold("bridge_admission_lane_unavailable")
	}
	if s.HasPosition || s.PositionCollateralRaw != 0 || s.PositionDebtRaw != 0 ||
		s.PrimeIdleRaw != 0 || s.CollateralIdleRaw != 0 || s.DebtIdleRaw != 0 ||
		s.PositionCollateralValueRaw != 0 || s.PositionDebtValueRaw != 0 {
		return nil, budgetHold("complete_position_exit_admission_unavailable")
	}
	if s.VoltrIdleRaw < 0 || s.VoltrStrategyIdleRaw < 0 || s.SquadsIdleRaw < 0 ||
		decision.AmountRaw < 0 || r.Action != decision.Action || r.AmountRaw != uint64(decision.AmountRaw) ||
		r.Report.ObservedSlot != uint64(s.Slot) || r.Report.Sequence != uint64(s.Slot) {
		return nil, budgetHold("bridge_admission_intent_mismatch")
	}
	idle, strategy, squads := uint64(s.VoltrIdleRaw), uint64(s.VoltrStrategyIdleRaw), uint64(s.SquadsIdleRaw)
	if r.Action == VoltrAllocateToSquads && (strategy != 0 || squads != 0) {
		return nil, budgetHold("bridge_allocation_requires_empty_strategy_custody")
	}
	if (r.Action == VoltrRestoreIdle && r.AmountRaw != strategy) ||
		(r.Action == StageSquadsToVoltr && r.AmountRaw != squads) {
		return nil, budgetHold("bridge_admission_requires_full_custody_exit")
	}
	makeStep := func(action Action, amount uint64) (BridgeExecutionEvidence, error) {
		if amount > math.MaxInt64 {
			return BridgeExecutionEvidence{}, budgetHold("bridge_exit_amount_overflow")
		}
		effects, afterStrategy, afterSquads, err := bridgeExpectedEffects(Decision{Action: action, AmountRaw: int64(amount)}, idle, strategy, squads)
		if err != nil {
			return BridgeExecutionEvidence{}, err
		}
		if afterStrategy > math.MaxUint64-afterSquads {
			return BridgeExecutionEvidence{}, budgetHold("bridge_exit_amount_overflow")
		}
		policy, err := policies.account(policyKey{action: action})
		if err != nil {
			return BridgeExecutionEvidence{}, err
		}
		next := r
		next.Action, next.AmountRaw, next.Policy = action, amount, policy
		// Voltr tracks strategy custody separately, so reported NAV excludes it.
		next.Report.NAVAfterRaw = afterSquads
		effects.Kind = "bridge"
		if action != StageSquadsToVoltr {
			effects.ReturnData = expectedAdaptorReturnData(next.Report.NAVAfterRaw)
		}
		if _, err = MeasureExecutableDebit(next, effects); err != nil {
			return BridgeExecutionEvidence{}, err
		}
		if action == VoltrAllocateToSquads {
			idle -= amount
		} else if action == VoltrRestoreIdle {
			idle += amount
		}
		strategy, squads = afterStrategy, afterSquads
		return BridgeExecutionEvidence{next, effects}, nil
	}
	first, err := makeStep(r.Action, r.AmountRaw)
	if err != nil {
		return nil, err
	}
	expected, err := jsonMarshalExpectedEffects(first.ExpectedEffects)
	if err != nil {
		return nil, err
	}
	actual, err := jsonMarshalExpectedEffects(evidence.ExpectedEffects)
	if err != nil || !bytes.Equal(expected, actual) || first.Request != r {
		return nil, budgetHold("bridge_admission_intent_mismatch")
	}
	steps := []BridgeExecutionEvidence{evidence}
	appendStep := func(action Action, amount uint64) error {
		step, err := makeStep(action, amount)
		if err == nil {
			steps = append(steps, step)
		}
		return err
	}
	// The existing journal demands a separate report after every capital
	// mutation, even when the bridge transaction also updated Voltr's NAV.
	if r.Action != ReportNAV {
		if err = appendStep(ReportNAV, 0); err != nil {
			return nil, err
		}
	}
	if squads > 0 {
		if err = appendStep(StageSquadsToVoltr, squads); err != nil {
			return nil, err
		}
		if err = appendStep(ReportNAV, 0); err != nil {
			return nil, err
		}
	}
	if strategy > 0 {
		if err = appendStep(VoltrRestoreIdle, strategy); err != nil {
			return nil, err
		}
		if err = appendStep(ReportNAV, 0); err != nil {
			return nil, err
		}
	}
	return steps, nil
}

// Derive prices once and ask the RPC for each compiled message's fee. All
// inputs, including the construction snapshot, must still be fresh together.
// Existing bridge builders contain no account creation or protocol fee debit;
// preparation verifies the existing custodies, adaptor, ticket and policies.
func observePhase3BridgeAdmission(ctx context.Context, rpc *chain.Client, view *View, observation Observation, decision Decision, evidence BridgeExecutionEvidence) (phase3BridgeAdmission, error) {
	return observePhase3BridgeAdmissionWindow(ctx, rpc, view, observation, decision, evidence, min(observationLagSlots(), adaptorMaxReportAgeSlots))
}

// observePhase3BridgeTemplateAdmission prices a follow-up NAV report that is
// never sent as priced: payoff and withdrawal pricers use it only for the
// report fee of their exit plan, and the real report is prepared again later. The adaptor's 32-slot report age limit therefore does not bound
// the current (Kamino) wire; the ordinary observation window does. Capping it
// at 32 slots (2fc768f) made every AUTO repayment expire before send, since
// that tick takes ~14 s (live 2026-09-28 13:33-13:39).
func observePhase3BridgeTemplateAdmission(ctx context.Context, rpc *chain.Client, view *View, observation Observation, decision Decision, evidence BridgeExecutionEvidence) (phase3BridgeAdmission, error) {
	return observePhase3BridgeAdmissionWindow(ctx, rpc, view, observation, decision, evidence, observationLagSlots())
}

func observePhase3BridgeAdmissionWindow(ctx context.Context, rpc *chain.Client, view *View, observation Observation, decision Decision, evidence BridgeExecutionEvidence, windowSlots int64) (phase3BridgeAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	plan := phase3BridgeAdmission{Snapshot: observation.Snapshot, Decision: decision}
	steps, err := phase3BridgeTemplates(observation.policies, observation.Snapshot, decision, evidence)
	if err != nil {
		return plan, err
	}
	if rpc == nil {
		return plan, budgetHold("bridge_admission_valuation_unavailable")
	}
	encoded, err := jsonMarshalExpectedEffects(evidence.ExpectedEffects)
	if err != nil {
		return plan, err
	}
	plan.Input, err = encodePhase3BuildInput(evidence.Request, encoded)
	if err != nil {
		return plan, err
	}
	slot, err := confirmedSlot(ctx, rpc)
	if err != nil {
		return plan, err
	}
	debits := make([]ExecutableDebit, len(steps))
	messages := make([][]byte, len(steps))
	fees := make([]MessageFeeObservation, len(steps))
	firstDebit := -1
	for i, step := range steps {
		debits[i], err = MeasureExecutableDebit(step.Request, step.ExpectedEffects)
		if err != nil {
			return plan, err
		}
		messages[i], err = CompileBridgeMessage(step.Request)
		if err != nil {
			return plan, err
		}
		if debits[i].Raw > 0 && firstDebit < 0 {
			firstDebit = i
		}
	}
	// These valuation reads have no dependency on one another. The closed
	// cash-return graph has at most six fee messages and two prices. Give
	// every read the same confirmed floor, then validate every result against
	// a final confirmed slot; an earlier response never extends freshness.
	minimumSlot := max(slot, observation.Snapshot.Slot)
	feeErrors := make([]error, len(steps))
	var token, sol BudgetPrice
	var tokenErr, solErr error
	var reads sync.WaitGroup
	for i := range steps {
		reads.Add(1)
		go func(i int) {
			defer reads.Done()
			fees[i], feeErrors[i] = observeMessageFee(ctx, rpc, messages[i], minimumSlot)
		}(i)
	}
	if firstDebit >= 0 {
		reads.Add(1)
		go func() {
			defer reads.Done()
			token, tokenErr = ObserveBudgetTokenPrice(ctx, rpc, view, RouteID, debits[firstDebit], minimumSlot)
		}()
	}
	reads.Add(1)
	go func() {
		defer reads.Done()
		sol, solErr = ObserveNativeSOLBudgetPrice(ctx, rpc, view, minimumSlot)
	}()
	reads.Wait()
	for _, feeErr := range feeErrors {
		if feeErr != nil {
			return plan, feeErr
		}
	}
	if tokenErr != nil {
		return plan, budgetHold("bridge_admission_token_valuation_unavailable")
	}
	if solErr != nil {
		return plan, budgetHold("bridge_admission_native_valuation_unavailable")
	}
	slot, err = confirmedSlot(ctx, rpc)
	if err != nil {
		return plan, err
	}
	// Every bridge wire carries a report for this snapshot slot, and the adaptor
	// refuses a report older than adaptorMaxReportAgeSlots (Custom 9): the wider
	// A1 window must not apply here.
	plan.ValidThroughSlot = observation.Snapshot.Slot + windowSlots
	if slot < observation.Snapshot.Slot || slot > plan.ValidThroughSlot {
		return plan, budgetHold("stale_bridge_admission_snapshot")
	}
	for i, step := range steps {
		cost, err := ValueTransactionCost(messages[i], debits[i], fees[i], 0, token, sol, slot)
		if err != nil {
			return plan, err
		}
		plan.ValidThroughSlot = min(plan.ValidThroughSlot, cost.ValidThroughSlot)
		if i == 0 {
			plan.CurrentCost = cost
		} else {
			raw, err := jsonMarshalExpectedEffects(step.ExpectedEffects)
			if err != nil {
				return plan, err
			}
			template, err := encodePhase3BuildInput(step.Request, raw)
			if err != nil {
				return plan, err
			}
			plan.Exit = append(plan.Exit, phase3BridgeExitCost{Action: step.Request.Action, Amount: step.Request.AmountRaw, Cost: cost, Template: template})
			plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros, cost.TotalMicros)
			if err != nil {
				return plan, err
			}
		}
	}
	return plan, nil
}
