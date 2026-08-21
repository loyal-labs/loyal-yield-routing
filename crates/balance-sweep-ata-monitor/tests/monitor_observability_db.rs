use std::env;

use loyal_yield_store::{
    sqlx, EarnReconciliationEnqueueInput, EarnReconciliationVaultInput, OrchestratorConfig,
    OrchestratorStore,
};
use serde_json::json;

const DATABASE_URL_ENV: &str = "ASK_2200_TEST_DATABASE_URL";

#[tokio::test]
#[ignore = "requires ASK_2200_TEST_DATABASE_URL pointing at a throwaway PostgreSQL database"]
async fn committed_health_snapshot_survives_restart_and_reports_stranded_work() {
    let database_url = env::var(DATABASE_URL_ENV).expect("test database URL must be set");
    let store = OrchestratorStore::connect(OrchestratorConfig::new(database_url.clone()))
        .await
        .expect("connect to throwaway database");
    sqlx::raw_sql(
        r#"
        CREATE SCHEMA loyal_yield;
        "#,
    )
    .execute(store.pool())
    .await
    .expect("create loyal_yield schema");
    sqlx::raw_sql(include_str!(
        "../../loyal-yield-store/migrations/0046_laserstream_replay_cursor.sql"
    ))
    .execute(store.pool())
    .await
    .expect("apply replay cursor migration");
    sqlx::raw_sql(include_str!(
        "../../loyal-yield-store/migrations/0049_durable_earn_reconciliation_jobs.sql"
    ))
    .execute(store.pool())
    .await
    .expect("apply durable Earn jobs migration");

    let consumer_name = "earn-smart-account:ask-2200";
    store
        .enqueue_earn_reconciliation_jobs(EarnReconciliationEnqueueInput {
            consumer_name: consumer_name.to_owned(),
            event_key: "ask-2200-event".to_owned(),
            durable_slot: 440_700_000,
            event_payload: json!({"kind": "account_update"}),
            vaults: (1_u8..=3)
                .map(|vault_index| EarnReconciliationVaultInput {
                    settings: "ask-2200-settings".to_owned(),
                    vault_index,
                    vault_pubkey: format!("ask-2200-vault-{vault_index}"),
                    vault_payload: json!({"vault_index": vault_index}),
                })
                .collect(),
        })
        .await
        .expect("enqueue durable jobs and cursor");

    let claimed = store
        .claim_earn_reconciliation_job(consumer_name, "ask-2200-claim", 120)
        .await
        .expect("claim first job")
        .expect("first job should be ready");
    store
        .retry_earn_reconciliation_job(claimed.id, "ask-2200-claim", "fixture proof failure", 15)
        .await
        .expect("retain failed job for retry");
    sqlx::query(
        r#"
        UPDATE loyal_yield.earn_reconciliation_jobs
        SET created_at = NOW() - INTERVAL '120 seconds'
        WHERE consumer_name = $1
        "#,
    )
    .bind(consumer_name)
    .execute(store.pool())
    .await
    .expect("age fixture jobs");

    let snapshot = store
        .load_earn_reconciliation_health_snapshot(consumer_name)
        .await
        .expect("load committed health snapshot");
    assert_eq!(snapshot.cursor_slot, 440_700_000);
    assert_eq!(snapshot.pending_jobs, 3);
    assert_eq!(snapshot.failed_pending_jobs, 1);
    assert!(snapshot.oldest_pending_age_seconds >= 120);

    drop(store);
    let restarted = OrchestratorStore::connect(OrchestratorConfig::new(database_url))
        .await
        .expect("reconnect after simulated process restart");
    let after_restart = restarted
        .load_earn_reconciliation_health_snapshot(consumer_name)
        .await
        .expect("reload health snapshot after restart");
    assert_eq!(after_restart.cursor_slot, snapshot.cursor_slot);
    assert_eq!(after_restart.pending_jobs, snapshot.pending_jobs);
    assert_eq!(
        after_restart.failed_pending_jobs,
        snapshot.failed_pending_jobs
    );
    assert!(
        after_restart.oldest_pending_age_seconds >= snapshot.oldest_pending_age_seconds,
        "oldest pending age must come from durable timestamps"
    );
}
