use chrono::{Duration, Utc};
use loyal_yield_orchestrator::sqlx::{postgres::PgPoolOptions, Row};
use loyal_yield_orchestrator::*;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{address_lookup_table::instruction::derive_lookup_table_address, pubkey::Pubkey};
use std::{
    env,
    error::Error,
    io,
    sync::{
        atomic::{AtomicU64, Ordering},
        Arc,
    },
    time::{SystemTime, UNIX_EPOCH},
};
use tokio::sync::Barrier;

type VerifyResult<T> = Result<T, Box<dyn Error + Send + Sync>>;

const ISOLATION_ENV: &str = "REUSABLE_ALT_DB_VERIFY_ISOLATED";
static PUBKEY_SEQUENCE: AtomicU64 = AtomicU64::new(0);

#[tokio::main]
async fn main() -> VerifyResult<()> {
    if env::var(ISOLATION_ENV).as_deref() != Ok("1") {
        return fail(format!(
            "refusing database writes: set {ISOLATION_ENV}=1 only for a disposable database"
        ));
    }
    let database_url = env::var("NEON_DATABASE_URL")
        .map_err(|_| io::Error::other("NEON_DATABASE_URL is required"))?;
    let pool = PgPoolOptions::new()
        .max_connections(12)
        .connect(&database_url)
        .await?;
    let database_name: String =
        loyal_yield_orchestrator::sqlx::query_scalar("SELECT current_database()")
            .fetch_one(&pool)
            .await?;
    ensure(
        database_name.contains("reusable_alt"),
        "database name must contain reusable_alt in addition to the explicit isolation marker",
    )?;
    let client = NeonSqlClient::from_pool(pool.clone());
    client
        .require_schema_migration(17, "reusable_route_lookup_tables")
        .await?;

    let run = format!("{}-{}", std::process::id(), Utc::now().timestamp_micros());
    let mut passed = Vec::new();

    verify_manager_identity_independence(&client, &run).await?;
    passed.push("durable ALT manager identity independence");

    verify_zero_class_and_request_sealing(&client, &run).await?;
    passed.push("sealed requests, route-independent idempotency, zero-class satisfaction");

    let planned = verify_atomic_and_concurrent_planning(&client, &run).await?;
    passed.push("atomic reservation/create/binding/outbox and concurrent planner idempotency");

    verify_active_head_growth_and_relocation(&client, &run).await?;
    passed.push("active packed-head in-place growth and capacity-driven relocation");

    verify_canonical_vault_route_cohorts(&client, &run).await?;
    passed
        .push("canonical vault cohort union, relocation, supersession, and cancellation lifecycle");

    verify_manifest_and_capacity_constraints(&client, &planned).await?;
    passed.push("manifest immutability, unique-address, and capacity constraints");

    verify_refresh_and_fencing(&client, &planned).await?;
    passed.push("fenced stale-slot create reservation refresh and stale lease rejection");

    verify_operation_metadata_constraints(&client, &run).await?;
    passed.push("unsigned reconcile recovery and signed-metadata durability constraint");

    verify_attempt_failure_policy(&client, &run).await?;
    passed.push("fenced per-attempt retry, exhaustion, and signed reconciliation policy");

    verify_reconciliation_deferral_and_family_lookup(&client, &run).await?;
    passed.push("paused-family reconciliation and attempt-neutral unsigned deferral");

    verify_crash_after_send_accounting(&client, &run).await?;
    passed.push("crash-after-send recovery accounting from persisted signed estimates");

    verify_cleanup_usage_exclusion(&client, &run).await?;
    passed.push("usage lease and cleanup enqueue mutual exclusion");

    verify_atomic_binding_activation_fence(&client, &run).await?;
    passed.push("atomic binding activation validation and one-winner head race");

    verify_rollbacks_and_finalization(&client, &run).await?;
    passed.push("family/binding rollback and expired-window reference release");

    verify_rollout_controls(&client, &run).await?;
    passed.push("rollout mode and global force-legacy preservation");

    verify_legacy_retirement(&client, &run).await?;
    passed.push("explicit fenced legacy retirement");

    verify_observable_snapshot(&client, &planned).await?;
    passed.push("operator snapshot fields and recent compilation evidence");

    println!(
        "{}",
        json!({
            "event": "reusable_alt_db_verifier",
            "result": "PASS",
            "databaseClass": "explicitly_isolated_disposable",
            "checks": passed,
            "externalRpcUsed": false,
            "signerLoaded": false,
            "productionActions": false,
        })
    );
    pool.close().await;
    Ok(())
}

async fn verify_manager_identity_independence(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<()> {
    let cluster = format!("db-verify-manager-{run}");
    let manager = unique_pubkey("overlapping-manager").to_string();
    loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.route_policies
            (settings, authority, policy_seed, policy_account, vault_index,
             vault_pubkey, delegated_signers, threshold, last_seen_slot,
             last_seen_signature)
        VALUES ($1, $2, 1, $3, 0, $4, ARRAY[$5]::TEXT[], 1, 1, $6)
        "#,
    )
    .bind(unique_pubkey("overlap-settings").to_string())
    .bind(unique_pubkey("overlap-authority").to_string())
    .bind(unique_pubkey("overlap-policy").to_string())
    .bind(unique_pubkey("overlap-vault").to_string())
    .bind(&manager)
    .bind(format!("overlap-signature-{run}"))
    .execute(client.pool())
    .await?;
    ensure(
        client
            .lookup_table_manager_identity_overlaps_control_plane(&manager)
            .await?,
        "active delegated route signer was not detected as an ALT manager overlap",
    )?;
    ensure(
        client
            .create_or_validate_lookup_table_family(LookupTableFamilyUpsert {
                cluster: cluster.clone(),
                logical_name: format!("overlap-family-{run}"),
                kind: LookupTableFamilyKind::VaultShards,
                desired_state: LookupTableFamilyState::Active,
                planner_version: "db-verifier-v1".to_owned(),
                catalog_version: "db-verifier-catalog-v1".to_owned(),
                active_generation: Some(0),
                previous_generation: None,
                rollback_until: None,
                provisioning_authority: manager.clone(),
                payer: manager,
                hard_capacity: 64,
                largest_atomic_expansion: 8,
                safety_margin: 4,
                allocation_high_water: 52,
            })
            .await
            .is_err(),
        "family bootstrap accepted an active delegated route signer as ALT manager",
    )?;
    let family_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_families WHERE cluster = $1",
    )
    .bind(&cluster)
    .fetch_one(client.pool())
    .await?;
    ensure(
        family_count == 0,
        "rejected overlapping ALT manager left a partial family row",
    )?;

    let deterministic_cluster = format!("db-verify-family-kind-{run}");
    let independent_manager = unique_pubkey("independent-family-manager").to_string();
    create_family(
        client,
        &deterministic_cluster,
        "vault-family-a",
        LookupTableFamilyKind::VaultShards,
        &independent_manager,
        40,
        Some(0),
    )
    .await?;
    ensure(
        create_family(
            client,
            &deterministic_cluster,
            "vault-family-b",
            LookupTableFamilyKind::VaultShards,
            &independent_manager,
            40,
            Some(0),
        )
        .await
        .is_err(),
        "schema accepted two active families of the same cluster/kind",
    )
}

#[derive(Clone)]
struct PlannedFixture {
    cluster: String,
    vault_id: VaultId,
    shared_family: LookupTableFamilyRecord,
    vault_family: LookupTableFamilyRecord,
    request: LookupTableProvisioningRequestRecord,
    vault_manifest_id: i64,
    vault_binding: LookupTableVaultBindingRecord,
    vault_operation: LookupTableOperationRecord,
    vault_table: ReusableLookupTableRecord,
}

async fn verify_zero_class_and_request_sealing(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<()> {
    let cluster = format!("db-verify-zero-{run}");
    let authority = unique_pubkey("zero-authority").to_string();
    let (_shared, vault_family) = create_families(client, &cluster, &authority, 40).await?;
    let vault_id = create_vault(client, &format!("zero-{run}"), 0).await?;

    // An unrelated pending operation must not prevent a zero-class request
    // from being satisfied; satisfaction is scoped to its manifests.
    let unrelated = insert_table(
        client,
        &cluster,
        &vault_family,
        LookupTableAllocationKind::VaultShard,
        99,
        0,
        LookupTableLifecycle::Active,
        false,
        unique_pubkey("zero-unrelated-table").to_string(),
    )
    .await?;
    set_verified_empty(client, unrelated.id).await?;
    client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("unrelated-{run}"),
            family_id: vault_family.id,
            route_lookup_table_id: Some(unrelated.id),
            manifest_id: None,
            binding_id: None,
            operation_kind: LookupTableOperationKind::Verify,
            target_generation: None,
            target_shard_ordinal: None,
            operation_context: json!({"source": "db_verifier"}),
            mutation_epoch: unrelated.mutation_epoch,
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            addresses: Vec::new(),
        })
        .await?;

    let input = LookupTableProvisioningRequestUpsert {
        cluster: cluster.clone(),
        vault_id,
        route_fingerprint: format!("route-a-{run}"),
        requirements_fingerprint: format!("requirements-zero-{run}"),
        shared_manifest_id: None,
        vault_manifest_id: None,
        desired_shared_hash: Some(format!("shared-empty-{run}")),
        desired_vault_hash: Some(format!("vault-empty-{run}")),
        shared_addresses: Vec::new(),
        vault_addresses: Vec::new(),
    };
    let request = client
        .upsert_lookup_table_provisioning_request(input.clone())
        .await?;
    ensure(
        request.sealed_at.is_some(),
        "zero-class request was not sealed",
    )?;
    let mut second_route = input.clone();
    second_route.route_fingerprint = format!("route-b-{run}");
    let idempotent = client
        .upsert_lookup_table_provisioning_request(second_route)
        .await?;
    ensure(
        idempotent.id == request.id && idempotent.route_fingerprint == input.route_fingerprint,
        "same requirements from another route did not reuse the first sealed request",
    )?;

    let content_mutation = loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_provisioning_requests SET route_fingerprint = 'mutated' WHERE id = $1",
    )
    .bind(request.id)
    .execute(client.pool())
    .await;
    ensure(
        content_mutation.is_err(),
        "sealed request content remained mutable",
    )?;
    let address_mutation = loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.lookup_table_provisioning_request_addresses
            (request_id, address, semantic_class, ordinal, account_role, is_writable)
        VALUES ($1, $2, 'vault', 0, 'late', FALSE)
        "#,
    )
    .bind(request.id)
    .bind(unique_pubkey("sealed-request-late-address").to_string())
    .execute(client.pool())
    .await;
    ensure(
        address_mutation.is_err(),
        "sealed request addresses remained mutable",
    )?;

    let leased = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "zero-planner",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("zero-class request was not leaseable"))?;
    let lease = request_lease(&leased)?;
    let plan = client
        .plan_lookup_table_provisioning_request(&cluster, leased.id, &lease, plan_policy(1_000))
        .await?;
    ensure(
        plan.request.request_status == LookupTableProvisioningRequestStatus::Satisfied,
        "zero-class request was blocked by unrelated control-plane work",
    )?;
    ensure(
        matches!(
            plan.vault_allocation,
            AtomicVaultAllocationResult::NotRequired
        ) && plan.shared_operations.is_empty(),
        "zero-class request allocated a meaningless physical table",
    )?;
    Ok(())
}

async fn verify_atomic_and_concurrent_planning(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<PlannedFixture> {
    let cluster = format!("db-verify-plan-{run}");
    let authority = unique_pubkey("plan-authority").to_string();
    let (shared_family, vault_family) = create_families(client, &cluster, &authority, 40).await?;
    let vault_id = create_vault(client, &format!("plan-{run}"), 1).await?;
    let shared_addresses = typed_addresses(LookupTableManifestSubject::SharedMarket, 2, "market");
    let vault_addresses = typed_addresses(LookupTableManifestSubject::Vault, 3, "vault");
    let input = LookupTableProvisioningRequestUpsert {
        cluster: cluster.clone(),
        vault_id,
        route_fingerprint: format!("route-first-{run}"),
        requirements_fingerprint: format!("requirements-shared-{run}"),
        shared_manifest_id: None,
        vault_manifest_id: None,
        desired_shared_hash: Some(format!("shared-hash-{run}")),
        desired_vault_hash: Some(format!("vault-hash-{run}")),
        shared_addresses: shared_addresses.clone(),
        vault_addresses: vault_addresses.clone(),
    };
    let request = client
        .upsert_lookup_table_provisioning_request(input.clone())
        .await?;
    let mut second_route = input.clone();
    second_route.route_fingerprint = format!("route-second-{run}");
    let same = client
        .upsert_lookup_table_provisioning_request(second_route)
        .await?;
    ensure(
        same.id == request.id,
        "route-independent provisioning request was duplicated",
    )?;
    let mut drifted = input;
    drifted.route_fingerprint = format!("route-third-{run}");
    drifted.vault_addresses[0].address = unique_pubkey("drifted-address").to_string();
    ensure(
        client
            .upsert_lookup_table_provisioning_request(drifted)
            .await
            .is_err(),
        "sealed request idempotency accepted address drift",
    )?;

    let leased = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "concurrent-planner",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("planning request was not leaseable"))?;
    let lease = request_lease(&leased)?;
    let barrier = Arc::new(Barrier::new(3));
    let first = {
        let client = client.clone();
        let cluster = cluster.clone();
        let lease = lease.clone();
        let barrier = barrier.clone();
        tokio::spawn(async move {
            barrier.wait().await;
            client
                .plan_lookup_table_provisioning_request(
                    &cluster,
                    leased.id,
                    &lease,
                    plan_policy(2_000),
                )
                .await
        })
    };
    let second = {
        let client = client.clone();
        let cluster = cluster.clone();
        let lease = lease.clone();
        let barrier = barrier.clone();
        tokio::spawn(async move {
            barrier.wait().await;
            client
                .plan_lookup_table_provisioning_request(
                    &cluster,
                    request.id,
                    &lease,
                    plan_policy(2_000),
                )
                .await
        })
    };
    barrier.wait().await;
    let first = first.await?;
    let second = second.await?;
    ensure(
        usize::from(first.is_ok()) + usize::from(second.is_ok()) == 1,
        "concurrent planner did not have exactly one fenced winner",
    )?;
    let plan = first.or(second)?;
    let (vault_binding, operations) = match plan.vault_allocation {
        AtomicVaultAllocationResult::CreateQueued {
            binding,
            operations,
            ..
        } => (binding, operations),
        other => {
            return fail(format!(
                "first vault allocation was not atomic create: {other:?}"
            ))
        }
    };
    ensure(
        operations.len() == 1 && plan.shared_operations.len() == 1,
        "atomic plan did not write exactly one shared and one vault outbox operation",
    )?;
    let vault_operation = operations[0].clone();
    let vault_table = client
        .reusable_lookup_table(vault_binding.route_lookup_table_id)
        .await?
        .ok_or_else(|| io::Error::other("atomic vault table disappeared"))?;
    ensure(
        vault_operation.binding_id == Some(vault_binding.id)
            && vault_operation.route_lookup_table_id == Some(vault_table.id)
            && vault_table.reserved_address_count == vault_binding.reserved_capacity,
        "table, binding, reservation accounting, and outbox were not atomically linked",
    )?;

    let counts_before = plan_counts(client, &cluster).await?;
    let retry = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "retry-planner",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("queued plan was not retryable"))?;
    client
        .plan_lookup_table_provisioning_request(
            &cluster,
            retry.id,
            &request_lease(&retry)?,
            plan_policy(2_000),
        )
        .await?;
    ensure(
        counts_before == plan_counts(client, &cluster).await?,
        "idempotent planner retry duplicated physical, binding, or outbox rows",
    )?;

    let request_row = request_by_id(client, request.id).await?;
    Ok(PlannedFixture {
        cluster,
        vault_id,
        shared_family,
        vault_family,
        vault_manifest_id: request_row
            .vault_manifest_id
            .ok_or_else(|| io::Error::other("vault manifest was not attached"))?,
        request: request_row,
        vault_binding,
        vault_operation,
        vault_table,
    })
}

async fn verify_active_head_growth_and_relocation(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<()> {
    let cluster = format!("db-verify-active-growth-{run}");
    let authority = unique_pubkey("active-growth-authority").to_string();
    let family = create_family(
        client,
        &cluster,
        "active-growth-vaults",
        LookupTableFamilyKind::VaultShards,
        &authority,
        52,
        Some(0),
    )
    .await?;
    let vault_id = create_vault(client, &format!("active-growth-{run}"), 7).await?;
    let mut base_addresses = typed_addresses(LookupTableManifestSubject::Vault, 2, "growth-base");
    let base_manifest = client
        .persist_lookup_table_manifest(LookupTableManifestWrite {
            family_id: family.id,
            subject_kind: LookupTableManifestSubject::Vault,
            subject_key: format!("growth-base-{run}"),
            vault_id: Some(vault_id),
            desired_set_hash: format!("growth-base-hash-{run}"),
            source_slot: Some(100),
            planner_version: family.planner_version.clone(),
            catalog_version: family.catalog_version.clone(),
            addresses: base_addresses.clone(),
        })
        .await?;
    let table = insert_table(
        client,
        &cluster,
        &family,
        LookupTableAllocationKind::VaultShard,
        0,
        0,
        LookupTableLifecycle::Active,
        true,
        derive_lookup_table_address(&Pubkey::try_from(authority.as_str())?, 50_000)
            .0
            .to_string(),
    )
    .await?;
    let observed_slot = 120;
    let membership = base_addresses
        .iter()
        .enumerate()
        .map(|(ordinal, address)| LookupTableMembershipAddress {
            address: address.address.clone(),
            ordinal: ordinal as i32,
            added_operation_id: None,
            added_slot: 100,
            usable_after_slot: 101,
            last_verified_slot: observed_slot,
            last_verified_at: Utc::now(),
        })
        .collect();
    let table = client
        .replace_confirmed_lookup_table_membership(table.id, 0, 1, observed_slot, membership)
        .await?;
    let table = client
        .mark_reusable_lookup_table_verification(
            table.id,
            table.mutation_epoch,
            LookupTableLifecycle::Active,
            LookupTableLifecycle::Active,
            true,
            table.address_count,
            observed_slot,
        )
        .await?;
    let base_binding = client
        .insert_lookup_table_vault_binding(LookupTableVaultBindingInsert {
            vault_id,
            family_id: family.id,
            route_lookup_table_id: table.id,
            manifest_id: base_manifest.id,
            binding_ordinal: 0,
            allocation_mode: LookupTableBindingMode::PackedShard,
            reserved_capacity: 4,
            predecessor_binding_id: None,
        })
        .await?;
    let base_binding = client
        .flip_lookup_table_binding_head(
            base_binding.id,
            observed_slot,
            Utc::now() + Duration::hours(1),
        )
        .await?
        .active;

    base_addresses.push(LookupTableManifestAddressRecord {
        address: unique_pubkey("growth-plus-one").to_string(),
        ordinal: 2,
        semantic_class: LookupTableManifestSubject::Vault,
        account_role: "growth_plus_one".to_owned(),
        is_writable: false,
    });
    let expanded_manifest = client
        .persist_lookup_table_manifest(LookupTableManifestWrite {
            family_id: family.id,
            subject_kind: LookupTableManifestSubject::Vault,
            subject_key: format!("growth-expanded-{run}"),
            vault_id: Some(vault_id),
            desired_set_hash: format!("growth-expanded-hash-{run}"),
            source_slot: Some(121),
            planner_version: family.planner_version.clone(),
            catalog_version: family.catalog_version.clone(),
            addresses: base_addresses.clone(),
        })
        .await?;
    let policy = PackedShardPolicy {
        hard_capacity: 64,
        largest_atomic_expansion: 8,
        safety_margin: 4,
        per_vault_growth_reservation: 2,
        max_vault_cohort: 4,
    };
    let expanded = client
        .allocate_vault_binding_and_queue_operation(AtomicVaultAllocationRequest {
            cluster: cluster.clone(),
            family_id: family.id,
            vault_id,
            manifest_id: expanded_manifest.id,
            binding_ordinal: 0,
            desired_addresses: base_addresses
                .iter()
                .map(|address| address.address.clone())
                .collect(),
            policy,
            next_generation: 0,
            next_shard_ordinal: 1,
            operation_context: json!({"recent_slot": 49_999}),
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            max_extension_addresses: 8,
        })
        .await?;
    let (expanded_binding, expanded_operations) = match expanded {
        AtomicVaultAllocationResult::BindingReserved {
            allocation:
                PackedVaultAllocation::ReserveExistingShard {
                    table_id,
                    reservation_delta,
                    ..
                },
            binding,
            operations,
        } if table_id == table.id && reservation_delta == 1 => (binding, operations),
        other => return fail(format!("+1 manifest did not expand in place: {other:?}")),
    };
    ensure(
        expanded_binding.route_lookup_table_id == table.id
            && expanded_binding.predecessor_binding_id == Some(base_binding.id)
            && expanded_binding.lifecycle_state == LookupTableBindingLifecycle::Preparing
            && expanded_operations.len() == 1
            && expanded_operations[0].operation_kind == LookupTableOperationKind::Extend,
        "in-place expansion did not preserve the active predecessor through warmup",
    )?;
    let after_expansion = client
        .reusable_lookup_table(table.id)
        .await?
        .ok_or_else(|| io::Error::other("in-place expansion table disappeared"))?;
    ensure(
        after_expansion.reserved_address_count == 5,
        "same-table manifest warmup double-counted the binding reservation",
    )?;

    let pressure_vault = create_vault(client, &format!("growth-pressure-{run}"), 8).await?;
    let pressure_manifest = client
        .persist_lookup_table_manifest(LookupTableManifestWrite {
            family_id: family.id,
            subject_kind: LookupTableManifestSubject::Vault,
            subject_key: format!("growth-pressure-{run}"),
            vault_id: Some(pressure_vault),
            desired_set_hash: format!("growth-pressure-hash-{run}"),
            source_slot: Some(122),
            planner_version: family.planner_version.clone(),
            catalog_version: family.catalog_version.clone(),
            addresses: typed_addresses(LookupTableManifestSubject::Vault, 1, "growth-pressure"),
        })
        .await?;
    client
        .insert_lookup_table_vault_binding(LookupTableVaultBindingInsert {
            vault_id: pressure_vault,
            family_id: family.id,
            route_lookup_table_id: table.id,
            manifest_id: pressure_manifest.id,
            binding_ordinal: 0,
            allocation_mode: LookupTableBindingMode::PackedShard,
            reserved_capacity: 47,
            predecessor_binding_id: None,
        })
        .await?;
    let mut outgrown_addresses = base_addresses;
    outgrown_addresses.push(LookupTableManifestAddressRecord {
        address: unique_pubkey("growth-outgrown").to_string(),
        ordinal: 3,
        semantic_class: LookupTableManifestSubject::Vault,
        account_role: "growth_outgrown".to_owned(),
        is_writable: false,
    });
    let outgrown_manifest = client
        .persist_lookup_table_manifest(LookupTableManifestWrite {
            family_id: family.id,
            subject_kind: LookupTableManifestSubject::Vault,
            subject_key: format!("growth-outgrown-{run}"),
            vault_id: Some(vault_id),
            desired_set_hash: format!("growth-outgrown-hash-{run}"),
            source_slot: Some(123),
            planner_version: family.planner_version.clone(),
            catalog_version: family.catalog_version.clone(),
            addresses: outgrown_addresses.clone(),
        })
        .await?;
    let relocated = client
        .allocate_vault_binding_and_queue_operation(AtomicVaultAllocationRequest {
            cluster,
            family_id: family.id,
            vault_id,
            manifest_id: outgrown_manifest.id,
            binding_ordinal: 0,
            desired_addresses: outgrown_addresses
                .iter()
                .map(|address| address.address.clone())
                .collect(),
            policy,
            next_generation: 0,
            next_shard_ordinal: 1,
            operation_context: json!({"recent_slot": 49_998}),
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            max_extension_addresses: 8,
        })
        .await?;
    match relocated {
        AtomicVaultAllocationResult::CreateQueued {
            allocation: PackedVaultAllocation::PrepareNewShard { .. },
            binding,
            operations,
        } => ensure(
            binding.route_lookup_table_id != table.id
                && binding.predecessor_binding_id == Some(base_binding.id)
                && operations.len() == 1
                && operations[0].operation_kind == LookupTableOperationKind::Create,
            "capacity outgrowth did not atomically relocate the complete manifest",
        ),
        other => fail(format!("capacity outgrowth did not relocate: {other:?}")),
    }
}

async fn verify_canonical_vault_route_cohorts(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<()> {
    let cluster = format!("db-verify-cohorts-{run}");
    let authority = unique_pubkey("cohort-authority").to_string();
    let (_shared_family, vault_family) = create_families(client, &cluster, &authority, 40).await?;
    let vault_id = create_vault(client, &format!("cohort-{run}"), 18).await?;
    let shared_a = typed_addresses(
        LookupTableManifestSubject::SharedMarket,
        1,
        "cohort-market-a",
    );
    let vault_a = typed_addresses(LookupTableManifestSubject::Vault, 2, "cohort-vault-a");
    let request_a = client
        .upsert_lookup_table_provisioning_request(LookupTableProvisioningRequestUpsert {
            cluster: cluster.clone(),
            vault_id,
            route_fingerprint: format!("cohort-route-a-{run}"),
            requirements_fingerprint: format!("cohort-requirements-a-{run}"),
            shared_manifest_id: None,
            vault_manifest_id: None,
            desired_shared_hash: Some(format!("cohort-shared-a-{run}")),
            desired_vault_hash: Some(format!("cohort-vault-a-{run}")),
            shared_addresses: shared_a,
            vault_addresses: vault_a.clone(),
        })
        .await?;
    let leased_a = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "cohort-planner-a",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("cohort A request was not leaseable"))?;
    let plan_a = client
        .plan_lookup_table_provisioning_request(
            &cluster,
            leased_a.id,
            &request_lease(&leased_a)?,
            plan_policy(70_000),
        )
        .await?;
    let binding_a = match plan_a.vault_allocation {
        AtomicVaultAllocationResult::CreateQueued { binding, .. } => binding,
        other => {
            return fail(format!(
                "cohort A did not create its first shard: {other:?}"
            ))
        }
    };
    materialize_binding_manifest(client, &binding_a, 70_001).await?;
    let binding_a = client
        .flip_lookup_table_binding_head(binding_a.id, 70_001, Utc::now() + Duration::hours(1))
        .await?
        .active;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_provisioning_requests SET request_status = 'satisfied', satisfied_at = now() WHERE id = $1",
    )
    .bind(request_a.id)
    .execute(client.pool())
    .await?;

    // Fill the remainder of the packed table's durable reservation budget with
    // another vault. The next aggregate revision must relocate in full rather
    // than append only route B and drop route A on the new shard.
    let pressure_vault = create_vault(client, &format!("cohort-pressure-{run}"), 19).await?;
    let pressure_manifest = client
        .persist_lookup_table_manifest(LookupTableManifestWrite {
            family_id: vault_family.id,
            subject_kind: LookupTableManifestSubject::Vault,
            subject_key: format!("cohort-pressure-{run}"),
            vault_id: Some(pressure_vault),
            desired_set_hash: format!("cohort-pressure-hash-{run}"),
            source_slot: Some(70_001),
            planner_version: vault_family.planner_version.clone(),
            catalog_version: vault_family.catalog_version.clone(),
            addresses: typed_addresses(LookupTableManifestSubject::Vault, 1, "cohort-pressure"),
        })
        .await?;
    client
        .insert_lookup_table_vault_binding(LookupTableVaultBindingInsert {
            vault_id: pressure_vault,
            family_id: vault_family.id,
            route_lookup_table_id: binding_a.route_lookup_table_id,
            manifest_id: pressure_manifest.id,
            binding_ordinal: 0,
            allocation_mode: LookupTableBindingMode::PackedShard,
            reserved_capacity: 34,
            predecessor_binding_id: None,
        })
        .await?;

    let shared_b = typed_addresses(
        LookupTableManifestSubject::SharedMarket,
        1,
        "cohort-market-b",
    );
    let vault_b = typed_addresses(LookupTableManifestSubject::Vault, 2, "cohort-vault-b");
    let request_b = client
        .upsert_lookup_table_provisioning_request(LookupTableProvisioningRequestUpsert {
            cluster: cluster.clone(),
            vault_id,
            route_fingerprint: format!("cohort-route-b-{run}"),
            requirements_fingerprint: format!("cohort-requirements-b-{run}"),
            shared_manifest_id: None,
            vault_manifest_id: None,
            desired_shared_hash: Some(format!("cohort-shared-b-{run}")),
            desired_vault_hash: Some(format!("cohort-vault-b-{run}")),
            shared_addresses: shared_b,
            vault_addresses: vault_b.clone(),
        })
        .await?;
    let leased_b = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "cohort-planner-b",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("cohort B request was not leaseable"))?;
    ensure(
        leased_b.id == request_b.id,
        "cohort B planner leased the wrong request",
    )?;
    let plan_b = client
        .plan_lookup_table_provisioning_request(
            &cluster,
            leased_b.id,
            &request_lease(&leased_b)?,
            plan_policy(70_002),
        )
        .await?;
    let binding_b = match plan_b.vault_allocation {
        AtomicVaultAllocationResult::CreateQueued { binding, .. } => binding,
        other => {
            return fail(format!(
                "aggregate A+B did not relocate under pressure: {other:?}"
            ))
        }
    };
    ensure(
        binding_b.route_lookup_table_id != binding_a.route_lookup_table_id,
        "aggregate relocation reused the capacity-constrained predecessor",
    )?;
    let aggregate = client
        .lookup_table_manifest(binding_b.manifest_id)
        .await?
        .ok_or_else(|| io::Error::other("aggregate A+B manifest disappeared"))?;
    let expected_union = vault_a
        .iter()
        .chain(vault_b.iter())
        .map(|row| row.address.clone())
        .collect::<std::collections::BTreeSet<_>>();
    let aggregate_addresses = aggregate
        .addresses
        .iter()
        .map(|row| row.address.clone())
        .collect::<std::collections::BTreeSet<_>>();
    ensure(
        aggregate_addresses == expected_union,
        "relocated vault aggregate did not contain the complete A union B cohort",
    )?;
    materialize_binding_manifest(client, &binding_b, 70_003).await?;
    let binding_b = client
        .flip_lookup_table_binding_head(binding_b.id, 70_003, Utc::now() + Duration::hours(1))
        .await?
        .active;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_provisioning_requests SET request_status = 'satisfied', satisfied_at = now() WHERE id IN ($1, $2)",
    )
    .bind(request_a.id)
    .bind(request_b.id)
    .execute(client.pool())
    .await?;

    let binding_count_before: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_vault_bindings WHERE family_id = $1 AND vault_id = $2",
    )
    .bind(vault_family.id)
    .bind(vault_id.as_i64())
    .fetch_one(client.pool())
    .await?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_provisioning_requests SET request_status = 'requested', satisfied_at = NULL WHERE id = $1",
    )
    .bind(request_a.id)
    .execute(client.pool())
    .await?;
    let stale_a = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "cohort-stale-a",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("stale cohort A request was not leaseable"))?;
    let stale_plan = client
        .plan_lookup_table_provisioning_request(
            &cluster,
            stale_a.id,
            &request_lease(&stale_a)?,
            plan_policy(70_004),
        )
        .await?;
    ensure(
        matches!(
            stale_plan.vault_allocation,
            AtomicVaultAllocationResult::Existing { ref binding } if binding.id == binding_b.id
        ),
        "stale cohort A planning displaced the canonical A+B desired head",
    )?;
    let binding_count_after: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_vault_bindings WHERE family_id = $1 AND vault_id = $2",
    )
    .bind(vault_family.id)
    .bind(vault_id.as_i64())
    .fetch_one(client.pool())
    .await?;
    ensure(
        binding_count_after == binding_count_before,
        "stale cohort A retry created another binding revision",
    )?;

    // Cancellation is the explicit cohort retirement state. Both shared and
    // vault aggregate source queries exclude it; re-upserting the same immutable
    // requirements later reactivates that cohort.
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_provisioning_requests SET request_status = 'cancelled' WHERE id = $1",
    )
    .bind(request_a.id)
    .execute(client.pool())
    .await?;
    let active_vault_source_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_provisioning_requests request
        JOIN loyal_yield.lookup_table_provisioning_request_addresses address
          ON address.request_id = request.id AND address.semantic_class = 'vault'
        WHERE request.cluster = $1 AND request.vault_id = $2
          AND request.sealed_at IS NOT NULL AND request.request_status <> 'cancelled'
        "#,
    )
    .bind(&cluster)
    .bind(vault_id.as_i64())
    .fetch_one(client.pool())
    .await?;
    let active_shared_cohort_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(DISTINCT request.shared_manifest_id)
        FROM loyal_yield.lookup_table_provisioning_requests request
        WHERE request.cluster = $1 AND request.vault_id = $2
          AND request.sealed_at IS NOT NULL AND request.request_status <> 'cancelled'
        "#,
    )
    .bind(cluster)
    .bind(vault_id.as_i64())
    .fetch_one(client.pool())
    .await?;
    ensure(
        active_vault_source_count == vault_b.len() as i64 && active_shared_cohort_count == 1,
        "cancelled request cohort remained in shared or vault desired-state sources",
    )
}

async fn materialize_binding_manifest(
    client: &NeonSqlClient,
    binding: &LookupTableVaultBindingRecord,
    observed_slot: i64,
) -> VerifyResult<()> {
    let manifest = client
        .lookup_table_manifest(binding.manifest_id)
        .await?
        .ok_or_else(|| io::Error::other("binding manifest disappeared during materialization"))?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_operations SET operation_state = 'cancelled' WHERE route_lookup_table_id = $1 AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')",
    )
    .bind(binding.route_lookup_table_id)
    .execute(client.pool())
    .await?;
    let membership = manifest
        .addresses
        .iter()
        .enumerate()
        .map(|(ordinal, address)| LookupTableMembershipAddress {
            address: address.address.clone(),
            ordinal: ordinal as i32,
            added_operation_id: None,
            added_slot: observed_slot - 1,
            usable_after_slot: observed_slot,
            last_verified_slot: observed_slot,
            last_verified_at: Utc::now(),
        })
        .collect::<Vec<_>>();
    let table = client
        .reusable_lookup_table(binding.route_lookup_table_id)
        .await?
        .ok_or_else(|| io::Error::other("binding table disappeared during materialization"))?;
    let table = client
        .replace_confirmed_lookup_table_membership(
            table.id,
            table.mutation_epoch,
            table.mutation_epoch + 1,
            observed_slot,
            membership,
        )
        .await?;
    let table = client
        .mark_reusable_lookup_table_verification(
            table.id,
            table.mutation_epoch,
            LookupTableLifecycle::Preparing,
            LookupTableLifecycle::Warming,
            true,
            table.address_count,
            observed_slot,
        )
        .await?;
    client
        .mark_reusable_lookup_table_verification(
            table.id,
            table.mutation_epoch,
            LookupTableLifecycle::Warming,
            LookupTableLifecycle::Active,
            true,
            table.address_count,
            observed_slot,
        )
        .await?;
    Ok(())
}

async fn verify_manifest_and_capacity_constraints(
    client: &NeonSqlClient,
    fixture: &PlannedFixture,
) -> VerifyResult<()> {
    let manifest = client
        .lookup_table_manifest(fixture.vault_manifest_id)
        .await?
        .ok_or_else(|| io::Error::other("vault manifest missing"))?;
    let content_mutation = loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_manifests SET desired_set_hash = 'mutated' WHERE id = $1",
    )
    .bind(manifest.id)
    .execute(client.pool())
    .await;
    ensure(
        content_mutation.is_err(),
        "sealed manifest content remained mutable",
    )?;
    let address_mutation = loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.lookup_table_manifest_addresses
            (manifest_id, address, ordinal, semantic_class, account_role, is_writable)
        VALUES ($1, $2, $3, 'vault', 'late', FALSE)
        "#,
    )
    .bind(manifest.id)
    .bind(unique_pubkey("sealed-manifest-late-address").to_string())
    .bind(manifest.address_count)
    .execute(client.pool())
    .await;
    ensure(
        address_mutation.is_err(),
        "sealed manifest addresses remained mutable",
    )?;

    let duplicate_table = loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.route_lookup_tables
            (cluster, scope, table_address, authority, payer)
        VALUES ($1, 'duplicate', $2, $3, $3)
        "#,
    )
    .bind(&fixture.cluster)
    .bind(&fixture.vault_table.table_address)
    .bind(&fixture.vault_table.authority)
    .execute(client.pool())
    .await;
    ensure(
        duplicate_table.is_err(),
        "physical table address uniqueness was not enforced",
    )?;

    let overflow_manifest = client
        .persist_lookup_table_manifest(LookupTableManifestWrite {
            family_id: fixture.vault_family.id,
            subject_kind: LookupTableManifestSubject::Vault,
            subject_key: format!("overflow:{}", fixture.vault_id.as_i64()),
            vault_id: Some(fixture.vault_id),
            desired_set_hash: format!("overflow-hash-{}", fixture.request.id),
            source_slot: Some(2_000),
            planner_version: fixture.vault_family.planner_version.clone(),
            catalog_version: fixture.vault_family.catalog_version.clone(),
            addresses: typed_addresses(LookupTableManifestSubject::Vault, 1, "overflow"),
        })
        .await?;
    let capacity_error = client
        .insert_lookup_table_vault_binding(LookupTableVaultBindingInsert {
            vault_id: fixture.vault_id,
            family_id: fixture.vault_family.id,
            route_lookup_table_id: fixture.vault_table.id,
            manifest_id: overflow_manifest.id,
            binding_ordinal: 9,
            allocation_mode: LookupTableBindingMode::PackedShard,
            reserved_capacity: fixture.vault_table.allocation_high_water,
            predecessor_binding_id: None,
        })
        .await;
    ensure(
        capacity_error.is_err(),
        "binding trigger allowed reservation accounting past high water",
    )?;
    let table = client
        .reusable_lookup_table(fixture.vault_table.id)
        .await?
        .ok_or_else(|| io::Error::other("vault table missing after rejected reservation"))?;
    ensure(
        table.reserved_address_count == fixture.vault_binding.reserved_capacity,
        "failed capacity reservation changed durable accounting",
    )?;
    Ok(())
}

async fn verify_refresh_and_fencing(
    client: &NeonSqlClient,
    fixture: &PlannedFixture,
) -> VerifyResult<()> {
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_operations SET operation_state = 'cancelled' WHERE family_id = $1 AND id <> $2 AND operation_state = 'queued'",
    )
    .bind(fixture.shared_family.id)
    .bind(fixture.vault_operation.id)
    .execute(client.pool())
    .await?;
    let leased = client
        .lease_next_lookup_table_operation(
            &fixture.cluster,
            "slot-refresh-a",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .ok_or_else(|| io::Error::other("vault create operation was not leaseable"))?;
    ensure(
        leased.operation.id == fixture.vault_operation.id,
        "unexpected operation won the refresh lease",
    )?;
    let lease = operation_lease(&leased.operation)?;
    let fresh_slot = 20_000_u64;
    let authority = Pubkey::try_from(fixture.vault_table.authority.as_str())?;
    let collision_address = derive_lookup_table_address(&authority, fresh_slot).0;
    loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.route_lookup_tables
            (cluster, scope, table_address, authority, payer, status, durable,
             address_count, address_hash, addresses)
        VALUES ($1, 'refresh-collision', $2, $3, $3, 'usable', TRUE,
                0, '', '[]'::jsonb)
        "#,
    )
    .bind(&fixture.cluster)
    .bind(collision_address.to_string())
    .bind(authority.to_string())
    .execute(client.pool())
    .await?;
    let old_address = fixture.vault_table.table_address.clone();
    let refreshed = client
        .refresh_leased_lookup_table_create_reservation(leased.operation.id, &lease, fresh_slot)
        .await?;
    let refreshed_table = refreshed
        .physical_table
        .as_ref()
        .ok_or_else(|| io::Error::other("refreshed operation lost physical table"))?;
    ensure(
        refreshed.operation.id == leased.operation.id
            && refreshed_table.id == fixture.vault_table.id
            && refreshed_table.table_address != old_address
            && refreshed_table.table_address
                == derive_lookup_table_address(&authority, fresh_slot - 1)
                    .0
                    .to_string()
            && refreshed.operation.operation_context["recent_slot"] == json!(fresh_slot - 1),
        "fresh-slot rekey changed IDs or ignored a durable address collision",
    )?;
    let binding = client
        .lookup_table_vault_bindings(fixture.vault_id, fixture.vault_family.id)
        .await?
        .into_iter()
        .find(|binding| binding.id == fixture.vault_binding.id)
        .ok_or_else(|| io::Error::other("binding disappeared after rekey"))?;
    ensure(
        binding.route_lookup_table_id == refreshed_table.id,
        "fresh-slot rekey changed binding identity",
    )?;

    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_operations SET lease_expires_at = now() - interval '1 second' WHERE id = $1",
    )
    .bind(leased.operation.id)
    .execute(client.pool())
    .await?;
    let second = client
        .lease_next_lookup_table_operation(
            &fixture.cluster,
            "slot-refresh-b",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .ok_or_else(|| io::Error::other("expired operation was not re-leased"))?;
    let second_lease = operation_lease(&second.operation)?;
    ensure(
        second.operation.fencing_token > leased.operation.fencing_token,
        "operation fencing token did not advance",
    )?;
    ensure(
        client
            .refresh_leased_lookup_table_create_reservation(
                leased.operation.id,
                &lease,
                fresh_slot + 100,
            )
            .await
            .is_err(),
        "stale operation lease rekeyed a reservation",
    )?;
    client
        .persist_signed_lookup_table_transaction(
            second.operation.id,
            &second_lease,
            SignedLookupTableTransaction {
                transaction_signature: "db-verifier-signature".to_owned(),
                message_hash: "db-verifier-message".to_owned(),
                recent_blockhash: "db-verifier-blockhash".to_owned(),
                last_valid_block_height: 99_999,
                estimated_fee_lamports: 5_000,
                estimated_rent_lamports: 10_000,
                estimated_reclaimed_rent_lamports: 0,
            },
        )
        .await?;
    ensure(
        client
            .refresh_leased_lookup_table_create_reservation(
                second.operation.id,
                &second_lease,
                fresh_slot + 200,
            )
            .await
            .is_err(),
        "signed create operation remained rekeyable",
    )?;
    let retried = client
        .retry_lookup_table_operation(
            second.operation.id,
            &second_lease,
            LookupTableOperationStatus::Signed,
            Utc::now() - Duration::seconds(1),
            "expired_slot_hashes",
            "db verifier retry",
        )
        .await?;
    ensure(
        retried.transaction_signature.is_none()
            && retried.message_hash.is_none()
            && retried.recent_blockhash.is_none()
            && retried.last_valid_block_height.is_none()
            && retried
                .operation_context
                .get("attempt_history")
                .and_then(Value::as_array)
                .is_some_and(|history| {
                    history.last().and_then(|attempt| {
                        attempt.get("transactionSignature").and_then(Value::as_str)
                    }) == Some("db-verifier-signature")
                }),
        "retry did not archive and clear the prior signed attempt identity",
    )?;
    let third = client
        .lease_next_lookup_table_operation(
            &fixture.cluster,
            "slot-refresh-c",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .ok_or_else(|| io::Error::other("cleared retry was not re-leased"))?;
    let third_lease = operation_lease(&third.operation)?;
    let rekeyed_retry = client
        .refresh_leased_lookup_table_create_reservation(
            third.operation.id,
            &third_lease,
            fresh_slot + 300,
        )
        .await?;
    ensure(
        rekeyed_retry.operation.transaction_signature.is_none(),
        "cleared retry could not be rekeyed for a fresh signature",
    )?;
    client
        .advance_lookup_table_operation(
            rekeyed_retry.operation.id,
            &third_lease,
            LookupTableOperationAdvance {
                expected_state: LookupTableOperationStatus::Leased,
                next_state: LookupTableOperationStatus::NeedsReconcile,
                observed_slot: None,
                error_code: Some("authority_drift".to_owned()),
                error_detail: Some(
                    "observed drift via https://user:secret@example.invalid/path".to_owned(),
                ),
                actual_fee_lamports: None,
                actual_rent_lamports: None,
                reclaimed_rent_lamports: None,
            },
        )
        .await?;
    Ok(())
}

async fn verify_operation_metadata_constraints(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<()> {
    let cluster = format!("db-verify-metadata-{run}");
    let authority = unique_pubkey("metadata-authority").to_string();
    let family = create_family(
        client,
        &cluster,
        "metadata-shared",
        LookupTableFamilyKind::SharedMarket,
        &authority,
        64,
        Some(1),
    )
    .await?;
    let table = insert_table(
        client,
        &cluster,
        &family,
        LookupTableAllocationKind::SharedMarket,
        2,
        0,
        LookupTableLifecycle::Preparing,
        false,
        derive_lookup_table_address(&Pubkey::try_from(authority.as_str())?, 30_000)
            .0
            .to_string(),
    )
    .await?;
    ensure(
        client
            .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
                idempotency_key: format!("metadata-unreserved-create-{run}"),
                family_id: family.id,
                route_lookup_table_id: None,
                manifest_id: None,
                binding_id: None,
                operation_kind: LookupTableOperationKind::Create,
                target_generation: Some(3),
                target_shard_ordinal: Some(0),
                operation_context: json!({"recent_slot": 30_001}),
                mutation_epoch: 0,
                estimated_fee_lamports: None,
                estimated_rent_lamports: None,
                addresses: Vec::new(),
            })
            .await
            .is_err(),
        "create operation was accepted without an atomic physical reservation",
    )?;
    let operation = client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("metadata-create-{run}"),
            family_id: family.id,
            route_lookup_table_id: Some(table.id),
            manifest_id: None,
            binding_id: None,
            operation_kind: LookupTableOperationKind::Create,
            target_generation: Some(table.generation),
            target_shard_ordinal: Some(table.shard_ordinal),
            operation_context: json!({"recent_slot": 30_000}),
            mutation_epoch: table.mutation_epoch,
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            addresses: Vec::new(),
        })
        .await?;
    let leased = client
        .lease_next_lookup_table_operation(
            &cluster,
            "metadata-worker",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .ok_or_else(|| io::Error::other("metadata operation was not leased"))?;
    let lease = operation_lease(&leased.operation)?;
    let reconcile = client
        .advance_lookup_table_operation(
            operation.id,
            &lease,
            LookupTableOperationAdvance {
                expected_state: LookupTableOperationStatus::Leased,
                next_state: LookupTableOperationStatus::NeedsReconcile,
                observed_slot: None,
                error_code: Some("unsigned_chain_drift".to_owned()),
                error_detail: Some("db verifier".to_owned()),
                actual_fee_lamports: None,
                actual_rent_lamports: None,
                reclaimed_rent_lamports: None,
            },
        )
        .await?;
    ensure(
        reconcile.transaction_signature.is_none()
            && reconcile.operation_state == LookupTableOperationStatus::NeedsReconcile,
        "unsigned leased mutation could not enter manual reconcile",
    )?;
    let unsigned_submitted = loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_operations SET operation_state = 'submitted' WHERE id = $1",
    )
    .bind(operation.id)
    .execute(client.pool())
    .await;
    ensure(
        unsigned_submitted.is_err(),
        "database allowed submitted mutation state without durable signed metadata",
    )?;
    Ok(())
}

async fn verify_attempt_failure_policy(client: &NeonSqlClient, run: &str) -> VerifyResult<()> {
    let cluster = format!("db-verify-attempt-failure-{run}");
    let authority = unique_pubkey("attempt-failure-authority").to_string();
    let family = create_family(
        client,
        &cluster,
        "attempt-failure-shared",
        LookupTableFamilyKind::SharedMarket,
        &authority,
        52,
        Some(1),
    )
    .await?;
    let first_table = insert_table(
        client,
        &cluster,
        &family,
        LookupTableAllocationKind::SharedMarket,
        2,
        0,
        LookupTableLifecycle::Preparing,
        false,
        derive_lookup_table_address(&Pubkey::try_from(authority.as_str())?, 31_000)
            .0
            .to_string(),
    )
    .await?;
    let first_operation = client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("attempt-failure-unsigned-{run}"),
            family_id: family.id,
            route_lookup_table_id: Some(first_table.id),
            manifest_id: None,
            binding_id: None,
            operation_kind: LookupTableOperationKind::Create,
            target_generation: Some(first_table.generation),
            target_shard_ordinal: Some(first_table.shard_ordinal),
            operation_context: json!({"recent_slot": 31_000}),
            mutation_epoch: first_table.mutation_epoch,
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            addresses: Vec::new(),
        })
        .await?;
    let first_lease = client
        .lease_next_lookup_table_operation(
            &cluster,
            "attempt-worker-a",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .ok_or_else(|| io::Error::other("unsigned failure operation was not leased"))?;
    ensure(
        first_lease.operation.id == first_operation.id,
        "unexpected unsigned failure operation was leased",
    )?;
    let retry_wait = client
        .record_lookup_table_operation_attempt_failure(
            first_operation.id,
            &operation_lease(&first_lease.operation)?,
            Utc::now() - Duration::seconds(1),
            2,
            "transient_rpc",
            "redacted transient failure",
        )
        .await?;
    ensure(
        retry_wait.operation_state == LookupTableOperationStatus::RetryWait
            && retry_wait.lease_owner.is_none()
            && retry_wait.lease_expires_at.is_none(),
        "first unsigned failure did not release its lease into retry_wait",
    )?;
    let second_lease = client
        .lease_next_lookup_table_operation(
            &cluster,
            "attempt-worker-b",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .ok_or_else(|| io::Error::other("retry_wait operation was not re-leased"))?;
    let exhausted = client
        .record_lookup_table_operation_attempt_failure(
            first_operation.id,
            &operation_lease(&second_lease.operation)?,
            Utc::now() + Duration::minutes(1),
            2,
            "retry_exhausted",
            "redacted exhausted failure",
        )
        .await?;
    ensure(
        exhausted.operation_state == LookupTableOperationStatus::PermanentFailure
            && exhausted.attempt_count == 2
            && exhausted.lease_owner.is_none(),
        "max-attempt unsigned failure was not permanently failed",
    )?;

    let signed_table = insert_table(
        client,
        &cluster,
        &family,
        LookupTableAllocationKind::SharedMarket,
        2,
        1,
        LookupTableLifecycle::Preparing,
        false,
        derive_lookup_table_address(&Pubkey::try_from(authority.as_str())?, 30_999)
            .0
            .to_string(),
    )
    .await?;
    let signed_operation = client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("attempt-failure-signed-{run}"),
            family_id: family.id,
            route_lookup_table_id: Some(signed_table.id),
            manifest_id: None,
            binding_id: None,
            operation_kind: LookupTableOperationKind::Create,
            target_generation: Some(signed_table.generation),
            target_shard_ordinal: Some(signed_table.shard_ordinal),
            operation_context: json!({"recent_slot": 30_999}),
            mutation_epoch: signed_table.mutation_epoch,
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            addresses: Vec::new(),
        })
        .await?;
    let signed_lease = client
        .lease_next_lookup_table_operation(
            &cluster,
            "attempt-worker-signed",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .ok_or_else(|| io::Error::other("signed failure operation was not leased"))?;
    ensure(
        signed_lease.operation.id == signed_operation.id,
        "unexpected signed failure operation was leased",
    )?;
    let lease = operation_lease(&signed_lease.operation)?;
    let signed = client
        .persist_signed_lookup_table_transaction(
            signed_operation.id,
            &lease,
            SignedLookupTableTransaction {
                transaction_signature: format!("attempt-signature-{run}"),
                message_hash: format!("attempt-message-{run}"),
                recent_blockhash: format!("attempt-blockhash-{run}"),
                last_valid_block_height: 77_777,
                estimated_fee_lamports: 5_001,
                estimated_rent_lamports: 12_346,
                estimated_reclaimed_rent_lamports: 7,
            },
        )
        .await?;
    let reconcile = client
        .record_lookup_table_operation_attempt_failure(
            signed_operation.id,
            &lease,
            Utc::now() + Duration::seconds(30),
            1,
            "send_unknown",
            "redacted post-sign failure",
        )
        .await?;
    ensure(
        reconcile.operation_state == LookupTableOperationStatus::NeedsReconcile
            && reconcile.transaction_signature == signed.transaction_signature
            && reconcile.message_hash == signed.message_hash
            && reconcile.recent_blockhash == signed.recent_blockhash
            && reconcile.last_valid_block_height == signed.last_valid_block_height
            && reconcile.estimated_fee_lamports == Some(5_001)
            && reconcile.estimated_rent_lamports == Some(12_346)
            && reconcile.operation_context["signedExpectedReclaimedRentLamports"] == json!(7)
            && reconcile.lease_owner.is_none()
            && reconcile.lease_expires_at.is_none(),
        "post-sign failure crossed the durable signing boundary or retained its lease",
    )
}

async fn verify_reconciliation_deferral_and_family_lookup(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<()> {
    let cluster = format!("db-verify-reconcile-deferral-{run}");
    let authority = unique_pubkey("reconcile-deferral-authority").to_string();
    let family = create_family(
        client,
        &cluster,
        "reconcile-deferral-shared",
        LookupTableFamilyKind::SharedMarket,
        &authority,
        52,
        Some(1),
    )
    .await?;
    let mutation_table = insert_table(
        client,
        &cluster,
        &family,
        LookupTableAllocationKind::SharedMarket,
        2,
        0,
        LookupTableLifecycle::Preparing,
        false,
        derive_lookup_table_address(&Pubkey::try_from(authority.as_str())?, 32_000)
            .0
            .to_string(),
    )
    .await?;
    let verify_table = insert_table(
        client,
        &cluster,
        &family,
        LookupTableAllocationKind::SharedMarket,
        1,
        0,
        LookupTableLifecycle::Active,
        false,
        unique_pubkey("reconcile-deferral-verify-table").to_string(),
    )
    .await?;
    set_verified_empty(client, verify_table.id).await?;
    let mutation = client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("reconcile-deferral-mutation-{run}"),
            family_id: family.id,
            route_lookup_table_id: Some(mutation_table.id),
            manifest_id: None,
            binding_id: None,
            operation_kind: LookupTableOperationKind::Create,
            target_generation: Some(mutation_table.generation),
            target_shard_ordinal: Some(mutation_table.shard_ordinal),
            operation_context: json!({"recent_slot": 32_000}),
            mutation_epoch: mutation_table.mutation_epoch,
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            addresses: Vec::new(),
        })
        .await?;
    let verify = client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("reconcile-deferral-verify-{run}"),
            family_id: family.id,
            route_lookup_table_id: Some(verify_table.id),
            manifest_id: None,
            binding_id: None,
            operation_kind: LookupTableOperationKind::Verify,
            target_generation: None,
            target_shard_ordinal: None,
            operation_context: json!({"source": "db_verifier_reconcile_deferral"}),
            mutation_epoch: verify_table.mutation_epoch,
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            addresses: Vec::new(),
        })
        .await?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_families SET desired_state = 'paused' WHERE id = $1",
    )
    .bind(family.id)
    .execute(client.pool())
    .await?;
    let paused = client
        .lookup_table_family_by_id(family.id)
        .await?
        .ok_or_else(|| io::Error::other("paused family lookup returned none"))?;
    ensure(
        paused.desired_state == LookupTableFamilyState::Paused,
        "family-by-id lookup filtered a paused family",
    )?;
    let leased = client
        .lease_next_lookup_table_operation(
            &cluster,
            "reconcile-deferral-worker",
            Utc::now() + Duration::minutes(5),
            true,
        )
        .await?
        .ok_or_else(|| io::Error::other("reconcile-only worker did not lease unsigned Verify"))?;
    ensure(
        leased.operation.id == verify.id
            && leased.operation.id != mutation.id
            && leased.operation.attempt_count == 1,
        "reconcile-only worker selected an unsigned mutation or miscounted the Verify lease",
    )?;
    let deferred = client
        .defer_unsigned_lookup_table_operation_without_attempt(
            verify.id,
            &operation_lease(&leased.operation)?,
            Utc::now() - Duration::seconds(1),
            "family_paused",
            "paused family gate; no RPC or signing attempted",
        )
        .await?;
    ensure(
        deferred.operation_state == LookupTableOperationStatus::RetryWait
            && deferred.attempt_count == 0
            && deferred.lease_owner.is_none()
            && deferred.transaction_signature.is_none()
            && deferred.message_hash.is_none()
            && deferred.recent_blockhash.is_none()
            && deferred.last_valid_block_height.is_none(),
        "attempt-neutral unsigned deferral did not restore the lease attempt exactly once",
    )?;
    let re_leased = client
        .lease_next_lookup_table_operation(
            &cluster,
            "reconcile-deferral-worker-b",
            Utc::now() + Duration::minutes(5),
            true,
        )
        .await?
        .ok_or_else(|| io::Error::other("deferred Verify was not re-leased"))?;
    ensure(
        re_leased.operation.id == verify.id && re_leased.operation.attempt_count == 1,
        "deferred Verify did not consume exactly one fresh attempt on re-lease",
    )?;
    client
        .advance_lookup_table_operation(
            verify.id,
            &operation_lease(&re_leased.operation)?,
            LookupTableOperationAdvance {
                expected_state: LookupTableOperationStatus::Leased,
                next_state: LookupTableOperationStatus::NeedsReconcile,
                observed_slot: None,
                error_code: Some("db_verifier_complete".to_owned()),
                error_detail: Some("deferral verifier finished".to_owned()),
                actual_fee_lamports: None,
                actual_rent_lamports: None,
                reclaimed_rent_lamports: None,
            },
        )
        .await?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_families SET desired_state = 'retiring' WHERE id = $1",
    )
    .bind(family.id)
    .execute(client.pool())
    .await?;
    let retiring = client
        .lookup_table_family_by_id(family.id)
        .await?
        .ok_or_else(|| io::Error::other("retiring family lookup returned none"))?;
    ensure(
        retiring.desired_state == LookupTableFamilyState::Retiring,
        "family-by-id lookup filtered a retiring family",
    )
}

async fn verify_crash_after_send_accounting(client: &NeonSqlClient, run: &str) -> VerifyResult<()> {
    let cluster = format!("db-verify-accounting-{run}");
    let authority = unique_pubkey("accounting-authority").to_string();
    let family = create_family(
        client,
        &cluster,
        "accounting-shared",
        LookupTableFamilyKind::SharedMarket,
        &authority,
        64,
        Some(1),
    )
    .await?;
    let table = insert_table(
        client,
        &cluster,
        &family,
        LookupTableAllocationKind::SharedMarket,
        2,
        0,
        LookupTableLifecycle::Preparing,
        false,
        derive_lookup_table_address(&Pubkey::try_from(authority.as_str())?, 40_000)
            .0
            .to_string(),
    )
    .await?;
    let operation = client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("accounting-create-{run}"),
            family_id: family.id,
            route_lookup_table_id: Some(table.id),
            manifest_id: None,
            binding_id: None,
            operation_kind: LookupTableOperationKind::Create,
            target_generation: Some(table.generation),
            target_shard_ordinal: Some(table.shard_ordinal),
            operation_context: json!({"recent_slot": 40_000}),
            mutation_epoch: 0,
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            addresses: Vec::new(),
        })
        .await?;
    let leased = client
        .lease_next_lookup_table_operation(
            &cluster,
            "accounting-before-send",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .ok_or_else(|| io::Error::other("accounting operation was not leased"))?;
    let signed = client
        .persist_signed_lookup_table_transaction(
            operation.id,
            &operation_lease(&leased.operation)?,
            SignedLookupTableTransaction {
                transaction_signature: "accounting-signature".to_owned(),
                message_hash: "accounting-message".to_owned(),
                recent_blockhash: "accounting-blockhash".to_owned(),
                last_valid_block_height: 123_456,
                estimated_fee_lamports: 5_000,
                estimated_rent_lamports: 12_345,
                estimated_reclaimed_rent_lamports: 0,
            },
        )
        .await?;
    let persisted = persisted_lookup_table_success_accounting(&signed)?;
    ensure(
        persisted.actual_fee_lamports == 5_000
            && persisted.actual_rent_lamports == 12_345
            && persisted.reclaimed_rent_lamports == 0,
        "signed transaction did not durably persist deterministic accounting",
    )?;
    let mut missing_accounting = signed.clone();
    missing_accounting.estimated_fee_lamports = None;
    ensure(
        persisted_lookup_table_success_accounting(&missing_accounting).is_err(),
        "missing signed accounting could be promoted to claimed actual spend",
    )?;

    // Simulate process death after send: expire the old worker lease and let a
    // new reconciler continue from the durable Signed state.
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_operations SET lease_expires_at = now() - interval '1 second' WHERE id = $1",
    )
    .bind(operation.id)
    .execute(client.pool())
    .await?;
    let recovered = client
        .lease_next_lookup_table_operation(
            &cluster,
            "accounting-reconciler",
            Utc::now() + Duration::minutes(5),
            true,
        )
        .await?
        .ok_or_else(|| io::Error::other("signed operation was not recoverable"))?;
    ensure(
        recovered.operation.operation_state == LookupTableOperationStatus::Signed,
        "crash recovery lost the durable Signed state",
    )?;
    let lease = operation_lease(&recovered.operation)?;
    let mut current = LookupTableOperationStatus::Signed;
    let states = [
        LookupTableOperationStatus::Submitted,
        LookupTableOperationStatus::Confirmed,
        LookupTableOperationStatus::Finalized,
        LookupTableOperationStatus::Reconciled,
        LookupTableOperationStatus::Complete,
    ];
    let mut completed = recovered.operation;
    for next in states {
        completed = client
            .advance_lookup_table_operation(
                operation.id,
                &lease,
                LookupTableOperationAdvance {
                    expected_state: current,
                    next_state: next,
                    observed_slot: Some(50_000),
                    error_code: None,
                    error_detail: None,
                    actual_fee_lamports: (next == LookupTableOperationStatus::Complete)
                        .then_some(persisted.actual_fee_lamports),
                    actual_rent_lamports: (next == LookupTableOperationStatus::Complete)
                        .then_some(persisted.actual_rent_lamports),
                    reclaimed_rent_lamports: (next == LookupTableOperationStatus::Complete)
                        .then_some(persisted.reclaimed_rent_lamports),
                },
            )
            .await?;
        current = next;
    }
    ensure(
        completed.operation_state == LookupTableOperationStatus::Complete
            && completed.actual_fee_lamports == Some(5_000)
            && completed.actual_rent_lamports == Some(12_345)
            && completed.reclaimed_rent_lamports == Some(0),
        "crash-after-send reconciliation completed without durable accounting",
    )?;
    Ok(())
}

async fn verify_cleanup_usage_exclusion(client: &NeonSqlClient, run: &str) -> VerifyResult<()> {
    let cluster = format!("db-verify-cleanup-{run}");
    let authority = unique_pubkey("cleanup-authority").to_string();
    let family = create_family(
        client,
        &cluster,
        "cleanup-shared",
        LookupTableFamilyKind::SharedMarket,
        &authority,
        64,
        Some(1),
    )
    .await?;
    let table = cleanup_table(client, &cluster, &family, 10, 0).await?;
    let leases = client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::RouteResolution,
            reference_key: format!("cleanup-lease-{run}"),
            route_lookup_table_ids: vec![table.id],
            vault_id: None,
            binding_id: None,
            route_fingerprint: Some(format!("cleanup-route-{run}")),
            requirements_fingerprint: Some(format!("cleanup-requirements-{run}")),
            expires_at: Utc::now() + Duration::minutes(5),
        })
        .await?;
    ensure(
        leases.len() == 1,
        "cleanup fixture usage lease was not recorded",
    )?;
    let cleanup = cleanup_operation(&cluster, &table, format!("cleanup-first-{run}"));
    ensure(
        client
            .enqueue_lookup_table_operation(cleanup.clone())
            .await
            .is_err(),
        "cleanup enqueue raced past an existing usage lease",
    )?;
    client
        .release_lookup_table_usage_leases(
            LookupTableUsageLeaseKind::RouteResolution,
            &format!("cleanup-lease-{run}"),
        )
        .await?;
    let queued = client.enqueue_lookup_table_operation(cleanup).await?;
    ensure(
        client
            .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
                cluster: cluster.clone(),
                lease_kind: LookupTableUsageLeaseKind::RouteResolution,
                reference_key: format!("after-cleanup-{run}"),
                route_lookup_table_ids: vec![table.id],
                vault_id: None,
                binding_id: None,
                route_fingerprint: None,
                requirements_fingerprint: None,
                expires_at: Utc::now() + Duration::minutes(5),
            })
            .await
            .is_err(),
        "usage lease raced past an existing cleanup operation",
    )?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_operations SET operation_state = 'cancelled' WHERE id = $1",
    )
    .bind(queued.id)
    .execute(client.pool())
    .await?;

    // Real concurrent races must serialize on the physical row. Exactly one
    // side may commit on every fresh table.
    for attempt in 0..6_i32 {
        let table = cleanup_table(client, &cluster, &family, 20 + attempt, 0).await?;
        let barrier = Arc::new(Barrier::new(3));
        let lease_task = {
            let client = client.clone();
            let cluster = cluster.clone();
            let barrier = barrier.clone();
            let reference = format!("race-lease-{run}-{attempt}");
            tokio::spawn(async move {
                barrier.wait().await;
                client
                    .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
                        cluster,
                        lease_kind: LookupTableUsageLeaseKind::RouteResolution,
                        reference_key: reference,
                        route_lookup_table_ids: vec![table.id],
                        vault_id: None,
                        binding_id: None,
                        route_fingerprint: None,
                        requirements_fingerprint: None,
                        expires_at: Utc::now() + Duration::minutes(5),
                    })
                    .await
            })
        };
        let cleanup_task = {
            let client = client.clone();
            let cluster = cluster.clone();
            let barrier = barrier.clone();
            let table = table.clone();
            let idempotency_key = format!("race-cleanup-{run}-{attempt}");
            tokio::spawn(async move {
                barrier.wait().await;
                client
                    .enqueue_lookup_table_operation(cleanup_operation(
                        &cluster,
                        &table,
                        idempotency_key,
                    ))
                    .await
            })
        };
        barrier.wait().await;
        let lease_result = lease_task.await?;
        let cleanup_result = cleanup_task.await?;
        ensure(
            usize::from(lease_result.is_ok()) + usize::from(cleanup_result.is_ok()) == 1,
            format!("cleanup/lease race {attempt} did not have exactly one winner"),
        )?;
    }
    Ok(())
}

async fn verify_atomic_binding_activation_fence(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<()> {
    let cluster = format!("db-verify-binding-fence-{run}");
    let authority = unique_pubkey("binding-fence-authority").to_string();
    let family = create_family(
        client,
        &cluster,
        "binding-fence-vaults",
        LookupTableFamilyKind::VaultShards,
        &authority,
        52,
        Some(0),
    )
    .await?;
    let vault_id = create_vault(client, &format!("binding-fence-{run}"), 11).await?;
    let manifest_addresses = typed_addresses(LookupTableManifestSubject::Vault, 1, "binding-fence");
    let manifest = client
        .persist_lookup_table_manifest(LookupTableManifestWrite {
            family_id: family.id,
            subject_kind: LookupTableManifestSubject::Vault,
            subject_key: format!("binding-fence-base-{run}"),
            vault_id: Some(vault_id),
            desired_set_hash: format!("binding-fence-base-hash-{run}"),
            source_slot: Some(120),
            planner_version: family.planner_version.clone(),
            catalog_version: family.catalog_version.clone(),
            addresses: manifest_addresses.clone(),
        })
        .await?;
    let table = insert_table(
        client,
        &cluster,
        &family,
        LookupTableAllocationKind::VaultShard,
        0,
        0,
        LookupTableLifecycle::Active,
        true,
        derive_lookup_table_address(&Pubkey::try_from(authority.as_str())?, 60_000)
            .0
            .to_string(),
    )
    .await?;
    let candidate = client
        .insert_lookup_table_vault_binding(LookupTableVaultBindingInsert {
            vault_id,
            family_id: family.id,
            route_lookup_table_id: table.id,
            manifest_id: manifest.id,
            binding_ordinal: 0,
            allocation_mode: LookupTableBindingMode::PackedShard,
            reserved_capacity: 3,
            predecessor_binding_id: None,
        })
        .await?;
    ensure(
        client
            .flip_lookup_table_binding_head(candidate.id, 120, Utc::now() + Duration::hours(1))
            .await
            .is_err(),
        "unverified physical table activated a binding head",
    )?;
    let table = client
        .replace_confirmed_lookup_table_membership(
            table.id,
            0,
            1,
            120,
            vec![LookupTableMembershipAddress {
                address: manifest_addresses[0].address.clone(),
                ordinal: 0,
                added_operation_id: None,
                added_slot: 120,
                usable_after_slot: 121,
                last_verified_slot: 120,
                last_verified_at: Utc::now(),
            }],
        )
        .await?;
    let table = client
        .mark_reusable_lookup_table_verification(
            table.id,
            table.mutation_epoch,
            LookupTableLifecycle::Active,
            LookupTableLifecycle::Active,
            true,
            1,
            120,
        )
        .await?;
    ensure(
        client
            .flip_lookup_table_binding_head(candidate.id, 120, Utc::now() + Duration::hours(1))
            .await
            .is_err(),
        "binding activation ignored the ALT warmup slot",
    )?;
    let pending = client
        .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
            idempotency_key: format!("binding-fence-pending-{run}"),
            family_id: family.id,
            route_lookup_table_id: Some(table.id),
            manifest_id: Some(manifest.id),
            binding_id: Some(candidate.id),
            operation_kind: LookupTableOperationKind::Verify,
            target_generation: None,
            target_shard_ordinal: None,
            operation_context: json!({"source": "db_verifier_binding_fence"}),
            mutation_epoch: table.mutation_epoch,
            estimated_fee_lamports: None,
            estimated_rent_lamports: None,
            addresses: Vec::new(),
        })
        .await?;
    ensure(
        client
            .flip_lookup_table_binding_head(candidate.id, 121, Utc::now() + Duration::hours(1))
            .await
            .is_err(),
        "binding activation ignored a conflicting pending operation",
    )?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_operations SET operation_state = 'cancelled' WHERE id = $1",
    )
    .bind(pending.id)
    .execute(client.pool())
    .await?;
    let active = client
        .flip_lookup_table_binding_head(candidate.id, 121, Utc::now() + Duration::hours(1))
        .await?
        .active;

    let mut contenders = Vec::new();
    for label in ["a", "b"] {
        let contender_manifest = client
            .persist_lookup_table_manifest(LookupTableManifestWrite {
                family_id: family.id,
                subject_kind: LookupTableManifestSubject::Vault,
                subject_key: format!("binding-fence-contender-{label}-{run}"),
                vault_id: Some(vault_id),
                desired_set_hash: format!("binding-fence-contender-hash-{label}-{run}"),
                source_slot: Some(121),
                planner_version: family.planner_version.clone(),
                catalog_version: family.catalog_version.clone(),
                addresses: manifest_addresses.clone(),
            })
            .await?;
        contenders.push(
            client
                .insert_lookup_table_vault_binding(LookupTableVaultBindingInsert {
                    vault_id,
                    family_id: family.id,
                    route_lookup_table_id: table.id,
                    manifest_id: contender_manifest.id,
                    binding_ordinal: 0,
                    allocation_mode: LookupTableBindingMode::PackedShard,
                    reserved_capacity: 3,
                    predecessor_binding_id: Some(active.id),
                })
                .await?,
        );
    }
    let flip_lease_key = format!("binding-flip-lease-wins-{run}");
    client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
            reference_key: flip_lease_key.clone(),
            route_lookup_table_ids: vec![table.id],
            vault_id: Some(vault_id),
            binding_id: Some(active.id),
            route_fingerprint: Some(format!("binding-flip-route-{run}")),
            requirements_fingerprint: Some(format!("binding-flip-requirements-{run}")),
            expires_at: Utc::now() + Duration::minutes(5),
        })
        .await?;
    ensure(
        client
            .flip_lookup_table_binding_head(contenders[1].id, 122, Utc::now() + Duration::hours(1))
            .await
            .is_err(),
        "binding head flip ignored a live predecessor usage lease",
    )?;
    client
        .release_lookup_table_usage_leases(
            LookupTableUsageLeaseKind::PreparedTransaction,
            &flip_lease_key,
        )
        .await?;
    let barrier = Arc::new(Barrier::new(3));
    let first = {
        let client = client.clone();
        let barrier = barrier.clone();
        let id = contenders[0].id;
        tokio::spawn(async move {
            barrier.wait().await;
            client
                .flip_lookup_table_binding_head(id, 122, Utc::now() + Duration::hours(1))
                .await
        })
    };
    let second = {
        let client = client.clone();
        let barrier = barrier.clone();
        let id = contenders[1].id;
        tokio::spawn(async move {
            barrier.wait().await;
            client
                .flip_lookup_table_binding_head(id, 122, Utc::now() + Duration::hours(1))
                .await
        })
    };
    barrier.wait().await;
    let first = first.await?;
    let second = second.await?;
    ensure(
        usize::from(first.is_ok()) + usize::from(second.is_ok()) == 1,
        "concurrent binding head activation did not have exactly one predecessor-fenced winner",
    )?;
    let active_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.lookup_table_vault_bindings
        WHERE vault_id = $1 AND family_id = $2 AND binding_ordinal = 0
          AND lifecycle_state = 'active'
        "#,
    )
    .bind(vault_id.as_i64())
    .bind(family.id)
    .fetch_one(client.pool())
    .await?;
    ensure(
        active_count == 1,
        "binding activation race left an ambiguous active head",
    )?;
    let contender_rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, lifecycle_state, desired_head_revision
        FROM loyal_yield.lookup_table_vault_bindings
        WHERE id = ANY($1) ORDER BY id
        "#,
    )
    .bind(&contenders.iter().map(|row| row.id).collect::<Vec<_>>())
    .fetch_all(client.pool())
    .await?;
    let desired = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT manifest_id, desired_revision
        FROM loyal_yield.lookup_table_vault_desired_heads
        WHERE family_id = $1 AND vault_id = $2 AND binding_ordinal = 0
        "#,
    )
    .bind(family.id)
    .bind(vault_id.as_i64())
    .fetch_one(client.pool())
    .await?;
    let reserved: i32 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT reserved_address_count FROM loyal_yield.route_lookup_tables WHERE id = $1",
    )
    .bind(table.id)
    .fetch_one(client.pool())
    .await?;
    ensure(
        contender_rows[0].try_get::<String, _>("lifecycle_state")? == "failed"
            && contender_rows[1].try_get::<String, _>("lifecycle_state")? == "active"
            && desired.try_get::<i64, _>("manifest_id")? == contenders[1].manifest_id
            && desired.try_get::<i64, _>("desired_revision")?
                == contenders[1].desired_head_revision
            && reserved == contenders[1].reserved_capacity,
        "newest desired binding did not win or superseded reservations leaked",
    )?;
    ensure(
        client
            .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
                cluster: cluster.clone(),
                lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
                reference_key: format!("binding-flip-wins-{run}"),
                route_lookup_table_ids: vec![table.id],
                vault_id: Some(vault_id),
                binding_id: Some(active.id),
                route_fingerprint: Some(format!("binding-stale-route-{run}")),
                requirements_fingerprint: Some(format!("binding-stale-requirements-{run}")),
                expires_at: Utc::now() + Duration::minutes(5),
            })
            .await
            .is_err(),
        "usage lease accepted the binding that a completed head flip had demoted",
    )?;
    ensure(
        client
            .flip_lookup_table_binding_head(contenders[0].id, 123, Utc::now() + Duration::hours(1))
            .await
            .is_err(),
        "superseded older binding reactivated after the newer head won",
    )
}

async fn verify_rollbacks_and_finalization(client: &NeonSqlClient, run: &str) -> VerifyResult<()> {
    let cluster = format!("db-verify-rollback-{run}");
    let authority = unique_pubkey("rollback-authority").to_string();
    let family = create_family(
        client,
        &cluster,
        "rollback-vaults",
        LookupTableFamilyKind::VaultShards,
        &authority,
        64,
        Some(1),
    )
    .await?;
    let vault_id = create_vault(client, &format!("rollback-{run}"), 2).await?;
    let table_one = insert_table(
        client,
        &cluster,
        &family,
        LookupTableAllocationKind::VaultShard,
        1,
        0,
        LookupTableLifecycle::Active,
        true,
        unique_pubkey("rollback-table-one").to_string(),
    )
    .await?;
    let table_two = insert_table(
        client,
        &cluster,
        &family,
        LookupTableAllocationKind::VaultShard,
        2,
        0,
        LookupTableLifecycle::Active,
        true,
        unique_pubkey("rollback-table-two").to_string(),
    )
    .await?;
    set_verified_empty(client, table_one.id).await?;
    set_verified_empty(client, table_two.id).await?;
    let manifest_one = empty_vault_manifest(client, &family, vault_id, format!("m1-{run}")).await?;
    let manifest_two = empty_vault_manifest(client, &family, vault_id, format!("m2-{run}")).await?;
    let binding_one = client
        .insert_lookup_table_vault_binding(LookupTableVaultBindingInsert {
            vault_id,
            family_id: family.id,
            route_lookup_table_id: table_one.id,
            manifest_id: manifest_one.id,
            binding_ordinal: 0,
            allocation_mode: LookupTableBindingMode::PackedShard,
            reserved_capacity: 4,
            predecessor_binding_id: None,
        })
        .await?;
    let binding_one = client
        .flip_lookup_table_binding_head(binding_one.id, 10, Utc::now() + Duration::hours(1))
        .await?
        .active;
    let binding_two = client
        .insert_lookup_table_vault_binding(LookupTableVaultBindingInsert {
            vault_id,
            family_id: family.id,
            route_lookup_table_id: table_two.id,
            manifest_id: manifest_two.id,
            binding_ordinal: 0,
            allocation_mode: LookupTableBindingMode::PackedShard,
            reserved_capacity: 6,
            predecessor_binding_id: Some(binding_one.id),
        })
        .await?;
    let generation_lease_key = format!("generation-lease-wins-{run}");
    client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
            reference_key: generation_lease_key.clone(),
            route_lookup_table_ids: vec![table_one.id],
            vault_id: Some(vault_id),
            binding_id: Some(binding_one.id),
            route_fingerprint: Some(format!("generation-route-{run}")),
            requirements_fingerprint: Some(format!("generation-requirements-{run}")),
            expires_at: Utc::now() + Duration::minutes(5),
        })
        .await?;
    ensure(
        client
            .activate_lookup_table_family_generation(family.id, 2, Utc::now() + Duration::hours(1))
            .await
            .is_err(),
        "family generation activation ignored a live predecessor usage lease",
    )?;
    client
        .release_lookup_table_usage_leases(
            LookupTableUsageLeaseKind::PreparedTransaction,
            &generation_lease_key,
        )
        .await?;
    let activated = client
        .activate_lookup_table_family_generation(family.id, 2, Utc::now() + Duration::hours(1))
        .await?;
    ensure(
        activated.active_generation == Some(2) && activated.previous_generation == Some(1),
        "family activation did not preserve rollback pointers",
    )?;
    ensure(
        client
            .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
                cluster: cluster.clone(),
                lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
                reference_key: format!("generation-activation-wins-{run}"),
                route_lookup_table_ids: vec![table_one.id],
                vault_id: Some(vault_id),
                binding_id: Some(binding_one.id),
                route_fingerprint: Some(format!("generation-old-route-{run}")),
                requirements_fingerprint: Some(format!("generation-old-requirements-{run}")),
                expires_at: Utc::now() + Duration::minutes(5),
            })
            .await
            .is_err(),
        "usage lease acquired the generation that activation had made standby",
    )?;
    let binding_flip = client
        .flip_lookup_table_binding_head(binding_two.id, 20, Utc::now() + Duration::hours(1))
        .await?;
    ensure(
        binding_flip.predecessor.as_ref().map(|row| row.id) == Some(binding_one.id),
        "binding activation did not preserve predecessor",
    )?;
    let retry_bootstrap = client
        .create_or_validate_lookup_table_family(LookupTableFamilyUpsert {
            cluster: cluster.clone(),
            logical_name: family.logical_name.clone(),
            kind: family.kind,
            desired_state: LookupTableFamilyState::Paused,
            planner_version: family.planner_version.clone(),
            catalog_version: family.catalog_version.clone(),
            active_generation: Some(999),
            previous_generation: Some(998),
            rollback_until: None,
            provisioning_authority: family.provisioning_authority.clone(),
            payer: family.payer.clone(),
            hard_capacity: family.hard_capacity,
            largest_atomic_expansion: family.largest_atomic_expansion,
            safety_margin: family.safety_margin,
            allocation_high_water: family.allocation_high_water,
        })
        .await?;
    ensure(
        retry_bootstrap.active_generation == Some(2)
            && retry_bootstrap.previous_generation == Some(1),
        "family bootstrap retry overwrote live generation pointers",
    )?;

    let rollback_lease_key = format!("rollback-lease-wins-{run}");
    client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::PreparedTransaction,
            reference_key: rollback_lease_key.clone(),
            route_lookup_table_ids: vec![table_two.id],
            vault_id: Some(vault_id),
            binding_id: Some(binding_flip.active.id),
            route_fingerprint: Some(format!("rollback-route-{run}")),
            requirements_fingerprint: Some(format!("rollback-requirements-{run}")),
            expires_at: Utc::now() + Duration::minutes(5),
        })
        .await?;
    ensure(
        client
            .rollback_lookup_table_family_generation(family.id)
            .await
            .is_err()
            && client
                .rollback_lookup_table_binding_head(binding_flip.active.id, 30)
                .await
                .is_err(),
        "family or binding rollback ignored a live current-head usage lease",
    )?;
    client
        .release_lookup_table_usage_leases(
            LookupTableUsageLeaseKind::PreparedTransaction,
            &rollback_lease_key,
        )
        .await?;

    let rolled_family = client
        .rollback_lookup_table_family_generation(family.id)
        .await?;
    ensure(
        rolled_family.active_generation == Some(1) && rolled_family.previous_generation == Some(2),
        "family rollback did not reactivate its standby predecessor",
    )?;
    let rolled_binding = client
        .rollback_lookup_table_binding_head(binding_flip.active.id, 30)
        .await?;
    ensure(
        rolled_binding.active.id == binding_one.id
            && rolled_binding.predecessor.as_ref().map(|row| row.id) == Some(binding_two.id),
        "binding rollback did not restore the predecessor head",
    )?;

    ensure(
        client
            .finalize_expired_lookup_table_rollbacks(family.id)
            .await
            .is_err(),
        "rollback finalization ran before the deadline",
    )?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_families SET rollback_until = now() - interval '1 second' WHERE id = $1",
    )
    .bind(family.id)
    .execute(client.pool())
    .await?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_vault_bindings SET rollback_until = now() - interval '1 second' WHERE family_id = $1 AND lifecycle_state = 'standby'",
    )
    .bind(family.id)
    .execute(client.pool())
    .await?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.route_lookup_tables SET rollback_until = now() - interval '1 second' WHERE family_id = $1 AND desired_state = 'standby'",
    )
    .bind(family.id)
    .execute(client.pool())
    .await?;
    let before_reserved: i32 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT reserved_address_count FROM loyal_yield.route_lookup_tables WHERE id = $1",
    )
    .bind(table_two.id)
    .fetch_one(client.pool())
    .await?;
    let finalized = client
        .finalize_expired_lookup_table_rollbacks(family.id)
        .await?;
    let after_reserved: i32 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT reserved_address_count FROM loyal_yield.route_lookup_tables WHERE id = $1",
    )
    .bind(table_two.id)
    .fetch_one(client.pool())
    .await?;
    let family_after = client
        .lookup_table_family(&cluster, &family.logical_name)
        .await?
        .ok_or_else(|| io::Error::other("rollback family disappeared"))?;
    ensure(
        finalized.cleared_previous_generation == Some(2)
            && finalized.retired_binding_ids.contains(&binding_two.id)
            && finalized.retiring_table_ids.contains(&table_two.id)
            && before_reserved - after_reserved == binding_two.reserved_capacity
            && family_after.previous_generation.is_none(),
        "expired rollback finalization did not release binding/table references",
    )?;

    // A replaced shard can become obsolete while remaining in the family's
    // current generation. Finalization must seal and retire that zero-reference
    // physical table without retiring the whole generation.
    let table_three = insert_table(
        client,
        &cluster,
        &family_after,
        LookupTableAllocationKind::VaultShard,
        1,
        1,
        LookupTableLifecycle::Active,
        true,
        unique_pubkey("rollback-current-table-three").to_string(),
    )
    .await?;
    let table_four = insert_table(
        client,
        &cluster,
        &family_after,
        LookupTableAllocationKind::VaultShard,
        1,
        2,
        LookupTableLifecycle::Active,
        true,
        unique_pubkey("rollback-current-table-four").to_string(),
    )
    .await?;
    set_verified_empty(client, table_three.id).await?;
    set_verified_empty(client, table_four.id).await?;
    let manifest_three =
        empty_vault_manifest(client, &family_after, vault_id, format!("m3-{run}")).await?;
    let manifest_four =
        empty_vault_manifest(client, &family_after, vault_id, format!("m4-{run}")).await?;
    let binding_three = client
        .insert_lookup_table_vault_binding(LookupTableVaultBindingInsert {
            vault_id,
            family_id: family.id,
            route_lookup_table_id: table_three.id,
            manifest_id: manifest_three.id,
            binding_ordinal: 1,
            allocation_mode: LookupTableBindingMode::PackedShard,
            reserved_capacity: 4,
            predecessor_binding_id: None,
        })
        .await?;
    let binding_three = client
        .flip_lookup_table_binding_head(binding_three.id, 40, Utc::now() + Duration::hours(1))
        .await?
        .active;
    let binding_four = client
        .insert_lookup_table_vault_binding(LookupTableVaultBindingInsert {
            vault_id,
            family_id: family.id,
            route_lookup_table_id: table_four.id,
            manifest_id: manifest_four.id,
            binding_ordinal: 1,
            allocation_mode: LookupTableBindingMode::PackedShard,
            reserved_capacity: 4,
            predecessor_binding_id: Some(binding_three.id),
        })
        .await?;
    let binding_four = client
        .flip_lookup_table_binding_head(binding_four.id, 41, Utc::now() + Duration::hours(1))
        .await?
        .active;
    client
        .rollback_lookup_table_binding_head(binding_four.id, 42)
        .await?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_vault_bindings SET rollback_until = now() - interval '1 second' WHERE id = $1",
    )
    .bind(binding_four.id)
    .execute(client.pool())
    .await?;
    let current_finalized = client
        .finalize_expired_lookup_table_rollbacks(family.id)
        .await?;
    let current_retired = client
        .reusable_lookup_table(table_four.id)
        .await?
        .ok_or_else(|| io::Error::other("current-generation retired shard disappeared"))?;
    ensure(
        current_finalized.cleared_previous_generation.is_none()
            && current_finalized
                .retired_binding_ids
                .contains(&binding_four.id)
            && current_finalized
                .retiring_table_ids
                .contains(&table_four.id)
            && current_retired.desired_state == LookupTableLifecycle::Retiring
            && !current_retired.accepting_allocations
            && current_retired.reserved_address_count == 0,
        "zero-reference current-generation shard was not sealed and retired",
    )?;
    let cleanup = client
        .lookup_table_cleanup_protection(&cluster, &current_retired.table_address)
        .await?
        .ok_or_else(|| io::Error::other("current-generation cleanup protection disappeared"))?;
    ensure(
        cleanup.can_deactivate && cleanup.protection_reasons.is_empty(),
        "safe zero-reference current-generation shard remained permanently cleanup-protected",
    )?;
    client
        .enqueue_lookup_table_operation(cleanup_operation(
            &cluster,
            &current_retired,
            format!("current-generation-cleanup-{run}"),
        ))
        .await?;
    let dedicated = insert_table(
        client,
        &cluster,
        &family_after,
        LookupTableAllocationKind::DedicatedVault,
        1,
        3,
        LookupTableLifecycle::Active,
        false,
        unique_pubkey("rollback-dedicated-never-reopen").to_string(),
    )
    .await?;
    set_verified_empty(client, dedicated.id).await?;
    let dedicated = client
        .mark_reusable_lookup_table_verification(
            dedicated.id,
            dedicated.mutation_epoch,
            LookupTableLifecycle::Active,
            LookupTableLifecycle::Active,
            true,
            0,
            50,
        )
        .await?;
    ensure(
        !dedicated.accepting_allocations,
        "dedicated table reconciliation reopened allocation acceptance",
    )?;
    Ok(())
}

async fn verify_rollout_controls(client: &NeonSqlClient, run: &str) -> VerifyResult<()> {
    let cluster = format!("db-verify-rollout-{run}");
    let mode = client
        .set_lookup_table_rollout_mode(
            &cluster,
            None,
            LookupTableRolloutMode::PreferReusable,
            Some("db verifier mode"),
            "db-verifier",
        )
        .await?;
    ensure(
        mode.rollout_mode == LookupTableRolloutMode::PreferReusable && !mode.force_legacy,
        "initial rollout mode was not stored",
    )?;
    let forced = client
        .set_lookup_table_force_legacy(&cluster, true, Some("db verifier force"), "db-verifier")
        .await?;
    ensure(
        forced.rollout_mode == LookupTableRolloutMode::PreferReusable && forced.force_legacy,
        "force-legacy toggle destroyed the global rollout mode",
    )?;
    let changed = client
        .set_lookup_table_rollout_mode(
            &cluster,
            None,
            LookupTableRolloutMode::Shadow,
            Some("db verifier shadow"),
            "db-verifier",
        )
        .await?;
    ensure(
        changed.rollout_mode == LookupTableRolloutMode::Shadow && changed.force_legacy,
        "rollout mode update implicitly cleared force-legacy",
    )?;
    let cleared = client
        .set_lookup_table_force_legacy(&cluster, false, Some("db verifier clear"), "db-verifier")
        .await?;
    ensure(
        cleared.rollout_mode == LookupTableRolloutMode::Shadow && !cleared.force_legacy,
        "clear-force-legacy destroyed the stored rollout mode",
    )?;
    Ok(())
}

async fn verify_legacy_retirement(client: &NeonSqlClient, run: &str) -> VerifyResult<()> {
    let cluster = format!("db-verify-legacy-{run}");
    let vault_id = create_vault(client, &format!("legacy-{run}"), 3).await?;
    let table_address = unique_pubkey("legacy-table").to_string();
    let authority = unique_pubkey("legacy-authority").to_string();
    let table_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_lookup_tables
            (cluster, scope, table_address, authority, payer, status, durable,
             address_count, address_hash, addresses)
        VALUES ($1, 'legacy-import', $2, $3, $3, 'usable', TRUE,
                0, 'legacy-hash', '[]'::jsonb)
        RETURNING id
        "#,
    )
    .bind(&cluster)
    .bind(&table_address)
    .bind(&authority)
    .fetch_one(client.pool())
    .await?;
    let request = LegacyLookupTableRetirementRequest {
        cluster: cluster.clone(),
        table_address: table_address.clone(),
        expected_authority: authority,
        expected_address_hash: "legacy-hash".to_owned(),
        expected_address_count: 0,
    };
    client
        .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
            cluster: cluster.clone(),
            lease_kind: LookupTableUsageLeaseKind::RouteResolution,
            reference_key: format!("legacy-lease-{run}"),
            route_lookup_table_ids: vec![table_id],
            vault_id: Some(vault_id),
            binding_id: None,
            route_fingerprint: Some(format!("legacy-route-{run}")),
            requirements_fingerprint: Some(format!("legacy-req-{run}")),
            expires_at: Utc::now() + Duration::minutes(5),
        })
        .await?;
    ensure(
        client
            .retire_legacy_route_lookup_table(request.clone())
            .await
            .is_err(),
        "legacy retirement ignored an active usage lease",
    )?;
    client
        .release_lookup_table_usage_leases(
            LookupTableUsageLeaseKind::RouteResolution,
            &format!("legacy-lease-{run}"),
        )
        .await?;
    ensure(
        client
            .retire_legacy_route_lookup_table(request.clone())
            .await
            .is_err(),
        "legacy retirement ran without cluster-wide reusable-only rollout",
    )?;
    client
        .set_lookup_table_rollout_mode(
            &cluster,
            None,
            LookupTableRolloutMode::ReusableOnly,
            Some("db verifier legacy retirement"),
            "db-verifier",
        )
        .await?;
    let observed_at = Utc::now();
    client
        .upsert_lookup_table_readiness(LookupTableReadinessRecord {
            cluster: cluster.clone(),
            vault_id,
            route_fingerprint: format!("legacy-route-{run}"),
            requirements_fingerprint: format!("legacy-req-{run}"),
            route_kind: "db_verifier_legacy".to_owned(),
            source_reserve: None,
            target_reserve: None,
            manifest_id: None,
            shared_family_id: None,
            vault_binding_id: None,
            readiness_state: LookupTableReadinessStatus::Ready,
            required_address_count: 0,
            covered_address_count: 0,
            missing_addresses: json!([]),
            legacy_table_ids: vec![table_id],
            reusable_table_ids: Vec::new(),
            compiled_message_size: Some(400),
            packet_limit: Some(1232),
            observed_slot: Some(77),
            observed_at,
            selection_kind: Some(LookupTableSelectionKind::Legacy),
            fallback_reason: Some("reusable_not_ready".to_owned()),
            rollout_mode: Some(LookupTableRolloutMode::PreferReusable),
            selected_table_ids: vec![table_id],
            selected_table_count: Some(1),
            packet_fits: Some(true),
            simulation_state: Some(LookupTableSimulationState::Succeeded),
            simulation_units_consumed: Some(10),
            simulation_error: None,
            updated_at: observed_at,
        })
        .await?;
    ensure(
        client
            .retire_legacy_route_lookup_table(request.clone())
            .await
            .is_err(),
        "legacy retirement ignored a current fallback/readiness reference",
    )?;
    client
        .upsert_lookup_table_readiness(LookupTableReadinessRecord {
            cluster: cluster.clone(),
            vault_id,
            route_fingerprint: format!("legacy-route-{run}"),
            requirements_fingerprint: format!("legacy-req-{run}"),
            route_kind: "db_verifier_legacy".to_owned(),
            source_reserve: None,
            target_reserve: None,
            manifest_id: None,
            shared_family_id: None,
            vault_binding_id: None,
            readiness_state: LookupTableReadinessStatus::Ready,
            required_address_count: 0,
            covered_address_count: 0,
            missing_addresses: json!([]),
            legacy_table_ids: vec![table_id],
            reusable_table_ids: Vec::new(),
            compiled_message_size: Some(350),
            packet_limit: Some(1232),
            observed_slot: Some(78),
            observed_at: Utc::now(),
            selection_kind: Some(LookupTableSelectionKind::Reusable),
            fallback_reason: None,
            rollout_mode: Some(LookupTableRolloutMode::ReusableOnly),
            selected_table_ids: Vec::new(),
            selected_table_count: Some(0),
            packet_fits: Some(true),
            simulation_state: Some(LookupTableSimulationState::Succeeded),
            simulation_units_consumed: Some(1),
            simulation_error: None,
            updated_at: Utc::now(),
        })
        .await?;
    let retired = client.retire_legacy_route_lookup_table(request).await?;
    ensure(
        retired.table_id == table_id && retired.status == "retiring" && !retired.durable,
        "legacy retirement did not atomically make the row non-selectable",
    )?;
    let remaining_evidence: Vec<i64> = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT legacy_table_ids
        FROM loyal_yield.lookup_table_route_readiness_current
        WHERE cluster = $1 AND vault_id = $2 AND route_fingerprint = $3
          AND requirements_fingerprint = $4
        "#,
    )
    .bind(&cluster)
    .bind(vault_id.as_i64())
    .bind(format!("legacy-route-{run}"))
    .bind(format!("legacy-req-{run}"))
    .fetch_one(client.pool())
    .await?;
    ensure(
        !remaining_evidence.contains(&table_id),
        "legacy retirement left a stale nonselected evidence reference",
    )?;
    ensure(
        !client
            .protected_legacy_route_lookup_table_addresses()
            .await?
            .contains(&table_address),
        "retired legacy row remained permanently protected from cleanup",
    )?;
    ensure(
        client
            .upsert_lookup_table_usage_leases(LookupTableUsageLeaseBundle {
                cluster: cluster.clone(),
                lease_kind: LookupTableUsageLeaseKind::RouteResolution,
                reference_key: format!("legacy-after-retire-{run}"),
                route_lookup_table_ids: vec![table_id],
                vault_id: Some(vault_id),
                binding_id: None,
                route_fingerprint: None,
                requirements_fingerprint: None,
                expires_at: Utc::now() + Duration::minutes(5),
            })
            .await
            .is_err(),
        "retired legacy row accepted a new route lease",
    )?;

    // Readiness selection and retirement lock the same physical row. Exactly
    // one can commit, so no stale selected-legacy reference can appear after
    // the row becomes retiring.
    client
        .set_lookup_table_rollout_mode(
            &cluster,
            None,
            LookupTableRolloutMode::ReusableOnly,
            Some("db verifier readiness race"),
            "db-verifier",
        )
        .await?;
    let (race_table_id, race_request) =
        insert_legacy_fixture(client, &cluster, "readiness-race").await?;
    let barrier = Arc::new(Barrier::new(3));
    let retire_task = {
        let client = client.clone();
        let barrier = barrier.clone();
        tokio::spawn(async move {
            barrier.wait().await;
            client.retire_legacy_route_lookup_table(race_request).await
        })
    };
    let readiness_task = {
        let client = client.clone();
        let cluster = cluster.clone();
        let barrier = barrier.clone();
        let route_fingerprint = format!("legacy-race-route-{run}");
        let requirements_fingerprint = format!("legacy-race-req-{run}");
        tokio::spawn(async move {
            barrier.wait().await;
            let now = Utc::now();
            client
                .upsert_lookup_table_readiness(LookupTableReadinessRecord {
                    cluster,
                    vault_id,
                    route_fingerprint,
                    requirements_fingerprint,
                    route_kind: "db_verifier_legacy_race".to_owned(),
                    source_reserve: None,
                    target_reserve: None,
                    manifest_id: None,
                    shared_family_id: None,
                    vault_binding_id: None,
                    readiness_state: LookupTableReadinessStatus::Ready,
                    required_address_count: 0,
                    covered_address_count: 0,
                    missing_addresses: json!([]),
                    legacy_table_ids: vec![race_table_id],
                    reusable_table_ids: Vec::new(),
                    compiled_message_size: Some(300),
                    packet_limit: Some(1232),
                    observed_slot: Some(79),
                    observed_at: now,
                    selection_kind: Some(LookupTableSelectionKind::Legacy),
                    fallback_reason: Some("race".to_owned()),
                    rollout_mode: Some(LookupTableRolloutMode::ReusableOnly),
                    selected_table_ids: vec![race_table_id],
                    selected_table_count: Some(1),
                    packet_fits: Some(true),
                    simulation_state: Some(LookupTableSimulationState::Succeeded),
                    simulation_units_consumed: Some(1),
                    simulation_error: None,
                    updated_at: now,
                })
                .await
        })
    };
    barrier.wait().await;
    let retired_race = retire_task.await?;
    let readiness_race = readiness_task.await?;
    ensure(
        usize::from(retired_race.is_ok()) + usize::from(readiness_race.is_ok()) == 1,
        "legacy retirement/readiness race did not have exactly one winner",
    )?;

    // Rollout writers and retirement share a cluster advisory fence, including
    // insertion of a previously absent override/global row.
    client
        .set_lookup_table_rollout_mode(
            &cluster,
            None,
            LookupTableRolloutMode::ReusableOnly,
            Some("db verifier rollout race reset"),
            "db-verifier",
        )
        .await?;
    let (rollout_table_id, rollout_request) =
        insert_legacy_fixture(client, &cluster, "rollout-race").await?;
    let mut guard = client.pool().begin().await?;
    loyal_yield_orchestrator::sqlx::query(
        "SELECT pg_advisory_xact_lock(hashtextextended('reusable-alt-rollout:' || $1, 0))",
    )
    .bind(&cluster)
    .execute(&mut *guard)
    .await?;
    let guarded_retire = {
        let client = client.clone();
        tokio::spawn(async move {
            client
                .retire_legacy_route_lookup_table(rollout_request)
                .await
        })
    };
    let guarded_rollout = {
        let client = client.clone();
        let cluster = cluster.clone();
        tokio::spawn(async move {
            client
                .set_lookup_table_rollout_mode(
                    &cluster,
                    None,
                    LookupTableRolloutMode::Shadow,
                    Some("db verifier concurrent rollout"),
                    "db-verifier",
                )
                .await
        })
    };
    tokio::time::sleep(std::time::Duration::from_millis(75)).await;
    ensure(
        !guarded_retire.is_finished() && !guarded_rollout.is_finished(),
        "rollout/retirement writers did not wait on the shared cluster fence",
    )?;
    guard.commit().await?;
    let guarded_retire = guarded_retire.await?;
    let guarded_rollout = guarded_rollout.await?;
    ensure(
        guarded_rollout.is_ok(),
        "concurrent rollout writer failed after cluster fence release",
    )?;
    let durable_after_race: bool = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT durable FROM loyal_yield.route_lookup_tables WHERE id = $1",
    )
    .bind(rollout_table_id)
    .fetch_one(client.pool())
    .await?;
    ensure(
        if guarded_retire.is_ok() {
            !durable_after_race
        } else {
            durable_after_race
        },
        "rollout/retirement serialization left an unsafe partial state",
    )?;
    Ok(())
}

async fn verify_observable_snapshot(
    client: &NeonSqlClient,
    fixture: &PlannedFixture,
) -> VerifyResult<()> {
    let good_table = insert_table(
        client,
        &fixture.cluster,
        &fixture.vault_family,
        LookupTableAllocationKind::VaultShard,
        fixture.vault_family.active_generation.unwrap_or_default(),
        99,
        LookupTableLifecycle::Active,
        false,
        unique_pubkey("snapshot-good-table").to_string(),
    )
    .await?;
    set_verified_empty(client, good_table.id).await?;
    client
        .upsert_lookup_table_readiness(LookupTableReadinessRecord {
            cluster: fixture.cluster.clone(),
            vault_id: fixture.vault_id,
            route_fingerprint: format!("snapshot-good-route-{}", fixture.request.id),
            requirements_fingerprint: format!("snapshot-good-req-{}", fixture.request.id),
            route_kind: "db_verifier_supported_route".to_owned(),
            source_reserve: None,
            target_reserve: None,
            manifest_id: None,
            shared_family_id: Some(fixture.shared_family.id),
            vault_binding_id: None,
            readiness_state: LookupTableReadinessStatus::Ready,
            required_address_count: 0,
            covered_address_count: 0,
            missing_addresses: json!([]),
            legacy_table_ids: Vec::new(),
            reusable_table_ids: vec![good_table.id],
            compiled_message_size: Some(400),
            packet_limit: Some(1232),
            observed_slot: Some(88_887),
            observed_at: Utc::now(),
            selection_kind: Some(LookupTableSelectionKind::Reusable),
            fallback_reason: None,
            rollout_mode: Some(LookupTableRolloutMode::ReusableOnly),
            selected_table_ids: vec![good_table.id],
            selected_table_count: Some(1),
            packet_fits: Some(true),
            simulation_state: Some(LookupTableSimulationState::Succeeded),
            simulation_units_consumed: Some(21_000),
            simulation_error: None,
            updated_at: Utc::now(),
        })
        .await?;
    client
        .upsert_lookup_table_readiness(LookupTableReadinessRecord {
            cluster: fixture.cluster.clone(),
            vault_id: fixture.vault_id,
            route_fingerprint: format!("snapshot-route-{}", fixture.request.id),
            requirements_fingerprint: fixture.request.requirements_fingerprint.clone(),
            route_kind: "db_verifier_widest_route".to_owned(),
            source_reserve: Some("source".to_owned()),
            target_reserve: Some("target".to_owned()),
            manifest_id: Some(fixture.vault_manifest_id),
            shared_family_id: Some(fixture.shared_family.id),
            vault_binding_id: Some(fixture.vault_binding.id),
            readiness_state: LookupTableReadinessStatus::Incomplete,
            required_address_count: 5,
            covered_address_count: 4,
            missing_addresses: json!([unique_pubkey("snapshot-missing").to_string()]),
            legacy_table_ids: Vec::new(),
            reusable_table_ids: vec![fixture.vault_table.id],
            compiled_message_size: Some(900),
            packet_limit: Some(1232),
            observed_slot: Some(88_888),
            observed_at: Utc::now(),
            selection_kind: Some(LookupTableSelectionKind::Blocked),
            fallback_reason: Some("missing_vault_account".to_owned()),
            rollout_mode: Some(LookupTableRolloutMode::ReusableOnly),
            selected_table_ids: Vec::new(),
            selected_table_count: Some(0),
            packet_fits: Some(true),
            simulation_state: Some(LookupTableSimulationState::Failed),
            simulation_units_consumed: Some(42_000),
            simulation_error: Some("db-verifier-simulation".to_owned()),
            updated_at: Utc::now(),
        })
        .await?;
    client
        .set_lookup_table_rollout_mode(
            &fixture.cluster,
            None,
            LookupTableRolloutMode::ReusableOnly,
            Some("db verifier snapshot"),
            "db-verifier",
        )
        .await?;
    let snapshot = client
        .lookup_table_control_plane_snapshot(&fixture.cluster)
        .await?;
    for key in [
        "readiness",
        "blockers",
        "recent_compilations",
        "queue",
        "terminal_failures",
        "drift",
        "tables",
        "rollout_controls",
        "lamports",
    ] {
        ensure(
            snapshot.get(key).is_some(),
            format!("control-plane snapshot omitted {key}"),
        )?;
    }
    ensure(
        snapshot["readiness"]["ready_active_vault_count"] == json!(0),
        "active vault was marked ready while one supported readiness row remained blocked",
    )?;
    for key in [
        "active_vault_count",
        "ready_active_vault_count",
        "ready_active_vault_percent",
        "vault_count",
        "ready_vault_count",
        "ready_percent",
    ] {
        ensure(
            snapshot["readiness"].get(key).is_some(),
            format!("snapshot readiness omitted {key}"),
        )?;
    }
    ensure(
        snapshot["queue"].get("oldest_age_seconds").is_some(),
        "snapshot queue omitted oldest age",
    )?;
    let drift = snapshot["drift"]
        .as_array()
        .ok_or_else(|| io::Error::other("snapshot drift is not an array"))?;
    ensure(
        drift
            .iter()
            .any(|row| row["error_code"] == "authority_drift")
            && !snapshot["drift"].to_string().contains("https://"),
        "snapshot omitted drift evidence or exposed an unredacted URL",
    )?;
    let tables = snapshot["tables"]
        .as_array()
        .ok_or_else(|| io::Error::other("snapshot tables is not an array"))?;
    let table = tables
        .iter()
        .find(|row| row["id"] == json!(fixture.vault_table.id))
        .ok_or_else(|| io::Error::other("snapshot omitted planned vault table"))?;
    for key in [
        "expected_authority",
        "address_hash",
        "mutation_epoch",
        "headroom",
        "fragmentation",
        "bound_vault_count",
        "last_verified_slot",
    ] {
        ensure(
            table.get(key).is_some(),
            format!("snapshot table omitted {key}"),
        )?;
    }
    let recent = snapshot["recent_compilations"]
        .as_array()
        .ok_or_else(|| io::Error::other("recent_compilations is not an array"))?;
    let compilation = recent
        .iter()
        .find(|row| row["vault_id"] == json!(fixture.vault_id.as_i64()))
        .ok_or_else(|| io::Error::other("snapshot omitted recent compilation"))?;
    for key in [
        "selection_kind",
        "selected_table_ids",
        "compiled_message_size",
        "packet_fits",
        "simulation_state",
        "simulation_units_consumed",
        "simulation_error",
    ] {
        ensure(
            compilation.get(key).is_some(),
            format!("recent compilation omitted {key}"),
        )?;
    }
    Ok(())
}

async fn create_families(
    client: &NeonSqlClient,
    cluster: &str,
    authority: &str,
    high_water: i32,
) -> VerifyResult<(LookupTableFamilyRecord, LookupTableFamilyRecord)> {
    let shared = create_family(
        client,
        cluster,
        "stable-market",
        LookupTableFamilyKind::SharedMarket,
        authority,
        high_water,
        Some(0),
    )
    .await?;
    let vault = create_family(
        client,
        cluster,
        "vault-shards",
        LookupTableFamilyKind::VaultShards,
        authority,
        high_water,
        Some(0),
    )
    .await?;
    Ok((shared, vault))
}

#[allow(clippy::too_many_arguments)]
async fn create_family(
    client: &NeonSqlClient,
    cluster: &str,
    logical_name: &str,
    kind: LookupTableFamilyKind,
    authority: &str,
    high_water: i32,
    active_generation: Option<i32>,
) -> VerifyResult<LookupTableFamilyRecord> {
    let allocation_high_water = high_water.min(62);
    let safety_margin = if 64 - allocation_high_water > 4 { 4 } else { 1 };
    Ok(client
        .create_or_validate_lookup_table_family(LookupTableFamilyUpsert {
            cluster: cluster.to_owned(),
            logical_name: logical_name.to_owned(),
            kind,
            desired_state: LookupTableFamilyState::Active,
            planner_version: "db-verifier-v1".to_owned(),
            catalog_version: "db-verifier-catalog-v1".to_owned(),
            active_generation,
            previous_generation: None,
            rollback_until: None,
            provisioning_authority: authority.to_owned(),
            payer: authority.to_owned(),
            hard_capacity: 64,
            largest_atomic_expansion: 64 - allocation_high_water - safety_margin,
            safety_margin,
            allocation_high_water,
        })
        .await?)
}

async fn create_vault(client: &NeonSqlClient, key: &str, index: i16) -> VerifyResult<VaultId> {
    let settings = unique_pubkey("vault-settings").to_string();
    let vault_pubkey = unique_pubkey("vault-pubkey").to_string();
    let policy_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_policies
            (settings, authority, policy_seed, policy_account, vault_index,
             vault_pubkey, threshold, last_seen_slot, last_seen_signature)
        VALUES ($1, $2, 0, $3, $4, $5, 1, 1, $6)
        RETURNING id
        "#,
    )
    .bind(&settings)
    .bind(unique_pubkey("policy-authority").to_string())
    .bind(unique_pubkey("policy-account").to_string())
    .bind(index)
    .bind(&vault_pubkey)
    .bind(format!("db-verifier-{key}"))
    .fetch_one(client.pool())
    .await?;
    let vault_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.managed_vaults
            (settings, vault_index, vault_pubkey, active_policy_id)
        VALUES ($1, $2, $3, $4)
        RETURNING id
        "#,
    )
    .bind(settings)
    .bind(index)
    .bind(vault_pubkey)
    .bind(policy_id)
    .fetch_one(client.pool())
    .await?;
    Ok(VaultId(vault_id))
}

fn typed_addresses(
    subject: LookupTableManifestSubject,
    count: usize,
    role: &str,
) -> Vec<LookupTableManifestAddressRecord> {
    (0..count)
        .map(|ordinal| LookupTableManifestAddressRecord {
            address: unique_pubkey(&format!("{role}-{ordinal}")).to_string(),
            ordinal: ordinal as i32,
            semantic_class: subject,
            account_role: format!("{role}_{ordinal}"),
            is_writable: ordinal % 2 == 0,
        })
        .collect()
}

fn plan_policy(recent_slot: u64) -> LookupTableProvisioningPlanPolicy {
    LookupTableProvisioningPlanPolicy {
        vault_policy: PackedShardPolicy {
            hard_capacity: 64,
            largest_atomic_expansion: 20,
            safety_margin: 4,
            per_vault_growth_reservation: 4,
            max_vault_cohort: 8,
        },
        shared_shard_capacity: 32,
        max_extension_addresses: 20,
        operation_context: json!({
            "source": "db_verifier",
            "recent_slot": recent_slot,
        }),
        estimated_fee_lamports: Some(5_000),
        estimated_rent_lamports: Some(10_000),
    }
}

fn request_lease(
    request: &LookupTableProvisioningRequestRecord,
) -> VerifyResult<LookupTableOperationLease> {
    Ok(LookupTableOperationLease::new(
        request
            .lease_owner
            .clone()
            .ok_or_else(|| io::Error::other("request lease has no owner"))?,
        request.fencing_token,
        request
            .lease_expires_at
            .ok_or_else(|| io::Error::other("request lease has no expiry"))?,
    )?)
}

fn operation_lease(
    operation: &LookupTableOperationRecord,
) -> VerifyResult<LookupTableOperationLease> {
    Ok(LookupTableOperationLease::new(
        operation
            .lease_owner
            .clone()
            .ok_or_else(|| io::Error::other("operation lease has no owner"))?,
        operation.fencing_token,
        operation
            .lease_expires_at
            .ok_or_else(|| io::Error::other("operation lease has no expiry"))?,
    )?)
}

async fn request_by_id(
    client: &NeonSqlClient,
    request_id: i64,
) -> VerifyResult<LookupTableProvisioningRequestRecord> {
    let row = loyal_yield_orchestrator::sqlx::query(
        "SELECT * FROM loyal_yield.lookup_table_provisioning_requests WHERE id = $1",
    )
    .bind(request_id)
    .fetch_one(client.pool())
    .await?;
    Ok(LookupTableProvisioningRequestRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        vault_id: VaultId(row.try_get("vault_id")?),
        route_fingerprint: row.try_get("route_fingerprint")?,
        requirements_fingerprint: row.try_get("requirements_fingerprint")?,
        shared_manifest_id: row.try_get("shared_manifest_id")?,
        vault_manifest_id: row.try_get("vault_manifest_id")?,
        desired_shared_hash: row.try_get("desired_shared_hash")?,
        desired_vault_hash: row.try_get("desired_vault_hash")?,
        desired_shared_address_count: row.try_get("desired_shared_address_count")?,
        desired_vault_address_count: row.try_get("desired_vault_address_count")?,
        sealed_at: row.try_get("sealed_at")?,
        request_status: row.try_get::<String, _>("request_status")?.parse()?,
        lease_owner: row.try_get("lease_owner")?,
        lease_expires_at: row.try_get("lease_expires_at")?,
        fencing_token: row.try_get("fencing_token")?,
        attempt_count: row.try_get("attempt_count")?,
        next_attempt_at: row.try_get("next_attempt_at")?,
        error_code: row.try_get("error_code")?,
        error_detail: row.try_get("error_detail")?,
        requested_at: row.try_get("requested_at")?,
        satisfied_at: row.try_get("satisfied_at")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

async fn plan_counts(client: &NeonSqlClient, cluster: &str) -> VerifyResult<(i64, i64, i64)> {
    let tables: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.route_lookup_tables WHERE cluster = $1 AND family_id IS NOT NULL",
    )
    .bind(cluster)
    .fetch_one(client.pool())
    .await?;
    let bindings: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.lookup_table_vault_bindings binding
        JOIN loyal_yield.lookup_table_families family ON family.id = binding.family_id
        WHERE family.cluster = $1
        "#,
    )
    .bind(cluster)
    .fetch_one(client.pool())
    .await?;
    let operations: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*) FROM loyal_yield.lookup_table_operations operation
        JOIN loyal_yield.lookup_table_families family ON family.id = operation.family_id
        WHERE family.cluster = $1
        "#,
    )
    .bind(cluster)
    .fetch_one(client.pool())
    .await?;
    Ok((tables, bindings, operations))
}

#[allow(clippy::too_many_arguments)]
async fn insert_table(
    client: &NeonSqlClient,
    cluster: &str,
    family: &LookupTableFamilyRecord,
    allocation_kind: LookupTableAllocationKind,
    generation: i32,
    shard_ordinal: i32,
    desired_state: LookupTableLifecycle,
    accepting_allocations: bool,
    table_address: String,
) -> VerifyResult<ReusableLookupTableRecord> {
    Ok(client
        .insert_reusable_lookup_table(ReusableLookupTableInsert {
            cluster: cluster.to_owned(),
            scope: format!(
                "db-verifier:{}:{generation}:{shard_ordinal}",
                family.logical_name
            ),
            table_address,
            authority: family.provisioning_authority.clone(),
            payer: family.payer.clone(),
            family_id: family.id,
            allocation_kind,
            generation,
            shard_ordinal,
            desired_state,
            accepting_allocations,
            allocation_high_water: family.allocation_high_water,
            mutation_epoch: 0,
            create_signature: None,
        })
        .await?)
}

async fn set_verified_empty(client: &NeonSqlClient, table_id: i64) -> VerifyResult<()> {
    loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.route_lookup_tables
        SET status = 'usable', last_verified_slot = 1,
            last_verified_at = now(), usable_address_count = address_count,
            address_hash = $2
        WHERE id = $1
        "#,
    )
    .bind(table_id)
    .bind(ordered_address_hash(&[]))
    .execute(client.pool())
    .await?;
    Ok(())
}

async fn cleanup_table(
    client: &NeonSqlClient,
    cluster: &str,
    family: &LookupTableFamilyRecord,
    generation: i32,
    shard_ordinal: i32,
) -> VerifyResult<ReusableLookupTableRecord> {
    let table = insert_table(
        client,
        cluster,
        family,
        LookupTableAllocationKind::SharedMarket,
        generation,
        shard_ordinal,
        LookupTableLifecycle::Active,
        false,
        unique_pubkey("cleanup-table").to_string(),
    )
    .await?;
    set_verified_empty(client, table.id).await?;
    Ok(client
        .reusable_lookup_table(table.id)
        .await?
        .ok_or_else(|| io::Error::other("cleanup table disappeared"))?)
}

fn cleanup_operation(
    cluster: &str,
    table: &ReusableLookupTableRecord,
    idempotency_key: String,
) -> LookupTableOperationEnqueue {
    LookupTableOperationEnqueue {
        idempotency_key,
        family_id: table.family_id,
        route_lookup_table_id: Some(table.id),
        manifest_id: None,
        binding_id: None,
        operation_kind: LookupTableOperationKind::Deactivate,
        target_generation: None,
        target_shard_ordinal: None,
        operation_context: json!({
            "source": "db_verifier_cleanup",
            "cluster": cluster,
            "table": table.table_address,
            "expectedAuthority": table.authority,
            "expectedAddressHash": table.address_hash,
            "expectedMutationEpoch": table.mutation_epoch,
        }),
        mutation_epoch: table.mutation_epoch,
        estimated_fee_lamports: None,
        estimated_rent_lamports: None,
        addresses: Vec::new(),
    }
}

async fn empty_vault_manifest(
    client: &NeonSqlClient,
    family: &LookupTableFamilyRecord,
    vault_id: VaultId,
    key: String,
) -> VerifyResult<LookupTableManifestRecord> {
    Ok(client
        .persist_lookup_table_manifest(LookupTableManifestWrite {
            family_id: family.id,
            subject_kind: LookupTableManifestSubject::Vault,
            subject_key: key.clone(),
            vault_id: Some(vault_id),
            desired_set_hash: format!("hash-{key}"),
            source_slot: Some(1),
            planner_version: family.planner_version.clone(),
            catalog_version: family.catalog_version.clone(),
            addresses: Vec::new(),
        })
        .await?)
}

async fn insert_legacy_fixture(
    client: &NeonSqlClient,
    cluster: &str,
    label: &str,
) -> VerifyResult<(i64, LegacyLookupTableRetirementRequest)> {
    let table_address = unique_pubkey(&format!("legacy-{label}-table")).to_string();
    let authority = unique_pubkey(&format!("legacy-{label}-authority")).to_string();
    let address_hash = format!("legacy-{label}-hash");
    let table_id: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.route_lookup_tables
            (cluster, scope, table_address, authority, payer, status, durable,
             address_count, address_hash, addresses)
        VALUES ($1, $2, $3, $4, $4, 'usable', TRUE, 0, $5, '[]'::jsonb)
        RETURNING id
        "#,
    )
    .bind(cluster)
    .bind(format!("legacy-{label}"))
    .bind(&table_address)
    .bind(&authority)
    .bind(&address_hash)
    .fetch_one(client.pool())
    .await?;
    Ok((
        table_id,
        LegacyLookupTableRetirementRequest {
            cluster: cluster.to_owned(),
            table_address,
            expected_authority: authority,
            expected_address_hash: address_hash,
            expected_address_count: 0,
        },
    ))
}

fn ensure(condition: bool, message: impl Into<String>) -> VerifyResult<()> {
    if condition {
        Ok(())
    } else {
        fail(message)
    }
}

fn unique_pubkey(label: &str) -> Pubkey {
    let sequence = PUBKEY_SEQUENCE.fetch_add(1, Ordering::Relaxed);
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let mut hasher = Sha256::new();
    hasher.update(std::process::id().to_le_bytes());
    hasher.update(now.to_le_bytes());
    hasher.update(sequence.to_le_bytes());
    hasher.update(label.as_bytes());
    Pubkey::new_from_array(hasher.finalize().into())
}

fn ordered_address_hash(addresses: &[String]) -> String {
    let mut hasher = Sha256::new();
    for address in addresses {
        hasher.update((address.len() as u64).to_le_bytes());
        hasher.update(address.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn fail<T>(message: impl Into<String>) -> VerifyResult<T> {
    Err(io::Error::other(message.into()).into())
}
