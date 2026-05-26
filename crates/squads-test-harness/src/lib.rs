//! Vertical-slice helpers for Squads yield-routing tests.
//!
//! The crate keeps the public API grouped by the concepts a test author is
//! likely to look for:
//!
//! - [`squads`] owns Squads PDA derivation, smart-account setup, and sync
//!   transaction payload helpers.
//! - [`policies`] owns raw Squads policy instruction builders.
//! - [`yield_route`] owns route-level policy bundles such as Kamino
//!   withdraw/swap/deposit setups.
//! - [`protocols`] owns test protocol fixtures, SPL account seeding, and mock
//!   Jupiter/Kamino/Loyal Hub instruction data.
//! - [`runtime`] owns LiteSVM setup and transaction submission helpers.
//!
//! Root-level re-exports are kept for existing tests. New code can import from
//! the domain modules or from [`prelude`] when a scenario needs the common
//! harness surface.

pub mod constants;
pub mod execution;
pub mod policies;
pub mod protocols;
pub mod runtime;
pub mod squads;
pub mod types;
pub mod yield_route;

pub use constants::*;
pub use execution::*;
pub use policies::*;
pub use protocols::*;
pub use runtime::*;
pub use squads::*;
pub use types::{
    FundedSquadsTestConfig, FundedSquadsTestContext, MockJupiterStableReserveTokenAccount,
    MockJupiterTokenAccounts, MockKaminoReserveTokenAccounts, MockProgram, SquadsPool,
    SquadsYieldRoutePolicies, SquadsYieldRoutePolicyInstruction,
    SquadsYieldRoutePolicyInstructions, SquadsYieldRoutePolicySeeds,
    SquadsYieldRoutePolicyWhitelist, SwapLane,
};
pub use yield_route::*;

pub mod prelude {
    pub use crate::constants::*;
    pub use crate::execution::*;
    pub use crate::policies::*;
    pub use crate::protocols::*;
    pub use crate::runtime::*;
    pub use crate::squads::*;
    pub use crate::types::{
        FundedSquadsTestConfig, FundedSquadsTestContext, MockJupiterStableReserveTokenAccount,
        MockJupiterTokenAccounts, MockKaminoReserveTokenAccounts, MockProgram, SquadsPool,
        SquadsYieldRoutePolicies, SquadsYieldRoutePolicyInstruction,
        SquadsYieldRoutePolicyInstructions, SquadsYieldRoutePolicySeeds,
        SquadsYieldRoutePolicyWhitelist, SwapLane,
    };
    pub use crate::yield_route::*;
}
