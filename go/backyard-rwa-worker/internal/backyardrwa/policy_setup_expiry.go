package backyardrwa

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Retire one expired wire, not its setup obligation or budget reservation. The
// complete signed attempt stays in the same journal row's append-only history.
// A funded creation remains funded; only a later separately priced unsigned
// refresh can change its request. This method never signs, sends or releases rent.
func (d *Database) recoverExpiredPolicySetup(ctx context.Context, rpc *RPCClient, id, expectedWire string) error {
	if d == nil || d.pool == nil || rpc == nil || id == "" || !sha256Pattern.MatchString(expectedWire) {
		return budgetHold("invalid_setup_expiry_recovery")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, id)
	if err != nil {
		return err
	}
	op, journal, lastExpired, err := loadPolicySetupExpiryTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if op.Status == Decided && auth.SignedWireSHA256 == "" && lastExpired == expectedWire {
		// A retry after commit/restart must not observe again, append twice or
		// overwrite a subsequently refreshed unsigned generation.
		if len(op.SignedWire) != 0 || op.SignedWireSHA256 != "" || op.TransactionSignature != "" || op.BroadcastIntentRecorded || op.ConfirmedSlot != 0 {
			return budgetHold("setup_expiry_wire_or_status_changed")
		}
		if _, err = d.validatePolicySetupReservationTx(ctx, tx, id, budget, auth, Decided); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if op.SignedWireSHA256 != expectedWire || (op.Status != Built && op.Status != Signed && op.Status != BroadcastIntent && op.Status != Submitted) {
		return budgetHold("setup_expiry_wire_or_status_changed")
	}
	if _, err = d.validatePolicySetupReservationTx(ctx, tx, id, budget, auth, op.Status); err != nil {
		return err
	}
	if err = validatePolicySetupSignedPayment(auth, op); err != nil {
		return err
	}
	if err = d.recoverVerifiedExpiredPolicySetupTx(ctx, tx, rpc, budget, auth, op, journal); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func loadPolicySetupExpiryTx(ctx context.Context, tx pgx.Tx, id string) (PersistedOperation, json.RawMessage, string, error) {
	var op PersistedOperation
	var journal json.RawMessage
	var last string
	var historyArray bool
	op.ID = id
	err := tx.QueryRow(ctx, `SELECT route_key,cycle,status,action,strategy_key,
	 COALESCE(signed_wire,'\x'::bytea),COALESCE(signed_wire_sha256,''),
	 COALESCE(transaction_signature,''),COALESCE(recent_blockhash,''),COALESCE(last_valid_block_height,0),
	 broadcast_intent_at IS NOT NULL,COALESCE(confirmed_slot,0),to_jsonb(o)-'expected_effects',
	 COALESCE(expected_effects->'setupExpiredWires'->-1->'operation'->>'SignedWireSHA256',''),
	 jsonb_typeof(COALESCE(expected_effects->'setupExpiredWires','[]'::jsonb))='array'
	 FROM loyal_yield.multiply_operations o WHERE operation_id=$1`, id).Scan(
		&op.RouteKey, &op.Cycle, &op.Status, &op.Decision.Action, &op.Decision.StrategyKey,
		&op.SignedWire, &op.SignedWireSHA256, &op.TransactionSignature, &op.RecentBlockhash,
		&op.LastValidBlockHeight, &op.BroadcastIntentRecorded, &op.ConfirmedSlot, &journal, &last, &historyArray)
	op.StrategyKey = op.Decision.StrategyKey
	if err == nil && !historyArray {
		err = budgetHold("invalid_setup_expiry_history")
	}
	return op, journal, last, err
}

// Called only after exact admin signature and locked reservation validation.
// Kept separate so controlled RPC/storage tests need no production secret.
func (d *Database) recoverVerifiedExpiredPolicySetupTx(ctx context.Context, tx pgx.Tx, rpc *RPCClient, budget Phase3Budget, auth phase3OperationAuthorization, op PersistedOperation, journal json.RawMessage) error {
	var genesis string
	if err := rpc.call(ctx, "getGenesisHash", []any{}, &genesis); err != nil || genesis != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return budgetHold("policy_setup_genesis_mismatch")
	}
	height, err := rpc.FinalizedBlockHeight(ctx)
	if err != nil {
		return err
	}
	if height <= op.LastValidBlockHeight {
		return budgetHold("setup_signed_wire_not_expired")
	}
	// Absence must be observed AFTER finalized expiry, with transaction-history
	// search enabled by SignatureStatus. Found/failed/malformed/ambiguous all HOLD.
	status, err := rpc.SignatureStatus(ctx, op.TransactionSignature)
	if err != nil {
		return err
	}
	if status.Found {
		return budgetHold("setup_expired_signature_found")
	}
	plan := auth.PolicySetup
	minimum := plan.FinalizedSettingsSlot
	if auth.PolicySetupCompletion != nil {
		minimum = auth.PolicySetupCompletion.FinalizedAccountSlot
	}
	slot, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx,
		[]string{bridgeSettings, bridgeSettingsSigner, plan.Policy}, minimum,
		map[string]struct{}{plan.Policy: {}}, "finalized")
	if err != nil {
		return err
	}
	if auth.PolicySetupCompletion != nil {
		err = validatePolicySetupFundedPrestate(*plan, accounts)
	} else {
		err = validatePolicySetupPrestate(plan.SettingsSHA256, plan.Request.Seed, accounts)
	}
	if err != nil {
		return err
	}
	history, err := json.Marshal(struct {
		Operation     PersistedOperation           `json:"operation"`
		Authorization phase3OperationAuthorization `json:"authorization"`
		Journal       json.RawMessage              `json:"journal"`
		Height        int64                        `json:"finalizedExpiryHeight"`
		Absent        bool                         `json:"signatureAbsentAfterExpiry"`
		Slot          int64                        `json:"finalizedPrestateSlot"`
		Accounts      []ConfirmedAccount           `json:"finalizedPrestate"`
	}{op, auth, journal, height, true, slot, accounts})
	if err != nil {
		return err
	}
	// Decided must clear the active wire/simulation columns in the same UPDATE.
	// The archived attempt retains all bytes, signature, authorization and proof.
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET
	 status='decided',message_sha256=NULL,signed_wire=NULL,signed_wire_sha256=NULL,
	 transaction_signature=NULL,recent_blockhash=NULL,last_valid_block_height=NULL,
	 simulation_slot=NULL,simulation_result=NULL,broadcast_intent_at=NULL,
	 recovery_reason='policy_setup_expired_absent_reobserve_required',
	 expected_effects=jsonb_set(expected_effects,'{setupExpiredWires}',
	 COALESCE(expected_effects->'setupExpiredWires','[]'::jsonb)||jsonb_build_array($4::jsonb),true),
	 updated_at=clock_timestamp()
	 WHERE operation_id=$1 AND status=$2 AND signed_wire_sha256=$3 AND confirmed_slot IS NULL`,
		op.ID, op.Status, op.SignedWireSHA256, string(history))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return budgetHold("setup_expiry_wire_or_status_changed")
	}
	auth.SignedWireSHA256, auth.SetupBuildCost, auth.SetupCompletionCost, auth.SendKnownCost = "", nil, nil, nil
	// No release/settlement/admission: paid spend and every reservation stay put.
	return d.writePhase3BudgetTx(ctx, tx, op.ID, budget, auth)
}
