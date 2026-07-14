use std::{env, error::Error};

use chrono::Utc;
use loyal_yield_orchestrator::{
    sqlx::{postgres::PgPoolOptions, Row},
    ConfirmSameMintRebalanceInput, DecisionId, DecisionStatus, NeonSqlClient, PolicyMatchInput,
    ReconciledReservePosition, ReconciledVaultState, AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
    ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
};
use serde_json::{json, Value};

type TestResult<T> = Result<T, Box<dyn Error>>;

fn reconciled_position(
    reserve: &str,
    market: &str,
    amount_raw: u64,
    planning_metadata: Value,
) -> ReconciledReservePosition {
    ReconciledReservePosition {
        reserve: reserve.to_owned(),
        market: Some(market.to_owned()),
        liquidity_mint: "USDC".to_owned(),
        amount_raw,
        supply_apy_bps: None,
        borrow_apy_bps: None,
        planning_metadata,
    }
}

#[tokio::test]
#[ignore = "requires an explicitly isolated disposable Postgres database"]
async fn supplied_post_snapshot_is_projected_before_decision_confirmation() -> TestResult<()> {
    if env::var("SAME_MINT_CONFIRMATION_DB_VERIFY_ISOLATED").as_deref() != Ok("1") {
        return Err(
            "set SAME_MINT_CONFIRMATION_DB_VERIFY_ISOLATED=1 only for a disposable DB".into(),
        );
    }
    let database_url = env::var("NEON_DATABASE_URL")?;
    let pool = PgPoolOptions::new()
        .max_connections(4)
        .connect(&database_url)
        .await?;
    let database_name: String =
        loyal_yield_orchestrator::sqlx::query_scalar("SELECT current_database()")
            .fetch_one(&pool)
            .await?;
    assert!(
        database_name.contains("same_mint_confirmation"),
        "disposable DB name must contain same_mint_confirmation"
    );

    let client = NeonSqlClient::from_pool(pool.clone());
    client.apply_migrations().await?;
    let run = Utc::now().timestamp_micros();
    let stored = client
        .record_policy_match(PolicyMatchInput {
            signature: format!("policy-signature-{run}"),
            slot: 1_000,
            settings: format!("settings-{run}"),
            authority: format!("authority-{run}"),
            policy_seed: 1,
            policy_account: format!("policy-{run}"),
            vault_index: 1,
            vault_pubkey: format!("vault-{run}"),
            delegated_signers: vec![format!("signer-{run}")],
            threshold: 1,
            route_modes: vec!["kamino_same_mint".to_owned()],
            stable_mints: vec!["USDC".to_owned()],
            kamino_markets: vec!["source-market".to_owned(), "target-market".to_owned()],
            kamino_liquidity_mints: vec!["USDC".to_owned()],
            universe_preset: None,
            risk_profile: None,
            swap_lanes: json!([]),
        })
        .await?;

    let post_snapshot = client
        .reconcile_vault(
            stored.vault.id,
            ReconciledVaultState {
                observed_slot: 1_003,
                observed_at: Some(Utc::now()),
                chain_slot: Some(1_003),
                lock_attempt_id: None,
                context: json!({
                    "kind": "same_mint_chain_reconcile_preview",
                    "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
                }),
                positions: vec![
                    reconciled_position(
                        "source",
                        "source-market",
                        0,
                        json!({
                            "source": "chain_reconcile_preview",
                            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
                            "redeemable_liquidity_amount_raw": "0",
                        }),
                    ),
                    reconciled_position(
                        "target",
                        "target-market",
                        4_052_119,
                        json!({
                            "source": "chain_reconcile_preview",
                            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
                            "redeemable_liquidity_amount_raw": "4233354",
                        }),
                    ),
                    reconciled_position(
                        "preserved",
                        "preserved-market",
                        80,
                        json!({
                            "source": "chain_reconcile_preview",
                            "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                        }),
                    ),
                ],
            },
        )
        .await?;
    let signature = format!("same-mint-signature-{run}");
    let decision_id = DecisionId(
        loyal_yield_orchestrator::sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.rebalance_decisions
                (vault_id, status, source_reserve, target_reserve, liquidity_mint,
                 source_liquidity_mint, target_liquidity_mint, amount_raw,
                 source_apy_bps, target_apy_bps, estimated_edge_bps,
                 decision_reason, execution_plan, idempotency_key, signature, submitted_slot)
            VALUES
                ($1, 'confirming', 'source', 'target', 'USDC', 'USDC', 'USDC', 4233255,
                 100, 200, 100, 'target_supply_apy_exceeds_source', $2, $3, $4, 1002)
            RETURNING id
            "#,
        )
        .bind(stored.vault.id.as_i64())
        .bind(json!({
            "kind": "same_mint",
            "route_amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
            "source_amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
            "redeemable_source_liquidity_amount_raw": 4_233_255,
        }))
        .bind(format!("same-mint-confirmation-test-{run}"))
        .bind(&signature)
        .fetch_one(&pool)
        .await?,
    );
    let before_confirm = client.current_positions(stored.vault.id).await?;
    assert_eq!(
        before_confirm
            .iter()
            .find(|position| position.reserve == "target")
            .map(|position| position.amount_raw),
        Some(4_052_119),
        "fixture must expose the old collateral-unit current projection"
    );

    let confirmed = client
        .confirm_same_mint_rebalance(ConfirmSameMintRebalanceInput {
            decision_id,
            signature: signature.clone(),
            submitted_slot: Some(1_002),
            confirmed_slot: 1_003,
            observed_at: Some(Utc::now()),
            post_snapshot_id: Some(post_snapshot.id),
        })
        .await?;
    assert_eq!(confirmed.status, DecisionStatus::Confirmed);

    let snapshot_rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT reserve, amount_raw, planning_metadata
        FROM loyal_yield.vault_position_snapshot_positions
        WHERE snapshot_id = $1
        ORDER BY reserve
        "#,
    )
    .bind(post_snapshot.id.as_i64())
    .fetch_all(&pool)
    .await?;
    let target_snapshot = snapshot_rows
        .iter()
        .find(|row| row.get::<String, _>("reserve") == "target")
        .expect("target snapshot row exists");
    assert_eq!(target_snapshot.get::<i64, _>("amount_raw"), 4_233_354);
    let target_metadata: Value = target_snapshot.get("planning_metadata");
    assert_eq!(
        target_metadata
            .get("amount_semantics")
            .and_then(Value::as_str),
        Some(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY)
    );
    assert_eq!(
        target_metadata
            .get("observed_collateral_amount_raw")
            .and_then(Value::as_i64),
        Some(4_052_119)
    );

    let current = client.current_positions(stored.vault.id).await?;
    assert_eq!(
        current
            .iter()
            .find(|position| position.reserve == "source")
            .map(|position| position.amount_raw),
        Some(0)
    );
    assert_eq!(
        current
            .iter()
            .find(|position| position.reserve == "target")
            .map(|position| position.amount_raw),
        Some(4_233_354)
    );
    assert_eq!(
        current
            .iter()
            .find(|position| position.reserve == "preserved")
            .map(|position| position.amount_raw),
        Some(80)
    );

    let snapshot_context: Value = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT context FROM loyal_yield.vault_position_snapshots WHERE id = $1",
    )
    .bind(post_snapshot.id.as_i64())
    .fetch_one(&pool)
    .await?;
    assert_eq!(
        snapshot_context.get("kind").and_then(Value::as_str),
        Some("same_mint_rebalance_confirmed")
    );
    assert_eq!(
        snapshot_context
            .pointer("/source_snapshot/kind")
            .and_then(Value::as_str),
        Some("same_mint_chain_reconcile_preview")
    );
    assert_eq!(
        snapshot_context
            .get("amount_semantics")
            .and_then(Value::as_str),
        Some(ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY)
    );

    let decision_row = loyal_yield_orchestrator::sqlx::query(
        "SELECT status::text AS status, post_snapshot_id FROM loyal_yield.rebalance_decisions WHERE id = $1",
    )
    .bind(decision_id.as_i64())
    .fetch_one(&pool)
    .await?;
    assert_eq!(decision_row.get::<String, _>("status"), "confirmed");
    assert_eq!(
        decision_row.get::<Option<i64>, _>("post_snapshot_id"),
        Some(post_snapshot.id.as_i64())
    );

    Ok(())
}
