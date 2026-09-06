package fleet

import (
	"math"
	"testing"
)

// These amounts protect the retained Rust executor's external contract:
// deposited collateral = signed withdrawal collateral + one recovery unit.
func TestCrossMintRecoveryAnchorAmounts(t *testing.T) {
	for _, tc := range []struct {
		name                                                 string
		liquidity, collateral, wantLiquidity, wantCollateral int64
		ok                                                   bool
	}{
		{"one-to-one", 1_000_000_000, 1_000_000_000, 999_999_999, 999_999_999, true},
		{"round-down", 7, 3, 4, 2, true},
		{"wide-product", math.MaxInt64, math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64 - 1, true},
		{"only-anchor", 7, 1, 0, 0, false},
		{"rounds-to-zero", 1, 2, 0, 0, false},
		{"missing-collateral", 7, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := VaultPosition{AmountRaw: tc.liquidity, SourceCollateralAmountRaw: tc.collateral, SourceAmountSemantics: amountSemanticsKaminoCollateralDeposited}
			got, ok := crossMintRecoveryAnchoredPosition(original)
			if ok != tc.ok {
				t.Fatalf("eligibility=%v want %v", ok, tc.ok)
			}
			if ok && (got.AmountRaw != tc.wantLiquidity || got.SourceCollateralAmountRaw != tc.wantCollateral || original.SourceCollateralAmountRaw-got.SourceCollateralAmountRaw != 1) {
				t.Fatalf("wrong anchored amounts: %+v", got)
			}
			if original.AmountRaw != tc.liquidity || original.SourceCollateralAmountRaw != tc.collateral {
				t.Fatal("mutated observed position")
			}
		})
	}
}
