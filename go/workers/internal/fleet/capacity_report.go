package fleet

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// capacityHeartbeat is how often the capacity line is logged when nothing in
// it changed.
const capacityHeartbeat = time.Hour

// ReserveCapacity is one reserve's line in kamino_fleet_capacity: the capital
// the planned holdings keep in it, what it pays, and the best same-mint
// destination with the room that destination has left this cycle. Vaults
// with an open opportunity or decision are not planned, so their capital is
// not in Held. Amounts are USD micros (raw units of a six-decimal stable).
type ReserveCapacity struct {
	Reserve  string `json:"reserve"`
	Mint     string `json:"mint"`
	Holdings int    `json:"holdings"`
	Held     int64  `json:"heldUsdMicros"`
	// Movable is the held capital whose move cleared every planner check.
	Movable   int64  `json:"movableUsdMicros"`
	APYBPS    int64  `json:"apyBps"`
	Available *int64 `json:"availableLiquidityUsdMicros"`
	// Best is empty when no eligible same-mint destination pays more.
	Best       string `json:"bestDestination,omitempty"`
	BestAPYBPS int64  `json:"bestDestinationApyBps,omitempty"`
	// BestRoom is the smallest of the destination's 2% frontier and deposit
	// room, less the committed and this wave's inflows; nil when its deposit
	// limit is unknown.
	BestRoom *int64 `json:"bestDestinationRoomUsdMicros"`
}

// capacityReport builds the per-reserve capacity lines, ordered by reserve.
func capacityReport(snapshot MarketSnapshot, vaults []FleetVault, inflight map[string]int64, plan FleetPlan) []ReserveCapacity {
	committed := map[string]int64{}
	for reserve, value := range inflight {
		committed[reserve] = value
	}
	for _, opportunity := range plan.Opportunities {
		committed[opportunity.Decision.TargetReserve] += opportunity.Decision.PrincipalUSDMicros
	}
	lines := map[string]*ReserveCapacity{}
	for _, vault := range vaults {
		p := vault.Position
		line := lines[p.SourceReserve]
		if line == nil {
			line = &ReserveCapacity{Reserve: p.SourceReserve, Mint: p.Mint, Movable: plan.Movable[p.SourceReserve]}
			if state, ok := snapshot.Reserves[p.SourceReserve]; ok {
				line.APYBPS, line.Available = state.SupplyAPYBPS, state.AvailableLiquidityRaw
			}
			lines[p.SourceReserve] = line
		}
		line.Holdings++
		line.Held += p.AmountRaw
	}
	report := make([]ReserveCapacity, 0, len(lines))
	for _, line := range lines {
		for _, candidate := range snapshot.Reserves {
			if !candidate.TargetEligible() || candidate.Mint != line.Mint || candidate.Address == line.Reserve || candidate.SupplyAPYBPS <= line.APYBPS {
				continue
			}
			if line.Best == "" || candidate.SupplyAPYBPS > line.BestAPYBPS || candidate.SupplyAPYBPS == line.BestAPYBPS && candidate.Address < line.Best {
				line.Best, line.BestAPYBPS = candidate.Address, candidate.SupplyAPYBPS
			}
		}
		if state, ok := snapshot.Reserves[line.Best]; ok {
			line.BestRoom = destinationRoom(state, committed[line.Best])
		}
		report = append(report, *line)
	}
	sort.Slice(report, func(i, j int) bool { return report[i].Reserve < report[j].Reserve })
	return report
}

// destinationRoom is the planner's room for one target: the smaller of its
// 2% frontier and its deposit room, less committed inflows, at least 0.
func destinationRoom(target ReserveState, committedInflow int64) *int64 {
	deposit, known := target.DepositRoomRaw()
	if !known {
		return nil
	}
	room := minInt64(maxInt64(target.TotalSupplyUSDMicros/50, 4_000_000), deposit)
	room = maxInt64(room-committedInflow, 0)
	return &room
}

// capacityChangeKey is the report coarsened to what an operator acts on, so
// accrual and basis-point drift do not re-log it every cycle: capital and
// room to $10k, APYs to 50 bps, the destination identity and holding counts.
func capacityChangeKey(report []ReserveCapacity) string {
	var key strings.Builder
	bucket := func(micros int64) int64 { return micros / 10_000_000_000 }
	for _, line := range report {
		room := "unknown"
		if line.BestRoom != nil {
			room = fmt.Sprint(bucket(*line.BestRoom))
		}
		fmt.Fprintf(&key, "%s:%d:%d:%d:%d:%s:%d:%s;", line.Reserve, line.Holdings, bucket(line.Held), bucket(line.Movable), line.APYBPS/50, line.Best, line.BestAPYBPS/50, room)
	}
	return key.String()
}

// capacityLog emits kamino_fleet_capacity when the coarse report changes and
// at least once per heartbeat.
type capacityLog struct {
	key  string
	last time.Time
}

func (c *capacityLog) due(now time.Time, report []ReserveCapacity) bool {
	key := capacityChangeKey(report)
	if key == c.key && now.Sub(c.last) < capacityHeartbeat {
		return false
	}
	c.key, c.last = key, now
	return true
}
