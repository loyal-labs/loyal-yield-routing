use std::{env, time::Duration};

use chrono::{Duration as ChronoDuration, Utc};
use loyal_yield_store::{
    fleet_orchestration::FleetHealthSnapshotProjection, sqlx, NeonSqlClient, NeonSqlConfig,
};

const LOCK_SQL: &str = r#"
    SELECT pg_advisory_xact_lock(
        hashtextextended('fleet-health-projector:' || $1, 0)
    )
"#;

fn database_url() -> String {
    let value = env::var("FLEET_VERIFY_DATABASE_URL")
        .expect("FLEET_VERIFY_DATABASE_URL must point at a disposable local database");
    assert!(
        value.contains("fleet_verify"),
        "refusing to run: database name must contain fleet_verify"
    );
    value
}

async fn client() -> NeonSqlClient {
    NeonSqlClient::connect(
        NeonSqlConfig::new(database_url())
            .with_max_connections(8)
            .with_acquire_timeout(Duration::from_secs(5)),
    )
    .await
    .expect("connect to isolated verifier database")
}

fn cluster(suffix: &str) -> String {
    format!(
        "fleet_verify_ask_2150_{}_{}_{}",
        std::process::id(),
        Utc::now().timestamp_nanos_opt().unwrap_or_default(),
        suffix
    )
}

async fn register_cluster(client: &NeonSqlClient, cluster: &str) {
    client
        .register_fleet_planning_cluster(cluster)
        .await
        .expect("register isolated cluster");
    client
        .heartbeat_fleet_planning_cluster(cluster)
        .await
        .expect("heartbeat isolated cluster");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn projection_is_transactional_rollout_safe_and_lease_independent() {
    let client = client().await;
    let release_cluster = cluster("release");
    register_cluster(&client, &release_cluster).await;

    sqlx::query(
        r#"
        INSERT INTO loyal_yield.fleet_health_projection_leases
            (cluster, owner, fencing_token, lease_expires_at)
        VALUES ($1, 'adversarial-old-projector', 99, now() + interval '1 day')
        "#,
    )
    .bind(&release_cluster)
    .execute(client.pool())
    .await
    .expect("seed adversarial legacy lease");

    let mut guard = client.pool().begin().await.expect("begin lock guard");
    sqlx::query(LOCK_SQL)
        .bind(&release_cluster)
        .execute(&mut *guard)
        .await
        .expect("hold projector advisory lock");

    let busy = client
        .project_fleet_orchestration_health_snapshot(&release_cluster, ChronoDuration::seconds(60))
        .await
        .expect("lock contention is a normal outcome");
    assert!(matches!(busy, FleetHealthSnapshotProjection::Busy));

    guard.rollback().await.expect("release lock by rollback");
    let published = client
        .project_fleet_orchestration_health_snapshot(&release_cluster, ChronoDuration::seconds(60))
        .await
        .expect("publish immediately after rollback");
    let published = match published {
        FleetHealthSnapshotProjection::Published(refresh) => refresh,
        other => panic!("expected Published after lock release, got {other:?}"),
    };
    assert_eq!(published.cluster, release_cluster);

    let legacy_lease: (String, i64) = sqlx::query_as(
        "SELECT owner, fencing_token FROM loyal_yield.fleet_health_projection_leases WHERE cluster = $1",
    )
    .bind(&release_cluster)
    .fetch_one(client.pool())
    .await
    .expect("read untouched legacy lease");
    assert_eq!(legacy_lease, ("adversarial-old-projector".to_owned(), 99));

    let before_not_due: (chrono::DateTime<Utc>, i64) = sqlx::query_as(
        "SELECT refreshed_at, fencing_token FROM loyal_yield.fleet_orchestration_health_snapshots WHERE cluster = $1",
    )
    .bind(&release_cluster)
    .fetch_one(client.pool())
    .await
    .expect("read initial snapshot");
    let not_due = client
        .project_fleet_orchestration_health_snapshot(&release_cluster, ChronoDuration::seconds(60))
        .await
        .expect("freshness gate");
    assert!(matches!(
        not_due,
        FleetHealthSnapshotProjection::NotDue { .. }
    ));
    let after_not_due: (chrono::DateTime<Utc>, i64) = sqlx::query_as(
        "SELECT refreshed_at, fencing_token FROM loyal_yield.fleet_orchestration_health_snapshots WHERE cluster = $1",
    )
    .bind(&release_cluster)
    .fetch_one(client.pool())
    .await
    .expect("read snapshot after NotDue");
    assert_eq!(before_not_due, after_not_due, "NotDue must not rewrite");

    let concurrent_cluster = cluster("concurrent");
    register_cluster(&client, &concurrent_cluster).await;
    let first_client = client.clone();
    let first_cluster = concurrent_cluster.clone();
    let first = tokio::spawn(async move {
        first_client
            .project_fleet_orchestration_health_snapshot(
                &first_cluster,
                ChronoDuration::seconds(60),
            )
            .await
    });
    let second_client = client.clone();
    let second_cluster = concurrent_cluster.clone();
    let second = tokio::spawn(async move {
        second_client
            .project_fleet_orchestration_health_snapshot(
                &second_cluster,
                ChronoDuration::seconds(60),
            )
            .await
    });
    let outcomes = [
        first.await.expect("first task").expect("first refresh"),
        second.await.expect("second task").expect("second refresh"),
    ];
    assert_eq!(
        outcomes
            .iter()
            .filter(|outcome| matches!(outcome, FleetHealthSnapshotProjection::Published(_)))
            .count(),
        1,
        "concurrent refreshes must publish at most once"
    );
    assert_eq!(
        sqlx::query_scalar::<_, i64>(
            "SELECT count(*) FROM loyal_yield.fleet_orchestration_health_snapshots WHERE cluster = $1",
        )
        .bind(&concurrent_cluster)
        .fetch_one(client.pool())
        .await
        .expect("count concurrent snapshots"),
        1
    );

    let rollback_cluster = cluster("rollback");
    register_cluster(&client, &rollback_cluster).await;
    let initial = client
        .project_fleet_orchestration_health_snapshot(&rollback_cluster, ChronoDuration::seconds(60))
        .await
        .expect("seed rollback snapshot");
    assert!(matches!(
        initial,
        FleetHealthSnapshotProjection::Published(_)
    ));
    sqlx::query(
        r#"
        UPDATE loyal_yield.fleet_orchestration_health_snapshots
        SET refresh_started_at = now() - interval '1 hour 1 second',
            refreshed_at = now() - interval '1 hour'
        WHERE cluster = $1
        "#,
    )
    .bind(&rollback_cluster)
    .execute(client.pool())
    .await
    .expect("age rollback snapshot");
    let before_error: (serde_json::Value, serde_json::Value, chrono::DateTime<Utc>, i64) =
        sqlx::query_as(
            "SELECT payload, source_watermark, refreshed_at, fencing_token FROM loyal_yield.fleet_orchestration_health_snapshots WHERE cluster = $1",
        )
        .bind(&rollback_cluster)
        .fetch_one(client.pool())
        .await
        .expect("read snapshot before forced error");

    sqlx::raw_sql(
        r#"
        CREATE OR REPLACE FUNCTION loyal_yield.fail_ask_2150_snapshot_write()
        RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN
            IF NEW.cluster LIKE 'fleet_verify_ask_2150_%_rollback' THEN
                RAISE EXCEPTION 'ASK-2150 forced snapshot publication failure';
            END IF;
            RETURN NEW;
        END;
        $$;
        CREATE TRIGGER fail_ask_2150_snapshot_write
        BEFORE INSERT OR UPDATE ON loyal_yield.fleet_orchestration_health_snapshots
        FOR EACH ROW EXECUTE FUNCTION loyal_yield.fail_ask_2150_snapshot_write();
        "#,
    )
    .execute(client.pool())
    .await
    .expect("install forced failure trigger");
    let failed = client
        .project_fleet_orchestration_health_snapshot(&rollback_cluster, ChronoDuration::seconds(1))
        .await;
    assert!(
        failed.is_err(),
        "forced database error must escape as an error"
    );
    sqlx::raw_sql(
        r#"
        DROP TRIGGER fail_ask_2150_snapshot_write
            ON loyal_yield.fleet_orchestration_health_snapshots;
        DROP FUNCTION loyal_yield.fail_ask_2150_snapshot_write();
        "#,
    )
    .execute(client.pool())
    .await
    .expect("remove forced failure trigger");
    let after_error: (serde_json::Value, serde_json::Value, chrono::DateTime<Utc>, i64) =
        sqlx::query_as(
            "SELECT payload, source_watermark, refreshed_at, fencing_token FROM loyal_yield.fleet_orchestration_health_snapshots WHERE cluster = $1",
        )
        .bind(&rollback_cluster)
        .fetch_one(client.pool())
        .await
        .expect("read snapshot after forced error");
    assert_eq!(before_error, after_error, "failed refresh must roll back");
}
