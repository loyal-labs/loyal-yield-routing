//! Rust SDK for constructing Loyal delegated Squads actions.
//!
//! The crate owns production-facing action setup. Test harnesses should use
//! this SDK to build instructions, then execute them in their own runtime.

mod actions;
mod ids;
mod protocols;
mod squads;

pub use actions::*;
pub use ids::*;
pub use protocols::{
    derive_loyal_hub_authority, derive_loyal_hub_config, derive_loyal_hub_inventory_account,
    derive_loyal_hub_lane_authority, derive_loyal_hub_lane_inventory_account,
};
pub use squads::{derive_action_account, LoyalActionError, Result};
