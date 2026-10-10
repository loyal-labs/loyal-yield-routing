package backyard

import (
	"math"
	"math/big"
	"testing"
	"time"
)

func TestHourlyRoutineNAVKeepsUrgentReportsAndSafety(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{RouteID, SelectedRouteID, onreONycUSDC, autoAUTOPYUSD.Lane} {
		t.Run(lane, func(t *testing.T) {
			s := base()
			s.RouteLane, s.StrategyKey, s.SelectorEntryPaused = lane, lane, true
			for _, age := range []int64{60, 3599, 3600} {
				s.LastReportAgeSeconds = age
				got := Decide(s)
				if (got.Action == ReportNAV) != (age >= 3600) {
					t.Fatalf("routine age=%d: %+v", age, got)
				}
			}
			s.LastReportAgeSeconds = 60
			s.WithdrawalDemandRaw, s.VoltrIdleRaw = 1, 1
			if got := Decide(s); got.Action != ReportNAV || got.Reason != "withdrawal_covered_nav_due" {
				t.Fatal("withdrawal reporting delayed", got)
			}
			s.WithdrawalDemandRaw, s.VoltrIdleRaw = 0, 0
			s.CapitalMutated = true
			if got := Decide(s); got.Action != ReportNAV {
				t.Fatal("capital mutation reporting delayed", got)
			}
			s.CapitalMutated, s.PostMutationNAVRequired, s.LastReportAgeSeconds = false, true, 1
			if got := Decide(s); got.Action != ReportNAV {
				t.Fatal("post-transaction reporting delayed", got)
			}
			s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 10, 10
			s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS = 6, 6, 6, 6000
			s.SquadsIdleRaw, s.DebtIdleRaw = 6, 0
			if lane == autoAUTOPYUSD.Lane {
				s.SquadsIdleRaw, s.DebtIdleRaw = 0, 6
			}
			if got := Decide(s); got.Reason != "hard_ltv_repay" {
				t.Fatal("risk repayment lost priority", got)
			}
		})
	}
}

func TestHourlyFeeBudgetUsesPilotShareGranularity(t *testing.T) {
	t.Parallel()
	const wealth, supply = 1_207_762_608, 3_256_644
	floor := new(big.Int).Quo(new(big.Int).Lsh(big.NewInt(wealth), 48), big.NewInt(supply))
	in := coherentFeeSelectorFixture(t, wealth, supply, floor)
	e := pilotEconomics{InitialNAV: wealth, EndingNAV: wealth}
	gain, ok := selectorFeeReservedGain(in.Snapshot, 30*24*time.Hour, e, 0)
	// At this real share granularity, hourly rounding plus the bounded
	// transition recipes costs < $0.30, not hundreds of dollars per month.
	if !ok || gain < -300_000 || gain >= 0 {
		t.Fatalf("rounding budget=%f known=%t", -gain, ok)
	}
	in.Quotes[0].Unlevered, in.Quotes[0].BorrowReceiveRaw = true, 0
	in.Quotes[0].CostRaw = 100_000
	in.Markets[0].NativeAPY = .10
	in.Markets[0].CurrentBorrowAPY = 0
	in.Markets[0].BorrowCurve = []BorrowCurvePoint{{0, 0}, {10000, 0}}
	first := SelectOpportunity(in, SelectorState{})
	advanceSelectorFixture(&in, time.Minute)
	got := SelectOpportunity(in, first.State)
	if got.Action != "ENTER" || got.SelectedQuote == nil || got.Candidates[0].BenefitRaw <= float64(in.Policy.MinimumBenefitRaw) {
		t.Fatalf("profitable pilot-size 1x entry blocked: %+v", got)
	}

	s := in.Snapshot
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.HasPosition, s.LeverageTargetLevel = true, 1
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = wealth, wealth
	s.VoltrIdleRaw, s.StrategyNAVRaw, s.PriorReportedNAVRaw, s.JournalArmedNAVRaw = 0, wealth, wealth, wealth
	armLeverageCapacityFixture(&s)
	market := leverageMarket(s.RouteLane, .12, math.Log1p(.06))
	market.CurrentBorrowAPY = .06
	up, ok := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{market}, DefaultSelectorPolicy())
	if !ok || up.Next != 1.5 || up.GainRaw <= float64(DefaultSelectorPolicy().MinimumBenefitRaw) {
		t.Fatalf("profitable pilot-size leverage move blocked: %+v known=%t", up, ok)
	}
}

// KEEP also pays the approved fee. Comparing a taxed candidate to untaxed KEEP
// can reject a profitable move even after correcting the report cadence.
func TestHourlyFeeComparisonDoesNotExemptKEEP(t *testing.T) {
	t.Parallel()
	const wealth, supply = 1_207_762_608, 3_256_644
	floor := new(big.Int).Quo(new(big.Int).Lsh(big.NewInt(wealth), 48), big.NewInt(supply))
	in := coherentFeeSelectorFixture(t, wealth, supply, floor)
	s := &in.Snapshot
	s.HasPosition, s.LeverageTargetLevel = true, 1
	s.LiquidationThresholdBPS = 8000
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = wealth, wealth
	s.VoltrIdleRaw, s.StrategyNAVRaw, s.PriorReportedNAVRaw, s.JournalArmedNAVRaw = 0, wealth, wealth, wealth
	in.Markets = []LaneEconomics{in.Markets[0]}
	in.Markets[0].Lane, in.Markets[0].NativeAPY = s.RouteLane, .10
	in.Markets[0].CurrentBorrowAPY = 0
	got := SelectOpportunity(in, SelectorState{})
	gross := forecastGain(wealth, wealth, 0, in.Markets[0], 0, in.Policy.Horizon.Hours()/(365.25*24))
	if got.KeepGainRaw > .81*gross || got.KeepGainRaw < .79*gross {
		t.Fatalf("KEEP fee upper estimate=%f gross=%f result=%+v", got.KeepGainRaw, gross, got)
	}
}

func TestHourlyKEEPUpperCoversPinnedHolderReturns(t *testing.T) {
	t.Parallel()
	in := coherentFeeSelectorFixture(t, 1_000_000, 1_000_000, new(big.Int).Lsh(big.NewInt(1), 48))
	for _, tc := range []struct{ gain, actual float64 }{
		{100_000, 79_999.489455}, {100_000, 79_616.834171}, {1, 0}, {-100_000, -100_000},
	} {
		upper := selectorKeepGainUpper(in.Snapshot, tc.gain)
		if upper < tc.actual || upper > tc.gain {
			t.Fatalf("KEEP upper=%f pinned=%f gross=%f", upper, tc.actual, tc.gain)
		}
	}
	// Recovery below a prior high-water mark does not owe a new performance fee.
	putScaledLittleSF(&in.Snapshot.VoltrHighWaterMarkBits, new(big.Int).Lsh(big.NewInt(2), 48))
	if got := selectorKeepGainUpper(in.Snapshot, 100_000); got != 100_000 {
		t.Fatalf("taxed recovery below HWM: %f", got)
	}
}

func TestHourlyFeeIncomeIncludesPeakBeforeEndingLoss(t *testing.T) {
	t.Parallel()
	m := LaneEconomics{NativeAPY: math.Expm1(.10)}
	e := pilotForecastEconomics(pilotEconomics{Debt: 1_000_000_000, Proceeds: 1_000_000_000, APR: .19}, 1_000_000_000, m, 3, 0)
	if e.Gain >= 0 || e.PositiveIncome < 2_000_000 || e.PositiveIncome > 4_000_000 {
		t.Fatalf("ending loss hid an earlier fee-paying peak: %+v", e)
	}
}

func TestHourlyNAVFreshnessRetainsActualReportTimestamp(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	for _, age := range []int64{1800, 3599, 3600} {
		s := base()
		s.ReportSequence, s.ReportSnapshotDigest = 9, sha256Bytes([]byte("hourly-nav"))
		s.PriorReportUpdatedUnix, s.LastReportAgeSeconds = now.Unix()-age, age
		p, err := newRouteObservationProjection(Observation{ObservedAt: now, Snapshot: s})
		if err != nil || p.NAVFresh != (age < 3600) || p.ReportObservedAt != time.Unix(now.Unix()-age, 0).UTC().Format(time.RFC3339) {
			t.Fatalf("hourly report age=%d: %+v err=%v", age, p, err)
		}
		s.PostMutationNAVRequired = true
		p, err = newRouteObservationProjection(Observation{ObservedAt: now, Snapshot: s})
		if err != nil || p.NAVFresh {
			t.Fatalf("unreported mutation presented as fresh NAV: %+v err=%v", p, err)
		}
	}
}

func TestHourlyKEEPUpperDoesNotTaxQ48DustWithoutProfit(t *testing.T) {
	t.Parallel()
	in := coherentFeeSelectorFixture(t, 1_051_576, 1<<60, big.NewInt(256))
	for _, gain := range []float64{0, -1, -1000} {
		if got := selectorKeepGainUpper(in.Snapshot, gain); got != gain {
			t.Fatalf("unchanged/loss report charged initial Q48 dust: gain=%f upper=%f", gain, got)
		}
	}
}
