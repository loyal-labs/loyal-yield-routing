use std::{collections::BTreeMap, fs, path::PathBuf, time::Duration};

use chrono::Utc;
use clap::Parser;
use loyal_yield_orchestrator::{
    row_is_fresh, yield_router_keypair_from_env, KaminoReserveMetadataResolver, NeonSqlConfig,
    OrchestratorStore, SameMintLoopConfig, YieldReserveTarget, YieldRouteRunConfig,
    YieldRoutingLoop,
};
use loyal_yield_router::timescale::{
    ReserveUpdateFilter, ReserveUpdateRow, SubscribeOptions, TimescaleRouterClient,
    TimescaleRouterClientConfig,
};
use serde::Deserialize;
use serde_json::json;
use solana_client::rpc_client::RpcClient;

#[derive(Debug, Parser)]
#[command(about = "Run the live Loyal yield-routing worker from Timescale reserve state")]
struct Cli {
    #[arg(long, env = "NEON_DATABASE_URL")]
    postgres_url: String,
    #[arg(long, env = "TIMESCALEDB_URL")]
    timescaledb_url: String,
    #[arg(long, env = "SOLANA_RPC_URL")]
    rpc_url: String,
    #[arg(long)]
    dry_run: bool,
    #[arg(long)]
    cluster: Option<String>,
    #[arg(long, default_value_t = 1)]
    min_edge_bps: i64,
    #[arg(long, default_value_t = 8)]
    batch_size: usize,
    #[arg(long, default_value_t = 50)]
    max_vaults: usize,
    #[arg(long, default_value_t = 2)]
    debounce_secs: u64,
    #[arg(long, default_value_t = 900)]
    max_apy_age_secs: u64,
    #[arg(long)]
    once: bool,
    #[arg(long, env = "YIELD_ROUTE_CONFIG_JSON")]
    config_json: Option<String>,
    #[arg(long, env = "YIELD_ROUTE_CONFIG_FILE")]
    config_file: Option<PathBuf>,
}

#[derive(Debug, Clone, Default, Deserialize)]
struct TestingOverrides {
    #[serde(default)]
    targets: Vec<YieldReserveTarget>,
    #[serde(default)]
    estimated_cost_lamports: i64,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse();
    let overrides = load_testing_overrides(&cli)?;

    let store = OrchestratorStore::connect(
        NeonSqlConfig::new(cli.postgres_url.clone())
            .with_max_connections(2)
            .with_acquire_timeout(Duration::from_secs(10)),
    )
    .await?;
    store.apply_migrations().await?;

    let timescale = TimescaleRouterClient::connect(
        TimescaleRouterClientConfig::new(cli.timescaledb_url.clone()).with_max_connections(2),
    )
    .await?;
    let rpc = RpcClient::new(cli.rpc_url.clone());
    let signer = yield_router_keypair_from_env()?;
    let mut resolver = KaminoReserveMetadataResolver::default();
    let filter = ReserveUpdateFilter::new();
    let latest_cursor = timescale.latest_event_id_cursor(filter.clone()).await?;

    let startup_rows = latest_fresh_rows(
        &timescale,
        filter.clone(),
        Duration::from_secs(cli.max_apy_age_secs),
    )
    .await?;
    let mut last_signature = rows_signature(&startup_rows);
    let startup_report = evaluate_rows(
        &cli,
        &overrides,
        &store,
        &rpc,
        &signer,
        &mut resolver,
        startup_rows,
    )
    .await?;
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "phase": "startup",
            "startAfterEventId": latest_cursor.map(|cursor| cursor.event_id),
            "report": startup_report,
        }))?
    );

    if cli.once {
        return Ok(());
    }

    let mut stream = timescale
        .subscribe(
            filter.clone(),
            SubscribeOptions {
                start_after_event_id: latest_cursor,
                start_after: None,
                catch_up_limit: cli.batch_size.max(1),
            },
        )
        .await?;
    let debounce = Duration::from_secs(cli.debounce_secs);

    loop {
        let wakeup = stream.next_update().await?;
        tokio::time::sleep(debounce).await;
        let rows = latest_fresh_rows(
            &timescale,
            filter.clone(),
            Duration::from_secs(cli.max_apy_age_secs),
        )
        .await?;
        let signature = rows_signature(&rows);
        if signature == last_signature {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "phase": "wakeup",
                    "wakeupEventId": wakeup.row.event_id,
                    "skipped": "latest APY snapshot unchanged",
                }))?
            );
            continue;
        }
        last_signature = signature;

        let report =
            evaluate_rows(&cli, &overrides, &store, &rpc, &signer, &mut resolver, rows).await?;
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "phase": "wakeup",
                "wakeupEventId": wakeup.row.event_id,
                "report": report,
            }))?
        );
    }
}

async fn latest_fresh_rows(
    timescale: &TimescaleRouterClient,
    filter: ReserveUpdateFilter,
    max_age: Duration,
) -> sqlx::Result<Vec<ReserveUpdateRow>> {
    let now = Utc::now();
    let rows = timescale.latest_reserves(filter).await?;
    Ok(rows
        .into_iter()
        .filter(|row| row_is_fresh(row, max_age, now))
        .collect())
}

async fn evaluate_rows(
    cli: &Cli,
    overrides: &TestingOverrides,
    store: &OrchestratorStore,
    rpc: &RpcClient,
    signer: &solana_sdk::signature::Keypair,
    resolver: &mut KaminoReserveMetadataResolver,
    rows: Vec<ReserveUpdateRow>,
) -> Result<loyal_yield_orchestrator::YieldRouteLoopReport, Box<dyn std::error::Error>> {
    let mut targets = Vec::new();
    let mut failures = Vec::new();
    for row in rows {
        match resolver.resolve_reserve_target(row, rpc).await {
            Ok(target) => targets.push(target),
            Err(error) => failures.push(error.to_string()),
        }
    }
    apply_testing_overrides(&mut targets, overrides);

    let route_config = YieldRouteRunConfig {
        loop_config: SameMintLoopConfig {
            cluster: cli.cluster.clone(),
            max_vaults: cli.max_vaults,
            batch_size: cli.batch_size,
            reconcile_positions: true,
            dry_run: cli.dry_run,
            submit_txs: !cli.dry_run,
            abandon_dry_run_decisions: cli.dry_run,
            worker_id: "yield-route-worker".to_owned(),
        },
        planner_config: loyal_yield_orchestrator::YieldRoutePlannerConfig {
            targets,
            min_edge_bps: cli.min_edge_bps,
            estimated_cost_lamports: overrides.estimated_cost_lamports,
        },
    };

    let loop_runner = YieldRoutingLoop::new(store, rpc, signer, route_config);
    let mut report = loop_runner.run_once().await?;
    if !failures.is_empty() {
        report.failures.extend(failures);
    }
    Ok(report)
}

fn load_testing_overrides(cli: &Cli) -> Result<TestingOverrides, Box<dyn std::error::Error>> {
    let config = if let Some(config_json) = &cli.config_json {
        Some(config_json.clone())
    } else if let Some(config_file) = &cli.config_file {
        Some(fs::read_to_string(config_file)?)
    } else {
        None
    };
    let Some(config) = config else {
        return Ok(TestingOverrides::default());
    };

    if let Ok(run_config) = serde_json::from_str::<YieldRouteRunConfig>(&config) {
        return Ok(TestingOverrides {
            targets: run_config.planner_config.targets,
            estimated_cost_lamports: run_config.planner_config.estimated_cost_lamports,
        });
    }
    Ok(serde_json::from_str::<TestingOverrides>(&config)?)
}

fn apply_testing_overrides(targets: &mut Vec<YieldReserveTarget>, overrides: &TestingOverrides) {
    if overrides.targets.is_empty() {
        return;
    }

    let mut by_reserve = targets
        .drain(..)
        .map(|target| (target.reserve.clone(), target))
        .collect::<BTreeMap<_, _>>();
    for target in &overrides.targets {
        by_reserve.insert(target.reserve.clone(), target.clone());
    }
    targets.extend(by_reserve.into_values());
}

fn rows_signature(rows: &[ReserveUpdateRow]) -> String {
    let mut parts = rows
        .iter()
        .map(|row| {
            let apy =
                loyal_yield_orchestrator::apy_ratio_to_bps(row.supply_apy).unwrap_or_default();
            format!("{}:{apy}", row.reserve)
        })
        .collect::<Vec<_>>();
    parts.sort();
    parts.join("|")
}
