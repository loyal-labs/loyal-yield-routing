//! Rust-only durable bridge for a split Backyard Voltr restoration send.
//!
//! Phase A leases one exact outbox origin/generation/leg, acquires the
//! existing logical conflict fence, persists the canonical manager wire, and
//! records the one allowed broadcast intent. It never sends Solana.
//!
//! Phase B consumes the non-secret Phase-A token plus the exact confirmed
//! manager readback, reloads the same fence, acknowledges that row, and only
//! cancels still-unleased siblings when the shortfall is zero. The actual
//! manager transaction remains an explicit TypeScript handoff between phases.

use chrono::{Duration, Utc};
use loyal_yield_orchestrator::{NeonSqlClient, NeonSqlConfig};
use loyal_yield_store::fleet_orchestration::{
    VoltrManagerConfirmationInput, VoltrManagerSignedIntentInput, VoltrRestorationBridgeCompletion,
    VoltrRestorationBridgeToken, VOLTR_FOUR_MARKET_CLUSTER, VOLTR_FOUR_MARKET_ROUTE_ID,
    VOLTR_FOUR_MARKET_ROUTE_SPEC_SHA256, VOLTR_FOUR_MARKET_VAULT,
};
use serde::Deserialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::{env, fs, path::PathBuf};

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct PhaseAInput {
    schema_version: u8,
    phase: String,
    cluster: String,
    route_id: String,
    route_spec_sha256: String,
    vault: String,
    owner: String,
    lease_seconds: i64,
    origin_id: String,
    generation: i64,
    leg_id: String,
    signed_intent: VoltrManagerSignedIntentInput,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct PhaseBInput {
    schema_version: u8,
    phase: String,
    token: VoltrRestorationBridgeToken,
    confirmation: VoltrManagerConfirmationInput,
}

fn fail(message: impl AsRef<str>) -> ! {
    eprintln!("backyard-voltr-restoration-bridge: {}", message.as_ref());
    std::process::exit(2)
}

fn read_input(path: &PathBuf) -> Value {
    let text = fs::read_to_string(path)
        .unwrap_or_else(|error| fail(format!("cannot read {}: {error}", path.display())));
    serde_json::from_str(&text)
        .unwrap_or_else(|error| fail(format!("invalid bridge JSON: {error}")))
}

fn sha256_json(value: &Value) -> String {
    format!(
        "{:x}",
        Sha256::digest(serde_json::to_vec(value).expect("serializable bridge JSON"))
    )
}

fn token_from_phase_a(
    handoff: &loyal_yield_store::fleet_orchestration::VoltrRestorationHandoff,
    signed: &VoltrManagerSignedIntentInput,
) -> VoltrRestorationBridgeToken {
    VoltrRestorationBridgeToken {
        schema_version: 1,
        event_id: handoff.lease.event.id,
        cluster: handoff.lease.event.cluster.clone(),
        owner: handoff.lease.owner.clone(),
        fencing_token: handoff.lease.fencing_token,
        origin_id: handoff.origin_id.clone(),
        generation: handoff.generation,
        leg_id: handoff.leg.leg_id.clone(),
        manager_intent_id: signed.manager_intent_id.clone(),
        expected_signature: signed.expected_signature.clone(),
        signed_transaction_sha256: signed.signed_transaction_sha256.clone(),
        message_sha256: signed.message_sha256.clone(),
        strategy_id: signed.strategy_id.clone(),
        reserve: signed.reserve.clone(),
        amount_raw: signed.amount_raw,
        lifecycle_id: signed.lifecycle_id.clone(),
        route_authorization_sha256: signed.route_authorization_sha256.clone(),
        protected_prestate_sha256: signed.protected_prestate_sha256.clone(),
        protected_address_set_sha256: signed.protected_address_set_sha256.clone(),
        protected_context_slot: signed.protected_context_slot,
    }
}

async fn phase_a(
    input: PhaseAInput,
    neon: &NeonSqlClient,
) -> Result<Value, Box<dyn std::error::Error>> {
    if input.schema_version != 1
        || input.phase != "prepare"
        || input.cluster != VOLTR_FOUR_MARKET_CLUSTER
        || input.route_id != VOLTR_FOUR_MARKET_ROUTE_ID
        || input.route_spec_sha256 != VOLTR_FOUR_MARKET_ROUTE_SPEC_SHA256
        || input.vault != VOLTR_FOUR_MARKET_VAULT
        || input.owner.trim().is_empty()
        || input.owner.len() > 128
        || input.lease_seconds < 60
        || input.lease_seconds > 900
        || input.generation <= 0
    {
        return Err("Phase-A bridge identity/lease bounds are malformed".into());
    }
    let expires_at = Utc::now() + Duration::seconds(input.lease_seconds);
    let Some(handoff) = neon
        .lease_exact_voltr_restoration_handoff(
            &input.cluster,
            &input.owner,
            expires_at,
            &input.origin_id,
            input.generation,
            &input.leg_id,
        )
        .await?
    else {
        return Err("exact restoration origin/generation/leg is not leasable".into());
    };
    if handoff.origin_id != input.origin_id
        || handoff.generation != input.generation
        || handoff.leg.leg_id != input.leg_id
        || handoff.leg.strategy_id != input.signed_intent.strategy_id
        || handoff.leg.reserve != input.signed_intent.reserve
        || handoff.leg.amount_raw != input.signed_intent.amount_raw
    {
        return Err("leased restoration leg does not equal the requested signed intent".into());
    }
    neon.acquire_voltr_restoration_logical_conflict_lease(&handoff)
        .await?;
    neon.persist_voltr_manager_signed_intent(&handoff, &input.signed_intent)
        .await?;
    neon.mark_voltr_manager_broadcast_intent(&handoff, &input.signed_intent.expected_signature)
        .await?;
    let token = token_from_phase_a(&handoff, &input.signed_intent);
    let token_value = serde_json::to_value(&token)?;
    Ok(json!({
        "verdict": "BACKYARD_VOLTR_RESTORATION_BRIDGE_PHASE_A_PASS",
        "broadcast": false,
        "signerLoaded": false,
        "phase": "prepare",
        "token": token,
        "tokenSha256": sha256_json(&token_value),
        "managerHandoff": {
            "operation": "manager-withdraw",
            "strategyId": handoff.leg.strategy_id,
            "reserve": handoff.leg.reserve,
            "amountRaw": handoff.leg.amount_raw,
            "eventId": handoff.lease.event.id,
            "fencingToken": handoff.lease.fencing_token,
            "leaseExpiresAt": expires_at.to_rfc3339(),
            "expectedSignature": input.signed_intent.expected_signature,
        },
        "nextStep": "Run the canonical TypeScript manager command with this exact persisted wire; do not rebuild or resend. Then invoke Phase B with the exact confirmed command readback.",
    }))
}

async fn phase_b(
    input: PhaseBInput,
    neon: &NeonSqlClient,
) -> Result<Value, Box<dyn std::error::Error>> {
    if input.schema_version != 1
        || input.phase != "confirm"
        || input.token.cluster != VOLTR_FOUR_MARKET_CLUSTER
    {
        return Err("Phase-B bridge schema/phase is malformed".into());
    }
    let completion: VoltrRestorationBridgeCompletion = neon
        .complete_voltr_restoration_bridge(&input.token, &input.confirmation)
        .await?;
    Ok(json!({
        "verdict": "BACKYARD_VOLTR_RESTORATION_BRIDGE_PHASE_B_PASS",
        "broadcast": false,
        "signerLoaded": false,
        "phase": "confirm",
        "completion": completion,
        "tokenSha256": sha256_json(&serde_json::to_value(&input.token)?),
    }))
}

fn main() {
    let mut args = env::args().skip(1);
    let mut input_path: Option<PathBuf> = None;
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--input" => input_path = args.next().map(PathBuf::from),
            "--help" | "-h" => fail(
                "usage: backyard-voltr-restoration-bridge --input <phase-a-or-b.json>\n\
                 Requires NEON_DATABASE_URL. It never loads a signer or sends Solana.",
            ),
            other => fail(format!("unknown argument: {other}")),
        }
    }
    let Some(input_path) = input_path else {
        fail("--input <phase-a-or-b.json> is required")
    };
    let value = read_input(&input_path);
    let phase = value
        .get("phase")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_owned();
    let neon_url =
        env::var("NEON_DATABASE_URL").unwrap_or_else(|_| fail("NEON_DATABASE_URL is required"));
    let runtime = tokio::runtime::Runtime::new().unwrap_or_else(|error| fail(error.to_string()));
    let result = runtime.block_on(async {
        let neon =
            NeonSqlClient::connect(NeonSqlConfig::new(neon_url).with_max_connections(2)).await?;
        if phase == "prepare" {
            phase_a(serde_json::from_value(value)?, &neon).await
        } else if phase == "confirm" {
            phase_b(serde_json::from_value(value)?, &neon).await
        } else {
            Err::<Value, Box<dyn std::error::Error>>("phase must be prepare or confirm".into())
        }
    });
    let output = result.unwrap_or_else(|error| fail(format!("bridge failed closed: {error}")));
    println!(
        "{}",
        serde_json::to_string(&output).expect("serializable bridge output")
    );
}
