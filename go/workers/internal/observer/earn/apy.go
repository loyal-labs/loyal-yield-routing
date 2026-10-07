package earn

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Hourly Earn APY snapshots, ported from balance-sweep-ata-monitor
// earn_apy.rs: a time-weighted 30-day trailing APY of the best eligible
// Kamino stable reserve per sample hour, Timescale reserve updates in, one
// loyal_yield.earn_apy_hourly_snapshots row per strategy and hour out. The
// table is owned by the existing production schema; the Go refresher never
// creates it.

const mainUSDCReserve = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"

// APYStrategy is one published fee-aware strategy.
type APYStrategy struct {
	Name, RiskProfile string
	markets           []string
	FeeBPS            int16
}

var apyStableMints = []string{
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
	"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA",
	"2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH", "DEkqHyPN7GMRJ5cArtQFAWefqbZb33Hyf6s5iCwjEonT",
	"Eh6XEPhSwoLv5wFApukmnaVSHQ6sAnoD9BmgmwQoN2sN", "CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH",
	"AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj", "USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB",
	"9zNQRsGLjNKwCUU5Gq5LR8beUCPzQMVMqKAi3SSZh54u", "AUSD1jCcCyPLybk1YnvPWsHQSrZ46dxwoMniN4N2UEB9",
	"3ThdFZQKM6kRyVGLG48kaPg5TRMhYMKY1iCRa9xop1WC", "BTRR3sj1Bn2ZjuemgbeQ6SCtf84iXS81CS7UDTSxUCaK",
}

// APYStrategyForRiskProfile is earn_apy_strategy_for_risk_profile.
func APYStrategyForRiskProfile(value string) (APYStrategy, bool) {
	markets := keyStrings(safeMarkets)
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "safe":
		return APYStrategy{Name: "safe_fee_aware_1bps", RiskProfile: "safe", markets: markets, FeeBPS: 1}, true
	case "medium":
		return APYStrategy{Name: "medium_fee_aware_1bps", RiskProfile: "medium", markets: append(markets, keyStrings(mediumMarkets)...), FeeBPS: 1}, true
	}
	return APYStrategy{}, false
}

const (
	apyWindow          = 30 * 24 * time.Hour
	apyOutputSpan      = 30 * 24 * time.Hour
	apyMaxSupply       = 0.5
	apyMinTotalSupply  = 100_000.0
	apyRefreshOverlap  = 3 * time.Hour
	apySeedLookback    = 48 * time.Hour
	APYRefreshInterval = time.Hour
)

// APYRefresher writes hourly Earn APY snapshots.
type APYRefresher struct {
	timescale, neon *pgxpool.Pool
	strategies      []APYStrategy
}

func NewAPYRefresher(timescale, neon *pgxpool.Pool, strategies []APYStrategy) *APYRefresher {
	return &APYRefresher{timescale: timescale, neon: neon, strategies: strategies}
}

type reserveUpdate struct {
	observedAt          time.Time
	reserve, mint       string
	stale               bool
	totalSupply, supply float64
}

type apySnapshot struct {
	sampleHour, windowStart time.Time
	loyalBPS, mainBPS       int32
}

// Refresh rebuilds each strategy's unsettled tail and returns rows written.
func (r *APYRefresher) Refresh(ctx context.Context, now time.Time) (int, error) {
	end := now.UTC().Truncate(time.Hour)
	retention := end.Add(-apyOutputSpan)
	written := 0
	for _, strategy := range r.strategies {
		var latest *time.Time
		if err := r.neon.QueryRow(ctx, `
            SELECT max(sample_hour) AS latest_sample_hour
            FROM loyal_yield.earn_apy_hourly_snapshots
            WHERE strategy = $1 AND risk_profile = $2 AND fee_bps = $3`, strategy.Name, strategy.RiskProfile, strategy.FeeBPS).Scan(&latest); err != nil {
			return written, fmt.Errorf("fetch latest Earn APY snapshot hour: %w", err)
		}
		outputStart := retention
		if latest != nil && latest.Add(-apyRefreshOverlap).After(retention) {
			outputStart = latest.Add(-apyRefreshOverlap)
		}
		if len(samplePoints(outputStart, end)) == 0 {
			continue
		}
		queryStart := outputStart.Add(-apyWindow)
		supported, err := r.supportedReserves(ctx, strategy)
		if err != nil {
			return written, err
		}
		rows, err := r.reserveUpdates(ctx, supported, queryStart, end)
		if err != nil {
			return written, err
		}
		snapshots := computeSnapshots(end, strategy.FeeBPS, outputStart, queryStart, rows, supported)
		if err := r.upsert(ctx, now, strategy, snapshots); err != nil {
			return written, err
		}
		written += len(snapshots)
	}
	return written, nil
}

func (r *APYRefresher) supportedReserves(ctx context.Context, strategy APYStrategy) (map[string]string, error) {
	rows, err := r.timescale.Query(ctx, `
            SELECT reserve, liquidity_mint
            FROM kamino.supported_reserves
            WHERE active = true
              AND market = ANY($1)
              AND liquidity_mint = ANY($2)
            ORDER BY market, liquidity_mint, reserve`, strategy.markets, apyStableMints)
	if err != nil {
		return nil, fmt.Errorf("fetch supported Kamino stable reserves: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var reserve, mint string
		if err := rows.Scan(&reserve, &mint); err != nil {
			return nil, err
		}
		out[reserve] = mint
	}
	return out, rows.Err()
}

func (r *APYRefresher) reserveUpdates(ctx context.Context, supported map[string]string, start, end time.Time) ([]reserveUpdate, error) {
	if len(supported) == 0 {
		return nil, nil
	}
	reserves := make([]string, 0, len(supported))
	for reserve := range supported {
		reserves = append(reserves, reserve)
	}
	sort.Strings(reserves)
	rows, err := r.timescale.Query(ctx, `
            WITH requested_reserves AS (
                SELECT unnest($1::text[]) AS reserve
            ),
            seed_rows AS (
                SELECT DISTINCT ON (ru.reserve)
                    $2::timestamptz AS observed_at,
                    ru.reserve,
                    ru.liquidity_mint,
                    ru.reserve_last_update_stale,
                    ru.total_supply_usd_estimate,
                    ru.supply_apy
                FROM kamino.reserve_updates ru
                JOIN requested_reserves rr ON rr.reserve = ru.reserve
                WHERE ru.observed_at < $2
                  AND ru.observed_at >= $6
                  AND ru.reserve_last_update_stale = false
                  AND ru.total_supply_usd_estimate > $4
                  AND ru.supply_apy >= 0
                  AND ru.supply_apy < $5
                ORDER BY ru.reserve, ru.observed_at DESC
            ),
            range_candidates AS (
                SELECT
                    date_bin('1 hour'::interval, ru.observed_at, $2::timestamptz) + '1 hour'::interval AS observed_at,
                    ru.observed_at AS raw_observed_at,
                    ru.reserve,
                    ru.liquidity_mint,
                    ru.reserve_last_update_stale,
                    ru.total_supply_usd_estimate,
                    ru.supply_apy,
                    row_number() OVER (
                        PARTITION BY ru.reserve, date_bin('1 hour'::interval, ru.observed_at, $2::timestamptz)
                        ORDER BY ru.observed_at DESC
                    ) AS row_number
                FROM kamino.reserve_updates ru
                JOIN requested_reserves rr ON rr.reserve = ru.reserve
                WHERE ru.observed_at >= $2
                  AND ru.observed_at <= $3
                  AND ru.reserve_last_update_stale = false
                  AND ru.total_supply_usd_estimate > $4
                  AND ru.supply_apy >= 0
                  AND ru.supply_apy < $5
            ),
            range_rows AS (
                SELECT observed_at, reserve, liquidity_mint, reserve_last_update_stale,
                       total_supply_usd_estimate, supply_apy
                FROM range_candidates
                WHERE row_number = 1
            )
            SELECT observed_at, reserve, liquidity_mint, reserve_last_update_stale,
                   total_supply_usd_estimate::float8, supply_apy::float8
            FROM (
                SELECT * FROM seed_rows
                UNION ALL
                SELECT * FROM range_rows
            ) rows
            ORDER BY observed_at ASC, reserve ASC`, reserves, start, end, apyMinTotalSupply, apyMaxSupply, start.Add(-apySeedLookback))
	if err != nil {
		return nil, fmt.Errorf("fetch Kamino reserve APY updates: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (reserveUpdate, error) {
		var update reserveUpdate
		err := row.Scan(&update.observedAt, &update.reserve, &update.mint, &update.stale, &update.totalSupply, &update.supply)
		return update, err
	})
}

func samplePoints(start, end time.Time) []time.Time {
	var points []time.Time
	for cursor := start.UTC().Truncate(time.Hour).Add(time.Hour); !cursor.After(end); cursor = cursor.Add(time.Hour) {
		points = append(points, cursor)
	}
	return points
}

type apySegment struct {
	start, end      time.Time
	loyal, mainUSDC float64
}

// computeSnapshots is compute_hourly_snapshots.
func computeSnapshots(end time.Time, feeBPS int16, outputStart, queryStart time.Time, rows []reserveUpdate, supported map[string]string) []apySnapshot {
	if !end.After(outputStart) {
		return nil
	}
	segments := apySegments(end, queryStart, rows, supported)
	var out []apySnapshot
	for _, hour := range samplePoints(outputStart, end) {
		start := hour.Add(-apyWindow)
		loyal := weightedAPY(segments, start, hour, func(s apySegment) float64 { return s.loyal }) - float64(max(feeBPS, 0))/10_000
		main := weightedAPY(segments, start, hour, func(s apySegment) float64 { return s.mainUSDC })
		out = append(out, apySnapshot{sampleHour: hour, windowStart: start, loyalBPS: ratioToBPS(loyal), mainBPS: ratioToBPS(main)})
	}
	return out
}

func apySegments(end, queryStart time.Time, input []reserveUpdate, supported map[string]string) []apySegment {
	rows := append([]reserveUpdate(nil), input...)
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].observedAt.Equal(rows[j].observedAt) {
			return rows[i].observedAt.Before(rows[j].observedAt)
		}
		return rows[i].reserve < rows[j].reserve
	})
	state := map[string]float64{}
	var main *float64
	apply := func(row reserveUpdate) {
		eligible := supported[row.reserve] == row.mint && row.mint != "" && !row.stale && row.totalSupply > apyMinTotalSupply &&
			row.supply >= 0 && row.supply < apyMaxSupply && !math.IsInf(row.supply, 0) && !math.IsNaN(row.supply)
		if eligible {
			state[row.reserve] = row.supply
		} else {
			delete(state, row.reserve)
		}
		if row.reserve == mainUSDCReserve {
			if eligible {
				value := row.supply
				main = &value
			} else {
				main = nil
			}
		}
	}
	selected := func() float64 {
		best, found := 0.0, false
		for _, value := range state {
			if !math.IsInf(value, 0) && !math.IsNaN(value) && value > 0 && (!found || value > best) {
				best, found = value, true
			}
		}
		return best
	}
	mainAPY := func() float64 {
		if main == nil {
			return 0
		}
		return *main
	}
	index := len(rows)
	for i, row := range rows {
		if row.observedAt.Before(queryStart) {
			apply(row)
		} else if index == len(rows) {
			index = i
		}
	}
	var segments []apySegment
	cursor := queryStart
	for cursor.Before(end) {
		next := end
		if index < len(rows) && rows[index].observedAt.Before(end) {
			next = rows[index].observedAt
		}
		if next.After(cursor) {
			segments = append(segments, apySegment{cursor, next, selected(), mainAPY()})
			cursor = next
		}
		for index < len(rows) && !rows[index].observedAt.After(cursor) {
			apply(rows[index])
			index++
		}
		if index >= len(rows) && cursor.Before(end) {
			segments = append(segments, apySegment{cursor, end, selected(), mainAPY()})
			break
		}
	}
	return segments
}

func weightedAPY(segments []apySegment, start, end time.Time, value func(apySegment) float64) float64 {
	if !end.After(start) {
		return 0
	}
	weighted, covered := 0.0, 0.0
	for _, segment := range segments {
		overlapStart, overlapEnd := segment.start, segment.end
		if start.After(overlapStart) {
			overlapStart = start
		}
		if end.Before(overlapEnd) {
			overlapEnd = end
		}
		if !overlapEnd.After(overlapStart) {
			continue
		}
		seconds := float64(max(overlapEnd.Sub(overlapStart).Milliseconds(), 0)) / 1000
		weighted += value(segment) * seconds
		covered += seconds
	}
	if covered <= 0 {
		return 0
	}
	return weighted / covered
}

func ratioToBPS(value float64) int32 {
	bps := math.Max(math.Round(value*10_000), 0)
	if bps > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(bps)
}

func (r *APYRefresher) upsert(ctx context.Context, generatedAt time.Time, strategy APYStrategy, snapshots []apySnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	metadata, err := json.Marshal(map[string]any{
		"metric":          "rolling_time_weighted_apy_bps",
		"loyal":           map[string]any{"strategy": strategy.Name, "riskProfile": strategy.RiskProfile, "feeBps": strategy.FeeBPS},
		"mainUsdcReserve": map[string]any{"reserve": mainUSDCReserve},
	})
	if err != nil {
		return err
	}
	batch := &pgx.Batch{}
	for _, snapshot := range snapshots {
		batch.Queue(`
        INSERT INTO loyal_yield.earn_apy_hourly_snapshots (
            strategy, risk_profile, fee_bps, sample_hour, window_started_at,
            window_ended_at, generated_at, loyal_apy_bps, main_usdc_reserve_apy_bps, metadata
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
        ON CONFLICT (strategy, risk_profile, fee_bps, sample_hour)
        DO UPDATE SET
            window_started_at = EXCLUDED.window_started_at,
            window_ended_at = EXCLUDED.window_ended_at,
            generated_at = EXCLUDED.generated_at,
            loyal_apy_bps = EXCLUDED.loyal_apy_bps,
            main_usdc_reserve_apy_bps = EXCLUDED.main_usdc_reserve_apy_bps,
            metadata = EXCLUDED.metadata`, strategy.Name, strategy.RiskProfile, strategy.FeeBPS, snapshot.sampleHour, snapshot.windowStart,
			snapshot.sampleHour, generatedAt, snapshot.loyalBPS, snapshot.mainBPS, metadata)
	}
	tx, err := r.neon.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("bulk upsert hourly Earn APY snapshots: %w", err)
	}
	return tx.Commit(ctx)
}
