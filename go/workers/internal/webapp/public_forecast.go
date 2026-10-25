package webapp

// GET /api/smart-accounts/earn-forecast/summary, porting
// web/apps/web/src/app/api/smart-accounts/earn-forecast/summary/route.ts +
// web/apps/web/src/lib/kamino/earn-forecast.server.ts. Public read: no auth on
// either side. Response and error bodies follow the TS key order exactly so
// bytes stay comparable with the Next route.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

const earnForecastSummaryPath = "/api/smart-accounts/earn-forecast/summary"

type EarnForecastApyWindow struct {
	EndedAt   string `json:"endedAt"`
	StartedAt string `json:"startedAt"`
}

type EarnForecastResponse struct {
	APYBps       int64                 `json:"apyBps"`
	Availability string                `json:"availability"`
	RangeHighBps int64                 `json:"rangeHighBps"`
	RangeLowBps  int64                 `json:"rangeLowBps"`
	Source       string                `json:"source"`
	Strategy     string                `json:"strategy"`
	UpdatedAt    string                `json:"updatedAt"`
	Window       EarnForecastApyWindow `json:"window"`
}

type EarnForecastApyHistorySample struct {
	APYBps     int64  `json:"apyBps"`
	ObservedAt string `json:"observedAt"`
}

type EarnForecastApyHistorySeries struct {
	Key      string                         `json:"key"`
	Label    string                         `json:"label"`
	Metadata map[string]any                 `json:"metadata"`
	Samples  []EarnForecastApyHistorySample `json:"samples"`
}

type EarnForecastApyHistoryResponse struct {
	FeeBps      int64                          `json:"feeBps"`
	GeneratedAt string                         `json:"generatedAt"`
	RiskProfile string                         `json:"riskProfile"`
	Samples     []EarnForecastApyHistorySample `json:"samples"`
	Series      []EarnForecastApyHistorySeries `json:"series"`
	Window      EarnForecastApyWindow          `json:"window"`
}

type EarnForecastSummaryResponse struct {
	Forecast EarnForecastResponse           `json:"forecast"`
	History  EarnForecastApyHistoryResponse `json:"history"`
}

type forecastRequest struct{}

type forecastOutput struct{ Body EarnForecastSummaryResponse }

// forecastUnavailableError renders Next's 503 body: error is a bare string,
// not the {code,message} envelope.
type forecastUnavailableError struct {
	Message string `json:"error"`
	status  int
}

func (e *forecastUnavailableError) Error() string  { return e.Message }
func (e *forecastUnavailableError) GetStatus() int { return e.status }

func (h earnRoutes) registerForecast(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-forecast-summary",
		Method:      http.MethodGet,
		Path:        earnForecastSummaryPath,
		Summary:     "Realized Earn APY summary and 30d history",
		Responses:   errorResponses[forecastUnavailableError](api, 503),
	}, h.earnForecastSummary)
}

func (h earnRoutes) earnForecastSummary(ctx context.Context, _ *forecastRequest) (*forecastOutput, error) {
	forecast := h.mediumFeeAwareEarnForecast(ctx, yieldForecastDeps(h.YieldDB), time.Now())
	if forecast == nil {
		return nil, &forecastUnavailableError{Message: "Earn APY is not measured yet", status: http.StatusServiceUnavailable}
	}
	return &forecastOutput{Body: EarnForecastSummaryResponse{Forecast: forecast.summary, History: forecast.history}}, nil
}

type forecastResult struct {
	history      EarnForecastApyHistoryResponse
	summary      EarnForecastResponse
	measuredAtMS int64
}

func realizedResultToForecast(result *forecastRealizedResult, now time.Time) *forecastResult {
	generatedAt := isoMilliseconds(now)
	window := EarnForecastApyWindow{
		EndedAt:   isoMilliseconds(time.UnixMilli(result.measuredAtMS)),
		StartedAt: generatedAt,
	}
	if len(result.loyalSeries) > 0 {
		window.StartedAt = result.loyalSeries[0].observedAt
	}
	rangeHigh, rangeLow := result.headlineBps, result.headlineBps
	for _, sample := range result.loyalSeries {
		rangeHigh = max(rangeHigh, sample.apyBps)
		rangeLow = min(rangeLow, sample.apyBps)
	}
	samples := make([]EarnForecastApyHistorySample, len(result.loyalSeries))
	for i, sample := range result.loyalSeries {
		samples[i] = EarnForecastApyHistorySample{APYBps: sample.apyBps, ObservedAt: sample.observedAt}
	}
	reserveSamples := make([]EarnForecastApyHistorySample, len(result.mainUsdcReserveSeries))
	for i, sample := range result.mainUsdcReserveSeries {
		reserveSamples[i] = EarnForecastApyHistorySample{APYBps: sample.apyBps, ObservedAt: sample.observedAt}
	}
	return &forecastResult{
		measuredAtMS: result.measuredAtMS,
		history: EarnForecastApyHistoryResponse{
			FeeBps:      forecastCrossMintFeeBps,
			GeneratedAt: generatedAt,
			RiskProfile: forecastSafeRiskProfile,
			Samples:     samples,
			Series: []EarnForecastApyHistorySeries{
				{
					Key:      "loyal",
					Label:    "Loyal Earn",
					Metadata: map[string]any{"metric": forecastRealizedMetric},
					Samples:  samples,
				},
				{
					Key:   "mainUsdcReserve",
					Label: "Kamino Main USDC",
					Metadata: map[string]any{
						"liquidityMint": KaminoMainMarketUSDCMint,
						"market":        KaminoMainMarket,
						"metric":        forecastRealizedMetric,
						"reserve":       KaminoMainMarketUSDCReserve,
					},
					Samples: reserveSamples,
				},
			},
			Window: window,
		},
		summary: EarnForecastResponse{
			APYBps:       result.headlineBps,
			Availability: "available",
			RangeHighBps: rangeHigh,
			RangeLowBps:  rangeLow,
			Source:       result.source,
			Strategy:     forecastRealizedStrategy,
			UpdatedAt:    window.EndedAt,
			Window:       window,
		},
	}
}

type forecastDeps struct {
	hasRecentPrices func(ctx context.Context, cluster string, sinceMS int64) (bool, error)
	loadAllocations func(ctx context.Context, cluster string, sinceMS, nowMS int64) (forecastAllocationHistory, error)
	loadHistories   func(ctx context.Context, cluster string, reserves []string, sinceMS int64) (map[string][]forecastSharePricePoint, error)
}

func yieldForecastDeps(pool *pgxpool.Pool) forecastDeps {
	// pool is the same yield database the Next route reads.
	return forecastDeps{
		hasRecentPrices: func(ctx context.Context, cluster string, sinceMS int64) (bool, error) {
			return hasEarnSharePriceHistory(ctx, pool, cluster, sinceMS)
		},
		loadAllocations: func(ctx context.Context, cluster string, sinceMS, nowMS int64) (forecastAllocationHistory, error) {
			return loadEarnAllocationHistory(ctx, pool, cluster, sinceMS, nowMS)
		},
		loadHistories: func(ctx context.Context, cluster string, reserves []string, sinceMS int64) (map[string][]forecastSharePricePoint, error) {
			return loadReserveSharePriceHistories(ctx, pool, cluster, reserves, sinceMS)
		},
	}
}

func (h earnRoutes) realizedEarnForecast(ctx context.Context, deps forecastDeps, now time.Time) (*forecastResult, error) {
	cluster := string(h.Cluster.Loyal())
	nowMS := now.UnixMilli()
	sinceMS := nowMS - forecastSeriesWindowMS - forecastRealizedWindowMS - forecastPreWindowLookback
	ok, err := deps.hasRecentPrices(ctx, cluster, sinceMS)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	allocations, err := deps.loadAllocations(ctx, cluster, sinceMS, nowMS)
	if err != nil {
		return nil, err
	}
	reserves := forecastReservesFor(allocations)
	histories, err := deps.loadHistories(ctx, cluster, reserves, sinceMS)
	if err != nil {
		return nil, err
	}
	result := forecastComputeRealizedApy(forecastRealizedInput{histories: histories, allocations: allocations, nowMS: nowMS})
	if result == nil {
		return nil, nil
	}
	return realizedResultToForecast(result, now), nil
}

func forecastReservesFor(allocations forecastAllocationHistory) []string {
	seen := map[string]bool{}
	var reserves []string
	for i := range allocations.snapshots {
		for _, reserve := range allocations.snapshots[i].weights.order {
			if !seen[reserve] {
				seen[reserve] = true
				reserves = append(reserves, reserve)
			}
		}
	}
	if !seen[KaminoMainMarketUSDCReserve] {
		reserves = append(reserves, KaminoMainMarketUSDCReserve)
	}
	return reserves
}

var (
	forecastCacheMu        sync.Mutex
	forecastCacheExpiresAt time.Time
	forecastCacheValue     *forecastResult
	forecastLastRealized   *forecastResult
)

func resetEarnForecastCacheForTests() {
	forecastCacheMu.Lock()
	defer forecastCacheMu.Unlock()
	forecastCacheExpiresAt = time.Time{}
	forecastCacheValue = nil
	forecastLastRealized = nil
}

// mediumFeeAwareEarnForecast serves a 5-minute cache with a 3-hour stale
// fallback as availability "stale"; the lock single-flights the DB reads.
func (h earnRoutes) mediumFeeAwareEarnForecast(ctx context.Context, deps forecastDeps, now time.Time) *forecastResult {
	forecastCacheMu.Lock()
	defer forecastCacheMu.Unlock()
	nowMS := now.UnixMilli()
	if forecastCacheExpiresAt.After(now) {
		return forecastCacheValue
	}

	// The cached result serves every caller, so one caller hanging up must
	// not cancel the read and cache a miss (Next's read ignores the abort).
	realized, err := h.realizedEarnForecast(context.WithoutCancel(ctx), deps, now)
	if err != nil {
		slog.Warn("[earn-forecast] realized APY read failed", "error", err)
	}
	if realized != nil {
		forecastLastRealized = realized
	}

	var value *forecastResult
	if realized != nil {
		value = realized
	} else if forecastLastRealized != nil && nowMS-forecastLastRealized.measuredAtMS <= forecastMaxStalenessMS {
		stale := *forecastLastRealized
		stale.summary.Availability = "stale"
		value = &stale
	}
	remainingFreshMS := forecastCacheTTLMS
	if value != nil {
		remainingFreshMS = max(0, forecastMaxStalenessMS-(nowMS-value.measuredAtMS))
	}
	forecastCacheExpiresAt = now.Add(time.Duration(min(forecastCacheTTLMS, remainingFreshMS)) * time.Millisecond)
	forecastCacheValue = value
	return value
}

func hasEarnSharePriceHistory(ctx context.Context, pool *pgxpool.Pool, cluster string, sinceMS int64) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var exists bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM loyal_yield.earn_reserve_share_prices
  WHERE cluster = $1 AND observed_at >= $2
)`, cluster, time.UnixMilli(sinceMS)).Scan(&exists)
	return exists, err
}

func loadEarnAllocationHistory(ctx context.Context, pool *pgxpool.Pool, cluster string, sinceMS, nowMS int64) (forecastAllocationHistory, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `SELECT excluded_amount_raw, idle_amount_raw, observed_at, reserve_amounts
FROM loyal_yield.earn_fleet_allocations_hourly
WHERE cluster = $1
  AND observed_hour >= $2
  AND observed_hour <= $3
  AND observed_at <= $3
ORDER BY observed_hour ASC`,
		cluster, time.UnixMilli(floorDiv(sinceMS-forecastMaxAllocGapMS, forecastHourMS)*forecastHourMS), time.UnixMilli(nowMS))
	if err != nil {
		return forecastAllocationHistory{}, err
	}
	defer rows.Close()
	var samples []fleetAllocationSample
	for rows.Next() {
		var observedAt time.Time
		var idleRaw, excludedRaw string
		var reserveAmounts []byte
		if err := rows.Scan(&excludedRaw, &idleRaw, &observedAt, &reserveAmounts); err != nil {
			return forecastAllocationHistory{}, err
		}
		amounts, _ := parseReserveAmounts(reserveAmounts)
		samples = append(samples, fleetAllocationSample{
			observedAtMS:      observedAt.UnixMilli(),
			reserveAmounts:    amounts,
			idleAmountRaw:     idleRaw,
			excludedAmountRaw: excludedRaw,
		})
	}
	if err := rows.Err(); err != nil {
		return forecastAllocationHistory{}, err
	}
	return earnAllocationHistoryFromSamples(samples, sinceMS), nil
}

// parseReserveAmounts reads the reserve_amounts jsonb object preserving its
// stored key order, which the TS Map inherits from the wire text. Values must
// be strings; anything else just fails that reserve's safeWeight.
func parseReserveAmounts(raw []byte) ([]fleetReserveAmount, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil {
		return nil, false
	} else if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, false
	}
	var amounts []fleetReserveAmount
	allStrings := true
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, false
		}
		var value string
		if err := dec.Decode(&value); err != nil {
			allStrings = false
			var skip any
			if err := dec.Decode(&skip); err != nil {
				return nil, false
			}
			continue
		}
		amounts = append(amounts, fleetReserveAmount{reserve: key, amountRaw: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	return amounts, allStrings
}

func loadReserveSharePriceHistories(ctx context.Context, pool *pgxpool.Pool, cluster string, reserves []string, sinceMS int64) (map[string][]forecastSharePricePoint, error) {
	histories := map[string][]forecastSharePricePoint{}
	if len(reserves) == 0 {
		return histories, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `SELECT observed_at, reserve, share_price
FROM loyal_yield.earn_reserve_share_prices
WHERE cluster = $1 AND reserve = ANY($2) AND observed_at >= $3
ORDER BY observed_at ASC`,
		cluster, reserves, time.UnixMilli(sinceMS))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var observedAt time.Time
		var reserve string
		var sharePrice float64
		if err := rows.Scan(&observedAt, &reserve, &sharePrice); err != nil {
			return nil, err
		}
		histories[reserve] = append(histories[reserve], forecastSharePricePoint{observedAtMS: observedAt.UnixMilli(), sharePrice: sharePrice})
	}
	return histories, rows.Err()
}
