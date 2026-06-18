use std::{
    cmp::Ordering, collections::BTreeSet, env, error::Error, path::PathBuf, process::Command,
    time::Duration as StdDuration,
};

use chrono::{DateTime, Duration, Utc};
use loyal_actions::USDC_MINT;
use loyal_yield_orchestrator::sqlx::Row;
use loyal_yield_orchestrator::{
    route_amount_evidence, solana_testing_keypair_from_env, yield_router_keypair_from_env,
    CurrentReservePosition, ManagedVault, NeonSqlClient, NeonSqlConfig, PolicyId, RoutePolicy,
    VaultId, ACTIVE_DECISION_STATUSES, SOLANA_TESTING_PK_ENV,
};
use loyal_yield_router::timescale::{
    SupportedReserveLatestQuery, SupportedReserveLatestRow, TimescaleRouterClient,
    TimescaleRouterClientConfig,
};
use serde_json::{json, Value};
use solana_sdk::{pubkey::Pubkey, signature::Signer};
use tokio::time::{sleep, Duration as TokioDuration};

const DEFAULT_POLL_INTERVAL_SECONDS: u64 = 300;
const DEFAULT_REBALANCE_COOLDOWN_SECONDS: u64 = 300;
const DEFAULT_MAX_CANDIDATE_AGE_SECONDS: i64 = 6 * 60 * 60;
const DEFAULT_MIN_EDGE_BPS: i64 = 1;
const SAME_MINT_ROUTE_MODE: &str = "same_mint_kamino";

#[derive(Debug, Clone)]
struct Options {
    once: bool,
    execute: bool,
    all_active_vaults: bool,
    settings: Option<String>,
    vault_index: Option<i16>,
    poll_interval_seconds: u64,
    rebalance_cooldown_seconds: u64,
    max_candidate_age_seconds: i64,
    min_edge_bps: i64,
}

#[derive(Debug, Clone)]
struct ResolvedVault {
    vault: ManagedVault,
    policy: RoutePolicy,
}

#[derive(Debug, Clone)]
struct PlannedMonitorMove {
    source: CurrentReservePosition,
    target: SupportedReserveLatestRow,
    amount_raw: i64,
    route_amount_semantics: String,
    source_amount_semantics: Option<String>,
    source_collateral_amount_raw: Option<i64>,
    redeemable_source_liquidity_amount_raw: Option<i64>,
    idle_vault_liquidity_amount_raw: Option<i64>,
    source_apy_bps: i64,
    target_apy_bps: i64,
    edge_bps: i64,
}

#[derive(Debug)]
struct RouteExecutionOutput {
    success: bool,
    status_code: Option<i32>,
    stdout_json: Option<Value>,
    stdout_text: Option<String>,
    stderr_text: String,
}

#[derive(Debug)]
struct ReconcileOutput {
    success: bool,
    status_code: Option<i32>,
    stdout_json: Option<Value>,
    stdout_text: Option<String>,
    stderr_text: String,
}

#[derive(Debug, Clone)]
struct RecentConfirmedRebalance {
    id: i64,
    updated_at: DateTime<Utc>,
    source_reserve: Option<String>,
    target_reserve: Option<String>,
    signature: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum VaultResolutionMode {
    Explicit,
    Authority,
    Fleet,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let options = parse_args(env::args().skip(1))?;
    let neon_url = env::var("NEON_DATABASE_URL").map_err(|_| "NEON_DATABASE_URL is required")?;
    let timescale_url = env::var("TIMESCALEDB_URL").map_err(|_| "TIMESCALEDB_URL is required")?;

    let authority = authority_for_options(&options)?;
    let optimizer_signer = optimizer_signer_for_options(&options)?;
    let neon = NeonSqlClient::connect(
        NeonSqlConfig::new(neon_url)
            .with_max_connections(2)
            .with_acquire_timeout(StdDuration::from_secs(10)),
    )
    .await?;
    let timescale = TimescaleRouterClient::connect(
        TimescaleRouterClientConfig::new(timescale_url)
            .with_max_connections(2)
            .with_schema("kamino"),
    )
    .await?;

    loop {
        let outcome = run_once(&options, authority, optimizer_signer, &neon, &timescale).await?;
        println!("{}", serde_json::to_string_pretty(&outcome)?);
        if options.once {
            break;
        }
        sleep(TokioDuration::from_secs(options.poll_interval_seconds)).await;
    }

    Ok(())
}

async fn run_once(
    options: &Options,
    authority: Option<Pubkey>,
    optimizer_signer: Option<Pubkey>,
    neon: &NeonSqlClient,
    timescale: &TimescaleRouterClient,
) -> Result<Value, Box<dyn Error>> {
    let candidates = load_safe_usdc_candidates(timescale).await?;
    if options.all_active_vaults {
        let optimizer_signer =
            optimizer_signer.ok_or("YIELD_ROUTER_KEYPAIR signer was not loaded")?;
        let vaults = fetch_all_active_vaults(neon, &optimizer_signer.to_string()).await?;
        let mut results = Vec::with_capacity(vaults.len());
        for vault in vaults {
            let vault_identity = vault_json(&vault);
            match run_vault_once(options, vault, neon, &candidates).await {
                Ok(result) => results.push(result),
                Err(error) => results.push(json!({
                    "status": "vault_error",
                    "execute": options.execute,
                    "vault": vault_identity,
                    "error": error.to_string(),
                })),
            }
        }
        return Ok(json!({
            "status": "fleet_poll",
            "execute": options.execute,
            "allActiveVaults": true,
            "discoveredVaultCount": results.len(),
            "candidateCount": candidates.len(),
            "pollIntervalSeconds": options.poll_interval_seconds,
            "rebalanceCooldownSeconds": options.rebalance_cooldown_seconds,
            "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
            "minEdgeBps": options.min_edge_bps,
            "results": results,
        }));
    }

    let vault = resolve_vault(neon, authority, options).await?;
    run_vault_once(options, vault, neon, &candidates).await
}

async fn run_vault_once(
    options: &Options,
    vault: ResolvedVault,
    neon: &NeonSqlClient,
    candidates: &[SupportedReserveLatestRow],
) -> Result<Value, Box<dyn Error>> {
    let policy_candidates = policy_eligible_candidates(&vault.policy, candidates);
    let active_decisions = active_decision_count(neon, vault.vault.id).await?;
    if active_decisions > 0 {
        return Ok(json!({
            "status": "skipped_active_decision",
            "execute": options.execute,
            "skipReason": "active_decision",
            "vault": vault_json(&vault),
            "activeDecisionCount": active_decisions,
            "candidates": candidates_json(candidates),
            "policyEligibleCandidates": candidates_json(&policy_candidates),
            "rebalanceCooldownSeconds": options.rebalance_cooldown_seconds,
        }));
    }

    let recent_rebalance =
        recent_confirmed_rebalance(neon, vault.vault.id, options.rebalance_cooldown_seconds)
            .await?;
    if let Some(recent_rebalance) = recent_rebalance {
        let cooldown_remaining_seconds = cooldown_remaining_seconds(
            recent_rebalance.updated_at,
            options.rebalance_cooldown_seconds,
        );
        return Ok(json!({
            "status": "skipped_recent_rebalance",
            "execute": options.execute,
            "skipReason": "recent_rebalance_cooldown",
            "vault": vault_json(&vault),
            "activeDecisionCount": active_decisions,
            "lastConfirmedRebalance": recent_confirmed_rebalance_json(&recent_rebalance),
            "lastConfirmedRebalanceAt": recent_rebalance.updated_at,
            "rebalanceCooldownSeconds": options.rebalance_cooldown_seconds,
            "cooldownRemainingSeconds": cooldown_remaining_seconds,
            "candidates": candidates_json(candidates),
            "policyEligibleCandidates": candidates_json(&policy_candidates),
        }));
    }

    let reconcile = reconcile_current_positions_for_vault(&vault, candidates)?;
    if !reconcile.success {
        return Ok(json!({
            "status": "reconcile_failed",
            "execute": options.execute,
            "vault": vault_json(&vault),
            "chainReconcile": reconcile_output_json(&reconcile),
            "candidates": candidates_json(candidates),
            "policyEligibleCandidates": candidates_json(&policy_candidates),
        }));
    }
    let positions = neon.current_positions(vault.vault.id).await?;
    let policy_positions = policy_eligible_positions(&positions, &policy_candidates);
    let freshest_cutoff = Utc::now() - Duration::seconds(options.max_candidate_age_seconds);
    let (fresh_candidates, stale_candidate_count) =
        split_fresh_candidates(&policy_candidates, freshest_cutoff);

    let plan = if fresh_candidates.is_empty() {
        Err("no_eligible_fresh_candidate_data".to_owned())
    } else {
        plan_move(&policy_positions, &fresh_candidates, options.min_edge_bps)
    };
    let (status, skip_reason) =
        monitor_status_and_skip_reason(&plan, active_decisions, options.execute);

    if options.execute && matches!(plan, Ok(Some(_))) && active_decisions == 0 {
        let planned_move = plan
            .as_ref()
            .ok()
            .and_then(Option::as_ref)
            .expect("matched planned move");
        let execution = execute_planned_move(&vault, planned_move)?;
        let active_decision_count_after = active_decision_count(neon, vault.vault.id).await?;
        let positions_after = neon.current_positions(vault.vault.id).await?;
        if !execution.success {
            return Ok(json!({
                "status": route_execution_status(&execution),
                "execute": true,
                "vault": vault_json(&vault),
                "activeDecisionCount": active_decisions,
                "activeDecisionCountAfter": active_decision_count_after,
                "currentPositions": positions_json(&positions),
                "policyEligibleCurrentPositions": positions_json(&policy_positions),
                "currentPositionsAfter": positions_json(&positions_after),
                "chainReconcile": reconcile_output_json(&reconcile),
                "candidates": candidates_json(candidates),
                "policyEligibleCandidates": candidates_json(&policy_candidates),
                "freshCandidateCount": fresh_candidates.len(),
                "staleCandidateCount": stale_candidate_count,
                "plannedMove": planned_move_json(Some(planned_move)),
                "routeExecution": route_execution_output_json(&execution),
            }));
        }
        return Ok(json!({
            "status": "executed",
            "execute": true,
            "vault": vault_json(&vault),
            "activeDecisionCount": active_decisions,
            "activeDecisionCountAfter": active_decision_count_after,
            "currentPositions": positions_json(&positions),
            "policyEligibleCurrentPositions": positions_json(&policy_positions),
            "currentPositionsAfter": positions_json(&positions_after),
            "chainReconcile": reconcile_output_json(&reconcile),
            "candidates": candidates_json(candidates),
            "policyEligibleCandidates": candidates_json(&policy_candidates),
            "freshCandidateCount": fresh_candidates.len(),
            "staleCandidateCount": stale_candidate_count,
            "plannedMove": planned_move_json(Some(planned_move)),
            "routeExecution": route_execution_output_json(&execution),
        }));
    }

    Ok(json!({
        "status": status,
        "execute": options.execute,
        "skipReason": skip_reason,
        "vault": vault_json(&vault),
        "activeDecisionCount": active_decisions,
        "currentPositions": positions_json(&positions),
        "policyEligibleCurrentPositions": positions_json(&policy_positions),
        "chainReconcile": reconcile_output_json(&reconcile),
        "candidates": candidates_json(candidates),
        "policyEligibleCandidates": candidates_json(&policy_candidates),
        "freshCandidateCount": fresh_candidates.len(),
        "staleCandidateCount": stale_candidate_count,
        "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
        "rebalanceCooldownSeconds": options.rebalance_cooldown_seconds,
        "minEdgeBps": options.min_edge_bps,
        "plannedMove": planned_move_json(plan.as_ref().ok().and_then(Option::as_ref)),
    }))
}

fn execute_planned_move(
    vault: &ResolvedVault,
    planned_move: &PlannedMonitorMove,
) -> Result<RouteExecutionOutput, Box<dyn Error>> {
    let binary = same_mint_reserve_swap_binary()?;
    let current_exe = env::current_exe()?;
    let is_local_debug = current_exe.to_string_lossy().contains("/target/debug/");
    let mut command = if binary.exists() && !is_local_debug {
        Command::new(binary)
    } else {
        let mut fallback = Command::new("cargo");
        fallback.args([
            "run",
            "-p",
            "loyal-yield-orchestrator",
            "--bin",
            "same-mint-reserve-swap",
            "--",
        ]);
        fallback
    };
    let output = command
        .arg("--settings")
        .arg(&vault.vault.settings)
        .arg("--vault-index")
        .arg(vault.vault.vault_index.to_string())
        .arg("--source-reserve")
        .arg(&planned_move.source.reserve)
        .arg("--target-reserve")
        .arg(&planned_move.target.reserve)
        .arg("--expected-source-snapshot-id")
        .arg(planned_move.source.snapshot_id.as_i64().to_string())
        .arg("--expected-amount-raw")
        .arg(planned_move.amount_raw.to_string())
        .arg("--expected-route-amount-semantics")
        .arg(&planned_move.route_amount_semantics)
        .arg("--expected-source-apy-bps")
        .arg(planned_move.source_apy_bps.to_string())
        .arg("--expected-target-apy-bps")
        .arg(planned_move.target_apy_bps.to_string())
        .arg("--expected-edge-bps")
        .arg(planned_move.edge_bps.to_string())
        .arg("--optimization-cycle")
        .arg("--reconcile-from-chain")
        .arg("--provision-lookup-table")
        .arg("--execute")
        .output()?;
    let stdout_text = String::from_utf8_lossy(&output.stdout).trim().to_owned();
    let stdout_json = if stdout_text.is_empty() {
        None
    } else {
        serde_json::from_str::<Value>(&stdout_text).ok()
    };
    Ok(RouteExecutionOutput {
        success: output.status.success(),
        status_code: output.status.code(),
        stdout_json,
        stdout_text: if stdout_text.is_empty() {
            None
        } else {
            Some(stdout_text)
        },
        stderr_text: String::from_utf8_lossy(&output.stderr).trim().to_owned(),
    })
}

fn same_mint_reserve_swap_binary() -> Result<PathBuf, Box<dyn Error>> {
    let current_exe = env::current_exe()?;
    let dir = current_exe
        .parent()
        .ok_or("current executable has no parent directory")?;
    Ok(dir.join("same-mint-reserve-swap"))
}

fn reconcile_current_positions_for_vault(
    vault: &ResolvedVault,
    policy_candidates: &[SupportedReserveLatestRow],
) -> Result<ReconcileOutput, Box<dyn Error>> {
    let reserves = reconcile_reserves_for_candidates(policy_candidates);
    if reserves.len() < 2 {
        return Err(
            "chain reconciliation requires at least two policy-eligible USDC reserves".into(),
        );
    }

    let binary = same_mint_reserve_swap_binary()?;
    let current_exe = env::current_exe()?;
    let is_local_debug = current_exe.to_string_lossy().contains("/target/debug/");
    let mut command = if binary.exists() && !is_local_debug {
        Command::new(binary)
    } else {
        let mut fallback = Command::new("cargo");
        fallback.args([
            "run",
            "-p",
            "loyal-yield-orchestrator",
            "--bin",
            "same-mint-reserve-swap",
            "--",
        ]);
        fallback
    };
    command
        .arg("--settings")
        .arg(&vault.vault.settings)
        .arg("--vault-index")
        .arg(vault.vault.vault_index.to_string())
        .arg("--source-reserve")
        .arg(&reserves[0])
        .arg("--target-reserve")
        .arg(&reserves[1])
        .arg("--reconcile-from-chain")
        .arg("--reconcile-current-positions");
    for reserve in &reserves {
        command.arg("--reconcile-reserve").arg(reserve);
    }

    let output = command.output()?;
    let stdout_text = String::from_utf8_lossy(&output.stdout).trim().to_owned();
    let stdout_json = if stdout_text.is_empty() {
        None
    } else {
        serde_json::from_str::<Value>(&stdout_text).ok()
    };
    Ok(ReconcileOutput {
        success: output.status.success(),
        status_code: output.status.code(),
        stdout_json,
        stdout_text: if stdout_text.is_empty() {
            None
        } else {
            Some(stdout_text)
        },
        stderr_text: String::from_utf8_lossy(&output.stderr).trim().to_owned(),
    })
}

fn reconcile_reserves_for_candidates(candidates: &[SupportedReserveLatestRow]) -> Vec<String> {
    let mut reserves = Vec::new();
    for candidate in candidates {
        if !reserves.iter().any(|reserve| reserve == &candidate.reserve) {
            reserves.push(candidate.reserve.clone());
        }
    }
    reserves
}

async fn load_safe_usdc_candidates(
    timescale: &TimescaleRouterClient,
) -> Result<Vec<SupportedReserveLatestRow>, Box<dyn Error>> {
    Ok(timescale
        .latest_supported_reserves(SupportedReserveLatestQuery::safe_usdc(
            USDC_MINT.to_string(),
        ))
        .await?)
}

fn policy_eligible_candidates(
    policy: &RoutePolicy,
    candidates: &[SupportedReserveLatestRow],
) -> Vec<SupportedReserveLatestRow> {
    let usdc = USDC_MINT.to_string();
    candidates
        .iter()
        .filter(|candidate| {
            candidate.liquidity_mint == usdc
                && candidate.market.as_ref().is_some_and(|market| {
                    policy
                        .kamino_markets
                        .iter()
                        .any(|allowed_market| allowed_market == market)
                })
                && policy.stable_mints.iter().any(|mint| mint == &usdc)
                && policy
                    .kamino_liquidity_mints
                    .iter()
                    .any(|mint| mint == &candidate.liquidity_mint)
        })
        .cloned()
        .collect()
}

fn policy_eligible_positions(
    positions: &[CurrentReservePosition],
    policy_candidates: &[SupportedReserveLatestRow],
) -> Vec<CurrentReservePosition> {
    let eligible_reserves = policy_candidates
        .iter()
        .map(|candidate| candidate.reserve.as_str())
        .collect::<BTreeSet<_>>();
    positions
        .iter()
        .filter(|position| eligible_reserves.contains(position.reserve.as_str()))
        .cloned()
        .collect()
}

fn plan_move(
    positions: &[CurrentReservePosition],
    candidates: &[SupportedReserveLatestRow],
    min_edge_bps: i64,
) -> Result<Option<PlannedMonitorMove>, String> {
    let source = positions
        .iter()
        .filter(|position| {
            position.liquidity_mint == USDC_MINT.to_string()
                && position.has_value
                && position.amount_raw > 0
                && route_amount_evidence(position).is_some()
        })
        .max_by_key(|position| position.amount_raw)
        .cloned()
        .ok_or_else(|| {
            if positions.iter().any(|position| {
                position.liquidity_mint == USDC_MINT.to_string()
                    && position.has_value
                    && position.amount_raw > 0
            }) {
                "unsupported_amount_semantics".to_owned()
            } else {
                "no_value_source".to_owned()
            }
        })?;
    let evidence =
        route_amount_evidence(&source).ok_or_else(|| "unsupported_amount_semantics".to_owned())?;
    let target = candidates
        .iter()
        .filter(|candidate| candidate.liquidity_mint == USDC_MINT.to_string())
        .max_by(|left, right| compare_candidate_preference(left, right))
        .cloned()
        .ok_or_else(|| "no_eligible_fresh_candidate_data".to_owned())?;
    if source.reserve == target.reserve {
        return Ok(None);
    }

    let source_apy_bps = candidates
        .iter()
        .find(|candidate| candidate.reserve == source.reserve)
        .map(|candidate| apy_to_bps(candidate.supply_apy))
        .or(source.supply_apy_bps)
        .unwrap_or_default();
    let target_apy_bps = apy_to_bps(target.supply_apy);
    let edge_bps = target_apy_bps - source_apy_bps;
    if edge_bps < min_edge_bps {
        return Ok(None);
    }

    Ok(Some(PlannedMonitorMove {
        source,
        target,
        amount_raw: evidence.amount_raw,
        route_amount_semantics: evidence.route_amount_semantics,
        source_amount_semantics: evidence.source_amount_semantics,
        source_collateral_amount_raw: evidence.source_collateral_amount_raw,
        redeemable_source_liquidity_amount_raw: evidence.redeemable_source_liquidity_amount_raw,
        idle_vault_liquidity_amount_raw: evidence.idle_vault_liquidity_amount_raw,
        source_apy_bps,
        target_apy_bps,
        edge_bps,
    }))
}

fn split_fresh_candidates(
    candidates: &[SupportedReserveLatestRow],
    freshest_cutoff: chrono::DateTime<Utc>,
) -> (Vec<SupportedReserveLatestRow>, usize) {
    let mut fresh = Vec::new();
    let mut stale_count = 0;
    for candidate in candidates {
        if candidate.observed_at >= freshest_cutoff {
            fresh.push(candidate.clone());
        } else {
            stale_count += 1;
        }
    }
    (fresh, stale_count)
}

fn monitor_status_and_skip_reason<'a>(
    plan: &'a Result<Option<PlannedMonitorMove>, String>,
    active_decisions: i64,
    execute: bool,
) -> (&'static str, Option<&'a str>) {
    if active_decisions > 0 {
        return ("skipped_active_decision", Some("active_decision"));
    }
    match plan {
        Ok(None) => ("skipped", Some("already_at_winner_or_no_positive_edge")),
        Ok(Some(_)) if execute => ("planned_execute", None),
        Ok(Some(_)) => ("planned_dry_run", None),
        Err(reason) => ("skipped", Some(reason.as_str())),
    }
}

async fn resolve_vault(
    neon: &NeonSqlClient,
    authority: Option<Pubkey>,
    options: &Options,
) -> Result<ResolvedVault, Box<dyn Error>> {
    match vault_resolution_mode(options)? {
        VaultResolutionMode::Explicit => {
            let settings = options
                .settings
                .as_ref()
                .expect("explicit mode has settings");
            let vault_index = options.vault_index.expect("explicit mode has vault index");
            let rows = fetch_vaults_by_settings_index(neon, settings, vault_index).await?;
            exactly_one(rows, "explicit settings/vault-index")
        }
        VaultResolutionMode::Authority => {
            let authority = authority.ok_or("SOLANA_TESTING_PK authority was not loaded")?;
            let rows = fetch_vaults_by_authority(neon, &authority.to_string()).await?;
            exactly_one(rows, "SOLANA_TESTING_PK authority")
        }
        VaultResolutionMode::Fleet => Err("--all-active-vaults resolves multiple vaults".into()),
    }
}

fn authority_for_options(options: &Options) -> Result<Option<Pubkey>, Box<dyn Error>> {
    match authority_env_for_options(options)? {
        Some(_) => Ok(Some(solana_testing_keypair_from_env()?.pubkey())),
        None => Ok(None),
    }
}

fn optimizer_signer_for_options(options: &Options) -> Result<Option<Pubkey>, Box<dyn Error>> {
    if options.all_active_vaults {
        Ok(Some(yield_router_keypair_from_env()?.pubkey()))
    } else {
        Ok(None)
    }
}

fn authority_env_for_options(options: &Options) -> Result<Option<&'static str>, Box<dyn Error>> {
    match vault_resolution_mode(options)? {
        VaultResolutionMode::Explicit => Ok(None),
        VaultResolutionMode::Fleet => Ok(None),
        VaultResolutionMode::Authority if options.execute => Err(
            "--execute requires explicit --settings and --vault-index; SOLANA_TESTING_PK authority discovery is setup/admin only"
                .into(),
        ),
        VaultResolutionMode::Authority => Ok(Some(SOLANA_TESTING_PK_ENV)),
    }
}

fn vault_resolution_mode(options: &Options) -> Result<VaultResolutionMode, Box<dyn Error>> {
    if options.all_active_vaults {
        if options.settings.is_some() || options.vault_index.is_some() {
            return Err(
                "--all-active-vaults is mutually exclusive with --settings/--vault-index".into(),
            );
        }
        return Ok(VaultResolutionMode::Fleet);
    }
    match (&options.settings, options.vault_index) {
        (Some(_), Some(_)) => Ok(VaultResolutionMode::Explicit),
        (None, None) => Ok(VaultResolutionMode::Authority),
        _ => Err("--settings and --vault-index must be provided together".into()),
    }
}

async fn fetch_all_active_vaults(
    neon: &NeonSqlClient,
    delegated_signer: &str,
) -> Result<Vec<ResolvedVault>, Box<dyn Error>> {
    let rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            v.id AS vault_id,
            v.settings,
            v.vault_index,
            v.vault_pubkey,
            v.active_policy_id,
            v.active AS vault_active,
            v.first_seen_at AS vault_first_seen_at,
            v.last_seen_at AS vault_last_seen_at,
            p.id AS policy_id,
            p.authority,
            p.policy_seed,
            p.policy_account,
            p.delegated_signers,
            p.threshold,
            p.route_modes,
            p.stable_mints,
            p.kamino_markets,
            p.kamino_liquidity_mints,
            p.universe_preset,
            p.risk_profile,
            p.swap_lanes,
            p.active AS policy_active,
            p.first_seen_at AS policy_first_seen_at,
            p.last_seen_at AS policy_last_seen_at,
            p.last_seen_slot,
            p.last_seen_signature
        FROM loyal_yield.managed_vaults v
        JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
        WHERE v.active = true
          AND p.active = true
          AND $1 = ANY(p.delegated_signers)
          AND $2 = ANY(p.route_modes)
          AND $3 = ANY(p.stable_mints)
          AND $3 = ANY(p.kamino_liquidity_mints)
          AND cardinality(p.kamino_markets) > 0
        ORDER BY v.last_seen_at DESC, v.id DESC
        "#,
    )
    .bind(delegated_signer)
    .bind(SAME_MINT_ROUTE_MODE)
    .bind(USDC_MINT.to_string())
    .fetch_all(neon.pool())
    .await?;
    rows.into_iter().map(resolved_vault_from_row).collect()
}

async fn fetch_vaults_by_settings_index(
    neon: &NeonSqlClient,
    settings: &str,
    vault_index: i16,
) -> Result<Vec<ResolvedVault>, Box<dyn Error>> {
    let rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            v.id AS vault_id,
            v.settings,
            v.vault_index,
            v.vault_pubkey,
            v.active_policy_id,
            v.active AS vault_active,
            v.first_seen_at AS vault_first_seen_at,
            v.last_seen_at AS vault_last_seen_at,
            p.id AS policy_id,
            p.authority,
            p.policy_seed,
            p.policy_account,
            p.delegated_signers,
            p.threshold,
            p.route_modes,
            p.stable_mints,
            p.kamino_markets,
            p.kamino_liquidity_mints,
            p.universe_preset,
            p.risk_profile,
            p.swap_lanes,
            p.active AS policy_active,
            p.first_seen_at AS policy_first_seen_at,
            p.last_seen_at AS policy_last_seen_at,
            p.last_seen_slot,
            p.last_seen_signature
        FROM loyal_yield.managed_vaults v
        JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
        WHERE v.settings = $1
          AND v.vault_index = $2
          AND v.active = true
          AND p.active = true
        ORDER BY v.last_seen_at DESC, v.id DESC
        "#,
    )
    .bind(settings)
    .bind(vault_index)
    .fetch_all(neon.pool())
    .await?;
    rows.into_iter().map(resolved_vault_from_row).collect()
}

async fn fetch_vaults_by_authority(
    neon: &NeonSqlClient,
    authority: &str,
) -> Result<Vec<ResolvedVault>, Box<dyn Error>> {
    let rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            v.id AS vault_id,
            v.settings,
            v.vault_index,
            v.vault_pubkey,
            v.active_policy_id,
            v.active AS vault_active,
            v.first_seen_at AS vault_first_seen_at,
            v.last_seen_at AS vault_last_seen_at,
            p.id AS policy_id,
            p.authority,
            p.policy_seed,
            p.policy_account,
            p.delegated_signers,
            p.threshold,
            p.route_modes,
            p.stable_mints,
            p.kamino_markets,
            p.kamino_liquidity_mints,
            p.universe_preset,
            p.risk_profile,
            p.swap_lanes,
            p.active AS policy_active,
            p.first_seen_at AS policy_first_seen_at,
            p.last_seen_at AS policy_last_seen_at,
            p.last_seen_slot,
            p.last_seen_signature
        FROM loyal_yield.managed_vaults v
        JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
        WHERE p.authority = $1
          AND v.active = true
          AND p.active = true
        ORDER BY v.last_seen_at DESC, v.id DESC
        "#,
    )
    .bind(authority)
    .fetch_all(neon.pool())
    .await?;
    rows.into_iter().map(resolved_vault_from_row).collect()
}

fn exactly_one(rows: Vec<ResolvedVault>, context: &str) -> Result<ResolvedVault, Box<dyn Error>> {
    match rows.len() {
        1 => Ok(rows.into_iter().next().expect("one row exists")),
        0 => Err(format!("no active managed vault found for {context}").into()),
        count => Err(format!(
            "{count} active managed vaults found for {context}; pass --settings and --vault-index"
        )
        .into()),
    }
}

fn resolved_vault_from_row(
    row: loyal_yield_orchestrator::sqlx::postgres::PgRow,
) -> Result<ResolvedVault, Box<dyn Error>> {
    let vault = ManagedVault {
        id: VaultId(row.try_get("vault_id")?),
        settings: row.try_get("settings")?,
        vault_index: row.try_get("vault_index")?,
        vault_pubkey: row.try_get("vault_pubkey")?,
        active_policy_id: PolicyId(row.try_get("active_policy_id")?),
        active: row.try_get("vault_active")?,
        first_seen_at: row.try_get("vault_first_seen_at")?,
        last_seen_at: row.try_get("vault_last_seen_at")?,
    };
    let policy = RoutePolicy {
        id: PolicyId(row.try_get("policy_id")?),
        settings: vault.settings.clone(),
        authority: row.try_get("authority")?,
        policy_seed: row.try_get("policy_seed")?,
        policy_account: row.try_get("policy_account")?,
        vault_index: vault.vault_index,
        vault_pubkey: vault.vault_pubkey.clone(),
        delegated_signers: row.try_get("delegated_signers")?,
        threshold: row.try_get("threshold")?,
        route_modes: row.try_get("route_modes")?,
        stable_mints: row.try_get("stable_mints")?,
        kamino_markets: row.try_get("kamino_markets")?,
        kamino_liquidity_mints: row.try_get("kamino_liquidity_mints")?,
        universe_preset: row.try_get("universe_preset")?,
        risk_profile: row.try_get("risk_profile")?,
        swap_lanes: row.try_get("swap_lanes")?,
        active: row.try_get("policy_active")?,
        first_seen_at: row.try_get("policy_first_seen_at")?,
        last_seen_at: row.try_get("policy_last_seen_at")?,
        last_seen_slot: row.try_get("last_seen_slot")?,
        last_seen_signature: row.try_get("last_seen_signature")?,
    };
    Ok(ResolvedVault { vault, policy })
}

async fn active_decision_count(
    neon: &NeonSqlClient,
    vault_id: VaultId,
) -> Result<i64, Box<dyn Error>> {
    let statuses = ACTIVE_DECISION_STATUSES
        .iter()
        .map(|status| (*status).to_owned())
        .collect::<Vec<_>>();
    let count = loyal_yield_orchestrator::sqlx::query_scalar::<_, i64>(
        r#"
        SELECT COUNT(*)::BIGINT
        FROM loyal_yield.rebalance_decisions
        WHERE vault_id = $1 AND status::text = ANY($2)
        "#,
    )
    .bind(vault_id.as_i64())
    .bind(statuses)
    .fetch_one(neon.pool())
    .await?;
    Ok(count)
}

async fn recent_confirmed_rebalance(
    neon: &NeonSqlClient,
    vault_id: VaultId,
    cooldown_seconds: u64,
) -> Result<Option<RecentConfirmedRebalance>, Box<dyn Error>> {
    if cooldown_seconds == 0 {
        return Ok(None);
    }
    let cooldown_seconds =
        i64::try_from(cooldown_seconds).map_err(|_| "--rebalance-cooldown-seconds is too large")?;
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, updated_at, source_reserve, target_reserve, signature
        FROM loyal_yield.rebalance_decisions
        WHERE vault_id = $1
          AND status::text = 'confirmed'
          AND updated_at > now() - ($2::BIGINT * interval '1 second')
        ORDER BY updated_at DESC, id DESC
        LIMIT 1
        "#,
    )
    .bind(vault_id.as_i64())
    .bind(cooldown_seconds)
    .fetch_optional(neon.pool())
    .await?;

    row.map(|row| {
        Ok::<_, loyal_yield_orchestrator::sqlx::Error>(RecentConfirmedRebalance {
            id: row.try_get("id")?,
            updated_at: row.try_get("updated_at")?,
            source_reserve: row.try_get("source_reserve")?,
            target_reserve: row.try_get("target_reserve")?,
            signature: row.try_get("signature")?,
        })
    })
    .transpose()
    .map_err(Into::into)
}

fn cooldown_remaining_seconds(last_confirmed_at: DateTime<Utc>, cooldown_seconds: u64) -> u64 {
    let elapsed_seconds = Utc::now()
        .signed_duration_since(last_confirmed_at)
        .num_seconds()
        .max(0) as u64;
    cooldown_seconds.saturating_sub(elapsed_seconds)
}

fn vault_json(resolved: &ResolvedVault) -> Value {
    json!({
        "vaultId": resolved.vault.id.as_i64(),
        "settings": resolved.vault.settings,
        "vaultIndex": resolved.vault.vault_index,
        "vaultPubkey": resolved.vault.vault_pubkey,
        "policyId": resolved.policy.id.as_i64(),
        "policyAccount": resolved.policy.policy_account,
        "policyAuthority": resolved.policy.authority,
        "delegatedSigners": resolved.policy.delegated_signers,
        "stableMints": resolved.policy.stable_mints,
        "kaminoMarkets": resolved.policy.kamino_markets,
        "kaminoLiquidityMints": resolved.policy.kamino_liquidity_mints,
    })
}

fn compare_candidate_preference(
    left: &SupportedReserveLatestRow,
    right: &SupportedReserveLatestRow,
) -> Ordering {
    apy_to_bps(left.supply_apy)
        .cmp(&apy_to_bps(right.supply_apy))
        .then_with(|| left.observed_at.cmp(&right.observed_at))
        .then_with(|| left.slot.cmp(&right.slot))
        .then_with(|| right.reserve.cmp(&left.reserve))
}

fn positions_json(positions: &[CurrentReservePosition]) -> Vec<Value> {
    positions
        .iter()
        .map(|position| {
            json!({
                "reserve": position.reserve,
                "market": position.market,
                "liquidityMint": position.liquidity_mint,
                "amountRaw": position.amount_raw,
                "hasValue": position.has_value,
                "supplyApyBps": position.supply_apy_bps,
                "snapshotId": position.snapshot_id.as_i64(),
                "observedSlot": position.observed_slot,
                "observedAt": position.observed_at,
            })
        })
        .collect()
}

fn candidates_json(candidates: &[SupportedReserveLatestRow]) -> Vec<Value> {
    candidates
        .iter()
        .map(|candidate| {
            json!({
                "observedAt": candidate.observed_at,
                "slot": candidate.slot,
                "reserve": candidate.reserve,
                "market": candidate.market,
                "marketName": candidate.market_name,
                "liquidityMint": candidate.liquidity_mint,
                "symbol": candidate.symbol,
                "supplyApy": candidate.supply_apy,
                "supplyApyBps": apy_to_bps(candidate.supply_apy),
                "totalSupplyUsdEstimate": candidate.total_supply_usd_estimate,
                "reserveLastUpdateStale": candidate.reserve_last_update_stale,
            })
        })
        .collect()
}

fn planned_move_json(plan: Option<&PlannedMonitorMove>) -> Value {
    match plan {
        Some(plan) => json!({
            "sourceReserve": plan.source.reserve,
            "targetReserve": plan.target.reserve,
            "targetMarket": plan.target.market,
            "liquidityMint": plan.source.liquidity_mint,
            "amountRaw": plan.amount_raw,
            "sourceCurrentAmountRaw": plan.source.amount_raw,
            "sourceSnapshotId": plan.source.snapshot_id.as_i64(),
            "routeAmountSemantics": plan.route_amount_semantics,
            "sourceAmountSemantics": plan.source_amount_semantics,
            "sourceCollateralAmountRaw": plan.source_collateral_amount_raw,
            "redeemableSourceLiquidityAmountRaw": plan.redeemable_source_liquidity_amount_raw,
            "idleVaultLiquidityAmountRaw": plan.idle_vault_liquidity_amount_raw,
            "sourceApyBps": plan.source_apy_bps,
            "targetApyBps": plan.target_apy_bps,
            "estimatedEdgeBps": plan.edge_bps,
        }),
        None => Value::Null,
    }
}

fn recent_confirmed_rebalance_json(rebalance: &RecentConfirmedRebalance) -> Value {
    json!({
        "id": rebalance.id,
        "updatedAt": rebalance.updated_at,
        "sourceReserve": rebalance.source_reserve,
        "targetReserve": rebalance.target_reserve,
        "signature": rebalance.signature,
    })
}

fn route_execution_output_json(execution: &RouteExecutionOutput) -> Value {
    json!({
        "success": execution.success,
        "statusCode": execution.status_code,
        "stdout": execution.stdout_json.as_ref().unwrap_or(&Value::Null),
        "stdoutText": if execution.stdout_json.is_some() {
            None
        } else {
            execution.stdout_text.as_deref()
        },
        "stderrText": if execution.stderr_text.is_empty() {
            None
        } else {
            Some(execution.stderr_text.as_str())
        },
    })
}

fn route_execution_status(execution: &RouteExecutionOutput) -> String {
    execution
        .stdout_json
        .as_ref()
        .and_then(|stdout| stdout.get("status"))
        .and_then(Value::as_str)
        .unwrap_or("execution_failed")
        .to_owned()
}

fn reconcile_output_json(reconcile: &ReconcileOutput) -> Value {
    json!({
        "success": reconcile.success,
        "statusCode": reconcile.status_code,
        "stdout": reconcile.stdout_json.as_ref().unwrap_or(&Value::Null),
        "stdoutText": if reconcile.stdout_json.is_some() {
            None
        } else {
            reconcile.stdout_text.as_deref()
        },
        "stderrText": if reconcile.stderr_text.is_empty() {
            None
        } else {
            Some(reconcile.stderr_text.as_str())
        },
    })
}

fn apy_to_bps(apy: f64) -> i64 {
    (apy * 10_000.0).round() as i64
}

fn parse_args(args: impl IntoIterator<Item = String>) -> Result<Options, Box<dyn Error>> {
    let mut options = Options {
        once: false,
        execute: false,
        all_active_vaults: false,
        settings: None,
        vault_index: None,
        poll_interval_seconds: DEFAULT_POLL_INTERVAL_SECONDS,
        rebalance_cooldown_seconds: DEFAULT_REBALANCE_COOLDOWN_SECONDS,
        max_candidate_age_seconds: DEFAULT_MAX_CANDIDATE_AGE_SECONDS,
        min_edge_bps: DEFAULT_MIN_EDGE_BPS,
    };
    let mut iter = args.into_iter();
    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--once" => options.once = true,
            "--execute" => options.execute = true,
            "--all-active-vaults" => options.all_active_vaults = true,
            "--settings" => {
                options.settings = Some(iter.next().ok_or("--settings requires a pubkey")?);
            }
            "--vault-index" => {
                options.vault_index = Some(
                    iter.next()
                        .ok_or("--vault-index requires a value")?
                        .parse()
                        .map_err(|_| "--vault-index must be an integer")?,
                );
            }
            "--poll-interval-seconds" => {
                options.poll_interval_seconds = iter
                    .next()
                    .ok_or("--poll-interval-seconds requires a value")?
                    .parse()
                    .map_err(|_| "--poll-interval-seconds must be an integer")?;
            }
            "--rebalance-cooldown-seconds" => {
                options.rebalance_cooldown_seconds = iter
                    .next()
                    .ok_or("--rebalance-cooldown-seconds requires a value")?
                    .parse()
                    .map_err(|_| "--rebalance-cooldown-seconds must be an integer")?;
            }
            "--max-candidate-age-seconds" => {
                options.max_candidate_age_seconds = iter
                    .next()
                    .ok_or("--max-candidate-age-seconds requires a value")?
                    .parse()
                    .map_err(|_| "--max-candidate-age-seconds must be an integer")?;
            }
            "--min-edge-bps" => {
                options.min_edge_bps = iter
                    .next()
                    .ok_or("--min-edge-bps requires a value")?
                    .parse()
                    .map_err(|_| "--min-edge-bps must be an integer")?;
            }
            "--help" | "-h" => return Err(usage().into()),
            other => return Err(format!("unknown argument: {other}\n{}", usage()).into()),
        }
    }
    if options.poll_interval_seconds == 0 {
        return Err("--poll-interval-seconds must be greater than 0".into());
    }
    if options.max_candidate_age_seconds <= 0 {
        return Err("--max-candidate-age-seconds must be greater than 0".into());
    }
    Ok(options)
}

fn usage() -> &'static str {
    "Usage: same-mint-yield-monitor [--once] [--execute] [--all-active-vaults | --settings <PUBKEY> --vault-index <N>] [--poll-interval-seconds <SECONDS>] [--rebalance-cooldown-seconds <SECONDS>] [--max-candidate-age-seconds <SECONDS>] [--min-edge-bps <BPS>]\n\nDry-run is the default. Fleet mode reads YIELD_ROUTER_KEYPAIR for DB discovery and never reads SOLANA_TESTING_PK. Explicit --settings/--vault-index mode does not read SOLANA_TESTING_PK. No-arg authority discovery mode is local dry-run/setup only and reads SOLANA_TESTING_PK. Live execution reads YIELD_ROUTER_KEYPAIR through same-mint-reserve-swap --optimization-cycle. The same-vault rebalance cooldown defaults to 300 seconds; pass --rebalance-cooldown-seconds 0 only for local/test disable."
}
