use anyhow::{bail, Result};
use clap::Parser;
use clap::ValueEnum;
use solana_sdk::pubkey::Pubkey;

use crate::source::DEFAULT_ACCOUNT_EVENT_CHANNEL_CAPACITY;

const DEFAULT_RPC_URL: &str = "https://api.mainnet-beta.solana.com";
const DEFAULT_KAMINO_API_BASE: &str = "https://api.kamino.finance";
const DEFAULT_SUPPORTED_RESERVE_REFRESH_INTERVAL_SECS: u64 = 120;
const MAX_SUPPORTED_RESERVE_REFRESH_INTERVAL_SECS: u64 = 180;

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

    #[arg(
        long,
        env = "KAMINO_SUPPORTED_RESERVE_REFRESH_INTERVAL_SECS",
        default_value_t = DEFAULT_SUPPORTED_RESERVE_REFRESH_INTERVAL_SECS
    )]
    pub supported_reserve_refresh_interval_secs: u64,

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

    /// Operator-only escape hatch for an intentional catalog contraction.
    /// Normal startup/refresh and the standard predeploy sync fail closed on
    /// removals so a partial-but-valid API response cannot erase a true peak.
    #[arg(long, requires = "sync_supported_reserves")]
    pub allow_supported_reserve_removals: bool,

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

    #[arg(
        long,
        env = "KAMINO_ACCOUNT_EVENT_CHANNEL_CAPACITY",
        default_value_t = DEFAULT_ACCOUNT_EVENT_CHANNEL_CAPACITY
    )]
    pub account_event_channel_capacity: usize,

    #[arg(long, default_value_t = 90)]
    pub progress_timeout_secs: u64,

    #[arg(long, default_value_t = 60)]
    pub status_log_interval_secs: u64,

    #[arg(long, default_value_t = 30)]
    pub confirmed_refresh_interval_secs: u64,

    #[arg(long, default_value_t = 20)]
    pub confirmed_refresh_timeout_secs: u64,
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
    if args.supported_reserve_refresh_interval_secs == 0 {
        bail!("--supported-reserve-refresh-interval-secs must be greater than zero");
    }
    if args.supported_reserve_refresh_interval_secs > MAX_SUPPORTED_RESERVE_REFRESH_INTERVAL_SECS {
        bail!("--supported-reserve-refresh-interval-secs must not exceed 180");
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
    if args.account_event_channel_capacity == 0 {
        bail!("--account-event-channel-capacity must be greater than zero");
    }
    if args.progress_timeout_secs == 0 {
        bail!("--progress-timeout-secs must be greater than zero");
    }
    if args.status_log_interval_secs == 0 {
        bail!("--status-log-interval-secs must be greater than zero");
    }
    if args.confirmed_refresh_interval_secs == 0 {
        bail!("--confirmed-refresh-interval-secs must be greater than zero");
    }
    if args.confirmed_refresh_timeout_secs == 0 {
        bail!("--confirmed-refresh-timeout-secs must be greater than zero");
    }
    if args.allow_supported_reserve_removals && !args.sync_supported_reserves {
        bail!("--allow-supported-reserve-removals requires --sync-supported-reserves");
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
