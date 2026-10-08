package autodeposit

import (
	"math"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
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
	pre := backyard.TransactionTokenBalance{Address: "wallet", Mint: USDCMint, Raw: 10}
	post := backyard.TransactionTokenBalance{Address: "wallet", Mint: USDCMint, Raw: 7}
	base := backyard.ConfirmedTransactionEvidence{Signature: "exact", Slot: 100, PreTokenBalances: []backyard.TransactionTokenBalance{pre}, PostTokenBalances: []backyard.TransactionTokenBalance{post}}
	got, err := receiptFromEvidence(base)
	if err != nil || len(got.Effects) != 1 || got.Effects[0].PreRaw != 10 || got.Effects[0].PostRaw != 7 {
		t.Fatalf("exact receipt conversion: %+v %v", got, err)
	}
	missing := base
	missing.PostTokenBalances = nil
	got, err = receiptFromEvidence(missing)
	if err != nil || len(got.Effects) != 0 {
		t.Fatal("missing evidence became a zero balance")
	}
	for _, mutate := range []func(*backyard.ConfirmedTransactionEvidence){
		func(e *backyard.ConfirmedTransactionEvidence) { e.Slot = 0 },
		func(e *backyard.ConfirmedTransactionEvidence) { e.PostTokenBalances[0].Raw = math.MaxUint64 },
		func(e *backyard.ConfirmedTransactionEvidence) { e.PostTokenBalances[0].Mint = "different" },
		func(e *backyard.ConfirmedTransactionEvidence) { e.PreTokenBalances = append(e.PreTokenBalances, pre) },
	} {
		bad := base
		bad.PreTokenBalances = append([]backyard.TransactionTokenBalance(nil), base.PreTokenBalances...)
		bad.PostTokenBalances = append([]backyard.TransactionTokenBalance(nil), base.PostTokenBalances...)
		mutate(&bad)
		if _, err := receiptFromEvidence(bad); err == nil {
			t.Fatal("invalid receipt evidence accepted")
		}
	}
}
