use std::{env, error::Error, sync::Arc};

use chrono::{Duration, Utc};
use loyal_yield_orchestrator::{
    sqlx::postgres::PgPoolOptions, LegacyLookupTableCleanupAttemptPrepare,
    LegacyLookupTableCleanupAttemptState, LookupTableAllocationKind,
    LookupTableClusterBudgetPolicy, LookupTableFamilyKind, LookupTableFamilyState,
    LookupTableFamilyUpsert, LookupTableLifecycle, LookupTableOperationEnqueue,
    LookupTableOperationKind, LookupTableOperationLease, NeonSqlClient, ReusableLookupTableInsert,
    SignedLegacyLookupTableCleanupAttempt,
};
use serde_json::json;
use sha2::{Digest, Sha256};
use solana_sdk::pubkey::Pubkey;
use tokio::sync::Barrier;

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
async fn cleanup_budget_and_crash_fences_share_v2_cluster_accounting() -> TestResult<()> {
    if env::var("REUSABLE_ALT_CLEANUP_DB_VERIFY_ISOLATED").as_deref() != Ok("1") {
        return Err(
            "set REUSABLE_ALT_CLEANUP_DB_VERIFY_ISOLATED=1 only for a disposable DB".into(),
        );
    }
    let database_url = env::var("NEON_DATABASE_URL")?;
    let pool = PgPoolOptions::new()
        .max_connections(8)
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

    let run = Utc::now().timestamp_micros();
    let cluster = format!("cleanup-budget-db-test-{run}");
    let authority = Pubkey::new_unique().to_string();
    let empty_addresses = Vec::<String>::new();
    let empty_hash = ordered_address_hash(&empty_addresses);
    let import_fingerprint = ordered_address_hash(&[format!("cleanup-import-{run}")]);
    let import_run_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.lookup_table_legacy_import_runs
            (cluster, rpc_genesis_hash, verified_slot, verified_at, legacy_kind,
             expected_table_count, verified_table_count, import_fingerprint,
             reason, updated_by)
        VALUES ($1, 'isolated-genesis', 10, now(), 'legacy_route', 1, 1, $2,
                'isolated cleanup budget regression', 'isolated-test')
        RETURNING id
        "#,
    )
    .bind(&cluster)
    .bind(&import_fingerprint)
    .fetch_one(&pool)
    .await?;
    let legacy_table = Pubkey::new_unique().to_string();
    let legacy_table_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_lookup_tables
            (cluster, scope, table_address, authority, payer, status, durable,
             address_count, address_hash, addresses, last_extended_slot,
             last_extended_start_index, last_verified_slot, last_verified_at,
             legacy_kind, legacy_import_run_id)
        VALUES ($1, 'legacy-cleanup-budget', $2, $3, $3, 'retiring', FALSE,
                0, $4, '[]'::jsonb, 1, 0, 10,
                (SELECT verified_at FROM loyal_yield.lookup_table_legacy_import_runs WHERE id = $5),
                'legacy_route', $5)
        RETURNING id
        "#,
    )
    .bind(&cluster)
    .bind(&legacy_table)
    .bind(&authority)
    .bind(&empty_hash)
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
        JOIN loyal_yield.route_lookup_tables route_table ON route_table.id = $2
        WHERE import_run.id = $1
        "#,
    )
    .bind(import_run_id)
    .bind(legacy_table_id)
    .execute(&pool)
    .await?;
    loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.lookup_table_rollout_controls
            (cluster, vault_id, rollout_mode, force_legacy, reason, updated_by)
        VALUES ($1, NULL, 'reusable_only', FALSE,
                'isolated cleanup regression', 'isolated-test')
        "#,
    )
    .bind(&cluster)
    .execute(&pool)
    .await?;
    let protection = client
        .legacy_lookup_table_cleanup_protection(&cluster, &legacy_table)
        .await?
        .expect("imported cleanup protection exists");
    assert!(protection.can_deactivate);
    let prepare = LegacyLookupTableCleanupAttemptPrepare {
        cluster: cluster.clone(),
        table_address: legacy_table.clone(),
        expected_authorization_token: protection.authorization_token,
        operation_kind: LookupTableOperationKind::Deactivate,
        expected_authority: authority.clone(),
        expected_address_count: 0,
        expected_address_hash: empty_hash,
        close_recipient: None,
        expected_reclaimed_lamports: None,
    };
    let attempt = client
        .prepare_legacy_lookup_table_cleanup_attempt(prepare.clone())
        .await?;

    let unsigned_without_budget = client
        .persist_signed_legacy_lookup_table_cleanup_attempt(
            attempt.id,
            SignedLegacyLookupTableCleanupAttempt {
                transaction_signature: format!("cleanup-no-budget-{run}"),
                message_hash: "a".repeat(64),
                recent_blockhash: format!("blockhash-no-budget-{run}"),
                last_valid_block_height: 100,
                estimated_fee_lamports: 60,
                recipient_balance_before: None,
            },
        )
        .await;
    assert!(
        unsigned_without_budget.is_err(),
        "database allowed signing metadata without a durable budget reservation"
    );

    let family = client
        .create_or_validate_lookup_table_family(LookupTableFamilyUpsert {
            cluster: cluster.clone(),
            logical_name: format!("cleanup-budget-v2-{run}"),
            kind: LookupTableFamilyKind::VaultShards,
            desired_state: LookupTableFamilyState::Active,
            planner_version: "cleanup-db-test-v1".to_owned(),
            catalog_version: "cleanup-db-test-v1".to_owned(),
            active_generation: Some(0),
            previous_generation: None,
            rollback_until: None,
            provisioning_authority: authority.clone(),
            payer: authority.clone(),
            hard_capacity: 64,
            largest_atomic_expansion: 8,
            safety_margin: 4,
            allocation_high_water: 52,
        })
        .await?;
    let v2_table = client
        .insert_reusable_lookup_table(ReusableLookupTableInsert {
            cluster: cluster.clone(),
            scope: format!("cleanup-budget-v2-{run}"),
            table_address: Pubkey::new_unique().to_string(),
            authority: authority.clone(),
            payer: authority.clone(),
            family_id: family.id,
            allocation_kind: LookupTableAllocationKind::VaultShard,
            generation: 0,
            shard_ordinal: 0,
            desired_state: LookupTableLifecycle::Active,
            accepting_allocations: true,
            allocation_high_water: 52,
            mutation_epoch: 0,
            create_signature: None,
        })
        .await?;
    client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("cleanup-budget-v2-operation-{run}"),
            family_id: family.id,
            route_lookup_table_id: Some(v2_table.id),
            manifest_id: None,
            binding_id: None,
            operation_kind: LookupTableOperationKind::Extend,
            target_generation: None,
            target_shard_ordinal: None,
            operation_context: json!({"source": "cleanup_budget_db_test"}),
            mutation_epoch: v2_table.mutation_epoch,
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            addresses: vec![Pubkey::new_unique().to_string()],
        })
        .await?;
    let leased = client
        .lease_next_lookup_table_operation(
            &cluster,
            "cleanup-budget-v2-owner",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .expect("v2 operation is leaseable");
    let lease = LookupTableOperationLease::new(
        leased
            .operation
            .lease_owner
            .clone()
            .expect("leased operation owner"),
        leased.operation.fencing_token,
        leased
            .operation
            .lease_expires_at
            .expect("leased operation expiry"),
    )?;
    let policy = LookupTableClusterBudgetPolicy {
        max_lamports: 100,
        rolling_window_seconds: 600,
    };
    let barrier = Arc::new(Barrier::new(3));
    let v2_task = {
        let client = client.clone();
        let cluster = cluster.clone();
        let lease = lease.clone();
        let barrier = barrier.clone();
        let policy = policy.clone();
        let operation_id = leased.operation.id;
        tokio::spawn(async move {
            barrier.wait().await;
            client
                .reserve_lookup_table_cluster_budget(&cluster, operation_id, &lease, policy, 60, 0)
                .await
        })
    };
    let cleanup_task = {
        let client = client.clone();
        let cluster = cluster.clone();
        let barrier = barrier.clone();
        let policy = policy.clone();
        tokio::spawn(async move {
            barrier.wait().await;
            client
                .reserve_legacy_lookup_table_cleanup_budget(&cluster, attempt.id, policy, 60, 0)
                .await
        })
    };
    barrier.wait().await;
    let v2_result = v2_task.await??;
    let cleanup_result = cleanup_task.await??;
    assert!(
        v2_result.approved ^ cleanup_result.approved,
        "shared v2/legacy cluster budget did not serialize to exactly one winner"
    );

    let expanded_policy = LookupTableClusterBudgetPolicy {
        max_lamports: 120,
        rolling_window_seconds: 600,
    };
    let cleanup_reservation = if cleanup_result.approved {
        cleanup_result
    } else {
        client
            .reserve_legacy_lookup_table_cleanup_budget(
                &cluster,
                attempt.id,
                expanded_policy.clone(),
                60,
                0,
            )
            .await?
    };
    if !v2_result.approved {
        client
            .reserve_lookup_table_cluster_budget(
                &cluster,
                leased.operation.id,
                &lease,
                expanded_policy.clone(),
                60,
                0,
            )
            .await?;
    }
    assert!(cleanup_reservation.approved);
    let replay = client
        .reserve_legacy_lookup_table_cleanup_budget(&cluster, attempt.id, expanded_policy, 60, 0)
        .await?;
    assert!(replay.approved && replay.replayed);
    assert_eq!(replay.reservation_id, cleanup_reservation.reservation_id);
    assert_eq!(replay.charged_lamports, 120);

    let signed = client
        .persist_signed_legacy_lookup_table_cleanup_attempt(
            attempt.id,
            SignedLegacyLookupTableCleanupAttempt {
                transaction_signature: format!("cleanup-durable-signature-{run}"),
                message_hash: "b".repeat(64),
                recent_blockhash: format!("cleanup-durable-blockhash-{run}"),
                last_valid_block_height: 200,
                estimated_fee_lamports: 60,
                recipient_balance_before: None,
            },
        )
        .await?;
    assert_eq!(
        signed.attempt_state,
        LegacyLookupTableCleanupAttemptState::Signed
    );
    let replayed_prepare = client
        .prepare_legacy_lookup_table_cleanup_attempt(prepare)
        .await?;
    assert_eq!(replayed_prepare.id, attempt.id);
    assert_eq!(
        replayed_prepare.attempt_state,
        LegacyLookupTableCleanupAttemptState::Signed,
        "crash after durable signature lost the pending attempt identity"
    );
    let pending = client
        .pending_legacy_lookup_table_cleanup_attempts(&cluster)
        .await?;
    assert_eq!(pending.len(), 1);
    assert_eq!(pending[0].id, attempt.id);

    let v2_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_cluster_budget_reservations WHERE cluster = $1",
    )
    .bind(&cluster)
    .fetch_one(&pool)
    .await?;
    let cleanup_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_legacy_cleanup_budget_reservations WHERE cluster = $1",
    )
    .bind(&cluster)
    .fetch_one(&pool)
    .await?;
    assert_eq!((v2_count, cleanup_count), (1, 1));
    Ok(())
}
