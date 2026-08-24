use loyal_yield_store::{
    BalanceSweepPolicyMatchInput, CrossMintSwapPolicyManifestInput, NeonSqlClient, NeonSqlConfig,
    PolicyRemovalInput,
};

const DATABASE_URL_ENV: &str = "ASK_2211_ACTIVITY_VERIFY_DATABASE_URL";

fn autodeposit_policy(
    policy_account: &str,
    signature: &str,
    slot: u64,
) -> BalanceSweepPolicyMatchInput {
    BalanceSweepPolicyMatchInput {
        signature: signature.to_owned(),
        slot,
        cluster: "mainnet-beta".to_owned(),
        settings: "settings-activity".to_owned(),
        authority: "wallet-activity".to_owned(),
        policy_seed: slot,
        policy_account: policy_account.to_owned(),
        vault_index: 1,
        vault_pubkey: "vault-activity".to_owned(),
        wallet: "wallet-activity".to_owned(),
        wallet_usdc_ata: "wallet-ata-activity".to_owned(),
        vault_usdc_ata: "vault-ata-activity".to_owned(),
        token_mint: "mint-activity".to_owned(),
        wallet_token_ata: "wallet-ata-activity".to_owned(),
        vault_token_ata: "vault-ata-activity".to_owned(),
        delegated_signers: vec!["signer-activity".to_owned()],
        threshold: 1,
        max_amount_per_period: 10_000_000,
    }
}

fn removal(policy_account: &str, signature: &str, slot: u64) -> PolicyRemovalInput {
    PolicyRemovalInput {
        signature: signature.to_owned(),
        slot,
        cluster: "mainnet-beta".to_owned(),
        source_commitment: "finalized".to_owned(),
        settings: "settings-activity".to_owned(),
        authority: "wallet-activity".to_owned(),
        policy_account: policy_account.to_owned(),
    }
}

fn autoswap_manifest(
    policy_account: &str,
    signature: &str,
    slot: u64,
) -> CrossMintSwapPolicyManifestInput {
    CrossMintSwapPolicyManifestInput {
        signature: signature.to_owned(),
        slot,
        cluster: "mainnet-beta".to_owned(),
        source_commitment: "finalized".to_owned(),
        mutation: "create".to_owned(),
        settings: "settings-activity".to_owned(),
        authority: "wallet-activity".to_owned(),
        policy_seed: Some(slot),
        policy_account: policy_account.to_owned(),
        vault_index: 1,
        vault_pubkey: "vault-activity".to_owned(),
        delegated_signer: "signer-activity".to_owned(),
        source_shard: "classic".to_owned(),
        max_slippage_bps: 50,
        daily_source_mint_spending_cap: 10_000_000,
        manifest_fingerprint: format!("fingerprint-{slot}"),
    }
}

#[tokio::test]
#[ignore = "requires ASK_2211_ACTIVITY_VERIFY_DATABASE_URL pointing at a throwaway database"]
async fn lifecycle_activity_is_append_only_idempotent_and_atomic() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway database");

    let setup = autodeposit_policy("autodeposit-policy", "autodeposit-setup", 100);
    client
        .record_balance_sweep_policy_match(setup.clone())
        .await
        .expect("record finalized Autodeposit setup");
    client
        .record_balance_sweep_policy_match(setup)
        .await
        .expect("replay is idempotent for Autodeposit setup");

    client
        .record_policy_removal(removal("autodeposit-policy", "autodeposit-close", 101))
        .await
        .expect("record finalized Autodeposit close");
    client
        .record_policy_removal(removal("autodeposit-policy", "autodeposit-close", 101))
        .await
        .expect("replay is idempotent for Autodeposit close");

    let autodeposit_events: Vec<String> = sqlx::query_scalar(
        r#"
        SELECT event_type
        FROM loyal_yield.earn_activity_events
        WHERE entity_key = 'autodeposit-policy'
        ORDER BY event_slot
        "#,
    )
    .fetch_all(client.pool())
    .await
    .expect("load Autodeposit activity");
    // setup after close is preserved instead of being overwritten in mutable state.
    assert_eq!(
        autodeposit_events,
        vec![
            "autodeposit_created".to_owned(),
            "autodeposit_closed".to_owned(),
        ]
    );

    let autoswap = autoswap_manifest("autoswap-policy", "autoswap-setup", 200);
    client
        .record_cross_mint_swap_policy_manifest(autoswap.clone())
        .await
        .expect("record finalized Autoswap setup");
    client
        .record_cross_mint_swap_policy_manifest(autoswap)
        .await
        .expect("replay is idempotent for Autoswap setup");
    client
        .record_policy_removal(removal("autoswap-policy", "autoswap-close", 201))
        .await
        .expect("record finalized Autoswap close");
    client
        .record_policy_removal(removal("autoswap-policy", "autoswap-close", 201))
        .await
        .expect("replay is idempotent for Autoswap close");

    let autoswap_events: Vec<String> = sqlx::query_scalar(
        r#"
        SELECT event_type
        FROM loyal_yield.earn_activity_events
        WHERE entity_key = 'autoswap-policy'
        ORDER BY event_slot
        "#,
    )
    .fetch_all(client.pool())
    .await
    .expect("load Autoswap activity");
    assert_eq!(
        autoswap_events,
        vec!["autoswap_created".to_owned(), "autoswap_closed".to_owned()]
    );

    sqlx::query(
        r#"
        CREATE FUNCTION loyal_yield.reject_atomic_activity_test()
        RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN
          IF NEW.entity_key = 'autodeposit-policy-atomic' THEN
            RAISE EXCEPTION 'forced activity insert failure';
          END IF;
          RETURN NEW;
        END;
        $$
        "#,
    )
    .execute(client.pool())
    .await
    .expect("install atomic rollback probe function");
    sqlx::query(
        r#"
        CREATE TRIGGER reject_atomic_activity_test
        BEFORE INSERT ON loyal_yield.earn_activity_events
        FOR EACH ROW EXECUTE FUNCTION loyal_yield.reject_atomic_activity_test()
        "#,
    )
    .execute(client.pool())
    .await
    .expect("install atomic rollback probe");

    let atomic_result = client
        .record_balance_sweep_policy_match(autodeposit_policy(
            "autodeposit-policy-atomic",
            "autodeposit-setup-atomic",
            300,
        ))
        .await;
    assert!(
        atomic_result.is_err(),
        "activity insert must fail the state write"
    );
    let partial_state_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.balance_sweep_targets WHERE policy_account = 'autodeposit-policy-atomic'",
    )
    .fetch_one(client.pool())
    .await
    .expect("check atomic rollback state");
    assert_eq!(
        partial_state_count, 0,
        "atomic rollback removes partial state"
    );
}
