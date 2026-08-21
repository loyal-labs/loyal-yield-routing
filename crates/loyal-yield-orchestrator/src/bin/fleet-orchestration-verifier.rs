#![recursion_limit = "512"]

use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    fs,
    path::{Path, PathBuf},
    process::{Command, ExitCode},
    str::FromStr,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use chrono::{DateTime, Utc};
use loyal_actions::{KAMINO_MAIN_USDC_RESERVE, USDC_MINT};
use loyal_yield_orchestrator::fleet_orchestration::{
    classify_authoritative_signature_status, evaluate_economics, evaluate_fresh_route_economics,
    fleet_worker_role_probe, functional_stuck_stage_fixture, functional_worker_resilience_fixture,
    material_frontier_deterministic_evidence, plan_capacity_aware_wave, rank_opportunities,
    route_fee_budget, run_deterministic_benchmark, schedule_authoritative_status_poll,
    AuthoritativeConfirmationDecision, AuthoritativePollUrgency, AuthoritativeSignatureStatus,
    CapacityBand, ConfirmationPollTrigger, EconomicPolicy, FleetStuckStage, FleetWorkerRole,
    FreshRouteEconomicsError, FreshRouteEconomicsInput, ImmutableMarketEpoch, IneligibleReason,
    MarketEpochReserve, MarketMintCoverage, MaterialFrontierDisposition, OpportunityInput,
    RebalanceOpportunityAdvance, RebalanceOpportunityAdvanceOutcome, RebalanceOpportunityClaimKind,
    RebalanceOpportunityInput, RebalanceOpportunityLease, RebalanceOpportunityRecord,
    RebalanceOpportunityState, RouteFeeBudgetError, RouteFeePayerKind, RouteFeePolicy,
    RuntimeSourceBinding, SignedRouteSubmissionAdvance, SignedRouteSubmissionInput,
    SignedRouteSubmissionRecord, TargetCapacityCurve, TargetCapacityObservation,
    TargetCapacityProjection, TargetCapacityReservationInput, WaveLimits,
};
use loyal_yield_orchestrator::{
    lookup_table_manifest_address_records_hash, lookup_table_rollout_lock_acquisition_count,
    supported_stable_mints, AtomicVaultAllocationResult, CrossMintVaultOptInLookup,
    DecisionAdvance, LookupTableAllocationKind, LookupTableFamilyKind, LookupTableFamilyRecord,
    LookupTableFamilyState, LookupTableFamilyUpsert, LookupTableLifecycle,
    LookupTableManifestAddressRecord, LookupTableManifestSubject, LookupTableMembershipAddress,
    LookupTableOperationEnqueue, LookupTableOperationKind, LookupTableOperationLease,
    LookupTableOperationRecord, LookupTableProvisionerBroadcastPermitResult,
    LookupTableProvisioningPlanPolicy, LookupTableProvisioningRequestUpsert,
    LookupTableReadinessRecord, LookupTableReadinessStatus, LookupTableRolloutMode,
    LookupTableSelectionKind, LookupTableSimulationState, LookupTableUsageLeaseBundle,
    LookupTableUsageLeaseKind, NeonSqlClient, NeonSqlConfig, OrchestratorError, PackedShardPolicy,
    ReconciledReservePosition, ReconciledVaultState, ReusableLookupTableInsert,
    SameMintRebalanceInput, SharedMarketCatalogUpsert, SignedLookupTableTransaction, VaultId,
    ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
};
use loyal_yield_store::fleet_orchestration::{
    CrossMintBalanceAnchors, CrossMintContinuationLease, CrossMintCustodyPhase,
    CrossMintExpectedEffect, CrossMintFallbackCapacityInput, CrossMintLegPublicationInput,
    CrossMintLegPurpose, CrossMintLegReconciliationInput, CrossMintMovementActivationInput,
    CrossMintMovementCloseInput, CrossMintMovementLeg, CrossMintMovementRecord,
    CrossMintNoEffectProofInput, CrossMintPolicyBindings, CrossMintReconciledEffect,
    CrossMintTerminalOutcome, KaminoPositionAnchor, SignedRouteSubmissionLease,
    SignedRouteSubmissionState, TokenBalanceAnchor, TokenBalanceDelta,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::signature::{Keypair, Signer};
use sqlx::{postgres::PgConnectOptions, Row};

const TEN_SECONDS_MILLIS: u128 = 10_000;
const PRODUCTION_EVIDENCE_MAX_AGE: chrono::Duration = chrono::Duration::seconds(120);
const PRODUCTION_EVIDENCE_MAX_FUTURE_SKEW: chrono::Duration = chrono::Duration::seconds(30);
const PRODUCTION_EVIDENCE_MAX_COLLECTION_SECONDS: i64 = 300;
const PRODUCTION_COMPONENT_MAX_LAG_SECONDS: i64 = 90;
const PRODUCTION_CLUSTER: &str = "mainnet-beta";
const PRODUCTION_RENDER_ENVIRONMENT_ID: &str = "evm-d8kgt4r7uimc73b1ul1g";
const HEAVY_RENDER_ENVIRONMENT_ID: &str = "evm-d8kgt3a8qa3s7382glc0";
const KAMINO_MONITOR_SERVICE_ID: &str = "srv-d8h4i9a8pkls73bver00";
const KAMINO_MONITOR_SERVICE_NAME: &str = "loyal-kamino-reserve-monitor";
const KAMINO_MONITOR_COMMAND: &str =
    "/usr/local/bin/kamino-reserve-monitor --confirmed-refresh-interval-secs 1";
const KAMINO_MONITOR_PREDEPLOY: &str = "/usr/local/bin/kamino-monitor-predeploy";
const TIMESCALE_MARKET_MIGRATION_VERSION: i64 = 5;
const TIMESCALE_MARKET_MIGRATION_NAME: &str = "kamino_confirmed_state_verification";
const MARKET_VERIFICATION_WARNING_SECONDS: i64 = 90;
const MARKET_EVIDENCE_QUERY_TIMEOUT_MILLISECONDS: i64 = 15_000;
const SUPPORTED_RESERVE_CATALOG_MAX_AGE_SECONDS: i64 = 300;
const IMAGE_PROVENANCE_SOURCE: &str = "https://github.com/loyal-labs/loyal-yield-routing";
const STANDARD_POLICY_PUBKEY: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const ADDRESS_LOOKUP_TABLE_PROGRAM_ID: &str = "AddressLookupTab1e1111111111111111111111111";
const MATERIAL_STAGE_MAX_AGE_SECONDS: i64 = 600;
const COMPLETE_SWEEP_MAX_AGE_SECONDS: i64 = 120;
const MATERIAL_PRINCIPAL_USD_MICROS: i64 = 1_000_000_000;
const PRODUCTION_SERVICE_NAMES: [&str; 6] = [
    "loyal-fleet-opportunity-planner",
    "loyal-fleet-route-revalidator",
    "loyal-fleet-route-executor",
    "loyal-fleet-route-confirmer",
    "loyal-fleet-route-reconciler",
    "loyal-route-lookup-table-provisioner",
];
const VERIFIED_MIGRATIONS: [(i64, &str, &str); 15] = [
    (
        23,
        "value_priority_rebalance_queue",
        "0023_value_priority_rebalance_queue.sql",
    ),
    (
        24,
        "fleet_route_confirmer",
        "0024_fleet_route_confirmer.sql",
    ),
    (
        25,
        "fee_only_route_payer_shards",
        "0025_fee_only_route_payer_shards.sql",
    ),
    (
        26,
        "target_capacity_reservations",
        "0026_target_capacity_reservations.sql",
    ),
    (
        27,
        "rebalance_opportunity_attempt_generations",
        "0027_rebalance_opportunity_attempt_generations.sql",
    ),
    (
        28,
        "reusable_alt_terminal_repair",
        "0028_reusable_alt_terminal_repair.sql",
    ),
    (
        29,
        "fleet_commit_lifetime_fences",
        "0029_fleet_commit_lifetime_fences.sql",
    ),
    (
        30,
        "fused_queue_accrual_binding",
        "0030_fused_queue_accrual_binding.sql",
    ),
    (
        31,
        "fleet_commit_lifetime_fence_errcode",
        "0031_fleet_commit_lifetime_fence_errcode.sql",
    ),
    (
        32,
        "idle_vault_decision_lookup_index",
        "0032_idle_vault_decision_lookup_index.sql",
    ),
    (
        33,
        "policy_setup_funding_reservations",
        "0033_policy_setup_funding_reservations.sql",
    ),
    (
        34,
        "fleet_health_snapshot_projection",
        "0034_fleet_health_snapshot_projection.sql",
    ),
    (
        35,
        "durable_cross_mint_movements",
        "0035_durable_cross_mint_movements.sql",
    ),
    (
        36,
        "cross_mint_swap_policies",
        "0036_cross_mint_swap_policies.sql",
    ),
    (
        37,
        "cross_mint_vault_opt_ins",
        "0037_cross_mint_vault_opt_ins.sql",
    ),
];

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
enum Verdict {
    Pass,
    Fail,
    NotRun,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Subcheck {
    name: &'static str,
    verdict: Verdict,
    #[serde(skip_serializing_if = "Option::is_none")]
    first_failing_invariant: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    safe_response: Option<String>,
    evidence: Value,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct VerifierCheck {
    id: u8,
    name: &'static str,
    verdict: Verdict,
    first_failing_invariant: Option<&'static str>,
    evidence: Value,
    subchecks: Vec<Subcheck>,
}

struct DeterministicEvidence {
    discovery_subchecks: Vec<Subcheck>,
    economic_subchecks: Vec<Subcheck>,
    execution_subchecks: Vec<Subcheck>,
}

struct DatabaseEvidence {
    migration_subchecks: Vec<Subcheck>,
    discovery_subchecks: Vec<Subcheck>,
    alt_subchecks: Vec<Subcheck>,
    execution_subchecks: Vec<Subcheck>,
}

struct LocalEvidence {
    repository_subchecks: Vec<Subcheck>,
    wiring_subchecks: Vec<Subcheck>,
    repository_root: PathBuf,
    head_commit: Option<String>,
    runtime_source_digest_sha256: String,
    production_light_worker_image_reference: Option<String>,
    production_heavy_worker_image_reference: Option<String>,
}

struct Cli {
    implementation: bool,
    end_state: bool,
    json_output: bool,
    database_url: Option<String>,
    isolated_database: bool,
    collect_repository_evidence: bool,
    repository_root: Option<PathBuf>,
    runtime_evidence_json: Option<PathBuf>,
    production_evidence_json: Option<PathBuf>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ProductionEvidenceV1 {
    schema_version: u32,
    event: String,
    collection_started_at: DateTime<Utc>,
    collected_at: DateTime<Utc>,
    captured_at: DateTime<Utc>,
    head_commit: Option<String>,
    scope: ProductionEvidenceScope,
    source: ProductionEvidenceSource,
    measurements: ProductionMeasurements,
    #[serde(rename = "recomputedVerdicts")]
    _recomputed_verdicts: Value,
    caller_verdicts_accepted: bool,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ProductionEvidenceScope {
    cluster: String,
    render_environment_id: String,
    cutover_at: Option<DateTime<Utc>>,
    baseline_path_supplied: bool,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ProductionEvidenceSource {
    repository_head: Option<String>,
    tracked_worktree_dirty: bool,
    render_yaml_sha256: String,
    collector_compiled_source_sha256: String,
    collector_checkout_source_sha256: Option<String>,
    collector_executable_sha256: Option<String>,
    collector_source: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ProductionMeasurements {
    render: Value,
    market_data_plane: ProductionMarketDataPlaneMeasurements,
    database: ProductionDatabaseMeasurements,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ProductionMarketDataPlaneMeasurements {
    timescale: Value,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ProductionDatabaseMeasurements {
    migrations: Value,
    queue: Value,
    positions: Value,
    movement: Value,
    #[serde(default)]
    alt_repair: Option<Value>,
}

struct ProductionEvidenceBinding {
    artifact: ProductionEvidenceV1,
    repository_root: PathBuf,
    head_commit: String,
    render_yaml_sha256: String,
}

#[derive(Debug)]
struct ExpectedProductionService {
    name: String,
    image: String,
    command: String,
    pre_deploy_command: String,
    plan: String,
    env_keys: BTreeSet<String>,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeEvidenceV1 {
    schema_version: u32,
    head_commit: String,
    runtime_source_digest_sha256: String,
    captured_at: DateTime<Utc>,
    hardware: String,
    discovery: RuntimeDiscoveryEvidence,
    alt: RuntimeAltEvidence,
    execution: RuntimeExecutionEvidence,
    replay: RuntimeReplayEvidence,
    wiring: RuntimeWiringEvidence,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeDiscoveryEvidence {
    fleet_size: u64,
    eligible_current_vaults: u64,
    accounted_vaults: u64,
    vault_outcomes_by_reason: BTreeMap<String, u64>,
    active_exclusions_by_state: BTreeMap<String, u64>,
    optimizer_epoch_id: i64,
    epoch_expires_at: DateTime<Utc>,
    one_immutable_epoch: bool,
    planning_sample_epoch_proofs: Vec<RuntimePlannerEpochProof>,
    planning_sample_count: u64,
    planning_p95_milliseconds: u64,
    replay_vault_count: u64,
    replay_milliseconds: u64,
    economically_ordered: bool,
    top_cohort_has_no_nonconflicting_priority_inversion: bool,
    child_route_or_reconcile_processes_spawned: u64,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimePlannerEpochProof {
    market_epoch_optimizer_id: i64,
    observed_opportunity_epoch_ids: Vec<i64>,
    selected_opportunity_epoch_ids: Vec<i64>,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeAltEvidence {
    typed_provisioner_dry_run_plans: u64,
    reusable_v2_plans: u64,
    legacy_or_exact_route_alt_plans: u64,
    ready_jobs_seeded: u64,
    ready_jobs_claimed: u64,
    waiting_alt_jobs: u64,
    waiting_alt_decisions: u64,
    claim_latency_gate_clock: String,
    ready_claim_baseline_p95_micros: u64,
    ready_claim_cold_p95_micros: u64,
    ready_claim_baseline_client_p95_micros: u64,
    ready_claim_cold_client_p95_micros: u64,
    durable_coverage_wakeup_rows: u64,
    affected_jobs_promoted: u64,
    unaffected_jobs_promoted: u64,
    additional_fleet_cycle_required: bool,
    normal_readiness_global_rollout_lock_acquisitions: u64,
    independent_physical_alt_lanes_progressed: u64,
    same_table_predecessor_violations: u64,
    stale_fence_commits: u64,
    usage_leases_rejected_during_mutation: u64,
    mutating_operations_leased_during_usage: u64,
    verify_operations_leased_during_usage: u64,
    usage_fence_broadcast_commits: u64,
    usage_fence_broadcast_rejections: u64,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeExecutionEvidence {
    duplicate_active_vault_movements: u64,
    nonoverlapping_concurrent_leases: u64,
    overlapping_lane_limit_violations: u64,
    physical_writable_key_congestion_visible: bool,
    expired_lease_reclaimed_with_higher_fence: bool,
    mixed_runnable_and_expired_claims_full_and_disjoint: bool,
    fleet_wide_exclusive_route_leases: u64,
    identical_byte_rebroadcast_attempts: u64,
    rebroadcast_byte_mismatches: u64,
    replacement_before_expiry_and_absence_proof: u64,
    ambiguous_or_stale_replacement_movements: u64,
    post_confirm_reads: u64,
    min_context_slot_violations: u64,
    policy_execution_signed_by_policy_keypair: bool,
    alt_mutations_authorized_and_paid_by_policy_keypair: bool,
    sharded_route_fixtures: u64,
    shard_is_final_fee_payer: bool,
    policy_is_second_static_signer: bool,
    final_manifest_and_alt_coverage_match: bool,
    final_packet_simulation_fee_and_hashes_match: bool,
    setup_idle_and_farm_init_use_policy_payer: bool,
    shard_registry_keypair_match: bool,
    reciprocal_authority_separation: bool,
    bounded_ranked_failover: bool,
    low_balance_limits_enforced: bool,
    atomic_immutable_spend_reservation: bool,
    source_evidence_contract_fixtures: RuntimeSourceEvidenceContractFixtures,
    target_capacity_concurrent_admission_bounded: bool,
    pre_send_target_capacity_released: bool,
    reconciled_capacity_strict_telemetry_fence: bool,
    preexisting_newer_telemetry_release: bool,
    readiness_writers_waited_on_per_vault_fence: bool,
    readiness_parent_before_physical_lock_order_proved: bool,
    serialized_readiness_row_count: u64,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeSourceEvidenceContractFixtures {
    reserve_position: RuntimeSourceEvidenceContractFixture,
    idle_vault_usdc: RuntimeSourceEvidenceContractFixture,
    contaminated_reserve: RuntimeSourceEvidenceContractFixture,
    mismatched_route_kind: RuntimeSourceEvidenceContractFixture,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeSourceEvidenceContractFixture {
    source_kind: String,
    source_reserve: Option<String>,
    source_snapshot_id: Option<i64>,
    execution_plan: Value,
    projected_evidence: RuntimeProjectedSourceEvidence,
    validation_error: Option<String>,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeProjectedSourceEvidence {
    expected_idle_token_account: Option<String>,
    expected_idle_observed_slot: Option<i64>,
    expected_idle_observed_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeReplayEvidence {
    route_sample_count: u64,
    warm_high_value_submission_p95_milliseconds: u64,
    warm_confirmation_p95_milliseconds: u64,
    explicitly_excluded_cluster_outages: u64,
    warm_baseline_p95_milliseconds: u64,
    warm_with_alt_backlog_p95_milliseconds: u64,
    recoverable_yield_usd_micros_per_hour: i64,
    submitted_within_two_minutes_yield_ppm: u64,
    submitted_within_ten_minutes_yield_ppm: u64,
    configured_max_fee_fraction_ppm: u64,
    observed_max_fee_fraction_ppm: u64,
    negative_value_routes: u64,
    database_deadlocks: u64,
    duplicate_movements: u64,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeWiringEvidence {
    probed_container_image_reference: String,
    local_container_image_id: String,
    light_registry_index_digest: String,
    light_linux_amd64_manifest_digest: String,
    light_provenance_vcs_revision: String,
    light_provenance_vcs_source: String,
    probed_heavy_container_image_reference: String,
    heavy_registry_index_digest: String,
    heavy_linux_amd64_manifest_digest: String,
    heavy_provenance_vcs_revision: String,
    heavy_provenance_vcs_source: String,
    runnable_role_probe_exit_codes: BTreeMap<String, i32>,
    recovery_poll_interval_milliseconds: u64,
    health_observation_interval_milliseconds: u64,
    stuck_stage_detection_milliseconds: BTreeMap<String, u64>,
}

#[derive(Clone)]
struct DatabaseFixture {
    client: NeonSqlClient,
    latency_client: NeonSqlClient,
    prefix: String,
}

#[derive(Clone, Copy)]
struct SeededOpportunity {
    id: i64,
    economic_priority: i64,
}

struct AltDatabaseRuntimeMeasurements {
    typed_provisioner_dry_run_plans: u64,
    reusable_v2_plans: u64,
    legacy_or_exact_route_alt_plans: u64,
    normal_readiness_global_rollout_lock_acquisitions: u64,
    independent_physical_alt_lanes_progressed: u64,
    same_table_predecessor_violations: u64,
    stale_fence_commits: u64,
    stale_fence_rejections: u64,
    usage_leases_rejected_during_mutation: u64,
    mutating_operations_leased_during_usage: u64,
    verify_operations_leased_during_usage: u64,
    usage_fence_broadcast_commits: u64,
    usage_fence_broadcast_rejections: u64,
    alt_authority_payer_identity_consistent: bool,
    policy_pubkey: String,
}

fn lookup_table_operation_lease(
    operation: &LookupTableOperationRecord,
) -> Result<LookupTableOperationLease, Box<dyn Error>> {
    Ok(LookupTableOperationLease::new(
        operation
            .lease_owner
            .clone()
            .ok_or("lookup-table operation lease has no owner")?,
        operation.fencing_token,
        operation
            .lease_expires_at
            .ok_or("lookup-table operation lease has no expiry")?,
    )?)
}

fn runtime_random_pubkey() -> String {
    Keypair::new().pubkey().to_string()
}

fn opportunity(id: i64) -> OpportunityInput {
    OpportunityInput {
        opportunity_id: id,
        optimizer_epoch_id: 1,
        vault_id: id + 100,
        tenant_id: format!("tenant-{id}"),
        source_snapshot_id: id + 200,
        observed_slot: id + 300,
        mint: "USDC".to_owned(),
        source_reserve: format!("source-{id}"),
        target_reserve: "target".to_owned(),
        notional_usd_micros: 100_000_000_000,
        source_net_apy_bps: 200,
        target_net_apy_bps: 600,
        confidence_ppm: 1_000_000,
        expected_service_millis: 10_000,
        holding_horizon_seconds: 365 * 24 * 60 * 60,
        estimated_execution_cost_usd_micros: 250_000,
        age_seconds: 0,
        fairness_credit: 0,
        writable_conflict_keys: vec![format!("vault-{}", id + 100)],
    }
}

fn subcheck(name: &'static str, passed: bool, evidence: Value) -> Subcheck {
    Subcheck {
        name,
        verdict: if passed { Verdict::Pass } else { Verdict::Fail },
        first_failing_invariant: (!passed).then(|| format!("{name} behavioral predicate failed")),
        safe_response: (!passed).then(|| {
            "fail closed: do not advance, replace, recover, or release capacity until this invariant is proved"
                .to_owned()
        }),
        evidence,
    }
}

fn not_run_subcheck(name: &'static str, evidence: Value) -> Subcheck {
    Subcheck {
        name,
        verdict: Verdict::NotRun,
        first_failing_invariant: Some(format!("{name} was not executed")),
        safe_response: Some(
            "keep the affected execution path disabled until the missing verifier evidence is collected"
                .to_owned(),
        ),
        evidence,
    }
}

fn cross_mint_not_run_subcheck(
    name: &'static str,
    invariant: &'static str,
    missing_capabilities: Vec<String>,
    connection_required: &'static str,
) -> Subcheck {
    not_run_subcheck(
        name,
        json!({
            "invariant": invariant,
            "status": "NOT_RUN",
            "safeDefault": "cross-mint progression remains disabled",
            "missingCapabilities": missing_capabilities,
            "connectionRequired": connection_required,
        }),
    )
}

fn aggregate_verdicts(verdicts: impl IntoIterator<Item = Verdict>) -> Verdict {
    let mut saw_not_run = false;
    for verdict in verdicts {
        match verdict {
            Verdict::Fail => return Verdict::Fail,
            Verdict::NotRun => saw_not_run = true,
            Verdict::Pass => {}
        }
    }
    if saw_not_run {
        Verdict::NotRun
    } else {
        Verdict::Pass
    }
}

fn aggregate_subchecks(subchecks: &[Subcheck]) -> Verdict {
    aggregate_verdicts(subchecks.iter().map(|subcheck| subcheck.verdict))
}

fn sha256_hex(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    let mut encoded = String::with_capacity(digest.len() * 2);
    for byte in digest {
        encoded.push_str(&format!("{byte:02x}"));
    }
    encoded
}

fn output_tail(bytes: &[u8]) -> String {
    const MAX_BYTES: usize = 2_000;
    let start = bytes.len().saturating_sub(MAX_BYTES);
    String::from_utf8_lossy(&bytes[start..]).into_owned()
}

fn run_repository_command(
    repository_root: &Path,
    name: &'static str,
    program: &str,
    args: &[&str],
) -> Subcheck {
    let started = Instant::now();
    let result = Command::new(program)
        .args(args)
        .current_dir(repository_root)
        .env_remove("NEON_DATABASE_URL")
        .env_remove("TIMESCALEDB_URL")
        .env_remove("SOLANA_RPC_URL")
        .env_remove("POLICY_KEYPAIR")
        .env_remove("YIELD_ROUTER_KEYPAIR")
        .env_remove("SOLANA_TESTING_PK")
        .output();
    match result {
        Ok(output) => subcheck(
            name,
            output.status.success(),
            json!({
                "argv": std::iter::once(program).chain(args.iter().copied()).collect::<Vec<_>>(),
                "exitCode": output.status.code(),
                "elapsedMillis": started.elapsed().as_millis(),
                "stdoutSha256": sha256_hex(&output.stdout),
                "stderrSha256": sha256_hex(&output.stderr),
                "stdoutTail": output_tail(&output.stdout),
                "stderrTail": output_tail(&output.stderr),
            }),
        ),
        Err(error) => subcheck(
            name,
            false,
            json!({
                "argv": std::iter::once(program).chain(args.iter().copied()).collect::<Vec<_>>(),
                "error": error.to_string(),
            }),
        ),
    }
}

fn run_migration_runner_check(repository_root: &Path, database_url: &str) -> Subcheck {
    let started = Instant::now();
    let run_mode = |mode: &str| {
        Command::new("cargo")
            .args([
                "run",
                "-q",
                "-p",
                "loyal-yield-orchestrator",
                "--bin",
                "yield-migrations",
                "--",
                mode,
            ])
            .current_dir(repository_root)
            .env("NEON_DATABASE_URL", database_url)
            .env_remove("TIMESCALEDB_URL")
            .env_remove("SOLANA_RPC_URL")
            .env_remove("POLICY_KEYPAIR")
            .output()
    };
    let apply = run_mode("--apply");
    let check = apply
        .as_ref()
        .ok()
        .filter(|output| output.status.success())
        .map(|_| run_mode("--check"));
    match (apply, check) {
        (Ok(apply), Some(Ok(check))) => subcheck(
            "dedicated_migration_runner_reapply_and_check_pass",
            apply.status.success() && check.status.success(),
            json!({
                "databaseUrl": "REDACTED",
                "elapsedMillis": started.elapsed().as_millis(),
                "apply": {
                    "exitCode": apply.status.code(),
                    "stdoutSha256": sha256_hex(&apply.stdout),
                    "stderrSha256": sha256_hex(&apply.stderr),
                    "stdoutTail": output_tail(&apply.stdout),
                    "stderrTail": output_tail(&apply.stderr),
                },
                "check": {
                    "exitCode": check.status.code(),
                    "stdoutSha256": sha256_hex(&check.stdout),
                    "stderrSha256": sha256_hex(&check.stderr),
                    "stdoutTail": output_tail(&check.stdout),
                    "stderrTail": output_tail(&check.stderr),
                },
            }),
        ),
        (Ok(apply), Some(Err(error))) => subcheck(
            "dedicated_migration_runner_reapply_and_check_pass",
            false,
            json!({
                "databaseUrl": "REDACTED",
                "applyExitCode": apply.status.code(),
                "applyStdoutSha256": sha256_hex(&apply.stdout),
                "applyStderrSha256": sha256_hex(&apply.stderr),
                "checkError": error.to_string(),
            }),
        ),
        (Ok(apply), _) => subcheck(
            "dedicated_migration_runner_reapply_and_check_pass",
            false,
            json!({
                "databaseUrl": "REDACTED",
                "applyExitCode": apply.status.code(),
                "applyStdoutSha256": sha256_hex(&apply.stdout),
                "applyStderrSha256": sha256_hex(&apply.stderr),
                "applyStdoutTail": output_tail(&apply.stdout),
                "applyStderrTail": output_tail(&apply.stderr),
                "checkSkipped": true,
            }),
        ),
        (Err(error), _) => subcheck(
            "dedicated_migration_runner_reapply_and_check_pass",
            false,
            json!({"databaseUrl": "REDACTED", "error": error.to_string()}),
        ),
    }
}

fn repository_root(explicit: Option<&Path>) -> Result<PathBuf, Box<dyn Error>> {
    let candidate = if let Some(root) = explicit {
        root.to_path_buf()
    } else {
        let current = env::current_dir()?;
        current
            .ancestors()
            .find(|path| {
                path.join("Cargo.toml").is_file()
                    && path.join("Dockerfile.light-workers").is_file()
                    && path.join("render.yaml").is_file()
            })
            .ok_or("could not discover repository root; pass --repository-root")?
            .to_path_buf()
    };
    let canonical = fs::canonicalize(candidate)?;
    if !canonical.join("Cargo.toml").is_file()
        || !canonical.join("Dockerfile.light-workers").is_file()
        || !canonical.join("render.yaml").is_file()
        || !canonical.join(".git").exists()
    {
        return Err(
            "repository root is missing Cargo.toml, Dockerfile.light-workers, render.yaml, or .git"
                .into(),
        );
    }
    Ok(canonical)
}

fn git_stdout(repository_root: &Path, args: &[&str]) -> Option<String> {
    let output = Command::new("git")
        .args(args)
        .current_dir(repository_root)
        .output()
        .ok()?;
    output
        .status
        .success()
        .then(|| String::from_utf8_lossy(&output.stdout).trim().to_owned())
}

fn git_success(repository_root: &Path, args: &[&str]) -> bool {
    Command::new("git")
        .args(args)
        .current_dir(repository_root)
        .status()
        .is_ok_and(|status| status.success())
}

fn sha256_file(path: &Path) -> Option<String> {
    fs::read(path).ok().map(|bytes| sha256_hex(&bytes))
}

fn changed_and_untracked_paths(repository_root: &Path) -> Vec<String> {
    let changed = git_stdout(
        repository_root,
        &["diff", "HEAD", "--name-only", "--diff-filter=ACMR"],
    )
    .unwrap_or_default();
    let untracked = git_stdout(
        repository_root,
        &["ls-files", "--others", "--exclude-standard"],
    )
    .unwrap_or_default();
    let mut paths = changed
        .lines()
        .chain(untracked.lines())
        .filter(|path| !path.trim().is_empty())
        .map(str::to_owned)
        .collect::<Vec<_>>();
    paths.sort();
    paths.dedup();
    paths
}

fn readable_changed_file<'a>(
    repository_root: &Path,
    relative: &'a str,
) -> Option<(&'a str, Vec<u8>)> {
    let path = repository_root.join(relative);
    let metadata = fs::symlink_metadata(&path).ok()?;
    if !metadata.file_type().is_file() || metadata.len() > 5_000_000 {
        return None;
    }
    let bytes = fs::read(path).ok()?;
    (!bytes.contains(&0)).then_some((relative, bytes))
}

fn high_confidence_secret_kinds(text: &str) -> Vec<&'static str> {
    fn has_prefixed_token(text: &str, prefix: &str, minimum_suffix: usize) -> bool {
        text.match_indices(prefix).any(|(start, _)| {
            text[start + prefix.len()..]
                .chars()
                .take_while(|character| {
                    character.is_ascii_alphanumeric() || matches!(character, '_' | '-')
                })
                .take(minimum_suffix)
                .count()
                >= minimum_suffix
        })
    }

    let mut kinds = Vec::new();
    let private_key_header = ["-----BEGIN ", "PRIVATE KEY-----"].concat();
    if text.contains(&private_key_header)
        || text.contains(&["-----BEGIN RSA ", "PRIVATE KEY-----"].concat())
        || text.contains(&["-----BEGIN EC ", "PRIVATE KEY-----"].concat())
        || text.contains(&["-----BEGIN OPENSSH ", "PRIVATE KEY-----"].concat())
    {
        kinds.push("private_key_pem");
    }
    let credential_schemes = [
        "postgres://",
        "postgresql://",
        "mysql://",
        "redis://",
        "rediss://",
        "mongodb://",
        "mongodb+srv://",
    ];
    if text.lines().any(|line| {
        let lower = line.to_ascii_lowercase();
        credential_schemes.iter().any(|scheme| {
            lower.find(scheme).is_some_and(|start| {
                lower[start + scheme.len()..]
                    .split_ascii_whitespace()
                    .next()
                    .is_some_and(|authority| authority.contains('@'))
            })
        })
    }) {
        kinds.push("credential_url");
    }
    if has_prefixed_token(text, "ghp_", 30) || has_prefixed_token(text, "github_pat_", 40) {
        kinds.push("github_token");
    }
    if has_prefixed_token(text, "xoxb-", 30) || has_prefixed_token(text, "xoxp-", 30) {
        kinds.push("slack_token");
    }
    if has_prefixed_token(text, "sk_live_", 20) || has_prefixed_token(text, "rk_live_", 20) {
        kinds.push("stripe_live_key");
    }
    if text.match_indices("AKIA").any(|(start, _)| {
        text.as_bytes()
            .get(start + 4..start + 20)
            .is_some_and(|suffix| {
                suffix.len() == 16
                    && suffix
                        .iter()
                        .all(|byte| byte.is_ascii_uppercase() || byte.is_ascii_digit())
            })
    }) {
        kinds.push("aws_access_key_id");
    }
    kinds
}

fn project_production_environment<'a>(render_yaml: &'a str, project_name: &str) -> Option<&'a str> {
    let project_start = render_yaml.find(&format!("  - name: {project_name}"))?;
    let project = &render_yaml[project_start..];
    let production_start = project.find("      - name: production")?;
    let production = &project[production_start..];
    let next_environment = production["      - name: production".len()..]
        .find("      - name:")
        .map(|offset| offset + "      - name: production".len());
    let next_project = production["      - name: production".len()..]
        .find("  - name:")
        .map(|offset| offset + "      - name: production".len());
    let end = next_environment.into_iter().chain(next_project).min();
    Some(end.map_or(production, |end| &production[..end]))
}

fn production_environment(render_yaml: &str) -> Option<&str> {
    project_production_environment(render_yaml, "loyal-yield-light-workers")
}

fn service_blocks(environment: &str) -> Vec<String> {
    let mut blocks = Vec::new();
    let mut current = String::new();
    for line in environment.lines() {
        if line.starts_with("          - type:") && !current.is_empty() {
            blocks.push(std::mem::take(&mut current));
        }
        if !current.is_empty() || line.starts_with("          - type:") {
            current.push_str(line);
            current.push('\n');
        }
    }
    if !current.is_empty() {
        blocks.push(current);
    }
    blocks
}

fn yaml_scalar<'a>(block: &'a str, key: &str) -> Option<&'a str> {
    block.lines().find_map(|line| {
        let trimmed = line.trim();
        let trimmed = trimmed.strip_prefix("- ").unwrap_or(trimmed);
        let value = trimmed.strip_prefix(key)?.strip_prefix(':')?.trim();
        if value.is_empty() {
            return None;
        }
        let bytes = value.as_bytes();
        let quoted = bytes.len() >= 2
            && matches!(bytes.first(), Some(b'\'' | b'"'))
            && bytes.first() == bytes.last();
        Some(if quoted {
            &value[1..value.len() - 1]
        } else {
            value
        })
    })
}

fn command_has_prefix(command: &str, expected_prefix: &str) -> bool {
    command == expected_prefix
        || command
            .strip_prefix(expected_prefix)
            .is_some_and(|suffix| suffix.starts_with(char::is_whitespace))
}

fn command_has_flag_value(command: &str, flag: &str, expected_value: &str) -> bool {
    let tokens = command.split_whitespace().collect::<Vec<_>>();
    tokens
        .windows(2)
        .any(|pair| pair[0] == flag && pair[1] == expected_value)
}

fn service_env_keys(block: &str) -> BTreeSet<&str> {
    block
        .lines()
        .filter_map(|line| line.trim().strip_prefix("- key:").map(str::trim))
        .filter(|key| !key.is_empty())
        .collect()
}

fn immutable_light_worker_sha(block: &str) -> Option<String> {
    let marker = "ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-";
    let image_url = yaml_scalar(block, "url")?;
    let start = image_url.strip_prefix(marker)?;
    let sha = start
        .chars()
        .take_while(|character| character.is_ascii_hexdigit())
        .collect::<String>();
    (sha.len() == 40 && start.len() == 40).then_some(sha)
}

fn immutable_light_worker_reference(block: &str) -> Option<String> {
    immutable_light_worker_sha(block)?;
    yaml_scalar(block, "url").map(ToOwned::to_owned)
}

fn collect_local_evidence(repository_root: &Path) -> Result<LocalEvidence, Box<dyn Error>> {
    let head_commit = git_stdout(repository_root, &["rev-parse", "HEAD"]);
    let runtime_source_digest_sha256 =
        RuntimeSourceBinding::capture(repository_root)?.runtime_source_digest_sha256;
    let mut repository_subchecks = vec![
        run_repository_command(
            repository_root,
            "git_diff_check",
            "git",
            &["diff", "HEAD", "--check"],
        ),
        run_repository_command(
            repository_root,
            "cargo_fmt_check",
            "cargo",
            &["fmt", "--all", "--", "--check"],
        ),
        run_repository_command(
            repository_root,
            "orchestrator_all_bins_compile",
            "cargo",
            &["check", "-p", "loyal-yield-orchestrator", "--bins"],
        ),
    ];

    let intended_paths = changed_and_untracked_paths(repository_root);
    let forbidden_env_files = intended_paths
        .iter()
        .map(String::as_str)
        .filter(|path| {
            Path::new(path)
                .file_name()
                .and_then(|name| name.to_str())
                .is_some_and(|name| {
                    (name == ".env" || name.starts_with(".env.")) && name != ".env.example"
                })
        })
        .collect::<Vec<_>>();
    repository_subchecks.push(subcheck(
        "no_plaintext_environment_file_added",
        forbidden_env_files.is_empty(),
        json!({"forbiddenPaths": forbidden_env_files}),
    ));
    let mut untracked_whitespace_findings = Vec::new();
    let mut secret_findings = Vec::new();
    for relative in &intended_paths {
        let Some((path, bytes)) = readable_changed_file(repository_root, relative) else {
            continue;
        };
        let trailing_whitespace_lines = bytes
            .split(|byte| *byte == b'\n')
            .filter(|line| {
                let line = line.strip_suffix(b"\r").unwrap_or(line);
                line.last().is_some_and(|byte| matches!(byte, b' ' | b'\t'))
            })
            .count();
        if trailing_whitespace_lines > 0 {
            untracked_whitespace_findings.push(json!({
                "path": path,
                "trailingWhitespaceLineCount": trailing_whitespace_lines,
            }));
        }
        let text = String::from_utf8_lossy(&bytes);
        let kinds = high_confidence_secret_kinds(&text);
        if !kinds.is_empty() {
            secret_findings.push(json!({
                "path": path,
                "findingCount": kinds.len(),
                "kinds": kinds,
            }));
        }
    }
    repository_subchecks.push(subcheck(
        "changed_and_untracked_intended_files_have_no_trailing_whitespace",
        untracked_whitespace_findings.is_empty(),
        json!({
            "scannedChangedOrUntrackedPathCount": intended_paths.len(),
            "findings": untracked_whitespace_findings,
        }),
    ));
    repository_subchecks.push(subcheck(
        "changed_files_have_no_high_confidence_plaintext_secrets",
        secret_findings.is_empty(),
        json!({
            "scannedChangedOrUntrackedPathCount": intended_paths.len(),
            "findings": secret_findings,
            "secretValuesReported": false,
        }),
    ));

    let mut migration_files = Vec::with_capacity(VERIFIED_MIGRATIONS.len());
    for (version, name, file_name) in VERIFIED_MIGRATIONS {
        let bytes = fs::read(
            repository_root
                .join("crates/loyal-yield-store/migrations")
                .join(file_name),
        )?;
        migration_files.push((version, name, file_name, bytes));
    }
    repository_subchecks.push(subcheck(
        "migration_repository_files_present",
        migration_files
            .iter()
            .all(|(_, _, _, bytes)| !bytes.is_empty()),
        json!({"migrations": migration_files.iter().map(|(version, name, file_name, bytes)| json!({
            "version": version,
            "name": name,
            "file": file_name,
            "sha256": sha256_hex(bytes),
        })).collect::<Vec<_>>() }),
    ));
    let dockerfile = fs::read_to_string(repository_root.join("Dockerfile.light-workers"))?;
    let durable_binaries = [
        "fleet-opportunity-planner",
        "same-mint-reserve-swap",
        "fleet-route-confirmer",
        "route-lookup-table-provisioner",
    ];
    let missing_image_binaries = durable_binaries
        .iter()
        .filter(|binary| {
            dockerfile
                .matches(&format!("/usr/local/bin/{binary}"))
                .count()
                < 2
        })
        .copied()
        .collect::<Vec<_>>();
    let mut wiring_subchecks = vec![subcheck(
        "light_worker_recipe_copies_durable_binaries_to_final_stage",
        missing_image_binaries.is_empty(),
        json!({
            "missingBinaries": missing_image_binaries,
            "proofScope": "Dockerfile recipe only; built-image probing is separate evidence"
        }),
    )];
    let heavy_dockerfile =
        fs::read_to_string(repository_root.join("Dockerfile.laserstream-workers"))?;
    let monitor_predeploy =
        fs::read_to_string(repository_root.join("scripts/kamino-monitor-predeploy.sh"))?;
    let monitor_predeploy_lines = monitor_predeploy
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .collect::<Vec<_>>();
    let expected_monitor_predeploy_lines = [
        "#!/bin/sh",
        "set -eu",
        "/usr/local/bin/loyal-timescale-migrations --apply",
        "exec /usr/local/bin/kamino-reserve-monitor --sync-supported-reserves",
    ];
    let monitor_wrapper_copy =
        format!("COPY --chmod=0755 scripts/kamino-monitor-predeploy.sh {KAMINO_MONITOR_PREDEPLOY}");
    wiring_subchecks.push(subcheck(
        "kamino_monitor_predeploy_is_one_image_executable_with_ordered_migration_and_sync",
        monitor_predeploy_lines == expected_monitor_predeploy_lines
            && heavy_dockerfile
                .lines()
                .any(|line| line.trim() == monitor_wrapper_copy),
        json!({
            "wrapperSource": "scripts/kamino-monitor-predeploy.sh",
            "imageCommand": KAMINO_MONITOR_PREDEPLOY,
            "imageCopyIsExecutable": heavy_dockerfile
                .lines()
                .any(|line| line.trim() == monitor_wrapper_copy),
            "migrationRunsBeforeSync": monitor_predeploy_lines == expected_monitor_predeploy_lines,
            "removalOverridePresent": monitor_predeploy.contains("--allow-supported-reserve-removals"),
        }),
    ));

    let workflow = fs::read_to_string(repository_root.join(".github/workflows/worker-images.yml"))?;
    wiring_subchecks.push(subcheck(
        "worker_image_workflow_uses_immutable_commit_tags",
        workflow.contains("dockerfile: Dockerfile.light-workers")
            && workflow.contains("dockerfile: Dockerfile.laserstream-workers")
            && workflow.contains(":sha-${{ github.sha }}"),
        json!({
            "workflow": ".github/workflows/worker-images.yml",
            "workflowSha256": sha256_hex(workflow.as_bytes()),
        }),
    ));
    let role_probe_contract = FleetWorkerRole::ALL.iter().all(|role| {
        let probe = fleet_worker_role_probe(*role);
        probe.get("status").and_then(Value::as_str) == Some("pass")
            && probe.get("role").and_then(Value::as_str) == Some(role.as_str())
            && probe.get("owningBinary").and_then(Value::as_str) == Some(role.owning_binary())
            && probe.get("networkAccessed").and_then(Value::as_bool) == Some(false)
            && probe.get("secretsLoaded").and_then(Value::as_bool) == Some(false)
            && probe.get("databaseMutated").and_then(Value::as_bool) == Some(false)
            && probe.get("transactionSent").and_then(Value::as_bool) == Some(false)
    });
    wiring_subchecks.push(subcheck(
        "six_role_probe_contract_is_exact_and_side_effect_free",
        role_probe_contract,
        json!({
            "roles": FleetWorkerRole::ALL.iter().map(|role| role.as_str()).collect::<Vec<_>>(),
            "probeArgument": "--role-probe",
            "networkAccessed": false,
            "secretsLoaded": false,
            "databaseMutated": false,
            "transactionSent": false,
        }),
    ));
    let stuck_fixture = functional_stuck_stage_fixture();
    wiring_subchecks.push(subcheck(
        "functional_status_fixture_detects_every_stuck_stage_within_one_health_observation",
        stuck_fixture.passed,
        serde_json::to_value(&stuck_fixture)?,
    ));
    let resilience_fixture = functional_worker_resilience_fixture();
    wiring_subchecks.push(subcheck(
        "functional_listener_reconnect_and_outer_task_fenced_recovery_fixture",
        resilience_fixture.passed,
        serde_json::to_value(&resilience_fixture)?,
    ));
    let material_frontier_fixture = material_frontier_deterministic_evidence();
    let material_frontier_fixture_passed = material_frontier_fixture
        .exact_epoch_changed_under_harmless_churn
        && material_frontier_fixture.harmless_churn_disposition
            == MaterialFrontierDisposition::ReuseScopedFrontier
        && material_frontier_fixture.material_apy_change_disposition
            == MaterialFrontierDisposition::FullSweepSupplyApyChanged
        && material_frontier_fixture.material_capacity_change_disposition
            == MaterialFrontierDisposition::FullSweepTargetCapacityChanged
        && material_frontier_fixture.material_topology_change_disposition
            == MaterialFrontierDisposition::FullSweepReserveTopologyChanged
        && material_frontier_fixture
            .material_apy_change_disposition
            .allows_current_route_revalidation()
        && material_frontier_fixture
            .material_capacity_change_disposition
            .allows_current_route_revalidation()
        && !material_frontier_fixture
            .material_topology_change_disposition
            .allows_current_route_revalidation()
        && material_frontier_fixture
            .material_topology_change_disposition
            .requires_current_route_topology_convergence()
        && !MaterialFrontierDisposition::FullSweepMarketPriceChanged
            .allows_current_route_revalidation()
        && !MaterialFrontierDisposition::FullSweepMarketPriceChanged
            .requires_current_route_topology_convergence();
    wiring_subchecks.push(subcheck(
        "functional_market_frontier_replans_material_change_without_starving_fresh_route_economics",
        material_frontier_fixture_passed,
        serde_json::to_value(&material_frontier_fixture)?,
    ));

    let render_yaml = fs::read_to_string(repository_root.join("render.yaml"))?;
    let production = production_environment(&render_yaml).unwrap_or_default();
    let blocks = service_blocks(production);
    let required_services = FleetWorkerRole::ALL.map(|role| (role.as_str(), role.command_prefix()));
    let mut matched_block_indexes = Vec::new();
    let mut matched_services = Vec::new();
    let mut missing_or_duplicate_roles = Vec::new();
    let mut durable_image_shas = Vec::new();
    let mut durable_image_references = Vec::new();
    let mut invalid_service_configuration = Vec::new();
    for (role, command_prefix) in required_services {
        let matching = blocks
            .iter()
            .enumerate()
            .filter(|(_, block)| {
                yaml_scalar(block, "type") == Some("worker")
                    && yaml_scalar(block, "dockerCommand").is_some_and(|command| {
                        command_has_prefix(command, command_prefix)
                            && (role != "priority_provisioner"
                                || (command.split_whitespace().any(|token| token == "--execute")
                                    && command.split_whitespace().any(|token| token == "--watch")))
                    })
            })
            .collect::<Vec<_>>();
        if matching.len() != 1 {
            missing_or_duplicate_roles.push(json!({
                "role": role,
                "commandPrefix": command_prefix,
                "matchingServiceCount": matching.len(),
            }));
            continue;
        }
        let (block_index, block) = matching[0];
        matched_block_indexes.push(block_index);
        let service_name = yaml_scalar(block, "name").unwrap_or("<missing-name>");
        let env_keys = service_env_keys(block);
        let (required_env, forbidden_env, required_command_flags): (
            &[&str],
            &[&str],
            &[(&str, &str)],
        ) = match role {
            "planner" => (
                &["NEON_DATABASE_URL", "TIMESCALEDB_URL", "YIELD_ALT_CLUSTER"],
                &[
                    "SOLANA_RPC_URL",
                    "POLICY_KEYPAIR",
                    "YIELD_ROUTE_FEE_PAYER_KEYPAIRS",
                    "SOLANA_TESTING_PK",
                    "YIELD_ROUTER_KEYPAIR",
                ],
                &[
                    ("--poll-interval-seconds", "1"),
                    ("--full-sweep-interval-seconds", "30"),
                    ("--dirty-batch-size", "256"),
                    ("--max-opportunities-per-wave", "128"),
                ],
            ),
            "revalidator" => (
                &[
                    "NEON_DATABASE_URL",
                    "TIMESCALEDB_URL",
                    "SOLANA_RPC_URL",
                    "YIELD_ALT_CLUSTER",
                    "POLICY_KEYPAIR",
                    "YIELD_ROUTE_FEE_PAYER_KEYPAIRS",
                ],
                &["SOLANA_TESTING_PK", "YIELD_ROUTER_KEYPAIR"],
                &[
                    ("--concurrency", "16"),
                    ("--fused-execute-concurrency", "8"),
                    ("--poll-interval-milliseconds", "250"),
                ],
            ),
            "executor" => (
                &[
                    "NEON_DATABASE_URL",
                    "TIMESCALEDB_URL",
                    "SOLANA_RPC_URL",
                    "YIELD_ALT_CLUSTER",
                    "POLICY_KEYPAIR",
                    "YIELD_ROUTE_FEE_PAYER_KEYPAIRS",
                ],
                &["SOLANA_TESTING_PK", "YIELD_ROUTER_KEYPAIR"],
                &[
                    ("--concurrency", "4"),
                    ("--poll-interval-milliseconds", "250"),
                ],
            ),
            "confirmer" => (
                &["NEON_DATABASE_URL", "SOLANA_RPC_URL", "YIELD_ALT_CLUSTER"],
                &[
                    "TIMESCALEDB_URL",
                    "POLICY_KEYPAIR",
                    "YIELD_ROUTE_FEE_PAYER_KEYPAIRS",
                    "SOLANA_TESTING_PK",
                    "YIELD_ROUTER_KEYPAIR",
                ],
                &[
                    ("--batch-size", "128"),
                    ("--broadcast-concurrency", "16"),
                    ("--poll-interval-milliseconds", "1000"),
                ],
            ),
            "reconciler" => (
                &["NEON_DATABASE_URL", "SOLANA_RPC_URL", "YIELD_ALT_CLUSTER"],
                &[
                    "TIMESCALEDB_URL",
                    "POLICY_KEYPAIR",
                    "YIELD_ROUTE_FEE_PAYER_KEYPAIRS",
                    "SOLANA_TESTING_PK",
                    "YIELD_ROUTER_KEYPAIR",
                ],
                &[
                    ("--concurrency", "64"),
                    ("--batch-size", "32"),
                    ("--poll-interval-milliseconds", "250"),
                    ("--position-sweep-interval-seconds", "300"),
                ],
            ),
            "priority_provisioner" => (
                &[
                    "NEON_DATABASE_URL",
                    "SOLANA_RPC_URL",
                    "YIELD_ALT_CLUSTER",
                    "POLICY_KEYPAIR",
                    "YIELD_ALT_MAX_LAMPORTS",
                    "YIELD_ALT_BUDGET_WINDOW_SECONDS",
                ],
                &[
                    "TIMESCALEDB_URL",
                    "YIELD_ROUTE_FEE_PAYER_KEYPAIRS",
                    "SOLANA_TESTING_PK",
                    "YIELD_ROUTER_KEYPAIR",
                ],
                &[
                    ("--max-operations", "32"),
                    ("--concurrency", "8"),
                    ("--rate-limit-ms", "250"),
                ],
            ),
            _ => (&[], &[], &[]),
        };
        let missing_env = required_env
            .iter()
            .filter(|key| !env_keys.contains(**key))
            .copied()
            .collect::<Vec<_>>();
        let mounted_forbidden_env = forbidden_env
            .iter()
            .filter(|key| env_keys.contains(**key))
            .copied()
            .collect::<Vec<_>>();
        let command = yaml_scalar(block, "dockerCommand").unwrap_or_default();
        let missing_command_flags = required_command_flags
            .iter()
            .filter(|(flag, value)| !command_has_flag_value(command, flag, value))
            .map(|(flag, value)| format!("{flag} {value}"))
            .collect::<Vec<_>>();
        matched_services.push(json!({
            "role": role,
            "serviceName": service_name,
            "plan": yaml_scalar(block, "plan"),
            "command": command,
            "envKeys": env_keys,
        }));
        if !missing_env.is_empty()
            || !mounted_forbidden_env.is_empty()
            || !missing_command_flags.is_empty()
        {
            invalid_service_configuration.push(json!({
                "role": role,
                "serviceName": service_name,
                "reason": "durable role env or explicit feedback-loop configuration is invalid",
                "missingEnv": missing_env,
                "mountedForbiddenEnv": mounted_forbidden_env,
                "missingCommandFlags": missing_command_flags,
            }));
        }
        if let Some(reference) = immutable_light_worker_reference(block) {
            durable_image_shas.push(
                immutable_light_worker_sha(block)
                    .expect("validated immutable light-worker reference has a commit SHA"),
            );
            durable_image_references.push(reference);
        } else {
            invalid_service_configuration.push(json!({
                "role": role,
                "serviceName": service_name,
                "reason": "missing immutable light-worker sha image reference",
            }));
        }
        if !yaml_scalar(block, "preDeployCommand")
            .is_some_and(|command| command.contains("/usr/local/bin/yield-migrations --apply"))
        {
            invalid_service_configuration.push(json!({
                "role": role,
                "serviceName": service_name,
                "reason": "missing Yield migration apply command",
            }));
        }
    }
    matched_block_indexes.sort_unstable();
    let distinct_service_blocks = matched_block_indexes
        .windows(2)
        .all(|pair| pair[0] != pair[1]);
    durable_image_shas.sort();
    durable_image_shas.dedup();
    let validated_image_service_count = durable_image_references.len();
    durable_image_references.sort();
    durable_image_references.dedup();
    let production_light_worker_image_reference = (durable_image_references.len() == 1
        && validated_image_service_count == required_services.len()
        && matched_block_indexes.len() == required_services.len())
    .then(|| durable_image_references[0].clone());
    let expected_kamino_monitor = production_expected_kamino_monitor(repository_root);
    let production_heavy_worker_image_reference = expected_kamino_monitor
        .as_ref()
        .ok()
        .map(|service| service.image.clone());
    wiring_subchecks.push(subcheck(
        "production_blueprint_uses_exact_kamino_monitor_predeploy_executable",
        expected_kamino_monitor.as_ref().is_ok_and(|service| {
            service.command == KAMINO_MONITOR_COMMAND
                && service.pre_deploy_command == KAMINO_MONITOR_PREDEPLOY
        }),
        json!({
            "service": KAMINO_MONITOR_SERVICE_NAME,
            "expectedCommand": KAMINO_MONITOR_COMMAND,
            "expectedPreDeployCommand": KAMINO_MONITOR_PREDEPLOY,
            "configuredCommand": expected_kamino_monitor.as_ref().ok().map(|service| service.command.as_str()),
            "configuredPreDeployCommand": expected_kamino_monitor.as_ref().ok().map(|service| service.pre_deploy_command.as_str()),
            "configurationError": expected_kamino_monitor.as_ref().err().map(|error| error.to_string()),
        }),
    ));
    wiring_subchecks.push(subcheck(
        "production_blueprint_declares_six_distinct_durable_worker_roles",
        missing_or_duplicate_roles.is_empty()
            && invalid_service_configuration.is_empty()
            && distinct_service_blocks
            && matched_block_indexes.len() == required_services.len(),
        json!({
            "requiredRoles": required_services.iter().map(|(role, _)| role).collect::<Vec<_>>(),
            "matchedServices": matched_services,
            "missingOrDuplicateRoles": missing_or_duplicate_roles,
            "invalidServiceConfiguration": invalid_service_configuration,
            "distinctServiceBlocks": distinct_service_blocks,
        }),
    ));
    let serial_execution_commands = blocks
        .iter()
        .filter(|block| {
            yaml_scalar(block, "dockerCommand").is_some_and(|command| {
                command_has_prefix(command, "/usr/local/bin/same-mint-yield-monitor")
                    && command.split_whitespace().any(|token| token == "--execute")
            })
        })
        .count();
    wiring_subchecks.push(subcheck(
        "production_serial_fleet_execution_removed",
        serial_execution_commands == 0,
        json!({"serialExecuteServiceCount": serial_execution_commands}),
    ));
    wiring_subchecks.push(subcheck(
        "configured_durable_workers_share_one_immutable_candidate_image",
        durable_image_shas.len() == 1
            && matched_block_indexes.len() == required_services.len()
            && invalid_service_configuration.is_empty(),
        json!({
            "imageCommitShas": durable_image_shas,
            "imageReferences": durable_image_references,
            "validatedImageServiceCount": validated_image_service_count,
            "checkoutHead": head_commit.clone(),
            "proofScope": "local Blueprint declaration only; registry presence, image contents, and live Render state are deployment evidence",
        }),
    ));
    Ok(LocalEvidence {
        repository_subchecks,
        wiring_subchecks,
        repository_root: repository_root.to_path_buf(),
        head_commit,
        runtime_source_digest_sha256,
        production_light_worker_image_reference,
        production_heavy_worker_image_reference,
    })
}

fn load_runtime_evidence(
    path: &Path,
    local: &LocalEvidence,
) -> Result<RuntimeEvidenceV1, Box<dyn Error>> {
    let bytes = fs::read(path)?;
    let evidence: RuntimeEvidenceV1 = serde_json::from_slice(&bytes)?;
    if evidence.schema_version != 1 {
        return Err(format!(
            "runtime evidence schemaVersion must be 1, got {}",
            evidence.schema_version
        )
        .into());
    }
    let head_commit = local
        .head_commit
        .as_deref()
        .ok_or("runtime evidence requires a readable checkout HEAD")?;
    if evidence.head_commit != head_commit {
        return Err("runtime evidence HEAD does not match the inspected checkout".into());
    }
    if evidence.runtime_source_digest_sha256 != local.runtime_source_digest_sha256 {
        return Err(
            "runtime evidence source digest does not match the inspected runtime inputs".into(),
        );
    }
    if evidence.hardware.trim().is_empty() {
        return Err("runtime evidence hardware description must be nonempty".into());
    }
    let now = Utc::now();
    if evidence.captured_at < now - chrono::Duration::hours(1)
        || evidence.captured_at > now + chrono::Duration::minutes(5)
    {
        return Err("runtime evidence must have been captured within the last hour".into());
    }
    Ok(evidence)
}

fn runtime_discovery_subcheck(evidence: &RuntimeEvidenceV1) -> Subcheck {
    let active_exclusion_count = evidence
        .discovery
        .active_exclusions_by_state
        .values()
        .copied()
        .sum::<u64>();
    let outcome_count = evidence
        .discovery
        .vault_outcomes_by_reason
        .values()
        .copied()
        .sum::<u64>();
    let active_outcome_count = evidence
        .discovery
        .vault_outcomes_by_reason
        .iter()
        .filter(|(reason, _)| {
            reason.as_str() == "active_decision" || reason.starts_with("active_queue_")
        })
        .map(|(_, count)| *count)
        .sum::<u64>();
    let epoch_proofs_are_complete = evidence.discovery.planning_sample_epoch_proofs.len()
        == usize::try_from(evidence.discovery.planning_sample_count).unwrap_or(usize::MAX)
        && evidence
            .discovery
            .planning_sample_epoch_proofs
            .iter()
            .all(|proof| {
                proof.market_epoch_optimizer_id > 0
                    && proof.observed_opportunity_epoch_ids.len() <= 1
                    && proof
                        .observed_opportunity_epoch_ids
                        .iter()
                        .all(|epoch_id| *epoch_id == proof.market_epoch_optimizer_id)
                    && proof.selected_opportunity_epoch_ids.len() <= 1
                    && proof
                        .selected_opportunity_epoch_ids
                        .iter()
                        .all(|epoch_id| *epoch_id == proof.market_epoch_optimizer_id)
            });
    let final_epoch_proof_matches = evidence
        .discovery
        .planning_sample_epoch_proofs
        .last()
        .is_some_and(|proof| {
            proof.market_epoch_optimizer_id == evidence.discovery.optimizer_epoch_id
                && (evidence
                    .discovery
                    .vault_outcomes_by_reason
                    .get("opportunity_observed")
                    .copied()
                    .unwrap_or_default()
                    == 0
                    || !proof.observed_opportunity_epoch_ids.is_empty())
        });
    let passed = evidence.discovery.fleet_size > 0
        && evidence.discovery.eligible_current_vaults > 0
        && evidence.discovery.fleet_size >= evidence.discovery.eligible_current_vaults
        && evidence.discovery.accounted_vaults == evidence.discovery.eligible_current_vaults
        && outcome_count == evidence.discovery.eligible_current_vaults
        && active_exclusion_count == active_outcome_count
        && evidence.discovery.optimizer_epoch_id > 0
        && evidence.discovery.one_immutable_epoch
        && epoch_proofs_are_complete
        && final_epoch_proof_matches
        && evidence.discovery.epoch_expires_at > evidence.captured_at
        && evidence.discovery.planning_sample_count > 0
        && evidence.discovery.planning_p95_milliseconds < 5_000
        && evidence.discovery.replay_vault_count == 10_000
        && evidence.discovery.replay_milliseconds < 10_000
        && evidence.discovery.economically_ordered
        && evidence
            .discovery
            .top_cohort_has_no_nonconflicting_priority_inversion
        && evidence
            .discovery
            .child_route_or_reconcile_processes_spawned
            == 0;
    subcheck(
        "bound_current_fleet_discovery_evidence_meets_completeness_and_latency_gates",
        passed,
        serde_json::to_value(&evidence.discovery).unwrap_or_else(|_| json!({})),
    )
}

fn runtime_alt_subcheck(evidence: &RuntimeEvidenceV1) -> Subcheck {
    let baseline = evidence.alt.ready_claim_baseline_p95_micros;
    let cold_effect_ppm = evidence
        .alt
        .ready_claim_cold_p95_micros
        .saturating_sub(baseline)
        .saturating_mul(1_000_000)
        / baseline.max(1);
    let passed = evidence.alt.typed_provisioner_dry_run_plans > 0
        && evidence.alt.reusable_v2_plans == evidence.alt.typed_provisioner_dry_run_plans
        && evidence.alt.legacy_or_exact_route_alt_plans == 0
        && evidence.alt.ready_jobs_seeded > 0
        && evidence.alt.ready_jobs_claimed == evidence.alt.ready_jobs_seeded
        && evidence.alt.waiting_alt_jobs >= 10_000
        && evidence.alt.waiting_alt_decisions == 0
        && evidence.alt.claim_latency_gate_clock == "postgres_statement_elapsed"
        && baseline > 0
        && cold_effect_ppm < 50_000
        && evidence.alt.ready_claim_baseline_client_p95_micros > 0
        && evidence.alt.ready_claim_cold_client_p95_micros > 0
        && evidence.alt.affected_jobs_promoted > 0
        && evidence.alt.durable_coverage_wakeup_rows >= evidence.alt.affected_jobs_promoted
        && evidence.alt.unaffected_jobs_promoted == 0
        && !evidence.alt.additional_fleet_cycle_required
        && evidence
            .alt
            .normal_readiness_global_rollout_lock_acquisitions
            == 0
        && evidence.alt.independent_physical_alt_lanes_progressed >= 2
        && evidence.alt.same_table_predecessor_violations == 0
        && evidence.alt.stale_fence_commits == 0
        && evidence.alt.usage_leases_rejected_during_mutation == 1
        && evidence.alt.mutating_operations_leased_during_usage == 0
        && evidence.alt.verify_operations_leased_during_usage == 1
        && evidence.alt.usage_fence_broadcast_commits == 0
        && evidence.alt.usage_fence_broadcast_rejections == 1;
    subcheck(
        "bound_alt_runtime_evidence_meets_head_of_line_wakeup_and_mutation_gates",
        passed,
        json!({
            "evidence": &evidence.alt,
            "coldBacklogEffectPpm": cold_effect_ppm,
            "limitPpm": 50_000,
        }),
    )
}

fn runtime_execution_subcheck(evidence: &RuntimeEvidenceV1) -> Subcheck {
    let execution = &evidence.execution;
    let passed = execution.duplicate_active_vault_movements == 0
        && execution.nonoverlapping_concurrent_leases >= 2
        && execution.overlapping_lane_limit_violations == 0
        && execution.physical_writable_key_congestion_visible
        && execution.expired_lease_reclaimed_with_higher_fence
        && execution.mixed_runnable_and_expired_claims_full_and_disjoint
        && execution.fleet_wide_exclusive_route_leases == 0
        && execution.identical_byte_rebroadcast_attempts >= 2
        && execution.rebroadcast_byte_mismatches == 0
        && execution.replacement_before_expiry_and_absence_proof == 0
        && execution.ambiguous_or_stale_replacement_movements == 0
        && execution.post_confirm_reads > 0
        && execution.min_context_slot_violations == 0
        && execution.policy_execution_signed_by_policy_keypair
        && execution.alt_mutations_authorized_and_paid_by_policy_keypair
        && execution.sharded_route_fixtures > 0
        && execution.shard_is_final_fee_payer
        && execution.policy_is_second_static_signer
        && execution.final_manifest_and_alt_coverage_match
        && execution.final_packet_simulation_fee_and_hashes_match
        && execution.setup_idle_and_farm_init_use_policy_payer
        && execution.shard_registry_keypair_match
        && execution.reciprocal_authority_separation
        && execution.bounded_ranked_failover
        && execution.low_balance_limits_enforced
        && execution.atomic_immutable_spend_reservation
        && execution.target_capacity_concurrent_admission_bounded
        && execution.pre_send_target_capacity_released
        && execution.reconciled_capacity_strict_telemetry_fence
        && execution.preexisting_newer_telemetry_release
        && execution.readiness_writers_waited_on_per_vault_fence
        && execution.readiness_parent_before_physical_lock_order_proved
        && execution.serialized_readiness_row_count == 2;
    subcheck(
        "bound_controlled_rpc_evidence_meets_replay_signer_and_reconciliation_gates",
        passed,
        serde_json::to_value(execution).unwrap_or_else(|_| json!({})),
    )
}

fn runtime_source_evidence_contract_subcheck(
    fixtures: &RuntimeSourceEvidenceContractFixtures,
) -> Subcheck {
    const CONTAMINATION_ERROR: &str =
        "same-mint reserve-position request cannot carry idle-vault evidence";
    const ROUTE_KIND_MISMATCH_ERROR: &str = "fleet execution_plan.kind \"idle_vault_deposit\" does not match source_kind \"reserve_position\"; expected \"same_mint\"";
    let reserve = &fixtures.reserve_position;
    let idle = &fixtures.idle_vault_usdc;
    let contaminated = &fixtures.contaminated_reserve;
    let mismatched = &fixtures.mismatched_route_kind;

    let plan_string = |fixture: &RuntimeSourceEvidenceContractFixture, field: &str| {
        fixture
            .execution_plan
            .get(field)
            .and_then(Value::as_str)
            .filter(|value| !value.trim().is_empty())
            .map(ToOwned::to_owned)
    };
    let plan_i64 = |fixture: &RuntimeSourceEvidenceContractFixture, field: &str| {
        fixture.execution_plan.get(field).and_then(Value::as_i64)
    };
    let plan_datetime = |fixture: &RuntimeSourceEvidenceContractFixture, field: &str| {
        plan_string(fixture, field)
            .and_then(|value| DateTime::parse_from_rfc3339(&value).ok())
            .map(|value| value.with_timezone(&Utc))
    };

    let reserve_passed = reserve.source_kind == "reserve_position"
        && plan_string(reserve, "kind").as_deref() == Some("same_mint")
        && plan_string(reserve, "source_kind").as_deref() == Some("reserve_position")
        && reserve
            .source_reserve
            .as_deref()
            .is_some_and(|value| !value.trim().is_empty())
        && reserve.source_snapshot_id.is_some_and(|id| id > 0)
        && plan_i64(reserve, "source_observed_slot").is_some_and(|slot| slot > 0)
        && plan_datetime(reserve, "source_observed_at").is_some()
        && plan_string(reserve, "idle_token_account").is_none()
        && reserve
            .projected_evidence
            .expected_idle_token_account
            .is_none()
        && reserve
            .projected_evidence
            .expected_idle_observed_slot
            .is_none()
        && reserve
            .projected_evidence
            .expected_idle_observed_at
            .is_none()
        && reserve.validation_error.is_none();

    let idle_plan_account = plan_string(idle, "idle_token_account");
    let idle_plan_slot = plan_i64(idle, "source_observed_slot");
    let idle_plan_at = plan_datetime(idle, "source_observed_at");
    let idle_passed = idle.source_kind == "idle_vault_usdc"
        && plan_string(idle, "kind").as_deref() == Some("idle_vault_deposit")
        && plan_string(idle, "source_kind").as_deref() == Some("idle_vault_usdc")
        && idle.source_reserve.is_none()
        && idle.source_snapshot_id.is_none()
        && idle_plan_account.is_some()
        && idle_plan_slot.is_some_and(|slot| slot > 0)
        && idle_plan_at.is_some()
        && idle
            .projected_evidence
            .expected_idle_token_account
            .as_deref()
            == idle_plan_account.as_deref()
        && idle.projected_evidence.expected_idle_observed_slot == idle_plan_slot
        && idle.projected_evidence.expected_idle_observed_at == idle_plan_at
        && idle.validation_error.is_none();

    let contaminated_plan_account = plan_string(contaminated, "idle_token_account");
    let contaminated_passed = contaminated.source_kind == "reserve_position"
        && plan_string(contaminated, "kind").as_deref() == Some("same_mint")
        && plan_string(contaminated, "source_kind").as_deref() == Some("reserve_position")
        && contaminated
            .source_reserve
            .as_deref()
            .is_some_and(|value| !value.trim().is_empty())
        && contaminated.source_snapshot_id.is_some_and(|id| id > 0)
        && plan_i64(contaminated, "source_observed_slot").is_some_and(|slot| slot > 0)
        && plan_datetime(contaminated, "source_observed_at").is_some()
        && contaminated_plan_account.is_some()
        && contaminated
            .projected_evidence
            .expected_idle_token_account
            .as_deref()
            == contaminated_plan_account.as_deref()
        && contaminated
            .projected_evidence
            .expected_idle_observed_slot
            .is_none()
        && contaminated
            .projected_evidence
            .expected_idle_observed_at
            .is_none()
        && contaminated.validation_error.as_deref() == Some(CONTAMINATION_ERROR);

    let mismatched_passed = mismatched.source_kind == "reserve_position"
        && plan_string(mismatched, "kind").as_deref() == Some("idle_vault_deposit")
        && plan_string(mismatched, "source_kind").as_deref() == Some("reserve_position")
        && mismatched
            .source_reserve
            .as_deref()
            .is_some_and(|value| !value.trim().is_empty())
        && mismatched.source_snapshot_id.is_some_and(|id| id > 0)
        && plan_string(mismatched, "idle_token_account").is_none()
        && mismatched
            .projected_evidence
            .expected_idle_token_account
            .is_none()
        && mismatched
            .projected_evidence
            .expected_idle_observed_slot
            .is_none()
        && mismatched
            .projected_evidence
            .expected_idle_observed_at
            .is_none()
        && mismatched.validation_error.as_deref() == Some(ROUTE_KIND_MISMATCH_ERROR);

    subcheck(
        "planner_executor_source_evidence_is_kind_scoped",
        reserve_passed && idle_passed && contaminated_passed && mismatched_passed,
        json!({
            "fixtures": fixtures,
            "reserveContractRecomputed": reserve_passed,
            "idleContractRecomputed": idle_passed,
            "crossKindContaminationRejected": contaminated_passed,
            "routeKindSourceKindMismatchRejected": mismatched_passed,
        }),
    )
}

fn runtime_replay_subcheck(evidence: &RuntimeEvidenceV1) -> Subcheck {
    let replay = &evidence.replay;
    let alt_effect_ppm = replay
        .warm_with_alt_backlog_p95_milliseconds
        .saturating_sub(replay.warm_baseline_p95_milliseconds)
        .saturating_mul(1_000_000)
        / replay.warm_baseline_p95_milliseconds.max(1);
    let passed = replay.route_sample_count > 0
        && replay.warm_high_value_submission_p95_milliseconds < 10_000
        && replay.warm_confirmation_p95_milliseconds < 30_000
        && replay.warm_baseline_p95_milliseconds > 0
        && alt_effect_ppm < 50_000
        && replay.recoverable_yield_usd_micros_per_hour > 0
        && replay.submitted_within_two_minutes_yield_ppm >= 900_000
        && replay.submitted_within_ten_minutes_yield_ppm >= 990_000
        && replay.configured_max_fee_fraction_ppm > 0
        && replay.observed_max_fee_fraction_ppm <= replay.configured_max_fee_fraction_ppm
        && replay.negative_value_routes == 0
        && replay.database_deadlocks == 0
        && replay.duplicate_movements == 0;
    subcheck(
        "bound_production_like_replay_meets_latency_value_and_price_gates",
        passed,
        json!({
            "evidence": replay,
            "altBacklogEffectPpm": alt_effect_ppm,
            "limitPpm": 50_000,
        }),
    )
}

fn runtime_wiring_subcheck(
    evidence: &RuntimeEvidenceV1,
    production_light_worker_image_reference: Option<&str>,
    production_heavy_worker_image_reference: Option<&str>,
    repository_root: Option<&Path>,
) -> Subcheck {
    let required_role_probes = FleetWorkerRole::ALL.map(FleetWorkerRole::as_str);
    let required_stuck_stages = FleetStuckStage::ALL.map(FleetStuckStage::as_str);
    let exact_role_set = evidence.wiring.runnable_role_probe_exit_codes.len()
        == required_role_probes.len()
        && required_role_probes
            .iter()
            .all(|role| evidence.wiring.runnable_role_probe_exit_codes.get(*role) == Some(&0));
    let recovery_poll = evidence.wiring.recovery_poll_interval_milliseconds;
    let health_observation = evidence.wiring.health_observation_interval_milliseconds;
    let probed_image_is_production_candidate = production_light_worker_image_reference
        .is_some_and(|expected| evidence.wiring.probed_container_image_reference == expected);
    let heavy_image_is_production_candidate = production_heavy_worker_image_reference
        .is_some_and(|expected| evidence.wiring.probed_heavy_container_image_reference == expected);
    let light_suffix = image_commit_suffix(&evidence.wiring.probed_container_image_reference);
    let heavy_suffix = image_commit_suffix(&evidence.wiring.probed_heavy_container_image_reference);
    let (light_source_bound, light_source_evidence) = repository_root
        .map(|root| image_source_binding(root, &evidence.wiring.probed_container_image_reference))
        .unwrap_or((false, json!({"error": "repository root unavailable"})));
    let (heavy_source_bound, heavy_source_evidence) = repository_root
        .map(|root| {
            image_source_binding(
                root,
                &evidence.wiring.probed_heavy_container_image_reference,
            )
        })
        .unwrap_or((false, json!({"error": "repository root unavailable"})));
    let registry_identity_bound = valid_sha256_digest(&evidence.wiring.light_registry_index_digest)
        && valid_sha256_digest(&evidence.wiring.light_linux_amd64_manifest_digest)
        && valid_sha256_digest(&evidence.wiring.heavy_registry_index_digest)
        && valid_sha256_digest(&evidence.wiring.heavy_linux_amd64_manifest_digest)
        && light_suffix.is_some()
        && light_suffix == heavy_suffix
        && Some(evidence.wiring.light_provenance_vcs_revision.as_str()) == light_suffix
        && Some(evidence.wiring.heavy_provenance_vcs_revision.as_str()) == heavy_suffix
        && evidence.wiring.light_provenance_vcs_source == IMAGE_PROVENANCE_SOURCE
        && evidence.wiring.heavy_provenance_vcs_source == IMAGE_PROVENANCE_SOURCE;
    let exact_stuck_stage_set = evidence.wiring.stuck_stage_detection_milliseconds.len()
        == required_stuck_stages.len()
        && recovery_poll > 0
        && health_observation > 0
        && required_stuck_stages.iter().all(|stage| {
            evidence
                .wiring
                .stuck_stage_detection_milliseconds
                .get(*stage)
                .is_some_and(|elapsed| *elapsed <= health_observation)
        });
    subcheck(
        "bound_local_container_and_stuck_stage_evidence_meets_feedback_gates",
        !evidence.wiring.local_container_image_id.trim().is_empty()
            && probed_image_is_production_candidate
            && heavy_image_is_production_candidate
            && light_source_bound
            && heavy_source_bound
            && registry_identity_bound
            && exact_role_set
            && exact_stuck_stage_set,
        json!({
            "evidence": &evidence.wiring,
            "productionLightWorkerImageReference": production_light_worker_image_reference,
            "productionHeavyWorkerImageReference": production_heavy_worker_image_reference,
            "probedImageIsProductionCandidate": probed_image_is_production_candidate,
            "heavyImageIsProductionCandidate": heavy_image_is_production_candidate,
            "registryIdentityBound": registry_identity_bound,
            "lightSourceBinding": light_source_evidence,
            "heavySourceBinding": heavy_source_evidence,
            "requiredRoleProbes": required_role_probes,
            "requiredStuckStages": required_stuck_stages,
        }),
    )
}

fn deterministic_evidence() -> Result<DeterministicEvidence, String> {
    let policy = EconomicPolicy::default();
    let base = opportunity(1);
    let base_score = evaluate_economics(&base, &policy, base.target_net_apy_bps)
        .map_err(|error| format!("{error:?}"))?;

    let mut larger = opportunity(2);
    larger.notional_usd_micros = base.notional_usd_micros * 2;
    let larger_score = evaluate_economics(&larger, &policy, larger.target_net_apy_bps)
        .map_err(|error| format!("{error:?}"))?;

    let mut wider_edge = opportunity(3);
    wider_edge.target_net_apy_bps = base.target_net_apy_bps + 300;
    let wider_edge_score = evaluate_economics(&wider_edge, &policy, wider_edge.target_net_apy_bps)
        .map_err(|error| format!("{error:?}"))?;

    let mut smaller_faster = opportunity(4);
    smaller_faster.notional_usd_micros = base.notional_usd_micros / 2;
    smaller_faster.target_net_apy_bps = 1_200;
    let smaller_faster_score =
        evaluate_economics(&smaller_faster, &policy, smaller_faster.target_net_apy_bps)
            .map_err(|error| format!("{error:?}"))?;
    let smaller_faster_ranked =
        rank_opportunities(vec![base.clone(), smaller_faster.clone()], &policy)
            .map_err(|error| format!("{error:?}"))?;
    let smaller_faster_outranks = smaller_faster_ranked
        .eligible
        .first()
        .is_some_and(|ranked| ranked.opportunity.opportunity_id == smaller_faster.opportunity_id);

    let mut aged = opportunity(5);
    aged.age_seconds = policy.starvation_deadline_seconds;
    let aged_score = evaluate_economics(&aged, &policy, aged.target_net_apy_bps)
        .map_err(|error| format!("{error:?}"))?;
    let mut fresh_competitor = opportunity(11);
    fresh_competitor.target_net_apy_bps += 1;
    let starvation_ranked =
        rank_opportunities(vec![fresh_competitor.clone(), aged.clone()], &policy)
            .map_err(|error| format!("{error:?}"))?;
    let aged_outranks_fresh = starvation_ranked
        .eligible
        .first()
        .is_some_and(|ranked| ranked.opportunity.opportunity_id == aged.opportunity_id);

    let mut dust = opportunity(6);
    dust.notional_usd_micros = policy.minimum_notional_usd_micros - 1;
    let dust_result = evaluate_economics(&dust, &policy, dust.target_net_apy_bps);
    let mut negative_value = opportunity(7);
    negative_value.estimated_execution_cost_usd_micros = 10_000_000_000;
    let negative_result =
        evaluate_economics(&negative_value, &policy, negative_value.target_net_apy_bps);
    let mut short_horizon = opportunity(12);
    short_horizon.holding_horizon_seconds = 60;
    let short_horizon_result =
        evaluate_economics(&short_horizon, &policy, short_horizon.target_net_apy_bps);
    let fee_policy = RouteFeePolicy::default();
    let minimum_fee_budget = route_fee_budget(policy.minimum_net_gain_usd_micros, fee_policy)
        .map_err(|error| format!("{error:?}"))?;
    let below_floor = route_fee_budget(policy.minimum_net_gain_usd_micros - 1, fee_policy);
    let high_value_fee_budget =
        route_fee_budget(10_000_000, fee_policy).map_err(|error| format!("{error:?}"))?;
    let capped_fee_usd_micros = i128::from(minimum_fee_budget.cap_lamports)
        * i128::from(fee_policy.conservative_sol_price_usd_micros)
        / 1_000_000_000;
    let profitable_drift = evaluate_fresh_route_economics(FreshRouteEconomicsInput {
        opportunity: base.clone(),
        durable_observed_target_apy_bps: 600,
        durable_capacity_adjusted_target_apy_bps: 550,
        current_source_apy_bps: 250,
        current_observed_target_apy_bps: 575,
        economic_policy: policy.clone(),
        fee_policy,
    });
    let reversed_edge = evaluate_fresh_route_economics(FreshRouteEconomicsInput {
        opportunity: base.clone(),
        durable_observed_target_apy_bps: 600,
        durable_capacity_adjusted_target_apy_bps: 550,
        current_source_apy_bps: 650,
        current_observed_target_apy_bps: 575,
        economic_policy: policy.clone(),
        fee_policy,
    });
    let profitable_drift_admitted = profitable_drift.as_ref().is_ok_and(|fresh| {
        fresh.capacity_haircut_bps == 50
            && fresh.current_capacity_adjusted_target_apy_bps == 525
            && fresh.score.capacity_adjusted_net_edge_bps == 275
            && fresh.fee_budget.cap_lamports > 0
    });
    let reversed_edge_rejected = matches!(
        &reversed_edge,
        Err(FreshRouteEconomicsError::EconomicallyIneligible {
            reason: IneligibleReason::NonPositiveEdge
        })
    );

    let mut low = opportunity(8);
    low.notional_usd_micros /= 2;
    let ranked = rank_opportunities(vec![low, larger.clone()], &policy)
        .map_err(|error| format!("{error:?}"))?;
    let priority_ordered = ranked
        .eligible
        .windows(2)
        .all(|pair| pair[0].economics.total_priority >= pair[1].economics.total_priority)
        && ranked
            .eligible
            .first()
            .is_some_and(|item| item.opportunity.opportunity_id == larger.opportunity_id);

    let mut capacity_a = opportunity(9);
    capacity_a.notional_usd_micros = 100_000_000_000;
    let mut capacity_b = opportunity(10);
    capacity_b.notional_usd_micros = 100_000_000_000;
    let capacity_wave = plan_capacity_aware_wave(
        vec![capacity_a, capacity_b],
        &policy,
        vec![TargetCapacityCurve {
            target_reserve: "target".to_owned(),
            observed_supply_usd_micros: 10_000_000_000_000,
            observed_net_apy_bps: 600,
            already_committed_inflow_usd_micros: 0,
            already_committed_outflow_usd_micros: 0,
            bands: vec![
                CapacityBand {
                    cumulative_inflow_usd_micros: 100_000_000_000,
                    target_net_apy_bps: 600,
                },
                CapacityBand {
                    cumulative_inflow_usd_micros: 200_000_000_000,
                    target_net_apy_bps: 150,
                },
            ],
        }],
        &WaveLimits {
            max_opportunities: 10,
            max_notional_usd_micros: 1_000_000_000_000,
            max_per_tenant: 10,
            max_per_writable_conflict_key: 1,
        },
    )
    .map_err(|error| format!("{error:?}"))?;

    let started = Instant::now();
    let benchmark = run_deterministic_benchmark(10_000, 0x004c_4f59_414c)
        .map_err(|error| format!("{error:?}"))?;
    let benchmark_millis = started.elapsed().as_millis();

    let market_observed_at = DateTime::<Utc>::from_timestamp(1_752_000_000, 0)
        .ok_or_else(|| "deterministic market timestamp is invalid".to_owned())?;
    let market_epoch = ImmutableMarketEpoch {
        optimizer_epoch_id: 77,
        fingerprint: "unchanged-market-snapshot".to_owned(),
        catalog_fingerprint: "unchanged-market-catalog".to_owned(),
        captured_at: market_observed_at + chrono::Duration::seconds(10),
        expires_at: market_observed_at + chrono::Duration::minutes(5),
        catalog_expires_at: market_observed_at + chrono::Duration::minutes(5),
        catalog_reserve_count: 1,
        oldest_market_observed_at: Some(market_observed_at),
        newest_market_observed_at: Some(market_observed_at),
        minimum_market_slot: Some(42),
        maximum_market_slot: Some(42),
        mint_coverage: vec![MarketMintCoverage {
            mint: "USDC".to_owned(),
            catalog_reserve_count: 1,
            verified_reserve_count: 1,
            eligible_target_reserve_count: 1,
            complete: true,
            expires_at: Some(market_observed_at + chrono::Duration::minutes(5)),
            blockers: Vec::new(),
        }],
        reserves: vec![MarketEpochReserve {
            state_event_id: 1,
            account_data_hash: "00".repeat(32),
            state_observed_at: market_observed_at,
            state_slot: 42,
            verification_commitment: "confirmed".to_owned(),
            reserve: "reserve".to_owned(),
            market: Some("market".to_owned()),
            liquidity_mint: "USDC".to_owned(),
            mint_decimals: 6,
            market_price_usd_micros: 1_000_000,
            reserve_last_update_slot: 42,
            economic_slot_lag: 0,
            economic_expires_at: market_observed_at + chrono::Duration::minutes(5),
            reserve_last_update_stale: false,
            reserve_price_status: 0,
            market_price_last_updated_ts: market_observed_at.timestamp(),
            available_amount_raw: "1000000000000".to_owned(),
            borrowed_amount_raw: "0".to_owned(),
            total_supply_amount_raw: "1000000000000".to_owned(),
            utilization_ppm: 0,
            borrow_apy_bps: 0,
            observed_at: market_observed_at,
            slot: 42,
            supply_apy_bps: 500,
            total_supply_usd_micros: 1_000_000_000_000,
            target_eligible: true,
        }],
    };
    let mut repeated_read = market_epoch.clone();
    repeated_read.captured_at += chrono::Duration::seconds(30);
    let durable_epoch = market_epoch.durable_optimizer_epoch_evidence();
    let repeated_durable_epoch = repeated_read.durable_optimizer_epoch_evidence();
    let stable_durable_epoch = market_epoch.captured_at != repeated_read.captured_at
        && durable_epoch == repeated_durable_epoch
        && serde_json::to_value(&durable_epoch).map_err(|error| error.to_string())?
            == serde_json::to_value(&repeated_durable_epoch).map_err(|error| error.to_string())?;
    let hinted_poll = schedule_authoritative_status_poll(ConfirmationPollTrigger::SubscriptionHint);
    let durable_poll =
        schedule_authoritative_status_poll(ConfirmationPollTrigger::DurableRecoveryDeadline);
    let missing_slot = classify_authoritative_signature_status(AuthoritativeSignatureStatus {
        slot: None,
        satisfies_confirmed_commitment: true,
        transaction_error: false,
    });
    let unconfirmed_slot = classify_authoritative_signature_status(AuthoritativeSignatureStatus {
        slot: Some(41),
        satisfies_confirmed_commitment: false,
        transaction_error: false,
    });
    let authoritative_success =
        classify_authoritative_signature_status(AuthoritativeSignatureStatus {
            slot: Some(42),
            satisfies_confirmed_commitment: true,
            transaction_error: false,
        });
    let authoritative_failure =
        classify_authoritative_signature_status(AuthoritativeSignatureStatus {
            slot: Some(43),
            satisfies_confirmed_commitment: true,
            transaction_error: true,
        });

    Ok(DeterministicEvidence {
        discovery_subchecks: vec![
            subcheck(
                "unchanged_market_snapshot_reuses_identical_durable_epoch_evidence",
                stable_durable_epoch,
                json!({
                    "firstPlannerCapturedAt": market_epoch.captured_at,
                    "secondPlannerCapturedAt": repeated_read.captured_at,
                    "durableObservedAt": durable_epoch.captured_at,
                    "fingerprint": durable_epoch.fingerprint,
                }),
            ),
            subcheck(
                "economic_priority_order",
                priority_ordered,
                json!({
                    "orderedIds": ranked.eligible.iter().map(|item| item.opportunity.opportunity_id).collect::<Vec<_>>()
                }),
            ),
            subcheck(
                "ten_thousand_vault_replay_under_ten_seconds",
                benchmark.input_count == 10_000 && benchmark_millis < TEN_SECONDS_MILLIS,
                json!({
                    "inputCount": benchmark.input_count,
                    "elapsedMillis": benchmark_millis,
                    "limitMillis": TEN_SECONDS_MILLIS,
                    "selectedCount": benchmark.selected_count,
                    "deferredCount": benchmark.deferred_count,
                    "rejectedCount": benchmark.rejected_count,
                    "deterministicDigest": benchmark.deterministic_digest,
                }),
            ),
        ],
        economic_subchecks: vec![
            subcheck(
                "notional_increases_priority",
                larger_score.total_priority > base_score.total_priority,
                json!({"base": base_score.total_priority, "larger": larger_score.total_priority}),
            ),
            subcheck(
                "edge_increases_priority",
                wider_edge_score.total_priority > base_score.total_priority,
                json!({"base": base_score.total_priority, "widerEdge": wider_edge_score.total_priority}),
            ),
            subcheck(
                "smaller_higher_lost_yield_route_actually_outranks_larger_route",
                smaller_faster_score.lost_yield_usd_micros_per_hour
                    > base_score.lost_yield_usd_micros_per_hour
                    && smaller_faster_outranks,
                json!({
                    "largerBalanceLostYield": base_score.lost_yield_usd_micros_per_hour,
                    "smallerBalanceLostYield": smaller_faster_score.lost_yield_usd_micros_per_hour,
                    "rankedIds": smaller_faster_ranked.eligible.iter().map(|ranked| ranked.opportunity.opportunity_id).collect::<Vec<_>>(),
                }),
            ),
            subcheck(
                "age_deadline_changes_actual_rank_order_and_prevents_starvation",
                aged_score.starved && aged_outranks_fresh,
                json!({
                    "agedPriority": aged_score.total_priority,
                    "starved": aged_score.starved,
                    "freshTargetApyBps": fresh_competitor.target_net_apy_bps,
                    "rankedIds": starvation_ranked.eligible.iter().map(|ranked| ranked.opportunity.opportunity_id).collect::<Vec<_>>(),
                }),
            ),
            subcheck(
                "dust_negative_value_and_short_holding_horizon_rejected",
                matches!(dust_result, Err(IneligibleReason::BelowMinimumNotional))
                    && matches!(
                        negative_result,
                        Err(IneligibleReason::ExpectedGainDoesNotCoverCost { .. })
                    )
                    && matches!(
                        short_horizon_result,
                        Err(IneligibleReason::ExpectedGainDoesNotCoverCost { .. })
                            | Err(IneligibleReason::BelowMinimumNetGain { .. })
                    ),
                json!({
                    "dust": dust_result,
                    "negativeValue": negative_result,
                    "shortHoldingHorizonSeconds": short_horizon.holding_horizon_seconds,
                    "shortHoldingHorizon": short_horizon_result,
                }),
            ),
            subcheck(
                "fee_floor_never_exceeds_incremental_yield_fraction",
                minimum_fee_budget.cap_lamports == fee_policy.minimum_cap_lamports
                    && capped_fee_usd_micros
                        <= i128::from(minimum_fee_budget.allowed_fee_usd_micros)
                    && matches!(
                        below_floor,
                        Err(RouteFeeBudgetError::FeeFloorExceedsEconomicCap { .. })
                    )
                    && high_value_fee_budget.cap_lamports == fee_policy.maximum_cap_lamports,
                json!({
                    "minimumNetGainUsdMicros": policy.minimum_net_gain_usd_micros,
                    "minimumFeeBudget": minimum_fee_budget,
                    "belowFloor": below_floor,
                    "highValueFeeBudget": high_value_fee_budget,
                    "feePolicy": fee_policy,
                }),
            ),
            subcheck(
                "profitable_current_market_drift_remains_admissible",
                profitable_drift_admitted,
                json!({
                    "durableObservedTargetApyBps": 600,
                    "durableCapacityAdjustedTargetApyBps": 550,
                    "currentSourceApyBps": 250,
                    "currentObservedTargetApyBps": 575,
                    "result": profitable_drift,
                }),
            ),
            subcheck(
                "reversed_current_market_edge_fails_closed",
                reversed_edge_rejected,
                json!({
                    "durableObservedTargetApyBps": 600,
                    "durableCapacityAdjustedTargetApyBps": 550,
                    "currentSourceApyBps": 650,
                    "currentObservedTargetApyBps": 575,
                    "result": reversed_edge,
                }),
            ),
            subcheck(
                "capacity_stops_marginally_negative_flow",
                capacity_wave.selected.len() == 1
                    && capacity_wave.rejected.len() == 1
                    && matches!(
                        capacity_wave.rejected[0].reason,
                        IneligibleReason::NonPositiveEdge
                    ),
                json!({
                    "selected": capacity_wave.selected.len(),
                    "rejected": capacity_wave.rejected.len(),
                    "reasons": capacity_wave.rejected.iter().map(|item| &item.reason).collect::<Vec<_>>(),
                }),
            ),
        ],
        execution_subchecks: vec![subcheck(
            "subscription_hint_only_accelerates_authoritative_confirmation_poll",
            hinted_poll.urgency == AuthoritativePollUrgency::Immediate
                && durable_poll.urgency == AuthoritativePollUrgency::Scheduled
                && missing_slot == AuthoritativeConfirmationDecision::Pending
                && unconfirmed_slot == AuthoritativeConfirmationDecision::Pending
                && authoritative_success
                    == AuthoritativeConfirmationDecision::Confirmed { slot: 42 }
                && authoritative_failure == AuthoritativeConfirmationDecision::Failed { slot: 43 },
            json!({
                "subscriptionHintAction": "immediate_getSignatureStatuses",
                "durableFallbackAction": "scheduled_batched_getSignatureStatuses",
                "hintCanTerminalize": false,
                "missingAuthoritativeSlot": format!("{missing_slot:?}"),
                "unconfirmedAuthoritativeSlot": format!("{unconfirmed_slot:?}"),
                "confirmedAuthoritativeSlot": format!("{authoritative_success:?}"),
                "failedAuthoritativeSlot": format!("{authoritative_failure:?}"),
            }),
        )],
    })
}

impl DatabaseFixture {
    async fn connect(database_url: &str) -> Result<Self, Box<dyn Error>> {
        let options = PgConnectOptions::from_str(database_url)?;
        let configured_database = options
            .get_database()
            .ok_or("isolated verifier URL must name a database")?;
        if !configured_database.contains("fleet_verify") {
            return Err(
                "refusing database verification: database name must contain fleet_verify".into(),
            );
        }

        let client = NeonSqlClient::connect(
            NeonSqlConfig::new(database_url)
                .with_max_connections(12)
                .with_acquire_timeout(Duration::from_secs(5)),
        )
        .await?;
        // Keep comparative queue timings on one session. The general fixture
        // pool intentionally exercises concurrency, but allowing an A/B
        // latency sample to bounce across twelve sessions introduces
        // unrelated prepared-plan and connection-scheduling variance that can
        // dwarf the five-percent ALT isolation threshold.
        let latency_client = NeonSqlClient::connect(
            NeonSqlConfig::new(database_url)
                .with_max_connections(1)
                .with_acquire_timeout(Duration::from_secs(5)),
        )
        .await?;
        let actual_database: String = sqlx::query_scalar("SELECT current_database()")
            .fetch_one(client.pool())
            .await?;
        if !actual_database.contains("fleet_verify") {
            return Err("refusing database verification: connected database name does not contain fleet_verify"
                .into());
        }
        for (version, name, _) in VERIFIED_MIGRATIONS {
            client.require_schema_migration(version, name).await?;
        }

        let unique = SystemTime::now().duration_since(UNIX_EPOCH)?.as_nanos();
        Ok(Self {
            client,
            latency_client,
            prefix: format!("fleet_verify_{}_{}", std::process::id(), unique),
        })
    }

    fn cluster(&self, suffix: &str) -> String {
        format!("{}_{}", self.prefix, suffix)
    }

    async fn seed_epoch(&self, cluster: &str) -> Result<i64, Box<dyn Error>> {
        let now = Utc::now();
        let policy_payer = format!("authority:{cluster}");
        let family_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.lookup_table_families
                (cluster, logical_name, kind, desired_state, planner_version,
                 catalog_version, active_generation, provisioning_authority,
                 payer, hard_capacity, largest_atomic_expansion,
                 safety_margin, allocation_high_water)
            VALUES
                ($1, 'fleet-verifier-shared-market', 'shared_market', 'active',
                 'fleet-verifier-v1', 'fleet-verifier-v1', 0, $2, $2,
                 256, 32, 8, 216)
            ON CONFLICT (cluster, logical_name) DO UPDATE
            SET updated_at = clock_timestamp()
            RETURNING id
            "#,
        )
        .bind(cluster)
        .bind(&policy_payer)
        .fetch_one(self.client.pool())
        .await?;
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.route_lookup_tables
                (cluster, scope, table_address, authority, payer, status,
                 durable, address_count, address_hash, addresses, family_id,
                 allocation_kind, generation, shard_ordinal, desired_state,
                 accepting_allocations, allocation_high_water,
                 reserved_address_count, usable_address_count,
                 last_extended_start_index, last_verified_slot,
                 last_verified_at, mutation_epoch)
            VALUES
                ($1, $2, $3, $4, $4, 'active', TRUE, 1, $5,
                 jsonb_build_array($4), $6, 'shared_market', 0, 0, 'active',
                 TRUE, 216, 1, 1, 0, 10000, clock_timestamp(), 0)
            ON CONFLICT (table_address) DO UPDATE
            SET updated_at = clock_timestamp()
            "#,
        )
        .bind(cluster)
        .bind(format!("reusable-v2:{cluster}"))
        .bind(format!("verifier-alt:{cluster}"))
        .bind(&policy_payer)
        .bind(format!("verifier-alt-hash:{cluster}"))
        .bind(family_id)
        .execute(self.client.pool())
        .await?;
        let epoch = self
            .client
            .upsert_optimizer_epoch(
                loyal_yield_orchestrator::fleet_orchestration::OptimizerEpochInput {
                    cluster: cluster.to_owned(),
                    epoch_key: format!("{}:epoch", cluster),
                    market_slot: 10_000,
                    observed_at: now,
                    expires_at: now + chrono::Duration::hours(4),
                    market_state: json!({"fixture": self.prefix}),
                },
            )
            .await?;
        Ok(epoch.id)
    }

    async fn seed_opportunity(
        &self,
        cluster: &str,
        epoch_id: i64,
        label: &str,
        state: &str,
        economic_priority: i64,
    ) -> Result<SeededOpportunity, Box<dyn Error>> {
        let identity = format!("{}:{}", self.prefix, label);
        let policy_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.route_policies
                (settings, authority, policy_seed, policy_account, vault_index,
                 vault_pubkey, delegated_signers, threshold, route_modes,
                 stable_mints, kamino_markets, kamino_liquidity_mints,
                 swap_lanes, active, last_seen_slot, last_seen_signature)
            VALUES
                ($1, $2, 1, $3, 0, $4, ARRAY[$2]::TEXT[], 1,
                 ARRAY['same_mint']::TEXT[], ARRAY['USDC']::TEXT[],
                 ARRAY['market']::TEXT[], ARRAY['USDC']::TEXT[],
                 '[]'::jsonb, TRUE, 10000, $5)
            RETURNING id
            "#,
        )
        .bind(format!("settings:{identity}"))
        .bind(format!("authority:{cluster}"))
        .bind(format!("policy:{identity}"))
        .bind(format!("vault:{identity}"))
        .bind(format!("signature:{identity}"))
        .fetch_one(self.client.pool())
        .await?;
        let vault_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.managed_vaults
                (settings, vault_index, vault_pubkey, active_policy_id, active)
            VALUES ($1, 0, $2, $3, TRUE)
            RETURNING id
            "#,
        )
        .bind(format!("settings:{identity}"))
        .bind(format!("vault:{identity}"))
        .bind(policy_id)
        .fetch_one(self.client.pool())
        .await?;
        let source_reserve = format!("source:{identity}");
        let target_reserve = format!("target:{identity}");
        let (alt_table_id, alt_mutation_epoch): (i64, i64) = sqlx::query_as(
            r#"
            SELECT route_table.id, route_table.mutation_epoch
            FROM loyal_yield.route_lookup_tables route_table
            WHERE route_table.cluster = $1
              AND route_table.authority = $2
              AND route_table.payer = $2
              AND route_table.family_id IS NOT NULL
              AND route_table.desired_state = 'active'
            ORDER BY route_table.id
            LIMIT 1
            "#,
        )
        .bind(cluster)
        .bind(format!("authority:{cluster}"))
        .fetch_one(self.client.pool())
        .await?;
        let execution_plan = json!({
            "kind": "same_mint",
            "route_amount_semantics": "redeemable_liquidity_amount",
            "optimizer_market_slot": 10_000,
            "verifier_policy_payer": format!("authority:{cluster}"),
            "verifier_alt_table_id": alt_table_id,
            "verifier_alt_mutation_epoch": alt_mutation_epoch,
        });
        let snapshot = self
            .client
            .apply_observed_patch(
                VaultId(vault_id),
                ReconciledVaultState {
                    observed_slot: 10_000,
                    observed_at: Some(Utc::now()),
                    chain_slot: Some(10_000),
                    lock_attempt_id: None,
                    context: json!({"fixture": self.prefix}),
                    positions: vec![
                        ReconciledReservePosition {
                            reserve: source_reserve.clone(),
                            market: Some("market".to_owned()),
                            liquidity_mint: "USDC".to_owned(),
                            amount_raw: 1_000_000,
                            supply_apy_bps: Some(200),
                            borrow_apy_bps: None,
                            planning_metadata: json!({
                                "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                            }),
                        },
                        ReconciledReservePosition {
                            reserve: target_reserve.clone(),
                            market: Some("market".to_owned()),
                            liquidity_mint: "USDC".to_owned(),
                            amount_raw: 0,
                            supply_apy_bps: Some(600),
                            borrow_apy_bps: None,
                            planning_metadata: json!({
                                "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                            }),
                        },
                    ],
                },
            )
            .await?;
        let opportunity_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.rebalance_opportunities
                (cluster, idempotency_key, vault_id, source_snapshot_id, optimizer_epoch_id,
                 route_fingerprint, requirements_fingerprint, source_reserve,
                 target_reserve, liquidity_mint, amount_raw,
                 principal_usd_micros, source_apy_bps, target_apy_bps,
                 estimated_edge_bps, estimated_cost_lamports,
                 annual_yield_gain_usd_micros, expected_net_gain_usd_micros,
                 economic_priority, priority_version, opportunity_state,
                 execution_plan, available_at, expires_at, created_at)
            VALUES
                ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'USDC', 1000000,
                 100000000, 200, 600, 400, 5000, 4000000, 3500000,
                 $10, 'fleet-verifier-v1', $11,
                 $12,
                 now() - interval '1 minute', now() + interval '2 hours',
                 now() - interval '1 minute')
            RETURNING id
            "#,
        )
        .bind(cluster)
        .bind(format!("opportunity:{identity}"))
        .bind(vault_id)
        .bind(snapshot.id.as_i64())
        .bind(epoch_id)
        .bind(format!("route:{identity}"))
        .bind(format!("requirements:{identity}"))
        .bind(source_reserve)
        .bind(target_reserve)
        .bind(economic_priority)
        .bind(state)
        .bind(execution_plan)
        .fetch_one(self.client.pool())
        .await?;
        Ok(SeededOpportunity {
            id: opportunity_id,
            economic_priority,
        })
    }

    async fn seed_claim_latency_cluster(
        &self,
        cluster: &str,
        epoch_id: i64,
        ready_count: i64,
        waiting_alt_count: i64,
        inert_vault_count: i64,
    ) -> Result<(), Box<dyn Error>> {
        let settings = format!("settings:{}:{}:claim-latency", self.prefix, cluster);
        let policy_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.route_policies
                (settings, authority, policy_seed, policy_account, vault_index,
                 vault_pubkey, delegated_signers, threshold, route_modes,
                 stable_mints, kamino_markets, kamino_liquidity_mints,
                 swap_lanes, active, last_seen_slot, last_seen_signature)
            VALUES
                ($1, $2, 1, $3, 0, $4, ARRAY[$2]::TEXT[], 1,
                 ARRAY['same_mint']::TEXT[], ARRAY['USDC']::TEXT[],
                 ARRAY['market']::TEXT[], ARRAY['USDC']::TEXT[],
                 '[]'::jsonb, TRUE, 10000, $5)
            RETURNING id
            "#,
        )
        .bind(&settings)
        .bind(format!("authority:{cluster}"))
        .bind(format!("policy:{cluster}"))
        .bind(format!("policy-vault:{cluster}"))
        .bind(format!("signature:{cluster}"))
        .fetch_one(self.client.pool())
        .await?;
        let total_count = ready_count
            .checked_add(waiting_alt_count)
            .and_then(|count| count.checked_add(inert_vault_count))
            .ok_or("claim-latency fixture count overflow")?;
        let opportunity_count = ready_count
            .checked_add(waiting_alt_count)
            .ok_or("claim-latency opportunity count overflow")?;
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.managed_vaults
                (settings, vault_index, vault_pubkey, active_policy_id, active)
            SELECT $1, 0, concat($2, ':vault:', ordinal), $3, TRUE
            FROM generate_series(1, $4::BIGINT) ordinal
            "#,
        )
        .bind(&settings)
        .bind(cluster)
        .bind(policy_id)
        .bind(total_count)
        .execute(self.client.pool())
        .await?;
        sqlx::query(
            r#"
            WITH ranked_vaults AS (
                SELECT vault.id,
                       row_number() OVER (ORDER BY vault.id) AS ordinal
                FROM loyal_yield.managed_vaults vault
                WHERE vault.active_policy_id = $1 AND vault.settings = $2
            )
            INSERT INTO loyal_yield.rebalance_opportunities
                (cluster, idempotency_key, vault_id, source_snapshot_id,
                 optimizer_epoch_id, route_fingerprint,
                 requirements_fingerprint, source_reserve, target_reserve,
                 liquidity_mint, amount_raw, principal_usd_micros,
                 source_apy_bps, target_apy_bps, estimated_edge_bps,
                 estimated_cost_lamports, annual_yield_gain_usd_micros,
                 expected_net_gain_usd_micros, economic_priority,
                 priority_version, opportunity_state, execution_plan,
                 available_at, expires_at, created_at)
            SELECT $3,
                   concat('opportunity:', $3, ':', ranked_vaults.id),
                   ranked_vaults.id, NULL, $4,
                   concat('route:', $3, ':', ranked_vaults.id),
                   concat('requirements:', $3, ':', ranked_vaults.id),
                   concat('source:', ranked_vaults.id),
                   concat('target:', ranked_vaults.id),
                   'USDC', 1000000, 100000000, 200, 600, 400, 5000,
                   4000000, 3500000,
                   1000000000 - ranked_vaults.ordinal,
                   'fleet-verifier-latency-v1',
                   CASE WHEN ranked_vaults.ordinal <= $5
                        THEN 'ready' ELSE 'waiting_alt' END,
                   '{"kind":"same_mint","route_amount_semantics":"redeemable_liquidity_amount","optimizer_market_slot":10000}'::jsonb,
                   now() - interval '1 minute', now() + interval '2 hours',
                   now() - interval '1 hour'
            FROM ranked_vaults
            WHERE ranked_vaults.ordinal <= $6
            "#,
        )
        .bind(policy_id)
        .bind(&settings)
        .bind(cluster)
        .bind(epoch_id)
        .bind(ready_count)
        .bind(opportunity_count)
        .execute(self.client.pool())
        .await?;
        Ok(())
    }

    async fn cleanup(&self) -> Result<Value, Box<dyn Error>> {
        let cluster_pattern = format!("{}%", self.prefix);
        let settings_pattern = format!("settings:{}%", self.prefix);
        sqlx::query("DELETE FROM loyal_yield.target_capacity_reservations WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.target_capacity_frontiers WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.route_account_conflict_leases WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.orchestration_outbox WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query(
            "DELETE FROM loyal_yield.route_policy_setup_funding_reservations WHERE cluster LIKE $1",
        )
        .bind(&cluster_pattern)
        .execute(self.client.pool())
        .await?;
        sqlx::query(
            "DELETE FROM loyal_yield.route_policy_setup_funding_payers WHERE cluster LIKE $1",
        )
        .bind(&cluster_pattern)
        .execute(self.client.pool())
        .await?;
        sqlx::query("DELETE FROM loyal_yield.signed_route_submissions WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.lookup_table_usage_leases WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query(
            "DELETE FROM loyal_yield.lookup_table_route_readiness_current WHERE cluster LIKE $1",
        )
        .bind(&cluster_pattern)
        .execute(self.client.pool())
        .await?;
        sqlx::query(
            "DELETE FROM loyal_yield.lookup_table_provisioning_request_consumers consumer USING loyal_yield.rebalance_opportunities opportunity WHERE consumer.opportunity_id = opportunity.id AND opportunity.cluster LIKE $1",
        )
        .bind(&cluster_pattern)
        .execute(self.client.pool())
        .await?;
        sqlx::query(
            "DELETE FROM loyal_yield.lookup_table_provisioning_requests WHERE cluster LIKE $1",
        )
        .bind(&cluster_pattern)
        .execute(self.client.pool())
        .await?;
        sqlx::query("DELETE FROM loyal_yield.rebalance_opportunities WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query(
            "DELETE FROM loyal_yield.rebalance_decisions decision USING loyal_yield.managed_vaults vault WHERE decision.vault_id = vault.id AND vault.settings LIKE $1",
        )
        .bind(&settings_pattern)
        .execute(self.client.pool())
        .await?;
        sqlx::query(
            "DELETE FROM loyal_yield.vault_reserve_positions_current position USING loyal_yield.managed_vaults vault WHERE position.vault_id = vault.id AND vault.settings LIKE $1",
        )
        .bind(&settings_pattern)
        .execute(self.client.pool())
        .await?;
        sqlx::query(
            "DELETE FROM loyal_yield.vault_position_snapshots snapshot USING loyal_yield.managed_vaults vault WHERE snapshot.vault_id = vault.id AND vault.settings LIKE $1",
        )
        .bind(&settings_pattern)
        .execute(self.client.pool())
        .await?;
        sqlx::query(
            "DELETE FROM loyal_yield.fleet_planning_dirty_vaults dirty USING loyal_yield.managed_vaults vault WHERE dirty.vault_id = vault.id AND vault.settings LIKE $1",
        )
        .bind(&settings_pattern)
        .execute(self.client.pool())
        .await?;
        sqlx::query("DELETE FROM loyal_yield.managed_vaults WHERE settings LIKE $1")
            .bind(&settings_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.cross_mint_swap_policies WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.cross_mint_vault_opt_ins WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.route_policies WHERE settings LIKE $1")
            .bind(&settings_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.route_lookup_tables WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.lookup_table_families WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.fleet_planning_state WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.fleet_planning_clusters WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;

        let mutable_rows: i64 = sqlx::query_scalar(
            r#"
            SELECT
                (SELECT count(*) FROM loyal_yield.rebalance_opportunities WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.lookup_table_usage_leases WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.target_capacity_frontiers WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.fleet_planning_state WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.fleet_planning_clusters WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.managed_vaults WHERE settings LIKE $2)
              + (SELECT count(*) FROM loyal_yield.route_policies WHERE settings LIKE $2)
              + (SELECT count(*) FROM loyal_yield.cross_mint_swap_policies WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.cross_mint_vault_opt_ins WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.route_lookup_tables WHERE cluster LIKE $1)
              + (SELECT count(*) FROM loyal_yield.lookup_table_families WHERE cluster LIKE $1)
            "#,
        )
        .bind(&cluster_pattern)
        .bind(&settings_pattern)
        .fetch_one(self.client.pool())
        .await?;
        let immutable_epochs: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM loyal_yield.optimizer_epochs WHERE cluster LIKE $1",
        )
        .bind(&cluster_pattern)
        .fetch_one(self.client.pool())
        .await?;
        Ok(json!({
            "mutableRowsRemaining": mutable_rows,
            "immutableEpochRowsRetainedBySchemaContract": immutable_epochs,
        }))
    }
}

async fn claim_one(
    client: &NeonSqlClient,
    cluster: &str,
    owner: &str,
    kind: RebalanceOpportunityClaimKind,
) -> Result<RebalanceOpportunityLease, Box<dyn Error>> {
    client
        .lease_next_rebalance_opportunity(
            cluster,
            owner,
            kind,
            Utc::now() + chrono::Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| format!("no {kind:?} opportunity was claimable").into())
}

async fn claim_latency_batch_micros(
    client: &NeonSqlClient,
    cluster: &str,
    owner: &str,
    count: usize,
) -> Result<(u128, u128), Box<dyn Error>> {
    let started = Instant::now();
    let (claimed, server_elapsed_micros) = client
        .lease_rebalance_opportunity_batch_measured(
            cluster,
            owner,
            RebalanceOpportunityClaimKind::Execute,
            i64::try_from(count)?,
            Utc::now() + chrono::Duration::minutes(10),
        )
        .await?;
    let client_elapsed_micros = started.elapsed().as_micros();
    if claimed.len() != count {
        return Err(format!(
            "claim-latency fixture requested {count} rows but claimed {}",
            claimed.len()
        )
        .into());
    }
    Ok((client_elapsed_micros, u128::from(server_elapsed_micros)))
}

fn p95_micros(samples: &mut [u128]) -> u128 {
    samples.sort_unstable();
    let index = (samples.len() * 95).div_ceil(100).saturating_sub(1);
    samples[index]
}

fn percentile_millis(samples: &mut [u128], percentile: usize) -> u128 {
    samples.sort_unstable();
    let index = (samples.len() * percentile).div_ceil(100).saturating_sub(1);
    samples[index]
}

#[derive(Clone, Copy)]
enum FleetLatencyLoadMode {
    LegacyPolicyLock,
    ReservationNonFused,
    ReservationFused,
}

impl FleetLatencyLoadMode {
    fn label(self) -> &'static str {
        match self {
            Self::LegacyPolicyLock => "legacy_policy_lock",
            Self::ReservationNonFused => "reservation_non_fused",
            Self::ReservationFused => "reservation_fused",
        }
    }

    fn uses_policy_lock(self) -> bool {
        matches!(self, Self::LegacyPolicyLock)
    }

    fn final_build_millis(self) -> u64 {
        match self {
            Self::LegacyPolicyLock | Self::ReservationNonFused => 126,
            Self::ReservationFused => 5,
        }
    }

    fn post_decision_millis(self) -> u64 {
        match self {
            Self::LegacyPolicyLock | Self::ReservationNonFused => 102,
            Self::ReservationFused => 43,
        }
    }
}

async fn run_fleet_latency_load_mode(
    fixture: &DatabaseFixture,
    mode: FleetLatencyLoadMode,
) -> Result<Value, Box<dyn Error>> {
    const JOBS: usize = 8;
    const PLANNER_WAKE_MILLIS: u64 = 10;
    const REVALIDATION_MILLIS: u64 = 126;
    const CONFLICT_RETRY_MILLIS: u64 = 20;
    const OBSERVATION_AGE_MILLIS: u128 = 136;

    let cluster = fixture.cluster(&format!("latency_load_{}", mode.label()));
    let epoch = fixture.seed_epoch(&cluster).await?;
    fixture
        .seed_claim_latency_cluster(&cluster, epoch, i64::try_from(JOBS)?, 0, 0)
        .await?;
    let leases = fixture
        .client
        .lease_rebalance_opportunity_batch(
            &cluster,
            &format!("latency-load-{}", mode.label()),
            RebalanceOpportunityClaimKind::Execute,
            i64::try_from(JOBS)?,
            Utc::now() + chrono::Duration::minutes(5),
        )
        .await?;
    if leases.len() != JOBS {
        return Err(format!(
            "{} latency load claimed {} of {JOBS} jobs",
            mode.label(),
            leases.len()
        )
        .into());
    }
    let payer = format!("authority:{cluster}");
    if !mode.uses_policy_lock() {
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.route_policy_setup_funding_payers
                (cluster, payer, observed_balance_lamports,
                 observed_balance_slot, observed_balance_at)
            VALUES ($1, $2, 1000000000, 10000, now())
            ON CONFLICT (cluster, payer) DO NOTHING
            "#,
        )
        .bind(&cluster)
        .bind(&payer)
        .execute(fixture.client.pool())
        .await?;
    }

    let wave_started = Instant::now();
    let mut tasks = Vec::with_capacity(JOBS);
    for (index, lease) in leases.into_iter().enumerate() {
        let client = fixture.client.clone();
        let cluster = cluster.clone();
        let payer = payer.clone();
        tasks.push(tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(PLANNER_WAKE_MILLIS)).await;
            tokio::time::sleep(Duration::from_millis(REVALIDATION_MILLIS)).await;
            let ready_at = wave_started.elapsed().as_millis();
            let mut conflict_keys = vec![
                format!("fleet-shared-write-lane:latency-{index:02}"),
                format!("vault-write:latency:{}", lease.opportunity.id),
            ];
            if mode.uses_policy_lock() {
                conflict_keys.push(format!("policy-setup-funding:{payer}"));
            }
            conflict_keys.sort_unstable();
            let mut conflict_retries = 0u64;
            loop {
                match client
                    .acquire_route_account_conflict_leases(
                        &lease,
                        &conflict_keys,
                        Utc::now() + chrono::Duration::minutes(4),
                    )
                    .await
                {
                    Ok(_) => break,
                    Err(error) if mode.uses_policy_lock() && conflict_retries < 1_000 => {
                        let _ = error;
                        conflict_retries += 1;
                        tokio::time::sleep(Duration::from_millis(CONFLICT_RETRY_MILLIS)).await;
                    }
                    Err(error) => return Err(error),
                }
            }

            let admission_started = Instant::now();
            if !mode.uses_policy_lock() {
                let mut tx = client.pool().begin().await?;
                sqlx::query(
                    r#"
                    SELECT payer
                    FROM loyal_yield.route_policy_setup_funding_payers
                    WHERE cluster = $1 AND payer = $2
                    FOR UPDATE
                    "#,
                )
                .bind(&cluster)
                .bind(&payer)
                .fetch_one(&mut *tx)
                .await?;
                tx.commit().await?;
            }
            let admission_micros = admission_started.elapsed().as_micros();

            tokio::time::sleep(Duration::from_millis(mode.final_build_millis())).await;
            let decision_at = wave_started.elapsed().as_millis();
            tokio::time::sleep(Duration::from_millis(32)).await;
            let submitted_at = wave_started.elapsed().as_millis();
            tokio::time::sleep(Duration::from_millis(
                mode.post_decision_millis().saturating_sub(32),
            ))
            .await;
            let completed_at = wave_started.elapsed().as_millis();
            sqlx::query(
                r#"
                DELETE FROM loyal_yield.route_account_conflict_leases
                WHERE cluster = $1
                  AND opportunity_id = $2
                  AND submission_id IS NULL
                "#,
            )
            .bind(&cluster)
            .bind(lease.opportunity.id)
            .execute(client.pool())
            .await?;
            Ok::<_, OrchestratorError>((
                ready_at,
                decision_at,
                submitted_at,
                completed_at,
                admission_micros,
                conflict_retries,
            ))
        }));
    }

    let mut ready_to_decision = Vec::with_capacity(JOBS);
    let mut ready_to_submitted = Vec::with_capacity(JOBS);
    let mut monitor_to_submitted = Vec::with_capacity(JOBS);
    let mut completion = Vec::with_capacity(JOBS);
    let mut admission_micros = Vec::with_capacity(JOBS);
    let mut conflict_retries = 0u64;
    for task in tasks {
        let (ready_at, decision_at, submitted_at, completed_at, admission, retries) =
            task.await??;
        ready_to_decision.push(decision_at.saturating_sub(ready_at));
        ready_to_submitted.push(submitted_at.saturating_sub(ready_at));
        monitor_to_submitted.push(submitted_at.saturating_add(OBSERVATION_AGE_MILLIS));
        completion.push(completed_at);
        admission_micros.push(admission);
        conflict_retries = conflict_retries.saturating_add(retries);
    }
    let ready_to_decision_p50 = percentile_millis(&mut ready_to_decision.clone(), 50);
    let ready_to_decision_p95 = percentile_millis(&mut ready_to_decision, 95);
    let ready_to_submitted_p50 = percentile_millis(&mut ready_to_submitted.clone(), 50);
    let monitor_to_submitted_p50 = percentile_millis(&mut monitor_to_submitted.clone(), 50);
    let monitor_to_submitted_p95 = percentile_millis(&mut monitor_to_submitted, 95);
    let completion_p95 = percentile_millis(&mut completion, 95);
    let admission_p95_micros = p95_micros(&mut admission_micros);
    Ok(json!({
        "mode": mode.label(),
        "jobs": JOBS,
        "timeScale": 100,
        "controlledDelays": {
            "observationAgeMillis": OBSERVATION_AGE_MILLIS,
            "plannerWakeMillis": PLANNER_WAKE_MILLIS,
            "revalidationMillis": REVALIDATION_MILLIS,
            "finalBuildMillis": mode.final_build_millis(),
            "postDecisionMillis": mode.post_decision_millis(),
            "conflictRetryMillis": CONFLICT_RETRY_MILLIS,
        },
        "readyToDecisionP50Millis": ready_to_decision_p50,
        "readyToDecisionP95Millis": ready_to_decision_p95,
        "readyToSubmittedP50Millis": ready_to_submitted_p50,
        "monitorTimestampToSubmittedP50Millis": monitor_to_submitted_p50,
        "monitorTimestampToSubmittedP95Millis": monitor_to_submitted_p95,
        "waveCompletionP95Millis": completion_p95,
        "reservationAdmissionP95Micros": admission_p95_micros,
        "conflictRetries": conflict_retries,
    }))
}

async fn run_fleet_latency_load(fixture: &DatabaseFixture) -> Result<Value, Box<dyn Error>> {
    let legacy =
        run_fleet_latency_load_mode(fixture, FleetLatencyLoadMode::LegacyPolicyLock).await?;
    let non_fused =
        run_fleet_latency_load_mode(fixture, FleetLatencyLoadMode::ReservationNonFused).await?;
    let fused =
        run_fleet_latency_load_mode(fixture, FleetLatencyLoadMode::ReservationFused).await?;
    let legacy_p50 = legacy["readyToSubmittedP50Millis"]
        .as_u64()
        .unwrap_or_default();
    let non_fused_p50 = non_fused["readyToSubmittedP50Millis"]
        .as_u64()
        .unwrap_or_default();
    let fused_p50 = fused["readyToSubmittedP50Millis"]
        .as_u64()
        .unwrap_or_default();
    Ok(json!({
        "schemaVersion": 1,
        "event": "fleet_latency_isolated_load",
        "isolated": true,
        "productionMutation": false,
        "legacy": legacy,
        "reservationNonFused": non_fused,
        "reservationFused": fused,
        "attribution": {
            "policySerializationP50Millis": legacy_p50.saturating_sub(non_fused_p50),
            "duplicateFinalBuildP50Millis": non_fused_p50.saturating_sub(fused_p50),
            "readyToSubmittedP50SpeedupPercent": if legacy_p50 == 0 { 0 } else {
                legacy_p50.saturating_sub(fused_p50).saturating_mul(100) / legacy_p50
            },
        }
    }))
}

async fn claim_index_tuple_reads(client: &NeonSqlClient) -> Result<(i64, i64), Box<dyn Error>> {
    // PostgreSQL statistics are backend-local before their periodic flush.
    // Force the verifier's one-session latency backend to publish its latest
    // counters, then clear the session snapshot before reading both indexes.
    sqlx::query("SELECT pg_stat_force_next_flush()")
        .execute(client.pool())
        .await?;
    sqlx::query("SELECT pg_stat_clear_snapshot()")
        .execute(client.pool())
        .await?;
    let rows = sqlx::query(
        r#"
        SELECT indexrelname, idx_tup_read
        FROM pg_stat_user_indexes
        WHERE schemaname = 'loyal_yield'
          AND indexrelname IN (
              'rebalance_opportunities_ready_priority_idx',
              'rebalance_opportunities_expired_lease_idx'
          )
        "#,
    )
    .fetch_all(client.pool())
    .await?;
    let mut runnable = None;
    let mut expired = None;
    for row in rows {
        let name: String = row.try_get("indexrelname")?;
        let reads: i64 = row.try_get("idx_tup_read")?;
        match name.as_str() {
            "rebalance_opportunities_ready_priority_idx" => runnable = Some(reads),
            "rebalance_opportunities_expired_lease_idx" => expired = Some(reads),
            _ => {}
        }
    }
    Ok((
        runnable.ok_or("runnable claim index statistics are missing")?,
        expired.ok_or("expired claim index statistics are missing")?,
    ))
}

async fn conflict_rows(
    client: &NeonSqlClient,
    submission_id: i64,
) -> Result<(Vec<String>, Option<DateTime<Utc>>), Box<dyn Error>> {
    let rows = sqlx::query(
        r#"
        SELECT writable_account_key, expires_at
        FROM loyal_yield.route_account_conflict_leases
        WHERE submission_id = $1
        ORDER BY writable_account_key
        "#,
    )
    .bind(submission_id)
    .fetch_all(client.pool())
    .await?;
    let keys = rows
        .iter()
        .map(|row| row.try_get::<String, _>("writable_account_key"))
        .collect::<Result<Vec<_>, _>>()?;
    let minimum_expiry = rows
        .iter()
        .map(|row| row.try_get::<DateTime<Utc>, _>("expires_at"))
        .collect::<Result<Vec<_>, _>>()?
        .into_iter()
        .min();
    Ok((keys, minimum_expiry))
}

fn same_mint_input_for_lease(
    lease: &RebalanceOpportunityLease,
) -> Result<SameMintRebalanceInput, Box<dyn Error>> {
    let opportunity = &lease.opportunity;
    Ok(SameMintRebalanceInput {
        vault_id: Some(opportunity.vault_id),
        settings: None,
        vault_index: None,
        source_reserve: opportunity
            .source_reserve
            .clone()
            .ok_or("signed fixture has no source reserve")?,
        target_reserve: opportunity.target_reserve.clone(),
        liquidity_mint: opportunity.liquidity_mint.clone(),
        amount_raw: opportunity.amount_raw,
        route_amount_semantics: ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
        source_amount_semantics: Some(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned()),
        source_collateral_amount_raw: None,
        redeemable_source_liquidity_amount_raw: Some(opportunity.amount_raw),
        idle_vault_liquidity_amount_raw: None,
        expected_source_snapshot_id: opportunity
            .source_snapshot_id
            .ok_or("signed fixture has no source snapshot")?,
        source_apy_bps: opportunity.source_apy_bps,
        target_apy_bps: opportunity.target_apy_bps,
        estimated_edge_bps: opportunity.estimated_edge_bps,
        estimated_cost_lamports: opportunity.estimated_cost_lamports,
        dry_run: false,
    })
}

fn rediscovery_input_for_opportunity(
    opportunity: &RebalanceOpportunityRecord,
) -> RebalanceOpportunityInput {
    RebalanceOpportunityInput {
        cluster: opportunity.cluster.clone(),
        vault_id: opportunity.vault_id,
        source_snapshot_id: opportunity.source_snapshot_id,
        optimizer_epoch_id: opportunity.optimizer_epoch_id,
        route_fingerprint: opportunity.route_fingerprint.clone(),
        requirements_fingerprint: opportunity.requirements_fingerprint.clone(),
        source_reserve: opportunity.source_reserve.clone(),
        target_reserve: opportunity.target_reserve.clone(),
        liquidity_mint: opportunity.liquidity_mint.clone(),
        amount_raw: opportunity.amount_raw,
        principal_usd_micros: opportunity.principal_usd_micros,
        source_apy_bps: opportunity.source_apy_bps,
        target_apy_bps: opportunity.target_apy_bps,
        estimated_edge_bps: opportunity.estimated_edge_bps,
        estimated_cost_lamports: opportunity.estimated_cost_lamports,
        annual_yield_gain_usd_micros: opportunity.annual_yield_gain_usd_micros,
        expected_net_gain_usd_micros: opportunity.expected_net_gain_usd_micros,
        economic_priority: opportunity.economic_priority,
        priority_version: opportunity.priority_version.clone(),
        execution_plan: opportunity.execution_plan.clone(),
        available_at: Utc::now(),
        expires_at: opportunity.expires_at,
        provisioning_request_id: None,
    }
}

async fn target_capacity_input_for_lease(
    fixture: &DatabaseFixture,
    lease: &RebalanceOpportunityLease,
) -> Result<TargetCapacityReservationInput, Box<dyn Error>> {
    let observation = TargetCapacityObservation {
        cluster: lease.opportunity.cluster.clone(),
        target_reserve: lease.opportunity.target_reserve.clone(),
        liquidity_mint: lease.opportunity.liquidity_mint.clone(),
        observed_supply_usd_micros: 20_000_000_000,
        observed_slot: 10_000,
        maximum_inflight_usd_micros: 1_000_000_000,
    };
    Ok(target_capacity_input_from_projection(
        lease,
        fixture.client.observe_target_capacity(observation).await?,
    ))
}

fn target_capacity_input_from_projection(
    lease: &RebalanceOpportunityLease,
    projection: TargetCapacityProjection,
) -> TargetCapacityReservationInput {
    let opportunity = &lease.opportunity;
    TargetCapacityReservationInput {
        projection,
        principal_usd_micros: opportunity.principal_usd_micros,
        economic_opportunity: OpportunityInput {
            opportunity_id: opportunity.id,
            optimizer_epoch_id: opportunity.optimizer_epoch_id,
            vault_id: opportunity.vault_id.as_i64(),
            tenant_id: opportunity.cluster.clone(),
            source_snapshot_id: opportunity
                .source_snapshot_id
                .map(|snapshot| snapshot.as_i64())
                .unwrap_or(opportunity.id)
                .max(1),
            observed_slot: 10_000,
            mint: opportunity.liquidity_mint.clone(),
            source_reserve: opportunity
                .source_reserve
                .clone()
                .unwrap_or_else(|| "idle-vault-usdc".to_owned()),
            target_reserve: opportunity.target_reserve.clone(),
            notional_usd_micros: opportunity.principal_usd_micros,
            source_net_apy_bps: opportunity.source_apy_bps,
            target_net_apy_bps: 600,
            confidence_ppm: 1_000_000,
            expected_service_millis: 10_000,
            holding_horizon_seconds: 365 * 24 * 60 * 60,
            estimated_execution_cost_usd_micros: 250_000,
            age_seconds: 0,
            fairness_credit: 0,
            writable_conflict_keys: Vec::new(),
        },
        current_observed_target_apy_bps: 600,
        economic_policy: EconomicPolicy::default(),
        fee_policy: RouteFeePolicy::default(),
    }
}

async fn signed_input_for_lease(
    fixture: &DatabaseFixture,
    lease: &RebalanceOpportunityLease,
    conflict_account_keys: Vec<String>,
    label: &str,
) -> Result<SignedRouteSubmissionInput, Box<dyn Error>> {
    let signed_transaction = format!("{}:{label}:exact-signed-bytes", fixture.prefix).into_bytes();
    let signed_transaction_hash = format!("{:x}", Sha256::digest(&signed_transaction));
    let fee_payer = lease
        .opportunity
        .execution_plan
        .get("verifier_policy_payer")
        .and_then(Value::as_str)
        .ok_or("signed fixture has no durable policy payer evidence")?
        .to_owned();
    let alt_table_id = lease
        .opportunity
        .execution_plan
        .get("verifier_alt_table_id")
        .and_then(Value::as_i64)
        .ok_or("signed fixture has no reusable-v2 ALT table evidence")?;
    let alt_mutation_epoch = lease
        .opportunity
        .execution_plan
        .get("verifier_alt_mutation_epoch")
        .and_then(Value::as_i64)
        .ok_or("signed fixture has no reusable-v2 ALT mutation epoch evidence")?;
    let input = SignedRouteSubmissionInput {
        cluster: lease.opportunity.cluster.clone(),
        semantic_key: format!("semantic:{}:{label}", fixture.prefix),
        opportunity_id: lease.opportunity.id,
        decision_id: None,
        signed_transaction,
        signed_transaction_hash,
        message_hash: format!("message:{}:{label}", fixture.prefix),
        transaction_signature: format!("transaction:{}:{label}", fixture.prefix),
        recent_blockhash: format!("blockhash:{}:{label}", fixture.prefix),
        last_valid_block_height: 100_000,
        source_snapshot_id: lease.opportunity.source_snapshot_id,
        optimizer_epoch_id: lease.opportunity.optimizer_epoch_id,
        alt_requirements_fingerprint: lease
            .opportunity
            .requirements_fingerprint
            .clone()
            .ok_or("signed fixture has no requirements fingerprint")?,
        alt_selection_fingerprint: format!("alt-selection:{}:{label}", fixture.prefix),
        alt_mutation_epochs: json!({
            "tables": [{
                "tableId": alt_table_id,
                "mutationEpoch": alt_mutation_epoch,
            }]
        }),
        fee_payer: fee_payer.clone(),
        fee_payer_kind: RouteFeePayerKind::Policy,
        fee_payer_balance_lamports: None,
        fee_payer_balance_slot: None,
        fee_payer_balance_observed_at: None,
        policy_setup_funding_lamports: None,
        compiled_fee_lamports: 5_000,
        writable_account_keys: vec![
            fee_payer,
            format!("transaction-write:{}:{label}", fixture.prefix),
        ],
        conflict_account_keys,
        executor_owner: lease.owner.clone(),
        executor_fencing_token: lease.fencing_token,
    };
    fixture
        .client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: input.cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
            reference_key: input.semantic_key.clone(),
            route_lookup_table_ids: vec![alt_table_id],
            vault_id: Some(lease.opportunity.vault_id),
            binding_id: None,
            route_fingerprint: lease.opportunity.route_fingerprint.clone(),
            requirements_fingerprint: Some(input.alt_requirements_fingerprint.clone()),
            expires_at: Utc::now() + chrono::Duration::minutes(15),
        })
        .await?;
    Ok(input)
}

async fn prepare_signed_submission_fixture(
    fixture: &DatabaseFixture,
    cluster: &str,
    optimizer_epoch_id: i64,
    label: &str,
    economic_priority: i64,
    last_valid_block_height: i64,
) -> Result<SignedRouteSubmissionRecord, Box<dyn Error>> {
    fixture
        .seed_opportunity(
            cluster,
            optimizer_epoch_id,
            label,
            "ready",
            economic_priority,
        )
        .await?;
    let lease = claim_one(
        &fixture.client,
        cluster,
        &format!("{label}-executor"),
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let conflicts = vec![
        format!("fleet-shared-write-lane:{}:{label}", fixture.prefix),
        format!("vault-write:{}:{label}", fixture.prefix),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &lease,
            &conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    let mut signed = signed_input_for_lease(fixture, &lease, conflicts, label).await?;
    signed.last_valid_block_height = last_valid_block_height;
    let (_, submission) = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            same_mint_input_for_lease(&lease)?,
            &lease,
            target_capacity_input_for_lease(fixture, &lease).await?,
            signed,
        )
        .await?;
    Ok(submission)
}

async fn reconciled_volume_for_cluster(
    fixture: &DatabaseFixture,
    cluster: &str,
) -> Result<(i64, i64, i64), Box<dyn Error>> {
    Ok(sqlx::query_as(
        r#"
        SELECT count(*)::BIGINT,
               COALESCE(sum(decision.amount_raw), 0)::BIGINT,
               COALESCE(sum(opportunity.principal_usd_micros), 0)::BIGINT
        FROM loyal_yield.signed_route_submissions submission
        JOIN loyal_yield.rebalance_opportunities opportunity
          ON opportunity.id = submission.opportunity_id
        JOIN loyal_yield.rebalance_decisions decision
          ON decision.id = submission.decision_id
        WHERE submission.cluster = $1
          AND submission.submission_state = 'reconciled'
        "#,
    )
    .bind(cluster)
    .fetch_one(fixture.client.pool())
    .await?)
}

async fn fee_shard_signed_input_for_lease(
    fixture: &DatabaseFixture,
    lease: &RebalanceOpportunityLease,
    conflict_account_keys: Vec<String>,
    label: &str,
    fee_payer: &str,
    observed_balance_lamports: i64,
    compiled_fee_lamports: i64,
) -> Result<SignedRouteSubmissionInput, Box<dyn Error>> {
    let mut input = signed_input_for_lease(fixture, lease, conflict_account_keys, label).await?;
    input.writable_account_keys[0] = fee_payer.to_owned();
    input.fee_payer = fee_payer.to_owned();
    input.fee_payer_kind = RouteFeePayerKind::FeeOnlyShard;
    input.fee_payer_balance_lamports = Some(observed_balance_lamports);
    input.fee_payer_balance_slot = Some(10_000);
    input.fee_payer_balance_observed_at = Some(Utc::now());
    input.compiled_fee_lamports = compiled_fee_lamports;
    Ok(input)
}

async fn create_runtime_alt_family(
    client: &NeonSqlClient,
    cluster: &str,
    logical_name: &str,
    kind: LookupTableFamilyKind,
    policy_pubkey: &str,
) -> Result<LookupTableFamilyRecord, Box<dyn Error>> {
    Ok(client
        .create_or_validate_lookup_table_family(LookupTableFamilyUpsert {
            cluster: cluster.to_owned(),
            logical_name: logical_name.to_owned(),
            kind,
            desired_state: LookupTableFamilyState::Active,
            planner_version: "fleet-runtime-verifier-v1".to_owned(),
            catalog_version: "fleet-runtime-verifier-v1".to_owned(),
            active_generation: Some(0),
            previous_generation: None,
            rollback_until: None,
            provisioning_authority: policy_pubkey.to_owned(),
            payer: policy_pubkey.to_owned(),
            hard_capacity: 64,
            largest_atomic_expansion: 20,
            safety_margin: 4,
            allocation_high_water: 40,
        })
        .await?)
}

async fn insert_runtime_alt_table(
    client: &NeonSqlClient,
    cluster: &str,
    family: &LookupTableFamilyRecord,
    allocation_kind: LookupTableAllocationKind,
    shard_ordinal: i32,
    policy_pubkey: &str,
) -> Result<loyal_yield_orchestrator::ReusableLookupTableRecord, Box<dyn Error>> {
    Ok(client
        .insert_reusable_lookup_table(ReusableLookupTableInsert {
            cluster: cluster.to_owned(),
            scope: format!(
                "fleet-runtime-verifier:{}:{shard_ordinal}",
                family.logical_name
            ),
            table_address: runtime_random_pubkey(),
            authority: policy_pubkey.to_owned(),
            payer: policy_pubkey.to_owned(),
            family_id: family.id,
            allocation_kind,
            generation: 0,
            shard_ordinal,
            desired_state: LookupTableLifecycle::Active,
            accepting_allocations: true,
            allocation_high_water: family.allocation_high_water,
            mutation_epoch: 0,
            create_signature: None,
        })
        .await?)
}

async fn create_runtime_alt_vault(
    client: &NeonSqlClient,
    label: &str,
    policy_pubkey: &str,
) -> Result<VaultId, Box<dyn Error>> {
    let settings = runtime_random_pubkey();
    let vault_pubkey = runtime_random_pubkey();
    let policy_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_policies
            (settings, authority, policy_seed, policy_account, vault_index,
             vault_pubkey, threshold, last_seen_slot, last_seen_signature)
        VALUES ($1, $2, 0, $3, 0, $4, 1, 10000, $5)
        RETURNING id
        "#,
    )
    .bind(&settings)
    .bind(policy_pubkey)
    .bind(runtime_random_pubkey())
    .bind(&vault_pubkey)
    .bind(format!("fleet-runtime-verifier:{label}"))
    .fetch_one(client.pool())
    .await?;
    let vault_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.managed_vaults
            (settings, vault_index, vault_pubkey, active_policy_id, active)
        VALUES ($1, 0, $2, $3, TRUE)
        RETURNING id
        "#,
    )
    .bind(settings)
    .bind(vault_pubkey)
    .bind(policy_id)
    .fetch_one(client.pool())
    .await?;
    Ok(VaultId(vault_id))
}

async fn enqueue_runtime_alt_extend(
    client: &NeonSqlClient,
    family: &LookupTableFamilyRecord,
    table_id: i64,
    mutation_epoch: i64,
    label: &str,
) -> Result<LookupTableOperationRecord, Box<dyn Error>> {
    Ok(client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("fleet-runtime-verifier:{label}"),
            family_id: family.id,
            route_lookup_table_id: Some(table_id),
            manifest_id: None,
            binding_id: None,
            operation_kind: LookupTableOperationKind::Extend,
            target_generation: None,
            target_shard_ordinal: None,
            operation_context: json!({"source": "fleet_isolated_database_runtime_measurements"}),
            mutation_epoch,
            estimated_fee_lamports: Some(5_000),
            estimated_rent_lamports: Some(0),
            addresses: vec![runtime_random_pubkey()],
        })
        .await?)
}

async fn run_alt_database_runtime_measurements(
    fixture: &DatabaseFixture,
) -> Result<AltDatabaseRuntimeMeasurements, Box<dyn Error>> {
    // These rows intentionally use an immutable-only prefix outside the main
    // fixture cleanup namespace. Catalog revisions, manifests, and spend
    // evidence are audit records and the caller already requires a disposable
    // database whose name contains `fleet_verify`.
    let runtime_prefix = format!(
        "immutable_alt_runtime_{}_{}",
        std::process::id(),
        Utc::now().timestamp_micros()
    );
    let policy_keypair = Keypair::new();
    let policy_pubkey = policy_keypair.pubkey().to_string();

    let plan_cluster = format!("{runtime_prefix}_plan");
    let shared_family = create_runtime_alt_family(
        &fixture.client,
        &plan_cluster,
        "stable-market",
        LookupTableFamilyKind::SharedMarket,
        &policy_pubkey,
    )
    .await?;
    let vault_family = create_runtime_alt_family(
        &fixture.client,
        &plan_cluster,
        "vault-shards",
        LookupTableFamilyKind::VaultShards,
        &policy_pubkey,
    )
    .await?;
    let shared_address = LookupTableManifestAddressRecord {
        address: runtime_random_pubkey(),
        ordinal: 0,
        semantic_class: LookupTableManifestSubject::SharedMarket,
        account_role: "stable_market_fixture".to_owned(),
        is_writable: false,
    };
    let shared_addresses = vec![shared_address.clone()];
    let shared_hash = lookup_table_manifest_address_records_hash(&shared_addresses);
    let catalog = fixture
        .client
        .upsert_shared_market_catalog(SharedMarketCatalogUpsert {
            cluster: plan_cluster.clone(),
            catalog_version: "fleet-runtime-verifier-v1".to_owned(),
            desired_set_hash: shared_hash.clone(),
            enabled_mints_hash: format!("{:x}", Sha256::digest(b"runtime-enabled-mints")),
            reserve_set_hash: format!("{:x}", Sha256::digest(b"runtime-reserve-set")),
            addresses: shared_addresses.clone(),
            source_slot: Some(10_000),
            source_observed_at: Some(Utc::now()),
            source_metadata: json!({"source": "fleet_isolated_database_runtime_measurements"}),
            reason: "isolated runtime measurement".to_owned(),
            updated_by: "fleet-orchestration-verifier".to_owned(),
        })
        .await?;
    let shared_table = insert_runtime_alt_table(
        &fixture.client,
        &plan_cluster,
        &shared_family,
        LookupTableAllocationKind::SharedMarket,
        0,
        &policy_pubkey,
    )
    .await?;
    let shared_table = fixture
        .client
        .replace_confirmed_lookup_table_membership(
            shared_table.id,
            shared_table.mutation_epoch,
            shared_table.mutation_epoch + 1,
            10_002,
            10_001,
            vec![LookupTableMembershipAddress {
                address: shared_address.address.clone(),
                ordinal: 0,
                added_operation_id: None,
                added_slot: 10_001,
                usable_after_slot: 10_002,
                last_verified_slot: 10_002,
                last_verified_at: Utc::now(),
            }],
        )
        .await?;
    fixture
        .client
        .mark_reusable_lookup_table_verification(
            shared_table.id,
            shared_table.mutation_epoch,
            LookupTableLifecycle::Active,
            LookupTableLifecycle::Active,
            true,
            shared_table.address_count,
            10_002,
        )
        .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_shared_market_catalog_heads
        SET target_generation = 0, readiness_state = 'active',
            activated_at = now(), updated_at = now()
        WHERE family_id = $1 AND catalog_revision_id = $2
        "#,
    )
    .bind(shared_family.id)
    .bind(catalog.catalog_revision_id)
    .execute(fixture.client.pool())
    .await?;

    let vault_id = create_runtime_alt_vault(
        &fixture.client,
        &format!("{runtime_prefix}:typed-plan"),
        &policy_pubkey,
    )
    .await?;
    let vault_addresses = vec![LookupTableManifestAddressRecord {
        address: runtime_random_pubkey(),
        ordinal: 0,
        semantic_class: LookupTableManifestSubject::Vault,
        account_role: "vault_fixture".to_owned(),
        is_writable: true,
    }];
    let vault_hash = lookup_table_manifest_address_records_hash(&vault_addresses);
    let request = fixture
        .client
        .upsert_lookup_table_provisioning_request(LookupTableProvisioningRequestUpsert {
            cluster: plan_cluster.clone(),
            vault_id,
            route_fingerprint: format!("{runtime_prefix}:route"),
            requirements_fingerprint: format!("{runtime_prefix}:requirements"),
            shared_manifest_id: None,
            vault_manifest_id: None,
            desired_shared_hash: Some(shared_hash),
            desired_vault_hash: Some(vault_hash),
            shared_addresses,
            vault_addresses,
        })
        .await?;
    let leased_request = fixture
        .client
        .lease_next_lookup_table_provisioning_request(
            &plan_cluster,
            "fleet-runtime-plan-worker",
            Utc::now() + chrono::Duration::minutes(5),
        )
        .await?
        .ok_or("typed provisioner request was not leaseable")?;
    if leased_request.id != request.id {
        return Err("typed provisioner leased the wrong isolated request".into());
    }
    let request_lease = LookupTableOperationLease::new(
        leased_request
            .lease_owner
            .clone()
            .ok_or("typed provisioner request lease has no owner")?,
        leased_request.fencing_token,
        leased_request
            .lease_expires_at
            .ok_or("typed provisioner request lease has no expiry")?,
    )?;
    let rollout_locks_before = lookup_table_rollout_lock_acquisition_count();
    let plan = fixture
        .client
        .plan_lookup_table_provisioning_request(
            &plan_cluster,
            leased_request.id,
            &request_lease,
            LookupTableProvisioningPlanPolicy {
                vault_policy: PackedShardPolicy {
                    hard_capacity: 64,
                    largest_atomic_expansion: 20,
                    safety_margin: 4,
                    per_vault_growth_reservation: 4,
                    max_vault_cohort: 8,
                },
                shared_shard_capacity: 40,
                max_extension_addresses: 20,
                operation_context: json!({
                    "source": "fleet_isolated_database_runtime_measurements",
                    "recent_slot": 10_002,
                }),
                estimated_fee_lamports: Some(5_000),
                estimated_rent_lamports: Some(1_000_000),
            },
        )
        .await?;
    let normal_readiness_global_rollout_lock_acquisitions =
        lookup_table_rollout_lock_acquisition_count().saturating_sub(rollout_locks_before);
    let plan_operations = match &plan.vault_allocation {
        AtomicVaultAllocationResult::BindingReserved { operations, .. }
        | AtomicVaultAllocationResult::CreateQueued { operations, .. } => operations,
        AtomicVaultAllocationResult::Existing { .. } | AtomicVaultAllocationResult::NotRequired => {
            return Err("typed provisioner dry run did not produce physical vault work".into());
        }
    };
    let mut reusable_v2_operation_count = 0_u64;
    let mut legacy_or_exact_route_operation_count = 0_u64;
    let mut policy_identity_matches = shared_family.provisioning_authority == policy_pubkey
        && shared_family.payer == policy_pubkey
        && vault_family.provisioning_authority == policy_pubkey
        && vault_family.payer == policy_pubkey;
    for operation in plan_operations {
        let table_id = operation
            .route_lookup_table_id
            .ok_or("typed provisioner operation has no physical table")?;
        let table = fixture
            .client
            .reusable_lookup_table(table_id)
            .await?
            .ok_or("typed provisioner physical table disappeared")?;
        if matches!(
            table.allocation_kind,
            LookupTableAllocationKind::VaultShard | LookupTableAllocationKind::DedicatedVault
        ) {
            reusable_v2_operation_count += 1;
        } else {
            legacy_or_exact_route_operation_count += 1;
        }
        policy_identity_matches &= table.authority == policy_pubkey && table.payer == policy_pubkey;
    }
    let typed_provisioner_dry_run_plans = 1;
    let reusable_v2_plans = u64::from(
        !plan_operations.is_empty()
            && reusable_v2_operation_count == u64::try_from(plan_operations.len())?,
    );
    let legacy_or_exact_route_alt_plans = u64::from(legacy_or_exact_route_operation_count > 0);

    let lane_cluster = format!("{runtime_prefix}_lanes");
    let lane_family = create_runtime_alt_family(
        &fixture.client,
        &lane_cluster,
        "vault-shards",
        LookupTableFamilyKind::VaultShards,
        &policy_pubkey,
    )
    .await?;
    let lane_a = insert_runtime_alt_table(
        &fixture.client,
        &lane_cluster,
        &lane_family,
        LookupTableAllocationKind::VaultShard,
        0,
        &policy_pubkey,
    )
    .await?;
    let lane_b = insert_runtime_alt_table(
        &fixture.client,
        &lane_cluster,
        &lane_family,
        LookupTableAllocationKind::VaultShard,
        1,
        &policy_pubkey,
    )
    .await?;
    let lane_a_operation = enqueue_runtime_alt_extend(
        &fixture.client,
        &lane_family,
        lane_a.id,
        lane_a.mutation_epoch,
        &format!("{runtime_prefix}:lane-a"),
    )
    .await?;
    let lane_b_operation = enqueue_runtime_alt_extend(
        &fixture.client,
        &lane_family,
        lane_b.id,
        lane_b.mutation_epoch,
        &format!("{runtime_prefix}:lane-b"),
    )
    .await?;
    let first_lane_lease = fixture
        .client
        .lease_next_lookup_table_operation(
            &lane_cluster,
            "fleet-runtime-lane-a",
            Utc::now() + chrono::Duration::minutes(5),
            false,
        )
        .await?
        .ok_or("first independent physical ALT lane was not leaseable")?;
    let second_lane_lease = fixture
        .client
        .lease_next_lookup_table_operation(
            &lane_cluster,
            "fleet-runtime-lane-b",
            Utc::now() + chrono::Duration::minutes(5),
            false,
        )
        .await?
        .ok_or("second independent physical ALT lane was blocked")?;
    let leased_lane_ids = [
        first_lane_lease.operation.id,
        second_lane_lease.operation.id,
    ];
    let independent_physical_alt_lanes_progressed = u64::from(
        first_lane_lease.operation.route_lookup_table_id
            != second_lane_lease.operation.route_lookup_table_id
            && leased_lane_ids.contains(&lane_a_operation.id)
            && leased_lane_ids.contains(&lane_b_operation.id),
    ) * 2;
    policy_identity_matches &= lane_family.provisioning_authority == policy_pubkey
        && lane_family.payer == policy_pubkey
        && lane_a.authority == policy_pubkey
        && lane_a.payer == policy_pubkey
        && lane_b.authority == policy_pubkey
        && lane_b.payer == policy_pubkey;

    let serial_cluster = format!("{runtime_prefix}_serial");
    let serial_family = create_runtime_alt_family(
        &fixture.client,
        &serial_cluster,
        "vault-shards",
        LookupTableFamilyKind::VaultShards,
        &policy_pubkey,
    )
    .await?;
    let serial_table = insert_runtime_alt_table(
        &fixture.client,
        &serial_cluster,
        &serial_family,
        LookupTableAllocationKind::VaultShard,
        0,
        &policy_pubkey,
    )
    .await?;
    let serial_first = enqueue_runtime_alt_extend(
        &fixture.client,
        &serial_family,
        serial_table.id,
        serial_table.mutation_epoch,
        &format!("{runtime_prefix}:serial-first"),
    )
    .await?;
    let _serial_second = enqueue_runtime_alt_extend(
        &fixture.client,
        &serial_family,
        serial_table.id,
        serial_table.mutation_epoch,
        &format!("{runtime_prefix}:serial-second"),
    )
    .await?;
    let serial_first_lease = fixture
        .client
        .lease_next_lookup_table_operation(
            &serial_cluster,
            "fleet-runtime-serial-first",
            Utc::now() + chrono::Duration::minutes(5),
            false,
        )
        .await?
        .ok_or("same-table predecessor was not leaseable")?;
    if serial_first_lease.operation.id != serial_first.id {
        return Err("same-table operation order did not preserve its predecessor".into());
    }
    let premature_successor = fixture
        .client
        .lease_next_lookup_table_operation(
            &serial_cluster,
            "fleet-runtime-serial-successor",
            Utc::now() + chrono::Duration::minutes(5),
            false,
        )
        .await?;
    let same_table_predecessor_violations = u64::from(premature_successor.is_some());
    let serial_fence = lookup_table_operation_lease(&serial_first_lease.operation)?;
    fixture
        .client
        .persist_signed_lookup_table_transaction(
            serial_first.id,
            &serial_fence,
            SignedLookupTableTransaction {
                transaction_signature: format!("{runtime_prefix}:signature"),
                message_hash: format!("{:x}", Sha256::digest(b"runtime-message")),
                recent_blockhash: runtime_random_pubkey(),
                last_valid_block_height: 100_000,
                estimated_fee_lamports: 5_000,
                estimated_rent_lamports: 0,
                estimated_reclaimed_rent_lamports: 0,
            },
        )
        .await?;
    sqlx::query(
        "UPDATE loyal_yield.route_lookup_tables SET mutation_epoch = mutation_epoch + 1 WHERE id = $1",
    )
    .bind(serial_table.id)
    .execute(fixture.client.pool())
    .await?;
    let stale_fence = fixture
        .client
        .grant_lookup_table_provisioner_broadcast_permit(
            &serial_cluster,
            serial_first.id,
            &serial_fence,
            Utc::now() + chrono::Duration::seconds(5),
        )
        .await?;
    let stale_fence_rejections = u64::from(matches!(
        &stale_fence,
        LookupTableProvisionerBroadcastPermitResult::Fenced { error_code, .. }
            if error_code == "lookup_table_identity_changed_before_broadcast"
    ));
    let stale_fence_commits = u64::from(matches!(
        stale_fence,
        LookupTableProvisionerBroadcastPermitResult::Granted { .. }
    ));
    policy_identity_matches &= serial_family.provisioning_authority == policy_pubkey
        && serial_family.payer == policy_pubkey
        && serial_table.authority == policy_pubkey
        && serial_table.payer == policy_pubkey;

    let reciprocal_cluster = format!("{runtime_prefix}_reciprocal_usage_fence");
    let reciprocal_family = create_runtime_alt_family(
        &fixture.client,
        &reciprocal_cluster,
        "stable-market",
        LookupTableFamilyKind::SharedMarket,
        &policy_pubkey,
    )
    .await?;

    // Direction one: a route cannot acquire protection after a physical
    // mutation has entered the durable operation queue.
    let mutation_first_table = insert_runtime_alt_table(
        &fixture.client,
        &reciprocal_cluster,
        &reciprocal_family,
        LookupTableAllocationKind::SharedMarket,
        0,
        &policy_pubkey,
    )
    .await?;
    let mutation_first = enqueue_runtime_alt_extend(
        &fixture.client,
        &reciprocal_family,
        mutation_first_table.id,
        mutation_first_table.mutation_epoch,
        &format!("{runtime_prefix}:mutation-first"),
    )
    .await?;
    let mutation_first_usage = fixture
        .client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: reciprocal_cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
            reference_key: format!("{runtime_prefix}:mutation-first-usage"),
            route_lookup_table_ids: vec![mutation_first_table.id],
            vault_id: None,
            binding_id: None,
            route_fingerprint: Some(format!("{runtime_prefix}:route")),
            requirements_fingerprint: Some(format!("{runtime_prefix}:requirements")),
            expires_at: Utc::now() + chrono::Duration::minutes(15),
        })
        .await;
    let usage_leases_rejected_during_mutation = u64::from(
        mutation_first_usage
            .as_ref()
            .is_err_and(|error| error.to_string().contains("nonterminal mutation operation")),
    );
    sqlx::query(
        "UPDATE loyal_yield.lookup_table_operations SET operation_state = 'cancelled', next_attempt_at = NULL, updated_at = now() WHERE id = $1",
    )
    .bind(mutation_first.id)
    .execute(fixture.client.pool())
    .await?;

    // Direction two: an active route usage lease keeps an unsigned mutator out
    // of the work queue, while a read-only verification remains runnable.
    let usage_first_table = insert_runtime_alt_table(
        &fixture.client,
        &reciprocal_cluster,
        &reciprocal_family,
        LookupTableAllocationKind::SharedMarket,
        1,
        &policy_pubkey,
    )
    .await?;
    fixture
        .client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: reciprocal_cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
            reference_key: format!("{runtime_prefix}:usage-first"),
            route_lookup_table_ids: vec![usage_first_table.id],
            vault_id: None,
            binding_id: None,
            route_fingerprint: Some(format!("{runtime_prefix}:route")),
            requirements_fingerprint: Some(format!("{runtime_prefix}:requirements")),
            expires_at: Utc::now() + chrono::Duration::minutes(15),
        })
        .await?;
    let usage_verify = fixture
        .client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("fleet-runtime-verifier:{runtime_prefix}:usage-verify"),
            family_id: reciprocal_family.id,
            route_lookup_table_id: Some(usage_first_table.id),
            manifest_id: None,
            binding_id: None,
            operation_kind: LookupTableOperationKind::Verify,
            target_generation: None,
            target_shard_ordinal: None,
            operation_context: json!({"source": "fleet_isolated_database_runtime_measurements"}),
            mutation_epoch: usage_first_table.mutation_epoch,
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            addresses: Vec::new(),
        })
        .await?;
    let usage_mutation = enqueue_runtime_alt_extend(
        &fixture.client,
        &reciprocal_family,
        usage_first_table.id,
        usage_first_table.mutation_epoch,
        &format!("{runtime_prefix}:usage-mutation"),
    )
    .await?;
    let usage_verify_lease = fixture
        .client
        .lease_next_lookup_table_operation(
            &reciprocal_cluster,
            "fleet-runtime-usage-verify",
            Utc::now() + chrono::Duration::minutes(5),
            false,
        )
        .await?;
    let verify_operations_leased_during_usage = u64::from(
        usage_verify_lease
            .as_ref()
            .is_some_and(|leased| leased.operation.id == usage_verify.id),
    );
    sqlx::query(
        "UPDATE loyal_yield.lookup_table_operations SET operation_state = 'cancelled', lease_owner = NULL, lease_expires_at = NULL, next_attempt_at = NULL, updated_at = now() WHERE id = $1",
    )
    .bind(usage_verify.id)
    .execute(fixture.client.pool())
    .await?;
    let usage_mutation_lease = fixture
        .client
        .lease_next_lookup_table_operation(
            &reciprocal_cluster,
            "fleet-runtime-usage-mutation",
            Utc::now() + chrono::Duration::minutes(5),
            false,
        )
        .await?;
    let mutating_operations_leased_during_usage = u64::from(
        usage_mutation_lease
            .as_ref()
            .is_some_and(|leased| leased.operation.id == usage_mutation.id),
    );

    // The final pre-broadcast fence also rejects a legacy/inconsistent race
    // in which protection appears after durable signing.
    let broadcast_fence_cluster = format!("{runtime_prefix}_broadcast_usage_fence");
    let broadcast_fence_family = create_runtime_alt_family(
        &fixture.client,
        &broadcast_fence_cluster,
        "vault-shards",
        LookupTableFamilyKind::VaultShards,
        &policy_pubkey,
    )
    .await?;
    let broadcast_fence_table = insert_runtime_alt_table(
        &fixture.client,
        &broadcast_fence_cluster,
        &broadcast_fence_family,
        LookupTableAllocationKind::VaultShard,
        0,
        &policy_pubkey,
    )
    .await?;
    let broadcast_fence_operation = enqueue_runtime_alt_extend(
        &fixture.client,
        &broadcast_fence_family,
        broadcast_fence_table.id,
        broadcast_fence_table.mutation_epoch,
        &format!("{runtime_prefix}:broadcast-fence"),
    )
    .await?;
    let broadcast_fence_leased = fixture
        .client
        .lease_next_lookup_table_operation(
            &broadcast_fence_cluster,
            "fleet-runtime-broadcast-fence",
            Utc::now() + chrono::Duration::minutes(5),
            false,
        )
        .await?
        .ok_or("broadcast-fence mutation was not leaseable before usage existed")?;
    if broadcast_fence_leased.operation.id != broadcast_fence_operation.id {
        return Err("broadcast-fence scenario leased the wrong operation".into());
    }
    let broadcast_fence_lease = lookup_table_operation_lease(&broadcast_fence_leased.operation)?;
    fixture
        .client
        .persist_signed_lookup_table_transaction(
            broadcast_fence_operation.id,
            &broadcast_fence_lease,
            SignedLookupTableTransaction {
                transaction_signature: format!("{runtime_prefix}:broadcast-fence-signature"),
                message_hash: format!("{:x}", Sha256::digest(b"usage-fence-message")),
                recent_blockhash: runtime_random_pubkey(),
                last_valid_block_height: 100_000,
                estimated_fee_lamports: 5_000,
                estimated_rent_lamports: 0,
                estimated_reclaimed_rent_lamports: 0,
            },
        )
        .await?;
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.lookup_table_usage_leases
            (cluster, lease_kind, reference_key, route_lookup_table_id,
             route_fingerprint, requirements_fingerprint, expires_at)
        VALUES ($1, 'prepared_transaction', $2, $3, $4, $5, $6)
        "#,
    )
    .bind(&broadcast_fence_cluster)
    .bind(format!("{runtime_prefix}:late-usage"))
    .bind(broadcast_fence_table.id)
    .bind(format!("{runtime_prefix}:route"))
    .bind(format!("{runtime_prefix}:requirements"))
    .bind(Utc::now() + chrono::Duration::minutes(15))
    .execute(fixture.client.pool())
    .await?;
    let usage_fence_permit = fixture
        .client
        .grant_lookup_table_provisioner_broadcast_permit(
            &broadcast_fence_cluster,
            broadcast_fence_operation.id,
            &broadcast_fence_lease,
            Utc::now() + chrono::Duration::seconds(5),
        )
        .await?;
    let usage_fence_broadcast_rejections = u64::from(matches!(
        &usage_fence_permit,
        LookupTableProvisionerBroadcastPermitResult::Fenced { error_code, .. }
            if error_code == "lookup_table_usage_lease_active_before_broadcast"
    ));
    let usage_fence_broadcast_commits = u64::from(matches!(
        usage_fence_permit,
        LookupTableProvisionerBroadcastPermitResult::Granted { .. }
    ));
    policy_identity_matches &= reciprocal_family.provisioning_authority == policy_pubkey
        && reciprocal_family.payer == policy_pubkey
        && broadcast_fence_family.provisioning_authority == policy_pubkey
        && broadcast_fence_family.payer == policy_pubkey
        && mutation_first_table.authority == policy_pubkey
        && usage_first_table.authority == policy_pubkey
        && broadcast_fence_table.authority == policy_pubkey;

    Ok(AltDatabaseRuntimeMeasurements {
        typed_provisioner_dry_run_plans,
        reusable_v2_plans,
        legacy_or_exact_route_alt_plans,
        normal_readiness_global_rollout_lock_acquisitions,
        independent_physical_alt_lanes_progressed,
        same_table_predecessor_violations,
        stale_fence_commits,
        stale_fence_rejections,
        usage_leases_rejected_during_mutation,
        mutating_operations_leased_during_usage,
        verify_operations_leased_during_usage,
        usage_fence_broadcast_commits,
        usage_fence_broadcast_rejections,
        alt_authority_payer_identity_consistent: policy_identity_matches,
        policy_pubkey,
    })
}

fn missing_cross_mint_columns(
    columns: &BTreeMap<String, BTreeSet<String>>,
    table: &str,
    required: &[&str],
) -> Vec<String> {
    let available = columns.get(table);
    required
        .iter()
        .filter(|column| available.is_none_or(|available| !available.contains(**column)))
        .map(|column| format!("loyal_yield.{table}.{column}"))
        .collect()
}

struct ActivatedCrossMintFixture {
    opportunity_lease: RebalanceOpportunityLease,
    movement: CrossMintMovementRecord,
    capacity: TargetCapacityReservationInput,
    policy_bindings: CrossMintPolicyBindings,
    reservation_id: i64,
}

fn cross_mint_fixture_policy_bindings(
    opportunity: &RebalanceOpportunityRecord,
) -> Result<CrossMintPolicyBindings, Box<dyn Error>> {
    CrossMintPolicyBindings::from_execution_plan(&opportunity.execution_plan).map_err(Into::into)
}

async fn seed_cross_mint_ready_opportunity(
    fixture: &DatabaseFixture,
    cluster: &str,
    epoch_id: i64,
    label: &str,
    economic_priority: i64,
) -> Result<SeededOpportunity, Box<dyn Error>> {
    let seeded = fixture
        .seed_opportunity(cluster, epoch_id, label, "ready", economic_priority)
        .await?;
    let policy = sqlx::query(
        r#"
        SELECT vault.settings, vault.vault_index, vault.vault_pubkey,
               policy.authority, policy.policy_account,
               policy.delegated_signers[1] AS delegated_signer
        FROM loyal_yield.rebalance_opportunities opportunity
        JOIN loyal_yield.managed_vaults vault ON vault.id = opportunity.vault_id
        JOIN loyal_yield.route_policies policy ON policy.id = vault.active_policy_id
        WHERE opportunity.id = $1
        "#,
    )
    .bind(seeded.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let settings: String = policy.try_get("settings")?;
    let vault_index: i16 = policy.try_get("vault_index")?;
    let vault_pubkey: String = policy.try_get("vault_pubkey")?;
    let authority: String = policy.try_get("authority")?;
    let base_policy_account: String = policy.try_get("policy_account")?;
    let delegated_signer: String = policy.try_get("delegated_signer")?;
    let swap_policy_account = format!("swap-policy:{}:{}", fixture.prefix, label);
    let token_2022_swap_policy_account =
        format!("swap-policy-token-2022:{}:{}", fixture.prefix, label);
    let deposit_policy_account = format!("deposit-policy:{}:{}", fixture.prefix, label);
    let base_signature = format!("base-finalized:{}:{}", fixture.prefix, label);
    let swap_signature = format!("swap-finalized:{}:{}", fixture.prefix, label);
    let token_2022_swap_signature =
        format!("swap-token-2022-finalized:{}:{}", fixture.prefix, label);
    let deposit_signature = format!("deposit-finalized:{}:{}", fixture.prefix, label);
    sqlx::query(
        r#"
        UPDATE loyal_yield.route_policies
        SET cluster = $2,
            source_commitment = 'finalized',
            finalized_eligible = TRUE,
            route_modes = ARRAY['same_mint_kamino']::TEXT[],
            stable_mints = ARRAY['USDC']::TEXT[],
            kamino_liquidity_mints = ARRAY['USDC']::TEXT[],
            last_seen_slot = 10000,
            last_seen_signature = $3
        WHERE policy_account = $1
        "#,
    )
    .bind(&base_policy_account)
    .bind(cluster)
    .bind(&base_signature)
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.route_policies
            (settings, authority, policy_seed, policy_account, vault_index,
             vault_pubkey, delegated_signers, threshold, route_modes,
             stable_mints, kamino_markets, kamino_liquidity_mints,
             universe_preset, risk_profile, swap_lanes, active,
             last_seen_slot, last_seen_signature, cluster, source_commitment,
             finalized_eligible)
        VALUES
            ($1, $2, 3, $3, $4, $5, ARRAY[$6]::TEXT[], 1,
             ARRAY['same_mint_kamino']::TEXT[], ARRAY['USDT']::TEXT[],
             ARRAY['fixture-market']::TEXT[], ARRAY['USDT']::TEXT[],
             NULL, 'safe', '[]'::JSONB, TRUE, 10000, $7, $8,
             'finalized', TRUE)
        "#,
    )
    .bind(&settings)
    .bind(&authority)
    .bind(&deposit_policy_account)
    .bind(vault_index)
    .bind(&vault_pubkey)
    .bind(&delegated_signer)
    .bind(&deposit_signature)
    .bind(cluster)
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.cross_mint_swap_policies
            (cluster, settings, authority, policy_seed, policy_account,
             vault_index, vault_pubkey, delegated_signer, source_shard,
             max_slippage_bps, daily_source_mint_spending_cap,
             manifest_fingerprint,
             active, start_eligible, last_mutation,
             source_commitment, last_seen_slot, last_seen_signature)
        VALUES
            ($1, $2, $3, 2, $4, $5, $6, $7, 'classic',
             50, 1000000000, repeat('a', 64), TRUE, TRUE,
             'create', 'finalized', 10000, $8),
            ($1, $2, $3, 3, $9, $5, $6, $7, 'token_2022',
             50, 1000000000, repeat('b', 64), TRUE, TRUE,
             'create', 'finalized', 10000, $10),
            ($1, $2, $3, 20, $4 || ':historical', $5, $6, $7, 'classic',
             50, 1000000000, repeat('a', 64), TRUE, TRUE,
             'create', 'finalized', 9999, $8 || ':historical'),
            ($1, $2, $3, 21, $9 || ':historical', $5, $6, $7, 'token_2022',
             50, 1000000000, repeat('b', 64), TRUE, TRUE,
             'create', 'finalized', 9999, $10 || ':historical')
        "#,
    )
    .bind(cluster)
    .bind(&settings)
    .bind(&authority)
    .bind(&swap_policy_account)
    .bind(vault_index)
    .bind(&vault_pubkey)
    .bind(&delegated_signer)
    .bind(&swap_signature)
    .bind(&token_2022_swap_policy_account)
    .bind(&token_2022_swap_signature)
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.cross_mint_vault_opt_ins
            (cluster, settings, vault_index, vault_pubkey, enabled,
             classic_policy_account, classic_policy_seed,
             token_2022_policy_account, token_2022_policy_seed,
             max_slippage_bps, daily_source_mint_spending_cap)
        VALUES ($1, $2, $3, $4, TRUE, $5, 2, $6, 3, 50, 1000000000)
        "#,
    )
    .bind(cluster)
    .bind(&settings)
    .bind(vault_index)
    .bind(&vault_pubkey)
    .bind(&swap_policy_account)
    .bind(&token_2022_swap_policy_account)
    .execute(fixture.client.pool())
    .await?;
    let updated = sqlx::query(
        r#"
        UPDATE loyal_yield.rebalance_opportunities
        SET liquidity_mint = 'USDT',
            source_liquidity_mint = 'USDC',
            target_liquidity_mint = 'USDT',
            estimated_cost_lamports = 50000,
            execution_plan = execution_plan || jsonb_build_object(
                'kind', 'cross_mint_jupiter',
                'source_liquidity_mint', 'USDC',
                'target_liquidity_mint', 'USDT',
                'swap_lane', 'jupiter_exact_in',
                'policy_bindings', jsonb_build_object(
                    'settings', $2,
                    'vault_index', $3,
                    'vault_pubkey', $4,
                    'delegated_signer', $5,
                    'withdraw', jsonb_build_object(
                        'policy_account', $6,
                        'observed_slot', 10000,
                        'observed_signature', $7,
                        'source_commitment', 'finalized',
                        'constraint_index', 0
                    ),
                    'swap', jsonb_build_object(
                        'policy_account', $8,
                        'source_shard', 'classic',
                        'enrollment_generation', 1,
                        'observed_slot', 10000,
                        'observed_signature', $9,
                        'source_commitment', 'finalized',
                        'max_slippage_bps', 50,
                        'daily_source_mint_spending_cap', 1000000000,
                        'manifest_fingerprint', repeat('a', 64)
                    ),
                    'deposit', jsonb_build_object(
                        'policy_account', $10,
                        'observed_slot', 10000,
                        'observed_signature', $11,
                        'source_commitment', 'finalized',
                        'constraint_index', 1
                    )
                )
            )
        WHERE id = $1 AND opportunity_state = 'ready'
        "#,
    )
    .bind(seeded.id)
    .bind(&settings)
    .bind(i64::from(vault_index))
    .bind(&vault_pubkey)
    .bind(&delegated_signer)
    .bind(&base_policy_account)
    .bind(&base_signature)
    .bind(&swap_policy_account)
    .bind(&swap_signature)
    .bind(&deposit_policy_account)
    .bind(&deposit_signature)
    .execute(fixture.client.pool())
    .await?;
    if updated.rows_affected() != 1 {
        return Err("cross-mint fixture did not update exactly one ready opportunity".into());
    }
    Ok(seeded)
}

async fn set_cross_mint_gates(
    fixture: &DatabaseFixture,
    cluster: &str,
    start_new_movements: bool,
    continue_or_recover_existing: bool,
) -> Result<(), Box<dyn Error>> {
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.cross_mint_movement_controls
            (cluster, start_new_movements, continue_or_recover_existing,
             generation, updated_by)
        VALUES ($1, $2, $3, 1, 'fleet-orchestration-verifier')
        ON CONFLICT (cluster) DO UPDATE
        SET start_new_movements = EXCLUDED.start_new_movements,
            continue_or_recover_existing = EXCLUDED.continue_or_recover_existing,
            generation = loyal_yield.cross_mint_movement_controls.generation + 1,
            updated_by = EXCLUDED.updated_by,
            updated_at = now()
        "#,
    )
    .bind(cluster)
    .bind(start_new_movements)
    .bind(continue_or_recover_existing)
    .execute(fixture.client.pool())
    .await?;
    Ok(())
}

async fn cross_mint_capacity_input_for_lease(
    fixture: &DatabaseFixture,
    lease: &RebalanceOpportunityLease,
) -> Result<TargetCapacityReservationInput, Box<dyn Error>> {
    let observation = TargetCapacityObservation {
        cluster: lease.opportunity.cluster.clone(),
        target_reserve: lease.opportunity.target_reserve.clone(),
        liquidity_mint: lease.opportunity.target_liquidity_mint.clone(),
        observed_supply_usd_micros: 20_000_000_000,
        observed_slot: 10_000,
        maximum_inflight_usd_micros: 1_000_000_000,
    };
    Ok(target_capacity_input_from_projection(
        lease,
        fixture.client.observe_target_capacity(observation).await?,
    ))
}

async fn activate_cross_mint_fixture(
    fixture: &DatabaseFixture,
    suffix: &str,
) -> Result<ActivatedCrossMintFixture, Box<dyn Error>> {
    let cluster = fixture.cluster(suffix);
    let epoch_id = fixture.seed_epoch(&cluster).await?;
    seed_cross_mint_ready_opportunity(fixture, &cluster, epoch_id, suffix, 50_000).await?;
    set_cross_mint_gates(fixture, &cluster, true, true).await?;
    let opportunity_lease = claim_one(
        &fixture.client,
        &cluster,
        &format!("{suffix}-activate"),
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let capacity = cross_mint_capacity_input_for_lease(fixture, &opportunity_lease).await?;
    let policy_bindings = cross_mint_fixture_policy_bindings(&opportunity_lease.opportunity)?;
    let movement = fixture
        .client
        .activate_cross_mint_movement(
            &opportunity_lease,
            CrossMintMovementActivationInput {
                capacity: capacity.clone(),
                initial_withdraw_compiled_fee_lamports: 5_000,
                preflight_certification: json!({
                    "kind": "cross_mint_preflight",
                    "fixture": suffix,
                }),
                policy_bindings: policy_bindings.clone(),
            },
        )
        .await?;
    let reservation_id: i64 = sqlx::query_scalar(
        "SELECT id FROM loyal_yield.target_capacity_reservations WHERE decision_id = $1",
    )
    .bind(movement.decision_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    Ok(ActivatedCrossMintFixture {
        opportunity_lease,
        movement,
        capacity,
        policy_bindings,
        reservation_id,
    })
}

async fn claim_cross_mint_continuation(
    fixture: &DatabaseFixture,
    cluster: &str,
    owner: &str,
) -> Result<CrossMintContinuationLease, Box<dyn Error>> {
    let crashed = fixture
        .client
        .claim_cross_mint_continuation(cluster, owner, 60)
        .await?
        .ok_or_else(|| format!("no cross-mint continuation was claimable in {cluster}"))?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_decisions SET continuation_lease_expires_at = now() - interval '1 second' WHERE id = $1 AND continuation_lease_owner = $2 AND continuation_fencing_token = $3",
    )
    .bind(crashed.movement.decision_id.as_i64())
    .bind(&crashed.owner)
    .bind(crashed.fencing_token)
    .execute(fixture.client.pool())
    .await?;
    let restarted = fixture
        .client
        .claim_cross_mint_continuation(cluster, &format!("{owner}-restart"), 60)
        .await?
        .ok_or_else(|| {
            format!("crashed cross-mint continuation was not reclaimable in {cluster}")
        })?;
    if restarted.fencing_token <= crashed.fencing_token
        || restarted.control_generation != crashed.control_generation
        || restarted.movement.decision_id != crashed.movement.decision_id
    {
        return Err("cross-mint before-persistence restart lost its durable fence".into());
    }
    Ok(restarted)
}

// Keeping each signed-leg invariant explicit at verifier call sites makes the
// scenario evidence easier to audit than hiding it in a mutable fixture.
#[allow(clippy::too_many_arguments)]
async fn cross_mint_leg_input(
    fixture: &DatabaseFixture,
    opportunity_lease: &RebalanceOpportunityLease,
    continuation: &CrossMintContinuationLease,
    label: &str,
    leg: CrossMintMovementLeg,
    purpose: CrossMintLegPurpose,
    generation: i64,
    expected_effect: CrossMintExpectedEffect,
) -> Result<CrossMintLegPublicationInput, Box<dyn Error>> {
    let conflicts = vec![
        format!("fleet-shared-write-lane:{}:{label}", fixture.prefix),
        format!(
            "vault-write:{}:{label}",
            continuation.movement.decision_id.as_i64()
        ),
    ];
    let mut submission =
        signed_input_for_lease(fixture, opportunity_lease, conflicts, label).await?;
    submission.decision_id = Some(continuation.movement.decision_id);
    submission.executor_owner = continuation.owner.clone();
    submission.executor_fencing_token = continuation.fencing_token;
    let expected_balance_anchors = CrossMintBalanceAnchors {
        debit: expected_effect
            .debit
            .as_ref()
            .map(|delta| TokenBalanceAnchor {
                mint: delta.mint.clone(),
                token_account: delta.token_account.clone(),
                amount_raw: continuation
                    .movement
                    .custody_observed_balance_raw
                    .unwrap_or(delta.amount_raw),
            }),
        credit: expected_effect
            .credit_mint
            .as_ref()
            .map(|mint| TokenBalanceAnchor {
                mint: mint.clone(),
                token_account: expected_effect
                    .credit_token_account
                    .clone()
                    .expect("validated cross-mint credit account"),
                amount_raw: 23,
            }),
        kamino_position: match leg {
            CrossMintMovementLeg::Withdraw => Some(KaminoPositionAnchor {
                reserve: continuation.movement.source_reserve.clone(),
                market: format!("kamino-market:{}", fixture.prefix),
                obligation: format!(
                    "kamino-obligation:{}:{}",
                    fixture.prefix,
                    continuation.movement.decision_id.as_i64()
                ),
                obligation_exists: true,
                deposited_collateral_amount_raw: continuation.movement.planned_amount_raw.max(1),
                minimum_deposit_amount_raw: None,
            }),
            CrossMintMovementLeg::Deposit => Some(KaminoPositionAnchor {
                reserve: if purpose == CrossMintLegPurpose::RecoverSource {
                    continuation.movement.source_reserve.clone()
                } else {
                    continuation.movement.active_target_reserve.clone()
                },
                market: format!("kamino-market:{}", fixture.prefix),
                obligation: format!(
                    "kamino-obligation:{}:{}",
                    fixture.prefix,
                    continuation.movement.decision_id.as_i64()
                ),
                obligation_exists: true,
                deposited_collateral_amount_raw: 0,
                minimum_deposit_amount_raw: None,
            }),
            CrossMintMovementLeg::Swap => None,
        },
    };
    let bindings = cross_mint_fixture_policy_bindings(&opportunity_lease.opportunity)?;
    let policy_account = match (leg, purpose) {
        (CrossMintMovementLeg::Withdraw, _) => bindings.withdraw.policy_account,
        (CrossMintMovementLeg::Swap, _) => bindings.swap.policy_account,
        (CrossMintMovementLeg::Deposit, CrossMintLegPurpose::RecoverSource) => {
            bindings.withdraw.policy_account
        }
        (CrossMintMovementLeg::Deposit, _) => bindings.deposit.policy_account,
    };
    Ok(CrossMintLegPublicationInput {
        leg,
        purpose,
        generation,
        policy_account,
        expected_effect,
        expected_balance_anchors,
        submission,
    })
}

async fn lease_pending_cross_mint_submission(
    fixture: &DatabaseFixture,
    cluster: &str,
    owner: &str,
    submission_id: i64,
) -> Result<SignedRouteSubmissionLease, Box<dyn Error>> {
    let leases = fixture
        .client
        .lease_pending_signed_route_submissions(
            cluster,
            owner,
            16,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?;
    leases
        .into_iter()
        .find(|lease| lease.submission.id == submission_id)
        .ok_or_else(|| {
            format!("signed cross-mint submission {submission_id} was not claimable").into()
        })
}

async fn lease_reconciliation_cross_mint_submission(
    fixture: &DatabaseFixture,
    cluster: &str,
    owner: &str,
    submission_id: i64,
) -> Result<SignedRouteSubmissionLease, Box<dyn Error>> {
    let leases = fixture
        .client
        .lease_reconciliation_pending_signed_route_submissions(
            cluster,
            owner,
            16,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?;
    leases
        .into_iter()
        .find(|lease| lease.submission.id == submission_id)
        .ok_or_else(|| {
            format!(
                "reconciliation-pending cross-mint submission {submission_id} was not claimable"
            )
            .into()
        })
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct CrossMintCrashWindowEvidence {
    before_persistence_fence_reclaimed: bool,
    persisted_prebroadcast_exact_wire: bool,
    broadcast_prestatus_exact_wire: bool,
    batch_finality_bypass_rejected: bool,
    finalized_prereconcile_receipt_persisted: bool,
    reconciled_precontinuation_projection_persisted: bool,
}

impl CrossMintCrashWindowEvidence {
    fn passed(&self) -> bool {
        self.before_persistence_fence_reclaimed
            && self.persisted_prebroadcast_exact_wire
            && self.broadcast_prestatus_exact_wire
            && self.batch_finality_bypass_rejected
            && self.finalized_prereconcile_receipt_persisted
            && self.reconciled_precontinuation_projection_persisted
    }
}

async fn finalize_and_reconcile_cross_mint_leg(
    fixture: &DatabaseFixture,
    cluster: &str,
    submission_id: i64,
    finalized_slot: i64,
    effect: CrossMintReconciledEffect,
) -> Result<(CrossMintMovementRecord, bool, CrossMintCrashWindowEvidence), Box<dyn Error>> {
    let owner = format!("reconcile-{submission_id}");
    let prebroadcast = lease_pending_cross_mint_submission(
        fixture,
        cluster,
        &format!("prebroadcast-{submission_id}"),
        submission_id,
    )
    .await?;
    let persisted_bytes = prebroadcast.submission.signed_transaction.clone();
    let initial_broadcast_count = prebroadcast.submission.broadcast_count;
    let now = Utc::now();
    fixture
        .client
        .advance_signed_route_submission(
            &prebroadcast,
            SignedRouteSubmissionAdvance::Deferred {
                checked_at: now,
                next_poll_at: now,
                error_detail: Some("verifier_persisted_prebroadcast_restart".to_owned()),
            },
        )
        .await?;
    let confirmation = lease_pending_cross_mint_submission(
        fixture,
        cluster,
        &format!("confirm-{submission_id}"),
        submission_id,
    )
    .await?;
    let persisted_prebroadcast_exact_wire = confirmation.submission.signed_transaction
        == persisted_bytes
        && confirmation.submission.broadcast_count == initial_broadcast_count;
    let mut prepared = fixture
        .client
        .prepare_signed_route_broadcast_batch(&[confirmation], Utc::now())
        .await?;
    let confirmation = prepared
        .pop()
        .ok_or("cross-mint broadcast preparation returned no submission")?;
    if !prepared.is_empty() {
        return Err("cross-mint broadcast preparation returned duplicate submissions".into());
    }
    fixture
        .client
        .advance_signed_route_submission(
            &confirmation,
            SignedRouteSubmissionAdvance::Submitted {
                checked_at: Utc::now(),
                observed_slot: Some(finalized_slot.saturating_sub(1)),
                next_poll_at: Utc::now(),
                broadcasted: false,
            },
        )
        .await?;
    let confirmation = lease_pending_cross_mint_submission(
        fixture,
        cluster,
        &format!("status-restart-{submission_id}"),
        submission_id,
    )
    .await?;
    if confirmation.submission.signed_transaction != persisted_bytes
        || confirmation.submission.broadcast_count != initial_broadcast_count + 1
    {
        return Err(
            "broadcast/prestatus restart changed exact signed bytes or broadcast count".into(),
        );
    }
    fixture
        .client
        .advance_signed_route_submission(
            &confirmation,
            SignedRouteSubmissionAdvance::Confirmed {
                checked_at: Utc::now(),
                confirmed_slot: finalized_slot,
            },
        )
        .await?;
    let batch_finality_bypass_rejected = fixture
        .client
        .confirm_signed_route_submission_batch(
            &[(confirmation.clone(), finalized_slot)],
            Utc::now(),
        )
        .await
        .is_err();
    let rejected_before_finality = fixture
        .client
        .advance_signed_route_submission(
            &confirmation,
            SignedRouteSubmissionAdvance::ReconciliationPending,
        )
        .await
        .is_err();
    let finalized = fixture
        .client
        .advance_signed_route_submission(
            &confirmation,
            SignedRouteSubmissionAdvance::Finalized {
                checked_at: Utc::now(),
                finalized_slot,
            },
        )
        .await?;
    if finalized.finalized_slot != Some(finalized_slot) {
        return Err("finalized/prereconcile checkpoint did not persist finality".into());
    }
    sqlx::query(
        r#"
        UPDATE loyal_yield.signed_route_submissions
        SET confirmation_lease_expires_at = now() - interval '1 second'
        WHERE id = $1
        "#,
    )
    .bind(submission_id)
    .execute(fixture.client.pool())
    .await?;
    let confirmation = lease_pending_cross_mint_submission(
        fixture,
        cluster,
        &format!("finality-restart-{submission_id}"),
        submission_id,
    )
    .await?;
    if confirmation.submission.signed_transaction != persisted_bytes
        || confirmation.submission.finalized_slot != Some(finalized_slot)
    {
        return Err("finalized/prereconcile restart lost exact wire or finality receipt".into());
    }
    fixture
        .client
        .advance_signed_route_submission(
            &confirmation,
            SignedRouteSubmissionAdvance::ReconciliationPending,
        )
        .await?;
    let reconciliation =
        lease_reconciliation_cross_mint_submission(fixture, cluster, &owner, submission_id).await?;
    let expected_anchors: CrossMintBalanceAnchors =
        serde_json::from_value(reconciliation.submission.expected_balance_anchors.clone())?;
    let reconciled_balance_anchors = CrossMintBalanceAnchors {
        debit: expected_anchors
            .debit
            .as_ref()
            .map(|pre| TokenBalanceAnchor {
                mint: pre.mint.clone(),
                token_account: pre.token_account.clone(),
                amount_raw: pre.amount_raw
                    - effect
                        .debit
                        .as_ref()
                        .expect("expected debit fixture")
                        .amount_raw,
            }),
        credit: expected_anchors
            .credit
            .as_ref()
            .map(|pre| TokenBalanceAnchor {
                mint: pre.mint.clone(),
                token_account: pre.token_account.clone(),
                amount_raw: pre.amount_raw
                    + effect
                        .credit
                        .as_ref()
                        .expect("expected credit fixture")
                        .amount_raw,
            }),
        kamino_position: expected_anchors.kamino_position.as_ref().map(|pre| {
            let withdrawal = effect.debit.is_none() && effect.credit.is_some();
            KaminoPositionAnchor {
                reserve: pre.reserve.clone(),
                market: pre.market.clone(),
                obligation: pre.obligation.clone(),
                obligation_exists: !withdrawal,
                deposited_collateral_amount_raw: if withdrawal {
                    0
                } else {
                    pre.deposited_collateral_amount_raw + 1
                },
                minimum_deposit_amount_raw: (!withdrawal).then_some(2),
            }
        }),
    };
    let movement = fixture
        .client
        .reconcile_cross_mint_leg(
            &reconciliation,
            CrossMintLegReconciliationInput {
                finalized_slot,
                effect,
                reconciled_balance_anchors,
            },
        )
        .await?;
    let durable_movement = fixture
        .client
        .cross_mint_movement(movement.decision_id)
        .await?;
    let continuation_attempt_count: i32 = sqlx::query_scalar(
        "SELECT continuation_attempt_count FROM loyal_yield.rebalance_decisions WHERE id = $1",
    )
    .bind(movement.decision_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let crash_windows = CrossMintCrashWindowEvidence {
        before_persistence_fence_reclaimed: continuation_attempt_count >= 2
            && reconciliation.submission.executor_fencing_token >= 2,
        persisted_prebroadcast_exact_wire,
        broadcast_prestatus_exact_wire: confirmation.submission.signed_transaction
            == persisted_bytes,
        batch_finality_bypass_rejected,
        finalized_prereconcile_receipt_persisted: confirmation.submission.finalized_slot
            == Some(finalized_slot),
        reconciled_precontinuation_projection_persisted: durable_movement.custody_version
            == movement.custody_version
            && durable_movement.custody_amount_raw == movement.custody_amount_raw
            && durable_movement.custody_account == movement.custody_account
            && durable_movement.terminal_outcome == movement.terminal_outcome,
    };
    Ok((movement, rejected_before_finality, crash_windows))
}

async fn cross_mint_capacity_state(
    fixture: &DatabaseFixture,
    decision_id: i64,
) -> Result<(i64, String, String, i64), Box<dyn Error>> {
    Ok(sqlx::query_as(
        r#"
        SELECT id, reservation_state::TEXT, target_reserve, reservation_generation
        FROM loyal_yield.target_capacity_reservations
        WHERE decision_id = $1
        "#,
    )
    .bind(decision_id)
    .fetch_one(fixture.client.pool())
    .await?)
}

async fn cross_mint_movement_subchecks(
    fixture: &DatabaseFixture,
    same_mint_regression_passed: bool,
    same_mint_regression_evidence: Value,
) -> Result<Vec<Subcheck>, Box<dyn Error>> {
    let column_rows = sqlx::query(
        r#"
        SELECT table_name, column_name
        FROM information_schema.columns
        WHERE table_schema = 'loyal_yield'
          AND table_name IN (
              'rebalance_opportunities',
              'rebalance_decisions',
              'signed_route_submissions',
              'target_capacity_reservations',
              'cross_mint_movement_controls',
              'cross_mint_no_effect_receipts',
              'cross_mint_swap_policies',
              'cross_mint_vault_opt_ins'
          )
        ORDER BY table_name, ordinal_position
        "#,
    )
    .fetch_all(fixture.client.pool())
    .await?;
    let mut columns = BTreeMap::<String, BTreeSet<String>>::new();
    for row in column_rows {
        columns
            .entry(row.try_get("table_name")?)
            .or_default()
            .insert(row.try_get("column_name")?);
    }

    let index_rows = sqlx::query(
        r#"
        SELECT tablename, indexname
        FROM pg_indexes
        WHERE schemaname = 'loyal_yield'
          AND tablename IN (
              'rebalance_decisions',
              'signed_route_submissions',
              'target_capacity_reservations',
              'cross_mint_swap_policies',
              'cross_mint_vault_opt_ins'
          )
        ORDER BY tablename, indexname
        "#,
    )
    .fetch_all(fixture.client.pool())
    .await?;
    let indexes = index_rows
        .iter()
        .map(|row| {
            Ok(json!({
                "table": row.try_get::<String, _>("tablename")?,
                "index": row.try_get::<String, _>("indexname")?,
            }))
        })
        .collect::<Result<Vec<_>, sqlx::Error>>()?;
    let index_names = index_rows
        .iter()
        .map(|row| row.try_get::<String, _>("indexname"))
        .collect::<Result<BTreeSet<_>, sqlx::Error>>()?;

    let movement_migration = sqlx::query_as::<_, (String, String)>(
        "SELECT name, checksum FROM loyal_yield.schema_migrations WHERE version = 35",
    )
    .fetch_optional(fixture.client.pool())
    .await?;
    let capability_migration = sqlx::query_as::<_, (String, String)>(
        "SELECT name, checksum FROM loyal_yield.schema_migrations WHERE version = 36",
    )
    .fetch_optional(fixture.client.pool())
    .await?;
    let opportunity_movement_columns = ["source_liquidity_mint", "target_liquidity_mint"];
    let decision_movement_columns = [
        "movement_route",
        "active_target_reserve",
        "custody_mint",
        "custody_amount_raw",
        "custody_account",
        "custody_reconciled_slot",
        "custody_version",
        "continuation_available_at",
        "continuation_lease_owner",
        "continuation_lease_expires_at",
        "continuation_fencing_token",
        "cross_mint_activation_control_generation",
        "cross_mint_preflight_certification",
        "continuation_control_generation",
        "terminal_outcome",
        "terminal_evidence",
        "terminal_reason",
        "terminal_observed_slot",
    ];
    let submission_leg_columns = [
        "movement_leg",
        "leg_purpose",
        "leg_generation",
        "required_commitment",
        "policy_account",
        "finalized_slot",
        "expected_effect",
        "reconciled_effect",
    ];
    let gate_columns = [
        "start_new_movements",
        "continue_or_recover_existing",
        "generation",
    ];
    let no_effect_receipt_columns = [
        "submission_id",
        "decision_id",
        "movement_leg",
        "leg_generation",
        "transaction_signature",
        "observed_block_height",
        "signature_history_checked_through_slot",
        "effect_check_slot",
        "expected_balance_anchors",
        "observed_balance_anchors",
        "signature_history_evidence",
        "evidence_hash",
    ];
    let swap_policy_columns = [
        "cluster",
        "settings",
        "authority",
        "policy_account",
        "vault_index",
        "vault_pubkey",
        "delegated_signer",
        "source_shard",
        "max_slippage_bps",
        "daily_source_mint_spending_cap",
        "manifest_fingerprint",
        "active",
        "start_eligible",
        "last_mutation",
        "source_commitment",
        "last_seen_slot",
        "last_seen_signature",
    ];
    let vault_opt_in_columns = [
        "cluster",
        "settings",
        "vault_index",
        "vault_pubkey",
        "enabled",
        "classic_policy_account",
        "classic_policy_seed",
        "token_2022_policy_account",
        "token_2022_policy_seed",
        "max_slippage_bps",
        "daily_source_mint_spending_cap",
        "generation",
        "created_at",
        "updated_at",
    ];

    let mut all_missing = missing_cross_mint_columns(
        &columns,
        "rebalance_opportunities",
        &opportunity_movement_columns,
    );
    all_missing.extend(missing_cross_mint_columns(
        &columns,
        "rebalance_decisions",
        &decision_movement_columns,
    ));
    all_missing.extend(missing_cross_mint_columns(
        &columns,
        "signed_route_submissions",
        &submission_leg_columns,
    ));
    all_missing.extend(missing_cross_mint_columns(
        &columns,
        "cross_mint_movement_controls",
        &gate_columns,
    ));
    all_missing.extend(missing_cross_mint_columns(
        &columns,
        "cross_mint_no_effect_receipts",
        &no_effect_receipt_columns,
    ));
    all_missing.extend(missing_cross_mint_columns(
        &columns,
        "cross_mint_swap_policies",
        &swap_policy_columns,
    ));
    all_missing.extend(missing_cross_mint_columns(
        &columns,
        "cross_mint_vault_opt_ins",
        &vault_opt_in_columns,
    ));
    for index in [
        "rebalance_decisions_cross_mint_continuation_idx",
        "signed_route_submissions_movement_leg_generation_uidx",
        "signed_route_submissions_one_nonterminal_opportunity_idx",
        "cross_mint_swap_policies_start_idx",
        "cross_mint_swap_policies_account_idx",
        "cross_mint_vault_opt_ins_enabled_idx",
    ] {
        if !index_names.contains(index) {
            all_missing.push(format!("database index: {index}"));
        }
    }
    if movement_migration.is_none() {
        all_missing.push("schema migration 35: durable_cross_mint_movements".to_owned());
    }
    if capability_migration.is_none() {
        all_missing.push("schema migration 36: cross_mint_swap_policies".to_owned());
    }
    let opt_in_migration = sqlx::query_as::<_, (String, String)>(
        "SELECT name, checksum FROM loyal_yield.schema_migrations WHERE version = 37",
    )
    .fetch_optional(fixture.client.pool())
    .await?;
    if opt_in_migration.is_none() {
        all_missing.push("schema migration 37: cross_mint_vault_opt_ins".to_owned());
    }

    let available_columns = columns
        .iter()
        .map(|(table, columns)| (table.clone(), columns.iter().cloned().collect::<Vec<_>>()))
        .collect::<BTreeMap<_, _>>();
    let behavior_contracts = [
        (
            "cross_mint_one_movement_one_nonterminal_submission",
            "one active movement per vault and one nonterminal leg submission per movement",
        ),
        (
            "cross_mint_activated_intent_and_economics_are_immutable",
            "an activated movement cannot mutate its amount, route, plan, or economics",
        ),
        (
            "cross_mint_finalized_only_sequential_leg_advancement",
            "leg n+1 cannot be published before leg n is finalized and atomically reconciled",
        ),
        (
            "cross_mint_reconciliation_uses_exact_w_and_o_deltas",
            "withdraw custody W and swap output O come from finalized deltas",
        ),
        (
            "cross_mint_intermediate_reconciliation_keeps_movement_active",
            "withdraw and swap reconciliation keep the parent movement active",
        ),
        (
            "cross_mint_target_capacity_is_movement_scoped",
            "capacity remains attached across intermediate terminal submissions",
        ),
        (
            "cross_mint_continuation_claim_is_fenced",
            "one continuation claimant wins and stale fences cannot publish",
        ),
        (
            "cross_mint_pre_persistence_restart_reclaims_without_wire",
            "a crashed compiler before persistence leaves no signed wire and a higher fence can reclaim",
        ),
        (
            "cross_mint_retry_reuses_exact_signed_bytes",
            "retry leases preserve exact signed bytes, hash, and signature",
        ),
        (
            "cross_mint_proved_no_effect_advances_leg_generation",
            "generation advances only after a terminal no-effect receipt",
        ),
        (
            "cross_mint_ambiguous_effect_freezes_progression",
            "ambiguity blocks continuation and retains capacity",
        ),
        (
            "cross_mint_source_idle_recovers_to_source_mint_reserve",
            "source-idle custody recovers through a source-mint deposit",
        ),
        (
            "cross_mint_target_fallback_atomically_rebinds_capacity",
            "target-idle custody atomically rebinds same-mint capacity",
        ),
        (
            "cross_mint_start_and_continue_gates_are_independent",
            "new starts stop while existing movement continuation remains enabled",
        ),
        (
            "cross_mint_manual_closure_requires_evidence",
            "manual closure is fenced, evidence-backed, and releases capacity",
        ),
    ];
    if !all_missing.is_empty() {
        let mut checks = vec![not_run_subcheck(
            "cross_mint_movement_schema_and_store_capabilities",
            json!({
                "status": "NOT_RUN",
                "reason": "the disposable database is missing the cross-mint movement schema contract",
                "safeDefault": "cross-mint progression remains disabled",
                "missingCapabilities": all_missing,
                "availableColumns": available_columns,
                "relevantIndexes": indexes,
                "migration35": movement_migration.as_ref().map(|(name, checksum)| json!({
                    "name": name,
                    "checksum": checksum,
                })),
                "migration36": capability_migration.as_ref().map(|(name, checksum)| json!({
                    "name": name,
                    "checksum": checksum,
                })),
                "migration37": opt_in_migration.as_ref().map(|(name, checksum)| json!({
                    "name": name,
                    "checksum": checksum,
                })),
                "connectionRequired": "apply migrations 35-37 to the fleet_verify database and rerun",
            }),
        )];
        checks.extend(behavior_contracts.into_iter().map(|(name, invariant)| {
            cross_mint_not_run_subcheck(
                name,
                invariant,
                vec!["disposable database schema contract".to_owned()],
                "apply migrations 35-37 to the fleet_verify database and rerun",
            )
        }));
        checks.push(subcheck(
            "same_mint_signed_lifecycle_regression_remains_behavioral",
            same_mint_regression_passed,
            same_mint_regression_evidence,
        ));
        return Ok(checks);
    }

    let mut checks = vec![subcheck(
        "cross_mint_movement_schema_and_store_capabilities",
        true,
        json!({
            "migration35": movement_migration.as_ref().map(|(name, checksum)| json!({
                "name": name,
                "checksum": checksum,
            })),
            "migration36": capability_migration.as_ref().map(|(name, checksum)| json!({
                "name": name,
                "checksum": checksum,
            })),
            "migration37": opt_in_migration.as_ref().map(|(name, checksum)| json!({
                "name": name,
                "checksum": checksum,
            })),
            "availableColumns": available_columns,
            "relevantIndexes": indexes,
            "typedStoreContract": [
                "activate_cross_mint_movement",
                "claim_cross_mint_continuation",
                "append_cross_mint_leg",
                "reconcile_cross_mint_leg",
                "rebind_cross_mint_fallback_capacity",
                "close_cross_mint_movement",
                "load_cross_mint_vault_opt_in",
                "upsert_cross_mint_vault_opt_in",
                "disable_cross_mint_vault_opt_in",
                "enable_cross_mint_vault_opt_in",
            ],
        }),
    )];

    let pre_persistence =
        activate_cross_mint_fixture(fixture, "cross_mint_pre_persistence_restart").await?;
    let pre_first = claim_cross_mint_continuation(
        fixture,
        &pre_persistence.movement.cluster,
        "pre-persistence-crashed-worker",
    )
    .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.rebalance_decisions
        SET continuation_lease_expires_at = now() - interval '1 second'
        WHERE id = $1
        "#,
    )
    .bind(pre_persistence.movement.decision_id.as_i64())
    .execute(fixture.client.pool())
    .await?;
    let pre_reclaimed = claim_cross_mint_continuation(
        fixture,
        &pre_persistence.movement.cluster,
        "pre-persistence-restarted-worker",
    )
    .await?;
    let pre_submission_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id = $1",
    )
    .bind(pre_persistence.movement.decision_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    checks.push(subcheck(
        "cross_mint_pre_persistence_restart_reclaims_without_wire",
        pre_submission_count == 0 && pre_reclaimed.fencing_token > pre_first.fencing_token,
        json!({
            "decisionId": pre_persistence.movement.decision_id.as_i64(),
            "crashedFence": pre_first.fencing_token,
            "reclaimedFence": pre_reclaimed.fencing_token,
            "signedSubmissionCount": pre_submission_count,
            "safeResponse": "rebuild only after the expired continuation fence is reclaimed",
        }),
    ));

    let success = activate_cross_mint_fixture(fixture, "cross_mint_success").await?;
    let repeated_activation = fixture
        .client
        .activate_cross_mint_movement(
            &success.opportunity_lease,
            CrossMintMovementActivationInput {
                capacity: success.capacity.clone(),
                initial_withdraw_compiled_fee_lamports: 5_000,
                preflight_certification: success.movement.preflight_certification.clone(),
                policy_bindings: success.policy_bindings.clone(),
            },
        )
        .await?;
    let success_cluster = success.movement.cluster.clone();
    let activated_intent_mutation_rejected = sqlx::query(
        "UPDATE loyal_yield.rebalance_opportunities SET amount_raw = amount_raw + 1 WHERE id = $1",
    )
    .bind(success.movement.opportunity_id)
    .execute(fixture.client.pool())
    .await
    .is_err();
    checks.push(subcheck(
        "cross_mint_activated_intent_and_economics_are_immutable",
        activated_intent_mutation_rejected,
        json!({
            "opportunityId": success.movement.opportunity_id,
            "amountMutationRejected": activated_intent_mutation_rejected,
            "safeResponse": "retain the original movement intent and do not create a replacement while custody is active",
        }),
    ));
    let claim_a =
        fixture
            .client
            .claim_cross_mint_continuation(&success_cluster, "cross-mint-racer-a", 60);
    let claim_b =
        fixture
            .client
            .claim_cross_mint_continuation(&success_cluster, "cross-mint-racer-b", 60);
    let (claim_a, claim_b) = tokio::join!(claim_a, claim_b);
    let claim_a = claim_a?;
    let claim_b = claim_b?;
    let winner_count = usize::from(claim_a.is_some()) + usize::from(claim_b.is_some());
    let race_winner = claim_a
        .or(claim_b)
        .ok_or("continuation race had no winner")?;
    let source_idle_account = format!("source-idle:{}", success.movement.decision_id.as_i64());
    let withdraw_expected = CrossMintExpectedEffect {
        debit: None,
        credit_mint: Some("USDC".to_owned()),
        credit_token_account: Some(source_idle_account.clone()),
        minimum_credit_amount_raw: Some(850_000),
    };
    let stale_input = cross_mint_leg_input(
        fixture,
        &success.opportunity_lease,
        &race_winner,
        "success-withdraw-stale",
        CrossMintMovementLeg::Withdraw,
        CrossMintLegPurpose::OptimizeYield,
        1,
        withdraw_expected.clone(),
    )
    .await?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_decisions SET continuation_lease_expires_at = now() - interval '1 second' WHERE id = $1 AND continuation_lease_owner = $2 AND continuation_fencing_token = $3",
    )
    .bind(success.movement.decision_id.as_i64())
    .bind(&race_winner.owner)
    .bind(race_winner.fencing_token)
    .execute(fixture.client.pool())
    .await?;
    let continuation = claim_cross_mint_continuation(
        fixture,
        &success_cluster,
        "success-withdraw-before-persistence",
    )
    .await?;
    let withdraw_input = cross_mint_leg_input(
        fixture,
        &success.opportunity_lease,
        &continuation,
        "success-withdraw",
        CrossMintMovementLeg::Withdraw,
        CrossMintLegPurpose::OptimizeYield,
        1,
        withdraw_expected,
    )
    .await?;
    let withdraw_submission = fixture
        .client
        .append_cross_mint_leg(&continuation, withdraw_input)
        .await?;
    let stale_publish_rejected = fixture
        .client
        .append_cross_mint_leg(&race_winner, stale_input)
        .await
        .is_err();
    let (movement_count, nonterminal_count): (i64, i64) = sqlx::query_as(
        r#"
        SELECT
            (SELECT count(*) FROM loyal_yield.rebalance_decisions
             WHERE vault_id = $1 AND movement_route = 'cross_mint_jupiter'
               AND status = 'confirming'),
            (SELECT count(*) FROM loyal_yield.signed_route_submissions
             WHERE decision_id = $2
               AND submission_state NOT IN ('reconciled', 'expired', 'failed'))
        "#,
    )
    .bind(success.movement.vault_id.as_i64())
    .bind(success.movement.decision_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    checks.push(subcheck(
        "cross_mint_one_movement_one_nonterminal_submission",
        repeated_activation.decision_id == success.movement.decision_id
            && movement_count == 1
            && nonterminal_count == 1,
        json!({
            "decisionId": success.movement.decision_id.as_i64(),
            "repeatedActivationDecisionId": repeated_activation.decision_id.as_i64(),
            "activeMovementCount": movement_count,
            "nonterminalSubmissionCount": nonterminal_count,
        }),
    ));
    checks.push(subcheck(
        "cross_mint_continuation_claim_is_fenced",
        winner_count == 1 && stale_publish_rejected,
        json!({
            "concurrentClaimWinners": winner_count,
            "raceWinnerFencingToken": race_winner.fencing_token,
            "restartWinnerFencingToken": continuation.fencing_token,
            "stalePublishRejected": stale_publish_rejected,
        }),
    ));

    let first_retry = lease_pending_cross_mint_submission(
        fixture,
        &success_cluster,
        "success-retry-one",
        withdraw_submission.id,
    )
    .await?;
    let retry_identity = (
        first_retry.submission.signed_transaction.clone(),
        first_retry.submission.signed_transaction_hash.clone(),
        first_retry.submission.transaction_signature.clone(),
    );
    let now = Utc::now();
    fixture
        .client
        .advance_signed_route_submission(
            &first_retry,
            SignedRouteSubmissionAdvance::Deferred {
                checked_at: now,
                next_poll_at: now,
                error_detail: Some("verifier_retry_boundary".to_owned()),
            },
        )
        .await?;
    let second_retry = lease_pending_cross_mint_submission(
        fixture,
        &success_cluster,
        "success-retry-two",
        withdraw_submission.id,
    )
    .await?;
    let exact_retry = retry_identity
        == (
            second_retry.submission.signed_transaction.clone(),
            second_retry.submission.signed_transaction_hash.clone(),
            second_retry.submission.transaction_signature.clone(),
        );
    let now = Utc::now();
    fixture
        .client
        .advance_signed_route_submission(
            &second_retry,
            SignedRouteSubmissionAdvance::Deferred {
                checked_at: now,
                next_poll_at: now,
                error_detail: None,
            },
        )
        .await?;
    checks.push(subcheck(
        "cross_mint_retry_reuses_exact_signed_bytes",
        exact_retry,
        json!({
            "submissionId": withdraw_submission.id,
            "signedTransactionSha256": withdraw_submission.signed_transaction_hash,
            "transactionSignature": withdraw_submission.transaction_signature,
            "leaseAttemptsCompared": 2,
        }),
    ));

    let withdraw_amount = 900_000;
    let (after_withdraw, withdraw_rejected_before_finality, withdraw_crash_windows) =
        finalize_and_reconcile_cross_mint_leg(
            fixture,
            &success_cluster,
            withdraw_submission.id,
            10_101,
            CrossMintReconciledEffect {
                debit: None,
                credit: Some(TokenBalanceDelta {
                    mint: "USDC".to_owned(),
                    token_account: source_idle_account.clone(),
                    amount_raw: withdraw_amount,
                }),
            },
        )
        .await?;
    let success_opt_in_lookup = CrossMintVaultOptInLookup {
        cluster: success_cluster.clone(),
        settings: success.policy_bindings.settings.clone(),
        vault_index: success.policy_bindings.vault_index,
        vault_pubkey: success.policy_bindings.vault_pubkey.clone(),
    };
    let success_opt_in = fixture
        .client
        .load_cross_mint_vault_opt_in(success_opt_in_lookup.clone())
        .await?
        .ok_or("successful cross-mint fixture lacks its enrollment")?;
    let paused_after_withdraw = fixture
        .client
        .disable_cross_mint_vault_opt_in(success_opt_in_lookup, success_opt_in.generation)
        .await?
        .ok_or("successful cross-mint enrollment disappeared while pausing")?;
    let withdraw_signature = withdraw_submission.transaction_signature.clone();
    let seeded_stale_confirmation = sqlx::query(
        r#"
        UPDATE loyal_yield.rebalance_decisions
        SET confirmed_slot = $2
        WHERE id = $1
          AND signature = $3
          AND status = 'confirming'::loyal_yield.decision_status
        "#,
    )
    .bind(success.movement.decision_id.as_i64())
    .bind(10_101_i64)
    .bind(&withdraw_signature)
    .execute(fixture.client.pool())
    .await?
    .rows_affected()
        == 1;
    let capacity_after_withdraw =
        cross_mint_capacity_state(fixture, success.movement.decision_id.as_i64()).await?;
    let swap_continuation =
        claim_cross_mint_continuation(fixture, &success_cluster, "success-swap").await?;
    let target_idle_account = format!("target-idle:{}", success.movement.decision_id.as_i64());
    let swap_submission = fixture
        .client
        .append_cross_mint_leg(
            &swap_continuation,
            cross_mint_leg_input(
                fixture,
                &success.opportunity_lease,
                &swap_continuation,
                "success-swap",
                CrossMintMovementLeg::Swap,
                CrossMintLegPurpose::OptimizeYield,
                1,
                CrossMintExpectedEffect {
                    debit: Some(TokenBalanceDelta {
                        mint: "USDC".to_owned(),
                        token_account: source_idle_account.clone(),
                        amount_raw: withdraw_amount,
                    }),
                    credit_mint: Some("USDT".to_owned()),
                    credit_token_account: Some(target_idle_account.clone()),
                    minimum_credit_amount_raw: Some(890_000),
                },
            )
            .await?,
        )
        .await?;
    let decision_after_append = sqlx::query(
        r#"
        SELECT signature, confirmed_slot, status::text AS status
        FROM loyal_yield.rebalance_decisions
        WHERE id = $1
        "#,
    )
    .bind(success.movement.decision_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let sequential_leg_decision_reset = decision_after_append
        .try_get::<Option<String>, _>("signature")?
        .is_none()
        && decision_after_append
            .try_get::<Option<i64>, _>("confirmed_slot")?
            .is_none()
        && decision_after_append.try_get::<String, _>("status")? == "confirming";
    let swap_broadcast_lease = lease_pending_cross_mint_submission(
        fixture,
        &success_cluster,
        "success-swap-broadcast-prepare",
        swap_submission.id,
    )
    .await?;
    let prepared_swap_broadcast = fixture
        .client
        .prepare_signed_route_broadcast_batch(&[swap_broadcast_lease], Utc::now())
        .await?;
    let sequential_leg_broadcast_prepared = prepared_swap_broadcast.len() == 1
        && prepared_swap_broadcast[0].submission.id == swap_submission.id
        && prepared_swap_broadcast[0].submission.broadcast_count == 1
        && prepared_swap_broadcast[0]
            .submission
            .error_detail
            .as_deref()
            == Some("broadcast_intent_persisted");
    let now = Utc::now();
    fixture
        .client
        .defer_signed_route_submission_lease_batch(
            &prepared_swap_broadcast,
            now,
            now,
            "verifier_sequential_leg_broadcast_boundary",
        )
        .await?;
    let swap_output = 895_000;
    let (after_swap, swap_rejected_before_finality, swap_crash_windows) =
        finalize_and_reconcile_cross_mint_leg(
            fixture,
            &success_cluster,
            swap_submission.id,
            10_202,
            CrossMintReconciledEffect {
                debit: Some(TokenBalanceDelta {
                    mint: "USDC".to_owned(),
                    token_account: source_idle_account.clone(),
                    amount_raw: withdraw_amount,
                }),
                credit: Some(TokenBalanceDelta {
                    mint: "USDT".to_owned(),
                    token_account: target_idle_account.clone(),
                    amount_raw: swap_output,
                }),
            },
        )
        .await?;
    let capacity_after_swap =
        cross_mint_capacity_state(fixture, success.movement.decision_id.as_i64()).await?;
    checks.push(subcheck(
        "cross_mint_finalized_only_sequential_leg_advancement",
        withdraw_submission.required_commitment == "finalized"
            && swap_submission.required_commitment == "finalized"
            && seeded_stale_confirmation
            && sequential_leg_decision_reset
            && sequential_leg_broadcast_prepared
            && withdraw_rejected_before_finality
            && swap_rejected_before_finality
            && after_withdraw.custody_reconciled_slot == Some(10_101)
            && after_swap.custody_reconciled_slot == Some(10_202),
        json!({
            "withdrawRequiredCommitment": withdraw_submission.required_commitment,
            "swapRequiredCommitment": swap_submission.required_commitment,
            "seededStaleConfirmation": seeded_stale_confirmation,
            "sequentialLegDecisionReset": sequential_leg_decision_reset,
            "sequentialLegBroadcastPrepared": sequential_leg_broadcast_prepared,
            "withdrawRejectedBeforeFinality": withdraw_rejected_before_finality,
            "swapRejectedBeforeFinality": swap_rejected_before_finality,
            "withdrawFinalizedSlot": after_withdraw.custody_reconciled_slot,
            "swapFinalizedSlot": after_swap.custody_reconciled_slot,
        }),
    ));
    checks.push(subcheck(
        "cross_mint_reconciliation_uses_exact_w_and_o_deltas",
        after_withdraw.planned_amount_raw == 1_000_000
            && after_withdraw.custody_amount_raw == withdraw_amount
            && after_withdraw.custody_observed_balance_raw == Some(23 + withdraw_amount)
            && after_swap.custody_amount_raw == swap_output
            && after_swap.custody_observed_balance_raw == Some(23 + swap_output)
            && after_swap.custody_account == target_idle_account,
        json!({
            "plannedAmountRaw": after_withdraw.planned_amount_raw,
            "withdrawDeltaW": after_withdraw.custody_amount_raw,
            "sourcePreexistingBalanceRaw": 23,
            "sourceObservedAggregateRaw": after_withdraw.custody_observed_balance_raw,
            "swapOutputDeltaO": after_swap.custody_amount_raw,
            "targetPreexistingBalanceRaw": 23,
            "targetObservedAggregateRaw": after_swap.custody_observed_balance_raw,
            "sourceIdleAccount": source_idle_account,
            "targetIdleAccount": target_idle_account,
        }),
    ));
    checks.push(subcheck(
        "cross_mint_intermediate_reconciliation_keeps_movement_active",
        after_withdraw.phase == CrossMintCustodyPhase::SourceIdle
            && after_swap.phase == CrossMintCustodyPhase::TargetIdle
            && after_withdraw.terminal_outcome.is_none()
            && after_swap.terminal_outcome.is_none(),
        json!({
            "withdrawPhase": after_withdraw.phase,
            "swapPhase": after_swap.phase,
            "withdrawCustodyVersion": after_withdraw.custody_version,
            "swapCustodyVersion": after_swap.custody_version,
        }),
    ));
    checks.push(subcheck(
        "cross_mint_target_capacity_is_movement_scoped",
        capacity_after_withdraw.0 == success.reservation_id
            && capacity_after_swap.0 == success.reservation_id
            && capacity_after_withdraw.1 == "active"
            && capacity_after_swap.1 == "active",
        json!({
            "reservationId": success.reservation_id,
            "afterWithdraw": capacity_after_withdraw,
            "afterSwap": capacity_after_swap,
        }),
    ));

    let deposit_continuation =
        claim_cross_mint_continuation(fixture, &success_cluster, "success-deposit").await?;
    let deposit_submission = fixture
        .client
        .append_cross_mint_leg(
            &deposit_continuation,
            cross_mint_leg_input(
                fixture,
                &success.opportunity_lease,
                &deposit_continuation,
                "success-deposit",
                CrossMintMovementLeg::Deposit,
                CrossMintLegPurpose::OptimizeYield,
                1,
                CrossMintExpectedEffect {
                    debit: Some(TokenBalanceDelta {
                        mint: "USDT".to_owned(),
                        token_account: target_idle_account.clone(),
                        amount_raw: swap_output,
                    }),
                    credit_mint: None,
                    credit_token_account: None,
                    minimum_credit_amount_raw: None,
                },
            )
            .await?,
        )
        .await?;
    let (completed, deposit_rejected_before_finality, deposit_crash_windows) =
        finalize_and_reconcile_cross_mint_leg(
            fixture,
            &success_cluster,
            deposit_submission.id,
            10_303,
            CrossMintReconciledEffect {
                debit: Some(TokenBalanceDelta {
                    mint: "USDT".to_owned(),
                    token_account: target_idle_account,
                    amount_raw: swap_output - 1,
                }),
                credit: None,
            },
        )
        .await?;
    let completed_capacity =
        cross_mint_capacity_state(fixture, success.movement.decision_id.as_i64()).await?;
    let completed_continuation = fixture
        .client
        .claim_cross_mint_continuation(&success_cluster, "completed-must-not-continue", 60)
        .await?;
    checks.push(subcheck(
        "cross_mint_success_terminalizes_only_after_target_deposit",
        deposit_rejected_before_finality
            && completed.phase == CrossMintCustodyPhase::TargetReserve
            && completed.terminal_outcome == Some(CrossMintTerminalOutcome::CompletedTarget)
            && completed.custody_amount_raw == 1
            && completed.custody_observed_balance_raw == Some(24)
            && completed.terminal_reason.as_deref() == Some("kamino_unmintable_rounding_dust")
            && completed.terminal_observed_slot == Some(10_303)
            && completed
                .terminal_evidence
                .as_ref()
                .is_some_and(|evidence| {
                    evidence.get("residualAmountRaw").and_then(Value::as_i64) == Some(1)
                        && evidence
                            .get("minimumDepositAmountRaw")
                            .and_then(Value::as_i64)
                            == Some(2)
                })
            && completed_continuation.is_none()
            && completed_capacity.1 == "awaiting_telemetry",
        json!({
            "phase": completed.phase,
            "terminalOutcome": completed.terminal_outcome,
            "terminalEvidence": completed.terminal_evidence,
            "terminalReason": completed.terminal_reason,
            "custodyVersion": completed.custody_version,
            "capacityState": completed_capacity.1,
            "continuationClaimedAfterTerminal": completed_continuation.is_some(),
        }),
    ));
    checks.push(subcheck(
        "cross_mint_pause_after_withdraw_allows_safe_continuation",
        !paused_after_withdraw.enabled
            && paused_after_withdraw.generation == success_opt_in.generation + 1
            && after_withdraw.phase == CrossMintCustodyPhase::SourceIdle
            && after_swap.phase == CrossMintCustodyPhase::TargetIdle
            && completed.terminal_outcome == Some(CrossMintTerminalOutcome::CompletedTarget),
        json!({
            "enabledAfterPause": paused_after_withdraw.enabled,
            "generationBefore": success_opt_in.generation,
            "generationAfter": paused_after_withdraw.generation,
            "withdrawPhase": after_withdraw.phase,
            "swapPhase": after_swap.phase,
            "terminalOutcome": completed.terminal_outcome,
        }),
    ));

    // No-effect receipts are intentionally immutable, including against
    // fixture cleanup. Keep this audit lineage under a separate disposable-DB
    // prefix just like fee-spend receipts.
    let no_effect_fixture = DatabaseFixture {
        client: fixture.client.clone(),
        latency_client: fixture.latency_client.clone(),
        prefix: format!("immutable_no_effect_{}", fixture.prefix),
    };
    let no_effect = activate_cross_mint_fixture(&no_effect_fixture, "cross_mint_no_effect").await?;
    let no_effect_continuation = claim_cross_mint_continuation(
        &no_effect_fixture,
        &no_effect.movement.cluster,
        "no-effect-generation-one",
    )
    .await?;
    let no_effect_account = format!("source-idle:{}", no_effect.movement.decision_id.as_i64());
    let no_effect_expected = CrossMintExpectedEffect {
        debit: None,
        credit_mint: Some("USDC".to_owned()),
        credit_token_account: Some(no_effect_account),
        minimum_credit_amount_raw: Some(1),
    };
    let mut unanchored_no_effect_input = cross_mint_leg_input(
        &no_effect_fixture,
        &no_effect.opportunity_lease,
        &no_effect_continuation,
        "no-effect-withdraw-unanchored",
        CrossMintMovementLeg::Withdraw,
        CrossMintLegPurpose::OptimizeYield,
        1,
        no_effect_expected.clone(),
    )
    .await?;
    unanchored_no_effect_input.expected_balance_anchors = CrossMintBalanceAnchors::default();
    let unanchored_publication_rejected = fixture
        .client
        .append_cross_mint_leg(&no_effect_continuation, unanchored_no_effect_input)
        .await
        .is_err();
    let generation_one = fixture
        .client
        .append_cross_mint_leg(
            &no_effect_continuation,
            cross_mint_leg_input(
                &no_effect_fixture,
                &no_effect.opportunity_lease,
                &no_effect_continuation,
                "no-effect-withdraw-one",
                CrossMintMovementLeg::Withdraw,
                CrossMintLegPurpose::OptimizeYield,
                1,
                no_effect_expected.clone(),
            )
            .await?,
        )
        .await?;
    let generation_one_lease = lease_pending_cross_mint_submission(
        &no_effect_fixture,
        &no_effect.movement.cluster,
        "no-effect-expirer",
        generation_one.id,
    )
    .await?;
    let expired_height = generation_one.last_valid_block_height + 1;
    let caller_booleans_without_receipt_rejected = fixture
        .client
        .advance_signed_route_submission(
            &generation_one_lease,
            SignedRouteSubmissionAdvance::Expired {
                checked_at: Utc::now(),
                observed_block_height: expired_height,
                signature_history_absent: true,
                effect_absence_proved: true,
            },
        )
        .await
        .is_err();
    let generation_one_expiry_check_slot = 10_801;
    let generation_one_expiry_pending = fixture
        .client
        .advance_signed_route_submission(
            &generation_one_lease,
            SignedRouteSubmissionAdvance::ExpiryCheckPending {
                checked_at: Utc::now(),
                observed_block_height: expired_height,
                effect_check_slot: generation_one_expiry_check_slot,
            },
        )
        .await?;
    let unbroadcast_cross_mint_uses_effect_lane = generation_one_expiry_pending.state
        == SignedRouteSubmissionState::ExpiryCheckPending
        && generation_one_expiry_pending.broadcast_count == 0;
    let generation_one_lease = fixture
        .client
        .lease_reconciliation_pending_signed_route_submissions(
            &no_effect.movement.cluster,
            "no-effect-reconciler",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .find(|lease| lease.submission.id == generation_one.id)
        .ok_or("unbroadcast cross-mint expiry did not enter the effect-check lane")?;
    let receipt = fixture
        .client
        .record_cross_mint_no_effect_receipt(
            &generation_one_lease,
            CrossMintNoEffectProofInput {
                observed_block_height: expired_height,
                signature_history_checked_through_slot: 10_802,
                effect_check_slot: generation_one_expiry_check_slot,
                observed_balance_anchors: generation_one.expected_balance_anchors.clone(),
                signature_history_evidence: json!({
                    "rpcCommitment": "finalized",
                    "transactionSignature": generation_one.transaction_signature,
                    "historyResult": "absent",
                    "checkedThroughSlot": 10_802,
                }),
                observed_at: Utc::now(),
            },
        )
        .await?;
    let receipt_mutation_rejected = sqlx::query(
        "UPDATE loyal_yield.cross_mint_no_effect_receipts SET observed_block_height = observed_block_height + 1 WHERE submission_id = $1",
    )
    .bind(generation_one.id)
    .execute(fixture.client.pool())
    .await
    .is_err();
    fixture
        .client
        .advance_signed_route_submission(
            &generation_one_lease,
            SignedRouteSubmissionAdvance::Expired {
                checked_at: Utc::now(),
                observed_block_height: expired_height,
                signature_history_absent: false,
                effect_absence_proved: false,
            },
        )
        .await?;
    let generation_two_continuation = claim_cross_mint_continuation(
        &no_effect_fixture,
        &no_effect.movement.cluster,
        "no-effect-generation-two",
    )
    .await?;
    fixture
        .client
        .append_cross_mint_leg(
            &generation_two_continuation,
            cross_mint_leg_input(
                &no_effect_fixture,
                &no_effect.opportunity_lease,
                &generation_two_continuation,
                "no-effect-withdraw-two",
                CrossMintMovementLeg::Withdraw,
                CrossMintLegPurpose::OptimizeYield,
                2,
                no_effect_expected,
            )
            .await?,
        )
        .await?;
    let generations: Vec<(i64, String)> = sqlx::query_as(
        r#"
        SELECT leg_generation, submission_state::TEXT
        FROM loyal_yield.signed_route_submissions
        WHERE decision_id = $1 AND movement_leg = 'withdraw'
        ORDER BY leg_generation
        "#,
    )
    .bind(no_effect.movement.decision_id.as_i64())
    .fetch_all(fixture.client.pool())
    .await?;
    checks.push(subcheck(
        "cross_mint_proved_no_effect_advances_leg_generation",
        unanchored_publication_rejected
            && caller_booleans_without_receipt_rejected
            && unbroadcast_cross_mint_uses_effect_lane
            && receipt_mutation_rejected
            && receipt.submission_id == generation_one.id
            && generations == vec![(1, "expired".to_owned()), (2, "signed".to_owned())],
        json!({
            "legHistory": generations,
            "unanchoredPublicationRejected": unanchored_publication_rejected,
            "callerBooleansWithoutReceiptRejected": caller_booleans_without_receipt_rejected,
            "unbroadcastCrossMintUsesEffectLane": unbroadcast_cross_mint_uses_effect_lane,
            "receiptId": receipt.submission_id,
            "receiptHash": receipt.evidence_hash,
            "receiptMutationRejected": receipt_mutation_rejected,
            "safeResponse": "persist an immutable finalized history and unchanged-balance receipt before expiring or replacing a cross-mint leg",
        }),
    ));

    let ambiguous = activate_cross_mint_fixture(fixture, "cross_mint_ambiguous").await?;
    let ambiguous_continuation =
        claim_cross_mint_continuation(fixture, &ambiguous.movement.cluster, "ambiguous-withdraw")
            .await?;
    let ambiguous_submission = fixture
        .client
        .append_cross_mint_leg(
            &ambiguous_continuation,
            cross_mint_leg_input(
                fixture,
                &ambiguous.opportunity_lease,
                &ambiguous_continuation,
                "ambiguous-withdraw",
                CrossMintMovementLeg::Withdraw,
                CrossMintLegPurpose::OptimizeYield,
                1,
                CrossMintExpectedEffect {
                    debit: None,
                    credit_mint: Some("USDC".to_owned()),
                    credit_token_account: Some(format!(
                        "source-idle:{}",
                        ambiguous.movement.decision_id.as_i64()
                    )),
                    minimum_credit_amount_raw: Some(1),
                },
            )
            .await?,
        )
        .await?;
    let ambiguous_submit_lease = lease_pending_cross_mint_submission(
        fixture,
        &ambiguous.movement.cluster,
        "ambiguous-broadcast",
        ambiguous_submission.id,
    )
    .await?;
    let now = Utc::now();
    fixture
        .client
        .advance_signed_route_submission(
            &ambiguous_submit_lease,
            SignedRouteSubmissionAdvance::Submitted {
                checked_at: now,
                observed_slot: Some(10_400),
                next_poll_at: now,
                broadcasted: true,
            },
        )
        .await?;
    let ambiguous_finality_lease = lease_pending_cross_mint_submission(
        fixture,
        &ambiguous.movement.cluster,
        "ambiguous-finality",
        ambiguous_submission.id,
    )
    .await?;
    fixture
        .client
        .advance_signed_route_submission(
            &ambiguous_finality_lease,
            SignedRouteSubmissionAdvance::Confirmed {
                checked_at: Utc::now(),
                confirmed_slot: 10_401,
            },
        )
        .await?;
    fixture
        .client
        .advance_signed_route_submission(
            &ambiguous_finality_lease,
            SignedRouteSubmissionAdvance::Finalized {
                checked_at: Utc::now(),
                finalized_slot: 10_401,
            },
        )
        .await?;
    fixture
        .client
        .advance_signed_route_submission(
            &ambiguous_finality_lease,
            SignedRouteSubmissionAdvance::ReconciliationPending,
        )
        .await?;
    let ambiguous_effect_lease = lease_reconciliation_cross_mint_submission(
        fixture,
        &ambiguous.movement.cluster,
        "ambiguous-effect",
        ambiguous_submission.id,
    )
    .await?;
    fixture
        .client
        .advance_signed_route_submission(
            &ambiguous_effect_lease,
            SignedRouteSubmissionAdvance::EffectAmbiguous {
                checked_at: Utc::now(),
                error_detail: "finalized custody metadata cannot be attributed".to_owned(),
            },
        )
        .await?;
    let ambiguity_continuation = fixture
        .client
        .claim_cross_mint_continuation(&ambiguous.movement.cluster, "ambiguous-must-freeze", 60)
        .await?;
    let ambiguous_capacity =
        cross_mint_capacity_state(fixture, ambiguous.movement.decision_id.as_i64()).await?;
    let ambiguous_state: String = sqlx::query_scalar(
        "SELECT submission_state::TEXT FROM loyal_yield.signed_route_submissions WHERE id = $1",
    )
    .bind(ambiguous_submission.id)
    .fetch_one(fixture.client.pool())
    .await?;
    checks.push(subcheck(
        "cross_mint_ambiguous_effect_freezes_progression",
        ambiguous_state == "effect_ambiguous"
            && ambiguity_continuation.is_none()
            && ambiguous_capacity.1 == "active",
        json!({
            "submissionState": ambiguous_state,
            "continuationClaimed": ambiguity_continuation.is_some(),
            "capacityState": ambiguous_capacity.1,
        }),
    ));
    let external_state = activate_cross_mint_fixture(fixture, "cross_mint_external_state").await?;
    let external_state_continuation = claim_cross_mint_continuation(
        fixture,
        &external_state.movement.cluster,
        "external-state-withdraw",
    )
    .await?;
    let external_state_submission = fixture
        .client
        .append_cross_mint_leg(
            &external_state_continuation,
            cross_mint_leg_input(
                fixture,
                &external_state.opportunity_lease,
                &external_state_continuation,
                "external-state-withdraw",
                CrossMintMovementLeg::Withdraw,
                CrossMintLegPurpose::OptimizeYield,
                1,
                CrossMintExpectedEffect {
                    debit: None,
                    credit_mint: Some("USDC".to_owned()),
                    credit_token_account: Some(format!(
                        "source-idle:{}",
                        external_state.movement.decision_id.as_i64()
                    )),
                    minimum_credit_amount_raw: Some(1),
                },
            )
            .await?,
        )
        .await?;
    let external_state_confirmation = lease_pending_cross_mint_submission(
        fixture,
        &external_state.movement.cluster,
        "external-state-expiry",
        external_state_submission.id,
    )
    .await?;
    fixture
        .client
        .advance_signed_route_submission(
            &external_state_confirmation,
            SignedRouteSubmissionAdvance::ExpiryCheckPending {
                checked_at: Utc::now(),
                observed_block_height: external_state_submission.last_valid_block_height + 1,
                effect_check_slot: 10_402,
            },
        )
        .await?;
    let external_state_lease = lease_reconciliation_cross_mint_submission(
        fixture,
        &external_state.movement.cluster,
        "external-state-resolution",
        external_state_submission.id,
    )
    .await?;
    fixture
        .client
        .advance_signed_route_submission(
            &external_state_lease,
            SignedRouteSubmissionAdvance::Failed {
                checked_at: Utc::now(),
                confirmed_slot: None,
                error_detail: "expired_cross_mint_balance_changed_by_external_state".to_owned(),
            },
        )
        .await?;
    let external_state_close_lease = claim_cross_mint_continuation(
        fixture,
        &external_state.movement.cluster,
        "external-state-manual-close",
    )
    .await?;
    let externally_invalidated = fixture
        .client
        .close_cross_mint_movement(
            &external_state_close_lease,
            CrossMintMovementCloseInput {
                outcome: CrossMintTerminalOutcome::ManualIntervention,
                observed_slot: 10_402,
                reason: "finalized custody changed outside the signed route".to_owned(),
                evidence: json!({
                    "kind": "custody_history_mismatch",
                    "commitment": "finalized",
                    "safeResponse": "leave the remaining funds user-owned and require a fresh movement",
                }),
            },
        )
        .await?;
    let external_state_capacity =
        cross_mint_capacity_state(fixture, external_state.movement.decision_id.as_i64()).await?;
    let external_state_submission_state: String = sqlx::query_scalar(
        "SELECT submission_state::TEXT FROM loyal_yield.signed_route_submissions WHERE id = $1",
    )
    .bind(external_state_submission.id)
    .fetch_one(fixture.client.pool())
    .await?;
    checks.push(subcheck(
        "cross_mint_proven_external_state_change_closes_for_manual_intervention",
        external_state_submission_state == "failed"
            && externally_invalidated.terminal_outcome
                == Some(CrossMintTerminalOutcome::ManualIntervention)
            && external_state_capacity.1 == "released",
        json!({
            "submissionState": external_state_submission_state,
            "terminalOutcome": externally_invalidated.terminal_outcome,
            "capacityState": external_state_capacity.1,
        }),
    ));

    let recovery = activate_cross_mint_fixture(fixture, "cross_mint_recovery").await?;
    let recovery_withdraw_continuation =
        claim_cross_mint_continuation(fixture, &recovery.movement.cluster, "recovery-withdraw")
            .await?;
    let recovery_idle = format!("source-idle:{}", recovery.movement.decision_id.as_i64());
    let recovery_withdraw = fixture
        .client
        .append_cross_mint_leg(
            &recovery_withdraw_continuation,
            cross_mint_leg_input(
                fixture,
                &recovery.opportunity_lease,
                &recovery_withdraw_continuation,
                "recovery-withdraw",
                CrossMintMovementLeg::Withdraw,
                CrossMintLegPurpose::OptimizeYield,
                1,
                CrossMintExpectedEffect {
                    debit: None,
                    credit_mint: Some("USDC".to_owned()),
                    credit_token_account: Some(recovery_idle.clone()),
                    minimum_credit_amount_raw: Some(700_000),
                },
            )
            .await?,
        )
        .await?;
    let recovery_amount = 750_000;
    finalize_and_reconcile_cross_mint_leg(
        fixture,
        &recovery.movement.cluster,
        recovery_withdraw.id,
        10_501,
        CrossMintReconciledEffect {
            debit: None,
            credit: Some(TokenBalanceDelta {
                mint: "USDC".to_owned(),
                token_account: recovery_idle.clone(),
                amount_raw: recovery_amount,
            }),
        },
    )
    .await?;
    let recovery_deposit_continuation =
        claim_cross_mint_continuation(fixture, &recovery.movement.cluster, "recovery-deposit")
            .await?;
    let recovery_deposit = fixture
        .client
        .append_cross_mint_leg(
            &recovery_deposit_continuation,
            cross_mint_leg_input(
                fixture,
                &recovery.opportunity_lease,
                &recovery_deposit_continuation,
                "recovery-deposit",
                CrossMintMovementLeg::Deposit,
                CrossMintLegPurpose::RecoverSource,
                1,
                CrossMintExpectedEffect {
                    debit: Some(TokenBalanceDelta {
                        mint: "USDC".to_owned(),
                        token_account: recovery_idle.clone(),
                        amount_raw: recovery_amount,
                    }),
                    credit_mint: None,
                    credit_token_account: None,
                    minimum_credit_amount_raw: None,
                },
            )
            .await?,
        )
        .await?;
    let (recovered, _, recovery_crash_windows) = finalize_and_reconcile_cross_mint_leg(
        fixture,
        &recovery.movement.cluster,
        recovery_deposit.id,
        10_502,
        CrossMintReconciledEffect {
            debit: Some(TokenBalanceDelta {
                mint: "USDC".to_owned(),
                token_account: recovery_idle,
                amount_raw: recovery_amount,
            }),
            credit: None,
        },
    )
    .await?;
    let recovered_capacity =
        cross_mint_capacity_state(fixture, recovery.movement.decision_id.as_i64()).await?;
    let recovered_continuation = fixture
        .client
        .claim_cross_mint_continuation(
            &recovery.movement.cluster,
            "recovered-must-not-continue",
            60,
        )
        .await?;
    checks.push(subcheck(
        "cross_mint_source_idle_recovers_to_source_mint_reserve",
        recovered.phase == CrossMintCustodyPhase::SourceReserve
            && recovered.terminal_outcome == Some(CrossMintTerminalOutcome::RecoveredSource)
            && recovered.custody_amount_raw == 0
            && recovered.custody_observed_balance_raw.is_none()
            && recovered_continuation.is_none()
            && recovered_capacity.1 == "released",
        json!({
            "phase": recovered.phase,
            "terminalOutcome": recovered.terminal_outcome,
            "custodyAccount": recovered.custody_account,
            "capacityState": recovered_capacity.1,
            "continuationClaimedAfterTerminal": recovered_continuation.is_some(),
        }),
    ));

    let fallback = activate_cross_mint_fixture(fixture, "cross_mint_fallback").await?;
    let fallback_source_idle = format!("source-idle:{}", fallback.movement.decision_id.as_i64());
    let fallback_target_idle = format!("target-idle:{}", fallback.movement.decision_id.as_i64());
    let fallback_withdraw_continuation =
        claim_cross_mint_continuation(fixture, &fallback.movement.cluster, "fallback-withdraw")
            .await?;
    let fallback_withdraw = fixture
        .client
        .append_cross_mint_leg(
            &fallback_withdraw_continuation,
            cross_mint_leg_input(
                fixture,
                &fallback.opportunity_lease,
                &fallback_withdraw_continuation,
                "fallback-withdraw",
                CrossMintMovementLeg::Withdraw,
                CrossMintLegPurpose::OptimizeYield,
                1,
                CrossMintExpectedEffect {
                    debit: None,
                    credit_mint: Some("USDC".to_owned()),
                    credit_token_account: Some(fallback_source_idle.clone()),
                    minimum_credit_amount_raw: Some(800_000),
                },
            )
            .await?,
        )
        .await?;
    finalize_and_reconcile_cross_mint_leg(
        fixture,
        &fallback.movement.cluster,
        fallback_withdraw.id,
        10_601,
        CrossMintReconciledEffect {
            debit: None,
            credit: Some(TokenBalanceDelta {
                mint: "USDC".to_owned(),
                token_account: fallback_source_idle.clone(),
                amount_raw: 810_000,
            }),
        },
    )
    .await?;
    let fallback_swap_continuation =
        claim_cross_mint_continuation(fixture, &fallback.movement.cluster, "fallback-swap").await?;
    let fallback_swap = fixture
        .client
        .append_cross_mint_leg(
            &fallback_swap_continuation,
            cross_mint_leg_input(
                fixture,
                &fallback.opportunity_lease,
                &fallback_swap_continuation,
                "fallback-swap",
                CrossMintMovementLeg::Swap,
                CrossMintLegPurpose::OptimizeYield,
                1,
                CrossMintExpectedEffect {
                    debit: Some(TokenBalanceDelta {
                        mint: "USDC".to_owned(),
                        token_account: fallback_source_idle.clone(),
                        amount_raw: 810_000,
                    }),
                    credit_mint: Some("USDT".to_owned()),
                    credit_token_account: Some(fallback_target_idle.clone()),
                    minimum_credit_amount_raw: Some(790_000),
                },
            )
            .await?,
        )
        .await?;
    finalize_and_reconcile_cross_mint_leg(
        fixture,
        &fallback.movement.cluster,
        fallback_swap.id,
        10_602,
        CrossMintReconciledEffect {
            debit: Some(TokenBalanceDelta {
                mint: "USDC".to_owned(),
                token_account: fallback_source_idle,
                amount_raw: 810_000,
            }),
            credit: Some(TokenBalanceDelta {
                mint: "USDT".to_owned(),
                token_account: fallback_target_idle.clone(),
                amount_raw: 800_000,
            }),
        },
    )
    .await?;
    let fallback_rebind_lease =
        claim_cross_mint_continuation(fixture, &fallback.movement.cluster, "fallback-rebind")
            .await?;
    let original_capacity =
        cross_mint_capacity_state(fixture, fallback.movement.decision_id.as_i64()).await?;
    let fallback_reserve = format!("fallback-target:{}", fallback.movement.decision_id.as_i64());
    let fallback_observation = TargetCapacityObservation {
        cluster: fallback.movement.cluster.clone(),
        target_reserve: fallback_reserve.clone(),
        liquidity_mint: "USDT".to_owned(),
        observed_supply_usd_micros: 30_000_000_000,
        observed_slot: 10_603,
        maximum_inflight_usd_micros: 1_000_000_000,
    };
    let fallback_projection = fixture
        .client
        .observe_target_capacity(fallback_observation.clone())
        .await?;
    let capacity_deadlocks_before: i64 = sqlx::query_scalar(
        "SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()",
    )
    .fetch_one(fixture.client.pool())
    .await?;
    let concurrent_observation = fallback.capacity.projection.observation.clone();
    let concurrent_started = Instant::now();
    let rebind = fixture.client.rebind_cross_mint_fallback_capacity(
        &fallback_rebind_lease,
        CrossMintFallbackCapacityInput {
            target: fallback_projection,
        },
    );
    let observe = fixture
        .client
        .observe_target_capacity(concurrent_observation);
    let (rebound, concurrent_projection) = tokio::join!(rebind, observe);
    let rebound = rebound?;
    let concurrent_projection = concurrent_projection?;
    let concurrent_capacity_elapsed_millis = concurrent_started.elapsed().as_millis();
    let capacity_deadlocks_after: i64 = sqlx::query_scalar(
        "SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()",
    )
    .fetch_one(fixture.client.pool())
    .await?;
    let rebound_movement: (String, i64) = sqlx::query_as(
        "SELECT active_target_reserve, continuation_fencing_token FROM loyal_yield.rebalance_decisions WHERE id = $1",
    )
    .bind(fallback.movement.decision_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let fallback_deposit_continuation =
        claim_cross_mint_continuation(fixture, &fallback.movement.cluster, "fallback-deposit")
            .await?;
    let fallback_deposit = fixture
        .client
        .append_cross_mint_leg(
            &fallback_deposit_continuation,
            cross_mint_leg_input(
                fixture,
                &fallback.opportunity_lease,
                &fallback_deposit_continuation,
                "fallback-deposit",
                CrossMintMovementLeg::Deposit,
                CrossMintLegPurpose::FallbackTarget,
                1,
                CrossMintExpectedEffect {
                    debit: Some(TokenBalanceDelta {
                        mint: "USDT".to_owned(),
                        token_account: fallback_target_idle.clone(),
                        amount_raw: 800_000,
                    }),
                    credit_mint: None,
                    credit_token_account: None,
                    minimum_credit_amount_raw: None,
                },
            )
            .await?,
        )
        .await?;
    let (fallback_completed, _, fallback_crash_windows) = finalize_and_reconcile_cross_mint_leg(
        fixture,
        &fallback.movement.cluster,
        fallback_deposit.id,
        10_604,
        CrossMintReconciledEffect {
            debit: Some(TokenBalanceDelta {
                mint: "USDT".to_owned(),
                token_account: fallback_target_idle,
                amount_raw: 800_000,
            }),
            credit: None,
        },
    )
    .await?;
    let fallback_terminal_continuation = fixture
        .client
        .claim_cross_mint_continuation(
            &fallback.movement.cluster,
            "fallback-terminal-must-not-continue",
            60,
        )
        .await?;
    checks.push(subcheck(
        "cross_mint_target_fallback_atomically_rebinds_capacity",
        rebound.id == original_capacity.0
            && rebound.target_reserve == fallback_reserve
            && rebound.reservation_generation > original_capacity.3
            && rebound_movement.0 == fallback_reserve
            && capacity_deadlocks_after == capacity_deadlocks_before
            && fallback_terminal_continuation.is_none()
            && fallback_completed.terminal_outcome
                == Some(CrossMintTerminalOutcome::CompletedTarget),
        json!({
            "reservationIdBefore": original_capacity.0,
            "reservationIdAfter": rebound.id,
            "generationBefore": original_capacity.3,
            "generationAfter": rebound.reservation_generation,
            "activeTargetReserve": rebound_movement.0,
            "concurrentObservationTelemetryVersion": concurrent_projection.telemetry_version,
            "concurrentCapacityElapsedMillis": concurrent_capacity_elapsed_millis,
            "deadlocksBefore": capacity_deadlocks_before,
            "deadlocksAfter": capacity_deadlocks_after,
            "terminalOutcome": fallback_completed.terminal_outcome,
            "continuationClaimedAfterTerminal": fallback_terminal_continuation.is_some(),
        }),
    ));
    let every_leg_purpose_crash_window = [
        withdraw_crash_windows.passed(),
        swap_crash_windows.passed(),
        deposit_crash_windows.passed(),
        recovery_crash_windows.passed(),
        fallback_crash_windows.passed(),
    ]
    .into_iter()
    .all(|passed| passed);
    checks.push(subcheck(
        "cross_mint_every_valid_leg_purpose_survives_every_crash_window",
        every_leg_purpose_crash_window,
        json!({
            "withdrawOptimizeYield": withdraw_crash_windows,
            "swapOptimizeYield": swap_crash_windows,
            "depositOptimizeYield": deposit_crash_windows,
            "depositRecoverSource": recovery_crash_windows,
            "depositFallbackTarget": fallback_crash_windows,
            "windows": [
                "before_persistence",
                "persisted_prebroadcast",
                "broadcast_prestatus",
                "finalized_prereconcile",
                "reconciled_precontinuation",
            ],
        }),
    ));

    let seed_mismatch_cluster = fixture.cluster("cross_mint_policy_seed_mismatch");
    let seed_mismatch_epoch = fixture.seed_epoch(&seed_mismatch_cluster).await?;
    seed_cross_mint_ready_opportunity(
        fixture,
        &seed_mismatch_cluster,
        seed_mismatch_epoch,
        "cross_mint_policy_seed_mismatch",
        50_000,
    )
    .await?;
    set_cross_mint_gates(fixture, &seed_mismatch_cluster, true, true).await?;
    let seed_mismatch_lease = claim_one(
        &fixture.client,
        &seed_mismatch_cluster,
        "cross-mint-policy-seed-mismatch",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let seed_mismatch_capacity =
        cross_mint_capacity_input_for_lease(fixture, &seed_mismatch_lease).await?;
    let seed_mismatch_bindings =
        cross_mint_fixture_policy_bindings(&seed_mismatch_lease.opportunity)?;
    let corrupted_opt_in = sqlx::query(
        r#"
        UPDATE loyal_yield.cross_mint_vault_opt_ins
        SET classic_policy_seed = classic_policy_seed + 1000000,
            updated_at = now()
        WHERE cluster = $1
        "#,
    )
    .bind(&seed_mismatch_cluster)
    .execute(fixture.client.pool())
    .await?;
    let seed_mismatch_error = fixture
        .client
        .activate_cross_mint_movement(
            &seed_mismatch_lease,
            CrossMintMovementActivationInput {
                capacity: seed_mismatch_capacity,
                initial_withdraw_compiled_fee_lamports: 5_000,
                preflight_certification: json!({
                    "kind": "cross_mint_preflight",
                    "fixture": "policy_seed_mismatch",
                }),
                policy_bindings: seed_mismatch_bindings,
            },
        )
        .await
        .expect_err("a mismatched enrolled policy seed cannot activate a movement");
    checks.push(subcheck(
        "cross_mint_activation_requires_exact_opted_in_policy_seed",
        corrupted_opt_in.rows_affected() == 1
            && seed_mismatch_error
                .to_string()
                .contains("lost an opted-in finalized policy binding"),
        json!({
            "corruptedEnrollmentRows": corrupted_opt_in.rows_affected(),
            "activationError": seed_mismatch_error.to_string(),
        }),
    ));

    let paused_before_activation_cluster = fixture.cluster("cross_mint_paused_before_activation");
    let paused_before_activation_epoch = fixture
        .seed_epoch(&paused_before_activation_cluster)
        .await?;
    seed_cross_mint_ready_opportunity(
        fixture,
        &paused_before_activation_cluster,
        paused_before_activation_epoch,
        "cross_mint_paused_before_activation",
        50_000,
    )
    .await?;
    set_cross_mint_gates(fixture, &paused_before_activation_cluster, true, true).await?;
    let paused_before_activation_lease = claim_one(
        &fixture.client,
        &paused_before_activation_cluster,
        "cross-mint-paused-before-activation",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let paused_before_activation_capacity =
        cross_mint_capacity_input_for_lease(fixture, &paused_before_activation_lease).await?;
    let paused_before_activation_bindings =
        cross_mint_fixture_policy_bindings(&paused_before_activation_lease.opportunity)?;
    let paused_before_activation_lookup = CrossMintVaultOptInLookup {
        cluster: paused_before_activation_cluster.clone(),
        settings: paused_before_activation_bindings.settings.clone(),
        vault_index: paused_before_activation_bindings.vault_index,
        vault_pubkey: paused_before_activation_bindings.vault_pubkey.clone(),
    };
    let activation_opt_in = fixture
        .client
        .load_cross_mint_vault_opt_in(paused_before_activation_lookup.clone())
        .await?
        .ok_or("pre-activation fixture lacks its enrollment")?;
    let paused_before_activation = fixture
        .client
        .disable_cross_mint_vault_opt_in(
            paused_before_activation_lookup.clone(),
            activation_opt_in.generation,
        )
        .await?
        .ok_or("pre-activation enrollment disappeared while pausing")?;
    let resumed_before_activation = fixture
        .client
        .enable_cross_mint_vault_opt_in(
            paused_before_activation_lookup,
            paused_before_activation.generation,
        )
        .await?
        .ok_or("pre-activation enrollment disappeared while resuming")?;
    let paused_activation_error = fixture
        .client
        .activate_cross_mint_movement(
            &paused_before_activation_lease,
            CrossMintMovementActivationInput {
                capacity: paused_before_activation_capacity,
                initial_withdraw_compiled_fee_lamports: 5_000,
                preflight_certification: json!({
                    "kind": "cross_mint_preflight",
                    "fixture": "paused_before_activation",
                }),
                policy_bindings: paused_before_activation_bindings.clone(),
            },
        )
        .await
        .expect_err("a plan from before pause and resume cannot activate a new movement");

    let paused_before_withdraw =
        activate_cross_mint_fixture(fixture, "cross_mint_paused_before_withdraw").await?;
    let paused_before_withdraw_lease = fixture
        .client
        .claim_cross_mint_continuation(
            &paused_before_withdraw.movement.cluster,
            "paused-before-withdraw",
            60,
        )
        .await?
        .ok_or("paused-before-withdraw movement was not claimable")?;
    let paused_before_withdraw_input = cross_mint_leg_input(
        fixture,
        &paused_before_withdraw.opportunity_lease,
        &paused_before_withdraw_lease,
        "paused-before-withdraw",
        CrossMintMovementLeg::Withdraw,
        CrossMintLegPurpose::OptimizeYield,
        1,
        CrossMintExpectedEffect {
            debit: None,
            credit_mint: Some("USDC".to_owned()),
            credit_token_account: Some(format!(
                "source-idle:{}",
                paused_before_withdraw.movement.decision_id.as_i64()
            )),
            minimum_credit_amount_raw: Some(600_000),
        },
    )
    .await?;
    let paused_before_withdraw_lookup = CrossMintVaultOptInLookup {
        cluster: paused_before_withdraw.movement.cluster.clone(),
        settings: paused_before_withdraw.policy_bindings.settings.clone(),
        vault_index: paused_before_withdraw.policy_bindings.vault_index,
        vault_pubkey: paused_before_withdraw.policy_bindings.vault_pubkey.clone(),
    };
    let withdraw_opt_in = fixture
        .client
        .load_cross_mint_vault_opt_in(paused_before_withdraw_lookup.clone())
        .await?
        .ok_or("pre-withdraw fixture lacks its enrollment")?;
    let paused_before_publication = fixture
        .client
        .disable_cross_mint_vault_opt_in(
            paused_before_withdraw_lookup.clone(),
            withdraw_opt_in.generation,
        )
        .await?
        .ok_or("pre-withdraw enrollment disappeared while pausing")?;
    let resumed_before_publication = fixture
        .client
        .enable_cross_mint_vault_opt_in(
            paused_before_withdraw_lookup,
            paused_before_publication.generation,
        )
        .await?
        .ok_or("pre-withdraw enrollment disappeared while resuming")?;
    let paused_withdraw_rejected = fixture
        .client
        .append_cross_mint_leg(&paused_before_withdraw_lease, paused_before_withdraw_input)
        .await
        .is_err();
    let paused_withdraw_signed_count: i64 = sqlx::query_scalar(
        "SELECT count(*)::BIGINT FROM loyal_yield.signed_route_submissions WHERE decision_id = $1",
    )
    .bind(paused_before_withdraw.movement.decision_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let paused_before_withdraw_closed = fixture
        .client
        .close_cross_mint_movement(
            &paused_before_withdraw_lease,
            CrossMintMovementCloseInput {
                outcome: CrossMintTerminalOutcome::CancelledBeforeWithdraw,
                observed_slot: 10_640,
                reason: "start_authority_revoked_before_withdraw".to_owned(),
                evidence: json!({
                    "kind": "start_authority_revoked_before_withdraw",
                    "cause": "stale_enrollment_generation",
                    "optInGeneration": resumed_before_publication.generation,
                }),
            },
        )
        .await?;
    checks.push(subcheck(
        "cross_mint_pause_fences_activation_and_initial_withdraw",
        !paused_before_activation.enabled
            && resumed_before_activation.enabled
            && resumed_before_activation.generation
                > paused_before_activation_bindings.swap.enrollment_generation
            && paused_activation_error
                .to_string()
                .contains("lost an opted-in finalized policy binding")
            && !paused_before_publication.enabled
            && resumed_before_publication.enabled
            && resumed_before_publication.generation
                > paused_before_withdraw.policy_bindings.swap.enrollment_generation
            && paused_withdraw_rejected
            && paused_withdraw_signed_count == 0
            && paused_before_withdraw_closed.terminal_outcome
                == Some(CrossMintTerminalOutcome::CancelledBeforeWithdraw),
        json!({
            "activationEnabledAfterPause": paused_before_activation.enabled,
            "activationEnabledAfterResume": resumed_before_activation.enabled,
            "activationPlannedGeneration": paused_before_activation_bindings.swap.enrollment_generation,
            "activationCurrentGeneration": resumed_before_activation.generation,
            "activationError": paused_activation_error.to_string(),
            "publicationEnabledAfterPause": paused_before_publication.enabled,
            "publicationEnabledAfterResume": resumed_before_publication.enabled,
            "publicationPlannedGeneration": paused_before_withdraw.policy_bindings.swap.enrollment_generation,
            "publicationCurrentGeneration": resumed_before_publication.generation,
            "publicationRejected": paused_withdraw_rejected,
            "signedSubmissionCount": paused_withdraw_signed_count,
            "terminalOutcome": paused_before_withdraw_closed.terminal_outcome,
        }),
    ));

    let revoked_start =
        activate_cross_mint_fixture(fixture, "cross_mint_policy_revoked_before_withdraw").await?;
    let revoked_start_lease = fixture
        .client
        .claim_cross_mint_continuation(
            &revoked_start.movement.cluster,
            "policy-revoked-before-withdraw",
            60,
        )
        .await?
        .ok_or("policy-revocation movement was not claimable")?;
    let revoked_start_input = cross_mint_leg_input(
        fixture,
        &revoked_start.opportunity_lease,
        &revoked_start_lease,
        "policy-revoked-before-withdraw",
        CrossMintMovementLeg::Withdraw,
        CrossMintLegPurpose::OptimizeYield,
        1,
        CrossMintExpectedEffect {
            debit: None,
            credit_mint: Some("USDC".to_owned()),
            credit_token_account: Some(format!(
                "source-idle:{}",
                revoked_start.movement.decision_id.as_i64()
            )),
            minimum_credit_amount_raw: Some(600_000),
        },
    )
    .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.cross_mint_swap_policies
        SET active = FALSE,
            start_eligible = FALSE,
            last_mutation = 'remove',
            last_seen_slot = last_seen_slot + 1,
            last_seen_signature = 'verifier-revoked-before-withdraw'
        WHERE cluster = $1 AND policy_account = $2
        "#,
    )
    .bind(&revoked_start.movement.cluster)
    .bind(&revoked_start.policy_bindings.swap.policy_account)
    .execute(fixture.client.pool())
    .await?;
    let revoked_start_rejected = fixture
        .client
        .append_cross_mint_leg(&revoked_start_lease, revoked_start_input)
        .await
        .is_err();
    let revoked_start_signed_count: i64 = sqlx::query_scalar(
        "SELECT count(*)::BIGINT FROM loyal_yield.signed_route_submissions WHERE decision_id = $1",
    )
    .bind(revoked_start.movement.decision_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let revoked_start_cancelled = fixture
        .client
        .close_cross_mint_movement(
            &revoked_start_lease,
            CrossMintMovementCloseInput {
                outcome: CrossMintTerminalOutcome::CancelledBeforeWithdraw,
                observed_slot: 10_650,
                reason: "start_authority_revoked_before_withdraw".to_owned(),
                evidence: json!({
                    "kind": "start_authority_revoked_before_withdraw",
                    "policyAccount": revoked_start.policy_bindings.swap.policy_account,
                }),
            },
        )
        .await?;
    let revoked_start_capacity =
        cross_mint_capacity_state(fixture, revoked_start.movement.decision_id.as_i64()).await?;
    checks.push(subcheck(
        "cross_mint_policy_revocation_linearizes_before_initial_signature_admission",
        revoked_start_rejected
            && revoked_start_signed_count == 0
            && revoked_start_cancelled.terminal_outcome
                == Some(CrossMintTerminalOutcome::CancelledBeforeWithdraw)
            && revoked_start_cancelled.custody_version == 0
            && revoked_start_capacity.1 == "released",
        json!({
            "publicationRejected": revoked_start_rejected,
            "signedSubmissionCount": revoked_start_signed_count,
            "terminalOutcome": revoked_start_cancelled.terminal_outcome,
            "custodyVersion": revoked_start_cancelled.custody_version,
            "capacityState": revoked_start_capacity.1,
        }),
    ));

    let default_gate_values = fixture
        .client
        .cross_mint_movement_gates(&format!("{}_missing_gate", fixture.prefix))
        .await?;

    let gate_race = activate_cross_mint_fixture(fixture, "cross_mint_gate_race").await?;
    let stale_control_lease = fixture
        .client
        .claim_cross_mint_continuation(&gate_race.movement.cluster, "gate-race-before-disable", 60)
        .await?
        .ok_or("gate-race movement was not claimable")?;
    let stale_control_account = format!("source-idle:{}", gate_race.movement.decision_id.as_i64());
    let stale_control_input = cross_mint_leg_input(
        fixture,
        &gate_race.opportunity_lease,
        &stale_control_lease,
        "gate-race-stale-publication",
        CrossMintMovementLeg::Withdraw,
        CrossMintLegPurpose::OptimizeYield,
        1,
        CrossMintExpectedEffect {
            debit: None,
            credit_mint: Some("USDC".to_owned()),
            credit_token_account: Some(stale_control_account),
            minimum_credit_amount_raw: Some(600_000),
        },
    )
    .await?;
    set_cross_mint_gates(fixture, &gate_race.movement.cluster, false, false).await?;
    let stale_control_publish_rejected = fixture
        .client
        .append_cross_mint_leg(&stale_control_lease, stale_control_input)
        .await
        .is_err();
    let disabled_race_claim = fixture
        .client
        .claim_cross_mint_continuation(&gate_race.movement.cluster, "gate-race-disabled-claim", 60)
        .await?;

    let gates = activate_cross_mint_fixture(fixture, "cross_mint_gates").await?;
    let continuing = claim_cross_mint_continuation(
        fixture,
        &gates.movement.cluster,
        "gates-withdraw-before-disable",
    )
    .await?;
    let gate_idle_account = format!("source-idle:{}", gates.movement.decision_id.as_i64());
    let gate_withdraw = fixture
        .client
        .append_cross_mint_leg(
            &continuing,
            cross_mint_leg_input(
                fixture,
                &gates.opportunity_lease,
                &continuing,
                "policy-revoked-after-withdraw",
                CrossMintMovementLeg::Withdraw,
                CrossMintLegPurpose::OptimizeYield,
                1,
                CrossMintExpectedEffect {
                    debit: None,
                    credit_mint: Some("USDC".to_owned()),
                    credit_token_account: Some(gate_idle_account.clone()),
                    minimum_credit_amount_raw: Some(600_000),
                },
            )
            .await?,
        )
        .await?;
    let gate_race_cancelled = fixture
        .client
        .close_cross_mint_movement(
            &stale_control_lease,
            CrossMintMovementCloseInput {
                outcome: CrossMintTerminalOutcome::CancelledBeforeWithdraw,
                observed_slot: 10_675,
                reason: "start_authority_revoked_before_withdraw".to_owned(),
                evidence: json!({
                    "kind": "start_authority_revoked_before_withdraw",
                    "controlGeneration": stale_control_lease.control_generation,
                }),
            },
        )
        .await?;
    let gate_race_capacity =
        cross_mint_capacity_state(fixture, gate_race.movement.decision_id.as_i64()).await?;
    let (gate_idle, _, _) = finalize_and_reconcile_cross_mint_leg(
        fixture,
        &gates.movement.cluster,
        gate_withdraw.id,
        10_699,
        CrossMintReconciledEffect {
            debit: None,
            credit: Some(TokenBalanceDelta {
                mint: "USDC".to_owned(),
                token_account: gate_idle_account,
                amount_raw: 610_000,
            }),
        },
    )
    .await?;
    set_cross_mint_gates(fixture, &gates.movement.cluster, false, false).await?;
    let continuation_blocked = fixture
        .client
        .claim_cross_mint_continuation(
            &gates.movement.cluster,
            "continuation-disabled-must-not-claim",
            60,
        )
        .await?;
    set_cross_mint_gates(fixture, &gates.movement.cluster, false, true).await?;
    let gate_values = fixture
        .client
        .cross_mint_movement_gates(&gates.movement.cluster)
        .await?;
    let continuing = claim_cross_mint_continuation(
        fixture,
        &gates.movement.cluster,
        "policy-revoked-idle-custody",
    )
    .await?;
    let blocked_seed = seed_cross_mint_ready_opportunity(
        fixture,
        &gates.movement.cluster,
        gates.opportunity_lease.opportunity.optimizer_epoch_id,
        "cross-mint-gates-blocked",
        40_000,
    )
    .await?;
    let blocked_lease = claim_one(
        &fixture.client,
        &gates.movement.cluster,
        "gates-blocked-start",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    if blocked_lease.opportunity.id != blocked_seed.id {
        return Err("gate fixture claimed an unexpected opportunity".into());
    }
    let blocked_capacity = cross_mint_capacity_input_for_lease(fixture, &blocked_lease).await?;
    let blocked_policy_bindings = cross_mint_fixture_policy_bindings(&blocked_lease.opportunity)?;
    let blocked_start = fixture
        .client
        .activate_cross_mint_movement(
            &blocked_lease,
            CrossMintMovementActivationInput {
                capacity: blocked_capacity,
                initial_withdraw_compiled_fee_lamports: 5_000,
                preflight_certification: json!({
                    "kind": "cross_mint_preflight",
                    "fixture": "blocked_start",
                }),
                policy_bindings: blocked_policy_bindings,
            },
        )
        .await;
    let closed = fixture
        .client
        .close_cross_mint_movement(
            &continuing,
            CrossMintMovementCloseInput {
                outcome: CrossMintTerminalOutcome::ManualIntervention,
                observed_slot: 10_700,
                reason: "verifier policy-revocation fixture".to_owned(),
                evidence: json!({
                    "kind": "policy_revoked",
                    "policyAccount": format!("policy:{}", fixture.prefix),
                    "custodyMint": gate_idle.custody_mint,
                    "custodyAccount": gate_idle.custody_account,
                    "attributedAmountRaw": gate_idle.custody_amount_raw,
                    "observedAggregateAmountRaw": gate_idle.custody_observed_balance_raw,
                }),
            },
        )
        .await?;
    let closed_capacity =
        cross_mint_capacity_state(fixture, gates.movement.decision_id.as_i64()).await?;
    checks.push(subcheck(
        "cross_mint_start_and_continue_gates_are_independent",
        !gate_values.start_new_movements
            && gate_values.continue_or_recover_existing
            && !default_gate_values.start_new_movements
            && default_gate_values.continue_or_recover_existing
            && continuation_blocked.is_none()
            && disabled_race_claim.is_none()
            && stale_control_publish_rejected
            && gate_race_cancelled.terminal_outcome
                == Some(CrossMintTerminalOutcome::CancelledBeforeWithdraw)
            && gate_race_capacity.1 == "released"
            && blocked_start.is_err()
            && continuing.movement.decision_id == gates.movement.decision_id,
        json!({
            "startNewMovements": gate_values.start_new_movements,
            "continueOrRecoverExisting": gate_values.continue_or_recover_existing,
            "missingGateStartDefault": default_gate_values.start_new_movements,
            "missingGateContinueDefault": default_gate_values.continue_or_recover_existing,
            "continuationClaimedWhileDisabled": continuation_blocked.is_some(),
            "staleGenerationPublishRejected": stale_control_publish_rejected,
            "disabledRaceClaimed": disabled_race_claim.is_some(),
            "gateRaceTerminalOutcome": gate_race_cancelled.terminal_outcome,
            "gateRaceCapacityState": gate_race_capacity.1,
            "staleControlGeneration": stale_control_lease.control_generation,
            "currentControlGeneration": gate_values.generation,
            "blockedStartError": blocked_start.err().map(|error| error.to_string()),
            "continuedDecisionId": continuing.movement.decision_id.as_i64(),
        }),
    ));
    checks.push(subcheck(
        "cross_mint_manual_closure_requires_evidence",
        closed.phase == CrossMintCustodyPhase::ManualIntervention
            && closed.terminal_outcome == Some(CrossMintTerminalOutcome::ManualIntervention)
            && closed.terminal_evidence.is_some()
            && closed.terminal_reason.as_deref() == Some("verifier policy-revocation fixture")
            && gate_idle.phase == CrossMintCustodyPhase::SourceIdle
            && gate_idle.custody_amount_raw == 610_000
            && closed_capacity.1 == "released",
        json!({
            "phase": closed.phase,
            "terminalOutcome": closed.terminal_outcome,
            "terminalEvidence": closed.terminal_evidence,
            "terminalReason": closed.terminal_reason,
            "capacityState": closed_capacity.1,
        }),
    ));

    checks.push(subcheck(
        "same_mint_signed_lifecycle_regression_remains_behavioral",
        same_mint_regression_passed,
        same_mint_regression_evidence,
    ));
    Ok(checks)
}

async fn run_database_checks(
    fixture: &DatabaseFixture,
) -> Result<DatabaseEvidence, Box<dyn Error>> {
    let database_deadlocks_before: i64 = sqlx::query_scalar(
        "SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()",
    )
    .fetch_one(fixture.client.pool())
    .await?;

    let readiness_serialization_cluster = fixture.cluster("readiness_serialization");
    let readiness_serialization_epoch =
        fixture.seed_epoch(&readiness_serialization_cluster).await?;
    let readiness_serialization_opportunity = fixture
        .seed_opportunity(
            &readiness_serialization_cluster,
            readiness_serialization_epoch,
            "readiness-serialization",
            "waiting_alt",
            1_000,
        )
        .await?;
    let readiness_serialization_vault_id: i64 = sqlx::query_scalar(
        "SELECT vault_id FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(readiness_serialization_opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let mut readiness_guard = fixture.client.pool().begin().await?;
    sqlx::query(
        "SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-readiness:' || $1 || ':' || $2::TEXT, 0))",
    )
    .bind(&readiness_serialization_cluster)
    .bind(readiness_serialization_vault_id)
    .execute(&mut *readiness_guard)
    .await?;
    let readiness_task = |ordinal: u8| {
        let client = fixture.client.clone();
        let cluster = readiness_serialization_cluster.clone();
        let vault_id = VaultId(readiness_serialization_vault_id);
        let route_fingerprint = format!("{}:readiness-route:{ordinal}", fixture.prefix);
        let requirements_fingerprint =
            format!("{}:readiness-requirements:{ordinal}", fixture.prefix);
        tokio::spawn(async move {
            let now = Utc::now();
            client
                .upsert_lookup_table_readiness(LookupTableReadinessRecord {
                    cluster,
                    vault_id,
                    route_fingerprint,
                    requirements_fingerprint,
                    route_kind: "fleet_verifier_readiness_serialization".to_owned(),
                    source_reserve: None,
                    target_reserve: None,
                    manifest_id: None,
                    shared_family_id: None,
                    vault_binding_id: None,
                    readiness_state: LookupTableReadinessStatus::Incomplete,
                    required_address_count: 0,
                    covered_address_count: 0,
                    missing_addresses: json!([]),
                    legacy_table_ids: Vec::new(),
                    reusable_table_ids: Vec::new(),
                    compiled_message_size: None,
                    packet_limit: Some(1232),
                    observed_slot: Some(90_000 + i64::from(ordinal)),
                    observed_at: now,
                    selection_kind: Some(LookupTableSelectionKind::Blocked),
                    fallback_reason: Some("fleet_verifier_serialization".to_owned()),
                    rollout_mode: Some(LookupTableRolloutMode::ReusableOnly),
                    selected_table_ids: Vec::new(),
                    selected_table_count: Some(0),
                    packet_fits: None,
                    simulation_state: Some(LookupTableSimulationState::NotRun),
                    simulation_units_consumed: None,
                    simulation_error: None,
                    updated_at: now,
                })
                .await
        })
    };
    let first_readiness_writer = readiness_task(1);
    let second_readiness_writer = readiness_task(2);
    tokio::time::sleep(Duration::from_millis(75)).await;
    let readiness_writers_waited =
        !first_readiness_writer.is_finished() && !second_readiness_writer.is_finished();
    readiness_guard.commit().await?;
    first_readiness_writer.await??;
    second_readiness_writer.await??;
    let serialized_readiness_row_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_route_readiness_current WHERE cluster = $1 AND vault_id = $2",
    )
    .bind(&readiness_serialization_cluster)
    .bind(readiness_serialization_vault_id)
    .fetch_one(fixture.client.pool())
    .await?;
    sqlx::query(
        "DELETE FROM loyal_yield.lookup_table_route_readiness_current WHERE cluster = $1 AND vault_id = $2",
    )
    .bind(&readiness_serialization_cluster)
    .bind(readiness_serialization_vault_id)
    .execute(fixture.client.pool())
    .await?;

    // Recreate the production family/physical-row lock graph. Readiness must
    // wait on the logical parent without owning the physical ALT row, allowing
    // a generation lifecycle transaction to take that row and finish.
    let readiness_family_id: i64 =
        sqlx::query_scalar("SELECT id FROM loyal_yield.lookup_table_families WHERE cluster = $1")
            .bind(&readiness_serialization_cluster)
            .fetch_one(fixture.client.pool())
            .await?;
    let readiness_table_id: i64 = sqlx::query_scalar(
        "SELECT id FROM loyal_yield.route_lookup_tables WHERE cluster = $1 AND family_id = $2",
    )
    .bind(&readiness_serialization_cluster)
    .bind(readiness_family_id)
    .fetch_one(fixture.client.pool())
    .await?;
    let mut lifecycle_guard = fixture.client.pool().begin().await?;
    sqlx::query("SELECT id FROM loyal_yield.lookup_table_families WHERE id = $1 FOR UPDATE")
        .bind(readiness_family_id)
        .execute(&mut *lifecycle_guard)
        .await?;
    let parent_order_client = fixture.client.clone();
    let parent_order_cluster = readiness_serialization_cluster.clone();
    let parent_order_task = tokio::spawn(async move {
        let now = Utc::now();
        parent_order_client
            .upsert_lookup_table_readiness(LookupTableReadinessRecord {
                cluster: parent_order_cluster,
                vault_id: VaultId(readiness_serialization_vault_id),
                route_fingerprint: "fleet-verifier-parent-order-route".to_owned(),
                requirements_fingerprint: "fleet-verifier-parent-order-requirements".to_owned(),
                route_kind: "fleet_verifier_parent_order".to_owned(),
                source_reserve: None,
                target_reserve: None,
                manifest_id: None,
                shared_family_id: Some(readiness_family_id),
                vault_binding_id: None,
                readiness_state: LookupTableReadinessStatus::Ready,
                required_address_count: 1,
                covered_address_count: 1,
                missing_addresses: json!([]),
                legacy_table_ids: Vec::new(),
                reusable_table_ids: vec![readiness_table_id],
                compiled_message_size: Some(256),
                packet_limit: Some(1232),
                observed_slot: Some(90_100),
                observed_at: now,
                selection_kind: Some(LookupTableSelectionKind::Reusable),
                fallback_reason: None,
                rollout_mode: Some(LookupTableRolloutMode::ReusableOnly),
                selected_table_ids: vec![readiness_table_id],
                selected_table_count: Some(1),
                packet_fits: Some(true),
                simulation_state: Some(LookupTableSimulationState::Succeeded),
                simulation_units_consumed: Some(1),
                simulation_error: None,
                updated_at: now,
            })
            .await
    });
    let mut readiness_waited_on_parent = false;
    for _ in 0..200 {
        readiness_waited_on_parent = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1 FROM pg_stat_activity
                WHERE datname = current_database()
                  AND pid <> pg_backend_pid()
                  AND state = 'active'
                  AND wait_event_type = 'Lock'
                  AND query LIKE '%FROM loyal_yield.lookup_table_families%'
                  AND query LIKE '%FOR SHARE%'
            )
            "#,
        )
        .fetch_one(fixture.client.pool())
        .await?;
        if readiness_waited_on_parent {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let lifecycle_physical_lock_result = sqlx::query(
        "SELECT id FROM loyal_yield.route_lookup_tables WHERE id = $1 FOR UPDATE NOWAIT",
    )
    .bind(readiness_table_id)
    .execute(&mut *lifecycle_guard)
    .await;
    let lifecycle_acquired_physical_while_readiness_waited = lifecycle_physical_lock_result.is_ok();
    lifecycle_guard.commit().await?;
    tokio::time::timeout(Duration::from_secs(10), parent_order_task)
        .await
        .map_err(|_| "readiness parent-order writer did not finish after lifecycle commit")?
        .map_err(|error| format!("readiness parent-order task failed: {error}"))??;
    let readiness_parent_before_physical_lock_order_proved =
        readiness_waited_on_parent && lifecycle_acquired_physical_while_readiness_waited;
    sqlx::query(
        "DELETE FROM loyal_yield.lookup_table_route_readiness_current WHERE cluster = $1 AND vault_id = $2",
    )
    .bind(&readiness_serialization_cluster)
    .bind(readiness_serialization_vault_id)
    .execute(fixture.client.pool())
    .await?;

    let empty_status_cluster = fixture.cluster("empty_status");
    fixture
        .client
        .register_fleet_planning_cluster(&empty_status_cluster)
        .await?;
    fixture
        .client
        .heartbeat_fleet_planning_cluster(&empty_status_cluster)
        .await?;
    let empty_status_epoch = fixture.seed_epoch(&empty_status_cluster).await?;
    let empty_status_rows = fixture
        .client
        .fleet_orchestration_status_source(&empty_status_cluster)
        .await?;
    let empty_status_visible = empty_status_rows.len() == 1
        && empty_status_rows[0].opportunity_state.is_none()
        && empty_status_rows[0].opportunity_count == 0
        && empty_status_rows[0].latest_market_epoch_id == Some(empty_status_epoch)
        && empty_status_rows[0].planner_registered_at.is_some()
        && empty_status_rows[0]
            .planner_last_seen_age_seconds
            .is_some_and(|age| (0..=5).contains(&age))
        && empty_status_rows[0].latest_market_epoch_expired == Some(false)
        && empty_status_rows[0].waiting_alt_opportunity_count == 0
        && empty_status_rows[0].ready_opportunity_count == 0
        && empty_status_rows[0].sender_submission_count == 0
        && empty_status_rows[0].confirmer_submission_count == 0
        && empty_status_rows[0].reconciler_submission_count == 0
        && empty_status_rows[0].current_epoch_opportunity_count == 0;

    // Force the production race at the exact insert boundary. The direct
    // writer changes the active occupant from revalidate to leased while
    // holding its row lock. Publication observes the pre-update row at its
    // lease probe, then blocks trying to supersede it. Once the writer commits,
    // PostgreSQL rechecks the supersede predicate, preserves the leased
    // occupant, and the real active-slot trigger rejects the competing insert.
    let active_slot_cluster = fixture.cluster("active_slot_conflict");
    let active_slot_epoch = fixture.seed_epoch(&active_slot_cluster).await?;
    let active_slot_seed = fixture
        .seed_opportunity(
            &active_slot_cluster,
            active_slot_epoch,
            "active-slot-owner",
            "revalidate",
            1_500,
        )
        .await?;
    let active_slot_owner = fixture
        .client
        .rebalance_opportunity(active_slot_seed.id)
        .await?
        .ok_or("active-slot owner disappeared before conflict fixture")?;
    let mut active_slot_competing_input = rediscovery_input_for_opportunity(&active_slot_owner);
    active_slot_competing_input.route_fingerprint =
        Some(format!("route:{}:active-slot-competitor", fixture.prefix));
    active_slot_competing_input.requirements_fingerprint = Some(format!(
        "requirements:{}:active-slot-competitor",
        fixture.prefix
    ));
    active_slot_competing_input.economic_priority += 1;
    active_slot_competing_input.available_at = Utc::now();

    let mut active_slot_direct_writer = fixture.client.pool().begin().await?;
    let active_slot_direct_write_count = sqlx::query(
        r#"
        UPDATE loyal_yield.rebalance_opportunities
        SET opportunity_state = 'leased',
            lease_kind = 'revalidate',
            lease_owner = $2,
            lease_expires_at = clock_timestamp() + interval '5 minutes',
            fencing_token = fencing_token + 1,
            attempt_count = attempt_count + 1,
            updated_at = clock_timestamp()
        WHERE id = $1 AND opportunity_state = 'revalidate'
        "#,
    )
    .bind(active_slot_seed.id)
    .bind(format!("{}:active-slot-direct-writer", fixture.prefix))
    .execute(&mut *active_slot_direct_writer)
    .await?
    .rows_affected();

    let active_slot_publish_client = fixture.client.clone();
    let active_slot_publish_task = tokio::spawn(async move {
        active_slot_publish_client
            .upsert_rebalance_opportunity(active_slot_competing_input)
            .await
    });
    let mut active_slot_publication_wait_observed = false;
    for _ in 0..200 {
        active_slot_publication_wait_observed = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM pg_stat_activity
                WHERE datname = current_database()
                  AND pid <> pg_backend_pid()
                  AND state = 'active'
                  AND wait_event_type = 'Lock'
                  AND query LIKE '%UPDATE loyal_yield.rebalance_opportunities%'
                  AND query LIKE '%opportunity_state = ''superseded''%'
            )
            "#,
        )
        .fetch_one(fixture.client.pool())
        .await?;
        if active_slot_publication_wait_observed {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    active_slot_direct_writer.commit().await?;
    let active_slot_publish_result =
        tokio::time::timeout(Duration::from_secs(10), active_slot_publish_task)
            .await
            .map_err(|_| "active-slot conflict publication did not finish after writer commit")?
            .map_err(|error| format!("active-slot publication task failed: {error}"))?;
    let (
        active_slot_typed_deferral,
        active_slot_returned_vault_id,
        active_slot_returned_opportunity_id,
        active_slot_returned_state,
        active_slot_returned_reason,
    ) = match &active_slot_publish_result {
        Err(OrchestratorError::OpportunityDeferredBehindActiveSlot {
            vault_id,
            slot_opportunity_id,
            slot_opportunity_state,
            reason,
        }) => (
            true,
            Some(vault_id.as_i64()),
            *slot_opportunity_id,
            slot_opportunity_state.clone(),
            Some(*reason),
        ),
        _ => (false, None, None, None, None),
    };
    let active_slot_opportunity_rows: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.rebalance_opportunities WHERE cluster = $1 AND vault_id = $2",
    )
    .bind(&active_slot_cluster)
    .bind(active_slot_owner.vault_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let active_slot_rows: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.active_rebalance_opportunity_slots WHERE cluster = $1 AND vault_id = $2",
    )
    .bind(&active_slot_cluster)
    .bind(active_slot_owner.vault_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let active_slot_conflict_is_contained = active_slot_direct_write_count == 1
        && active_slot_publication_wait_observed
        && active_slot_typed_deferral
        && active_slot_returned_vault_id == Some(active_slot_owner.vault_id.as_i64())
        && active_slot_returned_opportunity_id == Some(active_slot_seed.id)
        && active_slot_returned_state.as_deref() == Some("leased")
        && active_slot_returned_reason == Some("active_slot_owner_valid")
        && active_slot_opportunity_rows == 1
        && active_slot_rows == 1;

    let priority_cluster = fixture.cluster("priority");
    let priority_epoch = fixture.seed_epoch(&priority_cluster).await?;
    let low = fixture
        .seed_opportunity(
            &priority_cluster,
            priority_epoch,
            "priority-low",
            "ready",
            100,
        )
        .await?;
    let high = fixture
        .seed_opportunity(
            &priority_cluster,
            priority_epoch,
            "priority-high",
            "ready",
            10_000,
        )
        .await?;
    let revalidate = fixture
        .seed_opportunity(
            &priority_cluster,
            priority_epoch,
            "priority-revalidate",
            "revalidate",
            20_000,
        )
        .await?;
    let waiting = fixture
        .seed_opportunity(
            &priority_cluster,
            priority_epoch,
            "priority-waiting",
            "waiting_alt",
            30_000,
        )
        .await?;
    let high_claim = claim_one(
        &fixture.client,
        &priority_cluster,
        "priority-executor",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let revalidate_claim = claim_one(
        &fixture.client,
        &priority_cluster,
        "priority-revalidator",
        RebalanceOpportunityClaimKind::Revalidate,
    )
    .await?;
    let waiting_state: String = sqlx::query_scalar(
        "SELECT opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(waiting.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let low_state: String = sqlx::query_scalar(
        "SELECT opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(low.id)
    .fetch_one(fixture.client.pool())
    .await?;

    let skip_locked_cluster = fixture.cluster("skip_locked");
    let skip_locked_epoch = fixture.seed_epoch(&skip_locked_cluster).await?;
    let skip_a = fixture
        .seed_opportunity(
            &skip_locked_cluster,
            skip_locked_epoch,
            "skip-a",
            "ready",
            1_000,
        )
        .await?;
    let skip_b = fixture
        .seed_opportunity(
            &skip_locked_cluster,
            skip_locked_epoch,
            "skip-b",
            "ready",
            900,
        )
        .await?;
    let expiry = Utc::now() + chrono::Duration::minutes(5);
    let claim_a = fixture.client.lease_next_rebalance_opportunity(
        &skip_locked_cluster,
        "skip-worker-a",
        RebalanceOpportunityClaimKind::Execute,
        expiry,
    );
    let claim_b = fixture.client.lease_next_rebalance_opportunity(
        &skip_locked_cluster,
        "skip-worker-b",
        RebalanceOpportunityClaimKind::Execute,
        expiry,
    );
    let (claim_a, claim_b) = tokio::join!(claim_a, claim_b);
    let claim_a = claim_a?.ok_or("first concurrent claim returned no row")?;
    let claim_b = claim_b?.ok_or("second concurrent claim returned no row")?;
    let concurrent_ids = [claim_a.opportunity.id, claim_b.opportunity.id];
    let skip_locked_passed = claim_a.opportunity.id != claim_b.opportunity.id
        && concurrent_ids.contains(&skip_a.id)
        && concurrent_ids.contains(&skip_b.id);

    let batch_cluster = fixture.cluster("batch_claim");
    let batch_epoch = fixture.seed_epoch(&batch_cluster).await?;
    let starved = fixture
        .seed_opportunity(&batch_cluster, batch_epoch, "batch-starved", "ready", 1)
        .await?;
    let batch_high = fixture
        .seed_opportunity(&batch_cluster, batch_epoch, "batch-high", "ready", 30_000)
        .await?;
    let batch_medium = fixture
        .seed_opportunity(&batch_cluster, batch_epoch, "batch-medium", "ready", 20_000)
        .await?;
    let young_low = fixture
        .seed_opportunity(&batch_cluster, batch_epoch, "batch-young-low", "ready", 2)
        .await?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_opportunities SET created_at = now() - interval '10 hours' WHERE id = $1",
    )
    .bind(starved.id)
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_opportunities SET created_at = now() - interval '3 minutes' WHERE id = $1",
    )
    .bind(young_low.id)
    .execute(fixture.client.pool())
    .await?;
    let batch_claims = fixture
        .client
        .lease_rebalance_opportunity_batch(
            &batch_cluster,
            "batch-worker",
            RebalanceOpportunityClaimKind::Execute,
            4,
            Utc::now() + chrono::Duration::minutes(5),
        )
        .await?;
    let batch_claim_ids = batch_claims
        .iter()
        .map(|claim| claim.opportunity.id)
        .collect::<Vec<_>>();
    let batch_gradual_age_boost_and_priority_ordered =
        batch_claim_ids == vec![starved.id, batch_high.id, batch_medium.id, young_low.id];

    let (runnable_index_definition, expired_index_definition): (String, String) = sqlx::query_as(
        r#"
            SELECT
                pg_get_indexdef(
                    'loyal_yield.rebalance_opportunities_ready_priority_idx'::regclass
                ),
                pg_get_indexdef(
                    'loyal_yield.rebalance_opportunities_expired_lease_idx'::regclass
                )
            "#,
    )
    .fetch_one(fixture.client.pool())
    .await?;
    let claim_partial_index_predicates_are_lane_exact = runnable_index_definition
        .contains("opportunity_state = ANY (ARRAY['ready'::text, 'revalidate'::text])")
        && !runnable_index_definition.contains("'waiting_alt'::text")
        && !runnable_index_definition.contains("'leased'::text")
        && expired_index_definition.contains("(cluster, lease_kind, lease_expires_at, id)")
        && expired_index_definition.contains("WHERE (opportunity_state = 'leased'::text)")
        && !expired_index_definition.contains("'waiting_alt'::text")
        && !expired_index_definition.contains("'ready'::text")
        && !expired_index_definition.contains("'revalidate'::text");

    let baseline_latency_cluster = fixture.cluster("latency_baseline");
    let cold_latency_cluster = fixture.cluster("latency_cold");
    let baseline_latency_epoch = fixture.seed_epoch(&baseline_latency_cluster).await?;
    let cold_latency_epoch = fixture.seed_epoch(&cold_latency_cluster).await?;
    const READY_JOBS_SEEDED: usize = 4_160;
    const CLAIM_WARMUP_BATCH_SIZE: usize = 64;
    fixture
        .seed_claim_latency_cluster(
            &baseline_latency_cluster,
            baseline_latency_epoch,
            i64::try_from(READY_JOBS_SEEDED)?,
            0,
            10_000,
        )
        .await?;
    fixture
        .seed_claim_latency_cluster(
            &cold_latency_cluster,
            cold_latency_epoch,
            i64::try_from(READY_JOBS_SEEDED)?,
            10_000,
            0,
        )
        .await?;
    claim_latency_batch_micros(
        &fixture.latency_client,
        &baseline_latency_cluster,
        "latency-baseline-warmup",
        CLAIM_WARMUP_BATCH_SIZE,
    )
    .await?;
    claim_latency_batch_micros(
        &fixture.latency_client,
        &cold_latency_cluster,
        "latency-cold-warmup",
        CLAIM_WARMUP_BATCH_SIZE,
    )
    .await?;
    let (runnable_index_reads_before, expired_index_reads_before) =
        claim_index_tuple_reads(&fixture.latency_client).await?;
    let mut baseline_latency_samples = Vec::new();
    let mut cold_latency_samples = Vec::new();
    let mut baseline_client_latency_samples = Vec::new();
    let mut cold_client_latency_samples = Vec::new();
    // Sixty-three interleaved production-size batches make server p95 the
    // fourth-slowest observation. The gate uses PostgreSQL statement time from
    // the exact production SQL; client/Tokio wall time remains diagnostic.
    for round in 0..63 {
        if round % 2 == 0 {
            let (client_micros, server_micros) = claim_latency_batch_micros(
                &fixture.latency_client,
                &baseline_latency_cluster,
                &format!("latency-baseline-{round}"),
                64,
            )
            .await?;
            baseline_client_latency_samples.push(client_micros);
            baseline_latency_samples.push(server_micros);
            let (client_micros, server_micros) = claim_latency_batch_micros(
                &fixture.latency_client,
                &cold_latency_cluster,
                &format!("latency-cold-{round}"),
                64,
            )
            .await?;
            cold_client_latency_samples.push(client_micros);
            cold_latency_samples.push(server_micros);
        } else {
            let (client_micros, server_micros) = claim_latency_batch_micros(
                &fixture.latency_client,
                &cold_latency_cluster,
                &format!("latency-cold-{round}"),
                64,
            )
            .await?;
            cold_client_latency_samples.push(client_micros);
            cold_latency_samples.push(server_micros);
            let (client_micros, server_micros) = claim_latency_batch_micros(
                &fixture.latency_client,
                &baseline_latency_cluster,
                &format!("latency-baseline-{round}"),
                64,
            )
            .await?;
            baseline_client_latency_samples.push(client_micros);
            baseline_latency_samples.push(server_micros);
        }
    }
    let (runnable_index_reads_after, expired_index_reads_after) =
        claim_index_tuple_reads(&fixture.latency_client).await?;
    let runnable_index_tuple_reads =
        runnable_index_reads_after.saturating_sub(runnable_index_reads_before);
    let expired_index_tuple_reads =
        expired_index_reads_after.saturating_sub(expired_index_reads_before);
    const TIMED_CLAIM_SERIES: i64 = 2;
    const TIMED_CLAIM_ROUNDS: i64 = 63;
    const CLAIM_BATCH_SIZE: i64 = 64;
    const WARMUP_CLAIMS_PER_SERIES: i64 = 64;
    let timed_ready_rows_claimed = TIMED_CLAIM_SERIES * TIMED_CLAIM_ROUNDS * CLAIM_BATCH_SIZE;
    // Each ready -> leased update leaves one dead entry in the runnable B-tree
    // until vacuum. The exact no-vacuum traversal is triangular: every later
    // batch crosses its own series' warmup and prior claimed entries before its
    // live batch. Allow 20% for MVCC/page visibility overhead. A regression
    // that scans the 10,000 waiting_alt rows in every cold round adds 630,000
    // reads and remains well beyond this derived ceiling.
    let expected_ranked_lane_reads = TIMED_CLAIM_SERIES
        * (TIMED_CLAIM_ROUNDS * WARMUP_CLAIMS_PER_SERIES
            + CLAIM_BATCH_SIZE * TIMED_CLAIM_ROUNDS * (TIMED_CLAIM_ROUNDS + 1) / 2);
    // PostgreSQL 17 reads this partial index once for the ranked runnable
    // stream and again while locking/rechecking the candidate path, with two
    // additional index reads per claimed row. Plans that use primary-key
    // probes for the second path only reduce this one-sided ceiling. The
    // waiting_alt full-scan regression remains another 630,000 reads beyond
    // this derived production-statement baseline.
    let expected_runnable_self_churn_reads = expected_ranked_lane_reads
        .saturating_mul(2)
        .saturating_add(timed_ready_rows_claimed.saturating_mul(2));
    let runnable_self_churn_ceiling_reads = expected_runnable_self_churn_reads * 6 / 5;
    let waiting_alt_full_scan_regression_reads = 10_000_i64 * TIMED_CLAIM_ROUNDS;
    let claim_index_reads_are_bounded = runnable_index_tuple_reads
        <= runnable_self_churn_ceiling_reads
        && expired_index_tuple_reads <= TIMED_CLAIM_SERIES * TIMED_CLAIM_ROUNDS;
    let baseline_claim_p95_micros = p95_micros(&mut baseline_latency_samples);
    let cold_claim_p95_micros = p95_micros(&mut cold_latency_samples);
    let baseline_client_claim_p95_micros = p95_micros(&mut baseline_client_latency_samples);
    let cold_client_claim_p95_micros = p95_micros(&mut cold_client_latency_samples);
    let cold_backlog_effect_ppm = cold_claim_p95_micros
        .saturating_sub(baseline_claim_p95_micros)
        .saturating_mul(1_000_000)
        / baseline_claim_p95_micros.max(1);
    const TIMED_READY_JOBS_CLAIMED: usize = CLAIM_WARMUP_BATCH_SIZE + (63 * 64);
    let remaining_ready_jobs = READY_JOBS_SEEDED - TIMED_READY_JOBS_CLAIMED;
    claim_latency_batch_micros(
        &fixture.latency_client,
        &baseline_latency_cluster,
        "latency-baseline-drain",
        remaining_ready_jobs,
    )
    .await?;
    claim_latency_batch_micros(
        &fixture.latency_client,
        &cold_latency_cluster,
        "latency-cold-drain",
        remaining_ready_jobs,
    )
    .await?;
    let ready_jobs_claimed: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.rebalance_opportunities
        WHERE cluster = $1 AND opportunity_state = 'leased'
          AND lease_kind = 'execute'
        "#,
    )
    .bind(&cold_latency_cluster)
    .fetch_one(fixture.client.pool())
    .await?;
    let waiting_alt_jobs: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.rebalance_opportunities
        WHERE cluster = $1 AND opportunity_state = 'waiting_alt'
        "#,
    )
    .bind(&cold_latency_cluster)
    .fetch_one(fixture.client.pool())
    .await?;
    let waiting_alt_decisions: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.rebalance_opportunities opportunity
        JOIN loyal_yield.rebalance_decisions decision
          ON decision.id = opportunity.decision_id
        WHERE opportunity.cluster = $1
          AND opportunity.opportunity_state = 'waiting_alt'
        "#,
    )
    .bind(&cold_latency_cluster)
    .fetch_one(fixture.client.pool())
    .await?;

    let wakeup_cluster = fixture.cluster("alt_wakeup");
    let wakeup_epoch = fixture.seed_epoch(&wakeup_cluster).await?;
    let affected_waiting = fixture
        .seed_opportunity(
            &wakeup_cluster,
            wakeup_epoch,
            "alt-wakeup-affected",
            "waiting_alt",
            4_000,
        )
        .await?;
    let unaffected_waiting = fixture
        .seed_opportunity(
            &wakeup_cluster,
            wakeup_epoch,
            "alt-wakeup-unaffected",
            "waiting_alt",
            3_000,
        )
        .await?;
    let affected_vault_id: i64 = sqlx::query_scalar(
        "SELECT vault_id FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(affected_waiting.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let provisioning_request = fixture
        .client
        .upsert_lookup_table_provisioning_request(LookupTableProvisioningRequestUpsert {
            cluster: wakeup_cluster.clone(),
            vault_id: VaultId(affected_vault_id),
            route_fingerprint: format!("route:{}:alt-wakeup-affected", fixture.prefix),
            requirements_fingerprint: format!(
                "requirements:{}:alt-wakeup-affected",
                fixture.prefix
            ),
            shared_manifest_id: None,
            vault_manifest_id: None,
            desired_shared_hash: Some(format!("shared-hash:{}", fixture.prefix)),
            desired_vault_hash: Some(format!("vault-hash:{}", fixture.prefix)),
            shared_addresses: Vec::new(),
            vault_addresses: Vec::new(),
        })
        .await?;
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.lookup_table_provisioning_request_consumers
            (opportunity_id, provisioning_request_id)
        VALUES ($1, $2)
        "#,
    )
    .bind(affected_waiting.id)
    .bind(provisioning_request.id)
    .execute(fixture.client.pool())
    .await?;
    let waiting_decision_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.rebalance_decisions decision
        JOIN loyal_yield.rebalance_opportunities opportunity
          ON opportunity.decision_id = decision.id
        WHERE opportunity.cluster = $1
          AND opportunity.opportunity_state = 'waiting_alt'
        "#,
    )
    .bind(&wakeup_cluster)
    .fetch_one(fixture.client.pool())
    .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_provisioning_requests
        SET request_status = 'satisfied', satisfied_at = now(), updated_at = now()
        WHERE id = $1
        "#,
    )
    .bind(provisioning_request.id)
    .execute(fixture.client.pool())
    .await?;
    let affected_before_readmission_state: String = sqlx::query_scalar(
        "SELECT opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(affected_waiting.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let affected_after_readmission = fixture
        .client
        .re_admit_waiting_alt_opportunity(affected_waiting.id, wakeup_epoch)
        .await?;
    let unaffected_wakeup_state: String = sqlx::query_scalar(
        "SELECT opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(unaffected_waiting.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let affected_outbox_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.orchestration_outbox
        WHERE cluster = $1 AND event_kind = 'alt_satisfied'
          AND aggregate_kind = 'rebalance_opportunity'
          AND aggregate_id = $2
        "#,
    )
    .bind(&wakeup_cluster)
    .bind(affected_waiting.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let acknowledged_alt_wakeups = fixture
        .client
        .acknowledge_promoted_alt_outbox_batch(&wakeup_cluster, 32)
        .await?;
    let pending_affected_outbox_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.orchestration_outbox
        WHERE cluster = $1 AND event_kind = 'alt_satisfied'
          AND aggregate_kind = 'rebalance_opportunity'
          AND aggregate_id = $2 AND processed_at IS NULL
        "#,
    )
    .bind(&wakeup_cluster)
    .bind(affected_waiting.id)
    .fetch_one(fixture.client.pool())
    .await?;

    let alt_runtime_measurements = run_alt_database_runtime_measurements(fixture).await?;
    let fleet_latency_load = run_fleet_latency_load(fixture).await?;
    let fleet_latency_speedup_passed = fleet_latency_load["legacy"]["conflictRetries"]
        .as_u64()
        .is_some_and(|retries| retries > 0)
        && fleet_latency_load["reservationNonFused"]["conflictRetries"].as_u64() == Some(0)
        && fleet_latency_load["reservationFused"]["conflictRetries"].as_u64() == Some(0)
        && fleet_latency_load["reservationFused"]["reservationAdmissionP95Micros"]
            .as_u64()
            .is_some_and(|micros| micros < 50_000)
        && fleet_latency_load["attribution"]["policySerializationP50Millis"]
            .as_u64()
            .is_some_and(|millis| millis > 0)
        && fleet_latency_load["attribution"]["duplicateFinalBuildP50Millis"]
            .as_u64()
            .is_some_and(|millis| millis > 0)
        && fleet_latency_load["attribution"]["readyToSubmittedP50SpeedupPercent"]
            .as_u64()
            .is_some_and(|percent| percent >= 50);

    let reclaim_cluster = fixture.cluster("reclaim");
    let reclaim_epoch = fixture.seed_epoch(&reclaim_cluster).await?;
    let reclaim_seed = fixture
        .seed_opportunity(&reclaim_cluster, reclaim_epoch, "reclaim", "ready", 2_000)
        .await?;
    let reclaim_ready_seed = fixture
        .seed_opportunity(
            &reclaim_cluster,
            reclaim_epoch,
            "reclaim-ready-competitor",
            "ready",
            1_000,
        )
        .await?;
    let first_lease = claim_one(
        &fixture.client,
        &reclaim_cluster,
        "crashed-worker",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_opportunities SET lease_expires_at = now() - interval '1 second' WHERE id = $1",
    )
    .bind(reclaim_seed.id)
    .execute(fixture.client.pool())
    .await?;
    let reclaimed_batch = fixture
        .client
        .lease_rebalance_opportunity_batch(
            &reclaim_cluster,
            "replacement-worker",
            RebalanceOpportunityClaimKind::Execute,
            2,
            Utc::now() + chrono::Duration::minutes(5),
        )
        .await?;
    let reclaimed = reclaimed_batch
        .first()
        .ok_or("expired recovery fixture returned no reclaimed route")?
        .clone();
    let reclaimed_ready = reclaimed_batch
        .get(1)
        .ok_or("expired recovery fixture did not fill from the runnable lane")?
        .clone();
    let mixed_lane_global_order_preserved = reclaimed.opportunity.id == reclaim_seed.id
        && reclaimed_ready.opportunity.id == reclaim_ready_seed.id
        && reclaimed.fencing_token > first_lease.fencing_token;

    let mixed_concurrent_cluster = fixture.cluster("mixed_concurrent_claim");
    let mixed_concurrent_epoch = fixture.seed_epoch(&mixed_concurrent_cluster).await?;
    let mut mixed_expired_ids = BTreeSet::new();
    for ordinal in 0..2_i64 {
        let seeded = fixture
            .seed_opportunity(
                &mixed_concurrent_cluster,
                mixed_concurrent_epoch,
                &format!("mixed-expired-{ordinal}"),
                "ready",
                3_000 - ordinal,
            )
            .await?;
        mixed_expired_ids.insert(seeded.id);
    }
    let initially_leased_mixed = fixture
        .client
        .lease_rebalance_opportunity_batch(
            &mixed_concurrent_cluster,
            "mixed-prime-worker",
            RebalanceOpportunityClaimKind::Execute,
            2,
            Utc::now() + chrono::Duration::minutes(5),
        )
        .await?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_opportunities SET lease_expires_at = now() - interval '1 second' WHERE cluster = $1",
    )
    .bind(&mixed_concurrent_cluster)
    .execute(fixture.client.pool())
    .await?;
    let mut mixed_runnable_ids = BTreeSet::new();
    for ordinal in 0..2_i64 {
        let seeded = fixture
            .seed_opportunity(
                &mixed_concurrent_cluster,
                mixed_concurrent_epoch,
                &format!("mixed-runnable-{ordinal}"),
                "ready",
                1_000 - ordinal,
            )
            .await?;
        mixed_runnable_ids.insert(seeded.id);
    }
    let mixed_claim_a = fixture.client.lease_rebalance_opportunity_batch(
        &mixed_concurrent_cluster,
        "mixed-concurrent-a",
        RebalanceOpportunityClaimKind::Execute,
        2,
        Utc::now() + chrono::Duration::minutes(5),
    );
    let mixed_claim_b = fixture.client.lease_rebalance_opportunity_batch(
        &mixed_concurrent_cluster,
        "mixed-concurrent-b",
        RebalanceOpportunityClaimKind::Execute,
        2,
        Utc::now() + chrono::Duration::minutes(5),
    );
    let (mixed_claim_a, mixed_claim_b) = tokio::join!(mixed_claim_a, mixed_claim_b);
    let mixed_claim_a = mixed_claim_a?;
    let mixed_claim_b = mixed_claim_b?;
    let mixed_claim_a_ids = mixed_claim_a
        .iter()
        .map(|lease| lease.opportunity.id)
        .collect::<BTreeSet<_>>();
    let mixed_claim_b_ids = mixed_claim_b
        .iter()
        .map(|lease| lease.opportunity.id)
        .collect::<BTreeSet<_>>();
    let mixed_concurrent_ids = mixed_claim_a_ids
        .union(&mixed_claim_b_ids)
        .copied()
        .collect::<BTreeSet<_>>();
    let mixed_expected_ids = mixed_expired_ids
        .union(&mixed_runnable_ids)
        .copied()
        .collect::<BTreeSet<_>>();
    let mixed_concurrent_claims_are_full_disjoint_and_priority_ordered =
        initially_leased_mixed.len() == 2
            && mixed_claim_a.len() == 2
            && mixed_claim_b.len() == 2
            && mixed_claim_a_ids.is_disjoint(&mixed_claim_b_ids)
            && mixed_concurrent_ids == mixed_expected_ids;

    let retry_cluster = fixture.cluster("immediate_retry");
    let retry_epoch = fixture.seed_epoch(&retry_cluster).await?;
    let retry_seed = fixture
        .seed_opportunity(&retry_cluster, retry_epoch, "immediate-retry", "ready", 950)
        .await?;
    let retry_first = claim_one(
        &fixture.client,
        &retry_cluster,
        "retry-worker-failed-before-decision",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let retry_conflicts = vec![
        format!("fleet-shared-write-lane:{}:immediate-retry", fixture.prefix),
        format!(
            "vault-write:{}:{}",
            fixture.prefix,
            retry_first.opportunity.vault_id.as_i64()
        ),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &retry_first,
            &retry_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    fixture
        .client
        .advance_rebalance_opportunity(
            retry_first.opportunity.id,
            &retry_first,
            RebalanceOpportunityAdvance {
                next_state: RebalanceOpportunityState::Ready,
                available_at: Some(Utc::now()),
                decision_id: None,
                reason: Some("synthetic pre-decision retry".to_owned()),
                route_fingerprint: retry_first.opportunity.route_fingerprint.clone(),
                requirements_fingerprint: retry_first.opportunity.requirements_fingerprint.clone(),
                execution_plan: Some(retry_first.opportunity.execution_plan.clone()),
                provisioning_request_id: None,
            },
        )
        .await?
        .into_applied(retry_first.opportunity.id)?;
    let retry_conflicts_after_release: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id = $1",
    )
    .bind(retry_seed.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let retry_reclaimed = claim_one(
        &fixture.client,
        &retry_cluster,
        "retry-worker-replacement",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;

    let expired_retry_cluster = fixture.cluster("expired_retry_advance");
    let expired_retry_epoch = fixture.seed_epoch(&expired_retry_cluster).await?;
    let expired_retry_seed = fixture
        .seed_opportunity(
            &expired_retry_cluster,
            expired_retry_epoch,
            "expired-retry-advance",
            "ready",
            940,
        )
        .await?;
    let mut expired_retry_lease = claim_one(
        &fixture.client,
        &expired_retry_cluster,
        "expired-retry-worker",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let expired_retry_deadline: DateTime<Utc> = sqlx::query_scalar(
        r#"
        UPDATE loyal_yield.rebalance_opportunities
        SET expires_at = clock_timestamp() - interval '1 second',
            lease_expires_at = clock_timestamp() - interval '1 second'
        WHERE id = $1
        RETURNING expires_at
        "#,
    )
    .bind(expired_retry_seed.id)
    .fetch_one(fixture.client.pool())
    .await?;
    expired_retry_lease.opportunity.expires_at = expired_retry_deadline;
    expired_retry_lease.opportunity.lease_expires_at = Some(expired_retry_deadline);
    expired_retry_lease.expires_at = expired_retry_deadline;
    let expired_retry_swept = fixture
        .client
        .sweep_expired_rebalance_opportunities(&expired_retry_cluster, 1)
        .await?;
    let expired_retry_outcome = fixture
        .client
        .advance_rebalance_opportunity(
            expired_retry_seed.id,
            &expired_retry_lease,
            RebalanceOpportunityAdvance {
                next_state: RebalanceOpportunityState::Ready,
                available_at: Some(Utc::now() + chrono::Duration::seconds(2)),
                decision_id: None,
                reason: Some("synthetic effect-free retry".to_owned()),
                route_fingerprint: expired_retry_lease.opportunity.route_fingerprint.clone(),
                requirements_fingerprint: expired_retry_lease
                    .opportunity
                    .requirements_fingerprint
                    .clone(),
                execution_plan: Some(expired_retry_lease.opportunity.execution_plan.clone()),
                provisioning_request_id: None,
            },
        )
        .await?;
    let expired_retry_current = fixture
        .client
        .rebalance_opportunity(expired_retry_seed.id)
        .await?
        .ok_or("expired-retry opportunity disappeared")?;
    let expired_retry_advance_classified = matches!(
        expired_retry_outcome,
        RebalanceOpportunityAdvanceOutcome::Expired
    ) && expired_retry_swept == 1
        && expired_retry_current.state == RebalanceOpportunityState::Stale
        && expired_retry_current.terminal_reason.as_deref() == Some("optimizer_epoch_expired")
        && expired_retry_current.decision_id.is_none();

    let fenced_advance_cluster = fixture.cluster("fenced_advance");
    let fenced_advance_epoch = fixture.seed_epoch(&fenced_advance_cluster).await?;
    let fenced_advance_seed = fixture
        .seed_opportunity(
            &fenced_advance_cluster,
            fenced_advance_epoch,
            "fenced-advance",
            "ready",
            930,
        )
        .await?;
    let fenced_advance_lease = claim_one(
        &fixture.client,
        &fenced_advance_cluster,
        "fenced-advance-owner",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let mut stale_fenced_advance_lease = fenced_advance_lease.clone();
    stale_fenced_advance_lease.owner = "different-owner".to_owned();
    stale_fenced_advance_lease.fencing_token = stale_fenced_advance_lease
        .fencing_token
        .checked_add(1)
        .ok_or("fenced-advance token overflow")?;
    let fenced_advance_outcome = fixture
        .client
        .advance_rebalance_opportunity(
            fenced_advance_seed.id,
            &stale_fenced_advance_lease,
            RebalanceOpportunityAdvance {
                next_state: RebalanceOpportunityState::Ready,
                available_at: Some(Utc::now()),
                decision_id: None,
                reason: Some("synthetic stolen lease".to_owned()),
                route_fingerprint: fenced_advance_lease.opportunity.route_fingerprint.clone(),
                requirements_fingerprint: fenced_advance_lease
                    .opportunity
                    .requirements_fingerprint
                    .clone(),
                execution_plan: Some(fenced_advance_lease.opportunity.execution_plan.clone()),
                provisioning_request_id: None,
            },
        )
        .await?;
    let fenced_advance_current = fixture
        .client
        .rebalance_opportunity(fenced_advance_seed.id)
        .await?
        .ok_or("fenced-advance opportunity disappeared")?;
    let fenced_advance_classified = matches!(
        fenced_advance_outcome,
        RebalanceOpportunityAdvanceOutcome::Fenced
    ) && fenced_advance_current.state
        == RebalanceOpportunityState::Leased
        && fenced_advance_current.lease_owner == fenced_advance_lease.opportunity.lease_owner
        && fenced_advance_current.fencing_token == fenced_advance_lease.fencing_token;

    // A failed attempt is immutable audit evidence. Republish the exact same
    // economics only when the prior attempt is terminal and has a database-
    // proven no-effect outcome. Two planner waves racing that rediscovery must
    // converge on one next generation rather than reopen or overwrite history.
    let retry_generation_cluster = fixture.cluster("retry_generation");
    let retry_generation_epoch = fixture.seed_epoch(&retry_generation_cluster).await?;
    let retry_generation_seed = fixture
        .seed_opportunity(
            &retry_generation_cluster,
            retry_generation_epoch,
            "retry-generation",
            "revalidate",
            925,
        )
        .await?;
    let retry_generation_seed_record = fixture
        .client
        .rebalance_opportunity(retry_generation_seed.id)
        .await?
        .ok_or("retry-generation seed opportunity disappeared")?;
    let mut retry_generation_input =
        rediscovery_input_for_opportunity(&retry_generation_seed_record);
    let retry_generation_plan = retry_generation_input
        .execution_plan
        .as_object_mut()
        .ok_or("retry-generation execution plan is not an object")?;
    retry_generation_plan.insert("source_kind".to_owned(), json!("reserve_position"));
    retry_generation_plan.insert("source_observed_slot".to_owned(), json!(433_191_369));
    retry_generation_plan.insert(
        "source_observed_at".to_owned(),
        json!("2026-07-16T03:11:11Z"),
    );
    retry_generation_plan.insert("idle_token_account".to_owned(), Value::Null);
    sqlx::query("DELETE FROM loyal_yield.rebalance_opportunities WHERE id = $1")
        .bind(retry_generation_seed.id)
        .execute(fixture.client.pool())
        .await?;
    let retry_generation_first = fixture
        .client
        .upsert_rebalance_opportunity(retry_generation_input.clone())
        .await?;
    let retry_generation_lease = claim_one(
        &fixture.client,
        &retry_generation_cluster,
        "retry-generation-first-attempt",
        RebalanceOpportunityClaimKind::Revalidate,
    )
    .await?;
    let retry_generation_failed = fixture
        .client
        .advance_rebalance_opportunity(
            retry_generation_first.id,
            &retry_generation_lease,
            RebalanceOpportunityAdvance {
                next_state: RebalanceOpportunityState::Failed,
                available_at: None,
                decision_id: None,
                reason: Some(
                    "same-mint reserve-position request cannot carry idle-vault evidence"
                        .to_owned(),
                ),
                route_fingerprint: None,
                requirements_fingerprint: None,
                execution_plan: None,
                provisioning_request_id: None,
            },
        )
        .await?
        .into_applied(retry_generation_first.id)?;
    let retry_generation_client_a = fixture.client.clone();
    let retry_generation_client_b = fixture.client.clone();
    let retry_generation_input_a = retry_generation_input.clone();
    let retry_generation_input_b = retry_generation_input.clone();
    let (retry_generation_result_a, retry_generation_result_b) = tokio::join!(
        retry_generation_client_a.upsert_rebalance_opportunity(retry_generation_input_a),
        retry_generation_client_b.upsert_rebalance_opportunity(retry_generation_input_b),
    );
    let retry_generation_second_a = retry_generation_result_a?;
    let retry_generation_second_b = retry_generation_result_b?;
    let retry_generation_nonterminal_duplicate = fixture
        .client
        .upsert_rebalance_opportunity(retry_generation_input.clone())
        .await?;
    let retry_generation_rows = sqlx::query(
        r#"
        SELECT id, idempotency_key, rediscovery_key, attempt_generation,
               opportunity_state, execution_plan, terminal_reason, updated_at
        FROM loyal_yield.rebalance_opportunities
        WHERE rediscovery_key = $1
        ORDER BY attempt_generation, id
        "#,
    )
    .bind(&retry_generation_first.rediscovery_key)
    .fetch_all(fixture.client.pool())
    .await?;
    let retry_generation_ids = retry_generation_rows
        .iter()
        .map(|row| row.try_get::<i64, _>("id"))
        .collect::<Result<Vec<_>, _>>()?;
    let retry_generation_numbers = retry_generation_rows
        .iter()
        .map(|row| row.try_get::<i64, _>("attempt_generation"))
        .collect::<Result<Vec<_>, _>>()?;
    let retry_generation_states = retry_generation_rows
        .iter()
        .map(|row| row.try_get::<String, _>("opportunity_state"))
        .collect::<Result<Vec<_>, _>>()?;
    let retry_generation_idempotency_keys = retry_generation_rows
        .iter()
        .map(|row| row.try_get::<String, _>("idempotency_key"))
        .collect::<Result<Vec<_>, _>>()?;
    let retry_generation_first_updated_at = retry_generation_rows
        .first()
        .ok_or("retry-generation rows disappeared")?
        .try_get::<DateTime<Utc>, _>("updated_at")?;
    let retry_generation_first_execution_plan = retry_generation_rows
        .first()
        .ok_or("retry-generation rows disappeared")?
        .try_get::<Value, _>("execution_plan")?;
    let retry_generation_first_terminal_reason = retry_generation_rows
        .first()
        .ok_or("retry-generation rows disappeared")?
        .try_get::<Option<String>, _>("terminal_reason")?;
    let retry_generation_dirty_hint_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.fleet_planning_dirty_vaults
        WHERE cluster = $1 AND vault_id = $2
          AND reasons @> ARRAY['terminal_no_effect_retry_check']::TEXT[]
        "#,
    )
    .bind(&retry_generation_cluster)
    .bind(retry_generation_first.vault_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let retry_generation_concurrency_safe = retry_generation_first.attempt_generation == 1
        && retry_generation_failed.state == RebalanceOpportunityState::Failed
        && retry_generation_second_a.id == retry_generation_second_b.id
        && retry_generation_second_a.attempt_generation == 2
        && retry_generation_second_a.rediscovery_key == retry_generation_first.rediscovery_key
        && retry_generation_nonterminal_duplicate.id == retry_generation_second_a.id
        && retry_generation_ids == vec![retry_generation_first.id, retry_generation_second_a.id]
        && retry_generation_numbers == vec![1, 2]
        && retry_generation_states == vec!["failed", "revalidate"]
        && retry_generation_idempotency_keys.len() == 2
        && retry_generation_idempotency_keys[0] != retry_generation_idempotency_keys[1]
        && retry_generation_first_updated_at == retry_generation_failed.updated_at
        && retry_generation_dirty_hint_count == 1;
    let source_contract_failed_attempt_and_successor_evidence = retry_generation_concurrency_safe
        && retry_generation_first_execution_plan == retry_generation_first.execution_plan
        && retry_generation_first_terminal_reason.as_deref()
            == Some("same-mint reserve-position request cannot carry idle-vault evidence")
        && retry_generation_idempotency_keys
            .first()
            .map(String::as_str)
            == Some(retry_generation_first.idempotency_key.as_str())
        && retry_generation_failed.id == retry_generation_first.id
        && retry_generation_failed.idempotency_key == retry_generation_first.idempotency_key
        && retry_generation_failed.rediscovery_key == retry_generation_first.rediscovery_key
        && retry_generation_failed.attempt_generation == retry_generation_first.attempt_generation
        && retry_generation_failed.execution_plan == retry_generation_first.execution_plan;

    let fused_cluster = fixture.cluster("fused_execute");
    let fused_epoch = fixture.seed_epoch(&fused_cluster).await?;
    fixture
        .seed_opportunity(
            &fused_cluster,
            fused_epoch,
            "fused-success",
            "revalidate",
            940,
        )
        .await?;
    let fused_revalidation = claim_one(
        &fixture.client,
        &fused_cluster,
        "fused-revalidator-success",
        RebalanceOpportunityClaimKind::Revalidate,
    )
    .await?;
    let shared_fused_conflict = format!("fleet-shared-write-lane:{}:fused", fixture.prefix);
    let fused_conflicts = vec![
        shared_fused_conflict.clone(),
        format!(
            "vault-write:{}:{}",
            fixture.prefix,
            fused_revalidation.opportunity.vault_id.as_i64()
        ),
    ];
    let mut fused_execution_plan = fused_revalidation.opportunity.execution_plan.clone();
    let fused_fields = fused_execution_plan
        .as_object_mut()
        .ok_or("fused success fixture execution plan is not an object")?;
    fused_fields.insert(
        "exact_writable_account_keys".to_owned(),
        json!([format!("exact-write:{}:fused", fixture.prefix)]),
    );
    fused_fields.insert("conflict_account_keys".to_owned(), json!(&fused_conflicts));
    fused_fields.insert(
        "alt_readiness".to_owned(),
        json!({"fixture": "fused-success"}),
    );
    let fused_promoted = fixture
        .client
        .try_promote_revalidation_lease_to_execute(
            &fused_revalidation,
            &format!("route:{}:fused-success-exact", fixture.prefix),
            &format!("requirements:{}:fused-success-exact", fixture.prefix),
            &fused_execution_plan,
            &fused_conflicts,
        )
        .await?
        .ok_or("uncontended fused revalidation lease did not promote")?;
    let fused_conflict_rows = sqlx::query(
        r#"
        SELECT writable_account_key, lease_owner, fencing_token, submission_id
        FROM loyal_yield.route_account_conflict_leases
        WHERE opportunity_id = $1
        ORDER BY writable_account_key
        "#,
    )
    .bind(fused_promoted.opportunity.id)
    .fetch_all(fixture.client.pool())
    .await?;
    let fused_conflict_keys = fused_conflict_rows
        .iter()
        .map(|row| row.try_get::<String, _>("writable_account_key"))
        .collect::<Result<Vec<_>, _>>()?;
    let fused_exact_conflict_ownership = fused_conflict_keys == fused_conflicts
        && fused_conflict_rows.iter().all(|row| {
            row.try_get::<String, _>("lease_owner").ok().as_deref()
                == Some(fused_promoted.owner.as_str())
                && row.try_get::<i64, _>("fencing_token").ok() == Some(fused_promoted.fencing_token)
                && row.try_get::<Option<i64>, _>("submission_id").ok() == Some(None)
        });

    fixture
        .seed_opportunity(
            &fused_cluster,
            fused_epoch,
            "fused-conflicted",
            "revalidate",
            930,
        )
        .await?;
    let conflicted_revalidation = claim_one(
        &fixture.client,
        &fused_cluster,
        "fused-revalidator-conflicted",
        RebalanceOpportunityClaimKind::Revalidate,
    )
    .await?;
    let conflicted_keys = vec![
        shared_fused_conflict,
        format!(
            "vault-write:{}:{}",
            fixture.prefix,
            conflicted_revalidation.opportunity.vault_id.as_i64()
        ),
    ];
    let mut conflicted_plan = conflicted_revalidation.opportunity.execution_plan.clone();
    let conflicted_fields = conflicted_plan
        .as_object_mut()
        .ok_or("fused conflict fixture execution plan is not an object")?;
    conflicted_fields.insert(
        "exact_writable_account_keys".to_owned(),
        json!([format!("exact-write:{}:fused-conflicted", fixture.prefix)]),
    );
    conflicted_fields.insert("conflict_account_keys".to_owned(), json!(&conflicted_keys));
    conflicted_fields.insert(
        "alt_readiness".to_owned(),
        json!({"fixture": "fused-conflicted"}),
    );
    let conflicted_promotion = fixture
        .client
        .try_promote_revalidation_lease_to_execute(
            &conflicted_revalidation,
            &format!("route:{}:fused-conflicted-exact", fixture.prefix),
            &format!("requirements:{}:fused-conflicted-exact", fixture.prefix),
            &conflicted_plan,
            &conflicted_keys,
        )
        .await?;
    let conflicted_after = fixture
        .client
        .rebalance_opportunity(conflicted_revalidation.opportunity.id)
        .await?
        .ok_or("fused conflict fixture opportunity disappeared")?;
    let conflicted_rows: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id = $1",
    )
    .bind(conflicted_revalidation.opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let fused_fallback_preserved_revalidation = conflicted_promotion.is_none()
        && conflicted_after.state == RebalanceOpportunityState::Leased
        && conflicted_after.lease_kind == Some(RebalanceOpportunityClaimKind::Revalidate)
        && conflicted_after.lease_owner.as_deref() == Some(conflicted_revalidation.owner.as_str())
        && conflicted_after.fencing_token == conflicted_revalidation.fencing_token
        && conflicted_after.route_fingerprint
            == conflicted_revalidation.opportunity.route_fingerprint
        && conflicted_after.requirements_fingerprint
            == conflicted_revalidation.opportunity.requirements_fingerprint
        && conflicted_rows == 0;

    let commit_publication_cluster = fixture.cluster("commit_publication");
    let commit_publication_epoch = fixture.seed_epoch(&commit_publication_cluster).await?;
    let commit_publication_seed = fixture
        .seed_opportunity(
            &commit_publication_cluster,
            commit_publication_epoch,
            "commit-publication",
            "revalidate",
            926,
        )
        .await?;
    let commit_publication_record = fixture
        .client
        .rebalance_opportunity(commit_publication_seed.id)
        .await?
        .ok_or("commit-publication seed opportunity disappeared")?;
    let commit_publication_input = rediscovery_input_for_opportunity(&commit_publication_record);
    sqlx::query("DELETE FROM loyal_yield.rebalance_opportunities WHERE id = $1")
        .bind(commit_publication_seed.id)
        .execute(fixture.client.pool())
        .await?;
    sqlx::query(
        r#"
        CREATE OR REPLACE FUNCTION loyal_yield.fleet_verifier_force_short_publication_lifetime()
        RETURNS trigger
        LANGUAGE plpgsql
        AS $fixture$
        BEGIN
            IF NEW.cluster LIKE 'fleet_verify_%_commit_publication' THEN
                UPDATE loyal_yield.rebalance_opportunities
                SET expires_at = clock_timestamp() + interval '30 seconds'
                WHERE id = NEW.id;
            END IF;
            RETURN NULL;
        END;
        $fixture$
        "#,
    )
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        "DROP TRIGGER IF EXISTS aaa_fleet_verifier_force_short_publication_lifetime ON loyal_yield.rebalance_opportunities",
    )
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        r#"
        CREATE CONSTRAINT TRIGGER aaa_fleet_verifier_force_short_publication_lifetime
        AFTER INSERT ON loyal_yield.rebalance_opportunities
        DEFERRABLE INITIALLY DEFERRED
        FOR EACH ROW
        EXECUTE FUNCTION loyal_yield.fleet_verifier_force_short_publication_lifetime()
        "#,
    )
    .execute(fixture.client.pool())
    .await?;
    let commit_publication_result = fixture
        .client
        .upsert_rebalance_opportunity(commit_publication_input.clone())
        .await;
    sqlx::query(
        "DROP TRIGGER IF EXISTS aaa_fleet_verifier_force_short_publication_lifetime ON loyal_yield.rebalance_opportunities",
    )
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        "DROP FUNCTION IF EXISTS loyal_yield.fleet_verifier_force_short_publication_lifetime()",
    )
    .execute(fixture.client.pool())
    .await?;
    let commit_publication_error = commit_publication_result
        .as_ref()
        .err()
        .map(ToString::to_string);
    let commit_publication_rejected_during_commit = matches!(
        commit_publication_result.as_ref().err(),
        Some(OrchestratorError::OpportunityDeferredBehindEpochLifetime {
            stage: "database_commit_fence",
            ..
        })
    );
    let commit_publication_rows: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.rebalance_opportunities WHERE cluster = $1 AND vault_id = $2",
    )
    .bind(&commit_publication_cluster)
    .bind(commit_publication_record.vault_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;

    let commit_lifetime_cluster = fixture.cluster("commit_lifetime");
    let commit_lifetime_epoch = fixture.seed_epoch(&commit_lifetime_cluster).await?;
    fixture
        .seed_opportunity(
            &commit_lifetime_cluster,
            commit_lifetime_epoch,
            "commit-lifetime",
            "ready",
            925,
        )
        .await?;
    let commit_lifetime_fee_payer = format!("fee-shard:{}:commit-lifetime", fixture.prefix);
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.route_fee_payer_shards
            (cluster, fee_payer, enabled, minimum_balance_lamports,
             maximum_balance_lamports, rolling_window_seconds,
             maximum_window_spend_lamports, maximum_transaction_fee_lamports)
        VALUES ($1, $2, TRUE, 50000, 200000, 3600, 80000, 50000)
        "#,
    )
    .bind(&commit_lifetime_cluster)
    .bind(&commit_lifetime_fee_payer)
    .execute(fixture.client.pool())
    .await?;
    let commit_lifetime_lease = claim_one(
        &fixture.client,
        &commit_lifetime_cluster,
        "commit-lifetime-worker",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let commit_lifetime_conflicts = vec![
        format!("fleet-shared-write-lane:{}:commit-lifetime", fixture.prefix),
        format!(
            "vault-write:{}:{}:commit-lifetime",
            fixture.prefix,
            commit_lifetime_lease.opportunity.vault_id.as_i64()
        ),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &commit_lifetime_lease,
            &commit_lifetime_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    // This verifier-only deferred trigger runs first during COMMIT. The
    // application-level final check has already succeeded, so shortening the
    // opportunity here proves migration 29 rechecks the DB clock during COMMIT
    // and rolls back the fully-linked handoff atomically.
    sqlx::query(
        r#"
        CREATE OR REPLACE FUNCTION loyal_yield.fleet_verifier_force_short_lifetime()
        RETURNS trigger
        LANGUAGE plpgsql
        AS $fixture$
        BEGIN
            IF NEW.semantic_key LIKE 'semantic:fleet_verify_%:commit-lifetime' THEN
                UPDATE loyal_yield.rebalance_opportunities
                SET expires_at = clock_timestamp() + interval '30 seconds'
                WHERE id = NEW.opportunity_id;
            END IF;
            RETURN NULL;
        END;
        $fixture$
        "#,
    )
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        "DROP TRIGGER IF EXISTS aaa_fleet_verifier_force_short_lifetime ON loyal_yield.signed_route_submissions",
    )
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        r#"
        CREATE CONSTRAINT TRIGGER aaa_fleet_verifier_force_short_lifetime
        AFTER INSERT ON loyal_yield.signed_route_submissions
        DEFERRABLE INITIALLY DEFERRED
        FOR EACH ROW
        EXECUTE FUNCTION loyal_yield.fleet_verifier_force_short_lifetime()
        "#,
    )
    .execute(fixture.client.pool())
    .await?;
    let commit_lifetime_result = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            same_mint_input_for_lease(&commit_lifetime_lease)?,
            &commit_lifetime_lease,
            target_capacity_input_for_lease(fixture, &commit_lifetime_lease).await?,
            fee_shard_signed_input_for_lease(
                fixture,
                &commit_lifetime_lease,
                commit_lifetime_conflicts.clone(),
                "commit-lifetime",
                &commit_lifetime_fee_payer,
                100_000,
                5_000,
            )
            .await?,
        )
        .await;
    sqlx::query(
        "DROP TRIGGER IF EXISTS aaa_fleet_verifier_force_short_lifetime ON loyal_yield.signed_route_submissions",
    )
    .execute(fixture.client.pool())
    .await?;
    sqlx::query("DROP FUNCTION IF EXISTS loyal_yield.fleet_verifier_force_short_lifetime()")
        .execute(fixture.client.pool())
        .await?;
    let commit_lifetime_error = commit_lifetime_result
        .as_ref()
        .err()
        .map(ToString::to_string);
    let commit_lifetime_rejected_during_commit = commit_lifetime_error
        .as_deref()
        .is_some_and(|error| error.contains("signed route handoff cannot commit"));
    let commit_lifetime_decisions: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.rebalance_decisions WHERE vault_id = $1",
    )
    .bind(commit_lifetime_lease.opportunity.vault_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let (
        commit_lifetime_state,
        commit_lifetime_owner,
        commit_lifetime_fence,
        commit_lifetime_margin_preserved,
    ): (String, Option<String>, i64, bool) = sqlx::query_as(
        r#"
        SELECT opportunity_state, lease_owner, fencing_token,
               expires_at >= clock_timestamp() + interval '60 seconds'
        FROM loyal_yield.rebalance_opportunities
        WHERE id = $1
        "#,
    )
    .bind(commit_lifetime_lease.opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let commit_lifetime_capacity_reservations: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE opportunity_id = $1",
    )
    .bind(commit_lifetime_lease.opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let commit_lifetime_submissions: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE opportunity_id = $1",
    )
    .bind(commit_lifetime_lease.opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let commit_lifetime_fee_reservations: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.route_fee_payer_spend_reservations WHERE opportunity_id = $1",
    )
    .bind(commit_lifetime_lease.opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let (
        commit_lifetime_conflict_rows,
        commit_lifetime_unattached_conflict_rows,
        commit_lifetime_owned_conflict_rows,
    ): (i64, i64, i64) = sqlx::query_as(
        r#"
        SELECT count(*)::BIGINT,
               count(*) FILTER (WHERE submission_id IS NULL)::BIGINT,
               count(*) FILTER (
                   WHERE lease_owner = $2 AND fencing_token = $3
               )::BIGINT
        FROM loyal_yield.route_account_conflict_leases
        WHERE opportunity_id = $1
        "#,
    )
    .bind(commit_lifetime_lease.opportunity.id)
    .bind(&commit_lifetime_lease.owner)
    .bind(commit_lifetime_lease.fencing_token)
    .fetch_one(fixture.client.pool())
    .await?;

    // A deferred trigger event whose base row was genuinely deleted later in
    // the same transaction is cleanup, not publication, and must still commit.
    let deleted_cleanup_cluster = fixture.cluster("deleted_cleanup");
    let deleted_cleanup_epoch = fixture.seed_epoch(&deleted_cleanup_cluster).await?;
    let deleted_cleanup_seed = fixture
        .seed_opportunity(
            &deleted_cleanup_cluster,
            deleted_cleanup_epoch,
            "deleted-cleanup",
            "ready",
            925,
        )
        .await?;
    let mut deleted_cleanup_tx = fixture.client.pool().begin().await?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_opportunities SET opportunity_state = 'revalidate' WHERE id = $1",
    )
    .bind(deleted_cleanup_seed.id)
    .execute(&mut *deleted_cleanup_tx)
    .await?;
    sqlx::query("DELETE FROM loyal_yield.rebalance_opportunities WHERE id = $1")
        .bind(deleted_cleanup_seed.id)
        .execute(&mut *deleted_cleanup_tx)
        .await?;
    let deleted_cleanup_result = deleted_cleanup_tx.commit().await;
    let deleted_cleanup_rows: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(deleted_cleanup_seed.id)
    .fetch_one(fixture.client.pool())
    .await?;

    // An active opportunity must not become detached from its immutable epoch
    // by changing cluster after the application-level publication check. The
    // deferred trigger must select the base opportunity row and fail closed on
    // the now-missing reciprocal epoch join.
    let active_identity_cluster = fixture.cluster("active_epoch_identity");
    let active_identity_epoch = fixture.seed_epoch(&active_identity_cluster).await?;
    let active_identity_seed = fixture
        .seed_opportunity(
            &active_identity_cluster,
            active_identity_epoch,
            "active-epoch-identity",
            "ready",
            924,
        )
        .await?;
    let mismatched_active_identity_cluster = format!("{active_identity_cluster}:mismatched");
    let active_identity_mismatch_result =
        sqlx::query("UPDATE loyal_yield.rebalance_opportunities SET cluster = $2 WHERE id = $1")
            .bind(active_identity_seed.id)
            .bind(&mismatched_active_identity_cluster)
            .execute(fixture.client.pool())
            .await;
    let active_identity_mismatch_error = active_identity_mismatch_result
        .as_ref()
        .err()
        .map(ToString::to_string);
    let active_identity_mismatch_rejected = active_identity_mismatch_error
        .as_deref()
        .is_some_and(|error| error.contains("active rebalance opportunity cannot commit"));
    let active_identity_cluster_after: String =
        sqlx::query_scalar("SELECT cluster FROM loyal_yield.rebalance_opportunities WHERE id = $1")
            .bind(active_identity_seed.id)
            .fetch_one(fixture.client.pool())
            .await?;

    // Force a cross-row identity mismatch after the atomic handoff has done all
    // of its final application checks but before COMMIT. The signed fence must
    // still find the submission base row and reject the missing reciprocal
    // opportunity/epoch join instead of treating it as deletion cleanup.
    let signed_identity_cluster = fixture.cluster("signed_epoch_identity");
    let signed_identity_epoch = fixture.seed_epoch(&signed_identity_cluster).await?;
    let signed_identity_now = Utc::now();
    let signed_identity_wrong_epoch = fixture
        .client
        .upsert_optimizer_epoch(
            loyal_yield_orchestrator::fleet_orchestration::OptimizerEpochInput {
                cluster: signed_identity_cluster.clone(),
                epoch_key: format!("{signed_identity_cluster}:wrong-signed-epoch"),
                market_slot: 10_001,
                observed_at: signed_identity_now,
                expires_at: signed_identity_now + chrono::Duration::hours(4),
                market_state: json!({"fixture": fixture.prefix, "wrongSignedEpoch": true}),
            },
        )
        .await?;
    fixture
        .seed_opportunity(
            &signed_identity_cluster,
            signed_identity_epoch,
            "signed-epoch-identity",
            "ready",
            923,
        )
        .await?;
    let signed_identity_lease = claim_one(
        &fixture.client,
        &signed_identity_cluster,
        "signed-identity-worker",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let signed_identity_conflicts = vec![
        format!("fleet-shared-write-lane:{}:signed-identity", fixture.prefix),
        format!(
            "vault-write:{}:{}:signed-identity",
            fixture.prefix,
            signed_identity_lease.opportunity.vault_id.as_i64()
        ),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &signed_identity_lease,
            &signed_identity_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    let signed_identity_trigger_sql = format!(
        r#"
        CREATE OR REPLACE FUNCTION loyal_yield.fleet_verifier_force_signed_identity_mismatch()
        RETURNS trigger
        LANGUAGE plpgsql
        AS $fixture$
        BEGIN
            IF NEW.semantic_key LIKE 'semantic:fleet_verify_%:signed-epoch-identity' THEN
                UPDATE loyal_yield.rebalance_opportunities
                SET optimizer_epoch_id = {}
                WHERE id = NEW.opportunity_id;
            END IF;
            RETURN NULL;
        END;
        $fixture$
        "#,
        signed_identity_wrong_epoch.id
    );
    sqlx::query(&signed_identity_trigger_sql)
        .execute(fixture.client.pool())
        .await?;
    sqlx::query(
        "DROP TRIGGER IF EXISTS aaa_fleet_verifier_force_signed_identity_mismatch ON loyal_yield.signed_route_submissions",
    )
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        r#"
        CREATE CONSTRAINT TRIGGER aaa_fleet_verifier_force_signed_identity_mismatch
        AFTER INSERT ON loyal_yield.signed_route_submissions
        DEFERRABLE INITIALLY DEFERRED
        FOR EACH ROW
        EXECUTE FUNCTION loyal_yield.fleet_verifier_force_signed_identity_mismatch()
        "#,
    )
    .execute(fixture.client.pool())
    .await?;
    let signed_identity_result = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            same_mint_input_for_lease(&signed_identity_lease)?,
            &signed_identity_lease,
            target_capacity_input_for_lease(fixture, &signed_identity_lease).await?,
            signed_input_for_lease(
                fixture,
                &signed_identity_lease,
                signed_identity_conflicts,
                "signed-epoch-identity",
            )
            .await?,
        )
        .await;
    sqlx::query(
        "DROP TRIGGER IF EXISTS aaa_fleet_verifier_force_signed_identity_mismatch ON loyal_yield.signed_route_submissions",
    )
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        "DROP FUNCTION IF EXISTS loyal_yield.fleet_verifier_force_signed_identity_mismatch()",
    )
    .execute(fixture.client.pool())
    .await?;
    let signed_identity_error = signed_identity_result
        .as_ref()
        .err()
        .map(ToString::to_string);
    let signed_identity_mismatch_rejected = signed_identity_error
        .as_deref()
        .is_some_and(|error| error.contains("signed route handoff cannot commit"));
    let signed_identity_decisions: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.rebalance_decisions WHERE vault_id = $1",
    )
    .bind(signed_identity_lease.opportunity.vault_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let signed_identity_submissions: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE opportunity_id = $1",
    )
    .bind(signed_identity_lease.opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let signed_identity_epoch_after: i64 = sqlx::query_scalar(
        "SELECT optimizer_epoch_id FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(signed_identity_lease.opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;

    // Once a safely committed transaction has been broadcast, recording the
    // normal signed -> submitted transition must remain legal even if less than
    // sixty seconds of opportunity lifetime remain. Terminal cleanup is also
    // legal, while any terminal -> signed reactivation must re-enter the fence.
    let state_guard_cluster = fixture.cluster("signed_state_guard");
    let state_guard_epoch = fixture.seed_epoch(&state_guard_cluster).await?;
    fixture
        .seed_opportunity(
            &state_guard_cluster,
            state_guard_epoch,
            "signed-state-guard",
            "ready",
            922,
        )
        .await?;
    let state_guard_lease = claim_one(
        &fixture.client,
        &state_guard_cluster,
        "signed-state-worker",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let state_guard_conflicts = vec![
        format!(
            "fleet-shared-write-lane:{}:signed-state-guard",
            fixture.prefix
        ),
        format!(
            "vault-write:{}:{}:signed-state-guard",
            fixture.prefix,
            state_guard_lease.opportunity.vault_id.as_i64()
        ),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &state_guard_lease,
            &state_guard_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    let (_, state_guard_submission) = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            same_mint_input_for_lease(&state_guard_lease)?,
            &state_guard_lease,
            target_capacity_input_for_lease(fixture, &state_guard_lease).await?,
            signed_input_for_lease(
                fixture,
                &state_guard_lease,
                state_guard_conflicts,
                "signed-state-guard",
            )
            .await?,
        )
        .await?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_opportunities SET expires_at = clock_timestamp() + interval '30 seconds' WHERE id = $1",
    )
    .bind(state_guard_lease.opportunity.id)
    .execute(fixture.client.pool())
    .await?;
    let normal_submitted_result = sqlx::query(
        r#"
        UPDATE loyal_yield.signed_route_submissions
        SET submission_state = 'submitted',
            submitted_slot = 10001,
            submitted_at = clock_timestamp(),
            updated_at = clock_timestamp()
        WHERE id = $1
        "#,
    )
    .bind(state_guard_submission.id)
    .execute(fixture.client.pool())
    .await;
    let state_after_normal_submission: String = sqlx::query_scalar(
        "SELECT submission_state FROM loyal_yield.signed_route_submissions WHERE id = $1",
    )
    .bind(state_guard_submission.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let terminal_cleanup_result = sqlx::query(
        r#"
        UPDATE loyal_yield.signed_route_submissions
        SET submission_state = 'failed',
            error_detail = 'fleet verifier terminal cleanup',
            updated_at = clock_timestamp()
        WHERE id = $1
        "#,
    )
    .bind(state_guard_submission.id)
    .execute(fixture.client.pool())
    .await;
    let reactivation_result = sqlx::query(
        r#"
        UPDATE loyal_yield.signed_route_submissions
        SET submission_state = 'signed',
            updated_at = clock_timestamp()
        WHERE id = $1
        "#,
    )
    .bind(state_guard_submission.id)
    .execute(fixture.client.pool())
    .await;
    let reactivation_error = reactivation_result.as_ref().err().map(ToString::to_string);
    let reactivation_rejected = reactivation_error
        .as_deref()
        .is_some_and(|error| error.contains("signed route handoff cannot commit"));
    let (state_after_reactivation_attempt, opportunity_state_after_terminal_cleanup): (
        String,
        String,
    ) = sqlx::query_as(
        r#"
        SELECT submission.submission_state, opportunity.opportunity_state
        FROM loyal_yield.signed_route_submissions submission
        JOIN loyal_yield.rebalance_opportunities opportunity
          ON opportunity.id = submission.opportunity_id
        WHERE submission.id = $1
        "#,
    )
    .bind(state_guard_submission.id)
    .fetch_one(fixture.client.pool())
    .await?;

    let stale_cluster = fixture.cluster("stale_sweep");
    let stale_epoch = fixture.seed_epoch(&stale_cluster).await?;
    let stale_seed = fixture
        .seed_opportunity(&stale_cluster, stale_epoch, "stale", "ready", 900)
        .await?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_opportunities SET expires_at = now() - interval '1 second' WHERE id = $1",
    )
    .bind(stale_seed.id)
    .execute(fixture.client.pool())
    .await?;
    let swept_expired = fixture
        .client
        .sweep_expired_rebalance_opportunities(&stale_cluster, 32)
        .await?;
    let swept_state: String = sqlx::query_scalar(
        "SELECT opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(stale_seed.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let swept_claim = fixture
        .client
        .lease_next_rebalance_opportunity(
            &stale_cluster,
            "stale-worker",
            RebalanceOpportunityClaimKind::Execute,
            Utc::now() + chrono::Duration::minutes(5),
        )
        .await?;

    let stale_rediscovery_cluster = fixture.cluster("stale_rediscovery");
    let stale_rediscovery_epoch = fixture.seed_epoch(&stale_rediscovery_cluster).await?;
    let stale_rediscovery_fixture = fixture
        .seed_opportunity(
            &stale_rediscovery_cluster,
            stale_rediscovery_epoch,
            "stale-rediscovery",
            "ready",
            901,
        )
        .await?;
    let stale_rediscovery_fixture_record = fixture
        .client
        .rebalance_opportunity(stale_rediscovery_fixture.id)
        .await?
        .ok_or("stale rediscovery fixture disappeared")?;
    let stale_retry_seed = fixture
        .client
        .upsert_rebalance_opportunity(rediscovery_input_for_opportunity(
            &stale_rediscovery_fixture_record,
        ))
        .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.rebalance_opportunities
        SET opportunity_state = 'stale',
            terminal_reason = 'optimizer_epoch_expired',
            updated_at = clock_timestamp()
        WHERE id = $1
        "#,
    )
    .bind(stale_retry_seed.id)
    .execute(fixture.client.pool())
    .await?;
    let stale_rediscovered = fixture
        .client
        .upsert_rebalance_opportunity(rediscovery_input_for_opportunity(&stale_retry_seed))
        .await?;
    let stale_attempt_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.rebalance_opportunities WHERE rediscovery_key = $1",
    )
    .bind(&stale_retry_seed.rediscovery_key)
    .fetch_one(fixture.client.pool())
    .await?;

    let conflict_cluster = fixture.cluster("conflicts");
    let conflict_epoch = fixture.seed_epoch(&conflict_cluster).await?;
    for index in 0..66 {
        fixture
            .seed_opportunity(
                &conflict_cluster,
                conflict_epoch,
                &format!("conflict-{index}"),
                "ready",
                10_000 - index,
            )
            .await?;
    }
    let mut independent_count = 0usize;
    let mut first_vault_key = String::new();
    let first_lane_key = format!("fleet-shared-write-lane:{}:0", fixture.prefix);
    for lane in 0..64 {
        let lease = claim_one(
            &fixture.client,
            &conflict_cluster,
            &format!("lane-worker-{lane}"),
            RebalanceOpportunityClaimKind::Execute,
        )
        .await?;
        let vault_key = format!(
            "vault-write:{}:{}",
            fixture.prefix,
            lease.opportunity.vault_id.as_i64()
        );
        let lane_key = format!("fleet-shared-write-lane:{}:{lane}", fixture.prefix);
        fixture
            .client
            .acquire_route_account_conflict_leases(
                &lease,
                &[vault_key.clone(), lane_key],
                Utc::now() + chrono::Duration::minutes(4),
            )
            .await?;
        if lane == 0 {
            first_vault_key = vault_key;
        }
        independent_count += 1;
    }
    let same_vault_contender = claim_one(
        &fixture.client,
        &conflict_cluster,
        "same-vault-contender",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let same_vault_rejected = fixture
        .client
        .acquire_route_account_conflict_leases(
            &same_vault_contender,
            &[
                first_vault_key.clone(),
                format!("fleet-shared-write-lane:{}:extra", fixture.prefix),
            ],
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await
        .is_err();
    let same_lane_contender = claim_one(
        &fixture.client,
        &conflict_cluster,
        "same-lane-contender",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let same_lane_rejected = fixture
        .client
        .acquire_route_account_conflict_leases(
            &same_lane_contender,
            &[
                format!(
                    "vault-write:{}:{}",
                    fixture.prefix,
                    same_lane_contender.opportunity.vault_id.as_i64()
                ),
                first_lane_key.clone(),
            ],
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await
        .is_err();

    let capacity_cluster = fixture.cluster("target_capacity");
    let capacity_epoch = fixture.seed_epoch(&capacity_cluster).await?;
    let first_capacity_seed = fixture
        .seed_opportunity(
            &capacity_cluster,
            capacity_epoch,
            "capacity-first",
            "ready",
            2_000,
        )
        .await?;
    let second_capacity_seed = fixture
        .seed_opportunity(
            &capacity_cluster,
            capacity_epoch,
            "capacity-second",
            "ready",
            1_000,
        )
        .await?;
    let third_capacity_seed = fixture
        .seed_opportunity(
            &capacity_cluster,
            capacity_epoch,
            "capacity-third",
            "ready",
            500,
        )
        .await?;
    let shared_capacity_target: String = sqlx::query_scalar(
        "SELECT target_reserve FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(first_capacity_seed.id)
    .fetch_one(fixture.client.pool())
    .await?;
    sqlx::query("UPDATE loyal_yield.rebalance_opportunities SET target_reserve = $2 WHERE id = $1")
        .bind(second_capacity_seed.id)
        .bind(&shared_capacity_target)
        .execute(fixture.client.pool())
        .await?;
    sqlx::query("UPDATE loyal_yield.rebalance_opportunities SET target_reserve = $2 WHERE id = $1")
        .bind(third_capacity_seed.id)
        .bind(&shared_capacity_target)
        .execute(fixture.client.pool())
        .await?;
    let first_capacity_lease = claim_one(
        &fixture.client,
        &capacity_cluster,
        "capacity-executor-first",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let second_capacity_lease = claim_one(
        &fixture.client,
        &capacity_cluster,
        "capacity-executor-second",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let third_capacity_lease = claim_one(
        &fixture.client,
        &capacity_cluster,
        "capacity-executor-third",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let shared_capacity_observation = TargetCapacityObservation {
        cluster: capacity_cluster.clone(),
        target_reserve: shared_capacity_target.clone(),
        liquidity_mint: "USDC".to_owned(),
        observed_supply_usd_micros: 10_000_000_000,
        observed_slot: 20_000,
        maximum_inflight_usd_micros: 250_000_000,
    };
    let shared_capacity_projection = fixture
        .client
        .observe_target_capacity(shared_capacity_observation)
        .await?;
    let first_capacity_input = target_capacity_input_from_projection(
        &first_capacity_lease,
        shared_capacity_projection.clone(),
    );
    let second_capacity_input = target_capacity_input_from_projection(
        &second_capacity_lease,
        shared_capacity_projection.clone(),
    );
    let third_capacity_input =
        target_capacity_input_from_projection(&third_capacity_lease, shared_capacity_projection);
    let first_capacity_attempt = async {
        let mut tx = fixture
            .client
            .pool()
            .begin()
            .await
            .map_err(|error| error.to_string())?;
        match NeonSqlClient::reserve_target_capacity_in_connection(
            &mut tx,
            &first_capacity_lease,
            &first_capacity_input,
            5_000,
        )
        .await
        {
            Ok(reservation) => {
                tx.commit().await.map_err(|error| error.to_string())?;
                Ok(reservation)
            }
            Err(error) => {
                tx.rollback().await.map_err(|error| error.to_string())?;
                Err(error.to_string())
            }
        }
    };
    let second_capacity_attempt = async {
        let mut tx = fixture
            .client
            .pool()
            .begin()
            .await
            .map_err(|error| error.to_string())?;
        match NeonSqlClient::reserve_target_capacity_in_connection(
            &mut tx,
            &second_capacity_lease,
            &second_capacity_input,
            5_000,
        )
        .await
        {
            Ok(reservation) => {
                tx.commit().await.map_err(|error| error.to_string())?;
                Ok(reservation)
            }
            Err(error) => {
                tx.rollback().await.map_err(|error| error.to_string())?;
                Err(error.to_string())
            }
        }
    };
    let third_capacity_attempt = async {
        let mut tx = fixture
            .client
            .pool()
            .begin()
            .await
            .map_err(|error| error.to_string())?;
        match NeonSqlClient::reserve_target_capacity_in_connection(
            &mut tx,
            &third_capacity_lease,
            &third_capacity_input,
            5_000,
        )
        .await
        {
            Ok(reservation) => {
                tx.commit().await.map_err(|error| error.to_string())?;
                Ok(reservation)
            }
            Err(error) => {
                tx.rollback().await.map_err(|error| error.to_string())?;
                Err(error.to_string())
            }
        }
    };
    let (first_capacity_result, second_capacity_result, third_capacity_result) = tokio::join!(
        first_capacity_attempt,
        second_capacity_attempt,
        third_capacity_attempt
    );
    let capacity_results = [
        &first_capacity_result,
        &second_capacity_result,
        &third_capacity_result,
    ];
    let admitted_capacity_reservations = capacity_results
        .iter()
        .filter(|result| result.is_ok())
        .count();
    let capacity_rejection_errors = capacity_results
        .iter()
        .filter_map(|result| result.as_ref().err().cloned())
        .collect::<Vec<_>>();
    let capacity_excess_rejections = capacity_rejection_errors
        .iter()
        .filter(|error| error.contains("target capacity exhausted"))
        .count();
    let capacity_telemetry_fence_rejections = capacity_rejection_errors
        .iter()
        .filter(|error| error.contains("telemetry changed"))
        .count();
    let admitted_reservation_generations = capacity_results
        .iter()
        .filter_map(|result| {
            result
                .as_ref()
                .ok()
                .map(|reservation| reservation.reservation_generation)
        })
        .collect::<BTreeSet<_>>();
    let admitted_projected_target_apys = capacity_results
        .iter()
        .filter_map(|result| {
            result
                .as_ref()
                .ok()
                .map(|reservation| reservation.admitted_projected_target_apy_bps)
        })
        .collect::<BTreeSet<_>>();
    let admitted_atomic_economics_recomputed = capacity_results.iter().all(|result| match result {
        Ok(reservation) => {
            reservation.admitted_edge_bps
                == reservation.admitted_projected_target_apy_bps
                    - reservation.admitted_source_apy_bps
                && reservation.admitted_fee_cap_lamports >= 5_000
        }
        Err(_) => true,
    });
    let capacity_winner = capacity_results
        .iter()
        .find_map(|result| result.as_ref().ok())
        .ok_or("concurrent target-capacity fixture admitted no route")?;
    let live_capacity_usd_micros: i64 = sqlx::query_scalar(
        r#"
        SELECT COALESCE(SUM(principal_usd_micros), 0)::BIGINT
        FROM loyal_yield.target_capacity_reservations
        WHERE cluster = $1 AND target_reserve = $2
          AND reservation_state <> 'released'
        "#,
    )
    .bind(&capacity_cluster)
    .bind(&shared_capacity_target)
    .fetch_one(fixture.client.pool())
    .await?;
    let stale_state_capacity_release = fixture
        .client
        .release_unattached_target_capacity_reservation(
            capacity_winner.opportunity_id,
            capacity_winner.state_version + 1,
            capacity_winner.reservation_fencing_token,
            "verifier_stale_state",
        )
        .await?;
    let stale_fence_capacity_release = fixture
        .client
        .release_unattached_target_capacity_reservation(
            capacity_winner.opportunity_id,
            capacity_winner.state_version,
            capacity_winner.reservation_fencing_token + 1,
            "verifier_stale_fence",
        )
        .await?;
    let current_capacity_release = fixture
        .client
        .release_unattached_target_capacity_reservation(
            capacity_winner.opportunity_id,
            capacity_winner.state_version,
            capacity_winner.reservation_fencing_token,
            "verifier_pre_handoff_cleanup",
        )
        .await?;
    let target_capacity_concurrency_passed = admitted_capacity_reservations == 2
        && capacity_excess_rejections == 1
        && capacity_telemetry_fence_rejections == 0
        && admitted_reservation_generations == BTreeSet::from([1_i64, 2_i64])
        && admitted_projected_target_apys.len() == 2
        && admitted_atomic_economics_recomputed
        && live_capacity_usd_micros == 200_000_000
        && live_capacity_usd_micros <= 250_000_000
        && !stale_state_capacity_release
        && !stale_fence_capacity_release
        && current_capacity_release;

    // Spend reservations are immutable by design, so this isolated-database
    // fixture uses a separate prefix and intentionally retains its audit rows.
    // The caller already requires a disposable database name containing
    // `fleet_verify`; normal mutable fixture cleanup remains independently
    // checked below.
    let fee_floor_fixture = DatabaseFixture {
        client: fixture.client.clone(),
        latency_client: fixture.latency_client.clone(),
        prefix: format!("immutable_fee_floor_{}", fixture.prefix),
    };
    let fee_floor_cluster = fee_floor_fixture.cluster("admission");
    let fee_floor_epoch = fee_floor_fixture.seed_epoch(&fee_floor_cluster).await?;
    fee_floor_fixture
        .seed_opportunity(
            &fee_floor_cluster,
            fee_floor_epoch,
            "floor-first",
            "ready",
            2_000,
        )
        .await?;
    fee_floor_fixture
        .seed_opportunity(
            &fee_floor_cluster,
            fee_floor_epoch,
            "floor-second",
            "ready",
            1_000,
        )
        .await?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_opportunities SET estimated_cost_lamports = 50000 WHERE cluster = $1",
    )
    .bind(&fee_floor_cluster)
    .execute(fixture.client.pool())
    .await?;
    let fee_floor_payer = format!("fee-shard:{}", fee_floor_fixture.prefix);
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.route_fee_payer_shards
            (cluster, fee_payer, enabled, minimum_balance_lamports,
             maximum_balance_lamports, rolling_window_seconds,
             maximum_window_spend_lamports, maximum_transaction_fee_lamports)
        VALUES ($1, $2, TRUE, 50000, 200000, 3600, 80000, 50000)
        "#,
    )
    .bind(&fee_floor_cluster)
    .bind(&fee_floor_payer)
    .execute(fixture.client.pool())
    .await?;
    let first_floor_lease = claim_one(
        &fixture.client,
        &fee_floor_cluster,
        "fee-floor-first",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let first_floor_conflicts = vec![
        format!(
            "fleet-shared-write-lane:{}:floor-first",
            fee_floor_fixture.prefix
        ),
        format!(
            "vault-write:{}:{}",
            fee_floor_fixture.prefix,
            first_floor_lease.opportunity.vault_id.as_i64()
        ),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &first_floor_lease,
            &first_floor_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    let (_, first_floor_submission) = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            same_mint_input_for_lease(&first_floor_lease)?,
            &first_floor_lease,
            target_capacity_input_for_lease(&fee_floor_fixture, &first_floor_lease).await?,
            fee_shard_signed_input_for_lease(
                &fee_floor_fixture,
                &first_floor_lease,
                first_floor_conflicts,
                "floor-first",
                &fee_floor_payer,
                100_000,
                30_000,
            )
            .await?,
        )
        .await?;
    let second_floor_lease = claim_one(
        &fixture.client,
        &fee_floor_cluster,
        "fee-floor-second",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let second_floor_conflicts = vec![
        format!(
            "fleet-shared-write-lane:{}:floor-second",
            fee_floor_fixture.prefix
        ),
        format!(
            "vault-write:{}:{}",
            fee_floor_fixture.prefix,
            second_floor_lease.opportunity.vault_id.as_i64()
        ),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &second_floor_lease,
            &second_floor_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    let second_floor_same_mint = same_mint_input_for_lease(&second_floor_lease)?;
    let second_floor_signed = fee_shard_signed_input_for_lease(
        &fee_floor_fixture,
        &second_floor_lease,
        second_floor_conflicts,
        "floor-second",
        &fee_floor_payer,
        100_000,
        30_000,
    )
    .await?;
    let second_floor_capacity =
        target_capacity_input_for_lease(&fee_floor_fixture, &second_floor_lease).await?;
    let second_floor_blocked = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            second_floor_same_mint.clone(),
            &second_floor_lease,
            second_floor_capacity.clone(),
            second_floor_signed.clone(),
        )
        .await;
    let second_floor_blocked_error = second_floor_blocked.as_ref().err().map(ToString::to_string);
    let first_floor_confirmation_lease = fixture
        .client
        .lease_pending_signed_route_submissions(
            &fee_floor_cluster,
            "fee-floor-terminalizer",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .next()
        .ok_or("fee-floor fixture could not lease its first signed submission")?;
    let terminal_first_floor_submission = fixture
        .client
        .advance_signed_route_submission(
            &first_floor_confirmation_lease,
            SignedRouteSubmissionAdvance::Failed {
                checked_at: Utc::now(),
                confirmed_slot: None,
                error_detail: "synthetic terminal floor release".to_owned(),
            },
        )
        .await?;
    let (first_floor_capacity_state, first_floor_broadcast_count): (String, i32) = sqlx::query_as(
        r#"
            SELECT reservation.reservation_state, submission.broadcast_count
            FROM loyal_yield.target_capacity_reservations reservation
            JOIN loyal_yield.signed_route_submissions submission
              ON submission.id = reservation.signed_submission_id
            WHERE reservation.opportunity_id = $1
            "#,
    )
    .bind(first_floor_lease.opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let (_, second_floor_submission) = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            second_floor_same_mint,
            &second_floor_lease,
            second_floor_capacity,
            second_floor_signed,
        )
        .await?;
    let landed_failure_lease = fixture
        .client
        .lease_pending_signed_route_submissions(
            &fee_floor_cluster,
            "fee-floor-landed-failure",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .find(|lease| lease.submission.id == second_floor_submission.id)
        .ok_or("fee-floor fixture could not lease its landed-failure submission")?;
    let landed_failure = fixture
        .client
        .advance_signed_route_submission(
            &landed_failure_lease,
            SignedRouteSubmissionAdvance::Failed {
                checked_at: Utc::now(),
                confirmed_slot: Some(10_001),
                error_detail: "synthetic authoritative landed failure".to_owned(),
            },
        )
        .await?;
    let landed_failure_retains_slot = landed_failure.state.as_str() == "failed"
        && landed_failure.confirmed_slot == Some(10_001)
        && landed_failure.confirmed_at.is_some();
    let fee_floor_reservations: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.route_fee_payer_spend_reservations WHERE cluster = $1 AND fee_payer = $2",
    )
    .bind(&fee_floor_cluster)
    .bind(&fee_floor_payer)
    .fetch_one(fixture.client.pool())
    .await?;
    let fee_floor_admission_passed = second_floor_blocked_error
        .as_deref()
        .is_some_and(|error| error.contains("fee_payer_reselection_required"))
        && terminal_first_floor_submission.state.as_str() == "failed"
        && landed_failure_retains_slot
        && fee_floor_reservations == 2
        && first_floor_submission.compiled_fee_lamports == 30_000;
    let pre_send_terminal_failure_released_capacity =
        first_floor_broadcast_count == 0 && first_floor_capacity_state == "released";

    let setup_funding_fixture = DatabaseFixture {
        client: fixture.client.clone(),
        latency_client: fixture.latency_client.clone(),
        prefix: format!("setup_funding_{}", fixture.prefix),
    };
    let setup_funding_cluster = setup_funding_fixture.cluster("admission");
    let setup_funding_epoch = setup_funding_fixture
        .seed_epoch(&setup_funding_cluster)
        .await?;
    setup_funding_fixture
        .seed_opportunity(
            &setup_funding_cluster,
            setup_funding_epoch,
            "first",
            "ready",
            2_000,
        )
        .await?;
    setup_funding_fixture
        .seed_opportunity(
            &setup_funding_cluster,
            setup_funding_epoch,
            "second",
            "ready",
            1_000,
        )
        .await?;
    let setup_funding_payer = format!("authority:{setup_funding_cluster}");
    let first_setup_lease = claim_one(
        &fixture.client,
        &setup_funding_cluster,
        "setup-funding-first",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let first_setup_conflicts = vec![
        format!(
            "fleet-shared-write-lane:{}:first",
            setup_funding_fixture.prefix
        ),
        format!(
            "vault-write:{}:{}",
            setup_funding_fixture.prefix,
            first_setup_lease.opportunity.vault_id.as_i64()
        ),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &first_setup_lease,
            &first_setup_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    let mut first_setup_input = signed_input_for_lease(
        &setup_funding_fixture,
        &first_setup_lease,
        first_setup_conflicts,
        "first",
    )
    .await?;
    first_setup_input.policy_setup_funding_lamports = Some(30_000);
    first_setup_input.fee_payer_balance_lamports = Some(100_000);
    first_setup_input.fee_payer_balance_slot = Some(10_000);
    first_setup_input.fee_payer_balance_observed_at = Some(Utc::now());
    let (_, first_setup_submission) = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            same_mint_input_for_lease(&first_setup_lease)?,
            &first_setup_lease,
            target_capacity_input_for_lease(&setup_funding_fixture, &first_setup_lease).await?,
            first_setup_input,
        )
        .await?;

    let second_setup_lease = claim_one(
        &fixture.client,
        &setup_funding_cluster,
        "setup-funding-second",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let second_setup_conflicts = vec![
        format!(
            "fleet-shared-write-lane:{}:second",
            setup_funding_fixture.prefix
        ),
        format!(
            "vault-write:{}:{}",
            setup_funding_fixture.prefix,
            second_setup_lease.opportunity.vault_id.as_i64()
        ),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &second_setup_lease,
            &second_setup_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    let mut second_setup_input = signed_input_for_lease(
        &setup_funding_fixture,
        &second_setup_lease,
        second_setup_conflicts,
        "second",
    )
    .await?;
    second_setup_input.policy_setup_funding_lamports = Some(70_000);
    second_setup_input.fee_payer_balance_lamports = Some(100_000);
    second_setup_input.fee_payer_balance_slot = Some(10_000);
    second_setup_input.fee_payer_balance_observed_at = Some(Utc::now());
    let second_setup_same_mint = same_mint_input_for_lease(&second_setup_lease)?;
    let second_setup_capacity =
        target_capacity_input_for_lease(&setup_funding_fixture, &second_setup_lease).await?;
    let second_setup_blocked = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            second_setup_same_mint.clone(),
            &second_setup_lease,
            second_setup_capacity.clone(),
            second_setup_input.clone(),
        )
        .await;
    let second_setup_blocked_error = second_setup_blocked.as_ref().err().map(ToString::to_string);

    let first_setup_confirmation_lease = fixture
        .client
        .lease_pending_signed_route_submissions(
            &setup_funding_cluster,
            "setup-funding-terminalizer",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .find(|lease| lease.submission.id == first_setup_submission.id)
        .ok_or("setup-funding fixture could not lease its first submission")?;
    let terminal_first_setup_submission = fixture
        .client
        .advance_signed_route_submission(
            &first_setup_confirmation_lease,
            SignedRouteSubmissionAdvance::Failed {
                checked_at: Utc::now(),
                confirmed_slot: None,
                error_detail: "synthetic pre-send setup reservation release".to_owned(),
            },
        )
        .await?;
    second_setup_input.fee_payer_balance_lamports = Some(200_000);
    second_setup_input.fee_payer_balance_slot = Some(9_999);
    second_setup_input.fee_payer_balance_observed_at = Some(Utc::now());
    let second_setup_stale_snapshot = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            second_setup_same_mint.clone(),
            &second_setup_lease,
            second_setup_capacity.clone(),
            second_setup_input.clone(),
        )
        .await;
    let second_setup_stale_snapshot_error = second_setup_stale_snapshot
        .as_ref()
        .err()
        .map(ToString::to_string);
    second_setup_input.fee_payer_balance_lamports = Some(100_000);
    second_setup_input.fee_payer_balance_slot = Some(10_000);
    second_setup_input.fee_payer_balance_observed_at = Some(Utc::now());
    let (_, second_setup_submission) = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            second_setup_same_mint,
            &second_setup_lease,
            second_setup_capacity,
            second_setup_input,
        )
        .await?;
    let setup_funding_reservation_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.route_policy_setup_funding_reservations WHERE cluster = $1 AND payer = $2",
    )
    .bind(&setup_funding_cluster)
    .bind(&setup_funding_payer)
    .fetch_one(fixture.client.pool())
    .await?;
    let policy_setup_funding_reservations_passed = second_setup_blocked_error
        .as_deref()
        .is_some_and(|error| error.contains("policy_setup_funding_reselection_required"))
        && second_setup_stale_snapshot_error
            .as_deref()
            .is_some_and(|error| error.contains("durable admission frontier"))
        && setup_funding_reservation_count == 2
        && second_setup_submission.state.as_str() == "signed";

    let submission_cluster = fixture.cluster("submission");
    let submission_epoch = fixture.seed_epoch(&submission_cluster).await?;
    let signed_route_seed = fixture
        .seed_opportunity(
            &submission_cluster,
            submission_epoch,
            "signed-route",
            "revalidate",
            2_000,
        )
        .await?;
    let signed_route_seed_record = fixture
        .client
        .rebalance_opportunity(signed_route_seed.id)
        .await?
        .ok_or("signed-route seed opportunity disappeared")?;
    let signed_route_rediscovery_input =
        rediscovery_input_for_opportunity(&signed_route_seed_record);
    sqlx::query("DELETE FROM loyal_yield.rebalance_opportunities WHERE id = $1")
        .bind(signed_route_seed.id)
        .execute(fixture.client.pool())
        .await?;
    let signed_route_published = fixture
        .client
        .upsert_rebalance_opportunity(signed_route_rediscovery_input.clone())
        .await?;
    let signed_route_revalidation = claim_one(
        &fixture.client,
        &submission_cluster,
        "signed-route-revalidator",
        RebalanceOpportunityClaimKind::Revalidate,
    )
    .await?;
    let signed_route_ready = fixture
        .client
        .advance_rebalance_opportunity(
            signed_route_published.id,
            &signed_route_revalidation,
            RebalanceOpportunityAdvance {
                next_state: RebalanceOpportunityState::Ready,
                available_at: Some(Utc::now()),
                decision_id: None,
                reason: None,
                route_fingerprint: signed_route_published.route_fingerprint.clone(),
                requirements_fingerprint: signed_route_published.requirements_fingerprint.clone(),
                execution_plan: Some(signed_route_published.execution_plan.clone()),
                provisioning_request_id: None,
            },
        )
        .await?
        .into_applied(signed_route_published.id)?;
    fixture
        .seed_opportunity(
            &submission_cluster,
            submission_epoch,
            "signed-route-thief",
            "ready",
            1_000,
        )
        .await?;
    fixture
        .seed_opportunity(
            &submission_cluster,
            submission_epoch,
            "signed-route-ambiguous-thief",
            "ready",
            900,
        )
        .await?;
    let signed_route_lease = claim_one(
        &fixture.client,
        &submission_cluster,
        "signed-route-executor",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    if signed_route_lease.opportunity.id != signed_route_ready.id {
        return Err("signed-route product-published opportunity lost priority".into());
    }
    let semantic_conflicts = vec![
        format!("fleet-shared-write-lane:{}:signed", fixture.prefix),
        format!("policy-setup-funding:{}:signed", fixture.prefix),
        format!("vault-write:{}:signed", fixture.prefix),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &signed_route_lease,
            &semantic_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    let signed_input = signed_input_for_lease(
        fixture,
        &signed_route_lease,
        semantic_conflicts.clone(),
        "signed-route",
    )
    .await?;
    let mut same_mint_input = same_mint_input_for_lease(&signed_route_lease)?;
    let published_amount_raw = same_mint_input.amount_raw;
    let accrued_amount_raw = published_amount_raw
        .checked_add((published_amount_raw / 1_000_000).max(1))
        .ok_or("signed fixture accrued amount overflowed")?;
    same_mint_input.amount_raw = accrued_amount_raw;
    same_mint_input.redeemable_source_liquidity_amount_raw = Some(accrued_amount_raw);
    let capacity_input = target_capacity_input_for_lease(fixture, &signed_route_lease).await?;
    let mut refreshed_economics_input = same_mint_input.clone();
    refreshed_economics_input.source_apy_bps += 1;
    refreshed_economics_input.estimated_edge_bps -= 1;
    let refreshed_economics_rejected = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            refreshed_economics_input,
            &signed_route_lease,
            capacity_input.clone(),
            signed_input.clone(),
        )
        .await
        .is_err();
    let (prepared, persisted) = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            same_mint_input,
            &signed_route_lease,
            capacity_input,
            signed_input,
        )
        .await?;
    let immutable_opportunity = fixture
        .client
        .rebalance_opportunity(signed_route_lease.opportunity.id)
        .await?
        .ok_or("atomic signed fixture opportunity disappeared")?;
    let decision_id = prepared
        .decision_id
        .ok_or("atomic signed fixture did not return a decision")?;
    let (
        durable_decision_amount_raw,
        durable_decision_source_apy_bps,
        durable_decision_target_apy_bps,
        durable_decision_estimated_edge_bps,
        durable_decision_plan,
    ): (i64, i64, i64, i64, Value) = sqlx::query_as(
        "SELECT amount_raw, source_apy_bps, target_apy_bps, estimated_edge_bps, execution_plan FROM loyal_yield.rebalance_decisions WHERE id = $1",
    )
    .bind(decision_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let bounded_accrual_preserves_discovery_and_binds_signed_decision =
        immutable_opportunity.amount_raw == published_amount_raw
            && durable_decision_amount_raw == accrued_amount_raw
            && durable_decision_plan
                .get("amount_raw")
                .and_then(Value::as_i64)
                == Some(accrued_amount_raw)
            && durable_decision_plan
                .get("redeemable_source_liquidity_amount_raw")
                .and_then(Value::as_i64)
                == Some(accrued_amount_raw);
    let published_economics_bind_signed_decision = refreshed_economics_rejected
        && durable_decision_source_apy_bps == signed_route_lease.opportunity.source_apy_bps
        && durable_decision_target_apy_bps == signed_route_lease.opportunity.target_apy_bps
        && durable_decision_estimated_edge_bps == signed_route_lease.opportunity.estimated_edge_bps;
    let decision_linked = persisted.decision_id == Some(decision_id);
    let signed_evidence_immutable = sqlx::query(
        "UPDATE loyal_yield.signed_route_submissions SET message_hash = 'tampered' WHERE id = $1",
    )
    .bind(persisted.id)
    .execute(fixture.client.pool())
    .await
    .is_err();
    let submission_state_timestamp_immutable = sqlx::query(
        "UPDATE loyal_yield.signed_route_submissions SET submission_state_entered_at = submission_state_entered_at - interval '1 hour' WHERE id = $1",
    )
    .bind(persisted.id)
    .execute(fixture.client.pool())
    .await
    .is_err();
    let fee_payer_kind_immutable = sqlx::query(
        "UPDATE loyal_yield.signed_route_submissions SET fee_payer_kind = 'fee_only_shard' WHERE id = $1",
    )
    .bind(persisted.id)
    .execute(fixture.client.pool())
    .await
    .is_err();

    sqlx::query(
        r#"
        UPDATE loyal_yield.route_account_conflict_leases
        SET created_at = now() - interval '1 hour',
            expires_at = now() - interval '1 second'
        WHERE submission_id = $1
        "#,
    )
    .bind(persisted.id)
    .execute(fixture.client.pool())
    .await?;
    let thief_lease = claim_one(
        &fixture.client,
        &submission_cluster,
        "signed-route-thief",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let attached_conflicts_not_stolen = fixture
        .client
        .acquire_route_account_conflict_leases(
            &thief_lease,
            &semantic_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await
        .is_err();

    let confirmer_first = fixture
        .client
        .lease_pending_signed_route_submissions(
            &submission_cluster,
            "confirmer-crashed",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .next()
        .ok_or("confirmer could not claim signed fixture")?;
    sqlx::query(
        "UPDATE loyal_yield.signed_route_submissions SET confirmation_lease_expires_at = now() - interval '1 second' WHERE id = $1",
    )
    .bind(persisted.id)
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        "UPDATE loyal_yield.route_account_conflict_leases SET expires_at = now() - interval '1 second' WHERE submission_id = $1",
    )
    .bind(persisted.id)
    .execute(fixture.client.pool())
    .await?;
    let confirmer_reclaimed = fixture
        .client
        .lease_pending_signed_route_submissions(
            &submission_cluster,
            "confirmer-replacement",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .next()
        .ok_or("replacement confirmer could not reclaim signed fixture")?;
    let (confirmer_keys, confirmer_min_expiry) =
        conflict_rows(&fixture.client, persisted.id).await?;
    let confirmer_renewed_exact_set = confirmer_keys == semantic_conflicts
        && confirmer_min_expiry.is_some_and(|expires_at| {
            expires_at >= confirmer_reclaimed.expires_at + chrono::Duration::minutes(2)
        });
    let confirmer_fenced = confirmer_reclaimed.fencing_token > confirmer_first.fencing_token;
    let ambiguous_conflicts = semantic_conflicts
        .iter()
        .filter(|key| !key.starts_with("fleet-shared-write-lane:"))
        .cloned()
        .collect::<Vec<_>>();
    let reconciliation_conflicts = semantic_conflicts
        .iter()
        .filter(|key| {
            !key.starts_with("fleet-shared-write-lane:")
                && !key.starts_with("policy-setup-funding:")
        })
        .cloned()
        .collect::<Vec<_>>();
    fixture
        .client
        .ensure_signed_route_decision_confirming(&persisted)
        .await?;
    let confirming_state_after_explicit_transition: String = sqlx::query_scalar(
        "SELECT status::text FROM loyal_yield.rebalance_decisions WHERE id = $1",
    )
    .bind(decision_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    fixture
        .client
        .advance_signed_route_submission(
            &confirmer_reclaimed,
            SignedRouteSubmissionAdvance::BroadcastIntent {
                checked_at: Utc::now(),
            },
        )
        .await?;
    let submitted_at = Utc::now();
    let submitted_route = fixture
        .client
        .advance_signed_route_submission(
            &confirmer_reclaimed,
            SignedRouteSubmissionAdvance::Submitted {
                checked_at: submitted_at,
                observed_slot: Some(49_999),
                next_poll_at: submitted_at,
                // BroadcastIntent already durably accounted for the send.
                broadcasted: false,
            },
        )
        .await?;
    let rediscovery_while_submitted = fixture
        .client
        .upsert_rebalance_opportunity(signed_route_rediscovery_input.clone())
        .await?;
    let confirmer_after_submitted = fixture
        .client
        .lease_pending_signed_route_submissions(
            &submission_cluster,
            "confirmer-after-submitted-rediscovery",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .next()
        .ok_or("submitted signed fixture was not reclaimable")?;
    fixture
        .client
        .advance_signed_route_submission(
            &confirmer_after_submitted,
            SignedRouteSubmissionAdvance::ExpiryCheckPending {
                checked_at: Utc::now(),
                observed_block_height: 100_001,
                effect_check_slot: 50_000,
            },
        )
        .await?;
    let ambiguous_first = fixture
        .client
        .lease_reconciliation_pending_signed_route_submissions(
            &submission_cluster,
            "ambiguous-effect-check",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .next()
        .ok_or("expiry-check fixture was not reclaimable")?;
    let ambiguous = fixture
        .client
        .advance_signed_route_submission(
            &ambiguous_first,
            SignedRouteSubmissionAdvance::EffectAmbiguous {
                checked_at: Utc::now(),
                error_detail: "synthetic ambiguous post-expiry effect".to_owned(),
            },
        )
        .await?;
    let rediscovery_while_ambiguous = fixture
        .client
        .upsert_rebalance_opportunity(signed_route_rediscovery_input.clone())
        .await?;
    let (ambiguous_conflict_keys, _) = conflict_rows(&fixture.client, persisted.id).await?;
    let ambiguous_replacement_lease = claim_one(
        &fixture.client,
        &submission_cluster,
        "ambiguous-replacement-contender",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let ambiguous_replacement_rejected = fixture
        .client
        .acquire_route_account_conflict_leases(
            &ambiguous_replacement_lease,
            &[
                ambiguous_conflicts
                    .iter()
                    .find(|key| key.starts_with("vault-write:"))
                    .ok_or("ambiguous fixture has no retained vault conflict")?
                    .clone(),
                format!("fleet-shared-write-lane:{}:ambiguous-retry", fixture.prefix),
            ],
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await
        .is_err();
    let ambiguous_recovery = fixture
        .client
        .lease_reconciliation_pending_signed_route_submissions(
            &submission_cluster,
            "ambiguous-effect-recovery",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .next()
        .ok_or("ambiguous effect did not enter automatic recovery")?;
    let confirmed = fixture
        .client
        .advance_signed_route_submission(
            &ambiguous_recovery,
            SignedRouteSubmissionAdvance::Confirmed {
                checked_at: Utc::now(),
                confirmed_slot: 50_000,
            },
        )
        .await?;
    let mut recovery_handoff = ambiguous_recovery.clone();
    recovery_handoff.submission = confirmed;
    fixture
        .client
        .advance_signed_route_submission(
            &recovery_handoff,
            SignedRouteSubmissionAdvance::ReconciliationPending,
        )
        .await?;
    let (post_confirmation_conflict_keys, _) = conflict_rows(&fixture.client, persisted.id).await?;
    fixture
        .client
        .advance_decision(
            decision_id,
            DecisionAdvance::Confirm {
                slot: Some(50_000),
                post_snapshot_id: persisted.source_snapshot_id,
            },
        )
        .await?;

    let reconciler_first = fixture
        .client
        .lease_reconciliation_pending_signed_route_submissions(
            &submission_cluster,
            "reconciler-crashed",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .next()
        .ok_or("reconciler could not claim confirmed fixture")?;
    sqlx::query(
        "UPDATE loyal_yield.signed_route_submissions SET confirmation_lease_expires_at = now() - interval '1 second' WHERE id = $1",
    )
    .bind(persisted.id)
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        "UPDATE loyal_yield.route_account_conflict_leases SET expires_at = now() - interval '1 second' WHERE submission_id = $1",
    )
    .bind(persisted.id)
    .execute(fixture.client.pool())
    .await?;
    let reconciler_reclaimed = fixture
        .client
        .lease_reconciliation_pending_signed_route_submissions(
            &submission_cluster,
            "reconciler-replacement",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .next()
        .ok_or("replacement reconciler could not reclaim confirmed fixture")?;
    let (reconciler_keys, reconciler_min_expiry) =
        conflict_rows(&fixture.client, persisted.id).await?;
    let reconciler_renewed_exact_set = reconciler_keys == reconciliation_conflicts
        && reconciler_min_expiry.is_some_and(|expires_at| {
            expires_at >= reconciler_reclaimed.expires_at + chrono::Duration::minutes(2)
        });
    let reconciler_fenced = reconciler_reclaimed.fencing_token > reconciler_first.fencing_token;
    let reconciled_volume_before =
        reconciled_volume_for_cluster(fixture, &submission_cluster).await?;
    let reconciled_submission = fixture
        .client
        .advance_signed_route_submission(
            &reconciler_reclaimed,
            SignedRouteSubmissionAdvance::Reconciled {
                // Reconciliation freshness is later than the actual balance
                // movement; capacity must fence the confirmed slot below.
                reconciled_slot: 50_010,
            },
        )
        .await?;
    let reconciled_volume_after =
        reconciled_volume_for_cluster(fixture, &submission_cluster).await?;
    let replayed_reconciliation = fixture
        .client
        .advance_signed_route_submission(
            &reconciler_reclaimed,
            SignedRouteSubmissionAdvance::Reconciled {
                reconciled_slot: 50_010,
            },
        )
        .await;
    let reconciled_volume_after_replay =
        reconciled_volume_for_cluster(fixture, &submission_cluster).await?;
    let reconciled_volume_updates_exactly_once = reconciled_submission.state.as_str()
        == "reconciled"
        && reconciled_volume_after.0 == reconciled_volume_before.0 + 1
        && reconciled_volume_after.1 == reconciled_volume_before.1 + accrued_amount_raw
        && reconciled_volume_after.2
            == reconciled_volume_before.2 + signed_route_lease.opportunity.principal_usd_micros
        && replayed_reconciliation.is_err()
        && reconciled_volume_after_replay == reconciled_volume_after;
    let rediscovery_after_reconciled = fixture
        .client
        .upsert_rebalance_opportunity(signed_route_rediscovery_input.clone())
        .await?;
    let signed_route_rediscovery_attempt_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.rebalance_opportunities
        WHERE rediscovery_key = $1
        "#,
    )
    .bind(&signed_route_published.rediscovery_key)
    .fetch_one(fixture.client.pool())
    .await?;
    let signed_route_terminal_states_do_not_retry = submitted_route.state.as_str() == "submitted"
        && rediscovery_while_submitted.id == signed_route_published.id
        && ambiguous.state.as_str() == "effect_ambiguous"
        && rediscovery_while_ambiguous.id == signed_route_published.id
        && rediscovery_after_reconciled.id == signed_route_published.id
        && signed_route_rediscovery_attempt_count == 1;
    let (capacity_state_after_reconcile, capacity_movement_slot): (String, Option<i64>) =
        sqlx::query_as(
            r#"
            SELECT reservation_state, movement_slot
            FROM loyal_yield.target_capacity_reservations
            WHERE opportunity_id = $1
            "#,
        )
        .bind(persisted.opportunity_id)
        .fetch_one(fixture.client.pool())
        .await?;
    let signed_capacity_observation = TargetCapacityObservation {
        cluster: signed_route_lease.opportunity.cluster.clone(),
        target_reserve: signed_route_lease.opportunity.target_reserve.clone(),
        liquidity_mint: signed_route_lease.opportunity.liquidity_mint.clone(),
        observed_supply_usd_micros: 20_000_000_000,
        observed_slot: 50_000,
        maximum_inflight_usd_micros: 1_000_000_000,
    };
    let equal_slot_capacity_projection = fixture
        .client
        .observe_target_capacity(signed_capacity_observation.clone())
        .await?;
    let capacity_state_at_equal_slot: String = sqlx::query_scalar(
        "SELECT reservation_state FROM loyal_yield.target_capacity_reservations WHERE opportunity_id = $1",
    )
    .bind(persisted.opportunity_id)
    .fetch_one(fixture.client.pool())
    .await?;
    let crossed_slot_capacity_projection = fixture
        .client
        .observe_target_capacity(TargetCapacityObservation {
            observed_slot: 50_001,
            ..signed_capacity_observation
        })
        .await?;
    let capacity_state_after_crossed_slot: String = sqlx::query_scalar(
        "SELECT reservation_state FROM loyal_yield.target_capacity_reservations WHERE opportunity_id = $1",
    )
    .bind(persisted.opportunity_id)
    .fetch_one(fixture.client.pool())
    .await?;
    let reconciled_capacity_retention_passed = capacity_state_after_reconcile
        == "awaiting_telemetry"
        && capacity_movement_slot == Some(50_000)
        && equal_slot_capacity_projection.released_after_telemetry_count == 0
        && capacity_state_at_equal_slot == "awaiting_telemetry"
        && crossed_slot_capacity_projection.released_after_telemetry_count == 1
        && capacity_state_after_crossed_slot == "released";

    fixture
        .seed_opportunity(
            &submission_cluster,
            submission_epoch,
            "capacity-preobserved-reconcile",
            "ready",
            3_000,
        )
        .await?;
    let preobserved_lease = claim_one(
        &fixture.client,
        &submission_cluster,
        "capacity-preobserved-executor",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let preobserved_conflicts = vec![
        format!("fleet-shared-write-lane:{}:preobserved", fixture.prefix),
        format!(
            "vault-write:{}:{}",
            fixture.prefix,
            preobserved_lease.opportunity.vault_id.as_i64()
        ),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &preobserved_lease,
            &preobserved_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    let (_, preobserved_submission) = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            same_mint_input_for_lease(&preobserved_lease)?,
            &preobserved_lease,
            target_capacity_input_for_lease(fixture, &preobserved_lease).await?,
            signed_input_for_lease(
                fixture,
                &preobserved_lease,
                preobserved_conflicts,
                "capacity-preobserved-reconcile",
            )
            .await?,
        )
        .await?;
    let preobserved_decision_id = preobserved_submission
        .decision_id
        .ok_or("preobserved capacity fixture has no linked decision")?;
    let preobserved_target = TargetCapacityObservation {
        cluster: preobserved_lease.opportunity.cluster.clone(),
        target_reserve: preobserved_lease.opportunity.target_reserve.clone(),
        liquidity_mint: preobserved_lease.opportunity.liquidity_mint.clone(),
        observed_supply_usd_micros: 20_000_000_000,
        observed_slot: 60_001,
        maximum_inflight_usd_micros: 1_000_000_000,
    };
    fixture
        .client
        .observe_target_capacity(preobserved_target)
        .await?;
    fixture
        .client
        .ensure_signed_route_decision_confirming(&preobserved_submission)
        .await?;
    let preobserved_confirmation_lease = fixture
        .client
        .lease_pending_signed_route_submissions(
            &submission_cluster,
            "capacity-preobserved-confirmer",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .next()
        .ok_or("preobserved capacity fixture was not confirmable")?;
    let preobserved_confirmed = fixture
        .client
        .advance_signed_route_submission(
            &preobserved_confirmation_lease,
            SignedRouteSubmissionAdvance::Confirmed {
                checked_at: Utc::now(),
                confirmed_slot: 60_000,
            },
        )
        .await?;
    let mut preobserved_pending_handoff = preobserved_confirmation_lease.clone();
    preobserved_pending_handoff.submission = preobserved_confirmed;
    fixture
        .client
        .advance_signed_route_submission(
            &preobserved_pending_handoff,
            SignedRouteSubmissionAdvance::ReconciliationPending,
        )
        .await?;
    fixture
        .client
        .advance_decision(
            preobserved_decision_id,
            DecisionAdvance::Confirm {
                slot: Some(60_000),
                post_snapshot_id: preobserved_submission.source_snapshot_id,
            },
        )
        .await?;
    let preobserved_reconciliation_lease = fixture
        .client
        .lease_reconciliation_pending_signed_route_submissions(
            &submission_cluster,
            "capacity-preobserved-reconciler",
            1,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?
        .into_iter()
        .next()
        .ok_or("preobserved capacity fixture was not reconcilable")?;
    fixture
        .client
        .advance_signed_route_submission(
            &preobserved_reconciliation_lease,
            SignedRouteSubmissionAdvance::Reconciled {
                reconciled_slot: 60_010,
            },
        )
        .await?;
    let (preobserved_capacity_state, preobserved_release_reason): (String, Option<String>) =
        sqlx::query_as(
            r#"
            SELECT reservation_state, release_reason
            FROM loyal_yield.target_capacity_reservations
            WHERE opportunity_id = $1
            "#,
        )
        .bind(preobserved_lease.opportunity.id)
        .fetch_one(fixture.client.pool())
        .await?;
    let preexisting_newer_telemetry_releases_on_reconcile = preobserved_capacity_state
        == "released"
        && preobserved_release_reason.as_deref()
            == Some("target_telemetry_already_reflected_movement");
    let remaining_terminal_conflicts: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE submission_id = $1",
    )
    .bind(persisted.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let terminal_opportunity_state: String = sqlx::query_scalar(
        "SELECT opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(persisted.opportunity_id)
    .fetch_one(fixture.client.pool())
    .await?;

    // Semantic lanes bound database admission, but they are not evidence that
    // Solana transactions are physically independent. Seed two different
    // vaults with distinct semantic lanes and the same real payer/target keys,
    // then require the cluster-scoped health query to expose both hot keys.
    let physical_status_cluster = fixture.cluster("physical-writable-status");
    let physical_status_epoch = fixture.seed_epoch(&physical_status_cluster).await?;
    let first_physical_seed = fixture
        .seed_opportunity(
            &physical_status_cluster,
            physical_status_epoch,
            "physical-write-first",
            "ready",
            2_000,
        )
        .await?;
    let second_physical_seed = fixture
        .seed_opportunity(
            &physical_status_cluster,
            physical_status_epoch,
            "physical-write-second",
            "ready",
            1_000,
        )
        .await?;
    let shared_physical_target: String = sqlx::query_scalar(
        "SELECT target_reserve FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(first_physical_seed.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let second_original_target: String = sqlx::query_scalar(
        "SELECT target_reserve FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(second_physical_seed.id)
    .fetch_one(fixture.client.pool())
    .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.vault_position_snapshot_positions position
        SET reserve = $2
        FROM loyal_yield.rebalance_opportunities opportunity
        WHERE opportunity.id = $1
          AND position.snapshot_id = opportunity.source_snapshot_id
          AND position.reserve = $3
        "#,
    )
    .bind(second_physical_seed.id)
    .bind(&shared_physical_target)
    .bind(&second_original_target)
    .execute(fixture.client.pool())
    .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.vault_reserve_positions_current position
        SET reserve = $2
        FROM loyal_yield.rebalance_opportunities opportunity
        WHERE opportunity.id = $1
          AND position.vault_id = opportunity.vault_id
          AND position.reserve = $3
        "#,
    )
    .bind(second_physical_seed.id)
    .bind(&shared_physical_target)
    .bind(&second_original_target)
    .execute(fixture.client.pool())
    .await?;
    sqlx::query("UPDATE loyal_yield.rebalance_opportunities SET target_reserve = $2 WHERE id = $1")
        .bind(second_physical_seed.id)
        .bind(&shared_physical_target)
        .execute(fixture.client.pool())
        .await?;
    for index in 0..2 {
        let lease = claim_one(
            &fixture.client,
            &physical_status_cluster,
            &format!("physical-write-executor-{index}"),
            RebalanceOpportunityClaimKind::Execute,
        )
        .await?;
        let semantic_conflicts = vec![
            format!(
                "fleet-shared-write-lane:{}:physical-{index}",
                fixture.prefix
            ),
            format!(
                "vault-write:{}:physical:{}",
                fixture.prefix,
                lease.opportunity.vault_id.as_i64()
            ),
        ];
        fixture
            .client
            .acquire_route_account_conflict_leases(
                &lease,
                &semantic_conflicts,
                Utc::now() + chrono::Duration::minutes(4),
            )
            .await?;
        let mut signed_input = signed_input_for_lease(
            fixture,
            &lease,
            semantic_conflicts,
            &format!("physical-write-{index}"),
        )
        .await?;
        signed_input
            .writable_account_keys
            .push(shared_physical_target.clone());
        signed_input.writable_account_keys.sort_unstable();
        signed_input.writable_account_keys.dedup();
        fixture
            .client
            .prepare_same_mint_rebalance_with_signed_submission(
                same_mint_input_for_lease(&lease)?,
                &lease,
                target_capacity_input_for_lease(fixture, &lease).await?,
                signed_input,
            )
            .await?;
    }
    let physical_status = fixture
        .client
        .fleet_orchestration_status_source(&physical_status_cluster)
        .await?;
    let physical_status_row = physical_status
        .first()
        .ok_or("physical writable-key status fixture returned no health row")?;
    let shared_physical_payer = format!("authority:{physical_status_cluster}");
    let physical_payer_congestion = physical_status_row
        .top_physical_writable_key_congestion
        .iter()
        .find(|entry| entry.writable_account_key == shared_physical_payer);
    let physical_target_congestion = physical_status_row
        .top_physical_writable_key_congestion
        .iter()
        .find(|entry| entry.writable_account_key == shared_physical_target);
    let physical_writable_key_congestion_visible =
        physical_status_row.active_physical_writable_key_count >= 2
            && physical_payer_congestion.is_some_and(|entry| {
                entry.classification == "payer"
                    && entry.active_submission_count == 2
                    && entry.principal_usd_micros == 200_000_000
                    && entry.recoverable_yield_usd_micros_per_hour > 0
            })
            && physical_target_congestion.is_some_and(|entry| {
                entry.classification == "target"
                    && entry.active_submission_count == 2
                    && entry.principal_usd_micros == 200_000_000
                    && entry.recoverable_yield_usd_micros_per_hour > 0
            });

    let duplicate_active_vault_movements: i64 = sqlx::query_scalar(
        r#"
        SELECT COALESCE(sum(active_count - 1), 0)::BIGINT
        FROM (
            SELECT vault_id, count(*)::BIGINT AS active_count
            FROM loyal_yield.rebalance_opportunities
            WHERE cluster = $1 AND opportunity_state = 'leased'
              AND lease_kind = 'execute' AND lease_expires_at > now()
            GROUP BY vault_id
            HAVING count(*) > 1
        ) duplicate_vault
        "#,
    )
    .bind(&conflict_cluster)
    .fetch_one(fixture.client.pool())
    .await?;
    let fleet_wide_exclusive_route_leases: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.route_account_conflict_leases conflict
        JOIN loyal_yield.rebalance_opportunities opportunity
          ON opportunity.id = conflict.opportunity_id
        WHERE opportunity.cluster = $1 AND conflict.expires_at > now()
          AND conflict.writable_account_key NOT LIKE $2
          AND conflict.writable_account_key NOT LIKE $3
        "#,
    )
    .bind(&conflict_cluster)
    .bind(format!("vault-write:{}:%", fixture.prefix))
    .bind(format!("fleet-shared-write-lane:{}:%", fixture.prefix))
    .fetch_one(fixture.client.pool())
    .await?;
    let fee_shard_registry_rows: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.route_fee_payer_shards
        WHERE cluster = $1 AND fee_payer = $2 AND enabled
        "#,
    )
    .bind(&fee_floor_cluster)
    .bind(&fee_floor_payer)
    .fetch_one(fixture.client.pool())
    .await?;
    let fee_shard_alt_authority_rows: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.lookup_table_families
        WHERE provisioning_authority = $1 OR payer = $1
        "#,
    )
    .bind(&fee_floor_payer)
    .fetch_one(fixture.client.pool())
    .await?;
    let reciprocal_authority_separation = fee_shard_registry_rows == 1
        && fee_shard_alt_authority_rows == 0
        && fee_floor_payer != alt_runtime_measurements.policy_pubkey;
    let replacement_before_expiry_and_absence_proof: i64 = sqlx::query_scalar(
        r#"
        SELECT (CASE WHEN opportunity.decision_id IS NULL THEN 0 ELSE 1 END)
             + (SELECT count(*) FROM loyal_yield.signed_route_submissions submission
                WHERE submission.opportunity_id = opportunity.id)
        FROM loyal_yield.rebalance_opportunities opportunity
        WHERE opportunity.id = $1
        "#,
    )
    .bind(thief_lease.opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let ambiguous_or_stale_replacement_movements: i64 = sqlx::query_scalar(
        r#"
        SELECT (CASE WHEN opportunity.decision_id IS NULL THEN 0 ELSE 1 END)
             + (SELECT count(*) FROM loyal_yield.signed_route_submissions submission
                WHERE submission.opportunity_id = opportunity.id)
        FROM loyal_yield.rebalance_opportunities opportunity
        WHERE opportunity.id = $1
        "#,
    )
    .bind(ambiguous_replacement_lease.opportunity.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let database_deadlocks_after: i64 = sqlx::query_scalar(
        "SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()",
    )
    .fetch_one(fixture.client.pool())
    .await?;
    let database_deadlocks = database_deadlocks_after.saturating_sub(database_deadlocks_before);

    let affected_jobs_promoted =
        i64::from(affected_after_readmission.state == RebalanceOpportunityState::Revalidate);
    let unaffected_jobs_promoted = i64::from(unaffected_wakeup_state != "waiting_alt");
    let additional_fleet_cycle_required = !(affected_outbox_count == 1
        && acknowledged_alt_wakeups == 1
        && pending_affected_outbox_count == 0
        && affected_jobs_promoted == 1);
    let overlapping_lane_limit_violations =
        i64::from(!same_vault_rejected) + i64::from(!same_lane_rejected);
    let expired_lease_reclaimed_with_higher_fence = reclaimed.opportunity.id == reclaim_seed.id
        && reclaimed.fencing_token > first_lease.fencing_token
        && mixed_lane_global_order_preserved;

    let poison_cluster = fixture.cluster("signed_alt_poison_isolation");
    let poison_epoch = fixture.seed_epoch(&poison_cluster).await?;
    let poison_submission = prepare_signed_submission_fixture(
        fixture,
        &poison_cluster,
        poison_epoch,
        "poison-alt-row",
        3_000,
        10,
    )
    .await?;
    let valid_submission = prepare_signed_submission_fixture(
        fixture,
        &poison_cluster,
        poison_epoch,
        "valid-alt-row",
        2_000,
        100_000,
    )
    .await?;
    sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_usage_leases
        SET created_at = now() - interval '1 hour',
            expires_at = now() - interval '1 second', updated_at = now()
        WHERE lease_kind = 'prepared_transaction' AND reference_key = $1
        "#,
    )
    .bind(&poison_submission.semantic_key)
    .execute(fixture.client.pool())
    .await?;
    let normal_after_poison = fixture
        .client
        .lease_pending_signed_route_submissions(
            &poison_cluster,
            "poison-normal-confirmer",
            2,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?;
    let normal_after_poison_ids = normal_after_poison
        .iter()
        .map(|lease| lease.submission.id)
        .collect::<Vec<_>>();
    let recovery_after_poison = fixture
        .client
        .lease_unprotected_unbroadcast_signed_route_submissions(
            &poison_cluster,
            "poison-recovery-confirmer",
            2,
            Utc::now() + chrono::Duration::minutes(2),
        )
        .await?;
    let poison_recovery_lease = recovery_after_poison
        .iter()
        .find(|lease| lease.submission.id == poison_submission.id)
        .cloned()
        .ok_or("poisoned signed route did not enter terminal-only recovery")?;
    let expired_poison = fixture
        .client
        .advance_signed_route_submission(
            &poison_recovery_lease,
            SignedRouteSubmissionAdvance::Expired {
                checked_at: Utc::now(),
                observed_block_height: 11,
                signature_history_absent: true,
                effect_absence_proved: false,
            },
        )
        .await?;
    let poison_terminal_conflicts: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.route_account_conflict_leases WHERE submission_id = $1",
    )
    .bind(poison_submission.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let poison_active_alt_leases: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.lookup_table_usage_leases
        WHERE lease_kind = 'prepared_transaction' AND reference_key = $1
          AND released_at IS NULL
        "#,
    )
    .bind(&poison_submission.semantic_key)
    .fetch_one(fixture.client.pool())
    .await?;
    let poison_capacity_state: String = sqlx::query_scalar(
        "SELECT reservation_state FROM loyal_yield.target_capacity_reservations WHERE signed_submission_id = $1",
    )
    .bind(poison_submission.id)
    .fetch_one(fixture.client.pool())
    .await?;
    let (poison_opportunity_state, poison_decision_state): (String, String) = sqlx::query_as(
        r#"
        SELECT opportunity.opportunity_state, decision.status::text
        FROM loyal_yield.rebalance_opportunities opportunity
        JOIN loyal_yield.rebalance_decisions decision ON decision.id = opportunity.decision_id
        WHERE opportunity.id = $1
        "#,
    )
    .bind(poison_submission.opportunity_id)
    .fetch_one(fixture.client.pool())
    .await?;
    let valid_normal_lease = normal_after_poison
        .iter()
        .find(|lease| lease.submission.id == valid_submission.id)
        .ok_or("valid signed route was not independently claimable")?;
    fixture
        .client
        .advance_signed_route_submission(
            valid_normal_lease,
            SignedRouteSubmissionAdvance::Failed {
                checked_at: Utc::now(),
                confirmed_slot: None,
                error_detail: "synthetic verifier cleanup after poison isolation".to_owned(),
            },
        )
        .await?;
    let poison_row_isolated_and_recovered = normal_after_poison_ids == vec![valid_submission.id]
        && recovery_after_poison.len() == 1
        && expired_poison.state.as_str() == "expired"
        && poison_terminal_conflicts == 0
        && poison_active_alt_leases == 0
        && poison_capacity_state == "released"
        && poison_opportunity_state == "failed"
        && poison_decision_state == "failed";
    let cross_mint_movement_checks = cross_mint_movement_subchecks(
        fixture,
        decision_linked
            && confirming_state_after_explicit_transition == "confirming"
            && terminal_opportunity_state == "completed"
            && signed_evidence_immutable
            && fee_payer_kind_immutable
            && submission_state_timestamp_immutable,
        json!({
            "routeKind": "same_mint",
            "submissionId": persisted.id,
            "decisionId": decision_id.as_i64(),
            "decisionLinked": decision_linked,
            "stateAfterExplicitConfirmingTransition": confirming_state_after_explicit_transition,
            "terminalOpportunityState": terminal_opportunity_state,
            "signedBytesImmutable": signed_evidence_immutable,
            "feePayerKindImmutable": fee_payer_kind_immutable,
            "submissionStateTimestampImmutable": submission_state_timestamp_immutable,
        }),
    )
    .await?;
    let runtime_measurements_passed = alt_runtime_measurements.typed_provisioner_dry_run_plans > 0
        && alt_runtime_measurements.reusable_v2_plans
            == alt_runtime_measurements.typed_provisioner_dry_run_plans
        && alt_runtime_measurements.legacy_or_exact_route_alt_plans == 0
        && ready_jobs_claimed == i64::try_from(READY_JOBS_SEEDED)?
        && waiting_alt_jobs == 10_000
        && waiting_alt_decisions == 0
        && cold_backlog_effect_ppm < 50_000
        && claim_partial_index_predicates_are_lane_exact
        && claim_index_reads_are_bounded
        && affected_outbox_count == 1
        && affected_jobs_promoted == 1
        && unaffected_jobs_promoted == 0
        && !additional_fleet_cycle_required
        && alt_runtime_measurements.normal_readiness_global_rollout_lock_acquisitions == 0
        && alt_runtime_measurements.independent_physical_alt_lanes_progressed >= 2
        && alt_runtime_measurements.same_table_predecessor_violations == 0
        && alt_runtime_measurements.stale_fence_commits == 0
        && alt_runtime_measurements.stale_fence_rejections == 1
        && alt_runtime_measurements.usage_leases_rejected_during_mutation == 1
        && alt_runtime_measurements.mutating_operations_leased_during_usage == 0
        && alt_runtime_measurements.verify_operations_leased_during_usage == 1
        && alt_runtime_measurements.usage_fence_broadcast_commits == 0
        && alt_runtime_measurements.usage_fence_broadcast_rejections == 1
        && duplicate_active_vault_movements == 0
        && independent_count == 64
        && overlapping_lane_limit_violations == 0
        && physical_writable_key_congestion_visible
        && expired_lease_reclaimed_with_higher_fence
        && mixed_concurrent_claims_are_full_disjoint_and_priority_ordered
        && fleet_wide_exclusive_route_leases == 0
        && attached_conflicts_not_stolen
        && replacement_before_expiry_and_absence_proof == 0
        && ambiguous_replacement_rejected
        && ambiguous_or_stale_replacement_movements == 0
        && alt_runtime_measurements.alt_authority_payer_identity_consistent
        && reciprocal_authority_separation
        && fee_floor_admission_passed
        && policy_setup_funding_reservations_passed
        && fleet_latency_speedup_passed
        && target_capacity_concurrency_passed
        && pre_send_terminal_failure_released_capacity
        && reconciled_capacity_retention_passed
        && preexisting_newer_telemetry_releases_on_reconcile
        && poison_row_isolated_and_recovered
        && bounded_accrual_preserves_discovery_and_binds_signed_decision
        && published_economics_bind_signed_decision
        && reconciled_volume_updates_exactly_once
        && readiness_writers_waited
        && readiness_parent_before_physical_lock_order_proved
        && serialized_readiness_row_count == 2
        && database_deadlocks == 0;

    Ok(DatabaseEvidence {
        migration_subchecks: vec![
            subcheck(
                "isolated_database_migrated_through_37",
                true,
                json!({
                    "databaseNameGuard": "fleet_verify",
                    "migration23": "value_priority_rebalance_queue",
                    "migration24": "fleet_route_confirmer",
                    "migration25": "fee_only_route_payer_shards",
                    "migration26": "target_capacity_reservations",
                    "migration27": "rebalance_opportunity_attempt_generations",
                    "migration28": "reusable_alt_terminal_repair",
                    "migration29": "fleet_commit_lifetime_fences",
                    "migration30": "fused_queue_accrual_binding",
                    "migration31": "fleet_commit_lifetime_fence_errcode",
                    "migration32": "idle_vault_decision_lookup_index",
                    "migration33": "policy_setup_funding_reservations",
                    "migration34": "fleet_health_snapshot_projection",
                    "migration35": "durable_cross_mint_movements",
                    "migration36": "cross_mint_swap_policies",
                    "migration37": "cross_mint_vault_opt_ins",
                }),
            ),
            subcheck(
                "empty_registered_cluster_exposes_planner_and_market_health",
                empty_status_visible,
                json!({
                    "cluster": empty_status_cluster,
                    "rowCount": empty_status_rows.len(),
                    "opportunityState": empty_status_rows.first().and_then(|row| row.opportunity_state.as_deref()),
                    "latestMarketEpochId": empty_status_rows.first().and_then(|row| row.latest_market_epoch_id),
                    "plannerLastSeenAgeSeconds": empty_status_rows.first().and_then(|row| row.planner_last_seen_age_seconds),
                    "marketEpochExpired": empty_status_rows.first().and_then(|row| row.latest_market_epoch_expired),
                    "senderSubmissionCount": empty_status_rows.first().map(|row| row.sender_submission_count),
                    "confirmerSubmissionCount": empty_status_rows.first().map(|row| row.confirmer_submission_count),
                    "reconcilerSubmissionCount": empty_status_rows.first().map(|row| row.reconciler_submission_count),
                }),
            ),
        ],
        discovery_subchecks: vec![
            subcheck(
                "database_economic_priority_beats_id",
                high_claim.opportunity.id == high.id
                    && high.id > low.id
                    && high_claim.opportunity.economic_priority > low.economic_priority,
                json!({
                    "lowerId": low.id,
                    "claimedId": high_claim.opportunity.id,
                    "claimedPriority": high_claim.opportunity.economic_priority,
                }),
            ),
            subcheck(
                "concurrent_skip_locked_claims_are_independent",
                skip_locked_passed,
                json!({"claimedIds": concurrent_ids}),
            ),
            subcheck(
                "batch_claim_uses_gradual_age_boost_without_categorical_old_backlog_preemption",
                batch_gradual_age_boost_and_priority_ordered,
                json!({
                    "claimedIds": batch_claim_ids,
                    "expectedIds": [starved.id, batch_high.id, batch_medium.id, young_low.id],
                    "batchSize": batch_claims.len(),
                }),
            ),
        ],
        alt_subchecks: vec![
            subcheck(
                "runtime_alt_and_db_execution_measurements",
                runtime_measurements_passed,
                json!({
                    "schemaVersion": 1,
                    "event": "fleet_isolated_database_runtime_measurements",
                    "alt": {
                        "typedProvisionerDryRunPlans": alt_runtime_measurements.typed_provisioner_dry_run_plans,
                        "reusableV2Plans": alt_runtime_measurements.reusable_v2_plans,
                        "legacyOrExactRouteAltPlans": alt_runtime_measurements.legacy_or_exact_route_alt_plans,
                        "readyJobsSeeded": READY_JOBS_SEEDED,
                        "readyJobsClaimed": ready_jobs_claimed,
                        "waitingAltJobs": waiting_alt_jobs,
                        "waitingAltDecisions": waiting_alt_decisions,
                        "claimLatencyGateClock": "postgres_statement_elapsed",
                        "readyClaimBaselineP95Micros": baseline_claim_p95_micros,
                        "readyClaimColdP95Micros": cold_claim_p95_micros,
                        "readyClaimBaselineClientP95Micros": baseline_client_claim_p95_micros,
                        "readyClaimColdClientP95Micros": cold_client_claim_p95_micros,
                        "durableCoverageWakeupRows": affected_outbox_count,
                        "affectedJobsPromoted": affected_jobs_promoted,
                        "unaffectedJobsPromoted": unaffected_jobs_promoted,
                        "additionalFleetCycleRequired": additional_fleet_cycle_required,
                        "normalReadinessGlobalRolloutLockAcquisitions": alt_runtime_measurements.normal_readiness_global_rollout_lock_acquisitions,
                        "independentPhysicalAltLanesProgressed": alt_runtime_measurements.independent_physical_alt_lanes_progressed,
                        "sameTablePredecessorViolations": alt_runtime_measurements.same_table_predecessor_violations,
                        "staleFenceCommits": alt_runtime_measurements.stale_fence_commits,
                        "usageLeasesRejectedDuringMutation": alt_runtime_measurements.usage_leases_rejected_during_mutation,
                        "mutatingOperationsLeasedDuringUsage": alt_runtime_measurements.mutating_operations_leased_during_usage,
                        "verifyOperationsLeasedDuringUsage": alt_runtime_measurements.verify_operations_leased_during_usage,
                        "usageFenceBroadcastCommits": alt_runtime_measurements.usage_fence_broadcast_commits,
                        "usageFenceBroadcastRejections": alt_runtime_measurements.usage_fence_broadcast_rejections,
                    },
                    "execution": {
                        "duplicateActiveVaultMovements": duplicate_active_vault_movements,
                        "nonoverlappingConcurrentLeases": independent_count,
                        "overlappingLaneLimitViolations": overlapping_lane_limit_violations,
                        "physicalWritableKeyCongestionVisible": physical_writable_key_congestion_visible,
                        "expiredLeaseReclaimedWithHigherFence": expired_lease_reclaimed_with_higher_fence,
                        "mixedRunnableAndExpiredClaimsFullAndDisjoint": mixed_concurrent_claims_are_full_disjoint_and_priority_ordered,
                        "fleetWideExclusiveRouteLeases": fleet_wide_exclusive_route_leases,
                        "replacementBeforeExpiryAndAbsenceProof": replacement_before_expiry_and_absence_proof,
                        "ambiguousOrStaleReplacementMovements": ambiguous_or_stale_replacement_movements,
                        "reciprocalAuthoritySeparation": reciprocal_authority_separation,
                        "lowBalanceLimitsEnforced": fee_floor_admission_passed,
                        "atomicImmutableSpendReservation": fee_floor_admission_passed && fee_floor_reservations == 2,
                        "policySetupFundingReservationBounded": policy_setup_funding_reservations_passed,
                        "policySetupFundingReservationRows": setup_funding_reservation_count,
                        "targetCapacityConcurrentAdmissionBounded": target_capacity_concurrency_passed,
                        "preSendTargetCapacityReleased": pre_send_terminal_failure_released_capacity,
                        "reconciledCapacityStrictTelemetryFence": reconciled_capacity_retention_passed,
                        "preexistingNewerTelemetryRelease": preexisting_newer_telemetry_releases_on_reconcile,
                        "readinessWritersWaitedOnPerVaultFence": readiness_writers_waited,
                        "readinessParentBeforePhysicalLockOrderProved": readiness_parent_before_physical_lock_order_proved,
                        "serializedReadinessRowCount": serialized_readiness_row_count,
                        "databaseDeadlocks": database_deadlocks,
                    },
                    "latencyLoad": fleet_latency_load.clone(),
                }),
            ),
            subcheck(
                "readiness_parent_lock_precedes_physical_alt_lock",
                readiness_parent_before_physical_lock_order_proved && database_deadlocks == 0,
                json!({
                    "readinessWaitedOnLogicalParent": readiness_waited_on_parent,
                    "lifecycleAcquiredPhysicalWhileReadinessWaited": lifecycle_acquired_physical_while_readiness_waited,
                    "databaseDeadlocks": database_deadlocks,
                }),
            ),
            subcheck(
                "per_vault_readiness_writes_serialize_without_database_deadlocks",
                readiness_writers_waited
                    && serialized_readiness_row_count == 2
                    && database_deadlocks == 0,
                json!({
                    "writersWaitedOnPerVaultFence": readiness_writers_waited,
                    "committedReadinessRows": serialized_readiness_row_count,
                    "databaseDeadlocks": database_deadlocks,
                }),
            ),
            subcheck(
                "route_usage_and_alt_mutation_are_reciprocally_fenced",
                alt_runtime_measurements.usage_leases_rejected_during_mutation == 1
                    && alt_runtime_measurements.mutating_operations_leased_during_usage == 0
                    && alt_runtime_measurements.verify_operations_leased_during_usage == 1
                    && alt_runtime_measurements.usage_fence_broadcast_commits == 0
                    && alt_runtime_measurements.usage_fence_broadcast_rejections == 1,
                json!({
                    "usageLeasesRejectedDuringMutation": alt_runtime_measurements.usage_leases_rejected_during_mutation,
                    "mutatingOperationsLeasedDuringUsage": alt_runtime_measurements.mutating_operations_leased_during_usage,
                    "verifyOperationsLeasedDuringUsage": alt_runtime_measurements.verify_operations_leased_during_usage,
                    "usageFenceBroadcastCommits": alt_runtime_measurements.usage_fence_broadcast_commits,
                    "usageFenceBroadcastRejections": alt_runtime_measurements.usage_fence_broadcast_rejections,
                }),
            ),
            subcheck(
                "invalid_alt_row_isolated_and_terminally_recovered_without_broadcast",
                poison_row_isolated_and_recovered,
                json!({
                    "normalClaimIds": normal_after_poison_ids,
                    "validSubmissionId": valid_submission.id,
                    "recoveryClaimIds": recovery_after_poison.iter().map(|lease| lease.submission.id).collect::<Vec<_>>(),
                    "poisonSubmissionId": poison_submission.id,
                    "poisonTerminalState": expired_poison.state.as_str(),
                    "remainingConflictRows": poison_terminal_conflicts,
                    "activeAltUsageLeases": poison_active_alt_leases,
                    "capacityState": poison_capacity_state,
                    "opportunityState": poison_opportunity_state,
                    "decisionState": poison_decision_state,
                }),
            ),
            subcheck(
                "bounded_accrual_preserves_discovery_and_binds_signed_decision",
                bounded_accrual_preserves_discovery_and_binds_signed_decision,
                json!({
                    "publishedAmountRaw": published_amount_raw,
                    "refreshedAmountRaw": accrued_amount_raw,
                    "durableOpportunityAmountRaw": immutable_opportunity.amount_raw,
                    "durableDecisionAmountRaw": durable_decision_amount_raw,
                    "durableDecisionExecutionPlan": durable_decision_plan,
                }),
            ),
            subcheck(
                "fresh_economics_revalidate_but_published_economics_bind_decision",
                published_economics_bind_signed_decision,
                json!({
                    "refreshedEconomicsRejectedAtDecisionBinding": refreshed_economics_rejected,
                    "publishedSourceApyBps": signed_route_lease.opportunity.source_apy_bps,
                    "publishedTargetApyBps": signed_route_lease.opportunity.target_apy_bps,
                    "publishedEstimatedEdgeBps": signed_route_lease.opportunity.estimated_edge_bps,
                    "durableDecisionSourceApyBps": durable_decision_source_apy_bps,
                    "durableDecisionTargetApyBps": durable_decision_target_apy_bps,
                    "durableDecisionEstimatedEdgeBps": durable_decision_estimated_edge_bps,
                    "durableDecisionExecutionPlan": durable_decision_plan,
                }),
            ),
            subcheck(
                "reconciled_volume_counts_unique_submission_exactly_once",
                reconciled_volume_updates_exactly_once,
                json!({
                    "before": {
                        "submissionCount": reconciled_volume_before.0,
                        "amountRaw": reconciled_volume_before.1,
                        "principalUsdMicros": reconciled_volume_before.2,
                    },
                    "after": {
                        "submissionCount": reconciled_volume_after.0,
                        "amountRaw": reconciled_volume_after.1,
                        "principalUsdMicros": reconciled_volume_after.2,
                    },
                    "afterReplay": {
                        "submissionCount": reconciled_volume_after_replay.0,
                        "amountRaw": reconciled_volume_after_replay.1,
                        "principalUsdMicros": reconciled_volume_after_replay.2,
                    },
                    "replayRejected": replayed_reconciliation.is_err(),
                }),
            ),
            subcheck(
                "stale_physical_alt_fence_is_rejected_before_commit",
                alt_runtime_measurements.stale_fence_rejections == 1
                    && alt_runtime_measurements.stale_fence_commits == 0,
                json!({
                    "rejectedStaleFenceAttempts": alt_runtime_measurements.stale_fence_rejections,
                    "committedStaleFences": alt_runtime_measurements.stale_fence_commits,
                }),
            ),
            subcheck(
                "ready_revalidate_waiting_lanes_are_isolated",
                high_claim.claim_kind == RebalanceOpportunityClaimKind::Execute
                    && revalidate_claim.opportunity.id == revalidate.id
                    && revalidate_claim.claim_kind == RebalanceOpportunityClaimKind::Revalidate
                    && waiting_state == "waiting_alt"
                    && low_state == "ready",
                json!({
                    "executeClaimedState": high_claim.opportunity.state.as_str(),
                    "revalidateClaimedId": revalidate_claim.opportunity.id,
                    "waitingState": waiting_state,
                    "unclaimedReadyState": low_state,
                }),
            ),
            subcheck(
                "ten_thousand_alt_cold_jobs_change_ready_claim_p95_by_under_five_percent",
                cold_backlog_effect_ppm < 50_000,
                json!({
                    "readyClaimsPerRound": 64,
                    "rounds": 63,
                    "waitingAltCount": 10_000,
                    "gateClock": "postgres_statement_elapsed",
                    "baselineP95Micros": baseline_claim_p95_micros,
                    "coldP95Micros": cold_claim_p95_micros,
                    "baselineClientP95Micros": baseline_client_claim_p95_micros,
                    "coldClientP95Micros": cold_client_claim_p95_micros,
                    "coldBacklogEffectPpm": cold_backlog_effect_ppm,
                    "limitPpm": 50_000,
                    "timedReadyRowsClaimed": timed_ready_rows_claimed,
                    "runnableIndexTupleReads": runnable_index_tuple_reads,
                    "expiredIndexTupleReads": expired_index_tuple_reads,
                    "expectedRankedLaneReads": expected_ranked_lane_reads,
                    "expectedRunnableSelfChurnReads": expected_runnable_self_churn_reads,
                    "runnableSelfChurnCeilingReads": runnable_self_churn_ceiling_reads,
                    "waitingAltFullScanRegressionReads": waiting_alt_full_scan_regression_reads,
                    "indexTupleReadsBounded": claim_index_reads_are_bounded,
                    "baselineSamplesMicros": baseline_latency_samples,
                    "coldSamplesMicros": cold_latency_samples,
                    "baselineClientSamplesMicros": baseline_client_latency_samples,
                    "coldClientSamplesMicros": cold_client_latency_samples,
                }),
            ),
            subcheck(
                "claim_partial_indexes_exclude_waiting_alt_and_separate_active_leases",
                claim_partial_index_predicates_are_lane_exact,
                json!({
                    "runnableIndexDefinition": runnable_index_definition,
                    "expiredRecoveryIndexDefinition": expired_index_definition,
                    "runnableIndexExcludesWaitingAlt": !runnable_index_definition.contains("'waiting_alt'::text"),
                    "runnableIndexExcludesActiveLeased": !runnable_index_definition.contains("'leased'::text"),
                    "expiredRecoveryLaneIsLeaseExpiryKeyed": expired_index_definition.contains("(cluster, lease_kind, lease_expires_at, id)"),
                }),
            ),
            subcheck(
                "alt_coverage_requires_current_planner_readmission",
                waiting_decision_count == 0
                    && affected_before_readmission_state == "waiting_alt"
                    && affected_after_readmission.state.as_str() == "revalidate"
                    && unaffected_wakeup_state == "waiting_alt"
                    && affected_outbox_count == 1
                    && acknowledged_alt_wakeups == 1
                    && pending_affected_outbox_count == 0,
                json!({
                    "waitingDecisionCountBeforeWake": waiting_decision_count,
                    "affectedOpportunityId": affected_waiting.id,
                    "stateAfterCoverage": affected_before_readmission_state,
                    "stateAfterCurrentPlannerReadmission": affected_after_readmission.state.as_str(),
                    "unaffectedOpportunityId": unaffected_waiting.id,
                    "unaffectedState": unaffected_wakeup_state,
                    "durableOutboxRows": affected_outbox_count,
                    "acknowledgedOutboxRows": acknowledged_alt_wakeups,
                    "pendingOutboxRows": pending_affected_outbox_count,
                }),
            ),
        ],
        execution_subchecks: {
            let mut checks = vec![
            subcheck(
                "active_slot_conflict_is_contained",
                active_slot_conflict_is_contained,
                json!({
                    "directWriterUpdatedRows": active_slot_direct_write_count,
                    "publicationWaitObserved": active_slot_publication_wait_observed,
                    "typedDeferral": active_slot_typed_deferral,
                    "expectedVaultId": active_slot_owner.vault_id.as_i64(),
                    "returnedVaultId": active_slot_returned_vault_id,
                    "expectedSlotOpportunityId": active_slot_seed.id,
                    "returnedSlotOpportunityId": active_slot_returned_opportunity_id,
                    "returnedSlotOpportunityState": active_slot_returned_state,
                    "returnedReason": active_slot_returned_reason,
                    "opportunityRows": active_slot_opportunity_rows,
                    "activeSlotRows": active_slot_rows,
                    "rawResult": format!("{active_slot_publish_result:?}"),
                }),
            ),
            subcheck(
                "expired_opportunity_lease_reclaimed_with_higher_fence",
                reclaimed.opportunity.id == reclaim_seed.id
                    && reclaimed.fencing_token > first_lease.fencing_token
                    && mixed_lane_global_order_preserved,
                json!({
                    "firstFence": first_lease.fencing_token,
                    "reclaimedFence": reclaimed.fencing_token,
                    "orderedBatchIds": reclaimed_batch.iter().map(|lease| lease.opportunity.id).collect::<Vec<_>>(),
                    "expectedExpiredThenRunnableIds": [reclaim_seed.id, reclaim_ready_seed.id],
                }),
            ),
            subcheck(
                "mixed_runnable_and_expired_batches_are_globally_ordered_and_skip_locked",
                mixed_concurrent_claims_are_full_disjoint_and_priority_ordered,
                json!({
                    "initialExpiredLeaseCount": initially_leased_mixed.len(),
                    "firstConcurrentBatchIds": mixed_claim_a_ids,
                    "secondConcurrentBatchIds": mixed_claim_b_ids,
                    "expectedExpiredIds": mixed_expired_ids,
                    "expectedRunnableIds": mixed_runnable_ids,
                    "combinedConcurrentIds": mixed_concurrent_ids,
                }),
            ),
            subcheck(
                "pre_decision_failure_releases_conflicts_and_retries_immediately",
                retry_reclaimed.opportunity.id == retry_seed.id
                    && retry_reclaimed.fencing_token > retry_first.fencing_token
                    && retry_conflicts_after_release == 0,
                json!({
                    "firstFence": retry_first.fencing_token,
                    "reclaimedFence": retry_reclaimed.fencing_token,
                    "unattachedConflictRowsAfterRetry": retry_conflicts_after_release,
                }),
            ),
            subcheck(
                "expired_effect_free_retry_advance_is_classified_without_reopening_stale_work",
                expired_retry_advance_classified,
                json!({
                    "opportunityId": expired_retry_seed.id,
                    "sweptRows": expired_retry_swept,
                    "outcome": format!("{expired_retry_outcome:?}"),
                    "durableState": expired_retry_current.state.as_str(),
                    "terminalReason": expired_retry_current.terminal_reason,
                    "decisionId": expired_retry_current.decision_id.map(|id| id.as_i64()),
                }),
            ),
            subcheck(
                "mismatched_live_lease_advance_is_fenced_without_mutation",
                fenced_advance_classified,
                json!({
                    "opportunityId": fenced_advance_seed.id,
                    "outcome": format!("{fenced_advance_outcome:?}"),
                    "durableState": fenced_advance_current.state.as_str(),
                    "durableOwner": fenced_advance_current.lease_owner,
                    "durableFence": fenced_advance_current.fencing_token,
                    "staleOwner": stale_fenced_advance_lease.owner,
                    "staleFence": stale_fenced_advance_lease.fencing_token,
                }),
            ),
            subcheck(
                "terminal_no_effect_rediscovery_creates_one_concurrency_safe_retry_generation",
                retry_generation_concurrency_safe,
                json!({
                    "rediscoveryKey": retry_generation_first.rediscovery_key,
                    "attemptIds": retry_generation_ids,
                    "attemptGenerations": retry_generation_numbers,
                    "attemptStates": retry_generation_states,
                    "attemptIdempotencyKeys": retry_generation_idempotency_keys,
                    "concurrentResultIds": [
                        retry_generation_second_a.id,
                        retry_generation_second_b.id,
                    ],
                    "nonterminalDuplicateResultId": retry_generation_nonterminal_duplicate.id,
                    "firstFailedUpdatedAt": retry_generation_failed.updated_at,
                    "persistedFirstUpdatedAt": retry_generation_first_updated_at,
                    "durableRetryDirtyHints": retry_generation_dirty_hint_count,
                }),
            ),
            subcheck(
                "predecision_source_contract_failure_creates_one_immutable_retry_generation",
                source_contract_failed_attempt_and_successor_evidence,
                json!({
                    "historicalTerminalReason": retry_generation_first_terminal_reason,
                    "failedAttemptId": retry_generation_first.id,
                    "failedAttemptIdempotencyKey": retry_generation_first.idempotency_key,
                    "rediscoveryKey": retry_generation_first.rediscovery_key,
                    "failedAttemptGeneration": retry_generation_first.attempt_generation,
                    "persistedFailedExecutionPlan": retry_generation_first_execution_plan,
                    "persistedFailedUpdatedAt": retry_generation_first_updated_at,
                    "successorId": retry_generation_second_a.id,
                    "successorGeneration": retry_generation_second_a.attempt_generation,
                    "concurrentRediscoveryResultIds": [
                        retry_generation_second_a.id,
                        retry_generation_second_b.id,
                    ],
                    "successorState": retry_generation_second_a.state.as_str(),
                    "durableRetryDirtyHints": retry_generation_dirty_hint_count,
                }),
            ),
            subcheck(
                "fused_revalidation_promotes_only_with_immediate_exact_conflicts",
                fused_promoted.claim_kind == RebalanceOpportunityClaimKind::Execute
                    && fused_promoted.fencing_token > fused_revalidation.fencing_token
                    && fused_exact_conflict_ownership
                    && fused_fallback_preserved_revalidation,
                json!({
                    "revalidationFence": fused_revalidation.fencing_token,
                    "executeFence": fused_promoted.fencing_token,
                    "executeConflictKeys": fused_conflict_keys,
                    "exactConflictOwnership": fused_exact_conflict_ownership,
                    "conflictedPromotionReturnedFallback": conflicted_promotion.is_none(),
                    "conflictedLeaseKind": conflicted_after.lease_kind.map(|kind| kind.as_str()),
                    "conflictedFenceBefore": conflicted_revalidation.fencing_token,
                    "conflictedFenceAfter": conflicted_after.fencing_token,
                    "conflictedRows": conflicted_rows,
                }),
            ),
            subcheck(
                "commit_time_lifetime_fence_rolls_back_active_opportunity_publication",
                commit_publication_result.is_err()
                    && commit_publication_rejected_during_commit
                    && commit_publication_rows == 0,
                json!({
                    "publicationRejectedDuringCommit": commit_publication_result.is_err(),
                    "rejectedByCommitTimeLifetimeFence": commit_publication_rejected_during_commit,
                    "error": commit_publication_error,
                    "visibleOpportunityRows": commit_publication_rows,
                    "cluster": commit_publication_cluster,
                    "vaultId": commit_publication_record.vault_id.as_i64(),
                }),
            ),
            subcheck(
                "commit_time_lifetime_fence_rolls_back_fully_linked_signed_handoff",
                commit_lifetime_result.is_err()
                    && commit_lifetime_rejected_during_commit
                    && commit_lifetime_decisions == 0
                    && commit_lifetime_submissions == 0
                    && commit_lifetime_capacity_reservations == 0
                    && commit_lifetime_fee_reservations == 0
                    && commit_lifetime_conflict_rows
                        == i64::try_from(commit_lifetime_conflicts.len()).unwrap_or(-1)
                    && commit_lifetime_unattached_conflict_rows == commit_lifetime_conflict_rows
                    && commit_lifetime_owned_conflict_rows == commit_lifetime_conflict_rows
                    && commit_lifetime_margin_preserved
                    && commit_lifetime_state == "leased"
                    && commit_lifetime_owner.as_deref()
                        == Some(commit_lifetime_lease.owner.as_str())
                    && commit_lifetime_fence == commit_lifetime_lease.fencing_token,
                json!({
                    "handoffRejectedDuringCommit": commit_lifetime_result.is_err(),
                    "rejectedByCommitTimeLifetimeFence": commit_lifetime_rejected_during_commit,
                    "error": commit_lifetime_error,
                    "decisionRows": commit_lifetime_decisions,
                    "signedSubmissionRows": commit_lifetime_submissions,
                    "targetCapacityReservationRows": commit_lifetime_capacity_reservations,
                    "feeSpendReservationRows": commit_lifetime_fee_reservations,
                    "conflictRows": commit_lifetime_conflict_rows,
                    "unattachedConflictRows": commit_lifetime_unattached_conflict_rows,
                    "correctlyOwnedConflictRows": commit_lifetime_owned_conflict_rows,
                    "opportunityRetainedMinimumMargin": commit_lifetime_margin_preserved,
                    "opportunityStateAfterRollback": commit_lifetime_state,
                    "leaseOwnerAfterRollback": commit_lifetime_owner,
                    "fencingTokenAfterRollback": commit_lifetime_fence,
                }),
            ),
            subcheck(
                "commit_time_fence_allows_actual_row_deletion_cleanup",
                deleted_cleanup_result.is_ok() && deleted_cleanup_rows == 0,
                json!({
                    "deleteCommitted": deleted_cleanup_result.is_ok(),
                    "visibleOpportunityRows": deleted_cleanup_rows,
                    "opportunityId": deleted_cleanup_seed.id,
                }),
            ),
            subcheck(
                "commit_time_active_opportunity_epoch_identity_mismatch_fails_closed",
                active_identity_mismatch_result.is_err()
                    && active_identity_mismatch_rejected
                    && active_identity_cluster_after == active_identity_cluster,
                json!({
                    "identityMismatchRejectedDuringCommit": active_identity_mismatch_result.is_err(),
                    "rejectedByCommitTimeLifetimeFence": active_identity_mismatch_rejected,
                    "error": active_identity_mismatch_error,
                    "attemptedCluster": mismatched_active_identity_cluster,
                    "clusterAfterRollback": active_identity_cluster_after,
                    "expectedCluster": active_identity_cluster,
                }),
            ),
            subcheck(
                "commit_time_signed_handoff_identity_mismatch_fails_closed",
                signed_identity_result.is_err()
                    && signed_identity_mismatch_rejected
                    && signed_identity_decisions == 0
                    && signed_identity_submissions == 0
                    && signed_identity_epoch_after == signed_identity_epoch,
                json!({
                    "identityMismatchRejectedDuringCommit": signed_identity_result.is_err(),
                    "rejectedByCommitTimeLifetimeFence": signed_identity_mismatch_rejected,
                    "error": signed_identity_error,
                    "decisionRows": signed_identity_decisions,
                    "signedSubmissionRows": signed_identity_submissions,
                    "attemptedOptimizerEpochId": signed_identity_wrong_epoch.id,
                    "optimizerEpochIdAfterRollback": signed_identity_epoch_after,
                    "expectedOptimizerEpochId": signed_identity_epoch,
                }),
            ),
            subcheck(
                "normal_broadcast_transition_and_terminal_cleanup_remain_legal_but_reactivation_is_fenced",
                normal_submitted_result.is_ok()
                    && state_after_normal_submission == "submitted"
                    && terminal_cleanup_result.is_ok()
                    && reactivation_result.is_err()
                    && reactivation_rejected
                    && state_after_reactivation_attempt == "failed"
                    && opportunity_state_after_terminal_cleanup == "failed",
                json!({
                    "opportunityLifetimeAtNormalTransitionSeconds": 30,
                    "normalSignedToSubmittedCommitted": normal_submitted_result.is_ok(),
                    "stateAfterNormalSubmission": state_after_normal_submission,
                    "terminalCleanupCommitted": terminal_cleanup_result.is_ok(),
                    "terminalToSignedReactivationRejected": reactivation_result.is_err(),
                    "rejectedByCommitTimeLifetimeFence": reactivation_rejected,
                    "reactivationError": reactivation_error,
                    "stateAfterReactivationAttempt": state_after_reactivation_attempt,
                    "opportunityStateAfterTerminalCleanup": opportunity_state_after_terminal_cleanup,
                }),
            ),
            subcheck(
                "expired_unstarted_opportunity_is_swept_stale_and_unclaimable",
                swept_expired == 1 && swept_state == "stale" && swept_claim.is_none(),
                json!({
                    "sweptCount": swept_expired,
                    "state": swept_state,
                    "claimReturned": swept_claim.is_some(),
                }),
            ),
            subcheck(
                "stale_no_effect_opportunity_is_republished_as_a_fresh_attempt",
                stale_rediscovered.id != stale_retry_seed.id
                    && stale_rediscovered.attempt_generation
                        == stale_retry_seed.attempt_generation + 1
                    && stale_rediscovered.state == RebalanceOpportunityState::Revalidate
                    && stale_attempt_count == 2,
                json!({
                    "staleOpportunityId": stale_retry_seed.id,
                    "rediscoveredOpportunityId": stale_rediscovered.id,
                    "rediscoveredAttemptGeneration": stale_rediscovered.attempt_generation,
                    "rediscoveredState": stale_rediscovered.state.as_str(),
                    "attemptCount": stale_attempt_count,
                }),
            ),
            subcheck(
                "sixty_four_semantic_lanes_progress_independently",
                independent_count == 64,
                json!({"leasedIndependentVaultLanePairs": independent_count}),
            ),
            subcheck(
                "physical_writable_key_congestion_is_visible_beyond_semantic_lanes",
                physical_writable_key_congestion_visible,
                json!({
                    "cluster": physical_status_cluster,
                    "activePhysicalWritableKeyCount": physical_status_row.active_physical_writable_key_count,
                    "topPhysicalWritableKeyCongestion": physical_status_row.top_physical_writable_key_congestion,
                    "sharedPayer": shared_physical_payer,
                    "sharedTarget": shared_physical_target,
                }),
            ),
            subcheck(
                "same_vault_or_lane_conflicts_fail_closed",
                same_vault_rejected && same_lane_rejected,
                json!({
                    "sameVaultRejected": same_vault_rejected,
                    "sameLaneRejected": same_lane_rejected,
                }),
            ),
            subcheck(
                "fee_shard_floor_counts_nonterminal_reservations_and_releases_terminal_headroom",
                fee_floor_admission_passed,
                json!({
                    "observedBalanceLamports": 100_000,
                    "minimumBalanceLamports": 50_000,
                    "feePerRouteLamports": 30_000,
                    "secondAdmissionBlockedWhileFirstNonterminal": second_floor_blocked.is_err(),
                    "blockedError": second_floor_blocked_error,
                    "firstTerminalState": terminal_first_floor_submission.state.as_str(),
                    "secondAdmissionAfterTerminal": true,
                    "landedFailureConfirmedSlot": landed_failure.confirmed_slot,
                    "landedFailureConfirmedAtPersisted": landed_failure.confirmed_at.is_some(),
                    "immutableReservationCount": fee_floor_reservations,
                    "retainedFixtureCluster": fee_floor_cluster,
                }),
            ),
            subcheck(
                "policy_setup_funding_reservation_bounds_concurrent_debits_without_global_queue_lock",
                policy_setup_funding_reservations_passed,
                json!({
                    "sharedPayer": setup_funding_payer,
                    "observedBalanceLamports": 100_000,
                    "firstReservedLamports": 35_000,
                    "secondAttemptReservedLamports": 75_000,
                    "secondAdmissionBlockedWhileFirstNonterminal": second_setup_blocked_error.is_some(),
                    "blockedError": second_setup_blocked_error,
                    "staleHighBalanceSnapshotRejected": second_setup_stale_snapshot_error.is_some(),
                    "staleSnapshotError": second_setup_stale_snapshot_error,
                    "firstTerminalState": terminal_first_setup_submission.state.as_str(),
                    "secondAdmissionAfterTerminal": true,
                    "immutableReservationCount": setup_funding_reservation_count,
                    "globalPolicyConflictKeyRequired": false,
                }),
            ),
            subcheck(
                "isolated_fleet_load_measures_policy_lock_removal_and_fused_handoff_speedup",
                fleet_latency_speedup_passed,
                fleet_latency_load,
            ),
            subcheck(
                "concurrent_target_capacity_admits_until_headroom_and_rejects_only_excess",
                target_capacity_concurrency_passed,
                json!({
                    "simultaneousAttempts": 3,
                    "admittedReservations": admitted_capacity_reservations,
                    "excessCapacityRejections": capacity_excess_rejections,
                    "telemetryFenceRejections": capacity_telemetry_fence_rejections,
                    "rejectionErrors": capacity_rejection_errors,
                    "reservationGenerations": admitted_reservation_generations,
                    "admittedProjectedTargetApyBps": admitted_projected_target_apys,
                    "atomicEconomicsRecomputed": admitted_atomic_economics_recomputed,
                    "liveCommittedUsdMicros": live_capacity_usd_micros,
                    "maximumInflightUsdMicros": 250_000_000,
                    "staleStateReleaseCommitted": stale_state_capacity_release,
                    "staleFenceReleaseCommitted": stale_fence_capacity_release,
                    "currentFencedReleaseCommitted": current_capacity_release,
                }),
            ),
            subcheck(
                "pre_send_terminal_failure_releases_target_capacity",
                pre_send_terminal_failure_released_capacity,
                json!({
                    "submissionId": first_floor_submission.id,
                    "broadcastCount": first_floor_broadcast_count,
                    "reservationState": first_floor_capacity_state,
                }),
            ),
            subcheck(
                "reconciled_capacity_waits_for_strictly_newer_target_telemetry",
                reconciled_capacity_retention_passed,
                json!({
                    "movementSlot": capacity_movement_slot,
                    "confirmedMovementSlot": 50_000,
                    "reconciledFreshnessSlot": 50_010,
                    "stateAfterReconcile": capacity_state_after_reconcile,
                    "equalTelemetrySlot": 50_000,
                    "equalSlotReleaseCount": equal_slot_capacity_projection.released_after_telemetry_count,
                    "stateAtEqualSlot": capacity_state_at_equal_slot,
                    "crossedTelemetrySlot": 50_001,
                    "crossedSlotReleaseCount": crossed_slot_capacity_projection.released_after_telemetry_count,
                    "stateAfterCrossedSlot": capacity_state_after_crossed_slot,
                }),
            ),
            subcheck(
                "preexisting_newer_target_telemetry_releases_at_reconciliation",
                preexisting_newer_telemetry_releases_on_reconcile,
                json!({
                    "movementSlot": 60_000,
                    "reconciledFreshnessSlot": 60_010,
                    "preexistingTelemetrySlot": 60_001,
                    "reservationState": preobserved_capacity_state,
                    "releaseReason": preobserved_release_reason,
                }),
            ),
            subcheck(
                "landed_authoritative_failure_retains_confirmation_slot_and_time",
                landed_failure_retains_slot,
                json!({
                    "submissionId": landed_failure.id,
                    "state": landed_failure.state.as_str(),
                    "confirmedSlot": landed_failure.confirmed_slot,
                    "confirmedAtPersisted": landed_failure.confirmed_at.is_some(),
                }),
            ),
            subcheck(
                "attached_signed_conflicts_cannot_be_stolen_after_expiry",
                decision_linked
                    && attached_conflicts_not_stolen
                    && replacement_before_expiry_and_absence_proof == 0,
                json!({
                    "submissionId": persisted.id,
                    "decisionLinked": decision_linked,
                    "stealRejected": attached_conflicts_not_stolen,
                    "replacementMovementRows": replacement_before_expiry_and_absence_proof,
                }),
            ),
            subcheck(
                "signed_submission_links_decision_and_terminalizes_after_explicit_transitions",
                decision_linked
                    && confirming_state_after_explicit_transition == "confirming"
                    && terminal_opportunity_state == "completed",
                json!({
                    "submissionId": persisted.id,
                    "decisionId": decision_id.as_i64(),
                    "decisionLinked": decision_linked,
                    "stateAfterExplicitConfirmingTransition": confirming_state_after_explicit_transition,
                    "terminalOpportunityState": terminal_opportunity_state,
                }),
            ),
            subcheck(
                "submitted_ambiguous_and_reconciled_routes_never_generate_retry_attempts",
                signed_route_terminal_states_do_not_retry,
                json!({
                    "rediscoveryKey": signed_route_published.rediscovery_key,
                    "originalAttemptId": signed_route_published.id,
                    "submittedState": submitted_route.state.as_str(),
                    "submittedRediscoveryResultId": rediscovery_while_submitted.id,
                    "ambiguousState": ambiguous.state.as_str(),
                    "ambiguousRediscoveryResultId": rediscovery_while_ambiguous.id,
                    "reconciledRediscoveryResultId": rediscovery_after_reconciled.id,
                    "attemptCount": signed_route_rediscovery_attempt_count,
                }),
            ),
            subcheck(
                "signed_wire_and_identity_evidence_is_database_immutable",
                signed_evidence_immutable
                    && fee_payer_kind_immutable
                    && submission_state_timestamp_immutable,
                json!({
                    "submissionId": persisted.id,
                    "tamperedUpdateRejected": signed_evidence_immutable,
                    "feePayerKindMutationRejected": fee_payer_kind_immutable,
                    "stateEnteredAtMutationRejected": submission_state_timestamp_immutable,
                }),
            ),
            subcheck(
                "confirmer_reclaims_and_renews_exact_semantic_conflicts",
                confirmer_fenced && confirmer_renewed_exact_set,
                json!({
                    "firstFence": confirmer_first.fencing_token,
                    "reclaimedFence": confirmer_reclaimed.fencing_token,
                    "renewedKeys": confirmer_keys,
                }),
            ),
            subcheck(
                "ambiguous_effect_retains_setup_funding_until_confirmed_handoff",
                ambiguous.state.as_str() == "effect_ambiguous"
                    && ambiguous_conflict_keys == ambiguous_conflicts
                    && ambiguous_replacement_rejected
                    && ambiguous_or_stale_replacement_movements == 0
                    && ambiguous_recovery.submission.state.as_str() == "effect_ambiguous"
                    && ambiguous_recovery.fencing_token > ambiguous_first.fencing_token
                    && post_confirmation_conflict_keys == reconciliation_conflicts,
                json!({
                    "quarantinedState": ambiguous.state.as_str(),
                    "retainedConflictKeys": ambiguous_conflict_keys,
                    "replacementRejected": ambiguous_replacement_rejected,
                    "replacementMovementRows": ambiguous_or_stale_replacement_movements,
                    "firstRecoveryFence": ambiguous_first.fencing_token,
                    "reclaimedRecoveryFence": ambiguous_recovery.fencing_token,
                    "postConfirmationConflictKeys": post_confirmation_conflict_keys,
                }),
            ),
            subcheck(
                "reconciler_reclaims_and_renews_exact_semantic_conflicts",
                reconciler_fenced
                    && reconciler_renewed_exact_set
                    && remaining_terminal_conflicts == 0,
                json!({
                    "firstFence": reconciler_first.fencing_token,
                    "reclaimedFence": reconciler_reclaimed.fencing_token,
                    "renewedKeys": reconciler_keys,
                    "terminalConflictRows": remaining_terminal_conflicts,
                }),
            ),
            ];
            checks.extend(cross_mint_movement_checks);
            checks
        },
    })
}

async fn isolated_database_evidence(
    database_url: &str,
    repository_root: Option<&Path>,
) -> Result<DatabaseEvidence, Box<dyn Error>> {
    let fixture = DatabaseFixture::connect(database_url).await?;
    let checks = run_database_checks(&fixture).await;
    let repository_migration_checks = if let Some(repository_root) = repository_root {
        migration_repository_checks(&fixture, repository_root, database_url).await
    } else {
        Vec::new()
    };
    let cleanup = fixture.cleanup().await;
    match (checks, cleanup) {
        (Ok(mut evidence), Ok(cleanup_evidence)) => {
            evidence
                .migration_subchecks
                .extend(repository_migration_checks);
            let cleanup_passed = cleanup_evidence
                .get("mutableRowsRemaining")
                .and_then(Value::as_i64)
                == Some(0);
            evidence.migration_subchecks.push(subcheck(
                "isolated_mutable_fixture_cleanup",
                cleanup_passed,
                cleanup_evidence,
            ));
            Ok(evidence)
        }
        (Err(error), Ok(_)) => Err(error),
        (Ok(_), Err(cleanup_error)) => Err(cleanup_error),
        (Err(check_error), Err(cleanup_error)) => Err(format!(
            "database checks failed ({check_error}); fixture cleanup also failed ({cleanup_error})"
        )
        .into()),
    }
}

async fn migration_repository_checks(
    fixture: &DatabaseFixture,
    repository_root: &Path,
    database_url: &str,
) -> Vec<Subcheck> {
    let mut migrations = Vec::new();
    for (version, name, file_name) in VERIFIED_MIGRATIONS {
        let path = repository_root
            .join("crates/loyal-yield-store/migrations")
            .join(file_name);
        match fs::read(&path) {
            Ok(sql) => migrations.push((version, name, path, sql)),
            Err(error) => {
                return vec![subcheck(
                    "isolated_migration_repository_evidence",
                    false,
                    json!({"path": path, "error": error.to_string()}),
                )];
            }
        }
    }

    let mut ledger_evidence = Vec::new();
    let mut ledger_matches = true;
    for (version, expected_name, _, sql) in &migrations {
        let row = sqlx::query(
            "SELECT name, checksum FROM loyal_yield.schema_migrations WHERE version = $1",
        )
        .bind(version)
        .fetch_optional(fixture.client.pool())
        .await;
        match row {
            Ok(Some(row)) => {
                let name = row.try_get::<String, _>("name");
                let checksum = row.try_get::<String, _>("checksum");
                match (name, checksum) {
                    (Ok(name), Ok(checksum)) => {
                        let expected_checksum = sha256_hex(sql);
                        let matches = name == *expected_name && checksum == expected_checksum;
                        ledger_matches &= matches;
                        ledger_evidence.push(json!({
                            "version": version,
                            "name": name,
                            "expectedName": expected_name,
                            "checksum": checksum,
                            "expectedChecksum": expected_checksum,
                            "matches": matches,
                        }));
                    }
                    (Err(error), _) | (_, Err(error)) => {
                        ledger_matches = false;
                        ledger_evidence.push(json!({
                            "version": version,
                            "error": error.to_string(),
                        }));
                    }
                }
            }
            Ok(None) => {
                ledger_matches = false;
                ledger_evidence.push(json!({"version": version, "missing": true}));
            }
            Err(error) => {
                ledger_matches = false;
                ledger_evidence.push(json!({
                    "version": version,
                    "error": error.to_string(),
                }));
            }
        }
    }

    let reapply_result = async {
        let mut transaction = fixture.client.pool().begin().await?;
        for (_, _, _, sql) in &migrations {
            let sql = std::str::from_utf8(sql)
                .map_err(|error| sqlx::Error::Protocol(error.to_string()))?;
            sqlx::raw_sql(sql).execute(&mut *transaction).await?;
        }
        transaction.rollback().await
    }
    .await;

    let fee_payer_schema_contract = fee_payer_schema_contract_check(fixture).await;
    vec![
        run_migration_runner_check(repository_root, database_url),
        subcheck(
            "isolated_migration_ledger_matches_repository_bytes",
            ledger_matches,
            json!({"migrations": ledger_evidence}),
        ),
        subcheck(
            "migration_sql_23_through_31_reexecutes_in_rolled_back_transaction",
            reapply_result.is_ok(),
            json!({
                "transaction": "ROLLED_BACK",
                "error": reapply_result.err().map(|error| error.to_string()),
            }),
        ),
        fee_payer_schema_contract,
    ]
}

async fn fee_payer_schema_contract_check(fixture: &DatabaseFixture) -> Subcheck {
    let evidence = async {
        let ceiling_definition: String = sqlx::query_scalar(
            r#"
            SELECT pg_get_constraintdef(oid)
            FROM pg_constraint
            WHERE conrelid = 'loyal_yield.route_fee_payer_shards'::regclass
              AND conname = 'route_fee_payer_shards_low_balance_check'
            "#,
        )
        .fetch_one(fixture.client.pool())
        .await?;
        let trigger_rows = sqlx::query(
            r#"
            SELECT tgname, pg_get_triggerdef(oid) AS definition
            FROM pg_trigger
            WHERE NOT tgisinternal
              AND tgname IN (
                  'route_fee_payer_shards_alt_authority_separation',
                  'lookup_table_families_fee_payer_shard_separation',
                  'route_lookup_tables_fee_payer_shard_separation',
                  'route_policies_fee_payer_shard_separation',
                  'managed_vaults_fee_payer_shard_separation',
                  'route_fee_payer_spend_reservations_immutable'
              )
            ORDER BY tgname
            "#,
        )
        .fetch_all(fixture.client.pool())
        .await?;
        let triggers = trigger_rows
            .iter()
            .map(|row| {
                Ok((
                    row.try_get::<String, _>("tgname")?,
                    row.try_get::<String, _>("definition")?,
                ))
            })
            .collect::<Result<Vec<_>, sqlx::Error>>()?;
        let view_columns = sqlx::query_scalar::<_, String>(
            r#"
            SELECT column_name
            FROM information_schema.columns
            WHERE table_schema = 'loyal_yield'
              AND table_name = 'route_fee_payer_shard_status'
            ORDER BY ordinal_position
            "#,
        )
        .fetch_all(fixture.client.pool())
        .await?;

        Ok::<_, sqlx::Error>((ceiling_definition, triggers, view_columns))
    }
    .await;

    match evidence {
        Ok((ceiling_definition, triggers, view_columns)) => {
            let separation_triggers = [
                "route_fee_payer_shards_alt_authority_separation",
                "lookup_table_families_fee_payer_shard_separation",
                "route_lookup_tables_fee_payer_shard_separation",
                "route_policies_fee_payer_shard_separation",
                "managed_vaults_fee_payer_shard_separation",
            ];
            let separation_present = separation_triggers.iter().all(|expected| {
                triggers.iter().any(|(name, definition)| {
                    name == expected && definition.contains("BEFORE INSERT OR UPDATE")
                })
            });
            let reservations_immutable = triggers.iter().any(|(name, definition)| {
                name == "route_fee_payer_spend_reservations_immutable"
                    && (definition.contains("BEFORE UPDATE OR DELETE")
                        || definition.contains("BEFORE DELETE OR UPDATE"))
            });
            let required_view_columns = [
                "payer_role",
                "delegated_policy_signer",
                "reusable_alt_authority",
                "reusable_alt_payer",
                "setup_farm_or_rent_payer",
                "database_authority_separation_passes",
                "current_window_remaining_lamports",
            ];
            let status_view_present = required_view_columns
                .iter()
                .all(|required| view_columns.iter().any(|column| column == required));
            let passed = ceiling_definition.contains("maximum_balance_lamports <= 100000000")
                && separation_present
                && reservations_immutable
                && status_view_present;
            subcheck(
                "fee_payer_shard_database_contract_is_bounded_and_separated",
                passed,
                json!({
                    "ceilingConstraint": ceiling_definition,
                    "triggers": triggers.iter().map(|(name, definition)| json!({
                        "name": name,
                        "definition": definition,
                    })).collect::<Vec<_>>(),
                    "statusViewColumns": view_columns,
                }),
            )
        }
        Err(error) => subcheck(
            "fee_payer_shard_database_contract_is_bounded_and_separated",
            false,
            json!({"error": error.to_string()}),
        ),
    }
}

fn first_failed(subchecks: &[Subcheck]) -> Option<&'static str> {
    subchecks
        .iter()
        .find(|check| matches!(check.verdict, Verdict::Fail))
        .map(|check| check.name)
}

fn first_not_run(subchecks: &[Subcheck]) -> Option<&'static str> {
    subchecks
        .iter()
        .find(|check| matches!(check.verdict, Verdict::NotRun))
        .map(|check| check.name)
}

fn check(
    id: u8,
    name: &'static str,
    verdict: Verdict,
    first_failing_invariant: Option<&'static str>,
    evidence: Value,
    subchecks: Vec<Subcheck>,
) -> VerifierCheck {
    VerifierCheck {
        id,
        name,
        verdict,
        first_failing_invariant,
        evidence,
        subchecks,
    }
}

fn implementation_checks(
    database: Option<DatabaseEvidence>,
    local: Option<LocalEvidence>,
    runtime: Option<&RuntimeEvidenceV1>,
) -> Result<Vec<VerifierCheck>, String> {
    let deterministic = deterministic_evidence()?;
    let database_was_run = database.is_some();
    let local_was_run = local.is_some();
    let runtime_was_run = runtime.is_some();
    let (
        migration_subchecks,
        database_discovery_subchecks,
        alt_subchecks,
        database_execution_subchecks,
    ) = database
        .map(|evidence| {
            (
                evidence.migration_subchecks,
                evidence.discovery_subchecks,
                evidence.alt_subchecks,
                evidence.execution_subchecks,
            )
        })
        .unwrap_or_default();
    let (
        mut repository_subchecks,
        mut wiring_subchecks,
        repository_root,
        head_commit,
        runtime_source_digest_sha256,
        production_light_worker_image_reference,
        production_heavy_worker_image_reference,
    ) = local
        .map(|evidence| {
            (
                evidence.repository_subchecks,
                evidence.wiring_subchecks,
                Some(evidence.repository_root),
                evidence.head_commit,
                Some(evidence.runtime_source_digest_sha256),
                evidence.production_light_worker_image_reference,
                evidence.production_heavy_worker_image_reference,
            )
        })
        .unwrap_or_default();
    repository_subchecks.extend(migration_subchecks);
    let migration_failure = first_failed(&repository_subchecks);
    let migration_not_run = first_not_run(&repository_subchecks);
    let mut discovery_subchecks = deterministic.discovery_subchecks;
    discovery_subchecks.extend(database_discovery_subchecks);
    if let Some(runtime) = runtime {
        discovery_subchecks.push(runtime_discovery_subcheck(runtime));
    }
    let discovery_failure = first_failed(&discovery_subchecks);
    let economics_failure = first_failed(&deterministic.economic_subchecks);
    let mut alt_subchecks = alt_subchecks;
    if let Some(runtime) = runtime {
        alt_subchecks.push(runtime_alt_subcheck(runtime));
    }
    let alt_failure = first_failed(&alt_subchecks);
    let mut execution_subchecks = deterministic.execution_subchecks;
    execution_subchecks.extend(database_execution_subchecks);
    if let Some(runtime) = runtime {
        execution_subchecks.push(runtime_execution_subcheck(runtime));
        execution_subchecks.push(runtime_source_evidence_contract_subcheck(
            &runtime.execution.source_evidence_contract_fixtures,
        ));
    }
    let execution_failure = first_failed(&execution_subchecks);
    if let Some(runtime) = runtime {
        wiring_subchecks.push(runtime_wiring_subcheck(
            runtime,
            production_light_worker_image_reference.as_deref(),
            production_heavy_worker_image_reference.as_deref(),
            repository_root.as_deref(),
        ));
    }
    let wiring_failure = first_failed(&wiring_subchecks);

    Ok(vec![
        check(
            1,
            "repository_and_migration_integrity",
            if migration_failure.is_some() {
                Verdict::Fail
            } else if !(database_was_run && local_was_run) {
                Verdict::NotRun
            } else {
                aggregate_subchecks(&repository_subchecks)
            },
            migration_failure
                .or_else(|| {
                    (!(database_was_run && local_was_run)).then_some(
                    "collect repository commands and isolated migration idempotence evidence",
                    )
                })
                .or(migration_not_run),
            json!({
                "isolatedDatabase": if database_was_run { "RUN" } else { "NOT_RUN" },
                "repositoryEvidence": if local_was_run { "COLLECTED" } else { "NOT_RUN" },
                "repositoryRoot": repository_root,
                "headCommit": head_commit,
                "runtimeSourceDigestSha256": runtime_source_digest_sha256,
            }),
            repository_subchecks,
        ),
        check(
            2,
            "fast_complete_discovery",
            if discovery_failure.is_some() {
                Verdict::Fail
            } else if runtime_was_run {
                aggregate_subchecks(&discovery_subchecks)
            } else {
                Verdict::NotRun
            },
            discovery_failure.or_else(|| {
                (!runtime_was_run).then_some(
                    "source-bound live or captured current-fleet completeness, p95, epoch freshness, and zero-child-process evidence was not collected",
                )
            }),
            json!({
                "scope": if database_was_run {
                    if runtime_was_run {
                        "deterministic, isolated PostgreSQL, and source-bound current-fleet evidence"
                    } else {
                        "deterministic in-memory plus isolated PostgreSQL queue evidence"
                    }
                } else if runtime_was_run {
                    "deterministic plus source-bound current-fleet evidence"
                } else {
                    "deterministic in-memory evidence only"
                },
                "unverified": [
                    "every eligible current vault considered from one non-expired epoch",
                    "current-fleet planning p95 under five seconds",
                    "top-value non-conflicting cohort ordering",
                    "zero child route or reconcile processes"
                ]
            }),
            discovery_subchecks,
        ),
        check(
            3,
            "economic_behavior",
            aggregate_subchecks(&deterministic.economic_subchecks),
            economics_failure,
            json!({"scope": "deterministic in-memory planner inputs"}),
            deterministic.economic_subchecks,
        ),
        check(
            4,
            "alt_head_of_line_isolation",
            if alt_failure.is_some() {
                Verdict::Fail
            } else if database_was_run && runtime_was_run {
                aggregate_subchecks(&alt_subchecks)
            } else {
                Verdict::NotRun
            },
            alt_failure.or_else(|| {
                (!(database_was_run && runtime_was_run)).then_some(
                    "all-ready drain, automatic coverage wakeup, global-rollout-lock exclusion, and concurrent fenced physical ALT mutation remain unverified",
                )
            }),
            json!({
                "isolatedDatabase": if database_was_run { "RUN" } else { "NOT_RUN" },
                "requiredExternalEvidence": "drain every ready row while cold rows wait; observe automatic affected-only coverage wakeup; prove normal readiness avoids the rollout lock; and run concurrent independent physical ALT lanes with same-table predecessor fencing"
            }),
            alt_subchecks,
        ),
        check(
            5,
            "execution_concurrency_and_crash_safety",
            if execution_failure.is_some() {
                Verdict::Fail
            } else if database_was_run && runtime_was_run {
                aggregate_subchecks(&execution_subchecks)
            } else {
                Verdict::NotRun
            },
            execution_failure.or_else(|| {
                (!(database_was_run && runtime_was_run)).then_some(
                    "controlled-RPC exact-byte replay/replacement safety, slot-fenced reconciliation, real POLICY/shard signatures, and route-class fee-payer fixtures remain unverified",
                )
            }),
            json!({
                "isolatedDatabase": if database_was_run { "RUN" } else { "NOT_RUN" },
                "requiredExternalEvidence": "controlled RPC worker evidence for exact-byte replay, safe expiry replacement, minContextSlot reconciliation, POLICY plus shard signer sets, setup/idle POLICY fallback, and bounded shard failover"
            }),
            execution_subchecks,
        ),
        {
            let replay_subchecks = runtime
                .map(runtime_replay_subcheck)
                .into_iter()
                .collect::<Vec<_>>();
            let replay_failure = first_failed(&replay_subchecks);
            check(
                6,
                "performance_value_and_price",
                if replay_failure.is_some() {
                    Verdict::Fail
                } else if runtime_was_run {
                    aggregate_subchecks(&replay_subchecks)
                } else {
                    Verdict::NotRun
                },
                replay_failure.or_else(|| {
                    (!runtime_was_run).then_some("source-bound production-like submission, confirmation, ALT-backlog, yield-unlock, fee, and duplicate-movement SLOs were not measured")
                }),
                json!({
                    "implementationReplay": if runtime_was_run { "COLLECTED" } else { "NOT_RUN" },
                    "productionPerformance": "NOT_RUN",
                }),
                replay_subchecks,
            )
        },
        check(
            7,
            "production_wiring_and_short_feedback_loop",
            if wiring_failure.is_some() {
                Verdict::Fail
            } else if !(local_was_run && runtime_was_run) {
                Verdict::NotRun
            } else {
                aggregate_subchecks(&wiring_subchecks)
            },
            wiring_failure.or_else(|| {
                (!(local_was_run && runtime_was_run)).then_some(
                    "collect repository wiring plus source-bound local-container binary probes and functional stuck-stage evidence",
                )
            }),
            json!({
                "evidenceScope": "local repository plus source-bound local-container and functional status evidence",
                "repositoryWiring": if local_was_run { "INSPECTED" } else { "NOT_RUN" },
                "localContainerAndStatusEvidence": if runtime_was_run { "COLLECTED" } else { "NOT_RUN" },
                "deploymentEvidenceExcludedFromImplementation": [
                    "live image registry presence",
                    "live Render services and commands",
                    "production migration state"
                ],
                "operatorActionRequiredForDeployment": true,
            }),
            wiring_subchecks,
        ),
    ])
}

fn production_expected_services(
    repository_root: &Path,
) -> Result<Vec<ExpectedProductionService>, Box<dyn Error>> {
    let render_yaml = fs::read_to_string(repository_root.join("render.yaml"))?;
    let production = production_environment(&render_yaml)
        .ok_or("render.yaml has no loyal-yield-light-workers production environment")?;
    let blocks = service_blocks(production);
    PRODUCTION_SERVICE_NAMES
        .iter()
        .map(|name| {
            let matching = blocks
                .iter()
                .filter(|block| yaml_scalar(block, "name") == Some(*name))
                .collect::<Vec<_>>();
            if matching.len() != 1 {
                return Err(format!(
                    "render.yaml must declare exactly one production service named {name}"
                )
                .into());
            }
            let block = matching[0];
            Ok(ExpectedProductionService {
                name: (*name).to_owned(),
                image: yaml_scalar(block, "url")
                    .ok_or_else(|| format!("{name} has no image URL"))?
                    .to_owned(),
                command: yaml_scalar(block, "dockerCommand")
                    .ok_or_else(|| format!("{name} has no dockerCommand"))?
                    .to_owned(),
                pre_deploy_command: yaml_scalar(block, "preDeployCommand")
                    .ok_or_else(|| format!("{name} has no preDeployCommand"))?
                    .to_owned(),
                plan: yaml_scalar(block, "plan")
                    .ok_or_else(|| format!("{name} has no plan"))?
                    .to_owned(),
                env_keys: service_env_keys(block)
                    .into_iter()
                    .map(str::to_owned)
                    .collect(),
            })
        })
        .collect()
}

fn production_expected_kamino_monitor(
    repository_root: &Path,
) -> Result<ExpectedProductionService, Box<dyn Error>> {
    let render_yaml = fs::read_to_string(repository_root.join("render.yaml"))?;
    let production =
        project_production_environment(&render_yaml, "loyal-yield-laserstream-workers")
            .ok_or("render.yaml has no loyal-yield-laserstream-workers production environment")?;
    let matching = service_blocks(production)
        .into_iter()
        .filter(|block| yaml_scalar(block, "name") == Some(KAMINO_MONITOR_SERVICE_NAME))
        .collect::<Vec<_>>();
    if matching.len() != 1 {
        return Err(format!(
            "render.yaml must declare exactly one production service named {KAMINO_MONITOR_SERVICE_NAME}"
        )
        .into());
    }
    let block = &matching[0];
    Ok(ExpectedProductionService {
        name: KAMINO_MONITOR_SERVICE_NAME.to_owned(),
        image: yaml_scalar(block, "url")
            .ok_or("Kamino monitor has no image URL")?
            .to_owned(),
        command: yaml_scalar(block, "dockerCommand")
            .ok_or("Kamino monitor has no dockerCommand")?
            .to_owned(),
        pre_deploy_command: yaml_scalar(block, "preDeployCommand")
            .ok_or("Kamino monitor has no preDeployCommand")?
            .to_owned(),
        plan: yaml_scalar(block, "plan")
            .ok_or("Kamino monitor has no plan")?
            .to_owned(),
        env_keys: service_env_keys(block)
            .into_iter()
            .map(str::to_owned)
            .collect(),
    })
}

fn image_commit_suffix(image: &str) -> Option<&str> {
    let suffix = image.rsplit_once(":sha-")?.1;
    (suffix.len() == 40 && suffix.bytes().all(|byte| byte.is_ascii_hexdigit())).then_some(suffix)
}

fn valid_sha256_digest(value: &str) -> bool {
    value.strip_prefix("sha256:").is_some_and(|digest| {
        digest.len() == 64 && digest.bytes().all(|byte| byte.is_ascii_hexdigit())
    })
}

fn image_source_binding(repository_root: &Path, image: &str) -> (bool, Value) {
    const ALLOWED_POST_IMAGE_PATHS: [&str; 5] = [
        "render.yaml",
        "docs/plans/fleet-yield-orchestration-speed-verifier.md",
        "crates/loyal-yield-orchestrator/src/bin/fleet-orchestration-runtime-evidence.rs",
        "crates/loyal-yield-orchestrator/src/bin/fleet-orchestration-verifier.rs",
        "crates/loyal-yield-orchestrator/src/fleet_orchestration/runtime_evidence.rs",
    ];
    let head = git_stdout(repository_root, &["rev-parse", "HEAD"]);
    let image_commit = image_commit_suffix(image).map(str::to_owned);
    let object_is_commit = image_commit.as_deref().is_some_and(|commit| {
        git_success(
            repository_root,
            &["cat-file", "-e", &format!("{commit}^{{commit}}")],
        )
    });
    let is_head = image_commit.as_deref() == head.as_deref();
    let is_ancestor = image_commit.as_deref().is_some_and(|commit| {
        git_success(
            repository_root,
            &["merge-base", "--is-ancestor", commit, "HEAD"],
        )
    });
    let changed_paths = image_commit
        .as_deref()
        .filter(|_| object_is_commit && is_ancestor && !is_head)
        .and_then(|commit| git_stdout(repository_root, &["diff", "--name-only", commit, "HEAD"]))
        .map(|paths| {
            paths
                .lines()
                .filter(|path| !path.is_empty())
                .map(str::to_owned)
                .collect::<BTreeSet<_>>()
        })
        .unwrap_or_default();
    let changed_paths_allowed = changed_paths
        .iter()
        .all(|path| ALLOWED_POST_IMAGE_PATHS.contains(&path.as_str()));
    let passed = object_is_commit && (is_head || (is_ancestor && changed_paths_allowed));
    (
        passed,
        json!({
            "image": image,
            "imageCommit": image_commit,
            "checkoutHead": head,
            "imageObjectIsCommit": object_is_commit,
            "imageCommitIsHead": is_head,
            "imageCommitIsAncestor": is_ancestor,
            "postImageChangedPaths": changed_paths,
            "allowedPostImagePaths": ALLOWED_POST_IMAGE_PATHS,
            "postImageDiffIsLimitedToVerificationOrPinningFiles": changed_paths_allowed,
        }),
    )
}

fn load_production_evidence(
    path: &Path,
    repository_root: &Path,
) -> Result<ProductionEvidenceBinding, Box<dyn Error>> {
    let bytes = fs::read(path)?;
    let artifact: ProductionEvidenceV1 = serde_json::from_slice(&bytes)?;
    if artifact.schema_version != 1 {
        return Err(format!(
            "production evidence schemaVersion must be 1, got {}",
            artifact.schema_version
        )
        .into());
    }
    if artifact.event != "fleet_orchestration_production_evidence" {
        return Err("production evidence event is not recognized".into());
    }
    let now = Utc::now();
    let collection_duration = artifact
        .captured_at
        .signed_duration_since(artifact.collection_started_at);
    if artifact.captured_at < now - PRODUCTION_EVIDENCE_MAX_AGE
        || artifact.captured_at > now + PRODUCTION_EVIDENCE_MAX_FUTURE_SKEW
        || artifact.collected_at < now - PRODUCTION_EVIDENCE_MAX_AGE
        || artifact.collected_at > now + PRODUCTION_EVIDENCE_MAX_FUTURE_SKEW
        || artifact.collected_at != artifact.captured_at
        || collection_duration < chrono::Duration::zero()
        || collection_duration
            > chrono::Duration::seconds(PRODUCTION_EVIDENCE_MAX_COLLECTION_SECONDS)
    {
        return Err(
            "production evidence must be one fresh bounded internally consistent capture".into(),
        );
    }
    if artifact.scope.cluster != PRODUCTION_CLUSTER {
        return Err(format!("production evidence cluster must be {PRODUCTION_CLUSTER}").into());
    }
    if artifact.scope.render_environment_id != PRODUCTION_RENDER_ENVIRONMENT_ID {
        return Err(format!(
            "production evidence Render environment must be {PRODUCTION_RENDER_ENVIRONMENT_ID}"
        )
        .into());
    }
    if artifact.scope.cutover_at.is_none() || !artifact.scope.baseline_path_supplied {
        return Err(
            "end-state evidence requires a cutover timestamp and a supplied pre-cutover baseline"
                .into(),
        );
    }
    if artifact.caller_verdicts_accepted {
        return Err("production evidence unexpectedly accepts caller verdicts".into());
    }
    if artifact.source.tracked_worktree_dirty {
        return Err("production evidence was captured from a dirty tracked worktree".into());
    }
    if !artifact.source.collector_source.contains("measurements") {
        return Err("production evidence collector provenance is missing".into());
    }
    let head_commit = git_stdout(repository_root, &["rev-parse", "HEAD"])
        .ok_or("cannot read checkout HEAD for production evidence binding")?;
    let tracked_status = git_stdout(
        repository_root,
        &["status", "--porcelain", "--untracked-files=no"],
    )
    .ok_or("cannot inspect checkout worktree for production evidence binding")?;
    if !tracked_status.is_empty() {
        return Err("end-state verification requires a clean tracked checkout".into());
    }
    let artifact_head = artifact
        .head_commit
        .as_deref()
        .filter(|artifact_head| artifact.source.repository_head.as_deref() == Some(*artifact_head))
        .ok_or("production evidence HEAD fields do not match")?;
    let artifact_head_is_checkout = artifact_head == head_commit;
    let artifact_head_is_ancestor = git_success(
        repository_root,
        &["merge-base", "--is-ancestor", artifact_head, &head_commit],
    );
    let post_evidence_paths = (!artifact_head_is_checkout && artifact_head_is_ancestor)
        .then(|| {
            git_stdout(
                repository_root,
                &["diff", "--name-only", artifact_head, &head_commit],
            )
        })
        .flatten()
        .map(|paths| {
            paths
                .lines()
                .filter(|path| !path.is_empty())
                .map(str::to_owned)
                .collect::<BTreeSet<_>>()
        })
        .unwrap_or_default();
    let post_evidence_diff_is_verifier_only = !post_evidence_paths.is_empty()
        && post_evidence_paths.iter().all(|path| {
            path.as_str()
                == "crates/loyal-yield-orchestrator/src/bin/fleet-orchestration-verifier.rs"
        });
    if !(artifact_head_is_checkout
        || artifact_head_is_ancestor && post_evidence_diff_is_verifier_only)
    {
        return Err(
            "production evidence HEAD differs outside the independent verifier source".into(),
        );
    }
    let render_yaml = fs::read(repository_root.join("render.yaml"))?;
    let render_yaml_sha256 = sha256_hex(&render_yaml);
    if artifact.source.render_yaml_sha256 != render_yaml_sha256 {
        return Err("production evidence render.yaml digest does not match the checkout".into());
    }
    let collector_source_path = repository_root
        .join("crates/loyal-yield-orchestrator/src/bin/fleet-orchestration-production-evidence.rs");
    let collector_source_sha256 =
        sha256_file(&collector_source_path).ok_or("cannot hash the production collector source")?;
    if artifact.source.collector_compiled_source_sha256 != collector_source_sha256
        || artifact.source.collector_checkout_source_sha256.as_deref()
            != Some(collector_source_sha256.as_str())
    {
        return Err(
            "production collector executable is not built from the inspected source".into(),
        );
    }
    let collector_executable_path = env::current_exe()?
        .parent()
        .ok_or("verifier executable has no parent directory")?
        .join("fleet-orchestration-production-evidence");
    let collector_executable_sha256 = sha256_file(&collector_executable_path)
        .ok_or("production collector executable is not a sibling of the verifier")?;
    if artifact.source.collector_executable_sha256.as_deref()
        != Some(collector_executable_sha256.as_str())
    {
        return Err(
            "production evidence was not emitted by the current collector executable".into(),
        );
    }
    if artifact
        .measurements
        .render
        .get("environmentId")
        .and_then(Value::as_str)
        != Some(artifact.scope.render_environment_id.as_str())
    {
        return Err(
            "production Render measurements are not bound to the scoped environment".into(),
        );
    }

    Ok(ProductionEvidenceBinding {
        artifact,
        repository_root: repository_root.to_path_buf(),
        head_commit,
        render_yaml_sha256,
    })
}

fn value_i64(value: &Value, key: &str) -> Option<i64> {
    value.get(key).and_then(Value::as_i64)
}

fn value_bool(value: &Value, key: &str) -> Option<bool> {
    value.get(key).and_then(Value::as_bool)
}

fn value_string<'a>(value: &'a Value, key: &str) -> Option<&'a str> {
    value.get(key).and_then(Value::as_str)
}

fn env_value_fingerprint(nonce: &str, key: &str, value: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(b"loyal-render-env-scope-v1\0");
    hasher.update((nonce.len() as u64).to_le_bytes());
    hasher.update(nonce.as_bytes());
    hasher.update((key.len() as u64).to_le_bytes());
    hasher.update(key.as_bytes());
    hasher.update((value.len() as u64).to_le_bytes());
    hasher.update(value.as_bytes());
    format!("{:x}", hasher.finalize())
}

fn role_service_scoped_env_keys(env_keys: &BTreeSet<String>) -> BTreeSet<String> {
    env_keys.clone()
}

fn role_scope_value_keys(name: &str, env_keys: &BTreeSet<String>) -> BTreeSet<String> {
    role_service_scoped_env_keys(env_keys)
        .into_iter()
        .filter(|key| key.as_str() != "RUST_LOG")
        // Presence is required, but the local verifier does not need the
        // ingestion secret and must never place it in evidence.
        .filter(|key| key.as_str() != "OBSERVABILITY_INGESTION_API_KEY")
        .filter(|key| {
            !matches!(
                key.as_str(),
                "HELIUS_API_KEY"
                    | "LASERSTREAM_ENDPOINT"
                    | "KAMINO_API_BASE"
                    | "KAMINO_UPDATE_SOURCE"
            )
        })
        .filter(|key| name != "loyal-fleet-opportunity-planner" || key.as_str() != "POLICY_KEYPAIR")
        .collect()
}

fn monitor_scope_value_keys(env_keys: &BTreeSet<String>) -> BTreeSet<String> {
    env_keys
        .iter()
        .filter(|key| key.as_str() != "RUST_LOG")
        .filter(|key| key.as_str() != "OBSERVABILITY_INGESTION_API_KEY")
        .cloned()
        .collect()
}

fn expected_env_value(key: &str) -> Option<String> {
    match key {
        "YIELD_ALT_CLUSTER" => Some(PRODUCTION_CLUSTER.to_owned()),
        "KAMINO_UPDATE_SOURCE" => Some("laserstream".to_owned()),
        "OBSERVABILITY_ENABLED" => Some("true".to_owned()),
        "OBSERVABILITY_ENVIRONMENT" => Some("production".to_owned()),
        "OBSERVABILITY_OTLP_ENDPOINT" => Some("https://loyal-clickstack.onrender.com".to_owned()),
        // A blank fee-payer pool is a valid explicit configuration: the
        // route workers intentionally fall back to POLICY_KEYPAIR.
        "YIELD_ROUTE_FEE_PAYER_KEYPAIRS" => env::var(key).ok(),
        _ => env::var(key).ok().filter(|value| !value.trim().is_empty()),
    }
}

fn expected_env_fingerprints(
    nonce: &str,
    keys: impl IntoIterator<Item = String>,
) -> Option<BTreeMap<String, String>> {
    keys.into_iter()
        .map(|key| {
            let value = expected_env_value(&key)?;
            Some((key.clone(), env_value_fingerprint(nonce, &key, &value)))
        })
        .collect()
}

fn reported_env_fingerprints(value: Option<&Value>) -> Option<BTreeMap<String, String>> {
    serde_json::from_value(value?.clone()).ok()
}

fn exact_object_keys(value: &Value, expected: &[&str]) -> bool {
    value.as_object().is_some_and(|object| {
        object.len() == expected.len() && expected.iter().all(|key| object.contains_key(*key))
    })
}

fn migration_measurement_schema_known(value: &Value) -> bool {
    exact_object_keys(value, &["ledgerExists", "required", "pass"])
        && value
            .get("required")
            .and_then(Value::as_array)
            .is_some_and(|rows| {
                rows.iter().all(|row| {
                    exact_object_keys(
                        row,
                        &[
                            "version",
                            "name",
                            "sourceChecksum",
                            "applied",
                            "appliedName",
                            "appliedChecksum",
                            "appliedAt",
                            "matchesSource",
                        ],
                    )
                })
            })
}

fn render_measurement_schema_known(value: &Value) -> bool {
    if !exact_object_keys(
        value,
        &[
            "available",
            "capturedAt",
            "environmentId",
            "scopeFingerprintNonce",
            "expectedImageReferences",
            "roles",
            "allRolesMatch",
            "deployDigests",
            "oneImmutableDigest",
            "heavyEnvironmentId",
            "marketMonitor",
            "lightImageCommitSuffix",
            "laserstreamImageCommitSuffix",
            "imageCommitSuffixesMatch",
            "serialMonitor",
            "firstFleetSenderDeployStartedAt",
            "serialSuspendedBeforeFleetSenderStarted",
            "pass",
        ],
    ) {
        return false;
    }
    let roles_known = value
        .get("roles")
        .and_then(Value::as_array)
        .is_some_and(|roles| {
            roles.iter().all(|role| {
                exact_object_keys(
                    role,
                    &[
                        "name",
                        "id",
                        "present",
                        "matches",
                        "suspended",
                        "runtime",
                        "plan",
                        "numInstances",
                        "image",
                        "command",
                        "preDeployCommand",
                        "envKeys",
                        "envValueFingerprints",
                        "envBoundaryPasses",
                        "envBoundaryFailures",
                        "blueprintEnvKeys",
                        "blueprintEnvBoundaryPasses",
                        "blueprintEnvBoundaryFailures",
                        "latestDeploy",
                        "deployReadError",
                        "envReadError",
                    ],
                ) && role.get("latestDeploy").is_some_and(|deploy| {
                    exact_object_keys(
                        deploy,
                        &[
                            "id",
                            "status",
                            "imageRef",
                            "imageDigest",
                            "registryCredential",
                            "startedAt",
                            "finishedAt",
                        ],
                    )
                })
            })
        });
    let market_monitor_known = value.get("marketMonitor").is_some_and(|monitor| {
        exact_object_keys(
            monitor,
            &[
                "environmentId",
                "serviceId",
                "name",
                "present",
                "matches",
                "type",
                "suspended",
                "runtime",
                "plan",
                "numInstances",
                "image",
                "command",
                "preDeployCommand",
                "envKeys",
                "blueprintEnvKeys",
                "envValueFingerprints",
                "envKeyBoundaryExact",
                "dataScopeVerified",
                "latestDeploy",
                "serviceReadError",
                "deployReadError",
                "envReadError",
            ],
        ) && monitor.get("latestDeploy").is_some_and(|deploy| {
            exact_object_keys(
                deploy,
                &[
                    "id",
                    "status",
                    "imageRef",
                    "imageDigest",
                    "registryCredential",
                    "startedAt",
                    "finishedAt",
                ],
            )
        })
    });
    let serial_known = value.get("serialMonitor").is_some_and(|serial| {
        exact_object_keys(
            serial,
            &[
                "id",
                "present",
                "suspended",
                "scaledToZero",
                "executeFlagAbsent",
                "currentlyIncapableOfSending",
                "command",
                "suspensionEvents",
                "eventReadError",
            ],
        ) && serial
            .get("suspensionEvents")
            .and_then(Value::as_array)
            .is_some_and(|events| {
                events
                    .iter()
                    .all(|event| exact_object_keys(event, &["type", "timestamp"]))
            })
    });
    roles_known && market_monitor_known && serial_known
}

fn market_timescale_measurement_schema_known(value: &Value) -> bool {
    const TARGET_KEYS: &[&str] = &[
        "liquidityMint",
        "eligibleTargetCount",
        "riskBaskets",
        "reserve",
        "market",
        "supplyApy",
        "totalSupplyUsdEstimate",
        "reserveLastUpdateStale",
        "stateEventId",
        "accountDataHash",
        "stateObservedAt",
        "stateSlot",
        "verifiedAt",
        "verifiedSlot",
        "stateSource",
        "verificationCommitment",
        "verificationSource",
        "observationFloorSlot",
        "observationFloorObservationId",
        "observationFloorAccountDataHash",
        "observationFloorStateValid",
        "observationFloorSource",
        "observationFloorSourceRank",
        "observationFloorObservedAt",
    ];
    exact_object_keys(
        value,
        &[
            "available",
            "capturedAt",
            "migration",
            "relations",
            "enabledStableMints",
            "enabledMintWithEligibleTargetCount",
            "allEnabledMintTargetRowsPresent",
            "activeDistinctSupportedReserveCount",
            "activeSupportedReserveCatalogRowCount",
            "duplicateActiveSupportedReserveCount",
            "nonKaminoApiActiveSupportedReserveCount",
            "staleActiveSupportedReserveOver300SecondsCount",
            "oldestActiveSupportedReserveFetchedAt",
            "oldestActiveSupportedReserveAgeSeconds",
            "currentPointerCoverageCount",
            "verificationCoverageCount",
            "exactLatestViewCoverageCount",
            "eventHashObservedAtIdentityViolationCount",
            "verificationStateIdentityViolationCount",
            "latestViewIdentityViolationCount",
            "stateSlotGreaterThanVerifiedSlotCount",
            "immutableTapeExactRowCardinalityViolationCount",
            "latestViewRowCardinalityViolationCount",
            "observationFloorCoverageCount",
            "observationFloorIdentityViolationCount",
            "observationFloorFutureObservedAtCount",
            "staleObservationFloorOver90SecondsCount",
            "invalidObservationFloorStateCount",
            "currentStateBelowObservationFloorCount",
            "atOrBelowFloorExactHashAdmissionCount",
            "verificationAtOrBelowObservationFloorWithoutExactHashCount",
            "conflictingAtOrBelowFloorRoutableStateCount",
            "nonConfirmedCommitmentCount",
            "nonHttpCurrentStateCount",
            "nonHttpVerificationSourceCount",
            "futureCurrentStateObservedAtCount",
            "futureVerificationWatermarkCount",
            "warningOver90SecondsCount",
            "hardExpiredOver240SecondsCount",
            "oldestVerificationAgeSeconds",
            "coverageQueryMilliseconds",
            "safeTargetQueryMilliseconds",
            "topVerifiedSafeTargets",
            "readError",
            "pass",
        ],
    ) && value.get("migration").is_some_and(|migration| {
        exact_object_keys(
            migration,
            &[
                "version",
                "expectedName",
                "sourceChecksum",
                "appliedRowCount",
                "appliedName",
                "appliedChecksum",
                "appliedAt",
            ],
        )
    }) && value.get("relations").is_some_and(|relations| {
        exact_object_keys(
            relations,
            &[
                "migrationLedger",
                "supportedReserves",
                "reserveUpdates",
                "reserveCurrentStates",
                "reserveConfirmedObservationIdSequence",
                "reserveConfirmedObservationFloors",
                "reserveConfirmedVerifications",
                "latestVerifiedReserveUpdates",
            ],
        )
    }) && value
        .get("topVerifiedSafeTargets")
        .and_then(Value::as_array)
        .is_some_and(|targets| {
            targets
                .iter()
                .all(|target| exact_object_keys(target, TARGET_KEYS))
        })
}

fn queue_measurement_schema_known(value: &Value) -> bool {
    const STATUS_KEYS: &[&str] = &[
        "opportunity_state",
        "opportunity_count",
        "principal_usd_micros",
        "annual_yield_gain_usd_micros",
        "yield_gain_usd_micros_per_hour",
        "oldest_age_seconds",
        "oldest_state_age_seconds",
        "expired_lease_count",
        "pending_outbox_count",
        "pending_submission_count",
        "pending_compiled_fee_lamports",
        "expiry_check_pending_count",
        "effect_ambiguous_count",
        "oldest_pending_submission_age_seconds",
        "sender_submission_count",
        "oldest_sender_state_age_seconds",
        "confirmer_submission_count",
        "oldest_confirmer_state_age_seconds",
        "reconciler_submission_count",
        "oldest_reconciler_state_age_seconds",
        "planner_last_seen_age_seconds",
        "full_sweep_age_seconds",
        "complete_frontier",
        "observed_vault_count",
        "planned_opportunity_count",
        "planned_selected_count",
        "planned_deferred_count",
        "latest_market_epoch_id",
        "latest_market_epoch_age_seconds",
        "latest_market_epoch_expires_in_seconds",
        "latest_market_epoch_expired",
        "planner_epoch_matches_latest",
        "waiting_alt_opportunity_count",
        "waiting_alt_principal_usd_micros",
        "waiting_alt_yield_gain_usd_micros_per_hour",
        "oldest_waiting_alt_state_age_seconds",
        "ready_opportunity_count",
        "ready_principal_usd_micros",
        "ready_yield_gain_usd_micros_per_hour",
        "oldest_ready_state_age_seconds",
        "current_epoch_opportunity_count",
        "current_epoch_principal_usd_micros",
        "current_epoch_recoverable_yield_usd_micros_per_hour",
        "current_epoch_submitted_within_10s_yield_ppm",
        "current_epoch_submitted_within_2m_yield_ppm",
        "current_epoch_submitted_within_10m_yield_ppm",
        "current_epoch_confirmed_within_30s_yield_ppm",
        "current_epoch_submission_p95_milliseconds",
        "current_epoch_confirmation_p95_milliseconds",
        "current_epoch_compiled_fee_lamports",
    ];
    const TOP_KEYS: &[&str] = &[
        "id",
        "vault_id",
        "source_reserve",
        "target_reserve",
        "liquidity_mint",
        "principal_usd_micros",
        "annual_yield_gain_usd_micros",
        "expected_net_gain_usd_micros",
        "economic_priority",
        "opportunity_state",
        "terminal_reason",
        "state_age_seconds",
        "first_submitted_at",
        "first_confirmed_at",
    ];
    exact_object_keys(
        value,
        &[
            "available",
            "capturedAt",
            "statusRows",
            "activeDecisionsByStatus",
            "activeDecisionCount",
            "staleActiveDecisionCount",
            "duplicateActiveVaultMovementCount",
            "materialStuckOverTenMinutesCount",
            "targetCapacityOversubscriptionCount",
            "highValueOrderingInversionCount",
            "topCurrentEpochOpportunities",
        ],
    ) && value
        .get("statusRows")
        .and_then(Value::as_array)
        .is_some_and(|rows| rows.iter().all(|row| exact_object_keys(row, STATUS_KEYS)))
        && value
            .get("topCurrentEpochOpportunities")
            .and_then(Value::as_array)
            .is_some_and(|rows| rows.iter().all(|row| exact_object_keys(row, TOP_KEYS)))
}

fn position_measurement_schema_known(value: &Value) -> bool {
    let position_shape_known = |position: &Value| {
        exact_object_keys(
            position,
            &[
                "reserve",
                "liquidityMint",
                "amountRaw",
                "amountUsdc",
                "vaultCount",
                "vaultIds",
                "oldestObservedAt",
                "newestObservedAt",
                "minimumObservedSlot",
                "maximumObservedSlot",
                "freshnessMaximumAgeSeconds",
                "staleRowCount",
                "freshForBaseline",
            ],
        )
    };
    exact_object_keys(
        value,
        &[
            "available",
            "mainUsdcCohort",
            "mainUsdc",
            "globalMainUsdc",
            "reserveAggregates",
            "largestEligibleVaults",
        ],
    ) && value.get("mainUsdcCohort").is_some_and(|cohort| {
        exact_object_keys(
            cohort,
            &[
                "standardPolicyPubkey",
                "routeMode",
                "reserve",
                "market",
                "liquidityMint",
                "enabledStableMints",
                "vaultCount",
                "vaultIds",
            ],
        )
    }) && value.get("mainUsdc").is_some_and(position_shape_known)
        && value
            .get("globalMainUsdc")
            .is_some_and(position_shape_known)
        && value
            .get("reserveAggregates")
            .and_then(Value::as_array)
            .is_some_and(|rows| {
                rows.iter().all(|row| {
                    exact_object_keys(
                        row,
                        &[
                            "reserve",
                            "liquidity_mint",
                            "amount_raw",
                            "vault_count",
                            "oldest_observed_at",
                            "newest_observed_at",
                            "minimum_observed_slot",
                            "maximum_observed_slot",
                        ],
                    )
                })
            })
        && largest_account_measurement_schema_known(
            value.get("largestEligibleVaults").unwrap_or(&Value::Null),
        )
}

fn largest_account_measurement_schema_known(value: &Value) -> bool {
    const POSITION_KEYS: &[&str] = &[
        "reserve",
        "market",
        "liquidityMint",
        "amountRaw",
        "principalUsdMicros",
        "supplyApyBps",
    ];
    const VAULT_KEYS: &[&str] = &[
        "rank",
        "vaultId",
        "principalUsdMicros",
        "oldestObservedAt",
        "newestObservedAt",
        "minimumObservedSlot",
        "maximumObservedSlot",
        "positions",
        "bestReserve",
        "bestMarket",
        "bestLiquidityMint",
        "bestSupplyApyBps",
        "principalAtBestReserve",
        "opportunityId",
        "opportunityState",
        "opportunityTargetReserve",
        "estimatedEdgeBps",
        "expectedNetGainUsdMicros",
        "movedSubmissionId",
        "movedReconciledAt",
        "classification",
    ];
    exact_object_keys(
        value,
        &[
            "available",
            "cluster",
            "cutoverAt",
            "optimizerEpochId",
            "optimizerEpochKey",
            "optimizerEpochExpiresAt",
            "fullSweepCompletedAt",
            "completeFrontier",
            "rankedCount",
            "rankedPrincipalUsdMicros",
            "coveredPrincipalUsdMicros",
            "coveragePpm",
            "minimumCoveragePpm",
            "topThreeBlockedCount",
            "movedCount",
            "vaults",
            "pass",
        ],
    ) && value
        .get("vaults")
        .and_then(Value::as_array)
        .is_some_and(|vaults| {
            vaults.iter().all(|vault| {
                exact_object_keys(vault, VAULT_KEYS)
                    && vault
                        .get("positions")
                        .and_then(Value::as_array)
                        .is_some_and(|positions| {
                            positions
                                .iter()
                                .all(|position| exact_object_keys(position, POSITION_KEYS))
                        })
            })
        })
}

fn reconciled_volume_measurement_schema_known(value: &Value) -> bool {
    let snapshot_known = |snapshot: &Value| {
        exact_object_keys(
            snapshot,
            &[
                "movementCount",
                "amountRaw",
                "principalUsdMicros",
                "newestReconciledAt",
                "uniqueSubmissionCount",
                "uniqueOpportunityCount",
                "uniqueDecisionCount",
                "uniqueSignatureCount",
            ],
        )
    };
    exact_object_keys(
        value,
        &[
            "baseline",
            "current",
            "currentIdentityExact",
            "delta",
            "postCutoverMovementCount",
            "postCutoverAmountRaw",
            "postCutoverPrincipalUsdMicros",
            "pass",
        ],
    ) && value.get("current").is_some_and(snapshot_known)
        && value
            .get("baseline")
            .is_some_and(|snapshot| snapshot.is_null() || snapshot_known(snapshot))
        && value
            .get("delta")
            .is_some_and(|snapshot| snapshot.is_null() || snapshot_known(snapshot))
}

fn movement_measurement_schema_known(value: &Value) -> bool {
    const MOVEMENT_KEYS: &[&str] = &[
        "submissionId",
        "opportunityId",
        "decisionId",
        "opportunityDecisionId",
        "vaultId",
        "decisionVaultId",
        "signature",
        "submissionState",
        "opportunityState",
        "decisionStatus",
        "routeKind",
        "decisionRouteKind",
        "decisionSourceKind",
        "plannerSourceKind",
        "opportunityOptimizerEpochId",
        "submissionOptimizerEpochId",
        "optimizerEpochFingerprint",
        "optimizerEpochExpiresAt",
        "submissionOptimizerEpochEvidence",
        "optimizerEpochIdentityExact",
        "opportunitySourceSnapshotId",
        "submissionSourceSnapshotId",
        "sourceReserve",
        "decisionSourceSnapshotId",
        "decisionSourceReserve",
        "decisionSignature",
        "decisionConfirmedSlot",
        "decisionPostSnapshotId",
        "targetReserve",
        "decisionTargetReserve",
        "liquidityMint",
        "decisionLiquidityMint",
        "amountRaw",
        "decisionAmountRaw",
        "executedAmountRaw",
        "plannerExecutionPlan",
        "decisionExecutionPlan",
        "routeIdentityExact",
        "principalUsdMicros",
        "estimatedEdgeBps",
        "expectedNetGainUsdMicros",
        "economicPriority",
        "estimatedCostLamports",
        "compiledFeeLamports",
        "conservativeSolPriceUsdMicros",
        "compiledFeeUsdMicros",
        "feeFractionPpm",
        "feeFractionCapPpm",
        "economicPass",
        "createdAt",
        "submittedSlot",
        "submittedAt",
        "confirmedAt",
        "reconciledAt",
        "confirmedSlot",
        "reconciledSlot",
        "broadcastCount",
        "lastBroadcastAt",
        "lastValidBlockHeight",
        "expiryObservedBlockHeight",
        "effectCheckSlot",
        "lastStatusCheckedAt",
        "sourceSnapshotId",
        "sourceSnapshotVaultId",
        "sourceSnapshotContext",
        "postSnapshotId",
        "postSnapshotVaultId",
        "postSnapshotContext",
        "preTargetSnapshotId",
        "preTargetSnapshotVaultId",
        "preTargetSnapshotContext",
        "preTargetPlanningMetadata",
        "postSnapshotObservedSlot",
        "postSnapshotObservedAt",
        "postSnapshotAtOrAboveConfirmation",
        "preTargetSnapshotObservedSlot",
        "preTargetSnapshotObservedAt",
        "preSourceAmountRaw",
        "postSourceAmountRaw",
        "preTargetLiquidityMint",
        "preTargetHasValue",
        "preTargetAmountRaw",
        "postTargetLiquidityMint",
        "postTargetHasValue",
        "postTargetAmountRaw",
        "postTargetPlanningMetadata",
        "sourceDecreasedAndTargetIncreased",
        "idleTokenAccount",
        "plannerIdleTokenAccount",
        "preIdleSourceAmountRaw",
        "plannerPreIdleSourceAmountRaw",
        "preIdleSourceObservedSlot",
        "plannerPreIdleSourceObservedSlot",
        "preIdleSourceObservedAt",
        "plannerPreIdleSourceObservedAt",
        "idlePlanIdentityExact",
        "idlePlanEvidenceExact",
        "postIdleSourceTokenAccount",
        "postIdleSourceAmountRaw",
        "postIdleSourceObservedSlot",
        "postIdleSourceObservedAt",
        "idleSourceDecreasedAndTargetIncreased",
        "routeEffectProven",
        "rpcFound",
        "rpcFinalized",
        "rpcSuccessful",
        "rpcSlot",
        "finalizedSuccess",
        "terminalOutcomeSafe",
    ];
    exact_object_keys(
        value,
        &[
            "available",
            "cutoverAt",
            "rpcFinalityAvailable",
            "rpcReadError",
            "rpcFinalizedBlockHeight",
            "rpcFinalizedSlot",
            "submissionCount",
            "nonterminalSubmissionCount",
            "effectAmbiguousCount",
            "reconciledMovementCount",
            "reconciledAmountRaw",
            "reconciledPrincipalUsdMicros",
            "reconciledReserveMovementCount",
            "reconciledIdleDepositCount",
            "fullyFinalizedAndReconciledEffectCount",
            "economicFailureCount",
            "unsafeTerminalOutcomeCount",
            "databaseDeadlockCount",
            "duplicateMovementCount",
            "reconciledVolume",
            "mainUsdc",
            "movements",
            "pass",
            "currentEpochSlos",
            "movementSlos",
        ],
    ) && value
        .get("reconciledVolume")
        .is_some_and(reconciled_volume_measurement_schema_known)
        && value.get("mainUsdc").is_some_and(|main| {
            exact_object_keys(
                main,
                &[
                    "reserve",
                    "baselineCollectedAt",
                    "baselineAmountRaw",
                    "baselineCohortVaultCount",
                    "baselineCohortVaultIds",
                    "postBaselineCohortDepositAmountRaw",
                    "currentBaselineCohortAmountRaw",
                    "currentRouteableAmountRaw",
                    "confirmedOptimizerOutflowRaw",
                    "confirmedOptimizerInflowRaw",
                    "confirmedOptimizerNetOutflowRaw",
                    "baselineCohortConfirmedOptimizerOutflowRaw",
                    "baselineCohortConfirmedOptimizerInflowRaw",
                    "baselineCohortConfirmedOptimizerNetOutflowRaw",
                    "depositAdjustedReductionRaw",
                    "reductionAfterDepositsCoversConfirmedNetOutflow",
                ],
            )
        })
        && value.get("currentEpochSlos").is_some_and(|slos| {
            exact_object_keys(
                slos,
                &[
                    "currentEpochOpportunityCount",
                    "submissionP95Milliseconds",
                    "submissionP95LimitMilliseconds",
                    "confirmationP95Milliseconds",
                    "confirmationP95LimitMilliseconds",
                    "submittedWithinTwoMinutesYieldPpm",
                    "submittedWithinTwoMinutesMinimumYieldPpm",
                    "submittedWithinTenMinutesYieldPpm",
                    "submittedWithinTenMinutesMinimumYieldPpm",
                    "pass",
                ],
            )
        })
        && value.get("movementSlos").is_some_and(|slos| {
            exact_object_keys(
                slos,
                &[
                    "basis",
                    "reconciledMovementCount",
                    "submissionTimestampCount",
                    "reconciliationTimestampCount",
                    "maximumSignedToSubmittedMilliseconds",
                    "maximumSignedToReconciledMilliseconds",
                    "submissionLimitMilliseconds",
                    "reconciliationLimitMilliseconds",
                    "pass",
                ],
            )
        })
        && value
            .get("movements")
            .and_then(Value::as_array)
            .is_some_and(|rows| rows.iter().all(|row| exact_object_keys(row, MOVEMENT_KEYS)))
}

fn alt_repair_measurement_schema_known(value: &Value) -> bool {
    exact_object_keys(
        value,
        &[
            "available",
            "cluster",
            "finalizedRpcSlot",
            "altProgramId",
            "standardPolicyPubkey",
            "activeOrReferencedTableCount",
            "activeOrReferencedVerifiedCount",
            "activeOrReferencedWrongOwnerCount",
            "activeOrReferencedAuthorityMismatchCount",
            "activeOrReferencedPrefixMismatchCount",
            "damagedTableCount",
            "damagedNonAllocatingCount",
            "damagedActiveOrPreparingBindingCount",
            "damagedRunnableOperationCount",
            "damagedRouteDependencyCount",
            "historicalTerminalOperationCount",
            "terminalOperationWithRepairEvidenceCount",
            "terminalOperationMissingRepairEvidenceCount",
            "affectedTerminalRequestCount",
            "affectedRequestSatisfiedOrSuccessorCount",
            "unresolvedActiveTerminalRequestCount",
            "validPrefixTableCount",
            "validPrefixPreservedCount",
            "staleSuffixRetryCount",
            "newLegacyOrExactRouteTableCount",
            "activeAltMutatorCount",
            "budgetWindowSeconds",
            "budgetMaximumLamports",
            "budgetSpentLamports",
        ],
    )
}

fn production_migration_subcheck(binding: &ProductionEvidenceBinding) -> Subcheck {
    let evidence = &binding.artifact.measurements.database.migrations;
    let rows = evidence.get("required").and_then(Value::as_array);
    let ledger_exists = value_bool(evidence, "ledgerExists") == Some(true);
    let mut row_evidence = Vec::new();
    let schema_known = migration_measurement_schema_known(evidence);
    let mut all_match = schema_known
        && ledger_exists
        && rows.is_some_and(|rows| rows.len() == VERIFIED_MIGRATIONS.len());
    for (version, expected_name, file_name) in VERIFIED_MIGRATIONS {
        let path = binding
            .repository_root
            .join("crates/loyal-yield-store/migrations")
            .join(file_name);
        let expected_checksum = fs::read(path)
            .map(|bytes| sha256_hex(&bytes))
            .unwrap_or_default();
        let matching = rows
            .into_iter()
            .flatten()
            .filter(|row| value_i64(row, "version") == Some(version))
            .collect::<Vec<_>>();
        let row_matches = matching.len() == 1
            && !expected_checksum.is_empty()
            && value_string(matching[0], "name") == Some(expected_name)
            && value_string(matching[0], "appliedName") == Some(expected_name)
            && value_string(matching[0], "sourceChecksum") == Some(expected_checksum.as_str())
            && value_string(matching[0], "appliedChecksum") == Some(expected_checksum.as_str())
            && value_bool(matching[0], "applied") == Some(true)
            && matching[0]
                .get("appliedAt")
                .and_then(Value::as_str)
                .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
                .is_some();
        all_match &= row_matches;
        row_evidence.push(json!({
            "version": version,
            "expectedName": expected_name,
            "expectedChecksum": expected_checksum,
            "matchingRows": matching.len(),
            "matches": row_matches,
        }));
    }
    subcheck(
        "production_migrations_23_through_31_match_repository_bytes",
        all_match,
        json!({
            "ledgerExists": ledger_exists,
            "measurementSchemaKnown": schema_known,
            "required": row_evidence,
            "embeddedPassIgnored": evidence.get("pass"),
        }),
    )
}

fn production_render_subcheck(
    binding: &ProductionEvidenceBinding,
    runtime: Option<&RuntimeEvidenceV1>,
) -> Subcheck {
    let expected = match production_expected_services(&binding.repository_root) {
        Ok(expected) => expected,
        Err(error) => {
            return subcheck(
                "six_live_roles_match_clean_blueprint_and_one_digest",
                false,
                json!({"error": error.to_string()}),
            )
        }
    };
    let render = &binding.artifact.measurements.render;
    let schema_known = render_measurement_schema_known(render);
    let render_captured_at = value_string(render, "capturedAt")
        .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
        .map(|value| value.with_timezone(&Utc));
    let capture_is_fresh = render_captured_at.is_some_and(|captured_at| {
        captured_at >= binding.artifact.collection_started_at
            && captured_at <= binding.artifact.captured_at
            && binding
                .artifact
                .captured_at
                .signed_duration_since(captured_at)
                <= chrono::Duration::seconds(PRODUCTION_COMPONENT_MAX_LAG_SECONDS)
    });
    let scope_nonce = value_string(render, "scopeFingerprintNonce").unwrap_or_default();
    let scope_nonce_valid =
        scope_nonce.len() == 64 && scope_nonce.bytes().all(|byte| byte.is_ascii_hexdigit());
    let standard_policy_identity_valid = env::var("POLICY_KEYPAIR")
        .ok()
        .and_then(|value| loyal_yield_orchestrator::keypair_from_string(&value).ok())
        .is_some_and(|keypair| keypair.pubkey().to_string() == STANDARD_POLICY_PUBKEY);
    let roles = render.get("roles").and_then(Value::as_array);
    let mut role_evidence = Vec::new();
    let mut digests = BTreeSet::new();
    let mut all_roles_match = schema_known
        && value_bool(render, "available") == Some(true)
        && roles.is_some_and(|roles| roles.len() == expected.len());
    let mut sender_started = Vec::new();
    for expected_role in &expected {
        let matching = roles
            .into_iter()
            .flatten()
            .filter(|role| value_string(role, "name") == Some(expected_role.name.as_str()))
            .collect::<Vec<_>>();
        let role = matching.first().copied();
        let live_env = role
            .and_then(|role| role.get("envKeys"))
            .and_then(Value::as_array)
            .map(|keys| {
                keys.iter()
                    .filter_map(Value::as_str)
                    .map(str::to_owned)
                    .collect::<BTreeSet<_>>()
            });
        let blueprint_env = role
            .and_then(|role| role.get("blueprintEnvKeys"))
            .and_then(Value::as_array)
            .map(|keys| {
                keys.iter()
                    .filter_map(Value::as_str)
                    .map(str::to_owned)
                    .collect::<BTreeSet<_>>()
            });
        let expected_live_env = role_service_scoped_env_keys(&expected_role.env_keys);
        let expected_fingerprints = expected_env_fingerprints(
            scope_nonce,
            role_scope_value_keys(&expected_role.name, &expected_role.env_keys),
        );
        let reported_fingerprints =
            role.and_then(|role| reported_env_fingerprints(role.get("envValueFingerprints")));
        let deploy = role.and_then(|role| role.get("latestDeploy"));
        let digest = deploy.and_then(|deploy| value_string(deploy, "imageDigest"));
        if let Some(digest) = digest {
            digests.insert(digest.to_owned());
        }
        let started_at = deploy
            .and_then(|deploy| value_string(deploy, "startedAt"))
            .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
            .map(|value| value.with_timezone(&Utc));
        if matches!(
            expected_role.name.as_str(),
            "loyal-fleet-route-revalidator" | "loyal-fleet-route-executor"
        ) {
            if let Some(started_at) = started_at {
                sender_started.push(started_at);
            }
        }
        let role_matches = matching.len() == 1
            && role.and_then(|role| value_bool(role, "present")) == Some(true)
            && role.and_then(|role| value_string(role, "suspended")) == Some("not_suspended")
            && role.and_then(|role| value_string(role, "runtime")) == Some("image")
            && role.and_then(|role| value_string(role, "plan"))
                == Some(expected_role.plan.as_str())
            && role.and_then(|role| value_string(role, "image"))
                == Some(expected_role.image.as_str())
            && role.and_then(|role| value_string(role, "command"))
                == Some(expected_role.command.as_str())
            && role.and_then(|role| value_string(role, "preDeployCommand"))
                == Some(expected_role.pre_deploy_command.as_str())
            && live_env.as_ref() == Some(&expected_live_env)
            && blueprint_env.as_ref() == Some(&expected_role.env_keys)
            && role.and_then(|role| value_bool(role, "observabilityBoundaryPasses")) == Some(true)
            && expected_fingerprints.is_some()
            && reported_fingerprints == expected_fingerprints
            && role
                .and_then(|role| value_i64(role, "numInstances"))
                .is_some_and(|count| count > 0)
            && deploy.and_then(|deploy| value_string(deploy, "status")) == Some("live")
            && deploy.and_then(|deploy| value_string(deploy, "imageRef"))
                == Some(expected_role.image.as_str())
            && digest.is_some_and(valid_sha256_digest)
            && runtime.is_some_and(|runtime| {
                digest == Some(runtime.wiring.light_linux_amd64_manifest_digest.as_str())
            })
            && deploy.and_then(|deploy| value_string(deploy, "registryCredential"))
                == Some("loyal-ghcr")
            && started_at.is_some();
        all_roles_match &= role_matches;
        role_evidence.push(json!({
            "name": expected_role.name,
            "matchingRows": matching.len(),
            "matchesBlueprintAndLiveDeploy": role_matches,
            "image": role.and_then(|role| value_string(role, "image")),
            "command": role.and_then(|role| value_string(role, "command")),
            "envKeys": live_env,
            "blueprintEnvKeys": blueprint_env,
            "envFingerprintsMatchLocalScope": reported_fingerprints == expected_fingerprints,
            "deployDigest": digest,
            "deployStartedAt": started_at,
        }));
    }
    let expected_images = expected
        .iter()
        .map(|service| service.image.as_str())
        .collect::<BTreeSet<_>>();
    let one_image_and_digest = expected_images.len() == 1 && digests.len() == 1;
    let serial = render.get("serialMonitor");
    let serial_present = serial.and_then(|value| value_bool(value, "present")) == Some(true);
    let serial_command = serial
        .and_then(|value| value_string(value, "command"))
        .unwrap_or_default();
    let serial_incapable = !serial_present
        || serial.and_then(|value| value_bool(value, "suspended")) == Some(true)
        || serial.and_then(|value| value_bool(value, "scaledToZero")) == Some(true)
        || !serial_command
            .split_ascii_whitespace()
            .any(|token| token == "--execute");
    let latest_serial_suspension = serial
        .and_then(|value| value.get("suspensionEvents"))
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter(|event| value_string(event, "type").is_some_and(|kind| kind.contains("suspend")))
        .filter_map(|event| {
            value_string(event, "timestamp")
                .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
                .map(|value| value.with_timezone(&Utc))
        })
        .max();
    let first_sender_started = sender_started.into_iter().min();
    let serial_preceded_senders = match (
        serial_present,
        latest_serial_suspension,
        first_sender_started,
    ) {
        (false, _, Some(_)) => true,
        (true, Some(suspended), Some(sender)) => suspended <= sender,
        _ => false,
    };
    let runtime_image_bound = runtime.is_some_and(|runtime| {
        expected_images.len() == 1
            && expected_images.contains(runtime.wiring.probed_container_image_reference.as_str())
            && digests.len() == 1
            && digests.contains(&runtime.wiring.light_linux_amd64_manifest_digest)
    });
    let passed = schema_known
        && capture_is_fresh
        && scope_nonce_valid
        && standard_policy_identity_valid
        && all_roles_match
        && one_image_and_digest
        && runtime_image_bound
        && serial_incapable
        && serial_preceded_senders;
    subcheck(
        "six_live_roles_match_clean_blueprint_and_one_digest",
        passed,
        json!({
            "roles": role_evidence,
            "measurementSchemaKnown": schema_known,
            "renderCapturedAt": render_captured_at,
            "componentMaximumLagSeconds": PRODUCTION_COMPONENT_MAX_LAG_SECONDS,
            "captureIsFresh": capture_is_fresh,
            "scopeFingerprintNonceValid": scope_nonce_valid,
            "standardPolicyIdentityValid": standard_policy_identity_valid,
            "oneBlueprintImage": expected_images.len() == 1,
            "liveDeployDigests": digests,
            "oneLiveDigest": digests.len() == 1,
            "runtimeImageDigestAndReferenceBound": runtime_image_bound,
            "serialPresent": serial_present,
            "serialCurrentlyIncapableOfSending": serial_incapable,
            "serialSuspendedAt": latest_serial_suspension,
            "firstFleetSenderDeployStartedAt": first_sender_started,
            "serialSuspendedBeforeFleetSenderStarted": serial_preceded_senders,
            "embeddedPassesIgnored": {
                "allRolesMatch": render.get("allRolesMatch"),
                "oneImmutableDigest": render.get("oneImmutableDigest"),
                "serialOrdering": render.get("serialSuspendedBeforeFleetSenderStarted"),
                "pass": render.get("pass"),
            },
        }),
    )
}

fn production_confirmed_market_data_plane_subcheck(
    binding: &ProductionEvidenceBinding,
    runtime: Option<&RuntimeEvidenceV1>,
) -> Subcheck {
    let timescale = &binding.artifact.measurements.market_data_plane.timescale;
    let render = &binding.artifact.measurements.render;
    let expected_light = production_expected_services(&binding.repository_root);
    let expected_monitor = production_expected_kamino_monitor(&binding.repository_root);
    let (Ok(expected_light), Ok(expected_monitor)) = (expected_light, expected_monitor) else {
        return subcheck(
            "confirmed_kamino_market_data_plane_is_live_complete_and_fresh",
            false,
            json!({"error": "cannot read exact market-data or fleet Blueprint contract"}),
        );
    };

    let timescale_schema_known = market_timescale_measurement_schema_known(timescale);
    let render_schema_known = render_measurement_schema_known(render);
    let migration = timescale.get("migration").unwrap_or(&Value::Null);
    let relations = timescale.get("relations").unwrap_or(&Value::Null);
    let migration_path = binding.repository_root.join(
        "crates/loyal-timescale-migrations/migrations/0005_kamino_confirmed_state_verification.sql",
    );
    let expected_migration_checksum = fs::read(migration_path)
        .map(|bytes| sha256_hex(&bytes))
        .unwrap_or_default();
    let migration_applied_at = migration
        .get("appliedAt")
        .and_then(Value::as_str)
        .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
        .map(|value| value.with_timezone(&Utc));
    let migration_matches = !expected_migration_checksum.is_empty()
        && value_i64(migration, "version") == Some(TIMESCALE_MARKET_MIGRATION_VERSION)
        && value_string(migration, "expectedName") == Some(TIMESCALE_MARKET_MIGRATION_NAME)
        && value_string(migration, "sourceChecksum") == Some(expected_migration_checksum.as_str())
        && value_i64(migration, "appliedRowCount") == Some(1)
        && value_string(migration, "appliedName") == Some(TIMESCALE_MARKET_MIGRATION_NAME)
        && value_string(migration, "appliedChecksum") == Some(expected_migration_checksum.as_str())
        && migration_applied_at.is_some();
    let relations_complete = [
        "migrationLedger",
        "supportedReserves",
        "reserveUpdates",
        "reserveCurrentStates",
        "reserveConfirmedObservationIdSequence",
        "reserveConfirmedObservationFloors",
        "reserveConfirmedVerifications",
        "latestVerifiedReserveUpdates",
    ]
    .into_iter()
    .all(|key| value_bool(relations, key) == Some(true));
    let market_captured_at = timescale
        .get("capturedAt")
        .and_then(Value::as_str)
        .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
        .map(|value| value.with_timezone(&Utc));
    let capture_bound = market_captured_at.is_some_and(|market_captured_at| {
        market_captured_at >= binding.artifact.collection_started_at
            && market_captured_at <= binding.artifact.captured_at
            && binding
                .artifact
                .captured_at
                .signed_duration_since(market_captured_at)
                <= chrono::Duration::seconds(PRODUCTION_COMPONENT_MAX_LAG_SECONDS)
            && market_captured_at <= Utc::now() + PRODUCTION_EVIDENCE_MAX_FUTURE_SKEW
    });
    let migration_time_bound = migration_applied_at
        .zip(market_captured_at)
        .is_some_and(|(applied_at, captured_at)| applied_at <= captured_at);

    let active = value_i64(timescale, "activeDistinctSupportedReserveCount");
    let catalog_rows = value_i64(timescale, "activeSupportedReserveCatalogRowCount");
    let oldest_catalog_fetched_at =
        value_string(timescale, "oldestActiveSupportedReserveFetchedAt")
            .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
            .map(|value| value.with_timezone(&Utc));
    let oldest_catalog_age = value_i64(timescale, "oldestActiveSupportedReserveAgeSeconds");
    let catalog_complete_and_fresh = active.is_some_and(|count| count > 0)
        && catalog_rows == active
        && value_i64(timescale, "duplicateActiveSupportedReserveCount") == Some(0)
        && value_i64(timescale, "nonKaminoApiActiveSupportedReserveCount") == Some(0)
        && value_i64(timescale, "staleActiveSupportedReserveOver300SecondsCount") == Some(0)
        && oldest_catalog_age
            .is_some_and(|age| (0..=SUPPORTED_RESERVE_CATALOG_MAX_AGE_SECONDS).contains(&age))
        && oldest_catalog_fetched_at
            .zip(market_captured_at)
            .is_some_and(|(fetched_at, captured_at)| fetched_at <= captured_at);
    let current = value_i64(timescale, "currentPointerCoverageCount");
    let verifications = value_i64(timescale, "verificationCoverageCount");
    let exact_latest = value_i64(timescale, "exactLatestViewCoverageCount");
    let observation_floors = value_i64(timescale, "observationFloorCoverageCount");
    let full_active_coverage = active.is_some_and(|count| count > 0)
        && current == active
        && verifications == active
        && observation_floors == active
        && exact_latest == active;
    let zero_safety_errors = [
        "eventHashObservedAtIdentityViolationCount",
        "verificationStateIdentityViolationCount",
        "latestViewIdentityViolationCount",
        "stateSlotGreaterThanVerifiedSlotCount",
        "immutableTapeExactRowCardinalityViolationCount",
        "latestViewRowCardinalityViolationCount",
        "observationFloorIdentityViolationCount",
        "observationFloorFutureObservedAtCount",
        "staleObservationFloorOver90SecondsCount",
        "invalidObservationFloorStateCount",
        "verificationAtOrBelowObservationFloorWithoutExactHashCount",
        "conflictingAtOrBelowFloorRoutableStateCount",
        "nonConfirmedCommitmentCount",
        "nonHttpCurrentStateCount",
        "nonHttpVerificationSourceCount",
        "futureCurrentStateObservedAtCount",
        "futureVerificationWatermarkCount",
        "warningOver90SecondsCount",
        "hardExpiredOver240SecondsCount",
    ]
    .into_iter()
    .all(|key| value_i64(timescale, key) == Some(0));
    let oldest_age = value_i64(timescale, "oldestVerificationAgeSeconds");
    let freshness_bounded =
        oldest_age.is_some_and(|age| (0..=MARKET_VERIFICATION_WARNING_SECONDS).contains(&age));
    let query_durations_bounded = ["coverageQueryMilliseconds", "safeTargetQueryMilliseconds"]
        .into_iter()
        .all(|key| {
            value_i64(timescale, key).is_some_and(|milliseconds| {
                (0..=MARKET_EVIDENCE_QUERY_TIMEOUT_MILLISECONDS).contains(&milliseconds)
            })
        });

    let enabled_mints = timescale
        .get("enabledStableMints")
        .and_then(Value::as_array)
        .map(|mints| {
            mints
                .iter()
                .filter_map(Value::as_str)
                .map(str::to_owned)
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    let enabled_set = enabled_mints.iter().cloned().collect::<BTreeSet<_>>();
    let canonical_enabled_mints = supported_stable_mints();
    let position_enabled_mints = binding
        .artifact
        .measurements
        .database
        .positions
        .pointer("/mainUsdcCohort/enabledStableMints")
        .and_then(Value::as_array)
        .map(|mints| {
            mints
                .iter()
                .filter_map(Value::as_str)
                .map(str::to_owned)
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    let target_rows = timescale
        .get("topVerifiedSafeTargets")
        .and_then(Value::as_array);
    let target_mints = target_rows
        .into_iter()
        .flatten()
        .filter_map(|target| value_string(target, "liquidityMint"))
        .map(str::to_owned)
        .collect::<BTreeSet<_>>();
    let targets_exact = enabled_mints == canonical_enabled_mints
        && position_enabled_mints == canonical_enabled_mints
        && enabled_set.len() == enabled_mints.len()
        && target_rows.is_some_and(|rows| rows.len() == enabled_mints.len())
        && target_mints == enabled_set
        && target_rows.into_iter().flatten().all(|target| {
            let eligible_target_count = value_i64(target, "eligibleTargetCount");
            if eligible_target_count == Some(0) {
                const NULL_TARGET_KEYS: &[&str] = &[
                    "riskBaskets",
                    "reserve",
                    "market",
                    "supplyApy",
                    "totalSupplyUsdEstimate",
                    "reserveLastUpdateStale",
                    "stateEventId",
                    "accountDataHash",
                    "stateObservedAt",
                    "stateSlot",
                    "verifiedAt",
                    "verifiedSlot",
                    "stateSource",
                    "verificationCommitment",
                    "verificationSource",
                    "observationFloorSlot",
                    "observationFloorObservationId",
                    "observationFloorAccountDataHash",
                    "observationFloorStateValid",
                    "observationFloorSource",
                    "observationFloorSourceRank",
                    "observationFloorObservedAt",
                ];
                return NULL_TARGET_KEYS
                    .iter()
                    .all(|key| target.get(*key).is_some_and(Value::is_null));
            }
            let verified_at = target
                .get("verifiedAt")
                .and_then(Value::as_str)
                .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
                .map(|value| value.with_timezone(&Utc));
            let state_observed_at = target
                .get("stateObservedAt")
                .and_then(Value::as_str)
                .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
                .map(|value| value.with_timezone(&Utc));
            let observation_floor_observed_at = target
                .get("observationFloorObservedAt")
                .and_then(Value::as_str)
                .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
                .map(|value| value.with_timezone(&Utc));
            let hash = value_string(target, "accountDataHash").unwrap_or_default();
            let observation_floor_hash = value_string(target, "observationFloorAccountDataHash");
            let observation_floor_source =
                value_string(target, "observationFloorSource").unwrap_or_default();
            let expected_observation_floor_rank = match observation_floor_source {
                "laserstream_grpc" | "websocket" => Some(1),
                "http_snapshot" | "http_confirmed_refresh" => Some(2),
                _ => None,
            };
            let observation_floor_state_valid = value_bool(target, "observationFloorStateValid");
            let observation_floor_identity_valid =
                value_i64(target, "observationFloorObservationId").is_some_and(|id| id > 0)
                    && value_i64(target, "observationFloorSlot").is_some_and(|slot| slot >= 0)
                    && expected_observation_floor_rank.is_some()
                    && value_i64(target, "observationFloorSourceRank")
                        == expected_observation_floor_rank
                    && match observation_floor_state_valid {
                        Some(true) => observation_floor_hash.is_some_and(|floor_hash| {
                            floor_hash.len() == 64
                                && floor_hash.bytes().all(|byte| byte.is_ascii_hexdigit())
                        }),
                        Some(false) => observation_floor_hash.is_none(),
                        None => false,
                    }
                    && observation_floor_state_valid == Some(true);
            let floor_admission_valid = value_i64(target, "verifiedSlot")
                .zip(value_i64(target, "observationFloorSlot"))
                .is_some_and(|(verified_slot, floor_slot)| {
                    verified_slot > floor_slot
                        || (observation_floor_state_valid == Some(true)
                            && observation_floor_hash == Some(hash))
                });
            eligible_target_count.is_some_and(|count| count > 0)
                && target
                    .get("riskBaskets")
                    .and_then(Value::as_array)
                    .is_some_and(|baskets| {
                        baskets.iter().any(|basket| basket.as_str() == Some("safe"))
                    })
                && value_string(target, "reserve").is_some_and(|value| !value.trim().is_empty())
                && value_string(target, "market").is_some_and(|value| !value.trim().is_empty())
                && target
                    .get("supplyApy")
                    .and_then(Value::as_f64)
                    .is_some_and(|apy| apy.is_finite() && (0.0..0.5).contains(&apy))
                && target
                    .get("totalSupplyUsdEstimate")
                    .and_then(Value::as_f64)
                    .is_some_and(|supply| supply.is_finite() && supply > 100_000.0)
                && value_bool(target, "reserveLastUpdateStale") == Some(false)
                && value_i64(target, "stateEventId").is_some_and(|id| id > 0)
                && hash.len() == 64
                && hash.bytes().all(|byte| byte.is_ascii_hexdigit())
                && state_observed_at
                    .zip(market_captured_at)
                    .is_some_and(|(observed_at, captured_at)| observed_at <= captured_at)
                && value_i64(target, "stateSlot")
                    .zip(value_i64(target, "verifiedSlot"))
                    .is_some_and(|(state, verified)| state >= 0 && state <= verified)
                && value_string(target, "stateSource").is_some_and(|source| {
                    matches!(source, "http_snapshot" | "http_confirmed_refresh")
                })
                && value_string(target, "verificationCommitment") == Some("confirmed")
                && value_string(target, "verificationSource").is_some_and(|source| {
                    matches!(source, "http_snapshot" | "http_confirmed_refresh")
                })
                && observation_floor_identity_valid
                && floor_admission_valid
                && observation_floor_observed_at
                    .zip(market_captured_at)
                    .is_some_and(|(observed_at, captured_at)| {
                        observed_at <= captured_at
                            && captured_at.signed_duration_since(observed_at).num_seconds()
                                <= MARKET_VERIFICATION_WARNING_SECONDS
                    })
                && verified_at.is_some_and(|verified_at| {
                    market_captured_at.is_some_and(|captured_at| {
                        verified_at <= captured_at
                            && captured_at.signed_duration_since(verified_at).num_seconds()
                                <= MARKET_VERIFICATION_WARNING_SECONDS
                    })
                })
        });

    let monitor = render.get("marketMonitor").unwrap_or(&Value::Null);
    let live_env_keys = monitor
        .get("envKeys")
        .and_then(Value::as_array)
        .map(|keys| {
            keys.iter()
                .filter_map(Value::as_str)
                .map(str::to_owned)
                .collect::<BTreeSet<_>>()
        });
    let reported_blueprint_env_keys = monitor
        .get("blueprintEnvKeys")
        .and_then(Value::as_array)
        .map(|keys| {
            keys.iter()
                .filter_map(Value::as_str)
                .map(str::to_owned)
                .collect::<BTreeSet<_>>()
        });
    let required_monitor_env_keys = [
        "HELIUS_API_KEY",
        "KAMINO_API_BASE",
        "KAMINO_UPDATE_SOURCE",
        "LASERSTREAM_ENDPOINT",
        "RUST_LOG",
        "SOLANA_RPC_URL",
        "TIMESCALEDB_URL",
    ]
    .into_iter()
    .map(str::to_owned)
    .collect::<BTreeSet<_>>();
    let scope_nonce = value_string(render, "scopeFingerprintNonce").unwrap_or_default();
    let expected_monitor_fingerprints = expected_env_fingerprints(
        scope_nonce,
        monitor_scope_value_keys(&expected_monitor.env_keys),
    );
    let reported_monitor_fingerprints =
        reported_env_fingerprints(monitor.get("envValueFingerprints"));
    let deploy = monitor.get("latestDeploy").unwrap_or(&Value::Null);
    let monitor_deploy_started_at = deploy
        .get("startedAt")
        .and_then(Value::as_str)
        .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
        .map(|value| value.with_timezone(&Utc));
    let catalog_postdates_monitor_deploy = oldest_catalog_fetched_at
        .zip(monitor_deploy_started_at)
        .is_some_and(|(fetched_at, deploy_started_at)| fetched_at >= deploy_started_at);
    let monitor_exact = value_string(render, "heavyEnvironmentId")
        == Some(HEAVY_RENDER_ENVIRONMENT_ID)
        && value_string(monitor, "environmentId") == Some(HEAVY_RENDER_ENVIRONMENT_ID)
        && value_string(monitor, "serviceId") == Some(KAMINO_MONITOR_SERVICE_ID)
        && value_string(monitor, "name") == Some(KAMINO_MONITOR_SERVICE_NAME)
        && value_bool(monitor, "present") == Some(true)
        && value_string(monitor, "type") == Some("background_worker")
        && value_string(monitor, "suspended") == Some("not_suspended")
        && value_string(monitor, "runtime") == Some("image")
        && value_string(monitor, "plan") == Some(expected_monitor.plan.as_str())
        && value_i64(monitor, "numInstances").is_some_and(|count| count > 0)
        && expected_monitor.command == KAMINO_MONITOR_COMMAND
        && expected_monitor.pre_deploy_command == KAMINO_MONITOR_PREDEPLOY
        && value_string(monitor, "image") == Some(expected_monitor.image.as_str())
        && value_string(monitor, "command") == Some(KAMINO_MONITOR_COMMAND)
        && value_string(monitor, "preDeployCommand") == Some(KAMINO_MONITOR_PREDEPLOY)
        && required_monitor_env_keys.is_subset(&expected_monitor.env_keys)
        && live_env_keys.as_ref() == Some(&expected_monitor.env_keys)
        && reported_blueprint_env_keys.as_ref() == Some(&expected_monitor.env_keys)
        && value_bool(monitor, "observabilityBoundaryPasses") == Some(true)
        && expected_monitor_fingerprints.is_some()
        && reported_monitor_fingerprints == expected_monitor_fingerprints
        && value_string(deploy, "status") == Some("live")
        && value_string(deploy, "imageRef") == Some(expected_monitor.image.as_str())
        && value_string(deploy, "imageDigest").is_some_and(valid_sha256_digest)
        && runtime.is_some_and(|runtime| {
            value_string(deploy, "imageDigest")
                == Some(runtime.wiring.heavy_linux_amd64_manifest_digest.as_str())
                && runtime.wiring.probed_heavy_container_image_reference == expected_monitor.image
        })
        && value_string(deploy, "registryCredential") == Some("loyal-ghcr")
        && monitor_deploy_started_at.is_some();

    let light_images = expected_light
        .iter()
        .map(|service| service.image.as_str())
        .collect::<BTreeSet<_>>();
    let light_suffixes = light_images
        .iter()
        .filter_map(|image| image_commit_suffix(image))
        .collect::<BTreeSet<_>>();
    let light_suffix = (light_suffixes.len() == 1)
        .then(|| light_suffixes.iter().next().copied())
        .flatten();
    let laserstream_suffix = image_commit_suffix(&expected_monitor.image);
    let light_image = light_images.iter().next().copied().unwrap_or_default();
    let (light_checkout_bound, light_checkout_binding) =
        image_source_binding(&binding.repository_root, light_image);
    let (heavy_checkout_bound, heavy_checkout_binding) =
        image_source_binding(&binding.repository_root, &expected_monitor.image);
    let runtime_provenance_bound = runtime.is_some_and(|runtime| {
        Some(runtime.wiring.light_provenance_vcs_revision.as_str()) == light_suffix
            && Some(runtime.wiring.heavy_provenance_vcs_revision.as_str()) == laserstream_suffix
            && runtime.wiring.light_provenance_vcs_source == IMAGE_PROVENANCE_SOURCE
            && runtime.wiring.heavy_provenance_vcs_source == IMAGE_PROVENANCE_SOURCE
    });
    let image_source_bound = light_images.len() == 1
        && light_images.iter().all(|image| {
            image.starts_with("ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-")
        })
        && expected_monitor
            .image
            .starts_with("ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-")
        && light_suffix.is_some()
        && light_suffix == laserstream_suffix
        && light_checkout_bound
        && heavy_checkout_bound
        && runtime_provenance_bound
        && value_string(render, "lightImageCommitSuffix") == light_suffix
        && value_string(render, "laserstreamImageCommitSuffix") == laserstream_suffix;

    let passed = timescale_schema_known
        && render_schema_known
        && value_bool(timescale, "available") == Some(true)
        && timescale.get("readError").is_some_and(Value::is_null)
        && capture_bound
        && migration_matches
        && migration_time_bound
        && relations_complete
        && catalog_complete_and_fresh
        && catalog_postdates_monitor_deploy
        && full_active_coverage
        && zero_safety_errors
        && freshness_bounded
        && query_durations_bounded
        && targets_exact
        && monitor_exact
        && image_source_bound;
    subcheck(
        "confirmed_kamino_market_data_plane_is_live_complete_and_fresh",
        passed,
        json!({
            "timescaleMeasurementSchemaKnown": timescale_schema_known,
            "renderMeasurementSchemaKnown": render_schema_known,
            "repeatableReadCaptureBoundToArtifact": capture_bound,
            "migrationMatchesRepositoryBytes": migration_matches,
            "migrationAppliedAtNotFuture": migration_time_bound,
            "expectedMigrationChecksum": expected_migration_checksum,
            "relationsComplete": relations_complete,
            "activeCatalogRows": catalog_rows,
            "activeCatalogCompleteAndFresh": catalog_complete_and_fresh,
            "oldestActiveCatalogFetchedAt": oldest_catalog_fetched_at,
            "oldestActiveCatalogAgeSeconds": oldest_catalog_age,
            "catalogMaximumAgeSeconds": SUPPORTED_RESERVE_CATALOG_MAX_AGE_SECONDS,
            "catalogPostdatesMonitorDeploy": catalog_postdates_monitor_deploy,
            "activeSupportedReserves": active,
            "currentPointerCoverage": current,
            "verificationCoverage": verifications,
            "observationFloorCoverage": observation_floors,
            "exactLatestViewCoverage": exact_latest,
            "fullActiveCoverage": full_active_coverage,
            "zeroIdentitySlotCommitmentSourceFutureAndExpiryErrors": zero_safety_errors,
            "invalidObservationFloorStateCount": value_i64(timescale, "invalidObservationFloorStateCount"),
            "currentStateBelowObservationFloorCount": value_i64(timescale, "currentStateBelowObservationFloorCount"),
            "atOrBelowFloorExactHashAdmissionCount": value_i64(timescale, "atOrBelowFloorExactHashAdmissionCount"),
            "oldestVerificationAgeSeconds": oldest_age,
            "freshnessWarningSeconds": MARKET_VERIFICATION_WARNING_SECONDS,
            "queryDurationsBounded": query_durations_bounded,
            "queryTimeoutMilliseconds": MARKET_EVIDENCE_QUERY_TIMEOUT_MILLISECONDS,
            "enabledStableMints": enabled_mints,
            "canonicalProductionStableMints": canonical_enabled_mints,
            "positionCohortStableMints": position_enabled_mints,
            "topVerifiedSafeTargetsExact": targets_exact,
            "marketMonitorExact": monitor_exact,
            "lightImageCommitSuffix": light_suffix,
            "laserstreamImageCommitSuffix": laserstream_suffix,
            "imageSourceCommitExact": image_source_bound,
            "lightCheckoutBinding": light_checkout_binding,
            "heavyCheckoutBinding": heavy_checkout_binding,
            "runtimeProvenanceBound": runtime_provenance_bound,
            "embeddedPassesIgnored": {
                "timescale": timescale.get("pass"),
                "monitor": monitor.get("matches"),
                "renderImageSuffixes": render.get("imageCommitSuffixesMatch"),
                "render": render.get("pass"),
            },
        }),
    )
}

fn production_alt_repair_subcheck(binding: &ProductionEvidenceBinding) -> Subcheck {
    let Some(evidence) = binding.artifact.measurements.database.alt_repair.as_ref() else {
        return subcheck(
            "production_alt_damage_is_fenced_repaired_and_budgeted",
            false,
            json!({"error": "measurements.database.altRepair is missing"}),
        );
    };
    let schema_known = alt_repair_measurement_schema_known(evidence);
    let active = value_i64(evidence, "activeOrReferencedTableCount");
    let verified = value_i64(evidence, "activeOrReferencedVerifiedCount");
    let damaged = value_i64(evidence, "damagedTableCount");
    let damaged_nonallocating = value_i64(evidence, "damagedNonAllocatingCount");
    let terminal = value_i64(evidence, "historicalTerminalOperationCount");
    let terminal_repaired = value_i64(evidence, "terminalOperationWithRepairEvidenceCount");
    let affected_requests = value_i64(evidence, "affectedTerminalRequestCount");
    let resolved_requests = value_i64(evidence, "affectedRequestSatisfiedOrSuccessorCount");
    let valid_prefixes = value_i64(evidence, "validPrefixTableCount");
    let preserved_prefixes = value_i64(evidence, "validPrefixPreservedCount");
    let maximum_lamports = value_i64(evidence, "budgetMaximumLamports");
    let spent_lamports = value_i64(evidence, "budgetSpentLamports");
    let exact_required_counters_present = [
        active,
        verified,
        damaged,
        damaged_nonallocating,
        terminal,
        terminal_repaired,
        affected_requests,
        resolved_requests,
        valid_prefixes,
        preserved_prefixes,
        maximum_lamports,
        spent_lamports,
        value_i64(evidence, "activeOrReferencedWrongOwnerCount"),
        value_i64(evidence, "activeOrReferencedAuthorityMismatchCount"),
        value_i64(evidence, "activeOrReferencedPrefixMismatchCount"),
        value_i64(evidence, "damagedActiveOrPreparingBindingCount"),
        value_i64(evidence, "damagedRunnableOperationCount"),
        value_i64(evidence, "damagedRouteDependencyCount"),
        value_i64(evidence, "terminalOperationMissingRepairEvidenceCount"),
        value_i64(evidence, "unresolvedActiveTerminalRequestCount"),
        value_i64(evidence, "staleSuffixRetryCount"),
        value_i64(evidence, "newLegacyOrExactRouteTableCount"),
        value_i64(evidence, "activeAltMutatorCount"),
        value_i64(evidence, "budgetWindowSeconds"),
        value_i64(evidence, "finalizedRpcSlot"),
    ]
    .into_iter()
    .all(|value| value.is_some());
    let zero_error_counters = [
        "activeOrReferencedWrongOwnerCount",
        "activeOrReferencedAuthorityMismatchCount",
        "activeOrReferencedPrefixMismatchCount",
        "damagedActiveOrPreparingBindingCount",
        "damagedRunnableOperationCount",
        "damagedRouteDependencyCount",
        "terminalOperationMissingRepairEvidenceCount",
        "unresolvedActiveTerminalRequestCount",
        "staleSuffixRetryCount",
        "newLegacyOrExactRouteTableCount",
    ]
    .into_iter()
    .all(|key| value_i64(evidence, key) == Some(0));
    let passed = schema_known
        && value_bool(evidence, "available") == Some(true)
        && value_string(evidence, "cluster") == Some(PRODUCTION_CLUSTER)
        && value_i64(evidence, "finalizedRpcSlot").is_some_and(|slot| slot > 0)
        && value_string(evidence, "altProgramId") == Some(ADDRESS_LOOKUP_TABLE_PROGRAM_ID)
        && value_string(evidence, "standardPolicyPubkey") == Some(STANDARD_POLICY_PUBKEY)
        && exact_required_counters_present
        && active.is_some_and(|count| count > 0)
        && active == verified
        && damaged.is_some_and(|count| count > 0)
        && damaged == damaged_nonallocating
        && terminal.is_some_and(|count| count > 0)
        && terminal == terminal_repaired
        && affected_requests.is_some_and(|count| count > 0)
        && affected_requests == resolved_requests
        && valid_prefixes.is_some_and(|count| count > 0)
        && valid_prefixes == preserved_prefixes
        && zero_error_counters
        && value_i64(evidence, "activeAltMutatorCount") == Some(1)
        && value_i64(evidence, "budgetWindowSeconds").is_some_and(|seconds| seconds > 0)
        && maximum_lamports.is_some_and(|maximum| maximum > 0)
        && spent_lamports.is_some_and(|spent| spent >= 0)
        && maximum_lamports
            .zip(spent_lamports)
            .is_some_and(|(maximum, spent)| spent <= maximum);
    subcheck(
        "production_alt_damage_is_fenced_repaired_and_budgeted",
        passed,
        json!({
            "rawMeasurements": evidence,
            "measurementSchemaKnown": schema_known,
            "requiredCountersPresent": exact_required_counters_present,
            "activeTablesEqualVerifiedTables": active == verified,
            "damagedTablesEqualNonallocatingTables": damaged == damaged_nonallocating,
            "terminalOperationsEqualRepairEvidence": terminal == terminal_repaired,
            "affectedRequestsEqualResolvedOrSuccessorRequests": affected_requests == resolved_requests,
            "validPrefixesEqualPreservedPrefixes": valid_prefixes == preserved_prefixes,
            "unsafeCountersZero": zero_error_counters,
            "budgetWithinLimit": maximum_lamports.zip(spent_lamports).is_some_and(|(maximum, spent)| spent <= maximum),
            "embeddedPassIgnored": evidence.get("pass"),
        }),
    )
}

fn production_alt_mutator_identity_subcheck(binding: &ProductionEvidenceBinding) -> Subcheck {
    let evidence = binding.artifact.measurements.database.alt_repair.as_ref();
    let passed = evidence.is_some_and(|evidence| {
        alt_repair_measurement_schema_known(evidence)
            && value_bool(evidence, "available") == Some(true)
            && value_string(evidence, "cluster") == Some(PRODUCTION_CLUSTER)
            && value_string(evidence, "standardPolicyPubkey") == Some(STANDARD_POLICY_PUBKEY)
            && value_i64(evidence, "activeAltMutatorCount") == Some(1)
            && value_i64(evidence, "budgetWindowSeconds").is_some_and(|value| value > 0)
            && value_i64(evidence, "budgetMaximumLamports").is_some_and(|value| value > 0)
            && value_i64(evidence, "budgetSpentLamports")
                .zip(value_i64(evidence, "budgetMaximumLamports"))
                .is_some_and(|(spent, maximum)| spent >= 0 && spent <= maximum)
    });
    subcheck(
        "one_budgeted_provisioner_uses_standard_policy_identity",
        passed,
        json!({
            "standardPolicyPubkey": evidence.and_then(|value| value.get("standardPolicyPubkey")),
            "activeAltMutatorCount": evidence.and_then(|value| value.get("activeAltMutatorCount")),
            "budgetWindowSeconds": evidence.and_then(|value| value.get("budgetWindowSeconds")),
            "budgetMaximumLamports": evidence.and_then(|value| value.get("budgetMaximumLamports")),
            "budgetSpentLamports": evidence.and_then(|value| value.get("budgetSpentLamports")),
        }),
    )
}

fn production_deployment_checks(
    binding: &ProductionEvidenceBinding,
    runtime: Option<&RuntimeEvidenceV1>,
) -> Vec<VerifierCheck> {
    let alt_repair = production_alt_repair_subcheck(binding);
    let alt_failure = (!matches!(alt_repair.verdict, Verdict::Pass)).then_some(alt_repair.name);
    let migration = production_migration_subcheck(binding);
    let render = production_render_subcheck(binding, runtime);
    let market_data = production_confirmed_market_data_plane_subcheck(binding, runtime);
    let alt_mutator = production_alt_mutator_identity_subcheck(binding);
    let cutover_subchecks = vec![migration, render, market_data, alt_mutator];
    let cutover_failure = first_failed(&cutover_subchecks);
    vec![
        check(
            8,
            "production_alt_damage_recovery",
            alt_repair.verdict,
            alt_failure,
            json!({
                "sourceBoundHead": binding.head_commit,
                "cluster": binding.artifact.scope.cluster,
                "capturedAt": binding.artifact.captured_at,
            }),
            vec![alt_repair],
        ),
        check(
            9,
            "production_migration_and_atomic_executor_cutover",
            aggregate_subchecks(&cutover_subchecks),
            cutover_failure,
            json!({
                "sourceBoundHead": binding.head_commit,
                "renderYamlSha256": binding.render_yaml_sha256,
                "renderEnvironmentId": binding.artifact.scope.render_environment_id,
                "capturedAt": binding.artifact.captured_at,
            }),
            cutover_subchecks,
        ),
    ]
}

fn status_metric<'a>(queue: &'a Value, key: &str) -> Option<&'a Value> {
    queue.get("statusRows")?.as_array()?.first()?.get(key)
}

fn status_i64(queue: &Value, key: &str) -> Option<i64> {
    status_metric(queue, key).and_then(Value::as_i64)
}

fn production_component_capture_is_fresh(
    binding: &ProductionEvidenceBinding,
    component: &Value,
) -> bool {
    value_string(component, "capturedAt")
        .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
        .map(|value| value.with_timezone(&Utc))
        .is_some_and(|captured_at| {
            captured_at >= binding.artifact.collection_started_at
                && captured_at <= binding.artifact.captured_at
                && binding
                    .artifact
                    .captured_at
                    .signed_duration_since(captured_at)
                    <= chrono::Duration::seconds(PRODUCTION_COMPONENT_MAX_LAG_SECONDS)
        })
}

fn production_complete_fleet_subcheck(binding: &ProductionEvidenceBinding) -> Subcheck {
    let queue = &binding.artifact.measurements.database.queue;
    let Some(rows) = queue.get("statusRows").and_then(Value::as_array) else {
        return subcheck(
            "fresh_complete_epoch_accounts_for_fleet_and_drains_by_value",
            false,
            json!({"error": "queue.statusRows is missing"}),
        );
    };
    let schema_known = queue_measurement_schema_known(queue);
    let queue_capture_fresh = production_component_capture_is_fresh(binding, queue);
    let aggregate_keys = [
        "full_sweep_age_seconds",
        "complete_frontier",
        "observed_vault_count",
        "planned_opportunity_count",
        "planned_selected_count",
        "planned_deferred_count",
        "latest_market_epoch_id",
        "latest_market_epoch_expired",
        "planner_epoch_matches_latest",
        "current_epoch_opportunity_count",
        "current_epoch_principal_usd_micros",
        "current_epoch_recoverable_yield_usd_micros_per_hour",
    ];
    let aggregate_fields_consistent = rows.first().is_some_and(|first| {
        aggregate_keys
            .iter()
            .all(|key| rows.iter().all(|row| row.get(*key) == first.get(*key)))
    });
    let selected = status_i64(queue, "planned_selected_count");
    let deferred = status_i64(queue, "planned_deferred_count");
    let planned = status_i64(queue, "planned_opportunity_count");
    let drained_complete_frontier = [
        selected,
        deferred,
        planned,
        status_i64(queue, "current_epoch_opportunity_count"),
        status_i64(queue, "current_epoch_principal_usd_micros"),
        status_i64(queue, "current_epoch_recoverable_yield_usd_micros_per_hour"),
    ]
    .into_iter()
    .all(|value| value == Some(0));
    let planner_epoch_current_or_drained =
        status_metric(queue, "planner_epoch_matches_latest").and_then(Value::as_bool) == Some(true)
            || drained_complete_frontier;
    let fresh_complete_epoch = schema_known
        && value_bool(queue, "available") == Some(true)
        && !rows.is_empty()
        && status_i64(queue, "full_sweep_age_seconds")
            .is_some_and(|age| (0..=COMPLETE_SWEEP_MAX_AGE_SECONDS).contains(&age))
        && status_metric(queue, "complete_frontier").and_then(Value::as_bool) == Some(true)
        && planner_epoch_current_or_drained
        && status_metric(queue, "latest_market_epoch_expired").and_then(Value::as_bool)
            == Some(false)
        && status_i64(queue, "latest_market_epoch_id").is_some_and(|id| id > 0)
        && status_i64(queue, "observed_vault_count").is_some_and(|count| count > 0)
        && aggregate_fields_consistent;
    let planner_accounting_exact =
        selected
            .zip(deferred)
            .zip(planned)
            .is_some_and(|((selected, deferred), planned)| {
                selected >= 0 && deferred >= 0 && planned == selected + deferred
            });
    let bounded_stage_ages = [
        "oldest_waiting_alt_state_age_seconds",
        "oldest_ready_state_age_seconds",
        "oldest_sender_state_age_seconds",
        "oldest_confirmer_state_age_seconds",
        "oldest_reconciler_state_age_seconds",
    ]
    .into_iter()
    .all(|key| {
        status_metric(queue, key).is_some_and(|value| {
            value.is_null()
                || value
                    .as_i64()
                    .is_some_and(|age| (0..=MATERIAL_STAGE_MAX_AGE_SECONDS).contains(&age))
        })
    });
    let status_counts_well_formed = rows.iter().all(|row| {
        value_i64(row, "opportunity_count").is_some_and(|count| count >= 0)
            && value_i64(row, "principal_usd_micros").is_some_and(|amount| amount >= 0)
            && value_i64(row, "annual_yield_gain_usd_micros").is_some_and(|gain| gain >= 0)
            && value_i64(row, "expired_lease_count") == Some(0)
            && value_i64(row, "effect_ambiguous_count") == Some(0)
    });
    let zero_queue_counters = [
        "staleActiveDecisionCount",
        "duplicateActiveVaultMovementCount",
        "materialStuckOverTenMinutesCount",
        "targetCapacityOversubscriptionCount",
        "highValueOrderingInversionCount",
    ]
    .into_iter()
    .all(|key| value_i64(queue, key) == Some(0));
    let top = queue
        .get("topCurrentEpochOpportunities")
        .and_then(Value::as_array);
    let known_states = [
        "waiting_alt",
        "revalidate",
        "ready",
        "leased",
        "decision_created",
        "completed",
        "stale",
        "superseded",
        "failed",
        "cancelled",
    ];
    let material_top_rows_named = top.is_some_and(|top| {
        top.iter().all(|row| {
            let principal = value_i64(row, "principal_usd_micros").unwrap_or_default();
            if principal < MATERIAL_PRINCIPAL_USD_MICROS {
                return true;
            }
            let state = value_string(row, "opportunity_state");
            let current = matches!(
                state,
                Some(
                    "waiting_alt"
                        | "revalidate"
                        | "ready"
                        | "leased"
                        | "decision_created"
                        | "completed"
                )
            );
            let named_terminal =
                matches!(state, Some("stale" | "superseded" | "failed" | "cancelled"))
                    && row
                        .get("terminal_reason")
                        .and_then(Value::as_str)
                        .is_some_and(|reason| !reason.trim().is_empty());
            state.is_some_and(|state| known_states.contains(&state)) && (current || named_terminal)
        })
    });
    let aggregate_visible = status_i64(queue, "current_epoch_opportunity_count")
        .zip(status_i64(queue, "current_epoch_principal_usd_micros"))
        .zip(status_i64(
            queue,
            "current_epoch_recoverable_yield_usd_micros_per_hour",
        ))
        .is_some_and(|((count, principal), gain)| {
            count >= 0
                && principal >= 0
                && gain >= 0
                && ((count == 0 && principal == 0 && gain == 0)
                    || (count > 0 && principal > 0 && gain > 0))
        });
    let passed = queue_capture_fresh
        && fresh_complete_epoch
        && planner_accounting_exact
        && bounded_stage_ages
        && status_counts_well_formed
        && zero_queue_counters
        && material_top_rows_named
        && aggregate_visible;
    subcheck(
        "fresh_complete_epoch_accounts_for_fleet_and_drains_by_value",
        passed,
        json!({
            "statusRowCount": rows.len(),
            "measurementSchemaKnown": schema_known,
            "queueCaptureFresh": queue_capture_fresh,
            "componentMaximumLagSeconds": PRODUCTION_COMPONENT_MAX_LAG_SECONDS,
            "freshCompleteEpoch": fresh_complete_epoch,
            "plannerEpochCurrentOrDrained": planner_epoch_current_or_drained,
            "drainedCompleteFrontier": drained_complete_frontier,
            "aggregateFieldsConsistentAcrossRows": aggregate_fields_consistent,
            "observedVaultCount": status_i64(queue, "observed_vault_count"),
            "plannedOpportunityCount": planned,
            "plannedSelectedCount": selected,
            "plannedDeferredCount": deferred,
            "plannerAccountingExact": planner_accounting_exact,
            "boundedStageAges": bounded_stage_ages,
            "statusCountsWellFormed": status_counts_well_formed,
            "zeroQueueErrorCounters": zero_queue_counters,
            "materialTopRowsHaveCurrentStateOrNamedTerminalReason": material_top_rows_named,
            "aggregateOutcomeAndValueVisible": aggregate_visible,
            "queueCounters": {
                "staleActiveDecisionCount": queue.get("staleActiveDecisionCount"),
                "duplicateActiveVaultMovementCount": queue.get("duplicateActiveVaultMovementCount"),
                "materialStuckOverTenMinutesCount": queue.get("materialStuckOverTenMinutesCount"),
                "targetCapacityOversubscriptionCount": queue.get("targetCapacityOversubscriptionCount"),
                "highValueOrderingInversionCount": queue.get("highValueOrderingInversionCount"),
            },
        }),
    )
}

fn parse_rfc3339(value: Option<&Value>) -> Option<DateTime<Utc>> {
    value
        .and_then(Value::as_str)
        .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
        .map(|value| value.with_timezone(&Utc))
}

fn plan_string<'a>(row: &'a Value, plan_key: &str, field: &str) -> Option<&'a str> {
    row.get(plan_key)?.get(field)?.as_str()
}

fn plan_i64(row: &Value, plan_key: &str, field: &str) -> Option<i64> {
    row.get(plan_key)?.get(field).and_then(|value| {
        value
            .as_i64()
            .or_else(|| value.as_str().and_then(|value| value.parse().ok()))
    })
}

fn plan_timestamp(row: &Value, plan_key: &str, field: &str) -> Option<DateTime<Utc>> {
    plan_string(row, plan_key, field)
        .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
        .map(|value| value.with_timezone(&Utc))
}

fn plan_explicit_null(row: &Value, plan_key: &str, field: &str) -> bool {
    row.get(plan_key)
        .and_then(|plan| plan.get(field))
        .is_some_and(Value::is_null)
}

fn movement_chain_snapshot_context(row: &Value, field: &str) -> bool {
    matches!(
        row.get(field)
            .and_then(|context| context.get("kind"))
            .and_then(Value::as_str),
        Some("fleet_position_sweep" | "same_mint_chain_reconcile_preview")
    )
}

fn movement_chain_position_metadata(row: &Value, field: &str) -> bool {
    row.get(field)
        .and_then(|metadata| metadata.get("source"))
        .and_then(Value::as_str)
        == Some("chain_reconcile_preview")
}

fn reconciled_volume_snapshot(value: &Value) -> Option<(i64, i64, i64)> {
    let movement_count = value_i64(value, "movementCount")?;
    let amount_raw = value_i64(value, "amountRaw")?;
    let principal_usd_micros = value_i64(value, "principalUsdMicros")?;
    (movement_count >= 0
        && amount_raw >= 0
        && principal_usd_micros >= 0
        && value_i64(value, "uniqueSubmissionCount") == Some(movement_count)
        && value_i64(value, "uniqueOpportunityCount") == Some(movement_count)
        && value_i64(value, "uniqueDecisionCount") == Some(movement_count)
        && value_i64(value, "uniqueSignatureCount") == Some(movement_count))
    .then_some((movement_count, amount_raw, principal_usd_micros))
}

fn production_movement_subcheck(binding: &ProductionEvidenceBinding) -> Subcheck {
    let movement = &binding.artifact.measurements.database.movement;
    let queue = &binding.artifact.measurements.database.queue;
    let positions = &binding.artifact.measurements.database.positions;
    let schema_known = movement_measurement_schema_known(movement)
        && queue_measurement_schema_known(queue)
        && position_measurement_schema_known(positions);
    let queue_capture_fresh = production_component_capture_is_fresh(binding, queue);
    let Some(rows) = movement.get("movements").and_then(Value::as_array) else {
        return subcheck(
            "optimizer_movements_are_final_economic_reconciled_and_meet_slos",
            false,
            json!({"error": "movement.movements is missing"}),
        );
    };
    let cutover_at = binding.artifact.scope.cutover_at;
    let artifact_cutover_at = parse_rfc3339(movement.get("cutoverAt"));
    let cutover_bound = cutover_at.is_some() && cutover_at == artifact_cutover_at;
    let main_reserve = KAMINO_MAIN_USDC_RESERVE.to_string();
    let usdc_mint = USDC_MINT.to_string();
    let main = movement.get("mainUsdc").unwrap_or(&Value::Null);
    let baseline_cohort_vault_ids = main
        .get("baselineCohortVaultIds")
        .and_then(Value::as_array)
        .and_then(|ids| ids.iter().map(Value::as_i64).collect::<Option<Vec<_>>>())
        .unwrap_or_default();
    let baseline_cohort_vaults = baseline_cohort_vault_ids
        .iter()
        .copied()
        .collect::<BTreeSet<_>>();
    let baseline_cohort_count = value_i64(main, "baselineCohortVaultCount");
    let baseline_cohort_identity_exact = !baseline_cohort_vault_ids.is_empty()
        && baseline_cohort_vault_ids
            .iter()
            .all(|vault_id| *vault_id > 0)
        && baseline_cohort_vaults.len() == baseline_cohort_vault_ids.len()
        && i64::try_from(baseline_cohort_vault_ids.len()).ok() == baseline_cohort_count;
    let rpc_finalized_block_height = value_i64(movement, "rpcFinalizedBlockHeight");
    let rpc_finalized_slot = value_i64(movement, "rpcFinalizedSlot");
    let mut reconciled_movement_count = 0i64;
    let mut reconciled_reserve_count = 0i64;
    let mut reconciled_idle_deposit_count = 0i64;
    let mut fully_proven_count = 0i64;
    let mut economic_failure_count = 0i64;
    let mut unsafe_terminal_outcome_count = 0i64;
    let mut nonterminal_count = 0i64;
    let mut ambiguous_count = 0i64;
    let mut main_outflow = 0i128;
    let mut main_inflow = 0i128;
    let mut baseline_cohort_main_outflow = 0i128;
    let mut baseline_cohort_main_inflow = 0i128;
    let mut reconciled_amount_raw = 0i128;
    let mut reconciled_principal_usd_micros = 0i128;
    let mut identifiers = BTreeSet::new();
    let mut reconciled_submission_ids = BTreeSet::new();
    let mut all_rows_well_formed = true;
    for row in rows {
        let state = value_string(row, "submissionState").unwrap_or_default();
        if !matches!(state, "reconciled" | "expired" | "failed") {
            nonterminal_count += 1;
        }
        if state == "effect_ambiguous" {
            ambiguous_count += 1;
        }
        let route_kind = value_string(row, "routeKind");
        let decision_plan_kind = plan_string(row, "decisionExecutionPlan", "kind");
        let planner_plan_kind = plan_string(row, "plannerExecutionPlan", "kind");
        let decision_source_kind = plan_string(row, "decisionExecutionPlan", "source_kind");
        let planner_source_kind = plan_string(row, "plannerExecutionPlan", "source_kind");
        let opportunity_source_snapshot_id = value_i64(row, "opportunitySourceSnapshotId");
        let submission_source_snapshot_id = value_i64(row, "submissionSourceSnapshotId");
        let source = row.get("sourceReserve").and_then(Value::as_str);
        let decision_source_snapshot_id = value_i64(row, "decisionSourceSnapshotId");
        let decision_source = row.get("decisionSourceReserve").and_then(Value::as_str);
        let target = value_string(row, "targetReserve");
        let decision_target = value_string(row, "decisionTargetReserve");
        let mint = value_string(row, "liquidityMint");
        let decision_mint = value_string(row, "decisionLiquidityMint");
        let amount = value_i64(row, "amountRaw");
        let decision_amount = value_i64(row, "decisionAmountRaw");
        let executed_amount = value_i64(row, "executedAmountRaw");
        let opportunity_id = value_i64(row, "opportunityId");
        let decision_id = value_i64(row, "decisionId");
        let opportunity_decision_id = value_i64(row, "opportunityDecisionId");
        let submission_id = value_i64(row, "submissionId");
        let vault_id = value_i64(row, "vaultId");
        let decision_vault_id = value_i64(row, "decisionVaultId");
        let opportunity_optimizer_epoch_id = value_i64(row, "opportunityOptimizerEpochId");
        let submission_optimizer_epoch_id = value_i64(row, "submissionOptimizerEpochId");
        let optimizer_epoch_fingerprint = value_string(row, "optimizerEpochFingerprint");
        let optimizer_epoch_expires_at = parse_rfc3339(row.get("optimizerEpochExpiresAt"));
        let signed_optimizer_epoch_evidence = row
            .get("submissionOptimizerEpochEvidence")
            .unwrap_or(&Value::Null);
        let optimizer_epoch_identity_exact = opportunity_optimizer_epoch_id
            .is_some_and(|id| id > 0)
            && submission_optimizer_epoch_id == opportunity_optimizer_epoch_id
            && value_i64(signed_optimizer_epoch_evidence, "id") == opportunity_optimizer_epoch_id
            && value_string(signed_optimizer_epoch_evidence, "fingerprint")
                == optimizer_epoch_fingerprint
            && parse_rfc3339(signed_optimizer_epoch_evidence.get("expiresAt"))
                == optimizer_epoch_expires_at
            && optimizer_epoch_fingerprint.is_some_and(|fingerprint| {
                fingerprint.len() == 64 && fingerprint.bytes().all(|byte| byte.is_ascii_hexdigit())
            })
            && optimizer_epoch_expires_at.is_some();
        let source_snapshot_id = value_i64(row, "sourceSnapshotId");
        let source_snapshot_vault_id = value_i64(row, "sourceSnapshotVaultId");
        let pre_target_snapshot_id = value_i64(row, "preTargetSnapshotId");
        let pre_target_snapshot_vault_id = value_i64(row, "preTargetSnapshotVaultId");
        let created_at = parse_rfc3339(row.get("createdAt"));
        let route_identity_ok = match route_kind {
            Some("same_mint") => {
                source.is_some()
                    && source != target
                    && decision_source == source
                    && opportunity_source_snapshot_id.is_some_and(|id| id > 0)
                    && decision_source_snapshot_id == opportunity_source_snapshot_id
                    && submission_source_snapshot_id == opportunity_source_snapshot_id
                    && source_snapshot_id == opportunity_source_snapshot_id
                    && source_snapshot_vault_id == vault_id
                    && movement_chain_snapshot_context(row, "sourceSnapshotContext")
                    && pre_target_snapshot_id == opportunity_source_snapshot_id
                    && pre_target_snapshot_vault_id == vault_id
                    && movement_chain_snapshot_context(row, "preTargetSnapshotContext")
                    && movement_chain_position_metadata(row, "preTargetPlanningMetadata")
                    && decision_plan_kind == Some("same_mint")
                    && planner_plan_kind == Some("same_mint")
                    && plan_string(row, "decisionExecutionPlan", "source_reserve") == source
                    && plan_string(row, "plannerExecutionPlan", "source_reserve") == source
                    && plan_string(row, "decisionExecutionPlan", "target_reserve") == target
                    && plan_string(row, "plannerExecutionPlan", "target_reserve") == target
                    && plan_string(row, "decisionExecutionPlan", "liquidity_mint") == mint
                    && plan_string(row, "plannerExecutionPlan", "liquidity_mint") == mint
                    && plan_i64(row, "decisionExecutionPlan", "amount_raw") == decision_amount
                    && plan_i64(row, "plannerExecutionPlan", "amount_raw") == amount
            }
            Some("idle_vault_deposit") => {
                source.is_none()
                    && decision_source.is_none()
                    && opportunity_source_snapshot_id.is_none()
                    && decision_source_snapshot_id.is_none()
                    && submission_source_snapshot_id.is_none()
                    && source_snapshot_id.is_none()
                    && source_snapshot_vault_id.is_none()
                    && pre_target_snapshot_id.is_some_and(|id| id > 0)
                    && pre_target_snapshot_vault_id == vault_id
                    && movement_chain_snapshot_context(row, "preTargetSnapshotContext")
                    && movement_chain_position_metadata(row, "preTargetPlanningMetadata")
                    && decision_source_kind == Some("idle_vault")
                    && planner_source_kind == Some("idle_vault_usdc")
                    && decision_plan_kind == Some("idle_vault_deposit")
                    && planner_plan_kind == Some("idle_vault_deposit")
                    && plan_explicit_null(row, "decisionExecutionPlan", "source_reserve")
                    && plan_explicit_null(row, "plannerExecutionPlan", "source_reserve")
                    && plan_string(row, "decisionExecutionPlan", "target_reserve") == target
                    && plan_string(row, "plannerExecutionPlan", "target_reserve") == target
                    && plan_string(row, "decisionExecutionPlan", "liquidity_mint") == mint
                    && plan_string(row, "plannerExecutionPlan", "liquidity_mint") == mint
                    && plan_i64(row, "decisionExecutionPlan", "amount_raw") == decision_amount
                    && plan_i64(row, "plannerExecutionPlan", "amount_raw") == amount
                    && plan_string(row, "decisionExecutionPlan", "idle_token_account")
                        .is_some_and(|account| !account.trim().is_empty())
                    && plan_string(row, "decisionExecutionPlan", "idle_token_account")
                        == plan_string(row, "plannerExecutionPlan", "idle_token_account")
            }
            _ => false,
        };
        let identity_ok = opportunity_id.is_some_and(|id| id > 0)
            && decision_id.is_some_and(|id| id > 0)
            && submission_id.is_some_and(|id| id > 0)
            && vault_id.is_some_and(|id| id > 0)
            && opportunity_decision_id == decision_id
            && decision_vault_id == vault_id
            && optimizer_epoch_identity_exact
            && value_bool(row, "optimizerEpochIdentityExact") == Some(true)
            && value_string(row, "signature").is_some_and(|value| !value.trim().is_empty())
            && route_identity_ok
            && value_string(row, "decisionRouteKind") == decision_plan_kind
            && value_string(row, "decisionSourceKind") == decision_source_kind
            && value_string(row, "plannerSourceKind") == planner_source_kind
            && target.is_some_and(|value| !value.trim().is_empty())
            && decision_target == target
            && mint.is_some_and(|value| !value.trim().is_empty())
            && decision_mint == mint
            && amount.is_some_and(|amount| amount > 0)
            && decision_amount.is_some_and(|amount| amount > 0)
            && executed_amount == decision_amount
            && created_at
                .zip(cutover_at)
                .is_some_and(|(created, cutover)| created >= cutover)
            && value_bool(row, "routeIdentityExact") == Some(true);
        if let (Some(submission), Some(opportunity), Some(decision)) =
            (submission_id, opportunity_id, decision_id)
        {
            identifiers.insert((submission, opportunity, decision));
        }
        let estimated_edge = value_i64(row, "estimatedEdgeBps");
        let expected_gain = value_i64(row, "expectedNetGainUsdMicros");
        let compiled_fee = value_i64(row, "compiledFeeLamports");
        let estimated_cost = value_i64(row, "estimatedCostLamports");
        let sol_price = value_i64(row, "conservativeSolPriceUsdMicros");
        let fee_cap = value_i64(row, "feeFractionCapPpm");
        let computed_fee_usd = compiled_fee
            .zip(sol_price)
            .map(|(fee, price)| i128::from(fee) * i128::from(price) / 1_000_000_000i128);
        let computed_fee_fraction = computed_fee_usd
            .zip(expected_gain)
            .and_then(|(fee, gain)| (gain > 0).then_some(fee * 1_000_000i128 / i128::from(gain)));
        let economic = estimated_edge.is_some_and(|edge| edge > 0)
            && expected_gain.is_some_and(|gain| gain > 0)
            && compiled_fee.is_some_and(|fee| fee >= 0)
            && estimated_cost.is_some_and(|cost| cost >= 0)
            && compiled_fee
                .zip(estimated_cost)
                .is_some_and(|(fee, estimated)| fee <= estimated)
            && sol_price.is_some_and(|price| price > 0)
            && fee_cap.is_some_and(|cap| cap > 0)
            && computed_fee_fraction
                .zip(fee_cap)
                .is_some_and(|(fraction, cap)| fraction >= 0 && fraction <= i128::from(cap));
        if !economic {
            economic_failure_count += 1;
        }
        let submitted_slot = value_i64(row, "submittedSlot");
        let confirmed_slot = value_i64(row, "confirmedSlot");
        let reconciled_slot = value_i64(row, "reconciledSlot");
        let observed_slot = value_i64(row, "postSnapshotObservedSlot");
        let rpc_slot = value_i64(row, "rpcSlot");
        let reserve_source_effect = value_i64(row, "preSourceAmountRaw")
            .zip(value_i64(row, "postSourceAmountRaw"))
            .is_some_and(|(before, after)| after < before)
            && movement_chain_snapshot_context(row, "sourceSnapshotContext")
            && movement_chain_snapshot_context(row, "preTargetSnapshotContext")
            && movement_chain_position_metadata(row, "preTargetPlanningMetadata")
            && movement_chain_snapshot_context(row, "postSnapshotContext");
        let reserve_target_effect = row
            .get("preTargetAmountRaw")
            .and_then(Value::as_i64)
            .unwrap_or_default()
            < row
                .get("postTargetAmountRaw")
                .and_then(Value::as_i64)
                .unwrap_or_default();
        let idle_source_effect = value_i64(row, "preIdleSourceAmountRaw")
            .zip(value_i64(row, "postIdleSourceAmountRaw"))
            .zip(amount)
            .is_some_and(|((before, after), amount)| {
                before == amount && after >= 0 && after <= before.saturating_sub(amount)
            })
            && value_string(row, "idleTokenAccount")
                == value_string(row, "postIdleSourceTokenAccount")
            && value_string(row, "idleTokenAccount")
                == value_string(row, "plannerIdleTokenAccount")
            && movement_chain_snapshot_context(row, "postSnapshotContext")
            && movement_chain_position_metadata(row, "postTargetPlanningMetadata")
            && value_string(row, "idleTokenAccount")
                == plan_string(row, "decisionExecutionPlan", "idle_token_account")
            && value_string(row, "plannerIdleTokenAccount")
                == plan_string(row, "plannerExecutionPlan", "idle_token_account")
            && value_i64(row, "preIdleSourceAmountRaw")
                == value_i64(row, "plannerPreIdleSourceAmountRaw")
            && value_i64(row, "preIdleSourceAmountRaw")
                == plan_i64(
                    row,
                    "decisionExecutionPlan",
                    "idle_vault_liquidity_amount_raw",
                )
            && value_i64(row, "plannerPreIdleSourceAmountRaw")
                == plan_i64(
                    row,
                    "plannerExecutionPlan",
                    "idle_vault_liquidity_amount_raw",
                )
            && value_i64(row, "preIdleSourceObservedSlot")
                .zip(rpc_slot)
                .is_some_and(|(observed, rpc)| observed > 0 && observed <= rpc)
            && value_i64(row, "preIdleSourceObservedSlot")
                == value_i64(row, "plannerPreIdleSourceObservedSlot")
            && value_i64(row, "preIdleSourceObservedSlot")
                == plan_i64(row, "decisionExecutionPlan", "idle_observed_slot")
                    .or_else(|| plan_i64(row, "decisionExecutionPlan", "observed_slot"))
            && value_i64(row, "plannerPreIdleSourceObservedSlot")
                == plan_i64(row, "plannerExecutionPlan", "source_observed_slot")
                    .or_else(|| plan_i64(row, "plannerExecutionPlan", "idle_observed_slot"))
            && parse_rfc3339(row.get("preIdleSourceObservedAt"))
                .zip(created_at)
                .is_some_and(|(observed, created)| observed <= created)
            && parse_rfc3339(row.get("preIdleSourceObservedAt"))
                == parse_rfc3339(row.get("plannerPreIdleSourceObservedAt"))
            && parse_rfc3339(row.get("preIdleSourceObservedAt"))
                == plan_timestamp(row, "decisionExecutionPlan", "idle_observed_at")
                    .or_else(|| plan_timestamp(row, "decisionExecutionPlan", "observed_at"))
            && parse_rfc3339(row.get("plannerPreIdleSourceObservedAt"))
                == plan_timestamp(row, "plannerExecutionPlan", "source_observed_at")
                    .or_else(|| plan_timestamp(row, "plannerExecutionPlan", "idle_observed_at"))
            && value_i64(row, "postIdleSourceObservedSlot")
                .zip(rpc_slot)
                .is_some_and(|(observed, rpc)| observed >= rpc)
            && value_i64(row, "postIdleSourceObservedSlot") == observed_slot
            && parse_rfc3339(row.get("postIdleSourceObservedAt")).is_some()
            && parse_rfc3339(row.get("postIdleSourceObservedAt"))
                == parse_rfc3339(row.get("postSnapshotObservedAt"));
        let idle_target_effect = value_i64(row, "preTargetAmountRaw")
            .zip(value_i64(row, "postTargetAmountRaw"))
            .is_some_and(|(before, after)| after > before)
            && value_string(row, "preTargetLiquidityMint") == mint
            && value_string(row, "postTargetLiquidityMint") == mint
            && value_bool(row, "preTargetHasValue")
                == value_i64(row, "preTargetAmountRaw").map(|amount| amount > 0)
            && value_bool(row, "postTargetHasValue") == Some(true)
            && value_i64(row, "preTargetSnapshotObservedSlot")
                .zip(rpc_slot)
                .is_some_and(|(observed, rpc)| observed > 0 && observed <= rpc)
            && parse_rfc3339(row.get("preTargetSnapshotObservedAt"))
                .zip(created_at)
                .is_some_and(|(observed, created)| observed <= created);
        let route_effect = match route_kind {
            Some("same_mint") => reserve_source_effect && reserve_target_effect,
            Some("idle_vault_deposit") => idle_source_effect && idle_target_effect,
            _ => false,
        };
        let created_at = parse_rfc3339(row.get("createdAt"));
        let submitted_at = parse_rfc3339(row.get("submittedAt"));
        let confirmed_at = parse_rfc3339(row.get("confirmedAt"));
        let reconciled_at = parse_rfc3339(row.get("reconciledAt"));
        let last_broadcast_at = parse_rfc3339(row.get("lastBroadcastAt"));
        let last_status_checked_at = parse_rfc3339(row.get("lastStatusCheckedAt"));
        let post_snapshot_observed_at = parse_rfc3339(row.get("postSnapshotObservedAt"));
        let opportunity_state = value_string(row, "opportunityState");
        let decision_status = value_string(row, "decisionStatus");
        let reconciled_lifecycle_ordered = created_at
            .zip(submitted_at)
            .zip(confirmed_at)
            .zip(reconciled_at)
            .is_some_and(|(((created, submitted), confirmed), reconciled)| {
                created <= submitted && submitted <= confirmed && confirmed <= reconciled
            });
        let reconciled_slots_ordered = submitted_slot
            .zip(rpc_slot)
            .zip(reconciled_slot)
            .is_some_and(|((submitted, rpc), reconciled)| {
                submitted > 0 && submitted <= rpc && reconciled >= rpc
            });
        let post_snapshot_time_ordered = created_at
            .zip(post_snapshot_observed_at)
            .zip(reconciled_at)
            .is_some_and(|((created, observed), reconciled)| {
                created <= observed && observed <= reconciled
            });
        let broadcast_evidence_ordered = value_i64(row, "broadcastCount")
            .is_some_and(|count| count > 0)
            && created_at
                .zip(last_broadcast_at)
                .zip(submitted_at)
                .is_some_and(|((created, broadcast), submitted)| {
                    created <= broadcast && broadcast <= submitted
                });
        let final_effect = state == "reconciled"
            && opportunity_state == Some("completed")
            && decision_status == Some("confirmed")
            && value_string(row, "decisionSignature") == value_string(row, "signature")
            && value_i64(row, "decisionConfirmedSlot") == confirmed_slot
            && identity_ok
            && value_i64(row, "postSnapshotId").is_some_and(|id| id > 0)
            && value_i64(row, "postSnapshotVaultId") == vault_id
            && value_i64(row, "decisionPostSnapshotId") == value_i64(row, "postSnapshotId")
            && movement_chain_snapshot_context(row, "postSnapshotContext")
            && movement_chain_position_metadata(row, "postTargetPlanningMetadata")
            && reconciled_lifecycle_ordered
            && reconciled_slots_ordered
            && post_snapshot_time_ordered
            && broadcast_evidence_ordered
            && reconciled_slot
                .zip(rpc_slot)
                .is_some_and(|(reconciled, rpc)| reconciled >= rpc)
            && observed_slot
                .zip(rpc_slot)
                .is_some_and(|(observed, rpc)| observed >= rpc)
            && route_effect
            && value_bool(row, "rpcFound") == Some(true)
            && value_bool(row, "rpcFinalized") == Some(true)
            && value_bool(row, "rpcSuccessful") == Some(true)
            && rpc_slot == confirmed_slot;
        let failed_lifecycle_ordered = created_at
            .zip(submitted_at)
            .zip(confirmed_at)
            .zip(last_status_checked_at)
            .is_some_and(|(((created, submitted), confirmed), checked)| {
                created <= submitted && submitted <= confirmed && confirmed <= checked
            });
        let failed_terminal_safe = state == "failed"
            && opportunity_state == Some("failed")
            && decision_status == Some("failed")
            && value_string(row, "decisionSignature") == value_string(row, "signature")
            && identity_ok
            && broadcast_evidence_ordered
            && failed_lifecycle_ordered
            && submitted_slot
                .zip(rpc_slot)
                .is_some_and(|(submitted, rpc)| submitted > 0 && submitted <= rpc)
            && value_bool(row, "rpcFound") == Some(true)
            && value_bool(row, "rpcFinalized") == Some(true)
            && value_bool(row, "rpcSuccessful") == Some(false)
            && rpc_slot == confirmed_slot;
        let broadcast_count = value_i64(row, "broadcastCount");
        let last_valid_block_height = value_i64(row, "lastValidBlockHeight");
        let expiry_height_proven = rpc_finalized_block_height
            .zip(last_valid_block_height)
            .is_some_and(|(current, last_valid)| current > last_valid)
            && match broadcast_count {
                Some(0) => {
                    row.get("lastBroadcastAt").is_some_and(Value::is_null)
                        && row
                            .get("expiryObservedBlockHeight")
                            .is_some_and(Value::is_null)
                        && row.get("effectCheckSlot").is_some_and(Value::is_null)
                }
                Some(count) if count > 0 => {
                    let pre_route_slot = value_i64(row, "preTargetSnapshotObservedSlot")
                        .into_iter()
                        .chain(
                            plan_i64(row, "decisionExecutionPlan", "idle_observed_slot").or_else(
                                || plan_i64(row, "decisionExecutionPlan", "observed_slot"),
                            ),
                        )
                        .max()
                        .unwrap_or_default();
                    value_i64(row, "expiryObservedBlockHeight")
                        .zip(last_valid_block_height)
                        .zip(rpc_finalized_block_height)
                        .is_some_and(|((observed, last_valid), current)| {
                            observed > last_valid && observed <= current
                        })
                        && last_broadcast_at.is_some()
                        && value_i64(row, "effectCheckSlot").is_some_and(|slot| {
                            slot > pre_route_slot
                                && rpc_finalized_slot.is_some_and(|current| slot <= current)
                        })
                }
                _ => false,
            };
        let expired_lifecycle_ordered =
            created_at
                .zip(last_status_checked_at)
                .is_some_and(|(created, checked)| {
                    created <= checked
                        && last_broadcast_at
                            .is_none_or(|broadcast| created <= broadcast && broadcast <= checked)
                });
        let expired_terminal_safe = state == "expired"
            && opportunity_state == Some("failed")
            && decision_status == Some("failed")
            && identity_ok
            && expired_lifecycle_ordered
            && expiry_height_proven
            && value_bool(row, "rpcFound") == Some(false)
            && value_bool(row, "rpcFinalized") == Some(false)
            && value_bool(row, "rpcSuccessful") == Some(false)
            && row.get("rpcSlot").is_some_and(Value::is_null);
        let terminal_outcome_safe = match state {
            "reconciled" => final_effect,
            "failed" => failed_terminal_safe,
            "expired" => expired_terminal_safe,
            _ => false,
        };
        if !terminal_outcome_safe {
            unsafe_terminal_outcome_count += 1;
        }
        if state == "reconciled" {
            reconciled_movement_count += 1;
            reconciled_amount_raw += i128::from(executed_amount.unwrap_or_default());
            reconciled_principal_usd_micros +=
                i128::from(value_i64(row, "principalUsdMicros").unwrap_or_default());
            if let Some(submission_id) = submission_id {
                reconciled_submission_ids.insert(submission_id);
            }
            if final_effect {
                fully_proven_count += 1;
            }
            match route_kind {
                Some("same_mint") if source.is_some() => reconciled_reserve_count += 1,
                Some("idle_vault_deposit") if source.is_none() => {
                    reconciled_idle_deposit_count += 1;
                }
                _ => {}
            }
            if mint == Some(usdc_mint.as_str()) {
                if route_kind == Some("same_mint") && source == Some(main_reserve.as_str()) {
                    main_outflow += i128::from(executed_amount.unwrap_or_default());
                    if vault_id.is_some_and(|id| baseline_cohort_vaults.contains(&id)) {
                        baseline_cohort_main_outflow +=
                            i128::from(executed_amount.unwrap_or_default());
                    }
                }
                if matches!(route_kind, Some("same_mint" | "idle_vault_deposit"))
                    && target == Some(main_reserve.as_str())
                {
                    main_inflow += i128::from(executed_amount.unwrap_or_default());
                    if vault_id.is_some_and(|id| baseline_cohort_vaults.contains(&id)) {
                        baseline_cohort_main_inflow +=
                            i128::from(executed_amount.unwrap_or_default());
                    }
                }
            }
        }
        all_rows_well_formed &= identity_ok
            && value_bool(row, "routeEffectProven") == Some(route_effect)
            && value_bool(row, "terminalOutcomeSafe") == Some(terminal_outcome_safe)
            && value_bool(row, "finalizedSuccess")
                == Some(
                    value_bool(row, "rpcFound") == Some(true)
                        && value_bool(row, "rpcFinalized") == Some(true)
                        && value_bool(row, "rpcSuccessful") == Some(true)
                        && rpc_slot == confirmed_slot,
                );
    }
    let unique_identifiers = identifiers.len() == rows.len();
    let raw_counts_match = value_i64(movement, "submissionCount") == i64::try_from(rows.len()).ok()
        && value_i64(movement, "nonterminalSubmissionCount") == Some(nonterminal_count)
        && value_i64(movement, "effectAmbiguousCount") == Some(ambiguous_count)
        && value_i64(movement, "reconciledMovementCount") == Some(reconciled_movement_count)
        && value_i64(movement, "reconciledReserveMovementCount") == Some(reconciled_reserve_count)
        && value_i64(movement, "reconciledIdleDepositCount") == Some(reconciled_idle_deposit_count)
        && value_i64(movement, "fullyFinalizedAndReconciledEffectCount")
            == Some(fully_proven_count)
        && value_i64(movement, "economicFailureCount") == Some(economic_failure_count)
        && value_i64(movement, "unsafeTerminalOutcomeCount") == Some(unsafe_terminal_outcome_count)
        && value_i64(movement, "reconciledAmountRaw").map(i128::from)
            == Some(reconciled_amount_raw)
        && value_i64(movement, "reconciledPrincipalUsdMicros").map(i128::from)
            == Some(reconciled_principal_usd_micros);
    let volume = movement.get("reconciledVolume").unwrap_or(&Value::Null);
    let volume_current = volume.get("current").and_then(reconciled_volume_snapshot);
    let volume_baseline = volume.get("baseline").and_then(reconciled_volume_snapshot);
    let volume_delta = volume.get("delta").and_then(reconciled_volume_snapshot);
    let reconciled_volume_exact = volume_current
        .zip(volume_baseline)
        .zip(volume_delta)
        .is_some_and(|((current, baseline), delta)| {
            current.0.checked_sub(baseline.0) == Some(delta.0)
                && current.1.checked_sub(baseline.1) == Some(delta.1)
                && current.2.checked_sub(baseline.2) == Some(delta.2)
                && delta.0 == reconciled_movement_count
                && i128::from(delta.1) == reconciled_amount_raw
                && i128::from(delta.2) == reconciled_principal_usd_micros
        })
        && value_bool(volume, "currentIdentityExact") == Some(true)
        && value_i64(volume, "postCutoverMovementCount") == Some(reconciled_movement_count)
        && value_i64(volume, "postCutoverAmountRaw").map(i128::from) == Some(reconciled_amount_raw)
        && value_i64(volume, "postCutoverPrincipalUsdMicros").map(i128::from)
            == Some(reconciled_principal_usd_micros);

    let largest = positions
        .get("largestEligibleVaults")
        .unwrap_or(&Value::Null);
    let largest_vaults = largest
        .get("vaults")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    let mut largest_vault_ids = BTreeSet::new();
    let mut recomputed_ranked_principal = 0i128;
    let mut recomputed_covered_principal = 0i128;
    let mut recomputed_top_three_blocked = 0i64;
    let mut recomputed_moved_count = 0i64;
    let mut largest_rows_valid = !largest_vaults.is_empty() && largest_vaults.len() <= 10;
    for (index, vault) in largest_vaults.iter().enumerate() {
        let expected_rank = i64::try_from(index + 1).ok();
        let vault_id = value_i64(vault, "vaultId");
        let principal = value_i64(vault, "principalUsdMicros");
        let classification = value_string(vault, "classification");
        largest_rows_valid &= value_i64(vault, "rank") == expected_rank
            && vault_id.is_some_and(|id| id > 0 && largest_vault_ids.insert(id))
            && principal.is_some_and(|amount| amount > 0)
            && matches!(
                classification,
                Some("already_optimal" | "moved" | "no_positive_edge" | "blocked")
            );
        recomputed_ranked_principal += i128::from(principal.unwrap_or_default());
        if classification != Some("blocked") {
            recomputed_covered_principal += i128::from(principal.unwrap_or_default());
        } else if index < 3 {
            recomputed_top_three_blocked += 1;
        }
        match classification {
            Some("already_optimal") => {
                largest_rows_valid &= value_i64(vault, "principalAtBestReserve") == principal
                    && vault.get("movedSubmissionId").is_some_and(Value::is_null);
            }
            Some("moved") => {
                recomputed_moved_count += 1;
                largest_rows_valid &= value_i64(vault, "movedSubmissionId")
                    .is_some_and(|id| reconciled_submission_ids.contains(&id))
                    && value_string(vault, "opportunityTargetReserve")
                        == value_string(vault, "bestReserve")
                    && parse_rfc3339(vault.get("movedReconciledAt"))
                        .zip(cutover_at)
                        .is_some_and(|(moved_at, cutover)| moved_at >= cutover);
            }
            Some("no_positive_edge") => {
                largest_rows_valid &= vault.get("opportunityId").is_some_and(Value::is_null)
                    && vault.get("movedSubmissionId").is_some_and(Value::is_null);
            }
            Some("blocked") => {}
            _ => largest_rows_valid = false,
        }
    }
    let largest_coverage_pass = recomputed_ranked_principal > 0
        && recomputed_covered_principal.saturating_mul(10)
            >= recomputed_ranked_principal.saturating_mul(9);
    let largest_accounts_pass = largest_account_measurement_schema_known(largest)
        && value_bool(largest, "available") == Some(true)
        && value_string(largest, "cluster") == Some(binding.artifact.scope.cluster.as_str())
        && parse_rfc3339(largest.get("cutoverAt")) == cutover_at
        && value_bool(largest, "completeFrontier") == Some(true)
        && parse_rfc3339(largest.get("optimizerEpochExpiresAt"))
            .is_some_and(|expires_at| expires_at > binding.artifact.captured_at)
        && parse_rfc3339(largest.get("fullSweepCompletedAt")).is_some_and(|completed_at| {
            completed_at <= binding.artifact.captured_at
                && binding.artifact.captured_at - completed_at <= chrono::Duration::seconds(120)
        })
        && largest_rows_valid
        && value_i64(largest, "rankedCount") == i64::try_from(largest_vaults.len()).ok()
        && value_i64(largest, "rankedPrincipalUsdMicros").map(i128::from)
            == Some(recomputed_ranked_principal)
        && value_i64(largest, "coveredPrincipalUsdMicros").map(i128::from)
            == Some(recomputed_covered_principal)
        && value_i64(largest, "topThreeBlockedCount") == Some(recomputed_top_three_blocked)
        && value_i64(largest, "movedCount") == Some(recomputed_moved_count)
        && recomputed_top_three_blocked == 0
        && largest_coverage_pass;
    let baseline = value_i64(main, "baselineAmountRaw");
    let deposits = value_i64(main, "postBaselineCohortDepositAmountRaw");
    let current_baseline_cohort = value_i64(main, "currentBaselineCohortAmountRaw");
    let current_routeable = value_i64(main, "currentRouteableAmountRaw");
    let adjusted_reduction = baseline.zip(deposits).zip(current_baseline_cohort).map(
        |((baseline, deposits), current)| {
            i128::from(baseline) + i128::from(deposits) - i128::from(current)
        },
    );
    let confirmed_net_outflow = main_outflow - main_inflow;
    let baseline_cohort_confirmed_net_outflow =
        baseline_cohort_main_outflow - baseline_cohort_main_inflow;
    let reduction_covers_cohort_net_outflow = adjusted_reduction.is_some_and(|reduction| {
        baseline_cohort_confirmed_net_outflow > 0
            && reduction > 0
            && reduction.saturating_mul(100)
                >= baseline_cohort_confirmed_net_outflow.saturating_mul(95)
    });
    let reported_main_flows_match = value_i64(main, "confirmedOptimizerOutflowRaw").map(i128::from)
        == Some(main_outflow)
        && value_i64(main, "confirmedOptimizerInflowRaw").map(i128::from) == Some(main_inflow)
        && value_i64(main, "confirmedOptimizerNetOutflowRaw").map(i128::from)
            == Some(confirmed_net_outflow)
        && value_i64(main, "baselineCohortConfirmedOptimizerOutflowRaw").map(i128::from)
            == Some(baseline_cohort_main_outflow)
        && value_i64(main, "baselineCohortConfirmedOptimizerInflowRaw").map(i128::from)
            == Some(baseline_cohort_main_inflow)
        && value_i64(main, "baselineCohortConfirmedOptimizerNetOutflowRaw").map(i128::from)
            == Some(baseline_cohort_confirmed_net_outflow);
    let baseline_reduction_pass = parse_rfc3339(main.get("baselineCollectedAt"))
        .zip(cutover_at)
        .is_some_and(|(baseline_at, cutover)| baseline_at <= cutover)
        && value_string(main, "reserve") == Some(main_reserve.as_str())
        && baseline_cohort_identity_exact
        && baseline.is_some_and(|amount| amount > 0)
        && deposits.is_some_and(|amount| amount >= 0)
        && current_baseline_cohort.is_some_and(|amount| amount >= 0)
        && current_routeable.is_some_and(|amount| amount >= 0)
        && reduction_covers_cohort_net_outflow
        && reported_main_flows_match
        && value_i64(main, "depositAdjustedReductionRaw").map(i128::from) == adjusted_reduction
        && value_bool(main, "reductionAfterDepositsCoversConfirmedNetOutflow")
            == Some(reduction_covers_cohort_net_outflow);
    let current_position = positions.pointer("/mainUsdc").unwrap_or(&Value::Null);
    let positions_match = value_bool(positions, "available") == Some(true)
        && value_string(current_position, "reserve") == Some(main_reserve.as_str())
        && value_string(current_position, "liquidityMint") == Some(usdc_mint.as_str())
        && value_i64(current_position, "amountRaw") == current_routeable
        && value_i64(current_position, "staleRowCount") == Some(0)
        && value_bool(current_position, "freshForBaseline") == Some(true);
    let slo = movement.get("movementSlos").unwrap_or(&Value::Null);
    let reconciled_latencies = rows
        .iter()
        .filter(|row| value_string(row, "submissionState") == Some("reconciled"))
        .map(|row| {
            parse_rfc3339(row.get("createdAt"))
                .zip(parse_rfc3339(row.get("submittedAt")))
                .zip(parse_rfc3339(row.get("reconciledAt")))
                .map(|((created, submitted), reconciled)| {
                    (
                        (submitted - created).num_milliseconds(),
                        (reconciled - created).num_milliseconds(),
                    )
                })
        })
        .collect::<Option<Vec<_>>>();
    let recomputed_maximum_submission_millis = reconciled_latencies
        .as_ref()
        .and_then(|latencies| latencies.iter().map(|(submission, _)| *submission).max());
    let recomputed_maximum_reconciliation_millis =
        reconciled_latencies.as_ref().and_then(|latencies| {
            latencies
                .iter()
                .map(|(_, reconciliation)| *reconciliation)
                .max()
        });
    let slos_pass = reconciled_movement_count > 0
        && reconciled_latencies.as_ref().is_some_and(|latencies| {
            i64::try_from(latencies.len()).ok() == Some(reconciled_movement_count)
        })
        && recomputed_maximum_submission_millis
            .is_some_and(|millis| (0..=120_000).contains(&millis))
        && recomputed_maximum_reconciliation_millis
            .is_some_and(|millis| (0..=900_000).contains(&millis))
        && value_string(slo, "basis") == Some("post_cutover_reconciled_submissions")
        && value_i64(slo, "reconciledMovementCount") == Some(reconciled_movement_count)
        && value_i64(slo, "submissionTimestampCount") == Some(reconciled_movement_count)
        && value_i64(slo, "reconciliationTimestampCount") == Some(reconciled_movement_count)
        && value_i64(slo, "submissionLimitMilliseconds") == Some(120_000)
        && value_i64(slo, "reconciliationLimitMilliseconds") == Some(900_000)
        && value_i64(slo, "maximumSignedToSubmittedMilliseconds")
            == recomputed_maximum_submission_millis
        && value_i64(slo, "maximumSignedToReconciledMilliseconds")
            == recomputed_maximum_reconciliation_millis;
    let terminal_counters_zero = ambiguous_count == 0
        && nonterminal_count == 0
        && economic_failure_count == 0
        && unsafe_terminal_outcome_count == 0
        && value_i64(movement, "databaseDeadlockCount") == Some(0)
        && value_i64(movement, "duplicateMovementCount") == Some(0)
        && value_i64(queue, "staleActiveDecisionCount") == Some(0)
        && value_i64(queue, "duplicateActiveVaultMovementCount") == Some(0)
        && value_i64(queue, "targetCapacityOversubscriptionCount") == Some(0);
    let passed = schema_known
        && queue_capture_fresh
        && value_bool(movement, "available") == Some(true)
        && cutover_bound
        && value_bool(movement, "rpcFinalityAvailable") == Some(true)
        && rpc_finalized_block_height.is_some_and(|height| height > 0)
        && rpc_finalized_slot.is_some_and(|slot| slot > 0)
        && !rows.is_empty()
        && all_rows_well_formed
        && unique_identifiers
        && raw_counts_match
        && reconciled_volume_exact
        && reconciled_reserve_count > 0
        && reconciled_reserve_count + reconciled_idle_deposit_count == reconciled_movement_count
        && fully_proven_count == reconciled_movement_count
        && reported_main_flows_match
        && positions_match
        && largest_accounts_pass
        && slos_pass
        && terminal_counters_zero;
    subcheck(
        "optimizer_movements_are_final_economic_reconciled_and_meet_slos",
        passed,
        json!({
            "cutoverBound": cutover_bound,
            "measurementSchemasKnown": schema_known,
            "queueCaptureFresh": queue_capture_fresh,
            "componentMaximumLagSeconds": PRODUCTION_COMPONENT_MAX_LAG_SECONDS,
            "submissionRows": rows.len(),
            "rowsWellFormedAndPostCutover": all_rows_well_formed,
            "uniqueSubmissionOpportunityDecisionTuples": unique_identifiers,
            "rawCountsMatchRecomputation": raw_counts_match,
            "reconciledVolumeExact": reconciled_volume_exact,
            "recomputedVolumeAmountRaw": reconciled_amount_raw,
            "recomputedVolumePrincipalUsdMicros": reconciled_principal_usd_micros,
            "recomputedNonterminalCount": nonterminal_count,
            "recomputedAmbiguousCount": ambiguous_count,
            "recomputedEconomicFailureCount": economic_failure_count,
            "recomputedUnsafeTerminalOutcomeCount": unsafe_terminal_outcome_count,
            "recomputedReconciledMovementCount": reconciled_movement_count,
            "recomputedReconciledReserveMovementCount": reconciled_reserve_count,
            "recomputedReconciledIdleDepositCount": reconciled_idle_deposit_count,
            "recomputedFullyFinalizedEffectCount": fully_proven_count,
            "recomputedMainOutflowRaw": main_outflow,
            "recomputedMainInflowRaw": main_inflow,
            "recomputedMainNetOutflowRaw": confirmed_net_outflow,
            "baselineCohortVaultCount": baseline_cohort_vault_ids.len(),
            "baselineCohortIdentityExact": baseline_cohort_identity_exact,
            "recomputedBaselineCohortMainOutflowRaw": baseline_cohort_main_outflow,
            "recomputedBaselineCohortMainInflowRaw": baseline_cohort_main_inflow,
            "recomputedBaselineCohortMainNetOutflowRaw": baseline_cohort_confirmed_net_outflow,
            "reportedMainFlowsMatch": reported_main_flows_match,
            "recomputedDepositAdjustedReductionRaw": adjusted_reduction,
            "reductionCoversBaselineCohortNetOutflow": reduction_covers_cohort_net_outflow,
            "baselineReductionPass": baseline_reduction_pass,
            "freshPositionsMatch": positions_match,
            "largestAccountsPass": largest_accounts_pass,
            "largestRankedCount": largest_vaults.len(),
            "largestRowsValid": largest_rows_valid,
            "largestRecomputedRankedPrincipalUsdMicros": recomputed_ranked_principal,
            "largestRecomputedCoveredPrincipalUsdMicros": recomputed_covered_principal,
            "largestRecomputedTopThreeBlockedCount": recomputed_top_three_blocked,
            "largestRecomputedMovedCount": recomputed_moved_count,
            "movementSlosPass": slos_pass,
            "recomputedMaximumSignedToSubmittedMilliseconds": recomputed_maximum_submission_millis,
            "recomputedMaximumSignedToReconciledMilliseconds": recomputed_maximum_reconciliation_millis,
            "terminalAndSafetyCountersZero": terminal_counters_zero,
            "databaseDeadlockCount": movement.get("databaseDeadlockCount"),
            "duplicateMovementCount": movement.get("duplicateMovementCount"),
            "embeddedPassesIgnored": {
                "movement": movement.get("pass"),
                "movementSlos": slo.get("pass"),
            },
        }),
    )
}

fn production_performance_checks(binding: &ProductionEvidenceBinding) -> Vec<VerifierCheck> {
    let fleet = production_complete_fleet_subcheck(binding);
    let fleet_failure = (!matches!(fleet.verdict, Verdict::Pass)).then_some(fleet.name);
    let movement = production_movement_subcheck(binding);
    let movement_failure = (!matches!(movement.verdict, Verdict::Pass)).then_some(movement.name);
    vec![
        check(
            10,
            "complete_fleet_evaluation_and_economic_draining",
            fleet.verdict,
            fleet_failure,
            json!({
                "cluster": binding.artifact.scope.cluster,
                "capturedAt": binding.artifact.captured_at,
            }),
            vec![fleet],
        ),
        check(
            11,
            "correct_production_movement_and_reconciliation",
            movement.verdict,
            movement_failure,
            json!({
                "cluster": binding.artifact.scope.cluster,
                "cutoverAt": binding.artifact.scope.cutover_at,
                "capturedAt": binding.artifact.captured_at,
            }),
            vec![movement],
        ),
    ]
}

fn parse_cli() -> Result<Option<Cli>, Box<dyn Error>> {
    let mut implementation = false;
    let mut end_state = false;
    let mut json_output = false;
    let mut database_url = None;
    let mut isolated_database = false;
    let mut collect_repository_evidence = false;
    let mut repository_root = None;
    let mut runtime_evidence_json = None;
    let mut production_evidence_json = None;
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--implementation" => implementation = true,
            "--end-state" => end_state = true,
            "--json" => json_output = true,
            "--isolated-database" => isolated_database = true,
            "--collect-repository-evidence" => collect_repository_evidence = true,
            "--repository-root" => {
                repository_root = Some(PathBuf::from(
                    args.next().ok_or("--repository-root requires a path")?,
                ));
            }
            "--runtime-evidence-json" => {
                runtime_evidence_json = Some(PathBuf::from(
                    args.next()
                        .ok_or("--runtime-evidence-json requires a path")?,
                ));
            }
            "--production-evidence-json" => {
                production_evidence_json = Some(PathBuf::from(
                    args.next()
                        .ok_or("--production-evidence-json requires a path")?,
                ));
            }
            "--database-url" => {
                database_url = Some(
                    args.next()
                        .ok_or("--database-url requires a PostgreSQL URL")?,
                );
            }
            "--help" | "-h" => {
                println!(
                    "fleet-orchestration-verifier (--implementation|--end-state) [--json] [--collect-repository-evidence [--repository-root PATH]] [--runtime-evidence-json PATH] [--production-evidence-json PATH] [--isolated-database [--database-url URL|FLEET_VERIFY_DATABASE_URL]]\n\n--implementation verifies Checks 1-7 only. --end-state additionally requires a fresh source-bound schema-v1 production artifact, independently recomputes Checks 8-11 from raw measurements, and succeeds only for literal END_STATE: PASS."
                );
                return Ok(None);
            }
            other if other.starts_with("--database-url=") => {
                database_url = Some(other["--database-url=".len()..].to_owned());
            }
            other if other.starts_with("--repository-root=") => {
                repository_root = Some(PathBuf::from(&other["--repository-root=".len()..]));
            }
            other if other.starts_with("--runtime-evidence-json=") => {
                runtime_evidence_json =
                    Some(PathBuf::from(&other["--runtime-evidence-json=".len()..]));
            }
            other if other.starts_with("--production-evidence-json=") => {
                production_evidence_json =
                    Some(PathBuf::from(&other["--production-evidence-json=".len()..]));
            }
            other => return Err(format!("unknown argument: {other}").into()),
        }
    }
    if isolated_database && database_url.is_none() {
        database_url = env::var("FLEET_VERIFY_DATABASE_URL")
            .ok()
            .filter(|value| !value.trim().is_empty());
    }
    Ok(Some(Cli {
        implementation,
        end_state,
        json_output,
        database_url,
        isolated_database,
        collect_repository_evidence,
        repository_root,
        runtime_evidence_json,
        production_evidence_json,
    }))
}

fn failed_database_evidence(error: &dyn Error) -> DatabaseEvidence {
    let evidence = json!({
        "error": error.to_string(),
        "databaseUrl": "REDACTED",
    });
    DatabaseEvidence {
        migration_subchecks: vec![subcheck(
            "isolated_database_verification_failed",
            false,
            evidence.clone(),
        )],
        discovery_subchecks: vec![subcheck(
            "isolated_database_discovery_checks_failed",
            false,
            evidence.clone(),
        )],
        alt_subchecks: vec![subcheck(
            "isolated_database_alt_lane_checks_failed",
            false,
            evidence.clone(),
        )],
        execution_subchecks: vec![subcheck(
            "isolated_database_execution_checks_failed",
            false,
            evidence,
        )],
    }
}

async fn run() -> Result<ExitCode, Box<dyn Error>> {
    let Some(cli) = parse_cli()? else {
        return Ok(ExitCode::SUCCESS);
    };
    if cli.implementation == cli.end_state {
        return Err("exactly one of --implementation or --end-state is required".into());
    }
    if cli.database_url.is_some() != cli.isolated_database {
        return Err(
            "--isolated-database requires --database-url or FLEET_VERIFY_DATABASE_URL; isolated database checks are never implicit"
                .into(),
        );
    }
    if cli.repository_root.is_some() && !(cli.collect_repository_evidence || cli.end_state) {
        return Err("--repository-root requires --collect-repository-evidence".into());
    }
    if cli.runtime_evidence_json.is_some() && !(cli.collect_repository_evidence || cli.end_state) {
        return Err(
            "--runtime-evidence-json requires --collect-repository-evidence for HEAD and source binding"
                .into(),
        );
    }
    if cli.end_state && cli.production_evidence_json.is_none() {
        return Err("--end-state requires --production-evidence-json".into());
    }
    if cli.production_evidence_json.is_some() && !cli.end_state {
        return Err("--production-evidence-json is only valid with --end-state".into());
    }

    let collect_repository_evidence = cli.collect_repository_evidence || cli.end_state;
    let collected_repository_root = collect_repository_evidence
        .then(|| repository_root(cli.repository_root.as_deref()))
        .transpose()?;
    let production_evidence = cli
        .production_evidence_json
        .as_deref()
        .map(|path| {
            load_production_evidence(
                path,
                collected_repository_root
                    .as_deref()
                    .ok_or("production evidence requires a repository root")?,
            )
        })
        .transpose()?;
    let local_evidence = collected_repository_root
        .as_deref()
        .map(collect_local_evidence)
        .transpose()?;
    let runtime_evidence = if let Some(path) = cli.runtime_evidence_json.as_deref() {
        Some(load_runtime_evidence(
            path,
            local_evidence
                .as_ref()
                .ok_or("runtime evidence requires collected repository evidence")?,
        )?)
    } else {
        None
    };

    let database_evidence = if let Some(database_url) = cli.database_url.as_deref() {
        Some(
            match isolated_database_evidence(database_url, collected_repository_root.as_deref())
                .await
            {
                Ok(evidence) => evidence,
                Err(error) => failed_database_evidence(error.as_ref()),
            },
        )
    } else {
        None
    };
    let isolated_database_verdict = database_evidence
        .as_ref()
        .map(|evidence| {
            aggregate_verdicts(
                evidence
                    .migration_subchecks
                    .iter()
                    .chain(&evidence.discovery_subchecks)
                    .chain(&evidence.alt_subchecks)
                    .chain(&evidence.execution_subchecks)
                    .map(|subcheck| subcheck.verdict),
            )
        })
        .unwrap_or(Verdict::NotRun);
    let isolated_database_first_blocking_subcheck =
        database_evidence.as_ref().and_then(|evidence| {
            evidence
                .migration_subchecks
                .iter()
                .chain(&evidence.discovery_subchecks)
                .chain(&evidence.alt_subchecks)
                .chain(&evidence.execution_subchecks)
                .find(|subcheck| subcheck.verdict != Verdict::Pass)
                .map(|subcheck| (subcheck.name, subcheck.verdict))
        });

    let mut checks =
        implementation_checks(database_evidence, local_evidence, runtime_evidence.as_ref())?;
    let implementation_verdict = aggregate_verdicts(checks.iter().map(|check| check.verdict));
    let (deployment_verdict, production_performance_verdict) = if let Some(binding) =
        production_evidence.as_ref()
    {
        let deployment_checks = production_deployment_checks(binding, runtime_evidence.as_ref());
        let production_checks = production_performance_checks(binding);
        let deployment_verdict =
            aggregate_verdicts(deployment_checks.iter().map(|check| check.verdict));
        let production_performance_verdict =
            aggregate_verdicts(production_checks.iter().map(|check| check.verdict));
        checks.extend(deployment_checks);
        checks.extend(production_checks);
        (deployment_verdict, production_performance_verdict)
    } else {
        (Verdict::NotRun, Verdict::NotRun)
    };
    let end_state_verdict = aggregate_verdicts([
        implementation_verdict,
        deployment_verdict,
        production_performance_verdict,
    ]);
    // An isolated-database invocation is already an explicit verifier scope.
    // When no repository/runtime evidence was requested, report and exit on
    // every isolated database subcheck rather than unrelated NOT_RUN evidence
    // from the broader implementation/end-state verifier.
    let isolated_database_only =
        cli.isolated_database && !collect_repository_evidence && runtime_evidence.is_none();
    let (requested_scope, requested_scope_verdict) = if cli.end_state {
        ("END_STATE", end_state_verdict)
    } else if isolated_database_only {
        ("ISOLATED_DATABASE", isolated_database_verdict)
    } else {
        ("IMPLEMENTATION", implementation_verdict)
    };
    let first_blocking_check = if isolated_database_only {
        isolated_database_first_blocking_subcheck.map(|(name, verdict)| {
            json!({
                "name": name,
                "verdict": verdict,
            })
        })
    } else {
        checks
            .iter()
            .find(|check| !matches!(check.verdict, Verdict::Pass))
            .map(|check| {
                json!({
                    "id": check.id,
                    "name": check.name,
                    "verdict": check.verdict,
                    "invariant": check.first_failing_invariant,
                })
            })
    };
    let output = json!({
        "status": requested_scope_verdict,
        "requestedScope": requested_scope,
        "requestedScopeStatus": requested_scope_verdict,
        "implementation": implementation_verdict,
        "isolatedDatabase": isolated_database_verdict,
        "deployment": deployment_verdict,
        "productionPerformance": production_performance_verdict,
        "endState": end_state_verdict,
        "scopeVerdicts": {
            "IMPLEMENTATION": implementation_verdict,
            "ISOLATED_DATABASE": isolated_database_verdict,
            "DEPLOYMENT": deployment_verdict,
            "PRODUCTION_PERFORMANCE": production_performance_verdict,
            "END_STATE": end_state_verdict,
        },
        "firstBlockingCheck": first_blocking_check,
        "checks": checks,
    });
    if cli.json_output {
        println!("{}", serde_json::to_string(&output)?);
    } else {
        println!("{}", serde_json::to_string_pretty(&output)?);
    }

    Ok(if requested_scope_verdict == Verdict::Pass {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    })
}

#[tokio::main]
async fn main() -> ExitCode {
    match run().await {
        Ok(code) => code,
        Err(error) => {
            eprintln!("{error}");
            ExitCode::FAILURE
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use loyal_yield_orchestrator::fleet_orchestration::deterministic_fleet_route_source_contract_fixtures;

    #[tokio::test]
    #[ignore = "requires AUTOSWAP_REBALANCE_VERIFY_DATABASE_URL for a disposable PostgreSQL database"]
    async fn autoswap_rebalance_executes_opt_in_lock_queries() {
        let database_url = env::var("AUTOSWAP_REBALANCE_VERIFY_DATABASE_URL")
            .expect("AUTOSWAP_REBALANCE_VERIFY_DATABASE_URL must be set");
        let fixture = DatabaseFixture::connect(&database_url)
            .await
            .expect("connect to disposable verifier database");

        let activated = activate_cross_mint_fixture(&fixture, "autoswap_lock_alias")
            .await
            .expect("activate an Autoswap rebalance");
        let continuation = fixture
            .client
            .claim_cross_mint_continuation(
                &activated.movement.cluster,
                "autoswap-lock-alias-worker",
                60,
            )
            .await
            .expect("claim Autoswap continuation")
            .expect("activated Autoswap rebalance must be claimable");
        let source_idle_account =
            format!("source-idle:{}", activated.movement.decision_id.as_i64());
        let input = cross_mint_leg_input(
            &fixture,
            &activated.opportunity_lease,
            &continuation,
            "autoswap-lock-alias-withdraw",
            CrossMintMovementLeg::Withdraw,
            CrossMintLegPurpose::OptimizeYield,
            1,
            CrossMintExpectedEffect {
                debit: None,
                credit_mint: Some("USDC".to_owned()),
                credit_token_account: Some(source_idle_account),
                minimum_credit_amount_raw: Some(850_000),
            },
        )
        .await
        .expect("prepare the first Autoswap leg");
        let submission = fixture
            .client
            .append_cross_mint_leg(&continuation, input)
            .await
            .expect("publish the first Autoswap leg");

        assert!(submission.id > 0);
    }

    #[test]
    fn literal_source_evidence_contract_gate_accepts_code_owned_fixtures() {
        let code_owned = deterministic_fleet_route_source_contract_fixtures().unwrap();
        let serialized = serde_json::to_value(code_owned).unwrap();
        let verifier_fixture: RuntimeSourceEvidenceContractFixtures =
            serde_json::from_value(serialized).unwrap();

        let result = runtime_source_evidence_contract_subcheck(&verifier_fixture);

        assert_eq!(
            result.name,
            "planner_executor_source_evidence_is_kind_scoped"
        );
        assert_eq!(result.verdict, Verdict::Pass);
    }

    #[test]
    fn live_role_scope_keeps_observability_keys_without_fingerprinting_secret() {
        let keys = [
            "NEON_DATABASE_URL",
            "OBSERVABILITY_ENABLED",
            "OBSERVABILITY_ENVIRONMENT",
            "OBSERVABILITY_OTLP_ENDPOINT",
            "OBSERVABILITY_INGESTION_API_KEY",
        ]
        .into_iter()
        .map(str::to_owned)
        .collect::<BTreeSet<_>>();

        assert_eq!(role_service_scoped_env_keys(&keys), keys);
        let fingerprinted = role_scope_value_keys("loyal-fleet-opportunity-planner", &keys);
        assert!(fingerprinted.contains("OBSERVABILITY_ENABLED"));
        assert!(fingerprinted.contains("OBSERVABILITY_ENVIRONMENT"));
        assert!(fingerprinted.contains("OBSERVABILITY_OTLP_ENDPOINT"));
        assert!(!fingerprinted.contains("OBSERVABILITY_INGESTION_API_KEY"));
    }
}
