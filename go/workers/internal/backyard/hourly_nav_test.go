package backyard

import (
	"math"
	"math/big"
	"testing"
	"time"
)

func TestHourlyRoutineNAVKeepsUrgentReportsAndSafety(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{MapleSyrupUSDCLaneID, onreONycUSDC, autoAUTOPYUSD.Lane} {
		t.Run(lane, func(t *testing.T) {
			s := base()
			s.RouteLane, s.StrategyKey = lane, lane
			for _, age := range []int64{60, 3599, 3600} {
				s.LastReportAgeSeconds = age
				got := Decide(targeted(s))
				if (got.Action == ReportNAV) != (age >= 3600) {
					t.Fatalf("routine age=%d: %+v", age, got)
				}
			}
			s.LastReportAgeSeconds = 60
			s.WithdrawalDemandRaw, s.VoltrIdleRaw = 1, 1
			if got := Decide(targeted(s)); got.Action != ReportNAV || got.Reason != "withdrawal_covered_nav_due" {
				t.Fatal("withdrawal reporting delayed", got)
			}
			s.WithdrawalDemandRaw, s.VoltrIdleRaw = 0, 0
			s.CapitalMutated = true
			if got := Decide(targeted(s)); got.Action != ReportNAV {
				t.Fatal("capital mutation reporting delayed", got)
			}
			s.CapitalMutated, s.PostMutationNAVRequired, s.LastReportAgeSeconds = false, true, 1
			if got := Decide(targeted(s)); got.Action != ReportNAV {
				t.Fatal("post-transaction reporting delayed", got)
			}
			s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 10, 10
			s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw, s.LTVBPS = 6, 6, 6, 7000
			s.SquadsIdleRaw, s.DebtIdleRaw = 6, 0
			if lane == autoAUTOPYUSD.Lane {
				s.SquadsIdleRaw, s.DebtIdleRaw = 0, 6
			}
			if got := Decide(targeted(s)); got.Reason != hardLTVDeleverReason {
				t.Fatal("risk repayment lost priority", got)
			}
		})
	}
}

func TestUnfundedWithdrawalCannotStarveNAV(t *testing.T) {
	for _, lane := range []string{onreONycUSDC, MapleSyrupUSDCLaneID, autoAUTOPYUSD.Lane} {
		for _, demand := range []int64{100_658_737_179, 200_000_000_000} {
			b := book{voltr: 2_961_393_654, demand: demand, now: bookNow,
				lanes: []*bookLane{{lane: lane, coll: 206_696_625_546, debt: 68_875_562_002}},
				in:    targetInputs{Levels: map[string]laneLevel{lane: {Level: 1.75}}}}
			s, original := b.next()
			if original.Action != DeleverRouteStep || s.Target.Rule != ruleWithdrawal {
				t.Fatalf("fixture does not need a withdrawal sale: %+v", original)
			}
			for _, trustful := range []bool{false, true} {
				s.Trustful = trustful
				for _, age := range []int64{59, 60, 3600, 7200} {
					s.LastReportAgeSeconds = age
					got := Decide(s)
					if age < capitalMutationReportAgeSeconds {
						if got.Action != original.Action || got.MaxSlippageBPS != original.MaxSlippageBPS {
							t.Fatalf("fresh report changed the protected withdrawal: %+v", got)
						}
					} else if got.Action != ReportNAV || got.Reason != "withdrawal_waiting_nav_due" {
						t.Fatalf("%s trustful=%t demand=%d age=%d: %+v", lane, trustful, demand, age, got)
					}
				}
				unpriced := s
				unpriced.net = nil
				if got := Decide(unpriced); got.Action == ReportNAV {
					t.Fatal("unpriced assets reported", got)
				}
				unsafe := s
				unsafe.LTVBPS = 7000
				if got := Decide(unsafe); got.Reason != hardLTVDeleverReason {
					t.Fatal("NAV displaced emergency deleveraging", got)
				}
				pending := s
				pending.Nonterminal, pending.HasAmbiguousSubmission = Signed, true
				if got := Decide(pending); got.Action != RecoverTransaction {
					t.Fatal("NAV displaced signed transaction recovery", got)
				}
			}
		}
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
	in.Quotes[0].Unlevered, in.Quotes[0].BorrowReceiveRaw, in.Quotes[0].BorrowValueRaw, in.Quotes[0].ProceedsValueRaw = true, 0, 0, 0
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
	up, ok := decideLeverageTarget(nil, s, SelectorResult{Action: "KEEP"}, []LaneEconomics{market}, DefaultSelectorPolicy(), twentyBPSStepCost, nil)
	if !ok || up.Next != 1.5 || up.GainRaw <= 0 || up.BorrowRaw == 0 {
		t.Fatalf("profitable pilot-size leverage move blocked: %+v known=%t", up, ok)
	}
}

func TestHourlyFeeIncomeIncludesPeakBeforeEndingLoss(t *testing.T) {
	t.Parallel()
	m := LaneEconomics{NativeAPY: math.Expm1(.10)}
	e := pilotForecastEconomics(pilotEconomics{DebtUSDC: 1_000_000_000, Proceeds: 1_000_000_000, APR: .19}, 1_000_000_000, m, 3, 0)
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
		certifyProjectionFixture(&s, now)
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
