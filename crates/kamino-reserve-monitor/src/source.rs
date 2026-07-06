use std::{
    collections::HashMap,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};

use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use chrono::{DateTime, Utc};
use futures_util::{Stream, StreamExt};
use helius_laserstream::{
    grpc::{
        subscribe_update::UpdateOneof, CommitmentLevel, SubscribeRequest,
        SubscribeRequestFilterAccounts, SubscribeUpdate,
    },
    subscribe, LaserstreamConfig,
};
use solana_account_decoder::{UiAccount, UiAccountData, UiAccountEncoding};
use solana_client::rpc_config::RpcAccountInfoConfig;
use solana_client::rpc_response::Response;
use solana_pubsub_client::nonblocking::pubsub_client::PubsubClient;
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey};
use tokio::{
    sync::mpsc,
    task::{JoinHandle, JoinSet},
    time::{self, Instant},
};

type AccountNotification = Response<UiAccount>;

const SUBSCRIPTION_READ_INTERVAL: Duration = Duration::from_millis(500);
const WEBSOCKET_CONNECT_TIMEOUT: Duration = Duration::from_secs(20);
const SUBSCRIPTION_CLEANUP_TIMEOUT: Duration = Duration::from_secs(5);
pub const LASERSTREAM_SOURCE: &str = "laserstream_grpc";
pub const WEBSOCKET_SOURCE: &str = "websocket";
pub const CONFIRMED_COMMITMENT: &str = "confirmed";

#[derive(Clone, Copy, Debug)]
pub struct SubscriptionConfig {
    pub max_reconnect_attempts: usize,
    pub reconnect_base_delay: Duration,
    pub reconnect_max_delay: Duration,
    pub heartbeat_interval: Duration,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct UpdateSourceMetadata {
    pub source: &'static str,
    pub source_commitment: &'static str,
}

#[derive(Debug)]
pub enum AccountUpdateEvent {
    Connecting {
        reserve: Pubkey,
        attempt: usize,
    },
    Connected {
        reserve: Pubkey,
        attempt: usize,
    },
    AccountUpdate {
        metadata: UpdateSourceMetadata,
        reserve: Pubkey,
        slot: u64,
        owner: String,
        data: Vec<u8>,
        received_at: DateTime<Utc>,
        received_instant: Instant,
    },
    Heartbeat {
        reserve: Pubkey,
    },
    Reconnecting {
        reserve: Pubkey,
        attempt: usize,
        backoff: Duration,
        from_slot: Option<u64>,
        last_seen_slot: Option<u64>,
        error: String,
    },
    Failed {
        reserve: Pubkey,
        attempts: usize,
        error: String,
    },
    Stopped {
        reserve: Pubkey,
    },
}

pub trait AccountUpdateSource {
    fn spawn(
        self,
        reserves: Vec<Pubkey>,
        tx: mpsc::UnboundedSender<AccountUpdateEvent>,
        running: Arc<AtomicBool>,
    ) -> JoinHandle<()>;
}

#[derive(Debug, Clone)]
pub struct RpcWebsocketAccountUpdateSource {
    pub ws_url: String,
    pub config: SubscriptionConfig,
}

impl AccountUpdateSource for RpcWebsocketAccountUpdateSource {
    fn spawn(
        self,
        reserves: Vec<Pubkey>,
        tx: mpsc::UnboundedSender<AccountUpdateEvent>,
        running: Arc<AtomicBool>,
    ) -> JoinHandle<()> {
        tokio::spawn(async move {
            subscription_batch_loop(self.ws_url, reserves, self.config, tx, running).await;
        })
    }
}

#[derive(Debug, Clone)]
pub struct LaserstreamAccountUpdateSource {
    pub endpoint: String,
    pub api_key: String,
    pub initial_from_slot: u64,
    pub replay_overlap_slots: u64,
    pub config: SubscriptionConfig,
}

impl AccountUpdateSource for LaserstreamAccountUpdateSource {
    fn spawn(
        self,
        reserves: Vec<Pubkey>,
        tx: mpsc::UnboundedSender<AccountUpdateEvent>,
        running: Arc<AtomicBool>,
    ) -> JoinHandle<()> {
        tokio::spawn(async move {
            run_laserstream_subscription(self, reserves, tx, running).await;
        })
    }
}

pub fn build_laserstream_subscribe_request(
    reserves: &[Pubkey],
    from_slot: u64,
) -> SubscribeRequest {
    SubscribeRequest {
        accounts: HashMap::from([(
            "kamino_reserves".to_string(),
            SubscribeRequestFilterAccounts {
                account: reserves.iter().map(ToString::to_string).collect(),
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

async fn run_laserstream_subscription(
    source: LaserstreamAccountUpdateSource,
    reserves: Vec<Pubkey>,
    tx: mpsc::UnboundedSender<AccountUpdateEvent>,
    running: Arc<AtomicBool>,
) {
    let mut reconnect_attempts = 0usize;
    let mut last_seen_slot = None;
    while running.load(Ordering::Relaxed) {
        let attempt = reconnect_attempts + 1;
        let from_slot = laserstream_reconnect_from_slot(
            source.initial_from_slot,
            source.replay_overlap_slots,
            last_seen_slot,
        );
        for reserve in &reserves {
            if !send_event(
                &tx,
                AccountUpdateEvent::Connecting {
                    reserve: *reserve,
                    attempt,
                },
            ) {
                return;
            }
        }

        match run_laserstream_attempt(
            &source,
            &reserves,
            attempt,
            from_slot,
            &mut last_seen_slot,
            &tx,
            &running,
        )
        .await
        {
            Ok(()) => break,
            Err(error) => {
                if !running.load(Ordering::Relaxed) {
                    break;
                }
                if error.is_laserstream_replay_expired() {
                    fail_batch_immediately(
                        &reserves,
                        attempt,
                        format!(
                            "{}; failing worker so Render restarts with a fresh seed slot",
                            error.message
                        ),
                        &tx,
                    );
                    break;
                }
                if error.reached_connected {
                    reconnect_attempts = 0;
                }
                reconnect_attempts += 1;
                let next_from_slot = laserstream_reconnect_from_slot(
                    source.initial_from_slot,
                    source.replay_overlap_slots,
                    last_seen_slot,
                );
                if !schedule_batch_reconnect_or_fail(
                    &reserves,
                    reconnect_attempts,
                    error.message,
                    source.config,
                    Some((next_from_slot, last_seen_slot)),
                    &tx,
                    &running,
                )
                .await
                {
                    break;
                }
            }
        }
    }

    for reserve in &reserves {
        let _ = send_event(&tx, AccountUpdateEvent::Stopped { reserve: *reserve });
    }
}

async fn run_laserstream_attempt(
    source: &LaserstreamAccountUpdateSource,
    reserves: &[Pubkey],
    attempt: usize,
    from_slot: u64,
    last_seen_slot: &mut Option<u64>,
    tx: &mpsc::UnboundedSender<AccountUpdateEvent>,
    running: &Arc<AtomicBool>,
) -> std::result::Result<(), SubscriptionAttemptError> {
    let request = build_laserstream_subscribe_request(reserves, from_slot);
    let config = LaserstreamConfig::new(source.endpoint.clone(), source.api_key.clone())
        .with_max_reconnect_attempts(source.config.max_reconnect_attempts as u32)
        .with_replay(true);
    let (stream, _handle) = subscribe(config, request);
    futures_util::pin_mut!(stream);

    for reserve in reserves {
        if !send_event(
            tx,
            AccountUpdateEvent::Connected {
                reserve: *reserve,
                attempt,
            },
        ) {
            return Ok(());
        }
    }

    let mut heartbeat = time::interval(source.config.heartbeat_interval);
    heartbeat.set_missed_tick_behavior(time::MissedTickBehavior::Delay);

    while running.load(Ordering::Relaxed) {
        tokio::select! {
            update = stream.next() => {
                match update {
                    Some(Ok(update)) => {
                        match forward_laserstream_update(update, tx) {
                            Ok(Some(slot)) => record_laserstream_slot(last_seen_slot, slot),
                            Ok(None) => {}
                            Err(err) => {
                                return Err(SubscriptionAttemptError::after_connected(format!("{err:#}")));
                            }
                        }
                    }
                    Some(Err(err)) => {
                        return Err(SubscriptionAttemptError::after_connected(err.to_string()));
                    }
                    None => {
                        return Err(SubscriptionAttemptError::after_connected("LaserStream stream ended"));
                    }
                }
            }
            _ = heartbeat.tick() => {
                for reserve in reserves {
                    if !send_event(tx, AccountUpdateEvent::Heartbeat { reserve: *reserve }) {
                        return Ok(());
                    }
                }
            }
        }
    }

    Ok(())
}

fn laserstream_reconnect_from_slot(
    initial_from_slot: u64,
    replay_overlap_slots: u64,
    last_seen_slot: Option<u64>,
) -> u64 {
    last_seen_slot
        .map(|slot| {
            slot.saturating_sub(replay_overlap_slots)
                .max(initial_from_slot)
        })
        .unwrap_or(initial_from_slot)
}

fn record_laserstream_slot(last_seen_slot: &mut Option<u64>, slot: u64) {
    *last_seen_slot = Some(last_seen_slot.map_or(slot, |last_seen| last_seen.max(slot)));
}

fn forward_laserstream_update(
    update: SubscribeUpdate,
    tx: &mpsc::UnboundedSender<AccountUpdateEvent>,
) -> Result<Option<u64>> {
    let Some(UpdateOneof::Account(account_update)) = update.update_oneof else {
        return Ok(None);
    };
    let account = account_update
        .account
        .context("LaserStream account update was missing account payload")?;
    let reserve = pubkey_from_laserstream_bytes(&account.pubkey, "account pubkey")?;
    let owner = pubkey_from_laserstream_bytes(&account.owner, "account owner")?;
    let received_at = Utc::now();
    send_event(
        tx,
        AccountUpdateEvent::AccountUpdate {
            metadata: UpdateSourceMetadata {
                source: LASERSTREAM_SOURCE,
                source_commitment: CONFIRMED_COMMITMENT,
            },
            reserve,
            slot: account_update.slot,
            owner: owner.to_string(),
            data: account.data,
            received_at,
            received_instant: Instant::now(),
        },
    );
    Ok(Some(account_update.slot))
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

async fn subscription_batch_loop(
    ws_url: String,
    reserves: Vec<Pubkey>,
    config: SubscriptionConfig,
    tx: mpsc::UnboundedSender<AccountUpdateEvent>,
    running: Arc<AtomicBool>,
) {
    let mut reconnect_attempts = 0usize;

    while running.load(Ordering::Relaxed) {
        let attempt = reconnect_attempts + 1;
        for reserve in &reserves {
            if !send_event(
                &tx,
                AccountUpdateEvent::Connecting {
                    reserve: *reserve,
                    attempt,
                },
            ) {
                return;
            }
        }

        match run_subscription_batch(&ws_url, &reserves, attempt, config, &tx, &running).await {
            Ok(()) => break,
            Err(error) => {
                if !running.load(Ordering::Relaxed) {
                    break;
                }
                if error.reached_connected {
                    reconnect_attempts = 0;
                }
                reconnect_attempts += 1;
                if !schedule_batch_reconnect_or_fail(
                    &reserves,
                    reconnect_attempts,
                    error.message,
                    config,
                    None,
                    &tx,
                    &running,
                )
                .await
                {
                    break;
                }
            }
        }
    }
}

#[derive(Debug)]
struct SubscriptionAttemptError {
    message: String,
    reached_connected: bool,
}

impl SubscriptionAttemptError {
    fn before_connected(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            reached_connected: false,
        }
    }

    fn after_connected(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            reached_connected: true,
        }
    }

    fn is_laserstream_replay_expired(&self) -> bool {
        self.message.contains("Requested slot")
            && self
                .message
                .contains("older than the oldest available slot")
    }
}

async fn run_subscription_batch(
    ws_url: &str,
    reserves: &[Pubkey],
    attempt: usize,
    subscription_config: SubscriptionConfig,
    tx: &mpsc::UnboundedSender<AccountUpdateEvent>,
    running: &Arc<AtomicBool>,
) -> std::result::Result<(), SubscriptionAttemptError> {
    let client = match time::timeout(WEBSOCKET_CONNECT_TIMEOUT, PubsubClient::new(ws_url)).await {
        Ok(Ok(client)) => Arc::new(client),
        Ok(Err(err)) => {
            return Err(SubscriptionAttemptError::before_connected(format!(
                "connect websocket: {err}"
            )));
        }
        Err(_) => {
            return Err(SubscriptionAttemptError::before_connected(format!(
                "connect websocket timed out after {} seconds",
                WEBSOCKET_CONNECT_TIMEOUT.as_secs()
            )));
        }
    };

    let mut join_set = JoinSet::new();
    for reserve in reserves {
        let client = Arc::clone(&client);
        let tx = tx.clone();
        let running = Arc::clone(running);
        let reserve = *reserve;
        join_set.spawn(async move {
            let result = run_subscription_on_client(
                client,
                reserve,
                attempt,
                subscription_config,
                &tx,
                &running,
            )
            .await;
            (reserve, result)
        });
    }

    let mut batch_result = Ok(());
    while let Some(joined) = join_set.join_next().await {
        match joined {
            Ok((_reserve, Ok(()))) => {}
            Ok((reserve, Err(err))) => {
                join_set.shutdown().await;
                batch_result = Err(SubscriptionAttemptError {
                    message: format!("reserve {reserve}: {}", err.message),
                    reached_connected: err.reached_connected,
                });
                break;
            }
            Err(err) => {
                join_set.shutdown().await;
                batch_result = Err(SubscriptionAttemptError::before_connected(format!(
                    "subscription task failed: {err}"
                )));
                break;
            }
        }
    }

    shutdown_shared_client(client).await;
    batch_result
}

async fn run_subscription_on_client(
    client: Arc<PubsubClient>,
    reserve: Pubkey,
    attempt: usize,
    config: SubscriptionConfig,
    tx: &mpsc::UnboundedSender<AccountUpdateEvent>,
    running: &Arc<AtomicBool>,
) -> std::result::Result<(), SubscriptionAttemptError> {
    let rpc_config = RpcAccountInfoConfig {
        encoding: Some(UiAccountEncoding::Base64),
        commitment: Some(CommitmentConfig::confirmed()),
        ..RpcAccountInfoConfig::default()
    };

    match client.account_subscribe(&reserve, Some(rpc_config)).await {
        Ok((mut receiver, unsubscribe)) => {
            tracing::debug!(%reserve, attempt, "subscribed to reserve account");
            let read_result = if send_event(tx, AccountUpdateEvent::Connected { reserve, attempt })
            {
                read_subscription(
                    reserve,
                    &mut receiver,
                    tx,
                    config.heartbeat_interval,
                    running,
                )
                .await
            } else {
                Ok(())
            };

            if time::timeout(SUBSCRIPTION_CLEANUP_TIMEOUT, unsubscribe())
                .await
                .is_err()
            {
                tracing::warn!(%reserve, "timed out while unsubscribing account subscription");
            }
            drop(receiver);

            read_result.map_err(SubscriptionAttemptError::after_connected)
        }
        Err(err) => Err(SubscriptionAttemptError::before_connected(format!(
            "subscribe to reserve account {reserve}: {err}"
        ))),
    }
}

async fn read_subscription<S>(
    reserve: Pubkey,
    receiver: &mut S,
    tx: &mpsc::UnboundedSender<AccountUpdateEvent>,
    heartbeat_interval: Duration,
    running: &Arc<AtomicBool>,
) -> std::result::Result<(), String>
where
    S: Stream<Item = AccountNotification> + Unpin,
{
    let mut last_heartbeat = Instant::now();

    while running.load(Ordering::Relaxed) {
        match time::timeout(SUBSCRIPTION_READ_INTERVAL, receiver.next()).await {
            Ok(Some(notification)) => {
                last_heartbeat = Instant::now();
                match decode_ui_account_data(&notification.value) {
                    Ok(data) => {
                        if !send_event(
                            tx,
                            AccountUpdateEvent::AccountUpdate {
                                metadata: UpdateSourceMetadata {
                                    source: WEBSOCKET_SOURCE,
                                    source_commitment: CONFIRMED_COMMITMENT,
                                },
                                reserve,
                                slot: notification.context.slot,
                                owner: notification.value.owner.clone(),
                                data,
                                received_at: Utc::now(),
                                received_instant: Instant::now(),
                            },
                        ) {
                            return Ok(());
                        }
                    }
                    Err(err) => return Err(format!("decode account update: {err}")),
                }
            }
            Ok(None) => return Err("subscription stream ended".to_string()),
            Err(_) => {
                if last_heartbeat.elapsed() >= heartbeat_interval {
                    if !send_event(tx, AccountUpdateEvent::Heartbeat { reserve }) {
                        return Ok(());
                    }
                    last_heartbeat = Instant::now();
                }
            }
        }
    }
    Ok(())
}

async fn schedule_batch_reconnect_or_fail(
    reserves: &[Pubkey],
    attempts: usize,
    error: String,
    config: SubscriptionConfig,
    laserstream_cursor: Option<(u64, Option<u64>)>,
    tx: &mpsc::UnboundedSender<AccountUpdateEvent>,
    running: &Arc<AtomicBool>,
) -> bool {
    if attempts > config.max_reconnect_attempts {
        for reserve in reserves {
            let _ = send_event(
                tx,
                AccountUpdateEvent::Failed {
                    reserve: *reserve,
                    attempts,
                    error: error.clone(),
                },
            );
        }
        return false;
    }

    let backoff = reconnect_backoff(attempts, config);
    for reserve in reserves {
        if !send_event(
            tx,
            AccountUpdateEvent::Reconnecting {
                reserve: *reserve,
                attempt: attempts,
                backoff,
                from_slot: laserstream_cursor.map(|(from_slot, _)| from_slot),
                last_seen_slot: laserstream_cursor.and_then(|(_, last_seen_slot)| last_seen_slot),
                error: error.clone(),
            },
        ) {
            return false;
        }
    }
    let mut slept = Duration::ZERO;
    while slept < backoff {
        if !running.load(Ordering::Relaxed) {
            return false;
        }
        let tick = Duration::from_millis(100).min(backoff - slept);
        time::sleep(tick).await;
        slept += tick;
    }
    true
}

fn fail_batch_immediately(
    reserves: &[Pubkey],
    attempts: usize,
    error: String,
    tx: &mpsc::UnboundedSender<AccountUpdateEvent>,
) {
    for reserve in reserves {
        let _ = send_event(
            tx,
            AccountUpdateEvent::Failed {
                reserve: *reserve,
                attempts,
                error: error.clone(),
            },
        );
    }
}

fn reconnect_backoff(attempts: usize, config: SubscriptionConfig) -> Duration {
    let shift = attempts.saturating_sub(1).min(16) as u32;
    let multiplier = 1_u32 << shift;
    config
        .reconnect_base_delay
        .saturating_mul(multiplier)
        .min(config.reconnect_max_delay)
}

async fn shutdown_shared_client(client: Arc<PubsubClient>) {
    match Arc::try_unwrap(client) {
        Ok(client) => {
            let _ = time::timeout(SUBSCRIPTION_CLEANUP_TIMEOUT, client.shutdown()).await;
        }
        Err(_) => tracing::warn!("websocket client still had references during shutdown"),
    }
}

fn send_event(tx: &mpsc::UnboundedSender<AccountUpdateEvent>, event: AccountUpdateEvent) -> bool {
    tx.send(event).is_ok()
}

fn decode_ui_account_data(account: &UiAccount) -> Result<Vec<u8>> {
    match &account.data {
        UiAccountData::Binary(encoded, _) | UiAccountData::LegacyBinary(encoded) => BASE64_STANDARD
            .decode(encoded)
            .context("decode base64 account data"),
        UiAccountData::Json(_) => bail!("expected base64 account data, got JSON encoding"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use helius_laserstream::grpc::{SubscribeUpdateAccount, SubscribeUpdateAccountInfo};

    #[test]
    fn laserstream_reconnect_from_slot_keeps_initial_before_updates() {
        assert_eq!(laserstream_reconnect_from_slot(968, 32, None), 968);
    }

    #[test]
    fn laserstream_reconnect_from_slot_advances_with_overlap() {
        assert_eq!(laserstream_reconnect_from_slot(968, 32, Some(1_050)), 1_018);
    }

    #[test]
    fn laserstream_reconnect_from_slot_saturates_near_zero() {
        assert_eq!(laserstream_reconnect_from_slot(0, 32, Some(10)), 0);
    }

    #[test]
    fn laserstream_reconnect_from_slot_stays_above_initial_floor() {
        assert_eq!(laserstream_reconnect_from_slot(968, 32, Some(990)), 968);
    }

    #[test]
    fn records_highest_laserstream_account_update_slot() {
        let mut last_seen_slot = None;

        record_laserstream_slot(&mut last_seen_slot, 50);
        record_laserstream_slot(&mut last_seen_slot, 42);
        record_laserstream_slot(&mut last_seen_slot, 64);

        assert_eq!(last_seen_slot, Some(64));
    }

    #[test]
    fn forward_laserstream_update_returns_observed_account_slot() {
        let (tx, mut rx) = mpsc::unbounded_channel();
        let reserve = Pubkey::new_unique();
        let owner = Pubkey::new_unique();
        let slot = 42;
        let data = vec![1, 2, 3];
        let update = SubscribeUpdate {
            filters: Vec::new(),
            created_at: None,
            update_oneof: Some(UpdateOneof::Account(SubscribeUpdateAccount {
                account: Some(SubscribeUpdateAccountInfo {
                    pubkey: reserve.to_bytes().to_vec(),
                    lamports: 0,
                    owner: owner.to_bytes().to_vec(),
                    executable: false,
                    rent_epoch: 0,
                    data: data.clone(),
                    write_version: 0,
                    txn_signature: None,
                }),
                slot,
                is_startup: false,
            })),
        };

        let observed_slot = forward_laserstream_update(update, &tx).unwrap();

        assert_eq!(observed_slot, Some(slot));
        match rx.try_recv().unwrap() {
            AccountUpdateEvent::AccountUpdate {
                metadata,
                reserve: event_reserve,
                slot: event_slot,
                owner: event_owner,
                data: event_data,
                ..
            } => {
                assert_eq!(metadata.source, LASERSTREAM_SOURCE);
                assert_eq!(event_reserve, reserve);
                assert_eq!(event_slot, slot);
                assert_eq!(event_owner, owner.to_string());
                assert_eq!(event_data, data);
            }
            event => panic!("unexpected event: {event:?}"),
        }
    }

    #[test]
    fn detects_laserstream_replay_retention_error() {
        let error = SubscriptionAttemptError::after_connected(
            "gRPC status error: code: 'Operation was attempted past the valid range', message: \
             \"Requested slot 425513755 is older than the oldest available slot 426585224. \
             Please request a more recent slot.\"",
        );

        assert!(error.is_laserstream_replay_expired());
    }

    #[test]
    fn does_not_treat_generic_stream_errors_as_replay_expiry() {
        let error =
            SubscriptionAttemptError::after_connected("gRPC status error: transport closed");

        assert!(!error.is_laserstream_replay_expired());
    }
}
