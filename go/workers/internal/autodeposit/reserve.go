package autodeposit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

const (
	defaultEarnReserve = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"
)

var ErrUnresolvedCurrentReserve = errors.New("Autodeposit target reserve could not be resolved against chain truth")

type LiveVaultPosition struct {
	Reserve       string
	Market        string
	LiquidityMint string
	AmountRaw     int64
}

func resolveDepositReserve(tokenMint string, positions []LiveVaultPosition) (*LiveVaultPosition, error) {
	var best *LiveVaultPosition
	live := false
	for i := range positions {
		position := &positions[i]
		live = live || position.AmountRaw > 0
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
		return refuse("missing_market")
	case best != nil:
		return best, nil
	case live:
		return refuse("liquidity_mint_mismatch")
	case tokenMint != USDCMint:
		return refuse("default_reserve_mint_mismatch")
	}
	return &LiveVaultPosition{Reserve: defaultEarnReserve, Market: fleet.KaminoMainMarket, LiquidityMint: USDCMint}, nil
}

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
