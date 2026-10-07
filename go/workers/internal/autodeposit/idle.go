package autodeposit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// idleDeferralPrefix is the slot last_error text the TS executor writes and
// the Rust trigger's overdue check reads (LIKE prefix and marker regexes).
const idleDeferralPrefix = "existing idle vault balance must drain before direct autodeposit: "

// The marker's anchored, bounded captures: first-blocked epoch and deferral
// count. Malformed or unrelated history matches neither.
const (
	idleBlockedSincePattern = `^existing idle vault balance must drain before direct autodeposit: [0-9]+ \[idle_blocked_since=([0-9]{1,12}); idle_deferrals=[0-9]{1,7}\]$`
	idleDeferralsPattern    = `^existing idle vault balance must drain before direct autodeposit: [0-9]+ \[idle_blocked_since=[0-9]{1,12}; idle_deferrals=([0-9]{1,7})\]$`
)

// DeferIdleScheduledSlot is the TS executor's deferIdleVaultScheduledSlot,
// statement for statement: idle custody blocks a pull before any claim, so
// the slot stays scheduled or requested, waits PreSendRetryDelay, and carries
// the durable first-blocked time and deferral count a restarted Rust trigger
// reports as overdue. Only unclaimed, unsigned, due work may move; anything
// else keeps its owner. It reports whether the slot was deferred.
func (s *Store) DeferIdleScheduledSlot(ctx context.Context, targetID, scheduledSlotID, vaultBalanceRaw int64) (bool, error) {
	if s == nil || s.pool == nil {
		return false, errors.New("autodeposit store has no database pool")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
SET eligible_after = now() + ($4 * interval '1 second'),
    last_error = $5 || ' [idle_blocked_since=' ||
      COALESCE(substring(slot.last_error FROM $6)::bigint, extract(epoch FROM now())::bigint)::text ||
      '; idle_deferrals=' ||
      LEAST(COALESCE(substring(slot.last_error FROM $7)::bigint, 0) + 1, 1000000)::text || ']',
    updated_at = now()
WHERE slot.id = $1
  AND slot.target_id = $2
  AND slot.token_mint = $3
  AND slot.status IN ('scheduled', 'requested')
  AND slot.claim_token IS NULL
  AND slot.execution_id IS NULL
  AND slot.eligible_after <= now()
  AND EXISTS (
    SELECT 1 FROM loyal_yield.balance_sweep_surplus_lots AS lot
    WHERE lot.scheduled_slot_id = slot.id
      AND lot.target_id = slot.target_id
      AND lot.status = 'open'
      AND lot.remaining_amount_raw > 0
  )
  AND NOT EXISTS (
    SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
    WHERE attempt.scheduled_slot_id = slot.id
      AND attempt.attempt_state IN ('prepared', 'submitted', 'confirmed', 'unknown', 'ambiguous')
  )
RETURNING slot.id`, scheduledSlotID, targetID, USDCMint, int64(PreSendRetryDelay/time.Second),
		fmt.Sprintf("%s%d", idleDeferralPrefix, vaultBalanceRaw), idleBlockedSincePattern, idleDeferralsPattern).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("defer idle autodeposit slot %d: %w", scheduledSlotID, err)
	}
	return true, nil
}
