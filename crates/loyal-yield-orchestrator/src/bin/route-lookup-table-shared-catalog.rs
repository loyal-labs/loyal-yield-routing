use chrono::Utc;
use loyal_yield_orchestrator::{
    derive_shared_market_catalog, enabled_stable_mints_hash, load_finalized_kamino_reserve_catalog,
    lookup_table_manifest_address_records_hash, resolve_enabled_stable_mints,
    rpc_safety::{
        redacted_external_error, redacted_rpc_endpoint, validate_rpc_endpoint,
        validate_rpc_genesis_hash,
    },
    DerivedSharedMarketCatalog, LookupTableFamilyKind, LookupTableManifestAddressRecord,
    LookupTableManifestSubject, NeonSqlClient, NeonSqlConfig, SharedMarketCatalogHeadRecord,
    SharedMarketCatalogPlanPolicy, SharedMarketCatalogUpsert, SupportedKaminoReserve,
    ENABLED_STABLE_MINTS_ENV, SHARED_MARKET_LOGICAL_CATALOG_MAX_ADDRESSES,
};
use loyal_yield_router::timescale::{TimescaleRouterClient, TimescaleRouterClientConfig};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_client::rpc_client::RpcClient;
use solana_sdk::commitment_config::CommitmentConfig;
use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    time::Duration,
};

const DATABASE_URL_ENV: &str = "NEON_DATABASE_URL";
const TIMESCALE_URL_ENV: &str = "TIMESCALEDB_URL";
const RPC_URL_ENV: &str = "SOLANA_RPC_URL";
const CLUSTER_ENV: &str = "YIELD_ALT_CLUSTER";
const CATALOG_VERSION_ENV: &str = "YIELD_ALT_CATALOG_VERSION";
const DEFAULT_ADDRESS_CHUNK: usize = 20;
const MAX_ADDRESS_CHUNK: usize = 20;

#[derive(Debug, Clone, PartialEq, Eq)]
struct Options {
    cluster: String,
    rpc_url: String,
    database_url: String,
    timescale_url: String,
    catalog_version: String,
    enabled_mints: Vec<String>,
    address_chunk: usize,
    admin_write: bool,
    reason: Option<String>,
    updated_by: Option<String>,
    approval_fence: Option<SharedCatalogApprovalFence>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct SharedCatalogApprovalFence {
    desired_set_hash: String,
    enabled_mints_hash: String,
    ordered_address_hash: String,
    reserve_set_hash: String,
    reserve_count: usize,
    address_count: usize,
    minimum_source_slot: i64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct RetentionSafeSharedCatalog {
    catalog: DerivedSharedMarketCatalog,
    eligible_address_count: usize,
    required_source_address_count: usize,
    source_only_address_count: usize,
    previously_approved_address_count: usize,
    retained_only_address_count: usize,
    appended_address_count: usize,
}

#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord)]
struct DurableSourceReserveReference {
    reserve: String,
    reasons: Vec<String>,
}

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!(
            "{}",
            json!({
                "event": "shared_market_catalog_fatal",
                "error": redacted_external_error(&error.to_string()),
                "signerLoaded": false,
                "transactionsSent": false,
            })
        );
        std::process::exit(1);
    }
}

async fn run() -> Result<(), Box<dyn Error>> {
    let options = parse_args(env::args().skip(1), |name| env::var(name).ok())?;
    validate_rpc_endpoint(&options.rpc_url)?;

    let timescale = TimescaleRouterClient::connect(
        TimescaleRouterClientConfig::new(options.timescale_url.clone())
            .with_max_connections(2)
            .with_schema("kamino"),
    )
    .await?;
    let supported_reserves = load_supported_reserves(&timescale, &options.enabled_mints).await?;
    let known_stable_reserves =
        load_known_stable_reserves(&timescale, &options.enabled_mints).await?;
    let supported_reserve_max_updated_at =
        supported_reserves.iter().map(|row| row.updated_at).max();

    let client = NeonSqlClient::connect(
        NeonSqlConfig::new(options.database_url.clone())
            .with_max_connections(2)
            .with_acquire_timeout(Duration::from_secs(10)),
    )
    .await?;
    let catalog_schema_applied =
        schema_migration_applied(&client, 20, "demand_driven_shared_market_catalog").await?;
    if options.admin_write && !catalog_schema_applied {
        client
            .require_schema_migration(20, "demand_driven_shared_market_catalog")
            .await?;
    }
    let required_source_reserves =
        load_durable_source_reserve_references(&options.cluster, client.pool()).await?;
    let required_source_reserve_addresses = required_source_reserves
        .iter()
        .map(|reference| reference.reserve.clone())
        .collect::<Vec<_>>();
    let required_source_catalog_rows =
        load_reserves_by_address(&timescale, &required_source_reserve_addresses).await?;
    let required_source_catalog_rows = canonical_required_source_reserves(
        &required_source_reserves,
        required_source_catalog_rows,
    )?;
    let required_physical_catalog_rows = merge_catalog_reserve_rows(
        known_stable_reserves.clone(),
        required_source_catalog_rows.clone(),
    )?;
    let finalized_catalog_rows = merge_catalog_reserve_rows(
        supported_reserves.clone(),
        required_physical_catalog_rows.clone(),
    )?;

    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.clone(), CommitmentConfig::finalized());
    let genesis_hash = rpc
        .get_genesis_hash()
        .map_err(|_| "failed to read genesis hash from configured shared-market catalog RPC")?;
    validate_rpc_genesis_hash(&options.cluster, genesis_hash).map_err(|error| {
        format!("refusing shared-market catalog derivation against mismatched RPC: {error}")
    })?;
    let finalized = load_finalized_kamino_reserve_catalog(&rpc, &finalized_catalog_rows)?;
    if finalized.source_slot != finalized.max_source_slot {
        return Err("shared-market catalog did not come from one finalized RPC snapshot".into());
    }
    let eligible_reserve_addresses = supported_reserves
        .iter()
        .map(|row| row.reserve.clone())
        .collect::<BTreeSet<_>>();
    let required_physical_reserve_addresses = required_physical_catalog_rows
        .iter()
        .map(|row| row.reserve.clone())
        .collect::<BTreeSet<_>>();
    let eligible_finalized_reserves = finalized
        .reserves
        .iter()
        .filter(|reserve| eligible_reserve_addresses.contains(&reserve.reserve.to_string()))
        .cloned()
        .collect::<Vec<_>>();
    let required_source_finalized_reserves = finalized
        .reserves
        .iter()
        .filter(|reserve| {
            required_physical_reserve_addresses.contains(&reserve.reserve.to_string())
        })
        .cloned()
        .collect::<Vec<_>>();
    let eligible_catalog = derive_shared_market_catalog(&eligible_finalized_reserves)?;
    let required_source_addresses = if required_source_finalized_reserves.is_empty() {
        Vec::new()
    } else {
        derive_shared_market_catalog(&required_source_finalized_reserves)?.addresses
    };
    let enabled_mints_hash = enabled_stable_mints_hash(&options.enabled_mints)?;
    let source_slot = i64::try_from(finalized.source_slot)
        .map_err(|_| "finalized shared-market catalog slot exceeds PostgreSQL BIGINT")?;
    let source_observed_at = Some(Utc::now());

    // The seeder is the sole shared-catalog publisher. Serialize its admin
    // publications before reading retained history, and keep the guard until
    // both the immutable revision and its provisioning plan are durable.
    // Dry runs remain lock-free; the exact admin fence is recomputed after
    // taking this lock and therefore rejects a stale dry-run approval.
    let mut publication_guard = if options.admin_write {
        let mut tx = client.pool().begin().await?;
        loyal_yield_orchestrator::sqlx::query(
            "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
        )
        .bind(format!(
            "loyal_yield:shared-market-catalog:{}",
            options.cluster
        ))
        .execute(&mut *tx)
        .await?;
        loyal_yield_orchestrator::sqlx::query(
            r#"
            LOCK TABLE loyal_yield.vault_reserve_positions_current,
                       loyal_yield.rebalance_decisions,
                       loyal_yield.lookup_table_route_readiness_current
            IN SHARE MODE
            "#,
        )
        .execute(&mut *tx)
        .await?;
        let locked_source_reserves =
            load_durable_source_reserve_references(&options.cluster, &mut *tx).await?;
        if locked_source_reserves != required_source_reserves {
            return Err(
                "durable held/in-flight source reserve set changed during finalized catalog derivation; rerun dry-run and approval"
                    .into(),
            );
        }
        Some(tx)
    } else {
        None
    };
    let families = client
        .active_lookup_table_families(&options.cluster)
        .await?;
    let shared_families = families
        .iter()
        .filter(|family| family.kind == LookupTableFamilyKind::SharedMarket)
        .collect::<Vec<_>>();
    if shared_families.len() != 1 {
        println!(
            "{}",
            json!({
                "event": "shared_market_catalog_derived",
                "mode": if options.admin_write { "admin_write_preflight" } else { "dry_run_preflight" },
                "cluster": options.cluster,
                "catalogVersion": options.catalog_version,
                "enabledMints": options.enabled_mints,
                "enabledMintsHash": enabled_mints_hash,
                "reserveSetHash": eligible_catalog.reserve_set_hash,
                "desiredSetHash": eligible_catalog.desired_set_hash,
                "orderedAddressHash": eligible_catalog.ordered_address_hash,
                "reserveCount": eligible_finalized_reserves.len(),
                "knownStableReserveCount": known_stable_reserves.len(),
                "requiredSourceReserveCount": required_source_reserves.len(),
                "addressCount": eligible_catalog.addresses.len(),
                "retentionHistoryApplied": false,
                "sourceSlot": source_slot,
                "sourceObservedAt": source_observed_at,
                "catalogSchemaApplied": catalog_schema_applied,
                "activeSharedFamilyCount": shared_families.len(),
                "databaseWrites": false,
                "signerLoaded": false,
                "transactionsSent": false,
                "vaultBackfillAttempted": false,
            })
        );
        return Err(format!(
            "cluster {:?} requires exactly one active shared-market ALT family, found {}; the validated finalized catalog contains {} reserves and {} addresses",
            options.cluster,
            shared_families.len(),
            eligible_finalized_reserves.len(),
            eligible_catalog.addresses.len(),
        )
        .into());
    }
    let shared_family = shared_families[0];
    if shared_family.catalog_version != options.catalog_version {
        return Err(format!(
            "catalog version {:?} does not match shared-market family version {:?}",
            options.catalog_version, shared_family.catalog_version
        )
        .into());
    }
    let current_head = if catalog_schema_applied {
        client.shared_market_catalog_head(&options.cluster).await?
    } else {
        None
    };
    let previously_approved_addresses = if catalog_schema_applied {
        load_previously_approved_shared_addresses(&client, shared_family.id).await?
    } else {
        Vec::new()
    };
    let retention = retain_previously_approved_shared_addresses(
        eligible_catalog,
        current_head
            .as_ref()
            .map_or(&[][..], |head| head.addresses.as_slice()),
        &previously_approved_addresses,
        &required_source_addresses,
    )?;
    let catalog = &retention.catalog;
    verify_approval_fence(
        options.approval_fence.as_ref(),
        &catalog.desired_set_hash,
        &enabled_mints_hash,
        &catalog.ordered_address_hash,
        &catalog.reserve_set_hash,
        eligible_finalized_reserves.len(),
        catalog.addresses.len(),
        source_slot,
    )?;
    let shared_high_water = usize::try_from(shared_family.allocation_high_water)
        .map_err(|_| "shared-market family allocation high-water is invalid")?;
    if shared_high_water == 0 {
        return Err("shared-market family allocation high-water must be positive".into());
    }
    let shared_physical_shard_count = catalog
        .addresses
        .len()
        .saturating_add(shared_high_water - 1)
        / shared_high_water;
    let source_metadata = json!({
        "source": "kamino.supported_reserves+neon_source_references+finalized_rpc",
        "riskBasket": "safe",
        "enabledMints": options.enabled_mints,
        "enabledMintsHash": enabled_mints_hash,
        "reserveSetHash": catalog.reserve_set_hash,
        "orderedAddressHash": catalog.ordered_address_hash,
        "reserveCount": supported_reserves.len(),
        "knownStableReserveCount": known_stable_reserves.len(),
        "addressCount": catalog.addresses.len(),
        "physicalShardCount": shared_physical_shard_count,
        "physicalShardCapacity": shared_high_water,
        "eligibleAddressCount": retention.eligible_address_count,
        "requiredSourceAddressCount": retention.required_source_address_count,
        "sourceOnlyAddressCount": retention.source_only_address_count,
        "requiredSourceReserveCount": required_source_reserves.len(),
        "previouslyApprovedAddressCount": retention.previously_approved_address_count,
        "retainedOnlyAddressCount": retention.retained_only_address_count,
        "appendedAddressCount": retention.appended_address_count,
        "supportedReserveMaxUpdatedAt": supported_reserve_max_updated_at,
        "rpcCommitment": "finalized",
        "rpcContextSlot": source_slot,
        "rpcGenesisHash": genesis_hash.to_string(),
        "rpcEndpoint": redacted_rpc_endpoint(&options.rpc_url),
        "supportedReserveQuery": {
            "active": true,
            "riskBasket": "safe",
            "liquidityMintFilter": "enabled_stable_subset_only",
            "purpose": "new_target_eligibility_only",
            "apyFilter": false,
            "freshnessFilter": false,
            "liquidityFilter": false,
        },
        "retentionPolicy": {
            "mode": "append_only_until_durable_zero_reference_proof",
            "historySource": "all_durable_shared_catalog_revisions",
            "bootstrapSource": "all_known_enabled_stable_reserves+nonzero_current_positions+inflight_decisions+route_readiness",
            "previouslyApprovedOrder": "current_head_prefix_then_historical_first_approval",
            "roleMerge": "set_union",
            "writableMerge": "logical_or",
            "removalProofAvailable": false,
        },
        "requiredSourceReserves": required_source_reserves.iter().map(|reference| json!({
            "reserve": reference.reserve,
            "reasons": reference.reasons,
        })).collect::<Vec<_>>(),
        "requiredSourceCatalogRows": required_source_catalog_rows,
        "knownStableReserves": known_stable_reserves,
        "supportedReserves": supported_reserves,
    });
    let base_output = json!({
        "event": "shared_market_catalog",
        "mode": if options.admin_write { "admin_write" } else { "dry_run" },
        "cluster": options.cluster,
        "catalogVersion": options.catalog_version,
        "enabledMints": options.enabled_mints,
        "enabledMintsHash": enabled_mints_hash,
        "reserveSetHash": catalog.reserve_set_hash,
        "desiredSetHash": catalog.desired_set_hash,
        "orderedAddressHash": catalog.ordered_address_hash,
        "reserveCount": eligible_finalized_reserves.len(),
        "knownStableReserveCount": known_stable_reserves.len(),
        "requiredSourceReserveCount": required_source_reserves.len(),
        "addressCount": catalog.addresses.len(),
        "eligibleAddressCount": retention.eligible_address_count,
        "requiredSourceAddressCount": retention.required_source_address_count,
        "sourceOnlyAddressCount": retention.source_only_address_count,
        "previouslyApprovedAddressCount": retention.previously_approved_address_count,
        "retainedOnlyAddressCount": retention.retained_only_address_count,
        "appendedAddressCount": retention.appended_address_count,
        "retentionHistoryApplied": catalog_schema_applied,
        "retentionPolicy": {
            "mode": "append_only_until_durable_zero_reference_proof",
            "newTargetEligibility": "active_safe_enabled_stable_reserves",
            "requiredSources": "all_known_enabled_stable_reserves_plus_neon_nonzero_holdings_and_inflight_references",
            "removalProofAvailable": false,
        },
        "requiredSourceReserves": required_source_reserves.iter().map(|reference| json!({
            "reserve": reference.reserve,
            "reasons": reference.reasons,
        })).collect::<Vec<_>>(),
        "sharedFamilyId": shared_family.id,
        "sharedFamilyAllocationHighWater": shared_family.allocation_high_water,
        "sharedPhysicalShardCount": shared_physical_shard_count,
        "sourceSlot": source_slot,
        "approvalFence": {
            "expectedDesiredSetHash": catalog.desired_set_hash,
            "expectedEnabledMintsHash": enabled_mints_hash,
            "expectedOrderedAddressHash": catalog.ordered_address_hash,
            "expectedReserveSetHash": catalog.reserve_set_hash,
            "expectedReserveCount": eligible_finalized_reserves.len(),
            "expectedAddressCount": catalog.addresses.len(),
            "expectedMinimumSourceSlot": source_slot,
            "provided": options.approval_fence.is_some(),
            "matches": options.approval_fence.is_some(),
        },
        "sourceObservedAt": source_observed_at,
        "supportedReserveMaxUpdatedAt": supported_reserve_max_updated_at,
        "catalogSchemaApplied": catalog_schema_applied,
        "currentHeadUnavailableReason": (!catalog_schema_applied)
            .then_some("migration_20_not_applied"),
        "currentHead": current_head.as_ref().map(catalog_head_json),
        "headMatchesDerivedCatalog": current_head.as_ref().is_some_and(|head| {
            head.catalog_version == options.catalog_version
                && head.desired_set_hash == catalog.desired_set_hash
                && head.enabled_mints_hash == enabled_mints_hash
                && head.reserve_set_hash == catalog.reserve_set_hash
                && head.addresses == catalog.addresses
        }),
        "addresses": catalog.addresses.iter().map(|row| json!({
            "address": row.address,
            "ordinal": row.ordinal,
            "semanticClass": row.semantic_class.as_str(),
            "accountRole": row.account_role,
            "isWritable": row.is_writable,
        })).collect::<Vec<_>>(),
        "signerLoaded": false,
        "transactionsSent": false,
        "vaultBackfillAttempted": false,
    });

    if !options.admin_write {
        println!("{}", serde_json::to_string_pretty(&base_output)?);
        return Ok(());
    }

    let reason = options.reason.as_deref().expect("validated by parser");
    let updated_by = options.updated_by.as_deref().expect("validated by parser");
    let head = client
        .upsert_shared_market_catalog(SharedMarketCatalogUpsert {
            cluster: options.cluster.clone(),
            catalog_version: options.catalog_version.clone(),
            desired_set_hash: catalog.desired_set_hash.clone(),
            enabled_mints_hash: enabled_mints_hash.clone(),
            reserve_set_hash: catalog.reserve_set_hash.clone(),
            addresses: catalog.addresses.clone(),
            source_slot: Some(source_slot),
            source_observed_at,
            source_metadata,
            reason: reason.to_owned(),
            updated_by: updated_by.to_owned(),
        })
        .await?;
    let plan = client
        .plan_shared_market_catalog_head(
            &options.cluster,
            head.catalog_revision_id,
            SharedMarketCatalogPlanPolicy {
                shared_shard_capacity: u16::try_from(shared_family.allocation_high_water)
                    .map_err(|_| "shared-market family high-water does not fit planner capacity")?,
                max_extension_addresses: options.address_chunk,
                operation_context: json!({
                    "source": "shared_market_catalog_seeder",
                    "catalog_revision_id": head.catalog_revision_id,
                    "catalog_revision": head.catalog_revision,
                    "desired_set_hash": head.desired_set_hash,
                    "recent_slot": source_slot,
                    "reason": reason,
                    "updated_by": updated_by,
                }),
                estimated_fee_lamports: None,
                estimated_rent_lamports: None,
            },
        )
        .await?;
    let mut output = base_output;
    let output_object = output
        .as_object_mut()
        .ok_or("shared-market catalog output was not a JSON object")?;
    output_object.insert("persistedHead".to_owned(), catalog_head_json(&head));
    output_object.insert(
        "plan".to_owned(),
        json!({
            "catalogRevisionId": plan.catalog.catalog_revision_id,
            "catalogRevision": plan.catalog.catalog_revision,
            "targetGeneration": plan.shared_target_generation,
            "operationCount": plan.shared_operations.len(),
            "operations": plan.shared_operations.iter().map(|operation| json!({
                "id": operation.id,
                "kind": operation.operation_kind.as_str(),
                "state": operation.operation_state.as_str(),
                "routeLookupTableId": operation.route_lookup_table_id,
            })).collect::<Vec<_>>(),
        }),
    );
    output_object.insert("reason".to_owned(), Value::from(reason));
    output_object.insert("updatedBy".to_owned(), Value::from(updated_by));
    if let Some(guard) = publication_guard.take() {
        guard.commit().await?;
    }
    println!("{}", serde_json::to_string_pretty(&output)?);
    Ok(())
}

async fn schema_migration_applied(
    client: &NeonSqlClient,
    version: i64,
    name: &str,
) -> Result<bool, loyal_yield_orchestrator::sqlx::Error> {
    let ledger_exists: bool = loyal_yield_orchestrator::sqlx::query_scalar(
        "SELECT to_regclass('loyal_yield.schema_migrations') IS NOT NULL",
    )
    .fetch_one(client.pool())
    .await?;
    if !ledger_exists {
        return Ok(false);
    }
    loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM loyal_yield.schema_migrations
            WHERE version = $1
              AND name = $2
        )
        "#,
    )
    .bind(version)
    .bind(name)
    .fetch_one(client.pool())
    .await
}

async fn load_durable_source_reserve_references<'e, E>(
    cluster: &str,
    executor: E,
) -> Result<Vec<DurableSourceReserveReference>, loyal_yield_orchestrator::sqlx::Error>
where
    E: loyal_yield_orchestrator::sqlx::Executor<
        'e,
        Database = loyal_yield_orchestrator::sqlx::Postgres,
    >,
{
    let rows = loyal_yield_orchestrator::sqlx::query_as::<_, (String, String)>(
        r#"
        SELECT source.reserve, source.reason
        FROM (
            SELECT position.reserve,
                   'current_nonzero_position'::TEXT AS reason
            FROM loyal_yield.vault_reserve_positions_current position
            WHERE position.has_value OR position.amount_raw > 0

            UNION ALL

            SELECT decision.source_reserve AS reserve,
                   'inflight_rebalance_decision'::TEXT AS reason
            FROM loyal_yield.rebalance_decisions decision
            WHERE decision.source_reserve IS NOT NULL
              AND (
                  decision.status IN (
                      'planned', 'simulating', 'ready', 'submitted', 'confirming'
                  )
                  OR (decision.status = 'confirmed' AND decision.post_snapshot_id IS NULL)
              )

            UNION ALL

            SELECT readiness.source_reserve AS reserve,
                   'route_readiness_reference'::TEXT AS reason
            FROM loyal_yield.lookup_table_route_readiness_current readiness
            WHERE readiness.source_reserve IS NOT NULL
              AND readiness.cluster = $1
        ) source
        WHERE length(btrim(source.reserve)) > 0
        ORDER BY source.reserve, source.reason
        "#,
    )
    .bind(cluster)
    .fetch_all(executor)
    .await?;
    let mut reasons_by_reserve = BTreeMap::<String, BTreeSet<String>>::new();
    for (reserve, reason) in rows {
        reasons_by_reserve
            .entry(reserve)
            .or_default()
            .insert(reason);
    }
    Ok(reasons_by_reserve
        .into_iter()
        .map(|(reserve, reasons)| DurableSourceReserveReference {
            reserve,
            reasons: reasons.into_iter().collect(),
        })
        .collect())
}

fn canonical_required_source_reserves(
    required: &[DurableSourceReserveReference],
    rows: Vec<SupportedKaminoReserve>,
) -> Result<Vec<SupportedKaminoReserve>, Box<dyn Error>> {
    let required_addresses = required
        .iter()
        .map(|reference| reference.reserve.as_str())
        .collect::<BTreeSet<_>>();
    let mut canonical = BTreeMap::<String, SupportedKaminoReserve>::new();
    for row in rows {
        if !required_addresses.contains(row.reserve.as_str()) {
            continue;
        }
        if let Some(existing) = canonical.get(&row.reserve) {
            if existing.market != row.market || existing.liquidity_mint != row.liquidity_mint {
                return Err(format!(
                    "required source reserve {} has conflicting market/mint identities",
                    row.reserve
                )
                .into());
            }
            continue;
        }
        canonical.insert(row.reserve.clone(), row);
    }
    let missing = required_addresses
        .into_iter()
        .filter(|reserve| !canonical.contains_key(*reserve))
        .collect::<Vec<_>>();
    if !missing.is_empty() {
        return Err(format!(
            "durable held/in-flight source reserve catalog lookup is incomplete for {} reserve(s)",
            missing.len()
        )
        .into());
    }
    Ok(canonical.into_values().collect())
}

fn merge_catalog_reserve_rows(
    left: Vec<SupportedKaminoReserve>,
    right: Vec<SupportedKaminoReserve>,
) -> Result<Vec<SupportedKaminoReserve>, Box<dyn Error>> {
    let mut merged = BTreeMap::<String, SupportedKaminoReserve>::new();
    for row in left.into_iter().chain(right) {
        if let Some(existing) = merged.get(&row.reserve) {
            if existing.market != row.market || existing.liquidity_mint != row.liquidity_mint {
                return Err(format!(
                    "catalog reserve {} has conflicting market/mint identities",
                    row.reserve
                )
                .into());
            }
            continue;
        }
        merged.insert(row.reserve.clone(), row);
    }
    Ok(merged.into_values().collect())
}

async fn load_previously_approved_shared_addresses(
    client: &NeonSqlClient,
    family_id: i64,
) -> Result<Vec<LookupTableManifestAddressRecord>, loyal_yield_orchestrator::sqlx::Error> {
    let rows = loyal_yield_orchestrator::sqlx::query_as::<_, (String, String, bool)>(
        r#"
        SELECT address.address,
               address.account_role,
               address.is_writable
        FROM loyal_yield.lookup_table_shared_market_catalog_revisions revision
        JOIN loyal_yield.lookup_table_manifest_addresses address
          ON address.manifest_id = revision.manifest_id
        WHERE revision.family_id = $1
          AND address.semantic_class = 'shared_market'
        ORDER BY revision.catalog_revision, address.ordinal
        "#,
    )
    .bind(family_id)
    .fetch_all(client.pool())
    .await?;
    Ok(rows
        .into_iter()
        .map(
            |(address, account_role, is_writable)| LookupTableManifestAddressRecord {
                address,
                // The retention merge assigns the canonical physical ordinal
                // after deduplicating repeated immutable revisions.
                ordinal: 0,
                semantic_class: LookupTableManifestSubject::SharedMarket,
                account_role,
                is_writable,
            },
        )
        .collect())
}

fn retain_previously_approved_shared_addresses(
    eligible: DerivedSharedMarketCatalog,
    current_head: &[LookupTableManifestAddressRecord],
    catalog_history: &[LookupTableManifestAddressRecord],
    required_sources: &[LookupTableManifestAddressRecord],
) -> Result<RetentionSafeSharedCatalog, Box<dyn Error>> {
    let eligible_addresses = eligible
        .addresses
        .iter()
        .map(|record| record.address.clone())
        .collect::<BTreeSet<_>>();
    let required_source_addresses = required_sources
        .iter()
        .map(|record| record.address.clone())
        .collect::<BTreeSet<_>>();
    let desired_addresses = eligible_addresses
        .union(&required_source_addresses)
        .cloned()
        .collect::<BTreeSet<_>>();
    let previously_approved_addresses = current_head
        .iter()
        .chain(catalog_history.iter())
        .map(|record| record.address.clone())
        .collect::<BTreeSet<_>>();
    let mut addresses = Vec::<LookupTableManifestAddressRecord>::new();
    let mut index_by_address = BTreeMap::<String, usize>::new();

    // The current durable head fixes the physical ALT prefix. Historical-only
    // addresses then follow their first publication order, recovering any
    // address an older publisher may have dropped without reordering the
    // current table. Later revisions may only widen role/writability metadata.
    // Freshly eligible addresses append last, regardless of lexicographic
    // order.
    for record in current_head
        .iter()
        .chain(catalog_history.iter())
        .chain(required_sources.iter())
        .chain(eligible.addresses.iter())
    {
        if record.semantic_class != LookupTableManifestSubject::SharedMarket
            || record.address.trim().is_empty()
            || record.account_role.trim().is_empty()
        {
            return Err("shared-market retention input contains an invalid address record".into());
        }
        if let Some(index) = index_by_address.get(&record.address).copied() {
            let retained = &mut addresses[index];
            retained.account_role =
                canonical_role_union(retained.account_role.as_str(), record.account_role.as_str())?;
            retained.is_writable |= record.is_writable;
            continue;
        }
        let mut retained = record.clone();
        retained.ordinal = i32::try_from(addresses.len())
            .map_err(|_| "shared-market retention address count exceeds PostgreSQL INTEGER")?;
        retained.account_role = canonical_role_union("", retained.account_role.as_str())?;
        index_by_address.insert(retained.address.clone(), addresses.len());
        addresses.push(retained);
    }

    let desired_set_hash = lookup_table_manifest_address_records_hash(&addresses);
    let ordered_address_hash =
        length_prefixed_hash(addresses.iter().map(|record| record.address.as_str()));
    Ok(RetentionSafeSharedCatalog {
        catalog: DerivedSharedMarketCatalog {
            addresses,
            desired_set_hash,
            ordered_address_hash,
            // Reserve membership describes which markets may receive new
            // deposits. It intentionally does not authorize removal of
            // previously approved source/exit accounts from the ALT.
            reserve_set_hash: eligible.reserve_set_hash,
        },
        eligible_address_count: eligible_addresses.len(),
        required_source_address_count: required_source_addresses.len(),
        source_only_address_count: required_source_addresses
            .difference(&eligible_addresses)
            .count(),
        previously_approved_address_count: previously_approved_addresses.len(),
        retained_only_address_count: previously_approved_addresses
            .difference(&eligible_addresses)
            .count(),
        appended_address_count: desired_addresses
            .difference(&previously_approved_addresses)
            .count(),
    })
}

fn canonical_role_union(left: &str, right: &str) -> Result<String, Box<dyn Error>> {
    let roles = left
        .split(',')
        .chain(right.split(','))
        .map(str::trim)
        .filter(|role| !role.is_empty())
        .collect::<BTreeSet<_>>();
    if roles.is_empty() {
        return Err("shared-market retention role set must not be empty".into());
    }
    Ok(roles.into_iter().collect::<Vec<_>>().join(","))
}

fn length_prefixed_hash<'a>(values: impl IntoIterator<Item = &'a str>) -> String {
    let mut hasher = Sha256::new();
    for value in values {
        hasher.update((value.len() as u64).to_le_bytes());
        hasher.update(value.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

async fn load_supported_reserves(
    timescale: &TimescaleRouterClient,
    enabled_mints: &[String],
) -> Result<Vec<SupportedKaminoReserve>, loyal_yield_orchestrator::sqlx::Error> {
    loyal_yield_orchestrator::sqlx::query_as::<_, SupportedKaminoReserve>(
        r#"
        SELECT sr.market,
               sr.liquidity_mint,
               sr.reserve,
               sr.market_name,
               sr.symbol,
               sr.updated_at
        FROM kamino.supported_reserves sr
        WHERE sr.active = TRUE
          AND 'safe' = ANY(sr.risk_baskets)
          AND sr.liquidity_mint = ANY($1::TEXT[])
        ORDER BY sr.reserve, sr.market, sr.liquidity_mint
        "#,
    )
    .bind(enabled_mints)
    .fetch_all(timescale.pool())
    .await
}

async fn load_known_stable_reserves(
    timescale: &TimescaleRouterClient,
    enabled_mints: &[String],
) -> Result<Vec<SupportedKaminoReserve>, loyal_yield_orchestrator::sqlx::Error> {
    loyal_yield_orchestrator::sqlx::query_as::<_, SupportedKaminoReserve>(
        r#"
        SELECT sr.market,
               sr.liquidity_mint,
               sr.reserve,
               sr.market_name,
               sr.symbol,
               sr.updated_at
        FROM kamino.supported_reserves sr
        WHERE sr.liquidity_mint = ANY($1::TEXT[])
        ORDER BY sr.reserve, sr.market, sr.liquidity_mint
        "#,
    )
    .bind(enabled_mints)
    .fetch_all(timescale.pool())
    .await
}

async fn load_reserves_by_address(
    timescale: &TimescaleRouterClient,
    reserves: &[String],
) -> Result<Vec<SupportedKaminoReserve>, loyal_yield_orchestrator::sqlx::Error> {
    loyal_yield_orchestrator::sqlx::query_as::<_, SupportedKaminoReserve>(
        r#"
        SELECT sr.market,
               sr.liquidity_mint,
               sr.reserve,
               sr.market_name,
               sr.symbol,
               sr.updated_at
        FROM kamino.supported_reserves sr
        WHERE sr.reserve = ANY($1::TEXT[])
        ORDER BY sr.reserve, sr.market, sr.liquidity_mint
        "#,
    )
    .bind(reserves)
    .fetch_all(timescale.pool())
    .await
}

fn catalog_head_json(head: &SharedMarketCatalogHeadRecord) -> Value {
    json!({
        "familyId": head.family_id,
        "catalogRevisionId": head.catalog_revision_id,
        "catalogRevision": head.catalog_revision,
        "catalogVersion": head.catalog_version,
        "desiredSetHash": head.desired_set_hash,
        "enabledMintsHash": head.enabled_mints_hash,
        "reserveSetHash": head.reserve_set_hash,
        "addressCount": head.address_count,
        "sourceSlot": head.source_slot,
        "sourceObservedAt": head.source_observed_at,
        "activeGeneration": head.active_generation,
        "targetGeneration": head.target_generation,
        "readinessState": head.readiness_state.as_str(),
        "activatedAt": head.activated_at,
        "createdAt": head.created_at,
        "updatedAt": head.updated_at,
    })
}

fn parse_args<I, S, F>(args: I, read_env: F) -> Result<Options, Box<dyn Error>>
where
    I: IntoIterator<Item = S>,
    S: Into<String>,
    F: Fn(&str) -> Option<String>,
{
    let mut cluster = read_env(CLUSTER_ENV);
    let mut rpc_url = read_env(RPC_URL_ENV);
    let database_url = read_env(DATABASE_URL_ENV)
        .ok_or("NEON_DATABASE_URL is required for shared catalog head readback")?;
    let timescale_url = read_env(TIMESCALE_URL_ENV)
        .ok_or("TIMESCALEDB_URL is required for supported reserve discovery")?;
    let mut catalog_version = read_env(CATALOG_VERSION_ENV);
    let mut enabled_mints_raw = read_env(ENABLED_STABLE_MINTS_ENV);
    let mut address_chunk = DEFAULT_ADDRESS_CHUNK;
    let mut admin_write = false;
    let mut reason = None;
    let mut updated_by = None;
    let mut expected_desired_set_hash = None;
    let mut expected_enabled_mints_hash = None;
    let mut expected_ordered_address_hash = None;
    let mut expected_reserve_set_hash = None;
    let mut expected_reserve_count = None;
    let mut expected_address_count = None;
    let mut expected_minimum_source_slot = None;

    let mut args = args.into_iter().map(Into::into);
    while let Some(argument) = args.next() {
        match argument.as_str() {
            "--cluster" => cluster = Some(next_value(&mut args, "--cluster")?),
            "--rpc-url" => rpc_url = Some(next_value(&mut args, "--rpc-url")?),
            "--catalog-version" => {
                catalog_version = Some(next_value(&mut args, "--catalog-version")?)
            }
            "--enabled-stable-mints" => {
                enabled_mints_raw = Some(next_value(&mut args, "--enabled-stable-mints")?)
            }
            "--address-chunk" => {
                address_chunk = next_value(&mut args, "--address-chunk")?
                    .parse()
                    .map_err(|_| "--address-chunk must be an integer")?;
            }
            "--admin-write" => admin_write = true,
            "--reason" => reason = Some(next_value(&mut args, "--reason")?),
            "--updated-by" => updated_by = Some(next_value(&mut args, "--updated-by")?),
            "--expected-desired-set-hash" => {
                expected_desired_set_hash =
                    Some(next_value(&mut args, "--expected-desired-set-hash")?)
            }
            "--expected-enabled-mints-hash" => {
                expected_enabled_mints_hash =
                    Some(next_value(&mut args, "--expected-enabled-mints-hash")?)
            }
            "--expected-ordered-address-hash" => {
                expected_ordered_address_hash =
                    Some(next_value(&mut args, "--expected-ordered-address-hash")?)
            }
            "--expected-reserve-set-hash" => {
                expected_reserve_set_hash =
                    Some(next_value(&mut args, "--expected-reserve-set-hash")?)
            }
            "--expected-reserve-count" => {
                expected_reserve_count = Some(
                    next_value(&mut args, "--expected-reserve-count")?
                        .parse()
                        .map_err(|_| "--expected-reserve-count must be an integer")?,
                )
            }
            "--expected-address-count" => {
                expected_address_count = Some(
                    next_value(&mut args, "--expected-address-count")?
                        .parse()
                        .map_err(|_| "--expected-address-count must be an integer")?,
                )
            }
            "--expected-minimum-source-slot" => {
                expected_minimum_source_slot = Some(
                    next_value(&mut args, "--expected-minimum-source-slot")?
                        .parse()
                        .map_err(|_| "--expected-minimum-source-slot must be an integer")?,
                )
            }
            "--help" | "-h" => return Err(usage().into()),
            other => return Err(format!("unknown argument {other:?}\n{}", usage()).into()),
        }
    }

    let cluster = required_nonempty(cluster, "YIELD_ALT_CLUSTER or --cluster is required")?;
    let rpc_url = required_nonempty(rpc_url, "SOLANA_RPC_URL or --rpc-url is required")?;
    let catalog_version = required_nonempty(
        catalog_version,
        "YIELD_ALT_CATALOG_VERSION or --catalog-version is required",
    )?;
    if !(1..=MAX_ADDRESS_CHUNK).contains(&address_chunk) {
        return Err(format!("--address-chunk must be between 1 and {MAX_ADDRESS_CHUNK}").into());
    }
    if admin_write {
        required_nonempty(reason.clone(), "--admin-write requires --reason")?;
        required_nonempty(updated_by.clone(), "--admin-write requires --updated-by")?;
    } else if reason.is_some() || updated_by.is_some() {
        return Err("--reason/--updated-by require --admin-write".into());
    }
    let fence_fields = [
        expected_desired_set_hash.is_some(),
        expected_enabled_mints_hash.is_some(),
        expected_ordered_address_hash.is_some(),
        expected_reserve_set_hash.is_some(),
        expected_reserve_count.is_some(),
        expected_address_count.is_some(),
        expected_minimum_source_slot.is_some(),
    ];
    let has_any_fence = fence_fields.iter().any(|present| *present);
    let has_all_fences = fence_fields.iter().all(|present| *present);
    if has_any_fence && !has_all_fences {
        return Err(
            "shared-catalog approval fencing requires all seven --expected-* values".into(),
        );
    }
    if admin_write && !has_all_fences {
        return Err(
            "--admin-write requires the seven exact --expected-* values emitted by a fresh dry run"
                .into(),
        );
    }
    for (name, hash) in [
        (
            "--expected-desired-set-hash",
            expected_desired_set_hash.as_deref(),
        ),
        (
            "--expected-enabled-mints-hash",
            expected_enabled_mints_hash.as_deref(),
        ),
        (
            "--expected-ordered-address-hash",
            expected_ordered_address_hash.as_deref(),
        ),
        (
            "--expected-reserve-set-hash",
            expected_reserve_set_hash.as_deref(),
        ),
    ] {
        if hash.is_some_and(|hash| {
            hash.len() != 64 || !hash.bytes().all(|byte| byte.is_ascii_hexdigit())
        }) {
            return Err(format!("{name} must be a 64-character hexadecimal hash").into());
        }
    }
    if expected_reserve_count.is_some_and(|count| count == 0) {
        return Err("--expected-reserve-count must be greater than zero".into());
    }
    if expected_address_count
        .is_some_and(|count| count == 0 || count > SHARED_MARKET_LOGICAL_CATALOG_MAX_ADDRESSES)
    {
        return Err(format!(
            "--expected-address-count must be between 1 and {SHARED_MARKET_LOGICAL_CATALOG_MAX_ADDRESSES}"
        )
        .into());
    }
    if expected_minimum_source_slot.is_some_and(|slot| slot < 0) {
        return Err("--expected-minimum-source-slot must not be negative".into());
    }
    let approval_fence = has_all_fences.then(|| SharedCatalogApprovalFence {
        desired_set_hash: expected_desired_set_hash.expect("all fence fields checked"),
        enabled_mints_hash: expected_enabled_mints_hash.expect("all fence fields checked"),
        ordered_address_hash: expected_ordered_address_hash.expect("all fence fields checked"),
        reserve_set_hash: expected_reserve_set_hash.expect("all fence fields checked"),
        reserve_count: expected_reserve_count.expect("all fence fields checked"),
        address_count: expected_address_count.expect("all fence fields checked"),
        minimum_source_slot: expected_minimum_source_slot.expect("all fence fields checked"),
    });
    let enabled_mints = resolve_enabled_stable_mints(enabled_mints_raw.as_deref())?;
    Ok(Options {
        cluster,
        rpc_url,
        database_url,
        timescale_url,
        catalog_version,
        enabled_mints,
        address_chunk,
        admin_write,
        reason,
        updated_by,
        approval_fence,
    })
}

fn verify_approval_fence(
    fence: Option<&SharedCatalogApprovalFence>,
    desired_set_hash: &str,
    enabled_mints_hash: &str,
    ordered_address_hash: &str,
    reserve_set_hash: &str,
    reserve_count: usize,
    address_count: usize,
    source_slot: i64,
) -> Result<(), Box<dyn Error>> {
    let Some(fence) = fence else {
        return Ok(());
    };
    if fence.desired_set_hash != desired_set_hash
        || fence.enabled_mints_hash != enabled_mints_hash
        || fence.ordered_address_hash != ordered_address_hash
        || fence.reserve_set_hash != reserve_set_hash
        || fence.reserve_count != reserve_count
        || fence.address_count != address_count
        || source_slot < fence.minimum_source_slot
    {
        return Err(format!(
            "shared-catalog approval fence does not match the fresh finalized derivation (desired hash match: {}, enabled mints hash match: {}, ordered hash match: {}, reserve hash match: {}, reserve count match: {}, address count match: {}, minimum source slot freshness match: {})",
            fence.desired_set_hash == desired_set_hash,
            fence.enabled_mints_hash == enabled_mints_hash,
            fence.ordered_address_hash == ordered_address_hash,
            fence.reserve_set_hash == reserve_set_hash,
            fence.reserve_count == reserve_count,
            fence.address_count == address_count,
            source_slot >= fence.minimum_source_slot,
        )
        .into());
    }
    Ok(())
}

fn required_nonempty(
    value: Option<String>,
    message: &'static str,
) -> Result<String, Box<dyn Error>> {
    value
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| message.into())
}

fn next_value(
    args: &mut impl Iterator<Item = String>,
    option: &'static str,
) -> Result<String, Box<dyn Error>> {
    args.next()
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| format!("{option} requires a value").into())
}

fn usage() -> &'static str {
    "Usage: route-lookup-table-shared-catalog --catalog-version <VERSION> [--cluster <CLUSTER>] [--rpc-url <URL>] [--enabled-stable-mints <MINTS>] [--address-chunk <1..20>] [--admin-write --reason <TEXT> --updated-by <ID> --expected-desired-set-hash <HASH> --expected-enabled-mints-hash <HASH> --expected-ordered-address-hash <HASH> --expected-reserve-set-hash <HASH> --expected-reserve-count <N> --expected-address-count <N> --expected-minimum-source-slot <SLOT>]\n\nDry-run is the default. The command treats active safe rows for the explicit enabled stable mints as new-target eligibility, while physical shared inventory includes all known reserves for those mints regardless of active/risk state plus every nonzero-held or in-flight source referenced by Neon. It decodes that complete source-safe universe in one finalized RPC snapshot, validates owner/market/mint identity, and unions its accounts with every address in durable shared-catalog history. Previously approved source/exit accounts remain in the current head's physical order; historical-only recovery and newly required accounts append, while roles and writability only widen. Because there is no durable zero-live-reference proof yet, publication never removes a previously approved address. The logical catalog is append-packed deterministically across as many durable physical shared shards as its family high-water requires; route compilation selects only contributing shards. A dry run emits seven approval-fence values over this retention-safe catalog: all four hashes and both counts are exact, while minimum source slot is the accepted finalized freshness floor. --admin-write requires all seven and rejects content drift, held/in-flight source drift, or slot regression before atomically publishing the catalog head and queuing only its shared ALT plan; it never loads a signer, sends a transaction, or backfills vault ALTs."
}

#[cfg(test)]
mod tests {
    use super::*;
    use loyal_actions::{PYUSD_MINT, USDC_MINT};
    use std::collections::BTreeMap;

    fn environment() -> BTreeMap<String, String> {
        BTreeMap::from([
            (DATABASE_URL_ENV.to_owned(), "postgres://neon".to_owned()),
            (
                TIMESCALE_URL_ENV.to_owned(),
                "postgres://timescale".to_owned(),
            ),
            (RPC_URL_ENV.to_owned(), "https://rpc.example".to_owned()),
            (CLUSTER_ENV.to_owned(), "mainnet-beta".to_owned()),
            (CATALOG_VERSION_ENV.to_owned(), "stable-v1".to_owned()),
        ])
    }

    fn shared_address(
        address: &str,
        ordinal: i32,
        account_role: &str,
        is_writable: bool,
    ) -> LookupTableManifestAddressRecord {
        LookupTableManifestAddressRecord {
            address: address.to_owned(),
            ordinal,
            semantic_class: LookupTableManifestSubject::SharedMarket,
            account_role: account_role.to_owned(),
            is_writable,
        }
    }

    fn eligible_catalog(
        addresses: Vec<LookupTableManifestAddressRecord>,
    ) -> DerivedSharedMarketCatalog {
        DerivedSharedMarketCatalog {
            addresses,
            desired_set_hash: "eligible-desired-hash-is-recomputed".to_owned(),
            ordered_address_hash: "eligible-ordered-hash-is-recomputed".to_owned(),
            reserve_set_hash: "eligible-reserve-set-hash".to_owned(),
        }
    }

    #[test]
    fn shared_catalog_cli_is_dry_run_and_canonical_by_default() {
        let environment = environment();
        let options = parse_args(Vec::<String>::new(), |name| environment.get(name).cloned())
            .expect("default dry-run options");
        assert!(!options.admin_write);
        assert_eq!(options.enabled_mints.len(), 6);
        assert!(options.reason.is_none());
        assert!(options.updated_by.is_none());
        assert!(options.approval_fence.is_none());
    }

    #[test]
    fn shared_catalog_cli_requires_explicit_admin_audit_fields() {
        let environment = environment();
        assert!(parse_args(["--admin-write"], |name| environment.get(name).cloned()).is_err());
        let enabled_mints = format!("{USDC_MINT},{PYUSD_MINT}");
        let options = parse_args(
            vec![
                "--admin-write".to_owned(),
                "--reason".to_owned(),
                "initial durable shared catalog".to_owned(),
                "--updated-by".to_owned(),
                "operator@example".to_owned(),
                "--enabled-stable-mints".to_owned(),
                enabled_mints,
                "--expected-desired-set-hash".to_owned(),
                "a".repeat(64),
                "--expected-enabled-mints-hash".to_owned(),
                "d".repeat(64),
                "--expected-ordered-address-hash".to_owned(),
                "b".repeat(64),
                "--expected-reserve-set-hash".to_owned(),
                "c".repeat(64),
                "--expected-reserve-count".to_owned(),
                "4".to_owned(),
                "--expected-address-count".to_owned(),
                "237".to_owned(),
                "--expected-minimum-source-slot".to_owned(),
                "123".to_owned(),
            ],
            |name| environment.get(name).cloned(),
        )
        .expect("explicit admin options");
        assert!(options.admin_write);
        assert_eq!(
            options.enabled_mints,
            vec![PYUSD_MINT.to_string(), USDC_MINT.to_string()]
        );
        assert_eq!(
            options.approval_fence,
            Some(SharedCatalogApprovalFence {
                desired_set_hash: "a".repeat(64),
                enabled_mints_hash: "d".repeat(64),
                ordered_address_hash: "b".repeat(64),
                reserve_set_hash: "c".repeat(64),
                reserve_count: 4,
                address_count: 237,
                minimum_source_slot: 123,
            })
        );
    }

    #[test]
    fn shared_catalog_approval_fence_rejects_partial_or_stale_evidence() {
        let environment = environment();
        let partial = parse_args(["--expected-desired-set-hash", &"a".repeat(64)], |name| {
            environment.get(name).cloned()
        })
        .expect_err("partial fence must fail");
        assert!(partial.to_string().contains("requires all seven"));

        let fence = SharedCatalogApprovalFence {
            desired_set_hash: "a".repeat(64),
            enabled_mints_hash: "d".repeat(64),
            ordered_address_hash: "b".repeat(64),
            reserve_set_hash: "c".repeat(64),
            reserve_count: 4,
            address_count: 17,
            minimum_source_slot: 123,
        };
        verify_approval_fence(
            Some(&fence),
            &"a".repeat(64),
            &"d".repeat(64),
            &"b".repeat(64),
            &"c".repeat(64),
            4,
            17,
            123,
        )
        .expect("exact fence");
        verify_approval_fence(
            Some(&fence),
            &"a".repeat(64),
            &"d".repeat(64),
            &"b".repeat(64),
            &"c".repeat(64),
            4,
            17,
            124,
        )
        .expect("newer finalized source slot satisfies the freshness fence");
        let stale = verify_approval_fence(
            Some(&fence),
            &"a".repeat(64),
            &"d".repeat(64),
            &"b".repeat(64),
            &"c".repeat(64),
            4,
            18,
            123,
        )
        .expect_err("changed address count must fail");
        assert!(stale.to_string().contains("address count match: false"));
        let regressed_slot = verify_approval_fence(
            Some(&fence),
            &"a".repeat(64),
            &"d".repeat(64),
            &"b".repeat(64),
            &"c".repeat(64),
            4,
            17,
            122,
        )
        .expect_err("older finalized source slot must fail");
        assert!(regressed_slot
            .to_string()
            .contains("minimum source slot freshness match: false"));

        let changed_enabled_mints = verify_approval_fence(
            Some(&fence),
            &"a".repeat(64),
            &"e".repeat(64),
            &"b".repeat(64),
            &"c".repeat(64),
            4,
            17,
            123,
        )
        .expect_err("changed enabled mint set must fail");
        assert!(changed_enabled_mints
            .to_string()
            .contains("enabled mints hash match: false"));

        let changed_reserve_count = verify_approval_fence(
            Some(&fence),
            &"a".repeat(64),
            &"d".repeat(64),
            &"b".repeat(64),
            &"c".repeat(64),
            5,
            17,
            123,
        )
        .expect_err("changed reserve count must fail");
        assert!(changed_reserve_count
            .to_string()
            .contains("reserve count match: false"));
    }

    #[test]
    fn shared_catalog_retains_exit_accounts_and_appends_new_targets() {
        let previously_approved = vec![
            shared_address("z-source-account", 0, "reserve", true),
            shared_address("m-shared-account", 1, "market", false),
        ];
        let eligible = eligible_catalog(vec![
            // This address sorts before the historical prefix, but durable ALT
            // order must append it instead of reordering approved accounts.
            shared_address("a-new-target-account", 0, "reserve", false),
            shared_address("m-shared-account", 1, "oracle", true),
        ]);

        let retained = retain_previously_approved_shared_addresses(
            eligible,
            &previously_approved,
            &previously_approved,
            &[],
        )
        .expect("retention-safe catalog");
        assert_eq!(
            retained
                .catalog
                .addresses
                .iter()
                .map(|record| record.address.as_str())
                .collect::<Vec<_>>(),
            vec![
                "z-source-account",
                "m-shared-account",
                "a-new-target-account"
            ]
        );
        assert_eq!(retained.eligible_address_count, 2);
        assert_eq!(retained.previously_approved_address_count, 2);
        assert_eq!(retained.retained_only_address_count, 1);
        assert_eq!(retained.appended_address_count, 1);
        assert_eq!(retained.catalog.addresses[1].account_role, "market,oracle");
        assert!(retained.catalog.addresses[1].is_writable);
        assert_eq!(
            retained.catalog.desired_set_hash,
            lookup_table_manifest_address_records_hash(&retained.catalog.addresses)
        );
        assert_eq!(
            retained.catalog.ordered_address_hash,
            length_prefixed_hash(
                retained
                    .catalog
                    .addresses
                    .iter()
                    .map(|record| record.address.as_str())
            )
        );
    }

    #[test]
    fn shared_catalog_history_union_never_duplicates_or_narrows_metadata() {
        let previously_approved = vec![
            shared_address("source-a", 0, "reserve", false),
            shared_address("shared-b", 1, "market", false),
            // A later immutable revision repeats both addresses and widens A.
            shared_address("source-a", 0, "liquidity_supply", true),
            shared_address("shared-b", 1, "oracle", false),
        ];
        let eligible = eligible_catalog(vec![shared_address(
            "shared-b",
            0,
            "market_authority",
            false,
        )]);

        // The current physical head contains B. Historical A was previously
        // approved but (incorrectly) omitted by an older publisher, so it is
        // recovered after the live B prefix rather than forcing a reorder.
        let current_head = vec![shared_address("shared-b", 0, "market", false)];
        let retained = retain_previously_approved_shared_addresses(
            eligible,
            &current_head,
            &previously_approved,
            &[],
        )
        .expect("history union");
        assert_eq!(retained.catalog.addresses.len(), 2);
        assert_eq!(retained.catalog.addresses[0].address, "shared-b");
        assert_eq!(
            retained.catalog.addresses[0].account_role,
            "market,market_authority,oracle"
        );
        assert!(!retained.catalog.addresses[0].is_writable);
        assert_eq!(retained.catalog.addresses[1].address, "source-a");
        assert_eq!(
            retained.catalog.addresses[1].account_role,
            "liquidity_supply,reserve"
        );
        assert!(retained.catalog.addresses[1].is_writable);
        assert_eq!(retained.retained_only_address_count, 1);
        assert_eq!(retained.appended_address_count, 0);
    }

    #[test]
    fn first_catalog_includes_held_ineligible_source_without_targeting_it() {
        let eligible = eligible_catalog(vec![shared_address(
            "eligible-target-account",
            0,
            "reserve",
            true,
        )]);
        let held_ineligible_source = vec![shared_address(
            "held-inactive-source-account",
            0,
            "liquidity_supply",
            true,
        )];

        let retained = retain_previously_approved_shared_addresses(
            eligible,
            &[],
            &[],
            &held_ineligible_source,
        )
        .expect("first catalog retains held source");
        assert_eq!(
            retained
                .catalog
                .addresses
                .iter()
                .map(|record| record.address.as_str())
                .collect::<Vec<_>>(),
            vec!["held-inactive-source-account", "eligible-target-account"]
        );
        assert_eq!(retained.eligible_address_count, 1);
        assert_eq!(retained.required_source_address_count, 1);
        assert_eq!(retained.source_only_address_count, 1);
        assert_eq!(retained.previously_approved_address_count, 0);
        assert_eq!(retained.appended_address_count, 2);
        assert_eq!(
            retained.catalog.reserve_set_hash,
            "eligible-reserve-set-hash"
        );
    }
}
