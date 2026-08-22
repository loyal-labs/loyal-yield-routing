//! One-shot, read-only producer for the durable Voltr restoration evidence.
//!
//! The TypeScript manager executor remains the only component allowed to
//! build/sign/send Solana packets.  After those confirmed legs are manually
//! handed off, this command reloads the existing Neon orchestration outbox and
//! emits the exact `durableOutbox` section expected by the four-market
//! verifier.  It never loads a signer and never sends an RPC transaction.

use loyal_yield_orchestrator::{NeonSqlClient, NeonSqlConfig};
use loyal_yield_store::fleet_orchestration::VoltrRestorationOutboxReadback;
use serde::Deserialize;
use serde_json::json;
use std::{env, fs, path::PathBuf};

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ReadbackInput {
    schema_version: u8,
    cluster: String,
    origin_id: String,
    generation: i64,
    expected_leg_count: usize,
}

fn usage() -> ! {
    eprintln!(
        "usage: backyard-voltr-restoration-readback --input <readback.json>\n\n\
         Reads acknowledged rows from the existing Neon orchestration_outbox.\n\
         Requires NEON_DATABASE_URL; no signer or Solana transaction is loaded."
    );
    std::process::exit(2)
}

fn main() {
    let mut args = env::args().skip(1);
    let mut input_path: Option<PathBuf> = None;
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--input" => input_path = args.next().map(PathBuf::from),
            "--help" | "-h" => usage(),
            other => {
                eprintln!("unknown argument: {other}");
                usage();
            }
        }
    }
    let Some(input_path) = input_path else {
        usage()
    };
    let input_text = fs::read_to_string(&input_path).unwrap_or_else(|error| {
        eprintln!("cannot read {}: {error}", input_path.display());
        std::process::exit(2)
    });
    let input: ReadbackInput = serde_json::from_str(&input_text).unwrap_or_else(|error| {
        eprintln!("invalid restoration readback input: {error}");
        std::process::exit(2)
    });
    if input.schema_version != 1
        || input.cluster.trim().is_empty()
        || input.generation <= 0
        || input.expected_leg_count == 0
    {
        eprintln!("restoration readback input identity/count is malformed");
        std::process::exit(2)
    }
    let neon_url = env::var("NEON_DATABASE_URL").unwrap_or_else(|_| {
        eprintln!("NEON_DATABASE_URL is required");
        std::process::exit(2)
    });
    let runtime = tokio::runtime::Runtime::new().unwrap_or_else(|error| {
        eprintln!("cannot start Tokio runtime: {error}");
        std::process::exit(2)
    });
    let result: Result<VoltrRestorationOutboxReadback, Box<dyn std::error::Error>> = runtime
        .block_on(async {
            let neon = NeonSqlClient::connect(NeonSqlConfig::new(neon_url).with_max_connections(2))
                .await?;
            Ok(neon
                .read_voltr_restoration_outbox(
                    &input.cluster,
                    &input.origin_id,
                    input.generation,
                    input.expected_leg_count,
                )
                .await?)
        });
    let readback = result.unwrap_or_else(|error| {
        eprintln!("Voltr restoration readback failed closed: {error}");
        std::process::exit(2)
    });
    println!(
        "{}",
        serde_json::to_string(&json!({
            "verdict": "BACKYARD_VOLTR_RESTORATION_DURABLE_READBACK_PASS",
            "broadcast": false,
            "signerLoaded": false,
            "source": "loyal_yield.orchestration_outbox",
            "durableOutbox": readback,
        }))
        .expect("serializable readback")
    );
}
