use std::{
    collections::BTreeMap,
    env,
    error::Error,
    fs,
    path::{Path, PathBuf},
    process::{Command, Output},
    time::Instant,
};

use chrono::{DateTime, Utc};
use loyal_yield_orchestrator::fleet_orchestration::{
    collect_deterministic_runtime_replay, functional_stuck_stage_fixture,
    local_hardware_description, run_deterministic_benchmark, ControlledRuntimeEvidence,
    FleetWorkerRole, RuntimeAltEvidence, RuntimeDatabaseExecutionEvidence,
    RuntimeDiscoveryEvidence, RuntimeEvidenceFoundationV1, RuntimeEvidenceV1,
    RuntimeExecutionEvidence, RuntimePlannerEpochProof, RuntimeSourceBinding,
    RuntimeTransactionProbeEvidence, RuntimeWiringEvidence,
};
use serde::Deserialize;
use serde_json::Value;
use sha2::{Digest, Sha256};

const PLANNING_ROUNDS: usize = 7;
const REPLAY_VAULT_COUNT: usize = 10_000;
const REPLAY_SEED: u64 = 0x4c4f_5941_4c;

#[derive(Debug)]
struct Options {
    repository_root: PathBuf,
    image: String,
    container_engine: ContainerEngine,
    output: Option<PathBuf>,
    foundation: bool,
    replay_only: bool,
    isolated_database_url_env: String,
}

#[derive(Debug)]
struct EvidenceBinaries {
    planner: PathBuf,
    verifier: Option<PathBuf>,
    transaction_probe: Option<PathBuf>,
}

#[derive(Debug, Clone, Copy)]
enum ContainerEngine {
    Docker,
    Podman,
}

impl ContainerEngine {
    fn program(self) -> &'static str {
        match self {
            Self::Docker => "docker",
            Self::Podman => "podman",
        }
    }
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct PlannerOutput {
    status: String,
    mode: String,
    mutating: bool,
    planning_scope: String,
    child_processes_spawned: u64,
    epoch_fingerprint: String,
    epoch_expires_at: DateTime<Utc>,
    market_epoch_optimizer_id: i64,
    observed_opportunity_epoch_ids: Vec<i64>,
    selected_opportunity_epoch_ids: Vec<i64>,
    observation_and_planning_micros: u64,
    fleet_completeness: PlannerFleetCompleteness,
    top_value_cohort: Vec<PlannerTopValueItem>,
}

#[derive(Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
struct PlannerFleetCompleteness {
    eligible_managed_vaults: i64,
    active_opportunity_vaults_excluded_by_state: BTreeMap<String, i64>,
    vault_outcomes_by_reason: BTreeMap<String, i64>,
    fleet_vaults_accounted: i64,
    complete_vault_accounting: bool,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct PlannerTopValueItem {
    priority: i64,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct DatabaseMeasurementEnvelope {
    schema_version: u32,
    event: String,
    alt: RuntimeAltEvidence,
    execution: RuntimeDatabaseExecutionEvidence,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct TransactionProbeEnvelope {
    schema_version: u32,
    event: String,
    external_network_accessed: bool,
    production_transaction_sent: bool,
    execution: RuntimeTransactionProbeEvidence,
}

fn default_repository_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../..")
        .to_path_buf()
}

fn parse_options() -> Result<Options, Box<dyn Error>> {
    let mut repository_root = default_repository_root();
    let mut image = None;
    let mut output = None;
    let mut foundation = false;
    let mut replay_only = false;
    let mut isolated_database_url_env = "FLEET_VERIFY_DATABASE_URL".to_owned();
    let mut container_engine = ContainerEngine::Docker;
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--repository-root" => {
                repository_root =
                    PathBuf::from(args.next().ok_or("--repository-root requires a value")?);
            }
            "--image" => image = Some(args.next().ok_or("--image requires a value")?),
            "--container-engine" => {
                container_engine = match args
                    .next()
                    .ok_or("--container-engine requires docker or podman")?
                    .as_str()
                {
                    "docker" => ContainerEngine::Docker,
                    "podman" => ContainerEngine::Podman,
                    _ => return Err("--container-engine must be docker or podman".into()),
                };
            }
            "--output" => {
                output = Some(PathBuf::from(
                    args.next().ok_or("--output requires a value")?,
                ));
            }
            "--foundation" => foundation = true,
            "--replay-only" => replay_only = true,
            "--isolated-database-url-env" => {
                isolated_database_url_env = args
                    .next()
                    .ok_or("--isolated-database-url-env requires a variable name")?;
            }
            "--help" | "-h" => {
                println!(
                    "fleet-orchestration-runtime-evidence --image <LOCAL_IMAGE> \
                     [--container-engine docker|podman] [--repository-root <PATH>] \
                     [--isolated-database-url-env <NAME>] [--output <PATH>] [--foundation]\n\
                     fleet-orchestration-runtime-evidence --replay-only [--output <PATH>]"
                );
                std::process::exit(0);
            }
            other => return Err(format!("unknown argument: {other}").into()),
        }
    }
    if foundation && replay_only {
        return Err("--foundation and --replay-only are mutually exclusive".into());
    }
    let image = if replay_only {
        image.unwrap_or_default()
    } else {
        image.ok_or("--image is required")?
    };
    if !replay_only
        && (image.trim().is_empty()
            || image.starts_with('-')
            || image.chars().any(char::is_whitespace))
    {
        return Err("--image must be one non-option local image reference".into());
    }
    if isolated_database_url_env.is_empty()
        || !isolated_database_url_env
            .bytes()
            .all(|byte| byte.is_ascii_uppercase() || byte.is_ascii_digit() || byte == b'_')
    {
        return Err("--isolated-database-url-env must be an uppercase environment name".into());
    }
    let repository_root = fs::canonicalize(repository_root)?;
    if let Some(output_path) = output.as_deref() {
        let parent = output_path
            .parent()
            .filter(|parent| !parent.as_os_str().is_empty())
            .unwrap_or_else(|| Path::new("."));
        let canonical_parent = fs::canonicalize(parent)?;
        let file_name = output_path.file_name().ok_or("--output must name a file")?;
        if canonical_parent
            .join(file_name)
            .starts_with(&repository_root)
        {
            return Err("--output must be outside the source-bound repository".into());
        }
    }
    Ok(Options {
        repository_root,
        image,
        container_engine,
        output,
        foundation,
        replay_only,
        isolated_database_url_env,
    })
}

fn sha256_hex(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn command_failure(context: &str, output: &Output) -> String {
    format!(
        "{context} failed: exit_code={:?}, stderr_sha256={}, stderr_bytes={}",
        output.status.code(),
        sha256_hex(&output.stderr),
        output.stderr.len(),
    )
}

fn locate_built_binary(repository_root: &Path, name: &str) -> Result<PathBuf, Box<dyn Error>> {
    let current_parent = env::current_exe()?
        .parent()
        .ok_or("collector executable has no parent directory")?
        .to_path_buf();
    let mut candidates = vec![
        current_parent.join(name),
        repository_root.join("target/debug").join(name),
    ];
    if let Some(target_dir) = env::var_os("CARGO_TARGET_DIR") {
        let target_dir = PathBuf::from(target_dir);
        let target_dir = if target_dir.is_absolute() {
            target_dir
        } else {
            repository_root.join(target_dir)
        };
        candidates.insert(1, target_dir.join("debug").join(name));
    }
    candidates
        .into_iter()
        .find(|path| path.is_file())
        .ok_or_else(|| format!("cargo did not produce required evidence binary {name}").into())
}

fn build_evidence_binaries(
    repository_root: &Path,
    foundation: bool,
) -> Result<EvidenceBinaries, Box<dyn Error>> {
    let mut args = vec![
        "build",
        "--quiet",
        "--locked",
        "-p",
        "loyal-yield-orchestrator",
        "--bin",
        "fleet-opportunity-planner",
    ];
    if !foundation {
        args.extend([
            "--bin",
            "fleet-orchestration-verifier",
            "--bin",
            "same-mint-reserve-swap",
        ]);
    }
    let output = Command::new("cargo")
        .args(args)
        .current_dir(repository_root)
        .output()?;
    if !output.status.success() {
        return Err(command_failure("evidence binary build", &output).into());
    }
    Ok(EvidenceBinaries {
        planner: locate_built_binary(repository_root, "fleet-opportunity-planner")?,
        verifier: (!foundation)
            .then(|| locate_built_binary(repository_root, "fleet-orchestration-verifier"))
            .transpose()?,
        transaction_probe: (!foundation)
            .then(|| locate_built_binary(repository_root, "same-mint-reserve-swap"))
            .transpose()?,
    })
}

fn percentile_95(values: &mut [u64]) -> Option<u64> {
    if values.is_empty() {
        return None;
    }
    values.sort_unstable();
    let index = (values.len() * 95).div_ceil(100).saturating_sub(1);
    values.get(index).copied()
}

fn active_outcomes_match(
    active_by_state: &BTreeMap<String, i64>,
    outcomes: &BTreeMap<String, i64>,
) -> bool {
    let expected = active_by_state
        .iter()
        .map(|(state, count)| {
            let outcome = if state == "active_decision" {
                state.clone()
            } else {
                format!("active_queue_{state}")
            };
            (outcome, *count)
        })
        .collect::<BTreeMap<_, _>>();
    let actual = outcomes
        .iter()
        .filter(|(outcome, _)| {
            outcome.as_str() == "active_decision" || outcome.starts_with("active_queue_")
        })
        .map(|(outcome, count)| (outcome.clone(), *count))
        .collect::<BTreeMap<_, _>>();
    expected == actual
}

fn epoch_ids_match_market(ids: &[i64], market_epoch_optimizer_id: i64) -> bool {
    ids.len() <= 1
        && ids
            .iter()
            .all(|epoch_id| *epoch_id == market_epoch_optimizer_id)
}

fn planner_epoch_proof(run: &PlannerOutput) -> RuntimePlannerEpochProof {
    RuntimePlannerEpochProof {
        market_epoch_optimizer_id: run.market_epoch_optimizer_id,
        observed_opportunity_epoch_ids: run.observed_opportunity_epoch_ids.clone(),
        selected_opportunity_epoch_ids: run.selected_opportunity_epoch_ids.clone(),
    }
}

fn planner_epoch_proof_passes(proof: &RuntimePlannerEpochProof) -> bool {
    proof.market_epoch_optimizer_id > 0
        && epoch_ids_match_market(
            &proof.observed_opportunity_epoch_ids,
            proof.market_epoch_optimizer_id,
        )
        && epoch_ids_match_market(
            &proof.selected_opportunity_epoch_ids,
            proof.market_epoch_optimizer_id,
        )
}

fn positive_epoch_id(fingerprint: &str) -> i64 {
    let mut folded = 0xcbf29ce484222325u64;
    for byte in fingerprint.bytes() {
        folded ^= u64::from(byte);
        folded = folded.wrapping_mul(0x100000001b3);
    }
    i64::try_from(folded & i64::MAX as u64)
        .unwrap_or(i64::MAX)
        .max(1)
}

fn run_read_only_planner(
    repository_root: &Path,
    planner_binary: &Path,
) -> Result<PlannerOutput, Box<dyn Error>> {
    let output = Command::new(planner_binary)
        .args(["--once", "--dry-run", "--json"])
        .current_dir(repository_root)
        .output()?;
    if !output.status.success() {
        return Err(command_failure("read-only production planner", &output).into());
    }
    let parsed: PlannerOutput = serde_json::from_slice(&output.stdout).map_err(|error| {
        format!(
            "read-only planner emitted invalid JSON: {error}; stdout_sha256={}; stdout_bytes={}",
            sha256_hex(&output.stdout),
            output.stdout.len(),
        )
    })?;
    let epoch_proof = planner_epoch_proof(&parsed);
    let observed_opportunity_vaults = parsed
        .fleet_completeness
        .vault_outcomes_by_reason
        .get("opportunity_observed")
        .copied()
        .unwrap_or_default();
    if parsed.status != "planned"
        || parsed.mode != "live_read_only"
        || parsed.mutating
        || parsed.planning_scope != "full_fleet"
        || parsed.epoch_fingerprint.is_empty()
        || parsed.epoch_expires_at <= Utc::now()
        || !planner_epoch_proof_passes(&epoch_proof)
        || (observed_opportunity_vaults > 0
            && epoch_proof.observed_opportunity_epoch_ids.is_empty())
        || !parsed.fleet_completeness.complete_vault_accounting
        || !active_outcomes_match(
            &parsed
                .fleet_completeness
                .active_opportunity_vaults_excluded_by_state,
            &parsed.fleet_completeness.vault_outcomes_by_reason,
        )
    {
        return Err("read-only production planner did not satisfy its evidence contract".into());
    }
    Ok(parsed)
}

fn i64_count(value: i64, name: &str) -> Result<u64, Box<dyn Error>> {
    u64::try_from(value).map_err(|_| format!("planner reported a negative {name}").into())
}

fn checked_count_map(
    values: &BTreeMap<String, i64>,
    name: &str,
) -> Result<BTreeMap<String, u64>, Box<dyn Error>> {
    values
        .iter()
        .map(|(key, value)| Ok((key.clone(), i64_count(*value, name)?)))
        .collect()
}

fn collect_discovery(
    repository_root: &Path,
    planner_binary: &Path,
) -> Result<RuntimeDiscoveryEvidence, Box<dyn Error>> {
    let mut runs = Vec::with_capacity(PLANNING_ROUNDS);
    for _ in 0..PLANNING_ROUNDS {
        runs.push(run_read_only_planner(repository_root, planner_binary)?);
    }
    let final_run = runs.last().ok_or("planner produced no samples")?;
    let planning_sample_epoch_proofs = runs.iter().map(planner_epoch_proof).collect::<Vec<_>>();
    let one_immutable_epoch = planning_sample_epoch_proofs
        .iter()
        .all(planner_epoch_proof_passes);

    let mut planning_micros = runs
        .iter()
        .map(|run| run.observation_and_planning_micros)
        .collect::<Vec<_>>();
    let planning_p95_micros =
        percentile_95(&mut planning_micros).ok_or("planner produced no timing samples")?;
    let economically_ordered = runs.iter().all(|run| {
        run.top_value_cohort
            .windows(2)
            .all(|pair| pair[0].priority >= pair[1].priority)
    });
    // Strict descending economic order is stronger than the requested
    // non-conflicting check: no later item has a greater priority regardless
    // of whether its writable set overlaps.
    let top_cohort_has_no_nonconflicting_priority_inversion = economically_ordered;

    let replay_started = Instant::now();
    let replay = run_deterministic_benchmark(REPLAY_VAULT_COUNT, REPLAY_SEED)
        .map_err(|error| format!("production planner replay failed: {error:?}"))?;
    let replay_milliseconds =
        u64::try_from(replay_started.elapsed().as_millis()).unwrap_or(u64::MAX);
    let replay_ordered = replay
        .wave
        .selected
        .windows(2)
        .all(|pair| pair[0].economics.total_priority >= pair[1].economics.total_priority);
    if replay.input_count != REPLAY_VAULT_COUNT || !replay_ordered {
        return Err("production planner replay violated count or economic order".into());
    }

    // Queue state may advance while the read-only samples run. Report the
    // final complete partition (closest to capturedAt) while using every run
    // for the latency distribution and immutable-epoch check.
    let completeness = &final_run.fleet_completeness;
    let eligible_current_vaults = i64_count(
        completeness.eligible_managed_vaults,
        "eligible managed vault count",
    )?;
    let accounted_vaults = i64_count(
        completeness.fleet_vaults_accounted,
        "accounted managed vault count",
    )?;
    let vault_outcomes_by_reason = checked_count_map(
        &completeness.vault_outcomes_by_reason,
        "vault outcome count",
    )?;
    let outcome_total = vault_outcomes_by_reason
        .values()
        .try_fold(0u64, |total, count| total.checked_add(*count))
        .ok_or("vault outcome count overflow")?;
    if eligible_current_vaults != accounted_vaults || accounted_vaults != outcome_total {
        return Err("planner fleet accounting totals do not form an exact partition".into());
    }

    Ok(RuntimeDiscoveryEvidence {
        // The production planner's managed-and-policy-eligible scope is the
        // authoritative fleet denominator for this optimizer run.
        fleet_size: eligible_current_vaults,
        eligible_current_vaults,
        accounted_vaults,
        vault_outcomes_by_reason,
        active_exclusions_by_state: checked_count_map(
            &completeness.active_opportunity_vaults_excluded_by_state,
            "active exclusion count",
        )?,
        // Each independent read-only sample is internally bound to one
        // immutable epoch. Live samples may legitimately advance to a newer
        // epoch; the artifact records the final complete, non-expired one.
        optimizer_epoch_id: positive_epoch_id(&final_run.epoch_fingerprint),
        epoch_expires_at: final_run.epoch_expires_at,
        one_immutable_epoch,
        planning_sample_epoch_proofs,
        planning_sample_count: u64::try_from(runs.len()).unwrap_or(u64::MAX),
        planning_p95_milliseconds: planning_p95_micros.div_ceil(1_000),
        replay_vault_count: u64::try_from(replay.input_count).unwrap_or(u64::MAX),
        replay_milliseconds,
        economically_ordered,
        top_cohort_has_no_nonconflicting_priority_inversion,
        child_route_or_reconcile_processes_spawned: runs
            .iter()
            .map(|run| run.child_processes_spawned)
            .try_fold(0u64, u64::checked_add)
            .ok_or("planner child-process count overflow")?,
    })
}

fn find_named_subcheck<'a>(output: &'a Value, name: &str) -> Result<&'a Value, Box<dyn Error>> {
    let checks = output
        .get("checks")
        .and_then(Value::as_array)
        .ok_or("isolated verifier output has no checks array")?;
    let mut matches = checks
        .iter()
        .filter_map(|check| check.get("subchecks").and_then(Value::as_array))
        .flatten()
        .filter(|subcheck| subcheck.get("name").and_then(Value::as_str) == Some(name));
    let matched = matches
        .next()
        .ok_or_else(|| format!("isolated verifier did not emit required subcheck {name}"))?;
    if matches.next().is_some() {
        return Err(format!("isolated verifier emitted duplicate subcheck {name}").into());
    }
    if matched.get("verdict").and_then(Value::as_str) != Some("PASS") {
        return Err(format!("isolated verifier subcheck {name} did not pass").into());
    }
    matched
        .get("evidence")
        .ok_or_else(|| format!("isolated verifier subcheck {name} has no evidence").into())
}

fn collect_database_measurements(
    repository_root: &Path,
    verifier_binary: &Path,
    database_url_env: &str,
) -> Result<DatabaseMeasurementEnvelope, Box<dyn Error>> {
    let database_url = env::var(database_url_env)
        .map_err(|_| format!("{database_url_env} is required for isolated DB evidence"))?;
    if database_url.trim().is_empty() {
        return Err(format!("{database_url_env} must be nonempty").into());
    }
    let output = Command::new(verifier_binary)
        .args([
            "--implementation",
            "--json",
            "--collect-repository-evidence",
            "--repository-root",
        ])
        .arg(repository_root)
        .arg("--isolated-database")
        // The verifier consumes this environment variable when no URL is
        // supplied in argv. This keeps credentials out of process listings.
        .env("FLEET_VERIFY_DATABASE_URL", database_url)
        .current_dir(repository_root)
        .output()?;
    let verifier: Value = serde_json::from_slice(&output.stdout).map_err(|error| {
        format!(
            "isolated verifier emitted invalid JSON: {error}; exit_code={:?}; stdout_sha256={}; stdout_bytes={}; stderr_sha256={}; stderr_bytes={}",
            output.status.code(),
            sha256_hex(&output.stdout),
            output.stdout.len(),
            sha256_hex(&output.stderr),
            output.stderr.len(),
        )
    })?;
    let evidence = find_named_subcheck(&verifier, "runtime_alt_and_db_execution_measurements")?;
    let measurements: DatabaseMeasurementEnvelope = serde_json::from_value(evidence.clone())?;
    if measurements.schema_version != 1
        || measurements.event != "fleet_isolated_database_runtime_measurements"
    {
        return Err("isolated verifier measurement envelope has the wrong contract".into());
    }
    Ok(measurements)
}

fn collect_transaction_probe(
    repository_root: &Path,
    transaction_probe_binary: &Path,
) -> Result<RuntimeTransactionProbeEvidence, Box<dyn Error>> {
    let output = Command::new(transaction_probe_binary)
        .arg("--fleet-controlled-transaction-probe")
        .current_dir(repository_root)
        .output()?;
    if !output.status.success() {
        return Err(command_failure("controlled same-mint transaction probe", &output).into());
    }
    let probe: TransactionProbeEnvelope =
        serde_json::from_slice(&output.stdout).map_err(|error| {
            format!(
                "controlled transaction probe emitted invalid JSON: {error}; stdout_sha256={}; stdout_bytes={}",
                sha256_hex(&output.stdout),
                output.stdout.len(),
            )
        })?;
    if probe.schema_version != 1
        || probe.event != "fleet_transaction_runtime_probe"
        || probe.external_network_accessed
        || probe.production_transaction_sent
    {
        return Err("controlled transaction probe has the wrong contract".into());
    }
    Ok(probe.execution)
}

fn collect_controlled_evidence(
    repository_root: &Path,
    verifier_binary: &Path,
    transaction_probe_binary: &Path,
    database_url_env: &str,
) -> Result<ControlledRuntimeEvidence, Box<dyn Error>> {
    let database =
        collect_database_measurements(repository_root, verifier_binary, database_url_env)?;
    let transaction = collect_transaction_probe(repository_root, transaction_probe_binary)?;
    let mut replay = collect_deterministic_runtime_replay();
    replay.database_deadlocks = database.execution.database_deadlocks;
    Ok(ControlledRuntimeEvidence {
        alt: database.alt,
        execution: RuntimeExecutionEvidence::from_code_owned_probes(
            database.execution,
            transaction,
        ),
        replay,
    })
}

fn container_image_id(engine: ContainerEngine, image: &str) -> Result<String, Box<dyn Error>> {
    let output = Command::new(engine.program())
        .args(["image", "inspect", "--format", "{{.Id}}", image])
        .output()?;
    if !output.status.success() {
        return Err(command_failure("local container image inspection", &output).into());
    }
    let image_id = String::from_utf8(output.stdout)?;
    let image_id = image_id.trim();
    if image_id.is_empty() || image_id.lines().count() != 1 {
        return Err("container engine returned an invalid local image ID".into());
    }
    Ok(image_id.to_owned())
}

fn run_role_probe(
    engine: ContainerEngine,
    image: &str,
    role: FleetWorkerRole,
) -> Result<i32, Box<dyn Error>> {
    let entrypoint = format!("/usr/local/bin/{}", role.owning_binary());
    let mut command = Command::new(engine.program());
    command.args([
        "run",
        "--rm",
        "--network=none",
        "--read-only",
        "--cap-drop=ALL",
        "--security-opt=no-new-privileges",
        "--pull=never",
        "--entrypoint",
        &entrypoint,
        image,
    ]);
    command.args(role.local_probe_argv());
    let output = command.output()?;
    let exit_code = output.status.code().unwrap_or(-1);
    if !output.status.success() {
        return Ok(exit_code);
    }
    let probe: Value = serde_json::from_slice(&output.stdout).map_err(|error| {
        format!(
            "{} role probe emitted invalid JSON: {error}; stdout_sha256={}; stdout_bytes={}",
            role.as_str(),
            sha256_hex(&output.stdout),
            output.stdout.len(),
        )
    })?;
    let valid = probe.get("schemaVersion").and_then(Value::as_u64) == Some(1)
        && probe.get("event").and_then(Value::as_str) == Some("fleet_worker_role_probe")
        && probe.get("status").and_then(Value::as_str) == Some("pass")
        && probe.get("role").and_then(Value::as_str) == Some(role.as_str())
        && probe.get("owningBinary").and_then(Value::as_str) == Some(role.owning_binary())
        && probe.get("networkAccessed").and_then(Value::as_bool) == Some(false)
        && probe.get("secretsLoaded").and_then(Value::as_bool) == Some(false)
        && probe.get("databaseMutated").and_then(Value::as_bool) == Some(false)
        && probe.get("transactionSent").and_then(Value::as_bool) == Some(false);
    if !valid {
        return Err(format!("{} role probe violated its contract", role.as_str()).into());
    }
    Ok(exit_code)
}

fn collect_wiring(
    engine: ContainerEngine,
    image: &str,
) -> Result<RuntimeWiringEvidence, Box<dyn Error>> {
    let local_container_image_id = container_image_id(engine, image)?;
    let mut runnable_role_probe_exit_codes = BTreeMap::new();
    for role in FleetWorkerRole::ALL {
        runnable_role_probe_exit_codes.insert(
            role.as_str().to_owned(),
            run_role_probe(engine, image, role)?,
        );
    }
    let fixture = functional_stuck_stage_fixture();
    if !fixture.passed {
        return Err("functional stuck-stage fixture failed".into());
    }
    Ok(RuntimeWiringEvidence {
        probed_container_image_reference: image.to_owned(),
        local_container_image_id,
        runnable_role_probe_exit_codes,
        recovery_poll_interval_milliseconds: fixture.recovery_poll_interval_milliseconds,
        health_observation_interval_milliseconds: fixture.health_observation_interval_milliseconds,
        stuck_stage_detection_milliseconds: fixture.detection_milliseconds,
    })
}

fn write_artifact(path: Option<&Path>, bytes: &[u8]) -> Result<(), Box<dyn Error>> {
    match path {
        Some(path) => {
            let parent = path
                .parent()
                .filter(|parent| !parent.as_os_str().is_empty())
                .unwrap_or_else(|| Path::new("."));
            let file_name = path
                .file_name()
                .and_then(|name| name.to_str())
                .ok_or("--output must name a file")?;
            let temporary = parent.join(format!(".{file_name}.tmp-{}", std::process::id()));
            fs::write(&temporary, bytes)?;
            fs::rename(temporary, path)?;
        }
        None => println!("{}", String::from_utf8_lossy(bytes)),
    }
    Ok(())
}

fn main() -> Result<(), Box<dyn Error>> {
    let options = parse_options()?;
    if options.replay_only {
        let bytes = serde_json::to_vec_pretty(&collect_deterministic_runtime_replay())?;
        return write_artifact(options.output.as_deref(), &bytes);
    }
    let binaries = build_evidence_binaries(&options.repository_root, options.foundation)?;
    let before = RuntimeSourceBinding::capture(&options.repository_root)?;
    let wiring = collect_wiring(options.container_engine, &options.image)?;
    let controlled = if options.foundation {
        None
    } else {
        Some(collect_controlled_evidence(
            &options.repository_root,
            binaries
                .verifier
                .as_deref()
                .ok_or("complete collection requires the verifier binary")?,
            binaries
                .transaction_probe
                .as_deref()
                .ok_or("complete collection requires the transaction probe binary")?,
            &options.isolated_database_url_env,
        )?)
    };
    // Capture the live, expiring epoch last so capturedAt remains inside the
    // same market validity window even when the isolated DB fixture is slow.
    let discovery = collect_discovery(&options.repository_root, &binaries.planner)?;
    let after = RuntimeSourceBinding::capture(&options.repository_root)?;
    if before != after {
        return Err("runtime inputs or checkout HEAD changed during evidence collection".into());
    }
    let captured_at = Utc::now();
    let hardware = local_hardware_description();
    let bytes = match controlled {
        Some(controlled) => {
            serde_json::to_vec_pretty(&RuntimeEvidenceV1::from_collected_measurements(
                after,
                captured_at,
                hardware,
                discovery,
                controlled,
                wiring,
            )?)?
        }
        None => serde_json::to_vec_pretty(&RuntimeEvidenceFoundationV1::new(
            after,
            captured_at,
            hardware,
            discovery,
            wiring,
        ))?,
    };
    write_artifact(options.output.as_deref(), &bytes)
}
