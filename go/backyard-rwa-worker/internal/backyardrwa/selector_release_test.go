package backyardrwa

import (
	"math/big"
	"testing"
)

func selectorReleaseValues() kaminoReleaseValues {
	var one [16]byte
	putScaledFraction(one[:], new(big.Int).Lsh(big.NewInt(1), 60))
	return kaminoReleaseValues{CollateralRaw: 15_000_000, DebtRaw: 5_000_000,
		CollateralDecimals: 6, DebtDecimals: 6, CollateralPriceSF: one, DebtPriceSF: one}
}

func TestSelectorScalarReleaseRespectsProtocolAndRiskLimits(t *testing.T) {
	base := kaminoPilotReleaseLimits{MaxLTVPct: 80, LiquidationPct: 90, GlobalAllowedBorrowValue: 45_000_000}
	for _, tc := range []struct {
		name string
		set  func(*kaminoPilotReleaseLimits)
		want uint64
		hold string
	}{
		{name: "pilot_ceiling", want: 5_909_090},
		{name: "market_allowance", set: func(l *kaminoPilotReleaseLimits) { l.GlobalAllowedBorrowValue = 6 }, want: 1_249_999},
		{name: "market_at_debt", set: func(l *kaminoPilotReleaseLimits) { l.GlobalAllowedBorrowValue = 5 }, hold: "pilot_release_protocol_allowance_unavailable"},
		{name: "max_ltv_margin", set: func(l *kaminoPilotReleaseLimits) { l.MaxLTVPct = 57 }, want: 5_384_615},
		{name: "hard_stop_margin", set: func(l *kaminoPilotReleaseLimits) { l.MaxLTVPct, l.LiquidationPct = 60, 73 }, want: 5_566_037},
		{name: "no_release_margin", set: func(l *kaminoPilotReleaseLimits) { l.MaxLTVPct = 55 }, hold: "pilot_release_risk_margin_unavailable"},
		{name: "minimum_retention", set: func(l *kaminoPilotReleaseLimits) {
			putScaledFraction(l.MinimumRemainingValueSF[:], new(big.Int).Lsh(big.NewInt(10), 60))
		}, want: 4_999_999},
		{name: "minimum_exceeds_position", set: func(l *kaminoPilotReleaseLimits) {
			putScaledFraction(l.MinimumRemainingValueSF[:], new(big.Int).Lsh(big.NewInt(15), 60))
		}, hold: "pilot_release_minimum_collateral_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := base
			if tc.set != nil {
				tc.set(&limits)
			}
			got, err := pilotRepaymentLiquidityAllowanceForValues(selectorReleaseValues(), limits)
			if tc.hold != "" {
				assertBudgetHold(t, err, tc.hold)
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("allowance %d, want %d: %v", got, tc.want, err)
			}
		})
	}
}

func TestSelectorScalarReleaseUsesTokenScalesAndRejectsInvalidValues(t *testing.T) {
	limits := kaminoPilotReleaseLimits{MaxLTVPct: 80, LiquidationPct: 90, GlobalAllowedBorrowValue: 45_000_000}
	values := selectorReleaseValues()
	// 7.5 collateral tokens at $2 each and six-decimal USDC debt. The same
	// $10 market minimum must retain 5 collateral tokens plus one raw unit.
	values.CollateralRaw, values.CollateralDecimals = 7_500_000_000, 9
	putScaledFraction(values.CollateralPriceSF[:], new(big.Int).Lsh(big.NewInt(2), 60))
	putScaledFraction(limits.MinimumRemainingValueSF[:], new(big.Int).Lsh(big.NewInt(10), 60))
	got, err := pilotRepaymentLiquidityAllowanceForValues(values, limits)
	if err != nil || got != 2_499_999_999 {
		t.Fatal("cross-decimal minimum retention changed", got, err)
	}
	for _, mutate := range []func(*kaminoReleaseValues){
		func(v *kaminoReleaseValues) { v.CollateralRaw = 0 },
		func(v *kaminoReleaseValues) { v.DebtRaw = 0 },
		func(v *kaminoReleaseValues) { v.CollateralDecimals = 19 },
		func(v *kaminoReleaseValues) { v.DebtPriceSF = [16]byte{} },
		func(v *kaminoReleaseValues) { v.DebtRaw = 10_000_000 },
	} {
		invalid := values
		mutate(&invalid)
		if _, err := pilotRepaymentLiquidityAllowanceForValues(invalid, limits); err == nil {
			t.Fatal("invalid or unsafe scalar release accepted", invalid)
		}
	}
}

func TestPilotScalarExtractionPreservesRuntimeReceiptFloors(t *testing.T) {
	route, accounts := pilotReleaseFixture(t, SelectedRouteID)
	values := selectorReleaseValues()
	position := KaminoPosition{CollateralDepositedRaw: 6, RedeemablePrimeRaw: values.CollateralRaw, DebtRaw: values.DebtRaw,
		CollateralDecimals: values.CollateralDecimals, DebtDecimals: values.DebtDecimals,
		CollateralPriceSF: values.CollateralPriceSF, DebtPriceSF: values.DebtPriceSF}
	// With six large receipts, the preexisting receipt-ratio floors permit only
	// two receipts (5 USDC), even though scalar risk room is about 5.9 USDC.
	receipts, legacyRounded, err := withdrawExcessAtLTV(position, 5500)
	if err != nil || receipts != 2 || legacyRounded != 5_000_000 {
		t.Fatal("receipt-ratio floors changed", receipts, legacyRounded, err)
	}
	got, err := pilotRepaymentLiquidityAllowance(accounts, route, position, 90)
	if err != nil || got != legacyRounded {
		t.Fatal("scalar extraction enlarged actual release", got, legacyRounded, err)
	}
}
