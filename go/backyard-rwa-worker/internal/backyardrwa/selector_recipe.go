package backyardrwa

import (
	"context"
	"encoding/json"
	"math"
	"sort"
)

// A recipe is an economic forecast of a sequence, not an execution admission.
// Future inputs contain explicit hypothetical token effects. They are never
// simulated using invented accounts, persisted as operations, or sent. Actual
// execution rebuilds each step from observed balances and its existing budget.
type selectorRecipe struct {
	Inputs           []*phase3BuildInput     `json:"inputs"`
	Costs            []ValuedTransactionCost `json:"costs"`
	CostRaw          int64                   `json:"costRaw"`
	SetupLamports    uint64                  `json:"setupLamports"`
	NetworkLamports  uint64                  `json:"networkLamports"`
	ValidThroughSlot int64                   `json:"validThroughSlot"`
	EvidenceID       string                  `json:"evidenceId"`
}

// Price every compiled message, including repeated NAV reports. Share only
// observed prices within this bounded sample; fees still bind exact messages.
// The production build/send path keeps its actual custody/prestate validators.
func priceSelectorRecipe(ctx context.Context, rpc *RPCClient, lane string, inputs []*phase3BuildInput, minimumSlot int64) (selectorRecipe, error) {
	out := selectorRecipe{Inputs: inputs}
	if rpc == nil || !selectorLane(lane) || len(inputs) == 0 || len(inputs) > 32 || minimumSlot <= 0 || minimumSlot > math.MaxInt64-budgetMaxObservationLagSlots {
		return out, budgetHold("invalid_selector_recipe")
	}
	type step struct {
		request any
		effects ExpectedEffects
		message []byte
		debit   ExecutableDebit
		fee     MessageFeeObservation
		rent    uint64
	}
	steps := make([]step, len(inputs))
	prices := map[string]BudgetPrice{}
	sources := map[string]ExecutableDebit{}
	key := func(d ExecutableDebit) string { return d.Mint + ":" + d.TokenProgram }
	slot := minimumSlot
	out.ValidThroughSlot = minimumSlot + budgetMaxObservationLagSlots
	for i, input := range inputs {
		request, effects, message, err := input.decode()
		if err != nil {
			return out, err
		}
		debit, err := MeasureExecutableDebit(request, effects)
		if err != nil {
			return out, err
		}
		var rent uint64
		switch r := request.(type) {
		case BridgeBuildRequest:
			if r.Action != VoltrAllocateToSquads && r.Action != ReportNAV && r.Action != StageSquadsToVoltr && r.Action != VoltrRestoreIdle {
				return out, budgetHold("unsupported_selector_recipe_step")
			}
		case KaminoPrimeUSDCRequest:
			if r.RouteLane != lane {
				return out, budgetHold("selector_recipe_lane_mismatch")
			}
		case JupiterSwapRequest:
			if r.RouteLane != lane || len(effects.Accounts) != 2 {
				return out, budgetHold("selector_recipe_lane_mismatch")
			}
			dst := effects.Accounts[1]
			credit := ExecutableDebit{Source: dst.Address, Mint: dst.Mint, TokenProgram: dst.Owner, Raw: r.MinimumOutputRaw}
			if credit.Raw == 0 {
				return out, budgetHold("selector_recipe_missing_credit")
			}
			sources[key(credit)] = credit
		case KaminoInitializationRequest:
			if r.RouteLane != lane {
				return out, budgetHold("selector_recipe_lane_mismatch")
			}
			rent = r.RentLamports
		default:
			return out, budgetHold("unsupported_selector_recipe_step")
		}
		if debit.Raw > 0 {
			sources[key(debit)] = debit
		}
		fee, err := rpc.ObserveMessageFee(ctx, message, slot)
		if err != nil {
			return out, err
		}
		if r, ok := request.(KaminoInitializationRequest); ok && fee.Lamports > r.MaximumFeeLamports {
			return out, budgetHold("initializer_fee_changed")
		}
		if rent > math.MaxUint64-out.SetupLamports || fee.Lamports > math.MaxUint64-out.NetworkLamports {
			return out, budgetHold("selector_recipe_native_overflow")
		}
		out.SetupLamports += rent
		out.NetworkLamports += fee.Lamports
		steps[i] = step{request, effects, message, debit, fee, rent}
		slot = max(slot, fee.Slot)
	}
	keys := make([]string, 0, len(sources))
	for k := range sources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p, err := ObserveBudgetTokenPrice(ctx, rpc, lane, sources[k], slot)
		if err != nil {
			return out, err
		}
		prices[k] = p
		slot = max(slot, p.ObservedSlot)
	}
	sol, err := ObserveNativeSOLBudgetPrice(ctx, rpc, slot)
	if err != nil {
		return out, err
	}
	slot = max(slot, sol.ObservedSlot)
	nowSlot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return out, err
	}
	if nowSlot < slot || nowSlot > out.ValidThroughSlot {
		return out, budgetHold("selector_recipe_observation_expired")
	}
	for _, s := range steps {
		cost, err := ValueTransactionCost(s.message, s.debit, s.fee, s.rent, prices[key(s.debit)], sol, nowSlot)
		if err != nil {
			return out, err
		}
		var credit *BudgetPrice
		if _, ok := s.request.(JupiterSwapRequest); ok {
			dst := s.effects.Accounts[1]
			p := prices[key(ExecutableDebit{Mint: dst.Mint, TokenProgram: dst.Owner})]
			credit = &p
			cost.ValidThroughSlot = min(cost.ValidThroughSlot, p.ValidThroughSlot)
		}
		expense, err := classifyPilotExecutionCost(s.request, s.effects, cost, credit)
		if err != nil {
			return out, err
		}
		cost.ExecutionCost = &expense
		out.CostRaw, err = budgetSum(out.CostRaw, expense.TotalMicros)
		if err != nil {
			return out, err
		}
		out.ValidThroughSlot = min(out.ValidThroughSlot, cost.ValidThroughSlot)
		out.Costs = append(out.Costs, cost)
	}
	if nowSlot > out.ValidThroughSlot {
		return out, budgetHold("selector_recipe_observation_expired")
	}
	raw, err := json.Marshal(struct {
		Kind, Lane  string
		MinimumSlot int64
		Recipe      selectorRecipe
	}{"ECONOMIC_FORECAST_NOT_EXECUTION", lane, minimumSlot, out})
	if err != nil {
		return out, err
	}
	out.EvidenceID = sha256Bytes(raw)
	return out, nil
}
