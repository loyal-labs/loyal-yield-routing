#![recursion_limit = "512"]

use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    fs,
    path::{Path, PathBuf},
    process::{Command, ExitCode},
    str::FromStr,
};

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use chrono::{DateTime, Utc};
use loyal_actions::{KAMINO_MAIN_MARKET, KAMINO_MAIN_USDC_RESERVE, USDC_MINT};
use loyal_yield_orchestrator::{enabled_stable_mints_from_env, STANDARD_POLICY_AUTHORITY};
use reqwest::Client;
use serde::Serialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{
    address_lookup_table::{program as alt_program, state::AddressLookupTable},
    pubkey::Pubkey,
    signer::Signer,
};
use sqlx::{
    postgres::{PgConnectOptions, PgPoolOptions},
    PgPool, Row,
};

const SCHEMA_VERSION: u32 = 1;
const DEFAULT_CLUSTER: &str = "mainnet-beta";
const DEFAULT_RENDER_ENVIRONMENT_ID: &str = "evm-d8kgt4r7uimc73b1ul1g";
const RENDER_API_BASE_URL: &str = "https://api.render.com/v1";
const SERIAL_MONITOR_NAME: &str = "loyal-same-mint-yield-monitor";
const MATERIAL_PRINCIPAL_USD_MICROS: i64 = 1_000_000_000;
const MAX_MATERIAL_STAGE_AGE_SECONDS: i64 = 600;
const MAX_FULL_SWEEP_AGE_SECONDS: i64 = 120;
const SAME_MINT_ROUTE_MODE: &str = "same_mint_kamino";

const DURABLE_SERVICE_NAMES: [&str; 6] = [
    "loyal-fleet-opportunity-planner",
    "loyal-fleet-route-revalidator",
    "loyal-fleet-route-executor",
    "loyal-fleet-route-confirmer",
    "loyal-fleet-route-reconciler",
    "loyal-route-lookup-table-provisioner",
];

const REQUIRED_MIGRATIONS: [(i64, &str, &str); 6] = [
    (
        23,
        "value_priority_rebalance_queue",
        include_str!("../../migrations/0023_value_priority_rebalance_queue.sql"),
    ),
    (
        24,
        "fleet_route_confirmer",
        include_str!("../../migrations/0024_fleet_route_confirmer.sql"),
    ),
    (
        25,
        "fee_only_route_payer_shards",
        include_str!("../../migrations/0025_fee_only_route_payer_shards.sql"),
    ),
    (
        26,
        "target_capacity_reservations",
        include_str!("../../migrations/0026_target_capacity_reservations.sql"),
    ),
    (
        27,
        "rebalance_opportunity_attempt_generations",
        include_str!("../../migrations/0027_rebalance_opportunity_attempt_generations.sql"),
    ),
    (
        28,
        "reusable_alt_terminal_repair",
        include_str!("../../migrations/0028_reusable_alt_terminal_repair.sql"),
    ),
];

#[derive(Debug)]
struct Options {
    repository_root: PathBuf,
    cluster: String,
    render_environment_id: String,
    cutover_at: Option<DateTime<Utc>>,
    baseline: Option<PathBuf>,
    output: Option<PathBuf>,
    compact: bool,
}

#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
enum Verdict {
    Pass,
    Fail,
    NotRun,
}

#[derive(Debug, Clone)]
struct ExpectedService {
    name: String,
    image: String,
    command: String,
    pre_deploy_command: String,
    plan: String,
    env_keys: BTreeSet<String>,
}

#[derive(Debug, Clone)]
struct MovementRow {
    submission_id: i64,
    opportunity_id: i64,
    decision_id: i64,
    opportunity_decision_id: Option<i64>,
    vault_id: i64,
    decision_vault_id: i64,
    signature: String,
    submission_state: String,
    opportunity_state: String,
    decision_status: String,
    route_kind: String,
    opportunity_optimizer_epoch_id: i64,
    submission_optimizer_epoch_id: i64,
    opportunity_source_snapshot_id: Option<i64>,
    submission_source_snapshot_id: Option<i64>,
    source_reserve: Option<String>,
    target_reserve: String,
    liquidity_mint: String,
    amount_raw: i64,
    decision_source_snapshot_id: Option<i64>,
    decision_source_reserve: Option<String>,
    decision_signature: Option<String>,
    decision_confirmed_slot: Option<i64>,
    decision_post_snapshot_id: Option<i64>,
    decision_target_reserve: String,
    decision_liquidity_mint: String,
    decision_amount_raw: i64,
    principal_usd_micros: i64,
    estimated_edge_bps: i64,
    estimated_cost_lamports: i64,
    expected_net_gain_usd_micros: i64,
    economic_priority: i64,
    compiled_fee_lamports: i64,
    execution_plan: Value,
    decision_execution_plan: Value,
    created_at: DateTime<Utc>,
    submitted_slot: Option<i64>,
    submitted_at: Option<DateTime<Utc>>,
    confirmed_at: Option<DateTime<Utc>>,
    reconciled_at: Option<DateTime<Utc>>,
    confirmed_slot: Option<i64>,
    reconciled_slot: Option<i64>,
    broadcast_count: i32,
    last_broadcast_at: Option<DateTime<Utc>>,
    last_valid_block_height: i64,
    expiry_observed_block_height: Option<i64>,
    effect_check_slot: Option<i64>,
    last_status_checked_at: Option<DateTime<Utc>>,
    source_snapshot_id: Option<i64>,
    source_snapshot_vault_id: Option<i64>,
    source_snapshot_context: Option<Value>,
    post_snapshot_id: Option<i64>,
    post_snapshot_vault_id: Option<i64>,
    post_snapshot_context: Option<Value>,
    pre_target_snapshot_id: Option<i64>,
    pre_target_snapshot_vault_id: Option<i64>,
    pre_target_snapshot_context: Option<Value>,
    pre_target_planning_metadata: Option<Value>,
    post_snapshot_observed_slot: Option<i64>,
    post_snapshot_observed_at: Option<DateTime<Utc>>,
    pre_target_snapshot_observed_slot: Option<i64>,
    pre_target_snapshot_observed_at: Option<DateTime<Utc>>,
    pre_source_amount_raw: Option<i64>,
    post_source_amount_raw: Option<i64>,
    pre_target_liquidity_mint: Option<String>,
    pre_target_has_value: Option<bool>,
    pre_target_amount_raw: Option<i64>,
    post_target_liquidity_mint: Option<String>,
    post_target_has_value: Option<bool>,
    post_target_amount_raw: Option<i64>,
    post_target_planning_metadata: Option<Value>,
}

#[derive(Debug, Clone)]
struct SignatureFinality {
    found: bool,
    finalized: bool,
    successful: bool,
    slot: Option<i64>,
}

#[derive(Debug, Clone)]
struct FinalityEvidence {
    statuses: BTreeMap<String, SignatureFinality>,
    finalized_block_height: i64,
    finalized_slot: i64,
}

#[derive(Debug, Default, Clone, Copy)]
struct AltRenderRuntime {
    active_mutator_count: i64,
    budget_window_seconds: Option<i64>,
    budget_maximum_lamports: Option<i64>,
}

#[derive(Debug)]
struct ActiveAltTable {
    table_address: String,
    authority: String,
    payer: String,
    family_authority: String,
    family_payer: String,
    usable_address_count: i32,
    persisted_prefix: Vec<String>,
}

#[derive(Debug)]
struct RpcAltAccount {
    owner: String,
    authority: Option<String>,
    active: bool,
    addresses: Vec<String>,
}

#[derive(Debug)]
struct RetryPrefixEvidence {
    route_lookup_table_id: i64,
    table_address: String,
    authority: String,
    finalized_address_count: i32,
    finalized_address_hash: String,
    persisted_addresses: Vec<String>,
}

#[derive(Debug)]
struct DatabaseEvidence {
    migrations: Value,
    queue: Value,
    positions: Value,
    movements: Value,
    alt_repair: Value,
    migrations_pass: bool,
    queue_verdict: Verdict,
    movement_verdict: Verdict,
}

fn default_repository_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../..")
        .to_path_buf()
}

fn parse_options() -> Result<Option<Options>, Box<dyn Error>> {
    let mut repository_root = default_repository_root();
    let mut cluster = DEFAULT_CLUSTER.to_owned();
    let mut render_environment_id = env::var("YIELD_RENDER_PRODUCTION_ENVIRONMENT_ID")
        .ok()
        .filter(|value| !value.trim().is_empty())
        .unwrap_or_else(|| DEFAULT_RENDER_ENVIRONMENT_ID.to_owned());
    let mut cutover_at = None;
    let mut baseline = None;
    let mut output = None;
    let mut compact = false;
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--repository-root" => {
                repository_root =
                    PathBuf::from(args.next().ok_or("--repository-root requires a path")?);
            }
            "--cluster" => cluster = args.next().ok_or("--cluster requires a value")?,
            "--render-environment-id" => {
                render_environment_id = args
                    .next()
                    .ok_or("--render-environment-id requires a value")?;
            }
            "--cutover-at" => {
                let value = args
                    .next()
                    .ok_or("--cutover-at requires an RFC3339 value")?;
                cutover_at = Some(
                    DateTime::parse_from_rfc3339(&value)
                        .map_err(|_| "--cutover-at must be RFC3339")?
                        .with_timezone(&Utc),
                );
            }
            "--baseline" => {
                baseline = Some(PathBuf::from(
                    args.next().ok_or("--baseline requires a path")?,
                ));
            }
            "--output" => {
                output = Some(PathBuf::from(
                    args.next().ok_or("--output requires a path")?,
                ));
            }
            "--compact" | "--json" => compact = true,
            "--help" | "-h" => {
                println!(
                    "fleet-orchestration-production-evidence \
                     [--repository-root PATH] [--cluster NAME] \
                     [--render-environment-id ID] [--cutover-at RFC3339] \
                     [--baseline PATH] [--output PATH] [--json|--compact]\n\n\
                     Read-only production verification. Reads NEON_DATABASE_URL, \
                     RENDER_API_KEY, and (when --cutover-at is set) SOLANA_RPC_URL. \
                     Output never includes environment values, database/RPC URLs, \
                     signer material, or signed transaction bytes. Capture --output \
                     before cutover, then pass that artifact with --baseline after cutover."
                );
                return Ok(None);
            }
            other => return Err(format!("unknown argument: {other}").into()),
        }
    }
    if cluster.trim().is_empty()
        || render_environment_id.trim().is_empty()
        || !render_environment_id.starts_with("evm-")
    {
        return Err("cluster and a Render environment ID are required".into());
    }
    let repository_root = fs::canonicalize(repository_root)?;
    if !repository_root.join("render.yaml").is_file() {
        return Err("repository root does not contain render.yaml".into());
    }
    Ok(Some(Options {
        repository_root,
        cluster,
        render_environment_id,
        cutover_at,
        baseline,
        output,
        compact,
    }))
}

fn sha256_hex(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn git_output(repository_root: &Path, args: &[&str]) -> Option<String> {
    let output = Command::new("git")
        .arg("-C")
        .arg(repository_root)
        .args(args)
        .output()
        .ok()?;
    output
        .status
        .success()
        .then(|| String::from_utf8_lossy(&output.stdout).trim().to_owned())
}

fn source_evidence(repository_root: &Path, render_yaml: &str) -> Value {
    // Include untracked source files. In particular, a locally rebuilt but
    // uncommitted collector must never be able to describe itself as bound to
    // repositoryHead merely because Git was asked to ignore its source file.
    let status = git_output(repository_root, &["status", "--porcelain"]).unwrap_or_default();
    json!({
        "repositoryHead": git_output(repository_root, &["rev-parse", "HEAD"]),
        "trackedWorktreeDirty": !status.is_empty(),
        "renderYamlSha256": sha256_hex(render_yaml.as_bytes()),
        "collectorSource": "compiled production-owned measurements; caller verdicts are non-authoritative",
    })
}

fn production_environment(render_yaml: &str) -> Option<&str> {
    let project = render_yaml.find("  - name: loyal-yield-light-workers")?;
    let project = &render_yaml[project..];
    let production_start = project.find("      - name: production")?;
    let production = &project[production_start..];
    let staging_start = production.find("      - name: staging");
    Some(staging_start.map_or(production, |end| &production[..end]))
}

fn service_blocks(production: &str) -> Vec<String> {
    let mut blocks = Vec::new();
    let mut current = Vec::new();
    for line in production.lines() {
        if line.starts_with("          - type:") && !current.is_empty() {
            blocks.push(current.join("\n"));
            current.clear();
        }
        if !current.is_empty() || line.starts_with("          - type:") {
            current.push(line);
        }
    }
    if !current.is_empty() {
        blocks.push(current.join("\n"));
    }
    blocks
}

fn scalar(block: &str, key: &str) -> Option<String> {
    let prefix = format!("{key}:");
    block.lines().find_map(|line| {
        let trimmed = line.trim();
        trimmed
            .strip_prefix(&prefix)
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .map(str::to_owned)
    })
}

fn expected_services(render_yaml: &str) -> Result<Vec<ExpectedService>, Box<dyn Error>> {
    let production = production_environment(render_yaml)
        .ok_or("render.yaml has no loyal-light-workers production environment")?;
    let blocks = service_blocks(production);
    let mut expected = Vec::new();
    for required_name in DURABLE_SERVICE_NAMES {
        let block = blocks
            .iter()
            .find(|block| scalar(block, "name").as_deref() == Some(required_name))
            .ok_or_else(|| format!("render.yaml is missing {required_name}"))?;
        let env_keys = block
            .lines()
            .filter_map(|line| line.trim().strip_prefix("- key:"))
            .map(str::trim)
            .map(str::to_owned)
            .collect();
        expected.push(ExpectedService {
            name: required_name.to_owned(),
            image: scalar(block, "url")
                .ok_or_else(|| format!("{required_name} has no image URL"))?,
            command: scalar(block, "dockerCommand")
                .ok_or_else(|| format!("{required_name} has no command"))?,
            pre_deploy_command: scalar(block, "preDeployCommand")
                .ok_or_else(|| format!("{required_name} has no pre-deploy command"))?,
            plan: scalar(block, "plan").ok_or_else(|| format!("{required_name} has no plan"))?,
            env_keys,
        });
    }
    Ok(expected)
}

fn role_env_boundaries(name: &str, keys: &BTreeSet<String>) -> (bool, Vec<String>) {
    let mut failures = Vec::new();
    let require = |key: &str, failures: &mut Vec<String>| {
        if !keys.contains(key) {
            failures.push(format!("missing:{key}"));
        }
    };
    let forbid = |key: &str, failures: &mut Vec<String>| {
        if keys.contains(key) {
            failures.push(format!("forbidden:{key}"));
        }
    };
    require("NEON_DATABASE_URL", &mut failures);
    require("YIELD_ALT_CLUSTER", &mut failures);
    require("RUST_LOG", &mut failures);
    match name {
        "loyal-fleet-opportunity-planner" => {
            require("TIMESCALEDB_URL", &mut failures);
            forbid("SOLANA_RPC_URL", &mut failures);
            forbid("POLICY_KEYPAIR", &mut failures);
            forbid("YIELD_ROUTE_FEE_PAYER_KEYPAIRS", &mut failures);
        }
        "loyal-fleet-route-revalidator" | "loyal-fleet-route-executor" => {
            require("TIMESCALEDB_URL", &mut failures);
            require("SOLANA_RPC_URL", &mut failures);
            require("POLICY_KEYPAIR", &mut failures);
            // The fee-only shard pool is intentionally optional. POLICY_KEYPAIR
            // remains the standard policy signer and fallback fee payer.
        }
        "loyal-fleet-route-confirmer" | "loyal-fleet-route-reconciler" => {
            require("SOLANA_RPC_URL", &mut failures);
            forbid("POLICY_KEYPAIR", &mut failures);
            forbid("YIELD_ROUTE_FEE_PAYER_KEYPAIRS", &mut failures);
        }
        "loyal-route-lookup-table-provisioner" => {
            require("SOLANA_RPC_URL", &mut failures);
            require("POLICY_KEYPAIR", &mut failures);
            require("YIELD_ALT_MAX_LAMPORTS", &mut failures);
            require("YIELD_ALT_BUDGET_WINDOW_SECONDS", &mut failures);
            forbid("YIELD_ROUTE_FEE_PAYER_KEYPAIRS", &mut failures);
        }
        _ => failures.push("unknown_role".to_owned()),
    }
    (failures.is_empty(), failures)
}

async fn render_get(
    client: &Client,
    api_key: &str,
    path: &str,
    query: &[(&str, &str)],
) -> Result<Value, String> {
    let response = client
        .get(format!("{RENDER_API_BASE_URL}{path}"))
        .bearer_auth(api_key)
        .query(query)
        .send()
        .await
        .map_err(|_| "render_api_request_failed".to_owned())?;
    if !response.status().is_success() {
        return Err(format!("render_api_status_{}", response.status().as_u16()));
    }
    response
        .json::<Value>()
        .await
        .map_err(|_| "render_api_json_invalid".to_owned())
}

fn wrapped_array<'a>(value: &'a Value, key: &str) -> Vec<&'a Value> {
    value
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|item| item.get(key).or(Some(item)))
        .collect()
}

fn json_string(value: &Value, pointer: &str) -> Option<String> {
    value.pointer(pointer)?.as_str().map(str::to_owned)
}

async fn collect_render_evidence(
    expected: &[ExpectedService],
    environment_id: &str,
    cluster: &str,
) -> (Value, bool, AltRenderRuntime) {
    let Some(api_key) = env::var("RENDER_API_KEY")
        .ok()
        .filter(|value| !value.trim().is_empty())
    else {
        return (
            json!({
                "available": false,
                "error": "RENDER_API_KEY is missing",
                "environmentId": environment_id,
            }),
            false,
            AltRenderRuntime::default(),
        );
    };
    let client = Client::new();
    let services_response = match render_get(
        &client,
        &api_key,
        "/services",
        &[("environmentId", environment_id), ("limit", "100")],
    )
    .await
    {
        Ok(value) => value,
        Err(error) => {
            return (
                json!({
                    "available": false,
                    "error": error,
                    "environmentId": environment_id,
                }),
                false,
                AltRenderRuntime::default(),
            )
        }
    };
    let live_services = wrapped_array(&services_response, "service");
    let mut role_measurements = Vec::new();
    let mut all_roles_match = true;
    let mut deploy_digests = BTreeSet::new();
    let mut sender_started_at = Vec::new();
    let mut alt_runtime = AltRenderRuntime::default();

    for expected_service in expected {
        let Some(service) = live_services.iter().copied().find(|service| {
            json_string(service, "/name").as_deref() == Some(expected_service.name.as_str())
        }) else {
            all_roles_match = false;
            role_measurements.push(json!({
                "name": expected_service.name,
                "present": false,
                "matches": false,
            }));
            continue;
        };
        let service_id = json_string(service, "/id").unwrap_or_default();
        let deploy_response = render_get(
            &client,
            &api_key,
            &format!("/services/{service_id}/deploys"),
            &[("limit", "1")],
        )
        .await;
        let env_response = render_get(
            &client,
            &api_key,
            &format!("/services/{service_id}/env-vars"),
            &[("limit", "100")],
        )
        .await;
        let latest_deploy = deploy_response
            .as_ref()
            .ok()
            .and_then(|value| wrapped_array(value, "deploy").into_iter().next());
        let live_env = env_response
            .as_ref()
            .ok()
            .map(|value| {
                wrapped_array(value, "envVar")
                    .into_iter()
                    .filter_map(|entry| {
                        Some((json_string(entry, "/key")?, json_string(entry, "/value")))
                    })
                    .collect::<BTreeMap<_, _>>()
            })
            .unwrap_or_default();
        let live_env_keys = live_env.keys().cloned().collect::<BTreeSet<_>>();
        let (mut env_boundary_passes, mut env_failures) =
            role_env_boundaries(&expected_service.name, &live_env_keys);
        let (blueprint_env_boundary_passes, blueprint_env_failures) =
            role_env_boundaries(&expected_service.name, &expected_service.env_keys);
        let requires_standard_policy = matches!(
            expected_service.name.as_str(),
            "loyal-fleet-route-revalidator"
                | "loyal-fleet-route-executor"
                | "loyal-route-lookup-table-provisioner"
        );
        let standard_policy_identity_verified = !requires_standard_policy
            || live_env
                .get("POLICY_KEYPAIR")
                .and_then(Option::as_deref)
                .and_then(|value| loyal_yield_orchestrator::keypair_from_string(value).ok())
                .is_some_and(|keypair| keypair.pubkey().to_string() == STANDARD_POLICY_AUTHORITY);
        if !standard_policy_identity_verified {
            env_boundary_passes = false;
            env_failures.push("invalid:POLICY_KEYPAIR_standard_identity".to_owned());
        }
        let mut require_exact_value = |key: &str, expected: Option<&str>| {
            let matches = expected.is_some_and(|expected| {
                live_env.get(key).and_then(Option::as_deref) == Some(expected)
            });
            if !matches {
                env_boundary_passes = false;
                env_failures.push(format!("invalid:{key}_scope"));
            }
        };
        require_exact_value(
            "NEON_DATABASE_URL",
            env::var("NEON_DATABASE_URL").ok().as_deref(),
        );
        require_exact_value("YIELD_ALT_CLUSTER", Some(cluster));
        if live_env_keys.contains("SOLANA_RPC_URL") {
            require_exact_value("SOLANA_RPC_URL", env::var("SOLANA_RPC_URL").ok().as_deref());
        }
        if live_env_keys.contains("TIMESCALEDB_URL") {
            require_exact_value(
                "TIMESCALEDB_URL",
                env::var("TIMESCALEDB_URL").ok().as_deref(),
            );
        }
        let live_scope_verified = env_boundary_passes;
        let raw_live_command =
            json_string(service, "/serviceDetails/envSpecificDetails/dockerCommand");
        let raw_live_pre_deploy = json_string(
            service,
            "/serviceDetails/envSpecificDetails/preDeployCommand",
        );
        // Commands are evidence only when they exactly equal the checked-in
        // Blueprint. A mismatching live command could contain an accidentally
        // pasted secret, so fail closed without reflecting it into the artifact.
        let live_command = raw_live_command.as_ref().map(|command| {
            if command == &expected_service.command && live_scope_verified {
                command.clone()
            } else if !live_scope_verified {
                "[redacted: live role identity or data scope was not verified]".to_owned()
            } else {
                "[redacted: live command differs from blueprint]".to_owned()
            }
        });
        let live_pre_deploy = raw_live_pre_deploy.as_ref().map(|command| {
            if command == &expected_service.pre_deploy_command {
                command.clone()
            } else {
                "[redacted: live pre-deploy command differs from blueprint]".to_owned()
            }
        });
        let image_path = json_string(service, "/imagePath");
        let deploy_status = latest_deploy.and_then(|deploy| json_string(deploy, "/status"));
        let deploy_ref = latest_deploy.and_then(|deploy| json_string(deploy, "/image/ref"));
        let deploy_digest = latest_deploy.and_then(|deploy| json_string(deploy, "/image/sha"));
        let deploy_registry =
            latest_deploy.and_then(|deploy| json_string(deploy, "/image/registryCredential"));
        let deploy_started = latest_deploy
            .and_then(|deploy| json_string(deploy, "/startedAt"))
            .and_then(|value| DateTime::parse_from_rfc3339(&value).ok())
            .map(|value| value.with_timezone(&Utc));
        if matches!(
            expected_service.name.as_str(),
            "loyal-fleet-route-revalidator" | "loyal-fleet-route-executor"
        ) {
            if let Some(started) = deploy_started {
                sender_started_at.push(started);
            }
        }
        if let Some(digest) = deploy_digest.as_ref() {
            deploy_digests.insert(digest.clone());
        }
        let role_matches = json_string(service, "/suspended").as_deref() == Some("not_suspended")
            && json_string(service, "/type").as_deref() == Some("background_worker")
            && json_string(service, "/serviceDetails/runtime").as_deref() == Some("image")
            && json_string(service, "/serviceDetails/plan").as_deref()
                == Some(expected_service.plan.as_str())
            && image_path.as_deref() == Some(expected_service.image.as_str())
            && raw_live_command.as_deref() == Some(expected_service.command.as_str())
            && raw_live_pre_deploy.as_deref() == Some(expected_service.pre_deploy_command.as_str())
            && deploy_status.as_deref() == Some("live")
            && deploy_ref.as_deref() == Some(expected_service.image.as_str())
            && deploy_digest
                .as_deref()
                .is_some_and(|digest| digest.starts_with("sha256:"))
            && deploy_registry.as_deref() == Some("loyal-ghcr")
            && env_response.is_ok()
            && env_boundary_passes
            && blueprint_env_boundary_passes;
        if expected_service.name == "loyal-route-lookup-table-provisioner" {
            alt_runtime.budget_maximum_lamports = live_env
                .get("YIELD_ALT_MAX_LAMPORTS")
                .and_then(Option::as_deref)
                .and_then(|value| value.parse::<i64>().ok());
            alt_runtime.budget_window_seconds = live_env
                .get("YIELD_ALT_BUDGET_WINDOW_SECONDS")
                .and_then(Option::as_deref)
                .and_then(|value| value.parse::<i64>().ok());
        }
        all_roles_match &= role_matches;
        role_measurements.push(json!({
            "name": expected_service.name,
            "id": service_id,
            "present": true,
            "matches": role_matches,
            "suspended": json_string(service, "/suspended"),
            "runtime": json_string(service, "/serviceDetails/runtime"),
            "plan": json_string(service, "/serviceDetails/plan"),
            "numInstances": service.pointer("/serviceDetails/numInstances"),
            "image": image_path,
            "command": live_command,
            "preDeployCommand": live_pre_deploy,
            "envKeys": live_env_keys,
            "envBoundaryPasses": env_boundary_passes,
            "envBoundaryFailures": env_failures,
            "blueprintEnvKeys": expected_service.env_keys,
            "blueprintEnvBoundaryPasses": blueprint_env_boundary_passes,
            "blueprintEnvBoundaryFailures": blueprint_env_failures,
            "latestDeploy": latest_deploy.map(|deploy| json!({
                "id": json_string(deploy, "/id"),
                "status": deploy_status,
                "imageRef": deploy_ref,
                "imageDigest": deploy_digest,
                "registryCredential": deploy_registry,
                "startedAt": json_string(deploy, "/startedAt"),
                "finishedAt": json_string(deploy, "/finishedAt"),
            })),
            "deployReadError": deploy_response.err(),
            "envReadError": env_response.err(),
        }));
    }

    let serial = live_services
        .iter()
        .copied()
        .find(|service| json_string(service, "/name").as_deref() == Some(SERIAL_MONITOR_NAME));
    let (serial_measurement, serial_currently_incapable, serial_suspended_at) =
        if let Some(service) = serial {
            let service_id = json_string(service, "/id").unwrap_or_default();
            let events = render_get(
                &client,
                &api_key,
                &format!("/services/{service_id}/events"),
                &[("limit", "100")],
            )
            .await;
            let safe_events = events
                .as_ref()
                .ok()
                .map(|value| {
                    wrapped_array(value, "event")
                        .into_iter()
                        .filter_map(|event| {
                            let event_type = json_string(event, "/type")?;
                            (event_type.contains("suspend") || event_type.contains("resume")).then(
                                || {
                                    json!({
                                        "type": event_type,
                                        "timestamp": json_string(event, "/timestamp"),
                                    })
                                },
                            )
                        })
                        .collect::<Vec<_>>()
                })
                .unwrap_or_default();
            let suspended_at = safe_events
                .iter()
                .filter_map(|event| {
                    let event_type = event.get("type")?.as_str()?;
                    if !event_type.contains("suspend") {
                        return None;
                    }
                    DateTime::parse_from_rfc3339(event.get("timestamp")?.as_str()?)
                        .ok()
                        .map(|value| value.with_timezone(&Utc))
                })
                .max();
            let raw_command =
                json_string(service, "/serviceDetails/envSpecificDetails/dockerCommand")
                    .unwrap_or_default();
            let suspended = json_string(service, "/suspended").as_deref() == Some("suspended");
            let scaled_zero = service
                .pointer("/serviceDetails/numInstances")
                .and_then(Value::as_i64)
                == Some(0);
            let execute_absent = !raw_command
                .split_ascii_whitespace()
                .any(|part| part == "--execute");
            let incapable = suspended || scaled_zero || execute_absent;
            // Only the execute capability is needed by the verifier. Never
            // echo an arbitrary legacy command (and any accidental values in
            // it) into the production evidence artifact.
            let command = if execute_absent {
                "[redacted legacy command; --execute absent]"
            } else {
                "[redacted legacy command] --execute"
            };
            (
                json!({
                    "id": service_id,
                    "present": true,
                    "suspended": suspended,
                    "scaledToZero": scaled_zero,
                    "executeFlagAbsent": execute_absent,
                    "currentlyIncapableOfSending": incapable,
                    "command": command,
                    "suspensionEvents": safe_events,
                    "eventReadError": events.err(),
                }),
                incapable,
                suspended_at,
            )
        } else {
            (
                json!({
                    "present": false,
                    "currentlyIncapableOfSending": true,
                }),
                true,
                Some(DateTime::<Utc>::MIN_UTC),
            )
        };
    alt_runtime.active_mutator_count = i64::try_from(
        live_services
            .iter()
            .filter(|service| {
                let command =
                    json_string(service, "/serviceDetails/envSpecificDetails/dockerCommand")
                        .unwrap_or_default();
                json_string(service, "/suspended").as_deref() == Some("not_suspended")
                    && service
                        .pointer("/serviceDetails/numInstances")
                        .and_then(Value::as_i64)
                        .is_none_or(|count| count > 0)
                    && command
                        .split_ascii_whitespace()
                        .any(|part| part.ends_with("route-lookup-table-provisioner"))
                    && command
                        .split_ascii_whitespace()
                        .any(|part| part == "--execute")
            })
            .count(),
    )
    .unwrap_or(i64::MAX);
    let first_sender_started_at = sender_started_at.into_iter().min();
    let no_dual_execution_order = match (serial_suspended_at, first_sender_started_at) {
        (Some(suspended_at), Some(sender_started_at)) => suspended_at <= sender_started_at,
        (Some(_), None) => false,
        (None, _) => false,
    };
    let one_digest = deploy_digests.len() == 1;
    let expected_images = expected
        .iter()
        .map(|role| role.image.clone())
        .collect::<BTreeSet<_>>();
    let render_pass = expected.len() == DURABLE_SERVICE_NAMES.len()
        && expected_images.len() == 1
        && all_roles_match
        && one_digest
        && serial_currently_incapable
        && no_dual_execution_order;
    (
        json!({
            "available": true,
            "environmentId": environment_id,
            "expectedImageReferences": expected_images,
            "roles": role_measurements,
            "allRolesMatch": all_roles_match,
            "deployDigests": deploy_digests,
            "oneImmutableDigest": one_digest,
            "serialMonitor": serial_measurement,
            "firstFleetSenderDeployStartedAt": first_sender_started_at,
            "serialSuspendedBeforeFleetSenderStarted": no_dual_execution_order,
            "pass": render_pass,
        }),
        render_pass,
        alt_runtime,
    )
}

async fn connect_database(database_url: &str) -> Result<PgPool, ()> {
    let options = PgConnectOptions::from_str(database_url)
        .map_err(|_| ())?
        .statement_cache_capacity(0);
    PgPoolOptions::new()
        .max_connections(2)
        .connect_with(options)
        .await
        .map_err(|_| ())
}

async fn relation_exists(pool: &PgPool, relation: &str) -> Result<bool, sqlx::Error> {
    sqlx::query_scalar("SELECT to_regclass($1) IS NOT NULL")
        .bind(relation)
        .fetch_one(pool)
        .await
}

fn ordered_address_hash(addresses: &[String]) -> String {
    let mut hasher = Sha256::new();
    for address in addresses {
        hasher.update((address.len() as u64).to_le_bytes());
        hasher.update(address.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

async fn finalized_alt_accounts(
    rpc_url: &str,
    table_addresses: &[String],
) -> Result<(i64, BTreeMap<String, Option<RpcAltAccount>>), ()> {
    let client = Client::new();
    let slot_response = client
        .post(rpc_url)
        .json(&json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "getSlot",
            "params": [{"commitment": "finalized"}],
        }))
        .send()
        .await
        .map_err(|_| ())?;
    if !slot_response.status().is_success() {
        return Err(());
    }
    let finalized_slot = slot_response
        .json::<Value>()
        .await
        .map_err(|_| ())?
        .pointer("/result")
        .and_then(Value::as_i64)
        .filter(|slot| *slot > 0)
        .ok_or(())?;
    let mut accounts = table_addresses
        .iter()
        .cloned()
        .map(|address| (address, None))
        .collect::<BTreeMap<_, _>>();
    let valid_addresses = table_addresses
        .iter()
        .filter(|address| Pubkey::from_str(address).is_ok())
        .cloned()
        .collect::<Vec<_>>();
    for (batch_index, batch) in valid_addresses.chunks(100).enumerate() {
        let response = client
            .post(rpc_url)
            .json(&json!({
                "jsonrpc": "2.0",
                "id": batch_index + 2,
                "method": "getMultipleAccounts",
                "params": [batch, {
                    "commitment": "finalized",
                    "encoding": "base64",
                    "minContextSlot": finalized_slot,
                }],
            }))
            .send()
            .await
            .map_err(|_| ())?;
        if !response.status().is_success() {
            return Err(());
        }
        let value = response.json::<Value>().await.map_err(|_| ())?;
        let observed_slot = value
            .pointer("/result/context/slot")
            .and_then(Value::as_i64)
            .filter(|slot| *slot >= finalized_slot)
            .ok_or(())?;
        let values = value
            .pointer("/result/value")
            .and_then(Value::as_array)
            .ok_or(())?;
        if values.len() != batch.len() || observed_slot < finalized_slot {
            return Err(());
        }
        for (address, value) in batch.iter().zip(values) {
            if value.is_null() {
                continue;
            }
            let owner = value
                .get("owner")
                .and_then(Value::as_str)
                .ok_or(())?
                .to_owned();
            let encoded = value.pointer("/data/0").and_then(Value::as_str).ok_or(())?;
            let data = BASE64_STANDARD.decode(encoded).map_err(|_| ())?;
            let decoded = if owner == alt_program::id().to_string() {
                AddressLookupTable::deserialize(&data)
                    .ok()
                    .map(|table| RpcAltAccount {
                        owner: owner.clone(),
                        authority: table.meta.authority.map(|authority| authority.to_string()),
                        active: table.meta.deactivation_slot == u64::MAX,
                        addresses: table.addresses.iter().map(ToString::to_string).collect(),
                    })
            } else {
                Some(RpcAltAccount {
                    owner,
                    authority: None,
                    active: false,
                    addresses: Vec::new(),
                })
            };
            accounts.insert(address.clone(), decoded);
        }
    }
    Ok((finalized_slot, accounts))
}

async fn collect_migration_evidence(pool: &PgPool) -> Result<(Value, bool), sqlx::Error> {
    let ledger_exists = relation_exists(pool, "loyal_yield.schema_migrations").await?;
    let expected = REQUIRED_MIGRATIONS
        .iter()
        .map(|(version, name, sql)| (*version, (*name).to_owned(), sha256_hex(sql.as_bytes())))
        .collect::<Vec<_>>();
    if !ledger_exists {
        return Ok((
            json!({
                "ledgerExists": false,
                "required": expected.iter().map(|(version, name, checksum)| json!({
                    "version": version,
                    "name": name,
                    "sourceChecksum": checksum,
                    "applied": false,
                    "matchesSource": false,
                })).collect::<Vec<_>>(),
                "pass": false,
            }),
            false,
        ));
    }
    let rows = sqlx::query(
        r#"
        SELECT version, name, checksum, applied_at
        FROM loyal_yield.schema_migrations
        WHERE version = ANY($1)
        ORDER BY version
        "#,
    )
    .bind(expected.iter().map(|entry| entry.0).collect::<Vec<_>>())
    .fetch_all(pool)
    .await?;
    let applied = rows
        .into_iter()
        .map(|row| {
            Ok::<_, sqlx::Error>((
                row.try_get::<i64, _>("version")?,
                (
                    row.try_get::<String, _>("name")?,
                    row.try_get::<String, _>("checksum")?,
                    row.try_get::<DateTime<Utc>, _>("applied_at")?,
                ),
            ))
        })
        .collect::<Result<BTreeMap<_, _>, _>>()?;
    let measurements = expected
        .iter()
        .map(|(version, name, checksum)| {
            let row = applied.get(version);
            let matches = row.is_some_and(|(applied_name, applied_checksum, _)| {
                applied_name == name && applied_checksum == checksum
            });
            json!({
                "version": version,
                "name": name,
                "sourceChecksum": checksum,
                "applied": row.is_some(),
                "appliedName": row.map(|entry| entry.0.clone()),
                "appliedChecksum": row.map(|entry| entry.1.clone()),
                "appliedAt": row.map(|entry| entry.2),
                "matchesSource": matches,
            })
        })
        .collect::<Vec<_>>();
    let pass = measurements
        .iter()
        .all(|entry| entry.get("matchesSource").and_then(Value::as_bool) == Some(true));
    Ok((
        json!({
            "ledgerExists": true,
            "required": measurements,
            "pass": pass,
        }),
        pass,
    ))
}

fn unavailable_alt_repair_evidence(cluster: &str, runtime: AltRenderRuntime) -> Value {
    json!({
        "available": false,
        "cluster": cluster,
        "finalizedRpcSlot": -1,
        "altProgramId": alt_program::id().to_string(),
        "standardPolicyPubkey": STANDARD_POLICY_AUTHORITY,
        "activeOrReferencedTableCount": -1,
        "activeOrReferencedVerifiedCount": -1,
        "activeOrReferencedWrongOwnerCount": -1,
        "activeOrReferencedAuthorityMismatchCount": -1,
        "activeOrReferencedPrefixMismatchCount": -1,
        "damagedTableCount": -1,
        "damagedNonAllocatingCount": -1,
        "damagedActiveOrPreparingBindingCount": -1,
        "damagedRunnableOperationCount": -1,
        "damagedRouteDependencyCount": -1,
        "historicalTerminalOperationCount": -1,
        "terminalOperationWithRepairEvidenceCount": -1,
        "terminalOperationMissingRepairEvidenceCount": -1,
        "affectedTerminalRequestCount": -1,
        "affectedRequestSatisfiedOrSuccessorCount": -1,
        "unresolvedActiveTerminalRequestCount": -1,
        "validPrefixTableCount": -1,
        "validPrefixPreservedCount": -1,
        "staleSuffixRetryCount": -1,
        "newLegacyOrExactRouteTableCount": -1,
        "activeAltMutatorCount": runtime.active_mutator_count,
        "budgetWindowSeconds": runtime.budget_window_seconds.unwrap_or(-1),
        "budgetMaximumLamports": runtime.budget_maximum_lamports.unwrap_or(-1),
        "budgetSpentLamports": -1,
    })
}

async fn collect_alt_repair_evidence(
    pool: &PgPool,
    cluster: &str,
    rpc_url: Option<&str>,
    runtime: AltRenderRuntime,
) -> Result<Value, sqlx::Error> {
    for relation in [
        "loyal_yield.lookup_table_terminal_repairs",
        "loyal_yield.lookup_table_terminal_repair_operations",
        "loyal_yield.lookup_table_terminal_repair_requests",
        "loyal_yield.lookup_table_cluster_budget_reservations",
        "loyal_yield.lookup_table_provisioner_controls",
    ] {
        if !relation_exists(pool, relation).await? {
            return Ok(unavailable_alt_repair_evidence(cluster, runtime));
        }
    }
    let active_rows = sqlx::query(
        r#"
        SELECT route_table.id, route_table.table_address,
               route_table.authority, route_table.payer,
               family.provisioning_authority AS family_authority,
               family.payer AS family_payer,
               route_table.usable_address_count,
               ARRAY(
                   SELECT membership.address
                   FROM loyal_yield.lookup_table_addresses membership
                   WHERE membership.route_lookup_table_id = route_table.id
                     AND membership.ordinal < route_table.usable_address_count
                   ORDER BY membership.ordinal
               )::TEXT[] AS persisted_prefix
        FROM loyal_yield.route_lookup_tables route_table
        JOIN loyal_yield.lookup_table_families family
          ON family.id = route_table.family_id
        WHERE family.cluster = $1
          AND (
              route_table.desired_state IN (
                  'preparing', 'warming', 'active', 'standby', 'retiring'
              )
              OR EXISTS (
                  SELECT 1
                  FROM loyal_yield.lookup_table_vault_bindings binding
                  WHERE binding.route_lookup_table_id = route_table.id
                    AND binding.lifecycle_state IN (
                        'preparing', 'warming', 'active', 'standby', 'retiring'
                    )
              )
              OR EXISTS (
                  SELECT 1
                  FROM loyal_yield.lookup_table_usage_leases usage
                  WHERE usage.route_lookup_table_id = route_table.id
                    AND usage.released_at IS NULL AND usage.expires_at > now()
              )
              OR EXISTS (
                  SELECT 1
                  FROM loyal_yield.lookup_table_route_readiness_current readiness
                  WHERE readiness.cluster = $1
                    AND (
                        route_table.id = ANY(readiness.reusable_table_ids)
                        OR route_table.id = ANY(readiness.selected_table_ids)
                    )
              )
              OR EXISTS (
                  SELECT 1
                  FROM loyal_yield.lookup_table_operations operation
                  WHERE operation.route_lookup_table_id = route_table.id
                    AND operation.operation_state NOT IN (
                        'complete', 'permanent_failure', 'cancelled'
                    )
              )
          )
        ORDER BY route_table.id
        "#,
    )
    .bind(cluster)
    .fetch_all(pool)
    .await?
    .into_iter()
    .map(|row| {
        Ok::<_, sqlx::Error>(ActiveAltTable {
            table_address: row.try_get("table_address")?,
            authority: row.try_get("authority")?,
            payer: row.try_get("payer")?,
            family_authority: row.try_get("family_authority")?,
            family_payer: row.try_get("family_payer")?,
            usable_address_count: row
                .try_get::<Option<i32>, _>("usable_address_count")?
                .unwrap_or(-1),
            persisted_prefix: row.try_get("persisted_prefix")?,
        })
    })
    .collect::<Result<Vec<_>, _>>()?;
    let retry_prefixes = sqlx::query(
        r#"
        SELECT repair.route_lookup_table_id, route_table.table_address,
               route_table.authority, repair.finalized_address_count,
               repair.finalized_address_hash,
               ARRAY(
                   SELECT membership.address
                   FROM loyal_yield.lookup_table_addresses membership
                   WHERE membership.route_lookup_table_id = route_table.id
                   ORDER BY membership.ordinal
               )::TEXT[] AS persisted_addresses
        FROM loyal_yield.lookup_table_terminal_repairs repair
        JOIN loyal_yield.route_lookup_tables route_table
          ON route_table.id = repair.route_lookup_table_id
        WHERE repair.cluster = $1 AND repair.repair_kind = 'retry_suffix'
        ORDER BY repair.route_lookup_table_id, repair.id
        "#,
    )
    .bind(cluster)
    .fetch_all(pool)
    .await?
    .into_iter()
    .map(|row| {
        Ok::<_, sqlx::Error>(RetryPrefixEvidence {
            route_lookup_table_id: row.try_get("route_lookup_table_id")?,
            table_address: row.try_get("table_address")?,
            authority: row.try_get("authority")?,
            finalized_address_count: row.try_get("finalized_address_count")?,
            finalized_address_hash: row.try_get("finalized_address_hash")?,
            persisted_addresses: row.try_get("persisted_addresses")?,
        })
    })
    .collect::<Result<Vec<_>, _>>()?;
    let mut requested_addresses = active_rows
        .iter()
        .map(|table| table.table_address.clone())
        .chain(
            retry_prefixes
                .iter()
                .map(|table| table.table_address.clone()),
        )
        .collect::<Vec<_>>();
    requested_addresses.sort();
    requested_addresses.dedup();
    let Some(rpc_url) = rpc_url.filter(|value| !value.trim().is_empty()) else {
        return Ok(unavailable_alt_repair_evidence(cluster, runtime));
    };
    let Ok((finalized_rpc_slot, chain_accounts)) =
        finalized_alt_accounts(rpc_url, &requested_addresses).await
    else {
        return Ok(unavailable_alt_repair_evidence(cluster, runtime));
    };
    let alt_program_id = alt_program::id().to_string();
    let mut verified_count = 0i64;
    let mut wrong_owner_count = 0i64;
    let mut authority_mismatch_count = 0i64;
    let mut prefix_mismatch_count = 0i64;
    for table in &active_rows {
        let chain = chain_accounts
            .get(&table.table_address)
            .and_then(Option::as_ref);
        let correct_owner = chain.is_some_and(|account| account.owner == alt_program_id);
        if chain.is_some_and(|account| account.owner != alt_program_id) {
            wrong_owner_count += 1;
        }
        let standard_db_identity = table.authority == STANDARD_POLICY_AUTHORITY
            && table.payer == STANDARD_POLICY_AUTHORITY
            && table.family_authority == STANDARD_POLICY_AUTHORITY
            && table.family_payer == STANDARD_POLICY_AUTHORITY;
        let correct_authority = correct_owner
            && standard_db_identity
            && chain.and_then(|account| account.authority.as_deref())
                == Some(STANDARD_POLICY_AUTHORITY);
        if correct_owner && !correct_authority {
            authority_mismatch_count += 1;
        }
        let usable = usize::try_from(table.usable_address_count).ok();
        let prefix_matches = correct_owner
            && usable.is_some_and(|usable| {
                table.persisted_prefix.len() == usable
                    && chain.is_some_and(|account| {
                        account.active
                            && account.addresses.len() >= usable
                            && account.addresses[..usable] == table.persisted_prefix
                    })
            });
        if correct_owner && !prefix_matches {
            prefix_mismatch_count += 1;
        }
        if correct_owner && correct_authority && prefix_matches {
            verified_count += 1;
        }
    }
    let counters = sqlx::query(
        r#"
        WITH damaged AS (
            SELECT DISTINCT repair.route_lookup_table_id AS id
            FROM loyal_yield.lookup_table_terminal_repairs repair
            WHERE repair.cluster = $1
              AND repair.repair_kind = 'quarantine_phantom'
        ),
        affected_requests AS (
            SELECT request_link.request_id,
                   min(repair.created_at) AS repaired_at
            FROM loyal_yield.lookup_table_terminal_repair_requests request_link
            JOIN loyal_yield.lookup_table_terminal_repairs repair
              ON repair.id = request_link.repair_id
            WHERE repair.cluster = $1
            GROUP BY request_link.request_id
        ),
        resolved_requests AS (
            SELECT affected.request_id
            FROM affected_requests affected
            JOIN loyal_yield.lookup_table_provisioning_requests request
              ON request.id = affected.request_id
            WHERE request.request_status = 'satisfied'
               OR EXISTS (
                    SELECT 1
                    FROM loyal_yield.lookup_table_terminal_repair_requests request_link
                    JOIN loyal_yield.lookup_table_terminal_repairs repair
                      ON repair.id = request_link.repair_id
                    JOIN loyal_yield.lookup_table_operations successor
                      ON successor.id = repair.successor_operation_id
                    JOIN loyal_yield.route_lookup_tables route_table
                      ON route_table.id = successor.route_lookup_table_id
                    WHERE request_link.request_id = request.id
                      AND repair.cluster = $1
                      AND successor.operation_state NOT IN (
                          'permanent_failure', 'cancelled'
                      )
                      AND route_table.desired_state <> 'failed'
                      AND NOT EXISTS (
                          SELECT 1 FROM damaged
                          WHERE damaged.id = route_table.id
                      )
               )
               OR EXISTS (
                    SELECT 1
                    FROM loyal_yield.lookup_table_operations operation
                    JOIN loyal_yield.route_lookup_tables route_table
                      ON route_table.id = operation.route_lookup_table_id
                    WHERE operation.operation_context->>'request_id' = request.id::TEXT
                      AND operation.created_at >= affected.repaired_at
                      AND operation.operation_state NOT IN (
                          'permanent_failure', 'cancelled'
                      )
                      AND route_table.desired_state <> 'failed'
                      AND NOT EXISTS (
                          SELECT 1 FROM damaged
                          WHERE damaged.id = route_table.id
                      )
               )
               OR EXISTS (
                    SELECT 1
                    FROM loyal_yield.lookup_table_vault_bindings binding
                    JOIN loyal_yield.route_lookup_tables route_table
                      ON route_table.id = binding.route_lookup_table_id
                    WHERE binding.vault_id = request.vault_id
                      AND binding.manifest_id = request.vault_manifest_id
                      AND binding.created_at >= affected.repaired_at
                      AND binding.lifecycle_state IN (
                          'preparing', 'warming', 'active', 'standby', 'retiring'
                      )
                      AND route_table.desired_state <> 'failed'
                      AND NOT EXISTS (
                          SELECT 1 FROM damaged
                          WHERE damaged.id = route_table.id
                      )
               )
        ),
        terminal_operations AS (
            SELECT operation.id
            FROM loyal_yield.lookup_table_operations operation
            JOIN loyal_yield.lookup_table_families family
              ON family.id = operation.family_id
            WHERE family.cluster = $1
              AND operation.operation_state = 'permanent_failure'
        ),
        migration_28 AS (
            SELECT applied_at
            FROM loyal_yield.schema_migrations
            WHERE version = 28
              AND name = 'reusable_alt_terminal_repair'
            LIMIT 1
        )
        SELECT
            (SELECT count(*) FROM damaged)::BIGINT AS damaged_table_count,
            (SELECT count(*)
             FROM damaged
             JOIN loyal_yield.route_lookup_tables route_table
               ON route_table.id = damaged.id
             WHERE route_table.desired_state = 'failed'
               AND route_table.status = 'failed'
               AND route_table.accepting_allocations = FALSE
            )::BIGINT AS damaged_nonallocating_count,
            (SELECT count(*)
             FROM loyal_yield.lookup_table_vault_bindings binding
             WHERE binding.route_lookup_table_id IN (SELECT id FROM damaged)
               AND binding.lifecycle_state IN ('preparing', 'warming', 'active')
            )::BIGINT AS damaged_active_binding_count,
            (SELECT count(*)
             FROM loyal_yield.lookup_table_operations operation
             WHERE operation.route_lookup_table_id IN (SELECT id FROM damaged)
               AND operation.operation_state IN (
                   'queued', 'leased', 'signed', 'submitted', 'confirmed',
                   'finalized', 'reconciled', 'retry_wait', 'needs_reconcile'
               )
            )::BIGINT AS damaged_runnable_operation_count,
            (SELECT count(*) FROM (
                SELECT 'readiness:' || readiness.cluster || ':'
                       || readiness.vault_id::TEXT || ':'
                       || readiness.requirements_fingerprint AS dependency
                FROM loyal_yield.lookup_table_route_readiness_current readiness
                WHERE readiness.cluster = $1
                  AND EXISTS (
                      SELECT 1 FROM damaged
                      WHERE damaged.id = ANY(readiness.reusable_table_ids)
                         OR damaged.id = ANY(readiness.selected_table_ids)
                  )
                UNION
                SELECT 'lease:' || usage.id::TEXT
                FROM loyal_yield.lookup_table_usage_leases usage
                WHERE usage.route_lookup_table_id IN (SELECT id FROM damaged)
                  AND usage.released_at IS NULL AND usage.expires_at > now()
            ) dependency)::BIGINT AS damaged_route_dependency_count,
            (SELECT count(*) FROM terminal_operations)::BIGINT
                AS historical_terminal_operation_count,
            (SELECT count(*)
             FROM terminal_operations terminal
             WHERE EXISTS (
                 SELECT 1
                 FROM loyal_yield.lookup_table_terminal_repair_operations repair_operation
                 WHERE repair_operation.operation_id = terminal.id
             )
            )::BIGINT AS terminal_with_repair_count,
            (SELECT count(*)
             FROM terminal_operations terminal
             WHERE NOT EXISTS (
                 SELECT 1
                 FROM loyal_yield.lookup_table_terminal_repair_operations repair_operation
                 WHERE repair_operation.operation_id = terminal.id
             )
            )::BIGINT AS terminal_missing_repair_count,
            (SELECT count(*) FROM affected_requests)::BIGINT AS affected_request_count,
            (SELECT count(*) FROM resolved_requests)::BIGINT AS resolved_request_count,
            (SELECT count(*)
             FROM affected_requests affected
             WHERE NOT EXISTS (
                 SELECT 1 FROM resolved_requests resolved
                 WHERE resolved.request_id = affected.request_id
             )
            )::BIGINT AS unresolved_active_terminal_request_count,
            (SELECT count(*)
             FROM loyal_yield.lookup_table_terminal_repairs repair
             JOIN loyal_yield.lookup_table_operations successor
               ON successor.id = repair.successor_operation_id
             WHERE repair.cluster = $1 AND repair.repair_kind = 'retry_suffix'
               AND successor.operation_state = 'permanent_failure'
               AND NOT EXISTS (
                   SELECT 1
                   FROM loyal_yield.lookup_table_terminal_repair_operations repaired
                   WHERE repaired.operation_id = successor.id
               )
            )::BIGINT AS stale_suffix_retry_count,
            (SELECT count(*)
             FROM loyal_yield.route_lookup_tables route_table
             WHERE route_table.cluster = $1
               AND (
                   route_table.family_id IS NULL
                   OR route_table.allocation_kind = 'dedicated_vault'
               )
               AND route_table.created_at >= (
                   SELECT applied_at FROM migration_28
               )
            )::BIGINT AS new_legacy_or_exact_route_count
        "#,
    )
    .bind(cluster)
    .fetch_one(pool)
    .await?;
    let mut preserved_by_table = BTreeMap::<i64, bool>::new();
    for evidence in &retry_prefixes {
        let count = usize::try_from(evidence.finalized_address_count).ok();
        let chain = chain_accounts
            .get(&evidence.table_address)
            .and_then(Option::as_ref);
        let preserved = count.is_some_and(|count| {
            evidence.authority == STANDARD_POLICY_AUTHORITY
                && evidence.persisted_addresses.len() >= count
                && ordered_address_hash(&evidence.persisted_addresses[..count].to_vec())
                    == evidence.finalized_address_hash
                && chain.is_some_and(|account| {
                    account.owner == alt_program_id
                        && account.active
                        && account.authority.as_deref() == Some(STANDARD_POLICY_AUTHORITY)
                        && account.addresses.len() >= count
                        && ordered_address_hash(&account.addresses[..count].to_vec())
                            == evidence.finalized_address_hash
                })
        });
        preserved_by_table
            .entry(evidence.route_lookup_table_id)
            .and_modify(|current| *current &= preserved)
            .or_insert(preserved);
    }
    let valid_prefix_table_count = i64::try_from(preserved_by_table.len()).unwrap_or(i64::MAX);
    let valid_prefix_preserved_count = i64::try_from(
        preserved_by_table
            .values()
            .filter(|preserved| **preserved)
            .count(),
    )
    .unwrap_or(i64::MAX);
    let budget_spent_lamports: i64 = sqlx::query_scalar(
        r#"
        WITH v2_per_operation AS (
            SELECT operation.id AS subject_id,
                   COALESCE(sum(reservation.reserved_lamports), 0)::BIGINT
                       AS reserved_lamports,
                   (COALESCE(operation.actual_fee_lamports, 0)
                    + COALESCE(operation.actual_rent_lamports, 0))::BIGINT
                       AS actual_lamports
            FROM loyal_yield.lookup_table_cluster_budget_reservations reservation
            JOIN loyal_yield.lookup_table_operations operation
              ON operation.id = reservation.operation_id
            WHERE reservation.cluster = $1
              AND reservation.reserved_until > now()
              AND operation.operation_state <> 'cancelled'
            GROUP BY operation.id, operation.actual_fee_lamports,
                     operation.actual_rent_lamports
        ),
        legacy_per_attempt AS (
            SELECT attempt.id AS subject_id,
                   COALESCE(sum(reservation.reserved_lamports), 0)::BIGINT
                       AS reserved_lamports,
                   0::BIGINT AS actual_lamports
            FROM loyal_yield.lookup_table_legacy_cleanup_budget_reservations reservation
            JOIN loyal_yield.lookup_table_legacy_cleanup_attempts attempt
              ON attempt.id = reservation.legacy_cleanup_attempt_id
            WHERE reservation.cluster = $1
              AND reservation.reserved_until > now()
            GROUP BY attempt.id
        ),
        charges AS (
            SELECT reserved_lamports, actual_lamports FROM v2_per_operation
            UNION ALL
            SELECT reserved_lamports, actual_lamports FROM legacy_per_attempt
        )
        SELECT COALESCE(sum(GREATEST(reserved_lamports, actual_lamports)), 0)::BIGINT
        FROM charges
        "#,
    )
    .bind(cluster)
    .fetch_one(pool)
    .await?;
    Ok(json!({
        "available": true,
        "cluster": cluster,
        "finalizedRpcSlot": finalized_rpc_slot,
        "altProgramId": alt_program_id,
        "standardPolicyPubkey": STANDARD_POLICY_AUTHORITY,
        "activeOrReferencedTableCount": active_rows.len(),
        "activeOrReferencedVerifiedCount": verified_count,
        "activeOrReferencedWrongOwnerCount": wrong_owner_count,
        "activeOrReferencedAuthorityMismatchCount": authority_mismatch_count,
        "activeOrReferencedPrefixMismatchCount": prefix_mismatch_count,
        "damagedTableCount": counters.try_get::<i64, _>("damaged_table_count")?,
        "damagedNonAllocatingCount": counters.try_get::<i64, _>("damaged_nonallocating_count")?,
        "damagedActiveOrPreparingBindingCount": counters.try_get::<i64, _>("damaged_active_binding_count")?,
        "damagedRunnableOperationCount": counters.try_get::<i64, _>("damaged_runnable_operation_count")?,
        "damagedRouteDependencyCount": counters.try_get::<i64, _>("damaged_route_dependency_count")?,
        "historicalTerminalOperationCount": counters.try_get::<i64, _>("historical_terminal_operation_count")?,
        "terminalOperationWithRepairEvidenceCount": counters.try_get::<i64, _>("terminal_with_repair_count")?,
        "terminalOperationMissingRepairEvidenceCount": counters.try_get::<i64, _>("terminal_missing_repair_count")?,
        "affectedTerminalRequestCount": counters.try_get::<i64, _>("affected_request_count")?,
        "affectedRequestSatisfiedOrSuccessorCount": counters.try_get::<i64, _>("resolved_request_count")?,
        "unresolvedActiveTerminalRequestCount": counters.try_get::<i64, _>("unresolved_active_terminal_request_count")?,
        "validPrefixTableCount": valid_prefix_table_count,
        "validPrefixPreservedCount": valid_prefix_preserved_count,
        "staleSuffixRetryCount": counters.try_get::<i64, _>("stale_suffix_retry_count")?,
        "newLegacyOrExactRouteTableCount": counters.try_get::<i64, _>("new_legacy_or_exact_route_count")?,
        "activeAltMutatorCount": runtime.active_mutator_count,
        "budgetWindowSeconds": runtime.budget_window_seconds.unwrap_or(-1),
        "budgetMaximumLamports": runtime.budget_maximum_lamports.unwrap_or(-1),
        "budgetSpentLamports": budget_spent_lamports,
    }))
}

async fn collect_position_evidence(pool: &PgPool) -> Result<Value, Box<dyn Error>> {
    let main_reserve = KAMINO_MAIN_USDC_RESERVE.to_string();
    let main_market = KAMINO_MAIN_MARKET.to_string();
    let usdc_mint = USDC_MINT.to_string();
    let enabled_mints = enabled_stable_mints_from_env()?;
    let positions = sqlx::query(
        r#"
        WITH eligible_vaults AS MATERIALIZED (
            SELECT v.id AS vault_id,
                   policy.kamino_markets,
                   policy.stable_mints,
                   policy.kamino_liquidity_mints
            FROM loyal_yield.managed_vaults v
            JOIN loyal_yield.route_policies policy
              ON policy.id = v.active_policy_id
            WHERE v.active = TRUE
              AND policy.active = TRUE
              AND $1 = ANY(policy.delegated_signers)
              AND $2 = ANY(policy.route_modes)
              AND policy.stable_mints && $3::TEXT[]
              AND policy.kamino_liquidity_mints && $3::TEXT[]
              AND cardinality(policy.kamino_markets) > 0
        ),
        main_usdc_cohort AS MATERIALIZED (
            SELECT vault_id
            FROM eligible_vaults
            WHERE $4 = ANY(stable_mints)
              AND $4 = ANY(kamino_liquidity_mints)
              AND $5 = ANY(kamino_markets)
        ),
        routeable_main AS MATERIALIZED (
            SELECT position.*
            FROM loyal_yield.vault_reserve_positions_current position
            JOIN main_usdc_cohort cohort ON cohort.vault_id = position.vault_id
            WHERE position.reserve = $6
              AND position.liquidity_mint = $4
              AND position.has_value
              AND position.amount_raw > 0
              AND (position.market IS NULL OR position.market = $5)
        ),
        global_main AS MATERIALIZED (
            SELECT position.*
            FROM loyal_yield.vault_reserve_positions_current position
            WHERE position.reserve = $6
              AND position.liquidity_mint = $4
              AND position.has_value
              AND position.amount_raw > 0
        )
        SELECT
            (SELECT count(*)::BIGINT FROM main_usdc_cohort) AS cohort_vault_count,
            (SELECT COALESCE(array_agg(vault_id ORDER BY vault_id), ARRAY[]::BIGINT[])
             FROM main_usdc_cohort) AS cohort_vault_ids,
            (SELECT COALESCE(sum(amount_raw), 0)::BIGINT FROM routeable_main)
                AS routeable_amount_raw,
            (SELECT count(*)::BIGINT FROM routeable_main) AS routeable_vault_count,
            (SELECT COALESCE(array_agg(vault_id ORDER BY vault_id), ARRAY[]::BIGINT[])
             FROM routeable_main) AS routeable_vault_ids,
            (SELECT min(observed_at) FROM routeable_main) AS routeable_oldest_observed_at,
            (SELECT max(observed_at) FROM routeable_main) AS routeable_newest_observed_at,
            (SELECT min(observed_slot) FROM routeable_main) AS routeable_minimum_observed_slot,
            (SELECT max(observed_slot) FROM routeable_main) AS routeable_maximum_observed_slot,
            (SELECT count(*)::BIGINT FROM routeable_main
             WHERE observed_at < now() - interval '10 minutes') AS routeable_stale_row_count,
            (SELECT COALESCE(sum(amount_raw), 0)::BIGINT FROM global_main)
                AS global_amount_raw,
            (SELECT count(*)::BIGINT FROM global_main) AS global_vault_count,
            (SELECT COALESCE(array_agg(vault_id ORDER BY vault_id), ARRAY[]::BIGINT[])
             FROM global_main) AS global_vault_ids,
            (SELECT min(observed_at) FROM global_main) AS global_oldest_observed_at,
            (SELECT max(observed_at) FROM global_main) AS global_newest_observed_at,
            (SELECT min(observed_slot) FROM global_main) AS global_minimum_observed_slot,
            (SELECT max(observed_slot) FROM global_main) AS global_maximum_observed_slot,
            (SELECT count(*)::BIGINT FROM global_main
             WHERE observed_at < now() - interval '10 minutes') AS global_stale_row_count
        "#,
    )
    .bind(STANDARD_POLICY_AUTHORITY)
    .bind(SAME_MINT_ROUTE_MODE)
    .bind(&enabled_mints)
    .bind(&usdc_mint)
    .bind(&main_market)
    .bind(&main_reserve)
    .fetch_one(pool)
    .await?;
    let reserve_aggregates: Value = sqlx::query_scalar(
        r#"
        SELECT COALESCE(jsonb_agg(to_jsonb(aggregate_row)
                   ORDER BY aggregate_row.amount_raw DESC), '[]'::jsonb)
        FROM (
            SELECT reserve, liquidity_mint,
                   sum(amount_raw)::BIGINT AS amount_raw,
                   count(*)::BIGINT AS vault_count,
                   min(observed_at) AS oldest_observed_at,
                   max(observed_at) AS newest_observed_at,
                   min(observed_slot) AS minimum_observed_slot,
                   max(observed_slot) AS maximum_observed_slot
            FROM loyal_yield.vault_reserve_positions_current
            WHERE has_value AND amount_raw > 0
            GROUP BY reserve, liquidity_mint
        ) aggregate_row
        "#,
    )
    .fetch_one(pool)
    .await?;
    let routeable_amount_raw = positions.try_get::<i64, _>("routeable_amount_raw")?;
    let routeable_stale_row_count = positions.try_get::<i64, _>("routeable_stale_row_count")?;
    let global_amount_raw = positions.try_get::<i64, _>("global_amount_raw")?;
    let global_stale_row_count = positions.try_get::<i64, _>("global_stale_row_count")?;
    Ok(json!({
        "available": true,
        "mainUsdcCohort": {
            "standardPolicyPubkey": STANDARD_POLICY_AUTHORITY,
            "routeMode": SAME_MINT_ROUTE_MODE,
            "reserve": main_reserve,
            "market": main_market,
            "liquidityMint": usdc_mint,
            "enabledStableMints": enabled_mints,
            "vaultCount": positions.try_get::<i64, _>("cohort_vault_count")?,
            "vaultIds": positions.try_get::<Vec<i64>, _>("cohort_vault_ids")?,
        },
        "mainUsdc": {
            "reserve": main_reserve,
            "liquidityMint": usdc_mint,
            "amountRaw": routeable_amount_raw,
            "amountUsdc": routeable_amount_raw as f64 / 1_000_000.0,
            "vaultCount": positions.try_get::<i64, _>("routeable_vault_count")?,
            "vaultIds": positions.try_get::<Vec<i64>, _>("routeable_vault_ids")?,
            "oldestObservedAt": positions.try_get::<Option<DateTime<Utc>>, _>("routeable_oldest_observed_at")?,
            "newestObservedAt": positions.try_get::<Option<DateTime<Utc>>, _>("routeable_newest_observed_at")?,
            "minimumObservedSlot": positions.try_get::<Option<i64>, _>("routeable_minimum_observed_slot")?,
            "maximumObservedSlot": positions.try_get::<Option<i64>, _>("routeable_maximum_observed_slot")?,
            "freshnessMaximumAgeSeconds": MAX_MATERIAL_STAGE_AGE_SECONDS,
            "staleRowCount": routeable_stale_row_count,
            "freshForBaseline": routeable_stale_row_count == 0,
        },
        "globalMainUsdc": {
            "reserve": main_reserve,
            "liquidityMint": usdc_mint,
            "amountRaw": global_amount_raw,
            "amountUsdc": global_amount_raw as f64 / 1_000_000.0,
            "vaultCount": positions.try_get::<i64, _>("global_vault_count")?,
            "vaultIds": positions.try_get::<Vec<i64>, _>("global_vault_ids")?,
            "oldestObservedAt": positions.try_get::<Option<DateTime<Utc>>, _>("global_oldest_observed_at")?,
            "newestObservedAt": positions.try_get::<Option<DateTime<Utc>>, _>("global_newest_observed_at")?,
            "minimumObservedSlot": positions.try_get::<Option<i64>, _>("global_minimum_observed_slot")?,
            "maximumObservedSlot": positions.try_get::<Option<i64>, _>("global_maximum_observed_slot")?,
            "freshnessMaximumAgeSeconds": MAX_MATERIAL_STAGE_AGE_SECONDS,
            "staleRowCount": global_stale_row_count,
            "freshForBaseline": global_stale_row_count == 0,
        },
        "reserveAggregates": reserve_aggregates,
    }))
}

async fn collect_queue_evidence(pool: &PgPool, cluster: &str) -> Result<Value, sqlx::Error> {
    sqlx::query_scalar(
        r#"
        SELECT jsonb_build_object(
            'available', true,
            'statusRows', COALESCE((
                SELECT jsonb_agg(to_jsonb(status_row)
                                 ORDER BY status_row.opportunity_state NULLS FIRST)
                FROM (
                    SELECT opportunity_state, opportunity_count,
                           principal_usd_micros,
                           annual_yield_gain_usd_micros,
                           yield_gain_usd_micros_per_hour,
                           oldest_age_seconds, oldest_state_age_seconds,
                           expired_lease_count, pending_outbox_count,
                           pending_submission_count,
                           pending_compiled_fee_lamports,
                           expiry_check_pending_count, effect_ambiguous_count,
                           oldest_pending_submission_age_seconds,
                           sender_submission_count,
                           oldest_sender_state_age_seconds,
                           confirmer_submission_count,
                           oldest_confirmer_state_age_seconds,
                           reconciler_submission_count,
                           oldest_reconciler_state_age_seconds,
                           planner_last_seen_age_seconds,
                           full_sweep_age_seconds, complete_frontier,
                           observed_vault_count, planned_opportunity_count,
                           planned_selected_count, planned_deferred_count,
                           latest_market_epoch_id,
                           latest_market_epoch_age_seconds,
                           latest_market_epoch_expires_in_seconds,
                           latest_market_epoch_expired,
                           planner_epoch_matches_latest,
                           waiting_alt_opportunity_count,
                           waiting_alt_principal_usd_micros,
                           waiting_alt_yield_gain_usd_micros_per_hour,
                           oldest_waiting_alt_state_age_seconds,
                           ready_opportunity_count,
                           ready_principal_usd_micros,
                           ready_yield_gain_usd_micros_per_hour,
                           oldest_ready_state_age_seconds,
                           current_epoch_opportunity_count,
                           current_epoch_principal_usd_micros,
                           current_epoch_recoverable_yield_usd_micros_per_hour,
                           current_epoch_submitted_within_10s_yield_ppm,
                           current_epoch_submitted_within_2m_yield_ppm,
                           current_epoch_submitted_within_10m_yield_ppm,
                           current_epoch_confirmed_within_30s_yield_ppm,
                           current_epoch_submission_p95_milliseconds,
                           current_epoch_confirmation_p95_milliseconds,
                           current_epoch_compiled_fee_lamports
                    FROM loyal_yield.fleet_orchestration_status
                    WHERE cluster = $1
                ) status_row
            ), '[]'::jsonb),
            'activeDecisionsByStatus', COALESCE((
                SELECT jsonb_object_agg(status, decision_count)
                FROM (
                    SELECT status::TEXT AS status, count(*)::BIGINT AS decision_count
                    FROM loyal_yield.rebalance_decisions
                    WHERE status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming')
                    GROUP BY status
                ) active
            ), '{}'::jsonb),
            'activeDecisionCount', (
                SELECT count(*)::BIGINT
                FROM loyal_yield.rebalance_decisions
                WHERE status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming')
            ),
            'staleActiveDecisionCount', (
                SELECT count(*)::BIGINT
                FROM loyal_yield.rebalance_decisions
                WHERE status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming')
                  AND updated_at < now() - interval '10 minutes'
            ),
            'duplicateActiveVaultMovementCount', (
                SELECT count(*)::BIGINT
                FROM (
                    SELECT vault_id
                    FROM loyal_yield.rebalance_decisions
                    WHERE status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming')
                    GROUP BY vault_id HAVING count(*) > 1
                ) duplicates
            ),
            'materialStuckOverTenMinutesCount', (
                SELECT count(*)::BIGINT
                FROM loyal_yield.rebalance_opportunities
                WHERE cluster = $1
                  AND principal_usd_micros >= $2
                  AND opportunity_state IN ('waiting_alt', 'revalidate', 'ready', 'leased')
                  AND state_entered_at < now() - interval '10 minutes'
            ),
            'targetCapacityOversubscriptionCount', (
                SELECT count(*)::BIGINT
                FROM loyal_yield.target_capacity_frontiers frontier
                LEFT JOIN LATERAL (
                    SELECT COALESCE(sum(principal_usd_micros), 0)::BIGINT AS reserved
                    FROM loyal_yield.target_capacity_reservations reservation
                    WHERE reservation.cluster = frontier.cluster
                      AND reservation.target_reserve = frontier.target_reserve
                      AND reservation.liquidity_mint = frontier.liquidity_mint
                      AND reservation.reservation_state <> 'released'
                ) live ON TRUE
                WHERE frontier.cluster = $1
                  AND live.reserved > frontier.maximum_inflight_usd_micros
            ),
            'highValueOrderingInversionCount', (
                WITH latest_epoch AS (
                    SELECT id FROM loyal_yield.optimizer_epochs
                    WHERE cluster = $1 ORDER BY observed_at DESC, id DESC LIMIT 1
                ), first_submission AS (
                    SELECT opportunity_id, min(submitted_at) AS submitted_at
                    FROM loyal_yield.signed_route_submissions
                    WHERE cluster = $1 AND submitted_at IS NOT NULL
                    GROUP BY opportunity_id
                ), candidate AS (
                    SELECT opportunity.*, first_submission.submitted_at
                    FROM loyal_yield.rebalance_opportunities opportunity
                    JOIN latest_epoch ON latest_epoch.id = opportunity.optimizer_epoch_id
                    LEFT JOIN first_submission
                      ON first_submission.opportunity_id = opportunity.id
                    WHERE opportunity.principal_usd_micros >= $2
                )
                SELECT count(*)::BIGINT
                FROM candidate lower_value
                JOIN candidate higher_value
                  ON higher_value.economic_priority > lower_value.economic_priority
                 AND higher_value.created_at <= lower_value.submitted_at
                 AND higher_value.vault_id <> lower_value.vault_id
                 AND COALESCE(higher_value.source_reserve, '')
                     <> COALESCE(lower_value.source_reserve, '')
                 AND higher_value.target_reserve <> lower_value.target_reserve
                WHERE lower_value.submitted_at IS NOT NULL
                  AND (higher_value.submitted_at IS NULL
                       OR higher_value.submitted_at > lower_value.submitted_at)
                  AND higher_value.opportunity_state NOT IN (
                      'waiting_alt', 'revalidate', 'failed', 'cancelled',
                      'stale', 'superseded'
                  )
            ),
            'topCurrentEpochOpportunities', COALESCE((
                WITH latest_epoch AS (
                    SELECT id FROM loyal_yield.optimizer_epochs
                    WHERE cluster = $1 ORDER BY observed_at DESC, id DESC LIMIT 1
                ), lifecycle AS (
                    SELECT opportunity_id, min(submitted_at) AS first_submitted_at,
                           min(confirmed_at) AS first_confirmed_at
                    FROM loyal_yield.signed_route_submissions
                    WHERE cluster = $1 GROUP BY opportunity_id
                )
                SELECT jsonb_agg(to_jsonb(top_row)
                                 ORDER BY top_row.economic_priority DESC)
                FROM (
                    SELECT opportunity.id, opportunity.vault_id,
                           opportunity.source_reserve, opportunity.target_reserve,
                           opportunity.liquidity_mint,
                           opportunity.principal_usd_micros,
                           opportunity.annual_yield_gain_usd_micros,
                           opportunity.expected_net_gain_usd_micros,
                           opportunity.economic_priority,
                           opportunity.opportunity_state,
                           opportunity.terminal_reason,
                           extract(epoch FROM now() - opportunity.state_entered_at)::BIGINT
                               AS state_age_seconds,
                           lifecycle.first_submitted_at,
                           lifecycle.first_confirmed_at
                    FROM loyal_yield.rebalance_opportunities opportunity
                    JOIN latest_epoch ON latest_epoch.id = opportunity.optimizer_epoch_id
                    LEFT JOIN lifecycle ON lifecycle.opportunity_id = opportunity.id
                    ORDER BY opportunity.economic_priority DESC
                    LIMIT 50
                ) top_row
            ), '[]'::jsonb)
        )
        "#,
    )
    .bind(cluster)
    .bind(MATERIAL_PRINCIPAL_USD_MICROS)
    .fetch_one(pool)
    .await
}

fn metric_i64(value: &Value, key: &str) -> Option<i64> {
    value.get(key)?.as_i64()
}

fn queue_verdict(queue: &Value) -> Verdict {
    let Some(rows) = queue.get("statusRows").and_then(Value::as_array) else {
        return Verdict::Fail;
    };
    let Some(status) = rows.first() else {
        return Verdict::Fail;
    };
    let fresh_complete_epoch = metric_i64(status, "full_sweep_age_seconds")
        .is_some_and(|age| age <= MAX_FULL_SWEEP_AGE_SECONDS)
        && status.get("complete_frontier").and_then(Value::as_bool) == Some(true)
        && status
            .get("planner_epoch_matches_latest")
            .and_then(Value::as_bool)
            == Some(true)
        && status
            .get("latest_market_epoch_expired")
            .and_then(Value::as_bool)
            == Some(false)
        && metric_i64(status, "observed_vault_count").is_some_and(|count| count > 0);
    let stage_ages_bounded = [
        "oldest_waiting_alt_state_age_seconds",
        "oldest_ready_state_age_seconds",
        "oldest_sender_state_age_seconds",
        "oldest_confirmer_state_age_seconds",
        "oldest_reconciler_state_age_seconds",
    ]
    .into_iter()
    .all(|key| metric_i64(status, key).is_none_or(|age| age <= MAX_MATERIAL_STAGE_AGE_SECONDS));
    let counters_zero = metric_i64(status, "expired_lease_count") == Some(0)
        && metric_i64(status, "effect_ambiguous_count") == Some(0)
        && metric_i64(queue, "staleActiveDecisionCount") == Some(0)
        && metric_i64(queue, "duplicateActiveVaultMovementCount") == Some(0)
        && metric_i64(queue, "materialStuckOverTenMinutesCount") == Some(0)
        && metric_i64(queue, "targetCapacityOversubscriptionCount") == Some(0)
        && metric_i64(queue, "highValueOrderingInversionCount") == Some(0);
    if fresh_complete_epoch && stage_ages_bounded && counters_zero {
        Verdict::Pass
    } else {
        Verdict::Fail
    }
}

fn production_slo_measurements(queue: &Value) -> (Value, bool) {
    let status = queue
        .get("statusRows")
        .and_then(Value::as_array)
        .and_then(|rows| rows.first());
    let submission_p95 =
        status.and_then(|row| metric_i64(row, "current_epoch_submission_p95_milliseconds"));
    let confirmation_p95 =
        status.and_then(|row| metric_i64(row, "current_epoch_confirmation_p95_milliseconds"));
    let within_two_minutes =
        status.and_then(|row| metric_i64(row, "current_epoch_submitted_within_2m_yield_ppm"));
    let within_ten_minutes =
        status.and_then(|row| metric_i64(row, "current_epoch_submitted_within_10m_yield_ppm"));
    let opportunity_count =
        status.and_then(|row| metric_i64(row, "current_epoch_opportunity_count"));
    let pass = opportunity_count.is_some_and(|count| count > 0)
        && submission_p95.is_some_and(|millis| millis <= 10_000)
        && confirmation_p95.is_some_and(|millis| millis <= 30_000)
        && within_two_minutes.is_some_and(|ppm| ppm >= 900_000)
        && within_ten_minutes.is_some_and(|ppm| ppm >= 990_000);
    (
        json!({
            "currentEpochOpportunityCount": opportunity_count,
            "submissionP95Milliseconds": submission_p95,
            "submissionP95LimitMilliseconds": 10_000,
            "confirmationP95Milliseconds": confirmation_p95,
            "confirmationP95LimitMilliseconds": 30_000,
            "submittedWithinTwoMinutesYieldPpm": within_two_minutes,
            "submittedWithinTwoMinutesMinimumYieldPpm": 900_000,
            "submittedWithinTenMinutesYieldPpm": within_ten_minutes,
            "submittedWithinTenMinutesMinimumYieldPpm": 990_000,
            "pass": pass,
        }),
        pass,
    )
}

async fn load_movements(
    pool: &PgPool,
    cluster: &str,
    cutover_at: DateTime<Utc>,
) -> Result<Vec<MovementRow>, sqlx::Error> {
    let rows = sqlx::query(
        r#"
        SELECT submission.id AS submission_id,
               submission.opportunity_id, submission.decision_id,
               opportunity.decision_id AS opportunity_decision_id,
               opportunity.vault_id, decision.vault_id AS decision_vault_id,
               submission.transaction_signature, submission.submission_state,
               opportunity.opportunity_state,
               decision.status::text AS decision_status,
               COALESCE(opportunity.execution_plan->>'kind', '') AS route_kind,
               opportunity.optimizer_epoch_id AS opportunity_optimizer_epoch_id,
               submission.optimizer_epoch_id AS submission_optimizer_epoch_id,
               opportunity.source_snapshot_id AS opportunity_source_snapshot_id,
               submission.source_snapshot_id AS submission_source_snapshot_id,
               opportunity.source_reserve, opportunity.target_reserve,
               opportunity.liquidity_mint, opportunity.amount_raw,
               decision.source_snapshot_id AS decision_source_snapshot_id,
               decision.source_reserve AS decision_source_reserve,
               decision.signature AS decision_signature,
               decision.confirmed_slot AS decision_confirmed_slot,
               decision.post_snapshot_id AS decision_post_snapshot_id,
               decision.target_reserve AS decision_target_reserve,
               decision.liquidity_mint AS decision_liquidity_mint,
               decision.amount_raw AS decision_amount_raw,
               opportunity.principal_usd_micros,
               opportunity.estimated_edge_bps,
               opportunity.estimated_cost_lamports,
               opportunity.expected_net_gain_usd_micros,
               opportunity.economic_priority,
               submission.compiled_fee_lamports,
               opportunity.execution_plan,
               decision.execution_plan AS decision_execution_plan,
               submission.created_at, submission.submitted_slot,
               submission.submitted_at,
               submission.confirmed_at, submission.reconciled_at,
               submission.confirmed_slot, submission.reconciled_slot,
               submission.broadcast_count, submission.last_broadcast_at,
               submission.last_valid_block_height,
               submission.expiry_observed_block_height,
               submission.effect_check_slot,
               submission.last_status_checked_at,
               source_snapshot.id AS source_snapshot_id,
               source_snapshot.vault_id AS source_snapshot_vault_id,
               source_snapshot.context AS source_snapshot_context,
               post_snapshot.id AS post_snapshot_id,
               post_snapshot.vault_id AS post_snapshot_vault_id,
               post_snapshot.context AS post_snapshot_context,
               CASE
                   WHEN opportunity.execution_plan->>'kind' = 'same_mint'
                       THEN source_snapshot.id
                   WHEN opportunity.execution_plan->>'kind' = 'idle_vault_deposit'
                       THEN idle_pre_target.snapshot_id
                   ELSE NULL
               END AS pre_target_snapshot_id,
               CASE
                   WHEN opportunity.execution_plan->>'kind' = 'same_mint'
                       THEN source_snapshot.vault_id
                   WHEN opportunity.execution_plan->>'kind' = 'idle_vault_deposit'
                       THEN idle_pre_target.snapshot_vault_id
                   ELSE NULL
               END AS pre_target_snapshot_vault_id,
               CASE
                   WHEN opportunity.execution_plan->>'kind' = 'same_mint'
                       THEN source_snapshot.context
                   WHEN opportunity.execution_plan->>'kind' = 'idle_vault_deposit'
                       THEN idle_pre_target.snapshot_context
                   ELSE NULL
               END AS pre_target_snapshot_context,
               post_snapshot.observed_slot AS post_snapshot_observed_slot,
               post_snapshot.observed_at AS post_snapshot_observed_at,
               CASE
                   WHEN opportunity.execution_plan->>'kind' = 'same_mint'
                       THEN source_snapshot.observed_slot
                   WHEN opportunity.execution_plan->>'kind' = 'idle_vault_deposit'
                       THEN idle_pre_target.observed_slot
                   ELSE NULL
               END AS pre_target_snapshot_observed_slot,
               CASE
                   WHEN opportunity.execution_plan->>'kind' = 'same_mint'
                       THEN source_snapshot.observed_at
                   WHEN opportunity.execution_plan->>'kind' = 'idle_vault_deposit'
                       THEN idle_pre_target.observed_at
                   ELSE NULL
               END AS pre_target_snapshot_observed_at,
               pre_source.amount_raw AS pre_source_amount_raw,
               post_source.amount_raw AS post_source_amount_raw,
               CASE
                   WHEN opportunity.execution_plan->>'kind' = 'same_mint'
                       THEN pre_target.liquidity_mint
                   WHEN opportunity.execution_plan->>'kind' = 'idle_vault_deposit'
                       THEN idle_pre_target.liquidity_mint
                   ELSE NULL
               END AS pre_target_liquidity_mint,
               CASE
                   WHEN opportunity.execution_plan->>'kind' = 'same_mint'
                       THEN pre_target.has_value
                   WHEN opportunity.execution_plan->>'kind' = 'idle_vault_deposit'
                       THEN idle_pre_target.has_value
                   ELSE NULL
               END AS pre_target_has_value,
               CASE
                   WHEN opportunity.execution_plan->>'kind' = 'same_mint'
                       THEN pre_target.amount_raw
                   WHEN opportunity.execution_plan->>'kind' = 'idle_vault_deposit'
                       THEN idle_pre_target.amount_raw
                   ELSE NULL
               END AS pre_target_amount_raw,
               CASE
                   WHEN opportunity.execution_plan->>'kind' = 'same_mint'
                       THEN pre_target.planning_metadata
                   WHEN opportunity.execution_plan->>'kind' = 'idle_vault_deposit'
                       THEN idle_pre_target.planning_metadata
                   ELSE NULL
               END AS pre_target_planning_metadata,
               post_target.liquidity_mint AS post_target_liquidity_mint,
               post_target.has_value AS post_target_has_value,
               post_target.amount_raw AS post_target_amount_raw,
               post_target.planning_metadata AS post_target_planning_metadata
        FROM loyal_yield.signed_route_submissions submission
        JOIN loyal_yield.rebalance_opportunities opportunity
          ON opportunity.id = submission.opportunity_id
        JOIN loyal_yield.rebalance_decisions decision
          ON decision.id = submission.decision_id
        LEFT JOIN loyal_yield.vault_position_snapshots post_snapshot
          ON post_snapshot.id = decision.post_snapshot_id
        LEFT JOIN loyal_yield.vault_position_snapshots source_snapshot
          ON source_snapshot.id = decision.source_snapshot_id
        LEFT JOIN LATERAL (
            SELECT snapshot.id AS snapshot_id,
                   snapshot.vault_id AS snapshot_vault_id,
                   snapshot.context AS snapshot_context,
                   snapshot.observed_slot, snapshot.observed_at,
                   target.liquidity_mint, target.amount_raw, target.has_value,
                   target.planning_metadata
            FROM loyal_yield.vault_position_snapshots snapshot
            JOIN loyal_yield.vault_position_snapshot_positions target
              ON target.snapshot_id = snapshot.id
             AND target.reserve = opportunity.target_reserve
            WHERE opportunity.execution_plan->>'kind' = 'idle_vault_deposit'
              AND snapshot.vault_id = opportunity.vault_id
              AND snapshot.observed_at <= submission.created_at
              AND (
                  submission.confirmed_slot IS NULL
                  OR snapshot.observed_slot <= submission.confirmed_slot
              )
              AND target.liquidity_mint = opportunity.liquidity_mint
            ORDER BY snapshot.observed_slot DESC, snapshot.id DESC
            LIMIT 1
        ) idle_pre_target ON TRUE
        LEFT JOIN loyal_yield.vault_position_snapshot_positions pre_source
          ON pre_source.snapshot_id = decision.source_snapshot_id
         AND pre_source.reserve = decision.source_reserve
        LEFT JOIN loyal_yield.vault_position_snapshot_positions post_source
          ON post_source.snapshot_id = decision.post_snapshot_id
         AND post_source.reserve = decision.source_reserve
        LEFT JOIN loyal_yield.vault_position_snapshot_positions pre_target
          ON pre_target.snapshot_id = decision.source_snapshot_id
         AND pre_target.reserve = decision.target_reserve
        LEFT JOIN loyal_yield.vault_position_snapshot_positions post_target
          ON post_target.snapshot_id = decision.post_snapshot_id
         AND post_target.reserve = decision.target_reserve
        WHERE submission.cluster = $1 AND submission.created_at >= $2
        ORDER BY submission.created_at, submission.id
        "#,
    )
    .bind(cluster)
    .bind(cutover_at)
    .fetch_all(pool)
    .await?;
    rows.into_iter()
        .map(|row| {
            Ok(MovementRow {
                submission_id: row.try_get("submission_id")?,
                opportunity_id: row.try_get("opportunity_id")?,
                decision_id: row.try_get("decision_id")?,
                opportunity_decision_id: row.try_get("opportunity_decision_id")?,
                vault_id: row.try_get("vault_id")?,
                decision_vault_id: row.try_get("decision_vault_id")?,
                signature: row.try_get("transaction_signature")?,
                submission_state: row.try_get("submission_state")?,
                opportunity_state: row.try_get("opportunity_state")?,
                decision_status: row.try_get("decision_status")?,
                route_kind: row.try_get("route_kind")?,
                opportunity_optimizer_epoch_id: row.try_get("opportunity_optimizer_epoch_id")?,
                submission_optimizer_epoch_id: row.try_get("submission_optimizer_epoch_id")?,
                opportunity_source_snapshot_id: row.try_get("opportunity_source_snapshot_id")?,
                submission_source_snapshot_id: row.try_get("submission_source_snapshot_id")?,
                source_reserve: row.try_get("source_reserve")?,
                target_reserve: row.try_get("target_reserve")?,
                liquidity_mint: row.try_get("liquidity_mint")?,
                amount_raw: row.try_get("amount_raw")?,
                decision_source_snapshot_id: row.try_get("decision_source_snapshot_id")?,
                decision_source_reserve: row.try_get("decision_source_reserve")?,
                decision_signature: row.try_get("decision_signature")?,
                decision_confirmed_slot: row.try_get("decision_confirmed_slot")?,
                decision_post_snapshot_id: row.try_get("decision_post_snapshot_id")?,
                decision_target_reserve: row.try_get("decision_target_reserve")?,
                decision_liquidity_mint: row.try_get("decision_liquidity_mint")?,
                decision_amount_raw: row.try_get("decision_amount_raw")?,
                principal_usd_micros: row.try_get("principal_usd_micros")?,
                estimated_edge_bps: row.try_get("estimated_edge_bps")?,
                estimated_cost_lamports: row.try_get("estimated_cost_lamports")?,
                expected_net_gain_usd_micros: row.try_get("expected_net_gain_usd_micros")?,
                economic_priority: row.try_get("economic_priority")?,
                compiled_fee_lamports: row.try_get("compiled_fee_lamports")?,
                execution_plan: row.try_get("execution_plan")?,
                decision_execution_plan: row.try_get("decision_execution_plan")?,
                created_at: row.try_get("created_at")?,
                submitted_slot: row.try_get("submitted_slot")?,
                submitted_at: row.try_get("submitted_at")?,
                confirmed_at: row.try_get("confirmed_at")?,
                reconciled_at: row.try_get("reconciled_at")?,
                confirmed_slot: row.try_get("confirmed_slot")?,
                reconciled_slot: row.try_get("reconciled_slot")?,
                broadcast_count: row.try_get("broadcast_count")?,
                last_broadcast_at: row.try_get("last_broadcast_at")?,
                last_valid_block_height: row.try_get("last_valid_block_height")?,
                expiry_observed_block_height: row.try_get("expiry_observed_block_height")?,
                effect_check_slot: row.try_get("effect_check_slot")?,
                last_status_checked_at: row.try_get("last_status_checked_at")?,
                source_snapshot_id: row.try_get("source_snapshot_id")?,
                source_snapshot_vault_id: row.try_get("source_snapshot_vault_id")?,
                source_snapshot_context: row.try_get("source_snapshot_context")?,
                post_snapshot_id: row.try_get("post_snapshot_id")?,
                post_snapshot_vault_id: row.try_get("post_snapshot_vault_id")?,
                post_snapshot_context: row.try_get("post_snapshot_context")?,
                pre_target_snapshot_id: row.try_get("pre_target_snapshot_id")?,
                pre_target_snapshot_vault_id: row.try_get("pre_target_snapshot_vault_id")?,
                pre_target_snapshot_context: row.try_get("pre_target_snapshot_context")?,
                pre_target_planning_metadata: row.try_get("pre_target_planning_metadata")?,
                post_snapshot_observed_slot: row.try_get("post_snapshot_observed_slot")?,
                post_snapshot_observed_at: row.try_get("post_snapshot_observed_at")?,
                pre_target_snapshot_observed_slot: row
                    .try_get("pre_target_snapshot_observed_slot")?,
                pre_target_snapshot_observed_at: row.try_get("pre_target_snapshot_observed_at")?,
                pre_source_amount_raw: row.try_get("pre_source_amount_raw")?,
                post_source_amount_raw: row.try_get("post_source_amount_raw")?,
                pre_target_liquidity_mint: row.try_get("pre_target_liquidity_mint")?,
                pre_target_has_value: row.try_get("pre_target_has_value")?,
                pre_target_amount_raw: row.try_get("pre_target_amount_raw")?,
                post_target_liquidity_mint: row.try_get("post_target_liquidity_mint")?,
                post_target_has_value: row.try_get("post_target_has_value")?,
                post_target_amount_raw: row.try_get("post_target_amount_raw")?,
                post_target_planning_metadata: row.try_get("post_target_planning_metadata")?,
            })
        })
        .collect()
}

async fn finalized_signatures(
    rpc_url: Option<&str>,
    movements: &[MovementRow],
) -> Result<FinalityEvidence, ()> {
    let rpc_url = rpc_url.filter(|value| !value.trim().is_empty()).ok_or(())?;
    let client = Client::new();
    let mut finality = BTreeMap::new();
    for (batch_index, batch) in movements.chunks(256).enumerate() {
        let signatures = batch
            .iter()
            .map(|movement| movement.signature.clone())
            .collect::<Vec<_>>();
        let response = client
            .post(rpc_url)
            .json(&json!({
                "jsonrpc": "2.0",
                "id": batch_index + 1,
                "method": "getSignatureStatuses",
                "params": [signatures, {"searchTransactionHistory": true}],
            }))
            .send()
            .await
            .map_err(|_| ())?;
        if !response.status().is_success() {
            return Err(());
        }
        let value = response.json::<Value>().await.map_err(|_| ())?;
        let statuses = value
            .pointer("/result/value")
            .and_then(Value::as_array)
            .ok_or(())?;
        if statuses.len() != batch.len() {
            return Err(());
        }
        for (movement, status) in batch.iter().zip(statuses) {
            finality.insert(
                movement.signature.clone(),
                SignatureFinality {
                    found: !status.is_null(),
                    finalized: status.get("confirmationStatus").and_then(Value::as_str)
                        == Some("finalized"),
                    successful: !status.is_null() && status.get("err").is_some_and(Value::is_null),
                    slot: status.get("slot").and_then(Value::as_i64),
                },
            );
        }
    }
    let response = client
        .post(rpc_url)
        .json(&json!({
            "jsonrpc": "2.0",
            "id": "finalized-block-height",
            "method": "getBlockHeight",
            "params": [{"commitment": "finalized"}],
        }))
        .send()
        .await
        .map_err(|_| ())?;
    if !response.status().is_success() {
        return Err(());
    }
    let finalized_block_height = response
        .json::<Value>()
        .await
        .map_err(|_| ())?
        .pointer("/result")
        .and_then(Value::as_i64)
        .ok_or(())?;
    let response = client
        .post(rpc_url)
        .json(&json!({
            "jsonrpc": "2.0",
            "id": "finalized-slot",
            "method": "getSlot",
            "params": [{"commitment": "finalized"}],
        }))
        .send()
        .await
        .map_err(|_| ())?;
    if !response.status().is_success() {
        return Err(());
    }
    let finalized_slot = response
        .json::<Value>()
        .await
        .map_err(|_| ())?
        .pointer("/result")
        .and_then(Value::as_i64)
        .ok_or(())?;
    Ok(FinalityEvidence {
        statuses: finality,
        finalized_block_height,
        finalized_slot,
    })
}

fn execution_i64(plan: &Value, key: &str) -> Option<i64> {
    plan.get(key).and_then(|value| {
        value
            .as_i64()
            .or_else(|| value.as_str().and_then(|value| value.parse().ok()))
    })
}

fn execution_string<'a>(plan: &'a Value, key: &str) -> Option<&'a str> {
    plan.get(key).and_then(Value::as_str)
}

fn execution_timestamp(plan: &Value, key: &str) -> Option<DateTime<Utc>> {
    execution_string(plan, key)
        .and_then(|value| DateTime::parse_from_rfc3339(value).ok())
        .map(|value| value.with_timezone(&Utc))
}

fn chain_snapshot_context(context: Option<&Value>) -> bool {
    matches!(
        context
            .and_then(|context| context.get("kind"))
            .and_then(Value::as_str),
        Some("fleet_position_sweep" | "same_mint_chain_reconcile_preview")
    )
}

fn chain_position_metadata(metadata: Option<&Value>) -> bool {
    metadata
        .and_then(|metadata| metadata.get("source"))
        .and_then(Value::as_str)
        == Some("chain_reconcile_preview")
}

fn movement_json(
    movement: &MovementRow,
    finality: Option<&SignatureFinality>,
    finalized_block_height: Option<i64>,
    finalized_slot: Option<i64>,
) -> (Value, bool, bool, bool) {
    let rpc_slot = finality.and_then(|status| status.slot);
    let source_snapshot_chain_proven =
        chain_snapshot_context(movement.source_snapshot_context.as_ref());
    let pre_target_snapshot_chain_proven =
        chain_snapshot_context(movement.pre_target_snapshot_context.as_ref())
            && chain_position_metadata(movement.pre_target_planning_metadata.as_ref());
    let post_snapshot_chain_proven =
        chain_snapshot_context(movement.post_snapshot_context.as_ref())
            && chain_position_metadata(movement.post_target_planning_metadata.as_ref());
    let post_slot_fenced = rpc_slot
        .zip(movement.post_snapshot_observed_slot)
        .is_some_and(|(rpc, observed)| observed >= rpc);
    let target_increased = movement.post_target_amount_raw.unwrap_or_default()
        > movement.pre_target_amount_raw.unwrap_or_default();
    let reserve_effect = movement.route_kind == "same_mint"
        && movement.source_reserve.is_some()
        && source_snapshot_chain_proven
        && pre_target_snapshot_chain_proven
        && post_snapshot_chain_proven
        && movement
            .pre_source_amount_raw
            .zip(movement.post_source_amount_raw)
            .is_some_and(|(before, after)| after < before)
        && target_increased;
    let decision_route_kind =
        execution_string(&movement.decision_execution_plan, "kind").unwrap_or_default();
    let decision_source_kind = execution_string(&movement.decision_execution_plan, "source_kind");
    let planner_source_kind = execution_string(&movement.execution_plan, "source_kind");
    let same_mint_plan_identity_exact = decision_route_kind == "same_mint"
        && execution_string(&movement.execution_plan, "kind") == Some("same_mint")
        && execution_string(&movement.decision_execution_plan, "source_reserve")
            == movement.decision_source_reserve.as_deref()
        && execution_string(&movement.execution_plan, "source_reserve")
            == movement.source_reserve.as_deref()
        && execution_string(&movement.decision_execution_plan, "target_reserve")
            == Some(movement.target_reserve.as_str())
        && execution_string(&movement.execution_plan, "target_reserve")
            == Some(movement.target_reserve.as_str())
        && execution_string(&movement.decision_execution_plan, "liquidity_mint")
            == Some(movement.liquidity_mint.as_str())
        && execution_string(&movement.execution_plan, "liquidity_mint")
            == Some(movement.liquidity_mint.as_str())
        && execution_i64(&movement.decision_execution_plan, "amount_raw")
            == Some(movement.amount_raw)
        && execution_i64(&movement.execution_plan, "amount_raw") == Some(movement.amount_raw);
    let idle_token_account =
        execution_string(&movement.decision_execution_plan, "idle_token_account");
    let planner_idle_token_account =
        execution_string(&movement.execution_plan, "idle_token_account");
    let pre_idle_source_amount_raw = execution_i64(
        &movement.decision_execution_plan,
        "idle_vault_liquidity_amount_raw",
    );
    let planner_pre_idle_source_amount_raw =
        execution_i64(&movement.execution_plan, "idle_vault_liquidity_amount_raw");
    let pre_idle_source_observed_slot =
        execution_i64(&movement.decision_execution_plan, "idle_observed_slot")
            .or_else(|| execution_i64(&movement.decision_execution_plan, "observed_slot"));
    let planner_pre_idle_source_observed_slot =
        execution_i64(&movement.execution_plan, "source_observed_slot")
            .or_else(|| execution_i64(&movement.execution_plan, "idle_observed_slot"));
    let pre_idle_source_observed_at =
        execution_timestamp(&movement.decision_execution_plan, "idle_observed_at")
            .or_else(|| execution_timestamp(&movement.decision_execution_plan, "observed_at"));
    let planner_pre_idle_source_observed_at =
        execution_timestamp(&movement.execution_plan, "source_observed_at")
            .or_else(|| execution_timestamp(&movement.execution_plan, "idle_observed_at"));
    let post_target_metadata = movement
        .post_target_planning_metadata
        .as_ref()
        .unwrap_or(&Value::Null);
    let post_idle_source_token_account =
        execution_string(post_target_metadata, "vault_liquidity_ata");
    let post_idle_source_amount_raw =
        execution_i64(post_target_metadata, "idle_vault_liquidity_amount_raw")
            .or_else(|| execution_i64(post_target_metadata, "vault_liquidity_amount_raw"));
    let idle_plan_identity_exact = decision_route_kind == "idle_vault_deposit"
        && execution_string(&movement.execution_plan, "kind") == Some("idle_vault_deposit")
        && decision_source_kind == Some("idle_vault")
        && planner_source_kind == Some("idle_vault_usdc")
        && movement.source_reserve.is_none()
        && movement.opportunity_source_snapshot_id.is_none()
        && movement.decision_source_reserve.is_none()
        && movement.decision_source_snapshot_id.is_none()
        && movement.decision_target_reserve == movement.target_reserve
        && movement.decision_liquidity_mint == movement.liquidity_mint
        && movement.decision_amount_raw == movement.amount_raw
        && movement
            .decision_execution_plan
            .get("source_reserve")
            .is_some_and(Value::is_null)
        && movement
            .execution_plan
            .get("source_reserve")
            .is_some_and(Value::is_null)
        && execution_string(&movement.decision_execution_plan, "target_reserve")
            == Some(movement.target_reserve.as_str())
        && execution_string(&movement.decision_execution_plan, "liquidity_mint")
            == Some(movement.liquidity_mint.as_str())
        && execution_i64(&movement.decision_execution_plan, "amount_raw")
            == Some(movement.amount_raw)
        && execution_string(&movement.execution_plan, "target_reserve")
            == Some(movement.target_reserve.as_str())
        && execution_string(&movement.execution_plan, "liquidity_mint")
            == Some(movement.liquidity_mint.as_str())
        && execution_i64(&movement.execution_plan, "amount_raw") == Some(movement.amount_raw);
    let idle_plan_evidence_exact = idle_plan_identity_exact
        && idle_token_account == planner_idle_token_account
        && pre_idle_source_amount_raw == planner_pre_idle_source_amount_raw
        && pre_idle_source_observed_slot == planner_pre_idle_source_observed_slot
        && pre_idle_source_observed_at == planner_pre_idle_source_observed_at;
    let reciprocal_identity_exact = movement.opportunity_decision_id == Some(movement.decision_id)
        && movement.decision_vault_id == movement.vault_id
        && movement.opportunity_optimizer_epoch_id == movement.submission_optimizer_epoch_id;
    let snapshot_identity_exact = match movement.route_kind.as_str() {
        "same_mint" => {
            movement
                .opportunity_source_snapshot_id
                .is_some_and(|id| id > 0)
                && movement.decision_source_snapshot_id == movement.opportunity_source_snapshot_id
                && movement.submission_source_snapshot_id == movement.opportunity_source_snapshot_id
                && movement.source_snapshot_id == movement.opportunity_source_snapshot_id
                && movement.source_snapshot_vault_id == Some(movement.vault_id)
                && source_snapshot_chain_proven
                && movement.pre_target_snapshot_id == movement.opportunity_source_snapshot_id
                && movement.pre_target_snapshot_vault_id == Some(movement.vault_id)
                && pre_target_snapshot_chain_proven
        }
        "idle_vault_deposit" => {
            movement.opportunity_source_snapshot_id.is_none()
                && movement.decision_source_snapshot_id.is_none()
                && movement.submission_source_snapshot_id.is_none()
                && movement.source_snapshot_id.is_none()
                && movement.source_snapshot_vault_id.is_none()
                && movement.pre_target_snapshot_id.is_some_and(|id| id > 0)
                && movement.pre_target_snapshot_vault_id == Some(movement.vault_id)
                && pre_target_snapshot_chain_proven
        }
        _ => false,
    };
    let route_plan_identity_exact = match movement.route_kind.as_str() {
        "same_mint" => {
            movement.source_reserve.is_some()
                && movement.source_reserve.as_deref() != Some(movement.target_reserve.as_str())
                && movement.decision_source_reserve == movement.source_reserve
                && same_mint_plan_identity_exact
        }
        "idle_vault_deposit" => idle_plan_identity_exact,
        _ => false,
    };
    let route_identity_exact = reciprocal_identity_exact
        && snapshot_identity_exact
        && route_plan_identity_exact
        && movement.decision_target_reserve == movement.target_reserve
        && movement.decision_liquidity_mint == movement.liquidity_mint
        && movement.decision_amount_raw == movement.amount_raw;
    let idle_source_decreased = pre_idle_source_amount_raw
        .zip(post_idle_source_amount_raw)
        .is_some_and(|(before, after)| {
            before == movement.amount_raw
                && after >= 0
                && after <= before.saturating_sub(movement.amount_raw)
        });
    let idle_effect = movement.route_kind == "idle_vault_deposit"
        && movement.source_reserve.is_none()
        && idle_plan_evidence_exact
        && post_snapshot_chain_proven
        && idle_token_account.is_some()
        && idle_token_account == post_idle_source_token_account
        && pre_idle_source_observed_slot
            .zip(rpc_slot)
            .is_some_and(|(observed, rpc)| observed > 0 && observed <= rpc)
        && pre_idle_source_observed_at.is_some_and(|observed| observed <= movement.created_at)
        && movement
            .pre_target_snapshot_observed_slot
            .zip(rpc_slot)
            .is_some_and(|(observed, rpc)| observed > 0 && observed <= rpc)
        && movement
            .pre_target_snapshot_observed_at
            .is_some_and(|observed| observed <= movement.created_at)
        && movement
            .post_snapshot_observed_slot
            .zip(rpc_slot)
            .is_some_and(|(observed, rpc)| observed >= rpc)
        && movement.post_snapshot_observed_at.is_some()
        && idle_source_decreased
        && movement.pre_target_liquidity_mint.as_deref() == Some(movement.liquidity_mint.as_str())
        && movement.post_target_liquidity_mint.as_deref() == Some(movement.liquidity_mint.as_str())
        && movement.pre_target_amount_raw.is_some()
        && movement.post_target_amount_raw.is_some()
        && movement.pre_target_has_value == movement.pre_target_amount_raw.map(|amount| amount > 0)
        && movement.post_target_has_value == Some(true)
        && target_increased;
    let route_effect = match movement.route_kind.as_str() {
        "same_mint" => reserve_effect,
        "idle_vault_deposit" => idle_effect,
        _ => false,
    };
    let finalized_success = finality.is_some_and(|status| {
        status.found
            && status.finalized
            && status.successful
            && status
                .slot
                .zip(movement.confirmed_slot)
                .is_some_and(|(rpc_slot, confirmed_slot)| rpc_slot == confirmed_slot)
    });
    let conservative_sol_price = execution_i64(
        &movement.execution_plan,
        "conservative_sol_price_usd_micros",
    )
    .unwrap_or_default();
    let fee_fraction_cap =
        execution_i64(&movement.execution_plan, "fee_gain_fraction_ppm").unwrap_or_default();
    let fee_usd_micros = (i128::from(movement.compiled_fee_lamports)
        * i128::from(conservative_sol_price)
        / 1_000_000_000)
        .clamp(i128::from(i64::MIN), i128::from(i64::MAX)) as i64;
    let fee_fraction_ppm = if movement.expected_net_gain_usd_micros > 0 {
        Some(
            (i128::from(fee_usd_micros) * 1_000_000
                / i128::from(movement.expected_net_gain_usd_micros))
            .clamp(i128::from(i64::MIN), i128::from(i64::MAX)) as i64,
        )
    } else {
        None
    };
    let economic_pass = movement.estimated_edge_bps > 0
        && movement.expected_net_gain_usd_micros > 0
        && movement.compiled_fee_lamports <= movement.estimated_cost_lamports
        && fee_fraction_cap > 0
        && fee_fraction_ppm.is_some_and(|fraction| fraction <= fee_fraction_cap);
    let reconciled_lifecycle_ordered = movement
        .submitted_at
        .zip(movement.confirmed_at)
        .zip(movement.reconciled_at)
        .is_some_and(|((submitted, confirmed), reconciled)| {
            movement.created_at <= submitted && submitted <= confirmed && confirmed <= reconciled
        });
    let reconciled_slots_ordered = movement
        .submitted_slot
        .zip(rpc_slot)
        .zip(movement.reconciled_slot)
        .is_some_and(|((submitted, rpc), reconciled)| {
            submitted > 0 && submitted <= rpc && reconciled >= rpc
        });
    let post_snapshot_identity_exact = movement.post_snapshot_id.is_some_and(|id| id > 0)
        && movement.post_snapshot_vault_id == Some(movement.vault_id)
        && movement.decision_post_snapshot_id == movement.post_snapshot_id
        && post_snapshot_chain_proven;
    let post_snapshot_time_ordered = movement
        .post_snapshot_observed_at
        .zip(movement.reconciled_at)
        .is_some_and(|(observed, reconciled)| {
            movement.created_at <= observed && observed <= reconciled
        });
    let broadcast_evidence_ordered = movement.broadcast_count > 0
        && movement
            .last_broadcast_at
            .zip(movement.submitted_at)
            .is_some_and(|(broadcast, submitted)| {
                movement.created_at <= broadcast && broadcast <= submitted
            });
    let effect_pass = movement.submission_state == "reconciled"
        && movement.opportunity_state == "completed"
        && movement.decision_status == "confirmed"
        && movement.decision_signature.as_deref() == Some(movement.signature.as_str())
        && movement.decision_confirmed_slot == movement.confirmed_slot
        && route_identity_exact
        && post_snapshot_identity_exact
        && reconciled_lifecycle_ordered
        && reconciled_slots_ordered
        && post_snapshot_time_ordered
        && broadcast_evidence_ordered
        && movement.reconciled_at.is_some()
        && movement
            .reconciled_slot
            .zip(rpc_slot)
            .is_some_and(|(reconciled, rpc)| reconciled >= rpc)
        && post_slot_fenced
        && route_effect;
    let failed_lifecycle_ordered = movement
        .submitted_at
        .zip(movement.confirmed_at)
        .zip(movement.last_status_checked_at)
        .is_some_and(|((submitted, confirmed), checked)| {
            movement.created_at <= submitted && submitted <= confirmed && confirmed <= checked
        });
    let failed_terminal_safe = movement.opportunity_state == "failed"
        && movement.decision_status == "failed"
        && movement.decision_signature.as_deref() == Some(movement.signature.as_str())
        && route_identity_exact
        && broadcast_evidence_ordered
        && failed_lifecycle_ordered
        && movement
            .submitted_slot
            .zip(rpc_slot)
            .is_some_and(|(submitted, rpc)| submitted > 0 && submitted <= rpc)
        && finality.is_some_and(|status| {
            status.found
                && status.finalized
                && !status.successful
                && status.slot == movement.confirmed_slot
        });
    let expired_lifecycle_ordered = movement.last_status_checked_at.is_some_and(|checked| {
        movement.created_at <= checked
            && movement
                .last_broadcast_at
                .is_none_or(|broadcast| movement.created_at <= broadcast && broadcast <= checked)
    });
    let expired_height_proven = finalized_block_height
        .is_some_and(|height| height > movement.last_valid_block_height)
        && match movement.broadcast_count {
            0 => {
                movement.last_broadcast_at.is_none()
                    && movement.expiry_observed_block_height.is_none()
                    && movement.effect_check_slot.is_none()
            }
            count if count > 0 => {
                let pre_route_slot = movement
                    .pre_target_snapshot_observed_slot
                    .into_iter()
                    .chain(
                        execution_i64(&movement.decision_execution_plan, "idle_observed_slot")
                            .or_else(|| {
                                execution_i64(&movement.decision_execution_plan, "observed_slot")
                            }),
                    )
                    .max()
                    .unwrap_or_default();
                movement.last_broadcast_at.is_some()
                    && movement.expiry_observed_block_height.is_some_and(|height| {
                        height > movement.last_valid_block_height
                            && finalized_block_height.is_some_and(|current| height <= current)
                    })
                    && movement.effect_check_slot.is_some_and(|slot| {
                        slot > pre_route_slot
                            && finalized_slot.is_some_and(|current| slot <= current)
                    })
            }
            _ => false,
        };
    let expired_terminal_safe = movement.opportunity_state == "failed"
        && movement.decision_status == "failed"
        && route_identity_exact
        && expired_lifecycle_ordered
        && expired_height_proven
        && finality.is_some_and(|status| !status.found);
    let terminal_outcome_safe = match movement.submission_state.as_str() {
        "reconciled" => effect_pass && finalized_success,
        "failed" => failed_terminal_safe,
        "expired" => expired_terminal_safe,
        _ => false,
    };
    (
        json!({
            "submissionId": movement.submission_id,
            "opportunityId": movement.opportunity_id,
            "decisionId": movement.decision_id,
            "opportunityDecisionId": movement.opportunity_decision_id,
            "vaultId": movement.vault_id,
            "decisionVaultId": movement.decision_vault_id,
            "signature": movement.signature,
            "submissionState": movement.submission_state,
            "opportunityState": movement.opportunity_state,
            "decisionStatus": movement.decision_status,
            "routeKind": movement.route_kind,
            "decisionRouteKind": decision_route_kind,
            "decisionSourceKind": decision_source_kind,
            "plannerSourceKind": planner_source_kind,
            "opportunityOptimizerEpochId": movement.opportunity_optimizer_epoch_id,
            "submissionOptimizerEpochId": movement.submission_optimizer_epoch_id,
            "opportunitySourceSnapshotId": movement.opportunity_source_snapshot_id,
            "submissionSourceSnapshotId": movement.submission_source_snapshot_id,
            "sourceReserve": movement.source_reserve,
            "decisionSourceSnapshotId": movement.decision_source_snapshot_id,
            "decisionSourceReserve": movement.decision_source_reserve,
            "decisionSignature": movement.decision_signature,
            "decisionConfirmedSlot": movement.decision_confirmed_slot,
            "decisionPostSnapshotId": movement.decision_post_snapshot_id,
            "targetReserve": movement.target_reserve,
            "decisionTargetReserve": movement.decision_target_reserve,
            "liquidityMint": movement.liquidity_mint,
            "decisionLiquidityMint": movement.decision_liquidity_mint,
            "amountRaw": movement.amount_raw,
            "decisionAmountRaw": movement.decision_amount_raw,
            "plannerExecutionPlan": movement.execution_plan,
            "decisionExecutionPlan": movement.decision_execution_plan,
            "routeIdentityExact": route_identity_exact,
            "principalUsdMicros": movement.principal_usd_micros,
            "estimatedEdgeBps": movement.estimated_edge_bps,
            "expectedNetGainUsdMicros": movement.expected_net_gain_usd_micros,
            "economicPriority": movement.economic_priority,
            "estimatedCostLamports": movement.estimated_cost_lamports,
            "compiledFeeLamports": movement.compiled_fee_lamports,
            "conservativeSolPriceUsdMicros": conservative_sol_price,
            "compiledFeeUsdMicros": fee_usd_micros,
            "feeFractionPpm": fee_fraction_ppm,
            "feeFractionCapPpm": fee_fraction_cap,
            "economicPass": economic_pass,
            "createdAt": movement.created_at,
            "submittedSlot": movement.submitted_slot,
            "submittedAt": movement.submitted_at,
            "confirmedAt": movement.confirmed_at,
            "reconciledAt": movement.reconciled_at,
            "confirmedSlot": movement.confirmed_slot,
            "reconciledSlot": movement.reconciled_slot,
            "broadcastCount": movement.broadcast_count,
            "lastBroadcastAt": movement.last_broadcast_at,
            "lastValidBlockHeight": movement.last_valid_block_height,
            "expiryObservedBlockHeight": movement.expiry_observed_block_height,
            "effectCheckSlot": movement.effect_check_slot,
            "lastStatusCheckedAt": movement.last_status_checked_at,
            "sourceSnapshotId": movement.source_snapshot_id,
            "sourceSnapshotVaultId": movement.source_snapshot_vault_id,
            "sourceSnapshotContext": movement.source_snapshot_context,
            "postSnapshotId": movement.post_snapshot_id,
            "postSnapshotVaultId": movement.post_snapshot_vault_id,
            "postSnapshotContext": movement.post_snapshot_context,
            "preTargetSnapshotId": movement.pre_target_snapshot_id,
            "preTargetSnapshotVaultId": movement.pre_target_snapshot_vault_id,
            "preTargetSnapshotContext": movement.pre_target_snapshot_context,
            "preTargetPlanningMetadata": movement.pre_target_planning_metadata,
            "postSnapshotObservedSlot": movement.post_snapshot_observed_slot,
            "postSnapshotObservedAt": movement.post_snapshot_observed_at,
            "postSnapshotAtOrAboveConfirmation": post_slot_fenced,
            "preTargetSnapshotObservedSlot": movement.pre_target_snapshot_observed_slot,
            "preTargetSnapshotObservedAt": movement.pre_target_snapshot_observed_at,
            "preSourceAmountRaw": movement.pre_source_amount_raw,
            "postSourceAmountRaw": movement.post_source_amount_raw,
            "preTargetLiquidityMint": movement.pre_target_liquidity_mint,
            "preTargetHasValue": movement.pre_target_has_value,
            "preTargetAmountRaw": movement.pre_target_amount_raw,
            "postTargetLiquidityMint": movement.post_target_liquidity_mint,
            "postTargetHasValue": movement.post_target_has_value,
            "postTargetAmountRaw": movement.post_target_amount_raw,
            "postTargetPlanningMetadata": movement.post_target_planning_metadata,
            "sourceDecreasedAndTargetIncreased": reserve_effect,
            "idleTokenAccount": idle_token_account,
            "plannerIdleTokenAccount": planner_idle_token_account,
            "preIdleSourceAmountRaw": pre_idle_source_amount_raw,
            "plannerPreIdleSourceAmountRaw": planner_pre_idle_source_amount_raw,
            "preIdleSourceObservedSlot": pre_idle_source_observed_slot,
            "plannerPreIdleSourceObservedSlot": planner_pre_idle_source_observed_slot,
            "preIdleSourceObservedAt": pre_idle_source_observed_at,
            "plannerPreIdleSourceObservedAt": planner_pre_idle_source_observed_at,
            "idlePlanIdentityExact": idle_plan_identity_exact,
            "idlePlanEvidenceExact": idle_plan_evidence_exact,
            "postIdleSourceTokenAccount": post_idle_source_token_account,
            "postIdleSourceAmountRaw": post_idle_source_amount_raw,
            "postIdleSourceObservedSlot": movement.post_snapshot_observed_slot,
            "postIdleSourceObservedAt": movement.post_snapshot_observed_at,
            "idleSourceDecreasedAndTargetIncreased": idle_effect,
            "routeEffectProven": route_effect,
            "rpcFound": finality.map(|status| status.found),
            "rpcFinalized": finality.map(|status| status.finalized),
            "rpcSuccessful": finality.map(|status| status.successful),
            "rpcSlot": finality.and_then(|status| status.slot),
            "finalizedSuccess": finalized_success,
            "terminalOutcomeSafe": terminal_outcome_safe,
        }),
        effect_pass && finalized_success,
        economic_pass,
        terminal_outcome_safe,
    )
}

fn validated_baseline_collected_at(baseline: &Value, cluster: &str) -> Option<DateTime<Utc>> {
    if baseline.get("schemaVersion")?.as_u64()? != u64::from(SCHEMA_VERSION)
        || baseline.get("event")?.as_str()? != "fleet_orchestration_production_evidence"
        || baseline.pointer("/scope/cluster")?.as_str()? != cluster
        || !baseline.pointer("/scope/cutoverAt")?.is_null()
        || baseline.pointer("/scope/baselinePathSupplied")?.as_bool()?
        || baseline
            .pointer("/source/trackedWorktreeDirty")?
            .as_bool()?
        || baseline.get("callerVerdictsAccepted")?.as_bool()?
    {
        return None;
    }
    let collected_at = DateTime::parse_from_rfc3339(baseline.get("collectedAt")?.as_str()?)
        .ok()?
        .with_timezone(&Utc);
    let captured_at = DateTime::parse_from_rfc3339(baseline.get("capturedAt")?.as_str()?)
        .ok()?
        .with_timezone(&Utc);
    (collected_at == captured_at).then_some(collected_at)
}

fn baseline_main(
    baseline: Option<&Value>,
    cluster: &str,
) -> Option<(DateTime<Utc>, i64, Vec<i64>)> {
    let baseline = baseline?;
    let collected_at = validated_baseline_collected_at(baseline, cluster)?;
    if baseline
        .pointer("/measurements/database/positions/mainUsdc/freshForBaseline")?
        .as_bool()
        != Some(true)
    {
        return None;
    }
    let amount = baseline
        .pointer("/measurements/database/positions/mainUsdc/amountRaw")?
        .as_i64()?;
    let vault_ids = baseline
        .pointer("/measurements/database/positions/mainUsdcCohort/vaultIds")?
        .as_array()?
        .iter()
        .map(Value::as_i64)
        .collect::<Option<Vec<_>>>()?;
    let vault_count = baseline
        .pointer("/measurements/database/positions/mainUsdcCohort/vaultCount")?
        .as_i64()?;
    let unique_vault_ids = vault_ids.iter().copied().collect::<BTreeSet<_>>();
    if vault_ids.is_empty()
        || vault_ids.iter().any(|vault_id| *vault_id <= 0)
        || unique_vault_ids.len() != vault_ids.len()
        || i64::try_from(vault_ids.len()).ok()? != vault_count
    {
        return None;
    }
    Some((collected_at, amount, vault_ids))
}

async fn post_baseline_deposits(
    pool: &PgPool,
    baseline_at: DateTime<Utc>,
    vault_ids: &[i64],
) -> Result<i64, sqlx::Error> {
    if vault_ids.is_empty() {
        return Ok(0);
    }
    sqlx::query_scalar(
        r#"
        SELECT COALESCE(sum(execution.amount_raw), 0)::BIGINT
        FROM loyal_yield.balance_sweep_executions execution
        JOIN loyal_yield.balance_sweep_targets target
          ON target.id = execution.target_id
        JOIN loyal_yield.managed_vaults vault
          ON vault.settings = target.settings
         AND vault.vault_index = target.vault_index
         AND vault.vault_pubkey = target.vault_pubkey
        WHERE execution.inserted_at >= $1
          AND execution.token_mint = $2
          AND vault.id = ANY($3)
        "#,
    )
    .bind(baseline_at)
    .bind(USDC_MINT.to_string())
    .bind(vault_ids)
    .fetch_one(pool)
    .await
}

async fn current_baseline_cohort_main(
    pool: &PgPool,
    vault_ids: &[i64],
) -> Result<i64, sqlx::Error> {
    if vault_ids.is_empty() {
        return Ok(0);
    }
    sqlx::query_scalar(
        r#"
        SELECT COALESCE(sum(amount_raw), 0)::BIGINT
        FROM loyal_yield.vault_reserve_positions_current
        WHERE vault_id = ANY($1)
          AND reserve = $2
          AND liquidity_mint = $3
          AND has_value
          AND amount_raw > 0
        "#,
    )
    .bind(vault_ids)
    .bind(KAMINO_MAIN_USDC_RESERVE.to_string())
    .bind(USDC_MINT.to_string())
    .fetch_one(pool)
    .await
}

async fn database_deadlock_measurement(
    pool: &PgPool,
    cluster: &str,
    cutover_at: Option<DateTime<Utc>>,
    baseline: Option<&Value>,
) -> Result<i64, sqlx::Error> {
    let row = sqlx::query(
        r#"
        SELECT deadlocks::BIGINT AS deadlocks, stats_reset
        FROM pg_stat_database
        WHERE datname = current_database()
        "#,
    )
    .fetch_one(pool)
    .await?;
    let total: i64 = row.try_get("deadlocks")?;
    let stats_reset: Option<DateTime<Utc>> = row.try_get("stats_reset")?;
    let Some(cutover_at) = cutover_at else {
        // A pre-cutover artifact carries the cumulative counter solely so the
        // post-cutover collector can source-bind an exact delta.
        return Ok(total);
    };
    let baseline_total = baseline.and_then(|baseline| {
        let collected_at = validated_baseline_collected_at(baseline, cluster)?;
        (collected_at <= cutover_at
            // A stats reset after the baseline loses part of the interval and
            // cannot support an exact zero-deadlock claim.
            && stats_reset.is_none_or(|reset| reset <= collected_at))
        .then(|| {
            baseline
                .pointer("/measurements/database/movement/databaseDeadlockCount")?
                .as_i64()
        })
        .flatten()
    });
    if let Some(before) = baseline_total.filter(|before| *before >= 0 && *before <= total) {
        return Ok(total.saturating_sub(before));
    }
    // A cumulative counter without a pre-cutover fence cannot prove the
    // required interval. Emit a fail-closed integer instead of guessing zero.
    Ok(-1)
}

async fn duplicate_movement_count(
    pool: &PgPool,
    cluster: &str,
    cutover_at: DateTime<Utc>,
) -> Result<i64, sqlx::Error> {
    sqlx::query_scalar(
        r#"
        SELECT COALESCE(sum(movement_count - 1), 0)::BIGINT
        FROM (
            SELECT opportunity.optimizer_epoch_id, opportunity.vault_id,
                   count(DISTINCT submission.id)::BIGINT AS movement_count
            FROM loyal_yield.signed_route_submissions submission
            JOIN loyal_yield.rebalance_opportunities opportunity
              ON opportunity.id = submission.opportunity_id
            WHERE submission.cluster = $1
              AND submission.created_at >= $2
              AND submission.submission_state NOT IN ('expired', 'failed')
            GROUP BY opportunity.optimizer_epoch_id, opportunity.vault_id
            HAVING count(DISTINCT submission.id) > 1
        ) duplicates
        "#,
    )
    .bind(cluster)
    .bind(cutover_at)
    .fetch_one(pool)
    .await
}

async fn collect_movement_evidence(
    pool: &PgPool,
    cluster: &str,
    cutover_at: Option<DateTime<Utc>>,
    baseline: Option<&Value>,
    current_positions: &Value,
) -> Result<(Value, Verdict), sqlx::Error> {
    let database_deadlock_count =
        database_deadlock_measurement(pool, cluster, cutover_at, baseline).await?;
    let Some(cutover_at) = cutover_at else {
        return Ok((
            json!({
                "available": false,
                "reason": "--cutover-at is required for finalized movement evidence",
                "databaseDeadlockCount": database_deadlock_count,
                "duplicateMovementCount": 0,
            }),
            Verdict::NotRun,
        ));
    };
    let duplicate_movement_count = duplicate_movement_count(pool, cluster, cutover_at).await?;
    let movements = load_movements(pool, cluster, cutover_at).await?;
    let baseline =
        baseline_main(baseline, cluster).filter(|(baseline_at, _, _)| *baseline_at <= cutover_at);
    let (baseline_collected_at, baseline_amount_raw, baseline_vault_ids) = baseline
        .map(|(at, amount, ids)| (Some(at), Some(amount), ids))
        .unwrap_or((None, None, Vec::new()));
    let baseline_vaults = baseline_vault_ids.iter().copied().collect::<BTreeSet<_>>();
    let rpc_url = env::var("SOLANA_RPC_URL").ok();
    let finality = finalized_signatures(rpc_url.as_deref(), &movements).await;
    let mut safe_rows = Vec::new();
    let mut reconciled_movement_count = 0i64;
    let mut reconciled_reserve_count = 0i64;
    let mut reconciled_idle_deposit_count = 0i64;
    let mut fully_proven_count = 0i64;
    let mut economic_failure_count = 0i64;
    let mut unsafe_terminal_outcome_count = 0i64;
    let mut nonterminal_count = 0i64;
    let mut ambiguous_count = 0i64;
    let mut main_outflow_raw = 0i128;
    let mut main_inflow_raw = 0i128;
    let mut baseline_main_outflow_raw = 0i128;
    let mut baseline_main_inflow_raw = 0i128;
    let main_reserve = KAMINO_MAIN_USDC_RESERVE.to_string();
    let usdc_mint = USDC_MINT.to_string();
    for movement in &movements {
        if !matches!(
            movement.submission_state.as_str(),
            "reconciled" | "expired" | "failed"
        ) {
            nonterminal_count += 1;
        }
        if movement.submission_state == "effect_ambiguous" {
            ambiguous_count += 1;
        }
        if movement.submission_state == "reconciled" {
            reconciled_movement_count += 1;
            match movement.route_kind.as_str() {
                "same_mint" if movement.source_reserve.is_some() => {
                    reconciled_reserve_count += 1;
                }
                "idle_vault_deposit" if movement.source_reserve.is_none() => {
                    reconciled_idle_deposit_count += 1;
                }
                _ => {}
            }
        }
        if movement.submission_state == "reconciled" && movement.liquidity_mint == usdc_mint {
            if movement.route_kind == "same_mint"
                && movement.source_reserve.as_deref() == Some(main_reserve.as_str())
            {
                main_outflow_raw += i128::from(movement.amount_raw);
                if baseline_vaults.contains(&movement.vault_id) {
                    baseline_main_outflow_raw += i128::from(movement.amount_raw);
                }
            }
            if matches!(
                movement.route_kind.as_str(),
                "same_mint" | "idle_vault_deposit"
            ) && movement.target_reserve == main_reserve
            {
                main_inflow_raw += i128::from(movement.amount_raw);
                if baseline_vaults.contains(&movement.vault_id) {
                    baseline_main_inflow_raw += i128::from(movement.amount_raw);
                }
            }
        }
        let (safe, fully_proven, economic_pass, terminal_outcome_safe) = movement_json(
            movement,
            finality
                .as_ref()
                .ok()
                .and_then(|evidence| evidence.statuses.get(&movement.signature)),
            finality
                .as_ref()
                .ok()
                .map(|evidence| evidence.finalized_block_height),
            finality
                .as_ref()
                .ok()
                .map(|evidence| evidence.finalized_slot),
        );
        if fully_proven {
            fully_proven_count += 1;
        }
        if !economic_pass {
            economic_failure_count += 1;
        }
        if !terminal_outcome_safe {
            unsafe_terminal_outcome_count += 1;
        }
        safe_rows.push(safe);
    }
    let current_routeable_main = current_positions
        .pointer("/mainUsdc/amountRaw")
        .and_then(Value::as_i64);
    let current_baseline_main = if baseline_collected_at.is_some() {
        Some(current_baseline_cohort_main(pool, &baseline_vault_ids).await?)
    } else {
        None
    };
    let deposits = if let Some(at) = baseline_collected_at {
        Some(post_baseline_deposits(pool, at, &baseline_vault_ids).await?)
    } else {
        None
    };
    let adjusted_main_reduction = baseline_amount_raw
        .zip(current_baseline_main)
        .zip(deposits)
        .map(|((before, current), deposits)| {
            i128::from(before) + i128::from(deposits) - i128::from(current)
        });
    let confirmed_main_net_outflow = main_outflow_raw - main_inflow_raw;
    let baseline_confirmed_main_net_outflow = baseline_main_outflow_raw - baseline_main_inflow_raw;
    let main_reduction_pass = adjusted_main_reduction.is_some_and(|reduction| {
        baseline_confirmed_main_net_outflow > 0
            && reduction > 0
            && reduction.saturating_mul(100)
                >= baseline_confirmed_main_net_outflow.saturating_mul(95)
    });
    let rpc_available = finality
        .as_ref()
        .is_ok_and(|evidence| evidence.finalized_block_height > 0 && evidence.finalized_slot > 0);
    let pass = rpc_available
        && reconciled_reserve_count > 0
        && reconciled_reserve_count + reconciled_idle_deposit_count == reconciled_movement_count
        && fully_proven_count == reconciled_movement_count
        && economic_failure_count == 0
        && unsafe_terminal_outcome_count == 0
        && nonterminal_count == 0
        && ambiguous_count == 0
        && database_deadlock_count == 0
        && duplicate_movement_count == 0
        && main_reduction_pass;
    Ok((
        json!({
            "available": true,
            "cutoverAt": cutover_at,
            "rpcFinalityAvailable": rpc_available,
            "rpcReadError": finality.as_ref().err().map(|_| "finalized RPC evidence unavailable"),
            "rpcFinalizedBlockHeight": finality
                .as_ref()
                .ok()
                .map(|evidence| evidence.finalized_block_height),
            "rpcFinalizedSlot": finality
                .as_ref()
                .ok()
                .map(|evidence| evidence.finalized_slot),
            "submissionCount": movements.len(),
            "nonterminalSubmissionCount": nonterminal_count,
            "effectAmbiguousCount": ambiguous_count,
            "reconciledMovementCount": reconciled_movement_count,
            "reconciledReserveMovementCount": reconciled_reserve_count,
            "reconciledIdleDepositCount": reconciled_idle_deposit_count,
            "fullyFinalizedAndReconciledEffectCount": fully_proven_count,
            "economicFailureCount": economic_failure_count,
            "unsafeTerminalOutcomeCount": unsafe_terminal_outcome_count,
            "databaseDeadlockCount": database_deadlock_count,
            "duplicateMovementCount": duplicate_movement_count,
            "mainUsdc": {
                "reserve": main_reserve,
                "baselineCollectedAt": baseline_collected_at,
                "baselineAmountRaw": baseline_amount_raw,
                "baselineCohortVaultCount": baseline_vault_ids.len(),
                "baselineCohortVaultIds": baseline_vault_ids,
                "postBaselineCohortDepositAmountRaw": deposits,
                "currentBaselineCohortAmountRaw": current_baseline_main,
                "currentRouteableAmountRaw": current_routeable_main,
                "confirmedOptimizerOutflowRaw": main_outflow_raw,
                "confirmedOptimizerInflowRaw": main_inflow_raw,
                "confirmedOptimizerNetOutflowRaw": confirmed_main_net_outflow,
                "baselineCohortConfirmedOptimizerOutflowRaw": baseline_main_outflow_raw,
                "baselineCohortConfirmedOptimizerInflowRaw": baseline_main_inflow_raw,
                "baselineCohortConfirmedOptimizerNetOutflowRaw": baseline_confirmed_main_net_outflow,
                "depositAdjustedReductionRaw": adjusted_main_reduction,
                "reductionAfterDepositsCoversConfirmedNetOutflow": main_reduction_pass,
            },
            "movements": safe_rows,
            "pass": pass,
        }),
        if pass { Verdict::Pass } else { Verdict::Fail },
    ))
}

async fn collect_database_evidence(
    options: &Options,
    baseline: Option<&Value>,
    alt_runtime: AltRenderRuntime,
) -> DatabaseEvidence {
    let Some(database_url) = env::var("NEON_DATABASE_URL")
        .ok()
        .filter(|value| !value.trim().is_empty())
    else {
        return DatabaseEvidence {
            migrations: json!({"available": false, "error": "NEON_DATABASE_URL is missing"}),
            queue: json!({"available": false, "error": "database evidence unavailable"}),
            positions: json!({"available": false, "error": "database evidence unavailable"}),
            movements: json!({"available": false, "error": "database evidence unavailable"}),
            alt_repair: unavailable_alt_repair_evidence(&options.cluster, alt_runtime),
            migrations_pass: false,
            queue_verdict: Verdict::NotRun,
            movement_verdict: Verdict::NotRun,
        };
    };
    let Ok(pool) = connect_database(&database_url).await else {
        return DatabaseEvidence {
            migrations: json!({"available": false, "error": "database connection failed"}),
            queue: json!({"available": false, "error": "database connection failed"}),
            positions: json!({"available": false, "error": "database connection failed"}),
            movements: json!({"available": false, "error": "database connection failed"}),
            alt_repair: unavailable_alt_repair_evidence(&options.cluster, alt_runtime),
            migrations_pass: false,
            queue_verdict: Verdict::NotRun,
            movement_verdict: Verdict::NotRun,
        };
    };
    let positions = match collect_position_evidence(&pool).await {
        Ok(value) => value,
        Err(_) => json!({"available": false, "error": "position measurement query failed"}),
    };
    let (migrations, migrations_pass) = match collect_migration_evidence(&pool).await {
        Ok(value) => value,
        Err(_) => (
            json!({"available": false, "error": "migration measurement query failed"}),
            false,
        ),
    };
    let rpc_url = env::var("SOLANA_RPC_URL").ok();
    let alt_repair =
        collect_alt_repair_evidence(&pool, &options.cluster, rpc_url.as_deref(), alt_runtime)
            .await
            .unwrap_or_else(|_| unavailable_alt_repair_evidence(&options.cluster, alt_runtime));
    let queue_schema_available = relation_exists(&pool, "loyal_yield.fleet_orchestration_status")
        .await
        .unwrap_or(false)
        && relation_exists(&pool, "loyal_yield.target_capacity_reservations")
            .await
            .unwrap_or(false);
    let (queue, queue_verdict) = if queue_schema_available {
        match collect_queue_evidence(&pool, &options.cluster).await {
            Ok(value) => {
                let verdict = queue_verdict(&value);
                (value, verdict)
            }
            Err(_) => (
                json!({"available": false, "error": "queue measurement query failed"}),
                Verdict::Fail,
            ),
        }
    } else {
        (
            json!({
                "available": false,
                "error": "fleet queue schema is unavailable",
            }),
            Verdict::Fail,
        )
    };
    let (mut movements, mut movement_verdict) = if queue_schema_available
        && positions["available"] == true
    {
        match collect_movement_evidence(
            &pool,
            &options.cluster,
            options.cutover_at,
            baseline,
            &positions,
        )
        .await
        {
            Ok(value) => value,
            Err(_) => (
                json!({"available": false, "error": "movement measurement query failed"}),
                Verdict::Fail,
            ),
        }
    } else {
        (
            json!({"available": false, "error": "queue schema or position evidence unavailable"}),
            Verdict::NotRun,
        )
    };
    let (production_slos, production_slos_pass) = production_slo_measurements(&queue);
    if let Some(object) = movements.as_object_mut() {
        object.insert("productionSlos".to_owned(), production_slos);
        if movement_verdict == Verdict::Pass && !production_slos_pass {
            object.insert("pass".to_owned(), Value::Bool(false));
        }
    }
    if movement_verdict == Verdict::Pass && !production_slos_pass {
        movement_verdict = Verdict::Fail;
    }
    DatabaseEvidence {
        migrations,
        queue,
        positions,
        movements,
        alt_repair,
        migrations_pass,
        queue_verdict,
        movement_verdict,
    }
}

fn load_baseline(path: Option<&Path>) -> Result<Option<Value>, Box<dyn Error>> {
    path.map(|path| {
        let bytes = fs::read(path)?;
        let value: Value = serde_json::from_slice(&bytes)?;
        Ok(value)
    })
    .transpose()
}

fn baseline_is_source_bound(baseline: &Value, options: &Options, render_yaml: &str) -> bool {
    let Some(head) = git_output(&options.repository_root, &["rev-parse", "HEAD"]) else {
        return false;
    };
    validated_baseline_collected_at(baseline, &options.cluster).is_some()
        && baseline
            .pointer("/scope/renderEnvironmentId")
            .and_then(Value::as_str)
            == Some(options.render_environment_id.as_str())
        && baseline
            .pointer("/measurements/render/environmentId")
            .and_then(Value::as_str)
            == Some(options.render_environment_id.as_str())
        && baseline.get("headCommit").and_then(Value::as_str) == Some(head.as_str())
        && baseline
            .pointer("/source/repositoryHead")
            .and_then(Value::as_str)
            == Some(head.as_str())
        && baseline
            .pointer("/source/renderYamlSha256")
            .and_then(Value::as_str)
            == Some(sha256_hex(render_yaml.as_bytes()).as_str())
        && baseline
            .pointer("/source/collectorSource")
            .and_then(Value::as_str)
            .is_some_and(|source| source.contains("measurements"))
}

fn aggregate_verdict(verdicts: impl IntoIterator<Item = Verdict>) -> Verdict {
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

async fn run(options: Options) -> Result<ExitCode, Box<dyn Error>> {
    let collected_at = Utc::now();
    let render_yaml = fs::read_to_string(options.repository_root.join("render.yaml"))?;
    let expected = expected_services(&render_yaml)?;
    let baseline = load_baseline(options.baseline.as_deref())?;
    if baseline
        .as_ref()
        .is_some_and(|baseline| !baseline_is_source_bound(baseline, &options, &render_yaml))
    {
        return Err("baseline artifact is not bound to this clean source and Render scope".into());
    }
    let (render, render_pass, alt_runtime) =
        collect_render_evidence(&expected, &options.render_environment_id, &options.cluster).await;
    let database = collect_database_evidence(&options, baseline.as_ref(), alt_runtime).await;
    let deployment_verdict = if render_pass && database.migrations_pass {
        Verdict::Pass
    } else {
        Verdict::Fail
    };
    let production_performance_verdict =
        aggregate_verdict([database.queue_verdict, database.movement_verdict]);
    let end_state_verdict = aggregate_verdict([deployment_verdict, production_performance_verdict]);
    let output = json!({
        "schemaVersion": SCHEMA_VERSION,
        "event": "fleet_orchestration_production_evidence",
        "collectedAt": collected_at,
        "capturedAt": collected_at,
        "headCommit": git_output(&options.repository_root, &["rev-parse", "HEAD"]),
        "scope": {
            "cluster": options.cluster,
            "renderEnvironmentId": options.render_environment_id,
            "cutoverAt": options.cutover_at,
            "baselinePathSupplied": options.baseline.is_some(),
        },
        "source": source_evidence(&options.repository_root, &render_yaml),
        "measurements": {
            "render": render,
            "database": {
                "migrations": database.migrations,
                "queue": database.queue,
                "positions": database.positions,
                "movement": database.movements,
                "altRepair": database.alt_repair,
            },
        },
        // These are operator feedback only. The standing verifier must recompute
        // every verdict from the measurements above and source-bind this schema.
        "recomputedVerdicts": {
            "productionMigrationAndAtomicCutover": deployment_verdict,
            "completeFleetEvaluation": database.queue_verdict,
            "correctProductionMovement": database.movement_verdict,
            "deployment": deployment_verdict,
            "productionPerformance": production_performance_verdict,
            "endState": end_state_verdict,
        },
        "callerVerdictsAccepted": false,
    });
    let bytes = if options.compact {
        serde_json::to_vec(&output)?
    } else {
        serde_json::to_vec_pretty(&output)?
    };
    if let Some(path) = options.output.as_deref() {
        fs::write(path, &bytes)?;
    }
    println!("{}", String::from_utf8(bytes)?);
    Ok(if end_state_verdict == Verdict::Pass {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    })
}

#[tokio::main]
async fn main() -> Result<ExitCode, Box<dyn Error>> {
    let Some(options) = parse_options()? else {
        return Ok(ExitCode::SUCCESS);
    };
    run(options).await
}
