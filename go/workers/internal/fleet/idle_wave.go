package fleet

import (
	"encoding/json"
	"errors"
	"time"
)

func planWaveSource(snapshot MarketSnapshot, vault FleetVault, position VaultPosition, target string, now time.Time) Decision {
	if vault.IdleTokenAccount == "" {
		return Plan(snapshot, position, position.SourceReserve, target)
	}
	if position.SourceReserve != "" || position.SourceCollateralAmountRaw != 0 || len(vault.CrossMintTargets) != 0 {
		position.BlockedReason = "invalid_idle_source"
	}
	return planSourceAt(snapshot, position, "", target, true, now)
}

// This is the retained idle plan shape, not permission to publish it. Keep the
// reserve source and collateral absent; idle balance ownership is a distinct
// handoff from a subscription pull and must never imply another withdrawal.
func canonicalIdleExecutionPlan(v FleetVault, d Decision, targetAPY, targetSlot int64, targetObservedAt time.Time) (json.RawMessage, error) {
	if v.IdleTokenAccount == "" || d.RouteKind != "idle_vault_deposit" || d.SourceReserve != "" || d.SourceAPYBPS != 0 || d.SourceMint != d.TargetMint {
		return nil, errors.New("invalid idle diagnostic plan")
	}
	raw, err := canonicalSameMintExecutionPlan(v.Position, d, 0, targetAPY, targetSlot, targetObservedAt)
	if err != nil {
		return nil, err
	}
	var plan map[string]json.RawMessage
	if err = json.Unmarshal(raw, &plan); err != nil {
		return nil, err
	}
	for key, value := range map[string]any{
		"kind": "idle_vault_deposit", "route_kind": "idle_vault_deposit", "source_kind": "idle_vault_usdc",
		"source_reserve": nil, "route_amount_semantics": "idle_vault_liquidity", "source_amount_semantics": nil,
		"source_collateral_amount_raw": nil, "redeemable_source_liquidity_amount_raw": nil,
		"idle_vault_liquidity_amount_raw": d.AmountRaw, "idle_token_account": v.IdleTokenAccount,
		"estimated_execution_costs": map[string]any{"kind": "idle_vault_deposit", "deposit_usd_micros": d.EstimatedCostUSDMicros},
	} {
		plan[key], err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(plan)
}
