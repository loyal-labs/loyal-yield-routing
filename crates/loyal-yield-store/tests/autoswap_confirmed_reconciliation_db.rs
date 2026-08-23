use loyal_yield_store::{
    CrossMintSwapPolicyManifestInput, CrossMintVaultOptInLookup, NeonSqlClient, NeonSqlConfig,
    PolicyRemovalInput,
};

const DATABASE_URL_ENV: &str = "ASK_2192_VERIFY_DATABASE_URL";
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
        source_commitment: "confirmed".to_owned(),
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
#[ignore = "requires ASK_2192_VERIFY_DATABASE_URL pointing at a throwaway database"]
async fn autoswap_confirmed_pair_reconciles_once_and_removal_clears_it() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway database");
    let database_name: String = sqlx::query_scalar("SELECT current_database()")
        .fetch_one(client.pool())
        .await
        .expect("read test database name");
    assert!(
        database_name.contains("ask_2192_autoswap_confirmed"),
        "refusing to mutate database {database_name:?}"
    );

    let classic = manifest(
        "AddressLookupTab1e1111111111111111111111111",
        "classic",
        100,
    );
    let token = manifest(
        "BPFLoader1111111111111111111111111111111111",
        "token_2022",
        101,
    );

    let first = client
        .record_cross_mint_swap_policy_manifest(classic.clone())
        .await
        .expect("record first confirmed shard")
        .expect("first policy is decoded");
    assert!(first.start_eligible);
    assert_eq!(
        client
            .load_cross_mint_vault_opt_in(lookup())
            .await
            .expect("load after one shard"),
        None
    );

    let second = client
        .record_cross_mint_swap_policy_manifest(token.clone())
        .await
        .expect("record second confirmed shard")
        .expect("second policy is decoded");
    assert!(second.start_eligible);
    let ready = client
        .load_cross_mint_vault_opt_in(lookup())
        .await
        .expect("load confirmed pair")
        .expect("confirmed pair reconciles to one enrollment");
    assert!(ready.enabled);
    assert_eq!(ready.generation, 1);

    client
        .record_cross_mint_swap_policy_manifest(classic)
        .await
        .expect("replay classic shard");
    client
        .record_cross_mint_swap_policy_manifest(token)
        .await
        .expect("replay token shard");
    let replayed = client
        .load_cross_mint_vault_opt_in(lookup())
        .await
        .expect("load replayed pair")
        .expect("replayed enrollment remains");
    assert_eq!(replayed.generation, 1);

    for (signature, slot, policy) in [
        (
            "remove-token",
            102,
            "BPFLoader1111111111111111111111111111111111",
        ),
        (
            "remove-classic",
            103,
            "AddressLookupTab1e1111111111111111111111111",
        ),
    ] {
        client
            .record_policy_removal(PolicyRemovalInput {
                signature: signature.to_owned(),
                slot,
                cluster: CLUSTER.to_owned(),
                source_commitment: "confirmed".to_owned(),
                settings: SETTINGS.to_owned(),
                authority: AUTHORITY.to_owned(),
                policy_account: policy.to_owned(),
            })
            .await
            .expect("record confirmed policy removal");
    }
    assert_eq!(
        client
            .load_cross_mint_vault_opt_in(lookup())
            .await
            .expect("load after removal"),
        None
    );

    let reasons: Vec<String> = sqlx::query_scalar(
        "SELECT reason FROM loyal_yield.realtime_events WHERE event_type = 'earn.autoswap.configuration.changed' ORDER BY id",
    )
    .fetch_all(client.pool())
    .await
    .expect("load Autoswap realtime events");
    assert_eq!(reasons, vec!["autoswap_installed", "autoswap_removed"]);
}
