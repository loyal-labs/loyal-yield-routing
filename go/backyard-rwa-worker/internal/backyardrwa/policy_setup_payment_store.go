package backyardrwa

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// Call under the existing route lock. The setup pointer, action, reservation
// shape and (for creation continuation) settled parent must all agree. A valid
// hash alone cannot turn a lifecycle reservation into setup authority.
func (d *Database) validatePolicySetupReservationTx(ctx context.Context, tx pgx.Tx, id string, budget Phase3Budget, auth phase3OperationAuthorization, status OperationStatus) (Action, error) {
	var action Action
	var recordedStatus OperationStatus
	var lane, pointer, route string
	var decisionRaw []byte
	err := tx.QueryRow(ctx, `SELECT o.action,o.status,o.strategy_key,o.route_key,o.expected_effects->'decision',COALESCE(r.state->>'phase3SetupIntent','') FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states r USING(route_key) WHERE o.operation_id=$1`, id).Scan(&action, &recordedStatus, &lane, &route, &decisionRaw, &pointer)
	if err != nil {
		return action, err
	}
	input, err := policySetupPaymentInput(auth, action)
	if err != nil {
		return action, err
	}
	var decision decisionEvidence
	if recordedStatus != status || lane != "OnRe/ONyc/USDC" || json.Unmarshal(decisionRaw, &decision) != nil || decision.StrategyKey != lane || decision.Reason != "phase3_policy_setup" || decision.AmountRaw <= 0 || uint64(decision.AmountRaw) != input.Cost.SetupLamports {
		return action, budgetHold("setup_payment_journal_mismatch")
	}
	if err = budget.AuthorizeIntent(id, auth.IntentSHA256); err != nil {
		return action, err
	}
	root := id
	expected := BudgetReservation{OperationID: id, Family: "OnRe", IntentSHA256: auth.IntentSHA256, UpperMicros: input.Cost.TotalMicros}
	if action == PolicySetupPrefund {
		expected.ExitAfterMicros = Phase3TransactionCapMicros
	}
	if auth.PolicySetupCompletion != nil {
		root = auth.PolicySetupCompletion.PrefundOperationID
		expected.Recovery = true
		expected.ExitBeforeMicros = Phase3TransactionCapMicros
		var finalized bool
		var parentRaw []byte
		if err = tx.QueryRow(ctx, `SELECT status='reconciled' AND confirmation_status='finalized',expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND route_key=$2 AND action='POLICY_SETUP_PREFUND'`, root, route).Scan(&finalized, &parentRaw); err != nil {
			return action, err
		}
		var parent phase3OperationAuthorization
		if !finalized || json.Unmarshal(parentRaw, &parent) != nil || parent.GoalID != Phase3GoalID || parent.ReservationReleased || parent.BookedSpentMicros <= 0 || parent.SignedWireSHA256 != auth.PolicySetupCompletion.Prefund.WireSHA256 {
			return action, budgetHold("unproven_setup_prefund_parent")
		}
	}
	if pointer != root || budget.Reservations[id] != expected || budget.Families["OnRe"].ExitMicros != expected.ExitAfterMicros {
		return action, budgetHold("setup_payment_reservation_mismatch")
	}
	return action, nil
}

// This is the cost/prestate gate before loading an admin signer. It neither
// loads one nor attests deployment/farm readiness, which the setup construction
// coordinator must establish before invoking the eventual signer path.
func (d *Database) authorizePolicySetupPayment(ctx context.Context, rpc *RPCClient, id string) (policySetupPayment, error) {
	var out policySetupPayment
	if d == nil || d.pool == nil || rpc == nil || id == "" {
		return out, budgetHold("invalid_setup_payment_authorization")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, id)
	if err != nil {
		return out, err
	}
	action, err := d.validatePolicySetupReservationTx(ctx, tx, id, budget, auth, Decided)
	if err != nil {
		return out, err
	}
	out, err = observePolicySetupPayment(ctx, rpc, auth, action)
	if err != nil {
		return out, err
	}
	if out.Cost.TotalMicros > budget.Reservations[id].UpperMicros {
		return out, budgetHold("fresh_setup_cost_exceeds_reservation")
	}
	if out.CompletionCost != nil && out.CompletionCost.TotalMicros > budget.Reservations[id].ExitAfterMicros {
		return out, budgetHold("fresh_setup_completion_exceeds_reservation")
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return out, err
	}
	minimum, expiry := out.Cost.ObservationSlot, out.Cost.ValidThroughSlot
	if out.CompletionCost != nil {
		minimum = max(minimum, out.CompletionCost.ObservationSlot)
		expiry = min(expiry, out.CompletionCost.ValidThroughSlot)
	}
	if slot < minimum || slot > expiry {
		return out, budgetHold("policy_setup_valuation_expired")
	}
	auth.SetupBuildCost, auth.SetupCompletionCost = &out.Cost, out.CompletionCost
	if err = d.writePhase3BudgetTx(ctx, tx, id, budget, auth); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
