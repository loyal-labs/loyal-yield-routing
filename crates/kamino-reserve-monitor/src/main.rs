use std::{
    collections::HashMap,
    fs::{File, OpenOptions},
    io::{BufRead, BufReader, BufWriter, Write},
    path::Path,
    process::{Command, Stdio},
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::{Duration, Instant as StdInstant},
};

use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use chrono::TimeZone;
use chrono::{DateTime, Utc};
use kamino_reserve_monitor::{
    cli::{validate_args, Args, UpdateSourceKind},
    diff_snapshot, snapshot_from_account, snapshot_from_account_at,
    source::{
        AccountUpdateEvent, AccountUpdateSource, LaserstreamAccountUpdateSource,
        RpcWebsocketAccountUpdateSource, SubscriptionConfig, UpdateSourceMetadata,
        CONFIRMED_COMMITMENT,
    },
    targets::{KaminoApi, ReserveTarget},
    timescale::{ReserveUpdateRecord, TimescaleSink, TimescaleSinkConfig},
    ReserveDiff, ReserveSnapshot,
};
use klend_interface::KLEND_PROGRAM_ID;
use serde::Deserialize;
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

#[derive(Clone, Copy, Debug)]
struct SlotTimeEstimator {
    start_block: u64,
    start_unix: i64,
    stop_block: u64,
    stop_unix: i64,
}

impl SlotTimeEstimator {
    fn observed_at(self, slot: u64) -> Result<DateTime<Utc>> {
        let slot = slot.clamp(self.start_block, self.stop_block);
        let slot_span = (self.stop_block - self.start_block) as f64;
        let time_span = (self.stop_unix - self.start_unix) as f64;
        let offset_slots = (slot - self.start_block) as f64;
        let timestamp = self.start_unix + (offset_slots * time_span / slot_span).round() as i64;
        Utc.timestamp_opt(timestamp, 0)
            .single()
            .with_context(|| format!("derive observed_at for slot {slot}"))
    }
}

#[derive(Debug, Deserialize)]
struct SubstreamsEnvelope {
    #[serde(rename = "@block")]
    block: u64,
    #[serde(rename = "@data")]
    data: SubstreamsData,
}

#[derive(Debug, Deserialize)]
struct SubstreamsData {
    accounts: Vec<SubstreamsAccount>,
}

#[derive(Debug, Deserialize)]
struct SubstreamsAccount {
    address: String,
    owner: String,
    data: String,
}

#[derive(Clone, Debug)]
struct OwnedReserveUpdate {
    metadata: UpdateSourceMetadata,
    slot: u64,
    target: ReserveTarget,
    snapshot: ReserveSnapshot,
    diff_summary: String,
    diff: Option<ReserveDiff>,
    raw_account_data_base64: Option<String>,
    account_data_hash: String,
    received_at: DateTime<Utc>,
    decoded_at: DateTime<Utc>,
    receive_to_decode_ms: u128,
}

impl OwnedReserveUpdate {
    fn as_record(&self) -> ReserveUpdateRecord<'_> {
        ReserveUpdateRecord {
            kind: "reserve_update",
            source: self.metadata.source,
            observed_at: self.snapshot.observed_at,
            slot: self.slot,
            target: &self.target,
            snapshot: &self.snapshot,
            diff_summary: &self.diff_summary,
            diff: self.diff.as_ref(),
            raw_account_data_base64: self.raw_account_data_base64.as_deref(),
            source_commitment: self.metadata.source_commitment,
            account_data_hash: &self.account_data_hash,
            received_at: self.received_at,
            decoded_at: self.decoded_at,
            receive_to_decode_ms: self.receive_to_decode_ms,
        }
    }
}

#[derive(Debug)]
struct SubstreamsProgress {
    started_at: StdInstant,
    start_block: u64,
    stop_block: u64,
    total_rows: u64,
    reserve_rows: HashMap<Pubkey, u64>,
}

impl SubstreamsProgress {
    fn new(start_block: u64, stop_block: u64) -> Self {
        Self {
            started_at: StdInstant::now(),
            start_block,
            stop_block,
            total_rows: 0,
            reserve_rows: HashMap::new(),
        }
    }

    fn record_row(&mut self, reserve: Pubkey) -> u64 {
        self.total_rows += 1;
        let reserve_count = self.reserve_rows.entry(reserve).or_default();
        *reserve_count += 1;
        *reserve_count
    }

    fn percent_at(&self, block: u64) -> f64 {
        percent_between(block, self.start_block, self.stop_block)
    }

    fn eta_at(&self, block: u64) -> Option<Duration> {
        let completed = block.saturating_sub(self.start_block);
        if completed < 10_000 {
            return None;
        }
        let total = self.stop_block.saturating_sub(self.start_block);
        let remaining = total.saturating_sub(completed);
        let elapsed = self.started_at.elapsed().as_secs_f64();
        let seconds = elapsed * remaining as f64 / completed as f64;
        seconds
            .is_finite()
            .then(|| Duration::from_secs_f64(seconds))
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

    let mut timescale_config = TimescaleSinkConfig {
        url: args.timescaledb_url.clone(),
        schema: args.timescaledb_schema.clone(),
        ..TimescaleSinkConfig::new(args.timescaledb_url.clone())
    };
    if args.substreams_backfill {
        timescale_config.max_connections = args.substreams_insert_concurrency as u32;
        timescale_config.acquire_timeout = Duration::from_secs(30);
    }
    let timescale = TimescaleSink::connect(timescale_config).await?;

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

    if args.substreams_backfill {
        let estimator = SlotTimeEstimator {
            start_block: args
                .substreams_start_block
                .expect("validated substreams start block"),
            start_unix: args
                .substreams_start_unix
                .expect("validated substreams start unix"),
            stop_block: args
                .substreams_stop_block
                .expect("validated substreams stop block"),
            stop_unix: args
                .substreams_stop_unix
                .expect("validated substreams stop unix"),
        };
        let processing = ProcessingConfig {
            slot_duration_ms,
            store_raw: args.store_raw,
        };
        let mut jsonl = args.jsonl.as_deref().map(open_jsonl_writer).transpose()?;
        run_substreams_backfill(
            &args,
            &targets,
            processing,
            estimator,
            &timescale,
            jsonl.as_mut(),
        )
        .await?;
        return Ok(());
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

    let seed_slot = seed_http_snapshots(
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
    let subscription_config = SubscriptionConfig {
        max_reconnect_attempts: args.max_reconnect_attempts,
        reconnect_base_delay: Duration::from_millis(args.reconnect_base_delay_ms),
        reconnect_max_delay: Duration::from_secs(args.reconnect_max_delay_secs),
        heartbeat_interval: Duration::from_secs(args.subscription_heartbeat_secs),
    };
    let subscription_worker = match args.update_source {
        UpdateSourceKind::Laserstream => {
            let source = LaserstreamAccountUpdateSource {
                endpoint: args
                    .laserstream_endpoint
                    .clone()
                    .expect("validated LaserStream endpoint"),
                api_key: args
                    .helius_api_key
                    .clone()
                    .expect("validated Helius API key"),
                from_slot: seed_slot.saturating_sub(args.laserstream_replay_overlap_slots),
                config: subscription_config,
            };
            source.spawn(
                targets.iter().map(|target| target.reserve).collect(),
                tx,
                running.clone(),
            )
        }
        UpdateSourceKind::Websocket => {
            let source = RpcWebsocketAccountUpdateSource {
                ws_url: args.ws_url.clone().expect("validated websocket URL"),
                config: subscription_config,
            };
            source.spawn(
                targets.iter().map(|target| target.reserve).collect(),
                tx,
                running.clone(),
            )
        }
    };

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

async fn run_substreams_backfill(
    args: &Args,
    targets: &[ReserveTarget],
    processing: ProcessingConfig,
    estimator: SlotTimeEstimator,
    timescale: &TimescaleSink,
    jsonl: Option<&mut BufWriter<File>>,
) -> Result<()> {
    let target_by_reserve = targets
        .iter()
        .cloned()
        .map(|target| (target.reserve, target))
        .collect::<HashMap<_, _>>();
    let filter = build_substreams_account_filter(targets);
    let mut snapshots = HashMap::<Pubkey, ReserveSnapshot>::new();
    let mut jsonl = jsonl;
    let mut start_block = estimator.start_block;
    let mut total_rows = 0_u64;
    let mut chunk_index = 0_u64;
    let total_blocks = estimator.stop_block - estimator.start_block;
    let total_chunks = total_blocks.div_ceil(args.substreams_chunk_blocks);
    let mut progress = SubstreamsProgress::new(estimator.start_block, estimator.stop_block);

    eprintln!(
        "Substreams backfill starting: reserves={} blocks={}..{} chunks={} chunk_blocks={} progress_rows={} insert_batch_size={} production_mode={} parallel_workers={}",
        targets.len(),
        estimator.start_block,
        estimator.stop_block,
        total_chunks,
        args.substreams_chunk_blocks,
        args.substreams_progress_rows,
        args.substreams_insert_batch_size,
        args.substreams_production_mode,
        args.substreams_parallel_workers
            .map(|workers| workers.to_string())
            .unwrap_or_else(|| "none".to_string())
    );

    while start_block < estimator.stop_block {
        chunk_index += 1;
        let stop_block = start_block
            .saturating_add(args.substreams_chunk_blocks)
            .min(estimator.stop_block);
        eprintln!(
            "chunk {chunk_index}/{total_chunks} starting: blocks {start_block}..{stop_block} overall={} elapsed={}",
            format_percent(progress.percent_at(start_block)),
            format_duration(progress.started_at.elapsed())
        );
        let chunk_rows = run_substreams_chunk(
            args,
            &filter,
            start_block,
            stop_block,
            &target_by_reserve,
            processing,
            estimator,
            timescale,
            &mut snapshots,
            &mut progress,
            jsonl.as_deref_mut(),
        )
        .await
        .with_context(|| format!("backfill Substreams blocks {start_block}..{stop_block}"))?;
        total_rows += chunk_rows;
        eprintln!(
            "chunk {chunk_index}/{total_chunks} complete: rows={chunk_rows} total_rows={total_rows} reserves_seen={}/{} overall={} elapsed={} eta={}",
            progress.reserve_rows.len(),
            targets.len(),
            format_percent(progress.percent_at(stop_block)),
            format_duration(progress.started_at.elapsed()),
            progress
                        .eta_at(stop_block)
                        .map(format_duration)
                        .unwrap_or_else(|| "warming_up".to_string())
        );
        tracing::info!(
            start_block,
            stop_block,
            chunk_rows,
            total_rows,
            "Substreams backfill chunk complete"
        );
        start_block = stop_block;
    }

    if let Some(writer) = jsonl.as_deref_mut() {
        writer.flush().context("flush Substreams backfill JSONL")?;
    }
    tracing::info!(
        total_rows,
        reserves = targets.len(),
        start_block = estimator.start_block,
        stop_block = estimator.stop_block,
        "Substreams backfill complete"
    );
    Ok(())
}

async fn run_substreams_chunk(
    args: &Args,
    filter: &str,
    start_block: u64,
    stop_block: u64,
    target_by_reserve: &HashMap<Pubkey, ReserveTarget>,
    processing: ProcessingConfig,
    estimator: SlotTimeEstimator,
    timescale: &TimescaleSink,
    snapshots: &mut HashMap<Pubkey, ReserveSnapshot>,
    progress: &mut SubstreamsProgress,
    mut jsonl: Option<&mut BufWriter<File>>,
) -> Result<u64> {
    let params = format!("{}={filter}", args.substreams_module);
    let limit_processed_blocks = (stop_block - start_block).saturating_add(1_000);
    let mut command = Command::new(&args.substreams_cli);
    command
        .arg("run")
        .arg(&args.substreams_package)
        .arg(&args.substreams_module)
        .arg("-p")
        .arg(params)
        .arg("--bytes-encoding")
        .arg("base64")
        .arg("--start-block")
        .arg(start_block.to_string())
        .arg("--stop-block")
        .arg(stop_block.to_string())
        .arg("--output")
        .arg("jsonl")
        .arg("--limit-processed-blocks")
        .arg(limit_processed_blocks.to_string())
        .arg("--api-key-envvar")
        .arg(&args.substreams_api_key_envvar);
    if args.substreams_production_mode {
        command.arg("--production-mode");
    }
    if let Some(workers) = args.substreams_parallel_workers {
        command
            .arg("-H")
            .arg(format!("X-Substreams-Parallel-Workers={workers}"));
    }
    let mut child = command
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .spawn()
        .with_context(|| format!("spawn Substreams CLI {:?}", args.substreams_cli))?;
    let stdout = child.stdout.take().context("capture Substreams stdout")?;
    let reader = BufReader::new(stdout);
    let mut rows = 0_u64;
    let mut pending_updates =
        Vec::<OwnedReserveUpdate>::with_capacity(args.substreams_insert_batch_size.min(10_000));
    let mut inserted_rows = 0_usize;

    for line in reader.lines() {
        let line = line.context("read Substreams JSONL line")?;
        let trimmed = line.trim_start();
        if !trimmed.starts_with('{') {
            continue;
        }
        let envelope: SubstreamsEnvelope =
            serde_json::from_str(trimmed).context("parse Substreams JSONL envelope")?;
        for account in envelope.data.accounts {
            let reserve = decode_pubkey_base64(&account.address, "account address")?;
            let Some(target) = target_by_reserve.get(&reserve) else {
                tracing::debug!(%reserve, "dropping Substreams account for unknown reserve");
                continue;
            };
            let reserve_count = progress.record_row(reserve);
            let owner = decode_pubkey_base64(&account.owner, "account owner")?;
            ensure_klend_owner(&owner.to_string(), target, "substreams_backfill")?;
            let data = BASE64_STANDARD
                .decode(&account.data)
                .context("decode Substreams account data")?;
            let observed_at = estimator.observed_at(envelope.block)?;
            let now = Utc::now();
            let update = build_owned_update(
                UpdateSourceMetadata {
                    source: "substreams_backfill",
                    source_commitment: CONFIRMED_COMMITMENT,
                },
                target,
                envelope.block,
                &data,
                Some(observed_at),
                now,
                Instant::now(),
                processing,
                snapshots,
            )?;
            if let Some(writer) = jsonl.as_deref_mut() {
                let record = update.as_record();
                serde_json::to_writer(&mut *writer, &record)
                    .context("write Substreams backfill JSONL reserve update")?;
                writer
                    .write_all(b"\n")
                    .context("write Substreams backfill JSONL newline")?;
            }
            snapshots.insert(target.reserve, update.snapshot.clone());
            pending_updates.push(update);
            rows += 1;
            if progress.total_rows == 1 || progress.total_rows % args.substreams_progress_rows == 0
            {
                eprintln!(
                    "progress: rows={} chunk_rows={} slot={} overall={} reserves_seen={}/{} current_reserve=\"{}\" reserve_rows={} elapsed={} eta={}",
                    progress.total_rows,
                    rows,
                    envelope.block,
                    format_percent(progress.percent_at(envelope.block)),
                    progress.reserve_rows.len(),
                    target_by_reserve.len(),
                    reserve_label(target),
                    reserve_count,
                    format_duration(progress.started_at.elapsed()),
                    progress
                        .eta_at(envelope.block)
                        .map(format_duration)
                        .unwrap_or_else(|| "warming_up".to_string())
                );
            }
            if pending_updates.len() >= args.substreams_insert_batch_size {
                inserted_rows += flush_substreams_batch(timescale, &mut pending_updates).await?;
            }
        }
    }

    inserted_rows += flush_substreams_batch(timescale, &mut pending_updates).await?;

    let status = child.wait().context("wait for Substreams CLI")?;
    if !status.success() {
        bail!("Substreams CLI exited with {status}");
    }
    tracing::info!(
        rows,
        inserted_rows,
        start_block,
        stop_block,
        "Substreams chunk inserted"
    );
    Ok(rows)
}

fn build_substreams_account_filter(targets: &[ReserveTarget]) -> String {
    targets
        .iter()
        .map(|target| format!("account:{}", target.reserve))
        .collect::<Vec<_>>()
        .join(" || ")
}

async fn flush_substreams_batch(
    timescale: &TimescaleSink,
    pending_updates: &mut Vec<OwnedReserveUpdate>,
) -> Result<usize> {
    if pending_updates.is_empty() {
        return Ok(0);
    }
    let records = pending_updates
        .iter()
        .map(OwnedReserveUpdate::as_record)
        .collect::<Vec<_>>();
    let inserted = timescale.insert_batch_skip_duplicates(&records).await?;
    pending_updates.clear();
    Ok(inserted)
}

fn decode_pubkey_base64(encoded: &str, label: &str) -> Result<Pubkey> {
    let bytes = BASE64_STANDARD
        .decode(encoded)
        .with_context(|| format!("decode Substreams {label}"))?;
    if bytes.len() != 32 {
        bail!(
            "Substreams {label} decoded to {} bytes, expected 32",
            bytes.len()
        );
    }
    let mut array = [0_u8; 32];
    array.copy_from_slice(&bytes);
    Ok(Pubkey::new_from_array(array))
}

fn reserve_label(target: &ReserveTarget) -> String {
    match (&target.market_name, &target.symbol) {
        (Some(market), Some(symbol)) => format!("{market} {symbol} {}", target.reserve),
        (Some(market), None) => format!("{market} {}", target.reserve),
        (None, Some(symbol)) => format!("{symbol} {}", target.reserve),
        (None, None) => target.reserve.to_string(),
    }
}

fn percent_between(block: u64, start_block: u64, stop_block: u64) -> f64 {
    let total = stop_block.saturating_sub(start_block);
    if total == 0 {
        return 100.0;
    }
    let completed = block.saturating_sub(start_block).min(total);
    completed as f64 * 100.0 / total as f64
}

fn format_percent(value: f64) -> String {
    format!("{value:.2}%")
}

fn format_duration(duration: Duration) -> String {
    let total = duration.as_secs();
    let hours = total / 3_600;
    let minutes = (total % 3_600) / 60;
    let seconds = total % 60;
    if hours > 0 {
        format!("{hours}h{minutes:02}m{seconds:02}s")
    } else if minutes > 0 {
        format!("{minutes}m{seconds:02}s")
    } else {
        format!("{seconds}s")
    }
}

async fn seed_http_snapshots(
    rpc_url: &str,
    targets: &[ReserveTarget],
    processing: ProcessingConfig,
    timescale: &TimescaleSink,
    snapshots: &mut HashMap<Pubkey, ReserveSnapshot>,
    jsonl: Option<&mut BufWriter<File>>,
) -> Result<u64> {
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
            UpdateSourceMetadata {
                source: "http_snapshot",
                source_commitment: CONFIRMED_COMMITMENT,
            },
            target,
            slot,
            &account.data,
            None,
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
    Ok(slot)
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
                        tracing::debug!(%reserve, attempt, "subscription connecting");
                    }
                    AccountUpdateEvent::Connected { reserve, attempt } => {
                        subscription_states.insert(reserve, SubscriptionRuntimeState::Active);
                        tracing::debug!(%reserve, attempt, "subscription connected");
                    }
                    AccountUpdateEvent::AccountUpdate { metadata, reserve, slot, owner, data, received_at, received_instant } => {
                        subscription_states.insert(reserve, SubscriptionRuntimeState::Active);
                        let Some(target) = target_by_reserve.get(&reserve) else {
                            tracing::debug!(%reserve, "dropping update for unknown reserve");
                            continue;
                        };
                        ensure_klend_owner(&owner, target, metadata.source)?;
                        handle_account_data(
                            metadata,
                            target,
                            slot,
                            &data,
                            None,
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
    metadata: UpdateSourceMetadata,
    target: &ReserveTarget,
    slot: u64,
    data: &[u8],
    observed_at: Option<DateTime<Utc>>,
    received_at: DateTime<Utc>,
    received_instant: Instant,
    processing: ProcessingConfig,
    timescale: &TimescaleSink,
    snapshots: &mut HashMap<Pubkey, ReserveSnapshot>,
    jsonl: Option<&mut BufWriter<File>>,
) -> Result<()> {
    let update = build_owned_update(
        metadata,
        target,
        slot,
        data,
        observed_at,
        received_at,
        received_instant,
        processing,
        snapshots,
    )?;

    if let Some(writer) = jsonl {
        let record = update.as_record();
        serde_json::to_writer(&mut *writer, &record).context("write JSONL reserve update")?;
        writer.write_all(b"\n").context("write JSONL newline")?;
    }
    let insert_outcome = insert_owned_update(timescale.clone(), update.clone()).await?;
    let event_id = insert_outcome.event_id;

    tracing::debug!(
        source = update.metadata.source,
        event_id,
        inserted = insert_outcome.inserted,
        slot = update.snapshot.slot,
        reserve = %update.snapshot.reserve,
        symbol = update.snapshot.symbol.as_deref().unwrap_or("UNKNOWN"),
        supply_apy_bps = ratio_to_bps(update.snapshot.supply_apy),
        borrow_apy_bps = ratio_to_bps(update.snapshot.borrow_apy),
        utilization_bps = ratio_to_bps(update.snapshot.utilization),
        receive_to_decode_ms = update.receive_to_decode_ms,
        diff_summary = update.diff_summary,
        "reserve update processed"
    );

    snapshots.insert(target.reserve, update.snapshot);
    Ok(())
}

fn build_owned_update(
    metadata: UpdateSourceMetadata,
    target: &ReserveTarget,
    slot: u64,
    data: &[u8],
    observed_at: Option<DateTime<Utc>>,
    received_at: DateTime<Utc>,
    received_instant: Instant,
    processing: ProcessingConfig,
    snapshots: &HashMap<Pubkey, ReserveSnapshot>,
) -> Result<OwnedReserveUpdate> {
    let snapshot = if let Some(observed_at) = observed_at {
        snapshot_from_account_at(target, slot, data, processing.slot_duration_ms, observed_at)
    } else {
        snapshot_from_account(target, slot, data, processing.slot_duration_ms)
    }
    .with_context(|| format!("parse reserve account {}", target.reserve))?;
    validate_snapshot_target(target, &snapshot, metadata.source)?;

    let diff = snapshots
        .get(&target.reserve)
        .map(|previous| diff_snapshot(previous, &snapshot));
    let diff_summary = diff
        .as_ref()
        .map(ReserveDiff::summary)
        .unwrap_or_else(|| "initial_snapshot".to_string());
    let decoded_at = Utc::now();
    let receive_to_decode_ms = received_instant.elapsed().as_millis();
    let target = target_with_snapshot_metadata(target, &snapshot);
    let raw_account_data_base64 = processing.store_raw.then(|| BASE64_STANDARD.encode(data));
    let account_data_hash = TimescaleSink::account_data_hash(data);

    Ok(OwnedReserveUpdate {
        metadata,
        slot,
        target,
        snapshot,
        diff_summary,
        diff,
        raw_account_data_base64,
        account_data_hash,
        received_at,
        decoded_at,
        receive_to_decode_ms,
    })
}

async fn insert_owned_update(
    timescale: TimescaleSink,
    update: OwnedReserveUpdate,
) -> Result<kamino_reserve_monitor::timescale::TimescaleInsertOutcome> {
    let record = update.as_record();
    timescale.insert(&record).await
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
