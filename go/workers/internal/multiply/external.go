package multiply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Externally observed Earn MAX facts, ported from loyal-yield-store
// (admit_external_multiply_operation, project_earn_max_intent, the ready
// branch of project_earn_max_policy_set) and fleet_orchestration/multiply.rs
// (roll_terminal_policy_seed_base, upgrade_terminal_three_policy_manifest).
// The observer writes them; the Earn MAX worker owns everything it signs.

// RollTerminalPolicySeedBase moves a fully claimed, empty route onto a newer
// policy seed base so a re-created policy set can serve the next deposit.
func (s *RouteState) RollTerminalPolicySeedBase(policySeedBase, observedSlot uint64, observedAt time.Time) error {
	if policySeedBase == s.PolicySeedBase {
		return nil
	}
	if err := s.terminalRollable(policySeedBase, observedSlot); err != nil {
		return err
	}
	s.Generation++
	s.PolicySeedBase, s.ObservedSlot, s.ObservedAt = policySeedBase, observedSlot, observedAt
	return s.ValidatePersisted()
}

// UpgradeTerminalThreePolicyManifest moves a terminal earn_max_v1 (schema 8)
// route onto the current engine and the observed policy seed base.
func (s *RouteState) UpgradeTerminalThreePolicyManifest(policySeedBase, observedSlot uint64, observedAt time.Time) error {
	if s.EngineVersion != "earn_max_v1" || s.SchemaVersion != 8 {
		return errors.New("route goal change is invalid while another action is active")
	}
	if err := s.terminalRollable(policySeedBase, observedSlot); err != nil {
		return err
	}
	s.SchemaVersion, s.EngineVersion = StateSchemaVersion, EngineVersion
	s.Generation++
	s.PolicySeedBase, s.ObservedSlot, s.ObservedAt = policySeedBase, observedSlot, observedAt
	return s.ValidatePersisted()
}

func (s *RouteState) terminalRollable(policySeedBase, observedSlot uint64) error {
	emptyClaim := s.Position.Kind == "idle" && s.Position.Claim != nil && s.Position.Claim.AmountRaw == 0
	claimed := s.Withdrawal != nil && s.Withdrawal.Status == WithdrawalClaimed
	if policySeedBase <= s.PolicySeedBase || observedSlot < s.ObservedSlot || s.Goal != GoalClaimed || !emptyClaim || !claimed ||
		s.CurrentOperationID != nil || s.ManualRecoveryReason != nil {
		return errors.New("route goal change is invalid while another action is active")
	}
	return nil
}

func leasedNow(owner *string, expires *time.Time) bool {
	return owner != nil && expires != nil && expires.After(time.Now())
}

// ProjectReadyPolicySetInTx is the ready branch of project_earn_max_policy_set:
// a terminal route rolls to the newly ready policy seed base in the caller's
// policy-set transaction.
func ProjectReadyPolicySetInTx(ctx context.Context, tx pgx.Tx, settings string, vaultIndex uint8, policySeedBase, observedSlot uint64, observedAt time.Time) error {
	var raw []byte
	var owner *string
	var expires *time.Time
	err := tx.QueryRow(ctx, `
                SELECT state, lease_owner, lease_expires_at
                FROM loyal_yield.multiply_route_states
                WHERE settings=$1 AND vault_index=$2
                FOR UPDATE`, settings, int16(vaultIndex)).Scan(&raw, &owner, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if leasedNow(owner, expires) {
		return errors.New("terminal Earn MAX route is actively leased; retry policy projection")
	}
	state := &RouteState{}
	if err := jsonUnmarshalStrict(raw, state); err != nil {
		return err
	}
	if state.EngineVersion == EngineVersion && state.PolicySeedBase == policySeedBase {
		return nil
	}
	if state.EngineVersion == "earn_max_v1" {
		err = state.UpgradeTerminalThreePolicyManifest(policySeedBase, observedSlot, observedAt)
	} else {
		err = state.RollTerminalPolicySeedBase(policySeedBase, observedSlot, observedAt)
	}
	if err != nil {
		return err
	}
	encoded, err := jsonMarshal(state)
	if err != nil {
		return err
	}
	if state.Generation > 1<<63-1 {
		return errors.New("Earn MAX generation exceeds BIGINT")
	}
	_, err = tx.Exec(ctx, `
                        UPDATE loyal_yield.multiply_route_states
                        SET state=$2, state_version=$3, lease_owner=NULL,
                            lease_expires_at=NULL, updated_at=now()
                        WHERE route_key=$1`, state.RouteKey, encoded, int64(state.Generation))
	return err
}

// AdmitExternalOperation records one confirmed wallet deposit or root claim
// as a reconciled operation and advances the route in the same transaction.
func (s *Store) AdmitExternalOperation(ctx context.Context, lease *Lease, route *RouteState, operation *MultiplyOperation) (bool, error) {
	if err := validateNextRoute(lease, route); err != nil {
		return false, err
	}
	if err := operation.Validate(); err != nil {
		return false, err
	}
	if operation.Status != StatusReconciled || (operation.Action != ActionDepositClaimAsset && operation.Action != ActionClaim) ||
		operation.RouteKey != lease.RouteKey || operation.Cycle != route.Cycle || route.CurrentOperationID != nil {
		return false, errors.New("external operation is not bound to its route")
	}
	encoded, err := jsonMarshal(route)
	if err != nil {
		return false, err
	}
	var sourceIndex *int32
	if operation.SourceInstructionIndex != nil {
		value := int32(*operation.SourceInstructionIndex)
		sourceIndex = &value
	}
	var confirmed *int64
	if operation.ConfirmedSlot != nil {
		if *operation.ConfirmedSlot > 1<<63-1 {
			return false, errors.New("slot exceeds BIGINT")
		}
		value := int64(*operation.ConfirmedSlot)
		confirmed = &value
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx,
		"INSERT INTO loyal_yield.multiply_operations (operation_id, route_key, cycle, engine_version, action, strategy_key, status, idempotency_key, expected_effects, message_sha256, signed_wire_sha256, transaction_signature, source_instruction_index, recent_blockhash, confirmed_slot, reconciliation_sha256, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,'reconciled',$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16) ON CONFLICT DO NOTHING",
		operation.OperationID, operation.RouteKey, int64(operation.Cycle), EngineVersion, string(operation.Action),
		strategyKeyText(operation.StrategyKey), operation.IDempotencyKey, operation.ExpectedEffects, operation.MessageSHA256,
		operation.SignedWireSHA256, operation.TransactionSignature, sourceIndex, operation.RecentBlockhash, confirmed,
		operation.ReconciliationSHA256, operation.CreatedAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	version, ok, err := updateRouteInTx(ctx, tx, lease, encoded)
	if err != nil || !ok {
		return false, err
	}
	if operation.Action == ActionClaim {
		if route.Position.Kind != "idle" || route.Position.Claim == nil {
			return false, errors.New("Claimed route must have an idle position")
		}
		if confirmed == nil {
			return false, errors.New("Claim operation omitted its confirmed slot")
		}
		coverage := route.ObservedAt
		if route.Deposit != nil {
			coverage = route.Deposit.ObservedAt
		}
		snapshot, err := tx.Exec(ctx, `
                INSERT INTO loyal_yield.multiply_position_snapshots (
                    route_key, generation, observed_slot, observed_at, strategy_key,
                    claim_raw, collateral_raw, debt_raw, equity_usd_micros,
                    collateral_value_usd_micros, debt_value_usd_micros,
                    leverage_bps, ltv_bps, health_factor_ppm, supply_apy_bps,
                    borrow_apy_bps, forecast_apy_bps, valuation_source,
                    valuation_slot, valuation_observed_at, coverage_start_at
                ) VALUES (
                    $1, $2, $3, $4, NULL,
                    $5::numeric, 0, 0, $5::numeric,
                    0, 0,
                    NULL, NULL, NULL, NULL,
                    NULL, NULL, 'confirmed_claim_transfer',
                    $3, $4, $6
                )
                ON CONFLICT (route_key, observed_slot) DO NOTHING`,
			route.RouteKey, int64(route.Generation), *confirmed, route.ObservedAt,
			strconv.FormatUint(route.Position.Claim.AmountRaw, 10), coverage)
		if err != nil {
			return false, err
		}
		if snapshot.RowsAffected() != 1 {
			return false, errors.New("Claim snapshot observed slot already exists")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	lease.Version = version
	return true, nil
}

// IntentInput is one root-signed Earn MAX withdraw/cancel memo.
type IntentInput struct {
	Settings         string
	VaultIndex       uint8
	Signature        string
	InstructionIndex uint16
	Slot             uint64
	ObservedAt       time.Time
	// Withdraw is set for a withdraw memo; AmountRaw nil means "max".
	Withdraw *struct {
		RequestID, DestinationAccount string
		AmountRaw                     *uint64
	}
	CancelRequestID *string
}

// ProjectIntent is project_earn_max_intent: the chain location is the
// operation key, so replaying one memo is a no-op.
func (s *Store) ProjectIntent(ctx context.Context, input IntentInput) (bool, error) {
	if input.Settings == "" || input.Signature == "" || input.Slot == 0 {
		return false, errors.New("Earn MAX intent identity is malformed")
	}
	if input.Slot > 1<<63-1 {
		return false, errors.New("slot exceeds BIGINT")
	}
	idempotencyKey := fmt.Sprintf("%s:intent:%s:%d", EngineVersion, input.Signature, input.InstructionIndex)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var routeKey string
	var raw []byte
	var owner *string
	var expires *time.Time
	err = tx.QueryRow(ctx, `
            SELECT route_key, state, lease_owner, lease_expires_at
            FROM loyal_yield.multiply_route_states
            WHERE settings=$1 AND vault_index=$2
            FOR UPDATE`, input.Settings, int16(input.VaultIndex)).Scan(&routeKey, &raw, &owner, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errors.New("Earn MAX intent route is not projected yet")
	}
	if err != nil {
		return false, err
	}
	var duplicate bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations WHERE idempotency_key=$1)", idempotencyKey).Scan(&duplicate); err != nil {
		return false, err
	}
	if duplicate {
		return false, nil
	}
	if leasedNow(owner, expires) {
		return false, errors.New("Earn MAX intent route is actively leased; retry projection")
	}
	state := &RouteState{}
	if err := jsonUnmarshalStrict(raw, state); err != nil {
		return false, err
	}
	if input.Slot < state.ObservedSlot {
		return false, errors.New("Earn MAX intent is older than the projected route")
	}
	var action MultiplyAction
	var evidence map[string]any
	switch {
	case input.Withdraw != nil:
		var amount uint64
		if input.Withdraw.AmountRaw != nil {
			amount = *input.Withdraw.AmountRaw
		} else {
			var equity *string
			err := tx.QueryRow(ctx, `
                            SELECT equity_usd_micros::text
                            FROM loyal_yield.multiply_position_snapshots
                            WHERE route_key=$1
                            ORDER BY observed_slot DESC, id DESC
                            LIMIT 1`, routeKey).Scan(&equity)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && equity == nil) {
				return false, errors.New("Earn MAX full withdrawal has no current equity")
			}
			if err != nil {
				return false, err
			}
			if amount, err = strconv.ParseUint(*equity, 10, 64); err != nil {
				return false, errors.New("Earn MAX full withdrawal equity does not fit u64")
			}
		}
		if _, err := state.RequestWithdrawal(input.Withdraw.RequestID, input.Withdraw.DestinationAccount, amount, input.ObservedAt); err != nil {
			return false, err
		}
		action = ActionRequestWithdrawal
		evidence = map[string]any{"kind": "withdraw", "requestId": input.Withdraw.RequestID, "destinationAccount": input.Withdraw.DestinationAccount, "amountRaw": amount}
	case input.CancelRequestID != nil:
		if err := state.CancelWithdrawal(*input.CancelRequestID); err != nil {
			return false, err
		}
		action = ActionCancelWithdrawal
		evidence = map[string]any{"kind": "cancel", "requestId": *input.CancelRequestID}
	default:
		return false, errors.New("Earn MAX intent has no action")
	}
	state.ObservedSlot, state.ObservedAt = input.Slot, input.ObservedAt
	if err := state.ValidatePersisted(); err != nil {
		return false, err
	}
	evidenceBytes, err := json.Marshal(evidence)
	if err != nil {
		return false, err
	}
	reconciliation := sha256.Sum256(evidenceBytes)
	operationDigest := sha256.Sum256([]byte(idempotencyKey))
	encoded, err := jsonMarshal(state)
	if err != nil {
		return false, err
	}
	if state.Generation > 1<<63-1 || state.Cycle > 1<<63-1 {
		return false, errors.New("Earn MAX generation exceeds BIGINT")
	}
	changed, err := tx.Exec(ctx, `
            UPDATE loyal_yield.multiply_route_states
            SET state=$2, state_version=$3, lease_owner=NULL,
                lease_expires_at=NULL, updated_at=now()
            WHERE route_key=$1`, routeKey, encoded, int64(state.Generation))
	if err != nil {
		return false, err
	}
	if changed.RowsAffected() != 1 {
		return false, errors.New("Earn MAX intent route update missed")
	}
	effects, err := json.Marshal(map[string]any{"tokenDeltas": []any{}, "obligationDelta": nil, "intent": json.RawMessage(evidenceBytes)})
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
            INSERT INTO loyal_yield.multiply_operations (
                operation_id, route_key, cycle, engine_version, action, status,
                idempotency_key, expected_effects, transaction_signature,
                source_instruction_index, confirmed_slot, reconciliation_sha256,
                created_at, updated_at
            ) VALUES (
                $1, $2, $3, $4, $5, 'reconciled',
                $6, $7, $8, $9, $10, $11, $12, $12
            )`, "intent-"+hex.EncodeToString(operationDigest[:])[:32], routeKey, int64(state.Cycle), EngineVersion, string(action),
		idempotencyKey, effects, input.Signature, int32(input.InstructionIndex), int64(input.Slot),
		hex.EncodeToString(reconciliation[:]), input.ObservedAt); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
