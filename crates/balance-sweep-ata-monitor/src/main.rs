use std::{
    collections::HashMap,
    sync::{atomic::AtomicBool, Arc},
    time::Duration,
};

use anyhow::{bail, Result};
use balance_sweep_ata_monitor::{
    run_event_loop, seed_current_balances, AtaTarget, AtaUpdateSource, LaserstreamAtaUpdateSource,
    SubscriptionConfig, TimescaleAtaConfig, TimescaleAtaObservationSink, WebsocketAtaUpdateSource,
};
use clap::{Parser, ValueEnum};
use loyal_yield_orchestrator::{OrchestratorConfig, OrchestratorStore};
use tokio::sync::mpsc;

#[derive(Clone, Copy, Debug, PartialEq, Eq, ValueEnum)]
enum UpdateSourceKind {
    Laserstream,
    Websocket,
}

#[derive(Debug, Parser)]
#[command(about = "Monitor wallet USDC ATAs for active Loyal balance sweep targets")]
struct Args {
    #[arg(
        long,
        env = "SOLANA_RPC_URL",
        default_value = "https://api.mainnet-beta.solana.com"
    )]
    rpc_url: String,
    #[arg(long, env = "SOLANA_WS_URL")]
    ws_url: Option<String>,
    #[arg(long, env = "NEON_DATABASE_URL")]
    postgres_url: String,
    #[arg(long, env = "TIMESCALEDB_URL")]
    timescaledb_url: String,
    #[arg(long, default_value = "mainnet")]
    cluster: String,
    #[arg(
        long,
        env = "BALANCE_SWEEP_UPDATE_SOURCE",
        default_value = "laserstream"
    )]
    update_source: UpdateSourceKind,
    #[arg(long, env = "HELIUS_API_KEY")]
    helius_api_key: Option<String>,
    #[arg(long, env = "LASERSTREAM_ENDPOINT")]
    laserstream_endpoint: Option<String>,
    #[arg(long, default_value_t = 32)]
    laserstream_replay_overlap_slots: u64,
    #[arg(long)]
    once: bool,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();
    let args = Args::parse();
    tracing::info!(
        cluster = %args.cluster,
        update_source = ?args.update_source,
        once = args.once,
        "starting balance sweep ATA monitor"
    );
    let store = OrchestratorStore::connect(OrchestratorConfig::new(args.postgres_url)).await?;
    let targets = store
        .load_active_balance_sweep_targets(&args.cluster)
        .await?
        .iter()
        .map(AtaTarget::try_from)
        .collect::<Result<Vec<_>>>()?;
    tracing::info!(
        target_count = targets.len(),
        "loaded active balance sweep ATA targets"
    );
    let observations =
        TimescaleAtaObservationSink::connect(TimescaleAtaConfig::new(args.timescaledb_url)).await?;
    seed_current_balances(&args.rpc_url, &targets, &observations).await?;
    tracing::info!(
        target_count = targets.len(),
        "seeded current wallet ATA balances"
    );
    if args.once {
        tracing::info!("exiting after one-shot balance seed");
        return Ok(());
    }
    if targets.is_empty() {
        bail!(
            "no active balance sweep targets for cluster {}",
            args.cluster
        );
    }
    let accounts = targets
        .iter()
        .map(|target| target.wallet_usdc_ata)
        .collect::<Vec<_>>();
    let target_by_ata = targets
        .into_iter()
        .map(|target| (target.wallet_usdc_ata, target))
        .collect::<HashMap<_, _>>();
    let config = SubscriptionConfig {
        max_reconnect_attempts: 10,
        reconnect_base_delay: Duration::from_millis(500),
        reconnect_max_delay: Duration::from_secs(30),
        heartbeat_interval: Duration::from_secs(15),
    };
    let (tx, rx) = mpsc::unbounded_channel();
    let running = Arc::new(AtomicBool::new(true));
    let _worker = match args.update_source {
        UpdateSourceKind::Laserstream => LaserstreamAtaUpdateSource {
            endpoint: args
                .laserstream_endpoint
                .ok_or_else(|| anyhow::anyhow!("LASERSTREAM_ENDPOINT is required"))?,
            api_key: args
                .helius_api_key
                .ok_or_else(|| anyhow::anyhow!("HELIUS_API_KEY is required"))?,
            from_slot: args.laserstream_replay_overlap_slots,
            config,
        }
        .spawn(accounts, tx, running.clone()),
        UpdateSourceKind::Websocket => WebsocketAtaUpdateSource {
            ws_url: args
                .ws_url
                .ok_or_else(|| anyhow::anyhow!("SOLANA_WS_URL is required"))?,
            config,
        }
        .spawn(accounts, tx, running.clone()),
    };
    run_event_loop(rx, target_by_ata, observations, running).await
}
