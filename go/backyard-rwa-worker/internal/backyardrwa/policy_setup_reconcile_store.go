package backyardrwa

import (
	"context"
	"encoding/json"
	"reflect"
)

// The setup fence is removed only in the same lease-guarded transaction that
// persists final installed-state evidence and settles the creation reservation.
// This is reconciliation, not policy activation, retirement, signing or send.
func (d *Database) reconcilePolicySetupCreation(ctx context.Context, rpc *RPCClient, id string) error {
	if d == nil || d.pool == nil || rpc == nil || id == "" {
		return budgetHold("invalid_setup_creation_reconciliation")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, id)
	if err != nil {
		return err
	}
	r, cost, prefund, err := policySetupCreationInput(auth)
	if err != nil {
		return err
	}
	var op PersistedOperation
	op.ID = id
	var pointer string
	err = tx.QueryRow(ctx, `SELECT o.route_key,o.status,o.action,o.strategy_key,COALESCE(o.signed_wire,decode('','hex')),COALESCE(o.signed_wire_sha256,''),COALESCE(o.transaction_signature,''),COALESCE(o.recent_blockhash,''),COALESCE(o.last_valid_block_height,0),COALESCE(o.confirmed_slot,0),COALESCE(r.state->>'phase3SetupIntent','') FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states r USING(route_key) WHERE o.operation_id=$1`, id).Scan(&op.RouteKey, &op.Status, &op.Decision.Action, &op.Decision.StrategyKey, &op.SignedWire, &op.SignedWireSHA256, &op.TransactionSignature, &op.RecentBlockhash, &op.LastValidBlockHeight, &op.ConfirmedSlot, &pointer)
	if err != nil {
		return err
	}
	if op.Decision.Action != PolicySetupCreate || op.Decision.StrategyKey != "OnRe/ONyc/USDC" || auth.ReservationReleased || len(op.SignedWire) == 0 || auth.SignedWireSHA256 != op.SignedWireSHA256 || auth.SignedWireSHA256 != sha256Bytes(op.SignedWire) {
		return budgetHold("invalid_persisted_setup_creation")
	}
	root := id
	if auth.PolicySetupCompletion != nil {
		root = auth.PolicySetupCompletion.PrefundOperationID
	}
	if op.Status == Reconciled {
		var finalized bool
		var raw []byte
		var digest string
		if err := tx.QueryRow(ctx, `SELECT confirmation_status='finalized',reconciled_effects,reconciliation_sha256 FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&finalized, &raw, &digest); err != nil {
			return err
		}
		var receipt policySetupCreatedReceipt
		_, reserved := budget.Reservations[id]
		if !finalized || json.Unmarshal(raw, &receipt) != nil || digest != sha256Bytes(raw) || receipt.Signature != op.TransactionSignature || receipt.WireSHA256 != op.SignedWireSHA256 || receipt.Slot != op.ConfirmedSlot || auth.BookedSpentMicros <= 0 || reserved || pointer == root {
			return budgetHold("invalid_reconciled_setup_creation")
		}
		return tx.Commit(ctx)
	}
	if pointer != root || (op.Status != BroadcastIntent && op.Status != Submitted && op.Status != Confirmed && op.Status != Reconciling) {
		return budgetHold("setup_creation_not_submitted")
	}
	reservation, exists := budget.Reservations[id]
	if !exists || budget.AuthorizeIntent(id, auth.IntentSHA256) != nil || auth.BookedSpentMicros != 0 || auth.SendKnownCost == nil || reservation.Family != "OnRe" || reservation.ExitAfterMicros != 0 || reservation.Recovery != (prefund > 0) || budget.Families["OnRe"].ExitMicros != 0 {
		return budgetHold("unproven_setup_creation_send")
	}
	ix, _, err := policySetupCreateInstruction(r)
	if err != nil {
		return err
	}
	key, err := decodeKey(r.RecentBlockhash)
	if err != nil {
		return err
	}
	message, err := compileLegacyMessage(mustKey(bridgeSettingsSigner), key, []compiledInstruction{ix})
	if err != nil {
		return err
	}
	priced := auth.SendKnownCost
	checked, err := ValueTransactionCost(message, ExecutableDebit{}, priced.Fee, cost.SetupLamports, BudgetPrice{}, priced.NativePrice, priced.ObservationSlot)
	if err != nil || !reflect.DeepEqual(checked, *priced) || priced.TotalMicros > reservation.UpperMicros || priced.Fee.Lamports > cost.Fee.Lamports {
		return budgetHold("unproven_setup_creation_send")
	}
	if prefund > 0 {
		var parentAuth, receipt []byte
		var finalized bool
		if err := tx.QueryRow(ctx, `SELECT expected_effects->'phase3',reconciled_effects,status='reconciled' AND confirmation_status='finalized' FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND route_key=$2 AND action='POLICY_SETUP_PREFUND'`, root, op.RouteKey).Scan(&parentAuth, &receipt, &finalized); err != nil {
			return err
		}
		var parent phase3OperationAuthorization
		var proof policySetupPrefundReceipt
		if !finalized || json.Unmarshal(parentAuth, &parent) != nil || json.Unmarshal(receipt, &proof) != nil || parent.GoalID != Phase3GoalID || parent.BookedSpentMicros <= 0 || parent.ReservationReleased || parent.PolicySetup == nil || !reflect.DeepEqual(*parent.PolicySetup, *auth.PolicySetup) || !reflect.DeepEqual(proof, auth.PolicySetupCompletion.Prefund) {
			return budgetHold("unproven_setup_prefund_parent")
		}
	}
	receipt, err := observePolicySetupCreated(ctx, rpc, auth, op)
	if err != nil {
		return err
	}
	encoded, _ := json.Marshal(receipt)
	// Hash PostgreSQL's canonical JSONB text on retry, not the input formatting.
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled',confirmation_status='finalized',confirmed_slot=$2,reconciled_effects=$3::jsonb,updated_at=clock_timestamp() WHERE operation_id=$1 AND status=$4`, id, receipt.Slot, string(encoded), string(op.Status))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return budgetHold("setup_creation_lost_serialization")
	}
	var canonical []byte
	if err = tx.QueryRow(ctx, `SELECT reconciled_effects FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&canonical); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET reconciliation_sha256=$2 WHERE operation_id=$1`, id, sha256Bytes(canonical)); err != nil {
		return err
	}
	if err = d.settlePhase3ReservationTx(ctx, tx, id); err != nil {
		return err
	}
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	result, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=state-'phase3SetupIntent' WHERE route_key=$1 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at>clock_timestamp() AND state->>'phase3SetupIntent'=$4`, op.RouteKey, lease.Owner, lease.FencingToken, root)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrRouteLeaseLost
	}
	return tx.Commit(ctx)
}
