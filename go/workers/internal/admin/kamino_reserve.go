package admin

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	reserveWindow          = 7 * 24 * time.Hour
	reserveSampleInterval  = 5 * time.Minute
	minTotalSupplyUSD      = 100_000
	maxSupplyAPY           = 0.5
	VerifiedReserveMaxAge  = 240 * time.Second
	reserveMonitorFresh    = 5 * time.Minute
	reserveMonitorStaleTTL = 15 * time.Minute
)

var excludedGraphReserves = map[string]bool{
	"CAPsU1H49e6WuiQdte1ZU6zJaSGWXBSogAa1w6mhiKo2": true,
}

type SafeReserveApyStatusRow struct {
	Average24hApyPercent   *float64 `json:"average24hApyPercent"`
	Average7dApyPercent    *float64 `json:"average7dApyPercent"`
	LatestObservedAt       *string  `json:"latestObservedAt"`
	LiquidityMint          string   `json:"liquidityMint"`
	Market                 string   `json:"market"`
	MarketName             *string  `json:"marketName"`
	Reserve                string   `json:"reserve"`
	Status                 string   `json:"status"`
	SupplyApyPercent       *float64 `json:"supplyApyPercent"`
	Symbol                 *string  `json:"symbol"`
	TotalSupplyUsdEstimate *float64 `json:"totalSupplyUsdEstimate"`
}

type SafeReserveMintEligibilitySummary struct {
	BestSupplyApyPercent *float64 `json:"bestSupplyApyPercent"`
	EligibleReserveCount int      `json:"eligibleReserveCount"`
	LiquidityMint        string   `json:"liquidityMint"`
	Reason               string   `json:"reason"`
	Status               string   `json:"status"`
	Symbol               string   `json:"symbol"`
}

type SafeReserveApySeries struct {
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	LiquidityMint string  `json:"liquidityMint"`
	MarketName    *string `json:"marketName"`
	Reserve       string  `json:"reserve"`
}

type SafeReserveApyChartPoint map[string]any

type SafeReserveApyWindow struct {
	EndedAt   string `json:"endedAt"`
	StartedAt string `json:"startedAt"`
}

type SafeReserveApyMonitorData struct {
	ChartPoints           []SafeReserveApyChartPoint `json:"chartPoints"`
	GeneratedAt           string                     `json:"generatedAt"`
	SampleIntervalMinutes int                        `json:"sampleIntervalMinutes"`
	Series                []SafeReserveApySeries     `json:"series"`
	Statuses              []SafeReserveApyStatusRow  `json:"statuses"`
	Window                SafeReserveApyWindow       `json:"window"`
}

type currentCandidate struct {
	liquidityMint          string
	market                 string
	marketName             *string
	observedAt             *time.Time
	reserve                string
	supplyApy              *float64
	symbol                 *string
	totalSupplyUsdEstimate *float64
}

func classifyCandidate(row currentCandidate) string {
	if row.observedAt == nil {
		return "no-current-row"
	}
	if row.totalSupplyUsdEstimate == nil || *row.totalSupplyUsdEstimate <= minTotalSupplyUSD {
		return "below-liquidity"
	}
	if row.supplyApy == nil || *row.supplyApy < 0 || *row.supplyApy >= maxSupplyAPY {
		return "apy-out-of-range"
	}
	return "eligible"
}

func finiteOrNil(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	return v
}

func apyPercent(v *float64) *float64 {
	v = finiteOrNil(v)
	if v == nil {
		return nil
	}
	p := *v * 100
	return &p
}

func compareLocale(left, right string) int {
	l, r := strings.ToLower(left), strings.ToLower(right)
	if c := strings.Compare(l, r); c != 0 {
		return c
	}
	return -strings.Compare(left, right)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func reserveLabel(row SafeReserveApyStatusRow) string {
	symbol, ok := EarnStablecoinSymbol(row.LiquidityMint)
	if !ok {
		symbol = "Unknown"
		if row.Symbol != nil && *row.Symbol != "" {
			symbol = *row.Symbol
		}
	}
	market := row.Market
	if row.MarketName != nil {
		market = *row.MarketName
	}
	r := row.Reserve
	if len(r) >= 4 {
		r = r[:4] + "..." + r[len(r)-4:]
	}
	return fmt.Sprintf("%s · %s · %s", symbol, market, r)
}

func createStatusRows(rows []currentCandidate) []SafeReserveApyStatusRow {
	out := make([]SafeReserveApyStatusRow, 0, len(rows))
	for _, row := range rows {
		var observed *string
		if row.observedAt != nil {
			s := earnISO(*row.observedAt)
			observed = &s
		}
		out = append(out, SafeReserveApyStatusRow{
			LatestObservedAt:       observed,
			LiquidityMint:          row.liquidityMint,
			Market:                 row.market,
			MarketName:             row.marketName,
			Reserve:                row.reserve,
			Status:                 classifyCandidate(row),
			SupplyApyPercent:       apyPercent(row.supplyApy),
			Symbol:                 row.symbol,
			TotalSupplyUsdEstimate: finiteOrNil(row.totalSupplyUsdEstimate),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := compareLocale(deref(out[i].MarketName), deref(out[j].MarketName)); c != 0 {
			return c < 0
		}
		if c := compareLocale(out[i].Market, out[j].Market); c != 0 {
			return c < 0
		}
		return compareLocale(out[i].Reserve, out[j].Reserve) < 0
	})
	return out
}

func stablecoinMints() []string {
	mints := make([]string, len(EarnStablecoins))
	for i, coin := range EarnStablecoins {
		mints[i] = coin.Mint
	}
	return mints
}

func (d *Deps) CurrentSafeReserveApyStatuses(ctx context.Context) ([]SafeReserveApyStatusRow, error) {
	var rows []currentCandidate
	err := d.TimescaleRead(ctx, func(tx pgx.Tx) error {
		res, err := tx.Query(ctx, `
    SELECT
      supported.liquidity_mint AS "liquidityMint",
      supported.market AS "market",
      supported.market_name AS "marketName",
      latest.observed_at AS "observedAt",
      supported.reserve AS "reserve",
      latest.supply_apy AS "supplyApy",
      supported.symbol AS "symbol",
      latest.total_supply_usd_estimate AS "totalSupplyUsdEstimate"
    FROM kamino.supported_reserves AS supported
    LEFT JOIN LATERAL (
      SELECT
        updates.observed_at,
        updates.supply_apy,
        updates.total_supply_usd_estimate
      FROM kamino.latest_verified_reserve_updates AS updates
      WHERE updates.reserve = supported.reserve
        AND updates.market = supported.market
        AND updates.liquidity_mint = supported.liquidity_mint
        AND updates.verified_at >= now() - make_interval(
          secs => $1::float8
        )
        AND updates.verified_at <= now()
      ORDER BY updates.verified_at DESC
      LIMIT 1
    ) AS latest ON true
    WHERE supported.active = true
      AND supported.liquidity_mint = ANY($2::text[])
      AND supported.market = ANY($3::text[])
  `, VerifiedReserveMaxAge.Seconds(), stablecoinMints(), safeRiskBasketMarkets)
		if err != nil {
			return err
		}
		defer res.Close()
		for res.Next() {
			var row currentCandidate
			if err := res.Scan(&row.liquidityMint, &row.market, &row.marketName, &row.observedAt, &row.reserve, &row.supplyApy, &row.symbol, &row.totalSupplyUsdEstimate); err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return res.Err()
	})
	if err != nil {
		return nil, err
	}
	return createStatusRows(rows), nil
}

type apyHistoryRow struct {
	observedAt time.Time
	reserve    string
	supplyApy  float64
}

func (d *Deps) loadApyHistoryRows(ctx context.Context, startedAt, endedAt time.Time, reserves []string) ([]apyHistoryRow, error) {
	if len(reserves) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(reserves))
	for _, r := range reserves {
		if !seen[r] {
			seen[r] = true
			ids = append(ids, r)
		}
	}
	var rows []apyHistoryRow
	err := d.TimescaleRead(ctx, func(tx pgx.Tx) error {
		res, err := tx.Query(ctx, `
    WITH reserve_ids AS (
      SELECT unnest($1::text[]) AS reserve
    ),
    previous_samples AS (
      SELECT
        $2::timestamptz AS observed_at,
        reserve_ids.reserve,
        previous.supply_apy
      FROM reserve_ids
      LEFT JOIN LATERAL (
        SELECT updates.supply_apy
        FROM kamino.reserve_updates AS updates
        WHERE updates.reserve = reserve_ids.reserve
          AND updates.observed_at < $2::timestamptz
          AND updates.reserve_last_update_stale = false
          AND updates.supply_apy >= 0
          AND updates.supply_apy < $5::float8
        ORDER BY updates.observed_at DESC
        LIMIT 1
      ) previous ON true
      WHERE previous.supply_apy IS NOT NULL
    ),
    range_candidates AS (
      SELECT
        date_bin(
          make_interval(secs => $4::float8),
          updates.observed_at,
          $2::timestamptz
        ) AS sample_bucket,
        updates.observed_at,
        updates.reserve,
        updates.supply_apy
      FROM kamino.reserve_updates AS updates
      WHERE updates.reserve = ANY($1::text[])
        AND updates.observed_at >= $2::timestamptz
        AND updates.observed_at <= $3::timestamptz
        AND updates.reserve_last_update_stale = false
        AND updates.supply_apy >= 0
        AND updates.supply_apy < $5::float8
    ),
    range_samples AS (
      SELECT DISTINCT ON (reserve, sample_bucket)
        sample_bucket AS observed_at,
        reserve,
        supply_apy
      FROM range_candidates
      ORDER BY reserve, sample_bucket, observed_at DESC
    )
    SELECT observed_at, reserve, supply_apy
    FROM (
      SELECT observed_at, reserve, supply_apy, 0 AS sample_order
      FROM previous_samples
      UNION ALL
      SELECT observed_at, reserve, supply_apy, 1 AS sample_order
      FROM range_samples
    ) samples
    ORDER BY observed_at ASC, reserve ASC, sample_order ASC
  `, ids, startedAt, endedAt, reserveSampleInterval.Seconds(), maxSupplyAPY)
		if err != nil {
			return err
		}
		defer res.Close()
		for res.Next() {
			var row apyHistoryRow
			if err := res.Scan(&row.observedAt, &row.reserve, &row.supplyApy); err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return res.Err()
	})
	return rows, err
}

func createChartPoints(startedAt, endedAt time.Time, history []apyHistoryRow, series []SafeReserveApySeries) []SafeReserveApyChartPoint {
	byReserve := map[string][]apyHistoryRow{}
	for _, row := range history {
		byReserve[row.reserve] = append(byReserve[row.reserve], row)
	}
	for _, rows := range byReserve {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].observedAt.Before(rows[j].observedAt) })
	}
	intervalMs := reserveSampleInterval.Milliseconds()
	bucketCount := (endedAt.UnixMilli()-startedAt.UnixMilli())/intervalMs + 1
	cursor := map[string]int{}
	current := map[string]float64{}
	points := make([]SafeReserveApyChartPoint, 0, bucketCount)
	for i := int64(0); i < bucketCount; i++ {
		bucketMs := startedAt.UnixMilli() + i*intervalMs
		point := SafeReserveApyChartPoint{
			"observedAt":   earnISO(time.UnixMilli(bucketMs)),
			"observedAtMs": bucketMs,
		}
		for _, s := range series {
			rows := byReserve[s.Reserve]
			idx := cursor[s.Reserve]
			for idx < len(rows) && rows[idx].observedAt.UnixMilli() <= bucketMs {
				if v := rows[idx].supplyApy; !math.IsNaN(v) && !math.IsInf(v, 0) {
					current[s.Key] = v * 100
				}
				idx++
			}
			cursor[s.Reserve] = idx
			if v, ok := current[s.Key]; ok {
				point[s.Key] = v
			} else {
				point[s.Key] = nil
			}
		}
		points = append(points, point)
	}
	return points
}

func average(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	total := 0.0
	for _, v := range values {
		total += v
	}
	avg := total / float64(len(values))
	return &avg
}

func withApySummaries(points []SafeReserveApyChartPoint, endedAt time.Time, series []SafeReserveApySeries, statuses []SafeReserveApyStatusRow) []SafeReserveApyStatusRow {
	keyByReserve := map[string]string{}
	for _, s := range series {
		keyByReserve[s.Reserve] = s.Key
	}
	dayStartMs := endedAt.UnixMilli() - 24*60*60*1000
	out := make([]SafeReserveApyStatusRow, len(statuses))
	for i, status := range statuses {
		out[i] = status
		key, ok := keyByReserve[status.Reserve]
		if !ok {
			continue
		}
		var values7d, values24h []float64
		for _, point := range points {
			value, ok := point[key].(float64)
			if !ok {
				continue
			}
			values7d = append(values7d, value)
			if point["observedAtMs"].(int64) >= dayStartMs {
				values24h = append(values24h, value)
			}
		}
		out[i].Average24hApyPercent = average(values24h)
		out[i].Average7dApyPercent = average(values7d)
	}
	return out
}

func (d *Deps) SafeReserveApyMonitorDataFromStatuses(ctx context.Context, current []SafeReserveApyStatusRow, now time.Time) (SafeReserveApyMonitorData, error) {
	endedAt := now
	startedAt := endedAt.Add(-reserveWindow)
	base := make([]SafeReserveApyStatusRow, 0, len(current))
	for _, status := range current {
		if !excludedGraphReserves[status.Reserve] {
			base = append(base, status)
		}
	}
	series := make([]SafeReserveApySeries, len(base))
	reserves := make([]string, len(base))
	for i, row := range base {
		series[i] = SafeReserveApySeries{
			Key:           fmt.Sprintf("reserve%d", i+1),
			Label:         reserveLabel(row),
			LiquidityMint: row.LiquidityMint,
			MarketName:    row.MarketName,
			Reserve:       row.Reserve,
		}
		reserves[i] = row.Reserve
	}
	history, err := d.loadApyHistoryRows(ctx, startedAt, endedAt, reserves)
	if err != nil {
		return SafeReserveApyMonitorData{}, err
	}
	points := createChartPoints(startedAt, endedAt, history, series)
	return SafeReserveApyMonitorData{
		ChartPoints:           points,
		GeneratedAt:           earnISO(now),
		SampleIntervalMinutes: int(reserveSampleInterval.Minutes()),
		Series:                series,
		Statuses:              withApySummaries(points, endedAt, series, base),
		Window:                SafeReserveApyWindow{EndedAt: earnISO(endedAt), StartedAt: earnISO(startedAt)},
	}, nil
}

func (d *Deps) SafeReserveApyMonitorDataAt(ctx context.Context, now time.Time) (SafeReserveApyMonitorData, error) {
	current, err := d.CurrentSafeReserveApyStatuses(ctx)
	if err != nil {
		return SafeReserveApyMonitorData{}, err
	}
	return d.SafeReserveApyMonitorDataFromStatuses(ctx, current, now)
}

var reserveMonitorCache struct {
	sync.Mutex
	value      SafeReserveApyMonitorData
	expiresAt  time.Time
	staleUntil time.Time
	loaded     bool
	refreshing bool
}

func (d *Deps) SafeReserveApyMonitorData(ctx context.Context) (SafeReserveApyMonitorData, error) {
	c := &reserveMonitorCache
	c.Lock()
	now := time.Now()
	if c.loaded && now.Before(c.expiresAt) {
		defer c.Unlock()
		return c.value, nil
	}
	if c.loaded && now.Before(c.staleUntil) {
		defer c.Unlock()
		if !c.refreshing {
			c.refreshing = true
			go d.refreshReserveMonitor()
		}
		return c.value, nil
	}
	c.Unlock()
	return d.refreshReserveMonitorFrom(ctx)
}

func (d *Deps) refreshReserveMonitor() {
	_, _ = d.refreshReserveMonitorFrom(context.Background())
}

func (d *Deps) refreshReserveMonitorFrom(ctx context.Context) (SafeReserveApyMonitorData, error) {
	c := &reserveMonitorCache
	data, err := d.SafeReserveApyMonitorDataAt(ctx, time.Now())
	c.Lock()
	defer c.Unlock()
	c.refreshing = false
	if err != nil {
		return SafeReserveApyMonitorData{}, err
	}
	refreshed := time.Now()
	c.value, c.loaded = data, true
	c.expiresAt = refreshed.Add(reserveMonitorFresh)
	c.staleUntil = refreshed.Add(reserveMonitorStaleTTL)
	return data, nil
}

func (d *Deps) CurrentVerifiedReserveEligibilityByMint(ctx context.Context) ([]SafeReserveMintEligibilitySummary, error) {
	statuses, err := d.CurrentSafeReserveApyStatuses(ctx)
	if err != nil {
		return nil, err
	}
	return SummarizeSafeReserveEligibilityByMint(statuses), nil
}

func SummarizeSafeReserveEligibilityByMint(statuses []SafeReserveApyStatusRow) []SafeReserveMintEligibilitySummary {
	out := make([]SafeReserveMintEligibilitySummary, 0, len(EarnStablecoins))
	for _, coin := range EarnStablecoins {
		var rows, eligible []SafeReserveApyStatusRow
		for _, row := range statuses {
			if row.LiquidityMint != coin.Mint {
				continue
			}
			rows = append(rows, row)
			if row.Status == "eligible" {
				eligible = append(eligible, row)
			}
		}
		if len(eligible) > 0 {
			var best *float64
			for _, row := range eligible {
				if row.SupplyApyPercent != nil && (best == nil || *row.SupplyApyPercent > *best) {
					v := *row.SupplyApyPercent
					best = &v
				}
			}
			noun := "reserves"
			if len(eligible) == 1 {
				noun = "reserve"
			}
			out = append(out, SafeReserveMintEligibilitySummary{
				BestSupplyApyPercent: best,
				EligibleReserveCount: len(eligible),
				LiquidityMint:        coin.Mint,
				Reason:               fmt.Sprintf("%d eligible Safe %s", len(eligible), noun),
				Status:               "eligible",
				Symbol:               coin.Symbol,
			})
			continue
		}
		reason, status := describeIneligibleReserves(rows)
		out = append(out, SafeReserveMintEligibilitySummary{
			LiquidityMint: coin.Mint,
			Reason:        reason,
			Status:        status,
			Symbol:        coin.Symbol,
		})
	}
	return out
}

func describeIneligibleReserves(rows []SafeReserveApyStatusRow) (reason, status string) {
	if len(rows) == 0 {
		return "No supported Safe reserve", "no-supported-reserve"
	}
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Status]++
	}
	var reasons []string
	if n := counts["no-current-row"]; n > 0 {
		reasons = append(reasons, fmt.Sprintf("%d without a fresh verified row", n))
	}
	if n := counts["stale"]; n > 0 {
		reasons = append(reasons, fmt.Sprintf("%d stale", n))
	}
	if n := counts["below-liquidity"]; n > 0 {
		reasons = append(reasons, fmt.Sprintf("%d below the liquidity threshold", n))
	}
	if n := counts["apy-out-of-range"]; n > 0 {
		reasons = append(reasons, fmt.Sprintf("%d with APY outside bounds", n))
	}
	switch {
	case counts["no-current-row"] > 0:
		status = "no-current-row"
	case counts["stale"] > 0:
		status = "stale"
	case counts["below-liquidity"] > 0:
		status = "below-liquidity"
	default:
		status = "apy-out-of-range"
	}
	reason = strings.Join(reasons, ", ")
	if reason == "" {
		reason = "No eligible Safe reserve"
	}
	return reason, status
}
