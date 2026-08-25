use loyal_yield_store::{fleet_orchestration::TargetCapacityObservation, NeonSqlClient};
use sqlx::{postgres::PgPoolOptions, Row};

const DATABASE_URL_ENV: &str = "CROSS_MINT_STORE_TEST_DATABASE_URL";

#[tokio::test]
#[ignore = "requires CROSS_MINT_STORE_TEST_DATABASE_URL pointing at a throwaway database with migrations 0001-0037 applied"]
async fn finalized_effects_drive_custody_parent_and_capacity_lifecycle() {
    let database_url = match std::env::var(DATABASE_URL_ENV) {
        Ok(value) => value,
        Err(_) => {
            eprintln!("skipping: {DATABASE_URL_ENV} is not set");
            return;
        }
    };
    let pool = PgPoolOptions::new()
        .max_connections(1)
        .connect(&database_url)
        .await
        .expect("connect to throwaway cross-mint store database");
    let database_name: String = sqlx::query_scalar("SELECT current_database()")
        .fetch_one(&pool)
        .await
        .expect("read test database name");
    assert!(
        database_name.contains("cross_mint_store_test"),
        "refusing to mutate database {database_name:?}"
    );

    let mut tx = pool.begin().await.expect("begin fixture transaction");
    let policy_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_policies
            (settings, authority, policy_seed, policy_account, vault_index,
             vault_pubkey, delegated_signers, threshold, route_modes,
             stable_mints, kamino_markets, kamino_liquidity_mints,
             last_seen_slot, last_seen_signature)
        VALUES
            ('cross-mint-test-settings', 'cross-mint-test-authority', 1,
             'cross-mint-test-policy', 1, 'cross-mint-test-vault-pubkey',
             ARRAY['cross-mint-test-policy'], 1, ARRAY['yield_route'],
             ARRAY['source-mint', 'target-mint'], ARRAY['market'],
             ARRAY['source-mint', 'target-mint'], 1, 'policy-seen')
        RETURNING id
        "#,
    )
    .fetch_one(&mut *tx)
    .await
    .expect("insert test policy");
    let lookup_table_family_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.lookup_table_families
            (cluster, logical_name, kind, planner_version, catalog_version,
             provisioning_authority, payer, hard_capacity,
             largest_atomic_expansion, safety_margin, allocation_high_water)
        VALUES
            ('cross-mint-store-test', 'cross-mint-store-test-family',
             'shared_market', 'test-v1', 'test-v1',
             'cross-mint-test-policy', 'cross-mint-test-policy',
             256, 8, 8, 240)
        RETURNING id
        "#,
    )
    .fetch_one(&mut *tx)
    .await
    .expect("insert reusable ALT family");
    let lookup_table_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_lookup_tables
            (cluster, scope, table_address, authority, payer, status,
             durable, address_count, family_id, allocation_kind,
             generation, shard_ordinal, desired_state,
             accepting_allocations, allocation_high_water,
             reserved_address_count, usable_address_count, mutation_epoch)
        VALUES
            ('cross-mint-store-test', 'cross-mint-store-test-scope',
             'cross-mint-store-test-table', 'cross-mint-test-policy',
             'cross-mint-test-policy', 'usable', TRUE, 0, $1,
             'shared_market', 1, 0, 'active', TRUE, 240, 0, 0, 1)
        RETURNING id
        "#,
    )
    .bind(lookup_table_family_id)
    .fetch_one(&mut *tx)
    .await
    .expect("insert reusable ALT table");
    let vault_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.managed_vaults
            (settings, vault_index, vault_pubkey, active_policy_id)
        VALUES ('cross-mint-test-settings', 1,
                'cross-mint-test-vault-pubkey', $1)
        RETURNING id
        "#,
    )
    .bind(policy_id)
    .fetch_one(&mut *tx)
    .await
    .expect("insert test vault");
    let snapshot_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.vault_position_snapshots
            (vault_id, policy_id, observed_slot, chain_slot, context)
        VALUES ($1, $2, 1, 1, '{}'::jsonb)
        RETURNING id
        "#,
    )
    .bind(vault_id)
    .bind(policy_id)
    .fetch_one(&mut *tx)
    .await
    .expect("insert test snapshot");
    let epoch_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.optimizer_epochs
            (cluster, epoch_key, market_slot, observed_at, expires_at,
             market_state)
        VALUES ('cross-mint-store-test', 'cross-mint-store-test-epoch', 1,
                now(), now() + interval '1 hour', '{}'::jsonb)
        RETURNING id
        "#,
    )
    .fetch_one(&mut *tx)
    .await
    .expect("insert test epoch");
    let decision_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.rebalance_decisions
            (vault_id, source_snapshot_id, status, source_reserve,
             target_reserve, source_liquidity_mint,
             target_liquidity_mint, amount_raw, source_apy_bps,
             target_apy_bps, estimated_edge_bps,
             estimated_cost_lamports, decision_reason, execution_plan,
             idempotency_key, movement_route, active_target_reserve,
             custody_mint, custody_amount_raw, custody_account,
             custody_version, cross_mint_activation_control_generation,
             cross_mint_preflight_certification)
        VALUES
            ($1, $2, 'confirming', 'source-reserve', 'target-reserve',
             'source-mint', 'target-mint', 100, 100, 200, 100, 10000,
             'target_supply_apy_exceeds_source',
             '{"kind":"cross_mint_jupiter"}'::jsonb,
             'cross-mint-store-test-decision', 'cross_mint_jupiter',
             'target-reserve', 'source-mint', 100, 'source-reserve', 0, 1,
             '{"kind":"cross_mint_preflight","fixture":true}'::jsonb)
        RETURNING id
        "#,
    )
    .bind(vault_id)
    .bind(snapshot_id)
    .fetch_one(&mut *tx)
    .await
    .expect("insert cross-mint movement");
    let opportunity_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.rebalance_opportunities
            (cluster, idempotency_key, rediscovery_key, attempt_generation,
             vault_id, source_snapshot_id, optimizer_epoch_id,
             route_fingerprint, requirements_fingerprint, source_reserve,
             target_reserve, liquidity_mint, source_liquidity_mint,
             target_liquidity_mint, amount_raw, principal_usd_micros,
             source_apy_bps, target_apy_bps, estimated_edge_bps,
             estimated_cost_lamports, annual_yield_gain_usd_micros,
             expected_net_gain_usd_micros, economic_priority,
             priority_version, opportunity_state, execution_plan,
             available_at, expires_at, decision_id)
        VALUES
            ('cross-mint-store-test', 'cross-mint-store-test-opportunity',
             'cross-mint-store-test-opportunity', 1, $1, $2, $3,
             'route-fingerprint', 'requirements-fingerprint',
             'source-reserve', 'target-reserve', 'target-mint',
             'source-mint', 'target-mint', 100, 1000000, 100, 200, 100,
             10000, 100000, 90000, 100000, 'test-v1',
             'decision_created',
             '{"kind":"cross_mint_jupiter","source_liquidity_mint":"source-mint","target_liquidity_mint":"target-mint"}'::jsonb,
             now(), now() + interval '1 hour', $4)
        RETURNING id
        "#,
    )
    .bind(vault_id)
    .bind(snapshot_id)
    .bind(epoch_id)
    .bind(decision_id)
    .fetch_one(&mut *tx)
    .await
    .expect("insert immutable opportunity");
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.target_capacity_frontiers
            (cluster, target_reserve, liquidity_mint,
             observed_supply_usd_micros, observed_slot,
             maximum_inflight_usd_micros)
        VALUES ('cross-mint-store-test', 'target-reserve', 'target-mint',
                100000000, 10, 10000000)
        "#,
    )
    .execute(&mut *tx)
    .await
    .expect("insert target capacity frontier");
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.target_capacity_reservations
            (cluster, target_reserve, liquidity_mint, opportunity_id,
             decision_id, principal_usd_micros,
             admitted_observed_supply_usd_micros, admitted_observed_slot,
             admitted_maximum_inflight_usd_micros,
             admitted_telemetry_version, reservation_generation,
             admitted_observed_target_apy_bps,
             admitted_projected_target_apy_bps, admitted_source_apy_bps,
             admitted_edge_bps, admitted_net_holding_gain_usd_micros,
             admitted_fee_cap_lamports, reservation_fencing_token)
        VALUES
            ('cross-mint-store-test', 'target-reserve', 'target-mint', $1,
             $2, 1000000, 100000000, 10, 10000000, 0, 1,
             200, 190, 100, 90, 90000, 10000, 1)
        "#,
    )
    .bind(opportunity_id)
    .bind(decision_id)
    .execute(&mut *tx)
    .await
    .expect("insert movement-owned capacity");

    let withdraw_id = insert_submission(
        &mut tx,
        opportunity_id,
        decision_id,
        snapshot_id,
        epoch_id,
        lookup_table_id,
        "withdraw",
        "optimize_yield",
        1,
        "withdraw-signature",
    )
    .await;
    sqlx::query(
        r#"
        UPDATE loyal_yield.rebalance_decisions
        SET custody_mint = 'source-mint', custody_amount_raw = 97,
            custody_account = 'source-idle-ata', custody_observed_balance_raw = 102,
            custody_reconciled_slot = 20,
            custody_version = 1
        WHERE id = $1
        "#,
    )
    .bind(decision_id)
    .execute(&mut *tx)
    .await
    .expect("advance withdrawal custody projection");
    reconcile_submission(
        &mut tx,
        withdraw_id,
        20,
        None,
        Some(("source-mint", "source-idle-ata", 97)),
    )
    .await;
    sqlx::query("SET CONSTRAINTS ALL IMMEDIATE")
        .execute(&mut *tx)
        .await
        .expect("verify intermediate custody projection");
    sqlx::query("SET CONSTRAINTS ALL DEFERRED")
        .execute(&mut *tx)
        .await
        .expect("restore deferred custody verification");

    let intermediate = sqlx::query(
        r#"
        SELECT decision.status::text AS status, decision.terminal_outcome,
               opportunity.opportunity_state,
               reservation.reservation_state,
               decision.continuation_available_at IS NOT NULL AS continuation_ready
        FROM loyal_yield.rebalance_decisions decision
        JOIN loyal_yield.rebalance_opportunities opportunity
          ON opportunity.decision_id = decision.id
        JOIN loyal_yield.target_capacity_reservations reservation
          ON reservation.decision_id = decision.id
        WHERE decision.id = $1
        "#,
    )
    .bind(decision_id)
    .fetch_one(&mut *tx)
    .await
    .expect("read intermediate lifecycle");
    assert_eq!(intermediate.get::<String, _>("status"), "confirming");
    assert!(intermediate
        .get::<Option<String>, _>("terminal_outcome")
        .is_none());
    assert_eq!(
        intermediate.get::<String, _>("opportunity_state"),
        "decision_created"
    );
    assert_eq!(intermediate.get::<String, _>("reservation_state"), "active");
    assert!(intermediate.get::<bool, _>("continuation_ready"));

    let swap_id = insert_submission(
        &mut tx,
        opportunity_id,
        decision_id,
        snapshot_id,
        epoch_id,
        lookup_table_id,
        "swap",
        "optimize_yield",
        1,
        "swap-signature",
    )
    .await;
    sqlx::query(
        r#"
        UPDATE loyal_yield.rebalance_decisions
        SET custody_mint = 'target-mint', custody_amount_raw = 95,
            custody_account = 'target-idle-ata', custody_observed_balance_raw = 100,
            custody_reconciled_slot = 30,
            custody_version = 2, continuation_available_at = NULL
        WHERE id = $1
        "#,
    )
    .bind(decision_id)
    .execute(&mut *tx)
    .await
    .expect("advance swap custody projection");
    reconcile_submission(
        &mut tx,
        swap_id,
        30,
        Some(("source-mint", "source-idle-ata", 97)),
        Some(("target-mint", "target-idle-ata", 95)),
    )
    .await;

    let deposit_id = insert_submission(
        &mut tx,
        opportunity_id,
        decision_id,
        snapshot_id,
        epoch_id,
        lookup_table_id,
        "deposit",
        "optimize_yield",
        1,
        "deposit-signature",
    )
    .await;
    sqlx::query(
        r#"
        UPDATE loyal_yield.rebalance_decisions
        SET custody_mint = 'target-mint', custody_amount_raw = 0,
            custody_account = 'target-reserve', custody_observed_balance_raw = NULL,
            custody_reconciled_slot = 40,
            custody_version = 3, terminal_outcome = 'completed_target',
            status = 'confirmed', signature = 'deposit-signature',
            confirmed_slot = 40, continuation_available_at = NULL
        WHERE id = $1
        "#,
    )
    .bind(decision_id)
    .execute(&mut *tx)
    .await
    .expect("terminalize target deposit projection");
    reconcile_submission(
        &mut tx,
        deposit_id,
        40,
        Some(("target-mint", "target-idle-ata", 95)),
        None,
    )
    .await;
    sqlx::query("SET CONSTRAINTS ALL IMMEDIATE")
        .execute(&mut *tx)
        .await
        .expect("verify terminal custody projection");

    let terminal = sqlx::query(
        r#"
        SELECT decision.status::text AS status, decision.terminal_outcome,
               opportunity.opportunity_state,
               reservation.reservation_state
        FROM loyal_yield.rebalance_decisions decision
        JOIN loyal_yield.rebalance_opportunities opportunity
          ON opportunity.decision_id = decision.id
        JOIN loyal_yield.target_capacity_reservations reservation
          ON reservation.decision_id = decision.id
        WHERE decision.id = $1
        "#,
    )
    .bind(decision_id)
    .fetch_one(&mut *tx)
    .await
    .expect("read terminal lifecycle");
    assert_eq!(terminal.get::<String, _>("status"), "confirmed");
    assert_eq!(
        terminal.get::<String, _>("terminal_outcome"),
        "completed_target"
    );
    assert_eq!(terminal.get::<String, _>("opportunity_state"), "completed");
    assert_eq!(
        terminal.get::<String, _>("reservation_state"),
        "awaiting_telemetry"
    );
    tx.commit()
        .await
        .expect("commit terminal lifecycle fixture");

    let client = NeonSqlClient::from_pool(pool.clone());
    let movement_slot: i64 = sqlx::query_scalar(
        "SELECT movement_slot FROM loyal_yield.target_capacity_reservations WHERE decision_id = $1",
    )
    .bind(decision_id)
    .fetch_one(&pool)
    .await
    .expect("read durable capacity movement slot");
    let equal_slot_released = client
        .refresh_target_capacity_from_market_epoch(TargetCapacityObservation {
            cluster: "cross-mint-store-test".to_owned(),
            target_reserve: "target-reserve".to_owned(),
            liquidity_mint: "target-mint".to_owned(),
            observed_supply_usd_micros: 101_000_000,
            observed_slot: movement_slot,
            maximum_inflight_usd_micros: 10_000_000,
        })
        .await
        .expect("refresh equal-slot target telemetry");
    assert_eq!(equal_slot_released, 0);

    let newer_slot_released = client
        .refresh_target_capacity_from_market_epoch(TargetCapacityObservation {
            cluster: "cross-mint-store-test".to_owned(),
            target_reserve: "target-reserve".to_owned(),
            liquidity_mint: "target-mint".to_owned(),
            observed_supply_usd_micros: 101_000_000,
            observed_slot: movement_slot + 1,
            maximum_inflight_usd_micros: 10_000_000,
        })
        .await
        .expect("refresh newer target telemetry");
    assert_eq!(newer_slot_released, 1);
    let released = sqlx::query(
        r#"
        SELECT reservation_state, release_reason
        FROM loyal_yield.target_capacity_reservations
        WHERE decision_id = $1
        "#,
    )
    .bind(decision_id)
    .fetch_one(&pool)
    .await
    .expect("read planner-refreshed capacity lifecycle");
    assert_eq!(released.get::<String, _>("reservation_state"), "released");
    assert_eq!(
        released.get::<String, _>("release_reason"),
        "planner_target_telemetry_reflected_movement"
    );
}

#[allow(clippy::too_many_arguments)]
async fn insert_submission(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    opportunity_id: i64,
    decision_id: i64,
    snapshot_id: i64,
    epoch_id: i64,
    lookup_table_id: i64,
    leg: &str,
    purpose: &str,
    generation: i64,
    signature: &str,
) -> i64 {
    let expected_effect = if leg == "deposit" {
        serde_json::json!({
            "debit": {
                "mint": "target-mint",
                "tokenAccount": "target-idle-ata",
                "amountRaw": 95,
            }
        })
    } else {
        serde_json::json!({})
    };
    sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.signed_route_submissions
            (cluster, semantic_key, opportunity_id, decision_id,
             signed_transaction, signed_transaction_hash, message_hash,
             transaction_signature, recent_blockhash,
             last_valid_block_height, source_snapshot_id,
             optimizer_epoch_id, alt_requirements_fingerprint,
             alt_selection_fingerprint, alt_mutation_epochs, fee_payer,
             compiled_fee_lamports, writable_account_keys,
             conflict_account_keys, executor_owner,
             executor_fencing_token, submission_state, confirmed_slot,
             finalized_slot, finalized_at, movement_leg, leg_purpose,
             leg_generation, required_commitment, policy_account,
             expected_effect)
        VALUES
            ('cross-mint-store-test', $1, $2, $3, '\x01', 'hash',
             'message-hash', $4, 'blockhash', 1000, $5, $6,
             'requirements-fingerprint', 'selection-fingerprint',
             $11, 'cross-mint-test-policy', 1,
             ARRAY['cross-mint-test-policy'],
             ARRAY['vault-write:test', 'fleet-shared-write-lane:1'],
             'test-worker', 1, 'reconciliation_pending', $7, $7, now(),
             $8, $9, $10, 'finalized', 'cross-mint-test-policy',
             $12)
        RETURNING id
        "#,
    )
    .bind(format!("{leg}-{generation}-{signature}"))
    .bind(opportunity_id)
    .bind(decision_id)
    .bind(signature)
    .bind(snapshot_id)
    .bind(epoch_id)
    .bind(10 + generation)
    .bind(leg)
    .bind(purpose)
    .bind(generation)
    .bind(serde_json::json!({
        "tables": [{"tableId": lookup_table_id}],
    }))
    .bind(expected_effect)
    .fetch_one(&mut **tx)
    .await
    .expect("insert finalized reconciliation-pending leg")
}

async fn reconcile_submission(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    submission_id: i64,
    slot: i64,
    debit: Option<(&str, &str, i64)>,
    credit: Option<(&str, &str, i64)>,
) {
    let effect = serde_json::json!({
        "debit": debit.map(|(mint, token_account, amount_raw)| {
            serde_json::json!({
                "mint": mint,
                "tokenAccount": token_account,
                "amountRaw": amount_raw,
            })
        }),
        "credit": credit.map(|(mint, token_account, amount_raw)| {
            serde_json::json!({
                "mint": mint,
                "tokenAccount": token_account,
                "amountRaw": amount_raw,
            })
        }),
    });
    let balance_anchors = serde_json::json!({
        "debit": debit.map(|(mint, token_account, _)| {
            serde_json::json!({
                "mint": mint,
                "tokenAccount": token_account,
                "amountRaw": 5,
            })
        }),
        "credit": credit.map(|(mint, token_account, amount_raw)| {
            serde_json::json!({
                "mint": mint,
                "tokenAccount": token_account,
                "amountRaw": amount_raw + 5,
            })
        }),
    });
    sqlx::query(
        r#"
        UPDATE loyal_yield.signed_route_submissions
        SET submission_state = 'reconciled', reconciled_slot = $2,
            reconciled_at = now(), reconciled_effect = $3,
            reconciled_balance_anchors = $4,
            effect_debit_mint = $5, effect_debit_account = $6,
            effect_debit_amount_raw = $7, effect_credit_mint = $8,
            effect_credit_account = $9, effect_credit_amount_raw = $10
        WHERE id = $1
        "#,
    )
    .bind(submission_id)
    .bind(slot)
    .bind(effect)
    .bind(balance_anchors)
    .bind(debit.map(|value| value.0))
    .bind(debit.map(|value| value.1))
    .bind(debit.map(|value| value.2))
    .bind(credit.map(|value| value.0))
    .bind(credit.map(|value| value.1))
    .bind(credit.map(|value| value.2))
    .execute(&mut **tx)
    .await
    .expect("reconcile finalized leg effect");
}
