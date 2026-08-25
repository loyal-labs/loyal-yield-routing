use std::{
    collections::{HashMap, HashSet},
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::{Duration, Instant},
};

use anyhow::{bail, Context, Result};
pub use balance_sweep_ata_observations::{
    AtaObservationSink, BalanceSweepAtaObservation, BalanceSweepAtaObservationEvent,
    ObservationInsertOutcome, TimescaleAtaConfig, TimescaleAtaObservationSink, TimescaleAtaStream,
};
use chrono::{DateTime, Utc};
use futures_util::StreamExt;
use helius_laserstream::{
    grpc::{
        subscribe_update::UpdateOneof, CommitmentLevel, SubscribeRequest,
        SubscribeRequestFilterTransactions, SubscribeUpdate,
    },
    subscribe, LaserstreamConfig,
};
use loyal_actions::{SQUADS_SMART_ACCOUNT_PROGRAM_ID, USDC_MINT};
use loyal_observability::{EarnRebalanceMetrics, EarnRebalanceStage};
use loyal_squads_policy_monitor::{
    PolicyMonitor, PostgresPolicyMatchSink, EARN_MAX_POLICY_PROJECTION_CONSUMER,
};
use loyal_yield_store::{BalanceSweepTarget, BalanceSweepTargetId, OrchestratorStore};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};
use solana_account_decoder::{UiAccount, UiAccountData, UiAccountEncoding};
use solana_client::{rpc_client::RpcClient, rpc_config::RpcAccountInfoConfig};
use solana_program::program_pack::Pack;
use solana_pubsub_client::nonblocking::pubsub_client::PubsubClient;
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey, signature::Signature};
use tokio::{
    sync::{mpsc, Mutex, Notify, RwLock},
    task::{JoinHandle, JoinSet},
    time,
};

pub mod ata_recheck;
pub mod earn_apy;
pub mod earn_reconciliation;
#[cfg(feature = "local-e2e")]
pub mod local_e2e;
pub mod monitor_observability;
pub mod smart_account;

pub use earn_reconciliation::{
    enqueue_normalized_earn_update, process_next_autodeposit_reconciliation_request,
    process_next_earn_reconciliation_job, process_next_earn_reconciliation_job_with_policy_monitor,
    read_confirmed_squads_policy_transaction, reconcile_targeted_policy_vault_update,
    run_autodeposit_reconciliation_consumer, run_earn_reconciliation_consumer,
    AutodepositReconciliationProcessOutcome, EarnChainReader, EarnPolicyTransaction,
    EarnPolicyTransactionRead, EarnReconciliationProcessOutcome, FixtureEarnChainReader,
    RpcEarnChainReader,
};
pub use monitor_observability::{
    emit_earn_reconciliation_consumer_failed, emit_earn_reconciliation_health_snapshot_failed,
    emit_earn_reconciliation_job_failed, EarnMonitorMetrics,
};
pub use smart_account::{
    build_multi_channel_subscribe_request, normalize_laserstream_update, subscribe_request_json,
    EarnVaultWatch, EarnWatchAccount, NormalizedEarnUpdate, SubscriptionWatchSet,
    BALANCE_SWEEP_WALLET_ATAS, EARN_IDLE_TOKEN_ACCOUNTS, EARN_OBLIGATIONS, EARN_POLICY_ACCOUNTS,
    EARN_SMART_ACCOUNTS, EARN_VAULT_ACCOUNTS,
};

pub use ata_recheck::{
    record_missing_ata_zero_balance, record_skipped_ata_zero_balance, spawn_ata_recheck_worker,
    AtaRecheckConfig, AtaRecheckHandle,
};

type AccountNotification = solana_client::rpc_response::Response<UiAccount>;

pub const LASERSTREAM_SOURCE: &str = "laserstream_grpc";
pub const WEBSOCKET_SOURCE: &str = "websocket";
const EARN_MAX_MEMO_PROGRAM_ID: &str = "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr";
pub const RPC_SEED_SOURCE: &str = "rpc_seed";
pub const CONFIRMED_COMMITMENT: &str = "confirmed";
pub const FINALIZED_LASERSTREAM_COMMITMENT: &str = "finalized";

#[derive(Clone, Copy, Debug)]
pub struct SubscriptionConfig {
    pub max_reconnect_attempts: usize,
    pub reconnect_base_delay: Duration,
    pub reconnect_max_delay: Duration,
    pub heartbeat_interval: Duration,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct TokenAccountSnapshot {
    pub mint: Pubkey,
    pub owner: Pubkey,
    pub amount: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AtaTarget {
    pub id: BalanceSweepTargetId,
    pub cluster: String,
    pub wallet: String,
    pub wallet_usdc_ata: Pubkey,
    pub vault_pubkey: String,
    pub vault_usdc_ata: Pubkey,
}

impl AtaTarget {
    pub fn from_balance_sweep_target(
        value: &BalanceSweepTarget,
        cluster: impl Into<String>,
    ) -> Result<Self> {
        Ok(Self {
            id: value.id,
            cluster: cluster.into(),
            wallet: value.wallet.clone(),
            wallet_usdc_ata: value.wallet_token_ata.parse()?,
            vault_pubkey: value.vault_pubkey.clone(),
            vault_usdc_ata: value.vault_token_ata.parse()?,
        })
    }
}

#[derive(Debug)]
pub enum AtaUpdateEvent {
    Connecting {
        account: Pubkey,
        attempt: usize,
    },
    Connected {
        account: Pubkey,
        attempt: usize,
    },
    AccountUpdate {
        account: Pubkey,
        lamports: u64,
        slot: u64,
        owner: Pubkey,
        data: Vec<u8>,
        source: &'static str,
        source_commitment: &'static str,
        txn_signature: Option<String>,
        received_at: DateTime<Utc>,
    },
    EarnUpdate {
        update: NormalizedEarnUpdate,
    },
    Heartbeat {
        account: Pubkey,
    },
    Reconnecting {
        account: Pubkey,
        attempt: usize,
        backoff: Duration,
        error: String,
    },
    Failed {
        account: Pubkey,
        attempts: usize,
        error: String,
    },
    Stopped {
        account: Pubkey,
    },
}

/// Why an update could not become an observation for its target.
///
/// Both cases are per-account facts about on-chain state, not monitor
/// failures, so they must never take the session down with them.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AtaUpdateSkip {
    NonSplTokenOwner { owner: Pubkey },
    UnexpectedMint { mint: Pubkey },
}

impl AtaUpdateSkip {
    pub fn reason(&self) -> &'static str {
        match self {
            Self::NonSplTokenOwner { .. } => "non_spl_token_owner",
            Self::UnexpectedMint { .. } => "unexpected_mint",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AtaUpdateOutcome {
    Recorded(ObservationInsertOutcome),
    Skipped(AtaUpdateSkip),
}

pub trait AtaUpdateSource {
    fn spawn(
        self,
        accounts: Vec<Pubkey>,
        tx: mpsc::UnboundedSender<AtaUpdateEvent>,
        running: Arc<AtomicBool>,
    ) -> JoinHandle<()>;
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AtaTargetSetDiff {
    pub added: Vec<Pubkey>,
    pub removed: Vec<Pubkey>,
}

impl AtaTargetSetDiff {
    pub fn has_changes(&self) -> bool {
        !self.added.is_empty() || !self.removed.is_empty()
    }
}

pub fn ata_target_set(targets: &[AtaTarget]) -> HashSet<Pubkey> {
    targets
        .iter()
        .map(|target| target.wallet_usdc_ata)
        .collect()
}

pub fn diff_ata_target_sets(current: &HashSet<Pubkey>, next: &HashSet<Pubkey>) -> AtaTargetSetDiff {
    let mut added = next.difference(current).copied().collect::<Vec<Pubkey>>();
    let mut removed = current.difference(next).copied().collect::<Vec<Pubkey>>();
    added.sort_by_key(ToString::to_string);
    removed.sort_by_key(ToString::to_string);
    AtaTargetSetDiff { added, removed }
}

pub fn laserstream_replay_from_slot(current_slot: u64, replay_overlap_slots: u64) -> u64 {
    current_slot.saturating_sub(replay_overlap_slots)
}

#[derive(Debug, Clone)]
pub struct LaserstreamAtaUpdateSource {
    pub endpoint: String,
    pub api_key: String,
    pub from_slot: u64,
    pub config: SubscriptionConfig,
    /// Optional Earn bindings are folded into the same physical LaserStream
    /// SubscribeRequest.  `None` preserves the legacy ATA-only caller.
    pub watch_set: Option<SubscriptionWatchSet>,
}

#[derive(Debug, Clone)]
pub struct LaserstreamPolicyUpdateSource {
    pub endpoint: String,
    pub api_key: String,
    pub rpc_url: String,
    pub from_slot: u64,
    pub config: SubscriptionConfig,
}

impl LaserstreamPolicyUpdateSource {
    pub fn spawn(
        self,
        store: OrchestratorStore,
        policy_monitor: Arc<Mutex<PolicyMonitor<PostgresPolicyMatchSink>>>,
        running: Arc<AtomicBool>,
    ) -> JoinHandle<()> {
        tokio::spawn(async move {
            run_earn_max_policy_laserstream(self, store, policy_monitor, running).await;
        })
    }
}

#[derive(Clone)]
pub struct EarnUpdateContext {
    pub store: OrchestratorStore,
    pub consumer_name: String,
    pub watch_set: Arc<RwLock<SubscriptionWatchSet>>,
    pub wake: Arc<Notify>,
}

impl LaserstreamAtaUpdateSource {
    pub fn spawn_with_watch_set(
        self,
        accounts: Vec<Pubkey>,
        tx: mpsc::UnboundedSender<AtaUpdateEvent>,
        running: Arc<AtomicBool>,
        watch_set: Arc<RwLock<SubscriptionWatchSet>>,
    ) -> JoinHandle<()> {
        tokio::spawn(async move {
            run_laserstream_loop(self, accounts, tx, running, watch_set).await;
        })
    }
}

impl AtaUpdateSource for LaserstreamAtaUpdateSource {
    fn spawn(
        self,
        accounts: Vec<Pubkey>,
        tx: mpsc::UnboundedSender<AtaUpdateEvent>,
        running: Arc<AtomicBool>,
    ) -> JoinHandle<()> {
        let initial_watch_set = self.watch_set.clone().unwrap_or(SubscriptionWatchSet {
            balance_sweep_accounts: accounts.iter().map(ToString::to_string).collect(),
            earn_vaults: Vec::new(),
            observation_start_slot: None,
        });
        let watch_set = Arc::new(RwLock::new(initial_watch_set));
        self.spawn_with_watch_set(accounts, tx, running, watch_set)
    }
}

#[derive(Debug, Clone)]
pub struct WebsocketAtaUpdateSource {
    pub ws_url: String,
    pub config: SubscriptionConfig,
}

impl AtaUpdateSource for WebsocketAtaUpdateSource {
    fn spawn(
        self,
        accounts: Vec<Pubkey>,
        tx: mpsc::UnboundedSender<AtaUpdateEvent>,
        running: Arc<AtomicBool>,
    ) -> JoinHandle<()> {
        tokio::spawn(async move {
            run_websocket_loop(self.ws_url, accounts, self.config, tx, running).await;
        })
    }
}

pub fn decode_spl_token_account(data: &[u8]) -> Result<TokenAccountSnapshot> {
    let account = spl_token::state::Account::unpack(data).context("decode SPL token account")?;
    Ok(TokenAccountSnapshot {
        mint: account.mint,
        owner: account.owner,
        amount: account.amount,
    })
}

pub fn account_data_hash(data: &[u8]) -> String {
    let digest = Sha256::digest(data);
    digest.iter().map(|byte| format!("{byte:02x}")).collect()
}

pub fn raw_account_data_base64(data: &[u8]) -> String {
    use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
    BASE64_STANDARD.encode(data)
}

pub async fn seed_current_balances(
    rpc_url: &str,
    targets: &[AtaTarget],
    sink: &impl AtaObservationSink,
) -> Result<()> {
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    for chunk in targets.chunks(100) {
        let accounts: Vec<Pubkey> = chunk.iter().map(|target| target.wallet_usdc_ata).collect();
        // Stamp every observation with the slot the read itself happened at. A
        // separately fetched slot can postdate a recreation this read never
        // saw, and the slot-monotonic balance row would then keep the zero and
        // discard the recreation update that follows it.
        let config = RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::confirmed()),
            data_slice: None,
            min_context_slot: None,
        };
        let fetched = rpc
            .get_multiple_accounts_with_config(&accounts, config)
            .with_context(|| format!("fetch {} wallet ATA seed accounts", accounts.len()))?;
        let seed_observed_slot = fetched.context.slot;
        if seed_observed_slot == 0 {
            bail!("RPC seed observed slot was zero");
        }
        for (target, account) in chunk.iter().zip(fetched.value) {
            let Some(account) = account else {
                // The account is gone, which RPC has just confirmed at a known
                // slot. Recording it here repairs targets whose closing frame
                // was never observed, including rechecks lost to a restart.
                tracing::warn!(
                    wallet_usdc_ata = %target.wallet_usdc_ata,
                    "recording zero balance for missing wallet ATA seed account"
                );
                record_missing_ata_zero_balance(target, seed_observed_slot, sink).await?;
                continue;
            };
            let data = account.data.clone();
            let outcome = process_account_update(
                target,
                account.lamports,
                seed_observed_slot,
                account.owner,
                account.data,
                RPC_SEED_SOURCE,
                CONFIRMED_COMMITMENT,
                None,
                Utc::now(),
                sink,
            )
            .await?;
            // The account exists but was reclaimed or holds another mint, so it
            // carries no routeable USDC. Settle it here rather than leaving the
            // previous balance standing until a recheck that a restart may have
            // already erased.
            if let AtaUpdateOutcome::Skipped(skip) = outcome {
                record_skipped_ata_zero_balance(target, seed_observed_slot, skip, &data, sink)
                    .await?;
            }
        }
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
pub async fn process_account_update(
    target: &AtaTarget,
    lamports: u64,
    slot: u64,
    owner: Pubkey,
    data: Vec<u8>,
    source: &str,
    source_commitment: &str,
    txn_signature: Option<String>,
    received_at: DateTime<Utc>,
    sink: &impl AtaObservationSink,
) -> Result<AtaUpdateOutcome> {
    if owner != spl_token::id() {
        tracing::warn!(
            wallet_usdc_ata = %target.wallet_usdc_ata,
            owner = %owner,
            "skipping wallet ATA update owned by non-SPL-token program"
        );
        return Ok(AtaUpdateOutcome::Skipped(AtaUpdateSkip::NonSplTokenOwner {
            owner,
        }));
    }
    let snapshot = decode_spl_token_account(&data)?;
    if snapshot.mint != USDC_MINT {
        tracing::warn!(
            wallet_usdc_ata = %target.wallet_usdc_ata,
            mint = %snapshot.mint,
            "skipping wallet ATA update for non-USDC mint"
        );
        return Ok(AtaUpdateOutcome::Skipped(AtaUpdateSkip::UnexpectedMint {
            mint: snapshot.mint,
        }));
    }
    let hash = account_data_hash(&data);
    let raw_account_data_base64 = raw_account_data_base64(&data);
    let observation = BalanceSweepAtaObservation {
        target_id: target.id,
        cluster: target.cluster.clone(),
        wallet: target.wallet.clone(),
        wallet_usdc_ata: target.wallet_usdc_ata.to_string(),
        vault_pubkey: target.vault_pubkey.clone(),
        vault_usdc_ata: target.vault_usdc_ata.to_string(),
        amount_raw: snapshot.amount,
        owner: Some(snapshot.owner.to_string()),
        mint: snapshot.mint.to_string(),
        slot,
        observed_at: received_at,
        source: source.to_owned(),
        source_commitment: source_commitment.to_owned(),
        txn_signature: txn_signature.clone(),
        account_data_hash: hash.clone(),
        raw_account_data_base64: raw_account_data_base64.clone(),
        raw_evidence: json!({
            "lamports": lamports,
            "account_data_hash": hash,
            "txn_signature": txn_signature,
            "raw_account_data_base64": raw_account_data_base64,
            "source": source,
            "wallet": target.wallet,
            "wallet_usdc_ata": target.wallet_usdc_ata.to_string(),
            "vault_pubkey": target.vault_pubkey,
            "vault_usdc_ata": target.vault_usdc_ata.to_string(),
        }),
        received_at,
    };
    let outcome = sink.record_observation(observation).await?;
    Ok(AtaUpdateOutcome::Recorded(outcome))
}

pub async fn run_event_loop(
    mut rx: mpsc::UnboundedReceiver<AtaUpdateEvent>,
    targets: HashMap<Pubkey, AtaTarget>,
    sink: impl AtaObservationSink,
    running: Arc<AtomicBool>,
    recheck: Option<AtaRecheckHandle>,
    earn: Option<EarnUpdateContext>,
    earn_rebalance_metrics: EarnRebalanceMetrics,
) -> Result<()> {
    while running.load(Ordering::Relaxed) {
        let Some(event) = rx.recv().await else {
            break;
        };
        let event = match event {
            AtaUpdateEvent::EarnUpdate { update } => {
                let Some(earn) = earn.as_ref() else {
                    tracing::warn!(
                        slot = update.slot,
                        "dropping Earn update without persistence context"
                    );
                    continue;
                };
                let watch_set = earn.watch_set.read().await.clone();
                enqueue_normalized_earn_update(
                    &earn.store,
                    &earn.consumer_name,
                    &update,
                    &watch_set,
                )
                .await
                .map_err(|error| anyhow::anyhow!(error))?;
                earn.wake.notify_waiters();
                continue;
            }
            other => other,
        };
        if let AtaUpdateEvent::AccountUpdate {
            account,
            lamports,
            slot,
            owner,
            data,
            source,
            source_commitment,
            txn_signature,
            received_at,
        } = event
        {
            let Some(target) = targets.get(&account) else {
                tracing::debug!(account = %account, "ignoring unwatched ATA update");
                continue;
            };
            tracing::debug!(
                account = %account,
                target_id = target.id.as_i64(),
                slot,
                source,
                "writing raw wallet ATA observation"
            );
            let started = Instant::now();
            let outcome = process_account_update(
                target,
                lamports,
                slot,
                owner,
                data,
                source,
                source_commitment,
                txn_signature,
                received_at,
                &sink,
            )
            .await?;
            if matches!(outcome, AtaUpdateOutcome::Recorded(insert) if insert.inserted) {
                earn_rebalance_metrics.record_success(
                    EarnRebalanceStage::AtaObservationPersisted,
                    started.elapsed(),
                );
            }
            // The streamed frame cannot be trusted to describe the account's
            // settled state, so hand the target to the recheck queue and keep
            // serving every other target on this session.
            if let AtaUpdateOutcome::Skipped(skip) = outcome {
                match recheck.as_ref() {
                    Some(recheck) => recheck.enqueue(target, skip, slot),
                    None => tracing::warn!(
                        account = %account,
                        reason = skip.reason(),
                        slot,
                        "skipped wallet ATA update without a recheck queue"
                    ),
                }
            }
        }
    }
    Ok(())
}

pub fn build_laserstream_subscribe_request(
    accounts: &[Pubkey],
    from_slot: u64,
) -> SubscribeRequest {
    build_multi_channel_subscribe_request(
        &SubscriptionWatchSet {
            balance_sweep_accounts: accounts.iter().map(ToString::to_string).collect(),
            earn_vaults: Vec::new(),
            observation_start_slot: None,
        },
        from_slot,
    )
}

async fn run_laserstream_loop(
    source: LaserstreamAtaUpdateSource,
    accounts: Vec<Pubkey>,
    tx: mpsc::UnboundedSender<AtaUpdateEvent>,
    running: Arc<AtomicBool>,
    watch_set_state: Arc<RwLock<SubscriptionWatchSet>>,
) {
    let mut current_watch_set = source.watch_set.clone().unwrap_or(SubscriptionWatchSet {
        balance_sweep_accounts: Vec::new(),
        earn_vaults: Vec::new(),
        observation_start_slot: None,
    });
    current_watch_set
        .balance_sweep_accounts
        .extend(accounts.iter().map(ToString::to_string));
    current_watch_set.balance_sweep_accounts.sort();
    current_watch_set.balance_sweep_accounts.dedup();
    *watch_set_state.write().await = current_watch_set.clone();
    let request = build_multi_channel_subscribe_request(&current_watch_set, source.from_slot);
    tracing::info!(
        account_count = accounts.len(),
        endpoint = %source.endpoint,
        from_slot = source.from_slot,
        "starting Laserstream ATA subscription"
    );
    let mut attempt = 1;
    while running.load(Ordering::Relaxed) && attempt <= source.config.max_reconnect_attempts {
        for account in &accounts {
            let _ = tx.send(AtaUpdateEvent::Connecting {
                account: *account,
                attempt,
            });
        }
        let config = LaserstreamConfig::new(source.endpoint.clone(), source.api_key.clone())
            .with_max_reconnect_attempts(0)
            .with_replay(true);
        let (stream, _handle) = subscribe(config, request.clone());
        futures_util::pin_mut!(stream);
        for account in &accounts {
            tracing::info!(
                account = %account,
                attempt,
                "Laserstream ATA subscription connected"
            );
            let _ = tx.send(AtaUpdateEvent::Connected {
                account: *account,
                attempt,
            });
        }
        let mut heartbeat = time::interval(source.config.heartbeat_interval);
        let disconnect_error = loop {
            if !running.load(Ordering::Relaxed) {
                break None;
            }
            tokio::select! {
                update = stream.next() => {
                    match update {
                        Some(Ok(update)) => {
                            let _ = forward_laserstream_update(update, &tx);
                        }
                        Some(Err(error)) => {
                            tracing::warn!(error = %error, attempt, "Laserstream ATA stream failed");
                            break Some(error.to_string());
                        }
                        None => break Some("Laserstream ATA stream ended".to_owned()),
                    }
                }
                _ = heartbeat.tick() => {
                    for account in &accounts {
                        let _ = tx.send(AtaUpdateEvent::Heartbeat { account: *account });
                    }
                }
            }
        };
        let Some(error) = disconnect_error else {
            break;
        };
        if attempt >= source.config.max_reconnect_attempts {
            for account in &accounts {
                let _ = tx.send(AtaUpdateEvent::Failed {
                    account: *account,
                    attempts: attempt,
                    error: error.clone(),
                });
            }
            break;
        }
        let backoff = reconnect_backoff(source.config, attempt);
        tracing::warn!(
            attempt,
            next_attempt = attempt + 1,
            backoff_ms = backoff.as_millis(),
            error = %error,
            "reconnecting Laserstream ATA subscription"
        );
        for account in &accounts {
            let _ = tx.send(AtaUpdateEvent::Reconnecting {
                account: *account,
                attempt,
                backoff,
                error: error.clone(),
            });
        }
        time::sleep(backoff).await;
        attempt += 1;
    }
}

async fn run_earn_max_policy_laserstream(
    source: LaserstreamPolicyUpdateSource,
    store: OrchestratorStore,
    policy_monitor: Arc<Mutex<PolicyMonitor<PostgresPolicyMatchSink>>>,
    running: Arc<AtomicBool>,
) {
    let policy_transactions = HashMap::from([(
        "earn_max_policy_transactions".to_owned(),
        SubscribeRequestFilterTransactions {
            vote: Some(false),
            failed: Some(false),
            account_include: vec![SQUADS_SMART_ACCOUNT_PROGRAM_ID.to_string()],
            ..SubscribeRequestFilterTransactions::default()
        },
    )]);
    // Deposits are deliberately light outer SPL Token + Memo transactions.
    // Give their exact filter an independent stream so a global Squads replay
    // cannot delay user cash flows. Both streams retain the same confirmed,
    // idempotent projection boundary and durable offset.
    let cash_flow_transactions = HashMap::from([(
        "earn_max_usdc_cash_flows".to_owned(),
        SubscribeRequestFilterTransactions {
            vote: Some(false),
            failed: Some(false),
            // LaserStream applies account_include as OR and account_required as
            // AND. Name USDC explicitly instead of relying on an empty include
            // set so historical replay and the live stream use the same exact
            // transaction boundary.
            account_include: vec![USDC_MINT.to_string()],
            account_required: vec![
                EARN_MAX_MEMO_PROGRAM_ID.to_owned(),
                spl_token::ID.to_string(),
            ],
            ..SubscribeRequestFilterTransactions::default()
        },
    )]);
    tracing::info!(
        endpoint = %source.endpoint,
        from_slot = source.from_slot,
        program = %SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        commitment = CONFIRMED_COMMITMENT,
        "starting Earn MAX policy LaserStream subscription"
    );
    let from_slot = source.from_slot;

    let policy = run_earn_max_laserstream_subscription(
        source.clone(),
        SubscribeRequest {
            transactions: policy_transactions,
            commitment: Some(CommitmentLevel::Confirmed as i32),
            from_slot: Some(from_slot),
            ..SubscribeRequest::default()
        },
        EarnMaxProjectionKind::Policy,
        store.clone(),
        Arc::clone(&policy_monitor),
        Arc::clone(&running),
    );
    let cash_flow = run_earn_max_laserstream_subscription(
        source,
        SubscribeRequest {
            transactions: cash_flow_transactions,
            commitment: Some(CommitmentLevel::Confirmed as i32),
            from_slot: Some(from_slot),
            ..SubscribeRequest::default()
        },
        EarnMaxProjectionKind::CashFlow,
        store,
        policy_monitor,
        running,
    );
    tokio::join!(policy, cash_flow);
}

#[derive(Clone, Copy, Debug)]
enum EarnMaxProjectionKind {
    Policy,
    CashFlow,
}

async fn run_earn_max_laserstream_subscription(
    source: LaserstreamPolicyUpdateSource,
    request: SubscribeRequest,
    kind: EarnMaxProjectionKind,
    store: OrchestratorStore,
    policy_monitor: Arc<Mutex<PolicyMonitor<PostgresPolicyMatchSink>>>,
    running: Arc<AtomicBool>,
) {
    let rpc = Arc::new(RpcClient::new_with_commitment(
        source.rpc_url.clone(),
        CommitmentConfig::confirmed(),
    ));
    let mut attempt = 1;
    while running.load(Ordering::Relaxed) && attempt <= source.config.max_reconnect_attempts {
        let config = LaserstreamConfig::new(source.endpoint.clone(), source.api_key.clone())
            .with_max_reconnect_attempts(0)
            .with_replay(true);
        let (stream, _handle) = subscribe(config, request.clone());
        futures_util::pin_mut!(stream);
        tracing::info!(
            attempt,
            ?kind,
            "Earn MAX policy LaserStream subscription connected"
        );
        let mut heartbeat = time::interval(source.config.heartbeat_interval);
        let disconnect_error = loop {
            if !running.load(Ordering::Relaxed) {
                break None;
            }
            tokio::select! {
                update = stream.next() => {
                    match update {
                        Some(Ok(update)) => {
                            if let Err(error) = process_earn_max_policy_update(
                                update,
                                &store,
                                Arc::clone(&rpc),
                                Arc::clone(&policy_monitor),
                                kind,
                            ).await {
                                tracing::warn!(error = %error, attempt, ?kind, "Earn MAX policy projection failed");
                                break Some(error.to_string());
                            }
                        }
                        Some(Err(error)) => {
                            tracing::warn!(error = %error, attempt, "Earn MAX policy LaserStream failed");
                            break Some(error.to_string());
                        }
                        None => break Some("Earn MAX policy LaserStream ended".to_owned()),
                    }
                }
                _ = heartbeat.tick() => {}
            }
        };
        let Some(error) = disconnect_error else {
            break;
        };
        if attempt >= source.config.max_reconnect_attempts {
            tracing::error!(attempt, error = %error, "Earn MAX policy LaserStream exhausted reconnects");
            break;
        }
        let backoff = reconnect_backoff(source.config, attempt);
        tracing::warn!(
            attempt,
            next_attempt = attempt + 1,
            backoff_ms = backoff.as_millis(),
            error = %error,
            "reconnecting Earn MAX policy LaserStream"
        );
        time::sleep(backoff).await;
        attempt += 1;
    }
}

async fn process_earn_max_policy_update(
    update: SubscribeUpdate,
    store: &OrchestratorStore,
    rpc: Arc<RpcClient>,
    policy_monitor: Arc<Mutex<PolicyMonitor<PostgresPolicyMatchSink>>>,
    kind: EarnMaxProjectionKind,
) -> Result<()> {
    let Some(UpdateOneof::Transaction(transaction_update)) = update.update_oneof else {
        return Ok(());
    };
    let transaction = transaction_update
        .transaction
        .context("Earn MAX LaserStream transaction payload was missing")?;
    let slot = transaction_update.slot;
    let transaction =
        earn_reconciliation::decode_laserstream_squads_policy_transaction(transaction, slot)?;
    if let EarnPolicyTransactionRead::Transaction(transaction) = transaction {
        match kind {
            EarnMaxProjectionKind::Policy => {
                if !transaction.instructions.is_empty() {
                    policy_monitor
                        .lock()
                        .await
                        .process_policy_instructions(
                            &transaction.signature,
                            transaction.slot,
                            transaction.instructions.clone(),
                        )
                        .await
                        .map_err(|error| anyhow::anyhow!(error))?;
                }
                earn_reconciliation::project_earn_max_memos(store, &transaction).await?;
            }
            EarnMaxProjectionKind::CashFlow => {
                if !transaction
                    .earn_max_memos
                    .iter()
                    .any(|memo| memo.data.starts_with(b"loyal:earn-max:v2:"))
                {
                    return Ok(());
                }
                earn_reconciliation::project_earn_max_cash_flows(store, rpc, &transaction).await?;
            }
        }
    }
    store
        .advance_projection_offset(
            EARN_MAX_POLICY_PROJECTION_CONSUMER,
            i64::try_from(slot).context("Earn MAX policy slot exceeds BIGINT")?,
        )
        .await?;
    Ok(())
}

pub fn reconnect_backoff(config: SubscriptionConfig, completed_attempt: usize) -> Duration {
    let shift = completed_attempt.saturating_sub(1).min(31);
    let multiplier = 1_u32 << shift;
    config
        .reconnect_base_delay
        .saturating_mul(multiplier)
        .min(config.reconnect_max_delay)
}

fn forward_laserstream_update(
    update: SubscribeUpdate,
    tx: &mpsc::UnboundedSender<AtaUpdateEvent>,
) -> Result<()> {
    let earn_update = normalize_laserstream_update(update.clone())?;
    if let Some(UpdateOneof::Account(account_update)) = update.update_oneof {
        let account = account_update
            .account
            .context("LaserStream account update was missing account payload")?;
        let pubkey = pubkey_from_laserstream_bytes(&account.pubkey, "account pubkey")?;
        let owner = pubkey_from_laserstream_bytes(&account.owner, "account owner")?;
        let txn_signature = account
            .txn_signature
            .as_deref()
            .map(signature_from_laserstream_bytes)
            .transpose()?;
        // A shared binding is first delivered to the established ATA path;
        // its independent Earn wake-up follows below and is never decoded as
        // a balance delta.
        let _ = tx.send(AtaUpdateEvent::AccountUpdate {
            account: pubkey,
            lamports: account.lamports,
            slot: account_update.slot,
            owner,
            data: account.data,
            source: LASERSTREAM_SOURCE,
            source_commitment: FINALIZED_LASERSTREAM_COMMITMENT,
            txn_signature,
            received_at: Utc::now(),
        });
    }
    if let Some(earn_update) = earn_update {
        let _ = tx.send(AtaUpdateEvent::EarnUpdate {
            update: earn_update,
        });
    }
    Ok(())
}

fn signature_from_laserstream_bytes(bytes: &[u8]) -> Result<String> {
    let signature = Signature::try_from(bytes).context("LaserStream txn signature bytes")?;
    Ok(signature.to_string())
}

fn pubkey_from_laserstream_bytes(bytes: &[u8], label: &str) -> Result<Pubkey> {
    if bytes.len() != 32 {
        bail!(
            "LaserStream {label} decoded to {} bytes, expected 32",
            bytes.len()
        );
    }
    let mut array = [0_u8; 32];
    array.copy_from_slice(bytes);
    Ok(Pubkey::new_from_array(array))
}

async fn run_websocket_loop(
    ws_url: String,
    accounts: Vec<Pubkey>,
    _config: SubscriptionConfig,
    tx: mpsc::UnboundedSender<AtaUpdateEvent>,
    running: Arc<AtomicBool>,
) {
    let Ok(client) = PubsubClient::new(&ws_url).await else {
        tracing::warn!(ws_url = %ws_url, "failed to connect websocket ATA source");
        for account in accounts {
            let _ = tx.send(AtaUpdateEvent::Failed {
                account,
                attempts: 1,
                error: "connect websocket".to_owned(),
            });
        }
        return;
    };
    let client = Arc::new(client);
    let mut join_set = JoinSet::new();
    for account in accounts {
        let client = Arc::clone(&client);
        let tx = tx.clone();
        let running = Arc::clone(&running);
        join_set.spawn(async move {
            let config = RpcAccountInfoConfig {
                encoding: Some(UiAccountEncoding::Base64),
                commitment: Some(CommitmentConfig::confirmed()),
                ..Default::default()
            };
            let Ok((mut stream, unsubscribe)) =
                client.account_subscribe(&account, Some(config)).await
            else {
                tracing::warn!(account = %account, "failed to subscribe websocket ATA account");
                let _ = tx.send(AtaUpdateEvent::Failed {
                    account,
                    attempts: 1,
                    error: "subscribe websocket account".to_owned(),
                });
                return;
            };
            let _ = tx.send(AtaUpdateEvent::Connected {
                account,
                attempt: 1,
            });
            tracing::info!(account = %account, "websocket ATA subscription connected");
            while running.load(Ordering::Relaxed) {
                match time::timeout(Duration::from_millis(500), stream.next()).await {
                    Ok(Some(notification)) => {
                        let _ = forward_websocket_update(account, notification, &tx);
                    }
                    Ok(None) => break,
                    Err(_) => {}
                }
            }
            unsubscribe().await;
        });
    }
    while join_set.join_next().await.is_some() {}
}

fn forward_websocket_update(
    account: Pubkey,
    notification: AccountNotification,
    tx: &mpsc::UnboundedSender<AtaUpdateEvent>,
) -> Result<()> {
    let data = decode_ui_account_data(notification.value.data)?;
    let owner = notification.value.owner.parse()?;
    let _ = tx.send(AtaUpdateEvent::AccountUpdate {
        account,
        lamports: notification.value.lamports,
        slot: notification.context.slot,
        owner,
        data,
        source: WEBSOCKET_SOURCE,
        source_commitment: CONFIRMED_COMMITMENT,
        txn_signature: None,
        received_at: Utc::now(),
    });
    Ok(())
}

fn decode_ui_account_data(data: UiAccountData) -> Result<Vec<u8>> {
    match data {
        UiAccountData::Binary(encoded, UiAccountEncoding::Base64) => {
            use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
            BASE64_STANDARD
                .decode(encoded)
                .context("decode base64 account data")
        }
        UiAccountData::LegacyBinary(encoded) => {
            use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
            BASE64_STANDARD
                .decode(encoded)
                .context("decode legacy base64 account data")
        }
        _ => bail!("unsupported account data encoding"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fresh_subscription_request_preserves_replay_slot() {
        let request = build_multi_channel_subscribe_request(
            &SubscriptionWatchSet {
                balance_sweep_accounts: vec!["11111111111111111111111111111111".to_owned()],
                earn_vaults: Vec::new(),
                observation_start_slot: None,
            },
            42,
        );

        assert_eq!(request.from_slot, Some(42));
    }
}
