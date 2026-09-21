package backyardrwa

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"sort"
	"strconv"
	"sync"
)

// A recipe is an economic forecast of a sequence, not an execution admission.
// Future inputs contain explicit hypothetical token effects. They are never
// simulated using invented accounts, persisted as operations, or sent. Actual
// execution rebuilds each step from observed balances and its existing budget.
type selectorRecipe struct {
	Inputs  []*phase3BuildInput     `json:"inputs"`
	Costs   []ValuedTransactionCost `json:"costs"`
	CostRaw int64                   `json:"costRaw"`
	// ExpectedCostRaw is the forecast economic expense at central observed
	// prices, always present on freshly priced recipes (zero stays zero, never
	// an unknown). Every admission, reservation and spending bound keeps the
	// conservative CostRaw upper exposure bound; only selector net-yield
	// comparison reads this.
	ExpectedCostRaw  *int64 `json:"expectedCostRaw,omitempty"`
	SetupLamports    uint64 `json:"setupLamports"`
	NetworkLamports  uint64 `json:"networkLamports"`
	ValidThroughSlot int64  `json:"validThroughSlot"`
	EvidenceID       string `json:"evidenceId"`
}

// selectorMidpointPriceValue reconstructs the central observed USDC value of a
// token amount from the symmetric independently observed upper/lower interval
// components of one BudgetPrice, using integer arithmetic only. It prices
// expectations, never bounds: admissions and reservations stay on CostRaw.
func selectorMidpointPriceValue(p BudgetPrice, raw uint64, mint, program string, slot int64) (int64, error) {
	if raw == 0 || p.Mint != mint || p.TokenProgram != program || p.Decimals > 18 || p.ObservedSlot <= 0 || slot < p.ObservedSlot || slot > p.ValidThroughSlot || p.ValidThroughSlot < p.ObservedSlot || p.ValidThroughSlot-p.ObservedSlot > budgetMaxObservationLagSlots || !sha256Pattern.MatchString(p.EvidenceSHA256) {
		return 0, budgetHold("missing_stale_or_mismatched_usdc_valuation")
	}
	if p.Credit == nil {
		return 0, budgetHold("missing_credit_valuation_bounds")
	}
	tokenUpper, tokenLower := littleInt(p.TokenUpperSF[:]), littleInt(p.Credit.TokenLowerSF[:])
	usdcLower, usdcUpper := littleInt(p.USDCLowerSF[:]), littleInt(p.Credit.USDCUpperSF[:])
	if tokenLower.Cmp(tokenUpper) > 0 || usdcUpper.Cmp(usdcLower) < 0 || tokenUpper.Sign() <= 0 || tokenLower.Sign() <= 0 || usdcLower.Sign() <= 0 || usdcUpper.Sign() <= 0 {
		return 0, budgetHold("invalid_credit_valuation_interval")
	}
	var tokenMid, usdcMid [16]byte
	putScaledLittleSF(&tokenMid, new(big.Int).Quo(new(big.Int).Add(tokenUpper, tokenLower), big.NewInt(2)))
	putScaledLittleSF(&usdcMid, new(big.Int).Quo(new(big.Int).Add(usdcLower, usdcUpper), big.NewInt(2)))
	value, err := valueBetweenTokenRaw(raw, p.Decimals, 6, tokenMid, usdcMid, false)
	if err != nil || value > math.MaxInt64 {
		return 0, budgetHold("invalid_usdc_valuation")
	}
	return int64(value), nil
}

func putScaledLittleSF(out *[16]byte, value *big.Int) {
	if value.Sign() <= 0 || value.BitLen() > 128 {
		return
	}
	bytes := value.Bytes()
	for i := range bytes {
		out[i] = bytes[len(bytes)-1-i]
	}
}

// composeSelectorExpectedExpense combines each recipe's expected expense,
// falling back per recipe to its conservative CostRaw upper bound when no
// forecast is present.
// A flat idle source (zero bound, no forecast pointer) therefore contributes
// zero and cannot erase a valid destination forecast, while an old source
// recipe degrades conservatively to its own bound.
func composeSelectorExpectedExpense(source, destination selectorRecipe) (int64, error) {
	expected := func(r selectorRecipe) int64 {
		if r.ExpectedCostRaw != nil && *r.ExpectedCostRaw >= 0 && *r.ExpectedCostRaw <= r.CostRaw {
			return *r.ExpectedCostRaw
		}
		return r.CostRaw
	}
	return budgetSum(expected(source), expected(destination))
}

// Price every compiled message, including repeated NAV reports. Share only
// observed prices within this bounded sample; fees still bind exact messages.
// The production build/send path keeps its actual custody/prestate validators.
func priceSelectorRecipe(ctx context.Context, rpc *RPCClient, lane string, inputs []*phase3BuildInput, minimumSlot int64) (selectorRecipe, error) {
	return priceSelectorRecipeWithFloor(ctx, rpc, lane, inputs, minimumSlot, minimumSlot)
}

// Keep the sample start fixed while later prerequisites advance the minimum
// slot accepted for fees/prices and final collection. The public entry keeps
// the plain selector-lane gate and prices against the embedded manifest,
// which carries no AUTO binding, so the persisted-build path stays closed.
func priceSelectorRecipeWithFloor(ctx context.Context, rpc *RPCClient, lane string, inputs []*phase3BuildInput, minimumSlot, observationFloor int64) (selectorRecipe, error) {
	if !selectorLane(lane) {
		return selectorRecipe{Inputs: inputs}, budgetHold("invalid_selector_recipe")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return selectorRecipe{Inputs: inputs}, err
	}
	return manifest.priceSelectorRecipeWithFloor(ctx, rpc, lane, inputs, minimumSlot, observationFloor)
}

// The manifest-aware internal form prices retained recipe inputs against the
// SAME manifest that produced them, so AUTO payoff legs bound through a
// validated autoPolicy binding can enter the identical cost machinery. Beyond
// the reviewed selector lanes it admits exactly the AUTO lane and only when
// this manifest itself carries a fully validated binding — never a
// request-supplied one — and this enables no live selection: AUTO stays out
// of selectorLanes/selectorEntryLane.
func (m RouteManifest) priceSelectorRecipeWithFloor(ctx context.Context, rpc *RPCClient, lane string, inputs []*phase3BuildInput, minimumSlot, observationFloor int64) (selectorRecipe, error) {
	out := selectorRecipe{Inputs: inputs}
	authorized := selectorLane(lane)
	if !authorized && lane == autoAUTOPYUSD.Lane {
		if _, err := m.autoPolicyBinding(); err == nil {
			authorized = true
		}
	}
	if rpc == nil || !authorized || len(inputs) == 0 || len(inputs) > 32 || minimumSlot <= 0 || minimumSlot > math.MaxInt64-budgetMaxObservationLagSlots || observationFloor < minimumSlot || observationFloor-minimumSlot > budgetMaxObservationLagSlots {
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
		request, effects, message, err := input.decodeWithManifest(m)
		if err != nil {
			return out, err
		}
		debit, err := m.measureExecutableDebit(request, effects)
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
	expectedTotal := int64(0)
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
		expense, err := m.classifyPilotExecutionCost(s.request, s.effects, cost, credit)
		if err != nil {
			return out, err
		}
		cost.ExecutionCost = &expense
		// The expected swap expense prices the validated pre-threshold quote
		// output at central observed prices. The guaranteed MinimumOutputRaw
		// bound above remains the reservation and admission floor; fees and
		// protocol rounding keep their bound expense on every step.
		stepExpected := expense.TotalMicros
		if r, ok := s.request.(JupiterSwapRequest); ok {
			// The pre-threshold quoted output was already compiled and bound to
			// the guaranteed minimum by input.decode; only the safe ordering is
			// rechecked here — no redundant floor multiplication that could
			// overflow at u64 edges.
			if r.QuotedOutputRaw < r.MinimumOutputRaw {
				return out, budgetHold("selector_expected_quote_invalid")
			}
			if credit == nil {
				return out, budgetHold("missing_swap_execution_cost_bound")
			}
			dst := s.effects.Accounts[1]
			inValue, err := selectorMidpointPriceValue(prices[key(s.debit)], s.debit.Raw, s.debit.Mint, s.debit.TokenProgram, nowSlot)
			if err != nil {
				return out, err
			}
			outValue, err := selectorMidpointPriceValue(*credit, r.QuotedOutputRaw, dst.Mint, dst.Owner, nowSlot)
			if err != nil {
				return out, err
			}
			loss := int64(0)
			if inValue > outValue {
				loss = inValue - outValue
			}
			if stepExpected, err = budgetSum(expense.NetworkMicros, loss); err != nil {
				return out, err
			}
			if stepExpected > expense.TotalMicros {
				stepExpected = expense.TotalMicros
			}
		}
		out.CostRaw, err = budgetSum(out.CostRaw, expense.TotalMicros)
		if err != nil {
			return out, err
		}
		if expectedTotal, err = budgetSum(expectedTotal, stepExpected); err != nil {
			return out, err
		}
		out.ValidThroughSlot = min(out.ValidThroughSlot, cost.ValidThroughSlot)
		out.Costs = append(out.Costs, cost)
	}
	if nowSlot > out.ValidThroughSlot {
		return out, budgetHold("selector_recipe_observation_expired")
	}
	if expectedTotal > out.CostRaw {
		expectedTotal = out.CostRaw
	}
	out.ExpectedCostRaw = &expectedTotal
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

// selectorPayoffObservationFloor intersects every payoff swap leg into the
// observation window with the SAME budgetMaxObservationLagSlots bound a
// single-leg payoff used — horizons are never loosened for extra legs. A leg
// whose evidence predates the sample or exceeds the window holds with the
// same reason and Details recording the exact uncovered interval.
func selectorPayoffObservationFloor(sampleSlot, floor int64, legs ...JupiterExecutionEvidence) (int64, error) {
	for i, leg := range legs {
		observed, stale := floor, int64(0)
		for _, table := range leg.Request.LookupTables {
			observed = max(observed, table.ObservedSlot)
			if table.ObservedSlot < sampleSlot && (stale == 0 || table.ObservedSlot < stale) {
				stale = table.ObservedSlot
			}
		}
		legFloor, err := selectorSwapObservationFloor(leg.Request, sampleSlot, floor)
		if err == nil {
			floor = legFloor
			continue
		}
		if hold, ok := err.(*BudgetHold); ok {
			details := map[string]string{
				"legIndex":         strconv.FormatInt(int64(i), 10),
				"legObservedFloor": strconv.FormatInt(observed, 10),
			}
			if stale > 0 {
				details["coveredThroughSlot"] = strconv.FormatInt(stale, 10)
				details["uncoveredSlots"] = strconv.FormatInt(sampleSlot-stale, 10)
			} else {
				details["coveredThroughSlot"] = strconv.FormatInt(sampleSlot+budgetMaxObservationLagSlots, 10)
				details["uncoveredSlots"] = strconv.FormatInt(observed-(sampleSlot+budgetMaxObservationLagSlots), 10)
			}
			hold.Details = details
		}
		return 0, err
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
