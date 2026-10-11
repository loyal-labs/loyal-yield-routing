package kamino

import (
	"math"
	"math/big"
)

// AccrualPerCurve is the annual rate a reserve accrues per unit of its
// configured curve rate. KLend's curve is an annual rate. A TrueApr-basis
// reserve accrues it per second over SECONDS_PER_YEAR: the plain curve. A
// Legacy-basis reserve accrues it per slot over SLOTS_PER_YEAR, which KLend
// counts at SLOTS_PER_SECOND = 2; Solana's target slot is 400 ms (2.5 a
// second), so a year of slots accrues 2.5/2 = 1.25 times the curve.
func (r Reserve) AccrualPerCurve() float64 {
	if r.InterestRateBasis == trueAPRBasis {
		return 1
	}
	return 2.5 / 2
}

const trueAPRBasis = 1

// Rates are a reserve's annual rates at its stored utilization as KLend
// accrues them, the one reserve-rate definition the workers use. Suppliers
// earn borrow interest net of the protocol take; the host fixed rate is
// borrower-only. APY compounds continuously.
type Rates struct {
	Utilization                                float64
	BorrowAPR, SupplyAPR, BorrowAPY, SupplyAPY float64
}

func (r Reserve) Rates() (Rates, error) {
	total, err := r.TotalLiquiditySF()
	if err != nil {
		return Rates{}, err
	}
	var out Rates
	if total.Sign() > 0 {
		out.Utilization, _ = new(big.Rat).SetFrac(U128(r.BorrowedAmountSF), total).Float64()
	}
	accrual := r.AccrualPerCurve()
	curve := CurveRate(r.BorrowRateCurve, out.Utilization) * accrual
	out.BorrowAPR = curve + float64(r.HostFixedInterestRateBPS)/10_000*accrual
	out.SupplyAPR = curve * out.Utilization * (1 - float64(r.ProtocolTakeRatePct)/100)
	out.BorrowAPY, out.SupplyAPY = math.Expm1(out.BorrowAPR), math.Expm1(out.SupplyAPR)
	return out, nil
}

// CurveRate is a curve's annual rate at utilization: linear between its
// points, the terminal point at or above full utilization.
func CurveRate(curve [11]CurvePoint, utilization float64) float64 {
	u := min(max(utilization, 0), 1) * 10_000
	for i := 1; i < len(curve); i++ {
		a, b := curve[i-1], curve[i]
		if b.UtilizationRateBPS > a.UtilizationRateBPS && u <= float64(b.UtilizationRateBPS) {
			width := float64(b.UtilizationRateBPS - a.UtilizationRateBPS)
			return (float64(a.BorrowRateBPS) + (float64(b.BorrowRateBPS)-float64(a.BorrowRateBPS))*(u-float64(a.UtilizationRateBPS))/width) / 10_000
		}
	}
	return float64(curve[len(curve)-1].BorrowRateBPS) / 10_000
}
