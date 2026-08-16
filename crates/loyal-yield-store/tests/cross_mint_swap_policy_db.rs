use loyal_yield_store::{
    CrossMintSwapPolicyLookup, CrossMintSwapPolicyManifestInput, NeonSqlClient, NeonSqlConfig,
    PolicyRemovalInput,
};

const DATABASE_URL_ENV: &str = "CROSS_MINT_STORE_TEST_DATABASE_URL";
const CLUSTER: &str = "cross-mint-policy-catalog-db-test";
const SETTINGS: &str = "policy-catalog-settings";
const AUTHORITY: &str = "policy-catalog-authority";
const VAULT: &str = "policy-catalog-vault";

fn manifest(
    signature: &str,
    slot: u64,
    commitment: &str,
    policy_account: &str,
    source_shard: &str,
) -> CrossMintSwapPolicyManifestInput {
    CrossMintSwapPolicyManifestInput {
        signature: signature.to_owned(),
        slot,
        cluster: CLUSTER.to_owned(),
        source_commitment: commitment.to_owned(),
        mutation: "create".to_owned(),
        settings: SETTINGS.to_owned(),
        authority: AUTHORITY.to_owned(),
        policy_seed: Some(slot),
        policy_account: policy_account.to_owned(),
        vault_index: 1,
        vault_pubkey: VAULT.to_owned(),
        delegated_signer: "policy-catalog-signer".to_owned(),
        source_shard: source_shard.to_owned(),
        max_slippage_bps: 50,
        daily_source_mint_spending_cap: 1_000_000,
        manifest_fingerprint: format!("fingerprint-{policy_account}"),
    }
}

fn lookup(minimum_slot: u64) -> CrossMintSwapPolicyLookup {
    CrossMintSwapPolicyLookup {
        cluster: CLUSTER.to_owned(),
        settings: SETTINGS.to_owned(),
        vault_index: 1,
        vault_pubkey: VAULT.to_owned(),
        minimum_slot,
    }
}

async fn catalog(client: &NeonSqlClient, minimum_slot: u64) -> Vec<String> {
    let mut policies = client
        .load_finalized_active_cross_mint_swap_policies(lookup(minimum_slot))
        .await
        .expect("load finalized active policy catalog")
        .into_iter()
        .map(|policy| policy.policy_account)
        .collect::<Vec<_>>();
    policies.sort();
    policies
}

#[tokio::test]
#[ignore = "requires CROSS_MINT_STORE_TEST_DATABASE_URL pointing at a throwaway database with migrations 0001-0036 applied"]
async fn one_row_policy_catalog_is_finality_and_ambiguity_safe() {
    let database_url = match std::env::var(DATABASE_URL_ENV) {
        Ok(value) => value,
        Err(_) => {
            eprintln!("skipping: {DATABASE_URL_ENV} is not set");
            return;
        }
    };
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway policy catalog database");
    client
        .apply_migrations()
        .await
        .expect("apply the undeployed store migrations");

    let database_name: String = sqlx::query_scalar("SELECT current_database()")
        .fetch_one(client.pool())
        .await
        .expect("read test database name");
    assert!(
        database_name.contains("cross_mint_store_test"),
        "refusing to mutate database {database_name:?}"
    );

    let confirmed = client
        .record_cross_mint_swap_policy_manifest(manifest(
            "classic-create",
            100,
            "confirmed",
            "classic-policy",
            "classic",
        ))
        .await
        .expect("insert one confirmed classic policy row")
        .expect("confirmed manifest has complete policy identity");
    assert!(!confirmed.start_eligible);
    assert_eq!(catalog(&client, 0).await, Vec::<String>::new());

    let finalized = client
        .record_cross_mint_swap_policy_manifest(manifest(
            "classic-create",
            100,
            "finalized",
            "classic-policy",
            "classic",
        ))
        .await
        .expect("upgrade the same observation to finalized")
        .expect("finalized manifest has complete policy identity");
    assert!(finalized.start_eligible);
    assert_eq!(catalog(&client, 0).await, vec!["classic-policy"]);

    let stale = client
        .record_cross_mint_swap_policy_manifest(manifest(
            "stale-different-signature",
            99,
            "finalized",
            "classic-policy",
            "token_2022",
        ))
        .await
        .expect("ignore stale observation")
        .expect("stale observation returns the complete current policy");
    assert_eq!(stale.source_shard, "classic");
    assert_eq!(stale.last_seen_slot, 100);
    assert_eq!(catalog(&client, 101).await, Vec::<String>::new());

    let token_2022 = client
        .record_cross_mint_swap_policy_manifest(manifest(
            "token-create",
            110,
            "finalized",
            "token-policy",
            "token_2022",
        ))
        .await
        .expect("insert one finalized token-2022 policy row")
        .expect("token manifest has complete policy identity");
    assert!(token_2022.start_eligible);
    let both_shards = catalog(&client, 0).await;
    assert_eq!(both_shards, vec!["classic-policy", "token-policy"]);

    let duplicate_shard = client
        .record_cross_mint_swap_policy_manifest(manifest(
            "duplicate-classic",
            111,
            "finalized",
            "duplicate-classic-policy",
            "classic",
        ))
        .await
        .expect("insert duplicate classic shard row")
        .expect("duplicate manifest still has complete policy identity");
    assert!(duplicate_shard.start_eligible);
    let duplicate_visible = client
        .load_finalized_active_cross_mint_swap_policies(lookup(0))
        .await
        .expect("load duplicate shard rows");
    assert_eq!(duplicate_visible.len(), 3);
    assert_eq!(
        duplicate_visible
            .iter()
            .filter(|policy| policy.source_shard == "classic")
            .count(),
        2
    );
    assert_eq!(
        duplicate_visible
            .iter()
            .filter(|policy| policy.source_shard == "token_2022")
            .count(),
        1
    );

    let ambiguous = client
        .record_cross_mint_swap_policy_manifest(manifest(
            "token-conflicting-update",
            110,
            "finalized",
            "token-policy",
            "token_2022",
        ))
        .await
        .expect("mark a conflicting immutable observation ambiguous")
        .expect("ambiguous row retains its complete policy identity");
    assert!(!ambiguous.active);
    assert!(!ambiguous.start_eligible);
    assert_eq!(ambiguous.last_mutation, "ambiguous");
    assert_eq!(
        catalog(&client, 0).await,
        vec!["classic-policy", "duplicate-classic-policy"]
    );

    let removal = client
        .record_policy_removal(PolicyRemovalInput {
            signature: "classic-remove".to_owned(),
            slot: 120,
            cluster: CLUSTER.to_owned(),
            source_commitment: "finalized".to_owned(),
            settings: SETTINGS.to_owned(),
            authority: AUTHORITY.to_owned(),
            policy_account: "classic-policy".to_owned(),
        })
        .await
        .expect("deactivate one policy row");
    assert!(removal.swap_policy_deactivated);
    assert_eq!(catalog(&client, 0).await, vec!["duplicate-classic-policy"]);

    let unseen_removal = client
        .record_policy_removal(PolicyRemovalInput {
            signature: "unseen-remove".to_owned(),
            slot: 500,
            cluster: CLUSTER.to_owned(),
            source_commitment: "finalized".to_owned(),
            settings: SETTINGS.to_owned(),
            authority: AUTHORITY.to_owned(),
            policy_account: "removed-before-observed".to_owned(),
        })
        .await
        .expect("persist an unseen removal watermark");
    assert!(!unseen_removal.swap_policy_deactivated);
    let delayed_create = client
        .record_cross_mint_swap_policy_manifest(manifest(
            "delayed-create",
            499,
            "finalized",
            "removed-before-observed",
            "token_2022",
        ))
        .await
        .expect("ignore a create older than the removal watermark");
    assert!(delayed_create.is_none());

    client
        .record_cross_mint_swap_policy_manifest(manifest(
            "same-transaction",
            600,
            "finalized",
            "same-transaction-policy",
            "token_2022",
        ))
        .await
        .expect("record same-transaction create")
        .expect("same-transaction create is complete");
    let same_transaction_removal = client
        .record_policy_removal(PolicyRemovalInput {
            signature: "same-transaction".to_owned(),
            slot: 600,
            cluster: CLUSTER.to_owned(),
            source_commitment: "finalized".to_owned(),
            settings: SETTINGS.to_owned(),
            authority: AUTHORITY.to_owned(),
            policy_account: "same-transaction-policy".to_owned(),
        })
        .await
        .expect("apply a removal later in the same transaction");
    assert!(same_transaction_removal.swap_policy_deactivated);
    assert_eq!(catalog(&client, 0).await, vec!["duplicate-classic-policy"]);

    let row_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.cross_mint_swap_policies WHERE cluster = $1",
    )
    .bind(CLUSTER)
    .fetch_one(client.pool())
    .await
    .expect("count one-row policy catalog");
    assert_eq!(row_count, 5);
}
