package backyardrwa

import (
	"context"
	"encoding/json"
	"reflect"
)

// Refresh the unpaid creation only. The deterministic child operation and
// finalized parent stay in place; the original prefund is never released,
// repeated or rebooked. A stale caller can observe its immediate replacement
// but cannot overwrite a later generation. This method cannot sign or send.
func (d *Database) refreshPolicySetupCompletion(ctx context.Context, rpc *RPCClient, id, expectedIntent string) (DecisionRecord, error) {
	var result DecisionRecord
	if d == nil || d.pool == nil || rpc == nil || id == "" || !sha256Pattern.MatchString(expectedIntent) {
		return result, budgetHold("invalid_setup_completion_refresh")
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
	if auth.PolicySetup == nil || auth.PolicySetupCompletion == nil || auth.SignedWireSHA256 != "" || auth.SendKnownCost != nil {
		return result, budgetHold("setup_completion_not_proven_unsigned")
	}
	action, err := d.validatePolicySetupReservationTx(ctx, tx, id, budget, auth, Decided)
	if err != nil {
		return result, err
	}
	if action != PolicySetupCreate {
		return result, budgetHold("setup_completion_not_proven_unsigned")
	}
	var unsigned bool
	var route, refreshedFrom string
	if err = tx.QueryRow(ctx, `SELECT route_key,cycle,signed_wire IS NULL AND signed_wire_sha256 IS NULL AND transaction_signature IS NULL AND broadcast_intent_at IS NULL,COALESCE(expected_effects->'setupRefresh'->>'fromIntentSha256','') FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&route, &result.Cycle, &unsigned, &refreshedFrom); err != nil {
		return result, err
	}
	if !unsigned {
		return result, budgetHold("setup_completion_not_proven_unsigned")
	}
	result.OperationID, result.Status = id, Decided
	if auth.IntentSHA256 != expectedIntent {
		if refreshedFrom != expectedIntent {
			return result, budgetHold("setup_completion_refresh_superseded")
		}
		return result, tx.Commit(ctx)
	}
	var parent PersistedOperation
	var parentRaw []byte
	parent.ID = auth.PolicySetupCompletion.PrefundOperationID
	err = tx.QueryRow(ctx, `SELECT status,action,strategy_key,COALESCE(signed_wire,'\x'::bytea),COALESCE(signed_wire_sha256,''),COALESCE(transaction_signature,''),COALESCE(recent_blockhash,''),COALESCE(last_valid_block_height,0),COALESCE(confirmed_slot,0),expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND route_key=$2`, parent.ID, route).Scan(&parent.Status, &parent.Decision.Action, &parent.Decision.StrategyKey, &parent.SignedWire, &parent.SignedWireSHA256, &parent.TransactionSignature, &parent.RecentBlockhash, &parent.LastValidBlockHeight, &parent.ConfirmedSlot, &parentRaw)
	if err != nil {
		return result, err
	}
	var parentAuth phase3OperationAuthorization
	if parent.Status != Reconciled || json.Unmarshal(parentRaw, &parentAuth) != nil || !reflect.DeepEqual(parentAuth.PolicySetup, auth.PolicySetup) || parentAuth.BookedSpentMicros <= 0 {
		return result, budgetHold("unproven_setup_prefund_parent")
	}
	completion, err := observePolicySetupCompletion(ctx, rpc, *auth.PolicySetup, parent)
	if err != nil {
		return result, err
	}
	old := *auth.PolicySetupCompletion
	if !reflect.DeepEqual(completion.Prefund, old.Prefund) || completion.FinalizedAccountSlot < old.FinalizedAccountSlot || completion.ObservationSlot < old.ObservationSlot {
		return result, budgetHold("setup_completion_prefund_or_observation_changed")
	}
	digest, err := validatePolicySetupCompletion(*auth.PolicySetup, completion)
	if err != nil {
		return result, err
	}
	if digest == auth.IntentSHA256 {
		return result, tx.Commit(ctx)
	}
	// Restore and consume only the existing one-payment recovery allowance in
	// this transaction. releaseUnspent does not alter any family's spent total.
	if err = budget.releaseUnspent(id, auth.IntentSHA256); err != nil {
		return result, err
	}
	if err = budget.Admit(BudgetReservation{OperationID: id, Family: "OnRe", IntentSHA256: digest, UpperMicros: completion.Cost.TotalMicros, Recovery: true}); err != nil {
		return result, err
	}
	decision, _ := json.Marshal(decisionEvidence{AmountRaw: int64(completion.Cost.SetupLamports), Reason: "phase3_policy_setup", ObservationID: auth.PolicySetup.SettingsSHA256, ObservationSlot: completion.ObservationSlot, StrategyKey: "OnRe/ONyc/USDC"})
	history, _ := json.Marshal(struct {
		FromIntent string                `json:"fromIntentSha256"`
		Previous   policySetupCompletion `json:"previousCompletion"`
	}{auth.IntentSHA256, old})
	updated, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(jsonb_set(expected_effects,'{decision}',$2::jsonb),'{setupRefresh}',$3::jsonb,true),updated_at=clock_timestamp() WHERE operation_id=$1 AND status='decided' AND signed_wire IS NULL AND signed_wire_sha256 IS NULL AND transaction_signature IS NULL AND broadcast_intent_at IS NULL`, id, string(decision), string(history))
	if err != nil {
		return result, err
	}
	if updated.RowsAffected() != 1 {
		return result, budgetHold("setup_completion_not_proven_unsigned")
	}
	// No pre-sign/send proof survives a message or fee/rent change.
	auth = phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, PolicySetup: auth.PolicySetup, PolicySetupCompletion: &completion}
	if err = d.writePhase3BudgetTx(ctx, tx, id, budget, auth); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
