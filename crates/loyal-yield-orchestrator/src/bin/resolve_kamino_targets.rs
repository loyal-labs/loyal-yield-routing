use std::collections::BTreeMap;

use clap::Parser;
use loyal_yield_orchestrator::KaminoReserveMetadataResolver;
use loyal_yield_router::timescale::{
    ReserveUpdateFilter, TimescaleRouterClient, TimescaleRouterClientConfig,
};
use solana_client::rpc_client::RpcClient;

#[derive(Debug, Parser)]
struct Cli {
    #[arg(long, env = "SOLANA_RPC_URL")]
    rpc_url: String,
    #[arg(long, env = "TIMESCALEDB_URL")]
    timescaledb_url: String,
    #[arg(long = "reserve")]
    reserves: Vec<String>,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse();
    let timescale =
        TimescaleRouterClient::connect(TimescaleRouterClientConfig::new(cli.timescaledb_url))
            .await?;
    let rows = timescale
        .latest_reserves(ReserveUpdateFilter::new())
        .await?;
    let mut by_reserve = rows
        .into_iter()
        .map(|row| (row.reserve.clone(), row))
        .collect::<BTreeMap<_, _>>();
    let rpc = RpcClient::new(cli.rpc_url);
    let mut resolver = KaminoReserveMetadataResolver::default();
    let mut targets = Vec::new();
    for reserve in &cli.reserves {
        let row = by_reserve
            .remove(reserve)
            .ok_or_else(|| format!("reserve {reserve} not found in latest Timescale rows"))?;
        targets.push(resolver.resolve_reserve_target(row, &rpc).await?);
    }
    println!("{}", serde_json::to_string_pretty(&targets)?);
    Ok(())
}
