package backyard

import (
	"context"
	"encoding/binary"
	"math/big"
	"testing"
	"time"
)

// debtReserveWithRoom is a USDC debt reserve whose only binding borrow limit
// is its available liquidity.
func debtReserveWithRoom(t *testing.T, route RuntimeRoute, availableRaw uint64) ConfirmedAccount {
	t.Helper()
	d := reserveFixture(t, route.Kamino.DebtReserve, route.Kamino.DebtMint, 77, new(big.Int).Lsh(big.NewInt(1), 60), availableRaw, availableRaw)
	putKey(t, d.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(d.Data[kaminoReserveConfigOffset+160:], 1<<50)
	binary.LittleEndian.PutUint64(d.Data[kaminoReserveConfigOffset+168:], 1<<50)
	binary.LittleEndian.PutUint64(d.Data[kaminoOutsideBorrowLimitOffset:], 1<<50)
	binary.LittleEndian.PutUint64(d.Data[kaminoBorrowFactorOffset:], 100)
	return d
}

// A stream write that opens a watched lane's borrow room wakes the tick loop
// once; a write that leaves it closed, or keeps it open, wakes nothing; a
// lane watched only for its gauge never wakes. The woken tick borrows the
// room under the standing approval.
func TestReserveUpdateThatOpensRoomWakesATickThatBorrowsIt(t *testing.T) {
	t.Parallel()
	route, err := runtimeRoute(MapleSyrupUSDCLaneID)
	if err != nil {
		t.Fatal(err)
	}
	view := &View{accounts: map[string]viewAccount{}, receipts: map[string]uint64{}, advanced: make(chan struct{}), now: time.Now, through: 1000, confirmed: 1001}
	view.accounts[budgetClockAddress] = viewAccount{ConfirmedAccount: clockFixture(), slot: 100}
	view.accounts[route.Kamino.DebtReserve] = viewAccount{ConfirmedAccount: debtReserveWithRoom(t, route, 0), slot: 100}
	worker := &Worker{wake: make(chan struct{}, 1)}
	var reports []roomUpdate
	watch := roomWatch{lane: route.Lane, route: route, belowTarget: true}
	view.WatchRoom(rwa, []roomWatch{watch}, func(u roomUpdate) { reports = append(reports, u); worker.roomObserved(u) })
	woke := func() bool {
		select {
		case <-worker.wake:
			return true
		default:
			return false
		}
	}
	if woke() || len(reports) != 1 || reports[0].open || reports[0].opened {
		t.Fatalf("registering a closed reserve: woke or reported %+v", reports)
	}
	write := func(slot, available uint64) {
		t.Helper()
		if err := view.Handle(context.Background(), accountFrame(debtReserveWithRoom(t, route, available), slot, 1, viewAccountsFilter)); err != nil {
			t.Fatal(err)
		}
	}
	// Room below the $10 minimum borrow: no wake.
	write(190, 5_000_000)
	if woke() {
		t.Fatal("room below the minimum woke the worker")
	}
	// Room opens: one wake, reporting the room.
	const room = 3_300_000_000
	write(191, room)
	last := reports[len(reports)-1]
	if !woke() || !last.opened || last.roomRaw != room || last.roomUSD != 3300 {
		t.Fatalf("opening room did not wake: %+v", last)
	}
	// Room stays open: no second wake.
	write(192, room+1_000_000)
	if woke() {
		t.Fatal("room that stayed open woke the worker again")
	}
	// Re-registering on the next tick is not an edge either.
	view.WatchRoom(rwa, []roomWatch{watch}, worker.roomObserved)
	write(193, room)
	if woke() {
		t.Fatal("re-registration faked an opening")
	}
	// Closed then reopened: a new wake.
	write(194, 0)
	write(195, room)
	if !woke() {
		t.Fatal("reopened room did not wake")
	}
	// A held lane at its target is watched for the gauge only.
	gauge := watch
	gauge.belowTarget = false
	view.WatchRoom(rwa, []roomWatch{gauge}, worker.roomObserved)
	write(196, 0)
	write(197, room)
	if woke() {
		t.Fatal("a lane at its target woke the worker")
	}

	// The woken tick: the standing approval is the whole step; this tick's
	// capacity is the room the update opened, and the tick borrows exactly it.
	receive := kaminoReceiveWithinRoom(last.roomRaw, binary.LittleEndian.Uint64(debtReserveWithRoom(t, route, room).Data[kaminoReserveConfigOffset+40:]))
	s, _ := oct10WindowSnapshot(t, receive)
	s.LeverageUpApproved = true
	if d := Decide(targeted(s)); d.Action != OpenRouteStep || d.Reason != leverageUpReason || uint64(d.AmountRaw) != receive {
		t.Fatalf("the woken tick did not borrow the opened room %d: %+v", receive, d)
	}
}

// The watch set: every held lane's debt reserve for the gauge, waking only
// on a lane below its stored level.
func TestLeverageRoomWatchesFollowTheLevels(t *testing.T) {
	t.Parallel()
	s, _ := oct10WindowSnapshot(t, 0)
	other := s
	other.RouteLane, other.StrategyKey = onreONycUSDC, onreONycUSDC
	other.PositionCollateralValueRaw, other.PositionDebtValueRaw, other.PositionDebtRaw = 1_500_000_000, 500_000_000, 500_000_000
	other.LeverageTargetLevel = 0
	o := Observation{Snapshot: s, lanes: []Snapshot{s, other}}
	gaps := heldLaneLeverage(o)
	if len(gaps) != 2 || gaps[0].lane != s.RouteLane || !gaps[0].targeted || gaps[0].gap < 0.7 || gaps[1].targeted {
		t.Fatalf("held lane leverage: %+v", gaps)
	}
	watches := leverageRoomWatches(o)
	if len(watches) != 2 || !watches[0].belowTarget || watches[1].belowTarget || watches[1].lane != onreONycUSDC {
		t.Fatalf("watches: %+v", watches)
	}
	// A level below the position wakes nothing; one above it does.
	o.lanes[1].LeverageTargetLevel = 1.75
	watches = leverageRoomWatches(o)
	if len(watches) != 2 || !watches[1].belowTarget {
		t.Fatalf("a lane below its level is not woken on: %+v", watches)
	}
}
