mod domain;
mod planner;
mod reconcile;
mod route_builder;
mod rpc;
mod same_mint_loop;
mod signer;
mod store;
mod types;

pub use domain::state_transition;
pub use planner::*;
pub use reconcile::*;
pub use route_builder::*;
pub use rpc::*;
pub use same_mint_loop::*;
pub use signer::{
    keypair_from_hex, yield_router_keypair_from_env, PolicySignerError, YIELD_ROUTER_KEYPAIR_ENV,
};
pub use store::{NeonSqlClient, OrchestratorStore};
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
}

impl OrchestratorError {
    pub fn amount_out_of_range(value: u64) -> Self {
        Self::AmountOutOfRange { value }
    }
}
