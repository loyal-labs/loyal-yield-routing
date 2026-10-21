package autodeposit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

const targetRoutedSQL = `EXISTS (
      SELECT 1
      FROM loyal_yield.managed_vaults AS managed
      JOIN loyal_yield.route_policies AS policy
        ON policy.id = managed.active_policy_id
       AND policy.active = true
       AND policy.cluster = 'mainnet-beta'
       AND policy.authority = target.authority
       AND policy.settings = target.settings
       AND policy.vault_index = target.vault_index
       AND policy.vault_pubkey = target.vault_pubkey
       AND 'same_mint_kamino' = ANY(policy.route_modes)
      WHERE managed.active = true
        AND managed.settings = target.settings
        AND managed.vault_index = target.vault_index
        AND managed.vault_pubkey = target.vault_pubkey
  )`

const withdrawnSkipReason = "autodeposit skipped: the user withdrew everything from yield"

func (s *Store) VaultWithdrawn(ctx context.Context, target *TargetExecutionContext) (bool, error) {
	var withdrawn bool
	err := s.pool.QueryRow(ctx, `
WITH vault AS (
    SELECT id FROM loyal_yield.managed_vaults
    WHERE settings = $1 AND vault_index = $2 AND vault_pubkey = $3 AND active
)
SELECT NOT EXISTS (
        SELECT 1 FROM loyal_yield.vault_reserve_positions_current AS position
        WHERE position.vault_id IN (SELECT id FROM vault) AND position.amount_raw > 0)
   AND (EXISTS (
        SELECT 1 FROM loyal_yield.vault_reserve_positions_current AS position
        WHERE position.vault_id IN (SELECT id FROM vault))
     OR EXISTS (
        SELECT 1 FROM loyal_yield.user_yield_positions AS yp
        WHERE yp.settings = $1 AND yp.vault_index = $2 AND yp.wallet_address = $4))`,
		target.Settings, target.VaultIndex, target.VaultPubkey, target.Wallet).Scan(&withdrawn)
	if err != nil {
		return false, fmt.Errorf("load autodeposit withdrawal state for target %d: %w", target.TargetID, err)
	}
	return withdrawn, nil
}

func skipAutomaticSlots(ctx context.Context, tx pgx.Tx, targets []lockedTarget, slotIDs []int64) (int64, error) {
	if len(slotIDs) == 0 {
		return 0, nil
	}
	var skipped int64
	err := tx.QueryRow(ctx, `
WITH slot AS (
    UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
    SET status = 'canceled', last_error = $2, updated_at = now()
    WHERE slot.id = ANY($1::bigint[])
      AND slot.target_id = ANY($3::bigint[])
      AND slot.claim_token IS NULL
      AND slot.execution_id IS NULL
      AND NOT EXISTS (
        SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
        WHERE attempt.scheduled_slot_id = slot.id)
      AND ((slot.status = 'scheduled' AND slot.request_source IS NULL)
        OR (slot.status IN ('failed', 'released') AND NOT EXISTS (
            SELECT 1
            FROM loyal_yield.balance_sweep_surplus_lots AS held
            JOIN loyal_yield.balance_sweep_lot_claim_items AS item ON item.lot_id = held.id
            JOIN loyal_yield.balance_sweep_lot_claims AS claim ON claim.claim_token = item.claim_token
            WHERE held.scheduled_slot_id = slot.id
              AND (claim.status = 'selected'
                OR claim.execution_id IS NOT NULL
                OR EXISTS (
                    SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
                    WHERE attempt.claim_token = claim.claim_token)))))
    RETURNING slot.id
), lots AS (
    UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
    SET status = 'suppressed', updated_at = now()
    WHERE lot.scheduled_slot_id IN (SELECT id FROM slot)
      AND lot.status = 'open'
      AND lot.remaining_amount_raw > 0
    RETURNING lot.id
)
SELECT count(*) FROM slot`, slotIDs, withdrawnSkipReason, heldTargetIDs(targets)).Scan(&skipped)
	if err != nil {
		return 0, fmt.Errorf("skip automatic autodeposit slots %v: %w", slotIDs, err)
	}
	return skipped, nil
}

func (s *Store) SkipWithdrawnScheduledSlot(ctx context.Context, scheduledSlotID int64) (bool, error) {
	var skipped int64
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var targetID int64
		err := tx.QueryRow(ctx, `SELECT target_id FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = $1`, scheduledSlotID).Scan(&targetID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load target of autodeposit slot %d: %w", scheduledSlotID, err)
		}
		target, err := lockTarget(ctx, tx, targetID)
		if err != nil {
			return err
		}
		skipped, err = skipAutomaticSlots(ctx, tx, []lockedTarget{target}, []int64{scheduledSlotID})
		return err
	})
	return skipped == 1, err
}

// SkipUnroutedVaultSlots ends the automatic work of the held targets of a
// vault whose Earn route is gone. Earn calls it inside the transaction that retires
// the route, so no slot outlives its destination: dispatch requires the route
// and would otherwise never select, skip or close the slot. The targets stay
// active; a later inflow schedules new work and is decided again.
func SkipUnroutedVaultSlots(ctx context.Context, tx pgx.Tx, targets VaultTargets) (int64, error) {
	var slotIDs []int64
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(array_agg(slot.id ORDER BY slot.id), '{}')
FROM loyal_yield.balance_sweep_targets AS target
JOIN loyal_yield.balance_sweep_scheduled_slots AS slot
  ON slot.target_id = target.id
WHERE target.id = ANY($1::bigint[])
  AND slot.status IN ('scheduled', 'failed', 'released')
  AND NOT `+targetRoutedSQL, heldTargetIDs(targets.held)).Scan(&slotIDs); err != nil {
		return 0, fmt.Errorf("load unrouted autodeposit slots: %w", err)
	}
	return skipAutomaticSlots(ctx, tx, targets.held, slotIDs)
}

// SuppressWithdrawalLots retracts surplus scheduled from the vault's own Earn
// withdrawal when the wallet event was projected before Earn recorded the
// withdrawal; once the withdrawal is known the projection refuses that inflow
// itself. A slot left without lots is reused by the next inflow, as any
// scheduled slot is.
func SuppressWithdrawalLots(ctx context.Context, tx pgx.Tx, signature string, targets VaultTargets) error {
	if _, err := tx.Exec(ctx, `
UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
SET status = 'suppressed', updated_at = now()
WHERE lot.target_id = ANY($2::bigint[])
  AND lot.source_signature = $1
  AND lot.status = 'open'`, signature, heldTargetIDs(targets.held)); err != nil {
		return fmt.Errorf("suppress autodeposit lots of Earn withdrawal %s: %w", signature, err)
	}
	return nil
}

// VaultTargets is the set of a vault's targets this transaction holds FOR UPDATE; only LockVaultTargets makes one.
type VaultTargets struct{ held []lockedTarget }

// LockVaultTargets takes the vault's targets in id order and returns the held set; every writer of the vault's managed_vaults or route_policies rows calls it first.
func LockVaultTargets(ctx context.Context, tx pgx.Tx, settings string, vaultIndex int16, vaultPubkey string) (VaultTargets, error) {
	rows, err := tx.Query(ctx, `
SELECT id FROM loyal_yield.balance_sweep_targets
WHERE settings = $1 AND vault_index = $2 AND vault_pubkey = $3
ORDER BY id FOR UPDATE`, settings, vaultIndex, vaultPubkey)
	if err != nil {
		return VaultTargets{}, fmt.Errorf("lock autodeposit targets of vault %s: %w", vaultPubkey, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return VaultTargets{}, fmt.Errorf("lock autodeposit targets of vault %s: %w", vaultPubkey, err)
	}
	held := make([]lockedTarget, len(ids))
	for i, id := range ids {
		held[i] = lockedTarget{id: id}
	}
	return VaultTargets{held: held}, nil
}
