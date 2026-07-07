use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine as _};
use chrono::Utc;
use hmac::{Hmac, Mac};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::Sha256;
use sqlx::PgPool;
use subtle::ConstantTimeEq;

pub const DEFAULT_REALTIME_CHANNEL: &str = "loyal_yield_realtime";
pub const DEFAULT_SOLANA_ENV: &str = "mainnet-beta";
pub const SCOPE_AUTODEPOSIT: &str = "autodeposit";
pub const SCOPE_EARN: &str = "earn";
pub const SCOPE_ONBOARDING: &str = "onboarding";
pub const EVENT_AUTODEPOSIT_SLOT_CHANGED: &str = "autodeposit_slot_changed";
pub const EVENT_EARN_AUTODEPOSIT_SWEEP_REQUESTED: &str = "earn.autodeposit.sweep_requested";
pub const EVENT_EARN_AUTODEPOSIT_SWEEP_SELECTED: &str = "earn.autodeposit.sweep_selected";
pub const EVENT_EARN_AUTODEPOSIT_SWEEP_EXECUTED: &str = "earn.autodeposit.sweep_executed";
pub const EVENT_EARN_POSITION_CHANGED: &str = "earn.position.changed";
pub const EVENT_EARN_TRANSACTION_RECORDED: &str = "earn.transaction.recorded";
pub const EVENT_EARN_ONBOARDING_CHANGED: &str = "earn.onboarding.changed";

pub mod autodeposit_reasons {
    pub const SCHEDULED_SLOT_SCHEDULED: &str = "scheduled_slot_scheduled";
    pub const SCHEDULED_SLOT_REQUESTED: &str = "scheduled_slot_requested";
    pub const SCHEDULED_SLOT_SELECTED: &str = "scheduled_slot_selected";
    pub const SCHEDULED_SLOT_EXECUTED: &str = "scheduled_slot_executed";
    pub const SCHEDULED_SLOT_FAILED: &str = "scheduled_slot_failed";
    pub const SCHEDULED_SLOT_RELEASED: &str = "scheduled_slot_released";
}

pub type BoxError = Box<dyn std::error::Error + Send + Sync>;
type HmacSha256 = Hmac<Sha256>;

#[derive(Debug, Clone, Deserialize)]
pub struct RealtimeTokenClaims {
    pub exp: i64,
    #[serde(rename = "walletAddress", default)]
    pub wallet_address: Option<String>,
    #[serde(rename = "settingsPda", default)]
    pub settings_pda: Option<String>,
    #[serde(rename = "smartAccountAddress", default)]
    pub smart_account_address: Option<String>,
    #[serde(rename = "solanaEnv", default = "default_solana_env")]
    pub solana_env: String,
    #[serde(default)]
    pub scopes: Vec<String>,
}

#[derive(Debug, Clone, sqlx::FromRow)]
pub struct RealtimeEventRow {
    pub id: i64,
    pub event_type: String,
    pub scope: String,
    pub reason: String,
    pub solana_env: Option<String>,
    pub wallet_address: Option<String>,
    pub settings_pda: Option<String>,
    pub smart_account_address: Option<String>,
    pub vault_pubkey: Option<String>,
    pub target_id: Option<i64>,
    pub scheduled_slot_id: Option<i64>,
    pub execution_id: Option<i64>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RealtimeInvalidation {
    #[serde(rename = "type")]
    pub event_type: String,
    pub event_id: i64,
    pub scope: String,
    pub reason: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub solana_env: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub wallet_address: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub settings_pda: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub smart_account_address: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub vault_pubkey: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub target_id: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub scheduled_slot_id: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub execution_id: Option<i64>,
}

pub fn verify_hmac_token(token: &str, secret: &[u8]) -> Result<RealtimeTokenClaims, BoxError> {
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
    validate_claims(&claims)?;
    Ok(claims)
}

pub fn validate_claims(claims: &RealtimeTokenClaims) -> Result<(), BoxError> {
    if claims.exp <= Utc::now().timestamp() {
        return Err("token expired".into());
    }
    if claims.wallet_address.is_none()
        && claims.settings_pda.is_none()
        && claims.smart_account_address.is_none()
    {
        return Err("token must include walletAddress, settingsPda, or smartAccountAddress".into());
    }
    if claims.solana_env.trim().is_empty() {
        return Err("token solanaEnv cannot be empty".into());
    }
    if claims.scopes.is_empty() {
        return Err("token scopes are required".into());
    }
    Ok(())
}

pub fn default_solana_env() -> String {
    DEFAULT_SOLANA_ENV.to_owned()
}

pub fn notification_event_id_from_payload(payload: &str) -> Option<i64> {
    let value = serde_json::from_str::<serde_json::Value>(payload).ok()?;
    value.get("event_id")?.as_i64()
}

pub fn event_matches_claims(row: &RealtimeEventRow, claims: &RealtimeTokenClaims) -> bool {
    if private_event_requires_identity(&row.scope, &row.event_type) && !row_has_identity(row) {
        return false;
    }
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

pub fn private_scope_requires_identity(scope: &str) -> bool {
    matches!(scope, SCOPE_AUTODEPOSIT | SCOPE_EARN | SCOPE_ONBOARDING)
}

pub fn private_event_requires_identity(scope: &str, event_type: &str) -> bool {
    private_scope_requires_identity(scope) || event_type.starts_with("earn.")
}

pub fn row_has_identity(row: &RealtimeEventRow) -> bool {
    row.wallet_address.is_some()
        || row.settings_pda.is_some()
        || row.smart_account_address.is_some()
}

pub fn invalidation_for_row(row: &RealtimeEventRow) -> RealtimeInvalidation {
    RealtimeInvalidation {
        event_type: row.event_type.clone(),
        event_id: row.id,
        scope: row.scope.clone(),
        reason: row.reason.clone(),
        solana_env: row.solana_env.clone(),
        wallet_address: row.wallet_address.clone(),
        settings_pda: row.settings_pda.clone(),
        smart_account_address: row.smart_account_address.clone(),
        vault_pubkey: row.vault_pubkey.clone(),
        target_id: row.target_id,
        scheduled_slot_id: row.scheduled_slot_id,
        execution_id: row.execution_id,
    }
}

pub fn invalidation_json_for_row(row: &RealtimeEventRow) -> String {
    serde_json::to_string(&invalidation_for_row(row)).unwrap_or_else(|_| {
        json!({
            "type": "resync_required",
            "reason": "serialization_failed"
        })
        .to_string()
    })
}

pub fn resync_required_json(reason: &str) -> String {
    json!({
        "type": "resync_required",
        "reason": reason
    })
    .to_string()
}

pub async fn latest_event_id(pool: &PgPool) -> Result<i64, sqlx::Error> {
    let cursor: Option<i64> = sqlx::query_scalar("SELECT MAX(id) FROM loyal_yield.realtime_events")
        .fetch_one(pool)
        .await?;
    Ok(cursor.unwrap_or(0))
}

pub async fn min_event_id(pool: &PgPool) -> Result<Option<i64>, sqlx::Error> {
    sqlx::query_scalar("SELECT MIN(id) FROM loyal_yield.realtime_events")
        .fetch_one(pool)
        .await
}

pub async fn fetch_events_after(
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
            vault_pubkey,
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

pub async fn fetch_event_by_id(
    pool: &PgPool,
    event_id: i64,
) -> Result<Option<RealtimeEventRow>, sqlx::Error> {
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
            vault_pubkey,
            target_id,
            scheduled_slot_id,
            execution_id
        FROM loyal_yield.realtime_events
        WHERE id = $1
        "#,
    )
    .bind(event_id)
    .fetch_optional(pool)
    .await
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

    fn claims() -> RealtimeTokenClaims {
        RealtimeTokenClaims {
            exp: Utc::now().timestamp() + 60,
            wallet_address: Some("wallet-1".to_owned()),
            settings_pda: Some("settings-1".to_owned()),
            smart_account_address: Some("smart-1".to_owned()),
            solana_env: DEFAULT_SOLANA_ENV.to_owned(),
            scopes: vec![SCOPE_EARN.to_owned()],
        }
    }

    fn event_row(scope: &str) -> RealtimeEventRow {
        RealtimeEventRow {
            id: 1,
            event_type: EVENT_EARN_TRANSACTION_RECORDED.to_owned(),
            scope: scope.to_owned(),
            reason: "test".to_owned(),
            solana_env: Some(DEFAULT_SOLANA_ENV.to_owned()),
            wallet_address: Some("wallet-1".to_owned()),
            settings_pda: Some("settings-1".to_owned()),
            smart_account_address: Some("smart-1".to_owned()),
            vault_pubkey: None,
            target_id: None,
            scheduled_slot_id: None,
            execution_id: None,
        }
    }

    #[test]
    fn private_event_without_identity_never_matches() {
        let mut row = event_row(SCOPE_EARN);
        row.wallet_address = None;
        row.settings_pda = None;
        row.smart_account_address = None;

        assert!(!event_matches_claims(&row, &claims()));
    }

    #[test]
    fn private_event_requires_matching_identity_fields() {
        let row = event_row(SCOPE_EARN);
        assert!(event_matches_claims(&row, &claims()));

        let mut mismatched = claims();
        mismatched.settings_pda = Some("settings-2".to_owned());
        assert!(!event_matches_claims(&row, &mismatched));
    }

    #[test]
    fn smart_account_only_claim_is_valid_identity() {
        let mut smart_only = claims();
        smart_only.wallet_address = None;
        smart_only.settings_pda = None;

        assert!(validate_claims(&smart_only).is_ok());
    }
}
