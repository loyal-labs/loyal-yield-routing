use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    str::FromStr,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use loyal_yield_orchestrator::{
    keypair_from_string,
    rpc_safety::{
        redacted_external_error, redacted_rpc_endpoint, validate_rpc_endpoint,
        validate_rpc_genesis_hash,
    },
    FinalizedLegacyLookupTableCleanupAttempt, ImportedLegacyLookupTableCleanupRecord,
    LegacyLookupTableCleanupAttemptPrepare, LegacyLookupTableCleanupAttemptRecord,
    LegacyLookupTableCleanupAttemptState, LegacyLookupTableCleanupBudgetReservation,
    LegacyLookupTableCleanupProtection, LookupTableCleanupProtection,
    LookupTableClusterBudgetPolicy, LookupTableLifecycle, LookupTableOperationEnqueue,
    LookupTableOperationKind, LookupTableOperationStatus, NeonSqlClient, NeonSqlConfig,
    ReusableLookupTableRecord, SignedLegacyLookupTableCleanupAttempt, POLICY_KEYPAIR_ENV,
};
use serde::Deserialize;
use serde_json::{json, Map, Value};
use sha2::{Digest, Sha256};
use solana_client::{
    client_error::{ClientError, ClientErrorKind},
    rpc_client::RpcClient,
    rpc_config::{RpcAccountInfoConfig, RpcSendTransactionConfig, RpcSimulateTransactionConfig},
    rpc_custom_error::JSON_RPC_SERVER_ERROR_MIN_CONTEXT_SLOT_NOT_REACHED,
    rpc_request::RpcError as SolanaRpcError,
};
use solana_sdk::address_lookup_table::{
    instruction as address_lookup_table_instruction, program as address_lookup_table_program,
    state::{estimate_last_valid_slot, AddressLookupTable},
};
use solana_sdk::{
    account::Account,
    commitment_config::{CommitmentConfig, CommitmentLevel},
    instruction::Instruction,
    message::Message,
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
const MIN_CONTEXT_SLOT_MAX_ATTEMPTS: usize = 8;
const MIN_CONTEXT_SLOT_RETRY_DELAY: Duration = Duration::from_millis(250);

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
    max_lamports: Option<i64>,
    budget_window_seconds: Option<i64>,
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

#[derive(Clone, Debug)]
struct AuthorityHistoryEvidence {
    authority: Pubkey,
    page_count: usize,
    signature_count: usize,
    oldest_slot: Option<u64>,
    boundary_reached: bool,
    exhausted: bool,
}

#[derive(Clone, Debug)]
struct HistoryScanEvidence {
    events: Vec<HistoryEvent>,
    authorities: Vec<AuthorityHistoryEvidence>,
    mutation_set_hash: String,
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
struct PlannedRegisteredCleanup {
    row_index: usize,
    protection: LookupTableCleanupProtection,
    operation_kind: LookupTableOperationKind,
}

#[derive(Debug, Default)]
struct RegisteredCleanupEnqueueSummary {
    operation_count: usize,
    queued_count: usize,
}

#[derive(Debug)]
struct CleanupTransactionResult {
    attempt_id: i64,
    signature: String,
    finalized_slot: u64,
    simulation: Value,
    transaction_packet: CleanupTransactionPacket,
    estimated_fee_lamports: u64,
    recipient_balance_before: Option<u64>,
    recipient_balance_after: Option<u64>,
    expected_refund_lamports: u64,
    minimum_net_recipient_increase_lamports: u64,
    budget_reservation: LegacyLookupTableCleanupBudgetReservation,
}

#[derive(Debug)]
struct FinalizedCandidateFleet {
    candidates: BTreeMap<Pubkey, Result<Option<Candidate>, String>>,
    minimum_context_slot: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum LegacyCleanupChainEffect {
    Unchanged,
    Applied { observed_slot: u64 },
    Drifted,
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum PersistedCleanupSignatureState {
    FinalizedSuccess { slot: u64 },
    FinalizedFailure(String),
    Pending,
    NotFound,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum LegacyCleanupRecoveryDecision {
    Wait,
    Complete { observed_slot: u64 },
    ExpireAndRetry,
    PermanentFailure,
    ManualReconcile,
}

#[derive(Debug, Default)]
struct LegacyCleanupRecoverySummary {
    rows: Vec<Value>,
    completed_count: usize,
    waiting_count: usize,
    expired_count: usize,
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
        .require_schema_migration(21, "reusable_alt_production_controls")
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
    let protected_all = effective_cleanup_protected_tables(
        protected,
        &env_tables,
        &manual_allowlist,
        options.execute,
    );
    trace.finish(
        "cleanup.allowlist",
        phase_started,
        json!({
            "envTableCount": env_tables.len(),
            "manualAllowlistCount": manual_allowlist.len(),
            "protectedTableCount": protected_all.len(),
            "executeIgnoresEnvironmentProtection": options.execute,
        }),
    );

    let mut imported_fleet = database
        .imported_legacy_lookup_table_cleanup_fleet(&options.cluster)
        .await?;
    let registered_cleanup_inventory = database
        .registered_lookup_table_cleanup_inventory(&options.cluster)
        .await?;
    let inventory_fleet_hash = approved_imported_legacy_fleet_hash(&imported_fleet);
    let mut table_addresses = imported_fleet
        .iter()
        .map(|record| Pubkey::from_str(&record.source.table_address))
        .collect::<Result<Vec<_>, _>>()?;
    table_addresses.extend(
        registered_cleanup_inventory
            .iter()
            .map(|record| Pubkey::from_str(&record.table_address))
            .collect::<Result<Vec<_>, _>>()?,
    );
    table_addresses.sort();
    table_addresses.dedup();
    let history_authorities = imported_fleet
        .iter()
        .map(|record| Pubkey::from_str(&record.source.authority))
        .collect::<Result<BTreeSet<_>, _>>()?
        .into_iter()
        .collect::<Vec<_>>();
    let history_scan = if options.scan_history {
        let phase_started = Instant::now();
        let evidence =
            discover_tables_by_history(&options.rpc_url, &history_authorities, &options, &trace)
                .await?;
        trace.finish(
            "cleanup.scan_history",
            phase_started,
            json!({
                "eventCount": evidence.events.len(),
                "authorityCount": evidence.authorities.len(),
                "mutationSetHash": evidence.mutation_set_hash,
            }),
        );
        evidence
    } else {
        HistoryScanEvidence {
            events: Vec::new(),
            authorities: Vec::new(),
            mutation_set_hash: history_mutation_set_hash(&[]),
        }
    };
    let history_events = history_scan.events.clone();
    trace.event(
        "cleanup.discovery.complete",
        json!({
            "candidateAddressCount": table_addresses.len(),
            "inventorySource": "immutable_imported_database_fleet",
            "inventoryFleetHash": inventory_fleet_hash,
            "registeredV2RetirementCount": registered_cleanup_inventory.len(),
        }),
    );

    let phase_started = Instant::now();
    let current_slot = rpc.get_slot_with_commitment(CommitmentConfig::finalized())?;
    trace.finish(
        "cleanup.current_slot",
        phase_started,
        json!({ "currentSlot": current_slot }),
    );
    let mut recovery_summary = if options.execute {
        reconcile_pending_legacy_cleanup_attempts(&database, &rpc, &options, &history_events)
            .await?
    } else {
        LegacyCleanupRecoverySummary::default()
    };
    if options.execute {
        imported_fleet = database
            .imported_legacy_lookup_table_cleanup_fleet(&options.cluster)
            .await?;
        if approved_imported_legacy_fleet_hash(&imported_fleet) != inventory_fleet_hash {
            return Err("imported legacy fleet identity changed during cleanup recovery".into());
        }
        verify_imported_cleanup_history(&imported_fleet, &history_scan, options.min_slot)?;
    }
    let mut rows = std::mem::take(&mut recovery_summary.rows);
    let mut planned_cleanups = Vec::new();
    let mut planned_registered_cleanups = Vec::new();
    let mut registered_enqueue_summary = RegisteredCleanupEnqueueSummary::default();
    let mut total_reclaimable = 0_u64;
    let mut reclaimed_this_run = 0_u64;

    let phase_started = Instant::now();
    let loaded_fleet = load_candidates(&rpc, &table_addresses, &trace)?;
    if loaded_fleet.candidates.len() != table_addresses.len() {
        return Err(
            "finalized RPC inventory did not account for every database cleanup table".into(),
        );
    }
    let policy_authority = Pubkey::from_str(AFFECTED_POLICY_AUTHORITY)?;
    if options.execute
        && imported_fleet
            .iter()
            .any(|record| record.source.authority != policy_authority.to_string())
    {
        return Err(
            "imported cleanup fleet contains an authority other than the standard POLICY_KEYPAIR"
                .into(),
        );
    }
    if options
        .expected_fleet_count
        .is_some_and(|expected| expected != imported_fleet.len())
    {
        return Err(format!(
            "imported legacy fleet has {} tables, but --expected-fleet-count is {}",
            imported_fleet.len(),
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
            "accountCount": loaded_fleet.candidates.len(),
            "legacyFleetCount": imported_fleet.len(),
            "registeredV2RetirementCount": registered_cleanup_inventory.len(),
            "finalizedMinimumContextSlot": loaded_fleet.minimum_context_slot,
            "inventoryFleetHash": inventory_fleet_hash,
        }),
    );
    let phase_started = Instant::now();
    for imported in &imported_fleet {
        let table_address = Pubkey::from_str(&imported.source.table_address)?;
        let loaded_candidate = loaded_fleet
            .candidates
            .get(&table_address)
            .ok_or("imported cleanup table was omitted from finalized RPC batch")?;
        let candidate = match loaded_candidate {
            Ok(Some(candidate)) => candidate,
            Err(error) => {
                if options.execute {
                    return Err(format!(
                        "imported cleanup table {table_address} failed finalized decode: {error}"
                    )
                    .into());
                }
                rows.push(json!({
                    "table": table_address.to_string(),
                    "action": "skip",
                    "reason": safe_cleanup_operational_error_with_context(
                        "fetch_or_decode_failed",
                        &error,
                    ),
                    "importedControlPlane": imported_cleanup_record_json(imported),
                }));
                continue;
            }
            Ok(None) => {
                let candidate_history = history_events
                    .iter()
                    .filter(|event| event.table_address == table_address)
                    .map(|event| history_event_json(event, &protected_all))
                    .collect::<Vec<_>>();
                if imported.source.status != "closed" {
                    if options.execute {
                        return Err(format!(
                            "imported cleanup table {table_address} is absent at finalized commitment but database status is {}",
                            imported.source.status
                        )
                        .into());
                    }
                    rows.push(json!({
                        "table": table_address.to_string(),
                        "status": imported.source.status,
                        "action": "skip",
                        "reason": "imported_table_absent_before_durable_close_evidence",
                        "importedControlPlane": imported_cleanup_record_json(imported),
                        "historyEvents": candidate_history,
                    }));
                    continue;
                }
                rows.push(json!({
                    "table": table_address.to_string(),
                    "status": "closed",
                    "action": "complete",
                    "reason": "closed_imported_table_retained_in_durable_fleet_evidence",
                    "lamportsReclaimable": "0",
                    "importedControlPlane": imported_cleanup_record_json(imported),
                    "historyEvents": candidate_history,
                    "execution": {
                        "transactionsSent": false,
                        "closedSignatureVerifiedInFinalizedHistory": options.execute,
                    },
                }));
                continue;
            }
        };
        let exact_import_identity = candidate.owner == address_lookup_table_program::id()
            && candidate.authority.map(|value| value.to_string())
                == Some(imported.source.authority.clone())
            && candidate.address_count == usize::try_from(imported.source.address_count)?
            && ordered_candidate_address_hash(&candidate.addresses) == imported.source.address_hash
            && candidate
                .addresses
                .iter()
                .map(ToString::to_string)
                .collect::<Vec<_>>()
                == imported.source.addresses;
        if !exact_import_identity || imported.source.status == "closed" {
            if options.execute {
                return Err(format!(
                    "imported cleanup table {} finalized chain identity/lifecycle drifted from immutable evidence",
                    candidate.table_address
                )
                .into());
            }
            rows.push(json!({
                "table": candidate.table_address.to_string(),
                "action": "skip",
                "reason": "legacy_import_chain_identity_or_lifecycle_drift",
                "importedControlPlane": imported_cleanup_record_json(imported),
            }));
            continue;
        }
        let legacy_protection = database
            .legacy_lookup_table_cleanup_protection(
                &options.cluster,
                &candidate.table_address.to_string(),
            )
            .await?;
        let manually_protected = protected_all.contains(&candidate.table_address);
        let (action, reason) = if manually_protected {
            ("skip", "legacy_registry_env_or_manual_allowlist".to_owned())
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
        if matches!(action, "deactivate" | "close") {
            let row_index = rows.len();
            if options.execute {
                let authorization = require_retired_imported_legacy_cleanup_authorization(
                    &database,
                    &options.cluster,
                    candidate,
                    action,
                    legacy_protection.as_ref(),
                )
                .await?;
                let authority = candidate
                    .authority
                    .ok_or("candidate had no authority during execute")?;
                if authority != policy_authority {
                    return Err(format!(
                        "table {} authority {} does not match standard policy authority {}",
                        candidate.table_address, authority, policy_authority,
                    )
                    .into());
                }
                if action == "deactivate" {
                    let instruction = address_lookup_table_instruction::deactivate_lookup_table(
                        candidate.table_address,
                        policy_authority,
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
                    let recipient = options.recipient.unwrap_or(policy_authority);
                    if recipient != policy_authority {
                        return Err(
                            "close recipient must equal the POLICY_KEYPAIR public key".into()
                        );
                    }
                    let instruction = address_lookup_table_instruction::close_lookup_table(
                        candidate.table_address,
                        policy_authority,
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
            "importedControlPlane": imported_cleanup_record_json(imported),
            "legacyCleanupProtection": legacy_protection.as_ref().map(legacy_cleanup_protection_json),
            "historyEvents": candidate_history,
            "execution": execution,
        }));
    }
    for registered in &registered_cleanup_inventory {
        let table_address = Pubkey::from_str(&registered.table_address)?;
        let candidate_history = history_events
            .iter()
            .filter(|event| event.table_address == table_address)
            .map(|event| history_event_json(event, &protected_all))
            .collect::<Vec<_>>();
        let loaded_candidate = loaded_fleet
            .candidates
            .get(&table_address)
            .ok_or("registered v2 cleanup table was omitted from finalized RPC batch")?;
        let candidate = match loaded_candidate {
            Err(error) => {
                rows.push(json!({
                    "table": table_address.to_string(),
                    "action": "skip",
                    "reason": safe_cleanup_operational_error_with_context(
                        "registered_v2_fetch_or_decode_failed",
                        error,
                    ),
                    "registeredControlPlane": registered_cleanup_record_json(registered),
                    "historyEvents": candidate_history,
                }));
                continue;
            }
            Ok(None) => {
                rows.push(json!({
                    "table": table_address.to_string(),
                    "status": registered.desired_state.as_str(),
                    "action": if registered.desired_state == LookupTableLifecycle::Closed { "complete" } else { "skip" },
                    "reason": if registered.desired_state == LookupTableLifecycle::Closed {
                        "registered_v2_closed_account_absence_verified"
                    } else {
                        "registered_v2_account_absent_before_provisioner_reconciliation"
                    },
                    "registeredControlPlane": registered_cleanup_record_json(registered),
                    "historyEvents": candidate_history,
                    "execution": {
                        "mode": "provisioner_queue",
                        "signerLoaded": false,
                        "transactionsSent": false,
                    },
                }));
                continue;
            }
            Ok(Some(candidate)) => candidate,
        };
        let protection = database
            .lookup_table_cleanup_protection(&options.cluster, &registered.table_address)
            .await?;
        let (action, reason) = if let Some(protection) = protection.as_ref() {
            if protection.expected_authority != policy_authority.to_string() {
                (
                    "skip",
                    "registered_table_nonstandard_policy_authority".to_owned(),
                )
            } else {
                classify_registered_candidate(candidate, protection, &options.cluster, current_slot)
            }
        } else {
            (
                "skip",
                "registered_v2_cleanup_protection_missing".to_owned(),
            )
        };
        if matches!(action, "deactivate" | "close") {
            total_reclaimable = total_reclaimable.saturating_add(candidate.lamports);
            if let Some(protection) = protection.as_ref() {
                planned_registered_cleanups.push(PlannedRegisteredCleanup {
                    row_index: rows.len(),
                    protection: protection.clone(),
                    operation_kind: if action == "deactivate" {
                        LookupTableOperationKind::Deactivate
                    } else {
                        LookupTableOperationKind::Close
                    },
                });
            }
        }
        rows.push(json!({
            "table": candidate.table_address.to_string(),
            "owner": candidate.owner.to_string(),
            "authority": candidate.authority.map(|authority| authority.to_string()),
            "status": lookup_table_status(candidate, current_slot),
            "addressCount": candidate.address_count,
            "lamportsReclaimable": candidate.lamports.to_string(),
            "lastExtendedSlot": candidate.last_extended_slot,
            "deactivationSlot": candidate.deactivation_slot,
            "action": action,
            "reason": reason,
            "registeredControlPlane": registered_cleanup_record_json(registered),
            "cleanupProtection": protection.as_ref().map(cleanup_protection_json),
            "historyEvents": candidate_history,
            "execution": Value::Null,
        }));
    }
    if options.execute && !planned_registered_cleanups.is_empty() {
        let enqueue_started = Instant::now();
        registered_enqueue_summary = enqueue_registered_cleanups(
            &database,
            &options,
            &planned_registered_cleanups,
            &mut rows,
        )
        .await?;
        trace.finish(
            "cleanup.enqueue_registered",
            enqueue_started,
            json!({
                "plannedOperationCount": planned_registered_cleanups.len(),
                "operationCount": registered_enqueue_summary.operation_count,
                "queuedCount": registered_enqueue_summary.queued_count,
                "signerLoaded": false,
                "transactionsSent": false,
            }),
        );
    }
    trace.finish(
        "cleanup.candidates",
        phase_started,
        json!({
            "rowCount": rows.len(),
            "plannedExecutionCount": planned_cleanups.len(),
            "plannedProvisionerOperationCount": planned_registered_cleanups.len(),
            "queuedOperationCount": registered_enqueue_summary.queued_count,
        }),
    );

    if options.execute && !planned_cleanups.is_empty() {
        let phase_started = Instant::now();
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
        reclaimed_this_run = reclaimed_this_run.saturating_add(
            execute_planned_cleanups(
                &database,
                &rpc,
                &options,
                signer.as_ref(),
                &planned_cleanups,
                &mut rows,
            )
            .await?,
        );
        trace.finish(
            "cleanup.execute",
            phase_started,
            json!({ "plannedExecutionCount": planned_cleanups.len() }),
        );
    }

    let total_reclaimed = u64::try_from(
        database
            .cumulative_legacy_lookup_table_refunds(&options.cluster)
            .await?,
    )?;
    let phase_started = Instant::now();
    let report_chunks = [
        json!({
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
        }),
        json!({
            "legacyFleetCount": imported_fleet.len(),
            "registeredV2RetirementCount": registered_cleanup_inventory.len(),
            "inventoryFleetHash": inventory_fleet_hash,
            "inventorySource": "immutable_imported_database_fleet",
            "finalizedMinimumAccountContextSlot": loaded_fleet.minimum_context_slot,
            "expectedFleetCount": options.expected_fleet_count,
            "expectedFleetHash": options.expected_fleet_hash,
            "maxLamports": options.max_lamports,
            "budgetWindowSeconds": options.budget_window_seconds,
            "feesRecoverable": false,
            "feeNote": "ALT account rent can be reclaimed after close; transaction fees are not recoverable.",
            "currentSlot": current_slot,
            "totalReclaimableLamports": total_reclaimable.to_string(),
            "totalReclaimedLamports": total_reclaimed.to_string(),
            "reclaimedThisRunLamports": reclaimed_this_run.to_string(),
            "recoveredLegacyCleanupCount": recovery_summary.completed_count,
            "waitingLegacyCleanupCount": recovery_summary.waiting_count,
            "expiredLegacyCleanupAttemptCount": recovery_summary.expired_count,
        }),
        json!({
            "plannedExecutionCount": planned_cleanups.len(),
            "plannedProvisionerOperationCount": planned_registered_cleanups.len(),
            "provisionerOperationCount": registered_enqueue_summary.operation_count,
            "queuedProvisionerOperationCount": registered_enqueue_summary.queued_count,
            "registeredMutationBoundary": "dedicated_provisioner",
            "legacyDirectExecutionCount": planned_cleanups.len(),
            "historyEventCount": history_events.len(),
            "historyMutationSetHash": history_scan.mutation_set_hash,
            "historyAuthorities": history_scan.authorities.iter().map(authority_history_evidence_json).collect::<Vec<_>>(),
            "historyEvents": history_events.iter().map(|event| history_event_json(event, &protected_all)).collect::<Vec<_>>(),
            "candidates": rows,
        }),
    ];
    let mut report = Map::new();
    for chunk in report_chunks {
        report.extend(
            chunk
                .as_object()
                .ok_or("cleanup report chunk was not an object")?
                .clone(),
        );
    }
    println!("{}", serde_json::to_string_pretty(&Value::Object(report))?);
    trace.finish("cleanup.output", phase_started, json!({}));
    trace.event("cleanup.done", json!({}));
    Ok(())
}

async fn enqueue_registered_cleanups(
    database: &NeonSqlClient,
    options: &Options,
    planned: &[PlannedRegisteredCleanup],
    rows: &mut [Value],
) -> Result<RegisteredCleanupEnqueueSummary, Box<dyn Error>> {
    let policy_pubkey = Pubkey::from_str(AFFECTED_POLICY_AUTHORITY)?;
    let mut summary = RegisteredCleanupEnqueueSummary::default();
    for cleanup in planned {
        let protection = &cleanup.protection;
        let idempotency_key = format!(
            "registered-alt-cleanup:{}:{}:{}:{}",
            options.cluster,
            protection.table_id,
            protection.mutation_epoch,
            cleanup.operation_kind.as_str(),
        );
        let mut operation_context = json!({
            "source": "route-lookup-table-cleanup",
            "cluster": options.cluster,
            "table": protection.table_address,
            "expectedAuthority": protection.expected_authority,
            "expectedAddressCount": protection.address_count,
            "expectedAddressHash": protection.address_hash,
            "expectedMutationEpoch": protection.mutation_epoch,
        });
        if cleanup.operation_kind == LookupTableOperationKind::Close {
            operation_context
                .as_object_mut()
                .expect("cleanup operation context is an object")
                .insert(
                    "closeRecipient".to_owned(),
                    Value::String(policy_pubkey.to_string()),
                );
        }
        let operation = database
            .enqueue_lookup_table_operation(LookupTableOperationEnqueue {
                idempotency_key: idempotency_key.clone(),
                family_id: protection.family_id,
                route_lookup_table_id: Some(protection.table_id),
                manifest_id: None,
                binding_id: None,
                operation_kind: cleanup.operation_kind,
                target_generation: None,
                target_shard_ordinal: None,
                operation_context,
                mutation_epoch: protection.mutation_epoch,
                estimated_fee_lamports: None,
                estimated_rent_lamports: None,
                addresses: Vec::new(),
            })
            .await?;
        summary.operation_count += 1;
        summary.queued_count +=
            usize::from(operation.operation_state == LookupTableOperationStatus::Queued);
        let row = rows
            .get_mut(cleanup.row_index)
            .and_then(Value::as_object_mut)
            .ok_or("registered cleanup result row disappeared before enqueue reporting")?;
        row.insert(
            "execution".to_owned(),
            json!({
                "mode": "provisioner_queue",
                "operationId": operation.id,
                "idempotencyKey": idempotency_key,
                "operationKind": operation.operation_kind.as_str(),
                "operationState": operation.operation_state.as_str(),
                "signerLoaded": false,
                "transactionsSent": false,
            }),
        );
    }
    Ok(summary)
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

fn effective_cleanup_protected_tables(
    mut database_protected: BTreeSet<Pubkey>,
    environment_tables: &BTreeSet<Pubkey>,
    manual_allowlist: &BTreeSet<Pubkey>,
    execute: bool,
) -> BTreeSet<Pubkey> {
    if !execute {
        database_protected.extend(environment_tables.iter().copied());
        database_protected.extend(manual_allowlist.iter().copied());
    }
    database_protected
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
    let mut max_lamports = None;
    let mut budget_window_seconds = None;
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
            "--max-lamports" => {
                max_lamports = Some(
                    iter.next()
                        .ok_or("--max-lamports requires a value")?
                        .parse()
                        .map_err(|_| "--max-lamports must be an i64")?,
                );
            }
            "--budget-window-seconds" => {
                budget_window_seconds = Some(
                    iter.next()
                        .ok_or("--budget-window-seconds requires a value")?
                        .parse()
                        .map_err(|_| "--budget-window-seconds must be an i64")?,
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
                    "Usage: route-lookup-table-cleanup --cluster <CLUSTER> --rpc-url <URL> [--dry-run|--execute] [--recipient <POLICY_PUBKEY>] [--authority-key-env POLICY_KEYPAIR] [--scan-history] [--history-limit <PAGE_SIZE>] [--min-slot <APPROVED_BOUNDARY>] [--simulate-before-submit] [--bundle-size 1] [--trace-timing] [--expected-fleet-count <N> --expected-fleet-hash <HASH>] [--max-lamports <POSITIVE_LAMPORTS> --budget-window-seconds <SECONDS>]\n\nDry-run is the default. Legacy cleanup inventory is the complete immutable imported fleet in Neon, including already closed tables; registered v2 retirement is a separate DB-native inventory that only enqueues provisioner operations. Whole-program scans and ad-hoc table subsets are rejected. Every mode requires explicit YIELD_ALT_CLUSTER/--cluster, SOLANA_RPC_URL/--rpc-url, and NEON_DATABASE_URL. Execute requires the approved imported fleet count/hash, a positive history boundary, finalized paginated signer history, and explicit positive rolling budget fences. Each familyless legacy mutation uses the standard POLICY_KEYPAIR, finalized simulation/preflight, and one durable cluster-budget reservation before signing. Close rent is refunded only to the policy signer."
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
    if scan_program_accounts {
        return Err(
            "--scan-program-accounts is removed; cleanup inventory is the immutable imported database fleet"
                .into(),
        );
    }
    if !tables.is_empty() {
        return Err(
            "--table subsets are rejected; cleanup must account for the complete imported database fleet"
                .into(),
        );
    }
    if !(1..=1_000).contains(&history_limit) {
        return Err("--history-limit is a page size and must be between 1 and 1000".into());
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
        if !allowlist.is_empty() {
            return Err(
                "--execute rejects --allowlist; an approved imported-legacy fleet cannot be silently withheld from refund"
                    .into(),
            );
        }
        if expected_fleet_count.is_none() || expected_fleet_hash.is_none() {
            return Err(
                "--execute requires --expected-fleet-count and --expected-fleet-hash from an approved dry run"
                    .into(),
            );
        }
        if min_slot.is_none_or(|slot| slot == 0) {
            return Err(
                "--execute requires a positive --min-slot approved history boundary".into(),
            );
        }
        if max_lamports.is_none_or(|value| value <= 0) {
            return Err("--execute requires positive --max-lamports".into());
        }
        if budget_window_seconds.is_none_or(|value| !(1..=31_536_000).contains(&value)) {
            return Err("--execute requires --budget-window-seconds between 1 and 31536000".into());
        }
        if bundle_size != 1 {
            return Err(
                "--execute requires --bundle-size 1 so each legacy mutation holds its own database authorization fence"
                    .into(),
            );
        }
        scan_program_accounts = false;
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
        max_lamports,
        budget_window_seconds,
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

async fn discover_tables_by_history(
    rpc_url: &str,
    authorities: &[Pubkey],
    options: &Options,
    trace: &TraceLog,
) -> Result<HistoryScanEvidence, Box<dyn Error>> {
    let http = reqwest::Client::new();
    let mut events = Vec::new();
    let mut authority_evidence = Vec::new();
    for authority in authorities {
        let mut before = None::<String>;
        let mut page_count = 0_usize;
        let mut signature_count = 0_usize;
        let mut oldest_slot = None::<u64>;
        let mut boundary_reached = false;
        let mut exhausted = false;
        let mut seen_cursors = BTreeSet::new();
        loop {
            let phase_started = Instant::now();
            let mut config = json!({
                "limit": options.history_limit,
                "commitment": "finalized",
            });
            if let Some(before) = before.as_ref() {
                config
                    .as_object_mut()
                    .expect("history config is an object")
                    .insert("before".to_owned(), Value::String(before.clone()));
            }
            let signatures = rpc_call(
                &http,
                rpc_url,
                "getSignaturesForAddress",
                json!([authority.to_string(), config]),
            )
            .await?;
            let signatures = signatures.as_array().ok_or(
                "getSignaturesForAddress result was not an array during cleanup evidence scan",
            )?;
            page_count += 1;
            trace.finish(
                "cleanup.scan_history.signatures_page",
                phase_started,
                json!({
                    "authority": authority.to_string(),
                    "page": page_count,
                    "signatureCount": signatures.len(),
                    "pageSize": options.history_limit,
                    "before": before,
                }),
            );
            if signatures.is_empty() {
                exhausted = true;
                break;
            }
            let next_before = signatures
                .last()
                .and_then(|entry| entry.get("signature"))
                .and_then(Value::as_str)
                .ok_or("history page ended without a signature cursor")?
                .to_owned();
            if before.as_deref() == Some(next_before.as_str())
                || !seen_cursors.insert(next_before.clone())
            {
                return Err(format!(
                    "finalized signer history pagination made no progress for {authority}"
                )
                .into());
            }
            for entry in signatures {
                let signature = entry
                    .get("signature")
                    .and_then(Value::as_str)
                    .ok_or("history entry omitted signature")?;
                let slot = entry
                    .get("slot")
                    .and_then(Value::as_u64)
                    .ok_or("history entry omitted slot")?;
                signature_count += 1;
                oldest_slot = Some(oldest_slot.map_or(slot, |oldest| oldest.min(slot)));
                if options.min_slot.is_some_and(|min_slot| slot < min_slot) {
                    boundary_reached = true;
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
                            "commitment": "finalized",
                            "maxSupportedTransactionVersion": 0,
                        }
                    ]),
                )
                .await?;
                if transaction.is_null() {
                    return Err(format!(
                        "finalized transaction {signature} at slot {slot} was unavailable inside the approved history boundary"
                    )
                    .into());
                }
                events.extend(lookup_table_events_from_transaction(
                    signature,
                    slot,
                    block_time,
                    &transaction,
                    authorities,
                )?);
                trace.finish(
                    "cleanup.scan_history.transaction",
                    phase_started,
                    json!({
                        "signature": signature,
                        "slot": slot,
                        "eventCount": events.len(),
                    }),
                );
            }
            if boundary_reached {
                break;
            }
            before = Some(next_before);
        }
        authority_evidence.push(AuthorityHistoryEvidence {
            authority: *authority,
            page_count,
            signature_count,
            oldest_slot,
            boundary_reached,
            exhausted,
        });
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
    let mutation_set_hash = history_mutation_set_hash(&events);
    Ok(HistoryScanEvidence {
        events,
        authorities: authority_evidence,
        mutation_set_hash,
    })
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
    _trace: &TraceLog,
) -> Result<FinalizedCandidateFleet, Box<dyn Error>> {
    let mut candidates = BTreeMap::new();
    let mut minimum_context_slot = u64::MAX;
    for chunk in table_addresses.chunks(100) {
        let response =
            rpc.get_multiple_accounts_with_commitment(chunk, CommitmentConfig::finalized())?;
        minimum_context_slot = minimum_context_slot.min(response.context.slot);
        if response.value.len() != chunk.len() {
            return Err("finalized getMultipleAccounts returned a partial cleanup batch".into());
        }
        for (table_address, account) in chunk.iter().copied().zip(response.value) {
            let candidate = match account {
                None => Ok(None),
                Some(account) if absent_or_closed_account(Some(&account)) => Ok(None),
                Some(account) => candidate_from_account(table_address, &account).map(Some),
            };
            candidates.insert(table_address, candidate);
        }
    }
    if minimum_context_slot == u64::MAX {
        minimum_context_slot = rpc.get_slot_with_commitment(CommitmentConfig::finalized())?;
    }
    Ok(FinalizedCandidateFleet {
        candidates,
        minimum_context_slot,
    })
}

fn load_candidate(rpc: &RpcClient, table_address: Pubkey) -> Result<Candidate, Box<dyn Error>> {
    let account = rpc
        .get_account_with_commitment(&table_address, CommitmentConfig::finalized())?
        .value
        .ok_or_else(|| format!("AccountNotFound: pubkey={table_address}"))?;
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

fn absent_or_closed_account(account: Option<&Account>) -> bool {
    match account {
        None => true,
        Some(account) => account.lamports == 0 && account.data.is_empty(),
    }
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
    if protection.table_address != candidate.table_address.to_string() {
        drift.push("table_address_mismatch".to_owned());
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

fn approved_imported_legacy_fleet_hash(fleet: &[ImportedLegacyLookupTableCleanupRecord]) -> String {
    let mut fleet = fleet.to_vec();
    fleet.sort_by_key(|record| record.source.id);
    let mut parts = vec!["legacy-alt-imported-fleet-v2".to_owned()];
    for record in fleet {
        parts.extend([
            record.source.id.to_string(),
            record.source.cluster,
            record.source.scope,
            record.source.table_address,
            record.source.authority,
            record.source.address_count.to_string(),
            record.source.address_hash,
            record
                .source
                .legacy_kind
                .map_or_else(String::new, |kind| kind.as_str().to_owned()),
            record
                .source
                .legacy_import_run_id
                .unwrap_or_default()
                .to_string(),
            record.import_fingerprint,
            record.import_verified_slot.to_string(),
        ]);
        parts.extend(record.source.addresses);
    }
    let mut hasher = Sha256::new();
    for part in parts {
        hasher.update((part.len() as u64).to_le_bytes());
        hasher.update(part.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn imported_cleanup_record_json(record: &ImportedLegacyLookupTableCleanupRecord) -> Value {
    json!({
        "tableId": record.source.id,
        "scope": record.source.scope,
        "status": record.source.status,
        "durable": record.source.durable,
        "authority": record.source.authority,
        "addressCount": record.source.address_count,
        "addressHash": record.source.address_hash,
        "legacyKind": record.source.legacy_kind.map(|kind| kind.as_str()),
        "legacyImportRunId": record.source.legacy_import_run_id,
        "importFingerprint": record.import_fingerprint,
        "importVerifiedSlot": record.import_verified_slot,
        "deactivatedSlot": record.deactivated_slot,
        "deactivateSignature": record.deactivate_signature,
        "closedSignature": record.closed_signature,
        "closeRecipient": record.close_recipient,
        "reclaimedLamports": record.reclaimed_lamports,
    })
}

fn registered_cleanup_record_json(record: &ReusableLookupTableRecord) -> Value {
    json!({
        "tableId": record.id,
        "familyId": record.family_id,
        "allocationKind": record.allocation_kind.as_str(),
        "generation": record.generation,
        "shardOrdinal": record.shard_ordinal,
        "desiredState": record.desired_state.as_str(),
        "acceptingAllocations": record.accepting_allocations,
        "expectedAuthority": record.authority,
        "addressCount": record.address_count,
        "addressHash": record.address_hash,
        "mutationEpoch": record.mutation_epoch,
        "rollbackUntil": record.rollback_until,
    })
}

fn history_mutation_set_hash(events: &[HistoryEvent]) -> String {
    let mut events = events.to_vec();
    events.sort_by(|left, right| {
        left.slot
            .cmp(&right.slot)
            .then_with(|| left.signature.cmp(&right.signature))
            .then_with(|| left.kind.cmp(right.kind))
            .then_with(|| left.table_address.cmp(&right.table_address))
    });
    let mut hasher = Sha256::new();
    for part in std::iter::once("legacy-alt-finalized-mutation-set-v1".to_owned()).chain(
        events.into_iter().flat_map(|event| {
            [
                event.signature,
                event.slot.to_string(),
                event.kind.to_owned(),
                event.table_address.to_string(),
                event
                    .authority
                    .map_or_else(String::new, |value| value.to_string()),
                event
                    .payer_or_recipient
                    .map_or_else(String::new, |value| value.to_string()),
            ]
        }),
    ) {
        hasher.update((part.len() as u64).to_le_bytes());
        hasher.update(part.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn authority_history_evidence_json(evidence: &AuthorityHistoryEvidence) -> Value {
    json!({
        "authority": evidence.authority.to_string(),
        "pageCount": evidence.page_count,
        "signatureCount": evidence.signature_count,
        "oldestSlot": evidence.oldest_slot,
        "boundaryReached": evidence.boundary_reached,
        "exhausted": evidence.exhausted,
        "completeToBoundaryOrExhaustion": evidence.boundary_reached || evidence.exhausted,
    })
}

fn verify_imported_cleanup_history(
    fleet: &[ImportedLegacyLookupTableCleanupRecord],
    history: &HistoryScanEvidence,
    min_slot: Option<u64>,
) -> Result<(), Box<dyn Error>> {
    let min_slot = min_slot
        .filter(|slot| *slot > 0)
        .ok_or("execute history verification requires a positive approved minimum slot")?;
    let expected_authorities = fleet
        .iter()
        .map(|record| Pubkey::from_str(&record.source.authority))
        .collect::<Result<BTreeSet<_>, _>>()?;
    let observed_authorities = history
        .authorities
        .iter()
        .map(|evidence| evidence.authority)
        .collect::<BTreeSet<_>>();
    if expected_authorities != observed_authorities
        || history
            .authorities
            .iter()
            .any(|evidence| !evidence.boundary_reached && !evidence.exhausted)
    {
        return Err(
            "finalized signer history did not prove every imported authority to the approved boundary or exhaustion"
                .into(),
        );
    }
    for record in fleet {
        let table = Pubkey::from_str(&record.source.table_address)?;
        let authority = Pubkey::from_str(&record.source.authority)?;
        for (kind, signature) in [
            ("deactivate", record.deactivate_signature.as_deref()),
            ("close", record.closed_signature.as_deref()),
        ] {
            let Some(signature) = signature else {
                continue;
            };
            let exact_event = history.events.iter().any(|event| {
                event.signature == signature
                    && event.slot >= min_slot
                    && event.kind == kind
                    && event.table_address == table
                    && event.authority == Some(authority)
                    && (kind != "close" || event.payer_or_recipient == Some(authority))
            });
            if !exact_event {
                return Err(format!(
                    "stored {kind} signature for imported table {table} was not proven in finalized paginated history at or after slot {min_slot}"
                )
                .into());
            }
        }
        if record.source.status == "closed"
            && (record.deactivate_signature.is_none()
                || record.closed_signature.is_none()
                || record.close_recipient.as_deref() != Some(record.source.authority.as_str())
                || record.reclaimed_lamports.is_none_or(|value| value <= 0))
        {
            return Err(format!(
                "closed imported table {table} lacks complete durable mutation/refund evidence"
            )
            .into());
        }
    }
    Ok(())
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

fn legacy_cleanup_recovery_decision(
    signature_state: &PersistedCleanupSignatureState,
    chain_effect: LegacyCleanupChainEffect,
    blockhash_expired: bool,
) -> LegacyCleanupRecoveryDecision {
    match signature_state {
        PersistedCleanupSignatureState::FinalizedFailure(_) => {
            LegacyCleanupRecoveryDecision::PermanentFailure
        }
        PersistedCleanupSignatureState::Pending => LegacyCleanupRecoveryDecision::Wait,
        PersistedCleanupSignatureState::FinalizedSuccess { slot } => match chain_effect {
            LegacyCleanupChainEffect::Applied { observed_slot } => {
                LegacyCleanupRecoveryDecision::Complete {
                    observed_slot: observed_slot.max(*slot),
                }
            }
            LegacyCleanupChainEffect::Unchanged | LegacyCleanupChainEffect::Drifted => {
                LegacyCleanupRecoveryDecision::ManualReconcile
            }
        },
        PersistedCleanupSignatureState::NotFound => match chain_effect {
            LegacyCleanupChainEffect::Unchanged if blockhash_expired => {
                LegacyCleanupRecoveryDecision::ExpireAndRetry
            }
            LegacyCleanupChainEffect::Unchanged => LegacyCleanupRecoveryDecision::Wait,
            LegacyCleanupChainEffect::Applied { .. } | LegacyCleanupChainEffect::Drifted => {
                LegacyCleanupRecoveryDecision::ManualReconcile
            }
        },
    }
}

fn history_proves_persisted_cleanup_signature(
    attempt: &LegacyLookupTableCleanupAttemptRecord,
    history_events: &[HistoryEvent],
) -> Option<u64> {
    let signature = attempt.transaction_signature.as_deref()?;
    let table = Pubkey::from_str(&attempt.table_address).ok()?;
    let authority = Pubkey::from_str(&attempt.expected_authority).ok()?;
    history_events
        .iter()
        .find(|event| {
            event.signature == signature
                && event.table_address == table
                && event.kind == attempt.operation_kind.as_str()
                && event.authority == Some(authority)
                && (attempt.operation_kind != LookupTableOperationKind::Close
                    || event.payer_or_recipient == Some(authority))
        })
        .map(|event| event.slot)
}

fn load_persisted_cleanup_signature_state(
    rpc: &RpcClient,
    attempt: &LegacyLookupTableCleanupAttemptRecord,
    history_events: &[HistoryEvent],
) -> Result<PersistedCleanupSignatureState, Box<dyn Error>> {
    let signature = Signature::from_str(
        attempt
            .transaction_signature
            .as_deref()
            .ok_or("legacy cleanup recovery attempt has no durable signature")?,
    )?;
    let status = rpc
        .get_signature_statuses_with_history(&[signature])?
        .value
        .into_iter()
        .next()
        .flatten();
    if let Some(status) = status {
        if !status.satisfies_commitment(CommitmentConfig::finalized()) {
            return Ok(PersistedCleanupSignatureState::Pending);
        }
        if let Some(error) = status.err {
            return Ok(PersistedCleanupSignatureState::FinalizedFailure(format!(
                "{error:?}"
            )));
        }
        return Ok(PersistedCleanupSignatureState::FinalizedSuccess { slot: status.slot });
    }
    if let Some(slot) = history_proves_persisted_cleanup_signature(attempt, history_events) {
        return Ok(PersistedCleanupSignatureState::FinalizedSuccess { slot });
    }
    Ok(PersistedCleanupSignatureState::NotFound)
}

fn load_legacy_cleanup_chain_effect(
    rpc: &RpcClient,
    attempt: &LegacyLookupTableCleanupAttemptRecord,
    observed_slot: u64,
) -> Result<LegacyCleanupChainEffect, Box<dyn Error>> {
    let table_address = Pubkey::from_str(&attempt.table_address)?;
    let account = rpc
        .get_account_with_commitment(&table_address, CommitmentConfig::finalized())?
        .value;
    if absent_or_closed_account(account.as_ref()) {
        return Ok(
            if attempt.operation_kind == LookupTableOperationKind::Close {
                LegacyCleanupChainEffect::Applied { observed_slot }
            } else {
                LegacyCleanupChainEffect::Drifted
            },
        );
    }
    let account = account.expect("non-closed cleanup account must be present");
    let candidate = candidate_from_account(table_address, &account)
        .map_err(|error| format!("legacy cleanup recovery decode failed: {error}"))?;
    if candidate.owner != address_lookup_table_program::id()
        || candidate.authority.map(|value| value.to_string())
            != Some(attempt.expected_authority.clone())
        || i32::try_from(candidate.address_count)? != attempt.expected_address_count
        || ordered_candidate_address_hash(&candidate.addresses) != attempt.expected_address_hash
    {
        return Ok(LegacyCleanupChainEffect::Drifted);
    }
    Ok(match attempt.operation_kind {
        LookupTableOperationKind::Deactivate if candidate.deactivation_slot != u64::MAX => {
            LegacyCleanupChainEffect::Applied {
                observed_slot: candidate.deactivation_slot,
            }
        }
        LookupTableOperationKind::Deactivate | LookupTableOperationKind::Close => {
            LegacyCleanupChainEffect::Unchanged
        }
        _ => LegacyCleanupChainEffect::Drifted,
    })
}

async fn reconcile_pending_legacy_cleanup_attempts(
    database: &NeonSqlClient,
    rpc: &RpcClient,
    options: &Options,
    history_events: &[HistoryEvent],
) -> Result<LegacyCleanupRecoverySummary, Box<dyn Error>> {
    let attempts = database
        .pending_legacy_lookup_table_cleanup_attempts(&options.cluster)
        .await?;
    let mut summary = LegacyCleanupRecoverySummary::default();
    let current_height = i64::try_from(rpc.get_block_height()?)?;
    let finalized_slot = rpc.get_slot_with_commitment(CommitmentConfig::finalized())?;
    for attempt in attempts {
        if attempt.attempt_state == LegacyLookupTableCleanupAttemptState::Prepared {
            summary.waiting_count += 1;
            summary.rows.push(json!({
                "table": attempt.table_address,
                "action": "recover",
                "reason": "durable_prepared_attempt_can_resume_before_signing",
                "execution": {
                    "attemptId": attempt.id,
                    "attemptNumber": attempt.attempt_number,
                    "attemptState": attempt.attempt_state.as_str(),
                    "transactionsSent": false,
                },
            }));
            continue;
        }
        let signature = attempt
            .transaction_signature
            .clone()
            .ok_or("non-prepared legacy cleanup attempt lacks durable signature")?;
        let signature_state =
            load_persisted_cleanup_signature_state(rpc, &attempt, history_events)?;
        let chain_effect = load_legacy_cleanup_chain_effect(rpc, &attempt, finalized_slot)?;
        let blockhash_expired = attempt
            .last_valid_block_height
            .is_some_and(|height| current_height > height);
        let decision =
            legacy_cleanup_recovery_decision(&signature_state, chain_effect, blockhash_expired);
        let mut row = json!({
            "table": attempt.table_address,
            "action": "recover",
            "reason": format!("legacy_cleanup_recovery_{decision:?}").to_lowercase(),
            "execution": {
                "attemptId": attempt.id,
                "attemptNumber": attempt.attempt_number,
                "attemptState": attempt.attempt_state.as_str(),
                "signaturePersisted": true,
                "transactionsSent": false,
            },
        });
        match decision {
            LegacyCleanupRecoveryDecision::Wait => {
                summary.waiting_count += 1;
            }
            LegacyCleanupRecoveryDecision::ExpireAndRetry => {
                database
                    .expire_unobserved_legacy_lookup_table_cleanup_attempt(
                        attempt.id,
                        &signature,
                        current_height,
                    )
                    .await?;
                summary.expired_count += 1;
                row["execution"]["attemptState"] = json!("expired");
            }
            LegacyCleanupRecoveryDecision::PermanentFailure => {
                let detail = match signature_state {
                    PersistedCleanupSignatureState::FinalizedFailure(ref detail) => detail,
                    _ => "persisted cleanup transaction failed",
                };
                database
                    .fail_legacy_lookup_table_cleanup_attempt_permanently(
                        attempt.id, &signature, detail,
                    )
                    .await?;
                row["execution"]["attemptState"] = json!("permanent_failure");
            }
            LegacyCleanupRecoveryDecision::Complete { observed_slot } => {
                let recipient_balance_after = attempt
                    .close_recipient
                    .as_deref()
                    .map(Pubkey::from_str)
                    .transpose()?
                    .map(|recipient| finalized_account_lamports(rpc, &recipient, observed_slot))
                    .transpose()?
                    .map(i64::try_from)
                    .transpose()?;
                database
                    .complete_legacy_lookup_table_cleanup_attempt(
                        attempt.id,
                        FinalizedLegacyLookupTableCleanupAttempt {
                            transaction_signature: signature,
                            finalized_slot: i64::try_from(observed_slot)?,
                            recipient_balance_after,
                            actual_reclaimed_lamports: attempt.expected_reclaimed_lamports,
                        },
                    )
                    .await?;
                summary.completed_count += 1;
                row["execution"]["attemptState"] = json!("complete");
                row["execution"]["recoveredFromDurableSignature"] = json!(true);
            }
            LegacyCleanupRecoveryDecision::ManualReconcile => {
                database
                    .mark_legacy_lookup_table_cleanup_attempt_needs_reconcile(
                        attempt.id,
                        &signature,
                        "chain_history_mismatch",
                        "persisted cleanup signature and finalized chain/history do not prove one attributable effect",
                    )
                    .await?;
                return Err(format!(
                    "legacy cleanup attempt {} requires manual reconciliation; no resend was attempted",
                    attempt.id
                )
                .into());
            }
        }
        summary.rows.push(row);
    }
    Ok(summary)
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
    preflight_cleanup_batches_fit_packet(
        rpc,
        signer.pubkey(),
        planned_cleanups,
        options.bundle_size,
    )?;
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
            database,
            rpc,
            options,
            signer,
            &instructions,
            cleanup,
            operation_kind,
            close_recipient,
            expected_refund_lamports,
        )
        .await?;
        let mut execution = json!({
            "attemptId": result.attempt_id,
            "signature": result.signature.clone(),
            "finalizedSlot": result.finalized_slot.to_string(),
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
            "budgetReservation": result.budget_reservation,
            "simulation": result.simulation.clone(),
        });
        let (observed_slot, reclaimed_lamports) = if let Some(recipient) = cleanup.recipient {
            let post_close = retry_minimum_context_slot(|| {
                rpc.get_account_with_config(
                    &cleanup.table_address,
                    RpcAccountInfoConfig {
                        commitment: Some(CommitmentConfig::finalized()),
                        min_context_slot: Some(result.finalized_slot),
                        ..RpcAccountInfoConfig::default()
                    },
                )
            })?;
            if !absent_or_closed_account(post_close.value.as_ref()) {
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
                i64::try_from(result.finalized_slot)?,
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
            (i64::try_from(reloaded.deactivation_slot)?, None)
        };
        database
            .complete_legacy_lookup_table_cleanup_attempt(
                result.attempt_id,
                FinalizedLegacyLookupTableCleanupAttempt {
                    transaction_signature: result.signature.clone(),
                    finalized_slot: observed_slot,
                    recipient_balance_after: result
                        .recipient_balance_after
                        .map(i64::try_from)
                        .transpose()?,
                    actual_reclaimed_lamports: reclaimed_lamports,
                },
            )
            .await?;
        drop(authorization);
        execution["databaseRecord"] = json!({
            "status": "durable_attempt_completed",
            "attemptId": result.attempt_id,
        });
        set_candidate_execution(rows, cleanup.row_index, execution)?;
    }
    Ok(total_reclaimed)
}

fn preflight_cleanup_batches_fit_packet(
    rpc: &RpcClient,
    payer: Pubkey,
    planned_cleanups: &[PlannedCleanup],
    bundle_size: usize,
) -> Result<(), Box<dyn Error>> {
    let (blockhash, _) = rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
    for (batch_index, batch) in planned_cleanups.chunks(bundle_size).enumerate() {
        let instructions = batch
            .iter()
            .map(|cleanup| cleanup.instruction.clone())
            .collect::<Vec<_>>();
        let transaction = unsigned_cleanup_transaction(&instructions, payer, blockhash);
        ensure_cleanup_transaction_fits_packet(&transaction).map_err(|error| {
            format!(
                "cleanup batch {batch_index} with {} instruction(s) is too large: {error}",
                instructions.len()
            )
        })?;
    }
    Ok(())
}

fn unsigned_cleanup_transaction(
    instructions: &[Instruction],
    payer: Pubkey,
    blockhash: solana_sdk::hash::Hash,
) -> Transaction {
    let message = Message::new_with_blockhash(instructions, Some(&payer), &blockhash);
    let required_signatures = usize::from(message.header.num_required_signatures);
    let mut transaction = Transaction::new_unsigned(message);
    transaction.signatures = vec![Signature::default(); required_signatures];
    transaction
}

fn run_after_cleanup_budget_approval<T>(
    reservation: &LegacyLookupTableCleanupBudgetReservation,
    action: impl FnOnce() -> Result<T, Box<dyn Error>>,
) -> Result<T, Box<dyn Error>> {
    if !reservation.approved {
        return Err(format!(
            "legacy cleanup attempt {} exceeded the shared cluster rolling budget before signing",
            reservation.legacy_cleanup_attempt_id
        )
        .into());
    }
    action()
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

async fn send_cleanup_instruction_batch(
    database: &NeonSqlClient,
    rpc: &RpcClient,
    options: &Options,
    signer: &dyn Signer,
    instructions: &[Instruction],
    cleanup: &PlannedCleanup,
    operation_kind: LookupTableOperationKind,
    close_recipient: Option<Pubkey>,
    expected_refund_lamports: u64,
) -> Result<CleanupTransactionResult, Box<dyn Error>> {
    let (blockhash, last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
    let mut transaction = unsigned_cleanup_transaction(instructions, signer.pubkey(), blockhash);
    ensure_cleanup_transaction_fits_packet(&transaction)?;
    let minimum_context_slot = rpc.get_slot_with_commitment(CommitmentConfig::finalized())?;
    let simulation = simulate_cleanup_transaction(rpc, &transaction, minimum_context_slot)?;
    let estimated_fee_lamports = rpc.get_fee_for_message(&transaction.message)?;
    let recipient_balance_before = close_recipient
        .map(|recipient| finalized_account_lamports(rpc, &recipient, minimum_context_slot))
        .transpose()?;
    let attempt = database
        .prepare_legacy_lookup_table_cleanup_attempt(LegacyLookupTableCleanupAttemptPrepare {
            cluster: options.cluster.clone(),
            table_address: cleanup.table_address.to_string(),
            expected_authorization_token: cleanup.expected_cleanup_authorization_token.clone(),
            operation_kind,
            expected_authority: cleanup.expected_authority.to_string(),
            expected_address_count: i32::try_from(cleanup.expected_address_count)?,
            expected_address_hash: cleanup.expected_address_hash.clone(),
            close_recipient: close_recipient.map(|recipient| recipient.to_string()),
            expected_reclaimed_lamports: (expected_refund_lamports > 0)
                .then(|| i64::try_from(expected_refund_lamports))
                .transpose()?,
        })
        .await?;
    if attempt.attempt_state != LegacyLookupTableCleanupAttemptState::Prepared {
        return Err(format!(
            "legacy cleanup attempt {} is {}; reconcile it before any fresh broadcast",
            attempt.id, attempt.attempt_state,
        )
        .into());
    }
    let budget_reservation = database
        .reserve_legacy_lookup_table_cleanup_budget(
            &options.cluster,
            attempt.id,
            LookupTableClusterBudgetPolicy {
                max_lamports: options
                    .max_lamports
                    .ok_or("execute cleanup requires --max-lamports")?,
                rolling_window_seconds: options
                    .budget_window_seconds
                    .ok_or("execute cleanup requires --budget-window-seconds")?,
            },
            i64::try_from(estimated_fee_lamports)?,
            0,
        )
        .await?;
    run_after_cleanup_budget_approval(&budget_reservation, || {
        transaction
            .try_sign(&[signer], blockhash)
            .map_err(|error| error.into())
    })?;
    let transaction_packet = ensure_cleanup_transaction_fits_packet(&transaction)?;
    let signature = transaction
        .signatures
        .first()
        .ok_or("signed cleanup transaction has no signature")?
        .to_owned();
    let message_hash = {
        let mut hasher = Sha256::new();
        hasher.update(bincode::serialize(&transaction.message)?);
        format!("{:x}", hasher.finalize())
    };
    database
        .persist_signed_legacy_lookup_table_cleanup_attempt(
            attempt.id,
            SignedLegacyLookupTableCleanupAttempt {
                transaction_signature: signature.to_string(),
                message_hash,
                recent_blockhash: blockhash.to_string(),
                last_valid_block_height: i64::try_from(last_valid_block_height)?,
                estimated_fee_lamports: i64::try_from(estimated_fee_lamports)?,
                recipient_balance_before: recipient_balance_before
                    .map(i64::try_from)
                    .transpose()?,
            },
        )
        .await?;
    let returned_signature = match retry_minimum_context_slot(|| {
        rpc.send_transaction_with_config(
            &transaction,
            RpcSendTransactionConfig {
                skip_preflight: false,
                preflight_commitment: Some(CommitmentLevel::Finalized),
                max_retries: Some(0),
                min_context_slot: Some(minimum_context_slot),
                ..RpcSendTransactionConfig::default()
            },
        )
    }) {
        Ok(signature) => signature,
        Err(error) => {
            database
                .mark_legacy_lookup_table_cleanup_attempt_needs_reconcile(
                    attempt.id,
                    &signature.to_string(),
                    "ambiguous_send",
                    &safe_cleanup_operational_error(&error),
                )
                .await?;
            return Err(format!(
                "legacy cleanup send result is ambiguous; durable attempt {} must reconcile",
                attempt.id
            )
            .into());
        }
    };
    if returned_signature != signature {
        database
            .mark_legacy_lookup_table_cleanup_attempt_needs_reconcile(
                attempt.id,
                &signature.to_string(),
                "signature_mismatch",
                "RPC returned a different signature from the durable signed identity",
            )
            .await?;
        return Err(
            "cleanup RPC returned a signature different from the durable signed identity".into(),
        );
    }
    database
        .mark_legacy_lookup_table_cleanup_attempt_submitted(attempt.id, &signature.to_string())
        .await?;
    if let Err(error) =
        rpc.confirm_transaction_with_spinner(&signature, &blockhash, CommitmentConfig::finalized())
    {
        database
            .mark_legacy_lookup_table_cleanup_attempt_needs_reconcile(
                attempt.id,
                &signature.to_string(),
                "confirmation_ambiguous",
                &safe_cleanup_operational_error(&error),
            )
            .await?;
        return Err(format!(
            "legacy cleanup confirmation is ambiguous; durable attempt {} must reconcile",
            attempt.id
        )
        .into());
    }
    let finalized_slot = require_finalized_signature(rpc, &signature)?;
    let recipient_balance_after = close_recipient
        .map(|recipient| finalized_account_lamports(rpc, &recipient, finalized_slot))
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
        attempt_id: attempt.id,
        signature: signature.to_string(),
        finalized_slot,
        simulation,
        transaction_packet,
        estimated_fee_lamports,
        recipient_balance_before,
        recipient_balance_after,
        expected_refund_lamports,
        minimum_net_recipient_increase_lamports,
        budget_reservation,
    })
}

fn require_finalized_signature(
    rpc: &RpcClient,
    signature: &Signature,
) -> Result<u64, Box<dyn Error>> {
    let status = rpc
        .get_signature_statuses_with_history(&[*signature])?
        .value
        .into_iter()
        .next()
        .flatten()
        .ok_or("cleanup signature was not found after finalized confirmation")?;
    if let Some(error) = status.err.as_ref() {
        return Err(format!("cleanup transaction finalized with error: {error:?}").into());
    }
    if !status.satisfies_commitment(CommitmentConfig::finalized()) {
        return Err("cleanup transaction did not reach finalized commitment".into());
    }
    Ok(status.slot)
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
    minimum_context_slot: u64,
) -> Result<Value, Box<dyn Error>> {
    let simulation = retry_minimum_context_slot(|| {
        rpc.simulate_transaction_with_config(
            transaction,
            RpcSimulateTransactionConfig {
                sig_verify: false,
                replace_recent_blockhash: false,
                commitment: Some(CommitmentConfig::finalized()),
                min_context_slot: Some(minimum_context_slot),
                ..RpcSimulateTransactionConfig::default()
            },
        )
    })?;
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

fn retry_minimum_context_slot<T>(
    mut operation: impl FnMut() -> Result<T, ClientError>,
) -> Result<T, ClientError> {
    for attempt in 1..=MIN_CONTEXT_SLOT_MAX_ATTEMPTS {
        match operation() {
            Ok(value) => return Ok(value),
            Err(error)
                if attempt < MIN_CONTEXT_SLOT_MAX_ATTEMPTS
                    && is_minimum_context_slot_not_reached(&error) =>
            {
                std::thread::sleep(MIN_CONTEXT_SLOT_RETRY_DELAY);
            }
            Err(error) => return Err(error),
        }
    }
    unreachable!("minimum-context retry loop always returns on its final attempt")
}

fn finalized_account_lamports(
    rpc: &RpcClient,
    address: &Pubkey,
    minimum_context_slot: u64,
) -> Result<u64, Box<dyn Error>> {
    let account = retry_minimum_context_slot(|| {
        rpc.get_account_with_config(
            address,
            RpcAccountInfoConfig {
                commitment: Some(CommitmentConfig::finalized()),
                min_context_slot: Some(minimum_context_slot),
                ..RpcAccountInfoConfig::default()
            },
        )
    })?;
    Ok(account.value.map_or(0, |account| account.lamports))
}

fn is_minimum_context_slot_not_reached(error: &ClientError) -> bool {
    matches!(
        error.kind(),
        ClientErrorKind::RpcError(SolanaRpcError::RpcResponseError { code, .. })
            if *code == JSON_RPC_SERVER_ERROR_MIN_CONTEXT_SLOT_NOT_REACHED
    )
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
            "--min-slot".to_owned(),
            "123".to_owned(),
            "--max-lamports".to_owned(),
            "50000".to_owned(),
            "--budget-window-seconds".to_owned(),
            "3600".to_owned(),
            "--trace-timing".to_owned(),
        ])
        .expect("cleanup safety options should parse");

        assert!(options.execute);
        assert!(options.simulate_before_submit);
        assert!(!options.scan_program_accounts);
        assert!(options.scan_history);
        assert_eq!(options.limit, 0);
        assert_eq!(options.min_slot, Some(123));
        assert_eq!(options.max_lamports, Some(50_000));
        assert_eq!(options.budget_window_seconds, Some(3_600));
        assert_eq!(options.expected_fleet_count, Some(31));
        assert_eq!(options.bundle_size, 1);
        assert!(options.trace_timing);
    }

    #[test]
    fn alt_cleanup_execute_rejects_manual_allowlist_and_ignores_environment_protection() {
        let protected = Pubkey::new_unique();
        let environment_table = Pubkey::new_unique();
        let manual_table = Pubkey::new_unique();
        let effective = effective_cleanup_protected_tables(
            BTreeSet::from([protected]),
            &BTreeSet::from([environment_table]),
            &BTreeSet::from([manual_table]),
            true,
        );
        assert_eq!(effective, BTreeSet::from([protected]));

        let error = parse_args(vec![
            "--cluster".to_owned(),
            "localnet".to_owned(),
            "--rpc-url".to_owned(),
            "http://localhost:8899".to_owned(),
            "--execute".to_owned(),
            "--allowlist".to_owned(),
            manual_table.to_string(),
            "--expected-fleet-count".to_owned(),
            "1".to_owned(),
            "--expected-fleet-hash".to_owned(),
            "a".repeat(64),
        ])
        .expect_err("execute allowlist must not suppress an approved legacy refund");
        assert!(error.to_string().contains("rejects --allowlist"));

        let dry_run = effective_cleanup_protected_tables(
            BTreeSet::from([protected]),
            &BTreeSet::from([environment_table]),
            &BTreeSet::from([manual_table]),
            false,
        );
        assert_eq!(dry_run.len(), 3);
    }

    #[test]
    fn alt_cleanup_durable_recovery_covers_every_broadcast_crash_window() {
        assert_eq!(
            legacy_cleanup_recovery_decision(
                &PersistedCleanupSignatureState::NotFound,
                LegacyCleanupChainEffect::Unchanged,
                false,
            ),
            LegacyCleanupRecoveryDecision::Wait,
            "crash after signed persistence but before send must not resend before expiry",
        );
        assert_eq!(
            legacy_cleanup_recovery_decision(
                &PersistedCleanupSignatureState::NotFound,
                LegacyCleanupChainEffect::Unchanged,
                true,
            ),
            LegacyCleanupRecoveryDecision::ExpireAndRetry,
            "an absent signature may be replaced only after expiry and unchanged chain state",
        );
        assert_eq!(
            legacy_cleanup_recovery_decision(
                &PersistedCleanupSignatureState::FinalizedSuccess { slot: 50 },
                LegacyCleanupChainEffect::Applied { observed_slot: 50 },
                false,
            ),
            LegacyCleanupRecoveryDecision::Complete { observed_slot: 50 },
            "crash after finalized send but before DB record must complete from durable identity",
        );
        assert_eq!(
            legacy_cleanup_recovery_decision(
                &PersistedCleanupSignatureState::NotFound,
                LegacyCleanupChainEffect::Applied { observed_slot: 50 },
                true,
            ),
            LegacyCleanupRecoveryDecision::ManualReconcile,
            "an unattributed chain effect must never be treated as this attempt or resent",
        );
        assert_eq!(
            legacy_cleanup_recovery_decision(
                &PersistedCleanupSignatureState::FinalizedFailure("failed".to_owned()),
                LegacyCleanupChainEffect::Unchanged,
                false,
            ),
            LegacyCleanupRecoveryDecision::PermanentFailure,
        );
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
            "--min-slot".to_owned(),
            "123".to_owned(),
            "--max-lamports".to_owned(),
            "50000".to_owned(),
            "--budget-window-seconds".to_owned(),
            "3600".to_owned(),
        ])
        .expect_err("multi-table execute cannot share one legacy authorization fence");

        assert!(error.to_string().contains("database authorization fence"));
    }

    #[test]
    fn alt_cleanup_budget_denial_never_invokes_signing_action() {
        let reservation = LegacyLookupTableCleanupBudgetReservation {
            approved: false,
            replayed: false,
            reservation_id: None,
            cluster: "localnet".to_owned(),
            legacy_cleanup_attempt_id: 7,
            estimated_fee_lamports: 5_000,
            estimated_rent_lamports: 0,
            requested_lamports: 5_000,
            spent_lamports: 0,
            reserved_lamports: 50_000,
            charged_lamports: 50_000,
            remaining_lamports: 0,
            window_ends_at: chrono::Utc::now(),
        };
        let mut signer_invoked = false;

        let error = run_after_cleanup_budget_approval(&reservation, || {
            signer_invoked = true;
            Ok(())
        })
        .expect_err("denied cleanup budget must stop before signing");

        assert!(!signer_invoked);
        assert!(error.to_string().contains("before signing"));
    }

    fn imported_cleanup_record(
        table: Pubkey,
        authority: Pubkey,
    ) -> ImportedLegacyLookupTableCleanupRecord {
        ImportedLegacyLookupTableCleanupRecord {
            source: loyal_yield_orchestrator::LegacyLookupTableImportSource {
                id: 1,
                cluster: "localnet".to_owned(),
                scope: "legacy".to_owned(),
                table_address: table.to_string(),
                authority: authority.to_string(),
                status: "closed".to_owned(),
                durable: false,
                address_count: 0,
                address_hash: format!("{:x}", Sha256::new().finalize()),
                addresses: Vec::new(),
                legacy_kind: Some(loyal_yield_orchestrator::LegacyLookupTableKind::LegacyRoute),
                legacy_import_run_id: Some(9),
                last_extended_slot: Some(10),
                last_extended_start_index: Some(0),
                last_verified_slot: Some(10),
                last_verified_at: Some(chrono::Utc::now()),
            },
            import_fingerprint: "a".repeat(64),
            import_verified_slot: 10,
            deactivated_slot: Some(20),
            deactivate_signature: Some("deactivate-signature".to_owned()),
            closed_signature: Some("close-signature".to_owned()),
            close_recipient: Some(authority.to_string()),
            reclaimed_lamports: Some(1_000),
        }
    }

    #[test]
    fn alt_cleanup_closed_fleet_history_is_complete_and_hash_is_lifecycle_stable() {
        let table = Pubkey::new_unique();
        let authority = Pubkey::new_unique();
        let closed = imported_cleanup_record(table, authority);
        let history = HistoryScanEvidence {
            events: vec![
                HistoryEvent {
                    signature: "deactivate-signature".to_owned(),
                    slot: 20,
                    block_time: None,
                    kind: "deactivate",
                    table_address: table,
                    authority: Some(authority),
                    payer_or_recipient: None,
                    new_address_count: None,
                },
                HistoryEvent {
                    signature: "close-signature".to_owned(),
                    slot: 40,
                    block_time: None,
                    kind: "close",
                    table_address: table,
                    authority: Some(authority),
                    payer_or_recipient: Some(authority),
                    new_address_count: None,
                },
            ],
            authorities: vec![AuthorityHistoryEvidence {
                authority,
                page_count: 2,
                signature_count: 2,
                oldest_slot: Some(9),
                boundary_reached: true,
                exhausted: false,
            }],
            mutation_set_hash: "b".repeat(64),
        };
        verify_imported_cleanup_history(std::slice::from_ref(&closed), &history, Some(10))
            .expect("closed mutation history is attributable and complete");

        let mut pre_cleanup = closed.clone();
        pre_cleanup.source.status = "retiring".to_owned();
        pre_cleanup.deactivated_slot = None;
        pre_cleanup.deactivate_signature = None;
        pre_cleanup.closed_signature = None;
        pre_cleanup.close_recipient = None;
        pre_cleanup.reclaimed_lamports = None;
        assert_eq!(
            approved_imported_legacy_fleet_hash(&[closed.clone()]),
            approved_imported_legacy_fleet_hash(&[pre_cleanup]),
            "immutable fleet approval must survive partial cleanup retries"
        );

        let incomplete = HistoryScanEvidence {
            events: history.events[..1].to_vec(),
            ..history
        };
        assert!(verify_imported_cleanup_history(&[closed], &incomplete, Some(10)).is_err());
    }

    #[test]
    fn alt_cleanup_rejects_non_database_inventory_sources() {
        let table = Pubkey::new_unique();
        let program_scan_error = parse_args(vec![
            "--cluster".to_owned(),
            "localnet".to_owned(),
            "--rpc-url".to_owned(),
            "http://localhost:8899".to_owned(),
            "--scan-program-accounts".to_owned(),
        ])
        .expect_err("whole-program cleanup discovery must stay removed");
        assert!(program_scan_error
            .to_string()
            .contains("immutable imported database fleet"));

        let subset_error = parse_args(vec![
            "--cluster".to_owned(),
            "localnet".to_owned(),
            "--rpc-url".to_owned(),
            "http://localhost:8899".to_owned(),
            "--table".to_owned(),
            table.to_string(),
        ])
        .expect_err("partial table inventory must stay rejected");
        assert!(subset_error.to_string().contains("complete imported"));
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
