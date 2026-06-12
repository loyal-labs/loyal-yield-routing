use std::{process::Command, str::FromStr, time::Duration};

use anyhow::{Context, Result};
use balance_sweep_autodeposit_trigger::{
    classify_from_evidence, compute_sweep_amount, eligible_after, initial_surplus_amount,
    SurplusClassification, SweepAmountDecision, SweepCaps,
};
use chrono::{DateTime, Utc};
use clap::Parser;
use serde_json::Value;
use sqlx::{
    postgres::{PgConnectOptions, PgPoolOptions},
    PgPool, Row,
};
use tokio::time;

const CONSUMER_NAME: &str = "balance_sweep_autodeposit_trigger";

#[derive(Debug, Parser)]
#[command(about = "Project source-aware surplus lots from Loyal wallet balance events")]
struct Args {
    #[arg(long, env = "NEON_DATABASE_URL")]
    postgres_url: String,
    #[arg(long, default_value_t = 1000)]
    batch_limit: i64,
    #[arg(long, default_value_t = 10)]
    poll_interval_seconds: u64,
    #[arg(long)]
    once: bool,
    #[arg(long)]
    claim_target_id: Option<i64>,
    #[arg(long)]
    claim_token: Option<String>,
    #[arg(long)]
    claim_wallet_balance_raw: Option<i64>,
    #[arg(long)]
    claim_wallet_balance_floor_raw: Option<i64>,
    #[arg(long)]
    claim_remaining_allowance_raw: Option<i64>,
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
}

#[derive(Debug)]
struct WalletBalanceEventRow {
    event_id: i64,
    target_id: i64,
    amount_raw: i64,
    delta_amount_raw: Option<i64>,
    observed_at: DateTime<Utc>,
    txn_signature: Option<String>,
    raw_evidence: Value,
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
}

#[derive(Debug)]
struct ExecutableTargetRow {
    target_id: i64,
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
            wallet_balance_raw,
            wallet_balance_floor_raw,
            args.claim_remaining_allowance_raw,
        )
        .await?;
        println!("{}", serde_json::to_string_pretty(&outcome)?);
        return Ok(());
    }

    loop {
        let outcome = project_surplus_lots_once(&pool, args.batch_limit).await?;
        tracing::info!(
            events_scanned = outcome.events_scanned,
            previous_event_id = outcome.previous_event_id,
            last_event_id = outcome.last_event_id,
            lots_created = outcome.lots_created,
            outflow_amount_raw = outcome.outflow_amount_raw,
            lot_amount_depleted_raw = outcome.lot_amount_depleted_raw,
            "projected source-aware autodeposit surplus lots"
        );
        if args.execute_eligible {
            let executor_command = args.executor_command.as_deref().context(
                "--executor-command or BALANCE_SWEEP_EXECUTOR_COMMAND is required with --execute-eligible",
            )?;
            let execution_outcome =
                execute_eligible_targets_once(&pool, executor_command, args.execute_limit).await?;
            tracing::info!(
                targets_scanned = execution_outcome.targets_scanned,
                executions_attempted = execution_outcome.executions_attempted,
                executions_succeeded = execution_outcome.executions_succeeded,
                executions_failed = execution_outcome.executions_failed,
                "scanned eligible autodeposit lots for execution"
            );
        }
        if args.once {
            return Ok(());
        }
        time::sleep(Duration::from_secs(args.poll_interval_seconds)).await;
    }
}

async fn execute_eligible_targets_once(
    pool: &PgPool,
    executor_command: &str,
    limit: i64,
) -> Result<ExecutorOutcome> {
    let targets = load_executable_targets(pool, limit).await?;
    let mut outcome = ExecutorOutcome {
        targets_scanned: targets.len(),
        ..ExecutorOutcome::default()
    };
    for target in targets {
        outcome.executions_attempted += 1;
        let claim_token = format!(
            "autodeposit-trigger:{}:{}",
            target.target_id,
            Utc::now()
                .timestamp_nanos_opt()
                .unwrap_or_else(|| Utc::now().timestamp_micros())
        );
        let status = Command::new("sh")
            .arg("-c")
            .arg(build_executor_shell_command(
                executor_command,
                target.target_id,
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
                claim_token,
                status = ?status,
                "autodeposit executor exited unsuccessfully"
            );
        }
    }
    Ok(outcome)
}

fn build_executor_shell_command(
    executor_command: &str,
    target_id: i64,
    claim_token: &str,
) -> String {
    format!(
        "{} --execute --target-id {} --claim-token {}",
        executor_command, target_id, claim_token
    )
}

async fn load_executable_targets(pool: &PgPool, limit: i64) -> Result<Vec<ExecutableTargetRow>> {
    let rows = sqlx::query(
        r#"
        SELECT target.id AS target_id
        FROM loyal_yield.balance_sweep_targets AS target
        JOIN loyal_yield.balance_sweep_wallet_balances_current AS balance
          ON balance.target_id = target.id
        WHERE target.active = true
          AND target.lifecycle_status = 'active'
          AND target.wallet_balance_floor_raw IS NOT NULL
          AND balance.amount_raw > target.wallet_balance_floor_raw
          AND EXISTS (
              SELECT 1
              FROM loyal_yield.balance_sweep_surplus_lots AS lot
              WHERE lot.target_id = target.id
                AND lot.status = 'open'
                AND lot.remaining_amount_raw > 0
                AND lot.eligible_after <= now()
          )
          AND NOT EXISTS (
              SELECT 1
              FROM loyal_yield.balance_sweep_lot_claims AS claim
              WHERE claim.target_id = target.id
                AND claim.status = 'selected'
          )
        ORDER BY balance.updated_at ASC, target.id ASC
        LIMIT $1
        "#,
    )
    .bind(limit)
    .fetch_all(pool)
    .await?;
    rows.into_iter()
        .map(|row| {
            Ok(ExecutableTargetRow {
                target_id: row.try_get("target_id")?,
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

    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_execution_lots
            (execution_id, lot_id, amount_raw)
        SELECT $2, lot_id, amount_raw
        FROM loyal_yield.balance_sweep_lot_claim_items
        WHERE claim_token = $1
        ON CONFLICT (execution_id, lot_id) DO NOTHING
        "#,
    )
    .bind(claim_token)
    .bind(execution_id)
    .execute(&mut *tx)
    .await?;

    sqlx::query(
        r#"
        UPDATE loyal_yield.balance_sweep_lot_claims
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

    tx.commit().await?;
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

    for item in &outcome.lots {
        sqlx::query(
            r#"
            UPDATE loyal_yield.balance_sweep_surplus_lots
            SET remaining_amount_raw = remaining_amount_raw + $2,
                status = 'open',
                updated_at = now()
            WHERE id = $1
            "#,
        )
        .bind(item.lot_id)
        .bind(item.amount_raw)
        .execute(&mut *tx)
        .await?;
    }

    sqlx::query(
        r#"
        UPDATE loyal_yield.balance_sweep_lot_claims
        SET status = 'released',
            updated_at = now()
        WHERE claim_token = $1
        "#,
    )
    .bind(claim_token)
    .execute(&mut *tx)
    .await?;

    tx.commit().await?;
    outcome.status = "released".to_owned();
    Ok(outcome)
}

async fn claim_eligible_lots_once(
    pool: &PgPool,
    target_id: i64,
    claim_token: &str,
    wallet_balance_raw: i64,
    wallet_balance_floor_raw: i64,
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
        FOR UPDATE
        "#,
    )
    .bind(target_id)
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

    let open_lots = lock_eligible_lots(&mut tx, target_id).await?;
    let eligible_lot_amount_raw = open_lots
        .iter()
        .map(|lot| lot.remaining_amount_raw)
        .sum::<i64>();
    let decision = compute_sweep_amount(SweepCaps {
        eligible_lot_amount_raw,
        wallet_balance_raw,
        wallet_balance_floor_raw,
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

#[derive(Debug)]
struct OpenLotRow {
    id: i64,
    remaining_amount_raw: i64,
}

async fn lock_eligible_lots(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    target_id: i64,
) -> Result<Vec<OpenLotRow>> {
    let rows = sqlx::query(
        r#"
        SELECT id, remaining_amount_raw
        FROM loyal_yield.balance_sweep_surplus_lots
        WHERE target_id = $1
          AND status = 'open'
          AND remaining_amount_raw > 0
          AND eligible_after <= now()
        ORDER BY eligible_after ASC, created_at ASC, id ASC
        FOR UPDATE SKIP LOCKED
        "#,
    )
    .bind(target_id)
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
        FROM loyal_yield.balance_sweep_wallet_balance_events
        WHERE target_id = $1
        "#,
    )
    .bind(target_id)
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
        SELECT claim_token, target_id, amount_raw, status::text AS status, stale_check_event_id
        FROM loyal_yield.balance_sweep_lot_claims
        WHERE claim_token = $1
        FOR UPDATE
        "#,
    )
    .bind(claim_token)
    .fetch_optional(&mut **tx)
    .await?
    else {
        return Ok(None);
    };
    let existing_target_id: i64 = row.try_get("target_id")?;
    let item_rows = sqlx::query(
        r#"
        SELECT lot_id, amount_raw
        FROM loyal_yield.balance_sweep_lot_claim_items
        WHERE claim_token = $1
        ORDER BY lot_id ASC
        "#,
    )
    .bind(claim_token)
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
            event.raw_evidence,
            target.active AND target.lifecycle_status = 'active' AS target_active,
            target.wallet_balance_floor_raw
        FROM loyal_yield.balance_sweep_wallet_balance_events AS event
        JOIN loyal_yield.balance_sweep_targets AS target
          ON target.id = event.target_id
        WHERE event.event_id > $1
        ORDER BY event.event_id ASC
        LIMIT $2
        "#,
    )
    .bind(last_event_id)
    .bind(limit)
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
                raw_evidence: row.try_get("raw_evidence")?,
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
    insert_classified_lot(
        tx,
        event,
        amount_raw,
        SurplusClassification::InitialSurplus,
        "derived",
        "initial wallet ATA balance above the configured floor",
    )
    .await
}

async fn insert_positive_delta_lot(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &WalletBalanceEventRow,
    amount_raw: i64,
) -> Result<bool> {
    let (classification, confidence, reason) =
        classify_from_evidence(event.txn_signature.as_deref(), &event.raw_evidence);
    insert_classified_lot(
        tx,
        event,
        amount_raw,
        classification,
        confidence,
        reason.as_str(),
    )
    .await
}

async fn insert_classified_lot(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    event: &WalletBalanceEventRow,
    amount_raw: i64,
    classification: SurplusClassification,
    confidence: &str,
    reason: &str,
) -> Result<bool> {
    let eligible_after = eligible_after(classification, event.observed_at);
    let row = sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_surplus_lots
            (target_id, source_event_id, source_signature, original_amount_raw,
             remaining_amount_raw, classification, eligible_after, status, confidence, reason)
        VALUES ($1, $2, $3, $4, $4, $5::loyal_yield.balance_sweep_surplus_classification,
                $6, 'open', $7, $8)
        ON CONFLICT (source_event_id) DO NOTHING
        RETURNING id
        "#,
    )
    .bind(event.target_id)
    .bind(event.event_id)
    .bind(event.txn_signature.as_deref())
    .bind(amount_raw)
    .bind(classification.as_db_str())
    .bind(eligible_after)
    .bind(confidence)
    .bind(reason)
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
        SELECT id, remaining_amount_raw
        FROM loyal_yield.balance_sweep_surplus_lots
        WHERE target_id = $1
          AND status = 'open'
          AND remaining_amount_raw > 0
        ORDER BY created_at DESC, id DESC
        FOR UPDATE
        "#,
    )
    .bind(target_id)
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

#[cfg(test)]
mod tests {
    use super::build_executor_shell_command;

    const SOURCE: &str = include_str!("main.rs");

    #[test]
    fn executor_shell_command_executes_with_target_and_claim_token() {
        let command = build_executor_shell_command(
            "bun scripts/execute-autodeposit-policy.ts",
            42,
            "autodeposit-trigger:42:123",
        );
        assert_eq!(
            command,
            "bun scripts/execute-autodeposit-policy.ts --execute --target-id 42 --claim-token autodeposit-trigger:42:123"
        );
    }

    #[test]
    fn executable_scan_requires_active_lifecycle_and_no_selected_claim() {
        assert!(SOURCE.contains("target.active = true"));
        assert!(SOURCE.contains("target.lifecycle_status = 'active'"));
        assert!(SOURCE.contains("claim.status = 'selected'"));
        assert!(SOURCE.contains("AND NOT EXISTS"));
    }

    #[test]
    fn claim_path_refuses_inactive_targets_and_unprojected_events() {
        assert!(SOURCE.contains("SELECT active AND lifecycle_status = 'active'"));
        assert!(SOURCE.contains("FOR UPDATE"));
        assert!(SOURCE.contains("target_not_active"));
        assert!(SOURCE.contains("processed_event_id < stale_check_event_id"));
        assert!(SOURCE.contains("newer_unprocessed_wallet_event"));
    }

    #[test]
    fn claim_path_is_idempotent_and_locks_lots() {
        assert!(SOURCE.contains("load_existing_claim(&mut tx, claim_token, target_id)"));
        assert!(SOURCE.contains("WHERE claim_token = $1"));
        assert!(SOURCE.contains("FOR UPDATE SKIP LOCKED"));
        assert!(SOURCE.contains("remaining_amount_raw >= $2"));
    }

    #[test]
    fn first_wallet_event_can_create_initial_surplus_from_floor() {
        assert!(SOURCE.contains("let Some(delta_amount_raw) = event.delta_amount_raw else"));
        assert!(SOURCE.contains("insert_initial_surplus_lot_if_any"));
        assert!(SOURCE
            .contains("initial_surplus_amount(event.amount_raw, event.wallet_balance_floor_raw)"));
        assert!(SOURCE.contains("SurplusClassification::InitialSurplus"));
        assert!(SOURCE
            .contains("target.active AND target.lifecycle_status = 'active' AS target_active"));
    }

    #[test]
    fn initial_surplus_creation_is_source_event_id_idempotent() {
        assert!(SOURCE.contains("ON CONFLICT (source_event_id) DO NOTHING"));
    }
}
