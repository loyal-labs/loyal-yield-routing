#![allow(dead_code, unused_imports)]

use borsh::BorshSerialize;
use litesvm::LiteSVM;
use solana_sdk::{
    account::Account,
    hash::hashv,
    instruction::{AccountMeta, Instruction},
    message::Message,
    pubkey,
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
    transaction::Transaction,
};
use solana_system_interface::instruction as system_instruction;
use spl_token::solana_program::{program_option::COption, program_pack::Pack};
use std::{env, fs, io::Write, path::PathBuf};

use crate::types::*;
use crate::*;

pub fn create_squads_yield_route_policy_instructions(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
) -> SquadsYieldRoutePolicyInstructions {
    create_squads_yield_route_policy_instructions_with_swap_lanes(
        context,
        delegated_signer,
        whitelist,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_squads_yield_route_policy_instructions_with_swap_lanes(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
    swap_lanes: Vec<SwapLane>,
) -> SquadsYieldRoutePolicyInstructions {
    create_squads_yield_route_policy_instructions_with_seeds(
        context,
        delegated_signer,
        whitelist,
        swap_lanes,
        SquadsYieldRoutePolicySeeds::default(),
    )
}

pub fn create_squads_yield_route_policy_instructions_with_seeds(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
    swap_lanes: Vec<SwapLane>,
    seeds: SquadsYieldRoutePolicySeeds,
) -> SquadsYieldRoutePolicyInstructions {
    let settings = context.pool.settings;
    let authority = context.wallet_pubkey();
    let account_index = context.vault_index;
    let vault = context.vault;
    let stable_mints = unique_pubkeys(whitelist.stable_mints);
    let kamino_reserves = unique_kamino_reserves(whitelist.kamino_reserves);
    let (withdraw, _) = derive_squads_policy(&settings, seeds.withdraw);
    let (swap, _) = derive_squads_policy(&settings, seeds.swap);
    let (deposit, _) = derive_squads_policy(&settings, seeds.deposit);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw,
            swap,
            deposit,
        },
        instructions: vec![
            create_squads_program_interaction_route_kamino_withdraw_policy_instruction(
                settings,
                authority,
                delegated_signer,
                seeds.withdraw,
                account_index,
                vault,
                kamino_reserves.clone(),
            ),
            create_squads_program_interaction_route_stable_swap_policy_instruction(
                settings,
                authority,
                delegated_signer,
                seeds.swap,
                account_index,
                vault,
                stable_mints,
                swap_lanes,
            ),
            create_squads_program_interaction_route_kamino_deposit_policy_instruction(
                settings,
                authority,
                delegated_signer,
                seeds.deposit,
                account_index,
                vault,
                kamino_reserves,
            ),
        ],
    }
}

pub fn create_squads_yield_route_combined_kamino_policy_instructions(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
) -> SquadsYieldRoutePolicyInstructions {
    create_squads_yield_route_combined_kamino_policy_instructions_with_swap_lanes(
        context,
        delegated_signer,
        whitelist,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_squads_yield_route_combined_kamino_policy_instructions_with_swap_lanes(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
    swap_lanes: Vec<SwapLane>,
) -> SquadsYieldRoutePolicyInstructions {
    let settings = context.pool.settings;
    let authority = context.wallet_pubkey();
    let account_index = context.vault_index;
    let vault = context.vault;
    let stable_mints = unique_pubkeys(whitelist.stable_mints);
    let kamino_reserves = unique_kamino_reserves(whitelist.kamino_reserves);
    let rebalance_seed = YIELD_ROUTE_WITHDRAW_POLICY_SEED;
    let (rebalance, _) = derive_squads_policy(&settings, rebalance_seed);
    let (swap, _) = derive_squads_policy(&settings, YIELD_ROUTE_SWAP_POLICY_SEED);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw: rebalance,
            swap,
            deposit: rebalance,
        },
        instructions: vec![
            create_squads_program_interaction_route_kamino_rebalance_policy_instruction(
                settings,
                authority,
                delegated_signer,
                rebalance_seed,
                account_index,
                vault,
                kamino_reserves,
            ),
            create_squads_program_interaction_route_stable_swap_policy_instruction(
                settings,
                authority,
                delegated_signer,
                YIELD_ROUTE_SWAP_POLICY_SEED,
                account_index,
                vault,
                stable_mints,
                swap_lanes,
            ),
        ],
    }
}

pub fn create_squads_yield_route_market_mint_kamino_policy_instructions(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
) -> SquadsYieldRoutePolicyInstructions {
    create_squads_yield_route_market_mint_kamino_policy_instructions_with_swap_lanes(
        context,
        delegated_signer,
        whitelist,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_squads_yield_route_market_mint_kamino_policy_instructions_with_swap_lanes(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
    swap_lanes: Vec<SwapLane>,
) -> SquadsYieldRoutePolicyInstructions {
    let settings = context.pool.settings;
    let authority = context.wallet_pubkey();
    let account_index = context.vault_index;
    let vault = context.vault;
    let stable_mints = unique_pubkeys(whitelist.stable_mints);
    let kamino_reserves = unique_kamino_reserves(whitelist.kamino_reserves);
    let kamino_markets = unique_pubkeys(
        kamino_reserves
            .iter()
            .map(|reserve| reserve.market)
            .collect::<Vec<_>>(),
    );
    let kamino_mints = unique_pubkeys(
        kamino_reserves
            .iter()
            .map(|reserve| reserve.liquidity_mint)
            .collect::<Vec<_>>(),
    );
    let rebalance_seed = YIELD_ROUTE_WITHDRAW_POLICY_SEED;
    let (rebalance, _) = derive_squads_policy(&settings, rebalance_seed);
    let (swap, _) = derive_squads_policy(&settings, YIELD_ROUTE_SWAP_POLICY_SEED);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw: rebalance,
            swap,
            deposit: rebalance,
        },
        instructions: vec![
            create_squads_program_interaction_route_kamino_market_mint_rebalance_policy_instruction(
                settings,
                authority,
                delegated_signer,
                rebalance_seed,
                account_index,
                vault,
                kamino_markets,
                kamino_mints,
            ),
            create_squads_program_interaction_route_stable_swap_policy_instruction(
                settings,
                authority,
                delegated_signer,
                YIELD_ROUTE_SWAP_POLICY_SEED,
                account_index,
                vault,
                stable_mints,
                swap_lanes,
            ),
        ],
    }
}

pub fn create_squads_yield_route_all_in_one_market_mint_policy_instructions(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
) -> SquadsYieldRoutePolicyInstructions {
    create_squads_yield_route_all_in_one_market_mint_policy_instructions_with_swap_lanes(
        context,
        delegated_signer,
        whitelist,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_squads_yield_route_all_in_one_market_mint_policy_instructions_with_swap_lanes(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
    swap_lanes: Vec<SwapLane>,
) -> SquadsYieldRoutePolicyInstructions {
    let settings = context.pool.settings;
    let authority = context.wallet_pubkey();
    let account_index = context.vault_index;
    let vault = context.vault;
    let stable_mints = unique_pubkeys(whitelist.stable_mints);
    let kamino_reserves = unique_kamino_reserves(whitelist.kamino_reserves);
    let kamino_markets = unique_pubkeys(
        kamino_reserves
            .iter()
            .map(|reserve| reserve.market)
            .collect::<Vec<_>>(),
    );
    let kamino_mints = unique_pubkeys(
        kamino_reserves
            .iter()
            .map(|reserve| reserve.liquidity_mint)
            .collect::<Vec<_>>(),
    );
    let seed = YIELD_ROUTE_WITHDRAW_POLICY_SEED;
    let (policy, _) = derive_squads_policy(&settings, seed);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw: policy,
            swap: policy,
            deposit: policy,
        },
        instructions: vec![
            create_squads_program_interaction_route_all_in_one_market_mint_policy_instruction(
                settings,
                authority,
                delegated_signer,
                seed,
                account_index,
                vault,
                kamino_markets,
                kamino_mints,
                stable_mints,
                swap_lanes,
            ),
        ],
    }
}

pub fn create_squads_yield_route_all_in_one_mint_policy_instructions(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
) -> SquadsYieldRoutePolicyInstructions {
    create_squads_yield_route_all_in_one_mint_policy_instructions_with_swap_lanes(
        context,
        delegated_signer,
        whitelist,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_squads_yield_route_all_in_one_mint_policy_instructions_with_swap_lanes(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
    swap_lanes: Vec<SwapLane>,
) -> SquadsYieldRoutePolicyInstructions {
    let settings = context.pool.settings;
    let authority = context.wallet_pubkey();
    let account_index = context.vault_index;
    let vault = context.vault;
    let stable_mints = unique_pubkeys(whitelist.stable_mints);
    let kamino_reserves = unique_kamino_reserves(whitelist.kamino_reserves);
    let kamino_mints = unique_pubkeys(
        kamino_reserves
            .iter()
            .map(|reserve| reserve.liquidity_mint)
            .collect::<Vec<_>>(),
    );
    let seed = YIELD_ROUTE_WITHDRAW_POLICY_SEED;
    let (policy, _) = derive_squads_policy(&settings, seed);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw: policy,
            swap: policy,
            deposit: policy,
        },
        instructions: vec![
            create_squads_program_interaction_route_all_in_one_mint_policy_instruction(
                settings,
                authority,
                delegated_signer,
                seed,
                account_index,
                vault,
                kamino_mints,
                stable_mints,
                swap_lanes,
            ),
        ],
    }
}

pub fn create_squads_yield_route_swap_policy_instruction(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    stable_mints: Vec<Pubkey>,
) -> SquadsYieldRoutePolicyInstruction {
    create_squads_yield_route_swap_policy_instruction_with_swap_lanes(
        context,
        delegated_signer,
        stable_mints,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_squads_yield_route_swap_policy_instruction_with_swap_lanes(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    stable_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
) -> SquadsYieldRoutePolicyInstruction {
    create_squads_yield_route_swap_policy_instruction_with_seed(
        context,
        delegated_signer,
        stable_mints,
        swap_lanes,
        YIELD_ROUTE_STANDALONE_POLICY_SEED,
    )
}

pub fn create_squads_yield_route_swap_policy_instruction_with_seed(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    stable_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
    policy_seed: u64,
) -> SquadsYieldRoutePolicyInstruction {
    let settings = context.pool.settings;
    let (policy, _) = derive_squads_policy(&settings, policy_seed);

    SquadsYieldRoutePolicyInstruction {
        policy,
        instruction: create_squads_program_interaction_route_stable_swap_policy_instruction(
            settings,
            context.wallet_pubkey(),
            delegated_signer,
            policy_seed,
            context.vault_index,
            context.vault,
            unique_pubkeys(stable_mints),
            swap_lanes,
        ),
    }
}

fn unique_pubkeys(pubkeys: Vec<Pubkey>) -> Vec<Pubkey> {
    let mut unique = Vec::new();
    for pubkey in pubkeys {
        if !unique.contains(&pubkey) {
            unique.push(pubkey);
        }
    }
    unique
}

fn unique_kamino_reserves(
    reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> Vec<MockKaminoReserveTokenAccounts> {
    let mut unique = Vec::new();
    for reserve in reserves {
        if !unique
            .iter()
            .any(|existing: &MockKaminoReserveTokenAccounts| {
                existing.reserve == reserve.reserve && existing.market == reserve.market
            })
        {
            unique.push(reserve);
        }
    }
    unique
}
