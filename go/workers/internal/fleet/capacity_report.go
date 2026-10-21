package fleet

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	capacityHeartbeat    = time.Hour
	capacityBucketMicros = 10_000_000_000
	apyBucketBPS         = 50
)

// ReserveCapacity is one reserve's line in kamino_fleet_capacity: the capital
// the planned holdings keep in it, what it pays, and the best same-mint
// destination with the room that destination has left this cycle. Vaults
// with an open opportunity or decision are not planned, so their capital is
// not in Held.
type ReserveCapacity struct {
	Reserve                     string `json:"reserve"`
	Mint                        string `json:"mint"`
	Holdings                    int    `json:"holdings"`
	HeldUSDMicros               int64  `json:"heldUsdMicros"`
	MovableUSDMicros            int64  `json:"movableUsdMicros"`
	APYBPS                      int64  `json:"apyBps"`
	AvailableLiquidityUSDMicros *int64 `json:"availableLiquidityUsdMicros"`
	Best                        string `json:"bestDestination,omitempty"`
	BestAPYBPS                  int64  `json:"bestDestinationApyBps,omitempty"`
	BestRoomUSDMicros           *int64 `json:"bestDestinationRoomUsdMicros"`
}

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
			line = &ReserveCapacity{Reserve: p.SourceReserve, Mint: p.Mint, MovableUSDMicros: plan.Movable[p.SourceReserve]}
			if state, ok := snapshot.Reserves[p.SourceReserve]; ok {
				line.APYBPS, line.AvailableLiquidityUSDMicros = state.SupplyAPYBPS, state.AvailableLiquidityRaw
			}
			lines[p.SourceReserve] = line
		}
		line.Holdings++
		line.HeldUSDMicros += p.AmountRaw
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
			line.BestRoomUSDMicros = destinationRoom(state, committed[line.Best])
		}
		report = append(report, *line)
	}
	sort.Slice(report, func(i, j int) bool { return report[i].Reserve < report[j].Reserve })
	return report
}

func destinationRoom(target ReserveState, committedInflow int64) *int64 {
	deposit, known := target.DepositRoomRaw()
	if !known {
		return nil
	}
	room := minInt64(maxInt64(target.SupplyUSDMicros()/50, 4_000_000), deposit)
	room = maxInt64(room-committedInflow, 0)
	return &room
}

func capacityChangeKey(report []ReserveCapacity) string {
	var key strings.Builder
	bucket := func(micros int64) int64 { return micros / capacityBucketMicros }
	for _, line := range report {
		room := "unknown"
		if line.BestRoomUSDMicros != nil {
			room = fmt.Sprint(bucket(*line.BestRoomUSDMicros))
		}
		fmt.Fprintf(&key, "%s:%d:%d:%d:%d:%s:%d:%s;", line.Reserve, line.Holdings, bucket(line.HeldUSDMicros), bucket(line.MovableUSDMicros), line.APYBPS/apyBucketBPS, line.Best, line.BestAPYBPS/apyBucketBPS, room)
	}
	return key.String()
}

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
