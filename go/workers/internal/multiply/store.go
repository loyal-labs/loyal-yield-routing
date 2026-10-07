package multiply

// Durable store ported statement-for-statement from
// crates/loyal-yield-store/src/multiply_state_store.rs. Every UPDATE is
// fenced by state_version + lease_owner + fencing_token + lease expiry; the
// signed wire is immutable once persisted (only expiry/manual recovery
// null it, never a replacement).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/gagliardetto/solana-go"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

const (
	// LeaseTTL mirrors MULTIPLY_DEFAULT_LEASE_SECONDS.
	LeaseTTL = 30 * time.Second

	operationColumns = "operation_id, route_key, cycle, engine_version, action, strategy_key, status, idempotency_key, expected_effects, policy_account, policy_data_sha256, message_sha256, signed_wire, signed_wire_sha256, transaction_signature, source_instruction_index, recent_blockhash, last_valid_block_height, broadcast_intent_at, confirmed_slot, reconciliation_sha256, created_at, updated_at"

	routeStateSelect = "SELECT route_key, settings, vault_index, vault, state, state_version, fencing_token FROM loyal_yield.multiply_route_states WHERE route_key=$1"
)

// Store is the pool-injected durable multiply store.
type Store struct {
	pool     *pgxpool.Pool
	ownsPool bool
}

// NewStore opens the pooled connection through the root-owned db package.
func NewStore(ctx context.Context, dsn string) (*Store, error) {
	pool, err := db.Open(ctx, dsn, 4)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, ownsPool: true}, nil
}

// NewStoreFromPool borrows the engine's pool. The caller owns its lifetime;
// this constructor only checks the family's registered schema under the caller context.
func NewStoreFromPool(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	if ctx == nil || pool == nil {
		return nil, errors.New("multiply store requires caller context and pool")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store := &Store{pool: pool}
	if err := store.RequireSchema(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return store, nil
}

// Close releases only a pool opened by NewStore. A borrowed engine pool remains
// usable by other retail families until the engine owner closes it.
func (s *Store) Close() {
	if s != nil && s.ownsPool && s.pool != nil {
		s.pool.Close()
	}
}

// RequireSchema fails closed unless the currently registered multiply tables
// exist (the migrations are owned by the root schema, never created here).
func (s *Store) RequireSchema(ctx context.Context) error {
	return db.RequireTables(ctx, s.pool,
		"loyal_yield.multiply_route_states",
		"loyal_yield.multiply_operations",
		"loyal_yield.multiply_position_snapshots",
		"loyal_yield.earn_max_policy_sets",
		"loyal_yield.managed_vaults",
		"loyal_yield.route_policies",
		"loyal_yield.rebalance_opportunities",
		"loyal_yield.rebalance_decisions",
		"loyal_yield.balance_sweep_targets",
		"loyal_yield.balance_sweep_lot_claims",
		"loyal_yield.balance_sweep_transaction_attempts",
	)
}

// Pool exposes the injected pool for root composition (metrics, health).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Lease is a fencing-token route lease.
type Lease struct {
	RouteKey     string
	Owner        string
	ExpiresAt    time.Time
	FencingToken int64
	Version      int64
}

// StoredRoute pairs the typed route state with its lease columns.
type StoredRoute struct {
	RouteKey     string
	Settings     string
	VaultIndex   uint8
	Vault        string
	State        *RouteState
	Version      int64
	FencingToken int64
	Operation    *MultiplyOperation
}

// ReadyPolicySet mirrors ReadyEarnMaxPolicySet.
type ReadyPolicySet struct {
	Settings       string
	VaultIndex     uint8
	Vault          string
	PolicySeedBase uint64
	ObservedSlot   uint64
}

// PositionSnapshotInput mirrors MultiplyPositionSnapshotInput.
type PositionSnapshotInput struct {
	RouteKey            string
	Generation          uint64
	ObservedSlot        uint64
	ObservedAt          time.Time
	StrategyKey         *string
	ClaimRaw            string // numeric text
	CollateralRaw       string
	DebtRaw             string
	EquityUSD           string
	CollateralValueUSD  string
	DebtValueUSD        string
	LeverageBPS         *int64
	LTVBPS              *int64
	HealthFactorPPM     *int64
	SupplyAPYBPS        *int64
	BorrowAPYBPS        *int64
	ForecastAPYBPS      *string
	ValuationSource     string
	ValuationSlot       *uint64
	ValuationObservedAt *time.Time
	CoverageStartAt     *time.Time
}

// RecordPositionSnapshot mirrors record_multiply_position_snapshot; false
// means the (route_key, observed_slot) row already existed.
func (s *Store) RecordPositionSnapshot(ctx context.Context, input *PositionSnapshotInput) (bool, error) {
	if input == nil || strings.TrimSpace(input.RouteKey) == "" || input.Generation == 0 || input.Generation > 1<<63-1 || input.ObservedSlot == 0 || input.ObservedSlot > 1<<63-1 {
		return false, errors.New("multiply position snapshot identity is invalid")
	}
	var valuationSlot *int64
	if input.ValuationSlot != nil {
		value, err := int64FromU64(*input.ValuationSlot, "valuation slot")
		if err != nil {
			return false, err
		}
		valuationSlot = &value
	}
	tag, err := s.pool.Exec(ctx, `
        INSERT INTO loyal_yield.multiply_position_snapshots (
            route_key, generation, observed_slot, observed_at, strategy_key,
            claim_raw, collateral_raw, debt_raw, equity_usd_micros,
            collateral_value_usd_micros, debt_value_usd_micros,
            leverage_bps, ltv_bps, health_factor_ppm, supply_apy_bps,
            borrow_apy_bps, forecast_apy_bps, valuation_source,
            valuation_slot, valuation_observed_at, coverage_start_at
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6::numeric, $7::numeric, $8::numeric, $9::numeric,
            $10::numeric, $11::numeric,
            $12, $13, $14, $15,
            $16, $17, $18,
            $19, $20, $21
        )
        ON CONFLICT (route_key, observed_slot) DO NOTHING`,
		input.RouteKey, int64(input.Generation), int64(input.ObservedSlot), input.ObservedAt, input.StrategyKey,
		input.ClaimRaw, input.CollateralRaw, input.DebtRaw, input.EquityUSD,
		input.CollateralValueUSD, input.DebtValueUSD,
		input.LeverageBPS, input.LTVBPS, input.HealthFactorPPM, input.SupplyAPYBPS,
		input.BorrowAPYBPS, input.ForecastAPYBPS, input.ValuationSource,
		valuationSlot, input.ValuationObservedAt, input.CoverageStartAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// LoadUnbootstrappedPolicySet mirrors load_unbootstrapped_earn_max_policy_set.
func (s *Store) LoadUnbootstrappedPolicySet(ctx context.Context) (*ReadyPolicySet, error) {
	row := s.pool.QueryRow(ctx, `
        SELECT policy.settings, policy.vault_index, policy.vault,
               policy.policy_seed_base, policy.observed_slot
        FROM loyal_yield.earn_max_policy_sets policy
        WHERE policy.status = 'ready'
          AND policy.manifest_version = 'earn-max-v2'
          AND NOT EXISTS (
              SELECT 1
              FROM loyal_yield.multiply_route_states route
              WHERE route.settings = policy.settings
                AND route.vault_index = policy.vault_index
          )
        ORDER BY policy.observed_slot, policy.settings
        LIMIT 1`)
	var set ReadyPolicySet
	var vaultIndex, seedBase, observedSlot int64
	err := row.Scan(&set.Settings, &vaultIndex, &set.Vault, &seedBase, &observedSlot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if vaultIndex < 0 || vaultIndex > 255 {
		return nil, errors.New("earn max vault index exceeds u8")
	}
	if seedBase < 0 || observedSlot < 0 {
		return nil, errors.New("earn max policy identity is negative")
	}
	set.VaultIndex = uint8(vaultIndex)
	set.PolicySeedBase = uint64(seedBase)
	set.ObservedSlot = uint64(observedSlot)
	return &set, nil
}

// EarnMaxPolicySetReady mirrors earn_max_policy_set_ready.
func (s *Store) EarnMaxPolicySetReady(ctx context.Context, settings string, vaultIndex uint8, policySeedBase uint64) (bool, error) {
	var ready bool
	err := s.pool.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1
            FROM loyal_yield.earn_max_policy_sets
            WHERE settings = $1
              AND vault_index = $2
              AND manifest_version = 'earn-max-v2'
              AND status = 'ready'
              AND policy_seed_base = $3
        )`, settings, int16(vaultIndex), int64(policySeedBase)).Scan(&ready)
	return ready, err
}

// CreateRouteState mirrors create_multiply_route_state.
func (s *Store) CreateRouteState(ctx context.Context, state *RouteState) (bool, error) {
	if err := validateRouteState(state.RouteKey, state); err != nil {
		return false, err
	}
	if state.Generation != 1 {
		return false, errors.New("new route generation must be one")
	}
	encoded, err := jsonMarshal(state)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx,
		"INSERT INTO loyal_yield.multiply_route_states (route_key, settings, vault_index, vault, state) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (route_key) DO NOTHING",
		state.RouteKey, state.Settings, int16(state.VaultIndex), state.Vault, encoded)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// LoadRouteState mirrors load_multiply_route_state including the column/state
// identity checks and the current-operation binding check.
func (s *Store) LoadRouteState(ctx context.Context, routeKey string) (*StoredRoute, error) {
	var (
		key, settings, vault string
		vaultIndex           int16
		rawState             []byte
		version, fencing     int64
	)
	err := s.pool.QueryRow(ctx, routeStateSelect, routeKey).
		Scan(&key, &settings, &vaultIndex, &vault, &rawState, &version, &fencing)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	state := &RouteState{}
	if err := jsonUnmarshalStrict(rawState, state); err != nil {
		return nil, fmt.Errorf("route state is not schema-9 compatible: %w", err)
	}
	if err := validateRouteState(key, state); err != nil {
		return nil, err
	}
	if state.Settings != settings || int16(state.VaultIndex) != vaultIndex ||
		state.Vault != vault || uint64(version) != state.Generation {
		return nil, errors.New("route columns drifted from typed state")
	}
	stored := &StoredRoute{
		RouteKey: key, Settings: settings, VaultIndex: uint8(vaultIndex),
		Vault: vault, State: state, Version: version, FencingToken: fencing,
	}
	if state.CurrentOperationID != nil {
		operation, err := s.LoadOperation(ctx, *state.CurrentOperationID)
		if err != nil {
			return nil, err
		}
		if operation == nil {
			return nil, errors.New("route points at a missing current operation")
		}
		if operation.RouteKey != key || operation.Status.IsTerminal() {
			return nil, errors.New("route points at an invalid current operation")
		}
		stored.Operation = operation
	}
	return stored, nil
}

// LoadRouteStateBySettings mirrors load_multiply_route_state_by_settings.
func (s *Store) LoadRouteStateBySettings(ctx context.Context, settings string, vaultIndex uint8) (*StoredRoute, error) {
	if strings.TrimSpace(settings) == "" {
		return nil, errors.New("settings is empty")
	}
	var key string
	err := s.pool.QueryRow(ctx,
		"SELECT route_key FROM loyal_yield.multiply_route_states WHERE settings=$1 AND vault_index=$2",
		settings, int16(vaultIndex)).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.LoadRouteState(ctx, key)
}

// LoadOperation mirrors load_multiply_operation.
func (s *Store) LoadOperation(ctx context.Context, operationID string) (*MultiplyOperation, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+operationColumns+" FROM loyal_yield.multiply_operations WHERE operation_id=$1", operationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	return decodeOperation(rows)
}

// OperationExistsForSignature mirrors multiply_operation_exists_for_signature.
func (s *Store) OperationExistsForSignature(ctx context.Context, routeKey, signature string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1
            FROM loyal_yield.multiply_operations
            WHERE route_key = $1 AND transaction_signature = $2
        )`, routeKey, signature).Scan(&exists)
	return exists, err
}

// LeaseNextRoute shares the Rust lease rows and fencing tokens. Pending
// operations remain eligible after readiness removal or a new withdrawal
// debounce; those gates control only fresh admission.
func (s *Store) LeaseNextRoute(ctx context.Context, owner string, expiresAt time.Time) (*Lease, error) {
	if err := validateLeaseInput(owner, expiresAt); err != nil {
		return nil, err
	}
	var version, fencing int64
	var leasedKey string
	err := s.pool.QueryRow(ctx, `
        WITH candidate AS (
            SELECT route.route_key
            FROM loyal_yield.multiply_route_states route
            WHERE (route.lease_owner IS NULL OR route.lease_expires_at <= now())
              AND route.state ->> 'engineVersion' = 'earn_max_v2'
              AND (
                COALESCE(route.state ->> 'currentOperationId', '') <> ''
                OR (
                  EXISTS (
                    SELECT 1 FROM loyal_yield.earn_max_policy_sets policy
                    WHERE policy.settings = route.settings
                      AND policy.vault_index = route.vault_index
                      AND policy.status = 'ready'
                      AND policy.manifest_version = 'earn-max-v2'
                      AND policy.policy_seed_base = (route.state ->> 'policySeedBase')::BIGINT
                  )
                  AND (
                    route.state ->> 'goal' IN ('deploy', 'move')
                    OR (route.state ->> 'goal' = 'withdraw'
                        AND route.state #>> '{withdrawal,status}' <> 'claimable')
                    OR (route.state ->> 'goal' IN ('idle', 'claimed')
                        AND NOT EXISTS (
                          SELECT 1 FROM loyal_yield.multiply_position_snapshots snapshot
                          WHERE snapshot.route_key = route.route_key
                            AND snapshot.observed_at > now() - interval '5 minutes'
                        ))
                  )
                  AND NOT COALESCE((
                    route.state #>> '{withdrawal,status}' = 'requested'
                    AND (route.state #>> '{withdrawal,requestedAt}')::timestamptz
                      > now() - interval '30 seconds'
                  ), FALSE)
                )
              )
            ORDER BY (COALESCE(route.state ->> 'currentOperationId', '') <> '') DESC, route.updated_at, route.route_key
            FOR UPDATE OF route SKIP LOCKED
            LIMIT 1
        )
        UPDATE loyal_yield.multiply_route_states route
        SET lease_owner = $1,
            lease_expires_at = $2,
            fencing_token = route.fencing_token + 1
        FROM candidate
        WHERE route.route_key = candidate.route_key
        RETURNING route.route_key, route.state_version, route.fencing_token`,
		owner, expiresAt).Scan(&leasedKey, &version, &fencing)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Lease{RouteKey: leasedKey, Owner: owner, ExpiresAt: expiresAt,
		FencingToken: fencing, Version: version}, nil
}

// LeaseRoute mirrors lease_multiply_route_state.
func (s *Store) LeaseRoute(ctx context.Context, routeKey, owner string, expiresAt time.Time) (*Lease, error) {
	if err := validateLeaseInput(owner, expiresAt); err != nil {
		return nil, err
	}
	if strings.TrimSpace(routeKey) == "" {
		return nil, errors.New("route key is empty")
	}
	var version, fencing int64
	err := s.pool.QueryRow(ctx,
		"UPDATE loyal_yield.multiply_route_states SET lease_owner=$2, lease_expires_at=$3, fencing_token=fencing_token+1 WHERE route_key=$1 AND (lease_owner IS NULL OR lease_expires_at<=now()) RETURNING state_version, fencing_token",
		routeKey, owner, expiresAt).Scan(&version, &fencing)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Lease{RouteKey: routeKey, Owner: owner, ExpiresAt: expiresAt,
		FencingToken: fencing, Version: version}, nil
}

// RenewLease mirrors renew_multiply_route_lease.
func (s *Store) RenewLease(ctx context.Context, lease *Lease, expiresAt time.Time) (bool, error) {
	if err := validateLeaseInput(lease.Owner, expiresAt); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE loyal_yield.multiply_route_states SET lease_expires_at=$5 WHERE route_key=$1 AND state_version=$2 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>now()",
		lease.RouteKey, lease.Version, lease.Owner, lease.FencingToken, expiresAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		lease.ExpiresAt = expiresAt
	}
	return tag.RowsAffected() == 1, nil
}

// SaveRouteState mirrors save_multiply_route_state.
func (s *Store) SaveRouteState(ctx context.Context, lease *Lease, state *RouteState) (bool, error) {
	version, ok, err := s.updateRoute(ctx, lease, state)
	if err != nil || !ok {
		return false, err
	}
	lease.Version = version
	return true, nil
}

// PrepareOperation mirrors prepare_multiply_operation: insert the prepared
// operation and advance the route in one transaction.
func (s *Store) PrepareOperation(ctx context.Context, lease *Lease, route *RouteState, operation *MultiplyOperation) (bool, error) {
	if err := validateNextRoute(lease, route); err != nil {
		return false, err
	}
	if err := operation.Validate(); err != nil {
		return false, fmt.Errorf("prepared operation is invalid: %w", err)
	}
	if operation.Status != StatusPrepared || operation.RouteKey != lease.RouteKey ||
		route.CurrentOperationID == nil || *route.CurrentOperationID != operation.OperationID ||
		operation.Cycle != route.Cycle {
		return false, errors.New("prepared operation is not bound to its route")
	}
	encoded, err := jsonMarshal(route)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := checkFreshCustodyOwnership(ctx, tx, route); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx,
		"INSERT INTO loyal_yield.multiply_operations (operation_id, route_key, cycle, engine_version, action, strategy_key, status, idempotency_key, expected_effects, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,'prepared',$7,$8,$9,$9) ON CONFLICT (idempotency_key) DO NOTHING",
		operation.OperationID, operation.RouteKey, int64(operation.Cycle), EngineVersion,
		string(operation.Action), strategyKeyText(operation.StrategyKey), operation.IDempotencyKey,
		operation.ExpectedEffects, operation.CreatedAt)
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
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	lease.Version = version
	return true, nil
}

// PersistSignedOperation mirrors persist_signed_operation: the exact signed
// wire is accepted only by a lease-guarded update from 'prepared'. The wire
// cannot be replaced afterwards.
func (s *Store) PersistSignedOperation(ctx context.Context, lease *Lease, operationID, policyAccount, policyDataSHA256, messageSHA256 string, signed *SignedOperation) (bool, error) {
	if lease == nil || signed == nil || signed.LastValidBlockHeight <= 0 {
		return false, errors.New("signed operation or lease is missing")
	}
	if _, err := PersistedTransaction(&MultiplyOperation{SignedWire: signed.Wire, SignedWireSHA256: &signed.WireSHA256, TransactionSignature: &signed.TransactionSignature, RecentBlockhash: &signed.RecentBlockhash, MessageSHA256: &messageSHA256}); err != nil {
		return false, err
	}
	if err := validateHash(policyDataSHA256); err != nil {
		return false, err
	}
	if err := validateHash(messageSHA256); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `
        UPDATE loyal_yield.multiply_operations operation
        SET status='signed_persisted', policy_account=$6, policy_data_sha256=$7, message_sha256=$8,
            signed_wire=$9, signed_wire_sha256=$10, transaction_signature=$11,
            recent_blockhash=$12, last_valid_block_height=$13, updated_at=now()
        FROM loyal_yield.multiply_route_states route
        WHERE operation.operation_id=$1 AND operation.route_key=$2 AND operation.status='prepared'
          AND route.route_key=operation.route_key AND route.state_version=$3
          AND route.lease_owner=$4 AND route.fencing_token=$5 AND route.lease_expires_at>now()`,
		operationID, lease.RouteKey, lease.Version, lease.Owner, lease.FencingToken,
		policyAccount, policyDataSHA256, messageSHA256,
		signed.Wire, signed.WireSHA256, signed.TransactionSignature,
		signed.RecentBlockhash, signed.LastValidBlockHeight)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkBroadcastIntent mirrors mark_multiply_broadcast_intent.
func (s *Store) MarkBroadcastIntent(ctx context.Context, lease *Lease, operationID string, at time.Time) (bool, error) {
	return s.advanceOperation(ctx, lease, operationID, "signed_persisted", "broadcast_intent", &at, nil)
}

// MarkConfirmed mirrors mark_multiply_confirmed.
func (s *Store) MarkConfirmed(ctx context.Context, lease *Lease, operationID string, confirmedSlot uint64) (bool, error) {
	slot, err := int64FromU64(confirmedSlot, "slot")
	if err != nil {
		return false, err
	}
	return s.advanceOperation(ctx, lease, operationID, "broadcast_intent", "confirmed", nil, &slot)
}

func (s *Store) advanceOperation(ctx context.Context, lease *Lease, operationID, from, to string, broadcastAt *time.Time, confirmedSlot *int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
        UPDATE loyal_yield.multiply_operations operation
        SET status=$7, broadcast_intent_at=COALESCE($8, operation.broadcast_intent_at),
            confirmed_slot=COALESCE($9, operation.confirmed_slot), updated_at=now()
        FROM loyal_yield.multiply_route_states route
        WHERE operation.operation_id=$1 AND operation.route_key=$2 AND operation.status=$6
          AND route.route_key=operation.route_key AND route.state_version=$3
          AND route.lease_owner=$4 AND route.fencing_token=$5 AND route.lease_expires_at>now()`,
		operationID, lease.RouteKey, lease.Version, lease.Owner, lease.FencingToken,
		from, to, broadcastAt, confirmedSlot)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CancelPreparedOperation mirrors cancel_prepared_multiply_operation.
func (s *Store) CancelPreparedOperation(ctx context.Context, lease *Lease, operationID string, route *RouteState) (bool, error) {
	if err := validateNextRoute(lease, route); err != nil {
		return false, err
	}
	if route.CurrentOperationID != nil {
		return false, errors.New("cancelled route must clear current operation")
	}
	encoded, err := jsonMarshal(route)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var deletedID string
	err = tx.QueryRow(ctx,
		"DELETE FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND route_key=$2 AND status='prepared' RETURNING operation_id",
		operationID, lease.RouteKey).Scan(&deletedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	version, ok, err := updateRouteInTx(ctx, tx, lease, encoded)
	if err != nil || !ok {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	lease.Version = version
	return true, nil
}

// ExpireOperation mirrors expire_multiply_operation: a signature absent once
// the finalized height passed its blockhash can never land.
func (s *Store) ExpireOperation(ctx context.Context, lease *Lease, operationID string, route *RouteState) (bool, error) {
	if err := validateNextRoute(lease, route); err != nil {
		return false, err
	}
	if route.CurrentOperationID != nil {
		return false, errors.New("expired route must clear current operation")
	}
	encoded, err := jsonMarshal(route)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var expiredID string
	err = tx.QueryRow(ctx,
		"UPDATE loyal_yield.multiply_operations SET status='expired', signed_wire=NULL, updated_at=now() WHERE operation_id=$1 AND route_key=$2 AND status IN ('signed_persisted','broadcast_intent') AND last_valid_block_height IS NOT NULL RETURNING operation_id",
		operationID, lease.RouteKey).Scan(&expiredID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	version, ok, err := updateRouteInTx(ctx, tx, lease, encoded)
	if err != nil || !ok {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	lease.Version = version
	return true, nil
}

// MarkManualRecovery mirrors mark_multiply_manual_recovery.
func (s *Store) MarkManualRecovery(ctx context.Context, lease *Lease, operationID string, route *RouteState) (bool, error) {
	if err := validateNextRoute(lease, route); err != nil {
		return false, err
	}
	if route.CurrentOperationID != nil || route.Goal != GoalManualRecovery {
		return false, errors.New("manual recovery route is not terminal")
	}
	encoded, err := jsonMarshal(route)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var movedID string
	err = tx.QueryRow(ctx,
		"UPDATE loyal_yield.multiply_operations SET status='manual_recovery', updated_at=now() WHERE operation_id=$1 AND route_key=$2 AND status IN ('signed_persisted','broadcast_intent','confirmed','reconciliation_pending') RETURNING operation_id",
		operationID, lease.RouteKey).Scan(&movedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	version, ok, err := updateRouteInTx(ctx, tx, lease, encoded)
	if err != nil || !ok {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	lease.Version = version
	return true, nil
}

// ReconcileOperation requires exact transaction evidence and records it on the
// operation's reconciled_effects in the same fenced transaction as terminal
// operation/route publication. Rust never reads that column for Multiply rows.
func (s *Store) ReconcileOperation(ctx context.Context, lease *Lease, operationID, transactionSignature, reconciliationSHA256 string, confirmedSlot uint64, route *RouteState, proofs ...*ReconciledReceiptProof) (bool, error) {
	if len(proofs) != 1 || proofs[0] == nil {
		return false, errors.New("reconciliation requires an exact transaction receipt")
	}
	if err := validateHash(reconciliationSHA256); err != nil {
		return false, err
	}
	if err := validateNextRoute(lease, route); err != nil {
		return false, err
	}
	if route.CurrentOperationID != nil {
		return false, errors.New("reconciled route must clear current operation")
	}
	encoded, err := jsonMarshal(route)
	if err != nil {
		return false, err
	}
	slot, err := int64FromU64(confirmedSlot, "slot")
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	op, err := decodeOperation(tx.QueryRow(ctx, "SELECT "+operationColumns+" FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND route_key=$2 FOR UPDATE", operationID, lease.RouteKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if op.Status != StatusConfirmed && op.Status != StatusReconciliationPending {
		return false, nil
	}
	topology, err := TopologyForRoute(route)
	if err != nil {
		return false, err
	}
	e := proofs[0].evidence
	validated, err := validateConfirmedReceipt(op, topology, e.Transaction, e.LookupTables)
	if err != nil {
		return false, err
	}
	v := validated.evidence
	if e.OperationID != op.OperationID || e.Signature != transactionSignature || e.Signature != v.Signature || e.WireSHA256 != v.WireSHA256 || e.FinancialAnchorsSHA256 != v.FinancialAnchorsSHA256 || e.ConfirmedSlot != confirmedSlot || e.ConfirmedSlot != v.ConfirmedSlot || e.ObservationSlot < e.ConfirmedSlot || e.ObservationSlot != route.ObservedSlot || e.ObservationSlot > math.MaxInt64 {
		return false, errors.New("receipt evidence identity or observation slot drifted")
	}
	var reconciledID string
	err = tx.QueryRow(ctx,
		"UPDATE loyal_yield.multiply_operations SET status='reconciled', signed_wire=NULL, confirmed_slot=$3, reconciliation_sha256=$4, reconciled_effects=$6, updated_at=now() WHERE operation_id=$1 AND route_key=$2 AND status IN ('confirmed','reconciliation_pending') AND transaction_signature=$5 RETURNING operation_id",
		operationID, lease.RouteKey, slot, reconciliationSHA256, transactionSignature, e).Scan(&reconciledID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	version, ok, err := updateRouteInTx(ctx, tx, lease, encoded)
	if err != nil || !ok {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	lease.Version = version
	return true, nil
}

// ReleaseLease mirrors release_multiply_route_lease.
func (s *Store) ReleaseLease(ctx context.Context, lease *Lease) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		"UPDATE loyal_yield.multiply_route_states SET lease_owner=NULL, lease_expires_at=NULL WHERE route_key=$1 AND state_version=$2 AND lease_owner=$3 AND fencing_token=$4",
		lease.RouteKey, lease.Version, lease.Owner, lease.FencingToken)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) updateRoute(ctx context.Context, lease *Lease, state *RouteState) (int64, bool, error) {
	encoded, err := jsonMarshal(state)
	if err != nil {
		return 0, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	version, ok, err := updateRouteInTx(ctx, tx, lease, encoded)
	if err != nil || !ok {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return version, true, nil
}

func updateRouteInTx(ctx context.Context, tx pgx.Tx, lease *Lease, encoded []byte) (int64, bool, error) {
	var version int64
	err := tx.QueryRow(ctx,
		"UPDATE loyal_yield.multiply_route_states SET state=$5, state_version=state_version+1, updated_at=now() WHERE route_key=$1 AND state_version=$2 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>now() RETURNING state_version",
		lease.RouteKey, lease.Version, lease.Owner, lease.FencingToken, encoded).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return version, true, nil
}

func validateRouteState(routeKey string, state *RouteState) error {
	if err := state.ValidatePersisted(); err != nil {
		return err
	}
	if routeKey != state.RouteKey {
		return errors.New("route key does not match typed state")
	}
	return nil
}

func validateNextRoute(lease *Lease, state *RouteState) error {
	if err := validateRouteState(lease.RouteKey, state); err != nil {
		return err
	}
	if state.Generation != uint64(lease.Version)+1 {
		return errors.New("route generation must advance exactly once")
	}
	return nil
}

func validateLeaseInput(owner string, expiresAt time.Time) error {
	if strings.TrimSpace(owner) == "" || !expiresAt.After(time.Now()) {
		return errors.New("lease owner or expiry is invalid")
	}
	return nil
}

func validateHash(value string) error {
	if len(value) != 64 {
		return errors.New("expected a 64-character hexadecimal hash")
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F') {
			return errors.New("expected a 64-character hexadecimal hash")
		}
	}
	return nil
}

func int64FromU64(value uint64, field string) (int64, error) {
	if value > 1<<63-1 {
		return 0, fmt.Errorf("%s exceeds PostgreSQL BIGINT", field)
	}
	return int64(value), nil
}

func strategyKeyText(key StrategyKey) *string {
	if !key.Valid() {
		return nil
	}
	value := string(key)
	return &value
}

func decodeOperation(rows interface{ Scan(...any) error }) (*MultiplyOperation, error) {
	var (
		operationID, routeKey, engineVersion, action, status, idempotencyKey string
		cycle                                                                int64
		rawEffects                                                           []byte
	)
	var (
		strategyKey                         *string
		policyAccount, policyDataSHA256     *string
		messageSHA256, signedWireSHA256     *string
		signature, recentBlockhash          *string
		reconciliationSHA256                *string
		rawWire                             []byte
		sourceInstructionIndex              *int32
		lastValidBlockHeight, confirmedSlot *int64
		broadcastIntentAt                   *time.Time
		createdAt, updatedAt                time.Time
	)
	err := rows.Scan(&operationID, &routeKey, &cycle, &engineVersion, &action, &strategyKey, &status,
		&idempotencyKey, &rawEffects, &policyAccount, &policyDataSHA256, &messageSHA256, &rawWire,
		&signedWireSHA256, &signature, &sourceInstructionIndex, &recentBlockhash,
		&lastValidBlockHeight, &broadcastIntentAt, &confirmedSlot, &reconciliationSHA256,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	if cycle <= 0 {
		return nil, errors.New("operation cycle is invalid")
	}
	operation := &MultiplyOperation{
		OperationID: operationID, RouteKey: routeKey, Cycle: uint64(cycle),
		EngineVersion: engineVersion, Action: MultiplyAction(action), StrategyKey: StrategyKey(""),
		Status:    OperationStatus(status),
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	if strategyKey != nil {
		operation.StrategyKey = StrategyKey(*strategyKey)
		if !operation.StrategyKey.Valid() {
			return nil, errors.New("operation strategy key is invalid")
		}
	}
	if err := jsonUnmarshalStrict(rawEffects, &operation.ExpectedEffects); err != nil {
		return nil, fmt.Errorf("operation expected effects: %w", err)
	}
	operation.IDempotencyKey = idempotencyKey
	operation.PolicyAccount = policyAccount
	operation.PolicyDataSHA256 = policyDataSHA256
	operation.MessageSHA256 = messageSHA256
	operation.SignedWireSHA256 = signedWireSHA256
	operation.TransactionSignature = signature
	operation.RecentBlockhash = recentBlockhash
	operation.ReconciliationSHA256 = reconciliationSHA256
	operation.BroadcastIntentAt = broadcastIntentAt
	if rawWire != nil {
		operation.SignedWire = rawWire
		digest := sha256.Sum256(rawWire)
		if signedWireSHA256 == nil || *signedWireSHA256 != hex.EncodeToString(digest[:]) {
			return nil, errors.New("stored signed wire digest mismatch")
		}
	}
	if sourceInstructionIndex != nil {
		if *sourceInstructionIndex < 0 || *sourceInstructionIndex > 65535 {
			return nil, errors.New("source instruction index is invalid")
		}
		value := uint16(*sourceInstructionIndex)
		operation.SourceInstructionIndex = &value
	}
	if lastValidBlockHeight != nil {
		if *lastValidBlockHeight <= 0 {
			return nil, errors.New("operation expiry is invalid")
		}
		value := uint64(*lastValidBlockHeight)
		operation.LastValidBlockHeight = &value
	}
	if confirmedSlot != nil {
		if *confirmedSlot <= 0 {
			return nil, errors.New("operation slot is invalid")
		}
		value := uint64(*confirmedSlot)
		operation.ConfirmedSlot = &value
	}
	if err := operation.Validate(); err != nil {
		return nil, err
	}
	return operation, nil
}

// checkFreshCustodyOwnership uses the exact vault identity and the legacy
// idle-vault-handoff lock. Active foreign configuration itself blocks admission:
// the configuration owner must disable/drain the old family before activation.
// A Go-only mutex could not stop an unchanged Rust writer from starting later.
func checkFreshCustodyOwnership(ctx context.Context, tx pgx.Tx, route *RouteState) error {
	settings, err := solana.PublicKeyFromBase58(route.Settings)
	if err != nil {
		return err
	}
	topology, err := DeriveEarnMaxTopology(settings, route.PolicySeedBase)
	if err != nil {
		return err
	}
	if route.Vault != topology.Vault.String() || route.VaultIndex != topology.VaultIndex {
		return errors.New("multiply route custody identity does not match derived family vault")
	}
	own := map[string]bool{}
	for _, config := range topology.StrategyCatalog() {
		for _, policy := range []PolicyConfig{config.CollateralPolicy, config.DebtPolicy, config.SwapPolicy} {
			own[policy.Account.String()] = true
		}
	}
	rows, err := tx.Query(ctx, `SELECT vault.id,vault.active,policy.active,policy.policy_account FROM loyal_yield.managed_vaults vault JOIN loyal_yield.route_policies policy ON policy.id=vault.active_policy_id WHERE vault.vault_pubkey=$1 AND vault.vault_index=$2 FOR UPDATE OF vault,policy`, route.Vault, route.VaultIndex)
	if err != nil {
		return err
	}
	type managed struct {
		id                   int64
		active, policyActive bool
		policy               string
	}
	var managedRows []managed
	for rows.Next() {
		var v managed
		if err := rows.Scan(&v.id, &v.active, &v.policyActive, &v.policy); err != nil {
			rows.Close()
			return err
		}
		managedRows = append(managedRows, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Claim every mint's existing advisory namespace in deterministic order.
	mints := map[string]bool{USDCMint: true}
	for _, config := range topology.StrategyCatalog() {
		mints[config.CollateralMint] = true
		mints[config.DebtMint] = true
	}
	ordered := make([]string, 0, len(mints))
	for mint := range mints {
		ordered = append(ordered, mint)
	}
	sort.Strings(ordered)
	for _, v := range managedRows {
		for _, mint := range ordered {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(format('idle-vault-handoff:%s:%s',$1::bigint,$2::text),0::bigint))", v.id, mint); err != nil {
				return err
			}
		}
		if v.active && v.policyActive && !own[v.policy] {
			return errors.New("multiply custody has active foreign routing configuration")
		}
		var queued bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.rebalance_opportunities WHERE vault_id=$1 AND opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created')) OR EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions WHERE vault_id=$1 AND status::text IN ('planned','simulating','ready','submitted','confirming'))`, v.id).Scan(&queued); err != nil {
			return err
		}
		if queued {
			return errors.New("multiply custody is reserved by legacy fleet work")
		}
	}
	// Serialize the observed sweep configuration with the legacy target row.
	// This does not replace the external disable/drain activation contract.
	targets, err := tx.Query(ctx, `SELECT id FROM loyal_yield.balance_sweep_targets WHERE vault_pubkey=$1 AND vault_index=$2 ORDER BY id FOR UPDATE`, route.Vault, route.VaultIndex)
	if err != nil {
		return err
	}
	for targets.Next() {
		var id int64
		if err := targets.Scan(&id); err != nil {
			targets.Close()
			return err
		}
	}
	err = targets.Err()
	targets.Close()
	if err != nil {
		return err
	}
	// Inactive targets may still own previously signed or selected attempts.
	var sweep bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_targets target WHERE target.vault_pubkey=$1 AND target.vault_index=$2 AND ((target.desired_active AND target.chain_status='active') OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_lot_claims claim WHERE claim.target_id=target.id AND claim.status='selected') OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts attempt WHERE attempt.target_id=target.id AND (attempt.attempt_state IN ('prepared','submitted','unknown','ambiguous') OR (attempt.attempt_state='confirmed' AND EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_lot_claims unresolved WHERE unresolved.claim_token=attempt.claim_token AND unresolved.status<>'executed'))))))`, route.Vault, route.VaultIndex).Scan(&sweep); err != nil {
		return err
	}
	if sweep {
		return errors.New("multiply custody is owned by Autodeposit configuration or durable attempt")
	}
	return nil
}
