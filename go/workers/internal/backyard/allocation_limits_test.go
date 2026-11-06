package backyard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestProviderAdmissionCountsGrossExposureAndSharedCustodyOnce(t *testing.T) {
	const usd = int64(1_000_000)
	lanes := []Snapshot{
		{RouteLane: autoAUTOPYUSDTemplate.Lane, PositionCollateralValueRaw: 30_000 * usd, PositionDebtValueRaw: 20_000 * usd, CollateralIdleValueRaw: 3_000 * usd},
		{RouteLane: autoAUTOWYLDSTemplate.Lane, PositionCollateralValueRaw: 10_000 * usd, PositionDebtValueRaw: 7_000 * usd, CollateralIdleValueRaw: 3_000 * usd},
		{RouteLane: primePRIMEPYUSDTemplate.Lane, PositionCollateralValueRaw: 5_000 * usd},
		{RouteLane: onreONycUSDC, PositionCollateralValueRaw: 70_000 * usd, PositionDebtValueRaw: 50_000 * usd},
	}
	s := Snapshot{Slot: 77, TotalVaultNAVRaw: 100_000 * usd}
	s.net = fixtureSnapshotCertificate(s)
	for _, l := range lanes {
		s.HeldLanes += l.RouteLane + ","
	}
	s.HeldLanes = strings.TrimSuffix(s.HeldLanes, ",")
	s.net = fixtureSnapshotCertificate(s)
	for lane, want := range map[string]int64{primePRIMEUSDCTemplate.Lane: 52_000 * usd, autoAUTOPYUSDTemplate.Lane: 52_000 * usd, MapleSyrupUSDCLaneID: 100_000 * usd, onreONycUSDC: 30_000 * usd} {
		if got := newAllocationLimits(s, lanes, productionRWAPolicy, lane).ProviderRoomRaw; got != want {
			t.Fatalf("%s room %d want %d", lane, got, want)
		}
	}
	for lane, want := range map[string]int64{primePRIMEUSDCTemplate.Lane: 0, MapleSyrupUSDCLaneID: 4_500 * usd, onreONycUSDC: 45_000 * usd, autoAUTOPYUSDTemplate.Lane: 45_000 * usd} {
		if got := newAllocationLimits(s, lanes, productionRWAPolicy, lane).EquityRoomRaw; got != want {
			t.Fatalf("%s equity room %d want %d", lane, got, want)
		}
	}
	if room := newAllocationLimits(s, lanes[:3], productionRWAPolicy, MapleSyrupUSDCLaneID).ProviderRoomRaw; room != 0 {
		t.Fatal("incomplete holdings admitted new money", room)
	}
	lanes[0].PositionCollateralValueRaw = -1
	if room := newAllocationLimits(s, lanes, productionRWAPolicy, MapleSyrupUSDCLaneID).ProviderRoomRaw; room != 0 {
		t.Fatal("invalid holdings admitted new money", room)
	}
}

func TestNewCashPolicyUsesUnborrowedPrimeAndMapleWithTightExits(t *testing.T) {
	for _, lane := range []string{MapleSyrupUSDCLaneID, primePRIMEUSDCTemplate.Lane} {
		t.Run(lane, func(t *testing.T) {
			m, rpc, client, _ := selectorDestinationFixtureForLane(t, lane, nil)
			v := *m.vault
			v.DiversifyNewCash = true
			m.vault = &v
			q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 100_000_000), leveredTestMarket(lane), 100_000_000)
			if err != nil {
				t.Fatal(err)
			}
			if !q.Unlevered || !q.validBorrow() || q.BorrowReceiveRaw != 0 || q.ProceedsValueRaw != 0 || q.Cost.Step.TotalMicros != 0 || q.DestinationExit == nil {
				t.Fatalf("policy entered a borrow loop or lacked a priced exit: %+v", q)
			}
		})
	}
}

func TestPrimeAllocationCombinesDebtLanesAndProtectsPendingWithdrawals(t *testing.T) {
	const usd = int64(1_000_000)
	s := Snapshot{TotalVaultNAVRaw: 100_000 * usd, WithdrawalDemandRaw: 10_000 * usd,
		HeldLanes: primePRIMEUSDCTemplate.Lane + "," + primePRIMEPYUSDTemplate.Lane}
	s.Slot = 77
	s.net = fixtureSnapshotCertificate(s)
	lanes := []Snapshot{
		{RouteLane: primePRIMEUSDCTemplate.Lane, PositionCollateralValueRaw: 2_000 * usd, PositionDebtValueRaw: 1_000 * usd, CollateralIdleValueRaw: 200 * usd},
		{RouteLane: primePRIMEPYUSDTemplate.Lane, PositionCollateralValueRaw: 3_000 * usd, PositionDebtValueRaw: 500 * usd, CollateralIdleValueRaw: 200 * usd},
	}
	for _, destination := range []string{primePRIMEUSDCTemplate.Lane, primePRIMEPYUSDTemplate.Lane, primePRIMEUSDSTemplate.Lane} {
		limits := newAllocationLimits(s, lanes, productionRWAPolicy, destination)
		if limits.EquityRoomRaw != 300*usd || limits.ProviderRoomRaw != 94_800*usd {
			t.Fatal("PRIME target duplicated custody, included debt or ignored withdrawal cash", limits)
		}
	}
	for _, mutate := range []func(){
		func() { lanes = append(lanes, lanes[0]) },
		func() { lanes = lanes[:1] },
		func() { lanes[0].PositionDebtValueRaw = -1 },
	} {
		mutate()
		limits := newAllocationLimits(s, lanes, productionRWAPolicy, primePRIMEUSDCTemplate.Lane)
		if limits.EquityRoomRaw != 0 || limits.ProviderRoomRaw != 0 {
			t.Fatal("uncertain holdings admitted capital", limits)
		}
	}
}

func TestNewCashAdmissionRechecksCashAndProviderAfterRestart(t *testing.T) {
	const usd = int64(1_000_000)
	s := base()
	s.RouteLane, s.StrategyKey, s.Slot = MapleSyrupUSDCLaneID, MapleSyrupUSDCLaneID, 42
	s.TotalVaultNAVRaw, s.VoltrIdleRaw, s.WithdrawalDemandRaw = 100_000*usd, 20_000*usd, 5_000*usd
	s.CapacityRaw, s.PolicyLimitRaw, s.MaxTargetLTVEntryRaw = 100_000*usd, 100_000*usd, 100_000*usd
	s.MinimumCollateralDepositRaw = 1
	q := entryQuoteFixture(time.Now().UTC(), s.RouteLane, 10_000*usd)
	q.Unlevered, q.BorrowReceiveRaw, q.BorrowValueRaw, q.ProceedsValueRaw = true, 0, 0, 0
	in := targetInputs{Entry: &SelectorResult{Action: SelectorEnter, DestinationLane: s.RouteLane, SelectedQuote: &q}}
	decide := func() Decision {
		s.net = fixtureSnapshotCertificate(s)
		views := []Snapshot{s}
		applyAllocationLimits(views, productionRWAPolicy)
		raw, _ := json.Marshal(views[0])
		var restarted Snapshot
		if err := json.Unmarshal(raw, &restarted); err != nil {
			t.Fatal(err)
		}
		restarted.net = fixtureSnapshotCertificate(restarted)
		return Decide(targetedWith(restarted, in))
	}
	if d := decide(); d.Action != Hold {
		t.Fatal("old allocation spent cash reserved for withdrawals", d)
	}
	q.EquityRaw = 4_250 * usd // 5% of NAV after the reserve and pending withdrawal.
	if d := decide(); d.Action != VoltrAllocateToSquads || d.AmountRaw != q.EquityRaw {
		t.Fatal("remaining investable cash was refused", d)
	}
	// A withdrawal arrives after the allocation moved into shared custody.
	s.VoltrIdleRaw, s.SquadsIdleRaw = 5_000*usd, 10_000*usd
	if d := decide(); d.Action != StageSquadsToVoltr || d.AmountRaw != 10_000*usd {
		t.Fatal("allocated cash was not returned to fund the reserve", d)
	}
	s.VoltrIdleRaw, s.SquadsIdleRaw = 20_000*usd, 0
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 51_000*usd, 51_000*usd
	s.HeldLanes = s.RouteLane
	if d := decide(); d.Action != Hold {
		t.Fatal("over-limit holding caused an addition or forced sale", d)
	}
}

func TestNewCashCapBlocksBorrowingWithoutResettingHeldTarget(t *testing.T) {
	s := leverageSnapshot(1.5)
	s.LeverageTargetLevel, s.LeverageUpApproved = 1.75, true
	s.VoltrIdleRaw = 20_000_000_000
	s.NewAllocation = &allocationLimits{CashReserveRaw: 10_000_000_000}
	armLeverageCapacityFixture(&s)
	market := leverageMarket(s.RouteLane, .20, math.Log1p(.01))
	d, ok := decideLeverageTarget(s, SelectorResult{Action: SelectorKeep}, []LaneEconomics{market}, productionRWAPolicy, twentyBPSStepCost, nil)
	if !ok || d.Next != 1.75 || d.BorrowRaw != 0 || d.Down.MaxCostBPS != 0 {
		t.Fatal("cap rewrote the existing leverage target", d)
	}
	if got := Decide(targeted(s)); got.Action != Hold {
		t.Fatal("saved up approval bypassed the cap", got)
	}
	s.LTVBPS = hardLTVBPS(s.LiquidationThresholdBPS, s.MaxLTVBPS)
	if got := Decide(targeted(s)); got.Action != DeleverRouteStep || got.Reason != hardLTVDeleverReason {
		t.Fatal("admission cap prevented emergency risk reduction", got)
	}
	s = livePartialSnapshot()
	s.NewAllocation = &allocationLimits{ProviderRoomRaw: 0, CashReserveRaw: s.TotalVaultNAVRaw}
	if got := Decide(targeted(s)); got.Action != DeleverRouteStep {
		t.Fatal("admission cap prevented withdrawal", got)
	}
	s.Nonterminal = Signed
	if got := Decide(targeted(s)); got.Action != RecoverTransaction {
		t.Fatal("admission cap bypassed signed recovery", got)
	}
}

func TestNewCashProviderCapChecksFreshSwapQuote(t *testing.T) {
	m, _, client, accounts := selectorDestinationFixtureForLane(t, MapleSyrupUSDCLaneID, nil)
	binary.LittleEndian.PutUint64(accountAt(accounts, m.vault.cash).Data[64:72], 20_000)
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
	o := flatEntryObservation(42, 20_000)
	d := Decision{Action: SwapStableToCollateralStep, StrategyKey: MapleSyrupUSDCLaneID, AmountRaw: 20_000}
	o.routeBatch = &routeObservationBatch{Accounts: accounts}
	o.Snapshot.NewAllocation = &allocationLimits{ProviderRoomRaw: math.MaxInt64, EquityRoomRaw: math.MaxInt64}
	_, accepted, err := prepareJupiterFromTickObservation(context.Background(), rpc, m, d, client, o)
	if err != nil || accepted.Request.QuotedOutputRaw == 0 {
		t.Fatal("affordable quote refused", err)
	}
	o.Snapshot.NewAllocation.ProviderRoomRaw = 1
	_, _, err = prepareJupiterFromTickObservation(context.Background(), rpc, m, d, client, o)
	var hold *PreSendHold
	if !errors.As(err, &hold) || holdCause(hold) != "new_allocation_limit" {
		t.Fatal("fresh quote crossed provider cap", err)
	}
	o.Snapshot.NewAllocation.ProviderRoomRaw, o.Snapshot.NewAllocation.EquityRoomRaw = math.MaxInt64, 1
	_, _, err = prepareJupiterFromTickObservation(context.Background(), rpc, m, d, client, o)
	if !errors.As(err, &hold) || holdCause(hold) != "new_allocation_limit" {
		t.Fatal("fresh quote crossed allocation target", err)
	}
}

func TestNewCashSelectorRejectsExcessExposureAndRotations(t *testing.T) {
	in := selectorFixture()
	in.Policy.DiversifyNewCash = true
	q := &in.Quotes[0]
	// Three quarters of NAV in equity plus borrowing exceeds the gross cap.
	q.EquityRaw = in.Snapshot.TotalVaultNAVRaw * 3 / 4
	in.Markets[0].EntryCapacity = Capacity{Known: true, Raw: q.EquityRaw}
	q.BorrowReceiveRaw = uint64(q.EquityRaw / 2)
	*q = parBorrow(*q)
	if in.Snapshot.net != nil {
		in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
	}
	got := SelectOpportunity(in, SelectorState{})
	if got.Action != SelectorKeep || got.Candidates[0].BlockedReason != "new_allocation_limit" {
		t.Fatal("gross exposure ignored borrowed capital", got)
	}
	q.EquityRaw = in.Snapshot.TotalVaultNAVRaw / 4
	in.Markets[0].EntryCapacity.Raw = q.EquityRaw
	q.BorrowReceiveRaw = uint64(q.EquityRaw / 2)
	*q = parBorrow(*q)
	if in.Snapshot.net != nil {
		in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
	}
	got = SelectOpportunity(in, SelectorState{})
	advanceSelectorFixture(&in, in.Policy.Persistence)
	if in.Snapshot.net != nil {
		in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
	}
	got = SelectOpportunity(in, got.State)
	if got.Action != SelectorEnter {
		t.Fatal("eligible gradual entry was refused", got)
	}
	q.RotateFrom = in.Snapshot.RouteLane
	in.Snapshot.HeldLanes = q.RotateFrom
	in.Snapshot.HasPosition = true
	in.Snapshot.PositionCollateralRaw, in.Snapshot.PositionCollateralValueRaw = 1_000_000_000, 1_000_000_000
	in.Lanes = []Snapshot{in.Snapshot}
	if in.Snapshot.net != nil {
		in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
	}
	got = SelectOpportunity(in, got.State)
	if got.Action != SelectorKeep || got.SelectedQuote != nil {
		t.Fatal("new cash policy sold an existing holding to diversify", got)
	}
}

func smallAllocationFixture() SelectorInput {
	in := selectorFixture()
	in.Policy = productionRWAPolicy
	in.Policy.Persistence = time.Minute
	in.Snapshot.VoltrIdleRaw, in.Snapshot.TotalVaultNAVRaw = 100_000_000_000, 100_000_000_000
	market, quote := in.Markets[0], in.Quotes[0]
	in.Markets, in.Quotes = nil, nil
	for _, lane := range []string{onreONycUSDC, MapleSyrupUSDCLaneID, primePRIMEUSDCTemplate.Lane} {
		m, q := market, quote
		m.Lane, q.DestinationLane = lane, lane
		q.MinimumIdleRaw, q.EquityRaw = uint64(in.Snapshot.VoltrIdleRaw), 50_000_000_000
		q.BorrowReceiveRaw = uint64(q.EquityRaw / 2)
		if lane != onreONycUSDC {
			q.Unlevered, q.BorrowReceiveRaw = true, 0
			q.EquityRaw = 4_500_000_000
			m.NativeAPY = .08
			if lane == primePRIMEUSDCTemplate.Lane {
				m.NativeAPY = .09
			}
		}
		m.EntryCapacity = Capacity{Known: true, Raw: q.EquityRaw}
		in.Markets, in.Quotes = append(in.Markets, m), append(in.Quotes, parBorrow(q))
	}
	return in
}

func TestSmallAllocationsFillTargetsThenPreferCoreAndSurviveRestart(t *testing.T) {
	in := smallAllocationFixture()
	if in.Snapshot.net != nil {
		in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
	}
	result := SelectOpportunity(in, SelectorState{})
	if result.Action != SelectorKeep || result.DestinationLane != primePRIMEUSDCTemplate.Lane {
		t.Fatal("small target did not require its own persistence window", result)
	}
	raw, err := json.Marshal(result.State)
	var previous SelectorState
	if err != nil || json.Unmarshal(raw, &previous) != nil {
		t.Fatal("restart", err)
	}
	advanceSelectorFixture(&in, time.Minute)
	for _, want := range []string{primePRIMEUSDCTemplate.Lane, MapleSyrupUSDCLaneID, onreONycUSDC} {
		if in.Snapshot.net != nil {
			in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
		}
		result = SelectOpportunity(in, previous)
		if result.Action != SelectorEnter || result.DestinationLane != want || result.SelectedQuote == nil {
			t.Fatalf("wanted %s, got %+v", want, result)
		}
		if want == onreONycUSDC {
			break
		}
		if result.SelectedQuote.EquityRaw != 4_500_000_000 || !result.SelectedQuote.Unlevered {
			t.Fatal("small target exceeded 5% or used borrowing", result.SelectedQuote)
		}
		var core, small float64
		for _, c := range result.Candidates {
			if c.Lane == onreONycUSDC {
				core = c.BenefitRaw
			}
			if c.Lane == want {
				small = c.BenefitRaw
			}
		}
		if core <= small || small <= float64(in.Policy.MinimumBenefitRaw) {
			t.Fatal("priority did not choose a profitable small target over a larger gain", core, small)
		}
		in.Lanes = append(in.Lanes, Snapshot{RouteLane: want, HasPosition: true, PositionCollateralValueRaw: 4_500_000_000})
		in.Snapshot.VoltrIdleRaw -= 4_500_000_000
		if in.Snapshot.HeldLanes != "" {
			in.Snapshot.HeldLanes += ","
		}
		in.Snapshot.HeldLanes += want
		previous = result.State
		advanceSelectorFixture(&in, time.Second)
	}
}

func TestSmallAllocationPriorityNeverBypassesProfitabilityOrTarget(t *testing.T) {
	in := smallAllocationFixture()
	in.Markets[1].NativeAPY = .05 // Seven days' yield is below even the 10 bps allowance.
	in.Quotes[2].CostRaw = 100_000_000
	if in.Snapshot.net != nil {
		in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
	}
	first := SelectOpportunity(in, SelectorState{})
	advanceSelectorFixture(&in, time.Minute)
	if in.Snapshot.net != nil {
		in.Snapshot.net = fixtureSnapshotCertificate(in.Snapshot)
	}
	got := SelectOpportunity(in, first.State)
	if got.Action != SelectorEnter || got.DestinationLane != onreONycUSDC {
		t.Fatal("diversification forced an uneconomic entry", got)
	}
	q := in.Quotes[1]
	q.EquityRaw++
	if newAllocationQuoteAllowed(in.Snapshot, in.Lanes, in.Policy, q) {
		t.Fatal("an entry above the 5% target was admitted")
	}
	// The core bucket counts OnRe and AUTO net of debt, including shared custody once.
	in.Lanes = []Snapshot{
		{RouteLane: onreONycUSDC, PositionCollateralValueRaw: 90_000_000_000, PositionDebtValueRaw: 30_000_000_000},
		{RouteLane: autoAUTOPYUSDTemplate.Lane, PositionCollateralValueRaw: 30_000_000_000, PositionDebtValueRaw: 9_000_000_000},
	}
	limits := newAllocationLimits(in.Snapshot, in.Lanes, in.Policy, onreONycUSDC)
	if limits.EquityRoomRaw != 0 || limits.ProviderRoomRaw != 10_000_000_000 || limits.accepts(in.Quotes[0]) {
		t.Fatal("core exceeded 90% despite unused gross provider room", limits)
	}
}
