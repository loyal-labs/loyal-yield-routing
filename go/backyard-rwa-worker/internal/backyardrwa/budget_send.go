package backyardrwa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Byte strings retain the exact canonical request/effects encoding through
// JSONB storage; JSON objects would lose their original key ordering and digest.
type phase3BuildInput struct {
	Kind    string `json:"kind"`
	Request []byte `json:"request"`
	Effects []byte `json:"effects"`
}

func encodePhase3BuildInput(request any, effects []byte) (*phase3BuildInput, error) {
	input := &phase3BuildInput{Effects: append([]byte(nil), effects...)}
	switch request.(type) {
	case BridgeBuildRequest:
		input.Kind = "bridge"
	case KaminoPrimeUSDCRequest:
		input.Kind = "kamino"
	case JupiterSwapRequest:
		input.Kind = "jupiter"
	default:
		return nil, budgetHold("unmapped_economic_action")
	}
	var err error
	input.Request, err = json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return input, nil
}

func (input *phase3BuildInput) decode() (any, ExpectedEffects, []byte, error) {
	if input == nil {
		return nil, ExpectedEffects{}, nil, budgetHold("missing_persisted_build_input")
	}
	if !json.Valid(input.Request) {
		return nil, ExpectedEffects{}, nil, budgetHold("invalid_persisted_build_input")
	}
	var target any
	switch input.Kind {
	case "bridge":
		target = &BridgeBuildRequest{}
	case "kamino":
		target = &KaminoPrimeUSDCRequest{}
	case "jupiter":
		target = &JupiterSwapRequest{}
	default:
		return nil, ExpectedEffects{}, nil, budgetHold("invalid_persisted_build_input")
	}
	decoder := json.NewDecoder(bytes.NewReader(input.Request))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return nil, ExpectedEffects{}, nil, budgetHold("invalid_persisted_build_input")
	}
	var request any
	var message []byte
	var err error
	switch r := target.(type) {
	case *BridgeBuildRequest:
		request = *r
		message, err = CompileBridgeMessage(*r)
	case *KaminoPrimeUSDCRequest:
		request = *r
		message, err = CompileKaminoMessage(*r)
	case *JupiterSwapRequest:
		request = *r
		message, err = CompileJupiterMessage(*r)
	}
	if err != nil {
		return nil, ExpectedEffects{}, nil, budgetHold("persisted_build_no_longer_compiles")
	}
	effects, err := DecodeExpectedEffects(input.Effects)
	if err != nil {
		return nil, ExpectedEffects{}, nil, budgetHold("invalid_persisted_build_effects")
	}
	return request, effects, message, nil
}

// Only this marker permits the signed-HOLD expiry path. Invalid persisted
// identities must never release funds using untrusted expiry metadata.
type validatedSignedBudgetHold struct{ hold *BudgetHold }

func (e *validatedSignedBudgetHold) Error() string { return e.hold.Error() }
func (e *validatedSignedBudgetHold) Unwrap() error { return e.hold }

func revaluePhase3SignedInput(ctx context.Context, rpc *RPCClient, auth phase3OperationAuthorization, operation PersistedOperation) (ValuedTransactionCost, error) {
	if auth.PolicySetup != nil {
		if err := validatePolicySetupSignedPayment(auth, operation); err != nil {
			return ValuedTransactionCost{}, err
		}
		payment, err := observePolicySetupPayment(ctx, rpc, auth, operation.Decision.Action)
		if err != nil {
			return ValuedTransactionCost{}, err
		}
		cost := payment.Cost
		if payment.CompletionCost != nil {
			cost.ValidThroughSlot = min(cost.ValidThroughSlot, payment.CompletionCost.ValidThroughSlot)
		}
		return cost, nil
	}
	wire := operation.SignedWire
	if auth.GoalID != Phase3GoalID || len(wire) <= 65 || wire[0] != 1 || auth.SignedWireSHA256 != sha256Bytes(wire) {
		return ValuedTransactionCost{}, budgetHold("signed_wire_reservation_mismatch")
	}
	request, effects, message, err := auth.BuildInput.decode()
	if err != nil {
		return ValuedTransactionCost{}, err
	}
	digest, err := Phase3IntentDigest(request, auth.BuildInput.Effects)
	if err != nil || digest != auth.IntentSHA256 || !bytes.Equal(message, wire[65:]) {
		return ValuedTransactionCost{}, budgetHold("persisted_build_intent_mismatch")
	}
	var blockhash string
	var height int64
	switch r := request.(type) {
	case BridgeBuildRequest:
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case KaminoPrimeUSDCRequest:
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case JupiterSwapRequest:
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	}
	if operation.SignedWireSHA256 != auth.SignedWireSHA256 || operation.RecentBlockhash != blockhash || operation.LastValidBlockHeight != height || operation.TransactionSignature != encodeBase58(wire[1:65]) {
		return ValuedTransactionCost{}, budgetHold("persisted_signature_or_expiry_mismatch")
	}
	cost, err := observePhase3KnownBuildCost(ctx, rpc, request, effects)
	if err == nil && auth.BridgeAdmission != nil && auth.BridgeAdmission.LeverageProjection != nil {
		entry, ok := request.(JupiterSwapRequest)
		if !ok {
			err = budgetHold("leverage_projection_identity_mismatch")
		} else {
			var observed int64
			observed, err = validateLeverageAdmissionPrestate(ctx, rpc, entry, effects, auth.BridgeAdmission, cost.ObservationSlot)
			cost.ObservationSlot = max(cost.ObservationSlot, observed)
		}
	}
	if err == nil && auth.BridgeAdmission != nil && auth.BridgeAdmission.BorrowProjection != nil {
		entry, ok := request.(KaminoPrimeUSDCRequest)
		if !ok || effects.Kind != "kamino-borrow" {
			err = budgetHold("borrow_projection_identity_mismatch")
		} else {
			var observed int64
			observed, err = validateBorrowAdmissionPrestate(ctx, rpc, entry, auth.BridgeAdmission, cost.ObservationSlot)
			cost.ObservationSlot = max(cost.ObservationSlot, observed)
		}
	}
	if err == nil && auth.BridgeAdmission != nil && auth.BridgeAdmission.DepositProjection != nil {
		entry, ok := request.(KaminoPrimeUSDCRequest)
		_, leg, entryErr := kaminoPrimeUSDCInstruction(entry)
		if !ok || entryErr != nil || entry.Action != OpenRouteStep || leg != kaminoLegDeposit || effects.Deposit == nil {
			err = budgetHold("deposit_projection_identity_mismatch")
		} else {
			var route RuntimeRoute
			route, err = runtimeRoute(entry.RouteLane)
			if err == nil {
				var observed int64
				if auth.BridgeAdmission.Snapshot.PositionDebtRaw > 0 {
					observed, err = validateRedepositAdmissionPrestate(ctx, rpc, entry, auth.BridgeAdmission, cost.ObservationSlot)
				} else {
					observed, err = validateInitialDepositPrestate(ctx, rpc, route, cost.ObservationSlot)
				}
				cost.ObservationSlot = max(cost.ObservationSlot, observed)
			}
		}
	}
	if err == nil && auth.BridgeAdmission != nil {
		// Fresh principal pricing cannot extend the earlier complete exit
		// estimate. The locked send fence checks this reduced window again.
		cost.ValidThroughSlot = min(cost.ValidThroughSlot, auth.BridgeAdmission.ValidThroughSlot)
		if cost.ObservationSlot < auth.BridgeAdmission.CurrentCost.ObservationSlot || cost.ObservationSlot > cost.ValidThroughSlot {
			err = budgetHold("send_valuation_expired")
		}
	}
	var hold *BudgetHold
	if errors.As(err, &hold) {
		return cost, &validatedSignedBudgetHold{hold}
	}
	return cost, err
}

// No signer and no wire replacement: price the exact persisted message, then
// atomically recheck its reservation/lease and record broadcast intent. The
// coordinator still submits only its already persisted bytes, once.
func (d *Database) RevalueAndMarkBroadcastIntent(ctx context.Context, rpc *RPCClient, operation PersistedOperation) error {
	if d == nil || d.pool == nil || rpc == nil || operation.ID == "" || operation.Status != Signed {
		return fmt.Errorf("invalid final-send valuation input")
	}
	var encoded []byte
	if err := d.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND status='signed'`, operation.ID).Scan(&encoded); err != nil {
		return err
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(encoded, &auth) != nil {
		return budgetHold("invalid_durable_budget")
	}
	cost, err := revaluePhase3SignedInput(ctx, rpc, auth, operation)
	if err != nil {
		return err
	}
	err = d.markBroadcastIntent(ctx, operation.ID, rpc, auth.IntentSHA256, sha256Bytes(operation.SignedWire), cost)
	var hold *BudgetHold
	if errors.As(err, &hold) && (hold.Reason == "fresh_send_cost_exceeds_reservation" || hold.Reason == "send_valuation_expired" || hold.Reason == "send_valuation_slot_unavailable") {
		return &validatedSignedBudgetHold{hold}
	}
	return err
}

// Retain the reason while leaving signed bytes and reservations untouched.
// Merely not recording broadcast intent does not prove a signature absent.
func (d *Database) RecordPhase3SignedBudgetHold(ctx context.Context, operationID string, hold *BudgetHold) error {
	if d == nil || d.pool == nil || operationID == "" || hold == nil || hold.Reason == "" {
		return fmt.Errorf("invalid signed budget hold")
	}
	encoded, err := json.Marshal(hold)
	if err != nil {
		return err
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = d.lockOperationLease(ctx, tx, operationID); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET recovery_reason=$2,expected_effects=jsonb_set(expected_effects,'{budgetHold}',$3::jsonb),updated_at=clock_timestamp() WHERE operation_id=$1 AND status='signed'`, operationID, "phase3_budget_hold:"+hold.Reason, string(encoded))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return budgetHold("signed_budget_hold_state_changed")
	}
	return tx.Commit(ctx)
}
