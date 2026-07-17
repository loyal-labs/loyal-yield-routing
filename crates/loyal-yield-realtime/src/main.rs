use std::{
    collections::HashMap,
    convert::Infallible,
    env,
    net::SocketAddr,
    sync::{
        atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering},
        Arc, Mutex,
    },
    time::{Duration, Instant},
};

use axum::{
    extract::{Query, State},
    http::{header, HeaderMap, HeaderValue, Method, StatusCode},
    response::{
        sse::{Event, KeepAlive, Sse},
        IntoResponse, Response,
    },
    routing::get,
    Router,
};
use loyal_observability::{init_from_env, OperationalError};
use loyal_yield_realtime_core::{
    cleanup_expired_events_batch, event_matches_claims, fetch_events_after,
    fetch_matching_events_after, invalidation_json_for_row, latest_event_id, min_event_id,
    notification_event_id_from_payload, reject_pooled_connection_url, resync_required_json,
    verify_hmac_token_with_secrets, BoxError, RealtimeClientKind, RealtimeEventRow,
    RealtimeTokenClaims, DEFAULT_MAX_TOKEN_LIFETIME_SECONDS, DEFAULT_REALTIME_CHANNEL,
};
use serde::Deserialize;
use sqlx::{
    postgres::{PgListener, PgPoolOptions},
    PgPool, Row,
};
use tokio::{
    net::TcpListener,
    sync::{mpsc, RwLock},
    time::{interval, sleep, sleep_until, timeout, Instant as TokioInstant},
};
use tower_http::cors::{AllowOrigin, CorsLayer};
use url::Url;

const DEFAULT_HEARTBEAT_SECONDS: u64 = 15;
const DEFAULT_CATCH_UP_LIMIT: i64 = 500;
const DEFAULT_CLIENT_BUFFER: usize = 1024;
const DEFAULT_RETENTION_DAYS: i64 = 7;
const DEFAULT_RETENTION_BATCH_SIZE: i64 = 1000;
const DEFAULT_RETENTION_INTERVAL_SECONDS: u64 = 60 * 60;
const DEFAULT_READY_MAX_LAG: i64 = 1000;
const FALLBACK_TICK_SECONDS: u64 = 15;

type SseMessage = Result<Event, Infallible>;

#[derive(Clone)]
struct Config {
    database_url: String,
    auth_secret: Arc<[u8]>,
    previous_auth_secret: Option<Arc<[u8]>>,
    allowed_origins: Vec<HeaderValue>,
    heartbeat_seconds: u64,
    catch_up_limit: i64,
    client_buffer: usize,
    max_token_lifetime_seconds: i64,
    retention_days: i64,
    retention_batch_size: i64,
    retention_interval_seconds: u64,
    ready_max_lag: i64,
    channel: String,
    port: u16,
}

#[derive(Clone)]
struct AppState {
    pool: PgPool,
    config: Arc<Config>,
    clients: Arc<RwLock<HashMap<u64, ClientHandle>>>,
    next_client_id: Arc<AtomicU64>,
    listener_connected: Arc<AtomicBool>,
    admission_database_failure_reported: Arc<AtomicBool>,
    broadcast_cursor: Arc<AtomicI64>,
    metrics: Arc<Metrics>,
}

#[derive(Clone)]
struct ClientHandle {
    claims: RealtimeTokenClaims,
    minimum_live_event_id: Arc<AtomicI64>,
    sender: mpsc::Sender<SseMessage>,
}

#[derive(Default)]
struct Metrics {
    active_connections: AtomicU64,
    active_web: AtomicU64,
    active_mobile: AtomicU64,
    connections_total: AtomicU64,
    stream_lifetime_seconds_total: AtomicU64,
    expiration_closures: AtomicU64,
    listener_reconnects: AtomicU64,
    replayed_events: AtomicU64,
    resync_required: AtomicU64,
    queue_overflows: AtomicU64,
    broadcasts: AtomicU64,
    auth_failures: Mutex<HashMap<&'static str, u64>>,
}

#[derive(Default)]
struct AutodepositLatencyMetrics {
    completed_samples: u64,
    request_to_selected_ms_avg: u64,
    selected_to_pull_confirmed_ms_avg: u64,
    pull_confirmed_to_completed_ms_avg: u64,
    request_to_completed_ms_avg: u64,
}

#[derive(Debug, Deserialize, Default)]
struct EventsQuery {
    cursor: Option<String>,
    token: Option<String>,
}

struct ConnectionGuard {
    client_id: u64,
    client_kind: RealtimeClientKind,
    connected_at: Instant,
    clients: Arc<RwLock<HashMap<u64, ClientHandle>>>,
    metrics: Arc<Metrics>,
}

impl Drop for ConnectionGuard {
    fn drop(&mut self) {
        let client_id = self.client_id;
        let client_kind = self.client_kind;
        let connected_at = self.connected_at;
        let clients = self.clients.clone();
        let metrics = self.metrics.clone();
        tokio::spawn(async move {
            clients.write().await.remove(&client_id);
            metrics.active_connections.fetch_sub(1, Ordering::Relaxed);
            match client_kind {
                RealtimeClientKind::Web => {
                    metrics.active_web.fetch_sub(1, Ordering::Relaxed);
                }
                RealtimeClientKind::Mobile => {
                    metrics.active_mobile.fetch_sub(1, Ordering::Relaxed);
                }
            }
            let lifetime = connected_at.elapsed().as_secs();
            metrics
                .stream_lifetime_seconds_total
                .fetch_add(lifetime, Ordering::Relaxed);
            eprintln!(
                "realtime stream closed client_kind={} lifetime_seconds={lifetime}",
                client_kind.as_str()
            );
        });
    }
}

#[tokio::main]
async fn main() -> Result<(), BoxError> {
    let _observability = init_from_env("loyal-yield-realtime")?;
    if let Err(error) = run().await {
        OperationalError::new(
            "realtime_service_fatal",
            "run_realtime_service",
            "Yield realtime service stopped after a fatal error",
        )
        .retryable(true)
        .recovery_required(true)
        .emit();
        return Err(error);
    }
    Ok(())
}

async fn run() -> Result<(), BoxError> {
    let config = Arc::new(Config::from_env()?);
    reject_pooled_connection_url(&config.database_url)?;

    let pool = PgPoolOptions::new()
        .max_connections(8)
        .connect(&config.database_url)
        .await?;
    let initial_cursor = latest_event_id(&pool).await?;
    let state = AppState {
        pool,
        config: config.clone(),
        clients: Arc::new(RwLock::new(HashMap::new())),
        next_client_id: Arc::new(AtomicU64::new(1)),
        listener_connected: Arc::new(AtomicBool::new(false)),
        admission_database_failure_reported: Arc::new(AtomicBool::new(false)),
        broadcast_cursor: Arc::new(AtomicI64::new(initial_cursor)),
        metrics: Arc::new(Metrics::default()),
    };

    tokio::spawn(run_listener_loop(state.clone()));
    tokio::spawn(run_retention_loop(state.clone()));

    let app = Router::new()
        .route("/healthz", get(healthz))
        .route("/readyz", get(readyz))
        .route("/metrics", get(metrics))
        .route("/events", get(events))
        .with_state(state)
        .layer(cors_layer(&config));

    let address = SocketAddr::from(([0, 0, 0, 0], config.port));
    let listener = TcpListener::bind(address).await?;
    println!(
        "loyal-yield-realtime listening on 0.0.0.0:{} channel={}",
        config.port, config.channel
    );
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    Ok(())
}

async fn healthz() -> &'static str {
    "ok"
}

async fn readyz(State(state): State<AppState>) -> Response {
    let database_ok = timeout(
        Duration::from_secs(2),
        sqlx::query_scalar::<_, i32>("SELECT 1").fetch_one(&state.pool),
    )
    .await
    .is_ok_and(|result| result.is_ok_and(|value| value == 1));
    let latest = timeout(Duration::from_secs(2), latest_event_id(&state.pool)).await;
    let broadcast_cursor = state.broadcast_cursor.load(Ordering::Relaxed);
    let lag = latest
        .ok()
        .and_then(Result::ok)
        .map(|value| value.saturating_sub(broadcast_cursor));
    let listener_ok = state.listener_connected.load(Ordering::Relaxed);
    let lag_ok = lag.is_some_and(|value| value <= state.config.ready_max_lag);

    if database_ok && listener_ok && lag_ok {
        (
            StatusCode::OK,
            format!("ready listener=true broadcast_lag={}", lag.unwrap_or(0)),
        )
            .into_response()
    } else {
        (
            StatusCode::SERVICE_UNAVAILABLE,
            format!(
                "not_ready database={} listener={} broadcast_lag={}",
                database_ok,
                listener_ok,
                lag.map_or_else(|| "unknown".to_owned(), |value| value.to_string())
            ),
        )
            .into_response()
    }
}

async fn metrics(State(state): State<AppState>) -> Response {
    let auth_failures = state.metrics.auth_failures.lock().unwrap().clone();
    let mut lines = vec![
        metric(
            "loyal_realtime_active_connections",
            state.metrics.active_connections.load(Ordering::Relaxed),
        ),
        labeled_metric(
            "loyal_realtime_active_connections_by_client_kind",
            "client_kind",
            "web",
            state.metrics.active_web.load(Ordering::Relaxed),
        ),
        labeled_metric(
            "loyal_realtime_active_connections_by_client_kind",
            "client_kind",
            "mobile",
            state.metrics.active_mobile.load(Ordering::Relaxed),
        ),
        metric(
            "loyal_realtime_connections_total",
            state.metrics.connections_total.load(Ordering::Relaxed),
        ),
        metric(
            "loyal_realtime_stream_lifetime_seconds_total",
            state
                .metrics
                .stream_lifetime_seconds_total
                .load(Ordering::Relaxed),
        ),
        metric(
            "loyal_realtime_expiration_closures_total",
            state.metrics.expiration_closures.load(Ordering::Relaxed),
        ),
        metric(
            "loyal_realtime_listener_reconnects_total",
            state.metrics.listener_reconnects.load(Ordering::Relaxed),
        ),
        metric(
            "loyal_realtime_replayed_events_total",
            state.metrics.replayed_events.load(Ordering::Relaxed),
        ),
        metric(
            "loyal_realtime_resync_required_total",
            state.metrics.resync_required.load(Ordering::Relaxed),
        ),
        metric(
            "loyal_realtime_queue_overflows_total",
            state.metrics.queue_overflows.load(Ordering::Relaxed),
        ),
        metric(
            "loyal_realtime_broadcast_events_total",
            state.metrics.broadcasts.load(Ordering::Relaxed),
        ),
        metric(
            "loyal_realtime_broadcast_cursor",
            state.broadcast_cursor.load(Ordering::Relaxed).max(0) as u64,
        ),
    ];
    for (reason, count) in auth_failures {
        lines.push(labeled_metric(
            "loyal_realtime_auth_failures_total",
            "reason",
            reason,
            count,
        ));
    }
    match load_autodeposit_latency_metrics(&state.pool).await {
        Ok(latency) => {
            lines.push(metric(
                "loyal_realtime_autodeposit_completed_samples",
                latency.completed_samples,
            ));
            lines.push(metric(
                "loyal_realtime_autodeposit_request_to_selected_ms_avg",
                latency.request_to_selected_ms_avg,
            ));
            lines.push(metric(
                "loyal_realtime_autodeposit_selected_to_pull_confirmed_ms_avg",
                latency.selected_to_pull_confirmed_ms_avg,
            ));
            lines.push(metric(
                "loyal_realtime_autodeposit_pull_confirmed_to_completed_ms_avg",
                latency.pull_confirmed_to_completed_ms_avg,
            ));
            lines.push(metric(
                "loyal_realtime_autodeposit_request_to_completed_ms_avg",
                latency.request_to_completed_ms_avg,
            ));
        }
        Err(error) => {
            eprintln!("realtime autodeposit latency metrics query failed: {error}");
            lines.push(metric("loyal_realtime_autodeposit_latency_query_errors", 1));
        }
    }
    (
        [(header::CONTENT_TYPE, "text/plain; version=0.0.4")],
        lines.join("\n"),
    )
        .into_response()
}

async fn load_autodeposit_latency_metrics(
    pool: &PgPool,
) -> Result<AutodepositLatencyMetrics, sqlx::Error> {
    let row = sqlx::query(
        r#"
        WITH phases AS (
            SELECT
                target_id,
                scheduled_slot_id,
                MIN(created_at) FILTER (WHERE reason = 'requested') AS requested_at,
                MIN(created_at) FILTER (WHERE reason = 'selected') AS selected_at,
                MIN(created_at) FILTER (WHERE reason = 'pull_confirmed') AS pull_confirmed_at,
                MIN(created_at) FILTER (WHERE reason = 'completed') AS completed_at
            FROM loyal_yield.realtime_events
            WHERE event_type = 'earn.autodeposit.execution.changed'
              AND deliverable = TRUE
              AND created_at >= now() - interval '7 days'
              AND target_id IS NOT NULL
              AND scheduled_slot_id IS NOT NULL
            GROUP BY target_id, scheduled_slot_id
        )
        SELECT
            COUNT(*) FILTER (WHERE completed_at IS NOT NULL)::bigint AS completed_samples,
            COALESCE(AVG(EXTRACT(EPOCH FROM (selected_at - requested_at)) * 1000)
                FILTER (WHERE requested_at IS NOT NULL AND selected_at IS NOT NULL), 0)::bigint
                AS request_to_selected_ms_avg,
            COALESCE(AVG(EXTRACT(EPOCH FROM (pull_confirmed_at - selected_at)) * 1000)
                FILTER (WHERE selected_at IS NOT NULL AND pull_confirmed_at IS NOT NULL), 0)::bigint
                AS selected_to_pull_confirmed_ms_avg,
            COALESCE(AVG(EXTRACT(EPOCH FROM (completed_at - pull_confirmed_at)) * 1000)
                FILTER (WHERE pull_confirmed_at IS NOT NULL AND completed_at IS NOT NULL), 0)::bigint
                AS pull_confirmed_to_completed_ms_avg,
            COALESCE(AVG(EXTRACT(EPOCH FROM (completed_at - requested_at)) * 1000)
                FILTER (WHERE requested_at IS NOT NULL AND completed_at IS NOT NULL), 0)::bigint
                AS request_to_completed_ms_avg
        FROM phases
        "#,
    )
    .fetch_one(pool)
    .await?;
    Ok(AutodepositLatencyMetrics {
        completed_samples: row.try_get::<i64, _>("completed_samples")?.max(0) as u64,
        request_to_selected_ms_avg: row.try_get::<i64, _>("request_to_selected_ms_avg")?.max(0)
            as u64,
        selected_to_pull_confirmed_ms_avg: row
            .try_get::<i64, _>("selected_to_pull_confirmed_ms_avg")?
            .max(0) as u64,
        pull_confirmed_to_completed_ms_avg: row
            .try_get::<i64, _>("pull_confirmed_to_completed_ms_avg")?
            .max(0) as u64,
        request_to_completed_ms_avg: row.try_get::<i64, _>("request_to_completed_ms_avg")?.max(0)
            as u64,
    })
}

fn metric(name: &str, value: u64) -> String {
    format!("{name} {value}")
}

fn labeled_metric(name: &str, label: &str, label_value: &str, value: u64) -> String {
    format!("{name}{{{label}=\"{label_value}\"}} {value}")
}

async fn events(
    State(state): State<AppState>,
    Query(query): Query<EventsQuery>,
    headers: HeaderMap,
) -> Response {
    if query.token.is_some() {
        record_auth_failure(&state.metrics, "query_token");
        return (StatusCode::UNAUTHORIZED, "bearer token required").into_response();
    }
    let Some(token) = bearer_token(&headers) else {
        record_auth_failure(&state.metrics, "missing_bearer");
        return (StatusCode::UNAUTHORIZED, "bearer token required").into_response();
    };
    let now = chrono::Utc::now().timestamp();
    let claims = match verify_hmac_token_with_secrets(
        token,
        &state.config.auth_secret,
        state.config.previous_auth_secret.as_deref(),
        now,
        state.config.max_token_lifetime_seconds,
    ) {
        Ok(claims) => claims,
        Err(error) => {
            record_auth_failure(&state.metrics, error.reason().as_str());
            eprintln!(
                "realtime authentication failed reason={}",
                error.reason().as_str()
            );
            return (StatusCode::UNAUTHORIZED, "invalid bearer token").into_response();
        }
    };
    let cursor = match requested_cursor(&headers, query.cursor.as_deref()) {
        Ok(cursor) => cursor,
        Err(message) => return (StatusCode::BAD_REQUEST, message).into_response(),
    };

    let (sender, receiver) = mpsc::channel::<SseMessage>(state.config.client_buffer);
    let client_id = state.next_client_id.fetch_add(1, Ordering::Relaxed);
    let minimum_live_event_id = Arc::new(AtomicI64::new(i64::MAX));
    state.clients.write().await.insert(
        client_id,
        ClientHandle {
            claims: claims.clone(),
            minimum_live_event_id: minimum_live_event_id.clone(),
            sender: sender.clone(),
        },
    );
    state
        .metrics
        .active_connections
        .fetch_add(1, Ordering::Relaxed);
    state
        .metrics
        .connections_total
        .fetch_add(1, Ordering::Relaxed);
    match claims.client_kind {
        RealtimeClientKind::Web => {
            state.metrics.active_web.fetch_add(1, Ordering::Relaxed);
        }
        RealtimeClientKind::Mobile => {
            state.metrics.active_mobile.fetch_add(1, Ordering::Relaxed);
        }
    }

    let high_water = match latest_event_id(&state.pool).await {
        Ok(value) => {
            state
                .admission_database_failure_reported
                .store(false, Ordering::Relaxed);
            value
        }
        Err(error) => {
            if !state
                .admission_database_failure_reported
                .swap(true, Ordering::Relaxed)
            {
                OperationalError::new(
                    "realtime_admission_database_failed",
                    "load_realtime_admission_high_water",
                    "Yield realtime admission could not read the event high-water mark",
                )
                .retryable(true)
                .recovery_required(false)
                .emit();
            }
            eprintln!("realtime admission high-water lookup failed: {error}");
            state.clients.write().await.remove(&client_id);
            decrement_active(&state.metrics, claims.client_kind);
            return (StatusCode::SERVICE_UNAVAILABLE, "realtime unavailable").into_response();
        }
    };
    minimum_live_event_id.store(high_water, Ordering::Release);

    if let Some(cursor) = cursor {
        match enqueue_replay(&state, &claims, &sender, cursor, high_water).await {
            Ok(ReplayDisposition::Continue) => {}
            Ok(ReplayDisposition::Close) => {
                state.clients.write().await.remove(&client_id);
            }
            Err(error) => {
                eprintln!("realtime client replay failed: {error}");
                enqueue_resync(&state, &sender, "replay_failed");
                state.clients.write().await.remove(&client_id);
            }
        }
    }

    let client_kind = claims.client_kind;
    let expiration =
        TokioInstant::now() + Duration::from_secs(claims.exp.saturating_sub(now).max(0) as u64);
    let guard = ConnectionGuard {
        client_id,
        client_kind,
        connected_at: Instant::now(),
        clients: state.clients.clone(),
        metrics: state.metrics.clone(),
    };
    let metrics = state.metrics.clone();
    let stream = async_stream::stream! {
        let _guard = guard;
        let mut receiver = receiver;
        loop {
            tokio::select! {
                _ = sleep_until(expiration) => {
                    metrics.expiration_closures.fetch_add(1, Ordering::Relaxed);
                    eprintln!("realtime stream token expired client_kind={}", client_kind.as_str());
                    break;
                }
                item = receiver.recv() => {
                    match item {
                        Some(item) => yield item,
                        None => break,
                    }
                }
            }
        }
    };
    drop(sender);

    Sse::new(stream)
        .keep_alive(
            KeepAlive::new()
                .interval(Duration::from_secs(state.config.heartbeat_seconds))
                .text("heartbeat"),
        )
        .into_response()
}

enum ReplayDisposition {
    Continue,
    Close,
}

async fn enqueue_replay(
    state: &AppState,
    claims: &RealtimeTokenClaims,
    sender: &mpsc::Sender<SseMessage>,
    cursor: i64,
    high_water: i64,
) -> Result<ReplayDisposition, BoxError> {
    if matches!(min_event_id(&state.pool).await?, Some(min_id) if cursor.saturating_add(1) < min_id)
    {
        enqueue_resync(state, sender, "cursor_expired");
        return Ok(ReplayDisposition::Close);
    }

    let mut rows = fetch_matching_events_after(
        &state.pool,
        claims,
        cursor,
        high_water,
        state.config.catch_up_limit + 1,
    )
    .await?;
    if rows.len() as i64 > state.config.catch_up_limit {
        enqueue_resync(state, sender, "replay_limit_exceeded");
        return Ok(ReplayDisposition::Close);
    }
    let replay_count = rows.len() as u64;
    for row in rows.drain(..) {
        sender
            .try_send(Ok(sse_event_for_row(&row)))
            .map_err(|_| "replay queue unexpectedly full")?;
    }
    state
        .metrics
        .replayed_events
        .fetch_add(replay_count, Ordering::Relaxed);
    eprintln!("realtime replay complete count={replay_count}");
    Ok(ReplayDisposition::Continue)
}

fn enqueue_resync(state: &AppState, sender: &mpsc::Sender<SseMessage>, reason: &'static str) {
    state
        .metrics
        .resync_required
        .fetch_add(1, Ordering::Relaxed);
    let _ = sender.try_send(Ok(resync_required_event(reason)));
    eprintln!("realtime resync required reason={reason}");
}

fn bearer_token(headers: &HeaderMap) -> Option<&str> {
    let value = headers.get(header::AUTHORIZATION)?.to_str().ok()?;
    let token = value.strip_prefix("Bearer ")?;
    (!token.is_empty() && !token.contains(char::is_whitespace)).then_some(token)
}

fn requested_cursor(
    headers: &HeaderMap,
    query_cursor: Option<&str>,
) -> Result<Option<i64>, &'static str> {
    let header_cursor = headers
        .get("last-event-id")
        .map(|value| value.to_str().map_err(|_| "invalid Last-Event-ID"))
        .transpose()?
        .map(parse_cursor)
        .transpose()?;
    let query_cursor = query_cursor.map(parse_cursor).transpose()?;
    match (header_cursor, query_cursor) {
        (Some(header), Some(query)) if header != query => Err("conflicting cursors"),
        (Some(header), _) => Ok(Some(header)),
        (_, Some(query)) => Ok(Some(query)),
        (None, None) => Ok(None),
    }
}

fn parse_cursor(value: &str) -> Result<i64, &'static str> {
    let cursor = value
        .trim()
        .parse::<i64>()
        .map_err(|_| "cursor must be a decimal event id")?;
    (cursor >= 0)
        .then_some(cursor)
        .ok_or("cursor must be non-negative")
}

fn record_auth_failure(metrics: &Metrics, reason: &'static str) {
    let mut failures = metrics.auth_failures.lock().unwrap();
    *failures.entry(reason).or_default() += 1;
}

fn decrement_active(metrics: &Metrics, client_kind: RealtimeClientKind) {
    metrics.active_connections.fetch_sub(1, Ordering::Relaxed);
    match client_kind {
        RealtimeClientKind::Web => {
            metrics.active_web.fetch_sub(1, Ordering::Relaxed);
        }
        RealtimeClientKind::Mobile => {
            metrics.active_mobile.fetch_sub(1, Ordering::Relaxed);
        }
    }
}

async fn run_listener_loop(state: AppState) {
    let mut cursor = state.broadcast_cursor.load(Ordering::Relaxed);
    let mut outage_reported = false;
    loop {
        state.listener_connected.store(false, Ordering::Relaxed);
        if let Err(error) = run_listener_session(&state, &mut cursor).await {
            let had_connected = state.listener_connected.swap(false, Ordering::Relaxed);
            if had_connected {
                outage_reported = false;
            }
            if !outage_reported {
                OperationalError::new(
                    "realtime_listener_unavailable",
                    "listen_for_realtime_events",
                    "Yield realtime listener session is unavailable",
                )
                .retryable(true)
                .recovery_required(false)
                .emit();
                outage_reported = true;
            }
            state
                .metrics
                .listener_reconnects
                .fetch_add(1, Ordering::Relaxed);
            eprintln!("realtime listener session failed: {error}");
            sleep(Duration::from_secs(5)).await;
        }
    }
}

async fn run_listener_session(state: &AppState, cursor: &mut i64) -> Result<(), BoxError> {
    let mut listener = PgListener::connect(&state.config.database_url).await?;
    listener.listen(&state.config.channel).await?;
    state.listener_connected.store(true, Ordering::Relaxed);
    catch_up_and_broadcast(state, cursor).await?;

    let mut fallback = interval(Duration::from_secs(FALLBACK_TICK_SECONDS));
    loop {
        tokio::select! {
            notification = listener.recv() => {
                let notification = notification?;
                if notification_event_id_from_payload(notification.payload()).is_none() {
                    eprintln!("realtime notification payload did not include event_id");
                }
                catch_up_and_broadcast(state, cursor).await?;
            }
            _ = fallback.tick() => {
                catch_up_and_broadcast(state, cursor).await?;
            }
        }
    }
}

async fn catch_up_and_broadcast(state: &AppState, cursor: &mut i64) -> Result<(), BoxError> {
    loop {
        let rows = fetch_events_after(&state.pool, *cursor, state.config.catch_up_limit).await?;
        if rows.is_empty() {
            break;
        }
        for row in rows {
            *cursor = row.id;
            state.broadcast_cursor.store(*cursor, Ordering::Relaxed);
            broadcast_row(state, &row).await;
        }
    }
    Ok(())
}

async fn broadcast_row(state: &AppState, row: &RealtimeEventRow) {
    let handles: Vec<(u64, ClientHandle)> = state
        .clients
        .read()
        .await
        .iter()
        .map(|(id, handle)| (*id, handle.clone()))
        .collect();
    let mut dead_clients = Vec::new();

    for (client_id, handle) in handles {
        if row.id <= handle.minimum_live_event_id.load(Ordering::Acquire)
            || !event_matches_claims(row, &handle.claims)
        {
            continue;
        }
        match handle.sender.try_send(Ok(sse_event_for_row(row))) {
            Ok(()) => {
                state.metrics.broadcasts.fetch_add(1, Ordering::Relaxed);
            }
            Err(mpsc::error::TrySendError::Closed(_)) => dead_clients.push(client_id),
            Err(mpsc::error::TrySendError::Full(_)) => {
                state
                    .metrics
                    .queue_overflows
                    .fetch_add(1, Ordering::Relaxed);
                eprintln!(
                    "realtime slow client disconnected client_kind={} reason=queue_overflow",
                    handle.claims.client_kind.as_str()
                );
                dead_clients.push(client_id);
            }
        }
    }

    if !dead_clients.is_empty() {
        let mut clients = state.clients.write().await;
        for client_id in dead_clients {
            clients.remove(&client_id);
        }
    }
}

async fn run_retention_loop(state: AppState) {
    let mut tick = interval(Duration::from_secs(state.config.retention_interval_seconds));
    loop {
        tick.tick().await;
        loop {
            match cleanup_expired_events_batch(
                &state.pool,
                state.config.retention_days,
                state.config.retention_batch_size,
            )
            .await
            {
                Ok(deleted) => {
                    if deleted > 0 {
                        eprintln!("realtime retention cleanup deleted_rows={deleted}");
                    }
                    if deleted < state.config.retention_batch_size as u64 {
                        break;
                    }
                }
                Err(error) => {
                    eprintln!("realtime retention cleanup failed: {error}");
                    break;
                }
            }
        }
    }
}

fn sse_event_for_row(row: &RealtimeEventRow) -> Event {
    Event::default()
        .id(row.id.to_string())
        .event("loyal_yield")
        .data(invalidation_json_for_row(row))
}

fn resync_required_event(reason: &str) -> Event {
    Event::default()
        .event("loyal_yield")
        .data(resync_required_json(reason))
}

fn cors_layer(config: &Config) -> CorsLayer {
    CorsLayer::new()
        .allow_methods([Method::GET, Method::OPTIONS])
        .allow_headers([
            header::ACCEPT,
            header::AUTHORIZATION,
            header::CONTENT_TYPE,
            header::HeaderName::from_static("last-event-id"),
        ])
        .allow_origin(AllowOrigin::list(config.allowed_origins.clone()))
}

impl Config {
    fn from_env() -> Result<Self, BoxError> {
        let database_url = env::var("NEON_DATABASE_URL")
            .map_err(|_| "NEON_DATABASE_URL must be set for realtime service")?;
        let auth_secret = parse_secret("REALTIME_AUTH_SECRET")?;
        let previous_auth_secret = env::var("REALTIME_AUTH_PREVIOUS_SECRET")
            .ok()
            .filter(|value| !value.is_empty())
            .map(|value| validate_secret("REALTIME_AUTH_PREVIOUS_SECRET", value))
            .transpose()?;
        let catch_up_limit =
            parse_env_i64("REALTIME_CATCH_UP_LIMIT", DEFAULT_CATCH_UP_LIMIT).max(1);
        let client_buffer = parse_env_usize("REALTIME_CLIENT_BUFFER", DEFAULT_CLIENT_BUFFER);
        if client_buffer <= catch_up_limit as usize {
            return Err("REALTIME_CLIENT_BUFFER must exceed REALTIME_CATCH_UP_LIMIT".into());
        }

        Ok(Self {
            database_url,
            auth_secret,
            previous_auth_secret,
            allowed_origins: parse_allowed_origins()?,
            heartbeat_seconds: parse_env_u64(
                "REALTIME_HEARTBEAT_SECONDS",
                DEFAULT_HEARTBEAT_SECONDS,
            )
            .max(1),
            catch_up_limit,
            client_buffer,
            max_token_lifetime_seconds: parse_env_i64(
                "REALTIME_MAX_TOKEN_LIFETIME_SECONDS",
                DEFAULT_MAX_TOKEN_LIFETIME_SECONDS,
            )
            .clamp(1, DEFAULT_MAX_TOKEN_LIFETIME_SECONDS),
            retention_days: parse_env_i64("REALTIME_RETENTION_DAYS", DEFAULT_RETENTION_DAYS).max(1),
            retention_batch_size: parse_env_i64(
                "REALTIME_RETENTION_BATCH_SIZE",
                DEFAULT_RETENTION_BATCH_SIZE,
            )
            .max(1),
            retention_interval_seconds: parse_env_u64(
                "REALTIME_RETENTION_INTERVAL_SECONDS",
                DEFAULT_RETENTION_INTERVAL_SECONDS,
            )
            .max(60),
            ready_max_lag: parse_env_i64("REALTIME_READY_MAX_LAG", DEFAULT_READY_MAX_LAG).max(0),
            channel: env::var("REALTIME_CHANNEL")
                .unwrap_or_else(|_| DEFAULT_REALTIME_CHANNEL.to_owned()),
            port: parse_env_u16("PORT", 10000),
        })
    }
}

fn parse_secret(name: &str) -> Result<Arc<[u8]>, BoxError> {
    let value = env::var(name).map_err(|_| format!("{name} must be set for realtime service"))?;
    validate_secret(name, value)
}

fn validate_secret(name: &str, value: String) -> Result<Arc<[u8]>, BoxError> {
    if value.len() < 32 {
        return Err(format!("{name} must be at least 32 bytes").into());
    }
    Ok(Arc::from(value.into_bytes()))
}

fn parse_allowed_origins() -> Result<Vec<HeaderValue>, BoxError> {
    let value = env::var("REALTIME_ALLOWED_ORIGINS")
        .map_err(|_| "REALTIME_ALLOWED_ORIGINS must list exact browser origins")?;
    let is_render = env::var("RENDER").as_deref() == Ok("true");
    let origins = value
        .split(',')
        .map(str::trim)
        .filter(|origin| !origin.is_empty())
        .map(|origin| {
            let parsed = Url::parse(origin)?;
            if !matches!(parsed.scheme(), "http" | "https")
                || parsed.path() != "/"
                || parsed.query().is_some()
                || parsed.fragment().is_some()
                || parsed.username() != ""
                || parsed.password().is_some()
            {
                return Err(format!("invalid exact origin {origin}").into());
            }
            let host = parsed.host_str().ok_or("origin host missing")?;
            if is_render && matches!(host, "localhost" | "127.0.0.1" | "::1") {
                return Err("localhost origins are forbidden on Render".into());
            }
            HeaderValue::from_str(origin).map_err(Into::into)
        })
        .collect::<Result<Vec<_>, BoxError>>()?;
    if origins.is_empty() {
        return Err("REALTIME_ALLOWED_ORIGINS cannot be empty".into());
    }
    Ok(origins)
}

fn parse_env_u64(name: &str, default: u64) -> u64 {
    env::var(name)
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(default)
}

fn parse_env_i64(name: &str, default: i64) -> i64 {
    env::var(name)
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(default)
}

fn parse_env_usize(name: &str, default: usize) -> usize {
    env::var(name)
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(default)
}

fn parse_env_u16(name: &str, default: u16) -> u16 {
    env::var(name)
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(default)
}

async fn shutdown_signal() {
    let ctrl_c = async {
        let _ = tokio::signal::ctrl_c().await;
    };

    #[cfg(unix)]
    let terminate = async {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut signal) => {
                signal.recv().await;
            }
            Err(error) => {
                eprintln!("failed to install SIGTERM handler: {error}");
            }
        }
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }
    println!("loyal-yield-realtime shutting down");
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cursor_contract_rejects_conflicts_and_invalid_values() {
        let mut headers = HeaderMap::new();
        headers.insert("last-event-id", HeaderValue::from_static("10"));
        assert_eq!(requested_cursor(&headers, Some("10")), Ok(Some(10)));
        assert_eq!(
            requested_cursor(&headers, Some("11")),
            Err("conflicting cursors")
        );
        assert_eq!(parse_cursor("-1"), Err("cursor must be non-negative"));
        assert_eq!(
            parse_cursor("secret"),
            Err("cursor must be a decimal event id")
        );
    }

    #[test]
    fn bearer_contract_rejects_non_bearer_and_whitespace() {
        let mut headers = HeaderMap::new();
        headers.insert(header::AUTHORIZATION, HeaderValue::from_static("token"));
        assert!(bearer_token(&headers).is_none());
        headers.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Bearer secret value"),
        );
        assert!(bearer_token(&headers).is_none());
        headers.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Bearer opaque-token"),
        );
        assert_eq!(bearer_token(&headers), Some("opaque-token"));
    }
}
