package kamino

import (
	"errors"
	"math"
	"math/big"
	"testing"
)

func ratesFixture() Reserve {
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
	return reserve
}

// A reserve 40% borrowed on a 0-to-10% curve with a 10% protocol take and a
// 1% host rate: suppliers earn the curve on the borrowed share net of the
// take, borrowers the curve plus host. A TrueApr reserve accrues the plain
// curve at any slot rate. A Legacy reserve accrues curve/SLOTS_PER_YEAR per
// slot over KLend's 2-slots-a-second year, so its rates scale with the
// measured slot rate: 2.5 slots a second is 1.25x, mainnet's ~4.6 is 2.3x.
func TestReserveRatesScaleLegacyAccrualWithTheMeasuredSlotRate(t *testing.T) {
	reserve := ratesFixture()
	for _, tc := range []struct {
		basis          uint8
		slotsPerSecond float64
		accrual        float64
	}{{trueAPRBasis, 0, 1}, {trueAPRBasis, 4.6, 1}, {0, 2.5, 1.25}, {0, 4.6, 2.3}} {
		reserve.InterestRateBasis = tc.basis
		rates, err := reserve.Rates(tc.slotsPerSecond)
		if err != nil {
			t.Fatal(err)
		}
		want := Rates{Utilization: .4, BorrowAPR: .05 * tc.accrual, SupplyAPR: .04 * .4 * .9 * tc.accrual}
		want.BorrowAPY, want.SupplyAPY = math.Expm1(want.BorrowAPR), math.Expm1(want.SupplyAPR)
		for name, pair := range map[string][2]float64{"utilization": {rates.Utilization, want.Utilization}, "borrow APR": {rates.BorrowAPR, want.BorrowAPR}, "supply APR": {rates.SupplyAPR, want.SupplyAPR}, "borrow APY": {rates.BorrowAPY, want.BorrowAPY}, "supply APY": {rates.SupplyAPY, want.SupplyAPY}} {
			if math.Abs(pair[0]-pair[1]) > 1e-12 {
				t.Fatalf("basis %d at %v slots/s: %s = %v, want %v", tc.basis, tc.slotsPerSecond, name, pair[0], pair[1])
			}
		}
	}
}

// A Legacy reserve without a measured slot rate is unavailable, never priced
// at a guessed rate; an unknown basis, an early-repay penalty or a take above
// 100% is refused. Term debt prices on its curve like open-term debt.
func TestReserveRatesRefuseWhatTheyCannotPrice(t *testing.T) {
	reserve := ratesFixture()
	if _, err := reserve.Rates(0); !errors.Is(err, ErrSlotRateUnavailable) {
		t.Fatalf("Legacy rates without a slot rate = %v", err)
	}
	for name, mutate := range map[string]func(*Reserve){
		"unknown basis":  func(r *Reserve) { r.InterestRateBasis = 2 },
		"early repay":    func(r *Reserve) { r.EarlyRepayRemainingInterestPct = 5 },
		"take above 100": func(r *Reserve) { r.ProtocolTakeRatePct = 101 },
	} {
		r := ratesFixture()
		mutate(&r)
		if _, err := r.Rates(4.6); err == nil || errors.Is(err, ErrSlotRateUnavailable) {
			t.Fatalf("%s was priced: %v", name, err)
		}
	}
	open, term := ratesFixture(), ratesFixture()
	term.DebtTermSeconds, term.DebtMaturityTimestamp = 2_592_000, 1_800_000_000
	openRates, err := open.Rates(4.6)
	if err != nil {
		t.Fatal(err)
	}
	if termRates, err := term.Rates(4.6); err != nil || termRates != openRates {
		t.Fatalf("term debt rates %+v (%v), want the open-term curve's %+v", termRates, err, openRates)
	}
}
