use std::{collections::HashMap, str::FromStr, time::Duration as StdDuration};

use anyhow::{Context, Result};
use chrono::{DateTime, Duration, TimeZone, Utc};
use loyal_actions::{
    supported_yield_route_stable_mints, yield_route_universe_for_preset, KaminoStableRiskProfile,
    YieldRouteUniversePreset, KAMINO_MAIN_USDC_RESERVE,
};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sqlx::{
    postgres::{PgConnectOptions, PgPoolOptions},
    PgPool, Postgres, QueryBuilder, Row,
};

pub const EARN_APY_FEE_BPS: i16 = 1;
pub const EARN_APY_MEDIUM_STRATEGY: &str = "medium_fee_aware_1bps";
pub const EARN_APY_MEDIUM_RISK_PROFILE: &str = "medium";
pub const EARN_APY_SAFE_STRATEGY: &str = "safe_fee_aware_1bps";
pub const EARN_APY_SAFE_RISK_PROFILE: &str = "safe";

const DEFAULT_WINDOW_DAYS: i64 = 30;
const DEFAULT_OUTPUT_DAYS: i64 = 30;
const DEFAULT_MAX_SUPPLY_APY: f64 = 0.5;
const DEFAULT_MIN_TOTAL_SUPPLY_USD: f64 = 100_000.0;

#[derive(Debug, Clone)]
pub struct EarnApyRefreshConfig {
    pub window: Duration,
    pub output_span: Duration,
    pub max_supply_apy: f64,
    pub min_total_supply_usd: f64,
    pub strategies: Vec<EarnApyStrategy>,
}

impl Default for EarnApyRefreshConfig {
    fn default() -> Self {
        Self {
            window: Duration::days(DEFAULT_WINDOW_DAYS),
            output_span: Duration::days(DEFAULT_OUTPUT_DAYS),
            max_supply_apy: DEFAULT_MAX_SUPPLY_APY,
            min_total_supply_usd: DEFAULT_MIN_TOTAL_SUPPLY_USD,
            strategies: vec![earn_apy_safe_strategy()],
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnApyHourlySnapshot {
    pub sample_hour: DateTime<Utc>,
    pub window_started_at: DateTime<Utc>,
    pub window_ended_at: DateTime<Utc>,
    pub loyal_apy_bps: i32,
    pub main_usdc_reserve_apy_bps: i32,
}

#[derive(Debug, Clone)]
pub struct EarnApyRefreshOutcome {
    pub generated_at: DateTime<Utc>,
    pub inserted_or_updated: usize,
    pub first_sample_hour: Option<DateTime<Utc>>,
    pub last_sample_hour: Option<DateTime<Utc>>,
    pub profiles: usize,
}

#[derive(Debug, Clone)]
pub struct EarnApySnapshotRefresher {
    timescale_pool: PgPool,
    neon_pool: PgPool,
    config: EarnApyRefreshConfig,
}

impl EarnApySnapshotRefresher {
    pub async fn connect(
        timescale_url: &str,
        neon_url: &str,
        config: EarnApyRefreshConfig,
    ) -> Result<Self> {
        Ok(Self {
            timescale_pool: connect_pool(timescale_url)
                .await
                .context("connect Timescale for Earn APY snapshots")?,
            neon_pool: connect_pool(neon_url)
                .await
                .context("connect Yield Neon for Earn APY snapshots")?,
            config,
        })
    }

    pub async fn refresh(&self, now: DateTime<Utc>) -> Result<EarnApyRefreshOutcome> {
        ensure_earn_apy_hourly_snapshot_schema(&self.neon_pool).await?;

        let generated_at = now;
        let end = truncate_to_hour(now);
        let output_start = end - self.config.output_span;
        let query_start = output_start - self.config.window;
        let strategies = self.config.strategies.clone();
        let mut inserted_or_updated = 0;
        let mut first_sample_hour = None;
        let mut last_sample_hour = None;

        for strategy in &strategies {
            let supported_reserves = self.supported_reserves(strategy.risk_profile).await?;
            let rows = self
                .reserve_updates(&supported_reserves, query_start, end)
                .await?;
            let snapshots = compute_hourly_snapshots(HourlySnapshotInput {
                end,
                fee_bps: strategy.fee_bps,
                max_supply_apy: self.config.max_supply_apy,
                min_total_supply_usd: self.config.min_total_supply_usd,
                output_start,
                query_start,
                rows: &rows,
                supported_reserves: &supported_reserves,
                window: self.config.window,
            });

            inserted_or_updated +=
                upsert_hourly_snapshots(&self.neon_pool, generated_at, strategy, &snapshots)
                    .await?;
            first_sample_hour = first_sample_hour
                .or_else(|| snapshots.first().map(|snapshot| snapshot.sample_hour));
            if let Some(sample_hour) = snapshots.last().map(|snapshot| snapshot.sample_hour) {
                last_sample_hour = Some(sample_hour);
            }
        }

        Ok(EarnApyRefreshOutcome {
            generated_at,
            inserted_or_updated,
            first_sample_hour,
            last_sample_hour,
            profiles: strategies.len(),
        })
    }
}

async fn connect_pool(database_url: &str) -> Result<PgPool, sqlx::Error> {
    let options = PgConnectOptions::from_str(database_url)?.statement_cache_capacity(0);
    PgPoolOptions::new()
        .max_connections(2)
        .acquire_timeout(StdDuration::from_secs(5))
        .connect_with(options)
        .await
}

pub async fn ensure_earn_apy_hourly_snapshot_schema(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(
        r#"
        CREATE SCHEMA IF NOT EXISTS loyal_yield;

        CREATE TABLE IF NOT EXISTS loyal_yield.earn_apy_hourly_snapshots (
            id BIGSERIAL PRIMARY KEY,
            strategy TEXT NOT NULL,
            risk_profile TEXT NOT NULL,
            fee_bps SMALLINT NOT NULL,
            sample_hour TIMESTAMPTZ NOT NULL,
            window_started_at TIMESTAMPTZ NOT NULL,
            window_ended_at TIMESTAMPTZ NOT NULL,
            generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            loyal_apy_bps INTEGER NOT NULL,
            main_usdc_reserve_apy_bps INTEGER NOT NULL,
            metadata JSONB NOT NULL DEFAULT '{}'::jsonb
        );

        ALTER TABLE loyal_yield.earn_apy_hourly_snapshots
            ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

        CREATE UNIQUE INDEX IF NOT EXISTS earn_apy_hourly_snapshots_key_uidx
            ON loyal_yield.earn_apy_hourly_snapshots (
                strategy,
                risk_profile,
                fee_bps,
                sample_hour
            );

        CREATE INDEX IF NOT EXISTS earn_apy_hourly_snapshots_latest_idx
            ON loyal_yield.earn_apy_hourly_snapshots (
                strategy,
                risk_profile,
                fee_bps,
                sample_hour DESC
            );
        "#,
    )
    .execute(pool)
    .await
    .context("ensure loyal_yield.earn_apy_hourly_snapshots schema")?;

    Ok(())
}

impl EarnApySnapshotRefresher {
    async fn supported_reserves(
        &self,
        risk_profile: KaminoStableRiskProfile,
    ) -> Result<Vec<SupportedReserve>> {
        let universe =
            yield_route_universe_for_preset(YieldRouteUniversePreset::KaminoStable(risk_profile));
        let markets = universe
            .kamino_markets
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>();
        let liquidity_mints = supported_yield_route_stable_mints()
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>();
        let rows = sqlx::query(
            r#"
            SELECT reserve, liquidity_mint
            FROM kamino.supported_reserves
            WHERE active = true
              AND market = ANY($1)
              AND liquidity_mint = ANY($2)
            ORDER BY market, liquidity_mint, reserve
            "#,
        )
        .bind(markets)
        .bind(liquidity_mints)
        .fetch_all(&self.timescale_pool)
        .await
        .context("fetch supported Kamino stable reserves")?;

        Ok(rows
            .into_iter()
            .map(|row| SupportedReserve {
                reserve: row.get("reserve"),
                liquidity_mint: row.get("liquidity_mint"),
            })
            .collect())
    }

    async fn reserve_updates(
        &self,
        supported_reserves: &[SupportedReserve],
        start: DateTime<Utc>,
        end: DateTime<Utc>,
    ) -> Result<Vec<ReserveUpdate>> {
        let reserves = supported_reserves
            .iter()
            .map(|reserve| reserve.reserve.clone())
            .collect::<Vec<_>>();
        if reserves.is_empty() {
            return Ok(Vec::new());
        }

        let rows = sqlx::query(
            r#"
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
                  AND ru.reserve_last_update_stale = false
                  AND ru.total_supply_usd_estimate > $4
                  AND ru.supply_apy >= 0
                  AND ru.supply_apy < $5
                ORDER BY ru.reserve, ru.observed_at DESC
            ),
            range_candidates AS (
                SELECT
                    date_bin(
                        '1 hour'::interval,
                        ru.observed_at,
                        $2::timestamptz
                    ) + '1 hour'::interval AS observed_at,
                    ru.observed_at AS raw_observed_at,
                    ru.reserve,
                    ru.liquidity_mint,
                    ru.reserve_last_update_stale,
                    ru.total_supply_usd_estimate,
                    ru.supply_apy,
                    row_number() OVER (
                        PARTITION BY
                            ru.reserve,
                            date_bin(
                                '1 hour'::interval,
                                ru.observed_at,
                                $2::timestamptz
                            )
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
                SELECT
                    observed_at,
                    reserve,
                    liquidity_mint,
                    reserve_last_update_stale,
                    total_supply_usd_estimate,
                    supply_apy
                FROM range_candidates
                WHERE row_number = 1
            )
            SELECT *
            FROM (
                SELECT * FROM seed_rows
                UNION ALL
                SELECT * FROM range_rows
            ) rows
            ORDER BY observed_at ASC, reserve ASC
            "#,
        )
        .bind(reserves)
        .bind(start)
        .bind(end)
        .bind(self.config.min_total_supply_usd)
        .bind(self.config.max_supply_apy)
        .fetch_all(&self.timescale_pool)
        .await
        .context("fetch Kamino reserve APY updates")?;

        Ok(rows
            .into_iter()
            .map(|row| ReserveUpdate {
                liquidity_mint: row.get("liquidity_mint"),
                observed_at: row.get("observed_at"),
                reserve: row.get("reserve"),
                reserve_last_update_stale: row.get("reserve_last_update_stale"),
                supply_apy: row.get("supply_apy"),
                total_supply_usd_estimate: row.get("total_supply_usd_estimate"),
            })
            .collect())
    }
}

async fn upsert_hourly_snapshots(
    pool: &PgPool,
    generated_at: DateTime<Utc>,
    strategy: &EarnApyStrategy,
    snapshots: &[EarnApyHourlySnapshot],
) -> Result<usize> {
    if snapshots.is_empty() {
        return Ok(0);
    }

    let metadata = json!({
        "metric": "rolling_time_weighted_apy_bps",
        "loyal": {
            "strategy": strategy.name,
            "riskProfile": strategy.risk_profile_label,
            "feeBps": strategy.fee_bps
        },
        "mainUsdcReserve": {
            "reserve": KAMINO_MAIN_USDC_RESERVE.to_string()
        }
    });
    let mut builder = QueryBuilder::<Postgres>::new(
        r#"
        INSERT INTO loyal_yield.earn_apy_hourly_snapshots (
            strategy,
            risk_profile,
            fee_bps,
            sample_hour,
            window_started_at,
            window_ended_at,
            generated_at,
            loyal_apy_bps,
            main_usdc_reserve_apy_bps,
            metadata
        )
        "#,
    );

    builder.push_values(snapshots, |mut row, snapshot| {
        row.push_bind(strategy.name)
            .push_bind(strategy.risk_profile_label)
            .push_bind(strategy.fee_bps)
            .push_bind(snapshot.sample_hour)
            .push_bind(snapshot.window_started_at)
            .push_bind(snapshot.window_ended_at)
            .push_bind(generated_at)
            .push_bind(snapshot.loyal_apy_bps)
            .push_bind(snapshot.main_usdc_reserve_apy_bps)
            .push_bind(metadata.clone());
    });

    builder.push(
        r#"
        ON CONFLICT (strategy, risk_profile, fee_bps, sample_hour)
        DO UPDATE SET
            window_started_at = EXCLUDED.window_started_at,
            window_ended_at = EXCLUDED.window_ended_at,
            generated_at = EXCLUDED.generated_at,
            loyal_apy_bps = EXCLUDED.loyal_apy_bps,
            main_usdc_reserve_apy_bps = EXCLUDED.main_usdc_reserve_apy_bps,
            metadata = EXCLUDED.metadata
        "#,
    );

    builder
        .build()
        .execute(pool)
        .await
        .context("bulk upsert hourly Earn APY snapshots")?;

    Ok(snapshots.len())
}

#[derive(Debug, Clone, PartialEq)]
pub struct SupportedReserve {
    pub reserve: String,
    pub liquidity_mint: String,
}

#[derive(Debug, Clone, PartialEq)]
pub struct ReserveUpdate {
    pub observed_at: DateTime<Utc>,
    pub reserve: String,
    pub liquidity_mint: String,
    pub reserve_last_update_stale: bool,
    pub total_supply_usd_estimate: f64,
    pub supply_apy: f64,
}

#[derive(Debug, Clone, Copy)]
struct ReserveState {
    supply_apy: f64,
}

#[derive(Debug, Clone, Copy)]
struct ApySegment {
    start: DateTime<Utc>,
    end: DateTime<Utc>,
    loyal_apy: f64,
    main_usdc_apy: f64,
}

pub struct HourlySnapshotInput<'a> {
    pub end: DateTime<Utc>,
    pub fee_bps: i16,
    pub max_supply_apy: f64,
    pub min_total_supply_usd: f64,
    pub output_start: DateTime<Utc>,
    pub query_start: DateTime<Utc>,
    pub rows: &'a [ReserveUpdate],
    pub supported_reserves: &'a [SupportedReserve],
    pub window: Duration,
}

pub fn compute_hourly_snapshots(input: HourlySnapshotInput<'_>) -> Vec<EarnApyHourlySnapshot> {
    if input.end <= input.output_start || input.window <= Duration::zero() {
        return Vec::new();
    }

    let segments = build_apy_segments(&input);
    hourly_sample_points(input.output_start, input.end)
        .into_iter()
        .map(|sample_hour| {
            let window_started_at = sample_hour - input.window;
            EarnApyHourlySnapshot {
                sample_hour,
                window_started_at,
                window_ended_at: sample_hour,
                loyal_apy_bps: ratio_to_bps(
                    weighted_average_apy(&segments, window_started_at, sample_hour, |segment| {
                        segment.loyal_apy
                    }) - fee_bps_to_ratio(input.fee_bps),
                ),
                main_usdc_reserve_apy_bps: ratio_to_bps(weighted_average_apy(
                    &segments,
                    window_started_at,
                    sample_hour,
                    |segment| segment.main_usdc_apy,
                )),
            }
        })
        .collect()
}

fn build_apy_segments(input: &HourlySnapshotInput<'_>) -> Vec<ApySegment> {
    let supported = input
        .supported_reserves
        .iter()
        .map(|reserve| (reserve.reserve.as_str(), reserve.liquidity_mint.as_str()))
        .collect::<HashMap<_, _>>();
    let mut rows = input.rows.to_vec();
    rows.sort_by(|left, right| {
        left.observed_at
            .cmp(&right.observed_at)
            .then_with(|| left.reserve.cmp(&right.reserve))
    });

    let mut state = HashMap::<String, ReserveState>::new();
    let mut main_usdc_state = None::<ReserveState>;

    for row in rows
        .iter()
        .filter(|row| row.observed_at < input.query_start)
    {
        update_state(
            &mut state,
            &mut main_usdc_state,
            &supported,
            row,
            input.min_total_supply_usd,
            input.max_supply_apy,
        );
    }

    let mut segments = Vec::new();
    let mut cursor = input.query_start;
    let mut index = rows
        .iter()
        .position(|row| row.observed_at >= input.query_start)
        .unwrap_or(rows.len());

    while cursor < input.end {
        let next_observed_at = rows
            .get(index)
            .map(|row| row.observed_at)
            .unwrap_or(input.end)
            .min(input.end);

        if next_observed_at > cursor {
            segments.push(ApySegment {
                start: cursor,
                end: next_observed_at,
                loyal_apy: selected_strategy_apy(&state),
                main_usdc_apy: main_usdc_state.map(|state| state.supply_apy).unwrap_or(0.0),
            });
            cursor = next_observed_at;
        }

        while index < rows.len() && rows[index].observed_at <= cursor {
            update_state(
                &mut state,
                &mut main_usdc_state,
                &supported,
                &rows[index],
                input.min_total_supply_usd,
                input.max_supply_apy,
            );
            index += 1;
        }

        if index >= rows.len() && cursor < input.end {
            segments.push(ApySegment {
                start: cursor,
                end: input.end,
                loyal_apy: selected_strategy_apy(&state),
                main_usdc_apy: main_usdc_state.map(|state| state.supply_apy).unwrap_or(0.0),
            });
            break;
        }
    }

    segments
}

fn update_state(
    state: &mut HashMap<String, ReserveState>,
    main_usdc_state: &mut Option<ReserveState>,
    supported: &HashMap<&str, &str>,
    row: &ReserveUpdate,
    min_total_supply_usd: f64,
    max_supply_apy: f64,
) {
    let expected_mint = supported.get(row.reserve.as_str()).copied();
    let is_supported = expected_mint == Some(row.liquidity_mint.as_str());
    let is_eligible = is_supported
        && !row.reserve_last_update_stale
        && row.total_supply_usd_estimate > min_total_supply_usd
        && row.supply_apy >= 0.0
        && row.supply_apy < max_supply_apy
        && row.supply_apy.is_finite();

    if is_eligible {
        state.insert(
            row.reserve.clone(),
            ReserveState {
                supply_apy: row.supply_apy,
            },
        );
    } else {
        state.remove(&row.reserve);
    }

    if row.reserve == KAMINO_MAIN_USDC_RESERVE.to_string() {
        *main_usdc_state = if is_eligible {
            Some(ReserveState {
                supply_apy: row.supply_apy,
            })
        } else {
            None
        };
    }
}

fn selected_strategy_apy(state: &HashMap<String, ReserveState>) -> f64 {
    state
        .values()
        .map(|state| state.supply_apy)
        .filter(|apy| apy.is_finite() && *apy > 0.0)
        .max_by(f64::total_cmp)
        .unwrap_or(0.0)
}

fn weighted_average_apy(
    segments: &[ApySegment],
    start: DateTime<Utc>,
    end: DateTime<Utc>,
    apy: impl Fn(&ApySegment) -> f64,
) -> f64 {
    if end <= start {
        return 0.0;
    }

    let mut weighted = 0.0;
    let mut covered_seconds = 0.0;
    for segment in segments {
        let overlap_start = segment.start.max(start);
        let overlap_end = segment.end.min(end);
        if overlap_end <= overlap_start {
            continue;
        }
        let seconds = (overlap_end - overlap_start).num_milliseconds().max(0) as f64 / 1000.0;
        weighted += apy(segment) * seconds;
        covered_seconds += seconds;
    }

    if covered_seconds <= 0.0 {
        0.0
    } else {
        weighted / covered_seconds
    }
}

fn hourly_sample_points(start: DateTime<Utc>, end: DateTime<Utc>) -> Vec<DateTime<Utc>> {
    let mut cursor = truncate_to_hour(start) + Duration::hours(1);
    let mut points = Vec::new();
    while cursor <= end {
        points.push(cursor);
        cursor += Duration::hours(1);
    }
    points
}

fn truncate_to_hour(value: DateTime<Utc>) -> DateTime<Utc> {
    Utc.timestamp_opt((value.timestamp() / 3600) * 3600, 0)
        .single()
        .expect("valid UTC hour timestamp")
}

fn ratio_to_bps(value: f64) -> i32 {
    (value * 10_000.0).round().max(0.0) as i32
}

fn fee_bps_to_ratio(fee_bps: i16) -> f64 {
    f64::from(fee_bps.max(0)) / 10_000.0
}

#[derive(Debug, Clone, Copy)]
pub struct EarnApyStrategy {
    pub name: &'static str,
    pub risk_profile_label: &'static str,
    pub risk_profile: KaminoStableRiskProfile,
    pub fee_bps: i16,
}

pub fn earn_apy_safe_strategy() -> EarnApyStrategy {
    EarnApyStrategy {
        name: EARN_APY_SAFE_STRATEGY,
        risk_profile_label: EARN_APY_SAFE_RISK_PROFILE,
        risk_profile: KaminoStableRiskProfile::Safe,
        fee_bps: EARN_APY_FEE_BPS,
    }
}

pub fn earn_apy_medium_strategy() -> EarnApyStrategy {
    EarnApyStrategy {
        name: EARN_APY_MEDIUM_STRATEGY,
        risk_profile_label: EARN_APY_MEDIUM_RISK_PROFILE,
        risk_profile: KaminoStableRiskProfile::Medium,
        fee_bps: EARN_APY_FEE_BPS,
    }
}

pub fn earn_apy_strategies() -> [EarnApyStrategy; 2] {
    [earn_apy_safe_strategy(), earn_apy_medium_strategy()]
}

pub fn earn_apy_strategy_for_risk_profile(value: &str) -> Option<EarnApyStrategy> {
    match value.trim().to_ascii_lowercase().as_str() {
        EARN_APY_SAFE_RISK_PROFILE => Some(earn_apy_safe_strategy()),
        EARN_APY_MEDIUM_RISK_PROFILE => Some(earn_apy_medium_strategy()),
        _ => None,
    }
}
