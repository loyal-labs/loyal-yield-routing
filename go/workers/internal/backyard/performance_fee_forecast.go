package backyard

import (
	"math"
	"math/big"
	"time"
)

const selectorLiveSampleInterval = 15 * time.Second

// This is a selector-only gate, never M7 or a manual-recovery latch. Compare
// the HWM to the coherent BOOK, not independently accruing current NAV.
func selectorFeeBaselineKnown(s Snapshot) bool {
	if !s.VoltrHighWaterMarkKnown || !approvedVoltrFeeTerms(s) || s.VoltrTotalValueRaw < 0 ||
		s.LPSupplyInclFeesRaw <= 0 || s.FeeAccumulatorRaw < 0 || s.FeeAccumulatorRaw > s.LPSupplyInclFeesRaw {
		return false
	}
	floor := new(big.Int).Lsh(big.NewInt(s.VoltrTotalValueRaw), 48)
	floor.Quo(floor, big.NewInt(s.LPSupplyInclFeesRaw))
	return littleInt(s.VoltrHighWaterMarkBits[:]).Cmp(floor) >= 0
}

// selectorFeeReportBound covers hourly routine reports plus every possible
// fee-bearing step in the bounded source-exit and destination-entry recipes.
// Initial/pending and terminal reports each get an additional slot. Polls and
// selector wakeups do not accrue fees. Like the route forecast, this assumes
// one modeled move, no later capital flows, safety events or foreign cranks.
func selectorFeeReportBound(horizon time.Duration) int64 {
	return int64((horizon-1)/routineNAVReportInterval+1) + 2*maxSelectorRecipeSteps + 2
}

// selectorKeepGainUpper favors KEEP: one terminal crystallization without LP
// rounding. With fixed fees, no outside cashflow and an ordinary initial HWM,
// splitting gains across reports cannot leave original holders more wealth
// than this bound. A prior high-water mark exempts recovery, not new profit.
// Candidate forecasts still reserve repeated dilution and both ceilings.
func selectorKeepGainUpper(s Snapshot, gain float64) float64 {
	if !s.MonitorsArmed {
		return float64(s.TotalVaultNAVRaw) * performanceFeeForecast(gain/float64(s.TotalVaultNAVRaw))
	}
	ending := math.Floor(math.Nextafter(float64(s.TotalVaultNAVRaw)+gain, math.Inf(-1)))
	if !selectorFeeBaselineKnown(s) || !finite(ending) || ending <= 0 || ending > 1<<53 {
		return gain
	}
	scale := new(big.Int).Lsh(big.NewInt(1), 48)
	baseline := new(big.Int).Mul(littleInt(s.VoltrHighWaterMarkBits[:]), big.NewInt(s.LPSupplyInclFeesRaw))
	book := new(big.Int).Lsh(big.NewInt(s.VoltrTotalValueRaw), 48)
	if baseline.Cmp(book) < 0 {
		// Q48 floor dust is not guaranteed chargeable profit: an unchanged
		// report charges no performance fee, even with a large LP supply.
		baseline = book
	}
	eligible := new(big.Int).Lsh(big.NewInt(int64(ending)), 48)
	eligible.Sub(eligible, baseline)
	if eligible.Sign() <= 0 {
		return gain
	}
	// Floor the minimum fee: never make KEEP worse by rounding it up.
	fee := new(big.Int).Mul(eligible, big.NewInt(approvedAdminPerformanceFeeBPS))
	fee.Quo(fee, new(big.Int).Mul(scale, big.NewInt(10_000)))
	return gain - float64(fee.Int64())
}

// selectorFeeReservedGain is a conditional lower estimate under the existing
// fixed-rate, unchanged-holder/no-external-cashflow route forecast. Candidate
// fee-positive income is at most the modeled NAV rise to its peak plus unreported
// current NAV and sub-Q48 book dust. Each crystallization adds <=1 asset raw
// from fee rounding and <=1 LP valued at maxNAV/current effective supply.
// Existing/extra treasury LP can grow by at most maxNAV/minNAV. Reserve all
// that value, then compare to KEEP's upper return; never haircut a nominal edge.
func selectorFeeReservedGain(s Snapshot, horizon time.Duration, e pilotEconomics, idle float64) (float64, bool) {
	if !finite(e.Gain) || s.TotalVaultNAVRaw <= 0 {
		return 0, false
	}
	if !s.MonitorsArmed {
		if e.Gain <= 0 {
			return e.Gain, true
		}
		wealth := float64(s.TotalVaultNAVRaw)
		return wealth * performanceFeeForecast(e.Gain/wealth), true // projection only
	}
	if horizon <= 0 || !selectorFeeBaselineKnown(s) || !finite(idle) || idle < 0 ||
		!finite(e.PositiveIncome) || e.PositiveIncome < 0 || e.InitialNAV <= 0 {
		return 0, false
	}
	initial, ending := idle+e.InitialNAV, idle+e.EndingNAV
	maxNAV := math.Ceil(math.Nextafter(max(float64(s.TotalVaultNAVRaw), initial+e.PositiveIncome), math.Inf(1)))
	minNAV := math.Floor(math.Nextafter(min(initial, ending, float64(s.VoltrTotalValueRaw)), math.Inf(-1)))
	income := math.Ceil(math.Nextafter(e.PositiveIncome, math.Inf(1)))
	if !finite(maxNAV) || !finite(minNAV) || !finite(income) || maxNAV > 1<<53 || minNAV <= 0 || income > 1<<53 {
		return 0, false
	}
	// Ceil the book wealth above HWM exactly. Equality to the rounded Q48
	// baseline may leave <one Q48 unit per LP; it is not a zero-fee premise.
	scale := new(big.Int).Lsh(big.NewInt(1), 48)
	dust := new(big.Int).Lsh(big.NewInt(s.VoltrTotalValueRaw), 48)
	dust.Sub(dust, new(big.Int).Mul(littleInt(s.VoltrHighWaterMarkBits[:]), big.NewInt(s.LPSupplyInclFeesRaw)))
	dustRaw := new(big.Int)
	if dust.Sign() > 0 {
		dustRaw.Add(dust, new(big.Int).Sub(scale, big.NewInt(1))).Quo(dustRaw, scale)
	}
	eligible := new(big.Int).Add(big.NewInt(int64(income)), dustRaw)
	eligible.Add(eligible, big.NewInt(max(s.TotalVaultNAVRaw-s.VoltrTotalValueRaw, 0)))
	fee := new(big.Rat).SetFrac(new(big.Int).Mul(eligible, big.NewInt(approvedAdminPerformanceFeeBPS)), big.NewInt(10_000))
	unit := new(big.Rat).SetFrac(big.NewInt(int64(maxNAV)), big.NewInt(s.LPSupplyInclFeesRaw))
	// HWM truncation can leave <S/2^48 eligible asset raw after EACH
	// report, not just the baseline. Bootstrap S <= 2*S0*maxNAV/minNAV:
	// the reserve below must stay <minNAV/2, so original holders retain
	// at least half of minNAV throughout the modeled path.
	dustPerReport := new(big.Rat).SetFrac(
		new(big.Int).Mul(new(big.Int).Lsh(big.NewInt(s.LPSupplyInclFeesRaw), 1), big.NewInt(int64(maxNAV))),
		new(big.Int).Mul(big.NewInt(int64(minNAV)), scale))
	dustPerReport.Mul(dustPerReport, big.NewRat(approvedAdminPerformanceFeeBPS, 10_000))
	unit.Add(unit, big.NewRat(1, 1)).Add(unit, dustPerReport).Mul(unit, big.NewRat(selectorFeeReportBound(horizon), 1))
	fee.Add(fee, unit).Mul(fee, new(big.Rat).SetFrac(big.NewInt(int64(maxNAV)), big.NewInt(int64(minNAV))))
	reserve := new(big.Int).Add(fee.Num(), new(big.Int).Sub(fee.Denom(), big.NewInt(1)))
	reserve.Quo(reserve, fee.Denom())
	if !reserve.IsInt64() || reserve.Int64() > 1<<53 ||
		new(big.Int).Lsh(new(big.Int).Set(reserve), 1).Cmp(big.NewInt(int64(minNAV))) >= 0 {
		return 0, false
	}
	return e.Gain - float64(reserve.Int64()), true
}
