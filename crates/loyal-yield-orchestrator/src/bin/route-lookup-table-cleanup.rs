use std::{
    collections::BTreeSet,
    env,
    error::Error,
    str::FromStr,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
#[cfg(test)]
use loyal_yield_orchestrator::LookupTableLifecycle;
use loyal_yield_orchestrator::{
    keypair_from_string,
    rpc_safety::{
        redacted_external_error, redacted_rpc_endpoint, validate_rpc_endpoint,
        validate_rpc_genesis_hash,
    },
    LegacyLookupTableCleanupProtection, LookupTableCleanupProtection, LookupTableOperationKind,
    NeonSqlClient, NeonSqlConfig, VerifiedLegacyLookupTableCleanup, POLICY_KEYPAIR_ENV,
};
use serde::Deserialize;
use serde_json::{json, Map, Value};
use sha2::{Digest, Sha256};
use solana_client::rpc_client::RpcClient;
use solana_sdk::address_lookup_table::{
    instruction as address_lookup_table_instruction, program as address_lookup_table_program,
    state::{estimate_last_valid_slot, AddressLookupTable},
};
use solana_sdk::{
    account::Account,
    commitment_config::CommitmentConfig,
    instruction::Instruction,
    packet::PACKET_DATA_SIZE,
    pubkey::Pubkey,
    signature::{Signature, Signer},
    transaction::Transaction,
};

const AFFECTED_POLICY_AUTHORITY: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const AUDITED_KEYPAIR_ENVS: &[&str] = &[
    "YIELD_ROUTER_KEYPAIR",
    "POLICY_KEYPAIR",
    "DEPLOYMENT_PK",
    "SOLANA_TESTING_PK",
];

#[derive(Debug)]
struct Options {
    cluster: String,
    rpc_url: String,
    authorities: Vec<Pubkey>,
    tables: Vec<Pubkey>,
    allowlist: Vec<Pubkey>,
    recipient: Option<Pubkey>,
    execute: bool,
    scan_program_accounts: bool,
    scan_history: bool,
    include_env_authorities: bool,
    limit: usize,
    history_limit: usize,
    min_slot: Option<u64>,
    authority_key_env: Option<String>,
    simulate_before_submit: bool,
    bundle_size: usize,
    trace_timing: bool,
    expected_fleet_count: Option<usize>,
    expected_fleet_hash: Option<String>,
}

#[derive(Debug)]
struct Candidate {
    table_address: Pubkey,
    lamports: u64,
    owner: Pubkey,
    authority: Option<Pubkey>,
    address_count: usize,
    addresses: Vec<Pubkey>,
    deactivation_slot: u64,
    last_extended_slot: u64,
}

#[derive(Clone, Debug)]
struct HistoryEvent {
    signature: String,
    slot: u64,
    block_time: Option<i64>,
    kind: &'static str,
    table_address: Pubkey,
    authority: Option<Pubkey>,
    payer_or_recipient: Option<Pubkey>,
    new_address_count: Option<usize>,
}

#[derive(Debug)]
struct PlannedCleanup {
    row_index: usize,
    table_address: Pubkey,
    kind: &'static str,
    instruction: Instruction,
    recipient: Option<Pubkey>,
    reclaimed_lamports: u64,
    expected_authority: Pubkey,
    expected_address_count: usize,
    expected_address_hash: String,
    expected_cleanup_authorization_token: String,
}

#[derive(Debug)]
struct CleanupTransactionResult {
    signature: String,
    simulation: Value,
    transaction_packet: CleanupTransactionPacket,
    estimated_fee_lamports: u64,
    recipient_balance_before: Option<u64>,
    recipient_balance_after: Option<u64>,
    expected_refund_lamports: u64,
    minimum_net_recipient_increase_lamports: u64,
}

#[derive(Clone, Copy, Debug)]
struct CleanupTransactionPacket {
    packet_size_bytes: usize,
    packet_data_size_bytes: usize,
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
    error: Option<ProgramAccountsV2Error>,
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

#[derive(Debug, Deserialize)]
struct ProgramAccountsV2Error {
    code: i64,
    message: String,
}

#[derive(Debug)]
struct TraceLog {
    enabled: bool,
    started_at: Instant,
}

impl TraceLog {
    fn new(enabled: bool) -> Self {
        Self {
            enabled,
            started_at: Instant::now(),
        }
    }

    fn event(&self, event: &str, fields: Value) {
        if !self.enabled {
            return;
        }
        let mut payload = match fields {
            Value::Object(map) => map,
            _ => Map::new(),
        };
        payload.insert("event".to_owned(), json!(event));
        payload.insert("timestampMs".to_owned(), json!(unix_timestamp_ms()));
        payload.insert(
            "elapsedMs".to_owned(),
            json!(duration_ms(self.started_at.elapsed())),
        );
        eprintln!("{}", Value::Object(payload));
    }

    fn finish(&self, event: &str, started_at: Instant, fields: Value) {
        if !self.enabled {
            return;
        }
        let mut payload = match fields {
            Value::Object(map) => map,
            _ => Map::new(),
        };
        payload.insert(
            "durationMs".to_owned(),
            json!(duration_ms(started_at.elapsed())),
        );
        self.event(event, Value::Object(payload));
    }
}

fn safe_cleanup_operational_error(error: &dyn std::fmt::Display) -> String {
    redacted_external_error(&error.to_string())
}

fn safe_cleanup_operational_error_with_context(
    context: &str,
    error: &dyn std::fmt::Display,
) -> String {
    redacted_external_error(&format!("{context}: {error}"))
}

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!(
            "{}",
            json!({
                "event": "alt_cleanup_fatal",
                "error": safe_cleanup_operational_error(error.as_ref()),
            })
        );
        std::process::exit(1);
    }
}

async fn run() -> Result<(), Box<dyn Error>> {
    let options = parse_args(env::args().skip(1))?;
    validate_rpc_endpoint(&options.rpc_url)?;
    let database_url = env::var("NEON_DATABASE_URL")
        .map_err(|_| "NEON_DATABASE_URL is required for binding-aware ALT cleanup")?;
    let database = NeonSqlClient::connect(NeonSqlConfig::new(database_url)).await?;
    database
        .require_schema_migration(19, "legacy_lookup_table_imports")
        .await?;
    let trace = TraceLog::new(options.trace_timing);
    trace.event(
        "cleanup.start",
        json!({
            "cluster": options.cluster,
            "execute": options.execute,
            "scanProgramAccounts": options.scan_program_accounts,
            "scanHistory": options.scan_history,
            "limit": options.limit,
            "historyLimit": options.history_limit,
            "bundleSize": options.bundle_size,
        }),
    );
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.clone(), CommitmentConfig::finalized());
    let observed_genesis_hash = rpc
        .get_genesis_hash()
        .map_err(|_| "failed to read genesis hash from configured ALT cleanup RPC endpoint")?;
    validate_rpc_genesis_hash(&options.cluster, observed_genesis_hash).map_err(|error| {
        format!("refusing ALT cleanup read or mutation against mismatched RPC: {error}")
    })?;
    let signer = if options.execute {
        let signer = load_authority_signer(&options)?;
        let expected_policy = Pubkey::from_str(AFFECTED_POLICY_AUTHORITY)?;
        if signer.pubkey() != expected_policy {
            return Err(format!(
                "POLICY_KEYPAIR pubkey {} does not match the standard policy authority {}",
                signer.pubkey(),
                expected_policy
            )
            .into());
        }
        Some(signer)
    } else {
        None
    };
    let phase_started = Instant::now();
    let protected = protected_legacy_tables(&database).await?;
    trace.finish(
        "cleanup.protected_tables",
        phase_started,
        json!({ "count": protected.len() }),
    );
    let phase_started = Instant::now();
    let env_tables = route_lookup_tables_from_env()?;
    let manual_allowlist = options.allowlist.iter().copied().collect::<BTreeSet<_>>();
    let mut protected_all = protected;
    protected_all.extend(env_tables.iter().copied());
    protected_all.extend(manual_allowlist.iter().copied());
    trace.finish(
        "cleanup.allowlist",
        phase_started,
        json!({
            "envTableCount": env_tables.len(),
            "manualAllowlistCount": manual_allowlist.len(),
            "protectedTableCount": protected_all.len(),
        }),
    );

    let mut table_addresses = options.tables.clone();
    normalize_table_addresses(&mut table_addresses, options.limit);
    let history_events = if options.scan_history
        && remaining_table_limit(options.limit, table_addresses.len()) != Some(0)
    {
        let phase_started = Instant::now();
        let table_limit = remaining_table_limit(options.limit, table_addresses.len());
        let events = discover_tables_by_history(
            &options.rpc_url,
            &options.authorities,
            &options,
            table_limit,
            &trace,
        )
        .await?;
        trace.finish(
            "cleanup.scan_history",
            phase_started,
            json!({
                "eventCount": events.len(),
                "tableLimit": table_limit,
            }),
        );
        add_table_addresses(
            &mut table_addresses,
            events.iter().map(|event| event.table_address),
            options.limit,
        );
        events
    } else {
        if options.scan_history {
            trace.event(
                "cleanup.scan_history.skip",
                json!({ "reason": "candidate_limit_already_reached" }),
            );
        }
        Vec::new()
    };
    if options.scan_program_accounts {
        match remaining_table_limit(options.limit, table_addresses.len()) {
            Some(0) => trace.event(
                "cleanup.scan_program_accounts.skip",
                json!({ "reason": "candidate_limit_already_reached" }),
            ),
            remaining => {
                let phase_started = Instant::now();
                let discovered = discover_tables_by_program_scan(
                    &rpc,
                    &options.rpc_url,
                    &options.authorities,
                    remaining.unwrap_or(options.limit),
                )
                .await?;
                trace.finish(
                    "cleanup.scan_program_accounts",
                    phase_started,
                    json!({
                        "discoveredCount": discovered.len(),
                        "remainingLimit": remaining,
                    }),
                );
                add_table_addresses(&mut table_addresses, discovered, options.limit);
            }
        }
    }
    trace.event(
        "cleanup.discovery.complete",
        json!({ "candidateAddressCount": table_addresses.len() }),
    );

    let phase_started = Instant::now();
    let current_slot = rpc.get_slot_with_commitment(CommitmentConfig::finalized())?;
    trace.finish(
        "cleanup.current_slot",
        phase_started,
        json!({ "currentSlot": current_slot }),
    );
    let mut rows = Vec::new();
    let mut planned_cleanups = Vec::new();
    let queued_operation_count = 0_usize;
    let mut total_reclaimable = 0_u64;
    let mut total_reclaimed = 0_u64;

    let phase_started = Instant::now();
    let loaded_candidates = load_candidates(&rpc, &table_addresses, &trace)?;
    let mut classified_v2_tables = BTreeSet::new();
    for (table_address, loaded) in &loaded_candidates {
        if loaded.is_ok()
            && database
                .lookup_table_cleanup_protection(&options.cluster, &table_address.to_string())
                .await?
                .is_some()
        {
            classified_v2_tables.insert(*table_address);
        }
    }
    let policy_authority = Pubkey::from_str(AFFECTED_POLICY_AUTHORITY)?;
    let legacy_inventory = loaded_candidates
        .iter()
        .filter_map(|(_, loaded)| loaded.as_ref().ok())
        .filter(|candidate| {
            candidate.owner == address_lookup_table_program::id()
                && candidate.authority == Some(policy_authority)
                && !classified_v2_tables.contains(&candidate.table_address)
        })
        .collect::<Vec<_>>();
    let inventory_fleet_hash = approved_policy_fleet_hash(&legacy_inventory);
    if options
        .expected_fleet_count
        .is_some_and(|expected| expected != legacy_inventory.len())
    {
        return Err(format!(
            "policy legacy fleet has {} extant tables, but --expected-fleet-count is {}",
            legacy_inventory.len(),
            options.expected_fleet_count.expect("checked Some")
        )
        .into());
    }
    if options
        .expected_fleet_hash
        .as_deref()
        .is_some_and(|expected| expected != inventory_fleet_hash)
    {
        return Err("policy legacy fleet differs from --expected-fleet-hash".into());
    }
    trace.finish(
        "cleanup.candidate_accounts",
        phase_started,
        json!({
            "accountCount": loaded_candidates.len(),
            "legacyFleetCount": legacy_inventory.len(),
            "excludedV2TableCount": classified_v2_tables.len(),
            "inventoryFleetHash": inventory_fleet_hash,
        }),
    );
    let phase_started = Instant::now();
    for (table_address, loaded_candidate) in &loaded_candidates {
        let candidate = match loaded_candidate {
            Ok(candidate) => candidate,
            Err(error) => {
                rows.push(json!({
                    "table": table_address.to_string(),
                    "action": "skip",
                    "reason": safe_cleanup_operational_error_with_context(
                        "fetch_or_decode_failed",
                        &error,
                    ),
                }));
                continue;
            }
        };
        let registered_protection = database
            .lookup_table_cleanup_protection(&options.cluster, &candidate.table_address.to_string())
            .await?;
        let legacy_protection = if registered_protection.is_none() {
            database
                .legacy_lookup_table_cleanup_protection(
                    &options.cluster,
                    &candidate.table_address.to_string(),
                )
                .await?
        } else {
            None
        };
        let manually_protected = protected_all.contains(&candidate.table_address);
        let (action, reason) = if manually_protected {
            ("skip", "legacy_registry_env_or_manual_allowlist".to_owned())
        } else if registered_protection.is_some() {
            (
                "skip",
                "classified_v2_table_use_dedicated_provisioner".to_owned(),
            )
        } else if let Some(protection) = legacy_protection.as_ref() {
            classify_imported_legacy_candidate(candidate, protection, current_slot)
        } else {
            ("skip", "not_verified_imported_legacy_inventory".to_owned())
        };
        if matches!(action, "deactivate" | "close") {
            total_reclaimable = total_reclaimable.saturating_add(candidate.lamports);
        }
        let candidate_history = history_events
            .iter()
            .filter(|event| event.table_address == candidate.table_address)
            .map(|event| history_event_json(event, &protected_all))
            .collect::<Vec<_>>();

        let execution = Value::Null;
        if options.execute && matches!(action, "deactivate" | "close") {
            if registered_protection.is_none() {
                let authorization = require_retired_imported_legacy_cleanup_authorization(
                    &database,
                    &options.cluster,
                    candidate,
                    action,
                    legacy_protection.as_ref(),
                )
                .await?;
                let signer = signer
                    .as_ref()
                    .ok_or("POLICY_KEYPAIR was not loaded for cleanup execute")?;
                let authority = candidate
                    .authority
                    .ok_or("candidate had no authority during execute")?;
                if signer.pubkey() != authority {
                    return Err(format!(
                        "authority signer {} does not match table {} authority {}",
                        signer.pubkey(),
                        candidate.table_address,
                        authority
                    )
                    .into());
                }
                let row_index = rows.len();
                if action == "deactivate" {
                    let instruction = address_lookup_table_instruction::deactivate_lookup_table(
                        candidate.table_address,
                        signer.pubkey(),
                    );
                    planned_cleanups.push(PlannedCleanup {
                        row_index,
                        table_address: candidate.table_address,
                        kind: "deactivate_lookup_table",
                        instruction,
                        recipient: None,
                        reclaimed_lamports: 0,
                        expected_authority: authority,
                        expected_address_count: candidate.address_count,
                        expected_address_hash: ordered_candidate_address_hash(&candidate.addresses),
                        expected_cleanup_authorization_token: authorization.authorization_token,
                    });
                } else {
                    let recipient = options.recipient.unwrap_or_else(|| signer.pubkey());
                    if recipient != signer.pubkey() {
                        return Err(
                            "close recipient must equal the POLICY_KEYPAIR public key".into()
                        );
                    }
                    let instruction = address_lookup_table_instruction::close_lookup_table(
                        candidate.table_address,
                        signer.pubkey(),
                        recipient,
                    );
                    planned_cleanups.push(PlannedCleanup {
                        row_index,
                        table_address: candidate.table_address,
                        kind: "close_lookup_table",
                        instruction,
                        recipient: Some(recipient),
                        reclaimed_lamports: candidate.lamports,
                        expected_authority: authority,
                        expected_address_count: candidate.address_count,
                        expected_address_hash: ordered_candidate_address_hash(&candidate.addresses),
                        expected_cleanup_authorization_token: authorization.authorization_token,
                    });
                }
            }
        }

        rows.push(json!({
            "table": candidate.table_address.to_string(),
            "owner": candidate.owner.to_string(),
            "authority": candidate.authority.map(|authority| authority.to_string()),
            "status": lookup_table_status(&candidate, current_slot),
            "addressCount": candidate.address_count,
            "lamportsReclaimable": candidate.lamports.to_string(),
            "lastExtendedSlot": candidate.last_extended_slot,
            "deactivationSlot": candidate.deactivation_slot,
            "action": action,
            "reason": reason,
            "registeredControlPlane": registered_protection.as_ref().map(cleanup_protection_json),
            "legacyCleanupProtection": legacy_protection.as_ref().map(legacy_cleanup_protection_json),
            "historyEvents": candidate_history,
            "execution": execution,
        }));
    }
    trace.finish(
        "cleanup.candidates",
        phase_started,
        json!({
            "rowCount": rows.len(),
            "plannedExecutionCount": planned_cleanups.len(),
            "queuedOperationCount": queued_operation_count,
        }),
    );

    if options.execute && !planned_cleanups.is_empty() {
        let phase_started = Instant::now();
        let signer = signer.as_ref().ok_or("--execute requires POLICY_KEYPAIR")?;
        total_reclaimed = execute_planned_cleanups(
            &database,
            &rpc,
            &options,
            signer.as_ref(),
            &planned_cleanups,
            &mut rows,
        )
        .await?;
        trace.finish(
            "cleanup.execute",
            phase_started,
            json!({ "plannedExecutionCount": planned_cleanups.len() }),
        );
    }

    let phase_started = Instant::now();
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": if options.execute { "lookup_table_cleanup_execute" } else { "lookup_table_cleanup_dry_run" },
            "cluster": options.cluster,
            "execute": options.execute,
            "simulateBeforeSubmit": options.simulate_before_submit,
            "bundleSize": options.bundle_size,
            "traceTiming": options.trace_timing,
            "rpcUrl": redacted_rpc_endpoint(&options.rpc_url),
            "authorities": options.authorities.iter().map(ToString::to_string).collect::<Vec<_>>(),
            "includeEnvAuthorities": options.include_env_authorities,
            "scanProgramAccounts": options.scan_program_accounts,
            "scanHistory": options.scan_history,
            "historyLimit": options.history_limit,
            "minSlot": options.min_slot,
            "explicitTableCount": options.tables.len(),
            "protectedTableCount": protected_all.len(),
            "legacyFleetCount": legacy_inventory.len(),
            "inventoryFleetHash": inventory_fleet_hash,
            "excludedV2TableCount": classified_v2_tables.len(),
            "expectedFleetCount": options.expected_fleet_count,
            "expectedFleetHash": options.expected_fleet_hash,
            "feesRecoverable": false,
            "feeNote": "ALT account rent can be reclaimed after close; transaction fees are not recoverable.",
            "currentSlot": current_slot,
            "totalReclaimableLamports": total_reclaimable.to_string(),
            "totalReclaimedLamports": total_reclaimed.to_string(),
            "plannedExecutionCount": planned_cleanups.len(),
            "queuedProvisionerOperationCount": queued_operation_count,
            "registeredMutationBoundary": "dedicated_provisioner",
            "legacyDirectExecutionCount": planned_cleanups.len(),
            "historyEventCount": history_events.len(),
            "historyEvents": history_events.iter().map(|event| history_event_json(event, &protected_all)).collect::<Vec<_>>(),
            "candidates": rows,
        }))?
    );
    trace.finish("cleanup.output", phase_started, json!({}));
    trace.event("cleanup.done", json!({}));
    Ok(())
}

async fn require_retired_imported_legacy_cleanup_authorization(
    database: &NeonSqlClient,
    cluster: &str,
    candidate: &Candidate,
    action: &str,
    expected: Option<&LegacyLookupTableCleanupProtection>,
) -> Result<LegacyLookupTableCleanupProtection, Box<dyn Error>> {
    let protection = database
        .legacy_lookup_table_cleanup_protection(cluster, &candidate.table_address.to_string())
        .await?
        .ok_or("legacy ALT is missing imported cleanup evidence")?;
    if expected
        .is_some_and(|expected| expected.authorization_token != protection.authorization_token)
    {
        return Err("legacy ALT cleanup authorization changed during planning".into());
    }
    if protection.expected_authority
        != candidate
            .authority
            .map_or_else(String::new, |authority| authority.to_string())
        || usize::try_from(protection.address_count)? != candidate.address_count
        || protection.address_hash != ordered_candidate_address_hash(&candidate.addresses)
        || protection.ordered_addresses
            != candidate
                .addresses
                .iter()
                .map(ToString::to_string)
                .collect::<Vec<_>>()
    {
        return Err("legacy ALT chain identity differs from immutable import evidence".into());
    }
    let authorized = match action {
        "deactivate" => protection.can_deactivate,
        "close" => protection.can_close,
        _ => false,
    };
    if !authorized {
        return Err(format!(
            "legacy ALT {}/{} is not authorized to {action}: {}",
            cluster,
            candidate.table_address,
            protection.protection_reasons.join(",")
        )
        .into());
    }
    Ok(protection)
}

fn parse_args(args: impl IntoIterator<Item = String>) -> Result<Options, Box<dyn Error>> {
    parse_args_with_env(args, |name| env::var(name).ok())
}

fn parse_args_with_env<F>(
    args: impl IntoIterator<Item = String>,
    read_env: F,
) -> Result<Options, Box<dyn Error>>
where
    F: Fn(&str) -> Option<String>,
{
    let mut cluster = read_env("YIELD_ALT_CLUSTER");
    let mut rpc_url = read_env("SOLANA_RPC_URL").filter(|value| !value.trim().is_empty());
    let mut authorities = vec![Pubkey::from_str(AFFECTED_POLICY_AUTHORITY)?];
    let mut tables = Vec::new();
    let mut allowlist = Vec::new();
    let mut recipient = None;
    let mut execute = false;
    let mut scan_program_accounts = false;
    let mut scan_history = false;
    let mut include_env_authorities = false;
    let mut limit = 500_usize;
    let mut history_limit = 1_000_usize;
    let mut min_slot = None;
    let mut authority_key_env = None;
    let mut simulate_before_submit = false;
    let mut bundle_size = 1_usize;
    let mut trace_timing = false;
    let mut expected_fleet_count = None;
    let mut expected_fleet_hash = None;
    let mut iter = args.into_iter();
    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--cluster" => cluster = Some(iter.next().ok_or("--cluster requires a value")?),
            "--rpc-url" => {
                rpc_url = Some(iter.next().ok_or("--rpc-url requires a value")?);
            }
            "--authority" => authorities.push(parse_pubkey_arg(&arg, iter.next())?),
            "--table" => tables.push(parse_pubkey_arg(&arg, iter.next())?),
            "--allowlist" => allowlist.push(parse_pubkey_arg(&arg, iter.next())?),
            "--recipient" => recipient = Some(parse_pubkey_arg(&arg, iter.next())?),
            "--authority-key-env" => {
                authority_key_env = Some(iter.next().ok_or("--authority-key-env requires a value")?)
            }
            "--execute" => execute = true,
            "--dry-run" => execute = false,
            "--simulate-before-submit" => simulate_before_submit = true,
            "--trace-timing" => trace_timing = true,
            "--expected-fleet-count" => {
                expected_fleet_count = Some(
                    iter.next()
                        .ok_or("--expected-fleet-count requires a value")?
                        .parse()
                        .map_err(|_| "--expected-fleet-count must be a usize")?,
                );
            }
            "--expected-fleet-hash" => {
                expected_fleet_hash = Some(
                    iter.next()
                        .ok_or("--expected-fleet-hash requires a value")?,
                );
            }
            "--bundle-size" => {
                bundle_size = iter
                    .next()
                    .ok_or("--bundle-size requires a value")?
                    .parse()
                    .map_err(|_| "--bundle-size must be a usize")?;
                if bundle_size == 0 {
                    return Err("--bundle-size must be at least 1".into());
                }
            }
            "--scan-program-accounts" => scan_program_accounts = true,
            "--scan-history" => scan_history = true,
            "--include-env-authorities" => include_env_authorities = true,
            "--limit" => {
                limit = iter
                    .next()
                    .ok_or("--limit requires a value")?
                    .parse()
                    .map_err(|_| "--limit must be a usize")?;
            }
            "--history-limit" => {
                history_limit = iter
                    .next()
                    .ok_or("--history-limit requires a value")?
                    .parse()
                    .map_err(|_| "--history-limit must be a usize")?;
            }
            "--min-slot" => {
                min_slot = Some(
                    iter.next()
                        .ok_or("--min-slot requires a value")?
                        .parse()
                        .map_err(|_| "--min-slot must be a u64")?,
                );
            }
            "--help" | "-h" => {
                println!(
                    "Usage: route-lookup-table-cleanup --cluster <CLUSTER> --rpc-url <URL> [--table <PUBKEY>...] [--allowlist <PUBKEY>...] [--recipient <POLICY_PUBKEY>] [--scan-program-accounts] [--scan-history] [--history-limit <N>] [--min-slot <SLOT>] [--bundle-size 1] [--trace-timing] [--expected-fleet-count <N> --expected-fleet-hash <HASH> --execute]\n\nDry-run is the default. Every mode requires explicit YIELD_ALT_CLUSTER/--cluster, SOLANA_RPC_URL/--rpc-url, and NEON_DATABASE_URL. The RPC genesis hash is verified against the explicit cluster before any chain read or mutation. Execute always loads the standard POLICY_KEYPAIR, requires exhaustive program-account plus history discovery, requires the approved policy-authority legacy fleet count/hash, simulates every transaction, waits for finalization, and permits close refunds only to the policy signer. Classified v2 tables are never mutated by this command. Execute requires --bundle-size 1 so each mutation holds a dedicated database authorization fence through finalization. --trace-timing emits timestamped phase duration logs to stderr."
                );
                std::process::exit(0);
            }
            _ => return Err(format!("unknown argument: {arg}").into()),
        }
    }
    if include_env_authorities {
        authorities.extend(env_authority_pubkeys()?);
    }
    authorities.sort();
    authorities.dedup();
    let cluster = cluster.ok_or("YIELD_ALT_CLUSTER or --cluster is required")?;
    if !matches!(
        cluster.as_str(),
        "mainnet-beta" | "devnet" | "testnet" | "localnet"
    ) {
        return Err(format!(
            "YIELD_ALT_CLUSTER/--cluster must be mainnet-beta, devnet, testnet, or localnet; got {cluster:?}"
        )
        .into());
    }
    let rpc_url = rpc_url
        .filter(|value| !value.trim().is_empty())
        .ok_or("SOLANA_RPC_URL or --rpc-url is required for every cleanup mode")?;
    if expected_fleet_hash
        .as_deref()
        .is_some_and(|hash| hash.len() != 64 || !hash.bytes().all(|byte| byte.is_ascii_hexdigit()))
    {
        return Err("--expected-fleet-hash must be a 64-character hexadecimal hash".into());
    }
    if execute {
        let policy = Pubkey::from_str(AFFECTED_POLICY_AUTHORITY)?;
        if recipient.is_some_and(|value| value != policy) {
            return Err("--execute close recipient must equal the standard policy pubkey".into());
        }
        if authority_key_env
            .as_deref()
            .is_some_and(|name| name != POLICY_KEYPAIR_ENV)
        {
            return Err("--execute only permits the standard POLICY_KEYPAIR signer".into());
        }
        if include_env_authorities || authorities.iter().any(|authority| *authority != policy) {
            return Err("--execute only permits the standard policy authority inventory".into());
        }
        if expected_fleet_count.is_none() || expected_fleet_hash.is_none() {
            return Err(
                "--execute requires --expected-fleet-count and --expected-fleet-hash from an approved dry run"
                    .into(),
            );
        }
        if bundle_size != 1 {
            return Err(
                "--execute requires --bundle-size 1 so each legacy mutation holds its own database authorization fence"
                    .into(),
            );
        }
        scan_program_accounts = true;
        scan_history = true;
        limit = 0;
        simulate_before_submit = true;
    }
    Ok(Options {
        cluster,
        rpc_url,
        authorities,
        tables,
        allowlist,
        recipient,
        execute,
        scan_program_accounts,
        scan_history,
        include_env_authorities,
        limit,
        history_limit,
        min_slot,
        authority_key_env,
        simulate_before_submit,
        bundle_size,
        trace_timing,
        expected_fleet_count,
        expected_fleet_hash,
    })
}

fn env_authority_pubkeys() -> Result<Vec<Pubkey>, Box<dyn Error>> {
    let mut pubkeys = Vec::new();
    for name in AUDITED_KEYPAIR_ENVS {
        let Ok(value) = env::var(name) else {
            continue;
        };
        pubkeys.push(keypair_from_string(&value)?.pubkey());
    }
    Ok(pubkeys)
}

fn parse_pubkey_arg(flag: &str, value: Option<String>) -> Result<Pubkey, Box<dyn Error>> {
    let raw = value.ok_or_else(|| format!("{flag} requires a public key"))?;
    Pubkey::from_str(&raw).map_err(|_| format!("{flag} value {raw:?} is not a public key").into())
}

fn normalize_table_addresses(table_addresses: &mut Vec<Pubkey>, limit: usize) {
    let mut seen = BTreeSet::new();
    table_addresses.retain(|address| seen.insert(*address));
    if limit > 0 && table_addresses.len() > limit {
        table_addresses.truncate(limit);
    }
}

fn add_table_addresses(
    table_addresses: &mut Vec<Pubkey>,
    addresses: impl IntoIterator<Item = Pubkey>,
    limit: usize,
) {
    let mut seen = table_addresses.iter().copied().collect::<BTreeSet<_>>();
    for address in addresses {
        if seen.insert(address) {
            table_addresses.push(address);
            if limit > 0 && table_addresses.len() >= limit {
                break;
            }
        }
    }
}

fn remaining_table_limit(limit: usize, current_count: usize) -> Option<usize> {
    if limit == 0 {
        None
    } else {
        Some(limit.saturating_sub(current_count))
    }
}

fn duration_ms(duration: Duration) -> u128 {
    duration.as_millis()
}

fn unix_timestamp_ms() -> u128 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(duration_ms)
        .unwrap_or_default()
}

async fn protected_legacy_tables(
    client: &NeonSqlClient,
) -> Result<BTreeSet<Pubkey>, Box<dyn Error>> {
    let addresses = client
        .protected_legacy_route_lookup_table_addresses()
        .await?;
    parse_pubkey_set(addresses)
}

fn route_lookup_tables_from_env() -> Result<BTreeSet<Pubkey>, Box<dyn Error>> {
    let Ok(raw) = env::var("YIELD_ROUTE_LOOKUP_TABLES") else {
        return Ok(BTreeSet::new());
    };
    parse_pubkey_set(
        raw.split(|c: char| c == ',' || c.is_ascii_whitespace())
            .map(str::to_owned),
    )
}

fn parse_pubkey_set(
    values: impl IntoIterator<Item = String>,
) -> Result<BTreeSet<Pubkey>, Box<dyn Error>> {
    let mut set = BTreeSet::new();
    for value in values {
        let value = value.trim();
        if value.is_empty() {
            continue;
        }
        set.insert(Pubkey::from_str(value)?);
    }
    Ok(set)
}

async fn discover_tables_by_program_scan(
    rpc: &RpcClient,
    rpc_url: &str,
    authorities: &[Pubkey],
    limit: usize,
) -> Result<Vec<Pubkey>, Box<dyn Error>> {
    let accounts = match rpc.get_program_accounts(&address_lookup_table_program::id()) {
        Ok(accounts) => accounts,
        Err(error) => {
            return discover_tables_by_program_scan_v2(rpc_url, authorities, limit)
                .await
                .map_err(|fallback_error| {
                    format!(
                        "getProgramAccounts failed ({error}); getProgramAccountsV2 fallback failed ({fallback_error})"
                    )
                    .into()
                });
        }
    };
    let mut tables = Vec::new();
    for (table_address, account) in accounts {
        let Ok(table) = AddressLookupTable::deserialize(&account.data) else {
            continue;
        };
        if table
            .meta
            .authority
            .is_some_and(|authority| authorities.contains(&authority))
        {
            tables.push(table_address);
            if limit > 0 && tables.len() >= limit {
                break;
            }
        }
    }
    Ok(tables)
}

async fn discover_tables_by_program_scan_v2(
    rpc_url: &str,
    authorities: &[Pubkey],
    limit: usize,
) -> Result<Vec<Pubkey>, Box<dyn Error>> {
    let http = reqwest::Client::new();
    let mut pagination_key: Option<String> = None;
    let mut tables = Vec::new();

    loop {
        let mut config = json!({
            "encoding": "base64",
            "limit": 1000,
        });
        if let Some(key) = pagination_key.as_ref() {
            config["paginationKey"] = json!(key);
        }
        let response = http
            .post(rpc_url)
            .json(&json!({
                "jsonrpc": "2.0",
                "id": "route-lookup-table-cleanup",
                "method": "getProgramAccountsV2",
                "params": [
                    address_lookup_table_program::id().to_string(),
                    config,
                ],
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
        let account_count = result.accounts.len();
        for account in result.accounts {
            let table_address = Pubkey::from_str(&account.pubkey)?;
            let Some(data) = decode_program_account_data(&account.account.data)? else {
                continue;
            };
            let Ok(table) = AddressLookupTable::deserialize(&data) else {
                continue;
            };
            if table
                .meta
                .authority
                .is_some_and(|authority| authorities.contains(&authority))
            {
                tables.push(table_address);
                if limit > 0 && tables.len() >= limit {
                    return Ok(tables);
                }
            }
        }
        pagination_key = result.pagination_key;
        if pagination_key.is_none() || account_count == 0 {
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
        return Err(format!("unsupported getProgramAccountsV2 account encoding {encoding}").into());
    }
    Ok(Some(BASE64.decode(encoded)?))
}

async fn discover_tables_by_history(
    rpc_url: &str,
    authorities: &[Pubkey],
    options: &Options,
    table_limit: Option<usize>,
    trace: &TraceLog,
) -> Result<Vec<HistoryEvent>, Box<dyn Error>> {
    let http = reqwest::Client::new();
    let mut events = Vec::new();
    let mut seen_tables = BTreeSet::new();
    if table_limit == Some(0) || options.history_limit == 0 {
        return Ok(events);
    }
    for authority in authorities {
        let phase_started = Instant::now();
        let signatures = rpc_call(
            &http,
            rpc_url,
            "getSignaturesForAddress",
            json!([
                authority.to_string(),
                {
                    "limit": options.history_limit,
                }
            ]),
        )
        .await?;
        let Some(signatures) = signatures.as_array() else {
            continue;
        };
        trace.finish(
            "cleanup.scan_history.signatures",
            phase_started,
            json!({
                "authority": authority.to_string(),
                "signatureCount": signatures.len(),
                "requestedLimit": options.history_limit,
            }),
        );
        for entry in signatures {
            if table_limit.is_some_and(|limit| seen_tables.len() >= limit) {
                trace.event(
                    "cleanup.scan_history.limit_reached",
                    json!({ "uniqueTableCount": seen_tables.len() }),
                );
                break;
            }
            let Some(signature) = entry.get("signature").and_then(Value::as_str) else {
                continue;
            };
            let Some(slot) = entry.get("slot").and_then(Value::as_u64) else {
                continue;
            };
            if options.min_slot.is_some_and(|min_slot| slot < min_slot) {
                continue;
            }
            let block_time = entry.get("blockTime").and_then(Value::as_i64);
            let phase_started = Instant::now();
            let transaction = rpc_call(
                &http,
                rpc_url,
                "getTransaction",
                json!([
                    signature,
                    {
                        "encoding": "json",
                        "maxSupportedTransactionVersion": 0,
                    }
                ]),
            )
            .await?;
            let parsed_events = lookup_table_events_from_transaction(
                signature,
                slot,
                block_time,
                &transaction,
                authorities,
            )?;
            for event in parsed_events {
                seen_tables.insert(event.table_address);
                events.push(event);
            }
            trace.finish(
                "cleanup.scan_history.transaction",
                phase_started,
                json!({
                    "signature": signature,
                    "slot": slot,
                    "eventCount": events.len(),
                    "uniqueTableCount": seen_tables.len(),
                }),
            );
        }
    }
    events.sort_by(|left, right| {
        right
            .slot
            .cmp(&left.slot)
            .then_with(|| left.signature.cmp(&right.signature))
            .then_with(|| left.kind.cmp(right.kind))
    });
    events.dedup_by(|left, right| {
        left.signature == right.signature
            && left.kind == right.kind
            && left.table_address == right.table_address
    });
    Ok(events)
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
            "id": "route-lookup-table-cleanup",
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

fn lookup_table_events_from_transaction(
    signature: &str,
    slot: u64,
    block_time: Option<i64>,
    transaction: &Value,
    audited_authorities: &[Pubkey],
) -> Result<Vec<HistoryEvent>, Box<dyn Error>> {
    let account_keys = transaction
        .pointer("/transaction/message/accountKeys")
        .and_then(Value::as_array)
        .map(|values| {
            values
                .iter()
                .filter_map(account_key_from_value)
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    let mut events = Vec::new();
    if let Some(instructions) = transaction
        .pointer("/transaction/message/instructions")
        .and_then(Value::as_array)
    {
        for instruction in instructions {
            if let Some(event) = lookup_table_event_from_instruction(
                signature,
                slot,
                block_time,
                &account_keys,
                instruction,
                audited_authorities,
            )? {
                events.push(event);
            }
        }
    }
    if let Some(inner_groups) = transaction
        .pointer("/meta/innerInstructions")
        .and_then(Value::as_array)
    {
        for group in inner_groups {
            let Some(instructions) = group.get("instructions").and_then(Value::as_array) else {
                continue;
            };
            for instruction in instructions {
                if let Some(event) = lookup_table_event_from_instruction(
                    signature,
                    slot,
                    block_time,
                    &account_keys,
                    instruction,
                    audited_authorities,
                )? {
                    events.push(event);
                }
            }
        }
    }
    Ok(events)
}

fn account_key_from_value(value: &Value) -> Option<String> {
    value.as_str().map(str::to_owned).or_else(|| {
        value
            .get("pubkey")
            .and_then(Value::as_str)
            .map(str::to_owned)
    })
}

fn lookup_table_event_from_instruction(
    signature: &str,
    slot: u64,
    block_time: Option<i64>,
    account_keys: &[String],
    instruction: &Value,
    audited_authorities: &[Pubkey],
) -> Result<Option<HistoryEvent>, Box<dyn Error>> {
    let Some(program_id) = instruction
        .get("programIdIndex")
        .and_then(Value::as_u64)
        .and_then(|index| account_keys.get(index as usize))
    else {
        return Ok(None);
    };
    if program_id != &address_lookup_table_program::id().to_string() {
        return Ok(None);
    }
    let Some(accounts) = instruction.get("accounts").and_then(Value::as_array) else {
        return Ok(None);
    };
    let instruction_accounts = accounts
        .iter()
        .filter_map(Value::as_u64)
        .filter_map(|index| account_keys.get(index as usize))
        .map(|value| Pubkey::from_str(value))
        .collect::<Result<Vec<_>, _>>()?;
    if !instruction_accounts
        .iter()
        .any(|account| audited_authorities.contains(account))
    {
        return Ok(None);
    }
    let Some(table_address) = instruction_accounts.first().copied() else {
        return Ok(None);
    };
    let authority = instruction_accounts.get(1).copied();
    let payer_or_recipient = instruction_accounts.get(2).copied();
    let Some(data) = instruction.get("data").and_then(Value::as_str) else {
        return Ok(None);
    };
    let decoded = bs58::decode(data).into_vec()?;
    let program_instruction =
        bincode::deserialize::<address_lookup_table_instruction::ProgramInstruction>(&decoded)?;
    let (kind, new_address_count) = match program_instruction {
        address_lookup_table_instruction::ProgramInstruction::CreateLookupTable { .. } => {
            ("create", None)
        }
        address_lookup_table_instruction::ProgramInstruction::ExtendLookupTable {
            new_addresses,
        } => ("extend", Some(new_addresses.len())),
        address_lookup_table_instruction::ProgramInstruction::DeactivateLookupTable => {
            ("deactivate", None)
        }
        address_lookup_table_instruction::ProgramInstruction::CloseLookupTable => ("close", None),
        address_lookup_table_instruction::ProgramInstruction::FreezeLookupTable => ("freeze", None),
    };
    Ok(Some(HistoryEvent {
        signature: signature.to_owned(),
        slot,
        block_time,
        kind,
        table_address,
        authority,
        payer_or_recipient,
        new_address_count,
    }))
}

fn history_event_json(event: &HistoryEvent, protected_tables: &BTreeSet<Pubkey>) -> Value {
    json!({
        "signature": event.signature,
        "slot": event.slot,
        "blockTime": event.block_time,
        "kind": event.kind,
        "classification": history_event_classification(event, protected_tables),
        "table": event.table_address.to_string(),
        "authority": event.authority.map(|authority| authority.to_string()),
        "payerOrRecipient": event.payer_or_recipient.map(|value| value.to_string()),
        "newAddressCount": event.new_address_count,
    })
}

fn history_event_classification(
    event: &HistoryEvent,
    protected_tables: &BTreeSet<Pubkey>,
) -> &'static str {
    match event.kind {
        "create" | "extend" if protected_tables.contains(&event.table_address) => {
            "expected_provisioning_protected"
        }
        "create" | "extend" => "unexpected_create_extend",
        "deactivate" => "cleanup_deactivate",
        "close" => "cleanup_close",
        _ => "other_lookup_table_instruction",
    }
}

fn load_candidates(
    rpc: &RpcClient,
    table_addresses: &[Pubkey],
    trace: &TraceLog,
) -> Result<Vec<(Pubkey, Result<Candidate, String>)>, Box<dyn Error>> {
    let mut out = Vec::new();
    for chunk in table_addresses.chunks(100) {
        match rpc.get_multiple_accounts(chunk) {
            Ok(accounts) => {
                for (table_address, account) in chunk.iter().copied().zip(accounts) {
                    let candidate = match account {
                        Some(account) => candidate_from_account(table_address, &account),
                        None => Err(format!("AccountNotFound: pubkey={table_address}")),
                    };
                    out.push((table_address, candidate));
                }
            }
            Err(error) => {
                trace.event(
                    "cleanup.candidate_accounts.batch_fallback",
                    json!({
                        "batchSize": chunk.len(),
                        "error": safe_cleanup_operational_error(&error),
                        "mode": "get_account_per_table",
                    }),
                );
                for table_address in chunk {
                    out.push((
                        *table_address,
                        load_candidate(rpc, *table_address)
                            .map_err(|error| safe_cleanup_operational_error(error.as_ref())),
                    ));
                }
            }
        }
    }
    Ok(out)
}

fn load_candidate(rpc: &RpcClient, table_address: Pubkey) -> Result<Candidate, Box<dyn Error>> {
    let account = rpc.get_account(&table_address)?;
    candidate_from_account(table_address, &account).map_err(Into::into)
}

fn candidate_from_account(table_address: Pubkey, account: &Account) -> Result<Candidate, String> {
    let table = AddressLookupTable::deserialize(&account.data).map_err(|error| {
        format!("failed to deserialize address lookup table {table_address}: {error:?}")
    })?;
    Ok(Candidate {
        table_address,
        lamports: account.lamports,
        owner: account.owner,
        authority: table.meta.authority,
        address_count: table.addresses.len(),
        addresses: table.addresses.to_vec(),
        deactivation_slot: table.meta.deactivation_slot,
        last_extended_slot: table.meta.last_extended_slot,
    })
}

#[cfg(test)]
fn classify_candidate(
    candidate: &Candidate,
    authority_matches: bool,
    protected_reason: Option<&str>,
    current_slot: u64,
) -> (&'static str, String) {
    if candidate.owner != address_lookup_table_program::id() {
        return (
            "skip",
            "account_not_owned_by_address_lookup_table_program".to_owned(),
        );
    }
    if let Some(reason) = protected_reason {
        return ("skip", reason.to_owned());
    }
    if candidate.authority.is_none() {
        return ("skip", "lookup_table_has_no_close_authority".to_owned());
    }
    if !authority_matches {
        return ("skip", "authority_not_in_audited_set".to_owned());
    }
    if candidate.deactivation_slot == u64::MAX {
        return ("deactivate", "active_orphan_table".to_owned());
    }
    if current_slot <= estimate_last_valid_slot(candidate.deactivation_slot) {
        return (
            "defer",
            format!(
                "deactivation_slot_recent_until_at_least_{}",
                estimate_last_valid_slot(candidate.deactivation_slot)
            ),
        );
    }
    (
        "close",
        "deactivated_orphan_table_cooldown_elapsed".to_owned(),
    )
}

#[cfg(test)]
fn classify_registered_candidate(
    candidate: &Candidate,
    protection: &LookupTableCleanupProtection,
    expected_cluster: &str,
    current_slot: u64,
) -> (&'static str, String) {
    let mut drift = Vec::new();
    if protection.cluster != expected_cluster {
        drift.push(format!(
            "cluster_expected_{expected_cluster}_observed_{}",
            protection.cluster
        ));
    }
    if candidate.owner != address_lookup_table_program::id() {
        drift.push("owner_mismatch".to_owned());
    }
    match Pubkey::from_str(&protection.expected_authority) {
        Ok(expected_authority) if candidate.authority == Some(expected_authority) => {}
        Ok(_) => drift.push("authority_mismatch".to_owned()),
        Err(_) => drift.push("invalid_database_authority".to_owned()),
    }
    if i32::try_from(candidate.address_count).ok() != Some(protection.address_count) {
        drift.push("address_count_mismatch".to_owned());
    }
    if ordered_candidate_address_hash(&candidate.addresses) != protection.address_hash {
        drift.push("address_prefix_or_order_hash_mismatch".to_owned());
    }
    let chain_is_active = candidate.deactivation_slot == u64::MAX;
    match protection.desired_state {
        LookupTableLifecycle::Active
        | LookupTableLifecycle::Standby
        | LookupTableLifecycle::Retiring
            if !chain_is_active =>
        {
            drift.push("database_active_chain_deactivated".to_owned());
        }
        LookupTableLifecycle::Deactivated if chain_is_active => {
            drift.push("database_deactivated_chain_active".to_owned());
        }
        LookupTableLifecycle::Closed => {
            drift.push("database_closed_chain_account_exists".to_owned());
        }
        _ => {}
    }
    if !drift.is_empty() {
        return (
            "skip",
            format!("registered_database_chain_drift: {}", drift.join(",")),
        );
    }
    if protection.can_deactivate {
        return (
            "deactivate",
            "registered_retiring_table_has_zero_protected_references".to_owned(),
        );
    }
    if protection.can_close {
        if current_slot <= estimate_last_valid_slot(candidate.deactivation_slot) {
            return (
                "defer",
                format!(
                    "registered_table_cooldown_until_at_least_{}",
                    estimate_last_valid_slot(candidate.deactivation_slot)
                ),
            );
        }
        return (
            "close",
            "registered_deactivated_table_cooldown_elapsed".to_owned(),
        );
    }
    (
        "skip",
        format!(
            "registered_table_protected: {}",
            protection.protection_reasons.join(",")
        ),
    )
}

fn ordered_candidate_address_hash(addresses: &[Pubkey]) -> String {
    let mut hasher = Sha256::new();
    for address in addresses {
        let address = address.to_string();
        hasher.update((address.len() as u64).to_le_bytes());
        hasher.update(address.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn approved_policy_fleet_hash(candidates: &[&Candidate]) -> String {
    let mut candidates = candidates.to_vec();
    candidates.sort_by_key(|candidate| candidate.table_address);
    let mut parts = vec!["legacy-alt-policy-fleet-v1".to_owned()];
    for candidate in candidates {
        parts.extend([
            candidate.table_address.to_string(),
            candidate
                .authority
                .map_or_else(String::new, |value| value.to_string()),
            candidate.address_count.to_string(),
            ordered_candidate_address_hash(&candidate.addresses),
        ]);
    }
    let mut hasher = Sha256::new();
    for part in parts {
        hasher.update((part.len() as u64).to_le_bytes());
        hasher.update(part.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn cleanup_protection_json(protection: &LookupTableCleanupProtection) -> Value {
    json!({
        "cluster": protection.cluster,
        "tableId": protection.table_id,
        "familyId": protection.family_id,
        "expectedAuthority": protection.expected_authority,
        "addressCount": protection.address_count,
        "addressHash": protection.address_hash,
        "mutationEpoch": protection.mutation_epoch,
        "desiredState": protection.desired_state.as_str(),
        "acceptingAllocations": protection.accepting_allocations,
        "canDeactivate": protection.can_deactivate,
        "canClose": protection.can_close,
        "protectionReasons": protection.protection_reasons,
    })
}

fn legacy_cleanup_protection_json(protection: &LegacyLookupTableCleanupProtection) -> Value {
    json!({
        "cluster": protection.cluster,
        "tableId": protection.table_id,
        "importRunId": protection.import_run_id,
        "legacyKind": protection.legacy_kind.map(|kind| kind.as_str()),
        "status": protection.status,
        "durable": protection.durable,
        "expectedAuthority": protection.expected_authority,
        "addressCount": protection.address_count,
        "addressHash": protection.address_hash,
        "lastVerifiedSlot": protection.last_verified_slot,
        "zeroReference": protection.zero_reference,
        "nonselectable": protection.nonselectable,
        "canDeactivate": protection.can_deactivate,
        "canClose": protection.can_close,
        "authorizationToken": protection.authorization_token,
        "protectionReasons": protection.protection_reasons,
    })
}

fn classify_imported_legacy_candidate(
    candidate: &Candidate,
    protection: &LegacyLookupTableCleanupProtection,
    current_slot: u64,
) -> (&'static str, String) {
    let identity_matches = protection.family_id.is_none()
        && protection.import_run_id.is_some()
        && protection.expected_authority
            == candidate
                .authority
                .map_or_else(String::new, |authority| authority.to_string())
        && usize::try_from(protection.address_count).ok() == Some(candidate.address_count)
        && protection.address_hash == ordered_candidate_address_hash(&candidate.addresses)
        && protection.ordered_addresses
            == candidate
                .addresses
                .iter()
                .map(ToString::to_string)
                .collect::<Vec<_>>();
    if !identity_matches {
        return ("skip", "legacy_import_chain_identity_drift".to_owned());
    }
    if protection.can_deactivate && candidate.deactivation_slot == u64::MAX {
        return (
            "deactivate",
            "retired_imported_legacy_table_has_zero_references".to_owned(),
        );
    }
    if protection.can_close && candidate.deactivation_slot != u64::MAX {
        let close_after = estimate_last_valid_slot(candidate.deactivation_slot);
        if current_slot <= close_after {
            return (
                "defer",
                format!("retired_imported_legacy_table_cooldown_until_{close_after}"),
            );
        }
        return (
            "close",
            "retired_imported_legacy_table_cooldown_elapsed".to_owned(),
        );
    }
    (
        "skip",
        format!(
            "legacy_cleanup_not_authorized:{}",
            protection.protection_reasons.join(",")
        ),
    )
}

fn lookup_table_status(candidate: &Candidate, current_slot: u64) -> &'static str {
    if candidate.deactivation_slot == u64::MAX {
        "active"
    } else if current_slot <= estimate_last_valid_slot(candidate.deactivation_slot) {
        "deactivating"
    } else {
        "deactivated"
    }
}

fn load_authority_signer(options: &Options) -> Result<Box<dyn Signer>, Box<dyn Error>> {
    let env_name = options
        .authority_key_env
        .as_deref()
        .unwrap_or(POLICY_KEYPAIR_ENV);
    let value = env::var(env_name).map_err(|_| format!("{env_name} must be set for --execute"))?;
    Ok(Box::new(keypair_from_string(&value)?))
}

async fn execute_planned_cleanups(
    database: &NeonSqlClient,
    rpc: &RpcClient,
    options: &Options,
    signer: &dyn Signer,
    planned_cleanups: &[PlannedCleanup],
    rows: &mut [Value],
) -> Result<u64, Box<dyn Error>> {
    let mut total_reclaimed = 0_u64;
    preflight_cleanup_batches_fit_packet(rpc, signer, planned_cleanups, options.bundle_size)?;
    for (batch_index, batch) in planned_cleanups.chunks(options.bundle_size).enumerate() {
        let cleanup = batch
            .first()
            .filter(|_| batch.len() == 1)
            .ok_or("legacy cleanup execute requires exactly one instruction per fenced batch")?;
        let operation_kind = match cleanup.kind {
            "deactivate_lookup_table" => LookupTableOperationKind::Deactivate,
            "close_lookup_table" => LookupTableOperationKind::Close,
            other => return Err(format!("unsupported cleanup kind {other}").into()),
        };
        let authorization = database
            .begin_legacy_lookup_table_cleanup_authorization(
                &options.cluster,
                &cleanup.table_address.to_string(),
                &cleanup.expected_cleanup_authorization_token,
                operation_kind,
            )
            .await?;
        let candidate = load_candidate(rpc, cleanup.table_address)?;
        if authorization.protection().expected_authority
            != candidate
                .authority
                .map_or_else(String::new, |authority| authority.to_string())
            || usize::try_from(authorization.protection().address_count)? != candidate.address_count
            || authorization.protection().address_hash
                != ordered_candidate_address_hash(&candidate.addresses)
        {
            return Err(
                "legacy ALT chain identity changed after database authorization fencing".into(),
            );
        }
        revalidate_cleanup_chain_evidence(rpc, cleanup)?;
        let instructions = batch
            .iter()
            .map(|cleanup| cleanup.instruction.clone())
            .collect::<Vec<_>>();
        let expected_refund_lamports = batch
            .iter()
            .map(|cleanup| cleanup.reclaimed_lamports)
            .sum::<u64>();
        let close_recipient = batch.iter().find_map(|cleanup| cleanup.recipient);
        if batch
            .iter()
            .filter_map(|cleanup| cleanup.recipient)
            .any(|recipient| Some(recipient) != close_recipient)
        {
            return Err("cleanup batch contains multiple close recipients".into());
        }
        let result = send_cleanup_instruction_batch(
            rpc,
            signer,
            &instructions,
            close_recipient,
            expected_refund_lamports,
        )?;
        let mut execution = json!({
            "signature": result.signature.clone(),
            "kind": cleanup.kind,
            "batchIndex": batch_index,
            "batchInstructionIndex": 0,
            "batchSize": 1,
            "transaction": cleanup_transaction_packet_json(&result.transaction_packet),
            "finalized": true,
            "estimatedFeeLamports": result.estimated_fee_lamports.to_string(),
            "recipientBalanceBefore": result.recipient_balance_before.map(|value| value.to_string()),
            "recipientBalanceAfter": result.recipient_balance_after.map(|value| value.to_string()),
            "expectedBatchRefundLamports": result.expected_refund_lamports.to_string(),
            "minimumNetRecipientIncreaseLamports": result.minimum_net_recipient_increase_lamports.to_string(),
            "refundProven": result.expected_refund_lamports == 0 || result.recipient_balance_after.is_some(),
            "simulation": result.simulation.clone(),
        });
        let (observed_slot, close_recipient, reclaimed_lamports) =
            if let Some(recipient) = cleanup.recipient {
                let post_close = rpc.get_account_with_commitment(
                    &cleanup.table_address,
                    CommitmentConfig::finalized(),
                )?;
                if post_close.value.is_some() {
                    return Err(format!(
                        "closed ALT {} still exists at finalized commitment",
                        cleanup.table_address
                    )
                    .into());
                }
                total_reclaimed = total_reclaimed.saturating_add(cleanup.reclaimed_lamports);
                execution["recipient"] = json!(recipient.to_string());
                execution["reclaimedLamports"] = json!(cleanup.reclaimed_lamports.to_string());
                (
                    i64::try_from(rpc.get_slot_with_commitment(CommitmentConfig::finalized())?)?,
                    Some(recipient.to_string()),
                    Some(i64::try_from(cleanup.reclaimed_lamports)?),
                )
            } else {
                let reloaded = load_candidate(rpc, cleanup.table_address)?;
                if reloaded.deactivation_slot == u64::MAX {
                    return Err(format!(
                        "deactivated ALT {} remains active at finalized commitment",
                        cleanup.table_address
                    )
                    .into());
                }
                execution["actualDeactivationSlot"] = json!(reloaded.deactivation_slot.to_string());
                (i64::try_from(reloaded.deactivation_slot)?, None, None)
            };
        authorization
            .record_finalized(VerifiedLegacyLookupTableCleanup {
                cluster: options.cluster.clone(),
                table_address: cleanup.table_address.to_string(),
                expected_authorization_token: cleanup.expected_cleanup_authorization_token.clone(),
                operation_kind,
                transaction_signature: result.signature.clone(),
                observed_slot,
                close_recipient,
                reclaimed_lamports,
            })
            .await?;
        execution["databaseRecord"] = json!({ "status": "fenced_recorded" });
        set_candidate_execution(rows, cleanup.row_index, execution)?;
    }
    Ok(total_reclaimed)
}

fn preflight_cleanup_batches_fit_packet(
    rpc: &RpcClient,
    signer: &dyn Signer,
    planned_cleanups: &[PlannedCleanup],
    bundle_size: usize,
) -> Result<(), Box<dyn Error>> {
    let blockhash = rpc.get_latest_blockhash()?;
    for (batch_index, batch) in planned_cleanups.chunks(bundle_size).enumerate() {
        let instructions = batch
            .iter()
            .map(|cleanup| cleanup.instruction.clone())
            .collect::<Vec<_>>();
        let transaction = Transaction::new_signed_with_payer(
            &instructions,
            Some(&signer.pubkey()),
            &[signer],
            blockhash,
        );
        ensure_cleanup_transaction_fits_packet(&transaction).map_err(|error| {
            format!(
                "cleanup batch {batch_index} with {} instruction(s) is too large: {error}",
                instructions.len()
            )
        })?;
    }
    Ok(())
}

fn set_candidate_execution(
    rows: &mut [Value],
    row_index: usize,
    execution: Value,
) -> Result<(), Box<dyn Error>> {
    let row = rows
        .get_mut(row_index)
        .ok_or_else(|| format!("cleanup row index {row_index} was not found"))?;
    let row = row
        .as_object_mut()
        .ok_or_else(|| format!("cleanup row index {row_index} was not a JSON object"))?;
    row.insert("execution".to_owned(), execution);
    Ok(())
}

fn send_cleanup_instruction_batch(
    rpc: &RpcClient,
    signer: &dyn Signer,
    instructions: &[Instruction],
    close_recipient: Option<Pubkey>,
    expected_refund_lamports: u64,
) -> Result<CleanupTransactionResult, Box<dyn Error>> {
    let blockhash = rpc.get_latest_blockhash()?;
    let transaction = Transaction::new_signed_with_payer(
        instructions,
        Some(&signer.pubkey()),
        &[signer],
        blockhash,
    );
    let transaction_packet = ensure_cleanup_transaction_fits_packet(&transaction)?;
    let simulation = simulate_cleanup_transaction(rpc, &transaction)?;
    let estimated_fee_lamports = rpc.get_fee_for_message(&transaction.message)?;
    let recipient_balance_before = close_recipient
        .map(|recipient| {
            rpc.get_balance_with_commitment(&recipient, CommitmentConfig::finalized())
                .map(|response| response.value)
        })
        .transpose()?;
    let signature = rpc.send_and_confirm_transaction_with_spinner_and_commitment(
        &transaction,
        CommitmentConfig::finalized(),
    )?;
    require_finalized_signature(rpc, &signature)?;
    let recipient_balance_after = close_recipient
        .map(|recipient| {
            rpc.get_balance_with_commitment(&recipient, CommitmentConfig::finalized())
                .map(|response| response.value)
        })
        .transpose()?;
    let minimum_net_recipient_increase_lamports =
        expected_refund_lamports.saturating_sub(estimated_fee_lamports);
    if let (Some(before), Some(after)) = (recipient_balance_before, recipient_balance_after) {
        let minimum_after = before.saturating_add(minimum_net_recipient_increase_lamports);
        if after < minimum_after {
            return Err(format!(
                "finalized policy recipient balance did not prove the ALT refund: before={before}, after={after}, expected_refund={expected_refund_lamports}, estimated_fee={estimated_fee_lamports}"
            )
            .into());
        }
    }
    Ok(CleanupTransactionResult {
        signature: signature.to_string(),
        simulation,
        transaction_packet,
        estimated_fee_lamports,
        recipient_balance_before,
        recipient_balance_after,
        expected_refund_lamports,
        minimum_net_recipient_increase_lamports,
    })
}

fn require_finalized_signature(
    rpc: &RpcClient,
    signature: &Signature,
) -> Result<(), Box<dyn Error>> {
    let status = rpc
        .get_signature_statuses_with_history(&[*signature])?
        .value
        .into_iter()
        .next()
        .flatten()
        .ok_or("cleanup signature was not found after finalized confirmation")?;
    if let Some(error) = status.err {
        return Err(format!("cleanup transaction finalized with error: {error:?}").into());
    }
    if !status.satisfies_commitment(CommitmentConfig::finalized()) {
        return Err("cleanup transaction did not reach finalized commitment".into());
    }
    Ok(())
}

fn revalidate_cleanup_chain_evidence(
    rpc: &RpcClient,
    cleanup: &PlannedCleanup,
) -> Result<(), Box<dyn Error>> {
    let candidate = load_candidate(rpc, cleanup.table_address)?;
    if candidate.owner != address_lookup_table_program::id()
        || candidate.authority != Some(cleanup.expected_authority)
        || candidate.address_count != cleanup.expected_address_count
        || ordered_candidate_address_hash(&candidate.addresses) != cleanup.expected_address_hash
    {
        return Err(format!(
            "ALT {} chain evidence changed immediately before cleanup mutation",
            cleanup.table_address
        )
        .into());
    }
    match cleanup.kind {
        "deactivate_lookup_table" if candidate.deactivation_slot != u64::MAX => {
            Err("ALT is no longer active immediately before deactivation".into())
        }
        "close_lookup_table" if candidate.deactivation_slot == u64::MAX => {
            Err("ALT is active immediately before close".into())
        }
        "close_lookup_table"
            if rpc.get_slot_with_commitment(CommitmentConfig::finalized())?
                <= estimate_last_valid_slot(candidate.deactivation_slot) =>
        {
            Err("ALT deactivation cooldown is not finalized immediately before close".into())
        }
        _ => Ok(()),
    }
}

fn ensure_cleanup_transaction_fits_packet(
    transaction: &Transaction,
) -> Result<CleanupTransactionPacket, Box<dyn Error>> {
    let packet_size_bytes = bincode::serialize(transaction)?.len();
    let packet = CleanupTransactionPacket {
        packet_size_bytes,
        packet_data_size_bytes: PACKET_DATA_SIZE,
    };
    if packet_size_bytes > PACKET_DATA_SIZE {
        return Err(format!(
            "serialized cleanup transaction is {packet_size_bytes} bytes; Solana packet limit is {PACKET_DATA_SIZE} bytes"
        )
        .into());
    }
    Ok(packet)
}

fn cleanup_transaction_packet_json(packet: &CleanupTransactionPacket) -> Value {
    json!({
        "packetSizeBytes": packet.packet_size_bytes,
        "packetDataSizeBytes": packet.packet_data_size_bytes,
        "fitsPacketDataSize": packet.packet_size_bytes <= packet.packet_data_size_bytes,
    })
}

fn simulate_cleanup_transaction(
    rpc: &RpcClient,
    transaction: &Transaction,
) -> Result<Value, Box<dyn Error>> {
    let simulation = rpc.simulate_transaction(transaction)?;
    let logs = simulation.value.logs.clone().unwrap_or_default();
    if let Some(error) = simulation.value.err.as_ref() {
        return Err(format!(
            "cleanup transaction simulation failed: {error:?}; logs: {}",
            logs.join(" | ")
        )
        .into());
    }
    Ok(json!({
        "err": simulation.value.err.as_ref().map(|error| format!("{error:?}")),
        "logs": logs,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn alt_cleanup_caught_rpc_errors_never_expose_endpoint_credentials() {
        let safe = safe_cleanup_operational_error_with_context(
            "get_multiple_accounts_failed",
            &"HTTP 429 from https://user:password@example.test/private/path?api-key=query-secret access_token=header-secret",
        );

        assert!(safe.starts_with("get_multiple_accounts_failed:"));
        assert!(safe.contains("HTTP 429"));
        assert!(safe.len() <= 512);
        for secret in [
            "user",
            "password",
            "private/path",
            "api-key",
            "query-secret",
            "access_token",
            "header-secret",
        ] {
            assert!(
                !safe.contains(secret),
                "cleanup error leaked {secret}: {safe}"
            );
        }
    }

    fn candidate(authority: Pubkey, deactivation_slot: u64) -> Candidate {
        Candidate {
            table_address: Pubkey::new_unique(),
            lamports: 1_234_567,
            owner: address_lookup_table_program::id(),
            authority: Some(authority),
            address_count: 3,
            addresses: vec![
                Pubkey::new_unique(),
                Pubkey::new_unique(),
                Pubkey::new_unique(),
            ],
            deactivation_slot,
            last_extended_slot: 0,
        }
    }

    fn registered_protection(
        candidate: &Candidate,
        desired_state: LookupTableLifecycle,
        can_deactivate: bool,
        can_close: bool,
        protection_reasons: Vec<String>,
    ) -> LookupTableCleanupProtection {
        LookupTableCleanupProtection {
            cluster: "localnet".to_owned(),
            table_id: 7,
            family_id: 3,
            table_address: candidate.table_address.to_string(),
            expected_authority: candidate.authority.unwrap().to_string(),
            address_count: candidate.address_count as i32,
            address_hash: ordered_candidate_address_hash(&candidate.addresses),
            mutation_epoch: 11,
            desired_state,
            accepting_allocations: false,
            can_deactivate,
            can_close,
            protection_reasons,
        }
    }

    #[test]
    fn registered_cleanup_deactivates_only_after_zero_reference_readback() {
        let candidate = candidate(Pubkey::new_unique(), u64::MAX);
        let protection = registered_protection(
            &candidate,
            LookupTableLifecycle::Retiring,
            true,
            false,
            Vec::new(),
        );

        let (action, reason) =
            classify_registered_candidate(&candidate, &protection, "localnet", 100);

        assert_eq!(action, "deactivate");
        assert_eq!(
            reason,
            "registered_retiring_table_has_zero_protected_references"
        );
    }

    #[test]
    fn registered_cleanup_skips_live_binding() {
        let candidate = candidate(Pubkey::new_unique(), u64::MAX);
        let protection = registered_protection(
            &candidate,
            LookupTableLifecycle::Retiring,
            false,
            false,
            vec!["live_binding".to_owned()],
        );

        let (action, reason) =
            classify_registered_candidate(&candidate, &protection, "localnet", 100);

        assert_eq!(action, "skip");
        assert_eq!(reason, "registered_table_protected: live_binding");
    }

    #[test]
    fn registered_cleanup_fails_closed_on_authority_or_prefix_drift() {
        let mut candidate = candidate(Pubkey::new_unique(), u64::MAX);
        let protection = registered_protection(
            &candidate,
            LookupTableLifecycle::Retiring,
            true,
            false,
            Vec::new(),
        );
        candidate.authority = Some(Pubkey::new_unique());
        candidate.addresses.reverse();

        let (action, reason) =
            classify_registered_candidate(&candidate, &protection, "localnet", 100);

        assert_eq!(action, "skip");
        assert!(reason.contains("authority_mismatch"));
        assert!(reason.contains("address_prefix_or_order_hash_mismatch"));
    }

    #[test]
    fn registered_cleanup_waits_for_cooldown_before_close() {
        let deactivation_slot = 10;
        let candidate = candidate(Pubkey::new_unique(), deactivation_slot);
        let protection = registered_protection(
            &candidate,
            LookupTableLifecycle::Deactivated,
            false,
            true,
            Vec::new(),
        );

        let (defer_action, _) = classify_registered_candidate(
            &candidate,
            &protection,
            "localnet",
            estimate_last_valid_slot(deactivation_slot),
        );
        let (close_action, _) = classify_registered_candidate(
            &candidate,
            &protection,
            "localnet",
            estimate_last_valid_slot(deactivation_slot) + 1,
        );

        assert_eq!(defer_action, "defer");
        assert_eq!(close_action, "close");
    }

    #[test]
    fn alt_cleanup_classifies_active_audited_table_for_deactivation() {
        let authority = Pubkey::new_unique();
        let candidate = candidate(authority, u64::MAX);

        let (action, reason) = classify_candidate(&candidate, true, None, 100);

        assert_eq!(action, "deactivate");
        assert_eq!(reason, "active_orphan_table");
    }

    #[test]
    fn alt_cleanup_skips_protected_table() {
        let authority = Pubkey::new_unique();
        let candidate = candidate(authority, u64::MAX);

        let (action, reason) = classify_candidate(
            &candidate,
            true,
            Some("durable_registry_env_or_allowlist"),
            100,
        );

        assert_eq!(action, "skip");
        assert_eq!(reason, "durable_registry_env_or_allowlist");
    }

    #[test]
    fn alt_cleanup_classifies_elapsed_deactivated_table_for_close() {
        let authority = Pubkey::new_unique();
        let deactivation_slot = 10;
        let current_slot = estimate_last_valid_slot(deactivation_slot) + 1;
        let candidate = candidate(authority, deactivation_slot);

        let (action, reason) = classify_candidate(&candidate, true, None, current_slot);

        assert_eq!(action, "close");
        assert_eq!(reason, "deactivated_orphan_table_cooldown_elapsed");
        assert_eq!(lookup_table_status(&candidate, current_slot), "deactivated");
    }

    #[test]
    fn alt_cleanup_defers_recently_deactivated_table() {
        let authority = Pubkey::new_unique();
        let deactivation_slot = 10;
        let current_slot = estimate_last_valid_slot(deactivation_slot);
        let candidate = candidate(authority, deactivation_slot);

        let (action, reason) = classify_candidate(&candidate, true, None, current_slot);

        assert_eq!(action, "defer");
        assert!(reason.starts_with("deactivation_slot_recent_until_at_least_"));
        assert_eq!(
            lookup_table_status(&candidate, current_slot),
            "deactivating"
        );
    }

    #[test]
    fn alt_cleanup_skips_authority_mismatch() {
        let authority = Pubkey::new_unique();
        let candidate = candidate(authority, u64::MAX);

        let (action, reason) = classify_candidate(&candidate, false, None, 100);

        assert_eq!(action, "skip");
        assert_eq!(reason, "authority_not_in_audited_set");
    }

    #[test]
    fn alt_cleanup_redacts_every_credential_bearing_rpc_url_component() {
        assert_eq!(
            redacted_rpc_endpoint(
                "https://user:password@mainnet.helius-rpc.com/private/path?api-key=secret"
            ),
            "https://mainnet.helius-rpc.com"
        );
        assert_eq!(
            redacted_rpc_endpoint("https://example.quiknode.pro/path-token/"),
            "https://example.quiknode.pro"
        );
        assert_eq!(
            redacted_rpc_endpoint("http://localhost:8899"),
            "http://localhost:8899"
        );
    }

    #[test]
    fn alt_cleanup_every_mode_requires_an_explicit_rpc_endpoint() {
        let dry_run_error =
            parse_args_with_env(vec!["--cluster".to_owned(), "localnet".to_owned()], |_| {
                None
            })
            .expect_err("dry-run must not inherit an implicit mainnet endpoint");
        assert!(dry_run_error
            .to_string()
            .contains("required for every cleanup mode"));

        let error = parse_args_with_env(
            vec![
                "--cluster".to_owned(),
                "localnet".to_owned(),
                "--execute".to_owned(),
            ],
            |_| None,
        )
        .expect_err("execute must not inherit the implicit mainnet endpoint");

        assert!(error
            .to_string()
            .contains("required for every cleanup mode"));

        let blank_error = parse_args_with_env(
            vec![
                "--cluster".to_owned(),
                "localnet".to_owned(),
                "--execute".to_owned(),
                "--rpc-url".to_owned(),
                " ".to_owned(),
            ],
            |name| (name == "SOLANA_RPC_URL").then(|| "http://localhost:8899".to_owned()),
        )
        .expect_err("blank CLI RPC must not fall back to the environment");
        assert!(blank_error
            .to_string()
            .contains("required for every cleanup mode"));
    }

    #[test]
    fn alt_cleanup_parses_execute_safety_options() {
        let options = parse_args(vec![
            "--cluster".to_owned(),
            "localnet".to_owned(),
            "--rpc-url".to_owned(),
            "http://localhost:8899".to_owned(),
            "--execute".to_owned(),
            "--expected-fleet-count".to_owned(),
            "31".to_owned(),
            "--expected-fleet-hash".to_owned(),
            "a".repeat(64),
            "--bundle-size".to_owned(),
            "1".to_owned(),
            "--trace-timing".to_owned(),
        ])
        .expect("cleanup safety options should parse");

        assert!(options.execute);
        assert!(options.simulate_before_submit);
        assert!(options.scan_program_accounts);
        assert!(options.scan_history);
        assert_eq!(options.limit, 0);
        assert_eq!(options.expected_fleet_count, Some(31));
        assert_eq!(options.bundle_size, 1);
        assert!(options.trace_timing);
    }

    #[test]
    fn alt_cleanup_disables_trace_timing_by_default() {
        let options = parse_args(vec![
            "--cluster".to_owned(),
            "localnet".to_owned(),
            "--rpc-url".to_owned(),
            "http://localhost:8899".to_owned(),
        ])
        .expect("default cleanup options parse");

        assert!(!options.trace_timing);
    }

    #[test]
    fn alt_cleanup_rejects_zero_bundle_size() {
        let error = parse_args(vec![
            "--cluster".to_owned(),
            "localnet".to_owned(),
            "--bundle-size".to_owned(),
            "0".to_owned(),
        ])
        .expect_err("zero bundle size should be rejected");

        assert_eq!(error.to_string(), "--bundle-size must be at least 1");
    }

    #[test]
    fn alt_cleanup_rejects_unfenced_execute_batching() {
        let error = parse_args(vec![
            "--cluster".to_owned(),
            "localnet".to_owned(),
            "--rpc-url".to_owned(),
            "http://localhost:8899".to_owned(),
            "--execute".to_owned(),
            "--expected-fleet-count".to_owned(),
            "31".to_owned(),
            "--expected-fleet-hash".to_owned(),
            "a".repeat(64),
            "--bundle-size".to_owned(),
            "2".to_owned(),
        ])
        .expect_err("multi-table execute cannot share one legacy authorization fence");

        assert!(error.to_string().contains("database authorization fence"));
    }

    #[test]
    fn alt_cleanup_packet_guard_rejects_oversized_batch() {
        let authority = solana_sdk::signature::Keypair::new();
        let recipient = Pubkey::new_unique();
        let instructions = (0..128)
            .map(|_| {
                address_lookup_table_instruction::close_lookup_table(
                    Pubkey::new_unique(),
                    authority.pubkey(),
                    recipient,
                )
            })
            .collect::<Vec<_>>();
        let transaction = Transaction::new_signed_with_payer(
            &instructions,
            Some(&authority.pubkey()),
            &[&authority],
            solana_sdk::hash::Hash::new_unique(),
        );

        let error = ensure_cleanup_transaction_fits_packet(&transaction)
            .expect_err("oversized cleanup transaction should be rejected");

        assert!(error.to_string().contains("Solana packet limit"));
    }

    #[test]
    fn alt_cleanup_decodes_create_history_instruction() {
        let authority = Pubkey::new_unique();
        let payer = Pubkey::new_unique();
        let (instruction, lookup_table) =
            address_lookup_table_instruction::create_lookup_table(authority, payer, 123);
        let account_keys = account_keys_for_instruction(&instruction);
        let instruction_json = compiled_instruction_json(&instruction, &account_keys);

        let event = lookup_table_event_from_instruction(
            "signature",
            456,
            Some(789),
            &account_keys,
            &instruction_json,
            &[authority],
        )
        .expect("history parser should decode create")
        .expect("create instruction should produce an event");

        assert_eq!(event.kind, "create");
        assert_eq!(event.table_address, lookup_table);
        assert_eq!(event.authority, Some(authority));
        assert_eq!(event.payer_or_recipient, Some(payer));
        assert_eq!(event.slot, 456);
        assert_eq!(event.block_time, Some(789));
    }

    #[test]
    fn alt_cleanup_decodes_extend_history_instruction() {
        let lookup_table = Pubkey::new_unique();
        let authority = Pubkey::new_unique();
        let payer = Pubkey::new_unique();
        let new_addresses = vec![Pubkey::new_unique(), Pubkey::new_unique()];
        let instruction = address_lookup_table_instruction::extend_lookup_table(
            lookup_table,
            authority,
            Some(payer),
            new_addresses,
        );
        let account_keys = account_keys_for_instruction(&instruction);
        let instruction_json = compiled_instruction_json(&instruction, &account_keys);

        let event = lookup_table_event_from_instruction(
            "signature",
            456,
            None,
            &account_keys,
            &instruction_json,
            &[authority],
        )
        .expect("history parser should decode extend")
        .expect("extend instruction should produce an event");

        assert_eq!(event.kind, "extend");
        assert_eq!(event.table_address, lookup_table);
        assert_eq!(event.authority, Some(authority));
        assert_eq!(event.payer_or_recipient, Some(payer));
        assert_eq!(event.new_address_count, Some(2));
    }

    #[test]
    fn alt_cleanup_history_ignores_unaudited_instruction() {
        let audited_authority = Pubkey::new_unique();
        let other_authority = Pubkey::new_unique();
        let payer = Pubkey::new_unique();
        let (instruction, _) =
            address_lookup_table_instruction::create_lookup_table(other_authority, payer, 123);
        let account_keys = account_keys_for_instruction(&instruction);
        let instruction_json = compiled_instruction_json(&instruction, &account_keys);

        let event = lookup_table_event_from_instruction(
            "signature",
            456,
            None,
            &account_keys,
            &instruction_json,
            &[audited_authority],
        )
        .expect("history parser should decode instruction data");

        assert!(event.is_none());
    }

    #[test]
    fn alt_cleanup_classifies_history_create_extend_events() {
        let table_address = Pubkey::new_unique();
        let event = HistoryEvent {
            signature: "signature".to_owned(),
            slot: 1,
            block_time: None,
            kind: "create",
            table_address,
            authority: None,
            payer_or_recipient: None,
            new_address_count: None,
        };
        let mut protected = BTreeSet::new();

        assert_eq!(
            history_event_classification(&event, &protected),
            "unexpected_create_extend"
        );

        protected.insert(table_address);
        assert_eq!(
            history_event_classification(&event, &protected),
            "expected_provisioning_protected"
        );
    }

    fn account_keys_for_instruction(
        instruction: &solana_sdk::instruction::Instruction,
    ) -> Vec<String> {
        let mut keys = instruction
            .accounts
            .iter()
            .map(|meta| meta.pubkey.to_string())
            .collect::<Vec<_>>();
        keys.push(instruction.program_id.to_string());
        keys.sort();
        keys.dedup();
        keys
    }

    fn compiled_instruction_json(
        instruction: &solana_sdk::instruction::Instruction,
        account_keys: &[String],
    ) -> Value {
        let program_id_index = account_keys
            .iter()
            .position(|key| key == &instruction.program_id.to_string())
            .expect("program id should be present");
        let accounts = instruction
            .accounts
            .iter()
            .map(|meta| {
                account_keys
                    .iter()
                    .position(|key| key == &meta.pubkey.to_string())
                    .expect("instruction account should be present")
            })
            .collect::<Vec<_>>();
        json!({
            "programIdIndex": program_id_index,
            "accounts": accounts,
            "data": bs58::encode(&instruction.data).into_string(),
        })
    }
}
