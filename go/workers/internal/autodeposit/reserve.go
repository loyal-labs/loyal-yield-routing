package autodeposit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// currentReserveProjectionMaxAge is the TS executor's
// CURRENT_RESERVE_PROJECTION_MAX_AGE_SECONDS.
const currentReserveProjectionMaxAge = 900 * time.Second

// The TS executor's defaultEarnTarget (loyal-actions
// getKaminoUsdcEarnTargetForCluster, mainnet): Kamino's main-market USDC reserve.
const (
	defaultEarnMarket  = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	defaultEarnReserve = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"
)

// ErrUnresolvedCurrentReserve is the TS executor's
// UNRESOLVED_CURRENT_RESERVE_MARKER: no destination may be guessed.
var ErrUnresolvedCurrentReserve = errors.New("Autodeposit target reserve could not be resolved against chain truth")

// LiveVaultPosition is one vault_reserve_positions_current row: the vault's
// observed holding in one reserve, zero rows included.
type LiveVaultPosition struct {
	Reserve       string
	Market        string
	LiquidityMint string
	AmountRaw     int64
	ObservedSlot  *int64
	ObservedAt    *time.Time
}

// reserveResolution is the TS CurrentReserveResolution: keep the pointer,
// redirect to the one live reserve, fall back to the default reserve when the
// vault holds nothing, or refuse with a reason.
type reserveResolution struct {
	Reconciled   *LiveVaultPosition
	Default      bool
	Reason       string
	LiveReserves []string
}

// resolveCurrentReserve is the TS executor's resolveCurrentReserve. The
// stored pointer is not authority: a fleet rebalance moves every lot to
// another reserve without updating it, and depositing into the stale reserve
// recreates a position the fleet then drains again (ASK-2051). Redirecting
// moves user funds, so it overrides only on a fresh observation of exactly one
// live position in the target's mint. A vault that holds nothing has no
// position to fragment, so it takes the default reserve, as the TS executor
// did for a target without a pointer; anything else refuses the pull. A user
// who withdrew everything never reaches this on automatic work (see
// VaultWithdrawn); only their explicit request deposits again.
func resolveCurrentReserve(currentReserve, tokenMint string, positions []LiveVaultPosition, now time.Time) (reserveResolution, error) {
	var live []LiveVaultPosition
	var liveReserves []string
	for _, position := range positions {
		if position.AmountRaw > 0 {
			live = append(live, position)
			liveReserves = append(liveReserves, position.Reserve)
		}
	}
	fresh := func(observedAt *time.Time) bool {
		return observedAt != nil && now.Sub(*observedAt) <= currentReserveProjectionMaxAge
	}
	refuse := func(reason string) (reserveResolution, error) {
		detail, _ := json.Marshal(map[string]any{"reason": reason, "currentReserve": currentReserve, "liveReserves": liveReserves})
		return reserveResolution{Reason: reason, LiveReserves: liveReserves},
			fmt.Errorf("%w; refusing to pull. resolution=%s", ErrUnresolvedCurrentReserve, detail)
	}
	for _, position := range live {
		if position.Reserve == currentReserve {
			if position.LiquidityMint != tokenMint {
				return refuse("liquidity_mint_mismatch")
			}
			// Keeping the pointer is no redirect, so a lagging projection must
			// not turn a healthy target into a failure.
			return reserveResolution{Reason: "current_reserve_is_live", LiveReserves: liveReserves}, nil
		}
	}
	if len(live) == 0 {
		// A fresh zero row in the target's own mint proves the user withdrew
		// everything; no row at all is a vault never observed holding.
		reason := "no_live_position"
		for _, position := range positions {
			if position.LiquidityMint == tokenMint && fresh(position.ObservedAt) {
				reason = "vault_drained"
			}
		}
		return reserveResolution{Default: true, Reason: reason}, nil
	}
	if len(live) > 1 {
		return refuse("multiple_live_positions")
	}
	only := live[0]
	if only.LiquidityMint != tokenMint {
		return refuse("liquidity_mint_mismatch")
	}
	if !fresh(only.ObservedAt) {
		return refuse("stale_projection")
	}
	if only.Market == "" {
		// The TS loader threw on a row without its market.
		return refuse("missing_market")
	}
	return reserveResolution{Reconciled: &only, Reason: "reconciled", LiveReserves: liveReserves}, nil
}

// ResolveDepositReserve points a fresh claim's destination at the vault's
// observed holding before any funds move, and writes the corrected pointer
// back with a compare-and-set on the stale value, as the TS executor did. A
// lost race refuses: the winner's observation is at least as fresh. The
// default destination is not written back; once the deposit lands the vault
// holds it and the next claim reconciles to it.
func (s *Store) ResolveDepositReserve(ctx context.Context, target *TargetExecutionContext, now time.Time) error {
	current := derefString(target.CurrentReserve)
	positions, err := s.loadLiveVaultPositions(ctx, target)
	if err != nil {
		return err
	}
	resolution, err := resolveCurrentReserve(current, target.TokenMint, positions, now)
	if err != nil {
		return err
	}
	if resolution.Default {
		if target.TokenMint != USDCMint {
			return fmt.Errorf("%w; refusing to pull. resolution={\"reason\":\"default_reserve_mint_mismatch\",\"tokenMint\":%q}",
				ErrUnresolvedCurrentReserve, target.TokenMint)
		}
		reserve, market, mint := defaultEarnReserve, defaultEarnMarket, USDCMint
		target.CurrentReserve, target.CurrentMarket, target.CurrentLiquidityMint = &reserve, &market, &mint
		return nil
	}
	to := resolution.Reconciled
	if to == nil {
		return nil
	}
	if current == "" {
		// No pointer to correct: the vault's one live holding is the destination.
		target.CurrentReserve, target.CurrentMarket, target.CurrentLiquidityMint = &to.Reserve, &to.Market, &to.LiquidityMint
		return nil
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.user_yield_positions AS position
SET current_reserve = $6,
    current_market = $7,
    current_liquidity_mint = $8,
    current_amount_raw = $9,
    current_observed_slot = $10,
    current_observed_at = $11,
    updated_at = now()
WHERE position.settings = $1
  AND position.vault_index = $2
  AND position.vault_pubkey = $3
  AND position.wallet_address = $4
  AND position.status = 'active'
  AND position.current_reserve = $5`,
		target.Settings, target.VaultIndex, target.VaultPubkey, target.Wallet, current,
		to.Reserve, to.Market, to.LiquidityMint, to.AmountRaw, to.ObservedSlot, to.ObservedAt)
	if err != nil {
		return fmt.Errorf("persist reconciled autodeposit reserve: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w; refusing to pull. resolution={\"reason\":\"lost_reconciliation_race\",\"currentReserve\":%q,\"reconciledTo\":%q,\"persistedPositions\":%d}",
			ErrUnresolvedCurrentReserve, current, to.Reserve, tag.RowsAffected())
	}
	target.CurrentReserve, target.CurrentMarket, target.CurrentLiquidityMint = &to.Reserve, &to.Market, &to.LiquidityMint
	return nil
}

// loadLiveVaultPositions scopes on the vault's full identity: a replaced or
// deactivated vault keeps its rows and must neither redirect nor fake
// ambiguity. Zero rows are kept; they carry the drained-vault evidence.
func (s *Store) loadLiveVaultPositions(ctx context.Context, target *TargetExecutionContext) ([]LiveVaultPosition, error) {
	rows, err := s.pool.Query(ctx, `
SELECT position.reserve, COALESCE(position.market, ''), position.liquidity_mint, position.amount_raw,
       position.observed_slot, position.observed_at
FROM loyal_yield.vault_reserve_positions_current AS position
JOIN loyal_yield.managed_vaults AS vault
  ON vault.id = position.vault_id
 AND vault.settings = $1
 AND vault.vault_index = $2
 AND vault.vault_pubkey = $3
 AND vault.active
ORDER BY position.reserve`, target.Settings, target.VaultIndex, target.VaultPubkey)
	if err != nil {
		return nil, fmt.Errorf("load autodeposit live vault positions: %w", err)
	}
	defer rows.Close()
	var positions []LiveVaultPosition
	for rows.Next() {
		var position LiveVaultPosition
		if err := rows.Scan(&position.Reserve, &position.Market, &position.LiquidityMint, &position.AmountRaw,
			&position.ObservedSlot, &position.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan autodeposit live vault position: %w", err)
		}
		positions = append(positions, position)
	}
	return positions, rows.Err()
}

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

// SkipWithdrawnScheduledSlot cancels one automatic, unclaimed, unsigned slot
// and suppresses its lots, as a closed target's work is. A user-requested
// slot is an explicit ask to deposit again and is left to run. It reports
// whether the slot was skipped.
func (s *Store) SkipWithdrawnScheduledSlot(ctx context.Context, targetID, scheduledSlotID int64) (bool, error) {
	var skipped int64
	err := s.pool.QueryRow(ctx, `
WITH slot AS (
    UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
    SET status = 'canceled', last_error = $3, updated_at = now()
    WHERE slot.id = $1
      AND slot.target_id = $2
      AND slot.status = 'scheduled'
      AND slot.request_source IS NULL
      AND slot.claim_token IS NULL
      AND slot.execution_id IS NULL
      AND NOT EXISTS (
        SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
        WHERE attempt.scheduled_slot_id = slot.id)
    RETURNING slot.id
), lots AS (
    UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
    SET status = 'suppressed', updated_at = now()
    WHERE lot.scheduled_slot_id IN (SELECT id FROM slot)
      AND lot.status = 'open'
      AND lot.remaining_amount_raw > 0
    RETURNING lot.id
)
SELECT count(*) FROM slot`, scheduledSlotID, targetID, withdrawnSkipReason).Scan(&skipped)
	if err != nil {
		return false, fmt.Errorf("skip withdrawn autodeposit slot %d: %w", scheduledSlotID, err)
	}
	return skipped == 1, nil
}
