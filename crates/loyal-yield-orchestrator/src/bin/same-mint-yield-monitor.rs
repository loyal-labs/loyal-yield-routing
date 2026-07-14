use std::{
    cmp::Ordering,
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    path::PathBuf,
    process::Command,
    time::Duration as StdDuration,
};

use chrono::{DateTime, Duration, Utc};
use loyal_actions::USDC_MINT;
use loyal_yield_orchestrator::sqlx::Row;
use loyal_yield_orchestrator::{
    enabled_stable_mints_from_env, policy_keypair_from_env, route_amount_evidence,
    solana_testing_keypair_from_env, CurrentIdleTokenBalance, CurrentReservePosition, ManagedVault,
    NeonSqlClient, NeonSqlConfig, PolicyId, RoutePolicy, SupportedKaminoReserve, VaultId,
    ACTIVE_DECISION_STATUSES, SOLANA_TESTING_PK_ENV,
};
use loyal_yield_router::timescale::{
    SupportedReserveLatestRow, TimescaleRouterClient, TimescaleRouterClientConfig,
};
use serde_json::{json, Value};
use solana_sdk::{pubkey::Pubkey, signature::Signer};
use tokio::time::{sleep, Duration as TokioDuration};

const DEFAULT_POLL_INTERVAL_SECONDS: u64 = 300;
const DEFAULT_REBALANCE_COOLDOWN_SECONDS: u64 = 300;
const DEFAULT_MAX_CANDIDATE_AGE_SECONDS: i64 = 6 * 60 * 60;
const DEFAULT_MIN_EDGE_BPS: i64 = 1;
const DEFAULT_MIN_IDLE_DEPOSIT_RAW: i64 = 1_000_000;
const DEFAULT_FLEET_PAGE_SIZE: i64 = 50;
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
    min_idle_deposit_raw: i64,
    fleet_page_size: i64,
    enabled_mints: Vec<String>,
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

#[derive(Debug, Clone)]
struct PlannedIdleVaultDeposit {
    idle: CurrentIdleTokenBalance,
    target: SupportedReserveLatestRow,
    amount_raw: i64,
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
    liquidity_mint: Option<String>,
    source_liquidity_mint: Option<String>,
    target_liquidity_mint: Option<String>,
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
    // Keep exit visibility and entry eligibility separate. Known reserves may only supply
    // reconciliation/source identities; the safe latest rows remain the sole target input.
    let (candidates, known_reserves) = tokio::try_join!(
        load_safe_stable_candidates(timescale, &options.enabled_mints),
        load_known_stable_reserves(timescale, &options.enabled_mints),
    )?;
    if options.all_active_vaults {
        return run_fleet_once(
            options,
            optimizer_signer,
            neon,
            &candidates,
            &known_reserves,
        )
        .await;
    }

    let vault = resolve_vault(neon, authority, options).await?;
    let idle_balance = neon
        .current_idle_token_balance(vault.vault.id, &USDC_MINT.to_string())
        .await?;
    run_vault_once(
        options,
        vault,
        neon,
        &candidates,
        &known_reserves,
        idle_balance,
    )
    .await
}

async fn run_fleet_once(
    options: &Options,
    optimizer_signer: Option<Pubkey>,
    neon: &NeonSqlClient,
    candidates: &[SupportedReserveLatestRow],
    known_reserves: &[SupportedKaminoReserve],
) -> Result<Value, Box<dyn Error>> {
    let optimizer_signer = optimizer_signer.ok_or("POLICY_KEYPAIR signer was not loaded")?;
    let delegated_signer = optimizer_signer.to_string();
    let idle_mint = USDC_MINT.to_string();
    // Keep one poll finite even if new vaults are discovered while it is running.
    let scan_max_vault_id =
        fetch_max_active_vault_id(neon, &delegated_signer, &options.enabled_mints).await?;
    let mut idle_priority_count = 0usize;
    let mut discovered_vault_count = 0usize;
    if let Some(scan_max_vault_id) = scan_max_vault_id {
        let mut after_vault_id = 0i64;
        loop {
            let vaults = fetch_active_vault_page(
                neon,
                &delegated_signer,
                &options.enabled_mints,
                after_vault_id,
                scan_max_vault_id,
                options.fleet_page_size,
            )
            .await?;
            let Some(last_vault_id) = vaults.last().map(|vault| vault.vault.id.as_i64()) else {
                break;
            };
            let idle_balances = load_idle_balances_by_vault(neon, &vaults, &idle_mint).await?;
            for vault in vaults {
                let vault_identity = vault_json(&vault);
                let idle_balance = idle_balances.get(&vault.vault.id.as_i64()).cloned();
                let result = match run_vault_once(
                    options,
                    vault,
                    neon,
                    candidates,
                    known_reserves,
                    idle_balance,
                )
                .await
                {
                    Ok(result) => result,
                    Err(error) => json!({
                        "status": "vault_error",
                        "execute": options.execute,
                        "vault": vault_identity,
                        "error": error.to_string(),
                    }),
                };
                idle_priority_count += usize::from(
                    result
                        .get("plannedIdleVaultDeposit")
                        .is_some_and(|plan| !plan.is_null()),
                );
                println!("{}", serde_json::to_string(&result)?);
                discovered_vault_count += 1;
            }
            after_vault_id = last_vault_id;
        }
    }

    Ok(json!({
        "status": "fleet_poll",
        "execute": options.execute,
        "allActiveVaults": true,
        "enabledMints": options.enabled_mints.clone(),
        "discoveredVaultCount": discovered_vault_count,
        "scanMaxVaultId": scan_max_vault_id,
        "fleetPageSize": options.fleet_page_size,
        "candidateCount": candidates.len(),
        "knownSourceReserveCount": known_reserves.len(),
        "candidateCountsByMint": candidate_counts_by_mint(candidates),
        "knownSourceReserveCountsByMint": known_reserve_counts_by_mint(known_reserves),
        "skippedMints": skipped_mints(&options.enabled_mints, candidates),
        "pollIntervalSeconds": options.poll_interval_seconds,
        "rebalanceCooldownSeconds": options.rebalance_cooldown_seconds,
        "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
        "minEdgeBps": options.min_edge_bps,
        "minIdleDepositRaw": options.min_idle_deposit_raw,
        "idlePriorityDepositCount": idle_priority_count,
        "idleRoutingOrder": "per_vault_before_normal",
        "normalRebalancesDeferredForIdleDeposits": false,
    }))
}

async fn run_vault_once(
    options: &Options,
    vault: ResolvedVault,
    neon: &NeonSqlClient,
    candidates: &[SupportedReserveLatestRow],
    known_reserves: &[SupportedKaminoReserve],
    idle_balance: Option<CurrentIdleTokenBalance>,
) -> Result<Value, Box<dyn Error>> {
    let candidate_counts = candidate_counts_by_mint(candidates);
    let skipped_mint_list = skipped_mints(&options.enabled_mints, candidates);
    if !vault
        .policy
        .route_modes
        .iter()
        .any(|mode| mode == SAME_MINT_ROUTE_MODE)
    {
        return Ok(json!({
            "status": "skipped_policy_route_mode",
            "execute": options.execute,
            "skipReason": "policy_route_mode_missing",
            "enabledMints": options.enabled_mints.clone(),
            "vault": vault_json(&vault),
            "candidates": candidates_json(candidates),
            "policyEligibleCandidates": [],
            "candidateCountsByMint": candidate_counts,
            "policyEligibleCandidateCountsByMint": {},
            "skippedMints": skipped_mint_list,
            "requiredRouteMode": SAME_MINT_ROUTE_MODE,
        }));
    }
    let policy_candidates =
        policy_eligible_candidates(&vault.policy, candidates, &options.enabled_mints);
    let catalog_policy_source_reserves =
        policy_eligible_source_reserves(&vault.policy, known_reserves, &options.enabled_mints);
    let active_decisions = active_decision_count(neon, vault.vault.id).await?;
    if active_decisions > 0 {
        return Ok(json!({
            "status": "skipped_active_decision",
            "execute": options.execute,
            "skipReason": "active_decision",
            "enabledMints": options.enabled_mints.clone(),
            "vault": vault_json(&vault),
            "activeDecisionCount": active_decisions,
            "candidates": candidates_json(candidates),
            "policyEligibleCandidates": candidates_json(&policy_candidates),
            "candidateCountsByMint": candidate_counts,
            "policyEligibleCandidateCountsByMint": candidate_counts_by_mint(&policy_candidates),
            "skippedMints": skipped_mint_list,
            "rebalanceCooldownSeconds": options.rebalance_cooldown_seconds,
        }));
    }
    let persisted_positions_before_reconcile = neon.current_positions(vault.vault.id).await?;
    let policy_source_reserves = retain_persisted_policy_sources(
        &vault.policy,
        &options.enabled_mints,
        catalog_policy_source_reserves,
        &persisted_positions_before_reconcile,
    );

    let freshest_cutoff = Utc::now() - Duration::seconds(options.max_candidate_age_seconds);
    let (fresh_candidates, stale_candidate_count) =
        split_fresh_candidates(&policy_candidates, freshest_cutoff);
    let idle_plan = plan_idle_vault_deposit(
        idle_balance.as_ref(),
        &fresh_candidates,
        options.min_idle_deposit_raw,
    );
    if let Ok(Some(planned_idle_deposit)) = idle_plan.as_ref() {
        if options.execute {
            let execution =
                execute_idle_vault_deposit(&vault, planned_idle_deposit, &policy_source_reserves)?;
            let active_decision_count_after = active_decision_count(neon, vault.vault.id).await?;
            let execution_status = route_execution_status(&execution);
            if !execution.success || execution_status != "idle_vault_deposit_executed" {
                return Ok(json!({
                    "status": execution_status,
                    "execute": true,
                    "enabledMints": options.enabled_mints.clone(),
                    "vault": vault_json(&vault),
                    "activeDecisionCount": active_decisions,
                    "activeDecisionCountAfter": active_decision_count_after,
                    "idleVaultBalance": idle_balance.as_ref().map(idle_balance_json),
                    "plannedIdleVaultDeposit": idle_vault_deposit_json(Some(planned_idle_deposit)),
                    "routeExecution": route_execution_output_json(&execution),
                    "candidates": candidates_json(candidates),
                    "policyEligibleCandidates": candidates_json(&policy_candidates),
                    "candidateCountsByMint": candidate_counts,
                    "policyEligibleCandidateCountsByMint": candidate_counts_by_mint(&policy_candidates),
                    "freshCandidateCountsByMint": candidate_counts_by_mint(&fresh_candidates),
                    "skippedMints": skipped_mint_list,
                    "freshCandidateCount": fresh_candidates.len(),
                    "staleCandidateCount": stale_candidate_count,
                    "minIdleDepositRaw": options.min_idle_deposit_raw,
                }));
            }
            return Ok(json!({
                "status": "idle_vault_deposit_executed",
                "execute": true,
                "enabledMints": options.enabled_mints.clone(),
                "vault": vault_json(&vault),
                "activeDecisionCount": active_decisions,
                "activeDecisionCountAfter": active_decision_count_after,
                "idleVaultBalance": idle_balance.as_ref().map(idle_balance_json),
                "plannedIdleVaultDeposit": idle_vault_deposit_json(Some(planned_idle_deposit)),
                "routeExecution": route_execution_output_json(&execution),
                "candidates": candidates_json(candidates),
                "policyEligibleCandidates": candidates_json(&policy_candidates),
                "candidateCountsByMint": candidate_counts,
                "policyEligibleCandidateCountsByMint": candidate_counts_by_mint(&policy_candidates),
                "freshCandidateCountsByMint": candidate_counts_by_mint(&fresh_candidates),
                "skippedMints": skipped_mint_list,
                "freshCandidateCount": fresh_candidates.len(),
                "staleCandidateCount": stale_candidate_count,
                "minIdleDepositRaw": options.min_idle_deposit_raw,
            }));
        }
        return Ok(json!({
            "status": "planned_idle_vault_deposit_dry_run",
            "execute": false,
            "enabledMints": options.enabled_mints.clone(),
            "vault": vault_json(&vault),
            "activeDecisionCount": active_decisions,
            "idleVaultBalance": idle_balance.as_ref().map(idle_balance_json),
            "plannedIdleVaultDeposit": idle_vault_deposit_json(Some(planned_idle_deposit)),
            "candidates": candidates_json(candidates),
            "policyEligibleCandidates": candidates_json(&policy_candidates),
            "candidateCountsByMint": candidate_counts,
            "policyEligibleCandidateCountsByMint": candidate_counts_by_mint(&policy_candidates),
            "freshCandidateCountsByMint": candidate_counts_by_mint(&fresh_candidates),
            "skippedMints": skipped_mint_list,
            "freshCandidateCount": fresh_candidates.len(),
            "staleCandidateCount": stale_candidate_count,
            "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
            "minIdleDepositRaw": options.min_idle_deposit_raw,
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
            "enabledMints": options.enabled_mints.clone(),
            "vault": vault_json(&vault),
            "activeDecisionCount": active_decisions,
            "lastConfirmedRebalance": recent_confirmed_rebalance_json(&recent_rebalance),
            "lastConfirmedRebalanceAt": recent_rebalance.updated_at,
            "rebalanceCooldownSeconds": options.rebalance_cooldown_seconds,
            "cooldownRemainingSeconds": cooldown_remaining_seconds,
            "candidates": candidates_json(candidates),
            "policyEligibleCandidates": candidates_json(&policy_candidates),
            "candidateCountsByMint": candidate_counts,
            "policyEligibleCandidateCountsByMint": candidate_counts_by_mint(&policy_candidates),
            "skippedMints": skipped_mint_list,
            "idleVaultBalance": idle_balance.as_ref().map(idle_balance_json),
            "idleVaultDepositPlan": idle_vault_deposit_result_json(&idle_plan),
        }));
    }

    let reconcile = reconcile_current_positions_for_vault(&vault, &policy_source_reserves)?;
    if !reconcile.success {
        return Ok(json!({
            "status": "reconcile_failed",
            "execute": options.execute,
            "enabledMints": options.enabled_mints.clone(),
            "vault": vault_json(&vault),
            "chainReconcile": reconcile_output_json(&reconcile),
            "idleVaultBalance": idle_balance.as_ref().map(idle_balance_json),
            "idleVaultDepositPlan": idle_vault_deposit_result_json(&idle_plan),
            "candidates": candidates_json(candidates),
            "policyEligibleCandidates": candidates_json(&policy_candidates),
            "candidateCountsByMint": candidate_counts,
            "policyEligibleCandidateCountsByMint": candidate_counts_by_mint(&policy_candidates),
            "skippedMints": skipped_mint_list,
        }));
    }
    let positions = neon.current_positions(vault.vault.id).await?;
    let policy_positions = policy_eligible_positions(&positions, &policy_source_reserves);

    let plan = if fresh_candidates.is_empty() {
        Err("no_eligible_fresh_candidate_data".to_owned())
    } else {
        // Never widen this target argument to the source/reconciliation universe.
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
                "enabledMints": options.enabled_mints.clone(),
                "vault": vault_json(&vault),
                "activeDecisionCount": active_decisions,
                "activeDecisionCountAfter": active_decision_count_after,
                "currentPositions": positions_json(&positions),
                "policyEligibleCurrentPositions": positions_json(&policy_positions),
                "currentPositionsAfter": positions_json(&positions_after),
                "chainReconcile": reconcile_output_json(&reconcile),
                "idleVaultBalance": idle_balance.as_ref().map(idle_balance_json),
                "idleVaultDepositPlan": idle_vault_deposit_result_json(&idle_plan),
                "candidates": candidates_json(candidates),
                "policyEligibleCandidates": candidates_json(&policy_candidates),
                "candidateCountsByMint": candidate_counts,
                "policyEligibleCandidateCountsByMint": candidate_counts_by_mint(&policy_candidates),
                "freshCandidateCountsByMint": candidate_counts_by_mint(&fresh_candidates),
                "skippedMints": skipped_mint_list,
                "freshCandidateCount": fresh_candidates.len(),
                "staleCandidateCount": stale_candidate_count,
                "plannedMove": planned_move_json(Some(planned_move)),
                "routeExecution": route_execution_output_json(&execution),
            }));
        }
        return Ok(json!({
            "status": "executed",
            "execute": true,
            "enabledMints": options.enabled_mints.clone(),
            "vault": vault_json(&vault),
            "activeDecisionCount": active_decisions,
            "activeDecisionCountAfter": active_decision_count_after,
            "currentPositions": positions_json(&positions),
            "policyEligibleCurrentPositions": positions_json(&policy_positions),
            "currentPositionsAfter": positions_json(&positions_after),
            "chainReconcile": reconcile_output_json(&reconcile),
            "idleVaultBalance": idle_balance.as_ref().map(idle_balance_json),
            "idleVaultDepositPlan": idle_vault_deposit_result_json(&idle_plan),
            "candidates": candidates_json(candidates),
            "policyEligibleCandidates": candidates_json(&policy_candidates),
            "candidateCountsByMint": candidate_counts,
            "policyEligibleCandidateCountsByMint": candidate_counts_by_mint(&policy_candidates),
            "freshCandidateCountsByMint": candidate_counts_by_mint(&fresh_candidates),
            "skippedMints": skipped_mint_list,
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
        "enabledMints": options.enabled_mints.clone(),
        "vault": vault_json(&vault),
        "activeDecisionCount": active_decisions,
        "currentPositions": positions_json(&positions),
        "policyEligibleCurrentPositions": positions_json(&policy_positions),
        "chainReconcile": reconcile_output_json(&reconcile),
        "idleVaultBalance": idle_balance.as_ref().map(idle_balance_json),
        "idleVaultDepositPlan": idle_vault_deposit_result_json(&idle_plan),
        "candidates": candidates_json(candidates),
        "policyEligibleCandidates": candidates_json(&policy_candidates),
        "candidateCountsByMint": candidate_counts,
        "policyEligibleCandidateCountsByMint": candidate_counts_by_mint(&policy_candidates),
        "freshCandidateCountsByMint": candidate_counts_by_mint(&fresh_candidates),
        "skippedMints": skipped_mint_list,
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
        .arg("--expected-liquidity-mint")
        .arg(&planned_move.source.liquidity_mint)
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

fn execute_idle_vault_deposit(
    vault: &ResolvedVault,
    plan: &PlannedIdleVaultDeposit,
    policy_source_reserves: &[SupportedKaminoReserve],
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
    command
        .arg("--settings")
        .arg(&vault.vault.settings)
        .arg("--vault-index")
        .arg(vault.vault.vault_index.to_string())
        .arg("--deposit-idle-vault-reserve")
        .arg(&plan.target.reserve)
        .arg(plan.amount_raw.to_string())
        .arg("--expected-idle-token-account")
        .arg(&plan.idle.token_account)
        .arg("--expected-idle-observed-slot")
        .arg(plan.idle.observed_slot.to_string())
        .arg("--expected-idle-observed-at")
        .arg(plan.idle.observed_at.to_rfc3339())
        .arg("--expected-liquidity-mint")
        .arg(&plan.idle.mint)
        .arg("--expected-amount-raw")
        .arg(plan.amount_raw.to_string())
        .arg("--expected-target-apy-bps")
        .arg(plan.target_apy_bps.to_string())
        .arg("--expected-edge-bps")
        .arg(plan.edge_bps.to_string())
        .arg("--reconcile-from-chain")
        .arg("--execute");
    for reserve in idle_deposit_post_reconcile_reserves(policy_source_reserves) {
        command.arg("--reconcile-reserve").arg(reserve);
    }

    let output = command.output()?;
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

fn idle_deposit_post_reconcile_reserves(
    policy_source_reserves: &[SupportedKaminoReserve],
) -> Vec<String> {
    let mut reserves = Vec::new();
    for source_reserve in policy_source_reserves {
        if !reserves
            .iter()
            .any(|reserve| reserve == &source_reserve.reserve)
        {
            reserves.push(source_reserve.reserve.clone());
        }
    }
    reserves
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
    policy_source_reserves: &[SupportedKaminoReserve],
) -> Result<ReconcileOutput, Box<dyn Error>> {
    let reserves = reconcile_reserves_for_source_universe(policy_source_reserves);
    if reserves.len() < 2 {
        return Err(
            "chain reconciliation requires at least two policy-eligible reserves with the same liquidity mint".into(),
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

fn reconcile_reserves_for_source_universe(
    source_reserves: &[SupportedKaminoReserve],
) -> Vec<String> {
    let mut same_mint_pair = Vec::new();
    'outer: for source in source_reserves {
        for target in source_reserves {
            if source.reserve != target.reserve && source.liquidity_mint == target.liquidity_mint {
                same_mint_pair.push(source.reserve.clone());
                same_mint_pair.push(target.reserve.clone());
                break 'outer;
            }
        }
    }
    if same_mint_pair.len() < 2 {
        return Vec::new();
    }

    let mut reserves = Vec::new();
    for reserve in same_mint_pair {
        if !reserves.iter().any(|existing| existing == &reserve) {
            reserves.push(reserve);
        }
    }
    for source_reserve in source_reserves {
        if !reserves
            .iter()
            .any(|reserve| reserve == &source_reserve.reserve)
        {
            reserves.push(source_reserve.reserve.clone());
        }
    }
    reserves
}

async fn load_safe_stable_candidates(
    timescale: &TimescaleRouterClient,
    enabled_mints: &[String],
) -> Result<Vec<SupportedReserveLatestRow>, Box<dyn Error>> {
    if enabled_mints.is_empty() {
        return Ok(Vec::new());
    }

    let rows = loyal_yield_orchestrator::sqlx::query_as::<_, SupportedReserveLatestRow>(
        r#"
        SELECT l.observed_at,
               l.slot,
               l.reserve,
               l.market,
               l.market_name,
               l.liquidity_mint,
               l.symbol,
               l.supply_apy,
               l.borrow_apy,
               l.total_supply_usd_estimate,
               l.reserve_last_update_stale
        FROM kamino.supported_reserves sr
        JOIN kamino.latest_reserve_updates l
          ON l.reserve = sr.reserve
         AND l.market = sr.market
         AND l.liquidity_mint = sr.liquidity_mint
        WHERE sr.active = true
          AND $1 = ANY(sr.risk_baskets)
          AND sr.liquidity_mint = ANY($2)
          AND l.reserve_last_update_stale = false
          AND l.total_supply_usd_estimate > $3
          AND l.supply_apy >= $4
          AND l.supply_apy < $5
        ORDER BY l.supply_apy DESC, l.observed_at DESC, l.reserve ASC
        "#,
    )
    .bind("safe")
    .bind(enabled_mints.to_vec())
    .bind(100_000.0_f64)
    .bind(0.0_f64)
    .bind(0.5_f64)
    .fetch_all(timescale.pool())
    .await?;

    Ok(rows)
}

async fn load_known_stable_reserves(
    timescale: &TimescaleRouterClient,
    enabled_mints: &[String],
) -> Result<Vec<SupportedKaminoReserve>, Box<dyn Error>> {
    if enabled_mints.is_empty() {
        return Ok(Vec::new());
    }

    // This is the source/reconciliation universe, not the target universe. Inactive or
    // no-longer-safe rows must remain visible so an existing on-chain position can exit them.
    let rows = loyal_yield_orchestrator::sqlx::query_as::<_, SupportedKaminoReserve>(
        r#"
        SELECT sr.market,
               sr.liquidity_mint,
               sr.reserve,
               sr.market_name,
               sr.symbol,
               sr.updated_at
        FROM kamino.supported_reserves sr
        WHERE sr.liquidity_mint = ANY($1::TEXT[])
        ORDER BY sr.market, sr.liquidity_mint, sr.reserve
        "#,
    )
    .bind(enabled_mints.to_vec())
    .fetch_all(timescale.pool())
    .await?;

    Ok(rows)
}

fn policy_eligible_candidates(
    policy: &RoutePolicy,
    candidates: &[SupportedReserveLatestRow],
    enabled_mints: &[String],
) -> Vec<SupportedReserveLatestRow> {
    let enabled = enabled_mints
        .iter()
        .map(String::as_str)
        .collect::<BTreeSet<_>>();
    candidates
        .iter()
        .filter(|candidate| {
            candidate.market.as_ref().is_some_and(|market| {
                policy_allows_reserve(policy, &enabled, market, &candidate.liquidity_mint)
            })
        })
        .cloned()
        .collect()
}

fn policy_eligible_source_reserves(
    policy: &RoutePolicy,
    known_reserves: &[SupportedKaminoReserve],
    enabled_mints: &[String],
) -> Vec<SupportedKaminoReserve> {
    let enabled = enabled_mints
        .iter()
        .map(String::as_str)
        .collect::<BTreeSet<_>>();
    known_reserves
        .iter()
        .filter(|reserve| {
            policy_allows_reserve(policy, &enabled, &reserve.market, &reserve.liquidity_mint)
        })
        .cloned()
        .collect()
}

fn retain_persisted_policy_sources(
    policy: &RoutePolicy,
    enabled_mints: &[String],
    catalog_sources: Vec<SupportedKaminoReserve>,
    persisted_positions: &[CurrentReservePosition],
) -> Vec<SupportedKaminoReserve> {
    let enabled = enabled_mints
        .iter()
        .map(String::as_str)
        .collect::<BTreeSet<_>>();
    let mut sources = catalog_sources
        .into_iter()
        .map(|source| (source.reserve.clone(), source))
        .collect::<BTreeMap<_, _>>();

    // Persisted valued positions are a retention fence. A reserve can disappear from the
    // mutable Timescale catalog after funds were deposited, but it must stay in chain
    // reconciliation until the holding is observed at zero.
    for position in persisted_positions
        .iter()
        .filter(|position| position.has_value || position.amount_raw > 0)
    {
        let Some(market) = position.market.as_deref() else {
            continue;
        };
        if !policy_allows_reserve(policy, &enabled, market, &position.liquidity_mint) {
            continue;
        }
        sources
            .entry(position.reserve.clone())
            .or_insert_with(|| SupportedKaminoReserve {
                market: market.to_owned(),
                liquidity_mint: position.liquidity_mint.clone(),
                reserve: position.reserve.clone(),
                market_name: None,
                symbol: None,
                updated_at: position.observed_at,
            });
    }

    sources.into_values().collect()
}

fn policy_allows_reserve(
    policy: &RoutePolicy,
    enabled_mints: &BTreeSet<&str>,
    market: &str,
    liquidity_mint: &str,
) -> bool {
    enabled_mints.contains(liquidity_mint)
        && policy
            .kamino_markets
            .iter()
            .any(|allowed_market| allowed_market == market)
        && policy
            .stable_mints
            .iter()
            .any(|mint| mint == liquidity_mint)
        && policy
            .kamino_liquidity_mints
            .iter()
            .any(|mint| mint == liquidity_mint)
}

fn policy_eligible_positions(
    positions: &[CurrentReservePosition],
    policy_source_reserves: &[SupportedKaminoReserve],
) -> Vec<CurrentReservePosition> {
    let eligible_reserves = policy_source_reserves
        .iter()
        .map(|reserve| reserve.reserve.as_str())
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
    let valued_positions = positions
        .iter()
        .filter(|position| position.has_value && position.amount_raw > 0)
        .collect::<Vec<_>>();
    if valued_positions.is_empty() {
        return Err("no_value_source".to_owned());
    }
    let unsupported_value_source_exists = valued_positions
        .iter()
        .any(|position| route_amount_evidence(position).is_none());
    let mut best: Option<PlannedMonitorMove> = None;
    for source in valued_positions {
        let Some(evidence) = route_amount_evidence(source) else {
            continue;
        };
        let Some(target) = candidates
            .iter()
            .filter(|candidate| {
                candidate.liquidity_mint == source.liquidity_mint
                    && candidate.reserve != source.reserve
            })
            .max_by(|left, right| compare_candidate_preference(left, right))
            .cloned()
        else {
            continue;
        };
        let source_apy_bps = candidates
            .iter()
            .find(|candidate| candidate.reserve == source.reserve)
            .map(|candidate| apy_to_bps(candidate.supply_apy))
            .or(source.supply_apy_bps)
            .unwrap_or_default();
        let target_apy_bps = apy_to_bps(target.supply_apy);
        let edge_bps = target_apy_bps - source_apy_bps;
        if edge_bps < min_edge_bps {
            continue;
        }
        let candidate = PlannedMonitorMove {
            source: source.clone(),
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
        };
        if best
            .as_ref()
            .is_none_or(|current| compare_plan_preference(&candidate, current).is_gt())
        {
            best = Some(candidate);
        }
    }
    if best.is_none() && unsupported_value_source_exists {
        return Err("unsupported_amount_semantics".to_owned());
    }
    Ok(best)
}

fn plan_idle_vault_deposit(
    idle: Option<&CurrentIdleTokenBalance>,
    candidates: &[SupportedReserveLatestRow],
    min_idle_deposit_raw: i64,
) -> Result<Option<PlannedIdleVaultDeposit>, String> {
    let Some(idle) = idle else {
        return Ok(None);
    };
    if idle.amount_raw <= 0 {
        return Ok(None);
    }
    if idle.mint != USDC_MINT.to_string() {
        return Err("idle_vault_liquidity_non_usdc".to_owned());
    }
    if idle.amount_raw < min_idle_deposit_raw {
        return Err("idle_vault_liquidity_below_threshold".to_owned());
    }
    let Some(target) = candidates
        .iter()
        .filter(|candidate| candidate.liquidity_mint == idle.mint)
        .max_by(|left, right| compare_candidate_preference(left, right))
        .cloned()
    else {
        return Err("no_eligible_fresh_candidate_data".to_owned());
    };
    let target_apy_bps = apy_to_bps(target.supply_apy);
    let edge_bps = target_apy_bps;
    if edge_bps <= 0 {
        return Err("no_positive_idle_vault_deposit_edge".to_owned());
    }
    Ok(Some(PlannedIdleVaultDeposit {
        idle: idle.clone(),
        target,
        amount_raw: idle.amount_raw,
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
        Ok(Some(policy_keypair_from_env()?.pubkey()))
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

async fn fetch_max_active_vault_id(
    neon: &NeonSqlClient,
    delegated_signer: &str,
    enabled_mints: &[String],
) -> Result<Option<i64>, Box<dyn Error>> {
    let max_vault_id = loyal_yield_orchestrator::sqlx::query_scalar::<_, Option<i64>>(
        r#"
        SELECT MAX(v.id)
        FROM loyal_yield.managed_vaults v
        JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
        WHERE v.active = true
          AND p.active = true
          AND $1 = ANY(p.delegated_signers)
          AND $2 = ANY(p.route_modes)
          AND p.stable_mints && $3::TEXT[]
          AND p.kamino_liquidity_mints && $3::TEXT[]
          AND cardinality(p.kamino_markets) > 0
        "#,
    )
    .bind(delegated_signer)
    .bind(SAME_MINT_ROUTE_MODE)
    .bind(enabled_mints)
    .fetch_one(neon.pool())
    .await?;
    Ok(max_vault_id)
}

async fn fetch_active_vault_page(
    neon: &NeonSqlClient,
    delegated_signer: &str,
    enabled_mints: &[String],
    after_vault_id: i64,
    scan_max_vault_id: i64,
    page_size: i64,
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
          AND p.stable_mints && $3::TEXT[]
          AND p.kamino_liquidity_mints && $3::TEXT[]
          AND cardinality(p.kamino_markets) > 0
          AND v.id > $4
          AND v.id <= $5
        ORDER BY v.id
        LIMIT $6
        "#,
    )
    .bind(delegated_signer)
    .bind(SAME_MINT_ROUTE_MODE)
    .bind(enabled_mints)
    .bind(after_vault_id)
    .bind(scan_max_vault_id)
    .bind(page_size)
    .fetch_all(neon.pool())
    .await?;
    rows.into_iter().map(resolved_vault_from_row).collect()
}

async fn load_idle_balances_by_vault(
    neon: &NeonSqlClient,
    vaults: &[ResolvedVault],
    mint: &str,
) -> Result<BTreeMap<i64, CurrentIdleTokenBalance>, Box<dyn Error>> {
    let vault_ids = vaults
        .iter()
        .map(|vault| vault.vault.id)
        .collect::<Vec<_>>();
    let balances = neon
        .current_idle_token_balances_for_vaults(&vault_ids, mint)
        .await?;
    let mut by_vault = BTreeMap::<i64, CurrentIdleTokenBalance>::new();
    for balance in balances {
        by_vault.insert(balance.vault_id.as_i64(), balance);
    }
    Ok(by_vault)
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
        SELECT
            id,
            updated_at,
            source_reserve,
            target_reserve,
            liquidity_mint,
            source_liquidity_mint,
            target_liquidity_mint,
            signature
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
            liquidity_mint: row.try_get("liquidity_mint")?,
            source_liquidity_mint: row.try_get("source_liquidity_mint")?,
            target_liquidity_mint: row.try_get("target_liquidity_mint")?,
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

fn compare_plan_preference(left: &PlannedMonitorMove, right: &PlannedMonitorMove) -> Ordering {
    left.edge_bps
        .cmp(&right.edge_bps)
        .then_with(|| left.target_apy_bps.cmp(&right.target_apy_bps))
        .then_with(|| left.amount_raw.cmp(&right.amount_raw))
        .then_with(|| left.source.liquidity_mint.cmp(&right.source.liquidity_mint))
        .then_with(|| right.target.reserve.cmp(&left.target.reserve))
}

fn candidate_counts_by_mint(candidates: &[SupportedReserveLatestRow]) -> BTreeMap<String, usize> {
    let mut counts = BTreeMap::new();
    for candidate in candidates {
        *counts.entry(candidate.liquidity_mint.clone()).or_insert(0) += 1;
    }
    counts
}

fn known_reserve_counts_by_mint(reserves: &[SupportedKaminoReserve]) -> BTreeMap<String, usize> {
    let mut counts = BTreeMap::new();
    for reserve in reserves {
        *counts.entry(reserve.liquidity_mint.clone()).or_insert(0) += 1;
    }
    counts
}

fn skipped_mints(
    enabled_mints: &[String],
    candidates: &[SupportedReserveLatestRow],
) -> Vec<String> {
    let observed = candidates
        .iter()
        .map(|candidate| candidate.liquidity_mint.as_str())
        .collect::<BTreeSet<_>>();
    enabled_mints
        .iter()
        .filter(|mint| !observed.contains(mint.as_str()))
        .cloned()
        .collect()
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
            "sourceLiquidityMint": plan.source.liquidity_mint,
            "targetLiquidityMint": plan.target.liquidity_mint,
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

fn idle_balance_json(balance: &CurrentIdleTokenBalance) -> Value {
    json!({
        "vaultId": balance.vault_id.as_i64(),
        "mint": balance.mint,
        "amountRaw": balance.amount_raw,
        "owner": balance.owner,
        "tokenAccount": balance.token_account,
        "observedSlot": balance.observed_slot,
        "observedAt": balance.observed_at,
        "sourceCommitment": balance.source_commitment,
        "updatedAt": balance.updated_at,
    })
}

fn idle_vault_deposit_json(plan: Option<&PlannedIdleVaultDeposit>) -> Value {
    match plan {
        Some(plan) => json!({
            "kind": "idle_vault_deposit",
            "sourceKind": "idle_vault",
            "sourceApyBps": 0,
            "targetReserve": plan.target.reserve,
            "targetMarket": plan.target.market,
            "liquidityMint": plan.idle.mint,
            "amountRaw": plan.amount_raw,
            "idleVaultLiquidityAmountRaw": plan.amount_raw,
            "idleTokenAccount": plan.idle.token_account,
            "idleObservedSlot": plan.idle.observed_slot,
            "idleObservedAt": plan.idle.observed_at,
            "targetApyBps": plan.target_apy_bps,
            "estimatedEdgeBps": plan.edge_bps,
        }),
        None => Value::Null,
    }
}

fn idle_vault_deposit_result_json(plan: &Result<Option<PlannedIdleVaultDeposit>, String>) -> Value {
    match plan {
        Ok(Some(plan)) => idle_vault_deposit_json(Some(plan)),
        Ok(None) => Value::Null,
        Err(reason) => json!({
            "status": "skipped",
            "skipReason": reason,
        }),
    }
}

fn recent_confirmed_rebalance_json(rebalance: &RecentConfirmedRebalance) -> Value {
    json!({
        "id": rebalance.id,
        "updatedAt": rebalance.updated_at,
        "sourceReserve": rebalance.source_reserve,
        "targetReserve": rebalance.target_reserve,
        "liquidityMint": rebalance.liquidity_mint,
        "sourceLiquidityMint": rebalance.source_liquidity_mint,
        "targetLiquidityMint": rebalance.target_liquidity_mint,
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
        min_idle_deposit_raw: DEFAULT_MIN_IDLE_DEPOSIT_RAW,
        fleet_page_size: DEFAULT_FLEET_PAGE_SIZE,
        enabled_mints: enabled_stable_mints_from_env()?,
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
            "--min-idle-deposit-raw" => {
                options.min_idle_deposit_raw = iter
                    .next()
                    .ok_or("--min-idle-deposit-raw requires a value")?
                    .parse()
                    .map_err(|_| "--min-idle-deposit-raw must be an integer")?;
            }
            "--fleet-page-size" => {
                options.fleet_page_size = iter
                    .next()
                    .ok_or("--fleet-page-size requires a value")?
                    .parse()
                    .map_err(|_| "--fleet-page-size must be an integer")?;
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
    if options.min_idle_deposit_raw <= 0 {
        return Err("--min-idle-deposit-raw must be greater than 0".into());
    }
    if options.fleet_page_size <= 0 {
        return Err("--fleet-page-size must be greater than 0".into());
    }
    Ok(options)
}

fn usage() -> &'static str {
    "Usage: same-mint-yield-monitor [--once] [--execute] [--all-active-vaults | --settings <PUBKEY> --vault-index <N>] [--fleet-page-size <COUNT>] [--poll-interval-seconds <SECONDS>] [--rebalance-cooldown-seconds <SECONDS>] [--max-candidate-age-seconds <SECONDS>] [--min-edge-bps <BPS>] [--min-idle-deposit-raw <RAW>]\n\nDry-run is the default. Fleet mode reads POLICY_KEYPAIR for DB discovery, pages active vaults in batches of 50 by default, and never reads SOLANA_TESTING_PK. Explicit --settings/--vault-index mode does not read SOLANA_TESTING_PK. No-arg authority discovery mode is local dry-run/setup only and reads SOLANA_TESTING_PK. Live fleet execution passes idle-vault deposits through same-mint-reserve-swap, which reads POLICY_KEYPAIR as the delegated policy signer and transaction fee payer. Set EARN_ROUTER_ENABLED_STABLE_MINTS to a comma-separated subset of supported stable mint addresses for staged rollout. Idle-vault deposit routing is USDC-only and defaults to a 1_000_000 raw-unit threshold. The same-vault rebalance cooldown defaults to 300 seconds; pass --rebalance-cooldown-seconds 0 only for local/test disable."
}

#[cfg(test)]
mod tests {
    use super::*;
    use loyal_yield_orchestrator::SnapshotId;

    const MINT: &str = "enabled-stable-mint";
    const ALLOWED_MARKET: &str = "allowed-market";
    const UNSAFE_SOURCE: &str = "unsafe-or-inactive-held-source";
    const SAFE_TARGET: &str = "safe-active-target";

    fn policy() -> RoutePolicy {
        RoutePolicy {
            id: PolicyId(1),
            settings: "settings".to_owned(),
            authority: "authority".to_owned(),
            policy_seed: 1,
            policy_account: "policy-account".to_owned(),
            vault_index: 0,
            vault_pubkey: "vault".to_owned(),
            delegated_signers: Vec::new(),
            threshold: 1,
            route_modes: vec![SAME_MINT_ROUTE_MODE.to_owned()],
            stable_mints: vec![MINT.to_owned()],
            kamino_markets: vec![ALLOWED_MARKET.to_owned()],
            kamino_liquidity_mints: vec![MINT.to_owned()],
            universe_preset: None,
            risk_profile: Some("safe".to_owned()),
            swap_lanes: json!({}),
            active: true,
            first_seen_at: Utc::now(),
            last_seen_at: Utc::now(),
            last_seen_slot: 1,
            last_seen_signature: "signature".to_owned(),
        }
    }

    fn known_reserve(reserve: &str, market: &str, mint: &str) -> SupportedKaminoReserve {
        SupportedKaminoReserve {
            market: market.to_owned(),
            liquidity_mint: mint.to_owned(),
            reserve: reserve.to_owned(),
            market_name: None,
            symbol: None,
            updated_at: Utc::now(),
        }
    }

    fn safe_target(reserve: &str) -> SupportedReserveLatestRow {
        SupportedReserveLatestRow {
            observed_at: Utc::now(),
            slot: 100,
            reserve: reserve.to_owned(),
            market: Some(ALLOWED_MARKET.to_owned()),
            market_name: None,
            liquidity_mint: MINT.to_owned(),
            symbol: None,
            supply_apy: 0.02,
            borrow_apy: 0.03,
            total_supply_usd_estimate: 1_000_000.0,
            reserve_last_update_stale: false,
        }
    }

    fn held_position(reserve: &str, supply_apy_bps: Option<i64>) -> CurrentReservePosition {
        CurrentReservePosition {
            vault_id: VaultId(1),
            reserve: reserve.to_owned(),
            market: Some(ALLOWED_MARKET.to_owned()),
            liquidity_mint: MINT.to_owned(),
            amount_raw: 1_000_000,
            has_value: true,
            supply_apy_bps,
            borrow_apy_bps: None,
            snapshot_id: SnapshotId(1),
            observed_slot: 100,
            observed_at: Utc::now(),
            planning_metadata: json!({
                "amount_semantics": "redeemable_liquidity_amount",
            }),
        }
    }

    #[test]
    fn held_source_missing_from_timescale_can_move_to_safe_target() {
        let policy = policy();
        let enabled_mints = vec![MINT.to_owned()];
        let catalog_sources = policy_eligible_source_reserves(
            &policy,
            &[known_reserve(SAFE_TARGET, ALLOWED_MARKET, MINT)],
            &enabled_mints,
        );
        let held_source = held_position(UNSAFE_SOURCE, Some(25));
        let source_universe = retain_persisted_policy_sources(
            &policy,
            &enabled_mints,
            catalog_sources,
            std::slice::from_ref(&held_source),
        );
        let targets =
            policy_eligible_candidates(&policy, &[safe_target(SAFE_TARGET)], &enabled_mints);
        let sources = policy_eligible_positions(&[held_source], &source_universe);

        let plan = plan_move(&sources, &targets, 1)
            .expect("held source has supported amount semantics")
            .expect("safe target has a positive same-mint edge");

        assert_eq!(plan.source.reserve, UNSAFE_SOURCE);
        assert_eq!(plan.target.reserve, SAFE_TARGET);
        assert_eq!(plan.edge_bps, 175);
        assert!(source_universe
            .iter()
            .any(|reserve| reserve.reserve == UNSAFE_SOURCE));
    }

    #[test]
    fn timescale_missing_source_is_released_after_chain_observed_zero() {
        let policy = policy();
        let enabled_mints = vec![MINT.to_owned()];
        let catalog_sources = policy_eligible_source_reserves(
            &policy,
            &[known_reserve(SAFE_TARGET, ALLOWED_MARKET, MINT)],
            &enabled_mints,
        );
        let mut observed_zero = held_position(UNSAFE_SOURCE, Some(25));
        observed_zero.amount_raw = 0;
        observed_zero.has_value = false;

        let source_universe = retain_persisted_policy_sources(
            &policy,
            &enabled_mints,
            catalog_sources,
            &[observed_zero],
        );

        assert!(source_universe
            .iter()
            .all(|reserve| reserve.reserve != UNSAFE_SOURCE));
        assert!(source_universe
            .iter()
            .any(|reserve| reserve.reserve == SAFE_TARGET));
    }

    #[test]
    fn source_universe_never_expands_target_eligibility() {
        let policy = policy();
        let enabled_mints = vec![MINT.to_owned()];
        let source_universe = policy_eligible_source_reserves(
            &policy,
            &[
                known_reserve(UNSAFE_SOURCE, ALLOWED_MARKET, MINT),
                known_reserve(SAFE_TARGET, ALLOWED_MARKET, MINT),
            ],
            &enabled_mints,
        );
        let targets =
            policy_eligible_candidates(&policy, &[safe_target(SAFE_TARGET)], &enabled_mints);

        assert!(source_universe
            .iter()
            .any(|reserve| reserve.reserve == UNSAFE_SOURCE));
        assert!(targets
            .iter()
            .all(|candidate| candidate.reserve != UNSAFE_SOURCE));

        let plan = plan_move(&[held_position(UNSAFE_SOURCE, None)], &targets, 1)
            .expect("held source has supported amount semantics")
            .expect("safe target has a positive same-mint edge");
        assert_eq!(plan.target.reserve, SAFE_TARGET);
    }

    #[test]
    fn source_universe_still_enforces_policy_market_and_mint_boundaries() {
        let policy = policy();
        let enabled_mints = vec![MINT.to_owned()];
        let source_universe = policy_eligible_source_reserves(
            &policy,
            &[
                known_reserve(UNSAFE_SOURCE, ALLOWED_MARKET, MINT),
                known_reserve("wrong-market", "not-allowed", MINT),
                known_reserve("wrong-mint", ALLOWED_MARKET, "not-enabled"),
            ],
            &enabled_mints,
        );

        assert_eq!(source_universe.len(), 1);
        assert_eq!(source_universe[0].reserve, UNSAFE_SOURCE);
    }
}
