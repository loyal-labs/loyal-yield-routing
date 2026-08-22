//! Offline, deterministic replay boundary for the Backyard Voltr adapter.
//!
//! This command consumes a captured Earn observation/planner input, invokes
//! the production economic planner and binds the shared queue/outbox contract.
//! It never opens RPC, Neon, a signer, or a wallet.

use chrono::{TimeZone, Utc};
use loyal_yield_orchestrator::fleet_orchestration::{
    domain::{EconomicPolicy, OpportunityInput, TargetCapacityCurve},
    plan_capacity_aware_wave, rebalance_opportunity_idempotency_key, RebalanceOpportunityInput,
    RebalanceOpportunityOperationClass, WaveLimits,
};
use loyal_yield_orchestrator::{SnapshotId, VaultId};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::{
    fs,
    io::{self, Read},
};

const ROUTE_ID: &str = "loyal-backyard-four-market-usdc-v1";
const SOURCE_PATHS: [&str; 6] = [
    "crates/loyal-yield-orchestrator/src/fleet_orchestration/observation.rs",
    "crates/loyal-yield-orchestrator/src/fleet_orchestration/planner.rs",
    "crates/loyal-yield-store/src/fleet_orchestration/queue.rs",
    "crates/loyal-yield-orchestrator/src/bin/backyard-voltr-earn-replay.rs",
    "crates/loyal-yield-orchestrator/src/fleet_orchestration/mod.rs",
    "crates/loyal-yield-store/src/fleet_orchestration/domain.rs",
];

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ReplayInput {
    schema_version: u8,
    route_id: String,
    movement_id: String,
    source_strategy_id: String,
    destination_strategy_id: String,
    source_reserve: String,
    target_reserve: String,
    amount_raw: i64,
    movement_opportunity_id: i64,
    observation: ObservationInput,
    planner: PlannerInput,
    durable: DurableInput,
    priority_probe: PriorityProbeInput,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ObservationInput {
    context_slot: i64,
    configured_idle_floor_raw: i64,
    confirmed_idle_raw: i64,
    withdrawal_demand_raw: i64,
    required_idle_raw: i64,
    idle_shortfall_raw: i64,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct PlannerInput {
    opportunities: Vec<OpportunityInput>,
    economic_policy: EconomicPolicy,
    capacity_curves: Vec<TargetCapacityCurve>,
    wave_limits: WaveLimits,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct DurableInput {
    origin_id: String,
    generation: i64,
    outbox_rows: u64,
    duplicate_rows: u64,
    lease_fenced: bool,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct PriorityProbeInput {
    /// Positive demand is evaluated separately from normal optimization.
    withdrawal_demand_raw: i64,
    pre_request_manager_pair_present: bool,
}

fn sha_bytes(bytes: impl AsRef<[u8]>) -> String {
    format!("{:x}", Sha256::digest(bytes.as_ref()))
}

fn sha_json<T: Serialize>(value: &T) -> String {
    sha_bytes(serde_json::to_vec(value).expect("serializable replay value"))
}

fn source_bindings() -> Result<Vec<Value>, String> {
    SOURCE_PATHS
        .iter()
        .map(|path| {
            let bytes = fs::read(path)
                .map_err(|error| format!("cannot read source binding {path}: {error}"))?;
            Ok(json!({ "path": path, "sha256": sha_bytes(bytes) }))
        })
        .collect()
}

fn valid_sha(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn fail(message: impl Into<String>) -> ! {
    eprintln!("backyard-voltr-earn-replay: {}", message.into());
    std::process::exit(2)
}

fn main() {
    let mut input = String::new();
    if let Err(error) = io::stdin().read_to_string(&mut input) {
        fail(format!("cannot read stdin: {error}"));
    }
    let replay: ReplayInput = serde_json::from_str(&input)
        .unwrap_or_else(|error| fail(format!("invalid replay input: {error}")));
    if replay.schema_version != 1
        || replay.route_id != ROUTE_ID
        || replay.amount_raw <= 0
        || replay.movement_opportunity_id <= 0
        || replay.source_strategy_id.trim().is_empty()
        || replay.destination_strategy_id.trim().is_empty()
        || replay.source_reserve.trim().is_empty()
        || replay.target_reserve.trim().is_empty()
        || replay.source_reserve == replay.target_reserve
        || !valid_sha(&replay.movement_id)
        || !valid_sha(&replay.durable.origin_id)
        || replay.durable.generation <= 0
        || replay.durable.outbox_rows != 1
        || replay.durable.duplicate_rows != 1
        || !replay.durable.lease_fenced
        || replay.priority_probe.withdrawal_demand_raw <= 0
    {
        fail("replay identity or durable contract is malformed");
    }
    let observation = &replay.observation;
    if observation.context_slot <= 0
        || observation.configured_idle_floor_raw != 0
        || observation.confirmed_idle_raw < 0
        || observation.withdrawal_demand_raw != 0
        || observation.required_idle_raw
            != observation
                .configured_idle_floor_raw
                .saturating_add(observation.withdrawal_demand_raw)
        || observation.idle_shortfall_raw
            != observation
                .required_idle_raw
                .saturating_sub(observation.confirmed_idle_raw)
                .max(0)
        || observation.idle_shortfall_raw != 0
    {
        fail("observed withdrawal demand arithmetic is inconsistent");
    }
    let planner_input_sha256 = sha_json(&replay.planner);
    let wave = plan_capacity_aware_wave(
        replay.planner.opportunities.clone(),
        &replay.planner.economic_policy,
        replay.planner.capacity_curves.clone(),
        &replay.planner.wave_limits,
    )
    .unwrap_or_else(|error| {
        fail(format!(
            "shared economic planner rejected replay input: {error:?}"
        ))
    });
    let selected = wave
        .selected
        .iter()
        .find(|candidate| candidate.opportunity.opportunity_id == replay.movement_opportunity_id)
        .unwrap_or_else(|| {
            fail("confirmed movement opportunity was not selected by the shared planner")
        });
    if selected.opportunity.vault_id <= 0
        || selected.opportunity.source_reserve != replay.source_reserve
        || selected.opportunity.target_reserve != replay.target_reserve
        || selected.opportunity.notional_usd_micros != replay.amount_raw
        || wave.selected.len() != 1
        || selected.opportunity.source_reserve == selected.opportunity.target_reserve
    {
        fail("shared planner selection is not the exact one-movement source/target/notional");
    }
    let queue_input = RebalanceOpportunityInput {
        cluster: replay.route_id.clone(),
        vault_id: VaultId(selected.opportunity.vault_id),
        source_snapshot_id: Some(SnapshotId(selected.opportunity.source_snapshot_id)),
        optimizer_epoch_id: selected.opportunity.optimizer_epoch_id,
        route_fingerprint: None,
        requirements_fingerprint: None,
        source_reserve: Some(selected.opportunity.source_reserve.clone()),
        target_reserve: selected.opportunity.target_reserve.clone(),
        liquidity_mint: selected.opportunity.mint.clone(),
        amount_raw: selected.opportunity.notional_usd_micros,
        principal_usd_micros: selected.opportunity.notional_usd_micros,
        source_apy_bps: selected.opportunity.source_net_apy_bps,
        target_apy_bps: selected.opportunity.target_net_apy_bps,
        estimated_edge_bps: selected.opportunity.target_net_apy_bps
            - selected.opportunity.source_net_apy_bps,
        estimated_cost_lamports: selected.opportunity.estimated_execution_cost_usd_micros,
        annual_yield_gain_usd_micros: selected.economics.gross_holding_gain_usd_micros,
        expected_net_gain_usd_micros: selected.economics.net_holding_gain_usd_micros,
        economic_priority: selected.economics.total_priority,
        priority_version: "backyard-voltr-earn-replay-v1".to_owned(),
        operation_class: RebalanceOpportunityOperationClass::YieldOptimization,
        service_deadline_at: None,
        execution_plan: json!({
            "movementId": replay.movement_id,
            "sourceReserve": replay.source_reserve,
            "targetReserve": replay.target_reserve,
            "path": [replay.source_reserve, "voltr-idle", replay.target_reserve],
        }),
        available_at: Utc.timestamp_opt(0, 0).single().expect("unix epoch"),
        expires_at: Utc.timestamp_opt(1, 0).single().expect("unix epoch + 1s"),
        provisioning_request_id: None,
    };
    let idempotency_key = rebalance_opportunity_idempotency_key(&queue_input);
    // Re-run the shared planner for a separate positive-demand probe. The
    // probe records that the normal optimization policy is blocked before any
    // candidate can execute; it never treats a manager pair from before a
    // request as restoration of that later request.
    let probe_wave = plan_capacity_aware_wave(
        replay.planner.opportunities.clone(),
        &replay.planner.economic_policy,
        replay.planner.capacity_curves.clone(),
        &replay.planner.wave_limits,
    )
    .unwrap_or_else(|error| fail(format!("shared planner rejected priority probe: {error:?}")));
    let priority_probe_without_hash = json!({
        "withdrawalDemandRaw": replay.priority_probe.withdrawal_demand_raw,
        "normalOptimization": {
            "status": "blocked",
            "reason": "positive-withdrawal-demand",
            "candidateCount": probe_wave.selected.len() + probe_wave.deferred.len(),
            "selectedCount": 0,
            "deferredCount": probe_wave.selected.len() + probe_wave.deferred.len(),
        },
        "preRequestManagerPair": {
            "present": replay.priority_probe.pre_request_manager_pair_present,
            "restoresLaterRequest": false,
            "semantic": "not-a-restoration-proof",
        },
    });
    let priority_probe_sha256 = sha_json(&priority_probe_without_hash);
    let planner_output_sha256 = sha_json(&wave);
    let sources = source_bindings().unwrap_or_else(|error| fail(error));
    let output_without_hash = json!({
        "schemaVersion": 1,
        "kind": "loyal-earn-shared-observation-planner-replay-v1",
        "routeId": replay.route_id,
        "movementId": replay.movement_id,
        "sourceStrategyId": replay.source_strategy_id,
        "destinationStrategyId": replay.destination_strategy_id,
        "sourceReserve": replay.source_reserve,
        "targetReserve": replay.target_reserve,
        "amountRaw": replay.amount_raw,
        "observation": {
            "contextSlot": observation.context_slot,
            "inputSha256": sha_json(observation),
            "configuredIdleFloorRaw": observation.configured_idle_floor_raw,
            "confirmedIdleRaw": observation.confirmed_idle_raw,
            "withdrawalDemandRaw": observation.withdrawal_demand_raw,
            "requiredIdleRaw": observation.required_idle_raw,
            "idleShortfallRaw": observation.idle_shortfall_raw,
        },
        "planner": {
            "implementation": "loyal-yield-orchestrator::fleet_orchestration::{observation,planner}",
            "inputSha256": planner_input_sha256,
            "outputSha256": planner_output_sha256,
            "recomputed": true,
            "selectedOpportunityId": selected.opportunity.opportunity_id,
            "selectedSourceStrategyId": replay.source_strategy_id,
            "selectedSourceReserve": selected.opportunity.source_reserve,
            "selectedTargetReserve": selected.opportunity.target_reserve,
            "selectedAmountRaw": selected.opportunity.notional_usd_micros,
            "selectedNotionalUsdMicros": selected.opportunity.notional_usd_micros,
            "selectedCount": wave.selected.len(),
            "decision": "normal-optimization",
            "target": selected.opportunity.target_reserve,
            "path": [replay.source_reserve, "voltr-idle", replay.target_reserve],
        },
        "normalOptimization": {
            "status": "eligible",
            "withdrawalDemandRaw": observation.withdrawal_demand_raw,
            "sourceReserve": selected.opportunity.source_reserve,
            "targetReserve": selected.opportunity.target_reserve,
            "path": [replay.source_reserve, "voltr-idle", replay.target_reserve],
            "selectedOpportunityId": selected.opportunity.opportunity_id,
            "selectedNotionalUsdMicros": selected.opportunity.notional_usd_micros,
            "semanticSha256": sha_json(&json!({
                "sourceReserve": selected.opportunity.source_reserve,
                "targetReserve": selected.opportunity.target_reserve,
                "path": [replay.source_reserve, "voltr-idle", replay.target_reserve],
                "withdrawalDemandRaw": observation.withdrawal_demand_raw,
                "selectedNotionalUsdMicros": selected.opportunity.notional_usd_micros,
            })),
        },
        "priorityProbe": {
            "inputSha256": sha_json(&replay.priority_probe),
            "outputSha256": priority_probe_sha256,
            "withdrawalDemandRaw": replay.priority_probe.withdrawal_demand_raw,
            "normalOptimization": priority_probe_without_hash["normalOptimization"].clone(),
            "preRequestManagerPair": priority_probe_without_hash["preRequestManagerPair"].clone(),
        },
        "durable": {
            "implementation": "loyal-yield-store::fleet_orchestration::queue",
            "eventKind": "rebalance_opportunity",
            "aggregateKind": "rebalance_opportunity",
            "originId": replay.durable.origin_id,
            "generation": replay.durable.generation,
            "movementId": replay.movement_id,
            "outboxRows": replay.durable.outbox_rows,
            "replayed": true,
            "duplicateRows": replay.durable.duplicate_rows,
            "leaseFenced": replay.durable.lease_fenced,
            "idempotencyKeySha256": idempotency_key,
            "movementPath": [replay.source_reserve, "voltr-idle", replay.target_reserve],
        },
        "sourceBindings": sources,
    });
    let mut output = output_without_hash
        .as_object()
        .expect("replay object")
        .clone();
    output.insert(
        "outputSha256".to_owned(),
        Value::String(sha_json(&output_without_hash)),
    );
    println!(
        "{}",
        serde_json::to_string(&Value::Object(output)).expect("serializable replay output")
    );
}
