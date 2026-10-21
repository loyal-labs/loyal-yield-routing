package backyard

import (
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

type roomWatch struct {
	lane        string
	route       RuntimeRoute
	belowTarget bool
}

type roomKey struct{ lane, reserve string }

func (w roomWatch) key() roomKey { return roomKey{w.lane, w.reserve()} }

func (w roomWatch) reserve() string {
	return w.route.Kamino.DebtReserve
}

type roomUpdate struct {
	watch      roomWatch
	roomRaw    uint64
	roomUSD    float64
	roomPriced bool
	open       bool
	opened     bool
	slot       uint64
	at         time.Time
}

func roomOf(w roomWatch, account ConfirmedAccount, now int64) (roomUpdate, bool) {
	debt, err := decodeKaminoReserve(account, w.route.Kamino.DebtMint, w.route.Kamino)
	if err != nil {
		return roomUpdate{}, false
	}
	room, err := kaminoDebtReserveRoom(debt, now)
	if err != nil {
		return roomUpdate{}, false
	}
	usd, priced := reserveUSD(room, debt)
	return roomUpdate{watch: w, roomRaw: room, roomUSD: usd, roomPriced: priced, open: kaminoReceiveWithinRoom(room, debt.terms.BorrowFeeSF) >= leverageMinimumBorrowRaw}, true
}

func reserveUSD(raw uint64, r decodedKaminoReserve) (float64, bool) {
	price := littleInt(r.terms.MarketPriceSF[:])
	if price.Sign() <= 0 {
		return 0, false
	}
	v, _ := new(big.Rat).SetFrac(new(big.Int).Mul(new(big.Int).SetUint64(raw), price), new(big.Int).Lsh(big.NewInt(1), kamino.FractionBits)).Float64()
	return v / math.Pow10(int(r.mintDecimals)), true
}

func leverageRoomWatches(o Observation) []roomWatch {
	var out []roomWatch
	index := map[roomKey]int{}
	add := func(lane string, belowTarget bool) {
		l, ok := findEarnLane(lane)
		if !ok || l.exitOnly {
			return
		}
		w := roomWatch{lane: lane, route: l.route, belowTarget: belowTarget}
		if i, seen := index[w.key()]; seen {
			out[i].belowTarget = out[i].belowTarget || belowTarget
			return
		}
		index[w.key()] = len(out)
		out = append(out, w)
	}
	for _, lane := range heldLaneLeverage(o) {
		below := lane.targeted && lane.gap > 0
		add(lane.lane, below)
	}
	return out
}

type laneLeverage struct {
	lane            string
	targeted        bool
	target, current float64
	gap             float64
	termDays        float64
	termOwed        bool
}

func heldLaneLeverage(o Observation) []laneLeverage {
	var out []laneLeverage
	for _, s := range o.lanes {
		equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
		if !s.HasPosition || s.PositionCollateralValueRaw <= 0 || equity <= 0 || s.PositionDebtValueRaw < 0 {
			continue
		}
		l := laneLeverage{lane: s.RouteLane, current: float64(s.PositionCollateralValueRaw) / float64(equity)}
		l.termDays, l.termOwed = termDays(s)
		if s.LeverageTargetLevel > 0 {
			l.targeted, l.target = true, min(s.LeverageTargetLevel, laneMaxLevel(s.RouteLane))
			l.gap = max(l.target-l.current, 0)
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lane < out[j].lane })
	return out
}

func (w *Worker) roomObserved(u roomUpdate) {
	w.events.roomObserved(u)
	if u.opened {
		w.requestTick()
	}
}

type roomSet struct {
	watches map[string][]roomWatch
	open    map[roomKey]bool
	on      func(roomUpdate)
}

func (v *View) WatchRoom(vault *Vault, watches []roomWatch, on func(roomUpdate)) {
	if v == nil {
		return
	}
	defer v.flushRooms()
	v.mu.Lock()
	defer v.mu.Unlock()
	set := &roomSet{watches: map[string][]roomWatch{}, open: map[roomKey]bool{}, on: on}
	if v.rooms == nil {
		v.rooms = map[*Vault]*roomSet{}
	}
	v.rooms[vault] = set
	for _, w := range watches {
		set.watches[w.reserve()] = append(set.watches[w.reserve()], w)
	}
	for reserve, ws := range set.watches {
		if held, ok := v.accounts[reserve]; ok {
			v.queueRoomLocked(set, ws, held, false)
		}
	}
}

func (v *View) clockUnix() int64 {
	return clockUnixTimestamp([]ConfirmedAccount{v.accounts[budgetClockAddress].ConfirmedAccount})
}

type roomDelivery struct {
	on     func(roomUpdate)
	update roomUpdate
}

func (v *View) queueRoomLocked(set *roomSet, ws []roomWatch, write viewAccount, edges bool) {
	now := v.clockUnix()
	for _, w := range ws {
		u, ok := roomOf(w, write.ConfirmedAccount, now)
		if !ok {
			continue
		}
		u.opened = edges && w.belowTarget && u.open && !set.open[w.key()]
		set.open[w.key()] = u.open
		if set.on != nil {
			u.slot, u.at = write.slot, v.now()
			v.roomOut = append(v.roomOut, roomDelivery{set.on, u})
		}
	}
}

func (v *View) flushRooms() {
	v.delivering.Lock()
	defer v.delivering.Unlock()
	v.mu.Lock()
	out := v.roomOut
	v.roomOut = nil
	v.mu.Unlock()
	for _, d := range out {
		d.on(d.update)
	}
}
