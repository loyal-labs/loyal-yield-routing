//! Persistent, signerless confirmation for exact fleet route transactions.
//!
//! This worker never rebuilds or re-signs a transaction. It status-checks a
//! fenced batch first, then broadcasts the exact persisted bytes while their
//! blockhash remains valid. Route-specific balance reconciliation deliberately
//! starts after this worker's `reconciliation_pending` handoff.

use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    process::ExitCode,
    str::FromStr,
    sync::Arc,
    time::Duration,
};

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine};
use chrono::{Duration as ChronoDuration, Utc};
use futures_util::StreamExt;
use loyal_observability::{init_from_env, OperationalError};
use loyal_yield_orchestrator::{
    fleet_orchestration::{
        classify_authoritative_signature_status, fleet_worker_role_probe,
        schedule_authoritative_status_poll, AuthoritativeConfirmationDecision,
        AuthoritativePollUrgency, AuthoritativeSignatureStatus, ConfirmationPollTrigger,
        DurablePgWakeupEvent, DurablePgWakeupListener, FleetWorkerRole,
        SignedRouteSubmissionAdvance, SignedRouteSubmissionLease, SignedRouteSubmissionRecord,
        SignedRouteSubmissionState,
    },
    rpc_safety::{redacted_external_error, validate_rpc_endpoint, validate_rpc_genesis_hash},
    NeonSqlClient, NeonSqlConfig,
};
use serde::Serialize;
use serde_json::json;
use sha2::{Digest, Sha256};
use solana_client::{
    nonblocking::{pubsub_client::PubsubClient, rpc_client::RpcClient},
    rpc_config::RpcSignatureSubscribeConfig,
    rpc_request::RpcRequest,
};
use solana_sdk::{
    commitment_config::CommitmentConfig, message::VersionedMessage, signature::Signature,
    transaction::VersionedTransaction,
};
use tokio::{
    sync::{mpsc, oneshot, Mutex, Semaphore},
    task::{JoinHandle, JoinSet},
    time::Instant,
};

const DATABASE_URL_ENV: &str = "NEON_DATABASE_URL";
const RPC_URL_ENV: &str = "SOLANA_RPC_URL";
const WEBSOCKET_URL_ENV: &str = "SOLANA_WS_URL";
const CLUSTER_ENV: &str = "YIELD_ROUTE_CLUSTER";
const FALLBACK_CLUSTER_ENV: &str = "YIELD_ALT_CLUSTER";
const DEFAULT_POLL_INTERVAL_MILLISECONDS: u64 = 1_000;
const DEFAULT_BATCH_SIZE: i64 = 128;
const DEFAULT_LEASE_SECONDS: i64 = 30;
const DEFAULT_BROADCAST_CONCURRENCY: usize = 16;
const MAX_BATCH_SIZE: i64 = 256;
const PUBSUB_CONNECT_TIMEOUT: Duration = Duration::from_secs(3);
const PUBSUB_RECONNECT_BACKOFF: Duration = Duration::from_secs(2);
const PUBSUB_CLEANUP_TIMEOUT: Duration = Duration::from_millis(250);
const PUBSUB_TASK_CLEANUP_TIMEOUT: Duration = Duration::from_millis(300);
const HINT_STATUS_BATCH_COALESCE: Duration = Duration::from_millis(5);

#[derive(Debug, Clone)]
struct Options {
    database_url: String,
    rpc_url: String,
    websocket_url: String,
    cluster: String,
    worker_id: String,
    once: bool,
    poll_interval: Duration,
    batch_size: i64,
    lease_seconds: i64,
    broadcast_concurrency: usize,
}

#[derive(Debug)]
struct PubsubState {
    client: Option<Arc<PubsubClient>>,
    retry_after: Option<Instant>,
}

#[derive(Debug, Clone)]
struct SignatureHintPool {
    websocket_url: Arc<str>,
    state: Arc<Mutex<PubsubState>>,
    permits: Arc<Semaphore>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum SubscriptionWaitResult {
    Hint,
    Deadline,
    Unavailable,
}

#[derive(Debug)]
struct SignatureHintArm {
    broadcast_complete: Option<oneshot::Sender<bool>>,
    cancel: Option<oneshot::Sender<()>>,
    result: oneshot::Receiver<SubscriptionWaitResult>,
    task: JoinHandle<()>,
}

#[derive(Debug)]
struct AuthoritativeStatusRequest {
    signature: Signature,
    response: oneshot::Sender<AuthoritativeStatusReply>,
}

#[derive(Debug)]
struct AuthoritativeStatusReply {
    observation: Result<SignatureObservation, String>,
    rpc_batch_leader: bool,
}

#[derive(Debug, Clone)]
struct AuthoritativeStatusBatcher {
    requests: mpsc::Sender<AuthoritativeStatusRequest>,
}

#[derive(Debug, Clone)]
enum SignatureObservation {
    Confirmed {
        slot: i64,
    },
    Failed {
        slot: i64,
        detail: String,
    },
    Seen {
        slot: i64,
        error_detail: Option<String>,
    },
    Missing,
    Invalid {
        detail: String,
    },
    AlreadyConfirmed,
}

#[derive(Debug)]
enum BroadcastError {
    Ambiguous(String),
}

#[derive(Debug, Default)]
struct ItemOutcome {
    status_seen: usize,
    broadcasts_attempted: usize,
    broadcasts_succeeded: usize,
    ambiguous_sends: usize,
    confirmed: usize,
    reconciliation_pending: usize,
    expired: usize,
    failed: usize,
    deferred: usize,
    subscription_hints: usize,
    subscription_fallbacks: usize,
    subscription_unavailable: usize,
    authoritative_hint_polls: usize,
    authoritative_hint_poll_errors: usize,
    authoritative_hint_rpc_batches: usize,
}

impl ItemOutcome {
    fn merge(&mut self, other: Self) {
        self.status_seen += other.status_seen;
        self.broadcasts_attempted += other.broadcasts_attempted;
        self.broadcasts_succeeded += other.broadcasts_succeeded;
        self.ambiguous_sends += other.ambiguous_sends;
        self.confirmed += other.confirmed;
        self.reconciliation_pending += other.reconciliation_pending;
        self.expired += other.expired;
        self.failed += other.failed;
        self.deferred += other.deferred;
        self.subscription_hints += other.subscription_hints;
        self.subscription_fallbacks += other.subscription_fallbacks;
        self.subscription_unavailable += other.subscription_unavailable;
        self.authoritative_hint_polls += other.authoritative_hint_polls;
        self.authoritative_hint_poll_errors += other.authoritative_hint_poll_errors;
        self.authoritative_hint_rpc_batches += other.authoritative_hint_rpc_batches;
    }
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct PollHealth {
    event: &'static str,
    cluster: String,
    worker_id: String,
    claimed: usize,
    status_polled: usize,
    status_seen: usize,
    broadcasts_attempted: usize,
    broadcasts_succeeded: usize,
    ambiguous_sends: usize,
    confirmed: usize,
    reconciliation_pending: usize,
    expired: usize,
    failed: usize,
    deferred: usize,
    item_errors: usize,
    first_item_error: Option<String>,
    current_finalized_block_height: Option<i64>,
    current_finalized_slot: Option<i64>,
    elapsed_milliseconds: u128,
    signer_loaded: bool,
    transaction_bytes_rebuilt: bool,
    wakeup_listener_connected: bool,
    durable_recovery_poll_milliseconds: u128,
    signature_subscription_connected: bool,
    subscription_hints: usize,
    subscription_fallbacks: usize,
    subscription_unavailable: usize,
    authoritative_hint_polls: usize,
    authoritative_hint_poll_errors: usize,
    authoritative_hint_rpc_batches: usize,
}

impl AuthoritativeStatusBatcher {
    fn new(rpc: Arc<RpcClient>, max_batch_size: usize) -> Self {
        let (requests, mut receiver) = mpsc::channel::<AuthoritativeStatusRequest>(max_batch_size);
        tokio::spawn(async move {
            while let Some(first) = receiver.recv().await {
                let mut batch = Vec::with_capacity(max_batch_size);
                batch.push(first);
                let coalesce_deadline = Instant::now() + HINT_STATUS_BATCH_COALESCE;
                while batch.len() < max_batch_size {
                    match tokio::time::timeout_at(coalesce_deadline, receiver.recv()).await {
                        Ok(Some(request)) => batch.push(request),
                        Ok(None) | Err(_) => break,
                    }
                }

                let signatures = batch
                    .iter()
                    .map(|request| request.signature)
                    .collect::<Vec<_>>();
                let statuses = rpc.get_signature_statuses_with_history(&signatures).await;
                let statuses = match statuses {
                    Ok(statuses) if statuses.value.len() == batch.len() => statuses.value,
                    Ok(_) => {
                        for (index, request) in batch.into_iter().enumerate() {
                            let _ = request.response.send(AuthoritativeStatusReply {
                                observation: Err(
                                    "authoritative_subscription_hint_poll_length_mismatch"
                                        .to_owned(),
                                ),
                                rpc_batch_leader: index == 0,
                            });
                        }
                        continue;
                    }
                    Err(error) => {
                        let detail = safe_detail(&format!(
                            "authoritative_subscription_hint_poll_failed:{error}"
                        ));
                        for (index, request) in batch.into_iter().enumerate() {
                            let _ = request.response.send(AuthoritativeStatusReply {
                                observation: Err(detail.clone()),
                                rpc_batch_leader: index == 0,
                            });
                        }
                        continue;
                    }
                };

                for (index, (request, status)) in batch.into_iter().zip(statuses).enumerate() {
                    let observation = match status {
                        None => Ok(SignatureObservation::Missing),
                        Some(status) => match i64::try_from(status.slot) {
                            Err(_) => {
                                Err("authoritative_subscription_hint_slot_overflow".to_owned())
                            }
                            Ok(slot) => {
                                let error_detail = status.err.as_ref().map(|error| {
                                    safe_detail(&format!("transaction_error:{error:?}"))
                                });
                                match classify_authoritative_signature_status(
                                    AuthoritativeSignatureStatus {
                                        slot: Some(slot),
                                        satisfies_confirmed_commitment: status
                                            .satisfies_commitment(CommitmentConfig::confirmed()),
                                        transaction_error: status.err.is_some(),
                                    },
                                ) {
                                    AuthoritativeConfirmationDecision::Confirmed { slot } => {
                                        Ok(SignatureObservation::Confirmed { slot })
                                    }
                                    AuthoritativeConfirmationDecision::Failed { .. } => {
                                        Ok(SignatureObservation::Failed {
                                            slot,
                                            detail: error_detail.unwrap_or_else(|| {
                                                "authoritative_transaction_failed".to_owned()
                                            }),
                                        })
                                    }
                                    AuthoritativeConfirmationDecision::Pending => {
                                        Ok(SignatureObservation::Seen {
                                            slot,
                                            error_detail: error_detail.map(|detail| {
                                                safe_detail(&format!("unconfirmed_{detail}"))
                                            }),
                                        })
                                    }
                                    AuthoritativeConfirmationDecision::InvalidSlot => {
                                        Err("invalid_authoritative_subscription_hint_slot"
                                            .to_owned())
                                    }
                                }
                            }
                        },
                    };
                    let _ = request.response.send(AuthoritativeStatusReply {
                        observation,
                        rpc_batch_leader: index == 0,
                    });
                }
            }
        });
        Self { requests }
    }

    async fn observe(&self, signature: Signature) -> Result<AuthoritativeStatusReply, String> {
        let (response, receiver) = oneshot::channel();
        self.requests
            .send(AuthoritativeStatusRequest {
                signature,
                response,
            })
            .await
            .map_err(|_| "authoritative_subscription_hint_batcher_unavailable".to_owned())?;
        receiver
            .await
            .map_err(|_| "authoritative_subscription_hint_batcher_stopped".to_owned())
    }
}

impl SignatureHintPool {
    fn new(websocket_url: String, max_active_subscriptions: usize) -> Self {
        Self {
            websocket_url: Arc::from(websocket_url),
            state: Arc::new(Mutex::new(PubsubState {
                client: None,
                retry_after: None,
            })),
            permits: Arc::new(Semaphore::new(max_active_subscriptions)),
        }
    }

    async fn ensure_connected(&self) -> Result<Arc<PubsubClient>, ()> {
        let mut state = self.state.lock().await;
        if let Some(client) = state.client.as_ref() {
            return Ok(Arc::clone(client));
        }
        if state
            .retry_after
            .is_some_and(|retry_after| retry_after > Instant::now())
        {
            return Err(());
        }
        match tokio::time::timeout(
            PUBSUB_CONNECT_TIMEOUT,
            PubsubClient::new(&self.websocket_url),
        )
        .await
        {
            Ok(Ok(client)) => {
                let client = Arc::new(client);
                state.client = Some(Arc::clone(&client));
                state.retry_after = None;
                Ok(client)
            }
            Ok(Err(_)) | Err(_) => {
                state.retry_after = Some(Instant::now() + PUBSUB_RECONNECT_BACKOFF);
                Err(())
            }
        }
    }

    async fn connected(&self) -> bool {
        self.state.lock().await.client.is_some()
    }

    async fn invalidate(&self, failed_client: &Arc<PubsubClient>) {
        let mut state = self.state.lock().await;
        if state
            .client
            .as_ref()
            .is_some_and(|active| Arc::ptr_eq(active, failed_client))
        {
            state.client = None;
            state.retry_after = Some(Instant::now() + PUBSUB_RECONNECT_BACKOFF);
        }
    }

    async fn arm(&self, signature: Signature) -> Option<SignatureHintArm> {
        let permit = Arc::clone(&self.permits).try_acquire_owned().ok()?;
        let client = self.ensure_connected().await.ok()?;
        let (ready_sender, ready_receiver) = oneshot::channel();
        let (broadcast_complete, broadcast_ready) = oneshot::channel();
        let (cancel, cancel_receiver) = oneshot::channel();
        let (result_sender, result) = oneshot::channel();
        let pool = self.clone();
        let task = tokio::spawn(async move {
            let _permit = permit;
            let config = RpcSignatureSubscribeConfig {
                commitment: Some(CommitmentConfig::confirmed()),
                enable_received_notification: Some(false),
            };
            let subscription = client.signature_subscribe(&signature, Some(config)).await;
            let (mut notifications, unsubscribe) = match subscription {
                Ok(subscription) => subscription,
                Err(_) => {
                    pool.invalidate(&client).await;
                    let _ = ready_sender.send(false);
                    return;
                }
            };
            if ready_sender.send(true).is_err() {
                let _ = tokio::time::timeout(PUBSUB_CLEANUP_TIMEOUT, unsubscribe()).await;
                return;
            }
            if !broadcast_ready.await.unwrap_or(false) {
                let _ = tokio::time::timeout(PUBSUB_CLEANUP_TIMEOUT, unsubscribe()).await;
                return;
            }
            tokio::select! {
                notification = notifications.next() => {
                    match notification {
                        Some(_) => {
                            let _ = result_sender.send(SubscriptionWaitResult::Hint);
                        }
                        None => {
                            pool.invalidate(&client).await;
                            let _ = result_sender.send(SubscriptionWaitResult::Unavailable);
                        }
                    }
                }
                _ = cancel_receiver => {}
            }
            drop(notifications);
            let _ = tokio::time::timeout(PUBSUB_CLEANUP_TIMEOUT, unsubscribe()).await;
        });

        match tokio::time::timeout(PUBSUB_CONNECT_TIMEOUT, ready_receiver).await {
            Ok(Ok(true)) => Some(SignatureHintArm {
                broadcast_complete: Some(broadcast_complete),
                cancel: Some(cancel),
                result,
                task,
            }),
            _ => {
                drop(broadcast_complete);
                drop(cancel);
                task.abort();
                None
            }
        }
    }
}

impl SignatureHintArm {
    fn broadcast_finished(&mut self, possibly_landed: bool) {
        if let Some(sender) = self.broadcast_complete.take() {
            let _ = sender.send(possibly_landed);
        }
    }

    async fn wait_until(&mut self, deadline: Instant) -> SubscriptionWaitResult {
        match tokio::time::timeout_at(deadline, &mut self.result).await {
            Ok(result) => {
                let result = result.unwrap_or(SubscriptionWaitResult::Unavailable);
                if tokio::time::timeout(PUBSUB_TASK_CLEANUP_TIMEOUT, &mut self.task)
                    .await
                    .is_err()
                {
                    self.task.abort();
                }
                self.cancel.take();
                result
            }
            Err(_) => {
                if let Some(cancel) = self.cancel.take() {
                    let _ = cancel.send(());
                }
                if tokio::time::timeout(PUBSUB_TASK_CLEANUP_TIMEOUT, &mut self.task)
                    .await
                    .is_err()
                {
                    self.task.abort();
                }
                SubscriptionWaitResult::Deadline
            }
        }
    }
}

impl Drop for SignatureHintArm {
    fn drop(&mut self) {
        if let Some(cancel) = self.cancel.take() {
            let _ = cancel.send(());
        }
        self.task.abort();
    }
}

#[tokio::main]
async fn main() -> ExitCode {
    if env::args().skip(1).eq(["--role-probe"]) {
        println!("{}", fleet_worker_role_probe(FleetWorkerRole::Confirmer));
        return ExitCode::SUCCESS;
    }
    let _observability = match init_from_env("loyal-fleet-route-confirmer") {
        Ok(observability) => observability,
        Err(error) => {
            eprintln!("failed to initialize observability: {error}");
            return ExitCode::FAILURE;
        }
    };
    match run().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            OperationalError::new(
                "fleet_route_confirmer_fatal",
                "run_fleet_route_confirmer",
                "Fleet route confirmer stopped after a fatal error",
            )
            .retryable(true)
            .recovery_required(true)
            .emit();
            eprintln!(
                "{}",
                json!({
                    "event": "fleet_route_confirmer_fatal",
                    "error": safe_detail(&error.to_string()),
                    "signerLoaded": false,
                })
            );
            ExitCode::FAILURE
        }
    }
}

async fn run() -> Result<(), Box<dyn Error>> {
    let options = parse_options()?;
    validate_rpc_endpoint(&options.rpc_url)
        .map_err(|error| format!("invalid fleet route RPC endpoint: {error}"))?;
    let rpc = Arc::new(RpcClient::new_with_commitment(
        options.rpc_url.clone(),
        CommitmentConfig::confirmed(),
    ));
    let genesis_hash = rpc
        .get_genesis_hash()
        .await
        .map_err(|error| format!("failed to read route confirmer RPC genesis: {error}"))?;
    validate_rpc_genesis_hash(&options.cluster, genesis_hash)
        .map_err(|error| format!("refusing mismatched route confirmer RPC: {error}"))?;
    let max_status_batch_size = usize::try_from(options.batch_size)?;
    let signature_hints = Arc::new(SignatureHintPool::new(
        options.websocket_url.clone(),
        max_status_batch_size,
    ));
    let authoritative_status_batcher = Arc::new(AuthoritativeStatusBatcher::new(
        Arc::clone(&rpc),
        max_status_batch_size,
    ));
    if signature_hints.ensure_connected().await.is_err() {
        eprintln!(
            "{}",
            json!({
                "event": "fleet_route_confirmer_subscription_unavailable",
                "authoritativeBatchedPollingActive": true,
                "retryBackoffMilliseconds": PUBSUB_RECONNECT_BACKOFF.as_millis(),
            })
        );
    }

    let neon = NeonSqlClient::connect(
        NeonSqlConfig::new(options.database_url.clone()).with_max_connections(32),
    )
    .await?;
    neon.require_schema_migration(24, "fleet_route_confirmer")
        .await?;
    neon.require_schema_migration(25, "fee_only_route_payer_shards")
        .await?;
    neon.require_schema_migration(26, "target_capacity_reservations")
        .await?;
    neon.require_schema_migration(27, "rebalance_opportunity_attempt_generations")
        .await?;
    neon.require_schema_migration(29, "fleet_commit_lifetime_fences")
        .await?;
    neon.require_schema_migration(30, "fused_queue_accrual_binding")
        .await?;
    let mut wakeup_listener =
        DurablePgWakeupListener::new("loyal_yield_route_confirmation_wakeup")?;
    let broadcast_limit = Arc::new(Semaphore::new(options.broadcast_concurrency));
    let mut poll_error_reported = false;
    let mut item_errors_reported = false;

    loop {
        let started = Instant::now();
        let mut claimed = 0usize;
        match run_poll(
            &neon,
            Arc::clone(&rpc),
            &options,
            Arc::clone(&broadcast_limit),
            Arc::clone(&signature_hints),
            Arc::clone(&authoritative_status_batcher),
        )
        .await
        {
            Ok(mut health) => {
                poll_error_reported = false;
                if health.item_errors > 0 {
                    if !item_errors_reported {
                        OperationalError::new(
                            "fleet_route_confirmer_items_deferred_after_error",
                            "confirm_signed_route_submissions",
                            "Fleet route confirmer deferred submissions after item failures",
                        )
                        .retryable(true)
                        .recovery_required(true)
                        .emit();
                        item_errors_reported = true;
                    }
                } else {
                    item_errors_reported = false;
                }
                health.elapsed_milliseconds = started.elapsed().as_millis();
                claimed = health.claimed;
                health.wakeup_listener_connected = wakeup_listener.is_connected();
                println!("{}", serde_json::to_string(&health)?);
            }
            Err(error) => {
                if !options.once && !poll_error_reported {
                    OperationalError::new(
                        "fleet_route_confirmer_poll_failed",
                        "poll_signed_route_submissions",
                        "Fleet route confirmer poll failed",
                    )
                    .retryable(true)
                    .recovery_required(true)
                    .emit();
                    poll_error_reported = true;
                }
                println!(
                    "{}",
                    json!({
                        "event": "fleet_route_confirmer_poll_error",
                        "cluster": options.cluster,
                        "workerId": options.worker_id,
                        "error": safe_detail(&error.to_string()),
                        "elapsedMilliseconds": started.elapsed().as_millis(),
                        "signerLoaded": false,
                        "transactionBytesRebuilt": false,
                    })
                );
                if options.once {
                    return Err(error);
                }
            }
        }
        if options.once {
            return Ok(());
        }
        if claimed == 0 {
            wait_for_confirmation_wakeup(&mut wakeup_listener, &neon, options.poll_interval).await;
        }
    }
}

async fn wait_for_confirmation_wakeup(
    listener: &mut DurablePgWakeupListener,
    neon: &NeonSqlClient,
    recovery_poll: Duration,
) {
    match listener.wait(neon.pool(), recovery_poll).await {
        DurablePgWakeupEvent::Notification | DurablePgWakeupEvent::RecoveryPollElapsed => {}
        DurablePgWakeupEvent::Reconnected => {
            eprintln!(
                "{}",
                json!({
                    "event": "fleet_route_confirmer_listener_reconnected",
                    "immediateDurableScan": true,
                })
            );
        }
        DurablePgWakeupEvent::Disconnected { error, retry_after } => {
            eprintln!(
                "{}",
                json!({
                    "event": "fleet_route_confirmer_listener_disconnected",
                    "error": safe_detail(&error),
                    "durablePollingActive": true,
                    "immediateDurableScan": true,
                    "retryBackoffMilliseconds": retry_after.as_millis(),
                })
            );
        }
        DurablePgWakeupEvent::ReconnectFailed { error, retry_after } => {
            eprintln!(
                "{}",
                json!({
                    "event": "fleet_route_confirmer_listener_reconnect_failed",
                    "error": safe_detail(&error),
                    "durablePollingActive": true,
                    "immediateDurableScan": true,
                    "retryBackoffMilliseconds": retry_after.as_millis(),
                })
            );
        }
    }
}

async fn run_poll(
    neon: &NeonSqlClient,
    rpc: Arc<RpcClient>,
    options: &Options,
    broadcast_limit: Arc<Semaphore>,
    signature_hints: Arc<SignatureHintPool>,
    authoritative_status_batcher: Arc<AuthoritativeStatusBatcher>,
) -> Result<PollHealth, Box<dyn Error>> {
    let lease_expires_at = Utc::now() + ChronoDuration::seconds(options.lease_seconds);
    let leases = neon
        .lease_pending_signed_route_submissions(
            &options.cluster,
            &options.worker_id,
            options.batch_size,
            lease_expires_at,
        )
        .await?;
    let recovery_leases = neon
        .lease_unprotected_unbroadcast_signed_route_submissions(
            &options.cluster,
            &options.worker_id,
            options.batch_size,
            lease_expires_at,
        )
        .await?;
    let recovery_ids = recovery_leases
        .iter()
        .map(|lease| lease.submission.id)
        .collect::<BTreeSet<_>>();
    let claimed = leases.len().saturating_add(recovery_leases.len());
    if claimed == 0 {
        return Ok(empty_health(options, signature_hints.connected().await));
    }

    let mut pre_task_item_errors = 0usize;
    let mut pre_task_deferred = 0usize;
    let mut first_pre_task_error = None::<String>;

    let mut work = Vec::with_capacity(claimed);
    let mut status_leases = Vec::new();
    let mut signatures = Vec::new();
    for lease in leases.into_iter().chain(recovery_leases) {
        if lease.submission.state == SignedRouteSubmissionState::Confirmed {
            work.push((lease, SignatureObservation::AlreadyConfirmed));
            continue;
        }
        match Signature::from_str(&lease.submission.transaction_signature) {
            Ok(signature) => {
                status_leases.push(lease);
                signatures.push(signature);
            }
            Err(_) => work.push((
                lease,
                SignatureObservation::Invalid {
                    detail: "invalid_persisted_transaction_signature".to_owned(),
                },
            )),
        }
    }

    let mut current_height = None;
    let mut current_finalized_slot = None;
    let durable_status_poll_count = signatures.len();
    if !signatures.is_empty() {
        debug_assert_eq!(
            schedule_authoritative_status_poll(ConfirmationPollTrigger::DurableRecoveryDeadline)
                .urgency,
            AuthoritativePollUrgency::Scheduled
        );
        match rpc.get_signature_statuses_with_history(&signatures).await {
            Ok(statuses) if statuses.value.len() == status_leases.len() => {
                for (lease, status) in status_leases.into_iter().zip(statuses.value) {
                    let observation = match status {
                        Some(status) => {
                            let slot = match i64::try_from(status.slot) {
                                Ok(slot) => slot,
                                Err(_) => {
                                    work.push((
                                        lease,
                                        SignatureObservation::Invalid {
                                            detail: "authoritative_signature_status_slot_overflow"
                                                .to_owned(),
                                        },
                                    ));
                                    continue;
                                }
                            };
                            let error_detail = status
                                .err
                                .as_ref()
                                .map(|error| safe_detail(&format!("transaction_error:{error:?}")));
                            match classify_authoritative_signature_status(
                                AuthoritativeSignatureStatus {
                                    slot: Some(slot),
                                    satisfies_confirmed_commitment: status
                                        .satisfies_commitment(CommitmentConfig::confirmed()),
                                    transaction_error: status.err.is_some(),
                                },
                            ) {
                                AuthoritativeConfirmationDecision::Confirmed { slot } => {
                                    SignatureObservation::Confirmed { slot }
                                }
                                AuthoritativeConfirmationDecision::Failed { .. } => {
                                    SignatureObservation::Failed {
                                        slot,
                                        detail: error_detail.unwrap_or_else(|| {
                                            "authoritative_transaction_failed".to_owned()
                                        }),
                                    }
                                }
                                AuthoritativeConfirmationDecision::Pending => {
                                    SignatureObservation::Seen {
                                        slot,
                                        error_detail: error_detail.map(|detail| {
                                            safe_detail(&format!("unconfirmed_{detail}"))
                                        }),
                                    }
                                }
                                AuthoritativeConfirmationDecision::InvalidSlot => {
                                    SignatureObservation::Invalid {
                                        detail: "invalid_authoritative_signature_status_slot"
                                            .to_owned(),
                                    }
                                }
                            }
                        }
                        None => SignatureObservation::Missing,
                    };
                    work.push((lease, observation));
                }
            }
            Ok(_) => {
                let detail = "signature status batch length did not match the durable claim";
                defer_claims_after_error(neon, &status_leases, options.poll_interval, detail)
                    .await?;
                pre_task_item_errors = pre_task_item_errors.saturating_add(status_leases.len());
                pre_task_deferred = pre_task_deferred.saturating_add(status_leases.len());
                first_pre_task_error.get_or_insert_with(|| detail.to_owned());
            }
            Err(error) => {
                let detail = safe_detail(&format!("authoritative_status_poll_failed:{error}"));
                defer_claims_after_error(neon, &status_leases, options.poll_interval, &detail)
                    .await?;
                pre_task_item_errors = pre_task_item_errors.saturating_add(status_leases.len());
                pre_task_deferred = pre_task_deferred.saturating_add(status_leases.len());
                first_pre_task_error.get_or_insert(detail);
            }
        }
    }

    // Finalized height is auxiliary expiry evidence. A failure must not erase
    // authoritative status results for confirmed/failed/seen signatures.
    let missing_ids = work
        .iter()
        .filter_map(|(lease, observation)| {
            matches!(observation, SignatureObservation::Missing).then_some(lease.submission.id)
        })
        .collect::<BTreeSet<_>>();
    if !missing_ids.is_empty() {
        match rpc
            .get_block_height_with_commitment(CommitmentConfig::finalized())
            .await
            .and_then(|height| {
                i64::try_from(height).map_err(|error| {
                    solana_client::client_error::ClientError::from(std::io::Error::new(
                        std::io::ErrorKind::InvalidData,
                        error,
                    ))
                })
            }) {
            Ok(height) => current_height = Some(height),
            Err(error) => {
                let deferred = take_work_leases(&mut work, &missing_ids);
                let detail = safe_detail(&format!("finalized_block_height_failed:{error}"));
                defer_claims_after_error(neon, &deferred, options.poll_interval, &detail).await?;
                pre_task_item_errors = pre_task_item_errors.saturating_add(deferred.len());
                pre_task_deferred = pre_task_deferred.saturating_add(deferred.len());
                first_pre_task_error.get_or_insert(detail);
            }
        }
    }

    let expired_attempted_ids = current_height.map_or_else(BTreeSet::new, |height| {
        work.iter()
            .filter_map(|(lease, observation)| {
                (matches!(observation, SignatureObservation::Missing)
                    && lease.submission.broadcast_count > 0
                    && height > lease.submission.last_valid_block_height)
                    .then_some(lease.submission.id)
            })
            .collect()
    });
    if !expired_attempted_ids.is_empty() {
        match rpc
            .get_slot_with_commitment(CommitmentConfig::finalized())
            .await
            .and_then(|slot| {
                i64::try_from(slot).map_err(|error| {
                    solana_client::client_error::ClientError::from(std::io::Error::new(
                        std::io::ErrorKind::InvalidData,
                        error,
                    ))
                })
            }) {
            Ok(slot) => current_finalized_slot = Some(slot),
            Err(error) => {
                let deferred = take_work_leases(&mut work, &expired_attempted_ids);
                let detail = safe_detail(&format!("finalized_effect_slot_failed:{error}"));
                defer_claims_after_error(neon, &deferred, options.poll_interval, &detail).await?;
                pre_task_item_errors = pre_task_item_errors.saturating_add(deferred.len());
                pre_task_deferred = pre_task_deferred.saturating_add(deferred.len());
                first_pre_task_error.get_or_insert(detail);
            }
        }
    }

    // Invalid or expired ALT protection is terminal-only. Until its blockhash
    // expires, keep the exact signed bytes fenced without preparing a send.
    let recovery_wait_ids = work
        .iter()
        .filter_map(|(lease, observation)| {
            (recovery_ids.contains(&lease.submission.id)
                && matches!(observation, SignatureObservation::Missing)
                && current_height
                    .is_none_or(|height| height <= lease.submission.last_valid_block_height))
            .then_some(lease.submission.id)
        })
        .collect::<BTreeSet<_>>();
    if !recovery_wait_ids.is_empty() {
        let deferred = take_work_leases(&mut work, &recovery_wait_ids);
        let detail = "recovery_only_waiting_for_blockhash_expiry";
        defer_claims_after_error(neon, &deferred, options.poll_interval, detail).await?;
        pre_task_deferred = pre_task_deferred.saturating_add(deferred.len());
    }
    for (lease, observation) in &mut work {
        if recovery_ids.contains(&lease.submission.id) {
            if let SignatureObservation::Seen { error_detail, .. } = observation {
                if error_detail.is_none() {
                    *error_detail = Some("recovery_only_signature_seen_no_rebroadcast".to_owned());
                }
            }
        }
    }

    // Authoritative confirmations need no per-row state replay. Validate every
    // immutable wire image first, then commit decision confirmation evidence and
    // reconciliation handoff in one fenced set-based transaction.
    let mut confirmations = Vec::new();
    let mut confirmation_ids = BTreeSet::new();
    let mut batch_confirmed_count = 0usize;
    let mut invalid_confirmation_leases = Vec::new();
    let mut invalid_confirmation_detail = None;
    for (lease, observation) in &work {
        let slot = match observation {
            SignatureObservation::Confirmed { slot } => Some(*slot),
            SignatureObservation::AlreadyConfirmed => lease.submission.confirmed_slot,
            _ => None,
        };
        if matches!(
            observation,
            SignatureObservation::Confirmed { .. } | SignatureObservation::AlreadyConfirmed
        ) {
            match (slot, prepare_exact_wire(&lease.submission)) {
                (Some(slot), Ok(_)) => {
                    confirmations.push((lease.clone(), slot));
                    confirmation_ids.insert(lease.submission.id);
                    if matches!(observation, SignatureObservation::Confirmed { .. }) {
                        batch_confirmed_count = batch_confirmed_count.saturating_add(1);
                    }
                }
                (None, _) => {
                    invalid_confirmation_leases.push(lease.clone());
                    invalid_confirmation_detail
                        .get_or_insert_with(|| "confirmed_submission_missing_slot".to_owned());
                }
                (_, Err(detail)) => {
                    invalid_confirmation_leases.push(lease.clone());
                    invalid_confirmation_detail.get_or_insert(detail);
                }
            }
        }
    }
    if !invalid_confirmation_leases.is_empty() {
        let invalid_ids = invalid_confirmation_leases
            .iter()
            .map(|lease| lease.submission.id)
            .collect::<BTreeSet<_>>();
        work.retain(|(lease, _)| !invalid_ids.contains(&lease.submission.id));
        let detail = safe_detail(
            invalid_confirmation_detail
                .as_deref()
                .unwrap_or("invalid_confirmed_submission_evidence"),
        );
        defer_claims_after_error(
            neon,
            &invalid_confirmation_leases,
            options.poll_interval,
            &detail,
        )
        .await?;
        pre_task_item_errors =
            pre_task_item_errors.saturating_add(invalid_confirmation_leases.len());
        pre_task_deferred = pre_task_deferred.saturating_add(invalid_confirmation_leases.len());
        first_pre_task_error.get_or_insert(detail);
    }
    let mut outcome = ItemOutcome::default();
    if !confirmations.is_empty() {
        match neon
            .confirm_signed_route_submission_batch(&confirmations, Utc::now())
            .await
        {
            Ok(_) => {
                outcome.status_seen = outcome.status_seen.saturating_add(batch_confirmed_count);
                outcome.confirmed = outcome.confirmed.saturating_add(batch_confirmed_count);
                outcome.reconciliation_pending = outcome
                    .reconciliation_pending
                    .saturating_add(confirmations.len());
            }
            Err(error) => {
                let leases = confirmations
                    .iter()
                    .map(|(lease, _)| lease.clone())
                    .collect::<Vec<_>>();
                let detail = safe_detail(&format!("confirmation_batch_commit_failed:{error}"));
                defer_claims_after_error(neon, &leases, options.poll_interval, &detail).await?;
                pre_task_item_errors = pre_task_item_errors.saturating_add(leases.len());
                pre_task_deferred = pre_task_deferred.saturating_add(leases.len());
                first_pre_task_error.get_or_insert(detail);
            }
        }
        work.retain(|(lease, _)| !confirmation_ids.contains(&lease.submission.id));
    }

    // Batch the durable pre-broadcast boundary only for rows that will actually
    // send. Expired or status-error rows keep their existing non-broadcast path.
    let broadcast_ids = work
        .iter()
        .filter_map(|(lease, observation)| {
            (!recovery_ids.contains(&lease.submission.id)
                && observation_will_broadcast(observation, current_height, &lease.submission))
            .then_some(lease.submission.id)
        })
        .collect::<BTreeSet<_>>();
    let mut encoded_wire_by_id = BTreeMap::new();
    let mut broadcast_leases = Vec::new();
    let mut invalid_broadcast_leases = Vec::new();
    let mut invalid_broadcast_detail = None;
    for (lease, _) in &work {
        if !broadcast_ids.contains(&lease.submission.id) {
            continue;
        }
        match prepare_exact_wire(&lease.submission) {
            Ok(encoded) => {
                encoded_wire_by_id.insert(lease.submission.id, encoded);
                broadcast_leases.push(lease.clone());
            }
            Err(detail) => {
                invalid_broadcast_detail.get_or_insert(detail);
                invalid_broadcast_leases.push(lease.clone());
            }
        }
    }
    if !invalid_broadcast_leases.is_empty() {
        let invalid_ids = invalid_broadcast_leases
            .iter()
            .map(|lease| lease.submission.id)
            .collect::<BTreeSet<_>>();
        work.retain(|(lease, _)| !invalid_ids.contains(&lease.submission.id));
        let detail = safe_detail(
            invalid_broadcast_detail
                .as_deref()
                .unwrap_or("invalid_broadcast_wire_evidence"),
        );
        defer_claims_after_error(
            neon,
            &invalid_broadcast_leases,
            options.poll_interval,
            &detail,
        )
        .await?;
        pre_task_item_errors = pre_task_item_errors.saturating_add(invalid_broadcast_leases.len());
        pre_task_deferred = pre_task_deferred.saturating_add(invalid_broadcast_leases.len());
        first_pre_task_error.get_or_insert(detail);
        broadcast_leases.retain(|lease| !invalid_ids.contains(&lease.submission.id));
    }
    if !broadcast_leases.is_empty() {
        match neon
            .prepare_signed_route_broadcast_batch(&broadcast_leases, Utc::now())
            .await
        {
            Ok(prepared) => {
                let mut prepared_by_id = prepared
                    .into_iter()
                    .map(|lease| (lease.submission.id, lease))
                    .collect::<BTreeMap<_, _>>();
                for (lease, _) in &mut work {
                    if let Some(prepared) = prepared_by_id.remove(&lease.submission.id) {
                        *lease = prepared;
                    }
                }
            }
            Err(error) => {
                let failed_ids = broadcast_leases
                    .iter()
                    .map(|lease| lease.submission.id)
                    .collect::<BTreeSet<_>>();
                let detail = safe_detail(&format!("broadcast_batch_prepare_failed:{error}"));
                defer_claims_after_error(neon, &broadcast_leases, options.poll_interval, &detail)
                    .await?;
                work.retain(|(lease, _)| !failed_ids.contains(&lease.submission.id));
                pre_task_item_errors = pre_task_item_errors.saturating_add(broadcast_leases.len());
                pre_task_deferred = pre_task_deferred.saturating_add(broadcast_leases.len());
                first_pre_task_error.get_or_insert(detail);
            }
        }
    }

    let mut tasks = JoinSet::new();
    let task_leases = work
        .iter()
        .map(|(lease, _)| lease.clone())
        .collect::<Vec<_>>();
    for (lease, observation) in work {
        let neon = neon.clone();
        let rpc = Arc::clone(&rpc);
        let broadcast_limit = Arc::clone(&broadcast_limit);
        let signature_hints = Arc::clone(&signature_hints);
        let authoritative_status_batcher = Arc::clone(&authoritative_status_batcher);
        let poll_interval = options.poll_interval;
        let encoded_wire = encoded_wire_by_id.remove(&lease.submission.id);
        tasks.spawn(async move {
            process_submission(
                &neon,
                rpc,
                lease,
                observation,
                current_height,
                current_finalized_slot,
                poll_interval,
                broadcast_limit,
                signature_hints,
                authoritative_status_batcher,
                encoded_wire,
            )
            .await
        });
    }

    let mut item_errors = pre_task_item_errors;
    let mut first_item_error = first_pre_task_error;
    outcome.deferred = outcome.deferred.saturating_add(pre_task_deferred);
    while let Some(result) = tasks.join_next().await {
        match result {
            Ok(Ok(item)) => outcome.merge(item),
            Ok(Err(error)) => {
                item_errors += 1;
                first_item_error.get_or_insert_with(|| safe_detail(&error.to_string()));
            }
            Err(error) => {
                item_errors += 1;
                first_item_error.get_or_insert_with(|| safe_detail(&error.to_string()));
            }
        }
    }
    if item_errors > pre_task_item_errors {
        let detail = first_item_error
            .clone()
            .unwrap_or_else(|| "confirmation_task_failed".to_owned());
        defer_claims_after_error(neon, &task_leases, options.poll_interval, &detail).await?;
    }
    Ok(PollHealth {
        event: "fleet_route_confirmer_poll",
        cluster: options.cluster.clone(),
        worker_id: options.worker_id.clone(),
        claimed,
        status_polled: durable_status_poll_count + outcome.authoritative_hint_polls,
        status_seen: outcome.status_seen,
        broadcasts_attempted: outcome.broadcasts_attempted,
        broadcasts_succeeded: outcome.broadcasts_succeeded,
        ambiguous_sends: outcome.ambiguous_sends,
        confirmed: outcome.confirmed,
        reconciliation_pending: outcome.reconciliation_pending,
        expired: outcome.expired,
        failed: outcome.failed,
        deferred: outcome.deferred,
        item_errors,
        first_item_error,
        current_finalized_block_height: current_height,
        current_finalized_slot,
        elapsed_milliseconds: 0,
        signer_loaded: false,
        transaction_bytes_rebuilt: false,
        wakeup_listener_connected: false,
        durable_recovery_poll_milliseconds: options.poll_interval.as_millis(),
        signature_subscription_connected: signature_hints.connected().await,
        subscription_hints: outcome.subscription_hints,
        subscription_fallbacks: outcome.subscription_fallbacks,
        subscription_unavailable: outcome.subscription_unavailable,
        authoritative_hint_polls: outcome.authoritative_hint_polls,
        authoritative_hint_poll_errors: outcome.authoritative_hint_poll_errors,
        authoritative_hint_rpc_batches: outcome.authoritative_hint_rpc_batches,
    })
}

fn observation_will_broadcast(
    observation: &SignatureObservation,
    current_height: Option<i64>,
    submission: &SignedRouteSubmissionRecord,
) -> bool {
    let blockhash_expired =
        current_height.is_some_and(|height| height > submission.last_valid_block_height);
    !blockhash_expired
        && matches!(
            observation,
            SignatureObservation::Missing
                | SignatureObservation::Seen {
                    error_detail: None,
                    ..
                }
        )
}

fn take_work_leases(
    work: &mut Vec<(SignedRouteSubmissionLease, SignatureObservation)>,
    ids: &BTreeSet<i64>,
) -> Vec<SignedRouteSubmissionLease> {
    let mut retained = Vec::with_capacity(work.len());
    let mut removed = Vec::new();
    for (lease, observation) in work.drain(..) {
        if ids.contains(&lease.submission.id) {
            removed.push(lease);
        } else {
            retained.push((lease, observation));
        }
    }
    *work = retained;
    removed
}

async fn defer_claims_after_error(
    neon: &NeonSqlClient,
    leases: &[SignedRouteSubmissionLease],
    poll_interval: Duration,
    detail: &str,
) -> Result<u64, Box<dyn Error>> {
    let checked_at = Utc::now();
    let next_poll_at = checked_at + ChronoDuration::from_std(poll_interval)?;
    Ok(neon
        .defer_signed_route_submission_lease_batch(
            leases,
            checked_at,
            next_poll_at,
            &safe_detail(detail),
        )
        .await?)
}

#[allow(clippy::too_many_arguments)]
async fn process_submission(
    neon: &NeonSqlClient,
    rpc: Arc<RpcClient>,
    lease: SignedRouteSubmissionLease,
    observation: SignatureObservation,
    current_height: Option<i64>,
    current_finalized_slot: Option<i64>,
    poll_interval: Duration,
    broadcast_limit: Arc<Semaphore>,
    signature_hints: Arc<SignatureHintPool>,
    authoritative_status_batcher: Arc<AuthoritativeStatusBatcher>,
    prepared_encoded_wire: Option<String>,
) -> Result<ItemOutcome, Box<dyn Error + Send + Sync>> {
    // Bind every status observation and network action to the canonical exact
    // wire evidence first. Corrupt rows remain leased/retryable and visible;
    // they are never interpreted as confirmed or safe-to-replace work.
    let encoded_wire = match prepared_encoded_wire {
        Some(encoded_wire) => encoded_wire,
        None => prepare_exact_wire(&lease.submission)
            .map_err(|detail| format!("signed route wire validation failed: {detail}"))?,
    };
    let checked_at = Utc::now();
    let next_poll_at = checked_at + ChronoDuration::from_std(poll_interval)?;
    let durable_poll_deadline = Instant::now() + poll_interval;
    match observation {
        SignatureObservation::AlreadyConfirmed => {
            let slot = lease
                .submission
                .confirmed_slot
                .ok_or("confirmed submission is missing its confirmed slot")?;
            neon.confirm_signed_route_submission_batch(&[(lease.clone(), slot)], Utc::now())
                .await?;
            Ok(ItemOutcome {
                reconciliation_pending: 1,
                ..ItemOutcome::default()
            })
        }
        SignatureObservation::Confirmed { slot } => {
            neon.confirm_signed_route_submission_batch(&[(lease.clone(), slot)], checked_at)
                .await?;
            Ok(ItemOutcome {
                status_seen: 1,
                confirmed: 1,
                reconciliation_pending: 1,
                ..ItemOutcome::default()
            })
        }
        SignatureObservation::Failed { slot, detail } => {
            neon.advance_signed_route_submission(
                &lease,
                SignedRouteSubmissionAdvance::Failed {
                    checked_at,
                    confirmed_slot: Some(slot),
                    error_detail: detail,
                },
            )
            .await?;
            Ok(ItemOutcome {
                status_seen: 1,
                failed: 1,
                ..ItemOutcome::default()
            })
        }
        SignatureObservation::Invalid { detail } => {
            neon.advance_signed_route_submission(
                &lease,
                SignedRouteSubmissionAdvance::Failed {
                    checked_at,
                    confirmed_slot: None,
                    error_detail: detail,
                },
            )
            .await?;
            Ok(ItemOutcome {
                failed: 1,
                ..ItemOutcome::default()
            })
        }
        SignatureObservation::Missing
            if current_height
                .is_some_and(|height| height > lease.submission.last_valid_block_height) =>
        {
            if lease.submission.broadcast_count == 0 {
                let observed_block_height = current_height.expect("matched Some height");
                neon.advance_signed_route_submission(
                    &lease,
                    SignedRouteSubmissionAdvance::Expired {
                        checked_at,
                        observed_block_height,
                        signature_history_absent: true,
                        effect_absence_proved: false,
                    },
                )
                .await?;
                return Ok(ItemOutcome {
                    expired: 1,
                    ..ItemOutcome::default()
                });
            }
            // An attempted send needs a route-specific finalized effect check
            // before replacement. Hand it to the separate state-read lane so
            // confirmation/broadcast throughput is not occupied by RPC reads.
            let effect_check_slot = current_finalized_slot
                .ok_or("expired attempted route is missing its finalized effect-check slot")?;
            neon.advance_signed_route_submission(
                &lease,
                SignedRouteSubmissionAdvance::ExpiryCheckPending {
                    checked_at,
                    observed_block_height: current_height.expect("matched Some height"),
                    effect_check_slot,
                },
            )
            .await?;
            Ok(ItemOutcome {
                deferred: 1,
                ..ItemOutcome::default()
            })
        }
        SignatureObservation::Seen { slot, .. }
            if current_height
                .is_some_and(|height| height > lease.submission.last_valid_block_height) =>
        {
            ensure_decision_confirming(neon, &lease.submission, Some(slot)).await?;
            neon.advance_signed_route_submission(
                &lease,
                SignedRouteSubmissionAdvance::Submitted {
                    checked_at,
                    observed_slot: Some(slot),
                    next_poll_at,
                    broadcasted: false,
                },
            )
            .await?;
            Ok(ItemOutcome {
                status_seen: 1,
                deferred: 1,
                ..ItemOutcome::default()
            })
        }
        SignatureObservation::Seen {
            slot,
            error_detail: Some(error_detail),
        } => {
            // A processed/fork error is not terminal evidence. Keep the exact
            // signed bytes fenced and wait for confirmed/finalized status
            // rather than releasing locks or producing a replacement.
            ensure_decision_confirming(neon, &lease.submission, Some(slot)).await?;
            neon.advance_signed_route_submission(
                &lease,
                SignedRouteSubmissionAdvance::Deferred {
                    checked_at,
                    next_poll_at,
                    error_detail: Some(error_detail),
                },
            )
            .await?;
            Ok(ItemOutcome {
                status_seen: 1,
                deferred: 1,
                ..ItemOutcome::default()
            })
        }
        SignatureObservation::Missing
        | SignatureObservation::Seen {
            error_detail: None, ..
        } => {
            let mut observed_slot = match observation {
                SignatureObservation::Seen { slot, .. } => Some(slot),
                _ => None,
            };
            let signature = Signature::from_str(&lease.submission.transaction_signature)
                .map_err(|_| "persisted route signature became invalid after batch validation")?;
            let mut hint_arm = signature_hints.arm(signature).await;
            let subscription_was_unavailable = hint_arm.is_none();
            let permit = broadcast_limit.acquire().await?;
            if lease.submission.error_detail.as_deref() != Some("broadcast_intent_persisted")
                || lease.submission.broadcast_count <= 0
            {
                return Err("broadcast attempted without its atomic durable batch intent".into());
            }
            // The batch intent already incremented the counter. A value of one
            // is the first send and retains preflight; larger values are exact
            // byte rebroadcasts and may skip repeated preflight.
            let skip_preflight = lease.submission.broadcast_count > 1;
            let send = broadcast_exact(&rpc, &lease.submission, encoded_wire, skip_preflight).await;
            drop(permit);
            let subscription_result = if let Some(arm) = hint_arm.as_mut() {
                // A transport error is ambiguous, so the exact signature may
                // still land and remains worth observing.
                arm.broadcast_finished(true);
                arm.wait_until(durable_poll_deadline).await
            } else {
                SubscriptionWaitResult::Unavailable
            };

            let mut outcome = ItemOutcome {
                status_seen: usize::from(observed_slot.is_some()),
                broadcasts_attempted: 1,
                broadcasts_succeeded: usize::from(send.is_ok()),
                ambiguous_sends: usize::from(send.is_err()),
                subscription_hints: usize::from(matches!(
                    subscription_result,
                    SubscriptionWaitResult::Hint
                )),
                subscription_fallbacks: usize::from(matches!(
                    subscription_result,
                    SubscriptionWaitResult::Deadline
                )),
                subscription_unavailable: usize::from(subscription_was_unavailable)
                    + usize::from(
                        matches!(subscription_result, SubscriptionWaitResult::Unavailable)
                            && !subscription_was_unavailable,
                    ),
                ..ItemOutcome::default()
            };

            if subscription_result == SubscriptionWaitResult::Hint {
                let directive =
                    schedule_authoritative_status_poll(ConfirmationPollTrigger::SubscriptionHint);
                debug_assert_eq!(directive.urgency, AuthoritativePollUrgency::Immediate);
                outcome.authoritative_hint_polls = 1;
                match authoritative_status_batcher.observe(signature).await {
                    Ok(reply) => {
                        outcome.authoritative_hint_rpc_batches =
                            usize::from(reply.rpc_batch_leader);
                        match reply.observation {
                            Ok(SignatureObservation::Confirmed { slot }) => {
                                neon.confirm_signed_route_submission_batch(
                                    &[(lease.clone(), slot)],
                                    Utc::now(),
                                )
                                .await?;
                                outcome.status_seen += 1;
                                outcome.confirmed = 1;
                                outcome.reconciliation_pending = 1;
                                return Ok(outcome);
                            }
                            Ok(SignatureObservation::Failed { slot, detail }) => {
                                neon.advance_signed_route_submission(
                                    &lease,
                                    SignedRouteSubmissionAdvance::Failed {
                                        checked_at: Utc::now(),
                                        confirmed_slot: Some(slot),
                                        error_detail: detail,
                                    },
                                )
                                .await?;
                                outcome.status_seen += 1;
                                outcome.failed = 1;
                                return Ok(outcome);
                            }
                            Ok(SignatureObservation::Seen { slot, .. }) => {
                                outcome.status_seen += 1;
                                // Preserve any more recent authoritative slot
                                // without treating it as terminal.
                                if observed_slot.is_none_or(|previous| slot > previous) {
                                    observed_slot = Some(slot);
                                }
                            }
                            Ok(SignatureObservation::Missing) => {}
                            Ok(SignatureObservation::Invalid { .. })
                            | Ok(SignatureObservation::AlreadyConfirmed)
                            | Err(_) => {
                                outcome.authoritative_hint_poll_errors = 1;
                            }
                        }
                    }
                    Err(_) => {
                        outcome.authoritative_hint_poll_errors = 1;
                    }
                }
            }

            match send {
                Ok(()) => {
                    neon.advance_signed_route_submission(
                        &lease,
                        SignedRouteSubmissionAdvance::Submitted {
                            checked_at,
                            observed_slot,
                            next_poll_at,
                            broadcasted: false,
                        },
                    )
                    .await?;
                    Ok(outcome)
                }
                Err(BroadcastError::Ambiguous(error)) => {
                    neon.advance_signed_route_submission(
                        &lease,
                        SignedRouteSubmissionAdvance::Deferred {
                            checked_at,
                            next_poll_at,
                            error_detail: Some(safe_detail(&error)),
                        },
                    )
                    .await?;
                    outcome.deferred = 1;
                    Ok(outcome)
                }
            }
        }
    }
}

async fn ensure_decision_confirming(
    neon: &NeonSqlClient,
    submission: &SignedRouteSubmissionRecord,
    _observed_slot: Option<i64>,
) -> Result<(), Box<dyn Error + Send + Sync>> {
    neon.ensure_signed_route_decision_confirming(submission)
        .await?;
    Ok(())
}

fn prepare_exact_wire(submission: &SignedRouteSubmissionRecord) -> Result<String, String> {
    let transaction_hash = format!("{:x}", Sha256::digest(&submission.signed_transaction));
    if !transaction_hash.eq_ignore_ascii_case(&submission.signed_transaction_hash) {
        return Err("persisted_signed_transaction_hash_mismatch".to_owned());
    }
    let transaction: VersionedTransaction = bincode::deserialize(&submission.signed_transaction)
        .map_err(|_| "persisted_signed_transaction_decode_failed".to_owned())?;
    transaction
        .sanitize()
        .map_err(|_| "persisted_signed_transaction_sanitize_failed".to_owned())?;
    transaction
        .verify_and_hash_message()
        .map_err(|_| "persisted_signed_transaction_signature_verification_failed".to_owned())?;
    if !matches!(transaction.message, VersionedMessage::V0(_)) {
        return Err("persisted_route_transaction_is_not_v0".to_owned());
    }
    let reserialized = bincode::serialize(&transaction)
        .map_err(|_| "persisted_signed_transaction_reserialize_failed".to_owned())?;
    if reserialized != submission.signed_transaction {
        return Err("persisted_signed_transaction_is_not_canonical".to_owned());
    }
    let message_hash = format!(
        "{:x}",
        Sha256::digest(
            bincode::serialize(&transaction.message)
                .map_err(|_| "persisted_route_message_serialize_failed".to_owned())?
        )
    );
    if !message_hash.eq_ignore_ascii_case(&submission.message_hash) {
        return Err("persisted_route_message_hash_mismatch".to_owned());
    }
    if transaction
        .signatures
        .first()
        .is_none_or(|signature| signature.to_string() != submission.transaction_signature)
    {
        return Err("persisted_route_signature_mismatch".to_owned());
    }
    if transaction.message.recent_blockhash().to_string() != submission.recent_blockhash {
        return Err("persisted_route_blockhash_mismatch".to_owned());
    }
    if transaction
        .message
        .static_account_keys()
        .first()
        .is_none_or(|fee_payer| fee_payer.to_string() != submission.fee_payer)
    {
        return Err("persisted_route_fee_payer_mismatch".to_owned());
    }
    Ok(BASE64_STANDARD.encode(&submission.signed_transaction))
}

async fn broadcast_exact(
    rpc: &RpcClient,
    submission: &SignedRouteSubmissionRecord,
    encoded_wire: String,
    skip_preflight: bool,
) -> Result<(), BroadcastError> {
    let returned_signature: String = rpc
        .send(
            RpcRequest::SendTransaction,
            json!([
                encoded_wire,
                {
                    "encoding": "base64",
                    "skipPreflight": skip_preflight,
                    "preflightCommitment": "confirmed",
                    "maxRetries": 0,
                }
            ]),
        )
        .await
        .map_err(|error| BroadcastError::Ambiguous(redacted_external_error(&error.to_string())))?;
    if returned_signature != submission.transaction_signature {
        return Err(BroadcastError::Ambiguous(
            "RPC returned a signature different from the persisted transaction".to_owned(),
        ));
    }
    Ok(())
}

fn empty_health(options: &Options, signature_subscription_connected: bool) -> PollHealth {
    PollHealth {
        event: "fleet_route_confirmer_poll",
        cluster: options.cluster.clone(),
        worker_id: options.worker_id.clone(),
        claimed: 0,
        status_polled: 0,
        status_seen: 0,
        broadcasts_attempted: 0,
        broadcasts_succeeded: 0,
        ambiguous_sends: 0,
        confirmed: 0,
        reconciliation_pending: 0,
        expired: 0,
        failed: 0,
        deferred: 0,
        item_errors: 0,
        first_item_error: None,
        current_finalized_block_height: None,
        current_finalized_slot: None,
        elapsed_milliseconds: 0,
        signer_loaded: false,
        transaction_bytes_rebuilt: false,
        wakeup_listener_connected: false,
        durable_recovery_poll_milliseconds: options.poll_interval.as_millis(),
        signature_subscription_connected,
        subscription_hints: 0,
        subscription_fallbacks: 0,
        subscription_unavailable: 0,
        authoritative_hint_polls: 0,
        authoritative_hint_poll_errors: 0,
        authoritative_hint_rpc_batches: 0,
    }
}

fn parse_options() -> Result<Options, Box<dyn Error>> {
    let mut execute = false;
    let mut once = false;
    let mut cluster = None;
    let mut rpc_url = None;
    let mut websocket_url = None;
    let mut worker_id = format!("fleet-route-confirmer-{}", std::process::id());
    let mut poll_interval_milliseconds = DEFAULT_POLL_INTERVAL_MILLISECONDS;
    let mut batch_size = DEFAULT_BATCH_SIZE;
    let mut lease_seconds = DEFAULT_LEASE_SECONDS;
    let mut broadcast_concurrency = DEFAULT_BROADCAST_CONCURRENCY;
    let mut args = env::args().skip(1);
    while let Some(argument) = args.next() {
        match argument.as_str() {
            "--execute" => execute = true,
            "--once" => once = true,
            "--cluster" => cluster = Some(next_argument(&mut args, "--cluster")?),
            "--rpc-url" => rpc_url = Some(next_argument(&mut args, "--rpc-url")?),
            "--ws-url" => websocket_url = Some(next_argument(&mut args, "--ws-url")?),
            "--worker-id" => worker_id = next_argument(&mut args, "--worker-id")?,
            "--poll-interval-milliseconds" => {
                poll_interval_milliseconds =
                    next_argument(&mut args, "--poll-interval-milliseconds")?.parse()?;
            }
            "--batch-size" => {
                batch_size = next_argument(&mut args, "--batch-size")?.parse()?;
            }
            "--lease-seconds" => {
                lease_seconds = next_argument(&mut args, "--lease-seconds")?.parse()?;
            }
            "--broadcast-concurrency" => {
                broadcast_concurrency =
                    next_argument(&mut args, "--broadcast-concurrency")?.parse()?;
            }
            "--help" | "-h" => {
                println!("{}", usage());
                std::process::exit(0);
            }
            other => return Err(format!("unknown argument {other:?}\n{}", usage()).into()),
        }
    }
    if !execute {
        return Err(format!(
            "--execute is required; this explicit gate broadcasts only pre-signed exact bytes\n{}",
            usage()
        )
        .into());
    }
    let database_url = env::var(DATABASE_URL_ENV)
        .map_err(|_| "NEON_DATABASE_URL is required for route confirmation")?;
    let rpc_url = rpc_url
        .or_else(|| env::var(RPC_URL_ENV).ok())
        .filter(|value| !value.trim().is_empty())
        .ok_or("SOLANA_RPC_URL or --rpc-url is required")?;
    let websocket_url = websocket_url
        .or_else(|| env::var(WEBSOCKET_URL_ENV).ok())
        .filter(|value| !value.trim().is_empty())
        .map(Ok)
        .unwrap_or_else(|| websocket_url_for_rpc(&rpc_url))?;
    let cluster = cluster
        .or_else(|| env::var(CLUSTER_ENV).ok())
        .or_else(|| env::var(FALLBACK_CLUSTER_ENV).ok())
        .filter(|value| !value.trim().is_empty())
        .ok_or("--cluster, YIELD_ROUTE_CLUSTER, or YIELD_ALT_CLUSTER is required")?;
    if worker_id.trim().is_empty() || worker_id.len() > 128 {
        return Err("--worker-id must contain 1-128 characters".into());
    }
    if !(100..=60_000).contains(&poll_interval_milliseconds) {
        return Err("--poll-interval-milliseconds must be in 100..=60000".into());
    }
    if !(1..=MAX_BATCH_SIZE).contains(&batch_size) {
        return Err("--batch-size must be in 1..=256".into());
    }
    if !(10..=300).contains(&lease_seconds) {
        return Err("--lease-seconds must be in 10..=300".into());
    }
    if !(1..=64).contains(&broadcast_concurrency) {
        return Err("--broadcast-concurrency must be in 1..=64".into());
    }
    Ok(Options {
        database_url,
        rpc_url,
        websocket_url,
        cluster,
        worker_id,
        once,
        poll_interval: Duration::from_millis(poll_interval_milliseconds),
        batch_size,
        lease_seconds,
        broadcast_concurrency,
    })
}

fn next_argument(
    arguments: &mut impl Iterator<Item = String>,
    flag: &str,
) -> Result<String, Box<dyn Error>> {
    arguments
        .next()
        .ok_or_else(|| format!("{flag} requires a value").into())
}

fn websocket_url_for_rpc(rpc_url: &str) -> Result<String, Box<dyn Error>> {
    if let Some(rest) = rpc_url.strip_prefix("https://") {
        return Ok(format!("wss://{rest}"));
    }
    if let Some(rest) = rpc_url.strip_prefix("http://") {
        return Ok(format!("ws://{rest}"));
    }
    Err("Solana RPC URL must use http or https to derive its WebSocket endpoint".into())
}

fn safe_detail(detail: &str) -> String {
    redacted_external_error(detail).chars().take(480).collect()
}

fn usage() -> &'static str {
    "Usage: fleet-route-confirmer --execute [--once] [--cluster CLUSTER] [--rpc-url URL] [--ws-url URL] [--worker-id ID] [--poll-interval-milliseconds N] [--batch-size 1..256] [--lease-seconds 10..300] [--broadcast-concurrency 1..64]\n\nRequires NEON_DATABASE_URL, SOLANA_RPC_URL, and explicit YIELD_ROUTE_CLUSTER (YIELD_ALT_CLUSTER is accepted for shared deployment configuration). SOLANA_WS_URL is optional and otherwise derived from the RPC URL. WebSocket notifications only accelerate an authoritative getSignatureStatuses read; the bounded batched polling path remains the durable fallback. The worker never loads a signer, rebuilds, or re-signs, and stops successful routes at durable reconciliation_pending for a separate route-specific reconciler."
}
