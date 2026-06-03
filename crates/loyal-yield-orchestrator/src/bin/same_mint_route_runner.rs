use std::env;

use loyal_yield_orchestrator::{
    run_same_mint_yield_routing_loop, ConfiguredSameMintRoutePreparer, MainnetSameMintExecutor,
    MainnetSameMintExecutorConfig, MainnetSameMintExecutorConfigError, NeonSqlConfig,
    OrchestratorError, OrchestratorStore, PlannerConfig, PolicySignerError, SameMintReserveApy,
    SameMintRouteLoopError, SameMintRoutePreparerError, SameMintRoutingLoopConfig,
};
use thiserror::Error;

const DATABASE_URL_ENV: &str = "DATABASE_URL";
const NEON_DATABASE_URL_ENV: &str = "NEON_DATABASE_URL";
const SAME_MINT_RESERVE_APYS_JSON_ENV: &str = "SAME_MINT_RESERVE_APYS_JSON";
const SAME_MINT_BATCH_SIZE_ENV: &str = "SAME_MINT_BATCH_SIZE";
const SAME_MINT_MIN_EDGE_BPS_ENV: &str = "SAME_MINT_MIN_EDGE_BPS";
const SAME_MINT_ESTIMATED_COST_LAMPORTS_ENV: &str = "SAME_MINT_ESTIMATED_COST_LAMPORTS";
const SAME_MINT_APPLY_MIGRATIONS_ENV: &str = "SAME_MINT_APPLY_MIGRATIONS";

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("{error}");
        std::process::exit(1);
    }
}

async fn run() -> Result<(), RunnerError> {
    let database_url = database_url()?;
    let store = OrchestratorStore::connect(NeonSqlConfig::new(database_url)).await?;
    if optional_bool_env(SAME_MINT_APPLY_MIGRATIONS_ENV)? {
        store.apply_migrations().await?;
    }

    let reserve_apys = reserve_apys_from_env()?;
    let preparer = ConfiguredSameMintRoutePreparer::from_env()?;
    let executor_config = MainnetSameMintExecutorConfig::from_env()?;
    let submit_batches = executor_config.submit_transactions;
    let executor = MainnetSameMintExecutor::from_env(executor_config, preparer)?;

    let report = run_same_mint_yield_routing_loop(
        &store,
        &executor,
        reserve_apys,
        SameMintRoutingLoopConfig {
            planner: PlannerConfig {
                min_edge_bps: optional_i64_env(SAME_MINT_MIN_EDGE_BPS_ENV)?.unwrap_or(1),
                estimated_cost_lamports: optional_i64_env(SAME_MINT_ESTIMATED_COST_LAMPORTS_ENV)?
                    .unwrap_or(0),
            },
            batch_size: optional_usize_env(SAME_MINT_BATCH_SIZE_ENV)?.unwrap_or(1),
            submit_batches,
        },
    )
    .await?;

    println!("{}", serde_json::to_string_pretty(&report)?);
    Ok(())
}

fn database_url() -> Result<String, RunnerError> {
    env::var(DATABASE_URL_ENV)
        .or_else(|_| env::var(NEON_DATABASE_URL_ENV))
        .map_err(|_| RunnerError::MissingDatabaseUrl)
}

fn reserve_apys_from_env() -> Result<Vec<SameMintReserveApy>, RunnerError> {
    let value = env::var(SAME_MINT_RESERVE_APYS_JSON_ENV).map_err(|_| RunnerError::MissingEnv {
        name: SAME_MINT_RESERVE_APYS_JSON_ENV,
    })?;
    let apys = serde_json::from_str::<Vec<SameMintReserveApy>>(&value)?;
    if apys.is_empty() {
        return Err(RunnerError::EmptyReserveApys);
    }
    Ok(apys)
}

fn optional_bool_env(name: &'static str) -> Result<bool, RunnerError> {
    match env::var(name) {
        Ok(value) => parse_bool(&value).ok_or(RunnerError::InvalidBool { name }),
        Err(env::VarError::NotPresent) => Ok(false),
        Err(env::VarError::NotUnicode(_)) => Err(RunnerError::InvalidUnicode { name }),
    }
}

fn optional_i64_env(name: &'static str) -> Result<Option<i64>, RunnerError> {
    match env::var(name) {
        Ok(value) => value
            .parse::<i64>()
            .map(Some)
            .map_err(|_| RunnerError::InvalidInteger { name }),
        Err(env::VarError::NotPresent) => Ok(None),
        Err(env::VarError::NotUnicode(_)) => Err(RunnerError::InvalidUnicode { name }),
    }
}

fn optional_usize_env(name: &'static str) -> Result<Option<usize>, RunnerError> {
    match env::var(name) {
        Ok(value) => value
            .parse::<usize>()
            .map(Some)
            .map_err(|_| RunnerError::InvalidInteger { name }),
        Err(env::VarError::NotPresent) => Ok(None),
        Err(env::VarError::NotUnicode(_)) => Err(RunnerError::InvalidUnicode { name }),
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
enum RunnerError {
    #[error("set DATABASE_URL or NEON_DATABASE_URL")]
    MissingDatabaseUrl,
    #[error("{name} is not set")]
    MissingEnv { name: &'static str },
    #[error("{name} must be valid unicode")]
    InvalidUnicode { name: &'static str },
    #[error("{name} must be true/false, 1/0, or yes/no")]
    InvalidBool { name: &'static str },
    #[error("{name} must be an integer")]
    InvalidInteger { name: &'static str },
    #[error("SAME_MINT_RESERVE_APYS_JSON must contain at least one reserve APY")]
    EmptyReserveApys,
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
    Json(#[from] serde_json::Error),
}
