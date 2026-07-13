use std::{process::Command, str::FromStr, time::Duration};

use anyhow::{Context, Result};
use balance_sweep_autodeposit_trigger::{
    compute_sweep_amount, initial_surplus_amount, positive_delta_surplus_amount,
    scheduled_eligible_after, surplus_lot_classification_db_value, SweepAmountDecision, SweepCaps,
};
use chrono::{DateTime, Utc};
use clap::Parser;
use loyal_yield_realtime_core::neon_url_looks_pooled;
use sqlx::{
    postgres::{PgConnectOptions, PgListener, PgPoolOptions},
    PgPool, Row,
};
use tokio::time;

const CONSUMER_NAME: &str = "balance_sweep_autodeposit_trigger";
const USDC_MINT_ADDRESS: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const STALE_REQUESTED_SLOT_SECONDS: i64 = 15 * 60;
const REQUESTED_SLOT_TIMEOUT_ERROR: &str = "Autodeposit request timed out before worker selection.";
const MAX_DEBOUNCED_WAKEUPS: u64 = 1000;
const DEFAULT_AUTODEPOSIT_WAKE_CHANNEL: &str = "loyal_yield_autodeposit_wakeup";

#[derive(Debug, Parser)]
#[command(about = "Project autodeposit surplus lots from Loyal wallet balance events")]
struct Args {
    #[arg(long, env = "NEON_DATABASE_URL")]
    postgres_url: String,
    #[arg(long, default_value_t = 1000)]
    batch_limit: i64,
    #[arg(long, default_value_t = 10)]
    poll_interval_seconds: u64,
    #[arg(
        long,
        env = "BALANCE_SWEEP_REALTIME_CHANNEL",
        default_value = DEFAULT_AUTODEPOSIT_WAKE_CHANNEL
    )]
    realtime_channel: String,
    #[arg(long, env = "BALANCE_SWEEP_DISABLE_REALTIME_LISTEN")]
    disable_realtime_listen: bool,
    #[arg(
        long,
        env = "BALANCE_SWEEP_REALTIME_DEBOUNCE_MILLISECONDS",
        default_value_t = 250
    )]
    realtime_debounce_milliseconds: u64,
    #[arg(long)]
    once: bool,
    #[arg(long)]
    claim_target_id: Option<i64>,
    #[arg(long)]
    scheduled_slot_id: Option<i64>,
    #[arg(long)]
    claim_token: Option<String>,
    #[arg(long)]
    claim_wallet_balance_raw: Option<i64>,
    #[arg(long)]
    claim_wallet_balance_floor_raw: Option<i64>,
    #[arg(long)]
    claim_remaining_allowance_raw: Option<i64>,
    #[arg(long)]
    claim_max_amount_raw: Option<i64>,
    #[arg(long)]
    complete_claim_token: Option<String>,
    #[arg(long)]
    complete_execution_id: Option<i64>,
    #[arg(long)]
    release_claim_token: Option<String>,
    #[arg(long, env = "BALANCE_SWEEP_EXECUTE_ELIGIBLE")]
    execute_eligible: bool,
    #[arg(long, env = "BALANCE_SWEEP_EXECUTOR_COMMAND")]
    executor_command: Option<String>,
    #[arg(long, default_value_t = 25)]
    execute_limit: i64,
    #[arg(
        long,
        env = "BALANCE_SWEEP_STALE_SELECTED_CLAIM_SECONDS",
        default_value_t = 900
    )]
    stale_selected_claim_seconds: i64,
}

#[derive(Debug)]
struct WalletBalanceEventRow {
    event_id: i64,
    target_id: i64,
    amount_raw: i64,
    delta_amount_raw: Option<i64>,
    observed_at: DateTime<Utc>,
    txn_signature: Option<String>,
    target_active: bool,
    wallet_balance_floor_raw: Option<i64>,
}

#[derive(Debug, Default)]
struct TriggerOutcome {
    previous_event_id: i64,
    last_event_id: i64,
    events_scanned: usize,
    lots_created: usize,
    outflow_amount_raw: i64,
    lot_amount_depleted_raw: i64,
}

#[derive(Debug, Default)]
struct ExecutorOutcome {
    targets_scanned: usize,
    executions_attempted: usize,
    executions_succeeded: usize,
    executions_failed: usize,
    missing_route_policy_slots_failed: i64,
    stale_requested_slots_failed: i64,
    stale_claims_released: i64,
}

#[derive(Debug)]
struct ExecutableTargetRow {
    target_id: i64,
    scheduled_slot_id: i64,
}

#[derive(Debug, serde::Serialize)]
#[serde(rename_all = "snake_case")]
struct ClaimOutcome {
    status: String,
    reason: Option<String>,
    claim_token: Option<String>,
    target_id: i64,
    amount_raw: i64,
    stale_check_event_id: i64,
    lots: Vec<ClaimedLot>,
}

#[derive(Debug, serde::Serialize)]
#[serde(rename_all = "snake_case")]
struct ClaimedLot {
    lot_id: i64,
    amount_raw: i64,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();
    let args = Args::parse();
    let pool = connect(&args.postgres_url).await?;

    if let Some(claim_token) = args.complete_claim_token.as_deref() {
        let execution_id = args
            .complete_execution_id
            .context("--complete-execution-id is required with --complete-claim-token")?;
        let outcome = complete_claim_once(&pool, claim_token, execution_id).await?;
        println!("{}", serde_json::to_string_pretty(&outcome)?);
        return Ok(());
    }

    if let Some(claim_token) = args.release_claim_token.as_deref() {
        let outcome = release_claim_once(&pool, claim_token).await?;
        println!("{}", serde_json::to_string_pretty(&outcome)?);
        return Ok(());
    }

    if let Some(target_id) = args.claim_target_id {
        let claim_token = args
            .claim_token
            .as_deref()
            .context("--claim-token is required with --claim-target-id")?;
        let wallet_balance_raw = args
            .claim_wallet_balance_raw
            .context("--claim-wallet-balance-raw is required with --claim-target-id")?;
        let wallet_balance_floor_raw = args
            .claim_wallet_balance_floor_raw
            .context("--claim-wallet-balance-floor-raw is required with --claim-target-id")?;
        let outcome = claim_eligible_lots_once(
            &pool,
            target_id,
            claim_token,
            args.scheduled_slot_id,
            wallet_balance_raw,
            wallet_balance_floor_raw,
            args.claim_max_amount_raw,
            args.claim_remaining_allowance_raw,
        )
        .await?;
        println!("{}", serde_json::to_string_pretty(&outcome)?);
        return Ok(());
    }

    let mut realtime_listener = None;
    loop {
        let outcome = project_surplus_lots_once(&pool, args.batch_limit).await?;
        tracing::info!(
            events_scanned = outcome.events_scanned,
            previous_event_id = outcome.previous_event_id,
            last_event_id = outcome.last_event_id,
            lots_created = outcome.lots_created,
            outflow_amount_raw = outcome.outflow_amount_raw,
            lot_amount_depleted_raw = outcome.lot_amount_depleted_raw,
            "projected autodeposit surplus lots"
        );
        if args.execute_eligible {
            let executor_command = args.executor_command.as_deref().context(
                "--executor-command or BALANCE_SWEEP_EXECUTOR_COMMAND is required with --execute-eligible",
            )?;
            let execution_outcome = execute_eligible_targets_once(
                &pool,
                executor_command,
                args.execute_limit,
                args.stale_selected_claim_seconds,
            )
            .await?;
            tracing::info!(
                targets_scanned = execution_outcome.targets_scanned,
                executions_attempted = execution_outcome.executions_attempted,
                executions_succeeded = execution_outcome.executions_succeeded,
                executions_failed = execution_outcome.executions_failed,
                missing_route_policy_slots_failed =
                    execution_outcome.missing_route_policy_slots_failed,
                stale_requested_slots_failed = execution_outcome.stale_requested_slots_failed,
                stale_claims_released = execution_outcome.stale_claims_released,
                "scanned eligible autodeposit lots for execution"
            );
        }
        if args.once {
            return Ok(());
        }
        wait_for_next_autodeposit_scan(
            &args.postgres_url,
            &args.realtime_channel,
            args.disable_realtime_listen,
            &mut realtime_listener,
            Duration::from_secs(args.poll_interval_seconds),
            Duration::from_millis(args.realtime_debounce_milliseconds),
        )
        .await;
    }
}

async fn wait_for_next_autodeposit_scan(
    postgres_url: &str,
    channel: &str,
    disable_realtime_listen: bool,
    listener: &mut Option<PgListener>,
    poll_interval: Duration,
    debounce_interval: Duration,
) {
    if disable_realtime_listen {
        time::sleep(poll_interval).await;
        return;
    }

    if listener.is_none() {
        if neon_url_looks_pooled(postgres_url) {
            tracing::warn!(
                "NEON_DATABASE_URL appears to use a pooled -pooler host; LISTEN/NOTIFY requires a direct connection, falling back to timed polling if connect fails"
            );
        }
        match connect_realtime_listener(postgres_url, channel).await {
            Ok(connected) => {
                *listener = Some(connected);
                tracing::info!(channel, "autodeposit realtime listener connected");
            }
            Err(error) => {
                tracing::warn!(
                    error = %error,
                    channel,
                    "autodeposit realtime listener connect failed; using poll fallback"
                );
                time::sleep(poll_interval).await;
                return;
            }
        }
    }

    let deadline = time::Instant::now() + poll_interval;
    loop {
        let Some(active_listener) = listener.as_mut() else {
            return;
        };
        match time::timeout_at(deadline, active_listener.recv()).await {
            Ok(Ok(notification)) => {
                match autodeposit_wakeup_from_notification(notification.payload()) {
                    Some(scheduled_slot_id) => {
                        let mut wakeup_count = 1_u64;
                        time::sleep(debounce_interval).await;
                        while wakeup_count < MAX_DEBOUNCED_WAKEUPS {
                            match time::timeout(Duration::from_millis(10), active_listener.recv())
                                .await
                            {
                                Ok(Ok(notification)) => {
                                    match autodeposit_wakeup_from_notification(
                                        notification.payload(),
                                    ) {
                                        Some(_) => wakeup_count += 1,
                                        None => {}
                                    }
                                }
                                Ok(Err(error)) => {
                                    tracing::warn!(
                                        error = %error,
                                        "autodeposit realtime listener failed during debounce"
                                    );
                                    break;
                                }
                                Err(_) => break,
                            }
                        }
                        tracing::info!(
                            scheduled_slot_id,
                            wakeup_count,
                            "autodeposit requested-slot wakeup received"
                        );
                        return;
                    }
                    None => {
                        if time::Instant::now() >= deadline {
                            return;
                        }
                    }
                }
            }
            Ok(Err(error)) => {
                tracing::warn!(
                    error = %error,
                    "autodeposit realtime listener failed; reconnecting after poll fallback"
                );
                *listener = None;
                time::sleep(poll_interval).await;
                return;
            }
            Err(_) => return,
        }
    }
}

async fn connect_realtime_listener(postgres_url: &str, channel: &str) -> Result<PgListener> {
    let mut listener = PgListener::connect(postgres_url).await?;
    listener.listen(channel).await?;
    Ok(listener)
}

fn autodeposit_wakeup_from_notification(payload: &str) -> Option<i64> {
    #[derive(serde::Deserialize)]
    struct WakeupHint {
        scheduled_slot_id: i64,
    }

    match serde_json::from_str::<WakeupHint>(payload) {
        Ok(hint) if hint.scheduled_slot_id > 0 => Some(hint.scheduled_slot_id),
        _ => {
            tracing::warn!("autodeposit wakeup payload was not a valid scheduled-slot hint");
            None
        }
    }
}

async fn execute_eligible_targets_once(
    pool: &PgPool,
    executor_command: &str,
    limit: i64,
    stale_selected_claim_seconds: i64,
) -> Result<ExecutorOutcome> {
    let missing_route_policy_slots_failed =
        fail_slots_without_active_earn_route_policy_once(pool, limit).await?;
    let stale_requested_slots_failed = fail_stale_requested_slots_once(pool, limit).await?;
    let stale_claims_released =
        release_stale_selected_claims_once(pool, stale_selected_claim_seconds, limit).await?;
    let targets = load_executable_targets(pool, limit).await?;
    let mut outcome = ExecutorOutcome {
        targets_scanned: targets.len(),
        missing_route_policy_slots_failed,
        stale_requested_slots_failed,
        stale_claims_released,
        ..ExecutorOutcome::default()
    };
    for target in targets {
        outcome.executions_attempted += 1;
        let claim_token = format!(
            "autodeposit-trigger:{}:{}:{}",
            target.target_id,
            target.scheduled_slot_id,
            Utc::now()
                .timestamp_nanos_opt()
                .unwrap_or_else(|| Utc::now().timestamp_micros())
        );
        let status = Command::new("sh")
            .arg("-c")
            .arg(build_executor_shell_command(
                executor_command,
                target.target_id,
                target.scheduled_slot_id,
                &claim_token,
            ))
            .status()
            .with_context(|| {
                format!("spawn autodeposit executor for target {}", target.target_id)
            })?;
        if status.success() {
            outcome.executions_succeeded += 1;
        } else {
            outcome.executions_failed += 1;
            tracing::warn!(
                target_id = target.target_id,
                scheduled_slot_id = target.scheduled_slot_id,
                claim_token,
                status = ?status,
                "autodeposit executor exited unsuccessfully"
            );
        }
    }
    Ok(outcome)
}

async fn fail_slots_without_active_earn_route_policy_once(
    pool: &PgPool,
    limit: i64,
) -> Result<i64> {
    if limit <= 0 {
        return Ok(0);
    }

    let rows = sqlx::query(
        r#"
        WITH doomed_slots AS (
            SELECT slot.id, slot.target_id
            FROM loyal_yield.balance_sweep_scheduled_slots AS slot
            JOIN loyal_yield.balance_sweep_targets AS target
              ON target.id = slot.target_id
            WHERE slot.status IN ('scheduled', 'requested')
              AND slot.eligible_after <= now()
              AND target.active = true
              AND target.lifecycle_status = 'active'
              AND target.token_mint = $2
              AND slot.token_mint = target.token_mint
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.managed_vaults AS managed
                  JOIN loyal_yield.route_policies AS policy
                    ON policy.id = managed.active_policy_id
                   AND policy.active = true
                   AND policy.authority = target.authority
                   AND policy.settings = target.settings
                   AND policy.vault_index = target.vault_index
                   AND policy.vault_pubkey = target.vault_pubkey
                   AND 'same_mint_kamino' = ANY(policy.route_modes)
                  WHERE managed.active = true
                    AND managed.settings = target.settings
                    AND managed.vault_index = target.vault_index
                    AND managed.vault_pubkey = target.vault_pubkey
              )
            ORDER BY slot.eligible_after ASC, slot.id ASC
            LIMIT $1
            FOR UPDATE OF slot SKIP LOCKED
        )
        UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
        SET status = 'failed',
            claim_token = NULL,
            last_error = format(
                'Autodeposit target %s does not have an active Earn route policy.',
                doomed.target_id
            ),
            updated_at = now()
        FROM doomed_slots AS doomed
        WHERE slot.id = doomed.id
          AND slot.status IN ('scheduled', 'requested')
        RETURNING slot.id
        "#,
    )
    .bind(limit)
    .bind(USDC_MINT_ADDRESS)
    .fetch_all(pool)
    .await?;

    Ok(rows.len() as i64)
}

async fn fail_stale_requested_slots_once(pool: &PgPool, limit: i64) -> Result<i64> {
    if limit <= 0 {
        return Ok(0);
    }

    let rows = sqlx::query(
        r#"
        WITH stale_slots AS (
            SELECT slot.id
            FROM loyal_yield.balance_sweep_scheduled_slots AS slot
            WHERE slot.status = 'requested'
              AND COALESCE(slot.requested_at, slot.updated_at)
                    < now() - ($1::bigint * interval '1 second')
            ORDER BY COALESCE(slot.requested_at, slot.updated_at) ASC, slot.id ASC
            LIMIT $2
            FOR UPDATE SKIP LOCKED
        )
        UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
        SET status = 'failed',
            claim_token = NULL,
            last_error = $3,
            updated_at = now()
        FROM stale_slots AS stale
        WHERE slot.id = stale.id
          AND slot.status = 'requested'
        RETURNING slot.id
        "#,
    )
    .bind(STALE_REQUESTED_SLOT_SECONDS)
    .bind(limit)
    .bind(REQUESTED_SLOT_TIMEOUT_ERROR)
    .fetch_all(pool)
    .await?;

    Ok(rows.len() as i64)
}

async fn release_stale_selected_claims_once(
    pool: &PgPool,
    stale_selected_claim_seconds: i64,
    limit: i64,
) -> Result<i64> {
    if stale_selected_claim_seconds <= 0 || limit <= 0 {
        return Ok(0);
    }

    let row = sqlx::query(
        r#"
        WITH stale_claims AS (
            SELECT claim.claim_token
            FROM loyal_yield.balance_sweep_lot_claims AS claim
            JOIN loyal_yield.balance_sweep_targets AS target
              ON target.id = claim.target_id
            JOIN loyal_yield.balance_sweep_wallet_balances_current AS balance
              ON balance.target_id = target.id
             AND balance.mint = target.token_mint
            WHERE claim.status = 'selected'
              AND claim.execution_id IS NULL
              AND target.token_mint = $3
              AND target.wallet_balance_floor_raw IS NOT NULL
              AND balance.amount_raw - target.wallet_balance_floor_raw >= claim.amount_raw
              AND COALESCE((
                  SELECT offset_row.last_event_id
                  FROM loyal_yield.projection_offsets AS offset_row
                  WHERE offset_row.consumer_name = $4
              ), 0) >= (
                  SELECT COALESCE(MAX(event.event_id), 0)
                  FROM loyal_yield.balance_sweep_wallet_balance_events AS event
                  WHERE event.target_id = claim.target_id
                    AND event.mint = target.token_mint
              )
              AND claim.updated_at < now() - ($1::bigint * interval '1 second')
            ORDER BY claim.updated_at ASC, claim.claim_token ASC
            LIMIT $2
            FOR UPDATE SKIP LOCKED
        ),
        matched_items AS (
            SELECT item.lot_id, item.amount_raw
            FROM loyal_yield.balance_sweep_lot_claim_items AS item
            JOIN stale_claims
              ON stale_claims.claim_token = item.claim_token
        ),
        restored_lots AS (
            UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
            SET remaining_amount_raw = LEAST(
                    lot.original_amount_raw,
                    lot.remaining_amount_raw + item.amount_raw
                ),
                status = 'open',
                eligible_after = now(),
                updated_at = now()
            FROM matched_items AS item
            WHERE lot.id = item.lot_id
            RETURNING lot.scheduled_slot_id
        ),
        released_claims AS (
            UPDATE loyal_yield.balance_sweep_lot_claims AS claim
            SET status = 'released',
                updated_at = now()
            WHERE claim.claim_token IN (SELECT claim_token FROM stale_claims)
              AND EXISTS (SELECT 1 FROM restored_lots)
            RETURNING claim.claim_token
        ),
        failed_slots AS (
            UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
            SET status = 'failed',
                claim_token = NULL,
                last_error = 'stale selected claim released by autodeposit worker',
                updated_at = now()
            WHERE slot.claim_token IN (SELECT claim_token FROM released_claims)
               OR slot.id IN (
                  SELECT scheduled_slot_id
                  FROM restored_lots
                  WHERE scheduled_slot_id IS NOT NULL
               )
            RETURNING slot.id
        )
        SELECT COALESCE((SELECT COUNT(*) FROM released_claims), 0)::bigint AS released_claim_count
        "#,
    )
    .bind(stale_selected_claim_seconds)
    .bind(limit)
    .bind(USDC_MINT_ADDRESS)
    .bind(CONSUMER_NAME)
    .fetch_one(pool)
    .await?;

    Ok(row.try_get("released_claim_count")?)
}

fn build_executor_shell_command(
    executor_command: &str,
    target_id: i64,
    scheduled_slot_id: i64,
    claim_token: &str,
) -> String {
    format!(
        "{} --execute --target-id {} --scheduled-slot-id {} --claim-token {}",
        executor_command, target_id, scheduled_slot_id, claim_token
    )
}

async fn load_executable_targets(pool: &PgPool, limit: i64) -> Result<Vec<ExecutableTargetRow>> {
    let rows = sqlx::query(
        r#"
        SELECT
            target.id AS target_id,
            slot.id AS scheduled_slot_id
        FROM loyal_yield.balance_sweep_scheduled_slots AS slot
        JOIN loyal_yield.balance_sweep_targets AS target
          ON target.id = slot.target_id
        JOIN loyal_yield.balance_sweep_wallet_balances_current AS balance
          ON balance.target_id = target.id
         AND balance.mint = target.token_mint
        WHERE target.active = true
          AND target.lifecycle_status = 'active'
          AND target.token_mint = $2
          AND target.wallet_balance_floor_raw IS NOT NULL
          AND balance.amount_raw > target.wallet_balance_floor_raw
          AND slot.token_mint = target.token_mint
          AND slot.status IN ('scheduled', 'requested')
          AND slot.eligible_after <= now()
          AND EXISTS (
              SELECT 1
              FROM loyal_yield.balance_sweep_surplus_lots AS lot
              JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
                ON event.event_id = lot.source_event_id
              WHERE lot.target_id = target.id
                AND lot.scheduled_slot_id = slot.id
                AND event.mint = target.token_mint
                AND lot.status = 'open'
                AND lot.remaining_amount_raw > 0
          )
          AND NOT EXISTS (
              SELECT 1
              FROM loyal_yield.balance_sweep_lot_claims AS claim
              WHERE claim.target_id = target.id
                AND claim.status = 'selected'
          )
        ORDER BY
            CASE WHEN slot.status = 'requested' THEN 0 ELSE 1 END,
            slot.requested_at DESC NULLS LAST,
            slot.eligible_after ASC,
            balance.updated_at ASC,
            target.id ASC,
            slot.id ASC
        LIMIT $1
        "#,
    )
    .bind(limit)
    .bind(USDC_MINT_ADDRESS)
    .fetch_all(pool)
    .await?;
    rows.into_iter()
        .map(|row| {
            Ok(ExecutableTargetRow {
                target_id: row.try_get("target_id")?,
                scheduled_slot_id: row.try_get("scheduled_slot_id")?,
            })
        })
        .collect()
}

async fn complete_claim_once(
    pool: &PgPool,
    claim_token: &str,
    execution_id: i64,
) -> Result<ClaimOutcome> {
    let mut tx = pool.begin().await?;
    let Some(mut outcome) = load_existing_claim_by_token(&mut tx, claim_token).await? else {
        tx.commit().await?;
        return Ok(ClaimOutcome {
            status: "noop".to_owned(),
            reason: Some("claim_not_found".to_owned()),
            claim_token: None,
            target_id: 0,
            amount_raw: 0,
            stale_check_event_id: 0,
            lots: Vec::new(),
        });
    };
    if outcome.status != "selected" {
        tx.commit().await?;
        outcome.reason = Some("claim_not_selected".to_owned());
        return Ok(outcome);
    }
    if outcome.lots.is_empty() {
        tx.commit().await?;
        outcome.reason = Some("claim_has_no_supported_lots".to_owned());
        return Ok(outcome);
    }

    let update = sqlx::query(
        r#"
        WITH matched_lots AS (
            SELECT item.lot_id, item.amount_raw
            FROM loyal_yield.balance_sweep_lot_claim_items AS item
            JOIN loyal_yield.balance_sweep_surplus_lots AS lot
              ON lot.id = item.lot_id
            JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
              ON event.event_id = lot.source_event_id
            JOIN loyal_yield.balance_sweep_lot_claims AS claim
              ON claim.claim_token = item.claim_token
            JOIN loyal_yield.balance_sweep_targets AS target
              ON target.id = claim.target_id
            WHERE item.claim_token = $1
              AND lot.target_id = claim.target_id
              AND event.mint = target.token_mint
              AND target.token_mint = $3
        ),
        inserted AS (
            INSERT INTO loyal_yield.balance_sweep_execution_lots
                (execution_id, lot_id, amount_raw)
            SELECT $2, lot_id, amount_raw
            FROM matched_lots
            ON CONFLICT (execution_id, lot_id) DO NOTHING
            RETURNING lot_id
        )
        UPDATE loyal_yield.balance_sweep_lot_claims
        SET status = 'executed',
            execution_id = $2,
            updated_at = now()
        WHERE claim_token = $1
          AND status = 'selected'
          AND EXISTS (SELECT 1 FROM matched_lots)
        "#,
    )
    .bind(claim_token)
    .bind(execution_id)
    .bind(USDC_MINT_ADDRESS)
    .execute(&mut *tx)
    .await?;
    if update.rows_affected() > 0 {
        sqlx::query(
            r#"
            UPDATE loyal_yield.balance_sweep_scheduled_slots
            SET status = 'executed',
                execution_id = $2,
                updated_at = now()
            WHERE claim_token = $1
            "#,
        )
        .bind(claim_token)
        .bind(execution_id)
        .execute(&mut *tx)
        .await?;
    }

    tx.commit().await?;
    if update.rows_affected() == 0 {
        outcome.reason = Some("claim_has_no_supported_lots".to_owned());
        return Ok(outcome);
    }
    outcome.status = "executed".to_owned();
    Ok(outcome)
}

async fn release_claim_once(pool: &PgPool, claim_token: &str) -> Result<ClaimOutcome> {
    let mut tx = pool.begin().await?;
    let Some(mut outcome) = load_existing_claim_by_token(&mut tx, claim_token).await? else {
        tx.commit().await?;
        return Ok(ClaimOutcome {
            status: "noop".to_owned(),
            reason: Some("claim_not_found".to_owned()),
            claim_token: None,
            target_id: 0,
            amount_raw: 0,
            stale_check_event_id: 0,
            lots: Vec::new(),
        });
    };
    if outcome.status != "selected" {
        tx.commit().await?;
        outcome.reason = Some("claim_not_selected".to_owned());
        return Ok(outcome);
    }
    if outcome.lots.is_empty() {
        tx.commit().await?;
        outcome.reason = Some("claim_has_no_supported_lots".to_owned());
        return Ok(outcome);
    }

    let update = sqlx::query(
        r#"
        WITH matched_items AS (
            SELECT item.lot_id, item.amount_raw
            FROM loyal_yield.balance_sweep_lot_claim_items AS item
            JOIN loyal_yield.balance_sweep_surplus_lots AS lot
              ON lot.id = item.lot_id
            JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
              ON event.event_id = lot.source_event_id
            JOIN loyal_yield.balance_sweep_lot_claims AS claim
              ON claim.claim_token = item.claim_token
            JOIN loyal_yield.balance_sweep_targets AS target
              ON target.id = claim.target_id
            WHERE item.claim_token = $1
              AND lot.target_id = claim.target_id
              AND event.mint = target.token_mint
              AND target.token_mint = $2
        ),
        restored AS (
            UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
            SET remaining_amount_raw = LEAST(
                    lot.original_amount_raw,
                    lot.remaining_amount_raw + item.amount_raw
                ),
                status = 'open',
                updated_at = now()
            FROM matched_items AS item
            WHERE lot.id = item.lot_id
            RETURNING lot.id
        )
        UPDATE loyal_yield.balance_sweep_lot_claims
        SET status = 'released',
            updated_at = now()
        WHERE claim_token = $1
          AND status = 'selected'
          AND EXISTS (SELECT 1 FROM restored)
        "#,
    )
    .bind(claim_token)
    .bind(USDC_MINT_ADDRESS)
    .execute(&mut *tx)
    .await?;
    if update.rows_affected() > 0 {
        sqlx::query(
            r#"
            UPDATE loyal_yield.balance_sweep_scheduled_slots
            SET status = 'failed',
                claim_token = NULL,
                last_error = 'claim released before autodeposit pull',
                updated_at = now()
            WHERE claim_token = $1
            "#,
        )
        .bind(claim_token)
        .execute(&mut *tx)
        .await?;
    }

    tx.commit().await?;
    if update.rows_affected() == 0 {
        outcome.reason = Some("claim_has_no_supported_lots".to_owned());
        return Ok(outcome);
    }
    outcome.status = "released".to_owned();
    Ok(outcome)
}

async fn claim_eligible_lots_once(
    pool: &PgPool,
    target_id: i64,
    claim_token: &str,
    scheduled_slot_id: Option<i64>,
    wallet_balance_raw: i64,
    wallet_balance_floor_raw: i64,
    max_amount_per_period_raw: Option<i64>,
    remaining_allowance_raw: Option<i64>,
) -> Result<ClaimOutcome> {
    let mut tx = pool.begin().await?;
    if let Some(existing) = load_existing_claim(&mut tx, claim_token, target_id).await? {
        tx.commit().await?;
        return Ok(existing);
    }

    let target_active = sqlx::query_scalar::<_, bool>(
        r#"
        SELECT active AND lifecycle_status = 'active'
        FROM loyal_yield.balance_sweep_targets
        WHERE id = $1
          AND token_mint = $2
        FOR UPDATE
        "#,
    )
    .bind(target_id)
    .bind(USDC_MINT_ADDRESS)
    .fetch_optional(&mut *tx)
    .await?
    .unwrap_or(false);
    if !target_active {
        tx.commit().await?;
        return Ok(no_claim(target_id, "target_not_active"));
    }

    let stale_check_event_id = current_target_event_id(&mut tx, target_id).await?;
    let processed_event_id = projection_offset(&mut tx, CONSUMER_NAME).await?;
    if processed_event_id < stale_check_event_id {
        tx.commit().await?;
        return Ok(ClaimOutcome {
            status: "noop".to_owned(),
            reason: Some("newer_unprocessed_wallet_event".to_owned()),
            claim_token: None,
            target_id,
            amount_raw: 0,
            stale_check_event_id,
            lots: Vec::new(),
        });
    }

    if let Some(slot_id) = scheduled_slot_id {
        let slot_available = lock_executable_slot(&mut tx, target_id, slot_id).await?;
        if !slot_available {
            tx.commit().await?;
            return Ok(no_claim(target_id, "scheduled_slot_not_available"));
        }
    }

    let open_lots = lock_eligible_lots(&mut tx, target_id, scheduled_slot_id).await?;
    let eligible_lot_amount_raw = open_lots
        .iter()
        .map(|lot| lot.remaining_amount_raw)
        .sum::<i64>();
    let decision = compute_sweep_amount(SweepCaps {
        eligible_lot_amount_raw,
        wallet_balance_raw,
        wallet_balance_floor_raw,
        max_amount_per_period_raw,
        remaining_allowance_raw,
    });
    let SweepAmountDecision::Sweep { amount_raw, .. } = decision else {
        tx.commit().await?;
        return Ok(ClaimOutcome {
            status: "noop".to_owned(),
            reason: Some(
                match decision {
                    SweepAmountDecision::NoEligibleLots => "no_eligible_lots",
                    SweepAmountDecision::NoWalletExcess { .. } => "wallet_balance_not_above_floor",
                    SweepAmountDecision::AllowanceExhausted { .. } => "allowance_exhausted",
                    SweepAmountDecision::Sweep { .. } => unreachable!(),
                }
                .to_owned(),
            ),
            claim_token: None,
            target_id,
            amount_raw: 0,
            stale_check_event_id,
            lots: Vec::new(),
        });
    };

    let mut remaining_to_claim = amount_raw;
    let mut claimed = Vec::new();
    for lot in open_lots {
        if remaining_to_claim == 0 {
            break;
        }
        let claim_amount = remaining_to_claim.min(lot.remaining_amount_raw);
        claimed.push(ClaimedLot {
            lot_id: lot.id,
            amount_raw: claim_amount,
        });
        remaining_to_claim -= claim_amount;
    }
    if remaining_to_claim != 0 {
        tx.commit().await?;
        return Ok(no_claim(target_id, "insufficient_locked_lots"));
    }

    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_lot_claims
            (claim_token, target_id, amount_raw, status, stale_check_event_id)
        VALUES ($1, $2, $3, 'selected', $4)
        "#,
    )
    .bind(claim_token)
    .bind(target_id)
    .bind(amount_raw)
    .bind(stale_check_event_id)
    .execute(&mut *tx)
    .await?;

    for item in &claimed {
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.balance_sweep_lot_claim_items
                (claim_token, lot_id, amount_raw)
            VALUES ($1, $2, $3)
            "#,
        )
        .bind(claim_token)
        .bind(item.lot_id)
        .bind(item.amount_raw)
        .execute(&mut *tx)
        .await?;

        sqlx::query(
            r#"
            UPDATE loyal_yield.balance_sweep_surplus_lots
            SET remaining_amount_raw = remaining_amount_raw - $2,
                status = CASE
                    WHEN remaining_amount_raw - $2 = 0 THEN 'consumed'::loyal_yield.balance_sweep_surplus_lot_status
                    ELSE 'open'::loyal_yield.balance_sweep_surplus_lot_status
                END,
                updated_at = now()
            WHERE id = $1
              AND remaining_amount_raw >= $2
            "#,
        )
        .bind(item.lot_id)
        .bind(item.amount_raw)
        .execute(&mut *tx)
        .await?;
    }

    move_residual_open_lots_to_next_slot(&mut tx, scheduled_slot_id).await?;
    mark_slot_selected(&mut tx, scheduled_slot_id, claim_token).await?;

    tx.commit().await?;
    Ok(ClaimOutcome {
        status: "selected".to_owned(),
        reason: None,
        claim_token: Some(claim_token.to_owned()),
        target_id,
        amount_raw,
        stale_check_event_id,
        lots: claimed,
    })
}

async fn mark_slot_selected(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    scheduled_slot_id: Option<i64>,
    claim_token: &str,
) -> Result<()> {
    sqlx::query(
        r#"
        UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
        SET status = 'selected',
            claim_token = $2,
            last_error = NULL,
            updated_at = now()
        WHERE (
              $1::bigint IS NOT NULL
              AND slot.id = $1::bigint
              AND slot.status IN ('scheduled', 'requested')
        )
           OR (
              $1::bigint IS NULL
              AND slot.id IN (
                  SELECT DISTINCT lot.scheduled_slot_id
                  FROM loyal_yield.balance_sweep_lot_claim_items AS item
                  JOIN loyal_yield.balance_sweep_surplus_lots AS lot
                    ON lot.id = item.lot_id
                  WHERE item.claim_token = $2
                    AND lot.scheduled_slot_id IS NOT NULL
              )
           )
        "#,
    )
    .bind(scheduled_slot_id)
    .bind(claim_token)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

fn no_claim(target_id: i64, reason: &'static str) -> ClaimOutcome {
    ClaimOutcome {
        status: "noop".to_owned(),
        reason: Some(reason.to_owned()),
        claim_token: None,
        target_id,
        amount_raw: 0,
        stale_check_event_id: 0,
        lots: Vec::new(),
    }
}

async fn lock_executable_slot(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    target_id: i64,
    scheduled_slot_id: i64,
) -> Result<bool> {
    let row = sqlx::query(
        r#"
        SELECT id
        FROM loyal_yield.balance_sweep_scheduled_slots
        WHERE id = $1
          AND target_id = $2
          AND token_mint = $3
          AND status IN ('scheduled', 'requested')
          AND eligible_after <= now()
        FOR UPDATE
        "#,
    )
    .bind(scheduled_slot_id)
    .bind(target_id)
    .bind(USDC_MINT_ADDRESS)
    .fetch_optional(&mut **tx)
    .await?;
    Ok(row.is_some())
}

async fn move_residual_open_lots_to_next_slot(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    scheduled_slot_id: Option<i64>,
) -> Result<()> {
    let Some(scheduled_slot_id) = scheduled_slot_id else {
        return Ok(());
    };

    sqlx::query(
        r#"
        WITH residual AS (
            SELECT
                slot.target_id,
                slot.token_mint,
                MAX(lot.eligible_after) AS eligible_after
            FROM loyal_yield.balance_sweep_scheduled_slots AS slot
            JOIN loyal_yield.balance_sweep_surplus_lots AS lot
              ON lot.scheduled_slot_id = slot.id
            WHERE slot.id = $1
              AND lot.status = 'open'
              AND lot.remaining_amount_raw > 0
            GROUP BY slot.target_id, slot.token_mint
        ),
        inserted_slot AS (
            INSERT INTO loyal_yield.balance_sweep_scheduled_slots
                (target_id, token_mint, eligible_after, status)
            SELECT target_id, token_mint, eligible_after, 'scheduled'
            FROM residual
            RETURNING id
        )
        UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
        SET scheduled_slot_id = inserted_slot.id,
            updated_at = now()
        FROM inserted_slot
        WHERE lot.scheduled_slot_id = $1
          AND lot.status = 'open'
          AND lot.remaining_amount_raw > 0
        "#,
    )
    .bind(scheduled_slot_id)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

#[derive(Debug)]
struct OpenLotRow {
    id: i64,
    remaining_amount_raw: i64,
}

async fn lock_eligible_lots(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    target_id: i64,
    scheduled_slot_id: Option<i64>,
) -> Result<Vec<OpenLotRow>> {
    let rows = sqlx::query(
        r#"
        SELECT lot.id, lot.remaining_amount_raw
        FROM loyal_yield.balance_sweep_surplus_lots AS lot
        JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
          ON event.event_id = lot.source_event_id
        JOIN loyal_yield.balance_sweep_targets AS target
          ON target.id = lot.target_id
        WHERE lot.target_id = $1
          AND event.mint = target.token_mint
          AND target.token_mint = $2
          AND lot.status = 'open'
          AND lot.remaining_amount_raw > 0
          AND ($3::bigint IS NOT NULL OR lot.eligible_after <= now())
          AND ($3::bigint IS NULL OR lot.scheduled_slot_id = $3::bigint)
        ORDER BY lot.eligible_after ASC, lot.created_at ASC, lot.id ASC
        FOR UPDATE SKIP LOCKED
        "#,
    )
    .bind(target_id)
    .bind(USDC_MINT_ADDRESS)
    .bind(scheduled_slot_id)
    .fetch_all(&mut **tx)
    .await?;
    rows.into_iter()
        .map(|row| {
            Ok(OpenLotRow {
                id: row.try_get("id")?,
                remaining_amount_raw: row.try_get("remaining_amount_raw")?,
            })
        })
        .collect()
}

async fn current_target_event_id(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    target_id: i64,
) -> Result<i64> {
    Ok(sqlx::query_scalar(
        r#"
        SELECT COALESCE(MAX(event_id), 0)
        FROM loyal_yield.balance_sweep_wallet_balance_events AS event
        JOIN loyal_yield.balance_sweep_targets AS target
          ON target.id = event.target_id
        WHERE event.target_id = $1
          AND event.mint = target.token_mint
          AND target.token_mint = $2
        "#,
    )
    .bind(target_id)
    .bind(USDC_MINT_ADDRESS)
    .fetch_one(&mut **tx)
    .await?)
}

async fn projection_offset(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    consumer_name: &str,
) -> Result<i64> {
    Ok(sqlx::query_scalar(
        r#"
        SELECT COALESCE(last_event_id, 0)
        FROM loyal_yield.projection_offsets
        WHERE consumer_name = $1
        "#,
    )
    .bind(consumer_name)
    .fetch_optional(&mut **tx)
    .await?
    .unwrap_or(0))
}

async fn load_existing_claim(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    claim_token: &str,
    target_id: i64,
) -> Result<Option<ClaimOutcome>> {
    let Some(outcome) = load_existing_claim_by_token(tx, claim_token).await? else {
        return Ok(None);
    };
    if outcome.target_id != target_id {
        return Ok(Some(no_claim(target_id, "claim_token_target_mismatch")));
    }
    Ok(Some(outcome))
}

async fn load_existing_claim_by_token(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    claim_token: &str,
) -> Result<Option<ClaimOutcome>> {
    let Some(row) = sqlx::query(
        r#"
        SELECT claim.claim_token, claim.target_id, claim.amount_raw, claim.status::text AS status, claim.stale_check_event_id
        FROM loyal_yield.balance_sweep_lot_claims AS claim
        JOIN loyal_yield.balance_sweep_targets AS target
          ON target.id = claim.target_id
        WHERE claim.claim_token = $1
          AND target.token_mint = $2
        FOR UPDATE
        "#,
    )
    .bind(claim_token)
    .bind(USDC_MINT_ADDRESS)
    .fetch_optional(&mut **tx)
    .await?
    else {
        return Ok(None);
    };
    let existing_target_id: i64 = row.try_get("target_id")?;
    let item_rows = sqlx::query(
        r#"
        SELECT item.lot_id, item.amount_raw
        FROM loyal_yield.balance_sweep_lot_claim_items AS item
        JOIN loyal_yield.balance_sweep_surplus_lots AS lot
          ON lot.id = item.lot_id
        JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
          ON event.event_id = lot.source_event_id
        JOIN loyal_yield.balance_sweep_lot_claims AS claim
          ON claim.claim_token = item.claim_token
        JOIN loyal_yield.balance_sweep_targets AS target
          ON target.id = claim.target_id
        WHERE item.claim_token = $1
          AND lot.target_id = claim.target_id
          AND event.mint = target.token_mint
          AND target.token_mint = $2
        ORDER BY item.lot_id ASC
        "#,
    )
    .bind(claim_token)
    .bind(USDC_MINT_ADDRESS)
    .fetch_all(&mut **tx)
    .await?;
    let lots = item_rows
        .into_iter()
        .map(|item| {
            Ok(ClaimedLot {
                lot_id: item.try_get("lot_id")?,
                amount_raw: item.try_get("amount_raw")?,
            })
        })
        .collect::<Result<Vec<_>>>()?;
    Ok(Some(ClaimOutcome {
        status: row.try_get("status")?,
        reason: None,
        claim_token: Some(row.try_get("claim_token")?),
        target_id: existing_target_id,
        amount_raw: row.try_get("amount_raw")?,
        stale_check_event_id: row.try_get("stale_check_event_id")?,
        lots,
    }))
}

async fn connect(database_url: &str) -> Result<PgPool> {
    let options = PgConnectOptions::from_str(database_url)?.statement_cache_capacity(0);
    Ok(PgPoolOptions::new()
        .max_connections(5)
        .connect_with(options)
        .await?)
}

async fn project_surplus_lots_once(pool: &PgPool, batch_limit: i64) -> Result<TriggerOutcome> {
    let mut tx = pool.begin().await?;
    let previous_event_id = lock_projection_offset(&mut tx).await?;
    let events = fetch_events_after(&mut tx, previous_event_id, batch_limit).await?;
    let mut outcome = TriggerOutcome {
        previous_event_id,
        last_event_id: previous_event_id,
        events_scanned: events.len(),
        ..TriggerOutcome::default()
    };

    for event in events {
        outcome.last_event_id = event.event_id;
        let Some(delta_amount_raw) = event.delta_amount_raw else {
            if insert_initial_surplus_lot_if_any(&mut tx, &event).await? {
                outcome.lots_created += 1;
            }
            continue;
        };
        if delta_amount_raw > 0 {
            if insert_positive_delta_lot(&mut tx, &event, delta_amount_raw).await? {
                outcome.lots_created += 1;
            }
        } else if delta_amount_raw < 0 {
            let outflow_amount = delta_amount_raw.abs();
            outcome.outflow_amount_raw += outflow_amount;
            outcome.lot_amount_depleted_raw +=
                deplete_lots_newest_first(&mut tx, event.target_id, outflow_amount).await?;
        }
    }

    if outcome.last_event_id > previous_event_id {
        advance_projection_offset(&mut tx, outcome.last_event_id).await?;
    }

    tx.commit().await?;
    Ok(outcome)
}

async fn lock_projection_offset(tx: &mut sqlx::Transaction<'_, sqlx::Postgres>) -> Result<i64> {
    let row = sqlx::query(
        r#"
        INSERT INTO loyal_yield.projection_offsets (consumer_name, last_event_id)
        VALUES ($1, 0)
        ON CONFLICT (consumer_name) DO UPDATE
        SET consumer_name = EXCLUDED.consumer_name
        RETURNING last_event_id
        "#,
    )
    .bind(CONSUMER_NAME)
    .fetch_one(&mut **tx)
    .await?;
    let last_event_id: i64 = row.try_get("last_event_id")?;
    let locked: i64 = sqlx::query_scalar(
        r#"
        SELECT last_event_id
        FROM loyal_yield.projection_offsets
        WHERE consumer_name = $1
        FOR UPDATE
        "#,
    )
    .bind(CONSUMER_NAME)
    .fetch_one(&mut **tx)
    .await?;
    Ok(last_event_id.max(locked))
}

async fn fetch_events_after(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    last_event_id: i64,
    limit: i64,
) -> Result<Vec<WalletBalanceEventRow>> {
    let rows = sqlx::query(
        r#"
        SELECT
            event.event_id,
            event.target_id,
            event.amount_raw,
            event.delta_amount_raw,
            event.observed_at,
            event.txn_signature,
            target.active AND target.lifecycle_status = 'active' AS target_active,
            target.wallet_balance_floor_raw
        FROM loyal_yield.balance_sweep_wallet_balance_events AS event
        JOIN loyal_yield.balance_sweep_targets AS target
          ON target.id = event.target_id
        WHERE event.event_id > $1
          AND event.mint = target.token_mint
          AND target.token_mint = $3
        ORDER BY event.event_id ASC
        LIMIT $2
        "#,
    )
    .bind(last_event_id)
    .bind(limit)
    .bind(USDC_MINT_ADDRESS)
    .fetch_all(&mut **tx)
    .await?;

    rows.into_iter()
        .map(|row| {
            Ok(WalletBalanceEventRow {
                event_id: row.try_get("event_id")?,
                target_id: row.try_get("target_id")?,
                amount_raw: row.try_get("amount_raw")?,
                delta_amount_raw: row.try_get("delta_amount_raw")?,
                observed_at: row.try_get("observed_at")?,
                txn_signature: row.try_get("txn_signature")?,
                target_active: row.try_get("target_active")?,
                wallet_balance_floor_raw: row.try_get("wallet_balance_floor_raw")?,
            })
        })
        .collect()
}

async fn insert_initial_surplus_lot_if_any(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &WalletBalanceEventRow,
) -> Result<bool> {
    if !event.target_active {
        return Ok(false);
    }
    let Some(amount_raw) = initial_surplus_amount(event.amount_raw, event.wallet_balance_floor_raw)
    else {
        return Ok(false);
    };
    insert_scheduled_lot(
        tx,
        event,
        amount_raw,
        "derived",
        "initial wallet ATA balance above the configured floor scheduled for autodeposit after one hour",
    )
    .await
}

async fn insert_positive_delta_lot(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &WalletBalanceEventRow,
    delta_amount_raw: i64,
) -> Result<bool> {
    if !event.target_active {
        return Ok(false);
    }
    let Some(amount_raw) = positive_delta_surplus_amount(
        event.amount_raw,
        delta_amount_raw,
        event.wallet_balance_floor_raw,
    ) else {
        return Ok(false);
    };
    insert_scheduled_lot(
        tx,
        event,
        amount_raw,
        "derived",
        "wallet balance increase scheduled for autodeposit after one hour",
    )
    .await
}

async fn insert_scheduled_lot(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &WalletBalanceEventRow,
    amount_raw: i64,
    confidence: &str,
    reason: &str,
) -> Result<bool> {
    let eligible_after = scheduled_eligible_after(event.observed_at);
    let row = sqlx::query(
        r#"
        WITH current_slot AS (
            SELECT id
            FROM loyal_yield.balance_sweep_scheduled_slots
            WHERE target_id = $1
              AND token_mint = $9
              AND status = 'scheduled'
            ORDER BY eligible_after ASC, id ASC
            LIMIT 1
            FOR UPDATE
        ),
        updated_current_slot AS (
            UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
            SET eligible_after = GREATEST(slot.eligible_after, $6),
                updated_at = now()
            WHERE slot.id IN (SELECT id FROM current_slot)
            RETURNING slot.id
        ),
        inserted_slot AS (
            INSERT INTO loyal_yield.balance_sweep_scheduled_slots
                (target_id, token_mint, eligible_after, status)
            SELECT $1, $9, $6, 'scheduled'
            WHERE NOT EXISTS (SELECT 1 FROM updated_current_slot)
            RETURNING id
        ),
        selected_slot AS (
            SELECT id FROM updated_current_slot
            UNION ALL
            SELECT id FROM inserted_slot
            LIMIT 1
        )
        INSERT INTO loyal_yield.balance_sweep_surplus_lots
            (target_id, source_event_id, source_signature, original_amount_raw,
             remaining_amount_raw, classification, eligible_after, status, confidence, reason,
             scheduled_slot_id)
        SELECT $1, $2, $3, $4, $4, $5::loyal_yield.balance_sweep_surplus_classification,
               $6, 'open', $7, $8, selected_slot.id
        FROM selected_slot
        ON CONFLICT (source_event_id) DO NOTHING
        RETURNING id
        "#,
    )
    .bind(event.target_id)
    .bind(event.event_id)
    .bind(event.txn_signature.as_deref())
    .bind(amount_raw)
    .bind(surplus_lot_classification_db_value())
    .bind(eligible_after)
    .bind(confidence)
    .bind(reason)
    .bind(USDC_MINT_ADDRESS)
    .fetch_optional(&mut **tx)
    .await?;
    Ok(row.is_some())
}

async fn deplete_lots_newest_first(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    target_id: i64,
    outflow_amount_raw: i64,
) -> Result<i64> {
    let mut remaining_outflow = outflow_amount_raw;
    let rows = sqlx::query(
        r#"
        SELECT lot.id, lot.remaining_amount_raw
        FROM loyal_yield.balance_sweep_surplus_lots AS lot
        JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
          ON event.event_id = lot.source_event_id
        JOIN loyal_yield.balance_sweep_targets AS target
          ON target.id = lot.target_id
        WHERE lot.target_id = $1
          AND event.mint = target.token_mint
          AND target.token_mint = $2
          AND lot.status = 'open'
          AND lot.remaining_amount_raw > 0
        ORDER BY lot.created_at DESC, lot.id DESC
        FOR UPDATE
        "#,
    )
    .bind(target_id)
    .bind(USDC_MINT_ADDRESS)
    .fetch_all(&mut **tx)
    .await?;

    let mut depleted_amount = 0_i64;
    for row in rows {
        if remaining_outflow == 0 {
            break;
        }
        let lot_id: i64 = row.try_get("id")?;
        let lot_remaining: i64 = row.try_get("remaining_amount_raw")?;
        let consumed = remaining_outflow.min(lot_remaining);
        remaining_outflow -= consumed;
        depleted_amount += consumed;
        let next_remaining = lot_remaining - consumed;
        let next_status = if next_remaining == 0 {
            "depleted"
        } else {
            "open"
        };
        sqlx::query(
            r#"
            UPDATE loyal_yield.balance_sweep_surplus_lots
            SET remaining_amount_raw = $2,
                status = $3::loyal_yield.balance_sweep_surplus_lot_status,
                updated_at = now()
            WHERE id = $1
            "#,
        )
        .bind(lot_id)
        .bind(next_remaining)
        .bind(next_status)
        .execute(&mut **tx)
        .await
        .with_context(|| format!("deplete surplus lot {lot_id}"))?;
    }

    Ok(depleted_amount)
}

async fn advance_projection_offset(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event_id: i64,
) -> Result<()> {
    sqlx::query(
        r#"
        UPDATE loyal_yield.projection_offsets
        SET last_event_id = $2,
            updated_at = now()
        WHERE consumer_name = $1
        "#,
    )
    .bind(CONSUMER_NAME)
    .bind(event_id)
    .execute(&mut **tx)
    .await?;
    Ok(())
}
