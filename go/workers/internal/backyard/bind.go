package backyard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// phase3OperationAuthorization is expected_effects.phase3: the integrity
// record of one operation. Bind writes the intent, its exact build input, the
// shared-custody proof and the debt-clear authority; the signer adds the wire
// hash; send checks the persisted wire is still that intent. It holds no money
// policy. Rows written before the budget was deleted carry extra fields, which
// decoding ignores.
type phase3OperationAuthorization struct {
	DebtClear        *debtClearAuthority `json:"debtClear,omitempty"`
	IntentSHA256     string              `json:"intentSha256"`
	SignedWireSHA256 string              `json:"signedWireSha256,omitempty"`
	BuildInput       *phase3BuildInput   `json:"buildInput,omitempty"`
	// CustodyProof is the pre-decision shared-custody ownership proof
	// (doc 26) of a positive AUTO-PYUSD spend, bound under the route lock.
	// Nil for every other lane and zero-spend operation.
	CustodyProof *sharedCustodyProofBinding `json:"custodyProof,omitempty"`
}

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
	case KaminoInitializationRequest:
		input.Kind = "kamino-initialize"
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

// decode compiles the persisted request against the embedded route manifest,
// exactly as every send/reconcile/journal path has always done.
func (input *phase3BuildInput) decode() (any, ExpectedEffects, []byte, error) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return nil, ExpectedEffects{}, nil, err
	}
	return input.decodeWithManifest(manifest)
}

// decodeWithManifest is the manifest-aware form of decode. Kamino,
// initializer and Jupiter requests compile via the manifest-bound compilers
// with the fixed bridge delegate; the delegate is never request-supplied.
// Bridge compilation alone is manifest-independent.
func (input *phase3BuildInput) decodeWithManifest(m RouteManifest) (any, ExpectedEffects, []byte, error) {
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
	case "kamino-initialize":
		target = &KaminoInitializationRequest{}
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
		message, err = compileKaminoMessageForDelegate(*r, mustKey(bridgeDelegate))
	case *KaminoInitializationRequest:
		request = *r
		message, err = m.compileKaminoInitializationMessage(*r)
	case *JupiterSwapRequest:
		request = *r
		message, err = compileJupiterMessageForDelegate(*r, mustKey(bridgeDelegate))
	}
	if err != nil {
		return nil, ExpectedEffects{}, nil, budgetHold("persisted_build_no_longer_compiles")
	}
	effects, err := decodeExpectedEffectsWithManifest(m, input.Effects)
	if err != nil {
		return nil, ExpectedEffects{}, nil, budgetHold("invalid_persisted_build_effects")
	}
	return request, effects, message, nil
}

// decodeExpectedEffectsWithManifest delegates every non-initializer shape to
// the public decoder unchanged; initializer effects revalidate through the
// SAME explicit manifest that compiled the request.
func decodeExpectedEffectsWithManifest(m RouteManifest, data []byte) (ExpectedEffects, error) {
	var envelope struct {
		Schema          string          `json:"schema"`
		ExpectedEffects json.RawMessage `json:"expectedEffects"`
	}
	raw := data
	if json.Unmarshal(data, &envelope) == nil && envelope.Schema == "loyal-backyard-rwa-operation-evidence/v1" {
		if len(envelope.ExpectedEffects) == 0 || string(envelope.ExpectedEffects) == "null" {
			return ExpectedEffects{}, fmt.Errorf("operation has no built expected effects")
		}
		raw = envelope.ExpectedEffects
	}
	var expected ExpectedEffects
	if json.Unmarshal(raw, &expected) != nil {
		return DecodeExpectedEffects(data)
	}
	if expected.Kind != "kamino-initialize" && expected.Initialization == nil {
		return DecodeExpectedEffects(data)
	}
	if err := m.validateInitializationEffects(expected); err != nil {
		return ExpectedEffects{}, err
	}
	return expected, nil
}

// Phase3IntentDigest binds the exact production request and expected effects,
// including blockhash, amounts, account graph and policy identities.
func Phase3IntentDigest(request any, effects []byte) (string, error) {
	if !json.Valid(effects) {
		return "", budgetHold("invalid_intent_effects")
	}
	encoded, err := json.Marshal(struct {
		Request any             `json:"request"`
		Effects json.RawMessage `json:"effects"`
	}{request, json.RawMessage(effects)})
	if err != nil {
		return "", fmt.Errorf("encode intent: %w", err)
	}
	return sha256Bytes(encoded), nil
}

// bindOperation is the tick's step between RecordDecision and build. Under
// the route lock it checks the decided row is this decision, then records the
// operation's integrity: the intent hash and exact build input of the prepared
// wire, the AUTO-PYUSD shared-custody proof, the selector-entry allocation and
// the debt-clear authority (with the partial-repayment proof it classifies).
func (d *Database) bindOperation(ctx context.Context, rpc *chain.Client, m RouteManifest, operationID string, o Observation, decision Decision, request any, effects ExpectedEffects) error {
	if d == nil || d.pool == nil || rpc == nil {
		return budgetHold("bind_database_unavailable")
	}
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return err
	}
	input, err := encodePhase3BuildInput(request, encoded)
	if err != nil {
		return err
	}
	intent, err := Phase3IntentDigest(request, encoded)
	if err != nil {
		return err
	}
	risk, err := verifyDebtClearEmergency(m, o, decision, operationID, time.Now().UTC())
	if err != nil {
		return err
	}
	plan := debtClearPlan{Snapshot: o.Snapshot, Decision: decision}
	if r, ok := request.(KaminoPrimeUSDCRequest); ok && partialRepaymentReason(decision.Reason) {
		projection, err := observePartialRepaymentProjection(ctx, rpc, m, o.Snapshot, decision, r, effects)
		if err != nil {
			return err
		}
		plan.Repayment = &projection
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = d.lockOperationLease(ctx, tx, operationID); err != nil {
		return err
	}
	var status, lane, routeKey, action string
	var bound bool
	var decisionBytes []byte
	if err = tx.QueryRow(ctx, `SELECT status,COALESCE(strategy_key,''),route_key,COALESCE(action,''),expected_effects->'decision',expected_effects ? 'phase3'
		FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&status, &lane, &routeKey, &action, &decisionBytes, &bound); err != nil {
		return err
	}
	var recorded decisionEvidence
	if json.Unmarshal(decisionBytes, &recorded) != nil || bound || status != string(Decided) || lane != decision.StrategyKey || action != string(decision.Action) ||
		recorded.StrategyKey != lane || recorded.AmountRaw != decision.AmountRaw || recorded.Reason != decision.Reason ||
		recorded.ObservationID != o.Snapshot.ObservationID || recorded.ObservationSlot <= 0 || recorded.ObservationSlot > o.Snapshot.Slot {
		return budgetHold("bind_journal_mismatch")
	}
	// The custody proof was observed before the row existed; it must still
	// bind the generation and lease of the route lock held here.
	custody, err := bindSharedCustodyAdmissionProofOnManifest(ctx, tx, routeKey, lane, o, effects)
	if err != nil {
		return err
	}
	slot, err := confirmedSlot(ctx, rpc)
	if err != nil {
		return err
	}
	auth := phase3OperationAuthorization{IntentSHA256: intent, BuildInput: input}
	if custody.SpendRaw > 0 {
		auth.CustodyProof = &custody
	}
	if err = d.authorizeDebtClearTx(ctx, tx, m, operationID, request, effects, plan, &auth, risk, slot); err != nil {
		return err
	}
	if err = d.authorizeSelectorEntryTxOnManifest(ctx, m, tx, operationID, request, effects, slot, true); err != nil {
		return err
	}
	raw, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{phase3}',$2::jsonb,true),updated_at=clock_timestamp() WHERE operation_id=$1 AND status='decided'`, operationID, string(raw))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("bind lost serialization")
	}
	return tx.Commit(ctx)
}

// requireBoundIntent runs before a builder loads its signer: the decided row
// must already carry the bind of exactly this request and effects, so no wire
// is signed (or shown to an RPC simulation) for an unbound operation.
func (d *Database) requireBoundIntent(ctx context.Context, operationID string, request any, effects []byte) error {
	if d == nil || d.pool == nil {
		return fmt.Errorf("database is not configured")
	}
	intent, err := Phase3IntentDigest(request, effects)
	if err != nil {
		return err
	}
	var bound string
	if err = d.pool.QueryRow(ctx, `SELECT COALESCE(expected_effects->'phase3'->>'intentSha256','') FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND status='decided'`, operationID).Scan(&bound); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return budgetHold("operation_not_bound")
		}
		return err
	}
	if bound != intent {
		return budgetHold("operation_not_bound")
	}
	return nil
}

// readPhase3AuthorizationTx reads the bound record under the caller's
// operation lock.
func readPhase3AuthorizationTx(ctx context.Context, tx pgx.Tx, operationID string) (phase3OperationAuthorization, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT COALESCE(expected_effects->'phase3','null'::jsonb) FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&raw); err != nil {
		return phase3OperationAuthorization{}, err
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(raw, &auth) != nil || !sha256Pattern.MatchString(auth.IntentSHA256) || auth.BuildInput == nil {
		return auth, budgetHold("operation_not_bound")
	}
	return auth, nil
}

// bindSignedWireTx records the signed wire's hash on the bound record, under
// the same transaction that persists the wire.
func bindSignedWireTx(ctx context.Context, tx pgx.Tx, operationID, wireSHA256 string) error {
	auth, err := readPhase3AuthorizationTx(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if !sha256Pattern.MatchString(wireSHA256) || auth.SignedWireSHA256 != "" && auth.SignedWireSHA256 != wireSHA256 {
		return budgetHold("signed_wire_binding_mismatch")
	}
	_, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{phase3,signedWireSha256}',to_jsonb($2::text),true),updated_at=clock_timestamp() WHERE operation_id=$1`, operationID, wireSHA256)
	return err
}

// validateSignedIdentity proves the persisted signed wire is the bound intent:
// the bound build input recompiles to the signed message, and its blockhash,
// expiry and signature are the row's.
func (m RouteManifest) validateSignedIdentity(auth phase3OperationAuthorization, operation PersistedOperation) (any, ExpectedEffects, error) {
	wire := operation.SignedWire
	if len(wire) <= 65 || wire[0] != 1 || auth.SignedWireSHA256 != sha256Bytes(wire) {
		return nil, ExpectedEffects{}, budgetHold("signed_wire_binding_mismatch")
	}
	request, effects, message, err := auth.BuildInput.decodeWithManifest(m)
	if err != nil {
		return nil, ExpectedEffects{}, err
	}
	digest, err := Phase3IntentDigest(request, auth.BuildInput.Effects)
	if err != nil || digest != auth.IntentSHA256 || !bytes.Equal(message, wire[65:]) {
		return nil, ExpectedEffects{}, budgetHold("persisted_build_intent_mismatch")
	}
	var blockhash string
	var height int64
	switch r := request.(type) {
	case BridgeBuildRequest:
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case KaminoPrimeUSDCRequest:
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case KaminoInitializationRequest:
		if m.validateInitializerDecision(operation.Decision, r) != nil {
			return nil, ExpectedEffects{}, budgetHold("initializer_journal_identity_mismatch")
		}
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case JupiterSwapRequest:
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	}
	if operation.SignedWireSHA256 != auth.SignedWireSHA256 || operation.RecentBlockhash != blockhash || operation.LastValidBlockHeight != height || operation.TransactionSignature != encodeBase58(wire[1:65]) {
		return nil, ExpectedEffects{}, budgetHold("persisted_signature_or_expiry_mismatch")
	}
	return request, effects, nil
}

// finalSend proves the persisted signed wire is still the bound intent and
// returns the locked fence that records broadcast intent before its first
// send. Nothing here reads the chain: the transaction itself enforces the
// swap minimum, klend limits, the Squads policy and the adaptor's report age,
// and simulation already ran it. The database fences run at the decision's
// observation slot.
func (d *Database) finalSend(ctx context.Context, manifest RouteManifest, operation PersistedOperation) (func(context.Context) error, error) {
	if d == nil || d.pool == nil || operation.ID == "" || operation.Status != Signed {
		return nil, fmt.Errorf("invalid final-send input")
	}
	var encoded []byte
	if err := d.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND status='signed'`, operation.ID).Scan(&encoded); err != nil {
		return nil, err
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(encoded, &auth) != nil {
		return nil, budgetHold("operation_not_bound")
	}
	request, _, err := manifest.validateSignedIdentity(auth, operation)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Decision decisionEvidence `json:"decision"`
	}
	if json.Unmarshal(operation.ExpectedEffects, &envelope) != nil || envelope.Decision.ObservationSlot <= 0 {
		return nil, budgetHold("operation_decision_slot_unavailable")
	}
	return func(ctx context.Context) error {
		return d.markBroadcastIntentOnManifest(ctx, manifest, operation.ID, request, auth, envelope.Decision.ObservationSlot)
	}, nil
}

// authorizeSendTx is the locked final-send fence: the signed row is the bound
// wire, the debt-clear authority it was bound under is still live, and a
// selector entry still authorizes its allocation.
func (d *Database) authorizeSendTx(ctx context.Context, m RouteManifest, tx pgx.Tx, operationID string, request any, bound phase3OperationAuthorization, slot int64) error {
	auth, err := readPhase3AuthorizationTx(ctx, tx, operationID)
	if err != nil {
		return err
	}
	var wire []byte
	if err = tx.QueryRow(ctx, `SELECT signed_wire FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND status='signed'`, operationID).Scan(&wire); err != nil {
		return err
	}
	if len(wire) == 0 || auth.SignedWireSHA256 != sha256Bytes(wire) || auth.IntentSHA256 != bound.IntentSHA256 || auth.SignedWireSHA256 != bound.SignedWireSHA256 {
		return budgetHold("final_send_identity_changed")
	}
	_, effects, _, err := auth.BuildInput.decodeWithManifest(m)
	if err != nil {
		return err
	}
	if err = d.checkSignedDebtClearTx(ctx, tx, m, operationID, request, auth); err != nil {
		return err
	}
	return d.authorizeSelectorEntryTxOnManifest(ctx, m, tx, operationID, request, effects, slot, false)
}
