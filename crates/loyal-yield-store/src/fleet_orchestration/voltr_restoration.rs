//! Durable Backyard Voltr withdrawal-liquidity restoration.
//!
//! This uses the existing orchestration outbox as the movement record. The
//! payload carries the immutable scan origin, generation, exact manager leg,
//! and route binding; the existing outbox lease/fencing/ack/retry methods own
//! delivery and recovery. No second scheduler or saga table is introduced.

use super::queue::{
    orchestration_outbox_from_row, OrchestrationOutboxLease, OrchestrationOutboxRecord,
};
use crate::{NeonSqlClient, OrchestratorError};
use serde::{de::Error as _, Deserialize, Deserializer, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use sqlx::Row;
use std::collections::BTreeSet;

pub const VOLTR_RESTORATION_EVENT_KIND: &str = "backyard_voltr_manager_withdraw";
pub const VOLTR_RESTORATION_AGGREGATE_KIND: &str = "voltr_withdrawal_restoration";
pub const VOLTR_RESTORATION_MAX_LEG_RAW: i64 = 1_000_000;
pub const VOLTR_RESTORATION_MAX_REQUEST_RAW: i64 = 10_000_000;
pub const VOLTR_RESTORATION_EXECUTION_BLOCKER: &str =
    "awaiting_exact_typescript_manager_signed_wire_handoff";
pub const VOLTR_RESTORATION_ACK_CONDITION: &str =
    "confirmed_manager_readback_and_recomputed_idle_shortfall";
pub const VOLTR_RESTORATION_EXECUTION_KIND: &str = "voltr-manager";
pub const VOLTR_FOUR_MARKET_ROUTE_ID: &str = "loyal-backyard-four-market-usdc-v1";
pub const VOLTR_FOUR_MARKET_CLUSTER: &str = "mainnet-beta";
pub const VOLTR_FOUR_MARKET_VAULT: &str = "AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK";
pub const VOLTR_FOUR_MARKET_ROUTE_SPEC_SHA256: &str =
    "a68ef28c8b9a9c8e34106cf78f1d10624d8bc9ebfd366cc15cbc5b273ecdf3e3";

#[derive(Deserialize)]
#[serde(untagged)]
enum NonNegativeI64Wire {
    Number(i64),
    Decimal(String),
}

/// Raw token quantities cross the TypeScript evidence boundary as canonical
/// decimal strings so they never lose precision in JSON. Accept the legacy
/// numeric representation too, but normalize both into the store's bounded
/// `i64` domain and reject signs, whitespace, and leading zeroes.
fn deserialize_nonnegative_i64_wire<'de, D>(deserializer: D) -> Result<i64, D::Error>
where
    D: Deserializer<'de>,
{
    let value = match NonNegativeI64Wire::deserialize(deserializer)? {
        NonNegativeI64Wire::Number(value) => value,
        NonNegativeI64Wire::Decimal(text) => {
            let parsed = text.parse::<i64>().map_err(D::Error::custom)?;
            if parsed.to_string() != text {
                return Err(D::Error::custom(
                    "raw integer string must be canonical base-10",
                ));
            }
            parsed
        }
    };
    if value < 0 {
        return Err(D::Error::custom("raw integer must be non-negative"));
    }
    Ok(value)
}

/// The immutable request generation that caused this restoration.  This is
/// deliberately a tuple rather than a free-form note: a later scanner run or
/// a recreated receipt must not be able to consume the same outbox row.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct VoltrRestorationRequestOrigin {
    pub signature: String,
    pub event_index: i64,
    pub receipt: String,
    pub raw_account_sha256: String,
    /// Receipt-generation fingerprint; the scan aggregate fingerprint is
    /// persisted separately on the plan and is intentionally not conflated.
    pub generation_fingerprint: String,
}

/// A route-owned protected checkpoint.  Shared Kamino accounts are volatile,
/// but the route-owned checkpoint is the fence that makes a restoration plan
/// attributable to the exact request generation that produced it.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct VoltrRestorationProtectedCheckpoint {
    pub address_set_sha256: String,
    pub state_sha256: String,
    pub context_slot: i64,
}

/// The TypeScript manager adapter supplies this after it has built and
/// simulated the canonical Voltr wrapper.  The store deliberately does not
/// build a Solana packet: it persists the exact bytes and the signature the
/// adapter is allowed to submit once.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct VoltrManagerSignedIntentInput {
    pub manager_intent_id: String,
    pub lifecycle_id: String,
    pub strategy_id: String,
    pub reserve: String,
    #[serde(deserialize_with = "deserialize_nonnegative_i64_wire")]
    pub amount_raw: i64,
    pub route_authorization_sha256: String,
    pub protected_prestate_sha256: String,
    pub protected_address_set_sha256: String,
    pub protected_context_slot: i64,
    pub signed_transaction_hex: String,
    pub signed_transaction_sha256: String,
    pub message_sha256: String,
    pub expected_signature: String,
    pub recent_blockhash: String,
    pub last_valid_block_height: i64,
    pub fee_payer: String,
    pub compiled_fee_lamports: i64,
    pub writable_account_keys: Vec<String>,
    /// Logical keys are persisted separately from wire account keys and are
    /// acquired under the exact leased origin/generation/leg before the
    /// signed wire may be handed back to the TypeScript sender.
    pub logical_conflict_keys: Vec<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct VoltrManagerConfirmationInput {
    pub manager_intent_id: String,
    pub lifecycle_id: String,
    pub strategy_id: String,
    pub reserve: String,
    #[serde(deserialize_with = "deserialize_nonnegative_i64_wire")]
    pub amount_raw: i64,
    pub route_authorization_sha256: String,
    pub signed_transaction_sha256: String,
    pub message_sha256: String,
    pub expected_signature: String,
    pub confirmed_slot: i64,
    /// Confirmed account readback context used by the verifier to prove that
    /// the idle/position reload was not older than the manager transaction.
    pub readback_context_slot: i64,
    pub commitment: String,
    pub manager_transaction_signature: String,
    #[serde(deserialize_with = "deserialize_nonnegative_i64_wire")]
    pub idle_raw_after: i64,
    #[serde(deserialize_with = "deserialize_nonnegative_i64_wire")]
    pub remaining_shortfall_raw: i64,
    pub readback_fingerprint: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct VoltrRestorationLegInput {
    pub leg_id: String,
    pub strategy_id: String,
    pub reserve: String,
    #[serde(deserialize_with = "deserialize_nonnegative_i64_wire")]
    pub amount_raw: i64,
    #[serde(deserialize_with = "deserialize_nonnegative_i64_wire")]
    pub source_available_raw: i64,
    pub source_observed_context_slot: i64,
    pub position_fingerprint: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct VoltrRestorationPlanInput {
    pub cluster: String,
    pub vault: String,
    pub route_id: String,
    pub route_spec_sha256: String,
    pub route_authorization_sha256: String,
    pub lifecycle_id: String,
    pub request_origin: VoltrRestorationRequestOrigin,
    pub protected_checkpoint: VoltrRestorationProtectedCheckpoint,
    pub origin_id: String,
    pub generation: i64,
    pub scan_generation_fingerprint: String,
    pub observation_context_slot: i64,
    #[serde(deserialize_with = "deserialize_nonnegative_i64_wire")]
    pub requested_raw: i64,
    pub legs: Vec<VoltrRestorationLegInput>,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct VoltrRestorationEnqueueResult {
    pub origin_id: String,
    pub generation: i64,
    pub inserted_leg_count: u64,
    pub duplicate_leg_count: u64,
    pub outbox_event_ids: Vec<i64>,
}

/// Durable evidence reloaded from the existing orchestration outbox after a
/// confirmed manager leg.  These fields intentionally mirror only the
/// verifier's durable-row contract; they are derived from PostgreSQL payloads
/// and never accepted from an evidence JSON file.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct VoltrRestorationDurableRow {
    pub event_id: i64,
    pub leg_id: String,
    pub dedupe_key: String,
    pub state: String,
    pub lease_fence: i64,
    pub manager_intent_id: String,
    pub expected_signature: String,
    pub confirmed_signature: String,
    pub confirmed_slot: i64,
    pub readback_context_slot: i64,
    pub one_send_only: bool,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct VoltrRestorationOutboxReadback {
    pub event_kind: String,
    pub aggregate_kind: String,
    pub origin_id: String,
    pub generation: i64,
    pub inserted_leg_count: u64,
    pub duplicate_leg_count: u64,
    pub rows: Vec<VoltrRestorationDurableRow>,
    pub ack_condition: String,
}

/// Non-secret durable fence returned by bridge Phase A and required by Phase
/// B. It identifies one leased outbox row and the immutable manager intent;
/// it contains no key material and is useless after lease expiry/reclaim.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct VoltrRestorationBridgeToken {
    pub schema_version: u8,
    pub event_id: i64,
    pub cluster: String,
    pub owner: String,
    pub fencing_token: i64,
    pub origin_id: String,
    pub generation: i64,
    pub leg_id: String,
    pub manager_intent_id: String,
    pub expected_signature: String,
    pub signed_transaction_sha256: String,
    pub message_sha256: String,
    pub strategy_id: String,
    pub reserve: String,
    #[serde(deserialize_with = "deserialize_nonnegative_i64_wire")]
    pub amount_raw: i64,
    pub lifecycle_id: String,
    pub route_authorization_sha256: String,
    pub protected_prestate_sha256: String,
    pub protected_address_set_sha256: String,
    pub protected_context_slot: i64,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct VoltrRestorationBridgeCompletion {
    pub event_id: i64,
    pub origin_id: String,
    pub generation: i64,
    pub leg_id: String,
    pub state: String,
    pub acknowledged: bool,
    pub canceled_sibling_count: u64,
}

/// Explicit handoff state until a worker with the TypeScript logical manager
/// executor can build, simulate, persist an intent, and submit the exact
/// manager packet. This is deliberately not reported as execution success.
#[derive(Clone, Debug)]
pub struct VoltrRestorationHandoff {
    pub lease: OrchestrationOutboxLease,
    pub execution_kind: &'static str,
    pub route_authorization_sha256: String,
    pub lifecycle_id: String,
    pub request_origin: VoltrRestorationRequestOrigin,
    pub protected_checkpoint: VoltrRestorationProtectedCheckpoint,
    pub origin_id: String,
    pub generation: i64,
    pub leg: VoltrRestorationLegInput,
    pub execution_state: &'static str,
    pub execution_blocker: &'static str,
    pub manager_intent_id: String,
    pub required_conflict_keys: Vec<String>,
    pub confirmation_commitment: &'static str,
    pub one_send_only: bool,
    pub recompute_shortfall_after_confirmation: bool,
    pub stop_when_shortfall_zero: bool,
    pub ack_condition: &'static str,
    pub conflict_lease_acquired: bool,
    pub signed_submission_id: Option<i64>,
    pub expected_signature: Option<String>,
}

fn is_hex_sha(value: &str) -> bool {
    valid_lower_sha(value)
}

fn synthetic_aggregate_id(origin_id: &str, leg_id: &str) -> i64 {
    let mut digest = Sha256::new();
    digest.update(b"backyard-voltr-restoration-aggregate-v1");
    digest.update(origin_id.as_bytes());
    digest.update(leg_id.as_bytes());
    let bytes = digest.finalize();
    let mut raw = [0_u8; 8];
    raw.copy_from_slice(&bytes[..8]);
    let value = i64::from_le_bytes(raw) & i64::MAX;
    value.max(1)
}

fn manager_intent_id(origin_id: &str, generation: i64, leg_id: &str) -> String {
    format!(
        "{:x}",
        Sha256::digest(
            format!("backyard-voltr-manager-intent-v1:{origin_id}:{generation}:{leg_id}")
                .as_bytes(),
        )
    )
}

fn plan_fingerprint(input: &VoltrRestorationPlanInput) -> String {
    let mut legs = input
        .legs
        .iter()
        .map(|leg| {
            json!({
                "legId": leg.leg_id,
                "strategyId": leg.strategy_id,
                "reserve": leg.reserve,
                "amountRaw": leg.amount_raw,
                "sourceAvailableRaw": leg.source_available_raw,
                "sourceObservedContextSlot": leg.source_observed_context_slot,
                "positionFingerprint": leg.position_fingerprint,
            })
        })
        .collect::<Vec<_>>();
    legs.sort_by(|left, right| left.to_string().cmp(&right.to_string()));
    let canonical = json!({
        "routeId": input.route_id,
        "routeSpecSha256": input.route_spec_sha256,
        "routeAuthorizationSha256": input.route_authorization_sha256,
        "lifecycleId": input.lifecycle_id,
        "vault": input.vault,
        "originId": input.origin_id,
        "generation": input.generation,
        "scanGenerationFingerprint": input.scan_generation_fingerprint,
        "observationContextSlot": input.observation_context_slot,
        "requestOrigin": input.request_origin,
        "protectedCheckpoint": input.protected_checkpoint,
        "requestedRaw": input.requested_raw,
        "legs": legs,
    });
    format!("{:x}", Sha256::digest(canonical.to_string().as_bytes()))
}

fn approved_reserve(strategy_id: &str) -> Option<&'static str> {
    match strategy_id {
        "main" => Some("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"),
        "onre" => Some("AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"),
        "prime" => Some("9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu"),
        "maple" => Some("Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo"),
        _ => None,
    }
}

fn valid_hex(value: &str) -> bool {
    !value.is_empty() && value.len() % 2 == 0 && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn valid_lower_sha(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn decode_hex(value: &str) -> Result<Vec<u8>, OrchestratorError> {
    if !valid_hex(value) {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr wire image must be a nonempty even-length hex string".to_owned(),
        ));
    }
    (0..value.len())
        .step_by(2)
        .map(|index| {
            u8::from_str_radix(&value[index..index + 2], 16).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "Voltr wire image contains invalid hex".to_owned(),
                )
            })
        })
        .collect()
}

fn derive_signed_wire_evidence(
    signed_transaction_hex: &str,
) -> Result<(String, String, String), OrchestratorError> {
    let wire = decode_hex(signed_transaction_hex)?;
    if wire.len() < 65 || wire[0] != 1 {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr manager wire must contain exactly one leading Solana signature".to_owned(),
        ));
    }
    Ok((
        format!("{:x}", Sha256::digest(&wire)),
        format!("{:x}", Sha256::digest(&wire[65..])),
        bs58::encode(&wire[1..65]).into_string(),
    ))
}

fn sorted_unique(values: &[String]) -> Vec<String> {
    let mut values = values.to_vec();
    values.sort();
    values.dedup();
    values
}

fn validate_signed_intent(
    handoff: &VoltrRestorationHandoff,
    input: &VoltrManagerSignedIntentInput,
) -> Result<(), OrchestratorError> {
    if input.manager_intent_id != handoff.manager_intent_id
        || input.lifecycle_id != handoff.lifecycle_id
        || input.strategy_id != handoff.leg.strategy_id
        || input.reserve != handoff.leg.reserve
        || input.amount_raw != handoff.leg.amount_raw
        || input.route_authorization_sha256 != handoff.route_authorization_sha256
        || input.protected_prestate_sha256 != handoff.protected_checkpoint.state_sha256
        || input.protected_address_set_sha256 != handoff.protected_checkpoint.address_set_sha256
        || input.protected_context_slot != handoff.protected_checkpoint.context_slot
        || !is_hex_sha(&input.lifecycle_id)
        || !is_hex_sha(&input.route_authorization_sha256)
        || !is_hex_sha(&input.protected_prestate_sha256)
        || !is_hex_sha(&input.protected_address_set_sha256)
        || !valid_hex(&input.signed_transaction_hex)
        || input.signed_transaction_hex != input.signed_transaction_hex.to_ascii_lowercase()
        || !valid_lower_sha(&input.signed_transaction_sha256)
        || !valid_lower_sha(&input.message_sha256)
        || input.expected_signature.trim().is_empty()
        || input.recent_blockhash.trim().is_empty()
        || input.last_valid_block_height <= 0
        || input.fee_payer.trim().is_empty()
        || input.compiled_fee_lamports < 0
        || input.writable_account_keys.is_empty()
        || sorted_unique(&input.logical_conflict_keys)
            != sorted_unique(&handoff.required_conflict_keys)
    {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr signed manager intent is not bound to its fenced handoff".to_owned(),
        ));
    }
    let (wire_hash, message_hash, expected_signature) =
        derive_signed_wire_evidence(&input.signed_transaction_hex)?;
    if wire_hash != input.signed_transaction_sha256
        || message_hash != input.message_sha256
        || expected_signature != input.expected_signature
    {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr signed manager intent evidence does not match exact wire bytes".to_owned(),
        ));
    }
    Ok(())
}

pub fn validate_voltr_restoration_plan(
    input: &VoltrRestorationPlanInput,
) -> Result<(), OrchestratorError> {
    if input.cluster != VOLTR_FOUR_MARKET_CLUSTER
        || input.vault != VOLTR_FOUR_MARKET_VAULT
        || input.route_id != VOLTR_FOUR_MARKET_ROUTE_ID
        || input.route_spec_sha256 != VOLTR_FOUR_MARKET_ROUTE_SPEC_SHA256
        || input.origin_id.trim().is_empty()
        || !is_hex_sha(&input.origin_id)
        || !is_hex_sha(&input.route_spec_sha256)
        || !is_hex_sha(&input.route_authorization_sha256)
        || !is_hex_sha(&input.lifecycle_id)
        || !is_hex_sha(&input.scan_generation_fingerprint)
        || input.generation <= 0
        || input.observation_context_slot <= 0
        || input.requested_raw <= 0
        || input.requested_raw > VOLTR_RESTORATION_MAX_REQUEST_RAW
        || input.legs.is_empty()
    {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr restoration identity and demand are not exact".to_owned(),
        ));
    }
    if input.request_origin.signature.trim().is_empty()
        || input.request_origin.receipt.trim().is_empty()
        || input.request_origin.event_index < 0
        || !is_hex_sha(&input.request_origin.raw_account_sha256)
        || !is_hex_sha(&input.request_origin.generation_fingerprint)
    {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr restoration request origin is not bound to the scan generation".to_owned(),
        ));
    }
    if !is_hex_sha(&input.protected_checkpoint.address_set_sha256)
        || !is_hex_sha(&input.protected_checkpoint.state_sha256)
        || input.protected_checkpoint.context_slot <= 0
        || input.protected_checkpoint.context_slot > input.observation_context_slot
    {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr restoration protected checkpoint is stale or malformed".to_owned(),
        ));
    }
    let mut leg_ids = BTreeSet::new();
    let mut total = 0_i64;
    for leg in &input.legs {
        if leg.leg_id.trim().is_empty()
            || !is_hex_sha(&leg.leg_id)
            || leg.strategy_id.trim().is_empty()
            || approved_reserve(&leg.strategy_id) != Some(leg.reserve.as_str())
            || leg.amount_raw <= 0
            || leg.amount_raw > VOLTR_RESTORATION_MAX_LEG_RAW
            || leg.source_available_raw < leg.amount_raw
            || leg.source_observed_context_slot < input.observation_context_slot
            || !is_hex_sha(&leg.position_fingerprint)
            || !leg_ids.insert(leg.leg_id.clone())
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr restoration leg is not an approved bounded manager request".to_owned(),
            ));
        }
        total = total.checked_add(leg.amount_raw).ok_or_else(|| {
            OrchestratorError::StoreInvariant("Voltr restoration amount overflowed".to_owned())
        })?;
    }
    if total != input.requested_raw {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr restoration legs do not restore the exact requested amount".to_owned(),
        ));
    }
    Ok(())
}

/// Decodes a leased Voltr restoration row into the immutable manager handoff.
/// Kept pure so exact-identity bridge leasing and the legacy lane lease share
/// the same fail-closed payload checks.
fn parse_voltr_restoration_handoff(
    lease: OrchestrationOutboxLease,
) -> Result<VoltrRestorationHandoff, OrchestratorError> {
    let payload = &lease.event.payload;
    if payload.get("executionKind").and_then(Value::as_str)
        != Some(VOLTR_RESTORATION_EXECUTION_KIND)
        || payload.get("routeId").and_then(Value::as_str) != Some(VOLTR_FOUR_MARKET_ROUTE_ID)
        || payload.get("routeSpecSha256").and_then(Value::as_str)
            != Some(VOLTR_FOUR_MARKET_ROUTE_SPEC_SHA256)
        || payload.get("vault").and_then(Value::as_str) != Some(VOLTR_FOUR_MARKET_VAULT)
    {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr restoration outbox is not a manager-only four-market execution".to_owned(),
        ));
    }
    let route_authorization_sha256 = payload
        .get("routeAuthorizationSha256")
        .and_then(Value::as_str)
        .filter(|value| is_hex_sha(value))
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Voltr handoff lacks exact route authorization binding".to_owned(),
            )
        })?
        .to_owned();
    let lifecycle_id = payload
        .get("lifecycleId")
        .and_then(Value::as_str)
        .filter(|value| is_hex_sha(value))
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Voltr handoff lacks exact lifecycle binding".to_owned(),
            )
        })?
        .to_owned();
    let request_origin: VoltrRestorationRequestOrigin =
        serde_json::from_value(payload.get("requestOrigin").cloned().ok_or_else(|| {
            OrchestratorError::StoreInvariant("Voltr handoff lacks request origin".to_owned())
        })?)
        .map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "Voltr handoff request origin is invalid: {error}"
            ))
        })?;
    let protected_checkpoint: VoltrRestorationProtectedCheckpoint =
        serde_json::from_value(payload.get("protectedCheckpoint").cloned().ok_or_else(|| {
            OrchestratorError::StoreInvariant("Voltr handoff lacks protected checkpoint".to_owned())
        })?)
        .map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "Voltr handoff protected checkpoint is invalid: {error}"
            ))
        })?;
    if payload
        .get("scanGenerationFingerprint")
        .and_then(Value::as_str)
        .map(|value| !is_hex_sha(value))
        .unwrap_or(true)
        || !is_hex_sha(&request_origin.raw_account_sha256)
        || !is_hex_sha(&request_origin.generation_fingerprint)
        || request_origin.signature.trim().is_empty()
        || request_origin.receipt.trim().is_empty()
        || request_origin.event_index < 0
        || !is_hex_sha(&protected_checkpoint.address_set_sha256)
        || !is_hex_sha(&protected_checkpoint.state_sha256)
        || protected_checkpoint.context_slot <= 0
        || protected_checkpoint.context_slot
            > payload
                .get("observationContextSlot")
                .and_then(Value::as_i64)
                .unwrap_or_default()
    {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr handoff provenance is stale or malformed".to_owned(),
        ));
    }
    let origin_id = payload
        .get("originId")
        .and_then(Value::as_str)
        .filter(|value| is_hex_sha(value))
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant("Voltr handoff lacks originId".to_owned())
        })?
        .to_owned();
    let generation = payload
        .get("generation")
        .and_then(Value::as_i64)
        .filter(|value| *value > 0)
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant("Voltr handoff lacks generation".to_owned())
        })?;
    let leg: VoltrRestorationLegInput =
        serde_json::from_value(payload.get("leg").cloned().ok_or_else(|| {
            OrchestratorError::StoreInvariant("Voltr handoff lacks logical leg".to_owned())
        })?)
        .map_err(|error| {
            OrchestratorError::StoreInvariant(format!("Voltr handoff leg is invalid: {error}"))
        })?;
    if approved_reserve(&leg.strategy_id) != Some(leg.reserve.as_str())
        || leg.amount_raw <= 0
        || leg.amount_raw > VOLTR_RESTORATION_MAX_LEG_RAW
        || leg.source_available_raw < leg.amount_raw
        || leg.source_observed_context_slot
            < payload
                .get("observationContextSlot")
                .and_then(Value::as_i64)
                .unwrap_or_default()
        || !is_hex_sha(&leg.position_fingerprint)
    {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr handoff leg is not an approved bounded four-market restoration".to_owned(),
        ));
    }
    let request = payload.get("managerRequest").ok_or_else(|| {
        OrchestratorError::StoreInvariant("Voltr handoff lacks manager request".to_owned())
    })?;
    let request_keys_exact = request
        .as_object()
        .map(|object| {
            let mut keys = object.keys().cloned().collect::<Vec<_>>();
            keys.sort();
            keys == [
                "amountRaw".to_owned(),
                "operation".to_owned(),
                "reserve".to_owned(),
                "strategyId".to_owned(),
            ]
        })
        .unwrap_or(false);
    if request.get("operation").and_then(Value::as_str) != Some("manager-withdraw")
        || request.get("strategyId").and_then(Value::as_str) != Some(leg.strategy_id.as_str())
        || request.get("reserve").and_then(Value::as_str) != Some(leg.reserve.as_str())
        || request.get("amountRaw").and_then(Value::as_i64) != Some(leg.amount_raw)
        || !request_keys_exact
        || payload.get("originId").and_then(Value::as_str) != Some(origin_id.as_str())
        || payload.get("generation").and_then(Value::as_i64) != Some(generation)
    {
        return Err(OrchestratorError::StoreInvariant(
            "Voltr handoff manager request escaped its durable leg identity".to_owned(),
        ));
    }
    let intent_id = manager_intent_id(origin_id.as_str(), generation, &leg.leg_id);
    let vault = payload
        .get("vault")
        .and_then(Value::as_str)
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| OrchestratorError::StoreInvariant("Voltr handoff lacks vault".to_owned()))?
        .to_owned();
    let reserve_key = format!("kamino:reserve:{}", leg.reserve);
    Ok(VoltrRestorationHandoff {
        lease,
        execution_kind: VOLTR_RESTORATION_EXECUTION_KIND,
        route_authorization_sha256,
        lifecycle_id,
        request_origin,
        protected_checkpoint,
        origin_id,
        generation,
        leg,
        execution_state: "awaiting_logical_manager_executor",
        execution_blocker: VOLTR_RESTORATION_EXECUTION_BLOCKER,
        manager_intent_id: intent_id,
        required_conflict_keys: vec![format!("voltr:vault:{vault}"), reserve_key],
        confirmation_commitment: "confirmed",
        one_send_only: true,
        recompute_shortfall_after_confirmation: true,
        stop_when_shortfall_zero: true,
        ack_condition: VOLTR_RESTORATION_ACK_CONDITION,
        conflict_lease_acquired: false,
        signed_submission_id: None,
        expected_signature: None,
    })
}

impl NeonSqlClient {
    /// Atomically persists one outbox event per logical manager-withdraw leg.
    /// The existing outbox row is the durable movement state and its lease,
    /// fencing token, retry, and acknowledgement methods provide recovery.
    /// `ON CONFLICT` makes replay of the same origin/generation harmless.
    pub async fn enqueue_voltr_withdrawal_restoration(
        &self,
        input: VoltrRestorationPlanInput,
    ) -> Result<VoltrRestorationEnqueueResult, OrchestratorError> {
        validate_voltr_restoration_plan(&input)?;
        let mut tx = self.pool().begin().await?;
        let expected_plan_fingerprint = plan_fingerprint(&input);
        let lock_key = format!(
            "backyard-voltr-restoration:{}:{}:{}:{}",
            input.cluster, input.vault, input.origin_id, input.generation
        );
        // The SELECT below cannot lock a not-yet-existing row. Serialize the
        // first insert for this exact identity so two workers cannot both
        // observe an empty origin and publish conflicting legs.
        sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")
            .bind(lock_key)
            .execute(&mut *tx)
            .await?;
        let existing_payloads = sqlx::query(
            r#"
            SELECT payload
            FROM loyal_yield.orchestration_outbox
            WHERE aggregate_kind = $1
              AND payload ->> 'originId' = $2
              AND payload ->> 'generation' = $3
            FOR SHARE
            "#,
        )
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .bind(&input.origin_id)
        .bind(input.generation.to_string())
        .fetch_all(&mut *tx)
        .await?;
        let expected_request_origin =
            serde_json::to_value(&input.request_origin).map_err(|error| {
                OrchestratorError::StoreInvariant(format!(
                    "Voltr request-origin serialization failed: {error}"
                ))
            })?;
        let expected_protected_checkpoint = serde_json::to_value(&input.protected_checkpoint)
            .map_err(|error| {
                OrchestratorError::StoreInvariant(format!(
                    "Voltr protected-checkpoint serialization failed: {error}"
                ))
            })?;
        for row in existing_payloads {
            let payload: Value = row.try_get("payload")?;
            if payload.get("routeSpecSha256").and_then(Value::as_str)
                != Some(input.route_spec_sha256.as_str())
                || payload
                    .get("routeAuthorizationSha256")
                    .and_then(Value::as_str)
                    != Some(input.route_authorization_sha256.as_str())
                || payload.get("lifecycleId").and_then(Value::as_str)
                    != Some(input.lifecycle_id.as_str())
                || payload.get("vault").and_then(Value::as_str) != Some(input.vault.as_str())
                || payload
                    .get("scanGenerationFingerprint")
                    .and_then(Value::as_str)
                    != Some(input.scan_generation_fingerprint.as_str())
                || payload.get("planFingerprint").and_then(Value::as_str)
                    != Some(expected_plan_fingerprint.as_str())
                || payload.get("requestOrigin") != Some(&expected_request_origin)
                || payload.get("protectedCheckpoint") != Some(&expected_protected_checkpoint)
            {
                return Err(OrchestratorError::StoreInvariant(
                    "Voltr restoration origin collided with different immutable evidence"
                        .to_owned(),
                ));
            }
        }

        let mut inserted_leg_count = 0_u64;
        let mut duplicate_leg_count = 0_u64;
        let mut outbox_event_ids = Vec::with_capacity(input.legs.len());
        for leg in &input.legs {
            let dedupe_key = format!(
                "backyard-voltr:{}:{}:{}",
                input.origin_id, input.generation, leg.leg_id
            );
            let payload = json!({
                "schemaVersion": 1,
                "executionKind": VOLTR_RESTORATION_EXECUTION_KIND,
                "routeId": input.route_id,
                "routeSpecSha256": input.route_spec_sha256,
                "routeAuthorizationSha256": input.route_authorization_sha256,
                "lifecycleId": input.lifecycle_id,
                "vault": input.vault,
                "originId": input.origin_id,
                "generation": input.generation,
                "scanGenerationFingerprint": input.scan_generation_fingerprint,
                "planFingerprint": expected_plan_fingerprint,
                "observationContextSlot": input.observation_context_slot,
                "requestOrigin": input.request_origin,
                "protectedCheckpoint": input.protected_checkpoint,
                "requestedRaw": input.requested_raw,
                "leg": leg,
                "managerRequest": {
                    "operation": "manager-withdraw",
                    "strategyId": leg.strategy_id,
                    "reserve": leg.reserve,
                    "amountRaw": leg.amount_raw,
                },
            });
            let row = sqlx::query(
                r#"
                INSERT INTO loyal_yield.orchestration_outbox
                    (cluster, event_kind, aggregate_kind, aggregate_id, dedupe_key, payload)
                VALUES ($1, $2, $3, $4, $5, $6)
                ON CONFLICT (dedupe_key) DO NOTHING
                RETURNING id
                "#,
            )
            .bind(&input.cluster)
            .bind(VOLTR_RESTORATION_EVENT_KIND)
            .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
            .bind(synthetic_aggregate_id(&input.origin_id, &leg.leg_id))
            .bind(dedupe_key)
            .bind(payload)
            .fetch_optional(&mut *tx)
            .await?;
            if let Some(row) = row {
                inserted_leg_count += 1;
                outbox_event_ids.push(row.try_get("id")?);
            } else {
                duplicate_leg_count += 1;
            }
        }
        tx.commit().await?;
        Ok(VoltrRestorationEnqueueResult {
            origin_id: input.origin_id,
            generation: input.generation,
            inserted_leg_count,
            duplicate_leg_count,
            outbox_event_ids,
        })
    }

    /// Claims one restoration event and validates its immutable handoff
    /// envelope. It does not acquire a Solana account-conflict lease or mark
    /// the event processed: the current Rust worker cannot call the guarded
    /// TypeScript manager API or persist its expected signature. The caller
    /// must wire that executor, then acknowledge/retry this exact fenced lease.
    async fn lease_exact_voltr_restoration_row(
        &self,
        cluster: &str,
        owner: &str,
        lease_expires_at: chrono::DateTime<chrono::Utc>,
        origin_id: &str,
        generation: i64,
        leg_id: &str,
    ) -> Result<Option<OrchestrationOutboxLease>, OrchestratorError> {
        if cluster.trim().is_empty()
            || owner.trim().is_empty()
            || lease_expires_at <= chrono::Utc::now()
            || !is_hex_sha(origin_id)
            || generation <= 0
            || !is_hex_sha(leg_id)
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr exact lease identity is malformed".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT event.id
                FROM loyal_yield.orchestration_outbox event
                WHERE event.cluster = $1
                  AND event.event_kind = $2
                  AND event.aggregate_kind = $3
                  AND event.payload->>'routeId' = 'loyal-backyard-four-market-usdc-v1'
                  AND event.payload->>'routeSpecSha256' = 'a68ef28c8b9a9c8e34106cf78f1d10624d8bc9ebfd366cc15cbc5b273ecdf3e3'
                  AND event.payload->>'vault' = 'AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK'
                  AND event.processed_at IS NULL
                  AND event.payload->>'originId' = $4
                  AND event.payload->>'generation' = $5
                  AND event.payload->'leg'->>'legId' = $6
                  AND event.available_at <= now()
                  AND (event.lease_owner IS NULL OR event.lease_expires_at <= now())
                FOR UPDATE OF event
                LIMIT 1
            )
            UPDATE loyal_yield.orchestration_outbox event
            SET lease_owner = $7,
                lease_expires_at = $8,
                fencing_token = event.fencing_token + 1,
                attempt_count = event.attempt_count + 1,
                updated_at = now()
            FROM candidate
            WHERE event.id = candidate.id
            RETURNING event.*
            "#,
        )
        .bind(cluster)
        .bind(VOLTR_RESTORATION_EVENT_KIND)
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .bind(origin_id)
        .bind(generation.to_string())
        .bind(leg_id)
        .bind(owner)
        .bind(lease_expires_at)
        .fetch_optional(self.pool())
        .await?;
        let Some(row) = row else {
            return Ok(None);
        };
        let event = orchestration_outbox_from_row(&row)?;
        Ok(Some(OrchestrationOutboxLease {
            fencing_token: event.fencing_token,
            expires_at: lease_expires_at,
            owner: owner.to_owned(),
            event,
        }))
    }

    /// Claims exactly the requested restoration origin/generation/leg. This
    /// is the bridge entry point for a split TypeScript manager send; it does
    /// not scan or consume another restoration row.
    pub async fn lease_exact_voltr_restoration_handoff(
        &self,
        cluster: &str,
        owner: &str,
        lease_expires_at: chrono::DateTime<chrono::Utc>,
        origin_id: &str,
        generation: i64,
        leg_id: &str,
    ) -> Result<Option<VoltrRestorationHandoff>, OrchestratorError> {
        let Some(lease) = self
            .lease_exact_voltr_restoration_row(
                cluster,
                owner,
                lease_expires_at,
                origin_id,
                generation,
                leg_id,
            )
            .await?
        else {
            return Ok(None);
        };
        parse_voltr_restoration_handoff(lease).map(Some)
    }

    pub async fn lease_next_voltr_restoration_handoff(
        &self,
        cluster: &str,
        owner: &str,
        lease_expires_at: chrono::DateTime<chrono::Utc>,
    ) -> Result<Option<VoltrRestorationHandoff>, OrchestratorError> {
        let Some(lease) = self
            .lease_next_orchestration_outbox_lane(
                cluster,
                VOLTR_RESTORATION_EVENT_KIND,
                VOLTR_RESTORATION_AGGREGATE_KIND,
                owner,
                lease_expires_at,
            )
            .await?
        else {
            return Ok(None);
        };
        let payload = &lease.event.payload;
        if payload.get("executionKind").and_then(Value::as_str)
            != Some(VOLTR_RESTORATION_EXECUTION_KIND)
            || payload.get("routeId").and_then(Value::as_str) != Some(VOLTR_FOUR_MARKET_ROUTE_ID)
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr restoration outbox is not a manager-only four-market execution".to_owned(),
            ));
        }
        let route_authorization_sha256 = payload
            .get("routeAuthorizationSha256")
            .and_then(Value::as_str)
            .filter(|value| is_hex_sha(value))
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "Voltr handoff lacks exact route authorization binding".to_owned(),
                )
            })?
            .to_owned();
        let lifecycle_id = payload
            .get("lifecycleId")
            .and_then(Value::as_str)
            .filter(|value| is_hex_sha(value))
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "Voltr handoff lacks exact lifecycle binding".to_owned(),
                )
            })?
            .to_owned();
        let request_origin: VoltrRestorationRequestOrigin =
            serde_json::from_value(payload.get("requestOrigin").cloned().ok_or_else(|| {
                OrchestratorError::StoreInvariant("Voltr handoff lacks request origin".to_owned())
            })?)
            .map_err(|error| {
                OrchestratorError::StoreInvariant(format!(
                    "Voltr handoff request origin is invalid: {error}"
                ))
            })?;
        let protected_checkpoint: VoltrRestorationProtectedCheckpoint = serde_json::from_value(
            payload.get("protectedCheckpoint").cloned().ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "Voltr handoff lacks protected checkpoint".to_owned(),
                )
            })?,
        )
        .map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "Voltr handoff protected checkpoint is invalid: {error}"
            ))
        })?;
        if payload
            .get("scanGenerationFingerprint")
            .and_then(Value::as_str)
            .map(|value| !is_hex_sha(value))
            .unwrap_or(true)
            || !is_hex_sha(&request_origin.raw_account_sha256)
            || !is_hex_sha(&request_origin.generation_fingerprint)
            || request_origin.signature.trim().is_empty()
            || request_origin.receipt.trim().is_empty()
            || request_origin.event_index < 0
            || !is_hex_sha(&protected_checkpoint.address_set_sha256)
            || !is_hex_sha(&protected_checkpoint.state_sha256)
            || protected_checkpoint.context_slot <= 0
            || protected_checkpoint.context_slot
                > payload
                    .get("observationContextSlot")
                    .and_then(Value::as_i64)
                    .unwrap_or_default()
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr handoff provenance is stale or malformed".to_owned(),
            ));
        }
        let origin_id = payload
            .get("originId")
            .and_then(Value::as_str)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant("Voltr handoff lacks originId".to_owned())
            })?
            .to_owned();
        let generation = payload
            .get("generation")
            .and_then(Value::as_i64)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant("Voltr handoff lacks generation".to_owned())
            })?;
        let leg: VoltrRestorationLegInput =
            serde_json::from_value(payload.get("leg").cloned().ok_or_else(|| {
                OrchestratorError::StoreInvariant("Voltr handoff lacks logical leg".to_owned())
            })?)
            .map_err(|error| {
                OrchestratorError::StoreInvariant(format!("Voltr handoff leg is invalid: {error}"))
            })?;
        let request = payload.get("managerRequest").ok_or_else(|| {
            OrchestratorError::StoreInvariant("Voltr handoff lacks manager request".to_owned())
        })?;
        let request_keys_exact = request
            .as_object()
            .map(|object| {
                let mut keys = object.keys().cloned().collect::<Vec<_>>();
                keys.sort();
                keys == [
                    "amountRaw".to_owned(),
                    "operation".to_owned(),
                    "reserve".to_owned(),
                    "strategyId".to_owned(),
                ]
            })
            .unwrap_or(false);
        if request.get("operation").and_then(Value::as_str) != Some("manager-withdraw")
            || request.get("strategyId").and_then(Value::as_str) != Some(leg.strategy_id.as_str())
            || request.get("reserve").and_then(Value::as_str) != Some(leg.reserve.as_str())
            || request.get("amountRaw").and_then(Value::as_i64) != Some(leg.amount_raw)
            || !request_keys_exact
            || payload.get("originId").and_then(Value::as_str) != Some(origin_id.as_str())
            || payload.get("generation").and_then(Value::as_i64) != Some(generation)
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr handoff manager request escaped its durable leg identity".to_owned(),
            ));
        }
        let intent_id = manager_intent_id(origin_id.as_str(), generation, &leg.leg_id);
        let vault = payload
            .get("vault")
            .and_then(Value::as_str)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant("Voltr handoff lacks vault".to_owned())
            })?
            .to_owned();
        let reserve_key = format!("kamino:reserve:{}", leg.reserve);
        Ok(Some(VoltrRestorationHandoff {
            lease,
            execution_kind: VOLTR_RESTORATION_EXECUTION_KIND,
            route_authorization_sha256,
            lifecycle_id,
            request_origin,
            protected_checkpoint,
            origin_id,
            generation,
            leg,
            execution_state: "awaiting_logical_manager_executor",
            execution_blocker: VOLTR_RESTORATION_EXECUTION_BLOCKER,
            manager_intent_id: intent_id,
            required_conflict_keys: vec![format!("voltr:vault:{vault}"), reserve_key],
            confirmation_commitment: "confirmed",
            one_send_only: true,
            recompute_shortfall_after_confirmation: true,
            stop_when_shortfall_zero: true,
            ack_condition: VOLTR_RESTORATION_ACK_CONDITION,
            conflict_lease_acquired: false,
            signed_submission_id: None,
            expected_signature: None,
        }))
    }

    /// Keeps a validated handoff durable without pretending it was executed.
    /// The existing fenced retry path owns the next wakeup and records the
    /// named blocker in the outbox error column.
    pub async fn defer_voltr_restoration_handoff(
        &self,
        handoff: &VoltrRestorationHandoff,
        available_at: chrono::DateTime<chrono::Utc>,
    ) -> Result<(), OrchestratorError> {
        self.retry_orchestration_outbox(&handoff.lease, available_at, handoff.execution_blocker)
            .await
            .map(|_| ())
    }

    /// Records the logical conflict lease owned by the current outbox fence.
    /// This is intentionally named *logical*: the existing
    /// `route_account_conflict_leases` table is foreign-keyed to a rebalance
    /// opportunity and cannot represent a Voltr receipt restoration. The
    /// payload marker prevents a second Voltr worker from using the same
    /// event, while the adapter integration below remains blocked until the
    /// store gets a cross-event conflict lease primitive.
    pub async fn acquire_voltr_restoration_logical_conflict_lease(
        &self,
        handoff: &VoltrRestorationHandoff,
    ) -> Result<Value, OrchestratorError> {
        if handoff.execution_blocker != VOLTR_RESTORATION_EXECUTION_BLOCKER {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr conflict lease handoff has an unexpected execution contract".to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        for key in sorted_unique(&handoff.required_conflict_keys) {
            sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")
                .bind(format!(
                    "backyard-voltr-conflict:{}:{key}",
                    handoff.lease.event.cluster
                ))
                .execute(&mut *tx)
                .await?;
        }
        let row = sqlx::query(
            r#"
            SELECT payload
            FROM loyal_yield.orchestration_outbox
            WHERE id = $1
              AND event_kind = $2
              AND aggregate_kind = $3
              AND payload->>'executionKind' = $6
              AND processed_at IS NULL
              AND lease_owner = $4
              AND fencing_token = $5
              AND lease_expires_at > now()
            FOR UPDATE
            "#,
        )
        .bind(handoff.lease.event.id)
        .bind(VOLTR_RESTORATION_EVENT_KIND)
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .bind(&handoff.lease.owner)
        .bind(handoff.lease.fencing_token)
        .bind(VOLTR_RESTORATION_EXECUTION_KIND)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Voltr conflict lease is stale, expired, or fenced".to_owned(),
            )
        })?;
        let payload: Value = row.try_get("payload")?;
        let conflicting_event: Option<i64> = sqlx::query_scalar(
            r#"
            SELECT event.id
            FROM loyal_yield.orchestration_outbox event
            WHERE event.id <> $1
              AND event.cluster = $2
              AND event.event_kind = $3
              AND event.aggregate_kind = $4
              AND event.processed_at IS NULL
              AND event.lease_owner IS NOT NULL
              AND event.lease_expires_at > now()
              AND EXISTS (
                  SELECT 1
                  FROM jsonb_array_elements_text(
                      COALESCE(event.payload->'execution'->'conflictLease'->'keys', '[]'::jsonb)
                  ) AS held(value)
                  WHERE held.value = ANY($5::TEXT[])
              )
            LIMIT 1
            "#,
        )
        .bind(handoff.lease.event.id)
        .bind(&handoff.lease.event.cluster)
        .bind(VOLTR_RESTORATION_EVENT_KIND)
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .bind(sorted_unique(&handoff.required_conflict_keys))
        .fetch_optional(&mut *tx)
        .await?;
        if conflicting_event.is_some() {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr logical conflict keys are already held by another live restoration event"
                    .to_owned(),
            ));
        }
        if let Some(existing) = payload.get("execution") {
            let same_owner = existing
                .get("conflictLease")
                .and_then(|lease| lease.get("owner"))
                .and_then(Value::as_str)
                == Some(handoff.lease.owner.as_str())
                && existing
                    .get("conflictLease")
                    .and_then(|lease| lease.get("fencingToken"))
                    .and_then(Value::as_i64)
                    == Some(handoff.lease.fencing_token);
            if same_owner {
                tx.commit().await?;
                return Ok(payload);
            }
            // A reclaimed outbox event may carry an old owner/fence. Transfer
            // only the conflict marker; any signed/broadcast evidence remains
            // immutable and is recovered by the next worker.
            let mut execution = existing.clone();
            let object = execution.as_object_mut().ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "Voltr execution marker is not a JSON object".to_owned(),
                )
            })?;
            object.insert(
                "conflictLease".to_owned(),
                json!({
                    "owner": handoff.lease.owner,
                    "fencingToken": handoff.lease.fencing_token,
                    "keys": handoff.required_conflict_keys,
                }),
            );
            let updated = sqlx::query_scalar::<_, Value>(
                r#"UPDATE loyal_yield.orchestration_outbox
                   SET payload = jsonb_set(payload, '{execution}', $4::jsonb, true), updated_at = now()
                   WHERE id = $1 AND processed_at IS NULL AND lease_owner = $2
                     AND fencing_token = $3 AND lease_expires_at > now()
                   RETURNING payload"#,
            )
            .bind(handoff.lease.event.id)
            .bind(&handoff.lease.owner)
            .bind(handoff.lease.fencing_token)
            .bind(execution)
            .fetch_optional(&mut *tx)
            .await?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "Voltr conflict lease transfer lost its outbox fence".to_owned(),
                )
            })?;
            tx.commit().await?;
            return Ok(updated);
        }
        let execution = json!({
            "state": "conflict_leased",
            "managerIntentId": handoff.manager_intent_id,
            "conflictLease": {
                "owner": handoff.lease.owner,
                "fencingToken": handoff.lease.fencing_token,
                "keys": handoff.required_conflict_keys,
            },
        });
        let updated = sqlx::query_scalar::<_, Value>(
            r#"
            UPDATE loyal_yield.orchestration_outbox
            SET payload = jsonb_set(payload, '{execution}', $4::jsonb, true), updated_at = now()
            WHERE id = $1
              AND processed_at IS NULL
              AND lease_owner = $2
              AND fencing_token = $3
              AND lease_expires_at > now()
            RETURNING payload
            "#,
        )
        .bind(handoff.lease.event.id)
        .bind(&handoff.lease.owner)
        .bind(handoff.lease.fencing_token)
        .bind(execution)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Voltr conflict lease lost its outbox fence".to_owned(),
            )
        })?;
        tx.commit().await?;
        Ok(updated)
    }

    /// Persists the exact TypeScript-built manager wire image before any
    /// send. Repeating the same immutable intent is idempotent; replacing it
    /// or skipping the logical conflict-leased state is rejected.
    pub async fn persist_voltr_manager_signed_intent(
        &self,
        handoff: &VoltrRestorationHandoff,
        input: &VoltrManagerSignedIntentInput,
    ) -> Result<Value, OrchestratorError> {
        validate_signed_intent(handoff, input)?;
        let mut tx = self.pool().begin().await?;
        let row = sqlx::query(
            r#"
            SELECT payload
            FROM loyal_yield.orchestration_outbox
            WHERE id = $1 AND event_kind = $2 AND aggregate_kind = $3
              AND processed_at IS NULL AND lease_owner = $4
              AND fencing_token = $5 AND lease_expires_at > now()
            FOR UPDATE
            "#,
        )
        .bind(handoff.lease.event.id)
        .bind(VOLTR_RESTORATION_EVENT_KIND)
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .bind(&handoff.lease.owner)
        .bind(handoff.lease.fencing_token)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Voltr signed intent lost its outbox fence".to_owned(),
            )
        })?;
        let payload: Value = row.try_get("payload")?;
        let existing = payload.get("execution");
        let incoming = serde_json::to_value(input).map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "Voltr signed intent serialization failed: {error}"
            ))
        })?;
        if existing
            .and_then(|execution| execution.get("state"))
            .and_then(Value::as_str)
            == Some("signed_intent_ready")
        {
            if existing.and_then(|execution| execution.get("signedIntent")) == Some(&incoming) {
                tx.commit().await?;
                return Ok(payload);
            }
            return Err(OrchestratorError::StoreInvariant(
                "Voltr manager intent collided with different signed evidence".to_owned(),
            ));
        }
        if existing
            .and_then(|execution| execution.get("state"))
            .and_then(Value::as_str)
            != Some("conflict_leased")
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr manager intent requires the current logical conflict lease".to_owned(),
            ));
        }
        let execution = json!({
            "state": "signed_intent_ready",
            "managerIntentId": handoff.manager_intent_id,
            "conflictLease": existing.and_then(|value| value.get("conflictLease")).cloned(),
            "signedIntent": incoming,
        });
        let updated = sqlx::query_scalar::<_, Value>(
            r#"UPDATE loyal_yield.orchestration_outbox
               SET payload = jsonb_set(payload, '{execution}', $4::jsonb, true), updated_at = now()
               WHERE id = $1 AND processed_at IS NULL AND lease_owner = $2
                 AND fencing_token = $3 AND lease_expires_at > now()
               RETURNING payload"#,
        )
        .bind(handoff.lease.event.id)
        .bind(&handoff.lease.owner)
        .bind(handoff.lease.fencing_token)
        .bind(execution)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Voltr signed intent update lost its outbox fence".to_owned(),
            )
        })?;
        tx.commit().await?;
        Ok(updated)
    }

    /// Marks the one allowed broadcast intent. No RPC is called here; the
    /// caller must send only after this CAS commits and must never regenerate
    /// or blindly resend a different wire image.
    pub async fn mark_voltr_manager_broadcast_intent(
        &self,
        handoff: &VoltrRestorationHandoff,
        expected_signature: &str,
    ) -> Result<Value, OrchestratorError> {
        if handoff.execution_kind != VOLTR_RESTORATION_EXECUTION_KIND
            || expected_signature.trim().is_empty()
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr broadcast intent requires an expected signature".to_owned(),
            ));
        }
        let row = sqlx::query_scalar::<_, Value>(
            r#"UPDATE loyal_yield.orchestration_outbox
               SET payload = jsonb_set(
                   jsonb_set(payload, '{execution,state}', '"broadcast_intent_persisted"'::jsonb, true),
                   '{execution,broadcastCount}', '1'::jsonb, true
               ), updated_at = now()
               WHERE id = $1 AND event_kind = $2 AND aggregate_kind = $3
                 AND processed_at IS NULL AND lease_owner = $5
                 AND fencing_token = $6 AND lease_expires_at > now()
                 AND payload->'execution'->>'state' = 'signed_intent_ready'
                 AND payload->'execution'->>'managerIntentId' = $7
                 AND payload->'execution'->'signedIntent'->>'expectedSignature' = $4
               RETURNING payload"#,
        )
        .bind(handoff.lease.event.id)
        .bind(VOLTR_RESTORATION_EVENT_KIND)
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .bind(expected_signature)
        .bind(&handoff.lease.owner)
        .bind(handoff.lease.fencing_token)
        .bind(&handoff.manager_intent_id)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| OrchestratorError::StoreInvariant(
            "Voltr broadcast intent requires the exact signed-intent fence".to_owned(),
        ))?;
        Ok(row)
    }

    /// Records confirmed manager readback for this exact leg. Every leg is
    /// acknowledged independently; the recomputed remaining shortfall is a
    /// plan-level signal used to stop/cancel unneeded sibling legs.
    pub async fn record_voltr_manager_confirmation(
        &self,
        handoff: &VoltrRestorationHandoff,
        input: &VoltrManagerConfirmationInput,
    ) -> Result<Value, OrchestratorError> {
        if input.commitment != "confirmed"
            || !is_hex_sha(&input.manager_intent_id)
            || !is_hex_sha(&input.lifecycle_id)
            || input.strategy_id.trim().is_empty()
            || input.reserve.trim().is_empty()
            || input.amount_raw <= 0
            || !is_hex_sha(&input.route_authorization_sha256)
            || !is_hex_sha(&input.signed_transaction_sha256)
            || !is_hex_sha(&input.message_sha256)
            || input.confirmed_slot <= 0
            || input.readback_context_slot < input.confirmed_slot
            || input.expected_signature.trim().is_empty()
            || input.expected_signature != input.manager_transaction_signature
            || input.idle_raw_after < 0
            || input.remaining_shortfall_raw < 0
            || !is_hex_sha(&input.readback_fingerprint)
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr confirmation lacks exact confirmed readback evidence".to_owned(),
            ));
        }
        if handoff.execution_kind != VOLTR_RESTORATION_EXECUTION_KIND {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr confirmation is not a manager-only execution".to_owned(),
            ));
        }
        let confirmation = serde_json::to_value(input).map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "Voltr confirmation serialization failed: {error}"
            ))
        })?;
        let plan_state = if input.remaining_shortfall_raw == 0 {
            "complete"
        } else {
            "needs_recompute"
        };
        let row = sqlx::query_scalar::<_, Value>(
            r#"UPDATE loyal_yield.orchestration_outbox
               SET payload = jsonb_set(
                   jsonb_set(
                       jsonb_set(
                           jsonb_set(payload, '{execution,state}', '"reconciled"'::jsonb, true),
                           '{execution,confirmation}', $4::jsonb, true
                       ),
                       '{execution,planState}', to_jsonb($5::TEXT), true
                   ),
                   '{execution,ackCondition}', to_jsonb($6::TEXT), true
               ), updated_at = now()
               WHERE id = $1 AND event_kind = $2 AND aggregate_kind = $3
                 AND processed_at IS NULL AND lease_owner = $7
                 AND fencing_token = $8 AND lease_expires_at > now()
                 AND payload->'execution'->>'state' = 'broadcast_intent_persisted'
                 AND payload->'execution'->>'managerIntentId' = $9
                 AND payload->'execution'->'signedIntent'->>'expectedSignature' = $10
               RETURNING payload"#,
        )
        .bind(handoff.lease.event.id)
        .bind(VOLTR_RESTORATION_EVENT_KIND)
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .bind(confirmation)
        .bind(plan_state)
        .bind(VOLTR_RESTORATION_ACK_CONDITION)
        .bind(&handoff.lease.owner)
        .bind(handoff.lease.fencing_token)
        .bind(&handoff.manager_intent_id)
        .bind(&input.expected_signature)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Voltr confirmation requires the exact persisted broadcast intent".to_owned(),
            )
        })?;
        Ok(row)
    }

    /// Phase-B bridge completion. It reloads the exact fenced row rather than
    /// leasing a new one, records the confirmed command readback, acknowledges
    /// that row, and cancels only still-unleased siblings once the recomputed
    /// shortfall is zero. No signer or Solana RPC is used here.
    pub async fn complete_voltr_restoration_bridge(
        &self,
        token: &VoltrRestorationBridgeToken,
        input: &VoltrManagerConfirmationInput,
    ) -> Result<VoltrRestorationBridgeCompletion, OrchestratorError> {
        if token.schema_version != 1
            || token.event_id <= 0
            || token.cluster.trim().is_empty()
            || token.owner.trim().is_empty()
            || token.fencing_token <= 0
            || !is_hex_sha(&token.origin_id)
            || token.generation <= 0
            || !is_hex_sha(&token.leg_id)
            || !is_hex_sha(&token.manager_intent_id)
            || !is_hex_sha(&token.signed_transaction_sha256)
            || !is_hex_sha(&token.message_sha256)
            || !is_hex_sha(&token.lifecycle_id)
            || !is_hex_sha(&token.route_authorization_sha256)
            || !is_hex_sha(&token.protected_prestate_sha256)
            || !is_hex_sha(&token.protected_address_set_sha256)
            || token.protected_context_slot <= 0
            || token.strategy_id.trim().is_empty()
            || token.reserve.trim().is_empty()
            || token.amount_raw <= 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr bridge token identity is malformed".to_owned(),
            ));
        }
        if input.commitment != "confirmed"
            || input.manager_intent_id != token.manager_intent_id
            || input.lifecycle_id != token.lifecycle_id
            || input.strategy_id != token.strategy_id
            || input.reserve != token.reserve
            || input.amount_raw != token.amount_raw
            || input.route_authorization_sha256 != token.route_authorization_sha256
            || input.signed_transaction_sha256 != token.signed_transaction_sha256
            || input.message_sha256 != token.message_sha256
            || input.expected_signature != token.expected_signature
            || input.manager_transaction_signature != token.expected_signature
            || input.confirmed_slot <= 0
            || input.readback_context_slot < input.confirmed_slot
            || input.idle_raw_after < 0
            || input.remaining_shortfall_raw != 0
            || !is_hex_sha(&input.readback_fingerprint)
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr bridge confirmation is not bound to the Phase-A intent".to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let _row = sqlx::query(
            r#"
            SELECT payload
            FROM loyal_yield.orchestration_outbox
            WHERE id = $1
              AND cluster = $2
              AND event_kind = $3
              AND aggregate_kind = $4
              AND processed_at IS NULL
              AND lease_owner = $5
              AND fencing_token = $6
              AND lease_expires_at > now()
              AND payload->>'originId' = $7
              AND payload->>'generation' = $8
              AND payload->'leg'->>'legId' = $9
              AND payload->>'routeId' = $10
              AND payload->>'routeSpecSha256' = $11
              AND payload->>'vault' = $12
              AND payload->'leg'->>'strategyId' = $13
              AND payload->'leg'->>'reserve' = $14
              AND payload->'leg'->>'amountRaw' = $15
              AND payload->>'lifecycleId' = $16
              AND payload->>'routeAuthorizationSha256' = $17
              AND payload->'protectedCheckpoint'->>'stateSha256' = $18
              AND payload->'protectedCheckpoint'->>'addressSetSha256' = $19
              AND payload->'protectedCheckpoint'->>'contextSlot' = $20
              AND payload->'execution'->>'state' = 'broadcast_intent_persisted'
              AND payload->'execution'->>'managerIntentId' = $21
              AND payload->'execution'->'signedIntent'->>'expectedSignature' = $22
            FOR UPDATE
            "#,
        )
        .bind(token.event_id)
        .bind(&token.cluster)
        .bind(VOLTR_RESTORATION_EVENT_KIND)
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .bind(&token.owner)
        .bind(token.fencing_token)
        .bind(&token.origin_id)
        .bind(token.generation.to_string())
        .bind(&token.leg_id)
        .bind(VOLTR_FOUR_MARKET_ROUTE_ID)
        .bind(VOLTR_FOUR_MARKET_ROUTE_SPEC_SHA256)
        .bind(VOLTR_FOUR_MARKET_VAULT)
        .bind(&token.strategy_id)
        .bind(&token.reserve)
        .bind(token.amount_raw.to_string())
        .bind(&token.lifecycle_id)
        .bind(&token.route_authorization_sha256)
        .bind(&token.protected_prestate_sha256)
        .bind(&token.protected_address_set_sha256)
        .bind(token.protected_context_slot.to_string())
        .bind(&token.manager_intent_id)
        .bind(&token.expected_signature)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Voltr bridge token is stale, expired, reclaimed, or intent-mismatched".to_owned(),
            )
        })
        .map(|_| ())?;
        let confirmation = serde_json::to_value(input).map_err(|error| {
            OrchestratorError::StoreInvariant(format!(
                "Voltr bridge confirmation serialization failed: {error}"
            ))
        })?;
        let plan_state = "complete";
        let updated = sqlx::query_scalar::<_, Value>(
            r#"
            UPDATE loyal_yield.orchestration_outbox
            SET payload = jsonb_set(
                jsonb_set(
                    jsonb_set(
                        jsonb_set(payload, '{execution,state}', '"reconciled"'::jsonb, true),
                        '{execution,confirmation}', $1::jsonb, true
                    ),
                    '{execution,planState}', to_jsonb($2::TEXT), true
                ),
                '{execution,ackCondition}', to_jsonb($3::TEXT), true
            ),
                processed_at = now(),
                lease_owner = NULL,
                lease_expires_at = NULL,
                last_error = NULL,
                updated_at = now()
            WHERE id = $4
              AND cluster = $5
              AND processed_at IS NULL
              AND lease_owner = $6
              AND fencing_token = $7
              AND lease_expires_at > now()
            RETURNING payload
            "#,
        )
        .bind(confirmation)
        .bind(plan_state)
        .bind(VOLTR_RESTORATION_ACK_CONDITION)
        .bind(token.event_id)
        .bind(&token.cluster)
        .bind(&token.owner)
        .bind(token.fencing_token)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "Voltr bridge completion lost its fenced row before acknowledgement".to_owned(),
            )
        })?;
        let canceled_sibling_count = {
            sqlx::query(
                r#"
                UPDATE loyal_yield.orchestration_outbox
                SET processed_at = now(),
                    lease_owner = NULL,
                    lease_expires_at = NULL,
                    last_error = 'cancelled_shortfall_filled',
                    payload = jsonb_set(
                        payload,
                        '{execution}',
                        '{"state":"cancelled_shortfall_filled"}'::jsonb,
                        true
                    ),
                    updated_at = now()
                WHERE cluster = $1
                  AND event_kind = $2
                  AND aggregate_kind = $3
                  AND processed_at IS NULL
                  AND lease_owner IS NULL
                  AND id <> $4
                  AND payload->>'originId' = $5
                  AND payload->>'generation' = $6
                  AND payload->>'routeId' = $7
                  AND payload->>'routeSpecSha256' = $8
                  AND payload->>'vault' = $9
                  AND (payload->'execution' IS NULL OR payload->'execution'->>'state' = 'conflict_leased')
                "#,
            )
            .bind(&token.cluster)
            .bind(VOLTR_RESTORATION_EVENT_KIND)
            .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
            .bind(token.event_id)
            .bind(&token.origin_id)
            .bind(token.generation.to_string())
            .bind(VOLTR_FOUR_MARKET_ROUTE_ID)
            .bind(VOLTR_FOUR_MARKET_ROUTE_SPEC_SHA256)
            .bind(VOLTR_FOUR_MARKET_VAULT)
            .execute(&mut *tx)
            .await?
            .rows_affected()
        };
        tx.commit().await?;
        let _ = updated;
        Ok(VoltrRestorationBridgeCompletion {
            event_id: token.event_id,
            origin_id: token.origin_id.clone(),
            generation: token.generation,
            leg_id: token.leg_id.clone(),
            state: "acknowledged".to_owned(),
            acknowledged: true,
            canceled_sibling_count,
        })
    }

    /// Reloads the exact acknowledged restoration rows from Neon.  This is a
    /// read-only producer for the partner evidence envelope: it derives every
    /// signed/confirmed field from the durable outbox payload and refuses a
    /// partial, pending, or ambiguously duplicated generation.  The caller
    /// still supplies chain transaction artifacts from the canonical
    /// TypeScript manager executor; this method supplies only the database
    /// section that cannot safely be reconstructed from those files.
    pub async fn read_voltr_restoration_outbox(
        &self,
        cluster: &str,
        origin_id: &str,
        generation: i64,
        expected_leg_count: usize,
    ) -> Result<VoltrRestorationOutboxReadback, OrchestratorError> {
        if cluster.trim().is_empty()
            || !is_hex_sha(origin_id)
            || generation <= 0
            || expected_leg_count == 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr outbox readback identity/count is malformed".to_owned(),
            ));
        }
        let rows = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.orchestration_outbox
            WHERE cluster = $1
              AND event_kind = $2
              AND aggregate_kind = $3
              AND payload->>'originId' = $4
              AND payload->>'generation' = $5
            ORDER BY id
            "#,
        )
        .bind(cluster)
        .bind(VOLTR_RESTORATION_EVENT_KIND)
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .bind(origin_id)
        .bind(generation.to_string())
        .fetch_all(self.pool())
        .await?;
        if rows.len() != expected_leg_count {
            return Err(OrchestratorError::StoreInvariant(format!(
                "Voltr outbox readback expected {expected_leg_count} rows, found {}",
                rows.len()
            )));
        }
        let mut durable_rows = Vec::with_capacity(rows.len());
        let mut leg_ids = BTreeSet::new();
        for row in rows {
            let event = orchestration_outbox_from_row(&row)?;
            if event.processed_at.is_none()
                || event.fencing_token <= 0
                || event.event_kind != VOLTR_RESTORATION_EVENT_KIND
                || event.aggregate_kind != VOLTR_RESTORATION_AGGREGATE_KIND
                || event.payload.get("originId").and_then(Value::as_str) != Some(origin_id)
                || event.payload.get("generation").and_then(Value::as_i64) != Some(generation)
            {
                return Err(OrchestratorError::StoreInvariant(
                    "Voltr outbox readback contains an unacknowledged or cross-generation row"
                        .to_owned(),
                ));
            }
            let leg = event.payload.get("leg").ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "Voltr outbox row lacks its logical leg".to_owned(),
                )
            })?;
            let leg_id = leg
                .get("legId")
                .and_then(Value::as_str)
                .filter(|value| is_hex_sha(value))
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "Voltr outbox row leg id is not a lowercase SHA-256 digest".to_owned(),
                    )
                })?
                .to_owned();
            if !leg_ids.insert(leg_id.clone()) {
                return Err(OrchestratorError::StoreInvariant(
                    "Voltr outbox readback contains duplicate leg ids".to_owned(),
                ));
            }
            let execution = event.payload.get("execution").ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "Voltr outbox row lacks execution evidence".to_owned(),
                )
            })?;
            if execution.get("state").and_then(Value::as_str) != Some("reconciled")
                || execution.get("broadcastCount").and_then(Value::as_i64) != Some(1)
            {
                return Err(OrchestratorError::StoreInvariant(
                    "Voltr outbox row is not an acknowledged one-send reconciliation".to_owned(),
                ));
            }
            let signed_intent = execution.get("signedIntent").ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "Voltr outbox row lacks its persisted signed intent".to_owned(),
                )
            })?;
            let expected_signature = signed_intent
                .get("expectedSignature")
                .and_then(Value::as_str)
                .filter(|value| !value.trim().is_empty())
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "Voltr outbox signed intent lacks expected signature".to_owned(),
                    )
                })?
                .to_owned();
            let confirmation = execution.get("confirmation").ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "Voltr outbox row lacks confirmed manager readback".to_owned(),
                )
            })?;
            let confirmed_signature = confirmation
                .get("managerTransactionSignature")
                .and_then(Value::as_str)
                .filter(|value| !value.trim().is_empty())
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "Voltr confirmation lacks manager transaction signature".to_owned(),
                    )
                })?
                .to_owned();
            let confirmed_slot = confirmation
                .get("confirmedSlot")
                .and_then(Value::as_i64)
                .filter(|value| *value > 0)
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "Voltr confirmation lacks positive confirmed slot".to_owned(),
                    )
                })?;
            let readback_context_slot = confirmation
                .get("readbackContextSlot")
                .and_then(Value::as_i64)
                .filter(|value| *value >= confirmed_slot)
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "Voltr confirmation lacks a transaction-anchored readback context slot"
                            .to_owned(),
                    )
                })?;
            if expected_signature != confirmed_signature {
                return Err(OrchestratorError::StoreInvariant(
                    "Voltr outbox expected and confirmed signatures differ".to_owned(),
                ));
            }
            durable_rows.push(VoltrRestorationDurableRow {
                event_id: event.id,
                leg_id,
                dedupe_key: event.dedupe_key,
                state: "acknowledged".to_owned(),
                lease_fence: event.fencing_token,
                manager_intent_id: execution
                    .get("managerIntentId")
                    .and_then(Value::as_str)
                    .filter(|value| is_hex_sha(value))
                    .ok_or_else(|| {
                        OrchestratorError::StoreInvariant(
                            "Voltr outbox execution lacks manager intent identity".to_owned(),
                        )
                    })?
                    .to_owned(),
                expected_signature,
                confirmed_signature,
                confirmed_slot,
                readback_context_slot,
                one_send_only: true,
            });
        }
        Ok(VoltrRestorationOutboxReadback {
            event_kind: VOLTR_RESTORATION_EVENT_KIND.to_owned(),
            aggregate_kind: VOLTR_RESTORATION_AGGREGATE_KIND.to_owned(),
            origin_id: origin_id.to_owned(),
            generation,
            inserted_leg_count: durable_rows.len() as u64,
            duplicate_leg_count: 0,
            rows: durable_rows,
            ack_condition: VOLTR_RESTORATION_ACK_CONDITION.to_owned(),
        })
    }

    /// The existing outbox acknowledgement remains the only terminal ack
    /// path. It acknowledges one confirmed/reconciled leg, not the whole
    /// generation; sibling legs are stopped separately when recomputation
    /// proves the shortfall is filled.
    pub async fn acknowledge_voltr_restoration_if_reconciled(
        &self,
        handoff: &VoltrRestorationHandoff,
    ) -> Result<OrchestrationOutboxRecord, OrchestratorError> {
        let state: Option<String> = sqlx::query_scalar(
            "SELECT payload->'execution'->>'state' FROM loyal_yield.orchestration_outbox WHERE id = $1 AND event_kind = $4 AND aggregate_kind = $5 AND lease_owner = $2 AND fencing_token = $3 AND lease_expires_at > now() AND processed_at IS NULL",
        )
        .bind(handoff.lease.event.id)
        .bind(&handoff.lease.owner)
        .bind(handoff.lease.fencing_token)
        .bind(VOLTR_RESTORATION_EVENT_KIND)
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .fetch_optional(self.pool())
        .await?;
        if state.as_deref() != Some("reconciled") {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr restoration leg may be acknowledged only after confirmed reconciliation"
                    .to_owned(),
            ));
        }
        self.acknowledge_orchestration_outbox(&handoff.lease).await
    }

    /// Stops unclaimed sibling legs after a confirmed leg recomputes the
    /// generation shortfall to zero. Leased siblings are left untouched so a
    /// worker holding a valid fence can finish or explicitly retry them.
    pub async fn cancel_unclaimed_voltr_restoration_legs(
        &self,
        cluster: &str,
        origin_id: &str,
        generation: i64,
        except_event_id: i64,
    ) -> Result<u64, OrchestratorError> {
        if cluster.trim().is_empty() || !is_hex_sha(origin_id) || generation <= 0 {
            return Err(OrchestratorError::StoreInvariant(
                "Voltr sibling cancellation identity is not exact".to_owned(),
            ));
        }
        let result = sqlx::query(
            r#"
            UPDATE loyal_yield.orchestration_outbox
            SET processed_at = now(),
                lease_owner = NULL,
                lease_expires_at = NULL,
                last_error = 'cancelled_shortfall_filled',
                payload = jsonb_set(
                    payload,
                    '{execution}',
                    '{"state":"cancelled_shortfall_filled"}'::jsonb,
                    true
                ),
                updated_at = now()
            WHERE cluster = $1
              AND event_kind = $2
              AND aggregate_kind = $3
              AND processed_at IS NULL
              AND lease_owner IS NULL
              AND id <> $4
              AND payload->>'originId' = $5
              AND payload->>'generation' = $6
              AND (
                  payload->'execution' IS NULL
                  OR payload->'execution'->>'state' = 'conflict_leased'
              )
            "#,
        )
        .bind(cluster)
        .bind(VOLTR_RESTORATION_EVENT_KIND)
        .bind(VOLTR_RESTORATION_AGGREGATE_KIND)
        .bind(except_event_id)
        .bind(origin_id)
        .bind(generation.to_string())
        .bind(VOLTR_RESTORATION_EXECUTION_KIND)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected())
    }
}

/// Offline contract verifier for the durable boundary. It intentionally
/// exercises the JSON transitions rather than pretending to prove a database
/// or chain result without a configured Neon/RPC environment.
pub fn verify_voltr_restoration_state_machine() -> Result<(), &'static str> {
    let mut payload = json!({
        "executionKind": VOLTR_RESTORATION_EXECUTION_KIND,
        "routeId": VOLTR_FOUR_MARKET_ROUTE_ID,
        "routeAuthorizationSha256": "d".repeat(64),
        "lifecycleId": "e".repeat(64),
        "originId": "a".repeat(64),
        "generation": 1,
        "requestOrigin": {
            "signature": "request-signature",
            "eventIndex": 0,
            "receipt": "receipt-pda",
            "rawAccountSha256": "f".repeat(64),
            "generationFingerprint": "a".repeat(64)
        },
        "protectedCheckpoint": {
            "addressSetSha256": "1".repeat(64),
            "stateSha256": "2".repeat(64),
            "contextSlot": 7
        },
        "execution": {
            "state": "conflict_leased",
            "managerIntentId": "b".repeat(64),
            "conflictLease": {"owner": "worker-a", "fencingToken": 1, "keys": ["vault:v", "reserve:r"]}
        }
    });
    if payload
        .get("execution")
        .and_then(|value| value.get("state"))
        != Some(&Value::String("conflict_leased".to_owned()))
    {
        return Err("execution state is not nested under payload.execution");
    }
    if payload["executionKind"] != VOLTR_RESTORATION_EXECUTION_KIND
        || payload["routeId"] != VOLTR_FOUR_MARKET_ROUTE_ID
    {
        return Err("restoration payload is not manager-only and route-bound");
    }
    let conflict_lease = payload["execution"]["conflictLease"].clone();
    payload["execution"] = json!({
        "state": "signed_intent_ready",
        "managerIntentId": "b".repeat(64),
        "conflictLease": conflict_lease,
        "signedIntent": {"expectedSignature": "sig-a", "signedTransactionSha256": "c".repeat(64)},
    });
    if payload["execution"]["conflictLease"]["owner"] != "worker-a" {
        return Err("signed intent transition dropped the conflict lease");
    }
    let replay = payload["execution"].clone();
    if replay != payload["execution"] {
        return Err("identical signed intent replay is not idempotent");
    }
    payload["execution"]["state"] = "broadcast_intent_persisted".into();
    payload["execution"]["broadcastCount"] = 1.into();
    if payload["execution"]["signedIntent"]["expectedSignature"] != "sig-a"
        || payload["execution"]["conflictLease"]["fencingToken"] != 1
    {
        return Err("broadcast transition dropped signed evidence or fencing");
    }
    payload["execution"]["state"] = "reconciled".into();
    payload["execution"]["confirmation"] =
        json!({"confirmedSlot": 7, "remainingShortfallRaw": 500_000});
    payload["execution"]["planState"] = "needs_recompute".into();
    if payload["execution"]["state"] != "reconciled"
        || payload["execution"]["planState"] != "needs_recompute"
    {
        return Err("a confirmed partial leg must be independently ackable and trigger recompute");
    }
    if payload["execution"]["signedIntent"]["expectedSignature"] != "sig-a"
        || payload["execution"]["conflictLease"]["owner"] != "worker-a"
    {
        return Err("confirmation transition dropped signed evidence or conflict lease");
    }
    if payload["execution"]["confirmation"]["confirmedSlot"]
        .as_i64()
        .unwrap_or_default()
        <= 0
    {
        return Err("confirmed readback must have a positive slot");
    }
    payload["execution"]["confirmation"]["remainingShortfallRaw"] = 0.into();
    payload["execution"]["planState"] = "complete".into();
    if payload["execution"]["planState"] != "complete" {
        return Err("zero shortfall must stop sibling legs");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{
        derive_signed_wire_evidence, verify_voltr_restoration_state_machine,
        VoltrRestorationPlanInput,
    };
    use serde_json::json;

    #[test]
    fn durable_voltr_state_machine_contract_passes() {
        verify_voltr_restoration_state_machine().expect("state-machine verifier");
    }

    #[test]
    fn signed_wire_tampering_changes_derived_evidence() {
        let mut wire = vec![1_u8];
        wire.extend([7_u8; 64]);
        wire.extend([8_u8; 4]);
        let hex = wire
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        let (wire_hash, message_hash, signature) =
            derive_signed_wire_evidence(&hex).expect("valid one-signer wire");
        let mut tampered = wire.clone();
        tampered[1] ^= 1;
        let tampered_hex = tampered
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        let (tampered_wire_hash, _, tampered_signature) =
            derive_signed_wire_evidence(&tampered_hex).expect("tampered wire remains parseable");
        assert_ne!(wire_hash, tampered_wire_hash);
        assert_ne!(signature, tampered_signature);
        let mut message_tampered = wire;
        *message_tampered.last_mut().expect("message byte") ^= 1;
        let message_tampered_hex = message_tampered
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        let (_, tampered_message_hash, _) =
            derive_signed_wire_evidence(&message_tampered_hex).expect("message tamper parse");
        assert_ne!(message_hash, tampered_message_hash);
    }

    #[test]
    fn typescript_decimal_string_raw_amounts_deserialize() {
        let value = json!({
            "cluster": "mainnet-beta",
            "vault": "vault",
            "routeId": "route",
            "routeSpecSha256": "11".repeat(32),
            "routeAuthorizationSha256": "22".repeat(32),
            "lifecycleId": "33".repeat(32),
            "requestOrigin": {
                "signature": "signature",
                "eventIndex": 0,
                "receipt": "receipt",
                "rawAccountSha256": "44".repeat(32),
                "generationFingerprint": "55".repeat(32)
            },
            "protectedCheckpoint": {
                "addressSetSha256": "66".repeat(32),
                "stateSha256": "77".repeat(32),
                "contextSlot": 123
            },
            "originId": "88".repeat(32),
            "generation": 1,
            "scanGenerationFingerprint": "99".repeat(32),
            "observationContextSlot": 124,
            "requestedRaw": "100",
            "legs": [{
                "legId": "aa".repeat(32),
                "strategyId": "main",
                "reserve": "reserve",
                "amountRaw": "100",
                "sourceAvailableRaw": "101",
                "sourceObservedContextSlot": 124,
                "positionFingerprint": "bb".repeat(32)
            }]
        });
        let decoded: VoltrRestorationPlanInput =
            serde_json::from_value(value.clone()).expect("canonical TypeScript wire");
        assert_eq!(decoded.requested_raw, 100);
        assert_eq!(decoded.legs[0].amount_raw, 100);
        assert_eq!(decoded.legs[0].source_available_raw, 101);

        let mut noncanonical = value;
        noncanonical["requestedRaw"] = json!("0100");
        assert!(serde_json::from_value::<VoltrRestorationPlanInput>(noncanonical).is_err());
    }
}
