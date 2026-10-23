package webapp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Timescale Kamino reserve feed reads behind the Earn deposit-target
// selection. Ports web/apps/web/src/lib/kamino/timescale-reserve-client.server.ts:
// currentConfirmedReserves (:353-417), getCurrentEligibleSafeReserves
// (:734-779), getLatestReserveObservationsByReserve (:685-718) and
// isRecentChainFrontier (:327-351). A nil dtargetQuerier is the unconfigured
// TIMESCALEDB_URL feed (getTimescaleReserveDatabaseUrl :225-227).

const (
	// dtargetReserveStreamMaxAgeMS bounds both receipt age and the frontier's
	// actual chain age; the observer timestamps replays when received
	// (timescale-reserve-client.server.ts:162-165).
	dtargetReserveStreamMaxAgeMS = 240_000
	// DEFAULT_MIN_DELIBERATE_VENUE_TOTAL_SUPPLY_USD_ESTIMATE
	// (timescale-reserve-client.server.ts:162).
	dtargetMinVenueTotalSupplyUSD = 100_000.0
	// DEFAULT_MAX_SUPPLY_APY (web/apps/web/src/lib/kamino/max-supply-apy.ts:1).
	dtargetMaxSupplyApy = 0.5
	// dtargetFrontierBlockTimeTimeout is the RPC fetch timeout the web client
	// puts on its reserve block-time connection
	// (timescale-reserve-client.server.ts:169-184).
	dtargetFrontierBlockTimeTimeout = 5 * time.Second
)

// dtargetQuerier is the read surface the deposit-target SQL needs; a
// *pgxpool.Pool or pgx.Tx satisfies it, and tests inject fixtures.
type dtargetQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// dtargetBlockTimeFunc mirrors Connection.getBlockTime: unix seconds, ok=false
// for the RPC null result, err for a failed call.
type dtargetBlockTimeFunc func(ctx context.Context, slot int64) (unixSeconds int64, ok bool, err error)

type dtargetReserveRow struct {
	Reserve       string
	Market        *string
	LiquidityMint string
	SupplyApy     float64
}

type dtargetReserveObservation struct {
	ObservedAt             time.Time
	Reserve                string
	TotalSupplyUsdEstimate float64
}

// dtargetConfirmedCurrentReservesSQL is the shared CTE pair joining the
// observer's current state to its update by the complete observation identity,
// including observed_at for Timescale chunk pruning, and the active supported
// reserve by (reserve, market, liquidity mint)
// (timescale-reserve-client.server.ts:353-395).
const dtargetConfirmedCurrentReservesSQL = `
confirmed_current_reserves AS (
	SELECT updates.*
	FROM kamino.reserve_current_states AS states
	JOIN kamino.reserve_updates AS updates
	  ON updates.reserve = states.reserve
	 AND updates.observed_at = states.state_observed_at
	 AND updates.event_id = states.state_event_id
	 AND updates.slot = states.state_slot
	 AND updates.account_data_hash = states.account_data_hash
	 AND updates.source = states.state_source
	JOIN kamino.supported_reserves AS supported
	  ON supported.reserve = updates.reserve
	 AND supported.market = updates.market
	 AND supported.liquidity_mint = updates.liquidity_mint
	 AND supported.active = true
	WHERE updates.source_commitment = 'confirmed'
	  AND updates.source IN ('laserstream_grpc', 'http_snapshot', 'http_confirmed_refresh')
),
reserve_stream_frontier AS (
	SELECT observed_at, slot
	FROM confirmed_current_reserves
	ORDER BY slot DESC, observed_at DESC
	LIMIT 1
)`

// dtargetFrontierFreshnessSQL keeps only observations at or behind the
// statement-clock frontier inside the max stream age
// (timescale-reserve-client.server.ts:396-415).
const dtargetFrontierFreshnessSQL = `
		JOIN reserve_stream_frontier AS frontier
		  ON frontier.observed_at >= statement_timestamp() - $1::bigint * interval '1 millisecond'
		 AND frontier.observed_at <= statement_timestamp()
		 AND latest.observed_at <= statement_timestamp()`

// dtargetCurrentEligibleSafeReserves returns the highest-APY candidate rows
// feeding findBestSafeEarnReserveTarget, before the per-stablecoin and
// product-mint picks (getCurrentEligibleSafeReserves :734-779).
func dtargetCurrentEligibleSafeReserves(ctx context.Context, q dtargetQuerier, getBlockTime dtargetBlockTimeFunc, safeMarkets, stableMints []string) ([]dtargetReserveRow, error) {
	if q == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := q.Query(ctx, `
		WITH `+dtargetConfirmedCurrentReservesSQL+`
		SELECT latest.reserve, latest.market, latest.liquidity_mint,
		       latest.supply_apy::float8, frontier.slot
		FROM confirmed_current_reserves AS latest
`+dtargetFrontierFreshnessSQL+`
		WHERE latest.total_supply_usd_estimate > $2::float8
		  AND latest.supply_apy >= 0
		  AND latest.supply_apy < $3::float8
		  AND latest.market = ANY($4::text[])
		  AND latest.liquidity_mint = ANY($5::text[])
		ORDER BY latest.supply_apy DESC`,
		dtargetReserveStreamMaxAgeMS, dtargetMinVenueTotalSupplyUSD, dtargetMaxSupplyApy, safeMarkets, stableMints)
	if err != nil {
		return nil, fmt.Errorf("fetch eligible Safe Kamino reserves: %w", err)
	}
	defer rows.Close()
	var out []dtargetReserveRow
	var frontierSlot *int64
	for rows.Next() {
		var row dtargetReserveRow
		var slot int64
		if err := rows.Scan(&row.Reserve, &row.Market, &row.LiquidityMint, &row.SupplyApy, &slot); err != nil {
			return nil, err
		}
		if frontierSlot == nil {
			frontierSlot = &slot
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !dtargetRecentChainFrontier(ctx, getBlockTime, frontierSlot) {
		return nil, nil
	}
	return out, nil
}

// dtargetLatestReserveObservations reports whether the observer's active
// current state samples each requested reserve. Deliberately no stale-flag or
// APY-bounds filters: presence is the eligibility signal, and a transiently
// stale row must not fail deposits (getLatestReserveObservationsByReserve
// :685-718 and its comment :694-698).
func dtargetLatestReserveObservations(ctx context.Context, q dtargetQuerier, getBlockTime dtargetBlockTimeFunc, reserves []string) ([]dtargetReserveObservation, error) {
	if q == nil || len(reserves) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := q.Query(ctx, `
		WITH `+dtargetConfirmedCurrentReservesSQL+`
		SELECT latest.observed_at, latest.reserve,
		       latest.total_supply_usd_estimate::float8, frontier.slot
		FROM confirmed_current_reserves AS latest
`+dtargetFrontierFreshnessSQL+`
		WHERE latest.reserve = ANY($2::text[])
		ORDER BY latest.reserve ASC`,
		dtargetReserveStreamMaxAgeMS, reserves)
	if err != nil {
		return nil, fmt.Errorf("fetch latest Kamino reserve observations: %w", err)
	}
	defer rows.Close()
	var out []dtargetReserveObservation
	var frontierSlot *int64
	for rows.Next() {
		var observation dtargetReserveObservation
		var slot int64
		if err := rows.Scan(&observation.ObservedAt, &observation.Reserve, &observation.TotalSupplyUsdEstimate, &slot); err != nil {
			return nil, err
		}
		if frontierSlot == nil {
			frontierSlot = &slot
		}
		out = append(out, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !dtargetRecentChainFrontier(ctx, getBlockTime, frontierSlot) {
		return nil, nil
	}
	return out, nil
}

// dtargetRecentChainFrontier mirrors isRecentChainFrontier
// (timescale-reserve-client.server.ts:327-351): a positive slot whose block
// time is between 0 and the max stream age old proves the feed caught up to
// the chain; a failed or null block time is not recent.
func dtargetRecentChainFrontier(ctx context.Context, getBlockTime dtargetBlockTimeFunc, slot *int64) bool {
	if slot == nil || *slot <= 0 {
		return false
	}
	rpcCtx, cancel := context.WithTimeout(ctx, dtargetFrontierBlockTimeTimeout)
	defer cancel()
	blockTime, ok, err := getBlockTime(rpcCtx, *slot)
	if err != nil {
		slog.Error("[kamino-reserves] stream frontier block time unavailable", "slot", *slot)
		return false
	}
	if !ok || blockTime <= 0 {
		return false
	}
	age := time.Since(time.Unix(blockTime, 0))
	return age >= 0 && age <= dtargetReserveStreamMaxAgeMS*time.Millisecond
}
