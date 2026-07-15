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
use loyal_yield_orchestrator::fleet_orchestration::{
    classify_authoritative_signature_status, evaluate_economics, evaluate_fresh_route_economics,
    fleet_worker_role_probe, functional_stuck_stage_fixture, functional_worker_resilience_fixture,
    material_frontier_deterministic_evidence, plan_capacity_aware_wave, rank_opportunities,
    route_fee_budget, run_deterministic_benchmark, schedule_authoritative_status_poll,
    AuthoritativeConfirmationDecision, AuthoritativePollUrgency, AuthoritativeSignatureStatus,
    CapacityBand, ConfirmationPollTrigger, EconomicPolicy, FleetStuckStage, FleetWorkerRole,
    FreshRouteEconomicsError, FreshRouteEconomicsInput, ImmutableMarketEpoch, IneligibleReason,
    MarketEpochReserve, MaterialFrontierDisposition, OpportunityInput, RebalanceOpportunityAdvance,
    RebalanceOpportunityClaimKind, RebalanceOpportunityInput, RebalanceOpportunityLease,
    RebalanceOpportunityRecord, RebalanceOpportunityState, RouteFeeBudgetError, RouteFeePayerKind,
    RouteFeePolicy, SignedRouteSubmissionAdvance, SignedRouteSubmissionInput, TargetCapacityCurve,
    TargetCapacityObservation, TargetCapacityProjection, TargetCapacityReservationInput,
    WaveLimits,
};
use loyal_yield_orchestrator::{
    lookup_table_manifest_address_records_hash, lookup_table_rollout_lock_acquisition_count,
    AtomicVaultAllocationResult, DecisionAdvance, LookupTableAllocationKind, LookupTableFamilyKind,
    LookupTableFamilyRecord, LookupTableFamilyState, LookupTableFamilyUpsert, LookupTableLifecycle,
    LookupTableManifestAddressRecord, LookupTableManifestSubject, LookupTableMembershipAddress,
    LookupTableOperationEnqueue, LookupTableOperationKind, LookupTableOperationLease,
    LookupTableOperationRecord, LookupTableProvisionerBroadcastPermitResult,
    LookupTableProvisioningPlanPolicy, LookupTableProvisioningRequestUpsert,
    LookupTableUsageLeaseBundle, LookupTableUsageLeaseKind, NeonSqlClient, NeonSqlConfig,
    PackedShardPolicy, ReconciledReservePosition, ReconciledVaultState, ReusableLookupTableInsert,
    SameMintRebalanceInput, SharedMarketCatalogUpsert, SignedLookupTableTransaction, VaultId,
    ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::signature::{Keypair, Signer};
use sqlx::{postgres::PgConnectOptions, Row};

const TEN_SECONDS_MILLIS: u128 = 10_000;
const VERIFIED_MIGRATIONS: [(i64, &str, &str); 5] = [
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
}

struct Cli {
    implementation: bool,
    json_output: bool,
    database_url: Option<String>,
    isolated_database: bool,
    collect_repository_evidence: bool,
    repository_root: Option<PathBuf>,
    runtime_evidence_json: Option<PathBuf>,
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
struct RuntimeAltEvidence {
    typed_provisioner_dry_run_plans: u64,
    reusable_v2_plans: u64,
    legacy_or_exact_route_alt_plans: u64,
    ready_jobs_seeded: u64,
    ready_jobs_claimed: u64,
    waiting_alt_jobs: u64,
    waiting_alt_decisions: u64,
    ready_claim_baseline_p95_micros: u64,
    ready_claim_cold_p95_micros: u64,
    durable_coverage_wakeup_rows: u64,
    affected_jobs_promoted: u64,
    unaffected_jobs_promoted: u64,
    additional_fleet_cycle_required: bool,
    normal_readiness_global_rollout_lock_acquisitions: u64,
    independent_physical_alt_lanes_progressed: u64,
    same_table_predecessor_violations: u64,
    stale_fence_commits: u64,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeExecutionEvidence {
    duplicate_active_vault_movements: u64,
    nonoverlapping_concurrent_leases: u64,
    overlapping_lane_limit_violations: u64,
    physical_writable_key_congestion_visible: bool,
    expired_lease_reclaimed_with_higher_fence: bool,
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
        evidence,
    }
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

fn collect_digest_files(path: &Path, files: &mut Vec<PathBuf>) -> Result<(), Box<dyn Error>> {
    if path.is_file() {
        files.push(path.to_path_buf());
        return Ok(());
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

fn runtime_source_digest(repository_root: &Path) -> Result<String, Box<dyn Error>> {
    let inputs = [
        "Cargo.toml",
        "Cargo.lock",
        "Dockerfile.light-workers",
        "render.yaml",
        "crates/loyal-yield-orchestrator/Cargo.toml",
        "crates/loyal-yield-orchestrator/src",
        "crates/loyal-yield-orchestrator/migrations",
        "crates/loyal-yield-router/Cargo.toml",
        "crates/loyal-yield-router/src",
    ];
    let mut files = Vec::new();
    for input in inputs {
        collect_digest_files(&repository_root.join(input), &mut files)?;
    }
    files.sort();
    files.dedup();
    let mut digest = Sha256::new();
    for file in files {
        let relative = file.strip_prefix(repository_root)?;
        let relative = relative.to_string_lossy();
        let bytes = fs::read(&file)?;
        digest.update((relative.len() as u64).to_le_bytes());
        digest.update(relative.as_bytes());
        digest.update((bytes.len() as u64).to_le_bytes());
        digest.update(bytes);
    }
    let bytes = digest.finalize();
    Ok(bytes.iter().map(|byte| format!("{byte:02x}")).collect())
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

fn production_environment(render_yaml: &str) -> Option<&str> {
    let project_start = render_yaml.find("  - name: loyal-yield-light-workers")?;
    let project = &render_yaml[project_start..];
    let production_start = project.find("      - name: production")?;
    let production = &project[production_start..];
    let staging_start = production.find("      - name: staging");
    Some(staging_start.map_or(production, |end| &production[..end]))
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
        (!value.is_empty()).then_some(value.trim_matches(['\'', '"']))
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
    let runtime_source_digest_sha256 = runtime_source_digest(repository_root)?;
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
                .join("crates/loyal-yield-orchestrator/migrations")
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

    let workflow = fs::read_to_string(repository_root.join(".github/workflows/worker-images.yml"))?;
    wiring_subchecks.push(subcheck(
        "worker_image_workflow_uses_immutable_commit_tags",
        workflow.contains("dockerfile: Dockerfile.light-workers")
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
            == MaterialFrontierDisposition::FullSweepReserveTopologyChanged;
    wiring_subchecks.push(subcheck(
        "functional_material_market_frontier_skips_harmless_churn_and_wakes_on_material_change",
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
                    ("--concurrency", "8"),
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
                    ("--concurrency", "16"),
                    ("--batch-size", "32"),
                    ("--poll-interval-milliseconds", "250"),
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
    let passed = evidence.discovery.fleet_size > 0
        && evidence.discovery.eligible_current_vaults > 0
        && evidence.discovery.fleet_size >= evidence.discovery.eligible_current_vaults
        && evidence.discovery.accounted_vaults == evidence.discovery.eligible_current_vaults
        && outcome_count == evidence.discovery.eligible_current_vaults
        && active_exclusion_count == active_outcome_count
        && evidence.discovery.optimizer_epoch_id > 0
        && evidence.discovery.one_immutable_epoch
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
        && baseline > 0
        && cold_effect_ppm < 50_000
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
        && evidence.alt.stale_fence_commits == 0;
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
        && execution.atomic_immutable_spend_reservation;
    subcheck(
        "bound_controlled_rpc_evidence_meets_replay_signer_and_reconciliation_gates",
        passed,
        serde_json::to_value(execution).unwrap_or_else(|_| json!({})),
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
            && exact_role_set
            && exact_stuck_stage_set,
        json!({
            "evidence": &evidence.wiring,
            "productionLightWorkerImageReference": production_light_worker_image_reference,
            "probedImageIsProductionCandidate": probed_image_is_production_candidate,
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
            already_committed_inflow_usd_micros: 0,
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
    let benchmark = run_deterministic_benchmark(10_000, 0x4c4f_5941_4c)
        .map_err(|error| format!("{error:?}"))?;
    let benchmark_millis = started.elapsed().as_millis();

    let market_observed_at = DateTime::<Utc>::from_timestamp(1_752_000_000, 0)
        .ok_or_else(|| "deterministic market timestamp is invalid".to_owned())?;
    let market_epoch = ImmutableMarketEpoch {
        optimizer_epoch_id: 77,
        fingerprint: "unchanged-market-snapshot".to_owned(),
        captured_at: market_observed_at + chrono::Duration::seconds(10),
        expires_at: market_observed_at + chrono::Duration::minutes(5),
        oldest_market_observed_at: Some(market_observed_at),
        newest_market_observed_at: Some(market_observed_at),
        minimum_market_slot: Some(42),
        maximum_market_slot: Some(42),
        reserves: vec![MarketEpochReserve {
            reserve: "reserve".to_owned(),
            market: Some("market".to_owned()),
            liquidity_mint: "USDC".to_owned(),
            mint_decimals: 6,
            market_price_usd_micros: 1_000_000,
            observed_at: market_observed_at,
            slot: 42,
            supply_apy_bps: 500,
            total_supply_usd_micros: 1_000_000_000_000,
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
            .reconcile_vault(
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
        sqlx::query("DELETE FROM loyal_yield.signed_route_submissions WHERE cluster LIKE $1")
            .bind(&cluster_pattern)
            .execute(self.client.pool())
            .await?;
        sqlx::query("DELETE FROM loyal_yield.lookup_table_usage_leases WHERE cluster LIKE $1")
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

    Ok(AltDatabaseRuntimeMeasurements {
        typed_provisioner_dry_run_plans,
        reusable_v2_plans,
        legacy_or_exact_route_alt_plans,
        normal_readiness_global_rollout_lock_acquisitions,
        independent_physical_alt_lanes_progressed,
        same_table_predecessor_violations,
        stale_fence_commits,
        stale_fence_rejections,
        alt_authority_payer_identity_consistent: policy_identity_matches,
        policy_pubkey,
    })
}

async fn run_database_checks(
    fixture: &DatabaseFixture,
) -> Result<DatabaseEvidence, Box<dyn Error>> {
    let database_deadlocks_before: i64 = sqlx::query_scalar(
        "SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()",
    )
    .fetch_one(fixture.client.pool())
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
        .fleet_orchestration_status(&empty_status_cluster)
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
    fixture
        .seed_claim_latency_cluster(
            &baseline_latency_cluster,
            baseline_latency_epoch,
            4_096,
            0,
            10_000,
        )
        .await?;
    fixture
        .seed_claim_latency_cluster(&cold_latency_cluster, cold_latency_epoch, 4_096, 10_000, 0)
        .await?;
    claim_latency_batch_micros(
        &fixture.latency_client,
        &baseline_latency_cluster,
        "latency-baseline-warmup",
        8,
    )
    .await?;
    claim_latency_batch_micros(
        &fixture.latency_client,
        &cold_latency_cluster,
        "latency-cold-warmup",
        8,
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
    const WARMUP_CLAIMS_PER_SERIES: i64 = 8;
    let timed_ready_rows_claimed = TIMED_CLAIM_SERIES * TIMED_CLAIM_ROUNDS * CLAIM_BATCH_SIZE;
    // Each ready -> leased update leaves one dead entry in the runnable B-tree
    // until vacuum. The exact no-vacuum traversal is triangular: every later
    // batch crosses its own series' warmup and prior claimed entries before its
    // live batch. Allow 20% for MVCC/page visibility overhead. A regression
    // that scans the 10,000 waiting_alt rows in every cold round adds 630,000
    // reads and remains well beyond this derived ceiling.
    let expected_runnable_self_churn_reads = TIMED_CLAIM_SERIES
        * (TIMED_CLAIM_ROUNDS * WARMUP_CLAIMS_PER_SERIES
            + CLAIM_BATCH_SIZE * TIMED_CLAIM_ROUNDS * (TIMED_CLAIM_ROUNDS + 1) / 2);
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
    const READY_JOBS_SEEDED: usize = 4_096;
    const TIMED_READY_JOBS_CLAIMED: usize = 8 + (63 * 64);
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
        .await?;
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
    let retry_generation_input = rediscovery_input_for_opportunity(&retry_generation_seed_record);
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
                reason: Some("synthetic terminal pre-decision no-effect".to_owned()),
                route_fingerprint: None,
                requirements_fingerprint: None,
                execution_plan: None,
                provisioning_request_id: None,
            },
        )
        .await?;
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
               opportunity_state, updated_at
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

    let stale_handoff_cluster = fixture.cluster("stale_handoff");
    let stale_handoff_epoch = fixture.seed_epoch(&stale_handoff_cluster).await?;
    fixture
        .seed_opportunity(
            &stale_handoff_cluster,
            stale_handoff_epoch,
            "stale-handoff",
            "ready",
            925,
        )
        .await?;
    let stale_handoff_lease = claim_one(
        &fixture.client,
        &stale_handoff_cluster,
        "stale-handoff-worker",
        RebalanceOpportunityClaimKind::Execute,
    )
    .await?;
    let stale_handoff_conflicts = vec![
        format!("fleet-shared-write-lane:{}:stale-handoff", fixture.prefix),
        format!(
            "vault-write:{}:{}",
            fixture.prefix,
            stale_handoff_lease.opportunity.vault_id.as_i64()
        ),
    ];
    fixture
        .client
        .acquire_route_account_conflict_leases(
            &stale_handoff_lease,
            &stale_handoff_conflicts,
            Utc::now() + chrono::Duration::minutes(4),
        )
        .await?;
    sqlx::query(
        "UPDATE loyal_yield.rebalance_opportunities SET expires_at = clock_timestamp() - interval '1 millisecond' WHERE id = $1",
    )
    .bind(stale_handoff_lease.opportunity.id)
    .execute(fixture.client.pool())
    .await?;
    let stale_handoff_result = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            same_mint_input_for_lease(&stale_handoff_lease)?,
            &stale_handoff_lease,
            target_capacity_input_for_lease(fixture, &stale_handoff_lease).await?,
            signed_input_for_lease(
                fixture,
                &stale_handoff_lease,
                stale_handoff_conflicts,
                "stale-handoff",
            )
            .await?,
        )
        .await;
    let stale_handoff_error = stale_handoff_result.as_ref().err().map(ToString::to_string);
    let stale_handoff_decisions: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.rebalance_decisions WHERE vault_id = $1",
    )
    .bind(stale_handoff_lease.opportunity.vault_id.as_i64())
    .fetch_one(fixture.client.pool())
    .await?;
    let stale_handoff_submissions: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE opportunity_id = $1",
    )
    .bind(stale_handoff_lease.opportunity.id)
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
        .await?;
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
    let same_mint_input = same_mint_input_for_lease(&signed_route_lease)?;
    let (prepared, persisted) = fixture
        .client
        .prepare_same_mint_rebalance_with_signed_submission(
            same_mint_input,
            &signed_route_lease,
            target_capacity_input_for_lease(fixture, &signed_route_lease).await?,
            signed_input,
        )
        .await?;
    let decision_id = prepared
        .decision_id
        .ok_or("atomic signed fixture did not return a decision")?;
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
    let reconciliation_conflicts = semantic_conflicts
        .iter()
        .filter(|key| !key.starts_with("fleet-shared-write-lane:"))
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
                reconciliation_conflicts
                    .first()
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
    fixture
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
        .fleet_orchestration_status(&physical_status_cluster)
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
        && target_capacity_concurrency_passed
        && pre_send_terminal_failure_released_capacity
        && reconciled_capacity_retention_passed
        && preexisting_newer_telemetry_releases_on_reconcile
        && database_deadlocks == 0;

    Ok(DatabaseEvidence {
        migration_subchecks: vec![
            subcheck(
                "isolated_database_migrated_through_27",
                true,
                json!({
                    "databaseNameGuard": "fleet_verify",
                    "migration23": "value_priority_rebalance_queue",
                    "migration24": "fleet_route_confirmer",
                    "migration25": "fee_only_route_payer_shards",
                    "migration26": "target_capacity_reservations",
                    "migration27": "rebalance_opportunity_attempt_generations",
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
                        "targetCapacityConcurrentAdmissionBounded": target_capacity_concurrency_passed,
                        "preSendTargetCapacityReleased": pre_send_terminal_failure_released_capacity,
                        "reconciledCapacityStrictTelemetryFence": reconciled_capacity_retention_passed,
                        "preexistingNewerTelemetryRelease": preexisting_newer_telemetry_releases_on_reconcile,
                        "databaseDeadlocks": database_deadlocks,
                    },
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
        execution_subchecks: vec![
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
                "expired_opportunity_cannot_cross_atomic_signed_decision_handoff",
                stale_handoff_result.is_err()
                    && stale_handoff_decisions == 0
                    && stale_handoff_submissions == 0,
                json!({
                    "handoffRejected": stale_handoff_result.is_err(),
                    "error": stale_handoff_error,
                    "decisionRows": stale_handoff_decisions,
                    "signedSubmissionRows": stale_handoff_submissions,
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
                "ambiguous_effect_recovery_retains_vault_and_releases_shared_lane",
                ambiguous.state.as_str() == "effect_ambiguous"
                    && ambiguous_conflict_keys == reconciliation_conflicts
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
        ],
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
            .join("crates/loyal-yield-orchestrator/migrations")
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
            "migration_sql_23_through_27_reexecutes_in_rolled_back_transaction",
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
    ) = local
        .map(|evidence| {
            (
                evidence.repository_subchecks,
                evidence.wiring_subchecks,
                Some(evidence.repository_root),
                evidence.head_commit,
                Some(evidence.runtime_source_digest_sha256),
                evidence.production_light_worker_image_reference,
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
    }
    let execution_failure = first_failed(&execution_subchecks);
    if let Some(runtime) = runtime {
        wiring_subchecks.push(runtime_wiring_subcheck(
            runtime,
            production_light_worker_image_reference.as_deref(),
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
                } else {
                    if runtime_was_run {
                        "deterministic plus source-bound current-fleet evidence"
                    } else {
                        "deterministic in-memory evidence only"
                    }
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

fn parse_cli() -> Result<Option<Cli>, Box<dyn Error>> {
    let mut implementation = false;
    let mut json_output = false;
    let mut database_url = None;
    let mut isolated_database = false;
    let mut collect_repository_evidence = false;
    let mut repository_root = None;
    let mut runtime_evidence_json = None;
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--implementation" => implementation = true,
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
            "--database-url" => {
                database_url = Some(
                    args.next()
                        .ok_or("--database-url requires a PostgreSQL URL")?,
                );
            }
            "--help" | "-h" => {
                println!(
                    "fleet-orchestration-verifier --implementation [--json] [--collect-repository-evidence [--repository-root PATH]] [--runtime-evidence-json PATH] [--isolated-database [--database-url URL|FLEET_VERIFY_DATABASE_URL]]"
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
        json_output,
        database_url,
        isolated_database,
        collect_repository_evidence,
        repository_root,
        runtime_evidence_json,
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
    if !cli.implementation {
        return Err("--implementation is required".into());
    }
    if cli.database_url.is_some() != cli.isolated_database {
        return Err(
            "--isolated-database requires --database-url or FLEET_VERIFY_DATABASE_URL; isolated database checks are never implicit"
                .into(),
        );
    }
    if cli.repository_root.is_some() && !cli.collect_repository_evidence {
        return Err("--repository-root requires --collect-repository-evidence".into());
    }
    if cli.runtime_evidence_json.is_some() && !cli.collect_repository_evidence {
        return Err(
            "--runtime-evidence-json requires --collect-repository-evidence for HEAD and source binding"
                .into(),
        );
    }

    let collected_repository_root = cli
        .collect_repository_evidence
        .then(|| repository_root(cli.repository_root.as_deref()))
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

    let checks =
        implementation_checks(database_evidence, local_evidence, runtime_evidence.as_ref())?;
    let implementation_verdict = aggregate_verdicts(checks.iter().map(|check| check.verdict));
    // This command collects local/read-only implementation evidence only.
    // Registry/image existence and live Render state require an explicit
    // deployment evidence run; real fund-movement SLOs require a production
    // performance run. Absence of either is never promoted to PASS.
    let deployment_verdict = Verdict::NotRun;
    let production_performance_verdict = Verdict::NotRun;
    let end_state_verdict = aggregate_verdicts([
        implementation_verdict,
        deployment_verdict,
        production_performance_verdict,
    ]);
    let first_blocking_check = checks
        .iter()
        .find(|check| !matches!(check.verdict, Verdict::Pass))
        .map(|check| {
            json!({
                "id": check.id,
                "name": check.name,
                "verdict": check.verdict,
                "invariant": check.first_failing_invariant,
            })
        });
    let output = json!({
        "status": end_state_verdict,
        "requestedScope": "IMPLEMENTATION",
        "requestedScopeStatus": implementation_verdict,
        "implementation": implementation_verdict,
        "deployment": deployment_verdict,
        "productionPerformance": production_performance_verdict,
        "endState": end_state_verdict,
        "scopeVerdicts": {
            "IMPLEMENTATION": implementation_verdict,
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

    Ok(if implementation_verdict == Verdict::Pass {
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
