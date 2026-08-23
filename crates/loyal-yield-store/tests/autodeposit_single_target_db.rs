use loyal_yield_store::{
    AutodepositChainObservation, AutodepositRecurringDelegationObserved, NeonSqlClient,
    NeonSqlConfig,
};

const DATABASE_URL_ENV: &str = "ASK_2211_VERIFY_DATABASE_URL";

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
