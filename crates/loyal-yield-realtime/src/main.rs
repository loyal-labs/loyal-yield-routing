use std::{
    collections::HashMap,
    convert::Infallible,
    env,
    net::SocketAddr,
    sync::{
        atomic::{AtomicU64, Ordering},
        Arc,
    },
    time::Duration,
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
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine as _};
use chrono::Utc;
use hmac::{Hmac, Mac};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::Sha256;
use sqlx::{
    postgres::{PgListener, PgPoolOptions},
    PgPool,
};
use subtle::ConstantTimeEq;
use tokio::{
    net::TcpListener,
    sync::{mpsc, RwLock},
    time::{interval, sleep, timeout},
};
use tokio_stream::wrappers::ReceiverStream;
use tower_http::cors::{AllowOrigin, CorsLayer};

const DEFAULT_CHANNEL: &str = "loyal_yield_realtime";
const DEFAULT_HEARTBEAT_SECONDS: u64 = 20;
const DEFAULT_CATCH_UP_LIMIT: i64 = 500;
const DEFAULT_CLIENT_BUFFER: usize = 256;
const FALLBACK_TICK_SECONDS: u64 = 15;

type HmacSha256 = Hmac<Sha256>;
type SseMessage = Result<Event, Infallible>;
type BoxError = Box<dyn std::error::Error + Send + Sync>;

#[derive(Clone)]
struct Config {
    database_url: String,
    auth_secret: Arc<[u8]>,
    allowed_origins: Vec<HeaderValue>,
    heartbeat_seconds: u64,
    catch_up_limit: i64,
    channel: String,
    port: u16,
}

#[derive(Clone)]
struct AppState {
    pool: PgPool,
    config: Arc<Config>,
    clients: Arc<RwLock<HashMap<u64, ClientHandle>>>,
    next_client_id: Arc<AtomicU64>,
}

#[derive(Clone)]
struct ClientHandle {
    claims: RealtimeTokenClaims,
    sender: mpsc::Sender<SseMessage>,
}

#[derive(Debug, Deserialize)]
struct EventsQuery {
    token: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
struct RealtimeTokenClaims {
    exp: i64,
    #[serde(rename = "walletAddress", default)]
    wallet_address: Option<String>,
    #[serde(rename = "settingsPda", default)]
    settings_pda: Option<String>,
    #[serde(rename = "smartAccountAddress", default)]
    smart_account_address: Option<String>,
    #[serde(rename = "solanaEnv")]
    solana_env: String,
    scopes: Vec<String>,
}

#[derive(Debug, Clone, sqlx::FromRow)]
struct RealtimeEventRow {
    id: i64,
    event_type: String,
    scope: String,
    reason: String,
    solana_env: Option<String>,
    wallet_address: Option<String>,
    settings_pda: Option<String>,
    smart_account_address: Option<String>,
    target_id: Option<i64>,
    scheduled_slot_id: Option<i64>,
    execution_id: Option<i64>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct RealtimeInvalidation {
    #[serde(rename = "type")]
    event_type: String,
    event_id: i64,
    scope: String,
    reason: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    solana_env: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    wallet_address: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    settings_pda: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    smart_account_address: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    target_id: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    scheduled_slot_id: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    execution_id: Option<i64>,
}

#[tokio::main]
async fn main() -> Result<(), BoxError> {
    let config = Arc::new(Config::from_env()?);
    reject_pooled_connection_url(&config.database_url)?;

    let pool = PgPoolOptions::new()
        .max_connections(5)
        .connect(&config.database_url)
        .await?;
    let state = AppState {
        pool,
        config: config.clone(),
        clients: Arc::new(RwLock::new(HashMap::new())),
        next_client_id: Arc::new(AtomicU64::new(1)),
    };

    tokio::spawn(run_listener_loop(state.clone()));

    let app = Router::new()
        .route("/healthz", get(healthz))
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

async fn events(
    State(state): State<AppState>,
    Query(query): Query<EventsQuery>,
    headers: HeaderMap,
) -> Response {
    // EventSource reconnects with Last-Event-ID; axum::Sse returns text/event-stream.
    let Some(token) = query.token.as_deref() else {
        return (StatusCode::UNAUTHORIZED, "missing token").into_response();
    };
    let claims = match verify_token(token, &state.config.auth_secret) {
        Ok(claims) => claims,
        Err(_) => return (StatusCode::UNAUTHORIZED, "invalid token").into_response(),
    };

    let last_event_id = parse_last_event_id(&headers);
    let (sender, receiver) = mpsc::channel::<SseMessage>(DEFAULT_CLIENT_BUFFER);

    let client_id = state.next_client_id.fetch_add(1, Ordering::Relaxed);
    state.clients.write().await.insert(
        client_id,
        ClientHandle {
            claims: claims.clone(),
            sender: sender.clone(),
        },
    );
    let cleanup_clients = state.clients.clone();
    let cleanup_sender = sender.clone();
    tokio::spawn(async move {
        cleanup_sender.closed().await;
        cleanup_clients.write().await.remove(&client_id);
    });

    if let Some(cursor) = last_event_id {
        send_client_catch_up(&state, &claims, &sender, cursor).await;
    }

    Sse::new(ReceiverStream::new(receiver))
        .keep_alive(
            KeepAlive::new()
                .interval(Duration::from_secs(state.config.heartbeat_seconds))
                .text("heartbeat"),
        )
        .into_response()
}

async fn run_listener_loop(state: AppState) {
    let mut cursor = match latest_event_id(&state.pool).await {
        Ok(cursor) => cursor,
        Err(error) => {
            eprintln!("realtime latest cursor lookup failed: {error}");
            0
        }
    };
    loop {
        if let Err(error) = run_listener_session(&state, &mut cursor).await {
            eprintln!("realtime listener session failed: {error}");
            sleep(Duration::from_secs(5)).await;
        }
    }
}

async fn run_listener_session(state: &AppState, cursor: &mut i64) -> Result<(), BoxError> {
    let mut listener = PgListener::connect(&state.config.database_url).await?;
    listener.listen(&state.config.channel).await?;
    catch_up_and_broadcast(state, cursor).await?;

    let mut fallback = interval(Duration::from_secs(FALLBACK_TICK_SECONDS));
    loop {
        tokio::select! {
            notification = listener.recv() => {
                let notification = notification?;
                if let Err(error) = serde_json::from_str::<serde_json::Value>(notification.payload()) {
                    eprintln!("realtime notification payload was not JSON: {error}");
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
        if !event_matches_claims(row, &handle.claims) {
            continue;
        }
        match handle.sender.try_send(Ok(sse_event_for_row(row))) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Closed(_)) => dead_clients.push(client_id),
            Err(mpsc::error::TrySendError::Full(_)) => {
                let _ = timeout(
                    Duration::from_millis(100),
                    handle
                        .sender
                        .send(Ok(resync_required_event("client_queue_overflow"))),
                )
                .await;
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

async fn send_client_catch_up(
    state: &AppState,
    claims: &RealtimeTokenClaims,
    sender: &mpsc::Sender<SseMessage>,
    cursor: i64,
) {
    match min_event_id(&state.pool).await {
        Ok(Some(min_id)) if cursor.saturating_add(1) < min_id => {
            let _ = sender
                .send(Ok(resync_required_event("cursor_expired")))
                .await;
            return;
        }
        Ok(_) => {}
        Err(error) => {
            eprintln!("realtime min cursor lookup failed: {error}");
            let _ = sender
                .send(Ok(resync_required_event("cursor_check_failed")))
                .await;
            return;
        }
    }

    match fetch_events_after(&state.pool, cursor, state.config.catch_up_limit).await {
        Ok(rows) => {
            let truncated = rows.len() as i64 >= state.config.catch_up_limit;
            for row in rows {
                if event_matches_claims(&row, claims) {
                    let _ = sender.send(Ok(sse_event_for_row(&row))).await;
                }
            }
            if truncated {
                let _ = sender
                    .send(Ok(resync_required_event("catch_up_limit_exceeded")))
                    .await;
            }
        }
        Err(error) => {
            eprintln!("realtime client catch-up failed: {error}");
            let _ = sender
                .send(Ok(resync_required_event("catch_up_failed")))
                .await;
        }
    }
}

async fn latest_event_id(pool: &PgPool) -> Result<i64, sqlx::Error> {
    let cursor: Option<i64> = sqlx::query_scalar("SELECT MAX(id) FROM loyal_yield.realtime_events")
        .fetch_one(pool)
        .await?;
    Ok(cursor.unwrap_or(0))
}

async fn min_event_id(pool: &PgPool) -> Result<Option<i64>, sqlx::Error> {
    sqlx::query_scalar("SELECT MIN(id) FROM loyal_yield.realtime_events")
        .fetch_one(pool)
        .await
}

async fn fetch_events_after(
    pool: &PgPool,
    cursor: i64,
    limit: i64,
) -> Result<Vec<RealtimeEventRow>, sqlx::Error> {
    sqlx::query_as::<_, RealtimeEventRow>(
        r#"
        SELECT
            id,
            event_type,
            scope,
            reason,
            solana_env,
            wallet_address,
            settings_pda,
            smart_account_address,
            target_id,
            scheduled_slot_id,
            execution_id
        FROM loyal_yield.realtime_events
        WHERE id > $1
        ORDER BY id ASC
        LIMIT $2
        "#,
    )
    .bind(cursor)
    .bind(limit)
    .fetch_all(pool)
    .await
}

fn event_matches_claims(row: &RealtimeEventRow, claims: &RealtimeTokenClaims) -> bool {
    if !claims.scopes.iter().any(|scope| scope == &row.scope) {
        return false;
    }
    if let Some(row_env) = row.solana_env.as_deref() {
        if row_env != claims.solana_env {
            return false;
        }
    }
    if let Some(wallet_address) = row.wallet_address.as_deref() {
        if claims.wallet_address.as_deref() != Some(wallet_address) {
            return false;
        }
    }
    if let Some(settings_pda) = row.settings_pda.as_deref() {
        if claims.settings_pda.as_deref() != Some(settings_pda) {
            return false;
        }
    }
    if let Some(smart_account_address) = row.smart_account_address.as_deref() {
        if claims.smart_account_address.as_deref() != Some(smart_account_address) {
            return false;
        }
    }
    true
}

fn sse_event_for_row(row: &RealtimeEventRow) -> Event {
    let payload = RealtimeInvalidation {
        event_type: row.event_type.clone(),
        event_id: row.id,
        scope: row.scope.clone(),
        reason: row.reason.clone(),
        solana_env: row.solana_env.clone(),
        wallet_address: row.wallet_address.clone(),
        settings_pda: row.settings_pda.clone(),
        smart_account_address: row.smart_account_address.clone(),
        target_id: row.target_id,
        scheduled_slot_id: row.scheduled_slot_id,
        execution_id: row.execution_id,
    };
    let data = serde_json::to_string(&payload).unwrap_or_else(|_| {
        json!({
            "type": "resync_required",
            "reason": "serialization_failed"
        })
        .to_string()
    });
    Event::default()
        .id(row.id.to_string())
        .event("loyal_yield")
        .data(data)
}

fn resync_required_event(reason: &str) -> Event {
    Event::default().event("loyal_yield").data(
        json!({
            "type": "resync_required",
            "reason": reason
        })
        .to_string(),
    )
}

fn parse_last_event_id(headers: &HeaderMap) -> Option<i64> {
    headers
        .get("last-event-id")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.trim().parse::<i64>().ok())
}

fn verify_token(token: &str, secret: &[u8]) -> Result<RealtimeTokenClaims, BoxError> {
    let (encoded_payload, encoded_signature) = token
        .split_once('.')
        .ok_or("token must contain payload and signature")?;
    if encoded_signature.contains('.') {
        return Err("token must contain exactly one separator".into());
    }

    let signature = URL_SAFE_NO_PAD.decode(encoded_signature)?;
    let mut mac = HmacSha256::new_from_slice(secret)?;
    mac.update(encoded_payload.as_bytes());
    let expected = mac.finalize().into_bytes();
    if expected.as_slice().ct_eq(signature.as_slice()).unwrap_u8() != 1 {
        return Err("token signature mismatch".into());
    }

    let payload = URL_SAFE_NO_PAD.decode(encoded_payload)?;
    let claims: RealtimeTokenClaims = serde_json::from_slice(&payload)?;
    if claims.exp <= Utc::now().timestamp() {
        return Err("token expired".into());
    }
    if claims.wallet_address.is_none() && claims.settings_pda.is_none() {
        return Err("token must include walletAddress or settingsPda".into());
    }
    if claims.solana_env.trim().is_empty() {
        return Err("token solanaEnv is required".into());
    }
    if claims.scopes.is_empty() {
        return Err("token scopes are required".into());
    }
    Ok(claims)
}

fn reject_pooled_connection_url(database_url: &str) -> Result<(), BoxError> {
    let parsed = url::Url::parse(database_url)?;
    if parsed
        .host_str()
        .map(|host| host.contains("-pooler."))
        .unwrap_or(false)
    {
        return Err(
            "NEON_DATABASE_URL uses a pooled -pooler host; LISTEN/NOTIFY requires a direct connection"
                .into(),
        );
    }
    Ok(())
}

fn cors_layer(config: &Config) -> CorsLayer {
    let mut layer = CorsLayer::new().allow_methods([Method::GET]);
    layer = layer.allow_headers([
        header::ACCEPT,
        header::AUTHORIZATION,
        header::CONTENT_TYPE,
        HeaderNameCompat::last_event_id(),
    ]);
    if !config.allowed_origins.is_empty() {
        layer = layer.allow_origin(AllowOrigin::list(config.allowed_origins.clone()));
    }
    layer
}

struct HeaderNameCompat;

impl HeaderNameCompat {
    fn last_event_id() -> header::HeaderName {
        header::HeaderName::from_static("last-event-id")
    }
}

impl Config {
    fn from_env() -> Result<Self, BoxError> {
        let database_url = env::var("NEON_DATABASE_URL")
            .map_err(|_| "NEON_DATABASE_URL must be set for realtime service")?;
        let auth_secret = env::var("REALTIME_AUTH_SECRET")
            .map_err(|_| "REALTIME_AUTH_SECRET must be set for realtime service")?;
        if auth_secret.len() < 32 {
            return Err("REALTIME_AUTH_SECRET must be at least 32 bytes".into());
        }

        Ok(Self {
            database_url,
            auth_secret: Arc::from(auth_secret.into_bytes()),
            allowed_origins: parse_allowed_origins()?,
            heartbeat_seconds: parse_env_u64(
                "REALTIME_HEARTBEAT_SECONDS",
                DEFAULT_HEARTBEAT_SECONDS,
            ),
            catch_up_limit: parse_env_i64("REALTIME_CATCH_UP_LIMIT", DEFAULT_CATCH_UP_LIMIT).max(1),
            channel: env::var("REALTIME_CHANNEL").unwrap_or_else(|_| DEFAULT_CHANNEL.to_owned()),
            port: parse_env_u16("PORT", 10000),
        })
    }
}

fn parse_allowed_origins() -> Result<Vec<HeaderValue>, BoxError> {
    let Some(value) = env::var("REALTIME_ALLOWED_ORIGINS").ok() else {
        return Ok(Vec::new());
    };
    value
        .split(',')
        .map(str::trim)
        .filter(|origin| !origin.is_empty())
        .map(|origin| HeaderValue::from_str(origin).map_err(Into::into))
        .collect()
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
