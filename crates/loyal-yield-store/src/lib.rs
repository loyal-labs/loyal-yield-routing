//! Shared SQL/domain layer for the yield-routing workers.
//!
//! This crate carries no Solana, RPC, or observability dependencies on purpose:
//! it is what the small workers depend on instead of pulling the whole
//! orchestrator graph. Keep it that way — a heavyweight dependency added here
//! lands in every worker image.
//!
//! Adding a dependency also edits `Cargo.toml`, which moves `recipe.json` and
//! invalidates the `cargo chef cook` layer shared by all three images. Changes
//! confined to crate source leave `recipe.json` byte-identical and reuse it.

pub mod domain;
pub mod fleet_orchestration;
mod store;
pub mod types;

pub use domain::{
    route_amount_evidence, route_amount_evidence_from_metadata, state_transition,
    AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED, FIXED_KAMINO_MAIN_ROUTE_MODE,
    MAX_QUEUE_POSITIVE_AMOUNT_DRIFT_PPM, ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
};
pub use store::{NeonSqlClient, OrchestratorStore, RouteLookupTableProvisioningLock};
pub use types::*;

pub use sqlx;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum OrchestratorError {
    #[error("database error: {0}")]
    Sqlx(#[from] sqlx::Error),
    #[error("policy match slot {0} does not fit Postgres BIGINT")]
    SlotOutOfRange(u64),
    #[error("policy seed {0} does not fit Postgres BIGINT")]
    PolicySeedOutOfRange(u64),
    #[error("amount {value} does not fit Postgres BIGINT")]
    AmountOutOfRange { value: u64 },
    #[error("snapshot must include at least one supported reserve position")]
    EmptySnapshot,
    #[error("invalid value for decision status: {0}")]
    UnknownDecisionStatus(String),
    #[error("attempt is terminal in status {0}")]
    TerminalDecision(DecisionStatus),
    #[error("terminal decision repeat conflicts with persisted {field}")]
    ConflictingTerminalRepeat { field: &'static str },
    #[error("invalid decision transition from {from} with {advance:?}")]
    InvalidDecisionTransition {
        from: DecisionStatus,
        advance: DecisionAdvance,
    },
    #[error("unexpected store state: {0}")]
    StoreInvariant(String),
    #[error("same-mint rebalance validation failed: {0}")]
    SameMintRebalanceValidation(String),
    #[error(
        "vault {vault_id} observation slot {observed_slot} is older than current slot {current_slot}"
    )]
    StaleVaultObservation {
        vault_id: VaultId,
        observed_slot: i64,
        current_slot: i64,
    },
    #[error(
        "vault {vault_id} observation slot {observed_slot} conflicts with the current state at the same slot"
    )]
    ConflictingVaultObservation {
        vault_id: VaultId,
        observed_slot: i64,
    },
    #[error("lookup-table binding {binding_id} activation is blocked by a live usage lease")]
    LookupTableBindingActivationDeferred { binding_id: i64 },
    #[error("new opportunity for vault {vault_id} is deferred behind unexpired lease {leased_id}")]
    OpportunityDeferredBehindLease { vault_id: VaultId, leased_id: i64 },
    #[error(
        "new opportunity for vault {vault_id} is deferred behind active slot owner {slot_opportunity_id:?}"
    )]
    OpportunityDeferredBehindActiveSlot {
        vault_id: VaultId,
        slot_opportunity_id: Option<i64>,
        slot_opportunity_state: Option<String>,
        reason: &'static str,
    },
    #[error("optimizer epoch key {epoch_key} is stored under different immutable evidence")]
    OptimizerEpochEvidenceConflict { epoch_key: String },
    #[error(
        "opportunity for vault {vault_id} lost the minimum usable optimizer epoch lifetime at {stage}"
    )]
    OpportunityDeferredBehindEpochLifetime {
        vault_id: VaultId,
        stage: &'static str,
    },
}

impl OrchestratorError {
    pub fn amount_out_of_range(value: u64) -> Self {
        Self::AmountOutOfRange { value }
    }
}
