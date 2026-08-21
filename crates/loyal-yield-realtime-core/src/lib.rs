use std::fmt;

use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine as _};
use chrono::{DateTime, Utc};
use hmac::{Hmac, Mac};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::Sha256;
use sqlx::PgPool;
use subtle::ConstantTimeEq;

pub const DEFAULT_REALTIME_CHANNEL: &str = "loyal_yield_realtime";
pub const DEFAULT_SOLANA_ENV: &str = "mainnet-beta";
pub const REALTIME_TOKEN_VERSION: u8 = 1;
pub const REALTIME_TOKEN_ISSUER: &str = "loyal-apps";
pub const REALTIME_TOKEN_AUDIENCE: &str = "loyal-yield-realtime";
pub const DEFAULT_MAX_TOKEN_LIFETIME_SECONDS: i64 = 5 * 60;
pub const SCOPE_AUTODEPOSIT: &str = "autodeposit";
pub const SCOPE_EARN: &str = "earn";
pub const SCOPE_ONBOARDING: &str = "onboarding";
pub const EVENT_AUTODEPOSIT_CONFIGURATION_CHANGED: &str = "earn.autodeposit.configuration.changed";
pub const EVENT_AUTODEPOSIT_EXECUTION_CHANGED: &str = "earn.autodeposit.execution.changed";
pub const EVENT_EARN_POSITION_CHANGED: &str = "earn.position.changed";
pub const EVENT_EARN_REBALANCE_CONFIRMED: &str = "earn.rebalance.confirmed";
pub const EVENT_EARN_TRANSACTION_RECORDED: &str = "earn.transaction.recorded";
pub const EVENT_EARN_ONBOARDING_CHANGED: &str = "earn.onboarding.changed";
pub const EVENT_EARN_AUTOSWAP_CONFIGURATION_CHANGED: &str = "earn.autoswap.configuration.changed";

pub mod autodeposit_states {
    pub const SCHEDULED: &str = "scheduled";
    pub const REQUESTED: &str = "requested";
    pub const SELECTED: &str = "selected";
    pub const PULL_CONFIRMED: &str = "pull_confirmed";
    pub const COMPLETED: &str = "completed";
    pub const FAILED: &str = "failed";
    pub const CANCELED: &str = "canceled";
    pub const RELEASED: &str = "released";
}

pub type BoxError = Box<dyn std::error::Error + Send + Sync>;
type HmacSha256 = Hmac<Sha256>;

#[derive(Debug, Clone, Copy, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum RealtimeClientKind {
    Web,
    Mobile,
}

impl RealtimeClientKind {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Web => "web",
            Self::Mobile => "mobile",
        }
    }
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct RealtimeTokenClaims {
    pub v: u8,
    pub iss: String,
    pub aud: String,
    pub iat: i64,
    pub exp: i64,
    #[serde(rename = "walletAddress")]
    pub wallet_address: String,
    #[serde(rename = "settingsPda")]
    pub settings_pda: String,
    #[serde(rename = "earnVaultAddress")]
    pub earn_vault_address: String,
    #[serde(rename = "solanaEnv")]
    pub solana_env: String,
    pub scopes: Vec<String>,
    #[serde(rename = "clientKind")]
    pub client_kind: RealtimeClientKind,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AuthFailureReason {
    Malformed,
    Signature,
    Version,
    Issuer,
    Audience,
    IssuedAt,
    Expired,
    Lifetime,
    Identity,
    Cluster,
    Scope,
    ClientKind,
}

impl AuthFailureReason {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Malformed => "malformed",
            Self::Signature => "signature",
            Self::Version => "version",
            Self::Issuer => "issuer",
            Self::Audience => "audience",
            Self::IssuedAt => "issued_at",
            Self::Expired => "expired",
            Self::Lifetime => "lifetime",
            Self::Identity => "identity",
            Self::Cluster => "cluster",
            Self::Scope => "scope",
            Self::ClientKind => "client_kind",
        }
    }
}

#[derive(Debug)]
pub struct AuthError {
    reason: AuthFailureReason,
}

impl AuthError {
    fn new(reason: AuthFailureReason) -> Self {
        Self { reason }
    }

    pub fn reason(&self) -> AuthFailureReason {
        self.reason
    }
}

impl fmt::Display for AuthError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("realtime token rejected")
    }
}

impl std::error::Error for AuthError {}

#[derive(Debug, Clone)]
pub struct RealtimeEventRow {
    pub id: i64,
    pub created_at: DateTime<Utc>,
    pub event_type: String,
    pub scope: String,
    pub reason: String,
    pub solana_env: Option<String>,
    pub wallet_address: Option<String>,
    pub settings_pda: Option<String>,
    pub earn_vault_address: Option<String>,
    pub target_id: Option<i64>,
    pub scheduled_slot_id: Option<i64>,
    pub execution_id: Option<i64>,
    pub failure_code: Option<String>,
}

impl<'row> sqlx::FromRow<'row, sqlx::postgres::PgRow> for RealtimeEventRow {
    fn from_row(row: &'row sqlx::postgres::PgRow) -> Result<Self, sqlx::Error> {
        use sqlx::Row;

        Ok(Self {
            id: row.try_get("id")?,
            created_at: row.try_get("created_at")?,
            event_type: row.try_get("event_type")?,
            scope: row.try_get("scope")?,
            reason: row.try_get("reason")?,
            solana_env: row.try_get("solana_env")?,
            wallet_address: row.try_get("wallet_address")?,
            settings_pda: row.try_get("settings_pda")?,
            earn_vault_address: row.try_get("earn_vault_address")?,
            target_id: row.try_get("target_id")?,
            scheduled_slot_id: row.try_get("scheduled_slot_id")?,
            execution_id: row.try_get("execution_id")?,
            failure_code: row.try_get("failure_code")?,
        })
    }
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RealtimeInvalidation {
    pub schema_version: u8,
    pub event_id: String,
    pub event_type: String,
    pub occurred_at: DateTime<Utc>,
    pub scope: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub state: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub target_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub scheduled_slot_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub execution_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub failure_code: Option<String>,
}

pub fn verify_hmac_token(token: &str, secret: &[u8]) -> Result<RealtimeTokenClaims, AuthError> {
    verify_hmac_token_with_secrets(
        token,
        secret,
        None,
        Utc::now().timestamp(),
        DEFAULT_MAX_TOKEN_LIFETIME_SECONDS,
    )
}

pub fn verify_hmac_token_with_secrets(
    token: &str,
    current_secret: &[u8],
    previous_secret: Option<&[u8]>,
    now: i64,
    max_lifetime_seconds: i64,
) -> Result<RealtimeTokenClaims, AuthError> {
    let (encoded_payload, encoded_signature) = token
        .split_once('.')
        .ok_or_else(|| AuthError::new(AuthFailureReason::Malformed))?;
    if encoded_signature.contains('.') {
        return Err(AuthError::new(AuthFailureReason::Malformed));
    }

    let signature = URL_SAFE_NO_PAD
        .decode(encoded_signature)
        .map_err(|_| AuthError::new(AuthFailureReason::Malformed))?;
    if signature.len() != 32 {
        return Err(AuthError::new(AuthFailureReason::Malformed));
    }

    let current_matches = signature_matches(encoded_payload, &signature, current_secret)?;
    let previous_matches = match previous_secret {
        Some(secret) => signature_matches(encoded_payload, &signature, secret)?,
        None => false,
    };
    if !(current_matches | previous_matches) {
        return Err(AuthError::new(AuthFailureReason::Signature));
    }

    let payload = URL_SAFE_NO_PAD
        .decode(encoded_payload)
        .map_err(|_| AuthError::new(AuthFailureReason::Malformed))?;
    let claims: RealtimeTokenClaims = serde_json::from_slice(&payload)
        .map_err(|_| AuthError::new(AuthFailureReason::Malformed))?;
    validate_claims_at(&claims, now, max_lifetime_seconds)?;
    Ok(claims)
}

fn signature_matches(
    encoded_payload: &str,
    signature: &[u8],
    secret: &[u8],
) -> Result<bool, AuthError> {
    let mut mac = HmacSha256::new_from_slice(secret)
        .map_err(|_| AuthError::new(AuthFailureReason::Signature))?;
    mac.update(encoded_payload.as_bytes());
    let expected = mac.finalize().into_bytes();
    Ok(expected.as_slice().ct_eq(signature).unwrap_u8() == 1)
}

pub fn validate_claims(claims: &RealtimeTokenClaims) -> Result<(), AuthError> {
    validate_claims_at(
        claims,
        Utc::now().timestamp(),
        DEFAULT_MAX_TOKEN_LIFETIME_SECONDS,
    )
}

pub fn validate_claims_at(
    claims: &RealtimeTokenClaims,
    now: i64,
    max_lifetime_seconds: i64,
) -> Result<(), AuthError> {
    if claims.v != REALTIME_TOKEN_VERSION {
        return Err(AuthError::new(AuthFailureReason::Version));
    }
    if claims.iss != REALTIME_TOKEN_ISSUER {
        return Err(AuthError::new(AuthFailureReason::Issuer));
    }
    if claims.aud != REALTIME_TOKEN_AUDIENCE {
        return Err(AuthError::new(AuthFailureReason::Audience));
    }
    if claims.iat > now {
        return Err(AuthError::new(AuthFailureReason::IssuedAt));
    }
    if claims.exp <= now {
        return Err(AuthError::new(AuthFailureReason::Expired));
    }
    let lifetime = claims
        .exp
        .checked_sub(claims.iat)
        .ok_or_else(|| AuthError::new(AuthFailureReason::Lifetime))?;
    if lifetime <= 0 || lifetime > max_lifetime_seconds {
        return Err(AuthError::new(AuthFailureReason::Lifetime));
    }
    if !is_valid_solana_pubkey(&claims.wallet_address)
        || !is_valid_solana_pubkey(&claims.settings_pda)
        || !is_valid_solana_pubkey(&claims.earn_vault_address)
    {
        return Err(AuthError::new(AuthFailureReason::Identity));
    }
    if !matches!(claims.solana_env.as_str(), "mainnet-beta" | "devnet") {
        return Err(AuthError::new(AuthFailureReason::Cluster));
    }
    if claims.scopes.is_empty()
        || claims.scopes.iter().any(|scope| {
            !matches!(
                scope.as_str(),
                SCOPE_AUTODEPOSIT | SCOPE_EARN | SCOPE_ONBOARDING
            )
        })
    {
        return Err(AuthError::new(AuthFailureReason::Scope));
    }
    if !matches!(
        claims.client_kind,
        RealtimeClientKind::Web | RealtimeClientKind::Mobile
    ) {
        return Err(AuthError::new(AuthFailureReason::ClientKind));
    }
    Ok(())
}

fn is_valid_solana_pubkey(value: &str) -> bool {
    bs58::decode(value)
        .into_vec()
        .map(|bytes| bytes.len() == 32)
        .unwrap_or(false)
}

pub fn notification_event_id_from_payload(payload: &str) -> Option<i64> {
    let value = serde_json::from_str::<serde_json::Value>(payload).ok()?;
    value.get("event_id")?.as_i64()
}

pub fn event_matches_claims(row: &RealtimeEventRow, claims: &RealtimeTokenClaims) -> bool {
    claims.scopes.iter().any(|scope| scope == &row.scope)
        && row.solana_env.as_deref() == Some(claims.solana_env.as_str())
        && row.wallet_address.as_deref() == Some(claims.wallet_address.as_str())
        && row.settings_pda.as_deref() == Some(claims.settings_pda.as_str())
        && row.earn_vault_address.as_deref() == Some(claims.earn_vault_address.as_str())
}

pub fn invalidation_for_row(row: &RealtimeEventRow) -> RealtimeInvalidation {
    let is_autodeposit_progress = row.event_type == EVENT_AUTODEPOSIT_EXECUTION_CHANGED;
    RealtimeInvalidation {
        schema_version: 1,
        event_id: row.id.to_string(),
        event_type: row.event_type.clone(),
        occurred_at: row.created_at,
        scope: row.scope.clone(),
        state: is_autodeposit_progress.then(|| row.reason.clone()),
        reason: (!is_autodeposit_progress).then(|| row.reason.clone()),
        target_id: row.target_id.map(|value| value.to_string()),
        scheduled_slot_id: row.scheduled_slot_id.map(|value| value.to_string()),
        execution_id: row.execution_id.map(|value| value.to_string()),
        failure_code: row.failure_code.clone(),
    }
}

pub fn invalidation_json_for_row(row: &RealtimeEventRow) -> String {
    serde_json::to_string(&invalidation_for_row(row))
        .unwrap_or_else(|_| resync_required_json("serialization_failed"))
}

pub fn resync_required_json(reason: &str) -> String {
    json!({
        "schemaVersion": 1,
        "eventType": "resync_required",
        "reason": reason
    })
    .to_string()
}

const EVENT_SELECT: &str = r#"
    SELECT
        id,
        created_at,
        event_type,
        scope,
        reason,
        solana_env,
        wallet_address,
        settings_pda,
        earn_vault_address,
        target_id,
        scheduled_slot_id,
        execution_id,
        failure_code
    FROM loyal_yield.realtime_events
"#;

pub async fn latest_event_id(pool: &PgPool) -> Result<i64, sqlx::Error> {
    let cursor: Option<i64> = sqlx::query_scalar(
        "SELECT MAX(id) FROM loyal_yield.realtime_events WHERE deliverable = TRUE",
    )
    .fetch_one(pool)
    .await?;
    Ok(cursor.unwrap_or(0))
}

pub async fn min_event_id(pool: &PgPool) -> Result<Option<i64>, sqlx::Error> {
    sqlx::query_scalar("SELECT MIN(id) FROM loyal_yield.realtime_events WHERE deliverable = TRUE")
        .fetch_one(pool)
        .await
}

pub async fn fetch_events_after(
    pool: &PgPool,
    cursor: i64,
    limit: i64,
) -> Result<Vec<RealtimeEventRow>, sqlx::Error> {
    sqlx::query_as::<_, RealtimeEventRow>(&format!(
        "{EVENT_SELECT} WHERE deliverable = TRUE AND id > $1 ORDER BY id ASC LIMIT $2"
    ))
    .bind(cursor)
    .bind(limit)
    .fetch_all(pool)
    .await
}

pub async fn fetch_matching_events_after(
    pool: &PgPool,
    claims: &RealtimeTokenClaims,
    cursor: i64,
    high_water: i64,
    limit: i64,
) -> Result<Vec<RealtimeEventRow>, sqlx::Error> {
    sqlx::query_as::<_, RealtimeEventRow>(&format!(
        r#"{EVENT_SELECT}
        WHERE deliverable = TRUE
          AND id > $1
          AND id <= $2
          AND solana_env = $3
          AND wallet_address = $4
          AND settings_pda = $5
          AND earn_vault_address = $6
          AND scope = ANY($7)
        ORDER BY id ASC
        LIMIT $8"#
    ))
    .bind(cursor)
    .bind(high_water)
    .bind(&claims.solana_env)
    .bind(&claims.wallet_address)
    .bind(&claims.settings_pda)
    .bind(&claims.earn_vault_address)
    .bind(&claims.scopes)
    .bind(limit)
    .fetch_all(pool)
    .await
}

pub async fn fetch_event_by_id(
    pool: &PgPool,
    event_id: i64,
) -> Result<Option<RealtimeEventRow>, sqlx::Error> {
    sqlx::query_as::<_, RealtimeEventRow>(&format!(
        "{EVENT_SELECT} WHERE deliverable = TRUE AND id = $1"
    ))
    .bind(event_id)
    .fetch_optional(pool)
    .await
}

pub async fn cleanup_expired_events_batch(
    pool: &PgPool,
    retention_days: i64,
    batch_size: i64,
) -> Result<u64, sqlx::Error> {
    let result = sqlx::query(
        r#"
        WITH expired AS (
            SELECT id
            FROM loyal_yield.realtime_events
            WHERE created_at < now() - ($1::bigint * interval '1 day')
            ORDER BY id ASC
            LIMIT $2
        )
        DELETE FROM loyal_yield.realtime_events AS event
        USING expired
        WHERE event.id = expired.id
        "#,
    )
    .bind(retention_days)
    .bind(batch_size)
    .execute(pool)
    .await?;
    Ok(result.rows_affected())
}

pub fn neon_url_looks_pooled(postgres_url: &str) -> bool {
    url::Url::parse(postgres_url)
        .ok()
        .and_then(|parsed| parsed.host_str().map(str::to_owned))
        .map(|host| host.contains("-pooler."))
        .unwrap_or_else(|| {
            postgres_url.contains("-pooler.")
                || postgres_url.contains("-pooler:")
                || postgres_url.contains("-pooler/")
        })
}

pub fn reject_pooled_connection_url(database_url: &str) -> Result<(), BoxError> {
    if neon_url_looks_pooled(database_url) {
        return Err(
            "NEON_DATABASE_URL uses a pooled -pooler host; LISTEN/NOTIFY requires a direct connection"
                .into(),
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    const PUBKEY: &str = "11111111111111111111111111111111";

    fn claims(now: i64) -> RealtimeTokenClaims {
        RealtimeTokenClaims {
            v: REALTIME_TOKEN_VERSION,
            iss: REALTIME_TOKEN_ISSUER.to_owned(),
            aud: REALTIME_TOKEN_AUDIENCE.to_owned(),
            iat: now,
            exp: now + 60,
            wallet_address: PUBKEY.to_owned(),
            settings_pda: PUBKEY.to_owned(),
            earn_vault_address: PUBKEY.to_owned(),
            solana_env: DEFAULT_SOLANA_ENV.to_owned(),
            scopes: vec![SCOPE_EARN.to_owned()],
            client_kind: RealtimeClientKind::Web,
        }
    }

    fn event_row(scope: &str) -> RealtimeEventRow {
        RealtimeEventRow {
            id: 9_007_199_254_740_993,
            created_at: Utc::now(),
            event_type: EVENT_EARN_TRANSACTION_RECORDED.to_owned(),
            scope: scope.to_owned(),
            reason: "test".to_owned(),
            solana_env: Some(DEFAULT_SOLANA_ENV.to_owned()),
            wallet_address: Some(PUBKEY.to_owned()),
            settings_pda: Some(PUBKEY.to_owned()),
            earn_vault_address: Some(PUBKEY.to_owned()),
            target_id: Some(1),
            scheduled_slot_id: Some(2),
            execution_id: Some(3),
            failure_code: None,
        }
    }

    fn sign(claims: &RealtimeTokenClaims, secret: &[u8]) -> String {
        let encoded = URL_SAFE_NO_PAD.encode(serde_json::to_vec(claims).unwrap());
        let mut mac = HmacSha256::new_from_slice(secret).unwrap();
        mac.update(encoded.as_bytes());
        format!(
            "{encoded}.{}",
            URL_SAFE_NO_PAD.encode(mac.finalize().into_bytes())
        )
    }

    #[test]
    fn strict_claims_accept_current_and_previous_secret() {
        let now = 1_000_000;
        let token_claims = claims(now);
        let current = b"current-secret-that-is-at-least-32-bytes";
        let previous = b"previous-secret-that-is-at-least-32-bytes";
        let current_token = sign(&token_claims, current);
        let previous_token = sign(&token_claims, previous);

        assert!(
            verify_hmac_token_with_secrets(&current_token, current, Some(previous), now, 300)
                .is_ok()
        );
        assert!(
            verify_hmac_token_with_secrets(&previous_token, current, Some(previous), now, 300)
                .is_ok()
        );
    }

    #[test]
    fn strict_claims_reject_wrong_contract_and_lifetime() {
        let now = 1_000_000;
        let mut token_claims = claims(now);
        token_claims.aud = "wrong".to_owned();
        assert_eq!(
            validate_claims_at(&token_claims, now, 300)
                .unwrap_err()
                .reason(),
            AuthFailureReason::Audience
        );

        token_claims.aud = REALTIME_TOKEN_AUDIENCE.to_owned();
        token_claims.exp = now + 301;
        assert_eq!(
            validate_claims_at(&token_claims, now, 300)
                .unwrap_err()
                .reason(),
            AuthFailureReason::Lifetime
        );
    }

    #[test]
    fn private_event_requires_every_exact_identity_field() {
        let now = Utc::now().timestamp();
        let row = event_row(SCOPE_EARN);
        let token_claims = claims(now);
        assert!(event_matches_claims(&row, &token_claims));

        let mut incomplete = row.clone();
        incomplete.earn_vault_address = None;
        assert!(!event_matches_claims(&incomplete, &token_claims));

        let mut mismatched = token_claims;
        mismatched.solana_env = "devnet".to_owned();
        assert!(!event_matches_claims(&row, &mismatched));
    }

    #[test]
    fn invalidation_serializes_large_ids_as_strings() {
        let payload = invalidation_for_row(&event_row(SCOPE_EARN));
        let json = serde_json::to_value(payload).unwrap();
        assert_eq!(json["eventId"], "9007199254740993");
        assert_eq!(json["targetId"], "1");
    }
}
