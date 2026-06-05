use std::path::PathBuf;

use anyhow::{bail, Result};
use clap::{ArgAction, Parser, ValueEnum};
use solana_sdk::pubkey::Pubkey;

const DEFAULT_KAMINO_API_BASE: &str = "https://api.kamino.finance";
const DEFAULT_SUBSTREAMS_ENDPOINT: &str = "accounts.mainnet.sol.streamingfast.io:443";
const DEFAULT_SUBSTREAMS_PACKAGE_URL: &str =
    "https://spkg.io/streamingfast/solana_accounts_foundational-v0.1.1.spkg";

#[derive(Clone, Copy, Debug, PartialEq, Eq, ValueEnum)]
pub enum SubstreamsTransport {
    Grpc,
    Cli,
}

#[derive(Clone, Debug, Parser)]
#[command(author, version, about)]
pub struct Args {
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
    pub store_raw: bool,

    #[arg(long)]
    pub sync_supported_reserves: bool,

    #[arg(long)]
    pub substreams_backfill: bool,

    #[arg(long, value_enum, default_value = "grpc")]
    pub substreams_transport: SubstreamsTransport,

    #[arg(long, env = "SUBSTREAMS_ENDPOINT", default_value = DEFAULT_SUBSTREAMS_ENDPOINT)]
    pub substreams_endpoint: String,

    #[arg(long, default_value = "substreams")]
    pub substreams_cli: PathBuf,

    #[arg(long, default_value = "solana-accounts-foundational@v0.1.1")]
    pub substreams_package: String,

    #[arg(long, env = "SUBSTREAMS_PACKAGE_URL", default_value = DEFAULT_SUBSTREAMS_PACKAGE_URL)]
    pub substreams_package_url: String,

    #[arg(long, default_value = "crates/kamino-historic-data/substreams-adapter")]
    pub substreams_grpc_adapter: PathBuf,

    #[arg(long, default_value = "filtered_accounts")]
    pub substreams_module: String,

    #[arg(long)]
    pub substreams_start_block: Option<u64>,

    #[arg(long)]
    pub substreams_start_unix: Option<i64>,

    #[arg(long)]
    pub substreams_stop_block: Option<u64>,

    #[arg(long)]
    pub substreams_stop_unix: Option<i64>,

    #[arg(long, default_value_t = 100_000)]
    pub substreams_chunk_blocks: u64,

    #[arg(long, default_value_t = 5_000)]
    pub substreams_progress_rows: u64,

    #[arg(long, default_value_t = 8)]
    pub substreams_insert_concurrency: usize,

    #[arg(long, default_value_t = 1_000)]
    pub substreams_insert_batch_size: usize,

    #[arg(long, action = ArgAction::SetTrue)]
    pub substreams_skip_db_inserts: bool,

    #[arg(long, action = ArgAction::SetTrue)]
    pub substreams_production_mode: bool,

    #[arg(long)]
    pub substreams_parallel_workers: Option<usize>,

    #[arg(long, default_value_t = 1)]
    pub substreams_concurrent_streams: usize,

    #[arg(long, default_value = "SF_API_TOKEN")]
    pub substreams_api_key_envvar: String,

    #[arg(long)]
    pub jsonl: Option<PathBuf>,

    #[arg(long, value_name = "PATH", num_args = 1..)]
    pub import_jsonl: Vec<PathBuf>,
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
    if args.substreams_backfill {
        if args.substreams_start_block.is_none() {
            bail!("--substreams-start-block is required with --substreams-backfill");
        }
        if args.substreams_start_unix.is_none() {
            bail!("--substreams-start-unix is required with --substreams-backfill");
        }
        if args.substreams_stop_block.is_none() {
            bail!("--substreams-stop-block is required with --substreams-backfill");
        }
        if args.substreams_stop_unix.is_none() {
            bail!("--substreams-stop-unix is required with --substreams-backfill");
        }
        if args.substreams_start_block >= args.substreams_stop_block {
            bail!("--substreams-start-block must be lower than --substreams-stop-block");
        }
        if args.substreams_start_unix >= args.substreams_stop_unix {
            bail!("--substreams-start-unix must be lower than --substreams-stop-unix");
        }
        if matches!(args.substreams_transport, SubstreamsTransport::Grpc)
            && args.substreams_endpoint.trim().is_empty()
        {
            bail!("--substreams-endpoint cannot be empty with --substreams-transport grpc");
        }
        if matches!(args.substreams_transport, SubstreamsTransport::Grpc)
            && args.substreams_package_url.trim().is_empty()
        {
            bail!("--substreams-package-url cannot be empty with --substreams-transport grpc");
        }
        if args.substreams_chunk_blocks == 0 {
            bail!("--substreams-chunk-blocks must be greater than zero");
        }
        if args.substreams_progress_rows == 0 {
            bail!("--substreams-progress-rows must be greater than zero");
        }
        validate_substreams_insert_config(args)?;
        if args.substreams_skip_db_inserts && args.jsonl.is_none() {
            bail!("--jsonl is required with --substreams-skip-db-inserts");
        }
        if args
            .substreams_parallel_workers
            .is_some_and(|workers| workers == 0)
        {
            bail!("--substreams-parallel-workers must be greater than zero");
        }
        if args.substreams_concurrent_streams == 0 {
            bail!("--substreams-concurrent-streams must be greater than zero");
        }
        if args.substreams_concurrent_streams > 1 {
            if !matches!(args.substreams_transport, SubstreamsTransport::Grpc) {
                bail!("--substreams-concurrent-streams greater than 1 currently requires --substreams-transport grpc");
            }
            if !args.substreams_skip_db_inserts {
                bail!("--substreams-concurrent-streams greater than 1 currently requires --substreams-skip-db-inserts");
            }
            if args.jsonl.is_none() {
                bail!("--jsonl is required with --substreams-concurrent-streams greater than 1");
            }
        }
        if args.substreams_api_key_envvar.trim().is_empty() {
            bail!("--substreams-api-key-envvar cannot be empty");
        }
    }
    if !args.import_jsonl.is_empty() {
        validate_substreams_insert_config(args)?;
    }
    validate_pg_identifier(&args.timescaledb_schema, "--timescaledb-schema")
}

fn validate_substreams_insert_config(args: &Args) -> Result<()> {
    if args.substreams_insert_concurrency == 0 {
        bail!("--substreams-insert-concurrency must be greater than zero");
    }
    if args.substreams_insert_batch_size == 0 {
        bail!("--substreams-insert-batch-size must be greater than zero");
    }
    Ok(())
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
    fn accepts_timescale_only_sync_args() {
        let args = Args::try_parse_from([
            "kamino-historic-data",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--sync-supported-reserves",
        ])
        .expect("sync args should parse");

        validate_args(&args).expect("sync args should validate");
        assert!(args.sync_supported_reserves);
    }

    #[test]
    fn accepts_historic_sync_mode() {
        let args = Args::try_parse_from([
            "kamino-historic-data",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--sync-supported-reserves",
        ])
        .expect("historic args should parse");

        validate_args(&args).expect("historic args should validate");
        assert!(args.sync_supported_reserves);
    }

    #[test]
    fn accepts_import_jsonl_mode() {
        let args = Args::try_parse_from([
            "kamino-historic-data",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--import-jsonl",
            "snapshot.jsonl",
        ])
        .expect("import args should parse");

        validate_args(&args).expect("import args should validate");
        assert_eq!(args.import_jsonl, vec![PathBuf::from("snapshot.jsonl")]);
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
    fn accepts_substreams_backfill_mode() {
        let args = Args::try_parse_from([
            "kamino-reserve-monitor",
            "--timescaledb-url",
            "postgres://example.invalid/kamino",
            "--substreams-backfill",
            "--substreams-start-block",
            "410195947",
            "--substreams-start-unix",
            "1775001600",
            "--substreams-stop-block",
            "424374151",
            "--substreams-stop-unix",
            "1780628135",
        ])
        .expect("backfill args should parse");

        validate_args(&args).expect("backfill args should validate");
        assert!(args.substreams_backfill);
    }
}
