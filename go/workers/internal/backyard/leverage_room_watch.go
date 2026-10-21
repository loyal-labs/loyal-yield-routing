package backyard

import (
	"encoding/binary"
	"math"
	"math/big"
	"sort"
	"time"
)

// roomWatch is one reserve the worker waits on. Debt watches borrow room on
// a held lane's debt reserve; collateral watches deposit room on a
// collateral reserve. leverage marks a lane below its target, the only
// watches the leverage reaction time is measured on.
type roomWatch struct {
	lane     string
	route    RuntimeRoute
	debt     bool
	leverage bool
	// wakes: an opening wakes the tick loop (a lane below its target);
	// other held lanes are watched for the gauge.
	wakes bool
}

func (w roomWatch) reserve() string {
	if w.debt {
		return w.route.Kamino.DebtReserve
	}
	return w.route.Kamino.CollateralReserve
}

type roomUpdate struct {
	watch   roomWatch
	roomRaw uint64
	// roomUSD is the room at the reserve's own oracle price; NaN when the
	// price is not decodable (unknown is not zero).
	roomUSD float64
	open    bool
	opened  bool
	slot    uint64
	at      time.Time
}

func roomOf(w roomWatch, account ConfirmedAccount, now int64) (room uint64, usd float64, open bool, ok bool) {
	if w.debt {
		debt, err := decodeKaminoReserve(account, w.route.Kamino.DebtMint, w.route.Kamino)
		if err != nil {
			return 0, 0, false, false
		}
		room, err = kaminoDebtReserveRoom(account, debt, now)
		if err != nil {
			return 0, 0, false, false
		}
		rate := binary.LittleEndian.Uint64(account.Data[kaminoReserveConfigOffset+40:])
		return room, reserveUSD(room, debt), kaminoReceiveWithinRoom(room, rate) >= leverageMinimumBorrowRaw, true
	}
	collateral, err := decodeKaminoReserve(account, w.route.Kamino.CollateralMint, w.route.Kamino)
	if err != nil || collateral.totalLiquiditySF == nil {
		return 0, 0, false, false
	}
	deposited, err := ceilScaledBigFraction(collateral.totalLiquiditySF)
	if err != nil {
		return 0, 0, false, false
	}
	if collateral.depositLimitRaw > deposited {
		room = collateral.depositLimitRaw - deposited
	}
	usd = reserveUSD(room, collateral)
	return room, usd, usd >= float64(leverageMinimumBorrowRaw)/1e6, true
}

// reserveUSD values raw liquidity at the reserve's own Q60 market price.
func reserveUSD(raw uint64, r decodedKaminoReserve) float64 {
	price := littleInt(r.marketPriceSF[:])
	if price.Sign() <= 0 {
		return math.NaN()
	}
	v, _ := new(big.Rat).SetFrac(new(big.Int).Mul(new(big.Int).SetUint64(raw), price), new(big.Int).Lsh(big.NewInt(1), 60)).Float64()
	return v / math.Pow10(int(r.mintDecimals))
}

func leverageRoomWatches(o Observation) []roomWatch {
	var out []roomWatch
	index := map[string]int{}
	add := func(lane string, debt, leverage, wakes bool) {
		l, ok := findEarnLane(lane)
		if !ok || l.exitOnly {
			return
		}
		w := roomWatch{lane: lane, route: l.route, debt: debt, leverage: leverage, wakes: wakes}
		key := lane + "|" + w.reserve()
		if i, seen := index[key]; seen {
			out[i].leverage, out[i].wakes = out[i].leverage || leverage, out[i].wakes || wakes
			return
		}
		index[key] = len(out)
		out = append(out, w)
	}
	for _, lane := range heldLaneLeverage(o) {
		below := lane.targeted && lane.gap > 0
		add(lane.lane, true, below, below)
	}
	return out
}

type laneLeverage struct {
	lane            string
	targeted        bool
	target, current float64
	gap             float64
	// termDays is the lane's days to its term debt's maturity, when it owes
	// term debt of known maturity.
	termDays float64
	termOwed bool
}

func heldLaneLeverage(o Observation) []laneLeverage {
	var out []laneLeverage
	for _, s := range o.lanes {
		equity := s.PositionCollateralValueRaw - s.PositionDebtValueRaw
		if !s.HasPosition || s.PositionCollateralValueRaw <= 0 || equity <= 0 || s.PositionDebtValueRaw < 0 {
			continue
		}
		l := laneLeverage{lane: s.RouteLane, current: float64(s.PositionCollateralValueRaw) / float64(equity)}
		if s.PositionDebtRaw > 0 && s.DebtTermEndUnix > 0 && s.ChainUnix > 0 {
			l.termDays, l.termOwed = float64(s.DebtTermEndUnix-s.ChainUnix)/86_400, true
		}
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
		w.notifyRoomOpened()
	}
}

// notifyRoomOpened wakes the tick loop like a selector commit does: a queued
// wake survives an active tick.
func (w *Worker) notifyRoomOpened() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// roomSet is one vault's watches: reserve address to watches, each watch's
// last open state by lane|reserve, and the callback every recomputation
// reports to.
type roomSet struct {
	watches map[string][]roomWatch
	open    map[string]bool
	on      func(roomUpdate)
}

// WatchRoom replaces the reserves the view recomputes room for on vault's
// behalf, and the callback it reports each recomputation to (under the view
// lock: the callback must not block or read the view). Registering reports
// each watched reserve's current room without an edge: the registering tick
// already planned from it. Only a later stream write can wake.
func (v *View) WatchRoom(vault *Vault, watches []roomWatch, on func(roomUpdate)) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	set := &roomSet{watches: map[string][]roomWatch{}, open: map[string]bool{}, on: on}
	if v.rooms == nil {
		v.rooms = map[*Vault]*roomSet{}
	}
	v.rooms[vault] = set
	for _, w := range watches {
		set.watches[w.reserve()] = append(set.watches[w.reserve()], w)
	}
	for reserve, ws := range set.watches {
		if held, ok := v.accounts[reserve]; ok {
			v.observeRoom(set, ws, held, false)
		}
	}
}

func (v *View) clockUnix() int64 {
	return clockUnixTimestamp([]ConfirmedAccount{v.accounts[budgetClockAddress].ConfirmedAccount})
}

func (v *View) observeRoom(set *roomSet, ws []roomWatch, write viewAccount, edges bool) {
	now := v.clockUnix()
	for _, w := range ws {
		room, usd, open, ok := roomOf(w, write.ConfirmedAccount, now)
		if !ok {
			continue
		}
		key := w.lane + "|" + w.reserve()
		opened := edges && w.wakes && open && !set.open[key]
		set.open[key] = open
		if set.on != nil {
			set.on(roomUpdate{watch: w, roomRaw: room, roomUSD: usd, open: open, opened: opened, slot: write.slot, at: v.now()})
		}
	}
}
