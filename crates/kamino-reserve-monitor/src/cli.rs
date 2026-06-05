use anyhow::{bail, Result};
use clap::Parser;
use clap::ValueEnum;
use solana_sdk::pubkey::Pubkey;

const DEFAULT_RPC_URL: &str = "https://api.mainnet-beta.solana.com";
const DEFAULT_KAMINO_API_BASE: &str = "https://api.kamino.finance";

#[derive(Clone, Copy, Debug, PartialEq, Eq, ValueEnum)]
pub enum UpdateSourceKind {
    Laserstream,
    Websocket,
}

#[derive(Debug, Parser)]
#[command(author, version, about)]
pub struct Args {
    #[arg(long, env = "SOLANA_RPC_URL", default_value = DEFAULT_RPC_URL)]
    pub rpc_url: String,

    #[arg(long, env = "SOLANA_WS_URL")]
    pub ws_url: Option<String>,

    #[arg(long, env = "KAMINO_UPDATE_SOURCE", default_value = "laserstream")]
    pub update_source: UpdateSourceKind,

    #[arg(long, env = "HELIUS_API_KEY")]
    pub helius_api_key: Option<String>,

    #[arg(long, env = "LASERSTREAM_ENDPOINT")]
    pub laserstream_endpoint: Option<String>,

    #[arg(long, default_value_t = 32)]
    pub laserstream_replay_overlap_slots: u64,

    #[arg(long, env = "KAMINO_API_BASE", default_value = DEFAULT_KAMINO_API_BASE)]
    pub kamino_api_base: String,

    #[arg(long, default_value_t = 10)]
    pub kamino_api_timeout_secs: u64,

    #[arg(long, env = "TIMESCALEDB_URL")]
    pub timescaledb_url: String,

    #[arg(long, env = "TIMESCALEDB_SCHEMA", default_value = "kamino")]
    pub timescaledb_schema: String,

    #[arg(long, value_delimiter = ',')]
    pub reserve: Vec<Pubkey>,

    #[arg(long, default_value_t = 400.0)]
    pub slot_duration_ms: f64,

    #[arg(long)]
    pub no_slot_duration_api: bool,

    #[arg(long)]
    pub once: bool,

    #[arg(long)]
    pub store_raw: bool,

    #[arg(long)]
    pub sync_supported_reserves: bool,

    #[arg(long)]
    pub jsonl: Option<std::path::PathBuf>,

    #[arg(long, default_value_t = 10)]
    pub max_reconnect_attempts: usize,

    #[arg(long, default_value_t = 500)]
    pub reconnect_base_delay_ms: u64,

    #[arg(long, default_value_t = 30)]
    pub reconnect_max_delay_secs: u64,

    #[arg(long, default_value_t = 15)]
    pub subscription_heartbeat_secs: u64,

    #[arg(long, default_value_t = 90)]
    pub progress_timeout_secs: u64,

    #[arg(long, default_value_t = 60)]
    pub status_log_interval_secs: u64,
}

impl Args {
    pub fn parse_args() -> Self {
        Self::parse()
    }
}

pub fn validate_args(args: &Args) -> Result<()> {
    if args.timescaledb_url.trim().is_empty() {
        bail!("TIMESCALEDB_URL is required");
    }
    if args.slot_duration_ms <= 0.0 {
        bail!("--slot-duration-ms must be greater than zero");
    }
    if args.kamino_api_timeout_secs == 0 {
        bail!("--kamino-api-timeout-secs must be greater than zero");
    }
    if args.max_reconnect_attempts == 0 {
        bail!("--max-reconnect-attempts must be greater than zero");
    }
    if args.reconnect_base_delay_ms == 0 {
        bail!("--reconnect-base-delay-ms must be greater than zero");
    }
    if args.reconnect_max_delay_secs == 0 {
        bail!("--reconnect-max-delay-secs must be greater than zero");
    }
    if args.subscription_heartbeat_secs == 0 {
        bail!("--subscription-heartbeat-secs must be greater than zero");
    }
    if args.progress_timeout_secs == 0 {
        bail!("--progress-timeout-secs must be greater than zero");
    }
    if args.status_log_interval_secs == 0 {
        bail!("--status-log-interval-secs must be greater than zero");
    }
    if requires_account_update_source(args) {
        match args.update_source {
            UpdateSourceKind::Laserstream => {
                if args
                    .helius_api_key
                    .as_deref()
                    .is_none_or(|value| value.trim().is_empty())
                {
                    bail!("HELIUS_API_KEY is required when KAMINO_UPDATE_SOURCE=laserstream");
                }
                if args
                    .laserstream_endpoint
                    .as_deref()
                    .is_none_or(|value| value.trim().is_empty())
                {
                    bail!("LASERSTREAM_ENDPOINT is required when KAMINO_UPDATE_SOURCE=laserstream");
                }
            }
            UpdateSourceKind::Websocket => {
                if args
                    .ws_url
                    .as_deref()
                    .is_none_or(|value| value.trim().is_empty())
                {
                    bail!("SOLANA_WS_URL is required when KAMINO_UPDATE_SOURCE=websocket");
                }
            }
        }
    }
    validate_pg_identifier(&args.timescaledb_schema, "--timescaledb-schema")
}

fn requires_account_update_source(args: &Args) -> bool {
    !(args.sync_supported_reserves || args.once)
}

fn validate_pg_identifier(value: &str, flag: &str) -> Result<()> {
    let mut chars = value.chars();
    let Some(first) = chars.next() else {
        bail!("{flag} cannot be empty");
    };
    if !(first == '_' || first.is_ascii_alphabetic()) {
        bail!("{flag} must start with an ASCII letter or underscore");
    }
    if !chars.all(|ch| ch == '_' || ch.is_ascii_alphanumeric()) {
        bail!("{flag} may only contain ASCII letters, digits, and underscores");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_required_worker_env_args_without_health_flags() {
        let args = Args::try_parse_from([
            "kamino-reserve-monitor",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--once",
        ])
        .expect("worker args should parse");

        validate_args(&args).expect("worker args should validate");
        assert!(args.once);
    }

    #[test]
    fn accepts_laserstream_source_args() {
        let args = Args::try_parse_from([
            "kamino-reserve-monitor",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--update-source",
            "laserstream",
            "--helius-api-key",
            "test-key",
            "--laserstream-endpoint",
            "https://laserstream-mainnet-lax.helius-rpc.com",
        ])
        .expect("laserstream args should parse");

        validate_args(&args).expect("laserstream args should validate");
        assert_eq!(args.update_source, UpdateSourceKind::Laserstream);
    }

    #[test]
    fn rejects_laserstream_without_required_env() {
        let args = Args::try_parse_from([
            "kamino-reserve-monitor",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--update-source",
            "laserstream",
        ])
        .expect("laserstream args should parse");

        let err = validate_args(&args).expect_err("missing LaserStream env should fail");
        assert!(err.to_string().contains("HELIUS_API_KEY"));
    }

    #[test]
    fn accepts_websocket_source_args() {
        let args = Args::try_parse_from([
            "kamino-reserve-monitor",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--update-source",
            "websocket",
            "--ws-url",
            "wss://example.invalid",
        ])
        .expect("websocket args should parse");

        validate_args(&args).expect("websocket args should validate");
        assert_eq!(args.update_source, UpdateSourceKind::Websocket);
    }

    #[test]
    fn rejects_websocket_without_ws_url() {
        let args = Args::try_parse_from([
            "kamino-reserve-monitor",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--update-source",
            "websocket",
        ])
        .expect("websocket args should parse");

        let err = validate_args(&args).expect_err("missing websocket URL should fail");
        assert!(err.to_string().contains("SOLANA_WS_URL"));
    }

    #[test]
    fn rejects_removed_health_args() {
        assert!(Args::try_parse_from([
            "kamino-reserve-monitor",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--health-bind",
            "0.0.0.0:8787",
        ])
        .is_err());
        assert!(Args::try_parse_from([
            "kamino-reserve-monitor",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--port",
            "8787",
        ])
        .is_err());
    }

    #[test]
    fn accepts_supported_reserve_sync_mode() {
        let args = Args::try_parse_from([
            "kamino-reserve-monitor",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--sync-supported-reserves",
        ])
        .expect("sync args should parse");

        validate_args(&args).expect("sync args should validate");
        assert!(args.sync_supported_reserves);
    }

    #[test]
    fn rejects_historic_data_backfill_args() {
        assert!(Args::try_parse_from([
            "kamino-reserve-monitor",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--substreams-backfill",
        ])
        .is_err());
    }
}
