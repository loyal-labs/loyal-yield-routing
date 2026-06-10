use std::{
    collections::{HashMap, HashSet},
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};

use anyhow::{bail, Context, Result};
pub use balance_sweep_ata_observations::{
    AtaObservationSink, BalanceSweepAtaObservation, BalanceSweepAtaObservationEvent,
    ObservationInsertOutcome, TimescaleAtaConfig, TimescaleAtaObservationSink,
};
use chrono::{DateTime, Utc};
use futures_util::StreamExt;
use helius_laserstream::{
    grpc::{
        subscribe_update::UpdateOneof, CommitmentLevel, SubscribeRequest,
        SubscribeRequestFilterAccounts, SubscribeUpdate,
    },
    subscribe, LaserstreamConfig,
};
use loyal_actions::USDC_MINT;
use loyal_yield_orchestrator::{BalanceSweepTarget, BalanceSweepTargetId};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};
use solana_account_decoder::{UiAccount, UiAccountData, UiAccountEncoding};
use solana_client::{rpc_client::RpcClient, rpc_config::RpcAccountInfoConfig};
use solana_program::program_pack::Pack;
use solana_pubsub_client::nonblocking::pubsub_client::PubsubClient;
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey};
use tokio::{
    sync::mpsc,
    task::{JoinHandle, JoinSet},
    time,
};

pub mod earn_apy;

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
            wallet_usdc_ata: value.wallet_usdc_ata.parse()?,
            vault_pubkey: value.vault_pubkey.clone(),
            vault_usdc_ata: value.vault_usdc_ata.parse()?,
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
        slot: u64,
        owner: Pubkey,
        data: Vec<u8>,
        source: &'static str,
        source_commitment: &'static str,
        received_at: DateTime<Utc>,
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
}

impl AtaUpdateSource for LaserstreamAtaUpdateSource {
    fn spawn(
        self,
        accounts: Vec<Pubkey>,
        tx: mpsc::UnboundedSender<AtaUpdateEvent>,
        running: Arc<AtomicBool>,
    ) -> JoinHandle<()> {
        tokio::spawn(async move {
            run_laserstream_loop(self, accounts, tx, running).await;
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
        let fetched = rpc
            .get_multiple_accounts(&accounts)
            .with_context(|| format!("fetch {} wallet ATA seed accounts", accounts.len()))?;
        let seed_observed_slot = rpc
            .get_slot()
            .context("fetch confirmed seed observed slot")?;
        if seed_observed_slot == 0 {
            bail!("RPC seed observed slot was zero");
        }
        for (target, account) in chunk.iter().zip(fetched) {
            let Some(account) = account else {
                tracing::warn!(
                    wallet_usdc_ata = %target.wallet_usdc_ata,
                    "skipping missing wallet ATA seed account"
                );
                continue;
            };
            process_account_update(
                target,
                account.lamports,
                seed_observed_slot,
                account.owner,
                account.data,
                RPC_SEED_SOURCE,
                CONFIRMED_COMMITMENT,
                Utc::now(),
                sink,
            )
            .await?;
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
    received_at: DateTime<Utc>,
    sink: &impl AtaObservationSink,
) -> Result<ObservationInsertOutcome> {
    if owner != spl_token::id() {
        tracing::warn!(
            wallet_usdc_ata = %target.wallet_usdc_ata,
            owner = %owner,
            "skipping wallet ATA update owned by non-SPL-token program"
        );
        bail!(
            "wallet ATA {} owner is not SPL Token",
            target.wallet_usdc_ata
        );
    }
    let snapshot = decode_spl_token_account(&data)?;
    if snapshot.mint != USDC_MINT {
        tracing::warn!(
            wallet_usdc_ata = %target.wallet_usdc_ata,
            mint = %snapshot.mint,
            "skipping wallet ATA update for non-USDC mint"
        );
        bail!("wallet ATA {} mint is not USDC", target.wallet_usdc_ata);
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
        account_data_hash: hash.clone(),
        raw_account_data_base64: raw_account_data_base64.clone(),
        raw_evidence: json!({
            "lamports": lamports,
            "account_data_hash": hash,
            "raw_account_data_base64": raw_account_data_base64,
            "source": source,
            "wallet": target.wallet,
            "wallet_usdc_ata": target.wallet_usdc_ata.to_string(),
            "vault_pubkey": target.vault_pubkey,
            "vault_usdc_ata": target.vault_usdc_ata.to_string(),
        }),
        received_at,
    };
    sink.record_observation(observation).await
}

pub async fn run_event_loop(
    mut rx: mpsc::UnboundedReceiver<AtaUpdateEvent>,
    targets: HashMap<Pubkey, AtaTarget>,
    sink: impl AtaObservationSink,
    running: Arc<AtomicBool>,
) -> Result<()> {
    while running.load(Ordering::Relaxed) {
        let Some(event) = rx.recv().await else {
            break;
        };
        if let AtaUpdateEvent::AccountUpdate {
            account,
            slot,
            owner,
            data,
            source,
            source_commitment,
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
            process_account_update(
                target,
                0,
                slot,
                owner,
                data,
                source,
                source_commitment,
                received_at,
                &sink,
            )
            .await?;
        }
    }
    Ok(())
}

pub fn build_laserstream_subscribe_request(
    accounts: &[Pubkey],
    from_slot: u64,
) -> SubscribeRequest {
    SubscribeRequest {
        accounts: HashMap::from([(
            "balance_sweep_wallet_atas".to_string(),
            SubscribeRequestFilterAccounts {
                account: accounts.iter().map(ToString::to_string).collect(),
                owner: Vec::new(),
                filters: Vec::new(),
                nonempty_txn_signature: None,
            },
        )]),
        commitment: Some(CommitmentLevel::Confirmed as i32),
        accounts_data_slice: Vec::new(),
        from_slot: Some(from_slot),
        ..Default::default()
    }
}

async fn run_laserstream_loop(
    source: LaserstreamAtaUpdateSource,
    accounts: Vec<Pubkey>,
    tx: mpsc::UnboundedSender<AtaUpdateEvent>,
    running: Arc<AtomicBool>,
) {
    let request = build_laserstream_subscribe_request(&accounts, source.from_slot);
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
    let Some(UpdateOneof::Account(account_update)) = update.update_oneof else {
        return Ok(());
    };
    let account = account_update
        .account
        .context("LaserStream account update was missing account payload")?;
    let pubkey = pubkey_from_laserstream_bytes(&account.pubkey, "account pubkey")?;
    let owner = pubkey_from_laserstream_bytes(&account.owner, "account owner")?;
    let _ = tx.send(AtaUpdateEvent::AccountUpdate {
        account: pubkey,
        slot: account_update.slot,
        owner,
        data: account.data,
        source: LASERSTREAM_SOURCE,
        source_commitment: CONFIRMED_COMMITMENT,
        received_at: Utc::now(),
    });
    Ok(())
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
        slot: notification.context.slot,
        owner,
        data,
        source: WEBSOCKET_SOURCE,
        source_commitment: CONFIRMED_COMMITMENT,
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
    use futures_util::future::BoxFuture;
    use solana_program::program_pack::Pack;
    use spl_token::state::AccountState;

    struct VecSink {
        observations: std::sync::Mutex<Vec<BalanceSweepAtaObservation>>,
    }

    impl AtaObservationSink for VecSink {
        fn record_observation(
            &self,
            observation: BalanceSweepAtaObservation,
        ) -> BoxFuture<'_, Result<ObservationInsertOutcome>> {
            self.observations.lock().unwrap().push(observation);
            Box::pin(async move {
                Ok(ObservationInsertOutcome {
                    event_id: 1,
                    inserted: true,
                })
            })
        }
    }

    fn token_account_data(owner: Pubkey, amount: u64) -> Vec<u8> {
        let account = spl_token::state::Account {
            mint: USDC_MINT,
            owner,
            amount,
            delegate: Default::default(),
            state: AccountState::Initialized,
            is_native: Default::default(),
            delegated_amount: 0,
            close_authority: Default::default(),
        };
        let mut data = vec![0_u8; spl_token::state::Account::LEN];
        spl_token::state::Account::pack(account, &mut data).unwrap();
        data
    }

    #[test]
    fn decodes_spl_token_account_amount_owner_and_mint() {
        let owner = Pubkey::new_unique();
        let snapshot = decode_spl_token_account(&token_account_data(owner, 123)).unwrap();
        assert_eq!(snapshot.mint, USDC_MINT);
        assert_eq!(snapshot.owner, owner);
        assert_eq!(snapshot.amount, 123);
    }

    #[tokio::test]
    async fn account_update_writes_raw_observation_without_neon_projection() {
        let wallet = Pubkey::new_unique();
        let ata = Pubkey::new_unique();
        let target = AtaTarget {
            id: BalanceSweepTargetId(7),
            cluster: "devnet".to_owned(),
            wallet: wallet.to_string(),
            wallet_usdc_ata: ata,
            vault_pubkey: Pubkey::new_unique().to_string(),
            vault_usdc_ata: Pubkey::new_unique(),
        };
        let sink = VecSink {
            observations: std::sync::Mutex::new(Vec::new()),
        };
        process_account_update(
            &target,
            1_000_000,
            55,
            spl_token::id(),
            token_account_data(wallet, 456),
            LASERSTREAM_SOURCE,
            CONFIRMED_COMMITMENT,
            Utc::now(),
            &sink,
        )
        .await
        .unwrap();
        let observations = sink.observations.lock().unwrap();
        assert_eq!(observations.len(), 1);
        assert_eq!(observations[0].amount_raw, 456);
        assert_eq!(observations[0].target_id, BalanceSweepTargetId(7));
        assert_eq!(observations[0].account_data_hash.len(), 64);
        let decoded = {
            use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
            BASE64_STANDARD
                .decode(&observations[0].raw_account_data_base64)
                .unwrap()
        };
        assert_eq!(
            account_data_hash(&decoded),
            observations[0].account_data_hash
        );
        assert_eq!(
            observations[0].raw_evidence["wallet_usdc_ata"],
            serde_json::Value::String(ata.to_string())
        );
    }

    #[test]
    fn target_set_diff_is_empty_for_unchanged_targets() {
        let first = Pubkey::new_unique();
        let second = Pubkey::new_unique();
        let current = HashSet::from([first, second]);
        let next = HashSet::from([second, first]);

        let diff = diff_ata_target_sets(&current, &next);

        assert!(!diff.has_changes());
        assert!(diff.added.is_empty());
        assert!(diff.removed.is_empty());
    }

    #[test]
    fn target_set_diff_detects_added_ata() {
        let first = Pubkey::new_unique();
        let second = Pubkey::new_unique();
        let current = HashSet::from([first]);
        let next = HashSet::from([first, second]);

        let diff = diff_ata_target_sets(&current, &next);

        assert!(diff.has_changes());
        assert_eq!(diff.added, vec![second]);
        assert!(diff.removed.is_empty());
    }

    #[test]
    fn target_set_diff_detects_removed_ata() {
        let first = Pubkey::new_unique();
        let second = Pubkey::new_unique();
        let current = HashSet::from([first, second]);
        let next = HashSet::from([second]);

        let diff = diff_ata_target_sets(&current, &next);

        assert!(diff.has_changes());
        assert!(diff.added.is_empty());
        assert_eq!(diff.removed, vec![first]);
    }

    #[test]
    fn target_set_diff_allows_empty_target_set() {
        let removed = Pubkey::new_unique();
        let current = HashSet::from([removed]);
        let next = HashSet::new();

        let diff = diff_ata_target_sets(&current, &next);

        assert!(diff.has_changes());
        assert!(diff.added.is_empty());
        assert_eq!(diff.removed, vec![removed]);
    }

    #[tokio::test]
    async fn seed_current_balances_allows_empty_targets() {
        let sink = VecSink {
            observations: std::sync::Mutex::new(Vec::new()),
        };

        seed_current_balances("http://127.0.0.1:0", &[], &sink)
            .await
            .unwrap();

        assert!(sink.observations.lock().unwrap().is_empty());
    }

    #[test]
    fn laserstream_subscription_uses_dynamic_ata_targets() {
        let first = Pubkey::new_unique();
        let second = Pubkey::new_unique();
        let request = build_laserstream_subscribe_request(&[first, second], 123);
        let filter = request
            .accounts
            .get("balance_sweep_wallet_atas")
            .expect("balance sweep account filter");
        assert_eq!(filter.account, vec![first.to_string(), second.to_string()]);
        assert_eq!(filter.owner.len(), 0);
        assert_eq!(request.from_slot, Some(123));
        assert_eq!(request.commitment, Some(CommitmentLevel::Confirmed as i32));
    }

    #[test]
    fn laserstream_replay_slot_uses_current_slot_minus_overlap() {
        assert_eq!(laserstream_replay_from_slot(10_000, 32), 9_968);
        assert_eq!(laserstream_replay_from_slot(10, 32), 0);
    }

    #[test]
    fn reconnect_backoff_is_exponential_and_capped() {
        let config = SubscriptionConfig {
            max_reconnect_attempts: 10,
            reconnect_base_delay: Duration::from_millis(500),
            reconnect_max_delay: Duration::from_secs(2),
            heartbeat_interval: Duration::from_secs(15),
        };

        assert_eq!(reconnect_backoff(config, 1), Duration::from_millis(500));
        assert_eq!(reconnect_backoff(config, 2), Duration::from_secs(1));
        assert_eq!(reconnect_backoff(config, 3), Duration::from_secs(2));
        assert_eq!(reconnect_backoff(config, 8), Duration::from_secs(2));
    }
}
