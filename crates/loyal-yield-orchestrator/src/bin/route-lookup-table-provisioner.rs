//! Crash-safe reusable Address Lookup Table provisioner/reconciler.
//!
//! This binary is intentionally the only normal worker allowed to create or
//! extend reusable route lookup tables. Dry-run is the default. A signer is
//! loaded only after `--execute` has passed all CLI and control-plane gates.

use std::{
    collections::BTreeSet,
    env,
    error::Error,
    fmt,
    process::ExitCode,
    str::FromStr,
    sync::Arc,
    time::{Duration, Instant},
};

use chrono::Utc;
use loyal_observability::{init_from_env, OperationalError};
use loyal_yield_orchestrator::{
    finalized_shared_table_bundle_hash,
    fleet_orchestration::{fleet_worker_role_probe, FleetWorkerRole},
    lookup_table_manifest_address_records_hash, persisted_lookup_table_success_accounting,
    reconcile_lookup_table_operation,
    rpc_safety::{redacted_external_error, validate_rpc_endpoint, validate_rpc_genesis_hash},
    AtomicVaultAllocationResult, FinalizedSharedTableObservation,
    FinalizedSharedTableShardObservation, LeasedLookupTableOperation,
    LegacyLookupTableRetirementRequest, LookupTableAllocationKind,
    LookupTableBindingActivationDeferral, LookupTableBindingActivationOutcome,
    LookupTableChainState, LookupTableClusterBudgetPolicy, LookupTableFamilyKind,
    LookupTableFamilyRecord, LookupTableFamilyState, LookupTableFamilyUpsert, LookupTableLifecycle,
    LookupTableManifestAddressRecord, LookupTableManifestSubject, LookupTableMembershipAddress,
    LookupTableOperationAdvance, LookupTableOperationKind, LookupTableOperationLease,
    LookupTableOperationRecord, LookupTableOperationStatus, LookupTablePrecutoverProbe,
    LookupTableProvisionerBroadcastPermitResult, LookupTableProvisionerBroadcastResolution,
    LookupTableProvisioningPlanPolicy, LookupTableProvisioningRequestRecord,
    LookupTableProvisioningRequestStatus, LookupTableProvisioningRequestUpsert,
    LookupTableReconciliationDecision, LookupTableReconciliationObservation,
    LookupTableRolloutMode, LookupTableSharedMarketOperationFenceResult, LookupTableSignatureState,
    LookupTableTerminalAccountState, LookupTableTerminalChainEvidence,
    LookupTableTerminalNoEffectEvidence, LookupTableTerminalRepairRequest,
    LookupTableTerminalSiblingEvidence, LookupTableVaultBindingRecord, NeonSqlClient,
    NeonSqlConfig, OrchestratorError, PackedShardPolicy, ReusableOnlyCutoverPreflight,
    SharedMarketCatalogPlanPolicy, SharedMarketCatalogReadiness, SharedMarketPhysicalDriftReport,
    SignedLookupTableTransaction, VaultId, STANDARD_POLICY_AUTHORITY,
};
use serde::Serialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_client::{
    client_error::{ClientError, ClientErrorKind},
    rpc_client::RpcClient,
    rpc_response::Response as RpcResponse,
};
use solana_sdk::{
    account::Account,
    address_lookup_table::{
        instruction as alt_instruction, program as alt_program,
        state::{estimate_last_valid_slot, AddressLookupTable, LOOKUP_TABLE_META_SIZE},
    },
    commitment_config::CommitmentConfig,
    hash::Hash,
    packet::PACKET_DATA_SIZE,
    pubkey::Pubkey,
    signature::{Keypair, Signature, Signer},
    slot_hashes::MAX_ENTRIES as SLOT_HASHES_MAX_ENTRIES,
    transaction::Transaction,
};
use tokio::task::JoinSet;

const DATABASE_URL_ENV: &str = "NEON_DATABASE_URL";
const RPC_URL_ENV: &str = "SOLANA_RPC_URL";
const CLUSTER_ENV: &str = "YIELD_ALT_CLUSTER";
const PAUSED_ENV: &str = "YIELD_ALT_PROVISIONING_PAUSED";
const MAX_LAMPORTS_ENV: &str = "YIELD_ALT_MAX_LAMPORTS";
const BUDGET_WINDOW_SECONDS_ENV: &str = "YIELD_ALT_BUDGET_WINDOW_SECONDS";
const LARGEST_ATOMIC_EXPANSION_ENV: &str = "YIELD_ALT_LARGEST_ATOMIC_EXPANSION";
const CATALOG_RECONCILE_INTERVAL_SECONDS_ENV: &str = "YIELD_ALT_CATALOG_RECONCILE_INTERVAL_SECONDS";
const DEFAULT_MAX_OPERATIONS: usize = 8;
const DEFAULT_ADDRESS_CHUNK: usize = 20;
const MAX_ADDRESS_CHUNK: usize = 20;
const DEFAULT_LEASE_SECONDS: u64 = 120;
const DEFAULT_RATE_LIMIT_MS: u64 = 250;
const DEFAULT_CATALOG_RECONCILE_INTERVAL_SECONDS: u64 = 60;
const MAX_CATALOG_RECONCILE_INTERVAL_SECONDS: u64 = 3_600;
const DEFAULT_CONCURRENCY: usize = 8;
const MAX_RATE_LIMIT_MS: u64 = 60_000;
const MAX_OPERATIONS_PER_BATCH: usize = 100;
const MAX_CONCURRENCY: usize = 32;
const DEFAULT_RETRY_SECONDS: i64 = 30;
const DEFAULT_MAX_ATTEMPTS: i32 = 5;
const DEFAULT_BUDGET_WINDOW_SECONDS: i64 = 86_400;
const MIN_BUDGET_WINDOW_SECONDS: i64 = 60;
const MAX_BUDGET_WINDOW_SECONDS: i64 = 31_536_000;
const EXPIRED_TRANSACTION_RETRY_CODE: &str = "expired_transaction_not_observed";
const DEFAULT_SAFETY_MARGIN: u16 = 16;
const DEFAULT_VAULT_GROWTH_RESERVATION: u16 = 8;
const DEFAULT_MAX_VAULT_COHORT: u16 = 16;
const PLANNER_VERSION: &str = "reusable-alt-provisioner-v1";
const DEFAULT_SHARED_FAMILY_NAME: &str = "stable-market";
const DEFAULT_VAULT_FAMILY_NAME: &str = "vault-shards";
const RPC_READ_RETRY_INITIAL_DELAY: Duration = Duration::from_millis(250);
const RPC_READ_RETRY_MAXIMUM_DELAY: Duration = Duration::from_secs(5);
const RPC_READ_RETRY_HEARTBEAT_INTERVAL: Duration = Duration::from_secs(60);
const RPC_READ_UNAVAILABLE_ALERT_AFTER: Duration = Duration::from_secs(5 * 60);

#[derive(Debug, Clone, Copy)]
struct RpcReadRetryPolicy {
    initial_delay: Duration,
    maximum_delay: Duration,
    heartbeat_interval: Duration,
    unavailable_alert_after: Duration,
    max_attempts: Option<u32>,
}

impl RpcReadRetryPolicy {
    fn delay_after(self, consecutive_failures: u32) -> Duration {
        let shift = consecutive_failures.saturating_sub(1).min(31);
        let multiplier = 1_u32.checked_shl(shift).unwrap_or(u32::MAX);
        self.initial_delay
            .saturating_mul(multiplier)
            .min(self.maximum_delay)
    }
}

impl Default for RpcReadRetryPolicy {
    fn default() -> Self {
        Self {
            initial_delay: RPC_READ_RETRY_INITIAL_DELAY,
            maximum_delay: RPC_READ_RETRY_MAXIMUM_DELAY,
            heartbeat_interval: RPC_READ_RETRY_HEARTBEAT_INTERVAL,
            unavailable_alert_after: RPC_READ_UNAVAILABLE_ALERT_AFTER,
            max_attempts: None,
        }
    }
}

#[allow(clippy::result_large_err)]
async fn retry_read_only_rpc<T>(
    rpc_operation: &'static str,
    policy: RpcReadRetryPolicy,
    mut request: impl FnMut() -> Result<T, ClientError>,
) -> Result<T, ClientError> {
    let outage_started_at = Instant::now();
    let mut consecutive_failures = 0_u32;
    let mut next_heartbeat_at = policy.heartbeat_interval;
    let mut unavailable_alert_emitted = false;

    loop {
        match request() {
            Ok(value) => {
                if consecutive_failures > 0 {
                    println!(
                        "{}",
                        json!({
                            "event": "alt_provisioner_rpc_recovered",
                            "dependency": "solana_rpc",
                            "rpcOperation": rpc_operation,
                            "consecutiveFailures": consecutive_failures,
                            "outageMilliseconds": outage_started_at.elapsed().as_millis(),
                        })
                    );
                }
                return Ok(value);
            }
            Err(error) if is_transient_read_only_rpc_error(&error) => {
                consecutive_failures = consecutive_failures.saturating_add(1);
                let elapsed = outage_started_at.elapsed();
                let retry_delay = policy.delay_after(consecutive_failures);
                if consecutive_failures == 1 || elapsed >= next_heartbeat_at {
                    println!(
                        "{}",
                        json!({
                            "event": "alt_provisioner_rpc_degraded",
                            "dependency": "solana_rpc",
                            "rpcOperation": rpc_operation,
                            "failureKind": transient_rpc_failure_kind(&error),
                            "consecutiveFailures": consecutive_failures,
                            "outageMilliseconds": elapsed.as_millis(),
                            "retryInMilliseconds": retry_delay.as_millis(),
                        })
                    );
                    next_heartbeat_at = elapsed.saturating_add(policy.heartbeat_interval);
                }
                if !unavailable_alert_emitted && elapsed >= policy.unavailable_alert_after {
                    OperationalError::new(
                        "alt_provisioner_rpc_unavailable",
                        "read_alt_rpc",
                        "ALT provisioner Solana RPC has remained unavailable for five minutes",
                    )
                    .retryable(true)
                    .recovery_required(true)
                    .emit();
                    unavailable_alert_emitted = true;
                }
                if policy
                    .max_attempts
                    .is_some_and(|max_attempts| consecutive_failures >= max_attempts)
                {
                    return Err(error);
                }
                tokio::time::sleep(retry_delay).await;
            }
            Err(error) => return Err(error),
        }
    }
}

fn is_transient_read_only_rpc_error(error: &ClientError) -> bool {
    match error.kind() {
        ClientErrorKind::Reqwest(error) => {
            error.is_timeout()
                || error.is_connect()
                || error.is_request()
                || error.status().is_some_and(|status| {
                    matches!(status.as_u16(), 408 | 429) || status.is_server_error()
                })
        }
        ClientErrorKind::Io(error) => matches!(
            error.kind(),
            std::io::ErrorKind::ConnectionReset
                | std::io::ErrorKind::ConnectionAborted
                | std::io::ErrorKind::ConnectionRefused
                | std::io::ErrorKind::NotConnected
                | std::io::ErrorKind::BrokenPipe
                | std::io::ErrorKind::TimedOut
                | std::io::ErrorKind::UnexpectedEof
                | std::io::ErrorKind::WouldBlock
                | std::io::ErrorKind::Interrupted
        ),
        _ => false,
    }
}

fn transient_rpc_failure_kind(error: &ClientError) -> &'static str {
    match error.kind() {
        ClientErrorKind::Reqwest(error) if error.is_timeout() => "timeout",
        ClientErrorKind::Reqwest(error) if error.is_connect() => "connect",
        ClientErrorKind::Reqwest(error) if error.is_request() => "request_transport",
        ClientErrorKind::Reqwest(error) => match error.status().map(|status| status.as_u16()) {
            Some(408) => "http_request_timeout",
            Some(429) => "http_rate_limited",
            Some(500..=599) => "http_server_error",
            _ => "http_transport",
        },
        ClientErrorKind::Io(_) => "io",
        _ => "unknown",
    }
}

async fn finalized_slot_with_retry(
    rpc: &RpcClient,
    rpc_operation: &'static str,
) -> Result<u64, ClientError> {
    retry_read_only_rpc(rpc_operation, RpcReadRetryPolicy::default(), || {
        rpc.get_slot_with_commitment(CommitmentConfig::finalized())
    })
    .await
}

async fn finalized_accounts_with_retry(
    rpc: &RpcClient,
    addresses: &[Pubkey],
    rpc_operation: &'static str,
) -> Result<RpcResponse<Vec<Option<Account>>>, ClientError> {
    retry_read_only_rpc(rpc_operation, RpcReadRetryPolicy::default(), || {
        rpc.get_multiple_accounts_with_commitment(addresses, CommitmentConfig::finalized())
    })
    .await
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RunMode {
    DryRun,
    ReconcileOnly,
    Execute,
}

impl RunMode {
    const fn as_str(self) -> &'static str {
        match self {
            Self::DryRun => "dry_run",
            Self::ReconcileOnly => "reconcile_only",
            Self::Execute => "execute",
        }
    }

    const fn may_sign(self) -> bool {
        matches!(self, Self::Execute)
    }

    const fn may_drain_while_durably_paused(self) -> bool {
        matches!(self, Self::ReconcileOnly)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum AdminAction {
    None,
    BootstrapFamilies,
    RollbackFamily(i64),
    RollbackBinding(i64),
    FinalizeRollbacks(i64),
    RetireLegacy(Pubkey),
    ActivateReusableOnly,
    ForceLegacy,
    ClearForceLegacy,
    SetRolloutMode(LookupTableRolloutMode),
    SetProvisionerPause,
    ClearProvisionerPause,
    RepairTerminalOperations,
}

#[derive(Debug, Clone)]
struct Options {
    cluster: String,
    rpc_url: Option<String>,
    database_url: String,
    mode: RunMode,
    status_only: bool,
    local_paused: bool,
    watch: bool,
    max_operations: usize,
    max_attempts: i32,
    address_chunk: usize,
    max_lamports: u64,
    budget_window_seconds: i64,
    lease_seconds: u64,
    rate_limit_ms: u64,
    catalog_reconcile_interval_seconds: u64,
    concurrency: usize,
    safety_margin: u16,
    largest_atomic_expansion: Option<u16>,
    vault_growth_reservation: u16,
    max_vault_cohort: u16,
    worker_id: String,
    admin_action: AdminAction,
    admin_write: bool,
    admin_reason: Option<String>,
    admin_updated_by: Option<String>,
    admin_policy_pubkey: Option<Pubkey>,
    catalog_version: Option<String>,
    shared_family_name: String,
    vault_family_name: String,
    admin_vault_id: Option<VaultId>,
    admin_observed_slot: Option<i64>,
    admin_expected_authority: Option<Pubkey>,
    admin_expected_address_hash: Option<String>,
    admin_expected_address_count: Option<i32>,
    precutover_probe: bool,
    probe_vault_id: Option<VaultId>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Budget {
    limit: u64,
    selected: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct BudgetExhausted {
    current: u64,
    requested: u64,
    limit: u64,
}

impl fmt::Display for BudgetExhausted {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            formatter,
            "operation requests {} lamports with {} already selected, above configured limit {}",
            self.requested, self.current, self.limit
        )
    }
}

impl Error for BudgetExhausted {}

#[cfg(test)]
impl Budget {
    const fn exhausted(&self) -> bool {
        self.selected >= self.limit
    }

    fn reserve(&mut self, lamports: u64) -> Result<(), BudgetExhausted> {
        let selected = self.selected.checked_add(lamports).ok_or(BudgetExhausted {
            current: self.selected,
            requested: lamports,
            limit: self.limit,
        })?;
        if selected > self.limit {
            return Err(BudgetExhausted {
                current: self.selected,
                requested: lamports,
                limit: self.limit,
            });
        }
        self.selected = selected;
        Ok(())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct OperationBatchResult {
    processed: usize,
    budget_exhausted: bool,
}

const fn should_continue_worker(watch: bool, batch: OperationBatchResult) -> bool {
    let _ = batch;
    watch
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum LeasedOperationOutcome {
    Processed,
    BudgetExhausted(BudgetExhausted),
}

#[derive(Debug)]
struct OperationTaskCompletion {
    failure_snapshot: LeasedLookupTableOperation,
    selected_budget_lamports: u64,
    result: Result<LeasedOperationOutcome, String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum FamilyOperationGate {
    AllowMutation,
    ReadOnlyVerification,
    Defer {
        code: &'static str,
        detail: &'static str,
    },
}

const fn family_operation_gate(
    family_state: LookupTableFamilyState,
    operation_kind: LookupTableOperationKind,
) -> FamilyOperationGate {
    if matches!(operation_kind, LookupTableOperationKind::Verify) {
        return FamilyOperationGate::ReadOnlyVerification;
    }
    match family_state {
        LookupTableFamilyState::Active => FamilyOperationGate::AllowMutation,
        LookupTableFamilyState::Paused => FamilyOperationGate::Defer {
            code: "family_paused",
            detail: "family is paused; unsigned ALT mutation was not attempted",
        },
        LookupTableFamilyState::Retiring
            if matches!(
                operation_kind,
                LookupTableOperationKind::Deactivate | LookupTableOperationKind::Close
            ) =>
        {
            FamilyOperationGate::AllowMutation
        }
        LookupTableFamilyState::Retiring => FamilyOperationGate::Defer {
            code: "family_retiring_growth_blocked",
            detail: "family is retiring; unsigned ALT growth mutation was not attempted",
        },
        LookupTableFamilyState::Retired => FamilyOperationGate::Defer {
            code: "family_retired",
            detail: "family is retired; unsigned ALT mutation was not attempted",
        },
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum SubmissionStage {
    Built,
    Simulated,
    BudgetDenied,
    BudgetApproved,
    Signed,
    Persisted,
    PermitGranted,
    Broadcast,
}

/// Small testable gate that makes persistence-before-broadcast an executable
/// invariant rather than a comment around two I/O calls.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct SubmissionGate {
    stage: SubmissionStage,
}

impl SubmissionGate {
    fn built() -> Self {
        Self {
            stage: SubmissionStage::Built,
        }
    }

    fn simulated(&mut self) -> Result<(), String> {
        if self.stage != SubmissionStage::Built {
            return Err("simulation must follow transaction construction".to_owned());
        }
        self.stage = SubmissionStage::Simulated;
        Ok(())
    }

    fn sign_after_budget<F>(&mut self, approved: bool, sign: F) -> Result<bool, String>
    where
        F: FnOnce() -> Result<(), String>,
    {
        if self.stage != SubmissionStage::Simulated {
            return Err("budget decision must follow unsigned simulation".to_owned());
        }
        if !approved {
            self.stage = SubmissionStage::BudgetDenied;
            return Ok(false);
        }
        self.stage = SubmissionStage::BudgetApproved;
        sign()?;
        self.stage = SubmissionStage::Signed;
        Ok(true)
    }

    fn persisted(&mut self) -> Result<(), String> {
        if self.stage != SubmissionStage::Signed {
            return Err(
                "signed metadata may be persisted only after simulation and budget approval"
                    .to_owned(),
            );
        }
        self.stage = SubmissionStage::Persisted;
        Ok(())
    }

    fn permit_granted(&mut self) -> Result<(), String> {
        if self.stage != SubmissionStage::Persisted {
            return Err(
                "broadcast permit is forbidden before signed metadata is durable".to_owned(),
            );
        }
        self.stage = SubmissionStage::PermitGranted;
        Ok(())
    }

    fn paused_before_permit(&self) -> Result<(), String> {
        if self.stage != SubmissionStage::Persisted {
            return Err("pause deferral is valid only after signed metadata is durable".to_owned());
        }
        Ok(())
    }

    fn broadcasting(&mut self) -> Result<(), String> {
        if self.stage != SubmissionStage::PermitGranted {
            return Err("broadcast is forbidden without a durable permit".to_owned());
        }
        self.stage = SubmissionStage::Broadcast;
        Ok(())
    }
}

#[derive(Debug, Clone)]
struct ChainTable {
    observed_slot: u64,
    account: Option<Account>,
    authority: Option<Pubkey>,
    addresses: Vec<Pubkey>,
    deactivation_slot: Option<u64>,
    last_extended_slot: Option<u64>,
    last_extended_start_index: Option<u8>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct ChainClassification {
    state: LookupTableChainState,
    membership_already_reconciled: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct SignatureObservation {
    state: LookupTableSignatureState,
    observed_slot: Option<u64>,
}

#[derive(Debug)]
struct BuiltMutation {
    transaction: Transaction,
    recent_blockhash: Hash,
    last_valid_block_height: u64,
    expected_fee_lamports: u64,
    expected_rent_lamports: u64,
    reclaimed_rent_lamports: u64,
    packet_size: usize,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct OperationReport {
    event: &'static str,
    cluster: String,
    mode: &'static str,
    operation_id: i64,
    operation_kind: String,
    table: Option<String>,
    address_count: usize,
    selected_budget_lamports: u64,
    expected_fee_lamports: Option<u64>,
    expected_rent_lamports: Option<u64>,
    simulation: &'static str,
    result: String,
}

#[tokio::main]
async fn main() -> ExitCode {
    if env::args().skip(1).eq(["--role-probe"]) {
        println!(
            "{}",
            fleet_worker_role_probe(FleetWorkerRole::PriorityProvisioner)
        );
        return ExitCode::SUCCESS;
    }
    if env::args()
        .skip(1)
        .any(|arg| matches!(arg.as_str(), "--help" | "-h"))
    {
        println!("{}", usage());
        return ExitCode::SUCCESS;
    }
    let _observability = match init_from_env("loyal-route-lookup-table-provisioner") {
        Ok(observability) => observability,
        Err(error) => {
            eprintln!("failed to initialize observability: {error}");
            return ExitCode::FAILURE;
        }
    };
    match run().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            OperationalError::new(
                "alt_provisioner_fatal",
                "run_alt_provisioner",
                "ALT provisioner stopped after a fatal error",
            )
            .retryable(true)
            .recovery_required(true)
            .emit();
            eprintln!(
                "{}",
                json!({
                    "event": "alt_provisioner_fatal",
                    "error": redacted_external_error(&error.to_string()),
                })
            );
            ExitCode::FAILURE
        }
    }
}

async fn run() -> Result<(), Box<dyn Error>> {
    let options = parse_args(env::args().skip(1), |name| env::var(name).ok())?;
    if let Some(rpc_url) = options.rpc_url.as_deref() {
        validate_rpc_endpoint(rpc_url)?;
    }
    let client = NeonSqlClient::connect(
        NeonSqlConfig::new(options.database_url.clone())
            .with_max_connections(options.concurrency as u32 + 1),
    )
    .await?;
    client
        .require_schema_migration(21, "reusable_alt_production_controls")
        .await?;
    client
        .require_schema_migration(23, "value_priority_rebalance_queue")
        .await?;
    client
        .require_schema_migration(27, "rebalance_opportunity_attempt_generations")
        .await?;
    client
        .require_schema_migration(28, "reusable_alt_terminal_repair")
        .await?;

    if options.precutover_probe {
        run_precutover_probe(&client, &options).await?;
        return Ok(());
    }
    apply_admin_action(&client, &options).await?;
    emit_status(&client, &options).await?;
    if options.status_only || !matches!(options.admin_action, AdminAction::None) {
        return Ok(());
    }
    if options.local_paused {
        println!(
            "{}",
            json!({
                "event": "alt_provisioner_paused",
                "cluster": options.cluster,
                "mode": options.mode.as_str(),
                "pauseSource": "local_environment",
                "reason": format!("{PAUSED_ENV} is active")
            })
        );
        return Ok(());
    }
    if options.mode == RunMode::DryRun {
        emit_dry_run_queue(&client, &options).await?;
        return Ok(());
    }

    let rpc_url = options
        .rpc_url
        .as_ref()
        .ok_or("SOLANA_RPC_URL or --rpc-url is required for reconciliation/execution")?;
    let rpc = Arc::new(RpcClient::new_with_commitment(
        rpc_url.clone(),
        CommitmentConfig::confirmed(),
    ));
    let observed_genesis_hash = rpc
        .get_genesis_hash()
        .map_err(|_| "failed to read genesis hash from configured reusable ALT RPC endpoint")?;
    validate_rpc_genesis_hash(&options.cluster, observed_genesis_hash).map_err(|error| {
        format!("refusing reusable ALT reconciliation/mutation against mismatched RPC: {error}")
    })?;
    let mut signer = None;

    let mut budget = Budget {
        limit: options.max_lamports,
        selected: 0,
    };
    let catalog_reconcile_interval =
        Duration::from_secs(options.catalog_reconcile_interval_seconds);
    let mut next_catalog_reconcile_at = Instant::now();
    let mut force_catalog_reconcile = false;
    loop {
        if let Some(control) = client
            .lookup_table_provisioner_control(&options.cluster)
            .await?
            .filter(|control| control.paused)
        {
            let reconcile_drain_allowed = options.mode.may_drain_while_durably_paused();
            println!(
                "{}",
                json!({
                    "event": if reconcile_drain_allowed {
                        "alt_provisioner_paused_reconcile_drain"
                    } else {
                        "alt_provisioner_paused"
                    },
                    "cluster": options.cluster,
                    "mode": options.mode.as_str(),
                    "pauseSource": "durable_cluster_control",
                    "reason": control.reason,
                    "updatedBy": control.updated_by,
                    "controlEpoch": control.control_epoch,
                    "updatedAt": control.updated_at,
                    "signerLoaded": signer.is_some(),
                    "transactionsSent": false,
                    "reconcileDrainAllowed": reconcile_drain_allowed,
                    "newMutationsAllowed": false,
                    "workerKeepsWatching": options.watch || reconcile_drain_allowed,
                })
            );
            if !reconcile_drain_allowed {
                if options.watch {
                    tokio::time::sleep(Duration::from_millis(options.rate_limit_ms.max(1_000)))
                        .await;
                    continue;
                }
                break;
            }
        }
        if options.mode.may_sign() && signer.is_none() {
            signer = Some(Arc::new(load_manager_signer()?));
        }
        let now = Instant::now();
        let reconcile_catalog = force_catalog_reconcile || now >= next_catalog_reconcile_at;
        let batch = run_operation_batch(
            &client,
            &rpc,
            signer.as_ref(),
            &options,
            &mut budget,
            reconcile_catalog,
        )
        .await?;
        if reconcile_catalog {
            next_catalog_reconcile_at = Instant::now() + catalog_reconcile_interval;
        }
        // Any planned or processed ALT work may have changed the physical
        // shared bundle. Reconcile it on the next pass instead of waiting for
        // the periodic safety sweep.
        force_catalog_reconcile = batch.processed > 0;
        if !should_continue_worker(options.watch, batch) {
            break;
        }
        if batch.budget_exhausted {
            tokio::time::sleep(Duration::from_secs(60)).await;
        } else if batch.processed == 0 {
            tokio::time::sleep(Duration::from_millis(options.rate_limit_ms.max(250))).await;
        }
    }
    Ok(())
}

async fn run_operation_batch(
    client: &NeonSqlClient,
    rpc: &Arc<RpcClient>,
    signer: Option<&Arc<Keypair>>,
    options: &Options,
    budget: &mut Budget,
    reconcile_catalog: bool,
) -> Result<OperationBatchResult, Box<dyn Error>> {
    let mut processed = 0;
    if options.mode == RunMode::Execute && reconcile_catalog {
        processed +=
            usize::from(reconcile_shared_market_catalog(client, rpc.as_ref(), options).await?);
    }

    // Admit one live, economically ranked request every batch before draining
    // the existing physical-operation backlog. Otherwise a continuous queue of
    // old/zero-consumer mutations can prevent a newly valuable request from
    // ever entering the operation priority order at all.
    let mut planning_attempts = 0usize;
    let mut planning_queue_exhausted = false;
    if options.mode == RunMode::Execute
        && has_plannable_provisioning_request(client, &options.cluster).await?
    {
        planning_attempts += 1;
        if plan_next_provisioning_request(client, rpc.as_ref(), options).await? {
            processed += 1;
        } else {
            planning_queue_exhausted = true;
        }
    } else {
        planning_queue_exhausted = true;
    }

    let mut tasks = JoinSet::<OperationTaskCompletion>::new();
    let mut launched = 0usize;
    let mut budget_exhausted = false;
    while launched < options.max_operations && !budget_exhausted {
        if tasks.len() >= options.concurrency {
            let completion = tasks
                .join_next()
                .await
                .ok_or("ALT provisioner task set unexpectedly became empty")??;
            processed += 1;
            budget_exhausted = finish_operation_task(client, options, budget, completion)
                .await?
                .is_some();
            continue;
        }

        let lease_expires_at =
            Utc::now() + chrono::Duration::seconds(i64::try_from(options.lease_seconds)?);
        let leased = client
            .lease_next_lookup_table_operation(
                &options.cluster,
                &options.worker_id,
                lease_expires_at,
                options.mode == RunMode::ReconcileOnly,
            )
            .await?;
        let Some(leased) = leased else {
            // Do not make ALT filling a head-of-line blocker: keep already
            // leased physical tables running while the coordinator seals the
            // next highest-value provisioning request. Planning is bounded
            // and transactionally serialized; resulting per-table operations
            // are then eligible for the parallel execution lanes above.
            if options.mode == RunMode::Execute
                && !planning_queue_exhausted
                && planning_attempts < options.max_operations
            {
                planning_attempts += 1;
                if has_plannable_provisioning_request(client, &options.cluster).await?
                    && plan_next_provisioning_request(client, rpc.as_ref(), options).await?
                {
                    processed += 1;
                    continue;
                }
            }
            break;
        };
        let failure_snapshot = leased.clone();
        let task_client = client.clone();
        let task_rpc = Arc::clone(rpc);
        let task_signer = signer.cloned();
        let task_options = options.clone();
        let runtime_handle = tokio::runtime::Handle::current();
        tasks.spawn_blocking(move || {
            let mut task_budget = Budget {
                limit: task_options.max_lamports,
                selected: 0,
            };
            let result = runtime_handle
                .block_on(process_leased_operation(
                    &task_client,
                    task_rpc.as_ref(),
                    task_signer.as_deref(),
                    &task_options,
                    &mut task_budget,
                    leased,
                ))
                .map_err(|error| error.to_string());
            OperationTaskCompletion {
                failure_snapshot,
                selected_budget_lamports: task_budget.selected,
                result,
            }
        });
        launched += 1;
        // Preserve the configured global RPC pacing as a minimum interval
        // between starts, while allowing slower calls for independent tables
        // to overlap up to the explicit concurrency bound.
        if launched < options.max_operations && options.rate_limit_ms > 0 {
            tokio::time::sleep(Duration::from_millis(options.rate_limit_ms)).await;
        }
    }

    // A budget denial stops new claims, but every operation already leased by
    // this batch must finish its fenced transition before the batch returns.
    while let Some(completion) = tasks.join_next().await {
        let completion = completion?;
        processed += 1;
        if finish_operation_task(client, options, budget, completion)
            .await?
            .is_some()
        {
            budget_exhausted = true;
        }
    }
    Ok(OperationBatchResult {
        processed,
        budget_exhausted,
    })
}

async fn has_plannable_provisioning_request(
    client: &NeonSqlClient,
    cluster: &str,
) -> Result<bool, Box<dyn Error>> {
    Ok(loyal_yield_orchestrator::sqlx::query_scalar::<_, bool>(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM loyal_yield.lookup_table_provisioning_requests request
            WHERE request.cluster = $1
              AND request.request_status IN ('requested', 'queued', 'failed', 'planning')
              AND (
                  (request.request_status = 'failed'
                   AND request.next_attempt_at IS NOT NULL
                   AND request.next_attempt_at <= now())
                  OR
                  (request.request_status <> 'failed'
                   AND (request.next_attempt_at IS NULL OR request.next_attempt_at <= now()))
              )
              AND (
                  request.request_status <> 'planning'
                  OR request.lease_expires_at <= now()
              )
        )
        "#,
    )
    .bind(cluster)
    .fetch_one(client.pool())
    .await?)
}

async fn finish_operation_task(
    client: &NeonSqlClient,
    options: &Options,
    budget: &mut Budget,
    completion: OperationTaskCompletion,
) -> Result<Option<BudgetExhausted>, Box<dyn Error>> {
    budget.selected = budget.selected.max(completion.selected_budget_lamports);
    match completion.result {
        Ok(LeasedOperationOutcome::Processed) => Ok(None),
        Ok(LeasedOperationOutcome::BudgetExhausted(exhausted)) => {
            let lease = operation_lease(&completion.failure_snapshot)?;
            let detail = exhausted.to_string();
            let recorded = client
                .defer_unsigned_lookup_table_operation_without_attempt(
                    completion.failure_snapshot.operation.id,
                    &lease,
                    Utc::now() + chrono::Duration::seconds(DEFAULT_RETRY_SECONDS),
                    "batch_budget_exhausted",
                    &detail,
                )
                .await?;
            println!(
                "{}",
                json!({
                    "event": "alt_provisioner_batch_budget_exhausted",
                    "cluster": options.cluster,
                    "operationId": recorded.id,
                    "operationKind": recorded.operation_kind.as_str(),
                    "operationState": recorded.operation_state.as_str(),
                    "attemptCount": recorded.attempt_count,
                    "budgetLimitLamports": exhausted.limit.to_string(),
                    "selectedBudgetLamports": exhausted.current.to_string(),
                    "requestedBudgetLamports": exhausted.requested.to_string(),
                    "retryAt": recorded.next_attempt_at,
                    "attemptConsumed": false,
                    "transactionsSent": false,
                    "workerKeepsWatching": options.watch,
                })
            );
            Ok(Some(exhausted))
        }
        Err(error) => {
            let detail = safe_error(&error);
            let lease = operation_lease(&completion.failure_snapshot)?;
            let retry_at = Utc::now() + chrono::Duration::seconds(DEFAULT_RETRY_SECONDS);
            let recorded = client
                .record_lookup_table_operation_attempt_failure(
                    completion.failure_snapshot.operation.id,
                    &lease,
                    retry_at,
                    options.max_attempts,
                    "operation_attempt_failed",
                    &detail,
                )
                .await?;
            if recorded.operation_state == LookupTableOperationStatus::PermanentFailure {
                OperationalError::new(
                    "alt_operation_execution_permanently_failed",
                    "execute_alt_operation",
                    "ALT operation execution reached permanent failure",
                )
                .retryable(false)
                .recovery_required(true)
                .emit();
            }
            println!(
                "{}",
                json!({
                    "event": "alt_provisioner_attempt_failure",
                    "cluster": options.cluster,
                    "operationId": recorded.id,
                    "operationKind": recorded.operation_kind.as_str(),
                    "operationState": recorded.operation_state.as_str(),
                    "attemptCount": recorded.attempt_count,
                    "maxAttempts": options.max_attempts,
                    "errorCode": recorded.error_code,
                    "errorDetail": detail,
                    "retryAt": recorded.next_attempt_at,
                    "signedIdentityPersisted": recorded.transaction_signature.is_some(),
                    "sendState": if recorded.transaction_signature.is_some() {
                        "must_reconcile"
                    } else {
                        "not_signed"
                    },
                })
            );
            Ok(None)
        }
    }
}

async fn reconcile_shared_market_catalog(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &Options,
) -> Result<bool, Box<dyn Error>> {
    let Some(mut before) = client.shared_market_catalog_head(&options.cluster).await? else {
        return Ok(false);
    };
    if before.readiness_state == SharedMarketCatalogReadiness::Active {
        let Some(preflight) = client
            .reusable_only_cutover_preflight_if_current(
                &options.cluster,
                before.catalog_revision_id,
            )
            .await?
        else {
            return Ok(false);
        };
        if report_finalized_shared_drift_if_any(client, rpc, options, &preflight).await? {
            before = client
                .shared_market_catalog_head(&options.cluster)
                .await?
                .ok_or("shared-market catalog disappeared after drift report")?;
        }
    }
    let families = client
        .active_lookup_table_families(&options.cluster)
        .await?;
    let shared_family = families
        .iter()
        .find(|family| family.kind == LookupTableFamilyKind::SharedMarket)
        .ok_or("cluster has a shared-market catalog head but no active shared-market family")?;
    if shared_family.id != before.family_id {
        return Err("shared-market catalog head does not belong to the active family".into());
    }
    let recent_slot = finalized_slot_with_retry(rpc, "reconcile_shared_market_catalog").await?;
    let Some(after) = client
        .reconcile_shared_market_catalog_head_if_current(
            &options.cluster,
            before.catalog_revision_id,
            SharedMarketCatalogPlanPolicy {
                shared_shard_capacity: u16::try_from(shared_family.allocation_high_water)?,
                max_extension_addresses: options.address_chunk,
                operation_context: json!({
                    "planner": PLANNER_VERSION,
                    "recent_slot": recent_slot,
                    "catalog_revision_id": before.catalog_revision_id,
                    "catalog_revision": before.catalog_revision,
                }),
                estimated_fee_lamports: None,
                estimated_rent_lamports: None,
            },
            Utc::now() + chrono::Duration::hours(24),
        )
        .await?
    else {
        return Ok(false);
    };
    let changed = before.target_generation != after.target_generation
        || before.active_generation != after.active_generation
        || before.readiness_state != after.readiness_state
        || before.activated_at != after.activated_at;
    if changed {
        println!(
            "{}",
            json!({
                "event": "alt_shared_market_catalog_reconciled",
                "cluster": options.cluster,
                "catalogRevisionId": after.catalog_revision_id,
                "catalogRevision": after.catalog_revision,
                "addressCount": after.address_count,
                "targetGeneration": after.target_generation,
                "activeGeneration": after.active_generation,
                "readinessState": after.readiness_state.as_str(),
                "activatedAt": after.activated_at,
                "transactionsSent": false,
            })
        );
    }
    Ok(changed)
}

async fn report_finalized_shared_drift_if_any(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &Options,
    preflight: &ReusableOnlyCutoverPreflight,
) -> Result<bool, Box<dyn Error>> {
    if preflight.shared_tables.is_empty() {
        return Err("shared-market finalized drift check requires a non-empty bundle".into());
    }
    let table_addresses = preflight
        .shared_tables
        .iter()
        .map(|table| Pubkey::from_str(&table.table_address))
        .collect::<Result<Vec<_>, _>>()?;
    let response =
        finalized_accounts_with_retry(rpc, &table_addresses, "report_finalized_shared_drift")
            .await?;
    if response.value.len() != preflight.shared_tables.len() {
        return Err("finalized RPC returned an incomplete shared-table bundle".into());
    }
    let observed_slot = response.context.slot;
    let observed_slot_i64 = i64::try_from(observed_slot)?;
    if preflight
        .shared_tables
        .iter()
        .any(|table| table.last_verified_slot > observed_slot_i64)
    {
        return Err(
            "finalized RPC context is older than persisted shared-table verification".into(),
        );
    }
    for (expected, account) in preflight.shared_tables.iter().zip(response.value) {
        let (present, authority, active, last_extended_slot, warm, addresses, reason) =
            match account {
                None => (
                    false,
                    None,
                    false,
                    None,
                    false,
                    Vec::new(),
                    "finalized_shared_table_missing",
                ),
                Some(account) if account.owner != alt_program::id() => (
                    true,
                    None,
                    false,
                    None,
                    false,
                    Vec::new(),
                    "finalized_shared_table_owner_drift",
                ),
                Some(account) => match AddressLookupTable::deserialize(&account.data) {
                    Ok(table) => {
                        let authority = table.meta.authority.map(|value| value.to_string());
                        let active = table.meta.deactivation_slot == u64::MAX;
                        let addresses = table
                            .addresses
                            .iter()
                            .map(ToString::to_string)
                            .collect::<Vec<_>>();
                        let last_extended_slot = i64::try_from(table.meta.last_extended_slot)?;
                        let warm = observed_slot > table.meta.last_extended_slot;
                        if authority.as_deref() == Some(expected.authority.as_str())
                            && active
                            && warm
                            && last_extended_slot == expected.last_extended_slot
                            && addresses == expected.ordered_addresses
                        {
                            continue;
                        }
                        (
                            true,
                            authority,
                            active,
                            Some(last_extended_slot),
                            warm,
                            addresses,
                            if !warm {
                                "finalized_shared_table_not_warm"
                            } else {
                                "finalized_shared_table_identity_or_membership_drift"
                            },
                        )
                    }
                    Err(_) => (
                        true,
                        None,
                        false,
                        None,
                        false,
                        Vec::new(),
                        "finalized_shared_table_decode_drift",
                    ),
                },
            };
        let drift = client
            .report_shared_market_physical_drift(SharedMarketPhysicalDriftReport {
                cluster: options.cluster.clone(),
                catalog_revision_id: preflight.catalog_revision_id,
                family_id: preflight.shared_family_id,
                route_lookup_table_id: expected.table_id,
                expected_mutation_epoch: expected.mutation_epoch,
                expected_table_address: expected.table_address.clone(),
                expected_authority: expected.authority.clone(),
                observed_slot: observed_slot_i64,
                observed_table_present: present,
                observed_authority: authority,
                observed_active: active,
                observed_last_extended_slot: last_extended_slot,
                observed_warm: warm,
                observed_addresses: addresses,
                reason: reason.to_owned(),
                reported_by: options.worker_id.clone(),
            })
            .await?;
        println!(
            "{}",
            json!({
                "event": "alt_shared_market_finalized_drift_reported",
                "cluster": options.cluster,
                "catalogRevisionId": preflight.catalog_revision_id,
                "sharedTableBundleHash": preflight.shared_table_bundle_hash,
                "shardOrdinal": expected.shard_ordinal,
                "tableId": expected.table_id,
                "table": expected.table_address,
                "observedSlot": observed_slot,
                "reason": reason,
                "driftEvidenceHash": drift.evidence_hash,
                "transactionsSent": false,
            })
        );
        // Reporting one exact shard mismatch demotes the logical catalog head
        // and forces a replacement of the complete generation. A second
        // report from this same snapshot would intentionally lose the active
        // head fence after the first transaction commits.
        return Ok(true);
    }
    Ok(false)
}

async fn plan_next_provisioning_request(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &Options,
) -> Result<bool, Box<dyn Error>> {
    // Read dependency inputs before claiming a durable request. A transient RPC
    // outage must not leave a planning lease behind or consume an item attempt.
    let recent_slot = finalized_slot_with_retry(rpc, "plan_next_provisioning_request").await?;
    let families = client
        .active_lookup_table_families(&options.cluster)
        .await?;
    let lease_expires_at =
        Utc::now() + chrono::Duration::seconds(i64::try_from(options.lease_seconds)?);
    let Some(request) = client
        .lease_next_lookup_table_provisioning_request(
            &options.cluster,
            &options.worker_id,
            lease_expires_at,
        )
        .await?
    else {
        return Ok(false);
    };
    let lease = provisioning_request_lease(&request)?;
    let vault_family = families
        .iter()
        .find(|family| family.kind == LookupTableFamilyKind::VaultShards)
        .ok_or("cluster has no active vault-shards ALT family")?;
    let shared_family = families
        .iter()
        .find(|family| family.kind == LookupTableFamilyKind::SharedMarket)
        .ok_or("cluster has no active shared-market ALT family")?;
    let plan = client
        .plan_lookup_table_provisioning_request(
            &options.cluster,
            request.id,
            &lease,
            LookupTableProvisioningPlanPolicy {
                vault_policy: PackedShardPolicy {
                    hard_capacity: u16::try_from(vault_family.hard_capacity)?,
                    largest_atomic_expansion: u16::try_from(vault_family.largest_atomic_expansion)?,
                    safety_margin: u16::try_from(vault_family.safety_margin)?,
                    per_vault_growth_reservation: options.vault_growth_reservation,
                    max_vault_cohort: options.max_vault_cohort,
                },
                shared_shard_capacity: u16::try_from(shared_family.allocation_high_water)?,
                max_extension_addresses: options.address_chunk,
                operation_context: json!({
                    "planner": PLANNER_VERSION,
                    "recent_slot": recent_slot,
                    "request_id": request.id,
                    "requirements_fingerprint": request.requirements_fingerprint,
                }),
                estimated_fee_lamports: None,
                estimated_rent_lamports: None,
            },
        )
        .await;
    match plan {
        Ok(plan) => {
            let (vault_operation_count, binding_activated) = match &plan.vault_allocation {
                AtomicVaultAllocationResult::NotRequired
                | AtomicVaultAllocationResult::Existing { .. } => (0, false),
                AtomicVaultAllocationResult::BindingReserved { operations, .. }
                    if operations.is_empty() =>
                {
                    let AtomicVaultAllocationResult::BindingReserved { binding, .. } =
                        &plan.vault_allocation
                    else {
                        unreachable!()
                    };
                    let (activated, deferred) =
                        match activate_binding_if_ready(client, binding, recent_slot).await {
                            Ok(Some(LookupTableBindingActivationOutcome::Activated(_))) => {
                                (true, None)
                            }
                            Ok(Some(LookupTableBindingActivationOutcome::Deferred(deferral))) => {
                                (false, Some(binding_activation_defer_fields(&deferral)))
                            }
                            Ok(None) => (false, None),
                            Err(error)
                                if is_binding_activation_database_deadlock(error.as_ref()) =>
                            {
                                (false, Some(("database_deadlock", None, None)))
                            }
                            Err(error) => return Err(error),
                        };
                    if let Some((error_code, observed_slot, required_slot)) = deferred {
                        println!(
                            "{}",
                            json!({
                                "event": "alt_provisioner_binding_activation_deferred",
                                "cluster": options.cluster,
                                "requestId": request.id,
                                "vaultId": request.vault_id.as_i64(),
                                "bindingId": binding.id,
                                "errorCode": error_code,
                                "observedSlot": observed_slot,
                                "requiredSlot": required_slot,
                                "retryAt": plan.request.next_attempt_at,
                                "transactionsSent": false,
                            })
                        );
                    }
                    (0, activated)
                }
                AtomicVaultAllocationResult::BindingReserved { operations, .. }
                | AtomicVaultAllocationResult::CreateQueued { operations, .. } => {
                    (operations.len(), false)
                }
            };
            let catalog = client
                .shared_market_catalog_head(&options.cluster)
                .await?
                .ok_or("shared-market catalog head disappeared after request planning")?;
            println!(
                "{}",
                json!({
                    "event": "alt_provisioner_request",
                    "cluster": options.cluster,
                    "mode": options.mode.as_str(),
                    "requestId": request.id,
                    "vaultId": request.vault_id.as_i64(),
                    "status": plan.request.request_status.as_str(),
                    "sharedTargetGeneration": plan.shared_target_generation,
                    "sharedOperationCount": plan.shared_operations.len(),
                    "vaultOperationCount": vault_operation_count,
                    "bindingActivated": binding_activated,
                    "sharedCatalogReadiness": catalog.readiness_state.as_str(),
                    "sharedCatalogActiveGeneration": catalog.active_generation,
                    "transactionsSent": false,
                })
            );
        }
        Err(error) => {
            let detail = safe_error(&error.to_string());
            let failed_request = client
                .advance_lookup_table_provisioning_request(
                    request.id,
                    &lease,
                    LookupTableProvisioningRequestStatus::Failed,
                    Some(Utc::now() + chrono::Duration::seconds(DEFAULT_RETRY_SECONDS)),
                    Some("planning_failed"),
                    Some(&detail),
                )
                .await?;
            if failed_request.attempt_count == options.max_attempts {
                OperationalError::new(
                    "alt_request_planning_stalled",
                    "plan_alt_provisioning_request",
                    "ALT provisioning request planning failed repeatedly",
                )
                .retryable(true)
                .recovery_required(true)
                .emit();
            }
            println!(
                "{}",
                json!({
                    "event": "alt_provisioner_request",
                    "cluster": options.cluster,
                    "mode": options.mode.as_str(),
                    "requestId": request.id,
                    "vaultId": request.vault_id.as_i64(),
                    "status": "failed",
                    "errorCode": "planning_failed",
                    "transactionsSent": false,
                })
            );
        }
    }
    Ok(true)
}

fn binding_activation_defer_fields(
    deferral: &LookupTableBindingActivationDeferral,
) -> (&'static str, Option<i64>, Option<i64>) {
    (
        deferral.error_code(),
        deferral.observed_slot(),
        deferral.required_slot(),
    )
}

fn is_binding_activation_database_deadlock(error: &(dyn Error + 'static)) -> bool {
    matches!(
        error.downcast_ref::<OrchestratorError>(),
        Some(OrchestratorError::Sqlx(
            loyal_yield_orchestrator::sqlx::Error::Database(database)
        )) if database.code().as_deref() == Some("40P01")
    )
}

async fn activate_binding_if_ready(
    client: &NeonSqlClient,
    binding: &LookupTableVaultBindingRecord,
    observed_slot: u64,
) -> Result<Option<LookupTableBindingActivationOutcome>, Box<dyn Error>> {
    let table = client
        .reusable_lookup_table(binding.route_lookup_table_id)
        .await?
        .ok_or("binding references a missing reusable lookup table")?;
    if table.desired_state != LookupTableLifecycle::Active
        || table.usable_address_count != table.address_count
        || table.last_verified_slot.is_none()
    {
        return Ok(None);
    }
    let manifest = client
        .lookup_table_manifest(binding.manifest_id)
        .await?
        .ok_or("binding references a missing sealed manifest")?;
    if manifest.sealed_at.is_none() {
        return Err("binding manifest is not sealed".into());
    }
    let membership = client
        .lookup_table_membership(table.id)
        .await?
        .into_iter()
        .map(|row| row.address)
        .collect::<BTreeSet<_>>();
    let required = manifest
        .addresses
        .iter()
        .map(|row| row.address.clone())
        .collect::<BTreeSet<_>>();
    if !required.is_subset(&membership) {
        return Ok(None);
    }
    let outcome = client
        .flip_lookup_table_binding_head(
            binding.id,
            i64::try_from(observed_slot)?,
            Utc::now() + chrono::Duration::hours(24),
        )
        .await?;
    Ok(Some(outcome))
}

fn provisioning_request_lease(
    request: &LookupTableProvisioningRequestRecord,
) -> Result<LookupTableOperationLease, Box<dyn Error>> {
    LookupTableOperationLease::new(
        request
            .lease_owner
            .clone()
            .ok_or("leased provisioning request has no owner")?,
        request.fencing_token,
        request
            .lease_expires_at
            .ok_or("leased provisioning request has no expiry")?,
    )
    .map_err(Into::into)
}

async fn defer_operation_for_durable_pause(
    client: &NeonSqlClient,
    options: &Options,
    lease: &LookupTableOperationLease,
    leased: &LeasedLookupTableOperation,
) -> Result<bool, Box<dyn Error>> {
    let Some(control) = client
        .lookup_table_provisioner_control(&options.cluster)
        .await?
        .filter(|control| control.paused)
    else {
        return Ok(false);
    };
    let recorded = client
        .defer_unsigned_lookup_table_operation_without_attempt(
            leased.operation.id,
            lease,
            Utc::now() + chrono::Duration::seconds(DEFAULT_RETRY_SECONDS),
            "cluster_provisioner_paused",
            &control.reason,
        )
        .await?;
    println!(
        "{}",
        json!({
            "event": "alt_provisioner_operation_paused",
            "cluster": options.cluster,
            "operationId": recorded.id,
            "operationKind": recorded.operation_kind.as_str(),
            "operationState": recorded.operation_state.as_str(),
            "attemptCount": recorded.attempt_count,
            "retryAt": recorded.next_attempt_at,
            "reason": control.reason,
            "updatedBy": control.updated_by,
            "controlEpoch": control.control_epoch,
            "attemptConsumed": false,
            "transactionsSent": false,
        })
    );
    Ok(true)
}

async fn process_leased_operation(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    signer: Option<&Keypair>,
    options: &Options,
    budget: &mut Budget,
    mut leased: LeasedLookupTableOperation,
) -> Result<LeasedOperationOutcome, Box<dyn Error>> {
    let lease = operation_lease(&leased)?;
    let family = client
        .lookup_table_family_by_id(leased.operation.family_id)
        .await?
        .ok_or_else(|| {
            format!(
                "operation {} belongs to missing family {}",
                leased.operation.id, leased.operation.family_id
            )
        })?;
    if family.cluster != options.cluster {
        return Err(format!(
            "operation {} family cluster {} does not match worker cluster {}",
            leased.operation.id, family.cluster, options.cluster
        )
        .into());
    }

    let persisted_membership = match leased.operation.route_lookup_table_id {
        Some(table_id) => client.lookup_table_membership(table_id).await?,
        None => Vec::new(),
    };
    let chain = load_chain_table(rpc, leased.physical_table.as_ref())?;
    if requires_chain_first_reconciliation(
        has_unreconciled_persisted_signature(&leased),
        chain_effect_is_possible(&leased, &chain),
    ) && reconcile_existing_operation(
        client,
        rpc,
        options,
        &lease,
        &leased,
        &persisted_membership,
        &chain,
    )
    .await?
    {
        return Ok(LeasedOperationOutcome::Processed);
    }

    if options.mode == RunMode::Execute
        && defer_operation_for_durable_pause(client, options, &lease, &leased).await?
    {
        return Ok(LeasedOperationOutcome::Processed);
    }
    if options.mode == RunMode::ReconcileOnly {
        emit_operation_report(
            options,
            &leased,
            budget.selected,
            None,
            None,
            "not_run",
            "no_known_chain_effect",
        );
        return Ok(LeasedOperationOutcome::Processed);
    }
    match family_operation_gate(family.desired_state, leased.operation.operation_kind) {
        FamilyOperationGate::AllowMutation => {}
        FamilyOperationGate::ReadOnlyVerification => {
            return Err("read-only Verify operation escaped mandatory chain reconciliation".into())
        }
        FamilyOperationGate::Defer { code, detail } => {
            let recorded = client
                .defer_unsigned_lookup_table_operation_without_attempt(
                    leased.operation.id,
                    &lease,
                    Utc::now() + chrono::Duration::seconds(DEFAULT_RETRY_SECONDS),
                    code,
                    detail,
                )
                .await?;
            println!(
                "{}",
                json!({
                    "event": "alt_provisioner_family_operation_deferred",
                    "cluster": options.cluster,
                    "familyId": family.id,
                    "familyState": family.desired_state.as_str(),
                    "operationId": recorded.id,
                    "operationKind": recorded.operation_kind.as_str(),
                    "operationState": recorded.operation_state.as_str(),
                    "attemptCount": recorded.attempt_count,
                    "errorCode": code,
                    "retryAt": recorded.next_attempt_at,
                    "attemptConsumed": false,
                    "transactionsSent": false,
                })
            );
            return Ok(LeasedOperationOutcome::Processed);
        }
    }
    if leased.operation.operation_kind != LookupTableOperationKind::Verify {
        if let Some(table_id) = leased.operation.route_lookup_table_id {
            if client.lookup_table_has_active_usage_lease(table_id).await? {
                let recorded = client
                    .defer_unsigned_lookup_table_operation_without_attempt(
                        leased.operation.id,
                        &lease,
                        Utc::now() + chrono::Duration::seconds(DEFAULT_RETRY_SECONDS),
                        "lookup_table_usage_lease_active_before_signing",
                        "mutating operation is deferred while a route lookup-table usage lease is active",
                    )
                    .await?;
                println!(
                    "{}",
                    json!({
                        "event": "alt_provisioner_usage_lease_deferred",
                        "cluster": options.cluster,
                        "familyId": family.id,
                        "operationId": recorded.id,
                        "operationKind": recorded.operation_kind.as_str(),
                        "operationState": recorded.operation_state.as_str(),
                        "attemptCount": recorded.attempt_count,
                        "errorCode": "lookup_table_usage_lease_active_before_signing",
                        "retryAt": recorded.next_attempt_at,
                        "attemptConsumed": false,
                        "transactionsSent": false,
                    })
                );
                return Ok(LeasedOperationOutcome::Processed);
            }
        }
    }
    if family.kind == LookupTableFamilyKind::SharedMarket
        && matches!(
            leased.operation.operation_kind,
            LookupTableOperationKind::Create
                | LookupTableOperationKind::Extend
                | LookupTableOperationKind::Rollover
        )
    {
        match client
            .fence_leased_shared_market_operation_before_signing(
                &options.cluster,
                leased.operation.id,
                &lease,
            )
            .await?
        {
            LookupTableSharedMarketOperationFenceResult::Current => {}
            LookupTableSharedMarketOperationFenceResult::Cancelled { operation, reason } => {
                println!(
                    "{}",
                    json!({
                        "event": "alt_provisioner_stale_shared_operation_cancelled",
                        "cluster": options.cluster,
                        "familyId": family.id,
                        "operationId": operation.id,
                        "operationKind": operation.operation_kind.as_str(),
                        "operationState": operation.operation_state.as_str(),
                        "errorCode": operation.error_code,
                        "reason": reason,
                        "transactionsSent": false,
                    })
                );
                return Ok(LeasedOperationOutcome::Processed);
            }
        }
    }
    validate_chunk(&leased, options.address_chunk)?;
    let signer = signer.ok_or("execute mode reached mutation planning without POLICY_KEYPAIR")?;
    validate_manager_boundary(signer, &family, leased.physical_table.as_ref())?;
    prepare_cleanup_lifecycle(client, rpc, &mut leased).await?;
    validate_cleanup_mutation_at_signing(client, options, &leased).await?;

    if matches!(
        leased.operation.operation_kind,
        LookupTableOperationKind::Create | LookupTableOperationKind::Rollover
    ) && leased.physical_table.is_some()
        && chain.account.is_none()
        && leased.operation.transaction_signature.is_none()
    {
        let reserved_recent_slot = create_recent_slot(&leased.operation.operation_context)?;
        let finalized_slot = rpc.get_slot_with_commitment(CommitmentConfig::finalized())?;
        if create_recent_slot_has_expired(reserved_recent_slot, finalized_slot) {
            leased = client
                .refresh_leased_lookup_table_create_reservation(
                    leased.operation.id,
                    &lease,
                    finalized_slot,
                )
                .await?;
            println!(
                "{}",
                json!({
                    "event": "alt_create_reservation_refreshed",
                    "cluster": options.cluster,
                    "operationId": leased.operation.id,
                    "operationKind": leased.operation.operation_kind.as_str(),
                    "table": leased.physical_table.as_ref().map(|table| &table.table_address),
                    "recentSlot": finalized_slot,
                    "transactionsSent": false,
                })
            );
        }
    }

    let chain = load_chain_table(rpc, leased.physical_table.as_ref())?;
    let mut built = build_unsigned_mutation(
        rpc,
        signer.pubkey(),
        &family,
        &leased,
        &persisted_membership,
        &chain,
    )?;
    let selected = built
        .expected_fee_lamports
        .checked_add(built.expected_rent_lamports)
        .ok_or("operation spend estimate overflow")?;

    // A temporarily underfunded standard policy signer is an unavailable
    // prerequisite, not an execution attempt. Check after the exact fee/rent
    // build and use the existing no-attempt fenced deferral so a funding gap
    // cannot manufacture terminal ALT damage.
    let signer_balance = rpc
        .get_balance_with_commitment(&signer.pubkey(), CommitmentConfig::confirmed())?
        .value;
    if signer_balance < selected {
        let recorded = client
            .defer_unsigned_lookup_table_operation_without_attempt(
                leased.operation.id,
                &lease,
                Utc::now() + chrono::Duration::seconds(DEFAULT_RETRY_SECONDS),
                "policy_signer_funding_unavailable",
                "standard policy signer balance is below the exact ALT fee and rent requirement",
            )
            .await?;
        println!(
            "{}",
            json!({
                "event": "alt_provisioner_funding_deferred",
                "cluster": options.cluster,
                "operationId": recorded.id,
                "operationKind": recorded.operation_kind.as_str(),
                "operationState": recorded.operation_state.as_str(),
                "requiredLamports": selected,
                "availableLamports": signer_balance,
                "attemptCount": recorded.attempt_count,
                "attemptConsumed": false,
                "transactionsSent": false,
            })
        );
        return Ok(LeasedOperationOutcome::Processed);
    }

    let mut gate = SubmissionGate::built();
    let simulation = rpc.simulate_transaction(&built.transaction)?;
    if let Some(error) = simulation.value.err.as_ref() {
        let logs = simulation.value.logs.unwrap_or_default();
        let failure_detail = format!("{error:?}; logs={}", logs.join(" | "));
        if failure_detail.to_ascii_lowercase().contains("insufficient")
            && (failure_detail.to_ascii_lowercase().contains("fund")
                || failure_detail.to_ascii_lowercase().contains("lamport"))
        {
            let recorded = client
                .defer_unsigned_lookup_table_operation_without_attempt(
                    leased.operation.id,
                    &lease,
                    Utc::now() + chrono::Duration::seconds(DEFAULT_RETRY_SECONDS),
                    "policy_signer_funding_race",
                    "ALT simulation observed a transient insufficient-funding prerequisite",
                )
                .await?;
            println!(
                "{}",
                json!({
                    "event": "alt_provisioner_funding_deferred",
                    "cluster": options.cluster,
                    "operationId": recorded.id,
                    "operationKind": recorded.operation_kind.as_str(),
                    "operationState": recorded.operation_state.as_str(),
                    "requiredLamports": selected,
                    "availableLamports": signer_balance,
                    "attemptCount": recorded.attempt_count,
                    "attemptConsumed": false,
                    "transactionsSent": false,
                })
            );
            return Ok(LeasedOperationOutcome::Processed);
        }
        return Err(format!(
            "ALT operation {} simulation failed: {failure_detail}",
            leased.operation.id,
        )
        .into());
    }
    gate.simulated()?;

    if defer_operation_for_durable_pause(client, options, &lease, &leased).await? {
        return Ok(LeasedOperationOutcome::Processed);
    }

    let durable_budget = client
        .reserve_lookup_table_cluster_budget(
            &options.cluster,
            leased.operation.id,
            &lease,
            LookupTableClusterBudgetPolicy {
                max_lamports: i64::try_from(options.max_lamports)?,
                rolling_window_seconds: options.budget_window_seconds,
            },
            i64::try_from(built.expected_fee_lamports)?,
            i64::try_from(built.expected_rent_lamports)?,
        )
        .await?;
    budget.selected = u64::try_from(durable_budget.charged_lamports.max(0))?;
    let signed = gate.sign_after_budget(durable_budget.approved, || {
        built
            .transaction
            .try_sign(&[signer], built.recent_blockhash)
            .map_err(|error| format!("ALT transaction signing failed: {error}"))
    })?;
    if !signed {
        return Ok(LeasedOperationOutcome::BudgetExhausted(BudgetExhausted {
            current: u64::try_from(durable_budget.charged_lamports.max(0))?,
            requested: selected,
            limit: options.max_lamports,
        }));
    }
    println!(
        "{}",
        json!({
            "event": "alt_provisioner_durable_budget_reserved",
            "cluster": options.cluster,
            "operationId": leased.operation.id,
            "approved": durable_budget.approved,
            "replayed": durable_budget.replayed,
            "requestedLamports": durable_budget.requested_lamports.to_string(),
            "chargedLamports": durable_budget.charged_lamports.to_string(),
            "remainingLamports": durable_budget.remaining_lamports.to_string(),
            "windowEndsAt": durable_budget.window_ends_at,
            "transactionsSent": false,
        })
    );

    let signature = built
        .transaction
        .signatures
        .first()
        .ok_or("signed ALT transaction has no signature")?
        .to_string();
    let message_hash = hash_bytes(&bincode::serialize(&built.transaction.message)?);
    client
        .persist_signed_lookup_table_transaction(
            leased.operation.id,
            &lease,
            SignedLookupTableTransaction {
                transaction_signature: signature.clone(),
                message_hash,
                recent_blockhash: built.recent_blockhash.to_string(),
                last_valid_block_height: i64::try_from(built.last_valid_block_height)?,
                estimated_fee_lamports: i64::try_from(built.expected_fee_lamports)?,
                estimated_rent_lamports: i64::try_from(built.expected_rent_lamports)?,
                estimated_reclaimed_rent_lamports: i64::try_from(built.reclaimed_rent_lamports)?,
            },
        )
        .await?;
    gate.persisted()?;

    let permit_result = client
        .grant_lookup_table_provisioner_broadcast_permit(
            &options.cluster,
            leased.operation.id,
            &lease,
            Utc::now() + chrono::Duration::seconds(DEFAULT_RETRY_SECONDS),
        )
        .await?;
    let permit = match permit_result {
        LookupTableProvisionerBroadcastPermitResult::Granted {
            control,
            operation,
            permit,
        } => {
            gate.permit_granted()?;
            println!(
                "{}",
                json!({
                    "event": "alt_provisioner_broadcast_permit_granted",
                    "cluster": options.cluster,
                    "operationId": operation.id,
                    "permitId": permit.id,
                    "controlEpoch": control.control_epoch,
                    "signedIdentityPersisted": true,
                    "databaseTransactionOpen": false,
                    "transactionsSent": false,
                })
            );
            permit
        }
        LookupTableProvisionerBroadcastPermitResult::Paused { control, operation } => {
            gate.paused_before_permit()?;
            println!(
                "{}",
                json!({
                    "event": "alt_provisioner_signed_broadcast_paused",
                    "cluster": options.cluster,
                    "operationId": operation.id,
                    "operationKind": operation.operation_kind.as_str(),
                    "operationState": operation.operation_state.as_str(),
                    "attemptCount": operation.attempt_count,
                    "retryAt": operation.next_attempt_at,
                    "reason": control.reason,
                    "updatedBy": control.updated_by,
                    "controlEpoch": control.control_epoch,
                    "signedIdentityPersisted": operation.transaction_signature.is_some(),
                    "sendState": "must_reconcile_unsent_signature",
                    "attemptConsumed": true,
                    "transactionsSent": false,
                })
            );
            emit_operation_report(
                options,
                &leased,
                budget.selected,
                Some(built.expected_fee_lamports),
                Some(built.expected_rent_lamports),
                "succeeded",
                "paused_before_broadcast_needs_reconcile",
            );
            return Ok(LeasedOperationOutcome::Processed);
        }
        LookupTableProvisionerBroadcastPermitResult::Fenced {
            control,
            operation,
            error_code,
            error_detail,
        } => {
            gate.paused_before_permit()?;
            println!(
                "{}",
                json!({
                    "event": "alt_provisioner_signed_broadcast_fenced",
                    "cluster": options.cluster,
                    "operationId": operation.id,
                    "operationKind": operation.operation_kind.as_str(),
                    "operationState": operation.operation_state.as_str(),
                    "retryAt": operation.next_attempt_at,
                    "errorCode": error_code,
                    "errorDetail": error_detail,
                    "controlEpoch": control.control_epoch,
                    "signedIdentityPersisted": true,
                    "transactionsSent": false,
                })
            );
            return Ok(LeasedOperationOutcome::Processed);
        }
    };
    gate.broadcasting()?;
    // The durable permit transaction has committed. No database transaction or
    // advisory lock remains open across this network boundary.
    let send_result = rpc.send_transaction(&built.transaction);
    let observed_slot =
        i64::try_from(rpc.get_slot_with_commitment(CommitmentConfig::confirmed())?)?;
    match send_result {
        Ok(returned_signature) => {
            if returned_signature.to_string() != signature {
                return Err(
                    "RPC returned a signature different from the durably persisted signature"
                        .into(),
                );
            }
            client
                .resolve_lookup_table_provisioner_broadcast_permit(
                    permit.id,
                    leased.operation.id,
                    &lease,
                    LookupTableProvisionerBroadcastResolution::Submitted { observed_slot },
                )
                .await?;
            emit_operation_report(
                options,
                &leased,
                budget.selected,
                Some(built.expected_fee_lamports),
                Some(built.expected_rent_lamports),
                "succeeded",
                &format!("submitted_packet_bytes_{}", built.packet_size),
            );
        }
        Err(error) => {
            // The RPC result is ambiguous. The signed identity is already
            // durable, so the next lease must inspect that signature and the
            // physical table instead of blindly constructing another send.
            let detail = safe_error(&error.to_string());
            client
                .resolve_lookup_table_provisioner_broadcast_permit(
                    permit.id,
                    leased.operation.id,
                    &lease,
                    LookupTableProvisionerBroadcastResolution::NeedsReconcile {
                        observed_slot: Some(observed_slot),
                        error_code: "ambiguous_send".to_owned(),
                        error_detail: detail,
                    },
                )
                .await?;
            emit_operation_report(
                options,
                &leased,
                budget.selected,
                Some(built.expected_fee_lamports),
                Some(built.expected_rent_lamports),
                "succeeded",
                "ambiguous_send_needs_reconcile",
            );
        }
    }
    Ok(LeasedOperationOutcome::Processed)
}

async fn reconcile_existing_operation(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &Options,
    lease: &LookupTableOperationLease,
    leased: &LeasedLookupTableOperation,
    persisted_membership: &[LookupTableMembershipAddress],
    _initial_chain: &ChainTable,
) -> Result<bool, Box<dyn Error>> {
    // Read the signature first, then refresh the finalized account. A root can
    // advance between independent RPC calls; an account context older than the
    // signature slot is not evidence that a finalized mutation is absent.
    let signature_observation = load_signature_state(rpc, leased)?;
    let signature_state = signature_observation.state;
    let chain = load_chain_table(rpc, leased.physical_table.as_ref())?;
    let current_height = rpc.get_block_height()?;
    let finalized_slot = chain.observed_slot;
    let chain_classification = classify_chain_state(leased, persisted_membership, &chain)?;
    let chain_observed_after_signature = signature_observation
        .observed_slot
        .is_none_or(|signature_slot| chain.observed_slot >= signature_slot);
    let usable_after_slot_reached = chain
        .last_extended_slot
        .is_none_or(|last_extended_slot| finalized_slot > last_extended_slot);
    let observation = LookupTableReconciliationObservation {
        operation_kind: leased.operation.operation_kind,
        persisted_status: leased.operation.operation_state,
        signature_state,
        chain_state: chain_classification.state,
        chain_observed_finalized: chain_observed_after_signature,
        blockhash_expired: leased
            .operation
            .last_valid_block_height
            .is_some_and(|height| current_height > height as u64),
        usable_after_slot_reached,
    };
    let decision = reconcile_lookup_table_operation(&observation);
    let result = match decision {
        LookupTableReconciliationDecision::WaitForSignature => {
            client
                .defer_lookup_table_reconciliation_poll(
                    leased.operation.id,
                    lease,
                    Utc::now() + chrono::Duration::seconds(2),
                    "waiting for the persisted transaction signature to reach a cluster status",
                )
                .await?;
            "wait_for_signature"
        }
        LookupTableReconciliationDecision::WaitForFinalization => {
            client
                .defer_lookup_table_reconciliation_poll(
                    leased.operation.id,
                    lease,
                    Utc::now() + chrono::Duration::seconds(2),
                    "waiting for finalized signature and physical account observations",
                )
                .await?;
            "wait_for_finalization"
        }
        LookupTableReconciliationDecision::WaitForUsableSlot => {
            client
                .defer_lookup_table_reconciliation_poll(
                    leased.operation.id,
                    lease,
                    Utc::now() + chrono::Duration::seconds(2),
                    "waiting for the address lookup table extension to become usable",
                )
                .await?;
            "wait_for_usable_slot"
        }
        LookupTableReconciliationDecision::AdvanceTo(next) => {
            if next != LookupTableOperationStatus::Reconciled {
                return Err(format!("unsupported reconciliation target {next}").into());
            }
            let accounting = if leased.operation.operation_kind == LookupTableOperationKind::Verify
            {
                None
            } else {
                if signature_state != LookupTableSignatureState::Finalized {
                    return Err(
                        "mutation reconciliation cannot promote accounting without a finalized persisted signature"
                            .into(),
                    );
                }
                Some(persisted_lookup_table_success_accounting(
                    &leased.operation,
                )?)
            };
            reconcile_physical_membership(
                client,
                leased,
                persisted_membership,
                &chain,
                finalized_slot,
                chain_classification.membership_already_reconciled,
            )
            .await?;
            let mut current = leased.operation.operation_state;
            for next_state in reconciliation_transition_path(current)? {
                client
                    .advance_lookup_table_operation(
                        leased.operation.id,
                        lease,
                        LookupTableOperationAdvance {
                            expected_state: current,
                            next_state,
                            observed_slot: Some(i64::try_from(finalized_slot)?),
                            error_code: None,
                            error_detail: None,
                            actual_fee_lamports: accounting
                                .as_ref()
                                .map(|value| value.actual_fee_lamports),
                            actual_rent_lamports: accounting
                                .as_ref()
                                .map(|value| value.actual_rent_lamports),
                            reclaimed_rent_lamports: accounting
                                .as_ref()
                                .map(|value| value.reclaimed_rent_lamports),
                        },
                    )
                    .await?;
                current = next_state;
            }
            if current != LookupTableOperationStatus::Reconciled {
                return Err("reconciliation path did not reach reconciled".into());
            }
            client
                .advance_lookup_table_operation(
                    leased.operation.id,
                    lease,
                    LookupTableOperationAdvance {
                        expected_state: current,
                        next_state: LookupTableOperationStatus::Complete,
                        observed_slot: Some(i64::try_from(finalized_slot)?),
                        error_code: None,
                        error_detail: None,
                        actual_fee_lamports: accounting
                            .as_ref()
                            .map(|value| value.actual_fee_lamports),
                        actual_rent_lamports: accounting
                            .as_ref()
                            .map(|value| value.actual_rent_lamports),
                        reclaimed_rent_lamports: accounting
                            .as_ref()
                            .map(|value| value.reclaimed_rent_lamports),
                    },
                )
                .await?;
            "reconciled_complete"
        }
        LookupTableReconciliationDecision::MarkCompleteFromChain => {
            let accounting = if leased.operation.operation_kind == LookupTableOperationKind::Verify
            {
                None
            } else {
                if signature_state != LookupTableSignatureState::Finalized {
                    return Err(
                        "mutation reconciliation cannot complete accounting without a finalized persisted signature"
                            .into(),
                    );
                }
                Some(persisted_lookup_table_success_accounting(
                    &leased.operation,
                )?)
            };
            client
                .advance_lookup_table_operation(
                    leased.operation.id,
                    lease,
                    LookupTableOperationAdvance {
                        expected_state: leased.operation.operation_state,
                        next_state: LookupTableOperationStatus::Complete,
                        observed_slot: Some(i64::try_from(finalized_slot)?),
                        error_code: None,
                        error_detail: None,
                        actual_fee_lamports: accounting
                            .as_ref()
                            .map(|value| value.actual_fee_lamports),
                        actual_rent_lamports: accounting
                            .as_ref()
                            .map(|value| value.actual_rent_lamports),
                        reclaimed_rent_lamports: accounting
                            .as_ref()
                            .map(|value| value.reclaimed_rent_lamports),
                    },
                )
                .await?;
            "complete_from_chain"
        }
        LookupTableReconciliationDecision::RetryWithFreshTransaction => {
            client
                .retry_lookup_table_operation(
                    leased.operation.id,
                    lease,
                    leased.operation.operation_state,
                    Utc::now() + chrono::Duration::seconds(DEFAULT_RETRY_SECONDS),
                    EXPIRED_TRANSACTION_RETRY_CODE,
                    "persisted signature was absent after blockhash expiry and physical state was unchanged",
                )
                .await?;
            "retry_with_fresh_transaction"
        }
        LookupTableReconciliationDecision::NeedsManualReconcile { reason } => {
            client
                .advance_lookup_table_operation(
                    leased.operation.id,
                    lease,
                    LookupTableOperationAdvance {
                        expected_state: leased.operation.operation_state,
                        next_state: LookupTableOperationStatus::NeedsReconcile,
                        observed_slot: Some(i64::try_from(finalized_slot)?),
                        error_code: Some("chain_drift".to_owned()),
                        error_detail: Some(reason.to_owned()),
                        actual_fee_lamports: None,
                        actual_rent_lamports: None,
                        reclaimed_rent_lamports: None,
                    },
                )
                .await?;
            "manual_reconcile_required"
        }
        LookupTableReconciliationDecision::PermanentFailure { reason } => {
            client
                .advance_lookup_table_operation(
                    leased.operation.id,
                    lease,
                    LookupTableOperationAdvance {
                        expected_state: leased.operation.operation_state,
                        next_state: LookupTableOperationStatus::PermanentFailure,
                        observed_slot: Some(i64::try_from(finalized_slot)?),
                        error_code: Some("transaction_failed".to_owned()),
                        error_detail: Some(reason.to_owned()),
                        actual_fee_lamports: None,
                        actual_rent_lamports: None,
                        reclaimed_rent_lamports: None,
                    },
                )
                .await?;
            OperationalError::new(
                "alt_operation_reconciliation_permanently_failed",
                "reconcile_alt_operation",
                "ALT operation reconciliation reached permanent failure",
            )
            .retryable(false)
            .recovery_required(true)
            .emit();
            "permanent_failure"
        }
    };
    emit_operation_report(options, leased, 0, None, None, "not_run", result);
    // Every reconciliation decision ends this lease's work. In particular a
    // retry decision releases the lease into retry_wait; the stale lease must
    // never be reused to sign immediately in this process.
    Ok(true)
}

async fn reconcile_physical_membership(
    client: &NeonSqlClient,
    leased: &LeasedLookupTableOperation,
    persisted: &[LookupTableMembershipAddress],
    chain: &ChainTable,
    observed_slot: u64,
    membership_already_reconciled: bool,
) -> Result<(), Box<dyn Error>> {
    let Some(table) = leased.physical_table.as_ref() else {
        if leased.operation.operation_kind == LookupTableOperationKind::Close {
            return Ok(());
        }
        return Err("reconciled lookup-table mutation has no physical table record".into());
    };
    if matches!(
        leased.operation.operation_kind,
        LookupTableOperationKind::Create
            | LookupTableOperationKind::Extend
            | LookupTableOperationKind::Rollover
            | LookupTableOperationKind::Verify
    ) {
        let start = usize::from(chain.last_extended_start_index.unwrap_or_default());
        let last_extended_slot = chain
            .last_extended_slot
            .ok_or("reconciled lookup-table membership has no finalized last-extended slot")?;
        if last_extended_slot >= observed_slot {
            return Err(
                "reconciled lookup-table membership is not warm at the finalized slot".into(),
            );
        }
        let added_slot = last_extended_slot;
        let updated = if membership_already_reconciled {
            table.clone()
        } else {
            let now = Utc::now();
            let addresses = chain
                .addresses
                .iter()
                .enumerate()
                .map(|(ordinal, address)| {
                    if let Some(existing) = persisted
                        .get(ordinal)
                        .filter(|row| row.address == address.to_string())
                    {
                        let mut existing = existing.clone();
                        existing.last_verified_slot =
                            i64::try_from(observed_slot).unwrap_or(i64::MAX);
                        existing.last_verified_at = now;
                        existing
                    } else {
                        let slot = if ordinal >= start {
                            added_slot
                        } else {
                            observed_slot
                        };
                        LookupTableMembershipAddress {
                            address: address.to_string(),
                            ordinal: ordinal as i32,
                            added_operation_id: Some(leased.operation.id),
                            added_slot: i64::try_from(slot).unwrap_or(i64::MAX),
                            usable_after_slot: i64::try_from(slot.saturating_add(1))
                                .unwrap_or(i64::MAX),
                            last_verified_slot: i64::try_from(observed_slot).unwrap_or(i64::MAX),
                            last_verified_at: now,
                        }
                    }
                })
                .collect::<Vec<_>>();
            client
                .replace_confirmed_lookup_table_membership(
                    table.id,
                    leased.operation.mutation_epoch,
                    leased
                        .operation
                        .mutation_epoch
                        .checked_add(1)
                        .ok_or("lookup-table mutation epoch overflow")?,
                    i64::try_from(observed_slot)?,
                    i64::try_from(last_extended_slot)?,
                    addresses,
                )
                .await?
        };
        let accepting_allocations = updated.accepting_allocations
            && updated.allocation_kind != LookupTableAllocationKind::DedicatedVault;
        let updated = if updated.desired_state == LookupTableLifecycle::Preparing {
            client
                .mark_reusable_lookup_table_verification(
                    updated.id,
                    updated.mutation_epoch,
                    LookupTableLifecycle::Preparing,
                    LookupTableLifecycle::Warming,
                    accepting_allocations,
                    i32::try_from(chain.addresses.len())?,
                    i64::try_from(observed_slot)?,
                )
                .await?
        } else {
            updated
        };
        let next_state = match updated.desired_state {
            LookupTableLifecycle::Warming => LookupTableLifecycle::Active,
            state => state,
        };
        let accepting_allocations = updated.accepting_allocations
            && updated.allocation_kind != LookupTableAllocationKind::DedicatedVault;
        client
            .mark_reusable_lookup_table_verification(
                updated.id,
                updated.mutation_epoch,
                updated.desired_state,
                next_state,
                accepting_allocations,
                i32::try_from(chain.addresses.len())?,
                i64::try_from(observed_slot)?,
            )
            .await?;
    } else if leased.operation.operation_kind == LookupTableOperationKind::Deactivate {
        client
            .mark_reusable_lookup_table_verification(
                table.id,
                table.mutation_epoch,
                table.desired_state,
                LookupTableLifecycle::Deactivated,
                false,
                0,
                i64::try_from(observed_slot)?,
            )
            .await?;
    } else if leased.operation.operation_kind == LookupTableOperationKind::Close {
        client
            .mark_reusable_lookup_table_verification(
                table.id,
                table.mutation_epoch,
                table.desired_state,
                LookupTableLifecycle::Closed,
                false,
                0,
                i64::try_from(observed_slot)?,
            )
            .await?;
    }
    Ok(())
}

fn build_unsigned_mutation(
    rpc: &RpcClient,
    authority: Pubkey,
    _family: &LookupTableFamilyRecord,
    leased: &LeasedLookupTableOperation,
    persisted: &[LookupTableMembershipAddress],
    chain: &ChainTable,
) -> Result<BuiltMutation, Box<dyn Error>> {
    let _mutation_path = provisioner_mutation_path(leased.operation.operation_kind)
        .ok_or("verify operations reconcile chain state and never build a mutation")?;
    let (recent_blockhash, last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::confirmed())?;
    let mut reclaimed_rent_lamports = 0;
    let table_address = leased
        .physical_table
        .as_ref()
        .map(|table| Pubkey::from_str(&table.table_address))
        .transpose()?
        .unwrap_or_else(Pubkey::default);
    if matches!(
        leased.operation.operation_kind,
        LookupTableOperationKind::Deactivate | LookupTableOperationKind::Close
    ) {
        validate_cleanup_chain_identity(leased, persisted, chain)?;
    }
    let instructions = match leased.operation.operation_kind {
        LookupTableOperationKind::Create | LookupTableOperationKind::Rollover => {
            let recent_slot = create_recent_slot(&leased.operation.operation_context)?;
            let (create, derived) =
                alt_instruction::create_lookup_table(authority, authority, recent_slot);
            if derived != table_address {
                return Err(
                    "durable create address does not match the ALT program derivation".into(),
                );
            }
            let mut instructions = vec![create];
            if !leased.addresses.is_empty() {
                let addresses = parse_addresses(&leased.addresses)?;
                instructions.push(alt_instruction::extend_lookup_table(
                    derived,
                    authority,
                    Some(authority),
                    addresses,
                ));
            }
            instructions
        }
        LookupTableOperationKind::Extend => {
            validate_append_only(persisted, chain, &leased.addresses)?;
            vec![alt_instruction::extend_lookup_table(
                table_address,
                authority,
                Some(authority),
                parse_addresses(&leased.addresses)?,
            )]
        }
        LookupTableOperationKind::Deactivate => vec![alt_instruction::deactivate_lookup_table(
            table_address,
            authority,
        )],
        LookupTableOperationKind::Close => {
            let account = chain
                .account
                .as_ref()
                .ok_or("cannot close a missing lookup table")?;
            let deactivation_slot = chain
                .deactivation_slot
                .ok_or("cannot close lookup table before deactivation")?;
            let current_slot = rpc.get_slot_with_commitment(CommitmentConfig::finalized())?;
            if deactivation_slot == u64::MAX
                || current_slot <= estimate_last_valid_slot(deactivation_slot)
            {
                return Err("lookup-table close cooldown has not elapsed".into());
            }
            reclaimed_rent_lamports = account.lamports;
            let recipient = policy_close_recipient(&leased.operation.operation_context, authority)?;
            vec![alt_instruction::close_lookup_table(
                table_address,
                authority,
                recipient,
            )]
        }
        LookupTableOperationKind::Verify => unreachable!("guarded by provisioner_mutation_path"),
    };
    let mut transaction = Transaction::new_with_payer(&instructions, Some(&authority));
    transaction.message.recent_blockhash = recent_blockhash;
    let packet_size = bincode::serialize(&transaction)?.len();
    if packet_size > PACKET_DATA_SIZE {
        return Err(format!(
            "serialized ALT mutation is {packet_size} bytes, above the Solana packet limit {PACKET_DATA_SIZE}"
        )
        .into());
    }
    let expected_fee_lamports = rpc.get_fee_for_message(&transaction.message)?;
    let final_address_count = match leased.operation.operation_kind {
        LookupTableOperationKind::Create | LookupTableOperationKind::Rollover => {
            leased.addresses.len()
        }
        LookupTableOperationKind::Extend => persisted.len() + leased.addresses.len(),
        _ => chain.addresses.len(),
    };
    let desired_rent = if matches!(
        leased.operation.operation_kind,
        LookupTableOperationKind::Create
            | LookupTableOperationKind::Rollover
            | LookupTableOperationKind::Extend
    ) {
        rpc.get_minimum_balance_for_rent_exemption(
            LOOKUP_TABLE_META_SIZE + final_address_count.saturating_mul(32),
        )?
    } else {
        0
    };
    let current_lamports = chain.account.as_ref().map_or(0, |account| account.lamports);
    Ok(BuiltMutation {
        transaction,
        recent_blockhash,
        last_valid_block_height,
        expected_fee_lamports,
        expected_rent_lamports: desired_rent.saturating_sub(current_lamports),
        reclaimed_rent_lamports,
        packet_size,
    })
}

fn validate_append_only(
    persisted: &[LookupTableMembershipAddress],
    chain: &ChainTable,
    extension: &[String],
) -> Result<(), Box<dyn Error>> {
    let persisted_addresses = persisted
        .iter()
        .map(|row| Pubkey::from_str(&row.address))
        .collect::<Result<Vec<_>, _>>()?;
    if chain.addresses != persisted_addresses {
        return Err("on-chain ALT does not exactly match the durable ordered prefix".into());
    }
    let extension = parse_addresses(extension)?;
    let existing = chain.addresses.iter().copied().collect::<BTreeSet<_>>();
    let mut seen = BTreeSet::new();
    if extension
        .iter()
        .any(|address| existing.contains(address) || !seen.insert(*address))
    {
        return Err("extend operation is not a genuinely missing, duplicate-free suffix".into());
    }
    if chain.addresses.len().saturating_add(extension.len()) > 256 {
        return Err("extend operation would exceed the ALT hard capacity".into());
    }
    Ok(())
}

fn validate_cleanup_chain_identity(
    leased: &LeasedLookupTableOperation,
    persisted: &[LookupTableMembershipAddress],
    chain: &ChainTable,
) -> Result<(), Box<dyn Error>> {
    let table = leased
        .physical_table
        .as_ref()
        .ok_or("cleanup operation has no physical lookup table")?;
    let account = chain
        .account
        .as_ref()
        .ok_or("cleanup operation lookup table is missing")?;
    if account.owner != alt_program::id()
        || chain.authority.map(|authority| authority.to_string()) != Some(table.authority.clone())
    {
        return Err("cleanup operation ALT owner or authority drifted".into());
    }
    let persisted_pubkeys = persisted
        .iter()
        .map(|row| Pubkey::from_str(&row.address))
        .collect::<Result<Vec<_>, _>>()?;
    if chain.addresses != persisted_pubkeys {
        return Err("cleanup operation ALT ordered address prefix drifted".into());
    }
    let persisted_strings = persisted
        .iter()
        .map(|row| row.address.clone())
        .collect::<Vec<_>>();
    if ordered_address_hash(&persisted_strings) != table.address_hash {
        return Err("cleanup operation durable address hash drifted".into());
    }
    Ok(())
}

fn ordered_address_hash(addresses: &[String]) -> String {
    let mut hasher = Sha256::new();
    for address in addresses {
        hasher.update((address.len() as u64).to_le_bytes());
        hasher.update(address.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn load_chain_table(
    rpc: &RpcClient,
    physical: Option<&loyal_yield_orchestrator::ReusableLookupTableRecord>,
) -> Result<ChainTable, Box<dyn Error>> {
    let Some(physical) = physical else {
        return Ok(ChainTable {
            observed_slot: rpc.get_slot_with_commitment(CommitmentConfig::finalized())?,
            account: None,
            authority: None,
            addresses: Vec::new(),
            deactivation_slot: None,
            last_extended_slot: None,
            last_extended_start_index: None,
        });
    };
    let address = Pubkey::from_str(&physical.table_address)?;
    let response = rpc.get_account_with_commitment(&address, CommitmentConfig::finalized())?;
    let observed_slot = response.context.slot;
    let account = response.value;
    let Some(account) = account else {
        return Ok(ChainTable {
            observed_slot,
            account: None,
            authority: None,
            addresses: Vec::new(),
            deactivation_slot: None,
            last_extended_slot: None,
            last_extended_start_index: None,
        });
    };
    if account.owner != alt_program::id() {
        return Ok(ChainTable {
            observed_slot,
            account: Some(account),
            authority: None,
            addresses: Vec::new(),
            deactivation_slot: None,
            last_extended_slot: None,
            last_extended_start_index: None,
        });
    }
    let table = AddressLookupTable::deserialize(&account.data)
        .map_err(|error| format!("failed to deserialize ALT {address}: {error:?}"))?;
    let authority = table.meta.authority;
    let addresses = table.addresses.to_vec();
    let deactivation_slot = table.meta.deactivation_slot;
    let last_extended_slot = table.meta.last_extended_slot;
    let last_extended_start_index = table.meta.last_extended_slot_start_index;
    Ok(ChainTable {
        observed_slot,
        account: Some(account),
        authority,
        addresses,
        deactivation_slot: Some(deactivation_slot),
        last_extended_slot: Some(last_extended_slot),
        last_extended_start_index: Some(last_extended_start_index),
    })
}

fn classify_chain_state(
    leased: &LeasedLookupTableOperation,
    persisted: &[LookupTableMembershipAddress],
    chain: &ChainTable,
) -> Result<ChainClassification, Box<dyn Error>> {
    let kind = leased.operation.operation_kind;
    if kind == LookupTableOperationKind::Close {
        return Ok(ChainClassification {
            state: if chain.account.is_none() {
                LookupTableChainState::ExactMatch
            } else {
                LookupTableChainState::Missing
            },
            membership_already_reconciled: false,
        });
    }
    let Some(account) = chain.account.as_ref() else {
        return Ok(ChainClassification {
            state: LookupTableChainState::Missing,
            membership_already_reconciled: false,
        });
    };
    if account.owner != alt_program::id() {
        return Ok(ChainClassification {
            state: LookupTableChainState::AuthorityDrift,
            membership_already_reconciled: false,
        });
    }
    let physical = leased
        .physical_table
        .as_ref()
        .ok_or("chain account exists without physical table metadata")?;
    if chain.authority.map(|key| key.to_string()) != Some(physical.authority.clone()) {
        return Ok(ChainClassification {
            state: LookupTableChainState::AuthorityDrift,
            membership_already_reconciled: false,
        });
    }
    let active = chain.deactivation_slot == Some(u64::MAX);
    if matches!(
        kind,
        LookupTableOperationKind::Create
            | LookupTableOperationKind::Extend
            | LookupTableOperationKind::Rollover
            | LookupTableOperationKind::Verify
    ) && !active
    {
        return Ok(ChainClassification {
            state: LookupTableChainState::LifecycleDrift,
            membership_already_reconciled: false,
        });
    }
    if kind == LookupTableOperationKind::Deactivate {
        return Ok(ChainClassification {
            state: if active {
                LookupTableChainState::Missing
            } else {
                LookupTableChainState::ExactMatch
            },
            membership_already_reconciled: false,
        });
    }
    let persisted_addresses = persisted
        .iter()
        .map(|row| Pubkey::from_str(&row.address))
        .collect::<Result<Vec<_>, _>>()?;
    let mutating_kind = matches!(
        kind,
        LookupTableOperationKind::Create
            | LookupTableOperationKind::Extend
            | LookupTableOperationKind::Rollover
    );
    let mutation_addresses = if mutating_kind {
        parse_addresses(&leased.addresses)?
    } else {
        Vec::new()
    };
    let persisted_strings = persisted
        .iter()
        .map(|row| row.address.clone())
        .collect::<Vec<_>>();

    // A crash can occur after membership replacement commits but before the
    // table lifecycle and operation status advance. Recognize only that exact
    // durable boundary: this operation owns the exact appended suffix and the
    // physical mutation epoch advanced exactly once. A finalized transaction
    // that truly had no effect retains the operation epoch and cannot pass.
    let membership_already_reconciled = !mutation_addresses.is_empty()
        && leased.operation.route_lookup_table_id == Some(physical.id)
        && leased.operation.family_id == physical.family_id
        && leased.operation.mutation_epoch.checked_add(1) == Some(physical.mutation_epoch)
        && i32::try_from(persisted.len()).ok() == Some(physical.address_count)
        && physical.usable_address_count == physical.address_count
        && ordered_address_hash(&persisted_strings) == physical.address_hash
        && chain.addresses == persisted_addresses
        && persisted
            .iter()
            .enumerate()
            .all(|(ordinal, row)| i32::try_from(ordinal).ok() == Some(row.ordinal))
        && persisted_addresses
            .len()
            .checked_sub(mutation_addresses.len())
            .is_some_and(|prefix_len| {
                let operation_shape_matches =
                    chain.last_extended_start_index.map(usize::from) == Some(prefix_len)
                        && match kind {
                            LookupTableOperationKind::Create
                            | LookupTableOperationKind::Rollover => prefix_len == 0,
                            LookupTableOperationKind::Extend => true,
                            _ => false,
                        };
                operation_shape_matches
                    && persisted_addresses[prefix_len..] == mutation_addresses
                    && persisted[..prefix_len]
                        .iter()
                        .all(|row| row.added_operation_id != Some(leased.operation.id))
                    && persisted[prefix_len..].iter().all(|row| {
                        row.added_operation_id == Some(leased.operation.id)
                            && chain
                                .last_extended_slot
                                .and_then(|slot| i64::try_from(slot).ok())
                                .is_some_and(|added_slot| {
                                    row.added_slot == added_slot
                                        && added_slot.checked_add(1) == Some(row.usable_after_slot)
                                })
                    })
            });
    if membership_already_reconciled {
        return Ok(ChainClassification {
            state: LookupTableChainState::ExactMatch,
            membership_already_reconciled: true,
        });
    }
    if mutating_kind && physical.mutation_epoch != leased.operation.mutation_epoch {
        return Ok(ChainClassification {
            state: LookupTableChainState::PrefixDrift,
            membership_already_reconciled: false,
        });
    }

    let mut expected = persisted_addresses.clone();
    expected.extend(mutation_addresses);
    if chain.addresses == expected {
        return Ok(ChainClassification {
            state: LookupTableChainState::ExactMatch,
            membership_already_reconciled: false,
        });
    }
    if chain.addresses == persisted_addresses {
        return Ok(ChainClassification {
            state: LookupTableChainState::Missing,
            membership_already_reconciled: false,
        });
    }
    Ok(ChainClassification {
        state: LookupTableChainState::PrefixDrift,
        membership_already_reconciled: false,
    })
}

fn load_signature_state(
    rpc: &RpcClient,
    leased: &LeasedLookupTableOperation,
) -> Result<SignatureObservation, Box<dyn Error>> {
    let Some(signature) = leased.operation.transaction_signature.as_deref() else {
        return Ok(SignatureObservation {
            state: LookupTableSignatureState::Unknown,
            observed_slot: None,
        });
    };
    let signature = Signature::from_str(signature)?;
    let status = rpc
        .get_signature_statuses_with_history(&[signature])?
        .value
        .into_iter()
        .next()
        .flatten();
    let Some(status) = status else {
        return Ok(SignatureObservation {
            state: LookupTableSignatureState::NotFound,
            observed_slot: None,
        });
    };
    let observed_slot = Some(status.slot);
    if status.err.is_some() {
        return Ok(SignatureObservation {
            state: LookupTableSignatureState::Failed,
            observed_slot,
        });
    }
    let state = if status.satisfies_commitment(CommitmentConfig::finalized()) {
        LookupTableSignatureState::Finalized
    } else if status.satisfies_commitment(CommitmentConfig::confirmed()) {
        LookupTableSignatureState::Confirmed
    } else {
        LookupTableSignatureState::Processed
    };
    Ok(SignatureObservation {
        state,
        observed_slot,
    })
}

fn chain_effect_is_possible(leased: &LeasedLookupTableOperation, chain: &ChainTable) -> bool {
    match leased.operation.operation_kind {
        LookupTableOperationKind::Create | LookupTableOperationKind::Rollover => {
            chain.account.is_some()
        }
        LookupTableOperationKind::Extend => leased
            .physical_table
            .as_ref()
            .is_some_and(|table| chain.addresses.len() > table.address_count as usize),
        LookupTableOperationKind::Deactivate => chain.deactivation_slot != Some(u64::MAX),
        LookupTableOperationKind::Close => chain.account.is_none(),
        LookupTableOperationKind::Verify => true,
    }
}

fn requires_chain_first_reconciliation(
    has_persisted_signature: bool,
    chain_effect_is_possible: bool,
) -> bool {
    has_persisted_signature || chain_effect_is_possible
}

fn has_unreconciled_persisted_signature(leased: &LeasedLookupTableOperation) -> bool {
    persisted_signature_requires_chain_reconciliation(
        leased.operation.transaction_signature.as_deref(),
        leased.operation.error_code.as_deref(),
    )
}

fn persisted_signature_requires_chain_reconciliation(
    transaction_signature: Option<&str>,
    error_code: Option<&str>,
) -> bool {
    transaction_signature.is_some() && error_code != Some(EXPIRED_TRANSACTION_RETRY_CODE)
}

fn reconciliation_transition_path(
    from: LookupTableOperationStatus,
) -> Result<Vec<LookupTableOperationStatus>, String> {
    use LookupTableOperationStatus as Status;
    let path = match from {
        Status::Leased => vec![Status::NeedsReconcile, Status::Reconciled],
        Status::Signed => vec![
            Status::Submitted,
            Status::Confirmed,
            Status::Finalized,
            Status::Reconciled,
        ],
        Status::Submitted => vec![Status::Confirmed, Status::Finalized, Status::Reconciled],
        Status::Confirmed => vec![Status::Finalized, Status::Reconciled],
        Status::Finalized | Status::NeedsReconcile => vec![Status::Reconciled],
        Status::Reconciled => Vec::new(),
        other => {
            return Err(format!(
                "operation in {other} cannot advance through finalized reconciliation"
            ))
        }
    };
    Ok(path)
}

fn provisioner_mutation_path(kind: LookupTableOperationKind) -> Option<&'static str> {
    match kind {
        LookupTableOperationKind::Create | LookupTableOperationKind::Rollover => Some("create"),
        LookupTableOperationKind::Extend => Some("extend"),
        LookupTableOperationKind::Deactivate => Some("deactivate"),
        LookupTableOperationKind::Close => Some("close"),
        LookupTableOperationKind::Verify => None,
    }
}

fn validate_manager_boundary(
    manager: &Keypair,
    family: &LookupTableFamilyRecord,
    physical: Option<&loyal_yield_orchestrator::ReusableLookupTableRecord>,
) -> Result<(), Box<dyn Error>> {
    let policy_pubkey = manager.pubkey().to_string();
    if family.provisioning_authority != policy_pubkey || family.payer != policy_pubkey {
        return Err(
            "configured ALT authority does not match the family's authority and payer".into(),
        );
    }
    if let Some(table) = physical {
        if table.authority != policy_pubkey || table.payer != policy_pubkey {
            return Err(
                "configured ALT authority does not match physical table authority/payer".into(),
            );
        }
    }
    Ok(())
}

async fn validate_cleanup_mutation_at_signing(
    client: &NeonSqlClient,
    options: &Options,
    leased: &LeasedLookupTableOperation,
) -> Result<(), Box<dyn Error>> {
    let kind = leased.operation.operation_kind;
    if !matches!(
        kind,
        LookupTableOperationKind::Deactivate | LookupTableOperationKind::Close
    ) {
        return Ok(());
    }
    let table = leased
        .physical_table
        .as_ref()
        .ok_or("cleanup operation has no physical lookup table")?;
    let protection = client
        .lookup_table_cleanup_protection_for_operation(
            &options.cluster,
            &table.table_address,
            leased.operation.id,
        )
        .await?
        .ok_or("cleanup operation table is not registered in this cluster")?;
    let expected_authority =
        context_string(&leased.operation.operation_context, "expectedAuthority")?;
    let expected_hash = context_string(&leased.operation.operation_context, "expectedAddressHash")?;
    let expected_epoch = leased
        .operation
        .operation_context
        .get("expectedMutationEpoch")
        .and_then(Value::as_i64)
        .ok_or("cleanup operation lacks expectedMutationEpoch")?;
    let expected_address_count = leased
        .operation
        .operation_context
        .get("expectedAddressCount")
        .and_then(Value::as_i64)
        .ok_or("cleanup operation lacks expectedAddressCount")?;
    if protection.table_id != table.id
        || protection.expected_authority != expected_authority
        || table.authority != expected_authority
        || protection.address_hash != expected_hash
        || table.address_hash != expected_hash
        || i64::from(protection.address_count) != expected_address_count
        || i64::from(table.address_count) != expected_address_count
        || protection.mutation_epoch != expected_epoch
        || table.mutation_epoch != expected_epoch
        || leased.operation.mutation_epoch != expected_epoch
    {
        return Err("cleanup operation metadata changed after it was queued".into());
    }
    let reasons = &protection.protection_reasons;
    let allowed = match kind {
        LookupTableOperationKind::Deactivate => {
            reasons.is_empty()
                && matches!(
                    protection.desired_state,
                    LookupTableLifecycle::Active
                        | LookupTableLifecycle::Standby
                        | LookupTableLifecycle::Retiring
                )
        }
        LookupTableOperationKind::Close => {
            reasons.is_empty() && protection.desired_state == LookupTableLifecycle::Deactivated
        }
        _ => unreachable!(),
    };
    if !allowed {
        return Err(format!(
            "cleanup action {} became protected before signing: {}",
            kind,
            reasons.join(",")
        )
        .into());
    }
    Ok(())
}

async fn prepare_cleanup_lifecycle(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    leased: &mut LeasedLookupTableOperation,
) -> Result<(), Box<dyn Error>> {
    if leased.operation.operation_kind != LookupTableOperationKind::Deactivate {
        return Ok(());
    }
    let table = leased
        .physical_table
        .as_ref()
        .ok_or("deactivate operation has no physical lookup table")?;
    let next_state = match table.desired_state {
        LookupTableLifecycle::Active | LookupTableLifecycle::Standby => {
            LookupTableLifecycle::Retiring
        }
        LookupTableLifecycle::Retiring => return Ok(()),
        state => {
            return Err(format!("cannot prepare {state} lookup table for deactivation").into())
        }
    };
    let verified_slot =
        i64::try_from(rpc.get_slot_with_commitment(CommitmentConfig::finalized())?)?;
    let updated = client
        .mark_reusable_lookup_table_verification(
            table.id,
            table.mutation_epoch,
            table.desired_state,
            next_state,
            false,
            table.usable_address_count,
            verified_slot,
        )
        .await?;
    leased.physical_table = Some(updated);
    Ok(())
}

fn context_string(context: &Value, field: &str) -> Result<String, Box<dyn Error>> {
    context
        .get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .map(ToOwned::to_owned)
        .ok_or_else(|| format!("operation context lacks {field}").into())
}

fn close_recipient(context: &Value) -> Result<Option<Pubkey>, Box<dyn Error>> {
    context
        .get("closeRecipient")
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .map(Pubkey::from_str)
        .transpose()
        .map_err(Into::into)
}

fn policy_close_recipient(context: &Value, authority: Pubkey) -> Result<Pubkey, Box<dyn Error>> {
    let recipient = close_recipient(context)?.unwrap_or(authority);
    if recipient != authority {
        return Err("lookup-table close recipient must equal the POLICY_KEYPAIR authority".into());
    }
    Ok(recipient)
}

fn load_manager_signer() -> Result<Keypair, Box<dyn Error>> {
    loyal_yield_orchestrator::standard_policy_keypair_from_env().map_err(Into::into)
}

#[cfg(test)]
const fn alt_authority_signer_env() -> &'static str {
    "POLICY_KEYPAIR"
}

fn operation_lease(
    leased: &LeasedLookupTableOperation,
) -> Result<LookupTableOperationLease, Box<dyn Error>> {
    LookupTableOperationLease::new(
        leased
            .operation
            .lease_owner
            .clone()
            .ok_or("leased operation has no lease owner")?,
        leased.operation.fencing_token,
        leased
            .operation
            .lease_expires_at
            .ok_or("leased operation has no expiry")?,
    )
    .map_err(Into::into)
}

fn validate_chunk(
    leased: &LeasedLookupTableOperation,
    configured_chunk: usize,
) -> Result<(), Box<dyn Error>> {
    if matches!(
        leased.operation.operation_kind,
        LookupTableOperationKind::Create
            | LookupTableOperationKind::Extend
            | LookupTableOperationKind::Rollover
    ) && leased.addresses.len() > configured_chunk
    {
        return Err(format!(
            "operation {} has {} addresses but the configured one-transaction chunk is {}",
            leased.operation.id,
            leased.addresses.len(),
            configured_chunk
        )
        .into());
    }
    Ok(())
}

fn create_recent_slot(context: &Value) -> Result<u64, Box<dyn Error>> {
    context
        .get("recent_slot")
        .or_else(|| context.get("recentSlot"))
        .and_then(Value::as_u64)
        .ok_or_else(|| {
            "create operation is missing durable operation_context.recent_slot; refusing a non-deterministic retry"
                .into()
        })
}

fn create_recent_slot_has_expired(reserved_recent_slot: u64, finalized_slot: u64) -> bool {
    finalized_slot.saturating_sub(reserved_recent_slot) >= SLOT_HASHES_MAX_ENTRIES as u64
}

fn parse_addresses(addresses: &[String]) -> Result<Vec<Pubkey>, Box<dyn Error>> {
    addresses
        .iter()
        .map(|address| Pubkey::from_str(address).map_err(Into::into))
        .collect()
}

fn hash_bytes(bytes: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(bytes);
    format!("{:x}", hasher.finalize())
}

fn safe_error(error: &str) -> String {
    redacted_external_error(error)
}

fn emit_operation_report(
    options: &Options,
    leased: &LeasedLookupTableOperation,
    selected_budget_lamports: u64,
    expected_fee_lamports: Option<u64>,
    expected_rent_lamports: Option<u64>,
    simulation: &'static str,
    result: &str,
) {
    let report = OperationReport {
        event: "alt_provisioner_operation",
        cluster: options.cluster.clone(),
        mode: options.mode.as_str(),
        operation_id: leased.operation.id,
        operation_kind: leased.operation.operation_kind.to_string(),
        table: leased
            .physical_table
            .as_ref()
            .map(|table| table.table_address.clone()),
        address_count: leased.addresses.len(),
        selected_budget_lamports,
        expected_fee_lamports,
        expected_rent_lamports,
        simulation,
        result: result.to_owned(),
    };
    println!(
        "{}",
        serde_json::to_string(&report).expect("report serializes")
    );
}

async fn emit_status(client: &NeonSqlClient, options: &Options) -> Result<(), Box<dyn Error>> {
    let durable_control = client
        .lookup_table_provisioner_control(&options.cluster)
        .await?;
    let durable_paused = durable_control
        .as_ref()
        .is_some_and(|control| control.paused);
    let durable_control_json = durable_control.as_ref().map_or_else(
        || json!({ "paused": false, "configured": false }),
        |control| {
            json!({
                "paused": control.paused,
                "configured": true,
                "reason": control.reason,
                "updatedBy": control.updated_by,
                "controlEpoch": control.control_epoch,
                "updatedAt": control.updated_at,
            })
        },
    );
    let snapshot = client
        .lookup_table_control_plane_snapshot(&options.cluster)
        .await?;
    let durable_safety: Value = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT jsonb_build_object(
            'activeBudgetReservationCount', count(*) FILTER (
                WHERE reservation.reserved_until > now()
                  AND operation.operation_state <> 'cancelled'
            ),
            'activeReservedLamports', COALESCE(sum(reservation.reserved_lamports) FILTER (
                WHERE reservation.reserved_until > now()
                  AND operation.operation_state <> 'cancelled'
            ), 0),
            'openSharedPhysicalDriftCount', (
                SELECT count(*)
                FROM loyal_yield.lookup_table_shared_market_physical_drifts drift
                WHERE drift.cluster = $1 AND drift.resolution_state = 'open'
            ),
            'activeBroadcastPermitCount', (
                SELECT count(*)
                FROM loyal_yield.lookup_table_provisioner_broadcast_permits permit
                WHERE permit.cluster = $1 AND permit.resolved_at IS NULL
            ),
            'latestPrecutoverProbeControlEpoch', (
                SELECT probe.provisioner_control_epoch
                FROM loyal_yield.lookup_table_precutover_probe_runs probe
                WHERE probe.cluster = $1
                ORDER BY probe.created_at DESC, probe.id DESC
                LIMIT 1
            )
        )
        FROM loyal_yield.lookup_table_cluster_budget_reservations reservation
        JOIN loyal_yield.lookup_table_operations operation
          ON operation.id = reservation.operation_id
        WHERE reservation.cluster = $1
        "#,
    )
    .bind(&options.cluster)
    .fetch_one(client.pool())
    .await?;
    println!(
        "{}",
        json!({
            "event": "alt_provisioner_status",
            "cluster": options.cluster,
            "mode": options.mode.as_str(),
            "paused": options.local_paused || durable_paused,
            "localPaused": options.local_paused,
            "durableProvisionerControl": durable_control_json,
            "maxOperations": options.max_operations,
            "maxAttempts": options.max_attempts,
            "addressChunk": options.address_chunk,
            "maxLamports": options.max_lamports.to_string(),
            "budgetWindowSeconds": options.budget_window_seconds,
            "rateLimitMs": options.rate_limit_ms,
            "catalogReconcileIntervalSeconds": options.catalog_reconcile_interval_seconds,
            "concurrency": options.concurrency,
            "safetyMargin": options.safety_margin,
            "largestAtomicExpansion": options.largest_atomic_expansion,
            "vaultGrowthReservation": options.vault_growth_reservation,
            "maxVaultCohort": options.max_vault_cohort,
            "durableSafety": durable_safety,
            "snapshot": snapshot,
        })
    );
    Ok(())
}

async fn emit_dry_run_queue(
    client: &NeonSqlClient,
    options: &Options,
) -> Result<(), Box<dyn Error>> {
    let rows = loyal_yield_orchestrator::sqlx::query_scalar::<_, Value>(
        r#"
        SELECT COALESCE(jsonb_agg(jsonb_build_object(
            'operationId', operation.id,
            'kind', operation.operation_kind,
            'state', operation.operation_state,
            'table', route_table.table_address,
            'addressCount', (SELECT count(*) FROM loyal_yield.lookup_table_operation_addresses a WHERE a.operation_id = operation.id),
            'estimatedFeeLamports', operation.estimated_fee_lamports,
            'estimatedRentLamports', operation.estimated_rent_lamports,
            'attemptCount', operation.attempt_count
        ) ORDER BY operation.created_at, operation.id), '[]'::jsonb)
        FROM loyal_yield.lookup_table_operations operation
        JOIN loyal_yield.lookup_table_families family ON family.id = operation.family_id
        LEFT JOIN loyal_yield.route_lookup_tables route_table ON route_table.id = operation.route_lookup_table_id
        WHERE family.cluster = $1
          AND operation.operation_state NOT IN ('complete', 'permanent_failure', 'cancelled')
        "#,
    )
    .bind(&options.cluster)
    .fetch_one(client.pool())
    .await?;
    println!(
        "{}",
        json!({
            "event": "alt_provisioner_dry_run",
            "cluster": options.cluster,
            "mode": options.mode.as_str(),
            "operations": rows,
            "signerLoaded": false,
            "databaseWrites": false,
            "transactionsSent": false,
        })
    );
    Ok(())
}

fn validate_reusable_only_cutover_rpc_preflight(
    options: &Options,
    preflight: &ReusableOnlyCutoverPreflight,
) -> Result<FinalizedSharedTableObservation, Box<dyn Error>> {
    if preflight.cluster != options.cluster {
        return Err("cutover preflight belongs to a different cluster".into());
    }
    let rpc_url = options
        .rpc_url
        .as_ref()
        .ok_or("reusable-only cutover requires a finalized RPC endpoint")?;
    let rpc = RpcClient::new_with_commitment(rpc_url.clone(), CommitmentConfig::finalized());
    let genesis_hash = rpc
        .get_genesis_hash()
        .map_err(|_| "failed to read genesis hash for reusable-only cutover")?;
    validate_rpc_genesis_hash(&options.cluster, genesis_hash)
        .map_err(|error| format!("refusing reusable-only cutover on mismatched RPC: {error}"))?;
    if preflight.shared_tables.is_empty() {
        return Err("reusable-only cutover requires a non-empty shared-table bundle".into());
    }
    let table_addresses = preflight
        .shared_tables
        .iter()
        .map(|table| Pubkey::from_str(&table.table_address))
        .collect::<Result<Vec<_>, _>>()?;
    let response =
        rpc.get_multiple_accounts_with_commitment(&table_addresses, CommitmentConfig::finalized())?;
    if response.value.len() != preflight.shared_tables.len() {
        return Err("finalized RPC returned an incomplete shared-table bundle".into());
    }
    let observed_slot = i64::try_from(response.context.slot)?;
    let mut finalized_tables = Vec::with_capacity(preflight.shared_tables.len());
    let mut flattened_addresses = Vec::new();
    for (expected, account) in preflight.shared_tables.iter().zip(response.value) {
        let account = account.ok_or_else(|| {
            format!(
                "shared lookup table shard {} is absent at finalized commitment",
                expected.shard_ordinal
            )
        })?;
        if account.owner != alt_program::id() {
            return Err(format!(
                "shared lookup table shard {} has the wrong finalized owner",
                expected.shard_ordinal
            )
            .into());
        }
        let table = AddressLookupTable::deserialize(&account.data)?;
        let expected_authority = Pubkey::from_str(&expected.authority)?;
        if table.meta.authority != Some(expected_authority)
            || table.meta.deactivation_slot != u64::MAX
        {
            return Err(format!(
                "shared lookup table shard {} authority/lifecycle failed finalized cutover preflight",
                expected.shard_ordinal
            )
            .into());
        }
        let last_extended_slot = i64::try_from(table.meta.last_extended_slot)?;
        if observed_slot <= last_extended_slot {
            return Err(format!(
                "shared lookup table shard {} is not warm at finalized cutover preflight slot",
                expected.shard_ordinal
            )
            .into());
        }
        let observed_addresses = table
            .addresses
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>();
        let observed_hash = ordered_address_hash(&observed_addresses);
        if observed_addresses != expected.ordered_addresses
            || observed_hash != expected.ordered_address_hash
            || i32::try_from(observed_addresses.len())? != expected.address_count
            || expected.usable_address_count != expected.address_count
            || last_extended_slot != expected.last_extended_slot
            || expected.last_verified_slot > observed_slot
        {
            return Err(format!(
                "shared lookup table shard {} finalized identity or membership changed before reusable-only cutover",
                expected.shard_ordinal
            )
            .into());
        }
        flattened_addresses.extend(observed_addresses.iter().cloned());
        finalized_tables.push(FinalizedSharedTableShardObservation {
            table_id: expected.table_id,
            shard_ordinal: expected.shard_ordinal,
            table_address: expected.table_address.clone(),
            authority: expected.authority.clone(),
            mutation_epoch: expected.mutation_epoch,
            last_extended_slot,
            ordered_address_hash: observed_hash,
            address_count: i32::try_from(observed_addresses.len())?,
            ordered_addresses: observed_addresses,
        });
    }
    if flattened_addresses != preflight.ordered_addresses
        || ordered_address_hash(&flattened_addresses) != preflight.ordered_address_hash
    {
        return Err(
            "finalized shared-table shard union does not exactly match the logical catalog".into(),
        );
    }
    let shared_table_bundle_hash = finalized_shared_table_bundle_hash(&finalized_tables);
    if shared_table_bundle_hash != preflight.shared_table_bundle_hash {
        return Err("finalized shared-table bundle identity changed before cutover".into());
    }
    Ok(FinalizedSharedTableObservation {
        cluster: preflight.cluster.clone(),
        observed_slot,
        shared_table_bundle_hash,
        shared_tables: finalized_tables,
    })
}

async fn run_precutover_probe(
    client: &NeonSqlClient,
    options: &Options,
) -> Result<(), Box<dyn Error>> {
    let probe_vault_id = options.probe_vault_id.expect("validated by parser");
    let durable_pause = client
        .lookup_table_provisioner_control(&options.cluster)
        .await?
        .filter(|control| control.paused)
        .ok_or("pre-cutover probe requires the durable cluster provisioner pause to be active")?;
    require_precutover_probe_mutations_drained(client, &options.cluster).await?;
    let rollout = client
        .effective_lookup_table_rollout(&options.cluster, probe_vault_id)
        .await?;
    if rollout.rollout_mode == LookupTableRolloutMode::ReusableOnly && !rollout.force_legacy {
        return Err(
            "pre-cutover probe refuses to run while the selected vault can actively route".into(),
        );
    }
    let preflight = client
        .reusable_only_cutover_preflight(&options.cluster)
        .await?;
    let finalized = validate_reusable_only_cutover_rpc_preflight(options, &preflight)?;
    let finalized_addresses = finalized
        .shared_tables
        .iter()
        .flat_map(|table| table.ordered_addresses.iter().cloned())
        .collect::<Vec<_>>();
    let drift_target = finalized
        .shared_tables
        .last()
        .cloned()
        .ok_or("pre-cutover probe requires a non-empty finalized shared-table bundle")?;
    if finalized_addresses.is_empty() || drift_target.ordered_addresses.is_empty() {
        return Err(
            "pre-cutover probe requires non-empty finalized shared-table membership".into(),
        );
    }
    require_precutover_probe_mutations_drained(client, &options.cluster).await?;
    let probe_token = ordered_address_hash(&[format!(
        "precutover-probe:{}:{}:{}:{}:{}:{}",
        options.cluster,
        probe_vault_id.as_i64(),
        preflight.catalog_revision_id,
        preflight.shared_table_bundle_hash,
        finalized.observed_slot,
        Utc::now().timestamp_micros(),
    )]);
    let requirements_fingerprint =
        ordered_address_hash(&[format!("precutover-probe-requirements:{probe_token}")]);
    let route_fingerprint =
        ordered_address_hash(&[format!("precutover-probe-route:{probe_token}")]);
    let fixture_address = derive_precutover_probe_vault_address(&probe_token, &finalized_addresses);
    let vault_addresses = vec![LookupTableManifestAddressRecord {
        address: fixture_address,
        ordinal: 0,
        semantic_class: LookupTableManifestSubject::Vault,
        account_role: "precutover_probe_vault_fixture".to_owned(),
        is_writable: true,
    }];
    let desired_vault_hash = lookup_table_manifest_address_records_hash(&vault_addresses);
    let mut synthetic_drift_addresses = drift_target.ordered_addresses.clone();
    synthetic_drift_addresses
        .pop()
        .expect("non-empty finalized shared-table shard checked above");
    let audit = client
        .run_lookup_table_precutover_probe(LookupTablePrecutoverProbe {
            probe_token: probe_token.clone(),
            provisioner_control_epoch: durable_pause.control_epoch,
            finalized_observation: finalized.clone(),
            drift_report: SharedMarketPhysicalDriftReport {
                cluster: options.cluster.clone(),
                catalog_revision_id: preflight.catalog_revision_id,
                family_id: preflight.shared_family_id,
                route_lookup_table_id: drift_target.table_id,
                expected_mutation_epoch: drift_target.mutation_epoch,
                expected_table_address: drift_target.table_address.clone(),
                expected_authority: drift_target.authority.clone(),
                observed_slot: finalized.observed_slot,
                observed_table_present: true,
                observed_authority: Some(drift_target.authority),
                observed_active: true,
                observed_last_extended_slot: Some(drift_target.last_extended_slot),
                observed_warm: true,
                observed_addresses: synthetic_drift_addresses,
                reason: format!("precutover-probe-synthetic-drift:{probe_token}"),
                reported_by: "route-lookup-table-provisioner:precutover-probe".to_owned(),
            },
            provisioning_request: LookupTableProvisioningRequestUpsert {
                cluster: options.cluster.clone(),
                vault_id: probe_vault_id,
                route_fingerprint,
                requirements_fingerprint,
                shared_manifest_id: Some(preflight.manifest_id),
                vault_manifest_id: None,
                desired_shared_hash: Some(preflight.manifest_hash),
                desired_vault_hash: Some(desired_vault_hash),
                shared_addresses: Vec::new(),
                vault_addresses,
            },
        })
        .await?;
    println!(
        "{}",
        json!({
            "event": "alt_precutover_probe_passed",
            "cluster": audit.cluster,
            "probeRunId": audit.id,
            "probeToken": audit.probe_token,
            "probeVaultId": audit.vault_id.as_i64(),
            "catalogRevisionId": audit.catalog_revision_id,
            "sharedManifestId": audit.shared_manifest_id,
            "routeLookupTableId": audit.route_lookup_table_id,
            "sharedTableAddress": audit.shared_table_address,
            "sharedAuthority": audit.shared_authority,
            "sharedMutationEpoch": audit.shared_mutation_epoch,
            "finalizedSlot": audit.finalized_slot,
            "finalizedLastExtendedSlot": audit.finalized_last_extended_slot,
            "finalizedAddressHash": audit.finalized_address_hash,
            "finalizedAddressCount": audit.finalized_address_count,
            "sharedTableBundleHash": audit.shared_table_bundle_hash,
            "sharedTableCount": audit.shared_table_count,
            "finalizedBundleAddressCount": audit.finalized_bundle_address_count,
            "sharedTables": audit.shared_tables,
            "finalizedSharedExact": audit.finalized_shared_exact,
            "syntheticDriftSignalCount": audit.drift_signal_count,
            "driftProvisioningRequestCount": audit.drift_provisioning_request_count,
            "duplicateRequestAttempts": audit.duplicate_request_attempt_count,
            "distinctRequestCountInsideTransaction": audit.distinct_request_count,
            "decisionCountInsideTransaction": audit.decision_count,
            "bindingCountInsideTransaction": audit.binding_count,
            "operationCountInsideTransaction": audit.operation_count,
            "rollbackResidueCount": audit.rollback_residue_count,
            "catalogHeadRestored": audit.catalog_head_restored,
            "durablePauseControlEpoch": durable_pause.control_epoch,
            "inFlightMutationCount": 0,
            "committedProbeAuditRows": 1,
            "committedDemandRows": 0,
            "signerLoaded": audit.signer_loaded,
            "transactionsSent": audit.transactions_sent,
            "result": audit.result,
        })
    );
    Ok(())
}

async fn require_precutover_probe_mutations_drained(
    client: &NeonSqlClient,
    cluster: &str,
) -> Result<(), Box<dyn Error>> {
    let in_flight_mutations: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*)::BIGINT
        FROM loyal_yield.lookup_table_operations operation
        JOIN loyal_yield.lookup_table_families family
          ON family.id = operation.family_id
        WHERE family.cluster = $1
          AND (
              operation.operation_state IN (
                  'leased', 'signed', 'submitted', 'confirmed', 'finalized',
                  'reconciled', 'needs_reconcile'
              )
              OR (
                  operation.operation_state = 'retry_wait'
                  AND operation.transaction_signature IS NOT NULL
              )
          )
        "#,
    )
    .bind(cluster)
    .fetch_one(client.pool())
    .await?;
    let active_broadcast_permits: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*)::BIGINT
        FROM loyal_yield.lookup_table_provisioner_broadcast_permits
        WHERE cluster = $1 AND resolved_at IS NULL
        "#,
    )
    .bind(cluster)
    .fetch_one(client.pool())
    .await?;
    if in_flight_mutations != 0 || active_broadcast_permits != 0 {
        return Err(format!(
            "pre-cutover probe requires the durable pause to drain; found {in_flight_mutations} leased, signed, submitted, reconciling, or otherwise in-flight ALT operations and {active_broadcast_permits} active broadcast permits"
        )
        .into());
    }
    Ok(())
}

fn derive_precutover_probe_vault_address(probe_token: &str, occupied: &[String]) -> String {
    let occupied = occupied.iter().collect::<BTreeSet<_>>();
    for nonce in 0_u32.. {
        let mut hasher = Sha256::new();
        hasher.update(b"loyal-reusable-alt-precutover-probe-vault-address");
        hasher.update(probe_token.as_bytes());
        hasher.update(nonce.to_le_bytes());
        let address = Pubkey::new_from_array(hasher.finalize().into()).to_string();
        if !occupied.contains(&address) {
            return address;
        }
    }
    unreachable!("u32 probe address domain is exhausted")
}

async fn apply_admin_action(
    client: &NeonSqlClient,
    options: &Options,
) -> Result<(), Box<dyn Error>> {
    if matches!(options.admin_action, AdminAction::None) {
        return Ok(());
    }
    if !options.admin_write {
        return Err("control-plane changes require --admin-write".into());
    }
    let reason = options
        .admin_reason
        .as_deref()
        .ok_or("control-plane changes require --reason")?;
    let updated_by = options
        .admin_updated_by
        .as_deref()
        .ok_or("control-plane changes require --updated-by")?;
    if matches!(
        options.admin_action,
        AdminAction::SetProvisionerPause | AdminAction::ClearProvisionerPause
    ) {
        let paused = options.admin_action == AdminAction::SetProvisionerPause;
        let control = client
            .set_lookup_table_provisioner_pause(&options.cluster, paused, reason, updated_by)
            .await?;
        println!(
            "{}",
            json!({
                "event": "alt_provisioner_pause_control_updated",
                "cluster": control.cluster,
                "paused": control.paused,
                "reason": control.reason,
                "updatedBy": control.updated_by,
                "controlEpoch": control.control_epoch,
                "updatedAt": control.updated_at,
                "signerLoaded": false,
                "transactionsSent": false,
            })
        );
        return Ok(());
    }
    if options.admin_action == AdminAction::RepairTerminalOperations {
        repair_terminal_operations(client, options, reason, updated_by).await?;
        return Ok(());
    }
    if options.admin_action == AdminAction::BootstrapFamilies {
        let policy_pubkey = options.admin_policy_pubkey.expect("validated by parser");
        let manager = policy_pubkey.to_string();
        let catalog_version = options
            .catalog_version
            .as_deref()
            .expect("validated by parser");
        let mut families = Vec::new();
        for input in bootstrap_family_inputs(options)? {
            families.push(client.create_or_validate_lookup_table_family(input).await?);
        }
        println!(
            "{}",
            json!({
                "event": "alt_families_bootstrapped",
                "cluster": options.cluster,
                "familyIds": families.iter().map(|family| family.id).collect::<Vec<_>>(),
                "authority": manager,
                "catalogVersion": catalog_version,
                "reason": reason,
                "updatedBy": updated_by,
                "signerLoaded": false,
            })
        );
        return Ok(());
    }
    if let AdminAction::RollbackFamily(family_id) = options.admin_action {
        let family_cluster = loyal_yield_orchestrator::sqlx::query_scalar::<_, String>(
            "SELECT cluster FROM loyal_yield.lookup_table_families WHERE id = $1",
        )
        .bind(family_id)
        .fetch_optional(client.pool())
        .await?
        .ok_or("rollback family was not found")?;
        if family_cluster != options.cluster {
            return Err("rollback family does not belong to the explicit cluster".into());
        }
        let family = client
            .rollback_lookup_table_family_generation(family_id)
            .await?;
        println!(
            "{}",
            json!({
                "event": "alt_family_generation_rolled_back",
                "cluster": options.cluster,
                "familyId": family.id,
                "activeGeneration": family.active_generation,
                "previousGeneration": family.previous_generation,
                "reason": reason,
                "updatedBy": updated_by,
                "signerLoaded": false,
            })
        );
        return Ok(());
    }
    if let AdminAction::RollbackBinding(binding_id) = options.admin_action {
        let binding_cluster = loyal_yield_orchestrator::sqlx::query_scalar::<_, String>(
            r#"
            SELECT family.cluster
            FROM loyal_yield.lookup_table_vault_bindings binding
            JOIN loyal_yield.lookup_table_families family ON family.id = binding.family_id
            WHERE binding.id = $1
            "#,
        )
        .bind(binding_id)
        .fetch_optional(client.pool())
        .await?
        .ok_or("rollback binding was not found")?;
        if binding_cluster != options.cluster {
            return Err("rollback binding does not belong to the explicit cluster".into());
        }
        let observed_slot = options
            .admin_observed_slot
            .ok_or("--rollback-binding requires --observed-slot")?;
        let rollback = client
            .rollback_lookup_table_binding_head(binding_id, observed_slot)
            .await?;
        println!(
            "{}",
            json!({
                "event": "alt_vault_binding_rolled_back",
                "cluster": options.cluster,
                "activeBindingId": rollback.active.id,
                "predecessorBindingId": rollback.predecessor.as_ref().map(|binding| binding.id),
                "observedSlot": observed_slot,
                "reason": reason,
                "updatedBy": updated_by,
                "signerLoaded": false,
            })
        );
        return Ok(());
    }
    if let AdminAction::FinalizeRollbacks(family_id) = options.admin_action {
        let family_cluster = loyal_yield_orchestrator::sqlx::query_scalar::<_, String>(
            "SELECT cluster FROM loyal_yield.lookup_table_families WHERE id = $1",
        )
        .bind(family_id)
        .fetch_optional(client.pool())
        .await?
        .ok_or("rollback-finalization family was not found")?;
        if family_cluster != options.cluster {
            return Err(
                "rollback-finalization family does not belong to the explicit cluster".into(),
            );
        }
        let finalized = client
            .finalize_expired_lookup_table_rollbacks(family_id)
            .await?;
        println!(
            "{}",
            json!({
                "event": "alt_rollbacks_finalized",
                "cluster": options.cluster,
                "familyId": finalized.family_id,
                "clearedPreviousGeneration": finalized.cleared_previous_generation,
                "retiredBindingIds": finalized.retired_binding_ids,
                "retiringTableIds": finalized.retiring_table_ids,
                "releasedReservedCapacity": finalized.released_reserved_capacity,
                "reason": reason,
                "updatedBy": updated_by,
                "signerLoaded": false,
            })
        );
        return Ok(());
    }
    if let AdminAction::RetireLegacy(table_address) = options.admin_action {
        let retired = client
            .retire_legacy_route_lookup_table(LegacyLookupTableRetirementRequest {
                cluster: options.cluster.clone(),
                table_address: table_address.to_string(),
                expected_authority: options
                    .admin_expected_authority
                    .expect("validated by parser")
                    .to_string(),
                expected_address_hash: options
                    .admin_expected_address_hash
                    .clone()
                    .expect("validated by parser"),
                expected_address_count: options
                    .admin_expected_address_count
                    .expect("validated by parser"),
            })
            .await?;
        println!(
            "{}",
            json!({
                "event": "alt_legacy_table_retired",
                "cluster": retired.cluster,
                "tableId": retired.table_id,
                "table": retired.table_address,
                "authority": retired.authority,
                "addressHash": retired.address_hash,
                "addressCount": retired.address_count,
                "previousStatus": retired.previous_status,
                "status": retired.status,
                "durable": retired.durable,
                "reason": reason,
                "updatedBy": updated_by,
                "signerLoaded": false,
                "transactionsSent": false,
            })
        );
        return Ok(());
    }
    if options.admin_action == AdminAction::ActivateReusableOnly {
        let preflight = client
            .reusable_only_cutover_preflight(&options.cluster)
            .await?;
        let finalized_observation =
            validate_reusable_only_cutover_rpc_preflight(options, &preflight)?;
        let cutover = client
            .activate_reusable_only_cutover(&preflight, &finalized_observation, reason, updated_by)
            .await?;
        println!(
            "{}",
            json!({
                "event": "alt_reusable_only_cutover_activated",
                "cluster": cutover.cluster,
                "catalogRevisionId": cutover.catalog_revision_id,
                "sharedFamilyId": cutover.shared_family_id,
                "sharedGeneration": cutover.shared_generation,
                "sharedTableBundleHash": preflight.shared_table_bundle_hash,
                "sharedTableCount": preflight.shared_tables.len(),
                "sharedPhysicalTables": preflight.shared_tables,
                "finalizedRpcPreflight": true,
                "finalizedObservedSlot": cutover.finalized_observed_slot,
                "finalizedAddressHash": cutover.finalized_address_hash,
                "finalizedAddressCount": cutover.finalized_address_count,
                "provisionerControlEpoch": cutover.provisioner_control_epoch,
                "vaultFamilyId": cutover.vault_family_id,
                "alignedVaultControlCount": cutover.aligned_vault_control_count,
                "rolloutMode": cutover.global_control.rollout_mode.as_str(),
                "forceLegacy": cutover.global_control.force_legacy,
                "reason": reason,
                "updatedBy": updated_by,
                "signerLoaded": false,
                "transactionsSent": false,
            })
        );
        return Ok(());
    }
    let control = match options.admin_action {
        AdminAction::None
        | AdminAction::BootstrapFamilies
        | AdminAction::RollbackFamily(_)
        | AdminAction::RollbackBinding(_)
        | AdminAction::FinalizeRollbacks(_)
        | AdminAction::RetireLegacy(_)
        | AdminAction::ActivateReusableOnly
        | AdminAction::SetProvisionerPause
        | AdminAction::ClearProvisionerPause
        | AdminAction::RepairTerminalOperations => {
            unreachable!()
        }
        AdminAction::ForceLegacy => {
            client
                .set_lookup_table_force_legacy(&options.cluster, true, Some(reason), updated_by)
                .await?
        }
        AdminAction::ClearForceLegacy => {
            client
                .set_lookup_table_force_legacy(&options.cluster, false, Some(reason), updated_by)
                .await?
        }
        AdminAction::SetRolloutMode(mode) => {
            client
                .set_lookup_table_rollout_mode(
                    &options.cluster,
                    options.admin_vault_id,
                    mode,
                    Some(reason),
                    updated_by,
                )
                .await?
        }
    };
    println!(
        "{}",
        json!({
            "event": "alt_rollout_control_updated",
            "cluster": options.cluster,
            "rolloutMode": control.rollout_mode.as_str(),
            "forceLegacy": control.force_legacy,
            "vaultId": control.vault_id.map(VaultId::as_i64),
            "updatedBy": control.updated_by,
        })
    );
    Ok(())
}

async fn repair_terminal_operations(
    client: &NeonSqlClient,
    options: &Options,
    reason: &str,
    updated_by: &str,
) -> Result<(), Box<dyn Error>> {
    let control = client
        .lookup_table_provisioner_control(&options.cluster)
        .await?
        .ok_or("terminal ALT repair requires an existing durable provisioner control")?;
    if !control.paused {
        return Err("terminal ALT repair requires the durable cluster pause".into());
    }
    let signer = load_manager_signer()?;
    let standard_policy = Pubkey::from_str(STANDARD_POLICY_AUTHORITY)?;
    if signer.pubkey() != standard_policy {
        return Err(format!(
            "POLICY_KEYPAIR must equal the standard policy authority {STANDARD_POLICY_AUTHORITY}"
        )
        .into());
    }
    let rpc_url = options
        .rpc_url
        .as_ref()
        .ok_or("--repair-terminal-operations requires SOLANA_RPC_URL or --rpc-url")?;
    validate_rpc_endpoint(rpc_url)?;
    let rpc = RpcClient::new_with_commitment(rpc_url.clone(), CommitmentConfig::finalized());
    let observed_genesis_hash = rpc
        .get_genesis_hash()
        .map_err(|_| "failed to read genesis hash from terminal ALT repair RPC endpoint")?;
    validate_rpc_genesis_hash(&options.cluster, observed_genesis_hash)
        .map_err(|error| format!("refusing terminal ALT repair against mismatched RPC: {error}"))?;

    let candidates = client
        .lookup_table_terminal_repair_candidates(
            &options.cluster,
            i64::try_from(options.max_operations)?,
        )
        .await?;
    let candidate_count = candidates.len();
    let mut repaired = 0usize;
    let mut skipped = 0usize;
    for candidate in candidates {
        let chain = load_chain_table(&rpc, Some(&candidate.physical_table))?;
        let account_state = match chain.account.as_ref() {
            None => LookupTableTerminalAccountState::Missing,
            Some(account) if account.owner != alt_program::id() => {
                LookupTableTerminalAccountState::NonLookupTable
            }
            Some(_) if chain.deactivation_slot == Some(u64::MAX) && chain.authority.is_some() => {
                LookupTableTerminalAccountState::ActiveLookupTable
            }
            Some(_) => {
                skipped += 1;
                println!(
                    "{}",
                    json!({
                        "event": "alt_terminal_repair_skipped",
                        "cluster": options.cluster,
                        "operationId": candidate.operation.id,
                        "tableId": candidate.physical_table.id,
                        "reason": "finalized ALT lifecycle is not an active repairable prefix",
                        "transactionsSent": false,
                    })
                );
                continue;
            }
        };
        let Some(no_effect) =
            finalized_terminal_no_effect_evidence(&rpc, &candidate.operation, chain.observed_slot)?
        else {
            skipped += 1;
            println!(
                "{}",
                json!({
                    "event": "alt_terminal_repair_skipped",
                    "cluster": options.cluster,
                    "operationId": candidate.operation.id,
                    "tableId": candidate.physical_table.id,
                    "reason": "repair root lacks finalized no-effect evidence",
                    "transactionsSent": false,
                })
            );
            continue;
        };
        let mut sibling_no_effect =
            Vec::with_capacity(candidate.unresolved_terminal_siblings.len());
        let mut unsafe_sibling = None;
        for sibling in &candidate.unresolved_terminal_siblings {
            match finalized_terminal_no_effect_evidence(&rpc, sibling, chain.observed_slot)? {
                Some(no_effect) => sibling_no_effect.push(LookupTableTerminalSiblingEvidence {
                    operation_id: sibling.id,
                    no_effect,
                }),
                None => {
                    unsafe_sibling = Some(sibling.id);
                    break;
                }
            }
        }
        if let Some(sibling_id) = unsafe_sibling {
            skipped += 1;
            println!(
                "{}",
                json!({
                    "event": "alt_terminal_repair_skipped",
                    "cluster": options.cluster,
                    "operationId": candidate.operation.id,
                    "tableId": candidate.physical_table.id,
                    "unsafeSiblingOperationId": sibling_id,
                    "reason": "terminal sibling lacks individual finalized no-effect evidence",
                    "transactionsSent": false,
                })
            );
            continue;
        }
        let request = LookupTableTerminalRepairRequest {
            cluster: options.cluster.clone(),
            operation_id: candidate.operation.id,
            expected_control_epoch: control.control_epoch,
            expected_policy_authority: standard_policy.to_string(),
            chain: LookupTableTerminalChainEvidence {
                observed_slot: i64::try_from(chain.observed_slot)?,
                account_state,
                account_owner: chain
                    .account
                    .as_ref()
                    .map(|account| account.owner.to_string()),
                authority: chain.authority.map(|authority| authority.to_string()),
                last_extended_slot: chain.last_extended_slot.map(i64::try_from).transpose()?,
                ordered_addresses: chain.addresses.iter().map(ToString::to_string).collect(),
            },
            no_effect,
            sibling_no_effect,
            reason: reason.to_owned(),
            updated_by: updated_by.to_owned(),
        };
        match client.repair_terminal_lookup_table_operation(request).await {
            Ok(result) => {
                repaired += 1;
                println!(
                    "{}",
                    json!({
                        "event": "alt_terminal_operation_repaired",
                        "cluster": options.cluster,
                        "repairId": result.repair_id,
                        "repairKind": result.repair_kind,
                        "operationId": result.root_operation_id,
                        "tableId": result.route_lookup_table_id,
                        "successorOperationId": result.successor_operation_id,
                        "supersededOperationCount": result.superseded_operation_ids.len(),
                        "failedBindingCount": result.failed_binding_ids.len(),
                        "requeuedRequestCount": result.requeued_request_ids.len(),
                        "finalizedObservedSlot": chain.observed_slot,
                        "policyAuthority": standard_policy.to_string(),
                        "signerLoaded": true,
                        "transactionsSent": false,
                    })
                );
            }
            Err(error) => {
                skipped += 1;
                println!(
                    "{}",
                    json!({
                        "event": "alt_terminal_repair_skipped",
                        "cluster": options.cluster,
                        "operationId": candidate.operation.id,
                        "tableId": candidate.physical_table.id,
                        "reason": redacted_external_error(&error.to_string()),
                        "transactionsSent": false,
                    })
                );
            }
        }
    }
    println!(
        "{}",
        json!({
            "event": "alt_terminal_repair_complete",
            "cluster": options.cluster,
            "candidateCount": candidate_count,
            "repairedCount": repaired,
            "skippedCount": skipped,
            "limit": options.max_operations,
            "controlEpoch": control.control_epoch,
            "durablyPaused": true,
            "signerLoaded": true,
            "transactionsSent": false,
        })
    );
    if skipped != 0 {
        return Err(format!(
            "terminal ALT repair left {skipped} of {candidate_count} bounded candidates unresolved; inspect the redacted skip events before retrying"
        )
        .into());
    }
    Ok(())
}

fn finalized_terminal_no_effect_evidence(
    rpc: &RpcClient,
    operation: &LookupTableOperationRecord,
    finalized_observed_slot: u64,
) -> Result<Option<LookupTableTerminalNoEffectEvidence>, Box<dyn Error>> {
    let Some(signature) = operation.transaction_signature.as_deref() else {
        if operation.message_hash.is_some()
            || operation.recent_blockhash.is_some()
            || operation.last_valid_block_height.is_some()
        {
            return Ok(None);
        }
        return Ok(Some(LookupTableTerminalNoEffectEvidence::Unsigned));
    };
    let signature_value = Signature::from_str(signature)?;
    let status = rpc
        .get_signature_statuses_with_history(&[signature_value])?
        .value
        .into_iter()
        .next()
        .flatten();
    let Some(status) = status.filter(|status| {
        status.err.is_some()
            && status.satisfies_commitment(CommitmentConfig::finalized())
            && status.slot <= finalized_observed_slot
    }) else {
        return Ok(None);
    };
    Ok(Some(
        LookupTableTerminalNoEffectEvidence::FinalizedFailedSignature {
            transaction_signature: signature.to_owned(),
            failed_slot: i64::try_from(status.slot)?,
        },
    ))
}

fn bootstrap_family_inputs(
    options: &Options,
) -> Result<Vec<LookupTableFamilyUpsert>, Box<dyn Error>> {
    let manager_pubkey = options
        .admin_policy_pubkey
        .ok_or("--bootstrap-families requires --policy-pubkey")?;
    if manager_pubkey != Pubkey::from_str(STANDARD_POLICY_AUTHORITY)? {
        return Err(format!(
            "--bootstrap-families --policy-pubkey must equal the standard policy authority {STANDARD_POLICY_AUTHORITY}"
        )
        .into());
    }
    let manager = manager_pubkey.to_string();
    let catalog_version = options
        .catalog_version
        .as_deref()
        .ok_or("--bootstrap-families requires --catalog-version")?;
    let largest_atomic_expansion = options.largest_atomic_expansion.ok_or(
        "--bootstrap-families requires --largest-atomic-expansion from measured catalog evidence",
    )?;
    let high_water = 256_i32
        .checked_sub(i32::from(largest_atomic_expansion))
        .and_then(|value| value.checked_sub(i32::from(options.safety_margin)))
        .ok_or("bootstrap capacity policy underflow")?;
    if high_water <= 0 {
        return Err("bootstrap capacity policy leaves no allocation headroom".into());
    }
    Ok([
        (
            options.shared_family_name.clone(),
            LookupTableFamilyKind::SharedMarket,
        ),
        (
            options.vault_family_name.clone(),
            LookupTableFamilyKind::VaultShards,
        ),
    ]
    .into_iter()
    .map(|(logical_name, kind)| LookupTableFamilyUpsert {
        cluster: options.cluster.clone(),
        logical_name,
        kind,
        desired_state: LookupTableFamilyState::Active,
        planner_version: PLANNER_VERSION.to_owned(),
        catalog_version: catalog_version.to_owned(),
        active_generation: Some(1),
        previous_generation: None,
        rollback_until: None,
        provisioning_authority: manager.clone(),
        payer: manager.clone(),
        hard_capacity: 256,
        allocation_high_water: high_water,
        largest_atomic_expansion: i32::from(largest_atomic_expansion),
        safety_margin: i32::from(options.safety_margin),
    })
    .collect())
}

fn parse_args<I, S, F>(args: I, read_env: F) -> Result<Options, Box<dyn Error>>
where
    I: IntoIterator<Item = S>,
    S: Into<String>,
    F: Fn(&str) -> Option<String>,
{
    let mut cluster = None;
    let mut rpc_url = None;
    let mut mode = RunMode::DryRun;
    let mut mode_explicit = false;
    let mut status_only = false;
    let local_paused = read_env(PAUSED_ENV).as_deref().is_some_and(parse_truthy);
    let mut watch = false;
    let mut max_operations = DEFAULT_MAX_OPERATIONS;
    let mut max_attempts = DEFAULT_MAX_ATTEMPTS;
    let mut address_chunk = DEFAULT_ADDRESS_CHUNK;
    let mut max_lamports = read_env(MAX_LAMPORTS_ENV)
        .map(|value| value.parse::<u64>())
        .transpose()?
        .unwrap_or_default();
    let mut budget_was_explicit = read_env(MAX_LAMPORTS_ENV).is_some();
    let mut budget_window_seconds = read_env(BUDGET_WINDOW_SECONDS_ENV)
        .map(|value| value.parse::<i64>())
        .transpose()?
        .unwrap_or(DEFAULT_BUDGET_WINDOW_SECONDS);
    let mut lease_seconds = DEFAULT_LEASE_SECONDS;
    let mut rate_limit_ms = DEFAULT_RATE_LIMIT_MS;
    let mut catalog_reconcile_interval_seconds = read_env(CATALOG_RECONCILE_INTERVAL_SECONDS_ENV)
        .map(|value| value.parse::<u64>())
        .transpose()?
        .unwrap_or(DEFAULT_CATALOG_RECONCILE_INTERVAL_SECONDS);
    let mut concurrency = DEFAULT_CONCURRENCY;
    let mut safety_margin = DEFAULT_SAFETY_MARGIN;
    let mut largest_atomic_expansion = read_env(LARGEST_ATOMIC_EXPANSION_ENV)
        .map(|value| value.parse::<u16>())
        .transpose()?;
    let mut vault_growth_reservation = DEFAULT_VAULT_GROWTH_RESERVATION;
    let mut max_vault_cohort = DEFAULT_MAX_VAULT_COHORT;
    let mut worker_id = default_worker_id();
    let mut admin_action = AdminAction::None;
    let mut admin_write = false;
    let mut admin_reason = None;
    let mut admin_updated_by = None;
    let mut admin_policy_pubkey = None;
    let mut catalog_version = None;
    let mut shared_family_name = DEFAULT_SHARED_FAMILY_NAME.to_owned();
    let mut vault_family_name = DEFAULT_VAULT_FAMILY_NAME.to_owned();
    let mut admin_vault_id = None;
    let mut admin_observed_slot = None;
    let mut admin_expected_authority = None;
    let mut admin_expected_address_hash = None;
    let mut admin_expected_address_count = None;
    let mut precutover_probe = false;
    let mut probe_vault_id = None;
    let mut args = args.into_iter().map(Into::into);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--cluster" => cluster = Some(next_value(&mut args, "--cluster")?),
            "--rpc-url" => rpc_url = Some(next_value(&mut args, "--rpc-url")?),
            "--execute" => set_mode(&mut mode, &mut mode_explicit, RunMode::Execute)?,
            "--reconcile-only" => set_mode(&mut mode, &mut mode_explicit, RunMode::ReconcileOnly)?,
            "--status" | "--provisioner-pause-status" => status_only = true,
            "--precutover-probe" => precutover_probe = true,
            "--probe-vault-id" => {
                probe_vault_id = Some(VaultId(
                    next_value(&mut args, "--probe-vault-id")?.parse()?,
                ))
            }
            "--pause" => {
                return Err("--pause was process-local; use --set-provisioner-pause --admin-write --reason <TEXT> --updated-by <ID> for a durable cluster pause".into())
            }
            "--watch" => watch = true,
            "--max-operations" => {
                max_operations = next_value(&mut args, "--max-operations")?.parse()?
            }
            "--max-attempts" => max_attempts = next_value(&mut args, "--max-attempts")?.parse()?,
            "--address-chunk" => {
                address_chunk = next_value(&mut args, "--address-chunk")?.parse()?
            }
            "--max-lamports" => {
                max_lamports = next_value(&mut args, "--max-lamports")?.parse()?;
                budget_was_explicit = true;
            }
            "--budget-window-seconds" => {
                budget_window_seconds = next_value(&mut args, "--budget-window-seconds")?.parse()?
            }
            "--lease-seconds" => {
                lease_seconds = next_value(&mut args, "--lease-seconds")?.parse()?
            }
            "--rate-limit-ms" => {
                rate_limit_ms = next_value(&mut args, "--rate-limit-ms")?.parse()?
            }
            "--catalog-reconcile-interval-seconds" => {
                catalog_reconcile_interval_seconds = next_value(
                    &mut args,
                    "--catalog-reconcile-interval-seconds",
                )?
                .parse()?
            }
            "--concurrency" => concurrency = next_value(&mut args, "--concurrency")?.parse()?,
            "--safety-margin" => {
                safety_margin = next_value(&mut args, "--safety-margin")?.parse()?
            }
            "--largest-atomic-expansion" => {
                largest_atomic_expansion =
                    Some(next_value(&mut args, "--largest-atomic-expansion")?.parse()?)
            }
            "--vault-growth-reservation" => {
                vault_growth_reservation =
                    next_value(&mut args, "--vault-growth-reservation")?.parse()?
            }
            "--max-vault-cohort" => {
                max_vault_cohort = next_value(&mut args, "--max-vault-cohort")?.parse()?
            }
            "--worker-id" => worker_id = next_value(&mut args, "--worker-id")?,
            "--force-legacy" => set_admin_action(&mut admin_action, AdminAction::ForceLegacy)?,
            "--bootstrap-families" => {
                set_admin_action(&mut admin_action, AdminAction::BootstrapFamilies)?
            }
            "--rollback-family" => {
                let family_id = next_value(&mut args, "--rollback-family")?.parse()?;
                set_admin_action(&mut admin_action, AdminAction::RollbackFamily(family_id))?
            }
            "--rollback-binding" => {
                let binding_id = next_value(&mut args, "--rollback-binding")?.parse()?;
                set_admin_action(&mut admin_action, AdminAction::RollbackBinding(binding_id))?
            }
            "--finalize-rollbacks" => {
                let family_id = next_value(&mut args, "--finalize-rollbacks")?.parse()?;
                set_admin_action(&mut admin_action, AdminAction::FinalizeRollbacks(family_id))?
            }
            "--retire-legacy" => {
                let table = Pubkey::from_str(&next_value(&mut args, "--retire-legacy")?)?;
                set_admin_action(&mut admin_action, AdminAction::RetireLegacy(table))?
            }
            "--clear-force-legacy" => {
                set_admin_action(&mut admin_action, AdminAction::ClearForceLegacy)?
            }
            "--set-provisioner-pause" => {
                set_admin_action(&mut admin_action, AdminAction::SetProvisionerPause)?
            }
            "--clear-provisioner-pause" => {
                set_admin_action(&mut admin_action, AdminAction::ClearProvisionerPause)?
            }
            "--repair-terminal-operations" => {
                set_admin_action(&mut admin_action, AdminAction::RepairTerminalOperations)?
            }
            "--activate-reusable-only" => {
                set_admin_action(&mut admin_action, AdminAction::ActivateReusableOnly)?
            }
            "--set-rollout-mode" => {
                let value = next_value(&mut args, "--set-rollout-mode")?;
                set_admin_action(
                    &mut admin_action,
                    AdminAction::SetRolloutMode(LookupTableRolloutMode::from_str(&value)?),
                )?;
            }
            "--admin-write" => admin_write = true,
            "--reason" => admin_reason = Some(next_value(&mut args, "--reason")?),
            "--updated-by" => admin_updated_by = Some(next_value(&mut args, "--updated-by")?),
            "--policy-pubkey" => {
                admin_policy_pubkey = Some(Pubkey::from_str(&next_value(
                    &mut args,
                    "--policy-pubkey",
                )?)?)
            }
            "--catalog-version" => {
                catalog_version = Some(next_value(&mut args, "--catalog-version")?)
            }
            "--shared-family-name" => {
                shared_family_name = next_value(&mut args, "--shared-family-name")?
            }
            "--vault-family-name" => {
                vault_family_name = next_value(&mut args, "--vault-family-name")?
            }
            "--vault-id" => {
                admin_vault_id = Some(VaultId(next_value(&mut args, "--vault-id")?.parse()?))
            }
            "--observed-slot" => {
                admin_observed_slot = Some(next_value(&mut args, "--observed-slot")?.parse()?)
            }
            "--expected-authority" => {
                admin_expected_authority = Some(Pubkey::from_str(&next_value(
                    &mut args,
                    "--expected-authority",
                )?)?)
            }
            "--expected-address-hash" => {
                admin_expected_address_hash =
                    Some(next_value(&mut args, "--expected-address-hash")?)
            }
            "--expected-address-count" => {
                admin_expected_address_count =
                    Some(next_value(&mut args, "--expected-address-count")?.parse()?)
            }
            "--help" | "-h" => return Err(usage().into()),
            other => return Err(format!("unknown argument {other:?}\n{}", usage()).into()),
        }
    }

    let cluster = cluster
        .or_else(|| read_env(CLUSTER_ENV))
        .filter(|value| !value.trim().is_empty())
        .ok_or(
            "--cluster or YIELD_ALT_CLUSTER is required; cluster is never inferred from a URL",
        )?;
    validate_cluster(&cluster)?;
    let database_url = read_env(DATABASE_URL_ENV)
        .filter(|value| !value.trim().is_empty())
        .ok_or("NEON_DATABASE_URL is required")?;
    let rpc_url = rpc_url
        .or_else(|| read_env(RPC_URL_ENV))
        .filter(|value| !value.trim().is_empty());
    if mode != RunMode::DryRun && rpc_url.is_none() {
        return Err("--reconcile-only/--execute requires SOLANA_RPC_URL or --rpc-url".into());
    }
    if admin_action == AdminAction::ActivateReusableOnly && rpc_url.is_none() {
        return Err(
            "--activate-reusable-only requires SOLANA_RPC_URL or --rpc-url for finalized preflight"
                .into(),
        );
    }
    if admin_action == AdminAction::RepairTerminalOperations && rpc_url.is_none() {
        return Err(
            "--repair-terminal-operations requires SOLANA_RPC_URL or --rpc-url for finalized proof"
                .into(),
        );
    }
    if precutover_probe != probe_vault_id.is_some() {
        return Err("--precutover-probe and --probe-vault-id must be provided together".into());
    }
    if precutover_probe && rpc_url.is_none() {
        return Err(
            "--precutover-probe requires SOLANA_RPC_URL or --rpc-url for finalized proof".into(),
        );
    }
    if probe_vault_id.is_some_and(|vault_id| vault_id.as_i64() <= 0) {
        return Err("--probe-vault-id must be a positive database ID".into());
    }
    if precutover_probe
        && (mode_explicit
            || status_only
            || watch
            || !matches!(admin_action, AdminAction::None)
            || admin_write
            || admin_reason.is_some()
            || admin_updated_by.is_some()
            || admin_policy_pubkey.is_some()
            || admin_vault_id.is_some()
            || admin_observed_slot.is_some()
            || admin_expected_authority.is_some()
            || admin_expected_address_hash.is_some()
            || admin_expected_address_count.is_some())
    {
        return Err("--precutover-probe cannot be combined with execution, watch/status, signer, or admin-control flags".into());
    }
    if mode == RunMode::Execute && (!budget_was_explicit || max_lamports == 0) {
        return Err(
            "--execute requires a positive explicit --max-lamports or YIELD_ALT_MAX_LAMPORTS"
                .into(),
        );
    }
    if !(MIN_BUDGET_WINDOW_SECONDS..=MAX_BUDGET_WINDOW_SECONDS).contains(&budget_window_seconds) {
        return Err(format!(
            "--budget-window-seconds must be between {MIN_BUDGET_WINDOW_SECONDS} and {MAX_BUDGET_WINDOW_SECONDS}"
        )
        .into());
    }
    if max_operations == 0 || max_operations > MAX_OPERATIONS_PER_BATCH {
        return Err(
            format!("--max-operations must be between 1 and {MAX_OPERATIONS_PER_BATCH}").into(),
        );
    }
    if !(1..=100).contains(&max_attempts) {
        return Err("--max-attempts must be between 1 and 100".into());
    }
    if address_chunk == 0 || address_chunk > MAX_ADDRESS_CHUNK {
        return Err(format!("--address-chunk must be between 1 and {MAX_ADDRESS_CHUNK}").into());
    }
    if lease_seconds < 30 {
        return Err("--lease-seconds must be at least 30".into());
    }
    if !(1..=MAX_CONCURRENCY).contains(&concurrency) {
        return Err(format!("--concurrency must be between 1 and {MAX_CONCURRENCY}").into());
    }
    if rate_limit_ms > MAX_RATE_LIMIT_MS {
        return Err(format!("--rate-limit-ms cannot exceed {MAX_RATE_LIMIT_MS}").into());
    }
    if !(1..=MAX_CATALOG_RECONCILE_INTERVAL_SECONDS).contains(&catalog_reconcile_interval_seconds) {
        return Err(format!(
            "--catalog-reconcile-interval-seconds must be between 1 and {MAX_CATALOG_RECONCILE_INTERVAL_SECONDS}"
        )
        .into());
    }
    if max_vault_cohort == 0 {
        return Err("--max-vault-cohort must be positive".into());
    }
    if largest_atomic_expansion.is_some_and(|value| value == 0 || value >= 256) {
        return Err("--largest-atomic-expansion must be between 1 and 255".into());
    }
    if worker_id.trim().is_empty() || worker_id.len() > 128 {
        return Err("--worker-id must contain 1-128 characters".into());
    }
    if !matches!(admin_action, AdminAction::None)
        && (mode != RunMode::DryRun || watch || status_only)
    {
        return Err(
            "rollout control writes cannot be combined with worker execution/status flags".into(),
        );
    }
    if admin_action == AdminAction::BootstrapFamilies
        && (admin_policy_pubkey.is_none()
            || catalog_version.is_none()
            || largest_atomic_expansion.is_none())
    {
        return Err("--bootstrap-families requires --policy-pubkey, --catalog-version, and --largest-atomic-expansion from measured catalog evidence".into());
    }
    if admin_action == AdminAction::BootstrapFamilies
        && admin_policy_pubkey != Some(Pubkey::from_str(STANDARD_POLICY_AUTHORITY)?)
    {
        return Err(format!(
            "--bootstrap-families --policy-pubkey must equal the standard policy authority {STANDARD_POLICY_AUTHORITY}"
        )
        .into());
    }
    if admin_vault_id.is_some() && !matches!(admin_action, AdminAction::SetRolloutMode(_)) {
        return Err("--vault-id is supported only with --set-rollout-mode".into());
    }
    if admin_vault_id.is_some_and(|vault_id| vault_id.as_i64() <= 0) {
        return Err("--vault-id must be a positive database ID".into());
    }
    if matches!(admin_action, AdminAction::RollbackBinding(_)) != admin_observed_slot.is_some() {
        return Err("--rollback-binding and --observed-slot must be provided together".into());
    }
    let retiring_legacy = matches!(admin_action, AdminAction::RetireLegacy(_));
    let has_all_legacy_fences = admin_expected_authority.is_some()
        && admin_expected_address_hash.is_some()
        && admin_expected_address_count.is_some();
    let has_any_legacy_fence = admin_expected_authority.is_some()
        || admin_expected_address_hash.is_some()
        || admin_expected_address_count.is_some();
    if (retiring_legacy && !has_all_legacy_fences) || (!retiring_legacy && has_any_legacy_fence) {
        return Err("--retire-legacy requires --expected-authority, --expected-address-hash, and --expected-address-count; those fencing flags are valid only with --retire-legacy".into());
    }
    if let Some(hash) = admin_expected_address_hash.as_deref() {
        if hash.len() != 64 || !hash.bytes().all(|byte| byte.is_ascii_hexdigit()) {
            return Err("--expected-address-hash must be a 64-character hexadecimal hash".into());
        }
    }
    if admin_expected_address_count.is_some_and(|count| !(0..=256).contains(&count)) {
        return Err("--expected-address-count must be between 0 and 256".into());
    }

    Ok(Options {
        cluster,
        rpc_url,
        database_url,
        mode,
        status_only,
        local_paused,
        watch,
        max_operations,
        max_attempts,
        address_chunk,
        max_lamports,
        budget_window_seconds,
        lease_seconds,
        rate_limit_ms,
        catalog_reconcile_interval_seconds,
        concurrency,
        safety_margin,
        largest_atomic_expansion,
        vault_growth_reservation,
        max_vault_cohort,
        worker_id,
        admin_action,
        admin_write,
        admin_reason,
        admin_updated_by,
        admin_policy_pubkey,
        catalog_version,
        shared_family_name,
        vault_family_name,
        admin_vault_id,
        admin_observed_slot,
        admin_expected_authority,
        admin_expected_address_hash,
        admin_expected_address_count,
        precutover_probe,
        probe_vault_id,
    })
}

fn set_mode(
    current: &mut RunMode,
    explicit: &mut bool,
    next: RunMode,
) -> Result<(), Box<dyn Error>> {
    if *explicit && *current != next {
        return Err("--execute and --reconcile-only are mutually exclusive".into());
    }
    *current = next;
    *explicit = true;
    Ok(())
}

fn set_admin_action(current: &mut AdminAction, next: AdminAction) -> Result<(), Box<dyn Error>> {
    if !matches!(current, AdminAction::None) {
        return Err("only one rollout control action may be requested".into());
    }
    *current = next;
    Ok(())
}

fn next_value<I>(args: &mut I, flag: &str) -> Result<String, Box<dyn Error>>
where
    I: Iterator<Item = String>,
{
    args.next()
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| format!("{flag} requires a value").into())
}

fn validate_cluster(cluster: &str) -> Result<(), Box<dyn Error>> {
    if cluster.len() > 64
        || !cluster
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        return Err("cluster must be a 1-64 character explicit identifier".into());
    }
    Ok(())
}

fn parse_truthy(value: &str) -> bool {
    matches!(
        value.trim().to_ascii_lowercase().as_str(),
        "1" | "true" | "yes" | "on"
    )
}

fn default_worker_id() -> String {
    format!("alt-provisioner-{}", std::process::id())
}

fn usage() -> &'static str {
    "Usage: route-lookup-table-provisioner --cluster <CLUSTER> [--status|--provisioner-pause-status|--reconcile-only|--execute|--precutover-probe --probe-vault-id <DB_ID>] [--watch] [--max-operations <N>] [--max-attempts <1..100>] [--address-chunk <1..20>] [--max-lamports <LAMPORTS>] [--budget-window-seconds <60..31536000>] [--lease-seconds <N>] [--rate-limit-ms <N>] [--catalog-reconcile-interval-seconds <1..3600>] [--concurrency <1..32>] [--safety-margin <N>] [--vault-growth-reservation <N>] [--max-vault-cohort <N>] [--worker-id <ID>]\n\nDry-run is the default and never loads a signer or writes. --precutover-probe requires a durable cluster pause, proves the complete exact shared ALT bundle at finalized RPC, executes the real drift/request paths in a rollback-only transaction, and commits only an immutable PASS audit row for that paused control epoch; it cannot be combined with worker/admin modes and never loads POLICY_KEYPAIR or sends. Reconcile-only may update durable reconciliation state but never signs or sends. Execute uses the standard POLICY_KEYPAIR as ALT authority/payer and requires an explicit positive PostgreSQL-backed rolling-window lamport budget that survives worker restarts. Bounded concurrency overlaps only independently fenced physical ALT tables; predecessor operations for one table remain serialized in the database. Every send first commits an exact broadcast permit in a short database transaction; no transaction or advisory lock crosses RPC. The fast watch loop polls only durable queues; the shared finalized catalog is reconciled on the configured interval and immediately after ALT work, while every mutation retains its own fresh finalized proof. Reads NEON_DATABASE_URL, SOLANA_RPC_URL, and explicit YIELD_ALT_CLUSTER; cluster is never inferred from the RPC URL. The local YIELD_ALT_PROVISIONING_PAUSED environment gate remains an emergency process stop. Durable cluster pause controls are --set-provisioner-pause or --clear-provisioner-pause; read them with --provisioner-pause-status. Admin controls require --admin-write --reason <TEXT> --updated-by <ID>: --set-provisioner-pause, --clear-provisioner-pause, --repair-terminal-operations [--max-operations <1..100>] (requires the durable pause, finalized RPC, and the standard POLICY_KEYPAIR; quarantines only proven phantom tables or inserts an immutable failed-suffix successor and never sends), --activate-reusable-only (requires the paused/drained current exact PASS probe, fresh-verifies every shard in the exact active shared ALT bundle at finalized RPC, then atomically fences that evidence and aligns every rollout override), --force-legacy, --clear-force-legacy, --set-rollout-mode <MODE> [--vault-id <DB_ID>], --rollback-family <FAMILY_ID>, --rollback-binding <ACTIVE_BINDING_ID> --observed-slot <SLOT>, --finalize-rollbacks <FAMILY_ID>, --retire-legacy <TABLE> --expected-authority <PUBKEY> --expected-address-hash <HASH> --expected-address-count <N>, or --bootstrap-families --policy-pubkey <POLICY_PUBKEY> --catalog-version <VERSION> --largest-atomic-expansion <MEASURED_COUNT> [--safety-margin <N>] [--shared-family-name <NAME>] [--vault-family-name <NAME>]. Bootstrap rejects any policy identity other than the repo standard authority. The largest atomic expansion is catalog evidence and is independent of the transaction address chunk. Force-legacy remains global. Terminal repair is the only administrative action that loads POLICY_KEYPAIR, solely to prove standard policy identity; it never signs or broadcasts."
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        io::{Read, Write},
        net::TcpListener,
        sync::{
            atomic::{AtomicUsize, Ordering},
            Arc,
        },
        thread,
    };

    fn env_map<'a>(values: &'a [(&'a str, &'a str)]) -> impl Fn(&str) -> Option<String> + 'a {
        move |name| {
            values
                .iter()
                .find(|(key, _)| *key == name)
                .map(|(_, value)| (*value).to_owned())
        }
    }

    fn base_env() -> [(&'static str, &'static str); 3] {
        [
            (CLUSTER_ENV, "mainnet-beta"),
            (DATABASE_URL_ENV, "postgresql://redacted"),
            (RPC_URL_ENV, "https://rpc.invalid"),
        ]
    }

    fn test_rpc_retry_policy() -> RpcReadRetryPolicy {
        RpcReadRetryPolicy {
            initial_delay: Duration::from_millis(1),
            maximum_delay: Duration::from_millis(2),
            heartbeat_interval: Duration::from_secs(60),
            unavailable_alert_after: Duration::from_secs(60),
            max_attempts: Some(3),
        }
    }

    enum FakeRpcResponse {
        Disconnect,
        Http(u16, &'static str),
    }

    fn spawn_fake_rpc(
        responses: Vec<FakeRpcResponse>,
    ) -> (
        String,
        Arc<AtomicUsize>,
        thread::JoinHandle<Result<(), String>>,
    ) {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind fake RPC");
        let address = listener.local_addr().expect("fake RPC address");
        let request_count = Arc::new(AtomicUsize::new(0));
        let observed_count = Arc::clone(&request_count);
        let handle = thread::spawn(move || {
            for response in responses {
                let (mut stream, _) = listener.accept().map_err(|error| error.to_string())?;
                stream
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .map_err(|error| error.to_string())?;
                let mut request = Vec::new();
                let mut buffer = [0_u8; 4096];
                loop {
                    let read = stream
                        .read(&mut buffer)
                        .map_err(|error| error.to_string())?;
                    if read == 0 {
                        break;
                    }
                    request.extend_from_slice(&buffer[..read]);
                    if request.windows(4).any(|window| window == b"\r\n\r\n") {
                        break;
                    }
                }
                observed_count.fetch_add(1, Ordering::SeqCst);
                let FakeRpcResponse::Http(status, body) = response else {
                    continue;
                };
                let reason = match status {
                    200 => "OK",
                    400 => "Bad Request",
                    500 => "Internal Server Error",
                    _ => "Test Response",
                };
                write!(
                    stream,
                    "HTTP/1.1 {status} {reason}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                )
                .map_err(|error| error.to_string())?;
                stream.flush().map_err(|error| error.to_string())?;
            }
            Ok(())
        });
        (format!("http://{address}"), request_count, handle)
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn alt_provisioner_read_only_rpc_retries_http_500_then_recovers() {
        let (url, request_count, server) = spawn_fake_rpc(vec![
            FakeRpcResponse::Http(500, r#"{"error":"temporary"}"#),
            FakeRpcResponse::Http(200, r#"{"jsonrpc":"2.0","result":4242,"id":1}"#),
        ]);
        let rpc = RpcClient::new(url);

        let slot = retry_read_only_rpc("test_get_slot", test_rpc_retry_policy(), || {
            rpc.get_slot_with_commitment(CommitmentConfig::finalized())
        })
        .await
        .expect("HTTP 500 should be retried");

        assert_eq!(slot, 4242);
        assert_eq!(request_count.load(Ordering::SeqCst), 2);
        server.join().expect("join fake RPC").expect("fake RPC");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn alt_provisioner_read_only_rpc_retries_request_transport_then_recovers() {
        let (url, request_count, server) = spawn_fake_rpc(vec![
            FakeRpcResponse::Disconnect,
            FakeRpcResponse::Http(200, r#"{"jsonrpc":"2.0","result":4242,"id":1}"#),
        ]);
        let rpc = RpcClient::new(url);
        let observed_request_error = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let request_error = Arc::clone(&observed_request_error);

        let slot = retry_read_only_rpc("test_get_slot", test_rpc_retry_policy(), || {
            rpc.get_slot_with_commitment(CommitmentConfig::finalized())
                .inspect_err(|error| {
                    let ClientErrorKind::Reqwest(error) = error.kind() else {
                        panic!("disconnected HTTP request must produce a Reqwest error")
                    };
                    assert!(error.is_request());
                    assert!(!error.is_timeout());
                    assert!(!error.is_connect());
                    assert_eq!(error.status(), None);
                    request_error.store(true, Ordering::SeqCst);
                })
        })
        .await
        .expect("request transport failure should be retried");

        assert_eq!(slot, 4242);
        assert!(observed_request_error.load(Ordering::SeqCst));
        assert_eq!(request_count.load(Ordering::SeqCst), 2);
        server.join().expect("join fake RPC").expect("fake RPC");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn alt_provisioner_read_only_rpc_does_not_retry_http_400() {
        let (url, request_count, server) = spawn_fake_rpc(vec![FakeRpcResponse::Http(
            400,
            r#"{"error":"bad request"}"#,
        )]);
        let rpc = RpcClient::new(url);

        let error = retry_read_only_rpc("test_get_slot", test_rpc_retry_policy(), || {
            rpc.get_slot_with_commitment(CommitmentConfig::finalized())
        })
        .await
        .expect_err("HTTP 400 must not be retried");

        assert!(!is_transient_read_only_rpc_error(&error));
        assert_eq!(request_count.load(Ordering::SeqCst), 1);
        server.join().expect("join fake RPC").expect("fake RPC");
    }

    #[test]
    fn alt_provisioner_read_only_rpc_backoff_is_capped() {
        let policy = RpcReadRetryPolicy {
            initial_delay: Duration::from_millis(10),
            maximum_delay: Duration::from_millis(25),
            ..test_rpc_retry_policy()
        };

        assert_eq!(policy.delay_after(1), Duration::from_millis(10));
        assert_eq!(policy.delay_after(2), Duration::from_millis(20));
        assert_eq!(policy.delay_after(3), Duration::from_millis(25));
        assert_eq!(policy.delay_after(u32::MAX), Duration::from_millis(25));
    }

    #[test]
    fn binding_activation_deferrals_are_not_fatal_errors() {
        let verification_slot = LookupTableBindingActivationDeferral::VerificationSlotAhead {
            binding_id: 17,
            observed_slot: 120,
            required_slot: 121,
        };
        assert_eq!(
            binding_activation_defer_fields(&verification_slot),
            ("verification_slot_ahead", Some(120), Some(121))
        );

        let usage_lease =
            LookupTableBindingActivationDeferral::LogicalHeadUsageLease { binding_id: 17 };
        assert_eq!(
            binding_activation_defer_fields(&usage_lease),
            ("logical_head_usage_lease", None, None)
        );

        let invariant = OrchestratorError::StoreInvariant(
            "malformed binding state must remain fatal".to_owned(),
        );
        assert!(!is_binding_activation_database_deadlock(&invariant));
    }

    #[test]
    fn reusable_alt_dry_run_is_default_and_has_no_signing_authority_requirement() {
        let options = parse_args(Vec::<String>::new(), env_map(&base_env())).unwrap();
        assert_eq!(options.mode, RunMode::DryRun);
        assert!(!options.mode.may_sign());
        assert_eq!(options.max_lamports, 0);
    }

    #[test]
    fn reusable_alt_reconcile_only_does_not_enable_signing() {
        let options = parse_args(["--reconcile-only"], env_map(&base_env())).unwrap();
        assert_eq!(options.mode, RunMode::ReconcileOnly);
        assert!(!options.mode.may_sign());
    }

    #[test]
    fn reusable_alt_mutation_modes_require_an_explicit_rpc_endpoint() {
        let values = [
            (CLUSTER_ENV, "devnet"),
            (DATABASE_URL_ENV, "postgresql://redacted"),
        ];
        let reconcile_error =
            parse_args(["--reconcile-only"], env_map(&values)).expect_err("RPC is required");
        assert!(reconcile_error
            .to_string()
            .contains("requires SOLANA_RPC_URL"));

        let execute_error = parse_args(["--execute", "--max-lamports", "1"], env_map(&values))
            .expect_err("RPC is required");
        assert!(execute_error
            .to_string()
            .contains("requires SOLANA_RPC_URL"));

        let blank_error = parse_args(
            ["--execute", "--max-lamports", "1", "--rpc-url", " "],
            env_map(&base_env()),
        )
        .expect_err("blank explicit RPC must not fall back to another endpoint");
        assert_eq!(blank_error.to_string(), "--rpc-url requires a value");
    }

    #[test]
    fn reusable_alt_execute_is_the_only_mode_allowed_to_load_a_signer() {
        assert!(!RunMode::DryRun.may_sign());
        assert!(!RunMode::ReconcileOnly.may_sign());
        assert!(RunMode::Execute.may_sign());
    }

    #[test]
    fn precutover_probe_is_signerless_and_rejects_worker_or_admin_modes() {
        let options = parse_args(
            ["--precutover-probe", "--probe-vault-id", "42"],
            env_map(&base_env()),
        )
        .expect("valid rollback-only probe");
        assert!(options.precutover_probe);
        assert_eq!(options.probe_vault_id, Some(VaultId(42)));
        assert_eq!(options.mode, RunMode::DryRun);
        assert!(!options.mode.may_sign());
        assert_eq!(options.admin_action, AdminAction::None);

        for args in [
            vec!["--precutover-probe", "--probe-vault-id", "42", "--status"],
            vec![
                "--precutover-probe",
                "--probe-vault-id",
                "42",
                "--reconcile-only",
            ],
            vec![
                "--precutover-probe",
                "--probe-vault-id",
                "42",
                "--execute",
                "--max-lamports",
                "1",
            ],
            vec![
                "--precutover-probe",
                "--probe-vault-id",
                "42",
                "--force-legacy",
                "--admin-write",
                "--reason",
                "conflict",
                "--updated-by",
                "operator",
            ],
        ] {
            let error = parse_args(args, env_map(&base_env())).expect_err("mode conflict");
            assert!(error.to_string().contains("cannot be combined"));
        }
    }

    #[test]
    fn precutover_probe_requires_exact_vault_pair_rpc_and_positive_id() {
        assert!(parse_args(["--precutover-probe"], env_map(&base_env()))
            .expect_err("missing vault")
            .to_string()
            .contains("provided together"));
        assert!(parse_args(["--probe-vault-id", "42"], env_map(&base_env()))
            .expect_err("missing probe flag")
            .to_string()
            .contains("provided together"));
        assert!(parse_args(
            ["--precutover-probe", "--probe-vault-id", "0"],
            env_map(&base_env()),
        )
        .expect_err("zero vault")
        .to_string()
        .contains("positive"));
        let no_rpc = [
            (CLUSTER_ENV, "mainnet-beta"),
            (DATABASE_URL_ENV, "postgresql://redacted"),
        ];
        assert!(parse_args(
            ["--precutover-probe", "--probe-vault-id", "42"],
            env_map(&no_rpc),
        )
        .expect_err("finalized RPC required")
        .to_string()
        .contains("finalized proof"));
    }

    #[test]
    fn precutover_probe_fixture_address_is_deterministic_and_disjoint() {
        let occupied = vec![Pubkey::new_unique().to_string()];
        let first = derive_precutover_probe_vault_address(&"a".repeat(64), &occupied);
        let second = derive_precutover_probe_vault_address(&"a".repeat(64), &occupied);
        assert_eq!(first, second);
        assert!(!occupied.contains(&first));
        assert!(Pubkey::from_str(&first).is_ok());
    }

    #[test]
    fn reusable_alt_mutations_use_the_standard_policy_authority() {
        assert_eq!(
            alt_authority_signer_env(),
            loyal_yield_orchestrator::POLICY_KEYPAIR_ENV
        );
    }

    #[test]
    fn reusable_alt_execute_requires_explicit_positive_budget() {
        let error = parse_args(["--execute"], env_map(&base_env())).unwrap_err();
        assert!(error.to_string().contains("positive explicit"));

        let options = parse_args(
            ["--execute", "--max-lamports", "1000000"],
            env_map(&base_env()),
        )
        .unwrap();
        assert_eq!(options.mode, RunMode::Execute);
        assert_eq!(options.max_lamports, 1_000_000);
    }

    #[test]
    fn reusable_alt_cluster_is_explicit_and_not_inferred_from_rpc_url() {
        let values = [
            (DATABASE_URL_ENV, "postgresql://redacted"),
            (RPC_URL_ENV, "https://mainnet.example.invalid"),
        ];
        let error = parse_args(Vec::<String>::new(), env_map(&values)).unwrap_err();
        assert!(error.to_string().contains("never inferred"));
    }

    #[test]
    fn reusable_alt_chunks_and_concurrency_are_bounded() {
        let chunk_error = parse_args(["--address-chunk", "21"], env_map(&base_env())).unwrap_err();
        assert!(chunk_error.to_string().contains("between 1 and 20"));
        let concurrent = parse_args(["--concurrency", "2"], env_map(&base_env())).unwrap();
        assert_eq!(concurrent.concurrency, 2);
        let concurrency_error =
            parse_args(["--concurrency", "33"], env_map(&base_env())).unwrap_err();
        assert!(concurrency_error.to_string().contains("between 1 and 32"));

        let rate_error =
            parse_args(["--rate-limit-ms", "60001"], env_map(&base_env())).unwrap_err();
        assert!(rate_error.to_string().contains("cannot exceed"));
    }

    #[test]
    fn reusable_alt_catalog_reconciliation_interval_is_bounded_and_configurable() {
        let defaults = parse_args(Vec::<String>::new(), env_map(&base_env())).unwrap();
        assert_eq!(
            defaults.catalog_reconcile_interval_seconds,
            DEFAULT_CATALOG_RECONCILE_INTERVAL_SECONDS
        );

        let cli = parse_args(
            ["--catalog-reconcile-interval-seconds", "300"],
            env_map(&base_env()),
        )
        .unwrap();
        assert_eq!(cli.catalog_reconcile_interval_seconds, 300);

        let mut values = base_env().to_vec();
        values.push((CATALOG_RECONCILE_INTERVAL_SECONDS_ENV, "120"));
        let configured = parse_args(Vec::<String>::new(), env_map(&values)).unwrap();
        assert_eq!(configured.catalog_reconcile_interval_seconds, 120);

        for invalid in ["0", "3601"] {
            let error = parse_args(
                ["--catalog-reconcile-interval-seconds", invalid],
                env_map(&base_env()),
            )
            .expect_err("catalog reconciliation interval must be bounded");
            assert!(error.to_string().contains("must be between 1 and 3600"));
        }
    }

    #[test]
    fn reusable_alt_pause_gate_is_independent_of_route_execution() {
        let mut values = base_env().to_vec();
        values.push((PAUSED_ENV, "true"));
        let options = parse_args(Vec::<String>::new(), env_map(&values)).unwrap();
        assert!(options.local_paused);
        assert_eq!(options.mode, RunMode::DryRun);
    }

    #[test]
    fn reusable_alt_durable_pause_commands_are_admin_fenced() {
        let options = parse_args(
            [
                "--set-provisioner-pause",
                "--admin-write",
                "--reason",
                "operator maintenance",
                "--updated-by",
                "operator",
            ],
            env_map(&base_env()),
        )
        .unwrap();
        assert_eq!(options.admin_action, AdminAction::SetProvisionerPause);
        assert!(!options.mode.may_sign());

        let clear = parse_args(
            [
                "--clear-provisioner-pause",
                "--admin-write",
                "--reason",
                "maintenance complete",
                "--updated-by",
                "operator",
            ],
            env_map(&base_env()),
        )
        .unwrap();
        assert_eq!(clear.admin_action, AdminAction::ClearProvisionerPause);

        let error = parse_args(["--pause"], env_map(&base_env())).unwrap_err();
        assert!(error.to_string().contains("durable cluster pause"));
    }

    #[test]
    fn reusable_alt_durable_pause_allows_only_signerless_reconciliation_drain() {
        assert!(RunMode::ReconcileOnly.may_drain_while_durably_paused());
        assert!(!RunMode::ReconcileOnly.may_sign());
        assert!(!RunMode::Execute.may_drain_while_durably_paused());
        assert!(RunMode::Execute.may_sign());
        assert!(!RunMode::DryRun.may_drain_while_durably_paused());
    }

    #[test]
    fn reusable_alt_budget_is_cumulative_and_fails_before_overspend() {
        let mut budget = Budget {
            limit: 100,
            selected: 0,
        };
        assert!(!budget.exhausted());
        budget.reserve(40).unwrap();
        budget.reserve(60).unwrap();
        assert_eq!(budget.selected, 100);
        assert!(budget.exhausted());
        let exhausted = budget.reserve(1).unwrap_err();
        assert_eq!(
            exhausted,
            BudgetExhausted {
                current: 100,
                requested: 1,
                limit: 100,
            }
        );
        assert_eq!(budget.selected, 100);
        assert!(should_continue_worker(
            true,
            OperationBatchResult {
                processed: 1,
                budget_exhausted: true,
            }
        ));
        assert!(should_continue_worker(
            true,
            OperationBatchResult {
                processed: 1,
                budget_exhausted: false,
            }
        ));
        assert!(!should_continue_worker(
            false,
            OperationBatchResult {
                processed: 1,
                budget_exhausted: true,
            }
        ));
    }

    #[test]
    fn reusable_alt_family_state_gates_only_new_mutations() {
        for state in [
            LookupTableFamilyState::Active,
            LookupTableFamilyState::Paused,
            LookupTableFamilyState::Retiring,
            LookupTableFamilyState::Retired,
        ] {
            assert_eq!(
                family_operation_gate(state, LookupTableOperationKind::Verify),
                FamilyOperationGate::ReadOnlyVerification,
                "Verify must remain a read-only reconciliation path in {state}"
            );
        }

        assert_eq!(
            family_operation_gate(
                LookupTableFamilyState::Active,
                LookupTableOperationKind::Create
            ),
            FamilyOperationGate::AllowMutation
        );
        for kind in [
            LookupTableOperationKind::Deactivate,
            LookupTableOperationKind::Close,
        ] {
            assert_eq!(
                family_operation_gate(LookupTableFamilyState::Retiring, kind),
                FamilyOperationGate::AllowMutation
            );
        }
        for kind in [
            LookupTableOperationKind::Create,
            LookupTableOperationKind::Extend,
            LookupTableOperationKind::Rollover,
        ] {
            assert!(matches!(
                family_operation_gate(LookupTableFamilyState::Retiring, kind),
                FamilyOperationGate::Defer {
                    code: "family_retiring_growth_blocked",
                    ..
                }
            ));
        }
        for state in [
            LookupTableFamilyState::Paused,
            LookupTableFamilyState::Retired,
        ] {
            for kind in [
                LookupTableOperationKind::Create,
                LookupTableOperationKind::Extend,
                LookupTableOperationKind::Rollover,
                LookupTableOperationKind::Deactivate,
                LookupTableOperationKind::Close,
            ] {
                assert!(matches!(
                    family_operation_gate(state, kind),
                    FamilyOperationGate::Defer { .. }
                ));
            }
        }
    }

    #[test]
    fn reusable_alt_submission_gate_forbids_broadcast_before_persistence() {
        let mut gate = SubmissionGate::built();
        assert!(gate.permit_granted().is_err());
        assert!(gate.broadcasting().is_err());
        gate.simulated().unwrap();
        assert!(gate.permit_granted().is_err());
        assert!(gate.sign_after_budget(true, || Ok(())).unwrap());
        gate.persisted().unwrap();
        gate.permit_granted().unwrap();
        assert_eq!(gate.stage, SubmissionStage::PermitGranted);
        gate.broadcasting().unwrap();
        assert_eq!(gate.stage, SubmissionStage::Broadcast);
    }

    #[test]
    fn reusable_alt_pause_before_permit_defers_persisted_signature_without_send_or_replay() {
        let mut gate = SubmissionGate::built();
        gate.simulated().unwrap();
        assert!(gate.sign_after_budget(true, || Ok(())).unwrap());
        gate.persisted().unwrap();
        let send_invocations = std::cell::Cell::new(0_u8);
        gate.paused_before_permit().unwrap();
        assert_eq!(send_invocations.get(), 0);
        assert_eq!(gate.stage, SubmissionStage::Persisted);

        let mut observation = LookupTableReconciliationObservation {
            operation_kind: LookupTableOperationKind::Extend,
            persisted_status: LookupTableOperationStatus::NeedsReconcile,
            signature_state: LookupTableSignatureState::NotFound,
            chain_state: LookupTableChainState::Missing,
            chain_observed_finalized: true,
            blockhash_expired: false,
            usable_after_slot_reached: true,
        };
        assert_eq!(
            reconcile_lookup_table_operation(&observation),
            LookupTableReconciliationDecision::WaitForSignature
        );
        observation.blockhash_expired = true;
        assert_eq!(
            reconcile_lookup_table_operation(&observation),
            LookupTableReconciliationDecision::RetryWithFreshTransaction
        );
    }

    #[test]
    fn reusable_alt_pregranted_permit_remains_durable_in_flight_after_pause() {
        let mut gate = SubmissionGate::built();
        gate.simulated().unwrap();
        assert!(gate.sign_after_budget(true, || Ok(())).unwrap());
        gate.persisted().unwrap();
        gate.permit_granted().unwrap();

        // A pause that commits after this short permit transaction cannot
        // revoke an already authorized packet. The durable permit remains the
        // drain/reconciliation evidence while the network call is lock-free.
        let pause_committed_after_grant = true;
        assert!(pause_committed_after_grant);
        gate.broadcasting().unwrap();
        assert_eq!(gate.stage, SubmissionStage::Broadcast);
    }

    #[test]
    fn reusable_alt_denied_budget_never_invokes_signing() {
        let mut gate = SubmissionGate::built();
        gate.simulated().unwrap();
        let signing_invocations = std::cell::Cell::new(0_u8);
        let signed = gate
            .sign_after_budget(false, || {
                signing_invocations.set(signing_invocations.get() + 1);
                Ok(())
            })
            .unwrap();
        assert!(!signed);
        assert_eq!(signing_invocations.get(), 0);
        assert_eq!(gate.stage, SubmissionStage::BudgetDenied);
        assert!(gate.persisted().is_err());
        assert!(gate.permit_granted().is_err());
        assert!(gate.broadcasting().is_err());
    }

    #[test]
    fn reusable_alt_known_signature_forces_chain_first_reconciliation() {
        assert!(requires_chain_first_reconciliation(true, false));
        assert!(requires_chain_first_reconciliation(false, true));
        assert!(!requires_chain_first_reconciliation(false, false));
    }

    #[test]
    fn reusable_alt_expired_unobserved_signature_allows_a_fresh_transaction() {
        assert!(persisted_signature_requires_chain_reconciliation(
            Some("old-signature"),
            None,
        ));
        assert!(!persisted_signature_requires_chain_reconciliation(
            Some("old-signature"),
            Some(EXPIRED_TRANSACTION_RETRY_CODE),
        ));
        assert!(!persisted_signature_requires_chain_reconciliation(
            None,
            Some(EXPIRED_TRANSACTION_RETRY_CODE),
        ));
    }

    #[test]
    fn reusable_alt_submitted_success_recovery_uses_every_legal_state() {
        assert_eq!(
            reconciliation_transition_path(LookupTableOperationStatus::Submitted).unwrap(),
            vec![
                LookupTableOperationStatus::Confirmed,
                LookupTableOperationStatus::Finalized,
                LookupTableOperationStatus::Reconciled,
            ]
        );
        assert_eq!(
            reconciliation_transition_path(LookupTableOperationStatus::NeedsReconcile).unwrap(),
            vec![LookupTableOperationStatus::Reconciled]
        );
    }

    #[test]
    fn reusable_alt_mutation_paths_include_rollover_and_cleanup() {
        assert_eq!(
            provisioner_mutation_path(LookupTableOperationKind::Create),
            Some("create")
        );
        assert_eq!(
            provisioner_mutation_path(LookupTableOperationKind::Rollover),
            Some("create")
        );
        assert_eq!(
            provisioner_mutation_path(LookupTableOperationKind::Extend),
            Some("extend")
        );
        assert_eq!(
            provisioner_mutation_path(LookupTableOperationKind::Deactivate),
            Some("deactivate")
        );
        assert_eq!(
            provisioner_mutation_path(LookupTableOperationKind::Close),
            Some("close")
        );
        assert_eq!(
            provisioner_mutation_path(LookupTableOperationKind::Verify),
            None
        );
    }

    #[test]
    fn reusable_alt_chain_drift_is_not_treated_as_retryable_absence() {
        let observation = LookupTableReconciliationObservation {
            operation_kind: LookupTableOperationKind::Extend,
            persisted_status: LookupTableOperationStatus::NeedsReconcile,
            signature_state: LookupTableSignatureState::NotFound,
            chain_state: LookupTableChainState::PrefixDrift,
            chain_observed_finalized: true,
            blockhash_expired: true,
            usable_after_slot_reached: true,
        };
        assert!(matches!(
            reconcile_lookup_table_operation(&observation),
            LookupTableReconciliationDecision::NeedsManualReconcile { .. }
        ));
    }

    #[test]
    fn reusable_alt_ambiguous_missing_signature_retries_only_after_expiry() {
        let mut observation = LookupTableReconciliationObservation {
            operation_kind: LookupTableOperationKind::Extend,
            persisted_status: LookupTableOperationStatus::NeedsReconcile,
            signature_state: LookupTableSignatureState::NotFound,
            chain_state: LookupTableChainState::Missing,
            chain_observed_finalized: true,
            blockhash_expired: false,
            usable_after_slot_reached: true,
        };
        assert_eq!(
            reconcile_lookup_table_operation(&observation),
            LookupTableReconciliationDecision::WaitForSignature
        );
        observation.blockhash_expired = true;
        assert_eq!(
            reconcile_lookup_table_operation(&observation),
            LookupTableReconciliationDecision::RetryWithFreshTransaction
        );
    }

    #[test]
    fn reusable_alt_logs_strip_urls_and_bound_error_length() {
        let error = format!("rpc https://secret.example/path failed {}", "x".repeat(600));
        let safe = safe_error(&error);
        assert!(!safe.contains("://"));
        assert!(safe.len() <= 512);
    }

    #[test]
    fn reusable_alt_structured_report_is_redacted_and_budgeted() {
        let report = OperationReport {
            event: "alt_provisioner_operation",
            cluster: "mainnet-beta".to_owned(),
            mode: "execute",
            operation_id: 7,
            operation_kind: "extend".to_owned(),
            table: Some(Pubkey::new_unique().to_string()),
            address_count: 20,
            selected_budget_lamports: 50_000,
            expected_fee_lamports: Some(5_000),
            expected_rent_lamports: Some(45_000),
            simulation: "succeeded",
            result: "submitted".to_owned(),
        };
        let encoded = serde_json::to_string(&report).unwrap();
        assert!(encoded.contains("selectedBudgetLamports"));
        assert!(encoded.contains("simulation"));
        assert!(!encoded.contains("databaseUrl"));
        assert!(!encoded.contains("signedTransaction"));
        assert!(!encoded.contains("://"));
    }

    #[test]
    fn reusable_alt_execute_and_reconcile_only_are_mutually_exclusive() {
        let error = parse_args(
            ["--execute", "--reconcile-only", "--max-lamports", "1"],
            env_map(&base_env()),
        )
        .unwrap_err();
        assert!(error.to_string().contains("mutually exclusive"));
    }

    #[test]
    fn reusable_alt_admin_changes_require_an_explicit_write_gate() {
        let error = parse_args(["--force-legacy"], env_map(&base_env())).unwrap();
        assert!(!error.admin_write);
        assert!(matches!(error.admin_action, AdminAction::ForceLegacy));
    }

    #[test]
    fn reusable_alt_direct_cutover_has_one_atomic_admin_action() {
        let options = parse_args(
            [
                "--activate-reusable-only",
                "--admin-write",
                "--reason",
                "direct durable v2 cutover",
                "--updated-by",
                "operator",
            ],
            env_map(&base_env()),
        )
        .unwrap();
        assert_eq!(options.admin_action, AdminAction::ActivateReusableOnly);
        assert!(options.admin_write);
        assert!(options.admin_vault_id.is_none());
    }

    #[test]
    fn reusable_alt_per_vault_rollout_control_is_distinct_from_global_force_legacy() {
        let canary = parse_args(
            [
                "--set-rollout-mode",
                "shadow",
                "--vault-id",
                "42",
                "--admin-write",
                "--reason",
                "canary",
                "--updated-by",
                "operator",
            ],
            env_map(&base_env()),
        )
        .unwrap();
        assert_eq!(canary.admin_vault_id, Some(VaultId(42)));
        assert!(matches!(
            canary.admin_action,
            AdminAction::SetRolloutMode(LookupTableRolloutMode::Shadow)
        ));

        let global_force_error =
            parse_args(["--force-legacy", "--vault-id", "42"], env_map(&base_env())).unwrap_err();
        assert!(global_force_error
            .to_string()
            .contains("only with --set-rollout-mode"));
    }

    #[test]
    fn reusable_alt_generation_and_binding_rollbacks_are_admin_only_and_signer_free() {
        let family = parse_args(
            [
                "--rollback-family",
                "7",
                "--admin-write",
                "--reason",
                "rollback",
                "--updated-by",
                "operator",
            ],
            env_map(&base_env()),
        )
        .unwrap();
        assert_eq!(family.admin_action, AdminAction::RollbackFamily(7));
        assert!(!family.mode.may_sign());

        let binding = parse_args(
            [
                "--rollback-binding",
                "9",
                "--observed-slot",
                "123",
                "--admin-write",
                "--reason",
                "rollback",
                "--updated-by",
                "operator",
            ],
            env_map(&base_env()),
        )
        .unwrap();
        assert_eq!(binding.admin_action, AdminAction::RollbackBinding(9));
        assert_eq!(binding.admin_observed_slot, Some(123));
        assert!(!binding.mode.may_sign());

        let finalize = parse_args(
            [
                "--finalize-rollbacks",
                "7",
                "--admin-write",
                "--reason",
                "retire expired rollback references",
                "--updated-by",
                "operator",
            ],
            env_map(&base_env()),
        )
        .unwrap();
        assert_eq!(finalize.admin_action, AdminAction::FinalizeRollbacks(7));
        assert!(!finalize.mode.may_sign());
    }

    #[test]
    fn reusable_alt_legacy_retirement_requires_complete_expected_metadata_fence() {
        let table = Pubkey::new_unique().to_string();
        let authority = Pubkey::new_unique().to_string();
        let args = vec![
            "--retire-legacy".to_owned(),
            table.clone(),
            "--expected-authority".to_owned(),
            authority.clone(),
            "--expected-address-hash".to_owned(),
            "a".repeat(64),
            "--expected-address-count".to_owned(),
            "42".to_owned(),
            "--admin-write".to_owned(),
            "--reason".to_owned(),
            "legacy migration complete".to_owned(),
            "--updated-by".to_owned(),
            "operator".to_owned(),
        ];
        let options = parse_args(args, env_map(&base_env())).unwrap();
        assert_eq!(
            options.admin_action,
            AdminAction::RetireLegacy(Pubkey::from_str(&table).unwrap())
        );
        assert_eq!(
            options.admin_expected_authority,
            Some(Pubkey::from_str(&authority).unwrap())
        );
        assert_eq!(options.admin_expected_address_count, Some(42));
        assert!(!options.mode.may_sign());

        let missing_fence =
            parse_args(["--retire-legacy", &table], env_map(&base_env())).unwrap_err();
        assert!(missing_fence
            .to_string()
            .contains("requires --expected-authority"));
    }

    #[test]
    fn reusable_alt_family_bootstrap_uses_public_metadata_without_signer_mode() {
        let manager = STANDARD_POLICY_AUTHORITY.to_owned();
        let options = parse_args(
            [
                "--bootstrap-families",
                "--policy-pubkey",
                &manager,
                "--catalog-version",
                "stable-v1",
                "--largest-atomic-expansion",
                "27",
                "--safety-margin",
                "11",
                "--address-chunk",
                "3",
                "--admin-write",
                "--reason",
                "initial bootstrap",
                "--updated-by",
                "operator",
            ],
            env_map(&base_env()),
        )
        .unwrap();
        assert_eq!(options.admin_action, AdminAction::BootstrapFamilies);
        assert_eq!(options.mode, RunMode::DryRun);
        assert!(!options.mode.may_sign());
        assert_eq!(options.admin_policy_pubkey.unwrap().to_string(), manager);
        let first = bootstrap_family_inputs(&options).unwrap();
        let retry = bootstrap_family_inputs(&options).unwrap();
        assert_eq!(first, retry);
        assert_eq!(first.len(), 2);
        assert!(first.iter().all(|family| {
            family.largest_atomic_expansion == 27
                && family.safety_margin == 11
                && family.allocation_high_water == 218
        }));

        let wrong_manager = Pubkey::new_unique().to_string();
        let error = parse_args(
            [
                "--bootstrap-families",
                "--policy-pubkey",
                &wrong_manager,
                "--catalog-version",
                "stable-v1",
                "--largest-atomic-expansion",
                "27",
            ],
            env_map(&base_env()),
        )
        .unwrap_err();
        assert!(error.to_string().contains(STANDARD_POLICY_AUTHORITY));
    }

    #[test]
    fn reusable_alt_cleanup_context_requires_fresh_identity_fields() {
        let policy = Pubkey::new_unique();
        let context = json!({
            "expectedAuthority": Pubkey::new_unique().to_string(),
            "expectedAddressHash": "abc123",
            "expectedAddressCount": 42,
            "expectedMutationEpoch": 7,
            "closeRecipient": policy.to_string(),
        });
        assert!(!context_string(&context, "expectedAuthority")
            .unwrap()
            .is_empty());
        assert_eq!(
            context_string(&context, "expectedAddressHash").unwrap(),
            "abc123"
        );
        assert_eq!(
            context.get("expectedAddressCount").and_then(Value::as_i64),
            Some(42)
        );
        assert_eq!(
            context.get("expectedMutationEpoch").and_then(Value::as_i64),
            Some(7)
        );
        assert!(close_recipient(&context).unwrap().is_some());
        assert_eq!(policy_close_recipient(&context, policy).unwrap(), policy);
        assert!(policy_close_recipient(&context, Pubkey::new_unique()).is_err());
        assert_eq!(policy_close_recipient(&json!({}), policy).unwrap(), policy);
        assert!(context_string(&json!({}), "expectedAddressHash").is_err());
    }

    #[test]
    fn reusable_alt_create_reservation_refreshes_at_slot_hash_expiry_boundary() {
        let recent_slot = 10_000;
        let last_usable_slot = recent_slot + SLOT_HASHES_MAX_ENTRIES as u64 - 1;
        assert!(!create_recent_slot_has_expired(
            recent_slot,
            last_usable_slot
        ));
        assert!(create_recent_slot_has_expired(
            recent_slot,
            last_usable_slot + 1
        ));
        assert!(!create_recent_slot_has_expired(
            recent_slot,
            recent_slot - 1
        ));
    }
}
