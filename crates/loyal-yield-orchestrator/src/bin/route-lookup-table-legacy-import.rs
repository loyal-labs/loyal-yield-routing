//! Fleet-wide RPC reverification and audited import for pre-reusable ALTs.
//!
//! Dry-run is the default. No database mutation is attempted until every
//! eligible legacy registry row has passed the same finalized RPC snapshot
//! checks. The explicit write path then re-locks and compares the full fleet in
//! one database transaction before recording immutable evidence.

use std::{collections::BTreeSet, env, error::Error, str::FromStr};

use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use chrono::Utc;
use loyal_yield_orchestrator::{
    historical_legacy_lookup_table_address_hash, legacy_lookup_table_import_fingerprint,
    rpc_safety::{redacted_external_error, validate_rpc_endpoint, validate_rpc_genesis_hash},
    LegacyLookupTableFleetImportRequest, LegacyLookupTableImportSource, LegacyLookupTableKind,
    NeonSqlClient, NeonSqlConfig, VerifiedLegacyLookupTableImport,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_client::rpc_client::RpcClient;
use solana_sdk::{
    account::Account,
    address_lookup_table::{
        instruction as address_lookup_table_instruction, program as address_lookup_table_program,
        state::AddressLookupTable,
    },
    commitment_config::CommitmentConfig,
    pubkey::Pubkey,
};

const DATABASE_URL_ENV: &str = "NEON_DATABASE_URL";
const RPC_URL_ENV: &str = "SOLANA_RPC_URL";
const CLUSTER_ENV: &str = "YIELD_ALT_CLUSTER";
const CONFIGURED_TABLES_ENV: &str = "YIELD_ROUTE_LOOKUP_TABLES";
const POLICY_AUTHORITY: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const DEFAULT_HISTORY_LIMIT: usize = 1_000;

#[derive(Debug, Clone)]
struct Options {
    cluster: String,
    rpc_url: String,
    database_url: String,
    legacy_kind: LegacyLookupTableKind,
    expected_table_count: usize,
    expected_fleet_hash: Option<String>,
    history_limit: usize,
    configured_tables: BTreeSet<String>,
    admin_write: bool,
    reason: Option<String>,
    updated_by: Option<String>,
}

#[derive(Debug, Deserialize)]
struct RpcResponse {
    result: Option<Value>,
    error: Option<RpcError>,
}

#[derive(Debug, Deserialize)]
struct RpcError {
    code: i64,
    message: String,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountsV2Response {
    result: Option<ProgramAccountsV2Result>,
    error: Option<RpcError>,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountsV2Result {
    accounts: Vec<ProgramAccountsV2Account>,
    #[serde(rename = "paginationKey")]
    pagination_key: Option<String>,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountsV2Account {
    pubkey: String,
    account: ProgramAccountsV2AccountData,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountsV2AccountData {
    data: Value,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct VerificationFailure {
    table_id: i64,
    table_address: String,
    scope: String,
    reason: String,
}

#[derive(Debug, Clone)]
struct InventoryTable {
    table_address: Pubkey,
    authority: Pubkey,
    address_count: usize,
    address_hash: String,
}

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!(
            "{}",
            json!({
                "event": "legacy_alt_import_fatal",
                "error": redacted_external_error(&error.to_string()),
            })
        );
        std::process::exit(1);
    }
}

async fn run() -> Result<(), Box<dyn Error>> {
    let options = parse_args(env::args().skip(1), |name| env::var(name).ok())?;
    validate_rpc_endpoint(&options.rpc_url)?;
    let client = NeonSqlClient::connect(NeonSqlConfig::new(options.database_url.clone())).await?;
    client
        .require_schema_migration(19, "legacy_lookup_table_imports")
        .await?;
    let sources = client
        .legacy_lookup_tables_for_import(&options.cluster)
        .await?;
    if sources.is_empty() {
        return Err(
            "no eligible durable legacy lookup tables were found; refusing an empty fleet import"
                .into(),
        );
    }
    if sources.len() != options.expected_table_count {
        return Err(format!(
            "eligible legacy fleet has {} rows, but --expected-table-count is {}",
            sources.len(),
            options.expected_table_count
        )
        .into());
    }
    if sources.len() > 100 {
        return Err(
            "legacy fleet exceeds one getMultipleAccounts snapshot; split-brain RPC verification is forbidden"
                .into(),
        );
    }
    let registry_fleet_hash = registry_fleet_hash(&sources);
    let registered_addresses = sources
        .iter()
        .map(|source| source.table_address.clone())
        .collect::<BTreeSet<_>>();
    let unknown_configured = options
        .configured_tables
        .difference(&registered_addresses)
        .cloned()
        .collect::<Vec<_>>();
    if !unknown_configured.is_empty() {
        return Err(format!(
            "configured legacy lookup tables are absent from the registry: {}",
            unknown_configured.join(",")
        )
        .into());
    }

    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.clone(), CommitmentConfig::finalized());
    let observed_genesis_hash = rpc
        .get_genesis_hash()
        .map_err(|_| "failed to read genesis hash from configured legacy ALT import RPC")?;
    validate_rpc_genesis_hash(&options.cluster, observed_genesis_hash).map_err(|error| {
        format!("refusing legacy ALT import against mismatched RPC genesis: {error}")
    })?;
    let policy_authority = Pubkey::from_str(POLICY_AUTHORITY)?;
    let program_inventory =
        discover_policy_tables_by_program_scan(&rpc, &options.rpc_url, policy_authority).await?;
    let history_inventory = discover_policy_tables_by_history(
        &options.rpc_url,
        policy_authority,
        options.history_limit,
    )
    .await?;
    let mut discovered_inventory = program_inventory.clone();
    discovered_inventory.extend(history_inventory.iter().copied());
    let mut v2_tables = BTreeSet::new();
    for table in &discovered_inventory {
        if client
            .lookup_table_cleanup_protection(&options.cluster, &table.to_string())
            .await?
            .is_some()
        {
            v2_tables.insert(*table);
        }
    }
    let legacy_inventory = load_existing_policy_inventory(
        &rpc,
        discovered_inventory
            .difference(&v2_tables)
            .copied()
            .collect(),
        policy_authority,
    )?;
    let inventory_addresses = legacy_inventory
        .iter()
        .map(|candidate| candidate.table_address.to_string())
        .collect::<BTreeSet<_>>();
    if inventory_addresses != registered_addresses {
        let missing_from_registry = inventory_addresses
            .difference(&registered_addresses)
            .cloned()
            .collect::<Vec<_>>();
        let missing_from_inventory = registered_addresses
            .difference(&inventory_addresses)
            .cloned()
            .collect::<Vec<_>>();
        return Err(format!(
            "policy-authority ALT inventory differs from the eligible legacy registry; unregistered={missing_from_registry:?}; undiscovered={missing_from_inventory:?}"
        )
        .into());
    }
    if legacy_inventory.len() != options.expected_table_count {
        return Err(format!(
            "policy-authority legacy inventory has {} tables, but --expected-table-count is {}",
            legacy_inventory.len(),
            options.expected_table_count
        )
        .into());
    }
    let inventory_fleet_hash = approved_inventory_fleet_hash(&legacy_inventory);
    if options
        .expected_fleet_hash
        .as_deref()
        .is_some_and(|expected| expected != inventory_fleet_hash)
    {
        return Err("policy-authority legacy inventory differs from --expected-fleet-hash".into());
    }
    let (observed_slot_u64, verified, failures) =
        verify_fleet(&rpc, &sources, options.legacy_kind)?;
    let observed_slot = i64::try_from(observed_slot_u64)
        .map_err(|_| "finalized RPC slot does not fit PostgreSQL BIGINT")?;
    if !failures.is_empty() || verified.len() != sources.len() {
        println!(
            "{}",
            json!({
                "event": "legacy_alt_fleet_verification_failed",
                "cluster": options.cluster,
                "legacyKind": options.legacy_kind.as_str(),
                "expectedTableCount": sources.len(),
                "verifiedTableCount": verified.len(),
                "failureCount": failures.len(),
                "failures": failures,
                "databaseWrites": false,
            })
        );
        return Err(
            "legacy lookup-table fleet verification failed; zero import writes were attempted"
                .into(),
        );
    }

    let import_fingerprint = legacy_lookup_table_import_fingerprint(
        &options.cluster,
        &observed_genesis_hash.to_string(),
        observed_slot,
        &verified,
    );
    let historical_hash_normalization_count = verified
        .iter()
        .filter(|table| table.source.address_hash != table.observed_address_hash)
        .count();
    let table_report = verified
        .iter()
        .map(|table| {
            json!({
                "tableId": table.source.id,
                "tableAddress": table.source.table_address,
                "scope": table.source.scope,
                "addressCount": table.source.address_count,
                "persistedAddressHash": table.source.address_hash,
                "canonicalAddressHash": table.observed_address_hash,
                "hashNormalizationRequired": table.source.address_hash != table.observed_address_hash,
                "authority": table.source.authority,
                "lastExtendedSlot": table.observed_last_extended_slot,
            })
        })
        .collect::<Vec<_>>();
    if !options.admin_write {
        println!(
            "{}",
            json!({
                "event": "legacy_alt_fleet_verified",
                "mode": "dry_run",
                "cluster": options.cluster,
                "legacyKind": options.legacy_kind.as_str(),
                "verifiedSlot": observed_slot,
                "verifiedTableCount": verified.len(),
                "registryFleetHash": registry_fleet_hash,
                "inventoryFleetHash": inventory_fleet_hash,
                "programInventoryCount": program_inventory.len(),
                "historyInventoryCount": history_inventory.len(),
                "excludedV2TableCount": v2_tables.len(),
                "historicalHashNormalizationCount": historical_hash_normalization_count,
                "importFingerprint": import_fingerprint,
                "tables": table_report,
                "databaseWrites": false,
                "transactionsSent": false,
            })
        );
        return Ok(());
    }

    let verified_at = Utc::now();
    let result = client
        .import_verified_legacy_lookup_table_fleet(LegacyLookupTableFleetImportRequest {
            cluster: options.cluster.clone(),
            rpc_genesis_hash: observed_genesis_hash.to_string(),
            verified_slot: observed_slot,
            verified_at,
            import_fingerprint: import_fingerprint.clone(),
            reason: options.reason.expect("validated by parser"),
            updated_by: options.updated_by.expect("validated by parser"),
            expected_table_count: i32::try_from(options.expected_table_count)
                .map_err(|_| "expected table count does not fit PostgreSQL INTEGER")?,
            tables: verified,
        })
        .await?;
    println!(
        "{}",
        json!({
            "event": if result.replayed {
                "legacy_alt_fleet_import_replayed"
            } else {
                "legacy_alt_fleet_imported"
            },
            "mode": "admin_write",
            "cluster": result.cluster,
            "legacyKind": result.legacy_kind.as_str(),
            "importRunId": result.import_run_id,
            "verifiedSlot": result.verified_slot,
            "verifiedAt": result.verified_at,
            "importedTableCount": result.imported_table_count,
            "importFingerprint": result.import_fingerprint,
            "registryFleetHash": registry_fleet_hash,
            "inventoryFleetHash": inventory_fleet_hash,
            "programInventoryCount": program_inventory.len(),
            "historyInventoryCount": history_inventory.len(),
            "excludedV2TableCount": v2_tables.len(),
            "historicalHashNormalizationCount": historical_hash_normalization_count,
            "tables": table_report,
            "replayed": result.replayed,
            "databaseWrites": !result.replayed,
            "transactionsSent": false,
            "signerLoaded": false,
        })
    );
    Ok(())
}

async fn discover_policy_tables_by_program_scan(
    rpc: &RpcClient,
    rpc_url: &str,
    policy_authority: Pubkey,
) -> Result<BTreeSet<Pubkey>, Box<dyn Error>> {
    match rpc.get_program_accounts(&address_lookup_table_program::id()) {
        Ok(accounts) => Ok(accounts
            .into_iter()
            .filter_map(|(address, account)| {
                AddressLookupTable::deserialize(&account.data)
                    .ok()
                    .filter(|table| table.meta.authority == Some(policy_authority))
                    .map(|_| address)
            })
            .collect()),
        Err(_) => discover_policy_tables_by_program_scan_v2(rpc_url, policy_authority).await,
    }
}

async fn discover_policy_tables_by_program_scan_v2(
    rpc_url: &str,
    policy_authority: Pubkey,
) -> Result<BTreeSet<Pubkey>, Box<dyn Error>> {
    let http = reqwest::Client::new();
    let mut pagination_key: Option<String> = None;
    let mut tables = BTreeSet::new();
    loop {
        let mut config = json!({ "encoding": "base64", "limit": 1_000 });
        if let Some(key) = pagination_key.as_ref() {
            config["paginationKey"] = json!(key);
        }
        let response = http
            .post(rpc_url)
            .json(&json!({
                "jsonrpc": "2.0",
                "id": "route-lookup-table-legacy-import",
                "method": "getProgramAccountsV2",
                "params": [address_lookup_table_program::id().to_string(), config],
            }))
            .send()
            .await?
            .error_for_status()?
            .json::<ProgramAccountsV2Response>()
            .await?;
        if let Some(error) = response.error {
            return Err(format!(
                "getProgramAccountsV2 returned {}: {}",
                error.code, error.message
            )
            .into());
        }
        let result = response
            .result
            .ok_or("getProgramAccountsV2 response did not include result")?;
        let page_count = result.accounts.len();
        for account in result.accounts {
            let Some(data) = decode_program_account_data(&account.account.data)? else {
                continue;
            };
            let Ok(table) = AddressLookupTable::deserialize(&data) else {
                continue;
            };
            if table.meta.authority == Some(policy_authority) {
                tables.insert(Pubkey::from_str(&account.pubkey)?);
            }
        }
        pagination_key = result.pagination_key;
        if pagination_key.is_none() || page_count == 0 {
            break;
        }
    }
    Ok(tables)
}

fn decode_program_account_data(data: &Value) -> Result<Option<Vec<u8>>, Box<dyn Error>> {
    let Some(items) = data.as_array() else {
        return Ok(None);
    };
    let Some(encoded) = items.first().and_then(Value::as_str) else {
        return Ok(None);
    };
    let encoding = items.get(1).and_then(Value::as_str).unwrap_or("base64");
    if encoding != "base64" {
        return Err(format!("unsupported getProgramAccountsV2 encoding {encoding}").into());
    }
    Ok(Some(BASE64.decode(encoded)?))
}

async fn discover_policy_tables_by_history(
    rpc_url: &str,
    policy_authority: Pubkey,
    history_limit: usize,
) -> Result<BTreeSet<Pubkey>, Box<dyn Error>> {
    let http = reqwest::Client::new();
    let mut tables = BTreeSet::new();
    let mut before: Option<String> = None;
    let mut scanned = 0_usize;
    while scanned < history_limit {
        let page_limit = (history_limit - scanned).min(1_000);
        let mut config = json!({ "limit": page_limit });
        if let Some(signature) = before.as_ref() {
            config["before"] = json!(signature);
        }
        let signatures = rpc_call(
            &http,
            rpc_url,
            "getSignaturesForAddress",
            json!([policy_authority.to_string(), config]),
        )
        .await?;
        let signatures = signatures
            .as_array()
            .ok_or("getSignaturesForAddress result was not an array")?;
        if signatures.is_empty() {
            break;
        }
        for entry in signatures {
            let Some(signature) = entry.get("signature").and_then(Value::as_str) else {
                continue;
            };
            let transaction = rpc_call(
                &http,
                rpc_url,
                "getTransaction",
                json!([signature, { "encoding": "json", "maxSupportedTransactionVersion": 0 }]),
            )
            .await?;
            collect_policy_tables_from_transaction(&transaction, policy_authority, &mut tables)?;
        }
        scanned += signatures.len();
        before = signatures
            .last()
            .and_then(|entry| entry.get("signature"))
            .and_then(Value::as_str)
            .map(str::to_owned);
        if signatures.len() < page_limit || before.is_none() {
            break;
        }
    }
    Ok(tables)
}

async fn rpc_call(
    http: &reqwest::Client,
    rpc_url: &str,
    method: &str,
    params: Value,
) -> Result<Value, Box<dyn Error>> {
    let response = http
        .post(rpc_url)
        .json(&json!({
            "jsonrpc": "2.0",
            "id": "route-lookup-table-legacy-import",
            "method": method,
            "params": params,
        }))
        .send()
        .await?
        .error_for_status()?
        .json::<RpcResponse>()
        .await?;
    if let Some(error) = response.error {
        return Err(format!("RPC {method} returned {}: {}", error.code, error.message).into());
    }
    response
        .result
        .ok_or_else(|| format!("RPC {method} response did not include result").into())
}

fn collect_policy_tables_from_transaction(
    transaction: &Value,
    policy_authority: Pubkey,
    tables: &mut BTreeSet<Pubkey>,
) -> Result<(), Box<dyn Error>> {
    let account_keys = transaction
        .pointer("/transaction/message/accountKeys")
        .and_then(Value::as_array)
        .map(|values| {
            values
                .iter()
                .filter_map(|value| value.as_str().or_else(|| value.get("pubkey")?.as_str()))
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    if let Some(instructions) = transaction
        .pointer("/transaction/message/instructions")
        .and_then(Value::as_array)
    {
        for instruction in instructions {
            collect_policy_table_from_instruction(
                instruction,
                &account_keys,
                policy_authority,
                tables,
            )?;
        }
    }
    if let Some(groups) = transaction
        .pointer("/meta/innerInstructions")
        .and_then(Value::as_array)
    {
        for instruction in groups
            .iter()
            .filter_map(|group| group.get("instructions").and_then(Value::as_array))
            .flatten()
        {
            collect_policy_table_from_instruction(
                instruction,
                &account_keys,
                policy_authority,
                tables,
            )?;
        }
    }
    Ok(())
}

fn collect_policy_table_from_instruction(
    instruction: &Value,
    account_keys: &[&str],
    policy_authority: Pubkey,
    tables: &mut BTreeSet<Pubkey>,
) -> Result<(), Box<dyn Error>> {
    let Some(program_id) = instruction
        .get("programIdIndex")
        .and_then(Value::as_u64)
        .and_then(|index| account_keys.get(index as usize))
    else {
        return Ok(());
    };
    if *program_id != address_lookup_table_program::id().to_string() {
        return Ok(());
    }
    let instruction_accounts = instruction
        .get("accounts")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(Value::as_u64)
        .filter_map(|index| account_keys.get(index as usize))
        .map(|value| Pubkey::from_str(value))
        .collect::<Result<Vec<_>, _>>()?;
    if instruction_accounts.get(1) != Some(&policy_authority) {
        return Ok(());
    }
    let Some(data) = instruction.get("data").and_then(Value::as_str) else {
        return Ok(());
    };
    let decoded = bs58::decode(data).into_vec()?;
    bincode::deserialize::<address_lookup_table_instruction::ProgramInstruction>(&decoded)?;
    if let Some(table) = instruction_accounts.first() {
        tables.insert(*table);
    }
    Ok(())
}

fn load_existing_policy_inventory(
    rpc: &RpcClient,
    addresses: Vec<Pubkey>,
    policy_authority: Pubkey,
) -> Result<Vec<InventoryTable>, Box<dyn Error>> {
    let mut inventory = Vec::new();
    for chunk in addresses.chunks(100) {
        let response =
            rpc.get_multiple_accounts_with_commitment(chunk, CommitmentConfig::finalized())?;
        if response.value.len() != chunk.len() {
            return Err("finalized inventory RPC returned an incomplete account vector".into());
        }
        for (table_address, account) in chunk.iter().zip(response.value) {
            let Some(account) = account else {
                // History intentionally includes already-closed tables. They
                // are audit evidence, not members of the refundable fleet.
                continue;
            };
            if account.owner != address_lookup_table_program::id() {
                return Err(format!(
                    "inventory address {table_address} is not owned by the ALT program"
                )
                .into());
            }
            let table = AddressLookupTable::deserialize(&account.data)?;
            if table.meta.authority != Some(policy_authority) {
                return Err(format!(
                    "inventory address {table_address} no longer has policy authority"
                )
                .into());
            }
            let addresses = table
                .addresses
                .iter()
                .map(ToString::to_string)
                .collect::<Vec<_>>();
            inventory.push(InventoryTable {
                table_address: *table_address,
                authority: policy_authority,
                address_count: addresses.len(),
                address_hash: ordered_address_hash(&addresses),
            });
        }
    }
    inventory.sort_by_key(|table| table.table_address);
    Ok(inventory)
}

fn approved_inventory_fleet_hash(inventory: &[InventoryTable]) -> String {
    let mut parts = vec!["legacy-alt-policy-fleet-v1".to_owned()];
    for table in inventory {
        parts.extend([
            table.table_address.to_string(),
            table.authority.to_string(),
            table.address_count.to_string(),
            table.address_hash.clone(),
        ]);
    }
    ordered_address_hash(&parts)
}

#[allow(clippy::type_complexity)]
fn verify_fleet(
    rpc: &RpcClient,
    sources: &[LegacyLookupTableImportSource],
    legacy_kind: LegacyLookupTableKind,
) -> Result<
    (
        u64,
        Vec<VerifiedLegacyLookupTableImport>,
        Vec<VerificationFailure>,
    ),
    Box<dyn Error>,
> {
    if sources.len() > 100 {
        return Err("legacy fleet exceeds a single finalized RPC snapshot".into());
    }
    let mut keyed_sources = Vec::with_capacity(sources.len());
    let mut failures = Vec::new();
    for source in sources {
        match Pubkey::from_str(&source.table_address) {
            Ok(key) => keyed_sources.push((source, key)),
            Err(_) => failures.push(failure(source, "table address is not a public key")),
        }
    }
    if keyed_sources.is_empty() {
        return Ok((0, Vec::new(), failures));
    }
    let keys = keyed_sources
        .iter()
        .map(|(_, key)| *key)
        .collect::<Vec<_>>();
    let response = rpc
        .get_multiple_accounts_with_commitment(&keys, CommitmentConfig::finalized())
        .map_err(|error| {
            redacted_external_error(&format!(
                "finalized RPC lookup-table fleet load failed: {error}"
            ))
        })?;
    let observed_slot = response.context.slot;
    if response.value.len() != keyed_sources.len() {
        let reason = format!(
            "finalized RPC returned {} accounts for {} requested tables",
            response.value.len(),
            keyed_sources.len()
        );
        failures.extend(
            keyed_sources
                .iter()
                .map(|(source, _)| failure(source, &reason)),
        );
        return Ok((observed_slot, Vec::new(), failures));
    }
    let mut verified = Vec::with_capacity(keyed_sources.len());
    for ((source, _), account) in keyed_sources.iter().zip(response.value.iter()) {
        match verify_source(source, account.as_ref(), observed_slot, legacy_kind) {
            Ok(table) => verified.push(table),
            Err(reason) => failures.push(failure(source, &reason)),
        }
    }
    Ok((observed_slot, verified, failures))
}

fn verify_source(
    source: &LegacyLookupTableImportSource,
    account: Option<&Account>,
    observed_slot: u64,
    legacy_kind: LegacyLookupTableKind,
) -> Result<VerifiedLegacyLookupTableImport, String> {
    if source.cluster.trim().is_empty()
        || !source.durable
        || !matches!(source.status.as_str(), "active" | "warming" | "usable")
    {
        return Err("registry row is not an eligible durable legacy table".to_owned());
    }
    if source
        .legacy_kind
        .is_some_and(|persisted| persisted != legacy_kind)
    {
        return Err(
            "persisted legacy classification differs from requested classification".to_owned(),
        );
    }
    let expected_count = usize::try_from(source.address_count)
        .map_err(|_| "persisted address count is negative".to_owned())?;
    if expected_count != source.addresses.len() || expected_count > 256 {
        return Err("persisted address count differs from ordered address list".to_owned());
    }
    let canonical_address_hash = ordered_address_hash(&source.addresses);
    let historical_address_hash = historical_legacy_lookup_table_address_hash(&source.addresses);
    let is_unimported_historical_row = source.legacy_kind.is_none()
        && source.legacy_import_run_id.is_none()
        && source.address_hash == historical_address_hash;
    if source.address_hash != canonical_address_hash && !is_unimported_historical_row {
        return Err("persisted address hash differs from ordered address list".to_owned());
    }
    let expected_authority = Pubkey::from_str(&source.authority)
        .map_err(|_| "persisted authority is not a public key".to_owned())?;
    let account = account.ok_or_else(|| "lookup-table account is missing on RPC".to_owned())?;
    if account.executable {
        return Err("lookup-table account is unexpectedly executable".to_owned());
    }
    if account.owner != address_lookup_table_program::id() {
        return Err(format!(
            "account owner {} does not match the address lookup-table program",
            account.owner
        ));
    }
    let table = AddressLookupTable::deserialize(&account.data)
        .map_err(|error| format!("lookup-table account decode failed: {error}"))?;
    if table.meta.authority != Some(expected_authority) {
        return Err(format!(
            "lookup-table authority {:?} does not match expected {}",
            table.meta.authority, expected_authority
        ));
    }
    if table.meta.deactivation_slot != u64::MAX {
        return Err(format!(
            "lookup table is deactivating/deactivated at slot {}",
            table.meta.deactivation_slot
        ));
    }
    if observed_slot <= table.meta.last_extended_slot {
        return Err(format!(
            "lookup table is not fully warm at finalized slot {observed_slot}; last extended at {}",
            table.meta.last_extended_slot
        ));
    }
    let chain_addresses = table
        .addresses
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    if usize::from(table.meta.last_extended_slot_start_index) > chain_addresses.len() {
        return Err("lookup-table last extended start index exceeds its address count".to_owned());
    }
    if chain_addresses != source.addresses {
        return Err(
            "RPC full ordered membership differs from the persisted address list".to_owned(),
        );
    }
    let observed_address_hash = ordered_address_hash(&chain_addresses);
    if observed_address_hash != canonical_address_hash {
        return Err("RPC ordered membership hash differs from persisted address list".to_owned());
    }
    Ok(VerifiedLegacyLookupTableImport {
        source: source.clone(),
        legacy_kind,
        observed_owner: account.owner.to_string(),
        observed_authority: table
            .meta
            .authority
            .expect("authority equality checked")
            .to_string(),
        observed_deactivation_slot: table.meta.deactivation_slot.to_string(),
        observed_last_extended_slot: i64::try_from(table.meta.last_extended_slot)
            .map_err(|_| "lookup-table last extended slot does not fit PostgreSQL BIGINT")?,
        observed_last_extended_start_index: i32::from(table.meta.last_extended_slot_start_index),
        observed_address_count: i32::try_from(chain_addresses.len())
            .map_err(|_| "lookup-table address count does not fit PostgreSQL INTEGER")?,
        observed_address_hash,
        observed_addresses: chain_addresses,
    })
}

fn failure(source: &LegacyLookupTableImportSource, reason: &str) -> VerificationFailure {
    VerificationFailure {
        table_id: source.id,
        table_address: source.table_address.clone(),
        scope: source.scope.clone(),
        reason: redacted_external_error(reason),
    }
}

fn ordered_address_hash(addresses: &[String]) -> String {
    let mut hasher = Sha256::new();
    for address in addresses {
        hasher.update((address.len() as u64).to_le_bytes());
        hasher.update(address.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn registry_fleet_hash(sources: &[LegacyLookupTableImportSource]) -> String {
    let mut parts = Vec::new();
    for source in sources {
        parts.extend([
            source.id.to_string(),
            source.cluster.clone(),
            source.scope.clone(),
            source.table_address.clone(),
            source.authority.clone(),
            source.status.clone(),
            source.durable.to_string(),
            source.address_count.to_string(),
            source.address_hash.clone(),
            source
                .legacy_kind
                .map(LegacyLookupTableKind::as_str)
                .unwrap_or("unclassified")
                .to_owned(),
            source
                .legacy_import_run_id
                .map_or_else(|| "none".to_owned(), |value| value.to_string()),
            source
                .last_extended_slot
                .map_or_else(|| "none".to_owned(), |value| value.to_string()),
            source
                .last_extended_start_index
                .map_or_else(|| "none".to_owned(), |value| value.to_string()),
            source
                .last_verified_slot
                .map_or_else(|| "none".to_owned(), |value| value.to_string()),
            source
                .last_verified_at
                .map_or_else(|| "none".to_owned(), |value| value.to_rfc3339()),
        ]);
        parts.extend(source.addresses.iter().cloned());
    }
    ordered_address_hash(&parts)
}

fn parse_args<I, F>(args: I, read_env: F) -> Result<Options, Box<dyn Error>>
where
    I: IntoIterator,
    I::Item: Into<String>,
    F: Fn(&str) -> Option<String>,
{
    let mut cluster = None;
    let mut rpc_url = None;
    let mut legacy_kind = None;
    let mut expected_table_count = None;
    let mut expected_fleet_hash = None;
    let mut history_limit = DEFAULT_HISTORY_LIMIT;
    let mut admin_write = false;
    let mut reason = None;
    let mut updated_by = None;
    let mut args = args.into_iter().map(Into::into);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--cluster" => cluster = Some(next_value(&mut args, "--cluster")?),
            "--rpc-url" => rpc_url = Some(next_value(&mut args, "--rpc-url")?),
            "--legacy-kind" => {
                legacy_kind = Some(LegacyLookupTableKind::from_str(&next_value(
                    &mut args,
                    "--legacy-kind",
                )?)?)
            }
            "--expected-table-count" => {
                expected_table_count =
                    Some(next_value(&mut args, "--expected-table-count")?.parse()?)
            }
            "--expected-fleet-hash" => {
                expected_fleet_hash = Some(next_value(&mut args, "--expected-fleet-hash")?)
            }
            "--history-limit" => {
                history_limit = next_value(&mut args, "--history-limit")?.parse()?;
                if history_limit == 0 {
                    return Err("--history-limit must be at least 1".into());
                }
            }
            "--admin-write" => admin_write = true,
            "--reason" => reason = Some(next_value(&mut args, "--reason")?),
            "--updated-by" => updated_by = Some(next_value(&mut args, "--updated-by")?),
            "--help" | "-h" => return Err(usage().into()),
            other => return Err(format!("unknown argument {other:?}\n{}", usage()).into()),
        }
    }
    let cluster = cluster
        .or_else(|| read_env(CLUSTER_ENV))
        .filter(|value| !value.trim().is_empty())
        .ok_or("--cluster or YIELD_ALT_CLUSTER is required")?;
    let rpc_url = rpc_url
        .or_else(|| read_env(RPC_URL_ENV))
        .filter(|value| !value.trim().is_empty())
        .ok_or("--rpc-url or SOLANA_RPC_URL is required")?;
    let database_url = read_env(DATABASE_URL_ENV)
        .filter(|value| !value.trim().is_empty())
        .ok_or("NEON_DATABASE_URL is required")?;
    let legacy_kind = legacy_kind.ok_or("--legacy-kind is required")?;
    let expected_table_count: usize = expected_table_count
        .filter(|count| *count > 0 && *count <= 100)
        .ok_or("--expected-table-count between 1 and 100 is required")?;
    if expected_fleet_hash
        .as_deref()
        .is_some_and(|hash| hash.len() != 64 || !hash.bytes().all(|byte| byte.is_ascii_hexdigit()))
    {
        return Err("--expected-fleet-hash must be a 64-character hexadecimal hash".into());
    }
    let has_operator_metadata = reason.is_some() || updated_by.is_some();
    if admin_write
        && (reason
            .as_deref()
            .is_none_or(|value| value.trim().is_empty())
            || updated_by
                .as_deref()
                .is_none_or(|value| value.trim().is_empty()))
    {
        return Err("--admin-write requires non-empty --reason and --updated-by".into());
    }
    if admin_write && expected_fleet_hash.is_none() {
        return Err(
            "--admin-write requires --expected-fleet-hash from an approved dry-run inventory"
                .into(),
        );
    }
    if !admin_write && has_operator_metadata {
        return Err("--reason and --updated-by are valid only with --admin-write".into());
    }
    let configured_tables = read_env(CONFIGURED_TABLES_ENV)
        .map(|raw| {
            raw.split(|character: char| character == ',' || character.is_ascii_whitespace())
                .filter(|value| !value.is_empty())
                .map(|value| {
                    Pubkey::from_str(value)
                        .map(|pubkey| pubkey.to_string())
                        .map_err(|_| {
                            format!("{CONFIGURED_TABLES_ENV} contains an invalid public key")
                        })
                })
                .collect::<Result<BTreeSet<_>, _>>()
        })
        .transpose()?;
    Ok(Options {
        cluster,
        rpc_url,
        database_url,
        legacy_kind,
        expected_table_count,
        expected_fleet_hash,
        history_limit,
        configured_tables: configured_tables.unwrap_or_default(),
        admin_write,
        reason,
        updated_by,
    })
}

fn next_value(
    args: &mut impl Iterator<Item = String>,
    flag: &str,
) -> Result<String, Box<dyn Error>> {
    args.next()
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| format!("{flag} requires a value").into())
}

fn usage() -> &'static str {
    "Usage: route-lookup-table-legacy-import --cluster <CLUSTER> --legacy-kind <legacy_route|legacy_mixed> --expected-table-count <1..100> [--expected-fleet-hash <HASH>] [--history-limit <N>] [--rpc-url <URL>] [--admin-write --reason <TEXT> --updated-by <ID>]\n\nDry-run is the default and performs zero database writes. Every mode requires NEON_DATABASE_URL plus SOLANA_RPC_URL/--rpc-url, validates the explicit cluster genesis, inventories every extant ALT owned by the policy authority using both program-account and transaction-history discovery, excludes classified v2 family tables, and verifies the complete eligible legacy fleet in one finalized getMultipleAccounts snapshot. Dry-run prints the approved inventory fleet hash; --admin-write requires that exact hash and count and records the already-verified fleet atomically. YIELD_ROUTE_LOOKUP_TABLES, when set, is checked for unregistered tables. The command never loads a signer or sends a transaction."
}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_sdk::address_lookup_table::state::{AddressLookupTable, LookupTableMeta};
    use std::{borrow::Cow, collections::HashMap};

    fn env_map<'a>(values: &'a [(&'a str, &'a str)]) -> impl Fn(&str) -> Option<String> + 'a {
        let values = values
            .iter()
            .map(|(key, value)| ((*key).to_owned(), (*value).to_owned()))
            .collect::<HashMap<_, _>>();
        move |name| values.get(name).cloned()
    }

    fn source(authority: Pubkey, addresses: &[Pubkey]) -> LegacyLookupTableImportSource {
        let addresses = addresses
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>();
        LegacyLookupTableImportSource {
            id: 7,
            cluster: "mainnet-beta".to_owned(),
            scope: "legacy-scope".to_owned(),
            table_address: Pubkey::new_unique().to_string(),
            authority: authority.to_string(),
            status: "usable".to_owned(),
            durable: true,
            address_count: addresses.len() as i32,
            address_hash: ordered_address_hash(&addresses),
            addresses,
            legacy_kind: None,
            legacy_import_run_id: None,
            last_extended_slot: Some(10),
            last_extended_start_index: Some(0),
            last_verified_slot: None,
            last_verified_at: None,
        }
    }

    fn account(authority: Pubkey, addresses: &[Pubkey], last_extended_slot: u64) -> Account {
        let mut meta = LookupTableMeta::new(authority);
        meta.last_extended_slot = last_extended_slot;
        meta.last_extended_slot_start_index = 0;
        let data = AddressLookupTable {
            meta,
            addresses: Cow::Owned(addresses.to_vec()),
        }
        .serialize_for_tests()
        .unwrap();
        Account {
            lamports: 1,
            data,
            owner: address_lookup_table_program::id(),
            executable: false,
            rent_epoch: 0,
        }
    }

    #[test]
    fn dry_run_is_default_and_write_requires_operator_metadata() {
        let env = [
            (DATABASE_URL_ENV, "postgresql://example.invalid/db"),
            (RPC_URL_ENV, "https://rpc.example.invalid"),
            (CLUSTER_ENV, "mainnet-beta"),
        ];
        let dry = parse_args(
            [
                "--legacy-kind",
                "legacy_mixed",
                "--expected-table-count",
                "31",
            ],
            env_map(&env),
        )
        .unwrap();
        assert!(!dry.admin_write);
        assert!(parse_args(
            [
                "--legacy-kind",
                "legacy_mixed",
                "--expected-table-count",
                "31",
                "--admin-write",
            ],
            env_map(&env),
        )
        .is_err());
        assert!(
            parse_args(
                [
                    "--legacy-kind",
                    "legacy_mixed",
                    "--expected-table-count",
                    "31",
                    "--expected-fleet-hash",
                    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                    "--admin-write",
                    "--reason",
                    "verified fleet",
                    "--updated-by",
                    "operator",
                ],
                env_map(&env),
            )
            .unwrap()
            .admin_write
        );
    }

    #[test]
    fn canonical_hash_path_still_passes_full_verification() {
        let authority = Pubkey::new_unique();
        let addresses = [Pubkey::new_unique(), Pubkey::new_unique()];
        let mut source = source(authority, &addresses);
        source.legacy_import_run_id = Some(99);
        let account = account(authority, &addresses, 10);
        let verified = verify_source(
            &source,
            Some(&account),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .unwrap();
        assert_eq!(verified.source, source);
        assert_eq!(verified.observed_last_extended_slot, 10);
        assert_eq!(
            verified.observed_address_hash,
            ordered_address_hash(&verified.observed_addresses)
        );
    }

    #[test]
    fn historical_v1_hash_golden_vector_is_frozen() {
        let first = "11111111111111111111111111111111".to_owned();
        let second = "AddressLookupTab1e1111111111111111111111111".to_owned();
        let expected = "d1890c4c261f8c57ad5e722b4f542e88e8f4d18bb87262701ddef94634b2e62e";

        assert_eq!(
            historical_legacy_lookup_table_address_hash(&[first.clone(), second.clone()]),
            expected
        );
        assert_eq!(
            historical_legacy_lookup_table_address_hash(&[second, first]),
            expected,
            "the frozen v1 digest sorted address strings before hashing"
        );
    }

    #[test]
    fn unimported_historical_v1_hash_passes_and_yields_canonical_observation() {
        let authority = Pubkey::new_unique();
        let addresses = [Pubkey::new_unique(), Pubkey::new_unique()];
        let mut source = source(authority, &addresses);
        let canonical_hash = ordered_address_hash(&source.addresses);
        source.address_hash = historical_legacy_lookup_table_address_hash(&source.addresses);
        assert_ne!(source.address_hash, canonical_hash);

        let verified = verify_source(
            &source,
            Some(&account(authority, &addresses, 10)),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .unwrap();

        assert_eq!(verified.source.address_hash, source.address_hash);
        assert_eq!(verified.observed_address_hash, canonical_hash);
        assert_eq!(verified.observed_addresses, source.addresses);
    }

    #[test]
    fn historical_v1_hash_is_rejected_after_import() {
        let authority = Pubkey::new_unique();
        let addresses = [Pubkey::new_unique(), Pubkey::new_unique()];
        let mut source = source(authority, &addresses);
        source.address_hash = historical_legacy_lookup_table_address_hash(&source.addresses);
        source.legacy_import_run_id = Some(99);

        let error = verify_source(
            &source,
            Some(&account(authority, &addresses, 10)),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .unwrap_err();

        assert_eq!(
            error,
            "persisted address hash differs from ordered address list"
        );

        let mut classified_only = source;
        classified_only.legacy_import_run_id = None;
        classified_only.legacy_kind = Some(LegacyLookupTableKind::LegacyMixed);
        assert_eq!(
            verify_source(
                &classified_only,
                Some(&account(authority, &addresses, 10)),
                11,
                LegacyLookupTableKind::LegacyMixed,
            )
            .unwrap_err(),
            "persisted address hash differs from ordered address list"
        );
    }

    #[test]
    fn reordered_rpc_membership_fails_even_when_historical_set_hash_matches() {
        let authority = Pubkey::new_unique();
        let addresses = [Pubkey::new_unique(), Pubkey::new_unique()];
        let reordered = [addresses[1], addresses[0]];
        let mut source = source(authority, &addresses);
        source.address_hash = historical_legacy_lookup_table_address_hash(&source.addresses);
        let reordered_strings = reordered
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>();
        assert_eq!(
            historical_legacy_lookup_table_address_hash(&reordered_strings),
            source.address_hash
        );

        let error = verify_source(
            &source,
            Some(&account(authority, &reordered, 10)),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .unwrap_err();

        assert_eq!(
            error,
            "RPC full ordered membership differs from the persisted address list"
        );
    }

    #[test]
    fn invalid_noncanonical_nonhistorical_hash_fails_closed() {
        let authority = Pubkey::new_unique();
        let addresses = [Pubkey::new_unique(), Pubkey::new_unique()];
        let mut source = source(authority, &addresses);
        source.address_hash = "0".repeat(64);

        let error = verify_source(
            &source,
            Some(&account(authority, &addresses, 10)),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .unwrap_err();

        assert_eq!(
            error,
            "persisted address hash differs from ordered address list"
        );
    }

    #[test]
    fn missing_owner_authority_prefix_and_warmup_drift_fail_closed() {
        let authority = Pubkey::new_unique();
        let addresses = [Pubkey::new_unique(), Pubkey::new_unique()];
        let source = source(authority, &addresses);
        assert!(verify_source(&source, None, 11, LegacyLookupTableKind::LegacyMixed,).is_err());

        let mut wrong_owner = account(authority, &addresses, 10);
        wrong_owner.owner = Pubkey::new_unique();
        assert!(verify_source(
            &source,
            Some(&wrong_owner),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .is_err());

        let wrong_authority = account(Pubkey::new_unique(), &addresses, 10);
        assert!(verify_source(
            &source,
            Some(&wrong_authority),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .is_err());

        let wrong_prefix = account(authority, &[addresses[1], addresses[0]], 10);
        assert!(verify_source(
            &source,
            Some(&wrong_prefix),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .is_err());

        let not_warm = account(authority, &addresses, 11);
        assert!(verify_source(
            &source,
            Some(&not_warm),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .is_err());
    }

    #[test]
    fn lifecycle_executable_count_hash_and_extension_metadata_fail_closed() {
        let authority = Pubkey::new_unique();
        let addresses = [Pubkey::new_unique(), Pubkey::new_unique()];
        let source = source(authority, &addresses);

        let mut executable = account(authority, &addresses, 10);
        executable.executable = true;
        assert!(verify_source(
            &source,
            Some(&executable),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .is_err());

        let mut deactivating_meta = LookupTableMeta::new(authority);
        deactivating_meta.deactivation_slot = 12;
        deactivating_meta.last_extended_slot = 10;
        let mut deactivating = account(authority, &addresses, 10);
        deactivating.data = AddressLookupTable {
            meta: deactivating_meta,
            addresses: Cow::Owned(addresses.to_vec()),
        }
        .serialize_for_tests()
        .unwrap();
        assert!(verify_source(
            &source,
            Some(&deactivating),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .is_err());

        let mut invalid_start_meta = LookupTableMeta::new(authority);
        invalid_start_meta.last_extended_slot = 10;
        invalid_start_meta.last_extended_slot_start_index = 3;
        let mut invalid_start = account(authority, &addresses, 10);
        invalid_start.data = AddressLookupTable {
            meta: invalid_start_meta,
            addresses: Cow::Owned(addresses.to_vec()),
        }
        .serialize_for_tests()
        .unwrap();
        assert!(verify_source(
            &source,
            Some(&invalid_start),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .is_err());

        let valid = account(authority, &addresses, 10);
        let mut wrong_count = source.clone();
        wrong_count.address_count += 1;
        assert!(verify_source(
            &wrong_count,
            Some(&valid),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .is_err());
        let mut wrong_hash = source.clone();
        wrong_hash.address_hash = "0".repeat(64);
        assert!(verify_source(
            &wrong_hash,
            Some(&valid),
            11,
            LegacyLookupTableKind::LegacyMixed,
        )
        .is_err());
    }

    #[test]
    fn registry_fleet_hash_fences_the_complete_persisted_snapshot() {
        let authority = Pubkey::new_unique();
        let addresses = [Pubkey::new_unique(), Pubkey::new_unique()];
        let original = source(authority, &addresses);
        let baseline = registry_fleet_hash(std::slice::from_ref(&original));

        let mut mutations = Vec::new();
        let mut changed = original.clone();
        changed.cluster = "devnet".to_owned();
        mutations.push(changed);
        let mut changed = original.clone();
        changed.status = "warming".to_owned();
        mutations.push(changed);
        let mut changed = original.clone();
        changed.legacy_kind = Some(LegacyLookupTableKind::LegacyMixed);
        mutations.push(changed);
        let mut changed = original.clone();
        changed.legacy_import_run_id = Some(99);
        mutations.push(changed);
        let mut changed = original.clone();
        changed.last_extended_slot = Some(11);
        mutations.push(changed);
        let mut changed = original.clone();
        changed.last_verified_slot = Some(12);
        changed.last_verified_at = Some(Utc::now());
        mutations.push(changed);

        for changed in mutations {
            assert_ne!(
                registry_fleet_hash(&[changed]),
                baseline,
                "every persisted fleet field must participate in operator approval"
            );
        }
    }
}
