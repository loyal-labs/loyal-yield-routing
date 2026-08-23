use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::{fmt, time::Duration};

#[derive(Debug, Clone)]
pub struct NeonSqlConfig {
    pub url: String,
    pub max_connections: u32,
    pub acquire_timeout: Duration,
}

impl NeonSqlConfig {
    pub fn new(url: impl Into<String>) -> Self {
        Self {
            url: url.into(),
            max_connections: 5,
            acquire_timeout: Duration::from_secs(5),
        }
    }

    pub fn with_max_connections(mut self, max_connections: u32) -> Self {
        self.max_connections = max_connections;
        self
    }

    pub fn with_acquire_timeout(mut self, acquire_timeout: Duration) -> Self {
        self.acquire_timeout = acquire_timeout;
        self
    }
}

pub type OrchestratorConfig = NeonSqlConfig;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnMaxPolicySetProjectionInput {
    pub settings: String,
    pub vault_index: u8,
    pub vault: String,
    pub manifest_version: String,
    pub manifest_sha256: String,
    pub policy_seed_base: u64,
    pub status: String,
    pub policy_accounts: Value,
    pub observed_signature: String,
    pub observed_slot: u64,
    pub observed_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct MultiplyPositionSnapshotInput {
    pub route_key: String,
    pub generation: u64,
    pub observed_slot: u64,
    pub observed_at: DateTime<Utc>,
    pub strategy_key: Option<String>,
    pub claim_raw: u64,
    pub collateral_raw: u64,
    pub debt_raw: u64,
    pub equity_usd_micros: Option<String>,
    pub collateral_value_usd_micros: Option<String>,
    pub debt_value_usd_micros: Option<String>,
    pub leverage_bps: Option<u64>,
    pub ltv_bps: Option<u64>,
    pub health_factor_ppm: Option<u64>,
    pub supply_apy_bps: Option<u64>,
    pub borrow_apy_bps: Option<u64>,
    pub forecast_apy_bps: Option<i64>,
    pub valuation_source: Option<String>,
    pub valuation_slot: Option<u64>,
    pub valuation_observed_at: Option<DateTime<Utc>>,
    pub coverage_start_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct RouteLookupTable {
    pub id: i64,
    pub cluster: String,
    pub scope: String,
    pub table_address: String,
    pub authority: String,
    pub payer: String,
    pub status: String,
    pub durable: bool,
    pub address_count: i32,
    pub address_hash: String,
    pub addresses: Value,
    pub create_signature: Option<String>,
    pub extend_signatures: Value,
    pub last_extended_slot: Option<i64>,
    pub warmup_slot: Option<i64>,
    pub deactivated_slot: Option<i64>,
    pub deactivate_signature: Option<String>,
    pub closed_signature: Option<String>,
    pub close_recipient: Option<String>,
    pub reclaimed_lamports: Option<i64>,
    pub notes: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct RouteLookupTableUpsert {
    pub cluster: String,
    pub scope: String,
    pub table_address: String,
    pub authority: String,
    pub payer: String,
    pub status: String,
    pub durable: bool,
    pub address_count: i32,
    pub address_hash: String,
    pub addresses: Value,
    pub create_signature: Option<String>,
    pub extend_signatures: Value,
    pub last_extended_slot: Option<i64>,
    pub warmup_slot: Option<i64>,
    pub notes: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Hash)]
pub struct VaultId(pub i64);

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Hash)]
pub struct PolicyId(pub i64);

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Hash)]
pub struct SnapshotId(pub i64);

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Hash)]
pub struct DecisionId(pub i64);

impl VaultId {
    pub fn as_i64(self) -> i64 {
        self.0
    }
}

impl PolicyId {
    pub fn as_i64(self) -> i64 {
        self.0
    }
}

impl SnapshotId {
    pub fn as_i64(self) -> i64 {
        self.0
    }
}

impl DecisionId {
    pub fn as_i64(self) -> i64 {
        self.0
    }
}

impl fmt::Display for VaultId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

impl fmt::Display for PolicyId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

impl fmt::Display for SnapshotId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

impl fmt::Display for DecisionId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PolicyMatchInput {
    pub signature: String,
    pub slot: u64,
    pub cluster: String,
    pub source_commitment: String,
    pub settings: String,
    pub authority: String,
    pub policy_seed: u64,
    pub policy_account: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub delegated_signers: Vec<String>,
    pub threshold: u16,
    pub route_modes: Vec<String>,
    pub stable_mints: Vec<String>,
    pub kamino_markets: Vec<String>,
    pub kamino_liquidity_mints: Vec<String>,
    pub universe_preset: Option<String>,
    pub risk_profile: Option<String>,
    pub swap_lanes: Value,
}

/// Finality-aware event describing one immutable generalized swap policy.
///
/// This is deliberately the detector's strict semantic output. The policy's
/// canonical source/target universe and Jupiter dialects are on-chain policy
/// semantics, not store rows. A single account observation is the catalog
/// identity; planners authorize a pair against the observed source shard.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CrossMintSwapPolicyManifestInput {
    pub signature: String,
    pub slot: u64,
    pub cluster: String,
    pub source_commitment: String,
    pub mutation: String,
    pub settings: String,
    pub authority: String,
    pub policy_seed: Option<u64>,
    pub policy_account: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub delegated_signer: String,
    pub source_shard: String,
    pub max_slippage_bps: u16,
    pub daily_source_mint_spending_cap: u64,
    pub manifest_fingerprint: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CrossMintSwapPolicy {
    pub id: i64,
    pub cluster: String,
    pub settings: String,
    pub authority: String,
    pub policy_seed: Option<i64>,
    pub policy_account: String,
    pub vault_index: i16,
    pub vault_pubkey: String,
    pub delegated_signer: String,
    pub source_shard: String,
    pub max_slippage_bps: i32,
    pub daily_source_mint_spending_cap: i64,
    pub manifest_fingerprint: String,
    pub active: bool,
    pub start_eligible: bool,
    pub last_mutation: String,
    pub source_commitment: String,
    pub first_seen_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
    pub last_seen_slot: i64,
    pub last_seen_signature: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CrossMintSwapPolicyLookup {
    pub cluster: String,
    pub settings: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub minimum_slot: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CrossMintVaultOptInLookup {
    pub cluster: String,
    pub settings: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CrossMintVaultOptInUpsert {
    pub cluster: String,
    pub settings: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub enabled: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CrossMintVaultOptIn {
    pub cluster: String,
    pub settings: String,
    pub vault_index: i16,
    pub vault_pubkey: String,
    pub enabled: bool,
    pub generation: i64,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PolicyRemovalInput {
    pub signature: String,
    pub slot: u64,
    pub cluster: String,
    pub source_commitment: String,
    pub settings: String,
    pub authority: String,
    pub policy_account: String,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
pub struct PolicyRemovalResult {
    pub swap_policy_deactivated: bool,
    pub route_policy_deactivated: bool,
    pub managed_vault_deactivated: bool,
    pub balance_sweep_target_deactivated: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StoredPolicyMatch {
    pub policy: RoutePolicy,
    pub vault: ManagedVault,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Hash)]
pub struct BalanceSweepTargetId(pub i64);

impl BalanceSweepTargetId {
    pub fn as_i64(self) -> i64 {
        self.0
    }
}

impl fmt::Display for BalanceSweepTargetId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct BalanceSweepPolicyMatchInput {
    pub signature: String,
    pub slot: u64,
    pub settings: String,
    pub authority: String,
    pub policy_seed: u64,
    pub policy_account: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub wallet: String,
    pub wallet_usdc_ata: String,
    pub vault_usdc_ata: String,
    pub token_mint: String,
    pub wallet_token_ata: String,
    pub vault_token_ata: String,
    pub delegated_signers: Vec<String>,
    pub threshold: u16,
    pub max_amount_per_period: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct BalanceSweepTarget {
    pub id: BalanceSweepTargetId,
    pub settings: String,
    pub authority: String,
    pub policy_seed: i64,
    pub policy_account: String,
    pub vault_index: i16,
    pub vault_pubkey: String,
    pub wallet: String,
    pub wallet_usdc_ata: String,
    pub vault_usdc_ata: String,
    pub token_mint: String,
    pub wallet_token_ata: String,
    pub vault_token_ata: String,
    pub delegated_signers: Vec<String>,
    pub threshold: i32,
    pub max_amount_per_period: i64,
    pub active: bool,
    pub first_seen_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
    pub last_seen_slot: i64,
    pub last_seen_signature: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct WalletAtaBalanceUpdateInput {
    pub target_id: BalanceSweepTargetId,
    pub wallet: String,
    pub wallet_usdc_ata: String,
    pub wallet_token_ata: String,
    pub amount_raw: u64,
    pub owner: Option<String>,
    pub mint: String,
    pub observed_slot: u64,
    pub observed_at: Option<DateTime<Utc>>,
    pub source: String,
    pub source_commitment: String,
    pub txn_signature: Option<String>,
    pub account_data_hash: Option<String>,
    pub raw_evidence: Value,
}

/// One normalized LaserStream event and every Earn vault affected by it.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnReconciliationEnqueueInput {
    pub consumer_name: String,
    pub event_key: String,
    pub durable_slot: u64,
    pub event_payload: Value,
    pub vaults: Vec<EarnReconciliationVaultInput>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnReconciliationVaultInput {
    pub settings: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub vault_payload: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnReconciliationEnqueueOutcome {
    pub inserted_jobs: usize,
    pub cursor_slot: u64,
}

/// Authoritative durable state for Earn stream and reconciliation monitoring.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnReconciliationHealthSnapshot {
    pub cursor_slot: u64,
    pub pending_jobs: u64,
    pub failed_pending_jobs: u64,
    pub oldest_pending_age_seconds: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnReconciliationJob {
    pub id: i64,
    pub consumer_name: String,
    pub event_key: String,
    pub durable_slot: u64,
    pub event_payload: Value,
    pub vault_payload: Value,
    pub attempt_count: i32,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub enum EarnDirectMutation {
    PolicyOnly(EarnPolicyOnlyMutation),
    Deposit(EarnDepositMutation),
    Cleanup(EarnCleanupMutation),
    Noop,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnPolicyOnlyMutation {
    pub route_policy: PolicyMatchInput,
    pub setup_policy: PolicyMatchInput,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnDepositMutation {
    pub route_policy: PolicyMatchInput,
    pub setup_policy: Option<PolicyMatchInput>,
    pub deposit_signature: String,
    pub deposit_slot: u64,
    pub observed_slot: u64,
    pub deposit_mint: String,
    pub principal_amount_raw: u64,
    pub target_reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub target_supply_apy_bps: Option<i64>,
    pub wallet: String,
    pub smart_account_address: String,
    pub reserve_state: Vec<EarnReserveMutation>,
    pub idle_state: Vec<EarnIdleTokenMutation>,
    pub observed_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnReserveMutation {
    pub reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub amount_raw: u64,
    pub has_value: bool,
    pub supply_apy_bps: Option<i64>,
    pub borrow_apy_bps: Option<i64>,
    pub planning_metadata: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnIdleTokenMutation {
    pub mint: String,
    pub amount_raw: u64,
    pub owner: String,
    pub token_account: String,
    pub observed_slot: u64,
    pub observed_at: Option<DateTime<Utc>>,
    pub source_commitment: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnCleanupMutation {
    pub settings: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub cleanup_signature: String,
    pub confirmed_slot: u64,
    pub observed_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnReconciliationCompletionOutcome {
    pub applied_mutations: usize,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnReconciliationContext {
    pub route_policy: Option<PolicyMatchInput>,
    pub setup_policy: Option<PolicyMatchInput>,
    pub onboarding: Option<EarnOnboardingContext>,
    pub full_withdrawal: Option<EarnFullWithdrawalContext>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnOnboardingContext {
    pub status: String,
    pub deposit_signature: Option<String>,
    pub delegated_signer: String,
    pub route_policy_account: String,
    pub route_policy_seed: u64,
    pub route_policy_signature: Option<String>,
    pub route_policy_confirmed_slot: Option<u64>,
    pub setup_policy_account: Option<String>,
    pub setup_policy_seed: Option<u64>,
    pub setup_policy_signature: Option<String>,
    pub setup_policy_confirmed_slot: Option<u64>,
    pub target_reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnFullWithdrawalContext {
    pub signature: String,
    pub confirmed_slot: u64,
}

/// Database identity used to build the in-memory Earn LaserStream watch set.
/// Solana address derivation remains in the monitor so this store crate stays
/// free of Solana dependencies.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnSubscriptionTarget {
    pub environment: String,
    pub settings: String,
    pub wallet: String,
    pub vault_index: i16,
    pub vault_pubkey: Option<String>,
    pub policy_accounts: Vec<String>,
    pub markets: Vec<String>,
    pub autodeposit_accounts: Vec<String>,
    pub observation_start_slot: Option<u64>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct AutodepositTargetSnapshotContext {
    pub target_id: BalanceSweepTargetId,
    pub wallet: String,
    pub wallet_token_ata: String,
    pub policy_account: String,
    pub subscription_authority: String,
    pub recurring_delegation: String,
    pub setup_generation: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct AutodepositChainObservation {
    pub target_id: BalanceSweepTargetId,
    pub observation_slot: u64,
    pub observation_complete: bool,
    pub policy_valid: bool,
    pub subscription_authority_valid: bool,
    pub recurring_delegation_valid: bool,
    pub token_delegate_valid: bool,
    pub wallet_balance_raw: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct AutodepositChainObservationResult {
    pub target_id: BalanceSweepTargetId,
    pub chain_status: String,
    pub observation_slot: u64,
    pub bootstrap_generation: Option<i64>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct AutodepositRecurringDelegationObserved {
    pub wallet: String,
    pub vault_pubkey: String,
    pub subscription_authority: String,
    pub recurring_delegation: String,
    pub nonce: u64,
    pub amount_per_period: u64,
    pub period_length_seconds: u64,
    pub start_timestamp: i64,
    pub expiry_timestamp: i64,
    pub signature: String,
    pub slot: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct WalletAtaBalanceCurrent {
    pub target_id: BalanceSweepTargetId,
    pub wallet: String,
    pub wallet_usdc_ata: String,
    pub wallet_token_ata: String,
    pub amount_raw: i64,
    pub owner: Option<String>,
    pub mint: String,
    pub observed_slot: i64,
    pub observed_at: DateTime<Utc>,
    pub source: String,
    pub source_commitment: String,
    pub txn_signature: Option<String>,
    pub account_data_hash: Option<String>,
    pub raw_evidence: Value,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ProjectedWalletAtaBalanceUpdateInput {
    pub event_id: i64,
    pub update: WalletAtaBalanceUpdateInput,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ProjectionBatchOutcome {
    pub projected_count: usize,
    pub previous_event_id: i64,
    pub last_event_id: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PendingBalanceSweepSurplusLot {
    pub id: i64,
    pub target_id: BalanceSweepTargetId,
    pub source_event_id: i64,
    pub source_signature: Option<String>,
    pub source_mint: String,
    pub source_wallet_token_ata: String,
    pub classification: String,
    pub original_amount_raw: i64,
    pub remaining_amount_raw: i64,
    pub eligible_after: DateTime<Utc>,
    pub status: String,
    pub confidence: String,
    pub reason: String,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct BalanceSweepExecutionInput {
    pub target_id: BalanceSweepTargetId,
    pub signature: String,
    pub slot: u64,
    pub source_wallet_ata: String,
    pub destination_vault_ata: String,
    pub token_mint: String,
    pub source_token_ata: String,
    pub destination_token_ata: String,
    pub amount_raw: u64,
    pub source_pre_balance_raw: Option<u64>,
    pub source_post_balance_raw: Option<u64>,
    pub destination_pre_balance_raw: Option<u64>,
    pub destination_post_balance_raw: Option<u64>,
    pub source_commitment: String,
    pub raw_evidence: Value,
    pub decoded_evidence: Value,
    pub received_at: Option<DateTime<Utc>>,
    pub decoded_at: Option<DateTime<Utc>>,
    pub dedupe_key: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct BalanceSweepExecution {
    pub id: i64,
    pub target_id: BalanceSweepTargetId,
    pub signature: String,
    pub slot: i64,
    pub source_wallet_ata: String,
    pub destination_vault_ata: String,
    pub token_mint: String,
    pub source_token_ata: String,
    pub destination_token_ata: String,
    pub amount_raw: i64,
    pub source_pre_balance_raw: Option<i64>,
    pub source_post_balance_raw: Option<i64>,
    pub destination_pre_balance_raw: Option<i64>,
    pub destination_post_balance_raw: Option<i64>,
    pub source_commitment: String,
    pub raw_evidence: Value,
    pub decoded_evidence: Value,
    pub received_at: Option<DateTime<Utc>>,
    pub decoded_at: Option<DateTime<Utc>>,
    pub inserted_at: DateTime<Utc>,
    pub dedupe_key: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct RoutePolicy {
    pub id: PolicyId,
    pub cluster: String,
    pub source_commitment: String,
    pub finalized_eligible: bool,
    pub settings: String,
    pub authority: String,
    pub policy_seed: i64,
    pub policy_account: String,
    pub vault_index: i16,
    pub vault_pubkey: String,
    pub delegated_signers: Vec<String>,
    pub threshold: i32,
    pub route_modes: Vec<String>,
    pub stable_mints: Vec<String>,
    pub kamino_markets: Vec<String>,
    pub kamino_liquidity_mints: Vec<String>,
    pub universe_preset: Option<String>,
    pub risk_profile: Option<String>,
    pub swap_lanes: Value,
    pub active: bool,
    pub first_seen_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
    pub last_seen_slot: i64,
    pub last_seen_signature: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ManagedVault {
    pub id: VaultId,
    pub settings: String,
    pub vault_index: i16,
    pub vault_pubkey: String,
    pub active_policy_id: PolicyId,
    pub active: bool,
    pub first_seen_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ReconciledVaultState {
    pub observed_slot: i64,
    pub observed_at: Option<DateTime<Utc>>,
    pub chain_slot: Option<i64>,
    pub lock_attempt_id: Option<i64>,
    pub context: Value,
    pub positions: Vec<ReconciledReservePosition>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ReconciledReservePosition {
    pub reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub amount_raw: u64,
    pub supply_apy_bps: Option<i64>,
    pub borrow_apy_bps: Option<i64>,
    pub planning_metadata: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PositionSnapshot {
    pub id: SnapshotId,
    pub vault_id: VaultId,
    pub policy_id: PolicyId,
    pub observed_slot: i64,
    pub observed_at: DateTime<Utc>,
    pub chain_slot: Option<i64>,
    pub lock_attempt_id: Option<i64>,
    pub is_current: bool,
    pub context: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CurrentReservePosition {
    pub vault_id: VaultId,
    pub reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub amount_raw: i64,
    pub has_value: bool,
    pub supply_apy_bps: Option<i64>,
    pub borrow_apy_bps: Option<i64>,
    pub snapshot_id: SnapshotId,
    pub observed_slot: i64,
    pub observed_at: DateTime<Utc>,
    pub planning_metadata: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CurrentIdleTokenBalance {
    pub vault_id: VaultId,
    pub mint: String,
    pub amount_raw: i64,
    pub owner: String,
    pub token_account: String,
    pub observed_slot: i64,
    pub observed_at: DateTime<Utc>,
    pub source_commitment: String,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ReserveScore {
    pub reserve: String,
    pub supply_apy_bps: i64,
    pub borrow_apy_bps: Option<i64>,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
pub enum DecisionStatus {
    Planned,
    Simulating,
    Ready,
    Submitted,
    Confirming,
    Confirmed,
    Failed,
    Abandoned,
    Skipped,
}

impl DecisionStatus {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Planned => "planned",
            Self::Simulating => "simulating",
            Self::Ready => "ready",
            Self::Submitted => "submitted",
            Self::Confirming => "confirming",
            Self::Confirmed => "confirmed",
            Self::Failed => "failed",
            Self::Abandoned => "abandoned",
            Self::Skipped => "skipped",
        }
    }

    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "planned" => Some(Self::Planned),
            "simulating" => Some(Self::Simulating),
            "ready" => Some(Self::Ready),
            "submitted" => Some(Self::Submitted),
            "confirming" => Some(Self::Confirming),
            "confirmed" => Some(Self::Confirmed),
            "failed" => Some(Self::Failed),
            "abandoned" => Some(Self::Abandoned),
            "skipped" => Some(Self::Skipped),
            _ => None,
        }
    }

    pub fn is_terminal(self) -> bool {
        matches!(
            self,
            Self::Confirmed | Self::Failed | Self::Abandoned | Self::Skipped
        )
    }
}

impl fmt::Display for DecisionStatus {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DecisionReason {
    TargetSupplyApyExceedsSource,
    IdleVaultLiquidityAvailable,
    VoltrManagerOperation,
    ActiveDecision,
    NoValueSource,
    CrossMintOnly,
    NoSameMintEdge,
    UnsupportedAmountSemantics,
}

impl DecisionReason {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::TargetSupplyApyExceedsSource => "target_supply_apy_exceeds_source",
            Self::IdleVaultLiquidityAvailable => "idle_vault_liquidity_available",
            Self::VoltrManagerOperation => "voltr_manager_operation",
            Self::ActiveDecision => "active_decision",
            Self::NoValueSource => "no_value_source",
            Self::CrossMintOnly => "cross_mint_only",
            Self::NoSameMintEdge => "no_same_mint_edge",
            Self::UnsupportedAmountSemantics => "unsupported_amount_semantics",
        }
    }

    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "target_supply_apy_exceeds_source" => Some(Self::TargetSupplyApyExceedsSource),
            "idle_vault_liquidity_available" => Some(Self::IdleVaultLiquidityAvailable),
            "voltr_manager_operation" => Some(Self::VoltrManagerOperation),
            "active_decision" => Some(Self::ActiveDecision),
            "no_value_source" => Some(Self::NoValueSource),
            "cross_mint_only" => Some(Self::CrossMintOnly),
            "no_same_mint_edge" => Some(Self::NoSameMintEdge),
            "unsupported_amount_semantics" => Some(Self::UnsupportedAmountSemantics),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RebalanceDecision {
    pub id: DecisionId,
    pub vault_id: VaultId,
    pub source_snapshot_id: Option<SnapshotId>,
    pub status: DecisionStatus,
    pub source_reserve: Option<String>,
    pub target_reserve: Option<String>,
    pub liquidity_mint: Option<String>,
    pub source_liquidity_mint: Option<String>,
    pub target_liquidity_mint: Option<String>,
    pub amount_raw: Option<i64>,
    pub source_apy_bps: Option<i64>,
    pub target_apy_bps: Option<i64>,
    pub estimated_edge_bps: Option<i64>,
    pub estimated_cost_lamports: i64,
    pub decision_reason: DecisionReason,
    pub execution_plan: Value,
    pub abandon_reason: Option<String>,
    pub signature: Option<String>,
    pub submitted_slot: Option<i64>,
    pub confirmed_slot: Option<i64>,
    pub preflight_chain_slot: Option<i64>,
    pub post_snapshot_id: Option<SnapshotId>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PlannedRebalanceDecisionInput {
    pub source_snapshot_id: SnapshotId,
    pub source_reserve: String,
    pub target_reserve: String,
    pub source_liquidity_mint: String,
    pub target_liquidity_mint: String,
    pub amount_raw: i64,
    pub source_apy_bps: i64,
    pub target_apy_bps: i64,
    pub estimated_edge_bps: i64,
    pub estimated_cost_lamports: i64,
    pub execution_plan: Value,
}

impl PlannedRebalanceDecisionInput {
    #[allow(clippy::too_many_arguments)]
    pub fn same_mint(
        source_snapshot_id: SnapshotId,
        source_reserve: impl Into<String>,
        target_reserve: impl Into<String>,
        liquidity_mint: impl Into<String>,
        amount_raw: i64,
        source_apy_bps: i64,
        target_apy_bps: i64,
        estimated_edge_bps: i64,
    ) -> Self {
        let source_reserve = source_reserve.into();
        let target_reserve = target_reserve.into();
        let liquidity_mint = liquidity_mint.into();
        Self {
            source_snapshot_id,
            source_reserve: source_reserve.clone(),
            target_reserve: target_reserve.clone(),
            source_liquidity_mint: liquidity_mint.clone(),
            target_liquidity_mint: liquidity_mint.clone(),
            amount_raw,
            source_apy_bps,
            target_apy_bps,
            estimated_edge_bps,
            estimated_cost_lamports: 0,
            execution_plan: serde_json::json!({
                "kind": "same_mint",
                "source_reserve": source_reserve,
                "target_reserve": target_reserve,
                "liquidity_mint": liquidity_mint.clone(),
                "source_liquidity_mint": liquidity_mint.clone(),
                "target_liquidity_mint": liquidity_mint,
                "amount_raw": amount_raw,
                "route_amount_semantics": "redeemable_liquidity_amount",
                "source_amount_semantics": "redeemable_liquidity_amount",
                "redeemable_source_liquidity_amount_raw": amount_raw,
                "source_collateral_amount_raw": Value::Null,
                "idle_vault_liquidity_amount_raw": Value::Null,
            }),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct IdleVaultDepositDecisionInput {
    pub target_reserve: String,
    pub target_market: Option<String>,
    pub liquidity_mint: String,
    pub amount_raw: i64,
    pub idle_token_account: String,
    pub idle_observed_slot: i64,
    pub idle_observed_at: DateTime<Utc>,
    pub target_apy_bps: i64,
    pub estimated_edge_bps: i64,
    pub estimated_cost_lamports: i64,
    pub setup_obligation_before_deposit: bool,
    pub setup_obligation_policy: Option<String>,
    pub setup_obligation_policy_source: Option<String>,
    pub setup_obligation_vault_rent_top_up_lamports: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SameMintRebalanceInput {
    pub vault_id: Option<VaultId>,
    pub settings: Option<String>,
    pub vault_index: Option<i16>,
    pub source_reserve: String,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub amount_raw: i64,
    pub route_amount_semantics: String,
    pub source_amount_semantics: Option<String>,
    pub source_collateral_amount_raw: Option<i64>,
    pub redeemable_source_liquidity_amount_raw: Option<i64>,
    pub idle_vault_liquidity_amount_raw: Option<i64>,
    pub expected_source_snapshot_id: SnapshotId,
    pub source_apy_bps: i64,
    pub target_apy_bps: i64,
    pub estimated_edge_bps: i64,
    pub estimated_cost_lamports: i64,
    pub dry_run: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SameMintExecutionPreview {
    pub kind: String,
    pub source_reserve: String,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub amount_raw: i64,
    pub route_amount_semantics: String,
    pub source_amount_semantics: Option<String>,
    pub source_collateral_amount_raw: Option<i64>,
    pub redeemable_source_liquidity_amount_raw: Option<i64>,
    pub idle_vault_liquidity_amount_raw: Option<i64>,
    pub policy_executions: u8,
    pub route_steps: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SameMintRebalanceResult {
    pub vault_id: VaultId,
    pub decision_id: Option<DecisionId>,
    pub status: DecisionStatus,
    pub source_reserve: String,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub amount_raw: i64,
    pub signature: Option<String>,
    pub confirmed_slot: Option<i64>,
    pub skip_reason: Option<SkipReason>,
    pub error_reason: Option<String>,
    pub dry_run: bool,
    pub execution_preview: Option<SameMintExecutionPreview>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ConfirmSameMintRebalanceInput {
    pub decision_id: DecisionId,
    pub signature: String,
    pub submitted_slot: Option<i64>,
    pub confirmed_slot: i64,
    pub observed_at: Option<DateTime<Utc>>,
    pub post_snapshot_id: Option<SnapshotId>,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
pub enum SkipReason {
    ActiveDecision,
    NoValueSource,
    CrossMintOnly,
    NoSameMintEdge,
    UnsupportedAmountSemantics,
}

impl SkipReason {
    pub const fn decision_reason(self) -> DecisionReason {
        match self {
            Self::ActiveDecision => DecisionReason::ActiveDecision,
            Self::NoValueSource => DecisionReason::NoValueSource,
            Self::CrossMintOnly => DecisionReason::CrossMintOnly,
            Self::NoSameMintEdge => DecisionReason::NoSameMintEdge,
            Self::UnsupportedAmountSemantics => DecisionReason::UnsupportedAmountSemantics,
        }
    }
}

#[derive(Debug, Clone)]
pub struct PlanOutcome {
    pub vault_id: VaultId,
    pub decision_id: Option<DecisionId>,
    pub status: PlanOutcomeStatus,
}

#[allow(clippy::large_enum_variant)]
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PlanOutcomeStatus {
    Planned(RebalanceDecision),
    Skipped { reason: SkipReason },
}

impl PlanOutcome {
    pub fn planned(vault_id: VaultId, decision: RebalanceDecision) -> Self {
        Self {
            vault_id,
            decision_id: Some(decision.id),
            status: PlanOutcomeStatus::Planned(decision),
        }
    }

    pub fn skipped(
        vault_id: VaultId,
        reason: SkipReason,
        decision: Option<RebalanceDecision>,
    ) -> Self {
        Self {
            vault_id,
            decision_id: decision.as_ref().map(|decision| decision.id),
            status: PlanOutcomeStatus::Skipped { reason },
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PlannerConfig {
    pub min_edge_bps: i64,
    pub estimated_cost_lamports: i64,
}

impl Default for PlannerConfig {
    fn default() -> Self {
        Self {
            min_edge_bps: 1,
            estimated_cost_lamports: 0,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DecisionAdvance {
    StartSimulation,
    SimulationReady,
    Submit {
        signature: String,
        slot: Option<i64>,
    },
    StartConfirmation,
    Confirm {
        slot: Option<i64>,
        post_snapshot_id: Option<SnapshotId>,
    },
    Fail {
        reason: String,
    },
    Abandon {
        reason: String,
    },
}

#[derive(Debug)]
pub struct DecisionTransition {
    pub status: DecisionStatus,
    pub idempotent: bool,
    pub signature: Option<String>,
    pub submitted_slot: Option<i64>,
    pub confirmed_slot: Option<i64>,
    pub preflight_chain_slot: Option<i64>,
    pub post_snapshot_id: Option<SnapshotId>,
    pub abandon_reason: Option<String>,
    pub reason: Option<String>,
    pub payload: Value,
}

impl DecisionTransition {
    pub fn simple(status: DecisionStatus) -> Self {
        Self {
            status,
            idempotent: false,
            signature: None,
            submitted_slot: None,
            confirmed_slot: None,
            preflight_chain_slot: None,
            post_snapshot_id: None,
            abandon_reason: None,
            reason: None,
            payload: Value::Null,
        }
    }

    pub fn idempotent(status: DecisionStatus) -> Self {
        Self {
            idempotent: true,
            ..Self::simple(status)
        }
    }
}

pub const ACTIVE_DECISION_STATUSES: [&str; 5] = [
    DecisionStatus::Planned.as_str(),
    DecisionStatus::Simulating.as_str(),
    DecisionStatus::Ready.as_str(),
    DecisionStatus::Submitted.as_str(),
    DecisionStatus::Confirming.as_str(),
];
