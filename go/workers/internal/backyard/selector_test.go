package backyard

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
	quote := parBorrow(MoveQuote{MinimumIdleRaw: uint64(s.TotalVaultNAVRaw), BorrowReceiveRaw: uint64(s.TotalVaultNAVRaw / 2), SourceLane: s.RouteLane, DestinationLane: market.Lane, ObservationID: s.ObservationID, EquityRaw: s.TotalVaultNAVRaw, CostRaw: 10_000, ObservedAt: now, EvidenceID: "complete-sequence", SampleSlot: s.Slot, ValidThroughSlot: s.Slot + 32})
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
	t.Parallel()
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

// Persistence comes only from executable quotes: a capacity closure is no
// executable sample, so it ends the window, and a reopened lane must persist
// afresh. The window itself survives a JSON restart.
func TestSelectorPersistenceRestartsAfterCapacityClosureAndSurvivesJSONRestart(t *testing.T) {
	t.Parallel()
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
	if _, kept := closed.State.Advantages["OnRe/ONyc/USDC"]; kept {
		t.Fatal("a closed lane kept its persistence window", closed.State)
	}
	in.Markets[0].EntryCapacity = Capacity{Known: true, Unlimited: true}
	advanceSelectorFixture(&in, 31*time.Second)
	reopened := SelectOpportunity(in, closed.State)
	if reopened.Action != "KEEP" || reopened.Reason != "advantage_not_yet_persistent" {
		t.Fatal("reopened lane entered on pre-closure persistence", reopened)
	}
	raw, _ := json.Marshal(reopened.State)
	var restarted SelectorState
	if err := json.Unmarshal(raw, &restarted); err != nil {
		t.Fatal(err)
	}
	advanceSelectorFixture(&in, in.Policy.Persistence+time.Second)
	opened := SelectOpportunity(in, restarted)
	if opened.Action != "ENTER" {
		t.Fatal(opened)
	}
	// A long observation gap is not proof of continuous advantage.
	advanceSelectorFixture(&in, in.Policy.MaxSampleGap+time.Minute)
	if got := SelectOpportunity(in, opened.State); got.Action != "KEEP" || got.Reason != "advantage_not_yet_persistent" {
		t.Fatal(got)
	}
}
func TestSelectorMissingEvidenceNeverBecomesFreeCapacityOrZeroCosts(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	for _, lane := range basicLaneIDs() {
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
		s = targeted(s)
		original := s
		got := Decide(s)
		if got.Action != Hold || got.Reason != holdAtTarget || s != original {
			t.Fatalf("covered %s: %+v", lane, got)
		}
		// Squads cash beside a held lane stages before any position moves.
		s.VoltrIdleRaw = 0
		s.SquadsIdleRaw = 40
		if got = Decide(targeted(s)); got.Action != StageSquadsToVoltr || got.AmountRaw != 40 || s.WithdrawalDemandRaw != 10 {
			t.Fatalf("uncovered %s: %+v", lane, got)
		}
		s = base()
		s.RouteLane = lane
		s.WithdrawalDemandRaw = 10
		s.SquadsIdleRaw = 40
		if got = Decide(targeted(s)); got.Action != StageSquadsToVoltr || got.AmountRaw != 40 {
			t.Fatal(got)
		}
	}
}
func TestEconomicOutageDoesNotBlockSafetyWithdrawalOrExactRecovery(t *testing.T) {
	t.Parallel()
	in := selectorFixture()
	in.Markets = nil
	s := in.Snapshot
	s.HasPosition = true
	s.PositionDebtRaw = 100
	s.SquadsIdleRaw = 100
	s.LTVBPS = 7000
	if got := Decide(s); got.Action != DeleverRouteStep || got.Reason != hardLTVDeleverReason {
		t.Fatal(got)
	}
	s = in.Snapshot
	s.WithdrawalDemandRaw = s.VoltrIdleRaw + 1
	s.SquadsIdleRaw = 1
	if got := Decide(targeted(s)); got.Action != StageSquadsToVoltr || got.Reason != outboundStageReason {
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

func TestSelectorNeverPreemptsExecutableRiskReduction(t *testing.T) {
	t.Parallel()
	in := selectorFixture()
	in.Snapshot.HasPosition = true
	in.Snapshot.PositionDebtRaw = 100
	in.Snapshot.SquadsIdleRaw = 100
	in.Snapshot.LTVBPS = 7000
	got := SelectOpportunity(in, SelectorState{})
	if got.Action != "KEEP" || got.SelectedQuote != nil {
		t.Fatal(got)
	}
}

func TestSelectorNAVDoesNotEraseFreshAdvantage(t *testing.T) {
	t.Parallel()
	in := selectorFixture()
	first := SelectOpportunity(in, SelectorState{})
	in.Snapshot.LastReportAgeSeconds = 3600
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
	t.Parallel()
	in := selectorFixture()
	in.Markets[0].DebtSupplyRaw = 1_000_000_000
	in.Markets[0].DebtBorrowRaw = 900_000_000
	in.Markets[0].BorrowCurve = []BorrowCurvePoint{{0, 100}, {10000, 100}}
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 200_000_000}
	in.Quotes[0].EquityRaw, in.Quotes[0].BorrowReceiveRaw = 200_000_000, 100_000_000
	in.Quotes[0] = parBorrow(in.Quotes[0])
	first := SelectOpportunity(in, SelectorState{})
	advanceSelectorFixture(&in, time.Minute)
	got := SelectOpportunity(in, first.State)
	if got.Action != "ENTER" || got.Candidates[0].IdleRaw != 800_000_000 {
		t.Fatal(got)
	}
}

func TestStagedRestoreFinishesAfterWithdrawalDemandChanges(t *testing.T) {
	t.Parallel()
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

func TestPilotSelectorForecastsOnlyExecutableTrancheAndRetainsWholeVaultIdle(t *testing.T) {
	t.Parallel()
	in := selectorFixture()
	// The destination quotes only the bounded executable tranche; the rest of
	// the vault stays idle.
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 10_000_000}
	in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 100_000_000, 100_000_000
	in.Quotes[0].EquityRaw = 10_000_000
	in.Quotes[0].BorrowReceiveRaw = 10_000_000 / 2
	in.Quotes[0] = parBorrow(in.Quotes[0])
	in.Quotes[0].CostRaw = 100
	in.Policy.IdleBufferRaw = 2_000_000
	got := SelectOpportunity(in, SelectorState{})
	c := got.Candidates[0]
	if !c.CostsKnown || c.InvestedRaw != 9_999_900 || c.IdleRaw != 90_000_000 {
		t.Fatal("forecast did not conserve actual deployment, cost and idle principal", got)
	}
	// More idle capital cannot increase productive collateral, borrowing, or
	// gross destination income. The whole-wealth denominator does affect the
	// continuous fee projection; KEEP still uses actual source holdings.
	in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 20_000_000, 20_000_000
	smaller := SelectOpportunity(in, SelectorState{}).Candidates[0]
	if c.GrossGainRaw != smaller.GrossGainRaw || c.GainRaw <= smaller.GainRaw || c.GainRaw-smaller.GainRaw > 100 || c.BorrowAPR != smaller.BorrowAPR || smaller.IdleRaw != 10_000_000 {
		t.Fatal(c, smaller)
	}
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 3_000_000}
	in.Quotes[0].EquityRaw = 3_000_000
	in.Quotes[0].BorrowReceiveRaw = 3_000_000 / 2
	in.Quotes[0] = parBorrow(in.Quotes[0])
	partial := SelectOpportunity(in, SelectorState{}).Candidates[0]
	if !partial.CostsKnown || partial.InvestedRaw != 2_999_900 || partial.IdleRaw != 17_000_000 || partial.GainRaw >= c.GainRaw {
		t.Fatal("partial capacity did not reduce deployment", partial)
	}
	in.Policy.IdleBufferRaw = 19_000_000
	in.Quotes[0].EquityRaw = 1_000_000
	in.Quotes[0].BorrowReceiveRaw = 1_000_000 / 2
	in.Quotes[0] = parBorrow(in.Quotes[0])
	buffered := SelectOpportunity(in, SelectorState{}).Candidates[0]
	if !buffered.CostsKnown || buffered.InvestedRaw != 999_900 || buffered.IdleRaw != 19_000_000 {
		t.Fatal("idle buffer omitted from whole-vault forecast", buffered)
	}
	in.Quotes[0].EquityRaw = 20_000_000
	if wrong := SelectOpportunity(in, SelectorState{}).Candidates[0]; wrong.CostsKnown || wrong.BlockedReason != "bounded_move_cost_unavailable" {
		t.Fatal("accepted quote for unexecutable full-vault amount", wrong)
	}
}

func TestPilotSelectorForecastUsesQuotedBorrowInsteadOfLeverageAssumption(t *testing.T) {
	t.Parallel()
	in := selectorFixture()
	// The destination quotes exactly the bounded executable tranche.
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: 10_000_000}
	in.Quotes[0].EquityRaw = 10_000_000
	in.Quotes[0].BorrowReceiveRaw = 3_000_000
	in.Quotes[0].BorrowFeeRaw = 100
	in.Quotes[0] = parBorrow(in.Quotes[0])
	got := SelectOpportunity(in, SelectorState{}).Candidates[0]
	// The step owes its receive plus the fee; only the receive converts into
	// collateral (the fee is charged in the cost).
	debt := float64(3_000_100)
	collateral := float64(10_000_000-in.Quotes[0].CostRaw) + 3_000_000
	apr, err := projectedBorrowAPR(in.Markets[0], debt)
	if err != nil {
		t.Fatal(err)
	}
	gross := forecastGain(collateral, collateral, debt, in.Markets[0], apr, in.Policy.Horizon.Hours()/(365.25*24)) - float64(in.Quotes[0].CostRaw)
	want := float64(in.Snapshot.TotalVaultNAVRaw) * math.Expm1(.8*math.Log1p(gross/float64(in.Snapshot.TotalVaultNAVRaw)))
	if !got.CostsKnown || got.GainRaw != want {
		t.Fatal("quoted borrow ignored", got, want)
	}
	in.Quotes[0].BorrowReceiveRaw = 0
	if got = SelectOpportunity(in, SelectorState{}).Candidates[0]; got.CostsKnown || got.BlockedReason != "bounded_borrow_unavailable" {
		t.Fatal("missing amount accepted", got)
	}
}
