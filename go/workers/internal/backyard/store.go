package backyard

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// OperationInsert is the only execution journal shape. The SQL uses the existing
// multiply_operations table; callers must insert it before any transaction work.
const OperationInsert = `INSERT INTO loyal_yield.multiply_operations (operation_id, route_key, cycle, engine_version, action, status, idempotency_key, strategy_key, expected_effects, recovery_reason) VALUES ($1, $2, $3, 'backyard_rwa_v1', $4, $5, $6, $7, $8, $9)`

// strategyJournalHead opens the journal CTE over one route's operations.
const strategyJournalHead = "WITH strategy_journal AS (\n SELECT * FROM loyal_yield.multiply_operations WHERE route_key = $1 AND "

// ActiveStrategyJournalCTE excludes only individually bound, reviewed records
// from the retired strategy. Unknown/unscoped records remain visible and can
// still trip accounting monitors. It never scopes by deployment or manifest.
const ActiveStrategyJournalCTE = strategyJournalHead + retiredStrategyExclusion + "\n) "

// retiredStrategyExclusion is the predicate that hides the reviewed records of
// the retired strategy.
const retiredStrategyExclusion = `NOT COALESCE(
   route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
   AND engine_version = 'backyard_rwa_v1'
   AND confirmed_slot BETWEEN 1 AND 444157954
   AND expected_effects->>'journalStrategyConfig' = '9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj'
   AND expected_effects->'journalAssociation'->>'schema' = 'backyard-strategy-association/v1'
   AND expected_effects->'journalAssociation'->>'operationId' = operation_id
   AND expected_effects->'journalAssociation'->>'routeKey' = route_key
   AND expected_effects->'journalAssociation'->>'confirmedSlot' = confirmed_slot::text
   AND expected_effects->'journalAssociation'->>'signature' = transaction_signature
   AND expected_effects->'journalAssociation'->>'action' = action
   AND expected_effects->'journalAssociation'->>'status' = status
   AND expected_effects->'journalAssociation'->>'currentStrategyConfig' = 'DCpR24Eb6xCWxDyaZvCBTkadkxCB2vkqJN1EfYNWtLxY'
   AND expected_effects->'journalAssociation'->>'bootstrapSlot' = '446086069'
   AND expected_effects->'journalAssociation'->>'evidenceSha256' = '3319db2f57baf743be2471d72a1e9d34168a81ebc0378071f7a7f28e1d968a64', false)`

const PostMutationNAVRequiredSQL = ActiveStrategyJournalCTE + `SELECT COALESCE((SELECT action IN ('SWAP_USDC_TO_PRIME_STEP','SWAP_PRIME_TO_USDC_STEP','OPEN_PRIME_USDC_STEP','DELEVER_PRIME_USDC_STEP','SWAP_STABLE_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_STABLE_STEP','SWAP_DEBT_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_DEBT_STEP','SWAP_USDC_TO_DEBT_STEP','SWAP_DEBT_TO_USDC_STEP','OPEN_ROUTE_STEP','DELEVER_ROUTE_STEP') FROM strategy_journal WHERE route_key = $1 AND status = 'reconciled' AND action IN ('SWAP_USDC_TO_PRIME_STEP','SWAP_PRIME_TO_USDC_STEP','OPEN_PRIME_USDC_STEP','DELEVER_PRIME_USDC_STEP','SWAP_STABLE_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_STABLE_STEP','SWAP_DEBT_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_DEBT_STEP','SWAP_USDC_TO_DEBT_STEP','SWAP_DEBT_TO_USDC_STEP','OPEN_ROUTE_STEP','DELEVER_ROUTE_STEP','VOLTR_ALLOCATE_TO_SQUADS','STAGE_SQUADS_TO_VOLTR','VOLTR_RESTORE_IDLE','REPORT_NAV') ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id DESC LIMIT 1), false)`

// LatestDecisionEpochSQL advances after a fully reconciled mutation or an
// explicitly terminal pre-broadcast failure. A confirmed report-only operation
// that reached manual recovery is also safe to supersede: its immutable wire
// cannot move capital, while capital-moving manual recovery remains a hard
// automatic stop. This lets a corrected reconciler publish a fresh NAV report
// without making ambiguous money movement retryable.
const LatestDecisionEpochSQL = `SELECT COALESCE((SELECT operation_id FROM loyal_yield.multiply_operations WHERE route_key = $1 AND (status IN ('reconciled','failed') OR (status = 'manual_recovery' AND action = 'REPORT_NAV')) ORDER BY updated_at DESC, operation_id DESC LIMIT 1), 'genesis')`

// A reviewed migration may attach an evidence-backed disposition to an old
// failed reconciliation. The original failure and wire remain intact. Bind the
// disposition to its own row, so copying metadata cannot clear another incident.
const UnresolvedCapitalRecoverySQL = `SELECT EXISTS (SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key = $1 AND status = 'manual_recovery' AND action IN ('VOLTR_ALLOCATE_TO_SQUADS','STAGE_SQUADS_TO_VOLTR','VOLTR_RESTORE_IDLE','SWAP_USDC_TO_PRIME_STEP','SWAP_PRIME_TO_USDC_STEP','OPEN_PRIME_USDC_STEP','DELEVER_PRIME_USDC_STEP','SWAP_STABLE_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_STABLE_STEP','SWAP_DEBT_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_DEBT_STEP','SWAP_USDC_TO_DEBT_STEP','SWAP_DEBT_TO_USDC_STEP','OPEN_ROUTE_STEP','DELEVER_ROUTE_STEP') AND NOT COALESCE(
 expected_effects->'manualResolution'->>'schema'='backyard-manual-resolution/v1'
 AND expected_effects->'manualResolution'->>'disposition'='superseded_by_strategy_reset'
 AND expected_effects->'manualResolution'->>'operationId'=operation_id
 AND expected_effects->'manualResolution'->>'routeKey'=route_key
 AND expected_effects->'manualResolution'->>'action'=action
 AND expected_effects->'manualResolution'->>'signature'=transaction_signature
 AND expected_effects->'manualResolution'->>'confirmedSlot'=confirmed_slot::text
 AND expected_effects->'manualResolution'->>'evidenceSha256' ~ '^[0-9a-f]{64}$'
 AND length(expected_effects->'manualResolution'->>'evidencePath')>0,false))`

// PersistSignedUpdate is the write-ahead record of a tick's transaction: the
// exact signed wire, its simulation and the effects reconcile checks, in one
// statement, before anything sends it.
const PersistSignedUpdate = `UPDATE loyal_yield.multiply_operations SET status = 'signed', message_sha256 = $2, signed_wire = $3, signed_wire_sha256 = $4, transaction_signature = $5, recent_blockhash = $6, last_valid_block_height = $7,
 simulation_slot = $8, simulation_result = $9::jsonb, expected_effects = jsonb_set(expected_effects, '{expectedEffects}', $10::jsonb), updated_at = now() WHERE operation_id = $1 AND status = 'decided'`

const PersistBroadcastIntentUpdate = `UPDATE loyal_yield.multiply_operations SET status = 'broadcast_intent', broadcast_intent_at = now(), updated_at = now() WHERE operation_id = $1 AND status = 'signed' AND signed_wire IS NOT NULL`

const RouteProjectionUpdate = `UPDATE loyal_yield.multiply_route_states SET state = jsonb_set(state, '{observation}', $2::jsonb, true), updated_at = clock_timestamp() WHERE route_key = $1 AND (state -> 'observation' ->> 'observedSlot' IS NULL OR (state -> 'observation' ->> 'observedSlot')::bigint <= $3) RETURNING state_version`

const PositionSnapshotInsert = `INSERT INTO loyal_yield.multiply_position_snapshots (route_key, generation, observed_slot, observed_at, strategy_key, claim_raw, collateral_raw, debt_raw, equity_usd_micros, collateral_value_usd_micros, debt_value_usd_micros, ltv_bps, forecast_apy_bps, valuation_source, valuation_slot, valuation_observed_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $4) ON CONFLICT (route_key, observed_slot) DO NOTHING`

// Database is Backyard's store. The engine's Database is the writer
// (OpenDatabase): one session that holds the family advisory lock, so no
// write here re-checks who the writer is.
type Database struct {
	pool *pgxpool.Pool
	// writer: the pool's one session holds the lock, and Close releases it.
	writer bool
	// events are each route's events, by route key, set by the engine
	// before its workers start.
	events map[string]*events
}

// errWriterSessionLost: the writer's one session ended, and the lock with it.
var errWriterSessionLost = errors.New("writer session lost")

// writerLockKey is Backyard's one-writer lock, the family advisory lock; $1 is
// the family.
const writerLockKey = `hashtextextended('loyal-family:'||$1, 0)`

// OpenDatabase opens the Backyard writer: one database session that takes
// Backyard's one-writer lock (the family advisory lock) when it connects,
// waiting while another process holds it, and keeps it for the session's life.
// Every read and write of the engine goes through that session. The pool
// never opens a second one: once the session ends, every later call fails with
// errWriterSessionLost, the engine exits, and its supervisor restarts it into
// a fresh wait. databaseURL must be the direct (non-pooler) DSN: a pooler
// hands the session to strangers.
func OpenDatabase(ctx context.Context, databaseURL string) (*Database, error) {
	return openWriter(ctx, databaseURL, true)
}

// openOperatorWriter is the writer of a one-shot operator write: the same one
// session, but it refuses with engine.ErrFamilyHeld instead of waiting while
// another process holds the lock, so it refuses while the engine runs.
func openOperatorWriter(ctx context.Context, databaseURL string) (*Database, error) {
	return openWriter(ctx, databaseURL, false)
}

// openWriter opens the one-session writer; wait chooses waiting for the lock
// over refusing it.
func openWriter(ctx context.Context, databaseURL string, wait bool) (*Database, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("database URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("invalid Backyard database configuration")
	}
	// The tick loop and the selector collector take turns on the one
	// session; the pool never retires it on its own.
	config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
	config.MaxConnLifetime, config.MaxConnIdleTime = math.MaxInt64, math.MaxInt64
	// A caller's deadline cancels its statement on the server and keeps the
	// session; pgx's default closes the socket, which would end the writer.
	// A server that does not answer the cancel still loses the session.
	config.ConnConfig.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: conn, DeadlineDelay: 10 * time.Second}
	}
	// A failed checkout ping destroys the session; a dead one fails its next
	// statement anyway.
	config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
	var dialed atomic.Bool
	config.BeforeConnect = func(context.Context, *pgx.ConnConfig) error {
		if dialed.Swap(true) {
			return errWriterSessionLost
		}
		return nil
	}
	family := string(engine.FamilyBackyard)
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if wait {
			_, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+writerLockKey+`)`, family)
			return err
		}
		var held bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+writerLockKey+`)`, family).Scan(&held); err != nil {
			return err
		}
		if !held {
			return fmt.Errorf("%s: %w", family, engine.ErrFamilyHeld)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open Backyard database: %w", err)
	}
	if wait {
		_, _ = fmt.Fprintln(os.Stdout, "backyard-rwa-worker: waiting for the Backyard writer lock")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("open Backyard database: %w", err)
	}
	return &Database{pool: pool, writer: true}, nil
}

// inTx runs fn in one transaction. ctx bounds fn's statements; begin, commit
// and rollback each get their own short context, because pgx closes a
// connection whose begin, commit or rollback fails, and the writer has one
// session. A ctx that ended during fn rolls back.
func (d *Database) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	bounded := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	}
	begin, cancel := bounded()
	tx, err := conn.Begin(begin)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		end, cancel := bounded()
		defer cancel()
		_ = tx.Rollback(end)
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	end, cancel := bounded()
	defer cancel()
	return tx.Commit(end)
}

// openOperatorDatabase is a plain pool that takes no lock, for the one-shot
// operator commands that run beside the engine: the reads (in-flight) and the
// operator inputs the engine reads on its next tick (exit-lane, clear-hold).
// A command that writes the engine's own rows opens openOperatorWriter.
func openOperatorDatabase(ctx context.Context, databaseURL string) (*Database, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("database URL is required")
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := db.Open(startup, databaseURL, 4)
	if err != nil {
		return nil, fmt.Errorf("open Backyard database: %w", err)
	}
	return &Database{pool: pool}, nil
}

// Close closes the store. The writer first releases the lock on its own
// session: Postgres frees a session's lock only once its backend exits, after
// the client has gone, so without the unlock the lock could still be held when
// Close returns. The session leaves the pool before it unlocks (the pool never
// dials another), so nothing writes through it without the lock.
func (d *Database) Close() {
	if d == nil || d.pool == nil {
		return
	}
	if d.writer {
		// Acquire waits for a caller still on the session, as pool.Close
		// would; a lost session fails it at once (the pool dials no other).
		if conn, err := d.pool.Acquire(context.Background()); err == nil {
			session := conn.Hijack()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = session.Exec(ctx, `SELECT pg_advisory_unlock(`+writerLockKey+`)`, string(engine.FamilyBackyard))
			_ = session.Close(ctx)
			cancel()
		}
	}
	d.pool.Close()
}

type DecisionRecord struct {
	OperationID string
	Cycle       int64
	Status      OperationStatus
}

func durableDecisionIdempotencyKey(routeKey, operationEpoch string, decision Decision) (string, error) {
	if routeKey == "" || decision.IdempotencyKey == "" {
		return "", fmt.Errorf("decision idempotency identity is incomplete")
	}
	if operationEpoch == "" {
		return "", fmt.Errorf("durable operation epoch is empty")
	}
	return routeKey + ":" + operationEpoch + ":" + decision.IdempotencyKey, nil
}

type decisionEvidence struct {
	ValuationSource string `json:"valuationSource,omitempty"`
	ValuationSlot   int64  `json:"valuationSlot,omitempty"`
	AmountRaw       int64  `json:"amountRaw"`
	Reason          string `json:"reason"`
	ObservationID   string `json:"observationId"`
	ObservationSlot int64  `json:"observationSlot"`
	ManifestSHA256  string `json:"manifestSha256"`
	StrategyKey     string `json:"strategyKey"`
}

func restorePersistedDecision(expectedEffects []byte, action Action, idempotencyKey, strategyKey string) (Decision, error) {
	var envelope struct {
		Decision decisionEvidence `json:"decision"`
	}
	if err := json.Unmarshal(expectedEffects, &envelope); err != nil {
		return Decision{}, fmt.Errorf("decode persisted decision envelope: %w", err)
	}
	if envelope.Decision.StrategyKey != strategyKey {
		return Decision{}, fmt.Errorf("persisted decision strategy does not match operation row")
	}
	decision := Decision{
		Action: action, IdempotencyKey: idempotencyKey, StrategyKey: strategyKey,
		AmountRaw: envelope.Decision.AmountRaw, Reason: envelope.Decision.Reason,
	}
	if err := decision.Validate(); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

func newDecisionEvidence(observation Observation, decision Decision, manifestSHA256 string) decisionEvidence {
	source, slot, _ := persistedValuationMetadata(observation)
	return decisionEvidence{
		ValuationSource: source, ValuationSlot: slot,
		AmountRaw: decision.AmountRaw, Reason: decision.Reason,
		ObservationID: observation.Snapshot.ObservationID, ObservationSlot: observation.Snapshot.Slot,
		ManifestSHA256: manifestSHA256,
		StrategyKey:    decision.StrategyKey,
	}
}

// RecordDecisionOnManifest journals the tick's transaction as a decided operation. Only
// transactions get rows: a hold is a gauge (holdLog) and a manual stop is the
// latch row. The one-nonterminal partial unique index keeps one operation in
// flight.
func (d *Database) RecordDecisionOnManifest(
	ctx context.Context,
	manifest RouteManifest,
	routeKey string,
	observation Observation,
	decision Decision,
	manifestSHA256 string,
) (DecisionRecord, error) {
	if decision.Action == Hold || decision.Action == HoldManualRecovery {
		return DecisionRecord{}, fmt.Errorf("a hold is not a journal row")
	}
	if err := validateDecisionPersistenceOnManifest(d, manifest, routeKey, observation, decision, manifestSHA256); err != nil {
		return DecisionRecord{}, err
	}
	var record DecisionRecord
	err := d.inTx(ctx, func(tx pgx.Tx) (err error) {
		record, err = d.recordDecisionTx(ctx, tx, manifest.vault.Strategy, routeKey, observation, decision, manifestSHA256)
		return err
	})
	if err != nil {
		return DecisionRecord{}, err
	}
	return record, nil
}

// Provenance is optional for historical confirmed evidence. Empty and explicit
// confirmed sources serialize identically; projected valuations remain explicit.
func persistedValuationMetadata(o Observation) (string, int64, error) {
	source, slot := o.Snapshot.ValuationSource, o.Snapshot.ValuationSlot
	if source == "" && slot == 0 {
		source, slot = o.ValuationSource, o.ValuationSlot
	} else if (o.ValuationSource != "" || o.ValuationSlot != 0) && (o.ValuationSource != source || o.ValuationSlot != slot) {
		return "", 0, fmt.Errorf("observation valuation provenance disagrees with snapshot")
	}
	if source == "" && slot == 0 {
		return "", 0, nil
	}
	if (source != "confirmed" && source != routeRefreshValuationSource) || slot <= 0 || slot != o.Snapshot.Slot {
		return "", 0, fmt.Errorf("observation valuation provenance is invalid")
	}
	if source == "confirmed" {
		return "", 0, nil
	}
	return source, slot, nil
}

func validateDecisionPersistenceOnManifest(
	d *Database,
	manifest RouteManifest,
	routeKey string,
	observation Observation,
	decision Decision,
	manifestSHA256 string,
) error {
	if err := decision.Validate(); err != nil {
		return fmt.Errorf("validate decision before persistence: %w", err)
	}
	if d == nil || d.pool == nil || routeKey == "" {
		return fmt.Errorf("database is not configured")
	}
	if err := observation.Validate(); err != nil {
		return err
	}
	if _, _, err := persistedValuationMetadata(observation); err != nil {
		return err
	}
	if decision.Action != Hold && decision.Action != HoldManualRecovery &&
		(!observation.Snapshot.Fresh || observation.Snapshot.RouteKind != RouteKind) {
		return fmt.Errorf("transactional decision requires a fresh Backyard observation")
	}
	if !sha256Pattern.MatchString(manifestSHA256) {
		return fmt.Errorf("manifest hash is invalid")
	}
	return nil
}

func (d *Database) recordDecisionTx(
	ctx context.Context,
	tx pgx.Tx,
	strategy string,
	routeKey string,
	observation Observation,
	decision Decision,
	manifestSHA256 string,
) (DecisionRecord, error) {
	var unresolvedCapitalRecovery bool
	if err := tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, routeKey).Scan(&unresolvedCapitalRecovery); err != nil {
		return DecisionRecord{}, fmt.Errorf("read unresolved capital recovery: %w", err)
	}
	if unresolvedCapitalRecovery {
		return DecisionRecord{}, fmt.Errorf("unresolved capital-moving manual recovery blocks execution")
	}
	// Economic observations intentionally exclude slot. Namespace the
	// operation by the last completed lifecycle mutation, so a later cycle
	// can execute the same economic decision again.
	var operationEpoch string
	if err := tx.QueryRow(ctx, LatestDecisionEpochSQL, routeKey).Scan(&operationEpoch); err != nil {
		return DecisionRecord{}, fmt.Errorf("read durable operation epoch: %w", err)
	}
	persistedIdempotencyKey, err := durableDecisionIdempotencyKey(routeKey, operationEpoch, decision)
	if err != nil {
		return DecisionRecord{}, err
	}
	var cycle int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((state ->> 'cycle')::bigint, 1) FROM loyal_yield.multiply_route_states WHERE route_key = $1`, routeKey).Scan(&cycle); err != nil {
		return DecisionRecord{}, fmt.Errorf("read route cycle: %w", err)
	}
	expected, err := json.Marshal(map[string]any{
		"schema":                "loyal-backyard-rwa-operation-evidence/v1",
		"journalStrategyConfig": strategy,
		"decision":              newDecisionEvidence(observation, decision, manifestSHA256),
		"expectedEffects":       nil,
	})
	if err != nil {
		return DecisionRecord{}, err
	}
	idHash := sha256.Sum256([]byte(persistedIdempotencyKey))
	operationID := hex.EncodeToString(idHash[:])
	if _, err := tx.Exec(ctx, OperationInsert, operationID, routeKey, cycle, string(decision.Action), string(Decided), persistedIdempotencyKey, decision.StrategyKey, string(expected), nil); err != nil {
		return DecisionRecord{}, fmt.Errorf("insert decision: %w", err)
	}
	return DecisionRecord{OperationID: operationID, Cycle: cycle, Status: Decided}, nil
}

const nonterminalStatusSQL = `'decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling'`

// CountNonterminal counts the route's operations a restart would resume. It
// is the release gate's read: it takes no writer lock, so it runs beside the
// live worker.
func CountNonterminal(ctx context.Context, databaseURL, routeKey string) (int, error) {
	database, err := openOperatorDatabase(ctx, databaseURL)
	if err != nil {
		return 0, err
	}
	defer database.Close()
	var count int
	if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.multiply_operations
		WHERE route_key = $1 AND status IN (`+nonterminalStatusSQL+`)`, routeKey).Scan(&count); err != nil {
		return 0, fmt.Errorf("count nonterminal operations: %w", err)
	}
	return count, nil
}

func (d *Database) LoadNonterminal(ctx context.Context, routeKey string) (*PersistedOperation, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return nil, fmt.Errorf("database is not configured")
	}
	row := d.pool.QueryRow(ctx, `SELECT operation_id, route_key, cycle, action, status, idempotency_key, COALESCE(strategy_key, ''),
		expected_effects, COALESCE(signed_wire, ''::bytea), COALESCE(signed_wire_sha256, ''), COALESCE(transaction_signature, ''),
		COALESCE(recent_blockhash, ''), COALESCE(last_valid_block_height, 0),
		broadcast_intent_at IS NOT NULL, COALESCE(confirmed_slot, 0)
		FROM loyal_yield.multiply_operations
		WHERE route_key = $1 AND status IN (`+nonterminalStatusSQL+`)
		ORDER BY created_at, operation_id LIMIT 1`, routeKey)
	var operation PersistedOperation
	var action string
	var idempotencyKey string
	var strategyKey string
	if err := row.Scan(
		&operation.ID, &operation.RouteKey, &operation.Cycle, &action, &operation.Status,
		&idempotencyKey, &strategyKey, &operation.ExpectedEffects, &operation.SignedWire, &operation.SignedWireSHA256,
		&operation.TransactionSignature, &operation.RecentBlockhash,
		&operation.LastValidBlockHeight, &operation.BroadcastIntentRecorded,
		&operation.ConfirmedSlot,
	); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("load nonterminal operation: %w", err)
	}
	operation.StrategyKey = strategyKey
	decision, restoreErr := restorePersistedDecision(operation.ExpectedEffects, Action(action), idempotencyKey, strategyKey)
	if restoreErr != nil {
		return nil, fmt.Errorf("loaded nonterminal decision is invalid: %w", restoreErr)
	}
	operation.Decision = decision
	return &operation, nil
}

// PostMutationNAVRequired is restart-safe accounting cadence. A reconciled
// Jupiter/Kamino action remains dirty until a later reconciled bridge action
// (all fixed bridge paths carry ReportV1) becomes the latest money mutation.
func (d *Database) PostMutationNAVRequired(ctx context.Context, routeKey string) (bool, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return false, fmt.Errorf("database is not configured")
	}
	var required bool
	if err := d.pool.QueryRow(ctx, PostMutationNAVRequiredSQL, routeKey).Scan(&required); err != nil {
		return false, fmt.Errorf("read post-mutation NAV requirement: %w", err)
	}
	return required, nil
}

// ReconciledBridgeJournalState is the journal evidence the production observe
// path merges into a snapshot. Every fact is read from reconciled rows only, so
// an on-chain state is explained by what this worker actually reconciled and
// never by comparing two valuations with each other.
type ReconciledBridgeJournalState struct {
	// Ticket-consuming evidence. Only VOLTR_ALLOCATE_TO_SQUADS,
	// VOLTR_RESTORE_IDLE, and REPORT_NAV consume the report ticket, and each of
	// those transactions ends in the adaptor's report instruction, whose return
	// data is the NAV the worker armed. STAGE_SQUADS_TO_VOLTR moves Squads cash
	// into strategy custody without invoking Voltr: it consumes no ticket and
	// returns nothing, so counting it deadlocked the sequence monitor.
	TicketSequenceKnown       bool
	TicketSequenceRaw         int64
	ArmedNAVKnown             bool
	ArmedNAVRaw               int64
	ArmedNAVReturnDataMissing bool
	ArmedNAVMalformed         bool
	// Stage-transient evidence: a reconciled stage newer than the last ticket
	// consumption means the route sits between its stage and restore legs, so
	// nonzero strategy custody is expected while Voltr still books zero.
	StagedAmountKnown bool
	StagedAmountRaw   int64
	StageAfterTicket  bool
	// MutationAfterReport is a reconciled bridge mutation newer than the last
	// reconciled report. Read straight from the journal, it is the only
	// accepted explanation for a NAV move (S1/S2).
	MutationAfterReport bool
}

// ReconciledBridgeJournalSQL reads every journal fact the fail-closed monitors
// arm from in one round trip. Rows are reconciled only; the
// ticket-consuming list deliberately excludes STAGE_SQUADS_TO_VOLTR, and every
// subquery shares one ordering so "after" means the same thing everywhere. The
// ordering rows additionally return their tie-break columns so the Go side can
// compare a composite identity (see journalOrderKey) instead of a bare slot.
// Every lookup below reads the latest reconciled row of a group of these four
// actions, which is always one action's own latest row. The journal holds a
// HOLD row about every ten seconds and over 17k reconciled reports: ranking
// narrow columns first and fetching at most four full rows keeps this read
// out of the NAV report window (it took five seconds).
const ReconciledBridgeJournalSQL = `WITH latest_per_action AS (
 SELECT DISTINCT ON (action) operation_id FROM loyal_yield.multiply_operations
 WHERE route_key = $1 AND status = 'reconciled'
   AND action IN ('REPORT_NAV','VOLTR_ALLOCATE_TO_SQUADS','VOLTR_RESTORE_IDLE','STAGE_SQUADS_TO_VOLTR')
   AND ` + retiredStrategyExclusion + `
 ORDER BY action, confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC
), strategy_journal AS (
 SELECT * FROM loyal_yield.multiply_operations WHERE operation_id IN (SELECT operation_id FROM latest_per_action)
) SELECT
 (SELECT (expected_effects->'decision'->>'observationSlot')::bigint FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled'
     AND action IN ('REPORT_NAV','VOLTR_ALLOCATE_TO_SQUADS','VOLTR_RESTORE_IDLE')
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT expected_effects->'expectedEffects'->'returnData'->>'dataBase64' FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled'
     AND action IN ('REPORT_NAV','VOLTR_ALLOCATE_TO_SQUADS','VOLTR_RESTORE_IDLE')
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT (expected_effects->'decision'->>'amountRaw')::bigint FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled' AND action = 'STAGE_SQUADS_TO_VOLTR'
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT confirmed_slot FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled' AND action = 'STAGE_SQUADS_TO_VOLTR'
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT confirmed_slot FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled'
     AND action IN ('REPORT_NAV','VOLTR_ALLOCATE_TO_SQUADS','VOLTR_RESTORE_IDLE')
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT confirmed_slot FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled'
     AND action IN ('VOLTR_ALLOCATE_TO_SQUADS','STAGE_SQUADS_TO_VOLTR','VOLTR_RESTORE_IDLE')
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT confirmed_slot FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled' AND action = 'REPORT_NAV'
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT updated_at FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled' AND action = 'STAGE_SQUADS_TO_VOLTR'
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT operation_id FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled' AND action = 'STAGE_SQUADS_TO_VOLTR'
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT updated_at FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled'
     AND action IN ('REPORT_NAV','VOLTR_ALLOCATE_TO_SQUADS','VOLTR_RESTORE_IDLE')
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT operation_id FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled'
     AND action IN ('REPORT_NAV','VOLTR_ALLOCATE_TO_SQUADS','VOLTR_RESTORE_IDLE')
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT updated_at FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled'
     AND action IN ('VOLTR_ALLOCATE_TO_SQUADS','STAGE_SQUADS_TO_VOLTR','VOLTR_RESTORE_IDLE')
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT operation_id FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled'
     AND action IN ('VOLTR_ALLOCATE_TO_SQUADS','STAGE_SQUADS_TO_VOLTR','VOLTR_RESTORE_IDLE')
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT updated_at FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled' AND action = 'REPORT_NAV'
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1),
 (SELECT operation_id FROM strategy_journal
   WHERE route_key = $1 AND status = 'reconciled' AND action = 'REPORT_NAV'
   ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT 1)`

// journalOrderKey is the composite ordering identity of one journal row: the
// confirmed slot plus exactly the tie-break columns the SQL ordering uses, so
// two operations confirmed in the same slot still have one deterministic
// latest row.
type journalOrderKey struct {
	slot        int64
	updatedAt   time.Time
	operationID string
}

// journalOrderAfter reports whether a sorts strictly newer than b under the
// SQL ordering: confirmed_slot DESC, then updated_at DESC, then operation_id
// DESC under its byte-wise C collation.
func journalOrderAfter(a, b journalOrderKey) bool {
	if a.slot <= 0 || b.slot <= 0 {
		return false
	}
	if a.slot != b.slot {
		return a.slot > b.slot
	}
	if !a.updatedAt.Equal(b.updatedAt) {
		return a.updatedAt.After(b.updatedAt)
	}
	return a.operationID > b.operationID
}

// ReconciledBridgeJournal returns the journal facts the monitors arm from.
// A reconciled ticket-consuming operation without usable adaptor return data is
// reported as missing or malformed instead of known=false, so the receipt
// monitor holds durably rather than silently disarming.
func (d *Database) ReconciledBridgeJournal(ctx context.Context, routeKey string) (ReconciledBridgeJournalState, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return ReconciledBridgeJournalState{}, fmt.Errorf("database is not configured")
	}
	return d.readReconciledBridgeJournal(ctx, routeKey)
}

func (d *Database) readReconciledBridgeJournal(ctx context.Context, routeKey string) (ReconciledBridgeJournalState, error) {
	var sequence, staged, stageSlot, ticketSlot, mutationSlot, reportSlot pgtype.Int8
	var stageUpdated, ticketUpdated, mutationUpdated, reportUpdated pgtype.Timestamptz
	var armed, stageOp, ticketOp, mutationOp, reportOp pgtype.Text
	err := d.pool.QueryRow(ctx, ReconciledBridgeJournalSQL, routeKey).
		Scan(&sequence, &armed, &staged, &stageSlot, &ticketSlot, &mutationSlot, &reportSlot,
			&stageUpdated, &stageOp, &ticketUpdated, &ticketOp, &mutationUpdated, &mutationOp, &reportUpdated, &reportOp)
	if err != nil {
		return ReconciledBridgeJournalState{}, fmt.Errorf("read reconciled bridge journal: %w", err)
	}
	state := ReconciledBridgeJournalState{}
	if sequence.Valid {
		state.TicketSequenceKnown, state.TicketSequenceRaw = true, sequence.Int64
	}
	if armed.Valid {
		raw, err := base64.StdEncoding.DecodeString(armed.String)
		switch {
		case err != nil || len(raw) != 8:
			state.ArmedNAVMalformed = true
		case int64(binary.LittleEndian.Uint64(raw)) < 0:
			state.ArmedNAVMalformed = true
		default:
			state.ArmedNAVKnown = true
			state.ArmedNAVRaw = int64(binary.LittleEndian.Uint64(raw))
		}
	} else if sequence.Valid {
		state.ArmedNAVReturnDataMissing = true
	}
	if staged.Valid {
		state.StagedAmountKnown, state.StagedAmountRaw = true, staged.Int64
	}
	stage := journalOrderKey{updatedAt: stageUpdated.Time, operationID: stageOp.String}
	if stageSlot.Valid {
		stage.slot = stageSlot.Int64
	}
	ticket := journalOrderKey{updatedAt: ticketUpdated.Time, operationID: ticketOp.String}
	if ticketSlot.Valid {
		ticket.slot = ticketSlot.Int64
	}
	mutation := journalOrderKey{updatedAt: mutationUpdated.Time, operationID: mutationOp.String}
	if mutationSlot.Valid {
		mutation.slot = mutationSlot.Int64
	}
	report := journalOrderKey{updatedAt: reportUpdated.Time, operationID: reportOp.String}
	if reportSlot.Valid {
		report.slot = reportSlot.Int64
	}
	if journalOrderAfter(stage, ticket) {
		state.StageAfterTicket = true
	}
	if journalOrderAfter(mutation, report) {
		state.MutationAfterReport = true
	}
	return state, nil
}

type routeObservationProjection struct {
	ValuationSource             string `json:"valuationSource,omitempty"`
	ValuationSlot               int64  `json:"valuationSlot,omitempty"`
	ObservedSlot                int64  `json:"observedSlot"`
	ObservedAt                  string `json:"observedAt"`
	RouteStatus                 string `json:"routeStatus"`
	VoltrIdleRaw                string `json:"voltrIdleRaw"`
	VoltrStrategyIdleRaw        string `json:"voltrStrategyIdleRaw"`
	SquadsIdleRaw               string `json:"squadsIdleRaw"`
	DebtIdleRaw                 string `json:"debtIdleRaw"`
	PayoffDebtRaw               string `json:"payoffDebtRaw"`
	CollateralIdleValueRaw      string `json:"collateralIdleValueRaw"`
	MinimumCollateralDepositRaw string `json:"minimumCollateralDepositRaw"`
	AUMRaw                      string `json:"aumRaw"`
	AUMUSDMicros                string `json:"aumUsdMicros"`
	NAVRaw                      string `json:"navRaw"`
	NAVUSDMicros                string `json:"navUsdMicros"`
	ReportedNAVRaw              string `json:"reportedNavRaw"`
	ComputedStrategyNAV         string `json:"computedStrategyNavRaw"`
	ReportSequence              int64  `json:"reportSequence"`
	ReportSlot                  int64  `json:"reportSlot"`
	ReportObservedAt            string `json:"reportObservedAt"`
	ReportSnapshotDigest        string `json:"reportSnapshotDigest"`
	NAVFresh                    bool   `json:"navFresh"`
}

func newRouteObservationProjection(observation Observation) (routeObservationProjection, error) {
	source, valuationSlot, err := persistedValuationMetadata(observation)
	if err != nil {
		return routeObservationProjection{}, err
	}
	snapshot := observation.Snapshot
	if snapshot.VoltrIdleRaw < 0 || snapshot.VoltrStrategyIdleRaw < 0 || snapshot.SquadsIdleRaw < 0 || snapshot.DebtIdleRaw < 0 || snapshot.PayoffDebtRaw < 0 ||
		snapshot.PositionCollateralRaw < 0 || snapshot.PositionDebtRaw < 0 || snapshot.CollateralIdleValueRaw < 0 || snapshot.MinimumCollateralDepositRaw < 0 ||
		snapshot.PositionCollateralValueRaw < 0 || snapshot.PositionDebtValueRaw < 0 ||
		snapshot.StrategyNAVRaw < 0 || snapshot.TotalVaultNAVRaw < 0 || snapshot.PriorReportedNAVRaw < 0 ||
		snapshot.LTVBPS < 0 || snapshot.LTVBPS > 10_000 || snapshot.LastReportAgeSeconds < 0 ||
		snapshot.ReportSequence <= 0 || !sha256Pattern.MatchString(snapshot.ReportSnapshotDigest) {
		return routeObservationProjection{}, fmt.Errorf("route observation projection is incoherent")
	}
	if snapshot.VoltrIdleRaw > math.MaxInt64-snapshot.StrategyNAVRaw ||
		snapshot.TotalVaultNAVRaw != snapshot.VoltrIdleRaw+snapshot.StrategyNAVRaw {
		return routeObservationProjection{}, fmt.Errorf("route AUM does not match confirmed custody NAV")
	}
	if snapshot.HasPosition {
		if snapshot.PositionCollateralRaw <= 0 || snapshot.PositionCollateralValueRaw < snapshot.PositionDebtValueRaw ||
			(snapshot.PositionDebtRaw > 0 && snapshot.PositionDebtValueRaw <= 0) {
			return routeObservationProjection{}, fmt.Errorf("position projection is incoherent")
		}
	} else if snapshot.PositionCollateralRaw != 0 || snapshot.PositionDebtRaw != 0 ||
		snapshot.PositionCollateralValueRaw != 0 || snapshot.PositionDebtValueRaw != 0 || snapshot.LTVBPS != 0 {
		return routeObservationProjection{}, fmt.Errorf("flat route contains position values")
	}
	reportUpdatedAt := observation.ObservedAt.UTC().Add(-time.Duration(snapshot.LastReportAgeSeconds) * time.Second)
	if snapshot.PriorReportUpdatedUnix > 0 {
		reportUpdatedAt = time.Unix(snapshot.PriorReportUpdatedUnix, 0).UTC()
	}
	status := "idle"
	if snapshot.HasPosition {
		status = "positioned"
	}
	if snapshot.WithdrawalDemandRaw > 0 {
		status = "withdrawal_pending"
	}
	return routeObservationProjection{
		ValuationSource: source, ValuationSlot: valuationSlot,
		ObservedSlot: snapshot.Slot, ObservedAt: observation.ObservedAt.UTC().Format(time.RFC3339Nano), RouteStatus: status,
		VoltrIdleRaw: fmt.Sprint(snapshot.VoltrIdleRaw), VoltrStrategyIdleRaw: fmt.Sprint(snapshot.VoltrStrategyIdleRaw),
		SquadsIdleRaw: fmt.Sprint(snapshot.SquadsIdleRaw), DebtIdleRaw: fmt.Sprint(snapshot.DebtIdleRaw), AUMRaw: fmt.Sprint(snapshot.TotalVaultNAVRaw),
		PayoffDebtRaw:               fmt.Sprint(snapshot.PayoffDebtRaw),
		CollateralIdleValueRaw:      fmt.Sprint(snapshot.CollateralIdleValueRaw),
		MinimumCollateralDepositRaw: fmt.Sprint(snapshot.MinimumCollateralDepositRaw),
		AUMUSDMicros:                fmt.Sprint(snapshot.TotalVaultNAVRaw), NAVRaw: fmt.Sprint(snapshot.PriorReportedNAVRaw),
		NAVUSDMicros: fmt.Sprint(snapshot.PriorReportedNAVRaw), ReportedNAVRaw: fmt.Sprint(snapshot.PriorReportedNAVRaw),
		ComputedStrategyNAV: fmt.Sprint(snapshot.StrategyNAVRaw), ReportSequence: snapshot.ReportSequence,
		ReportSlot: snapshot.ReportSequence, ReportObservedAt: reportUpdatedAt.Format(time.RFC3339),
		ReportSnapshotDigest: snapshot.ReportSnapshotDigest,
		NAVFresh:             !snapshot.CapitalMutated && !snapshot.PostMutationNAVRequired && snapshot.LastReportAgeSeconds < int64(routineNAVReportInterval.Seconds()),
	}, nil
}

// RecordPositionSnapshot atomically persists the confirmed route projection and
// its current position shape. Flat
// and collateral-only rows are retained so the admin view cannot fall back to a
// stale leveraged snapshot. APY remains unknown for positions; confirmed idle
// capital has an exact zero forecast rather than an invented protocol yield.
func (d *Database) RecordPositionSnapshot(ctx context.Context, routeKey string, observation Observation) error {
	if d == nil || d.pool == nil || routeKey == "" || observation.Validate() != nil {
		return fmt.Errorf("position snapshot database or observation is invalid")
	}
	snapshot := observation.Snapshot
	projection, err := newRouteObservationProjection(observation)
	if err != nil {
		return err
	}
	projectionJSON, err := json.Marshal(projection)
	if err != nil {
		return fmt.Errorf("encode route observation projection: %w", err)
	}
	if snapshot.SquadsIdleRaw > math.MaxInt64-snapshot.VoltrStrategyIdleRaw {
		return fmt.Errorf("position snapshot idle claim overflows")
	}
	claimRaw := snapshot.SquadsIdleRaw + snapshot.VoltrStrategyIdleRaw
	var strategyKey *string
	var forecastAPYBPS *int64
	if snapshot.HasPosition {
		value := snapshot.StrategyKey
		strategyKey = &value
	} else {
		zero := int64(0)
		forecastAPYBPS = &zero
	}
	valuationSource, valuationSlot := projection.ValuationSource, projection.ValuationSlot
	if valuationSource == "" {
		valuationSource, valuationSlot = "backyard_rwa_v1_onchain_route", snapshot.Slot
	}
	return d.inTx(ctx, func(tx pgx.Tx) error {
		var generation int64
		if err := tx.QueryRow(ctx, RouteProjectionUpdate, routeKey, projectionJSON, snapshot.Slot).Scan(&generation); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("route observation projection regressed")
			}
			return fmt.Errorf("persist route observation projection: %w", err)
		}
		if _, err := tx.Exec(ctx, PositionSnapshotInsert,
			routeKey, generation, snapshot.Slot, observation.ObservedAt.UTC(), strategyKey, claimRaw,
			snapshot.PositionCollateralRaw, snapshot.PositionDebtRaw, snapshot.StrategyNAVRaw,
			snapshot.PositionCollateralValueRaw, snapshot.PositionDebtValueRaw, snapshot.LTVBPS, forecastAPYBPS, valuationSource, valuationSlot,
		); err != nil {
			return fmt.Errorf("persist route position snapshot: %w", err)
		}
		return nil
	})
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
		return fmt.Errorf("transition %s -> %s found no %s operation", from, to, from)
	}
	return nil
}

// MarkPreBroadcastFailed releases the one-nonterminal invariant only for
// states that prove no send was possible. Recovery never rebuilds, re-signs,
// or submits the abandoned operation.
func (d *Database) MarkPreBroadcastFailed(ctx context.Context, operationID string, from OperationStatus, reason string) error {
	if reason == "" || (from != Decided && from != Built && from != Simulated) {
		return fmt.Errorf("pre-broadcast failure requires an unsent source and explicit reason")
	}
	return d.transition(ctx, operationID, from, Failed, `, recovery_reason = $4`, reason)
}

// MarkExpiredAbsentFailed releases the one-nonterminal invariant only after
// lifecycle recovery has independently proved both facts that make a landing
// impossible: the persisted signature is absent and its blockhash is expired.
// Found, ambiguous, or failed-on-chain signatures must remain recovery stops.
func (d *Database) MarkExpiredAbsentFailed(ctx context.Context, operationID string, from OperationStatus) error {
	if from != Signed && from != BroadcastIntent && from != Submitted {
		return fmt.Errorf("expired-absent failure requires a signed source")
	}
	if err := d.transition(ctx, operationID, from, Failed,
		`, recovery_reason = 'signature_absent_after_blockhash_expiry'`); err != nil {
		return err
	}
	d.eventFailedAfterSend(ctx, operationID, "signature_absent_after_blockhash_expiry")
	return nil
}

// PersistSigned records the tick's exact signed wire with its simulation and
// expected effects. Nothing is sent before it commits.
func (d *Database) PersistSigned(ctx context.Context, operationID string, signed SignedTransaction, simulation SimulationResult, effects []byte) error {
	if simulation.Slot <= 0 || !json.Valid(effects) {
		return fmt.Errorf("invalid signed transaction evidence")
	}
	result, err := simulationJSON(simulation)
	if err != nil {
		return err
	}
	tag, err := d.pool.Exec(ctx, PersistSignedUpdate, operationID, signed.messageSHA256, signed.signedWire, signed.signedWireSHA256,
		signed.transactionSignature, signed.recentBlockhash, signed.lastValidBlockHeight, simulation.Slot, result, string(effects))
	if err != nil {
		return fmt.Errorf("persist exact signed wire: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("persist signed wire found no decided operation")
	}
	return nil
}

// MarkBroadcastIntent records that the signed wire may now be sent.
func (d *Database) MarkBroadcastIntent(ctx context.Context, operationID string) error {
	tag, err := d.pool.Exec(ctx, PersistBroadcastIntentUpdate, operationID)
	if err != nil {
		return fmt.Errorf("persist broadcast intent: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("broadcast intent found no signed operation")
	}
	return nil
}

func (d *Database) MarkConfirmed(ctx context.Context, operationID string, from OperationStatus, slot int64) error {
	if slot <= 0 || (from != BroadcastIntent && from != Submitted) {
		return fmt.Errorf("invalid confirmation")
	}
	tag, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status = 'confirmed', confirmed_slot = $2, confirmation_status = 'confirmed', updated_at = now() WHERE operation_id = $1 AND status = $3`, operationID, slot, string(from))
	if err != nil {
		return fmt.Errorf("persist confirmation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("confirmation found no %s operation", from)
	}
	return nil
}

// MarkReconciled settles a landed operation whose finalized receipt matched
// its persisted effects.
func (d *Database) MarkReconciled(ctx context.Context, operationID string, reconciliation Reconciliation, effects []byte) error {
	if err := reconciliation.Validate(); err != nil || !json.Valid(effects) {
		return fmt.Errorf("invalid reconciliation evidence")
	}
	tag, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciled',confirmation_status='finalized',reconciliation_sha256=$2,reconciled_effects=$3::jsonb,updated_at=clock_timestamp() WHERE operation_id=$1 AND status IN ('confirmed','reconciling')`, operationID, reconciliation.EffectsSHA256, string(effects))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("reconciliation found no landed operation")
	}
	d.eventReconciled(ctx, operationID)
	return nil
}

func (d *Database) MarkManualRecovery(ctx context.Context, operationID string, from OperationStatus, reason string) error {
	if reason == "" || !IsNonterminal(from) {
		return fmt.Errorf("manual recovery reason and nonterminal source are required")
	}
	tag, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status = 'manual_recovery', recovery_reason = $2, updated_at = now() WHERE operation_id = $1 AND status = $3`, operationID, reason, string(from))
	if err != nil {
		return fmt.Errorf("persist manual recovery: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("manual recovery found no %s operation", from)
	}
	d.eventFailedAfterSend(ctx, operationID, reason)
	return nil
}
