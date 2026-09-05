package backyardrwa

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"

	"github.com/jackc/pgx/v5"
)

func isPolicySetupAction(action Action) bool {
	return action == PolicySetupPrefund || action == PolicySetupCreate
}

// Hold the existing per-transaction allowance for the one remaining payment,
// not just today's quote. A restart must reprice that payment within this
// reserve; unused headroom is never booked as spend. Call only after validation.
func policySetupCompletionReserve(plan policySetupObservation) int64 {
	if plan.Mode == "prefund-then-create" {
		return Phase3TransactionCapMicros
	}
	return 0
}

// Reconstruct every unsigned message and cost instead of trusting persisted
// totals or a self-asserted "priced" flag. Freshness is checked again under the
// route lock. This remains bookkeeping, not farm/deployment or signer proof.
func validatePolicySetupPlan(plan policySetupObservation) (string, error) {
	bad := budgetHold("invalid_policy_setup_plan")
	if plan.ProductionSetupAdmission || plan.FinalizedSettingsSlot <= 0 || plan.ObservationSlot < plan.FinalizedSettingsSlot || plan.ObservationSlot > plan.ValidThroughSlot || !sha256Pattern.MatchString(plan.SettingsSHA256) || plan.Request.Seed <= 139 || plan.Request.LastValidBlockHeight <= 0 {
		return "", bad
	}
	policy, err := policySetupAddress(plan.Request.Seed)
	if err != nil || plan.Policy != encodeBase58(policy[:]) {
		return "", bad
	}
	create, size, err := policySetupCreateInstruction(plan.Request)
	if err != nil || plan.AllocatedBytes != size || plan.RentLamports == 0 {
		return "", bad
	}
	blockhash, err := decodeKey(plan.Request.RecentBlockhash)
	if err != nil {
		return "", bad
	}
	var messages [][]byte
	var rent []uint64
	switch plan.Mode {
	case "direct-create":
		message, err := compileLegacyMessage(mustKey(bridgeSettingsSigner), blockhash, []compiledInstruction{create})
		if err != nil {
			return "", bad
		}
		messages, rent = [][]byte{message}, []uint64{plan.RentLamports}
	case "prefund-then-create":
		pair, err := compilePolicySetupMessages(plan.Request, plan.RentLamports/2)
		if err != nil {
			return "", bad
		}
		messages, rent = pair[:], []uint64{plan.RentLamports / 2, plan.RentLamports - plan.RentLamports/2}
	default:
		return "", bad
	}
	if len(plan.Payments) != len(messages) {
		return "", bad
	}
	var total int64
	var debit uint64
	validThrough := int64(math.MaxInt64)
	for i, payment := range plan.Payments {
		cost, err := ValueTransactionCost(messages[i], ExecutableDebit{}, payment.Fee, rent[i], BudgetPrice{}, payment.NativePrice, payment.ObservationSlot)
		if err != nil || !reflect.DeepEqual(payment, cost) || cost.TotalMicros <= 0 || cost.TotalMicros > Phase3TransactionCapMicros || payment.ObservationSlot > plan.ObservationSlot {
			return "", bad
		}
		total, err = budgetSum(total, cost.TotalMicros)
		if err != nil || rent[i] > math.MaxUint64-cost.Fee.Lamports || debit > math.MaxUint64-rent[i]-cost.Fee.Lamports {
			return "", bad
		}
		debit += rent[i] + cost.Fee.Lamports
		validThrough = min(validThrough, cost.ValidThroughSlot)
	}
	remaining := int64(0)
	if len(plan.Payments) == 2 {
		remaining = plan.Payments[1].TotalMicros
	}
	if total != plan.TotalMicros || remaining != plan.CompletionReserveMicros || validThrough != plan.ValidThroughSlot || debit > plan.PayerBalanceLamports {
		return "", bad
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", bad
	}
	return sha256Bytes(encoded), nil
}

// Persist one policy's complete setup intent and initial payment reservation
// atomically in the existing journal and route budget. The route-state value is
// only a pointer to that journal row, not a second setup ledger. No CLI, signer,
// broadcast or automatic worker invocation is exposed by this bookkeeping API.
func (d *Database) persistPolicySetupIntent(ctx context.Context, rpc *RPCClient, routeKey string, plan policySetupObservation) (DecisionRecord, error) {
	digest, err := validatePolicySetupPlan(plan)
	if err != nil {
		return DecisionRecord{}, err
	}
	lease, err := d.currentLease()
	if err != nil || lease.RouteKey != routeKey {
		return DecisionRecord{}, ErrRouteLeaseLost
	}
	if rpc == nil {
		return DecisionRecord{}, budgetHold("policy_setup_rpc_missing")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return DecisionRecord{}, err
	}
	defer tx.Rollback(ctx)
	var version int64
	var raw []byte
	if err = tx.QueryRow(ctx, RouteStateForUpdate, routeKey, lease.Owner, lease.FencingToken).Scan(&version, &raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DecisionRecord{}, ErrRouteLeaseLost
		}
		return DecisionRecord{}, err
	}
	var state map[string]json.RawMessage
	var generation int64
	var budget Phase3Budget
	if json.Unmarshal(raw, &state) != nil || json.Unmarshal(state["generation"], &generation) != nil || generation != version || version <= 0 || version == math.MaxInt64 || json.Unmarshal(state["phase3"], &budget) != nil || budget.validate() != nil {
		return DecisionRecord{}, budgetHold("invalid_setup_route_state")
	}
	idempotency := "phase3-setup:" + Phase3GoalID + ":" + routeKey + ":" + digest
	id := sha256Bytes([]byte(idempotency))
	if pointer, exists := state["phase3SetupIntent"]; exists {
		var existing string
		if json.Unmarshal(pointer, &existing) != nil || existing != id {
			return DecisionRecord{}, budgetHold("policy_setup_in_progress")
		}
		var saved []byte
		var record DecisionRecord
		record.OperationID = id
		if err = tx.QueryRow(ctx, `SELECT cycle,status,expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND route_key=$2`, id, routeKey).Scan(&record.Cycle, &record.Status, &saved); err != nil {
			return DecisionRecord{}, err
		}
		var auth phase3OperationAuthorization
		if json.Unmarshal(saved, &auth) != nil || !IsNonterminal(record.Status) || auth.PolicySetup == nil || auth.GoalID != Phase3GoalID || auth.IntentSHA256 != digest || auth.ReservationReleased || auth.BookedSpentMicros != 0 {
			return DecisionRecord{}, budgetHold("invalid_persisted_setup_intent")
		}
		savedDigest, err := validatePolicySetupPlan(*auth.PolicySetup)
		if err != nil || savedDigest != digest || budget.AuthorizeIntent(id, digest) != nil {
			return DecisionRecord{}, budgetHold("invalid_persisted_setup_intent")
		}
		expected := BudgetReservation{OperationID: id, Family: "OnRe", IntentSHA256: digest, UpperMicros: plan.Payments[0].TotalMicros, ExitAfterMicros: policySetupCompletionReserve(plan)}
		if budget.Reservations[id] != expected || budget.Families["OnRe"].ExitMicros != expected.ExitAfterMicros {
			return DecisionRecord{}, budgetHold("invalid_persisted_setup_intent")
		}
		// An exact retry reads the original intent without refreshing expiry,
		// replacing its request, writing another row or replenishing headroom.
		return record, tx.Commit(ctx)
	}
	if budget.Closed {
		return DecisionRecord{}, budgetHold("goal_envelope_expired")
	}
	if len(budget.Reservations) != 0 {
		return DecisionRecord{}, budgetHold("unresolved_submission_reservation")
	}
	for _, family := range budget.Families {
		if family.ExitMicros != 0 {
			return DecisionRecord{}, budgetHold("setup_conflicts_with_reserved_exit")
		}
	}
	var active, recovery bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN ('prepared','signed_persisted','broadcast_intent','confirmed','reconciliation_pending','decided','built','simulated','signed','submitted','reconciling'))`, routeKey).Scan(&active); err != nil {
		return DecisionRecord{}, err
	}
	if err = tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, routeKey).Scan(&recovery); err != nil {
		return DecisionRecord{}, err
	}
	if active || recovery {
		return DecisionRecord{}, budgetHold("unresolved_work_prevents_setup")
	}
	slot, accounts, err := rpc.getMultipleAccounts(ctx, []string{bridgeSettings, bridgeSettingsSigner, plan.Policy}, plan.ObservationSlot, map[string]struct{}{plan.Policy: {}})
	if err != nil {
		return DecisionRecord{}, err
	}
	if err = validatePolicySetupPrestate(plan.SettingsSHA256, plan.Request.Seed, accounts); err != nil {
		return DecisionRecord{}, err
	}
	if slot > plan.ValidThroughSlot {
		return DecisionRecord{}, budgetHold("policy_setup_valuation_expired")
	}
	var debit uint64
	for _, payment := range plan.Payments {
		debit += payment.SetupLamports + payment.Fee.Lamports
	} // bounded by validated plan
	if accounts[1].Lamports < debit {
		return DecisionRecord{}, budgetHold("policy_setup_payer_underfunded")
	}
	var cycle int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE((state->>'cycle')::bigint,1) FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&cycle); err != nil {
		return DecisionRecord{}, err
	}
	if cycle <= 0 {
		return DecisionRecord{}, budgetHold("invalid_setup_route_state")
	}
	action := PolicySetupCreate
	if plan.Mode == "prefund-then-create" {
		action = PolicySetupPrefund
	}
	if plan.Payments[0].SetupLamports > math.MaxInt64 {
		return DecisionRecord{}, budgetHold("invalid_policy_setup_plan")
	}
	decision := decisionEvidence{AmountRaw: int64(plan.Payments[0].SetupLamports), Reason: "phase3_policy_setup", ObservationID: plan.SettingsSHA256, ObservationSlot: plan.ObservationSlot, StrategyKey: "OnRe/ONyc/USDC"}
	expected, _ := json.Marshal(map[string]any{"schema": "loyal-backyard-rwa-operation-evidence/v1", "decision": decision, "expectedEffects": nil})
	reservation := BudgetReservation{OperationID: id, Family: "OnRe", IntentSHA256: digest, UpperMicros: plan.Payments[0].TotalMicros, ExitAfterMicros: policySetupCompletionReserve(plan)}
	if err = budget.Admit(reservation); err != nil {
		return DecisionRecord{}, err
	}
	if _, err = tx.Exec(ctx, OperationInsert, id, routeKey, cycle, string(action), string(Decided), idempotency, "OnRe/ONyc/USDC", string(expected), nil); err != nil {
		return DecisionRecord{}, err
	}
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, PolicySetup: &plan}
	if err = d.writePhase3BudgetTx(ctx, tx, id, budget, auth); err != nil {
		return DecisionRecord{}, err
	}
	pointer, _ := json.Marshal(id)
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3SetupIntent}',$4::jsonb,true) WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`, routeKey, lease.Owner, lease.FencingToken, string(pointer))
	if err != nil {
		return DecisionRecord{}, err
	}
	if result.RowsAffected() != 1 {
		return DecisionRecord{}, ErrRouteLeaseLost
	}
	return DecisionRecord{OperationID: id, Cycle: cycle, Status: Decided}, tx.Commit(ctx)
}

// Only the initial, proven-never-signed intent can be canceled here. A signed,
// submitted, settled or partially completed setup must retain its pointer and
// reserve for explicit setup reconciliation; it cannot become a fresh budget.
func (d *Database) cancelUnsentPolicySetupIntent(ctx context.Context, operationID string) error {
	if d == nil || d.pool == nil || operationID == "" {
		return budgetHold("invalid_policy_setup_cancel")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, operationID)
	if err != nil {
		return err
	}
	var status OperationStatus
	var action Action
	var pointer string
	var neverSigned bool
	if err = tx.QueryRow(ctx, `SELECT operation.status,operation.action,COALESCE(route.state->>'phase3SetupIntent',''),operation.signed_wire IS NULL AND operation.transaction_signature IS NULL AND operation.broadcast_intent_at IS NULL
	 FROM loyal_yield.multiply_operations operation JOIN loyal_yield.multiply_route_states route USING(route_key) WHERE operation_id=$1`, operationID).Scan(&status, &action, &pointer, &neverSigned); err != nil {
		return err
	}
	if pointer != operationID || !isPolicySetupAction(action) || status != Decided || !neverSigned || auth.PolicySetup == nil || auth.GoalID != Phase3GoalID || auth.SignedWireSHA256 != "" || auth.BookedSpentMicros != 0 || auth.ReservationReleased {
		return budgetHold("policy_setup_not_proven_unsent")
	}
	digest, err := validatePolicySetupPlan(*auth.PolicySetup)
	if err != nil || digest != auth.IntentSHA256 {
		return budgetHold("invalid_persisted_setup_intent")
	}
	if err = budget.releaseUnspent(operationID, auth.IntentSHA256); err != nil {
		return err
	}
	auth.ReservationReleased = true
	if err = d.writePhase3BudgetTx(ctx, tx, operationID, budget, auth); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed',recovery_reason='policy_setup_canceled_before_signing',updated_at=clock_timestamp() WHERE operation_id=$1 AND status='decided'`, operationID); err != nil {
		return err
	}
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=state-'phase3SetupIntent' WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp()`, lease.RouteKey, lease.Owner, lease.FencingToken)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrRouteLeaseLost
	}
	return tx.Commit(ctx)
}
