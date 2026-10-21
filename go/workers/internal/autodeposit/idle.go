package autodeposit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const ownedCustodyDeferral = "another family holds open custody on the vault USDC account"

// VaultCustodyHeldElsewhere reports whether funds in the vault USDC account
// may belong to someone other than this claim. A cross-mint movement owns
// them from its decision row on, wherever custody_account points: the row
// names the source reserve until the withdraw leg finalizes, while the funds
// already sit in the account. An abandoned movement (closed_by_user,
// manual_intervention) leaves its custody in its credit account for an
// operator. Another selected Autodeposit claim on the vault owns its pull. A
// non-zero sinceAttemptID also counts any cross-mint decision created after
// that attempt, so a movement that started and finished meanwhile is seen.
func (s *Store) VaultCustodyHeldElsewhere(ctx context.Context, vaultPubkey, vaultUsdcAta, ownClaimToken string, sinceAttemptID int64) (bool, error) {
	var held bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM loyal_yield.rebalance_decisions AS decision
    JOIN loyal_yield.managed_vaults AS vault ON vault.id = decision.vault_id
    WHERE vault.vault_pubkey = $1
      AND decision.movement_route = 'cross_mint_jupiter'
      AND (decision.terminal_outcome IS NULL
        OR (decision.terminal_outcome IN ('closed_by_user', 'manual_intervention') AND decision.custody_account = $2)
        OR decision.created_at >= (
            SELECT attempt.created_at FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
            WHERE attempt.id = $4)))
OR EXISTS (
    SELECT 1 FROM loyal_yield.balance_sweep_lot_claims AS claim
    JOIN loyal_yield.balance_sweep_targets AS target ON target.id = claim.target_id
    WHERE target.vault_pubkey = $1
      AND claim.status = 'selected'
      AND claim.claim_token <> $3)`, vaultPubkey, vaultUsdcAta, ownClaimToken, sinceAttemptID).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("load vault custody owners: %w", err)
	}
	return held, nil
}

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
