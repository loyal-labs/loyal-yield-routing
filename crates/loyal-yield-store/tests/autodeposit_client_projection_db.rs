use loyal_yield_store::{
    AutodepositProjectionStatus, AutodepositSnapshotInput, AutodepositVaultConfigInput,
    NeonSqlClient, NeonSqlConfig,
};

const DATABASE_URL_ENV: &str = "ASK_2211_VERIFY_DATABASE_URL";

fn config(desired_active: bool, floor: u64) -> AutodepositVaultConfigInput {
    AutodepositVaultConfigInput {
        cluster: "mainnet-beta".to_owned(),
        settings: "settings-ask-2211".to_owned(),
        wallet: "wallet-ask-2211".to_owned(),
        vault_index: 1,
        vault_pubkey: "vault-ask-2211".to_owned(),
        desired_active,
        wallet_balance_floor_raw: floor,
        expected_policy_account: "policy-ask-2211".to_owned(),
        expected_subscription_authority: "authority-ask-2211".to_owned(),
        expected_recurring_delegation: "delegation-ask-2211".to_owned(),
        observation_start_slot: 100,
    }
}

fn snapshot(
    config_id: i64,
    slot: u64,
    complete: bool,
    values: [bool; 4],
) -> AutodepositSnapshotInput {
    AutodepositSnapshotInput {
        config_id,
        observation_slot: slot,
        observation_complete: complete,
        policy_valid: values[0],
        subscription_authority_valid: values[1],
        recurring_delegation_valid: values[2],
        token_delegate_valid: values[3],
        reason: None,
    }
}

#[tokio::test]
#[ignore = "requires ASK_2211_VERIFY_DATABASE_URL pointing at a throwaway database"]
async fn autodeposit_client_projection_is_monotonic_and_intent_gated() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway database");
    let database_name: String = sqlx::query_scalar("SELECT current_database()")
        .fetch_one(client.pool())
        .await
        .expect("read test database name");
    assert!(database_name.contains("ask_2211_autodeposit"));

    let configured = client
        .upsert_autodeposit_vault_config(config(true, 5_000_000))
        .await
        .expect("store authenticated client intent before broadcast");
    assert_eq!(configured.generation, 1);

    let pending = client
        .reconcile_autodeposit_snapshot(snapshot(configured.id, 100, false, [false; 4]))
        .await
        .expect("record incomplete initial observation");
    assert_eq!(pending.status, AutodepositProjectionStatus::Pending);

    let active = client
        .reconcile_autodeposit_snapshot(snapshot(configured.id, 101, true, [true; 4]))
        .await
        .expect("project complete on-chain installation");
    assert_eq!(active.status, AutodepositProjectionStatus::Active);
    assert_eq!(active.bootstrap_generation, Some(1));
    assert!(client
        .effective_autodeposit_active(configured.id)
        .await
        .unwrap());

    let stale = client
        .reconcile_autodeposit_snapshot(snapshot(configured.id, 99, true, [false; 4]))
        .await
        .expect("ignore stale close observation");
    assert_eq!(stale.status, AutodepositProjectionStatus::Active);
    assert_eq!(stale.observation_slot, 101);

    let paused = client
        .upsert_autodeposit_vault_config(config(false, 5_000_000))
        .await
        .expect("pause remains an off-chain control");
    assert_eq!(paused.generation, 2);
    assert!(!client
        .effective_autodeposit_active(configured.id)
        .await
        .unwrap());

    let floor_changed = client
        .upsert_autodeposit_vault_config(config(false, 7_000_000))
        .await
        .expect("floor remains mutable off-chain configuration");
    assert_eq!(floor_changed.generation, 3);

    let inconsistent = client
        .reconcile_autodeposit_snapshot(snapshot(
            configured.id,
            102,
            true,
            [true, true, false, true],
        ))
        .await
        .expect("project partial installation");
    assert_eq!(
        inconsistent.status,
        AutodepositProjectionStatus::Inconsistent
    );

    let closed = client
        .reconcile_autodeposit_snapshot(snapshot(configured.id, 103, true, [false; 4]))
        .await
        .expect("project objective chain closure");
    assert_eq!(closed.status, AutodepositProjectionStatus::Closed);
    assert!(!client
        .effective_autodeposit_active(configured.id)
        .await
        .unwrap());

    let statuses: Vec<String> = sqlx::query_scalar(
        "SELECT reason FROM loyal_yield.realtime_events WHERE event_type = 'earn.autodeposit.changed' ORDER BY id",
    )
    .fetch_all(client.pool())
    .await
    .expect("load SSE invalidations");
    assert_eq!(
        statuses,
        vec!["pending", "active", "inconsistent", "closed"]
    );
}
