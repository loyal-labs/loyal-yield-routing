#![allow(clippy::too_many_arguments)]

//! Vertical-slice helpers for Squads and Loyal Actions tests.
//!
//! The crate keeps the public API grouped by the concepts a test author is
//! likely to look for:
//!
//! - [`squads`] owns Squads PDA derivation, smart-account setup, and sync
//!   transaction payload helpers.
//! - [`actions`] adapts test contexts and mock protocol accounts into
//!   `loyal-actions` SDK inputs.
//! - [`policies`] owns raw Squads policy instruction builders for focused
//!   Squads tests.
//! - [`protocols`] owns test protocol fixtures, SPL account seeding, and mock
//!   Jupiter/Kamino/Loyal Hub instruction data.
//! - [`runtime`] owns LiteSVM setup and transaction submission helpers.
//!
//! New scenario tests can import from [`prelude`] when they need the common
//! harness surface.

pub mod actions;
pub mod constants;
pub mod execution;
pub mod policies;
pub mod protocols;
pub mod runtime;
pub mod squads;
pub mod types;

pub use actions::*;
pub use constants::*;
pub use execution::*;
pub use policies::*;
pub use protocols::*;
pub use runtime::*;
pub use squads::*;
pub use types::{
    FundedSquadsTestConfig, FundedSquadsTestContext, MockJupiterStableReserveTokenAccount,
    MockJupiterTokenAccounts, MockKaminoReserveTokenAccounts, MockProgram,
    SquadsInternalFundTransferPayload, SquadsInternalFundTransferPolicyCreationPayload, SquadsPool,
};

pub mod prelude {
    pub use crate::actions::*;
    pub use crate::constants::*;
    pub use crate::execution::*;
    pub use crate::policies::*;
    pub use crate::protocols::*;
    pub use crate::runtime::*;
    pub use crate::squads::*;
    pub use crate::types::{
        FundedSquadsTestConfig, FundedSquadsTestContext, MockJupiterStableReserveTokenAccount,
        MockJupiterTokenAccounts, MockKaminoReserveTokenAccounts, MockProgram,
        SquadsInternalFundTransferPayload, SquadsInternalFundTransferPolicyCreationPayload,
        SquadsPool,
    };
}
