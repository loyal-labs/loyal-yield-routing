package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OperationInsert is the only execution journal shape. The SQL uses the existing
// multiply_operations table; callers must insert it before any transaction work.
const OperationInsert = `INSERT INTO loyal_yield.multiply_operations (operation_id, route_key, cycle, engine_version, action, status, idempotency_key, strategy_key, expected_effects, recovery_reason) VALUES ($1, $2, $3, 'backyard_rwa_v1', $4, $5, $6, $7, $8, $9)`

const RouteStateForUpdate = `SELECT state_version, state FROM loyal_yield.multiply_route_states WHERE route_key = $1 AND lease_owner = $2 AND fencing_token = $3 AND lease_expires_at > clock_timestamp() FOR UPDATE`

const AcquireRouteLeaseSQL = `UPDATE loyal_yield.multiply_route_states SET lease_owner = $2, lease_expires_at = clock_timestamp() + ($3 * interval '1 millisecond'), fencing_token = fencing_token + 1, updated_at = clock_timestamp() WHERE route_key = $1 AND (lease_owner IS NULL OR lease_expires_at <= clock_timestamp()) RETURNING fencing_token, lease_expires_at`

const RefreshRouteLeaseSQL = `UPDATE loyal_yield.multiply_route_states SET lease_expires_at = clock_timestamp() + ($4 * interval '1 millisecond'), updated_at = clock_timestamp() WHERE route_key = $1 AND lease_owner = $2 AND fencing_token = $3 AND lease_expires_at > clock_timestamp() RETURNING lease_expires_at`

const ReleaseRouteLeaseSQL = `UPDATE loyal_yield.multiply_route_states SET lease_owner = NULL, lease_expires_at = NULL, updated_at = clock_timestamp() WHERE route_key = $1 AND lease_owner = $2 AND fencing_token = $3`

const AssertRouteLeaseSQL = `SELECT lease_expires_at FROM loyal_yield.multiply_route_states WHERE route_key = $1 AND lease_owner = $2 AND fencing_token = $3 AND lease_expires_at > clock_timestamp()`

const OperationRouteForLeaseSQL = `SELECT operation.route_key FROM loyal_yield.multiply_operations operation JOIN loyal_yield.multiply_route_states route ON route.route_key = operation.route_key WHERE operation.operation_id = $1 AND route.lease_owner = $2 AND route.fencing_token = $3 AND route.lease_expires_at > clock_timestamp() FOR UPDATE OF route`

const PostMutationNAVRequiredSQL = `SELECT COALESCE((SELECT action IN ('SWAP_USDC_TO_PRIME_STEP','SWAP_PRIME_TO_USDC_STEP','OPEN_PRIME_USDC_STEP','DELEVER_PRIME_USDC_STEP','SWAP_STABLE_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_STABLE_STEP','OPEN_ROUTE_STEP','DELEVER_ROUTE_STEP') FROM loyal_yield.multiply_operations WHERE route_key = $1 AND status = 'reconciled' AND action IN ('SWAP_USDC_TO_PRIME_STEP','SWAP_PRIME_TO_USDC_STEP','OPEN_PRIME_USDC_STEP','DELEVER_PRIME_USDC_STEP','SWAP_STABLE_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_STABLE_STEP','OPEN_ROUTE_STEP','DELEVER_ROUTE_STEP','VOLTR_ALLOCATE_TO_SQUADS','STAGE_SQUADS_TO_VOLTR','VOLTR_RESTORE_IDLE','REPORT_NAV') ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id DESC LIMIT 1), false)`

// LatestDecisionEpochSQL advances after a fully reconciled mutation or an
// explicitly terminal pre-broadcast failure. A confirmed report-only operation
// that reached manual recovery is also safe to supersede: its immutable wire
// cannot move capital, while capital-moving manual recovery remains a hard
// automatic stop. This lets a corrected reconciler publish a fresh NAV report
// without making ambiguous money movement retryable.
const LatestDecisionEpochSQL = `SELECT COALESCE((SELECT operation_id FROM loyal_yield.multiply_operations WHERE route_key = $1 AND (status IN ('reconciled','failed') OR (status = 'manual_recovery' AND action = 'REPORT_NAV')) ORDER BY updated_at DESC, operation_id DESC LIMIT 1), 'genesis')`

// The sole exclusion is the operator-authorized, independently finalized
// Voltr restore incident. Keep every identity field in this predicate so no
// other manual recovery becomes executable merely by sharing an action.
const UnresolvedCapitalRecoverySQL = `SELECT EXISTS (SELECT 1 FROM loyal_yield.multiply_operations WHERE route_key = $1 AND status = 'manual_recovery' AND action IN ('VOLTR_ALLOCATE_TO_SQUADS','STAGE_SQUADS_TO_VOLTR','VOLTR_RESTORE_IDLE','SWAP_USDC_TO_PRIME_STEP','SWAP_PRIME_TO_USDC_STEP','OPEN_PRIME_USDC_STEP','DELEVER_PRIME_USDC_STEP','SWAP_STABLE_TO_COLLATERAL_STEP','SWAP_COLLATERAL_TO_STABLE_STEP','OPEN_ROUTE_STEP','DELEVER_ROUTE_STEP') AND NOT (operation_id = 'fe45a0369bf950da3ea311a4c493377cf9720a92c359c0bfbe739a3d9f699cbe' AND action = 'VOLTR_RESTORE_IDLE' AND transaction_signature = '46UBvSw1zjtZyDVUVaissm9SEXsKFKnYCQYKd23njb1NS1Ktkzsup5ic9XA55FxyTCpkoYuuM8hhn4MioGU2X7Wz' AND confirmed_slot = 444157954 AND recovery_reason = 'exact_effect_reconciliation_failed'))`

const PersistSignedUpdate = `UPDATE loyal_yield.multiply_operations SET status = 'signed', message_sha256 = $2, signed_wire = $3, signed_wire_sha256 = $4, transaction_signature = $5, recent_blockhash = $6, last_valid_block_height = $7, updated_at = now() WHERE operation_id = $1 AND status = 'simulated'`

const PersistBroadcastIntentUpdate = `UPDATE loyal_yield.multiply_operations SET status = 'broadcast_intent', broadcast_intent_at = now(), updated_at = now() WHERE operation_id = $1 AND status = 'signed' AND signed_wire IS NOT NULL`

const RouteProjectionUpdate = `UPDATE loyal_yield.multiply_route_states SET state = jsonb_set(state, '{observation}', $4::jsonb, true), updated_at = clock_timestamp() WHERE route_key = $1 AND lease_owner = $2 AND fencing_token = $3 AND lease_expires_at > clock_timestamp() AND (state -> 'observation' ->> 'observedSlot' IS NULL OR (state -> 'observation' ->> 'observedSlot')::bigint <= $5)`

const PositionSnapshotInsert = `INSERT INTO loyal_yield.multiply_position_snapshots (route_key, generation, observed_slot, observed_at, strategy_key, claim_raw, collateral_raw, debt_raw, equity_usd_micros, collateral_value_usd_micros, debt_value_usd_micros, ltv_bps, forecast_apy_bps, valuation_source, valuation_slot, valuation_observed_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'backyard_rwa_v1_onchain_route', $3, $4) ON CONFLICT (route_key, observed_slot) DO NOTHING`

func PersistedForSend(status OperationStatus) bool {
	return status == BroadcastIntent
}

var (
	ErrRouteLeaseUnavailable = errors.New("route lease is held by another worker")
	ErrRouteLeaseLost        = errors.New("route lease was lost or expired")
)

type RouteLease struct {
	RouteKey     string
	Owner        string
	FencingToken int64
	ExpiresAt    time.Time
}

type Database struct {
	pool    *pgxpool.Pool
	leaseMu sync.RWMutex
	lease   *RouteLease
}

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

func durationMilliseconds(duration time.Duration) (int64, error) {
	if duration <= 0 || duration%time.Millisecond != 0 {
		return 0, fmt.Errorf("lease duration must be a positive whole number of milliseconds")
	}
	milliseconds := duration.Milliseconds()
	if milliseconds <= 0 {
		return 0, fmt.Errorf("lease duration is too small")
	}
	return milliseconds, nil
}

func (d *Database) setLease(lease *RouteLease) {
	d.leaseMu.Lock()
	defer d.leaseMu.Unlock()
	if lease == nil {
		d.lease = nil
		return
	}
	copy := *lease
	d.lease = &copy
}

func (d *Database) currentLease() (RouteLease, error) {
	if d == nil || d.pool == nil {
		return RouteLease{}, fmt.Errorf("database is not configured")
	}
	d.leaseMu.RLock()
	defer d.leaseMu.RUnlock()
	if d.lease == nil {
		return RouteLease{}, ErrRouteLeaseLost
	}
	return *d.lease, nil
}

// AcquireRouteLease never treats an unexpired lease as re-entrant, even when
// its owner text is identical. Render can briefly overlap two instances during
// a restart; only an absent or expired row may increment the fencing token.
func (d *Database) AcquireRouteLease(ctx context.Context, routeKey, owner string, ttl time.Duration) (RouteLease, error) {
	if d == nil || d.pool == nil || routeKey == "" || owner == "" {
		return RouteLease{}, fmt.Errorf("database, route key, and lease owner are required")
	}
	milliseconds, err := durationMilliseconds(ttl)
	if err != nil {
		return RouteLease{}, err
	}
	var lease RouteLease
	lease.RouteKey, lease.Owner = routeKey, owner
	err = d.pool.QueryRow(ctx, AcquireRouteLeaseSQL, routeKey, owner, milliseconds).
		Scan(&lease.FencingToken, &lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RouteLease{}, ErrRouteLeaseUnavailable
	}
	if err != nil {
		return RouteLease{}, fmt.Errorf("acquire route lease: %w", err)
	}
	if lease.FencingToken <= 0 || lease.ExpiresAt.IsZero() {
		return RouteLease{}, fmt.Errorf("database returned an invalid route lease")
	}
	d.setLease(&lease)
	return lease, nil
}

func (d *Database) RefreshRouteLease(ctx context.Context, ttl time.Duration) (RouteLease, error) {
	lease, err := d.currentLease()
	if err != nil {
		return RouteLease{}, err
	}
	milliseconds, err := durationMilliseconds(ttl)
	if err != nil {
		return RouteLease{}, err
	}
	err = d.pool.QueryRow(ctx, RefreshRouteLeaseSQL, lease.RouteKey, lease.Owner, lease.FencingToken, milliseconds).
		Scan(&lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		d.setLease(nil)
		return RouteLease{}, ErrRouteLeaseLost
	}
	if err != nil {
		return RouteLease{}, fmt.Errorf("refresh route lease: %w", err)
	}
	d.setLease(&lease)
	return lease, nil
}

// ReleaseRouteLease clears only the exact owner/fencing pair. A zero-row result
// means ownership already changed and must never be overwritten by shutdown.
func (d *Database) ReleaseRouteLease(ctx context.Context) (bool, error) {
	lease, err := d.currentLease()
	if errors.Is(err, ErrRouteLeaseLost) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := d.pool.Exec(ctx, ReleaseRouteLeaseSQL, lease.RouteKey, lease.Owner, lease.FencingToken)
	if err != nil {
		return false, fmt.Errorf("release route lease: %w", err)
	}
	d.setLease(nil)
	return result.RowsAffected() == 1, nil
}

func (d *Database) AssertRouteLease(ctx context.Context, routeKey string) error {
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	if lease.RouteKey != routeKey {
		return ErrRouteLeaseLost
	}
	var expiresAt time.Time
	err = d.pool.QueryRow(ctx, AssertRouteLeaseSQL, lease.RouteKey, lease.Owner, lease.FencingToken).Scan(&expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		d.setLease(nil)
		return ErrRouteLeaseLost
	}
	if err != nil {
		return fmt.Errorf("assert route lease: %w", err)
	}
	return nil
}

func (d *Database) lockOperationLease(ctx context.Context, tx pgx.Tx, operationID string) error {
	lease, err := d.currentLease()
	if err != nil {
		return err
	}
	var routeKey string
	err = tx.QueryRow(ctx, OperationRouteForLeaseSQL, operationID, lease.Owner, lease.FencingToken).Scan(&routeKey)
	if errors.Is(err, pgx.ErrNoRows) || routeKey != lease.RouteKey {
		d.setLease(nil)
		return ErrRouteLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock operation lease: %w", err)
	}
	return nil
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
	if decision.Action == Hold || decision.Action == HoldManualRecovery {
		return routeKey + ":" + decision.IdempotencyKey, nil
	}
	if operationEpoch == "" {
		return "", fmt.Errorf("durable operation epoch is empty")
	}
	return routeKey + ":" + operationEpoch + ":" + decision.IdempotencyKey, nil
}

type decisionEvidence struct {
	AmountRaw           int64  `json:"amountRaw"`
	Reason              string `json:"reason"`
	ObservationID       string `json:"observationId"`
	ObservationSlot     int64  `json:"observationSlot"`
	ManifestSHA256      string `json:"manifestSha256"`
	PolicyCatalogSHA256 string `json:"policyCatalogSha256"`
	StrategyKey         string `json:"strategyKey"`
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

func newDecisionEvidence(observation Observation, decision Decision, manifestSHA256, policyCatalogSHA256 string) decisionEvidence {
	return decisionEvidence{
		AmountRaw: decision.AmountRaw, Reason: decision.Reason,
		ObservationID: observation.Snapshot.ObservationID, ObservationSlot: observation.Snapshot.Slot,
		ManifestSHA256: manifestSHA256, PolicyCatalogSHA256: policyCatalogSHA256,
		StrategyKey: decision.StrategyKey,
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
	if err := decision.Validate(); err != nil {
		return DecisionRecord{}, fmt.Errorf("validate decision before persistence: %w", err)
	}
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
	lease, err := d.currentLease()
	if err != nil || lease.RouteKey != routeKey {
		return DecisionRecord{}, ErrRouteLeaseLost
	}
	var stateVersion int64
	var routeState []byte
	if err := tx.QueryRow(ctx, RouteStateForUpdate, routeKey, lease.Owner, lease.FencingToken).Scan(&stateVersion, &routeState); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			d.setLease(nil)
			return DecisionRecord{}, ErrRouteLeaseLost
		}
		return DecisionRecord{}, fmt.Errorf("lock route: %w", err)
	}
	if stateVersion <= 0 || len(routeState) == 0 {
		return DecisionRecord{}, fmt.Errorf("invalid locked route state")
	}
	operationEpoch := ""
	if decision.Action != Hold && decision.Action != HoldManualRecovery {
		var unresolvedCapitalRecovery bool
		if err := tx.QueryRow(ctx, UnresolvedCapitalRecoverySQL, routeKey).Scan(&unresolvedCapitalRecovery); err != nil {
			return DecisionRecord{}, fmt.Errorf("read unresolved capital recovery: %w", err)
		}
		if unresolvedCapitalRecovery {
			return DecisionRecord{}, fmt.Errorf("unresolved capital-moving manual recovery blocks execution")
		}
		// Economic observations intentionally exclude slot. Namespace executable
		// decisions by the last completed lifecycle mutation so retries before
		// reconciliation dedupe, while a genuinely later cycle can execute the
		// same economic decision again. HOLD remains globally deduped.
		if err := tx.QueryRow(ctx, LatestDecisionEpochSQL, routeKey).Scan(&operationEpoch); err != nil {
			return DecisionRecord{}, fmt.Errorf("read durable operation epoch: %w", err)
		}
	}
	persistedIdempotencyKey, err := durableDecisionIdempotencyKey(routeKey, operationEpoch, decision)
	if err != nil {
		return DecisionRecord{}, err
	}
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
	strategyKey := decision.StrategyKey
	if strategyKey == "" {
		strategyKey = RouteID
	}
	if _, err := tx.Exec(ctx, OperationInsert, operationID, routeKey, cycle, string(decision.Action), string(status), persistedIdempotencyKey, strategyKey, string(expected), recoveryReason); err != nil {
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
	if err := d.AssertRouteLease(ctx, routeKey); err != nil {
		return nil, err
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
	if err := d.AssertRouteLease(ctx, routeKey); err != nil {
		return false, err
	}
	var required bool
	if err := d.pool.QueryRow(ctx, PostMutationNAVRequiredSQL, routeKey).Scan(&required); err != nil {
		return false, fmt.Errorf("read post-mutation NAV requirement: %w", err)
	}
	return required, nil
}

type routeObservationProjection struct {
	ObservedSlot         int64  `json:"observedSlot"`
	ObservedAt           string `json:"observedAt"`
	RouteStatus          string `json:"routeStatus"`
	VoltrIdleRaw         string `json:"voltrIdleRaw"`
	VoltrStrategyIdleRaw string `json:"voltrStrategyIdleRaw"`
	SquadsIdleRaw        string `json:"squadsIdleRaw"`
	AUMRaw               string `json:"aumRaw"`
	AUMUSDMicros         string `json:"aumUsdMicros"`
	NAVRaw               string `json:"navRaw"`
	NAVUSDMicros         string `json:"navUsdMicros"`
	ReportedNAVRaw       string `json:"reportedNavRaw"`
	ComputedStrategyNAV  string `json:"computedStrategyNavRaw"`
	ReportSequence       int64  `json:"reportSequence"`
	ReportSlot           int64  `json:"reportSlot"`
	ReportObservedAt     string `json:"reportObservedAt"`
	ReportSnapshotDigest string `json:"reportSnapshotDigest"`
	NAVFresh             bool   `json:"navFresh"`
}

func newRouteObservationProjection(observation Observation) (routeObservationProjection, error) {
	snapshot := observation.Snapshot
	if snapshot.VoltrIdleRaw < 0 || snapshot.VoltrStrategyIdleRaw < 0 || snapshot.SquadsIdleRaw < 0 ||
		snapshot.PositionCollateralRaw < 0 || snapshot.PositionDebtRaw < 0 ||
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
		ObservedSlot: snapshot.Slot, ObservedAt: observation.ObservedAt.UTC().Format(time.RFC3339Nano), RouteStatus: status,
		VoltrIdleRaw: fmt.Sprint(snapshot.VoltrIdleRaw), VoltrStrategyIdleRaw: fmt.Sprint(snapshot.VoltrStrategyIdleRaw),
		SquadsIdleRaw: fmt.Sprint(snapshot.SquadsIdleRaw), AUMRaw: fmt.Sprint(snapshot.TotalVaultNAVRaw),
		AUMUSDMicros: fmt.Sprint(snapshot.TotalVaultNAVRaw), NAVRaw: fmt.Sprint(snapshot.PriorReportedNAVRaw),
		NAVUSDMicros: fmt.Sprint(snapshot.PriorReportedNAVRaw), ReportedNAVRaw: fmt.Sprint(snapshot.PriorReportedNAVRaw),
		ComputedStrategyNAV: fmt.Sprint(snapshot.StrategyNAVRaw), ReportSequence: snapshot.ReportSequence,
		ReportSlot: snapshot.ReportSequence, ReportObservedAt: reportUpdatedAt.Format(time.RFC3339),
		ReportSnapshotDigest: snapshot.ReportSnapshotDigest,
		NAVFresh:             !snapshot.CapitalMutated && snapshot.LastReportAgeSeconds < 60,
	}, nil
}

// RecordPositionSnapshot atomically persists the confirmed route projection and
// its current position shape under the same live lease fence as decisions. Flat
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
	lease, err := d.currentLease()
	if err != nil || lease.RouteKey != routeKey {
		return ErrRouteLeaseLost
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var generation int64
	var state []byte
	if err := tx.QueryRow(ctx, RouteStateForUpdate, routeKey, lease.Owner, lease.FencingToken).Scan(&generation, &state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			d.setLease(nil)
			return ErrRouteLeaseLost
		}
		return fmt.Errorf("lock route for position snapshot: %w", err)
	}
	if snapshot.SquadsIdleRaw > math.MaxInt64-snapshot.VoltrStrategyIdleRaw {
		return fmt.Errorf("position snapshot idle claim overflows")
	}
	claimRaw := snapshot.SquadsIdleRaw + snapshot.VoltrStrategyIdleRaw
	result, err := tx.Exec(ctx, RouteProjectionUpdate,
		routeKey, lease.Owner, lease.FencingToken, projectionJSON, snapshot.Slot,
	)
	if err != nil {
		return fmt.Errorf("persist route observation projection: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("route observation projection lost its lease or regressed")
	}
	var strategyKey *string
	var forecastAPYBPS *int64
	if snapshot.HasPosition {
		value := snapshot.StrategyKey
		if value == "" {
			value = RouteID
		}
		strategyKey = &value
	} else {
		zero := int64(0)
		forecastAPYBPS = &zero
	}
	if _, err := tx.Exec(ctx, PositionSnapshotInsert,
		routeKey, generation, snapshot.Slot, observation.ObservedAt.UTC(), strategyKey, claimRaw,
		snapshot.PositionCollateralRaw, snapshot.PositionDebtRaw, snapshot.StrategyNAVRaw,
		snapshot.PositionCollateralValueRaw, snapshot.PositionDebtValueRaw, snapshot.LTVBPS, forecastAPYBPS,
	); err != nil {
		return fmt.Errorf("persist route position snapshot: %w", err)
	}
	return tx.Commit(ctx)
}

func (d *Database) transition(ctx context.Context, operationID string, from, to OperationStatus, suffix string, args ...any) error {
	if d == nil || d.pool == nil || operationID == "" || !CanTransition(from, to) {
		return fmt.Errorf("invalid durable transition %s -> %s", from, to)
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := d.lockOperationLease(ctx, tx, operationID); err != nil {
		return err
	}
	query := `UPDATE loyal_yield.multiply_operations SET status = $2, updated_at = now()` + suffix + ` WHERE operation_id = $1 AND status = $3`
	parameters := []any{operationID, string(to), string(from)}
	parameters = append(parameters, args...)
	result, err := tx.Exec(ctx, query, parameters...)
	if err != nil {
		return fmt.Errorf("persist transition %s -> %s: %w", from, to, err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("transition %s -> %s lost serialization", from, to)
	}
	return tx.Commit(ctx)
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
	if from != BroadcastIntent && from != Submitted {
		return fmt.Errorf("expired-absent failure requires a submitted source")
	}
	return d.transition(ctx, operationID, from, Failed,
		`, recovery_reason = 'signature_absent_after_blockhash_expiry'`)
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
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := d.lockOperationLease(ctx, tx, operationID); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, PersistSignedUpdate, operationID, build.MessageSHA256, build.SignedWire,
		build.SignedWireSHA256, build.TransactionSignature, build.RecentBlockhash, build.LastValidBlockHeight)
	if err != nil {
		return fmt.Errorf("persist exact signed wire: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("persist signed wire lost serialization")
	}
	return tx.Commit(ctx)
}

func (d *Database) MarkBroadcastIntent(ctx context.Context, operationID string) error {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := d.lockOperationLease(ctx, tx, operationID); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, PersistBroadcastIntentUpdate, operationID)
	if err != nil {
		return fmt.Errorf("persist broadcast intent: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("broadcast intent lost serialization")
	}
	return tx.Commit(ctx)
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
		tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := d.lockOperationLease(ctx, tx, operationID); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status = 'confirmed', confirmed_slot = $2, confirmation_status = 'confirmed', updated_at = now() WHERE operation_id = $1 AND status = 'broadcast_intent'`, operationID, slot)
		if err != nil {
			return fmt.Errorf("persist recovered confirmation: %w", err)
		}
		if result.RowsAffected() != 1 {
			return fmt.Errorf("persist recovered confirmation lost serialization")
		}
		return tx.Commit(ctx)
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
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := d.lockOperationLease(ctx, tx, operationID); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status = 'manual_recovery', recovery_reason = $2, updated_at = now() WHERE operation_id = $1 AND status = $3`, operationID, reason, string(from))
	if err != nil {
		return fmt.Errorf("persist manual recovery: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("persist manual recovery lost serialization")
	}
	return tx.Commit(ctx)
}
