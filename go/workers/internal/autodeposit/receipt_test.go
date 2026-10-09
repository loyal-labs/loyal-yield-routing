package autodeposit

import (
	"math"
	"testing"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func TestPullReceiptUsesActualTransactionBalances(t *testing.T) {
	plan, _ := testPullPlan()
	attempt := DurableAttempt{OperationKind: OperationPull, State: AttemptConfirmed, AmountRaw: plan.AmountRaw, Signature: "exact", ConfirmedSlot: ptrInt64(100)}
	receipt := ReceiptEvidence{Signature: "exact", Slot: 100, Effects: []ReceiptEffect{
		{TokenAccount: plan.Target.WalletUsdcAta, Mint: USDCMint, PreRaw: 9_000_000, PostRaw: 9_000_000 - plan.AmountRaw},
		{TokenAccount: plan.Target.VaultUsdcAta, Mint: USDCMint, PreRaw: 50, PostRaw: 50 + plan.AmountRaw},
	}}
	if err := validatePullReceipt(plan, attempt, receipt); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ReceiptEvidence){
		func(r *ReceiptEvidence) { r.Slot = 0 },
		func(r *ReceiptEvidence) { r.Slot = 99 },
		func(r *ReceiptEvidence) { r.Signature = "other" },
		func(r *ReceiptEvidence) { r.Effects[0].PostRaw++ },
		func(r *ReceiptEvidence) { r.Effects[1].Mint = "other" },
		func(r *ReceiptEvidence) { r.Effects[1].PreRaw = -1 },
	} {
		bad := receipt
		bad.Effects = append([]ReceiptEffect(nil), receipt.Effects...)
		mutate(&bad)
		if validatePullReceipt(plan, attempt, bad) == nil {
			t.Fatal("contradictory or incomplete pull evidence accepted")
		}
	}
}

func TestReceiptConversionPreservesUnknownAndRejectsOverflow(t *testing.T) {
	wallet, usdc := solana.PublicKey{1}, mustKey(USDCMint)
	balance := func(raw uint64) map[solana.PublicKey]chain.TokenBalance {
		return map[solana.PublicKey]chain.TokenBalance{wallet: {Mint: usdc, Amount: raw}}
	}
	got, err := receiptEvidence("exact", chain.Receipt{Slot: 100, Pre: balance(10), Post: balance(7)})
	if err != nil || got.Slot != 100 || len(got.Effects) != 1 || got.Effects[0].PreRaw != 10 || got.Effects[0].PostRaw != 7 || got.Effects[0].Mint != USDCMint {
		t.Fatalf("exact receipt conversion: %+v %v", got, err)
	}
	got, err = receiptEvidence("exact", chain.Receipt{Slot: 100, Pre: balance(10)})
	if err != nil || len(got.Effects) != 0 {
		t.Fatal("missing evidence became a zero balance")
	}
	for name, bad := range map[string]chain.Receipt{
		"overflow":    {Slot: 100, Pre: balance(10), Post: balance(math.MaxUint64)},
		"mint change": {Slot: 100, Pre: balance(10), Post: map[solana.PublicKey]chain.TokenBalance{wallet: {Mint: solana.PublicKey{2}, Amount: 7}}},
		"chain error": {Slot: 100, Err: map[string]any{"InstructionError": []any{0, "Custom"}}, Pre: balance(10), Post: balance(7)},
	} {
		if _, err := receiptEvidence("exact", bad); err == nil {
			t.Fatalf("%s: invalid receipt evidence accepted", name)
		}
	}
}
