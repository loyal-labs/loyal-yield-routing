package autodeposit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const idleDeferralPrefix = "existing idle vault balance must drain before direct autodeposit: "

func (s *Store) DeferScheduledSlot(ctx context.Context, targetID, scheduledSlotID int64, reason string, idle bool) (bool, error) {
	if s == nil || s.pool == nil {
		return false, errors.New("autodeposit store has no database pool")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
SET eligible_after = now() + ($4 * interval '1 second'),
    last_error = $5,
    idle_blocked_since = CASE WHEN $6 THEN COALESCE(slot.idle_blocked_since, now()) ELSE slot.idle_blocked_since END,
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
      AND attempt.attempt_state IN ('prepared', 'submitted', 'confirmed')
  )
RETURNING slot.id`, scheduledSlotID, targetID, USDCMint, int64(PreSendRetryDelay/time.Second), reason, idle).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("defer autodeposit slot %d: %w", scheduledSlotID, err)
	}
	return true, nil
}
