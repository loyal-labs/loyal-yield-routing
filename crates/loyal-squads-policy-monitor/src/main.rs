use clap::{Parser, ValueEnum};
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
    #[arg(long, env = "POLICY_MONITOR_MODE", default_value = "shadow")]
    mode: MonitorMode,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, ValueEnum)]
enum MonitorMode {
    /// Compatibility rollback path which still writes the policy projection.
    Fallback,
    /// Observe and log only; LaserStream owns the durable projection.
    Shadow,
    Disabled,
}

#[tokio::main]
#[allow(clippy::result_large_err)]
async fn main() -> Result<(), MonitorError> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let cli = Cli::parse();
    if cli.mode == MonitorMode::Disabled {
        return Ok(());
    }
    let config = MonitorConfig::new(cli.cluster, cli.commitment, cli.ws_url, cli.api_key)?;
    match (cli.mode, cli.postgres_url) {
        (MonitorMode::Fallback, Some(url)) => {
            let mut monitor =
                PolicyMonitor::new(config, PostgresPolicyMatchSink::connect(url).await?);
            monitor.run(cli.once).await
        }
        (MonitorMode::Fallback, None) => Err(MonitorError::Decode(
            "fallback policy monitor requires NEON_DATABASE_URL".to_owned(),
        )),
        (MonitorMode::Shadow, _) => {
            let mut monitor = PolicyMonitor::new(config, StdoutPolicyMatchSink);
            monitor.run(cli.once).await
        }
        (MonitorMode::Disabled, _) => unreachable!("disabled mode exits before configuration"),
    }
}
