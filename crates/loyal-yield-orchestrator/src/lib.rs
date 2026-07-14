mod domain;
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
    AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED, ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
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
    solana_testing_keypair_from_env, yield_router_keypair_from_env, PolicySignerError,
    POLICY_KEYPAIR_ENV, SOLANA_TESTING_PK_ENV, YIELD_ROUTER_KEYPAIR_ENV,
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
    #[error("lookup-table binding {binding_id} activation is blocked by a live usage lease")]
    LookupTableBindingActivationDeferred { binding_id: i64 },
}

impl OrchestratorError {
    pub fn amount_out_of_range(value: u64) -> Self {
        Self::AmountOutOfRange { value }
    }
}
