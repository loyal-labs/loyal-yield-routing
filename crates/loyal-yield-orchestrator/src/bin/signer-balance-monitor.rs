//! Signerless SOL balance monitoring for the fee payers the fleet signs with.
//!
//! On 2026-08-07 the standard policy authority fell to 0.001 SOL and every autodeposit
//! stopped. The executor's own balance guard reported it correctly, but only at the
//! moment work was already blocked: the first signal of the outage was the outage. This
//! process exists to make the shortfall observable while it is still cheap to fix.
//!
//! It reads balances by public key only. Reading a balance never needs a secret, so this
//! service refuses to start when key material is present in its environment, matching the
//! posture of the reusable ALT alert monitor.

use std::{
    collections::BTreeSet, env, error::Error, process::ExitCode, str::FromStr, time::Duration,
};

use loyal_observability::{init_from_env, OperationalError};
use loyal_yield_orchestrator::{
    rpc_safety::{redacted_external_error, validate_rpc_endpoint, validate_rpc_genesis_hash},
    STANDARD_POLICY_AUTHORITY,
};
use serde_json::json;
use solana_client::rpc_client::RpcClient;
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey};

const RPC_URL_ENV: &str = "SOLANA_RPC_URL";
const CLUSTER_ENV: &str = "YIELD_SIGNER_BALANCE_CLUSTER";
const WATCHED_ENV: &str = "YIELD_SIGNER_BALANCE_PUBKEYS";
const MIN_LAMPORTS_ENV: &str = "YIELD_SIGNER_BALANCE_MIN_LAMPORTS";
const LOW_LAMPORTS_ENV: &str = "YIELD_SIGNER_BALANCE_LOW_LAMPORTS";
const INTERVAL_ENV: &str = "YIELD_SIGNER_BALANCE_INTERVAL_SECS";

/// Matches the autodeposit executor's hard floor: below this, work stops.
const DEFAULT_MIN_LAMPORTS: u64 = 50_000_000;
/// Roughly twelve hours of warning before the floor stops the fleet.
///
/// Derived from the 21 days to 2026-08-07, over which the signer spent 12.94 SOL against
/// only 0.036 SOL of transaction fees; the balance goes on rent for accounts the routes
/// create, so it falls in bursts rather than steadily. The headroom is the 90th percentile
/// of observed rolling twelve-hour burn (0.494 SOL). The mean (0.108 SOL) would be outrun
/// by any ordinary busy period, and the 95th (1.90 SOL) would fire against nearly every
/// top-up this wallet has received.
const DEFAULT_LOW_LAMPORTS: u64 = 550_000_000;
const DEFAULT_INTERVAL_SECS: u64 = 300;

/// Reading a balance needs no secret, so any key material here is a misconfiguration.
const FORBIDDEN_ENVIRONMENTS: [&str; 5] = [
    "POLICY_KEYPAIR",
    "YIELD_ROUTER_KEYPAIR",
    "YIELD_ROUTE_FEE_PAYER_KEYPAIRS",
    "SOLANA_TESTING_PK",
    "DEPLOYMENT_PK",
];

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RunMode {
    Once,
    Watch,
}

#[derive(Debug, Clone)]
struct Options {
    rpc_url: String,
    cluster: String,
    watched: Vec<WatchedSigner>,
    min_lamports: u64,
    low_lamports: u64,
    interval: Duration,
    mode: RunMode,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct WatchedSigner {
    label: String,
    pubkey: Pubkey,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
enum BalanceState {
    Healthy,
    Low,
    Exhausted,
}

impl BalanceState {
    fn classify(balance: u64, low_lamports: u64, min_lamports: u64) -> Self {
        if balance < min_lamports {
            Self::Exhausted
        } else if balance < low_lamports {
            Self::Low
        } else {
            Self::Healthy
        }
    }
}

#[tokio::main]
async fn main() -> ExitCode {
    // The guard must outlive every alert this process emits: dropping it shuts the OTLP
    // providers down, which would leave a service whose entire purpose is to be heard
    // reporting only to local stdout.
    let _observability = match init_from_env("signer-balance-monitor") {
        Ok(guard) => guard,
        Err(error) => {
            eprintln!(
                "{}",
                json!({
                    "event": "signer_balance_monitor_fatal",
                    "error": redacted_external_error(&error.to_string()),
                    "signerLoaded": false,
                })
            );
            return ExitCode::FAILURE;
        }
    };
    match run().await {
        Ok(Outcome { worst, complete }) if worst == BalanceState::Exhausted || !complete => {
            ExitCode::from(20)
        }
        Ok(_) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!(
                "{}",
                json!({
                    "event": "signer_balance_monitor_fatal",
                    "error": redacted_external_error(&error.to_string()),
                    "signerLoaded": false,
                })
            );
            ExitCode::FAILURE
        }
    }
}

/// The result of a scan, and whether it actually saw everything it was asked to.
///
/// These are tracked apart because a healthy verdict drawn from an incomplete scan is
/// worse than no verdict: `--once` returning success would report the fleet funded when
/// the one signer that matters was never read.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Outcome {
    worst: BalanceState,
    complete: bool,
}

async fn run() -> Result<Outcome, Box<dyn Error>> {
    let options = parse_args_with_env(
        env::args().skip(1),
        |key| env::var(key).ok(),
        |key| env::var_os(key).is_some(),
    )?;
    validate_rpc_endpoint(&options.rpc_url)
        .map_err(|error| format!("invalid signer balance RPC endpoint: {error}"))?;

    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.clone(), CommitmentConfig::finalized());
    let genesis_hash = rpc
        .get_genesis_hash()
        .map_err(|_| "failed to read genesis hash from signer balance RPC")?;
    validate_rpc_genesis_hash(&options.cluster, genesis_hash)
        .map_err(|error| format!("signer balance RPC cluster mismatch: {error}"))?;

    let mut outcome = Outcome {
        complete: true,
        worst: BalanceState::Healthy,
    };
    loop {
        let scan = scan_once(&rpc, &options)?;
        outcome = Outcome {
            complete: outcome.complete && scan.complete,
            worst: outcome.worst.max(scan.worst),
        };
        if options.mode == RunMode::Once {
            return Ok(outcome);
        }
        tokio::time::sleep(options.interval).await;
    }
}

fn scan_once(rpc: &RpcClient, options: &Options) -> Result<Outcome, Box<dyn Error>> {
    let mut worst = BalanceState::Healthy;
    let mut complete = true;
    for signer in &options.watched {
        // One unreachable signer must not hide the rest: a read failure is reported and
        // the scan continues, because the balance we could not read is never the only one
        // that matters.
        let balance = match rpc.get_balance(&signer.pubkey) {
            Ok(balance) => balance,
            Err(error) => {
                OperationalError::new(
                    "signer_balance_read_failed",
                    "read_signer_balance",
                    "signer balance could not be read from RPC",
                )
                .retryable(true)
                .recovery_required(false)
                .emit();
                eprintln!(
                    "{}",
                    json!({
                        "event": "signer_balance_read_failed",
                        "label": signer.label,
                        "pubkey": signer.pubkey.to_string(),
                        "error": redacted_external_error(&error.to_string()),
                    })
                );
                complete = false;
                continue;
            }
        };
        let state = BalanceState::classify(balance, options.low_lamports, options.min_lamports);
        worst = worst.max(state);
        report(signer, balance, state, options);
    }
    Ok(Outcome { complete, worst })
}

fn report(signer: &WatchedSigner, balance: u64, state: BalanceState, options: &Options) {
    let remaining_transactions = balance / options.min_lamports.max(1);
    let payload = json!({
        "event": "signer_balance_observed",
        "label": signer.label,
        "pubkey": signer.pubkey.to_string(),
        "balanceLamports": balance,
        "lowLamports": options.low_lamports,
        "minLamports": options.min_lamports,
        "remainingTransactions": remaining_transactions,
        "state": match state {
            BalanceState::Healthy => "healthy",
            BalanceState::Low => "low",
            BalanceState::Exhausted => "exhausted",
        },
    });
    match state {
        BalanceState::Healthy => println!("{payload}"),
        // Deliberately still an alert, not a silent log: the point of this service is to
        // be heard while a top-up is routine rather than after the fleet has stopped.
        BalanceState::Low => {
            eprintln!("{payload}");
            OperationalError::new(
                "signer_balance_low",
                "fund_fleet_signer",
                "fleet signer SOL balance is running low; top it up before work stops",
            )
            .retryable(false)
            .recovery_required(true)
            .emit();
        }
        BalanceState::Exhausted => {
            eprintln!("{payload}");
            OperationalError::new(
                "signer_balance_exhausted",
                "fund_fleet_signer",
                "fleet signer is out of SOL and cannot pay for transactions",
            )
            .retryable(false)
            .recovery_required(true)
            .emit();
        }
    }
}

/// Parses `label=pubkey` pairs, tolerating a bare pubkey by labelling it with itself.
///
/// The standard policy authority is always watched even when unconfigured. It is a
/// compile-time constant and the signer whose exhaustion stops every autodeposit, so
/// leaving it dependent on deployment configuration would reintroduce the gap this
/// service closes.
fn parse_watched(raw: Option<&str>) -> Result<Vec<WatchedSigner>, Box<dyn Error>> {
    let mut watched = Vec::new();
    let mut seen = BTreeSet::new();
    let standard = Pubkey::from_str(STANDARD_POLICY_AUTHORITY)
        .map_err(|_| "standard policy authority is not a valid public key")?;
    watched.push(WatchedSigner {
        label: "standard_policy_authority".to_owned(),
        pubkey: standard,
    });
    seen.insert(standard);

    for entry in raw.unwrap_or_default().split(',') {
        let entry = entry.trim();
        if entry.is_empty() {
            continue;
        }
        let (label, encoded) = match entry.split_once('=') {
            Some((label, encoded)) => (label.trim().to_owned(), encoded.trim()),
            None => (entry.to_owned(), entry),
        };
        let pubkey = Pubkey::from_str(encoded)
            .map_err(|_| format!("watched signer {label} is not a valid public key"))?;
        if !seen.insert(pubkey) {
            continue;
        }
        watched.push(WatchedSigner { label, pubkey });
    }
    Ok(watched)
}

fn parse_args_with_env<I, F, P>(
    args: I,
    mut env_value: F,
    mut env_present: P,
) -> Result<Options, Box<dyn Error>>
where
    I: IntoIterator<Item = String>,
    F: FnMut(&str) -> Option<String>,
    P: FnMut(&str) -> bool,
{
    for key in FORBIDDEN_ENVIRONMENTS {
        if env_present(key) {
            return Err(format!(
                "environment {key} must not be present in the signerless signer balance monitor"
            )
            .into());
        }
    }

    let rpc_url =
        env_value(RPC_URL_ENV).ok_or("SOLANA_RPC_URL is required for signer balance monitoring")?;
    let cluster = env_value(CLUSTER_ENV).unwrap_or_else(|| "mainnet-beta".to_owned());
    let mut watched_raw = env_value(WATCHED_ENV);
    let mut min_lamports =
        parse_lamports(env_value(MIN_LAMPORTS_ENV).as_deref(), MIN_LAMPORTS_ENV)?
            .unwrap_or(DEFAULT_MIN_LAMPORTS);
    let mut low_lamports =
        parse_lamports(env_value(LOW_LAMPORTS_ENV).as_deref(), LOW_LAMPORTS_ENV)?
            .unwrap_or(DEFAULT_LOW_LAMPORTS);
    let mut interval_secs = parse_lamports(env_value(INTERVAL_ENV).as_deref(), INTERVAL_ENV)?
        .unwrap_or(DEFAULT_INTERVAL_SECS);
    let mut mode = RunMode::Watch;

    let mut args = args.into_iter();
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--once" => mode = RunMode::Once,
            "--watch" => mode = RunMode::Watch,
            "--pubkeys" => watched_raw = Some(next_value(&mut args, "--pubkeys")?),
            "--min-lamports" => {
                min_lamports = next_value(&mut args, "--min-lamports")?
                    .parse()
                    .map_err(|_| "--min-lamports must be a non-negative integer")?
            }
            "--low-lamports" => {
                low_lamports = next_value(&mut args, "--low-lamports")?
                    .parse()
                    .map_err(|_| "--low-lamports must be a non-negative integer")?
            }
            "--interval-secs" => {
                interval_secs = next_value(&mut args, "--interval-secs")?
                    .parse()
                    .map_err(|_| "--interval-secs must be a non-negative integer")?
            }
            other => return Err(format!("unknown argument {other}").into()),
        }
    }

    if low_lamports < min_lamports {
        return Err(
            "--low-lamports must be at least --min-lamports, or the warning can never fire before \
             the floor"
                .into(),
        );
    }

    Ok(Options {
        rpc_url,
        cluster,
        watched: parse_watched(watched_raw.as_deref())?,
        min_lamports,
        low_lamports,
        interval: Duration::from_secs(interval_secs.max(1)),
        mode,
    })
}

fn parse_lamports(raw: Option<&str>, name: &str) -> Result<Option<u64>, Box<dyn Error>> {
    let Some(raw) = raw.map(str::trim).filter(|value| !value.is_empty()) else {
        return Ok(None);
    };
    raw.parse::<u64>()
        .map(Some)
        .map_err(|_| format!("{name} must be a non-negative integer").into())
}

fn next_value<I>(args: &mut I, flag: &str) -> Result<String, Box<dyn Error>>
where
    I: Iterator<Item = String>,
{
    args.next()
        .ok_or_else(|| format!("{flag} requires a value").into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classify_separates_the_floor_from_the_warning() {
        assert_eq!(
            BalanceState::classify(49_999_999, 150_000_000, 50_000_000),
            BalanceState::Exhausted
        );
        assert_eq!(
            BalanceState::classify(50_000_000, 150_000_000, 50_000_000),
            BalanceState::Low
        );
        assert_eq!(
            BalanceState::classify(150_000_000, 150_000_000, 50_000_000),
            BalanceState::Healthy
        );
    }

    #[test]
    fn an_incomplete_scan_never_reports_success() {
        // A read failure leaves `worst` at Healthy, so completeness has to be what
        // decides the exit code; otherwise `--once` reports the fleet funded on a scan
        // that never observed the signer that matters.
        let incomplete = Outcome {
            complete: false,
            worst: BalanceState::Healthy,
        };
        let healthy = Outcome {
            complete: true,
            worst: BalanceState::Healthy,
        };

        assert!(incomplete.worst == BalanceState::Healthy);
        assert!(!incomplete.complete);
        assert!(healthy.complete && healthy.worst == BalanceState::Healthy);
    }

    #[test]
    fn the_standard_policy_authority_is_watched_without_configuration() {
        let watched = parse_watched(None).expect("watched signers");
        assert_eq!(watched.len(), 1);
        assert_eq!(watched[0].pubkey.to_string(), STANDARD_POLICY_AUTHORITY);
    }

    #[test]
    fn watched_signers_accept_labels_and_reject_duplicates() {
        let watched = parse_watched(Some(&format!(
            "router=9v77yTayXd2ezWbiZCwqvJFdtx4Mudf5NQbQGKwY9euk,{STANDARD_POLICY_AUTHORITY}"
        )))
        .expect("watched signers");

        assert_eq!(watched.len(), 2);
        assert_eq!(watched[1].label, "router");
        assert_eq!(
            watched
                .iter()
                .filter(|signer| signer.pubkey.to_string() == STANDARD_POLICY_AUTHORITY)
                .count(),
            1
        );
    }

    #[test]
    fn key_material_in_the_environment_refuses_to_start() {
        let error = parse_args_with_env(
            Vec::new(),
            |key| (key == RPC_URL_ENV).then(|| "https://rpc.example".to_owned()),
            |key| key == "POLICY_KEYPAIR",
        )
        .expect_err("signerless monitor must reject key material");

        assert!(error.to_string().contains("POLICY_KEYPAIR"));
    }

    #[test]
    fn a_warning_below_the_floor_is_rejected_as_unreachable() {
        let error = parse_args_with_env(
            ["--low-lamports".to_owned(), "10".to_owned()],
            |key| (key == RPC_URL_ENV).then(|| "https://rpc.example".to_owned()),
            |_| false,
        )
        .expect_err("an unreachable warning threshold must be rejected");

        assert!(error.to_string().contains("--low-lamports"));
    }
}
