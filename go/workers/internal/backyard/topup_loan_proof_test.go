package backyard

import (
	"math/big"
	"testing"
)

// Proof-only: mirrors deployed-verified KLend a08760976f51a3a58c4a0c6ea27b4a0e565bca79 ObligationLiquidity::
// calculate_amount_with_accrued_interest. This is not a runtime admission gate.
func TestTopupLoanProtocolProof(t *testing.T) {
	f := new(big.Int).Lsh(big.NewInt(1), 60)
	accrue := func(amount, former, current *big.Int) *big.Int {
		return new(big.Int).Quo(new(big.Int).Mul(amount, current), former)
	}
	upperAndGap := func(amount, former, current *big.Int, count int64) (*big.Int, *big.Int) {
		upper := accrue(amount, former, current)
		if count == 0 || current.Cmp(former) == 0 {
			return upper, big.NewInt(0)
		}
		// gap < count*current/former; largest possible INTEGER gap is
		// ceil(count*current/former)-1, computed without float or raw ceil.
		numerator := new(big.Int).Mul(big.NewInt(count), current)
		gap := new(big.Int).Quo(new(big.Int).Sub(numerator, big.NewInt(1)), former)
		return upper, gap
	}
	within := func(value, upper, gap *big.Int) bool {
		return value.Cmp(upper) <= 0 && value.Cmp(new(big.Int).Sub(upper, gap)) >= 0
	}
	t.Run("same endpoint cannot prove transaction history", func(t *testing.T) {
		// A valid 0->1 bps curve at utilization 1e-6 has this annual Q60
		// rate. Pinned one-slot compounding yields exactly ONE SF rate bit.
		annual := new(big.Int).Quo(new(big.Int).Quo(new(big.Int).Set(f), big.NewInt(1_000_000)), big.NewInt(10_000))
		if new(big.Int).Quo(annual, big.NewInt(63_072_000)).Cmp(big.NewInt(1)) != 0 {
			t.Fatal("counterexample rate is not reachable by pinned compounding")
		}
		for _, principal := range []int64{1, 3} {
			// principal=1: no fee. principal=3: receive2 + minimum fee1;
			// receive1 with fee1 would correctly fail BorrowTooSmall.
			count := 2*principal + 1
			original := new(big.Int).Add(new(big.Int).Mul(big.NewInt(1000), f), new(big.Int).Rsh(new(big.Int).Set(f), 1))
			legal, former := new(big.Int).Set(original), new(big.Int).Set(f)
			for i := int64(1); i <= count; i++ {
				current := new(big.Int).Add(f, big.NewInt(i))
				legal = accrue(legal, former, current)
				former = current
			}
			penultimate := new(big.Int).Add(f, big.NewInt(count-1))
			changed := accrue(original, f, penultimate)
			changed.Sub(changed, new(big.Int).Mul(big.NewInt(principal), f))
			changed = accrue(changed, penultimate, former)
			changed.Add(changed, new(big.Int).Mul(big.NewInt(principal), f))
			upper, gap := upperAndGap(original, f, former, count)
			if changed.Cmp(legal) != 0 || !within(changed, upper, gap) {
				t.Fatal("counterexample no longer has the identical endpoint", principal)
			}
			t.Logf("principal=%d raw, refreshes=%d, identical endpoint; gap=%s SF bits", principal, count, new(big.Int).Sub(upper, legal))
		}
	})
}
