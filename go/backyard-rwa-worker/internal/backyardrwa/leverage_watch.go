package backyardrwa

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// B2 watch-only (docs/plans/b2-variable-leverage-design.md). Scores each lane
// at each leverage level from the same selector feed and simulates, per design
// option, which level the lane would hold. It never feeds a decision and
// sends nothing: its only outputs are log lines.
// ponytail: virtual levels live in memory and restart at 1.5x with the worker.

var leverageWatchLevels = []float64{1, 1.5, 1.75, 2}

// leverageStep: move from -> to when the spread at `to` (up) or at `from`
// (down) crosses min. Up needs spread >= min, down fires at spread < min.
type leverageStep struct {
	from, to, min float64
}

// Thresholds in APY points (0.01 = 1 pt). The up/down gap stops flip-flopping.
var leverageWatchOptions = map[string][]leverageStep{
	"1": {{1, 1.5, 0.01}, {1.5, 1.75, 0.02}, {1.75, 1.5, 0.01}, {1.5, 1, 0}},
	"2": {{1, 1.5, 0.01}, {1.5, 1.75, 0.02}, {1.75, 2, 0.025}, {2, 1.75, 0.015}, {1.75, 1.5, 0.01}, {1.5, 1, 0}},
	"3": {{1, 1.5, 0.01}, {1.5, 1, 0}},
}

// leverageSpread is token yield minus the borrow APY at the utilization the
// lane's debt would reach at that level. The source lane already carries a
// 1.5x loan, which the observed utilization includes.
func leverageSpread(m LaneEconomics, level float64, equityRaw int64, source bool) (float64, bool) {
	debt := (level - 1) * float64(equityRaw)
	if source {
		debt -= 0.5 * float64(equityRaw)
	}
	apr, err := projectedBorrowAPR(m, max(debt, 0))
	if err != nil || !finite(apr) {
		return 0, false
	}
	return m.NativeAPY + m.SupplyAPY - math.Expm1(apr), true
}

// nextLeverageLevel applies at most one step from the current level.
func nextLeverageLevel(steps []leverageStep, current float64, spreadAt func(float64) (float64, bool)) float64 {
	for _, s := range steps {
		if s.from != current {
			continue
		}
		up := s.to > s.from
		at := s.from
		if up {
			at = s.to
		}
		spread, ok := spreadAt(at)
		if !ok {
			continue
		}
		if (up && spread >= s.min) || (!up && spread < s.min) {
			return s.to
		}
	}
	return current
}

type leverageWatch struct {
	levels map[string]float64 // option|lane -> virtual level
}

// observe returns one line per virtual move and, when summary is set, one
// line with each lane's spread and APY by level.
func (w *leverageWatch) observe(markets []LaneEconomics, sourceLane string, equityRaw int64, summary bool, enterable func(string) bool) []string {
	if w.levels == nil {
		w.levels = map[string]float64{}
	}
	var lines, lanes []string
	sorted := append([]LaneEconomics(nil), markets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Lane < sorted[j].Lane })
	for _, m := range sorted {
		source := m.Lane == sourceLane
		spreadAt := func(level float64) (float64, bool) { return leverageSpread(m, level, equityRaw, source) }
		for _, option := range []string{"1", "2", "3"} {
			key := option + "|" + m.Lane
			current, ok := w.levels[key]
			if !ok {
				current = 1.5
			}
			next := nextLeverageLevel(leverageWatchOptions[option], current, spreadAt)
			w.levels[key] = next
			if next != current {
				spread, _ := spreadAt(max(current, next))
				lines = append(lines, fmt.Sprintf("backyard-rwa-worker: leverage watch move option=%s lane=%s %.2fx->%.2fx spread=%.2f", option, m.Lane, current, next, spread*100))
			}
		}
		if summary {
			parts := []string{}
			for _, level := range leverageWatchLevels {
				if spread, ok := spreadAt(level); ok {
					parts = append(parts, fmt.Sprintf("%.2fx=%.2f", level, (m.NativeAPY+m.SupplyAPY+(level-1)*spread)*100))
				}
			}
			spread, _ := spreadAt(1.5)
			entry := "no"
			if enterable(m.Lane) {
				entry = "yes"
			}
			lanes = append(lanes, fmt.Sprintf("%s(spread=%.2f enterable=%s apy %s levels 1/2/3=%.2f/%.2f/%.2f)", m.Lane, spread*100, entry, strings.Join(parts, " "),
				w.levels["1|"+m.Lane], w.levels["2|"+m.Lane], w.levels["3|"+m.Lane]))
		}
	}
	if summary && len(lanes) > 0 {
		lines = append(lines, "backyard-rwa-worker: leverage watch "+strings.Join(lanes, " "))
	}
	return lines
}
