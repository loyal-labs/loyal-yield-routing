use clap::Parser;
use loyal_squads_policy_monitor::{
    Cluster, Commitment, MonitorConfig, MonitorError, PolicyMonitor, PostgresPolicyMatchSink,
    StdoutPolicyMatchSink,
};

#[derive(Debug, Parser)]
#[command(about = "Stream Squads smart-account transactions and emit Loyal route-policy matches")]
struct Cli {
    #[arg(long, default_value_t = Cluster::Mainnet)]
    cluster: Cluster,
    #[arg(long, env = "HELIUS_API_KEY")]
    api_key: Option<String>,
    #[arg(long)]
    ws_url: Option<String>,
    #[arg(long, default_value_t = Commitment::Confirmed)]
    commitment: Commitment,
    #[arg(long)]
    once: bool,
    #[arg(long, env = "NEON_DATABASE_URL")]
    postgres_url: Option<String>,
}

#[tokio::main]
async fn main() -> Result<(), MonitorError> {
    let cli = Cli::parse();
    let config = MonitorConfig::new(cli.cluster, cli.commitment, cli.ws_url, cli.api_key)?;
    match cli.postgres_url {
        Some(url) => {
            let mut monitor =
                PolicyMonitor::new(config, PostgresPolicyMatchSink::connect(url).await?);
            monitor.run(cli.once).await
        }
        None => {
            let mut monitor = PolicyMonitor::new(config, StdoutPolicyMatchSink);
            monitor.run(cli.once).await
        }
    }
}
