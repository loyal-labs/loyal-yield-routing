package backyard

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// leverageSpread is token yield minus the borrow APY at the utilization the
// lane's hypothetical debt would reach at that level. Current raw source debt
// is already included in observed utilization. The hypothetical debt begins as
// USDC value and becomes raw debt units at the lane's own verified reserve
// prices (DebtRawPerUSDCRaw); a lane without that price omits the column.
func leverageSpread(m LaneEconomics, level float64, equityRaw int64, source bool, observed ...Snapshot) (float64, bool) {
	debt, ok := levelDebtRaw(m, level, equityRaw)
	if !ok {
		return 0, false
	}
	if source && len(observed) == 1 && observed[0].RouteLane == m.Lane {
		if observed[0].PositionDebtRaw < 0 {
			return 0, false
		}
		debt -= float64(observed[0].PositionDebtRaw)
	}
	return leverageBorrowSpread(m, max(debt, 0))
}

func levelDebtRaw(m LaneEconomics, level float64, equityRaw int64) (float64, bool) {
	debt := (level - 1) * float64(equityRaw)
	if !finite(debt) || debt < 0 || debt > 1<<53 {
		return 0, false
	}
	if debt > 0 {
		l, ok := findEarnLane(m.Lane)
		if !ok {
			return 0, false
		}
		if l.route.Kamino.DebtMint != bridgeUSDC {
			if !finite(m.DebtRawPerUSDCRaw) || m.DebtRawPerUSDCRaw <= 0 {
				return 0, false
			}
			debt = math.Ceil(debt * m.DebtRawPerUSDCRaw)
		}
	}
	return debt, true
}

func leverageLevelAPY(m LaneEconomics, level float64, equityRaw int64, source bool, feeBPS int64, observed ...Snapshot) (float64, bool) {
	spread, ok := leverageSpread(m, level, equityRaw, source, observed...)
	if !ok {
		return 0, false
	}
	return performanceFeeForecast(m.NativeAPY+m.SupplyAPY+(level-1)*spread, feeBPS), true
}

func performanceFeeForecast(apy float64, feeBPS int64) float64 {
	if apy <= 0 || feeBPS == 0 {
		return apy
	}
	return math.Expm1((1 - float64(feeBPS)/10_000) * math.Log1p(apy))
}

type leverageWatch struct {
	// summary is the structured copy of the last summary line (nil when the
	// last observe printed none): the numbers the line printed, for storage.
	summary []leverageWatchLane
}

type leverageWatchLane struct {
	Lane       string           `json:"lane"`
	ObservedAt time.Time        `json:"observedAt"`
	SpreadBPS  *int64           `json:"spreadBps,omitempty"`
	Enterable  bool             `json:"enterable"`
	APYBPS     map[string]int64 `json:"apyBps"`
	// NativeBPS is the realized native APY the lane is scored on;
	// FeedNativeBPS is the yield feed's figure, display only.
	NativeBPS     int64  `json:"nativeBps"`
	FeedNativeBPS *int64 `json:"feedNativeBps,omitempty"`
}

func (w *leverageWatch) observe(markets []LaneEconomics, sourceLane string, equityRaw int64, summary bool, feeBPS int64, observed ...Snapshot) []string {
	w.summary = nil
	if !summary {
		return nil
	}
	var lanes []string
	var summaryLanes []leverageWatchLane
	sorted := append([]LaneEconomics(nil), markets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Lane < sorted[j].Lane })
	for _, m := range sorted {
		source := m.Lane == sourceLane
		lane := leverageWatchLane{Lane: m.Lane, APYBPS: map[string]int64{}, Enterable: earnActiveLane(m.Lane),
			NativeBPS: int64(math.Round(m.NativeAPY * 10_000))}
		native := nativeFeedText(float64(lane.NativeBPS)/10_000, nil)
		if m.FeedNativeAPY != nil {
			feed := int64(math.Round(*m.FeedNativeAPY * 10_000))
			lane.FeedNativeBPS = &feed
			printed := float64(feed) / 10_000
			native = nativeFeedText(float64(lane.NativeBPS)/10_000, &printed)
		}
		parts := []string{}
		for _, r := range leverageLadder {
			if apy, ok := leverageLevelAPY(m, r.level, equityRaw, source, feeBPS, observed...); ok {
				bps := int64(math.Round(apy * 10_000))
				lane.APYBPS[fmt.Sprintf("%.2f", r.level)] = bps
				parts = append(parts, fmt.Sprintf("%.2fx=%.2f", r.level, float64(bps)/100))
			}
		}
		spreadText := "unavailable"
		if spread, known := leverageSpread(m, 1.5, equityRaw, source, observed...); known {
			bps := int64(math.Round(spread * 10_000))
			lane.SpreadBPS = &bps
			spreadText = fmt.Sprintf("%.2f", float64(bps)/100)
		}
		entry := "no"
		if lane.Enterable {
			entry = "yes"
		}
		// The printed line and the stored summary are the same values.
		lanes = append(lanes, fmt.Sprintf("%s(spread=%s %s enterable=%s apy %s)", m.Lane, spreadText, native, entry, strings.Join(parts, " ")))
		summaryLanes = append(summaryLanes, lane)
	}
	if len(lanes) == 0 {
		return nil
	}
	w.summary = summaryLanes
	return []string{"backyard-rwa-worker: leverage watch " + strings.Join(lanes, " ")}
}
