package autodeposit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// FailStaleRequestedSlots closes requested slots that no worker picked up within
// the legacy selection window. A request that silently stays requested forever
// hides a dead consumer behind an apparently pending ask.
func (s *Store) FailStaleRequestedSlots(ctx context.Context, limit int64) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("autodeposit store has no database pool")
	}
	if limit <= 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
WITH stale_slots AS (
    SELECT slot.id
    FROM loyal_yield.balance_sweep_scheduled_slots AS slot
    WHERE slot.status = 'requested'
      AND COALESCE(slot.requested_at, slot.updated_at)
            < now() - ($1::bigint * interval '1 second')
    ORDER BY COALESCE(slot.requested_at, slot.updated_at) ASC, slot.id ASC
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
SET status = 'failed',
    claim_token = NULL,
    last_error = $3,
    updated_at = now()
FROM stale_slots AS stale
WHERE slot.id = stale.id
  AND slot.status = 'requested'
RETURNING slot.id`, int64(StaleRequestedSlotSeconds), limit, RequestedSlotTimeoutError)
	if err != nil {
		return 0, fmt.Errorf("fail stale autodeposit requested slots: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReleaseStaleSelectedClaims restores lots from selected claims that never
// reached a pull. Restoration is deliberately conservative: a claim is stale
// only when its lot value is still in the wallet, the projection has caught up
// with the wallet's event history, and no pull attempt exists that could have
// moved the funds. Anything else is recovery work, not reclaimable stock.
func (s *Store) ReleaseStaleSelectedClaims(ctx context.Context, staleSelectedClaimSeconds, limit int64) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("autodeposit store has no database pool")
	}
	if staleSelectedClaimSeconds <= 0 || limit <= 0 {
		return 0, nil
	}
	var released int64
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
WITH stale_claims AS (
    SELECT claim.claim_token
    FROM loyal_yield.balance_sweep_lot_claims AS claim
    JOIN loyal_yield.balance_sweep_targets AS target
      ON target.id = claim.target_id
    JOIN loyal_yield.balance_sweep_wallet_balances_current AS balance
      ON balance.target_id = target.id
     AND balance.mint = target.token_mint
    WHERE claim.status = 'selected'
      AND claim.execution_id IS NULL
      AND NOT EXISTS (
          SELECT 1
          FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
          WHERE attempt.claim_token = claim.claim_token
            AND attempt.operation_kind = 'pull'
            AND attempt.attempt_state = ANY($5::text[])
      )
      AND target.token_mint = $3
      AND target.wallet_balance_floor_raw IS NOT NULL
      AND balance.amount_raw - target.wallet_balance_floor_raw >= claim.amount_raw
      AND COALESCE((
          SELECT offset_row.last_event_id
          FROM loyal_yield.projection_offsets AS offset_row
          WHERE offset_row.consumer_name = $4
      ), 0) >= (
          SELECT COALESCE(MAX(event.event_id), 0)
          FROM loyal_yield.balance_sweep_wallet_balance_events AS event
          WHERE event.target_id = claim.target_id
            AND event.mint = target.token_mint
      )
      AND claim.updated_at < now() - ($1::bigint * interval '1 second')
    ORDER BY claim.updated_at ASC, claim.claim_token ASC
    LIMIT $2
    FOR UPDATE SKIP LOCKED
),
matched_items AS (
    SELECT item.lot_id, item.amount_raw
    FROM loyal_yield.balance_sweep_lot_claim_items AS item
    JOIN stale_claims
      ON stale_claims.claim_token = item.claim_token
),
restored_lots AS (
    UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
    SET remaining_amount_raw = LEAST(
            lot.original_amount_raw,
            lot.remaining_amount_raw + item.amount_raw
        ),
        status = 'open',
        eligible_after = now(),
        updated_at = now()
    FROM matched_items AS item
    WHERE lot.id = item.lot_id
    RETURNING lot.scheduled_slot_id
),
released_claims AS (
    UPDATE loyal_yield.balance_sweep_lot_claims AS claim
    SET status = 'released',
        updated_at = now()
    WHERE claim.claim_token IN (SELECT claim_token FROM stale_claims)
      AND EXISTS (SELECT 1 FROM restored_lots)
    RETURNING claim.claim_token
),
failed_slots AS (
    UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
    SET status = 'failed',
        claim_token = NULL,
        last_error = 'stale selected claim released by autodeposit worker',
        updated_at = now()
    WHERE slot.claim_token IN (SELECT claim_token FROM released_claims)
       OR slot.id IN (
          SELECT scheduled_slot_id
          FROM restored_lots
          WHERE scheduled_slot_id IS NOT NULL
       )
    RETURNING slot.id
)
SELECT COALESCE((SELECT COUNT(*) FROM released_claims), 0)::bigint AS released_claim_count`,
			staleSelectedClaimSeconds, limit, USDCMint, ConsumerName, ClaimHoldingPullAttemptStates)
		if err := row.Scan(&released); err != nil {
			return fmt.Errorf("release stale autodeposit claims: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return released, nil
}

// LoadExecutableTargets returns one scan's dispatch list: pull recovery rows
// first, then fresh scheduled slots, prioritized for execution.
//
// Recovery precedes fresh work even for a paused target: a claim whose pull
// already holds wallet custody must be resolved regardless of desired
// enablement, because the funds are already out of the wallet. Fresh slots
// require desired_active AND chain_status='active' — desired enablement alone is
// not observed eligibility.
func (s *Store) LoadExecutableTargets(ctx context.Context, limit int64, hintedSlotIDs []int64) ([]ExecutableTarget, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("autodeposit store has no database pool")
	}
	if limit <= 0 {
		return nil, nil
	}
	recovery, err := s.loadPullRecoveryTargets(ctx, limit)
	if err != nil {
		return nil, err
	}
	remaining := limit - int64(len(recovery))
	var fresh []ExecutableTarget
	if remaining > 0 {
		fresh, err = s.loadFreshTargets(ctx, remaining, hintedSlotIDs)
		if err != nil {
			return nil, err
		}
	}
	combined := make([]ExecutableTarget, 0, len(recovery)+len(fresh))
	combined = append(combined, recovery...)
	combined = append(combined, fresh...)
	if len(hintedSlotIDs) == 0 && len(fresh) == len(combined) {
		return fresh, nil
	}
	return PrioritizeExecutableTargets(combined, hintedSlotIDs, int(limit)), nil
}

func (s *Store) loadPullRecoveryTargets(ctx context.Context, limit int64) ([]ExecutableTarget, error) {
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT ON (attempt.claim_token)
    target.id AS target_id,
    slot.id AS scheduled_slot_id,
    claim.claim_token
FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
JOIN loyal_yield.balance_sweep_lot_claims AS claim
  ON claim.claim_token = attempt.claim_token
 AND claim.status = 'selected'
JOIN loyal_yield.balance_sweep_scheduled_slots AS slot
  ON slot.claim_token = claim.claim_token
 AND slot.target_id = claim.target_id
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = claim.target_id
WHERE attempt.operation_kind = 'pull'
  AND attempt.attempt_state = ANY($3::text[])
  AND target.token_mint = $2
ORDER BY attempt.claim_token, attempt.updated_at ASC, attempt.id ASC
LIMIT $1`, limit, USDCMint, AutomaticPullRecoveryStates)
	if err != nil {
		return nil, fmt.Errorf("load autodeposit pull recovery targets: %w", err)
	}
	defer rows.Close()
	var targets []ExecutableTarget
	for rows.Next() {
		var target ExecutableTarget
		if err := rows.Scan(&target.TargetID, &target.ScheduledSlotID, &target.ClaimToken); err != nil {
			return nil, fmt.Errorf("scan autodeposit pull recovery target: %w", err)
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (s *Store) loadFreshTargets(ctx context.Context, limit int64, hintedSlotIDs []int64) ([]ExecutableTarget, error) {
	rows, err := s.pool.Query(ctx, `
SELECT
    target.id AS target_id,
    slot.id AS scheduled_slot_id
FROM loyal_yield.balance_sweep_scheduled_slots AS slot
JOIN loyal_yield.balance_sweep_targets AS target
  ON target.id = slot.target_id
JOIN loyal_yield.balance_sweep_wallet_balances_current AS balance
  ON balance.target_id = target.id
 AND balance.mint = target.token_mint
WHERE target.desired_active = true
  AND target.chain_status = 'active'
  AND target.token_mint = $2
  AND target.wallet_balance_floor_raw IS NOT NULL
  AND balance.amount_raw > target.wallet_balance_floor_raw
  AND slot.token_mint = target.token_mint
  AND slot.status IN ('scheduled', 'requested')
  AND slot.eligible_after <= now()
  AND EXISTS (
      SELECT 1
      FROM loyal_yield.managed_vaults AS managed
      JOIN loyal_yield.route_policies AS policy
        ON policy.id = managed.active_policy_id
       AND policy.active = true
       AND policy.authority = target.authority
       AND policy.settings = target.settings
       AND policy.vault_index = target.vault_index
       AND policy.vault_pubkey = target.vault_pubkey
       AND 'same_mint_kamino' = ANY(policy.route_modes)
      WHERE managed.active = true
        AND managed.settings = target.settings
        AND managed.vault_index = target.vault_index
        AND managed.vault_pubkey = target.vault_pubkey
  )
  AND EXISTS (
      SELECT 1
      FROM loyal_yield.balance_sweep_surplus_lots AS lot
      JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
        ON event.event_id = lot.source_event_id
      WHERE lot.target_id = target.id
        AND lot.scheduled_slot_id = slot.id
        AND event.mint = target.token_mint
        AND lot.status = 'open'
        AND lot.remaining_amount_raw > 0
  )
  AND NOT EXISTS (
      SELECT 1
      FROM loyal_yield.balance_sweep_lot_claims AS claim
      WHERE claim.target_id = target.id
        AND claim.status = 'selected'
  )
ORDER BY
    CASE WHEN slot.id = ANY($3::bigint[]) THEN 0 ELSE 1 END,
    CASE
        WHEN slot.id = ANY($3::bigint[])
        THEN array_position($3::bigint[], slot.id)
    END ASC NULLS LAST,
    CASE WHEN slot.status = 'requested' THEN 0 ELSE 1 END,
    slot.requested_at DESC NULLS LAST,
    slot.eligible_after ASC,
    balance.updated_at ASC,
    target.id ASC,
    slot.id ASC
LIMIT $1`, limit, USDCMint, hintedSlotIDs)
	if err != nil {
		return nil, fmt.Errorf("load fresh autodeposit targets: %w", err)
	}
	defer rows.Close()
	targets := make([]ExecutableTarget, 0, len(hintedSlotIDs))
	for rows.Next() {
		var target ExecutableTarget
		if err := rows.Scan(&target.TargetID, &target.ScheduledSlotID); err != nil {
			return nil, fmt.Errorf("scan fresh autodeposit target: %w", err)
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}
