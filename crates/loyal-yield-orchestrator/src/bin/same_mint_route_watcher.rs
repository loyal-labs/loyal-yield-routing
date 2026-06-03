use std::{env, time::Duration};

use loyal_yield_orchestrator::{
    comma_list, latest_same_mint_apys, run_same_mint_yield_routing_loop,
    ConfiguredSameMintRoutePreparer, MainnetSameMintExecutor, MainnetSameMintExecutorConfig,
    MainnetSameMintExecutorConfigError, NeonSqlConfig, OrchestratorError, OrchestratorStore,
    PlannerConfig, PolicySignerError, SameMintRouteLoopError, SameMintRoutePreparerError,
    SameMintRoutingLoopConfig, TimescaleSameMintError, SAME_MINT_TIMESCALE_CHANGED_FIELDS_ENV,
    SAME_MINT_TIMESCALE_INCLUDE_STALE_ENV, SAME_MINT_TIMESCALE_MARKETS_ENV,
    SAME_MINT_TIMESCALE_MIN_SUPPLY_USD_ENV, SAME_MINT_TIMESCALE_RESERVES_ENV,
    SAME_MINT_TIMESCALE_SYMBOLS_ENV, TIMESCALEDB_NOTIFY_CHANNEL_ENV, TIMESCALEDB_SCHEMA_ENV,
    TIMESCALEDB_URL_ENV,
};
use loyal_yield_router::timescale::{
    ReserveUpdateFilter, SubscribeOptions, TimescaleRouterClient, TimescaleRouterClientConfig,
};
use thiserror::Error;
use tokio::time::timeout;

const DATABASE_URL_ENV: &str = "DATABASE_URL";
const NEON_DATABASE_URL_ENV: &str = "NEON_DATABASE_URL";
const SAME_MINT_APPLY_MIGRATIONS_ENV: &str = "SAME_MINT_APPLY_MIGRATIONS";
const SAME_MINT_BATCH_SIZE_ENV: &str = "SAME_MINT_BATCH_SIZE";
const SAME_MINT_MIN_EDGE_BPS_ENV: &str = "SAME_MINT_MIN_EDGE_BPS";
const SAME_MINT_ESTIMATED_COST_LAMPORTS_ENV: &str = "SAME_MINT_ESTIMATED_COST_LAMPORTS";
const SAME_MINT_WATCH_ONCE_ENV: &str = "SAME_MINT_WATCH_ONCE";
const SAME_MINT_WATCH_TIMEOUT_SECS_ENV: &str = "SAME_MINT_WATCH_TIMEOUT_SECS";
const DEFAULT_WATCH_TIMEOUT_SECS: u64 = 60;

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("{error}");
        std::process::exit(1);
    }
}

async fn run() -> Result<(), WatcherError> {
    let database_url = database_url()?;
    let store = OrchestratorStore::connect(NeonSqlConfig::new(database_url)).await?;
    if optional_bool_env(SAME_MINT_APPLY_MIGRATIONS_ENV)?.unwrap_or(false) {
        store.apply_migrations().await?;
    }

    let timescale = TimescaleRouterClient::connect(timescale_config_from_env()?).await?;
    let filter = reserve_update_filter_from_env()?;
    let preparer = ConfiguredSameMintRoutePreparer::from_env()?;
    let executor_config = MainnetSameMintExecutorConfig::from_env()?;
    let submit_batches = executor_config.submit_transactions;
    let executor = MainnetSameMintExecutor::from_env(executor_config, preparer)?;
    let loop_config = loop_config(submit_batches)?;
    let watch_once = optional_bool_env(SAME_MINT_WATCH_ONCE_ENV)?.unwrap_or(false);
    let timeout_duration = Duration::from_secs(
        optional_u64_env(SAME_MINT_WATCH_TIMEOUT_SECS_ENV)?.unwrap_or(DEFAULT_WATCH_TIMEOUT_SECS),
    );

    let mut stream = timescale
        .subscribe(filter.clone(), SubscribeOptions::default())
        .await?;
    loop {
        let item = timeout(timeout_duration, stream.next_update())
            .await
            .map_err(|_| WatcherError::Timeout(timeout_duration))??;
        eprintln!(
            "received same-mint APY update reserve={} slot={} symbol={:?}",
            item.row.reserve, item.row.slot, item.row.symbol
        );

        let reserve_apys = latest_same_mint_apys(&timescale, filter.clone()).await?;
        let report =
            run_same_mint_yield_routing_loop(&store, &executor, reserve_apys, loop_config.clone())
                .await?;
        println!("{}", serde_json::to_string_pretty(&report)?);

        if watch_once {
            return Ok(());
        }
    }
}

fn database_url() -> Result<String, WatcherError> {
    env::var(DATABASE_URL_ENV)
        .or_else(|_| env::var(NEON_DATABASE_URL_ENV))
        .map_err(|_| WatcherError::MissingDatabaseUrl)
}

fn timescale_config_from_env() -> Result<TimescaleRouterClientConfig, WatcherError> {
    let url = env::var(TIMESCALEDB_URL_ENV).map_err(|_| WatcherError::MissingEnv {
        name: TIMESCALEDB_URL_ENV,
    })?;
    let mut config = TimescaleRouterClientConfig::new(url);
    if let Ok(schema) = env::var(TIMESCALEDB_SCHEMA_ENV) {
        config = config.with_schema(schema);
    }
    if let Ok(channel) = env::var(TIMESCALEDB_NOTIFY_CHANNEL_ENV) {
        config = config.with_notify_channel(channel);
    }
    Ok(config)
}

fn reserve_update_filter_from_env() -> Result<ReserveUpdateFilter, WatcherError> {
    let mut filter = ReserveUpdateFilter::new()
        .with_reserves(comma_list(env::var(SAME_MINT_TIMESCALE_RESERVES_ENV).ok()))
        .with_symbols(comma_list(env::var(SAME_MINT_TIMESCALE_SYMBOLS_ENV).ok()))
        .with_markets(comma_list(env::var(SAME_MINT_TIMESCALE_MARKETS_ENV).ok()))
        .with_changed_fields(comma_list(
            env::var(SAME_MINT_TIMESCALE_CHANGED_FIELDS_ENV).ok(),
        ));

    if let Some(min_supply_usd) = optional_f64_env(SAME_MINT_TIMESCALE_MIN_SUPPLY_USD_ENV)? {
        filter = filter.with_min_supply_usd(min_supply_usd);
    }
    if !optional_bool_env(SAME_MINT_TIMESCALE_INCLUDE_STALE_ENV)?.unwrap_or(false) {
        filter = filter.with_stale(false);
    }

    Ok(filter)
}

fn loop_config(submit_batches: bool) -> Result<SameMintRoutingLoopConfig, WatcherError> {
    Ok(SameMintRoutingLoopConfig {
        planner: PlannerConfig {
            min_edge_bps: optional_i64_env(SAME_MINT_MIN_EDGE_BPS_ENV)?.unwrap_or(1),
            estimated_cost_lamports: optional_i64_env(SAME_MINT_ESTIMATED_COST_LAMPORTS_ENV)?
                .unwrap_or(0),
        },
        batch_size: optional_usize_env(SAME_MINT_BATCH_SIZE_ENV)?.unwrap_or(1),
        submit_batches,
    })
}

fn optional_bool_env(name: &'static str) -> Result<Option<bool>, WatcherError> {
    match env::var(name) {
        Ok(value) => parse_bool(&value)
            .ok_or(WatcherError::InvalidBool { name })
            .map(Some),
        Err(env::VarError::NotPresent) => Ok(None),
        Err(env::VarError::NotUnicode(_)) => Err(WatcherError::InvalidUnicode { name }),
    }
}

fn optional_i64_env(name: &'static str) -> Result<Option<i64>, WatcherError> {
    optional_parse_env(name)
}

fn optional_u64_env(name: &'static str) -> Result<Option<u64>, WatcherError> {
    optional_parse_env(name)
}

fn optional_usize_env(name: &'static str) -> Result<Option<usize>, WatcherError> {
    optional_parse_env(name)
}

fn optional_f64_env(name: &'static str) -> Result<Option<f64>, WatcherError> {
    optional_parse_env(name)
}

fn optional_parse_env<T>(name: &'static str) -> Result<Option<T>, WatcherError>
where
    T: std::str::FromStr,
{
    match env::var(name) {
        Ok(value) => value
            .parse::<T>()
            .map(Some)
            .map_err(|_| WatcherError::InvalidNumber { name }),
        Err(env::VarError::NotPresent) => Ok(None),
        Err(env::VarError::NotUnicode(_)) => Err(WatcherError::InvalidUnicode { name }),
    }
}

fn parse_bool(value: &str) -> Option<bool> {
    match value.trim().to_ascii_lowercase().as_str() {
        "1" | "true" | "yes" => Some(true),
        "0" | "false" | "no" => Some(false),
        _ => None,
    }
}

#[derive(Debug, Error)]
enum WatcherError {
    #[error("set DATABASE_URL or NEON_DATABASE_URL")]
    MissingDatabaseUrl,
    #[error("{name} is not set")]
    MissingEnv { name: &'static str },
    #[error("{name} must be valid unicode")]
    InvalidUnicode { name: &'static str },
    #[error("{name} must be true/false, 1/0, or yes/no")]
    InvalidBool { name: &'static str },
    #[error("{name} must be a number")]
    InvalidNumber { name: &'static str },
    #[error("timed out after {0:?} waiting for a TimescaleDB APY update")]
    Timeout(Duration),
    #[error(transparent)]
    Orchestrator(#[from] OrchestratorError),
    #[error(transparent)]
    ExecutorConfig(#[from] MainnetSameMintExecutorConfigError),
    #[error(transparent)]
    Signer(#[from] PolicySignerError),
    #[error(transparent)]
    Preparer(#[from] SameMintRoutePreparerError),
    #[error(transparent)]
    Loop(#[from] SameMintRouteLoopError),
    #[error(transparent)]
    Timescale(#[from] TimescaleSameMintError),
    #[error(transparent)]
    Sqlx(#[from] sqlx::Error),
    #[error(transparent)]
    Json(#[from] serde_json::Error),
}
