package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OperationInsert is the only execution journal shape. The SQL uses the existing
// multiply_operations table; callers must insert it before any transaction work.
const OperationInsert = `INSERT INTO loyal_yield.multiply_operations (operation_id, route_key, cycle, engine_version, action, status, idempotency_key, expected_effects, recovery_reason) VALUES ($1, $2, $3, 'backyard_rwa_v1', $4, $5, $6, $7, $8)`

const RouteStateForUpdate = `SELECT state_version, state FROM loyal_yield.multiply_route_states WHERE route_key = $1 FOR UPDATE`

const PersistSignedUpdate = `UPDATE loyal_yield.multiply_operations SET status = 'signed', message_sha256 = $2, signed_wire = $3, signed_wire_sha256 = $4, transaction_signature = $5, recent_blockhash = $6, last_valid_block_height = $7, updated_at = now() WHERE operation_id = $1 AND status = 'simulated'`

const PersistBroadcastIntentUpdate = `UPDATE loyal_yield.multiply_operations SET status = 'broadcast_intent', broadcast_intent_at = now(), updated_at = now() WHERE operation_id = $1 AND status = 'signed' AND signed_wire IS NOT NULL`

const PositionSnapshotInsert = `INSERT INTO loyal_yield.multiply_position_snapshots (route_key, generation, observed_slot, observed_at, strategy_key, claim_raw, collateral_raw, debt_raw, equity_usd_micros, collateral_value_usd_micros, debt_value_usd_micros, ltv_bps, supply_apy_bps, borrow_apy_bps, forecast_apy_bps, valuation_source, valuation_slot, valuation_observed_at) VALUES ($1, $2, $3, $4, 'PRIME/USDC', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 'backyard_rwa_v1', $3, $4) ON CONFLICT (route_key, generation) DO NOTHING`

func PersistedForSend(status OperationStatus) bool {
	return status == BroadcastIntent
}

type Database struct{ pool *pgxpool.Pool }

func OpenDatabase(ctx context.Context, databaseURL string) (*Database, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("database URL is required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open Backyard database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping Backyard database: %w", err)
	}
	return &Database{pool: pool}, nil
}

func (d *Database) Close() {
	if d != nil && d.pool != nil {
		d.pool.Close()
	}
}

type DecisionRecord struct {
	OperationID string
	Cycle       int64
	Status      OperationStatus
}

type decisionEvidence struct {
	AmountRaw           int64  `json:"amountRaw"`
	Reason              string `json:"reason"`
	ObservationID       string `json:"observationId"`
	ObservationSlot     int64  `json:"observationSlot"`
	ManifestSHA256      string `json:"manifestSha256"`
	PolicyCatalogSHA256 string `json:"policyCatalogSha256"`
}

func newDecisionEvidence(observation Observation, decision Decision, manifestSHA256, policyCatalogSHA256 string) decisionEvidence {
	return decisionEvidence{
		AmountRaw: decision.AmountRaw, Reason: decision.Reason,
		ObservationID: observation.Snapshot.ObservationID, ObservationSlot: observation.Snapshot.Slot,
		ManifestSHA256: manifestSHA256, PolicyCatalogSHA256: policyCatalogSHA256,
	}
}

func initialDecisionStatus(decision Decision) (OperationStatus, any) {
	if decision.Action == Hold {
		return Held, nil
	}
	if decision.Action == HoldManualRecovery {
		return ManualRecovery, decision.Reason
	}
	return Decided, nil
}

// RecordDecision serializes on the existing route row and relies on the
// existing one-nonterminal partial unique index as the final concurrency gate.
// HOLD and HOLD_MANUAL_RECOVERY are persisted as terminal journal decisions;
// neither can accidentally occupy the transaction execution slot.
func (d *Database) RecordDecision(
	ctx context.Context,
	routeKey string,
	observation Observation,
	decision Decision,
	manifestSHA256 string,
	policyCatalogSHA256 string,
) (DecisionRecord, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return DecisionRecord{}, fmt.Errorf("database is not configured")
	}
	if err := observation.Validate(); err != nil {
		return DecisionRecord{}, err
	}
	if decision.Action != Hold && decision.Action != HoldManualRecovery &&
		(!observation.Snapshot.Fresh || observation.Snapshot.RouteKind != RouteKind) {
		return DecisionRecord{}, fmt.Errorf("transactional decision requires a fresh Backyard observation")
	}
	if err := decision.Validate(); err != nil {
		return DecisionRecord{}, err
	}
	if !sha256Pattern.MatchString(manifestSHA256) || !sha256Pattern.MatchString(policyCatalogSHA256) {
		return DecisionRecord{}, fmt.Errorf("manifest or policy catalog hash is invalid")
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DecisionRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var stateVersion int64
	var routeState []byte
	if err := tx.QueryRow(ctx, RouteStateForUpdate, routeKey).Scan(&stateVersion, &routeState); err != nil {
		return DecisionRecord{}, fmt.Errorf("lock route: %w", err)
	}
	if stateVersion <= 0 || len(routeState) == 0 {
		return DecisionRecord{}, fmt.Errorf("invalid locked route state")
	}
	persistedIdempotencyKey := routeKey + ":" + decision.IdempotencyKey
	var existing DecisionRecord
	var existingAction string
	var existingEffects []byte
	err = tx.QueryRow(ctx, `SELECT operation_id, cycle, status, action, expected_effects FROM loyal_yield.multiply_operations WHERE idempotency_key = $1`, persistedIdempotencyKey).
		Scan(&existing.OperationID, &existing.Cycle, &existing.Status, &existingAction, &existingEffects)
	if err == nil {
		if existingAction != string(decision.Action) {
			return DecisionRecord{}, fmt.Errorf("idempotency identity has a different action")
		}
		var existingEnvelope struct {
			Decision decisionEvidence `json:"decision"`
		}
		if json.Unmarshal(existingEffects, &existingEnvelope) != nil {
			return DecisionRecord{}, fmt.Errorf("idempotency identity has invalid decision evidence")
		}
		candidate := newDecisionEvidence(observation, decision, manifestSHA256, policyCatalogSHA256)
		// An identical economic state may be confirmed again at a later slot.
		// Preserve the first durable observation slot while treating the later
		// read as the same decision identity.
		candidate.ObservationSlot = existingEnvelope.Decision.ObservationSlot
		if existingEnvelope.Decision != candidate {
			return DecisionRecord{}, fmt.Errorf("idempotency identity has different decision evidence")
		}
		if err := tx.Commit(ctx); err != nil {
			return DecisionRecord{}, err
		}
		return existing, nil
	}
	if err != pgx.ErrNoRows {
		return DecisionRecord{}, fmt.Errorf("read duplicate decision: %w", err)
	}
	var cycle int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((state ->> 'cycle')::bigint, 1) FROM loyal_yield.multiply_route_states WHERE route_key = $1`, routeKey).Scan(&cycle); err != nil {
		return DecisionRecord{}, fmt.Errorf("read route cycle: %w", err)
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key = $1 AND status IN ('prepared','signed_persisted','broadcast_intent','confirmed','reconciliation_pending','decided','built','simulated','signed','submitted','reconciling'))`, routeKey).Scan(&active); err != nil {
		return DecisionRecord{}, fmt.Errorf("read active operation: %w", err)
	}
	if active {
		return DecisionRecord{}, fmt.Errorf("one nonterminal operation already exists")
	}
	expected, err := json.Marshal(map[string]any{
		"schema":          "loyal-backyard-rwa-operation-evidence/v1",
		"decision":        newDecisionEvidence(observation, decision, manifestSHA256, policyCatalogSHA256),
		"expectedEffects": nil,
	})
	if err != nil {
		return DecisionRecord{}, err
	}
	idHash := sha256.Sum256([]byte(persistedIdempotencyKey))
	operationID := hex.EncodeToString(idHash[:])
	status, recoveryReason := initialDecisionStatus(decision)
	if _, err := tx.Exec(ctx, OperationInsert, operationID, routeKey, cycle, string(decision.Action), string(status), persistedIdempotencyKey, string(expected), recoveryReason); err != nil {
		return DecisionRecord{}, fmt.Errorf("insert decision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DecisionRecord{}, err
	}
	return DecisionRecord{OperationID: operationID, Cycle: cycle, Status: status}, nil
}

const nonterminalStatusSQL = `'decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling'`

func (d *Database) LoadNonterminal(ctx context.Context, routeKey string) (*PersistedOperation, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return nil, fmt.Errorf("database is not configured")
	}
	row := d.pool.QueryRow(ctx, `SELECT operation_id, route_key, cycle, action, status, idempotency_key,
		expected_effects, COALESCE(signed_wire, ''::bytea), COALESCE(signed_wire_sha256, ''), COALESCE(transaction_signature, ''),
		COALESCE(recent_blockhash, ''), COALESCE(last_valid_block_height, 0),
		broadcast_intent_at IS NOT NULL, COALESCE(confirmed_slot, 0)
		FROM loyal_yield.multiply_operations
		WHERE route_key = $1 AND status IN (`+nonterminalStatusSQL+`)
		ORDER BY created_at, operation_id LIMIT 1`, routeKey)
	var operation PersistedOperation
	var action string
	var idempotencyKey string
	if err := row.Scan(
		&operation.ID, &operation.RouteKey, &operation.Cycle, &action, &operation.Status,
		&idempotencyKey, &operation.ExpectedEffects, &operation.SignedWire, &operation.SignedWireSHA256,
		&operation.TransactionSignature, &operation.RecentBlockhash,
		&operation.LastValidBlockHeight, &operation.BroadcastIntentRecorded,
		&operation.ConfirmedSlot,
	); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("load nonterminal operation: %w", err)
	}
	operation.Decision.Action = Action(action)
	operation.Decision.IdempotencyKey = idempotencyKey
	return &operation, nil
}

func (d *Database) transition(ctx context.Context, operationID string, from, to OperationStatus, suffix string, args ...any) error {
	if d == nil || d.pool == nil || operationID == "" || !CanTransition(from, to) {
		return fmt.Errorf("invalid durable transition %s -> %s", from, to)
	}
	query := `UPDATE loyal_yield.multiply_operations SET status = $2, updated_at = now()` + suffix + ` WHERE operation_id = $1 AND status = $3`
	parameters := []any{operationID, string(to), string(from)}
	parameters = append(parameters, args...)
	result, err := d.pool.Exec(ctx, query, parameters...)
	if err != nil {
		return fmt.Errorf("persist transition %s -> %s: %w", from, to, err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("transition %s -> %s lost serialization", from, to)
	}
	return nil
}

func (d *Database) MarkBuilt(ctx context.Context, operationID, messageSHA256 string, expectedEffects []byte) error {
	if !sha256Pattern.MatchString(messageSHA256) || !json.Valid(expectedEffects) {
		return fmt.Errorf("invalid built transaction evidence")
	}
	if _, err := DecodeExpectedEffects(expectedEffects); err != nil {
		return err
	}
	// Preserve the pre-construction decision envelope and merge only the
	// independently checked execution-effect fields.
	return d.transition(ctx, operationID, Decided, Built,
		`, message_sha256 = $4, expected_effects = jsonb_set(expected_effects, '{expectedEffects}', $5::jsonb)`, messageSHA256, string(expectedEffects))
}

func (d *Database) MarkSimulated(ctx context.Context, operationID string, simulation SimulationResult) error {
	if simulation.Slot <= 0 {
		return fmt.Errorf("invalid simulation evidence")
	}
	encoded, err := json.Marshal(simulation)
	if err != nil {
		return err
	}
	return d.transition(ctx, operationID, Built, Simulated,
		`, simulation_slot = $4, simulation_result = $5::jsonb`, simulation.Slot, string(encoded))
}

func (d *Database) PersistSigned(ctx context.Context, operationID string, build BuildResult) error {
	if err := build.validateForDelegate(mustKey(bridgeDelegate)); err != nil {
		return err
	}
	result, err := d.pool.Exec(ctx, PersistSignedUpdate, operationID, build.MessageSHA256, build.SignedWire,
		build.SignedWireSHA256, build.TransactionSignature, build.RecentBlockhash, build.LastValidBlockHeight)
	if err != nil {
		return fmt.Errorf("persist exact signed wire: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("persist signed wire lost serialization")
	}
	return nil
}

func (d *Database) MarkBroadcastIntent(ctx context.Context, operationID string) error {
	result, err := d.pool.Exec(ctx, PersistBroadcastIntentUpdate, operationID)
	if err != nil {
		return fmt.Errorf("persist broadcast intent: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("broadcast intent lost serialization")
	}
	return nil
}

func (d *Database) MarkSubmitted(ctx context.Context, operationID string) error {
	return d.transition(ctx, operationID, BroadcastIntent, Submitted, `, submitted_at = now()`)
}

func (d *Database) MarkConfirmed(ctx context.Context, operationID string, from OperationStatus, slot int64) error {
	if slot <= 0 || (from != BroadcastIntent && from != Submitted) {
		return fmt.Errorf("invalid confirmation")
	}
	if from == BroadcastIntent {
		// A crash may occur after send but before submitted is recorded. Observing
		// the persisted signature confirmed is the only allowed shortcut.
		result, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status = 'confirmed', confirmed_slot = $2, confirmation_status = 'confirmed', updated_at = now() WHERE operation_id = $1 AND status = 'broadcast_intent'`, operationID, slot)
		if err != nil {
			return fmt.Errorf("persist recovered confirmation: %w", err)
		}
		if result.RowsAffected() != 1 {
			return fmt.Errorf("persist recovered confirmation lost serialization")
		}
		return nil
	}
	return d.transition(ctx, operationID, Submitted, Confirmed,
		`, confirmed_slot = $4, confirmation_status = 'confirmed'`, slot)
}

func (d *Database) MarkReconciling(ctx context.Context, operationID string) error {
	return d.transition(ctx, operationID, Confirmed, Reconciling, ``)
}

func (d *Database) MarkReconciled(ctx context.Context, operationID string, reconciliation Reconciliation, effects []byte) error {
	if err := reconciliation.Validate(); err != nil || !json.Valid(effects) {
		return fmt.Errorf("invalid reconciliation evidence")
	}
	return d.transition(ctx, operationID, Reconciling, Reconciled,
		`, confirmed_slot = $4, reconciliation_sha256 = $5, reconciled_effects = $6::jsonb`,
		reconciliation.ConfirmedSlot, reconciliation.EffectsSHA256, string(effects))
}

func (d *Database) MarkManualRecovery(ctx context.Context, operationID string, from OperationStatus, reason string) error {
	if reason == "" || !IsNonterminal(from) {
		return fmt.Errorf("manual recovery reason and nonterminal source are required")
	}
	result, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status = 'manual_recovery', recovery_reason = $2, updated_at = now() WHERE operation_id = $1 AND status = $3`, operationID, reason, string(from))
	if err != nil {
		return fmt.Errorf("persist manual recovery: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("persist manual recovery lost serialization")
	}
	return nil
}
