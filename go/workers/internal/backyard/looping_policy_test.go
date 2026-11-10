package backyard

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestBackyardUnleveredEntryEarnsCostsAndRequiresFreshPersistence(t *testing.T) {
	in := selectorFixture()
	in.Policy = productionLoopingPolicy
	in.Snapshot.ApprovedAdminPerformanceFeeBPS = 0
	in.Snapshot.TotalVaultNAVRaw, in.Snapshot.VoltrIdleRaw = 2_017_000_000, 2_017_000_000
	in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
	m := &in.Markets[0]
	m.Lane, m.NativeAPY = autoAUTOPYUSD.Lane, .10642725789894358
	m.EntryCapacity = Capacity{Known: true, Raw: 333_333_333}
	// Complete production quote from November 9, including its conservative cost.
	q := &in.Quotes[0]
	q.DestinationLane, q.EquityRaw, q.CostRaw = m.Lane, 333_333_333, 5_027_585
	q.MinimumIdleRaw, q.Unlevered = uint64(in.Snapshot.VoltrIdleRaw), true
	q.BorrowReceiveRaw, q.BorrowFeeRaw, q.BorrowValueRaw, q.ProceedsValueRaw = 0, 0, 0, 0
	short := in
	short.Policy.Horizon = productionRWAPolicy.Horizon
	if got := SelectOpportunity(short, SelectorState{}); got.Action != SelectorKeep || got.Candidates[0].BenefitRaw >= 0 {
		t.Fatal("seven-day economics unexpectedly admitted the retained quote", got)
	}
	result := SelectOpportunity(in, SelectorState{})
	if result.Action != SelectorKeep || result.Reason != "advantage_not_yet_persistent" || result.Candidates[0].BenefitRaw <= float64(in.Policy.MinimumBenefitRaw) {
		t.Fatal("longer holding period did not earn costs with fresh persistence", result)
	}
	for elapsed := time.Duration(0); elapsed < in.Policy.Persistence; elapsed += 5 * time.Minute {
		// A restart must retain this policy and its unleveraged advantage window.
		raw, err := json.Marshal(result.State)
		var previous SelectorState
		if err != nil || json.Unmarshal(raw, &previous) != nil {
			t.Fatal("selector state round trip", err)
		}
		advanceSelectorFixture(&in, 5*time.Minute)
		result = SelectOpportunity(in, previous)
	}
	if result.Action != SelectorEnter || result.SelectedQuote == nil || !result.SelectedQuote.Unlevered {
		t.Fatal("profitable persistent unleveraged entry was not selected", result)
	}
	in.Quotes[0].CostRaw = 30_000_000
	if got := SelectOpportunity(in, result.State); got.Action != SelectorKeep {
		t.Fatal("longer horizon bypassed profitability", got)
	}
}

func TestBackyardPolicyQuotesWithoutBorrowingWhenDebtIsAvailable(t *testing.T) {
	for _, unlevered := range []bool{false, true} {
		m, rpc, client, _ := selectorDestinationFixtureForLane(t, onreONycUSDC, nil)
		v := *m.vault
		v.DiversifyNewCash = true
		v.UnleveredNewCash = unlevered
		m.vault = &v
		q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 100_000_000), leveredTestMarket(onreONycUSDC), 100_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if !q.validBorrow() || q.DestinationExit == nil || q.CostRaw <= 0 {
			t.Fatal("entry lost its priced exit or cost evidence", q)
		}
		if unlevered {
			if !q.Unlevered || q.BorrowReceiveRaw != 0 || q.BorrowValueRaw != 0 || q.ProceedsValueRaw != 0 || q.Cost.Step.TotalMicros != 0 {
				t.Fatal("Backyard quote introduced borrowing", q)
			}
		} else if q.Unlevered || q.BorrowReceiveRaw == 0 {
			t.Fatal("Earn MAX's existing leveraged entry changed", q)
		}
	}
}

func TestBackyardLeveragedSizingKeepsGrossExecutionCapAndCashReserve(t *testing.T) {
	for _, original := range []*Vault{rwa, looping} {
		v := *original
		v.ProtectExecution = true
		v.DiversifyNewCash = original == rwa && productionRWAPolicy.DiversifyNewCash
		v.UnleveredNewCash = false
		o, d := executionFixture(t, &v)
		o.Snapshot.Target = LaneTarget{Rule: ruleEntry}
		d.Action, d.StrategyKey, d.AmountRaw = VoltrAllocateToSquads, autoAUTOPYUSD.Lane, executionTradeValueRaw
		_, err := boundedExecutionDecision(&v, o, d)
		if err == nil {
			t.Fatalf("%s entry did not respect its actual borrowing policy: %v", original.Name, err)
		}
		d.AmountRaw = 333_333_333
		if _, err := boundedExecutionDecision(&v, o, d); err != nil {
			t.Fatal("small leveraged entry was refused", err)
		}
		d.AmountRaw = executionTradeValueRaw
		d.AmountRaw++
		if _, err := boundedExecutionDecision(&v, o, d); err == nil {
			t.Fatal("oversized entry escaped the $1,000 execution cap")
		}
	}
	s := base()
	s.TotalVaultNAVRaw, s.VoltrIdleRaw = 2_000_000_000, 2_000_000_000
	s.net = fixtureSnapshotCertificate(s)
	q := autoEntryQuoteFixture(time.Now(), 1_000_000_000)
	q.Unlevered = true
	q.BorrowReceiveRaw, q.BorrowFeeRaw, q.BorrowValueRaw, q.ProceedsValueRaw = 0, 0, 0, 0
	if !newAllocationQuoteAllowed(s, nil, productionLoopingPolicy, q) {
		t.Fatal("Backyard refused a $1,000 entry with sufficient cash and capacity")
	}
	s.WithdrawalDemandRaw = 900_000_001
	s.net = fixtureSnapshotCertificate(s)
	if newAllocationQuoteAllowed(s, nil, productionLoopingPolicy, q) {
		t.Fatal("entry spent the reserve or pending withdrawal cash")
	}
}

func TestBackyardAllowsEconomicBorrowingButKeepsCashAndRiskExits(t *testing.T) {
	s := leverageSnapshot(1)
	s.TotalVaultNAVRaw, s.VoltrIdleRaw = 2_017_000_000, 293_000_000
	s.StrategyNAVRaw = s.TotalVaultNAVRaw - s.VoltrIdleRaw
	s.PositionCollateralValueRaw, s.PositionCollateralRaw = 1_624_000_000, 1_624_000_000
	armFeeAuthorityFixture(t, &s)
	s.AdminPerformanceFeeBPS, s.ApprovedAdminPerformanceFeeBPS = 0, 0
	armLeverageCapacityFixture(&s)
	s.net = fixtureSnapshotCertificate(s)
	lanes := []Snapshot{s}
	applyAllocationLimits(lanes, productionLoopingPolicy)
	s = lanes[0]
	market := leverageMarket(s.RouteLane, .1067, math.Log1p(.0666))
	d, ok := decideLeverageTarget(s, SelectorResult{Action: SelectorKeep}, []LaneEconomics{market}, productionLoopingPolicy, twentyBPSStepCost, nil)
	if !ok || d.BorrowRaw == 0 || d.Next != 1.5 || d.Reason != leverageReasonApproved {
		t.Fatal("Backyard's live-sized position could not take an economic leverage step", d)
	}
	s.LeverageTargetLevel, s.LeverageUpApproved = d.Next, true
	if !allocationBorrowAllowed(s, d.BorrowRaw) {
		t.Fatal("provider allocation limit still prevents borrowing")
	}
	s.VoltrIdleRaw = 201_699_999
	if allocationBorrowAllowed(s, d.BorrowRaw) {
		t.Fatal("borrowing continued with the cash reserve below 10%")
	}
	s = leverageSnapshot(1)
	s.TotalVaultNAVRaw, s.VoltrIdleRaw = 2_017_000_000, 0
	lanes = []Snapshot{s}
	applyAllocationLimits(lanes, productionLoopingPolicy)
	s = lanes[0]
	s.PositionDebtRaw, s.PositionDebtValueRaw = 1_000_000, 1_000_000
	s.LTVBPS = hardLTVBPS(s.LiquidationThresholdBPS, s.MaxLTVBPS)
	if got := Decide(targeted(s)); got.Action != DeleverRouteStep || got.Reason != hardLTVDeleverReason {
		t.Fatal("cash protection prevented emergency debt reduction", got)
	}
	s = livePartialSnapshot()
	lanes = []Snapshot{s}
	applyAllocationLimits(lanes, productionLoopingPolicy)
	if got := Decide(targeted(lanes[0])); got.Action != DeleverRouteStep {
		t.Fatal("Backyard policy prevented withdrawal funding", got)
	}
	s.Nonterminal = Signed
	if got := Decide(targeted(s)); got.Action != RecoverTransaction {
		t.Fatal("Backyard policy bypassed signed-transaction recovery", got)
	}
}

func TestBackyardChoosesNetBenefitWithoutForcedAllocations(t *testing.T) {
	in := smallAllocationFixture()
	in.Policy = productionLoopingPolicy
	in.Quotes[0].EquityRaw, in.Quotes[0].BorrowReceiveRaw = 90_000_000_000, 45_000_000_000
	in.Quotes[0] = parBorrow(in.Quotes[0])
	in.Markets[0].EntryCapacity.Raw = in.Quotes[0].EquityRaw
	in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
	result := SelectOpportunity(in, SelectorState{})
	for elapsed := time.Duration(0); elapsed < in.Policy.Persistence; elapsed += 5 * time.Minute {
		advanceSelectorFixture(&in, 5*time.Minute)
		result = SelectOpportunity(in, result.State)
	}
	if result.Action != SelectorEnter || result.DestinationLane != onreONycUSDC || result.SelectedQuote == nil || result.SelectedQuote.Unlevered || result.SelectedQuote.EquityRaw != 90_000_000_000 {
		t.Fatal("forced small allocations or the provider cap displaced the better executable return", result)
	}
	if !productionRWAPolicy.DiversifyNewCash || productionRWAPolicy.Horizon != 7*24*time.Hour {
		t.Fatal("Backyard policy changed Loyal Earn MAX")
	}
}
