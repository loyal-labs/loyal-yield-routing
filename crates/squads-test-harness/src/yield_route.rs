use crate::*;
use solana_sdk::pubkey::Pubkey;

struct YieldRoutePolicyContext {
    settings: Pubkey,
    authority: Pubkey,
    account_index: u8,
    vault: Pubkey,
    stable_mints: Vec<Pubkey>,
    kamino_reserves: Vec<MockKaminoReserveTokenAccounts>,
}

impl YieldRoutePolicyContext {
    fn new(context: &FundedSquadsTestContext, whitelist: SquadsYieldRoutePolicyWhitelist) -> Self {
        Self {
            settings: context.pool.settings,
            authority: context.wallet_pubkey(),
            account_index: context.vault_index,
            vault: context.vault,
            stable_mints: unique_pubkeys(whitelist.stable_mints),
            kamino_reserves: unique_kamino_reserves(whitelist.kamino_reserves),
        }
    }

    fn policy(&self, seed: u64) -> Pubkey {
        derive_squads_policy(&self.settings, seed).0
    }

    fn kamino_markets(&self) -> Vec<Pubkey> {
        unique_pubkeys(
            self.kamino_reserves
                .iter()
                .map(|reserve| reserve.market)
                .collect::<Vec<_>>(),
        )
    }

    fn kamino_mints(&self) -> Vec<Pubkey> {
        unique_pubkeys(
            self.kamino_reserves
                .iter()
                .map(|reserve| reserve.liquidity_mint)
                .collect::<Vec<_>>(),
        )
    }
}

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
    let route = YieldRoutePolicyContext::new(context, whitelist);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw: route.policy(seeds.withdraw),
            swap: route.policy(seeds.swap),
            deposit: route.policy(seeds.deposit),
        },
        instructions: vec![
            create_squads_program_interaction_route_kamino_withdraw_policy_instruction(
                route.settings,
                route.authority,
                delegated_signer,
                seeds.withdraw,
                route.account_index,
                route.vault,
                route.kamino_reserves.clone(),
            ),
            create_squads_program_interaction_route_stable_swap_policy_instruction(
                route.settings,
                route.authority,
                delegated_signer,
                seeds.swap,
                route.account_index,
                route.vault,
                route.stable_mints,
                swap_lanes,
            ),
            create_squads_program_interaction_route_kamino_deposit_policy_instruction(
                route.settings,
                route.authority,
                delegated_signer,
                seeds.deposit,
                route.account_index,
                route.vault,
                route.kamino_reserves,
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
    let route = YieldRoutePolicyContext::new(context, whitelist);
    let rebalance_seed = YIELD_ROUTE_WITHDRAW_POLICY_SEED;
    let rebalance = route.policy(rebalance_seed);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw: rebalance,
            swap: route.policy(YIELD_ROUTE_SWAP_POLICY_SEED),
            deposit: rebalance,
        },
        instructions: vec![
            create_squads_program_interaction_route_kamino_rebalance_policy_instruction(
                route.settings,
                route.authority,
                delegated_signer,
                rebalance_seed,
                route.account_index,
                route.vault,
                route.kamino_reserves,
            ),
            create_squads_program_interaction_route_stable_swap_policy_instruction(
                route.settings,
                route.authority,
                delegated_signer,
                YIELD_ROUTE_SWAP_POLICY_SEED,
                route.account_index,
                route.vault,
                route.stable_mints,
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
    let route = YieldRoutePolicyContext::new(context, whitelist);
    let kamino_markets = route.kamino_markets();
    let kamino_mints = route.kamino_mints();
    let rebalance_seed = YIELD_ROUTE_WITHDRAW_POLICY_SEED;
    let rebalance = route.policy(rebalance_seed);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw: rebalance,
            swap: route.policy(YIELD_ROUTE_SWAP_POLICY_SEED),
            deposit: rebalance,
        },
        instructions: vec![
            create_squads_program_interaction_route_kamino_market_mint_rebalance_policy_instruction(
                route.settings,
                route.authority,
                delegated_signer,
                rebalance_seed,
                route.account_index,
                route.vault,
                kamino_markets,
                kamino_mints,
            ),
            create_squads_program_interaction_route_stable_swap_policy_instruction(
                route.settings,
                route.authority,
                delegated_signer,
                YIELD_ROUTE_SWAP_POLICY_SEED,
                route.account_index,
                route.vault,
                route.stable_mints,
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
    let route = YieldRoutePolicyContext::new(context, whitelist);
    let kamino_markets = route.kamino_markets();
    let kamino_mints = route.kamino_mints();
    let seed = YIELD_ROUTE_WITHDRAW_POLICY_SEED;
    let policy = route.policy(seed);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw: policy,
            swap: policy,
            deposit: policy,
        },
        instructions: vec![
            create_squads_program_interaction_route_all_in_one_market_mint_policy_instruction(
                route.settings,
                route.authority,
                delegated_signer,
                seed,
                route.account_index,
                route.vault,
                kamino_markets,
                kamino_mints,
                route.stable_mints,
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
    let route = YieldRoutePolicyContext::new(context, whitelist);
    let kamino_mints = route.kamino_mints();
    let seed = YIELD_ROUTE_WITHDRAW_POLICY_SEED;
    let policy = route.policy(seed);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw: policy,
            swap: policy,
            deposit: policy,
        },
        instructions: vec![
            create_squads_program_interaction_route_all_in_one_mint_policy_instruction(
                route.settings,
                route.authority,
                delegated_signer,
                seed,
                route.account_index,
                route.vault,
                kamino_mints,
                route.stable_mints,
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
