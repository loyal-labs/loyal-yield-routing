package backyard

import (
	"fmt"
	"math/big"
	"time"
)

const (
	feeUnitsPerMicroUSDC     int64 = (10_000 / loopingPerformanceFeeBPS) << voltrHighWaterMarkFractionBits
	loopingPerformanceFeeBPS       = 1000
	loopingWeeklyFeeMaxRaw   int64 = 10_000_000_000
	loopingTreasuryUSDC            = "8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce"
)

// ActivationSlot is the finalized zero-fee cutover. Each later version is
// installed only by reconciliation of the exact report that published it.
type performanceFeeState struct {
	ActivationSlot    int64  `json:"activationSlot"`
	WeeklyCapRaw      int64  `json:"weeklyCapRaw"`
	Version           int64  `json:"version"`
	PeakBits          string `json:"peakBits"`
	LastAssessment    int64  `json:"lastAssessment"`
	PublishedValue    int64  `json:"publishedValue"`
	PublishedSupply   int64  `json:"publishedSupply"`
	CapitalSlot       int64  `json:"capitalSlot"`
	CapitalHold       string `json:"capitalHold,omitempty"`
	Remainder         int64  `json:"remainder"`
	PublishedCash     int64  `json:"publishedCash"`
	PaidThisWeek      int64  `json:"paidThisWeek"`
	PublishedStrategy int64  `json:"publishedStrategy"`
	Outstanding       int64  `json:"outstanding"`
	Payable           int64  `json:"payable"`
	Cutoff            int64  `json:"cutoff"`
}

type performanceFeeEntry struct {
	Resolution  feeCapitalResolution `json:"capitalResolution,omitzero"`
	Before      performanceFeeState  `json:"before"`
	After       performanceFeeState  `json:"after"`
	Slot        int64                `json:"slot"`
	At          int64                `json:"at"`
	Assessed    int64                `json:"assessedRaw"`
	OutsideCash int64                `json:"outsideCashRaw"`
	Paid        int64                `json:"paidRaw"`
}

func mondayCutoff(at int64) int64 {
	t := time.Unix(at, 0).UTC()
	return t.Truncate(24*time.Hour).AddDate(0, 0, -(int(t.Weekday())+6)%7).Unix()
}

func (f performanceFeeState) matured(at int64) int64 {
	if mondayCutoff(at) > f.Cutoff {
		return f.Outstanding
	}
	return f.Payable
}

func (f performanceFeeState) paidSinceMonday(at int64) int64 {
	if mondayCutoff(at) > f.Cutoff {
		return 0
	}
	return f.PaidThisWeek
}

func (f performanceFeeState) paymentRoom(at int64) int64 {
	return min(f.matured(at), f.WeeklyCapRaw-f.paidSinceMonday(at))
}

func ceilQuo(n, d *big.Int) *big.Int {
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, d, r)
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func feePriceBits(value, supply int64) *big.Int {
	return ceilQuo(new(big.Int).Lsh(big.NewInt(value), voltrHighWaterMarkFractionBits), big.NewInt(supply))
}

func (f performanceFeeState) valid() bool {
	if f.ActivationSlot <= 0 || f.WeeklyCapRaw <= 0 || f.WeeklyCapRaw > loopingWeeklyFeeMaxRaw || f.Version < 0 {
		return false
	}
	if f.Version == 0 {
		return f == (performanceFeeState{ActivationSlot: f.ActivationSlot, WeeklyCapRaw: f.WeeklyCapRaw})
	}
	bits, ok := new(big.Int).SetString(f.PeakBits, 10)
	return ok && bits.Sign() >= 0 && bits.BitLen() <= 128 && f.LastAssessment > 0 && f.PublishedValue >= 0 &&
		f.PublishedSupply > 0 && f.PublishedStrategy >= 0 && f.PublishedStrategy <= f.PublishedValue &&
		f.CapitalSlot >= f.ActivationSlot && f.Remainder >= 0 && f.Remainder < feeUnitsPerMicroUSDC && f.PublishedCash >= 0 && f.PaidThisWeek >= 0 && f.PaidThisWeek <= f.WeeklyCapRaw &&
		f.Outstanding >= 0 && f.Payable >= 0 && f.Payable <= f.Outstanding && f.Cutoff >= mondayCutoff(f.LastAssessment) && f.Cutoff == mondayCutoff(f.Cutoff)
}

// assess uses the pre-operation supply and total wealth. Allocating/returning
// strategy principal does not change wealth. A payout releases its reservation.
func (f performanceFeeState) assess(s Snapshot, grossValue, postStrategy, postCash, payout int64, hourly bool) (performanceFeeEntry, error) {
	entry := performanceFeeEntry{Before: f, After: f, Slot: s.Slot, At: s.ChainUnix, Paid: payout}
	n := &entry.After
	if !f.valid() || !s.Trustful || !s.MonitorsArmed || !approvedVoltrFeeTerms(s) ||
		s.AdminPerformanceFeeBPS != 0 || s.IssuanceFeeBPS != 0 || s.Slot < f.ActivationSlot || s.ChainUnix <= 0 ||
		s.ChainUnix < f.LastAssessment || s.LPSupplyInclFeesRaw <= 0 || s.VoltrTotalValueRaw < 0 ||
		s.FeeAccumulatorRaw < 0 || s.FeeAccumulatorRaw > s.LPSupplyInclFeesRaw || grossValue < f.Outstanding ||
		postStrategy < 0 || postCash < 0 || s.SquadsIdleRaw < 0 || payout < 0 || payout > f.paymentRoom(s.ChainUnix) ||
		f.Version > 0 && f.PublishedStrategy != s.PriorReportedNAVRaw {
		return performanceFeeEntry{}, staleHold("looping_fee_checkpoint_mismatch")
	}
	netValue := grossValue - f.Outstanding
	supply := s.LPSupplyInclFeesRaw
	peak := littleInt(s.VoltrHighWaterMarkBits[:])
	cutoff := mondayCutoff(s.ChainUnix)
	n.Payable = f.matured(s.ChainUnix)
	n.Cutoff = cutoff
	n.PaidThisWeek = f.paidSinceMonday(s.ChainUnix) + payout
	n.PublishedCash = postCash
	if f.Version == 0 {
		// Exclude unpublished gains too. A lower cutover price retains the old loss carryforward.
		peak = maxBig(peak, feePriceBits(netValue, supply))
		n.LastAssessment = s.ChainUnix
		n.CapitalSlot = s.Slot
	} else {
		peak, _ = new(big.Int).SetString(f.PeakBits, 10)
		// A public deposit, claim, burn or harvest between manager reports
		// can move the published price. Exclude capital-driven appreciation.
		if f.PublishedValue > 0 {
			numerator := new(big.Int).Mul(big.NewInt(s.VoltrTotalValueRaw), big.NewInt(f.PublishedSupply))
			denominator := new(big.Int).Mul(big.NewInt(f.PublishedValue), big.NewInt(supply))
			if numerator.Cmp(denominator) > 0 {
				peak = ceilQuo(numerator.Mul(numerator, peak), denominator)
			}
		}
		// Newly issued shares enter at the last published price. If an extra
		// capital/drift report published gains between hourly assessments,
		// their purchase price cannot become assessable performance.
		price := feePriceBits(s.VoltrTotalValueRaw, supply)
		if supply > f.PublishedSupply && price.Cmp(peak) > 0 {
			added := new(big.Int).Mul(price, big.NewInt(supply-f.PublishedSupply))
			basis := new(big.Int).Mul(peak, big.NewInt(f.PublishedSupply))
			peak = ceilQuo(basis.Add(basis, added), big.NewInt(supply))
		}
		// Only journaled strategy operations explain movement in Squads cash.
		// Lift the HWM by unsolicited principal, including while underwater.
		expected := new(big.Int).Add(big.NewInt(f.PublishedCash), big.NewInt(s.performanceCashDelta))
		outside := new(big.Int).Sub(big.NewInt(s.SquadsIdleRaw), expected)
		if !outside.IsInt64() || outside.Sign() < 0 {
			return performanceFeeEntry{}, staleHold("looping_fee_unexplained_cash_debit")
		}
		entry.OutsideCash = outside.Int64()
		peak.Add(peak, feePriceBits(entry.OutsideCash, supply))
		if hourly && s.ChainUnix-f.LastAssessment >= int64(time.Hour/time.Second) {
			eligible := new(big.Int).Lsh(big.NewInt(netValue), voltrHighWaterMarkFractionBits)
			eligible.Sub(eligible, new(big.Int).Mul(peak, big.NewInt(supply)))
			n.CapitalHold = s.feeCapitalHold
			if s.feeCapitalSlot != s.Slot && n.CapitalHold == "" {
				return performanceFeeEntry{}, staleHold("looping_fee_capital_unverified")
			}
			if n.CapitalHold == "" {
				n.CapitalSlot = s.Slot
			}
			if r := s.feeResolution; r.Signature != "" {
				if !r.valid() || f.CapitalHold != r.Signature || s.feeCapitalSlot != s.Slot {
					return performanceFeeEntry{}, staleHold("looping_fee_resolution_mismatch")
				}
				entry.Resolution = r
				// Waive the uncertain interval. Carry the old peak plus the
				// reviewed principal per share, even when underwater.
				peak = maxBig(peak.Add(peak, feePriceBits(r.Principal, r.Supply)), feePriceBits(netValue, supply))
			} else if eligible.Sign() > 0 && n.CapitalHold == "" {
				fee, remainder := new(big.Int), new(big.Int)
				fee.QuoRem(eligible.Add(eligible, big.NewInt(f.Remainder)), big.NewInt(feeUnitsPerMicroUSDC), remainder)
				n.Remainder = remainder.Int64()
				if !fee.IsInt64() || fee.Int64() > netValue {
					return performanceFeeEntry{}, fmt.Errorf("fee exceeds net assets")
				}
				entry.Assessed = fee.Int64()
				peak = maxBig(peak, feePriceBits(netValue-entry.Assessed, supply))
				if s.ChainUnix == cutoff {
					n.Payable += entry.Assessed
				}
			}
			n.LastAssessment = s.ChainUnix
		}
	}
	n.Version++
	n.PeakBits = peak.String()
	n.Outstanding += entry.Assessed - payout
	n.Payable -= payout
	if postStrategy < n.Outstanding {
		return performanceFeeEntry{}, staleHold("looping_fee_reserve_exceeds_strategy")
	}
	n.PublishedStrategy, n.PublishedSupply, n.PublishedValue = postStrategy-n.Outstanding, supply, netValue-entry.Assessed
	if !n.valid() {
		return performanceFeeEntry{}, staleHold("invalid_looping_fee_transition")
	}
	return entry, nil
}

func maxBig(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

func (s Snapshot) projectedFeeBPS() int64 {
	if s.performanceFee != nil && s.performanceFee.Version > 0 {
		return loopingPerformanceFeeBPS
	}
	return s.ApprovedAdminPerformanceFeeBPS
}

// Voltr's totalValue excludes unsolicited transfers to its idle ATA.
func feeBookedIdle(total, strategy, physical int64) (int64, error) {
	if total < 0 || strategy < 0 || strategy > total || total-strategy > physical {
		return 0, staleHold("looping_fee_book_not_reconciled")
	}
	return total - strategy, nil
}

func reservePerformanceFee(nav *RouteNAVSnapshot, raw int64) error {
	if raw == 0 || nav.net == nil {
		return nil
	}
	if raw < 0 || uint64(raw) > nav.StrategyNAVRaw || uint64(raw) > nav.TotalVaultNAVRaw {
		return staleHold("looping_fee_reserve_exceeds_strategy")
	}
	nav.StrategyNAVRaw -= uint64(raw)
	nav.TotalVaultNAVRaw -= uint64(raw)
	nav.SnapshotDigest = navDigest(sha256Bytes([]byte(fmt.Sprintf("%s:performance-fee:%d", nav.SnapshotDigest, raw))))
	nav.Report.NAVAfterRaw, nav.Report.SnapshotDigest = nav.StrategyNAVRaw, nav.SnapshotDigest
	cert := *nav.net
	cert.report = nav.Report
	nav.net = &cert
	return nil
}
