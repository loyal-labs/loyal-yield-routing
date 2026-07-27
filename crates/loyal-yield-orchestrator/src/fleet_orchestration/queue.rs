use super::observation::MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS;
use crate::{
    DecisionId, NeonSqlClient, OrchestratorError, SnapshotId, VaultId, ACTIVE_DECISION_STATUSES,
};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use sqlx::Row;
use std::collections::{BTreeMap, BTreeSet};

pub const REBALANCE_OPPORTUNITY_WAKEUP_CHANNEL: &str = "loyal_yield_rebalance_wakeup";

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OptimizerEpochInput {
    pub cluster: String,
    pub epoch_key: String,
    pub market_slot: i64,
    pub observed_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
    pub market_state: Value,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct OptimizerEpochRecord {
    pub id: i64,
    pub cluster: String,
    pub epoch_key: String,
    pub market_slot: i64,
    pub observed_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
    pub market_state: Value,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FleetPlanningStateInput {
    pub cluster: String,
    pub full_sweep_started_at: DateTime<Utc>,
    pub full_sweep_completed_at: DateTime<Utc>,
    pub optimizer_epoch_key: String,
    pub optimizer_epoch_expires_at: DateTime<Utc>,
    pub complete_frontier: bool,
    pub observed_vault_count: i64,
    pub opportunity_count: i64,
    pub selected_count: i64,
    pub deferred_count: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct FleetPlanningStateRecord {
    pub cluster: String,
    pub full_sweep_started_at: DateTime<Utc>,
    pub full_sweep_completed_at: DateTime<Utc>,
    pub optimizer_epoch_key: String,
    pub optimizer_epoch_expires_at: DateTime<Utc>,
    pub complete_frontier: bool,
    pub observed_vault_count: i64,
    pub opportunity_count: i64,
    pub selected_count: i64,
    pub deferred_count: i64,
    pub generation: i64,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct FleetPlanningDirtyVaultRecord {
    pub cluster: String,
    pub vault_id: VaultId,
    pub reasons: Vec<String>,
    pub maximum_observed_slot: Option<i64>,
    pub first_dirty_at: DateTime<Utc>,
    pub last_dirty_at: DateTime<Utc>,
    pub available_at: DateTime<Utc>,
    pub lease_owner: Option<String>,
    pub lease_expires_at: Option<DateTime<Utc>>,
    pub fencing_token: i64,
    pub generation: i64,
    pub attempt_count: i32,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct FleetPlanningDirtyVaultLease {
    pub dirty: FleetPlanningDirtyVaultRecord,
    pub owner: String,
    pub fencing_token: i64,
    pub generation: i64,
    pub expires_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum RebalanceOpportunityState {
    WaitingAlt,
    Revalidate,
    Ready,
    Leased,
    DecisionCreated,
    Completed,
    Stale,
    Superseded,
    Failed,
    Cancelled,
}

impl RebalanceOpportunityState {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::WaitingAlt => "waiting_alt",
            Self::Revalidate => "revalidate",
            Self::Ready => "ready",
            Self::Leased => "leased",
            Self::DecisionCreated => "decision_created",
            Self::Completed => "completed",
            Self::Stale => "stale",
            Self::Superseded => "superseded",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
        }
    }

    fn parse(value: &str) -> Result<Self, OrchestratorError> {
        match value {
            "waiting_alt" => Ok(Self::WaitingAlt),
            "revalidate" => Ok(Self::Revalidate),
            "ready" => Ok(Self::Ready),
            "leased" => Ok(Self::Leased),
            "decision_created" => Ok(Self::DecisionCreated),
            "completed" => Ok(Self::Completed),
            "stale" => Ok(Self::Stale),
            "superseded" => Ok(Self::Superseded),
            "failed" => Ok(Self::Failed),
            "cancelled" => Ok(Self::Cancelled),
            other => Err(OrchestratorError::StoreInvariant(format!(
                "unknown rebalance opportunity state {other:?}"
            ))),
        }
    }

    pub const fn is_terminal(self) -> bool {
        matches!(
            self,
            Self::DecisionCreated
                | Self::Completed
                | Self::Stale
                | Self::Superseded
                | Self::Failed
                | Self::Cancelled
        )
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum RebalanceOpportunityClaimKind {
    Execute,
    Revalidate,
}

impl RebalanceOpportunityClaimKind {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Execute => "execute",
            Self::Revalidate => "revalidate",
        }
    }

    fn parse(value: &str) -> Result<Self, OrchestratorError> {
        match value {
            "execute" => Ok(Self::Execute),
            "revalidate" => Ok(Self::Revalidate),
            other => Err(OrchestratorError::StoreInvariant(format!(
                "unknown rebalance opportunity lease kind {other:?}"
            ))),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RebalanceOpportunityInput {
    pub cluster: String,
    pub vault_id: VaultId,
    pub source_snapshot_id: Option<SnapshotId>,
    pub optimizer_epoch_id: i64,
    pub route_fingerprint: Option<String>,
    pub requirements_fingerprint: Option<String>,
    pub source_reserve: Option<String>,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub amount_raw: i64,
    pub principal_usd_micros: i64,
    pub source_apy_bps: i64,
    pub target_apy_bps: i64,
    pub estimated_edge_bps: i64,
    pub estimated_cost_lamports: i64,
    pub annual_yield_gain_usd_micros: i64,
    pub expected_net_gain_usd_micros: i64,
    pub economic_priority: i64,
    pub priority_version: String,
    pub execution_plan: Value,
    pub available_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
    pub provisioning_request_id: Option<i64>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RebalanceOpportunityRecord {
    pub id: i64,
    pub cluster: String,
    pub idempotency_key: String,
    /// Stable identity of the exact economic/source observation. Multiple
    /// immutable attempts may share it only after terminal no-effect proof.
    pub rediscovery_key: String,
    pub attempt_generation: i64,
    pub vault_id: VaultId,
    pub source_snapshot_id: Option<SnapshotId>,
    pub optimizer_epoch_id: i64,
    pub route_fingerprint: Option<String>,
    pub requirements_fingerprint: Option<String>,
    pub source_reserve: Option<String>,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub amount_raw: i64,
    pub principal_usd_micros: i64,
    pub source_apy_bps: i64,
    pub target_apy_bps: i64,
    pub estimated_edge_bps: i64,
    pub estimated_cost_lamports: i64,
    pub annual_yield_gain_usd_micros: i64,
    pub expected_net_gain_usd_micros: i64,
    pub economic_priority: i64,
    pub priority_version: String,
    pub state: RebalanceOpportunityState,
    pub execution_plan: Value,
    pub available_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
    pub lease_kind: Option<RebalanceOpportunityClaimKind>,
    pub lease_owner: Option<String>,
    pub lease_expires_at: Option<DateTime<Utc>>,
    pub fencing_token: i64,
    pub attempt_count: i32,
    pub decision_id: Option<DecisionId>,
    pub terminal_reason: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct RebalanceOpportunityLease {
    pub opportunity: RebalanceOpportunityRecord,
    pub claim_kind: RebalanceOpportunityClaimKind,
    pub owner: String,
    pub fencing_token: i64,
    pub expires_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct RebalanceOpportunityAdvance {
    pub next_state: RebalanceOpportunityState,
    pub available_at: Option<DateTime<Utc>>,
    pub decision_id: Option<DecisionId>,
    pub reason: Option<String>,
    /// Exact compiler output. Required when a revalidation lease publishes
    /// either executable coverage or durable ALT demand.
    pub route_fingerprint: Option<String>,
    pub requirements_fingerprint: Option<String>,
    pub execution_plan: Option<Value>,
    pub provisioning_request_id: Option<i64>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OrchestrationOutboxRecord {
    pub id: i64,
    pub cluster: String,
    pub event_kind: String,
    pub aggregate_kind: String,
    pub aggregate_id: i64,
    pub dedupe_key: String,
    pub payload: Value,
    pub available_at: DateTime<Utc>,
    pub lease_owner: Option<String>,
    pub lease_expires_at: Option<DateTime<Utc>>,
    pub fencing_token: i64,
    pub attempt_count: i32,
    pub processed_at: Option<DateTime<Utc>>,
    pub last_error: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct OrchestrationOutboxLease {
    pub event: OrchestrationOutboxRecord,
    pub owner: String,
    pub fencing_token: i64,
    pub expires_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SignedRouteSubmissionInput {
    pub cluster: String,
    pub semantic_key: String,
    pub opportunity_id: i64,
    pub decision_id: Option<DecisionId>,
    pub signed_transaction: Vec<u8>,
    pub signed_transaction_hash: String,
    pub message_hash: String,
    pub transaction_signature: String,
    pub recent_blockhash: String,
    pub last_valid_block_height: i64,
    pub source_snapshot_id: Option<SnapshotId>,
    pub optimizer_epoch_id: i64,
    pub alt_requirements_fingerprint: String,
    pub alt_selection_fingerprint: String,
    pub alt_mutation_epochs: Value,
    pub fee_payer: String,
    pub fee_payer_kind: RouteFeePayerKind,
    /// Required only for fee-only shards. The database rechecks this exact
    /// observation against the durable floor/ceiling while reserving spend in
    /// the same transaction as the signed route.
    pub fee_payer_balance_lamports: Option<i64>,
    pub fee_payer_balance_slot: Option<i64>,
    pub fee_payer_balance_observed_at: Option<DateTime<Utc>>,
    pub compiled_fee_lamports: i64,
    /// Complete writable-account evidence from the compiled transaction.
    pub writable_account_keys: Vec<String>,
    /// Semantic execution locks: one vault-specific key plus one bounded
    /// shared-write lane. This bounds DB admission only. The exact writable
    /// evidence above remains authoritative for physical Solana account-lock
    /// contention, including a shared fee payer or peak protocol reserve.
    pub conflict_account_keys: Vec<String>,
    pub executor_owner: String,
    pub executor_fencing_token: i64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RouteFeePayerKind {
    Policy,
    FeeOnlyShard,
}

impl RouteFeePayerKind {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Policy => "policy",
            Self::FeeOnlyShard => "fee_only_shard",
        }
    }

    fn parse(value: &str) -> Result<Self, OrchestratorError> {
        match value {
            "policy" => Ok(Self::Policy),
            "fee_only_shard" => Ok(Self::FeeOnlyShard),
            other => Err(OrchestratorError::StoreInvariant(format!(
                "unknown route fee-payer kind {other:?}"
            ))),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RouteFeePayerShardConfig {
    pub cluster: String,
    pub fee_payer: String,
    pub minimum_balance_lamports: i64,
    pub maximum_balance_lamports: i64,
    pub rolling_window_seconds: i32,
    pub maximum_window_spend_lamports: i64,
    pub maximum_transaction_fee_lamports: i64,
    pub current_window_reserved_lamports: i64,
    pub database_authority_separation_passes: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RouteFeePayerAuthorityStatus {
    pub reusable_family_count: i64,
    pub reusable_family_policy_mismatch_count: i64,
    pub reusable_table_count: i64,
    pub reusable_table_policy_mismatch_count: i64,
}

impl RouteFeePayerAuthorityStatus {
    pub const fn policy_authority_and_payer_match(&self) -> bool {
        self.reusable_family_count > 0
            && self.reusable_table_count > 0
            && self.reusable_family_policy_mismatch_count == 0
            && self.reusable_table_policy_mismatch_count == 0
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum SignedRouteSubmissionState {
    Signed,
    Submitted,
    Confirmed,
    ReconciliationPending,
    ExpiryCheckPending,
    EffectAmbiguous,
    Reconciled,
    Expired,
    Failed,
}

impl SignedRouteSubmissionState {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Signed => "signed",
            Self::Submitted => "submitted",
            Self::Confirmed => "confirmed",
            Self::ReconciliationPending => "reconciliation_pending",
            Self::ExpiryCheckPending => "expiry_check_pending",
            Self::EffectAmbiguous => "effect_ambiguous",
            Self::Reconciled => "reconciled",
            Self::Expired => "expired",
            Self::Failed => "failed",
        }
    }

    fn parse(value: &str) -> Result<Self, OrchestratorError> {
        match value {
            "signed" => Ok(Self::Signed),
            "submitted" => Ok(Self::Submitted),
            "confirmed" => Ok(Self::Confirmed),
            "reconciliation_pending" => Ok(Self::ReconciliationPending),
            "expiry_check_pending" => Ok(Self::ExpiryCheckPending),
            "effect_ambiguous" => Ok(Self::EffectAmbiguous),
            "reconciled" => Ok(Self::Reconciled),
            "expired" => Ok(Self::Expired),
            "failed" => Ok(Self::Failed),
            other => Err(OrchestratorError::StoreInvariant(format!(
                "unknown signed route submission state {other:?}"
            ))),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SignedRouteSubmissionRecord {
    pub id: i64,
    pub cluster: String,
    pub semantic_key: String,
    pub opportunity_id: i64,
    pub decision_id: Option<DecisionId>,
    pub signed_transaction: Vec<u8>,
    pub signed_transaction_hash: String,
    pub message_hash: String,
    pub transaction_signature: String,
    pub recent_blockhash: String,
    pub last_valid_block_height: i64,
    pub source_snapshot_id: Option<SnapshotId>,
    pub optimizer_epoch_id: i64,
    pub alt_requirements_fingerprint: String,
    pub alt_selection_fingerprint: String,
    pub alt_mutation_epochs: Value,
    pub fee_payer: String,
    pub fee_payer_kind: RouteFeePayerKind,
    pub compiled_fee_lamports: i64,
    pub writable_account_keys: Vec<String>,
    pub conflict_account_keys: Vec<String>,
    pub executor_owner: String,
    pub executor_fencing_token: i64,
    pub state: SignedRouteSubmissionState,
    pub confirmation_available_at: DateTime<Utc>,
    pub confirmation_lease_owner: Option<String>,
    pub confirmation_lease_expires_at: Option<DateTime<Utc>>,
    pub confirmation_fencing_token: i64,
    pub confirmation_attempt_count: i32,
    pub broadcast_count: i32,
    pub last_broadcast_at: Option<DateTime<Utc>>,
    pub last_status_checked_at: Option<DateTime<Utc>>,
    pub expiry_observed_block_height: Option<i64>,
    pub effect_check_slot: Option<i64>,
    pub submitted_slot: Option<i64>,
    pub submitted_at: Option<DateTime<Utc>>,
    pub confirmed_slot: Option<i64>,
    pub confirmed_at: Option<DateTime<Utc>>,
    pub reconciled_slot: Option<i64>,
    pub reconciled_at: Option<DateTime<Utc>>,
    pub error_detail: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct SignedRouteSubmissionLease {
    pub submission: SignedRouteSubmissionRecord,
    pub owner: String,
    pub fencing_token: i64,
    pub expires_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub enum SignedRouteSubmissionAdvance {
    BroadcastIntent {
        checked_at: DateTime<Utc>,
    },
    Submitted {
        checked_at: DateTime<Utc>,
        observed_slot: Option<i64>,
        next_poll_at: DateTime<Utc>,
        broadcasted: bool,
    },
    Deferred {
        checked_at: DateTime<Utc>,
        next_poll_at: DateTime<Utc>,
        error_detail: Option<String>,
    },
    Confirmed {
        checked_at: DateTime<Utc>,
        confirmed_slot: i64,
    },
    ReconciliationPending,
    ExpiryCheckPending {
        checked_at: DateTime<Utc>,
        observed_block_height: i64,
        effect_check_slot: i64,
    },
    EffectAmbiguous {
        checked_at: DateTime<Utc>,
        error_detail: String,
    },
    Reconciled {
        reconciled_slot: i64,
    },
    Expired {
        checked_at: DateTime<Utc>,
        observed_block_height: i64,
        signature_history_absent: bool,
        effect_absence_proved: bool,
    },
    Failed {
        checked_at: DateTime<Utc>,
        confirmed_slot: Option<i64>,
        error_detail: String,
    },
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RouteAccountConflictLease {
    pub cluster: String,
    pub writable_account_key: String,
    pub opportunity_id: i64,
    pub lease_owner: String,
    pub fencing_token: i64,
    pub expires_at: DateTime<Utc>,
    pub submission_id: Option<i64>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PhysicalWritableKeyCongestion {
    pub writable_account_key: String,
    pub classification: String,
    pub active_submission_count: i64,
    pub principal_usd_micros: i64,
    pub recoverable_yield_usd_micros_per_hour: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FleetOrchestrationStatus {
    pub cluster: String,
    pub opportunity_state: Option<String>,
    pub opportunity_count: i64,
    pub principal_usd_micros: i64,
    pub annual_yield_gain_usd_micros: i64,
    pub yield_gain_usd_micros_per_hour: i64,
    pub oldest_created_at: Option<DateTime<Utc>>,
    pub oldest_state_entered_at: Option<DateTime<Utc>>,
    pub oldest_age_seconds: Option<i64>,
    pub oldest_state_age_seconds: Option<i64>,
    pub expired_lease_count: i64,
    pub pending_outbox_count: i64,
    pub pending_submission_count: i64,
    pub pending_compiled_fee_lamports: i64,
    pub expiry_check_pending_count: i64,
    pub effect_ambiguous_count: i64,
    pub oldest_pending_submission_at: Option<DateTime<Utc>>,
    pub oldest_pending_submission_age_seconds: Option<i64>,
    pub sender_submission_count: i64,
    pub oldest_sender_state_entered_at: Option<DateTime<Utc>>,
    pub oldest_sender_state_age_seconds: Option<i64>,
    pub confirmer_submission_count: i64,
    pub oldest_confirmer_state_entered_at: Option<DateTime<Utc>>,
    pub oldest_confirmer_state_age_seconds: Option<i64>,
    pub reconciler_submission_count: i64,
    pub oldest_reconciler_state_entered_at: Option<DateTime<Utc>>,
    pub oldest_reconciler_state_age_seconds: Option<i64>,
    pub planner_registered_at: Option<DateTime<Utc>>,
    pub planner_last_seen_at: Option<DateTime<Utc>>,
    pub planner_last_seen_age_seconds: Option<i64>,
    pub full_sweep_started_at: Option<DateTime<Utc>>,
    pub full_sweep_completed_at: Option<DateTime<Utc>>,
    pub full_sweep_age_seconds: Option<i64>,
    pub planned_optimizer_epoch_key: Option<String>,
    pub planned_optimizer_epoch_expires_at: Option<DateTime<Utc>>,
    pub complete_frontier: Option<bool>,
    pub observed_vault_count: Option<i64>,
    pub planned_opportunity_count: Option<i64>,
    pub planned_selected_count: Option<i64>,
    pub planned_deferred_count: Option<i64>,
    pub planning_generation: Option<i64>,
    pub latest_market_epoch_id: Option<i64>,
    pub latest_market_epoch_key: Option<String>,
    pub latest_market_slot: Option<i64>,
    pub latest_market_observed_at: Option<DateTime<Utc>>,
    pub latest_market_expires_at: Option<DateTime<Utc>>,
    pub latest_market_epoch_age_seconds: Option<i64>,
    pub latest_market_epoch_expires_in_seconds: Option<i64>,
    pub latest_market_epoch_expired: Option<bool>,
    pub planner_epoch_matches_latest: Option<bool>,
    pub waiting_alt_opportunity_count: i64,
    pub waiting_alt_principal_usd_micros: i64,
    pub waiting_alt_yield_gain_usd_micros_per_hour: i64,
    pub oldest_waiting_alt_state_entered_at: Option<DateTime<Utc>>,
    pub oldest_waiting_alt_state_age_seconds: Option<i64>,
    pub ready_opportunity_count: i64,
    pub ready_principal_usd_micros: i64,
    pub ready_yield_gain_usd_micros_per_hour: i64,
    pub oldest_ready_state_entered_at: Option<DateTime<Utc>>,
    pub oldest_ready_state_age_seconds: Option<i64>,
    pub current_epoch_opportunity_count: i64,
    pub current_epoch_principal_usd_micros: i64,
    pub current_epoch_recoverable_yield_usd_micros_per_hour: i64,
    pub current_epoch_submitted_within_10s_yield_ppm: i64,
    pub current_epoch_submitted_within_2m_yield_ppm: i64,
    pub current_epoch_submitted_within_10m_yield_ppm: i64,
    pub current_epoch_confirmed_within_30s_yield_ppm: i64,
    pub current_epoch_submission_p95_milliseconds: Option<i64>,
    pub current_epoch_confirmation_p95_milliseconds: Option<i64>,
    pub current_epoch_compiled_fee_lamports: i64,
    /// Bounded, cluster-local evidence of the actual Solana writable accounts
    /// shared by nonterminal route submissions. The 64 semantic DB lanes are
    /// admission bounds only and must not be read as physical independence.
    pub active_physical_writable_key_count: i64,
    pub top_physical_writable_key_congestion: Vec<PhysicalWritableKeyCongestion>,
}

impl NeonSqlClient {
    pub async fn route_fee_payer_authority_status(
        &self,
        cluster: &str,
        policy_pubkey: &str,
    ) -> Result<RouteFeePayerAuthorityStatus, OrchestratorError> {
        if cluster.trim().is_empty() || policy_pubkey.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "fee-payer authority status requires cluster and policy public key".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            SELECT
                (SELECT COUNT(*)::BIGINT
                 FROM loyal_yield.lookup_table_families family
                 WHERE family.cluster = $1) AS reusable_family_count,
                (SELECT COUNT(*)::BIGINT
                 FROM loyal_yield.lookup_table_families family
                 WHERE family.cluster = $1
                   AND (
                       family.provisioning_authority <> $2
                       OR family.payer <> $2
                   )) AS reusable_family_policy_mismatch_count,
                (SELECT COUNT(*)::BIGINT
                 FROM loyal_yield.route_lookup_tables route_table
                 WHERE route_table.cluster = $1
                   AND route_table.family_id IS NOT NULL) AS reusable_table_count,
                (SELECT COUNT(*)::BIGINT
                 FROM loyal_yield.route_lookup_tables route_table
                 WHERE route_table.cluster = $1
                   AND route_table.family_id IS NOT NULL
                   AND (
                       route_table.authority <> $2
                       OR route_table.payer <> $2
                   )) AS reusable_table_policy_mismatch_count
            "#,
        )
        .bind(cluster)
        .bind(policy_pubkey)
        .fetch_one(self.pool())
        .await?;
        Ok(RouteFeePayerAuthorityStatus {
            reusable_family_count: row.try_get("reusable_family_count")?,
            reusable_family_policy_mismatch_count: row
                .try_get("reusable_family_policy_mismatch_count")?,
            reusable_table_count: row.try_get("reusable_table_count")?,
            reusable_table_policy_mismatch_count: row
                .try_get("reusable_table_policy_mismatch_count")?,
        })
    }

    /// Returns only explicitly enabled fee-only route payers. Secret material
    /// never enters the database; callers must intersect these public keys
    /// with their locally mounted keypair pool before selecting one.
    pub async fn enabled_route_fee_payer_shards(
        &self,
        cluster: &str,
    ) -> Result<Vec<RouteFeePayerShardConfig>, OrchestratorError> {
        if cluster.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "fee-payer shard lookup requires a cluster".to_owned(),
            ));
        }
        let rows = sqlx::query(
            r#"
            SELECT cluster, fee_payer, minimum_balance_lamports,
                   maximum_balance_lamports, rolling_window_seconds,
                   maximum_window_spend_lamports,
                   maximum_transaction_fee_lamports,
                   current_window_reserved_lamports,
                   database_authority_separation_passes
            FROM loyal_yield.route_fee_payer_shard_status
            WHERE cluster = $1
              AND enabled
            ORDER BY fee_payer
            "#,
        )
        .bind(cluster)
        .fetch_all(self.pool())
        .await?;
        rows.iter()
            .map(|row| {
                Ok(RouteFeePayerShardConfig {
                    cluster: row.try_get("cluster")?,
                    fee_payer: row.try_get("fee_payer")?,
                    minimum_balance_lamports: row.try_get("minimum_balance_lamports")?,
                    maximum_balance_lamports: row.try_get("maximum_balance_lamports")?,
                    rolling_window_seconds: row.try_get("rolling_window_seconds")?,
                    maximum_window_spend_lamports: row.try_get("maximum_window_spend_lamports")?,
                    maximum_transaction_fee_lamports: row
                        .try_get("maximum_transaction_fee_lamports")?,
                    current_window_reserved_lamports: row
                        .try_get("current_window_reserved_lamports")?,
                    database_authority_separation_passes: row
                        .try_get("database_authority_separation_passes")?,
                })
            })
            .collect()
    }

    pub async fn upsert_optimizer_epoch(
        &self,
        input: OptimizerEpochInput,
    ) -> Result<OptimizerEpochRecord, OrchestratorError> {
        if input.cluster.trim().is_empty()
            || input.epoch_key.trim().is_empty()
            || input.market_slot < 0
            || input.expires_at <= input.observed_at
            || !input.market_state.is_object()
        {
            return Err(OrchestratorError::StoreInvariant(
                "optimizer epoch requires immutable cluster, key, slot, lifetime, and object market evidence"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.optimizer_epochs
                (cluster, epoch_key, market_slot, observed_at, expires_at, market_state)
            VALUES ($1, $2, $3, $4, $5, $6)
            ON CONFLICT (cluster, epoch_key) DO NOTHING
            "#,
        )
        .bind(&input.cluster)
        .bind(&input.epoch_key)
        .bind(input.market_slot)
        .bind(input.observed_at)
        .bind(input.expires_at)
        .bind(&input.market_state)
        .execute(&mut *tx)
        .await?;
        let row = sqlx::query(
            "SELECT * FROM loyal_yield.optimizer_epochs WHERE cluster = $1 AND epoch_key = $2 FOR SHARE",
        )
        .bind(&input.cluster)
        .bind(&input.epoch_key)
        .fetch_one(&mut *tx)
        .await?;
        let epoch = optimizer_epoch_from_row(&row)?;
        if epoch.market_slot != input.market_slot
            || epoch.observed_at != input.observed_at
            || epoch.expires_at != input.expires_at
            || epoch.market_state != input.market_state
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "optimizer epoch key {:?} collided with different immutable evidence",
                input.epoch_key
            )));
        }
        tx.commit().await?;
        Ok(epoch)
    }

    pub async fn optimizer_epoch(
        &self,
        cluster: &str,
        optimizer_epoch_id: i64,
    ) -> Result<Option<OptimizerEpochRecord>, OrchestratorError> {
        if cluster.trim().is_empty() || optimizer_epoch_id <= 0 {
            return Err(OrchestratorError::StoreInvariant(
                "optimizer epoch lookup requires a cluster and positive id".to_owned(),
            ));
        }
        let row = sqlx::query(
            "SELECT * FROM loyal_yield.optimizer_epochs WHERE cluster = $1 AND id = $2",
        )
        .bind(cluster)
        .bind(optimizer_epoch_id)
        .fetch_optional(self.pool())
        .await?;
        row.as_ref().map(optimizer_epoch_from_row).transpose()
    }

    pub async fn fleet_planning_state(
        &self,
        cluster: &str,
    ) -> Result<Option<FleetPlanningStateRecord>, OrchestratorError> {
        if cluster.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "fleet planning state requires a cluster".to_owned(),
            ));
        }
        let row = sqlx::query("SELECT * FROM loyal_yield.fleet_planning_state WHERE cluster = $1")
            .bind(cluster)
            .fetch_optional(self.pool())
            .await?;
        row.as_ref().map(fleet_planning_state_from_row).transpose()
    }

    /// Registers an explicit planner cluster for fan-out from legacy source
    /// projections that do not themselves carry cluster identity. No trigger
    /// is allowed to infer a mainnet default.
    pub async fn register_fleet_planning_cluster(
        &self,
        cluster: &str,
    ) -> Result<(), OrchestratorError> {
        if cluster.trim().is_empty() || cluster.trim() != cluster {
            return Err(OrchestratorError::StoreInvariant(
                "fleet planning cluster registration requires a canonical cluster".to_owned(),
            ));
        }
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.fleet_planning_clusters (cluster)
            VALUES ($1)
            ON CONFLICT (cluster) DO UPDATE
            SET last_seen_at = now()
            "#,
        )
        .bind(cluster)
        .execute(self.pool())
        .await?;
        Ok(())
    }

    /// Refreshes the liveness timestamp for an already registered persistent
    /// planner without enqueueing work or changing the authoritative planning
    /// frontier. A missing registration is an invariant failure rather than an
    /// implicit cluster creation.
    pub async fn heartbeat_fleet_planning_cluster(
        &self,
        cluster: &str,
    ) -> Result<(), OrchestratorError> {
        if cluster.trim().is_empty() || cluster.trim() != cluster {
            return Err(OrchestratorError::StoreInvariant(
                "fleet planning heartbeat requires a canonical cluster".to_owned(),
            ));
        }
        let result = sqlx::query(
            r#"
            UPDATE loyal_yield.fleet_planning_clusters
            SET last_seen_at = clock_timestamp()
            WHERE cluster = $1
            "#,
        )
        .bind(cluster)
        .execute(self.pool())
        .await?;
        if result.rows_affected() != 1 {
            return Err(OrchestratorError::StoreInvariant(format!(
                "fleet planning heartbeat has no registered cluster {cluster}"
            )));
        }
        Ok(())
    }

    /// Publishes the mutable watermark for the last authoritative full-fleet
    /// pass. The referenced optimizer epoch remains immutable; this row only
    /// describes whether its global ranking frontier was complete enough to
    /// permit safe dirty-cohort replacement.
    pub async fn record_fleet_planning_full_sweep(
        &self,
        input: FleetPlanningStateInput,
    ) -> Result<FleetPlanningStateRecord, OrchestratorError> {
        if input.cluster.trim().is_empty()
            || input.optimizer_epoch_key.trim().is_empty()
            || input.full_sweep_completed_at < input.full_sweep_started_at
            || input.optimizer_epoch_expires_at <= input.full_sweep_started_at
            || input.observed_vault_count < 0
            || input.opportunity_count < 0
            || input.selected_count < 0
            || input.deferred_count < 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "full fleet planning watermark requires bounded immutable epoch and count evidence"
                    .to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.fleet_planning_state
                (cluster, full_sweep_started_at, full_sweep_completed_at,
                 optimizer_epoch_key, optimizer_epoch_expires_at,
                 complete_frontier, observed_vault_count, opportunity_count,
                 selected_count, deferred_count)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
            ON CONFLICT (cluster) DO UPDATE
            SET full_sweep_started_at = EXCLUDED.full_sweep_started_at,
                full_sweep_completed_at = EXCLUDED.full_sweep_completed_at,
                optimizer_epoch_key = EXCLUDED.optimizer_epoch_key,
                optimizer_epoch_expires_at = EXCLUDED.optimizer_epoch_expires_at,
                complete_frontier = EXCLUDED.complete_frontier,
                observed_vault_count = EXCLUDED.observed_vault_count,
                opportunity_count = EXCLUDED.opportunity_count,
                selected_count = EXCLUDED.selected_count,
                deferred_count = EXCLUDED.deferred_count,
                generation = loyal_yield.fleet_planning_state.generation + 1,
                updated_at = now()
            RETURNING *
            "#,
        )
        .bind(&input.cluster)
        .bind(input.full_sweep_started_at)
        .bind(input.full_sweep_completed_at)
        .bind(&input.optimizer_epoch_key)
        .bind(input.optimizer_epoch_expires_at)
        .bind(input.complete_frontier)
        .bind(input.observed_vault_count)
        .bind(input.opportunity_count)
        .bind(input.selected_count)
        .bind(input.deferred_count)
        .fetch_one(self.pool())
        .await?;
        fleet_planning_state_from_row(&row)
    }

    /// Claims a bounded dirty cohort with SKIP LOCKED. Producer writes bump
    /// `generation`, so acknowledgement cannot erase an event that arrived
    /// while this lease was processing older evidence.
    pub async fn lease_fleet_planning_dirty_vaults(
        &self,
        cluster: &str,
        owner: &str,
        lease_expires_at: DateTime<Utc>,
        limit: i64,
    ) -> Result<Vec<FleetPlanningDirtyVaultLease>, OrchestratorError> {
        if cluster.trim().is_empty()
            || owner.trim().is_empty()
            || lease_expires_at <= Utc::now()
            || !(1..=1_024).contains(&limit)
        {
            return Err(OrchestratorError::StoreInvariant(
                "dirty fleet planning lease requires cluster, owner, future expiry, and limit in 1..=1024"
                    .to_owned(),
            ));
        }
        let rows = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT dirty.cluster, dirty.vault_id
                FROM loyal_yield.fleet_planning_dirty_vaults dirty
                WHERE dirty.cluster = $1
                  AND dirty.available_at <= now()
                  AND (
                      dirty.lease_owner IS NULL
                      OR dirty.lease_expires_at <= now()
                  )
                ORDER BY dirty.available_at, dirty.last_dirty_at, dirty.vault_id
                FOR UPDATE OF dirty SKIP LOCKED
                LIMIT $4
            )
            UPDATE loyal_yield.fleet_planning_dirty_vaults dirty
            SET lease_owner = $2,
                lease_expires_at = $3,
                fencing_token = dirty.fencing_token + 1,
                attempt_count = dirty.attempt_count + 1,
                updated_at = now()
            FROM candidate
            WHERE dirty.cluster = candidate.cluster
              AND dirty.vault_id = candidate.vault_id
            RETURNING dirty.*
            "#,
        )
        .bind(cluster)
        .bind(owner)
        .bind(lease_expires_at)
        .bind(limit)
        .fetch_all(self.pool())
        .await?;
        rows.iter()
            .map(|row| {
                let dirty = fleet_planning_dirty_vault_from_row(row)?;
                Ok(FleetPlanningDirtyVaultLease {
                    fencing_token: dirty.fencing_token,
                    generation: dirty.generation,
                    expires_at: lease_expires_at,
                    owner: owner.to_owned(),
                    dirty,
                })
            })
            .collect()
    }

    /// Deletes an unchanged claimed hint, or merely releases it when a newer
    /// producer generation arrived during processing. Both outcomes are safe
    /// acknowledgements of the generation supplied by the caller.
    pub async fn acknowledge_fleet_planning_dirty_vaults(
        &self,
        leases: &[FleetPlanningDirtyVaultLease],
    ) -> Result<u64, OrchestratorError> {
        if leases.is_empty() {
            return Ok(0);
        }
        let cluster = &leases[0].dirty.cluster;
        let owner = &leases[0].owner;
        if leases.iter().any(|lease| {
            lease.dirty.cluster != *cluster
                || lease.owner != *owner
                || lease.fencing_token <= 0
                || lease.generation <= 0
        }) {
            return Err(OrchestratorError::StoreInvariant(
                "dirty fleet planning acknowledgement requires one cluster/owner and positive fences"
                    .to_owned(),
            ));
        }
        let vault_ids = leases
            .iter()
            .map(|lease| lease.dirty.vault_id.as_i64())
            .collect::<Vec<_>>();
        let fencing_tokens = leases
            .iter()
            .map(|lease| lease.fencing_token)
            .collect::<Vec<_>>();
        let generations = leases
            .iter()
            .map(|lease| lease.generation)
            .collect::<Vec<_>>();
        let acknowledged: i64 = sqlx::query_scalar(
            r#"
            WITH claimed AS (
                SELECT *
                FROM unnest($1::BIGINT[], $2::BIGINT[], $3::BIGINT[])
                    AS claim(vault_id, fencing_token, generation)
            ), deleted AS (
                DELETE FROM loyal_yield.fleet_planning_dirty_vaults dirty
                USING claimed
                WHERE dirty.cluster = $4
                  AND dirty.vault_id = claimed.vault_id
                  AND dirty.lease_owner = $5
                  AND dirty.fencing_token = claimed.fencing_token
                  AND dirty.generation = claimed.generation
                  AND dirty.lease_expires_at > now()
                RETURNING dirty.vault_id
            ), released_newer AS (
                UPDATE loyal_yield.fleet_planning_dirty_vaults dirty
                SET lease_owner = NULL,
                    lease_expires_at = NULL,
                    available_at = LEAST(dirty.available_at, now()),
                    updated_at = now()
                FROM claimed
                WHERE dirty.cluster = $4
                  AND dirty.vault_id = claimed.vault_id
                  AND dirty.lease_owner = $5
                  AND dirty.fencing_token = claimed.fencing_token
                  AND dirty.generation <> claimed.generation
                  AND dirty.lease_expires_at > now()
                RETURNING dirty.vault_id
            )
            SELECT (
                (SELECT count(*) FROM deleted)
                + (SELECT count(*) FROM released_newer)
            )::BIGINT
            "#,
        )
        .bind(vault_ids)
        .bind(fencing_tokens)
        .bind(generations)
        .bind(cluster)
        .bind(owner)
        .fetch_one(self.pool())
        .await?;
        Ok(u64::try_from(acknowledged).unwrap_or_default())
    }

    pub async fn retry_fleet_planning_dirty_vaults(
        &self,
        leases: &[FleetPlanningDirtyVaultLease],
        available_at: DateTime<Utc>,
    ) -> Result<u64, OrchestratorError> {
        if leases.is_empty() {
            return Ok(0);
        }
        let cluster = &leases[0].dirty.cluster;
        let owner = &leases[0].owner;
        if leases.iter().any(|lease| {
            lease.dirty.cluster != *cluster || lease.owner != *owner || lease.fencing_token <= 0
        }) {
            return Err(OrchestratorError::StoreInvariant(
                "dirty fleet planning retry requires one cluster/owner and positive fences"
                    .to_owned(),
            ));
        }
        let vault_ids = leases
            .iter()
            .map(|lease| lease.dirty.vault_id.as_i64())
            .collect::<Vec<_>>();
        let fencing_tokens = leases
            .iter()
            .map(|lease| lease.fencing_token)
            .collect::<Vec<_>>();
        let result = sqlx::query(
            r#"
            WITH claimed AS (
                SELECT *
                FROM unnest($1::BIGINT[], $2::BIGINT[])
                    AS claim(vault_id, fencing_token)
            )
            UPDATE loyal_yield.fleet_planning_dirty_vaults dirty
            SET lease_owner = NULL,
                lease_expires_at = NULL,
                available_at = GREATEST(dirty.available_at, $5),
                updated_at = now()
            FROM claimed
            WHERE dirty.cluster = $3
              AND dirty.vault_id = claimed.vault_id
              AND dirty.lease_owner = $4
              AND dirty.fencing_token = claimed.fencing_token
            "#,
        )
        .bind(vault_ids)
        .bind(fencing_tokens)
        .bind(cluster)
        .bind(owner)
        .bind(available_at)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected())
    }

    /// Clears only hints already covered when a full sweep began. Writes that
    /// race after the cutoff retain a greater `last_dirty_at` and survive.
    pub async fn clear_fleet_planning_dirty_observed_before(
        &self,
        cluster: &str,
        cutoff: DateTime<Utc>,
    ) -> Result<u64, OrchestratorError> {
        if cluster.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "dirty fleet planning recovery clear requires a cluster".to_owned(),
            ));
        }
        let result = sqlx::query(
            r#"
            DELETE FROM loyal_yield.fleet_planning_dirty_vaults
            WHERE cluster = $1
              AND last_dirty_at <= $2
              AND (lease_owner IS NULL OR lease_expires_at <= now())
            "#,
        )
        .bind(cluster)
        .bind(cutoff)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected())
    }

    /// Removes obsolete, unleased scheduling intents for dirty vaults that a
    /// successful scoped replan no longer selected. Decisions and persisted
    /// signed work stay exclusively owned by their recovery lanes.
    pub async fn retire_unselected_dirty_vault_opportunities(
        &self,
        cluster: &str,
        dirty_vault_ids: &[i64],
        selected_vault_ids: &[i64],
    ) -> Result<u64, OrchestratorError> {
        let dirty = dirty_vault_ids.iter().copied().collect::<BTreeSet<_>>();
        let selected = selected_vault_ids.iter().copied().collect::<BTreeSet<_>>();
        if cluster.trim().is_empty()
            || dirty.is_empty()
            || dirty.len() != dirty_vault_ids.len()
            || selected.len() != selected_vault_ids.len()
            || dirty.iter().any(|vault_id| *vault_id <= 0)
            || selected.iter().any(|vault_id| !dirty.contains(vault_id))
        {
            return Err(OrchestratorError::StoreInvariant(
                "dirty opportunity retirement requires one cluster and unique positive selected subset"
                    .to_owned(),
            ));
        }
        let retired: i64 = sqlx::query_scalar(
            r#"
            WITH retired AS (
                UPDATE loyal_yield.rebalance_opportunities opportunity
                SET opportunity_state = 'superseded',
                    lease_kind = NULL,
                    lease_owner = NULL,
                    lease_expires_at = NULL,
                    terminal_reason = 'dirty_vault_replanned_without_current_route',
                    updated_at = now()
                WHERE opportunity.cluster = $1
                  AND opportunity.vault_id = ANY($2::BIGINT[])
                  AND NOT (opportunity.vault_id = ANY($3::BIGINT[]))
                  AND opportunity.decision_id IS NULL
                  AND (
                      opportunity.opportunity_state IN (
                          'waiting_alt', 'revalidate', 'ready'
                      )
                      OR (
                          opportunity.opportunity_state = 'leased'
                          AND opportunity.lease_expires_at <= now()
                      )
                  )
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.signed_route_submissions submission
                      WHERE submission.opportunity_id = opportunity.id
                        AND submission.submission_state NOT IN (
                            'reconciled', 'expired', 'failed'
                        )
                  )
                RETURNING opportunity.id
            ), released_conflicts AS (
                DELETE FROM loyal_yield.route_account_conflict_leases conflict
                USING retired
                WHERE conflict.opportunity_id = retired.id
                  AND conflict.submission_id IS NULL
                RETURNING conflict.opportunity_id
            )
            SELECT count(*)::BIGINT FROM retired
            "#,
        )
        .bind(cluster)
        .bind(dirty_vault_ids)
        .bind(selected_vault_ids)
        .fetch_one(self.pool())
        .await?;
        Ok(u64::try_from(retired).unwrap_or_default())
    }

    /// Publishes one exact, immutable opportunity and supersedes any older
    /// scheduling intent for the same vault. If an ALT request is linked, its
    /// row is share-locked so satisfaction cannot race past consumer creation.
    pub async fn upsert_rebalance_opportunity(
        &self,
        input: RebalanceOpportunityInput,
    ) -> Result<RebalanceOpportunityRecord, OrchestratorError> {
        validate_opportunity_input(&input)?;
        let rediscovery_key = rebalance_opportunity_idempotency_key(&input);
        let mut tx = self.pool().begin().await?;

        let vault_active: Option<bool> = sqlx::query_scalar(
            "SELECT active FROM loyal_yield.managed_vaults WHERE id = $1 FOR UPDATE",
        )
        .bind(input.vault_id.as_i64())
        .fetch_optional(&mut *tx)
        .await?;
        if vault_active != Some(true) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "cannot queue rebalance opportunity for missing or inactive vault {}",
                input.vault_id
            )));
        }

        if let Some(source_snapshot_id) = input.source_snapshot_id {
            let snapshot_vault_id: Option<i64> = sqlx::query_scalar(
                "SELECT vault_id FROM loyal_yield.vault_position_snapshots WHERE id = $1 FOR SHARE",
            )
            .bind(source_snapshot_id.as_i64())
            .fetch_optional(&mut *tx)
            .await?;
            if snapshot_vault_id != Some(input.vault_id.as_i64()) {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "source snapshot {source_snapshot_id} does not belong to opportunity vault {}",
                    input.vault_id
                )));
            }
        }

        let minimum_publication_lifetime_seconds =
            i32::try_from(MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "minimum market-epoch publication lifetime does not fit PostgreSQL INTEGER"
                        .to_owned(),
                )
            })?;
        let epoch = sqlx::query(
            r#"
            SELECT cluster, expires_at,
                   expires_at >= clock_timestamp()
                       + make_interval(secs => $2::INTEGER)
                   AND $3::TIMESTAMPTZ >= clock_timestamp()
                       + make_interval(secs => $2::INTEGER)
                       AS publication_lifetime_ready
            FROM loyal_yield.optimizer_epochs
            WHERE id = $1
            FOR SHARE
            "#,
        )
        .bind(input.optimizer_epoch_id)
        .bind(minimum_publication_lifetime_seconds)
        .bind(input.expires_at)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "optimizer epoch {} does not exist",
                input.optimizer_epoch_id
            ))
        })?;
        if epoch.try_get::<String, _>("cluster")? != input.cluster
            || epoch.try_get::<DateTime<Utc>, _>("expires_at")? < input.expires_at
            || !epoch.try_get::<bool, _>("publication_lifetime_ready")?
        {
            return Err(OrchestratorError::StoreInvariant(
                "opportunity cluster/lifetime exceeds or has less than the minimum usable optimizer epoch evidence"
                    .to_owned(),
            ));
        }

        let initial_state = if let Some(request_id) = input.provisioning_request_id {
            let requirements_fingerprint =
                input.requirements_fingerprint.as_deref().ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "ALT-blocked opportunity requires exact requirements fingerprint"
                            .to_owned(),
                    )
                })?;
            if input.route_fingerprint.as_deref().is_none_or(str::is_empty) {
                return Err(OrchestratorError::StoreInvariant(
                    "ALT-blocked opportunity requires exact route fingerprint".to_owned(),
                ));
            }
            let request = sqlx::query(
                r#"
                SELECT cluster, vault_id, requirements_fingerprint, sealed_at, request_status
                FROM loyal_yield.lookup_table_provisioning_requests
                WHERE id = $1
                FOR SHARE
                "#,
            )
            .bind(request_id)
            .fetch_optional(&mut *tx)
            .await?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(format!(
                    "lookup-table provisioning request {request_id} does not exist"
                ))
            })?;
            if request.try_get::<String, _>("cluster")? != input.cluster
                || request.try_get::<i64, _>("vault_id")? != input.vault_id.as_i64()
                || request.try_get::<String, _>("requirements_fingerprint")?
                    != requirements_fingerprint
                || request
                    .try_get::<Option<DateTime<Utc>>, _>("sealed_at")?
                    .is_none()
            {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "lookup-table provisioning request {request_id} does not match the sealed opportunity demand"
                )));
            }
            if request.try_get::<String, _>("request_status")? == "satisfied" {
                RebalanceOpportunityState::Revalidate
            } else {
                RebalanceOpportunityState::WaitingAlt
            }
        } else {
            RebalanceOpportunityState::Revalidate
        };

        // The managed-vault row above is the per-vault publication mutex. It
        // serializes concurrent planner passes before they inspect the latest
        // immutable generation, so at most one pass can create the next retry.
        let latest_attempt = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.rebalance_opportunities
            WHERE rediscovery_key = $1
            ORDER BY attempt_generation DESC, id DESC
            LIMIT 1
            FOR UPDATE
            "#,
        )
        .bind(&rediscovery_key)
        .fetch_optional(&mut *tx)
        .await?;
        let (attempt_generation, idempotency_key) = if let Some(row) = latest_attempt {
            let latest = rebalance_opportunity_from_row(&row)?;
            if latest.rediscovery_key != rediscovery_key
                || !rebalance_opportunity_matches_input(&latest, &input)
            {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "rebalance opportunity rediscovery key {rediscovery_key:?} collided with different immutable evidence"
                )));
            }

            let terminal_no_effect_proved: bool = sqlx::query_scalar(
                r#"
                SELECT opportunity.opportunity_state = 'failed'
                   AND NOT EXISTS (
                       SELECT 1
                       FROM loyal_yield.target_capacity_reservations reservation
                       WHERE reservation.opportunity_id = opportunity.id
                         AND reservation.reservation_state <> 'released'
                   )
                   AND NOT EXISTS (
                       SELECT 1
                       FROM loyal_yield.route_account_conflict_leases conflict
                       WHERE conflict.opportunity_id = opportunity.id
                   )
                   AND (
                       (
                           opportunity.decision_id IS NULL
                           AND NOT EXISTS (
                               SELECT 1
                               FROM loyal_yield.signed_route_submissions submission
                               WHERE submission.opportunity_id = opportunity.id
                           )
                       )
                       OR (
                           opportunity.decision_id IS NOT NULL
                           AND (
                               SELECT count(*)
                               FROM loyal_yield.signed_route_submissions submission
                               WHERE submission.opportunity_id = opportunity.id
                           ) = 1
                           AND EXISTS (
                               SELECT 1
                               FROM loyal_yield.signed_route_submissions submission
                               WHERE submission.opportunity_id = opportunity.id
                                 AND submission.decision_id = opportunity.decision_id
                                 AND (
                                     submission.submission_state = 'expired'
                                     OR (
                                         submission.submission_state = 'failed'
                                         AND (
                                             submission.confirmed_slot IS NOT NULL
                                             OR submission.broadcast_count = 0
                                         )
                                     )
                                 )
                           )
                       )
                   )
                FROM loyal_yield.rebalance_opportunities opportunity
                WHERE opportunity.id = $1
                "#,
            )
            .bind(latest.id)
            .fetch_one(&mut *tx)
            .await?;
            if !terminal_no_effect_proved {
                tx.commit().await?;
                return Ok(latest);
            }
            let generation = latest.attempt_generation.checked_add(1).ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "rebalance opportunity attempt generation overflowed".to_owned(),
                )
            })?;
            let key = rebalance_opportunity_attempt_idempotency_key(&rediscovery_key, generation)?;
            (generation, key)
        } else {
            (
                1,
                rebalance_opportunity_attempt_idempotency_key(&rediscovery_key, 1)?,
            )
        };

        let unexpired_competing_lease: Option<i64> = sqlx::query_scalar(
            r#"
            SELECT id
            FROM loyal_yield.rebalance_opportunities
            WHERE cluster = $1 AND vault_id = $2
              AND rediscovery_key <> $3
              AND opportunity_state = 'leased'
              AND lease_expires_at > now()
            FOR UPDATE
            "#,
        )
        .bind(&input.cluster)
        .bind(input.vault_id.as_i64())
        .bind(&rediscovery_key)
        .fetch_optional(&mut *tx)
        .await?;
        if let Some(leased_id) = unexpired_competing_lease {
            return Err(OrchestratorError::OpportunityDeferredBehindLease {
                vault_id: input.vault_id,
                leased_id,
            });
        }

        sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_opportunities
            SET opportunity_state = 'superseded',
                lease_kind = NULL,
                lease_owner = NULL,
                lease_expires_at = NULL,
                terminal_reason = 'newer_opportunity_published',
                updated_at = now()
            WHERE cluster = $1 AND vault_id = $2
              AND rediscovery_key <> $3
              AND opportunity_state IN ('waiting_alt', 'revalidate', 'ready', 'leased')
              AND (opportunity_state <> 'leased' OR lease_expires_at <= now())
            "#,
        )
        .bind(&input.cluster)
        .bind(input.vault_id.as_i64())
        .bind(&rediscovery_key)
        .execute(&mut *tx)
        .await?;

        let row = match sqlx::query(
            r#"
            INSERT INTO loyal_yield.rebalance_opportunities
                (cluster, idempotency_key, rediscovery_key, attempt_generation,
                 vault_id, source_snapshot_id, optimizer_epoch_id, route_fingerprint,
                 requirements_fingerprint, source_reserve, target_reserve,
                 liquidity_mint, amount_raw, principal_usd_micros,
                 source_apy_bps, target_apy_bps, estimated_edge_bps,
                 estimated_cost_lamports, annual_yield_gain_usd_micros,
                 expected_net_gain_usd_micros, economic_priority,
                 priority_version, opportunity_state, execution_plan,
                 available_at, expires_at)
            SELECT
                $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
                $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
                $21, $22, $23, $24, $25, $26
            FROM loyal_yield.optimizer_epochs epoch
            WHERE epoch.id = $7
              AND epoch.cluster = $1
              AND epoch.expires_at >= clock_timestamp()
                  + make_interval(secs => $27::INTEGER)
              AND $26::TIMESTAMPTZ >= clock_timestamp()
                  + make_interval(secs => $27::INTEGER)
            ON CONFLICT DO NOTHING
            RETURNING *
            "#,
        )
        .bind(&input.cluster)
        .bind(&idempotency_key)
        .bind(&rediscovery_key)
        .bind(attempt_generation)
        .bind(input.vault_id.as_i64())
        .bind(input.source_snapshot_id.map(SnapshotId::as_i64))
        .bind(input.optimizer_epoch_id)
        .bind(&input.route_fingerprint)
        .bind(&input.requirements_fingerprint)
        .bind(&input.source_reserve)
        .bind(&input.target_reserve)
        .bind(&input.liquidity_mint)
        .bind(input.amount_raw)
        .bind(input.principal_usd_micros)
        .bind(input.source_apy_bps)
        .bind(input.target_apy_bps)
        .bind(input.estimated_edge_bps)
        .bind(input.estimated_cost_lamports)
        .bind(input.annual_yield_gain_usd_micros)
        .bind(input.expected_net_gain_usd_micros)
        .bind(input.economic_priority)
        .bind(&input.priority_version)
        .bind(initial_state.as_str())
        .bind(&input.execution_plan)
        .bind(input.available_at)
        .bind(input.expires_at)
        .bind(minimum_publication_lifetime_seconds)
        .fetch_optional(&mut *tx)
        .await
        {
            Ok(row) => row,
            Err(error) if is_active_opportunity_slot_conflict(&error) => {
                // PostgreSQL leaves the transaction aborted after a uniqueness
                // violation. Discard it before collecting best-effort evidence
                // through a fresh pool connection.
                let _ = tx.rollback().await;
                let slot_owner = sqlx::query_as::<_, (i64, Option<String>)>(
                    r#"
                    SELECT slot.opportunity_id, opportunity.opportunity_state
                    FROM loyal_yield.active_rebalance_opportunity_slots slot
                    LEFT JOIN loyal_yield.rebalance_opportunities opportunity
                      ON opportunity.id = slot.opportunity_id
                    WHERE slot.vault_id = $1 AND slot.cluster = $2
                    "#,
                )
                .bind(input.vault_id.as_i64())
                .bind(&input.cluster)
                .fetch_optional(self.pool())
                .await
                .ok()
                .flatten();
                let (slot_opportunity_id, slot_opportunity_state, reason) = match slot_owner {
                    Some((opportunity_id, opportunity_state)) => (
                        Some(opportunity_id),
                        opportunity_state,
                        "active_slot_owner_valid",
                    ),
                    None => (None, None, "active_slot_owner_unresolved"),
                };
                return Err(OrchestratorError::OpportunityDeferredBehindActiveSlot {
                    vault_id: input.vault_id,
                    slot_opportunity_id,
                    slot_opportunity_state,
                    reason,
                });
            }
            Err(error) => return Err(error.into()),
        };
        let row = match row {
            Some(row) => row,
            None => {
                let publication_lifetime_ready: bool = sqlx::query_scalar(
                    r#"
                    SELECT EXISTS (
                        SELECT 1
                        FROM loyal_yield.optimizer_epochs epoch
                        WHERE epoch.id = $1
                          AND epoch.cluster = $2
                          AND epoch.expires_at >= clock_timestamp()
                              + make_interval(secs => $3::INTEGER)
                          AND $4::TIMESTAMPTZ >= clock_timestamp()
                              + make_interval(secs => $3::INTEGER)
                    )
                    "#,
                )
                .bind(input.optimizer_epoch_id)
                .bind(&input.cluster)
                .bind(minimum_publication_lifetime_seconds)
                .bind(input.expires_at)
                .fetch_one(&mut *tx)
                .await?;
                if !publication_lifetime_ready {
                    return Err(OrchestratorError::StoreInvariant(
                        "rebalance opportunity publication lost its minimum usable market-epoch lifetime before insertion"
                            .to_owned(),
                    ));
                }
                sqlx::query(
                    r#"
                SELECT *
                FROM loyal_yield.rebalance_opportunities
                WHERE rediscovery_key = $1 AND attempt_generation = $2
                FOR SHARE
                "#,
                )
                .bind(&rediscovery_key)
                .bind(attempt_generation)
                .fetch_one(&mut *tx)
                .await?
            }
        };
        let opportunity = rebalance_opportunity_from_row(&row)?;
        if opportunity.rediscovery_key != rediscovery_key
            || opportunity.attempt_generation != attempt_generation
            || !rebalance_opportunity_matches_input(&opportunity, &input)
        {
            return Err(OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity attempt key {idempotency_key:?} collided with different immutable evidence"
            )));
        }

        if let Some(request_id) = input.provisioning_request_id {
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_provisioning_request_consumers
                    (opportunity_id, provisioning_request_id)
                VALUES ($1, $2)
                ON CONFLICT (opportunity_id) DO UPDATE
                SET provisioning_request_id = EXCLUDED.provisioning_request_id
                "#,
            )
            .bind(opportunity.id)
            .bind(request_id)
            .execute(&mut *tx)
            .await?;
        }

        // Consumer linkage above can itself wait on a locked provisioning
        // row. Recheck with the database wall clock immediately before commit
        // so no stalled publication becomes visible after spending the
        // lifetime margin that made its market evidence executable.
        let publication_lifetime_ready: bool = sqlx::query_scalar(
            r#"
            SELECT opportunity.expires_at >= clock_timestamp()
                       + make_interval(secs => $2::INTEGER)
               AND epoch.expires_at >= clock_timestamp()
                       + make_interval(secs => $2::INTEGER)
            FROM loyal_yield.rebalance_opportunities opportunity
            JOIN loyal_yield.optimizer_epochs epoch
              ON epoch.id = opportunity.optimizer_epoch_id
            WHERE opportunity.id = $1
            "#,
        )
        .bind(opportunity.id)
        .bind(minimum_publication_lifetime_seconds)
        .fetch_one(&mut *tx)
        .await?;
        if !publication_lifetime_ready {
            return Err(OrchestratorError::StoreInvariant(
                "rebalance opportunity publication lost its minimum usable market-epoch lifetime before commit"
                    .to_owned(),
            ));
        }

        tx.commit().await?;
        Ok(opportunity)
    }

    /// Re-admits an ALT-cold opportunity only after the current planner wave
    /// selected the exact optimizer epoch again and its sealed ALT request is
    /// satisfied. ALT completion alone never makes stale economics executable.
    pub async fn re_admit_waiting_alt_opportunity(
        &self,
        opportunity_id: i64,
        optimizer_epoch_id: i64,
    ) -> Result<RebalanceOpportunityRecord, OrchestratorError> {
        if opportunity_id <= 0 || optimizer_epoch_id <= 0 {
            return Err(OrchestratorError::StoreInvariant(
                "ALT-cold re-admission requires positive opportunity and optimizer epoch ids"
                    .to_owned(),
            ));
        }

        let mut tx = self.pool().begin().await?;
        let row = sqlx::query(
            "SELECT * FROM loyal_yield.rebalance_opportunities WHERE id = $1 FOR UPDATE",
        )
        .bind(opportunity_id)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity {opportunity_id} does not exist"
            ))
        })?;
        let current = rebalance_opportunity_from_row(&row)?;
        if current.optimizer_epoch_id != optimizer_epoch_id {
            return Err(OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity {opportunity_id} belongs to optimizer epoch {}, not current epoch {optimizer_epoch_id}",
                current.optimizer_epoch_id
            )));
        }
        if current.state != RebalanceOpportunityState::WaitingAlt {
            tx.commit().await?;
            return Ok(current);
        }

        let alt_satisfied: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.lookup_table_provisioning_request_consumers consumer
                JOIN loyal_yield.lookup_table_provisioning_requests request
                  ON request.id = consumer.provisioning_request_id
                WHERE consumer.opportunity_id = $1
                  AND request.cluster = $2
                  AND request.request_status = 'satisfied'
                  AND request.requirements_fingerprint = $3
            )
            "#,
        )
        .bind(opportunity_id)
        .bind(&current.cluster)
        .bind(
            current
                .requirements_fingerprint
                .as_deref()
                .unwrap_or_default(),
        )
        .fetch_one(&mut *tx)
        .await?;
        if !alt_satisfied || current.expires_at <= Utc::now() {
            tx.commit().await?;
            return Ok(current);
        }

        let minimum_publication_lifetime_seconds =
            i32::try_from(MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "minimum market-epoch publication lifetime does not fit PostgreSQL INTEGER"
                        .to_owned(),
                )
            })?;
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_opportunities
            SET opportunity_state = 'revalidate',
                available_at = now(),
                lease_kind = NULL,
                lease_owner = NULL,
                lease_expires_at = NULL,
                terminal_reason = NULL,
                updated_at = now()
            WHERE id = $1
              AND opportunity_state = 'waiting_alt'
              AND optimizer_epoch_id = $2
              AND expires_at >= clock_timestamp()
                  + make_interval(secs => $3::INTEGER)
            RETURNING *
            "#,
        )
        .bind(opportunity_id)
        .bind(optimizer_epoch_id)
        .bind(minimum_publication_lifetime_seconds)
        .fetch_optional(&mut *tx)
        .await?;
        let Some(row) = row else {
            tx.commit().await?;
            return Ok(current);
        };
        let readmitted = rebalance_opportunity_from_row(&row)?;
        tx.commit().await?;
        Ok(readmitted)
    }

    pub async fn rebalance_opportunity(
        &self,
        opportunity_id: i64,
    ) -> Result<Option<RebalanceOpportunityRecord>, OrchestratorError> {
        let row = sqlx::query("SELECT * FROM loyal_yield.rebalance_opportunities WHERE id = $1")
            .bind(opportunity_id)
            .fetch_optional(self.pool())
            .await?;
        row.as_ref().map(rebalance_opportunity_from_row).transpose()
    }

    /// Cheap pre-sign/pre-send fence check. Call it immediately before every
    /// irreversible executor step; a previously returned lease is not proof of
    /// current ownership.
    pub async fn validate_rebalance_opportunity_lease(
        &self,
        lease: &RebalanceOpportunityLease,
    ) -> Result<RebalanceOpportunityRecord, OrchestratorError> {
        let row = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.rebalance_opportunities
            WHERE id = $1
              AND opportunity_state = 'leased'
              AND lease_kind = $2
              AND lease_owner = $3
              AND fencing_token = $4
              AND lease_expires_at > now()
            "#,
        )
        .bind(lease.opportunity.id)
        .bind(lease.claim_kind.as_str())
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity {} lease is stale, expired, or fenced",
                lease.opportunity.id
            ))
        })?;
        rebalance_opportunity_from_row(&row)
    }

    pub async fn fleet_orchestration_status(
        &self,
        cluster: &str,
    ) -> Result<Vec<FleetOrchestrationStatus>, OrchestratorError> {
        const HOT_WRITABLE_KEY_LIMIT: i64 = 16;
        let rows = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.fleet_orchestration_status
            WHERE cluster = $1
            ORDER BY opportunity_state NULLS LAST
            "#,
        )
        .bind(cluster)
        .fetch_all(self.pool())
        .await?;
        // Match the two partial queue indexes from migration 24 exactly. The
        // UNION keeps each branch indexable and limits expansion to current
        // nonterminal holders instead of scanning signed-submission history.
        let congestion_rows = sqlx::query(
            r#"
            WITH active_submission AS (
                SELECT submission.id, submission.opportunity_id,
                       submission.fee_payer, submission.writable_account_keys
                FROM loyal_yield.signed_route_submissions submission
                WHERE submission.cluster = $1
                  AND submission.decision_id IS NOT NULL
                  AND submission.submission_state IN (
                      'signed', 'submitted', 'confirmed'
                  )

                UNION ALL

                SELECT submission.id, submission.opportunity_id,
                       submission.fee_payer, submission.writable_account_keys
                FROM loyal_yield.signed_route_submissions submission
                WHERE submission.cluster = $1
                  AND submission.decision_id IS NOT NULL
                  AND submission.submission_state IN (
                      'reconciliation_pending',
                      'expiry_check_pending',
                      'effect_ambiguous'
                  )
            ), physical_write AS (
                SELECT submission.id AS submission_id,
                       writable.writable_account_key,
                       CASE
                           WHEN writable.writable_account_key = submission.fee_payer THEN 0
                           WHEN writable.writable_account_key = opportunity.target_reserve THEN 1
                           ELSE 2
                       END AS classification_rank,
                       opportunity.principal_usd_micros,
                       opportunity.annual_yield_gain_usd_micros
                FROM active_submission submission
                JOIN loyal_yield.rebalance_opportunities opportunity
                  ON opportunity.id = submission.opportunity_id
                CROSS JOIN LATERAL unnest(submission.writable_account_keys)
                    AS writable(writable_account_key)
            ), congestion AS (
                SELECT writable_account_key,
                       min(classification_rank) AS classification_rank,
                       count(*)::BIGINT AS active_submission_count,
                       COALESCE(sum(principal_usd_micros), 0)::BIGINT
                           AS principal_usd_micros,
                       COALESCE(
                           (sum(annual_yield_gain_usd_micros) / 8760)::BIGINT,
                           0
                       )::BIGINT AS recoverable_yield_usd_micros_per_hour
                FROM physical_write
                GROUP BY writable_account_key
            )
            SELECT writable_account_key,
                   CASE classification_rank
                       WHEN 0 THEN 'payer'
                       WHEN 1 THEN 'target'
                       ELSE 'other'
                   END AS classification,
                   active_submission_count,
                   principal_usd_micros,
                   recoverable_yield_usd_micros_per_hour,
                   count(*) OVER ()::BIGINT AS total_active_physical_writable_key_count
            FROM congestion
            ORDER BY active_submission_count DESC,
                     recoverable_yield_usd_micros_per_hour DESC,
                     principal_usd_micros DESC,
                     writable_account_key
            LIMIT $2
            "#,
        )
        .bind(cluster)
        .bind(HOT_WRITABLE_KEY_LIMIT)
        .fetch_all(self.pool())
        .await?;
        let active_physical_writable_key_count = congestion_rows
            .first()
            .map(|row| row.try_get("total_active_physical_writable_key_count"))
            .transpose()?
            .unwrap_or_default();
        let top_physical_writable_key_congestion = congestion_rows
            .iter()
            .map(physical_writable_key_congestion_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        rows.iter()
            .map(fleet_status_from_row)
            .map(|status| {
                status.map(|mut status| {
                    status.active_physical_writable_key_count = active_physical_writable_key_count;
                    status.top_physical_writable_key_congestion =
                        top_physical_writable_key_congestion.clone();
                    status
                })
            })
            .collect()
    }

    /// Retires work whose immutable market epoch can no longer authorize a
    /// decision. Live leases, decisions, and signed submissions are excluded;
    /// their dedicated recovery lanes remain authoritative.
    pub async fn sweep_expired_rebalance_opportunities(
        &self,
        cluster: &str,
        limit: i64,
    ) -> Result<u64, OrchestratorError> {
        if cluster.trim().is_empty() || !(1..=10_000).contains(&limit) {
            return Err(OrchestratorError::StoreInvariant(
                "opportunity expiry sweep requires cluster and limit in 1..=10000".to_owned(),
            ));
        }
        let swept: i64 = sqlx::query_scalar(
            r#"
            WITH candidate AS (
                SELECT opportunity.id
                FROM loyal_yield.rebalance_opportunities opportunity
                WHERE opportunity.cluster = $1
                  AND opportunity.expires_at <= now()
                  AND opportunity.opportunity_state IN (
                      'waiting_alt', 'revalidate', 'ready', 'leased'
                  )
                  AND (
                      opportunity.opportunity_state <> 'leased'
                      OR opportunity.lease_expires_at <= now()
                  )
                  AND opportunity.decision_id IS NULL
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.signed_route_submissions submission
                      WHERE submission.opportunity_id = opportunity.id
                        AND submission.submission_state NOT IN (
                            'reconciled', 'expired', 'failed'
                        )
                  )
                ORDER BY opportunity.expires_at, opportunity.id
                FOR UPDATE OF opportunity SKIP LOCKED
                LIMIT $2
            ), stale AS (
                UPDATE loyal_yield.rebalance_opportunities opportunity
                SET opportunity_state = 'stale',
                    lease_kind = NULL,
                    lease_owner = NULL,
                    lease_expires_at = NULL,
                    terminal_reason = 'optimizer_epoch_expired',
                    updated_at = now()
                FROM candidate
                WHERE opportunity.id = candidate.id
                RETURNING opportunity.id
            ), released_conflicts AS (
                DELETE FROM loyal_yield.route_account_conflict_leases conflict
                USING stale
                WHERE conflict.opportunity_id = stale.id
                  AND conflict.submission_id IS NULL
                RETURNING conflict.opportunity_id
            )
            SELECT count(*)::BIGINT FROM stale
            "#,
        )
        .bind(cluster)
        .bind(limit)
        .fetch_one(self.pool())
        .await?;
        Ok(u64::try_from(swept).unwrap_or_default())
    }

    /// Claims one durable wakeup without coupling correctness to `NOTIFY`.
    /// Expired claims are recovered with a higher fence and concurrent
    /// consumers skip each other's rows.
    pub async fn lease_next_orchestration_outbox(
        &self,
        cluster: &str,
        owner: &str,
        lease_expires_at: DateTime<Utc>,
    ) -> Result<Option<OrchestrationOutboxLease>, OrchestratorError> {
        if cluster.trim().is_empty() || owner.trim().is_empty() || lease_expires_at <= Utc::now() {
            return Err(OrchestratorError::StoreInvariant(
                "outbox lease requires cluster, owner, and a future expiry".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT event.id
                FROM loyal_yield.orchestration_outbox event
                WHERE event.cluster = $1
                  AND event.processed_at IS NULL
                  AND event.available_at <= now()
                  AND (
                      event.lease_owner IS NULL
                      OR event.lease_expires_at <= now()
                  )
                ORDER BY event.available_at, event.created_at, event.id
                FOR UPDATE OF event SKIP LOCKED
                LIMIT 1
            )
            UPDATE loyal_yield.orchestration_outbox event
            SET lease_owner = $2,
                lease_expires_at = $3,
                fencing_token = event.fencing_token + 1,
                attempt_count = event.attempt_count + 1,
                updated_at = now()
            FROM candidate
            WHERE event.id = candidate.id
            RETURNING event.*
            "#,
        )
        .bind(cluster)
        .bind(owner)
        .bind(lease_expires_at)
        .fetch_optional(self.pool())
        .await?;
        let Some(row) = row else {
            return Ok(None);
        };
        let event = orchestration_outbox_from_row(&row)?;
        Ok(Some(OrchestrationOutboxLease {
            fencing_token: event.fencing_token,
            expires_at: lease_expires_at,
            owner: owner.to_owned(),
            event,
        }))
    }

    /// Acknowledges only the still-current outbox claim. A late worker cannot
    /// erase work already reclaimed under a newer fencing token.
    pub async fn acknowledge_orchestration_outbox(
        &self,
        lease: &OrchestrationOutboxLease,
    ) -> Result<OrchestrationOutboxRecord, OrchestratorError> {
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.orchestration_outbox
            SET processed_at = now(),
                lease_owner = NULL,
                lease_expires_at = NULL,
                last_error = NULL,
                updated_at = now()
            WHERE id = $1
              AND processed_at IS NULL
              AND lease_owner = $2
              AND fencing_token = $3
              AND lease_expires_at > now()
            RETURNING *
            "#,
        )
        .bind(lease.event.id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "outbox event {} acknowledgement is stale, expired, or fenced",
                lease.event.id
            ))
        })?;
        orchestration_outbox_from_row(&row)
    }

    /// Releases a failed delivery immediately so the queue's retry delay, not
    /// the claim TTL, controls the feedback loop.
    pub async fn retry_orchestration_outbox(
        &self,
        lease: &OrchestrationOutboxLease,
        available_at: DateTime<Utc>,
        error: &str,
    ) -> Result<OrchestrationOutboxRecord, OrchestratorError> {
        if error.trim().is_empty() {
            return Err(OrchestratorError::StoreInvariant(
                "outbox retry requires a nonempty error".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.orchestration_outbox
            SET available_at = $4,
                lease_owner = NULL,
                lease_expires_at = NULL,
                last_error = $5,
                updated_at = now()
            WHERE id = $1
              AND processed_at IS NULL
              AND lease_owner = $2
              AND fencing_token = $3
              AND lease_expires_at > now()
            RETURNING *
            "#,
        )
        .bind(lease.event.id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(available_at)
        .bind(error)
        .fetch_optional(self.pool())
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "outbox event {} retry is stale, expired, or fenced",
                lease.event.id
            ))
        })?;
        orchestration_outbox_from_row(&row)
    }

    /// Acknowledges ALT wakeups only after their durable opportunity state is
    /// already visible. The opportunity row is the correctness source; the
    /// outbox is a recoverable low-latency/audit signal and must not grow
    /// forever when notifications are lost or no listener is connected.
    pub async fn acknowledge_promoted_alt_outbox_batch(
        &self,
        cluster: &str,
        limit: i64,
    ) -> Result<u64, OrchestratorError> {
        if cluster.trim().is_empty() || !(1..=1024).contains(&limit) {
            return Err(OrchestratorError::StoreInvariant(
                "ALT outbox acknowledgement requires cluster and limit in 1..=1024".to_owned(),
            ));
        }
        let result = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT event.id
                FROM loyal_yield.orchestration_outbox event
                JOIN loyal_yield.rebalance_opportunities opportunity
                  ON event.aggregate_kind = 'rebalance_opportunity'
                 AND opportunity.id = event.aggregate_id
                WHERE event.cluster = $1
                  AND event.event_kind = 'alt_satisfied'
                  AND event.processed_at IS NULL
                  AND event.available_at <= now()
                  AND opportunity.opportunity_state <> 'waiting_alt'
                ORDER BY event.available_at, event.created_at, event.id
                FOR UPDATE OF event SKIP LOCKED
                LIMIT $2
            )
            UPDATE loyal_yield.orchestration_outbox event
            SET processed_at = now(),
                lease_owner = NULL,
                lease_expires_at = NULL,
                last_error = NULL,
                updated_at = now()
            FROM candidate
            WHERE event.id = candidate.id
            "#,
        )
        .bind(cluster)
        .bind(limit)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected())
    }

    /// Atomically owns the complete semantic conflict set for an execute
    /// lease. Fleet routes use a vault-exclusive key plus one bounded shared
    /// write lane; exact transaction writables are persisted separately.
    /// Keys are acquired in lexical order; one conflict rolls the set back.
    pub async fn acquire_route_account_conflict_leases(
        &self,
        opportunity_lease: &RebalanceOpportunityLease,
        writable_account_keys: &[String],
        expires_at: DateTime<Utc>,
    ) -> Result<Vec<RouteAccountConflictLease>, OrchestratorError> {
        if opportunity_lease.claim_kind != RebalanceOpportunityClaimKind::Execute {
            return Err(OrchestratorError::StoreInvariant(
                "only an execute lease may acquire route-account conflicts".to_owned(),
            ));
        }
        if expires_at <= Utc::now() || expires_at > opportunity_lease.expires_at {
            return Err(OrchestratorError::StoreInvariant(
                "conflict lease must expire in the future and no later than its opportunity lease"
                    .to_owned(),
            ));
        }
        let writable_account_keys = canonical_writable_account_keys(writable_account_keys)?;
        let mut tx = self.pool().begin().await?;
        let cluster: String = sqlx::query_scalar(
            r#"
            SELECT cluster
            FROM loyal_yield.rebalance_opportunities
            WHERE id = $1
              AND opportunity_state = 'leased'
              AND lease_kind = 'execute'
              AND lease_owner = $2
              AND fencing_token = $3
              AND lease_expires_at > now()
            FOR SHARE
            "#,
        )
        .bind(opportunity_lease.opportunity.id)
        .bind(&opportunity_lease.owner)
        .bind(opportunity_lease.fencing_token)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity {} conflict claim is stale, expired, or fenced",
                opportunity_lease.opportunity.id
            ))
        })?;
        if cluster != opportunity_lease.opportunity.cluster {
            return Err(OrchestratorError::StoreInvariant(
                "execute lease cluster differs from its durable opportunity".to_owned(),
            ));
        }

        for writable_account_key in &writable_account_keys {
            let acquired = sqlx::query(
                r#"
                INSERT INTO loyal_yield.route_account_conflict_leases AS conflict
                    (cluster, writable_account_key, opportunity_id, lease_owner,
                     fencing_token, expires_at)
                VALUES ($1, $2, $3, $4, $5, $6)
                ON CONFLICT (cluster, writable_account_key) DO UPDATE
                SET opportunity_id = EXCLUDED.opportunity_id,
                    lease_owner = EXCLUDED.lease_owner,
                    fencing_token = EXCLUDED.fencing_token,
                    expires_at = EXCLUDED.expires_at,
                    submission_id = NULL,
                    updated_at = now()
                WHERE conflict.submission_id IS NULL
                  AND (
                      conflict.expires_at <= now()
                      OR (
                          conflict.opportunity_id = EXCLUDED.opportunity_id
                          AND conflict.lease_owner = EXCLUDED.lease_owner
                          AND conflict.fencing_token = EXCLUDED.fencing_token
                      )
                  )
                RETURNING writable_account_key
                "#,
            )
            .bind(&cluster)
            .bind(writable_account_key)
            .bind(opportunity_lease.opportunity.id)
            .bind(&opportunity_lease.owner)
            .bind(opportunity_lease.fencing_token)
            .bind(expires_at)
            .fetch_optional(&mut *tx)
            .await?;
            if acquired.is_none() {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "writable account {writable_account_key:?} is owned by another route"
                )));
            }
        }

        sqlx::query(
            r#"
            DELETE FROM loyal_yield.route_account_conflict_leases
            WHERE cluster = $1
              AND opportunity_id = $2
              AND lease_owner = $3
              AND fencing_token = $4
              AND submission_id IS NULL
              AND NOT (writable_account_key = ANY($5))
            "#,
        )
        .bind(&cluster)
        .bind(opportunity_lease.opportunity.id)
        .bind(&opportunity_lease.owner)
        .bind(opportunity_lease.fencing_token)
        .bind(&writable_account_keys)
        .execute(&mut *tx)
        .await?;

        let rows = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.route_account_conflict_leases
            WHERE cluster = $1
              AND opportunity_id = $2
              AND lease_owner = $3
              AND fencing_token = $4
            ORDER BY writable_account_key
            "#,
        )
        .bind(&cluster)
        .bind(opportunity_lease.opportunity.id)
        .bind(&opportunity_lease.owner)
        .bind(opportunity_lease.fencing_token)
        .fetch_all(&mut *tx)
        .await?;
        let leases = rows
            .iter()
            .map(route_account_conflict_lease_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        if leases.len() != writable_account_keys.len() {
            return Err(OrchestratorError::StoreInvariant(
                "durable conflict lease set differs from the requested exact set".to_owned(),
            ));
        }
        tx.commit().await?;
        Ok(leases)
    }

    /// Releases only the complete semantic set held by this executor fence.
    /// Repeating a completed release is harmless; partial release requests are
    /// rejected so an account cannot be accidentally left unprotected.
    pub async fn release_route_account_conflict_leases(
        &self,
        opportunity_lease: &RebalanceOpportunityLease,
        writable_account_keys: &[String],
    ) -> Result<u64, OrchestratorError> {
        let writable_account_keys = canonical_writable_account_keys(writable_account_keys)?;
        let mut tx = self.pool().begin().await?;
        let held_keys = sqlx::query_scalar::<_, String>(
            r#"
            SELECT writable_account_key
            FROM loyal_yield.route_account_conflict_leases
            WHERE cluster = $1
              AND opportunity_id = $2
              AND lease_owner = $3
              AND fencing_token = $4
            ORDER BY writable_account_key
            FOR UPDATE
            "#,
        )
        .bind(&opportunity_lease.opportunity.cluster)
        .bind(opportunity_lease.opportunity.id)
        .bind(&opportunity_lease.owner)
        .bind(opportunity_lease.fencing_token)
        .fetch_all(&mut *tx)
        .await?;
        if held_keys.is_empty() {
            tx.commit().await?;
            return Ok(0);
        }
        if held_keys != writable_account_keys {
            return Err(OrchestratorError::StoreInvariant(
                "conflict release must name the exact currently-held writable-account set"
                    .to_owned(),
            ));
        }
        let result = sqlx::query(
            r#"
            DELETE FROM loyal_yield.route_account_conflict_leases
            WHERE cluster = $1
              AND opportunity_id = $2
              AND lease_owner = $3
              AND fencing_token = $4
              AND submission_id IS NULL
              AND writable_account_key = ANY($5)
            "#,
        )
        .bind(&opportunity_lease.opportunity.cluster)
        .bind(opportunity_lease.opportunity.id)
        .bind(&opportunity_lease.owner)
        .bind(opportunity_lease.fencing_token)
        .bind(&writable_account_keys)
        .execute(&mut *tx)
        .await?;
        if result.rows_affected() != writable_account_keys.len() as u64 {
            return Err(OrchestratorError::StoreInvariant(
                "conflict release lost atomic ownership of its exact key set".to_owned(),
            ));
        }
        tx.commit().await?;
        Ok(result.rows_affected())
    }

    /// Persists exact signed wire bytes inside a caller-owned transaction.
    /// Fleet callers must create/link the movement decision before committing;
    /// a deferred database constraint rejects decision-less signed rows.
    pub(crate) async fn persist_signed_route_submission_in_connection(
        connection: &mut sqlx::PgConnection,
        opportunity_lease: &RebalanceOpportunityLease,
        input: &SignedRouteSubmissionInput,
    ) -> Result<SignedRouteSubmissionRecord, OrchestratorError> {
        validate_signed_route_submission_input(opportunity_lease, input)?;
        let writable_account_keys = canonical_writable_account_keys(&input.writable_account_keys)?;
        let conflict_account_keys = canonical_conflict_account_keys(&input.conflict_account_keys)?;
        let signed_transaction_hash = format!("{:x}", Sha256::digest(&input.signed_transaction));
        if !signed_transaction_hash.eq_ignore_ascii_case(&input.signed_transaction_hash) {
            return Err(OrchestratorError::StoreInvariant(
                "signed transaction hash does not match the exact persisted wire bytes".to_owned(),
            ));
        }
        let minimum_publication_lifetime_seconds =
            i32::try_from(MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "minimum market-epoch publication lifetime does not fit PostgreSQL INTEGER"
                        .to_owned(),
                )
            })?;

        let row = sqlx::query(
            r#"
            SELECT opportunity.*
            FROM loyal_yield.rebalance_opportunities opportunity
            JOIN loyal_yield.optimizer_epochs epoch
              ON epoch.id = opportunity.optimizer_epoch_id
             AND epoch.cluster = opportunity.cluster
            WHERE opportunity.id = $1
              AND opportunity.opportunity_state = 'leased'
              AND opportunity.lease_kind = 'execute'
              AND opportunity.lease_owner = $2
              AND opportunity.fencing_token = $3
              AND opportunity.lease_expires_at > clock_timestamp()
              AND opportunity.expires_at >= clock_timestamp()
                  + make_interval(secs => $4::INTEGER)
              AND epoch.expires_at >= clock_timestamp()
                  + make_interval(secs => $4::INTEGER)
            FOR SHARE OF opportunity, epoch
            "#,
        )
        .bind(opportunity_lease.opportunity.id)
        .bind(&opportunity_lease.owner)
        .bind(opportunity_lease.fencing_token)
        .bind(minimum_publication_lifetime_seconds)
        .fetch_optional(&mut *connection)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity {} signing lease is stale, below the minimum signed-publication lifetime, or fenced",
                opportunity_lease.opportunity.id
            ))
        })?;
        let opportunity = rebalance_opportunity_from_row(&row)?;
        if opportunity.cluster != input.cluster
            || opportunity.id != input.opportunity_id
            || opportunity.source_snapshot_id != input.source_snapshot_id
            || opportunity.optimizer_epoch_id != input.optimizer_epoch_id
            || opportunity.requirements_fingerprint.as_deref()
                != Some(input.alt_requirements_fingerprint.as_str())
        {
            return Err(OrchestratorError::StoreInvariant(
                "signed submission evidence does not match its leased opportunity".to_owned(),
            ));
        }
        if input.compiled_fee_lamports > opportunity.estimated_cost_lamports {
            return Err(OrchestratorError::StoreInvariant(
                "signed submission compiled fee exceeds its economic opportunity cap".to_owned(),
            ));
        }

        let conflict_keys = sqlx::query_scalar::<_, String>(
            r#"
            SELECT writable_account_key
            FROM loyal_yield.route_account_conflict_leases
            WHERE cluster = $1
              AND opportunity_id = $2
              AND lease_owner = $3
              AND fencing_token = $4
              AND expires_at > now()
            ORDER BY writable_account_key
            FOR SHARE
            "#,
        )
        .bind(&input.cluster)
        .bind(input.opportunity_id)
        .bind(&input.executor_owner)
        .bind(input.executor_fencing_token)
        .fetch_all(&mut *connection)
        .await?;
        if conflict_keys != conflict_account_keys {
            return Err(OrchestratorError::StoreInvariant(
                "signed submission requires its exact live semantic conflict set".to_owned(),
            ));
        }

        sqlx::query(
            r#"
            INSERT INTO loyal_yield.signed_route_submissions
                (cluster, semantic_key, opportunity_id, decision_id,
                 signed_transaction, signed_transaction_hash, message_hash,
                 transaction_signature, recent_blockhash, last_valid_block_height,
                 source_snapshot_id, optimizer_epoch_id,
                 alt_requirements_fingerprint, alt_selection_fingerprint,
                 alt_mutation_epochs, fee_payer, fee_payer_kind,
                 compiled_fee_lamports,
                 writable_account_keys, conflict_account_keys,
                 executor_owner, executor_fencing_token)
            VALUES
                ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
                 $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
            ON CONFLICT (semantic_key) DO NOTHING
            "#,
        )
        .bind(&input.cluster)
        .bind(&input.semantic_key)
        .bind(input.opportunity_id)
        .bind(input.decision_id.map(DecisionId::as_i64))
        .bind(&input.signed_transaction)
        .bind(&signed_transaction_hash)
        .bind(&input.message_hash)
        .bind(&input.transaction_signature)
        .bind(&input.recent_blockhash)
        .bind(input.last_valid_block_height)
        .bind(input.source_snapshot_id.map(SnapshotId::as_i64))
        .bind(input.optimizer_epoch_id)
        .bind(&input.alt_requirements_fingerprint)
        .bind(&input.alt_selection_fingerprint)
        .bind(&input.alt_mutation_epochs)
        .bind(&input.fee_payer)
        .bind(input.fee_payer_kind.as_str())
        .bind(input.compiled_fee_lamports)
        .bind(&writable_account_keys)
        .bind(&conflict_account_keys)
        .bind(&input.executor_owner)
        .bind(input.executor_fencing_token)
        .execute(&mut *connection)
        .await?;

        let row = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.signed_route_submissions
            WHERE semantic_key = $1
            FOR SHARE
            "#,
        )
        .bind(&input.semantic_key)
        .fetch_one(&mut *connection)
        .await?;
        let submission = signed_route_submission_from_row(&row)?;
        if !signed_route_submission_matches_input(
            &submission,
            input,
            &signed_transaction_hash,
            &writable_account_keys,
            &conflict_account_keys,
        ) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "signed submission semantic key {:?} collided with different immutable evidence",
                input.semantic_key
            )));
        }
        reserve_fee_only_route_payer_spend(connection, input, submission.id).await?;
        let attached = sqlx::query(
            r#"
            UPDATE loyal_yield.route_account_conflict_leases
            SET submission_id = $5,
                expires_at = GREATEST(expires_at, now() + interval '10 minutes'),
                updated_at = now()
            WHERE cluster = $1
              AND opportunity_id = $2
              AND lease_owner = $3
              AND fencing_token = $4
              AND writable_account_key = ANY($6)
              AND expires_at > now()
            "#,
        )
        .bind(&input.cluster)
        .bind(input.opportunity_id)
        .bind(&input.executor_owner)
        .bind(input.executor_fencing_token)
        .bind(submission.id)
        .bind(&conflict_account_keys)
        .execute(&mut *connection)
        .await?;
        if attached.rows_affected() != conflict_account_keys.len() as u64 {
            return Err(OrchestratorError::StoreInvariant(
                "signed submission failed to assume its exact semantic conflict set".to_owned(),
            ));
        }
        Ok(submission)
    }

    /// Rechecks the DB clock after the decision, submission, and capacity rows
    /// are linked. Callers must invoke this as their final SQL statement before
    /// committing an atomic signed-decision handoff.
    pub(crate) async fn assert_signed_route_publication_lifetime_in_connection(
        connection: &mut sqlx::PgConnection,
        opportunity_id: i64,
        decision_id: DecisionId,
        signed_submission_id: i64,
    ) -> Result<(), OrchestratorError> {
        let minimum_publication_lifetime_seconds =
            i32::try_from(MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS).map_err(|_| {
                OrchestratorError::StoreInvariant(
                    "minimum market-epoch publication lifetime does not fit PostgreSQL INTEGER"
                        .to_owned(),
                )
            })?;
        let publication_lifetime_ready: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.rebalance_opportunities opportunity
                JOIN loyal_yield.optimizer_epochs epoch
                  ON epoch.id = opportunity.optimizer_epoch_id
                 AND epoch.cluster = opportunity.cluster
                JOIN loyal_yield.signed_route_submissions submission
                  ON submission.id = $3
                 AND submission.opportunity_id = opportunity.id
                 AND submission.decision_id = $2
                WHERE opportunity.id = $1
                  AND opportunity.opportunity_state = 'decision_created'
                  AND opportunity.decision_id = $2
                  AND submission.submission_state = 'signed'
                  AND opportunity.expires_at >= clock_timestamp()
                      + make_interval(secs => $4::INTEGER)
                  AND epoch.expires_at >= clock_timestamp()
                      + make_interval(secs => $4::INTEGER)
            )
            "#,
        )
        .bind(opportunity_id)
        .bind(decision_id.as_i64())
        .bind(signed_submission_id)
        .bind(minimum_publication_lifetime_seconds)
        .fetch_one(&mut *connection)
        .await?;
        if !publication_lifetime_ready {
            return Err(OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity {opportunity_id} lost the minimum usable market-epoch lifetime before signed decision commit"
            )));
        }
        Ok(())
    }

    pub async fn signed_route_submission_by_semantic_key(
        &self,
        semantic_key: &str,
    ) -> Result<Option<SignedRouteSubmissionRecord>, OrchestratorError> {
        let row = sqlx::query(
            "SELECT * FROM loyal_yield.signed_route_submissions WHERE semantic_key = $1",
        )
        .bind(semantic_key)
        .fetch_optional(self.pool())
        .await?;
        row.as_ref()
            .map(signed_route_submission_from_row)
            .transpose()
    }

    pub async fn active_signed_route_submission_for_opportunity(
        &self,
        opportunity_id: i64,
    ) -> Result<Option<SignedRouteSubmissionRecord>, OrchestratorError> {
        let row = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.signed_route_submissions
            WHERE opportunity_id = $1
              AND submission_state NOT IN ('reconciled', 'expired', 'failed')
            "#,
        )
        .bind(opportunity_id)
        .fetch_optional(self.pool())
        .await?;
        row.as_ref()
            .map(signed_route_submission_from_row)
            .transpose()
    }

    /// Collapses the already-built, simulated, fee-checked, and signed fleet
    /// handoff into one fenced decision transition. Legacy decision flows keep
    /// their stepwise simulation lifecycle; signed fleet routes do not spend
    /// nine database round trips replaying work that happened before signing.
    pub async fn ensure_signed_route_decision_confirming(
        &self,
        submission: &SignedRouteSubmissionRecord,
    ) -> Result<(), OrchestratorError> {
        let decision_id = submission.decision_id.ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "signed route submission has no linked decision".to_owned(),
            )
        })?;
        let advanced = sqlx::query_scalar::<_, i64>(
            r#"
            WITH eligible AS (
                SELECT decision.id
                FROM loyal_yield.signed_route_submissions persisted
                JOIN loyal_yield.rebalance_decisions decision
                  ON decision.id = persisted.decision_id
                WHERE persisted.id = $1
                  AND persisted.decision_id = $2
                  AND persisted.transaction_signature = $3
                  AND persisted.submission_state IN (
                      'signed', 'submitted', 'confirmed'
                  )
                  AND decision.status::text IN (
                      'planned', 'simulating', 'ready', 'submitted',
                      'confirming', 'confirmed'
                  )
                  AND (
                      decision.signature IS NULL
                      OR decision.signature = persisted.transaction_signature
                  )
                FOR UPDATE OF persisted, decision
            ), advanced AS (
                UPDATE loyal_yield.rebalance_decisions decision
                SET status = CASE
                        WHEN decision.status::text = 'confirmed'
                            THEN decision.status
                        ELSE 'confirming'::loyal_yield.decision_status
                    END,
                    signature = COALESCE(decision.signature, $3),
                    updated_at = now()
                FROM eligible
                WHERE decision.id = eligible.id
                RETURNING decision.id
            )
            SELECT id FROM advanced
            "#,
        )
        .bind(submission.id)
        .bind(decision_id.as_i64())
        .bind(&submission.transaction_signature)
        .fetch_optional(self.pool())
        .await?;
        if advanced != Some(decision_id.as_i64()) {
            return Err(OrchestratorError::StoreInvariant(format!(
                "signed route decision {decision_id} is missing, terminal, or diverged from its immutable signature"
            )));
        }
        Ok(())
    }

    /// Atomically advances the linked decisions and records first/rebroadcast
    /// intent for a fenced batch. Callers must validate every exact wire image
    /// before invoking this method; no network send may happen before it
    /// commits. A single stale row rolls the transaction back so a partial
    /// batch can never cross the durable broadcast boundary unnoticed.
    pub async fn prepare_signed_route_broadcast_batch(
        &self,
        leases: &[SignedRouteSubmissionLease],
        checked_at: DateTime<Utc>,
    ) -> Result<Vec<SignedRouteSubmissionLease>, OrchestratorError> {
        if leases.is_empty() {
            return Ok(Vec::new());
        }
        if leases.len() > 256
            || leases
                .iter()
                .map(|lease| lease.submission.id)
                .collect::<BTreeSet<_>>()
                .len()
                != leases.len()
            || leases.iter().any(|lease| {
                lease.submission.id <= 0
                    || lease.owner.trim().is_empty()
                    || lease.fencing_token <= 0
                    || lease.submission.transaction_signature.trim().is_empty()
            })
        {
            return Err(OrchestratorError::StoreInvariant(
                "signed-route broadcast batch requires 1..=256 unique, fenced submissions"
                    .to_owned(),
            ));
        }

        let ids = leases
            .iter()
            .map(|lease| lease.submission.id)
            .collect::<Vec<_>>();
        let owners = leases
            .iter()
            .map(|lease| lease.owner.clone())
            .collect::<Vec<_>>();
        let fencing_tokens = leases
            .iter()
            .map(|lease| lease.fencing_token)
            .collect::<Vec<_>>();
        let signatures = leases
            .iter()
            .map(|lease| lease.submission.transaction_signature.clone())
            .collect::<Vec<_>>();
        let lease_by_id = leases
            .iter()
            .map(|lease| (lease.submission.id, lease))
            .collect::<BTreeMap<_, _>>();

        let mut tx = self.pool().begin().await?;
        let rows = sqlx::query(
            r#"
            WITH expected AS (
                SELECT claim.id, claim.lease_owner, claim.fencing_token,
                       claim.transaction_signature
                FROM unnest(
                    $1::BIGINT[], $2::TEXT[], $3::BIGINT[], $4::TEXT[]
                ) AS claim(
                    id, lease_owner, fencing_token, transaction_signature
                )
            ), eligible AS (
                SELECT submission.id, submission.decision_id,
                       submission.transaction_signature
                FROM expected claim
                JOIN loyal_yield.signed_route_submissions submission
                  ON submission.id = claim.id
                JOIN loyal_yield.rebalance_decisions decision
                  ON decision.id = submission.decision_id
                WHERE submission.submission_state IN ('signed', 'submitted')
                  AND submission.transaction_signature = claim.transaction_signature
                  AND submission.confirmation_lease_owner = claim.lease_owner
                  AND submission.confirmation_fencing_token = claim.fencing_token
                  AND submission.confirmation_lease_expires_at > clock_timestamp()
                  AND decision.status::text IN (
                      'planned', 'simulating', 'ready', 'submitted',
                      'confirming', 'confirmed'
                  )
                  AND (
                      decision.signature IS NULL
                      OR decision.signature = submission.transaction_signature
                  )
                FOR UPDATE OF submission, decision
            ), advanced_decisions AS (
                UPDATE loyal_yield.rebalance_decisions decision
                SET status = CASE
                        WHEN decision.status::text = 'confirmed'
                            THEN decision.status
                        ELSE 'confirming'::loyal_yield.decision_status
                    END,
                    signature = COALESCE(
                        decision.signature, eligible.transaction_signature
                    ),
                    updated_at = now()
                FROM eligible
                WHERE decision.id = eligible.decision_id
                RETURNING decision.id
            ), intents AS (
                UPDATE loyal_yield.signed_route_submissions submission
                SET broadcast_count = submission.broadcast_count + 1,
                    last_broadcast_at = $5,
                    last_status_checked_at = $5,
                    error_detail = 'broadcast_intent_persisted',
                    updated_at = now()
                FROM eligible
                WHERE submission.id = eligible.id
                  AND (SELECT count(*) FROM advanced_decisions) =
                      (SELECT count(*) FROM eligible)
                RETURNING submission.*
            )
            SELECT * FROM intents
            "#,
        )
        .bind(ids)
        .bind(owners)
        .bind(fencing_tokens)
        .bind(signatures)
        .bind(checked_at)
        .fetch_all(&mut *tx)
        .await?;
        if rows.len() != leases.len() {
            tx.rollback().await?;
            return Err(OrchestratorError::StoreInvariant(
                "signed-route broadcast batch contains a stale, expired, or divergent fence"
                    .to_owned(),
            ));
        }

        let mut prepared = Vec::with_capacity(rows.len());
        for row in &rows {
            let submission = signed_route_submission_from_row(row)?;
            let original = lease_by_id.get(&submission.id).ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "signed-route broadcast batch returned an unexpected submission".to_owned(),
                )
            })?;
            prepared.push(SignedRouteSubmissionLease {
                submission,
                owner: original.owner.clone(),
                fencing_token: original.fencing_token,
                expires_at: original.expires_at,
            });
        }
        tx.commit().await?;
        Ok(prepared)
    }

    /// Commits authoritative confirmation evidence and the asynchronous
    /// reconciliation handoff for a whole fenced batch in one transaction.
    /// The durable confirmed slot remains explicit even though no worker can
    /// observe an intermediate leased `confirmed` row.
    pub async fn confirm_signed_route_submission_batch(
        &self,
        confirmations: &[(SignedRouteSubmissionLease, i64)],
        checked_at: DateTime<Utc>,
    ) -> Result<u64, OrchestratorError> {
        if confirmations.is_empty() {
            return Ok(0);
        }
        if confirmations.len() > 256
            || confirmations.iter().any(|(lease, slot)| {
                lease.submission.id <= 0
                    || lease.owner.trim().is_empty()
                    || lease.fencing_token <= 0
                    || *slot < 0
                    || lease.submission.transaction_signature.trim().is_empty()
            })
            || confirmations
                .iter()
                .map(|(lease, _)| lease.submission.id)
                .collect::<BTreeSet<_>>()
                .len()
                != confirmations.len()
        {
            return Err(OrchestratorError::StoreInvariant(
                "signed-route confirmation batch requires 1..=256 unique, fenced observations"
                    .to_owned(),
            ));
        }

        let ids = confirmations
            .iter()
            .map(|(lease, _)| lease.submission.id)
            .collect::<Vec<_>>();
        let owners = confirmations
            .iter()
            .map(|(lease, _)| lease.owner.clone())
            .collect::<Vec<_>>();
        let fencing_tokens = confirmations
            .iter()
            .map(|(lease, _)| lease.fencing_token)
            .collect::<Vec<_>>();
        let signatures = confirmations
            .iter()
            .map(|(lease, _)| lease.submission.transaction_signature.clone())
            .collect::<Vec<_>>();
        let slots = confirmations
            .iter()
            .map(|(_, slot)| *slot)
            .collect::<Vec<_>>();

        let mut tx = self.pool().begin().await?;
        let rows = sqlx::query(
            r#"
            WITH expected AS (
                SELECT claim.id, claim.lease_owner, claim.fencing_token,
                       claim.transaction_signature, claim.confirmed_slot
                FROM unnest(
                    $1::BIGINT[], $2::TEXT[], $3::BIGINT[], $4::TEXT[],
                    $5::BIGINT[]
                ) AS claim(
                    id, lease_owner, fencing_token, transaction_signature,
                    confirmed_slot
                )
            ), eligible AS (
                SELECT submission.id, submission.decision_id,
                       submission.semantic_key, submission.transaction_signature,
                       claim.confirmed_slot
                FROM expected claim
                JOIN loyal_yield.signed_route_submissions submission
                  ON submission.id = claim.id
                JOIN loyal_yield.rebalance_decisions decision
                  ON decision.id = submission.decision_id
                WHERE submission.submission_state IN (
                          'signed', 'submitted', 'confirmed'
                      )
                  AND submission.transaction_signature = claim.transaction_signature
                  AND (
                      submission.confirmed_slot IS NULL
                      OR submission.confirmed_slot = claim.confirmed_slot
                  )
                  AND submission.confirmation_lease_owner = claim.lease_owner
                  AND submission.confirmation_fencing_token = claim.fencing_token
                  AND submission.confirmation_lease_expires_at > clock_timestamp()
                  AND decision.status::text IN (
                      'planned', 'simulating', 'ready', 'submitted',
                      'confirming', 'confirmed'
                  )
                  AND (
                      decision.signature IS NULL
                      OR decision.signature = submission.transaction_signature
                  )
                FOR UPDATE OF submission, decision
            ), advanced_decisions AS (
                UPDATE loyal_yield.rebalance_decisions decision
                SET status = CASE
                        WHEN decision.status::text = 'confirmed'
                            THEN decision.status
                        ELSE 'confirming'::loyal_yield.decision_status
                    END,
                    signature = COALESCE(
                        decision.signature, eligible.transaction_signature
                    ),
                    updated_at = now()
                FROM eligible
                WHERE decision.id = eligible.decision_id
                RETURNING decision.id
            ), pending AS (
                UPDATE loyal_yield.signed_route_submissions submission
                SET submission_state = 'reconciliation_pending',
                    submitted_slot = COALESCE(
                        submission.submitted_slot, eligible.confirmed_slot
                    ),
                    submitted_at = COALESCE(
                        submission.submitted_at,
                        submission.last_broadcast_at,
                        $6
                    ),
                    confirmed_slot = COALESCE(
                        submission.confirmed_slot, eligible.confirmed_slot
                    ),
                    confirmed_at = COALESCE(submission.confirmed_at, $6),
                    last_status_checked_at = $6,
                    confirmation_lease_owner = NULL,
                    confirmation_lease_expires_at = NULL,
                    error_detail = NULL,
                    updated_at = now()
                FROM eligible
                WHERE submission.id = eligible.id
                  AND (SELECT count(*) FROM advanced_decisions) =
                      (SELECT count(*) FROM eligible)
                RETURNING submission.id, submission.semantic_key
            ), released_transient_conflicts AS (
                DELETE FROM loyal_yield.route_account_conflict_leases conflict
                USING pending
                WHERE conflict.submission_id = pending.id
                  AND (
                      conflict.writable_account_key LIKE
                          'fleet-shared-write-lane:%'
                      OR conflict.writable_account_key LIKE
                          'policy-setup-funding:%'
                  )
                RETURNING conflict.writable_account_key
            ), released_alt AS (
                UPDATE loyal_yield.lookup_table_usage_leases usage
                SET released_at = COALESCE(usage.released_at, now()),
                    updated_at = now()
                FROM pending
                WHERE usage.lease_kind = 'prepared_transaction'
                  AND usage.reference_key = pending.semantic_key
                RETURNING usage.id
            )
            SELECT count(*)::BIGINT FROM pending
            "#,
        )
        .bind(ids)
        .bind(owners)
        .bind(fencing_tokens)
        .bind(signatures)
        .bind(slots)
        .bind(checked_at)
        .fetch_one(&mut *tx)
        .await?;
        let advanced: i64 = rows.try_get(0)?;
        if usize::try_from(advanced).ok() != Some(confirmations.len()) {
            tx.rollback().await?;
            return Err(OrchestratorError::StoreInvariant(
                "signed-route confirmation batch contains a stale, expired, or divergent fence"
                    .to_owned(),
            ));
        }
        tx.commit().await?;
        Ok(u64::try_from(advanced).unwrap_or_default())
    }

    /// Releases only the still-current confirmation leases after a local or
    /// upstream failure. Submission state and immutable wire evidence are not
    /// changed; terminal or already-handed-off rows are deliberately ignored.
    pub async fn defer_signed_route_submission_lease_batch(
        &self,
        leases: &[SignedRouteSubmissionLease],
        checked_at: DateTime<Utc>,
        next_poll_at: DateTime<Utc>,
        error_detail: &str,
    ) -> Result<u64, OrchestratorError> {
        if leases.is_empty() {
            return Ok(0);
        }
        if leases.len() > 256
            || next_poll_at < checked_at
            || error_detail.trim().is_empty()
            || error_detail.len() > 512
            || leases
                .iter()
                .map(|lease| lease.submission.id)
                .collect::<BTreeSet<_>>()
                .len()
                != leases.len()
        {
            return Err(OrchestratorError::StoreInvariant(
                "signed-route failure release requires unique bounded leases, error, and retry time"
                    .to_owned(),
            ));
        }
        let ids = leases
            .iter()
            .map(|lease| lease.submission.id)
            .collect::<Vec<_>>();
        let owners = leases
            .iter()
            .map(|lease| lease.owner.clone())
            .collect::<Vec<_>>();
        let fencing_tokens = leases
            .iter()
            .map(|lease| lease.fencing_token)
            .collect::<Vec<_>>();
        let signatures = leases
            .iter()
            .map(|lease| lease.submission.transaction_signature.clone())
            .collect::<Vec<_>>();
        let released = sqlx::query_scalar::<_, i64>(
            r#"
            WITH expected AS (
                SELECT claim.id, claim.lease_owner, claim.fencing_token,
                       claim.transaction_signature
                FROM unnest(
                    $1::BIGINT[], $2::TEXT[], $3::BIGINT[], $4::TEXT[]
                ) AS claim(
                    id, lease_owner, fencing_token, transaction_signature
                )
            ), released AS (
                UPDATE loyal_yield.signed_route_submissions submission
                SET confirmation_available_at = $6,
                    confirmation_lease_owner = NULL,
                    confirmation_lease_expires_at = NULL,
                    last_status_checked_at = $5,
                    error_detail = $7,
                    updated_at = now()
                FROM expected claim
                WHERE submission.id = claim.id
                  AND submission.submission_state IN (
                      'signed', 'submitted', 'confirmed'
                  )
                  AND submission.transaction_signature = claim.transaction_signature
                  AND submission.confirmation_lease_owner = claim.lease_owner
                  AND submission.confirmation_fencing_token = claim.fencing_token
                  AND submission.confirmation_lease_expires_at > clock_timestamp()
                RETURNING submission.id
            )
            SELECT count(*)::BIGINT FROM released
            "#,
        )
        .bind(ids)
        .bind(owners)
        .bind(fencing_tokens)
        .bind(signatures)
        .bind(checked_at)
        .bind(next_poll_at)
        .bind(error_detail)
        .fetch_one(self.pool())
        .await?;
        Ok(u64::try_from(released).unwrap_or_default())
    }

    /// Returns send/recovery work only after the decision trigger has linked
    /// the signed payload to its durable decision. Exact bytes are never
    /// rebuilt by a submission worker.
    pub async fn pending_signed_route_submissions(
        &self,
        cluster: &str,
        limit: i64,
    ) -> Result<Vec<SignedRouteSubmissionRecord>, OrchestratorError> {
        if cluster.trim().is_empty() || !(1..=1_000).contains(&limit) {
            return Err(OrchestratorError::StoreInvariant(
                "pending signed-submission fetch requires cluster and limit in 1..=1000".to_owned(),
            ));
        }
        let rows = sqlx::query(
            r#"
            SELECT *
            FROM loyal_yield.signed_route_submissions
            WHERE cluster = $1
              AND decision_id IS NOT NULL
              AND submission_state IN ('signed', 'submitted')
            ORDER BY created_at, id
            LIMIT $2
            "#,
        )
        .bind(cluster)
        .bind(limit)
        .fetch_all(self.pool())
        .await?;
        rows.iter().map(signed_route_submission_from_row).collect()
    }

    /// Claims a value-prioritized confirmation batch. Confirmed rows are also
    /// claimable so a crash between the durable `confirmed` write and the
    /// `reconciliation_pending` handoff cannot strand money movement.
    pub async fn lease_pending_signed_route_submissions(
        &self,
        cluster: &str,
        owner: &str,
        limit: i64,
        lease_expires_at: DateTime<Utc>,
    ) -> Result<Vec<SignedRouteSubmissionLease>, OrchestratorError> {
        if cluster.trim().is_empty()
            || owner.trim().is_empty()
            || owner.len() > 128
            || !(1..=256).contains(&limit)
            || lease_expires_at <= Utc::now()
        {
            return Err(OrchestratorError::StoreInvariant(
                "signed-submission claim requires cluster, owner, limit in 1..=256, and future expiry"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let rows = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT submission.id
                FROM loyal_yield.signed_route_submissions submission
                JOIN loyal_yield.rebalance_opportunities opportunity
                  ON opportunity.id = submission.opportunity_id
                WHERE submission.cluster = $1
                  AND submission.decision_id IS NOT NULL
                  AND submission.submission_state IN ('signed', 'submitted', 'confirmed')
                  AND submission.confirmation_available_at <= now()
                  AND (
                      submission.confirmation_lease_owner IS NULL
                      OR submission.confirmation_lease_expires_at <= now()
                  )
                  AND cardinality(submission.conflict_account_keys) = (
                      SELECT count(*)::INTEGER
                      FROM loyal_yield.route_account_conflict_leases conflict
                      WHERE conflict.submission_id = submission.id
                        AND conflict.cluster = submission.cluster
                        AND conflict.writable_account_key = ANY(
                            submission.conflict_account_keys
                        )
                  )
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.route_account_conflict_leases conflict
                      WHERE conflict.submission_id = submission.id
                        AND (
                            conflict.cluster <> submission.cluster
                            OR NOT (
                                conflict.writable_account_key = ANY(
                                    submission.conflict_account_keys
                                )
                            )
                        )
                  )
                ORDER BY
                    CASE submission.submission_state WHEN 'confirmed' THEN 0 ELSE 1 END,
                    opportunity.economic_priority DESC,
                    submission.created_at,
                    submission.id
                FOR UPDATE OF submission SKIP LOCKED
                LIMIT $4
            )
            UPDATE loyal_yield.signed_route_submissions submission
            SET confirmation_lease_owner = $2,
                confirmation_lease_expires_at = $3,
                confirmation_fencing_token = submission.confirmation_fencing_token + 1,
                confirmation_attempt_count = submission.confirmation_attempt_count + 1,
                updated_at = now()
            FROM candidate
            WHERE submission.id = candidate.id
            RETURNING submission.*
            "#,
        )
        .bind(cluster)
        .bind(owner)
        .bind(lease_expires_at)
        .bind(limit)
        .fetch_all(&mut *tx)
        .await?;
        let leases = rows
            .iter()
            .map(|row| {
                let submission = signed_route_submission_from_row(row)?;
                Ok(SignedRouteSubmissionLease {
                    fencing_token: submission.confirmation_fencing_token,
                    expires_at: lease_expires_at,
                    owner: owner.to_owned(),
                    submission,
                })
            })
            .collect::<Result<Vec<_>, OrchestratorError>>()?;
        if !leases.is_empty() {
            let submission_ids = leases
                .iter()
                .map(|lease| lease.submission.id)
                .collect::<Vec<_>>();
            let expected_conflict_count = leases
                .iter()
                .map(|lease| lease.submission.conflict_account_keys.len() as u64)
                .sum::<u64>();
            let renewed_conflicts = sqlx::query(
                r#"
                UPDATE loyal_yield.route_account_conflict_leases
                SET expires_at = GREATEST(expires_at, $2 + interval '2 minutes'),
                    updated_at = now()
                WHERE submission_id = ANY($1)
                "#,
            )
            .bind(&submission_ids)
            .bind(lease_expires_at)
            .execute(&mut *tx)
            .await?;
            if renewed_conflicts.rows_affected() != expected_conflict_count {
                return Err(OrchestratorError::StoreInvariant(
                    "confirmation claim lost the exact signed-route semantic conflict set"
                        .to_owned(),
                ));
            }
            let semantic_keys = leases
                .iter()
                .map(|lease| lease.submission.semantic_key.clone())
                .collect::<Vec<_>>();
            let expected_alt_leases = leases.iter().try_fold(
                BTreeMap::new(),
                |mut expected, lease| -> Result<_, OrchestratorError> {
                    for (table_id, mutation_epoch) in
                        signed_route_submission_alt_table_epochs(&lease.submission)?
                    {
                        expected.insert(
                            (lease.submission.semantic_key.clone(), table_id),
                            (
                                mutation_epoch,
                                lease.submission.alt_requirements_fingerprint.clone(),
                            ),
                        );
                    }
                    Ok(expected)
                },
            )?;
            let protected_alt_rows = sqlx::query(
                r#"
                SELECT usage.reference_key,
                       usage.route_lookup_table_id,
                       usage.requirements_fingerprint,
                       route_table.mutation_epoch
                FROM loyal_yield.lookup_table_usage_leases usage
                JOIN loyal_yield.route_lookup_tables route_table
                  ON route_table.id = usage.route_lookup_table_id
                WHERE usage.lease_kind = 'prepared_transaction'
                  AND usage.reference_key = ANY($1)
                  AND usage.cluster = $2
                  AND usage.released_at IS NULL
                  AND usage.expires_at > now()
                  AND route_table.cluster = usage.cluster
                  AND route_table.family_id IS NOT NULL
                  AND route_table.desired_state = 'active'
                  AND route_table.mutation_epoch IS NOT NULL
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.lookup_table_operations cleanup
                      WHERE cleanup.route_lookup_table_id = route_table.id
                        AND cleanup.operation_kind IN ('deactivate', 'close')
                        AND cleanup.operation_state NOT IN (
                            'complete', 'permanent_failure', 'cancelled'
                        )
                )
                ORDER BY usage.reference_key, usage.route_lookup_table_id
                FOR UPDATE OF usage, route_table
                "#,
            )
            .bind(&semantic_keys)
            .bind(cluster)
            .fetch_all(&mut *tx)
            .await?;
            let protected_alt_rows = protected_alt_rows
                .iter()
                .map(|row| {
                    let key = (
                        row.try_get::<String, _>("reference_key")?,
                        row.try_get::<i64, _>("route_lookup_table_id")?,
                    );
                    let value = (
                        row.try_get::<i64, _>("mutation_epoch")?,
                        row.try_get::<Option<String>, _>("requirements_fingerprint")?
                            .unwrap_or_default(),
                    );
                    Ok((key, value))
                })
                .collect::<Result<BTreeMap<_, _>, OrchestratorError>>()?;
            if protected_alt_rows != expected_alt_leases {
                return Err(OrchestratorError::StoreInvariant(
                    "confirmation claim cannot prove the exact unexpired selectable ALT protection set"
                        .to_owned(),
                ));
            }
            let renewed_alt_leases = sqlx::query(
                r#"
                UPDATE loyal_yield.lookup_table_usage_leases
                SET expires_at = GREATEST(expires_at, $2 + interval '2 minutes'),
                    updated_at = now()
                WHERE lease_kind = 'prepared_transaction'
                  AND reference_key = ANY($1)
                  AND released_at IS NULL
                  AND expires_at > now()
                RETURNING reference_key, route_lookup_table_id
                "#,
            )
            .bind(&semantic_keys)
            .bind(lease_expires_at)
            .fetch_all(&mut *tx)
            .await?;
            let renewed_alt_leases = renewed_alt_leases
                .iter()
                .map(|row| {
                    Ok((
                        row.try_get::<String, _>("reference_key")?,
                        row.try_get::<i64, _>("route_lookup_table_id")?,
                    ))
                })
                .collect::<Result<BTreeSet<_>, OrchestratorError>>()?;
            if renewed_alt_leases != expected_alt_leases.keys().cloned().collect() {
                return Err(OrchestratorError::StoreInvariant(
                    "confirmation claim lost the exact prepared-ALT lease set".to_owned(),
                ));
            }
        }
        tx.commit().await?;
        Ok(leases)
    }

    /// Claims confirmed routes that still need their slot-fenced post-state
    /// persisted. This is a separate lane from signature confirmation so slow
    /// RPC state reads cannot consume broadcast capacity.
    pub async fn lease_reconciliation_pending_signed_route_submissions(
        &self,
        cluster: &str,
        owner: &str,
        limit: i64,
        lease_expires_at: DateTime<Utc>,
    ) -> Result<Vec<SignedRouteSubmissionLease>, OrchestratorError> {
        if cluster.trim().is_empty()
            || owner.trim().is_empty()
            || owner.len() > 128
            || !(1..=256).contains(&limit)
            || lease_expires_at <= Utc::now()
        {
            return Err(OrchestratorError::StoreInvariant(
                "reconciliation claim requires cluster, owner, limit in 1..=256, and future expiry"
                    .to_owned(),
            ));
        }

        let mut tx = self.pool().begin().await?;
        let rows = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT submission.id
                FROM loyal_yield.signed_route_submissions submission
                JOIN loyal_yield.rebalance_opportunities opportunity
                  ON opportunity.id = submission.opportunity_id
                WHERE submission.cluster = $1
                  AND submission.decision_id IS NOT NULL
                  AND submission.submission_state IN (
                      'reconciliation_pending', 'expiry_check_pending',
                      'effect_ambiguous'
                  )
                  AND (
                      (
                          submission.submission_state = 'reconciliation_pending'
                          AND submission.confirmed_slot IS NOT NULL
                      )
                      OR (
                          submission.submission_state = 'expiry_check_pending'
                          AND submission.expiry_observed_block_height IS NOT NULL
                          AND submission.effect_check_slot IS NOT NULL
                      )
                      OR (
                          submission.submission_state = 'effect_ambiguous'
                          AND submission.expiry_observed_block_height IS NOT NULL
                          AND submission.effect_check_slot IS NOT NULL
                      )
                  )
                  AND submission.confirmation_available_at <= now()
                  AND (
                      submission.confirmation_lease_owner IS NULL
                      OR submission.confirmation_lease_expires_at <= now()
                  )
                  AND (
                      (
                          submission.submission_state = 'expiry_check_pending'
                          AND cardinality(submission.conflict_account_keys) = (
                              SELECT count(*)::INTEGER
                              FROM loyal_yield.route_account_conflict_leases conflict
                              WHERE conflict.submission_id = submission.id
                                AND conflict.cluster = submission.cluster
                                AND conflict.writable_account_key = ANY(
                                    submission.conflict_account_keys
                                )
                          )
                      )
                      OR (
                          submission.submission_state = 'reconciliation_pending'
                          AND cardinality(submission.conflict_account_keys) - (
                              SELECT count(*)::INTEGER
                              FROM unnest(submission.conflict_account_keys) AS key
                              WHERE key LIKE 'fleet-shared-write-lane:%'
                                 OR key LIKE 'policy-setup-funding:%'
                          ) = (
                              SELECT count(*)::INTEGER
                              FROM loyal_yield.route_account_conflict_leases conflict
                              WHERE conflict.submission_id = submission.id
                                AND conflict.cluster = submission.cluster
                                AND conflict.writable_account_key = ANY(
                                    submission.conflict_account_keys
                                )
                                AND conflict.writable_account_key NOT LIKE
                                    'fleet-shared-write-lane:%'
                                AND conflict.writable_account_key NOT LIKE
                                    'policy-setup-funding:%'
                          )
                          AND NOT EXISTS (
                              SELECT 1
                              FROM loyal_yield.route_account_conflict_leases conflict
                              WHERE conflict.submission_id = submission.id
                                AND (
                                    conflict.writable_account_key LIKE
                                        'fleet-shared-write-lane:%'
                                    OR conflict.writable_account_key LIKE
                                        'policy-setup-funding:%'
                                )
                          )
                      )
                      OR (
                          submission.submission_state = 'effect_ambiguous'
                          AND cardinality(submission.conflict_account_keys) - (
                              SELECT count(*)::INTEGER
                              FROM unnest(submission.conflict_account_keys) AS key
                              WHERE key LIKE 'fleet-shared-write-lane:%'
                          ) = (
                              SELECT count(*)::INTEGER
                              FROM loyal_yield.route_account_conflict_leases conflict
                              WHERE conflict.submission_id = submission.id
                                AND conflict.cluster = submission.cluster
                                AND conflict.writable_account_key = ANY(
                                    submission.conflict_account_keys
                                )
                                AND conflict.writable_account_key NOT LIKE
                                    'fleet-shared-write-lane:%'
                          )
                          AND NOT EXISTS (
                              SELECT 1
                              FROM loyal_yield.route_account_conflict_leases conflict
                              WHERE conflict.submission_id = submission.id
                                AND conflict.writable_account_key LIKE
                                    'fleet-shared-write-lane:%'
                          )
                      )
                  )
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.route_account_conflict_leases conflict
                      WHERE conflict.submission_id = submission.id
                        AND (
                            conflict.cluster <> submission.cluster
                            OR NOT (
                                conflict.writable_account_key = ANY(
                                    submission.conflict_account_keys
                                )
                            )
                        )
                  )
                ORDER BY
                    opportunity.economic_priority DESC,
                    submission.created_at,
                    submission.id
                FOR UPDATE OF submission SKIP LOCKED
                LIMIT $4
            ), claimed AS (
                UPDATE loyal_yield.signed_route_submissions submission
                SET confirmation_lease_owner = $2,
                    confirmation_lease_expires_at = $3,
                    confirmation_fencing_token = submission.confirmation_fencing_token + 1,
                    confirmation_attempt_count = submission.confirmation_attempt_count + 1,
                    updated_at = now()
                FROM candidate
                WHERE submission.id = candidate.id
                RETURNING submission.*
            )
            SELECT claimed.*
            FROM claimed
            JOIN loyal_yield.rebalance_opportunities opportunity
              ON opportunity.id = claimed.opportunity_id
            ORDER BY
                opportunity.economic_priority DESC,
                claimed.created_at,
                claimed.id
            "#,
        )
        .bind(cluster)
        .bind(owner)
        .bind(lease_expires_at)
        .bind(limit)
        .fetch_all(&mut *tx)
        .await?;
        let leases = rows
            .iter()
            .map(|row| {
                let submission = signed_route_submission_from_row(row)?;
                Ok(SignedRouteSubmissionLease {
                    fencing_token: submission.confirmation_fencing_token,
                    expires_at: lease_expires_at,
                    owner: owner.to_owned(),
                    submission,
                })
            })
            .collect::<Result<Vec<_>, OrchestratorError>>()?;

        if !leases.is_empty() {
            let submission_ids = leases
                .iter()
                .map(|lease| lease.submission.id)
                .collect::<Vec<_>>();
            let expected_conflict_count = leases
                .iter()
                .map(|lease| {
                    let released = lease
                        .submission
                        .conflict_account_keys
                        .iter()
                        .filter(|key| {
                            key.starts_with("fleet-shared-write-lane:")
                                || (lease.submission.state
                                    == SignedRouteSubmissionState::ReconciliationPending
                                    && key.starts_with("policy-setup-funding:"))
                        })
                        .count();
                    let retained = if matches!(
                        lease.submission.state,
                        SignedRouteSubmissionState::ReconciliationPending
                            | SignedRouteSubmissionState::EffectAmbiguous
                    ) {
                        lease
                            .submission
                            .conflict_account_keys
                            .len()
                            .saturating_sub(released)
                    } else {
                        lease.submission.conflict_account_keys.len()
                    };
                    retained as u64
                })
                .sum::<u64>();
            let renewed_conflicts = sqlx::query(
                r#"
                WITH locked_conflict AS (
                    SELECT conflict.cluster, conflict.writable_account_key
                    FROM loyal_yield.route_account_conflict_leases conflict
                    WHERE conflict.submission_id = ANY($1)
                    ORDER BY conflict.cluster, conflict.writable_account_key
                    FOR UPDATE
                )
                UPDATE loyal_yield.route_account_conflict_leases conflict
                SET expires_at = GREATEST(
                        conflict.expires_at,
                        $2 + interval '2 minutes'
                    ),
                    updated_at = now()
                FROM locked_conflict
                WHERE conflict.cluster = locked_conflict.cluster
                  AND conflict.writable_account_key = locked_conflict.writable_account_key
                "#,
            )
            .bind(&submission_ids)
            .bind(lease_expires_at)
            .execute(&mut *tx)
            .await?;
            if renewed_conflicts.rows_affected() != expected_conflict_count {
                return Err(OrchestratorError::StoreInvariant(
                    "reconciliation claim lost the exact signed-route conflict set".to_owned(),
                ));
            }
        }

        tx.commit().await?;
        Ok(leases)
    }

    /// Applies one fenced submission transition. Every network observation is
    /// committed before the lease is released; confirmation intentionally
    /// retains the lease for the following reconciliation-pending handoff.
    pub async fn advance_signed_route_submission(
        &self,
        lease: &SignedRouteSubmissionLease,
        advance: SignedRouteSubmissionAdvance,
    ) -> Result<SignedRouteSubmissionRecord, OrchestratorError> {
        let row = match advance {
            SignedRouteSubmissionAdvance::BroadcastIntent { checked_at } => sqlx::query(
                r#"
                UPDATE loyal_yield.signed_route_submissions
                SET broadcast_count = broadcast_count + 1,
                    last_broadcast_at = $4,
                    last_status_checked_at = $4,
                    error_detail = 'broadcast_intent_persisted',
                    updated_at = now()
                WHERE id = $1
                  AND submission_state IN ('signed', 'submitted')
                  AND confirmation_lease_owner = $2
                  AND confirmation_fencing_token = $3
                  AND confirmation_lease_expires_at > now()
                RETURNING *
                "#,
            )
            .bind(lease.submission.id)
            .bind(&lease.owner)
            .bind(lease.fencing_token)
            .bind(checked_at)
            .fetch_optional(self.pool())
            .await?,
            SignedRouteSubmissionAdvance::Submitted {
                checked_at,
                observed_slot,
                next_poll_at,
                broadcasted,
            } => {
                if observed_slot.is_some_and(|slot| slot < 0) || next_poll_at < checked_at {
                    return Err(OrchestratorError::StoreInvariant(
                        "submitted observation requires a nonnegative slot and nondecreasing poll time"
                            .to_owned(),
                    ));
                }
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.signed_route_submissions
                    SET submission_state = 'submitted',
                        submitted_slot = COALESCE(submitted_slot, $5),
                        submitted_at = COALESCE(submitted_at, $4),
                        confirmation_available_at = $6,
                        confirmation_lease_owner = NULL,
                        confirmation_lease_expires_at = NULL,
                        broadcast_count = broadcast_count + CASE WHEN $7 THEN 1 ELSE 0 END,
                        last_broadcast_at = CASE WHEN $7 THEN $4 ELSE last_broadcast_at END,
                        last_status_checked_at = $4,
                        error_detail = NULL,
                        updated_at = now()
                    WHERE id = $1
                      AND submission_state IN ('signed', 'submitted')
                      AND confirmation_lease_owner = $2
                      AND confirmation_fencing_token = $3
                      AND confirmation_lease_expires_at > now()
                    RETURNING *
                    "#,
                )
                .bind(lease.submission.id)
                .bind(&lease.owner)
                .bind(lease.fencing_token)
                .bind(checked_at)
                .bind(observed_slot)
                .bind(next_poll_at)
                .bind(broadcasted)
                .fetch_optional(self.pool())
                .await?
            }
            SignedRouteSubmissionAdvance::Deferred {
                checked_at,
                next_poll_at,
                error_detail,
            } => {
                if next_poll_at < checked_at
                    || error_detail
                        .as_deref()
                        .is_some_and(|detail| detail.trim().is_empty() || detail.len() > 512)
                {
                    return Err(OrchestratorError::StoreInvariant(
                        "submission deferral requires a nondecreasing poll time and bounded error"
                            .to_owned(),
                    ));
                }
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.signed_route_submissions
                    SET confirmation_available_at = $5,
                        confirmation_lease_owner = NULL,
                        confirmation_lease_expires_at = NULL,
                        last_status_checked_at = $4,
                        error_detail = $6,
                        updated_at = now()
                    WHERE id = $1
                      AND submission_state IN (
                          'signed', 'submitted', 'reconciliation_pending',
                          'expiry_check_pending', 'effect_ambiguous'
                      )
                      AND confirmation_lease_owner = $2
                      AND confirmation_fencing_token = $3
                      AND confirmation_lease_expires_at > now()
                    RETURNING *
                    "#,
                )
                .bind(lease.submission.id)
                .bind(&lease.owner)
                .bind(lease.fencing_token)
                .bind(checked_at)
                .bind(next_poll_at)
                .bind(error_detail)
                .fetch_optional(self.pool())
                .await?
            }
            SignedRouteSubmissionAdvance::Confirmed {
                checked_at,
                confirmed_slot,
            } => {
                if confirmed_slot < 0 {
                    return Err(OrchestratorError::StoreInvariant(
                        "confirmed submission requires a nonnegative signature status slot"
                            .to_owned(),
                    ));
                }
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.signed_route_submissions
                    SET submission_state = 'confirmed',
                        submitted_slot = COALESCE(submitted_slot, $5),
                        submitted_at = COALESCE(submitted_at, last_broadcast_at, $4),
                        confirmed_slot = COALESCE(confirmed_slot, $5),
                        confirmed_at = COALESCE(confirmed_at, $4),
                        last_status_checked_at = $4,
                        error_detail = NULL,
                        updated_at = now()
                    WHERE id = $1
                      AND submission_state IN (
                          'signed', 'submitted', 'confirmed', 'expiry_check_pending',
                          'effect_ambiguous'
                      )
                      AND (confirmed_slot IS NULL OR confirmed_slot = $5)
                      AND confirmation_lease_owner = $2
                      AND confirmation_fencing_token = $3
                      AND confirmation_lease_expires_at > now()
                    RETURNING *
                    "#,
                )
                .bind(lease.submission.id)
                .bind(&lease.owner)
                .bind(lease.fencing_token)
                .bind(checked_at)
                .bind(confirmed_slot)
                .fetch_optional(self.pool())
                .await?
            }
            SignedRouteSubmissionAdvance::ReconciliationPending => sqlx::query(
                r#"
                WITH pending AS (
                    UPDATE loyal_yield.signed_route_submissions
                    SET submission_state = 'reconciliation_pending',
                        confirmation_lease_owner = NULL,
                        confirmation_lease_expires_at = NULL,
                        error_detail = NULL,
                        updated_at = now()
                    WHERE id = $1
                      AND submission_state = 'confirmed'
                      AND confirmation_lease_owner = $2
                      AND confirmation_fencing_token = $3
                      AND confirmation_lease_expires_at > now()
                    RETURNING *
                ), released_transient_conflicts AS (
                    DELETE FROM loyal_yield.route_account_conflict_leases conflict
                    USING pending
                    WHERE conflict.submission_id = pending.id
                      AND (
                          conflict.writable_account_key LIKE
                              'fleet-shared-write-lane:%'
                          OR conflict.writable_account_key LIKE
                              'policy-setup-funding:%'
                      )
                    RETURNING conflict.writable_account_key
                ), released_alt AS (
                    UPDATE loyal_yield.lookup_table_usage_leases usage
                    SET released_at = COALESCE(usage.released_at, now()),
                        updated_at = now()
                    FROM pending
                    WHERE usage.lease_kind = 'prepared_transaction'
                      AND usage.reference_key = pending.semantic_key
                    RETURNING usage.id
                )
                SELECT * FROM pending
                "#,
            )
            .bind(lease.submission.id)
            .bind(&lease.owner)
            .bind(lease.fencing_token)
            .fetch_optional(self.pool())
            .await?,
            SignedRouteSubmissionAdvance::ExpiryCheckPending {
                checked_at,
                observed_block_height,
                effect_check_slot,
            } => {
                if observed_block_height < 0 || effect_check_slot < 0 {
                    return Err(OrchestratorError::StoreInvariant(
                        "expiry effect check requires nonnegative finalized height and slot"
                            .to_owned(),
                    ));
                }
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.signed_route_submissions
                    SET submission_state = 'expiry_check_pending',
                        expiry_observed_block_height = $5,
                        effect_check_slot = $6,
                        confirmation_available_at = $4,
                        confirmation_lease_owner = NULL,
                        confirmation_lease_expires_at = NULL,
                        last_status_checked_at = $4,
                        error_detail = concat(
                            'blockhash_expired_at_height_', $5,
                            '_awaiting_effect_absence_proof'
                        ),
                        updated_at = now()
                    WHERE id = $1
                      AND submission_state IN ('signed', 'submitted')
                      AND broadcast_count > 0
                      AND confirmation_lease_owner = $2
                      AND confirmation_fencing_token = $3
                      AND confirmation_lease_expires_at > now()
                      AND last_valid_block_height < $5
                    RETURNING *
                    "#,
                )
                .bind(lease.submission.id)
                .bind(&lease.owner)
                .bind(lease.fencing_token)
                .bind(checked_at)
                .bind(observed_block_height)
                .bind(effect_check_slot)
                .fetch_optional(self.pool())
                .await?
            }
            SignedRouteSubmissionAdvance::EffectAmbiguous {
                checked_at,
                error_detail,
            } => {
                if error_detail.trim().is_empty() || error_detail.len() > 512 {
                    return Err(OrchestratorError::StoreInvariant(
                        "ambiguous route effect requires a bounded nonempty error".to_owned(),
                    ));
                }
                sqlx::query(
                    r#"
                    WITH quarantined AS (
                        UPDATE loyal_yield.signed_route_submissions
                        SET submission_state = 'effect_ambiguous',
                            confirmation_lease_owner = NULL,
                            confirmation_lease_expires_at = NULL,
                            last_status_checked_at = $4,
                            error_detail = $5,
                            updated_at = now()
                        WHERE id = $1
                          AND submission_state = 'expiry_check_pending'
                          AND confirmation_lease_owner = $2
                          AND confirmation_fencing_token = $3
                          AND confirmation_lease_expires_at > now()
                        RETURNING *
                    ), released_shared_lane AS (
                        DELETE FROM loyal_yield.route_account_conflict_leases conflict
                        USING quarantined
                        WHERE conflict.submission_id = quarantined.id
                          AND conflict.writable_account_key LIKE 'fleet-shared-write-lane:%'
                        RETURNING conflict.writable_account_key
                    )
                    SELECT quarantined.* FROM quarantined
                    "#,
                )
                .bind(lease.submission.id)
                .bind(&lease.owner)
                .bind(lease.fencing_token)
                .bind(checked_at)
                .bind(error_detail)
                .fetch_optional(self.pool())
                .await?
            }
            SignedRouteSubmissionAdvance::Reconciled { reconciled_slot } => {
                let confirmed_slot = lease.submission.confirmed_slot.ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "reconciled submission requires a confirmed signature slot".to_owned(),
                    )
                })?;
                if confirmed_slot < 0 || reconciled_slot < confirmed_slot {
                    return Err(OrchestratorError::StoreInvariant(
                        "reconciled submission slot must be at or after its confirmed slot"
                            .to_owned(),
                    ));
                }
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.signed_route_submissions
                    SET submission_state = 'reconciled',
                        reconciled_slot = $4,
                        reconciled_at = now(),
                        confirmation_lease_owner = NULL,
                        confirmation_lease_expires_at = NULL,
                        error_detail = NULL,
                        updated_at = now()
                    WHERE id = $1
                      AND submission_state = 'reconciliation_pending'
                      AND confirmed_slot IS NOT NULL
                      AND $4 >= confirmed_slot
                      AND confirmation_lease_owner = $2
                      AND confirmation_fencing_token = $3
                      AND confirmation_lease_expires_at > now()
                    RETURNING *
                    "#,
                )
                .bind(lease.submission.id)
                .bind(&lease.owner)
                .bind(lease.fencing_token)
                .bind(reconciled_slot)
                .fetch_optional(self.pool())
                .await?
            }
            SignedRouteSubmissionAdvance::Expired {
                checked_at,
                observed_block_height,
                signature_history_absent,
                effect_absence_proved,
            } => {
                if observed_block_height < 0 || !signature_history_absent {
                    return Err(OrchestratorError::StoreInvariant(
                        "expired submission requires history-absent signature evidence and a nonnegative observed block height".to_owned(),
                    ));
                }
                let detail = format!("blockhash_expired_at_height_{observed_block_height}");
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.signed_route_submissions
                    SET submission_state = 'expired',
                        confirmation_lease_owner = NULL,
                        confirmation_lease_expires_at = NULL,
                        last_status_checked_at = $4,
                        error_detail = $5,
                        updated_at = now()
                    WHERE id = $1
                      AND (
                          (
                              submission_state IN ('signed', 'submitted')
                              AND broadcast_count = 0
                          )
                          OR (
                              submission_state IN (
                                  'expiry_check_pending', 'effect_ambiguous'
                              )
                              AND broadcast_count > 0
                              AND $7
                              AND expiry_observed_block_height = $6
                              AND effect_check_slot IS NOT NULL
                          )
                      )
                      AND confirmation_lease_owner = $2
                      AND confirmation_fencing_token = $3
                      AND confirmation_lease_expires_at > now()
                      AND last_valid_block_height < $6
                    RETURNING *
                    "#,
                )
                .bind(lease.submission.id)
                .bind(&lease.owner)
                .bind(lease.fencing_token)
                .bind(checked_at)
                .bind(detail)
                .bind(observed_block_height)
                .bind(effect_absence_proved)
                .fetch_optional(self.pool())
                .await?
            }
            SignedRouteSubmissionAdvance::Failed {
                checked_at,
                confirmed_slot,
                error_detail,
            } => {
                if confirmed_slot.is_some_and(|slot| slot < 0)
                    || error_detail.trim().is_empty()
                    || error_detail.len() > 512
                {
                    return Err(OrchestratorError::StoreInvariant(
                        "failed submission requires a bounded nonempty error".to_owned(),
                    ));
                }
                sqlx::query(
                    r#"
                    UPDATE loyal_yield.signed_route_submissions
                    SET submission_state = 'failed',
                        submitted_slot = CASE
                            WHEN $6 IS NOT NULL THEN COALESCE(submitted_slot, $6)
                            ELSE submitted_slot
                        END,
                        submitted_at = CASE
                            WHEN $6 IS NOT NULL THEN COALESCE(
                                submitted_at,
                                last_broadcast_at,
                                $4
                            )
                            ELSE submitted_at
                        END,
                        confirmed_slot = COALESCE(confirmed_slot, $6),
                        confirmed_at = CASE
                            WHEN $6 IS NOT NULL THEN COALESCE(confirmed_at, $4)
                            ELSE confirmed_at
                        END,
                        confirmation_lease_owner = NULL,
                        confirmation_lease_expires_at = NULL,
                        last_status_checked_at = $4,
                        error_detail = $5,
                        updated_at = now()
                    WHERE id = $1
                      AND submission_state IN (
                          'signed', 'submitted', 'expiry_check_pending',
                          'effect_ambiguous'
                      )
                      AND confirmation_lease_owner = $2
                      AND confirmation_fencing_token = $3
                      AND confirmation_lease_expires_at > now()
                      AND (confirmed_slot IS NULL OR $6 IS NULL OR confirmed_slot = $6)
                    RETURNING *
                    "#,
                )
                .bind(lease.submission.id)
                .bind(&lease.owner)
                .bind(lease.fencing_token)
                .bind(checked_at)
                .bind(error_detail)
                .bind(confirmed_slot)
                .fetch_optional(self.pool())
                .await?
            }
        }
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "signed submission {} transition is stale, expired, or fenced",
                lease.submission.id
            ))
        })?;
        signed_route_submission_from_row(&row)
    }

    /// Claims the highest-value eligible item without blocking other workers.
    /// Expired leases are recoverable only by a worker in the same claim lane.
    pub async fn lease_next_rebalance_opportunity(
        &self,
        cluster: &str,
        owner: &str,
        claim_kind: RebalanceOpportunityClaimKind,
        lease_expires_at: DateTime<Utc>,
    ) -> Result<Option<RebalanceOpportunityLease>, OrchestratorError> {
        Ok(self
            .lease_rebalance_opportunity_batch(cluster, owner, claim_kind, 1, lease_expires_at)
            .await?
            .into_iter()
            .next())
    }

    /// Claims a bounded priority wave with one database round trip. Queue age
    /// contributes one priority unit per second, so waiting work eventually
    /// progresses without categorically placing an old low-value backlog ahead
    /// of newly discovered high-yield deposits.
    pub async fn lease_rebalance_opportunity_batch(
        &self,
        cluster: &str,
        owner: &str,
        claim_kind: RebalanceOpportunityClaimKind,
        limit: i64,
        lease_expires_at: DateTime<Utc>,
    ) -> Result<Vec<RebalanceOpportunityLease>, OrchestratorError> {
        Ok(self
            .lease_rebalance_opportunity_batch_measured(
                cluster,
                owner,
                claim_kind,
                limit,
                lease_expires_at,
            )
            .await?
            .0)
    }

    /// Executes the exact production claim statement while exposing its
    /// server-side statement latency to the isolated performance verifier.
    /// Normal workers use `lease_rebalance_opportunity_batch` and discard this
    /// diagnostic; no extra query or timing-only transaction is introduced.
    #[doc(hidden)]
    pub async fn lease_rebalance_opportunity_batch_measured(
        &self,
        cluster: &str,
        owner: &str,
        claim_kind: RebalanceOpportunityClaimKind,
        limit: i64,
        lease_expires_at: DateTime<Utc>,
    ) -> Result<(Vec<RebalanceOpportunityLease>, u64), OrchestratorError> {
        if cluster.trim().is_empty()
            || owner.trim().is_empty()
            || !(1..=256).contains(&limit)
            || lease_expires_at <= Utc::now()
        {
            return Err(OrchestratorError::StoreInvariant(
                "rebalance opportunity batch lease requires cluster, owner, limit in 1..=256, and a future expiry".to_owned(),
            ));
        }
        let active_statuses = ACTIVE_DECISION_STATUSES
            .iter()
            .map(|status| (*status).to_owned())
            .collect::<Vec<_>>();
        // The deferred migration-29 commit trigger requires sixty seconds of
        // reciprocal opportunity/epoch lifetime when a row becomes leased.
        // Keep a small statement-to-commit cushion so near-expiry work is
        // skipped by the claimant instead of aborting the whole worker.
        let minimum_claim_lifetime_seconds = i32::try_from(
            MINIMUM_USABLE_MARKET_EPOCH_LIFETIME_SECONDS
                .checked_add(5)
                .ok_or_else(|| {
                    OrchestratorError::StoreInvariant(
                        "minimum claim lifetime overflowed".to_owned(),
                    )
                })?,
        )
        .map_err(|_| {
            OrchestratorError::StoreInvariant(
                "minimum claim lifetime does not fit PostgreSQL INTEGER".to_owned(),
            )
        })?;
        // Keep the runnable state literal in the statement so PostgreSQL can
        // prove this query is covered by the partial ready-priority index.
        // A bind parameter here can force a generic plan that scans ALT-cold
        // rows even though they are not claimable in this lane.
        let runnable_state_predicate = match claim_kind {
            RebalanceOpportunityClaimKind::Execute => "opportunity.opportunity_state = 'ready'",
            RebalanceOpportunityClaimKind::Revalidate => {
                "opportunity.opportunity_state = 'revalidate'"
            }
        };
        // Runnable work and expired crash recovery have different selective
        // indexes. Merge their ordered streams before taking row locks, so
        // SKIP LOCKED can continue into either lane without locking candidates
        // the batch will not claim. This keeps active, unexpired leases off the
        // hot runnable scan while preserving global order and eventual
        // recovery: an older row retains its scheduler-anchor age advantage
        // over newer work exactly as it did in the single-lane query.
        let claim_sql = format!(
            r#"
            WITH ranked_candidate AS NOT MATERIALIZED ((
                SELECT opportunity.id,
                       opportunity.scheduler_priority_anchor,
                       opportunity.economic_priority,
                       opportunity.created_at
                FROM loyal_yield.rebalance_opportunities opportunity
                WHERE opportunity.cluster = $1
                  AND ({runnable_state_predicate})
                  AND opportunity.available_at <= now()
                  AND opportunity.expires_at >= clock_timestamp()
                      + make_interval(secs => $7::INTEGER)
                  AND EXISTS (
                      SELECT 1
                      FROM loyal_yield.optimizer_epochs epoch
                      WHERE epoch.id = opportunity.optimizer_epoch_id
                        AND epoch.cluster = opportunity.cluster
                        AND epoch.expires_at >= clock_timestamp()
                            + make_interval(secs => $7::INTEGER)
                  )
                ORDER BY
                    opportunity.scheduler_priority_anchor DESC,
                    opportunity.economic_priority DESC,
                    opportunity.created_at,
                    opportunity.id
            ) UNION ALL (
                SELECT opportunity.id,
                       opportunity.scheduler_priority_anchor,
                       opportunity.economic_priority,
                       opportunity.created_at
                FROM loyal_yield.rebalance_opportunities opportunity
                WHERE opportunity.cluster = $1
                  AND opportunity.opportunity_state = 'leased'
                  AND opportunity.lease_kind = $4
                  AND opportunity.lease_expires_at <= now()
                  AND opportunity.available_at <= now()
                  AND opportunity.expires_at >= clock_timestamp()
                      + make_interval(secs => $7::INTEGER)
                  AND EXISTS (
                      SELECT 1
                      FROM loyal_yield.optimizer_epochs epoch
                      WHERE epoch.id = opportunity.optimizer_epoch_id
                        AND epoch.cluster = opportunity.cluster
                        AND epoch.expires_at >= clock_timestamp()
                            + make_interval(secs => $7::INTEGER)
                  )
                ORDER BY
                    opportunity.scheduler_priority_anchor DESC,
                    opportunity.economic_priority DESC,
                    opportunity.created_at,
                    opportunity.id
            )), candidate AS (
                SELECT opportunity.id
                FROM ranked_candidate ranked
                JOIN loyal_yield.rebalance_opportunities opportunity
                  ON opportunity.id = ranked.id
                -- Keep vault/policy eligibility as parameterized primary-key
                -- probes. Without the non-flattenable LATERAL boundary,
                -- PostgreSQL can choose a full managed_vaults hash scan when
                -- this cluster has a large waiting_alt population, even
                -- though the ranked lane predicates are selective.
                JOIN LATERAL (
                    SELECT vault.active_policy_id
                    FROM loyal_yield.managed_vaults vault
                    WHERE vault.id = opportunity.vault_id
                      AND vault.active
                    OFFSET 0
                ) vault ON TRUE
                JOIN LATERAL (
                    SELECT policy.id
                    FROM loyal_yield.route_policies policy
                    WHERE policy.id = vault.active_policy_id
                      AND policy.active
                    OFFSET 0
                ) policy ON TRUE
                WHERE opportunity.cluster = $1
                  AND (
                      ({runnable_state_predicate})
                      OR (
                          opportunity.opportunity_state = 'leased'
                          AND opportunity.lease_kind = $4
                          AND opportunity.lease_expires_at <= now()
                      )
                  )
                  AND opportunity.available_at <= now()
                  AND opportunity.expires_at >= clock_timestamp()
                      + make_interval(secs => $7::INTEGER)
                  AND EXISTS (
                      SELECT 1
                      FROM loyal_yield.optimizer_epochs epoch
                      WHERE epoch.id = opportunity.optimizer_epoch_id
                        AND epoch.cluster = opportunity.cluster
                        AND epoch.expires_at >= clock_timestamp()
                            + make_interval(secs => $7::INTEGER)
                  )
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.rebalance_decisions decision
                      WHERE decision.vault_id = opportunity.vault_id
                        AND decision.status::text = ANY($5)
                  )
                  AND NOT EXISTS (
                      SELECT 1
                      FROM loyal_yield.signed_route_submissions submission
                      WHERE submission.opportunity_id = opportunity.id
                        AND submission.submission_state NOT IN ('reconciled', 'expired', 'failed')
                  )
                ORDER BY
                    ranked.scheduler_priority_anchor DESC,
                    ranked.economic_priority DESC,
                    ranked.created_at,
                    ranked.id
                FOR UPDATE OF opportunity SKIP LOCKED
                LIMIT $6
            ), claimed AS (
                UPDATE loyal_yield.rebalance_opportunities opportunity
                SET opportunity_state = 'leased',
                    lease_kind = $4,
                    lease_owner = $2,
                    lease_expires_at = $3,
                    fencing_token = opportunity.fencing_token + 1,
                    attempt_count = opportunity.attempt_count + 1,
                    updated_at = now()
                FROM candidate
                WHERE opportunity.id = candidate.id
                RETURNING opportunity.*
            )
            SELECT claimed.*,
                   GREATEST(
                       floor(EXTRACT(EPOCH FROM (
                           clock_timestamp() - statement_timestamp()
                       )) * 1000000),
                       0
                   )::BIGINT AS claim_server_elapsed_micros
            FROM claimed
            ORDER BY
                scheduler_priority_anchor DESC,
                economic_priority DESC,
                created_at,
                id
            "#
        );
        // Each claim lane has its own literal state predicate in the SQL text.
        // Both custom and generic plans retain the runnable/expired partial
        // indexes and Merge Append, so allow SQLx/PostgreSQL to cache the two
        // hot statements instead of paying parse/plan cost on every batch.
        let rows = sqlx::query(&claim_sql)
            .bind(cluster)
            .bind(owner)
            .bind(lease_expires_at)
            .bind(claim_kind.as_str())
            .bind(&active_statuses)
            .bind(limit)
            .bind(minimum_claim_lifetime_seconds)
            .fetch_all(self.pool())
            .await?;
        let server_elapsed_micros = rows
            .iter()
            .map(|row| row.try_get::<i64, _>("claim_server_elapsed_micros"))
            .collect::<Result<Vec<_>, _>>()?
            .into_iter()
            .max()
            .unwrap_or_default();
        let leases = rows
            .iter()
            .map(|row| {
                let opportunity = rebalance_opportunity_from_row(row)?;
                Ok::<RebalanceOpportunityLease, OrchestratorError>(RebalanceOpportunityLease {
                    fencing_token: opportunity.fencing_token,
                    expires_at: lease_expires_at,
                    owner: owner.to_owned(),
                    claim_kind,
                    opportunity,
                })
            })
            .collect::<Result<Vec<_>, _>>()?;
        Ok((
            leases,
            u64::try_from(server_elapsed_micros).unwrap_or_default(),
        ))
    }

    /// Converts a still-current revalidation lease into an execute lease
    /// without putting the opportunity back through the durable ready queue.
    ///
    /// This is deliberately a try-operation: callers may use an already-built
    /// route only when they also hold an immediately available local execution
    /// permit and can atomically acquire the complete semantic conflict set.
    /// A conflict returns `Ok(None)` after rolling the transaction back, so the
    /// original revalidation fence remains valid and can publish normal durable
    /// `ready` state. No transaction bytes are accepted here; signing remains
    /// behind the resulting execute lease and its normal final fences.
    pub async fn try_promote_revalidation_lease_to_execute(
        &self,
        lease: &RebalanceOpportunityLease,
        route_fingerprint: &str,
        requirements_fingerprint: &str,
        execution_plan: &Value,
        conflict_account_keys: &[String],
    ) -> Result<Option<RebalanceOpportunityLease>, OrchestratorError> {
        if lease.claim_kind != RebalanceOpportunityClaimKind::Revalidate
            || route_fingerprint.trim().is_empty()
            || requirements_fingerprint.trim().is_empty()
            || !execution_plan.is_object()
        {
            return Err(OrchestratorError::StoreInvariant(
                "fused execution promotion requires a revalidation lease and complete exact route evidence"
                    .to_owned(),
            ));
        }
        let conflict_account_keys = canonical_conflict_account_keys(conflict_account_keys)?;
        let plan_conflict_account_keys = execution_plan
            .get("conflict_account_keys")
            .and_then(Value::as_array)
            .map(|keys| {
                keys.iter()
                    .filter_map(Value::as_str)
                    .map(ToOwned::to_owned)
                    .collect::<Vec<_>>()
            })
            .unwrap_or_default();
        if canonical_conflict_account_keys(&plan_conflict_account_keys)? != conflict_account_keys {
            return Err(OrchestratorError::StoreInvariant(
                "fused execution promotion conflict evidence differs from its durable execution plan"
                    .to_owned(),
            ));
        }

        let active_statuses = ACTIVE_DECISION_STATUSES
            .iter()
            .map(|status| (*status).to_owned())
            .collect::<Vec<_>>();
        let mut tx = self.pool().begin().await?;
        let row = sqlx::query(
            r#"
            SELECT opportunity.*
            FROM loyal_yield.rebalance_opportunities opportunity
            JOIN loyal_yield.optimizer_epochs epoch
              ON epoch.id = opportunity.optimizer_epoch_id
             AND epoch.cluster = opportunity.cluster
            WHERE opportunity.id = $1
              AND opportunity.opportunity_state = 'leased'
              AND opportunity.lease_kind = 'revalidate'
              AND opportunity.lease_owner = $2
              AND opportunity.fencing_token = $3
              AND opportunity.lease_expires_at > clock_timestamp()
              AND opportunity.expires_at > clock_timestamp()
              AND epoch.expires_at > clock_timestamp()
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.rebalance_decisions decision
                  WHERE decision.vault_id = opportunity.vault_id
                    AND decision.status::text = ANY($4)
              )
            FOR UPDATE OF opportunity
            "#,
        )
        .bind(lease.opportunity.id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(&active_statuses)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity {} fused promotion is stale, expired, fenced, or blocked by an active decision",
                lease.opportunity.id
            ))
        })?;
        let current = rebalance_opportunity_from_row(&row)?;
        if current.cluster != lease.opportunity.cluster
            || current.vault_id != lease.opportunity.vault_id
            || current.optimizer_epoch_id != lease.opportunity.optimizer_epoch_id
        {
            return Err(OrchestratorError::StoreInvariant(
                "fused execution promotion durable identity changed while leased".to_owned(),
            ));
        }
        let execute_fencing_token = current.fencing_token.checked_add(1).ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "rebalance opportunity fencing token overflowed".to_owned(),
            )
        })?;

        for conflict_account_key in &conflict_account_keys {
            let acquired = sqlx::query_scalar::<_, String>(
                r#"
                INSERT INTO loyal_yield.route_account_conflict_leases AS conflict
                    (cluster, writable_account_key, opportunity_id, lease_owner,
                     fencing_token, expires_at)
                VALUES ($1, $2, $3, $4, $5, $6)
                ON CONFLICT (cluster, writable_account_key) DO UPDATE
                SET opportunity_id = EXCLUDED.opportunity_id,
                    lease_owner = EXCLUDED.lease_owner,
                    fencing_token = EXCLUDED.fencing_token,
                    expires_at = EXCLUDED.expires_at,
                    submission_id = NULL,
                    updated_at = now()
                WHERE conflict.submission_id IS NULL
                  AND conflict.expires_at <= now()
                RETURNING writable_account_key
                "#,
            )
            .bind(&current.cluster)
            .bind(conflict_account_key)
            .bind(current.id)
            .bind(&lease.owner)
            .bind(execute_fencing_token)
            .bind(lease.expires_at)
            .fetch_optional(&mut *tx)
            .await?;
            if acquired.is_none() {
                tx.rollback().await?;
                return Ok(None);
            }
        }

        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_opportunities
            SET lease_kind = 'execute',
                fencing_token = $5,
                attempt_count = attempt_count + 1,
                route_fingerprint = $6,
                requirements_fingerprint = $7,
                execution_plan = $8,
                terminal_reason = NULL,
                updated_at = now()
            WHERE id = $1
              AND opportunity_state = 'leased'
              AND lease_kind = 'revalidate'
              AND lease_owner = $2
              AND fencing_token = $3
              AND lease_expires_at = $4
              AND lease_expires_at > clock_timestamp()
            RETURNING *
            "#,
        )
        .bind(current.id)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(lease.expires_at)
        .bind(execute_fencing_token)
        .bind(route_fingerprint)
        .bind(requirements_fingerprint)
        .bind(execution_plan)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity {} changed during fused execution promotion",
                current.id
            ))
        })?;
        let opportunity = rebalance_opportunity_from_row(&row)?;
        tx.commit().await?;
        Ok(Some(RebalanceOpportunityLease {
            opportunity,
            claim_kind: RebalanceOpportunityClaimKind::Execute,
            owner: lease.owner.clone(),
            fencing_token: execute_fencing_token,
            expires_at: lease.expires_at,
        }))
    }

    pub async fn advance_rebalance_opportunity(
        &self,
        opportunity_id: i64,
        lease: &RebalanceOpportunityLease,
        advance: RebalanceOpportunityAdvance,
    ) -> Result<RebalanceOpportunityRecord, OrchestratorError> {
        validate_opportunity_advance(lease.claim_kind, &advance)?;
        let release_unattached_conflicts = lease.claim_kind
            == RebalanceOpportunityClaimKind::Execute
            && advance.next_state != RebalanceOpportunityState::DecisionCreated;
        let mut tx = self.pool().begin().await?;
        let existing_request_id: Option<i64> = sqlx::query_scalar(
            r#"
            SELECT provisioning_request_id
            FROM loyal_yield.lookup_table_provisioning_request_consumers
            WHERE opportunity_id = $1
            "#,
        )
        .bind(opportunity_id)
        .fetch_optional(&mut *tx)
        .await?;
        let mut request_ids = BTreeSet::new();
        if let Some(request_id) = existing_request_id {
            request_ids.insert(request_id);
        }
        if let Some(request_id) = advance.provisioning_request_id {
            request_ids.insert(request_id);
        }
        if !request_ids.is_empty() {
            let request_ids = request_ids.into_iter().collect::<Vec<_>>();
            let locked_request_ids = sqlx::query_scalar::<_, i64>(
                r#"
                SELECT id
                FROM loyal_yield.lookup_table_provisioning_requests
                WHERE id = ANY($1)
                ORDER BY id
                FOR UPDATE
                "#,
            )
            .bind(&request_ids)
            .fetch_all(&mut *tx)
            .await?;
            if locked_request_ids != request_ids {
                return Err(OrchestratorError::StoreInvariant(
                    "rebalance opportunity references a missing ALT request".to_owned(),
                ));
            }
        }
        let current = sqlx::query(
            r#"
            SELECT * FROM loyal_yield.rebalance_opportunities
            WHERE id = $1 AND opportunity_state = 'leased'
              AND lease_kind = $2 AND lease_owner = $3 AND fencing_token = $4
              AND lease_expires_at > now()
            FOR UPDATE
            "#,
        )
        .bind(opportunity_id)
        .bind(lease.claim_kind.as_str())
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity {opportunity_id} lease is stale, expired, or fenced"
            ))
        })?;
        let current = rebalance_opportunity_from_row(&current)?;

        let route_fingerprint = advance
            .route_fingerprint
            .as_deref()
            .or(current.route_fingerprint.as_deref());
        let requirements_fingerprint = advance
            .requirements_fingerprint
            .as_deref()
            .or(current.requirements_fingerprint.as_deref());
        if matches!(
            advance.next_state,
            RebalanceOpportunityState::WaitingAlt | RebalanceOpportunityState::Ready
        ) && (route_fingerprint.is_none_or(str::is_empty)
            || requirements_fingerprint.is_none_or(str::is_empty))
        {
            return Err(OrchestratorError::StoreInvariant(
                "revalidation must persist exact route and requirements fingerprints before execution or ALT wait"
                    .to_owned(),
            ));
        }
        if matches!(
            advance.next_state,
            RebalanceOpportunityState::WaitingAlt | RebalanceOpportunityState::Ready
        ) && advance
            .execution_plan
            .as_ref()
            .is_none_or(|plan| !plan.is_object())
        {
            return Err(OrchestratorError::StoreInvariant(
                "revalidation must persist exact object execution evidence".to_owned(),
            ));
        }

        if advance.next_state == RebalanceOpportunityState::WaitingAlt {
            let request_id = advance.provisioning_request_id.ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "waiting_alt transition requires the exact provisioning request".to_owned(),
                )
            })?;
            let request = sqlx::query(
                r#"
                SELECT cluster, vault_id, requirements_fingerprint, sealed_at, request_status
                FROM loyal_yield.lookup_table_provisioning_requests
                WHERE id = $1
                FOR SHARE
                "#,
            )
            .bind(request_id)
            .fetch_optional(&mut *tx)
            .await?
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(format!(
                    "lookup-table provisioning request {request_id} does not exist"
                ))
            })?;
            if request.try_get::<String, _>("cluster")? != current.cluster
                || request.try_get::<i64, _>("vault_id")? != current.vault_id.as_i64()
                || request.try_get::<String, _>("requirements_fingerprint")?
                    != requirements_fingerprint.expect("validated above")
                || request
                    .try_get::<Option<DateTime<Utc>>, _>("sealed_at")?
                    .is_none()
                || request.try_get::<String, _>("request_status")? == "satisfied"
            {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "rebalance opportunity {opportunity_id} cannot attach a mismatched, unsealed, or already-satisfied ALT request"
                )));
            }
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.lookup_table_provisioning_request_consumers
                    (opportunity_id, provisioning_request_id)
                VALUES ($1, $2)
                ON CONFLICT (opportunity_id) DO UPDATE
                SET provisioning_request_id = EXCLUDED.provisioning_request_id
                "#,
            )
            .bind(opportunity_id)
            .bind(request_id)
            .execute(&mut *tx)
            .await?;
        } else if advance.provisioning_request_id.is_some() {
            return Err(OrchestratorError::StoreInvariant(
                "only waiting_alt transition may attach a provisioning request".to_owned(),
            ));
        }
        if let Some(decision_id) = advance.decision_id {
            let decision_matches: bool = sqlx::query_scalar(
                "SELECT EXISTS (SELECT 1 FROM loyal_yield.rebalance_decisions WHERE id = $1 AND vault_id = $2)",
            )
            .bind(decision_id.as_i64())
            .bind(current.vault_id.as_i64())
            .fetch_one(&mut *tx)
            .await?;
            if !decision_matches {
                return Err(OrchestratorError::StoreInvariant(format!(
                    "decision {decision_id} does not belong to opportunity vault {}",
                    current.vault_id
                )));
            }
        }

        let available_at = advance.available_at.unwrap_or_else(Utc::now);
        if available_at >= current.expires_at {
            return Err(OrchestratorError::StoreInvariant(
                "rebalance opportunity retry cannot start at or after expiry".to_owned(),
            ));
        }
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.rebalance_opportunities
            SET opportunity_state = $5,
                available_at = $6,
                lease_kind = NULL,
                lease_owner = NULL,
                lease_expires_at = NULL,
                decision_id = $7,
                terminal_reason = $8,
                route_fingerprint = COALESCE($9, route_fingerprint),
                requirements_fingerprint = COALESCE($10, requirements_fingerprint),
                execution_plan = COALESCE($11, execution_plan),
                updated_at = now()
            WHERE id = $1 AND opportunity_state = 'leased'
              AND lease_kind = $2 AND lease_owner = $3 AND fencing_token = $4
              AND lease_expires_at > now()
            RETURNING *
            "#,
        )
        .bind(opportunity_id)
        .bind(lease.claim_kind.as_str())
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(advance.next_state.as_str())
        .bind(available_at)
        .bind(advance.decision_id.map(DecisionId::as_i64))
        .bind(advance.reason)
        .bind(advance.route_fingerprint)
        .bind(advance.requirements_fingerprint)
        .bind(advance.execution_plan)
        .fetch_optional(&mut *tx)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "rebalance opportunity {opportunity_id} lost fencing while advancing"
            ))
        })?;
        let opportunity = rebalance_opportunity_from_row(&row)?;
        if release_unattached_conflicts {
            sqlx::query(
                r#"
                DELETE FROM loyal_yield.route_account_conflict_leases
                WHERE opportunity_id = $1
                  AND lease_owner = $2
                  AND fencing_token = $3
                  AND submission_id IS NULL
                "#,
            )
            .bind(opportunity_id)
            .bind(&lease.owner)
            .bind(lease.fencing_token)
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;
        Ok(opportunity)
    }
}

fn is_active_opportunity_slot_conflict(error: &sqlx::Error) -> bool {
    let sqlx::Error::Database(database_error) = error else {
        return false;
    };
    database_error.code().as_deref() == Some("23505")
        && database_error.constraint() == Some("active_rebalance_opportunity_slots_pkey")
}

pub fn rebalance_opportunity_idempotency_key(input: &RebalanceOpportunityInput) -> String {
    let mut hasher = Sha256::new();
    for value in [
        "loyal-rebalance-opportunity-v1".to_owned(),
        input.cluster.clone(),
        input.vault_id.as_i64().to_string(),
        input
            .source_snapshot_id
            .map(SnapshotId::as_i64)
            .map_or_else(|| "idle".to_owned(), |id| id.to_string()),
        input.optimizer_epoch_id.to_string(),
        input.route_fingerprint.clone().unwrap_or_default(),
        input.requirements_fingerprint.clone().unwrap_or_default(),
        input.source_reserve.clone().unwrap_or_default(),
        input.target_reserve.clone(),
        input.liquidity_mint.clone(),
        input.amount_raw.to_string(),
        input.principal_usd_micros.to_string(),
        input.source_apy_bps.to_string(),
        input.target_apy_bps.to_string(),
        input.estimated_edge_bps.to_string(),
        input.estimated_cost_lamports.to_string(),
        input.annual_yield_gain_usd_micros.to_string(),
        input.expected_net_gain_usd_micros.to_string(),
        input.economic_priority.to_string(),
        input.priority_version.clone(),
        serde_json::to_string(&input.execution_plan)
            .expect("validated JSON opportunity evidence must serialize"),
        input.expires_at.to_rfc3339(),
        input
            .provisioning_request_id
            .map_or_else(String::new, |id| id.to_string()),
    ] {
        let bytes = value.as_bytes();
        hasher.update((bytes.len() as u64).to_le_bytes());
        hasher.update(bytes);
    }
    format!("{:x}", hasher.finalize())
}

pub fn rebalance_opportunity_attempt_idempotency_key(
    rediscovery_key: &str,
    attempt_generation: i64,
) -> Result<String, OrchestratorError> {
    if rediscovery_key.trim().is_empty() || attempt_generation <= 0 {
        return Err(OrchestratorError::StoreInvariant(
            "rebalance opportunity retry identity requires a key and positive generation"
                .to_owned(),
        ));
    }
    if attempt_generation == 1 {
        return Ok(rediscovery_key.to_owned());
    }
    let mut hasher = Sha256::new();
    for value in [
        "loyal-rebalance-opportunity-attempt-v1".to_owned(),
        rediscovery_key.to_owned(),
        attempt_generation.to_string(),
    ] {
        let bytes = value.as_bytes();
        hasher.update((bytes.len() as u64).to_le_bytes());
        hasher.update(bytes);
    }
    Ok(format!("{:x}", hasher.finalize()))
}

fn validate_opportunity_input(input: &RebalanceOpportunityInput) -> Result<(), OrchestratorError> {
    if input.cluster.trim().is_empty()
        || input.target_reserve.trim().is_empty()
        || input.liquidity_mint.trim().is_empty()
        || input.priority_version.trim().is_empty()
        || input
            .source_reserve
            .as_deref()
            .is_some_and(|reserve| reserve.trim().is_empty())
    {
        return Err(OrchestratorError::StoreInvariant(
            "rebalance opportunity identity fields must be nonempty".to_owned(),
        ));
    }
    if input.optimizer_epoch_id <= 0
        || input.amount_raw <= 0
        || input.principal_usd_micros <= 0
        || input.estimated_edge_bps <= 0
        || input.estimated_cost_lamports < 0
        || input.annual_yield_gain_usd_micros <= 0
        || input.expected_net_gain_usd_micros <= 0
        || input.economic_priority <= 0
        || !input.execution_plan.is_object()
    {
        return Err(OrchestratorError::StoreInvariant(
            "rebalance opportunity must have positive value, edge, priority, and an object execution plan"
                .to_owned(),
        ));
    }
    if input.route_fingerprint.is_some() != input.requirements_fingerprint.is_some()
        || input
            .route_fingerprint
            .as_deref()
            .is_some_and(str::is_empty)
        || input
            .requirements_fingerprint
            .as_deref()
            .is_some_and(str::is_empty)
    {
        return Err(OrchestratorError::StoreInvariant(
            "rebalance opportunity exact route and requirements fingerprints must be both absent or both nonempty"
                .to_owned(),
        ));
    }
    if input.target_apy_bps - input.source_apy_bps != input.estimated_edge_bps {
        return Err(OrchestratorError::StoreInvariant(
            "rebalance opportunity APYs do not match its estimated edge".to_owned(),
        ));
    }
    let now = Utc::now();
    if input.available_at >= input.expires_at || input.expires_at <= now {
        return Err(OrchestratorError::StoreInvariant(
            "rebalance opportunity must be available before a future expiry".to_owned(),
        ));
    }
    Ok(())
}

fn rebalance_opportunity_matches_input(
    opportunity: &RebalanceOpportunityRecord,
    input: &RebalanceOpportunityInput,
) -> bool {
    let compiler_enriched_original = input.route_fingerprint.is_none()
        && input.requirements_fingerprint.is_none()
        && opportunity.route_fingerprint.is_some()
        && opportunity.requirements_fingerprint.is_some();
    let exact_evidence_matches = compiler_enriched_original
        || (opportunity.route_fingerprint == input.route_fingerprint
            && opportunity.requirements_fingerprint == input.requirements_fingerprint);
    let execution_evidence_matches =
        compiler_enriched_original || opportunity.execution_plan == input.execution_plan;
    opportunity.cluster == input.cluster
        && opportunity.vault_id == input.vault_id
        && opportunity.source_snapshot_id == input.source_snapshot_id
        && opportunity.optimizer_epoch_id == input.optimizer_epoch_id
        && exact_evidence_matches
        && opportunity.source_reserve == input.source_reserve
        && opportunity.target_reserve == input.target_reserve
        && opportunity.liquidity_mint == input.liquidity_mint
        && opportunity.amount_raw == input.amount_raw
        && opportunity.principal_usd_micros == input.principal_usd_micros
        && opportunity.source_apy_bps == input.source_apy_bps
        && opportunity.target_apy_bps == input.target_apy_bps
        && opportunity.estimated_edge_bps == input.estimated_edge_bps
        && opportunity.estimated_cost_lamports == input.estimated_cost_lamports
        && opportunity.annual_yield_gain_usd_micros == input.annual_yield_gain_usd_micros
        && opportunity.expected_net_gain_usd_micros == input.expected_net_gain_usd_micros
        && opportunity.economic_priority == input.economic_priority
        && opportunity.priority_version == input.priority_version
        && execution_evidence_matches
        && opportunity.expires_at == input.expires_at
}

fn validate_opportunity_advance(
    claim_kind: RebalanceOpportunityClaimKind,
    advance: &RebalanceOpportunityAdvance,
) -> Result<(), OrchestratorError> {
    let allowed = match claim_kind {
        RebalanceOpportunityClaimKind::Execute => matches!(
            advance.next_state,
            RebalanceOpportunityState::DecisionCreated
                | RebalanceOpportunityState::Ready
                | RebalanceOpportunityState::Revalidate
                | RebalanceOpportunityState::WaitingAlt
                | RebalanceOpportunityState::Stale
                | RebalanceOpportunityState::Failed
                | RebalanceOpportunityState::Cancelled
        ),
        RebalanceOpportunityClaimKind::Revalidate => matches!(
            advance.next_state,
            RebalanceOpportunityState::Revalidate
                | RebalanceOpportunityState::Ready
                | RebalanceOpportunityState::WaitingAlt
                | RebalanceOpportunityState::Stale
                | RebalanceOpportunityState::Superseded
                | RebalanceOpportunityState::Failed
                | RebalanceOpportunityState::Cancelled
        ),
    };
    if !allowed {
        return Err(OrchestratorError::StoreInvariant(format!(
            "invalid {claim_kind:?} opportunity transition to {}",
            advance.next_state.as_str()
        )));
    }
    if (advance.next_state == RebalanceOpportunityState::DecisionCreated)
        != advance.decision_id.is_some()
    {
        return Err(OrchestratorError::StoreInvariant(
            "only decision_created opportunities may carry a decision id".to_owned(),
        ));
    }
    if advance.next_state.is_terminal()
        && advance.next_state != RebalanceOpportunityState::DecisionCreated
        && advance.reason.as_deref().is_none_or(str::is_empty)
    {
        return Err(OrchestratorError::StoreInvariant(
            "terminal rebalance opportunity transition requires a reason".to_owned(),
        ));
    }
    Ok(())
}

fn validate_signed_route_submission_input(
    opportunity_lease: &RebalanceOpportunityLease,
    input: &SignedRouteSubmissionInput,
) -> Result<(), OrchestratorError> {
    if opportunity_lease.claim_kind != RebalanceOpportunityClaimKind::Execute
        || input.opportunity_id != opportunity_lease.opportunity.id
        || input.cluster != opportunity_lease.opportunity.cluster
        || input.executor_owner != opportunity_lease.owner
        || input.executor_fencing_token != opportunity_lease.fencing_token
    {
        return Err(OrchestratorError::StoreInvariant(
            "signed submission must use the exact execute opportunity lease identity".to_owned(),
        ));
    }
    if input.cluster.trim().is_empty()
        || input.semantic_key.trim().is_empty()
        || input.signed_transaction_hash.trim().is_empty()
        || input.message_hash.trim().is_empty()
        || input.transaction_signature.trim().is_empty()
        || input.recent_blockhash.trim().is_empty()
        || input.alt_requirements_fingerprint.trim().is_empty()
        || input.alt_selection_fingerprint.trim().is_empty()
        || input.fee_payer.trim().is_empty()
        || input.executor_owner.trim().is_empty()
        || input.signed_transaction.is_empty()
        || input.optimizer_epoch_id <= 0
        || input.compiled_fee_lamports < 0
        || input.last_valid_block_height < 0
        || input.executor_fencing_token <= 0
        || !input.alt_mutation_epochs.is_object()
    {
        return Err(OrchestratorError::StoreInvariant(
            "signed submission requires complete immutable wire, blockhash, ALT, and executor evidence"
                .to_owned(),
        ));
    }
    match (
        input.fee_payer_kind,
        input.fee_payer_balance_lamports,
        input.fee_payer_balance_slot,
        input.fee_payer_balance_observed_at,
    ) {
        (RouteFeePayerKind::Policy, None, None, None) => {}
        (
            RouteFeePayerKind::FeeOnlyShard,
            Some(balance),
            Some(observed_slot),
            Some(observed_at),
        ) if balance >= 0 && observed_slot >= 0 && observed_at <= Utc::now() => {}
        _ => {
            return Err(OrchestratorError::StoreInvariant(
                "signed route fee-payer kind requires matching balance snapshot evidence"
                    .to_owned(),
            ));
        }
    }
    let writable_account_keys = canonical_writable_account_keys(&input.writable_account_keys)?;
    if !writable_account_keys.contains(&input.fee_payer) {
        return Err(OrchestratorError::StoreInvariant(
            "signed submission fee payer is absent from exact writable-account evidence".to_owned(),
        ));
    }
    canonical_conflict_account_keys(&input.conflict_account_keys)?;
    Ok(())
}

async fn reserve_fee_only_route_payer_spend(
    connection: &mut sqlx::PgConnection,
    input: &SignedRouteSubmissionInput,
    signed_submission_id: i64,
) -> Result<(), OrchestratorError> {
    if input.fee_payer_kind == RouteFeePayerKind::Policy {
        let policy_payer_is_bound: bool = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.rebalance_opportunities opportunity
                JOIN loyal_yield.managed_vaults vault
                  ON vault.id = opportunity.vault_id
                 AND vault.active
                JOIN loyal_yield.route_policies policy
                  ON policy.id = vault.active_policy_id
                 AND policy.active
                WHERE opportunity.id = $1
                  AND opportunity.cluster = $2
                  AND $3 = ANY(policy.delegated_signers)
                  AND jsonb_array_length(
                      COALESCE($4::jsonb -> 'tables', '[]'::jsonb)
                  ) > 0
                  AND NOT EXISTS (
                      SELECT 1
                      FROM jsonb_array_elements(
                          COALESCE($4::jsonb -> 'tables', '[]'::jsonb)
                      ) selected
                      LEFT JOIN loyal_yield.route_lookup_tables route_table
                        ON route_table.id = (selected ->> 'tableId')::BIGINT
                      LEFT JOIN loyal_yield.lookup_table_families family
                        ON family.id = route_table.family_id
                      WHERE route_table.id IS NULL
                         OR route_table.cluster <> $2
                         OR route_table.authority <> $3
                         OR route_table.payer <> $3
                         OR route_table.family_id IS NULL
                         OR family.id IS NULL
                         OR family.cluster <> $2
                         OR family.provisioning_authority <> $3
                         OR family.payer <> $3
                  )
            )
            "#,
        )
        .bind(input.opportunity_id)
        .bind(&input.cluster)
        .bind(&input.fee_payer)
        .bind(&input.alt_mutation_epochs)
        .fetch_one(&mut *connection)
        .await?;
        if !policy_payer_is_bound {
            return Err(OrchestratorError::StoreInvariant(
                "policy route fee payer is not the vault's durable delegated signer and reusable-v2 authority/payer"
                    .to_owned(),
            ));
        }
        return Ok(());
    }
    let observed_balance_lamports = input.fee_payer_balance_lamports.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "fee-only route payer is missing its exact balance observation".to_owned(),
        )
    })?;
    let observed_balance_slot = input.fee_payer_balance_slot.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "fee-only route payer is missing its balance observation context slot".to_owned(),
        )
    })?;
    let observed_balance_at = input.fee_payer_balance_observed_at.ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "fee-only route payer is missing its balance observation time".to_owned(),
        )
    })?;

    // Idempotent retries preserve their first immutable reservation even if an
    // operator disables the shard after the original atomic handoff.
    if let Some(row) = sqlx::query(
        r#"
        SELECT cluster, fee_payer, opportunity_id, signed_submission_id,
               compiled_fee_lamports, observed_balance_lamports,
               observed_balance_slot, observed_balance_at
        FROM loyal_yield.route_fee_payer_spend_reservations
        WHERE semantic_key = $1
        FOR SHARE
        "#,
    )
    .bind(&input.semantic_key)
    .fetch_optional(&mut *connection)
    .await?
    {
        let matches = row.try_get::<String, _>("cluster")? == input.cluster
            && row.try_get::<String, _>("fee_payer")? == input.fee_payer
            && row.try_get::<i64, _>("opportunity_id")? == input.opportunity_id
            && row.try_get::<i64, _>("signed_submission_id")? == signed_submission_id
            && row.try_get::<i64, _>("compiled_fee_lamports")? == input.compiled_fee_lamports
            && row.try_get::<i64, _>("observed_balance_lamports")? == observed_balance_lamports
            && row.try_get::<i64, _>("observed_balance_slot")? == observed_balance_slot
            && row.try_get::<DateTime<Utc>, _>("observed_balance_at")? == observed_balance_at;
        if !matches {
            return Err(OrchestratorError::StoreInvariant(format!(
                "fee-payer reservation key {:?} collided with different immutable evidence",
                input.semantic_key
            )));
        }
        return Ok(());
    }

    let shard = sqlx::query(
        r#"
        SELECT minimum_balance_lamports, maximum_balance_lamports,
               rolling_window_seconds, maximum_window_spend_lamports,
               maximum_transaction_fee_lamports,
               clock_timestamp() AS admission_checked_at
        FROM loyal_yield.route_fee_payer_shards
        WHERE cluster = $1
          AND fee_payer = $2
          AND enabled
          AND NOT EXISTS (
              SELECT 1
              FROM loyal_yield.lookup_table_families family
              WHERE family.cluster = route_fee_payer_shards.cluster
                AND route_fee_payer_shards.fee_payer
                    IN (family.provisioning_authority, family.payer)
          )
          AND NOT EXISTS (
              SELECT 1
              FROM loyal_yield.route_lookup_tables route_table
              WHERE route_table.cluster = route_fee_payer_shards.cluster
                AND route_fee_payer_shards.fee_payer
                    IN (route_table.authority, route_table.payer)
          )
          AND NOT EXISTS (
              SELECT 1
              FROM loyal_yield.route_policies policy
              WHERE route_fee_payer_shards.fee_payer IN (
                  policy.settings,
                  policy.authority,
                  policy.policy_account,
                  policy.vault_pubkey
              ) OR route_fee_payer_shards.fee_payer = ANY(policy.delegated_signers)
          )
          AND NOT EXISTS (
              SELECT 1
              FROM loyal_yield.managed_vaults vault
              WHERE route_fee_payer_shards.fee_payer
                  IN (vault.settings, vault.vault_pubkey)
          )
        FOR UPDATE
        "#,
    )
    .bind(&input.cluster)
    .bind(&input.fee_payer)
    .fetch_optional(&mut *connection)
    .await?
    .ok_or_else(|| {
        OrchestratorError::StoreInvariant(
            "fee_payer_reselection_required: fee-only route payer is not an enabled durable shard"
                .to_owned(),
        )
    })?;
    let minimum_balance_lamports: i64 = shard.try_get("minimum_balance_lamports")?;
    let maximum_balance_lamports: i64 = shard.try_get("maximum_balance_lamports")?;
    let rolling_window_seconds: i32 = shard.try_get("rolling_window_seconds")?;
    let maximum_window_spend_lamports: i64 = shard.try_get("maximum_window_spend_lamports")?;
    let maximum_transaction_fee_lamports: i64 =
        shard.try_get("maximum_transaction_fee_lamports")?;
    let admission_checked_at: DateTime<Utc> = shard.try_get("admission_checked_at")?;
    if observed_balance_at < admission_checked_at - chrono::Duration::seconds(2)
        || observed_balance_at > admission_checked_at + chrono::Duration::seconds(5)
    {
        return Err(OrchestratorError::StoreInvariant(
            "fee_payer_reselection_required: fee-only payer balance snapshot is stale or future-dated"
                .to_owned(),
        ));
    }
    if input.compiled_fee_lamports > maximum_transaction_fee_lamports {
        return Err(OrchestratorError::StoreInvariant(
            "fee_payer_reselection_required: compiled fee exceeds fee-only payer per-transaction budget"
                .to_owned(),
        ));
    }
    // The shard row lock serializes all admissions for this payer. Subtract
    // only fees that the RPC balance snapshot cannot already include: an
    // unresolved nonterminal broadcast, or any transaction (including a
    // terminal failed/reconciled one) confirmed after the snapshot slot.
    let not_in_observed_balance_lamports: i64 = sqlx::query_scalar(
        r#"
        SELECT COALESCE(SUM(reservation.compiled_fee_lamports), 0)::BIGINT
        FROM loyal_yield.route_fee_payer_spend_reservations reservation
        JOIN loyal_yield.signed_route_submissions submission
          ON submission.id = reservation.signed_submission_id
        WHERE reservation.cluster = $1
          AND reservation.fee_payer = $2
          AND (
              (
                  submission.confirmed_slot IS NULL
                  AND submission.submission_state NOT IN (
                      'reconciled', 'expired', 'failed'
                  )
              )
              OR submission.confirmed_slot > $3
          )
        "#,
    )
    .bind(&input.cluster)
    .bind(&input.fee_payer)
    .bind(observed_balance_slot)
    .fetch_one(&mut *connection)
    .await?;
    let balance_after_committed_fees = observed_balance_lamports
        .checked_sub(not_in_observed_balance_lamports)
        .and_then(|balance| balance.checked_sub(input.compiled_fee_lamports));
    if observed_balance_lamports < minimum_balance_lamports
        || observed_balance_lamports > maximum_balance_lamports
        || balance_after_committed_fees.is_none_or(|balance| balance < minimum_balance_lamports)
    {
        return Err(OrchestratorError::StoreInvariant(
            "fee_payer_reselection_required: fee-only payer balance is outside its durable floor/ceiling budget"
                .to_owned(),
        ));
    }
    let current_window_spend_lamports: i64 = sqlx::query_scalar(
        r#"
        SELECT COALESCE(SUM(compiled_fee_lamports), 0)::BIGINT
        FROM loyal_yield.route_fee_payer_spend_reservations
        WHERE cluster = $1
          AND fee_payer = $2
          AND created_at >= clock_timestamp() - $3 * interval '1 second'
        "#,
    )
    .bind(&input.cluster)
    .bind(&input.fee_payer)
    .bind(rolling_window_seconds)
    .fetch_one(&mut *connection)
    .await?;
    if current_window_spend_lamports
        .checked_add(input.compiled_fee_lamports)
        .is_none_or(|spend| spend > maximum_window_spend_lamports)
    {
        return Err(OrchestratorError::StoreInvariant(
            "fee_payer_reselection_required: fee-only payer rolling spend budget is exhausted"
                .to_owned(),
        ));
    }

    sqlx::query(
        r#"
        INSERT INTO loyal_yield.route_fee_payer_spend_reservations
            (cluster, fee_payer, semantic_key, opportunity_id,
             signed_submission_id, compiled_fee_lamports,
             observed_balance_lamports, observed_balance_slot,
             observed_balance_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
        "#,
    )
    .bind(&input.cluster)
    .bind(&input.fee_payer)
    .bind(&input.semantic_key)
    .bind(input.opportunity_id)
    .bind(signed_submission_id)
    .bind(input.compiled_fee_lamports)
    .bind(observed_balance_lamports)
    .bind(observed_balance_slot)
    .bind(observed_balance_at)
    .execute(&mut *connection)
    .await?;
    Ok(())
}

fn signed_route_submission_alt_table_epochs(
    submission: &SignedRouteSubmissionRecord,
) -> Result<BTreeMap<i64, i64>, OrchestratorError> {
    let tables = submission
        .alt_mutation_epochs
        .get("tables")
        .and_then(Value::as_array)
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(format!(
                "signed submission {} has no canonical ALT table evidence",
                submission.id
            ))
        })?;
    let mut table_epochs = BTreeMap::new();
    for table in tables {
        let table_id = table
            .get("tableId")
            .and_then(Value::as_i64)
            .filter(|table_id| *table_id > 0)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(format!(
                    "signed submission {} has invalid ALT table evidence",
                    submission.id
                ))
            })?;
        let mutation_epoch = table
            .get("mutationEpoch")
            .and_then(Value::as_i64)
            .filter(|mutation_epoch| *mutation_epoch >= 0)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(format!(
                    "signed submission {} has invalid ALT mutation epoch evidence",
                    submission.id
                ))
            })?;
        if table_epochs.insert(table_id, mutation_epoch).is_some() {
            return Err(OrchestratorError::StoreInvariant(format!(
                "signed submission {} repeats ALT table {table_id}",
                submission.id
            )));
        }
    }
    Ok(table_epochs)
}

fn canonical_writable_account_keys(
    writable_account_keys: &[String],
) -> Result<Vec<String>, OrchestratorError> {
    if writable_account_keys.is_empty()
        || writable_account_keys
            .iter()
            .any(|key| key.trim().is_empty() || key.trim() != key)
    {
        return Err(OrchestratorError::StoreInvariant(
            "route conflict set requires nonempty canonical writable-account keys".to_owned(),
        ));
    }
    let mut canonical = writable_account_keys.to_vec();
    canonical.sort_unstable();
    canonical.dedup();
    Ok(canonical)
}

fn canonical_conflict_account_keys(
    conflict_account_keys: &[String],
) -> Result<Vec<String>, OrchestratorError> {
    if conflict_account_keys.len() < 2
        || conflict_account_keys
            .iter()
            .any(|key| key.trim().is_empty() || key.trim() != key)
    {
        return Err(OrchestratorError::StoreInvariant(
            "route conflict ownership requires canonical vault and shared-write lane keys"
                .to_owned(),
        ));
    }
    let mut canonical = conflict_account_keys.to_vec();
    canonical.sort_unstable();
    canonical.dedup();
    let vault_key_count = canonical
        .iter()
        .filter(|key| key.starts_with("vault-write:"))
        .count();
    let lane_key_count = canonical
        .iter()
        .filter(|key| key.starts_with("fleet-shared-write-lane:"))
        .count();
    if canonical.len() != conflict_account_keys.len() || vault_key_count != 1 || lane_key_count != 1
    {
        return Err(OrchestratorError::StoreInvariant(
            "route conflict ownership requires exactly one vault key and one bounded shared-write lane"
                .to_owned(),
        ));
    }
    Ok(canonical)
}

fn signed_route_submission_matches_input(
    submission: &SignedRouteSubmissionRecord,
    input: &SignedRouteSubmissionInput,
    signed_transaction_hash: &str,
    writable_account_keys: &[String],
    conflict_account_keys: &[String],
) -> bool {
    let decision_matches = input
        .decision_id
        .is_none_or(|decision_id| submission.decision_id == Some(decision_id));
    submission.cluster == input.cluster
        && submission.semantic_key == input.semantic_key
        && submission.opportunity_id == input.opportunity_id
        && decision_matches
        && submission.signed_transaction == input.signed_transaction
        && submission.signed_transaction_hash == signed_transaction_hash
        && submission.message_hash == input.message_hash
        && submission.transaction_signature == input.transaction_signature
        && submission.recent_blockhash == input.recent_blockhash
        && submission.last_valid_block_height == input.last_valid_block_height
        && submission.source_snapshot_id == input.source_snapshot_id
        && submission.optimizer_epoch_id == input.optimizer_epoch_id
        && submission.alt_requirements_fingerprint == input.alt_requirements_fingerprint
        && submission.alt_selection_fingerprint == input.alt_selection_fingerprint
        && submission.alt_mutation_epochs == input.alt_mutation_epochs
        && submission.fee_payer == input.fee_payer
        && submission.fee_payer_kind == input.fee_payer_kind
        && submission.compiled_fee_lamports == input.compiled_fee_lamports
        && submission.writable_account_keys == writable_account_keys
        && submission.conflict_account_keys == conflict_account_keys
        && submission.executor_owner == input.executor_owner
        && submission.executor_fencing_token == input.executor_fencing_token
}

fn rebalance_opportunity_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<RebalanceOpportunityRecord, OrchestratorError> {
    let state = RebalanceOpportunityState::parse(row.try_get("opportunity_state")?)?;
    let lease_kind = row
        .try_get::<Option<String>, _>("lease_kind")?
        .as_deref()
        .map(RebalanceOpportunityClaimKind::parse)
        .transpose()?;
    Ok(RebalanceOpportunityRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        idempotency_key: row.try_get("idempotency_key")?,
        rediscovery_key: row.try_get("rediscovery_key")?,
        attempt_generation: row.try_get("attempt_generation")?,
        vault_id: VaultId(row.try_get("vault_id")?),
        source_snapshot_id: row
            .try_get::<Option<i64>, _>("source_snapshot_id")?
            .map(SnapshotId),
        optimizer_epoch_id: row.try_get("optimizer_epoch_id")?,
        route_fingerprint: row.try_get("route_fingerprint")?,
        requirements_fingerprint: row.try_get("requirements_fingerprint")?,
        source_reserve: row.try_get("source_reserve")?,
        target_reserve: row.try_get("target_reserve")?,
        liquidity_mint: row.try_get("liquidity_mint")?,
        amount_raw: row.try_get("amount_raw")?,
        principal_usd_micros: row.try_get("principal_usd_micros")?,
        source_apy_bps: row.try_get("source_apy_bps")?,
        target_apy_bps: row.try_get("target_apy_bps")?,
        estimated_edge_bps: row.try_get("estimated_edge_bps")?,
        estimated_cost_lamports: row.try_get("estimated_cost_lamports")?,
        annual_yield_gain_usd_micros: row.try_get("annual_yield_gain_usd_micros")?,
        expected_net_gain_usd_micros: row.try_get("expected_net_gain_usd_micros")?,
        economic_priority: row.try_get("economic_priority")?,
        priority_version: row.try_get("priority_version")?,
        state,
        execution_plan: row.try_get("execution_plan")?,
        available_at: row.try_get("available_at")?,
        expires_at: row.try_get("expires_at")?,
        lease_kind,
        lease_owner: row.try_get("lease_owner")?,
        lease_expires_at: row.try_get("lease_expires_at")?,
        fencing_token: row.try_get("fencing_token")?,
        attempt_count: row.try_get("attempt_count")?,
        decision_id: row
            .try_get::<Option<i64>, _>("decision_id")?
            .map(DecisionId),
        terminal_reason: row.try_get("terminal_reason")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn optimizer_epoch_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<OptimizerEpochRecord, OrchestratorError> {
    Ok(OptimizerEpochRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        epoch_key: row.try_get("epoch_key")?,
        market_slot: row.try_get("market_slot")?,
        observed_at: row.try_get("observed_at")?,
        expires_at: row.try_get("expires_at")?,
        market_state: row.try_get("market_state")?,
        created_at: row.try_get("created_at")?,
    })
}

fn fleet_planning_state_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<FleetPlanningStateRecord, OrchestratorError> {
    Ok(FleetPlanningStateRecord {
        cluster: row.try_get("cluster")?,
        full_sweep_started_at: row.try_get("full_sweep_started_at")?,
        full_sweep_completed_at: row.try_get("full_sweep_completed_at")?,
        optimizer_epoch_key: row.try_get("optimizer_epoch_key")?,
        optimizer_epoch_expires_at: row.try_get("optimizer_epoch_expires_at")?,
        complete_frontier: row.try_get("complete_frontier")?,
        observed_vault_count: row.try_get("observed_vault_count")?,
        opportunity_count: row.try_get("opportunity_count")?,
        selected_count: row.try_get("selected_count")?,
        deferred_count: row.try_get("deferred_count")?,
        generation: row.try_get("generation")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn fleet_planning_dirty_vault_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<FleetPlanningDirtyVaultRecord, OrchestratorError> {
    Ok(FleetPlanningDirtyVaultRecord {
        cluster: row.try_get("cluster")?,
        vault_id: VaultId(row.try_get("vault_id")?),
        reasons: row.try_get("reasons")?,
        maximum_observed_slot: row.try_get("maximum_observed_slot")?,
        first_dirty_at: row.try_get("first_dirty_at")?,
        last_dirty_at: row.try_get("last_dirty_at")?,
        available_at: row.try_get("available_at")?,
        lease_owner: row.try_get("lease_owner")?,
        lease_expires_at: row.try_get("lease_expires_at")?,
        fencing_token: row.try_get("fencing_token")?,
        generation: row.try_get("generation")?,
        attempt_count: row.try_get("attempt_count")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn orchestration_outbox_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<OrchestrationOutboxRecord, OrchestratorError> {
    Ok(OrchestrationOutboxRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        event_kind: row.try_get("event_kind")?,
        aggregate_kind: row.try_get("aggregate_kind")?,
        aggregate_id: row.try_get("aggregate_id")?,
        dedupe_key: row.try_get("dedupe_key")?,
        payload: row.try_get("payload")?,
        available_at: row.try_get("available_at")?,
        lease_owner: row.try_get("lease_owner")?,
        lease_expires_at: row.try_get("lease_expires_at")?,
        fencing_token: row.try_get("fencing_token")?,
        attempt_count: row.try_get("attempt_count")?,
        processed_at: row.try_get("processed_at")?,
        last_error: row.try_get("last_error")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn signed_route_submission_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<SignedRouteSubmissionRecord, OrchestratorError> {
    Ok(SignedRouteSubmissionRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        semantic_key: row.try_get("semantic_key")?,
        opportunity_id: row.try_get("opportunity_id")?,
        decision_id: row
            .try_get::<Option<i64>, _>("decision_id")?
            .map(DecisionId),
        signed_transaction: row.try_get("signed_transaction")?,
        signed_transaction_hash: row.try_get("signed_transaction_hash")?,
        message_hash: row.try_get("message_hash")?,
        transaction_signature: row.try_get("transaction_signature")?,
        recent_blockhash: row.try_get("recent_blockhash")?,
        last_valid_block_height: row.try_get("last_valid_block_height")?,
        source_snapshot_id: row
            .try_get::<Option<i64>, _>("source_snapshot_id")?
            .map(SnapshotId),
        optimizer_epoch_id: row.try_get("optimizer_epoch_id")?,
        alt_requirements_fingerprint: row.try_get("alt_requirements_fingerprint")?,
        alt_selection_fingerprint: row.try_get("alt_selection_fingerprint")?,
        alt_mutation_epochs: row.try_get("alt_mutation_epochs")?,
        fee_payer: row.try_get("fee_payer")?,
        fee_payer_kind: RouteFeePayerKind::parse(row.try_get("fee_payer_kind")?)?,
        compiled_fee_lamports: row.try_get("compiled_fee_lamports")?,
        writable_account_keys: row.try_get("writable_account_keys")?,
        conflict_account_keys: row.try_get("conflict_account_keys")?,
        executor_owner: row.try_get("executor_owner")?,
        executor_fencing_token: row.try_get("executor_fencing_token")?,
        state: SignedRouteSubmissionState::parse(row.try_get("submission_state")?)?,
        confirmation_available_at: row.try_get("confirmation_available_at")?,
        confirmation_lease_owner: row.try_get("confirmation_lease_owner")?,
        confirmation_lease_expires_at: row.try_get("confirmation_lease_expires_at")?,
        confirmation_fencing_token: row.try_get("confirmation_fencing_token")?,
        confirmation_attempt_count: row.try_get("confirmation_attempt_count")?,
        broadcast_count: row.try_get("broadcast_count")?,
        last_broadcast_at: row.try_get("last_broadcast_at")?,
        last_status_checked_at: row.try_get("last_status_checked_at")?,
        expiry_observed_block_height: row.try_get("expiry_observed_block_height")?,
        effect_check_slot: row.try_get("effect_check_slot")?,
        submitted_slot: row.try_get("submitted_slot")?,
        submitted_at: row.try_get("submitted_at")?,
        confirmed_slot: row.try_get("confirmed_slot")?,
        confirmed_at: row.try_get("confirmed_at")?,
        reconciled_slot: row.try_get("reconciled_slot")?,
        reconciled_at: row.try_get("reconciled_at")?,
        error_detail: row.try_get("error_detail")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

fn route_account_conflict_lease_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<RouteAccountConflictLease, OrchestratorError> {
    Ok(RouteAccountConflictLease {
        cluster: row.try_get("cluster")?,
        writable_account_key: row.try_get("writable_account_key")?,
        opportunity_id: row.try_get("opportunity_id")?,
        lease_owner: row.try_get("lease_owner")?,
        fencing_token: row.try_get("fencing_token")?,
        expires_at: row.try_get("expires_at")?,
        submission_id: row.try_get("submission_id")?,
    })
}

fn physical_writable_key_congestion_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<PhysicalWritableKeyCongestion, OrchestratorError> {
    let congestion = PhysicalWritableKeyCongestion {
        writable_account_key: row.try_get("writable_account_key")?,
        classification: row.try_get("classification")?,
        active_submission_count: row.try_get("active_submission_count")?,
        principal_usd_micros: row.try_get("principal_usd_micros")?,
        recoverable_yield_usd_micros_per_hour: row
            .try_get("recoverable_yield_usd_micros_per_hour")?,
    };
    if congestion.writable_account_key.trim().is_empty()
        || !matches!(
            congestion.classification.as_str(),
            "payer" | "target" | "other"
        )
        || congestion.active_submission_count <= 0
        || congestion.principal_usd_micros < 0
        || congestion.recoverable_yield_usd_micros_per_hour < 0
    {
        return Err(OrchestratorError::StoreInvariant(
            "physical writable-key congestion evidence is malformed".to_owned(),
        ));
    }
    Ok(congestion)
}

fn fleet_status_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<FleetOrchestrationStatus, OrchestratorError> {
    Ok(FleetOrchestrationStatus {
        cluster: row.try_get("cluster")?,
        opportunity_state: row.try_get("opportunity_state")?,
        opportunity_count: row.try_get("opportunity_count")?,
        principal_usd_micros: row.try_get("principal_usd_micros")?,
        annual_yield_gain_usd_micros: row.try_get("annual_yield_gain_usd_micros")?,
        yield_gain_usd_micros_per_hour: row.try_get("yield_gain_usd_micros_per_hour")?,
        oldest_created_at: row.try_get("oldest_created_at")?,
        oldest_state_entered_at: row.try_get("oldest_state_entered_at")?,
        oldest_age_seconds: row.try_get("oldest_age_seconds")?,
        oldest_state_age_seconds: row.try_get("oldest_state_age_seconds")?,
        expired_lease_count: row.try_get("expired_lease_count")?,
        pending_outbox_count: row.try_get("pending_outbox_count")?,
        pending_submission_count: row.try_get("pending_submission_count")?,
        pending_compiled_fee_lamports: row.try_get("pending_compiled_fee_lamports")?,
        expiry_check_pending_count: row.try_get("expiry_check_pending_count")?,
        effect_ambiguous_count: row.try_get("effect_ambiguous_count")?,
        oldest_pending_submission_at: row.try_get("oldest_pending_submission_at")?,
        oldest_pending_submission_age_seconds: row
            .try_get("oldest_pending_submission_age_seconds")?,
        sender_submission_count: row.try_get("sender_submission_count")?,
        oldest_sender_state_entered_at: row.try_get("oldest_sender_state_entered_at")?,
        oldest_sender_state_age_seconds: row.try_get("oldest_sender_state_age_seconds")?,
        confirmer_submission_count: row.try_get("confirmer_submission_count")?,
        oldest_confirmer_state_entered_at: row.try_get("oldest_confirmer_state_entered_at")?,
        oldest_confirmer_state_age_seconds: row.try_get("oldest_confirmer_state_age_seconds")?,
        reconciler_submission_count: row.try_get("reconciler_submission_count")?,
        oldest_reconciler_state_entered_at: row.try_get("oldest_reconciler_state_entered_at")?,
        oldest_reconciler_state_age_seconds: row.try_get("oldest_reconciler_state_age_seconds")?,
        planner_registered_at: row.try_get("planner_registered_at")?,
        planner_last_seen_at: row.try_get("planner_last_seen_at")?,
        planner_last_seen_age_seconds: row.try_get("planner_last_seen_age_seconds")?,
        full_sweep_started_at: row.try_get("full_sweep_started_at")?,
        full_sweep_completed_at: row.try_get("full_sweep_completed_at")?,
        full_sweep_age_seconds: row.try_get("full_sweep_age_seconds")?,
        planned_optimizer_epoch_key: row.try_get("planned_optimizer_epoch_key")?,
        planned_optimizer_epoch_expires_at: row.try_get("planned_optimizer_epoch_expires_at")?,
        complete_frontier: row.try_get("complete_frontier")?,
        observed_vault_count: row.try_get("observed_vault_count")?,
        planned_opportunity_count: row.try_get("planned_opportunity_count")?,
        planned_selected_count: row.try_get("planned_selected_count")?,
        planned_deferred_count: row.try_get("planned_deferred_count")?,
        planning_generation: row.try_get("planning_generation")?,
        latest_market_epoch_id: row.try_get("latest_market_epoch_id")?,
        latest_market_epoch_key: row.try_get("latest_market_epoch_key")?,
        latest_market_slot: row.try_get("latest_market_slot")?,
        latest_market_observed_at: row.try_get("latest_market_observed_at")?,
        latest_market_expires_at: row.try_get("latest_market_expires_at")?,
        latest_market_epoch_age_seconds: row.try_get("latest_market_epoch_age_seconds")?,
        latest_market_epoch_expires_in_seconds: row
            .try_get("latest_market_epoch_expires_in_seconds")?,
        latest_market_epoch_expired: row.try_get("latest_market_epoch_expired")?,
        planner_epoch_matches_latest: row.try_get("planner_epoch_matches_latest")?,
        waiting_alt_opportunity_count: row.try_get("waiting_alt_opportunity_count")?,
        waiting_alt_principal_usd_micros: row.try_get("waiting_alt_principal_usd_micros")?,
        waiting_alt_yield_gain_usd_micros_per_hour: row
            .try_get("waiting_alt_yield_gain_usd_micros_per_hour")?,
        oldest_waiting_alt_state_entered_at: row.try_get("oldest_waiting_alt_state_entered_at")?,
        oldest_waiting_alt_state_age_seconds: row
            .try_get("oldest_waiting_alt_state_age_seconds")?,
        ready_opportunity_count: row.try_get("ready_opportunity_count")?,
        ready_principal_usd_micros: row.try_get("ready_principal_usd_micros")?,
        ready_yield_gain_usd_micros_per_hour: row
            .try_get("ready_yield_gain_usd_micros_per_hour")?,
        oldest_ready_state_entered_at: row.try_get("oldest_ready_state_entered_at")?,
        oldest_ready_state_age_seconds: row.try_get("oldest_ready_state_age_seconds")?,
        current_epoch_opportunity_count: row.try_get("current_epoch_opportunity_count")?,
        current_epoch_principal_usd_micros: row.try_get("current_epoch_principal_usd_micros")?,
        current_epoch_recoverable_yield_usd_micros_per_hour: row
            .try_get("current_epoch_recoverable_yield_usd_micros_per_hour")?,
        current_epoch_submitted_within_10s_yield_ppm: row
            .try_get("current_epoch_submitted_within_10s_yield_ppm")?,
        current_epoch_submitted_within_2m_yield_ppm: row
            .try_get("current_epoch_submitted_within_2m_yield_ppm")?,
        current_epoch_submitted_within_10m_yield_ppm: row
            .try_get("current_epoch_submitted_within_10m_yield_ppm")?,
        current_epoch_confirmed_within_30s_yield_ppm: row
            .try_get("current_epoch_confirmed_within_30s_yield_ppm")?,
        current_epoch_submission_p95_milliseconds: row
            .try_get("current_epoch_submission_p95_milliseconds")?,
        current_epoch_confirmation_p95_milliseconds: row
            .try_get("current_epoch_confirmation_p95_milliseconds")?,
        current_epoch_compiled_fee_lamports: row.try_get("current_epoch_compiled_fee_lamports")?,
        active_physical_writable_key_count: 0,
        top_physical_writable_key_congestion: Vec::new(),
    })
}
