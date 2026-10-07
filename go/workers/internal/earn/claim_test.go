package earn

import (
	"errors"
	"testing"
)

func TestFullClaimPaysTheCustodyBalanceAtBuildTime(t *testing.T) {
	// The original failure: "max" saved a 10 USDC equity snapshot, the unwind
	// realized 9.99 after fees, and a claim of the saved amount could not pay.
	saved, realized := uint64(10_000_000), uint64(9_990_000)
	if payout, err := ClaimPayout(ClaimRequest{Full: true, AmountRaw: saved}, realized); err != nil || payout != realized {
		t.Fatalf("full payout %d %v, want the realized custody", payout, err)
	}
	if err := VerifyClaimTransfer(ClaimRequest{Full: true}, realized, 0, realized); err != nil {
		t.Fatal(err)
	}
	if err := VerifyClaimTransfer(ClaimRequest{Full: true}, realized, 1, realized-1); err == nil {
		t.Fatal("a full claim that left custody behind was accepted")
	}
}

func TestExplicitClaimPaysExactlyItsAmount(t *testing.T) {
	request := ClaimRequest{AmountRaw: 4_000_000}
	if payout, err := ClaimPayout(request, 9_000_000); err != nil || payout != 4_000_000 {
		t.Fatalf("explicit payout %d %v", payout, err)
	}
	if _, err := ClaimPayout(request, 3_999_999); !errors.Is(err, ErrClaimShortfall) {
		t.Fatalf("shortfall became a smaller payout: %v", err)
	}
	if err := VerifyClaimTransfer(request, 9_000_000, 5_000_000, 4_000_000); err != nil {
		t.Fatal(err)
	}
	if err := VerifyClaimTransfer(request, 9_000_000, 5_000_001, 3_999_999); err == nil {
		t.Fatal("a partial payout of an explicit request was accepted")
	}
}
