package backyardrwa

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"sync"
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
	return priceSelectorRecipeWithFloor(ctx, rpc, lane, inputs, minimumSlot, minimumSlot)
}

// Keep the sample start fixed while later prerequisites advance the minimum
// slot accepted for fees/prices and final collection.
func priceSelectorRecipeWithFloor(ctx context.Context, rpc *RPCClient, lane string, inputs []*phase3BuildInput, minimumSlot, observationFloor int64) (selectorRecipe, error) {
	out := selectorRecipe{Inputs: inputs}
	if rpc == nil || !selectorLane(lane) || len(inputs) == 0 || len(inputs) > 32 || minimumSlot <= 0 || minimumSlot > math.MaxInt64-budgetMaxObservationLagSlots || observationFloor < minimumSlot || observationFloor-minimumSlot > budgetMaxObservationLagSlots {
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
	slot := observationFloor
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
			slot, err = selectorSwapObservationFloor(r, minimumSlot, slot)
			if err != nil {
				return out, err
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
		steps[i] = step{request: request, effects: effects, message: message, debit: debit, rent: rent}
	}
	keys := make([]string, 0, len(sources))
	for k := range sources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Compile and validate the entire recipe before starting any read. All
	// exact-message fees and independent prices use the prerequisite floor;
	// retain their individual slots and validate them at the final slot.
	// Four reads per recipe bounds fanout even when all three lanes quote.
	floor := slot
	tokenPrices := make([]BudgetPrice, len(keys))
	var sol BudgetPrice
	if err := selectorRecipeReads(len(steps)+len(keys)+1, func(i int) (err error) {
		switch {
		case i < len(steps):
			steps[i].fee, err = rpc.ObserveMessageFee(ctx, steps[i].message, floor)
		case i < len(steps)+len(keys):
			j := i - len(steps)
			tokenPrices[j], err = ObserveBudgetTokenPrice(ctx, rpc, lane, sources[keys[j]], floor)
		default:
			sol, err = ObserveNativeSOLBudgetPrice(ctx, rpc, floor)
		}
		return err
	}); err != nil {
		return out, err
	}
	for _, s := range steps {
		if r, ok := s.request.(KaminoInitializationRequest); ok && s.fee.Lamports > r.MaximumFeeLamports {
			return out, budgetHold("initializer_fee_changed")
		}
		if s.rent > math.MaxUint64-out.SetupLamports || s.fee.Lamports > math.MaxUint64-out.NetworkLamports {
			return out, budgetHold("selector_recipe_native_overflow")
		}
		out.SetupLamports += s.rent
		out.NetworkLamports += s.fee.Lamports
		slot = max(slot, s.fee.Slot)
	}
	for i, k := range keys {
		prices[k] = tokenPrices[i]
		slot = max(slot, tokenPrices[i].ObservedSlot)
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

func selectorSwapObservationFloor(r JupiterSwapRequest, sampleSlot, floor int64) (int64, error) {
	if sampleSlot <= 0 || sampleSlot > math.MaxInt64-budgetMaxObservationLagSlots || floor < sampleSlot {
		return 0, budgetHold("selector_recipe_observation_expired")
	}
	for _, table := range r.LookupTables {
		if table.ObservedSlot < sampleSlot {
			return 0, budgetHold("selector_recipe_observation_expired")
		}
		floor = max(floor, table.ObservedSlot)
	}
	if floor > sampleSlot+budgetMaxObservationLagSlots {
		return 0, budgetHold("selector_recipe_observation_expired")
	}
	return floor, nil
}

// Each worker owns disjoint result indexes. Join every read, then return the
// first error in recipe order, so completion order cannot hide a failed input.
func selectorRecipeReads(count int, read func(int) error) error {
	errs := make([]error, count)
	workers := min(4, count)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < count; i += workers {
				errs[i] = read(i)
			}
		}(worker)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
