package backyardrwa

import (
	"context"
	"fmt"
	"time"
)

// The strategy-two allocation policy caps each execution at
// strategyTwoBridgeLegCapRaw and its embedded spending window at
// strategyTwoDailyAllocationCapRaw. The chain enforces both, but a wire that
// only fails at landing still burned the attempt, so the worker keeps its own
// second guard: the sum of allocation amounts it has sent in the trailing 24
// hours, read from its own durable operation journal, must leave room for the
// next amount.
const (
	allocationDailyLimitReason = "allocation_daily_limit"
	allocationDailyWindow      = 24 * time.Hour
)

// evaluateAllocationDailyLimit is the pure guard. An overflow-safe sum keeps
// a long outage from accidentally passing a huge next amount.
func evaluateAllocationDailyLimit(sentRaw, nextRaw uint64) error {
	if nextRaw > strategyTwoDailyAllocationCapRaw {
		return budgetHold(allocationDailyLimitReason)
	}
	if sentRaw > strategyTwoDailyAllocationCapRaw || nextRaw > strategyTwoDailyAllocationCapRaw-sentRaw {
		return budgetHold(allocationDailyLimitReason)
	}
	return nil
}

// allocationCountedStatuses are the operation states that prove a broadcast
// happened: everything from the broadcast intent onward, including failed
// landings and manual_recovery rows (a refused wire still consumed the attempt
// window, and a parked row was broadcast and was never proven to have failed
// before broadcast). Only pre-broadcast states are excluded.
var allocationCountedStatuses = []string{
	string(BroadcastIntent), string(Submitted), string(Confirmed),
	string(Reconciling), string(Reconciled), string(ManualRecovery), string(Failed),
}

const allocationSentWindowSQL = `SELECT COALESCE(SUM((expected_effects->'decision'->>'amountRaw')::bigint), 0)::bigint
FROM loyal_yield.multiply_operations
WHERE route_key = $1 AND action = 'VOLTR_ALLOCATE_TO_SQUADS'
  AND status = ANY($2) AND COALESCE(broadcast_intent_at, created_at) >= $3`

// AllocationSentRawTrailingWindow sums the allocation amounts this worker's
// journal recorded as sent inside the trailing daily window. The window is
// keyed on the broadcast intent stamp (falling back to the immutable row
// creation time when it is missing) and never on updated_at, which
// reconciliation retries and status refreshes keep moving forward.
func (d *Database) AllocationSentRawTrailingWindow(ctx context.Context, routeKey string) (uint64, error) {
	if d == nil || d.pool == nil || routeKey == "" {
		return 0, fmt.Errorf("database is not configured")
	}
	if err := d.AssertRouteLease(ctx, routeKey); err != nil {
		return 0, err
	}
	var sent int64
	if err := d.pool.QueryRow(ctx, allocationSentWindowSQL,
		routeKey, allocationCountedStatuses, time.Now().Add(-allocationDailyWindow)).Scan(&sent); err != nil {
		return 0, fmt.Errorf("read trailing allocation window: %w", err)
	}
	if sent < 0 {
		return 0, fmt.Errorf("trailing allocation window summed negative")
	}
	return uint64(sent), nil
}
