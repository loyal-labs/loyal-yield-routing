use loyal_yield_store::{
    AutodepositChainObservation, AutodepositRecurringDelegationObserved,
    BalanceSweepPolicyMatchInput, NeonSqlClient, NeonSqlConfig,
};

const DATABASE_URL_ENV: &str = "ASK_2211_VERIFY_DATABASE_URL";

fn policy_match(policy_account: &str, policy_seed: u64, slot: u64) -> BalanceSweepPolicyMatchInput {
    BalanceSweepPolicyMatchInput {
        signature: format!("signature-{policy_seed}"),
        slot,
        cluster: "mainnet-beta".to_owned(),
        settings: "settings-rollover".to_owned(),
        authority: "wallet-rollover".to_owned(),
        policy_seed,
        policy_account: policy_account.to_owned(),
        vault_index: 1,
        vault_pubkey: "vault-rollover".to_owned(),
        wallet: "wallet-rollover".to_owned(),
        wallet_usdc_ata: "wallet-ata-rollover".to_owned(),
        vault_usdc_ata: "vault-ata-rollover".to_owned(),
        token_mint: "mint-rollover".to_owned(),
        wallet_token_ata: "wallet-ata-rollover".to_owned(),
        vault_token_ata: "vault-ata-rollover".to_owned(),
        delegated_signers: vec!["signer-rollover".to_owned()],
        threshold: 1,
        max_amount_per_period: 10_000_000,
    }
}

#[tokio::test]
#[ignore = "requires ASK_2211_VERIFY_DATABASE_URL pointing at a throwaway database"]
async fn newer_policy_atomically_replaces_pending_target() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway database");

    let first = client
        .record_balance_sweep_policy_match(policy_match("policy-rollover-1", 1, 100))
        .await
        .expect("record first pending policy");
    let second = client
        .record_balance_sweep_policy_match(policy_match("policy-rollover-2", 2, 101))
        .await
        .expect("replace the pending policy without a unique-index collision");
    assert_ne!(first.id, second.id);

    let rows: Vec<(String, bool, String)> = sqlx::query_as(
        r#"
        SELECT policy_account, desired_active, chain_status
        FROM loyal_yield.balance_sweep_targets
        WHERE settings = 'settings-rollover'
        ORDER BY policy_seed
        "#,
    )
    .fetch_all(client.pool())
    .await
    .expect("load rollover rows");
    assert_eq!(
        rows,
        vec![
            ("policy-rollover-1".to_owned(), false, "closed".to_owned()),
            ("policy-rollover-2".to_owned(), true, "pending".to_owned()),
        ]
    );

    let replay = client
        .record_balance_sweep_policy_match(policy_match("policy-rollover-stale", 3, 99))
        .await
        .expect("ignore a stale policy observation without colliding");
    assert_eq!(replay.id, second.id);
    let current_count: i64 = sqlx::query_scalar(
        r#"
        SELECT COUNT(*)
        FROM loyal_yield.balance_sweep_targets
        WHERE settings = 'settings-rollover'
          AND chain_status <> 'closed'
        "#,
    )
    .fetch_one(client.pool())
    .await
    .expect("count current rollover targets");
    assert_eq!(current_count, 1);
}

#[tokio::test]
#[ignore = "requires ASK_2211_VERIFY_DATABASE_URL pointing at a throwaway database"]
async fn single_target_preserves_intent_and_owns_chain_lifecycle() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway database");

    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_targets
            (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
             wallet, wallet_usdc_ata, vault_usdc_ata, token_mint, wallet_token_ata,
             vault_token_ata, delegated_signers, threshold, max_amount_per_period,
             desired_active, chain_status, chain_observation_slot,
             wallet_balance_floor_raw, last_seen_slot, last_seen_signature, cluster)
        VALUES
            ('settings-ask-2211','wallet-ask-2211',7,'policy-ask-2211',1,
             'vault-ask-2211','wallet-ask-2211','wallet-ata-ask-2211',
             'vault-ata-ask-2211','mint-ask-2211','wallet-ata-ask-2211',
             'vault-ata-ask-2211',ARRAY['signer-ask-2211'],1,10000000,
             TRUE,'pending',100,NULL,100,'policy-signature-ask-2211','mainnet-beta')
        "#,
    )
    .execute(client.pool())
    .await
    .expect("seed discovered policy target");

    let target_id = client
        .record_autodeposit_recurring_delegation(AutodepositRecurringDelegationObserved {
            wallet: "wallet-ask-2211".to_owned(),
            vault_pubkey: "vault-ask-2211".to_owned(),
            subscription_authority: "authority-ask-2211".to_owned(),
            recurring_delegation: "delegation-ask-2211".to_owned(),
            nonce: 11,
            amount_per_period: 10_000_000,
            period_length_seconds: 2_592_000,
            start_timestamp: 1,
            expiry_timestamp: 0,
            signature: "delegation-signature-ask-2211".to_owned(),
            slot: 101,
        })
        .await
        .expect("attach delegation discovered from exact transaction");

    let active = client
        .reconcile_autodeposit_chain_observation(AutodepositChainObservation {
            target_id,
            observation_slot: 102,
            observation_complete: true,
            policy_valid: true,
            subscription_authority_valid: true,
            recurring_delegation_valid: true,
            token_delegate_valid: true,
            wallet_balance_raw: 9_000_000,
        })
        .await
        .expect("activate finalized chain state");
    assert_eq!(active.chain_status, "active");
    assert_eq!(active.bootstrap_generation, None);

    let bootstrap_lots: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id = $1",
    )
    .bind(target_id.as_i64())
    .fetch_one(client.pool())
    .await
    .unwrap();
    assert_eq!(bootstrap_lots, 0);

    sqlx::query(
        "UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw = 5000000 WHERE id = $1",
    )
    .bind(target_id.as_i64())
    .execute(client.pool())
    .await
    .unwrap();
    let bootstrapped = client
        .reconcile_autodeposit_chain_observation(AutodepositChainObservation {
            target_id,
            observation_slot: 103,
            observation_complete: true,
            policy_valid: true,
            subscription_authority_valid: true,
            recurring_delegation_valid: true,
            token_delegate_valid: true,
            wallet_balance_raw: 9_000_000,
        })
        .await
        .expect("bootstrap after initial floor intent arrives");
    assert_eq!(bootstrapped.bootstrap_generation, Some(1));

    sqlx::query(
        "UPDATE loyal_yield.balance_sweep_targets SET desired_active = FALSE WHERE id = $1",
    )
    .bind(target_id.as_i64())
    .execute(client.pool())
    .await
    .unwrap();
    sqlx::query(
        "UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw = 7000000 WHERE id = $1",
    )
    .bind(target_id.as_i64())
    .execute(client.pool())
    .await
    .unwrap();

    client
        .reconcile_autodeposit_chain_observation(AutodepositChainObservation {
            target_id,
            observation_slot: 104,
            observation_complete: true,
            policy_valid: true,
            subscription_authority_valid: true,
            recurring_delegation_valid: true,
            token_delegate_valid: true,
            wallet_balance_raw: 9_000_000,
        })
        .await
        .expect("replay active state without changing intent");

    let row: (bool, i64, i64) = sqlx::query_as(
        "SELECT desired_active, wallet_balance_floor_raw, (SELECT COUNT(*) FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id = $1) FROM loyal_yield.balance_sweep_targets WHERE id = $1",
    )
    .bind(target_id.as_i64())
    .fetch_one(client.pool())
    .await
    .unwrap();
    assert_eq!(row, (false, 7_000_000, 1));

    let stale = client
        .reconcile_autodeposit_chain_observation(AutodepositChainObservation {
            target_id,
            observation_slot: 99,
            observation_complete: true,
            policy_valid: false,
            subscription_authority_valid: false,
            recurring_delegation_valid: false,
            token_delegate_valid: false,
            wallet_balance_raw: 0,
        })
        .await
        .expect("ignore stale close");
    assert_eq!(stale.chain_status, "active");

    let closed = client
        .reconcile_autodeposit_chain_observation(AutodepositChainObservation {
            target_id,
            observation_slot: 105,
            observation_complete: true,
            policy_valid: false,
            subscription_authority_valid: true,
            recurring_delegation_valid: false,
            token_delegate_valid: false,
            wallet_balance_raw: 9_000_000,
        })
        .await
        .expect("close finalized chain state");
    assert_eq!(closed.chain_status, "closed");

    let live_work: i64 = sqlx::query_scalar(
        r#"
        SELECT
          (SELECT COUNT(*) FROM loyal_yield.balance_sweep_scheduled_slots
           WHERE target_id = $1 AND status IN ('scheduled','requested'))
        + (SELECT COUNT(*) FROM loyal_yield.balance_sweep_surplus_lots
           WHERE target_id = $1 AND status = 'open')
        "#,
    )
    .bind(target_id.as_i64())
    .fetch_one(client.pool())
    .await
    .unwrap();
    assert_eq!(live_work, 0);

    let reasons: Vec<String> = sqlx::query_scalar(
        "SELECT reason FROM loyal_yield.realtime_events WHERE event_type = 'earn.autodeposit.configuration.changed' ORDER BY id",
    )
    .fetch_all(client.pool())
    .await
    .unwrap();
    assert!(reasons.contains(&"allowance_created".to_owned()));
    assert!(reasons.contains(&"allowance_paused".to_owned()));
    assert!(reasons.contains(&"allowance_updated".to_owned()));
    assert!(reasons.contains(&"allowance_removed".to_owned()));
}

#[tokio::test]
#[ignore = "requires ASK_2211_VERIFY_DATABASE_URL pointing at a throwaway database"]
async fn confirmed_rebalance_emits_once_after_single_target_migration() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway database");

    let policy_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_policies
            (settings, authority, policy_seed, policy_account, vault_index,
             vault_pubkey, delegated_signers, threshold, route_modes,
             stable_mints, kamino_markets, kamino_liquidity_mints,
             last_seen_slot, last_seen_signature)
        VALUES
            ('settings-rebalance-60', 'wallet-rebalance-60', 60,
             'route-policy-rebalance-60', 1, 'vault-rebalance-60',
             ARRAY['signer-rebalance-60'], 1, ARRAY['same_mint_kamino'],
             ARRAY['mint-rebalance-60'], ARRAY['market-rebalance-60'],
             ARRAY['mint-rebalance-60'], 600, 'route-policy-signature-60')
        RETURNING id
        "#,
    )
    .fetch_one(client.pool())
    .await
    .expect("insert route policy");

    let vault_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.managed_vaults
            (settings, vault_index, vault_pubkey, active_policy_id)
        VALUES ('settings-rebalance-60', 1, 'vault-rebalance-60', $1)
        RETURNING id
        "#,
    )
    .bind(policy_id)
    .fetch_one(client.pool())
    .await
    .expect("insert managed vault");

    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_targets
            (settings, authority, policy_seed, policy_account, vault_index,
             vault_pubkey, wallet, wallet_usdc_ata, vault_usdc_ata, token_mint,
             wallet_token_ata, vault_token_ata, delegated_signers, threshold,
             max_amount_per_period, desired_active, chain_status,
             chain_observation_slot, wallet_balance_floor_raw, last_seen_slot,
             last_seen_signature, cluster)
        VALUES
            ('settings-rebalance-60', 'wallet-rebalance-60', 60,
             'sweep-policy-rebalance-60', 1, 'vault-rebalance-60',
             'wallet-rebalance-60', 'wallet-ata-rebalance-60',
             'vault-ata-rebalance-60', 'mint-rebalance-60',
             'wallet-ata-rebalance-60', 'vault-ata-rebalance-60',
             ARRAY['signer-rebalance-60'], 1, 1000000, FALSE, 'active',
             600, 0, 600, 'target-signature-rebalance-60', 'mainnet-beta')
        "#,
    )
    .execute(client.pool())
    .await
    .expect("insert paused but chain-current target");

    let source_snapshot_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.vault_position_snapshots
            (vault_id, policy_id, observed_slot, is_current)
        VALUES ($1, $2, 600, FALSE)
        RETURNING id
        "#,
    )
    .bind(vault_id)
    .bind(policy_id)
    .fetch_one(client.pool())
    .await
    .expect("insert source snapshot");
    let post_snapshot_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.vault_position_snapshots
            (vault_id, policy_id, observed_slot, is_current)
        VALUES ($1, $2, 601, TRUE)
        RETURNING id
        "#,
    )
    .bind(vault_id)
    .bind(policy_id)
    .fetch_one(client.pool())
    .await
    .expect("insert post snapshot");

    let decision_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.rebalance_decisions
            (vault_id, source_snapshot_id, status, source_reserve,
             target_reserve, liquidity_mint, source_liquidity_mint,
             target_liquidity_mint, amount_raw, source_apy_bps,
             target_apy_bps, estimated_edge_bps, decision_reason,
             idempotency_key)
        VALUES
            ($1, $2, 'confirming', 'source-reserve-rebalance-60',
             'target-reserve-rebalance-60', 'mint-rebalance-60',
             'mint-rebalance-60', 'mint-rebalance-60', 1000000, 100,
             200, 100, 'target_supply_apy_exceeds_source',
             'decision-rebalance-60')
        RETURNING id
        "#,
    )
    .bind(vault_id)
    .bind(source_snapshot_id)
    .fetch_one(client.pool())
    .await
    .expect("insert confirming decision");

    sqlx::query(
        r#"
        UPDATE loyal_yield.rebalance_decisions
        SET status = 'confirmed',
            confirmed_slot = 601,
            post_snapshot_id = $2,
            updated_at = now()
        WHERE id = $1
        "#,
    )
    .bind(decision_id)
    .bind(post_snapshot_id)
    .execute(client.pool())
    .await
    .expect("confirm rebalance after migration 60");

    sqlx::query("UPDATE loyal_yield.rebalance_decisions SET updated_at = now() WHERE id = $1")
        .bind(decision_id)
        .execute(client.pool())
        .await
        .expect("repeat confirmed update");

    let events: Vec<(String, String, String)> = sqlx::query_as(
        r#"
        SELECT event_type, solana_env, source_id
        FROM loyal_yield.realtime_events
        WHERE source_table = 'rebalance_decisions'
          AND source_id = $1
        ORDER BY id
        "#,
    )
    .bind(decision_id.to_string())
    .fetch_all(client.pool())
    .await
    .expect("load rebalance realtime events");
    assert_eq!(
        events,
        vec![(
            "earn.rebalance.confirmed".to_owned(),
            "mainnet-beta".to_owned(),
            decision_id.to_string(),
        )]
    );
}
