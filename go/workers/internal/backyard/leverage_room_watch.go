package backyard

import (
	"encoding/binary"
	"math"
	"math/big"
	"sort"
	"time"
)

// Leverage reacts to reserve room the moment it opens. The account view
// already streams every lane's reserves over LaserStream; the worker tells it
// which reserves it is waiting on (roomWatch), and the view recomputes their
// room from each update it applies. When a watched reserve's room crosses
// from below to at or above the minimum, the view wakes the worker's tick
// loop: no polling, no second loop, and the tick that wakes plans from its
// own observation as always.

// roomWatch is one reserve the worker waits on. Debt watches borrow room on
// a lane's debt reserve (a held lane below its stored leverage target, or a
// tranche destination); collateral watches deposit room on a tranche
// destination's collateral reserve. leverage marks a lane below its target,
// the only watches the leverage reaction time is measured on.
type roomWatch struct {
	lane     string
	route    RuntimeRoute
	debt     bool
	leverage bool
	// wakes: an opening wakes the tick loop (a lane below its target or a
	// tranche destination); other held lanes are watched for the gauge.
	wakes bool
}

func (w roomWatch) reserve() string {
	if w.debt {
		return w.route.Kamino.DebtReserve
	}
	return w.route.Kamino.CollateralReserve
}

// roomUpdate is one watched reserve's room as the view applied it.
type roomUpdate struct {
	watch   roomWatch
	roomRaw uint64
	// roomUSD is the room at the reserve's own oracle price; NaN when the
	// price is not decodable (unknown is not zero).
	roomUSD float64
	open    bool
	// opened: the room crossed from below the minimum to at or above it.
	opened bool
	slot   uint64
	at     time.Time
}

// roomOf recomputes a watched reserve's room from its account alone, at
// chain time now. Debt room is kaminoDebtReserveRoom, open when the
// fee-inclusive receive it allows reaches the minimum borrow. Deposit room is
// the collateral reserve's remaining deposit limit, open at $10 of it.
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

// leverageRoomWatches is what a tick watches: the debt reserve of every held
// lane (for the room gauge), waking on the ones below their stored leverage
// target, and a tranche destination's debt and collateral reserves.
func leverageRoomWatches(o Observation) []roomWatch {
	var out []roomWatch
	index := map[string]int{}
	add := func(lane string, debt, leverage, wakes bool) {
		route, err := runtimeRoute(lane)
		if err != nil || !earnActiveLane(lane) {
			return
		}
		w := roomWatch{lane: lane, route: route, debt: debt, leverage: leverage, wakes: wakes}
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
	if o.planning != nil && o.planning.tranche != nil {
		add(o.planning.tranche.Destination, true, false, true)
		add(o.planning.tranche.Destination, false, false, true)
	}
	return out
}

// laneLeverage is one held lane's actual leverage (collateral over equity)
// and, when it has a stored target, the target minus it.
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

// heldLaneLeverage is every held lane with positive equity in one
// observation: the working lane from its snapshot, the others from their
// observed values.
func heldLaneLeverage(o Observation) []laneLeverage {
	values := map[string][2]int64{}
	terms := map[string]int64{}
	s := o.Snapshot
	if s.HasPosition && s.PositionCollateralValueRaw > 0 {
		values[s.RouteLane] = [2]int64{s.PositionCollateralValueRaw, s.PositionDebtValueRaw}
		if s.PositionDebtRaw > 0 {
			terms[s.RouteLane] = s.DebtTermEndUnix
		}
	}
	for _, f := range o.heldFacts {
		values[f.Lane] = [2]int64{f.CollateralValueRaw, f.DebtValueRaw}
		if f.DebtValueRaw > 0 {
			terms[f.Lane] = f.DebtTermEndUnix
		}
	}
	var out []laneLeverage
	for lane, v := range values {
		equity := v[0] - v[1]
		if equity <= 0 || v[1] < 0 {
			continue
		}
		l := laneLeverage{lane: lane, current: float64(v[0]) / float64(equity)}
		if end := terms[lane]; end > 0 && s.ChainUnix > 0 {
			l.termDays, l.termOwed = float64(end-s.ChainUnix)/86_400, true
		}
		if o.planning != nil {
			if target, ok := o.planning.leverage[lane]; ok && target.Level > 0 {
				l.targeted, l.target = true, min(target.Level, leverageMaxLiveLevel)
				l.gap = max(l.target-l.current, 0)
			}
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lane < out[j].lane })
	return out
}

// roomObserved is the view's room callback: the gauges and reaction clock,
// and a wake when a watched reserve's room opens.
func (w *Worker) roomObserved(u roomUpdate) {
	backyardEvents.roomObserved(u)
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

// WatchRoom replaces the reserves the view recomputes room for, and the
// callback it reports each recomputation to (under the view lock: the
// callback must not block or read the view). Registering reports each
// watched reserve's current room without an edge: the registering tick
// already planned from it. Only a later stream write can wake.
func (v *View) WatchRoom(watches []roomWatch, on func(roomUpdate)) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.roomWatches, v.roomOpen, v.onRoom = map[string][]roomWatch{}, map[string]bool{}, on
	for _, w := range watches {
		v.roomWatches[w.reserve()] = append(v.roomWatches[w.reserve()], w)
	}
	for reserve, ws := range v.roomWatches {
		if held, ok := v.accounts[reserve]; ok {
			v.observeRoom(ws, held, false)
		}
	}
}

func (v *View) clockUnix() int64 {
	return clockUnixTimestamp([]ConfirmedAccount{v.accounts[budgetClockAddress].ConfirmedAccount})
}

// observeRoom recomputes a watched reserve's room from an applied write and
// reports it; edges wake only when edges is set (a stream write).
func (v *View) observeRoom(ws []roomWatch, write viewAccount, edges bool) {
	now := v.clockUnix()
	for _, w := range ws {
		room, usd, open, ok := roomOf(w, write.ConfirmedAccount, now)
		if !ok {
			continue
		}
		key := w.lane + "|" + w.reserve()
		opened := edges && w.wakes && open && !v.roomOpen[key]
		v.roomOpen[key] = open
		if v.onRoom != nil {
			v.onRoom(roomUpdate{watch: w, roomRaw: room, roomUSD: usd, open: open, opened: opened, slot: write.slot, at: v.now()})
		}
	}
}
