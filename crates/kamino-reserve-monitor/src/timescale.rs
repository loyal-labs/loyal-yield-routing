use std::{str::FromStr, time::Duration};

use anyhow::{bail, Context, Result};
use serde::Serialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::pubkey::Pubkey;
use sqlx::{
    postgres::{PgPoolOptions, PgRow},
    FromRow, PgPool, Row,
};

use crate::{
    apy::ReserveSnapshot,
    targets::{ReserveTarget, SupportedReserveRecord},
    ReserveDiff,
};

const TABLE_NAME: &str = "reserve_updates";

#[derive(Debug, Clone)]
pub struct TimescaleSinkConfig {
    pub url: String,
    pub schema: String,
    pub max_connections: u32,
    pub acquire_timeout: Duration,
}

impl TimescaleSinkConfig {
    pub fn new(url: impl Into<String>) -> Self {
        Self {
            url: url.into(),
            schema: "kamino".to_string(),
            max_connections: 2,
            acquire_timeout: Duration::from_secs(5),
        }
    }
}

#[derive(Clone)]
pub struct TimescaleSink {
    pool: PgPool,
    insert_sql: String,
    insert_batch_sql: String,
    dedupe_lookup_sql: String,
    load_supported_targets_sql: String,
    load_supported_targets_filtered_sql: String,
    deactivate_supported_reserves_sql: String,
    upsert_supported_reserve_sql: String,
}

#[derive(Debug, Serialize)]
pub struct ReserveUpdateRecord<'a> {
    pub kind: &'static str,
    pub source: &'static str,
    pub observed_at: chrono::DateTime<chrono::Utc>,
    pub slot: u64,
    pub target: &'a ReserveTarget,
    pub snapshot: &'a ReserveSnapshot,
    pub diff_summary: &'a str,
    pub diff: Option<&'a ReserveDiff>,
    pub raw_account_data_base64: Option<&'a str>,
    pub source_commitment: &'a str,
    pub account_data_hash: &'a str,
    pub received_at: chrono::DateTime<chrono::Utc>,
    pub decoded_at: chrono::DateTime<chrono::Utc>,
    pub receive_to_decode_ms: u128,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TimescaleInsertOutcome {
    pub event_id: i64,
    pub inserted: bool,
}

impl TimescaleSink {
    pub async fn connect(config: TimescaleSinkConfig) -> Result<Self> {
        validate_identifier(&config.schema, "--timescaledb-schema")?;
        let pool = PgPoolOptions::new()
            .max_connections(config.max_connections)
            .acquire_timeout(config.acquire_timeout)
            .connect(&config.url)
            .await
            .context("connect TimescaleDB pool")?;

        Ok(Self {
            pool,
            insert_sql: insert_sql(&config.schema),
            insert_batch_sql: insert_batch_sql(&config.schema),
            dedupe_lookup_sql: dedupe_lookup_sql(&config.schema),
            load_supported_targets_sql: load_supported_targets_sql(&config.schema, false),
            load_supported_targets_filtered_sql: load_supported_targets_sql(&config.schema, true),
            deactivate_supported_reserves_sql: deactivate_supported_reserves_sql(&config.schema),
            upsert_supported_reserve_sql: upsert_supported_reserve_sql(&config.schema),
        })
    }

    pub fn pool(&self) -> PgPool {
        self.pool.clone()
    }

    pub fn account_data_hash(data: &[u8]) -> String {
        let digest = Sha256::digest(data);
        hex_encode(&digest)
    }

    pub fn dedupe_key(record: &ReserveUpdateRecord<'_>) -> String {
        Self::dedupe_key_parts(
            record.source_commitment,
            &record.snapshot.reserve.to_string(),
            record.slot,
            record.account_data_hash,
        )
    }

    pub fn dedupe_key_parts(
        source_commitment: &str,
        reserve: &str,
        slot: u64,
        account_data_hash: &str,
    ) -> String {
        format!("{source_commitment}:{reserve}:{slot}:{account_data_hash}")
    }

    pub async fn insert(&self, record: &ReserveUpdateRecord<'_>) -> Result<TimescaleInsertOutcome> {
        let target_json = serde_json::to_value(record.target).context("serialize target JSON")?;
        let snapshot_json =
            serde_json::to_value(record.snapshot).context("serialize snapshot JSON")?;
        let diff_json = record
            .diff
            .map(serde_json::to_value)
            .transpose()
            .context("serialize diff JSON")?
            .unwrap_or_else(|| json!({}));
        let record_json =
            serde_json::to_value(record).context("serialize full reserve update JSON")?;
        let slot = i64_from_u64(record.slot, "slot")?;
        let reserve_last_update_slot = i64_from_u64(
            record.snapshot.reserve_last_update_slot,
            "reserve_last_update_slot",
        )?;
        let market_price_last_updated_ts = i64_from_u64(
            record.snapshot.market_price_last_updated_ts,
            "market_price_last_updated_ts",
        )?;
        let mint_decimals = i32_from_u64(record.snapshot.mint_decimals, "mint_decimals")?;
        let cumulative_borrow_rate_bsf = record
            .snapshot
            .cumulative_borrow_rate_bsf
            .iter()
            .map(u64::to_string)
            .collect::<Vec<_>>()
            .join(":");
        let changed_fields = record
            .diff
            .map(|diff| {
                diff.changed_fields
                    .iter()
                    .map(|field| (*field).to_string())
                    .collect::<Vec<_>>()
            })
            .unwrap_or_default();
        let dedupe_key = Self::dedupe_key(record);
        let decode_to_insert_ms = chrono::Utc::now()
            .signed_duration_since(record.decoded_at)
            .num_milliseconds()
            .max(0);
        let receive_to_decode_ms =
            i64_from_u128(record.receive_to_decode_ms, "receive_to_decode_ms")?;

        let inserted_event_id = sqlx::query_scalar::<_, i64>(&self.insert_sql)
            .bind(dedupe_key.clone())
            .bind(record.snapshot.reserve.to_string())
            .bind(slot)
            .bind(record.account_data_hash)
            .bind(record.observed_at)
            .bind(record.kind)
            .bind(record.source)
            .bind(record.snapshot.market.map(|market| market.to_string()))
            .bind(record.target.market_name.clone())
            .bind(
                record
                    .snapshot
                    .symbol
                    .clone()
                    .or_else(|| record.target.symbol.clone()),
            )
            .bind(record.snapshot.liquidity_mint.to_string())
            .bind(mint_decimals)
            .bind(reserve_last_update_slot)
            .bind(record.snapshot.reserve_last_update_stale)
            .bind(i16::from(record.snapshot.reserve_price_status))
            .bind(record.snapshot.available_amount)
            .bind(record.snapshot.borrowed_amount)
            .bind(record.snapshot.borrowed_amount_sf.clone())
            .bind(record.snapshot.total_supply_amount)
            .bind(record.snapshot.market_price_usd)
            .bind(market_price_last_updated_ts)
            .bind(cumulative_borrow_rate_bsf)
            .bind(record.snapshot.total_supply_usd_estimate)
            .bind(record.snapshot.total_borrow_usd_estimate)
            .bind(record.snapshot.utilization)
            .bind(record.snapshot.borrow_apr)
            .bind(record.snapshot.supply_apr)
            .bind(record.snapshot.borrow_apy)
            .bind(record.snapshot.supply_apy)
            .bind(i16::from(record.snapshot.protocol_take_rate_pct))
            .bind(i32::from(record.snapshot.host_fixed_interest_rate_bps))
            .bind(record.diff.is_some_and(|diff| diff.changed))
            .bind(changed_fields)
            .bind(record.diff_summary)
            .bind(diff_json)
            .bind(target_json)
            .bind(snapshot_json)
            .bind(record_json)
            .bind(record.raw_account_data_base64)
            .bind(record.target.api_supply_apy)
            .bind(record.target.api_borrow_apy)
            .bind(record.target.api_total_supply_usd)
            .bind(record.target.api_total_borrow_usd)
            .bind(record.source_commitment)
            .bind(record.received_at)
            .bind(record.decoded_at)
            .bind(receive_to_decode_ms)
            .bind(decode_to_insert_ms)
            .fetch_optional(&self.pool)
            .await
            .context("insert TimescaleDB reserve update")?;

        let outcome = if let Some(event_id) = inserted_event_id {
            TimescaleInsertOutcome {
                event_id,
                inserted: true,
            }
        } else {
            let event_id = sqlx::query_scalar::<_, i64>(&self.dedupe_lookup_sql)
                .bind(&dedupe_key)
                .fetch_one(&self.pool)
                .await
                .context("lookup duplicate TimescaleDB reserve update")?;
            TimescaleInsertOutcome {
                event_id,
                inserted: false,
            }
        };

        Ok(outcome)
    }

    pub async fn insert_batch_skip_duplicates(
        &self,
        records: &[ReserveUpdateRecord<'_>],
    ) -> Result<usize> {
        if records.is_empty() {
            return Ok(0);
        }
        let rows = records
            .iter()
            .map(batch_record_json)
            .collect::<Result<Vec<_>>>()?;
        let rows_json = Value::Array(rows);
        let inserted = sqlx::query_scalar::<_, i64>(&self.insert_batch_sql)
            .bind(rows_json)
            .fetch_one(&self.pool)
            .await
            .context("batch insert TimescaleDB reserve updates")?;
        Ok(inserted.max(0) as usize)
    }

    pub async fn insert_prepared_batch_skip_duplicates(&self, rows: Vec<Value>) -> Result<usize> {
        if rows.is_empty() {
            return Ok(0);
        }
        let rows_json = Value::Array(rows);
        let inserted = sqlx::query_scalar::<_, i64>(&self.insert_batch_sql)
            .bind(rows_json)
            .fetch_one(&self.pool)
            .await
            .context("batch insert prepared TimescaleDB reserve updates")?;
        Ok(inserted.max(0) as usize)
    }

    pub async fn load_supported_targets(
        &self,
        requested_reserves: &[Pubkey],
    ) -> Result<Vec<ReserveTarget>> {
        let rows = if requested_reserves.is_empty() {
            sqlx::query_as::<_, SupportedReserveTargetRow>(&self.load_supported_targets_sql)
                .fetch_all(&self.pool)
                .await
        } else {
            sqlx::query_as::<_, SupportedReserveTargetRow>(
                &self.load_supported_targets_filtered_sql,
            )
            .bind(
                requested_reserves
                    .iter()
                    .map(ToString::to_string)
                    .collect::<Vec<_>>(),
            )
            .fetch_all(&self.pool)
            .await
        }
        .context("load supported Kamino reserve targets")?;

        rows.into_iter()
            .map(SupportedReserveTargetRow::try_into_target)
            .collect()
    }

    pub async fn upsert_supported_reserves(
        &self,
        records: &[SupportedReserveRecord],
    ) -> Result<usize> {
        let mut tx = self
            .pool
            .begin()
            .await
            .context("begin supported reserve sync transaction")?;

        sqlx::query(&self.deactivate_supported_reserves_sql)
            .execute(&mut *tx)
            .await
            .context("deactivate existing supported reserves")?;

        for record in records {
            sqlx::query(&self.upsert_supported_reserve_sql)
                .bind(record.market.to_string())
                .bind(record.liquidity_mint.to_string())
                .bind(record.reserve.to_string())
                .bind(record.market_name.clone())
                .bind(record.symbol.clone())
                .bind(record.risk_baskets.clone())
                .execute(&mut *tx)
                .await
                .with_context(|| {
                    format!(
                        "upsert supported reserve market {} mint {}",
                        record.market, record.liquidity_mint
                    )
                })?;
        }

        tx.commit()
            .await
            .context("commit supported reserve sync transaction")?;
        Ok(records.len())
    }
}

struct SupportedReserveTargetRow {
    reserve: String,
    market: String,
    market_name: Option<String>,
    symbol: Option<String>,
    liquidity_mint: String,
}

impl<'r> FromRow<'r, PgRow> for SupportedReserveTargetRow {
    fn from_row(row: &'r PgRow) -> Result<Self, sqlx::Error> {
        Ok(Self {
            reserve: row.try_get("reserve")?,
            market: row.try_get("market")?,
            market_name: row.try_get("market_name")?,
            symbol: row.try_get("symbol")?,
            liquidity_mint: row.try_get("liquidity_mint")?,
        })
    }
}

impl SupportedReserveTargetRow {
    fn try_into_target(self) -> Result<ReserveTarget> {
        Ok(ReserveTarget {
            reserve: Pubkey::from_str(&self.reserve).with_context(|| {
                format!("supported reserve has invalid reserve {}", self.reserve)
            })?,
            market: Some(Pubkey::from_str(&self.market).with_context(|| {
                format!("supported reserve has invalid market {}", self.market)
            })?),
            market_name: self.market_name,
            symbol: self.symbol,
            liquidity_mint: Some(Pubkey::from_str(&self.liquidity_mint).with_context(|| {
                format!(
                    "supported reserve has invalid liquidity mint {}",
                    self.liquidity_mint
                )
            })?),
            api_supply_apy: None,
            api_borrow_apy: None,
            api_total_supply_usd: None,
            api_total_borrow_usd: None,
        })
    }
}

fn insert_sql(schema: &str) -> String {
    let qualified_table = format!("{}.{}", quote_ident(schema), quote_ident(TABLE_NAME));
    let qualified_dedupe = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_update_dedupe")
    );
    let qualified_sequence = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_update_event_id_seq")
    );
    format!(
        r#"
WITH inserted_dedupe AS (
    INSERT INTO {qualified_dedupe} (
        dedupe_key, event_id, reserve, slot, account_data_hash
    ) VALUES (
        $1, nextval('{qualified_sequence}'::regclass), $2, $3, $4
    )
    ON CONFLICT (dedupe_key) DO NOTHING
    RETURNING event_id
)
INSERT INTO {qualified_table} (
    event_id, observed_at, slot, kind, source, reserve, market, market_name, symbol,
    liquidity_mint, mint_decimals, reserve_last_update_slot, reserve_last_update_stale,
    reserve_price_status, available_amount, borrowed_amount, borrowed_amount_sf,
    total_supply_amount, market_price_usd, market_price_last_updated_ts,
    cumulative_borrow_rate_bsf, total_supply_usd_estimate, total_borrow_usd_estimate,
    utilization, borrow_apr, supply_apr, borrow_apy, supply_apy,
    protocol_take_rate_pct, host_fixed_interest_rate_bps, diff_changed,
    changed_fields, diff_summary, diff, target, snapshot, record,
    raw_account_data_base64, api_supply_apy, api_borrow_apy,
    api_total_supply_usd, api_total_borrow_usd, source_commitment,
    account_data_hash, received_at, decoded_at, receive_to_decode_ms,
    decode_to_insert_ms
)
SELECT
    event_id, $5, $3, $6, $7, $2, $8, $9, $10, $11,
    $12, $13, $14, $15, $16, $17, $18, $19, $20, $21,
    $22, $23, $24, $25, $26, $27, $28, $29, $30, $31,
    $32, $33, $34, $35, $36, $37, $38, $39, $40, $41,
    $42, $43, $44, $4, $45, $46, $47, $48
FROM inserted_dedupe
RETURNING event_id;
"#
    )
}

fn insert_batch_sql(schema: &str) -> String {
    let qualified_table = format!("{}.{}", quote_ident(schema), quote_ident(TABLE_NAME));
    let qualified_dedupe = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_update_dedupe")
    );
    let qualified_sequence = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_update_event_id_seq")
    );
    format!(
        r#"
WITH input AS (
    SELECT *
    FROM jsonb_to_recordset($1::jsonb) AS row (
        dedupe_key text,
        reserve text,
        slot bigint,
        account_data_hash text,
        observed_at timestamptz,
        kind text,
        source text,
        market text,
        market_name text,
        symbol text,
        liquidity_mint text,
        mint_decimals integer,
        reserve_last_update_slot bigint,
        reserve_last_update_stale boolean,
        reserve_price_status smallint,
        available_amount double precision,
        borrowed_amount double precision,
        borrowed_amount_sf text,
        total_supply_amount double precision,
        market_price_usd double precision,
        market_price_last_updated_ts bigint,
        cumulative_borrow_rate_bsf text,
        total_supply_usd_estimate double precision,
        total_borrow_usd_estimate double precision,
        utilization double precision,
        borrow_apr double precision,
        supply_apr double precision,
        borrow_apy double precision,
        supply_apy double precision,
        protocol_take_rate_pct smallint,
        host_fixed_interest_rate_bps integer,
        diff_changed boolean,
        changed_fields text[],
        diff_summary text,
        diff jsonb,
        target jsonb,
        snapshot jsonb,
        record jsonb,
        raw_account_data_base64 text,
        api_supply_apy double precision,
        api_borrow_apy double precision,
        api_total_supply_usd double precision,
        api_total_borrow_usd double precision,
        source_commitment text,
        received_at timestamptz,
        decoded_at timestamptz,
        receive_to_decode_ms bigint,
        decode_to_insert_ms bigint
    )
),
inserted_dedupe AS (
    INSERT INTO {qualified_dedupe} (
        dedupe_key, event_id, reserve, slot, account_data_hash
    )
    SELECT
        dedupe_key,
        nextval('{qualified_sequence}'::regclass),
        reserve,
        slot,
        account_data_hash
    FROM input
    ON CONFLICT (dedupe_key) DO NOTHING
    RETURNING dedupe_key, event_id
),
inserted_updates AS (
    INSERT INTO {qualified_table} (
        event_id, observed_at, slot, kind, source, reserve, market, market_name, symbol,
        liquidity_mint, mint_decimals, reserve_last_update_slot, reserve_last_update_stale,
        reserve_price_status, available_amount, borrowed_amount, borrowed_amount_sf,
        total_supply_amount, market_price_usd, market_price_last_updated_ts,
        cumulative_borrow_rate_bsf, total_supply_usd_estimate, total_borrow_usd_estimate,
        utilization, borrow_apr, supply_apr, borrow_apy, supply_apy,
        protocol_take_rate_pct, host_fixed_interest_rate_bps, diff_changed,
        changed_fields, diff_summary, diff, target, snapshot, record,
        raw_account_data_base64, api_supply_apy, api_borrow_apy,
        api_total_supply_usd, api_total_borrow_usd, source_commitment,
        account_data_hash, received_at, decoded_at, receive_to_decode_ms,
        decode_to_insert_ms
    )
    SELECT
        d.event_id, i.observed_at, i.slot, i.kind, i.source, i.reserve, i.market,
        i.market_name, i.symbol, i.liquidity_mint, i.mint_decimals,
        i.reserve_last_update_slot, i.reserve_last_update_stale, i.reserve_price_status,
        i.available_amount, i.borrowed_amount, i.borrowed_amount_sf,
        i.total_supply_amount, i.market_price_usd, i.market_price_last_updated_ts,
        i.cumulative_borrow_rate_bsf, i.total_supply_usd_estimate,
        i.total_borrow_usd_estimate, i.utilization, i.borrow_apr, i.supply_apr,
        i.borrow_apy, i.supply_apy, i.protocol_take_rate_pct,
        i.host_fixed_interest_rate_bps, i.diff_changed, i.changed_fields,
        i.diff_summary, i.diff, i.target, i.snapshot, i.record,
        i.raw_account_data_base64, i.api_supply_apy, i.api_borrow_apy,
        i.api_total_supply_usd, i.api_total_borrow_usd, i.source_commitment,
        i.account_data_hash, i.received_at, i.decoded_at, i.receive_to_decode_ms,
        i.decode_to_insert_ms
    FROM input i
    JOIN inserted_dedupe d USING (dedupe_key)
    RETURNING event_id
)
SELECT count(*)::bigint FROM inserted_updates;
"#
    )
}

fn batch_record_json(record: &ReserveUpdateRecord<'_>) -> Result<Value> {
    let target_json = serde_json::to_value(record.target).context("serialize target JSON")?;
    let snapshot_json = serde_json::to_value(record.snapshot).context("serialize snapshot JSON")?;
    let diff_json = record
        .diff
        .map(serde_json::to_value)
        .transpose()
        .context("serialize diff JSON")?
        .unwrap_or_else(|| json!({}));
    let record_json = serde_json::to_value(record).context("serialize full reserve update JSON")?;
    let slot = i64_from_u64(record.slot, "slot")?;
    let reserve_last_update_slot = i64_from_u64(
        record.snapshot.reserve_last_update_slot,
        "reserve_last_update_slot",
    )?;
    let market_price_last_updated_ts = i64_from_u64(
        record.snapshot.market_price_last_updated_ts,
        "market_price_last_updated_ts",
    )?;
    let mint_decimals = i32_from_u64(record.snapshot.mint_decimals, "mint_decimals")?;
    let cumulative_borrow_rate_bsf = record
        .snapshot
        .cumulative_borrow_rate_bsf
        .iter()
        .map(u64::to_string)
        .collect::<Vec<_>>()
        .join(":");
    let changed_fields = record
        .diff
        .map(|diff| {
            diff.changed_fields
                .iter()
                .map(|field| (*field).to_string())
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    let dedupe_key = TimescaleSink::dedupe_key(record);
    let decode_to_insert_ms = chrono::Utc::now()
        .signed_duration_since(record.decoded_at)
        .num_milliseconds()
        .max(0);
    let receive_to_decode_ms = i64_from_u128(record.receive_to_decode_ms, "receive_to_decode_ms")?;

    Ok(json!({
        "dedupe_key": dedupe_key,
        "reserve": record.snapshot.reserve.to_string(),
        "slot": slot,
        "account_data_hash": record.account_data_hash,
        "observed_at": record.observed_at,
        "kind": record.kind,
        "source": record.source,
        "market": record.snapshot.market.map(|market| market.to_string()),
        "market_name": record.target.market_name,
        "symbol": record.snapshot.symbol.clone().or_else(|| record.target.symbol.clone()),
        "liquidity_mint": record.snapshot.liquidity_mint.to_string(),
        "mint_decimals": mint_decimals,
        "reserve_last_update_slot": reserve_last_update_slot,
        "reserve_last_update_stale": record.snapshot.reserve_last_update_stale,
        "reserve_price_status": i16::from(record.snapshot.reserve_price_status),
        "available_amount": record.snapshot.available_amount,
        "borrowed_amount": record.snapshot.borrowed_amount,
        "borrowed_amount_sf": record.snapshot.borrowed_amount_sf,
        "total_supply_amount": record.snapshot.total_supply_amount,
        "market_price_usd": record.snapshot.market_price_usd,
        "market_price_last_updated_ts": market_price_last_updated_ts,
        "cumulative_borrow_rate_bsf": cumulative_borrow_rate_bsf,
        "total_supply_usd_estimate": record.snapshot.total_supply_usd_estimate,
        "total_borrow_usd_estimate": record.snapshot.total_borrow_usd_estimate,
        "utilization": record.snapshot.utilization,
        "borrow_apr": record.snapshot.borrow_apr,
        "supply_apr": record.snapshot.supply_apr,
        "borrow_apy": record.snapshot.borrow_apy,
        "supply_apy": record.snapshot.supply_apy,
        "protocol_take_rate_pct": i16::from(record.snapshot.protocol_take_rate_pct),
        "host_fixed_interest_rate_bps": i32::from(record.snapshot.host_fixed_interest_rate_bps),
        "diff_changed": record.diff.is_some_and(|diff| diff.changed),
        "changed_fields": changed_fields,
        "diff_summary": record.diff_summary,
        "diff": diff_json,
        "target": target_json,
        "snapshot": snapshot_json,
        "record": record_json,
        "raw_account_data_base64": record.raw_account_data_base64,
        "api_supply_apy": record.target.api_supply_apy,
        "api_borrow_apy": record.target.api_borrow_apy,
        "api_total_supply_usd": record.target.api_total_supply_usd,
        "api_total_borrow_usd": record.target.api_total_borrow_usd,
        "source_commitment": record.source_commitment,
        "received_at": record.received_at,
        "decoded_at": record.decoded_at,
        "receive_to_decode_ms": receive_to_decode_ms,
        "decode_to_insert_ms": decode_to_insert_ms,
    }))
}

fn dedupe_lookup_sql(schema: &str) -> String {
    let qualified_dedupe = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_update_dedupe")
    );
    format!("SELECT event_id FROM {qualified_dedupe} WHERE dedupe_key = $1")
}

fn load_supported_targets_sql(schema: &str, filtered: bool) -> String {
    let qualified_table = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("supported_reserves")
    );
    let filter = if filtered {
        " AND reserve = ANY($1)"
    } else {
        ""
    };
    format!(
        "SELECT reserve, market, market_name, symbol, liquidity_mint
         FROM {qualified_table}
         WHERE active = TRUE{filter}
         ORDER BY market ASC, liquidity_mint ASC, reserve ASC"
    )
}

fn deactivate_supported_reserves_sql(schema: &str) -> String {
    let qualified_table = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("supported_reserves")
    );
    format!(
        "UPDATE {qualified_table}
         SET active = FALSE, updated_at = now()
         WHERE source = 'kamino-api'"
    )
}

fn upsert_supported_reserve_sql(schema: &str) -> String {
    let qualified_table = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("supported_reserves")
    );
    format!(
        "INSERT INTO {qualified_table} (
            market, liquidity_mint, reserve, market_name, symbol, risk_baskets,
            source, active, fetched_at, updated_at
         ) VALUES (
            $1, $2, $3, $4, $5, $6, 'kamino-api', TRUE, now(), now()
         )
         ON CONFLICT (market, liquidity_mint) DO UPDATE
         SET reserve = EXCLUDED.reserve,
             market_name = EXCLUDED.market_name,
             symbol = EXCLUDED.symbol,
             risk_baskets = EXCLUDED.risk_baskets,
             source = EXCLUDED.source,
             active = TRUE,
             fetched_at = EXCLUDED.fetched_at,
             updated_at = EXCLUDED.updated_at"
    )
}

fn i64_from_u64(value: u64, field: &str) -> Result<i64> {
    i64::try_from(value).with_context(|| format!("{field} does not fit into PostgreSQL BIGINT"))
}

fn i32_from_u64(value: u64, field: &str) -> Result<i32> {
    i32::try_from(value).with_context(|| format!("{field} does not fit into PostgreSQL INTEGER"))
}

fn i64_from_u128(value: u128, field: &str) -> Result<i64> {
    i64::try_from(value).with_context(|| format!("{field} does not fit into PostgreSQL BIGINT"))
}

fn hex_encode(bytes: &[u8]) -> String {
    let mut encoded = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        encoded.push_str(&format!("{byte:02x}"));
    }
    encoded
}

fn quote_ident(identifier: &str) -> String {
    format!("\"{}\"", identifier.replace('"', "\"\""))
}

fn validate_identifier(value: &str, name: &str) -> Result<()> {
    let mut chars = value.chars();
    let Some(first) = chars.next() else {
        bail!("{name} cannot be empty");
    };
    if !(first == '_' || first.is_ascii_alphabetic()) {
        bail!("{name} must start with an ASCII letter or underscore");
    }
    if !chars.all(|ch| ch == '_' || ch.is_ascii_alphanumeric()) {
        bail!("{name} may only contain ASCII letters, digits, and underscores");
    }
    Ok(())
}
