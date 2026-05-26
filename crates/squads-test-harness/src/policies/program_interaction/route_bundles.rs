use super::common::create_squads_compact_program_interaction_policy_instruction;
use super::kamino::{
    kamino_route_market_mint_instruction_constraint, kamino_route_mint_instruction_constraint,
};
use super::stable_swap::{
    jupiter_route_stable_swap_minimal_instruction_constraint,
    loyal_hub_route_stable_swap_instruction_constraint,
};
use crate::*;
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};

pub fn create_squads_program_interaction_route_all_in_one_market_mint_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
    allowed_swap_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
) -> Instruction {
    assert!(
        !swap_lanes.is_empty(),
        "yield route all-in-one policy needs at least one swap lane"
    );
    let mut constraints = Vec::with_capacity(2 + swap_lanes.len());
    constraints.push(kamino_route_market_mint_instruction_constraint(
        vault,
        markets.clone(),
        liquidity_mints.clone(),
        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ));
    for lane in swap_lanes {
        match lane {
            SwapLane::Jupiter => {
                constraints.push(jupiter_route_stable_swap_minimal_instruction_constraint(
                    vault,
                    allowed_swap_mints.clone(),
                ))
            }
            SwapLane::LoyalHub {
                hub_authorizer,
                max_fee_bps,
            } => constraints.push(loyal_hub_route_stable_swap_instruction_constraint(
                vault,
                allowed_swap_mints.clone(),
                hub_authorizer,
                max_fee_bps,
            )),
        }
    }
    constraints.push(kamino_route_market_mint_instruction_constraint(
        vault,
        markets,
        liquidity_mints,
        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ));

    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        constraints,
    )
}

pub fn create_squads_program_interaction_route_all_in_one_mint_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    liquidity_mints: Vec<Pubkey>,
    allowed_swap_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
) -> Instruction {
    assert!(
        !swap_lanes.is_empty(),
        "yield route all-in-one policy needs at least one swap lane"
    );
    let mut constraints = Vec::with_capacity(2 + swap_lanes.len());
    constraints.push(kamino_route_mint_instruction_constraint(
        vault,
        liquidity_mints.clone(),
        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ));
    for lane in swap_lanes {
        match lane {
            SwapLane::Jupiter => {
                constraints.push(jupiter_route_stable_swap_minimal_instruction_constraint(
                    vault,
                    allowed_swap_mints.clone(),
                ))
            }
            SwapLane::LoyalHub {
                hub_authorizer,
                max_fee_bps,
            } => constraints.push(loyal_hub_route_stable_swap_instruction_constraint(
                vault,
                allowed_swap_mints.clone(),
                hub_authorizer,
                max_fee_bps,
            )),
        }
    }
    constraints.push(kamino_route_mint_instruction_constraint(
        vault,
        liquidity_mints,
        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ));

    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        constraints,
    )
}
