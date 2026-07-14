use std::{env, error::Error, time::Duration};

use chrono::Utc;
use loyal_yield_orchestrator::{
    complete_lookup_table_render_failure_delivery, enqueue_lookup_table_test_alerts,
    fail_lookup_table_alert_delivery, lease_lookup_table_alert_deliveries,
    lease_lookup_table_alert_deliveries_by_ids, load_lookup_table_alert_rules,
    load_lookup_table_alert_snapshot, lookup_table_alert_fingerprint,
    record_lookup_table_alert_observation,
    sqlx::{postgres::PgPoolOptions, Row},
    LookupTableAlertCondition, LookupTableAlertObservation, LookupTableAlertSeverity,
    LookupTableAlertThresholds, NeonSqlClient,
};
use serde_json::json;
use sha2::{Digest, Sha256};
use solana_sdk::{hash::hash, pubkey::Pubkey};

type TestResult<T> = Result<T, Box<dyn Error>>;

fn ordered_address_hash(addresses: &[String]) -> String {
    let mut hasher = Sha256::new();
    for address in addresses {
        hasher.update((address.len() as u64).to_le_bytes());
        hasher.update(address.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

#[tokio::test]
#[ignore = "requires an explicitly isolated disposable Postgres database"]
async fn durable_incident_and_outbox_lifecycle_is_idempotent() -> TestResult<()> {
    if env::var("REUSABLE_ALT_ALERT_DB_VERIFY_ISOLATED").as_deref() != Ok("1") {
        return Err("set REUSABLE_ALT_ALERT_DB_VERIFY_ISOLATED=1 only for a disposable DB".into());
    }
    let database_url = env::var("NEON_DATABASE_URL")?;
    let pool = PgPoolOptions::new()
        .max_connections(4)
        .connect(&database_url)
        .await?;
    let database_name: String =
        loyal_yield_orchestrator::sqlx::query_scalar("SELECT current_database()")
            .fetch_one(&pool)
            .await?;
    assert!(
        database_name.contains("reusable_alt"),
        "disposable DB name must contain reusable_alt"
    );
    let client = NeonSqlClient::from_pool(pool.clone());
    client
        .require_schema_migration(21, "reusable_alt_production_controls")
        .await?;

    let cluster = format!("alert-db-test-{}", Utc::now().timestamp_micros());
    let policy_pubkey = Pubkey::new_unique().to_string();
    let import_run_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.lookup_table_legacy_import_runs
            (cluster, rpc_genesis_hash, verified_slot, verified_at, legacy_kind,
             expected_table_count, verified_table_count, import_fingerprint,
             reason, updated_by)
        VALUES ($1, 'isolated-genesis', 10, now(), 'legacy_route', 2, 2, $2,
                'isolated cleanup alert regression', 'isolated-test')
        RETURNING id
        "#,
    )
    .bind(&cluster)
    .bind("a".repeat(64))
    .fetch_one(&pool)
    .await?;
    let retiring_table_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_lookup_tables
            (cluster, scope, table_address, authority, payer, status, durable,
             address_count, address_hash, addresses, last_extended_slot,
             last_extended_start_index, last_verified_slot, last_verified_at, legacy_kind,
             legacy_import_run_id, updated_at)
        VALUES ($1, 'legacy-retiring', $2, $3, $3, 'retiring', FALSE,
                0, $4, '[]'::jsonb, 1, 0, 10,
                (SELECT verified_at FROM loyal_yield.lookup_table_legacy_import_runs WHERE id = $5),
                'legacy_route', $5,
                now() - interval '2 seconds')
        RETURNING id
        "#,
    )
    .bind(&cluster)
    .bind(
        Pubkey::new_from_array(hash(format!("{cluster}:retiring").as_bytes()).to_bytes())
            .to_string(),
    )
    .bind(&policy_pubkey)
    .bind(ordered_address_hash(&Vec::<String>::new()))
    .bind(import_run_id)
    .fetch_one(&pool)
    .await?;
    let closed_without_refund_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_lookup_tables
            (cluster, scope, table_address, authority, payer, status, durable,
             address_count, address_hash, addresses, last_extended_slot,
             last_extended_start_index, last_verified_slot, last_verified_at, legacy_kind,
             legacy_import_run_id, deactivated_slot, deactivate_signature)
        VALUES ($1, 'legacy-closed', $2, $3, $3, 'closed', FALSE,
                0, $4, '[]'::jsonb, 1, 0, 10,
                (SELECT verified_at FROM loyal_yield.lookup_table_legacy_import_runs WHERE id = $5),
                'legacy_route', $5,
                11, 'deactivate-signature')
        RETURNING id
        "#,
    )
    .bind(&cluster)
    .bind(
        Pubkey::new_from_array(hash(format!("{cluster}:closed").as_bytes()).to_bytes()).to_string(),
    )
    .bind(&policy_pubkey)
    .bind(ordered_address_hash(&Vec::<String>::new()))
    .bind(import_run_id)
    .fetch_one(&pool)
    .await?;
    loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.lookup_table_legacy_import_evidence
            (import_run_id, route_lookup_table_id, table_address, scope,
             legacy_kind, expected_authority, observed_authority, observed_owner,
             observed_deactivation_slot, observed_last_extended_slot,
             observed_last_extended_start_index, address_count, address_hash,
             addresses, verified_slot, verified_at)
        SELECT import_run.id, route_table.id, route_table.table_address,
               route_table.scope, 'legacy_route', route_table.authority,
               route_table.authority,
               'AddressLookupTab1e1111111111111111111111111',
               '18446744073709551615', route_table.last_extended_slot,
               route_table.last_extended_start_index, route_table.address_count,
               route_table.address_hash, route_table.addresses,
               import_run.verified_slot, import_run.verified_at
        FROM loyal_yield.lookup_table_legacy_import_runs import_run
        JOIN loyal_yield.route_lookup_tables route_table
          ON route_table.id = ANY($2::BIGINT[])
        WHERE import_run.id = $1
        ORDER BY route_table.id
        "#,
    )
    .bind(import_run_id)
    .bind(vec![retiring_table_id, closed_without_refund_id])
    .execute(&pool)
    .await?;
    let imported_cleanup_fleet = client
        .imported_legacy_lookup_table_cleanup_fleet(&cluster)
        .await?;
    assert_eq!(imported_cleanup_fleet.len(), 2);
    assert!(imported_cleanup_fleet
        .iter()
        .any(|record| record.source.id == closed_without_refund_id
            && record.source.status == "closed"));
    let cleanup_snapshot = load_lookup_table_alert_snapshot(
        &pool,
        &cluster,
        &policy_pubkey,
        &LookupTableAlertThresholds {
            cleanup_grace: Duration::from_secs(1),
            ..LookupTableAlertThresholds::default()
        },
    )
    .await?;
    assert_eq!(cleanup_snapshot.cleanup_anomaly_count, 2);
    assert_eq!(
        cleanup_snapshot.cleanup_anomaly_table_ids,
        vec![retiring_table_id, closed_without_refund_id],
        "retired familyless imports or missing close/refund evidence disappeared from alerts"
    );
    let before = mutation_surface_counts(&pool).await?;
    let rules = load_lookup_table_alert_rules(&pool).await?;
    assert_eq!(rules.len(), 9);
    assert!(rules
        .iter()
        .all(|rule| rule.enabled && rule.rule_version == 1));
    let snapshot = load_lookup_table_alert_snapshot(
        &pool,
        &cluster,
        &policy_pubkey,
        &LookupTableAlertThresholds::default(),
    )
    .await?;
    assert_eq!(snapshot.shared_head_count, 0);
    assert_eq!(snapshot.healthy_shared_head_count, 0);
    assert_eq!(snapshot.fallback_use_count, 1);
    assert!(snapshot.physical_expectations.is_empty());
    let start = Utc::now();
    let active = observation(true, "first", 1);

    let opened = record_lookup_table_alert_observation(
        &pool,
        &cluster,
        &policy_pubkey,
        "cluster",
        &active,
        start,
        Duration::from_secs(3_600),
        3,
    )
    .await?;
    assert_eq!(opened.revision, Some(1));
    let incident_id = opened.incident_id.expect("open creates an incident");

    let repeated = record_lookup_table_alert_observation(
        &pool,
        &cluster,
        &policy_pubkey,
        "cluster",
        &active,
        Utc::now(),
        Duration::from_secs(3_600),
        3,
    )
    .await?;
    assert_eq!(repeated.incident_id, Some(incident_id));
    assert!(repeated.event_kind.is_none());

    let changed = observation(true, "changed", 2);
    let reminder = record_lookup_table_alert_observation(
        &pool,
        &cluster,
        &policy_pubkey,
        "cluster",
        &changed,
        Utc::now(),
        Duration::from_secs(3_600),
        3,
    )
    .await?;
    assert_eq!(reminder.incident_id, Some(incident_id));
    assert_eq!(reminder.revision, Some(2));

    let healthy = observation(false, "healthy", 0);
    let resolved = record_lookup_table_alert_observation(
        &pool,
        &cluster,
        &policy_pubkey,
        "cluster",
        &healthy,
        Utc::now(),
        Duration::from_secs(3_600),
        3,
    )
    .await?;
    assert_eq!(resolved.incident_id, Some(incident_id));
    assert_eq!(resolved.revision, Some(3));

    let repeated_healthy = record_lookup_table_alert_observation(
        &pool,
        &cluster,
        &policy_pubkey,
        "cluster",
        &healthy,
        Utc::now(),
        Duration::from_secs(3_600),
        3,
    )
    .await?;
    assert!(repeated_healthy.event_kind.is_none());

    let reopened = record_lookup_table_alert_observation(
        &pool,
        &cluster,
        &policy_pubkey,
        "cluster",
        &active,
        Utc::now(),
        Duration::from_secs(3_600),
        3,
    )
    .await?;
    assert_eq!(reopened.incident_id, Some(incident_id));
    assert_eq!(reopened.revision, Some(4));

    let incident = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT incident_status, revision, occurrence_count
        FROM loyal_yield.lookup_table_alert_incidents
        WHERE id = $1
        "#,
    )
    .bind(incident_id)
    .fetch_one(&pool)
    .await?;
    assert_eq!(incident.try_get::<String, _>("incident_status")?, "open");
    assert_eq!(incident.try_get::<i64, _>("revision")?, 4);
    assert_eq!(incident.try_get::<i64, _>("occurrence_count")?, 4);

    let deliveries = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT incident_revision, event_kind
        FROM loyal_yield.lookup_table_alert_deliveries
        WHERE incident_id = $1
        ORDER BY incident_revision
        "#,
    )
    .bind(incident_id)
    .fetch_all(&pool)
    .await?;
    assert_eq!(deliveries.len(), 4);
    assert_eq!(
        deliveries
            .iter()
            .map(|row| row.try_get::<String, _>("event_kind").unwrap())
            .collect::<Vec<_>>(),
        ["open", "reminder", "resolved", "open"]
    );

    let leased = lease_lookup_table_alert_deliveries(
        &pool,
        "isolated-alert-db-test",
        10,
        Duration::from_secs(60),
    )
    .await?;
    assert_eq!(leased.len(), 4);
    assert!(complete_lookup_table_render_failure_delivery(
        &pool,
        leased[0].id,
        leased[0].fencing_token + 1,
    )
    .await
    .is_err());
    complete_lookup_table_render_failure_delivery(&pool, leased[0].id, leased[0].fencing_token)
        .await?;
    for delivery in &leased[1..] {
        fail_lookup_table_alert_delivery(
            &pool,
            delivery,
            Duration::from_secs(60),
            "isolated expected retry",
            None,
        )
        .await?;
    }

    let unrelated_delivery_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.lookup_table_alert_deliveries
            (incident_id, incident_revision, alert_condition, event_kind,
             idempotency_key, cluster, policy_pubkey, payload, max_attempts)
        VALUES (NULL, NULL, 'missing_coverage', 'test', $1, $2, $3,
                '{"event":"test","condition":"missing_coverage","testId":"unrelated-backlog"}'::jsonb,
                3)
        RETURNING id
        "#,
    )
    .bind(format!("unrelated:{cluster}:{incident_id}"))
    .bind(&cluster)
    .bind(&policy_pubkey)
    .fetch_one(&pool)
    .await?;

    let test_id = format!("isolated-{incident_id}");
    let test_delivery_ids = enqueue_lookup_table_test_alerts(
        &pool,
        &cluster,
        &policy_pubkey,
        &test_id,
        &rules,
        3,
        Utc::now(),
    )
    .await?;
    assert_eq!(test_delivery_ids.len(), 9);
    let incident_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_alert_incidents WHERE cluster = $1",
    )
    .bind(&cluster)
    .fetch_one(&pool)
    .await?;
    assert_eq!(incident_count, 1, "test delivery created an incident");
    let test_delivery_conditions: Vec<String> = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT array_agg(alert_condition ORDER BY alert_condition)
        FROM loyal_yield.lookup_table_alert_deliveries
        WHERE event_kind = 'test' AND id = ANY($1)
        "#,
    )
    .bind(&test_delivery_ids)
    .fetch_one(&pool)
    .await?;
    let mut expected_conditions = LookupTableAlertCondition::ALL
        .map(LookupTableAlertCondition::as_str)
        .map(str::to_owned)
        .to_vec();
    expected_conditions.sort();
    assert_eq!(test_delivery_conditions, expected_conditions);
    let leased_tests = lease_lookup_table_alert_deliveries_by_ids(
        &pool,
        "isolated-all-rules-test",
        &test_delivery_ids,
        Duration::from_secs(60),
    )
    .await?;
    assert_eq!(leased_tests.len(), 9);
    for delivery in &leased_tests {
        assert_eq!(delivery.payload["event"], "test");
        assert!(delivery.payload["condition"].is_string());
        complete_lookup_table_render_failure_delivery(&pool, delivery.id, delivery.fencing_token)
            .await?;
    }
    let unrelated_delivery_state: String = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT delivery_state FROM loyal_yield.lookup_table_alert_deliveries WHERE id = $1",
    )
    .bind(unrelated_delivery_id)
    .fetch_one(&pool)
    .await?;
    assert_eq!(
        unrelated_delivery_state, "pending",
        "targeted safe test consumed unrelated production backlog"
    );
    let delivered_test_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_alert_deliveries
        WHERE id = ANY($1)
          AND delivery_state = 'delivered'
          AND delivered_via = 'render_failure'
          AND incident_id IS NULL
        "#,
    )
    .bind(&test_delivery_ids)
    .fetch_one(&pool)
    .await?;
    assert_eq!(delivered_test_count, 9);

    let concurrent_cluster = format!("{cluster}-concurrent");
    let concurrent_observed_at = Utc::now();
    let (left, right) = tokio::join!(
        record_lookup_table_alert_observation(
            &pool,
            &concurrent_cluster,
            &policy_pubkey,
            "cluster",
            &active,
            concurrent_observed_at,
            Duration::from_secs(3_600),
            3,
        ),
        record_lookup_table_alert_observation(
            &pool,
            &concurrent_cluster,
            &policy_pubkey,
            "cluster",
            &active,
            concurrent_observed_at,
            Duration::from_secs(3_600),
            3,
        )
    );
    let concurrent_transitions = [left?, right?];
    assert_eq!(
        concurrent_transitions
            .iter()
            .filter(|transition| transition.event_kind.is_some())
            .count(),
        1,
        "concurrent identical evidence enqueued duplicate open transitions"
    );
    let concurrent_state = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT incident.occurrence_count,
               count(delivery.id) AS delivery_count
        FROM loyal_yield.lookup_table_alert_incidents incident
        LEFT JOIN loyal_yield.lookup_table_alert_deliveries delivery
          ON delivery.incident_id = incident.id
        WHERE incident.cluster = $1
        GROUP BY incident.id, incident.occurrence_count
        "#,
    )
    .bind(&concurrent_cluster)
    .fetch_one(&pool)
    .await?;
    assert_eq!(concurrent_state.try_get::<i64, _>("occurrence_count")?, 2);
    assert_eq!(concurrent_state.try_get::<i64, _>("delivery_count")?, 1);

    let invalid_condition = loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.lookup_table_alert_incidents
            (cluster, policy_pubkey, alert_condition, scope_key, incident_status,
             severity, fingerprint, summary, details, first_observed_at,
             opened_at, last_observed_at, last_notified_at, occurrence_count,
             revision)
        SELECT cluster, policy_pubkey, 'not_a_contract_condition', 'invalid',
               incident_status, severity, fingerprint, summary, details,
               first_observed_at, opened_at, last_observed_at, last_notified_at,
               occurrence_count, revision
        FROM loyal_yield.lookup_table_alert_incidents WHERE id = $1
        "#,
    )
    .bind(incident_id)
    .execute(&pool)
    .await;
    assert!(invalid_condition.is_err());
    let cross_condition_delivery = loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.lookup_table_alert_deliveries
            (incident_id, incident_revision, alert_condition, event_kind,
             idempotency_key, cluster, policy_pubkey, payload, max_attempts)
        VALUES ($1, 999, 'fallback_use', 'open', $2, $3, $4, '{}'::jsonb, 3)
        "#,
    )
    .bind(incident_id)
    .bind(format!("cross-condition:{incident_id}"))
    .bind(&cluster)
    .bind(&policy_pubkey)
    .execute(&pool)
    .await;
    assert!(
        cross_condition_delivery.is_err(),
        "delivery condition escaped its incident's durable rule identity"
    );
    assert!(
        loyal_yield_orchestrator::sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_alert_rules
            SET description = description || ' invalid-unversioned-change'
            WHERE rule_key = 'missing_coverage'
            "#,
        )
        .execute(&pool)
        .await
        .is_err(),
        "rule configuration changed without advancing its durable version"
    );
    assert!(
        loyal_yield_orchestrator::sqlx::query(
            "DELETE FROM loyal_yield.lookup_table_alert_rules WHERE rule_key = 'missing_coverage'",
        )
        .execute(&pool)
        .await
        .is_err(),
        "durable alert rule was deleted"
    );
    assert!(loyal_yield_orchestrator::sqlx::query(
        "DELETE FROM loyal_yield.lookup_table_alert_incidents WHERE id = $1",
    )
    .bind(incident_id)
    .execute(&pool)
    .await
    .is_err());

    let after = mutation_surface_counts(&pool).await?;
    assert_eq!(
        before, after,
        "alert verification mutated route demand, ALT operations, decisions, or physical tables"
    );
    pool.close().await;
    Ok(())
}

fn observation(active: bool, label: &str, count: i64) -> LookupTableAlertObservation {
    let details = json!({"label": label, "count": count});
    LookupTableAlertObservation {
        condition: LookupTableAlertCondition::MissingCoverage,
        active,
        severity: if active {
            LookupTableAlertSeverity::Warning
        } else {
            LookupTableAlertSeverity::Info
        },
        fingerprint: lookup_table_alert_fingerprint(
            LookupTableAlertCondition::MissingCoverage,
            &details,
        ),
        summary: format!("isolated alert DB test {label}"),
        details,
    }
}

async fn mutation_surface_counts(
    pool: &loyal_yield_orchestrator::sqlx::PgPool,
) -> TestResult<(i64, i64, i64, i64)> {
    Ok((
        loyal_yield_orchestrator::sqlx::query_scalar(
            "SELECT count(*) FROM loyal_yield.lookup_table_provisioning_requests",
        )
        .fetch_one(pool)
        .await?,
        loyal_yield_orchestrator::sqlx::query_scalar(
            "SELECT count(*) FROM loyal_yield.lookup_table_operations",
        )
        .fetch_one(pool)
        .await?,
        loyal_yield_orchestrator::sqlx::query_scalar(
            "SELECT count(*) FROM loyal_yield.rebalance_decisions",
        )
        .fetch_one(pool)
        .await?,
        loyal_yield_orchestrator::sqlx::query_scalar(
            "SELECT count(*) FROM loyal_yield.route_lookup_tables",
        )
        .fetch_one(pool)
        .await?,
    ))
}
