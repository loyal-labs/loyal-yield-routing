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
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

const OperationInsert = `INSERT INTO loyal_yield.multiply_operations (operation_id, route_key, cycle, engine_version, action, status, idempotency_key, strategy_key, expected_effects, recovery_reason) VALUES ($1, $2, $3, 'backyard_rwa_v1', $4, $5, $6, $7, $8, $9)`

var (
	retiredRouteActions    = []Action{"SWAP_USDC_TO_PRIME_STEP", "SWAP_PRIME_TO_USDC_STEP", "OPEN_PRIME_USDC_STEP", "DELEVER_PRIME_USDC_STEP"}
	routeMutationActions   = slices.Concat(retiredRouteActions, []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep, OpenRouteStep, DeleverRouteStep})
	bridgeMutationActions  = []Action{VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle}
	ticketConsumingActions = []Action{VoltrAllocateToSquads, VoltrRestoreIdle, ReportNAV}
)

func actionsSQL(groups ...[]Action) string {
	quoted := []string{}
	for _, action := range slices.Concat(groups...) {
		quoted = append(quoted, "'"+string(action)+"'")
	}
	return strings.Join(quoted, ",")
}

var PostMutationNAVRequiredSQL = `SELECT COALESCE((SELECT action IN (` + actionsSQL(routeMutationActions) + `) FROM loyal_yield.multiply_operations WHERE route_key = $1 AND status = 'reconciled' AND action IN (` + actionsSQL(routeMutationActions, bridgeMutationActions, []Action{ReportNAV}) + `) ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id DESC LIMIT 1), false)`

var LatestDecisionEpochSQL = `SELECT COALESCE((SELECT operation_id FROM loyal_yield.multiply_operations WHERE route_key = $1 AND (status IN ('reconciled','failed') OR (status = 'manual_recovery' AND action = '` + string(ReportNAV) + `')) ORDER BY updated_at DESC, operation_id DESC LIMIT 1), 'genesis')`

var UnresolvedCapitalRecoverySQL = `SELECT EXISTS (SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key = $1 AND status = 'manual_recovery' AND action IN (` + actionsSQL(bridgeMutationActions, routeMutationActions) + `))`

const PersistSignedUpdate = `UPDATE loyal_yield.multiply_operations SET status = 'signed', message_sha256 = $2, signed_wire = $3, signed_wire_sha256 = $4, transaction_signature = $5, recent_blockhash = $6, last_valid_block_height = $7,
 simulation_slot = $8, simulation_result = $9::jsonb, expected_effects = jsonb_set(expected_effects, '{expectedEffects}', $10::jsonb), updated_at = now() WHERE operation_id = $1 AND status = 'decided'`

const PersistBroadcastIntentUpdate = `UPDATE loyal_yield.multiply_operations SET status = 'broadcast_intent', broadcast_intent_at = now(), updated_at = now() WHERE operation_id = $1 AND status = 'signed' AND signed_wire IS NOT NULL`

const RouteProjectionUpdate = `UPDATE loyal_yield.multiply_route_states SET state = jsonb_set(state, '{observation}', $2::jsonb, true), updated_at = clock_timestamp() WHERE route_key = $1 AND (state -> 'observation' ->> 'observedSlot' IS NULL OR (state -> 'observation' ->> 'observedSlot')::bigint <= $3) RETURNING state_version`

const PositionSnapshotInsert = `INSERT INTO loyal_yield.multiply_position_snapshots (route_key, generation, observed_slot, observed_at, strategy_key, claim_raw, collateral_raw, debt_raw, equity_usd_micros, collateral_value_usd_micros, debt_value_usd_micros, ltv_bps, forecast_apy_bps, valuation_source, valuation_slot, valuation_observed_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $4) ON CONFLICT (route_key, observed_slot) DO NOTHING`

type Database struct {
	pool   *pgxpool.Pool
	writer bool
	events map[string]*events
}

var errWriterSessionLost = errors.New("writer session lost")

const writerLockKey = `hashtextextended('loyal-family:'||$1, 0)`

// OpenDatabase opens the Backyard writer: one database session that takes
// Backyard's one-writer lock (the family advisory lock) when it connects,
// waiting while another process holds it, and keeps it for the session's life.
// Every read and write of the engine goes through that session. The pool
// never opens a second one: once the session ends, every later call fails with
// errWriterSessionLost, the engine exits, and its supervisor restarts it into
// a fresh wait.
func OpenDatabase(ctx context.Context, databaseURL engine.DirectDSN) (*Database, error) {
	return openWriter(ctx, string(databaseURL), true)
}

func openOperatorWriter(ctx context.Context, databaseURL string) (*Database, error) {
	return openWriter(ctx, databaseURL, false)
}

func openWriter(ctx context.Context, databaseURL string, wait bool) (*Database, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("database URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("invalid Backyard database configuration")
	}
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

const nonterminalStatusSQL = `'decided','built','simulated','signed','broadcast_intent','confirmed','reconciling'`

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

func (d *Database) ObservedNAVReport(ctx context.Context, routeKey string) (navReport, bool, error) {
	var raw []byte
	err := d.pool.QueryRow(ctx, `SELECT state->'observation' FROM loyal_yield.multiply_route_states WHERE route_key = $1`, routeKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && raw == nil {
		return navReport{}, false, nil
	}
	if err != nil {
		return navReport{}, false, fmt.Errorf("read persisted route observation: %w", err)
	}
	var projection routeObservationProjection
	if err := json.Unmarshal(raw, &projection); err != nil {
		return navReport{}, false, fmt.Errorf("decode persisted route observation: %w", err)
	}
	return projection.navReport()
}

type ReconciledBridgeJournalState struct {
	TicketSequenceKnown       bool
	TicketSequenceRaw         int64
	ArmedNAVKnown             bool
	ArmedNAVRaw               int64
	ArmedNAVReturnDataMissing bool
	ArmedNAVMalformed         bool
	StagedAmountKnown         bool
	StagedAmountRaw           int64
	StageAfterTicket          bool
	MutationAfterReport       bool
}

const journalOrderSQL = `confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC`

func journalLatestOfSQL(name string, actions []Action) string {
	return name + ` AS (SELECT * FROM strategy_journal WHERE action IN (` + actionsSQL(actions) + `) ORDER BY ` + journalOrderSQL + ` LIMIT 1)`
}

func journalAfterSQL(later, earlier string) string {
	return `COALESCE((SELECT l.confirmed_slot > 0 AND e.confirmed_slot > 0 AND
   (l.confirmed_slot, l.updated_at, l.operation_id COLLATE "C") > (e.confirmed_slot, e.updated_at, e.operation_id COLLATE "C")
  FROM ` + later + ` l, ` + earlier + ` e), false)`
}

var ReconciledBridgeJournalSQL = `WITH latest_per_action AS (
 SELECT DISTINCT ON (action) operation_id FROM loyal_yield.multiply_operations
 WHERE route_key = $1 AND status = 'reconciled' AND action IN (` + actionsSQL(bridgeMutationActions, []Action{ReportNAV}) + `)
 ORDER BY action, ` + journalOrderSQL + `
), strategy_journal AS (
 SELECT * FROM loyal_yield.multiply_operations WHERE operation_id IN (SELECT operation_id FROM latest_per_action)
), ` + journalLatestOfSQL("ticket", ticketConsumingActions) + `, ` + journalLatestOfSQL("mutation", bridgeMutationActions) + `,
` + journalLatestOfSQL("stage", []Action{StageSquadsToVoltr}) + `, ` + journalLatestOfSQL("report", []Action{ReportNAV}) + `
SELECT
 (SELECT (expected_effects->'decision'->>'observationSlot')::bigint FROM ticket),
 (SELECT expected_effects->'expectedEffects'->'returnData'->>'dataBase64' FROM ticket),
 (SELECT (expected_effects->'decision'->>'amountRaw')::bigint FROM stage),
 ` + journalAfterSQL("stage", "ticket") + `,
 ` + journalAfterSQL("mutation", "report")

func (d *Database) ReconciledBridgeJournal(ctx context.Context, routeKey string) (ReconciledBridgeJournalState, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return ReconciledBridgeJournalState{}, fmt.Errorf("database is not configured")
	}
	return d.readReconciledBridgeJournal(ctx, routeKey)
}

func (d *Database) readReconciledBridgeJournal(ctx context.Context, routeKey string) (ReconciledBridgeJournalState, error) {
	var sequence, staged pgtype.Int8
	var armed pgtype.Text
	state := ReconciledBridgeJournalState{}
	if err := d.pool.QueryRow(ctx, ReconciledBridgeJournalSQL, routeKey).Scan(&sequence, &armed, &staged, &state.StageAfterTicket, &state.MutationAfterReport); err != nil {
		return ReconciledBridgeJournalState{}, fmt.Errorf("read reconciled bridge journal: %w", err)
	}
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

func (p routeObservationProjection) navReport() (navReport, bool, error) {
	computed, computedErr := strconv.ParseInt(p.ComputedStrategyNAV, 10, 64)
	reported, reportedErr := strconv.ParseInt(p.ReportedNAVRaw, 10, 64)
	at, atErr := time.Parse(time.RFC3339, p.ReportObservedAt)
	if err := errors.Join(computedErr, reportedErr, atErr); err != nil {
		return navReport{}, false, fmt.Errorf("persisted route observation NAV fields: %w", err)
	}
	return navReport{at: at, holdsValue: computed > 0 || reported > 0 || p.RouteStatus == "positioned"}, true, nil
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

func (d *Database) MarkPreBroadcastFailed(ctx context.Context, operationID string, from OperationStatus, reason string) error {
	if reason == "" || !from.unsigned() {
		return fmt.Errorf("pre-broadcast failure requires an unsent source and explicit reason")
	}
	return d.transition(ctx, operationID, from, Failed, `, recovery_reason = $4`, reason)
}

func (d *Database) MarkExpiredAbsentFailed(ctx context.Context, operationID string, from OperationStatus, landed chain.Outcome) error {
	if landed.Kind != chain.Expired || !from.signedUnlanded() {
		return fmt.Errorf("expired-absent failure requires a signed source")
	}
	if err := d.transition(ctx, operationID, from, Failed,
		`, recovery_reason = 'signature_absent_after_blockhash_expiry'`); err != nil {
		return err
	}
	d.eventFailedAfterSend(ctx, operationID, "signature_absent_after_blockhash_expiry")
	return nil
}

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
	if slot <= 0 || from != BroadcastIntent {
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
