use clap::Parser;
use loyal_yield_orchestrator::{
    yield_router_keypair_from_env, NeonSqlConfig, OrchestratorStore, SameMintLoopConfig,
    SameMintRouteRunConfig, SameMintYieldRoutingLoop,
};
use solana_client::rpc_client::RpcClient;
use std::{fs, path::PathBuf, time::Duration};

#[derive(Debug, Parser)]
#[command(about = "Manually trigger one same-mint Loyal yield-routing batch")]
struct Cli {
    #[arg(long, env = "NEON_DATABASE_URL")]
    postgres_url: String,
    #[arg(long, env = "SOLANA_RPC_URL")]
    rpc_url: String,
    #[arg(long, env = "SAME_MINT_ROUTE_CONFIG_JSON")]
    config_json: Option<String>,
    #[arg(long, env = "SAME_MINT_ROUTE_CONFIG_FILE")]
    config_file: Option<PathBuf>,
    #[arg(long)]
    cluster: Option<String>,
    #[arg(long)]
    max_vaults: Option<usize>,
    #[arg(long)]
    batch_size: Option<usize>,
    #[arg(long)]
    no_reconcile: bool,
    #[arg(long)]
    keep_ready: bool,
    #[arg(long)]
    submit_txs: bool,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse();
    let mut route_config = load_route_config(&cli)?;
    apply_cli_overrides(&mut route_config.loop_config, &cli);

    let store = OrchestratorStore::connect(
        NeonSqlConfig::new(cli.postgres_url)
            .with_max_connections(2)
            .with_acquire_timeout(Duration::from_secs(10)),
    )
    .await?;
    store.apply_migrations().await?;

    let rpc = RpcClient::new(cli.rpc_url);
    let signer = yield_router_keypair_from_env()?;
    let loop_runner = SameMintYieldRoutingLoop::new(&store, &rpc, &signer, route_config);
    let report = loop_runner.run_once().await?;

    println!("{}", serde_json::to_string_pretty(&report)?);
    Ok(())
}

fn load_route_config(cli: &Cli) -> Result<SameMintRouteRunConfig, Box<dyn std::error::Error>> {
    let config = if let Some(config_json) = &cli.config_json {
        config_json.clone()
    } else if let Some(config_file) = &cli.config_file {
        fs::read_to_string(config_file)?
    } else {
        return Err("pass --config-json, --config-file, SAME_MINT_ROUTE_CONFIG_JSON, or SAME_MINT_ROUTE_CONFIG_FILE".into());
    };
    Ok(serde_json::from_str(&config)?)
}

fn apply_cli_overrides(config: &mut SameMintLoopConfig, cli: &Cli) {
    if let Some(cluster) = &cli.cluster {
        config.cluster = Some(cluster.clone());
    }
    if let Some(max_vaults) = cli.max_vaults {
        config.max_vaults = max_vaults;
    }
    if let Some(batch_size) = cli.batch_size {
        config.batch_size = batch_size;
    }
    if cli.no_reconcile {
        config.reconcile_positions = false;
    }
    if cli.keep_ready {
        config.abandon_dry_run_decisions = false;
    }
    config.submit_txs = cli.submit_txs;
    config.dry_run = !cli.submit_txs;
}
