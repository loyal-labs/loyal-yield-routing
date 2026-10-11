package kamino

import (
	"math"
	"math/big"
	"testing"
)

// A reserve 40% borrowed on a 0-to-10% curve with a 10% protocol take and a
// 1% host rate: suppliers earn the curve on the borrowed share net of the
// take, borrowers the curve plus host. A TrueApr reserve accrues the plain
// curve; a Legacy reserve accrues per slot over KLend's 2-slots-a-second
// year, 1.25x the curve at Solana's 400 ms slots.
func TestReserveRatesAccrueTheCurveByInterestBasis(t *testing.T) {
	var borrowed [16]byte
	sf := new(big.Int).Lsh(big.NewInt(40), 60).Bytes()
	for i := range sf {
		borrowed[i] = sf[len(sf)-1-i]
	}
	reserve := Reserve{AvailableAmount: 60, BorrowedAmountSF: borrowed, ProtocolTakeRatePct: 10, HostFixedInterestRateBPS: 100}
	for i := range reserve.BorrowRateCurve {
		reserve.BorrowRateCurve[i] = CurvePoint{10_000, 1_000}
	}
	reserve.BorrowRateCurve[0] = CurvePoint{0, 0}
	for basis, accrual := range map[uint8]float64{trueAPRBasis: 1, 0: 1.25} {
		reserve.InterestRateBasis = basis
		rates, err := reserve.Rates()
		if err != nil {
			t.Fatal(err)
		}
		want := Rates{Utilization: .4, BorrowAPR: .05 * accrual, SupplyAPR: .04 * .4 * .9 * accrual}
		want.BorrowAPY, want.SupplyAPY = math.Expm1(want.BorrowAPR), math.Expm1(want.SupplyAPR)
		for name, pair := range map[string][2]float64{"utilization": {rates.Utilization, want.Utilization}, "borrow APR": {rates.BorrowAPR, want.BorrowAPR}, "supply APR": {rates.SupplyAPR, want.SupplyAPR}, "borrow APY": {rates.BorrowAPY, want.BorrowAPY}, "supply APY": {rates.SupplyAPY, want.SupplyAPY}} {
			if math.Abs(pair[0]-pair[1]) > 1e-12 {
				t.Fatalf("basis %d %s = %v, want %v", basis, name, pair[0], pair[1])
			}
		}
	}
}
