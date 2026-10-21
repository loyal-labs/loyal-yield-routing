package backyard

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

func budgetTestPrice(mint, program string, decimals uint8, token, usdc uint64) BudgetPrice {
	p := BudgetPrice{Mint: mint, TokenProgram: program, Decimals: decimals, ObservedSlot: 42, ValidThroughSlot: 74, EvidenceSHA256: strings.Repeat("a", 64)}
	binary.LittleEndian.PutUint64(p.TokenUpperSF[:8], token)
	binary.LittleEndian.PutUint64(p.USDCLowerSF[:8], usdc)
	return p
}

func TestBudgetPriceRejectsStaleMismatchedAndOverflowInputs(t *testing.T) {
	t.Parallel()
	p := budgetTestPrice("asset", classicTokenProgram, 9, 1, 1)
	for _, tc := range []struct {
		mint, program string
		slot          int64
	}{{"other", classicTokenProgram, 42}, {"asset", token2022Program, 42}, {"asset", classicTokenProgram, 75}, {"asset", classicTokenProgram, 41}} {
		_, err := p.valueUpper(1, tc.mint, tc.program, tc.slot)
		assertPreSendHold(t, err, "missing_stale_or_mismatched_usdc_valuation")
	}
	p.Decimals = 0
	_, err := p.valueUpper(math.MaxUint64, "asset", classicTokenProgram, 42)
	assertPreSendHold(t, err, "invalid_usdc_valuation")
}

func TestPriceMarginCeilsAndRejectsU128Overflow(t *testing.T) {
	t.Parallel()
	var price [16]byte
	price[0] = 1
	upper, err := UpperPriceMargin(price, 1)
	if err != nil || upper[0] != 2 {
		t.Fatalf("margin rounded down: %v %v", upper, err)
	}
	for i := range price {
		price[i] = 255
	}
	_, err = UpperPriceMargin(price, 1)
	assertPreSendHold(t, err, "invalid_usdc_valuation")
}
