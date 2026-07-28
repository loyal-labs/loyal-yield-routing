#![recursion_limit = "512"]

use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    fs,
    path::{Path, PathBuf},
    process::{Command, ExitCode},
    str::FromStr,
    time::Instant,
};

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use chrono::{DateTime, Utc};
use loyal_actions::{KAMINO_MAIN_MARKET, KAMINO_MAIN_USDC_RESERVE, USDC_MINT};
use loyal_yield_orchestrator::{
    supported_stable_mints, AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
    MAX_QUEUE_POSITIVE_AMOUNT_DRIFT_PPM, ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
    STANDARD_POLICY_AUTHORITY,
};
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
const HEAVY_RENDER_ENVIRONMENT_ID: &str = "evm-d8kgt3a8qa3s7382glc0";
const KAMINO_MONITOR_SERVICE_ID: &str = "srv-d8h4i9a8pkls73bver00";
const KAMINO_MONITOR_SERVICE_NAME: &str = "loyal-kamino-reserve-monitor";
const RENDER_API_BASE_URL: &str = "https://api.render.com/v1";
const SERIAL_MONITOR_NAME: &str = "loyal-same-mint-yield-monitor";
const MATERIAL_PRINCIPAL_USD_MICROS: i64 = 1_000_000_000;
const MAX_MATERIAL_STAGE_AGE_SECONDS: i64 = 600;
const MAX_FULL_SWEEP_AGE_SECONDS: i64 = 120;
const SAME_MINT_ROUTE_MODE: &str = "same_mint_kamino";
const ROUTE_AMOUNT_SEMANTICS_IDLE_VAULT_LIQUIDITY: &str = "idle_vault_liquidity";
const TIMESCALE_MARKET_MIGRATION_VERSION: i64 = 5;
const TIMESCALE_MARKET_MIGRATION_NAME: &str = "kamino_confirmed_state_verification";
const TIMESCALE_MARKET_MIGRATION_SQL: &str = include_str!(
    "../../../loyal-timescale-migrations/migrations/0005_kamino_confirmed_state_verification.sql"
);
const MARKET_VERIFICATION_WARNING_SECONDS: i64 = 90;
const MARKET_VERIFICATION_HARD_EXPIRY_SECONDS: i64 = 240;
const SUPPORTED_RESERVE_CATALOG_MAX_AGE_SECONDS: i64 = 300;
const MARKET_EVIDENCE_QUERY_TIMEOUT_MILLISECONDS: i64 = 15_000;
const PRODUCTION_EVIDENCE_MAX_COLLECTION_SECONDS: i64 = 300;
const COMPILED_COLLECTOR_SOURCE: &[u8] =
    include_bytes!("fleet-orchestration-production-evidence.rs");

const DURABLE_SERVICE_NAMES: [&str; 6] = [
    "loyal-fleet-opportunity-planner",
    "loyal-fleet-route-revalidator",
    "loyal-fleet-route-executor",
    "loyal-fleet-route-confirmer",
    "loyal-fleet-route-reconciler",
    "loyal-route-lookup-table-provisioner",
];

const REQUIRED_MIGRATIONS: [(i64, &str, &str); 9] = [
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
    (
        29,
        "fleet_commit_lifetime_fences",
        include_str!("../../migrations/0029_fleet_commit_lifetime_fences.sql"),
    ),
    (
        30,
        "fused_queue_accrual_binding",
        include_str!("../../migrations/0030_fused_queue_accrual_binding.sql"),
    ),
    (
        31,
        "fleet_commit_lifetime_fence_errcode",
        include_str!("../../migrations/0031_fleet_commit_lifetime_fence_errcode.sql"),
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
    optimizer_epoch_fingerprint: String,
    optimizer_epoch_expires_at: DateTime<Utc>,
    submission_optimizer_epoch_evidence: Value,
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

#[derive(Debug, Clone, Copy, Serialize)]
#[serde(rename_all = "camelCase")]
struct ReconciledVolumeSnapshot {
    movement_count: i64,
    amount_raw: i64,
    principal_usd_micros: i64,
    newest_reconciled_at: Option<DateTime<Utc>>,
    unique_submission_count: i64,
    unique_opportunity_count: i64,
    unique_decision_count: i64,
    unique_signature_count: i64,
}

impl ReconciledVolumeSnapshot {
    fn identity_exact(self) -> bool {
        self.movement_count >= 0
            && self.amount_raw >= 0
            && self.principal_usd_micros >= 0
            && self.unique_submission_count == self.movement_count
            && self.unique_opportunity_count == self.movement_count
            && self.unique_decision_count == self.movement_count
            && self.unique_signature_count == self.movement_count
    }

    fn checked_delta(self, baseline: Self) -> Option<Self> {
        Some(Self {
            movement_count: self.movement_count.checked_sub(baseline.movement_count)?,
            amount_raw: self.amount_raw.checked_sub(baseline.amount_raw)?,
            principal_usd_micros: self
                .principal_usd_micros
                .checked_sub(baseline.principal_usd_micros)?,
            newest_reconciled_at: self.newest_reconciled_at,
            unique_submission_count: self
                .unique_submission_count
                .checked_sub(baseline.unique_submission_count)?,
            unique_opportunity_count: self
                .unique_opportunity_count
                .checked_sub(baseline.unique_opportunity_count)?,
            unique_decision_count: self
                .unique_decision_count
                .checked_sub(baseline.unique_decision_count)?,
            unique_signature_count: self
                .unique_signature_count
                .checked_sub(baseline.unique_signature_count)?,
        })
        .filter(|delta| delta.identity_exact())
    }
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

#[derive(Debug)]
struct MarketDataPlaneEvidence {
    timescale: Value,
    pass: bool,
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
                     TIMESCALEDB_URL, RENDER_API_KEY, and (when --cutover-at is set) \
                     SOLANA_RPC_URL. \
                     Output never includes environment values, database/RPC URLs, \
                     signer material, or signed transaction bytes. Capture --output \
                     before cutover, then pass that artifact with --baseline after cutover."
                );
                return Ok(None);
            }
            other => return Err(format!("unknown argument: {other}").into()),
        }
    }
    if cluster.trim().is_empty() {
        return Err("cluster is required".into());
    }
    if render_environment_id != DEFAULT_RENDER_ENVIRONMENT_ID {
        return Err(format!(
            "production evidence must use Render environment {DEFAULT_RENDER_ENVIRONMENT_ID}"
        )
        .into());
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

fn sha256_file(path: &Path) -> Option<String> {
    fs::read(path).ok().map(|bytes| sha256_hex(&bytes))
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

fn collector_checkout_source_sha256(repository_root: &Path) -> Option<String> {
    fs::read(
        repository_root.join(
            "crates/loyal-yield-orchestrator/src/bin/fleet-orchestration-production-evidence.rs",
        ),
    )
    .ok()
    .map(|bytes| sha256_hex(&bytes))
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
        "collectorCompiledSourceSha256": sha256_hex(COMPILED_COLLECTOR_SOURCE),
        "collectorCheckoutSourceSha256": collector_checkout_source_sha256(repository_root),
        "collectorExecutableSha256": env::current_exe().ok().as_deref().and_then(sha256_file),
        "collectorSource": "compiled production-owned measurements; caller verdicts are non-authoritative",
    })
}

fn scope_fingerprint_nonce(
    repository_root: &Path,
    render_yaml: &str,
    collection_started_at: DateTime<Utc>,
) -> String {
    let mut hasher = Sha256::new();
    hasher.update(b"loyal-production-scope-fingerprint-v1\0");
    hasher.update(
        git_output(repository_root, &["rev-parse", "HEAD"])
            .unwrap_or_default()
            .as_bytes(),
    );
    hasher.update(sha256_hex(render_yaml.as_bytes()).as_bytes());
    hasher.update(collection_started_at.timestamp_micros().to_le_bytes());
    hasher.update(std::process::id().to_le_bytes());
    format!("{:x}", hasher.finalize())
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

fn fingerprint_live_values(
    live_env: &BTreeMap<String, Option<String>>,
    nonce: &str,
    keys: impl IntoIterator<Item = String>,
) -> BTreeMap<String, String> {
    keys.into_iter()
        .filter_map(|key| {
            let value = live_env.get(&key)?.as_deref()?;
            Some((key.clone(), env_value_fingerprint(nonce, &key, value)))
        })
        .collect()
}

fn role_scope_value_keys(name: &str, env_keys: &BTreeSet<String>) -> BTreeSet<String> {
    env_keys
        .iter()
        .filter(|key| key.as_str() != "RUST_LOG")
        .filter(|key| {
            key.as_str() != "HELIUS_API_KEY"
                && key.as_str() != "LASERSTREAM_ENDPOINT"
                && key.as_str() != "KAMINO_API_BASE"
                && key.as_str() != "KAMINO_UPDATE_SOURCE"
        })
        .filter(|key| name != "loyal-fleet-opportunity-planner" || key.as_str() != "POLICY_KEYPAIR")
        .cloned()
        .collect()
}

fn monitor_scope_value_keys(env_keys: &BTreeSet<String>) -> BTreeSet<String> {
    env_keys
        .iter()
        .filter(|key| key.as_str() != "RUST_LOG")
        .cloned()
        .collect()
}

fn project_production_environment<'a>(render_yaml: &'a str, project_name: &str) -> Option<&'a str> {
    let project = render_yaml.find(&format!("  - name: {project_name}"))?;
    let project = &render_yaml[project..];
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

fn expected_kamino_monitor(render_yaml: &str) -> Result<ExpectedService, Box<dyn Error>> {
    let production = project_production_environment(render_yaml, "loyal-yield-laserstream-workers")
        .ok_or("render.yaml has no loyal-yield-laserstream-workers production environment")?;
    let matching = service_blocks(production)
        .into_iter()
        .filter(|block| scalar(block, "name").as_deref() == Some(KAMINO_MONITOR_SERVICE_NAME))
        .collect::<Vec<_>>();
    if matching.len() != 1 {
        return Err(format!(
            "render.yaml must declare exactly one production service named {KAMINO_MONITOR_SERVICE_NAME}"
        )
        .into());
    }
    let block = &matching[0];
    Ok(ExpectedService {
        name: KAMINO_MONITOR_SERVICE_NAME.to_owned(),
        image: scalar(block, "url").ok_or("Kamino monitor has no image URL")?,
        command: scalar(block, "dockerCommand").ok_or("Kamino monitor has no command")?,
        pre_deploy_command: scalar(block, "preDeployCommand")
            .ok_or("Kamino monitor has no pre-deploy command")?,
        plan: scalar(block, "plan").ok_or("Kamino monitor has no plan")?,
        env_keys: block
            .lines()
            .filter_map(|line| line.trim().strip_prefix("- key:"))
            .map(str::trim)
            .map(str::to_owned)
            .collect(),
    })
}

fn image_commit_suffix(image: &str) -> Option<&str> {
    let suffix = image.rsplit_once(":sha-")?.1;
    (suffix.len() == 40 && suffix.bytes().all(|byte| byte.is_ascii_hexdigit())).then_some(suffix)
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

async fn collect_market_monitor_render_evidence(
    client: &Client,
    api_key: &str,
    expected_monitor: &ExpectedService,
    expected_light_services: &[ExpectedService],
    scope_fingerprint_nonce: &str,
) -> (Value, bool, bool) {
    let services_response = render_get(
        client,
        api_key,
        "/services",
        &[
            ("environmentId", HEAVY_RENDER_ENVIRONMENT_ID),
            ("limit", "100"),
        ],
    )
    .await;
    let service = services_response.as_ref().ok().and_then(|value| {
        let matching = wrapped_array(value, "service")
            .into_iter()
            .filter(|service| {
                json_string(service, "/id").as_deref() == Some(KAMINO_MONITOR_SERVICE_ID)
                    && json_string(service, "/name").as_deref() == Some(KAMINO_MONITOR_SERVICE_NAME)
            })
            .collect::<Vec<_>>();
        (matching.len() == 1).then_some(matching[0])
    });

    let deploy_response = if service.is_some() {
        render_get(
            client,
            api_key,
            &format!("/services/{KAMINO_MONITOR_SERVICE_ID}/deploys"),
            &[("limit", "1")],
        )
        .await
    } else {
        Err("scoped_market_monitor_not_found".to_owned())
    };
    let env_response = if service.is_some() {
        render_get(
            client,
            api_key,
            &format!("/services/{KAMINO_MONITOR_SERVICE_ID}/env-vars"),
            &[("limit", "100")],
        )
        .await
    } else {
        Err("scoped_market_monitor_not_found".to_owned())
    };
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
    let env_key_boundary_exact = live_env_keys == expected_monitor.env_keys;
    let env_value_fingerprints = fingerprint_live_values(
        &live_env,
        scope_fingerprint_nonce,
        monitor_scope_value_keys(&live_env_keys),
    );
    let data_scope_verified = env_response.is_ok()
        && live_env
            .get("TIMESCALEDB_URL")
            .and_then(Option::as_deref)
            .zip(env::var("TIMESCALEDB_URL").ok().as_deref())
            .is_some_and(|(live, expected)| live == expected)
        && live_env
            .get("SOLANA_RPC_URL")
            .and_then(Option::as_deref)
            .zip(env::var("SOLANA_RPC_URL").ok().as_deref())
            .is_some_and(|(live, expected)| live == expected)
        && live_env
            .get("KAMINO_UPDATE_SOURCE")
            .and_then(Option::as_deref)
            == Some("laserstream");
    let raw_command = service.and_then(|service| {
        json_string(service, "/serviceDetails/envSpecificDetails/dockerCommand")
    });
    let raw_pre_deploy = service.and_then(|service| {
        json_string(
            service,
            "/serviceDetails/envSpecificDetails/preDeployCommand",
        )
    });
    let command = raw_command.as_ref().map(|command| {
        if command == &expected_monitor.command && data_scope_verified {
            command.clone()
        } else {
            "[redacted: live market-monitor command or data scope differs from blueprint]"
                .to_owned()
        }
    });
    let pre_deploy = raw_pre_deploy.as_ref().map(|command| {
        if command == &expected_monitor.pre_deploy_command {
            command.clone()
        } else {
            "[redacted: live market-monitor pre-deploy differs from blueprint]".to_owned()
        }
    });
    let image_path = service.and_then(|service| json_string(service, "/imagePath"));
    let deploy_status = latest_deploy.and_then(|deploy| json_string(deploy, "/status"));
    let deploy_ref = latest_deploy.and_then(|deploy| json_string(deploy, "/image/ref"));
    let deploy_digest = latest_deploy.and_then(|deploy| json_string(deploy, "/image/sha"));
    let deploy_registry =
        latest_deploy.and_then(|deploy| json_string(deploy, "/image/registryCredential"));
    let light_suffixes = expected_light_services
        .iter()
        .filter_map(|service| image_commit_suffix(&service.image))
        .collect::<BTreeSet<_>>();
    let light_commit_suffix = (light_suffixes.len() == 1)
        .then(|| light_suffixes.iter().next().copied())
        .flatten();
    let laserstream_commit_suffix = image_commit_suffix(&expected_monitor.image);
    let image_commit_suffixes_match = light_commit_suffix.is_some()
        && light_commit_suffix == laserstream_commit_suffix
        && expected_light_services
            .iter()
            .all(|service| image_commit_suffix(&service.image) == light_commit_suffix);
    let monitor_matches = service.is_some()
        && json_string(service.expect("checked above"), "/suspended").as_deref()
            == Some("not_suspended")
        && json_string(service.expect("checked above"), "/type").as_deref()
            == Some("background_worker")
        && json_string(service.expect("checked above"), "/serviceDetails/runtime").as_deref()
            == Some("image")
        && json_string(service.expect("checked above"), "/serviceDetails/plan").as_deref()
            == Some(expected_monitor.plan.as_str())
        && image_path.as_deref() == Some(expected_monitor.image.as_str())
        && raw_command.as_deref() == Some(expected_monitor.command.as_str())
        && raw_pre_deploy.as_deref() == Some(expected_monitor.pre_deploy_command.as_str())
        && env_key_boundary_exact
        && data_scope_verified
        && deploy_status.as_deref() == Some("live")
        && deploy_ref.as_deref() == Some(expected_monitor.image.as_str())
        && deploy_digest
            .as_deref()
            .is_some_and(|digest| digest.starts_with("sha256:"))
        && deploy_registry.as_deref() == Some("loyal-ghcr");

    (
        json!({
            "environmentId": HEAVY_RENDER_ENVIRONMENT_ID,
            "serviceId": KAMINO_MONITOR_SERVICE_ID,
            "name": KAMINO_MONITOR_SERVICE_NAME,
            "present": service.is_some(),
            "matches": monitor_matches,
            "type": service.and_then(|service| json_string(service, "/type")),
            "suspended": service.and_then(|service| json_string(service, "/suspended")),
            "runtime": service.and_then(|service| json_string(service, "/serviceDetails/runtime")),
            "plan": service.and_then(|service| json_string(service, "/serviceDetails/plan")),
            "numInstances": service.and_then(|service| service.pointer("/serviceDetails/numInstances")),
            "image": image_path,
            "command": command,
            "preDeployCommand": pre_deploy,
            "envKeys": live_env_keys,
            "blueprintEnvKeys": expected_monitor.env_keys,
            "envValueFingerprints": env_value_fingerprints,
            "envKeyBoundaryExact": env_key_boundary_exact,
            "dataScopeVerified": data_scope_verified,
            "latestDeploy": latest_deploy.map(|deploy| json!({
                "id": json_string(deploy, "/id"),
                "status": deploy_status,
                "imageRef": deploy_ref,
                "imageDigest": deploy_digest,
                "registryCredential": deploy_registry,
                "startedAt": json_string(deploy, "/startedAt"),
                "finishedAt": json_string(deploy, "/finishedAt"),
            })),
            "serviceReadError": services_response.err(),
            "deployReadError": deploy_response.err(),
            "envReadError": env_response.err(),
        }),
        monitor_matches,
        image_commit_suffixes_match,
    )
}

async fn collect_render_evidence(
    expected: &[ExpectedService],
    expected_monitor: &ExpectedService,
    environment_id: &str,
    cluster: &str,
    scope_fingerprint_nonce: &str,
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
    let (market_monitor, market_monitor_matches, image_commit_suffixes_match) =
        collect_market_monitor_render_evidence(
            &client,
            &api_key,
            expected_monitor,
            expected,
            scope_fingerprint_nonce,
        )
        .await;
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
        let env_value_fingerprints = fingerprint_live_values(
            &live_env,
            scope_fingerprint_nonce,
            role_scope_value_keys(&expected_service.name, &live_env_keys),
        );
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
            "envValueFingerprints": env_value_fingerprints,
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
    let light_image_commit_suffix = expected_images
        .iter()
        .filter_map(|image| image_commit_suffix(image))
        .collect::<BTreeSet<_>>()
        .into_iter()
        .next()
        .map(str::to_owned);
    let laserstream_image_commit_suffix =
        image_commit_suffix(&expected_monitor.image).map(str::to_owned);
    let render_pass = expected.len() == DURABLE_SERVICE_NAMES.len()
        && expected_images.len() == 1
        && all_roles_match
        && one_digest
        && market_monitor_matches
        && image_commit_suffixes_match
        && serial_currently_incapable
        && no_dual_execution_order;
    (
        json!({
            "available": true,
            "capturedAt": Utc::now(),
            "environmentId": environment_id,
            "scopeFingerprintNonce": scope_fingerprint_nonce,
            "expectedImageReferences": expected_images,
            "roles": role_measurements,
            "allRolesMatch": all_roles_match,
            "deployDigests": deploy_digests,
            "oneImmutableDigest": one_digest,
            "heavyEnvironmentId": HEAVY_RENDER_ENVIRONMENT_ID,
            "marketMonitor": market_monitor,
            "lightImageCommitSuffix": light_image_commit_suffix,
            "laserstreamImageCommitSuffix": laserstream_image_commit_suffix,
            "imageCommitSuffixesMatch": image_commit_suffixes_match,
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

fn market_timescale_unavailable(
    captured_at: DateTime<Utc>,
    enabled_mints: &[String],
    relations: Value,
    migration: Value,
    error: &str,
) -> MarketDataPlaneEvidence {
    MarketDataPlaneEvidence {
        timescale: json!({
            "available": false,
            "capturedAt": captured_at,
            "migration": migration,
            "relations": relations,
            "enabledStableMints": enabled_mints,
            "activeDistinctSupportedReserveCount": Value::Null,
            "activeSupportedReserveCatalogRowCount": Value::Null,
            "duplicateActiveSupportedReserveCount": Value::Null,
            "nonKaminoApiActiveSupportedReserveCount": Value::Null,
            "staleActiveSupportedReserveOver300SecondsCount": Value::Null,
            "oldestActiveSupportedReserveFetchedAt": Value::Null,
            "oldestActiveSupportedReserveAgeSeconds": Value::Null,
            "currentPointerCoverageCount": Value::Null,
            "verificationCoverageCount": Value::Null,
            "exactLatestViewCoverageCount": Value::Null,
            "eventHashObservedAtIdentityViolationCount": Value::Null,
            "verificationStateIdentityViolationCount": Value::Null,
            "latestViewIdentityViolationCount": Value::Null,
            "stateSlotGreaterThanVerifiedSlotCount": Value::Null,
            "immutableTapeExactRowCardinalityViolationCount": Value::Null,
            "latestViewRowCardinalityViolationCount": Value::Null,
            "observationFloorCoverageCount": Value::Null,
            "observationFloorIdentityViolationCount": Value::Null,
            "observationFloorFutureObservedAtCount": Value::Null,
            "staleObservationFloorOver90SecondsCount": Value::Null,
            "invalidObservationFloorStateCount": Value::Null,
            "currentStateBelowObservationFloorCount": Value::Null,
            "atOrBelowFloorExactHashAdmissionCount": Value::Null,
            "verificationAtOrBelowObservationFloorWithoutExactHashCount": Value::Null,
            "conflictingAtOrBelowFloorRoutableStateCount": Value::Null,
            "nonConfirmedCommitmentCount": Value::Null,
            "nonHttpCurrentStateCount": Value::Null,
            "nonHttpVerificationSourceCount": Value::Null,
            "futureCurrentStateObservedAtCount": Value::Null,
            "futureVerificationWatermarkCount": Value::Null,
            "warningOver90SecondsCount": Value::Null,
            "hardExpiredOver240SecondsCount": Value::Null,
            "oldestVerificationAgeSeconds": Value::Null,
            "coverageQueryMilliseconds": Value::Null,
            "safeTargetQueryMilliseconds": Value::Null,
            "topVerifiedSafeTargets": [],
            "readError": error,
            "pass": false,
        }),
        pass: false,
    }
}

async fn collect_market_timescale_evidence(captured_at: DateTime<Utc>) -> MarketDataPlaneEvidence {
    let source_checksum = sha256_hex(TIMESCALE_MARKET_MIGRATION_SQL.as_bytes());
    // The production Blueprint intentionally omits EARN_ROUTER_ENABLED_STABLE_MINTS,
    // so the live planner uses the canonical code-owned six-mint default. Do
    // not let the collector's local shell environment narrow this denominator.
    let enabled_mints = supported_stable_mints();
    let Some(database_url) = env::var("TIMESCALEDB_URL")
        .ok()
        .filter(|value| !value.trim().is_empty())
    else {
        return market_timescale_unavailable(
            captured_at,
            &enabled_mints,
            json!({
                "migrationLedger": false,
                "supportedReserves": false,
                "reserveUpdates": false,
                "reserveCurrentStates": false,
                "reserveConfirmedObservationIdSequence": false,
                "reserveConfirmedObservationFloors": false,
                "reserveConfirmedVerifications": false,
                "latestVerifiedReserveUpdates": false,
            }),
            json!({
                "version": TIMESCALE_MARKET_MIGRATION_VERSION,
                "expectedName": TIMESCALE_MARKET_MIGRATION_NAME,
                "sourceChecksum": source_checksum,
                "appliedRowCount": 0,
                "appliedName": Value::Null,
                "appliedChecksum": Value::Null,
                "appliedAt": Value::Null,
            }),
            "TIMESCALEDB_URL is missing",
        );
    };
    let Ok(pool) = connect_database(&database_url).await else {
        return market_timescale_unavailable(
            captured_at,
            &enabled_mints,
            json!({
                "migrationLedger": false,
                "supportedReserves": false,
                "reserveUpdates": false,
                "reserveCurrentStates": false,
                "reserveConfirmedObservationIdSequence": false,
                "reserveConfirmedObservationFloors": false,
                "reserveConfirmedVerifications": false,
                "latestVerifiedReserveUpdates": false,
            }),
            json!({
                "version": TIMESCALE_MARKET_MIGRATION_VERSION,
                "expectedName": TIMESCALE_MARKET_MIGRATION_NAME,
                "sourceChecksum": source_checksum,
                "appliedRowCount": 0,
                "appliedName": Value::Null,
                "appliedChecksum": Value::Null,
                "appliedAt": Value::Null,
            }),
            "Timescale connection failed",
        );
    };

    let relation_row = sqlx::query(
        r#"
        SELECT to_regclass('loyal.timescale_schema_migrations') IS NOT NULL
                   AS migration_ledger,
               to_regclass('kamino.supported_reserves') IS NOT NULL
                   AS supported_reserves,
               to_regclass('kamino.reserve_updates') IS NOT NULL
                   AS reserve_updates,
               to_regclass('kamino.reserve_current_states') IS NOT NULL
                   AS reserve_current_states,
               to_regclass('kamino.reserve_confirmed_observation_id_seq') IS NOT NULL
                   AS reserve_confirmed_observation_id_sequence,
               to_regclass('kamino.reserve_confirmed_observation_floors') IS NOT NULL
                   AS reserve_confirmed_observation_floors,
               to_regclass('kamino.reserve_confirmed_verifications') IS NOT NULL
                   AS reserve_confirmed_verifications,
               to_regclass('kamino.latest_verified_reserve_updates') IS NOT NULL
                   AS latest_verified_reserve_updates
        "#,
    )
    .fetch_one(&pool)
    .await;
    let Ok(relation_row) = relation_row else {
        return market_timescale_unavailable(
            captured_at,
            &enabled_mints,
            json!({
                "migrationLedger": false,
                "supportedReserves": false,
                "reserveUpdates": false,
                "reserveCurrentStates": false,
                "reserveConfirmedObservationIdSequence": false,
                "reserveConfirmedObservationFloors": false,
                "reserveConfirmedVerifications": false,
                "latestVerifiedReserveUpdates": false,
            }),
            json!({
                "version": TIMESCALE_MARKET_MIGRATION_VERSION,
                "expectedName": TIMESCALE_MARKET_MIGRATION_NAME,
                "sourceChecksum": source_checksum,
                "appliedRowCount": 0,
                "appliedName": Value::Null,
                "appliedChecksum": Value::Null,
                "appliedAt": Value::Null,
            }),
            "Timescale relation inspection failed",
        );
    };
    let migration_ledger = relation_row
        .try_get::<bool, _>("migration_ledger")
        .unwrap_or(false);
    let supported_reserves = relation_row
        .try_get::<bool, _>("supported_reserves")
        .unwrap_or(false);
    let reserve_updates = relation_row
        .try_get::<bool, _>("reserve_updates")
        .unwrap_or(false);
    let reserve_current_states = relation_row
        .try_get::<bool, _>("reserve_current_states")
        .unwrap_or(false);
    let reserve_confirmed_observation_id_sequence = relation_row
        .try_get::<bool, _>("reserve_confirmed_observation_id_sequence")
        .unwrap_or(false);
    let reserve_confirmed_observation_floors = relation_row
        .try_get::<bool, _>("reserve_confirmed_observation_floors")
        .unwrap_or(false);
    let reserve_confirmed_verifications = relation_row
        .try_get::<bool, _>("reserve_confirmed_verifications")
        .unwrap_or(false);
    let latest_verified_reserve_updates = relation_row
        .try_get::<bool, _>("latest_verified_reserve_updates")
        .unwrap_or(false);
    let relations = json!({
        "migrationLedger": migration_ledger,
        "supportedReserves": supported_reserves,
        "reserveUpdates": reserve_updates,
        "reserveCurrentStates": reserve_current_states,
        "reserveConfirmedObservationIdSequence": reserve_confirmed_observation_id_sequence,
        "reserveConfirmedObservationFloors": reserve_confirmed_observation_floors,
        "reserveConfirmedVerifications": reserve_confirmed_verifications,
        "latestVerifiedReserveUpdates": latest_verified_reserve_updates,
    });

    let applied_rows = if migration_ledger {
        sqlx::query(
            r#"
            SELECT name, checksum, applied_at
            FROM loyal.timescale_schema_migrations
            WHERE version = $1
            "#,
        )
        .bind(TIMESCALE_MARKET_MIGRATION_VERSION)
        .fetch_all(&pool)
        .await
        .ok()
    } else {
        None
    };
    let applied_row_count = applied_rows
        .as_ref()
        .and_then(|rows| i64::try_from(rows.len()).ok())
        .unwrap_or(0);
    let applied_name = applied_rows.as_ref().and_then(|rows| {
        (rows.len() == 1)
            .then(|| rows[0].try_get::<String, _>("name").ok())
            .flatten()
    });
    let applied_checksum = applied_rows.as_ref().and_then(|rows| {
        (rows.len() == 1)
            .then(|| rows[0].try_get::<String, _>("checksum").ok())
            .flatten()
    });
    let applied_at = applied_rows.as_ref().and_then(|rows| {
        (rows.len() == 1)
            .then(|| rows[0].try_get::<DateTime<Utc>, _>("applied_at").ok())
            .flatten()
    });
    let migration = json!({
        "version": TIMESCALE_MARKET_MIGRATION_VERSION,
        "expectedName": TIMESCALE_MARKET_MIGRATION_NAME,
        "sourceChecksum": source_checksum,
        "appliedRowCount": applied_row_count,
        "appliedName": applied_name,
        "appliedChecksum": applied_checksum,
        "appliedAt": applied_at,
    });
    let all_relations_exist = migration_ledger
        && supported_reserves
        && reserve_updates
        && reserve_current_states
        && reserve_confirmed_observation_id_sequence
        && reserve_confirmed_observation_floors
        && reserve_confirmed_verifications
        && latest_verified_reserve_updates;
    if !all_relations_exist {
        return market_timescale_unavailable(
            captured_at,
            &enabled_mints,
            relations,
            migration,
            "required Timescale market-data relation is missing",
        );
    }

    let mut transaction = match pool.begin().await {
        Ok(transaction) => transaction,
        Err(_) => {
            return market_timescale_unavailable(
                captured_at,
                &enabled_mints,
                relations,
                migration,
                "Timescale market snapshot transaction failed",
            )
        }
    };
    if sqlx::query("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY")
        .execute(&mut *transaction)
        .await
        .is_err()
    {
        return market_timescale_unavailable(
            captured_at,
            &enabled_mints,
            relations,
            migration,
            "Timescale market snapshot fence failed",
        );
    }
    let statement_timeout_sql =
        format!("SET LOCAL statement_timeout = '{MARKET_EVIDENCE_QUERY_TIMEOUT_MILLISECONDS}ms'");
    if sqlx::query(&statement_timeout_sql)
        .execute(&mut *transaction)
        .await
        .is_err()
        || sqlx::query("SET LOCAL lock_timeout = '2000ms'")
            .execute(&mut *transaction)
            .await
            .is_err()
    {
        return market_timescale_unavailable(
            captured_at,
            &enabled_mints,
            relations,
            migration,
            "Timescale market query timeout fence failed",
        );
    }
    let captured_at = match sqlx::query_scalar::<_, DateTime<Utc>>("SELECT clock_timestamp()")
        .fetch_one(&mut *transaction)
        .await
    {
        Ok(captured_at) => captured_at,
        Err(_) => {
            return market_timescale_unavailable(
                captured_at,
                &enabled_mints,
                relations,
                migration,
                "Timescale market snapshot clock failed",
            )
        }
    };

    let coverage_query_started = Instant::now();
    let counters = sqlx::query(
        r#"
        WITH active_catalog AS MATERIALIZED (
            SELECT market, liquidity_mint, reserve, source, fetched_at
            FROM kamino.supported_reserves
            WHERE active
        ), active AS MATERIALIZED (
            SELECT reserve
            FROM active_catalog
            GROUP BY reserve
        ), evidence AS (
            SELECT active.reserve,
                   current_state.state_event_id,
                   current_state.account_data_hash AS current_hash,
                   current_state.state_slot,
                   current_state.state_observed_at,
                   current_state.state_source,
                   observation_floor.reserve AS observation_floor_reserve,
                   observation_floor.floor_slot AS observation_floor_slot,
                   observation_floor.observation_id AS observation_floor_observation_id,
                   observation_floor.account_data_hash AS observation_floor_hash,
                   observation_floor.state_valid AS observation_floor_state_valid,
                   observation_floor.source AS observation_floor_source,
                   observation_floor.source_rank AS observation_floor_source_rank,
                   observation_floor.observed_at AS observation_floor_observed_at,
                   verification.state_event_id AS verification_event_id,
                   verification.account_data_hash AS verification_hash,
                   verification.verified_slot,
                   verification.verified_at,
                   verification.commitment,
                   verification.verification_source,
                   state.exact_row_count AS event_exact_row_count,
                   state.event_id AS event_id,
                   state.account_data_hash AS event_hash,
                   state.observed_at AS event_observed_at,
                   state.slot AS event_slot,
                   state.source AS event_source,
                   state.source_commitment AS event_commitment,
                   latest.exact_row_count AS latest_exact_row_count,
                   latest.event_id AS latest_event_id,
                   latest.account_data_hash AS latest_hash,
                   latest.observed_at AS latest_observed_at,
                   latest.slot AS latest_state_slot,
                   latest.verified_slot AS latest_verified_slot,
                   latest.verified_at AS latest_verified_at,
                   latest.verification_commitment AS latest_commitment,
                   latest.source AS latest_state_source,
                   latest.verification_source AS latest_verification_source
            FROM active
            LEFT JOIN kamino.reserve_current_states current_state
              ON current_state.reserve = active.reserve
            LEFT JOIN kamino.reserve_confirmed_observation_floors observation_floor
              ON observation_floor.reserve = active.reserve
            LEFT JOIN kamino.reserve_confirmed_verifications verification
              ON verification.reserve = active.reserve
            LEFT JOIN LATERAL (
                SELECT count(*)::BIGINT AS exact_row_count,
                       min(candidate.event_id) AS event_id,
                       min(candidate.account_data_hash) AS account_data_hash,
                       min(candidate.observed_at) AS observed_at,
                       min(candidate.slot) AS slot,
                       min(candidate.source) AS source,
                       min(candidate.source_commitment) AS source_commitment
                FROM (
                    SELECT state.event_id, state.account_data_hash,
                           state.observed_at, state.slot, state.source,
                           state.source_commitment
                    FROM kamino.reserve_updates state
                    WHERE state.reserve = current_state.reserve
                      AND state.event_id = current_state.state_event_id
                    LIMIT 2
                ) candidate
            ) state ON true
            LEFT JOIN LATERAL (
                SELECT count(*)::BIGINT AS exact_row_count,
                       min(candidate.event_id) AS event_id,
                       min(candidate.account_data_hash) AS account_data_hash,
                       min(candidate.observed_at) AS observed_at,
                       min(candidate.slot) AS slot,
                       min(candidate.verified_slot) AS verified_slot,
                       min(candidate.verified_at) AS verified_at,
                       min(candidate.verification_commitment) AS verification_commitment,
                       min(candidate.source) AS source,
                       min(candidate.verification_source) AS verification_source
                FROM (
                    SELECT latest.event_id, latest.account_data_hash,
                           latest.observed_at, latest.slot, latest.verified_slot,
                           latest.verified_at, latest.verification_commitment,
                           latest.source, latest.verification_source
                    FROM kamino.latest_verified_reserve_updates latest
                    WHERE latest.reserve = active.reserve
                    LIMIT 2
                ) candidate
            ) latest ON true
        )
        SELECT count(*)::BIGINT AS active_count,
               (SELECT count(*)::BIGINT FROM active_catalog)
                   AS active_catalog_row_count,
               (SELECT count(*)::BIGINT
                FROM (
                    SELECT reserve
                    FROM active_catalog
                    GROUP BY reserve
                    HAVING count(*) <> 1
                ) duplicate_reserves)
                   AS duplicate_active_supported_reserve_count,
               (SELECT count(*)::BIGINT
                FROM active_catalog
                WHERE source IS DISTINCT FROM 'kamino-api')
                   AS non_kamino_api_active_supported_reserve_count,
               (SELECT count(*)::BIGINT
                FROM active_catalog
                WHERE fetched_at < $1 - make_interval(secs => $4::INTEGER))
                   AS stale_active_supported_reserve_count,
               (SELECT min(fetched_at) FROM active_catalog)
                   AS oldest_active_supported_reserve_fetched_at,
               (SELECT floor(max(extract(epoch FROM ($1 - fetched_at))))::BIGINT
                FROM active_catalog)
                   AS oldest_active_supported_reserve_age_seconds,
               count(*) FILTER (WHERE state_event_id IS NOT NULL)::BIGINT
                   AS current_pointer_count,
               count(*) FILTER (WHERE verification_event_id IS NOT NULL)::BIGINT
                   AS verification_count,
               count(*) FILTER (WHERE latest_exact_row_count = 1)::BIGINT
                   AS latest_view_count,
               count(*) FILTER (
                   WHERE state_event_id IS NOT NULL
                     AND event_exact_row_count IS DISTINCT FROM 1
               )::BIGINT AS immutable_tape_exact_row_cardinality_violation_count,
               count(*) FILTER (
                   WHERE latest_exact_row_count IS DISTINCT FROM 1
               )::BIGINT AS latest_view_row_cardinality_violation_count,
               count(*) FILTER (
                   WHERE state_event_id IS NOT NULL
                     AND (event_exact_row_count IS DISTINCT FROM 1
                       OR event_id IS NULL
                       OR event_hash IS DISTINCT FROM current_hash
                       OR event_observed_at IS DISTINCT FROM state_observed_at
                       OR event_slot IS DISTINCT FROM state_slot
                       OR event_source IS DISTINCT FROM state_source)
               )::BIGINT AS event_identity_violation_count,
               count(*) FILTER (
                   WHERE verification_event_id IS NOT NULL
                     AND (state_event_id IS NULL
                       OR verification_event_id IS DISTINCT FROM state_event_id
                       OR verification_hash IS DISTINCT FROM current_hash)
               )::BIGINT AS verification_identity_violation_count,
               count(*) FILTER (
                   WHERE latest_exact_row_count > 0
                     AND (latest_exact_row_count IS DISTINCT FROM 1
                       OR latest_event_id IS DISTINCT FROM state_event_id
                       OR latest_hash IS DISTINCT FROM current_hash
                       OR latest_observed_at IS DISTINCT FROM state_observed_at
                       OR latest_state_slot IS DISTINCT FROM state_slot
                       OR latest_verified_slot IS DISTINCT FROM verified_slot
                       OR latest_verified_at IS DISTINCT FROM verified_at
                       OR latest_commitment IS DISTINCT FROM commitment
                       OR latest_state_source IS DISTINCT FROM state_source
                       OR latest_verification_source IS DISTINCT FROM verification_source)
               )::BIGINT AS latest_identity_violation_count,
               count(*) FILTER (
                   WHERE state_slot IS NOT NULL AND verified_slot IS NOT NULL
                     AND state_slot > verified_slot
               )::BIGINT AS state_slot_after_verification_count,
               count(*) FILTER (WHERE observation_floor_reserve IS NOT NULL)::BIGINT
                   AS observation_floor_count,
               count(*) FILTER (
                   WHERE observation_floor_reserve IS NOT NULL
                     AND (observation_floor_slot < 0
                       OR observation_floor_observation_id IS NULL
                       OR observation_floor_observation_id <= 0
                       OR observation_floor_source IS NULL
                       OR observation_floor_source NOT IN (
                            'http_snapshot', 'http_confirmed_refresh',
                            'laserstream_grpc', 'websocket'
                       )
                       OR observation_floor_source_rank IS NULL
                       OR observation_floor_source_rank <> CASE
                            WHEN observation_floor_source IN (
                                'http_snapshot', 'http_confirmed_refresh'
                            ) THEN 2
                            WHEN observation_floor_source IN (
                                'laserstream_grpc', 'websocket'
                            ) THEN 1
                            ELSE 0
                          END
                       OR observation_floor_observed_at IS NULL
                       OR (observation_floor_state_valid AND (
                            observation_floor_hash IS NULL
                            OR observation_floor_hash !~ '^[0-9a-fA-F]{64}$'
                       ))
                       OR (NOT observation_floor_state_valid
                           AND observation_floor_hash IS NOT NULL))
               )::BIGINT AS observation_floor_identity_violation_count,
               count(*) FILTER (
                   WHERE observation_floor_observed_at > $1
               )::BIGINT AS observation_floor_future_count,
               count(*) FILTER (
                   WHERE observation_floor_reserve IS NOT NULL
                     AND observation_floor_observed_at
                         < $1 - make_interval(secs => $2::INTEGER)
               )::BIGINT AS stale_observation_floor_count,
               count(*) FILTER (
                   WHERE observation_floor_reserve IS NOT NULL
                     AND state_event_id IS NOT NULL
                     AND state_slot < observation_floor_slot
               )::BIGINT AS current_state_below_observation_floor_count,
               count(*) FILTER (
                   WHERE observation_floor_reserve IS NOT NULL
                     AND verification_event_id IS NOT NULL
                     AND verified_slot <= observation_floor_slot
                     AND observation_floor_state_valid
                     AND observation_floor_hash = current_hash
               )::BIGINT AS at_or_below_floor_exact_hash_admission_count,
               count(*) FILTER (
                   WHERE observation_floor_reserve IS NOT NULL
                     AND verification_event_id IS NOT NULL
                     AND verified_slot <= observation_floor_slot
                     AND NOT (
                          observation_floor_state_valid
                      AND observation_floor_hash = current_hash
                     )
               )::BIGINT AS verification_at_or_below_floor_without_exact_hash_count,
               count(*) FILTER (
                   WHERE observation_floor_reserve IS NOT NULL
                     AND latest_event_id IS NOT NULL
                     AND latest_verified_slot <= observation_floor_slot
                     AND NOT (
                          observation_floor_state_valid
                      AND observation_floor_hash = latest_hash
                     )
               )::BIGINT AS conflicting_at_or_below_floor_routable_state_count,
               count(*) FILTER (
                   WHERE observation_floor_reserve IS NOT NULL
                     AND NOT observation_floor_state_valid
               )::BIGINT AS invalid_observation_floor_state_count,
               count(*) FILTER (
                   WHERE (state_event_id IS NOT NULL
                          AND event_commitment IS DISTINCT FROM 'confirmed')
                      OR (verification_event_id IS NOT NULL
                          AND commitment IS DISTINCT FROM 'confirmed')
               )::BIGINT AS non_confirmed_count,
               count(*) FILTER (
                   WHERE state_event_id IS NOT NULL
                     AND (state_source IS NULL OR NOT (state_source = ANY(
                         ARRAY['http_snapshot', 'http_confirmed_refresh']::TEXT[]
                     )))
               )::BIGINT AS non_http_current_state_count,
               count(*) FILTER (
                   WHERE verification_event_id IS NOT NULL
                     AND (verification_source IS NULL
                       OR NOT (verification_source = ANY(
                            ARRAY['http_snapshot', 'http_confirmed_refresh']::TEXT[]
                       )))
               )::BIGINT AS non_http_verification_source_count,
               count(*) FILTER (WHERE state_observed_at > $1)::BIGINT
                   AS future_current_state_count,
               count(*) FILTER (WHERE verified_at > $1)::BIGINT
                   AS future_verification_count,
               count(*) FILTER (
                   WHERE verified_at < $1 - make_interval(secs => $2::INTEGER)
               )::BIGINT AS warning_expired_count,
               count(*) FILTER (
                   WHERE verified_at < $1 - make_interval(secs => $3::INTEGER)
               )::BIGINT AS hard_expired_count,
               floor(max(extract(epoch FROM ($1 - verified_at))))::BIGINT
                   AS oldest_verification_age_seconds
        FROM evidence
        "#,
    )
    .bind(captured_at)
    .bind(MARKET_VERIFICATION_WARNING_SECONDS)
    .bind(MARKET_VERIFICATION_HARD_EXPIRY_SECONDS)
    .bind(SUPPORTED_RESERVE_CATALOG_MAX_AGE_SECONDS)
    .fetch_one(&mut *transaction)
    .await;
    let coverage_query_milliseconds =
        i64::try_from(coverage_query_started.elapsed().as_millis()).unwrap_or(i64::MAX);
    let Ok(counters) = counters else {
        return market_timescale_unavailable(
            captured_at,
            &enabled_mints,
            relations,
            migration,
            "Timescale market coverage query failed",
        );
    };

    let safe_target_query_started = Instant::now();
    let targets = sqlx::query(
        r#"
        WITH enabled AS (
            SELECT enabled_mint.liquidity_mint
            FROM unnest($1::TEXT[]) AS enabled_mint(liquidity_mint)
        ), ranked AS (
            SELECT supported.liquidity_mint, supported.risk_baskets,
                   latest.reserve, latest.market, latest.supply_apy,
                   latest.total_supply_usd_estimate,
                   latest.reserve_last_update_stale,
                   latest.event_id AS state_event_id,
                   latest.account_data_hash,
                   latest.observed_at AS state_observed_at,
                   latest.slot AS state_slot,
                   latest.verified_at, latest.verified_slot,
                   latest.source AS state_source,
                   latest.verification_commitment,
                   latest.verification_source,
                   observation_floor.floor_slot AS observation_floor_slot,
                   observation_floor.observation_id AS observation_floor_observation_id,
                   observation_floor.account_data_hash AS observation_floor_hash,
                   observation_floor.state_valid AS observation_floor_state_valid,
                   observation_floor.source AS observation_floor_source,
                   observation_floor.source_rank AS observation_floor_source_rank,
                   observation_floor.observed_at AS observation_floor_observed_at,
                   count(*) OVER (PARTITION BY supported.liquidity_mint)::BIGINT
                       AS eligible_target_count,
                   row_number() OVER (
                       PARTITION BY supported.liquidity_mint
                       ORDER BY latest.supply_apy DESC,
                                latest.total_supply_usd_estimate DESC,
                                latest.reserve ASC
                   ) AS target_rank
            FROM kamino.supported_reserves supported
            JOIN kamino.latest_verified_reserve_updates latest
              ON latest.reserve = supported.reserve
             AND latest.market = supported.market
             AND latest.liquidity_mint = supported.liquidity_mint
            JOIN kamino.reserve_confirmed_observation_floors observation_floor
              ON observation_floor.reserve = supported.reserve
            WHERE supported.active
              AND supported.liquidity_mint = ANY($1::TEXT[])
              AND 'safe' = ANY(supported.risk_baskets)
              AND latest.total_supply_usd_estimate > 100000.0
              AND latest.supply_apy >= 0.0
              AND latest.supply_apy < 0.5
              AND NOT latest.reserve_last_update_stale
              AND latest.verified_at <= $2
              AND latest.verified_at >= $2 - make_interval(secs => $3::INTEGER)
              AND latest.verification_commitment = 'confirmed'
              AND latest.slot <= latest.verified_slot
              AND (
                    latest.verified_slot > observation_floor.floor_slot
                 OR (
                        observation_floor.state_valid
                    AND observation_floor.account_data_hash = latest.account_data_hash
                 )
              )
        )
        SELECT enabled.liquidity_mint,
               COALESCE(ranked.eligible_target_count, 0)::BIGINT
                   AS eligible_target_count,
               ranked.reserve, ranked.market, ranked.supply_apy,
               ranked.risk_baskets, ranked.total_supply_usd_estimate,
               ranked.reserve_last_update_stale, ranked.state_event_id,
               ranked.account_data_hash, ranked.state_observed_at,
               ranked.state_slot, ranked.verified_at, ranked.verified_slot,
               ranked.state_source, ranked.verification_commitment,
               ranked.verification_source, ranked.observation_floor_slot,
               ranked.observation_floor_observation_id,
               ranked.observation_floor_hash, ranked.observation_floor_state_valid,
               ranked.observation_floor_source, ranked.observation_floor_source_rank,
               ranked.observation_floor_observed_at
        FROM enabled
        LEFT JOIN ranked
          ON ranked.liquidity_mint = enabled.liquidity_mint
         AND ranked.target_rank = 1
        ORDER BY enabled.liquidity_mint
        "#,
    )
    .bind(&enabled_mints)
    .bind(captured_at)
    .bind(MARKET_VERIFICATION_WARNING_SECONDS)
    .fetch_all(&mut *transaction)
    .await;
    let safe_target_query_milliseconds =
        i64::try_from(safe_target_query_started.elapsed().as_millis()).unwrap_or(i64::MAX);
    let Ok(targets) = targets else {
        return market_timescale_unavailable(
            captured_at,
            &enabled_mints,
            relations,
            migration,
            "Timescale safe-target query failed",
        );
    };
    let _ = transaction.rollback().await;
    let top_targets = targets
        .iter()
        .map(|row| {
            json!({
                "liquidityMint": row.try_get::<String, _>("liquidity_mint").ok(),
                "eligibleTargetCount": row.try_get::<i64, _>("eligible_target_count").ok(),
                "riskBaskets": row.try_get::<Option<Vec<String>>, _>("risk_baskets").ok().flatten(),
                "reserve": row.try_get::<Option<String>, _>("reserve").ok().flatten(),
                "market": row.try_get::<Option<String>, _>("market").ok().flatten(),
                "supplyApy": row.try_get::<Option<f64>, _>("supply_apy").ok().flatten(),
                "totalSupplyUsdEstimate": row.try_get::<Option<f64>, _>("total_supply_usd_estimate").ok().flatten(),
                "reserveLastUpdateStale": row.try_get::<Option<bool>, _>("reserve_last_update_stale").ok().flatten(),
                "stateEventId": row.try_get::<Option<i64>, _>("state_event_id").ok().flatten(),
                "accountDataHash": row.try_get::<Option<String>, _>("account_data_hash").ok().flatten(),
                "stateObservedAt": row.try_get::<Option<DateTime<Utc>>, _>("state_observed_at").ok().flatten(),
                "stateSlot": row.try_get::<Option<i64>, _>("state_slot").ok().flatten(),
                "verifiedAt": row.try_get::<Option<DateTime<Utc>>, _>("verified_at").ok().flatten(),
                "verifiedSlot": row.try_get::<Option<i64>, _>("verified_slot").ok().flatten(),
                "stateSource": row.try_get::<Option<String>, _>("state_source").ok().flatten(),
                "verificationCommitment": row.try_get::<Option<String>, _>("verification_commitment").ok().flatten(),
                "verificationSource": row.try_get::<Option<String>, _>("verification_source").ok().flatten(),
                "observationFloorSlot": row.try_get::<Option<i64>, _>("observation_floor_slot").ok().flatten(),
                "observationFloorObservationId": row.try_get::<Option<i64>, _>("observation_floor_observation_id").ok().flatten(),
                "observationFloorAccountDataHash": row.try_get::<Option<String>, _>("observation_floor_hash").ok().flatten(),
                "observationFloorStateValid": row.try_get::<Option<bool>, _>("observation_floor_state_valid").ok().flatten(),
                "observationFloorSource": row.try_get::<Option<String>, _>("observation_floor_source").ok().flatten(),
                "observationFloorSourceRank": row.try_get::<Option<i16>, _>("observation_floor_source_rank").ok().flatten(),
                "observationFloorObservedAt": row.try_get::<Option<DateTime<Utc>>, _>("observation_floor_observed_at").ok().flatten(),
            })
        })
        .collect::<Vec<_>>();

    let active_count = counters.try_get::<i64, _>("active_count").ok();
    let active_catalog_row_count = counters.try_get::<i64, _>("active_catalog_row_count").ok();
    let duplicate_active_supported_reserves = counters
        .try_get::<i64, _>("duplicate_active_supported_reserve_count")
        .ok();
    let non_kamino_api_active_supported_reserves = counters
        .try_get::<i64, _>("non_kamino_api_active_supported_reserve_count")
        .ok();
    let stale_active_supported_reserves = counters
        .try_get::<i64, _>("stale_active_supported_reserve_count")
        .ok();
    let oldest_active_supported_reserve_fetched_at = counters
        .try_get::<Option<DateTime<Utc>>, _>("oldest_active_supported_reserve_fetched_at")
        .ok()
        .flatten();
    let oldest_active_supported_reserve_age_seconds = counters
        .try_get::<Option<i64>, _>("oldest_active_supported_reserve_age_seconds")
        .ok()
        .flatten();
    let current_pointer_count = counters.try_get::<i64, _>("current_pointer_count").ok();
    let verification_count = counters.try_get::<i64, _>("verification_count").ok();
    let latest_view_count = counters.try_get::<i64, _>("latest_view_count").ok();
    let event_identity_violations = counters
        .try_get::<i64, _>("event_identity_violation_count")
        .ok();
    let verification_identity_violations = counters
        .try_get::<i64, _>("verification_identity_violation_count")
        .ok();
    let latest_identity_violations = counters
        .try_get::<i64, _>("latest_identity_violation_count")
        .ok();
    let slot_violations = counters
        .try_get::<i64, _>("state_slot_after_verification_count")
        .ok();
    let immutable_tape_cardinality_violations = counters
        .try_get::<i64, _>("immutable_tape_exact_row_cardinality_violation_count")
        .ok();
    let latest_view_cardinality_violations = counters
        .try_get::<i64, _>("latest_view_row_cardinality_violation_count")
        .ok();
    let observation_floor_count = counters.try_get::<i64, _>("observation_floor_count").ok();
    let observation_floor_identity_violations = counters
        .try_get::<i64, _>("observation_floor_identity_violation_count")
        .ok();
    let observation_floor_future = counters
        .try_get::<i64, _>("observation_floor_future_count")
        .ok();
    let stale_observation_floors = counters
        .try_get::<i64, _>("stale_observation_floor_count")
        .ok();
    let invalid_observation_floor_states = counters
        .try_get::<i64, _>("invalid_observation_floor_state_count")
        .ok();
    let current_states_below_floor = counters
        .try_get::<i64, _>("current_state_below_observation_floor_count")
        .ok();
    let exact_hash_admissions = counters
        .try_get::<i64, _>("at_or_below_floor_exact_hash_admission_count")
        .ok();
    let at_or_below_floor_verification_violations = counters
        .try_get::<i64, _>("verification_at_or_below_floor_without_exact_hash_count")
        .ok();
    let at_or_below_floor_routable_violations = counters
        .try_get::<i64, _>("conflicting_at_or_below_floor_routable_state_count")
        .ok();
    let non_confirmed = counters.try_get::<i64, _>("non_confirmed_count").ok();
    let non_http = counters
        .try_get::<i64, _>("non_http_current_state_count")
        .ok();
    let non_http_verifications = counters
        .try_get::<i64, _>("non_http_verification_source_count")
        .ok();
    let future_current_states = counters
        .try_get::<i64, _>("future_current_state_count")
        .ok();
    let future = counters.try_get::<i64, _>("future_verification_count").ok();
    let warning = counters.try_get::<i64, _>("warning_expired_count").ok();
    let hard = counters.try_get::<i64, _>("hard_expired_count").ok();
    let oldest_age = counters
        .try_get::<Option<i64>, _>("oldest_verification_age_seconds")
        .ok()
        .flatten();
    let targets_complete = top_targets.len() == enabled_mints.len()
        && top_targets.iter().all(|target| {
            target
                .get("eligibleTargetCount")
                .and_then(Value::as_i64)
                .is_some_and(|count| count > 0)
        });
    let migration_matches = applied_row_count == 1
        && applied_name.as_deref() == Some(TIMESCALE_MARKET_MIGRATION_NAME)
        && applied_checksum.as_deref() == Some(source_checksum.as_str())
        && applied_at.is_some();
    let pass = migration_matches
        && active_count.is_some_and(|count| count > 0)
        && active_catalog_row_count == active_count
        && duplicate_active_supported_reserves == Some(0)
        && non_kamino_api_active_supported_reserves == Some(0)
        && stale_active_supported_reserves == Some(0)
        && oldest_active_supported_reserve_fetched_at
            .is_some_and(|fetched_at| fetched_at <= captured_at)
        && oldest_active_supported_reserve_age_seconds
            .is_some_and(|age| (0..=SUPPORTED_RESERVE_CATALOG_MAX_AGE_SECONDS).contains(&age))
        && current_pointer_count == active_count
        && verification_count == active_count
        && latest_view_count == active_count
        && observation_floor_count == active_count
        && event_identity_violations == Some(0)
        && verification_identity_violations == Some(0)
        && latest_identity_violations == Some(0)
        && slot_violations == Some(0)
        && immutable_tape_cardinality_violations == Some(0)
        && latest_view_cardinality_violations == Some(0)
        && observation_floor_identity_violations == Some(0)
        && observation_floor_future == Some(0)
        && stale_observation_floors == Some(0)
        && invalid_observation_floor_states == Some(0)
        && at_or_below_floor_verification_violations == Some(0)
        && at_or_below_floor_routable_violations == Some(0)
        && non_confirmed == Some(0)
        && non_http == Some(0)
        && non_http_verifications == Some(0)
        && future_current_states == Some(0)
        && future == Some(0)
        && hard == Some(0)
        && oldest_age.is_some_and(|age| (0..=MARKET_VERIFICATION_WARNING_SECONDS).contains(&age))
        && (0..=MARKET_EVIDENCE_QUERY_TIMEOUT_MILLISECONDS).contains(&coverage_query_milliseconds)
        && (0..=MARKET_EVIDENCE_QUERY_TIMEOUT_MILLISECONDS)
            .contains(&safe_target_query_milliseconds)
        && targets_complete;
    MarketDataPlaneEvidence {
        timescale: json!({
            "available": true,
            "capturedAt": captured_at,
            "migration": migration,
            "relations": relations,
            "enabledStableMints": enabled_mints,
            "activeDistinctSupportedReserveCount": active_count,
            "activeSupportedReserveCatalogRowCount": active_catalog_row_count,
            "duplicateActiveSupportedReserveCount": duplicate_active_supported_reserves,
            "nonKaminoApiActiveSupportedReserveCount": non_kamino_api_active_supported_reserves,
            "staleActiveSupportedReserveOver300SecondsCount": stale_active_supported_reserves,
            "oldestActiveSupportedReserveFetchedAt": oldest_active_supported_reserve_fetched_at,
            "oldestActiveSupportedReserveAgeSeconds": oldest_active_supported_reserve_age_seconds,
            "currentPointerCoverageCount": current_pointer_count,
            "verificationCoverageCount": verification_count,
            "exactLatestViewCoverageCount": latest_view_count,
            "eventHashObservedAtIdentityViolationCount": event_identity_violations,
            "verificationStateIdentityViolationCount": verification_identity_violations,
            "latestViewIdentityViolationCount": latest_identity_violations,
            "stateSlotGreaterThanVerifiedSlotCount": slot_violations,
            "immutableTapeExactRowCardinalityViolationCount": immutable_tape_cardinality_violations,
            "latestViewRowCardinalityViolationCount": latest_view_cardinality_violations,
            "observationFloorCoverageCount": observation_floor_count,
            "observationFloorIdentityViolationCount": observation_floor_identity_violations,
            "observationFloorFutureObservedAtCount": observation_floor_future,
            "staleObservationFloorOver90SecondsCount": stale_observation_floors,
            "invalidObservationFloorStateCount": invalid_observation_floor_states,
            "currentStateBelowObservationFloorCount": current_states_below_floor,
            "atOrBelowFloorExactHashAdmissionCount": exact_hash_admissions,
            "verificationAtOrBelowObservationFloorWithoutExactHashCount": at_or_below_floor_verification_violations,
            "conflictingAtOrBelowFloorRoutableStateCount": at_or_below_floor_routable_violations,
            "nonConfirmedCommitmentCount": non_confirmed,
            "nonHttpCurrentStateCount": non_http,
            "nonHttpVerificationSourceCount": non_http_verifications,
            "futureCurrentStateObservedAtCount": future_current_states,
            "futureVerificationWatermarkCount": future,
            "warningOver90SecondsCount": warning,
            "hardExpiredOver240SecondsCount": hard,
            "oldestVerificationAgeSeconds": oldest_age,
            "coverageQueryMilliseconds": coverage_query_milliseconds,
            "safeTargetQueryMilliseconds": safe_target_query_milliseconds,
            "topVerifiedSafeTargets": top_targets,
            "readError": Value::Null,
            "pass": pass,
        }),
        pass,
    }
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
    // Exact live planner env evidence proves there is no production narrowing
    // override, so baseline membership must use the same code-owned default.
    let enabled_mints = supported_stable_mints();
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
        main_positions AS MATERIALIZED (
            SELECT position.*,
                   CASE
                       WHEN position.planning_metadata->>'amount_semantics' = $7
                            AND COALESCE(
                                NULLIF(position.planning_metadata->>'redeemable_source_liquidity_amount_raw', ''),
                                NULLIF(position.planning_metadata->>'redeemable_liquidity_amount_raw', '')
                            ) ~ '^[0-9]+$'
                       THEN COALESCE(
                           NULLIF(position.planning_metadata->>'redeemable_source_liquidity_amount_raw', ''),
                           NULLIF(position.planning_metadata->>'redeemable_liquidity_amount_raw', '')
                       )::BIGINT
                       WHEN position.planning_metadata->>'amount_semantics' = $8
                       THEN position.amount_raw
                       ELSE NULL
                   END AS routeable_liquidity_amount_raw
            FROM loyal_yield.vault_reserve_positions_current position
            WHERE position.reserve = $6
              AND position.liquidity_mint = $4
              AND position.has_value
              AND position.amount_raw > 0
        ),
        routeable_main AS MATERIALIZED (
            SELECT position.*
            FROM main_positions position
            JOIN main_usdc_cohort cohort ON cohort.vault_id = position.vault_id
            WHERE position.routeable_liquidity_amount_raw > 0
              AND (position.market IS NULL OR position.market = $5)
        ),
        unresolved_routeable_main AS MATERIALIZED (
            SELECT position.*
            FROM main_positions position
            JOIN main_usdc_cohort cohort ON cohort.vault_id = position.vault_id
            WHERE position.routeable_liquidity_amount_raw IS NULL
        ),
        global_main AS MATERIALIZED (
            SELECT position.*
            FROM main_positions position
            WHERE position.routeable_liquidity_amount_raw > 0
        ),
        unresolved_global_main AS MATERIALIZED (
            SELECT position.*
            FROM main_positions position
            WHERE position.routeable_liquidity_amount_raw IS NULL
        )
        SELECT
            (SELECT count(*)::BIGINT FROM main_usdc_cohort) AS cohort_vault_count,
            (SELECT COALESCE(array_agg(vault_id ORDER BY vault_id), ARRAY[]::BIGINT[])
             FROM main_usdc_cohort) AS cohort_vault_ids,
            (SELECT COALESCE(sum(routeable_liquidity_amount_raw), 0)::BIGINT FROM routeable_main)
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
            (SELECT count(*)::BIGINT FROM unresolved_routeable_main)
                AS routeable_unresolved_amount_semantics_count,
            (SELECT COALESCE(sum(routeable_liquidity_amount_raw), 0)::BIGINT FROM global_main)
                AS global_amount_raw,
            (SELECT count(*)::BIGINT FROM global_main) AS global_vault_count,
            (SELECT COALESCE(array_agg(vault_id ORDER BY vault_id), ARRAY[]::BIGINT[])
             FROM global_main) AS global_vault_ids,
            (SELECT min(observed_at) FROM global_main) AS global_oldest_observed_at,
            (SELECT max(observed_at) FROM global_main) AS global_newest_observed_at,
            (SELECT min(observed_slot) FROM global_main) AS global_minimum_observed_slot,
            (SELECT max(observed_slot) FROM global_main) AS global_maximum_observed_slot,
            (SELECT count(*)::BIGINT FROM global_main
             WHERE observed_at < now() - interval '10 minutes') AS global_stale_row_count,
            (SELECT count(*)::BIGINT FROM unresolved_global_main)
                AS global_unresolved_amount_semantics_count
        "#,
    )
    .bind(STANDARD_POLICY_AUTHORITY)
    .bind(SAME_MINT_ROUTE_MODE)
    .bind(&enabled_mints)
    .bind(&usdc_mint)
    .bind(&main_market)
    .bind(&main_reserve)
    .bind(AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED)
    .bind(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY)
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
    let routeable_unresolved_amount_semantics_count =
        positions.try_get::<i64, _>("routeable_unresolved_amount_semantics_count")?;
    let global_amount_raw = positions.try_get::<i64, _>("global_amount_raw")?;
    let global_stale_row_count = positions.try_get::<i64, _>("global_stale_row_count")?;
    let global_unresolved_amount_semantics_count =
        positions.try_get::<i64, _>("global_unresolved_amount_semantics_count")?;
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
            "freshForBaseline": routeable_stale_row_count == 0
                && routeable_unresolved_amount_semantics_count == 0,
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
            "freshForBaseline": global_stale_row_count == 0
                && global_unresolved_amount_semantics_count == 0,
        },
        "reserveAggregates": reserve_aggregates,
    }))
}

async fn collect_largest_account_evidence(
    pool: &PgPool,
    cluster: &str,
    cutover_at: Option<DateTime<Utc>>,
) -> Result<Value, Box<dyn Error>> {
    let enabled_mints = supported_stable_mints();
    Ok(sqlx::query_scalar(
        r#"
        WITH planning AS MATERIALIZED (
            SELECT state.*
            FROM loyal_yield.fleet_planning_state state
            WHERE state.cluster = $1
        ),
        current_epoch AS MATERIALIZED (
            SELECT epoch.*
            FROM planning
            JOIN loyal_yield.optimizer_epochs epoch
              ON epoch.cluster = planning.cluster
             AND epoch.epoch_key = planning.optimizer_epoch_key
        ),
        epoch_reserves AS MATERIALIZED (
            SELECT
                reserve.value->>'reserve' AS reserve,
                reserve.value->>'market' AS market,
                reserve.value->>'liquidityMint' AS liquidity_mint,
                CASE WHEN reserve.value->>'mintDecimals' ~ '^[0-9]+$'
                     THEN (reserve.value->>'mintDecimals')::INTEGER END AS mint_decimals,
                CASE WHEN reserve.value->>'marketPriceUsdMicros' ~ '^[0-9]+$'
                     THEN (reserve.value->>'marketPriceUsdMicros')::BIGINT END
                    AS market_price_usd_micros,
                CASE WHEN reserve.value->>'supplyApyBps' ~ '^-?[0-9]+$'
                     THEN (reserve.value->>'supplyApyBps')::BIGINT END AS supply_apy_bps,
                COALESCE((reserve.value->>'targetEligible')::BOOLEAN, FALSE) AS target_eligible
            FROM current_epoch epoch
            CROSS JOIN LATERAL jsonb_array_elements(epoch.market_state->'reserves') reserve(value)
        ),
        eligible_vaults AS MATERIALIZED (
            SELECT vault.id AS vault_id, policy.kamino_markets,
                   policy.stable_mints, policy.kamino_liquidity_mints
            FROM loyal_yield.managed_vaults vault
            JOIN loyal_yield.route_policies policy ON policy.id = vault.active_policy_id
            WHERE vault.active AND policy.active
              AND $2 = ANY(policy.delegated_signers)
              AND $3 = ANY(policy.route_modes)
              AND policy.stable_mints && $4::TEXT[]
              AND policy.kamino_liquidity_mints && $4::TEXT[]
              AND cardinality(policy.kamino_markets) > 0
        ),
        routeable_positions AS MATERIALIZED (
            SELECT position.vault_id, position.reserve, position.market,
                   position.liquidity_mint, position.observed_slot,
                   position.observed_at,
                   CASE
                       WHEN position.planning_metadata->>'amount_semantics' = $5
                            AND COALESCE(
                                NULLIF(position.planning_metadata->>'redeemable_source_liquidity_amount_raw', ''),
                                NULLIF(position.planning_metadata->>'redeemable_liquidity_amount_raw', '')
                            ) ~ '^[0-9]+$'
                       THEN COALESCE(
                           NULLIF(position.planning_metadata->>'redeemable_source_liquidity_amount_raw', ''),
                           NULLIF(position.planning_metadata->>'redeemable_liquidity_amount_raw', '')
                       )::BIGINT
                       WHEN position.planning_metadata->>'amount_semantics' = $6
                       THEN position.amount_raw
                       ELSE NULL
                   END AS routeable_amount_raw,
                   eligible.kamino_markets, eligible.stable_mints,
                   eligible.kamino_liquidity_mints
            FROM loyal_yield.vault_reserve_positions_current position
            JOIN eligible_vaults eligible ON eligible.vault_id = position.vault_id
            WHERE position.has_value AND position.amount_raw > 0
              AND position.liquidity_mint = ANY(eligible.stable_mints)
              AND position.liquidity_mint = ANY(eligible.kamino_liquidity_mints)
              AND (position.market IS NULL OR position.market = ANY(eligible.kamino_markets))
        ),
        valued_positions AS MATERIALIZED (
            SELECT position.*,
                   market.supply_apy_bps,
                   (position.routeable_amount_raw::NUMERIC
                    * market.market_price_usd_micros::NUMERIC
                    / power(10::NUMERIC, market.mint_decimals))::BIGINT
                       AS principal_usd_micros
            FROM routeable_positions position
            JOIN epoch_reserves market
              ON market.reserve = position.reserve
             AND market.liquidity_mint = position.liquidity_mint
             AND (position.market IS NULL OR market.market = position.market)
            WHERE position.routeable_amount_raw > 0
              AND market.market_price_usd_micros > 0
              AND market.mint_decimals BETWEEN 0 AND 18
        ),
        vault_values AS MATERIALIZED (
            SELECT position.vault_id,
                   max(position.kamino_markets) AS kamino_markets,
                   max(position.stable_mints) AS stable_mints,
                   max(position.kamino_liquidity_mints) AS kamino_liquidity_mints,
                   sum(position.principal_usd_micros)::BIGINT AS principal_usd_micros,
                   min(position.observed_at) AS oldest_observed_at,
                   max(position.observed_at) AS newest_observed_at,
                   min(position.observed_slot) AS minimum_observed_slot,
                   max(position.observed_slot) AS maximum_observed_slot,
                   jsonb_agg(jsonb_build_object(
                       'reserve', position.reserve,
                       'market', position.market,
                       'liquidityMint', position.liquidity_mint,
                       'amountRaw', position.routeable_amount_raw,
                       'principalUsdMicros', position.principal_usd_micros,
                       'supplyApyBps', position.supply_apy_bps
                   ) ORDER BY position.principal_usd_micros DESC, position.reserve) AS positions
            FROM valued_positions position
            GROUP BY position.vault_id
        ),
        ranked AS MATERIALIZED (
            SELECT value.*, row_number() OVER (
                       ORDER BY value.principal_usd_micros DESC, value.vault_id
                   )::BIGINT AS rank
            FROM vault_values value
            ORDER BY value.principal_usd_micros DESC, value.vault_id
            LIMIT 10
        ),
        evidence AS MATERIALIZED (
            SELECT ranked.*,
                   best.reserve AS best_reserve,
                   best.market AS best_market,
                   best.liquidity_mint AS best_liquidity_mint,
                   best.supply_apy_bps AS best_supply_apy_bps,
                   opportunity.id AS opportunity_id,
                   opportunity.opportunity_state,
                   opportunity.target_reserve,
                   opportunity.estimated_edge_bps,
                   opportunity.expected_net_gain_usd_micros,
                   submission.id AS moved_submission_id,
                   submission.reconciled_at AS moved_reconciled_at,
                   COALESCE((SELECT sum((item->>'principalUsdMicros')::BIGINT)
                             FROM jsonb_array_elements(ranked.positions) item
                             WHERE item->>'reserve' = best.reserve), 0)::BIGINT
                       AS principal_at_best_reserve
            FROM ranked
            LEFT JOIN LATERAL (
                SELECT market.*
                FROM epoch_reserves market
                WHERE market.target_eligible
                  AND market.market = ANY(ranked.kamino_markets)
                  AND market.liquidity_mint = ANY(ranked.stable_mints)
                  AND market.liquidity_mint = ANY(ranked.kamino_liquidity_mints)
                ORDER BY market.supply_apy_bps DESC, market.reserve
                LIMIT 1
            ) best ON TRUE
            LEFT JOIN LATERAL (
                SELECT candidate.*
                FROM loyal_yield.rebalance_opportunities candidate
                JOIN current_epoch epoch ON epoch.id = candidate.optimizer_epoch_id
                WHERE candidate.cluster = $1 AND candidate.vault_id = ranked.vault_id
                ORDER BY candidate.attempt_generation DESC, candidate.id DESC
                LIMIT 1
            ) opportunity ON TRUE
            LEFT JOIN LATERAL (
                SELECT signed.id, signed.reconciled_at
                FROM loyal_yield.signed_route_submissions signed
                WHERE signed.opportunity_id = opportunity.id
                  AND signed.submission_state = 'reconciled'
                  AND $7::TIMESTAMPTZ IS NOT NULL
                  AND signed.reconciled_at >= $7
                  AND opportunity.target_reserve = best.reserve
                ORDER BY signed.id DESC
                LIMIT 1
            ) submission ON TRUE
        ),
        classified AS MATERIALIZED (
            SELECT evidence.*,
                   CASE
                       WHEN planning.cluster IS NULL OR current_epoch.id IS NULL
                            OR NOT planning.complete_frontier
                            OR planning.optimizer_epoch_expires_at <= now()
                            OR planning.full_sweep_completed_at < now() - interval '120 seconds'
                            OR evidence.oldest_observed_at < now() - interval '10 minutes'
                            OR evidence.best_reserve IS NULL
                           THEN 'blocked'
                       WHEN evidence.moved_submission_id IS NOT NULL THEN 'moved'
                       WHEN evidence.principal_at_best_reserve = evidence.principal_usd_micros
                           THEN 'already_optimal'
                       WHEN evidence.opportunity_id IS NULL THEN 'no_positive_edge'
                       ELSE 'blocked'
                   END AS classification
            FROM evidence
            LEFT JOIN planning ON TRUE
            LEFT JOIN current_epoch ON TRUE
        ),
        summary AS (
            SELECT count(*)::BIGINT AS ranked_count,
                   COALESCE(sum(principal_usd_micros), 0)::BIGINT AS ranked_principal,
                   COALESCE(sum(principal_usd_micros) FILTER (
                       WHERE classification <> 'blocked'), 0)::BIGINT AS covered_principal,
                   count(*) FILTER (WHERE rank <= 3 AND classification = 'blocked')::BIGINT
                       AS top_three_blocked_count,
                   count(*) FILTER (WHERE classification = 'moved')::BIGINT AS moved_count
            FROM classified
        )
        SELECT jsonb_build_object(
            'available', planning.cluster IS NOT NULL AND current_epoch.id IS NOT NULL,
            'cluster', $1,
            'cutoverAt', $7::TIMESTAMPTZ,
            'optimizerEpochId', current_epoch.id,
            'optimizerEpochKey', planning.optimizer_epoch_key,
            'optimizerEpochExpiresAt', planning.optimizer_epoch_expires_at,
            'fullSweepCompletedAt', planning.full_sweep_completed_at,
            'completeFrontier', planning.complete_frontier,
            'rankedCount', summary.ranked_count,
            'rankedPrincipalUsdMicros', summary.ranked_principal,
            'coveredPrincipalUsdMicros', summary.covered_principal,
            'coveragePpm', CASE WHEN summary.ranked_principal = 0 THEN 0 ELSE
                (1000000::NUMERIC * summary.covered_principal / summary.ranked_principal)::BIGINT END,
            'minimumCoveragePpm', 900000,
            'topThreeBlockedCount', summary.top_three_blocked_count,
            'movedCount', summary.moved_count,
            'vaults', COALESCE((SELECT jsonb_agg(jsonb_build_object(
                'rank', row.rank,
                'vaultId', row.vault_id,
                'principalUsdMicros', row.principal_usd_micros,
                'oldestObservedAt', row.oldest_observed_at,
                'newestObservedAt', row.newest_observed_at,
                'minimumObservedSlot', row.minimum_observed_slot,
                'maximumObservedSlot', row.maximum_observed_slot,
                'positions', row.positions,
                'bestReserve', row.best_reserve,
                'bestMarket', row.best_market,
                'bestLiquidityMint', row.best_liquidity_mint,
                'bestSupplyApyBps', row.best_supply_apy_bps,
                'principalAtBestReserve', row.principal_at_best_reserve,
                'opportunityId', row.opportunity_id,
                'opportunityState', row.opportunity_state,
                'opportunityTargetReserve', row.target_reserve,
                'estimatedEdgeBps', row.estimated_edge_bps,
                'expectedNetGainUsdMicros', row.expected_net_gain_usd_micros,
                'movedSubmissionId', row.moved_submission_id,
                'movedReconciledAt', row.moved_reconciled_at,
                'classification', row.classification
            ) ORDER BY row.rank) FROM classified row), '[]'::JSONB),
            'pass', $7::TIMESTAMPTZ IS NOT NULL
                AND summary.ranked_count > 0
                AND summary.top_three_blocked_count = 0
                AND summary.covered_principal * 10 >= summary.ranked_principal * 9
        )
        FROM summary
        LEFT JOIN planning ON TRUE
        LEFT JOIN current_epoch ON TRUE
        "#,
    )
    .bind(cluster)
    .bind(STANDARD_POLICY_AUTHORITY)
    .bind(SAME_MINT_ROUTE_MODE)
    .bind(&enabled_mints)
    .bind(AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED)
    .bind(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY)
    .bind(cutover_at)
    .fetch_one(pool)
    .await?)
}

async fn collect_queue_evidence(pool: &PgPool, cluster: &str) -> Result<Value, sqlx::Error> {
    sqlx::query_scalar(
        r#"
        SELECT jsonb_build_object(
            'available', true,
            'capturedAt', clock_timestamp(),
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
    let planner_accounting_exact = metric_i64(status, "planned_opportunity_count")
        .zip(metric_i64(status, "planned_selected_count"))
        .zip(metric_i64(status, "planned_deferred_count"))
        .is_some_and(|((planned, selected), deferred)| {
            planned >= 0
                && selected >= 0
                && deferred >= 0
                && selected.checked_add(deferred) == Some(planned)
        });
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
    if fresh_complete_epoch && planner_accounting_exact && stage_ages_bounded && counters_zero {
        Verdict::Pass
    } else {
        Verdict::Fail
    }
}

fn current_epoch_slo_measurements(queue: &Value) -> (Value, bool) {
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

fn bounded_movement_slo_measurements(movements: &[MovementRow]) -> (Value, bool) {
    const SUBMISSION_LIMIT_MILLISECONDS: i64 = 120_000;
    const RECONCILIATION_LIMIT_MILLISECONDS: i64 = 900_000;

    let reconciled = movements
        .iter()
        .filter(|movement| movement.submission_state == "reconciled")
        .collect::<Vec<_>>();
    let signed_to_submitted = reconciled
        .iter()
        .filter_map(|movement| {
            movement
                .submitted_at
                .map(|submitted| (submitted - movement.created_at).num_milliseconds())
        })
        .collect::<Vec<_>>();
    let signed_to_reconciled = reconciled
        .iter()
        .filter_map(|movement| {
            movement
                .reconciled_at
                .map(|reconciled| (reconciled - movement.created_at).num_milliseconds())
        })
        .collect::<Vec<_>>();
    let maximum_submission_milliseconds = signed_to_submitted.iter().copied().max();
    let maximum_reconciliation_milliseconds = signed_to_reconciled.iter().copied().max();
    let pass = !reconciled.is_empty()
        && signed_to_submitted.len() == reconciled.len()
        && signed_to_reconciled.len() == reconciled.len()
        && signed_to_submitted
            .iter()
            .all(|millis| (0..=SUBMISSION_LIMIT_MILLISECONDS).contains(millis))
        && signed_to_reconciled
            .iter()
            .all(|millis| (0..=RECONCILIATION_LIMIT_MILLISECONDS).contains(millis));

    (
        json!({
            "basis": "post_cutover_reconciled_submissions",
            "reconciledMovementCount": reconciled.len(),
            "submissionTimestampCount": signed_to_submitted.len(),
            "reconciliationTimestampCount": signed_to_reconciled.len(),
            "maximumSignedToSubmittedMilliseconds": maximum_submission_milliseconds,
            "submissionLimitMilliseconds": SUBMISSION_LIMIT_MILLISECONDS,
            "maximumSignedToReconciledMilliseconds": maximum_reconciliation_milliseconds,
            "reconciliationLimitMilliseconds": RECONCILIATION_LIMIT_MILLISECONDS,
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
               optimizer_epoch.epoch_key AS optimizer_epoch_fingerprint,
               optimizer_epoch.expires_at AS optimizer_epoch_expires_at,
               COALESCE(
                   submission.alt_mutation_epochs->'optimizerEpoch',
                   'null'::JSONB
               ) AS submission_optimizer_epoch_evidence,
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
        JOIN loyal_yield.optimizer_epochs optimizer_epoch
          ON optimizer_epoch.id = opportunity.optimizer_epoch_id
         AND optimizer_epoch.cluster = opportunity.cluster
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
                optimizer_epoch_fingerprint: row.try_get("optimizer_epoch_fingerprint")?,
                optimizer_epoch_expires_at: row.try_get("optimizer_epoch_expires_at")?,
                submission_optimizer_epoch_evidence: row
                    .try_get("submission_optimizer_epoch_evidence")?,
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

fn same_mint_route_amount_evidence_exact(movement: &MovementRow) -> bool {
    let published_amount = Some(movement.amount_raw);
    let executed_amount = Some(movement.decision_amount_raw);
    let source_collateral = movement
        .pre_source_amount_raw
        .filter(|source_collateral| *source_collateral > 0);
    let bounded_positive_accrual = movement.amount_raw > 0
        && movement.decision_amount_raw >= movement.amount_raw
        && i128::from(movement.decision_amount_raw - movement.amount_raw) * 1_000_000
            <= i128::from(movement.amount_raw) * i128::from(MAX_QUEUE_POSITIVE_AMOUNT_DRIFT_PPM);
    bounded_positive_accrual
        && execution_string(&movement.execution_plan, "route_amount_semantics")
            == Some(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY)
        && execution_string(&movement.decision_execution_plan, "route_amount_semantics")
            == Some(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY)
        && execution_string(&movement.execution_plan, "source_amount_semantics")
            == Some(AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED)
        && execution_string(&movement.decision_execution_plan, "source_amount_semantics")
            == Some(AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED)
        && execution_i64(&movement.execution_plan, "source_collateral_amount_raw")
            == source_collateral
        && execution_i64(
            &movement.decision_execution_plan,
            "source_collateral_amount_raw",
        ) == source_collateral
        && execution_i64(
            &movement.execution_plan,
            "redeemable_source_liquidity_amount_raw",
        ) == published_amount
        && execution_i64(
            &movement.decision_execution_plan,
            "redeemable_source_liquidity_amount_raw",
        ) == executed_amount
}

fn idle_route_amount_evidence_exact(movement: &MovementRow) -> bool {
    let amount = Some(movement.amount_raw);
    execution_string(&movement.execution_plan, "route_amount_semantics")
        == Some(ROUTE_AMOUNT_SEMANTICS_IDLE_VAULT_LIQUIDITY)
        && execution_string(&movement.decision_execution_plan, "route_amount_semantics")
            == Some(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY)
        && execution_string(&movement.decision_execution_plan, "source_amount_semantics")
            == Some("idle_vault")
        && execution_i64(&movement.execution_plan, "idle_vault_liquidity_amount_raw") == amount
        && execution_i64(
            &movement.decision_execution_plan,
            "idle_vault_liquidity_amount_raw",
        ) == amount
}

fn movement_route_amount_evidence_exact(movement: &MovementRow) -> bool {
    match movement.route_kind.as_str() {
        "same_mint" => same_mint_route_amount_evidence_exact(movement),
        "idle_vault_deposit" => idle_route_amount_evidence_exact(movement),
        _ => false,
    }
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
        && same_mint_route_amount_evidence_exact(movement)
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
            == Some(movement.decision_amount_raw)
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
        && idle_route_amount_evidence_exact(movement)
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
    let signed_optimizer_epoch_id =
        execution_i64(&movement.submission_optimizer_epoch_evidence, "id");
    let signed_optimizer_epoch_fingerprint =
        execution_string(&movement.submission_optimizer_epoch_evidence, "fingerprint");
    let signed_optimizer_epoch_expires_at =
        execution_timestamp(&movement.submission_optimizer_epoch_evidence, "expiresAt");
    let optimizer_epoch_identity_exact = movement.opportunity_optimizer_epoch_id > 0
        && movement.opportunity_optimizer_epoch_id == movement.submission_optimizer_epoch_id
        && signed_optimizer_epoch_id == Some(movement.opportunity_optimizer_epoch_id)
        && signed_optimizer_epoch_fingerprint
            == Some(movement.optimizer_epoch_fingerprint.as_str())
        && signed_optimizer_epoch_expires_at == Some(movement.optimizer_epoch_expires_at);
    let reciprocal_identity_exact = movement.opportunity_decision_id == Some(movement.decision_id)
        && movement.decision_vault_id == movement.vault_id
        && optimizer_epoch_identity_exact;
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
        && match movement.route_kind.as_str() {
            "same_mint" => same_mint_route_amount_evidence_exact(movement),
            "idle_vault_deposit" => movement.decision_amount_raw == movement.amount_raw,
            _ => false,
        };
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
            "optimizerEpochFingerprint": movement.optimizer_epoch_fingerprint,
            "optimizerEpochExpiresAt": movement.optimizer_epoch_expires_at,
            "submissionOptimizerEpochEvidence": movement.submission_optimizer_epoch_evidence,
            "optimizerEpochIdentityExact": optimizer_epoch_identity_exact,
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
            "executedAmountRaw": movement.decision_amount_raw,
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
    let collection_started_at =
        DateTime::parse_from_rfc3339(baseline.get("collectionStartedAt")?.as_str()?)
            .ok()?
            .with_timezone(&Utc);
    (collected_at == captured_at
        && captured_at >= collection_started_at
        && captured_at.signed_duration_since(collection_started_at)
            <= chrono::Duration::seconds(PRODUCTION_EVIDENCE_MAX_COLLECTION_SECONDS))
    .then_some(collected_at)
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
) -> Result<Option<i64>, sqlx::Error> {
    if vault_ids.is_empty() {
        return Ok(Some(0));
    }
    sqlx::query_scalar::<_, Option<i64>>(
        r#"
        WITH cohort_main AS MATERIALIZED (
            SELECT CASE
                       WHEN planning_metadata->>'amount_semantics' = $4
                            AND COALESCE(
                                NULLIF(planning_metadata->>'redeemable_source_liquidity_amount_raw', ''),
                                NULLIF(planning_metadata->>'redeemable_liquidity_amount_raw', '')
                            ) ~ '^[0-9]+$'
                       THEN COALESCE(
                           NULLIF(planning_metadata->>'redeemable_source_liquidity_amount_raw', ''),
                           NULLIF(planning_metadata->>'redeemable_liquidity_amount_raw', '')
                       )::BIGINT
                       WHEN planning_metadata->>'amount_semantics' = $5
                       THEN amount_raw
                       ELSE NULL
                   END AS routeable_liquidity_amount_raw
            FROM loyal_yield.vault_reserve_positions_current
            WHERE vault_id = ANY($1)
              AND reserve = $2
              AND liquidity_mint = $3
              AND has_value
              AND amount_raw > 0
        )
        SELECT CASE
                   WHEN count(*) FILTER (
                       WHERE routeable_liquidity_amount_raw IS NULL
                          OR routeable_liquidity_amount_raw <= 0
                   ) > 0
                   THEN NULL
                   ELSE COALESCE(sum(routeable_liquidity_amount_raw), 0)::BIGINT
               END
        FROM cohort_main
        "#,
    )
    .bind(vault_ids)
    .bind(KAMINO_MAIN_USDC_RESERVE.to_string())
    .bind(USDC_MINT.to_string())
    .bind(AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED)
    .bind(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY)
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

async fn reconciled_volume_snapshot(
    pool: &PgPool,
    cluster: &str,
) -> Result<ReconciledVolumeSnapshot, sqlx::Error> {
    let row = sqlx::query(
        r#"
        SELECT count(*)::BIGINT AS movement_count,
               COALESCE(sum(decision.amount_raw), 0)::BIGINT AS amount_raw,
               COALESCE(sum(opportunity.principal_usd_micros), 0)::BIGINT
                   AS principal_usd_micros,
               max(submission.reconciled_at) AS newest_reconciled_at,
               count(DISTINCT submission.id)::BIGINT AS unique_submission_count,
               count(DISTINCT submission.opportunity_id)::BIGINT
                   AS unique_opportunity_count,
               count(DISTINCT submission.decision_id)::BIGINT AS unique_decision_count,
               count(DISTINCT submission.transaction_signature)::BIGINT
                   AS unique_signature_count
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
    .fetch_one(pool)
    .await?;
    Ok(ReconciledVolumeSnapshot {
        movement_count: row.try_get("movement_count")?,
        amount_raw: row.try_get("amount_raw")?,
        principal_usd_micros: row.try_get("principal_usd_micros")?,
        newest_reconciled_at: row.try_get("newest_reconciled_at")?,
        unique_submission_count: row.try_get("unique_submission_count")?,
        unique_opportunity_count: row.try_get("unique_opportunity_count")?,
        unique_decision_count: row.try_get("unique_decision_count")?,
        unique_signature_count: row.try_get("unique_signature_count")?,
    })
}

fn baseline_reconciled_volume(
    baseline: Option<&Value>,
    cluster: &str,
    cutover_at: Option<DateTime<Utc>>,
) -> Option<ReconciledVolumeSnapshot> {
    let baseline = baseline?;
    let collected_at = validated_baseline_collected_at(baseline, cluster)?;
    if cutover_at.is_some_and(|cutover_at| collected_at > cutover_at) {
        return None;
    }
    let value = baseline.pointer("/measurements/database/movement/reconciledVolume/current")?;
    Some(ReconciledVolumeSnapshot {
        movement_count: value.get("movementCount")?.as_i64()?,
        amount_raw: value.get("amountRaw")?.as_i64()?,
        principal_usd_micros: value.get("principalUsdMicros")?.as_i64()?,
        newest_reconciled_at: value
            .get("newestReconciledAt")
            .and_then(Value::as_str)
            .map(DateTime::parse_from_rfc3339)
            .transpose()
            .ok()?
            .map(|at| at.with_timezone(&Utc)),
        unique_submission_count: value.get("uniqueSubmissionCount")?.as_i64()?,
        unique_opportunity_count: value.get("uniqueOpportunityCount")?.as_i64()?,
        unique_decision_count: value.get("uniqueDecisionCount")?.as_i64()?,
        unique_signature_count: value.get("uniqueSignatureCount")?.as_i64()?,
    })
    .filter(|snapshot| snapshot.identity_exact())
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
    let current_volume = reconciled_volume_snapshot(pool, cluster).await?;
    let baseline_volume = baseline_reconciled_volume(baseline, cluster, cutover_at);
    let volume_delta = baseline_volume.and_then(|before| current_volume.checked_delta(before));
    let Some(cutover_at) = cutover_at else {
        return Ok((
            json!({
                "available": false,
                "reason": "--cutover-at is required for finalized movement evidence",
                "databaseDeadlockCount": database_deadlock_count,
                "duplicateMovementCount": 0,
                "reconciledVolume": {
                    "current": current_volume,
                    "currentIdentityExact": current_volume.identity_exact(),
                    "baseline": null,
                    "delta": null,
                },
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
    let mut reconciled_amount_raw = 0i64;
    let mut reconciled_principal_usd_micros = 0i64;
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
            reconciled_amount_raw =
                reconciled_amount_raw.saturating_add(movement.decision_amount_raw);
            reconciled_principal_usd_micros =
                reconciled_principal_usd_micros.saturating_add(movement.principal_usd_micros);
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
        if movement.submission_state == "reconciled"
            && movement.liquidity_mint == usdc_mint
            && movement_route_amount_evidence_exact(movement)
        {
            if movement.route_kind == "same_mint"
                && movement.source_reserve.as_deref() == Some(main_reserve.as_str())
            {
                main_outflow_raw += i128::from(movement.decision_amount_raw);
                if baseline_vaults.contains(&movement.vault_id) {
                    baseline_main_outflow_raw += i128::from(movement.decision_amount_raw);
                }
            }
            if matches!(
                movement.route_kind.as_str(),
                "same_mint" | "idle_vault_deposit"
            ) && movement.target_reserve == main_reserve
            {
                main_inflow_raw += i128::from(movement.decision_amount_raw);
                if baseline_vaults.contains(&movement.vault_id) {
                    baseline_main_inflow_raw += i128::from(movement.decision_amount_raw);
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
        current_baseline_cohort_main(pool, &baseline_vault_ids).await?
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
    let volume_pass = current_volume.identity_exact()
        && volume_delta.is_some_and(|delta| {
            delta.movement_count >= reconciled_movement_count
                && delta.amount_raw >= reconciled_amount_raw
                && delta.principal_usd_micros >= reconciled_principal_usd_micros
                && delta.movement_count > 0
                && delta.amount_raw > 0
                && delta.principal_usd_micros > 0
        });
    let (movement_slos, movement_slos_pass) = bounded_movement_slo_measurements(&movements);
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
        && volume_pass
        && main_reduction_pass
        && movement_slos_pass;
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
            "reconciledAmountRaw": reconciled_amount_raw,
            "reconciledPrincipalUsdMicros": reconciled_principal_usd_micros,
            "reconciledReserveMovementCount": reconciled_reserve_count,
            "reconciledIdleDepositCount": reconciled_idle_deposit_count,
            "fullyFinalizedAndReconciledEffectCount": fully_proven_count,
            "economicFailureCount": economic_failure_count,
            "unsafeTerminalOutcomeCount": unsafe_terminal_outcome_count,
            "databaseDeadlockCount": database_deadlock_count,
            "duplicateMovementCount": duplicate_movement_count,
            "movementSlos": movement_slos,
            "reconciledVolume": {
                "current": current_volume,
                "currentIdentityExact": current_volume.identity_exact(),
                "baseline": baseline_volume,
                "delta": volume_delta,
                "postCutoverMovementCount": reconciled_movement_count,
                "postCutoverAmountRaw": reconciled_amount_raw,
                "postCutoverPrincipalUsdMicros": reconciled_principal_usd_micros,
                "pass": volume_pass,
            },
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
    let mut positions = match collect_position_evidence(&pool).await {
        Ok(value) => value,
        Err(_) => json!({"available": false, "error": "position measurement query failed"}),
    };
    if let Some(object) = positions.as_object_mut() {
        object.insert(
            "largestEligibleVaults".to_owned(),
            collect_largest_account_evidence(&pool, &options.cluster, options.cutover_at)
                .await
                .unwrap_or_else(|_| {
                    json!({
                        "available": false,
                        "pass": false,
                        "error": "largest-account measurement query failed",
                    })
                }),
        );
    }
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
    // Capture queue/status last. Movement reconciliation can perform a bounded
    // chain sweep; a queue snapshot taken before it is not valid liveness
    // evidence for the end of this artifact.
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
    let (current_epoch_slos, _) = current_epoch_slo_measurements(&queue);
    if let Some(object) = movements.as_object_mut() {
        object.insert("currentEpochSlos".to_owned(), current_epoch_slos);
    }
    let largest_accounts_pass = positions
        .pointer("/largestEligibleVaults/pass")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    if options.cutover_at.is_some() && !largest_accounts_pass {
        movement_verdict = Verdict::Fail;
        if let Some(object) = movements.as_object_mut() {
            object.insert("pass".to_owned(), Value::Bool(false));
        }
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
    let compiled_source_sha256 = sha256_hex(COMPILED_COLLECTOR_SOURCE);
    let checkout_source_sha256 = collector_checkout_source_sha256(&options.repository_root);
    let executable_sha256 = env::current_exe().ok().as_deref().and_then(sha256_file);
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
            .pointer("/source/collectorCompiledSourceSha256")
            .and_then(Value::as_str)
            == Some(compiled_source_sha256.as_str())
        && baseline
            .pointer("/source/collectorCheckoutSourceSha256")
            .and_then(Value::as_str)
            == checkout_source_sha256.as_deref()
        && baseline
            .pointer("/source/collectorExecutableSha256")
            .and_then(Value::as_str)
            == executable_sha256.as_deref()
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
    let collection_started_at = Utc::now();
    let render_yaml = fs::read_to_string(options.repository_root.join("render.yaml"))?;
    let collector_source_before = collector_checkout_source_sha256(&options.repository_root);
    if collector_source_before.as_deref() != Some(sha256_hex(COMPILED_COLLECTOR_SOURCE).as_str()) {
        return Err("collector executable source does not match the checkout".into());
    }
    let fingerprint_nonce = scope_fingerprint_nonce(
        &options.repository_root,
        &render_yaml,
        collection_started_at,
    );
    let expected = expected_services(&render_yaml)?;
    let expected_monitor = expected_kamino_monitor(&render_yaml)?;
    let baseline = load_baseline(options.baseline.as_deref())?;
    if baseline
        .as_ref()
        .is_some_and(|baseline| !baseline_is_source_bound(baseline, &options, &render_yaml))
    {
        return Err("baseline artifact is not bound to this clean source and Render scope".into());
    }
    // The first Render read supplies the live provisioner budget scope needed
    // by the ALT audit. It is intentionally not the freshness measurement.
    let (_, _, alt_runtime) = collect_render_evidence(
        &expected,
        &expected_monitor,
        &options.render_environment_id,
        &options.cluster,
        &fingerprint_nonce,
    )
    .await;
    let database = collect_database_evidence(&options, baseline.as_ref(), alt_runtime).await;
    let (render, render_pass, _) = collect_render_evidence(
        &expected,
        &expected_monitor,
        &options.render_environment_id,
        &options.cluster,
        &fingerprint_nonce,
    )
    .await;
    let market_data_plane = collect_market_timescale_evidence(Utc::now()).await;
    let final_render_yaml = fs::read_to_string(options.repository_root.join("render.yaml"))?;
    let collector_source_after = collector_checkout_source_sha256(&options.repository_root);
    if final_render_yaml != render_yaml || collector_source_after != collector_source_before {
        return Err("collector source or render.yaml changed during evidence collection".into());
    }
    let deployment_verdict = if render_pass && database.migrations_pass && market_data_plane.pass {
        Verdict::Pass
    } else {
        Verdict::Fail
    };
    let production_performance_verdict =
        aggregate_verdict([database.queue_verdict, database.movement_verdict]);
    let end_state_verdict = aggregate_verdict([deployment_verdict, production_performance_verdict]);
    let head_commit = git_output(&options.repository_root, &["rev-parse", "HEAD"]);
    let source = source_evidence(&options.repository_root, &render_yaml);
    let captured_at = Utc::now();
    let output = json!({
        "schemaVersion": SCHEMA_VERSION,
        "event": "fleet_orchestration_production_evidence",
        "collectionStartedAt": collection_started_at,
        "collectedAt": captured_at,
        "capturedAt": captured_at,
        "headCommit": head_commit,
        "scope": {
            "cluster": options.cluster,
            "renderEnvironmentId": options.render_environment_id,
            "cutoverAt": options.cutover_at,
            "baselinePathSupplied": options.baseline.is_some(),
        },
        "source": source,
        "measurements": {
            "render": render,
            "marketDataPlane": {
                "timescale": market_data_plane.timescale,
            },
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
            "confirmedKaminoMarketDataPlane": if market_data_plane.pass { Verdict::Pass } else { Verdict::Fail },
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
