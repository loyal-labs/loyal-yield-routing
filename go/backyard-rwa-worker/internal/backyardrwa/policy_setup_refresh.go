package backyardrwa

import (
	"context"
	"encoding/json"
)

// Replace only an initial, proven-never-signed setup intent. The fresh plan is
// obtained with observePolicySetup, then rechecked under the existing lease.
// The failed old row and its replacement link remain in the same journal.
// No signer, submission, funded continuation or runtime binding is enabled.
func (d *Database) refreshUnsentPolicySetupIntent(ctx context.Context, rpc *RPCClient, id string, plan policySetupObservation) (DecisionRecord, error) {
	var empty DecisionRecord
	if d == nil || d.pool == nil || rpc == nil || id == "" {
		return empty, budgetHold("invalid_policy_setup_refresh")
	}
	digest, err := validatePolicySetupPlan(plan)
	if err != nil {
		return empty, err
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, id)
	if err != nil {
		return empty, err
	}
	var route, pointer, replacement string
	if err = tx.QueryRow(ctx, `SELECT o.route_key,COALESCE(r.state->>'phase3SetupIntent',''),COALESCE(o.expected_effects->>'policySetupReplacementOperationId','') FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states r USING(route_key) WHERE o.operation_id=$1`, id).Scan(&route, &pointer, &replacement); err != nil {
		return empty, err
	}
	old := auth.PolicySetup
	if auth.GoalID != Phase3GoalID || old == nil || auth.PolicySetupCompletion != nil ||
		plan.Request.Operation != old.Request.Operation || plan.Request.Seed != old.Request.Seed ||
		plan.Policy != old.Policy || plan.SettingsSHA256 != old.SettingsSHA256 ||
		plan.FinalizedSettingsSlot < old.FinalizedSettingsSlot || plan.ObservationSlot < old.ObservationSlot {
		return empty, budgetHold("policy_setup_refresh_identity_changed")
	}
	oldDigest, err := validatePolicySetupPlan(*old)
	if err != nil || oldDigest != auth.IntentSHA256 {
		return empty, budgetHold("invalid_persisted_setup_intent")
	}
	if replacement != "" {
		// Retry cannot create another generation or resurrect completed setup.
		expectedID := sha256Bytes([]byte("phase3-setup:" + Phase3GoalID + ":" + route + ":" + digest))
		if !auth.ReservationReleased || pointer != replacement || replacement != expectedID {
			return empty, budgetHold("policy_setup_refresh_superseded")
		}
		record, err := d.persistPolicySetupIntentTx(ctx, tx, rpc, route, plan)
		if err != nil {
			return empty, err
		}
		return record, tx.Commit(ctx)
	}
	if pointer != id {
		return empty, budgetHold("policy_setup_not_proven_unsent")
	}
	if digest == oldDigest {
		return empty, budgetHold("policy_setup_refresh_unchanged")
	}
	if _, err = d.validatePolicySetupReservationTx(ctx, tx, id, budget, auth, Decided); err != nil {
		return empty, err
	}
	if err = d.cancelUnsentPolicySetupIntentTx(ctx, tx, id); err != nil {
		return empty, err
	}
	record, err := d.persistPolicySetupIntentTx(ctx, tx, rpc, route, plan)
	if err != nil {
		return empty, err
	}
	link, _ := json.Marshal(record.OperationID)
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET recovery_reason='policy_setup_unsigned_refresh',expected_effects=jsonb_set(expected_effects,'{policySetupReplacementOperationId}',$2::jsonb,true) WHERE operation_id=$1 AND status='failed'`, id, string(link)); err != nil {
		return empty, err
	}
	return record, tx.Commit(ctx)
}
