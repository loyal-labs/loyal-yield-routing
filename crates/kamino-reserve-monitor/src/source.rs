use std::{
    collections::HashMap,
    future::Future,
    pin::Pin,
    sync::{
        atomic::{AtomicBool, AtomicU64, Ordering},
        Arc,
    },
    time::Duration,
};

use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use chrono::{DateTime, Utc};
use futures_util::{Sink, SinkExt, Stream, StreamExt};
use laserstream_core_client::{ClientTlsConfig, GeyserGrpcClient};
use laserstream_core_proto::geyser::{
    subscribe_update::UpdateOneof, CommitmentLevel, SubscribeRequest,
    SubscribeRequestFilterAccounts, SubscribeRequestPing, SubscribeUpdate,
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
pub use loyal_kamino_data::source_metadata::{UpdateSourceMetadata, CONFIRMED_COMMITMENT};

pub const DEFAULT_ACCOUNT_EVENT_CHANNEL_CAPACITY: usize = 256;

#[derive(Clone, Debug)]
pub struct AccountEventSender {
    tx: mpsc::Sender<AccountUpdateEvent>,
    backpressure_events: Arc<AtomicU64>,
}

impl AccountEventSender {
    pub fn channel(capacity: usize) -> (Self, mpsc::Receiver<AccountUpdateEvent>) {
        let (tx, rx) = mpsc::channel(capacity);
        (
            Self {
                tx,
                backpressure_events: Arc::new(AtomicU64::new(0)),
            },
            rx,
        )
    }

    async fn send(&self, event: AccountUpdateEvent) -> bool {
        if self.tx.capacity() == 0 {
            let blocked = self
                .backpressure_events
                .fetch_add(1, Ordering::Relaxed)
                .saturating_add(1);
            if blocked.is_power_of_two() {
                tracing::warn!(
                    blocked_sends = blocked,
                    channel_capacity = self.tx.max_capacity(),
                    "reserve event channel is full; applying source backpressure"
                );
            }
        }
        self.tx.send(event).await.is_ok()
    }

    pub fn depth(&self) -> usize {
        self.tx.max_capacity().saturating_sub(self.tx.capacity())
    }

    pub fn capacity(&self) -> usize {
        self.tx.max_capacity()
    }

    pub fn backpressure_events(&self) -> u64 {
        self.backpressure_events.load(Ordering::Relaxed)
    }
}

#[derive(Clone, Debug)]
pub struct DurableReplayCursor {
    durable_slot: Arc<AtomicU64>,
    overlap_slots: u64,
}

impl DurableReplayCursor {
    pub fn new(seed_slot: u64, overlap_slots: u64) -> Self {
        Self {
            durable_slot: Arc::new(AtomicU64::new(seed_slot)),
            overlap_slots,
        }
    }

    pub fn replay_from_slot(&self) -> u64 {
        self.durable_slot().saturating_sub(self.overlap_slots)
    }

    pub fn durable_slot(&self) -> u64 {
        self.durable_slot.load(Ordering::Acquire)
    }

    pub fn advance_after_durable_write(&self, slot: u64) {
        self.durable_slot.fetch_max(slot, Ordering::AcqRel);
    }
}

#[derive(Clone, Copy, Debug)]
pub struct SubscriptionConfig {
    pub max_reconnect_attempts: usize,
    pub reconnect_base_delay: Duration,
    pub reconnect_max_delay: Duration,
    pub heartbeat_interval: Duration,
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
        tx: AccountEventSender,
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
        tx: AccountEventSender,
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
    pub replay_cursor: DurableReplayCursor,
    pub config: SubscriptionConfig,
}

impl AccountUpdateSource for LaserstreamAccountUpdateSource {
    fn spawn(
        self,
        reserves: Vec<Pubkey>,
        tx: AccountEventSender,
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
    tx: AccountEventSender,
    running: Arc<AtomicBool>,
) {
    run_laserstream_subscription_with_attempt(
        source,
        reserves,
        tx,
        running,
        |source, reserves, attempt, replay_from_slot, tx, running| {
            Box::pin(run_laserstream_attempt(
                source,
                reserves,
                attempt,
                replay_from_slot,
                tx,
                running,
            ))
        },
    )
    .await;
}

type LaserstreamAttemptFuture<'a> =
    Pin<Box<dyn Future<Output = std::result::Result<(), SubscriptionAttemptError>> + Send + 'a>>;

async fn run_laserstream_subscription_with_attempt<F>(
    source: LaserstreamAccountUpdateSource,
    reserves: Vec<Pubkey>,
    tx: AccountEventSender,
    running: Arc<AtomicBool>,
    mut run_attempt: F,
) where
    F: for<'a> FnMut(
        &'a LaserstreamAccountUpdateSource,
        &'a [Pubkey],
        usize,
        u64,
        &'a AccountEventSender,
        &'a Arc<AtomicBool>,
    ) -> LaserstreamAttemptFuture<'a>,
{
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
            )
            .await
            {
                return;
            }
        }

        let replay_from_slot = source.replay_cursor.replay_from_slot();
        match run_attempt(&source, &reserves, attempt, replay_from_slot, &tx, &running).await {
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
                    )
                    .await;
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
                    source.config,
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
        let _ = send_event(&tx, AccountUpdateEvent::Stopped { reserve: *reserve }).await;
    }
}

async fn run_laserstream_attempt(
    source: &LaserstreamAccountUpdateSource,
    reserves: &[Pubkey],
    attempt: usize,
    replay_from_slot: u64,
    tx: &AccountEventSender,
    running: &Arc<AtomicBool>,
) -> std::result::Result<(), SubscriptionAttemptError> {
    let request = build_laserstream_subscribe_request(reserves, replay_from_slot);
    tracing::info!(
        replay_from_slot,
        attempt,
        "starting LaserStream subscription attempt"
    );
    // Use one physical subscription per outer attempt. The Helius convenience
    // wrapper reconnects internally from its receive-side slot, which can move
    // ahead of Timescale. Returning every stream end/error to the outer loop
    // keeps replay controlled by DurableReplayCursor.
    let builder = GeyserGrpcClient::build_from_shared(source.endpoint.clone())
        .map_err(|error| SubscriptionAttemptError::before_connected(error.to_string()))?
        .x_token(Some(source.api_key.clone()))
        .map_err(|error| SubscriptionAttemptError::before_connected(error.to_string()))?
        .tls_config(ClientTlsConfig::new().with_enabled_roots())
        .map_err(|error| SubscriptionAttemptError::before_connected(error.to_string()))?
        .connect_timeout(WEBSOCKET_CONNECT_TIMEOUT)
        .timeout(Duration::from_secs(30))
        .http2_keep_alive_interval(Duration::from_secs(30))
        .keep_alive_timeout(Duration::from_secs(5))
        .keep_alive_while_idle(true)
        .initial_stream_window_size(Some(4 * 1024 * 1024))
        .initial_connection_window_size(Some(8 * 1024 * 1024))
        .http2_adaptive_window(true)
        .tcp_nodelay(true)
        .buffer_size(Some(64 * 1024))
        .max_decoding_message_size(1_000_000_000)
        .max_encoding_message_size(32_000_000);
    let mut client = builder
        .connect()
        .await
        .map_err(|error| SubscriptionAttemptError::before_connected(error.to_string()))?;
    let (subscription_sender, stream) = client
        .subscribe_with_request(Some(request))
        .await
        .map_err(|error| SubscriptionAttemptError::before_connected(error.to_string()))?;

    for reserve in reserves {
        if !send_event(
            tx,
            AccountUpdateEvent::Connected {
                reserve: *reserve,
                attempt,
            },
        )
        .await
        {
            return Ok(());
        }
    }

    let result = consume_laserstream_stream(
        stream,
        subscription_sender,
        reserves,
        source.config.heartbeat_interval,
        tx,
        running,
    )
    .await;
    result
}

async fn consume_laserstream_stream<S, E, W, WE>(
    stream: S,
    mut subscription_sender: W,
    reserves: &[Pubkey],
    heartbeat_interval: Duration,
    tx: &AccountEventSender,
    running: &Arc<AtomicBool>,
) -> std::result::Result<(), SubscriptionAttemptError>
where
    S: Stream<Item = std::result::Result<SubscribeUpdate, E>> + Send,
    E: std::fmt::Display,
    W: Sink<SubscribeRequest, Error = WE> + Unpin + Send,
    WE: std::fmt::Display,
{
    futures_util::pin_mut!(stream);
    let mut heartbeat = time::interval(heartbeat_interval);
    heartbeat.set_missed_tick_behavior(time::MissedTickBehavior::Delay);
    let mut ping_interval = time::interval(Duration::from_secs(30));
    ping_interval.set_missed_tick_behavior(time::MissedTickBehavior::Delay);
    ping_interval.tick().await;
    let mut ping_id = 0_i32;

    while running.load(Ordering::Relaxed) {
        tokio::select! {
            update = stream.next() => {
                match update {
                    Some(Ok(update)) if matches!(&update.update_oneof, Some(UpdateOneof::Ping(_))) => {
                        subscription_sender
                            .send(SubscribeRequest {
                                ping: Some(SubscribeRequestPing { id: 1 }),
                                ..Default::default()
                            })
                            .await
                            .map_err(|error| SubscriptionAttemptError::after_connected(error.to_string()))?;
                    }
                    Some(Ok(update)) if matches!(&update.update_oneof, Some(UpdateOneof::Pong(_))) => {}
                    Some(Ok(update)) => {
                        if let Err(err) = forward_laserstream_update(update, tx).await {
                            return Err(SubscriptionAttemptError::after_connected(format!("{err:#}")));
                        }
                    }
                    Some(Err(err)) => {
                        return Err(SubscriptionAttemptError::after_connected(err.to_string()));
                    }
                    None => {
                        return Err(laserstream_stream_ended_error());
                    }
                }
            }
            _ = ping_interval.tick() => {
                ping_id = ping_id.wrapping_add(1);
                subscription_sender
                    .send(SubscribeRequest {
                        ping: Some(SubscribeRequestPing { id: ping_id }),
                        ..Default::default()
                    })
                    .await
                    .map_err(|error| SubscriptionAttemptError::after_connected(error.to_string()))?;
            }
            _ = heartbeat.tick() => {
                for reserve in reserves {
                    if !send_event(tx, AccountUpdateEvent::Heartbeat { reserve: *reserve }).await {
                        return Ok(());
                    }
                }
            }
        }
    }

    Ok(())
}

fn laserstream_stream_ended_error() -> SubscriptionAttemptError {
    SubscriptionAttemptError::after_connected("LaserStream stream ended")
}

async fn forward_laserstream_update(
    update: SubscribeUpdate,
    tx: &AccountEventSender,
) -> Result<()> {
    let Some(UpdateOneof::Account(account_update)) = update.update_oneof else {
        return Ok(());
    };
    let account = account_update
        .account
        .context("LaserStream account update was missing account payload")?;
    let reserve = pubkey_from_laserstream_bytes(&account.pubkey, "account pubkey")?;
    let owner = pubkey_from_laserstream_bytes(&account.owner, "account owner")?;
    let received_at = Utc::now();
    let _ = send_event(
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
    )
    .await;
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

async fn subscription_batch_loop(
    ws_url: String,
    reserves: Vec<Pubkey>,
    config: SubscriptionConfig,
    tx: AccountEventSender,
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
            )
            .await
            {
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
    tx: &AccountEventSender,
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
    tx: &AccountEventSender,
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
            let read_result =
                if send_event(tx, AccountUpdateEvent::Connected { reserve, attempt }).await {
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
    tx: &AccountEventSender,
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
                        )
                        .await
                        {
                            return Ok(());
                        }
                    }
                    Err(err) => return Err(format!("decode account update: {err}")),
                }
            }
            Ok(None) => return Err("subscription stream ended".to_string()),
            Err(_) => {
                if last_heartbeat.elapsed() >= heartbeat_interval {
                    if !send_event(tx, AccountUpdateEvent::Heartbeat { reserve }).await {
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
    tx: &AccountEventSender,
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
            )
            .await;
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
                error: error.clone(),
            },
        )
        .await
        {
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

async fn fail_batch_immediately(
    reserves: &[Pubkey],
    attempts: usize,
    error: String,
    tx: &AccountEventSender,
) {
    for reserve in reserves {
        let _ = send_event(
            tx,
            AccountUpdateEvent::Failed {
                reserve: *reserve,
                attempts,
                error: error.clone(),
            },
        )
        .await;
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

async fn send_event(tx: &AccountEventSender, event: AccountUpdateEvent) -> bool {
    tx.send(event).await
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
    use std::sync::Mutex;

    use super::*;

    #[tokio::test]
    async fn bounded_channel_applies_backpressure_without_dropping_fifo_events() {
        let reserve = Pubkey::new_unique();
        let (tx, mut rx) = AccountEventSender::channel(1);
        assert!(tx.send(AccountUpdateEvent::Heartbeat { reserve }).await);

        let second_reserve = Pubkey::new_unique();
        let blocked_tx = tx.clone();
        let blocked_send = tokio::spawn(async move {
            blocked_tx
                .send(AccountUpdateEvent::Heartbeat {
                    reserve: second_reserve,
                })
                .await
        });
        tokio::task::yield_now().await;

        assert!(!blocked_send.is_finished());
        assert_eq!(tx.depth(), 1);
        assert_eq!(tx.backpressure_events(), 1);
        assert!(matches!(
            rx.recv().await,
            Some(AccountUpdateEvent::Heartbeat { reserve: received }) if received == reserve
        ));
        assert!(blocked_send.await.expect("blocked sender task"));
        assert!(matches!(
            rx.recv().await,
            Some(AccountUpdateEvent::Heartbeat { reserve: received }) if received == second_reserve
        ));
    }

    #[test]
    fn replay_cursor_advances_only_when_persistence_side_marks_a_slot_durable() {
        let cursor = DurableReplayCursor::new(100, 32);
        let source_view = cursor.clone();

        assert_eq!(source_view.replay_from_slot(), 68);
        assert_eq!(source_view.durable_slot(), 100);

        cursor.advance_after_durable_write(140);
        assert_eq!(source_view.durable_slot(), 140);
        assert_eq!(source_view.replay_from_slot(), 108);
        cursor.advance_after_durable_write(120);
        assert_eq!(source_view.durable_slot(), 140);
    }

    #[tokio::test]
    async fn clean_stream_closure_reconnects_from_latest_durable_cursor() {
        let reserve = Pubkey::new_unique();
        let cursor = DurableReplayCursor::new(100, 32);
        let source = LaserstreamAccountUpdateSource {
            endpoint: "https://example.invalid".to_string(),
            api_key: "unused-in-test".to_string(),
            replay_cursor: cursor.clone(),
            config: SubscriptionConfig {
                max_reconnect_attempts: 2,
                reconnect_base_delay: Duration::from_millis(1),
                reconnect_max_delay: Duration::from_millis(1),
                heartbeat_interval: Duration::from_secs(60),
            },
        };
        let running = Arc::new(AtomicBool::new(true));
        let (tx, _rx) = AccountEventSender::channel(16);
        let replay_starts = Arc::new(Mutex::new(Vec::new()));
        let observed_replay_starts = Arc::clone(&replay_starts);
        let test_cursor = cursor.clone();

        run_laserstream_subscription_with_attempt(
            source,
            vec![reserve],
            tx,
            Arc::clone(&running),
            move |_source, reserves, attempt, replay_from_slot, tx, running| {
                let replay_starts = Arc::clone(&observed_replay_starts);
                let test_cursor = test_cursor.clone();
                let reserves = reserves.to_vec();
                let tx = tx.clone();
                let running = Arc::clone(running);
                Box::pin(async move {
                    replay_starts
                        .lock()
                        .expect("replay start lock")
                        .push(replay_from_slot);
                    if attempt == 1 {
                        let closed_stream = futures_util::stream::empty::<
                            std::result::Result<SubscribeUpdate, String>,
                        >();
                        let closure_error = consume_laserstream_stream(
                            closed_stream,
                            futures_util::sink::drain(),
                            &reserves,
                            Duration::from_secs(60),
                            &tx,
                            &running,
                        )
                        .await
                        .expect_err("clean closure must return to the outer retry loop");
                        test_cursor.advance_after_durable_write(140);
                        Err(closure_error)
                    } else {
                        running.store(false, Ordering::Relaxed);
                        Ok(())
                    }
                })
            },
        )
        .await;

        assert_eq!(
            *replay_starts.lock().expect("replay start lock"),
            vec![68, 108]
        );
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
