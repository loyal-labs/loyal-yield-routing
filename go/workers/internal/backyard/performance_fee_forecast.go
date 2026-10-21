package backyard

import (
	"math"
	"math/big"
	"time"
)

const selectorLiveSampleInterval = 15 * time.Second

const voltrHighWaterMarkFractionBits = 48

func selectorFeeBaselineKnown(s Snapshot) bool {
	if !approvedVoltrFeeTerms(s) || s.VoltrTotalValueRaw < 0 ||
		s.LPSupplyInclFeesRaw <= 0 || s.FeeAccumulatorRaw < 0 || s.FeeAccumulatorRaw > s.LPSupplyInclFeesRaw {
		return false
	}
	floor := new(big.Int).Lsh(big.NewInt(s.VoltrTotalValueRaw), voltrHighWaterMarkFractionBits)
	floor.Quo(floor, big.NewInt(s.LPSupplyInclFeesRaw))
	return littleInt(s.VoltrHighWaterMarkBits[:]).Cmp(floor) >= 0
}

// selectorEntryReportBound bounds the fee-bearing NAV reports an entry and
// its unwind can add beyond the routine ones: one after each capital
// mutation (allocate, swap, deposit, up steps; down steps, withdraw, swap,
// stage, restore), well under it.
const selectorEntryReportBound = 64

const initialAndTerminalReportSlots = 2

func selectorFeeReportBound(horizon time.Duration) int64 {
	return int64((horizon-1)/routineNAVReportInterval+1) + selectorEntryReportBound + initialAndTerminalReportSlots
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
		return wealth * performanceFeeForecast(e.Gain/wealth, s.ApprovedAdminPerformanceFeeBPS), true
	}
	if horizon <= 0 || !selectorFeeBaselineKnown(s) || !finite(idle) || idle < 0 ||
		!finite(e.PositiveIncome) || e.PositiveIncome < 0 || e.InitialNAV <= 0 {
		return 0, false
	}
	if s.ApprovedAdminPerformanceFeeBPS == 0 {
		return e.Gain, true
	}
	initial, ending := idle+e.InitialNAV, idle+e.EndingNAV
	maxNAV := math.Ceil(math.Nextafter(max(float64(s.TotalVaultNAVRaw), initial+e.PositiveIncome), math.Inf(1)))
	minNAV := math.Floor(math.Nextafter(min(initial, ending, float64(s.VoltrTotalValueRaw)), math.Inf(-1)))
	income := math.Ceil(math.Nextafter(e.PositiveIncome, math.Inf(1)))
	if !finite(maxNAV) || !finite(minNAV) || !finite(income) || maxNAV > 1<<53 || minNAV <= 0 || income > 1<<53 {
		return 0, false
	}
	scale := new(big.Int).Lsh(big.NewInt(1), voltrHighWaterMarkFractionBits)
	dust := new(big.Int).Lsh(big.NewInt(s.VoltrTotalValueRaw), voltrHighWaterMarkFractionBits)
	dust.Sub(dust, new(big.Int).Mul(littleInt(s.VoltrHighWaterMarkBits[:]), big.NewInt(s.LPSupplyInclFeesRaw)))
	dustRaw := new(big.Int)
	if dust.Sign() > 0 {
		dustRaw.Add(dust, new(big.Int).Sub(scale, big.NewInt(1))).Quo(dustRaw, scale)
	}
	eligible := new(big.Int).Add(big.NewInt(int64(income)), dustRaw)
	eligible.Add(eligible, big.NewInt(max(s.TotalVaultNAVRaw-s.VoltrTotalValueRaw, 0)))
	fee := new(big.Rat).SetFrac(new(big.Int).Mul(eligible, big.NewInt(s.ApprovedAdminPerformanceFeeBPS)), big.NewInt(10_000))
	unit := new(big.Rat).SetFrac(big.NewInt(int64(maxNAV)), big.NewInt(s.LPSupplyInclFeesRaw))
	dustPerReport := new(big.Rat).SetFrac(
		new(big.Int).Mul(new(big.Int).Lsh(big.NewInt(s.LPSupplyInclFeesRaw), 1), big.NewInt(int64(maxNAV))),
		new(big.Int).Mul(big.NewInt(int64(minNAV)), scale))
	dustPerReport.Mul(dustPerReport, big.NewRat(s.ApprovedAdminPerformanceFeeBPS, 10_000))
	unit.Add(unit, big.NewRat(1, 1)).Add(unit, dustPerReport).Mul(unit, big.NewRat(selectorFeeReportBound(horizon), 1))
	fee.Add(fee, unit).Mul(fee, new(big.Rat).SetFrac(big.NewInt(int64(maxNAV)), big.NewInt(int64(minNAV))))
	reserve := new(big.Int).Add(fee.Num(), new(big.Int).Sub(fee.Denom(), big.NewInt(1)))
	reserve.Quo(reserve, fee.Denom())
	if !reserve.IsInt64() || reserve.Int64() > 1<<53 || !reserveLeavesHoldersHalfMinNAV(reserve, int64(minNAV)) {
		return 0, false
	}
	return e.Gain - float64(reserve.Int64()), true
}

func reserveLeavesHoldersHalfMinNAV(reserve *big.Int, minNAV int64) bool {
	return new(big.Int).Lsh(new(big.Int).Set(reserve), 1).Cmp(big.NewInt(minNAV)) < 0
}
