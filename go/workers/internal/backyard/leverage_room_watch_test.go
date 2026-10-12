package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
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
	route, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	view := &View{accounts: map[string]viewAccount{}, receipts: map[string]uint64{}, advanced: make(chan struct{}), now: time.Now, through: 1000, confirmed: 1001}
	view.accounts[budgetClockAddress] = viewAccount{ConfirmedAccount: clockFixture(), slot: 100}
	view.accounts[route.Kamino.DebtReserve] = viewAccount{ConfirmedAccount: debtReserveWithRoom(t, route, 0), slot: 100}
	worker := &Worker{wake: make(chan struct{}, 1)}
	var reports []roomUpdate
	watch := roomWatch{lane: route.Lane, route: route, debt: true, leverage: true, wakes: true}
	view.WatchRoom([]roomWatch{watch}, func(u roomUpdate) { reports = append(reports, u); worker.roomObserved(u) })
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
	view.WatchRoom([]roomWatch{watch}, worker.roomObserved)
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
	gauge.leverage, gauge.wakes = false, false
	view.WatchRoom([]roomWatch{gauge}, worker.roomObserved)
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
	if len(watches) != 2 || !watches[0].wakes || !watches[0].debt || watches[1].wakes || watches[1].lane != onreONycUSDC {
		t.Fatalf("watches: %+v", watches)
	}
	// A level below the position wakes nothing; one above it does.
	o.lanes[1].LeverageTargetLevel = 1.75
	watches = leverageRoomWatches(o)
	if len(watches) != 2 || !watches[1].wakes {
		t.Fatalf("a lane below its level is not woken on: %+v", watches)
	}
}

// The reaction clock starts when a lane below its target first shows room
// for the minimum borrow and stops when its leverage_up borrow lands: the
// histogram, the per-lane counter, the room gauge and one log line.
func TestLeverageReactionIsMeasuredFromTheRoomOpening(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	registry := prometheus.NewRegistry()
	e := newEvents(slog.New(slog.NewJSONHandler(&out, nil)), engine.NewFacts(registry))
	now := time.Unix(kaminoFixtureUnix, 0)
	e.now = func() time.Time { return now }
	route, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	watch := roomWatch{lane: route.Lane, route: route, debt: true, leverage: true, wakes: true}
	e.leverageObserved([]laneLeverage{{lane: route.Lane, targeted: true, target: 1.75, current: 1.01, gap: 0.74}})
	e.roomObserved(roomUpdate{watch: watch, roomRaw: 0, roomUSD: 0, at: now.Add(-time.Minute)})
	e.roomObserved(roomUpdate{watch: watch, roomRaw: 3_300_000_000, roomUSD: 3300, open: true, opened: true, at: now.Add(-7 * time.Second)})
	e.roomObserved(roomUpdate{watch: watch, roomRaw: 3_400_000_000, roomUSD: 3400, open: true, at: now.Add(-3 * time.Second)})
	e.leverageUpLanded(route.Lane, 3_300_000_000, "sig")
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, family := range families {
		for _, m := range family.GetMetric() {
			switch {
			case m.GetHistogram() != nil:
				got[family.GetName()+"_sum"], got[family.GetName()+"_count"] = m.GetHistogram().GetSampleSum(), float64(m.GetHistogram().GetSampleCount())
			case m.GetCounter() != nil:
				got[family.GetName()] = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				got[family.GetName()] = m.GetGauge().GetValue()
			}
		}
	}
	if got["loyal_backyard_leverage_reaction_seconds_sum"] != 7 || got["loyal_backyard_leverage_reaction_seconds_count"] != 1 ||
		got["loyal_backyard_leverage_up_landed_total"] != 1 || got["loyal_backyard_leverage_borrow_room_usd"] != 3400 || got["loyal_backyard_leverage_gap"] != 0.74 {
		t.Fatalf("leverage metrics: %v", got)
	}
	if !strings.Contains(out.String(), `"msg":"backyard_leverage_up_landed"`) || !strings.Contains(out.String(), `"reaction_seconds":7`) || !strings.Contains(out.String(), `"room_raw":3400000000`) {
		t.Fatalf("landed log line: %s", out.String())
	}
	// The lane is no longer held: its series go away, not to zero.
	e.leverageObserved(nil)
	families, _ = registry.Gather()
	for _, family := range families {
		if strings.HasPrefix(family.GetName(), "loyal_backyard_leverage_gap") || strings.HasPrefix(family.GetName(), "loyal_backyard_leverage_borrow_room") {
			t.Fatalf("a lane no longer held kept %s", family.GetName())
		}
	}
}
