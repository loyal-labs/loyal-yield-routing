package backyard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type ManualRecoveryLatch struct {
	Reason          string
	ObservationID   string
	ObservationSlot int64
}

const latchManualRecoverySQL = `INSERT INTO loyal_yield.backyard_manual_recovery_latches
 (route_key, reason, observation_id, observation_slot) VALUES ($1, $2, $3, $4)
 ON CONFLICT (route_key) DO UPDATE SET
   reason = EXCLUDED.reason,
   observation_id = EXCLUDED.observation_id,
   observation_slot = EXCLUDED.observation_slot,
   latched_at = clock_timestamp(),
   cleared_at = NULL,
   cleared_reason = NULL
 WHERE loyal_yield.backyard_manual_recovery_latches.cleared_at IS NOT NULL`

func validateManualRecoveryLatch(latch ManualRecoveryLatch) error {
	if latch.Reason == "" || latch.ObservationID == "" || latch.ObservationSlot <= 0 {
		return fmt.Errorf("manual recovery latch identity is incomplete")
	}
	return nil
}

func (d *Database) LatchManualRecovery(ctx context.Context, routeKey string, latch ManualRecoveryLatch) error {
	if d == nil || d.pool == nil || routeKey == "" {
		return fmt.Errorf("database is not configured")
	}
	if err := validateManualRecoveryLatch(latch); err != nil {
		return err
	}
	if _, err := d.pool.Exec(ctx, latchManualRecoverySQL, routeKey, latch.Reason, latch.ObservationID, latch.ObservationSlot); err != nil {
		return fmt.Errorf("latch manual recovery: %w", err)
	}
	return nil
}

func (d *Database) ManualRecoveryLatch(ctx context.Context, routeKey string) (ManualRecoveryLatch, bool, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return ManualRecoveryLatch{}, false, fmt.Errorf("database is not configured")
	}
	var latch ManualRecoveryLatch
	err := d.pool.QueryRow(ctx,
		`SELECT reason, observation_id, observation_slot FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key = $1 AND cleared_at IS NULL`,
		routeKey).Scan(&latch.Reason, &latch.ObservationID, &latch.ObservationSlot)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualRecoveryLatch{}, false, nil
	}
	if err != nil {
		return ManualRecoveryLatch{}, false, fmt.Errorf("read manual recovery latch: %w", err)
	}
	return latch, true, nil
}

func ClearManualRecoveryHold(ctx context.Context, databaseURL, routeKey, reason string) (string, error) {
	if routeKey == "" {
		return "", fmt.Errorf("a route key is required to clear a manual recovery hold")
	}
	if reason == "" {
		return "", fmt.Errorf("an explicit non-empty reason is required to clear a manual recovery hold")
	}
	database, err := openOperatorDatabase(ctx, databaseURL)
	if err != nil {
		return "", err
	}
	defer database.Close()
	clearedAt, latched, err := database.clearManualRecoveryLatch(ctx, routeKey, reason)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("cleared %s manual recovery hold for route %s (cleared %s)",
		latched, routeKey, clearedAt.UTC().Format(time.RFC3339)), nil
}

// clearManualRecoveryLatch clears the active latch. The generation step is
// the previous binary's clear counter.
func (d *Database) clearManualRecoveryLatch(ctx context.Context, routeKey, reason string) (time.Time, string, error) {
	var clearedAt time.Time
	var latched string
	err := d.pool.QueryRow(ctx,
		`UPDATE loyal_yield.backyard_manual_recovery_latches SET cleared_at = clock_timestamp(), cleared_reason = $2, generation = generation + 1
		 WHERE route_key = $1 AND cleared_at IS NULL RETURNING cleared_at, reason`, routeKey, reason).Scan(&clearedAt, &latched)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, "", fmt.Errorf("route %s has no active manual recovery latch", routeKey)
	}
	if err != nil {
		return time.Time{}, "", fmt.Errorf("clear manual recovery latch: %w", err)
	}
	return clearedAt, latched, nil
}
