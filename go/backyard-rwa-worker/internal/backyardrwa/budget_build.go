package backyardrwa

import (
	"context"
	"strconv"
	"time"
)

// observePhase3KnownBuildCost revalues the exact executable principal and
// unsigned-message fee before the production builder can load a signer.
// It is a rejection/revalidation gate, NOT complete admission: account setup
// and the remaining exit graph still require independently bounded funding.
// In particular, setupLamports=0 here does not certify that setup is free.
func observePhase3KnownBuildCost(ctx context.Context, rpc *RPCClient, request any, effects ExpectedEffects) (ValuedTransactionCost, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	debit, err := MeasureExecutableDebit(request, effects)
	if err != nil {
		return ValuedTransactionCost{}, err
	}
	var message []byte
	lane := RouteID
	switch r := request.(type) {
	case BridgeBuildRequest:
		message, err = CompileBridgeMessage(r)
	case KaminoPrimeUSDCRequest:
		message, err = CompileKaminoMessage(r)
		lane = r.RouteLane
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
	if r, ok := request.(KaminoPrimeUSDCRequest); ok && r.FullPayoff {
		bound, err := validateFullPayoffRequest(ctx, rpc, r, effects, slot)
		if err != nil {
			return ValuedTransactionCost{}, err
		}
		slot = max(slot, bound.ObservedSlot)
	}
	if r, ok := request.(JupiterSwapRequest); ok {
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
	cost, err := ValueTransactionCost(message, debit, fee, 0, token, sol, slot)
	if err != nil {
		return cost, err
	}
	if cost.TotalMicros > Phase3TransactionCapMicros {
		return cost, &BudgetHold{Reason: "transaction_cap_exceeded", Details: map[string]string{
			"knownCostMicros":       strconv.FormatInt(cost.TotalMicros, 10),
			"principalMicros":       strconv.FormatInt(cost.PrincipalMicros, 10),
			"networkFeeMicros":      strconv.FormatInt(cost.NetworkFeeMicros, 10),
			"transactionCapMicros":  strconv.FormatInt(Phase3TransactionCapMicros, 10),
			"observationSlot":       strconv.FormatInt(cost.ObservationSlot, 10),
			"messageSha256":         cost.MessageSHA256,
			"tokenValuationSha256":  token.EvidenceSHA256,
			"nativeValuationSha256": sol.EvidenceSHA256,
			"coverage":              "principal_and_network_fee_only_setup_and_exit_not_admitted",
		}}
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
	return database.authorizePhase3Build(ctx, operationID, request, encodedEffects, cost)
}
