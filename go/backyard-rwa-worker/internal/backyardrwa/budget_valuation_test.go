package backyardrwa

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

func TestTransactionValuationNormalizesUSDCAndNativeFees(t *testing.T) {
	message, err := CompileBridgeMessage(bridgeTestRequest(ReportNAV, 0))
	if err != nil {
		t.Fatal(err)
	}
	debit := ExecutableDebit{Source: "custody", Mint: "nine-decimal-collateral", TokenProgram: classicTokenProgram, Raw: 900_000_000}
	price := budgetTestPrice(debit.Mint, debit.TokenProgram, 9, 1, 2)
	sol := budgetTestPrice(nativeSOLBudgetAsset, "11111111111111111111111111111111", 9, 100, 2)
	fee := MessageFeeObservation{MessageSHA256: sha256Bytes(message), Slot: 42, Lamports: 5000}
	cost, err := ValueTransactionCost(message, debit, fee, 1000, price, sol, 42)
	if err != nil || cost.PrincipalMicros != 450_000 || cost.NetworkFeeMicros != 250 || cost.SetupLamportsMicros != 50 || cost.TotalMicros != 450_300 {
		t.Fatalf("wrong USDC-normalized cost: %+v %v", cost, err)
	}
	// Quoting in USDC is not the same as declaring a one-dollar stable peg.
	price.USDCLowerSF = price.TokenUpperSF
	cost, err = ValueTransactionCost(message, debit, fee, 0, price, sol, 42)
	if err != nil || cost.PrincipalMicros != 900_000 {
		t.Fatalf("USDC price change was ignored: %+v %v", cost, err)
	}
	fee.Slot = 1
	_, err = ValueTransactionCost(message, debit, fee, 0, price, sol, 42)
	assertBudgetHold(t, err, "fee_message_or_slot_mismatch")
}

func TestBudgetPriceRejectsStaleMismatchedAndOverflowInputs(t *testing.T) {
	p := budgetTestPrice("asset", classicTokenProgram, 9, 1, 1)
	for _, tc := range []struct {
		mint, program string
		slot          int64
	}{{"other", classicTokenProgram, 42}, {"asset", token2022Program, 42}, {"asset", classicTokenProgram, 75}, {"asset", classicTokenProgram, 41}} {
		_, err := p.valueUpper(1, tc.mint, tc.program, tc.slot)
		assertBudgetHold(t, err, "missing_stale_or_mismatched_usdc_valuation")
	}
	p.Decimals = 0
	_, err := p.valueUpper(math.MaxUint64, "asset", classicTokenProgram, 42)
	assertBudgetHold(t, err, "invalid_usdc_valuation")
}

func TestPriceMarginCeilsAndRejectsU128Overflow(t *testing.T) {
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
	assertBudgetHold(t, err, "invalid_usdc_valuation")
}
