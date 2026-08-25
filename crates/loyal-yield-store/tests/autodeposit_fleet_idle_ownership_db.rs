use chrono::Utc;
use loyal_yield_store::{
    sqlx, IdleVaultDepositDecisionInput, NeonSqlClient, NeonSqlConfig, VaultId,
};
use std::time::Duration;

const DATABASE_URL_ENV: &str = "AUTODEPOSIT_FLEET_IDLE_VERIFY_DATABASE_URL";

async fn assert_idle_eligibility(
    client: &NeonSqlClient,
    vault_id: i64,
    mint: &str,
    expected: bool,
) {
    let single = client
        .current_idle_token_balance(VaultId(vault_id), mint)
        .await
        .expect("read single-vault idle balance");
    let batch = client
        .current_idle_token_balances_for_vaults(&[VaultId(vault_id)], mint)
        .await
        .expect("read batch idle balances");
    assert_eq!(
        single.is_some(),
        expected,
        "single-reader eligibility mismatch"
    );
    assert_eq!(
        batch.len(),
        usize::from(expected),
        "batch-reader eligibility mismatch"
    );
}

#[tokio::test]
#[ignore = "requires a fully migrated disposable PostgreSQL database"]
async fn autodeposit_attempt_state_owns_idle_funds_until_top_up_confirms() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("disposable database URL");
    assert!(
        database_url.contains("fleet_verify_autodeposit_idle"),
        "refusing to mutate a database outside the verifier namespace"
    );
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url.clone()))
        .await
        .expect("connect to disposable database");
    let suffix = format!("{}", std::process::id());
    let settings = format!("idle-ownership-settings-{suffix}");
    let vault_pubkey = format!("idle-ownership-vault-{suffix}");
    let mint = format!("idle-ownership-mint-{suffix}");
    let policy_account = format!("idle-ownership-policy-{suffix}");
    let signer = format!("idle-ownership-signer-{suffix}");

    let policy_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_policies
            (settings, authority, policy_seed, policy_account, vault_index,
             vault_pubkey, delegated_signers, threshold, route_modes,
             stable_mints, kamino_markets, kamino_liquidity_mints,
             last_seen_slot, last_seen_signature)
        VALUES ($1, $2, 1, $3, 1, $4, ARRAY[$5], 1,
                ARRAY['same_mint_kamino'], ARRAY[$6], ARRAY['market'],
                ARRAY[$6], 1, $7)
        RETURNING id
        "#,
    )
    .bind(&settings)
    .bind(format!("authority-{suffix}"))
    .bind(&policy_account)
    .bind(&vault_pubkey)
    .bind(&signer)
    .bind(&mint)
    .bind(format!("policy-seen-{suffix}"))
    .fetch_one(client.pool())
    .await
    .expect("insert route policy");
    let vault_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.managed_vaults
            (settings, vault_index, vault_pubkey, active_policy_id)
        VALUES ($1, 1, $2, $3)
        RETURNING id
        "#,
    )
    .bind(&settings)
    .bind(&vault_pubkey)
    .bind(policy_id)
    .fetch_one(client.pool())
    .await
    .expect("insert managed vault");
    let target_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.balance_sweep_targets
            (settings, authority, policy_seed, policy_account, vault_index,
             vault_pubkey, wallet, wallet_usdc_ata, vault_usdc_ata, token_mint,
             wallet_token_ata, vault_token_ata, delegated_signers, threshold,
             max_amount_per_period, desired_active, chain_status,
             chain_observation_slot, last_seen_slot, last_seen_signature, cluster)
        VALUES ($1, $2, 2, $3, 1, $4, $5, $6, $7, $8, $6, $7,
                ARRAY[$9], 1, 1000, TRUE, 'active', 1, 1, $10, 'mainnet-beta')
        RETURNING id
        "#,
    )
    .bind(&settings)
    .bind(format!("wallet-{suffix}"))
    .bind(format!("autodeposit-policy-{suffix}"))
    .bind(&vault_pubkey)
    .bind(format!("wallet-{suffix}"))
    .bind(format!("wallet-ata-{suffix}"))
    .bind(format!("vault-ata-{suffix}"))
    .bind(&mint)
    .bind(&signer)
    .bind(format!("target-seen-{suffix}"))
    .fetch_one(client.pool())
    .await
    .expect("insert Autodeposit target");
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.vault_idle_token_balances_current
            (vault_id, mint, amount_raw, owner, token_account, observed_slot,
             observed_at, source_commitment, updated_at)
        VALUES ($1, $2, 100, $3, $4, 1, now(), 'finalized', now())
        "#,
    )
    .bind(vault_id)
    .bind(&mint)
    .bind(&vault_pubkey)
    .bind(format!("idle-token-{suffix}"))
    .execute(client.pool())
    .await
    .expect("insert idle balance");
    let claim_token = format!("idle-ownership-claim-{suffix}");
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_lot_claims
            (claim_token, target_id, amount_raw, status)
        VALUES ($1, $2, 100, 'selected')
        "#,
    )
    .bind(&claim_token)
    .bind(target_id)
    .execute(client.pool())
    .await
    .expect("insert selected claim");

    assert_idle_eligibility(&client, vault_id, &mint, true).await;

    let idle_decision_input = IdleVaultDepositDecisionInput {
        target_reserve: format!("reserve-{suffix}"),
        target_market: Some("market".to_owned()),
        liquidity_mint: mint.clone(),
        amount_raw: 100,
        idle_token_account: format!("idle-token-{suffix}"),
        idle_observed_slot: 1,
        idle_observed_at: Utc::now(),
        target_apy_bps: 100,
        estimated_edge_bps: 100,
        estimated_cost_lamports: 1,
        setup_obligation_before_deposit: false,
        setup_obligation_policy: None,
        setup_obligation_policy_source: None,
        setup_obligation_vault_rent_top_up_lamports: 0,
    };

    let mut autodeposit_tx = client
        .pool()
        .begin()
        .await
        .expect("begin Autodeposit handoff");
    sqlx::query(
        r#"
        SELECT pg_advisory_xact_lock(
            hashtextextended(
                format('idle-vault-handoff:%s:%s', $1::BIGINT, $2::TEXT),
                0::BIGINT
            )
        )
        "#,
    )
    .bind(vault_id)
    .bind(&mint)
    .execute(&mut *autodeposit_tx)
    .await
    .expect("acquire Autodeposit side of idle handoff lock");

    let competing_database_url = database_url.clone();
    let competing_input = idle_decision_input.clone();
    let fleet_attempt = tokio::spawn(async move {
        let competing_client = NeonSqlClient::connect(NeonSqlConfig::new(competing_database_url))
            .await
            .expect("connect competing Fleet client");
        competing_client
            .record_idle_vault_deposit_decision(VaultId(vault_id), competing_input)
            .await
    });
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(
        !fleet_attempt.is_finished(),
        "Fleet must wait while Autodeposit owns the atomic handoff lock"
    );

    let pull_signature = format!("pull-signature-{suffix}");
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_transaction_attempts
            (claim_token, target_id, operation_kind, attempt_number, amount_raw,
             source_pre_balance_raw, destination_pre_balance_raw, signature,
             signed_transaction_base64, signed_transaction_sha256,
             recent_blockhash, last_valid_block_height, attempt_state)
        VALUES ($1, $2, 'pull', 1, 100, 100, 0, $3, 'c2lnbmVk',
                repeat('a', 64), 'blockhash', 100, 'prepared')
        "#,
    )
    .bind(&claim_token)
    .bind(target_id)
    .bind(&pull_signature)
    .execute(&mut *autodeposit_tx)
    .await
    .expect("insert prepared pull");
    autodeposit_tx
        .commit()
        .await
        .expect("commit prepared pull and release handoff lock");
    let fleet_error = fleet_attempt
        .await
        .expect("join competing Fleet attempt")
        .expect_err("Fleet must lose when Autodeposit prepares the pull first");
    assert!(
        fleet_error
            .to_string()
            .contains("owned by an active Autodeposit pull"),
        "unexpected Fleet ownership error: {fleet_error}"
    );
    assert_idle_eligibility(&client, vault_id, &mint, false).await;

    for terminal_state in ["failed", "expired"] {
        sqlx::query(
            "UPDATE loyal_yield.balance_sweep_transaction_attempts SET attempt_state = $1 WHERE signature = $2",
        )
        .bind(terminal_state)
        .bind(&pull_signature)
        .execute(client.pool())
        .await
        .expect("move pull to terminal state");
        assert_idle_eligibility(&client, vault_id, &mint, true).await;
    }

    sqlx::query(
        "UPDATE loyal_yield.balance_sweep_transaction_attempts SET attempt_state = 'confirmed', confirmed_slot = 2 WHERE signature = $1",
    )
    .bind(&pull_signature)
    .execute(client.pool())
    .await
    .expect("confirm pull");
    assert_idle_eligibility(&client, vault_id, &mint, false).await;

    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_transaction_attempts
            (claim_token, target_id, operation_kind, attempt_number, amount_raw,
             source_pre_balance_raw, destination_pre_balance_raw, signature,
             signed_transaction_base64, signed_transaction_sha256,
             recent_blockhash, last_valid_block_height, attempt_state, confirmed_slot)
        VALUES ($1, $2, 'top_up', 1, 100, 100, 0, $3, 'c2lnbmVk',
                repeat('b', 64), 'blockhash', 100, 'confirmed', 3)
        "#,
    )
    .bind(&claim_token)
    .bind(target_id)
    .bind(format!("top-up-signature-{suffix}"))
    .execute(client.pool())
    .await
    .expect("insert confirmed top-up");
    assert_idle_eligibility(&client, vault_id, &mint, true).await;

    let competing_claim = format!("idle-ownership-competing-claim-{suffix}");
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_lot_claims
            (claim_token, target_id, amount_raw, status)
        VALUES ($1, $2, 100, 'selected')
        "#,
    )
    .bind(&competing_claim)
    .bind(target_id)
    .execute(client.pool())
    .await
    .expect("insert competing Autodeposit claim");

    let mut fleet_tx = client.pool().begin().await.expect("begin Fleet handoff");
    sqlx::query(
        r#"
        SELECT pg_advisory_xact_lock(
            hashtextextended(
                format('idle-vault-handoff:%s:%s', $1::BIGINT, $2::TEXT),
                0::BIGINT
            )
        )
        "#,
    )
    .bind(vault_id)
    .bind(&mint)
    .execute(&mut *fleet_tx)
    .await
    .expect("acquire Fleet side of idle handoff lock");
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.rebalance_decisions
            (vault_id, status, target_reserve, liquidity_mint,
             source_liquidity_mint, target_liquidity_mint, amount_raw,
             source_apy_bps, target_apy_bps, estimated_edge_bps,
             decision_reason, execution_plan, idempotency_key)
        VALUES ($1, 'planned', $2, $3, $3, $3, 100, 0, 100, 100,
                'idle_vault_liquidity_available',
                jsonb_build_object('kind', 'idle_vault_deposit'), $4)
        "#,
    )
    .bind(vault_id)
    .bind(format!("fleet-owned-reserve-{suffix}"))
    .bind(&mint)
    .bind(format!("fleet-owned-idempotency-{suffix}"))
    .execute(&mut *fleet_tx)
    .await
    .expect("persist Fleet ownership before releasing handoff lock");

    let prepared_signature = format!("blocked-pull-signature-{suffix}");
    let competing_database_url = database_url.clone();
    let competing_mint = mint.clone();
    let autodeposit_attempt = tokio::spawn(async move {
        let competing_client = NeonSqlClient::connect(NeonSqlConfig::new(competing_database_url))
            .await
            .expect("connect competing Autodeposit client");
        let mut tx = competing_client
            .pool()
            .begin()
            .await
            .expect("begin competing Autodeposit handoff");
        sqlx::query(
            r#"
            SELECT pg_advisory_xact_lock(
                hashtextextended(
                    format('idle-vault-handoff:%s:%s', $1::BIGINT, $2::TEXT),
                    0::BIGINT
                )
            )
            "#,
        )
        .bind(vault_id)
        .bind(&competing_mint)
        .execute(&mut *tx)
        .await
        .expect("wait for Fleet handoff lock");
        let inserted_attempt: Option<i64> = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.balance_sweep_transaction_attempts
                (claim_token, target_id, operation_kind, attempt_number, amount_raw,
                 source_pre_balance_raw, destination_pre_balance_raw, signature,
                 signed_transaction_base64, signed_transaction_sha256,
                 recent_blockhash, last_valid_block_height, attempt_state)
            SELECT $1, $2, 'pull', 1, 100, 100, 0, $3, 'c2lnbmVk',
                   repeat('c', 64), 'blockhash', 100, 'prepared'
            WHERE NOT EXISTS (
                SELECT 1
                FROM loyal_yield.balance_sweep_targets AS target
                JOIN loyal_yield.managed_vaults AS vault
                  ON vault.settings = target.settings
                 AND vault.vault_index = target.vault_index
                 AND vault.vault_pubkey = target.vault_pubkey
                JOIN loyal_yield.rebalance_decisions AS fleet_decision
                  ON fleet_decision.vault_id = vault.id
                 AND fleet_decision.liquidity_mint = target.token_mint
                 AND fleet_decision.status::text IN (
                     'planned', 'simulating', 'ready', 'submitted', 'confirming'
                 )
                 AND fleet_decision.execution_plan ->> 'kind' = 'idle_vault_deposit'
                WHERE target.id = $2
            )
            RETURNING id
            "#,
        )
        .bind(&competing_claim)
        .bind(target_id)
        .bind(&prepared_signature)
        .fetch_optional(&mut *tx)
        .await
        .expect("attempt Autodeposit preparation while Fleet owns idle funds");
        tx.commit()
            .await
            .expect("commit competing Autodeposit handoff");
        inserted_attempt
    });
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(
        !autodeposit_attempt.is_finished(),
        "Autodeposit must wait while Fleet owns the atomic handoff lock"
    );
    fleet_tx
        .commit()
        .await
        .expect("commit Fleet ownership and release handoff lock");
    let inserted_attempt = autodeposit_attempt
        .await
        .expect("join competing Autodeposit attempt");
    assert!(
        inserted_attempt.is_none(),
        "Autodeposit must refuse to prepare a pull while Fleet owns the vault mint"
    );
}
