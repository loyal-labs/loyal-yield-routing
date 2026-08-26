use std::{
    collections::{HashMap, HashSet},
    str::FromStr,
    sync::{
        atomic::{AtomicBool, AtomicU64, Ordering},
        Arc,
    },
    time::Duration,
};

use anyhow::{Context, Result};
use balance_sweep_ata_monitor::earn_apy::{
    earn_apy_strategy_for_risk_profile, EarnApyRefreshConfig, EarnApySnapshotRefresher,
};
use balance_sweep_ata_monitor::{
    ata_target_set, diff_ata_target_sets, enqueue_normalized_earn_update,
    laserstream_replay_from_slot, run_autodeposit_reconciliation_consumer,
    run_earn_reconciliation_consumer, run_event_loop, seed_current_balances,
    spawn_ata_recheck_worker, AtaRecheckConfig, AtaRecheckHandle, AtaTarget, AtaUpdateSource,
    EarnMonitorMetrics, EarnUpdateContext, LaserstreamAtaUpdateSource,
    LaserstreamPolicyUpdateSource, NormalizedEarnUpdate, RpcEarnChainReader, SubscriptionConfig,
    SubscriptionWatchSet, TimescaleAtaConfig, TimescaleAtaObservationSink, TimescaleAtaStream,
    WebsocketAtaUpdateSource, EARN_IDLE_TOKEN_ACCOUNTS,
};
use chrono::Utc;
use clap::{Parser, ValueEnum};
use loyal_actions::{derive_associated_token_account, USDC_MINT};
use loyal_observability::{init_from_env, EarnRebalanceMetrics, OperationalError};
use loyal_squads_policy_monitor::{
    Cluster as PolicyCluster, Commitment as PolicyCommitment, MonitorConfig as PolicyMonitorConfig,
    PolicyMonitor, PostgresPolicyMatchSink, EARN_MAX_POLICY_PROJECTION_CONSUMER,
};
use loyal_yield_store::{OrchestratorConfig, OrchestratorError, OrchestratorStore};
use opentelemetry::metrics::Meter;
use solana_client::rpc_client::{GetConfirmedSignaturesForAddress2Config, RpcClient};
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey, signature::Signature};
use sqlx::postgres::PgListener;
use tokio::{
    sync::{mpsc, Mutex, Notify, RwLock},
    task::JoinHandle,
    time,
};

const EARN_APY_FAILURE_REPORT_THRESHOLD: u32 = 3;
const EARN_RECONCILIATION_CONCURRENCY: usize = 4;
const AUTODEPOSIT_RECONCILIATION_CONCURRENCY: usize = 4;
const EARN_MAX_POLICY_BOOTSTRAP_REPLAY_SLOTS: u64 = 10_000;
const EARN_MAX_ACCOUNT_HISTORY_PAGE_SIZE: usize = 1_000;
const EARN_MAX_ACCOUNT_HISTORY_LIMIT: usize = 10_000;

#[derive(Clone, Copy, Debug, PartialEq, Eq, ValueEnum)]
enum UpdateSourceKind {
    Laserstream,
    Websocket,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, ValueEnum)]
enum LaserstreamColdStartMode {
    #[default]
    Durable,
    Finalized,
}

#[derive(Debug, Parser)]
#[command(about = "Monitor wallet USDC ATAs for active Loyal balance sweep targets")]
struct Args {
    #[arg(
        long,
        env = "SOLANA_RPC_URL",
        default_value = "https://api.mainnet-beta.solana.com"
    )]
    rpc_url: String,
    #[arg(long, env = "SOLANA_WS_URL")]
    ws_url: Option<String>,
    #[arg(long, env = "NEON_DATABASE_URL")]
    postgres_url: String,
    #[arg(long, env = "TIMESCALEDB_URL")]
    timescaledb_url: String,
    #[arg(long, env = "BALANCE_SWEEP_ATA_STREAM", default_value = "production")]
    ata_stream: TimescaleAtaStream,
    #[arg(long, default_value = "mainnet")]
    cluster: String,
    #[arg(
        long,
        env = "BALANCE_SWEEP_UPDATE_SOURCE",
        default_value = "laserstream"
    )]
    update_source: UpdateSourceKind,
    #[arg(long, env = "HELIUS_API_KEY")]
    helius_api_key: Option<String>,
    #[arg(long, env = "LASERSTREAM_ENDPOINT")]
    laserstream_endpoint: Option<String>,
    #[arg(long, default_value_t = 32)]
    laserstream_replay_overlap_slots: u64,
    #[arg(
        long,
        env = "LASERSTREAM_COLD_START_MODE",
        value_enum,
        default_value_t = LaserstreamColdStartMode::Durable
    )]
    laserstream_cold_start_mode: LaserstreamColdStartMode,
    #[arg(
        long,
        env = "BALANCE_SWEEP_TARGET_REFRESH_SECONDS",
        default_value_t = 300,
        value_parser = clap::value_parser!(u64).range(1..)
    )]
    target_refresh_seconds: u64,
    #[arg(long)]
    once: bool,
    #[arg(
        long,
        env = "EARN_APY_REFRESH_INTERVAL_SECONDS",
        default_value_t = 3600,
        value_parser = clap::value_parser!(u64).range(1..)
    )]
    earn_apy_refresh_interval_seconds: u64,
    #[arg(long, env = "DISABLE_EARN_APY_REFRESH")]
    disable_earn_apy_refresh: bool,
    #[arg(long, env = "EARN_APY_RISK_PROFILES", default_value = "safe")]
    earn_apy_risk_profiles: String,
    #[arg(long)]
    earn_apy_only: bool,
    #[arg(
        long,
        env = "BALANCE_SWEEP_ATA_RECHECK_DELAY_SECONDS",
        default_value_t = 30,
        value_parser = clap::value_parser!(u64).range(1..)
    )]
    ata_recheck_delay_seconds: u64,
    #[arg(
        long,
        env = "BALANCE_SWEEP_ATA_RECHECK_RETRY_SECONDS",
        default_value_t = 30,
        value_parser = clap::value_parser!(u64).range(1..)
    )]
    ata_recheck_retry_seconds: u64,
    #[arg(
        long,
        env = "BALANCE_SWEEP_ATA_RECHECK_MAX_ATTEMPTS",
        default_value_t = 3,
        value_parser = clap::value_parser!(u32).range(1..)
    )]
    ata_recheck_max_attempts: u32,
}

struct MonitorSession {
    target_atas: HashSet<Pubkey>,
    earn_watch_set: SubscriptionWatchSet,
    processed_frontier: Arc<AtomicU64>,
    running: Arc<AtomicBool>,
    source_task: JoinHandle<()>,
    event_loop_task: JoinHandle<Result<()>>,
    finished: mpsc::UnboundedReceiver<()>,
}

impl MonitorSession {
    fn has_exited(&self) -> bool {
        self.source_task.is_finished() || self.event_loop_task.is_finished()
    }

    fn processed_frontier(&self) -> Option<u64> {
        let slot = self.processed_frontier.load(Ordering::Acquire);
        (slot > 0).then_some(slot)
    }
}

#[tokio::main]
async fn main() -> Result<()> {
    let observability = init_from_env("loyal-balance-sweep-ata-monitor")?;
    let meter = observability.meter("loyal-balance-sweep-ata-monitor");
    let earn_rebalance_metrics = observability.earn_rebalance_metrics();
    if let Err(error) = run(meter, earn_rebalance_metrics).await {
        OperationalError::new(
            "balance_sweep_ata_monitor_fatal",
            "run_balance_sweep_ata_monitor",
            "Balance sweep ATA monitor stopped after a fatal error",
        )
        .retryable(true)
        .recovery_required(true)
        .emit();
        return Err(error);
    }
    Ok(())
}

async fn run(meter: Meter, earn_rebalance_metrics: EarnRebalanceMetrics) -> Result<()> {
    let args = Args::parse();
    tracing::info!(
        cluster = %args.cluster,
        ata_stream = %args.ata_stream,
        update_source = ?args.update_source,
        target_refresh_seconds = args.target_refresh_seconds,
        once = args.once,
        "starting balance sweep ATA monitor"
    );
    if args.earn_apy_only {
        refresh_earn_apy_once(&args).await?;
        tracing::info!("exiting after one-shot Earn APY snapshot refresh");
        return Ok(());
    }

    let store =
        OrchestratorStore::connect(OrchestratorConfig::new(args.postgres_url.clone())).await?;
    let observations = TimescaleAtaObservationSink::connect(
        TimescaleAtaConfig::new(args.timescaledb_url.clone()).with_stream(args.ata_stream),
    )
    .await?;
    let config = SubscriptionConfig {
        max_reconnect_attempts: 10,
        reconnect_base_delay: Duration::from_millis(500),
        reconnect_max_delay: Duration::from_secs(30),
        heartbeat_interval: Duration::from_secs(15),
    };

    let targets = load_active_ata_targets(&store, &args.cluster).await?;
    let watch_set = load_subscription_watch_set(&store, &args.cluster, &targets).await?;
    if args.once {
        seed_current_balances(&args.rpc_url, &targets, &observations).await?;
        tracing::info!(
            target_count = targets.len(),
            "seeded current wallet ATA balances"
        );
        tracing::info!("exiting after one-shot balance seed");
        return Ok(());
    }

    if !args.disable_earn_apy_refresh {
        let refresher = connect_earn_apy_refresher(&args).await?;
        tokio::spawn(run_earn_apy_refresh_loop(
            refresher,
            Duration::from_secs(args.earn_apy_refresh_interval_seconds),
        ));
    }

    // Owned by the process, not the session, so rechecks queued by a session
    // still settle after that session is rebuilt.
    let (recheck, _recheck_task) = spawn_ata_recheck_worker(
        args.rpc_url.clone(),
        observations.clone(),
        AtaRecheckConfig {
            delay: Duration::from_secs(args.ata_recheck_delay_seconds),
            retry_backoff: Duration::from_secs(args.ata_recheck_retry_seconds),
            max_attempts: args.ata_recheck_max_attempts,
        },
    );

    // Reconciliation belongs to the process, not a LaserStream session. A
    // transport reconnect must not cancel a proof already in flight, and a
    // proof failure must never participate in session supervision.
    let earn_consumer_running = Arc::new(AtomicBool::new(true));
    let earn_wake = Arc::new(Notify::new());
    let earn_monitor_metrics = EarnMonitorMetrics::new(&meter, "earn-smart-account", &args.cluster);
    let policy_monitor = if args.update_source == UpdateSourceKind::Laserstream {
        let policy_cluster = match args.cluster.as_str() {
            "mainnet" | "mainnet-beta" => PolicyCluster::Mainnet,
            "devnet" => PolicyCluster::Devnet,
            other => anyhow::bail!("unsupported policy-monitor cluster {other}"),
        };
        let earn_max_delegate = std::env::var("EARN_MAX_DELEGATE")
            .context("EARN_MAX_DELEGATE is required for LaserStream policy projection")?
            .parse::<Pubkey>()
            .context("EARN_MAX_DELEGATE must be a Solana pubkey")?;
        let monitor = PolicyMonitor::new(
            PolicyMonitorConfig {
                cluster: policy_cluster,
                commitment: PolicyCommitment::Confirmed,
                ws_url: String::new(),
            },
            PostgresPolicyMatchSink::from_store(store.clone()),
        )
        .with_earn_max_projection(args.rpc_url.clone(), earn_max_delegate);
        Some(Arc::new(Mutex::new(monitor)))
    } else {
        None
    };
    let mut earn_consumer_tasks = Vec::new();
    let mut autodeposit_consumer_tasks = Vec::new();
    let mut policy_projection_task = None;
    if let Some(policy_monitor) = policy_monitor {
        let consumer_name = format!("earn-smart-account:{}", args.cluster);
        let chain = Arc::new(RpcEarnChainReader::new(&args.rpc_url, store.clone()));
        let policy_from_slot = earn_max_policy_replay_start_slot(
            &store,
            &args.rpc_url,
            args.laserstream_replay_overlap_slots,
        )
        .await?;
        policy_projection_task = Some(
            LaserstreamPolicyUpdateSource {
                endpoint: args
                    .laserstream_endpoint
                    .clone()
                    .ok_or_else(|| anyhow::anyhow!("LASERSTREAM_ENDPOINT is required"))?,
                api_key: args
                    .helius_api_key
                    .clone()
                    .ok_or_else(|| anyhow::anyhow!("HELIUS_API_KEY is required"))?,
                from_slot: policy_from_slot,
                config,
            }
            .spawn(
                store.clone(),
                policy_monitor.clone(),
                earn_consumer_running.clone(),
            ),
        );
        for worker_index in 0..AUTODEPOSIT_RECONCILIATION_CONCURRENCY {
            let claim_owner = format!(
                "autodeposit:{}:{}:{}:{}",
                args.cluster,
                std::process::id(),
                Utc::now().timestamp_nanos_opt().unwrap_or_default(),
                worker_index
            );
            autodeposit_consumer_tasks.push(tokio::spawn(run_autodeposit_reconciliation_consumer(
                store.clone(),
                claim_owner,
                chain.clone(),
                earn_wake.clone(),
                earn_consumer_running.clone(),
            )));
        }
        for worker_index in 0..EARN_RECONCILIATION_CONCURRENCY {
            let claim_owner = format!(
                "{}:{}:{}:{}",
                consumer_name,
                std::process::id(),
                Utc::now().timestamp_nanos_opt().unwrap_or_default(),
                worker_index
            );
            earn_consumer_tasks.push(tokio::spawn(run_earn_reconciliation_consumer(
                store.clone(),
                consumer_name.clone(),
                claim_owner,
                chain.clone(),
                policy_monitor.clone(),
                earn_wake.clone(),
                earn_consumer_running.clone(),
                earn_monitor_metrics.clone(),
            )));
        }
    }
    let autodeposit_watch_wake = Arc::new(Notify::new());
    let autodeposit_watch_task = tokio::spawn(run_autodeposit_watch_listener(
        args.postgres_url.clone(),
        autodeposit_watch_wake.clone(),
    ));
    let result = supervise_monitor_sessions(
        args,
        store,
        observations,
        config,
        targets,
        watch_set,
        recheck,
        earn_wake.clone(),
        autodeposit_watch_wake,
        earn_rebalance_metrics,
    )
    .await;
    earn_consumer_running.store(false, Ordering::Relaxed);
    earn_wake.notify_waiters();
    for task in earn_consumer_tasks {
        task.abort();
        let _ = task.await;
    }
    for task in autodeposit_consumer_tasks {
        task.abort();
        let _ = task.await;
    }
    if let Some(task) = policy_projection_task {
        task.abort();
        let _ = task.await;
    }
    autodeposit_watch_task.abort();
    let _ = autodeposit_watch_task.await;
    result
}

async fn run_autodeposit_watch_listener(database_url: String, wake: Arc<Notify>) {
    loop {
        match PgListener::connect(&database_url).await {
            Ok(mut listener) => {
                if let Err(error) = listener.listen("loyal_yield_autodeposit_watch").await {
                    tracing::warn!(error = %error, "failed to LISTEN for Autodeposit watch changes");
                } else {
                    loop {
                        match listener.recv().await {
                            Ok(_) => wake.notify_one(),
                            Err(error) => {
                                tracing::warn!(error = %error, "Autodeposit watch listener disconnected");
                                break;
                            }
                        }
                    }
                }
            }
            Err(error) => {
                tracing::warn!(error = %error, "failed to connect Autodeposit watch listener");
            }
        }
        time::sleep(Duration::from_secs(5)).await;
    }
}

async fn connect_earn_apy_refresher(args: &Args) -> Result<EarnApySnapshotRefresher> {
    let config = EarnApyRefreshConfig {
        strategies: parse_earn_apy_strategies(&args.earn_apy_risk_profiles)?,
        ..EarnApyRefreshConfig::default()
    };
    EarnApySnapshotRefresher::connect(&args.timescaledb_url, &args.postgres_url, config).await
}

fn parse_earn_apy_strategies(
    value: &str,
) -> Result<Vec<balance_sweep_ata_monitor::earn_apy::EarnApyStrategy>> {
    let strategies = value
        .split(',')
        .map(str::trim)
        .filter(|profile| !profile.is_empty())
        .map(|profile| {
            earn_apy_strategy_for_risk_profile(profile)
                .ok_or_else(|| anyhow::anyhow!("unsupported Earn APY risk profile: {profile}"))
        })
        .collect::<Result<Vec<_>>>()?;
    if strategies.is_empty() {
        anyhow::bail!("at least one Earn APY risk profile is required");
    }
    Ok(strategies)
}

async fn refresh_earn_apy_once(args: &Args) -> Result<()> {
    let refresher = connect_earn_apy_refresher(args).await?;
    let outcome = refresher.refresh(Utc::now()).await?;
    tracing::info!(
        generated_at = %outcome.generated_at,
        profiles = outcome.profiles,
        inserted_or_updated = outcome.inserted_or_updated,
        first_sample_hour = ?outcome.first_sample_hour,
        last_sample_hour = ?outcome.last_sample_hour,
        "refreshed hourly Earn APY snapshots"
    );
    Ok(())
}

async fn run_earn_apy_refresh_loop(
    refresher: EarnApySnapshotRefresher,
    refresh_interval: Duration,
) {
    let mut consecutive_failures = 0_u32;
    loop {
        let now = Utc::now();
        match refresher.refresh(now).await {
            Ok(outcome) => {
                consecutive_failures = 0;
                tracing::info!(
                    generated_at = %outcome.generated_at,
                    profiles = outcome.profiles,
                    inserted_or_updated = outcome.inserted_or_updated,
                    first_sample_hour = ?outcome.first_sample_hour,
                    last_sample_hour = ?outcome.last_sample_hour,
                    "refreshed hourly Earn APY snapshots"
                );
            }
            Err(error) => {
                consecutive_failures = consecutive_failures.saturating_add(1);
                if consecutive_failures == EARN_APY_FAILURE_REPORT_THRESHOLD {
                    OperationalError::new(
                        "earn_apy_refresh_stalled",
                        "refresh_earn_apy_snapshots",
                        "Earn APY snapshot refresh failed repeatedly",
                    )
                    .retryable(true)
                    .recovery_required(true)
                    .emit();
                }
                tracing::warn!(error = %error, "failed to refresh hourly Earn APY snapshots");
            }
        }

        time::sleep(refresh_interval).await;
    }
}

async fn supervise_monitor_sessions(
    args: Args,
    store: OrchestratorStore,
    observations: TimescaleAtaObservationSink,
    config: SubscriptionConfig,
    initial_targets: Vec<AtaTarget>,
    initial_watch_set: SubscriptionWatchSet,
    recheck: AtaRecheckHandle,
    earn_wake: Arc<Notify>,
    autodeposit_watch_wake: Arc<Notify>,
    earn_rebalance_metrics: EarnRebalanceMetrics,
) -> Result<()> {
    let refresh_interval = Duration::from_secs(args.target_refresh_seconds);
    let mut session: Option<MonitorSession> = None;
    let mut next_state = Some((initial_targets, initial_watch_set));
    let mut resume_from_durable_cursor = false;
    let mut finalized_cold_start_pending =
        args.laserstream_cold_start_mode == LaserstreamColdStartMode::Finalized;

    loop {
        if session.as_ref().is_some_and(MonitorSession::has_exited) {
            let finished = session.take().expect("checked session exists");
            log_finished_session(finished).await;
            resume_from_durable_cursor = true;
        }

        let (targets, mut watch_set) = match next_state.take() {
            Some(state) => state,
            None => {
                let targets = load_active_ata_targets(&store, &args.cluster).await?;
                let watch_set =
                    load_subscription_watch_set(&store, &args.cluster, &targets).await?;
                (targets, watch_set)
            }
        };
        let new_earn_binding_observation_start_slot = watch_set
            .new_earn_binding_observation_start_slot(
                session.as_ref().map(|existing| &existing.earn_watch_set),
            );
        if let Some(existing) = session.as_ref() {
            watch_set.retain_previous_earn_bindings(&existing.earn_watch_set)?;
        }
        if args.update_source == UpdateSourceKind::Laserstream {
            match enqueue_earn_max_rpc_gap_updates(
                &store,
                &args.rpc_url,
                &format!("earn-smart-account:{}", args.cluster),
                &watch_set,
            )
            .await
            {
                Ok(inserted_jobs) if inserted_jobs > 0 => {
                    tracing::info!(
                        inserted_jobs,
                        "enqueued Earn MAX custody updates recovered from confirmed account history"
                    );
                    earn_wake.notify_waiters();
                }
                Ok(_) => {}
                Err(error) => tracing::warn!(
                    error = %error,
                    "failed to reconcile Earn MAX custody history gap"
                ),
            }
        }
        let desired_atas = ata_target_set(&targets);
        let current_atas = session
            .as_ref()
            .map(|session| session.target_atas.clone())
            .unwrap_or_default();
        let diff = diff_ata_target_sets(&current_atas, &desired_atas);
        let earn_changed = session
            .as_ref()
            .is_none_or(|session| session.earn_watch_set != watch_set);

        tracing::info!(
            target_count = targets.len(),
            added_count = diff.added.len(),
            removed_count = diff.removed.len(),
            earn_vault_count = watch_set.earn_vaults.len(),
            earn_changed,
            "loaded active balance sweep ATA targets"
        );

        if desired_atas.is_empty() && watch_set.earn_vaults.is_empty() {
            if let Some(existing) = session.take() {
                tracing::info!(
                    "stopping balance sweep ATA subscription because target set is empty"
                );
                stop_session(existing).await;
            }
            resume_from_durable_cursor = false;
            tracing::info!(
                refresh_seconds = args.target_refresh_seconds,
                "waiting for active balance sweep ATA targets"
            );
        } else if session_requires_rebuild(session.is_none(), diff.has_changes(), earn_changed) {
            let watch_set_replay_from_slot_override =
                replay_override_for_watch_set_change(WatchSetReplayContext {
                    session_present: session.is_some(),
                    resume_from_durable_cursor,
                    watch_set_changed: diff.has_changes() || earn_changed,
                    processed_frontier: session
                        .as_ref()
                        .and_then(MonitorSession::processed_frontier),
                    new_earn_binding_observation_start_slot,
                    replay_overlap_slots: args.laserstream_replay_overlap_slots,
                });
            if let Some(existing) = session.take() {
                tracing::info!(
                    added_count = diff.added.len(),
                    removed_count = diff.removed.len(),
                    earn_changed,
                    "rebuilding balance sweep ATA subscription for refreshed target set"
                );
                stop_session(existing).await;
            }

            let added_targets = targets
                .iter()
                .filter(|target| diff.added.contains(&target.wallet_usdc_ata))
                .cloned()
                .collect::<Vec<_>>();
            seed_current_balances(&args.rpc_url, &added_targets, &observations).await?;
            tracing::info!(
                seeded_target_count = added_targets.len(),
                target_count = targets.len(),
                "seeded current wallet ATA balances before subscription start"
            );

            let replay_from_slot_override = if should_use_finalized_cold_start(
                finalized_cold_start_pending,
                resume_from_durable_cursor,
                args.update_source,
            ) {
                let (finalized_slot, from_slot) = finalized_laserstream_replay_start_slot(
                    &args.rpc_url,
                    args.laserstream_replay_overlap_slots,
                )
                .await?;
                tracing::info!(
                    finalized_slot,
                    replay_overlap_slots = args.laserstream_replay_overlap_slots,
                    from_slot,
                    "starting first Laserstream session from finalized chain tip minus overlap"
                );
                Some(from_slot)
            } else {
                watch_set_replay_from_slot_override
            };

            let started = start_session(
                &args,
                targets,
                watch_set,
                store.clone(),
                observations.clone(),
                config,
                recheck.clone(),
                replay_from_slot_override,
                earn_wake.clone(),
                earn_rebalance_metrics.clone(),
            )
            .await
            .context("start balance sweep ATA monitor session")?;
            session = Some(started);
            finalized_cold_start_pending = false;
            resume_from_durable_cursor = false;
        } else {
            tracing::debug!(
                target_count = targets.len(),
                "balance sweep ATA target set unchanged"
            );
        }

        if let Some(existing) = session.as_mut() {
            if wait_for_refresh_or_session_exit(
                refresh_interval,
                &mut existing.finished,
                &autodeposit_watch_wake,
            )
            .await
            {
                let finished = session.take().expect("session exit was observed");
                log_finished_session(finished).await;
                resume_from_durable_cursor = true;
            }
        } else {
            tokio::select! {
                _ = time::sleep(refresh_interval) => {}
                _ = autodeposit_watch_wake.notified() => {}
            }
        }
    }
}

async fn enqueue_earn_max_rpc_gap_updates(
    store: &OrchestratorStore,
    rpc_url: &str,
    consumer_name: &str,
    watch_set: &SubscriptionWatchSet,
) -> Result<usize> {
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    let mut inserted_jobs = 0_usize;
    for vault in watch_set.earn_vaults.iter().filter(|vault| vault.earn_max) {
        let route_key = format!("earn-max:{}:{}", vault.settings, vault.vault_index);
        let Some(stored) = store
            .load_multiply_route_state(&route_key)
            .await
            .map_err(orchestrator_error)?
        else {
            continue;
        };
        if stored.state.engine_version != "earn_max_v2" {
            continue;
        }
        let vault_pubkey = Pubkey::from_str(&vault.vault)
            .with_context(|| format!("invalid Earn MAX vault {}", vault.vault))?;
        let claim_custody = derive_associated_token_account(vault_pubkey, USDC_MINT, spl_token::ID);
        let anchor_slot = stored.state.observed_slot;
        let mut before = None;
        let mut candidates = Vec::new();
        let mut reached_anchor = false;
        loop {
            let page = rpc
                .get_signatures_for_address_with_config(
                    &claim_custody,
                    GetConfirmedSignaturesForAddress2Config {
                        before,
                        until: None,
                        limit: Some(EARN_MAX_ACCOUNT_HISTORY_PAGE_SIZE),
                        commitment: Some(CommitmentConfig::confirmed()),
                    },
                )
                .with_context(|| {
                    format!("read confirmed Earn MAX custody history for {claim_custody}")
                })?;
            if page.is_empty() {
                break;
            }
            for status in &page {
                if status.slot <= anchor_slot {
                    reached_anchor = true;
                    break;
                }
                if status.err.is_none() {
                    candidates.push((status.slot, status.signature.clone()));
                }
            }
            if reached_anchor || page.len() < EARN_MAX_ACCOUNT_HISTORY_PAGE_SIZE {
                break;
            }
            if candidates.len() >= EARN_MAX_ACCOUNT_HISTORY_LIMIT {
                anyhow::bail!(
                    "Earn MAX custody history exceeded {} signatures before anchor slot {} for {}",
                    EARN_MAX_ACCOUNT_HISTORY_LIMIT,
                    anchor_slot,
                    claim_custody
                );
            }
            before = Some(Signature::from_str(
                &page
                    .last()
                    .context("Earn MAX custody history page unexpectedly became empty")?
                    .signature,
            )?);
        }
        candidates.sort_by(|left, right| left.cmp(right));
        for (slot, signature) in candidates {
            let update = NormalizedEarnUpdate {
                event_key: Some(format!(
                    "earn-max-rpc-gap:{slot}:{signature}:{claim_custody}"
                )),
                filters: vec![EARN_IDLE_TOKEN_ACCOUNTS.to_owned()],
                event_kind: "account".to_owned(),
                account_pubkey: Some(claim_custody.to_string()),
                slot,
                signature: Some(signature),
            };
            let outcome =
                enqueue_normalized_earn_update(store, consumer_name, &update, watch_set).await?;
            inserted_jobs = inserted_jobs.saturating_add(outcome.inserted_jobs);
        }
    }
    Ok(inserted_jobs)
}

fn session_requires_rebuild(session_missing: bool, ata_changed: bool, earn_changed: bool) -> bool {
    session_missing || ata_changed || earn_changed
}

fn should_use_finalized_cold_start(
    finalized_cold_start_pending: bool,
    resume_from_durable_cursor: bool,
    update_source: UpdateSourceKind,
) -> bool {
    finalized_cold_start_pending
        && !resume_from_durable_cursor
        && update_source == UpdateSourceKind::Laserstream
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct WatchSetReplayContext {
    session_present: bool,
    resume_from_durable_cursor: bool,
    watch_set_changed: bool,
    processed_frontier: Option<u64>,
    new_earn_binding_observation_start_slot: Option<u64>,
    replay_overlap_slots: u64,
}

fn replay_override_for_watch_set_change(context: WatchSetReplayContext) -> Option<u64> {
    if !context.watch_set_changed || context.resume_from_durable_cursor {
        return None;
    }

    let continuity_start = context
        .session_present
        .then_some(context.processed_frontier)
        .flatten()
        .map(|slot| laserstream_replay_from_slot(slot, context.replay_overlap_slots));
    let new_binding_start = context
        .new_earn_binding_observation_start_slot
        .map(|slot| laserstream_replay_from_slot(slot, context.replay_overlap_slots));

    match (continuity_start, new_binding_start) {
        (Some(checkpoint), Some(start)) => Some(checkpoint.min(start)),
        (checkpoint, start) => checkpoint.or(start),
    }
}

async fn wait_for_refresh_or_session_exit(
    refresh_interval: Duration,
    finished: &mut mpsc::UnboundedReceiver<()>,
    autodeposit_watch_wake: &Notify,
) -> bool {
    tokio::select! {
        _ = time::sleep(refresh_interval) => false,
        _ = finished.recv() => true,
        _ = autodeposit_watch_wake.notified() => false,
    }
}

async fn load_active_ata_targets(
    store: &OrchestratorStore,
    cluster: &str,
) -> Result<Vec<AtaTarget>> {
    let usdc_mint = USDC_MINT.to_string();
    let targets = store
        .load_active_balance_sweep_targets()
        .await
        .map_err(orchestrator_error)?
        .iter()
        .filter(|target| target.token_mint.as_str() == usdc_mint.as_str())
        .map(|target| AtaTarget::from_balance_sweep_target(target, cluster))
        .collect::<Result<Vec<_>>>()?;
    Ok(targets)
}

async fn load_subscription_watch_set(
    store: &OrchestratorStore,
    environment: &str,
    balance_targets: &[AtaTarget],
) -> Result<SubscriptionWatchSet> {
    let targets = store
        .load_earn_subscription_targets(environment)
        .await
        .map_err(orchestrator_error)?;
    SubscriptionWatchSet::from_targets(
        balance_targets
            .iter()
            .map(|target| target.wallet_usdc_ata.to_string())
            .collect(),
        targets,
    )
}

fn orchestrator_error(error: OrchestratorError) -> anyhow::Error {
    anyhow::anyhow!(error)
}

async fn start_session(
    args: &Args,
    targets: Vec<AtaTarget>,
    watch_set: SubscriptionWatchSet,
    store: OrchestratorStore,
    observations: TimescaleAtaObservationSink,
    config: SubscriptionConfig,
    recheck: AtaRecheckHandle,
    replay_from_slot_override: Option<u64>,
    earn_wake: Arc<Notify>,
    earn_rebalance_metrics: EarnRebalanceMetrics,
) -> Result<MonitorSession> {
    if args.update_source == UpdateSourceKind::Websocket && !watch_set.earn_vaults.is_empty() {
        anyhow::bail!(
            "Earn smart-account monitoring requires BALANCE_SWEEP_UPDATE_SOURCE=laserstream"
        );
    }
    let accounts = targets
        .iter()
        .map(|target| target.wallet_usdc_ata)
        .collect::<Vec<_>>();
    let target_atas = ata_target_set(&targets);
    let target_by_ata = targets
        .into_iter()
        .map(|target| (target.wallet_usdc_ata, target))
        .collect::<HashMap<_, _>>();
    let (tx, rx) = mpsc::unbounded_channel();
    let (finished_tx, finished) = mpsc::unbounded_channel();
    let running = Arc::new(AtomicBool::new(true));
    let processed_frontier = Arc::new(AtomicU64::new(0));
    let watch_set_state = Arc::new(RwLock::new(watch_set.clone()));
    let raw_source_task = match args.update_source {
        UpdateSourceKind::Laserstream => {
            let consumer_name = format!("earn-smart-account:{}", args.cluster);
            let from_slot = match replay_from_slot_override {
                Some(from_slot) => from_slot,
                None => {
                    laserstream_replay_start_slot(
                        &store,
                        &consumer_name,
                        &args.rpc_url,
                        args.laserstream_replay_overlap_slots,
                    )
                    .await?
                }
            };
            processed_frontier.store(from_slot, Ordering::Release);
            let source = LaserstreamAtaUpdateSource {
                endpoint: args
                    .laserstream_endpoint
                    .clone()
                    .ok_or_else(|| anyhow::anyhow!("LASERSTREAM_ENDPOINT is required"))?,
                api_key: args
                    .helius_api_key
                    .clone()
                    .ok_or_else(|| anyhow::anyhow!("HELIUS_API_KEY is required"))?,
                from_slot,
                config,
                watch_set: Some(watch_set.clone()),
            };
            source.spawn_with_watch_set(accounts, tx, running.clone(), watch_set_state.clone())
        }
        UpdateSourceKind::Websocket => WebsocketAtaUpdateSource {
            ws_url: args
                .ws_url
                .clone()
                .ok_or_else(|| anyhow::anyhow!("SOLANA_WS_URL is required"))?,
            config,
        }
        .spawn(accounts, tx, running.clone()),
    };
    let source_finished_tx = finished_tx.clone();
    let source_task = tokio::spawn(async move {
        if let Err(error) = raw_source_task.await {
            if !error.is_cancelled() {
                tracing::warn!(error = %error, "balance sweep ATA source task failed");
            }
        }
        let _ = source_finished_tx.send(());
    });
    let consumer_name = format!("earn-smart-account:{}", args.cluster);
    let earn = (args.update_source == UpdateSourceKind::Laserstream).then(|| EarnUpdateContext {
        store,
        consumer_name,
        watch_set: watch_set_state,
        wake: earn_wake,
    });
    let event_running = running.clone();
    let event_processed_frontier = processed_frontier.clone();
    let event_finished_tx = finished_tx.clone();
    let event_loop_task = tokio::spawn(async move {
        let result = run_event_loop(
            rx,
            target_by_ata,
            observations,
            event_running,
            Some(recheck),
            earn,
            earn_rebalance_metrics,
            event_processed_frontier,
        )
        .await;
        let _ = event_finished_tx.send(());
        result
    });
    drop(finished_tx);
    Ok(MonitorSession {
        target_atas,
        earn_watch_set: watch_set,
        processed_frontier,
        running,
        source_task,
        event_loop_task,
        finished,
    })
}

async fn laserstream_replay_start_slot(
    store: &OrchestratorStore,
    consumer_name: &str,
    rpc_url: &str,
    replay_overlap_slots: u64,
) -> Result<u64> {
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    let current_slot = rpc
        .get_slot()
        .context("fetch confirmed RPC slot for Laserstream replay overlap")?;
    let current_fallback = laserstream_replay_from_slot(current_slot, replay_overlap_slots);
    let durable = store
        .load_laserstream_replay_cursor(consumer_name)
        .await
        .map_err(orchestrator_error)?;
    Ok(durable
        .map(|slot| laserstream_replay_from_slot(slot, replay_overlap_slots).min(current_slot))
        .unwrap_or(current_fallback))
}

async fn finalized_laserstream_replay_start_slot(
    rpc_url: &str,
    replay_overlap_slots: u64,
) -> Result<(u64, u64)> {
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::finalized());
    let finalized_slot = rpc
        .get_slot()
        .context("fetch finalized RPC slot for Laserstream cold start")?;
    Ok((
        finalized_slot,
        laserstream_replay_from_slot(finalized_slot, replay_overlap_slots),
    ))
}

async fn earn_max_policy_replay_start_slot(
    store: &OrchestratorStore,
    rpc_url: &str,
    replay_overlap_slots: u64,
) -> Result<u64> {
    earn_max_projection_replay_start_slot(
        store,
        rpc_url,
        EARN_MAX_POLICY_PROJECTION_CONSUMER,
        replay_overlap_slots,
    )
    .await
}

async fn earn_max_projection_replay_start_slot(
    store: &OrchestratorStore,
    rpc_url: &str,
    consumer_name: &str,
    replay_overlap_slots: u64,
) -> Result<u64> {
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    let current_slot = rpc
        .get_slot()
        .context("fetch confirmed RPC slot for Earn MAX policy replay")?;
    let durable = store
        .projection_offset(consumer_name)
        .await
        .map_err(orchestrator_error)?;
    if durable > 0 {
        return Ok(laserstream_replay_from_slot(
            u64::try_from(durable).context("Earn MAX policy cursor is negative")?,
            replay_overlap_slots,
        )
        .min(current_slot));
    }
    Ok(laserstream_replay_from_slot(
        current_slot,
        replay_overlap_slots.max(EARN_MAX_POLICY_BOOTSTRAP_REPLAY_SLOTS),
    ))
}

async fn stop_session(session: MonitorSession) {
    session.running.store(false, Ordering::Relaxed);
    session.source_task.abort();
    session.event_loop_task.abort();
    let _ = session.source_task.await;
    let _ = session.event_loop_task.await;
}

async fn log_finished_session(session: MonitorSession) {
    OperationalError::new(
        "balance_sweep_ata_session_failed",
        "run_balance_sweep_ata_session",
        "Balance sweep ATA monitor session exited unexpectedly",
    )
    .retryable(true)
    .recovery_required(false)
    .emit();
    tracing::warn!("balance sweep ATA monitor session exited before refresh");
    session.running.store(false, Ordering::Relaxed);

    if session.source_task.is_finished() {
        if let Err(error) = session.source_task.await {
            if !error.is_cancelled() {
                tracing::warn!(error = %error, "balance sweep ATA source task failed");
            }
        }
    } else {
        session.source_task.abort();
        let _ = session.source_task.await;
    }

    if session.event_loop_task.is_finished() {
        match session.event_loop_task.await {
            Ok(Ok(())) => {}
            Ok(Err(error)) => tracing::warn!(error = %error, "balance sweep ATA event loop failed"),
            Err(error) if error.is_cancelled() => {}
            Err(error) => {
                tracing::warn!(error = %error, "balance sweep ATA event loop task failed")
            }
        }
    } else {
        session.event_loop_task.abort();
        let _ = session.event_loop_task.await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn watch_change_replays_from_live_processed_frontier() {
        assert!(session_requires_rebuild(false, false, true));
        assert_eq!(
            replay_override_for_watch_set_change(WatchSetReplayContext {
                session_present: true,
                resume_from_durable_cursor: false,
                watch_set_changed: true,
                processed_frontier: Some(90),
                new_earn_binding_observation_start_slot: None,
                replay_overlap_slots: 32,
            }),
            Some(58)
        );
    }

    #[test]
    fn failed_session_restarts_from_durable_cursor() {
        assert!(!should_use_finalized_cold_start(
            true,
            true,
            UpdateSourceKind::Laserstream,
        ));
        assert_eq!(
            replay_override_for_watch_set_change(WatchSetReplayContext {
                session_present: false,
                resume_from_durable_cursor: true,
                watch_set_changed: true,
                processed_frontier: Some(900),
                new_earn_binding_observation_start_slot: Some(700),
                replay_overlap_slots: 32,
            }),
            None
        );
    }

    #[test]
    fn finalized_cold_start_only_applies_to_first_laserstream_session() {
        assert!(should_use_finalized_cold_start(
            true,
            false,
            UpdateSourceKind::Laserstream,
        ));
        assert!(!should_use_finalized_cold_start(
            false,
            false,
            UpdateSourceKind::Laserstream,
        ));
        assert!(!should_use_finalized_cold_start(
            true,
            false,
            UpdateSourceKind::Websocket,
        ));
    }

    #[tokio::test]
    async fn failed_session_wakes_supervisor_before_refresh_deadline() {
        let (finished_tx, mut finished_rx) = mpsc::unbounded_channel();
        finished_tx.send(()).unwrap();
        let watch_wake = Notify::new();

        let woke_for_exit = time::timeout(
            Duration::from_millis(100),
            wait_for_refresh_or_session_exit(
                Duration::from_secs(300),
                &mut finished_rx,
                &watch_wake,
            ),
        )
        .await
        .expect("supervisor stayed asleep after session exit");

        assert!(woke_for_exit);
    }

    #[test]
    fn autodeposit_new_watch_replays_from_configuration_boundary() {
        assert_eq!(
            replay_override_for_watch_set_change(WatchSetReplayContext {
                session_present: true,
                resume_from_durable_cursor: false,
                watch_set_changed: true,
                processed_frontier: Some(900),
                new_earn_binding_observation_start_slot: Some(700),
                replay_overlap_slots: 32,
            }),
            Some(668)
        );
        assert_eq!(
            replay_override_for_watch_set_change(WatchSetReplayContext {
                session_present: false,
                resume_from_durable_cursor: false,
                watch_set_changed: true,
                processed_frontier: None,
                new_earn_binding_observation_start_slot: Some(700),
                replay_overlap_slots: 32,
            }),
            Some(668)
        );
    }

    #[test]
    fn ata_only_watch_change_uses_live_frontier() {
        assert_eq!(
            replay_override_for_watch_set_change(WatchSetReplayContext {
                session_present: true,
                resume_from_durable_cursor: false,
                watch_set_changed: true,
                processed_frontier: Some(900),
                new_earn_binding_observation_start_slot: None,
                replay_overlap_slots: 32,
            }),
            Some(868)
        );
    }
}
