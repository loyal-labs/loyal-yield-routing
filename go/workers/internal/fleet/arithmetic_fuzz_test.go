package fleet

import (
	"math"
	"math/big"
	"testing"
)

// Proportional custody/economics must use exact truncation toward zero and
// reject final BIGINT overflow, even if intermediate products exceed BIGINT.
func FuzzProportionalAmount(f *testing.F) {
	for _, parts := range [][3]int64{
		{1_000_000_000_000, 4999, 10_000},
		{math.MaxInt64, 2, 2}, {math.MinInt64, -1, 1},
		{-1, math.MinInt64, -1}, {math.MinInt64, 1, -1},
		{0, math.MaxInt64, -1}, {math.MaxInt64, math.MaxInt64, math.MaxInt64},
		{1, -2, 3}, {math.MinInt64, math.MinInt64, math.MinInt64},
	} {
		f.Add(parts[0], parts[1], parts[2])
	}
	f.Fuzz(func(t *testing.T, left, right, divisor int64) {
		got, ok := mulDivInt64(left, right, divisor)
		if divisor == 0 {
			if ok {
				t.Fatal("zero divisor accepted")
			}
			return
		}
		expected := new(big.Int).Mul(big.NewInt(left), big.NewInt(right))
		expected.Quo(expected, big.NewInt(divisor))
		if ok != expected.IsInt64() || ok && got != expected.Int64() {
			t.Fatalf("%d*%d/%d = %d/%v, exact %s", left, right, divisor, got, ok, expected)
		}
	})
}
