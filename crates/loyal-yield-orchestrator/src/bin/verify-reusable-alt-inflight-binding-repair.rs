use loyal_yield_orchestrator::sqlx::{postgres::PgPoolOptions, Row};
use loyal_yield_orchestrator::{
    AtomicVaultAllocationRequest, AtomicVaultAllocationResult, LookupTableOperationKind,
    LookupTableOperationStatus, NeonSqlClient, PackedShardPolicy, VaultId,
};
use serde_json::json;
use std::{collections::BTreeSet, env, error::Error, io};

type VerifyResult<T> = Result<T, Box<dyn Error + Send + Sync>>;

const ISOLATION_ENV: &str = "REUSABLE_ALT_INFLIGHT_VERIFY_ISOLATED";

#[tokio::main]
async fn main() -> VerifyResult<()> {
    if env::var(ISOLATION_ENV).as_deref() != Ok("1") {
        return fail(format!(
            "refusing database writes: set {ISOLATION_ENV}=1 only for the focused disposable verifier"
        ));
    }
    let database_url = env::var("NEON_DATABASE_URL")
        .map_err(|_| io::Error::other("NEON_DATABASE_URL is required"))?;
    let pool = PgPoolOptions::new()
        .max_connections(4)
        .connect(&database_url)
        .await?;
    let database_name: String =
        loyal_yield_orchestrator::sqlx::query_scalar("SELECT current_database()")
            .fetch_one(&pool)
            .await?;
    ensure(
        database_name.contains("reusable_alt_inflight"),
        "database name must contain reusable_alt_inflight",
    )?;
    let scenario =
        env::var("REUSABLE_ALT_INFLIGHT_VERIFY_SCENARIO").unwrap_or_else(|_| "planner".to_owned());
    if scenario != "planner" {
        let client = NeonSqlClient::from_pool(pool.clone());
        match scenario.as_str() {
            "unsafe" => verify_unsafe_multiple_operation_owners(&client).await?,
            "signed" => verify_signed_operation_reconciliation(&client).await?,
            other => return fail(format!("unknown focused verifier scenario {other}")),
        }
        pool.close().await;
        return Ok(());
    }

    let fixture = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT family.id AS family_id,
               vault.id AS vault_id,
               manifest.id AS manifest_id,
               stale.id AS stale_binding_id,
               canonical.id AS canonical_binding_id,
               unrelated.id AS unrelated_operation_id
        FROM loyal_yield.lookup_table_families family
        JOIN loyal_yield.lookup_table_manifests manifest
          ON manifest.family_id = family.id
         AND manifest.subject_key = 'planner-duplicate'
        JOIN loyal_yield.managed_vaults vault ON vault.id = manifest.vault_id
        JOIN loyal_yield.lookup_table_vault_bindings stale
          ON stale.manifest_id = manifest.id
         AND stale.route_lookup_table_id = (
             SELECT id FROM loyal_yield.route_lookup_tables
             WHERE scope = 'planner-stale-table'
         )
        JOIN loyal_yield.lookup_table_vault_bindings canonical
          ON canonical.manifest_id = manifest.id
         AND canonical.route_lookup_table_id = (
             SELECT id FROM loyal_yield.route_lookup_tables
             WHERE scope = 'planner-canonical-table'
         )
        JOIN loyal_yield.lookup_table_operations unrelated
          ON unrelated.idempotency_key = 'unrelated-pending-verify'
        WHERE family.cluster = 'reusable_alt_inflight_local'
        "#,
    )
    .fetch_one(&pool)
    .await?;
    let family_id: i64 = fixture.try_get("family_id")?;
    let vault_id = VaultId(fixture.try_get("vault_id")?);
    let manifest_id: i64 = fixture.try_get("manifest_id")?;
    let stale_binding_id: i64 = fixture.try_get("stale_binding_id")?;
    let canonical_binding_id: i64 = fixture.try_get("canonical_binding_id")?;
    let unrelated_operation_id: i64 = fixture.try_get("unrelated_operation_id")?;
    let desired_addresses = loyal_yield_orchestrator::sqlx::query_scalar::<_, String>(
        r#"
        SELECT address
        FROM loyal_yield.lookup_table_manifest_addresses
        WHERE manifest_id = $1
        ORDER BY ordinal
        "#,
    )
    .bind(manifest_id)
    .fetch_all(&pool)
    .await?
    .into_iter()
    .collect::<BTreeSet<_>>();

    let duplicate_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_vault_bindings
        WHERE vault_id = $1 AND family_id = $2 AND binding_ordinal = 0
          AND lifecycle_state IN ('preparing', 'warming')
        "#,
    )
    .bind(vault_id.as_i64())
    .bind(family_id)
    .fetch_one(&pool)
    .await?;
    ensure(
        duplicate_count == 2,
        "planner fixture did not reproduce two in-flight bindings",
    )?;
    println!("PASS planner_duplicate_reproduced");

    let client = NeonSqlClient::from_pool(pool.clone());
    let request = AtomicVaultAllocationRequest {
        cluster: "reusable_alt_inflight_local".to_owned(),
        family_id,
        vault_id,
        manifest_id,
        binding_ordinal: 0,
        desired_addresses,
        policy: PackedShardPolicy {
            hard_capacity: 64,
            largest_atomic_expansion: 8,
            safety_margin: 4,
            per_vault_growth_reservation: 0,
            max_vault_cohort: 8,
        },
        next_generation: 0,
        next_shard_ordinal: 40,
        operation_context: json!({
            "source": "reusable_alt_inflight_verifier",
            "recent_slot": 400
        }),
        estimated_fee_lamports: None,
        estimated_rent_lamports: None,
        max_extension_addresses: 8,
    };
    let first = client
        .allocate_vault_binding_and_queue_operation(request.clone())
        .await?;
    let first_operation_id = match first {
        AtomicVaultAllocationResult::BindingReserved {
            binding,
            operations,
            ..
        } => {
            ensure(
                binding.id == canonical_binding_id,
                "planner did not reuse the operation-owning canonical binding",
            )?;
            ensure(
                operations.len() == 1
                    && operations[0].binding_id == Some(canonical_binding_id)
                    && operations[0].id != unrelated_operation_id
                    && operations[0].operation_kind == LookupTableOperationKind::Extend,
                "same-table unrelated pending operation suppressed canonical binding work",
            )?;
            operations[0].id
        }
        other => {
            return fail(format!(
                "planner duplicate fixture returned unexpected allocation: {other:?}"
            ))
        }
    };

    let binding_states = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, lifecycle_state
        FROM loyal_yield.lookup_table_vault_bindings
        WHERE id = ANY($1)
        ORDER BY id
        "#,
    )
    .bind(&vec![stale_binding_id, canonical_binding_id])
    .fetch_all(&pool)
    .await?;
    let stale_failed = binding_states.iter().any(|row| {
        row.try_get::<i64, _>("id").ok() == Some(stale_binding_id)
            && matches!(
                row.try_get::<String, _>("lifecycle_state").as_deref(),
                Ok("failed")
            )
    });
    let canonical_preparing = binding_states.iter().any(|row| {
        row.try_get::<i64, _>("id").ok() == Some(canonical_binding_id)
            && matches!(
                row.try_get::<String, _>("lifecycle_state").as_deref(),
                Ok("preparing")
            )
    });
    ensure(
        stale_failed && canonical_preparing,
        "planner did not fail only the stale no-operation binding",
    )?;

    let second = client
        .allocate_vault_binding_and_queue_operation(request)
        .await?;
    match second {
        AtomicVaultAllocationResult::BindingReserved {
            binding,
            operations,
            ..
        } => ensure(
            binding.id == canonical_binding_id
                && operations.len() == 1
                && operations[0].id == first_operation_id,
            "planner retry did not reuse canonical binding-scoped operation",
        )?,
        other => {
            return fail(format!(
                "planner retry returned unexpected allocation: {other:?}"
            ))
        }
    }
    println!("PASS planner_binding_scoped_reconciliation");
    pool.close().await;
    Ok(())
}

async fn verify_unsafe_multiple_operation_owners(client: &NeonSqlClient) -> VerifyResult<()> {
    let (request, _) = fixture_request(client, "unsafe-duplicate").await?;
    let states_before = fixture_binding_states(client, "unsafe-duplicate").await?;
    let error = client
        .allocate_vault_binding_and_queue_operation(request)
        .await
        .expect_err("multiple operation-owning bindings must fail closed");
    ensure(
        error
            .to_string()
            .contains("multiple operation-owning in-flight bindings"),
        "unsafe duplicate failed for the wrong reason",
    )?;
    ensure(
        fixture_binding_states(client, "unsafe-duplicate").await? == states_before,
        "unsafe duplicate planner failure changed binding state",
    )?;
    println!("PASS planner_multiple_operation_owners_fail_closed");
    Ok(())
}

async fn verify_signed_operation_reconciliation(client: &NeonSqlClient) -> VerifyResult<()> {
    let (request, signed_binding_id) = fixture_request(client, "unsafe-duplicate").await?;
    let signed_operation_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT operation.id
        FROM loyal_yield.lookup_table_operations operation
        WHERE operation.idempotency_key = 'unsafe-stale-operation'
          AND operation.operation_state = 'signed'
          AND operation.transaction_signature IS NOT NULL
        "#,
    )
    .fetch_one(client.pool())
    .await?;
    let result = client
        .allocate_vault_binding_and_queue_operation(request)
        .await?;
    match result {
        AtomicVaultAllocationResult::BindingReserved {
            binding,
            operations,
            ..
        } => ensure(
            binding.id == signed_binding_id
                && operations.len() == 1
                && operations[0].id == signed_operation_id
                && operations[0].operation_state == LookupTableOperationStatus::Signed
                && operations[0].transaction_signature.is_some(),
            "signed binding was not returned through normal operation reconciliation",
        )?,
        other => {
            return fail(format!(
                "signed duplicate fixture returned unexpected allocation: {other:?}"
            ))
        }
    }
    let no_operation_inflight: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_vault_bindings binding
        JOIN loyal_yield.lookup_table_manifests manifest
          ON manifest.id = binding.manifest_id
        WHERE manifest.subject_key = 'unsafe-duplicate'
          AND binding.lifecycle_state IN ('preparing', 'warming')
          AND binding.id <> $1
          AND NOT EXISTS (
              SELECT 1
              FROM loyal_yield.lookup_table_operations operation
              WHERE operation.binding_id = binding.id
          )
        "#,
    )
    .bind(signed_binding_id)
    .fetch_one(client.pool())
    .await?;
    ensure(
        no_operation_inflight == 0,
        "signed reconciliation left a duplicate no-operation binding in flight",
    )?;
    println!("PASS planner_signed_operation_preserved_for_reconciliation");
    Ok(())
}

async fn fixture_request(
    client: &NeonSqlClient,
    subject_key: &str,
) -> VerifyResult<(AtomicVaultAllocationRequest, i64)> {
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT family.id AS family_id,
               vault.id AS vault_id,
               manifest.id AS manifest_id,
               operation_binding.id AS operation_binding_id
        FROM loyal_yield.lookup_table_families family
        JOIN loyal_yield.lookup_table_manifests manifest
          ON manifest.family_id = family.id
         AND manifest.subject_key = $1
        JOIN loyal_yield.managed_vaults vault ON vault.id = manifest.vault_id
        JOIN loyal_yield.lookup_table_vault_bindings operation_binding
          ON operation_binding.manifest_id = manifest.id
         AND EXISTS (
             SELECT 1
             FROM loyal_yield.lookup_table_operations operation
             WHERE operation.binding_id = operation_binding.id
               AND operation.idempotency_key = 'unsafe-stale-operation'
         )
        WHERE family.cluster = 'reusable_alt_inflight_local'
        "#,
    )
    .bind(subject_key)
    .fetch_one(client.pool())
    .await?;
    let family_id: i64 = row.try_get("family_id")?;
    let vault_id = VaultId(row.try_get("vault_id")?);
    let manifest_id: i64 = row.try_get("manifest_id")?;
    let operation_binding_id: i64 = row.try_get("operation_binding_id")?;
    let desired_addresses = loyal_yield_orchestrator::sqlx::query_scalar::<_, String>(
        r#"
        SELECT address
        FROM loyal_yield.lookup_table_manifest_addresses
        WHERE manifest_id = $1
        ORDER BY ordinal
        "#,
    )
    .bind(manifest_id)
    .fetch_all(client.pool())
    .await?
    .into_iter()
    .collect::<BTreeSet<_>>();
    Ok((
        AtomicVaultAllocationRequest {
            cluster: "reusable_alt_inflight_local".to_owned(),
            family_id,
            vault_id,
            manifest_id,
            binding_ordinal: 0,
            desired_addresses,
            policy: PackedShardPolicy {
                hard_capacity: 64,
                largest_atomic_expansion: 8,
                safety_margin: 4,
                per_vault_growth_reservation: 0,
                max_vault_cohort: 8,
            },
            next_generation: 0,
            next_shard_ordinal: 41,
            operation_context: json!({
                "source": "reusable_alt_inflight_verifier",
                "recent_slot": 401
            }),
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            max_extension_addresses: 8,
        },
        operation_binding_id,
    ))
}

async fn fixture_binding_states(
    client: &NeonSqlClient,
    subject_key: &str,
) -> VerifyResult<Vec<(i64, String)>> {
    let rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT binding.id, binding.lifecycle_state
        FROM loyal_yield.lookup_table_vault_bindings binding
        JOIN loyal_yield.lookup_table_manifests manifest
          ON manifest.id = binding.manifest_id
        WHERE manifest.subject_key = $1
        ORDER BY binding.id
        "#,
    )
    .bind(subject_key)
    .fetch_all(client.pool())
    .await?;
    rows.iter()
        .map(|row| Ok((row.try_get("id")?, row.try_get("lifecycle_state")?)))
        .collect()
}

fn ensure(condition: bool, message: impl Into<String>) -> VerifyResult<()> {
    if condition {
        Ok(())
    } else {
        fail(message)
    }
}

fn fail<T>(message: impl Into<String>) -> VerifyResult<T> {
    Err(io::Error::other(message.into()).into())
}
