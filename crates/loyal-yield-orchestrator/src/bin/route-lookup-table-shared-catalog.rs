use chrono::Utc;
use loyal_yield_orchestrator::{
    derive_shared_market_catalog, enabled_stable_mints_hash, load_finalized_kamino_reserve_catalog,
    resolve_enabled_stable_mints,
    rpc_safety::{
        redacted_external_error, redacted_rpc_endpoint, validate_rpc_endpoint,
        validate_rpc_genesis_hash,
    },
    LookupTableFamilyKind, NeonSqlClient, NeonSqlConfig, SharedMarketCatalogHeadRecord,
    SharedMarketCatalogPlanPolicy, SharedMarketCatalogUpsert, SupportedKaminoReserve,
    ENABLED_STABLE_MINTS_ENV,
};
use loyal_yield_router::timescale::{TimescaleRouterClient, TimescaleRouterClientConfig};
use serde_json::{json, Value};
use solana_client::rpc_client::RpcClient;
use solana_sdk::commitment_config::CommitmentConfig;
use std::{env, error::Error, time::Duration};

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
    let supported_reserve_max_updated_at =
        supported_reserves.iter().map(|row| row.updated_at).max();

    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.clone(), CommitmentConfig::finalized());
    let genesis_hash = rpc
        .get_genesis_hash()
        .map_err(|_| "failed to read genesis hash from configured shared-market catalog RPC")?;
    validate_rpc_genesis_hash(&options.cluster, genesis_hash).map_err(|error| {
        format!("refusing shared-market catalog derivation against mismatched RPC: {error}")
    })?;
    let finalized = load_finalized_kamino_reserve_catalog(&rpc, &supported_reserves)?;
    if finalized.source_slot != finalized.max_source_slot {
        return Err("shared-market catalog did not come from one finalized RPC snapshot".into());
    }
    let catalog = derive_shared_market_catalog(&finalized.reserves)?;
    let enabled_mints_hash = enabled_stable_mints_hash(&options.enabled_mints)?;
    let source_slot = i64::try_from(finalized.source_slot)
        .map_err(|_| "finalized shared-market catalog slot exceeds PostgreSQL BIGINT")?;
    let source_observed_at = Some(Utc::now());

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
                "reserveSetHash": catalog.reserve_set_hash,
                "desiredSetHash": catalog.desired_set_hash,
                "orderedAddressHash": catalog.ordered_address_hash,
                "reserveCount": finalized.reserves.len(),
                "addressCount": catalog.addresses.len(),
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
            finalized.reserves.len(),
            catalog.addresses.len(),
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
    let shared_high_water = usize::try_from(shared_family.allocation_high_water)
        .map_err(|_| "shared-market family allocation high-water is invalid")?;
    if catalog.addresses.len() > shared_high_water {
        return Err(format!(
            "derived shared-market catalog has {} addresses, exceeding durable shared ALT high-water {}",
            catalog.addresses.len(), shared_high_water
        )
        .into());
    }

    let current_head = if catalog_schema_applied {
        client.shared_market_catalog_head(&options.cluster).await?
    } else {
        None
    };
    let source_metadata = json!({
        "source": "kamino.supported_reserves+finalized_rpc",
        "riskBasket": "safe",
        "enabledMints": options.enabled_mints,
        "enabledMintsHash": enabled_mints_hash,
        "reserveSetHash": catalog.reserve_set_hash,
        "orderedAddressHash": catalog.ordered_address_hash,
        "reserveCount": supported_reserves.len(),
        "addressCount": catalog.addresses.len(),
        "supportedReserveMaxUpdatedAt": supported_reserve_max_updated_at,
        "rpcCommitment": "finalized",
        "rpcContextSlot": source_slot,
        "rpcGenesisHash": genesis_hash.to_string(),
        "rpcEndpoint": redacted_rpc_endpoint(&options.rpc_url),
        "supportedReserveQuery": {
            "active": true,
            "riskBasket": "safe",
            "liquidityMintFilter": "enabled_stable_subset_only",
            "apyFilter": false,
            "freshnessFilter": false,
            "liquidityFilter": false,
        },
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
        "reserveCount": finalized.reserves.len(),
        "addressCount": catalog.addresses.len(),
        "sharedFamilyId": shared_family.id,
        "sharedFamilyAllocationHighWater": shared_family.allocation_high_water,
        "sourceSlot": source_slot,
        "sourceObservedAt": source_observed_at,
        "supportedReserveMaxUpdatedAt": supported_reserve_max_updated_at,
        "catalogSchemaApplied": catalog_schema_applied,
        "currentHeadUnavailableReason": (!catalog_schema_applied)
            .then_some("migration_19_not_applied"),
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
    })
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
    "Usage: route-lookup-table-shared-catalog --catalog-version <VERSION> [--cluster <CLUSTER>] [--rpc-url <URL>] [--enabled-stable-mints <MINTS>] [--address-chunk <1..20>] [--admin-write --reason <TEXT> --updated-by <ID>]\n\nDry-run is the default. The command reads active safe rows from kamino.supported_reserves using only the canonical enabled-stable-mint subset, loads every reserve in one finalized RPC snapshot, validates owner/market/mint identity, and derives the deterministic durable shared-market ALT catalog. --admin-write atomically publishes the catalog head and queues only its shared ALT plan; it never loads a signer, sends a transaction, or backfills vault ALTs."
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

    #[test]
    fn shared_catalog_cli_is_dry_run_and_canonical_by_default() {
        let environment = environment();
        let options = parse_args(Vec::<String>::new(), |name| environment.get(name).cloned())
            .expect("default dry-run options");
        assert!(!options.admin_write);
        assert_eq!(options.enabled_mints.len(), 6);
        assert!(options.reason.is_none());
        assert!(options.updated_by.is_none());
    }

    #[test]
    fn shared_catalog_cli_requires_explicit_admin_audit_fields() {
        let environment = environment();
        assert!(parse_args(["--admin-write"], |name| environment.get(name).cloned()).is_err());
        let enabled_mints = format!("{USDC_MINT},{PYUSD_MINT}");
        let options = parse_args(
            [
                "--admin-write",
                "--reason",
                "initial durable shared catalog",
                "--updated-by",
                "operator@example",
                "--enabled-stable-mints",
                enabled_mints.as_str(),
            ],
            |name| environment.get(name).cloned(),
        )
        .expect("explicit admin options");
        assert!(options.admin_write);
        assert_eq!(
            options.enabled_mints,
            vec![PYUSD_MINT.to_string(), USDC_MINT.to_string()]
        );
    }
}
