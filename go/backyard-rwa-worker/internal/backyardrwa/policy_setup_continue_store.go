package backyardrwa

import (
	"context"
	"encoding/json"
	"reflect"
)

// Finalize the exact prefund and reserve its creation continuation in one
// transaction under the existing route lease. A failure at any point leaves
// the original journal/reservation intact. This method never signs or sends.
func (d *Database) continuePolicySetupPrefund(ctx context.Context, rpc *RPCClient, id string) (DecisionRecord, error) {
	var result DecisionRecord
	if d == nil || d.pool == nil || rpc == nil || id == "" {
		return result, budgetHold("invalid_policy_setup_continuation")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, id)
	if err != nil {
		return result, err
	}
	var op PersistedOperation
	var pointer string
	var cycle int64
	op.ID = id
	err = tx.QueryRow(ctx, `SELECT o.route_key,o.cycle,o.status,o.action,o.strategy_key,COALESCE(o.signed_wire,'\x'::bytea),COALESCE(o.signed_wire_sha256,''),COALESCE(o.transaction_signature,''),COALESCE(o.recent_blockhash,''),COALESCE(o.last_valid_block_height,0),COALESCE(o.confirmed_slot,0),COALESCE(r.state->>'phase3SetupIntent','') FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states r USING(route_key) WHERE o.operation_id=$1`, id).Scan(&op.RouteKey, &cycle, &op.Status, &op.Decision.Action, &op.Decision.StrategyKey, &op.SignedWire, &op.SignedWireSHA256, &op.TransactionSignature, &op.RecentBlockhash, &op.LastValidBlockHeight, &op.ConfirmedSlot, &pointer)
	if err != nil {
		return result, err
	}
	if pointer != id || op.Decision.Action != PolicySetupPrefund || op.Decision.StrategyKey != "OnRe/ONyc/USDC" || auth.GoalID != Phase3GoalID || auth.PolicySetup == nil || auth.PolicySetupCompletion != nil || auth.ReservationReleased {
		return result, budgetHold("invalid_persisted_setup_intent")
	}
	digest, err := validatePolicySetupPlan(*auth.PolicySetup)
	if err != nil || digest != auth.IntentSHA256 || auth.SignedWireSHA256 != op.SignedWireSHA256 || auth.SignedWireSHA256 != sha256Bytes(op.SignedWire) {
		return result, budgetHold("invalid_persisted_setup_intent")
	}
	idempotency := "phase3-setup-create:" + id
	childID := sha256Bytes([]byte(idempotency))
	if op.Status == Reconciled {
		var saved []byte
		result.OperationID = childID
		if err = tx.QueryRow(ctx, `SELECT cycle,status,expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND route_key=$2 AND action='POLICY_SETUP_CREATE' AND strategy_key='OnRe/ONyc/USDC'`, childID, op.RouteKey).Scan(&result.Cycle, &result.Status, &saved); err != nil {
			return result, err
		}
		var child phase3OperationAuthorization
		if auth.BookedSpentMicros <= 0 || !IsNonterminal(result.Status) || json.Unmarshal(saved, &child) != nil || child.PolicySetupCompletion == nil || child.PolicySetupCompletion.PrefundOperationID != id || child.GoalID != Phase3GoalID || budget.AuthorizeIntent(childID, child.IntentSHA256) != nil {
			return result, budgetHold("invalid_persisted_setup_continuation")
		}
		checked, err := validatePolicySetupCompletion(*auth.PolicySetup, *child.PolicySetupCompletion)
		if err != nil || checked != child.IntentSHA256 || child.PolicySetup == nil || !reflect.DeepEqual(*child.PolicySetup, *auth.PolicySetup) || child.ReservationReleased || child.BookedSpentMicros != 0 || budget.Reservations[childID] != (BudgetReservation{OperationID: childID, Family: "OnRe", IntentSHA256: checked, UpperMicros: child.PolicySetupCompletion.Cost.TotalMicros, ExitBeforeMicros: Phase3TransactionCapMicros, Recovery: true}) || budget.Families["OnRe"].ExitMicros != 0 {
			return result, budgetHold("invalid_persisted_setup_continuation")
		}
		return result, tx.Commit(ctx)
	}
	if op.Status != BroadcastIntent && op.Status != Submitted && op.Status != Confirmed && op.Status != Reconciling {
		return result, budgetHold("policy_setup_prefund_not_submitted")
	}
	reservation, exists := budget.Reservations[id]
	if !exists || budget.AuthorizeIntent(id, digest) != nil || auth.BookedSpentMicros != 0 || reservation.Family != "OnRe" || reservation.ExitAfterMicros != Phase3TransactionCapMicros || budget.Families["OnRe"].ExitMicros != Phase3TransactionCapMicros || auth.SendKnownCost == nil {
		return result, budgetHold("unproven_setup_prefund_send")
	}
	// Require the persisted final-send valuation, not merely an unsigned plan.
	priced := auth.SendKnownCost
	pair, err := compilePolicySetupMessages(auth.PolicySetup.Request, auth.PolicySetup.RentLamports/2)
	if err != nil {
		return result, err
	}
	checked, err := ValueTransactionCost(pair[0], ExecutableDebit{}, priced.Fee, auth.PolicySetup.Payments[0].SetupLamports, BudgetPrice{}, priced.NativePrice, priced.ObservationSlot)
	if err != nil || !reflect.DeepEqual(checked, *priced) || priced.TotalMicros > reservation.UpperMicros || priced.Fee.Lamports > auth.PolicySetup.Payments[0].Fee.Lamports {
		return result, budgetHold("unproven_setup_prefund_send")
	}
	completion, err := observePolicySetupCompletion(ctx, rpc, *auth.PolicySetup, op)
	if err != nil {
		return result, err
	}
	childDigest, err := validatePolicySetupCompletion(*auth.PolicySetup, completion)
	if err != nil {
		return result, err
	}
	receiptJSON, _ := json.Marshal(completion.Prefund)
	updated, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled',confirmation_status='finalized',confirmed_slot=$2,reconciliation_sha256=$3,reconciled_effects=$4::jsonb,updated_at=clock_timestamp() WHERE operation_id=$1 AND status=$5`, id, completion.Prefund.Slot, sha256Bytes(receiptJSON), string(receiptJSON), string(op.Status))
	if err != nil {
		return result, err
	}
	if updated.RowsAffected() != 1 {
		return result, budgetHold("policy_setup_reconciliation_lost_serialization")
	}
	if err = d.settlePhase3ReservationTx(ctx, tx, id); err != nil {
		return result, err
	}
	budget, _, err = d.readPhase3BudgetTx(ctx, tx, id)
	if err != nil {
		return result, err
	}
	if err = budget.Admit(BudgetReservation{OperationID: childID, Family: "OnRe", IntentSHA256: childDigest, UpperMicros: completion.Cost.TotalMicros, Recovery: true}); err != nil {
		return result, err
	}
	decision := decisionEvidence{AmountRaw: int64(completion.Cost.SetupLamports), Reason: "phase3_policy_setup", ObservationID: auth.PolicySetup.SettingsSHA256, ObservationSlot: completion.ObservationSlot, StrategyKey: "OnRe/ONyc/USDC"}
	expected, _ := json.Marshal(map[string]any{"schema": "loyal-backyard-rwa-operation-evidence/v1", "decision": decision, "expectedEffects": nil})
	if _, err = tx.Exec(ctx, OperationInsert, childID, op.RouteKey, cycle, string(PolicySetupCreate), string(Decided), idempotency, "OnRe/ONyc/USDC", string(expected), nil); err != nil {
		return result, err
	}
	child := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: childDigest, PolicySetup: auth.PolicySetup, PolicySetupCompletion: &completion}
	if err = d.writePhase3BudgetTx(ctx, tx, childID, budget, child); err != nil {
		return result, err
	}
	return DecisionRecord{OperationID: childID, Cycle: cycle, Status: Decided}, tx.Commit(ctx)
}
