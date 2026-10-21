package backyard

import (
	"context"
	"math/big"
	"testing"
	"time"
)

// blockDebtUtilization puts the lane's debt reserve at its utilization
// limit: 90% configured, 95% borrowed, so KLend refuses new borrowing.
func blockDebtUtilization(t *testing.T, lane string) func([]ConfirmedAccount) {
	return func(accounts []ConfirmedAccount) {
		route, _ := runtimeRoute(lane)
		for i := range accounts {
			if accounts[i].Address == route.Kamino.DebtReserve {
				d := accounts[i].Data
				d[kaminoReserveConfigOffset+645] = 90
				putScaledFraction(d[232:248], new(big.Int).Lsh(big.NewInt(19_000_000_000), 60))
			}
		}
	}
}

// (a) A blocked OnRe debt reserve prices a debt-free 1x entry: the entry
// swap and its sale back, no up step.
func TestBlockedDestinationPricesAnUnleveredEntry(t *testing.T) {
	t.Parallel()
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, onreONycUSDC, blockDebtUtilization(t, onreONycUSDC))
	q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 1_000_000), leveredTestMarket(onreONycUSDC), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Unlevered || q.SpreadUnlevered || q.BorrowReceiveRaw != 0 || q.BorrowFeeRaw != 0 || q.BorrowValueRaw != 0 || q.Cost.Step != (leverageStepCost{}) || q.CostRaw <= 0 || q.CostRaw >= 1_000_000 || !q.validBorrow() {
		t.Fatalf("1x quote %+v", q)
	}
}

// (c) Not blocked: the same lane keeps the leveraged entry exactly; blocked
// Maple, a B2 lane since the registry, takes the 1x path like OnRe.
func TestUnblockedLaneKeepsTheLeveragedEntryAndBlockedMapleTakes1x(t *testing.T) {
	t.Parallel()
	m, rpc, client, _ := selectorDestinationFixtureForLane(t, onreONycUSDC, nil)
	q, err := testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 100_000_000), leveredTestMarket(onreONycUSDC), 100_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if q.Unlevered || q.BorrowReceiveRaw == 0 || q.Cost.Step.TotalMicros <= 0 {
		t.Fatalf("unblocked OnRe lost its leveraged entry: %+v", q)
	}
	m, rpc, client, _ = selectorDestinationFixtureForLane(t, SelectedRouteID, blockDebtUtilization(t, SelectedRouteID))
	q, err = testLanePricer(t, rpc, client, m, 42).priceEntry(context.Background(), flatEntryObservation(42, 1_000_000), leveredTestMarket(SelectedRouteID), 1_000_000)
	if err != nil || !q.Unlevered || q.BorrowReceiveRaw != 0 {
		t.Fatalf("blocked Maple did not price the 1x entry: %+v %v", q, err)
	}
}

// Live shape 2026-09-29: funded debt-free AUTO at 1x (~$1,677, borrowing
// blocked), OnRe at 11.02% vs AUTO 9.48% at 1x, OnRe borrowing blocked too.
func unleveredSwitchFixture(cost int64) SelectorInput { return unleveredSwitchFixtureAt(cost, .1102) }

func unleveredSwitchFixtureAt(cost int64, onreAPY float64) SelectorInput {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s := base()
	s.RouteLane, s.StrategyKey = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw = true, 1_677_000_000, 1_677_000_000
	s.StrategyNAVRaw, s.PriorReportedNAVRaw, s.TotalVaultNAVRaw = 1_677_000_000, 1_677_000_000, 1_677_000_000
	s.BorrowUtilizationBlocked, s.LeverageTargetLevel = true, 1.5
	p := DefaultSelectorPolicy()
	curve := []BorrowCurvePoint{{0, 400}, {9000, 1200}, {10000, 10000}}
	auto := LaneEconomics{Lane: autoAUTOPYUSD.Lane, EvidenceID: "auto", ObservedAt: now, NativeObservedAt: now, NativeAPY: .0948, CurrentBorrowAPY: .12, BorrowCurve: curve, DebtSupplyRaw: 1e12, DebtBorrowRaw: .926e12, EntryCapacity: Capacity{Known: true}}
	onre := LaneEconomics{Lane: onreONycUSDC, EvidenceID: "onre", ObservedAt: now, NativeObservedAt: now, NativeAPY: onreAPY, CurrentBorrowAPY: .12, BorrowCurve: curve, DebtSupplyRaw: 1e12, DebtBorrowRaw: .926e12, EntryCapacity: Capacity{Known: true, Raw: 1_677_000_000}}
	q := MoveQuote{Unlevered: true, MinimumIdleRaw: 1_677_000_000, SourceLane: s.RouteLane, DestinationLane: onreONycUSDC, ObservationID: s.ObservationID, EquityRaw: 1_677_000_000, CostRaw: cost, ObservedAt: now, EvidenceID: "complete-1x-sequence", SampleSlot: s.Slot, ValidThroughSlot: s.Slot + 32}
	return SelectorInput{Now: now, Snapshot: s, Markets: []LaneEconomics{auto, onre}, Quotes: []MoveQuote{q}, Policy: p}
}

func selectUnlevered(in SelectorInput, previous SelectorState) SelectorResult {
	return SelectOpportunity(in, previous)
}

// A 1x quote carrying any borrow is invalid; 1x is AUTO/OnRe only.
func TestUnleveredQuoteShape(t *testing.T) {
	t.Parallel()
	q := unleveredSwitchFixture(1).Quotes[0]
	if !q.validBorrow() {
		t.Fatal("valid 1x quote refused")
	}
	for name, bad := range map[string]func(*MoveQuote){
		"borrow":    func(q *MoveQuote) { q.BorrowReceiveRaw = 1 },
		"fee":       func(q *MoveQuote) { q.BorrowFeeRaw = 1 },
		"exit-only": func(q *MoveQuote) { q.DestinationLane = ethenaUSDePYUSD.Lane },
		"no equity": func(q *MoveQuote) { q.EquityRaw = 0 },
	} {
		c := q
		bad(&c)
		if c.validBorrow() {
			t.Fatalf("%s: invalid 1x quote accepted", name)
		}
	}
}

// (d) A flat vault enters the selector's 1x lane: allocation, swap and
// deposit, then borrow blocked -> the step holds at target; when the pool
// reopens with an approved 1.5x level, B2 levers through leverage_up.
func TestUnleveredEntryLifecycleAndNoLoop(t *testing.T) {
	t.Parallel()
	s := base()
	s.Slot = 42
	s.RouteLane, s.StrategyKey = onreONycUSDC, onreONycUSDC
	s.BorrowUtilizationBlocked = true
	s.CapacityRaw, s.MaxTargetLTVEntryRaw, s.PolicyLimitRaw = 2_000_000_000, 2_000_000_000, int64(strategyTwoBridgeLegCapRaw)
	s.VoltrIdleRaw, s.TotalVaultNAVRaw = 1_677_000_000, 1_677_000_000
	s.MinimumCollateralDepositRaw = 1
	q := entryQuoteFixture(time.Now().UTC(), onreONycUSDC, 1_677_000_000)
	q.Unlevered, q.BorrowReceiveRaw, q.BorrowValueRaw, q.ProceedsValueRaw = true, 0, 0, 0
	in := targetInputs{Entry: &SelectorResult{Action: "ENTER", DestinationLane: onreONycUSDC, SelectedQuote: &q}}
	check := func(action Action, reason string) {
		t.Helper()
		if got := Decide(targetedWith(s, in)); got.Action != action || got.Reason != reason {
			t.Fatalf("want %s %s, got %+v (target %+v)", action, reason, got, targetedWith(s, in).Target)
		}
	}
	check(VoltrAllocateToSquads, entryAllocationReason)
	s.VoltrIdleRaw, s.SquadsIdleRaw = 0, 1_677_000_000
	check(SwapStableToCollateralStep, entrySwapReason)
	s.SquadsIdleRaw, s.CollateralIdleRaw = 0, 1_670_000_000
	check(OpenRouteStep, entryDepositReason)
	s.CollateralIdleRaw = 0
	s.HasPosition, s.PositionCollateralRaw, s.PositionCollateralValueRaw, s.StrategyNAVRaw = true, 1_670_000_000, 1_670_000_000, 1_670_000_000
	check(Hold, holdAtTarget)
	s.LeverageTargetLevel = 1
	check(Hold, holdAtTarget)
	// Pool reopens with an approved 1.5x level: B2 levers through leverage_up.
	s.BorrowUtilizationBlocked, s.LeverageTargetLevel = false, 1.5
	armLeverageCapacityFixture(&s)
	check(OpenRouteStep, leverageUpReason)
}
