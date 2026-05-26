//! Rust SDK for constructing Loyal delegated Squads actions.
//!
//! The crate owns production-facing action setup. Test harnesses should use
//! this SDK to build instructions, then execute them in their own runtime.

mod ids;
mod squads;
mod yield_route;

pub use ids::*;
pub use squads::derive_action_account;
pub use yield_route::*;
