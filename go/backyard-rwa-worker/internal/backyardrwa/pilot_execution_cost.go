package backyardrwa

import (
	"context"
	"math"
	"reflect"
)

// An execution-cost bound is booked at its admitted upper value, never as
// claimed realized P&L. Principal/rent remains in the gross movement ledger.
// Interest, market P&L and performance fees remain in independently observed
// NAV; this counter does not purport to measure investment performance.
type PilotExecutionCost struct {
	NetworkMicros          int64        `json:"networkMicros"`
	ProtocolRoundingMicros int64        `json:"protocolRoundingMicros"`
	SwapLossMicros         int64        `json:"swapLossMicros"`
	TotalMicros            int64        `json:"totalMicros"`
	CreditPrice            *BudgetPrice `json:"creditPrice,omitempty"`
}

// Classify only compiler-supported movements. The existing prestate and final
// reconciliation gates establish conservation, fees and actual rent ownership.
// Missing bounds are a refusal, never permission to recycle the entire debit.
func classifyPilotExecutionCost(request any, effects ExpectedEffects, cost ValuedTransactionCost, credit *BudgetPrice) (PilotExecutionCost, error) {
	out := PilotExecutionCost{NetworkMicros: cost.NetworkFeeMicros}
	debit, err := MeasureExecutableDebit(request, effects)
	if err != nil {
		return out, err
	}
	gross, err := budgetSum(cost.PrincipalMicros, cost.NetworkFeeMicros, cost.SetupLamportsMicros)
	if err != nil || cost.NetworkFeeMicros <= 0 || cost.TotalMicros != gross || cost.Debit != debit {
		return out, budgetHold("execution_cost_gross_identity_mismatch")
	}
	var message []byte
	switch r := request.(type) {
	case BridgeBuildRequest:
		message, err = CompileBridgeMessage(r)
	case KaminoPrimeUSDCRequest:
		message, err = CompileKaminoMessage(r)
	case KaminoInitializationRequest:
		message, err = CompileKaminoInitializationMessage(r)
	case JupiterSwapRequest:
		message, err = CompileJupiterMessage(r)
	}
	if err != nil {
		return out, err
	}
	var token BudgetPrice
	if cost.TokenPrice != nil {
		token = *cost.TokenPrice
	}
	rebuilt, err := ValueTransactionCost(message, debit, cost.Fee, cost.SetupLamports, token, cost.NativePrice, cost.ObservationSlot)
	if err != nil {
		return out, err
	}
	if rebuilt.MessageSHA256 != cost.MessageSHA256 || rebuilt.PrincipalMicros != cost.PrincipalMicros || rebuilt.NetworkFeeMicros != cost.NetworkFeeMicros || rebuilt.SetupLamportsMicros != cost.SetupLamportsMicros || cost.ValidThroughSlot > rebuilt.ValidThroughSlot || cost.ObservationSlot > cost.ValidThroughSlot {
		return out, budgetHold("execution_cost_valuation_mismatch")
	}
	valueProtocol := func(raw uint64) error {
		if raw == 0 {
			return nil
		}
		if cost.TokenPrice == nil {
			return budgetHold("execution_cost_missing_token_price")
		}
		var e error
		out.ProtocolRoundingMicros, e = cost.TokenPrice.valueUpper(raw, debit.Mint, debit.TokenProgram, cost.ObservationSlot)
		return e
	}
	switch r := request.(type) {
	case KaminoInitializationRequest:
		if cost.SetupLamports != r.RentLamports || cost.PrincipalMicros != 0 || debit.Raw != 0 {
			return out, budgetHold("initializer_cost_rent_mismatch")
		}
		// The exact native reconciler proves rent moved into the new obligation.
	case BridgeBuildRequest:
		if cost.SetupLamports != 0 || (effects.Kind != "bridge" && effects.Kind != "") || !effects.Conserved {
			return out, budgetHold("unsupported_bridge_execution_cost")
		}
	case JupiterSwapRequest:
		if cost.SetupLamports != 0 || effects.Kind != "cross-mint-swap" || len(effects.Accounts) != 2 || credit == nil {
			return out, budgetHold("missing_swap_execution_cost_bound")
		}
		destination := effects.Accounts[1]
		if destination.AfterRaw < destination.BeforeRaw || destination.MinimumAfterRaw == nil || *destination.MinimumAfterRaw < destination.BeforeRaw || *destination.MinimumAfterRaw-destination.BeforeRaw != r.MinimumOutputRaw || destination.AfterRaw-destination.BeforeRaw != r.MinimumOutputRaw {
			return out, budgetHold("swap_minimum_credit_mismatch")
		}
		lower, err := credit.valueLower(r.MinimumOutputRaw, destination.Mint, destination.Owner, cost.ObservationSlot)
		if err != nil {
			return out, err
		}
		out.SwapLossMicros = max(0, cost.PrincipalMicros-lower)
		copied := *credit
		out.CreditPrice = &copied
	case KaminoPrimeUSDCRequest:
		if cost.SetupLamports != 0 {
			return out, budgetHold("unsupported_kamino_setup_cost")
		}
		_, leg, err := kaminoPrimeUSDCInstruction(r)
		if err != nil {
			return out, err
		}
		var roundingRaw uint64
		switch leg {
		case kaminoLegBorrow:
			if effects.Kind != "kamino-borrow" || debit.Raw < r.AmountRaw {
				return out, budgetHold("borrow_execution_fee_missing")
			}
			roundingRaw = debit.Raw - r.AmountRaw
		case kaminoLegDeposit:
			if effects.Deposit == nil || effects.Deposit.MaximumDebitRaw != r.AmountRaw || effects.Deposit.MinimumDebitRaw == 0 || effects.Deposit.MinimumDebitRaw > effects.Deposit.MaximumDebitRaw {
				return out, budgetHold("deposit_execution_rounding_missing")
			}
			// Deliberately book the whole independently bounded receipt-rounding
			// window, even when its unused input remains in wallet custody.
			roundingRaw = effects.Deposit.MaximumDebitRaw - effects.Deposit.MinimumDebitRaw
		case kaminoLegRepay:
			if effects.Repayment == nil || effects.Repayment.MinimumDebitRaw == 0 || effects.Repayment.MaximumDebitRaw < effects.Repayment.MinimumDebitRaw || effects.Repayment.MaximumDebitRaw != r.AmountRaw {
				return out, budgetHold("repayment_execution_rounding_missing")
			}
			roundingRaw = effects.Repayment.MaximumDebitRaw - effects.Repayment.MinimumDebitRaw + 1
		case kaminoLegWithdraw:
			// Supported KLend receipt redemption floors one liquidity-token unit.
			// Fees are absent from this pinned instruction; no other withdrawal
			// implementation can enter through this compiler.
			roundingRaw = 1
		default:
			return out, budgetHold("unclassified_kamino_execution_cost")
		}
		if roundingRaw > debit.Raw {
			return out, budgetHold("execution_rounding_exceeds_principal")
		}
		if err = valueProtocol(roundingRaw); err != nil {
			return out, err
		}
	default:
		return out, budgetHold("unclassified_execution_cost")
	}
	out.TotalMicros, err = budgetSum(out.NetworkMicros, out.ProtocolRoundingMicros, out.SwapLossMicros)
	if err != nil || out.TotalMicros <= 0 || out.TotalMicros > cost.TotalMicros {
		return out, budgetHold("invalid_execution_cost_bound")
	}
	return out, nil
}

func observePilotExecutionCost(ctx context.Context, rpc *RPCClient, request any, effects ExpectedEffects, cost ValuedTransactionCost) (ValuedTransactionCost, error) {
	var credit *BudgetPrice
	if r, ok := request.(JupiterSwapRequest); ok {
		if len(effects.Accounts) != 2 || r.MinimumOutputRaw == 0 {
			return cost, budgetHold("missing_swap_execution_cost_bound")
		}
		a := effects.Accounts[1]
		p, err := ObserveBudgetTokenPrice(ctx, rpc, r.RouteLane, ExecutableDebit{Source: a.Address, Mint: a.Mint, TokenProgram: a.Owner, Raw: r.MinimumOutputRaw}, cost.ObservationSlot)
		if err != nil {
			return cost, err
		}
		credit = &p
		cost.ObservationSlot = max(cost.ObservationSlot, p.ObservedSlot)
		cost.ValidThroughSlot = min(cost.ValidThroughSlot, p.ValidThroughSlot)
	}
	if rpc == nil {
		return cost, budgetHold("execution_cost_rpc_unavailable")
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return cost, err
	}
	if slot < cost.ObservationSlot || slot > cost.ValidThroughSlot || slot > math.MaxInt64-budgetMaxObservationLagSlots {
		return cost, budgetHold("execution_cost_observation_expired")
	}
	cost.ObservationSlot = slot
	bound, err := classifyPilotExecutionCost(request, effects, cost, credit)
	if err != nil {
		return cost, err
	}
	cost.ExecutionCost = &bound
	return cost, nil
}

// Both pre-signing and the locked final-send fence recompute the bound from
// the persisted executable input. Repricing can narrow an allowance, not grow it.
func validateReservedExecutionCost(budget Phase3Budget, reservation BudgetReservation, request any, effects ExpectedEffects, cost ValuedTransactionCost) error {
	if budget.Pilot == nil {
		return nil
	}
	if cost.ExecutionCost == nil {
		return budgetHold("missing_or_invalid_execution_cost_bound")
	}
	bound, err := classifyPilotExecutionCost(request, effects, cost, cost.ExecutionCost.CreditPrice)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(bound, *cost.ExecutionCost) {
		return budgetHold("execution_cost_classification_mismatch")
	}
	if reservation.ExecutionCostUpperMicros <= 0 || bound.TotalMicros > reservation.ExecutionCostUpperMicros {
		return budgetHold("fresh_execution_cost_exceeds_reservation")
	}
	return nil
}
