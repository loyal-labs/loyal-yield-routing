use std::{
    cmp::Ordering,
    env,
    error::Error,
    path::PathBuf,
    process::{Command, Output},
    time::{Duration as StdDuration, Instant},
};

use chrono::{Duration, Utc};
use loyal_actions::{KAMINO_MAIN_USDC_RESERVE, USDC_MINT};
use loyal_yield_orchestrator::{
    solana_testing_keypair_from_env,
    sqlx::{postgres::PgPoolOptions, PgPool, Row},
    yield_router_keypair_from_env,
};
use loyal_yield_router::timescale::{
    SupportedReserveLatestQuery, SupportedReserveLatestRow, TimescaleRouterClient,
    TimescaleRouterClientConfig,
};
use serde_json::{json, Value};
use solana_sdk::signature::Signer;
use tokio::time::{sleep, Duration as TokioDuration};

const DEFAULT_VAULT_INDEX: i16 = 1;
const DEFAULT_POLL_INTERVAL_SECONDS: u64 = 15;
const DEFAULT_TIMEOUT_SECONDS: u64 = 300;
const DEFAULT_MAX_CANDIDATE_AGE_SECONDS: i64 = 6 * 60 * 60;

#[derive(Debug, Clone)]
struct Options {
    settings: String,
    vault_index: i16,
    amount_raw: u64,
    poll_interval_seconds: u64,
    timeout_seconds: u64,
    max_candidate_age_seconds: i64,
    execute: bool,
}

#[derive(Debug, Clone)]
struct EdgePrecondition {
    main: SupportedReserveLatestRow,
    best: SupportedReserveLatestRow,
    edge_bps: i64,
}

#[derive(Debug)]
struct ChildOutput {
    success: bool,
    status_code: Option<i32>,
    stdout_json: Option<Value>,
    stdout_text: Option<String>,
    stderr_text: String,
}

#[derive(Debug, Clone)]
struct SignerBoundary {
    setup_authority: String,
    optimizer: String,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let options = parse_args(env::args().skip(1))?;
    log_event(
        "start",
        json!({
            "execute": options.execute,
            "settings": options.settings,
            "vaultIndex": options.vault_index,
            "amountRaw": options.amount_raw.to_string(),
            "pollIntervalSeconds": options.poll_interval_seconds,
            "timeoutSeconds": options.timeout_seconds,
            "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
        }),
    );
    let timescale_url = env::var("TIMESCALEDB_URL").map_err(|_| "TIMESCALEDB_URL is required")?;
    log_event("timescale_connect_start", json!({}));
    let timescale = TimescaleRouterClient::connect(
        TimescaleRouterClientConfig::new(timescale_url)
            .with_max_connections(1)
            .with_schema("kamino"),
    )
    .await?;
    log_event(
        "timescale_candidates_start",
        json!({ "mint": USDC_MINT.to_string() }),
    );
    let candidates = timescale
        .latest_supported_reserves(SupportedReserveLatestQuery::safe_usdc(
            USDC_MINT.to_string(),
        ))
        .await?;
    log_event(
        "timescale_candidates_loaded",
        json!({
            "candidateCount": candidates.len(),
            "reserves": candidates
                .iter()
                .map(|candidate| candidate.reserve.as_str())
                .collect::<Vec<_>>(),
        }),
    );
    let fresh_candidates = fresh_candidates(&candidates, options.max_candidate_age_seconds);
    log_event(
        "fresh_candidates_filtered",
        json!({
            "freshCandidateCount": fresh_candidates.len(),
            "staleCandidateCount": candidates.len().saturating_sub(fresh_candidates.len()),
            "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
        }),
    );
    if fresh_candidates.is_empty() {
        let report = json!({
            "reason": "no_fresh_safe_usdc_candidates",
            "candidateCount": candidates.len(),
            "freshCandidateCount": 0,
            "staleCandidateCount": candidates.len(),
            "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
            "candidates": candidates_json(&candidates),
        });
        log_event(
            "blocked_no_fresh_candidate_precondition",
            json!({ "precondition": report }),
        );
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "blocked_no_fresh_candidate_precondition",
                "execute": options.execute,
                "precondition": report,
                "checkedAt": Utc::now(),
            }))?
        );
        return Err("blocked_no_fresh_candidate_precondition".into());
    }
    let precondition = match positive_main_to_best_edge(&fresh_candidates) {
        Ok(edge) => edge,
        Err(report) => {
            log_event(
                "blocked_no_positive_edge_precondition",
                json!({
                    "precondition": report,
                    "freshCandidateCount": fresh_candidates.len(),
                    "staleCandidateCount": candidates.len().saturating_sub(fresh_candidates.len()),
                    "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
                }),
            );
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "blocked_no_positive_edge_precondition",
                    "execute": options.execute,
                    "precondition": report,
                    "freshCandidateCount": fresh_candidates.len(),
                    "staleCandidateCount": candidates.len().saturating_sub(fresh_candidates.len()),
                    "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
                    "checkedAt": Utc::now(),
                }))?
            );
            return Err("blocked_no_positive_edge_precondition".into());
        }
    };
    log_event(
        "positive_edge_precondition_passed",
        edge_precondition_json(&precondition),
    );

    let phases = phase_commands(&options, &precondition);
    if !options.execute {
        log_event("dry_run_complete", json!({ "phaseCount": phases.len() }));
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "monitor_e2e_dry_run",
                "execute": false,
                "settings": options.settings,
                "vaultIndex": options.vault_index,
                "amountRaw": options.amount_raw.to_string(),
                "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
                "precondition": edge_precondition_json(&precondition),
                "phaseCommands": phases,
                "note": "dry-run prints the exact child commands; add --execute only after live approval",
            }))?
        );
        return Ok(());
    }

    let neon_url = env::var("NEON_DATABASE_URL")
        .or_else(|_| env::var("DATABASE_URL"))
        .map_err(|_| "NEON_DATABASE_URL is required")?;
    log_event("neon_connect_start", json!({}));
    let pool = PgPoolOptions::new()
        .max_connections(1)
        .connect(&neon_url)
        .await?;
    log_event("db_readback_start", json!({ "label": "before_setup" }));
    let before_setup = db_readback(&pool, &options).await?;
    log_event(
        "db_readback_complete",
        json!({
            "label": "before_setup",
            "summary": db_readback_summary(&before_setup),
        }),
    );
    if let Err(error) = ensure_db_no_existing_value(&before_setup) {
        log_event(
            "blocked_existing_position_precondition",
            json!({ "reason": error.to_string() }),
        );
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "blocked_existing_position_precondition",
                "execute": true,
                "settings": options.settings,
                "vaultIndex": options.vault_index,
                "amountRaw": options.amount_raw.to_string(),
                "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
                "precondition": edge_precondition_json(&precondition),
                "dbReadback": before_setup,
                "reason": error.to_string(),
            }))?
        );
        return Err(error);
    }

    let policy_update = run_named_child("policy_update", &phases[0])?;
    ensure_child_success("policy_update", &policy_update)?;
    log_event(
        "db_readback_start",
        json!({ "label": "after_policy_update" }),
    );
    let after_policy_update = db_readback(&pool, &options).await?;
    log_event(
        "db_readback_complete",
        json!({
            "label": "after_policy_update",
            "summary": db_readback_summary(&after_policy_update),
        }),
    );
    ensure_db_active_policy(&after_policy_update)?;

    let setup_obligation = run_named_child("setup_obligation", &phases[1])?;
    ensure_child_success("setup_obligation", &setup_obligation)?;
    log_event(
        "db_readback_start",
        json!({ "label": "after_setup_obligation" }),
    );
    let after_setup_obligation = db_readback(&pool, &options).await?;
    log_event(
        "db_readback_complete",
        json!({
            "label": "after_setup_obligation",
            "summary": db_readback_summary(&after_setup_obligation),
        }),
    );
    ensure_db_active_policy(&after_setup_obligation)?;

    let initial_deposit = run_named_child("initial_deposit", &phases[2])?;
    ensure_child_success("initial_deposit", &initial_deposit)?;
    log_event("db_readback_start", json!({ "label": "after_deposit" }));
    let after_deposit = db_readback(&pool, &options).await?;
    log_event(
        "db_readback_complete",
        json!({
            "label": "after_deposit",
            "summary": db_readback_summary(&after_deposit),
        }),
    );
    ensure_db_position_has_value(&after_deposit, &KAMINO_MAIN_USDC_RESERVE.to_string())?;

    let monitor_result = wait_for_monitor_execution(&options).await?;
    let final_reserve = monitor_result
        .get("plannedMove")
        .and_then(|plan| plan.get("targetReserve"))
        .and_then(Value::as_str)
        .or_else(|| {
            monitor_result
                .get("routeExecution")
                .and_then(|execution| execution.get("stdout"))
                .and_then(|stdout| stdout.get("preparedDecision"))
                .and_then(|decision| decision.get("targetReserve"))
                .and_then(Value::as_str)
        })
        .ok_or("monitor executed without a target reserve in its JSON output")?
        .to_owned();
    log_event(
        "monitor_execution_selected_final_reserve",
        json!({ "finalReserve": final_reserve }),
    );
    log_event(
        "db_readback_start",
        json!({ "label": "after_optimization" }),
    );
    let after_optimization = db_readback(&pool, &options).await?;
    log_event(
        "db_readback_complete",
        json!({
            "label": "after_optimization",
            "summary": db_readback_summary(&after_optimization),
        }),
    );
    ensure_db_position_has_value(&after_optimization, &final_reserve)?;

    let full_withdraw_command = full_withdraw_command(&options, &final_reserve);
    let full_withdraw = run_named_child("full_withdraw", &full_withdraw_command)?;
    ensure_child_success("full_withdraw", &full_withdraw)?;
    log_event(
        "db_readback_start",
        json!({ "label": "after_full_withdraw" }),
    );
    let after_full_withdraw = db_readback(&pool, &options).await?;
    log_event(
        "db_readback_complete",
        json!({
            "label": "after_full_withdraw",
            "summary": db_readback_summary(&after_full_withdraw),
        }),
    );
    ensure_db_inactive_and_zero(&after_full_withdraw)?;

    let post_cleanup_monitor_command = post_cleanup_monitor_command(&options);
    let post_cleanup_monitor =
        run_named_child("post_cleanup_monitor", &post_cleanup_monitor_command)?;
    ensure_child_success("post_cleanup_monitor", &post_cleanup_monitor)?;
    let signer_boundary = signer_boundary_from_env()?;
    let execution_evidence = e2e_execution_evidence(
        &options,
        &monitor_result,
        &final_reserve,
        &full_withdraw,
        &post_cleanup_monitor,
        &signer_boundary,
    )?;
    log_event("execution_evidence_passed", execution_evidence.clone());

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "monitor_e2e_executed",
            "execute": true,
            "settings": options.settings,
            "vaultIndex": options.vault_index,
            "amountRaw": options.amount_raw.to_string(),
            "maxCandidateAgeSeconds": options.max_candidate_age_seconds,
            "precondition": edge_precondition_json(&precondition),
            "policyUpdate": child_output_json(&policy_update),
            "setupObligation": child_output_json(&setup_obligation),
            "initialDeposit": child_output_json(&initial_deposit),
            "monitorExecution": monitor_result,
            "finalReserve": final_reserve,
            "fullWithdrawCommand": full_withdraw_command,
            "fullWithdraw": child_output_json(&full_withdraw),
            "postCleanupMonitorCommand": post_cleanup_monitor_command,
            "postCleanupMonitor": child_output_json(&post_cleanup_monitor),
            "dbReadbacks": {
                "afterPolicyUpdate": after_policy_update,
                "afterSetupObligation": after_setup_obligation,
                "afterDeposit": after_deposit,
                "afterOptimization": after_optimization,
                "afterFullWithdraw": after_full_withdraw,
            },
            "executionEvidence": execution_evidence,
        }))?
    );

    Ok(())
}

fn positive_main_to_best_edge(
    candidates: &[SupportedReserveLatestRow],
) -> Result<EdgePrecondition, Value> {
    let Some(main) = candidates
        .iter()
        .find(|candidate| candidate.reserve == KAMINO_MAIN_USDC_RESERVE.to_string())
        .cloned()
    else {
        return Err(json!({
            "reason": "main_usdc_candidate_missing",
            "candidateCount": candidates.len(),
            "candidates": candidates_json(candidates),
        }));
    };
    let Some(best) = candidates
        .iter()
        .max_by(|left, right| compare_candidate_preference(left, right))
        .cloned()
    else {
        return Err(json!({
            "reason": "no_safe_usdc_candidates",
            "candidateCount": 0,
        }));
    };
    let edge_bps = apy_to_bps(best.supply_apy) - apy_to_bps(main.supply_apy);
    if best.reserve == main.reserve || edge_bps <= 0 {
        return Err(json!({
            "reason": "main_usdc_is_not_beaten",
            "mainReserve": main.reserve,
            "mainApyBps": apy_to_bps(main.supply_apy),
            "bestReserve": best.reserve,
            "bestApyBps": apy_to_bps(best.supply_apy),
            "edgeBps": edge_bps,
            "candidates": candidates_json(candidates),
        }));
    }
    Ok(EdgePrecondition {
        main,
        best,
        edge_bps,
    })
}

fn fresh_candidates(
    candidates: &[SupportedReserveLatestRow],
    max_candidate_age_seconds: i64,
) -> Vec<SupportedReserveLatestRow> {
    let freshest_cutoff = Utc::now() - Duration::seconds(max_candidate_age_seconds);
    candidates
        .iter()
        .filter(|candidate| candidate.observed_at >= freshest_cutoff)
        .cloned()
        .collect()
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

fn phase_commands(options: &Options, precondition: &EdgePrecondition) -> Vec<Vec<String>> {
    vec![
        vec![
            "same-mint-reserve-swap".to_owned(),
            "--settings".to_owned(),
            options.settings.clone(),
            "--vault-index".to_owned(),
            options.vault_index.to_string(),
            "--update-policy".to_owned(),
            "--provision-lookup-table".to_owned(),
            "--execute".to_owned(),
        ],
        setup_obligation_command(options, &precondition.best.reserve),
        vec![
            "same-mint-reserve-swap".to_owned(),
            "--settings".to_owned(),
            options.settings.clone(),
            "--vault-index".to_owned(),
            options.vault_index.to_string(),
            "--deposit-main-usdc".to_owned(),
            options.amount_raw.to_string(),
            "--execute".to_owned(),
        ],
        vec![
            "same-mint-yield-monitor".to_owned(),
            "--once".to_owned(),
            "--all-active-vaults".to_owned(),
            "--execute".to_owned(),
            "--poll-interval-seconds".to_owned(),
            options.poll_interval_seconds.to_string(),
            "--max-candidate-age-seconds".to_owned(),
            options.max_candidate_age_seconds.to_string(),
            "--min-edge-bps".to_owned(),
            "1".to_owned(),
        ],
        full_withdraw_command(options, &precondition.best.reserve),
    ]
}

fn setup_obligation_command(options: &Options, reserve: &str) -> Vec<String> {
    vec![
        "same-mint-reserve-swap".to_owned(),
        "--settings".to_owned(),
        options.settings.clone(),
        "--vault-index".to_owned(),
        options.vault_index.to_string(),
        "--setup-obligation-reserve".to_owned(),
        reserve.to_owned(),
        "--execute".to_owned(),
    ]
}

fn full_withdraw_command(options: &Options, reserve: &str) -> Vec<String> {
    vec![
        "same-mint-reserve-swap".to_owned(),
        "--settings".to_owned(),
        options.settings.clone(),
        "--vault-index".to_owned(),
        options.vault_index.to_string(),
        "--full-withdraw-reserve".to_owned(),
        reserve.to_owned(),
        "--execute".to_owned(),
    ]
}

fn post_cleanup_monitor_command(options: &Options) -> Vec<String> {
    vec![
        "same-mint-yield-monitor".to_owned(),
        "--once".to_owned(),
        "--all-active-vaults".to_owned(),
        "--poll-interval-seconds".to_owned(),
        options.poll_interval_seconds.to_string(),
        "--max-candidate-age-seconds".to_owned(),
        options.max_candidate_age_seconds.to_string(),
    ]
}

async fn wait_for_monitor_execution(options: &Options) -> Result<Value, Box<dyn Error>> {
    let deadline = Instant::now() + StdDuration::from_secs(options.timeout_seconds);
    let mut attempt: u64 = 0;
    loop {
        attempt += 1;
        let command = vec![
            "same-mint-yield-monitor".to_owned(),
            "--once".to_owned(),
            "--all-active-vaults".to_owned(),
            "--execute".to_owned(),
            "--poll-interval-seconds".to_owned(),
            options.poll_interval_seconds.to_string(),
            "--max-candidate-age-seconds".to_owned(),
            options.max_candidate_age_seconds.to_string(),
        ];
        log_event(
            "monitor_attempt_start",
            json!({
                "attempt": attempt,
                "remainingSeconds": deadline
                    .saturating_duration_since(Instant::now())
                    .as_secs(),
            }),
        );
        let output = run_named_child("monitor_attempt", &command)?;
        ensure_child_success("same_mint_yield_monitor", &output)?;
        let stdout = output
            .stdout_json
            .as_ref()
            .ok_or("monitor did not print JSON")?;
        log_event(
            "monitor_attempt_complete",
            json!({
                "attempt": attempt,
                "summary": monitor_stdout_summary(stdout, options),
            }),
        );
        if let Some(result) = stdout
            .get("results")
            .and_then(Value::as_array)
            .and_then(|results| {
                results.iter().find(|result| {
                    result
                        .get("vault")
                        .and_then(|vault| vault.get("settings"))
                        .and_then(Value::as_str)
                        == Some(options.settings.as_str())
                        && result
                            .get("vault")
                            .and_then(|vault| vault.get("vaultIndex"))
                            .and_then(Value::as_i64)
                            == Some(i64::from(options.vault_index))
                })
            })
        {
            if result.get("status").and_then(Value::as_str) == Some("executed") {
                log_event(
                    "monitor_attempt_selected_vault_executed",
                    json!({ "attempt": attempt }),
                );
                return Ok(result.clone());
            }
            log_event(
                "monitor_attempt_selected_vault_not_executed",
                json!({
                    "attempt": attempt,
                    "status": result.get("status").and_then(Value::as_str),
                    "reason": result.get("reason").and_then(Value::as_str),
                }),
            );
        } else {
            log_event(
                "monitor_attempt_selected_vault_missing",
                json!({ "attempt": attempt }),
            );
        }

        if Instant::now() >= deadline {
            log_event(
                "monitor_timeout",
                json!({
                    "attempts": attempt,
                    "timeoutSeconds": options.timeout_seconds,
                }),
            );
            return Err("timed out waiting for fleet monitor to execute selected vault".into());
        }
        log_event(
            "monitor_sleep",
            json!({
                "attempt": attempt,
                "sleepSeconds": options.poll_interval_seconds,
            }),
        );
        sleep(TokioDuration::from_secs(options.poll_interval_seconds)).await;
    }
}

fn e2e_execution_evidence(
    options: &Options,
    monitor_result: &Value,
    final_reserve: &str,
    full_withdraw: &ChildOutput,
    post_cleanup_monitor: &ChildOutput,
    signer_boundary: &SignerBoundary,
) -> Result<Value, Box<dyn Error>> {
    if monitor_result.get("status").and_then(Value::as_str) != Some("executed") {
        return Err("monitor result did not execute the selected vault".into());
    }
    let route_stdout = monitor_result
        .pointer("/routeExecution/stdout")
        .ok_or("monitor result is missing route execution stdout")?;
    assert_json_str_eq(
        route_stdout.pointer("/routeExecution/signer"),
        &signer_boundary.optimizer,
        "routeExecution.signer",
    )?;
    assert_json_str_eq(
        route_stdout.pointer("/routeExecution/feePayer"),
        &signer_boundary.optimizer,
        "routeExecution.feePayer",
    )?;
    assert_json_str_eq(
        route_stdout.pointer("/executionPickup/transaction/feePayer"),
        &signer_boundary.optimizer,
        "executionPickup.transaction.feePayer",
    )?;
    assert_json_array_contains(
        route_stdout.pointer("/executionPickup/transaction/signerPubkeys"),
        &signer_boundary.optimizer,
        "executionPickup.transaction.signerPubkeys",
    )?;
    let signature = non_empty_json_str(route_stdout.pointer("/executionPickup/signature"))
        .ok_or("route execution output is missing confirmed signature")?;
    let final_status = route_stdout
        .pointer("/executionPickup/finalStatus")
        .and_then(Value::as_str)
        .ok_or("route execution output is missing final status")?;
    if final_status != "confirmed" {
        return Err(
            format!("route execution final status was {final_status}, expected confirmed").into(),
        );
    }
    let confirmed_status = route_stdout
        .pointer("/confirmedDecision/status")
        .and_then(Value::as_str)
        .ok_or("route execution output is missing confirmed decision status")?;
    if confirmed_status != "confirmed" {
        return Err(format!(
            "confirmed decision status was {confirmed_status}, expected confirmed"
        )
        .into());
    }
    let final_position = selected_position(
        monitor_result
            .get("currentPositionsAfter")
            .ok_or("monitor result is missing currentPositionsAfter")?,
        final_reserve,
    )
    .ok_or("monitor result did not report the final target reserve position")?;
    let final_amount_raw = json_i64(final_position.get("amountRaw"))
        .ok_or("final target reserve position is missing amountRaw")?;
    let final_has_value = final_position
        .get("hasValue")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    if final_amount_raw <= 0 || !final_has_value {
        return Err("final target reserve position does not have value after optimization".into());
    }

    let full_stdout = full_withdraw
        .stdout_json
        .as_ref()
        .ok_or("full withdraw did not print JSON")?;
    if full_stdout.get("status").and_then(Value::as_str) != Some("full_withdraw_reserve_executed") {
        return Err("full withdraw did not execute successfully".into());
    }
    if full_stdout
        .pointer("/withdraw/reserve")
        .and_then(Value::as_str)
        != Some(final_reserve)
    {
        return Err("full withdraw did not target the optimizer final reserve".into());
    }
    assert_json_str_eq(
        full_stdout.pointer("/policyWithdraw/signer"),
        &signer_boundary.optimizer,
        "policyWithdraw.signer",
    )?;
    assert_json_str_eq(
        full_stdout.pointer("/policyWithdrawTransaction/transaction/feePayer"),
        &signer_boundary.optimizer,
        "policyWithdrawTransaction.transaction.feePayer",
    )?;
    assert_json_array_contains(
        full_stdout.pointer("/policyWithdrawTransaction/transaction/signerPubkeys"),
        &signer_boundary.optimizer,
        "policyWithdrawTransaction.transaction.signerPubkeys",
    )?;
    assert_json_str_eq(
        full_stdout.pointer("/walletRecovery/wallet"),
        &signer_boundary.setup_authority,
        "walletRecovery.wallet",
    )?;
    assert_json_str_eq(
        full_stdout.pointer("/walletRecovery/cleanupSigner"),
        &signer_boundary.setup_authority,
        "walletRecovery.cleanupSigner",
    )?;
    assert_json_str_eq(
        full_stdout.pointer("/walletRecoveryTransaction/transaction/feePayer"),
        &signer_boundary.setup_authority,
        "walletRecoveryTransaction.transaction.feePayer",
    )?;
    assert_json_array_contains(
        full_stdout.pointer("/walletRecoveryTransaction/transaction/signerPubkeys"),
        &signer_boundary.setup_authority,
        "walletRecoveryTransaction.transaction.signerPubkeys",
    )?;
    assert_json_str_eq(
        full_stdout.pointer("/policyClose/authority"),
        &signer_boundary.setup_authority,
        "policyClose.authority",
    )?;
    assert_json_str_eq(
        full_stdout.pointer("/policyCloseTransaction/transaction/feePayer"),
        &signer_boundary.setup_authority,
        "policyCloseTransaction.transaction.feePayer",
    )?;
    assert_json_array_contains(
        full_stdout.pointer("/policyCloseTransaction/transaction/signerPubkeys"),
        &signer_boundary.setup_authority,
        "policyCloseTransaction.transaction.signerPubkeys",
    )?;
    let all_tracked_positions_zero = full_stdout
        .pointer("/positionCleanupProof/allTrackedPositionsZero")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    if !all_tracked_positions_zero {
        return Err("full withdraw did not reconcile all tracked positions to zero".into());
    }
    let policy_active = full_stdout
        .pointer("/positionCleanupProof/inactiveRows/policyActive")
        .and_then(Value::as_bool)
        .ok_or("full withdraw output is missing policy inactive row evidence")?;
    let vault_active = full_stdout
        .pointer("/positionCleanupProof/inactiveRows/vaultActive")
        .and_then(Value::as_bool)
        .ok_or("full withdraw output is missing vault inactive row evidence")?;
    if policy_active || vault_active {
        return Err("full withdraw did not mark policy and vault inactive".into());
    }
    let wallet_usdc_delta = json_i64(full_stdout.pointer("/walletRecovery/walletUsdcDeltaRaw"))
        .ok_or("full withdraw output is missing wallet USDC delta evidence")?;
    if wallet_usdc_delta <= 0 {
        return Err("full withdraw did not return USDC to the wallet".into());
    }
    let policy_closed = full_stdout
        .pointer("/policyClose/policyClosed")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    if !policy_closed {
        return Err("full withdraw did not close the policy account".into());
    }
    let policy_close_signature =
        non_empty_json_str(full_stdout.pointer("/policyCloseTransaction/signature"))
            .ok_or("full withdraw output is missing policy close signature")?;

    let remaining_monitored =
        post_cleanup_monitor_contains_selected_vault(post_cleanup_monitor, options)?;
    if remaining_monitored {
        return Err(
            "selected vault is still discoverable by the fleet monitor after cleanup".into(),
        );
    }

    Ok(json!({
        "confirmedDecision": {
            "signature": signature,
            "finalStatus": final_status,
            "confirmedStatus": confirmed_status,
        },
        "signerBoundary": {
            "setupAuthority": signer_boundary.setup_authority.as_str(),
            "optimizer": signer_boundary.optimizer.as_str(),
            "routeExecutionSigner": signer_boundary.optimizer.as_str(),
            "fullWithdrawPolicySigner": signer_boundary.optimizer.as_str(),
            "walletRecoverySigner": signer_boundary.setup_authority.as_str(),
            "policyCloseAuthority": signer_boundary.setup_authority.as_str(),
        },
        "finalReservePosition": final_position,
        "fullWithdraw": {
            "reserve": final_reserve,
            "allTrackedPositionsZero": all_tracked_positions_zero,
            "policyActive": policy_active,
            "vaultActive": vault_active,
            "walletUsdcDeltaRaw": wallet_usdc_delta.to_string(),
            "policyClosed": policy_closed,
            "policyCloseSignature": policy_close_signature,
        },
        "postCleanupFleetDiscovery": {
            "selectedVaultStillMonitored": false,
        },
    }))
}

fn signer_boundary_from_env() -> Result<SignerBoundary, Box<dyn Error>> {
    Ok(SignerBoundary {
        setup_authority: solana_testing_keypair_from_env()?.pubkey().to_string(),
        optimizer: yield_router_keypair_from_env()?.pubkey().to_string(),
    })
}

fn assert_json_str_eq(
    value: Option<&Value>,
    expected: &str,
    label: &str,
) -> Result<(), Box<dyn Error>> {
    let actual = value
        .and_then(Value::as_str)
        .ok_or_else(|| format!("{label} is missing or not a string"))?;
    if actual != expected {
        return Err(format!("{label} was {actual}, expected {expected}").into());
    }
    Ok(())
}

fn assert_json_array_contains(
    value: Option<&Value>,
    expected: &str,
    label: &str,
) -> Result<(), Box<dyn Error>> {
    let values = value
        .and_then(Value::as_array)
        .ok_or_else(|| format!("{label} is missing or not an array"))?;
    if !values.iter().any(|value| value.as_str() == Some(expected)) {
        return Err(format!("{label} does not contain expected signer {expected}").into());
    }
    Ok(())
}

async fn db_readback(pool: &PgPool, options: &Options) -> Result<Value, Box<dyn Error>> {
    let Some(vault_row) = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            v.id AS vault_id,
            v.active AS vault_active,
            v.active_policy_id,
            p.id AS policy_id,
            p.active AS policy_active,
            p.policy_account,
            p.delegated_signers,
            p.route_modes
        FROM loyal_yield.managed_vaults v
        JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
        WHERE v.settings = $1
          AND v.vault_index = $2
        ORDER BY v.id DESC
        LIMIT 1
        "#,
    )
    .bind(&options.settings)
    .bind(options.vault_index)
    .fetch_optional(pool)
    .await?
    else {
        return Ok(json!({
            "found": false,
            "settings": options.settings,
            "vaultIndex": options.vault_index,
        }));
    };

    let vault_id = vault_row.try_get::<i64, _>("vault_id")?;
    let position_rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT reserve, market, liquidity_mint, amount_raw, has_value, snapshot_id, observed_slot
        FROM loyal_yield.vault_reserve_positions_current
        WHERE vault_id = $1
        ORDER BY reserve
        "#,
    )
    .bind(vault_id)
    .fetch_all(pool)
    .await?;
    let decision_rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, status::text AS status, source_reserve, target_reserve, signature, confirmed_slot, post_snapshot_id
        FROM loyal_yield.rebalance_decisions
        WHERE vault_id = $1
        ORDER BY id DESC
        LIMIT 5
        "#,
    )
    .bind(vault_id)
    .fetch_all(pool)
    .await?;

    Ok(json!({
        "found": true,
        "vault": {
            "id": vault_id,
            "active": vault_row.try_get::<bool, _>("vault_active")?,
            "activePolicyId": vault_row.try_get::<i64, _>("active_policy_id")?,
        },
        "policy": {
            "id": vault_row.try_get::<i64, _>("policy_id")?,
            "active": vault_row.try_get::<bool, _>("policy_active")?,
            "policyAccount": vault_row.try_get::<String, _>("policy_account")?,
            "delegatedSigners": vault_row.try_get::<Vec<String>, _>("delegated_signers")?,
            "routeModes": vault_row.try_get::<Vec<String>, _>("route_modes")?,
        },
        "currentPositions": position_rows
            .iter()
            .map(|row| {
                Ok(json!({
                    "reserve": row.try_get::<String, _>("reserve")?,
                    "market": row.try_get::<Option<String>, _>("market")?,
                    "liquidityMint": row.try_get::<String, _>("liquidity_mint")?,
                    "amountRaw": row.try_get::<i64, _>("amount_raw")?.to_string(),
                    "hasValue": row.try_get::<bool, _>("has_value")?,
                    "snapshotId": row.try_get::<i64, _>("snapshot_id")?,
                    "observedSlot": row.try_get::<i64, _>("observed_slot")?,
                }))
            })
            .collect::<Result<Vec<_>, loyal_yield_orchestrator::sqlx::Error>>()?,
        "recentDecisions": decision_rows
            .iter()
            .map(|row| {
                Ok(json!({
                    "id": row.try_get::<i64, _>("id")?,
                    "status": row.try_get::<String, _>("status")?,
                    "sourceReserve": row.try_get::<Option<String>, _>("source_reserve")?,
                    "targetReserve": row.try_get::<Option<String>, _>("target_reserve")?,
                    "signature": row.try_get::<Option<String>, _>("signature")?,
                    "confirmedSlot": row.try_get::<Option<i64>, _>("confirmed_slot")?,
                    "postSnapshotId": row.try_get::<Option<i64>, _>("post_snapshot_id")?,
                }))
            })
            .collect::<Result<Vec<_>, loyal_yield_orchestrator::sqlx::Error>>()?,
    }))
}

fn ensure_db_active_policy(readback: &Value) -> Result<(), Box<dyn Error>> {
    if readback.get("found").and_then(Value::as_bool) != Some(true) {
        return Err("DB readback did not find the managed vault".into());
    }
    if readback.pointer("/vault/active").and_then(Value::as_bool) != Some(true)
        || readback.pointer("/policy/active").and_then(Value::as_bool) != Some(true)
    {
        return Err("DB readback expected active managed_vault and route_policy".into());
    }
    Ok(())
}

fn ensure_db_no_existing_value(readback: &Value) -> Result<(), Box<dyn Error>> {
    if readback.get("found").and_then(Value::as_bool) != Some(true) {
        return Err("DB readback did not find the managed vault before setup".into());
    }
    let positions = readback
        .get("currentPositions")
        .and_then(Value::as_array)
        .ok_or("DB readback is missing currentPositions")?;
    if let Some(position) = positions
        .iter()
        .find(|position| json_i64(position.get("amountRaw")).unwrap_or(0) != 0)
    {
        let reserve = position
            .get("reserve")
            .and_then(Value::as_str)
            .unwrap_or("<unknown>");
        let amount = json_i64(position.get("amountRaw")).unwrap_or(0);
        return Err(format!(
            "selected vault has pre-existing non-zero position {amount} in reserve {reserve}; run full withdrawal cleanup before live E2E"
        )
        .into());
    }
    Ok(())
}

fn ensure_db_position_has_value(readback: &Value, reserve: &str) -> Result<(), Box<dyn Error>> {
    ensure_db_active_policy(readback)?;
    let position = selected_position(
        readback
            .get("currentPositions")
            .ok_or("DB readback is missing currentPositions")?,
        reserve,
    )
    .ok_or_else(|| format!("DB readback is missing reserve {reserve}"))?;
    let amount_raw =
        json_i64(position.get("amountRaw")).ok_or("DB position is missing amountRaw")?;
    if amount_raw <= 0 || position.get("hasValue").and_then(Value::as_bool) != Some(true) {
        return Err(format!("DB position for reserve {reserve} has no value").into());
    }
    Ok(())
}

fn ensure_db_inactive_and_zero(readback: &Value) -> Result<(), Box<dyn Error>> {
    if readback.get("found").and_then(Value::as_bool) != Some(true) {
        return Err("DB readback did not find the managed vault after full withdraw".into());
    }
    if readback.pointer("/vault/active").and_then(Value::as_bool) != Some(false)
        || readback.pointer("/policy/active").and_then(Value::as_bool) != Some(false)
    {
        return Err("DB readback expected inactive managed_vault and route_policy".into());
    }
    let positions = readback
        .get("currentPositions")
        .and_then(Value::as_array)
        .ok_or("DB readback is missing currentPositions")?;
    if positions
        .iter()
        .any(|position| json_i64(position.get("amountRaw")).unwrap_or(0) != 0)
    {
        return Err("DB readback still has a non-zero current position".into());
    }
    Ok(())
}

fn selected_position(positions: &Value, reserve: &str) -> Option<Value> {
    positions.as_array()?.iter().find_map(|position| {
        (position.get("reserve").and_then(Value::as_str) == Some(reserve)).then(|| position.clone())
    })
}

fn post_cleanup_monitor_contains_selected_vault(
    output: &ChildOutput,
    options: &Options,
) -> Result<bool, Box<dyn Error>> {
    let stdout = output
        .stdout_json
        .as_ref()
        .ok_or("post-cleanup fleet monitor did not print JSON")?;
    let results = stdout
        .get("results")
        .and_then(Value::as_array)
        .ok_or("post-cleanup fleet monitor output is missing results")?;
    Ok(results
        .iter()
        .any(|result| result_matches_selected_vault(result, options)))
}

fn result_matches_selected_vault(result: &Value, options: &Options) -> bool {
    result
        .get("vault")
        .and_then(|vault| vault.get("settings"))
        .and_then(Value::as_str)
        == Some(options.settings.as_str())
        && result
            .get("vault")
            .and_then(|vault| vault.get("vaultIndex"))
            .and_then(Value::as_i64)
            == Some(i64::from(options.vault_index))
}

fn non_empty_json_str(value: Option<&Value>) -> Option<&str> {
    value
        .and_then(Value::as_str)
        .filter(|value| !value.trim().is_empty())
}

fn json_i64(value: Option<&Value>) -> Option<i64> {
    value.and_then(|value| {
        value
            .as_i64()
            .or_else(|| value.as_str().and_then(|raw| raw.parse::<i64>().ok()))
    })
}

fn log_event(event: &str, payload: Value) {
    let entry = json!({
        "ts": Utc::now(),
        "event": event,
        "payload": payload,
    });
    match serde_json::to_string(&entry) {
        Ok(line) => eprintln!("[same-mint-monitor-e2e] {line}"),
        Err(error) => eprintln!(
            "[same-mint-monitor-e2e] {{\"event\":\"log_serialization_failed\",\"label\":\"{event}\",\"error\":\"{error}\"}}"
        ),
    }
}

fn run_named_child(name: &str, args: &[String]) -> Result<ChildOutput, Box<dyn Error>> {
    log_event(
        "child_start",
        json!({
            "name": name,
            "command": args,
        }),
    );
    let started_at = Instant::now();
    let output = run_child(args);
    match &output {
        Ok(child) => log_event(
            "child_complete",
            json!({
                "name": name,
                "elapsedMs": started_at.elapsed().as_millis(),
                "summary": child_output_summary(child),
            }),
        ),
        Err(error) => log_event(
            "child_spawn_failed",
            json!({
                "name": name,
                "elapsedMs": started_at.elapsed().as_millis(),
                "error": error.to_string(),
            }),
        ),
    }
    output
}

fn run_child(args: &[String]) -> Result<ChildOutput, Box<dyn Error>> {
    let mut command = command_for_binary(
        args.first()
            .ok_or("child command must include a binary name")?
            .as_str(),
    )?;
    command.args(&args[1..]);
    child_output(command.output()?)
}

fn command_for_binary(name: &str) -> Result<Command, Box<dyn Error>> {
    let binary = sibling_binary(name)?;
    let current_exe = env::current_exe()?;
    let is_local_debug = current_exe.to_string_lossy().contains("/target/debug/");
    if binary.exists() && !is_local_debug {
        return Ok(Command::new(binary));
    }

    let mut command = Command::new("cargo");
    command.args(["run", "-p", "loyal-yield-orchestrator", "--bin", name, "--"]);
    Ok(command)
}

fn sibling_binary(name: &str) -> Result<PathBuf, Box<dyn Error>> {
    let current_exe = env::current_exe()?;
    let dir = current_exe
        .parent()
        .ok_or("current executable has no parent directory")?;
    Ok(dir.join(name))
}

fn child_output(output: Output) -> Result<ChildOutput, Box<dyn Error>> {
    let stdout_text = String::from_utf8_lossy(&output.stdout).trim().to_owned();
    let stdout_json = if stdout_text.is_empty() {
        None
    } else {
        serde_json::from_str::<Value>(&stdout_text).ok()
    };
    Ok(ChildOutput {
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

fn ensure_child_success(name: &str, output: &ChildOutput) -> Result<(), Box<dyn Error>> {
    if output.success {
        Ok(())
    } else {
        log_event(
            "child_failed",
            json!({
                "name": name,
                "summary": child_output_summary(output),
            }),
        );
        Err(format!(
            "{name} failed with status {:?}: {}",
            output.status_code, output.stderr_text
        )
        .into())
    }
}

fn child_output_summary(output: &ChildOutput) -> Value {
    json!({
        "success": output.success,
        "statusCode": output.status_code,
        "stdoutStatus": output
            .stdout_json
            .as_ref()
            .and_then(|stdout| stdout.get("status"))
            .and_then(Value::as_str),
        "stdoutBytes": output.stdout_text.as_ref().map(String::len).unwrap_or(0),
        "stderrBytes": output.stderr_text.len(),
        "stderrTail": if output.success || output.stderr_text.is_empty() {
            None
        } else {
            Some(text_tail(&output.stderr_text, 1200))
        },
    })
}

fn db_readback_summary(readback: &Value) -> Value {
    let positions = readback.get("currentPositions").and_then(Value::as_array);
    let nonzero_positions: Vec<Value> = positions
        .map(|positions| {
            positions
                .iter()
                .filter_map(|position| {
                    let amount_raw = json_i64(position.get("amountRaw")).unwrap_or(0);
                    (amount_raw != 0).then(|| {
                        json!({
                            "reserve": position.get("reserve").and_then(Value::as_str),
                            "amountRaw": amount_raw.to_string(),
                            "hasValue": position.get("hasValue").and_then(Value::as_bool),
                        })
                    })
                })
                .collect()
        })
        .unwrap_or_default();
    json!({
        "found": readback.get("found").and_then(Value::as_bool),
        "vaultActive": readback.pointer("/vault/active").and_then(Value::as_bool),
        "policyActive": readback.pointer("/policy/active").and_then(Value::as_bool),
        "positionCount": positions.map(Vec::len).unwrap_or(0),
        "nonzeroPositions": nonzero_positions,
        "recentDecisionStatuses": readback
            .get("recentDecisions")
            .and_then(Value::as_array)
            .map(|decisions| {
                decisions
                    .iter()
                    .map(|decision| {
                        json!({
                            "id": decision.get("id").and_then(Value::as_i64),
                            "status": decision.get("status").and_then(Value::as_str),
                            "targetReserve": decision.get("targetReserve").and_then(Value::as_str),
                            "signaturePresent": decision
                                .get("signature")
                                .and_then(Value::as_str)
                                .map(|signature| !signature.is_empty())
                                .unwrap_or(false),
                        })
                    })
                    .collect::<Vec<_>>()
            })
            .unwrap_or_default(),
    })
}

fn monitor_stdout_summary(stdout: &Value, options: &Options) -> Value {
    let results = stdout.get("results").and_then(Value::as_array);
    let selected = results.and_then(|results| {
        results
            .iter()
            .find(|result| result_matches_selected_vault(result, options))
    });
    json!({
        "status": stdout.get("status").and_then(Value::as_str),
        "resultCount": results.map(Vec::len).unwrap_or(0),
        "selectedVault": selected.map(|result| {
            json!({
                "status": result.get("status").and_then(Value::as_str),
                "reason": result.get("reason").and_then(Value::as_str),
                "sourceReserve": result
                    .get("plannedMove")
                    .and_then(|plan| plan.get("sourceReserve"))
                    .and_then(Value::as_str),
                "targetReserve": result
                    .get("plannedMove")
                    .and_then(|plan| plan.get("targetReserve"))
                    .and_then(Value::as_str),
                "routeExecutionStatus": result
                    .pointer("/routeExecution/stdout/status")
                    .and_then(Value::as_str),
            })
        }),
    })
}

fn text_tail(value: &str, max_chars: usize) -> String {
    let mut tail = value.chars().rev().take(max_chars).collect::<Vec<_>>();
    tail.reverse();
    tail.into_iter().collect()
}

fn edge_precondition_json(edge: &EdgePrecondition) -> Value {
    json!({
        "mainReserve": edge.main.reserve,
        "mainApyBps": apy_to_bps(edge.main.supply_apy),
        "bestReserve": edge.best.reserve,
        "bestMarket": edge.best.market,
        "bestApyBps": apy_to_bps(edge.best.supply_apy),
        "edgeBps": edge.edge_bps,
    })
}

fn candidates_json(candidates: &[SupportedReserveLatestRow]) -> Vec<Value> {
    candidates
        .iter()
        .map(|candidate| {
            json!({
                "observedAt": candidate.observed_at,
                "reserve": candidate.reserve,
                "market": candidate.market,
                "liquidityMint": candidate.liquidity_mint,
                "supplyApyBps": apy_to_bps(candidate.supply_apy),
                "totalSupplyUsdEstimate": candidate.total_supply_usd_estimate,
            })
        })
        .collect()
}

fn child_output_json(output: &ChildOutput) -> Value {
    json!({
        "success": output.success,
        "statusCode": output.status_code,
        "stdout": output.stdout_json.as_ref().unwrap_or(&Value::Null),
        "stdoutText": if output.stdout_json.is_some() {
            None
        } else {
            output.stdout_text.as_deref()
        },
        "stderrText": if output.stderr_text.is_empty() {
            None
        } else {
            Some(output.stderr_text.as_str())
        },
    })
}

fn apy_to_bps(apy: f64) -> i64 {
    (apy * 10_000.0).round() as i64
}

fn parse_args(args: impl IntoIterator<Item = String>) -> Result<Options, Box<dyn Error>> {
    let mut settings = None;
    let mut vault_index = DEFAULT_VAULT_INDEX;
    let mut amount_raw = None;
    let mut poll_interval_seconds = DEFAULT_POLL_INTERVAL_SECONDS;
    let mut timeout_seconds = DEFAULT_TIMEOUT_SECONDS;
    let mut max_candidate_age_seconds = DEFAULT_MAX_CANDIDATE_AGE_SECONDS;
    let mut execute = false;
    let mut iter = args.into_iter();
    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--settings" => {
                settings = Some(iter.next().ok_or("--settings requires a pubkey")?);
            }
            "--vault-index" => {
                vault_index = iter
                    .next()
                    .ok_or("--vault-index requires a value")?
                    .parse()
                    .map_err(|_| "--vault-index must be an integer")?;
            }
            "--amount-raw" => {
                amount_raw = Some(
                    iter.next()
                        .ok_or("--amount-raw requires a value")?
                        .parse()
                        .map_err(|_| "--amount-raw must be a u64")?,
                );
            }
            "--poll-interval-seconds" => {
                poll_interval_seconds = iter
                    .next()
                    .ok_or("--poll-interval-seconds requires a value")?
                    .parse()
                    .map_err(|_| "--poll-interval-seconds must be a u64")?;
            }
            "--timeout-seconds" => {
                timeout_seconds = iter
                    .next()
                    .ok_or("--timeout-seconds requires a value")?
                    .parse()
                    .map_err(|_| "--timeout-seconds must be a u64")?;
            }
            "--max-candidate-age-seconds" => {
                max_candidate_age_seconds = iter
                    .next()
                    .ok_or("--max-candidate-age-seconds requires a value")?
                    .parse()
                    .map_err(|_| "--max-candidate-age-seconds must be an integer")?;
            }
            "--execute" => execute = true,
            "--help" | "-h" => return Err(usage().into()),
            other => return Err(format!("unknown argument: {other}\n{}", usage()).into()),
        }
    }
    let amount_raw = amount_raw.ok_or("--amount-raw is required")?;
    if amount_raw == 0 {
        return Err("--amount-raw must be greater than 0".into());
    }
    if poll_interval_seconds == 0 || timeout_seconds == 0 {
        return Err("--poll-interval-seconds and --timeout-seconds must be greater than 0".into());
    }
    if max_candidate_age_seconds <= 0 {
        return Err("--max-candidate-age-seconds must be greater than 0".into());
    }
    Ok(Options {
        settings: settings.ok_or("--settings is required")?,
        vault_index,
        amount_raw,
        poll_interval_seconds,
        timeout_seconds,
        max_candidate_age_seconds,
        execute,
    })
}

fn usage() -> &'static str {
    "Usage: same-mint-monitor-e2e --settings <PUBKEY> [--vault-index <N>] --amount-raw <U64> [--poll-interval-seconds <SECONDS>] [--timeout-seconds <SECONDS>] [--max-candidate-age-seconds <SECONDS>] [--execute]"
}

#[cfg(test)]
mod tests {
    use super::*;

    fn candidate(reserve: &str, apy: f64) -> SupportedReserveLatestRow {
        SupportedReserveLatestRow {
            observed_at: Utc::now(),
            slot: 1,
            reserve: reserve.to_owned(),
            market: Some("market".to_owned()),
            market_name: Some("Market".to_owned()),
            liquidity_mint: USDC_MINT.to_string(),
            symbol: Some("USDC".to_owned()),
            supply_apy: apy,
            borrow_apy: 0.0,
            total_supply_usd_estimate: 1_000_000.0,
            reserve_last_update_stale: false,
        }
    }

    #[test]
    fn precondition_requires_best_reserve_to_beat_main() {
        let other = "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu";
        let edge = positive_main_to_best_edge(&[
            candidate(&KAMINO_MAIN_USDC_RESERVE.to_string(), 0.04),
            candidate("Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo", 0.05),
            candidate(other, 0.06),
        ])
        .expect("positive edge");
        assert_eq!(edge.best.reserve, other);
        assert_eq!(edge.edge_bps, 200);

        let blocked = positive_main_to_best_edge(&[
            candidate(&KAMINO_MAIN_USDC_RESERVE.to_string(), 0.06),
            candidate(other, 0.04),
        ])
        .expect_err("main is already best");
        assert_eq!(blocked["reason"], "main_usdc_is_not_beaten");
    }

    #[test]
    fn parse_defaults_to_dry_run_vault_one() {
        let options = parse_args([
            "--settings".to_owned(),
            "settings".to_owned(),
            "--amount-raw".to_owned(),
            "100".to_owned(),
        ])
        .expect("parse options");
        assert_eq!(options.vault_index, 1);
        assert_eq!(options.amount_raw, 100);
        assert_eq!(
            options.max_candidate_age_seconds,
            DEFAULT_MAX_CANDIDATE_AGE_SECONDS
        );
        assert!(!options.execute);
    }

    #[test]
    fn fresh_candidate_filter_matches_monitor_max_age() {
        let fresh = candidate(&KAMINO_MAIN_USDC_RESERVE.to_string(), 0.04);
        let mut stale = candidate("9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", 0.08);
        stale.observed_at = Utc::now() - Duration::seconds(DEFAULT_MAX_CANDIDATE_AGE_SECONDS + 1);

        let fresh_candidates =
            fresh_candidates(&[fresh.clone(), stale], DEFAULT_MAX_CANDIDATE_AGE_SECONDS);

        assert_eq!(fresh_candidates.len(), 1);
        assert_eq!(fresh_candidates[0].reserve, fresh.reserve);
        let blocked = positive_main_to_best_edge(&fresh_candidates)
            .expect_err("stale high-APY candidate must not satisfy the live precondition");
        assert_eq!(blocked["reason"], "main_usdc_is_not_beaten");
    }

    #[test]
    fn e2e_policy_phase_uses_create_capable_update_path() {
        let options = Options {
            settings: "settings".to_owned(),
            vault_index: 1,
            amount_raw: 100,
            poll_interval_seconds: 15,
            timeout_seconds: 300,
            max_candidate_age_seconds: DEFAULT_MAX_CANDIDATE_AGE_SECONDS,
            execute: true,
        };
        let edge = EdgePrecondition {
            main: candidate(&KAMINO_MAIN_USDC_RESERVE.to_string(), 0.03),
            best: candidate("9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", 0.05),
            edge_bps: 200,
        };

        let phases = phase_commands(&options, &edge);
        assert!(phases[0].contains(&"--update-policy".to_owned()));
        assert!(phases[0].contains(&"--provision-lookup-table".to_owned()));
        assert!(!phases[0].contains(&"--update-active-policy".to_owned()));
        assert!(phases[1].contains(&"--setup-obligation-reserve".to_owned()));
        assert!(phases[1].contains(&edge.best.reserve));
        assert!(phases[3].contains(&"--max-candidate-age-seconds".to_owned()));
        assert!(phases[3].contains(&DEFAULT_MAX_CANDIDATE_AGE_SECONDS.to_string()));
    }

    #[test]
    fn live_e2e_preflight_blocks_existing_positions() {
        let clean = json!({
            "found": true,
            "currentPositions": [
                {
                    "reserve": "main",
                    "amountRaw": "0",
                    "hasValue": false
                }
            ]
        });
        ensure_db_no_existing_value(&clean).expect("clean vault can proceed");

        let dirty = json!({
            "found": true,
            "currentPositions": [
                {
                    "reserve": "figure",
                    "amountRaw": "819434",
                    "hasValue": true
                }
            ]
        });
        let error = ensure_db_no_existing_value(&dirty).expect_err("dirty vault blocks live E2E");

        assert!(error.to_string().contains("pre-existing non-zero position"));
        assert!(error.to_string().contains("figure"));
    }

    #[test]
    fn execute_evidence_requires_confirmed_route_cleanup_and_inactive_fleet() {
        let options = Options {
            settings: "settings".to_owned(),
            vault_index: 1,
            amount_raw: 100,
            poll_interval_seconds: 15,
            timeout_seconds: 300,
            max_candidate_age_seconds: DEFAULT_MAX_CANDIDATE_AGE_SECONDS,
            execute: true,
        };
        let final_reserve = "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu";
        let signer_boundary = SignerBoundary {
            setup_authority: "setup-authority".to_owned(),
            optimizer: "optimizer-signer".to_owned(),
        };
        let monitor_result = json!({
            "status": "executed",
            "routeExecution": {
                "stdout": {
                    "routeExecution": {
                        "signer": "optimizer-signer",
                        "feePayer": "optimizer-signer"
                    },
                    "executionPickup": {
                        "signature": "5sig",
                        "finalStatus": "confirmed",
                        "transaction": {
                            "feePayer": "optimizer-signer",
                            "signerPubkeys": ["optimizer-signer"]
                        }
                    },
                    "confirmedDecision": {
                        "status": "confirmed"
                    }
                }
            },
            "currentPositionsAfter": [
                {
                    "reserve": final_reserve,
                    "amountRaw": 100,
                    "hasValue": true
                }
            ]
        });
        let full_withdraw = ChildOutput {
            success: true,
            status_code: Some(0),
            stdout_json: Some(json!({
                "status": "full_withdraw_reserve_executed",
                "withdraw": {
                    "reserve": final_reserve
                },
                "policyWithdraw": {
                    "signer": "optimizer-signer"
                },
                "policyWithdrawTransaction": {
                    "transaction": {
                        "feePayer": "optimizer-signer",
                        "signerPubkeys": ["optimizer-signer"]
                    }
                },
                "positionCleanupProof": {
                    "allTrackedPositionsZero": true,
                    "inactiveRows": {
                        "policyActive": false,
                        "vaultActive": false
                    }
                },
                "walletRecovery": {
                    "wallet": "setup-authority",
                    "cleanupSigner": "setup-authority",
                    "walletUsdcDeltaRaw": "100"
                },
                "walletRecoveryTransaction": {
                    "transaction": {
                        "feePayer": "setup-authority",
                        "signerPubkeys": ["setup-authority"]
                    }
                },
                "policyClose": {
                    "authority": "setup-authority",
                    "policyClosed": true
                },
                "policyCloseTransaction": {
                    "signature": "5close",
                    "transaction": {
                        "feePayer": "setup-authority",
                        "signerPubkeys": ["setup-authority"]
                    }
                }
            })),
            stdout_text: None,
            stderr_text: String::new(),
        };
        let post_cleanup_monitor = ChildOutput {
            success: true,
            status_code: Some(0),
            stdout_json: Some(json!({
                "status": "fleet_poll",
                "results": [
                    {
                        "vault": {
                            "settings": "other",
                            "vaultIndex": 1
                        }
                    }
                ]
            })),
            stdout_text: None,
            stderr_text: String::new(),
        };

        let evidence = e2e_execution_evidence(
            &options,
            &monitor_result,
            final_reserve,
            &full_withdraw,
            &post_cleanup_monitor,
            &signer_boundary,
        )
        .expect("evidence passes");
        assert_eq!(evidence["confirmedDecision"]["signature"], json!("5sig"));
        assert_eq!(
            evidence["postCleanupFleetDiscovery"]["selectedVaultStillMonitored"],
            json!(false)
        );

        let still_monitored = ChildOutput {
            stdout_json: Some(json!({
                "status": "fleet_poll",
                "results": [
                    {
                        "vault": {
                            "settings": "settings",
                            "vaultIndex": 1
                        }
                    }
                ]
            })),
            ..post_cleanup_monitor
        };
        let error = e2e_execution_evidence(
            &options,
            &monitor_result,
            final_reserve,
            &full_withdraw,
            &still_monitored,
            &signer_boundary,
        )
        .expect_err("selected vault must disappear after cleanup");
        assert!(error.to_string().contains("still discoverable"));
    }
}
