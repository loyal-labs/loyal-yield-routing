use std::{
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
            tracing::info!(%reserve, attempt, "subscribed to reserve account");
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
