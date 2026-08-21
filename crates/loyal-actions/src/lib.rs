//! Rust SDK for constructing Loyal delegated Squads actions.
//!
//! The crate owns production-facing action setup. Test harnesses should use
//! this SDK to build instructions, then execute them in their own runtime.

mod actions;
pub mod autonomous_vaults;
mod detection;
mod ids;
pub mod jupiter;
pub mod lookup_tables;
mod protocols;
mod squads;
mod stablecoins;

pub use actions::*;
pub use detection::*;
pub use ids::*;
pub use lookup_tables::*;
pub use loyal_hub_abi as hub_abi;
pub use protocols::{
    derive_kamino_lending_market_authority, derive_kamino_obligation,
    derive_kamino_obligation_farm_user_state, derive_kamino_user_metadata,
    derive_kamino_vanilla_obligation, derive_loyal_hub_authority,
    derive_loyal_hub_authority_for_program, derive_loyal_hub_config,
    derive_loyal_hub_config_for_program, derive_loyal_hub_inventory_account,
    derive_loyal_hub_inventory_account_for_program,
    derive_loyal_hub_inventory_account_for_token_program, derive_loyal_hub_lane_authority,
    derive_loyal_hub_lane_authority_for_program, derive_loyal_hub_lane_inventory_account,
    derive_loyal_hub_lane_inventory_account_for_program,
    derive_loyal_hub_lane_inventory_account_for_token_program, derive_loyal_hub_pending_admin,
    derive_loyal_hub_pending_admin_for_program, derive_recurring_delegation,
    derive_subscription_authority, derive_subscription_event_authority,
    group_loyal_hub_rebalance_transfers, hub_rebalance, kamino_init_obligation_farm_instruction,
    loyal_hub_accept_admin_transfer_data, loyal_hub_accept_admin_transfer_instruction,
    loyal_hub_accept_admin_transfer_instruction_for_program, loyal_hub_config_data,
    loyal_hub_initialize_config_data, loyal_hub_initialize_config_instruction,
    loyal_hub_initialize_config_instruction_for_program, loyal_hub_rebalance_inventory_data,
    loyal_hub_rebalance_inventory_instruction,
    loyal_hub_rebalance_inventory_instruction_for_program,
    loyal_hub_rebalance_inventory_instruction_for_program_with_token_program,
    loyal_hub_rebalance_inventory_instruction_with_token_program,
    loyal_hub_request_admin_transfer_data, loyal_hub_request_admin_transfer_instruction,
    loyal_hub_request_admin_transfer_instruction_for_program, loyal_hub_set_admin_data,
    loyal_hub_set_admin_instruction, loyal_hub_set_admin_instruction_for_program,
    loyal_hub_set_hub_authorizer_data, loyal_hub_set_hub_authorizer_instruction,
    loyal_hub_set_hub_authorizer_instruction_for_program, loyal_hub_set_inventory_rebalancer_data,
    loyal_hub_set_inventory_rebalancer_instruction,
    loyal_hub_set_inventory_rebalancer_instruction_for_program, loyal_hub_set_lane_count_data,
    loyal_hub_set_lane_count_instruction, loyal_hub_set_lane_count_instruction_for_program,
    loyal_hub_set_max_fee_data, loyal_hub_set_max_fee_instruction,
    loyal_hub_set_max_fee_instruction_for_program, loyal_hub_set_paused_data,
    loyal_hub_set_paused_instruction, loyal_hub_set_paused_instruction_for_program,
    loyal_hub_swap_exact_in_data, loyal_hub_swap_exact_in_instruction,
    loyal_hub_swap_exact_in_instruction_for_program,
    loyal_hub_swap_exact_in_instruction_for_program_with_token_programs,
    loyal_hub_swap_exact_in_instruction_with_token_programs, loyal_hub_withdraw_inventory_data,
    loyal_hub_withdraw_inventory_instruction, loyal_hub_withdraw_inventory_instruction_for_program,
    loyal_hub_withdraw_inventory_instruction_for_program_with_token_program,
    loyal_hub_withdraw_inventory_instruction_with_source,
    loyal_hub_withdraw_inventory_instruction_with_source_for_program,
    loyal_hub_withdraw_inventory_instruction_with_source_for_program_with_token_program,
    loyal_hub_withdraw_inventory_instruction_with_source_with_token_program,
    loyal_hub_withdraw_inventory_instruction_with_token_program,
    subscription_create_recurring_delegation_data, subscription_init_authority_data,
    subscription_revoke_delegation_data, subscription_transfer_recurring_data,
    KaminoInitObligationFarm, LoyalHubLaneRebalanceTransfer, LoyalHubMintRebalanceBatch,
    LoyalHubRebalanceBuilder, LoyalHubRebalanceTransfer, LoyalHubSwapExactIn,
};
pub use squads::{
    compile_squads_inner_instruction, create_unlimited_spl_spending_limit_policy_instruction,
    decode_settings_signer_handoff_instruction, derive_action_account,
    derive_associated_token_account, derive_classic_associated_token_account,
    derive_squads_v4_vault, derive_squads_vault, execute_program_interaction_policy_instruction,
    execute_spl_spending_limit_policy_instruction, execute_sync_transaction_instruction,
    handoff_settings_signer_instruction, remove_policies_instruction, remove_policy_instruction,
    LoyalActionError, Result, SquadsCompiledInstruction, SquadsSettingsSignerHandoff,
};
pub use stablecoins::{
    earn_stablecoin, earn_stablecoin_pairs, earn_stablecoins, EarnStablecoin, EarnStablecoinPair,
    EARN_STABLECOINS,
};
