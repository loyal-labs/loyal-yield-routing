package autodeposit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

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
}

// resolveDepositReserve picks a fresh claim's destination from the vault's
// observed holdings, the observer's one projection of them, with no stored
// pointer: of the reserves in the deposit's mint that hold a position, the
// largest, ties broken by reserve address. A fleet rebalance moves the
// holding, so the deposit follows it and never recreates a position the fleet
// drains again (ASK-2051); a vault split across reserves (85 production
// vaults) deposits where most of it already is. A vault that holds nothing in
// the deposit's mint takes the default reserve, which exists for USDC only. A
// user who withdrew everything never reaches this on automatic work (see
// VaultWithdrawn); only their explicit request deposits again.
func resolveDepositReserve(tokenMint string, positions []LiveVaultPosition) (*LiveVaultPosition, error) {
	var best *LiveVaultPosition
	for i := range positions {
		position := &positions[i]
		if position.AmountRaw <= 0 || position.LiquidityMint != tokenMint {
			continue
		}
		if best == nil || position.AmountRaw > best.AmountRaw || position.AmountRaw == best.AmountRaw && position.Reserve < best.Reserve {
			best = position
		}
	}
	refuse := func(reason string) (*LiveVaultPosition, error) {
		detail, _ := json.Marshal(map[string]any{"reason": reason, "tokenMint": tokenMint})
		return nil, fmt.Errorf("%w; refusing to pull. resolution=%s", ErrUnresolvedCurrentReserve, detail)
	}
	switch {
	case best != nil && best.Market == "":
		// The TS loader threw on a row without its market.
		return refuse("missing_market")
	case best != nil:
		return best, nil
	case tokenMint != USDCMint:
		return refuse("default_reserve_mint_mismatch")
	}
	return &LiveVaultPosition{Reserve: defaultEarnReserve, Market: defaultEarnMarket, LiquidityMint: USDCMint}, nil
}

// ResolveDepositReserve points a fresh claim's destination at the vault's
// observed holding before any funds move.
func (s *Store) ResolveDepositReserve(ctx context.Context, target *TargetExecutionContext) error {
	positions, err := s.loadLiveVaultPositions(ctx, target)
	if err != nil {
		return err
	}
	to, err := resolveDepositReserve(target.TokenMint, positions)
	if err != nil {
		return err
	}
	target.CurrentReserve, target.CurrentMarket, target.CurrentLiquidityMint = &to.Reserve, &to.Market, &to.LiquidityMint
	return nil
}

// loadLiveVaultPositions scopes on the vault's full identity: a replaced or
// deactivated vault keeps its rows and must neither redirect nor fake
// ambiguity.
func (s *Store) loadLiveVaultPositions(ctx context.Context, target *TargetExecutionContext) ([]LiveVaultPosition, error) {
	rows, err := s.pool.Query(ctx, `
SELECT position.reserve, COALESCE(position.market, ''), position.liquidity_mint, position.amount_raw
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
		if err := rows.Scan(&position.Reserve, &position.Market, &position.LiquidityMint, &position.AmountRaw); err != nil {
			return nil, fmt.Errorf("scan autodeposit live vault position: %w", err)
		}
		positions = append(positions, position)
	}
	return positions, rows.Err()
}
