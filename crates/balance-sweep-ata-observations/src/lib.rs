use std::{error::Error, fmt, str::FromStr, time::Duration};

use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use futures_util::future::BoxFuture;
use loyal_yield_orchestrator::{BalanceSweepTargetId, WalletAtaBalanceUpdateInput};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};
use sqlx::{
    postgres::{PgConnectOptions, PgPoolOptions},
    PgPool, Row,
};

#[derive(Debug, Clone)]
pub struct TimescaleAtaConfig {
    pub url: String,
    pub max_connections: u32,
    pub acquire_timeout: Duration,
    pub stream: TimescaleAtaStream,
}

impl TimescaleAtaConfig {
    pub fn new(url: impl Into<String>) -> Self {
        Self {
            url: url.into(),
            max_connections: 5,
            acquire_timeout: Duration::from_secs(5),
            stream: TimescaleAtaStream::Production,
        }
    }

    pub fn with_stream(mut self, stream: TimescaleAtaStream) -> Self {
        self.stream = stream;
        self
    }
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum TimescaleAtaStream {
    #[default]
    Production,
    Staging,
}

impl TimescaleAtaStream {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Production => "production",
            Self::Staging => "staging",
        }
    }

    fn schema(self) -> &'static str {
        match self {
            Self::Production => "loyal_prod",
            Self::Staging => "loyal_staging",
        }
    }
}

impl fmt::Display for TimescaleAtaStream {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

impl FromStr for TimescaleAtaStream {
    type Err = TimescaleAtaStreamParseError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        match value.trim().to_ascii_lowercase().as_str() {
            "production" | "prod" => Ok(Self::Production),
            "staging" | "stage" => Ok(Self::Staging),
            _ => Err(TimescaleAtaStreamParseError(value.to_owned())),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TimescaleAtaStreamParseError(String);

impl fmt::Display for TimescaleAtaStreamParseError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            formatter,
            "unsupported BALANCE_SWEEP_ATA_STREAM {:?}; expected production or staging",
            self.0
        )
    }
}

impl Error for TimescaleAtaStreamParseError {}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct BalanceSweepAtaObservation {
    pub target_id: BalanceSweepTargetId,
    pub cluster: String,
    pub wallet: String,
    pub wallet_usdc_ata: String,
    pub vault_pubkey: String,
    pub vault_usdc_ata: String,
    pub amount_raw: u64,
    pub owner: Option<String>,
    pub mint: String,
    pub slot: u64,
    pub observed_at: DateTime<Utc>,
    pub source: String,
    pub source_commitment: String,
    pub txn_signature: Option<String>,
    pub account_data_hash: String,
    pub raw_account_data_base64: String,
    pub raw_evidence: serde_json::Value,
    pub received_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct BalanceSweepAtaObservationEvent {
    pub event_id: i64,
    pub observation: BalanceSweepAtaObservation,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ObservationInsertOutcome {
    pub event_id: i64,
    pub inserted: bool,
}

pub trait AtaObservationSink {
    fn record_observation(
        &self,
        observation: BalanceSweepAtaObservation,
    ) -> BoxFuture<'_, Result<ObservationInsertOutcome>>;
}

#[derive(Clone)]
pub struct TimescaleAtaObservationSink {
    pool: PgPool,
    stream: TimescaleAtaStream,
}

impl TimescaleAtaObservationSink {
    pub async fn connect(config: TimescaleAtaConfig) -> Result<Self> {
        let options = PgConnectOptions::from_str(&config.url)?.statement_cache_capacity(0);
        let pool = PgPoolOptions::new()
            .max_connections(config.max_connections)
            .acquire_timeout(config.acquire_timeout)
            .connect_with(options)
            .await?;
        Ok(Self {
            pool,
            stream: config.stream,
        })
    }

    pub fn from_pool(pool: PgPool) -> Self {
        Self::from_pool_with_stream(pool, TimescaleAtaStream::Production)
    }

    pub fn from_pool_with_stream(pool: PgPool, stream: TimescaleAtaStream) -> Self {
        Self { pool, stream }
    }

    pub fn stream(&self) -> TimescaleAtaStream {
        self.stream
    }

    pub async fn latest_observations(
        &self,
        limit: i64,
    ) -> Result<Vec<BalanceSweepAtaObservationEvent>> {
        let query = format!(
            r#"
            SELECT
                event_id, target_id, cluster, wallet, wallet_usdc_ata, vault_pubkey, vault_usdc_ata,
                amount_raw, owner, mint, slot, observed_at, source, source_commitment,
                txn_signature, account_data_hash, raw_account_data_base64, raw_evidence, received_at
            FROM {}.latest_balance_sweep_wallet_ata_observations
            ORDER BY event_id
            LIMIT $1
            "#,
            self.stream.schema()
        );
        let rows = sqlx::query(&query)
            .bind(limit)
            .fetch_all(&self.pool)
            .await?;

        rows.into_iter()
            .map(balance_sweep_observation_event_from_row)
            .collect()
    }

    pub async fn observations_after_event_id(
        &self,
        last_event_id: i64,
        limit: i64,
    ) -> Result<Vec<BalanceSweepAtaObservationEvent>> {
        let query = format!(
            r#"
            SELECT
                event_id, target_id, cluster, wallet, wallet_usdc_ata, vault_pubkey, vault_usdc_ata,
                amount_raw, owner, mint, slot, observed_at, source, source_commitment,
                txn_signature, account_data_hash, raw_account_data_base64, raw_evidence, received_at
            FROM {}.balance_sweep_wallet_ata_observations
            WHERE event_id > $1
            ORDER BY event_id ASC
            LIMIT $2
            "#,
            self.stream.schema()
        );
        let rows = sqlx::query(&query)
            .bind(last_event_id)
            .bind(limit)
            .fetch_all(&self.pool)
            .await?;

        rows.into_iter()
            .map(balance_sweep_observation_event_from_row)
            .collect()
    }
}

impl AtaObservationSink for TimescaleAtaObservationSink {
    fn record_observation(
        &self,
        observation: BalanceSweepAtaObservation,
    ) -> BoxFuture<'_, Result<ObservationInsertOutcome>> {
        Box::pin(async move { insert_observation(&self.pool, self.stream, observation).await })
    }
}

pub fn observation_dedupe_key(observation: &BalanceSweepAtaObservation) -> String {
    let mut hasher = Sha256::new();
    hasher.update(observation.source_commitment.as_bytes());
    hasher.update(b":");
    hasher.update(observation.wallet_usdc_ata.as_bytes());
    hasher.update(b":");
    hasher.update(observation.slot.to_string().as_bytes());
    hasher.update(b":");
    hasher.update(observation.account_data_hash.as_bytes());
    let digest = hasher.finalize();
    digest.iter().map(|byte| format!("{byte:02x}")).collect()
}

pub fn observation_to_wallet_balance_update(
    observation: BalanceSweepAtaObservation,
) -> WalletAtaBalanceUpdateInput {
    let raw_evidence = with_raw_account_data_base64(
        observation.raw_evidence,
        observation.raw_account_data_base64,
    );
    WalletAtaBalanceUpdateInput {
        target_id: observation.target_id,
        wallet: observation.wallet,
        wallet_usdc_ata: observation.wallet_usdc_ata,
        amount_raw: observation.amount_raw,
        owner: observation.owner,
        mint: observation.mint,
        observed_slot: observation.slot,
        observed_at: Some(observation.observed_at),
        source: observation.source,
        source_commitment: observation.source_commitment,
        txn_signature: observation.txn_signature,
        account_data_hash: Some(observation.account_data_hash),
        raw_evidence,
    }
}

fn with_raw_account_data_base64(
    raw_evidence: serde_json::Value,
    raw_account_data_base64: String,
) -> serde_json::Value {
    match raw_evidence {
        serde_json::Value::Object(mut object) => {
            object.insert(
                "raw_account_data_base64".to_owned(),
                serde_json::Value::String(raw_account_data_base64),
            );
            serde_json::Value::Object(object)
        }
        other => json!({
            "raw_evidence": other,
            "raw_account_data_base64": raw_account_data_base64,
        }),
    }
}

async fn insert_observation(
    pool: &PgPool,
    stream: TimescaleAtaStream,
    observation: BalanceSweepAtaObservation,
) -> Result<ObservationInsertOutcome> {
    let dedupe_key = observation_dedupe_key(&observation);
    let amount_raw = i64::try_from(observation.amount_raw).context("amount_raw exceeds i64")?;
    let slot = i64::try_from(observation.slot).context("slot exceeds i64")?;
    let schema = stream.schema();
    let event_sequence = format!("{schema}.balance_sweep_wallet_ata_observation_event_id_seq");
    let query = format!(
        r#"
        WITH candidate AS (
            SELECT nextval('{event_sequence}'::regclass) AS event_id
        ),
        claimed AS (
            INSERT INTO {schema}.balance_sweep_wallet_ata_observation_dedupe
                (dedupe_key, event_id, source_commitment, wallet_usdc_ata, slot, account_data_hash)
            SELECT $1, candidate.event_id, $14, $5, $11, $15
            FROM candidate
            ON CONFLICT (dedupe_key) DO NOTHING
            RETURNING event_id
        ),
        inserted AS (
            INSERT INTO {schema}.balance_sweep_wallet_ata_observations
                (event_id, cluster, target_id, wallet, wallet_usdc_ata, vault_pubkey, vault_usdc_ata,
                 amount_raw, owner, mint, slot, observed_at, source, source_commitment,
                 txn_signature, account_data_hash, raw_account_data_base64, raw_evidence, received_at)
            SELECT claimed.event_id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $19, $15, $16, $17, $18
            FROM claimed
            RETURNING event_id
        )
        SELECT event_id, TRUE AS inserted FROM inserted
        UNION ALL
        SELECT event_id, FALSE AS inserted
        FROM {schema}.balance_sweep_wallet_ata_observation_dedupe
        WHERE dedupe_key = $1
          AND NOT EXISTS (SELECT 1 FROM inserted)
        LIMIT 1
        "#,
    );
    let row = sqlx::query(&query)
        .bind(&dedupe_key)
        .bind(&observation.cluster)
        .bind(observation.target_id.as_i64())
        .bind(&observation.wallet)
        .bind(&observation.wallet_usdc_ata)
        .bind(&observation.vault_pubkey)
        .bind(&observation.vault_usdc_ata)
        .bind(amount_raw)
        .bind(observation.owner.as_deref())
        .bind(&observation.mint)
        .bind(slot)
        .bind(observation.observed_at)
        .bind(&observation.source)
        .bind(&observation.source_commitment)
        .bind(&observation.account_data_hash)
        .bind(&observation.raw_account_data_base64)
        .bind(&observation.raw_evidence)
        .bind(observation.received_at)
        .bind(observation.txn_signature.as_deref())
        .fetch_one(pool)
        .await?;

    Ok(ObservationInsertOutcome {
        event_id: row.try_get("event_id")?,
        inserted: row.try_get("inserted")?,
    })
}

fn balance_sweep_observation_event_from_row(
    row: sqlx::postgres::PgRow,
) -> Result<BalanceSweepAtaObservationEvent> {
    let amount_raw: i64 = row.try_get("amount_raw")?;
    let slot: i64 = row.try_get("slot")?;
    Ok(BalanceSweepAtaObservationEvent {
        event_id: row.try_get("event_id")?,
        observation: BalanceSweepAtaObservation {
            target_id: BalanceSweepTargetId(row.try_get("target_id")?),
            cluster: row.try_get("cluster")?,
            wallet: row.try_get("wallet")?,
            wallet_usdc_ata: row.try_get("wallet_usdc_ata")?,
            vault_pubkey: row.try_get("vault_pubkey")?,
            vault_usdc_ata: row.try_get("vault_usdc_ata")?,
            amount_raw: amount_raw
                .try_into()
                .context("Timescale amount_raw was negative or too large")?,
            owner: row.try_get("owner")?,
            mint: row.try_get("mint")?,
            slot: slot
                .try_into()
                .context("Timescale slot was negative or too large")?,
            observed_at: row.try_get("observed_at")?,
            source: row.try_get("source")?,
            source_commitment: row.try_get("source_commitment")?,
            txn_signature: row.try_get("txn_signature")?,
            account_data_hash: row.try_get("account_data_hash")?,
            raw_account_data_base64: row.try_get("raw_account_data_base64")?,
            raw_evidence: row.try_get("raw_evidence")?,
            received_at: row.try_get("received_at")?,
        },
    })
}
