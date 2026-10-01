package backyardrwa

import (
	"encoding/binary"
	"math"
	"math/big"
	"testing"
	"time"
)

// A fee-paying forecast must reduce profitable carry without rebating losses.
func TestPerformanceFeeAPYUsesNetProfit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		native, want float64
	}{
		{"profit", .10, .07923034529889078}, {"loss", -.10, -.10}, {"zero", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := LaneEconomics{NativeAPY: tc.native, BorrowCurve: []BorrowCurvePoint{{0, 0}, {10000, 0}}, DebtSupplyRaw: 1e12}
			got, ok := leverageLevelAPY(m, 1, 1_000_000, false)
			if !ok || math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("fee-paying APY=%v ok=%t, want %v", got, ok, tc.want)
			}
		})
	}
}

// Forecast profit is taxed after movement expense, not before. The conservative
// user-benefit gate must not persist or enter a route with only a gross edge.
func TestPerformanceFeeSelectorDoesNotSpendOnGrossOnlyBenefit(t *testing.T) {
	in := selectorFixture()
	in.Policy.Horizon = 365 * 24 * time.Hour
	in.Policy.MinimumBenefitRaw = 90_000_000
	in.Markets[0].NativeAPY = .10
	in.Markets[0].CurrentBorrowAPY = 0
	in.Markets[0].BorrowCurve = []BorrowCurvePoint{{0, 0}, {10000, 0}}
	in.Quotes[0].CostRaw = 40_000_000
	got := SelectOpportunity(in, SelectorState{})
	if len(got.Candidates) != 1 {
		t.Fatalf("candidates=%+v", got)
	}
	// 1.5x carry less $40 expense: about $104 gross, below $90 after fees.
	if got.Candidates[0].GainRaw >= 90_000_000 || got.Candidates[0].BenefitRaw >= 90_000_000 {
		t.Fatalf("gross-only advantage accepted: %+v", got)
	}
	advanceSelectorFixture(&in, time.Minute)
	again := SelectOpportunity(in, got.State)
	if again.Action != "KEEP" || again.SelectedQuote != nil {
		t.Fatalf("fee-unprofitable move became executable: %+v", again)
	}
}

// Feed-level persistence also uses investor benefit; it must not accumulate an
// advantage that fails once the same fee is included in the executable quote.
func TestPerformanceFeeFeedPersistenceUsesInvestorBenefit(t *testing.T) {
	in := selectorFixture()
	in.Policy.Horizon = 365 * 24 * time.Hour
	in.Policy.MinimumBenefitRaw = 130_000_000
	in.Markets[0].NativeAPY = .10
	in.Markets[0].CurrentBorrowAPY = 0
	in.Markets[0].BorrowCurve = []BorrowCurvePoint{{0, 0}, {10000, 0}}
	got := SelectOpportunity(in, SelectorState{})
	if len(got.State.Advantages) != 0 || got.Action != "KEEP" {
		t.Fatalf("pre-fee feed advantage persisted: %+v", got)
	}
}

// The priced 1x window must not persist a pre-fee advantage when the fee-aware
// investor benefit is below the required minimum.
func TestPerformanceFeeUnleveredPersistenceUsesInvestorBenefit(t *testing.T) {
	in := unleveredSwitchFixtureAt(3_000_000, .16)
	in.Policy.MinimumBenefitRaw = 3_000_000
	state := SelectorState{}
	for i := 0; i < 8; i++ {
		got := selectUnlevered(in, state)
		candidate := onreCandidate(t, got)
		if candidate.BenefitRaw >= 3_000_000 || got.Action != "KEEP" || got.SelectedQuote != nil {
			t.Fatalf("gross-only 1x benefit admitted: %+v", got)
		}
		if _, ok := got.State.Advantages[unleveredAdvantageKey(onreONycUSDC)]; ok {
			t.Fatalf("gross-only 1x advantage persisted: %+v", got.State)
		}
		state = got.State
		advanceSelectorFixture(&in, 5*time.Minute)
	}
}

// coherentFeeSelectorFixture runs the real vault decoder/merge before economic
// selection. Unlike the old unarmed model fixtures, it proves the money gate.
func coherentFeeSelectorFixture(t *testing.T, wealth, supply uint64, hwm *big.Int) SelectorInput {
	t.Helper()
	in := selectorFixture()
	in.Snapshot = monitorSnapshot(t, func(accounts []ConfirmedAccount, s *Snapshot) {
		vault := accountAt(accounts, bridgeVoltrVault).Data
		binary.LittleEndian.PutUint64(vault[168:176], wealth)
		binary.LittleEndian.PutUint64(vault[616:624], 0)
		putScaledFraction(vault[624:640], hwm)
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeLPMint).Data[36:44], supply)
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeIdleATA).Data[64:72], wealth)
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 0)
		binary.LittleEndian.PutUint64(accountAt(accounts, kaminoPrimeCustody).Data[64:72], 0)
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyReceipt).Data[104:112], 0)
		for i := range accounts {
			if accounts[i].Address == kaminoPrimeUSDCObligation {
				accounts[i].Owner, accounts[i].Data, accounts[i].Lamports = "", nil, 0
			}
		}
		nav, err := ComputeRouteNAV(s.Slot, accounts, readyWorkerManifest(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := applyRouteNAVSnapshot(s, nav, in.Now); err != nil {
			t.Fatal(err)
		}
		s.RouteLane, s.StrategyKey = SelectedRouteID, SelectedRouteID
		s.VoltrIdleRaw, s.StrategyNAVRaw, s.JournalArmedNAVRaw = int64(wealth), 0, 0
		s.SquadsIdleRaw, s.PrimeIdleRaw, s.CollateralIdleRaw = 0, 0, 0
		s.LastReportAgeSeconds = 0
	})
	in.Quotes[0].ObservationID = in.Snapshot.ObservationID
	in.Quotes[0].EquityRaw, in.Quotes[0].MinimumIdleRaw = int64(wealth), wealth
	in.Quotes[0].BorrowReceiveRaw = wealth / 2
	in.Quotes[0].CostRaw = 0
	in.Quotes[0].SampleSlot, in.Quotes[0].ValidThroughSlot = in.Snapshot.Slot, in.Snapshot.Slot+32
	return in
}

// Pin the program proof's independent holder-wealth literals, not a fee
// simulator which could reproduce the same forecast bug.
func TestPerformanceFeeMoneyBoundCoversPinnedDilutionAndRounding(t *testing.T) {
	for _, tc := range []struct {
		name              string
		gross, holderGain float64
	}{
		{"one report A1100000 S1018519", 100_000, 79_999.489455},
		{"split reports A1100000 S1018880", 100_000, 79_616.834171},
		{"one raw A1000001 S1000001", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := coherentFeeSelectorFixture(t, 1_000_000, 1_000_000, new(big.Int).Lsh(big.NewInt(1), 48))
			in.Snapshot.PilotActive = true
			in.Quotes[0].Unlevered, in.Quotes[0].BorrowReceiveRaw = true, 0
			in.Policy.Horizon = 365 * 24 * time.Hour
			in.Markets[0].NativeAPY = math.Expm1(math.Log1p(tc.gross/1_000_000) * (365.25 / 365))
			got := SelectOpportunity(in, SelectorState{})
			if len(got.Candidates) != 1 || got.Candidates[0].GainRaw > tc.holderGain+1e-6 {
				t.Fatalf("forecast exceeds pinned diluted holder wealth %.6f: %+v", tc.holderGain, got)
			}
			if tc.gross == 1 && (got.SelectedQuote != nil || len(got.State.Advantages) != 0) {
				t.Fatalf("rounding-consumed profit can spend/persist: %+v", got)
			}
		})
	}
}

func TestPerformanceFeeLegacyHWMBlocksOnlyEconomicSelector(t *testing.T) {
	in := coherentFeeSelectorFixture(t, 1_000_000, 1_000_000, new(big.Int).Lsh(big.NewInt(1), 47))
	previous := SelectorState{SourceLane: in.Snapshot.RouteLane, Advantages: map[string]AdvantageWindow{
		in.Markets[0].Lane: {Since: in.Now.Add(-time.Hour), LastSample: in.Now},
	}}
	got := SelectOpportunity(in, previous)
	if got.Action != "KEEP" || got.Reason != "fee_hwm_baseline_unavailable" || len(got.State.Advantages) != 0 || got.SelectedQuote != nil {
		t.Fatalf("old wealth fee cliff reached selection/persistence: %+v", got)
	}
	in.Snapshot.LastReportAgeSeconds = 3600
	if got := Decide(in.Snapshot); got.Action != ReportNAV {
		t.Fatalf("economic HWM gate blocked truthful NAV: %+v", got)
	}
	in.Snapshot.LastReportAgeSeconds = 0
	in.Snapshot.WithdrawalDemandRaw = 1_000_001
	in.Snapshot.SquadsIdleRaw = 1
	if got := Decide(in.Snapshot); got.Action != StageSquadsToVoltr {
		t.Fatalf("economic HWM gate blocked withdrawal: %+v", got)
	}
}

func TestPerformanceFeeBoundReservesRepeatedRoundingAndPendingNAV(t *testing.T) {
	in := coherentFeeSelectorFixture(t, 1_000_000, 1_000_000, new(big.Int).Lsh(big.NewInt(1), 48))
	e := pilotEconomics{Gain: 100_000, PositiveIncome: 100_000, InitialNAV: 1_000_000, EndingNAV: 1_100_000}
	gain, ok := selectorFeeReservedGain(in.Snapshot, 5*time.Second, e, 0)
	// At least 20,000 fee assets grow by up to 1.1, before BOTH integer
	// ceilings over polling + wakes. This is below the split-report proof.
	if !ok || gain <= 0 || gain > 78_000 || gain > 79_616.834171 {
		t.Fatalf("fee reserve is not a usable diluted-holder bound: %.6f ok=%t", gain, ok)
	}
	before := gain
	in.Snapshot.TotalVaultNAVRaw += 10_000 // ordinary unreported gain, not HWM drift
	if !selectorFeeBaselineKnown(in.Snapshot) {
		t.Fatal("fresh NAV gain invalidated coherent BOOK baseline")
	}
	gain, ok = selectorFeeReservedGain(in.Snapshot, 5*time.Second, e, 0)
	if !ok || gain > before-2_000 {
		t.Fatalf("pending NAV fee was not reserved: before=%f after=%f ok=%t", before, gain, ok)
	}
}

func TestPerformanceFeeQ48FloorKnownAndUnknownBaseline(t *testing.T) {
	floor := new(big.Int).Quo(new(big.Int).Lsh(big.NewInt(1_000_000), 48), big.NewInt(3_000_000))
	in := coherentFeeSelectorFixture(t, 1_000_000, 3_000_000, floor)
	if !selectorFeeBaselineKnown(in.Snapshot) {
		t.Fatal("equal rounded Q48 baseline rejected")
	}
	in.Snapshot.TotalVaultNAVRaw++
	if got := SelectOpportunity(in, SelectorState{}); got.Reason == "fee_hwm_baseline_unavailable" {
		t.Fatal("ordinary unreported NAV starved the baseline", got)
	}
	previous := SelectorState{SourceLane: in.Snapshot.RouteLane, Advantages: map[string]AdvantageWindow{
		in.Markets[0].Lane: {Since: in.Now.Add(-time.Hour), LastSample: in.Now},
	}}
	in.Snapshot.VoltrHighWaterMarkKnown = false
	got := SelectOpportunity(in, previous)
	if got.Action != "KEEP" || got.Reason != "fee_hwm_baseline_unavailable" || len(got.State.Advantages) != 0 {
		t.Fatalf("unknown armed HWM could persist/spend: %+v", got)
	}
	// Fixed-array HWM must not break Snapshot's existing immutable comparison.
	before := in.Snapshot
	_ = Decide(in.Snapshot)
	if in.Snapshot != before {
		t.Fatal("decision mutated the HWM baseline")
	}
}

func TestPerformanceFeeReservesHWMPrecisionAtEveryReport(t *testing.T) {
	// Exact baseline: A=2^20, S=2^60, HWM bits=256. Later HWM floors
	// can leave thousands of eligible raw assets even without an old cliff.
	in := coherentFeeSelectorFixture(t, 1<<20, 1<<60, big.NewInt(256))
	e := pilotEconomics{Gain: 1_000, PositiveIncome: 1_000, InitialNAV: 1 << 20, EndingNAV: (1 << 20) + 1_000}
	gain, ok := selectorFeeReservedGain(in.Snapshot, 5*time.Second, e, 0)
	if !ok || gain > -9_000 {
		t.Fatalf("per-report Q48 dust omitted: gain=%f ok=%t", gain, ok)
	}
	// A nonpositive endpoint cannot majorate future fee-LP growth.
	e.EndingNAV = 0
	if _, ok := selectorFeeReservedGain(in.Snapshot, 5*time.Second, e, 0); ok {
		t.Fatal("nonpositive fee-LP growth denominator accepted")
	}
}

func TestPerformanceFeeLeverageHWMKEEPDoesNotRaiseTarget(t *testing.T) {
	in := coherentFeeSelectorFixture(t, 1_000_000_000, 1_000_000_000, new(big.Int).Lsh(big.NewInt(1), 47))
	s := in.Snapshot
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.PilotActive, s.HasPosition, s.LeverageTargetLevel = true, true, 1
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 1_000_000_000, 1_000_000_000
	s.VoltrIdleRaw, s.StrategyNAVRaw, s.PriorReportedNAVRaw = 0, 1_000_000_000, 1_000_000_000
	s.JournalArmedNAVRaw = 1_000_000_000
	in.Snapshot = s
	keep := SelectOpportunity(in, SelectorState{})
	market := leverageMarket(s.RouteLane, .30, math.Log1p(.06))
	market.CurrentBorrowAPY = .06
	got, ok := decideLeverageTarget(s, keep, []LaneEconomics{market}, DefaultSelectorPolicy())
	if !ok || got.Next != 1 || got.Reason != "fee_hwm_baseline_unavailable" {
		t.Fatalf("HWM-blocked KEEP became an economic UP: %+v ok=%t selector=%+v", got, ok, keep)
	}
	for _, unknown := range []bool{false, true} {
		s.LeverageTargetLevel = 1.5
		s.PositionCollateralValueRaw, s.PositionCollateralRaw, s.PositionDebtValueRaw = 1_500_000_000, 1_500_000_000, 500_000_000
		s.VoltrHighWaterMarkKnown = !unknown
		market.NativeAPY = .01
		got, ok = decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{market}, DefaultSelectorPolicy())
		if !ok || got.Next != 1 {
			t.Fatalf("bad/unknown HWM blocked DOWN: %+v ok=%t", got, ok)
		}
	}
}

func TestPerformanceFeeLeverageUPNeedsFeeReservedWholePositionEdge(t *testing.T) {
	in := coherentFeeSelectorFixture(t, 1_000_000_000, 1_000_000_000, new(big.Int).Lsh(big.NewInt(1), 48))
	s := in.Snapshot
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.PilotActive, s.HasPosition, s.LeverageTargetLevel = true, true, 1
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 1_000_000_000, 1_000_000_000
	s.VoltrIdleRaw, s.StrategyNAVRaw, s.PriorReportedNAVRaw, s.JournalArmedNAVRaw = 0, 1_000_000_000, 1_000_000_000, 1_000_000_000
	p := DefaultSelectorPolicy()
	market := leverageMarket(s.RouteLane, .12, math.Log1p(.06))
	market.CurrentBorrowAPY = .06
	got, ok := decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{market}, p)
	// Original gross spread gate passes (~2.46m gain -1.03m cost). The
	// whole-position performance fee + cadence rounding consumes that edge.
	if !ok || got.Next != 1 || got.Reason != "up_move_below_minimum_benefit" {
		t.Fatalf("gross-only leverage edge spent: %+v ok=%t", got, ok)
	}
	market.NativeAPY = .60
	got, ok = decideLeverageTarget(s, SelectorResult{Action: "KEEP"}, []LaneEconomics{market}, p)
	if !ok || got.Next != 1.5 || got.GainRaw <= float64(p.MinimumBenefitRaw) {
		t.Fatalf("proved fee-reserved UP was not supported: %+v ok=%t", got, ok)
	}
}
