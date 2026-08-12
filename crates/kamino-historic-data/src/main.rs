use std::{
    collections::HashMap,
    fs::{File, OpenOptions},
    io::{BufRead, BufReader, BufWriter, Write},
    path::{Path, PathBuf},
    process::{Command, Stdio},
    sync::mpsc::{self as std_mpsc, RecvTimeoutError},
    thread,
    time::{Duration, Instant as StdInstant},
};

use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use chrono::TimeZone;
use chrono::{DateTime, Utc};
use klend_interface::KLEND_PROGRAM_ID;
use loyal_kamino_codec::{
    diff_snapshot, snapshot_from_account, snapshot_from_account_at, ReserveDiff, ReserveSnapshot,
};
use loyal_kamino_data::{
    source_metadata::{UpdateSourceMetadata, CONFIRMED_COMMITMENT},
    targets::{KaminoApi, ReserveTarget},
    timescale::{ReserveUpdateRecord, TimescaleSink, TimescaleSinkConfig},
};
use serde::Deserialize;
use serde_json::{json, Map, Value};
use solana_sdk::pubkey::Pubkey;
use tokio::{task::JoinSet, time::Instant};
use tracing_subscriber::EnvFilter;

mod cli;
mod substreams_grpc;

use cli::{validate_args, Args, SubstreamsTransport};
use substreams_grpc::{decode_block_output, SubstreamsGrpcAdapterEvent};

#[derive(Clone, Copy, Debug)]
struct ProcessingConfig {
    slot_duration_ms: f64,
    store_raw: bool,
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

#[derive(Clone, Copy, Debug)]
struct SubstreamsChunk {
    index: u64,
    total: u64,
    start_block: u64,
    stop_block: u64,
}

#[derive(Debug)]
struct SubstreamsParallelChunkResult {
    chunk: SubstreamsChunk,
    rows: u64,
    shard_path: PathBuf,
    elapsed: Duration,
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
    if args.substreams_backfill || !args.import_jsonl.is_empty() {
        timescale_config.max_connections = args.substreams_insert_concurrency as u32;
        timescale_config.acquire_timeout = Duration::from_secs(30);
    }
    let timescale = TimescaleSink::connect(timescale_config).await?;

    if !args.import_jsonl.is_empty() {
        import_jsonl_shards(&args, &timescale).await?;
        return Ok(());
    }

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
        if args.substreams_concurrent_streams > 1 {
            run_substreams_backfill(&args, &targets, processing, estimator, &timescale, None)
                .await?;
        } else {
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
        }
        return Ok(());
    }

    bail!("historic data command requires --sync-supported-reserves, --substreams-backfill, or --import-jsonl");
}

async fn fetch_supported_reserves_blocking(
    kamino_api_base: String,
    timeout_secs: u64,
) -> Result<Vec<loyal_kamino_codec::SupportedReserveRecord>> {
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
    if args.substreams_concurrent_streams > 1 {
        return run_substreams_parallel_backfill(args, targets, processing, estimator, timescale)
            .await;
    }

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
        "Substreams backfill starting: transport={:?} reserves={} blocks={}..{} chunks={} chunk_blocks={} progress_rows={} insert_batch_size={} production_mode={} parallel_workers={} grpc_adapter={}",
        args.substreams_transport,
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
            .unwrap_or_else(|| "none".to_string()),
        args.substreams_grpc_adapter.display()
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
        let chunk_rows = match args.substreams_transport {
            SubstreamsTransport::Grpc => {
                run_substreams_grpc_chunk(
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
                    args.substreams_progress_rows,
                    args.substreams_insert_batch_size,
                    jsonl.as_deref_mut(),
                )
                .await
            }
            SubstreamsTransport::Cli => {
                run_substreams_cli_chunk(
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
            }
        }
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

    if let Some(writer) = jsonl {
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

async fn run_substreams_parallel_backfill(
    args: &Args,
    targets: &[ReserveTarget],
    processing: ProcessingConfig,
    estimator: SlotTimeEstimator,
    timescale: &TimescaleSink,
) -> Result<()> {
    let jsonl_base_path = args
        .jsonl
        .as_deref()
        .context("validated JSONL path for parallel Substreams backfill")?;
    let chunks = substreams_chunks(estimator, args.substreams_chunk_blocks);
    let total_chunks = chunks.len() as u64;
    let stream_limit = args.substreams_concurrent_streams.min(chunks.len());
    let filter = build_substreams_account_filter(targets);
    let target_by_reserve = targets
        .iter()
        .cloned()
        .map(|target| (target.reserve, target))
        .collect::<HashMap<_, _>>();
    let started_at = StdInstant::now();
    let mut join_set = JoinSet::<Result<SubstreamsParallelChunkResult>>::new();
    let mut next_chunk = 0_usize;
    let mut completed_chunks = 0_u64;
    let mut total_rows = 0_u64;

    eprintln!(
        "Substreams parallel backfill starting: transport={:?} reserves={} blocks={}..{} chunks={} chunk_blocks={} concurrent_streams={} parallel_workers_per_stream={} total_worker_budget={} progress_rows={} insert_batch_size={} production_mode={} shard_base={}",
        args.substreams_transport,
        targets.len(),
        estimator.start_block,
        estimator.stop_block,
        total_chunks,
        args.substreams_chunk_blocks,
        stream_limit,
        args.substreams_parallel_workers
            .map(|workers| workers.to_string())
            .unwrap_or_else(|| "provider_default".to_string()),
        args.substreams_parallel_workers.unwrap_or_default() * stream_limit,
        args.substreams_progress_rows,
        args.substreams_insert_batch_size,
        args.substreams_production_mode,
        jsonl_base_path.display()
    );

    while next_chunk < chunks.len() || !join_set.is_empty() {
        while next_chunk < chunks.len() && join_set.len() < stream_limit {
            let chunk = chunks[next_chunk];
            next_chunk += 1;
            spawn_substreams_parallel_chunk(
                &mut join_set,
                args.clone(),
                filter.clone(),
                target_by_reserve.clone(),
                processing,
                estimator,
                timescale.clone(),
                chunk,
                shard_jsonl_path(jsonl_base_path, chunk),
            );
            eprintln!(
                "parallel chunk {}/{} scheduled: blocks {}..{} active_streams={} queued={} elapsed={}",
                chunk.index,
                chunk.total,
                chunk.start_block,
                chunk.stop_block,
                join_set.len(),
                chunks.len().saturating_sub(next_chunk),
                format_duration(started_at.elapsed())
            );
        }

        let Some(joined) = join_set.join_next().await else {
            break;
        };
        let result = joined.context("join parallel Substreams chunk task")??;
        completed_chunks += 1;
        total_rows += result.rows;
        let last_completed_block = result.chunk.stop_block;
        eprintln!(
            "parallel chunk {}/{} complete: rows={} total_rows={} completed_chunks={}/{} active_streams={} shard={} overall={} elapsed={} chunk_elapsed={} eta={}",
            result.chunk.index,
            result.chunk.total,
            result.rows,
            total_rows,
            completed_chunks,
            total_chunks,
            join_set.len(),
            result.shard_path.display(),
            format_percent(percent_between(
                last_completed_block,
                estimator.start_block,
                estimator.stop_block
            )),
            format_duration(started_at.elapsed()),
            format_duration(result.elapsed),
            parallel_eta(started_at.elapsed(), completed_chunks, total_chunks)
                .map(format_duration)
                .unwrap_or_else(|| "warming_up".to_string())
        );
    }

    tracing::info!(
        total_rows,
        reserves = targets.len(),
        start_block = estimator.start_block,
        stop_block = estimator.stop_block,
        chunks = total_chunks,
        stream_limit,
        "Substreams parallel backfill complete"
    );
    eprintln!(
        "Substreams parallel backfill complete: chunks={} rows={} elapsed={} shard_base={}",
        total_chunks,
        total_rows,
        format_duration(started_at.elapsed()),
        jsonl_base_path.display()
    );
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn spawn_substreams_parallel_chunk(
    join_set: &mut JoinSet<Result<SubstreamsParallelChunkResult>>,
    args: Args,
    filter: String,
    target_by_reserve: HashMap<Pubkey, ReserveTarget>,
    processing: ProcessingConfig,
    estimator: SlotTimeEstimator,
    timescale: TimescaleSink,
    chunk: SubstreamsChunk,
    shard_path: PathBuf,
) {
    let handle = tokio::runtime::Handle::current();
    join_set.spawn_blocking(move || {
        handle.block_on(async move {
            let started_at = StdInstant::now();
            let mut snapshots = HashMap::<Pubkey, ReserveSnapshot>::new();
            let mut progress = SubstreamsProgress::new(estimator.start_block, estimator.stop_block);
            let mut jsonl = open_jsonl_writer(&shard_path)?;
            eprintln!(
                "parallel stream chunk {}/{} starting: blocks {}..{} shard={}",
                chunk.index,
                chunk.total,
                chunk.start_block,
                chunk.stop_block,
                shard_path.display()
            );
            let rows = run_substreams_grpc_chunk(
                &args,
                &filter,
                chunk.start_block,
                chunk.stop_block,
                &target_by_reserve,
                processing,
                estimator,
                &timescale,
                &mut snapshots,
                &mut progress,
                args.substreams_progress_rows,
                args.substreams_insert_batch_size,
                Some(&mut jsonl),
            )
            .await?;
            jsonl
                .flush()
                .with_context(|| format!("flush shard JSONL {}", shard_path.display()))?;
            Ok(SubstreamsParallelChunkResult {
                chunk,
                rows,
                shard_path,
                elapsed: started_at.elapsed(),
            })
        })
    });
}

fn substreams_chunks(estimator: SlotTimeEstimator, chunk_blocks: u64) -> Vec<SubstreamsChunk> {
    let total_blocks = estimator.stop_block - estimator.start_block;
    let total = total_blocks.div_ceil(chunk_blocks);
    let mut chunks = Vec::with_capacity(total as usize);
    let mut start_block = estimator.start_block;
    let mut index = 1_u64;
    while start_block < estimator.stop_block {
        let stop_block = start_block
            .saturating_add(chunk_blocks)
            .min(estimator.stop_block);
        chunks.push(SubstreamsChunk {
            index,
            total,
            start_block,
            stop_block,
        });
        start_block = stop_block;
        index += 1;
    }
    chunks
}

fn shard_jsonl_path(base: &Path, chunk: SubstreamsChunk) -> PathBuf {
    let file_name = base
        .file_name()
        .map(|name| name.to_string_lossy())
        .unwrap_or_else(|| "substreams-backfill.jsonl".into());
    let shard_name = format!(
        "{file_name}.part-{:05}-of-{:05}.jsonl",
        chunk.index, chunk.total
    );
    base.parent()
        .map(|parent| parent.join(&shard_name))
        .unwrap_or_else(|| PathBuf::from(shard_name))
}

fn parallel_eta(elapsed: Duration, completed_chunks: u64, total_chunks: u64) -> Option<Duration> {
    if completed_chunks == 0 || completed_chunks >= total_chunks {
        return None;
    }
    let remaining = total_chunks - completed_chunks;
    let seconds = elapsed.as_secs_f64() * remaining as f64 / completed_chunks as f64;
    seconds
        .is_finite()
        .then(|| Duration::from_secs_f64(seconds))
}

#[allow(clippy::too_many_arguments)]
async fn run_substreams_grpc_chunk(
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
    progress_rows: u64,
    insert_batch_size: usize,
    mut jsonl: Option<&mut BufWriter<File>>,
) -> Result<u64> {
    let params = format!("{}={filter}", args.substreams_module);
    let limit_processed_blocks = (stop_block - start_block).saturating_add(1_000);
    let mut command = if args.substreams_grpc_adapter.is_dir() {
        let adapter_dir = args
            .substreams_grpc_adapter
            .canonicalize()
            .with_context(|| {
                format!(
                    "resolve Substreams gRPC adapter path {}",
                    args.substreams_grpc_adapter.display()
                )
            })?;
        let mut command = Command::new("go");
        command.current_dir(adapter_dir).arg("run").arg(".");
        command
    } else {
        Command::new(&args.substreams_grpc_adapter)
    };
    command
        .env("GOTOOLCHAIN", "auto")
        .arg("--endpoint")
        .arg(&args.substreams_endpoint)
        .arg("--package")
        .arg(&args.substreams_package_url)
        .arg("--module")
        .arg(&args.substreams_module)
        .arg("--params")
        .arg(params)
        .arg("--start-block")
        .arg(start_block.to_string())
        .arg("--stop-block")
        .arg(stop_block.to_string())
        .arg("--limit-processed-blocks")
        .arg(limit_processed_blocks.to_string())
        .arg("--api-key-envvar")
        .arg(&args.substreams_api_key_envvar);
    if args.substreams_production_mode {
        command.arg("--production-mode");
    }
    if let Some(workers) = args.substreams_parallel_workers {
        command.arg("--parallel-workers").arg(workers.to_string());
    }

    eprintln!(
        "grpc adapter starting: endpoint={} package={} module={} blocks={}..{} limit_processed_blocks={}",
        args.substreams_endpoint,
        args.substreams_package_url,
        args.substreams_module,
        start_block,
        stop_block,
        limit_processed_blocks
    );
    let mut child = command
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .spawn()
        .with_context(|| {
            format!(
                "spawn Substreams gRPC adapter {:?}",
                args.substreams_grpc_adapter
            )
        })?;
    let stdout = child.stdout.take().context("capture gRPC adapter stdout")?;
    let reader = BufReader::new(stdout);
    let (line_tx, line_rx) = std_mpsc::channel::<Result<String, String>>();
    let reader_worker = thread::spawn(move || {
        for line in reader.lines() {
            let send_result = match line {
                Ok(line) => line_tx.send(Ok(line)),
                Err(err) => line_tx.send(Err(err.to_string())),
            };
            if send_result.is_err() {
                break;
            }
        }
    });
    let mut rows = 0_u64;
    let mut pending_updates =
        Vec::<OwnedReserveUpdate>::with_capacity(insert_batch_size.min(10_000));
    let mut inserted_rows = 0_usize;
    let mut last_seen_block = start_block;
    let mut last_adapter_line_at = StdInstant::now();
    let mut printed_grpc_progress = false;
    let mut last_grpc_progress_print_at = StdInstant::now();
    let mut last_printed_progress_block = start_block;

    loop {
        let line = match line_rx.recv_timeout(Duration::from_secs(30)) {
            Ok(Ok(line)) => {
                last_adapter_line_at = StdInstant::now();
                line
            }
            Ok(Err(err)) => bail!("read Substreams gRPC adapter line: {err}"),
            Err(RecvTimeoutError::Timeout) => {
                eprintln!(
                    "grpc heartbeat: waiting_on=adapter_or_provider chunk_blocks={}..{} last_block={}/{} chunk_overall={} overall={} rows={} inserted={} pending={} reserves_seen={}/{} elapsed={} quiet_for={} eta={}",
                    start_block,
                    stop_block,
                    last_seen_block,
                    estimator.stop_block,
                    format_percent(percent_between(last_seen_block, start_block, stop_block)),
                    format_percent(progress.percent_at(last_seen_block)),
                    progress.total_rows,
                    inserted_rows,
                    pending_updates.len(),
                    progress.reserve_rows.len(),
                    target_by_reserve.len(),
                    format_duration(progress.started_at.elapsed()),
                    format_duration(last_adapter_line_at.elapsed()),
                    progress
                        .eta_at(last_seen_block)
                        .map(format_duration)
                        .unwrap_or_else(|| "warming_up".to_string())
                );
                continue;
            }
            Err(RecvTimeoutError::Disconnected) => break,
        };
        let trimmed = line.trim_start();
        if !trimmed.starts_with('{') {
            continue;
        }
        let event: SubstreamsGrpcAdapterEvent =
            serde_json::from_str(trimmed).context("parse Substreams gRPC adapter event")?;
        match event {
            SubstreamsGrpcAdapterEvent::Session {
                trace_id,
                resolved_start_block,
                linear_handoff_block,
                max_parallel_workers,
                chain_head,
            } => {
                eprintln!(
                    "grpc session: trace_id={} resolved_start={} linear_handoff={} max_parallel_workers={} chain_head={}",
                    trace_id,
                    resolved_start_block,
                    linear_handoff_block,
                    max_parallel_workers,
                    chain_head
                );
            }
            SubstreamsGrpcAdapterEvent::Progress(update) => {
                let progress_block = update
                    .highest_contiguous_block
                    .unwrap_or_else(|| start_block.saturating_add(update.processed_blocks))
                    .clamp(start_block, stop_block);
                last_seen_block = progress_block;
                let should_print = !printed_grpc_progress
                    || last_grpc_progress_print_at.elapsed() >= Duration::from_secs(5)
                    || progress_block.saturating_sub(last_printed_progress_block) >= 25_000
                    || progress_block >= stop_block;
                if should_print {
                    printed_grpc_progress = true;
                    last_grpc_progress_print_at = StdInstant::now();
                    last_printed_progress_block = progress_block;
                    eprintln!(
                        "grpc progress: chunk_blocks={}..{} block={}/{} overall={} processed_blocks={} running_jobs={} completed_ranges={} bytes_read={} bytes_written={} rows={} inserted={} reserves_seen={}/{} elapsed={} eta={}",
                        start_block,
                        stop_block,
                        progress_block,
                        estimator.stop_block,
                        format_percent(progress.percent_at(progress_block)),
                        update.processed_blocks,
                        update.running_job_count,
                        update.completed_range_count,
                        update.total_bytes_read,
                        update.total_bytes_written,
                        progress.total_rows,
                        inserted_rows,
                        progress.reserve_rows.len(),
                        target_by_reserve.len(),
                        format_duration(progress.started_at.elapsed()),
                        progress
                            .eta_at(progress_block)
                            .map(format_duration)
                            .unwrap_or_else(|| "warming_up".to_string())
                    );
                }
            }
            SubstreamsGrpcAdapterEvent::Undo { last_valid_block } => {
                bail!("Substreams sent undo signal at last_valid_block={last_valid_block}");
            }
            SubstreamsGrpcAdapterEvent::Complete { cursor } => {
                eprintln!("grpc complete: cursor={cursor}");
            }
            SubstreamsGrpcAdapterEvent::Error { message } => {
                bail!("Substreams gRPC adapter error: {message}");
            }
            SubstreamsGrpcAdapterEvent::Block(output) => {
                last_seen_block = output.block.clamp(start_block, stop_block);
                let update = decode_block_output(output, |value| {
                    BASE64_STANDARD
                        .decode(value)
                        .context("decode Substreams gRPC adapter base64 output")
                })?;
                for account in update.accounts {
                    if account.deleted {
                        continue;
                    }
                    let reserve = decode_pubkey_bytes(&account.address, "account address")?;
                    let Some(target) = target_by_reserve.get(&reserve) else {
                        tracing::debug!(%reserve, "dropping Substreams account for unknown reserve");
                        continue;
                    };
                    if has_zero_discriminator(&account.data) {
                        tracing::warn!(
                            %reserve,
                            slot = update.slot,
                            "skipping zero-discriminator historical reserve account update"
                        );
                        eprintln!(
                            "zero account skipped: reserve={} slot={} chunk_blocks={}..{}",
                            reserve, update.slot, start_block, stop_block
                        );
                        continue;
                    }
                    let reserve_count = progress.record_row(reserve);
                    let owner = decode_pubkey_bytes(&account.owner, "account owner")?;
                    ensure_klend_owner(&owner.to_string(), target, "substreams_backfill")?;
                    let observed_at = update
                        .observed_at
                        .map(Ok)
                        .unwrap_or_else(|| estimator.observed_at(update.slot))?;
                    let now = Utc::now();
                    let owned_update = build_owned_update(
                        UpdateSourceMetadata {
                            source: "substreams_backfill",
                            source_commitment: CONFIRMED_COMMITMENT,
                        },
                        target,
                        update.slot,
                        &account.data,
                        Some(observed_at),
                        now,
                        Instant::now(),
                        processing,
                        snapshots,
                    )?;
                    if let Some(writer) = jsonl.as_deref_mut() {
                        let record = owned_update.as_record();
                        serde_json::to_writer(&mut *writer, &record)
                            .context("write Substreams backfill JSONL reserve update")?;
                        writer
                            .write_all(b"\n")
                            .context("write Substreams backfill JSONL newline")?;
                    }
                    snapshots.insert(target.reserve, owned_update.snapshot.clone());
                    pending_updates.push(owned_update);
                    rows += 1;
                    if progress.total_rows == 1 || progress.total_rows % progress_rows == 0 {
                        eprintln!(
                            "reserve row: rows={} chunk_rows={} slot={} overall={} reserves_seen={}/{} current_reserve=\"{}\" reserve_rows={} elapsed={} eta={}",
                            progress.total_rows,
                            rows,
                            update.slot,
                            format_percent(progress.percent_at(update.slot)),
                            progress.reserve_rows.len(),
                            target_by_reserve.len(),
                            reserve_label(target),
                            reserve_count,
                            format_duration(progress.started_at.elapsed()),
                            progress
                                .eta_at(update.slot)
                                .map(format_duration)
                                .unwrap_or_else(|| "warming_up".to_string())
                        );
                    }
                    if pending_updates.len() >= insert_batch_size {
                        inserted_rows += flush_substreams_batch(
                            timescale,
                            &mut pending_updates,
                            args.substreams_skip_db_inserts,
                        )
                        .await?;
                    }
                }
            }
        }
    }

    inserted_rows += flush_substreams_batch(
        timescale,
        &mut pending_updates,
        args.substreams_skip_db_inserts,
    )
    .await?;
    let status = child.wait().context("wait for Substreams gRPC adapter")?;
    reader_worker
        .join()
        .map_err(|_| anyhow::anyhow!("Substreams gRPC adapter reader thread panicked"))?;
    if !status.success() {
        bail!("Substreams gRPC adapter exited with {status}");
    }
    tracing::info!(
        rows,
        inserted_rows,
        start_block,
        stop_block,
        "Substreams gRPC chunk inserted"
    );
    Ok(rows)
}

#[allow(clippy::too_many_arguments)]
async fn run_substreams_cli_chunk(
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
            if has_zero_discriminator(&data) {
                tracing::warn!(
                    %reserve,
                    slot = envelope.block,
                    "skipping zero-discriminator historical reserve account update"
                );
                eprintln!(
                    "zero account skipped: reserve={} slot={} chunk_blocks={}..{}",
                    reserve, envelope.block, start_block, stop_block
                );
                continue;
            }
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
                inserted_rows += flush_substreams_batch(
                    timescale,
                    &mut pending_updates,
                    args.substreams_skip_db_inserts,
                )
                .await?;
            }
        }
    }

    inserted_rows += flush_substreams_batch(
        timescale,
        &mut pending_updates,
        args.substreams_skip_db_inserts,
    )
    .await?;

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
    skip_db_inserts: bool,
) -> Result<usize> {
    if pending_updates.is_empty() {
        return Ok(0);
    }
    let first_slot = pending_updates
        .first()
        .map(|update| update.slot)
        .unwrap_or_default();
    let last_slot = pending_updates
        .last()
        .map(|update| update.slot)
        .unwrap_or_default();
    eprintln!(
        "batch flush starting: pending={} slots={}..{}",
        pending_updates.len(),
        first_slot,
        last_slot
    );
    if skip_db_inserts {
        eprintln!(
            "batch flush skipped db insert: local_only=true attempted={} slots={}..{}",
            pending_updates.len(),
            first_slot,
            last_slot
        );
        pending_updates.clear();
        return Ok(0);
    }
    let records = pending_updates
        .iter()
        .map(OwnedReserveUpdate::as_record)
        .collect::<Vec<_>>();
    eprintln!(
        "batch flush inserting: records={} slots={}..{}",
        records.len(),
        first_slot,
        last_slot
    );
    let inserted = timescale.insert_batch_skip_duplicates(&records).await?;
    eprintln!(
        "batch flush complete: inserted={} attempted={} slots={}..{}",
        inserted,
        records.len(),
        first_slot,
        last_slot
    );
    pending_updates.clear();
    Ok(inserted)
}

async fn import_jsonl_shards(args: &Args, timescale: &TimescaleSink) -> Result<()> {
    let started_at = StdInstant::now();
    let mut total_rows = 0_usize;
    let mut total_inserted = 0_usize;

    eprintln!(
        "JSONL import starting: shards={} batch_size={} concurrency={} elapsed=0s",
        args.import_jsonl.len(),
        args.substreams_insert_batch_size,
        args.substreams_insert_concurrency
    );

    for (index, path) in args.import_jsonl.iter().enumerate() {
        let shard_started_at = StdInstant::now();
        let file = File::open(path)
            .with_context(|| format!("open JSONL import shard {}", path.display()))?;
        let reader = BufReader::new(file);
        let mut pending = Vec::<Value>::with_capacity(args.substreams_insert_batch_size);
        let mut shard_rows = 0_usize;
        let mut shard_inserted = 0_usize;

        eprintln!(
            "JSONL shard {}/{} starting: path={}",
            index + 1,
            args.import_jsonl.len(),
            path.display()
        );

        for (line_index, line) in reader.lines().enumerate() {
            let line_number = line_index + 1;
            let line =
                line.with_context(|| format!("read {} line {line_number}", path.display()))?;
            if line.trim().is_empty() {
                continue;
            }
            let record: Value = serde_json::from_str(&line)
                .with_context(|| format!("parse {} line {line_number}", path.display()))?;
            pending.push(
                prepare_jsonl_record_for_timescale(&record).with_context(|| {
                    format!(
                        "prepare {} line {line_number} for Timescale import",
                        path.display()
                    )
                })?,
            );
            shard_rows += 1;
            total_rows += 1;
            if shard_rows == 1 || shard_rows % 10_000 == 0 {
                eprintln!(
                    "JSONL shard {}/{} prepared: shard_rows={} total_rows={} pending={} elapsed={} path={}",
                    index + 1,
                    args.import_jsonl.len(),
                    shard_rows,
                    total_rows,
                    pending.len(),
                    format_duration(started_at.elapsed()),
                    path.display()
                );
            }

            if pending.len() >= args.substreams_insert_batch_size {
                let attempted = pending.len();
                eprintln!(
                    "JSONL shard {}/{} inserting batch: attempted={} shard_rows={} total_rows={} elapsed={} path={}",
                    index + 1,
                    args.import_jsonl.len(),
                    attempted,
                    shard_rows,
                    total_rows,
                    format_duration(started_at.elapsed()),
                    path.display()
                );
                let inserted = timescale
                    .insert_prepared_batch_skip_duplicates(std::mem::take(&mut pending))
                    .await?;
                shard_inserted += inserted;
                total_inserted += inserted;
                eprintln!(
                    "JSONL shard {}/{} flush: attempted={} inserted={} shard_rows={} shard_inserted={} total_rows={} total_inserted={} elapsed={} path={}",
                    index + 1,
                    args.import_jsonl.len(),
                    attempted,
                    inserted,
                    shard_rows,
                    shard_inserted,
                    total_rows,
                    total_inserted,
                    format_duration(started_at.elapsed()),
                    path.display()
                );
            }
        }

        if !pending.is_empty() {
            let attempted = pending.len();
            eprintln!(
                "JSONL shard {}/{} inserting final batch: attempted={} shard_rows={} total_rows={} elapsed={} path={}",
                index + 1,
                args.import_jsonl.len(),
                attempted,
                shard_rows,
                total_rows,
                format_duration(started_at.elapsed()),
                path.display()
            );
            let inserted = timescale
                .insert_prepared_batch_skip_duplicates(std::mem::take(&mut pending))
                .await?;
            shard_inserted += inserted;
            total_inserted += inserted;
            eprintln!(
                "JSONL shard {}/{} final flush: attempted={} inserted={} shard_rows={} shard_inserted={} total_rows={} total_inserted={} elapsed={} path={}",
                index + 1,
                args.import_jsonl.len(),
                attempted,
                inserted,
                shard_rows,
                shard_inserted,
                total_rows,
                total_inserted,
                format_duration(started_at.elapsed()),
                path.display()
            );
        }

        eprintln!(
            "JSONL shard {}/{} complete: rows={} inserted={} duplicates={} shard_elapsed={} total_elapsed={} path={}",
            index + 1,
            args.import_jsonl.len(),
            shard_rows,
            shard_inserted,
            shard_rows.saturating_sub(shard_inserted),
            format_duration(shard_started_at.elapsed()),
            format_duration(started_at.elapsed()),
            path.display()
        );
    }

    eprintln!(
        "JSONL import complete: shards={} rows={} inserted={} duplicates={} elapsed={}",
        args.import_jsonl.len(),
        total_rows,
        total_inserted,
        total_rows.saturating_sub(total_inserted),
        format_duration(started_at.elapsed())
    );
    Ok(())
}

fn prepare_jsonl_record_for_timescale(record: &Value) -> Result<Value> {
    let snapshot = required_object(record, "snapshot")?;
    let target = required_object(record, "target")?;
    let diff = record.get("diff").cloned().unwrap_or(Value::Null);
    let source_commitment = required_str(record, "source_commitment")?;
    let source = required_str(record, "source")?;
    let slot = required_u64(record, "slot")?;
    let account_data_hash = required_str(record, "account_data_hash")?;
    let reserve = pubkey_json_to_string(
        required_value_from_map(snapshot, "reserve")?,
        "snapshot.reserve",
    )?;
    let market = optional_pubkey_json_to_string(snapshot.get("market"), "snapshot.market")?;
    let liquidity_mint = pubkey_json_to_string(
        required_value_from_map(snapshot, "liquidity_mint")?,
        "snapshot.liquidity_mint",
    )?;
    let cumulative_borrow_rate_bsf = required_array(snapshot, "cumulative_borrow_rate_bsf")?
        .iter()
        .map(|value| {
            value
                .as_u64()
                .map(|number| number.to_string())
                .with_context(|| "snapshot.cumulative_borrow_rate_bsf must contain u64 values")
        })
        .collect::<Result<Vec<_>>>()?
        .join(":");
    let changed_fields = diff
        .get("changed_fields")
        .and_then(Value::as_array)
        .map(|fields| {
            fields
                .iter()
                .map(|field| {
                    field
                        .as_str()
                        .map(ToString::to_string)
                        .with_context(|| "diff.changed_fields must contain strings")
                })
                .collect::<Result<Vec<_>>>()
        })
        .transpose()?
        .unwrap_or_default();
    let diff_changed = diff
        .get("changed")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    let prepared_diff = if diff.is_null() { json!({}) } else { diff };

    let mut row = Map::new();
    row.insert(
        "dedupe_key".to_string(),
        json!(TimescaleSink::dedupe_key_parts(
            source_commitment,
            source,
            &reserve,
            slot,
            account_data_hash,
        )),
    );
    row.insert("reserve".to_string(), json!(reserve));
    row.insert("slot".to_string(), json!(required_i64(record, "slot")?));
    row.insert("account_data_hash".to_string(), json!(account_data_hash));
    row.insert(
        "observed_at".to_string(),
        required_value(record, "observed_at")?.clone(),
    );
    row.insert("kind".to_string(), required_value(record, "kind")?.clone());
    row.insert(
        "source".to_string(),
        required_value(record, "source")?.clone(),
    );
    row.insert("market".to_string(), json!(market));
    row.insert(
        "market_name".to_string(),
        target.get("market_name").cloned().unwrap_or(Value::Null),
    );
    row.insert(
        "symbol".to_string(),
        snapshot
            .get("symbol")
            .filter(|value| !value.is_null())
            .cloned()
            .or_else(|| target.get("symbol").cloned())
            .unwrap_or(Value::Null),
    );
    row.insert("liquidity_mint".to_string(), json!(liquidity_mint));
    row.insert(
        "mint_decimals".to_string(),
        json!(required_i64_from_map(snapshot, "mint_decimals")?),
    );
    row.insert(
        "reserve_last_update_slot".to_string(),
        json!(required_i64_from_map(snapshot, "reserve_last_update_slot")?),
    );
    row.insert(
        "reserve_last_update_stale".to_string(),
        required_value_from_map(snapshot, "reserve_last_update_stale")?.clone(),
    );
    row.insert(
        "reserve_price_status".to_string(),
        json!(required_i64_from_map(snapshot, "reserve_price_status")?),
    );
    row.insert(
        "available_amount".to_string(),
        required_value_from_map(snapshot, "available_amount")?.clone(),
    );
    row.insert(
        "borrowed_amount".to_string(),
        required_value_from_map(snapshot, "borrowed_amount")?.clone(),
    );
    row.insert(
        "borrowed_amount_sf".to_string(),
        required_value_from_map(snapshot, "borrowed_amount_sf")?.clone(),
    );
    row.insert(
        "total_supply_amount".to_string(),
        required_value_from_map(snapshot, "total_supply_amount")?.clone(),
    );
    row.insert(
        "market_price_usd".to_string(),
        required_value_from_map(snapshot, "market_price_usd")?.clone(),
    );
    row.insert(
        "market_price_last_updated_ts".to_string(),
        json!(required_i64_from_map(
            snapshot,
            "market_price_last_updated_ts"
        )?),
    );
    row.insert(
        "cumulative_borrow_rate_bsf".to_string(),
        json!(cumulative_borrow_rate_bsf),
    );
    row.insert(
        "total_supply_usd_estimate".to_string(),
        required_value_from_map(snapshot, "total_supply_usd_estimate")?.clone(),
    );
    row.insert(
        "total_borrow_usd_estimate".to_string(),
        required_value_from_map(snapshot, "total_borrow_usd_estimate")?.clone(),
    );
    row.insert(
        "utilization".to_string(),
        required_value_from_map(snapshot, "utilization")?.clone(),
    );
    row.insert(
        "borrow_apr".to_string(),
        required_value_from_map(snapshot, "borrow_apr")?.clone(),
    );
    row.insert(
        "supply_apr".to_string(),
        required_value_from_map(snapshot, "supply_apr")?.clone(),
    );
    row.insert(
        "borrow_apy".to_string(),
        required_value_from_map(snapshot, "borrow_apy")?.clone(),
    );
    row.insert(
        "supply_apy".to_string(),
        required_value_from_map(snapshot, "supply_apy")?.clone(),
    );
    row.insert(
        "protocol_take_rate_pct".to_string(),
        json!(required_i64_from_map(snapshot, "protocol_take_rate_pct")?),
    );
    row.insert(
        "host_fixed_interest_rate_bps".to_string(),
        json!(required_i64_from_map(
            snapshot,
            "host_fixed_interest_rate_bps"
        )?),
    );
    row.insert("diff_changed".to_string(), json!(diff_changed));
    row.insert("changed_fields".to_string(), json!(changed_fields));
    row.insert(
        "diff_summary".to_string(),
        required_value(record, "diff_summary")?.clone(),
    );
    row.insert("diff".to_string(), prepared_diff);
    row.insert("target".to_string(), Value::Object(target.clone()));
    row.insert("snapshot".to_string(), Value::Object(snapshot.clone()));
    row.insert("record".to_string(), record.clone());
    row.insert(
        "raw_account_data_base64".to_string(),
        record
            .get("raw_account_data_base64")
            .cloned()
            .unwrap_or(Value::Null),
    );
    row.insert(
        "api_supply_apy".to_string(),
        target.get("api_supply_apy").cloned().unwrap_or(Value::Null),
    );
    row.insert(
        "api_borrow_apy".to_string(),
        target.get("api_borrow_apy").cloned().unwrap_or(Value::Null),
    );
    row.insert(
        "api_total_supply_usd".to_string(),
        target
            .get("api_total_supply_usd")
            .cloned()
            .unwrap_or(Value::Null),
    );
    row.insert(
        "api_total_borrow_usd".to_string(),
        target
            .get("api_total_borrow_usd")
            .cloned()
            .unwrap_or(Value::Null),
    );
    row.insert("source_commitment".to_string(), json!(source_commitment));
    row.insert(
        "received_at".to_string(),
        required_value(record, "received_at")?.clone(),
    );
    row.insert(
        "decoded_at".to_string(),
        required_value(record, "decoded_at")?.clone(),
    );
    row.insert(
        "receive_to_decode_ms".to_string(),
        json!(required_i64(record, "receive_to_decode_ms")?),
    );
    row.insert("decode_to_insert_ms".to_string(), json!(0));

    Ok(Value::Object(row))
}

fn required_object<'a>(
    record: &'a Value,
    field: &str,
) -> Result<&'a serde_json::Map<String, Value>> {
    required_value(record, field)?
        .as_object()
        .with_context(|| format!("{field} must be an object"))
}

fn required_array<'a>(
    record: &'a serde_json::Map<String, Value>,
    field: &str,
) -> Result<&'a [Value]> {
    required_value_from_map(record, field)?
        .as_array()
        .map(Vec::as_slice)
        .with_context(|| format!("{field} must be an array"))
}

fn required_value<'a>(record: &'a Value, field: &str) -> Result<&'a Value> {
    record
        .get(field)
        .with_context(|| format!("record missing {field}"))
}

fn required_value_from_map<'a>(
    record: &'a serde_json::Map<String, Value>,
    field: &str,
) -> Result<&'a Value> {
    record
        .get(field)
        .with_context(|| format!("record missing {field}"))
}

fn required_str<'a>(record: &'a Value, field: &str) -> Result<&'a str> {
    required_value(record, field)?
        .as_str()
        .with_context(|| format!("{field} must be a string"))
}

fn required_u64(record: &Value, field: &str) -> Result<u64> {
    required_value(record, field)?
        .as_u64()
        .with_context(|| format!("{field} must be a u64"))
}

fn required_i64(record: &Value, field: &str) -> Result<i64> {
    let value = required_value(record, field)?;
    json_value_to_i64(value, field)
}

fn required_i64_from_map(record: &serde_json::Map<String, Value>, field: &str) -> Result<i64> {
    let value = required_value_from_map(record, field)?;
    json_value_to_i64(value, field)
}

fn json_value_to_i64(value: &Value, field: &str) -> Result<i64> {
    if let Some(number) = value.as_i64() {
        return Ok(number);
    }
    let number = value
        .as_u64()
        .with_context(|| format!("{field} must be an integer"))?;
    i64::try_from(number).with_context(|| format!("{field} exceeds i64 range"))
}

fn pubkey_json_to_string(value: &Value, label: &str) -> Result<String> {
    match value {
        Value::String(value) => {
            let pubkey = value
                .parse::<Pubkey>()
                .with_context(|| format!("{label} must be a pubkey"))?;
            Ok(pubkey.to_string())
        }
        Value::Array(values) => {
            if values.len() != 32 {
                bail!("{label} must contain 32 bytes");
            }
            let mut bytes = [0_u8; 32];
            for (index, value) in values.iter().enumerate() {
                let byte = value
                    .as_u64()
                    .with_context(|| format!("{label}[{index}] must be a byte"))?;
                bytes[index] = u8::try_from(byte)
                    .with_context(|| format!("{label}[{index}] exceeds u8 range"))?;
            }
            Ok(Pubkey::new_from_array(bytes).to_string())
        }
        _ => bail!("{label} must be a pubkey string or byte array"),
    }
}

fn optional_pubkey_json_to_string(value: Option<&Value>, label: &str) -> Result<Option<String>> {
    value
        .filter(|value| !value.is_null())
        .map(|value| pubkey_json_to_string(value, label))
        .transpose()
}

fn decode_pubkey_base64(encoded: &str, label: &str) -> Result<Pubkey> {
    let bytes = BASE64_STANDARD
        .decode(encoded)
        .with_context(|| format!("decode Substreams {label}"))?;
    decode_pubkey_bytes(&bytes, label)
}

fn decode_pubkey_bytes(bytes: &[u8], label: &str) -> Result<Pubkey> {
    if bytes.len() != 32 {
        bail!(
            "Substreams {label} decoded to {} bytes, expected 32",
            bytes.len()
        );
    }
    let mut array = [0_u8; 32];
    array.copy_from_slice(bytes);
    Ok(Pubkey::new_from_array(array))
}

fn has_zero_discriminator(data: &[u8]) -> bool {
    data.len() >= 8 && data[..8].iter().all(|byte| *byte == 0)
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

#[allow(clippy::too_many_arguments)]
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
