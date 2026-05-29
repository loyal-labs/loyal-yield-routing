//! Rust SDK for constructing Loyal delegated Squads actions.
//!
//! The crate owns production-facing action setup. Test harnesses should use
//! this SDK to build instructions, then execute them in their own runtime.

mod actions;
mod detection;
mod ids;
mod protocols;
mod squads;

pub use actions::*;
pub use detection::*;
pub use ids::*;
pub use loyal_hub_abi as hub_abi;
pub use protocols::{
    derive_loyal_hub_authority, derive_loyal_hub_config, derive_loyal_hub_inventory_account,
    derive_loyal_hub_lane_authority, derive_loyal_hub_lane_inventory_account,
    group_loyal_hub_rebalance_transfers, hub_rebalance, loyal_hub_config_data,
    loyal_hub_initialize_config_data, loyal_hub_initialize_config_instruction,
    loyal_hub_rebalance_inventory_data, loyal_hub_rebalance_inventory_instruction,
    loyal_hub_set_max_fee_data, loyal_hub_set_max_fee_instruction, loyal_hub_set_paused_data,
    loyal_hub_set_paused_instruction, loyal_hub_swap_exact_in_data,
    loyal_hub_swap_exact_in_instruction, loyal_hub_withdraw_inventory_data,
    loyal_hub_withdraw_inventory_instruction, loyal_hub_withdraw_inventory_instruction_with_source,
    LoyalHubLaneRebalanceTransfer, LoyalHubMintRebalanceBatch, LoyalHubRebalanceBuilder,
    LoyalHubRebalanceTransfer, LoyalHubSwapExactIn,
};
pub use squads::{derive_action_account, LoyalActionError, Result};
