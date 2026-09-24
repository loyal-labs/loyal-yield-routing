package backyardrwa

import "testing"

// Snapshot debt is priced on the refreshed-reserve simulation; prestate
// captures read raw reserves or a later simulation. Live 2026-09-24: a
// $99.50 Maple borrow held every step because 99,495,523 != 99,495,499.
// Interest inside the priced window passes; a real borrow or repay does not.
func TestSameAccruingDebtToleratesOnlyPricedInterest(t *testing.T) {
	b := KaminoPayoffBound{ObservedDebtRaw: 99_495_499, UpperDebtRaw: 99_495_685}
	for _, c := range []struct {
		snapshot int64
		want     bool
	}{
		{99_495_499, true}, {99_495_523, true}, {99_495_476, true}, // same principal, other basis
		{99_495_499 + 187, true}, {99_495_499 + 188, false}, // window interest + 1 unit ceil
		{99_495_499 - 188, false}, {199_495_499, false}, {1, false}, {0, false}, // real borrow / repay
	} {
		if got := sameAccruingDebt(b, c.snapshot); got != c.want {
			t.Fatalf("snapshot %d: got %v want %v", c.snapshot, got, c.want)
		}
	}
	if sameAccruingDebt(KaminoPayoffBound{ObservedDebtRaw: 10, UpperDebtRaw: 9}, 10) || sameAccruingDebt(KaminoPayoffBound{}, 0) {
		t.Fatal("malformed bound accepted")
	}
}
