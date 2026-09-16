package backyardrwa

import (
	"context"
	"time"
)

// observePhase3KnownBuildCost revalues the exact executable principal and
// unsigned-message fee before the production builder can load a signer.
// It measures cost without selecting a deployment budget. The locked durable
// admission/build/send boundaries enforce the current authorized caps. Account setup
// and the remaining exit graph still require independently bounded funding.
// Only the typed initializer admits its exact native rent in this gate.
func observePhase3KnownBuildCost(ctx context.Context, rpc *RPCClient, request any, effects ExpectedEffects) (ValuedTransactionCost, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	debit, err := MeasureExecutableDebit(request, effects)
	if err != nil {
		return ValuedTransactionCost{}, err
	}
	var message []byte
	var setupLamports uint64
	var initializerPrestateSlot int64
	lane := RouteID
	switch r := request.(type) {
	case BridgeBuildRequest:
		message, err = CompileBridgeMessage(r)
	case KaminoPrimeUSDCRequest:
		message, err = CompileKaminoMessage(r)
		lane = r.RouteLane
	case KaminoInitializationRequest:
		message, err = CompileKaminoInitializationMessage(r)
		lane, setupLamports = r.RouteLane, r.RentLamports
	case JupiterSwapRequest:
		message, err = CompileJupiterMessage(r)
		lane = r.RouteLane
	default:
		return ValuedTransactionCost{}, budgetHold("unmapped_economic_action")
	}
	if err != nil {
		return ValuedTransactionCost{}, err
	}
	if rpc == nil {
		return ValuedTransactionCost{}, budgetHold("build_valuation_unavailable")
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return ValuedTransactionCost{}, budgetHold("build_valuation_unavailable")
	}
	if r, ok := request.(KaminoInitializationRequest); ok {
		slot, err = validateKaminoInitializationPrestate(ctx, rpc, r, slot)
		initializerPrestateSlot = slot
		if err != nil {
			return ValuedTransactionCost{}, err
		}
	}
	if r, ok := request.(KaminoPrimeUSDCRequest); ok && r.FullPayoff {
		bound, err := validateFullPayoffRequest(ctx, rpc, r, effects, slot)
		if err != nil {
			return ValuedTransactionCost{}, err
		}
		slot = max(slot, bound.ObservedSlot)
	}
	if r, ok := request.(KaminoPrimeUSDCRequest); ok && effects.Deposit != nil {
		observed, err := validateDepositRequest(ctx, rpc, r, effects, slot)
		if err != nil {
			return ValuedTransactionCost{}, err
		}
		slot = max(slot, observed)
	}
	if r, ok := request.(KaminoPrimeUSDCRequest); ok && effects.Kind == "kamino-borrow" {
		observed, err := validateBorrowRequest(ctx, rpc, r, effects, slot)
		if err != nil {
			return ValuedTransactionCost{}, err
		}
		slot = max(slot, observed)
	}
	if r, ok := request.(KaminoPrimeUSDCRequest); ok && r.RepaymentRelease {
		bound, _, err := validateRepaymentReleaseRequest(ctx, rpc, r, effects, slot)
		if err != nil {
			return ValuedTransactionCost{}, err
		}
		slot = max(slot, bound.Payoff.ObservedSlot)
	}
	if r, ok := request.(JupiterSwapRequest); ok {
		if r.PositionReturnReserved {
			bound, _, err := validateLeverageSwap(ctx, rpc, r, effects, slot)
			if err != nil {
				return ValuedTransactionCost{}, err
			}
			slot = max(slot, bound.ObservedSlot)
		}
		if r.EntryReturnReserved {
			observed, err := validateEntrySwap(ctx, rpc, r, effects, slot)
			if err != nil {
				return ValuedTransactionCost{}, err
			}
			slot = max(slot, observed)
		}
		if r.FullPayoffFunding {
			bound, _, err := validatePayoffFunding(ctx, rpc, r, effects, slot, 3)
			if err != nil {
				return ValuedTransactionCost{}, err
			}
			slot = max(slot, bound.ObservedSlot)
		}
		slot, err = revalidateJupiterLookupTables(ctx, rpc, r, slot)
		if err != nil {
			return ValuedTransactionCost{}, err
		}
	}
	fee, err := rpc.ObserveMessageFee(ctx, message, slot)
	if err != nil {
		return ValuedTransactionCost{}, err
	}
	if r, ok := request.(KaminoInitializationRequest); ok && fee.Lamports > r.MaximumFeeLamports {
		return ValuedTransactionCost{}, budgetHold("initializer_fee_changed")
	}
	var token BudgetPrice
	if debit.Raw > 0 {
		token, err = ObserveBudgetTokenPrice(ctx, rpc, lane, debit, fee.Slot)
		if err != nil {
			return ValuedTransactionCost{}, budgetHold("build_token_valuation_unavailable")
		}
	}
	sol, err := ObserveNativeSOLBudgetPrice(ctx, rpc, max(fee.Slot, token.ObservedSlot))
	if err != nil {
		return ValuedTransactionCost{}, budgetHold("build_native_valuation_unavailable")
	}
	slot, err = rpc.ConfirmedSlot(ctx)
	if err != nil {
		return ValuedTransactionCost{}, budgetHold("build_valuation_unavailable")
	}
	if initializerPrestateSlot > 0 && (slot < initializerPrestateSlot || slot-initializerPrestateSlot > budgetMaxObservationLagSlots) {
		return ValuedTransactionCost{}, budgetHold("initializer_prestate_expired")
	}
	cost, err := ValueTransactionCost(message, debit, fee, setupLamports, token, sol, slot)
	if err != nil {
		return cost, err
	}
	if initializerPrestateSlot > 0 {
		cost.ValidThroughSlot = min(cost.ValidThroughSlot, initializerPrestateSlot+budgetMaxObservationLagSlots)
	}
	return cost, nil
}

// Every production builder uses this gate before signer access. Passing it
// does not create a reservation or authorize a missing setup/exit plan.
func authorizePhase3ProductionBuild(ctx context.Context, database *Database, rpc *RPCClient, operationID string, request any, effects ExpectedEffects, encodedEffects []byte) error {
	cost, err := observePhase3KnownBuildCost(ctx, rpc, request, effects)
	if err != nil {
		return err
	}
	return database.authorizePhase3Build(ctx, rpc, operationID, request, encodedEffects, cost)
}
