package webapp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Timescale APY reader behind loadApySamples, ported from
// kamino/timescale-reserve-client.server.ts getReserveApyHistorySamplesForReserves
// (:481). One bucketed query per load; the pool lives for the call, like the
// TS client that opens and closes around the request.

// earningsDefaultMaxSupplyApy is DEFAULT_MAX_SUPPLY_APY (max-supply-apy.ts:1):
// rows at or above it are curated-API sentinels, not real yields.
const earningsDefaultMaxSupplyApy = 0.5

const earningsApyQuery = `WITH requested_reserves AS (
  SELECT unnest($1::text[]) AS reserve
),
previous_samples AS (
  SELECT r.reserve, sample.observed_at, sample.supply_apy
  FROM requested_reserves r
  CROSS JOIN LATERAL (
    SELECT observed_at, supply_apy
    FROM (
      (
        SELECT observed_at, supply_apy
        FROM kamino.reserve_updates
        WHERE reserve = r.reserve
          AND reserve_last_update_stale = false
          AND supply_apy >= 0
          AND supply_apy < $4
          AND observed_at < $2::timestamptz
        ORDER BY observed_at DESC
        LIMIT 1
      )
      UNION ALL
      (
        SELECT observed_at, supply_apy
        FROM kamino.reserve_apy_backfill
        WHERE reserve = r.reserve
          AND supply_apy >= 0
          AND supply_apy < $4
          AND observed_at < $2::timestamptz
        ORDER BY observed_at DESC
        LIMIT 1
      )
    ) candidates
    ORDER BY observed_at DESC
    LIMIT 1
  ) sample
),
latest_samples AS (
  SELECT r.reserve, sample.observed_at, sample.supply_apy
  FROM requested_reserves r
  CROSS JOIN LATERAL (
    SELECT observed_at, supply_apy
    FROM (
      (
        SELECT observed_at, supply_apy
        FROM kamino.reserve_updates
        WHERE reserve = r.reserve
          AND reserve_last_update_stale = false
          AND supply_apy >= 0
          AND supply_apy < $4
          AND observed_at <= $3::timestamptz
        ORDER BY observed_at DESC
        LIMIT 1
      )
      UNION ALL
      (
        SELECT observed_at, supply_apy
        FROM kamino.reserve_apy_backfill
        WHERE reserve = r.reserve
          AND supply_apy >= 0
          AND supply_apy < $4
          AND observed_at <= $3::timestamptz
        ORDER BY observed_at DESC
        LIMIT 1
      )
    ) candidates
    ORDER BY observed_at DESC
    LIMIT 1
  ) sample
),
range_samples AS (
  SELECT r.reserve, sample.observed_at, sample.supply_apy
  FROM requested_reserves r
  CROSS JOIN generate_series(
    $2::timestamptz,
    $3::timestamptz,
    make_interval(secs => $5)
  ) AS bucket(bucket_start)
  CROSS JOIN LATERAL (
    SELECT observed_at, supply_apy
    FROM (
      (
        SELECT observed_at, supply_apy
        FROM kamino.reserve_updates
        WHERE reserve = r.reserve
          AND reserve_last_update_stale = false
          AND supply_apy >= 0
          AND supply_apy < $4
          AND observed_at >= bucket.bucket_start
          AND observed_at < bucket.bucket_start + make_interval(secs => $5)
          AND observed_at <= $3::timestamptz
        ORDER BY observed_at DESC
        LIMIT 1
      )
      UNION ALL
      (
        SELECT observed_at, supply_apy
        FROM kamino.reserve_apy_backfill
        WHERE reserve = r.reserve
          AND supply_apy >= 0
          AND supply_apy < $4
          AND observed_at >= bucket.bucket_start
          AND observed_at < bucket.bucket_start + make_interval(secs => $5)
          AND observed_at <= $3::timestamptz
        ORDER BY observed_at DESC
        LIMIT 1
      )
    ) candidates
    ORDER BY observed_at DESC
    LIMIT 1
  ) sample
)
SELECT reserve, observed_at, supply_apy
FROM (
  SELECT reserve, observed_at, supply_apy FROM previous_samples
  UNION
  SELECT reserve, observed_at, supply_apy FROM range_samples
  UNION
  SELECT reserve, observed_at, supply_apy FROM latest_samples
) samples
ORDER BY observed_at ASC, reserve ASC`

// earningsApyReserves ports loadApySamples' reserve union (read-service:766):
// the call-site reserve list plus every reserve a positive path principal or
// kamino snapshot exposure touches, deduplicated and sorted.
func earningsApyReserves(args earningsApyArgs) []string {
	seen := map[string]bool{}
	var reserves []string
	add := func(reserve string) {
		if reserve == "" || seen[reserve] {
			return
		}
		seen[reserve] = true
		reserves = append(reserves, reserve)
	}
	for _, reserve := range args.reserves {
		add(reserve)
	}
	for _, event := range args.pathEvents {
		if event.principalAmountRaw > 0 {
			add(event.reserve)
		}
	}
	for _, snapshot := range args.portfolioSnapshots {
		for _, exposure := range snapshot.exposures {
			if exposure.kind == "kamino" && exposure.amountRaw > 0 && exposure.reserve != nil {
				add(*exposure.reserve)
			}
		}
	}
	sort.Strings(reserves)
	return reserves
}

// newEarningsApyLoader returns the read-service loadApySamples dependency.
// TIMESCALEDB_URL names the timescale pool; without it the loader fails the
// way getTimescaleReserveDatabaseUrl does.
func newEarningsApyLoader() func(context.Context, earningsReadInput, earningsApyArgs) ([]reserveApySample, error) {
	return func(ctx context.Context, _ earningsReadInput, args earningsApyArgs) ([]reserveApySample, error) {
		reserves := earningsApyReserves(args)
		if len(reserves) == 0 {
			return []reserveApySample{}, nil
		}
		databaseURL := os.Getenv("TIMESCALEDB_URL")
		if databaseURL == "" {
			return nil, fmt.Errorf("missing_timescale_database_url")
		}
		poolCfg, err := pgxpool.ParseConfig(databaseURL)
		if err != nil {
			return nil, err
		}
		poolCfg.MaxConns = 1
		pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
		if err != nil {
			return nil, err
		}
		defer pool.Close()

		const sampleIntervalSeconds = 24 * 60 * 60
		rows, err := pool.Query(ctx, earningsApyQuery,
			reserves, args.start, args.end, earningsDefaultMaxSupplyApy, sampleIntervalSeconds)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		samples := []reserveApySample{}
		for rows.Next() {
			var sample reserveApySample
			var reserve string
			var observedAt time.Time
			if err := rows.Scan(&reserve, &observedAt, &sample.supplyApy); err != nil {
				return nil, err
			}
			sample.observedAt = msTime(observedAt)
			sample.reserve = &reserve
			samples = append(samples, sample)
		}
		return samples, rows.Err()
	}
}
