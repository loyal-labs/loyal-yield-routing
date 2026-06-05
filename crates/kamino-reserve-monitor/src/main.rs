use std::{
    collections::HashMap,
    fs::{File, OpenOptions},
    io::{BufWriter, Write},
    path::Path,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};

use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use chrono::{DateTime, Utc};
use kamino_reserve_monitor::{
    cli::{validate_args, Args},
    diff_snapshot, snapshot_from_account,
    source::{
        AccountUpdateEvent, AccountUpdateSource, RpcWebsocketAccountUpdateSource,
        SubscriptionConfig,
    },
    targets::{KaminoApi, ReserveTarget},
    timescale::{ReserveUpdateRecord, TimescaleSink, TimescaleSinkConfig},
    ReserveDiff, ReserveSnapshot,
};
use klend_interface::KLEND_PROGRAM_ID;
use solana_client::rpc_client::RpcClient;
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey};
use tokio::{
    sync::mpsc,
    time::{Instant, MissedTickBehavior},
};
use tracing_subscriber::EnvFilter;

#[derive(Clone, Copy, Debug)]
struct ProcessingConfig {
    slot_duration_ms: f64,
    store_raw: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum SubscriptionRuntimeState {
    Connecting,
    Active,
    Reconnecting,
    Failed,
    Stopped,
}

impl SubscriptionRuntimeState {
    fn is_terminal(self) -> bool {
        matches!(self, Self::Failed | Self::Stopped)
    }
}

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(EnvFilter::from_default_env())
        .init();

    if let Err(err) = run().await {
        tracing::error!(error = %format!("{err:#}"), "fatal");
        eprintln!("fatal: {err:#}");
        std::process::exit(1);
    }
}

async fn run() -> Result<()> {
    let args = Args::parse_args();
    validate_args(&args)?;

    let timescale = TimescaleSink::connect(TimescaleSinkConfig {
        url: args.timescaledb_url.clone(),
        schema: args.timescaledb_schema.clone(),
        ..TimescaleSinkConfig::new(args.timescaledb_url.clone())
    })
    .await?;

    if args.sync_supported_reserves {
        let supported_reserves = fetch_supported_reserves_blocking(
            args.kamino_api_base.clone(),
            args.kamino_api_timeout_secs,
        )
        .await?;
        let count = timescale
            .upsert_supported_reserves(&supported_reserves)
            .await?;
        tracing::info!(count, "supported Kamino reserves synced");
        return Ok(());
    }

    let slot_duration_ms = if args.no_slot_duration_api {
        args.slot_duration_ms
    } else {
        fetch_slot_duration_ms_blocking(args.kamino_api_base.clone(), args.kamino_api_timeout_secs)
            .await
            .unwrap_or_else(|err| {
                tracing::warn!(
                    error = %err,
                    fallback_ms = args.slot_duration_ms,
                    "failed to fetch Kamino slot duration, using fallback"
                );
                args.slot_duration_ms
            })
    };

    let targets = timescale.load_supported_targets(&args.reserve).await?;
    if targets.is_empty() {
        bail!("no active supported Kamino reserve targets selected; run --sync-supported-reserves first");
    }

    let running = Arc::new(AtomicBool::new(true));
    install_ctrlc_handler(running.clone())?;
    let progress_timeout = Duration::from_secs(args.progress_timeout_secs);

    let processing = ProcessingConfig {
        slot_duration_ms,
        store_raw: args.store_raw,
    };
    let mut jsonl = args.jsonl.as_deref().map(open_jsonl_writer).transpose()?;
    let mut snapshots = HashMap::<Pubkey, ReserveSnapshot>::new();

    seed_http_snapshots(
        &args.rpc_url,
        &targets,
        processing,
        &timescale,
        &mut snapshots,
        jsonl.as_mut(),
    )
    .await?;

    if args.once {
        tracing::info!("--once complete after HTTP reserve seed");
        return Ok(());
    }

    let target_by_reserve = targets
        .iter()
        .cloned()
        .map(|target| (target.reserve, target))
        .collect::<HashMap<_, _>>();
    let (tx, rx) = mpsc::unbounded_channel();
    let source = RpcWebsocketAccountUpdateSource {
        ws_url: args.ws_url.clone(),
        config: SubscriptionConfig {
            max_reconnect_attempts: args.max_reconnect_attempts,
            reconnect_base_delay: Duration::from_millis(args.reconnect_base_delay_ms),
            reconnect_max_delay: Duration::from_secs(args.reconnect_max_delay_secs),
            heartbeat_interval: Duration::from_secs(args.subscription_heartbeat_secs),
        },
    };
    let subscription_worker = source.spawn(
        targets.iter().map(|target| target.reserve).collect(),
        tx,
        running.clone(),
    );

    let result = run_event_loop(
        rx,
        &target_by_reserve,
        processing,
        &timescale,
        &mut snapshots,
        jsonl.as_mut(),
        running,
        progress_timeout,
    )
    .await;

    if result.is_ok() {
        if let Err(err) = tokio::time::timeout(Duration::from_secs(10), subscription_worker).await {
            tracing::warn!(error = %err, "timed out waiting for subscription worker shutdown");
        }
    } else {
        subscription_worker.abort();
    }
    result
}

async fn fetch_supported_reserves_blocking(
    kamino_api_base: String,
    timeout_secs: u64,
) -> Result<Vec<kamino_reserve_monitor::targets::SupportedReserveRecord>> {
    tokio::task::spawn_blocking(move || {
        let api = KaminoApi::new(kamino_api_base, Duration::from_secs(timeout_secs))?;
        api.fetch_supported_reserves()
    })
    .await
    .context("join Kamino supported reserve sync task")?
}

async fn fetch_slot_duration_ms_blocking(
    kamino_api_base: String,
    timeout_secs: u64,
) -> Result<f64> {
    tokio::task::spawn_blocking(move || {
        let api = KaminoApi::new(kamino_api_base, Duration::from_secs(timeout_secs))?;
        api.fetch_slot_duration_ms()
    })
    .await
    .context("join Kamino slot duration task")?
}

async fn seed_http_snapshots(
    rpc_url: &str,
    targets: &[ReserveTarget],
    processing: ProcessingConfig,
    timescale: &TimescaleSink,
    snapshots: &mut HashMap<Pubkey, ReserveSnapshot>,
    jsonl: Option<&mut BufWriter<File>>,
) -> Result<()> {
    let rpc = RpcClient::new_with_commitment(rpc_url.to_string(), CommitmentConfig::confirmed());
    let keys = targets
        .iter()
        .map(|target| target.reserve)
        .collect::<Vec<_>>();
    let accounts = rpc
        .get_multiple_accounts(&keys)
        .context("fetch initial reserve accounts")?;
    let slot = rpc.get_slot().context("fetch initial slot")?;
    let mut jsonl = jsonl;

    for (target, account) in targets.iter().zip(accounts) {
        let Some(account) = account else {
            bail!("HTTP seed reserve account {} was missing", target.reserve);
        };
        ensure_klend_owner(&account.owner.to_string(), target, "http_snapshot")?;
        handle_account_data(
            "http_snapshot",
            target,
            slot,
            &account.data,
            Utc::now(),
            Instant::now(),
            processing,
            timescale,
            snapshots,
            jsonl.as_deref_mut(),
        )
        .await?;
    }

    if let Some(writer) = jsonl.as_deref_mut() {
        writer.flush().context("flush seed JSONL")?;
    }
    Ok(())
}

async fn run_event_loop(
    mut rx: mpsc::UnboundedReceiver<AccountUpdateEvent>,
    target_by_reserve: &HashMap<Pubkey, ReserveTarget>,
    processing: ProcessingConfig,
    timescale: &TimescaleSink,
    snapshots: &mut HashMap<Pubkey, ReserveSnapshot>,
    mut jsonl: Option<&mut BufWriter<File>>,
    running: Arc<AtomicBool>,
    progress_timeout: Duration,
) -> Result<()> {
    let mut subscription_states = target_by_reserve
        .keys()
        .copied()
        .map(|reserve| (reserve, SubscriptionRuntimeState::Connecting))
        .collect::<HashMap<_, _>>();
    let mut last_progress_at = Instant::now();
    let mut timeout_tick = tokio::time::interval(Duration::from_millis(500));
    timeout_tick.set_missed_tick_behavior(MissedTickBehavior::Delay);

    while running.load(Ordering::Relaxed) {
        tokio::select! {
            event = rx.recv() => {
                let Some(event) = event else {
                    bail!("subscription event channel disconnected unexpectedly; states={}", format_subscription_states(&subscription_states));
                };
                last_progress_at = Instant::now();
                match event {
                    AccountUpdateEvent::Connecting { reserve, attempt } => {
                        subscription_states.insert(reserve, SubscriptionRuntimeState::Connecting);
                        tracing::info!(%reserve, attempt, "subscription connecting");
                    }
                    AccountUpdateEvent::Connected { reserve, attempt } => {
                        subscription_states.insert(reserve, SubscriptionRuntimeState::Active);
                        tracing::info!(%reserve, attempt, "subscription connected");
                    }
                    AccountUpdateEvent::AccountUpdate { reserve, slot, owner, data, received_at, received_instant } => {
                        subscription_states.insert(reserve, SubscriptionRuntimeState::Active);
                        let Some(target) = target_by_reserve.get(&reserve) else {
                            tracing::debug!(%reserve, "dropping update for unknown reserve");
                            continue;
                        };
                        ensure_klend_owner(&owner, target, "websocket")?;
                        handle_account_data(
                            "websocket",
                            target,
                            slot,
                            &data,
                            received_at,
                            received_instant,
                            processing,
                            timescale,
                            snapshots,
                            jsonl.as_deref_mut(),
                        )
                        .await?;
                    }
                    AccountUpdateEvent::Heartbeat { reserve } => {
                        subscription_states.insert(reserve, SubscriptionRuntimeState::Active);
                    }
                    AccountUpdateEvent::Reconnecting { reserve, attempt, backoff, error } => {
                        subscription_states.insert(reserve, SubscriptionRuntimeState::Reconnecting);
                        tracing::warn!(%reserve, attempt, backoff_ms = backoff.as_millis(), %error, "subscription reconnect scheduled");
                    }
                    AccountUpdateEvent::Failed { reserve, attempts, error } => {
                        subscription_states.insert(reserve, SubscriptionRuntimeState::Failed);
                        tracing::error!(%reserve, attempts, %error, "subscription failed permanently");
                    }
                    AccountUpdateEvent::Stopped { reserve } => {
                        subscription_states.insert(reserve, SubscriptionRuntimeState::Stopped);
                    }
                }
            }
            _ = timeout_tick.tick() => {}
        }

        if subscription_states
            .values()
            .all(|state| state.is_terminal())
        {
            bail!(
                "all subscriptions are stopped or failed; states={}",
                format_subscription_states(&subscription_states)
            );
        }
        if last_progress_at.elapsed() > progress_timeout {
            bail!(
                "no subscription progress for {} seconds; states={}",
                progress_timeout.as_secs(),
                format_subscription_states(&subscription_states)
            );
        }
    }

    if let Some(writer) = jsonl.as_deref_mut() {
        writer.flush().context("flush JSONL before shutdown")?;
    }
    Ok(())
}

async fn handle_account_data(
    source: &'static str,
    target: &ReserveTarget,
    slot: u64,
    data: &[u8],
    received_at: DateTime<Utc>,
    received_instant: Instant,
    processing: ProcessingConfig,
    timescale: &TimescaleSink,
    snapshots: &mut HashMap<Pubkey, ReserveSnapshot>,
    jsonl: Option<&mut BufWriter<File>>,
) -> Result<()> {
    let decode_started_at = Instant::now();
    let snapshot = snapshot_from_account(target, slot, data, processing.slot_duration_ms)
        .with_context(|| format!("parse reserve account {}", target.reserve))?;
    validate_snapshot_target(target, &snapshot, source)?;

    let diff = snapshots
        .get(&target.reserve)
        .map(|previous| diff_snapshot(previous, &snapshot));
    let diff_summary = diff
        .as_ref()
        .map(ReserveDiff::summary)
        .unwrap_or_else(|| "initial_snapshot".to_string());
    let decoded_at = Utc::now();
    let decoded_instant = Instant::now();
    let receive_to_decode_ms = received_instant.elapsed().as_millis();
    let decode_latency_ms = decoded_instant
        .duration_since(decode_started_at)
        .as_millis();

    let enriched_target = target_with_snapshot_metadata(target, &snapshot);
    let raw_account_data_base64 = processing.store_raw.then(|| BASE64_STANDARD.encode(data));
    let account_data_hash = TimescaleSink::account_data_hash(data);
    let record = ReserveUpdateRecord {
        kind: "reserve_update",
        source,
        observed_at: snapshot.observed_at,
        slot,
        target: &enriched_target,
        snapshot: &snapshot,
        diff_summary: &diff_summary,
        diff: diff.as_ref(),
        raw_account_data_base64: raw_account_data_base64.as_deref(),
        source_commitment: "confirmed",
        account_data_hash: &account_data_hash,
        received_at,
        decoded_at,
        receive_to_decode_ms,
    };

    if let Some(writer) = jsonl {
        serde_json::to_writer(&mut *writer, &record).context("write JSONL reserve update")?;
        writer.write_all(b"\n").context("write JSONL newline")?;
    }
    let insert_outcome = timescale.insert(&record).await?;
    let event_id = insert_outcome.event_id;

    tracing::info!(
        source,
        event_id,
        inserted = insert_outcome.inserted,
        slot = snapshot.slot,
        reserve = %snapshot.reserve,
        symbol = snapshot.symbol.as_deref().unwrap_or("UNKNOWN"),
        supply_apy_bps = ratio_to_bps(snapshot.supply_apy),
        borrow_apy_bps = ratio_to_bps(snapshot.borrow_apy),
        utilization_bps = ratio_to_bps(snapshot.utilization),
        receive_to_decode_ms,
        decode_latency_ms,
        diff_summary,
        "reserve update processed"
    );

    snapshots.insert(target.reserve, snapshot);
    Ok(())
}

fn ensure_klend_owner(owner: &str, target: &ReserveTarget, source: &str) -> Result<()> {
    let expected = KLEND_PROGRAM_ID.to_string();
    if owner == expected {
        return Ok(());
    }
    bail!(
        "{source} reserve {} has owner {owner}, expected KLend program {expected}",
        target.reserve
    )
}

fn validate_snapshot_target(
    target: &ReserveTarget,
    snapshot: &ReserveSnapshot,
    source: &str,
) -> Result<()> {
    if let Some(expected_market) = target.market {
        if snapshot.market != Some(expected_market) {
            bail!(
                "{source} reserve {} decoded market {:?}, expected {}",
                target.reserve,
                snapshot.market,
                expected_market
            );
        }
    }
    if let Some(expected_mint) = target.liquidity_mint {
        if snapshot.liquidity_mint != expected_mint {
            bail!(
                "{source} reserve {} decoded liquidity mint {}, expected {}",
                target.reserve,
                snapshot.liquidity_mint,
                expected_mint
            );
        }
    }
    Ok(())
}

fn target_with_snapshot_metadata(
    target: &ReserveTarget,
    snapshot: &ReserveSnapshot,
) -> ReserveTarget {
    let mut enriched = target.clone();
    if enriched.market.is_none() {
        enriched.market = snapshot.market;
    }
    if enriched.symbol.is_none() {
        enriched.symbol = snapshot.symbol.clone();
    }
    if enriched.liquidity_mint.is_none() {
        enriched.liquidity_mint = Some(snapshot.liquidity_mint);
    }
    enriched
}

fn format_subscription_states(states: &HashMap<Pubkey, SubscriptionRuntimeState>) -> String {
    let mut entries = states
        .iter()
        .map(|(reserve, state)| format!("{reserve}={state:?}"))
        .collect::<Vec<_>>();
    entries.sort();
    entries.join(", ")
}

fn ratio_to_bps(value: f64) -> i64 {
    (value * 10_000.0).round() as i64
}

fn open_jsonl_writer(path: &Path) -> Result<BufWriter<File>> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)
            .with_context(|| format!("create JSONL output directory {parent:?}"))?;
    }
    let file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
        .with_context(|| format!("open JSONL output {path:?}"))?;
    Ok(BufWriter::new(file))
}

fn install_ctrlc_handler(running: Arc<AtomicBool>) -> Result<()> {
    ctrlc::set_handler(move || {
        running.store(false, Ordering::Relaxed);
    })
    .context("install Ctrl-C handler")
}
