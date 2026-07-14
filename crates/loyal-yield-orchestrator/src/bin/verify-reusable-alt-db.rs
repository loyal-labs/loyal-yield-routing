use chrono::{Duration, Utc};
use loyal_yield_orchestrator::sqlx::{postgres::PgPoolOptions, PgPool, Row};
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
        .require_schema_migration(20, "demand_driven_shared_market_catalog")
        .await?;
    verify_independent_sql_invariants(&pool).await?;

    let run = format!("{}-{}", std::process::id(), Utc::now().timestamp_micros());
    let mut passed = Vec::new();

    verify_policy_authority_reuse_and_family_identity(&client, &run).await?;
    passed.push("policy authority reuse, authority/payer consistency, and family identity fencing");

    verify_shared_market_catalog_control_plane(&client, &run).await?;
    passed.push(
        "vault-independent shared catalog head, finalized physical-drift rollover, rollback-safe revisions, exact activation, and fenced direct cutover",
    );

    verify_durable_cluster_budget(&client, &run).await?;
    passed.push(
        "concurrent PostgreSQL-backed cluster budget reservation, fence idempotency, and overspend denial",
    );

    verify_zero_class_and_request_sealing(&client, &run).await?;
    passed.push("sealed requests, route-independent idempotency, zero-class satisfaction");

    verify_nonempty_eventual_satisfaction_and_requeue(&client, &run).await?;
    passed.push(
        "non-empty provisioning convergence, exact reusable coverage, and satisfied-request requeue",
    );

    verify_idle_predecision_deferral_evidence(&client, &run).await?;
    passed.push(
        "idle missing-vault control-plane defer has blocker/request evidence and no decision/signature side effect",
    );

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

    verify_legacy_import_audit(&client, &run).await?;
    passed
        .push("atomic legacy fleet import, immutable evidence, replay, and stale-snapshot fencing");

    verify_legacy_retirement(&client, &run).await?;
    passed.push("explicit fenced legacy retirement");

    verify_observable_snapshot(&client, &planned).await?;
    passed.push("operator snapshot fields and recent compilation evidence");

    verify_independent_sql_invariants(&pool).await?;
    passed.push("independent SQL invariants before and after behavior fixtures");

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

async fn verify_independent_sql_invariants(pool: &PgPool) -> VerifyResult<()> {
    let sql = include_str!("../../../../scripts/verify-reusable-alt-schema.sql")
        .strip_prefix("\\set ON_ERROR_STOP on\n")
        .ok_or_else(|| {
            io::Error::other(
                "independent reusable ALT SQL verifier must begin with the expected psql guard",
            )
        })?;
    loyal_yield_orchestrator::sqlx::raw_sql(sql)
        .execute(pool)
        .await?;
    Ok(())
}

async fn verify_policy_authority_reuse_and_family_identity(
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
    let policy_family = client
        .create_or_validate_lookup_table_family(LookupTableFamilyUpsert {
            cluster: cluster.clone(),
            logical_name: format!("policy-authority-family-{run}"),
            kind: LookupTableFamilyKind::VaultShards,
            desired_state: LookupTableFamilyState::Active,
            planner_version: "db-verifier-v1".to_owned(),
            catalog_version: "db-verifier-catalog-v1".to_owned(),
            active_generation: Some(0),
            previous_generation: None,
            rollback_until: None,
            provisioning_authority: manager.clone(),
            payer: manager.clone(),
            hard_capacity: 64,
            largest_atomic_expansion: 8,
            safety_margin: 4,
            allocation_high_water: 52,
        })
        .await?;
    ensure(
        policy_family.provisioning_authority == manager && policy_family.payer == manager,
        "family did not preserve the reused policy authority as matching authority/payer",
    )?;

    let mismatch_cluster = format!("db-verify-family-payer-mismatch-{run}");
    ensure(
        client
            .create_or_validate_lookup_table_family(LookupTableFamilyUpsert {
                cluster: mismatch_cluster.clone(),
                logical_name: format!("payer-mismatch-family-{run}"),
                kind: LookupTableFamilyKind::VaultShards,
                desired_state: LookupTableFamilyState::Active,
                planner_version: "db-verifier-v1".to_owned(),
                catalog_version: "db-verifier-catalog-v1".to_owned(),
                active_generation: Some(0),
                previous_generation: None,
                rollback_until: None,
                provisioning_authority: manager.clone(),
                payer: unique_pubkey("different-family-payer").to_string(),
                hard_capacity: 64,
                largest_atomic_expansion: 8,
                safety_margin: 4,
                allocation_high_water: 52,
            })
            .await
            .is_err(),
        "family bootstrap accepted different provisioning authority and payer",
    )?;
    let mismatch_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_families WHERE cluster = $1",
    )
    .bind(&mismatch_cluster)
    .fetch_one(client.pool())
    .await?;
    ensure(
        mismatch_count == 0,
        "rejected authority/payer mismatch left a partial family row",
    )?;

    let deterministic_cluster = format!("db-verify-family-kind-{run}");
    let policy_authority = unique_pubkey("policy-family-authority").to_string();
    create_family(
        client,
        &deterministic_cluster,
        "vault-family-a",
        LookupTableFamilyKind::VaultShards,
        &policy_authority,
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
            &policy_authority,
            40,
            Some(0),
        )
        .await
        .is_err(),
        "schema accepted two active families of the same cluster/kind",
    )
}

async fn verify_shared_market_catalog_control_plane(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<()> {
    let cluster = format!("db-verify-shared-catalog-{run}");
    let authority = unique_pubkey("shared-catalog-authority").to_string();
    let (shared_family, _vault_family) = create_families(client, &cluster, &authority, 40).await?;
    let vault_id = create_vault(client, &format!("shared-catalog-{run}"), 41).await?;
    let catalog_v1 = typed_addresses(
        LookupTableManifestSubject::SharedMarket,
        2,
        "shared-catalog-v1",
    );
    let head_v1 = publish_shared_catalog(client, &cluster, catalog_v1.clone(), run, 90_000).await?;
    let replayed =
        publish_shared_catalog(client, &cluster, catalog_v1.clone(), run, 90_001).await?;
    ensure(
        head_v1.catalog_revision == 1
            && head_v1.catalog_revision_id == replayed.catalog_revision_id
            && head_v1.manifest_id == replayed.manifest_id
            && head_v1.readiness_state == SharedMarketCatalogReadiness::Pending
            && head_v1.address_count == 2,
        "shared catalog publication was not immutable and idempotent",
    )?;
    let oversized_addresses = typed_addresses(
        LookupTableManifestSubject::SharedMarket,
        41,
        "shared-catalog-oversized",
    );
    let oversized_hash = lookup_table_manifest_address_records_hash(&oversized_addresses);
    let oversized_manifest = client
        .persist_lookup_table_manifest(LookupTableManifestWrite {
            family_id: shared_family.id,
            subject_kind: LookupTableManifestSubject::SharedMarket,
            subject_key: format!("shared-catalog-oversized-{run}"),
            vault_id: None,
            desired_set_hash: oversized_hash.clone(),
            source_slot: Some(90_000),
            planner_version: shared_family.planner_version.clone(),
            catalog_version: shared_family.catalog_version.clone(),
            addresses: oversized_addresses,
        })
        .await?;
    let oversized_revision = loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.lookup_table_shared_market_catalog_revisions
            (family_id, manifest_id, catalog_revision, catalog_version,
             desired_set_hash, enabled_mints_hash, reserve_set_hash,
             address_count, source_slot, source_metadata, reason, updated_by)
        VALUES ($1, $2, 99, $3, $4, $4, $4, 41, 90000,
                '{}'::jsonb, 'must fail capacity fence', 'isolated-db-verifier')
        "#,
    )
    .bind(shared_family.id)
    .bind(oversized_manifest.id)
    .bind(&shared_family.catalog_version)
    .bind(&oversized_hash)
    .execute(client.pool())
    .await;
    ensure(
        oversized_revision.is_err(),
        "database accepted a shared catalog larger than one family ALT high-water mark",
    )?;
    ensure(
        client
            .plan_shared_market_catalog_head(
                &cluster,
                head_v1.catalog_revision_id + 1,
                shared_catalog_policy(90_000),
            )
            .await
            .is_err(),
        "shared catalog planner accepted a stale revision fence",
    )?;
    ensure(
        client
            .reusable_only_cutover_preflight(&cluster)
            .await
            .is_err(),
        "reusable-only cutover preflight accepted a pending shared catalog",
    )?;
    let plan_v1 = client
        .plan_shared_market_catalog_head(
            &cluster,
            head_v1.catalog_revision_id,
            shared_catalog_policy(90_000),
        )
        .await?;
    ensure(
        plan_v1.shared_operations.len() == 1
            && plan_v1.catalog.target_generation == Some(plan_v1.shared_target_generation)
            && plan_v1.catalog.readiness_state == SharedMarketCatalogReadiness::Provisioning,
        "shared catalog did not plan one durable physical ALT",
    )?;
    for operation in &plan_v1.shared_operations {
        materialize_operation_manifest(client, operation, 90_001).await?;
    }
    let active_v1 = client
        .reconcile_shared_market_catalog_head(
            &cluster,
            head_v1.catalog_revision_id,
            shared_catalog_policy(90_001),
            Utc::now() + Duration::hours(1),
        )
        .await?;
    ensure(
        active_v1.readiness_state == SharedMarketCatalogReadiness::Active
            && active_v1.target_generation == active_v1.active_generation
            && active_v1.activated_at.is_some()
            && !active_v1.reason.trim().is_empty()
            && !active_v1.updated_by.trim().is_empty(),
        "shared catalog reconciliation omitted active status or publication reason evidence",
    )?;
    client
        .upsert_lookup_table_rollout_control(
            &cluster,
            None,
            LookupTableRolloutMode::Shadow,
            true,
            Some("isolated pre-cutover global override"),
            "isolated-db-verifier",
        )
        .await?;
    client
        .upsert_lookup_table_rollout_control(
            &cluster,
            Some(vault_id),
            LookupTableRolloutMode::Legacy,
            true,
            Some("isolated pre-cutover vault override"),
            "isolated-db-verifier",
        )
        .await?;
    let stale_cutover_preflight = client.reusable_only_cutover_preflight(&cluster).await?;
    let drifted_addresses = stale_cutover_preflight.ordered_addresses.clone();
    let drift_report = client
        .report_shared_market_physical_drift(SharedMarketPhysicalDriftReport {
            cluster: cluster.clone(),
            catalog_revision_id: stale_cutover_preflight.catalog_revision_id,
            family_id: stale_cutover_preflight.shared_family_id,
            route_lookup_table_id: stale_cutover_preflight.physical_table_id,
            expected_mutation_epoch: stale_cutover_preflight.physical_mutation_epoch,
            expected_table_address: stale_cutover_preflight.physical_table_address.clone(),
            expected_authority: stale_cutover_preflight.physical_authority.clone(),
            observed_slot: 90_002,
            observed_table_present: true,
            observed_authority: Some(stale_cutover_preflight.physical_authority.clone()),
            observed_active: true,
            observed_last_extended_slot: Some(90_002),
            observed_warm: false,
            observed_addresses: drifted_addresses,
            reason: "isolated finalized warmup drift".to_owned(),
            reported_by: "isolated-db-verifier".to_owned(),
        })
        .await?;
    ensure(
        drift_report.resolution_state == SharedMarketPhysicalDriftResolution::Open
            && client
                .activate_reusable_only_cutover(
                    &stale_cutover_preflight,
                    "must reject stale finalized evidence",
                    "isolated-db-verifier",
                )
                .await
                .is_err(),
        "shared physical drift did not invalidate stale reusable-only cutover evidence",
    )?;
    let effective_before_repair = client
        .effective_lookup_table_rollout(&cluster, vault_id)
        .await?;
    ensure(
        effective_before_repair.force_legacy
            && effective_before_repair.rollout_mode == LookupTableRolloutMode::Legacy,
        "failed stale cutover changed rollout controls",
    )?;
    let drift_plan = client
        .plan_shared_market_catalog_head(
            &cluster,
            head_v1.catalog_revision_id,
            shared_catalog_policy(90_002),
        )
        .await?;
    ensure(
        drift_plan.shared_target_generation != active_v1.active_generation.unwrap_or_default()
            && !drift_plan.shared_operations.is_empty(),
        "open finalized shared drift did not force a replacement generation",
    )?;
    for operation in &drift_plan.shared_operations {
        materialize_operation_manifest(client, operation, 90_003).await?;
    }
    let repaired_v1 = client
        .reconcile_shared_market_catalog_head(
            &cluster,
            head_v1.catalog_revision_id,
            shared_catalog_policy(90_003),
            Utc::now() + Duration::hours(1),
        )
        .await?;
    let persisted_drift_state: String = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT resolution_state FROM loyal_yield.lookup_table_shared_market_physical_drifts WHERE id = $1",
    )
    .bind(drift_report.id)
    .fetch_one(client.pool())
    .await?;
    ensure(
        repaired_v1.readiness_state == SharedMarketCatalogReadiness::Active
            && repaired_v1.active_generation == repaired_v1.target_generation
            && persisted_drift_state == "resolved",
        "shared physical drift replacement did not activate and resolve its durable evidence",
    )?;
    let cutover_preflight = client.reusable_only_cutover_preflight(&cluster).await?;
    let cutover = client
        .activate_reusable_only_cutover(
            &cutover_preflight,
            "isolated demand-driven direct cutover",
            "isolated-db-verifier",
        )
        .await?;
    let effective = client
        .effective_lookup_table_rollout(&cluster, vault_id)
        .await?;
    ensure(
        cutover.catalog_revision_id == head_v1.catalog_revision_id
            && cutover.aligned_vault_control_count == 1
            && effective.rollout_mode == LookupTableRolloutMode::ReusableOnly
            && !effective.force_legacy
            && effective.global.as_ref().is_some_and(|row| {
                row.rollout_mode == LookupTableRolloutMode::ReusableOnly && !row.force_legacy
            })
            && effective.vault.as_ref().is_some_and(|row| {
                row.rollout_mode == LookupTableRolloutMode::ReusableOnly && !row.force_legacy
            }),
        "direct cutover left a hidden global or per-vault legacy override",
    )?;
    let covered = client
        .validate_shared_market_catalog_route(&cluster, vec![catalog_v1[0].clone()])
        .await?;
    ensure(
        covered.state == SharedMarketCatalogRouteValidationState::Covered,
        "active catalog rejected a covered route subset",
    )?;
    let mut semantic_drift_row = catalog_v1[0].clone();
    semantic_drift_row.account_role = "unexpected_catalog_role".to_owned();
    let semantic_drift = client
        .validate_shared_market_catalog_route(&cluster, vec![semantic_drift_row])
        .await?;
    ensure(
        semantic_drift.state == SharedMarketCatalogRouteValidationState::Drift
            && semantic_drift.semantic_mismatch_addresses == vec![catalog_v1[0].address.clone()],
        "shared catalog runtime fence omitted typed semantic drift evidence",
    )?;

    let unknown_shared = typed_addresses(
        LookupTableManifestSubject::SharedMarket,
        1,
        "shared-catalog-unknown-route",
    );
    let drift = client
        .validate_shared_market_catalog_route(&cluster, unknown_shared.clone())
        .await?;
    ensure(
        drift.state == SharedMarketCatalogRouteValidationState::Drift
            && drift.route_missing_addresses == vec![unknown_shared[0].address.clone()],
        "shared catalog runtime fence omitted route drift evidence",
    )?;
    let drift_route_fingerprint = format!("shared-catalog-drift-route-{run}");
    let drift_requirements_fingerprint = format!("shared-catalog-drift-requirements-{run}");
    client
        .upsert_lookup_table_readiness(LookupTableReadinessRecord {
            cluster: cluster.clone(),
            vault_id,
            route_fingerprint: drift_route_fingerprint.clone(),
            requirements_fingerprint: drift_requirements_fingerprint.clone(),
            route_kind: "db_verifier_shared_catalog_drift".to_owned(),
            source_reserve: None,
            target_reserve: None,
            manifest_id: None,
            shared_family_id: Some(shared_family.id),
            vault_binding_id: None,
            readiness_state: LookupTableReadinessStatus::Incomplete,
            required_address_count: 1,
            covered_address_count: 0,
            missing_addresses: json!(drift.route_missing_addresses),
            legacy_table_ids: Vec::new(),
            reusable_table_ids: Vec::new(),
            compiled_message_size: None,
            packet_limit: Some(1232),
            observed_slot: Some(90_002),
            observed_at: Utc::now(),
            selection_kind: Some(LookupTableSelectionKind::Blocked),
            fallback_reason: Some("shared_market_catalog_drift".to_owned()),
            rollout_mode: Some(LookupTableRolloutMode::ReusableOnly),
            selected_table_ids: Vec::new(),
            selected_table_count: Some(0),
            packet_fits: None,
            simulation_state: Some(LookupTableSimulationState::NotRun),
            simulation_units_consumed: None,
            simulation_error: None,
            updated_at: Utc::now(),
        })
        .await?;
    let drift_readiness = client
        .lookup_table_readiness(
            &cluster,
            vault_id,
            &drift_route_fingerprint,
            &drift_requirements_fingerprint,
        )
        .await?
        .ok_or_else(|| io::Error::other("shared catalog drift readiness signal disappeared"))?;
    let route_created_request_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_provisioning_requests WHERE cluster = $1 AND vault_id = $2",
    )
    .bind(&cluster)
    .bind(vault_id.as_i64())
    .fetch_one(client.pool())
    .await?;
    ensure(
        route_created_request_count == 0
            && drift_readiness.readiness_state == LookupTableReadinessStatus::Incomplete
            && drift_readiness.selection_kind == Some(LookupTableSelectionKind::Blocked)
            && drift_readiness.fallback_reason.as_deref() == Some("shared_market_catalog_drift"),
        "shared catalog drift did not remain a request-free readiness/repair signal",
    )?;

    // Even a manually injected malformed request must fail before vault
    // allocation. Normal route code does not create this request; the fixture
    // exercises the database planner's second defensive fence.
    let drift_request = client
        .upsert_lookup_table_provisioning_request(LookupTableProvisioningRequestUpsert {
            cluster: cluster.clone(),
            vault_id,
            route_fingerprint: drift_route_fingerprint,
            requirements_fingerprint: drift_requirements_fingerprint,
            shared_manifest_id: None,
            vault_manifest_id: None,
            desired_shared_hash: Some(format!("shared-catalog-drift-shared-{run}")),
            desired_vault_hash: Some(format!("shared-catalog-drift-vault-{run}")),
            shared_addresses: unknown_shared,
            vault_addresses: typed_addresses(
                LookupTableManifestSubject::Vault,
                1,
                "shared-catalog-drift-vault",
            ),
        })
        .await?;
    let leased = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "shared-catalog-drift-planner",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("shared catalog drift request was not leaseable"))?;
    let lease = request_lease(&leased)?;
    ensure(
        client
            .plan_lookup_table_provisioning_request(
                &cluster,
                drift_request.id,
                &lease,
                plan_policy(90_002),
            )
            .await
            .is_err(),
        "route shared drift reached vault allocation",
    )?;
    let vault_side_effect_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT
            (SELECT count(*) FROM loyal_yield.lookup_table_vault_bindings WHERE vault_id = $1)
          + (SELECT count(*) FROM loyal_yield.lookup_table_manifests
             WHERE vault_id = $1 AND subject_kind = 'vault')
        "#,
    )
    .bind(vault_id.as_i64())
    .fetch_one(client.pool())
    .await?;
    ensure(
        vault_side_effect_count == 0,
        "shared drift left a vault manifest or allocation side effect",
    )?;
    client
        .advance_lookup_table_provisioning_request(
            drift_request.id,
            &lease,
            LookupTableProvisioningRequestStatus::Failed,
            None,
            Some("catalog_drift"),
            Some("isolated verifier expected drift"),
        )
        .await?;
    let failed_drift_request = request_by_id(client, drift_request.id).await?;
    ensure(
        failed_drift_request.request_status == LookupTableProvisioningRequestStatus::Failed
            && failed_drift_request.error_code.as_deref() == Some("catalog_drift")
            && failed_drift_request.error_detail.as_deref()
                == Some("isolated verifier expected drift"),
        "failed shared-drift defense omitted durable request status/reason evidence",
    )?;

    let catalog_v2 = normalize_catalog_addresses([
        vec![catalog_v1[1].clone()],
        typed_addresses(
            LookupTableManifestSubject::SharedMarket,
            1,
            "shared-catalog-v2-new",
        ),
    ]);
    let head_v2 = publish_shared_catalog(client, &cluster, catalog_v2.clone(), run, 90_003).await?;
    ensure(
        head_v2.catalog_revision == 2
            && head_v2.catalog_revision_id != head_v1.catalog_revision_id
            && head_v2.readiness_state == SharedMarketCatalogReadiness::Pending,
        "shared catalog head did not advance as a pending monotonic revision",
    )?;
    let pre_rollover = client
        .validate_shared_market_catalog_route(&cluster, vec![catalog_v2[0].clone()])
        .await?;
    ensure(
        pre_rollover.state == SharedMarketCatalogRouteValidationState::Drift
            && !pre_rollover.active_missing_addresses.is_empty()
            && !pre_rollover.active_extra_addresses.is_empty(),
        "append-only old shared ALT was accepted after the catalog head changed",
    )?;
    let plan_v2 = client
        .plan_shared_market_catalog_head(
            &cluster,
            head_v2.catalog_revision_id,
            shared_catalog_policy(90_003),
        )
        .await?;
    ensure(
        plan_v2.shared_target_generation != active_v1.active_generation.unwrap_or_default()
            && plan_v2.shared_operations.len() == 1,
        "catalog shrink/change did not plan an exact rollover generation",
    )?;
    for operation in &plan_v2.shared_operations {
        materialize_operation_manifest(client, operation, 90_004).await?;
    }
    let active_v2 = client
        .reconcile_shared_market_catalog_head(
            &cluster,
            head_v2.catalog_revision_id,
            shared_catalog_policy(90_004),
            Utc::now() + Duration::hours(1),
        )
        .await?;
    let covered_v2 = client
        .validate_shared_market_catalog_route(&cluster, vec![catalog_v2[0].clone()])
        .await?;
    ensure(
        active_v2.readiness_state == SharedMarketCatalogReadiness::Active
            && covered_v2.state == SharedMarketCatalogRouteValidationState::Covered
            && covered_v2.active_missing_addresses.is_empty()
            && covered_v2.active_extra_addresses.is_empty(),
        "catalog rollover did not activate exact new physical membership",
    )?;

    let rollback_head = publish_shared_catalog(client, &cluster, catalog_v1, run, 90_005).await?;
    ensure(
        rollback_head.catalog_revision == 3
            && rollback_head.manifest_id == head_v1.manifest_id
            && rollback_head.catalog_revision_id != head_v1.catalog_revision_id
            && rollback_head.readiness_state == SharedMarketCatalogReadiness::Pending,
        "monotonic catalog rollback could not reuse its prior immutable sealed manifest",
    )
}

async fn verify_durable_cluster_budget(client: &NeonSqlClient, run: &str) -> VerifyResult<()> {
    let cluster = format!("db-verify-durable-budget-{run}");
    let authority = unique_pubkey("durable-budget-authority").to_string();
    let (_shared_family, vault_family) = create_families(client, &cluster, &authority, 40).await?;
    for shard_ordinal in 0..2 {
        let table = insert_table(
            client,
            &cluster,
            &vault_family,
            LookupTableAllocationKind::VaultShard,
            0,
            shard_ordinal,
            LookupTableLifecycle::Active,
            true,
            unique_pubkey("durable-budget-table").to_string(),
        )
        .await?;
        client
            .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
                idempotency_key: format!("durable-budget-{run}-{shard_ordinal}"),
                family_id: vault_family.id,
                route_lookup_table_id: Some(table.id),
                manifest_id: None,
                binding_id: None,
                operation_kind: LookupTableOperationKind::Extend,
                target_generation: None,
                target_shard_ordinal: None,
                operation_context: json!({"source": "db_verifier_durable_budget"}),
                mutation_epoch: table.mutation_epoch,
                estimated_fee_lamports: None,
                estimated_rent_lamports: None,
                addresses: vec![unique_pubkey("durable-budget-address").to_string()],
            })
            .await?;
    }
    let leased_a = client
        .lease_next_lookup_table_operation(
            &cluster,
            "durable-budget-a",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .ok_or_else(|| io::Error::other("first budget operation was not leaseable"))?;
    let leased_b = client
        .lease_next_lookup_table_operation(
            &cluster,
            "durable-budget-b",
            Utc::now() + Duration::minutes(5),
            false,
        )
        .await?
        .ok_or_else(|| io::Error::other("second budget operation was not leaseable"))?;
    let lease_a = operation_lease(&leased_a.operation)?;
    let lease_b = operation_lease(&leased_b.operation)?;
    let policy = LookupTableClusterBudgetPolicy {
        max_lamports: 100,
        rolling_window_seconds: 600,
    };
    let barrier = Arc::new(Barrier::new(3));
    let task_a = {
        let client = client.clone();
        let cluster = cluster.clone();
        let barrier = barrier.clone();
        let policy = policy.clone();
        let lease = lease_a.clone();
        let operation_id = leased_a.operation.id;
        tokio::spawn(async move {
            barrier.wait().await;
            client
                .reserve_lookup_table_cluster_budget(&cluster, operation_id, &lease, policy, 30, 30)
                .await
        })
    };
    let task_b = {
        let client = client.clone();
        let cluster = cluster.clone();
        let barrier = barrier.clone();
        let policy = policy.clone();
        let lease = lease_b.clone();
        let operation_id = leased_b.operation.id;
        tokio::spawn(async move {
            barrier.wait().await;
            client
                .reserve_lookup_table_cluster_budget(&cluster, operation_id, &lease, policy, 30, 30)
                .await
        })
    };
    barrier.wait().await;
    let result_a = task_a.await??;
    let result_b = task_b.await??;
    ensure(
        result_a.approved ^ result_b.approved,
        "concurrent cluster budget reservations did not produce exactly one winner",
    )?;
    let (winner, winner_operation, winner_lease) = if result_a.approved {
        (result_a, leased_a.operation.id, lease_a)
    } else {
        (result_b, leased_b.operation.id, lease_b)
    };
    ensure(
        winner.charged_lamports == 60
            && winner.reserved_lamports == 60
            && winner.remaining_lamports == 40,
        "durable cluster budget winner reported incorrect rolling-window accounting",
    )?;
    let replay = client
        .reserve_lookup_table_cluster_budget(
            &cluster,
            winner_operation,
            &winner_lease,
            policy,
            30,
            30,
        )
        .await?;
    ensure(
        replay.approved && replay.replayed && replay.reservation_id == winner.reservation_id,
        "same operation/fence budget reservation was not idempotent",
    )?;
    ensure(
        client
            .reserve_lookup_table_cluster_budget(
                &cluster,
                winner_operation,
                &winner_lease,
                LookupTableClusterBudgetPolicy {
                    max_lamports: 100,
                    rolling_window_seconds: 600,
                },
                31,
                30,
            )
            .await
            .is_err(),
        "same operation/fence budget reservation accepted conflicting accounting",
    )?;
    let persisted_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_cluster_budget_reservations WHERE cluster = $1",
    )
    .bind(&cluster)
    .fetch_one(client.pool())
    .await?;
    ensure(
        persisted_count == 1,
        "denied concurrent budget attempt persisted a reservation",
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
    publish_and_activate_shared_catalog(
        client,
        &cluster,
        typed_addresses(
            LookupTableManifestSubject::SharedMarket,
            1,
            "zero-stable-catalog",
        ),
        run,
        9_900,
    )
    .await?;
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

async fn verify_nonempty_eventual_satisfaction_and_requeue(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<()> {
    let cluster = format!("db-verify-eventual-{run}");
    let authority = unique_pubkey("eventual-authority").to_string();
    let (shared_family, _vault_family) = create_families(client, &cluster, &authority, 40).await?;
    let vault_id = create_vault(client, &format!("eventual-{run}"), 2).await?;
    let shared_addresses = typed_addresses(
        LookupTableManifestSubject::SharedMarket,
        2,
        "eventual-market",
    );
    publish_shared_catalog(client, &cluster, shared_addresses.clone(), run, 79_999).await?;
    let vault_addresses = typed_addresses(LookupTableManifestSubject::Vault, 3, "eventual-vault");
    let input = LookupTableProvisioningRequestUpsert {
        cluster: cluster.clone(),
        vault_id,
        route_fingerprint: format!("eventual-route-{run}"),
        requirements_fingerprint: format!("eventual-requirements-{run}"),
        shared_manifest_id: None,
        vault_manifest_id: None,
        desired_shared_hash: Some(format!("eventual-shared-hash-{run}")),
        desired_vault_hash: Some(format!("eventual-vault-hash-{run}")),
        shared_addresses: shared_addresses.clone(),
        vault_addresses: vault_addresses.clone(),
    };
    let request = client
        .upsert_lookup_table_provisioning_request(input.clone())
        .await?;
    let initial_address_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_provisioning_request_addresses WHERE request_id = $1",
    )
    .bind(request.id)
    .fetch_one(client.pool())
    .await?;

    let leased = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "eventual-planner-initial",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("non-empty request was not initially leaseable"))?;
    ensure(
        leased.id == request.id,
        "non-empty planner leased the wrong request",
    )?;
    let initial_plan = client
        .plan_lookup_table_provisioning_request(
            &cluster,
            leased.id,
            &request_lease(&leased)?,
            plan_policy(80_000),
        )
        .await?;
    ensure(
        initial_plan.request.request_status == LookupTableProvisioningRequestStatus::Queued
            && !initial_plan.shared_operations.is_empty(),
        "non-empty request did not queue its initial shared-market work",
    )?;
    let vault_binding = match &initial_plan.vault_allocation {
        AtomicVaultAllocationResult::CreateQueued {
            binding,
            operations,
            ..
        } if !operations.is_empty() => binding.clone(),
        other => {
            return fail(format!(
                "non-empty request did not queue its initial vault shard: {other:?}"
            ))
        }
    };

    let observed_slot = 80_001;
    let mut expected_table_ids = initial_plan
        .shared_operations
        .iter()
        .map(|operation| {
            operation
                .route_lookup_table_id
                .ok_or_else(|| io::Error::other("shared-market operation has no physical table"))
        })
        .collect::<Result<Vec<_>, _>>()?;
    for operation in &initial_plan.shared_operations {
        materialize_operation_manifest(client, operation, observed_slot).await?;
    }
    materialize_binding_manifest(client, &vault_binding, observed_slot).await?;
    expected_table_ids.push(vault_binding.route_lookup_table_id);
    expected_table_ids.sort_unstable();
    expected_table_ids.dedup();

    client
        .activate_lookup_table_family_generation(
            shared_family.id,
            initial_plan.shared_target_generation,
            Utc::now() + Duration::hours(1),
        )
        .await?;
    let active_binding = client
        .flip_lookup_table_binding_head(
            vault_binding.id,
            observed_slot,
            Utc::now() + Duration::hours(1),
        )
        .await?
        .active;

    let retry = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "eventual-planner-ready",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("materialized request was not re-leaseable"))?;
    ensure(
        retry.id == request.id,
        "materialized planner leased the wrong request",
    )?;
    let satisfied_plan = client
        .plan_lookup_table_provisioning_request(
            &cluster,
            retry.id,
            &request_lease(&retry)?,
            plan_policy(u64::try_from(observed_slot + 1)?),
        )
        .await?;
    ensure(
        satisfied_plan.request.request_status == LookupTableProvisioningRequestStatus::Satisfied
            && satisfied_plan.request.satisfied_at.is_some()
            && satisfied_plan.shared_operations.is_empty()
            && matches!(
                satisfied_plan.vault_allocation,
                AtomicVaultAllocationResult::Existing { ref binding }
                    if binding.id == active_binding.id
            ),
        "materialized non-empty request did not converge through production planning",
    )?;

    let required_addresses = shared_addresses
        .iter()
        .chain(vault_addresses.iter())
        .map(|address| address.address.clone())
        .collect::<std::collections::BTreeSet<_>>();
    let resolution = client
        .resolve_reusable_lookup_table_bundle(
            &cluster,
            vault_id,
            required_addresses.clone(),
            observed_slot + 1,
            16,
        )
        .await?;
    let mut selected_table_ids = resolution
        .tables
        .iter()
        .map(|table| table.table_id)
        .collect::<Vec<_>>();
    selected_table_ids.sort_unstable();
    ensure(
        resolution.required_addresses == required_addresses
            && resolution.missing_addresses.is_empty()
            && selected_table_ids == expected_table_ids,
        "satisfied request did not resolve through the exact shared and vault reusable tables",
    )?;

    let requeued = client
        .upsert_lookup_table_provisioning_request(input)
        .await?;
    let request_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_provisioning_requests WHERE cluster = $1 AND vault_id = $2 AND requirements_fingerprint = $3",
    )
    .bind(&cluster)
    .bind(vault_id.as_i64())
    .bind(&request.requirements_fingerprint)
    .fetch_one(client.pool())
    .await?;
    let final_address_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_provisioning_request_addresses WHERE request_id = $1",
    )
    .bind(request.id)
    .fetch_one(client.pool())
    .await?;
    ensure(
        requeued.id == request.id
            && requeued.request_status == LookupTableProvisioningRequestStatus::Requested
            && requeued.satisfied_at.is_none()
            && request_count == 1
            && final_address_count == initial_address_count,
        "re-upserting identical satisfied requirements did not reopen the same immutable request",
    )?;

    let requeued_lease = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "eventual-planner-requeued",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("requeued satisfied request was not leaseable"))?;
    let requeued_plan = client
        .plan_lookup_table_provisioning_request(
            &cluster,
            requeued_lease.id,
            &request_lease(&requeued_lease)?,
            plan_policy(u64::try_from(observed_slot + 2)?),
        )
        .await?;
    ensure(
        requeued_plan.request.id == request.id
            && requeued_plan.request.request_status
                == LookupTableProvisioningRequestStatus::Satisfied
            && requeued_plan.request.satisfied_at.is_some()
            && requeued_plan.shared_operations.is_empty()
            && matches!(
                requeued_plan.vault_allocation,
                AtomicVaultAllocationResult::Existing { .. }
            ),
        "reopened satisfied request did not reconverge without new physical work",
    )?;

    loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.route_lookup_tables
        SET last_verified_slot = NULL, last_verified_at = NULL, updated_at = now()
        WHERE id = $1
        "#,
    )
    .bind(active_binding.route_lookup_table_id)
    .execute(client.pool())
    .await?;
    loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_provisioning_requests
        SET request_status = 'requested', satisfied_at = NULL,
            requested_at = now(), updated_at = now()
        WHERE id = $1
        "#,
    )
    .bind(request.id)
    .execute(client.pool())
    .await?;
    let unverified_retry = client
        .lease_next_lookup_table_provisioning_request(
            &cluster,
            "eventual-planner-unverified-physical",
            Utc::now() + Duration::minutes(5),
        )
        .await?
        .ok_or_else(|| io::Error::other("unverified physical request was not leaseable"))?;
    let unverified_plan = client
        .plan_lookup_table_provisioning_request(
            &cluster,
            unverified_retry.id,
            &request_lease(&unverified_retry)?,
            plan_policy(u64::try_from(observed_slot + 3)?),
        )
        .await?;
    ensure(
        unverified_plan.request.request_status == LookupTableProvisioningRequestStatus::Queued
            && matches!(
                unverified_plan.vault_allocation,
                AtomicVaultAllocationResult::BindingReserved { ref binding, ref operations, .. }
                    if binding.id == active_binding.id
                        && operations.iter().any(|operation| {
                            operation.operation_kind == LookupTableOperationKind::Verify
                        })
            ),
        "matching manifest/revision falsely satisfied against an unverified active physical ALT",
    )
}

async fn verify_idle_predecision_deferral_evidence(
    client: &NeonSqlClient,
    run: &str,
) -> VerifyResult<()> {
    let cluster = format!("db-verify-idle-predecision-{run}");
    let authority = unique_pubkey("idle-predecision-authority").to_string();
    let (shared_family, _vault_family) = create_families(client, &cluster, &authority, 40).await?;
    let shared_addresses = typed_addresses(
        LookupTableManifestSubject::SharedMarket,
        1,
        "idle-predecision-shared",
    );
    let active_catalog = publish_and_activate_shared_catalog(
        client,
        &cluster,
        shared_addresses.clone(),
        run,
        81_000,
    )
    .await?;
    ensure(
        active_catalog.readiness_state == SharedMarketCatalogReadiness::Active,
        "idle predecision fixture did not start from an active shared catalog",
    )?;
    let vault_id = create_vault(client, &format!("idle-predecision-{run}"), 12).await?;
    let route_fingerprint = format!("idle-predecision-route-{run}");
    let requirements_fingerprint = format!("idle-predecision-requirements-{run}");
    let vault_addresses = typed_addresses(
        LookupTableManifestSubject::Vault,
        2,
        "idle-predecision-vault",
    );
    let missing = vault_addresses
        .iter()
        .map(|row| row.address.clone())
        .collect::<Vec<_>>();

    let decisions_before: (i64, i64) = loyal_yield_orchestrator::sqlx::query_as(
        r#"
        SELECT count(*), count(*) FILTER (WHERE signature IS NOT NULL)
        FROM loyal_yield.rebalance_decisions
        WHERE vault_id = $1
        "#,
    )
    .bind(vault_id.as_i64())
    .fetch_one(client.pool())
    .await?;

    client
        .upsert_lookup_table_readiness(LookupTableReadinessRecord {
            cluster: cluster.clone(),
            vault_id,
            route_fingerprint: route_fingerprint.clone(),
            requirements_fingerprint: requirements_fingerprint.clone(),
            route_kind: "idle_vault_deposit".to_owned(),
            source_reserve: None,
            target_reserve: Some("idle-target".to_owned()),
            manifest_id: None,
            shared_family_id: Some(shared_family.id),
            vault_binding_id: None,
            readiness_state: LookupTableReadinessStatus::Incomplete,
            required_address_count: i32::try_from(shared_addresses.len() + vault_addresses.len())?,
            covered_address_count: i32::try_from(shared_addresses.len())?,
            missing_addresses: json!(missing),
            legacy_table_ids: Vec::new(),
            reusable_table_ids: Vec::new(),
            compiled_message_size: None,
            packet_limit: Some(1232),
            observed_slot: Some(81_002),
            observed_at: Utc::now(),
            selection_kind: Some(LookupTableSelectionKind::Blocked),
            fallback_reason: Some("missing_vault_account".to_owned()),
            rollout_mode: Some(LookupTableRolloutMode::ReusableOnly),
            selected_table_ids: Vec::new(),
            selected_table_count: Some(0),
            packet_fits: None,
            simulation_state: Some(LookupTableSimulationState::NotRun),
            simulation_units_consumed: None,
            simulation_error: None,
            updated_at: Utc::now(),
        })
        .await?;
    let request = client
        .upsert_lookup_table_provisioning_request(LookupTableProvisioningRequestUpsert {
            cluster: cluster.clone(),
            vault_id,
            route_fingerprint: route_fingerprint.clone(),
            requirements_fingerprint: requirements_fingerprint.clone(),
            shared_manifest_id: Some(active_catalog.manifest_id),
            vault_manifest_id: None,
            desired_shared_hash: Some(active_catalog.desired_set_hash.clone()),
            desired_vault_hash: Some(lookup_table_manifest_address_records_hash(&vault_addresses)),
            shared_addresses,
            vault_addresses,
        })
        .await?;
    let readiness = client
        .lookup_table_readiness(
            &cluster,
            vault_id,
            &route_fingerprint,
            &requirements_fingerprint,
        )
        .await?
        .ok_or_else(|| io::Error::other("idle predecision readiness evidence disappeared"))?;
    let decisions_after: (i64, i64) = loyal_yield_orchestrator::sqlx::query_as(
        r#"
        SELECT count(*), count(*) FILTER (WHERE signature IS NOT NULL)
        FROM loyal_yield.rebalance_decisions
        WHERE vault_id = $1
        "#,
    )
    .bind(vault_id.as_i64())
    .fetch_one(client.pool())
    .await?;
    let allocated_bindings: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_vault_bindings WHERE vault_id = $1",
    )
    .bind(vault_id.as_i64())
    .fetch_one(client.pool())
    .await?;
    ensure(
        decisions_after == decisions_before
            && allocated_bindings == 0
            && request.sealed_at.is_some()
            && request.request_status == LookupTableProvisioningRequestStatus::Requested
            && request.error_code.is_none()
            && readiness.readiness_state == LookupTableReadinessStatus::Incomplete
            && readiness.selection_kind == Some(LookupTableSelectionKind::Blocked)
            && readiness.fallback_reason.as_deref() == Some("missing_vault_account")
            && readiness.simulation_state == Some(LookupTableSimulationState::NotRun),
        "idle missing-vault persistence did not remain predecision/no-send with one sealed request",
    )
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
    publish_shared_catalog(client, &cluster, shared_addresses.clone(), run, 1_999).await?;
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
    let shared_b = typed_addresses(
        LookupTableManifestSubject::SharedMarket,
        1,
        "cohort-market-b",
    );
    publish_shared_catalog(
        client,
        &cluster,
        normalize_catalog_addresses([shared_a.clone(), shared_b.clone()]),
        run,
        69_999,
    )
    .await?;
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
    materialize_table_manifest(
        client,
        binding.route_lookup_table_id,
        binding.manifest_id,
        observed_slot,
    )
    .await
}

async fn materialize_operation_manifest(
    client: &NeonSqlClient,
    operation: &LookupTableOperationRecord,
    observed_slot: i64,
) -> VerifyResult<()> {
    let table_id = operation
        .route_lookup_table_id
        .ok_or_else(|| io::Error::other("provisioning operation has no physical table"))?;
    let manifest_id = operation
        .manifest_id
        .ok_or_else(|| io::Error::other("provisioning operation has no manifest"))?;
    materialize_table_manifest(client, table_id, manifest_id, observed_slot).await
}

async fn materialize_table_manifest(
    client: &NeonSqlClient,
    table_id: i64,
    manifest_id: i64,
    observed_slot: i64,
) -> VerifyResult<()> {
    let manifest = client
        .lookup_table_manifest(manifest_id)
        .await?
        .ok_or_else(|| io::Error::other("table manifest disappeared during materialization"))?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.lookup_table_operations SET operation_state = 'cancelled' WHERE route_lookup_table_id = $1 AND operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')",
    )
    .bind(table_id)
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
        .reusable_lookup_table(table_id)
        .await?
        .ok_or_else(|| io::Error::other("physical table disappeared during materialization"))?;
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

async fn verify_legacy_import_audit(client: &NeonSqlClient, run: &str) -> VerifyResult<()> {
    let cluster = format!("db-verify-legacy-import-{run}");
    for ordinal in 0..2 {
        let addresses = vec![
            unique_pubkey(&format!("legacy-import-{ordinal}-a")).to_string(),
            unique_pubkey(&format!("legacy-import-{ordinal}-b")).to_string(),
        ];
        loyal_yield_orchestrator::sqlx::query(
            r#"
            INSERT INTO loyal_yield.route_lookup_tables
                (cluster, scope, table_address, authority, payer, status, durable,
                 address_count, address_hash, addresses, last_extended_slot,
                 last_extended_start_index, warmup_slot)
            VALUES ($1, $2, $3, $4, $4, 'usable', TRUE, $5, $6, $7, 90, 0, 91)
            "#,
        )
        .bind(&cluster)
        .bind(format!("legacy-import-scope-{ordinal}"))
        .bind(unique_pubkey(&format!("legacy-import-table-{ordinal}")).to_string())
        .bind(unique_pubkey(&format!("legacy-import-authority-{ordinal}")).to_string())
        .bind(i32::try_from(addresses.len())?)
        .bind(ordered_address_hash(&addresses))
        .bind(json!(addresses))
        .execute(client.pool())
        .await?;
    }

    let sources = client.legacy_lookup_tables_for_import(&cluster).await?;
    let first_request = legacy_import_request(&cluster, sources, 100, "first verified import")?;
    let first = client
        .import_verified_legacy_lookup_table_fleet(first_request)
        .await?;
    ensure(
        first.imported_table_count == 2 && !first.replayed,
        "fresh legacy fleet import did not persist both tables",
    )?;
    let imported_rows: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.route_lookup_tables
        WHERE cluster = $1
          AND legacy_kind = 'legacy_mixed'
          AND legacy_import_run_id = $2
          AND last_verified_slot = 100
        "#,
    )
    .bind(&cluster)
    .bind(first.import_run_id)
    .fetch_one(client.pool())
    .await?;
    let evidence_rows: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_legacy_import_evidence WHERE import_run_id = $1",
    )
    .bind(first.import_run_id)
    .fetch_one(client.pool())
    .await?;
    ensure(
        imported_rows == 2 && evidence_rows == 2,
        "legacy import registry pointers and immutable evidence are incomplete",
    )?;
    ensure(
        loyal_yield_orchestrator::sqlx::query(
            r#"
            INSERT INTO loyal_yield.lookup_table_legacy_import_evidence
                (import_run_id, route_lookup_table_id, table_address, scope,
                 legacy_kind, expected_authority, observed_authority,
                 observed_owner, observed_deactivation_slot,
                 observed_last_extended_slot,
                 observed_last_extended_start_index, address_count,
                 address_hash, addresses, verified_slot, verified_at)
            SELECT import_run_id, route_lookup_table_id, table_address, scope,
                   legacy_kind, expected_authority, observed_authority,
                   observed_owner, observed_deactivation_slot,
                   observed_last_extended_slot,
                   observed_last_extended_start_index, address_count,
                   address_hash, addresses, verified_slot, verified_at
            FROM loyal_yield.lookup_table_legacy_import_evidence
            WHERE import_run_id = $1
            ORDER BY route_lookup_table_id
            LIMIT 1
            "#,
        )
        .bind(first.import_run_id)
        .execute(client.pool())
        .await
        .is_err(),
        "legacy import evidence exceeded the run's approved fleet count",
    )?;

    let replay_sources = client.legacy_lookup_tables_for_import(&cluster).await?;
    let replay = client
        .import_verified_legacy_lookup_table_fleet(legacy_import_request(
            &cluster,
            replay_sources,
            100,
            "idempotent replay",
        )?)
        .await?;
    ensure(
        replay.replayed && replay.import_run_id == first.import_run_id,
        "exact legacy fleet replay was not an observable no-op",
    )?;
    let replay_run_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_legacy_import_runs WHERE cluster = $1",
    )
    .bind(&cluster)
    .fetch_one(client.pool())
    .await?;
    ensure(
        replay_run_count == 1,
        "exact legacy import replay duplicated immutable audit history",
    )?;

    let newer_sources = client.legacy_lookup_tables_for_import(&cluster).await?;
    let newer = client
        .import_verified_legacy_lookup_table_fleet(legacy_import_request(
            &cluster,
            newer_sources,
            101,
            "higher slot reverification",
        )?)
        .await?;
    ensure(
        !newer.replayed && newer.import_run_id != first.import_run_id,
        "higher-slot legacy reverification did not append a new audit run",
    )?;
    let stale_sources = client.legacy_lookup_tables_for_import(&cluster).await?;
    ensure(
        client
            .import_verified_legacy_lookup_table_fleet(legacy_import_request(
                &cluster,
                stale_sources,
                100,
                "stale reverification",
            )?)
            .await
            .is_err(),
        "legacy import accepted a verified-slot regression",
    )?;

    ensure(
        loyal_yield_orchestrator::sqlx::query(
            "UPDATE loyal_yield.lookup_table_legacy_import_runs SET reason = reason WHERE id = $1",
        )
        .bind(first.import_run_id)
        .execute(client.pool())
        .await
        .is_err(),
        "legacy import run audit row was mutable",
    )?;
    ensure(
        loyal_yield_orchestrator::sqlx::query(
            "UPDATE loyal_yield.lookup_table_legacy_import_evidence SET observed_owner = observed_owner WHERE import_run_id = $1",
        )
        .bind(first.import_run_id)
        .execute(client.pool())
        .await
        .is_err(),
        "legacy import per-table evidence was mutable",
    )?;
    ensure(
        loyal_yield_orchestrator::sqlx::query(
            "UPDATE loyal_yield.route_lookup_tables SET address_hash = $2 WHERE id = (SELECT min(id) FROM loyal_yield.route_lookup_tables WHERE cluster = $1)",
        )
        .bind(&cluster)
        .bind("0".repeat(64))
        .execute(client.pool())
        .await
        .is_err(),
        "imported legacy registry evidence could be changed without reverification",
    )?;

    let cleanup_source = client
        .legacy_lookup_tables_for_import(&cluster)
        .await?
        .into_iter()
        .next()
        .ok_or_else(|| io::Error::other("imported legacy cleanup fixture disappeared"))?;
    client
        .set_lookup_table_rollout_mode(
            &cluster,
            None,
            LookupTableRolloutMode::ReusableOnly,
            Some("db verifier legacy cleanup fence"),
            "db-verifier",
        )
        .await?;
    let cleanup_vault_id = create_vault(client, &format!("legacy-cleanup-{run}"), 19).await?;
    let cleanup_readiness_route = format!("legacy-cleanup-route-{run}");
    let cleanup_readiness_requirements = format!("legacy-cleanup-req-{run}");
    let cleanup_observed_at = Utc::now();
    client
        .upsert_lookup_table_readiness(LookupTableReadinessRecord {
            cluster: cluster.clone(),
            vault_id: cleanup_vault_id,
            route_fingerprint: cleanup_readiness_route.clone(),
            requirements_fingerprint: cleanup_readiness_requirements.clone(),
            route_kind: "db_verifier_imported_legacy_cleanup".to_owned(),
            source_reserve: None,
            target_reserve: None,
            manifest_id: None,
            shared_family_id: None,
            vault_binding_id: None,
            readiness_state: LookupTableReadinessStatus::Ready,
            required_address_count: 0,
            covered_address_count: 0,
            missing_addresses: json!([]),
            legacy_table_ids: vec![cleanup_source.id],
            reusable_table_ids: Vec::new(),
            compiled_message_size: Some(300),
            packet_limit: Some(1232),
            observed_slot: Some(149),
            observed_at: cleanup_observed_at,
            selection_kind: Some(LookupTableSelectionKind::Legacy),
            fallback_reason: Some("pre_cutover_legacy".to_owned()),
            rollout_mode: Some(LookupTableRolloutMode::PreferReusable),
            selected_table_ids: vec![cleanup_source.id],
            selected_table_count: Some(1),
            packet_fits: Some(true),
            simulation_state: Some(LookupTableSimulationState::Succeeded),
            simulation_units_consumed: Some(1),
            simulation_error: None,
            updated_at: cleanup_observed_at,
        })
        .await?;
    loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_route_readiness_current readiness
        SET updated_at = control.updated_at - interval '1 second'
        FROM loyal_yield.lookup_table_rollout_controls control
        WHERE readiness.cluster = $1 AND readiness.vault_id = $2
          AND readiness.route_fingerprint = $3
          AND readiness.requirements_fingerprint = $4
          AND control.cluster = readiness.cluster AND control.vault_id IS NULL
        "#,
    )
    .bind(&cluster)
    .bind(cleanup_vault_id.as_i64())
    .bind(&cleanup_readiness_route)
    .bind(&cleanup_readiness_requirements)
    .execute(client.pool())
    .await?;
    client
        .retire_legacy_route_lookup_table(LegacyLookupTableRetirementRequest {
            cluster: cluster.clone(),
            table_address: cleanup_source.table_address.clone(),
            expected_authority: cleanup_source.authority.clone(),
            expected_address_hash: cleanup_source.address_hash.clone(),
            expected_address_count: cleanup_source.address_count,
        })
        .await?;
    ensure(
        loyal_yield_orchestrator::sqlx::query(
            r#"
            UPDATE loyal_yield.lookup_table_route_readiness_current
            SET legacy_table_ids = ARRAY[$5]::BIGINT[],
                selected_table_ids = ARRAY[$5]::BIGINT[],
                selected_table_count = 1,
                selection_kind = 'legacy'
            WHERE cluster = $1 AND vault_id = $2
              AND route_fingerprint = $3 AND requirements_fingerprint = $4
            "#,
        )
        .bind(&cluster)
        .bind(cleanup_vault_id.as_i64())
        .bind(&cleanup_readiness_route)
        .bind(&cleanup_readiness_requirements)
        .bind(cleanup_source.id)
        .execute(client.pool())
        .await
        .is_err(),
        "post-retirement readiness update reacquired an imported legacy reference",
    )?;
    let deactivate_protection = client
        .legacy_lookup_table_cleanup_protection(&cluster, &cleanup_source.table_address)
        .await?
        .ok_or_else(|| io::Error::other("retired imported cleanup protection disappeared"))?;
    ensure(
        deactivate_protection.can_deactivate
            && deactivate_protection.zero_reference
            && deactivate_protection.nonselectable,
        "retired imported ALT was not authorized only after zero-reference retirement",
    )?;
    ensure(
        client
            .begin_legacy_lookup_table_cleanup_authorization(
                &cluster,
                &cleanup_source.table_address,
                &"0".repeat(64),
                LookupTableOperationKind::Deactivate,
            )
            .await
            .is_err(),
        "legacy cleanup accepted a stale authorization token",
    )?;
    let authorization = client
        .begin_legacy_lookup_table_cleanup_authorization(
            &cluster,
            &cleanup_source.table_address,
            &deactivate_protection.authorization_token,
            LookupTableOperationKind::Deactivate,
        )
        .await?;
    ensure(
        client
            .set_lookup_table_rollout_mode(
                &cluster,
                None,
                LookupTableRolloutMode::Shadow,
                Some("must fail during durable cleanup fence"),
                "db-verifier",
            )
            .await
            .is_err(),
        "rollout reversal bypassed the durable legacy cleanup fence",
    )?;
    authorization
        .record_finalized(VerifiedLegacyLookupTableCleanup {
            cluster: cluster.clone(),
            table_address: cleanup_source.table_address.clone(),
            expected_authorization_token: deactivate_protection.authorization_token.clone(),
            operation_kind: LookupTableOperationKind::Deactivate,
            transaction_signature: "db-verifier-deactivate-signature".to_owned(),
            observed_slot: 150,
            close_recipient: None,
            reclaimed_lamports: None,
        })
        .await?;
    client
        .set_lookup_table_rollout_mode(
            &cluster,
            None,
            LookupTableRolloutMode::ReusableOnly,
            Some("restore reusable-only before close"),
            "db-verifier",
        )
        .await?;
    let close_protection = client
        .legacy_lookup_table_cleanup_protection(&cluster, &cleanup_source.table_address)
        .await?
        .ok_or_else(|| io::Error::other("deactivated cleanup protection disappeared"))?;
    ensure(
        close_protection.can_close && !close_protection.can_deactivate,
        "recorded legacy deactivation did not transition to close-only authorization",
    )?;
    let invalid_close = client
        .begin_legacy_lookup_table_cleanup_authorization(
            &cluster,
            &cleanup_source.table_address,
            &close_protection.authorization_token,
            LookupTableOperationKind::Close,
        )
        .await?;
    ensure(
        invalid_close
            .record_finalized(VerifiedLegacyLookupTableCleanup {
                cluster: cluster.clone(),
                table_address: cleanup_source.table_address.clone(),
                expected_authorization_token: close_protection.authorization_token.clone(),
                operation_kind: LookupTableOperationKind::Close,
                transaction_signature: "db-verifier-invalid-close".to_owned(),
                observed_slot: 200,
                close_recipient: Some(unique_pubkey("wrong-refund-recipient").to_string()),
                reclaimed_lamports: Some(1),
            })
            .await
            .is_err(),
        "legacy close accepted a non-policy refund recipient",
    )?;
    let valid_close = client
        .begin_legacy_lookup_table_cleanup_authorization(
            &cluster,
            &cleanup_source.table_address,
            &close_protection.authorization_token,
            LookupTableOperationKind::Close,
        )
        .await?;
    valid_close
        .record_finalized(VerifiedLegacyLookupTableCleanup {
            cluster: cluster.clone(),
            table_address: cleanup_source.table_address.clone(),
            expected_authorization_token: close_protection.authorization_token,
            operation_kind: LookupTableOperationKind::Close,
            transaction_signature: "db-verifier-close-signature".to_owned(),
            observed_slot: 201,
            close_recipient: Some(cleanup_source.authority.clone()),
            reclaimed_lamports: Some(1_234_567),
        })
        .await?;
    let closed_status: String = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT status FROM loyal_yield.route_lookup_tables WHERE cluster = $1 AND table_address = $2",
    )
    .bind(&cluster)
    .bind(&cleanup_source.table_address)
    .fetch_one(client.pool())
    .await?;
    ensure(
        closed_status == "closed",
        "fenced finalized legacy close was not recorded",
    )?;

    let stale_cluster = format!("db-verify-legacy-import-stale-{run}");
    let stale_addresses = vec![unique_pubkey("legacy-stale-address").to_string()];
    loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.route_lookup_tables
            (cluster, scope, table_address, authority, payer, status, durable,
             address_count, address_hash, addresses, last_extended_slot,
             last_extended_start_index)
        VALUES ($1, 'stale-scope', $2, $3, $3, 'usable', TRUE, 1, $4, $5, 90, 0)
        "#,
    )
    .bind(&stale_cluster)
    .bind(unique_pubkey("legacy-stale-table").to_string())
    .bind(unique_pubkey("legacy-stale-authority").to_string())
    .bind(ordered_address_hash(&stale_addresses))
    .bind(json!(stale_addresses))
    .execute(client.pool())
    .await?;
    let stale_request = legacy_import_request(
        &stale_cluster,
        client
            .legacy_lookup_tables_for_import(&stale_cluster)
            .await?,
        100,
        "stale snapshot must fail",
    )?;
    loyal_yield_orchestrator::sqlx::query(
        "UPDATE loyal_yield.route_lookup_tables SET scope = 'changed-after-rpc' WHERE cluster = $1",
    )
    .bind(&stale_cluster)
    .execute(client.pool())
    .await?;
    ensure(
        client
            .import_verified_legacy_lookup_table_fleet(stale_request)
            .await
            .is_err(),
        "legacy import committed after its registry snapshot changed",
    )?;
    let stale_writes: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.lookup_table_legacy_import_runs WHERE cluster = $1",
    )
    .bind(&stale_cluster)
    .fetch_one(client.pool())
    .await?;
    ensure(stale_writes == 0, "failed fleet import left audit writes")
}

fn legacy_import_request(
    cluster: &str,
    sources: Vec<LegacyLookupTableImportSource>,
    verified_slot: i64,
    reason: &str,
) -> VerifyResult<LegacyLookupTableFleetImportRequest> {
    let tables = sources
        .into_iter()
        .map(|source| VerifiedLegacyLookupTableImport {
            observed_owner: solana_sdk::address_lookup_table::program::id().to_string(),
            observed_authority: source.authority.clone(),
            observed_deactivation_slot: u64::MAX.to_string(),
            observed_last_extended_slot: 90,
            observed_last_extended_start_index: 0,
            observed_address_count: source.address_count,
            observed_address_hash: source.address_hash.clone(),
            observed_addresses: source.addresses.clone(),
            source,
            legacy_kind: LegacyLookupTableKind::LegacyMixed,
        })
        .collect::<Vec<_>>();
    let import_fingerprint = legacy_lookup_table_import_fingerprint(
        cluster,
        "db-verifier-genesis",
        verified_slot,
        &tables,
    );
    Ok(LegacyLookupTableFleetImportRequest {
        cluster: cluster.to_owned(),
        rpc_genesis_hash: "db-verifier-genesis".to_owned(),
        verified_slot,
        verified_at: Utc::now(),
        import_fingerprint,
        reason: reason.to_owned(),
        updated_by: "db-verifier".to_owned(),
        expected_table_count: i32::try_from(tables.len())?,
        tables,
    })
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
    loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.lookup_table_route_readiness_current readiness
        SET updated_at = control.updated_at - interval '1 second'
        FROM loyal_yield.lookup_table_rollout_controls control
        WHERE readiness.cluster = $1 AND readiness.vault_id = $2
          AND readiness.route_fingerprint = $3
          AND readiness.requirements_fingerprint = $4
          AND control.cluster = readiness.cluster AND control.vault_id IS NULL
        "#,
    )
    .bind(&cluster)
    .bind(vault_id.as_i64())
    .bind(format!("legacy-route-{run}"))
    .bind(format!("legacy-req-{run}"))
    .execute(client.pool())
    .await?;
    let retired = client.retire_legacy_route_lookup_table(request).await?;
    ensure(
        retired.table_id == table_id && retired.status == "retiring" && !retired.durable,
        "legacy retirement did not atomically make the row non-selectable",
    )?;
    let remaining_evidence = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT legacy_table_ids, selected_table_ids, selected_table_count,
               selection_kind, fallback_reason
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
    let remaining_legacy_ids: Vec<i64> = remaining_evidence.try_get("legacy_table_ids")?;
    let remaining_selected_ids: Vec<i64> = remaining_evidence.try_get("selected_table_ids")?;
    ensure(
        !remaining_legacy_ids.contains(&table_id)
            && !remaining_selected_ids.contains(&table_id)
            && remaining_evidence.try_get::<Option<i32>, _>("selected_table_count")? == Some(0)
            && remaining_evidence
                .try_get::<Option<String>, _>("selection_kind")?
                .as_deref()
                == Some("blocked")
            && remaining_evidence
                .try_get::<Option<String>, _>("fallback_reason")?
                .as_deref()
                == Some("legacy_table_retired"),
        "legacy retirement left a stale selected or evidence reference",
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

fn normalize_catalog_addresses(
    groups: impl IntoIterator<Item = Vec<LookupTableManifestAddressRecord>>,
) -> Vec<LookupTableManifestAddressRecord> {
    let mut rows = groups.into_iter().flatten().collect::<Vec<_>>();
    rows.sort_by(|left, right| left.address.cmp(&right.address));
    rows.dedup_by(|left, right| left.address == right.address);
    for (ordinal, row) in rows.iter_mut().enumerate() {
        row.ordinal = ordinal as i32;
        row.semantic_class = LookupTableManifestSubject::SharedMarket;
    }
    rows
}

async fn publish_shared_catalog(
    client: &NeonSqlClient,
    cluster: &str,
    addresses: Vec<LookupTableManifestAddressRecord>,
    run: &str,
    source_slot: i64,
) -> VerifyResult<SharedMarketCatalogHeadRecord> {
    let desired_set_hash = lookup_table_manifest_address_records_hash(&addresses);
    let identity_hash = ordered_address_hash(
        &addresses
            .iter()
            .map(|row| row.address.clone())
            .collect::<Vec<_>>(),
    );
    Ok(client
        .upsert_shared_market_catalog(SharedMarketCatalogUpsert {
            cluster: cluster.to_owned(),
            catalog_version: "db-verifier-catalog-v1".to_owned(),
            desired_set_hash,
            enabled_mints_hash: identity_hash.clone(),
            reserve_set_hash: identity_hash,
            addresses,
            source_slot: Some(source_slot),
            source_observed_at: Some(Utc::now()),
            source_metadata: json!({"source": "isolated_db_verifier", "run": run}),
            reason: "isolated reusable ALT database verification".to_owned(),
            updated_by: "verify-reusable-alt-db".to_owned(),
        })
        .await?)
}

fn shared_catalog_policy(slot: i64) -> SharedMarketCatalogPlanPolicy {
    SharedMarketCatalogPlanPolicy {
        shared_shard_capacity: 40,
        max_extension_addresses: 20,
        operation_context: json!({"source": "isolated_db_verifier", "recent_slot": slot}),
        estimated_fee_lamports: Some(5_000),
        estimated_rent_lamports: Some(1_000_000),
    }
}

async fn publish_and_activate_shared_catalog(
    client: &NeonSqlClient,
    cluster: &str,
    addresses: Vec<LookupTableManifestAddressRecord>,
    run: &str,
    source_slot: i64,
) -> VerifyResult<SharedMarketCatalogHeadRecord> {
    let head = publish_shared_catalog(client, cluster, addresses, run, source_slot).await?;
    let plan = client
        .plan_shared_market_catalog_head(
            cluster,
            head.catalog_revision_id,
            shared_catalog_policy(source_slot),
        )
        .await?;
    for operation in &plan.shared_operations {
        materialize_operation_manifest(client, operation, source_slot + 1).await?;
    }
    Ok(client
        .reconcile_shared_market_catalog_head(
            cluster,
            head.catalog_revision_id,
            shared_catalog_policy(source_slot + 1),
            Utc::now() + Duration::hours(1),
        )
        .await?)
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
