package backyard

import (
	"encoding/binary"
	"math/big"
	"testing"
	"time"
)

// overflowSource is the live Oct 11 shape scaled to $100k of equity:
// AUTO/AUTO/PYUSD at 1.5x (collateral $150k, debt $50k) under a stored 1.75x
// target, the PYUSD reserve without room for the minimum borrow, and the
// wYLDS reserve with $5k of room. A settled lone held lane.
func overflowSource() Snapshot {
	s := base()
	s.Slot, s.ObservationID = 455_000_000, "pyusd-no-room"
	s.RouteLane, s.StrategyKey, s.HeldLanes = autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane, autoAUTOPYUSD.Lane
	s.HasPosition, s.ObligationPresent, s.ObligationPresenceKnown = true, true, true
	s.PositionCollateralRaw, s.PositionCollateralValueRaw = 150_000_000_000, 150_000_000_000
	s.PositionDebtRaw, s.PositionDebtValueRaw, s.PayoffDebtRaw = 50_000_000_000, 50_000_000_000, 50_000_000_000
	s.LTVBPS = leverageLevelLTVBPS(1.5)
	s.StrategyNAVRaw, s.TotalVaultNAVRaw, s.PriorReportedNAVRaw = 100_000_000_000, 100_000_000_000, 100_000_000_000
	s.LeverageTargetLevel = 1.75
	armLeverageCapacityFixture(&s)
	// PYUSD: no room, so no capacity-sized step (the approval stands).
	s.AdditionalDebtRoomRaw, s.LeverageBorrow150Raw, s.LeverageBorrow175Raw = 0, 0, 0
	s.OverflowRoomRaw, s.OverflowRoomValueRaw, s.OverflowRoomKnown = 5_000_000_000, 5_000_000_000, true
	return s
}

func overflowMarkets(now time.Time) []LaneEconomics {
	wylds := trancheMarket(autoAUTOWYLDSLane, 0.12, 0.0583, now)
	wylds.DebtRawPerUSDCRaw = 1
	return []LaneEconomics{trancheMarket(autoAUTOPYUSD.Lane, 0.12, 0.09, now), wylds}
}

// PYUSD shut and wYLDS open: the overflow tranche frees E of AUTO from the
// PYUSD obligation with no repay (PYUSD heads to its own 1.75x target), hands
// the shared custody's AUTO to the wYLDS lane, which initializes its
// obligation, deposits the AUTO and then levers with leverage_up. E is what
// wYLDS's $5k of room levers to 1.75x.
func TestOverflowMovesAUTOIntoTheWYLDSLaneWhenPYUSDHasNoRoom(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	policy := DefaultSelectorPolicy()
	source := overflowSource()
	in := SelectorInput{Now: now, Snapshot: source, Markets: overflowMarkets(now), Policy: policy}
	result, tranche := applyTrancheSelection(in, SelectorResult{Action: "KEEP"}, nil, nil, 2, now)
	const e = int64(6_666_666_666) // floor($5k / 0.75)
	if tranche == nil || result.Action != "TRANCHE" || result.Reason != "overflow_tranche" || tranche.Source != autoAUTOPYUSD.Lane ||
		tranche.Destination != autoAUTOWYLDSLane || tranche.EquityRaw != e || tranche.TargetIdleRaw != 0 || !tranche.overflow() {
		t.Fatalf("no overflow tranche: %+v %+v", result, tranche)
	}
	for _, c := range result.Tranches {
		if c.Destination == autoAUTOWYLDSLane && c.BlockedReason != overflowLaneBlockedReason {
			t.Fatalf("the overflow lane was an economic tranche destination: %+v", c)
		}
	}

	// Source half: one release of E's collateral, no repay; the release is
	// the partial planner's own leg (no debt-clear confirmation) and its
	// recorded LTV is the 1.75x level.
	freeing := source
	applyTranche(&freeing, tranche)
	d := Decide(freeing)
	if d.Action != DeleverRouteStep || d.Reason != partialReleaseReason || d.AmountRaw != e || d.StrategyKey != autoAUTOPYUSD.Lane {
		t.Fatalf("source half did not release E: %+v", d)
	}
	if a, r, n, ok := partialWithdrawalStep(freeing); !ok || a != d.Action || r != d.Reason || n != d.AmountRaw {
		t.Fatal("the overflow release is not the partial planner's leg")
	}
	if wire, sized := partialWithdrawalWireAmount(freeing, d); !sized || wire != e {
		t.Fatalf("release wire %d, want %d receipts (one receipt = $1 here)", wire, e)
	}
	if target, ok := partialWithdrawalTargetLTVBPS(freeing); !ok || target != leverageLevelLTVBPS(1.75) {
		t.Fatalf("overflow release LTV %d", target)
	}
	if trancheTransition(*tranche, freeing, now) != trancheStay {
		t.Fatal("handoff before the release")
	}
	// Released: the AUTO waits in the shared custody; PYUSD keeps its debt
	// and sits between 1.5x and 1.75x.
	freed := freeing
	freed.PositionCollateralRaw -= e
	freed.PositionCollateralValueRaw -= e
	freed.CollateralIdleRaw, freed.CollateralIdleValueRaw = e, e
	freed.LTVBPS = int64(50_000_000_000 * 10_000 / freed.PositionCollateralValueRaw)
	freed.PartialWithdrawalOperationID, freed.PartialWithdrawalLTVBPS = "release", leverageLevelLTVBPS(1.75)
	if freed.PositionDebtRaw != source.PositionDebtRaw || freed.LTVBPS > leverageLevelLTVBPS(1.75) {
		t.Fatalf("source repaid or passed its target: %+v", freed)
	}
	if got := Decide(freed); got.Action != Hold || got.Reason != "overflow_source_freed" {
		t.Fatalf("freed source did not wait for the handoff: %+v", got)
	}
	if trancheTransition(*tranche, freed, now) != trancheHandoff {
		t.Fatal("freed source did not hand over")
	}
	handed := *tranche
	handed.SourceFreed = true

	// Destination half: the wYLDS lane's view owns the shared custody's AUTO.
	dest := base()
	dest.Slot, dest.ObservationID = source.Slot+50, "wylds-flat"
	dest.RouteLane, dest.StrategyKey, dest.HeldLanes = autoAUTOWYLDSLane, autoAUTOWYLDSLane, autoAUTOPYUSD.Lane
	dest.ObligationPresenceKnown, dest.ObligationPresent = true, false
	dest.CollateralIdleRaw, dest.CollateralIdleValueRaw = e, e
	dest.StrategyNAVRaw, dest.PriorReportedNAVRaw, dest.OtherLanesNAVRaw = 100_000_000_000, 100_000_000_000, 100_000_000_000-e
	dest.TotalVaultNAVRaw = 100_000_000_000
	dest.SelectorEntryPaused = true // no selector entry authorizes it
	applyTranche(&dest, &handed)
	if !dest.TrancheOverflowDestination || dest.TrancheDestinationRaw != e || dest.TrancheOverflowRaw != 0 {
		t.Fatalf("destination view: %+v", dest)
	}
	if got := Decide(dest); got.Action != InitializeKaminoObligation || got.StrategyKey != autoAUTOWYLDSLane {
		t.Fatalf("wYLDS obligation not initialized: %+v", got)
	}
	dest.ObligationPresent = true
	if got := Decide(dest); got.Action != OpenRouteStep || got.Reason != "collateral_ready" || got.AmountRaw != e || got.StrategyKey != autoAUTOWYLDSLane {
		t.Fatalf("freed AUTO not deposited into wYLDS: %+v", got)
	}

	// Deposited at 1x: wYLDS's own target and approval lever it.
	funded := leverageSnapshot(1)
	funded.RouteLane, funded.StrategyKey = autoAUTOWYLDSLane, autoAUTOWYLDSLane
	funded.HeldLanes = autoAUTOPYUSD.Lane + "," + autoAUTOWYLDSLane
	funded.LeverageTargetLevel = 0
	funded.TotalVaultNAVRaw, funded.StrategyNAVRaw = 1_000_000_000, 1_000_000_000
	armLeverageCapacityFixture(&funded)
	armFeeAuthorityFixture(t, &funded)
	applyTranche(&funded, &handed)
	target, ok := decideLeverageTarget(funded, SelectorResult{Action: "KEEP"}, []LaneEconomics{leverageMarket(autoAUTOWYLDSLane, 0.60, 0.0583)}, policy, twentyBPSStepCost)
	if !ok || target.Lane != autoAUTOWYLDSLane || target.Next <= 1 || target.Reason != "up_move_approved" {
		t.Fatalf("wYLDS got no up target: %+v", target)
	}
	funded.LeverageTargetLevel = target.Next
	armLeverageCapacityFixture(&funded)
	if got := Decide(funded); got.Action != OpenRouteStep || got.Reason != leverageUpReason || got.StrategyKey != autoAUTOWYLDSLane {
		t.Fatalf("leverage_up did not run on wYLDS: %+v", got)
	}
	// The up step stakes the borrowed wYLDS to AUTO through Hastra inside it.
	if up, ok := stepDecision(funded, Decide(funded)); !ok || !up || !conversionVenueHastra(Decision{Action: SwapDebtToCollateralStep, StrategyKey: autoAUTOWYLDSLane}) {
		t.Fatal("the wYLDS up step does not stake through Hastra")
	}
}

// While PYUSD has room the PYUSD step wins: no overflow tranche, and the
// tick borrows PYUSD.
func TestPYUSDRoomKeepsThePYUSDStepAndNeverOpensWYLDS(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	source := overflowSource()
	armLeverageCapacityFixture(&source) // $1k of PYUSD room and a live approval
	in := SelectorInput{Now: now, Snapshot: source, Markets: overflowMarkets(now), Policy: DefaultSelectorPolicy()}
	result, tranche := applyTrancheSelection(in, SelectorResult{Action: "KEEP"}, nil, nil, 2, now)
	if tranche != nil || result.Action != "KEEP" || result.Overflow != "overflow_source_has_room" {
		t.Fatalf("overflow opened while PYUSD had room: %+v %+v", result, tranche)
	}
	if got := Decide(source); got.Action != OpenRouteStep || got.Reason != leverageUpReason || got.StrategyKey != autoAUTOPYUSD.Lane {
		t.Fatalf("PYUSD step did not run: %+v", got)
	}
	// And the wYLDS reserve alone, with PYUSD at its target, starts nothing.
	at := overflowSource()
	at.PositionCollateralRaw, at.PositionCollateralValueRaw = 175_000_000_000, 175_000_000_000
	at.PositionDebtRaw, at.PositionDebtValueRaw = 75_000_000_000, 75_000_000_000
	at.LTVBPS = leverageLevelLTVBPS(1.75)
	in.Snapshot = at
	if _, tranche := applyTrancheSelection(in, SelectorResult{Action: "KEEP"}, nil, nil, 2, now); tranche != nil {
		t.Fatal("overflow opened from a source at its target")
	}
	// wYLDS is never quoted as a selector entry of its own.
	if selectorDestinationLane(autoAUTOWYLDSLane) {
		t.Fatal("wYLDS is a selector destination")
	}
}

// termLane is the wYLDS lane at 1.75x with term debt maturing in left.
func termLane(left time.Duration) Snapshot {
	s := leverageSnapshot(1.75)
	s.RouteLane, s.StrategyKey = autoAUTOWYLDSLane, autoAUTOWYLDSLane
	s.ChainUnix = kaminoFixtureUnix
	s.DebtTermEndUnix = kaminoFixtureUnix + int64(left/time.Second)
	return s
}

// Keep-alive: inside 3 days with room the lane borrows the keep-alive size;
// at 5 days it does not.
func TestTermKeepAliveFiresInsideThreeDaysOnly(t *testing.T) {
	t.Parallel()
	due := termLane(60 * time.Hour)
	got := Decide(due)
	if got.Action != OpenRouteStep || got.Reason != termKeepAliveReason || got.AmountRaw != int64(termKeepAliveBorrowRaw) || got.StrategyKey != autoAUTOWYLDSLane {
		t.Fatalf("keep-alive did not fire at 2.5 days: %+v", got)
	}
	if got := Decide(termLane(120 * time.Hour)); got.Reason == termKeepAliveReason {
		t.Fatalf("keep-alive fired at 5 days: %+v", got)
	}
	// Between 1 and 3 days without room it holds, named; it never delevers.
	shut := termLane(36 * time.Hour)
	shut.AdditionalDebtRoomRaw = 0
	if got := Decide(shut); got.Action != Hold || got.Reason != "term_keepalive_no_room" {
		t.Fatalf("keep-alive without room: %+v", got)
	}
	// The entry fence passes only the exact keep-alive borrow on its lane.
	request := KaminoPrimeUSDCRequest{RouteLane: autoAUTOWYLDSLane, AmountRaw: termKeepAliveBorrowRaw}
	if _, err := termKeepAliveBypassesEntryFence(request, leverageUpReason, false, autoAUTOWYLDSLane); err != nil {
		t.Fatal("a non-keep-alive reason was judged as keep-alive")
	}
	if ok, _ := termKeepAliveBypassesEntryFence(request, termKeepAliveReason, true, autoAUTOWYLDSLane); ok {
		t.Fatal("keep-alive bypassed the entry fence during an unwind")
	}
}

// Under a day with no room (or past maturity) the lane delevers fully by
// atomic down steps (withdraw, Hastra redeem, repay) sized on the payoff.
func TestTermDeleverUnderOneDayWithoutRoom(t *testing.T) {
	t.Parallel()
	s := termLane(12 * time.Hour)
	s.AdditionalDebtRoomRaw = 0
	got := Decide(s)
	if got.Action != DeleverRouteStep || got.Reason != leverageDownReason || got.StrategyKey != autoAUTOWYLDSLane || got.AmountRaw != stepDownMarker {
		t.Fatalf("no delever at 0.5 days without room: %+v", got)
	}
	if up, ok := stepDecision(s, got); !ok || up {
		t.Fatalf("the delever is not a down step: %+v", got)
	}
	// With room, the same 12 hours keeps the lane alive instead.
	if got := Decide(termLane(12 * time.Hour)); got.Reason != termKeepAliveReason {
		t.Fatalf("keep-alive lost to delever while room existed: %+v", got)
	}
	// Matured: KLend refuses the borrow whatever the room; delever.
	if got := Decide(termLane(-time.Hour)); got.Reason != leverageDownReason {
		t.Fatalf("matured term debt did not delever: %+v", got)
	}
	// The selector moves the working lane to a held lane inside keep-alive
	// time, and away from it no rotation runs.
	working := overflowSource()
	working.ChainUnix = kaminoFixtureUnix
	working.HeldLanes = autoAUTOPYUSD.Lane + "," + autoAUTOWYLDSLane
	facts := []heldLaneFact{{Lane: autoAUTOWYLDSLane, CollateralValueRaw: 1_750_000_000, DebtValueRaw: 750_000_000, DebtTermEndUnix: kaminoFixtureUnix + 2*86_400}}
	if lane := termDueHeldLane(working, facts); lane != autoAUTOWYLDSLane {
		t.Fatalf("term-due held lane not chosen: %q", lane)
	}
	facts[0].DebtTermEndUnix = kaminoFixtureUnix + 10*86_400
	if lane := termDueHeldLane(working, facts); lane != "" {
		t.Fatalf("a lane 10 days out was moved to: %q", lane)
	}
}

// No wYLDS leftover converts to USDC: debt residue beside a position waits
// for the next down step, and a drain's residue stakes to AUTO.
func TestNoDebtToUSDCForWYLDS(t *testing.T) {
	t.Parallel()
	if debtToUSDCConvertible(autoAUTOWYLDSLane) || !debtToUSDCConvertible(autoAUTOPYUSD.Lane) || laneSwap(autoAUTOWYLDSLane, SwapDebtToUSDCStep) {
		t.Fatal("wYLDS debt is convertible to USDC")
	}
	// Payoff residue beside a debt-free position stays idle.
	residue := leverageSnapshot(1)
	residue.RouteLane, residue.StrategyKey = autoAUTOWYLDSLane, autoAUTOWYLDSLane
	residue.DebtIdleRaw = 5_000
	if got := Decide(residue); got.Action == SwapDebtToCollateralStep || got.Action == SwapDebtToUSDCStep {
		t.Fatalf("payoff residue converted: %+v", got)
	}
	// A partial withdrawal's leftover debt cash is no leg of its own.
	partial := termLane(20 * 24 * time.Hour)
	partial.WithdrawalDemandRaw, partial.VoltrIdleRaw = 100_000_000, 0
	partial.DebtIdleRaw = 5_000
	partial.PartialWithdrawalOperationID, partial.PartialWithdrawalLTVBPS = "op", partial.LTVBPS
	if a, _, _, ok := partialWithdrawalStep(partial); ok && (a == SwapDebtToUSDCStep || a == SwapDebtToCollateralStep) {
		t.Fatalf("partial leftover debt converted: %s", a)
	}
	// A drain's residue after the payoff.
	drain := base()
	drain.RouteLane, drain.StrategyKey = autoAUTOWYLDSLane, autoAUTOWYLDSLane
	drain.Unwind, drain.DebtIdleRaw = true, 5_000
	drain.StrategyNAVRaw, drain.PriorReportedNAVRaw = 5_000, 5_000
	if got := Decide(drain); got.Action != SwapDebtToCollateralStep || got.Reason != "withdrawal_stake_debt_residue" {
		t.Fatalf("drain residue: %+v", got)
	}
	// Nothing in the whole decision space above emits debt to USDC on wYLDS.
	for _, s := range []Snapshot{residue, partial, drain} {
		if got := Decide(s); got.Action == SwapDebtToUSDCStep {
			t.Fatalf("debt to USDC on wYLDS: %+v", got)
		}
	}
}

// Two held AUTO lanes are one book: AUTO/PYUSD (working) and AUTO/wYLDS share
// the collateral reserve and the AUTO custody, which is counted once, while
// wYLDS's own debt custody is counted. The hand sum: the one-lane book
// 32_000_010, plus wYLDS's 5e9 receipts (10e9 AUTO at $1.5 = 15_000_000)
// minus 2_000_000 wYLDS debt plus 1_000_000 idle wYLDS (both at par). Its
// term maturity reaches the held-lane facts.
func TestTwoAUTOLanesNAVCountsTheSharedAUTOCustodyOnce(t *testing.T) {
	manifest, accounts := multiLaneFixture(t, func(accounts *[]ConfirmedAccount) {
		route, err := runtimeRoute(autoAUTOWYLDSLane)
		if err != nil {
			t.Fatal(err)
		}
		peg := new(big.Int).Lsh(big.NewInt(1), 60)
		debt := kaminoReserveImage(t, route.Kamino.Market, route.Kamino.DebtReserve, route.Kamino.DebtMint, 77, peg, 98_000_000, 2_000_000, 100_000_000, 6)
		binary.LittleEndian.PutUint64(debt.Data[kaminoReserveConfigOffset+928:], 2_592_000)
		upsertConfirmedAccount(accounts, debt)
		obligation := kaminoObligationImage(t, route, 77, 5_000_000_000, 2_000_000)
		binary.LittleEndian.PutUint64(obligation.Data[1208+80:], uint64(kaminoFixtureUnix-86_400))
		upsertConfirmedAccount(accounts, obligation)
		custody := tokenAccountFixture(t, route.DebtCustody, route.Kamino.DebtMint, bridgeVault, 1_000_000)
		custody.Owner = route.DebtTokenProgram
		upsertConfirmedAccount(accounts, custody)
	})
	s := observeMultiLane(t, manifest, accounts)
	snap := s.Snapshot
	if snap.RouteLane != autoAUTOPYUSD.Lane || snap.HeldLanes != autoAUTOPYUSD.Lane+","+autoAUTOWYLDSLane || snap.CrossLaneHardLTV {
		t.Fatalf("held set or working lane wrong: %q %q", snap.RouteLane, snap.HeldLanes)
	}
	if snap.OtherLanesNAVRaw != 14_000_000 || snap.StrategyNAVRaw != 32_000_010+14_000_000 || snap.TotalVaultNAVRaw != 32_000_021+14_000_000 {
		t.Fatalf("two AUTO lanes NAV: other %d strategy %d total %d", snap.OtherLanesNAVRaw, snap.StrategyNAVRaw, snap.TotalVaultNAVRaw)
	}
	if snap.PositionCollateralValueRaw != 30_000_000 || snap.PositionDebtValueRaw != 7_000_000 || snap.ObservationID == oneLaneAUTOObservationID {
		t.Fatalf("working view absorbed the wYLDS lane or kept the one-lane identity: %+v", snap)
	}
	if len(s.heldFacts) != 1 || s.heldFacts[0].Lane != autoAUTOWYLDSLane || s.heldFacts[0].DebtTermEndUnix != kaminoFixtureUnix-86_400+2_592_000 {
		t.Fatalf("wYLDS term maturity missing from held facts: %+v", s.heldFacts)
	}
	// The report the bridge builds values the same book over the union of
	// both lanes' accounts.
	routes, err := navRoutes(autoAUTOPYUSD, snap.heldLanes())
	if err != nil {
		t.Fatal(err)
	}
	navAccounts, err := selectRouteNAVAccountsForRoutes(accounts, routes)
	if err != nil {
		t.Fatal(err)
	}
	nav, err := ComputeRouteNAVForRoutes(77, navAccounts, manifest, nil, routes)
	if err != nil || nav.Report.NAVAfterRaw != uint64(snap.StrategyNAVRaw) {
		t.Fatalf("report NAV is not the two-lane book: %d %v", nav.Report.NAVAfterRaw, err)
	}
	wyldsRoute, _ := runtimeRoute(autoAUTOWYLDSLane)
	seen := false
	for _, a := range navAccounts {
		seen = seen || a.Address == wyldsRoute.DebtCustody
	}
	if !seen {
		t.Fatal("the wYLDS debt custody is not in the fingerprinted NAV accounts")
	}
}

// Hard LTV runs over both AUTO lanes before the working lane: a wYLDS lane
// past its hard LTV becomes the tick's view and gets the risk decision,
// whose repayment buffer converts through Hastra.
func TestWYLDSLanePastHardLTVIsProtectedFirst(t *testing.T) {
	manifest, accounts := multiLaneFixture(t, func(accounts *[]ConfirmedAccount) {
		route, err := runtimeRoute(autoAUTOWYLDSLane)
		if err != nil {
			t.Fatal(err)
		}
		peg := new(big.Int).Lsh(big.NewInt(1), 60)
		// 5e9 receipts redeem $15 of AUTO; $10 of wYLDS debt is 66.7%,
		// past the 6000 bps hard LTV.
		upsertConfirmedAccount(accounts, kaminoReserveImage(t, route.Kamino.Market, route.Kamino.DebtReserve, route.Kamino.DebtMint, 77, peg, 90_000_000, 10_000_000, 100_000_000, 6))
		upsertConfirmedAccount(accounts, kaminoObligationImage(t, route, 77, 5_000_000_000, 10_000_000))
	})
	s := observeMultiLane(t, manifest, accounts).Snapshot
	if s.RouteLane != autoAUTOWYLDSLane || !s.CrossLaneHardLTV || s.LTVBPS < 6000 {
		t.Fatalf("risky wYLDS lane was not protected first: %q cross=%t ltv=%d", s.RouteLane, s.CrossLaneHardLTV, s.LTVBPS)
	}
	s.MonitorsArmed = false
	d := Decide(s)
	if d.StrategyKey != autoAUTOWYLDSLane || d.Reason != hardLTVDeleverReason || d.Action != DeleverRouteStep || d.AmountRaw <= 0 {
		t.Fatalf("risky wYLDS lane did not get the hard-LTV decision: %+v", d)
	}
}
