package kamino

import (
	"errors"
	"math"
	"math/big"
)

var ErrSlotRateUnavailable = errors.New("Kamino Legacy-basis rates need the measured slot rate")

var ErrUnsupportedInterestTerms = errors.New("Kamino reserve interest terms are unsupported")

const (
	legacyBasis  = 0
	trueAPRBasis = 1
	// klendSlotsPerSecond is KLend's SLOTS_PER_SECOND, the slot count its
	// SLOTS_PER_YEAR divides a Legacy-basis annual rate by.
	klendSlotsPerSecond = 2
)

// SupportedInterestTerms rejects a reserve whose interest needs a model the
// workers do not have: an unknown rate basis, an early-repay penalty, or a
// protocol take above 100%. Term debt (DebtMaturityTimestamp,
// DebtTermSeconds) accrues on the same curve as open-term debt: its term
// decides only when an obligation may borrow and when its debt becomes
// liquidatable (ObligationLiquidity.DebtTermEnd), not what it accrues.
func (r Reserve) SupportedInterestTerms() error {
	if r.InterestRateBasis > trueAPRBasis || r.EarlyRepayRemainingInterestPct != 0 || r.ProtocolTakeRatePct > 100 {
		return ErrUnsupportedInterestTerms
	}
	return nil
}

func (r Reserve) NeedsSlotRate() bool { return r.InterestRateBasis == legacyBasis }

// AccrualPerCurve is the annual rate a reserve accrues per unit of its
// configured curve rate. A TrueApr-basis reserve accrues the curve per second
// over SECONDS_PER_YEAR: the plain curve. A Legacy-basis reserve accrues
// curve/SLOTS_PER_YEAR per slot, with SLOTS_PER_YEAR counted at 2 slots a
// second, so a year earns the curve times the chain's real slot rate over 2.
// slotsPerSecond is that measured rate; it is not read for TrueApr.
func (r Reserve) AccrualPerCurve(slotsPerSecond float64) (float64, error) {
	if err := r.SupportedInterestTerms(); err != nil {
		return 0, err
	}
	if r.InterestRateBasis == trueAPRBasis {
		return 1, nil
	}
	if !(slotsPerSecond > 0) || math.IsInf(slotsPerSecond, 0) {
		return 0, ErrSlotRateUnavailable
	}
	return slotsPerSecond / klendSlotsPerSecond, nil
}

type Rates struct {
	Utilization                                float64
	BorrowAPR, SupplyAPR, BorrowAPY, SupplyAPY float64
}

func (r Reserve) Rates(slotsPerSecond float64) (Rates, error) {
	accrual, err := r.AccrualPerCurve(slotsPerSecond)
	if err != nil {
		return Rates{}, err
	}
	total, err := r.TotalLiquiditySF()
	if err != nil {
		return Rates{}, err
	}
	var out Rates
	if total.Sign() > 0 {
		out.Utilization, _ = new(big.Rat).SetFrac(U128(r.BorrowedAmountSF), total).Float64()
	}
	curve := CurveRate(r.BorrowRateCurve, out.Utilization) * accrual
	out.BorrowAPR = curve + float64(r.HostFixedInterestRateBPS)/10_000*accrual
	out.SupplyAPR = curve * out.Utilization * (1 - float64(r.ProtocolTakeRatePct)/100)
	out.BorrowAPY, out.SupplyAPY = math.Expm1(out.BorrowAPR), math.Expm1(out.SupplyAPR)
	return out, nil
}

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
