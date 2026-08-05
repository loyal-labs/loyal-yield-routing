mod domain;
pub mod fleet_orchestration;
pub mod lookup_table_alerts;
pub mod lookup_tables;
pub mod rpc_safety;
mod shared_market_catalog;
mod signer;
mod stable_mints;
mod store;
mod types;

pub use domain::{
    route_amount_evidence, route_amount_evidence_from_metadata, state_transition,
    AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED, FIXED_KAMINO_MAIN_ROUTE_MODE,
    MAX_QUEUE_POSITIVE_AMOUNT_DRIFT_PPM, ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
};
pub use lookup_table_alerts::*;
pub use lookup_tables::*;
pub use shared_market_catalog::{
    decode_kamino_reserve_account, derive_shared_market_catalog,
    load_finalized_kamino_reserve_catalog, validate_supported_reserve, DerivedSharedMarketCatalog,
    FinalizedKaminoReserveCatalog, KaminoReserveCatalogAccount, SharedMarketCatalogError,
    SupportedKaminoReserve,
};
pub use signer::{
    keypair_from_env, keypair_from_hex, keypair_from_string, policy_keypair_from_env,
    route_fee_payer_keypairs_from_env, solana_testing_keypair_from_env,
    standard_policy_keypair_from_env, yield_router_keypair_from_env, PolicySignerError,
    POLICY_KEYPAIR_ENV, SOLANA_TESTING_PK_ENV, STANDARD_POLICY_AUTHORITY, YIELD_ROUTER_KEYPAIR_ENV,
    YIELD_ROUTE_FEE_PAYER_KEYPAIRS_ENV,
};
pub use stable_mints::{
    enabled_stable_mints_from_env, enabled_stable_mints_hash, resolve_enabled_stable_mints,
    supported_stable_mints, StableMintConfigError, ENABLED_STABLE_MINTS_ENV,
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
    /// The market snapshot re-derived an epoch key that is already stored under
    /// different immutable evidence. The stored row stays authoritative and the
    /// caller must re-observe instead of publishing against ambiguous evidence.
    /// This is recoverable by construction: no route may be admitted, but the
    /// planning process has nothing to repair.
    #[error("optimizer epoch key {epoch_key} is stored under different immutable evidence")]
    OptimizerEpochEvidenceConflict { epoch_key: String },
    /// The optimizer epoch backing this publication fell below the minimum
    /// usable lifetime while the wave was being written. Wall-clock passage is
    /// not a store defect, so the opportunity is deferred to the next wave.
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
