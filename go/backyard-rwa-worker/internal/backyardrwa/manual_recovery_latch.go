package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ManualRecoveryLatch is the durable route-level stop behind every
// HOLD_MANUAL_RECOVERY decision. The journal row records the decision; this row
// is what makes the stop survive restarts and healthy batches. Every tick
// re-reads it before observing and re-records the hold until an operator clears
// it with an explicit reason. Nothing in the worker clears it automatically.
type ManualRecoveryLatch struct {
	Reason          string
	ObservationID   string
	ObservationSlot int64
	Generation      int64
}

var errManualRecoveryLatchGenerationChanged = errors.New("manual recovery latch generation changed")

// The insert keeps the first active latch: an already-latched route preserves
// its original reason, and only a cleared latch may be armed again.
const latchManualRecoverySQL = `INSERT INTO loyal_yield.backyard_manual_recovery_latches
 (route_key, reason, observation_id, observation_slot, generation) VALUES ($1, $2, $3, $4, $5)
 ON CONFLICT (route_key) DO UPDATE SET
   reason = EXCLUDED.reason,
   observation_id = EXCLUDED.observation_id,
   observation_slot = EXCLUDED.observation_slot,
   generation = GREATEST(loyal_yield.backyard_manual_recovery_latches.generation, EXCLUDED.generation),
   latched_at = clock_timestamp(),
   cleared_at = NULL,
   cleared_reason = NULL
 WHERE loyal_yield.backyard_manual_recovery_latches.cleared_at IS NOT NULL`

const manualRecoveryLatchColumns = `reason, observation_id, observation_slot, generation`

// The physical latch is authoritative when present. This query is the
// journal-derived fallback for an old or missing physical row. latched:* rows
// are re-records, never evidence: choose the newest genuine hold first, then
// compare its generation and composite journal ordering key with the latest
// clear.
const manualRecoveryDerivedLatchSQL = `
	WITH manual_recovery_holds AS (
		SELECT operation_id, recovery_reason, expected_effects, confirmed_slot, updated_at,
			COALESCE(
				CASE
					WHEN expected_effects ->> 'latchGeneration' ~ '^[0-9]+$'
					THEN (expected_effects ->> 'latchGeneration')::bigint
				END,
				0
			) AS generation,
			COALESCE(
				NULLIF(recovery_reason, ''),
				NULLIF(expected_effects -> 'decision' ->> 'reason', ''),
				'legacy_manual_recovery'
			) AS reason
		FROM loyal_yield.multiply_operations
		WHERE route_key = $1
		  AND action = 'HOLD_MANUAL_RECOVERY'
		  AND status = 'manual_recovery'
	), latest_genuine_hold AS (
		SELECT operation_id, recovery_reason, expected_effects, confirmed_slot, updated_at, generation, reason
		FROM manual_recovery_holds
		WHERE reason NOT LIKE 'latched:%'
		ORDER BY updated_at DESC, operation_id DESC
		LIMIT 1
	), latest_clear AS (
		SELECT operation_id, expected_effects, updated_at,
			COALESCE(
				CASE
					WHEN expected_effects ->> 'latchGeneration' ~ '^[0-9]+$'
					THEN (expected_effects ->> 'latchGeneration')::bigint
				END,
				0
			) AS generation
		FROM loyal_yield.multiply_operations
		WHERE route_key = $1
		  AND action = 'HOLD_CLEARED'
		  AND status = 'manual_recovery'
		ORDER BY updated_at DESC, operation_id DESC
			LIMIT 1
	)
	SELECT hold.reason,
	       COALESCE(
		       NULLIF(hold.expected_effects -> 'decision' ->> 'observationId', ''),
		       hold.operation_id
	       ),
	       GREATEST(
		       COALESCE(
			       CASE
				       WHEN hold.expected_effects -> 'decision' ->> 'observationSlot' ~ '^[0-9]+$'
				       THEN (hold.expected_effects -> 'decision' ->> 'observationSlot')::bigint
			       END,
			       CASE WHEN hold.confirmed_slot > 0 THEN hold.confirmed_slot END,
			       1
		       ),
		       1
	       ),
	       hold.generation,
	       hold.updated_at
	FROM latest_genuine_hold AS hold
	LEFT JOIN latest_clear AS clear ON TRUE
	WHERE clear.operation_id IS NULL
	   OR hold.generation > clear.generation
	   OR (
		   hold.generation = clear.generation
		   AND (hold.updated_at, hold.operation_id) > (clear.updated_at, clear.operation_id)
	   )`

func validateManualRecoveryLatch(latch ManualRecoveryLatch) error {
	if latch.Reason == "" || latch.ObservationID == "" || latch.ObservationSlot <= 0 || latch.Generation < 0 {
		return fmt.Errorf("manual recovery latch identity is incomplete")
	}
	return nil
}

func manualRecoveryLatchReason(reason string) string {
	return strings.TrimPrefix(reason, "latched:")
}

// LatchManualRecovery persists the stop for the route.
func (d *Database) LatchManualRecovery(ctx context.Context, routeKey string, latch ManualRecoveryLatch) error {
	if d == nil || d.pool == nil || routeKey == "" {
		return fmt.Errorf("database is not configured")
	}
	if err := validateManualRecoveryLatch(latch); err != nil {
		return err
	}
	if _, err := d.pool.Exec(ctx, latchManualRecoverySQL, routeKey, manualRecoveryLatchReason(latch.Reason), latch.ObservationID, latch.ObservationSlot, latch.Generation); err != nil {
		return fmt.Errorf("latch manual recovery: %w", err)
	}
	return nil
}

// ManualRecoveryLatch reports the route's active stop. The journal is a
// derived fallback so a terminal hold still blocks execution if a latch row
// was lost or predates the latch migration.
func (d *Database) ManualRecoveryLatch(ctx context.Context, routeKey string) (ManualRecoveryLatch, bool, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return ManualRecoveryLatch{}, false, fmt.Errorf("database is not configured")
	}
	var latch ManualRecoveryLatch
	err := d.pool.QueryRow(ctx,
		`SELECT `+manualRecoveryLatchColumns+` FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1 AND cleared_at IS NULL`,
		routeKey).Scan(&latch.Reason, &latch.ObservationID, &latch.ObservationSlot, &latch.Generation)
	if err == nil {
		return latch, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) && !manualRecoveryLatchTableMissing(err) {
		return ManualRecoveryLatch{}, false, fmt.Errorf("read manual recovery latch: %w", err)
	}
	err = d.pool.QueryRow(ctx, manualRecoveryDerivedLatchSQL, routeKey).
		Scan(&latch.Reason, &latch.ObservationID, &latch.ObservationSlot, &latch.Generation, new(time.Time))
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualRecoveryLatch{}, false, nil
	}
	if err != nil {
		return ManualRecoveryLatch{}, false, fmt.Errorf("read derived manual recovery latch: %w", err)
	}
	return latch, true, nil
}

func manualRecoveryLatchTableMissing(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// ClearManualRecoveryHold is the operator command that lifts the stop. It
// refuses an empty reason, clears the latch, and journals the action as a
// HOLD_CLEARED row carrying the reason and the clearing timestamp. It is never
// called by the worker loop.
func ClearManualRecoveryHold(ctx context.Context, databaseURL, routeKey, reason string) (string, error) {
	if routeKey == "" {
		return "", fmt.Errorf("a route key is required to clear a manual recovery hold")
	}
	if reason == "" {
		return "", fmt.Errorf("an explicit non-empty reason is required to clear a manual recovery hold")
	}
	database, err := OpenDatabase(ctx, databaseURL)
	if err != nil {
		return "", err
	}
	defer database.Close()
	clearedAt, latched, err := database.clearManualRecoveryLatch(ctx, routeKey, reason)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("cleared %s manual recovery hold for route %s (cleared %s)",
		latched.Reason, routeKey, clearedAt.UTC().Format(time.RFC3339)), nil
}

func (d *Database) clearManualRecoveryLatch(ctx context.Context, routeKey, reason string) (time.Time, ManualRecoveryLatch, error) {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return time.Time{}, ManualRecoveryLatch{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var route string
	if err := tx.QueryRow(ctx,
		`SELECT route_key FROM loyal_yield.multiply_route_states WHERE route_key = $1 FOR UPDATE`, routeKey).
		Scan(&route); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, ManualRecoveryLatch{}, fmt.Errorf("route %s does not exist", routeKey)
		}
		return time.Time{}, ManualRecoveryLatch{}, fmt.Errorf("lock route for manual recovery clear: %w", err)
	}
	var latch ManualRecoveryLatch
	var latchedAt time.Time
	err = tx.QueryRow(ctx,
		`SELECT `+manualRecoveryLatchColumns+`, latched_at FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1 AND cleared_at IS NULL FOR UPDATE`,
		routeKey).Scan(&latch.Reason, &latch.ObservationID, &latch.ObservationSlot, &latch.Generation, &latchedAt)
	physicalLatch := err == nil
	if !physicalLatch && (errors.Is(err, pgx.ErrNoRows) || manualRecoveryLatchTableMissing(err)) {
		err = tx.QueryRow(ctx, manualRecoveryDerivedLatchSQL, routeKey).
			Scan(&latch.Reason, &latch.ObservationID, &latch.ObservationSlot, &latch.Generation, &latchedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, ManualRecoveryLatch{}, fmt.Errorf("route %s has no active manual recovery latch", routeKey)
		}
	}
	if err != nil {
		return time.Time{}, ManualRecoveryLatch{}, fmt.Errorf("lock manual recovery latch: %w", err)
	}
	var clearedAt time.Time
	clearedGeneration := latch.Generation
	if physicalLatch {
		err = tx.QueryRow(ctx,
			`UPDATE loyal_yield.backyard_manual_recovery_latches SET cleared_at = clock_timestamp(), cleared_reason = $2, generation = generation + 1
			 WHERE route_key = $1 AND cleared_at IS NULL RETURNING cleared_at, generation`, routeKey, reason).Scan(&clearedAt, &clearedGeneration)
		if err != nil {
			return time.Time{}, ManualRecoveryLatch{}, fmt.Errorf("clear manual recovery latch: %w", err)
		}
	} else {
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&clearedAt); err != nil {
			return time.Time{}, ManualRecoveryLatch{}, fmt.Errorf("timestamp derived manual recovery clear: %w", err)
		}
		clearedGeneration++
	}
	// HOLD_CLEARED is journaled as a terminal manual_recovery row so it never
	// occupies the nonterminal execution slot, never advances the decision
	// epoch, and never looks like an unresolved capital recovery.
	identity := routeKey + ":hold_cleared:" + clearedAt.UTC().Format(time.RFC3339Nano)
	digest := sha256.Sum256([]byte(identity))
	effects, err := json.Marshal(map[string]any{
		"schema":          "loyal-backyard-rwa-hold-cleared/v1",
		"latchedReason":   latch.Reason,
		"latchedAt":       latchedAt.UTC().Format(time.RFC3339Nano),
		"clearedReason":   reason,
		"clearedAt":       clearedAt.UTC().Format(time.RFC3339Nano),
		"latchGeneration": clearedGeneration,
	})
	if err != nil {
		return time.Time{}, ManualRecoveryLatch{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
		 (operation_id, route_key, cycle, engine_version, action, status, idempotency_key, strategy_key, expected_effects, recovery_reason)
		 VALUES ($1, $2, (SELECT COALESCE((state ->> 'cycle')::bigint, 1) FROM loyal_yield.multiply_route_states WHERE route_key = $2),
		         'backyard_rwa_v1', 'HOLD_CLEARED', 'manual_recovery', $3, $4, $5, $6)`,
		hex.EncodeToString(digest[:]), routeKey, identity, RouteID, string(effects), reason); err != nil {
		return time.Time{}, ManualRecoveryLatch{}, fmt.Errorf("journal hold cleared: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, ManualRecoveryLatch{}, err
	}
	return clearedAt, latch, nil
}
