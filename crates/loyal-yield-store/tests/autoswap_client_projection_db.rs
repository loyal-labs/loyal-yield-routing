use loyal_yield_store::{
    CrossMintSwapPolicyManifestInput, CrossMintVaultOptInLookup, NeonSqlClient, NeonSqlConfig,
    PolicyRemovalInput,
};

const DATABASE_URL_ENV: &str = "AUTOSWAP_CLIENT_VERIFY_DATABASE_URL";
const CLUSTER: &str = "mainnet-beta";
const SETTINGS: &str = "Vote111111111111111111111111111111111111111";
const AUTHORITY: &str = "Stake11111111111111111111111111111111111111";
const VAULT: &str = "Config1111111111111111111111111111111111111";
const SIGNER: &str = "ComputeBudget111111111111111111111111111111";

fn manifest(policy: &str, shard: &str, slot: u64) -> CrossMintSwapPolicyManifestInput {
    CrossMintSwapPolicyManifestInput {
        signature: format!("signature-{policy}"),
        slot,
        cluster: CLUSTER.to_owned(),
        source_commitment: "finalized".to_owned(),
        mutation: "create".to_owned(),
        settings: SETTINGS.to_owned(),
        authority: AUTHORITY.to_owned(),
        policy_seed: Some(slot),
        policy_account: policy.to_owned(),
        vault_index: 1,
        vault_pubkey: VAULT.to_owned(),
        delegated_signer: SIGNER.to_owned(),
        source_shard: shard.to_owned(),
        max_slippage_bps: 50,
        daily_source_mint_spending_cap: 1_000_000,
        manifest_fingerprint: format!("fingerprint-{policy}"),
    }
}

fn lookup() -> CrossMintVaultOptInLookup {
    CrossMintVaultOptInLookup {
        cluster: CLUSTER.to_owned(),
        settings: SETTINGS.to_owned(),
        vault_index: 1,
        vault_pubkey: VAULT.to_owned(),
    }
}

#[tokio::test]
#[ignore = "requires AUTOSWAP_CLIENT_VERIFY_DATABASE_URL pointing at a throwaway database"]
async fn autoswap_opt_in_is_created_by_pair_and_replay_preserves_pause() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway database");

    let database_name: String = sqlx::query_scalar("SELECT current_database()")
        .fetch_one(client.pool())
        .await
        .expect("read test database name");
    assert!(
        database_name.contains("ask_2168_autoswap_client"),
        "refusing to mutate database {database_name:?}"
    );

    let classic = manifest(
        "AddressLookupTab1e1111111111111111111111111",
        "classic",
        100,
    );
    client
        .record_cross_mint_swap_policy_manifest(classic.clone())
        .await
        .expect("record first shard");
    assert_eq!(
        client
            .load_cross_mint_vault_opt_in(lookup())
            .await
            .expect("load opt-in after first shard"),
        None,
        "one policy is not an enrollment"
    );

    let token = manifest(
        "BPFLoader1111111111111111111111111111111111",
        "token_2022",
        101,
    );
    client
        .record_cross_mint_swap_policy_manifest(token.clone())
        .await
        .expect("record second shard");
    let enrolled = client
        .load_cross_mint_vault_opt_in(lookup())
        .await
        .expect("load pair-derived opt-in")
        .expect("complete pair creates opt-in");
    assert!(enrolled.enabled);
    assert_eq!(enrolled.generation, 1);

    let paused = client
        .disable_cross_mint_vault_opt_in(lookup(), enrolled.generation)
        .await
        .expect("pause opt-in")
        .expect("opt-in exists");
    assert!(!paused.enabled);
    assert_eq!(paused.generation, 2);

    client
        .record_cross_mint_swap_policy_manifest(classic)
        .await
        .expect("replay first shard");
    client
        .record_cross_mint_swap_policy_manifest(token)
        .await
        .expect("replay second shard");
    let still_paused = client
        .load_cross_mint_vault_opt_in(lookup())
        .await
        .expect("load replayed opt-in")
        .expect("opt-in remains");
    assert!(
        !still_paused.enabled,
        "chain replay must not change user intent"
    );
    assert_eq!(still_paused.generation, 2);

    client
        .record_policy_removal(PolicyRemovalInput {
            signature: "remove-token".to_owned(),
            slot: 102,
            cluster: CLUSTER.to_owned(),
            source_commitment: "finalized".to_owned(),
            settings: SETTINGS.to_owned(),
            authority: AUTHORITY.to_owned(),
            policy_account: "BPFLoader1111111111111111111111111111111111".to_owned(),
        })
        .await
        .expect("remove one shard");
    let resume_error = client
        .enable_cross_mint_vault_opt_in(lookup(), still_paused.generation)
        .await
        .expect_err("resume must fail closed without a complete pair");
    assert!(resume_error
        .to_string()
        .contains("canonical finalized policy pair"));

    client
        .record_policy_removal(PolicyRemovalInput {
            signature: "remove-classic".to_owned(),
            slot: 103,
            cluster: CLUSTER.to_owned(),
            source_commitment: "finalized".to_owned(),
            settings: SETTINGS.to_owned(),
            authority: AUTHORITY.to_owned(),
            policy_account: "AddressLookupTab1e1111111111111111111111111".to_owned(),
        })
        .await
        .expect("remove second shard");
    assert_eq!(
        client
            .load_cross_mint_vault_opt_in(lookup())
            .await
            .expect("load opt-in after complete removal"),
        None,
        "the finalized removal of both on-chain shards removes enrollment"
    );

    let reasons: Vec<String> = sqlx::query_scalar(
        "SELECT reason FROM loyal_yield.realtime_events WHERE event_type = 'earn.autoswap.configuration.changed' ORDER BY id",
    )
    .fetch_all(client.pool())
    .await
    .expect("load Autoswap realtime events");
    assert_eq!(
        reasons,
        vec!["autoswap_installed", "autoswap_paused", "autoswap_removed"]
    );
}
