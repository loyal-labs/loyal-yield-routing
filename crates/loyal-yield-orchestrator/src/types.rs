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
    pub amount_raw: u64,
    pub owner: Option<String>,
    pub mint: String,
    pub observed_slot: u64,
    pub observed_at: Option<DateTime<Utc>>,
    pub source: String,
    pub source_commitment: String,
    pub account_data_hash: Option<String>,
    pub raw_evidence: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct WalletAtaBalanceCurrent {
    pub target_id: BalanceSweepTargetId,
    pub wallet: String,
    pub wallet_usdc_ata: String,
    pub amount_raw: i64,
    pub owner: Option<String>,
    pub mint: String,
    pub observed_slot: i64,
    pub observed_at: DateTime<Utc>,
    pub source: String,
    pub source_commitment: String,
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
pub struct BalanceSweepExecutionInput {
    pub target_id: BalanceSweepTargetId,
    pub signature: String,
    pub slot: u64,
    pub source_wallet_ata: String,
    pub destination_vault_ata: String,
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
    ActiveDecision,
    NoValueSource,
    CrossMintOnly,
    NoSameMintEdge,
}

impl DecisionReason {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::TargetSupplyApyExceedsSource => "target_supply_apy_exceeds_source",
            Self::ActiveDecision => "active_decision",
            Self::NoValueSource => "no_value_source",
            Self::CrossMintOnly => "cross_mint_only",
            Self::NoSameMintEdge => "no_same_mint_edge",
        }
    }

    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "target_supply_apy_exceeds_source" => Some(Self::TargetSupplyApyExceedsSource),
            "active_decision" => Some(Self::ActiveDecision),
            "no_value_source" => Some(Self::NoValueSource),
            "cross_mint_only" => Some(Self::CrossMintOnly),
            "no_same_mint_edge" => Some(Self::NoSameMintEdge),
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
        let liquidity_mint = liquidity_mint.into();
        Self {
            source_snapshot_id,
            source_reserve: source_reserve.into(),
            target_reserve: target_reserve.into(),
            source_liquidity_mint: liquidity_mint.clone(),
            target_liquidity_mint: liquidity_mint,
            amount_raw,
            source_apy_bps,
            target_apy_bps,
            estimated_edge_bps,
            estimated_cost_lamports: 0,
            execution_plan: Value::Object(Default::default()),
        }
    }
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
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
pub enum SkipReason {
    ActiveDecision,
    NoValueSource,
    CrossMintOnly,
    NoSameMintEdge,
}

impl SkipReason {
    pub const fn decision_reason(self) -> DecisionReason {
        match self {
            Self::ActiveDecision => DecisionReason::ActiveDecision,
            Self::NoValueSource => DecisionReason::NoValueSource,
            Self::CrossMintOnly => DecisionReason::CrossMintOnly,
            Self::NoSameMintEdge => DecisionReason::NoSameMintEdge,
        }
    }
}

#[derive(Debug, Clone)]
pub struct PlanOutcome {
    pub vault_id: VaultId,
    pub decision_id: Option<DecisionId>,
    pub status: PlanOutcomeStatus,
}

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
