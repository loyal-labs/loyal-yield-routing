use std::{collections::BTreeSet, str::FromStr, time::Duration};

use anyhow::{bail, Context, Result};
use serde::Serialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::pubkey::Pubkey;
use sqlx::{
    postgres::{PgPoolOptions, PgRow},
    FromRow, PgPool, Row,
};

use loyal_kamino_codec::{
    ReserveDiff, ReserveSnapshot, ReserveTarget, SupportedReserveRecord,
    RESERVE_OBSERVATION_SCHEMA_VERSION,
};

const TABLE_NAME: &str = "reserve_updates";
const HTTP_SNAPSHOT_SOURCE: &str = "http_snapshot";
const HTTP_CONFIRMED_REFRESH_SOURCE: &str = "http_confirmed_refresh";
const LASERSTREAM_SOURCE: &str = "laserstream_grpc";
const WEBSOCKET_SOURCE: &str = "websocket";
/// Namespace seed for reserve-scoped transaction locks protecting the durable
/// observation floor. Hash collisions only add harmless serialization.
const CONFIRMED_OBSERVATION_FLOOR_LOCK_SEED: i64 = 5_499_540_200_513_621;
/// Serializes every catalog comparison/publication across overlapping monitor
/// processes. The exact value is a repo-owned namespace, not external state.
const SUPPORTED_RESERVE_CATALOG_LOCK_KEY: i64 = 5_499_540_200_513_620;

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
    load_active_supported_reserve_identities_sql: String,
    deactivate_supported_reserves_sql: String,
    upsert_supported_reserve_sql: String,
    upsert_current_state_sql: String,
    delete_stale_current_verification_sql: String,
    upsert_confirmed_verification_sql: String,
    advance_confirmed_observation_floor_sql: String,
    verify_confirmed_states_sql: String,
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
    /// This call inserted or advanced the compact HTTP-owned state pointer.
    pub current_state_admitted: bool,
    /// This call inserted or advanced the matching confirmed watermark.
    pub confirmed_verification_admitted: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct ConfirmedStateVerification {
    pub reserve: String,
    pub account_data_hash: String,
    pub verified_slot: i64,
    pub verified_at: chrono::DateTime<chrono::Utc>,
    pub commitment: &'static str,
    pub verification_source: &'static str,
    pub state_valid: bool,
}

#[derive(Debug, Default, PartialEq, Eq)]
pub struct ConfirmedStateVerificationOutcome {
    /// The confirmed read already matched the compact HTTP-owned state and
    /// could retain or advance its verification watermark.
    pub matched: BTreeSet<String>,
    /// The read conflicted with a pre-existing equal/newer observation floor.
    /// It updated the durable rank-2 floor, but must be observed again before
    /// the caller may persist or admit the candidate state.
    pub deferred: BTreeSet<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum SupportedReserveCatalogPublication {
    /// The live monitor may only renew metadata/timestamps for the exact
    /// decoding topology it already owns. Additions, removals, and reserve
    /// replacements require the explicit sync path and a process restart.
    ExactRefresh,
    /// Explicit sync allows bootstrap, additions, and reserve replacement for
    /// a retained (market, mint) pair. Pair removal stays operator-gated.
    ExplicitSync { allow_removals: bool },
}

#[derive(Debug)]
struct SupportedReserveIdentityRow {
    market: String,
    liquidity_mint: String,
    reserve: String,
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
            load_active_supported_reserve_identities_sql:
                load_active_supported_reserve_identities_sql(&config.schema),
            deactivate_supported_reserves_sql: deactivate_supported_reserves_sql(&config.schema),
            upsert_supported_reserve_sql: upsert_supported_reserve_sql(&config.schema),
            upsert_current_state_sql: upsert_current_state_sql(&config.schema),
            delete_stale_current_verification_sql: delete_stale_current_verification_sql(
                &config.schema,
            ),
            upsert_confirmed_verification_sql: upsert_confirmed_verification_sql(&config.schema),
            advance_confirmed_observation_floor_sql: advance_confirmed_observation_floor_sql(
                &config.schema,
            ),
            verify_confirmed_states_sql: verify_confirmed_states_sql(&config.schema),
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
            record.source,
            &record.snapshot.reserve.to_string(),
            record.slot,
            record.account_data_hash,
        )
    }

    pub fn dedupe_key_parts(
        source_commitment: &str,
        source: &str,
        reserve: &str,
        slot: u64,
        account_data_hash: &str,
    ) -> String {
        // Preserve the established subscription/historic keyspace while
        // separating HTTP proof observations from an identical stream row.
        // Otherwise a stream-first row could be mistaken for HTTP provenance.
        let provenance = if is_confirmed_http_source(source) {
            ":http"
        } else {
            ""
        };
        format!(
            "v{RESERVE_OBSERVATION_SCHEMA_VERSION}:{source_commitment}{provenance}:{reserve}:{slot}:{account_data_hash}"
        )
    }

    pub async fn insert(&self, record: &ReserveUpdateRecord<'_>) -> Result<TimescaleInsertOutcome> {
        let is_confirmed_http = is_confirmed_http_source(record.source);
        if is_confirmed_http && record.source_commitment != "confirmed" {
            bail!(
                "HTTP reserve verification source {} must use confirmed commitment",
                record.source
            );
        }
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

        let mut tx = self
            .pool
            .begin()
            .await
            .context("begin confirmed reserve update transaction")?;
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
            .fetch_optional(&mut *tx)
            .await
            .context("insert TimescaleDB reserve update")?;

        let outcome = if let Some(event_id) = inserted_event_id {
            TimescaleInsertOutcome {
                event_id,
                inserted: true,
                current_state_admitted: false,
                confirmed_verification_admitted: false,
            }
        } else {
            let event_id = sqlx::query_scalar::<_, i64>(&self.dedupe_lookup_sql)
                .bind(&dedupe_key)
                .fetch_one(&mut *tx)
                .await
                .context("lookup duplicate TimescaleDB reserve update")?;
            TimescaleInsertOutcome {
                event_id,
                inserted: false,
                current_state_admitted: false,
                confirmed_verification_admitted: false,
            }
        };

        let mut outcome = outcome;

        if is_confirmed_http {
            let current_state_result = sqlx::query(&self.upsert_current_state_sql)
                .bind(record.snapshot.reserve.to_string())
                .bind(outcome.event_id)
                .bind(record.account_data_hash)
                .bind(slot)
                .bind(record.observed_at)
                .bind(record.source)
                .execute(&mut *tx)
                .await
                .context("advance HTTP-owned current reserve state pointer")?;
            outcome.current_state_admitted = current_state_result.rows_affected() > 0;
            sqlx::query(&self.delete_stale_current_verification_sql)
                .bind(record.snapshot.reserve.to_string())
                .execute(&mut *tx)
                .await
                .context("remove verification superseded by HTTP current state")?;
            let verification_result = sqlx::query(&self.upsert_confirmed_verification_sql)
                .bind(record.snapshot.reserve.to_string())
                .bind(outcome.event_id)
                .bind(record.account_data_hash)
                .bind(slot)
                .bind(record.received_at)
                .bind(record.source_commitment)
                .bind(record.source)
                .execute(&mut *tx)
                .await
                .context("advance HTTP confirmed reserve verification watermark")?;
            outcome.confirmed_verification_admitted = verification_result.rows_affected() > 0;
        } else if record.source_commitment == "confirmed"
            && is_confirmed_subscription_source(record.source)
        {
            sqlx::query(&self.advance_confirmed_observation_floor_sql)
                .bind(record.snapshot.reserve.to_string())
                .bind(slot)
                .bind(Some(record.account_data_hash))
                .bind(true)
                .bind(record.source)
                .bind(1_i16)
                .bind(record.observed_at)
                .execute(&mut *tx)
                .await
                .context("advance confirmed stream floor")?;
        }

        tx.commit()
            .await
            .context("commit confirmed reserve update transaction")?;

        Ok(outcome)
    }

    /// Records malformed or wrong-owner confirmed subscription evidence in
    /// the durable observation floor before the caller logs and continues.
    /// This never advances the HTTP-owned pointer or freshness watermark.
    pub async fn record_malformed_confirmed_stream_state(
        &self,
        reserve: &Pubkey,
        slot: u64,
        source: &str,
        observed_at: chrono::DateTime<chrono::Utc>,
    ) -> Result<()> {
        if !is_confirmed_subscription_source(source) {
            bail!("unsupported confirmed subscription source {source}");
        }
        let slot = i64_from_u64(slot, "malformed subscription slot")?;
        sqlx::query(&self.advance_confirmed_observation_floor_sql)
            .bind(reserve.to_string())
            .bind(slot)
            .bind(Option::<String>::None)
            .bind(false)
            .bind(source)
            .bind(1_i16)
            .bind(observed_at)
            .execute(&self.pool)
            .await
            .context("advance malformed confirmed stream floor")?;
        Ok(())
    }

    /// Advances watermarks only for hashes matching the compact current-state
    /// pointer. A non-regressing confirmed mismatch invalidates the old
    /// watermark before the reserve is returned as unmatched.
    pub async fn verify_confirmed_states(
        &self,
        verifications: &[ConfirmedStateVerification],
    ) -> Result<ConfirmedStateVerificationOutcome> {
        if verifications.is_empty() {
            return Ok(ConfirmedStateVerificationOutcome::default());
        }
        let reserve_addresses = verifications
            .iter()
            .map(|verification| verification.reserve.clone())
            .collect::<BTreeSet<_>>();
        if reserve_addresses.len() != verifications.len() {
            bail!("confirmed reserve verification batch contains duplicate reserves");
        }
        let rows = serde_json::to_value(verifications)
            .context("serialize confirmed reserve verification batch")?;
        let mut tx = self
            .pool
            .begin()
            .await
            .context("begin confirmed reserve verification transaction")?;
        // Acquire every reserve lock in deterministic order in a separate
        // statement. If this waits on another writer, the verification query
        // below receives a fresh READ COMMITTED snapshot after that writer
        // commits, including when the floor row did not exist beforehand.
        sqlx::query(
            "SELECT pg_advisory_xact_lock(hashtextextended(locked.reserve, $2))
             FROM unnest($1::text[]) WITH ORDINALITY AS locked(reserve, lock_order)
             ORDER BY locked.lock_order",
        )
        .bind(reserve_addresses.into_iter().collect::<Vec<_>>())
        .bind(CONFIRMED_OBSERVATION_FLOOR_LOCK_SEED)
        .fetch_all(&mut *tx)
        .await
        .context("lock confirmed observation floors for HTTP verification batch")?;
        let classified = sqlx::query_as::<_, (String, String)>(&self.verify_confirmed_states_sql)
            .bind(rows)
            .fetch_all(&mut *tx)
            .await
            .context("advance confirmed reserve verification watermarks")?;
        let mut outcome = ConfirmedStateVerificationOutcome::default();
        for (reserve, classification) in classified {
            match classification.as_str() {
                "matched" => {
                    outcome.matched.insert(reserve);
                }
                "deferred" => {
                    outcome.deferred.insert(reserve);
                }
                other => bail!("unknown confirmed verification classification {other}"),
            }
        }
        tx.commit()
            .await
            .context("commit confirmed reserve verification transaction")?;
        Ok(outcome)
    }

    /// Historical/backfill ingestion only. These rows are evidence, not a live
    /// confirmed read, so this path deliberately cannot advance freshness.
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

    /// Prepared historical/backfill variant; likewise never advances the live
    /// confirmed verification watermark.
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
        self.sync_supported_reserves(records, false).await
    }

    /// Explicit catalog sync. Bootstrap, additions, and reserve replacement for
    /// an existing (market, mint) pair are allowed. Removing a previously active
    /// pair requires the operator-only flag at the CLI boundary.
    pub async fn sync_supported_reserves(
        &self,
        records: &[SupportedReserveRecord],
        allow_removals: bool,
    ) -> Result<usize> {
        let (count, _) = self
            .publish_supported_reserves(
                records,
                SupportedReserveCatalogPublication::ExplicitSync { allow_removals },
                None,
            )
            .await?;
        Ok(count)
    }

    /// Renews the live catalog only when the API returned the exact decoding
    /// identity set already committed. Validation, timestamp renewal, and target
    /// loading share one advisory-locked transaction, so a partial response can
    /// neither shrink the planner denominator nor race a topology-changing sync.
    pub async fn refresh_supported_reserves(
        &self,
        records: &[SupportedReserveRecord],
        requested_reserves: &[Pubkey],
    ) -> Result<Vec<ReserveTarget>> {
        let (_, targets) = self
            .publish_supported_reserves(
                records,
                SupportedReserveCatalogPublication::ExactRefresh,
                Some(requested_reserves),
            )
            .await?;
        targets.context("exact supported reserve refresh did not load targets")
    }

    async fn publish_supported_reserves(
        &self,
        records: &[SupportedReserveRecord],
        publication: SupportedReserveCatalogPublication,
        requested_reserves: Option<&[Pubkey]>,
    ) -> Result<(usize, Option<Vec<ReserveTarget>>)> {
        if records.is_empty() {
            bail!("supported reserve catalog response must not be empty");
        }
        let incoming_pairs = records
            .iter()
            .map(|record| (record.market.to_string(), record.liquidity_mint.to_string()))
            .collect::<BTreeSet<_>>();
        let incoming_identities = records
            .iter()
            .map(|record| {
                (
                    record.market.to_string(),
                    record.liquidity_mint.to_string(),
                    record.reserve.to_string(),
                )
            })
            .collect::<BTreeSet<_>>();
        if incoming_pairs.len() != records.len() || incoming_identities.len() != records.len() {
            bail!("supported reserve catalog response contains duplicate decoding identities");
        }

        let mut tx = self
            .pool
            .begin()
            .await
            .context("begin supported reserve sync transaction")?;

        sqlx::query("SELECT pg_advisory_xact_lock($1)")
            .bind(SUPPORTED_RESERVE_CATALOG_LOCK_KEY)
            .execute(&mut *tx)
            .await
            .context("acquire supported reserve catalog publication lock")?;
        let current = sqlx::query_as::<_, SupportedReserveIdentityRow>(
            &self.load_active_supported_reserve_identities_sql,
        )
        .fetch_all(&mut *tx)
        .await
        .context("load active supported reserve identities before publication")?;
        let current_pairs = current
            .iter()
            .map(|row| (row.market.clone(), row.liquidity_mint.clone()))
            .collect::<BTreeSet<_>>();
        let current_identities = current
            .iter()
            .map(|row| {
                (
                    row.market.clone(),
                    row.liquidity_mint.clone(),
                    row.reserve.clone(),
                )
            })
            .collect::<BTreeSet<_>>();
        if current_pairs.len() != current.len() || current_identities.len() != current.len() {
            bail!("committed supported reserve catalog contains duplicate decoding identities");
        }
        match publication {
            SupportedReserveCatalogPublication::ExactRefresh => {
                if current_identities.is_empty() {
                    bail!(
                        "normal supported reserve refresh requires a bootstrapped catalog; run explicit sync first"
                    );
                }
                if incoming_identities != current_identities {
                    let removed = current_identities.difference(&incoming_identities).count();
                    let added = incoming_identities.difference(&current_identities).count();
                    bail!(
                        "normal supported reserve refresh rejected decoding topology change; removed_identity_count={removed} added_identity_count={added}; run explicit sync and restart"
                    );
                }
            }
            SupportedReserveCatalogPublication::ExplicitSync { allow_removals } => {
                if !allow_removals && !current_pairs.is_subset(&incoming_pairs) {
                    let removed = current_pairs.difference(&incoming_pairs).count();
                    bail!(
                        "supported reserve sync rejected {removed} removed market/mint pairs; intentional removals require --allow-supported-reserve-removals"
                    );
                }
            }
        }

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

        let targets = if let Some(requested_reserves) = requested_reserves {
            let rows = if requested_reserves.is_empty() {
                sqlx::query_as::<_, SupportedReserveTargetRow>(&self.load_supported_targets_sql)
                    .fetch_all(&mut *tx)
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
                .fetch_all(&mut *tx)
                .await
            }
            .context("load supported Kamino reserve targets in catalog transaction")?;
            Some(
                rows.into_iter()
                    .map(SupportedReserveTargetRow::try_into_target)
                    .collect::<Result<Vec<_>>>()?,
            )
        } else {
            None
        };

        tx.commit()
            .await
            .context("commit supported reserve sync transaction")?;
        Ok((records.len(), targets))
    }
}

struct SupportedReserveTargetRow {
    reserve: String,
    market: String,
    market_name: Option<String>,
    symbol: Option<String>,
    liquidity_mint: String,
}

impl<'r> FromRow<'r, PgRow> for SupportedReserveIdentityRow {
    fn from_row(row: &'r PgRow) -> Result<Self, sqlx::Error> {
        Ok(Self {
            market: row.try_get("market")?,
            liquidity_mint: row.try_get("liquidity_mint")?,
            reserve: row.try_get("reserve")?,
        })
    }
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

/// Single source of truth for how far a confirmed verification may trail the
/// LaserStream observation floor. The verified-updates view reads the same
/// function, so admission, eviction, and visibility cannot drift apart.
fn qualified_slot_tolerance(schema: &str) -> String {
    format!(
        "{}.{}()",
        quote_ident(schema),
        quote_ident("confirmed_verification_slot_tolerance")
    )
}

fn upsert_current_state_sql(schema: &str) -> String {
    let qualified_updates = format!("{}.{}", quote_ident(schema), quote_ident("reserve_updates"));
    let qualified_current = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_current_states")
    );
    let qualified_observation_floors = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_confirmed_observation_floors")
    );
    let qualified_verifications = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_confirmed_verifications")
    );
    let slot_tolerance = qualified_slot_tolerance(schema);
    format!(
        r#"
INSERT INTO {qualified_current} AS current (
    reserve, state_event_id, account_data_hash, state_slot, state_observed_at, state_source
)
SELECT
    state.reserve,
    state.event_id,
    state.account_data_hash,
    state.slot,
    state.observed_at,
    state.source
FROM {qualified_updates} state
LEFT JOIN {qualified_observation_floors} observation_floor
  ON observation_floor.reserve = state.reserve
LEFT JOIN {qualified_verifications} prior_verification
  ON prior_verification.reserve = state.reserve
WHERE state.reserve = $1
  AND state.event_id = $2
  AND state.account_data_hash = $3
  AND state.slot = $4
  AND state.observed_at = $5
  AND state.source IN ('http_snapshot', 'http_confirmed_refresh')
  AND $6 IN ('http_snapshot', 'http_confirmed_refresh')
  AND state.source_commitment = 'confirmed'
  -- At or below the floor, the exact valid floor hash or a bounded trailing
  -- margin may own the pointer. This still fences equal-slot observations from
  -- overlapping monitors, but it no longer requires an HTTP read to outrun the
  -- LaserStream floor, which it cannot do on an active reserve.
  AND (
        observation_floor.reserve IS NULL
     OR state.slot > observation_floor.floor_slot
     OR (
            observation_floor.state_valid
        AND observation_floor.account_data_hash = state.account_data_hash
     )
     OR (
            observation_floor.state_valid
        AND observation_floor.floor_slot > state.slot
        AND observation_floor.floor_slot - state.slot <= {slot_tolerance}
     )
  )
  AND (
        prior_verification.reserve IS NULL
     OR state.slot > prior_verification.verified_slot
     OR prior_verification.account_data_hash = state.account_data_hash
  )
ON CONFLICT (reserve) DO UPDATE
SET state_event_id = EXCLUDED.state_event_id,
    account_data_hash = EXCLUDED.account_data_hash,
    state_slot = EXCLUDED.state_slot,
    state_observed_at = EXCLUDED.state_observed_at,
    state_source = EXCLUDED.state_source,
    updated_at = now()
-- Only confirmed HTTP observations own this pointer. Event id is only a
-- deterministic tiebreaker between HTTP reads at the same context slot.
WHERE (EXCLUDED.state_slot, EXCLUDED.state_event_id)
    > (current.state_slot, current.state_event_id)
"#
    )
}

fn delete_stale_current_verification_sql(schema: &str) -> String {
    let qualified_current = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_current_states")
    );
    let qualified_verifications = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_confirmed_verifications")
    );
    format!(
        r#"
DELETE FROM {qualified_verifications} verification
USING {qualified_current} state
WHERE state.reserve = $1
  AND verification.reserve = state.reserve
  AND (
        verification.state_event_id <> state.state_event_id
     OR verification.account_data_hash <> state.account_data_hash
  )
"#
    )
}

fn upsert_confirmed_verification_sql(schema: &str) -> String {
    let qualified_current = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_current_states")
    );
    let qualified_verifications = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_confirmed_verifications")
    );
    let qualified_observation_floors = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_confirmed_observation_floors")
    );
    let slot_tolerance = qualified_slot_tolerance(schema);
    format!(
        r#"
INSERT INTO {qualified_verifications} AS current (
    reserve, state_event_id, account_data_hash, verified_slot, verified_at, commitment,
    verification_source
)
SELECT $1, $2, $3, $4, $5, $6, $7
FROM {qualified_current} state
LEFT JOIN {qualified_observation_floors} observation_floor
  ON observation_floor.reserve = state.reserve
WHERE state.reserve = $1
  AND state.state_event_id = $2
  AND state.account_data_hash = $3
  AND state.state_slot <= $4
  AND state.state_source IN ('http_snapshot', 'http_confirmed_refresh')
  AND $7 IN ('http_snapshot', 'http_confirmed_refresh')
  AND (
        observation_floor.reserve IS NULL
     OR $4 > observation_floor.floor_slot
     OR (
            observation_floor.state_valid
        AND observation_floor.account_data_hash = state.account_data_hash
     )
     -- Admit a confirmed read that merely trails the floor. Without this the
     -- verifier loses to LaserStream on every active reserve and the reserve
     -- can never re-enter the verified view. Strictly trailing only: an
     -- equal-slot read must still win on hash, which is the overlapping-monitor
     -- fence. An invalid floor never tolerates, because it reports the account
     -- itself as unusable rather than merely moved on.
     OR (
            observation_floor.state_valid
        AND observation_floor.floor_slot > $4
        AND observation_floor.floor_slot - $4 <= {slot_tolerance}
     )
  )
ON CONFLICT (reserve) DO UPDATE
SET state_event_id = EXCLUDED.state_event_id,
    account_data_hash = EXCLUDED.account_data_hash,
    verified_slot = EXCLUDED.verified_slot,
    verified_at = EXCLUDED.verified_at,
    commitment = EXCLUDED.commitment,
    verification_source = EXCLUDED.verification_source,
    updated_at = now()
WHERE (EXCLUDED.verified_slot, EXCLUDED.state_event_id)
    > (current.verified_slot, current.state_event_id)
"#
    )
}

fn advance_confirmed_observation_floor_sql(schema: &str) -> String {
    let qualified_current = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_current_states")
    );
    let qualified_verifications = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_confirmed_verifications")
    );
    let qualified_observation_floors = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_confirmed_observation_floors")
    );
    let slot_tolerance = qualified_slot_tolerance(schema);
    format!(
        r#"
WITH observation_lock AS MATERIALIZED (
    SELECT pg_advisory_xact_lock(
        hashtextextended($1, {CONFIRMED_OBSERVATION_FLOOR_LOCK_SEED})
    )
), advanced_floor AS (
    INSERT INTO {qualified_observation_floors} AS current (
        reserve, floor_slot, account_data_hash, state_valid, source, source_rank,
        observed_at
    )
    SELECT $1, $2, $3, $4, $5, $6, $7
    FROM observation_lock
    ON CONFLICT (reserve) DO UPDATE
    SET floor_slot = CASE
            WHEN EXCLUDED.floor_slot > current.floor_slot THEN EXCLUDED.floor_slot
            ELSE current.floor_slot
        END,
        account_data_hash = CASE
            WHEN EXCLUDED.floor_slot > current.floor_slot
                THEN EXCLUDED.account_data_hash
            WHEN EXCLUDED.source_rank > current.source_rank
                THEN EXCLUDED.account_data_hash
            WHEN current.state_valid
             AND EXCLUDED.state_valid
             AND current.account_data_hash = EXCLUDED.account_data_hash
                THEN current.account_data_hash
            ELSE NULL
        END,
        state_valid = CASE
            WHEN EXCLUDED.floor_slot > current.floor_slot THEN EXCLUDED.state_valid
            WHEN EXCLUDED.source_rank > current.source_rank THEN EXCLUDED.state_valid
            ELSE current.state_valid
             AND EXCLUDED.state_valid
             AND current.account_data_hash = EXCLUDED.account_data_hash
        END,
        source = CASE
            WHEN EXCLUDED.floor_slot > current.floor_slot
              OR EXCLUDED.source_rank >= current.source_rank
                THEN EXCLUDED.source
            ELSE current.source
        END,
        source_rank = CASE
            WHEN EXCLUDED.floor_slot > current.floor_slot THEN EXCLUDED.source_rank
            ELSE GREATEST(current.source_rank, EXCLUDED.source_rank)
        END,
        observation_id = EXCLUDED.observation_id,
        observed_at = GREATEST(current.observed_at, EXCLUDED.observed_at),
        updated_at = now()
    WHERE EXCLUDED.floor_slot > current.floor_slot
       OR (
            EXCLUDED.floor_slot = current.floor_slot
        AND EXCLUDED.source_rank >= current.source_rank
       )
    RETURNING reserve, floor_slot, observation_id, account_data_hash, state_valid,
              source_rank
)
DELETE FROM {qualified_verifications} verification
USING advanced_floor observation_floor, {qualified_current} state
WHERE verification.reserve = observation_floor.reserve
  AND state.reserve = observation_floor.reserve
  AND verification.state_event_id = state.state_event_id
  AND verification.account_data_hash = state.account_data_hash
  AND verification.verified_slot <= observation_floor.floor_slot
  -- Only evict once the verification has trailed the floor past the tolerance.
  -- Evicting on the first newer LaserStream observation is what made the
  -- verified view flap on every active reserve. An equal-slot conflict is not
  -- staleness but a genuine disagreement, so it is still evicted immediately,
  -- and so is any invalid floor: that floor reports the reserve account as
  -- unusable, which must fence routability now rather than after the window.
  AND (
        NOT observation_floor.state_valid
     OR verification.verified_slot = observation_floor.floor_slot
     OR observation_floor.floor_slot - verification.verified_slot > {slot_tolerance}
  )
  AND (
        NOT observation_floor.state_valid
     OR observation_floor.account_data_hash <> state.account_data_hash
  )
"#
    )
}

fn verify_confirmed_states_sql(schema: &str) -> String {
    let qualified_updates = format!("{}.{}", quote_ident(schema), quote_ident("reserve_updates"));
    let qualified_current = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_current_states")
    );
    let qualified_verifications = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_confirmed_verifications")
    );
    let qualified_observation_floors = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("reserve_confirmed_observation_floors")
    );
    let slot_tolerance = qualified_slot_tolerance(schema);
    let observation_schema_version = RESERVE_OBSERVATION_SCHEMA_VERSION;
    format!(
        r#"
WITH input AS (
    SELECT *
    FROM jsonb_to_recordset($1::jsonb) AS row (
        reserve text,
        account_data_hash text,
        verified_slot bigint,
        verified_at timestamptz,
        commitment text,
        verification_source text,
        state_valid boolean
    )
), eligible_input AS MATERIALIZED (
    SELECT *
    FROM input
    WHERE commitment = 'confirmed'
      AND verification_source IN ('http_snapshot', 'http_confirmed_refresh')
), locked_existing_floors AS MATERIALIZED (
    -- Read and lock the pre-update floor before attempting the batch merge.
    -- The dependent CTE below makes this ordering explicit, so an equal-slot
    -- conflict cannot first rewrite the floor and then validate itself against
    -- the rewritten value.
    SELECT
        existing.reserve,
        existing.floor_slot,
        existing.account_data_hash,
        existing.state_valid,
        existing.source,
        existing.source_rank,
        existing.observed_at
    FROM {qualified_observation_floors} existing
    JOIN (
        SELECT DISTINCT reserve
        FROM eligible_input
    ) requested
      ON requested.reserve = existing.reserve
    ORDER BY existing.reserve
    FOR UPDATE OF existing
), input_with_prior_floor AS MATERIALIZED (
    SELECT
        input.*,
        prior.floor_slot AS prior_floor_slot,
        prior.account_data_hash AS prior_floor_account_data_hash,
        prior.state_valid AS prior_floor_state_valid
    FROM eligible_input input
    LEFT JOIN locked_existing_floors prior
      ON prior.reserve = input.reserve
), advanced_floors AS (
    INSERT INTO {qualified_observation_floors} AS current (
        reserve, floor_slot, account_data_hash, state_valid, source, source_rank,
        observed_at
    )
    SELECT
        reserve,
        verified_slot,
        CASE WHEN state_valid THEN account_data_hash ELSE NULL END,
        state_valid,
        verification_source,
        2,
        verified_at
    FROM input_with_prior_floor
    ON CONFLICT (reserve) DO UPDATE
    SET floor_slot = EXCLUDED.floor_slot,
        account_data_hash = EXCLUDED.account_data_hash,
        state_valid = EXCLUDED.state_valid,
        source = EXCLUDED.source,
        source_rank = EXCLUDED.source_rank,
        observation_id = EXCLUDED.observation_id,
        observed_at = GREATEST(current.observed_at, EXCLUDED.observed_at),
        updated_at = now()
    WHERE EXCLUDED.floor_slot > current.floor_slot
       OR (
            EXCLUDED.floor_slot = current.floor_slot
        AND EXCLUDED.source_rank >= current.source_rank
       )
    RETURNING reserve, floor_slot, account_data_hash, state_valid, source_rank
), effective_floors AS MATERIALIZED (
    SELECT
        input.reserve,
        COALESCE(input.prior_floor_slot, advanced.floor_slot) AS floor_slot,
        CASE
            WHEN input.prior_floor_slot IS NOT NULL
                THEN input.prior_floor_account_data_hash
            ELSE advanced.account_data_hash
        END AS floor_account_data_hash,
        CASE
            WHEN input.prior_floor_slot IS NOT NULL THEN input.prior_floor_state_valid
            ELSE advanced.state_valid
        END AS floor_state_valid
    FROM input_with_prior_floor input
    LEFT JOIN advanced_floors advanced
      ON advanced.reserve = input.reserve
), deferred AS MATERIALIZED (
    -- Classify against the pre-update floor even when no compact pointer
    -- exists yet. Otherwise a stream-created floor could be rewritten by the
    -- first conflicting HTTP read and then admitted by same-read fallback.
    SELECT input.reserve
    FROM input_with_prior_floor input
    WHERE input.state_valid = true
      AND input.prior_floor_slot IS NOT NULL
      AND (
            -- Trailing the floor is the normal outcome of a confirmed read
            -- racing LaserStream, so only trailing past the shared tolerance
            -- counts as staleness. Deferring every trailing read is what kept
            -- an evicted reserve from ever re-entering the verified view.
            input.prior_floor_slot - input.verified_slot > {slot_tolerance}
         OR (
                -- An invalid floor reports the reserve account as unusable, so
                -- it fences at any distance rather than after the window.
                input.verified_slot < input.prior_floor_slot
            AND input.prior_floor_state_valid = false
         )
         OR (
                input.verified_slot = input.prior_floor_slot
            AND (
                    input.prior_floor_state_valid = false
                 OR input.prior_floor_account_data_hash IS DISTINCT FROM input.account_data_hash
            )
         )
      )
), locked_current AS MATERIALIZED (
    SELECT
        input.reserve,
        input.account_data_hash AS confirmed_account_data_hash,
        input.verified_slot,
        input.verified_at,
        input.commitment,
        input.verification_source,
        input.state_valid,
        current_state.state_event_id,
        current_state.account_data_hash AS current_account_data_hash,
        current_state.state_slot,
        current_state.state_source,
        COALESCE(
            (current_update.snapshot ->> 'observation_schema_version')::integer,
            0
        ) AS current_observation_schema_version,
        floor.floor_slot,
        floor.floor_account_data_hash,
        floor.floor_state_valid
    FROM eligible_input input
    JOIN effective_floors floor
      ON floor.reserve = input.reserve
    JOIN {qualified_current} current_state
      ON current_state.reserve = input.reserve
    JOIN {qualified_updates} current_update
      ON current_update.reserve = current_state.reserve
     AND current_update.event_id = current_state.state_event_id
     AND current_update.account_data_hash = current_state.account_data_hash
     AND current_update.slot = current_state.state_slot
    FOR UPDATE OF current_state
), invalidated AS (
    DELETE FROM {qualified_verifications} verification
    USING locked_current state
    WHERE verification.reserve = state.reserve
      AND (
            state.confirmed_account_data_hash <> state.current_account_data_hash
         OR state.state_valid = false
         OR state.floor_state_valid = false
         OR state.floor_account_data_hash IS DISTINCT FROM state.current_account_data_hash
      )
      -- Equality revokes a conflicting/invalid old watermark; this branch
      -- cannot admit a candidate state.
      AND state.verified_slot >= state.state_slot
      AND state.verified_slot >= state.floor_slot
      AND state.verified_slot >= verification.verified_slot
    RETURNING verification.reserve
), matching AS MATERIALIZED (
    SELECT
        state.reserve,
        state.state_event_id,
        state.confirmed_account_data_hash AS account_data_hash,
        state.verified_slot,
        state.verified_at,
        state.commitment,
        state.verification_source
    FROM locked_current state
    WHERE state.state_valid = true
      AND NOT EXISTS (
            SELECT 1
            FROM deferred
            WHERE deferred.reserve = state.reserve
      )
      AND state.confirmed_account_data_hash = state.current_account_data_hash
      AND state.verified_slot >= state.state_slot
      AND state.state_source IN ('http_snapshot', 'http_confirmed_refresh')
      AND state.current_observation_schema_version = {observation_schema_version}
      AND (
            state.verified_slot > state.floor_slot
         OR (
                state.verified_slot = state.floor_slot
            AND state.floor_state_valid
            AND state.floor_account_data_hash = state.current_account_data_hash
         )
      )
), advanced AS (
    INSERT INTO {qualified_verifications} AS current (
        reserve, state_event_id, account_data_hash, verified_slot, verified_at, commitment,
        verification_source
    )
    SELECT
        reserve, state_event_id, account_data_hash, verified_slot, verified_at, commitment,
        verification_source
    FROM matching
    ON CONFLICT (reserve) DO UPDATE
    SET state_event_id = EXCLUDED.state_event_id,
        account_data_hash = EXCLUDED.account_data_hash,
        verified_slot = EXCLUDED.verified_slot,
        verified_at = EXCLUDED.verified_at,
        commitment = EXCLUDED.commitment,
        verification_source = EXCLUDED.verification_source,
        updated_at = now()
    WHERE EXCLUDED.verified_slot > current.verified_slot
       OR (
            EXCLUDED.verified_slot = current.verified_slot
        AND EXCLUDED.state_event_id > current.state_event_id
       )
       OR (
            EXCLUDED.verified_slot = current.verified_slot
        AND EXCLUDED.state_event_id = current.state_event_id
        AND EXCLUDED.verified_at > current.verified_at
       )
    RETURNING reserve
)
SELECT reserve, 'matched'::text AS classification
FROM matching
UNION ALL
SELECT reserve, 'deferred'::text AS classification
FROM deferred
ORDER BY reserve, classification
"#
    )
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

fn load_active_supported_reserve_identities_sql(schema: &str) -> String {
    let qualified_table = format!(
        "{}.{}",
        quote_ident(schema),
        quote_ident("supported_reserves")
    );
    format!(
        "SELECT market, liquidity_mint, reserve
         FROM {qualified_table}
         WHERE active = TRUE
         ORDER BY market, liquidity_mint, reserve"
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
         WHERE active = TRUE"
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

fn is_confirmed_http_source(source: &str) -> bool {
    matches!(source, HTTP_SNAPSHOT_SOURCE | HTTP_CONFIRMED_REFRESH_SOURCE)
}

fn is_confirmed_subscription_source(source: &str) -> bool {
    matches!(source, LASERSTREAM_SOURCE | WEBSOCKET_SOURCE)
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
