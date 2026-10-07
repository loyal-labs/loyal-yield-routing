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
// redirect to the one live reserve, or refuse with a reason.
type reserveResolution struct {
	Reconciled   *LiveVaultPosition
	Reason       string
	LiveReserves []string
}

// resolveCurrentReserve is the TS executor's resolveCurrentReserve. The
// stored pointer is not authority: a fleet rebalance moves every lot to
// another reserve without updating it, and depositing into the stale reserve
// recreates a position the fleet then drains again (ASK-2051). Redirecting
// moves user funds, so it overrides only on a fresh observation of exactly one
// live position in the target's mint; anything else refuses the pull.
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
		// everything; no row at all is a silent projector.
		for _, position := range positions {
			if position.LiquidityMint == tokenMint && fresh(position.ObservedAt) {
				return refuse("vault_drained")
			}
		}
		return refuse("no_live_position")
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
// lost race refuses: the winner's observation is at least as fresh.
func (s *Store) ResolveDepositReserve(ctx context.Context, target *TargetExecutionContext, now time.Time) error {
	if target.CurrentReserve == nil || *target.CurrentReserve == "" {
		return nil
	}
	positions, err := s.loadLiveVaultPositions(ctx, target)
	if err != nil {
		return err
	}
	resolution, err := resolveCurrentReserve(*target.CurrentReserve, target.TokenMint, positions, now)
	if err != nil || resolution.Reconciled == nil {
		return err
	}
	to := resolution.Reconciled
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
		target.Settings, target.VaultIndex, target.VaultPubkey, target.Wallet, *target.CurrentReserve,
		to.Reserve, to.Market, to.LiquidityMint, to.AmountRaw, to.ObservedSlot, to.ObservedAt)
	if err != nil {
		return fmt.Errorf("persist reconciled autodeposit reserve: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w; refusing to pull. resolution={\"reason\":\"lost_reconciliation_race\",\"currentReserve\":%q,\"reconciledTo\":%q,\"persistedPositions\":%d}",
			ErrUnresolvedCurrentReserve, *target.CurrentReserve, to.Reserve, tag.RowsAffected())
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
