package webapp

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type earningsReadInput struct {
	cluster       string
	settings      string
	timezone      string
	vaultIndex    int16
	walletAddress string
}

type earningsPosition struct {
	id                   int64
	initialReserve       string
	initialMarket        *string
	initialLiquidityMint string
	principalAmountRaw   int64
}

func (p earningsPosition) seed() earningsPositionSeed {
	return earningsPositionSeed{
		initialReserve:       p.initialReserve,
		initialMarket:        p.initialMarket,
		initialLiquidityMint: p.initialLiquidityMint,
	}
}

type earningsSnapshotRecord struct {
	generatedAt    time.Time
	payload        EarningsRangeSet
	sourceRevision *string
	checkpoint     *earningsCheckpointWire
}

type earningsUnavailableError struct {
	code       string // "earnings_unavailable" | "history_incomplete"
	message    string
	detailCode string
	coverage   *EarningsCoverage
}

func (e *earningsUnavailableError) Error() string { return e.message }

func unavailableErr(detailCode, message string) *earningsUnavailableError {
	return &earningsUnavailableError{code: "history_incomplete", message: message, detailCode: detailCode}
}

var errEarningsRefreshInProgress = errors.New("earnings refresh is in progress")

type earningsApyTimeoutError struct{}

func (e *earningsApyTimeoutError) Error() string { return "earnings_timescale_timeout" }

func normalizedErrorKind(err error) (errorCode, errorName, errorDetailCode string) {
	errorDetailCode = ""
	errorName = errorNameOf(err)
	var unavailable *earningsUnavailableError
	var timeout *earningsApyTimeoutError
	switch {
	case errors.As(err, &unavailable):
		return unavailable.code, errorName, unavailable.detailCode
	case errors.As(err, &timeout):
		return "timescale_timeout", errorName, ""
	}
	msg := err.Error()
	switch {
	case contains(msg, "earn_earnings_snapshots"):
		return "snapshot_store_unavailable", errorName, ""
	case contains(msg, "timescale"):
		return "timescale_unavailable", errorName, ""
	}
	return "dependency_unavailable", errorName, ""
}

func earningsErrorCause(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, errEarningsRefreshInProgress):
		return "refresh_in_progress"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &pgErr):
		return "sqlstate_" + pgErr.Code
	}
	return "unknown"
}

func errorNameOf(err error) string {
	var unavailable *earningsUnavailableError
	var timeout *earningsApyTimeoutError
	var history *earningsHistoryError
	switch {
	case errors.As(err, &unavailable):
		return "EarnEarningsUnavailableError"
	case errors.As(err, &timeout):
		return "EarnEarningsApyTimeoutError"
	case errors.As(err, &history):
		return "EarningsPrincipalHistoryError"
	}
	return "Error"
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

type earningsApyArgs struct {
	end                time.Time
	pathEvents         []earningsPathEvent
	portfolioSnapshots []earningsPortfolioSnapshot
	reserves           []string // nil = absent, as in the TS call site
	start              time.Time
}

type earningsSaveArgs struct {
	input          earningsReadInput
	payload        EarningsRangeSet
	principal      int64
	timezone       string
	sourceRevision *string
	checkpoint     *earningsCheckpointWire
}

type earningsReadDeps struct {
	apyTimeout             time.Duration
	loadApySamples         func(context.Context, earningsReadInput, earningsApyArgs) ([]reserveApySample, error)
	loadLedgerEvents       func(context.Context, earningsReadInput) ([]earningsEvent, error)
	loadHoldingEvents      func(context.Context, earningsReadInput) ([]earningsHoldingEvent, error)
	loadPositions          func(context.Context, earningsReadInput) ([]earningsPosition, error)
	loadPortfolioSnapshots func(context.Context, earningsReadInput, *time.Time) ([]earningsPortfolioSnapshot, error)
	loadSnapshot           func(context.Context, earningsReadInput, string) (*earningsSnapshotRecord, error)
	loadSourceRevision     func(context.Context, earningsReadInput) (string, error)
	loadHistoryFingerprint func(context.Context, earningsReadInput, time.Time) (string, error)
	saveSnapshot           func(context.Context, earningsSaveArgs) error
	now                    func() time.Time
}

func parseI64(v string) (int64, error) {
	return strconv.ParseInt(v, 10, 64)
}

func mustParseISO(v string) time.Time {
	t, err := time.Parse("2006-01-02T15:04:05.000Z07:00", v)
	if err != nil {
		return time.Time{}
	}
	return t
}

func earningsCheckpointTime(firstDepositAt, now time.Time) time.Time {
	const day = int64(86_400_000)
	first := firstDepositAt.UnixMilli()
	closedApyPeriod := first + ((now.UnixMilli()-first)/day)*day
	return msTimeMillis((closedApyPeriod / day) * day)
}

func checkpointSeedSnapshot(checkpoint earningsCheckpointWire) (earningsPortfolioSnapshot, error) {
	slot, err := parseI64(checkpoint.Seed.ObservedSlot)
	if err != nil {
		return earningsPortfolioSnapshot{}, err
	}
	snapshot := earningsPortfolioSnapshot{
		observedAt:   mustParseISO(checkpoint.At),
		observedSlot: slot,
		exposures:    make([]earningsPortfolioExposure, 0, len(checkpoint.Seed.Exposures)),
	}
	for _, exposure := range checkpoint.Seed.Exposures {
		amount, err := parseI64(exposure.AmountRaw)
		if err != nil {
			return earningsPortfolioSnapshot{}, err
		}
		snapshot.exposures = append(snapshot.exposures, earningsPortfolioExposure{
			amountRaw:     amount,
			kind:          exposure.Kind,
			liquidityMint: exposure.LiquidityMint,
			reserve:       cloneStr(exposure.Reserve),
			sourceID:      exposure.SourceID,
		})
	}
	return snapshot, nil
}

func createEmptyEarnEarningsRangeSet(now time.Time, timezone string) (EarningsRangeSet, error) {
	timezone = normalizeEarningsTimezone(timezone)
	ranges := EarningsRanges{}
	for _, rangeID := range earningsRangeIDs {
		calculated, err := calculateEarnEarnings(calculateEarnEarningsArgs{
			apySamples: []reserveApySample{},
			events:     []earningsEvent{},
			now:        now,
			pathEvents: []earningsPathEvent{},
			rangeID:    rangeID,
			timezone:   timezone,
		})
		if err != nil {
			return EarningsRangeSet{}, err
		}
		ranges.set(rangeID, calculated)
	}
	return EarningsRangeSet{
		Coverage: EarningsCoverage{
			GappedReserves:      []string{},
			MissingReserves:     []string{},
			StaleReserves:       []string{},
			EventCount:          0,
			ReserveCount:        0,
			SampleCount:         0,
			SampledReserveCount: 0,
		},
		Freshness:                "fresh",
		GeneratedAt:              isoMS(now),
		HistoryRevision:          getMaterialEarningsHistoryRevision(nil),
		Outcome:                  "empty",
		PrincipalMatchesHistory:  true,
		Ranges:                   ranges,
		SourcePrincipalAmountRaw: "0",
	}, nil
}

func staleSnapshotOverlay(snapshot earningsSnapshotRecord, now time.Time, staleReason string) EarningsRangeSet {
	payload := snapshot.payload
	ageMs := now.UnixMilli() - snapshot.generatedAt.UnixMilli()
	if ageMs < 0 {
		ageMs = 0
	}
	payload.Freshness = "stale"
	payload.SnapshotAgeMs = &ageMs
	payload.StaleReason = strPtr(staleReason)
	return payload
}

func warnEarningsRead(step string, err error) {
	code, name, detail := normalizedErrorKind(err)
	slog.Warn("earnings-read "+step, "errorCode", code, "errorName", name, "errorDetailCode", detail)
}

// readEarningsRangeSet ports readEarnEarningsRangeSet (read-service:930): the
// full rebuild. `useCheckpoint` mirrors the TS recursion flag for a retry that
// must ignore a mismatched stored checkpoint.
func readEarningsRangeSet(ctx context.Context, input earningsReadInput, deps earningsReadDeps, useCheckpoint bool) (EarningsRangeSet, error) {
	now := deps.now()
	timezone := normalizeEarningsTimezone(input.timezone)
	var snapshot *earningsSnapshotRecord
	if loaded, err := deps.loadSnapshot(ctx, input, timezone); err != nil {
		warnEarningsRead("snapshot read unavailable", err)
	} else if loaded != nil {
		snapshot = loaded
	}
	return rebuildEarningsRangeSet(ctx, input, deps, useCheckpoint, timezone, now, snapshot, time.Now())
}

func earningsReadFailure(err error, snapshot *earningsSnapshotRecord, now, startedAt time.Time) (EarningsRangeSet, error) {
	var history *earningsHistoryError
	unavailable, isUnavailable := err.(*earningsUnavailableError)
	if errors.As(err, &history) {
		unavailable = &earningsUnavailableError{
			code:       "history_incomplete",
			message:    history.Error(),
			detailCode: "principal_history_ambiguous",
		}
		isUnavailable = true
	}
	reason := "dependency_unavailable"
	switch {
	case isUnavailable:
		reason = unavailable.code
	case errors.Is(err, errEarningsRefreshInProgress):
		reason = "refresh_in_progress"
	}
	if snapshot != nil {
		slog.Warn("earnings-read stale",
			"staleReason", reason,
			"outcome", snapshot.payload.Outcome,
			"staleAgeMs", time.Since(startedAt).Milliseconds())
		return staleSnapshotOverlay(*snapshot, now, reason), nil
	}
	if isUnavailable {
		return EarningsRangeSet{}, unavailable
	}
	code, name, _ := normalizedErrorKind(err)
	slog.Warn("earnings-read unavailable",
		"errorCode", code,
		"errorName", name,
		"cause", earningsErrorCause(err),
		"elapsedMs", time.Since(startedAt).Milliseconds())
	return EarningsRangeSet{}, &earningsUnavailableError{code: "earnings_unavailable", message: "Earn earnings are unavailable.", detailCode: "earnings_unavailable"}
}

func rebuildEarningsRangeSet(ctx context.Context, input earningsReadInput, deps earningsReadDeps, useCheckpoint bool, timezone string, now time.Time, snapshot *earningsSnapshotRecord, startedAt time.Time) (payload EarningsRangeSet, err error) {
	defer func() {
		if err != nil {
			payload, err = earningsReadFailure(err, snapshot, now, startedAt)
		}
	}()

	var sourceRevision string
	if deps.loadSourceRevision != nil {
		sourceRevision, err = deps.loadSourceRevision(ctx, input)
		if err != nil {
			return EarningsRangeSet{}, err
		}
	}
	ledgerEvents, err := deps.loadLedgerEvents(ctx, input)
	if err != nil {
		return EarningsRangeSet{}, err
	}
	sortedLedger, err := sortEarningsEvents(ledgerEvents)
	if err != nil {
		return EarningsRangeSet{}, err
	}
	var firstLedgerDeposit *time.Time
	for i := range sortedLedger {
		if sortedLedger[i].eventType == "deposit" {
			firstLedgerDeposit = &sortedLedger[i].confirmedAt
			break
		}
	}

	historyFingerprint := func(before time.Time) *string {
		if deps.loadHistoryFingerprint == nil {
			return nil
		}
		fingerprint, err := deps.loadHistoryFingerprint(ctx, input, before)
		if err != nil {
			warnEarningsRead("checkpoint verification unavailable", err)
			return nil
		}
		return &fingerprint
	}

	var nextCheckpointAt *time.Time
	if firstLedgerDeposit != nil {
		at := earningsCheckpointTime(*firstLedgerDeposit, now)
		nextCheckpointAt = &at
	}
	ledgerFingerprint := func(before time.Time) string {
		return earningsLedgerFingerprint(sortedLedger, before.UnixMilli())
	}

	var checkpoint *earningsCheckpointWire
	if useCheckpoint && snapshot != nil && snapshot.checkpoint != nil &&
		snapshot.checkpoint.Version == 1 &&
		isServerVerifiedEarnEarningsPayload(&snapshot.payload) &&
		firstLedgerDeposit != nil &&
		snapshot.checkpoint.FirstDepositAt == isoMS(*firstLedgerDeposit) &&
		!mustParseISO(snapshot.checkpoint.At).After(now) {
		checkpoint = snapshot.checkpoint
	}
	if checkpoint != nil {
		broken := deps.loadHistoryFingerprint == nil ||
			checkpoint.LedgerFingerprint != ledgerFingerprint(mustParseISO(checkpoint.At))
		if !broken {
			if fp := historyFingerprint(mustParseISO(checkpoint.At)); fp == nil || checkpoint.HistoryFingerprint != *fp {
				broken = true
			}
		}
		if broken {
			checkpoint = nil
		}
	}
	var nextHistoryFingerprint *string
	if nextCheckpointAt != nil && deps.loadHistoryFingerprint != nil {
		nextHistoryFingerprint = historyFingerprint(*nextCheckpointAt)
	}

	positions, err := deps.loadPositions(ctx, input)
	if err != nil {
		return EarningsRangeSet{}, err
	}
	var observedSince *time.Time
	if checkpoint != nil {
		at := mustParseISO(checkpoint.At)
		observedSince = &at
	}
	loadedPortfolioSnapshots, err := deps.loadPortfolioSnapshots(ctx, input, observedSince)
	if err != nil {
		return EarningsRangeSet{}, err
	}
	var holdingEvents []earningsHoldingEvent
	if checkpoint == nil && deps.loadHoldingEvents != nil {
		holdingEvents, err = deps.loadHoldingEvents(ctx, input)
		if err != nil {
			return EarningsRangeSet{}, err
		}
	}

	completeSnapshots := loadedPortfolioSnapshots
	if checkpoint != nil {
		seedSnapshot, seedErr := checkpointSeedSnapshot(*checkpoint)
		if seedErr != nil {
			return EarningsRangeSet{}, seedErr
		}
		completeSnapshots = append([]earningsPortfolioSnapshot{seedSnapshot}, loadedPortfolioSnapshots...)
	}
	rawPortfolioSnapshots := buildHoldingBackedPortfolioSnapshots(completeSnapshots, holdingEvents)

	fallbackMint := ""
	if len(positions) > 0 {
		fallbackMint = positions[0].initialLiquidityMint
	}
	effectiveLedger := make([]earningsEvent, 0, len(sortedLedger))
	for _, event := range sortedLedger {
		if event.confirmedAt.After(now) {
			continue
		}
		if event.liquidityMint == "" {
			event.liquidityMint = fallbackMint
		}
		effectiveLedger = append(effectiveLedger, event)
	}
	var firstDepositAt *time.Time
	for i := range effectiveLedger {
		if effectiveLedger[i].eventType == "deposit" {
			firstDepositAt = &effectiveLedger[i].confirmedAt
			break
		}
	}
	var portfolioSnapshots []earningsPortfolioSnapshot
	if firstDepositAt != nil {
		start := *firstDepositAt
		if checkpoint != nil {
			start = mustParseISO(checkpoint.At)
		}
		portfolioSnapshots = normalizeEarningsPortfolioSnapshots(rawPortfolioSnapshots, start, now)
	}

	if len(positions) == 0 {
		return createEmptyEarnEarningsRangeSet(now, timezone)
	}

	var storedPrincipal int64
	for _, position := range positions {
		storedPrincipal += position.principalAmountRaw
	}
	hasPositionHistory := len(effectiveLedger) > 0
	somePositionEvidence := false
	for _, event := range effectiveLedger {
		if event.positionID == nil || event.initializesPosition == nil {
			hasPositionHistory = false
		}
		if event.positionID != nil || event.initializesPosition != nil {
			somePositionEvidence = true
		}
	}
	if !hasPositionHistory && somePositionEvidence {
		return EarningsRangeSet{}, &earningsHistoryError{detail: "principal_history_position_incomplete"}
	}

	candidates := legacyPrincipalClamps
	if hasPositionHistory {
		candidates = []principalClamp{clampPosition}
	}
	principalIndexes := map[principalClamp]*principalIndex{}
	var matchingClamps []principalClamp
	for _, clamp := range candidates {
		projected, err := principalAt(effectiveLedger, principalIndexes, now, clamp)
		if err != nil {
			return EarningsRangeSet{}, err
		}
		if projected == storedPrincipal {
			matchingClamps = append(matchingClamps, clamp)
		}
	}
	if len(matchingClamps) > 1 {
		for _, event := range effectiveLedger {
			firstAt, err := principalAt(effectiveLedger, principalIndexes, event.confirmedAt, matchingClamps[0])
			if err != nil {
				return EarningsRangeSet{}, err
			}
			secondAt, err := principalAt(effectiveLedger, principalIndexes, event.confirmedAt, matchingClamps[1])
			if err != nil {
				return EarningsRangeSet{}, err
			}
			if firstAt != secondAt {
				return EarningsRangeSet{}, &earningsUnavailableError{
					code:       "history_incomplete",
					message:    "Earn principal history is ambiguous.",
					detailCode: "principal_history_ambiguous",
				}
			}
		}
	}
	principalMatchesHistory := len(matchingClamps) > 0
	activeClamp := clampTotal
	if len(matchingClamps) > 0 {
		activeClamp = matchingClamps[0]
	}
	projectedPrincipal, err := principalAt(effectiveLedger, principalIndexes, now, activeClamp)
	if err != nil {
		return EarningsRangeSet{}, err
	}

	historyRevision := getPortfolioEarningsHistoryRevision(effectiveLedger, portfolioSnapshots)

	if !principalMatchesHistory || len(effectiveLedger) == 0 || (projectedPrincipal > 0 && len(portfolioSnapshots) == 0) {
		detailCode := "principal_history_mismatch"
		if principalMatchesHistory {
			detailCode = "deposit_history_incomplete"
		}
		return EarningsRangeSet{}, &earningsUnavailableError{
			code:       "history_incomplete",
			message:    "Earn principal history is incomplete.",
			detailCode: detailCode,
		}
	}
	if firstDepositAt == nil {
		return EarningsRangeSet{}, &earningsUnavailableError{
			code:       "history_incomplete",
			message:    "Earn deposit history is incomplete.",
			detailCode: "deposit_history_incomplete",
		}
	}

	apyCtx, cancelApy := context.WithTimeout(ctx, deps.apyTimeout)
	defer cancelApy()
	apySamples, err := deps.loadApySamples(apyCtx, input, earningsApyArgs{
		end:                now,
		pathEvents:         []earningsPathEvent{},
		portfolioSnapshots: portfolioSnapshots,
		reserves:           checkpointReservesOrNil(checkpoint),
		start:              *firstDepositAt,
	})
	if err != nil {
		var timeout *earningsApyTimeoutError
		if errors.As(err, &timeout) || ctx.Err() != nil && errors.Is(apyCtx.Err(), context.DeadlineExceeded) {
			return EarningsRangeSet{}, &earningsApyTimeoutError{}
		}
		return EarningsRangeSet{}, err
	}

	apyFingerprint := func(before time.Time, reserves []string) string {
		return earningsApyFingerprint(apySamples, before.UnixMilli(), reserves)
	}
	if checkpoint != nil &&
		checkpoint.ApyFingerprint != apyFingerprint(mustParseISO(checkpoint.At), checkpoint.Reserves) {
		return readEarningsRangeSet(ctx, input, deps, false)
	}

	coverage := getPortfolioEarningsCoverage(apySamples, now, portfolioSnapshots)
	if len(coverage.MissingReserves) > 0 || len(coverage.GappedReserves) > 0 || len(coverage.StaleReserves) > 0 || coverage.SampledReserveCount != coverage.ReserveCount {
		return EarningsRangeSet{}, &earningsUnavailableError{
			code:       "history_incomplete",
			message:    "Earn APY history coverage is incomplete.",
			detailCode: "apy_coverage_incomplete",
			coverage:   &coverage,
		}
	}

	ranges := EarningsRanges{}
	for _, rangeID := range earningsRangeIDs {
		var checkpointResponse *earningsCheckpointResponse
		if checkpoint != nil {
			checkpointResponse = &earningsCheckpointResponse{
				at:       mustParseISO(checkpoint.At),
				response: checkpoint.Ranges.at(rangeID),
			}
		}
		calculated, calcErr := calculateEarnEarnings(calculateEarnEarningsArgs{
			apySamples:         apySamples,
			events:             effectiveLedger,
			now:                now,
			pathEvents:         []earningsPathEvent{},
			portfolioSnapshots: portfolioSnapshots,
			principalClamp:     activeClamp,
			rangeID:            rangeID,
			timezone:           timezone,
			checkpoint:         checkpointResponse,
		})
		if calcErr != nil {
			return EarningsRangeSet{}, calcErr
		}
		ranges.set(rangeID, calculated)
	}
	if checkpoint != nil {
		if fp := historyFingerprint(mustParseISO(checkpoint.At)); fp == nil || checkpoint.HistoryFingerprint != *fp {
			return readEarningsRangeSet(ctx, input, deps, false)
		}
	}

	var nextCheckpoint *earningsCheckpointWire
	if checkpoint != nil && nextCheckpointAt != nil && checkpoint.At == isoMS(*nextCheckpointAt) {
		nextCheckpoint = checkpoint
	}
	var seed *earningsPortfolioSnapshot
	if nextCheckpointAt != nil {
		for i := range portfolioSnapshots {
			if portfolioSnapshots[i].observedAt.Before(*nextCheckpointAt) {
				seed = &portfolioSnapshots[i]
			}
		}
	}
	completeBeforeCheckpoint := checkpoint != nil
	if !completeBeforeCheckpoint && nextCheckpointAt != nil {
		for _, item := range loadedPortfolioSnapshots {
			if item.observedAt.Before(*nextCheckpointAt) {
				completeBeforeCheckpoint = true
				break
			}
		}
	}
	if nextCheckpointAt != nil && seed != nil && completeBeforeCheckpoint &&
		nextHistoryFingerprint != nil && firstDepositAt != nil {
		verify := historyFingerprint(*nextCheckpointAt)
		if verify != nil && *nextHistoryFingerprint == *verify {
			checkpointRanges := EarningsRanges{}
			for _, rangeID := range earningsRangeIDs {
				var checkpointResponse *earningsCheckpointResponse
				if checkpoint != nil {
					checkpointResponse = &earningsCheckpointResponse{
						at:       mustParseISO(checkpoint.At),
						response: checkpoint.Ranges.at(rangeID),
					}
				}
				calculated, calcErr := calculateEarnEarnings(calculateEarnEarningsArgs{
					apySamples:         apySamples,
					events:             effectiveLedger,
					now:                *nextCheckpointAt,
					pathEvents:         []earningsPathEvent{},
					portfolioSnapshots: portfolioSnapshots,
					principalClamp:     activeClamp,
					rangeID:            rangeID,
					timezone:           timezone,
					checkpoint:         checkpointResponse,
				})
				if calcErr != nil {
					return EarningsRangeSet{}, calcErr
				}
				checkpointRanges.set(rangeID, calculated)
			}
			wire, wireErr := buildCheckpointWire(*nextCheckpointAt, *firstDepositAt, *nextHistoryFingerprint, ledgerFingerprint(*nextCheckpointAt), apyFingerprint(*nextCheckpointAt, nil), apySamples, checkpointRanges, *seed)
			if wireErr != nil {
				return EarningsRangeSet{}, wireErr
			}
			nextCheckpoint = wire
		}
	}

	payload = EarningsRangeSet{
		Coverage:                 coverage,
		Freshness:                "fresh",
		GeneratedAt:              isoMS(now),
		HistoryRevision:          historyRevision,
		Outcome:                  "ready",
		PrincipalMatchesHistory:  principalMatchesHistory,
		Ranges:                   ranges,
		SourcePrincipalAmountRaw: formatInt64(projectedPrincipal),
	}

	var savedRevision *string
	skipSave := false
	if deps.loadSourceRevision == nil {
		savedRevision = strPtr(sourceRevision)
	} else {
		latest, revErr := deps.loadSourceRevision(ctx, input)
		if revErr != nil {
			// TS evaluates the revision inside the saveSnapshot arguments: a
			// failed re-read skips the save and its catch logs, it never stores
			// a NULL-revision snapshot.
			warnEarningsRead("snapshot write unavailable", revErr)
			skipSave = true
		} else if latest == sourceRevision {
			savedRevision = strPtr(sourceRevision)
		}
	}
	if !skipSave {
		if err := deps.saveSnapshot(ctx, earningsSaveArgs{
			input:          input,
			payload:        payload,
			principal:      projectedPrincipal,
			timezone:       timezone,
			sourceRevision: savedRevision,
			checkpoint:     nextCheckpoint,
		}); err != nil {
			warnEarningsRead("snapshot write unavailable", err)
		}
	}

	slog.Info("earnings-read ready",
		"calculationMode", map[bool]string{true: "incremental", false: "full"}[checkpoint != nil],
		"outcome", payload.Outcome,
		"principalMatchesHistory", principalMatchesHistory,
		"historyRevision", historyRevision,
		"reserveCount", coverage.ReserveCount,
		"sampleCount", coverage.SampleCount,
		"totalMs", time.Since(startedAt).Milliseconds())
	return payload, nil
}

func checkpointReservesOrNil(checkpoint *earningsCheckpointWire) []string {
	if checkpoint == nil {
		return nil
	}
	return checkpoint.Reserves
}

func buildCheckpointWire(at time.Time, firstDepositAt time.Time, historyFingerprint, ledgerFingerprint, apyFingerprint string, apySamples []reserveApySample, ranges EarningsRanges, seed earningsPortfolioSnapshot) (*earningsCheckpointWire, error) {
	reserveSet := map[string]bool{}
	var reserves []string
	for _, sample := range apySamples {
		if sample.reserve != nil && !reserveSet[*sample.reserve] {
			reserveSet[*sample.reserve] = true
			reserves = append(reserves, *sample.reserve)
		}
	}
	sortStrings(reserves)

	exposures := make([]earningsCheckpointSeedExposure, 0, len(seed.exposures))
	for _, exposure := range seed.exposures {
		exposures = append(exposures, earningsCheckpointSeedExposure{
			AmountRaw:     formatInt64(exposure.amountRaw),
			Kind:          exposure.kind,
			LiquidityMint: exposure.liquidityMint,
			Reserve:       cloneStr(exposure.reserve),
			SourceID:      exposure.sourceID,
		})
	}
	return &earningsCheckpointWire{
		Version:            1,
		At:                 isoMS(at),
		FirstDepositAt:     isoMS(firstDepositAt),
		HistoryFingerprint: historyFingerprint,
		LedgerFingerprint:  ledgerFingerprint,
		ApyFingerprint:     apyFingerprint,
		Reserves:           ensureStrings(reserves),
		Ranges:             ranges,
		Seed: earningsCheckpointSeed{
			ObservedSlot: formatInt64(seed.observedSlot),
			Exposures:    exposures,
		},
	}, nil
}

func sortStrings(v []string) { sort.Slice(v, func(i, j int) bool { return v[i] < v[j] }) }
