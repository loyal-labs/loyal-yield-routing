use loyal_yield_store::NeonSqlClient;
use sqlx::postgres::PgPoolOptions;

const DATABASE_URL_ENV: &str = "EARN_WATCH_TEST_DATABASE_URL";
const SCHEMA_FIXTURE: &str =
    include_str!("../../../test-fixtures/earn-watch-production-schema.sql");

#[tokio::test]
#[ignore = "requires EARN_WATCH_TEST_DATABASE_URL pointing at a throwaway database"]
async fn production_schema_combination_filters_app_settings_in_sql() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let pool = PgPoolOptions::new()
        .max_connections(1)
        .connect(&database_url)
        .await
        .expect("connect to test database");
    sqlx::raw_sql(SCHEMA_FIXTURE)
        .execute(&pool)
        .await
        .expect("install production-shaped watch schema");

    let store = NeonSqlClient::from_pool(pool);
    let targets = store
        .load_earn_subscription_targets("mainnet-beta")
        .await
        .expect("load Earn subscription targets");
    assert_eq!(
        targets.len(),
        4,
        "expected app, managed-vault, cross-mint, and Earn MAX targets"
    );
    assert!(targets.iter().all(|target| target.settings == "settings-a"));
    assert_eq!(targets.iter().filter(|target| target.earn_max).count(), 1);
    assert_eq!(
        targets
            .iter()
            .filter(|target| target.policy_accounts == vec!["cross-policy-a".to_owned()])
            .count(),
        1
    );
    let managed = targets
        .iter()
        .find(|target| target.vault_pubkey.as_deref() == Some("managed-vault-a"))
        .expect("managed-vault-only target");
    assert_eq!(managed.wallet, "wallet-a");
    assert_eq!(managed.vault_index, 2);
    assert_eq!(
        managed.policy_accounts,
        vec![
            "managed-active-policy-a".to_owned(),
            "managed-setup-policy-a".to_owned()
        ]
    );
    assert_eq!(managed.markets, vec!["managed-market-a".to_owned()]);
    assert_eq!(managed.observation_start_slot, Some(89));

    let unowned_targets = store
        .load_earn_subscription_targets("devnet")
        .await
        .expect("load empty app environment");
    assert!(unowned_targets.is_empty());
}
