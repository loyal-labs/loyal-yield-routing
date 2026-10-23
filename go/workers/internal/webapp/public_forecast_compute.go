package webapp

// Pure port of web/apps/web/src/lib/kamino/earn-realized-apy.shared.ts and
// earn-fleet-allocation.shared.ts: realized Earn APY from Kamino reserve share
// prices. Signed share-price growth measures realized reserve returns; the
// headline annualizes the observed fleet allocation's return.

import (
	"cmp"
	"math"
	"slices"
	"time"
)

const (
	forecastHourMS = int64(60 * 60 * 1000)
	forecastDayMS  = 24 * forecastHourMS
	forecastYearMS = 365 * forecastDayMS

	forecastRealizedWindowMS  = 7 * forecastDayMS
	forecastSeriesWindowMS    = 30 * forecastDayMS
	forecastLiveWindowMS      = forecastDayMS
	forecastLiveMinWindowMS   = 6 * forecastHourMS
	forecastMaxStalenessMS    = 3 * forecastHourMS
	forecastMaxInterpGapMS    = 6 * forecastHourMS
	forecastMaxAllocGapMS     = 6 * forecastHourMS
	forecastMinCoveredShare   = 0.9
	forecastMaxExcludedShare  = 0.01
	forecastIdleReserve       = "__idle__"
	forecastFleetVaultID      = "fleet"
	forecastRealizedStrategy  = "realized_7d_share_price"
	forecastRealizedMetric    = "realized_7d_apy_bps"
	forecastCrossMintFeeBps   = 1
	forecastSafeRiskProfile   = "safe"
	forecastCacheTTLMS        = int64(5 * 60 * 1000)
	forecastPreWindowLookback = int64(2 * 60 * 60 * 1000)
)

// Kamino main market constants (earn-forecast.server.ts:26-31).
const (
	KaminoMainMarket            = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	KaminoMainMarketUSDCReserve = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"
	KaminoMainMarketUSDCMint    = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)

type forecastSharePricePoint struct {
	observedAtMS int64
	sharePrice   float64
}

// forecastWeights preserves the insertion order of the TS Map it ports: the
// weighted sums iterate reserves in first-seen order, and float addition is
// order-sensitive.
type forecastWeights struct {
	order  []string
	byName map[string]float64
}

func newForecastWeights() forecastWeights {
	return forecastWeights{byName: map[string]float64{}}
}

func (w forecastWeights) len() int { return len(w.order) }

func (w *forecastWeights) add(reserve string, delta float64) {
	if _, ok := w.byName[reserve]; !ok {
		w.order = append(w.order, reserve)
	}
	w.byName[reserve] += delta
}

func (w forecastWeights) anyPositive() bool {
	for _, reserve := range w.order {
		if w.byName[reserve] > 0 {
			return true
		}
	}
	return false
}

func (w forecastWeights) clone() forecastWeights {
	byName := make(map[string]float64, len(w.byName))
	for reserve, weight := range w.byName {
		byName[reserve] = weight
	}
	return forecastWeights{order: append([]string(nil), w.order...), byName: byName}
}

type forecastSnapshot struct {
	vault         int
	observedAtMS  int64
	weights       forecastWeights
	idleAmountRaw float64
	unsupported   bool
}

type forecastAllocationHistory struct {
	currentIdleMismatch bool
	snapshots           []forecastSnapshot
	vaultsFirstSeenMS   []int64
}

// earnAllocationHistoryFromSamples ports earn-fleet-allocation.shared.ts:25-69.
// Amounts are raw integer strings; anything else marks the snapshot unsupported.
func earnAllocationHistoryFromSamples(samples []fleetAllocationSample, sinceMS int64) forecastAllocationHistory {
	history := forecastAllocationHistory{vaultsFirstSeenMS: []int64{floorDiv(sinceMS, forecastHourMS) * forecastHourMS}}
	for _, sample := range samples {
		weights := newForecastWeights()
		idle, idleOK := safeSampleWeight(sample.idleAmountRaw)
		excluded, excludedOK := safeSampleWeight(sample.excludedAmountRaw)
		unsupported := !idleOK || !excludedOK
		covered := 0.0
		if idleOK {
			covered = idle
		}
		for _, entry := range sample.reserveAmounts {
			amount, ok := safeSampleWeight(entry.amountRaw)
			switch {
			case !ok:
				unsupported = true
			case amount > 0:
				weights.add(entry.reserve, amount)
				covered += amount
			}
		}
		if excludedOK && excluded > covered*forecastMaxExcludedShare {
			unsupported = true
		}
		history.snapshots = append(history.snapshots, forecastSnapshot{
			observedAtMS:  sample.observedAtMS,
			weights:       weights,
			idleAmountRaw: idle,
			unsupported:   unsupported,
		})
	}
	return history
}

type fleetAllocationSample struct {
	observedAtMS      int64
	reserveAmounts    []fleetReserveAmount
	idleAmountRaw     string
	excludedAmountRaw string
}

type fleetReserveAmount struct {
	reserve   string
	amountRaw string
}

// safeSampleWeight ports safeWeight: /^[0-9]+$/ and a JS safe integer.
func safeSampleWeight(raw string) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	amount, ok := parseSafeInteger(raw)
	if !ok {
		return 0, false
	}
	return amount, true
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// forecastSharePriceAt ports sharePriceAt: last point at or before atMS,
// geometric interpolation across gaps of at most forecastMaxInterpGapMS.
func forecastSharePriceAt(points []forecastSharePricePoint, atMS int64) (float64, bool) {
	low, high := 0, len(points)-1
	index := -1
	for low <= high {
		middle := int(uint(low+high) >> 1)
		if points[middle].observedAtMS <= atMS {
			index = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if index < 0 {
		return 0, false
	}
	before := points[index]
	if before.observedAtMS == atMS {
		return before.sharePrice, true
	}
	if index+1 >= len(points) {
		return 0, false
	}
	after := points[index+1]
	gapMS := after.observedAtMS - before.observedAtMS
	if gapMS > forecastMaxInterpGapMS {
		return 0, false
	}
	fraction := float64(atMS-before.observedAtMS) / float64(gapMS)
	return before.sharePrice * math.Pow(after.sharePrice/before.sharePrice, fraction), true
}

// forecastUnclampedAnnualizedBps ports unclampedAnnualizedBps.
func forecastUnclampedAnnualizedBps(points []forecastSharePricePoint, endMS, windowMS, minWindowMS int64) (float64, bool) {
	if len(points) == 0 {
		return 0, false
	}
	endPrice, ok := forecastSharePriceAt(points, endMS)
	if !ok {
		return 0, false
	}
	startMS := max(endMS-windowMS, points[0].observedAtMS)
	spanMS := endMS - startMS
	if spanMS <= 0 || spanMS < minWindowMS {
		return 0, false
	}
	startPrice, ok := forecastSharePriceAt(points, startMS)
	if !ok || startPrice <= 0 {
		return 0, false
	}
	apy := math.Pow(endPrice/startPrice, float64(forecastYearMS)/float64(spanMS)) - 1
	return apy * 10_000, true
}

func forecastClampAndRoundBps(bps float64) int64 {
	return max(int64(math.Round(bps)), 0)
}

// parseSafeInteger parses a digit-only string as a float64 that stays a JS
// safe integer, mirroring Number(value) + Number.isSafeInteger.
func parseSafeInteger(raw string) (float64, bool) {
	var amount float64
	for i := 0; i < len(raw); i++ {
		amount = amount*10 + float64(raw[i]-'0')
		if amount > 9_007_199_254_740_991 {
			return 0, false
		}
	}
	return amount, true
}

func forecastAllocationIndexAt(timeline []forecastAllocationPoint, atMS int64) int {
	low, high := 0, len(timeline)-1
	index := -1
	for low <= high {
		middle := int(uint(low+high) >> 1)
		if timeline[middle].atMS <= atMS {
			index = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return index
}

// forecastCoveredSegmentGrowth ports coveredSegmentGrowth
// (earn-realized-apy.shared.ts:231-265); iteration stays in insertion order.
func forecastCoveredSegmentGrowth(histories map[string][]forecastSharePricePoint, weights forecastWeights, startMS, endMS int64) (float64, bool) {
	totalWeight, coveredWeight, weightedSum := 0.0, 0.0, 0.0
	for _, reserve := range weights.order {
		weight := weights.byName[reserve]
		if weight <= 0 {
			continue
		}
		totalWeight += weight
		if reserve == forecastIdleReserve {
			coveredWeight += weight
			weightedSum += weight
			continue
		}
		points := histories[reserve]
		startPrice, startOK := forecastSharePriceAt(points, startMS)
		endPrice, endOK := forecastSharePriceAt(points, endMS)
		if startOK && startPrice > 0 && endOK {
			coveredWeight += weight
			weightedSum += weight * (endPrice / startPrice)
		}
	}
	if totalWeight <= 0 || coveredWeight/totalWeight < forecastMinCoveredShare {
		return 0, false
	}
	return weightedSum / coveredWeight, true
}

// forecastPortfolioAnnualizedGrowth ports portfolioAnnualizedGrowth
// (earn-realized-apy.shared.ts:267-308).
func forecastPortfolioAnnualizedGrowth(input forecastRealizedInput, timeline []forecastAllocationPoint, endMS, windowMS, minWindowMS int64) (float64, bool) {
	if len(input.allocations.snapshots) == 0 {
		return 0, false
	}
	firstSnapshot := input.allocations.snapshots[0].observedAtMS
	startMS := max(endMS-windowMS, firstSnapshot)
	spanMS := endMS - startMS
	if spanMS < minWindowMS {
		return 0, false
	}

	atMS := startMS
	growth := 1.0
	for atMS < endMS {
		index := forecastAllocationIndexAt(timeline, atMS)
		if index < 0 || timeline[index].incomplete {
			return 0, false
		}
		state := &timeline[index]
		var nextSnapshotMS int64
		hasNext := false
		if index+1 < len(timeline) {
			nextSnapshotMS, hasNext = timeline[index+1].atMS, true
		}
		nextHour := (floorDiv(atMS, forecastHourMS) + 1) * forecastHourMS
		nextMS := min(endMS, nextHour)
		if hasNext && nextSnapshotMS < nextMS {
			nextMS = nextSnapshotMS
		}
		segment, ok := forecastCoveredSegmentGrowth(input.histories, state.weights, atMS, nextMS)
		if !ok || segment <= 0 {
			return 0, false
		}
		growth *= segment
		atMS = nextMS
	}
	return (math.Pow(growth, float64(forecastYearMS)/float64(spanMS)) - 1) * 10_000, true
}

type forecastRealizedInput struct {
	// Ascending by observedAtMS.
	histories   map[string][]forecastSharePricePoint
	allocations forecastAllocationHistory
	nowMS       int64
}

type forecastApySample struct {
	apyBps     int64
	observedAt string
}

type forecastRealizedResult struct {
	realized7dBps         *int64
	liveBps               *int64
	headlineBps           int64
	source                string // "realized_7d" | "live"
	loyalSeries           []forecastApySample
	mainUsdcReserveSeries []forecastApySample
	measuredAtMS          int64
}

func int64Ptr(v int64) *int64 { return &v }

// forecastComputeRealizedApy ports computeRealizedApy
// (earn-realized-apy.shared.ts:310-410).
func forecastComputeRealizedApy(input forecastRealizedInput) *forecastRealizedResult {
	if input.allocations.currentIdleMismatch {
		return nil
	}
	// Never let a future-dated recorder row set the measurement time or supply
	// the interpolation point for a return that has not yet happened.
	bounded := make(map[string][]forecastSharePricePoint, len(input.histories))
	for reserve, points := range input.histories {
		filtered := points
		for i, point := range points {
			if point.observedAtMS > input.nowMS {
				filtered = points[:i]
				break
			}
		}
		bounded[reserve] = filtered
	}
	timeline := forecastAllocationTimeline(input.allocations)
	// Math.max() of no series is -Infinity, which Number.isFinite rejects.
	measurementEndMS := int64(0)
	sawMeasurement := false
	for _, points := range bounded {
		if len(points) > 0 {
			if latest := points[len(points)-1].observedAtMS; !sawMeasurement || latest > measurementEndMS {
				measurementEndMS, sawMeasurement = latest, true
			}
		}
	}
	if !sawMeasurement {
		return nil
	}
	aligned := false
	for attempt := 0; attempt <= len(timeline); attempt++ {
		index := forecastAllocationIndexAt(timeline, measurementEndMS)
		if index < 0 || timeline[index].incomplete {
			return nil
		}
		state := &timeline[index]
		coveredWeight, totalWeight := 0.0, 0.0
		var latestTimes []int64
		for _, reserve := range state.weights.order {
			weight := state.weights.byName[reserve]
			totalWeight += weight
			if reserve == forecastIdleReserve {
				coveredWeight += weight
				continue
			}
			points := bounded[reserve]
			if len(points) == 0 {
				continue
			}
			latest := points[len(points)-1]
			if input.nowMS-latest.observedAtMS <= forecastMaxStalenessMS {
				coveredWeight += weight
				latestTimes = append(latestTimes, latest.observedAtMS)
			}
		}
		if totalWeight <= 0 || coveredWeight/totalWeight < forecastMinCoveredShare || len(latestTimes) == 0 {
			return nil
		}
		nextEndMS := min(measurementEndMS, slices.Min(latestTimes))
		if nextEndMS == measurementEndMS {
			aligned = true
			break
		}
		measurementEndMS = nextEndMS
	}
	if !aligned {
		return nil
	}
	boundedInput := forecastRealizedInput{histories: bounded, allocations: input.allocations, nowMS: input.nowMS}
	realizedGrowth, realizedOK := forecastPortfolioAnnualizedGrowth(boundedInput, timeline, measurementEndMS, forecastRealizedWindowMS, forecastRealizedWindowMS)
	liveGrowth, liveOK := forecastPortfolioAnnualizedGrowth(boundedInput, timeline, measurementEndMS, forecastLiveWindowMS, forecastLiveMinWindowMS)
	var realized7dBps, liveBps *int64
	if realizedOK {
		realized7dBps = int64Ptr(forecastClampAndRoundBps(realizedGrowth))
	}
	if liveOK {
		liveBps = int64Ptr(forecastClampAndRoundBps(liveGrowth))
	}
	if realized7dBps == nil && liveBps == nil {
		return nil
	}

	result := &forecastRealizedResult{
		liveBps:               liveBps,
		realized7dBps:         realized7dBps,
		loyalSeries:           forecastBuildLoyalSeries(boundedInput, timeline),
		mainUsdcReserveSeries: forecastBuildReserveSeries(bounded[KaminoMainMarketUSDCReserve], input.nowMS),
		measuredAtMS:          measurementEndMS,
		source:                "live",
	}
	if realized7dBps != nil {
		result.headlineBps = *realized7dBps
		result.source = "realized_7d"
	} else {
		result.headlineBps = *liveBps
	}
	return result
}

func forecastSeriesHours(nowMS int64) []int64 {
	var hours []int64
	lastHour := floorDiv(nowMS, forecastHourMS) * forecastHourMS
	for hour := lastHour - forecastSeriesWindowMS + forecastHourMS; hour <= lastHour; hour += forecastHourMS {
		hours = append(hours, hour)
	}
	return hours
}

// forecastBuildLoyalSeries ports buildLoyalSeries
// (earn-realized-apy.shared.ts:425-446).
func forecastBuildLoyalSeries(input forecastRealizedInput, timeline []forecastAllocationPoint) []forecastApySample {
	var samples []forecastApySample
	for _, hour := range forecastSeriesHours(input.nowMS) {
		growth, ok := forecastPortfolioAnnualizedGrowth(input, timeline, hour, forecastRealizedWindowMS, forecastRealizedWindowMS)
		if ok {
			samples = append(samples, forecastApySample{apyBps: forecastClampAndRoundBps(growth), observedAt: forecastISO(hour)})
		}
	}
	return samples
}

// forecastBuildReserveSeries ports buildReserveSeries
// (earn-realized-apy.shared.ts:448-468).
func forecastBuildReserveSeries(points []forecastSharePricePoint, nowMS int64) []forecastApySample {
	var samples []forecastApySample
	for _, hour := range forecastSeriesHours(nowMS) {
		growth, ok := forecastUnclampedAnnualizedBps(points, hour, forecastRealizedWindowMS, forecastRealizedWindowMS)
		if ok {
			samples = append(samples, forecastApySample{apyBps: forecastClampAndRoundBps(growth), observedAt: forecastISO(hour)})
		}
	}
	return samples
}

// forecastISO renders JavaScript Date.prototype.toISOString for a millisecond
// timestamp; isoMilliseconds renders the same shape for a time.Time.
func forecastISO(ms int64) string {
	return isoMilliseconds(time.UnixMilli(ms))
}

type forecastAllocationPoint struct {
	atMS       int64
	incomplete bool
	weights    forecastWeights
}

// forecastAllocationTimeline ports allocationTimeline (earn-realized-apy.shared.ts:131-210).
// Snapshot identity (TS `===`) becomes pointer identity; vault ids become indexes.
func forecastAllocationTimeline(allocations forecastAllocationHistory) []forecastAllocationPoint {
	const (
		changeStart uint8 = iota
		changeSnapshot
		changeExpiry
	)
	type change struct {
		atMS  int64
		kind  uint8
		vault int
		snap  *forecastSnapshot
	}
	var changes []change
	for vault, firstSeenMS := range allocations.vaultsFirstSeenMS {
		changes = append(changes, change{atMS: firstSeenMS, kind: changeStart, vault: vault})
	}
	for i := range allocations.snapshots {
		snap := &allocations.snapshots[i]
		changes = append(changes, change{atMS: snap.observedAtMS, kind: changeSnapshot, vault: snap.vault, snap: snap})
		if snap.idleAmountRaw > 0 || snap.weights.anyPositive() {
			changes = append(changes, change{atMS: snap.observedAtMS + forecastMaxAllocGapMS, kind: changeExpiry, vault: snap.vault, snap: snap})
		}
	}
	slices.SortStableFunc(changes, func(a, b change) int {
		if a.atMS != b.atMS {
			return cmp.Compare(a.atMS, b.atMS)
		}
		return cmp.Compare(a.kind, b.kind)
	})

	current := map[int]*forecastSnapshot{}
	invalidVaults := map[int]bool{}
	weights := newForecastWeights()
	var timeline []forecastAllocationPoint
	addWeights := func(snap *forecastSnapshot, direction float64) {
		for _, reserve := range snap.weights.order {
			weights.add(reserve, direction*snap.weights.byName[reserve])
		}
		weights.add(forecastIdleReserve, direction*snap.idleAmountRaw)
	}
	for i := range changes {
		change := &changes[i]
		switch change.kind {
		case changeStart:
			if _, ok := current[change.vault]; !ok {
				invalidVaults[change.vault] = true
			}
		case changeSnapshot:
			if previous, ok := current[change.vault]; ok {
				addWeights(previous, -1)
			}
			current[change.vault] = change.snap
			addWeights(change.snap, 1)
			if change.snap.unsupported {
				invalidVaults[change.vault] = true
			} else {
				delete(invalidVaults, change.vault)
			}
		case changeExpiry:
			if current[change.vault] == change.snap {
				invalidVaults[change.vault] = true
			}
		}
		point := forecastAllocationPoint{atMS: change.atMS, incomplete: len(invalidVaults) > 0, weights: weights.clone()}
		if n := len(timeline); n > 0 && timeline[n-1].atMS == point.atMS {
			timeline[n-1] = point
		} else {
			timeline = append(timeline, point)
		}
	}
	return timeline
}
