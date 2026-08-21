use std::{
    env,
    time::{Duration, Instant},
};

use loyal_yield_store::{
    sqlx, sqlx::Row, EarnReconciliationEnqueueInput, EarnReconciliationVaultInput,
    OrchestratorConfig, OrchestratorStore,
};
use serde_json::json;

const DATABASE_URL_ENV: &str = "ASK_2200_TEST_DATABASE_URL";
const COMPLETED_HISTORY_JOBS: i64 = 100_000;
const ACTIVE_LOAD_JOBS: i64 = 250;
const SNAPSHOT_SAMPLES: usize = 25;
const SNAPSHOT_P95_BUDGET: Duration = Duration::from_millis(50);
const MAX_JOB_ROWS_READ_PER_SAMPLE: i64 = 2_000;

#[tokio::test]
#[ignore = "requires ASK_2200_TEST_DATABASE_URL pointing at a throwaway PostgreSQL database"]
async fn committed_health_snapshot_survives_restart_and_reports_stranded_work() {
    let database_url = env::var(DATABASE_URL_ENV).expect("test database URL must be set");
    let store = OrchestratorStore::connect(
        OrchestratorConfig::new(database_url.clone()).with_max_connections(1),
    )
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
    let restarted =
        OrchestratorStore::connect(OrchestratorConfig::new(database_url).with_max_connections(1))
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

    seed_loaded_job_history(&restarted, consumer_name).await;
    verify_loaded_snapshot_performance(&restarted, consumer_name).await;
}

async fn seed_loaded_job_history(store: &OrchestratorStore, consumer_name: &str) {
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.earn_reconciliation_jobs (
            consumer_name, event_key, durable_slot, settings, vault_index,
            vault_pubkey, event_payload, vault_payload, completed_at,
            created_at, updated_at
        )
        SELECT $1,
               'ask-2200-completed-' || series::TEXT,
               440700000 + series,
               'ask-2200-settings-' || (series % 1000)::TEXT,
               (series % 256)::SMALLINT,
               'ask-2200-completed-vault-' || (series % 1000)::TEXT,
               '{}'::JSONB,
               '{}'::JSONB,
               NOW(),
               NOW() - INTERVAL '1 day',
               NOW()
        FROM generate_series(1, $2::BIGINT) AS series
        "#,
    )
    .bind(consumer_name)
    .bind(COMPLETED_HISTORY_JOBS)
    .execute(store.pool())
    .await
    .expect("seed completed reconciliation history");

    sqlx::query(
        r#"
        INSERT INTO loyal_yield.earn_reconciliation_jobs (
            consumer_name, event_key, durable_slot, settings, vault_index,
            vault_pubkey, event_payload, vault_payload, last_error,
            created_at, updated_at
        )
        SELECT $1,
               'ask-2200-pending-' || series::TEXT,
               440800000 + series,
               'ask-2200-pending-settings-' || series::TEXT,
               (series % 256)::SMALLINT,
               'ask-2200-pending-vault-' || series::TEXT,
               '{}'::JSONB,
               '{}'::JSONB,
               CASE WHEN series % 10 = 0 THEN 'fixture proof failure' END,
               NOW() - INTERVAL '300 seconds',
               NOW()
        FROM generate_series(1, $2::BIGINT) AS series
        "#,
    )
    .bind(consumer_name)
    .bind(ACTIVE_LOAD_JOBS)
    .execute(store.pool())
    .await
    .expect("seed pending reconciliation load");

    sqlx::query("ANALYZE loyal_yield.earn_reconciliation_jobs")
        .execute(store.pool())
        .await
        .expect("analyze loaded reconciliation table");
}

async fn verify_loaded_snapshot_performance(store: &OrchestratorStore, consumer_name: &str) {
    let expected_pending = u64::try_from(ACTIVE_LOAD_JOBS).expect("active load fits u64") + 3;
    let expected_failed = u64::try_from(ACTIVE_LOAD_JOBS / 10).expect("failed load fits u64") + 1;

    let warm_snapshot = store
        .load_earn_reconciliation_health_snapshot(consumer_name)
        .await
        .expect("warm loaded health snapshot");
    assert_eq!(warm_snapshot.pending_jobs, expected_pending);
    assert_eq!(warm_snapshot.failed_pending_jobs, expected_failed);

    sqlx::query(
        "SELECT pg_stat_reset_single_table_counters(\
            'loyal_yield.earn_reconciliation_jobs'::REGCLASS)",
    )
    .execute(store.pool())
    .await
    .expect("reset reconciliation table statistics");

    let mut samples = Vec::with_capacity(SNAPSHOT_SAMPLES);
    for _ in 0..SNAPSHOT_SAMPLES {
        let started_at = Instant::now();
        let sampled = store
            .load_earn_reconciliation_health_snapshot(consumer_name)
            .await
            .expect("load health snapshot under history load");
        samples.push(started_at.elapsed());
        assert_eq!(sampled.pending_jobs, expected_pending);
        assert_eq!(sampled.failed_pending_jobs, expected_failed);
    }

    sqlx::query("SELECT pg_stat_force_next_flush()")
        .execute(store.pool())
        .await
        .expect("flush reconciliation table statistics");
    let read_stats = sqlx::query(
        r#"
        SELECT table_stats.seq_tup_read::BIGINT AS seq_tup_read,
               COALESCE(SUM(index_stats.idx_tup_read), 0)::BIGINT AS idx_tup_read
        FROM pg_stat_user_tables table_stats
        LEFT JOIN pg_stat_user_indexes index_stats
          ON index_stats.relid = table_stats.relid
        WHERE table_stats.schemaname = 'loyal_yield'
          AND table_stats.relname = 'earn_reconciliation_jobs'
        GROUP BY table_stats.seq_tup_read
        "#,
    )
    .fetch_one(store.pool())
    .await
    .expect("read reconciliation table statistics");
    let rows_read =
        read_stats.get::<i64, _>("seq_tup_read") + read_stats.get::<i64, _>("idx_tup_read");
    let rows_read_budget = MAX_JOB_ROWS_READ_PER_SAMPLE
        * i64::try_from(SNAPSHOT_SAMPLES).expect("sample count fits i64");

    samples.sort_unstable();
    let p95_index = ((SNAPSHOT_SAMPLES * 95).div_ceil(100)).saturating_sub(1);
    let p95 = samples[p95_index];
    let max = *samples.last().expect("snapshot samples are not empty");
    eprintln!(
        "ASK-2200 loaded snapshot: history={COMPLETED_HISTORY_JOBS}, pending={expected_pending}, samples={SNAPSHOT_SAMPLES}, p95_ms={:.3}, max_ms={:.3}, job_rows_read={rows_read}",
        p95.as_secs_f64() * 1000.0,
        max.as_secs_f64() * 1000.0,
    );

    assert!(
        p95 <= SNAPSHOT_P95_BUDGET,
        "health snapshot p95 {p95:?} exceeded {SNAPSHOT_P95_BUDGET:?}"
    );
    assert!(
        rows_read <= rows_read_budget,
        "health snapshots read {rows_read} job rows; budget is {rows_read_budget}, so completed history is affecting monitoring cost"
    );
}
