package backyardrwa

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func selectorFixture() SelectorInput {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	s := base()
	s.RouteLane = SelectedRouteID
	s.StrategyKey = s.RouteLane
	s.VoltrIdleRaw = 1_000_000_000
	s.TotalVaultNAVRaw = s.VoltrIdleRaw
	p := DefaultSelectorPolicy()
	p.Persistence = time.Minute
	p.MinimumBenefitRaw = 1
	p.UncertaintyBPS = 0
	market := LaneEconomics{Lane: "OnRe/ONyc/USDC", EvidenceID: "rates", ObservedAt: now, NativeObservedAt: now, NativeAPY: .15, SupplyAPY: 0, CurrentBorrowAPY: .04, BorrowCurve: []BorrowCurvePoint{{0, 400}, {8000, 400}, {10000, 10000}}, DebtSupplyRaw: 1e15, DebtBorrowRaw: 1e14, EntryCapacity: Capacity{Known: true, Unlimited: true}}
	quote := MoveQuote{MinimumIdleRaw: uint64(s.TotalVaultNAVRaw), BorrowReceiveRaw: uint64(s.TotalVaultNAVRaw / 2), SourceLane: s.RouteLane, DestinationLane: market.Lane, ObservationID: s.ObservationID, EquityRaw: s.TotalVaultNAVRaw, CostRaw: 10_000, ObservedAt: now, EvidenceID: "complete-sequence", SampleSlot: s.Slot, ValidThroughSlot: s.Slot + 32}
	return SelectorInput{Now: now, Snapshot: s, Markets: []LaneEconomics{market}, Quotes: []MoveQuote{quote}, Policy: p}
}
func advanceSelectorFixture(in *SelectorInput, d time.Duration) {
	in.Now = in.Now.Add(d)
	for i := range in.Markets {
		in.Markets[i].ObservedAt = in.Now
		in.Markets[i].NativeObservedAt = in.Now
	}
	for i := range in.Quotes {
		in.Quotes[i].ObservedAt = in.Now
	}
}

func TestSelectorDoesNotRefreshOldRecipeWithFreshTimestamp(t *testing.T) {
	for _, kind := range []string{"expired", "future", "unbounded", "missing"} {
		t.Run(kind, func(t *testing.T) {
			in := selectorFixture()
			first := SelectOpportunity(in, SelectorState{})
			advanceSelectorFixture(&in, time.Minute)
			switch kind {
			case "expired":
				in.Snapshot.Slot = in.Quotes[0].ValidThroughSlot + 1
			case "future":
				in.Quotes[0].SampleSlot = in.Snapshot.Slot + 1
			case "unbounded":
				in.Quotes[0].ValidThroughSlot++
			case "missing":
				in.Quotes[0].SampleSlot = 0
			}
			got := SelectOpportunity(in, first.State)
			if got.Action == "ENTER" || got.Action == "SWITCH" || got.SelectedQuote != nil {
				t.Fatal("timestamp refreshed invalid recipe evidence", got)
			}
		})
	}
}
func TestSelectorPersistenceSurvivesCapacityClosureAndJSONRestart(t *testing.T) {
	in := selectorFixture()
	first := SelectOpportunity(in, SelectorState{})
	if first.Action != "KEEP" || first.Reason != "advantage_not_yet_persistent" {
		t.Fatal(first)
	}
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 0}
	advanceSelectorFixture(&in, 30*time.Second)
	closed := SelectOpportunity(in, first.State)
	if closed.Action != "KEEP" || closed.Candidates[0].BlockedReason != "entry_closed" {
		t.Fatal(closed)
	}
	raw, _ := json.Marshal(closed.State)
	var restarted SelectorState
	if err := json.Unmarshal(raw, &restarted); err != nil {
		t.Fatal(err)
	}
	in.Markets[0].EntryCapacity = Capacity{Known: true, Unlimited: true}
	advanceSelectorFixture(&in, 31*time.Second)
	opened := SelectOpportunity(in, restarted)
	if opened.Action != "ENTER" {
		t.Fatal(opened)
	}
	// A long observation gap is not proof of continuous advantage.
	advanceSelectorFixture(&in, 3*time.Minute)
	if got := SelectOpportunity(in, opened.State); got.Action != "KEEP" || got.Reason != "advantage_not_yet_persistent" {
		t.Fatal(got)
	}
}
func TestSelectorPartialCapacityValuesIdleAndDoesNotExitFullCurrentLane(t *testing.T) {
	in := selectorFixture()
	s := &in.Snapshot
	s.VoltrIdleRaw = 0
	s.HasPosition = true
	s.PositionCollateralRaw = 1_500_000_000
	s.PositionDebtRaw = 500_000_000
	s.PositionCollateralValueRaw = s.PositionCollateralRaw
	s.PositionDebtValueRaw = s.PositionDebtRaw
	s.StrategyNAVRaw = 1_000_000_000
	s.PriorReportedNAVRaw = s.StrategyNAVRaw
	s.LTVBPS = 3334
	current := in.Markets[0]
	current.Lane = s.RouteLane
	current.NativeAPY = .10
	current.EntryCapacity = Capacity{Known: true, Raw: 0}
	in.Markets = append(in.Markets, current)
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 100_000_000}
	in.Quotes[0].EquityRaw = 100_000_000
	got := SelectOpportunity(in, SelectorState{})
	if got.Action != "KEEP" || got.KeepGainRaw <= 0 {
		t.Fatal(got)
	}
	for _, c := range got.Candidates {
		if c.Lane == "OnRe/ONyc/USDC" && (c.IdleRaw != 900_000_000 || c.BenefitRaw >= 0) {
			t.Fatal(c)
		}
	}
	// Closing entry to the owned lane does not erase its actual income.
	current.EntryBlockedReason = "entry_closed"
	in.Markets[1] = current
	again := SelectOpportunity(in, SelectorState{})
	if again.KeepGainRaw != got.KeepGainRaw || again.Action != "KEEP" {
		t.Fatal(again)
	}
}
func TestSelectorMissingEvidenceNeverBecomesFreeCapacityOrZeroCosts(t *testing.T) {
	for _, mutate := range []func(*SelectorInput){
		func(i *SelectorInput) { i.Markets[0].EntryCapacity = Capacity{} },
		func(i *SelectorInput) { i.Quotes = nil },
		func(i *SelectorInput) { i.Quotes[0].EquityRaw-- },
		func(i *SelectorInput) { i.Markets[0].NativeAPY = math.NaN() },
		func(i *SelectorInput) { i.Markets[0].NativeObservedAt = i.Now.Add(time.Second) },
		func(i *SelectorInput) { i.Markets[0].BorrowCurve = nil },
		func(i *SelectorInput) { i.Quotes[0].CostRaw = -1 },
	} {
		in := selectorFixture()
		mutate(&in)
		got := SelectOpportunity(in, SelectorState{})
		if got.Action != "KEEP" {
			t.Fatal(got)
		}
	}
}
func TestSelectorPricesOneBorrowPassAtProjectedUtilization(t *testing.T) {
	in := selectorFixture()
	in.Markets[0].DebtSupplyRaw = 2_000_000_000
	in.Markets[0].DebtBorrowRaw = 1_500_000_000
	got := SelectOpportunity(in, SelectorState{})
	if len(got.Candidates) != 1 || got.Candidates[0].BorrowAPR <= .9 || got.Action != "KEEP" {
		t.Fatal(got)
	}
	if singlePassLeverage != 1.5 {
		t.Fatal("selector differs from the executor's one-pass leverage")
	}
}
func TestWithdrawalDemandRemainsTruthfulAcrossTypedLanes(t *testing.T) {
	for _, lane := range selectorLanes {
		s := base()
		s.RouteLane = lane
		s.WithdrawalDemandRaw = 10
		s.VoltrIdleRaw = 10
		s.StrategyNAVRaw = 900
		s.PriorReportedNAVRaw = 900
		s.HasPosition = true
		s.PositionCollateralRaw = 1000
		s.PositionDebtRaw = 100
		s.LTVBPS = 1000
		original := s
		got := Decide(s)
		if got.Action != Hold || got.Reason != "withdrawal_covered" || s != original {
			t.Fatalf("covered %s: %+v", lane, got)
		}
		s.VoltrIdleRaw = 0
		s.SquadsIdleRaw = 40
		got = Decide(s)
		if got.Action != DeleverRouteStep || got.AmountRaw != 1 || got.Reason != "withdrawal_release_repayment_collateral" || s.WithdrawalDemandRaw != 10 {
			t.Fatalf("uncovered %s: %+v", lane, got)
		}
		s = base()
		s.RouteLane = lane
		s.WithdrawalDemandRaw = 10
		s.SquadsIdleRaw = 40
		if got = Decide(s); got.Action != StageSquadsToVoltr || got.AmountRaw != 40 {
			t.Fatal(got)
		}
	}
}
func TestEconomicOutageDoesNotBlockSafetyWithdrawalOrExactRecovery(t *testing.T) {
	in := selectorFixture()
	in.Markets = nil
	s := in.Snapshot
	s.HasPosition = true
	s.PositionDebtRaw = 100
	s.SquadsIdleRaw = 100
	s.LTVBPS = 6000
	if got := Decide(s); got.Action != DeleverRouteStep || got.Reason != "hard_ltv_repay" {
		t.Fatal(got)
	}
	s = in.Snapshot
	s.WithdrawalDemandRaw = s.VoltrIdleRaw + 1
	s.SquadsIdleRaw = 1
	if got := Decide(s); got.Action != StageSquadsToVoltr {
		t.Fatal(got)
	}
	s.Nonterminal = Signed
	s.HasAmbiguousSubmission = true
	if got := Decide(s); got.Action != RecoverTransaction {
		t.Fatal(got)
	}
	in.Snapshot = s
	if got := SelectOpportunity(in, SelectorState{}); got.Action != "KEEP" || got.Reason != "execution_recovery_or_safety_first" {
		t.Fatal(got)
	}
}
func TestCommittedUnwindDoesNotRewriteWithdrawalOrTrustFlatIntent(t *testing.T) {
	s := base()
	s.RouteLane = SelectedRouteID
	s.StrategyKey = s.RouteLane
	s.PositionCollateralRaw = 100
	s.HasPosition = true
	intent := UnwindIntent{SourceLane: s.RouteLane, Reason: "economic_rotation", ObservationID: "admitted", MaxCollateralRaw: 100, MaxDebtRaw: 50, CostBoundRaw: 100, BudgetScope: Phase3GoalID, BudgetFamily: "Maple", EvidenceID: sha256Bytes([]byte("exit")), CreatedAt: time.Now().UTC()}
	if err := applyUnwindIntent(&s, &intent); err != nil {
		t.Fatal(err)
	}
	if got := Decide(s); got.Action != DeleverRouteStep || s.WithdrawalDemandRaw != 0 || unwindComplete(s) {
		t.Fatal(got)
	}
	s.HasPosition = false
	s.PositionCollateralRaw = 0
	s.CollateralIdleRaw = 1
	if unwindComplete(s) {
		t.Fatal("custody residue hidden")
	}
	s.CollateralIdleRaw = 0
	s.PositionDebtRaw = 1
	if unwindComplete(s) {
		t.Fatal("debt residue hidden")
	}
	s.PositionDebtRaw = 0
	s.PriorReportedNAVRaw = 1
	if unwindComplete(s) {
		t.Fatal("stale report hidden")
	}
	s.PriorReportedNAVRaw = 0
	if !unwindComplete(s) {
		t.Fatal("flat reconciled state did not complete")
	}
	s.RouteLane = "OnRe/ONyc/USDC"
	if applyUnwindIntent(&s, &intent) == nil {
		t.Fatal("source changed before reconciliation")
	}
}

func TestSelectorNeverPreemptsExecutableRiskReduction(t *testing.T) {
	in := selectorFixture()
	in.Snapshot.HasPosition = true
	in.Snapshot.PositionDebtRaw = 100
	in.Snapshot.SquadsIdleRaw = 100
	in.Snapshot.LTVBPS = 6000
	got := SelectOpportunity(in, SelectorState{})
	if got.Action != "KEEP" || got.Reason != "execution_recovery_or_safety_first" {
		t.Fatal(got)
	}
}
func TestSelectedFullCustodyExitCannotBeSilentlyClamped(t *testing.T) {
	s := base()
	s.RouteLane = SelectedRouteID
	s.WithdrawalDemandRaw = 1
	s.SquadsIdleRaw = Phase2TransactionCapRaw + 1
	if got := Decide(s); got.Action != HoldManualRecovery || got.Reason != "full_custody_exit_exceeds_transaction_cap" {
		t.Fatal(got)
	}
}

func TestSelectorNAVDoesNotEraseFreshAdvantage(t *testing.T) {
	in := selectorFixture()
	first := SelectOpportunity(in, SelectorState{})
	in.Snapshot.LastReportAgeSeconds = 60
	advanceSelectorFixture(&in, 30*time.Second)
	accounting := SelectOpportunity(in, first.State)
	if accounting.Action != "KEEP" || accounting.Reason != "accounting_first" || !accounting.State.Advantages[in.Markets[0].Lane].Since.Equal(in.Now.Add(-30*time.Second)) {
		t.Fatal(accounting)
	}
	advanceSelectorFixture(&in, 31*time.Second)
	in.Snapshot.LastReportAgeSeconds = 0
	if got := SelectOpportunity(in, accounting.State); got.Action != "ENTER" {
		t.Fatal(got)
	}
	in.Snapshot.RouteLane = "PRIME/USDC"
	in.Quotes[0].SourceLane = in.Snapshot.RouteLane
	if got := SelectOpportunity(in, accounting.State); got.Action != "KEEP" {
		t.Fatal("ownership change retained advantage", got)
	}
}

func TestSelectorPartialAllocationCanBecomePersistent(t *testing.T) {
	in := selectorFixture()
	in.Markets[0].DebtSupplyRaw = 1_000_000_000
	in.Markets[0].DebtBorrowRaw = 900_000_000
	in.Markets[0].BorrowCurve = []BorrowCurvePoint{{0, 100}, {10000, 100}}
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 200_000_000}
	in.Quotes[0].EquityRaw = 200_000_000
	first := SelectOpportunity(in, SelectorState{})
	advanceSelectorFixture(&in, time.Minute)
	got := SelectOpportunity(in, first.State)
	if got.Action != "ENTER" || got.Candidates[0].IdleRaw != 800_000_000 {
		t.Fatal(got)
	}
}

func TestStagedRestoreFinishesAfterWithdrawalDemandChanges(t *testing.T) {
	for _, demand := range []int64{0, 3} {
		s := base()
		s.RouteLane = SelectedRouteID
		s.WithdrawalDemandRaw, s.VoltrIdleRaw = demand, 10
		s.VoltrStrategyIdleRaw, s.StagedAmountRaw, s.StagedAmountKnown = 5, 5, true
		s.PostMutationNAVRequired = true
		if got := Decide(s); got.Action != VoltrRestoreIdle || got.AmountRaw != 5 {
			t.Fatal(got)
		}
		s.StagedAmountKnown = false
		if got := Decide(s); got.Action != HoldManualRecovery || got.Reason != "custody_mismatch" {
			t.Fatal(got)
		}
	}
}

func TestTypedUSDCRepaymentUsesCanonicalExecutionContract(t *testing.T) {
	for _, lane := range []string{"PRIME/USDC", SelectedRouteID, "OnRe/ONyc/USDC"} {
		s := base()
		s.RouteLane = lane
		s.WithdrawalDemandRaw = 1
		s.HasPosition, s.PositionDebtRaw, s.SquadsIdleRaw = true, 5, 5
		s.DebtIdleRaw = 0 // Same USDC account must not be counted twice.
		d := Decide(s)
		action, err := fixedRouteAction(d.Action, lane)
		if err != nil || (lane == RouteID && action != DeleverPrimeUSDCStep) || (lane != RouteID && action != DeleverRouteStep) || d.AmountRaw != 5 {
			t.Fatal(lane, d, action, err)
		}
	}
}

func TestPilotSelectorForecastsOnlyExecutableTrancheAndRetainsWholeVaultIdle(t *testing.T) {
	in := selectorFixture()
	in.Snapshot.PilotActive = true
	// The destination quotes only the bounded executable tranche; the rest of
	// the vault stays idle.
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 10_000_000}
	in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 100_000_000, 100_000_000
	in.Quotes[0].EquityRaw = 10_000_000
	in.Quotes[0].BorrowReceiveRaw = 10_000_000 / 2
	in.Quotes[0].CostRaw = 100
	in.Policy.IdleBufferRaw = 2_000_000
	got := SelectOpportunity(in, SelectorState{})
	c := got.Candidates[0]
	if !c.CostsKnown || c.InvestedRaw != 9_999_900 || c.IdleRaw != 90_000_000 {
		t.Fatal("forecast did not conserve actual deployment, cost and idle principal", got)
	}
	// More idle capital cannot increase productive collateral, borrowing, or
	// modeled destination income. Keep uses the actual source holdings instead.
	in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 20_000_000, 20_000_000
	smaller := SelectOpportunity(in, SelectorState{}).Candidates[0]
	if c.GrossGainRaw != smaller.GrossGainRaw || c.GainRaw != smaller.GainRaw || c.BorrowAPR != smaller.BorrowAPR || smaller.IdleRaw != 10_000_000 {
		t.Fatal(c, smaller)
	}
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 3_000_000}
	in.Quotes[0].EquityRaw = 3_000_000
	in.Quotes[0].BorrowReceiveRaw = 3_000_000 / 2
	partial := SelectOpportunity(in, SelectorState{}).Candidates[0]
	if !partial.CostsKnown || partial.InvestedRaw != 2_999_900 || partial.IdleRaw != 17_000_000 || partial.GainRaw >= c.GainRaw {
		t.Fatal("partial capacity did not reduce deployment", partial)
	}
	in.Policy.IdleBufferRaw = 19_000_000
	in.Quotes[0].EquityRaw = 1_000_000
	in.Quotes[0].BorrowReceiveRaw = 1_000_000 / 2
	buffered := SelectOpportunity(in, SelectorState{}).Candidates[0]
	if !buffered.CostsKnown || buffered.InvestedRaw != 999_900 || buffered.IdleRaw != 19_000_000 {
		t.Fatal("idle buffer omitted from whole-vault forecast", buffered)
	}
	in.Quotes[0].EquityRaw = 20_000_000
	if wrong := SelectOpportunity(in, SelectorState{}).Candidates[0]; wrong.CostsKnown || wrong.BlockedReason != "bounded_move_cost_unavailable" {
		t.Fatal("accepted quote for unexecutable full-vault amount", wrong)
	}
}

func TestPilotSelectorKeepsActualSourceIncomeWhenCandidateTrancheIsSmaller(t *testing.T) {
	in := selectorFixture()
	in.Snapshot.PilotActive = true
	// The destination quotes only the bounded candidate tranche against the
	// larger funded source position.
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 10_000_000}
	in.Snapshot.VoltrIdleRaw = 0
	in.Snapshot.HasPosition = true
	in.Snapshot.PositionCollateralRaw, in.Snapshot.PositionCollateralValueRaw = 150_000_000, 150_000_000
	in.Snapshot.PositionDebtRaw, in.Snapshot.PositionDebtValueRaw = 50_000_000, 50_000_000
	in.Snapshot.StrategyNAVRaw, in.Snapshot.PriorReportedNAVRaw, in.Snapshot.TotalVaultNAVRaw = 100_000_000, 100_000_000, 100_000_000
	in.Snapshot.LTVBPS = 3334
	current := in.Markets[0]
	current.Lane = in.Snapshot.RouteLane
	current.EntryCapacity = Capacity{Known: true}
	in.Markets = append(in.Markets, current)
	in.Quotes[0].EquityRaw, in.Quotes[0].CostRaw = 10_000_000, 100
	in.Quotes[0].BorrowReceiveRaw = 5_000_000
	got := SelectOpportunity(in, SelectorState{})
	if got.Action != "KEEP" || got.KeepGainRaw <= 0 {
		t.Fatal(got)
	}
	for _, c := range got.Candidates {
		if c.Lane != current.Lane && (!c.CostsKnown || c.BenefitRaw >= 0 || c.GainRaw >= got.KeepGainRaw) {
			t.Fatal("source income was clipped to candidate tranche", c, got.KeepGainRaw)
		}
	}
}

func TestPilotSelectorForecastUsesQuotedBorrowInsteadOfLeverageAssumption(t *testing.T) {
	in := selectorFixture()
	in.Snapshot.PilotActive = true
	// The destination quotes exactly the bounded executable tranche.
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 10_000_000}
	in.Quotes[0].EquityRaw = 10_000_000
	in.Quotes[0].BorrowReceiveRaw = 3_000_000
	in.Quotes[0].BorrowFeeRaw = 100
	got := SelectOpportunity(in, SelectorState{}).Candidates[0]
	debt := float64(3_000_100)
	collateral := float64(10_000_000-in.Quotes[0].CostRaw) + debt
	apr, err := projectedBorrowAPR(in.Markets[0], debt)
	if err != nil {
		t.Fatal(err)
	}
	want := forecastGain(collateral, collateral, debt, in.Markets[0], apr, in.Policy.Horizon.Hours()/(365.25*24)) - float64(in.Quotes[0].CostRaw)
	if !got.CostsKnown || got.GainRaw != want {
		t.Fatal("quoted borrow ignored", got, want)
	}
	in.Quotes[0].BorrowReceiveRaw = 0
	if got = SelectOpportunity(in, SelectorState{}).Candidates[0]; got.CostsKnown || got.BlockedReason != "bounded_borrow_unavailable" {
		t.Fatal("missing amount accepted", got)
	}
}

// sameLaneSelectorFixture is a settled, fully funded one-pass Maple position
// with later deposits stranded as Voltr idle. The same reserve pays a fresh,
// strictly larger one-pass entry more than the stale deployed position earns.
func sameLaneSelectorFixture() SelectorInput {
	in := selectorFixture()
	s := &in.Snapshot
	s.PilotActive, s.HasPosition = true, true
	s.VoltrIdleRaw, s.TotalVaultNAVRaw = 90_000_000, 100_000_000
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 15_000_000, 15_000_000
	s.PositionDebtRaw, s.PositionDebtValueRaw = 5_000_000, 5_000_000
	s.StrategyNAVRaw, s.PriorReportedNAVRaw, s.LTVBPS = 10_000_000, 10_000_000, 3334
	current := in.Markets[0]
	current.Lane, current.NativeAPY = s.RouteLane, .10
	in.Markets = []LaneEconomics{current}
	in.Quotes[0].DestinationLane = s.RouteLane
	in.Quotes[0].EquityRaw, in.Quotes[0].CostRaw = 20_000_000, 100
	in.Quotes[0].BorrowReceiveRaw, in.Quotes[0].MinimumIdleRaw = 10_000_000, 99_900_000
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 20_000_000}
	return in
}

func sameLaneSelectorHistory(in SelectorInput) SelectorState {
	return SelectorState{SourceLane: in.Snapshot.RouteLane, Advantages: map[string]AdvantageWindow{in.Snapshot.RouteLane: {Since: in.Now.Add(-2 * time.Minute), LastSample: in.Now.Add(-time.Second)}}}
}

// Eligibility is bound lane-by-lane and position-by-position: only an active
// pilot's settled, positively funded Maple position with idle above the buffer
// may price a same-lane reinvestment. Everything else stays a keep baseline.
func TestSameLaneReinvestmentEligibilityBindings(t *testing.T) {
	in := sameLaneSelectorFixture()
	if !sameLaneReinvestmentEligible(in.Snapshot, in.Policy) {
		t.Fatal("settled funded Maple position with excess idle not eligible", in.Snapshot)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*SelectorInput)
	}{
		{"pilot_inactive", func(i *SelectorInput) { i.Snapshot.PilotActive = false }},
		{"non_maple_lane", func(i *SelectorInput) {
			i.Snapshot.RouteLane, i.Snapshot.StrategyKey = "OnRe/ONyc/USDC", "OnRe/ONyc/USDC"
		}},
		{"zero_position", func(i *SelectorInput) { i.Snapshot.HasPosition = false }},
		{"zero_collateral", func(i *SelectorInput) { i.Snapshot.PositionCollateralRaw, i.Snapshot.PositionCollateralValueRaw = 0, 0 }},
		{"zero_debt", func(i *SelectorInput) { i.Snapshot.PositionDebtRaw, i.Snapshot.PositionDebtValueRaw = 0, 0 }},
		{"zero_nav", func(i *SelectorInput) { i.Snapshot.StrategyNAVRaw, i.Snapshot.PriorReportedNAVRaw = 0, 0 }},
		{"incomplete_tranche", func(i *SelectorInput) { i.Snapshot.SquadsIdleRaw = 5_000_000 }},
		{"idle_at_buffer", func(i *SelectorInput) { i.Policy.IdleBufferRaw = i.Snapshot.VoltrIdleRaw }},
		{"no_idle", func(i *SelectorInput) { i.Snapshot.VoltrIdleRaw = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := in
			tc.mutate(&mutated)
			if sameLaneReinvestmentEligible(mutated.Snapshot, mutated.Policy) {
				t.Fatal("ineligible state admitted same-lane reinvestment", tc.name, mutated.Snapshot)
			}
		})
	}
}

func TestPilotSelectorSwitchesToLargerSameLaneReinvestment(t *testing.T) {
	in := sameLaneSelectorFixture()
	got := SelectOpportunity(in, sameLaneSelectorHistory(in))
	if got.Action != "SWITCH" || got.DestinationLane != in.Snapshot.RouteLane || got.EquityRaw != 19_999_900 || got.SelectedQuote == nil {
		t.Fatal("persistent profitable extra idle did not select the same-lane switch", got)
	}
	if got.SelectedQuote.DestinationLane != in.Snapshot.RouteLane || got.SelectedQuote.EquityRaw != 20_000_000 {
		t.Fatal("selected quote is not the complete same-lane move", got.SelectedQuote)
	}
}

func TestPilotSameLaneSwitchStaysBlockedWithoutStrictReinvestmentCase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*SelectorInput)
		reason string
	}{
		{"no_idle", func(i *SelectorInput) { i.Snapshot.VoltrIdleRaw = 0 }, "current_position_is_keep_baseline"},
		{"buffer_only", func(i *SelectorInput) { i.Policy.IdleBufferRaw = i.Snapshot.VoltrIdleRaw }, "current_position_is_keep_baseline"},
		{"at_cap", func(i *SelectorInput) {
			i.Quotes[0].EquityRaw, i.Markets[0].EntryCapacity.Raw = 10_000_100, 10_000_100
		}, "same_lane_reinvestment_not_larger"},
		{"high_cost", func(i *SelectorInput) { i.Quotes[0].CostRaw = 5_000_000 }, ""},
		{"closed_capacity", func(i *SelectorInput) { i.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 0} }, "entry_closed"},
		{"stale_quote", func(i *SelectorInput) { i.Quotes[0].ObservedAt = i.Now.Add(-31 * time.Second) }, "bounded_move_cost_unavailable"},
		{"incomplete_tranche", func(i *SelectorInput) { i.Snapshot.SquadsIdleRaw = 5_000_000 }, "complete_current_tranche_first"},
		{"withdrawal", func(i *SelectorInput) { i.Snapshot.WithdrawalDemandRaw = 1 }, "withdrawal_unwind_or_accounting_first"},
		{"non_maple_lane", func(i *SelectorInput) {
			i.Snapshot.RouteLane, i.Snapshot.StrategyKey = "OnRe/ONyc/USDC", "OnRe/ONyc/USDC"
			i.Markets[0].Lane = i.Snapshot.RouteLane
			i.Quotes[0].SourceLane, i.Quotes[0].DestinationLane = i.Snapshot.RouteLane, i.Snapshot.RouteLane
		}, "current_position_is_keep_baseline"},
		{"working_ceiling", func(i *SelectorInput) {
			s := &i.Snapshot
			s.PositionCollateralRaw, s.PositionCollateralValueRaw = 150_000_000_000, 150_000_000_000
			s.PositionDebtRaw, s.PositionDebtValueRaw = 50_000_000_000, 50_000_000_000
			s.StrategyNAVRaw, s.PriorReportedNAVRaw = PilotWorkingTrancheCapRaw, PilotWorkingTrancheCapRaw
			s.VoltrIdleRaw, s.TotalVaultNAVRaw = 100_000_000_000, 200_000_000_000
			i.Quotes[0].EquityRaw = PilotWorkingTrancheCapRaw
			i.Quotes[0].BorrowReceiveRaw = uint64(PilotWorkingTrancheCapRaw) / 2
			i.Quotes[0].MinimumIdleRaw = 199_900_000_000
			i.Markets[0].EntryCapacity = Capacity{Known: true, Raw: PilotWorkingTrancheCapRaw}
		}, "same_lane_reinvestment_not_larger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := sameLaneSelectorFixture()
			tc.mutate(&in)
			got := SelectOpportunity(in, sameLaneSelectorHistory(in))
			if got.Action != "KEEP" || got.SelectedQuote != nil {
				t.Fatal("blocked same-lane case still switched", got)
			}
			if tc.reason == "" {
				if got.Reason != "no_worthwhile_executable_move" {
					t.Fatal("high cost did not fail the net-benefit gate", got)
				}
				return
			}
			blocked := false
			for _, c := range got.Candidates {
				if c.BlockedReason == tc.reason {
					blocked = true
				}
			}
			if !blocked && got.Reason != tc.reason {
				t.Fatal("expected gate reason missing", tc.reason, got)
			}
		})
	}
}

func TestFlatMapleUnwindReportsBeforeCompleting(t *testing.T) {
	s := base()
	s.RouteLane = SelectedRouteID
	s.StrategyKey = s.RouteLane
	s.Unwind = true
	// Flat after restore, but the reconciled bridge mutations are unreported:
	// unwindComplete needs !CapitalMutated, so only a report can finish it.
	s.CapitalMutated = true
	if got := Decide(s); got.Action != ReportNAV || got.Reason != "withdrawal_terminal_nav_due" {
		t.Fatal("flat unwind held before its terminal report", got)
	}
	s.CapitalMutated, s.PriorReportedNAVRaw, s.LastReportAgeSeconds = false, 0, 0
	if got := Decide(s); got.Action != Hold || got.Reason != "unwind_complete" || !unwindComplete(s) {
		t.Fatal("reported flat unwind did not complete", got)
	}
}
