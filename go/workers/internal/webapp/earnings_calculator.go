package webapp

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// Port of earnings-calculator.server.ts. The numbers here are micro-USDC
// integers carried as int64 (JS bigint); USD values are float64 because the
// served JSON carries them as JS numbers and every sum must round-trip the
// same IEEE-754 arithmetic as the TS.

var earningsRangeIDs = [...]string{"7D", "30D", "1Y", "ALL"}

const (
	earningsStablecoinMicros = 1_000_000.0
	earningsSecondsPerYear   = 365.0 * 24 * 60 * 60
	earningsMaxSampleGapMs   = int64(36 * 60 * 60 * 1000)
)

// Earnings event, portfolio snapshot and path-event values, mirroring
// YieldPositionEvent / YieldPortfolioSnapshot / YieldPositionPathEvent.
// Optional JS fields are pointers so the canonical fingerprint JSON can skip
// them exactly like JSON.stringify skips undefined.

type earningsEvent struct {
	amountRaw           int64
	confirmedAt         time.Time
	confirmedSlot       int64
	liquidityMint       string
	holdingEventID      *int64
	positionID          *int64
	initializesPosition *bool
	principalDeltaRaw   *int64
	settlesPosition     *bool
	eventType           earningsEventType
}

type earningsPortfolioExposure struct {
	amountRaw     int64
	kind          earningsExposureKind
	liquidityMint string
	reserve       *string
	sourceID      string
}

type earningsPortfolioSnapshot struct {
	exposures    []earningsPortfolioExposure
	observedAt   time.Time
	observedSlot int64
}

type earningsPathEvent struct {
	amountRaw          int64
	confirmedAt        time.Time
	liquidityMint      string
	market             *string
	principalAmountRaw int64
	reserve            string
	eventType          earningsEventType
}

type reserveApySample struct {
	observedAt time.Time
	reserve    *string
	supplyApy  float64
}

type earningsHistoryError struct{ detail string }

func (e *earningsHistoryError) Error() string { return e.detail }

func earningsInt64(v int64) *int64    { return &v }
func earningsBool(v bool) *bool       { return &v }
func earningsString(v string) *string { return &v }
func firstEarningsInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func sortEarningsEvents(events []earningsEvent) ([]earningsEvent, error) {
	ordered := make([]earningsEvent, len(events))
	copy(ordered, events)
	var ambiguous error
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		leftMs, rightMs := left.confirmedAt.UnixMilli(), right.confirmedAt.UnixMilli()
		if leftMs != rightMs {
			return leftMs < rightMs
		}
		if left.confirmedSlot != right.confirmedSlot {
			return left.confirmedSlot < right.confirmedSlot
		}
		if left.holdingEventID != nil && right.holdingEventID != nil && *left.holdingEventID != *right.holdingEventID {
			return *left.holdingEventID < *right.holdingEventID
		}
		sameKind := left.eventType == right.eventType &&
			left.initializesPosition == nil && right.initializesPosition == nil &&
			left.settlesPosition == nil && right.settlesPosition == nil
		if !sameKind && ambiguous == nil {
			ambiguous = &earningsHistoryError{detail: "principal_history_order_ambiguous"}
		}
		return false
	})
	if ambiguous != nil {
		return nil, ambiguous
	}
	return ordered, nil
}

func lastIndexAtOrBefore(times []int64, atMs int64) int {
	low, high, found := 0, len(times)-1, -1
	for low <= high {
		mid := int(uint(low+high) >> 1)
		if times[mid] <= atMs {
			found = mid
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return found
}

func clampedSubtract(current, amount int64) int64 {
	if current > amount {
		return current - amount
	}
	return 0
}

type principalClamp string

const (
	clampTotal    principalClamp = "total"
	clampPerMint  principalClamp = "per-mint"
	clampPosition principalClamp = "position"
)

var legacyPrincipalClamps = []principalClamp{clampTotal, clampPerMint}

type principalIndex struct {
	times  []int64
	totals []int64
}

func buildPrincipalIndex(sorted []earningsEvent, clamp principalClamp) (*principalIndex, error) {
	index := &principalIndex{times: make([]int64, 0, len(sorted)), totals: make([]int64, 0, len(sorted))}
	byMint := map[string]int64{}
	var total int64
	for _, event := range sorted {
		if event.principalDeltaRaw != nil &&
			((event.eventType == "deposit" && *event.principalDeltaRaw < 0) ||
				(event.eventType != "deposit" && *event.principalDeltaRaw > 0)) {
			return nil, &earningsHistoryError{detail: "principal_history_delta_invalid"}
		}
		amount := event.amountRaw
		if event.principalDeltaRaw != nil {
			if event.eventType == "deposit" {
				amount = *event.principalDeltaRaw
			} else {
				amount = -*event.principalDeltaRaw
			}
		}
		if clamp == clampTotal {
			if event.eventType == "deposit" {
				total += amount
			} else {
				total = clampedSubtract(total, amount)
			}
		} else {
			mint := event.liquidityMint
			if clamp == clampPosition {
				if event.positionID == nil || event.initializesPosition == nil {
					return nil, &earningsHistoryError{detail: "principal_history_position_incomplete"}
				}
				mint = formatInt64(*event.positionID)
				if _, seen := byMint[mint]; !seen && (event.eventType != "deposit" || !*event.initializesPosition) {
					return nil, &earningsHistoryError{detail: "principal_history_position_incomplete"}
				}
			}
			current := byMint[mint]
			var next int64
			switch {
			case event.settlesPosition != nil && *event.settlesPosition:
				next = 0
			case event.eventType == "deposit":
				base := current
				if clamp == clampPosition && event.initializesPosition != nil && *event.initializesPosition {
					base = 0
				}
				next = base + amount
			default:
				next = clampedSubtract(current, amount)
			}
			byMint[mint] = next
			total += next - current
		}
		index.totals = append(index.totals, total)
		index.times = append(index.times, event.confirmedAt.UnixMilli())
	}
	return index, nil
}

func principalAt(sorted []earningsEvent, indexes map[principalClamp]*principalIndex, at time.Time, clamp principalClamp) (int64, error) {
	index := indexes[clamp]
	if index == nil {
		built, err := buildPrincipalIndex(sorted, clamp)
		if err != nil {
			return 0, err
		}
		indexes[clamp] = built
		index = built
	}
	if found := lastIndexAtOrBefore(index.times, at.UnixMilli()); found != -1 {
		return index.totals[found], nil
	}
	return 0, nil
}

func rawToUsd(raw int64) float64 { return float64(raw) / earningsStablecoinMicros }

func formatInt64(v int64) string { return strconv.FormatInt(v, 10) }

func deriveApyBps(avgPrincipalUsd float64, bucketSeconds, earnedUsd float64) *int64 {
	if avgPrincipalUsd <= 0 || bucketSeconds <= 0 {
		return nil
	}
	bps := int64(math.Round((earnedUsd / avgPrincipalUsd) * (earningsSecondsPerYear / bucketSeconds) * 10_000))
	return &bps
}

type apySeries struct {
	apys      []float64
	positions []int
	times     []int64
}

type apyIndex struct {
	samples   []reserveApySample
	all       apySeries
	byReserve map[string]*apySeries
	unscoped  apySeries
}

func buildApyIndex(unsorted []reserveApySample) *apyIndex {
	samples := make([]reserveApySample, len(unsorted))
	copy(samples, unsorted)
	sort.SliceStable(samples, func(i, j int) bool {
		return samples[i].observedAt.UnixMilli() < samples[j].observedAt.UnixMilli()
	})
	index := &apyIndex{samples: samples, byReserve: map[string]*apySeries{}}
	for position, sample := range samples {
		t := sample.observedAt.UnixMilli()
		push := func(series *apySeries) {
			series.apys = append(series.apys, sample.supplyApy)
			series.positions = append(series.positions, position)
			series.times = append(series.times, t)
		}
		push(&index.all)
		if sample.reserve == nil {
			push(&index.unscoped)
		} else {
			series := index.byReserve[*sample.reserve]
			if series == nil {
				series = &apySeries{}
				index.byReserve[*sample.reserve] = series
			}
			push(series)
		}
	}
	return index
}

// getApyAt returns the latest sample at or before `at`, reserve-scoped when
// the reserve has its own series and that series' newest match sits later in
// the sample list than the unscoped one.
func getApyAt(index *apyIndex, reserve *string, at time.Time) *float64 {
	atMs := at.UnixMilli()
	if reserve == nil || *reserve == "" {
		if found := lastIndexAtOrBefore(index.all.times, atMs); found != -1 {
			apy := index.all.apys[found]
			return &apy
		}
		return nil
	}
	scoped := index.byReserve[*reserve]
	scopedFound, scopedPosition := -1, -1
	if scoped != nil {
		scopedFound = lastIndexAtOrBefore(scoped.times, atMs)
		if scopedFound != -1 {
			scopedPosition = scoped.positions[scopedFound]
		}
	}
	unscopedFound := lastIndexAtOrBefore(index.unscoped.times, atMs)
	unscopedPosition := -1
	if unscopedFound != -1 {
		unscopedPosition = index.unscoped.positions[unscopedFound]
	}
	if scopedPosition == -1 && unscopedPosition == -1 {
		return nil
	}
	if scoped != nil && scopedPosition > unscopedPosition {
		apy := scoped.apys[scopedFound]
		return &apy
	}
	apy := index.unscoped.apys[unscopedFound]
	return &apy
}

type earningsWindow struct {
	avgPrincipalUsd    float64
	earnedUsd          float64
	principalAmountRaw int64
}

type portfolioSnapshotIndex struct {
	snapshots []earningsPortfolioSnapshot
	times     []int64
}

func newPortfolioSnapshotIndex(unsorted []earningsPortfolioSnapshot) *portfolioSnapshotIndex {
	snapshots := make([]earningsPortfolioSnapshot, len(unsorted))
	copy(snapshots, unsorted)
	sort.SliceStable(snapshots, func(i, j int) bool {
		return snapshots[i].observedAt.UnixMilli() < snapshots[j].observedAt.UnixMilli()
	})
	times := make([]int64, len(snapshots))
	for i, snapshot := range snapshots {
		times[i] = snapshot.observedAt.UnixMilli()
	}
	return &portfolioSnapshotIndex{snapshots: snapshots, times: times}
}

func (i *portfolioSnapshotIndex) at(t time.Time) *earningsPortfolioSnapshot {
	if found := lastIndexAtOrBefore(i.times, t.UnixMilli()); found != -1 {
		return &i.snapshots[found]
	}
	return nil
}

type pathEventIndex struct {
	events []earningsPathEvent
	times  []int64
}

func newPathEventIndex(unsorted []earningsPathEvent) *pathEventIndex {
	events := make([]earningsPathEvent, len(unsorted))
	copy(events, unsorted)
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].confirmedAt.UnixMilli() < events[j].confirmedAt.UnixMilli()
	})
	times := make([]int64, len(events))
	for i, event := range events {
		times[i] = event.confirmedAt.UnixMilli()
	}
	return &pathEventIndex{events: events, times: times}
}

func (i *pathEventIndex) at(t time.Time) *earningsPathEvent {
	if found := lastIndexAtOrBefore(i.times, t.UnixMilli()); found != -1 {
		return &i.events[found]
	}
	return nil
}

func sortedKeys(set map[int64]struct{}) []int64 {
	keys := make([]int64, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func calculateWindow(apy *apyIndex, pathIdx *pathEventIndex, samples []reserveApySample, startAt, endAt time.Time) earningsWindow {
	startMs, endMs := startAt.UnixMilli(), endAt.UnixMilli()
	endPrincipal := int64(0)
	if state := pathIdx.at(endAt); state != nil {
		endPrincipal = state.principalAmountRaw
	}
	if endMs <= startMs {
		return earningsWindow{principalAmountRaw: endPrincipal}
	}

	changeTimes := map[int64]struct{}{startMs: {}, endMs: {}}
	for _, event := range pathIdx.events {
		if t := event.confirmedAt.UnixMilli(); t > startMs && t < endMs {
			changeTimes[t] = struct{}{}
		}
	}
	for _, sample := range samples {
		if t := sample.observedAt.UnixMilli(); t > startMs && t < endMs {
			changeTimes[t] = struct{}{}
		}
	}
	sortedTimes := sortedKeys(changeTimes)

	var earnedUsd, principalSeconds float64
	for i := 0; i < len(sortedTimes)-1; i++ {
		segmentStart := msTimeMillis(sortedTimes[i])
		segmentSeconds := float64(sortedTimes[i+1]-sortedTimes[i]) / 1000
		state := pathIdx.at(segmentStart)
		var principalUsd float64
		var reserve *string
		if state != nil {
			principalUsd = rawToUsd(state.principalAmountRaw)
			reserve = earningsString(state.reserve)
		}
		principalSeconds += principalUsd * segmentSeconds
		if apy := getApyAt(apy, reserve, segmentStart); apy != nil && principalUsd > 0 && segmentSeconds > 0 {
			earnedUsd += (principalUsd * *apy * segmentSeconds) / earningsSecondsPerYear
		}
	}

	bucketSeconds := float64(endMs-startMs) / 1000
	avg := 0.0
	if bucketSeconds > 0 {
		avg = principalSeconds / bucketSeconds
	}
	return earningsWindow{avgPrincipalUsd: avg, earnedUsd: earnedUsd, principalAmountRaw: endPrincipal}
}

func portfolioApyAt(apy *apyIndex, at time.Time, snapshot *earningsPortfolioSnapshot) *float64 {
	if snapshot == nil {
		return nil
	}
	var weighted float64
	var totalRaw int64
	for _, exposure := range snapshot.exposures {
		if exposure.amountRaw <= 0 {
			continue
		}
		totalRaw += exposure.amountRaw
		if exposure.kind == "idle" {
			continue
		}
		apyAt := getApyAt(apy, exposure.reserve, at)
		if apyAt == nil {
			return nil
		}
		weighted += float64(exposure.amountRaw) * *apyAt
	}
	if totalRaw <= 0 {
		return nil
	}
	value := weighted / float64(totalRaw)
	return &value
}

// calculatePortfolioWindow: earnings accrue per snapshot exposure; the
// principal line still comes from the ledger.
func calculatePortfolioWindow(apy *apyIndex, events []earningsEvent, principalIndexes map[principalClamp]*principalIndex, clamp principalClamp, snapshotIdx *portfolioSnapshotIndex, samples []reserveApySample, startAt, endAt time.Time) (earningsWindow, error) {
	startMs, endMs := startAt.UnixMilli(), endAt.UnixMilli()
	if endMs <= startMs {
		principal, err := principalAt(events, principalIndexes, endAt, clamp)
		if err != nil {
			return earningsWindow{}, err
		}
		return earningsWindow{principalAmountRaw: principal}, nil
	}
	changeTimes := map[int64]struct{}{startMs: {}, endMs: {}}
	for _, event := range events {
		if t := event.confirmedAt.UnixMilli(); t > startMs && t < endMs {
			changeTimes[t] = struct{}{}
		}
	}
	for _, snapshot := range snapshotIdx.snapshots {
		if t := snapshot.observedAt.UnixMilli(); t > startMs && t < endMs {
			changeTimes[t] = struct{}{}
		}
	}
	for _, sample := range samples {
		if t := sample.observedAt.UnixMilli(); t > startMs && t < endMs {
			changeTimes[t] = struct{}{}
		}
	}
	sortedTimes := sortedKeys(changeTimes)

	var earnedUsd, principalSeconds float64
	for i := 0; i < len(sortedTimes)-1; i++ {
		segmentStart := msTimeMillis(sortedTimes[i])
		segmentSeconds := float64(sortedTimes[i+1]-sortedTimes[i]) / 1000
		principalRaw, err := principalAt(events, principalIndexes, segmentStart, clamp)
		if err != nil {
			return earningsWindow{}, err
		}
		principalSeconds += rawToUsd(principalRaw) * segmentSeconds
		snapshot := snapshotIdx.at(segmentStart)
		if snapshot == nil {
			continue
		}
		for _, exposure := range snapshot.exposures {
			if exposure.kind != "kamino" || exposure.amountRaw <= 0 || exposure.reserve == nil {
				continue
			}
			if apyAt := getApyAt(apy, exposure.reserve, segmentStart); apyAt != nil {
				earnedUsd += (rawToUsd(exposure.amountRaw) * *apyAt * segmentSeconds) / earningsSecondsPerYear
			}
		}
	}
	bucketSeconds := float64(endMs-startMs) / 1000
	principal, err := principalAt(events, principalIndexes, endAt, clamp)
	if err != nil {
		return earningsWindow{}, err
	}
	avg := 0.0
	if bucketSeconds > 0 {
		avg = principalSeconds / bucketSeconds
	}
	return earningsWindow{avgPrincipalUsd: avg, earnedUsd: earnedUsd, principalAmountRaw: principal}, nil
}

func legacyEventsToPathEvents(sorted []earningsEvent) []earningsPathEvent {
	var principal int64
	events := make([]earningsPathEvent, 0, len(sorted))
	for _, event := range sorted {
		if event.eventType == "deposit" {
			principal += event.amountRaw
		} else {
			principal -= event.amountRaw
		}
		if principal < 0 {
			principal = 0
		}
		events = append(events, earningsPathEvent{
			amountRaw:          principal,
			confirmedAt:        event.confirmedAt,
			principalAmountRaw: principal,
			eventType:          event.eventType,
		})
	}
	return events
}

type earningsCheckpointResponse struct {
	at       time.Time
	response EarningsRange
}

type calculateEarnEarningsArgs struct {
	apySamples         []reserveApySample
	events             []earningsEvent
	now                time.Time
	pathEvents         []earningsPathEvent
	portfolioSnapshots []earningsPortfolioSnapshot
	principalClamp     principalClamp
	rangeID            string
	timezone           string
	checkpoint         *earningsCheckpointResponse
}

func calculateEarnEarnings(args calculateEarnEarningsArgs) (EarningsRange, error) {
	clamp := args.principalClamp
	if clamp == "" {
		clamp = clampTotal
	}
	filtered := make([]earningsEvent, 0, len(args.events))
	for _, event := range args.events {
		if !event.confirmedAt.After(args.now) {
			filtered = append(filtered, event)
		}
	}
	sorted, err := sortEarningsEvents(filtered)
	if err != nil {
		return EarningsRange{}, err
	}

	pathEvents := args.pathEvents
	if pathEvents == nil {
		pathEvents = legacyEventsToPathEvents(sorted)
	}
	pathIdx := newPathEventIndex(pathEvents)
	pathSorted := pathIdx.events
	apy := buildApyIndex(args.apySamples)
	apySorted := apy.samples
	snapshotIdx := newPortfolioSnapshotIndex(args.portfolioSnapshots)
	snapshots := snapshotIdx.snapshots

	firstDepositAt := firstDepositOf(sorted, pathSorted)
	lastDepositAt := lastDepositOf(sorted, pathSorted)
	usePortfolio := len(snapshots) > 0
	buckets := createEarningsBuckets(firstDepositAt, args.now, args.rangeID, args.timezone)

	var resumeAtMs int64 = math.MinInt64
	previousBars := map[string]EarningsBar{}
	if args.checkpoint != nil {
		resumeAtMs = args.checkpoint.at.UnixMilli()
		for _, bar := range args.checkpoint.response.Bars {
			previousBars[bar.StartAt] = bar
		}
	}

	principalIndexes := map[principalClamp]*principalIndex{}

	bars := make([]EarningsBar, 0, len(buckets))
	for _, bucket := range buckets {
		previous, hasPrevious := previousBars[isoMS(bucket.startAt)]
		if bucket.endAt.UnixMilli() <= resumeAtMs && hasPrevious {
			bar := previous
			bar.IsCurrent = bucket.isCurrent
			bar.Label = bucket.label
			bars = append(bars, bar)
			continue
		}
		startMs := bucket.startAt.UnixMilli()
		if resumeAtMs > startMs {
			startMs = resumeAtMs
		}
		startAt := msTimeMillis(startMs)
		var result earningsWindow
		if usePortfolio {
			result, err = calculatePortfolioWindow(apy, sorted, principalIndexes, clamp, snapshotIdx, apySorted, startAt, bucket.endAt)
		} else {
			result = calculateWindow(apy, pathIdx, apySorted, startAt, bucket.endAt)
		}
		if err != nil {
			return EarningsRange{}, err
		}
		bucketSeconds := float64(bucket.endAt.UnixMilli()-bucket.startAt.UnixMilli()) / 1000
		if hasPrevious && bucket.startAt.UnixMilli() < resumeAtMs {
			previousSeconds := float64(resumeAtMs-bucket.startAt.UnixMilli()) / 1000
			tailSeconds := float64(bucket.endAt.UnixMilli()-startMs) / 1000
			if bucketSeconds > 0 {
				result.avgPrincipalUsd = (previous.AvgPrincipalUsd*previousSeconds + result.avgPrincipalUsd*tailSeconds) / bucketSeconds
			} else {
				result.avgPrincipalUsd = 0
			}
			result.earnedUsd += previous.EarnedUsd
		}
		bars = append(bars, EarningsBar{
			APYBps:             deriveApyBps(result.avgPrincipalUsd, bucketSeconds, result.earnedUsd),
			AvgPrincipalUsd:    result.avgPrincipalUsd,
			EarnedUsd:          result.earnedUsd,
			EndAt:              isoMS(bucket.endAt),
			IsCurrent:          bucket.isCurrent,
			Label:              bucket.label,
			PrincipalAmountRaw: formatInt64(result.principalAmountRaw),
			PrincipalUsd:       rawToUsd(result.principalAmountRaw),
			StartAt:            isoMS(bucket.startAt),
		})
	}

	lifetimeStart := args.now
	if firstDepositAt != nil {
		lifetimeStart = *firstDepositAt
	}
	lifetime, err := calculateEarningsRange(args, sorted, apy, apySorted, snapshotIdx, principalIndexes, pathIdx, usePortfolio, lifetimeStart, resumeAtMs)
	if err != nil {
		return EarningsRange{}, err
	}
	sinceLastDeposit, err := calculateEarningsRange(args, sorted, apy, apySorted, snapshotIdx, principalIndexes, pathIdx, usePortfolio, ptrOr(lastDepositAt, args.now), resumeAtMs)
	if err != nil {
		return EarningsRange{}, err
	}
	today, err := calculateEarningsRange(args, sorted, apy, apySorted, snapshotIdx, principalIndexes, pathIdx, usePortfolio, startOfLocalDay(args.now, args.timezone), resumeAtMs)
	if err != nil {
		return EarningsRange{}, err
	}
	if args.checkpoint != nil {
		lifetime.earnedUsd += args.checkpoint.response.LifetimeEarnedUsd
		if lastDepositAt != nil && lastDepositAt.UnixMilli() < resumeAtMs {
			sinceLastDeposit.earnedUsd += args.checkpoint.response.SinceLastDepositEarnedUsd
		}
		if startOfLocalDay(args.now, args.timezone).UnixMilli() < resumeAtMs {
			today.earnedUsd += args.checkpoint.response.TodayEarnedUsd
		}
	}

	var principalAmountRaw int64
	var currentApy *float64
	if usePortfolio {
		principalAmountRaw, err = principalAt(sorted, principalIndexes, args.now, clamp)
		if err != nil {
			return EarningsRange{}, err
		}
		currentApy = portfolioApyAt(apy, args.now, snapshotIdx.at(args.now))
	} else if state := pathIdx.at(args.now); state != nil {
		principalAmountRaw = state.principalAmountRaw
		currentApy = getApyAt(apy, earningsString(state.reserve), args.now)
	}

	var currentApyBps *int64
	if currentApy != nil {
		bps := int64(math.Round(*currentApy * 10_000))
		currentApyBps = &bps
	}
	var lastDepositAtStr *string
	if lastDepositAt != nil {
		lastDepositAtStr = strPtr(isoMS(*lastDepositAt))
	}
	var rangeEarnedUsd float64
	for _, bar := range bars {
		rangeEarnedUsd += bar.EarnedUsd
	}
	return EarningsRange{
		Bars:                      bars,
		CurrentApyBps:             currentApyBps,
		LastDepositAt:             lastDepositAtStr,
		LifetimeEarnedUsd:         lifetime.earnedUsd,
		PrincipalAmountRaw:        formatInt64(principalAmountRaw),
		PrincipalUsd:              rawToUsd(principalAmountRaw),
		RangeEarnedUsd:            rangeEarnedUsd,
		SinceLastDepositEarnedUsd: sinceLastDeposit.earnedUsd,
		TodayEarnedUsd:            today.earnedUsd,
	}, nil
}

// calculateEarningsRange is the inner `calculateRange` closure of
// calculateEarnEarnings with the checkpoint resume start applied.
func calculateEarningsRange(args calculateEarnEarningsArgs, sorted []earningsEvent, apy *apyIndex, apySorted []reserveApySample, snapshotIdx *portfolioSnapshotIndex, principalIndexes map[principalClamp]*principalIndex, pathIdx *pathEventIndex, usePortfolio bool, from time.Time, resumeAtMs int64) (earningsWindow, error) {
	startMs := from.UnixMilli()
	if resumeAtMs > startMs {
		startMs = resumeAtMs
	}
	startAt := msTimeMillis(startMs)
	if usePortfolio {
		return calculatePortfolioWindow(apy, sorted, principalIndexes, args.principalClamp, snapshotIdx, apySorted, startAt, args.now)
	}
	return calculateWindow(apy, pathIdx, apySorted, startAt, args.now), nil
}

func firstDepositOf(sorted []earningsEvent, pathSorted []earningsPathEvent) *time.Time {
	for i := range sorted {
		if sorted[i].eventType == "deposit" {
			return &sorted[i].confirmedAt
		}
	}
	for i := range pathSorted {
		if pathSorted[i].eventType == "deposit" {
			return &pathSorted[i].confirmedAt
		}
	}
	return nil
}

func lastDepositOf(sorted []earningsEvent, pathSorted []earningsPathEvent) *time.Time {
	for i := len(sorted) - 1; i >= 0; i-- {
		if sorted[i].eventType == "deposit" {
			return &sorted[i].confirmedAt
		}
	}
	for i := len(pathSorted) - 1; i >= 0; i-- {
		if pathSorted[i].eventType == "deposit" {
			return &pathSorted[i].confirmedAt
		}
	}
	return nil
}

func ptrOr(v *time.Time, fallback time.Time) time.Time {
	if v != nil {
		return *v
	}
	return fallback
}

func strPtr(v string) *string { return &v }
