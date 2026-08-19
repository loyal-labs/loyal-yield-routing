use std::{
    collections::{HashMap, HashSet},
    future::Future,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};

use anyhow::{bail, Context, Result};
pub use balance_sweep_ata_observations::{
    AtaObservationSink, BalanceSweepAtaObservation, BalanceSweepAtaObservationEvent,
    ObservationInsertOutcome, TimescaleAtaConfig, TimescaleAtaObservationSink, TimescaleAtaStream,
};
use chrono::{DateTime, Utc};
use futures_util::StreamExt;
use helius_laserstream::{
    grpc::{subscribe_update::UpdateOneof, SubscribeRequest, SubscribeUpdate},
    subscribe, LaserstreamConfig,
};
use loyal_actions::USDC_MINT;
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
    sync::{mpsc, oneshot, RwLock},
    task::{JoinHandle, JoinSet},
    time,
};

pub mod ata_recheck;
pub mod earn_apy;
pub mod earn_reconciliation;
pub mod smart_account;

pub use earn_reconciliation::{
    reconcile_normalized_earn_update, EarnChainReader, FixtureEarnChainReader, RpcEarnChainReader,
};
pub use smart_account::{
    build_multi_channel_subscribe_request, normalize_laserstream_update, subscribe_request_json,
    EarnVaultWatch, EarnWatchAccount, NormalizedEarnUpdate, SubscriptionWatchSet,
    BALANCE_SWEEP_WALLET_ATAS, EARN_IDLE_TOKEN_ACCOUNTS, EARN_OBLIGATIONS, EARN_POLICY_ACCOUNTS,
    EARN_VAULT_ACCOUNTS,
};

pub use ata_recheck::{
    record_missing_ata_zero_balance, record_skipped_ata_zero_balance, spawn_ata_recheck_worker,
    AtaRecheckConfig, AtaRecheckHandle,
};

type AccountNotification = solana_client::rpc_response::Response<UiAccount>;

pub const LASERSTREAM_SOURCE: &str = "laserstream_grpc";
pub const WEBSOCKET_SOURCE: &str = "websocket";
pub const RPC_SEED_SOURCE: &str = "rpc_seed";
pub const CONFIRMED_COMMITMENT: &str = "confirmed";

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

#[derive(Clone)]
pub struct LaserstreamSubscriptionUpdateHandle {
    tx: mpsc::UnboundedSender<LaserstreamSubscriptionReplacement>,
}

impl LaserstreamSubscriptionUpdateHandle {
    pub async fn replace(&self, watch_set: SubscriptionWatchSet) -> Result<()> {
        let (accepted_tx, accepted_rx) = oneshot::channel();
        self.tx
            .send(LaserstreamSubscriptionReplacement {
                watch_set,
                accepted: accepted_tx,
            })
            .map_err(|_| anyhow::anyhow!("LaserStream subscription update task stopped"))?;
        accepted_rx
            .await
            .map_err(|_| anyhow::anyhow!("LaserStream subscription update was not accepted"))
    }
}

struct LaserstreamSubscriptionReplacement {
    watch_set: SubscriptionWatchSet,
    accepted: oneshot::Sender<()>,
}

#[derive(Clone)]
pub struct EarnUpdateContext {
    pub store: OrchestratorStore,
    pub consumer_name: String,
    pub watch_set: Arc<RwLock<SubscriptionWatchSet>>,
    pub chain: Arc<dyn EarnChainReader>,
}

impl LaserstreamAtaUpdateSource {
    pub fn spawn_with_updates(
        self,
        accounts: Vec<Pubkey>,
        tx: mpsc::UnboundedSender<AtaUpdateEvent>,
        running: Arc<AtomicBool>,
        watch_set: Arc<RwLock<SubscriptionWatchSet>>,
    ) -> (JoinHandle<()>, LaserstreamSubscriptionUpdateHandle) {
        let (update_tx, update_rx) = mpsc::unbounded_channel();
        let task = tokio::spawn(async move {
            run_laserstream_loop(self, accounts, tx, running, update_rx, watch_set).await;
        });
        (task, LaserstreamSubscriptionUpdateHandle { tx: update_tx })
    }
}

impl AtaUpdateSource for LaserstreamAtaUpdateSource {
    fn spawn(
        self,
        accounts: Vec<Pubkey>,
        tx: mpsc::UnboundedSender<AtaUpdateEvent>,
        running: Arc<AtomicBool>,
    ) -> JoinHandle<()> {
        let (_update_tx, update_rx) = mpsc::unbounded_channel();
        let initial_watch_set = self.watch_set.clone().unwrap_or(SubscriptionWatchSet {
            balance_sweep_accounts: accounts.iter().map(ToString::to_string).collect(),
            earn_vaults: Vec::new(),
        });
        let watch_set = Arc::new(RwLock::new(initial_watch_set));
        tokio::spawn(async move {
            run_laserstream_loop(self, accounts, tx, running, update_rx, watch_set).await;
        })
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
                reconcile_normalized_earn_update(
                    &earn.store,
                    &earn.consumer_name,
                    &update,
                    &watch_set,
                    earn.chain.as_ref(),
                )
                .await
                .map_err(|error| anyhow::anyhow!(error))?;
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
        },
        from_slot,
    )
}

async fn run_laserstream_loop(
    source: LaserstreamAtaUpdateSource,
    accounts: Vec<Pubkey>,
    tx: mpsc::UnboundedSender<AtaUpdateEvent>,
    running: Arc<AtomicBool>,
    mut subscription_updates: mpsc::UnboundedReceiver<LaserstreamSubscriptionReplacement>,
    watch_set_state: Arc<RwLock<SubscriptionWatchSet>>,
) {
    let mut current_watch_set = source.watch_set.clone().unwrap_or(SubscriptionWatchSet {
        balance_sweep_accounts: Vec::new(),
        earn_vaults: Vec::new(),
    });
    current_watch_set
        .balance_sweep_accounts
        .extend(accounts.iter().map(ToString::to_string));
    current_watch_set.balance_sweep_accounts.sort();
    current_watch_set.balance_sweep_accounts.dedup();
    *watch_set_state.write().await = current_watch_set.clone();
    let mut request = build_multi_channel_subscribe_request(&current_watch_set, source.from_slot);
    let mut subscription_updates_open = true;
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
        let (stream, handle) = subscribe(config, request.clone());
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
                replacement = subscription_updates.recv(), if subscription_updates_open => {
                    let Some(LaserstreamSubscriptionReplacement {
                        watch_set: mut replacement,
                        accepted,
                    }) = replacement else {
                        subscription_updates_open = false;
                        continue;
                    };
                    let write_result = stage_and_write_subscription_replacement(
                        &mut replacement,
                        &accounts,
                        source.from_slot,
                        &mut current_watch_set,
                        &mut request,
                        &watch_set_state,
                        |replacement_request| handle.write(replacement_request),
                    )
                    .await;
                    // The supervisor may commit this desired watch set after
                    // either a successful live write or a failed write whose
                    // replacement request is retained for the reconnect.
                    let _ = accepted.send(());
                    if let Err(error) = write_result {
                        break Some(format!("replace LaserStream subscription: {error}"));
                    }
                    tracing::info!(
                        earn_vault_count = current_watch_set.earn_vaults.len(),
                        "replaced live LaserStream smart-account subscription"
                    );
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

async fn stage_and_write_subscription_replacement<F, Fut, E>(
    replacement: &mut SubscriptionWatchSet,
    accounts: &[Pubkey],
    from_slot: u64,
    current_watch_set: &mut SubscriptionWatchSet,
    request: &mut SubscribeRequest,
    watch_set_state: &Arc<RwLock<SubscriptionWatchSet>>,
    write: F,
) -> std::result::Result<(), E>
where
    F: FnOnce(SubscribeRequest) -> Fut,
    Fut: Future<Output = std::result::Result<(), E>>,
{
    replacement
        .balance_sweep_accounts
        .extend(accounts.iter().map(ToString::to_string));
    replacement.balance_sweep_accounts.sort();
    replacement.balance_sweep_accounts.dedup();
    let replacement_request = build_multi_channel_subscribe_request(replacement, from_slot);

    // Publish the routing map before Helius can deliver a newly subscribed
    // address. Retain the desired request before attempting the live write so
    // a failed write reconnects with the new accounts instead of the old set.
    *watch_set_state.write().await = replacement.clone();
    *current_watch_set = replacement.clone();
    *request = replacement_request.clone();

    write(replacement_request).await
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
            source_commitment: CONFIRMED_COMMITMENT,
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

    #[tokio::test]
    async fn subscription_replacement_waits_for_source_acknowledgement() {
        let (tx, mut rx) = mpsc::unbounded_channel();
        let handle = LaserstreamSubscriptionUpdateHandle { tx };
        let replacement = SubscriptionWatchSet {
            balance_sweep_accounts: vec!["11111111111111111111111111111111".to_owned()],
            earn_vaults: Vec::new(),
        };
        let replace_task = tokio::spawn(async move { handle.replace(replacement).await });

        tokio::task::yield_now().await;
        assert!(!replace_task.is_finished());
        let pending = rx.recv().await.expect("replacement was queued");
        assert!(!replace_task.is_finished());
        pending.accepted.send(()).unwrap();
        replace_task.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn subscription_replacement_failed_write_is_retained_for_reconnect() {
        let legacy_account: Pubkey = "11111111111111111111111111111111".parse().unwrap();
        let policy_account = "AddressLookupTab1e1111111111111111111111111";
        let mut replacement = SubscriptionWatchSet {
            balance_sweep_accounts: Vec::new(),
            earn_vaults: vec![EarnVaultWatch {
                environment: "test".to_owned(),
                settings: "Vote111111111111111111111111111111111111111".to_owned(),
                wallet: "Stake11111111111111111111111111111111111111".to_owned(),
                vault: "Config1111111111111111111111111111111111111".to_owned(),
                vault_index: 1,
                accounts: vec![EarnWatchAccount {
                    pubkey: policy_account.to_owned(),
                    role: "policy".to_owned(),
                }],
            }],
        };
        let mut current_watch_set = SubscriptionWatchSet {
            balance_sweep_accounts: vec![legacy_account.to_string()],
            earn_vaults: Vec::new(),
        };
        let mut reconnect_request = build_multi_channel_subscribe_request(&current_watch_set, 10);
        let shared_state = Arc::new(RwLock::new(current_watch_set.clone()));

        let result = stage_and_write_subscription_replacement(
            &mut replacement,
            &[legacy_account],
            10,
            &mut current_watch_set,
            &mut reconnect_request,
            &shared_state,
            |_| async { Err::<(), _>("simulated live write failure") },
        )
        .await;

        assert_eq!(result, Err("simulated live write failure"));
        assert_eq!(current_watch_set, replacement);
        assert_eq!(*shared_state.read().await, replacement);
        assert_eq!(
            reconnect_request.accounts[EARN_POLICY_ACCOUNTS].account,
            vec![policy_account.to_owned()]
        );
    }
}
