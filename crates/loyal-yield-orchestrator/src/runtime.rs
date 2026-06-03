use chrono::Utc;
use loyal_yield_router::data_lake::{
    DataLakeSqlClient, DataLakeSqlConfig, ReserveUpdateFilter, ReserveUpdateRow,
};
use serde_json::json;
use std::env;
use std::time::Duration;
use tokio::time;

use crate::pipeline::{ReserveApySample, WorkerKind, DEFAULT_STRATEGY};
use crate::workers::{TargetWorker, VaultScanWorker};
use crate::{NeonSqlClient, NeonSqlConfig, OrchestratorError};

#[derive(Debug, Clone)]
pub struct WorkerRuntimeConfig {
    pub database_url: String,
    pub timescale_url: Option<String>,
    pub cluster: String,
    pub worker_id: String,
    pub apply_migrations: bool,
    pub poll_interval: Duration,
    pub max_connections: u32,
    pub target_min_supply_usd: f64,
    pub vault_fanout_limit: i64,
    pub reconcile_max_attempts: i32,
}

impl WorkerRuntimeConfig {
    pub fn from_env() -> Result<Self, OrchestratorError> {
        let database_url = env::var("DATABASE_URL")
            .or_else(|_| env::var("NEON_DATABASE_URL"))
            .map_err(|_| {
                OrchestratorError::Config(
                    "DATABASE_URL or NEON_DATABASE_URL is required".to_owned(),
                )
            })?;
        let timescale_url = env::var("TIMESCALE_DATABASE_URL")
            .or_else(|_| env::var("TIMESCALEDB_TEST_URL"))
            .ok();
        let cluster = env::var("LOYAL_YIELD_CLUSTER").unwrap_or_else(|_| "mainnet".to_owned());
        let worker_id = env::var("LOYAL_YIELD_WORKER_ID").unwrap_or_else(|_| {
            format!("loyal-yield-orchestrator-{}", Utc::now().timestamp_millis())
        });
        let apply_migrations = env_bool("LOYAL_YIELD_APPLY_MIGRATIONS", true);
        let poll_interval = Duration::from_millis(env_u64("LOYAL_YIELD_POLL_MS", 5_000));
        let max_connections = env_u64("LOYAL_YIELD_DB_MAX_CONNECTIONS", 5)
            .try_into()
            .unwrap_or(5);
        let target_min_supply_usd = env_f64("LOYAL_YIELD_MIN_SUPPLY_USD", 0.0);
        let vault_fanout_limit = env_u64("LOYAL_YIELD_VAULT_FANOUT_LIMIT", 500)
            .try_into()
            .unwrap_or(500);
        let reconcile_max_attempts = env_u64("LOYAL_YIELD_RECONCILE_MAX_ATTEMPTS", 5)
            .try_into()
            .unwrap_or(5);

        Ok(Self {
            database_url,
            timescale_url,
            cluster,
            worker_id,
            apply_migrations,
            poll_interval,
            max_connections,
            target_min_supply_usd,
            vault_fanout_limit,
            reconcile_max_attempts,
        })
    }
}

pub struct WorkerRuntime {
    store: NeonSqlClient,
    config: WorkerRuntimeConfig,
    data_lake: Option<DataLakeSqlClient>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RuntimeTick {
    pub targets_upserted: usize,
    pub reconcile_jobs_enqueued: u64,
    pub reconcile_leases_released: u64,
    pub reconcile_jobs_dead: u64,
}

impl WorkerRuntime {
    pub async fn connect(config: WorkerRuntimeConfig) -> Result<Self, OrchestratorError> {
        let store = NeonSqlClient::connect(
            NeonSqlConfig::new(config.database_url.clone())
                .with_max_connections(config.max_connections)
                .with_acquire_timeout(Duration::from_secs(10)),
        )
        .await?;
        if config.apply_migrations {
            store.apply_migrations().await?;
        }

        let data_lake = match config.timescale_url.as_ref() {
            Some(url) => Some(
                DataLakeSqlClient::connect(
                    DataLakeSqlConfig::new(url.clone()).with_max_connections(2),
                )
                .await
                .map_err(OrchestratorError::Sqlx)?,
            ),
            None => None,
        };

        Ok(Self {
            store,
            config,
            data_lake,
        })
    }

    pub fn from_parts(
        store: NeonSqlClient,
        config: WorkerRuntimeConfig,
        data_lake: Option<DataLakeSqlClient>,
    ) -> Self {
        Self {
            store,
            config,
            data_lake,
        }
    }

    pub async fn run_until_shutdown(&self) -> Result<(), OrchestratorError> {
        let mut interval = time::interval(self.config.poll_interval);
        loop {
            tokio::select! {
                _ = tokio::signal::ctrl_c() => {
                    return Ok(());
                }
                _ = interval.tick() => {
                    let tick = self.run_once().await?;
                    println!(
                        "worker_id={} targets_upserted={} reconcile_jobs_enqueued={} reconcile_leases_released={} reconcile_jobs_dead={}",
                        self.config.worker_id,
                        tick.targets_upserted,
                        tick.reconcile_jobs_enqueued,
                        tick.reconcile_leases_released,
                        tick.reconcile_jobs_dead
                    );
                }
            }
        }
    }

    pub async fn run_once(&self) -> Result<RuntimeTick, OrchestratorError> {
        let targets_upserted = self.run_target_worker_once().await?;
        let reconcile_jobs_enqueued = self.run_vault_scan_once().await?;
        let (reconcile_leases_released, reconcile_jobs_dead) = self
            .store
            .sweep_expired_reconcile_leases(self.config.reconcile_max_attempts)
            .await?;

        Ok(RuntimeTick {
            targets_upserted,
            reconcile_jobs_enqueued,
            reconcile_leases_released,
            reconcile_jobs_dead,
        })
    }

    async fn run_target_worker_once(&self) -> Result<usize, OrchestratorError> {
        let Some(data_lake) = self.data_lake.as_ref() else {
            return Ok(0);
        };

        let rows = data_lake
            .latest_reserves(ReserveUpdateFilter::new())
            .await
            .map_err(OrchestratorError::Sqlx)?;
        let samples = rows.iter().map(sample_from_row).collect::<Vec<_>>();
        let worker = TargetWorker::new(self.config.cluster.clone())
            .with_min_supply_usd(self.config.target_min_supply_usd);
        let targets = worker.select_targets(&samples);
        let mut count = 0;
        for target in targets {
            let observed_at = Some(target.observed_at);
            let cursor = target.source_cursor.clone();
            self.store.upsert_reserve_target(target).await?;
            self.store
                .upsert_worker_cursor(
                    WorkerKind::Target.as_str(),
                    &self.config.cluster,
                    DEFAULT_STRATEGY,
                    cursor,
                    observed_at,
                )
                .await?;
            count += 1;
        }
        Ok(count)
    }

    async fn run_vault_scan_once(&self) -> Result<u64, OrchestratorError> {
        let targets = self
            .store
            .reserve_targets(&self.config.cluster, DEFAULT_STRATEGY)
            .await?;
        let mut enqueued = 0;
        for target in targets {
            let limit = VaultScanWorker::fanout_limit(
                self.config.vault_fanout_limit as usize,
                self.config.vault_fanout_limit as usize,
            ) as i64;
            enqueued += self
                .store
                .enqueue_reconcile_jobs_for_target(&target, limit)
                .await?;
        }
        Ok(enqueued)
    }
}

fn sample_from_row(row: &ReserveUpdateRow) -> ReserveApySample {
    ReserveApySample {
        reserve: row.reserve.clone(),
        market: row.market.clone(),
        liquidity_mint: row.liquidity_mint.clone(),
        supply_apy_bps: apy_to_bps(row.supply_apy),
        total_supply_usd_estimate: row.total_supply_usd_estimate,
        stale: row.reserve_last_update_stale,
        observed_slot: Some(row.slot),
        observed_at: row.observed_at,
        source_cursor: json!({
            "observed_at": row.observed_at,
            "slot": row.slot,
            "reserve": row.reserve
        }),
    }
}

fn apy_to_bps(apy: f64) -> i64 {
    if !apy.is_finite() {
        return -1;
    }
    (apy * 10_000.0).round() as i64
}

fn env_bool(name: &str, default: bool) -> bool {
    env::var(name)
        .ok()
        .and_then(|value| match value.as_str() {
            "1" | "true" | "TRUE" | "yes" | "YES" => Some(true),
            "0" | "false" | "FALSE" | "no" | "NO" => Some(false),
            _ => None,
        })
        .unwrap_or(default)
}

fn env_u64(name: &str, default: u64) -> u64 {
    env::var(name)
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .unwrap_or(default)
}

fn env_f64(name: &str, default: f64) -> f64 {
    env::var(name)
        .ok()
        .and_then(|value| value.parse::<f64>().ok())
        .filter(|value| value.is_finite())
        .unwrap_or(default)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn apy_to_bps_converts_fractional_apy() {
        assert_eq!(apy_to_bps(0.0525), 525);
    }

    #[test]
    fn apy_to_bps_rejects_non_finite_values() {
        assert_eq!(apy_to_bps(f64::NAN), -1);
    }
}
