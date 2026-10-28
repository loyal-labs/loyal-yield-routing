package webapp

import "testing"

func TestEarnObserverBalanceIncludesReserveAlongsideIdleDust(t *testing.T) {
	const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	reserve := stateVaultReserveRow{
		Reserve:          "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59",
		LiquidityMint:    usdc,
		AmountRaw:        175820678,
		PlanningMetadata: map[string]any{"kind": "earn_laserstream_complete_snapshot", "amount_semantics": "redeemable_liquidity_amount"},
	}
	var reserves []stateVaultReserveRow
	if stateUserFacingReserveMetadata(reserve.PlanningMetadata) {
		reserves = append(reserves, reserve)
	}
	position := stateYieldPositionRow{CurrentLiquidityMint: usdc, CurrentAmountRaw: 175768441, Status: "active"}
	result, err := serializeEarnPosition(position, nil, reserves, []stateVaultIdleRow{{Mint: usdc, AmountRaw: 3}}, LoyalMainnetBeta)
	if err != nil {
		t.Fatal(err)
	}
	if result.CurrentTotalAmountRaw != "175820681" || len(result.Holdings) != 2 {
		t.Fatalf("observer reserve disappeared behind idle dust: total=%s holdings=%d", result.CurrentTotalAmountRaw, len(result.Holdings))
	}
}

func TestEarnReserveAmountSemantics(t *testing.T) {
	for _, key := range []string{"amountSemantics", "amount_semantics"} {
		for _, tc := range []struct {
			semantics string
			want      bool
		}{
			{"redeemable_liquidity_amount", true}, // Observer's current reserve balances.
			{"kamino_redeemable_liquidity", true}, // Legacy reconciliation balances.
			{"kamino_obligation_collateral_deposited_amount", false},
			{"", false},
			{"unknown", false},
		} {
			t.Run(key+"/"+tc.semantics, func(t *testing.T) {
				if got := stateUserFacingReserveMetadata(map[string]any{key: tc.semantics}); got != tc.want {
					t.Fatalf("reserve amount accepted = %v, want %v", got, tc.want)
				}
			})
		}
	}
	if stateUserFacingReserveMetadata(nil) {
		t.Fatal("reserve with unknown amount units must not be included in the balance")
	}
}
