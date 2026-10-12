package backyard

import (
	"errors"
	"strings"
	"testing"
)

// A small book the table tests walk: every lane's position at par (one raw
// unit is one bridge USDC unit), the vault's cash, and the chain facts the
// target reads. Each step applies the decision's effect the way its
// transaction lands; the walk ends at the first hold.
type bookLane struct {
	lane              string
	coll, debt, idle  int64 // position collateral, debt, idle collateral custody
	termEnd           int64 // term debt maturity (unix), 0 for open debt
	blocked           bool  // the debt reserve's utilization closes borrowing
	room              int64 // the debt reserve's fee-inclusive borrow room
	obligationPresent bool
}

type book struct {
	lanes                 []*bookLane
	voltr, squads, demand int64
	now                   int64
	in                    targetInputs
}

const bookNow = int64(1_760_000_000)

func (b *book) lane(id string) *bookLane {
	for _, l := range b.lanes {
		if l.lane == id {
			return l
		}
	}
	l := &bookLane{lane: id, room: 1 << 50}
	b.lanes = append(b.lanes, l)
	return l
}

func (b *book) equity() int64 {
	total := b.squads
	for _, l := range b.lanes {
		total += l.coll + l.idle - l.debt
	}
	return total
}

// views is the observer's per-lane views over the book, in registry order:
// the held lanes, or the candidate the flat vault looks at.
func (b *book) views() []Snapshot {
	var held []string
	for _, id := range earnLaneIDs(true) {
		for _, l := range b.lanes {
			if l.lane == id && (l.coll > 0 || l.debt > 0 || l.idle > 0) {
				held = append(held, id)
			}
		}
	}
	nav := b.equity()
	var views []Snapshot
	for _, id := range held {
		l := b.lane(id)
		s := Snapshot{ObservationID: "book", Slot: 100, RouteKind: RouteKind, Fresh: true, RouteLane: id, StrategyKey: id,
			HeldLanes: strings.Join(held, ","), LiquidationThresholdBPS: 8000, MinimumCollateralDepositRaw: 1,
			CapacityRaw: 1 << 50, PolicyLimitRaw: 1 << 50, MaxTargetLTVEntryRaw: 1 << 50, TopupDepositRoomRaw: 1 << 50,
			VoltrIdleRaw: b.voltr, SquadsIdleRaw: b.squads, WithdrawalDemandRaw: b.demand,
			CollateralIdleRaw: l.idle, CollateralIdleValueRaw: l.idle,
			HasPosition: l.coll > 0 || l.debt > 0, PositionCollateralRaw: l.coll, PositionCollateralValueRaw: l.coll,
			PositionDebtRaw: l.debt, PositionDebtValueRaw: l.debt, PayoffDebtRaw: l.debt,
			StrategyNAVRaw: nav, PriorReportedNAVRaw: nav, TotalVaultNAVRaw: nav + b.voltr, OtherLanesNAVRaw: nav - (l.coll + l.idle - l.debt) - b.squads,
			ChainUnix: b.now, BorrowCapacityKnown: true, BorrowUtilizationBlocked: l.blocked, AdditionalDebtRoomRaw: uint64(l.room),
			ObligationPresenceKnown: true, ObligationPresent: true}
		copy(s.BorrowDebtPriceSF[:], leverageTestPriceBytes())
		s.BorrowUSDCPriceSF, s.BorrowDebtDecimals = s.BorrowDebtPriceSF, 6
		if l.coll > 0 {
			s.LTVBPS = (l.debt*10_000 + l.coll - 1) / l.coll
		}
		if l.debt > 0 && l.termEnd > 0 {
			s.DebtTermEndUnix = l.termEnd
		}
		equity := l.coll - l.debt
		for _, level := range []float64{1.5, 1.75} {
			borrow := max(int64(float64(equity)*(level-1))-l.debt, 0)
			if l.blocked {
				borrow = 0
			}
			borrow = min(borrow, l.room)
			if level == 1.5 {
				s.LeverageBorrow150Raw = uint64(borrow)
			} else {
				s.LeverageBorrow175Raw = uint64(borrow)
			}
		}
		if overflow, ok := overflowLaneOf(id); ok && s.HasPosition && overflowSourceBorrowClosed(s) {
			o := b.lane(overflow)
			s.OverflowRoomKnown, s.OverflowRoomRaw, s.OverflowRoomValueRaw, s.OverflowObligationPresent = true, uint64(o.room), o.room, o.obligationPresent
		}
		level := b.in.Levels[id]
		s.LeverageTargetLevel, s.LeverageUpApproved = level.Level, level.UpApproved && level.Level > 1
		views = append(views, s)
	}
	if len(views) == 0 {
		views = append(views, Snapshot{ObservationID: "book", Slot: 100, RouteKind: RouteKind, Fresh: true, RouteLane: SelectedRouteID, StrategyKey: SelectedRouteID,
			LiquidationThresholdBPS: 8000, VoltrIdleRaw: b.voltr, SquadsIdleRaw: b.squads, WithdrawalDemandRaw: b.demand, ChainUnix: b.now,
			StrategyNAVRaw: nav, PriorReportedNAVRaw: nav, TotalVaultNAVRaw: nav + b.voltr})
	}
	return views
}

// next is one tick: the target over the views, then Decide on the working
// lane's view.
func (b *book) next() (Snapshot, Decision) {
	views := b.views()
	i, t := target(views, b.in)
	s := views[i]
	s.Target = t
	return s, Decide(s)
}

// apply lands d: the effect its transaction has on the book.
func (b *book) apply(t *testing.T, s Snapshot, d Decision) {
	t.Helper()
	l := b.lane(d.StrategyKey)
	switch d.Action {
	case InitializeKaminoObligation:
		l.obligationPresent = true
	case OpenRouteStep:
		switch d.Reason {
		case leverageUpReason, termKeepAliveReason:
			// A borrow converts to collateral inside the step; a term
			// borrow restarts the term.
			l.debt += d.AmountRaw
			l.coll += d.AmountRaw
			l.room -= d.AmountRaw
			if l.termEnd > 0 {
				l.termEnd = b.now + 30*86_400
			}
		default:
			l.coll += l.idle
			l.idle = 0
		}
	case DeleverRouteStep:
		switch d.Reason {
		case overflowMoveReason:
			to := b.lane(s.Target.MoveTo)
			l.coll -= d.AmountRaw
			to.coll += d.AmountRaw
		case releaseReason:
			// A release frees equity E at the position's own level: it
			// withdraws C*E/(C-D), repays the debt share, and the rest (E)
			// waits as idle collateral.
			withdrawn := int64(float64(d.AmountRaw) * float64(l.coll) / float64(l.coll-l.debt))
			repay := withdrawn - d.AmountRaw
			l.coll -= withdrawn
			l.debt -= repay
			l.idle += d.AmountRaw
		case exitWithdrawReason:
			l.idle += l.coll
			l.coll = 0
		default:
			repay, _ := stepDownRepay(s, d.Reason)
			r := min(int64(repay), l.debt)
			l.debt -= r
			l.coll -= r
		}
	case SwapCollateralToStableStep:
		b.squads += l.idle
		l.idle = 0
	case SwapStableToCollateralStep:
		l.idle += b.squads
		b.squads = 0
	case StageSquadsToVoltr:
		b.voltr += b.squads
		b.squads = 0
	case VoltrAllocateToSquads:
		b.voltr -= d.AmountRaw
		b.squads += d.AmountRaw
	default:
		t.Fatalf("the walk cannot apply %+v", d)
	}
}

// walk steps the book until the first hold and returns every decision,
// ending with that hold.
func (b *book) walk(t *testing.T, limit int) []Decision {
	t.Helper()
	var steps []Decision
	for len(steps) < limit {
		s, d := b.next()
		steps = append(steps, d)
		if d.Action == Hold || d.Action == HoldManualRecovery {
			return steps
		}
		b.apply(t, s, d)
	}
	t.Fatalf("no hold after %d steps: %+v", limit, steps)
	return nil
}

func stepNames(steps []Decision) string {
	names := make([]string, len(steps))
	for i, d := range steps {
		names[i] = string(d.Action) + ":" + d.Reason
	}
	return strings.Join(names, " ")
}

func wantSteps(t *testing.T, steps []Decision, want ...string) {
	t.Helper()
	if got := stepNames(steps); got != strings.Join(want, " ") {
		t.Fatalf("steps\n got %s\nwant %s", got, strings.Join(want, " "))
	}
}

func ltvOf(l *bookLane) int64 { return l.debt * 10_000 / l.coll }

// mapleBook is one Maple lane with $100k equity at its stored level.
func mapleBook(level float64, approved bool) *book {
	b := &book{now: bookNow, in: targetInputs{Levels: map[string]laneLevel{SelectedRouteID: {Level: level, UpApproved: approved}}}}
	l := b.lane(SelectedRouteID)
	l.coll, l.debt, l.obligationPresent = 150_000_000_000, 50_000_000_000, true
	return b
}

func TestTargetWithdrawalFreesExactlyTheDemand(t *testing.T) {
	t.Parallel()
	b := mapleBook(1.5, false)
	b.demand = 10_000_000_000 // $10k queued, nothing idle
	steps := b.walk(t, 10)
	// Release (debt share repaid, LTV kept), its collateral to USDC, stage.
	wantSteps(t, steps,
		"DELEVER_ROUTE_STEP:"+releaseReason,
		"SWAP_COLLATERAL_TO_STABLE_STEP:"+outboundCollateralReason,
		"STAGE_SQUADS_TO_VOLTR:"+outboundStageReason,
		"HOLD:"+holdAtTarget)
	l := b.lane(SelectedRouteID)
	if b.voltr < b.demand || b.voltr > b.demand+b.demand/50 || ltvOf(l) < 3332 || ltvOf(l) > 3334 || l.debt == 0 {
		t.Fatalf("freed %d for %d, lane LTV %d debt %d", b.voltr, b.demand, ltvOf(l), l.debt)
	}
}

func TestTargetLevelOneSevenFiveStepsUpOnceApproved(t *testing.T) {
	t.Parallel()
	// Unapproved: the stored level alone moves nothing.
	b := mapleBook(1.75, false)
	wantSteps(t, b.walk(t, 5), "HOLD:"+holdAtTarget)
	b.in.Levels[SelectedRouteID] = laneLevel{Level: 1.75, UpApproved: true}
	wantSteps(t, b.walk(t, 5), "OPEN_ROUTE_STEP:"+leverageUpReason, "HOLD:"+holdAtTarget)
	if l := b.lane(SelectedRouteID); ltvOf(l) < 4280 || ltvOf(l) > 4286 {
		t.Fatalf("1.75 step left LTV %d", ltvOf(l))
	}
	// The stored level falls to 1.5: one partial down step, never debt 0.
	b.in.Levels[SelectedRouteID] = laneLevel{Level: 1.5}
	wantSteps(t, b.walk(t, 5), "DELEVER_ROUTE_STEP:"+leverageDownPartialReason, "HOLD:"+holdAtTarget)
	if l := b.lane(SelectedRouteID); l.debt == 0 || ltvOf(l) > 3334 {
		t.Fatalf("1.5 down step left debt %d LTV %d", l.debt, ltvOf(l))
	}
	// Level 1 is economic: it holds at 1.5, not at debt 0.
	b.in.Levels[SelectedRouteID] = laneLevel{Level: 1}
	wantSteps(t, b.walk(t, 5), "HOLD:"+holdAtTarget)
}

func TestTargetHardLTVStepsDownOrHoldsNoRoom(t *testing.T) {
	t.Parallel()
	b := mapleBook(1.75, true)
	l := b.lane(SelectedRouteID)
	l.debt = 92_000_000_000 // LTV 6133 > hard 6000
	wantSteps(t, b.walk(t, 5), "DELEVER_ROUTE_STEP:"+hardLTVDeleverReason, "HOLD:"+holdAtTarget)
	if ltvOf(l) >= 6000 || l.debt == 0 {
		t.Fatalf("hard LTV step left LTV %d debt %d", ltvOf(l), l.debt)
	}
	// With no withdraw room the prepared step holds no_room (step_prepare);
	// the target stays the down step.
	l.debt = 92_000_000_000
	views := b.views()
	if _, tgt := target(views, b.in); tgt.Rule != ruleHardLTV {
		t.Fatalf("hard LTV target %+v", tgt)
	}
	var st stepState
	st.obligation.DepositedValueSF[0], st.obligation.AllowedBorrowValueSF[0] = 100, 80
	st.obligation.BorrowFactorAdjustedDebtValueSF[0], st.obligation.BorrowedAssetsMarketValueSF[0] = 79, 79
	if _, err := st.stepWithdrawReceipts(); err == nil {
		t.Fatal("a reserve without withdraw room sized a down step")
	} else if hold, ok := err.(*BudgetHold); !ok || hold.Reason != holdNoRoom {
		t.Fatalf("no withdraw room is not no_room: %v", err)
	}
}

// wyldsBook is the wYLDS lane at 1.75x with term debt maturing in left.
func wyldsBook(left int64, room int64) *book {
	b := &book{now: bookNow, in: targetInputs{Levels: map[string]laneLevel{autoAUTOWYLDSLane: {Level: 1.75}}}}
	l := b.lane(autoAUTOWYLDSLane)
	l.coll, l.debt, l.termEnd, l.room, l.obligationPresent = 58_980_000_000, 25_260_000_000, bookNow+left, room, true
	return b
}

func TestTargetTermMaturity(t *testing.T) {
	t.Parallel()
	// Under a day left and no room for the keep-alive borrow: debt 0 by one
	// down step.
	b := wyldsBook(20*3_600, 0)
	wantSteps(t, b.walk(t, 5), "DELEVER_ROUTE_STEP:"+debtZeroReason, "HOLD:"+holdAtTarget)
	if l := b.lane(autoAUTOWYLDSLane); l.debt != 0 || l.coll <= 0 {
		t.Fatalf("maturity left debt %d collateral %d", l.debt, l.coll)
	}
	// Under three days with room: the minimum up step keeps it alive, and
	// the restarted term needs nothing more.
	b = wyldsBook(2*86_400, 1<<40)
	wantSteps(t, b.walk(t, 5), "OPEN_ROUTE_STEP:"+termKeepAliveReason, "HOLD:"+holdAtTarget)
	if l := b.lane(autoAUTOWYLDSLane); l.termEnd != bookNow+30*86_400 || l.debt != 25_260_000_000+int64(termKeepAliveBorrowRaw) {
		t.Fatalf("keep-alive left term %d debt %d", l.termEnd, l.debt)
	}
	// More than three days left: nothing.
	b = wyldsBook(4*86_400, 1<<40)
	wantSteps(t, b.walk(t, 5), "HOLD:"+holdAtTarget)
}

func TestTargetOperatorExit(t *testing.T) {
	t.Parallel()
	// Debt 0: one full down step, then the lane holds its equity.
	b := mapleBook(1.5, false)
	b.in.Exits = map[string]laneExit{SelectedRouteID: {}}
	wantSteps(t, b.walk(t, 5), "DELEVER_ROUTE_STEP:"+debtZeroReason, "HOLD:"+holdAtTarget)
	if l := b.lane(SelectedRouteID); l.debt != 0 || l.coll != 100_000_000_000 {
		t.Fatalf("debt-0 exit left debt %d collateral %d", l.debt, l.coll)
	}
	// Equity 0 as well: withdraw, convert, stage; the cash ends in Voltr.
	b = mapleBook(1.5, false)
	b.in.Exits = map[string]laneExit{SelectedRouteID: {EquityZero: true}}
	wantSteps(t, b.walk(t, 8),
		"DELEVER_ROUTE_STEP:"+debtZeroReason,
		"DELEVER_ROUTE_STEP:"+exitWithdrawReason,
		"SWAP_COLLATERAL_TO_STABLE_STEP:"+outboundCollateralReason,
		// Flat, the vault's Squads cash returns to Voltr.
		"STAGE_SQUADS_TO_VOLTR:"+returnCashReason,
		"HOLD:"+holdAtTarget)
	if b.voltr != 100_000_000_000 || b.equity() != 0 {
		t.Fatalf("equity-0 exit left Voltr %d equity %d", b.voltr, b.equity())
	}
}

// The Oct 11 overflow, replayed: AUTO/AUTO/PYUSD at 1.5x (LTV 3333) with
// its stored level 1.75 and PYUSD borrowing closed; the wYLDS reserve has
// room. The target moves the collateral the source can release down to the
// 1.75 LTV into the wYLDS obligation in one transaction, and the wYLDS lane
// levers it to 1.75 by two up steps: four transactions (with the
// obligation's initializer) where the move machine took thirteen. The end
// state is the live book (both lanes at 1.75x, ~$67.5k / ~$33.7k equity).
func TestTargetOverflowReplaysOctoberEleventhInFourTransactions(t *testing.T) {
	t.Parallel()
	b := &book{now: bookNow, in: targetInputs{Levels: map[string]laneLevel{
		autoAUTOPYUSD.Lane: {Level: 1.75, UpApproved: true},
		autoAUTOWYLDSLane:  {Level: 1.75, UpApproved: true},
	}}}
	pyusd := b.lane(autoAUTOPYUSD.Lane)
	pyusd.coll, pyusd.debt, pyusd.blocked, pyusd.obligationPresent = 151_858_745_714, 50_609_586_879, true, true
	wylds := b.lane(autoAUTOWYLDSLane)
	wylds.room, wylds.termEnd = 191_124_551_624, bookNow+30*86_400
	steps := b.walk(t, 20)
	wantSteps(t, steps,
		"INITIALIZE_KAMINO_OBLIGATION:multiply_obligation_missing",
		"DELEVER_ROUTE_STEP:"+overflowMoveReason,
		"OPEN_ROUTE_STEP:"+leverageUpReason,
		"OPEN_ROUTE_STEP:"+leverageUpReason,
		"HOLD:"+holdAtTarget)
	if len(steps)-1 != 4 {
		t.Fatalf("%d transactions", len(steps)-1)
	}
	for _, l := range []*bookLane{pyusd, wylds} {
		if ltv := ltvOf(l); ltv < 4270 || ltv > 4286 {
			t.Fatalf("%s ended at LTV %d", l.lane, ltv)
		}
	}
	if e := wylds.coll - wylds.debt; e < 33_000_000_000 || e > 34_500_000_000 {
		t.Fatalf("moved equity %d", e)
	}
	// Restarted on that end state the target is the state: no more moves.
	wantSteps(t, b.walk(t, 3), "HOLD:"+holdAtTarget)
}

// P1: an up step larger than a split-policy lane's leg cap borrows up to the
// cap (with the collateral already in custody counted against it) instead of
// holding; only a cap with no room left for the minimum borrow holds.
func TestUpStepOverTheLegCapSizesToTheCap(t *testing.T) {
	t.Parallel()
	var st stepState
	for _, lane := range earnLaneIDs(false) {
		route, err := runtimeRoute(lane)
		if err == nil && stepLegCap(route) != ^uint64(0) {
			st.route = route
			break
		}
	}
	if st.route.Lane == "" {
		t.Skip("no split-policy lane is installed")
	}
	limit := stepLegCap(st.route) - stepLegCap(st.route)/10
	st.position = leverageTestPosition(3*limit, limit)
	if got, err := stepUpBorrowWithinCap(st, 2*limit); err != nil || got != limit {
		t.Fatalf("over-cap borrow sized %d (%v), want the cap %d", got, err, limit)
	}
	st.heldCollateral = limit / 4
	if got, err := stepUpBorrowWithinCap(st, 2*limit); err != nil || got != limit-limit/4 {
		t.Fatalf("held collateral not counted: %d (%v)", got, err)
	}
	if got, err := stepUpBorrowWithinCap(st, limit/2); err != nil || got != limit/2 {
		t.Fatalf("an in-cap borrow changed: %d (%v)", got, err)
	}
	st.heldCollateral = limit
	if _, err := stepUpBorrowWithinCap(st, 2*limit); err == nil {
		t.Fatal("a full cap borrowed")
	}
}

// A step the chain refuses in simulation is the chain_refused hold carrying
// the simulation error, journaled on the never-sent row; an unavailable
// endpoint stays a retry and the Squads spending limit keeps its own stop.
func TestStepSimulationRefusalIsChainRefused(t *testing.T) {
	t.Parallel()
	refused := stepSimulationHold(errors.New(`signed transaction simulation failed: slot=1 err={"InstructionError":[3,{"Custom":6098}]}`))
	var hold *BudgetHold
	if !errors.As(refused, &hold) || hold.Reason != holdChainRefused || !strings.Contains(hold.Details["simulation"], "6098") {
		t.Fatalf("refusal: %v", refused)
	}
	for _, err := range []error{confirmedObservationUnavailable(errors.New("rpc down")), &SquadsSpendingLimitError{Slot: 1}} {
		if got := stepSimulationHold(err); got != err {
			t.Fatalf("%v became %v", err, got)
		}
	}
}

// A withdrawal larger than 90% of the biggest lane frees the rest from the
// next lane: Maple ($100k) frees up to 90% of itself, OnRe ($60k) the rest.
func TestTargetWithdrawalSpansLanes(t *testing.T) {
	t.Parallel()
	b := mapleBook(1.5, false)
	b.in.Levels[onreONycUSDC] = laneLevel{Level: 1.5}
	onre := b.lane(onreONycUSDC)
	onre.coll, onre.debt, onre.obligationPresent = 90_000_000_000, 30_000_000_000, true
	b.demand = 95_000_000_000
	steps := b.walk(t, 12)
	release := "DELEVER_ROUTE_STEP:" + releaseReason
	swap := "SWAP_COLLATERAL_TO_STABLE_STEP:" + outboundCollateralReason
	stage := "STAGE_SQUADS_TO_VOLTR:" + outboundStageReason
	wantSteps(t, steps, release, swap, stage, release, swap, stage, "HOLD:"+holdAtTarget)
	if b.voltr < b.demand || b.lane(SelectedRouteID).debt == 0 || onre.debt == 0 {
		t.Fatalf("freed %d of %d; debts %d %d", b.voltr, b.demand, b.lane(SelectedRouteID).debt, onre.debt)
	}
}

// Each overflow move is sized by the room the overflow reserve still has,
// and the overflow lane levers it before the next move: with room for only
// $10k of borrowing, one move of ~$13.3k and its up steps, then nothing.
func TestTargetOverflowMovesOnlyWhatTheRoomLevers(t *testing.T) {
	t.Parallel()
	b := &book{now: bookNow, in: targetInputs{Levels: map[string]laneLevel{
		autoAUTOPYUSD.Lane: {Level: 1.75, UpApproved: true},
		autoAUTOWYLDSLane:  {Level: 1.75, UpApproved: true},
	}}}
	pyusd := b.lane(autoAUTOPYUSD.Lane)
	pyusd.coll, pyusd.debt, pyusd.blocked, pyusd.obligationPresent = 151_858_745_714, 50_609_586_879, true, true
	wylds := b.lane(autoAUTOWYLDSLane)
	wylds.room, wylds.termEnd, wylds.obligationPresent = 10_000_000_000, bookNow+30*86_400, true
	steps := b.walk(t, 12)
	moves := 0
	for _, d := range steps {
		if d.Reason == overflowMoveReason {
			moves++
		}
	}
	if moved := wylds.coll - wylds.debt; moves != 1 || moved < 13_000_000_000 || moved > 13_400_000_000 || wylds.room > leverageMinimumBorrowRaw {
		t.Fatalf("%d moves of %d, room left %d: %s", moves, moved, wylds.room, stepNames(steps))
	}
}

// An unfunded withdrawal runs before a term's keep-alive until its last day.
func TestTargetKeepAliveWaitsForAWithdrawalUntilTheLastDay(t *testing.T) {
	t.Parallel()
	b := wyldsBook(2*86_400, 1<<40)
	b.demand = 5_000_000_000
	if s, d := b.next(); s.Target.Rule != ruleWithdrawal || d.Reason != releaseReason {
		t.Fatalf("2 days left: %+v %+v", s.Target, d)
	}
	b = wyldsBook(20*3_600, 1<<40)
	b.demand = 5_000_000_000
	if s, d := b.next(); s.Target.Rule != ruleTermKeepAlive || d.Reason != termKeepAliveReason {
		t.Fatalf("20 hours left: %+v %+v", s.Target, d)
	}
}
