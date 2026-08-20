use std::{
    collections::{HashMap, HashSet},
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};

use anyhow::{Context, Result};
use balance_sweep_ata_monitor::earn_apy::{
    earn_apy_strategy_for_risk_profile, EarnApyRefreshConfig, EarnApySnapshotRefresher,
};
use balance_sweep_ata_monitor::{
    ata_target_set, diff_ata_target_sets, laserstream_replay_from_slot, run_event_loop,
    seed_current_balances, spawn_ata_recheck_worker, AtaRecheckConfig, AtaRecheckHandle, AtaTarget,
    AtaUpdateSource, EarnUpdateContext, LaserstreamAtaUpdateSource, RpcEarnChainReader,
    SubscriptionConfig, SubscriptionWatchSet, TimescaleAtaConfig, TimescaleAtaObservationSink,
    TimescaleAtaStream, WebsocketAtaUpdateSource,
};
use chrono::Utc;
use clap::{Parser, ValueEnum};
use loyal_actions::USDC_MINT;
use loyal_observability::{init_from_env, OperationalError};
use loyal_yield_store::{OrchestratorConfig, OrchestratorError, OrchestratorStore};
use solana_client::rpc_client::RpcClient;
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey};
use tokio::{
    sync::{mpsc, RwLock},
    task::JoinHandle,
    time,
};

const EARN_APY_FAILURE_REPORT_THRESHOLD: u32 = 3;

#[derive(Clone, Copy, Debug, PartialEq, Eq, ValueEnum)]
enum UpdateSourceKind {
    Laserstream,
    Websocket,
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
    replay_from_slot: Option<u64>,
    running: Arc<AtomicBool>,
    source_task: JoinHandle<()>,
    event_loop_task: JoinHandle<Result<()>>,
    finished: mpsc::UnboundedReceiver<()>,
}

impl MonitorSession {
    fn has_exited(&self) -> bool {
        self.source_task.is_finished() || self.event_loop_task.is_finished()
    }
}

#[tokio::main]
async fn main() -> Result<()> {
    let _observability = init_from_env("loyal-balance-sweep-ata-monitor")?;
    if let Err(error) = run().await {
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

async fn run() -> Result<()> {
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

    supervise_monitor_sessions(
        args,
        store,
        observations,
        config,
        targets,
        watch_set,
        recheck,
    )
    .await
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
) -> Result<()> {
    let refresh_interval = Duration::from_secs(args.target_refresh_seconds);
    let mut session: Option<MonitorSession> = None;
    let mut next_state = Some((initial_targets, initial_watch_set));
    let mut earn_replay_checkpoint = None;

    loop {
        if session.as_ref().is_some_and(MonitorSession::has_exited) {
            let finished = session.take().expect("checked session exists");
            log_finished_session(finished).await;
        }

        let refresh_replay_boundary =
            if session.is_some() && args.update_source == UpdateSourceKind::Laserstream {
                match laserstream_current_replay_boundary(
                    &args.rpc_url,
                    args.laserstream_replay_overlap_slots,
                )
                .await
                {
                    Ok(boundary) => Some(boundary),
                    Err(error) => {
                        tracing::warn!(
                            error = %error,
                            "failed to advance Earn watch-set replay checkpoint"
                        );
                        None
                    }
                }
            } else {
                None
            };

        let (targets, mut watch_set) = match next_state.take() {
            Some(state) => state,
            None => {
                let targets = load_active_ata_targets(&store, &args.cluster).await?;
                let watch_set =
                    load_subscription_watch_set(&store, &args.cluster, &targets).await?;
                (targets, watch_set)
            }
        };
        if let Some(existing) = session.as_ref() {
            watch_set.retain_previous_earn_bindings(&existing.earn_watch_set)?;
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
            earn_replay_checkpoint = None;
            tracing::info!(
                refresh_seconds = args.target_refresh_seconds,
                "waiting for active balance sweep ATA targets"
            );
        } else if session_requires_rebuild(session.is_none(), diff.has_changes(), earn_changed) {
            let replay_from_slot_override = replay_override_for_watch_set_change(
                session.is_some(),
                earn_changed,
                earn_replay_checkpoint,
            );
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

            let started = start_session(
                &args,
                targets,
                watch_set,
                store.clone(),
                observations.clone(),
                config,
                recheck.clone(),
                replay_from_slot_override,
            )
            .await
            .context("start balance sweep ATA monitor session")?;
            earn_replay_checkpoint = replay_checkpoint_after_session_start(
                refresh_replay_boundary,
                started.replay_from_slot,
            );
            session = Some(started);
        } else {
            tracing::debug!(
                target_count = targets.len(),
                "balance sweep ATA target set unchanged"
            );
        }

        if session.is_some() && !earn_changed {
            earn_replay_checkpoint = refresh_replay_boundary.or(earn_replay_checkpoint);
        }

        if let Some(existing) = session.as_mut() {
            if wait_for_refresh_or_session_exit(refresh_interval, &mut existing.finished).await {
                let finished = session.take().expect("session exit was observed");
                log_finished_session(finished).await;
            }
        } else {
            time::sleep(refresh_interval).await;
        }
    }
}

fn session_requires_rebuild(session_missing: bool, ata_changed: bool, earn_changed: bool) -> bool {
    session_missing || ata_changed || earn_changed
}

fn replay_override_for_watch_set_change(
    session_present: bool,
    earn_changed: bool,
    replay_checkpoint: Option<u64>,
) -> Option<u64> {
    (session_present && earn_changed)
        .then_some(replay_checkpoint)
        .flatten()
}

fn replay_checkpoint_after_session_start(
    refresh_boundary: Option<u64>,
    session_replay_from_slot: Option<u64>,
) -> Option<u64> {
    refresh_boundary.or(session_replay_from_slot)
}

async fn wait_for_refresh_or_session_exit(
    refresh_interval: Duration,
    finished: &mut mpsc::UnboundedReceiver<()>,
) -> bool {
    tokio::select! {
        _ = time::sleep(refresh_interval) => false,
        _ = finished.recv() => true,
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
    let watch_set_state = Arc::new(RwLock::new(watch_set.clone()));
    let (raw_source_task, replay_from_slot) = match args.update_source {
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
            (
                source.spawn_with_watch_set(accounts, tx, running.clone(), watch_set_state.clone()),
                Some(from_slot),
            )
        }
        UpdateSourceKind::Websocket => (
            WebsocketAtaUpdateSource {
                ws_url: args
                    .ws_url
                    .clone()
                    .ok_or_else(|| anyhow::anyhow!("SOLANA_WS_URL is required"))?,
                config,
            }
            .spawn(accounts, tx, running.clone()),
            None,
        ),
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
    let earn = (args.update_source == UpdateSourceKind::Laserstream).then(|| EarnUpdateContext {
        chain: Arc::new(RpcEarnChainReader::new(&args.rpc_url, store.clone())),
        store,
        consumer_name: format!("earn-smart-account:{}", args.cluster),
        watch_set: watch_set_state,
    });
    let event_running = running.clone();
    let event_finished_tx = finished_tx.clone();
    let event_loop_task = tokio::spawn(async move {
        let result = run_event_loop(
            rx,
            target_by_ata,
            observations,
            event_running,
            Some(recheck),
            earn,
        )
        .await;
        let _ = event_finished_tx.send(());
        result
    });
    drop(finished_tx);
    Ok(MonitorSession {
        target_atas,
        earn_watch_set: watch_set,
        replay_from_slot,
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

async fn laserstream_current_replay_boundary(
    rpc_url: &str,
    replay_overlap_slots: u64,
) -> Result<u64> {
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    let current_slot = rpc
        .get_slot()
        .context("fetch confirmed RPC slot for Earn watch-set replay checkpoint")?;
    Ok(laserstream_replay_from_slot(
        current_slot,
        replay_overlap_slots,
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
    fn earn_watch_change_replays_once_then_advances_checkpoint() {
        assert!(session_requires_rebuild(false, false, true));
        assert_eq!(
            replay_override_for_watch_set_change(true, true, Some(42)),
            Some(42)
        );

        let advanced = replay_checkpoint_after_session_start(Some(90), Some(42));
        assert_eq!(advanced, Some(90));
        assert_eq!(
            replay_override_for_watch_set_change(true, true, advanced),
            Some(90)
        );
        assert_eq!(
            replay_checkpoint_after_session_start(Some(140), Some(90)),
            Some(140)
        );
    }

    #[test]
    fn failed_session_restarts_from_durable_cursor() {
        assert_eq!(
            replay_override_for_watch_set_change(false, true, Some(42)),
            None
        );
    }

    #[tokio::test]
    async fn failed_session_wakes_supervisor_before_refresh_deadline() {
        let (finished_tx, mut finished_rx) = mpsc::unbounded_channel();
        finished_tx.send(()).unwrap();

        let woke_for_exit = time::timeout(
            Duration::from_millis(100),
            wait_for_refresh_or_session_exit(Duration::from_secs(300), &mut finished_rx),
        )
        .await
        .expect("supervisor stayed asleep after session exit");

        assert!(woke_for_exit);
    }
}
