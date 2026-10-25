package webapp

import (
	"sort"
	"time"
)

type earningsHoldingEvent struct {
	amountRaw                int64
	confirmedAt              time.Time
	confirmedSlot            int64
	eventType                string // user_yield_holding_event_type value
	id                       int64
	reserve                  string
	market                   *string
	principalDeltaRaw        *int64
	rebalanceAmountRaw       *int64
	withdrawnAmountRaw       *int64
	liquidityMint            string
	sourceReserve            *string
	sourceMarket             *string
	sourceLiquidityMint      *string
	destinationReserve       *string
	destinationMarket        *string
	destinationLiquidityMint *string
	principalAmountRaw       int64
	positionID               int64
	signature                string
	eventKind                string // "deposit" | "withdrawal" | "rebalance" | "reconciliation"
}

func comparePathEvents(left, right *earningsHoldingEvent) int {
	if d := left.confirmedAt.UnixMilli() - right.confirmedAt.UnixMilli(); d != 0 {
		return int(sign64(d))
	}
	if left.confirmedSlot != right.confirmedSlot {
		if left.confirmedSlot < right.confirmedSlot {
			return -1
		}
		return 1
	}
	if left.id != right.id {
		if left.id < right.id {
			return -1
		}
		return 1
	}
	order := map[string]int{"deposit": 0, "rebalance": 1, "reconciliation": 2, "withdrawal": 3}
	return order[left.eventKind] - order[right.eventKind]
}

func sign64(v int64) int64 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// findHoldingAt ports read-service:212 over chronologically ordered events.
func findHoldingAt(events []earningsHoldingEvent, at time.Time) *earningsHoldingEvent {
	atMs := at.UnixMilli()
	var current *earningsHoldingEvent
	for i := range events {
		if events[i].confirmedAt.UnixMilli() > atMs {
			break
		}
		current = &events[i]
	}
	return current
}

func sortHoldingEventsAscending(events []earningsHoldingEvent) []earningsHoldingEvent {
	ordered := make([]earningsHoldingEvent, len(events))
	copy(ordered, events)
	sort.SliceStable(ordered, func(i, j int) bool {
		return comparePathEvents(&ordered[i], &ordered[j]) < 0
	})
	return ordered
}

// buildHoldingBackedPortfolioSnapshots ports read-service:227: replay holding
// events into per-position exposures until the first complete snapshot takes
// over. Exposure order inside a snapshot follows first insertion, like a JS Map.
// buildHoldingBackedPortfolioSnapshots ports read-service:227: complete
// vault-product snapshots, plus a holding-event replay for everything before
// the first complete one.
func buildHoldingBackedPortfolioSnapshots(completeSnapshots []earningsPortfolioSnapshot, holdingEvents []earningsHoldingEvent) []earningsPortfolioSnapshot {
	sortedComplete := sortedPortfolioSnapshots(completeSnapshots)
	var firstCompleteAtMs int64 = 1 << 62
	if len(sortedComplete) > 0 {
		firstCompleteAtMs = sortedComplete[0].observedAt.UnixMilli()
	}

	var positionOrder []int64
	exposuresByPosition := map[int64]earningsPortfolioExposure{}
	holdingBacked := []earningsPortfolioSnapshot{}

	for _, event := range sortHoldingEventsAscending(holdingEvents) {
		if event.confirmedAt.UnixMilli() >= firstCompleteAtMs {
			break
		}
		if event.amountRaw > 0 {
			reserve := event.reserve
			if _, exists := exposuresByPosition[event.positionID]; !exists {
				positionOrder = append(positionOrder, event.positionID)
			}
			exposuresByPosition[event.positionID] = earningsPortfolioExposure{
				amountRaw:     event.amountRaw,
				kind:          "kamino",
				liquidityMint: event.liquidityMint,
				reserve:       earningsString(reserve),
				sourceID:      "position:" + formatInt64(event.positionID) + ":reserve:" + reserve,
			}
		} else {
			delete(exposuresByPosition, event.positionID)
			for i, id := range positionOrder {
				if id == event.positionID {
					positionOrder = append(positionOrder[:i], positionOrder[i+1:]...)
					break
				}
			}
		}
		exposures := make([]earningsPortfolioExposure, 0, len(positionOrder))
		for _, id := range positionOrder {
			exposures = append(exposures, exposuresByPosition[id])
		}
		holdingBacked = append(holdingBacked, earningsPortfolioSnapshot{
			exposures:    exposures,
			observedAt:   event.confirmedAt,
			observedSlot: event.confirmedSlot,
		})
	}

	return append(holdingBacked, sortedComplete...)
}

func sortedPortfolioSnapshots(snapshots []earningsPortfolioSnapshot) []earningsPortfolioSnapshot {
	ordered := make([]earningsPortfolioSnapshot, len(snapshots))
	copy(ordered, snapshots)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].observedAt.UnixMilli() < ordered[j].observedAt.UnixMilli()
	})
	return ordered
}

func normalizeEarningsPortfolioSnapshots(snapshots []earningsPortfolioSnapshot, start, end time.Time) []earningsPortfolioSnapshot {
	sorted := sortedPortfolioSnapshots(snapshots)
	normalized := []earningsPortfolioSnapshot{}
	var seed *earningsPortfolioSnapshot
	for i := range sorted {
		if sorted[i].observedAt.After(end) {
			break
		}
		if !sorted[i].observedAt.After(start) {
			seed = &sorted[i]
		} else {
			normalized = append(normalized, sorted[i])
		}
	}
	if seed != nil && !start.After(end) {
		seedCopy := *seed
		seedCopy.observedAt = start
		normalized = append([]earningsPortfolioSnapshot{seedCopy}, normalized...)
	}
	return normalized
}

func buildCanonicalEarningsPath(holdingEvents []earningsHoldingEvent, ledgerEvents []earningsEvent, position earningsPositionSeed) ([]earningsPathEvent, error) {
	holdings := sortHoldingEventsAscending(holdingEvents)

	ledger := make([]earningsEvent, len(ledgerEvents))
	copy(ledger, ledgerEvents)
	sort.SliceStable(ledger, func(i, j int) bool {
		return ledger[i].confirmedAt.UnixMilli() < ledger[j].confirmedAt.UnixMilli()
	})

	var principalAmountRaw int64
	reserve := position.initialReserve
	var market *string
	if position.initialMarket != nil {
		m := *position.initialMarket
		market = &m
	}
	liquidityMint := position.initialLiquidityMint

	principalEvents := make([]earningsPathEvent, 0, len(ledger))
	for _, event := range ledger {
		var matchingHolding *earningsHoldingEvent
		for i := range holdings {
			if holdings[i].eventKind == event.eventType &&
				abs64(holdings[i].confirmedAt.UnixMilli()-event.confirmedAt.UnixMilli()) < 1000 {
				matchingHolding = &holdings[i]
				break
			}
		}
		holding := matchingHolding
		if holding == nil {
			holding = findHoldingAt(holdings, event.confirmedAt)
		}
		if holding != nil {
			reserve = holding.reserve
			market = cloneStr(holding.market)
			liquidityMint = holding.liquidityMint
		}

		if event.eventType == "deposit" {
			principalAmountRaw += event.amountRaw
		} else {
			var recordedPrincipalDeltaRaw *int64
			if matchingHolding != nil {
				recordedPrincipalDeltaRaw = matchingHolding.principalDeltaRaw
			}
			var principalReductionRaw int64
			switch {
			case recordedPrincipalDeltaRaw != nil && *recordedPrincipalDeltaRaw == 0:
				principalReductionRaw = 0
			case matchingHolding != nil && matchingHolding.eventType == "withdrawal_full":
				principalReductionRaw = principalAmountRaw
			case recordedPrincipalDeltaRaw != nil && *recordedPrincipalDeltaRaw < 0:
				principalReductionRaw = -*recordedPrincipalDeltaRaw
			default:
				principalReductionRaw = event.amountRaw
			}
			principalAmountRaw = clampedSubtract(principalAmountRaw, principalReductionRaw)
		}

		principalEvents = append(principalEvents, earningsPathEvent{
			amountRaw:          principalAmountRaw,
			confirmedAt:        event.confirmedAt,
			liquidityMint:      liquidityMint,
			market:             cloneStr(market),
			principalAmountRaw: principalAmountRaw,
			reserve:            reserve,
			eventType:          event.eventType,
		})
	}

	transitions := []earningsPathEvent{}
	for i := range holdings {
		event := holdings[i]
		if event.eventKind != "rebalance" && event.eventKind != "reconciliation" {
			continue
		}
		var principalAtEvent int64
		for j := range principalEvents {
			if principalEvents[j].confirmedAt.UnixMilli() > event.confirmedAt.UnixMilli() {
				break
			}
			principalAtEvent = principalEvents[j].principalAmountRaw
		}
		reserveAt := event.reserve
		transitions = append(transitions, earningsPathEvent{
			amountRaw:          event.amountRaw,
			confirmedAt:        event.confirmedAt,
			liquidityMint:      event.liquidityMint,
			market:             cloneStr(event.market),
			principalAmountRaw: principalAtEvent,
			reserve:            reserveAt,
			eventType:          event.eventKind,
		})
	}

	combined := append(append([]earningsPathEvent{}, principalEvents...), transitions...)
	sort.SliceStable(combined, func(i, j int) bool {
		return comparePathOnPath(&combined[i], &combined[j]) < 0
	})
	material := []earningsPathEvent{}
	for _, event := range combined {
		routine := false
		if len(material) > 0 && event.eventType == "reconciliation" {
			previous := material[len(material)-1]
			routine = previous.reserve == event.reserve &&
				strEqPtr(previous.market, event.market) &&
				previous.liquidityMint == event.liquidityMint &&
				previous.principalAmountRaw == event.principalAmountRaw
		}
		if !routine {
			material = append(material, event)
		}
	}
	return material, nil
}

func comparePathOnPath(left, right *earningsPathEvent) int {
	if d := left.confirmedAt.UnixMilli() - right.confirmedAt.UnixMilli(); d != 0 {
		return int(sign64(d))
	}
	order := map[string]int{"deposit": 0, "rebalance": 1, "reconciliation": 2, "withdrawal": 3}
	return order[left.eventType] - order[right.eventType]
}

type earningsPositionSeed struct {
	initialReserve       string
	initialMarket        *string
	initialLiquidityMint string
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func cloneStr(v *string) *string {
	if v == nil {
		return nil
	}
	s := *v
	return &s
}

func strEqPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// kaminoReservesOf returns the deduplicated reserves carrying value in one
// snapshot, in exposure order (JS iterates the Set in insertion order).
func kaminoReservesOf(snapshot earningsPortfolioSnapshot) []string {
	var reserves []string
	seen := map[string]bool{}
	for _, exposure := range snapshot.exposures {
		if exposure.kind == "kamino" && exposure.amountRaw > 0 && exposure.reserve != nil && !seen[*exposure.reserve] {
			seen[*exposure.reserve] = true
			reserves = append(reserves, *exposure.reserve)
		}
	}
	return reserves
}

func getPortfolioEarningsCoverage(apySamples []reserveApySample, now time.Time, snapshots []earningsPortfolioSnapshot) EarningsCoverage {
	required := map[string]int64{}
	var requiredOrder []string
	for _, snapshot := range snapshots {
		for _, exposure := range snapshot.exposures {
			if exposure.kind != "kamino" || exposure.amountRaw <= 0 || exposure.reserve == nil {
				continue
			}
			reserve := *exposure.reserve
			if earliest, ok := required[reserve]; !ok {
				required[reserve] = snapshot.observedAt.UnixMilli()
				requiredOrder = append(requiredOrder, reserve)
			} else if snapshot.observedAt.UnixMilli() < earliest {
				required[reserve] = snapshot.observedAt.UnixMilli()
			}
		}
	}

	var missingReserves []string
	for _, reserve := range requiredOrder {
		requiredAt := required[reserve]
		covered := false
		for _, sample := range apySamples {
			if sample.reserve != nil && *sample.reserve == reserve && sample.observedAt.UnixMilli() <= requiredAt {
				covered = true
				break
			}
		}
		if !covered {
			missingReserves = append(missingReserves, reserve)
		}
	}
	sort.Strings(missingReserves)

	gappedReserves := map[string]bool{}
	var maxSampleGapMs *int64
	for index := range snapshots {
		snapshot := snapshots[index]
		intervalEnd := now
		if index+1 < len(snapshots) {
			intervalEnd = snapshots[index+1].observedAt
		}
		for _, reserve := range kaminoReservesOf(snapshot) {
			var samples []reserveApySample
			for _, sample := range apySamples {
				if sample.reserve != nil && *sample.reserve == reserve && !sample.observedAt.After(intervalEnd) {
					samples = append(samples, sample)
				}
			}
			sort.SliceStable(samples, func(i, j int) bool {
				return samples[i].observedAt.UnixMilli() < samples[j].observedAt.UnixMilli()
			})
			var seed *reserveApySample
			for i := range samples {
				if !samples[i].observedAt.After(snapshot.observedAt) {
					seed = &samples[i]
				}
			}
			if seed == nil {
				gappedReserves[reserve] = true
				continue
			}
			cursor := snapshot.observedAt.UnixMilli()
			for _, sample := range samples {
				sampleMs := sample.observedAt.UnixMilli()
				if sampleMs <= cursor {
					continue
				}
				gap := sampleMs - cursor
				maxSampleGapMs = maxMsPtr(maxSampleGapMs, gap)
				if gap > earningsMaxSampleGapMs {
					gappedReserves[reserve] = true
				}
				cursor = sampleMs
			}
			trailingGap := intervalEnd.UnixMilli() - cursor
			maxSampleGapMs = maxMsPtr(maxSampleGapMs, trailingGap)
			if trailingGap > earningsMaxSampleGapMs {
				gappedReserves[reserve] = true
			}
		}
	}

	var currentReserves []string
	seenCurrent := map[string]bool{}
	if len(snapshots) > 0 {
		for _, reserve := range kaminoReservesOf(snapshots[len(snapshots)-1]) {
			if !seenCurrent[reserve] {
				seenCurrent[reserve] = true
				currentReserves = append(currentReserves, reserve)
			}
		}
	}
	// JS: Math.max(...ages) with a missing sample giving Infinity, and
	// JSON.stringify(Infinity) -> null; no current reserves -> null.
	var currentReserveSampleAgeMs *int64
	if len(currentReserves) > 0 {
		allAges := true
		maxCurrentAge := 0.0
		for _, reserve := range currentReserves {
			var latest *reserveApySample
			for i := range apySamples {
				if apySamples[i].reserve != nil && *apySamples[i].reserve == reserve {
					latest = &apySamples[i]
				}
			}
			if latest == nil {
				allAges = false
				break
			}
			age := float64(now.UnixMilli() - latest.observedAt.UnixMilli())
			if age < 0 {
				age = 0
			}
			if age > maxCurrentAge {
				maxCurrentAge = age
			}
		}
		if allAges {
			ageMs := int64(maxCurrentAge)
			currentReserveSampleAgeMs = &ageMs
		}
	}

	var staleReserves []string
	for _, reserve := range currentReserves {
		var latest *reserveApySample
		for i := range apySamples {
			if apySamples[i].reserve != nil && *apySamples[i].reserve == reserve {
				latest = &apySamples[i]
			}
		}
		stale := latest == nil
		if !stale {
			age := now.UnixMilli() - latest.observedAt.UnixMilli()
			if age < 0 {
				age = 0
			}
			stale = age > earningsMaxSampleGapMs
		}
		if stale {
			staleReserves = append(staleReserves, reserve)
		}
	}
	sort.Strings(staleReserves)

	sampledReserveCount := map[string]bool{}
	for _, sample := range apySamples {
		if sample.reserve != nil {
			if _, ok := required[*sample.reserve]; ok {
				sampledReserveCount[*sample.reserve] = true
			}
		}
	}

	var gapped []string
	for reserve := range gappedReserves {
		gapped = append(gapped, reserve)
	}
	sort.Strings(gapped)

	return EarningsCoverage{
		CurrentReserveSampleAgeMs: currentReserveSampleAgeMs,
		EventCount:                len(snapshots),
		GappedReserves:            ensureStrings(gapped),
		MaxSampleGapMs:            maxSampleGapMs,
		MissingReserves:           ensureStrings(missingReserves),
		ReserveCount:              len(required),
		SampleCount:               len(apySamples),
		SampledReserveCount:       len(sampledReserveCount),
		StaleReserves:             ensureStrings(staleReserves),
	}
}

func getEarningsCoverage(apySamples []reserveApySample, now time.Time, pathEvents []earningsPathEvent) EarningsCoverage {
	required := map[string]int64{}
	var requiredOrder []string
	for _, event := range pathEvents {
		if event.principalAmountRaw <= 0 || event.reserve == "" {
			continue
		}
		reserve := event.reserve
		if current, ok := required[reserve]; !ok {
			required[reserve] = event.confirmedAt.UnixMilli()
			requiredOrder = append(requiredOrder, reserve)
		} else if event.confirmedAt.UnixMilli() < current {
			required[reserve] = event.confirmedAt.UnixMilli()
		}
	}

	sampledReserves := map[string]bool{}
	for _, sample := range apySamples {
		if sample.reserve != nil {
			if _, ok := required[*sample.reserve]; ok {
				sampledReserves[*sample.reserve] = true
			}
		}
	}

	var missingReserves []string
	for _, reserve := range requiredOrder {
		requiredAt := required[reserve]
		covered := false
		for _, sample := range apySamples {
			if sample.reserve != nil && *sample.reserve == reserve && sample.observedAt.UnixMilli() <= requiredAt {
				covered = true
				break
			}
		}
		if !covered {
			missingReserves = append(missingReserves, reserve)
		}
	}
	sort.Strings(missingReserves)

	gappedReserves := map[string]bool{}
	var maxSampleGapMs *int64
	for index := range pathEvents {
		event := pathEvents[index]
		if event.principalAmountRaw <= 0 || event.reserve == "" {
			continue
		}
		intervalEnd := now
		if index+1 < len(pathEvents) {
			intervalEnd = pathEvents[index+1].confirmedAt
		}
		var samples []reserveApySample
		for _, sample := range apySamples {
			if sample.reserve != nil && *sample.reserve == event.reserve && sample.observedAt.UnixMilli() <= intervalEnd.UnixMilli() {
				samples = append(samples, sample)
			}
		}
		sort.SliceStable(samples, func(i, j int) bool {
			return samples[i].observedAt.UnixMilli() < samples[j].observedAt.UnixMilli()
		})
		var seed *reserveApySample
		for i := range samples {
			if samples[i].observedAt.UnixMilli() <= event.confirmedAt.UnixMilli() {
				seed = &samples[i]
			}
		}
		if seed == nil {
			gappedReserves[event.reserve] = true
			continue
		}
		cursorMs := event.confirmedAt.UnixMilli()
		for _, sample := range samples {
			sampleMs := sample.observedAt.UnixMilli()
			if sampleMs <= cursorMs {
				continue
			}
			gapMs := sampleMs - cursorMs
			maxSampleGapMs = maxMsPtr(maxSampleGapMs, gapMs)
			if gapMs > earningsMaxSampleGapMs {
				gappedReserves[event.reserve] = true
			}
			cursorMs = sampleMs
		}
		trailingGapMs := intervalEnd.UnixMilli() - cursorMs
		maxSampleGapMs = maxMsPtr(maxSampleGapMs, trailingGapMs)
		if trailingGapMs > earningsMaxSampleGapMs {
			gappedReserves[event.reserve] = true
		}
	}

	var currentReserve string
	for i := len(pathEvents) - 1; i >= 0; i-- {
		if pathEvents[i].principalAmountRaw > 0 {
			currentReserve = pathEvents[i].reserve
			break
		}
	}
	var currentReserveSampleAgeMs *int64
	var staleReserves []string
	if currentReserve != "" {
		var latest *reserveApySample
		for i := range apySamples {
			if apySamples[i].reserve != nil && *apySamples[i].reserve == currentReserve {
				latest = &apySamples[i]
			}
		}
		if latest != nil {
			age := now.UnixMilli() - latest.observedAt.UnixMilli()
			if age < 0 {
				age = 0
			}
			currentReserveSampleAgeMs = &age
			if age > earningsMaxSampleGapMs {
				staleReserves = []string{currentReserve}
			}
		}
	}

	var gapped []string
	for reserve := range gappedReserves {
		gapped = append(gapped, reserve)
	}
	sort.Strings(gapped)

	return EarningsCoverage{
		CurrentReserveSampleAgeMs: currentReserveSampleAgeMs,
		EventCount:                len(pathEvents),
		GappedReserves:            ensureStrings(gapped),
		MaxSampleGapMs:            maxSampleGapMs,
		MissingReserves:           ensureStrings(missingReserves),
		ReserveCount:              len(required),
		SampleCount:               len(apySamples),
		SampledReserveCount:       len(sampledReserves),
		StaleReserves:             ensureStrings(staleReserves),
	}
}

func maxMsPtr(current *int64, candidate int64) *int64 {
	if current != nil && *current >= candidate {
		return current
	}
	return &candidate
}

func ensureStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
