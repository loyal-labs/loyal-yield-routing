use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::fmt;
use std::time::Duration;

use crate::{DecisionId, VaultId};

pub const DEFAULT_STRATEGY: &str = "same_mint_max_apy_v1";

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum WorkerKind {
    Target,
    VaultScan,
    Reconcile,
    Planner,
    Simulation,
    Batch,
    Submit,
    Confirm,
    Sweeper,
}

impl WorkerKind {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Target => "target",
            Self::VaultScan => "vault_scan",
            Self::Reconcile => "reconcile",
            Self::Planner => "planner",
            Self::Simulation => "simulation",
            Self::Batch => "batch",
            Self::Submit => "submit",
            Self::Confirm => "confirm",
            Self::Sweeper => "sweeper",
        }
    }
}

impl fmt::Display for WorkerKind {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum QueueStatus {
    Pending,
    Leased,
    Succeeded,
    Failed,
    Dead,
}

impl QueueStatus {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Pending => "pending",
            Self::Leased => "leased",
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Dead => "dead",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AttemptStatus {
    Building,
    Simulated,
    Ready,
    Batched,
    Failed,
    Expired,
}

impl AttemptStatus {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Building => "building",
            Self::Simulated => "simulated",
            Self::Ready => "ready",
            Self::Batched => "batched",
            Self::Failed => "failed",
            Self::Expired => "expired",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BatchStatus {
    Building,
    Signed,
    Submitted,
    Confirming,
    Confirmed,
    Failed,
    Expired,
}

impl BatchStatus {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Building => "building",
            Self::Signed => "signed",
            Self::Submitted => "submitted",
            Self::Confirming => "confirming",
            Self::Confirmed => "confirmed",
            Self::Failed => "failed",
            Self::Expired => "expired",
        }
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct ReserveApySample {
    pub reserve: String,
    pub market: Option<String>,
    pub liquidity_mint: String,
    pub supply_apy_bps: i64,
    pub total_supply_usd_estimate: f64,
    pub stale: bool,
    pub observed_slot: Option<i64>,
    pub observed_at: DateTime<Utc>,
    pub source_cursor: Value,
}

#[derive(Debug, Clone, PartialEq)]
pub struct ReserveTargetCandidate {
    pub cluster: String,
    pub strategy: String,
    pub liquidity_mint: String,
    pub target_reserve: String,
    pub target_market: Option<String>,
    pub target_supply_apy_bps: i64,
    pub observed_slot: Option<i64>,
    pub observed_at: DateTime<Utc>,
    pub source_cursor: Value,
    pub filters: Value,
    pub target_epoch: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReserveTarget {
    pub id: i64,
    pub cluster: String,
    pub strategy: String,
    pub liquidity_mint: String,
    pub target_reserve: String,
    pub target_market: Option<String>,
    pub target_supply_apy_bps: i64,
    pub target_epoch: String,
    pub stale: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VaultReconcileJob {
    pub id: i64,
    pub vault_id: VaultId,
    pub target_id: Option<i64>,
    pub cluster: String,
    pub liquidity_mint: String,
    pub target_reserve: String,
    pub target_epoch: String,
    pub attempt_count: i32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DecisionWorkItem {
    pub decision_id: DecisionId,
    pub vault_id: VaultId,
    pub cluster: String,
    pub liquidity_mint: String,
    pub source_reserve: String,
    pub target_reserve: String,
    pub amount_raw: i64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReadyAttempt {
    pub attempt_id: i64,
    pub decision: DecisionWorkItem,
    pub estimated_compute_units: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BatchPlan {
    pub attempts: Vec<ReadyAttempt>,
}

impl BatchPlan {
    pub fn is_empty(&self) -> bool {
        self.attempts.is_empty()
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SignedBatch {
    pub batch_id: i64,
    pub signature: String,
    pub recent_blockhash: String,
    pub last_valid_block_height: i64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SubmitObservation {
    Accepted,
    RateLimited,
    Unknown,
    Fatal(String),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SubmitAction {
    Broadcast,
    Backoff(Duration),
    ExpireAndReconcile,
    Fail(String),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ConfirmationObservation {
    Confirmed { slot: Option<i64> },
    Failed { reason: String },
    Unknown,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ConfirmationAction {
    KeepPolling,
    MarkConfirmed { slot: Option<i64> },
    MarkFailed { reason: String },
    ExpireAndReconcile,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LeaseState {
    pub status: QueueStatus,
    pub lease_expires_at: Option<DateTime<Utc>>,
    pub attempt_count: i32,
    pub max_attempts: i32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SweepAction {
    Keep,
    ReleaseLease,
    DeadLetter,
}
