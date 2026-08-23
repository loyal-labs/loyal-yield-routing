use loyal_yield_store::{
    AutodepositRecurringDelegationObserved, BalanceSweepTargetId, EarnReconciliationEnqueueInput,
    EarnReconciliationVaultInput, NeonSqlClient, NeonSqlConfig,
};
use serde_json::json;

const DATABASE_URL_ENV: &str = "ASK_2211_VERIFY_DATABASE_URL";
const TEST_ADVISORY_LOCK: i64 = 2_211_006_000;

async fn acquire_test_lock(client: &NeonSqlClient) -> sqlx::Transaction<'_, sqlx::Postgres> {
    let mut transaction = client.pool().begin().await.expect("begin test lock");
    sqlx::query("SELECT pg_advisory_xact_lock($1)")
        .bind(TEST_ADVISORY_LOCK)
        .execute(&mut *transaction)
        .await
        .expect("acquire test advisory lock");
    transaction
}

async fn insert_target(
    client: &NeonSqlClient,
    suffix: &str,
    policy_seed: i64,
) -> BalanceSweepTargetId {
    let id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.balance_sweep_targets
            (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
             wallet, wallet_usdc_ata, vault_usdc_ata, token_mint, wallet_token_ata,
             vault_token_ata, delegated_signers, threshold, max_amount_per_period,
             desired_active, chain_status, chain_observation_slot,
             wallet_balance_floor_raw, last_seen_slot, last_seen_signature, cluster)
        VALUES
            ($1,$2,$3,$4,1,$5,$2,$6,$7,$8,$6,$7,ARRAY[$9],1,10000000,
             TRUE,'pending',100,0,100,$10,'mainnet-beta')
        RETURNING id
        "#,
    )
    .bind(format!("settings-{suffix}"))
    .bind(format!("wallet-{suffix}"))
    .bind(policy_seed)
    .bind(format!("policy-{suffix}"))
    .bind(format!("vault-{suffix}"))
    .bind(format!("wallet-ata-{suffix}"))
    .bind(format!("vault-ata-{suffix}"))
    .bind(format!("mint-{suffix}"))
    .bind(format!("signer-{suffix}"))
    .bind(format!("policy-signature-{suffix}"))
    .fetch_one(client.pool())
    .await
    .expect("insert Autodeposit target");
    BalanceSweepTargetId(id)
}

#[tokio::test]
#[ignore = "requires ASK_2211_VERIFY_DATABASE_URL pointing at a throwaway database"]
async fn requests_coalesce_without_losing_newer_slots_or_cross_target_concurrency() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway database");
    let _test_lock = acquire_test_lock(&client).await;
    let first = insert_target(&client, "coalesced-first", 71).await;
    let second = insert_target(&client, "coalesced-second", 72).await;
    let consumer = "earn-smart-account:ask-2211-coalesced";

    for slot in [101_u64, 105] {
        client
            .enqueue_earn_reconciliation_jobs(EarnReconciliationEnqueueInput {
                consumer_name: consumer.to_owned(),
                event_key: format!("coalesced-{slot}"),
                durable_slot: slot,
                event_payload: json!({"kind": "account", "slot": slot}),
                vaults: vec![EarnReconciliationVaultInput {
                    settings: "settings-coalesced-first".to_owned(),
                    vault_index: 1,
                    vault_pubkey: "vault-coalesced-first".to_owned(),
                    vault_payload: json!({"vault": "first"}),
                }],
                autodeposit_target_ids: vec![first],
            })
            .await
            .expect("atomically enqueue Earn event and Autodeposit request");
    }

    let queue_state: (i64, i64, i64) = sqlx::query_as(
        r#"
        SELECT COUNT(*), MAX(requested_slot), MAX(processed_slot)
        FROM loyal_yield.autodeposit_reconciliation_requests
        WHERE target_id = $1
        "#,
    )
    .bind(first.as_i64())
    .fetch_one(client.pool())
    .await
    .unwrap();
    assert_eq!(queue_state, (1, 105, 0));
    let cursor: i64 = sqlx::query_scalar(
        "SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name = $1",
    )
    .bind(consumer)
    .fetch_one(client.pool())
    .await
    .unwrap();
    assert_eq!(cursor, 105);

    let first_claim = client
        .claim_autodeposit_reconciliation_request("worker-a", 120)
        .await
        .unwrap()
        .expect("first target should be ready");
    assert_eq!(first_claim.target_id, first);
    assert_eq!(first_claim.requested_slot, 105);
    client
        .enqueue_autodeposit_reconciliation_request(first, 110)
        .await
        .expect("raise high-water while claimed");
    assert!(client
        .claim_autodeposit_reconciliation_request("worker-b", 120)
        .await
        .unwrap()
        .is_none());

    let still_pending = client
        .complete_autodeposit_reconciliation_request(first, "worker-a", 106)
        .await
        .unwrap();
    assert!(still_pending);
    let second_claim_for_first = client
        .claim_autodeposit_reconciliation_request("worker-b", 120)
        .await
        .unwrap()
        .expect("newer slot must remain ready");
    assert_eq!(second_claim_for_first.target_id, first);
    assert_eq!(second_claim_for_first.requested_slot, 110);

    client
        .enqueue_autodeposit_reconciliation_request(second, 120)
        .await
        .unwrap();
    let independent_claim = client
        .claim_autodeposit_reconciliation_request("worker-c", 120)
        .await
        .unwrap()
        .expect("different target should be independently claimable");
    assert_eq!(independent_claim.target_id, second);
    client
        .complete_autodeposit_reconciliation_request(first, "worker-b", 110)
        .await
        .unwrap();
    client
        .complete_autodeposit_reconciliation_request(second, "worker-c", 120)
        .await
        .unwrap();

    let request_count: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id IN ($1, $2)",
    )
    .bind(first.as_i64())
    .bind(second.as_i64())
    .fetch_one(client.pool())
    .await
    .unwrap();
    assert_eq!(request_count, 2);
}

#[tokio::test]
#[ignore = "requires ASK_2211_VERIFY_DATABASE_URL pointing at a throwaway database"]
async fn recurring_delegation_discovery_schedules_the_first_snapshot_atomically() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway database");
    let _test_lock = acquire_test_lock(&client).await;
    let target = insert_target(&client, "discovery", 73).await;

    let returned = client
        .record_autodeposit_recurring_delegation(AutodepositRecurringDelegationObserved {
            wallet: "wallet-discovery".to_owned(),
            vault_pubkey: "vault-discovery".to_owned(),
            subscription_authority: "authority-discovery".to_owned(),
            recurring_delegation: "delegation-discovery".to_owned(),
            nonce: 4,
            amount_per_period: 10_000_000,
            period_length_seconds: 2_592_000,
            start_timestamp: 1,
            expiry_timestamp: 0,
            signature: "delegation-signature-discovery".to_owned(),
            slot: 130,
        })
        .await
        .expect("record recurring delegation and first snapshot request");
    assert_eq!(returned, target);

    let request: (i64, i64) = sqlx::query_as(
        "SELECT requested_slot, processed_slot FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id = $1",
    )
    .bind(target.as_i64())
    .fetch_one(client.pool())
    .await
    .unwrap();
    assert_eq!(request, (130, 0));

    let claimed = client
        .claim_autodeposit_reconciliation_request("discovery-worker", 120)
        .await
        .unwrap()
        .expect("discovery request should be ready");
    assert_eq!(claimed.target_id, target);
    client
        .complete_autodeposit_reconciliation_request(target, "discovery-worker", 130)
        .await
        .unwrap();
}
