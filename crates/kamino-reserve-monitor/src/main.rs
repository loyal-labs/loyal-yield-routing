use std::{
    collections::{BTreeSet, HashMap},
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
    cli::{validate_args, Args, UpdateSourceKind},
    diff_snapshot, snapshot_from_account, snapshot_from_account_at,
    source::{
        AccountUpdateEvent, AccountUpdateSource, LaserstreamAccountUpdateSource,
        RpcWebsocketAccountUpdateSource, SubscriptionConfig, UpdateSourceMetadata,
        CONFIRMED_COMMITMENT,
    },
    targets::{KaminoApi, ReserveTarget},
    timescale::{
        ConfirmedStateVerification, ReserveUpdateRecord, TimescaleSink, TimescaleSinkConfig,
    },
    verification::{ConfirmedRefreshEvent, ConfirmedReserveState, ConfirmedReserveVerifier},
    ReserveDiff, ReserveSnapshot,
};
use klend_interface::KLEND_PROGRAM_ID;
use solana_sdk::pubkey::Pubkey;
use tokio::{
    sync::mpsc,
    task::JoinHandle,
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

#[derive(Debug)]
enum SupportedReserveCatalogRefreshEvent {
    Synced {
        catalog_count: usize,
        targets: Vec<ReserveTarget>,
    },
    Failed(String),
}

impl SubscriptionRuntimeState {
    fn is_terminal(self) -> bool {
        matches!(self, Self::Failed | Self::Stopped)
    }
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

    let timescale_config = TimescaleSinkConfig {
        url: args.timescaledb_url.clone(),
        schema: args.timescaledb_schema.clone(),
        ..TimescaleSinkConfig::new(args.timescaledb_url.clone())
    };
    let timescale = TimescaleSink::connect(timescale_config).await?;
    let confirmed_verifier = ConfirmedReserveVerifier::new(
        args.rpc_url.clone(),
        Duration::from_secs(args.confirmed_refresh_timeout_secs),
    );

    if args.sync_supported_reserves {
        let supported_reserves = fetch_supported_reserves_blocking(
            args.kamino_api_base.clone(),
            args.kamino_api_timeout_secs,
        )
        .await?;
        let count = timescale
            .sync_supported_reserves(&supported_reserves, args.allow_supported_reserve_removals)
            .await?;
        tracing::info!(count, "supported Kamino reserves synced");
        return Ok(());
    }

    // Normal and one-shot monitors must not begin from an arbitrarily old
    // database catalog. A complete API response is committed atomically
    // before decoding targets are selected; any failure aborts startup.
    let supported_reserves = fetch_supported_reserves_blocking(
        args.kamino_api_base.clone(),
        args.kamino_api_timeout_secs,
    )
    .await
    .context("fetch supported Kamino reserves before monitor startup")?;
    let targets = timescale
        .refresh_supported_reserves(&supported_reserves, &args.reserve)
        .await
        .context("publish supported Kamino reserves before monitor startup")?;
    let supported_reserve_count = supported_reserves.len();
    tracing::info!(
        supported_reserve_count,
        "supported Kamino reserve catalog refreshed before target selection"
    );

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

    if targets.is_empty() {
        bail!("no active supported Kamino reserve targets selected; run --sync-supported-reserves first");
    }

    let running = Arc::new(AtomicBool::new(true));
    install_ctrlc_handler(running.clone())?;
    let progress_timeout = Duration::from_secs(args.progress_timeout_secs);
    let status_log_interval = Duration::from_secs(args.status_log_interval_secs);

    let processing = ProcessingConfig {
        slot_duration_ms,
        store_raw: args.store_raw,
    };
    let mut jsonl = args.jsonl.as_deref().map(open_jsonl_writer).transpose()?;
    let mut snapshots = HashMap::<Pubkey, ReserveSnapshot>::new();

    let seed_slot = seed_http_snapshots(
        &confirmed_verifier,
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
    let (confirmed_tx, confirmed_rx) = mpsc::unbounded_channel();
    let (catalog_tx, catalog_rx) = mpsc::unbounded_channel();
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
    let confirmed_refresh_worker = confirmed_verifier.spawn_periodic(
        targets.iter().map(|target| target.reserve).collect(),
        Duration::from_secs(args.confirmed_refresh_interval_secs),
        confirmed_tx,
        running.clone(),
    );
    let supported_reserve_catalog_worker = spawn_supported_reserve_catalog_refresh(
        timescale.clone(),
        args.kamino_api_base.clone(),
        args.kamino_api_timeout_secs,
        args.reserve.clone(),
        Duration::from_secs(args.supported_reserve_refresh_interval_secs),
        catalog_tx,
        running.clone(),
    );

    let result = run_event_loop(
        rx,
        confirmed_rx,
        catalog_rx,
        target_by_reserve,
        processing,
        &timescale,
        &mut snapshots,
        jsonl.as_mut(),
        running,
        progress_timeout,
        status_log_interval,
    )
    .await;

    if result.is_ok() {
        if let Err(err) = tokio::time::timeout(Duration::from_secs(10), subscription_worker).await {
            tracing::warn!(error = %err, "timed out waiting for subscription worker shutdown");
        }
        if let Err(err) =
            tokio::time::timeout(Duration::from_secs(10), confirmed_refresh_worker).await
        {
            tracing::warn!(error = %err, "timed out waiting for confirmed refresh worker shutdown");
        }
        if let Err(err) =
            tokio::time::timeout(Duration::from_secs(10), supported_reserve_catalog_worker).await
        {
            tracing::warn!(error = %err, "timed out waiting for supported reserve catalog worker shutdown");
        }
    } else {
        subscription_worker.abort();
        confirmed_refresh_worker.abort();
        supported_reserve_catalog_worker.abort();
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

fn spawn_supported_reserve_catalog_refresh(
    timescale: TimescaleSink,
    kamino_api_base: String,
    kamino_api_timeout_secs: u64,
    requested_reserves: Vec<Pubkey>,
    interval: Duration,
    tx: mpsc::UnboundedSender<SupportedReserveCatalogRefreshEvent>,
    running: Arc<AtomicBool>,
) -> JoinHandle<()> {
    tokio::spawn(async move {
        let mut tick = tokio::time::interval_at(Instant::now() + interval, interval);
        tick.set_missed_tick_behavior(MissedTickBehavior::Skip);
        while running.load(Ordering::Relaxed) {
            tick.tick().await;
            if !running.load(Ordering::Relaxed) {
                break;
            }

            let event = match fetch_supported_reserves_blocking(
                kamino_api_base.clone(),
                kamino_api_timeout_secs,
            )
            .await
            {
                Ok(records) => {
                    let catalog_count = records.len();
                    let refreshed = timescale
                        .refresh_supported_reserves(&records, &requested_reserves)
                        .await;
                    match refreshed {
                        Ok(targets) => SupportedReserveCatalogRefreshEvent::Synced {
                            catalog_count,
                            targets,
                        },
                        Err(error) => {
                            SupportedReserveCatalogRefreshEvent::Failed(format!("{error:#}"))
                        }
                    }
                }
                Err(error) => SupportedReserveCatalogRefreshEvent::Failed(format!("{error:#}")),
            };
            if tx.send(event).is_err() {
                break;
            }
        }
    })
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
    confirmed_verifier: &ConfirmedReserveVerifier,
    targets: &[ReserveTarget],
    processing: ProcessingConfig,
    timescale: &TimescaleSink,
    snapshots: &mut HashMap<Pubkey, ReserveSnapshot>,
    jsonl: Option<&mut BufWriter<File>>,
) -> Result<u64> {
    let keys = targets
        .iter()
        .map(|target| target.reserve)
        .collect::<Vec<_>>();
    let states = confirmed_verifier
        .fetch(&keys)
        .await
        .context("fetch initial confirmed reserve accounts")?;
    let seed_slot = states
        .iter()
        .map(|state| state.verified_slot)
        .min()
        .context("confirmed reserve seed returned no states")?;
    let mut jsonl = jsonl;
    let target_by_reserve = targets
        .iter()
        .cloned()
        .map(|target| (target.reserve, target))
        .collect::<HashMap<_, _>>();
    let outcome = refresh_confirmed_snapshots(
        states,
        "http_snapshot",
        &target_by_reserve,
        processing,
        timescale,
        snapshots,
        jsonl.as_deref_mut(),
    )
    .await?;
    tracing::info!(
        verified = outcome.verified,
        deferred_states = outcome.deferred_states,
        invalid_states = outcome.invalid_states,
        reused_events = outcome.existing_events,
        inserted_events = outcome.inserted_events,
        "initial confirmed reserve snapshot completed"
    );

    if let Some(writer) = jsonl.as_deref_mut() {
        writer.flush().context("flush seed JSONL")?;
    }
    Ok(seed_slot)
}

async fn run_event_loop(
    mut rx: mpsc::UnboundedReceiver<AccountUpdateEvent>,
    mut confirmed_rx: mpsc::UnboundedReceiver<ConfirmedRefreshEvent>,
    mut catalog_rx: mpsc::UnboundedReceiver<SupportedReserveCatalogRefreshEvent>,
    mut target_by_reserve: HashMap<Pubkey, ReserveTarget>,
    processing: ProcessingConfig,
    timescale: &TimescaleSink,
    snapshots: &mut HashMap<Pubkey, ReserveSnapshot>,
    mut jsonl: Option<&mut BufWriter<File>>,
    running: Arc<AtomicBool>,
    progress_timeout: Duration,
    status_log_interval: Duration,
) -> Result<()> {
    let mut subscription_states = target_by_reserve
        .keys()
        .copied()
        .map(|reserve| (reserve, SubscriptionRuntimeState::Connecting))
        .collect::<HashMap<_, _>>();
    let mut last_progress_at = Instant::now();
    let mut timeout_tick = tokio::time::interval(Duration::from_millis(500));
    timeout_tick.set_missed_tick_behavior(MissedTickBehavior::Delay);
    let mut status_tick = tokio::time::interval(status_log_interval);
    status_tick.set_missed_tick_behavior(MissedTickBehavior::Delay);
    let mut updates_processed = 0_u64;
    let mut last_status_updates_processed = 0_u64;
    let mut confirmed_refreshes_completed = 0_u64;
    let mut confirmed_states_verified = 0_u64;
    let mut catalog_refreshes_completed = 0_u64;
    let mut catalog_refreshes_failed = 0_u64;

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
                        if let Err(error) = ensure_klend_owner(&owner, target, metadata.source) {
                            timescale
                                .record_malformed_confirmed_stream_state(
                                    &reserve,
                                    slot,
                                    metadata.source,
                                    received_at,
                                )
                                .await?;
                            tracing::warn!(
                                %reserve,
                                slot,
                                error = %format!("{error:#}"),
                                "invalid subscription owner; strict-newer observation fenced routability"
                            );
                            continue;
                        }
                        let update = match build_owned_update(
                            metadata,
                            target,
                            slot,
                            &data,
                            None,
                            received_at,
                            received_instant,
                            processing,
                            snapshots,
                        ) {
                            Ok(update) => update,
                            Err(error) => {
                                timescale
                                    .record_malformed_confirmed_stream_state(
                                        &reserve,
                                        slot,
                                        metadata.source,
                                        received_at,
                                    )
                                    .await?;
                                tracing::warn!(
                                    %reserve,
                                    slot,
                                    error = %format!("{error:#}"),
                                    "invalid subscription account data; strict-newer observation fenced routability"
                                );
                                continue;
                            }
                        };
                        persist_owned_update(
                            timescale,
                            update,
                            snapshots,
                            jsonl.as_deref_mut(),
                        ).await?;
                        updates_processed = updates_processed.saturating_add(1);
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
            refresh = confirmed_rx.recv() => {
                let Some(refresh) = refresh else {
                    bail!("confirmed refresh channel disconnected unexpectedly");
                };
                match refresh {
                    ConfirmedRefreshEvent::Refreshed(states) => {
                        let outcome = refresh_confirmed_snapshots(
                            states,
                            "http_confirmed_refresh",
                            &target_by_reserve,
                            processing,
                            timescale,
                            snapshots,
                            jsonl.as_deref_mut(),
                        ).await?;
                        confirmed_refreshes_completed = confirmed_refreshes_completed.saturating_add(1);
                        confirmed_states_verified = confirmed_states_verified
                            .saturating_add(outcome.verified as u64);
                        tracing::info!(
                            verified = outcome.verified,
                            deferred_states = outcome.deferred_states,
                            invalid_states = outcome.invalid_states,
                            existing_events = outcome.existing_events,
                            inserted_events = outcome.inserted_events,
                            "confirmed reserve refresh completed"
                        );
                    }
                    ConfirmedRefreshEvent::Failed(error) => {
                        tracing::warn!(%error, "confirmed reserve refresh failed; existing watermarks were not advanced");
                    }
                }
            }
            refresh = catalog_rx.recv() => {
                let Some(refresh) = refresh else {
                    bail!("supported reserve catalog refresh worker disconnected unexpectedly");
                };
                match refresh {
                    SupportedReserveCatalogRefreshEvent::Synced { catalog_count, targets } => {
                        let subscribed_identity = target_decoding_identities(target_by_reserve.values());
                        let refreshed_identity = target_decoding_identities(targets.iter());
                        if subscribed_identity != refreshed_identity {
                            let removed_identity_count = subscribed_identity
                                .difference(&refreshed_identity)
                                .count();
                            let added_identity_count = refreshed_identity
                                .difference(&subscribed_identity)
                                .count();
                            bail!(
                                "supported reserve decoding topology changed after catalog sync; catalog_count={catalog_count} subscribed_target_count={} refreshed_target_count={} removed_identity_count={removed_identity_count} added_identity_count={added_identity_count}; restart required",
                                subscribed_identity.len(),
                                refreshed_identity.len(),
                            );
                        }
                        target_by_reserve = targets
                            .into_iter()
                            .map(|target| (target.reserve, target))
                            .collect();
                        catalog_refreshes_completed = catalog_refreshes_completed.saturating_add(1);
                        tracing::info!(
                            catalog_count,
                            selected_target_count = target_by_reserve.len(),
                            catalog_refreshes_completed,
                            "supported Kamino reserve catalog refreshed without decoding topology change"
                        );
                    }
                    SupportedReserveCatalogRefreshEvent::Failed(error) => {
                        catalog_refreshes_failed = catalog_refreshes_failed.saturating_add(1);
                        tracing::warn!(
                            %error,
                            catalog_refreshes_failed,
                            "supported Kamino reserve catalog refresh failed; subscriptions remain on the last committed topology"
                        );
                    }
                }
            }
            _ = status_tick.tick() => {
                let states = count_subscription_states(&subscription_states);
                let interval_updates = updates_processed.saturating_sub(last_status_updates_processed);
                last_status_updates_processed = updates_processed;
                tracing::info!(
                    updates_processed,
                    interval_updates,
                    active = states.active,
                    connecting = states.connecting,
                    reconnecting = states.reconnecting,
                    failed = states.failed,
                    stopped = states.stopped,
                    total_reserves = subscription_states.len(),
                    confirmed_refreshes_completed,
                    confirmed_states_verified,
                    catalog_refreshes_completed,
                    catalog_refreshes_failed,
                    "reserve monitor status"
                );
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

fn target_decoding_identities<'a>(
    targets: impl IntoIterator<Item = &'a ReserveTarget>,
) -> BTreeSet<(String, Option<String>, Option<String>)> {
    targets
        .into_iter()
        .map(|target| {
            (
                target.reserve.to_string(),
                target.market.map(|market| market.to_string()),
                target
                    .liquidity_mint
                    .map(|liquidity_mint| liquidity_mint.to_string()),
            )
        })
        .collect()
}

#[derive(Clone, Copy, Debug, Default)]
struct SubscriptionStateCounts {
    connecting: usize,
    active: usize,
    reconnecting: usize,
    failed: usize,
    stopped: usize,
}

#[derive(Clone, Copy, Debug, Default)]
struct ConfirmedRefreshOutcome {
    verified: usize,
    deferred_states: usize,
    invalid_states: usize,
    existing_events: usize,
    inserted_events: usize,
}

async fn refresh_confirmed_snapshots(
    states: Vec<ConfirmedReserveState>,
    source: &'static str,
    target_by_reserve: &HashMap<Pubkey, ReserveTarget>,
    processing: ProcessingConfig,
    timescale: &TimescaleSink,
    snapshots: &mut HashMap<Pubkey, ReserveSnapshot>,
    mut jsonl: Option<&mut BufWriter<File>>,
) -> Result<ConfirmedRefreshOutcome> {
    let mut verifications = Vec::with_capacity(states.len());
    let mut decoded_snapshots = HashMap::with_capacity(states.len());
    let mut validation_errors = Vec::new();
    for state in &states {
        let decoded = (|| -> Result<ReserveSnapshot> {
            let target = target_by_reserve.get(&state.reserve).with_context(|| {
                format!(
                    "confirmed refresh returned unknown reserve {}",
                    state.reserve
                )
            })?;
            let account = state.account.as_ref().with_context(|| {
                format!("confirmed reserve account {} was missing", state.reserve)
            })?;
            ensure_klend_owner(&account.owner.to_string(), target, source)?;
            let snapshot = snapshot_from_account_at(
                target,
                state.verified_slot,
                &account.data,
                processing.slot_duration_ms,
                state.verified_at,
            )
            .with_context(|| format!("parse reserve account {}", target.reserve))?;
            validate_snapshot_target(target, &snapshot, source)?;
            Ok(snapshot)
        })();
        let state_valid = match decoded {
            Ok(snapshot) => {
                decoded_snapshots.insert(state.reserve, snapshot);
                true
            }
            Err(error) => {
                validation_errors.push((state.reserve, format!("{error:#}")));
                false
            }
        };
        verifications.push(ConfirmedStateVerification {
            reserve: state.reserve.to_string(),
            account_data_hash: state
                .account
                .as_ref()
                .map(|account| TimescaleSink::account_data_hash(&account.data))
                .unwrap_or_default(),
            verified_slot: i64::try_from(state.verified_slot)
                .context("confirmed refresh slot does not fit PostgreSQL BIGINT")?,
            verified_at: state.verified_at,
            commitment: CONFIRMED_COMMITMENT,
            verification_source: source,
            state_valid,
        });
    }

    let verification_outcome = timescale.verify_confirmed_states(&verifications).await?;
    for (reserve, error) in &validation_errors {
        tracing::warn!(
            %reserve,
            error,
            "confirmed reserve validation failed; reserve watermark remains absent"
        );
    }
    let mut outcome = ConfirmedRefreshOutcome {
        // `matched` contains valid states already bound to the compact pointer
        // and watermark. Unmatched decoded rows count only after insertion
        // reports that both durable records were actually admitted.
        verified: verification_outcome.matched.len(),
        deferred_states: verification_outcome.deferred.len(),
        invalid_states: validation_errors.len(),
        existing_events: verification_outcome.matched.len(),
        inserted_events: 0,
    };
    for state in states {
        if !decoded_snapshots.contains_key(&state.reserve) {
            continue;
        }
        if verification_outcome
            .matched
            .contains(&state.reserve.to_string())
        {
            let snapshot = decoded_snapshots
                .remove(&state.reserve)
                .expect("confirmed refresh snapshots were decoded before persistence");
            snapshots.insert(state.reserve, snapshot);
            continue;
        }
        if verification_outcome
            .deferred
            .contains(&state.reserve.to_string())
        {
            tracing::warn!(
                reserve = %state.reserve,
                verified_slot = state.verified_slot,
                "confirmed reserve state conflicted with its pre-update floor; deferred until an independent exact read"
            );
            continue;
        }
        let target = target_by_reserve
            .get(&state.reserve)
            .expect("confirmed refresh targets were validated before persistence");
        let insert = handle_account_data(
            UpdateSourceMetadata {
                source,
                source_commitment: CONFIRMED_COMMITMENT,
            },
            target,
            state.verified_slot,
            &state
                .account
                .as_ref()
                .expect("valid confirmed state must contain account data")
                .data,
            Some(state.verified_at),
            state.verified_at,
            state.received_instant,
            processing,
            timescale,
            snapshots,
            jsonl.as_deref_mut(),
        )
        .await?;
        outcome.inserted_events += usize::from(insert.inserted);
        outcome.verified +=
            usize::from(insert.current_state_admitted && insert.confirmed_verification_admitted);
    }
    Ok(outcome)
}

fn count_subscription_states(
    states: &HashMap<Pubkey, SubscriptionRuntimeState>,
) -> SubscriptionStateCounts {
    let mut counts = SubscriptionStateCounts::default();
    for state in states.values() {
        match state {
            SubscriptionRuntimeState::Connecting => counts.connecting += 1,
            SubscriptionRuntimeState::Active => counts.active += 1,
            SubscriptionRuntimeState::Reconnecting => counts.reconnecting += 1,
            SubscriptionRuntimeState::Failed => counts.failed += 1,
            SubscriptionRuntimeState::Stopped => counts.stopped += 1,
        }
    }
    counts
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
) -> Result<kamino_reserve_monitor::timescale::TimescaleInsertOutcome> {
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

    persist_owned_update(timescale, update, snapshots, jsonl).await
}

async fn persist_owned_update(
    timescale: &TimescaleSink,
    update: OwnedReserveUpdate,
    snapshots: &mut HashMap<Pubkey, ReserveSnapshot>,
    jsonl: Option<&mut BufWriter<File>>,
) -> Result<kamino_reserve_monitor::timescale::TimescaleInsertOutcome> {
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

    snapshots.insert(update.snapshot.reserve, update.snapshot);
    Ok(insert_outcome)
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
