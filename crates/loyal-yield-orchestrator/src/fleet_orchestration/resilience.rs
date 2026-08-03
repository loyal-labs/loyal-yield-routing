use std::time::Duration;

use chrono::{DateTime, Duration as ChronoDuration, Utc};
use serde::Serialize;
use sqlx::{postgres::PgListener, PgPool};
use thiserror::Error;
use tokio::time::Instant;

use super::queue::SignedRouteSubmissionAdvance;
use crate::rpc_safety::redacted_external_error;

pub const DEFAULT_LISTENER_RECONNECT_INITIAL_BACKOFF: Duration = Duration::from_millis(250);
pub const DEFAULT_LISTENER_RECONNECT_MAX_BACKOFF: Duration = Duration::from_secs(5);
pub const DEFAULT_LISTENER_MAX_CONNECTION_AGE: Duration = Duration::from_secs(5 * 60);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct BoundedReconnectBackoff {
    initial: Duration,
    maximum: Duration,
    next: Duration,
}

impl BoundedReconnectBackoff {
    fn new(initial: Duration, maximum: Duration) -> Result<Self, DurablePgListenerError> {
        if initial.is_zero() || maximum < initial {
            return Err(DurablePgListenerError::InvalidBackoff);
        }
        Ok(Self {
            initial,
            maximum,
            next: initial,
        })
    }

    fn record_failure(&mut self) -> Duration {
        let delay = self.next;
        self.next = self.next.saturating_mul(2).min(self.maximum);
        delay
    }

    fn reset(&mut self) {
        self.next = self.initial;
    }
}

#[derive(Debug)]
pub struct DurablePgWakeupListener {
    channel: String,
    listener: Option<PgListener>,
    connected_at: Option<Instant>,
    backoff: BoundedReconnectBackoff,
    next_reconnect_at: Instant,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DurablePgWakeupEvent {
    Notification,
    RecoveryPollElapsed,
    Reconnected,
    Disconnected {
        error: String,
        retry_after: Duration,
    },
    ReconnectFailed {
        error: String,
        retry_after: Duration,
    },
}

impl DurablePgWakeupEvent {
    /// Every reconnect boundary returns to the caller instead of waiting for a
    /// second notification. The outer loop therefore performs a durable scan
    /// immediately before and immediately after reconnect.
    pub const fn requires_immediate_durable_scan(&self) -> bool {
        !matches!(self, Self::RecoveryPollElapsed)
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Error)]
pub enum DurablePgListenerError {
    #[error("PostgreSQL wakeup channel must be a nonempty identifier")]
    InvalidChannel,
    #[error("PostgreSQL listener reconnect backoff is invalid")]
    InvalidBackoff,
}

impl DurablePgWakeupListener {
    pub fn new(channel: impl Into<String>) -> Result<Self, DurablePgListenerError> {
        Self::with_backoff(
            channel,
            DEFAULT_LISTENER_RECONNECT_INITIAL_BACKOFF,
            DEFAULT_LISTENER_RECONNECT_MAX_BACKOFF,
        )
    }

    pub fn with_backoff(
        channel: impl Into<String>,
        initial_backoff: Duration,
        maximum_backoff: Duration,
    ) -> Result<Self, DurablePgListenerError> {
        let channel = channel.into();
        if channel.is_empty()
            || !channel
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
        {
            return Err(DurablePgListenerError::InvalidChannel);
        }
        Ok(Self {
            channel,
            listener: None,
            connected_at: None,
            backoff: BoundedReconnectBackoff::new(initial_backoff, maximum_backoff)?,
            next_reconnect_at: Instant::now(),
        })
    }

    pub fn is_connected(&self) -> bool {
        self.listener.is_some()
    }

    pub async fn wait(&mut self, pool: &PgPool, recovery_poll: Duration) -> DurablePgWakeupEvent {
        if self.connected_at.is_some_and(|connected_at| {
            connected_at.elapsed() >= DEFAULT_LISTENER_MAX_CONNECTION_AGE
        }) {
            // Recycle long-lived LISTEN sessions so an anomalous backend cannot
            // pin PostgreSQL's xmin horizon indefinitely. Callers perform a
            // durable scan at every reconnect boundary, so notifications remain
            // wake-up hints rather than the source of truth.
            self.listener = None;
            self.connected_at = None;
        }

        if self.listener.is_none() {
            let now = Instant::now();
            if now < self.next_reconnect_at {
                tokio::time::sleep(recovery_poll.min(self.next_reconnect_at - now)).await;
                return DurablePgWakeupEvent::RecoveryPollElapsed;
            }
            match PgListener::connect_with(pool).await {
                Ok(mut listener) => match listener.listen(&self.channel).await {
                    Ok(()) => {
                        self.listener = Some(listener);
                        self.connected_at = Some(Instant::now());
                        self.backoff.reset();
                        return DurablePgWakeupEvent::Reconnected;
                    }
                    Err(error) => {
                        let retry_after = self.backoff.record_failure();
                        self.next_reconnect_at = Instant::now() + retry_after;
                        return DurablePgWakeupEvent::ReconnectFailed {
                            error: redacted_external_error(&error.to_string()),
                            retry_after,
                        };
                    }
                },
                Err(error) => {
                    let retry_after = self.backoff.record_failure();
                    self.next_reconnect_at = Instant::now() + retry_after;
                    return DurablePgWakeupEvent::ReconnectFailed {
                        error: redacted_external_error(&error.to_string()),
                        retry_after,
                    };
                }
            }
        }

        let receive = self
            .listener
            .as_mut()
            .expect("connected listener exists")
            .recv();
        match tokio::time::timeout(recovery_poll, receive).await {
            Ok(Ok(_)) => DurablePgWakeupEvent::Notification,
            Err(_) => DurablePgWakeupEvent::RecoveryPollElapsed,
            Ok(Err(error)) => {
                self.listener = None;
                self.connected_at = None;
                let retry_after = self.backoff.record_failure();
                self.next_reconnect_at = Instant::now() + retry_after;
                DurablePgWakeupEvent::Disconnected {
                    error: redacted_external_error(&error.to_string()),
                    retry_after,
                }
            }
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum OuterTaskFailureKind {
    ReturnedError,
    Panicked,
    JoinFailure,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct OuterTaskFailureRecovery {
    pub kind: OuterTaskFailureKind,
    pub defer_with_fence: bool,
    pub error_code: &'static str,
}

impl OuterTaskFailureRecovery {
    /// Builds the exact bounded, fenced transition used by the reconciler's
    /// outer failure boundary. Raw task errors never enter durable state.
    pub fn fenced_deferred_advance(
        self,
        checked_at: DateTime<Utc>,
    ) -> Option<SignedRouteSubmissionAdvance> {
        self.defer_with_fence
            .then_some(SignedRouteSubmissionAdvance::Deferred {
                checked_at,
                next_poll_at: checked_at + ChronoDuration::seconds(1),
                error_detail: Some(self.error_code.to_owned()),
            })
    }
}

pub const fn outer_task_failure_recovery(
    kind: OuterTaskFailureKind,
    lease_available: bool,
) -> OuterTaskFailureRecovery {
    let error_code = match kind {
        OuterTaskFailureKind::ReturnedError => "outer_task_returned_error",
        OuterTaskFailureKind::Panicked => "outer_task_panicked",
        OuterTaskFailureKind::JoinFailure => "outer_task_join_failed",
    };
    OuterTaskFailureRecovery {
        kind,
        defer_with_fence: lease_available
            && matches!(
                kind,
                OuterTaskFailureKind::ReturnedError | OuterTaskFailureKind::Panicked
            ),
        error_code,
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkerResilienceFixtureEvidence {
    pub reconnect_delays_milliseconds: Vec<u64>,
    pub maximum_reconnect_delay_milliseconds: u64,
    pub reset_delay_milliseconds: u64,
    pub disconnect_scans_immediately: bool,
    pub reconnect_scans_immediately: bool,
    pub returned_error_defers_with_fence: bool,
    pub panic_defers_with_fence: bool,
    pub join_failure_without_lease_does_not_mutate: bool,
    pub passed: bool,
}

pub fn functional_worker_resilience_fixture() -> WorkerResilienceFixtureEvidence {
    let mut backoff = BoundedReconnectBackoff::new(
        DEFAULT_LISTENER_RECONNECT_INITIAL_BACKOFF,
        DEFAULT_LISTENER_RECONNECT_MAX_BACKOFF,
    )
    .expect("default listener backoff is valid");
    let reconnect_delays = (0..7).map(|_| backoff.record_failure()).collect::<Vec<_>>();
    backoff.reset();
    let reset_delay = backoff.record_failure();
    let disconnect_scans_immediately = DurablePgWakeupEvent::Disconnected {
        error: "fixture".to_owned(),
        retry_after: DEFAULT_LISTENER_RECONNECT_INITIAL_BACKOFF,
    }
    .requires_immediate_durable_scan();
    let reconnect_scans_immediately =
        DurablePgWakeupEvent::Reconnected.requires_immediate_durable_scan();
    let checked_at = DateTime::<Utc>::from_timestamp(1_752_000_000, 0)
        .expect("fixed worker resilience fixture timestamp must be valid");
    let returned_error_defers_with_fence = matches!(
        outer_task_failure_recovery(OuterTaskFailureKind::ReturnedError, true)
            .fenced_deferred_advance(checked_at),
        Some(SignedRouteSubmissionAdvance::Deferred {
            checked_at: persisted_checked_at,
            next_poll_at,
            error_detail: Some(error_detail),
        }) if persisted_checked_at == checked_at
            && next_poll_at == checked_at + ChronoDuration::seconds(1)
            && error_detail == "outer_task_returned_error"
    );
    let panic_defers_with_fence = matches!(
        outer_task_failure_recovery(OuterTaskFailureKind::Panicked, true)
            .fenced_deferred_advance(checked_at),
        Some(SignedRouteSubmissionAdvance::Deferred {
            checked_at: persisted_checked_at,
            next_poll_at,
            error_detail: Some(error_detail),
        }) if persisted_checked_at == checked_at
            && next_poll_at == checked_at + ChronoDuration::seconds(1)
            && error_detail == "outer_task_panicked"
    );
    let join_failure_without_lease_does_not_mutate =
        outer_task_failure_recovery(OuterTaskFailureKind::JoinFailure, false)
            .fenced_deferred_advance(checked_at)
            .is_none();
    let maximum_reconnect_delay = reconnect_delays.iter().copied().max().unwrap_or_default();
    let bounded = reconnect_delays
        == [250_u64, 500, 1_000, 2_000, 4_000, 5_000, 5_000].map(Duration::from_millis)
        && maximum_reconnect_delay == DEFAULT_LISTENER_RECONNECT_MAX_BACKOFF;
    let reset = reset_delay == DEFAULT_LISTENER_RECONNECT_INITIAL_BACKOFF;
    let passed = bounded
        && reset
        && disconnect_scans_immediately
        && reconnect_scans_immediately
        && returned_error_defers_with_fence
        && panic_defers_with_fence
        && join_failure_without_lease_does_not_mutate;
    WorkerResilienceFixtureEvidence {
        reconnect_delays_milliseconds: reconnect_delays
            .into_iter()
            .map(|delay| u64::try_from(delay.as_millis()).unwrap_or(u64::MAX))
            .collect(),
        maximum_reconnect_delay_milliseconds: u64::try_from(maximum_reconnect_delay.as_millis())
            .unwrap_or(u64::MAX),
        reset_delay_milliseconds: u64::try_from(reset_delay.as_millis()).unwrap_or(u64::MAX),
        disconnect_scans_immediately,
        reconnect_scans_immediately,
        returned_error_defers_with_fence,
        panic_defers_with_fence,
        join_failure_without_lease_does_not_mutate,
        passed,
    }
}
