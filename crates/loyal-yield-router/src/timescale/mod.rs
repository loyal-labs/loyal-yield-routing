use std::{collections::VecDeque, time::Duration};

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use sqlx::{
    postgres::{PgListener, PgPoolOptions, PgRow},
    FromRow, PgPool, Postgres, QueryBuilder, Row,
};

const DEFAULT_SCHEMA: &str = "kamino";
const DEFAULT_NOTIFY_CHANNEL: &str = "kamino_reserve_updates";
const RESERVE_UPDATES_TABLE: &str = "reserve_updates";
const LATEST_RESERVE_UPDATES_VIEW: &str = "latest_reserve_updates";
const SUPPORTED_RESERVES_TABLE: &str = "supported_reserves";
const RESERVE_UPDATE_ROW_COLUMNS: &str = "event_id, observed_at, slot, source, source_commitment, reserve, market, market_name, symbol, liquidity_mint, supply_apy, borrow_apy, utilization, total_supply_usd_estimate, total_borrow_usd_estimate, reserve_last_update_stale, diff_changed, changed_fields, diff_summary";
const RESERVE_WINDOW_STATS_COLUMNS: &str = "reserve, market, symbol, COUNT(*)::BIGINT AS update_count, AVG(supply_apy) AS avg_supply_apy, MIN(supply_apy) AS min_supply_apy, MAX(supply_apy) AS max_supply_apy, AVG(borrow_apy) AS avg_borrow_apy, AVG(utilization) AS avg_utilization, AVG(total_supply_usd_estimate) AS avg_supply_usd, AVG(total_borrow_usd_estimate) AS avg_borrow_usd, MAX(slot) AS max_slot, MAX(observed_at) AS last_observed_at";

#[derive(Debug, Clone)]
pub struct TimescaleRouterClientConfig {
    pub url: String,
    pub schema: String,
    pub notify_channel: String,
    pub max_connections: u32,
    pub acquire_timeout: Duration,
}

impl TimescaleRouterClientConfig {
    pub fn new(url: impl Into<String>) -> Self {
        Self {
            url: url.into(),
            schema: DEFAULT_SCHEMA.to_string(),
            notify_channel: DEFAULT_NOTIFY_CHANNEL.to_string(),
            max_connections: 5,
            acquire_timeout: Duration::from_secs(5),
        }
    }

    pub fn with_schema(mut self, schema: impl Into<String>) -> Self {
        self.schema = schema.into();
        self
    }

    pub fn with_notify_channel(mut self, notify_channel: impl Into<String>) -> Self {
        self.notify_channel = notify_channel.into();
        self
    }

    pub fn with_max_connections(mut self, max_connections: u32) -> Self {
        self.max_connections = max_connections;
        self
    }
}

#[derive(Clone)]
pub struct TimescaleRouterClient {
    pool: PgPool,
    schema: String,
    notify_channel: String,
}

impl TimescaleRouterClient {
    pub async fn connect(config: TimescaleRouterClientConfig) -> sqlx::Result<Self> {
        validate_identifier(&config.schema, "schema")?;
        validate_identifier(&config.notify_channel, "notify channel")?;
        let pool = PgPoolOptions::new()
            .max_connections(config.max_connections)
            .acquire_timeout(config.acquire_timeout)
            .connect(&config.url)
            .await?;

        Ok(Self {
            pool,
            schema: config.schema,
            notify_channel: config.notify_channel,
        })
    }

    pub fn from_pool(pool: PgPool, schema: impl Into<String>) -> sqlx::Result<Self> {
        let schema = schema.into();
        validate_identifier(&schema, "schema")?;
        Ok(Self {
            pool,
            schema,
            notify_channel: DEFAULT_NOTIFY_CHANNEL.to_string(),
        })
    }

    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    pub async fn latest_reserves(
        &self,
        filter: ReserveUpdateFilter,
    ) -> sqlx::Result<Vec<ReserveUpdateRow>> {
        let mut builder = self.select_latest_update_rows();
        push_update_filters(&mut builder, &filter);
        builder.push(" ORDER BY supply_apy DESC, observed_at DESC, slot DESC, reserve ASC");
        fetch_update_rows(builder, &self.pool).await
    }

    pub async fn latest_supported_reserves(
        &self,
        query: SupportedReserveLatestQuery,
    ) -> sqlx::Result<Vec<SupportedReserveLatestRow>> {
        let SupportedReserveLatestQuery {
            risk_baskets,
            liquidity_mint,
            markets,
            min_supply_usd,
            min_supply_apy,
            max_supply_apy,
            stale,
            observed_after,
            observed_before_or_at,
            limit,
        } = query;
        let mut builder = QueryBuilder::<Postgres>::new(format!(
            "SELECT l.observed_at, l.slot, l.reserve, l.market, l.market_name, \
             l.liquidity_mint, l.symbol, l.mint_decimals, l.market_price_usd, \
             l.supply_apy, l.borrow_apy, \
             l.total_supply_usd_estimate, l.reserve_last_update_stale \
             FROM {} sr \
             JOIN (SELECT DISTINCT ON (reserve) \
                        observed_at, event_id, slot, reserve, market, market_name, \
                        liquidity_mint, symbol, mint_decimals, market_price_usd, \
                        supply_apy, borrow_apy, total_supply_usd_estimate, \
                        reserve_last_update_stale \
                   FROM {}",
            self.qualified(SUPPORTED_RESERVES_TABLE),
            self.qualified(RESERVE_UPDATES_TABLE)
        ));
        let has_observed_after = observed_after.is_some();
        if let Some(observed_after) = observed_after {
            builder
                .push(" WHERE observed_at >= ")
                .push_bind(observed_after);
        }
        if let Some(observed_before_or_at) = observed_before_or_at {
            builder
                .push(if has_observed_after {
                    " AND observed_at <= "
                } else {
                    " WHERE observed_at <= "
                })
                .push_bind(observed_before_or_at);
        }
        builder.push(
            " ORDER BY reserve, event_id DESC) l ON l.reserve = sr.reserve \
                AND l.market = sr.market \
                AND l.liquidity_mint = sr.liquidity_mint \
             WHERE sr.active = true",
        );

        if !risk_baskets.is_empty() {
            builder.push(" AND (");
            for (index, risk_basket) in risk_baskets.iter().enumerate() {
                if index > 0 {
                    builder.push(" OR ");
                }
                builder
                    .push_bind(risk_basket)
                    .push(" = ANY(sr.risk_baskets)");
            }
            builder.push(")");
        }
        if let Some(liquidity_mint) = liquidity_mint {
            builder
                .push(" AND sr.liquidity_mint = ")
                .push_bind(liquidity_mint);
        }
        if !markets.is_empty() {
            builder.push(" AND sr.market = ANY(").push_bind(markets);
            builder.push(")");
        }
        if let Some(stale) = stale {
            builder
                .push(" AND l.reserve_last_update_stale = ")
                .push_bind(stale);
        }
        if let Some(min_supply_usd) = min_supply_usd {
            builder
                .push(" AND l.total_supply_usd_estimate > ")
                .push_bind(min_supply_usd);
        }
        if let Some(min_supply_apy) = min_supply_apy {
            builder
                .push(" AND l.supply_apy >= ")
                .push_bind(min_supply_apy);
        }
        if let Some(max_supply_apy) = max_supply_apy {
            builder
                .push(" AND l.supply_apy < ")
                .push_bind(max_supply_apy);
        }

        builder.push(" ORDER BY l.supply_apy DESC, l.observed_at DESC, l.reserve ASC");
        if let Some(limit) = limit {
            builder.push(" LIMIT ").push_bind(limit_i64(limit));
        }

        builder
            .build_query_as::<SupportedReserveLatestRow>()
            .fetch_all(&self.pool)
            .await
    }

    pub async fn reserve_history(
        &self,
        query: ReserveHistoryQuery,
    ) -> sqlx::Result<Vec<ReserveUpdateRow>> {
        let mut builder = self.select_update_rows_from(RESERVE_UPDATES_TABLE);
        push_update_filters(&mut builder, &query.filter);
        push_time_bounds(&mut builder, query.since, query.until);
        builder.push(" ORDER BY ");
        builder.push(query.order.time_sql());
        builder.push(" LIMIT ");
        builder.push_bind(limit_i64(query.limit));
        fetch_update_rows(builder, &self.pool).await
    }

    pub async fn reserve_updates_after(
        &self,
        cursor: &ReserveUpdateCursor,
        filter: ReserveUpdateFilter,
        limit: usize,
    ) -> sqlx::Result<Vec<ReserveUpdateRow>> {
        let mut builder = self.select_update_rows_from(RESERVE_UPDATES_TABLE);
        push_update_filters(&mut builder, &filter);
        push_where_or_and(&mut builder);
        builder
            .push("(observed_at, slot, reserve) > (")
            .push_bind(cursor.observed_at)
            .push(", ")
            .push_bind(cursor.slot)
            .push(", ")
            .push_bind(cursor.reserve.clone())
            .push(")");
        builder.push(" ORDER BY observed_at ASC, slot ASC, reserve ASC LIMIT ");
        builder.push_bind(limit_i64(limit));
        fetch_update_rows(builder, &self.pool).await
    }

    pub async fn reserve_updates_after_event_id(
        &self,
        cursor: ReserveUpdateEventIdCursor,
        filter: ReserveUpdateFilter,
        limit: usize,
    ) -> sqlx::Result<Vec<ReserveUpdateRow>> {
        let mut builder = self.select_update_rows_from(RESERVE_UPDATES_TABLE);
        push_update_filters(&mut builder, &filter);
        push_where_or_and(&mut builder);
        builder.push("event_id > ").push_bind(cursor.event_id);
        builder.push(" ORDER BY event_id ASC LIMIT ");
        builder.push_bind(limit_i64(limit));
        fetch_update_rows(builder, &self.pool).await
    }

    pub async fn latest_cursor(
        &self,
        filter: ReserveUpdateFilter,
    ) -> sqlx::Result<Option<ReserveUpdateCursor>> {
        let mut builder = QueryBuilder::<Postgres>::new(format!(
            "SELECT observed_at, slot, reserve FROM {}",
            self.qualified(RESERVE_UPDATES_TABLE)
        ));
        push_update_filters(&mut builder, &filter);
        builder.push(" ORDER BY observed_at DESC, slot DESC, reserve DESC LIMIT 1");

        builder
            .build_query_as::<ReserveUpdateCursor>()
            .fetch_optional(&self.pool)
            .await
    }

    pub async fn latest_event_id_cursor(
        &self,
        filter: ReserveUpdateFilter,
    ) -> sqlx::Result<Option<ReserveUpdateEventIdCursor>> {
        let mut builder = QueryBuilder::<Postgres>::new(format!(
            "SELECT event_id FROM {}",
            self.qualified(RESERVE_UPDATES_TABLE)
        ));
        push_update_filters(&mut builder, &filter);
        builder.push(" ORDER BY event_id DESC LIMIT 1");

        builder
            .build_query_as::<ReserveUpdateEventIdCursor>()
            .fetch_optional(&self.pool)
            .await
    }

    pub async fn reserve_window_stats(
        &self,
        query: ReserveWindowStatsQuery,
    ) -> sqlx::Result<Vec<ReserveWindowStats>> {
        let mut builder = QueryBuilder::<Postgres>::new(format!(
            "SELECT {RESERVE_WINDOW_STATS_COLUMNS} FROM {}",
            self.qualified(RESERVE_UPDATES_TABLE)
        ));
        push_update_filters(&mut builder, &query.filter);
        push_time_bounds(&mut builder, query.since, query.until);
        builder.push(
            " GROUP BY reserve, market, symbol ORDER BY last_observed_at DESC, reserve ASC LIMIT ",
        );
        builder.push_bind(limit_i64(query.limit));

        builder
            .build_query_as::<ReserveWindowStats>()
            .fetch_all(&self.pool)
            .await
    }

    pub async fn subscribe(
        &self,
        filter: ReserveUpdateFilter,
        options: SubscribeOptions,
    ) -> sqlx::Result<ReserveUpdateStream> {
        let mut listener = PgListener::connect_with(&self.pool).await?;
        listener.listen(&self.notify_channel).await?;

        let last_event_id_cursor = match options.start_after_event_id {
            Some(cursor) => Some(cursor),
            None if options.start_after.is_none() => {
                self.latest_event_id_cursor(filter.clone()).await?
            }
            None => None,
        };

        Ok(ReserveUpdateStream {
            client: self.clone(),
            listener,
            filter,
            last_event_id_cursor,
            legacy_last_cursor: options.start_after,
            pending: VecDeque::new(),
            catch_up_limit: options.catch_up_limit.max(1),
        })
    }

    fn select_update_rows_from(&self, table_or_view: &str) -> QueryBuilder<'static, Postgres> {
        QueryBuilder::<Postgres>::new(format!(
            "SELECT {RESERVE_UPDATE_ROW_COLUMNS} FROM {}",
            self.qualified(table_or_view)
        ))
    }

    fn select_latest_update_rows(&self) -> QueryBuilder<'static, Postgres> {
        QueryBuilder::<Postgres>::new(format!(
            "SELECT {RESERVE_UPDATE_ROW_COLUMNS} FROM {}",
            self.qualified(LATEST_RESERVE_UPDATES_VIEW)
        ))
    }

    fn qualified(&self, table_or_view: &str) -> String {
        format!(
            "{}.{}",
            quote_ident(&self.schema),
            quote_ident(table_or_view)
        )
    }
}

#[derive(Debug, Clone, Default, PartialEq)]
pub struct SupportedReserveLatestQuery {
    pub risk_baskets: Vec<String>,
    pub liquidity_mint: Option<String>,
    pub markets: Vec<String>,
    pub min_supply_usd: Option<f64>,
    pub min_supply_apy: Option<f64>,
    pub max_supply_apy: Option<f64>,
    pub stale: Option<bool>,
    /// Limits the candidate history before selecting the latest row per
    /// reserve. This enables Timescale chunk exclusion for freshness-bound
    /// consumers without allowing an older row to pass later quality filters.
    pub observed_after: Option<DateTime<Utc>>,
    /// Excludes future-dated observations from a captured snapshot.
    pub observed_before_or_at: Option<DateTime<Utc>>,
    pub limit: Option<usize>,
}

impl SupportedReserveLatestQuery {
    pub fn safe_stable(liquidity_mint: impl Into<String>) -> Self {
        Self {
            risk_baskets: vec!["safe".to_owned()],
            liquidity_mint: Some(liquidity_mint.into()),
            min_supply_usd: Some(100_000.0),
            min_supply_apy: Some(0.0),
            max_supply_apy: Some(0.5),
            stale: Some(false),
            ..Self::default()
        }
    }
}

pub struct ReserveUpdateStream {
    client: TimescaleRouterClient,
    listener: PgListener,
    filter: ReserveUpdateFilter,
    last_event_id_cursor: Option<ReserveUpdateEventIdCursor>,
    legacy_last_cursor: Option<ReserveUpdateCursor>,
    pending: VecDeque<ReserveUpdateRow>,
    catch_up_limit: usize,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct SupportedReserveLatestRow {
    pub observed_at: DateTime<Utc>,
    pub slot: i64,
    pub reserve: String,
    pub market: Option<String>,
    pub market_name: Option<String>,
    pub liquidity_mint: String,
    pub symbol: Option<String>,
    pub mint_decimals: i32,
    pub market_price_usd: f64,
    pub supply_apy: f64,
    pub borrow_apy: f64,
    pub total_supply_usd_estimate: f64,
    pub reserve_last_update_stale: bool,
}

impl<'r> FromRow<'r, PgRow> for SupportedReserveLatestRow {
    fn from_row(row: &'r PgRow) -> Result<Self, sqlx::Error> {
        Ok(Self {
            observed_at: row.try_get("observed_at")?,
            slot: row.try_get("slot")?,
            reserve: row.try_get("reserve")?,
            market: row.try_get("market")?,
            market_name: row.try_get("market_name")?,
            liquidity_mint: row.try_get("liquidity_mint")?,
            symbol: row.try_get("symbol")?,
            mint_decimals: row.try_get("mint_decimals")?,
            market_price_usd: row.try_get("market_price_usd")?,
            supply_apy: row.try_get("supply_apy")?,
            borrow_apy: row.try_get("borrow_apy")?,
            total_supply_usd_estimate: row.try_get("total_supply_usd_estimate")?,
            reserve_last_update_stale: row.try_get("reserve_last_update_stale")?,
        })
    }
}

impl ReserveUpdateStream {
    pub async fn next_update(&mut self) -> sqlx::Result<ReserveStreamItem> {
        loop {
            if let Some(row) = self.pending.pop_front() {
                self.remember(&row);
                return Ok(ReserveStreamItem {
                    notification: None,
                    row,
                });
            }

            if let Some(cursor) = self.last_event_id_cursor {
                let rows = self
                    .client
                    .reserve_updates_after_event_id(
                        cursor,
                        self.filter.clone(),
                        self.catch_up_limit,
                    )
                    .await?;
                if !rows.is_empty() {
                    self.pending = rows.into();
                    continue;
                }
            } else if let Some(cursor) = self.legacy_last_cursor.clone() {
                let rows = self
                    .client
                    .reserve_updates_after(&cursor, self.filter.clone(), self.catch_up_limit)
                    .await?;
                if !rows.is_empty() {
                    self.pending = rows.into();
                    continue;
                }
            }

            let notification = self.listener.recv().await?;
            let payload = serde_json::from_str::<ReserveUpdateNotification>(notification.payload())
                .map_err(|error| sqlx::Error::Protocol(error.to_string()))?;
            if self.last_event_id_cursor.is_none() && self.legacy_last_cursor.is_none() {
                self.last_event_id_cursor = Some(ReserveUpdateEventIdCursor {
                    event_id: payload.event_id.saturating_sub(1),
                });
            }
        }
    }

    fn remember(&mut self, row: &ReserveUpdateRow) {
        self.last_event_id_cursor = Some(row.event_id_cursor());
        self.legacy_last_cursor = Some(row.cursor());
    }
}

#[derive(Debug, Clone, Default, PartialEq)]
pub struct ReserveUpdateFilter {
    pub reserves: Vec<String>,
    pub symbols: Vec<String>,
    pub markets: Vec<String>,
    pub changed_fields: Vec<String>,
    pub min_supply_usd: Option<f64>,
    pub stale: Option<bool>,
}

impl ReserveUpdateFilter {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn with_reserves<I, S>(mut self, reserves: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.reserves = reserves.into_iter().map(Into::into).collect();
        self
    }

    pub fn with_symbols<I, S>(mut self, symbols: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.symbols = symbols
            .into_iter()
            .map(|symbol| symbol.into().to_ascii_uppercase())
            .collect();
        self
    }

    pub fn with_markets<I, S>(mut self, markets: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.markets = markets.into_iter().map(Into::into).collect();
        self
    }

    pub fn with_changed_fields<I, S>(mut self, changed_fields: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.changed_fields = changed_fields.into_iter().map(Into::into).collect();
        self
    }

    pub fn with_min_supply_usd(mut self, min_supply_usd: f64) -> Self {
        self.min_supply_usd = min_supply_usd
            .is_finite()
            .then_some(min_supply_usd.max(0.0));
        self
    }

    pub fn with_stale(mut self, stale: bool) -> Self {
        self.stale = Some(stale);
        self
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum QueryOrder {
    Asc,
    Desc,
}

impl QueryOrder {
    fn time_sql(self) -> &'static str {
        match self {
            Self::Asc => "observed_at ASC, slot ASC, reserve ASC",
            Self::Desc => "observed_at DESC, slot DESC, reserve DESC",
        }
    }
}

#[derive(Debug, Clone)]
pub struct ReserveHistoryQuery {
    pub filter: ReserveUpdateFilter,
    pub since: Option<DateTime<Utc>>,
    pub until: Option<DateTime<Utc>>,
    pub limit: usize,
    pub order: QueryOrder,
}

impl Default for ReserveHistoryQuery {
    fn default() -> Self {
        Self {
            filter: ReserveUpdateFilter::default(),
            since: None,
            until: None,
            limit: 500,
            order: QueryOrder::Asc,
        }
    }
}

#[derive(Debug, Clone)]
pub struct ReserveWindowStatsQuery {
    pub filter: ReserveUpdateFilter,
    pub since: Option<DateTime<Utc>>,
    pub until: Option<DateTime<Utc>>,
    pub limit: usize,
}

impl Default for ReserveWindowStatsQuery {
    fn default() -> Self {
        Self {
            filter: ReserveUpdateFilter::default(),
            since: None,
            until: None,
            limit: 500,
        }
    }
}

#[derive(Debug, Clone, Eq, PartialEq)]
pub struct SubscribeOptions {
    pub start_after_event_id: Option<ReserveUpdateEventIdCursor>,
    pub start_after: Option<ReserveUpdateCursor>,
    pub catch_up_limit: usize,
}

impl Default for SubscribeOptions {
    fn default() -> Self {
        Self {
            start_after_event_id: None,
            start_after: None,
            catch_up_limit: 500,
        }
    }
}

#[derive(Debug, Clone, Deserialize, Serialize, PartialEq)]
pub struct ReserveUpdateNotification {
    pub event_id: i64,
    pub observed_at: DateTime<Utc>,
    pub slot: i64,
    pub reserve: String,
    #[serde(default)]
    pub market: Option<String>,
    #[serde(default)]
    pub symbol: Option<String>,
    pub source: String,
    #[serde(default)]
    pub source_commitment: Option<String>,
    pub supply_apy: f64,
    pub borrow_apy: f64,
    pub utilization: f64,
    pub diff_changed: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, PartialOrd, Ord, Serialize)]
pub struct ReserveUpdateEventIdCursor {
    pub event_id: i64,
}

impl<'r> FromRow<'r, PgRow> for ReserveUpdateEventIdCursor {
    fn from_row(row: &'r PgRow) -> Result<Self, sqlx::Error> {
        Ok(Self {
            event_id: row.try_get("event_id")?,
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq, PartialOrd, Ord, Serialize)]
pub struct ReserveUpdateCursor {
    pub observed_at: DateTime<Utc>,
    pub slot: i64,
    pub reserve: String,
}

impl<'r> FromRow<'r, PgRow> for ReserveUpdateCursor {
    fn from_row(row: &'r PgRow) -> Result<Self, sqlx::Error> {
        Ok(Self {
            observed_at: row.try_get("observed_at")?,
            slot: row.try_get("slot")?,
            reserve: row.try_get("reserve")?,
        })
    }
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct ReserveUpdateRow {
    pub event_id: i64,
    pub observed_at: DateTime<Utc>,
    pub slot: i64,
    pub source: String,
    pub source_commitment: String,
    pub reserve: String,
    pub market: Option<String>,
    pub market_name: Option<String>,
    pub symbol: Option<String>,
    pub liquidity_mint: String,
    pub supply_apy: f64,
    pub borrow_apy: f64,
    pub utilization: f64,
    pub total_supply_usd_estimate: f64,
    pub total_borrow_usd_estimate: f64,
    pub reserve_last_update_stale: bool,
    pub diff_changed: bool,
    pub changed_fields: Vec<String>,
    pub diff_summary: String,
}

impl ReserveUpdateRow {
    pub fn event_id_cursor(&self) -> ReserveUpdateEventIdCursor {
        ReserveUpdateEventIdCursor {
            event_id: self.event_id,
        }
    }

    pub fn cursor(&self) -> ReserveUpdateCursor {
        ReserveUpdateCursor {
            observed_at: self.observed_at,
            slot: self.slot,
            reserve: self.reserve.clone(),
        }
    }
}

impl<'r> FromRow<'r, PgRow> for ReserveUpdateRow {
    fn from_row(row: &'r PgRow) -> Result<Self, sqlx::Error> {
        Ok(Self {
            event_id: row.try_get("event_id")?,
            observed_at: row.try_get("observed_at")?,
            slot: row.try_get("slot")?,
            source: row.try_get("source")?,
            source_commitment: row.try_get("source_commitment")?,
            reserve: row.try_get("reserve")?,
            market: row.try_get("market")?,
            market_name: row.try_get("market_name")?,
            symbol: row.try_get("symbol")?,
            liquidity_mint: row.try_get("liquidity_mint")?,
            supply_apy: row.try_get("supply_apy")?,
            borrow_apy: row.try_get("borrow_apy")?,
            utilization: row.try_get("utilization")?,
            total_supply_usd_estimate: row.try_get("total_supply_usd_estimate")?,
            total_borrow_usd_estimate: row.try_get("total_borrow_usd_estimate")?,
            reserve_last_update_stale: row.try_get("reserve_last_update_stale")?,
            diff_changed: row.try_get("diff_changed")?,
            changed_fields: row.try_get("changed_fields")?,
            diff_summary: row.try_get("diff_summary")?,
        })
    }
}

#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct ReserveStreamItem {
    pub notification: Option<ReserveUpdateNotification>,
    pub row: ReserveUpdateRow,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct ReserveWindowStats {
    pub reserve: String,
    pub market: Option<String>,
    pub symbol: Option<String>,
    pub update_count: i64,
    pub avg_supply_apy: f64,
    pub min_supply_apy: f64,
    pub max_supply_apy: f64,
    pub avg_borrow_apy: f64,
    pub avg_utilization: f64,
    pub avg_supply_usd: f64,
    pub avg_borrow_usd: f64,
    pub max_slot: i64,
    pub last_observed_at: DateTime<Utc>,
}

impl<'r> FromRow<'r, PgRow> for ReserveWindowStats {
    fn from_row(row: &'r PgRow) -> Result<Self, sqlx::Error> {
        Ok(Self {
            reserve: row.try_get("reserve")?,
            market: row.try_get("market")?,
            symbol: row.try_get("symbol")?,
            update_count: row.try_get("update_count")?,
            avg_supply_apy: row.try_get("avg_supply_apy")?,
            min_supply_apy: row.try_get("min_supply_apy")?,
            max_supply_apy: row.try_get("max_supply_apy")?,
            avg_borrow_apy: row.try_get("avg_borrow_apy")?,
            avg_utilization: row.try_get("avg_utilization")?,
            avg_supply_usd: row.try_get("avg_supply_usd")?,
            avg_borrow_usd: row.try_get("avg_borrow_usd")?,
            max_slot: row.try_get("max_slot")?,
            last_observed_at: row.try_get("last_observed_at")?,
        })
    }
}

async fn fetch_update_rows(
    mut builder: QueryBuilder<'_, Postgres>,
    pool: &PgPool,
) -> sqlx::Result<Vec<ReserveUpdateRow>> {
    builder
        .build_query_as::<ReserveUpdateRow>()
        .fetch_all(pool)
        .await
}

fn push_update_filters(builder: &mut QueryBuilder<'_, Postgres>, filter: &ReserveUpdateFilter) {
    if !filter.reserves.is_empty() {
        push_where_or_and(builder);
        builder
            .push("reserve = ANY(")
            .push_bind(filter.reserves.clone())
            .push(")");
    }
    if !filter.symbols.is_empty() {
        push_where_or_and(builder);
        builder
            .push("symbol = ANY(")
            .push_bind(filter.symbols.clone())
            .push(")");
    }
    if !filter.markets.is_empty() {
        push_where_or_and(builder);
        builder
            .push("market = ANY(")
            .push_bind(filter.markets.clone())
            .push(")");
    }
    if !filter.changed_fields.is_empty() {
        push_where_or_and(builder);
        builder
            .push("changed_fields @> ")
            .push_bind(filter.changed_fields.clone());
    }
    if let Some(min_supply_usd) = filter.min_supply_usd {
        push_where_or_and(builder);
        builder
            .push("total_supply_usd_estimate >= ")
            .push_bind(min_supply_usd);
    }
    if let Some(stale) = filter.stale {
        push_where_or_and(builder);
        builder
            .push("reserve_last_update_stale = ")
            .push_bind(stale);
    }
}

fn push_time_bounds(
    builder: &mut QueryBuilder<'_, Postgres>,
    since: Option<DateTime<Utc>>,
    until: Option<DateTime<Utc>>,
) {
    if let Some(since) = since {
        push_where_or_and(builder);
        builder.push("observed_at >= ").push_bind(since);
    }
    if let Some(until) = until {
        push_where_or_and(builder);
        builder.push("observed_at <= ").push_bind(until);
    }
}

fn push_where_or_and(builder: &mut QueryBuilder<'_, Postgres>) {
    if builder.sql().contains(" WHERE ") {
        builder.push(" AND ");
    } else {
        builder.push(" WHERE ");
    }
}

fn limit_i64(limit: usize) -> i64 {
    i64::try_from(limit.max(1)).unwrap_or(i64::MAX)
}

fn validate_identifier(value: &str, name: &str) -> sqlx::Result<()> {
    let mut chars = value.chars();
    let Some(first) = chars.next() else {
        return Err(sqlx::Error::Protocol(format!("{name} cannot be empty")));
    };
    if !(first == '_' || first.is_ascii_alphabetic()) {
        return Err(sqlx::Error::Protocol(format!(
            "{name} must start with an ASCII letter or underscore"
        )));
    }
    if !chars.all(|ch| ch == '_' || ch.is_ascii_alphanumeric()) {
        return Err(sqlx::Error::Protocol(format!(
            "{name} may only contain ASCII letters, digits, and underscores"
        )));
    }
    Ok(())
}

fn quote_ident(identifier: &str) -> String {
    format!("\"{}\"", identifier.replace('"', "\"\""))
}
