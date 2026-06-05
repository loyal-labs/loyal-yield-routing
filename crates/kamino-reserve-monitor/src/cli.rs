use std::path::PathBuf;

use anyhow::{bail, Result};
use clap::Parser;
use solana_sdk::pubkey::Pubkey;

const DEFAULT_RPC_URL: &str = "https://api.mainnet-beta.solana.com";
const DEFAULT_WS_URL: &str = "wss://api.mainnet-beta.solana.com/";
const DEFAULT_KAMINO_API_BASE: &str = "https://api.kamino.finance";

#[derive(Debug, Parser)]
#[command(author, version, about)]
pub struct Args {
    #[arg(long, env = "SOLANA_RPC_URL", default_value = DEFAULT_RPC_URL)]
    pub rpc_url: String,

    #[arg(long, env = "SOLANA_WS_URL", default_value = DEFAULT_WS_URL)]
    pub ws_url: String,

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
    pub jsonl: Option<PathBuf>,

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
    validate_pg_identifier(&args.timescaledb_schema, "--timescaledb-schema")
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
}
