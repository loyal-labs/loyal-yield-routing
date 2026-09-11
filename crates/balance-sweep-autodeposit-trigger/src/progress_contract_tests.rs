//! Executable process/SQL contracts, not assertions about Rust field defaults.
use super::*;

#[test]
fn child_exit_protocol_never_confuses_pending_or_legacy_zero_with_completion() {
    let mut outcome = ExecutorOutcome::default();
    for script in [
        "exit \"$AUTODEPOSIT_COMPLETED_EXIT_CODE\"",
        "exit \"$AUTODEPOSIT_DEFERRED_EXIT_CODE\"",
        "exit \"$AUTODEPOSIT_RECOVERY_PENDING_EXIT_CODE\"",
        "exit \"$AUTODEPOSIT_NOOP_EXIT_CODE\"",
        "exit 23",
        // Arbitrary stdout claiming completion is NOT the outcome protocol.
        "printf '{\"status\":\"autodeposit_completed\"}'; exit 0",
        "exit 27",
        "exit 1",
        "exit 99",
        "kill -TERM $$",
    ] {
        let status = Command::new("sh")
            .arg("-c")
            .arg(script)
            .envs(EXECUTOR_OUTCOME_ENV.map(|(key, value)| (key, value.to_string())))
            .stdout(std::process::Stdio::null())
            .status()
            .expect("run executor process contract");
        let alert = record_executor_exit(&mut outcome, status.code());
        if script == "exit 27" {
            assert_eq!(alert.unwrap().code, "autodeposit_dependency_unavailable");
        }
    }
    assert_eq!(outcome.executions_completed, 1);
    assert_eq!(outcome.executions_deferred, 1);
    assert_eq!(outcome.executions_recovery_pending, 1);
    assert_eq!(outcome.executions_noop, 1);
    assert_eq!(outcome.executions_not_actionable, 1);
    assert_eq!(outcome.executions_process_success_unclassified, 1);
    assert_eq!(outcome.executions_failed, 4);
}

#[tokio::test]
#[ignore = "requires a disposable loopback PostgreSQL database ending in _test"]
async fn overdue_query_uses_owned_work_and_repeated_actionable_idle_evidence() -> Result<()> {
    let url = std::env::var("AUTODEPOSIT_TEST_DATABASE_URL")?;
    let options = PgConnectOptions::from_str(&url)?;
    assert!(matches!(
        options.get_host(),
        "localhost" | "127.0.0.1" | "::1"
    ));
    assert!(options
        .get_database()
        .unwrap_or_default()
        .ends_with("_test"));
    let pool = PgPoolOptions::new()
        .max_connections(1)
        .connect_with(options)
        .await?;
    let mut tx = pool.begin().await?;
    // The fresh schema intentionally fails rather than altering an existing fixture
    // or application schema. Everything is rolled back, including DDL.
    sqlx::raw_sql(r#"
        CREATE SCHEMA loyal_yield;
        CREATE TABLE loyal_yield.balance_sweep_lot_claims (
            claim_token text PRIMARY KEY, target_id bigint, status text, created_at timestamptz,
            updated_at timestamptz DEFAULT now()
        );
        CREATE TABLE loyal_yield.balance_sweep_scheduled_slots (
            id bigint PRIMARY KEY, target_id bigint, claim_token text, token_mint text,
            status text, eligible_after timestamptz, last_error text,
            created_at timestamptz DEFAULT now(), updated_at timestamptz DEFAULT now()
        );
        CREATE TABLE loyal_yield.balance_sweep_transaction_attempts (
            claim_token text, operation_kind text, attempt_state text, attempt_number int
        );
        CREATE TABLE loyal_yield.balance_sweep_targets (
            id bigint PRIMARY KEY, token_mint text, desired_active bool, chain_status text,
            wallet_balance_floor_raw bigint, authority text, settings text,
            vault_index bigint, vault_pubkey text
        );
        CREATE TABLE loyal_yield.balance_sweep_wallet_balances_current (
            target_id bigint, mint text, amount_raw bigint
        );
        CREATE TABLE loyal_yield.balance_sweep_surplus_lots (
            id bigint PRIMARY KEY, scheduled_slot_id bigint, target_id bigint,
            status text, remaining_amount_raw bigint
        );
        CREATE TABLE loyal_yield.balance_sweep_lot_claim_items (lot_id bigint, claim_token text);
        CREATE TABLE loyal_yield.managed_vaults (
            active_policy_id bigint, active bool, settings text, vault_index bigint, vault_pubkey text
        );
        CREATE TABLE loyal_yield.route_policies (
            id bigint, active bool, authority text, settings text, vault_index bigint,
            vault_pubkey text, route_modes text[]
        );
        INSERT INTO loyal_yield.balance_sweep_lot_claims VALUES
            ('confirmed-pull', 1, 'selected', now() - interval '22 days', now()),
            ('confirmed-topup', 2, 'selected', now() - interval '1 day', now()),
            ('ambiguous', 3, 'selected', now() - interval '1 hour', now()),
            ('young', 4, 'selected', now() - interval '1 minute', now()),
            ('completed', 5, 'executed', now() - interval '1 day', now());
        INSERT INTO loyal_yield.balance_sweep_scheduled_slots
            (id, target_id, claim_token, token_mint, status, eligible_after)
            SELECT target_id, target_id, claim_token, 'USDC', 'selected', now()
            FROM loyal_yield.balance_sweep_lot_claims;
        INSERT INTO loyal_yield.balance_sweep_transaction_attempts VALUES
            ('confirmed-pull', 'pull', 'confirmed', 1),
            ('confirmed-topup', 'pull', 'confirmed', 1),
            ('confirmed-topup', 'top_up', 'confirmed', 1),
            ('ambiguous', 'pull', 'ambiguous', 1);
        INSERT INTO loyal_yield.balance_sweep_targets
            SELECT id, 'USDC', true, 'active', 100, 'a', 's', 0, 'v'
            FROM generate_series(10, 17) AS id;
        INSERT INTO loyal_yield.balance_sweep_wallet_balances_current
            SELECT id, 'USDC', CASE WHEN id = 11 THEN 100 ELSE 1000 END
            FROM generate_series(10, 17) AS id;
        INSERT INTO loyal_yield.managed_vaults VALUES (1, true, 's', 0, 'v');
        INSERT INTO loyal_yield.route_policies VALUES (1, true, 'a', 's', 0, 'v', ARRAY['same_mint_kamino']);
        INSERT INTO loyal_yield.balance_sweep_scheduled_slots
            (id, target_id, token_mint, status, eligible_after, last_error, created_at)
            SELECT id, id, 'USDC', 'scheduled', now() - interval '1 minute',
                   CASE WHEN id = 12 THEN NULL ELSE
                       'existing idle vault balance must drain before direct autodeposit: 1' END,
                   now() - interval '10 days'
            FROM generate_series(10, 17) AS id;
        INSERT INTO loyal_yield.balance_sweep_surplus_lots
            SELECT id, id, id, 'open', CASE WHEN id = 13 THEN 0 ELSE 900 END
            FROM generate_series(10, 17) AS id;
        INSERT INTO loyal_yield.balance_sweep_lot_claims (claim_token, target_id, status, created_at)
            SELECT id || ':' || n, id, 'released',
                   now() - CASE WHEN id = 16 THEN interval '1 minute' ELSE interval '2 hours' END
            FROM generate_series(10, 17) AS id CROSS JOIN generate_series(1, 3) AS n
            WHERE id != 14 OR n = 1;
        INSERT INTO loyal_yield.balance_sweep_lot_claim_items
            SELECT target_id, claim_token FROM loyal_yield.balance_sweep_lot_claims WHERE status = 'released';
        UPDATE loyal_yield.balance_sweep_targets SET desired_active = false WHERE id = 15;
        UPDATE loyal_yield.balance_sweep_targets SET settings = 'no-live-policy' WHERE id = 17;
    "#).execute(&mut *tx).await?;
    let rows = sqlx::query(OVERDUE_AUTODEPOSIT_WORK_SQL)
        .bind("USDC")
        .fetch_all(&mut *tx)
        .await?;
    let actual = rows
        .iter()
        .map(|row| {
            (
                row.get::<i64, _>("target_id"),
                row.get::<String, _>("owning_stage"),
            )
        })
        .collect::<Vec<_>>();
    assert_eq!(
        actual,
        vec![
            (1, "deposit_to_kamino".into()),
            (2, "finalize_deposit".into()),
            (10, "preflight_idle_drain".into()),
            (3, "reconcile_pull".into()),
        ]
    );
    assert!(rows
        .iter()
        .all(|row| row.get::<i64, _>("overdue_target_stage_count") == 4));
    // Repeated reads/lease-like touches must not erase age or alerts.
    sqlx::query("UPDATE loyal_yield.balance_sweep_lot_claims SET updated_at = now()")
        .execute(&mut *tx)
        .await?;
    let repeated = sqlx::query(OVERDUE_AUTODEPOSIT_WORK_SQL)
        .bind("USDC")
        .fetch_all(&mut *tx)
        .await?;
    assert_eq!(repeated.len(), 4);
    // Pre-claim deferrals have no released claim history. Their bounded durable
    // suffix must carry age/count; old slot timestamps alone are never evidence.
    sqlx::query("DELETE FROM loyal_yield.balance_sweep_lot_claim_items WHERE lot_id = 10")
        .execute(&mut *tx)
        .await?;
    let old_epoch: i64 =
        sqlx::query_scalar("SELECT EXTRACT(EPOCH FROM now() - interval '2 hours')::bigint")
            .fetch_one(&mut *tx)
            .await?;
    let young_epoch: i64 =
        sqlx::query_scalar("SELECT EXTRACT(EPOCH FROM now() - interval '5 minutes')::bigint")
            .fetch_one(&mut *tx)
            .await?;
    let prefix = "existing idle vault balance must drain before direct autodeposit: 1";
    for (error, expected_count) in [
        (
            format!("{prefix} [idle_blocked_since={old_epoch}; idle_deferrals=3]"),
            4,
        ),
        (
            format!("{prefix} [idle_blocked_since={young_epoch}; idle_deferrals=1000000]"),
            3,
        ),
        (
            format!("{prefix} [idle_blocked_since={old_epoch}; idle_deferrals=1]"),
            3,
        ),
        (
            format!("{prefix} [idle_blocked_since=bad; idle_deferrals=3]"),
            3,
        ),
        (
            format!("{prefix} [idle_blocked_since=9999999999999; idle_deferrals=3]"),
            3,
        ),
        (
            format!("{prefix} [idle_blocked_since={old_epoch}; idle_deferrals=99999999]"),
            3,
        ),
        (
            format!("unrelated [idle_blocked_since={old_epoch}; idle_deferrals=3]"),
            3,
        ),
        (prefix.to_owned(), 3),
        (
            format!("{prefix} [idle_blocked_since={old_epoch}; idle_deferrals=3]"),
            4,
        ),
    ] {
        sqlx::query("UPDATE loyal_yield.balance_sweep_scheduled_slots SET last_error = $1, updated_at = now() WHERE id = 10")
            .bind(error).execute(&mut *tx).await?;
        let marked = sqlx::query(OVERDUE_AUTODEPOSIT_WORK_SQL)
            .bind("USDC")
            .fetch_all(&mut *tx)
            .await?;
        assert_eq!(marked.len(), expected_count);
    }
    // A large backlog still emits only one oldest representative per stage,
    // while exposing the full distinct affected-target count.
    sqlx::raw_sql(r#"
        INSERT INTO loyal_yield.balance_sweep_lot_claims (claim_token, target_id, status, created_at)
            SELECT 'backlog:' || id, id, 'selected', now() - interval '21 days'
            FROM generate_series(100, 129) AS id;
        INSERT INTO loyal_yield.balance_sweep_scheduled_slots
            (id, target_id, claim_token, token_mint, status, eligible_after)
            SELECT target_id, target_id, claim_token, 'USDC', 'selected', now()
            FROM loyal_yield.balance_sweep_lot_claims WHERE target_id >= 100;
        INSERT INTO loyal_yield.balance_sweep_transaction_attempts
            SELECT claim_token, 'pull', 'confirmed', 1
            FROM loyal_yield.balance_sweep_lot_claims WHERE target_id >= 100;
    "#).execute(&mut *tx).await?;
    let backlog = sqlx::query(OVERDUE_AUTODEPOSIT_WORK_SQL)
        .bind("USDC")
        .fetch_all(&mut *tx)
        .await?;
    assert_eq!(backlog.len(), 4);
    assert_eq!(backlog[0].get::<i64, _>("target_id"), 1);
    assert_eq!(backlog[0].get::<i64, _>("overdue_stage_target_count"), 31);
    assert_eq!(backlog[0].get::<i64, _>("overdue_target_stage_count"), 34);
    // Completion and a drained open lot remove actionable ownership.
    sqlx::raw_sql("UPDATE loyal_yield.balance_sweep_lot_claims SET status = 'executed'; UPDATE loyal_yield.balance_sweep_surplus_lots SET remaining_amount_raw = 0;")
        .execute(&mut *tx).await?;
    assert!(sqlx::query(OVERDUE_AUTODEPOSIT_WORK_SQL)
        .bind("USDC")
        .fetch_all(&mut *tx)
        .await?
        .is_empty());
    tx.rollback().await?;
    Ok(())
}
