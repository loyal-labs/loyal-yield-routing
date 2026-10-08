package autodeposit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

// targetRoutedSQL is fresh work's route predicate over a row aliased target:
// the vault's active managed row points at an active same_mint_kamino policy.
// Without it automatic work has no destination and is never dispatched.
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

// withdrawnSkipReason is the slot last_error for automatic work skipped
// because the user took everything out of yield.
const withdrawnSkipReason = "autodeposit skipped: the user withdrew everything from yield"

// VaultWithdrawn reports whether the user took everything out of yield: the
// vault holds nothing now but held something before, as a reserve row or a
// yield position (active or closed). A vault with no history never held
// anything and takes the default reserve like a first deposit.
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

// skipAutomaticSlots cancels the listed slots that are still automatic,
// unclaimed and unsigned work, and suppresses their open lots, as a closed
// target's work is. A user-requested scheduled slot is an explicit ask to
// deposit again and is left to run. A failed or released slot is ended too,
// so it does not linger as an offer to deposit, but only when it holds the
// unheld evidence schedule repair requires before requeueing one: no claim,
// no execution and no attempt on the slot, and no live, executed or attempted
// claim over its lots. Anything else belongs to its recovery. It returns how
// many slots it canceled.
func skipAutomaticSlots(ctx context.Context, tx pgx.Tx, slotIDs []int64) (int64, error) {
	if len(slotIDs) == 0 {
		return 0, nil
	}
	var skipped int64
	err := tx.QueryRow(ctx, `
WITH slot AS (
    UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
    SET status = 'canceled', last_error = $2, updated_at = now()
    WHERE slot.id = ANY($1::bigint[])
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
SELECT count(*) FROM slot`, slotIDs, withdrawnSkipReason).Scan(&skipped)
	if err != nil {
		return 0, fmt.Errorf("skip automatic autodeposit slots %v: %w", slotIDs, err)
	}
	return skipped, nil
}

// SkipWithdrawnScheduledSlot ends one automatic slot of a user who withdrew
// everything. It reports whether the slot was skipped.
func (s *Store) SkipWithdrawnScheduledSlot(ctx context.Context, scheduledSlotID int64) (bool, error) {
	var skipped int64
	err := WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) (err error) {
		skipped, err = skipAutomaticSlots(ctx, tx, []int64{scheduledSlotID})
		return err
	})
	return skipped == 1, err
}

// SkipUnroutedVaultSlots ends the automatic work of every target on a vault
// whose Earn route is gone. Earn calls it inside the transaction that retires
// the route, so no slot outlives its destination: dispatch requires the route
// and would otherwise never select, skip or close the slot. The targets stay
// active; a later inflow schedules new work and is decided again.
func SkipUnroutedVaultSlots(ctx context.Context, tx pgx.Tx, settings string, vaultIndex int16, vaultPubkey string) (int64, error) {
	var slotIDs []int64
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(array_agg(slot.id ORDER BY slot.id), '{}')
FROM loyal_yield.balance_sweep_targets AS target
JOIN loyal_yield.balance_sweep_scheduled_slots AS slot
  ON slot.target_id = target.id
WHERE target.settings = $1
  AND target.vault_index = $2
  AND target.vault_pubkey = $3
  AND slot.status IN ('scheduled', 'failed', 'released')
  AND NOT `+targetRoutedSQL, settings, vaultIndex, vaultPubkey).Scan(&slotIDs); err != nil {
		return 0, fmt.Errorf("load unrouted autodeposit slots: %w", err)
	}
	return skipAutomaticSlots(ctx, tx, slotIDs)
}
