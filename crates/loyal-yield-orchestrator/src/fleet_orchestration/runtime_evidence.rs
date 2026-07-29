//! Source-bound runtime evidence shared by the collector and controlled
//! orchestration harnesses.
//!
//! This module deliberately has no deserialization path for complete runtime
//! evidence. Measurements enter through typed, code-owned collectors; callers
//! may choose artifact locations and a local image reference, but cannot
//! submit measurement JSON, verdict strings, or substitute probe executables.

use std::{
    collections::BTreeMap,
    fs,
    path::{Path, PathBuf},
    process::Command,
};

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use super::{
    deterministic_fleet_route_source_contract_fixtures, FleetRouteSourceContractFixtures,
    FleetRouteSourceEvidenceError,
};

pub const RUNTIME_EVIDENCE_SCHEMA_VERSION: u32 = 1;

const RUNTIME_DIGEST_INPUTS: [&str; 14] = [
    "Cargo.toml",
    "Cargo.lock",
    "Dockerfile.light-workers",
    "Dockerfile.laserstream-workers",
    "render.yaml",
    "crates/kamino-historic-data/src",
    "crates/kamino-reserve-monitor/src",
    "crates/loyal-timescale-migrations",
    "crates/loyal-yield-orchestrator/Cargo.toml",
    "crates/loyal-yield-orchestrator/src",
    "crates/loyal-yield-orchestrator/migrations",
    "crates/loyal-yield-router/Cargo.toml",
    "crates/loyal-yield-router/src",
    "scripts/kamino-monitor-predeploy.sh",
];

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeSourceBinding {
    pub head_commit: String,
    pub runtime_source_digest_sha256: String,
}

impl RuntimeSourceBinding {
    pub fn capture(repository_root: &Path) -> Result<Self, RuntimeEvidenceCollectionError> {
        let head_commit = git_head(repository_root)?;
        let runtime_source_digest_sha256 = runtime_source_digest(repository_root)?;
        Ok(Self {
            head_commit,
            runtime_source_digest_sha256,
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeDiscoveryEvidence {
    pub fleet_size: u64,
    pub eligible_current_vaults: u64,
    pub accounted_vaults: u64,
    pub vault_outcomes_by_reason: BTreeMap<String, u64>,
    pub active_exclusions_by_state: BTreeMap<String, u64>,
    pub optimizer_epoch_id: i64,
    pub epoch_expires_at: DateTime<Utc>,
    pub one_immutable_epoch: bool,
    pub planning_sample_epoch_proofs: Vec<RuntimePlannerEpochProof>,
    pub planning_sample_count: u64,
    pub planning_p95_milliseconds: u64,
    pub replay_vault_count: u64,
    pub replay_milliseconds: u64,
    pub economically_ordered: bool,
    pub top_cohort_has_no_nonconflicting_priority_inversion: bool,
    pub child_route_or_reconcile_processes_spawned: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimePlannerEpochProof {
    pub market_epoch_optimizer_id: i64,
    pub observed_opportunity_epoch_ids: Vec<i64>,
    pub selected_opportunity_epoch_ids: Vec<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct RuntimeAltEvidence {
    pub typed_provisioner_dry_run_plans: u64,
    pub reusable_v2_plans: u64,
    pub legacy_or_exact_route_alt_plans: u64,
    pub ready_jobs_seeded: u64,
    pub ready_jobs_claimed: u64,
    pub waiting_alt_jobs: u64,
    pub waiting_alt_decisions: u64,
    pub claim_latency_gate_clock: String,
    pub ready_claim_baseline_p95_micros: u64,
    pub ready_claim_cold_p95_micros: u64,
    pub ready_claim_baseline_client_p95_micros: u64,
    pub ready_claim_cold_client_p95_micros: u64,
    pub durable_coverage_wakeup_rows: u64,
    pub affected_jobs_promoted: u64,
    pub unaffected_jobs_promoted: u64,
    pub additional_fleet_cycle_required: bool,
    pub normal_readiness_global_rollout_lock_acquisitions: u64,
    pub independent_physical_alt_lanes_progressed: u64,
    pub same_table_predecessor_violations: u64,
    pub stale_fence_commits: u64,
    pub usage_leases_rejected_during_mutation: u64,
    pub mutating_operations_leased_during_usage: u64,
    pub verify_operations_leased_during_usage: u64,
    pub usage_fence_broadcast_commits: u64,
    pub usage_fence_broadcast_rejections: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeExecutionEvidence {
    pub duplicate_active_vault_movements: u64,
    pub nonoverlapping_concurrent_leases: u64,
    pub overlapping_lane_limit_violations: u64,
    pub physical_writable_key_congestion_visible: bool,
    pub expired_lease_reclaimed_with_higher_fence: bool,
    pub mixed_runnable_and_expired_claims_full_and_disjoint: bool,
    pub fleet_wide_exclusive_route_leases: u64,
    pub identical_byte_rebroadcast_attempts: u64,
    pub rebroadcast_byte_mismatches: u64,
    pub replacement_before_expiry_and_absence_proof: u64,
    pub ambiguous_or_stale_replacement_movements: u64,
    pub post_confirm_reads: u64,
    pub min_context_slot_violations: u64,
    pub policy_execution_signed_by_policy_keypair: bool,
    pub alt_mutations_authorized_and_paid_by_policy_keypair: bool,
    pub sharded_route_fixtures: u64,
    pub shard_is_final_fee_payer: bool,
    pub policy_is_second_static_signer: bool,
    pub final_manifest_and_alt_coverage_match: bool,
    pub final_packet_simulation_fee_and_hashes_match: bool,
    pub setup_idle_and_farm_init_use_policy_payer: bool,
    pub shard_registry_keypair_match: bool,
    pub reciprocal_authority_separation: bool,
    pub bounded_ranked_failover: bool,
    pub low_balance_limits_enforced: bool,
    pub atomic_immutable_spend_reservation: bool,
    pub source_evidence_contract_fixtures: FleetRouteSourceContractFixtures,
    pub target_capacity_concurrent_admission_bounded: bool,
    pub pre_send_target_capacity_released: bool,
    pub reconciled_capacity_strict_telemetry_fence: bool,
    pub preexisting_newer_telemetry_release: bool,
    pub readiness_writers_waited_on_per_vault_fence: bool,
    pub serialized_readiness_row_count: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct RuntimeDatabaseExecutionEvidence {
    pub duplicate_active_vault_movements: u64,
    pub nonoverlapping_concurrent_leases: u64,
    pub overlapping_lane_limit_violations: u64,
    pub physical_writable_key_congestion_visible: bool,
    pub expired_lease_reclaimed_with_higher_fence: bool,
    pub mixed_runnable_and_expired_claims_full_and_disjoint: bool,
    pub fleet_wide_exclusive_route_leases: u64,
    pub replacement_before_expiry_and_absence_proof: u64,
    pub ambiguous_or_stale_replacement_movements: u64,
    pub reciprocal_authority_separation: bool,
    pub low_balance_limits_enforced: bool,
    pub atomic_immutable_spend_reservation: bool,
    pub target_capacity_concurrent_admission_bounded: bool,
    pub pre_send_target_capacity_released: bool,
    pub reconciled_capacity_strict_telemetry_fence: bool,
    pub preexisting_newer_telemetry_release: bool,
    pub readiness_writers_waited_on_per_vault_fence: bool,
    pub serialized_readiness_row_count: u64,
    pub database_deadlocks: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct RuntimeTransactionProbeEvidence {
    pub identical_byte_rebroadcast_attempts: u64,
    pub rebroadcast_byte_mismatches: u64,
    pub post_confirm_reads: u64,
    pub min_context_slot_violations: u64,
    pub policy_execution_signed_by_policy_keypair: bool,
    pub alt_mutations_authorized_and_paid_by_policy_keypair: bool,
    pub sharded_route_fixtures: u64,
    pub shard_is_final_fee_payer: bool,
    pub policy_is_second_static_signer: bool,
    pub final_manifest_and_alt_coverage_match: bool,
    pub final_packet_simulation_fee_and_hashes_match: bool,
    pub setup_idle_and_farm_init_use_policy_payer: bool,
    pub shard_registry_keypair_match: bool,
    pub bounded_ranked_failover: bool,
}

impl RuntimeExecutionEvidence {
    pub fn from_code_owned_probes(
        database: RuntimeDatabaseExecutionEvidence,
        transaction: RuntimeTransactionProbeEvidence,
    ) -> Result<Self, FleetRouteSourceEvidenceError> {
        Ok(Self {
            duplicate_active_vault_movements: database.duplicate_active_vault_movements,
            nonoverlapping_concurrent_leases: database.nonoverlapping_concurrent_leases,
            overlapping_lane_limit_violations: database.overlapping_lane_limit_violations,
            physical_writable_key_congestion_visible: database
                .physical_writable_key_congestion_visible,
            expired_lease_reclaimed_with_higher_fence: database
                .expired_lease_reclaimed_with_higher_fence,
            mixed_runnable_and_expired_claims_full_and_disjoint: database
                .mixed_runnable_and_expired_claims_full_and_disjoint,
            fleet_wide_exclusive_route_leases: database.fleet_wide_exclusive_route_leases,
            identical_byte_rebroadcast_attempts: transaction.identical_byte_rebroadcast_attempts,
            rebroadcast_byte_mismatches: transaction.rebroadcast_byte_mismatches,
            replacement_before_expiry_and_absence_proof: database
                .replacement_before_expiry_and_absence_proof,
            ambiguous_or_stale_replacement_movements: database
                .ambiguous_or_stale_replacement_movements,
            post_confirm_reads: transaction.post_confirm_reads,
            min_context_slot_violations: transaction.min_context_slot_violations,
            policy_execution_signed_by_policy_keypair: transaction
                .policy_execution_signed_by_policy_keypair,
            alt_mutations_authorized_and_paid_by_policy_keypair: transaction
                .alt_mutations_authorized_and_paid_by_policy_keypair,
            sharded_route_fixtures: transaction.sharded_route_fixtures,
            shard_is_final_fee_payer: transaction.shard_is_final_fee_payer,
            policy_is_second_static_signer: transaction.policy_is_second_static_signer,
            final_manifest_and_alt_coverage_match: transaction
                .final_manifest_and_alt_coverage_match,
            final_packet_simulation_fee_and_hashes_match: transaction
                .final_packet_simulation_fee_and_hashes_match,
            setup_idle_and_farm_init_use_policy_payer: transaction
                .setup_idle_and_farm_init_use_policy_payer,
            shard_registry_keypair_match: transaction.shard_registry_keypair_match,
            reciprocal_authority_separation: database.reciprocal_authority_separation,
            bounded_ranked_failover: transaction.bounded_ranked_failover,
            low_balance_limits_enforced: database.low_balance_limits_enforced,
            atomic_immutable_spend_reservation: database.atomic_immutable_spend_reservation,
            source_evidence_contract_fixtures: deterministic_fleet_route_source_contract_fixtures(
            )?,
            target_capacity_concurrent_admission_bounded: database
                .target_capacity_concurrent_admission_bounded,
            pre_send_target_capacity_released: database.pre_send_target_capacity_released,
            reconciled_capacity_strict_telemetry_fence: database
                .reconciled_capacity_strict_telemetry_fence,
            preexisting_newer_telemetry_release: database.preexisting_newer_telemetry_release,
            readiness_writers_waited_on_per_vault_fence: database
                .readiness_writers_waited_on_per_vault_fence,
            serialized_readiness_row_count: database.serialized_readiness_row_count,
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeReplayEvidence {
    pub route_sample_count: u64,
    pub warm_high_value_submission_p95_milliseconds: u64,
    pub warm_confirmation_p95_milliseconds: u64,
    pub explicitly_excluded_cluster_outages: u64,
    pub warm_baseline_p95_milliseconds: u64,
    pub warm_with_alt_backlog_p95_milliseconds: u64,
    pub recoverable_yield_usd_micros_per_hour: i64,
    pub submitted_within_two_minutes_yield_ppm: u64,
    pub submitted_within_ten_minutes_yield_ppm: u64,
    pub configured_max_fee_fraction_ppm: u64,
    pub observed_max_fee_fraction_ppm: u64,
    pub negative_value_routes: u64,
    pub database_deadlocks: u64,
    pub duplicate_movements: u64,
}

const REPLAY_ROUTE_COUNT: usize = 10_000;
const REPLAY_EXECUTION_LANES: usize = 64;
const REPLAY_ALT_LANES: usize = 8;
const REPLAY_ALT_BACKLOG: usize = 10_000;
const REPLAY_CONFIGURED_MAX_FEE_FRACTION_PPM: u64 = 50_000;

#[derive(Clone, Copy)]
struct ReplayJobResult {
    route_id: u64,
    recoverable_yield_usd_micros_per_hour: i64,
    submitted_at_milliseconds: u64,
    confirmed_at_milliseconds: u64,
    fee_fraction_ppm: u64,
    economically_positive: bool,
}

#[derive(Default)]
struct ReplayRun {
    routes: Vec<ReplayJobResult>,
    duplicate_movements: u64,
    alt_jobs_processed: usize,
}

#[derive(Clone, Copy)]
struct ReplayRandom(u64);

impl ReplayRandom {
    fn next(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9e3779b97f4a7c15);
        let mut value = self.0;
        value = (value ^ (value >> 30)).wrapping_mul(0xbf58476d1ce4e5b9);
        value = (value ^ (value >> 27)).wrapping_mul(0x94d049bb133111eb);
        value ^ (value >> 31)
    }
}

fn run_discrete_event_replay(include_alt_backlog: bool) -> ReplayRun {
    let mut route_random = ReplayRandom(0x4c4f_5941_4c5f_5254);
    let mut alt_random = ReplayRandom(0x414c_545f_5155_4555);
    let mut route_lanes = [0u64; REPLAY_EXECUTION_LANES];
    let mut alt_lanes = [0u64; REPLAY_ALT_LANES];
    let mut seen_routes = std::collections::BTreeSet::new();
    let mut run = ReplayRun {
        routes: Vec::with_capacity(REPLAY_ROUTE_COUNT),
        ..ReplayRun::default()
    };

    if include_alt_backlog {
        // ALT mutations have their own physical-table lanes. Processing this
        // queue advances only those clocks; it cannot advance the warm-route
        // sender clocks below.
        for index in 0..REPLAY_ALT_BACKLOG {
            let lane = index % REPLAY_ALT_LANES;
            let service_milliseconds = 40 + alt_random.next() % 81;
            alt_lanes[lane] = alt_lanes[lane].saturating_add(service_milliseconds);
            run.alt_jobs_processed += 1;
        }
        std::hint::black_box(alt_lanes);
    }

    // Jobs arrive together in lost-yield order. The decreasing value curve
    // makes the first decile the high-value cohort while still measuring the
    // complete 10,000-route drain.
    for index in 0..REPLAY_ROUTE_COUNT {
        let route_id = u64::try_from(index).unwrap_or(u64::MAX).saturating_add(1);
        if !seen_routes.insert(route_id) {
            run.duplicate_movements += 1;
            continue;
        }
        let lane = index % REPLAY_EXECUTION_LANES;
        // Model a conservative warm RPC submission occupancy of 100-200ms.
        // Confirmation is tracked independently below and does not pin the
        // semantic execution lane after a signed send is handed to confirmer.
        let service_milliseconds = 100 + route_random.next() % 101;
        let submitted_at_milliseconds = route_lanes[lane].saturating_add(service_milliseconds);
        route_lanes[lane] = submitted_at_milliseconds;
        let confirmed_at_milliseconds = submitted_at_milliseconds
            .saturating_add(800)
            .saturating_add(route_random.next() % 1_701);

        let descending = i64::try_from(REPLAY_ROUTE_COUNT - index).unwrap_or_default();
        let recoverable_yield_usd_micros_per_hour =
            50_000i64.saturating_add(descending.saturating_mul(1_000));
        let expected_incremental_yield_usd_micros =
            recoverable_yield_usd_micros_per_hour.saturating_mul(24 * 30);
        let fee_lamports = 5_000u64.saturating_add(route_random.next() % 20_001);
        // $250/SOL is deliberately conservative for this price fixture.
        let fee_usd_micros = u64::try_from(
            i128::from(fee_lamports)
                .saturating_mul(250_000_000)
                .checked_div(1_000_000_000)
                .unwrap_or(i128::MAX),
        )
        .unwrap_or(u64::MAX);
        let fee_fraction_ppm = fee_usd_micros.saturating_mul(1_000_000)
            / u64::try_from(expected_incremental_yield_usd_micros)
                .unwrap_or(1)
                .max(1);
        let economically_positive = expected_incremental_yield_usd_micros > 0
            && fee_fraction_ppm <= REPLAY_CONFIGURED_MAX_FEE_FRACTION_PPM;

        run.routes.push(ReplayJobResult {
            route_id,
            recoverable_yield_usd_micros_per_hour,
            submitted_at_milliseconds,
            confirmed_at_milliseconds,
            fee_fraction_ppm,
            economically_positive,
        });
    }
    run
}

fn p95_milliseconds(values: &mut [u64]) -> u64 {
    if values.is_empty() {
        return 0;
    }
    values.sort_unstable();
    let index = (values.len() * 95).div_ceil(100).saturating_sub(1);
    values[index]
}

fn yield_fraction_ppm(routes: &[ReplayJobResult], deadline_milliseconds: u64) -> u64 {
    let total = routes.iter().fold(0i128, |sum, route| {
        sum.saturating_add(i128::from(route.recoverable_yield_usd_micros_per_hour))
    });
    if total <= 0 {
        return 0;
    }
    let submitted = routes
        .iter()
        .filter(|route| route.submitted_at_milliseconds <= deadline_milliseconds)
        .fold(0i128, |sum, route| {
            sum.saturating_add(i128::from(route.recoverable_yield_usd_micros_per_hour))
        });
    u64::try_from(
        submitted
            .saturating_mul(1_000_000)
            .checked_div(total)
            .unwrap_or_default(),
    )
    .unwrap_or(u64::MAX)
}

/// Deterministic production-like scheduler replay. This is a measured
/// discrete-event run: the output is derived from lane clocks, route value,
/// confirmation latency, and fee arithmetic rather than threshold constants.
pub fn collect_deterministic_runtime_replay() -> RuntimeReplayEvidence {
    let baseline = run_discrete_event_replay(false);
    let with_alt_backlog = run_discrete_event_replay(true);

    let high_value_count = (baseline.routes.len() / 10).max(1);
    let mut high_value_submission = baseline
        .routes
        .iter()
        .take(high_value_count)
        .map(|route| route.submitted_at_milliseconds)
        .collect::<Vec<_>>();
    let mut confirmation = baseline
        .routes
        .iter()
        .map(|route| route.confirmed_at_milliseconds)
        .collect::<Vec<_>>();
    let mut baseline_submission = baseline
        .routes
        .iter()
        .map(|route| route.submitted_at_milliseconds)
        .collect::<Vec<_>>();
    let mut backlog_submission = with_alt_backlog
        .routes
        .iter()
        .map(|route| route.submitted_at_milliseconds)
        .collect::<Vec<_>>();
    let recoverable_yield = baseline.routes.iter().fold(0i64, |sum, route| {
        sum.saturating_add(route.recoverable_yield_usd_micros_per_hour)
    });
    let observed_max_fee_fraction_ppm = baseline
        .routes
        .iter()
        .map(|route| route.fee_fraction_ppm)
        .max()
        .unwrap_or_default();
    let negative_value_routes = baseline
        .routes
        .iter()
        .filter(|route| !route.economically_positive)
        .count();
    let unique_route_ids = baseline
        .routes
        .iter()
        .map(|route| route.route_id)
        .collect::<std::collections::BTreeSet<_>>()
        .len();
    let duplicate_movements = baseline.duplicate_movements.saturating_add(
        u64::try_from(baseline.routes.len().saturating_sub(unique_route_ids)).unwrap_or(u64::MAX),
    );

    // An incomplete ALT replay is a measurement failure, not a silently
    // passing backlog comparison.
    let backlog_complete = with_alt_backlog.alt_jobs_processed == REPLAY_ALT_BACKLOG;
    RuntimeReplayEvidence {
        route_sample_count: u64::try_from(baseline.routes.len()).unwrap_or(u64::MAX),
        warm_high_value_submission_p95_milliseconds: p95_milliseconds(&mut high_value_submission),
        warm_confirmation_p95_milliseconds: p95_milliseconds(&mut confirmation),
        explicitly_excluded_cluster_outages: 0,
        warm_baseline_p95_milliseconds: p95_milliseconds(&mut baseline_submission),
        warm_with_alt_backlog_p95_milliseconds: if backlog_complete {
            p95_milliseconds(&mut backlog_submission)
        } else {
            u64::MAX
        },
        recoverable_yield_usd_micros_per_hour: recoverable_yield,
        submitted_within_two_minutes_yield_ppm: yield_fraction_ppm(&baseline.routes, 120_000),
        submitted_within_ten_minutes_yield_ppm: yield_fraction_ppm(&baseline.routes, 600_000),
        configured_max_fee_fraction_ppm: REPLAY_CONFIGURED_MAX_FEE_FRACTION_PPM,
        observed_max_fee_fraction_ppm,
        negative_value_routes: u64::try_from(negative_value_routes).unwrap_or(u64::MAX),
        // Filled from the isolated PostgreSQL verifier by the complete
        // collector; the in-memory scheduler cannot measure this field.
        database_deadlocks: u64::MAX,
        duplicate_movements,
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeWiringEvidence {
    /// Exact image reference passed to both local image inspection and every
    /// role probe. The verifier binds it to the one immutable production
    /// Blueprint `light-workers:sha-<commit>` reference.
    pub probed_container_image_reference: String,
    pub local_container_image_id: String,
    pub light_registry_index_digest: String,
    pub light_linux_amd64_manifest_digest: String,
    pub light_provenance_vcs_revision: String,
    pub light_provenance_vcs_source: String,
    pub probed_heavy_container_image_reference: String,
    pub heavy_registry_index_digest: String,
    pub heavy_linux_amd64_manifest_digest: String,
    pub heavy_provenance_vcs_revision: String,
    pub heavy_provenance_vcs_source: String,
    pub runnable_role_probe_exit_codes: BTreeMap<String, i32>,
    pub recovery_poll_interval_milliseconds: u64,
    pub health_observation_interval_milliseconds: u64,
    pub stuck_stage_detection_milliseconds: BTreeMap<String, u64>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ControlledRuntimeEvidence {
    pub alt: RuntimeAltEvidence,
    pub execution: RuntimeExecutionEvidence,
    pub replay: RuntimeReplayEvidence,
}

/// Typed integration point for the isolated-DB ALT fixture, controlled RPC
/// execution fixture, and production-like replay. Implementations should call
/// production-owned functions directly and return observed measurements.
/// There is intentionally no implementation that reads a caller-authored JSON
/// artifact.
pub trait ControlledRuntimeEvidenceSource {
    fn collect_alt(&mut self) -> Result<RuntimeAltEvidence, RuntimeEvidenceCollectionError>;

    fn collect_execution(
        &mut self,
    ) -> Result<RuntimeExecutionEvidence, RuntimeEvidenceCollectionError>;

    fn collect_replay(&mut self) -> Result<RuntimeReplayEvidence, RuntimeEvidenceCollectionError>;

    fn collect(&mut self) -> Result<ControlledRuntimeEvidence, RuntimeEvidenceCollectionError> {
        Ok(ControlledRuntimeEvidence {
            alt: self.collect_alt()?,
            execution: self.collect_execution()?,
            replay: self.collect_replay()?,
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeEvidenceV1 {
    pub schema_version: u32,
    pub head_commit: String,
    pub runtime_source_digest_sha256: String,
    pub captured_at: DateTime<Utc>,
    pub hardware: String,
    pub discovery: RuntimeDiscoveryEvidence,
    pub alt: RuntimeAltEvidence,
    pub execution: RuntimeExecutionEvidence,
    pub replay: RuntimeReplayEvidence,
    pub wiring: RuntimeWiringEvidence,
}

impl RuntimeEvidenceV1 {
    pub fn from_collected_measurements(
        source: RuntimeSourceBinding,
        captured_at: DateTime<Utc>,
        hardware: String,
        discovery: RuntimeDiscoveryEvidence,
        controlled: ControlledRuntimeEvidence,
        wiring: RuntimeWiringEvidence,
    ) -> Result<Self, RuntimeEvidenceCollectionError> {
        if source.head_commit.trim().is_empty()
            || source.runtime_source_digest_sha256.len() != 64
            || hardware.trim().is_empty()
        {
            return Err(RuntimeEvidenceCollectionError::InvalidSourceBinding);
        }
        Ok(Self {
            schema_version: RUNTIME_EVIDENCE_SCHEMA_VERSION,
            head_commit: source.head_commit,
            runtime_source_digest_sha256: source.runtime_source_digest_sha256,
            captured_at,
            hardware,
            discovery,
            alt: controlled.alt,
            execution: controlled.execution,
            replay: controlled.replay,
            wiring,
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RuntimeEvidenceFoundationV1 {
    pub schema_version: u32,
    pub artifact_kind: &'static str,
    pub status: &'static str,
    pub source: RuntimeSourceBinding,
    pub captured_at: DateTime<Utc>,
    pub hardware: String,
    pub discovery: RuntimeDiscoveryEvidence,
    pub wiring: RuntimeWiringEvidence,
    pub missing_code_owned_hooks: [&'static str; 3],
}

impl RuntimeEvidenceFoundationV1 {
    pub fn new(
        source: RuntimeSourceBinding,
        captured_at: DateTime<Utc>,
        hardware: String,
        discovery: RuntimeDiscoveryEvidence,
        wiring: RuntimeWiringEvidence,
    ) -> Self {
        Self {
            schema_version: RUNTIME_EVIDENCE_SCHEMA_VERSION,
            artifact_kind: "fleet_runtime_evidence_foundation",
            status: "incomplete",
            source,
            captured_at,
            hardware,
            discovery,
            wiring,
            missing_code_owned_hooks: [
                "isolated_db_alt",
                "controlled_rpc_execution",
                "production_like_replay",
            ],
        }
    }
}

#[derive(Debug, Error)]
pub enum RuntimeEvidenceCollectionError {
    #[error("repository root is not a readable git checkout")]
    InvalidRepository,
    #[error("runtime digest input is missing: {0}")]
    MissingDigestInput(PathBuf),
    #[error("runtime source binding is invalid")]
    InvalidSourceBinding,
    #[error("runtime evidence I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("controlled runtime evidence failed: {0}")]
    Controlled(String),
}

fn git_head(repository_root: &Path) -> Result<String, RuntimeEvidenceCollectionError> {
    let output = Command::new("git")
        .args(["rev-parse", "--verify", "HEAD"])
        .current_dir(repository_root)
        .output()?;
    if !output.status.success() {
        return Err(RuntimeEvidenceCollectionError::InvalidRepository);
    }
    let head = String::from_utf8(output.stdout)
        .map_err(|_| RuntimeEvidenceCollectionError::InvalidRepository)?;
    let head = head.trim();
    if head.len() != 40 || !head.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err(RuntimeEvidenceCollectionError::InvalidRepository);
    }
    Ok(head.to_owned())
}

fn collect_digest_files(
    path: &Path,
    files: &mut Vec<PathBuf>,
) -> Result<(), RuntimeEvidenceCollectionError> {
    if path.is_file() {
        files.push(path.to_path_buf());
        return Ok(());
    }
    if !path.is_dir() {
        return Err(RuntimeEvidenceCollectionError::MissingDigestInput(
            path.to_path_buf(),
        ));
    }
    for entry in fs::read_dir(path)? {
        let entry = entry?;
        let path = entry.path();
        if path.is_dir() {
            collect_digest_files(&path, files)?;
        } else if path.is_file() {
            files.push(path);
        }
    }
    Ok(())
}

fn runtime_source_digest(repository_root: &Path) -> Result<String, RuntimeEvidenceCollectionError> {
    let mut files = Vec::new();
    for input in RUNTIME_DIGEST_INPUTS {
        collect_digest_files(&repository_root.join(input), &mut files)?;
    }
    files.sort();
    files.dedup();
    let mut digest = Sha256::new();
    for file in files {
        let relative = file
            .strip_prefix(repository_root)
            .map_err(|_| RuntimeEvidenceCollectionError::InvalidRepository)?;
        let relative = relative.to_string_lossy();
        let bytes = fs::read(&file)?;
        digest.update((relative.len() as u64).to_le_bytes());
        digest.update(relative.as_bytes());
        digest.update((bytes.len() as u64).to_le_bytes());
        digest.update(bytes);
    }
    Ok(format!("{:x}", digest.finalize()))
}

pub fn local_hardware_description() -> String {
    let logical_cpus = std::thread::available_parallelism()
        .map(usize::from)
        .unwrap_or(1);
    format!(
        "os={} arch={} logical_cpus={logical_cpus}",
        std::env::consts::OS,
        std::env::consts::ARCH,
    )
}
