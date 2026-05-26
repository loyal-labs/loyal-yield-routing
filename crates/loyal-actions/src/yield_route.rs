use crate::squads::{
    create_program_interaction_action_instruction, LoyalActionError, Result,
    SquadsAccountConstraint, SquadsAccountConstraintType, SquadsDataConstraint, SquadsDataOperator,
    SquadsDataValue, SquadsInstructionConstraint,
};
use crate::*;
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct LoyalActionContext {
    pub settings: Pubkey,
    pub authority: Pubkey,
    pub delegated_signer: Pubkey,
    pub account_index: u8,
    pub vault: Pubkey,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct YieldRouteUniverse {
    pub stable_mints: Vec<Pubkey>,
    pub kamino_markets: Vec<Pubkey>,
    pub kamino_liquidity_mints: Vec<Pubkey>,
}

impl YieldRouteUniverse {
    pub fn new(
        stable_mints: Vec<Pubkey>,
        kamino_markets: Vec<Pubkey>,
        kamino_liquidity_mints: Vec<Pubkey>,
    ) -> Self {
        Self {
            stable_mints: unique_pubkeys(stable_mints),
            kamino_markets: unique_pubkeys(kamino_markets),
            kamino_liquidity_mints: unique_pubkeys(kamino_liquidity_mints),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SwapLane {
    Jupiter,
    LoyalHub {
        hub_authorizer: Pubkey,
        max_fee_bps: u16,
    },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct YieldRouteActionSeeds {
    pub withdraw: u64,
    pub swap: u64,
    pub deposit: u64,
}

impl Default for YieldRouteActionSeeds {
    fn default() -> Self {
        Self {
            withdraw: YIELD_ROUTE_WITHDRAW_ACTION_SEED,
            swap: YIELD_ROUTE_SWAP_ACTION_SEED,
            deposit: YIELD_ROUTE_DEPOSIT_ACTION_SEED,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct YieldRouteActionAccounts {
    pub withdraw: Pubkey,
    pub swap: Pubkey,
    pub deposit: Pubkey,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct YieldRouteActionSetup {
    pub accounts: YieldRouteActionAccounts,
    pub instructions: Vec<Instruction>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct YieldRouteActionInstruction {
    pub account: Pubkey,
    pub instruction: Instruction,
}

pub fn create_three_step_yield_route_actions(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
) -> Result<YieldRouteActionSetup> {
    create_three_step_yield_route_actions_with_swap_lanes(
        context,
        universe,
        vec![SwapLane::Jupiter],
        YieldRouteActionSeeds::default(),
    )
}

pub fn create_three_step_yield_route_actions_with_swap_lanes(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    swap_lanes: Vec<SwapLane>,
    seeds: YieldRouteActionSeeds,
) -> Result<YieldRouteActionSetup> {
    validate_swap_lanes(&swap_lanes)?;
    let accounts = action_accounts(context.settings, seeds);

    Ok(YieldRouteActionSetup {
        accounts,
        instructions: vec![
            kamino_action_instruction(
                context,
                seeds.withdraw,
                vec![kamino_market_mint_constraint(
                    context.vault,
                    universe.kamino_markets.clone(),
                    universe.kamino_liquidity_mints.clone(),
                    KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
                )],
            ),
            stable_swap_action_instruction(
                context,
                seeds.swap,
                universe.stable_mints.clone(),
                swap_lanes,
                false,
            )?,
            kamino_action_instruction(
                context,
                seeds.deposit,
                vec![kamino_market_mint_constraint(
                    context.vault,
                    universe.kamino_markets,
                    universe.kamino_liquidity_mints,
                    KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
                )],
            ),
        ],
    })
}

pub fn create_combined_kamino_yield_route_actions(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
) -> Result<YieldRouteActionSetup> {
    create_combined_kamino_yield_route_actions_with_swap_lanes(
        context,
        universe,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_combined_kamino_yield_route_actions_with_swap_lanes(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    swap_lanes: Vec<SwapLane>,
) -> Result<YieldRouteActionSetup> {
    validate_swap_lanes(&swap_lanes)?;
    let rebalance_seed = YIELD_ROUTE_WITHDRAW_ACTION_SEED;
    let rebalance = derive_action_account(&context.settings, rebalance_seed).0;

    Ok(YieldRouteActionSetup {
        accounts: YieldRouteActionAccounts {
            withdraw: rebalance,
            swap: derive_action_account(&context.settings, YIELD_ROUTE_SWAP_ACTION_SEED).0,
            deposit: rebalance,
        },
        instructions: vec![
            kamino_action_instruction(
                context,
                rebalance_seed,
                vec![
                    kamino_market_mint_constraint(
                        context.vault,
                        universe.kamino_markets.clone(),
                        universe.kamino_liquidity_mints.clone(),
                        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
                    ),
                    kamino_market_mint_constraint(
                        context.vault,
                        universe.kamino_markets,
                        universe.kamino_liquidity_mints,
                        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
                    ),
                ],
            ),
            stable_swap_action_instruction(
                context,
                YIELD_ROUTE_SWAP_ACTION_SEED,
                universe.stable_mints,
                swap_lanes,
                false,
            )?,
        ],
    })
}

pub fn create_all_in_one_market_mint_yield_route_action(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
) -> Result<YieldRouteActionSetup> {
    create_all_in_one_market_mint_yield_route_action_with_swap_lanes(
        context,
        universe,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_all_in_one_market_mint_yield_route_action_with_swap_lanes(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    swap_lanes: Vec<SwapLane>,
) -> Result<YieldRouteActionSetup> {
    validate_swap_lanes(&swap_lanes)?;
    let seed = YIELD_ROUTE_WITHDRAW_ACTION_SEED;
    let action_account = derive_action_account(&context.settings, seed).0;
    let mut constraints = Vec::with_capacity(2 + swap_lanes.len());
    constraints.push(kamino_market_mint_constraint(
        context.vault,
        universe.kamino_markets.clone(),
        universe.kamino_liquidity_mints.clone(),
        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ));
    constraints.extend(stable_swap_constraints(
        context.vault,
        universe.stable_mints,
        swap_lanes,
        true,
    )?);
    constraints.push(kamino_market_mint_constraint(
        context.vault,
        universe.kamino_markets,
        universe.kamino_liquidity_mints,
        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ));

    Ok(YieldRouteActionSetup {
        accounts: YieldRouteActionAccounts {
            withdraw: action_account,
            swap: action_account,
            deposit: action_account,
        },
        instructions: vec![create_program_interaction_action_instruction(
            context.settings,
            context.authority,
            context.delegated_signer,
            seed,
            context.account_index,
            constraints,
        )],
    })
}

pub fn create_all_in_one_mint_yield_route_action(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
) -> Result<YieldRouteActionSetup> {
    create_all_in_one_mint_yield_route_action_with_swap_lanes(
        context,
        universe,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_all_in_one_mint_yield_route_action_with_swap_lanes(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    swap_lanes: Vec<SwapLane>,
) -> Result<YieldRouteActionSetup> {
    validate_swap_lanes(&swap_lanes)?;
    let seed = YIELD_ROUTE_WITHDRAW_ACTION_SEED;
    let action_account = derive_action_account(&context.settings, seed).0;
    let mut constraints = Vec::with_capacity(2 + swap_lanes.len());
    constraints.push(kamino_mint_constraint(
        context.vault,
        universe.kamino_liquidity_mints.clone(),
        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ));
    constraints.extend(stable_swap_constraints(
        context.vault,
        universe.stable_mints,
        swap_lanes,
        true,
    )?);
    constraints.push(kamino_mint_constraint(
        context.vault,
        universe.kamino_liquidity_mints,
        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ));

    Ok(YieldRouteActionSetup {
        accounts: YieldRouteActionAccounts {
            withdraw: action_account,
            swap: action_account,
            deposit: action_account,
        },
        instructions: vec![create_program_interaction_action_instruction(
            context.settings,
            context.authority,
            context.delegated_signer,
            seed,
            context.account_index,
            constraints,
        )],
    })
}

pub fn create_swap_yield_route_action(
    context: LoyalActionContext,
    stable_mints: Vec<Pubkey>,
) -> Result<YieldRouteActionInstruction> {
    create_swap_yield_route_action_with_swap_lanes(
        context,
        stable_mints,
        vec![SwapLane::Jupiter],
        YIELD_ROUTE_STANDALONE_ACTION_SEED,
    )
}

pub fn create_swap_yield_route_action_with_swap_lanes(
    context: LoyalActionContext,
    stable_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
    action_seed: u64,
) -> Result<YieldRouteActionInstruction> {
    validate_swap_lanes(&swap_lanes)?;
    Ok(YieldRouteActionInstruction {
        account: derive_action_account(&context.settings, action_seed).0,
        instruction: stable_swap_action_instruction(
            context,
            action_seed,
            unique_pubkeys(stable_mints),
            swap_lanes,
            false,
        )?,
    })
}

fn action_accounts(settings: Pubkey, seeds: YieldRouteActionSeeds) -> YieldRouteActionAccounts {
    YieldRouteActionAccounts {
        withdraw: derive_action_account(&settings, seeds.withdraw).0,
        swap: derive_action_account(&settings, seeds.swap).0,
        deposit: derive_action_account(&settings, seeds.deposit).0,
    }
}

fn kamino_action_instruction(
    context: LoyalActionContext,
    action_seed: u64,
    constraints: Vec<SquadsInstructionConstraint>,
) -> Instruction {
    create_program_interaction_action_instruction(
        context.settings,
        context.authority,
        context.delegated_signer,
        action_seed,
        context.account_index,
        constraints,
    )
}

fn stable_swap_action_instruction(
    context: LoyalActionContext,
    action_seed: u64,
    allowed_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
    minimal_jupiter: bool,
) -> Result<Instruction> {
    Ok(create_program_interaction_action_instruction(
        context.settings,
        context.authority,
        context.delegated_signer,
        action_seed,
        context.account_index,
        stable_swap_constraints(context.vault, allowed_mints, swap_lanes, minimal_jupiter)?,
    ))
}

fn stable_swap_constraints(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
    minimal_jupiter: bool,
) -> Result<Vec<SquadsInstructionConstraint>> {
    validate_swap_lanes(&swap_lanes)?;
    let allowed_mints = unique_pubkeys(allowed_mints);
    let mut constraints = Vec::with_capacity(swap_lanes.len());
    for lane in swap_lanes {
        match lane {
            SwapLane::Jupiter if minimal_jupiter => {
                constraints.push(jupiter_minimal_constraint(vault, allowed_mints.clone()))
            }
            SwapLane::Jupiter => constraints.push(jupiter_constraint(vault, allowed_mints.clone())),
            SwapLane::LoyalHub {
                hub_authorizer,
                max_fee_bps,
            } => constraints.push(loyal_hub_constraint(
                vault,
                allowed_mints.clone(),
                hub_authorizer,
                max_fee_bps,
            )),
        }
    }
    Ok(constraints)
}

fn validate_swap_lanes(swap_lanes: &[SwapLane]) -> Result<()> {
    if swap_lanes.is_empty() {
        return Err(LoyalActionError::EmptySwapLanes);
    }
    Ok(())
}

fn kamino_market_mint_constraint(
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            pubkey_constraint(2, unique_pubkeys(markets), None),
            pubkey_constraint(3, unique_pubkeys(liquidity_mints), Some(spl_token::id())),
            pubkey_constraint(10, vec![spl_token::id()], None),
        ],
        data_constraints: discriminator_constraint(discriminator),
    }
}

fn kamino_mint_constraint(
    vault: Pubkey,
    liquidity_mints: Vec<Pubkey>,
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            pubkey_constraint(3, unique_pubkeys(liquidity_mints), Some(spl_token::id())),
            pubkey_constraint(10, vec![spl_token::id()], None),
        ],
        data_constraints: discriminator_constraint(discriminator),
    }
}

fn jupiter_constraint(vault: Pubkey, allowed_mints: Vec<Pubkey>) -> SquadsInstructionConstraint {
    let allowed_mints = unique_pubkeys(allowed_mints);
    SquadsInstructionConstraint {
        program_id: JUPITER_V6_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            account_data_constraint(1, Some(spl_token::id())),
            account_data_constraint(2, Some(spl_token::id())),
            pubkey_constraint(3, allowed_mints.clone(), Some(spl_token::id())),
            pubkey_constraint(4, allowed_mints, Some(spl_token::id())),
            pubkey_constraint(5, vec![spl_token::id()], None),
            account_data_constraint(6, Some(spl_token::id())),
            account_data_constraint(7, Some(spl_token::id())),
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8(MOCK_JUPITER_STABLE_EXACT_IN),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

fn jupiter_minimal_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    let allowed_mints = unique_pubkeys(allowed_mints);
    SquadsInstructionConstraint {
        program_id: JUPITER_V6_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vec![vault], None),
            account_data_constraint(1, Some(spl_token::id())),
            account_data_constraint(2, Some(spl_token::id())),
            pubkey_constraint(3, allowed_mints.clone(), Some(spl_token::id())),
            pubkey_constraint(4, allowed_mints, Some(spl_token::id())),
            pubkey_constraint(5, vec![spl_token::id()], None),
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8(MOCK_JUPITER_STABLE_EXACT_IN),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

fn loyal_hub_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
) -> SquadsInstructionConstraint {
    let allowed_mints = unique_pubkeys(allowed_mints);
    let inventory_accounts = allowed_mints
        .iter()
        .map(|mint| derive_loyal_hub_inventory_account(*mint))
        .collect::<Vec<_>>();
    SquadsInstructionConstraint {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(
                0,
                vec![derive_loyal_hub_config()],
                Some(LOYAL_HUB_SWAP_PROGRAM_ID),
            ),
            pubkey_constraint(1, vec![vault], None),
            pubkey_constraint(4, inventory_accounts.clone(), Some(spl_token::id())),
            pubkey_constraint(5, inventory_accounts, Some(spl_token::id())),
            pubkey_constraint(6, allowed_mints.clone(), Some(spl_token::id())),
            pubkey_constraint(7, allowed_mints, Some(spl_token::id())),
            pubkey_constraint(9, vec![hub_authorizer], None),
            pubkey_constraint(10, vec![spl_token::id()], None),
        ],
        data_constraints: vec![
            SquadsDataConstraint {
                data_offset: 0,
                data_value: SquadsDataValue::U8(LOYAL_HUB_SWAP_EXACT_IN),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: 25,
                data_value: SquadsDataValue::U16Le(max_fee_bps),
                operator: SquadsDataOperator::LessThanOrEqualTo,
            },
        ],
    }
}

fn pubkey_constraint(
    account_index: u8,
    pubkeys: Vec<Pubkey>,
    owner: Option<Pubkey>,
) -> SquadsAccountConstraint {
    SquadsAccountConstraint {
        account_index,
        account_constraint: SquadsAccountConstraintType::Pubkey(pubkeys),
        owner,
    }
}

fn account_data_constraint(account_index: u8, owner: Option<Pubkey>) -> SquadsAccountConstraint {
    SquadsAccountConstraint {
        account_index,
        account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
        owner,
    }
}

fn discriminator_constraint(discriminator: [u8; 8]) -> Vec<SquadsDataConstraint> {
    vec![SquadsDataConstraint {
        data_offset: 0,
        data_value: SquadsDataValue::U8Slice(discriminator.to_vec()),
        operator: SquadsDataOperator::Equals,
    }]
}

pub fn derive_loyal_hub_config() -> Pubkey {
    Pubkey::find_program_address(&[LOYAL_HUB_CONFIG_SEED], &LOYAL_HUB_SWAP_PROGRAM_ID).0
}

pub fn derive_loyal_hub_authority() -> Pubkey {
    Pubkey::find_program_address(&[LOYAL_HUB_AUTHORITY_SEED], &LOYAL_HUB_SWAP_PROGRAM_ID).0
}

pub fn derive_loyal_hub_inventory_account(mint: Pubkey) -> Pubkey {
    let hub_authority = derive_loyal_hub_authority();
    Pubkey::find_program_address(
        &[
            hub_authority.as_ref(),
            spl_token::id().as_ref(),
            mint.as_ref(),
        ],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
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

#[cfg(test)]
mod tests {
    use super::*;

    fn context() -> LoyalActionContext {
        LoyalActionContext {
            settings: Pubkey::new_unique(),
            authority: Pubkey::new_unique(),
            delegated_signer: Pubkey::new_unique(),
            account_index: 0,
            vault: Pubkey::new_unique(),
        }
    }

    fn universe() -> YieldRouteUniverse {
        let stable = Pubkey::new_unique();
        let market = Pubkey::new_unique();
        let mint = Pubkey::new_unique();
        YieldRouteUniverse::new(vec![stable, stable], vec![market, market], vec![mint, mint])
    }

    #[test]
    fn deduplicates_route_universe_in_order() {
        let stable = Pubkey::new_unique();
        let market = Pubkey::new_unique();
        let mint = Pubkey::new_unique();

        let universe =
            YieldRouteUniverse::new(vec![stable, stable], vec![market, market], vec![mint, mint]);

        assert_eq!(universe.stable_mints, vec![stable]);
        assert_eq!(universe.kamino_markets, vec![market]);
        assert_eq!(universe.kamino_liquidity_mints, vec![mint]);
    }

    #[test]
    fn rejects_empty_swap_lanes() {
        let result = create_all_in_one_market_mint_yield_route_action_with_swap_lanes(
            context(),
            universe(),
            vec![],
        );

        assert_eq!(result.unwrap_err(), LoyalActionError::EmptySwapLanes);
    }

    #[test]
    fn builds_one_all_in_one_market_mint_action() {
        let context = context();
        let setup = create_all_in_one_market_mint_yield_route_action(context, universe()).unwrap();

        assert_eq!(setup.instructions.len(), 1);
        assert_eq!(setup.accounts.withdraw, setup.accounts.swap);
        assert_eq!(setup.accounts.swap, setup.accounts.deposit);
        assert_eq!(
            setup.accounts.withdraw,
            derive_action_account(&context.settings, YIELD_ROUTE_WITHDRAW_ACTION_SEED).0
        );
        assert_eq!(
            setup.instructions[0].program_id,
            SQUADS_SMART_ACCOUNT_PROGRAM_ID
        );
        assert_eq!(setup.instructions[0].accounts.len(), 6);
    }

    #[test]
    fn builds_three_step_action_accounts_from_default_seeds() {
        let context = context();
        let setup = create_three_step_yield_route_actions(context, universe()).unwrap();

        assert_eq!(setup.instructions.len(), 3);
        assert_eq!(
            setup.accounts.withdraw,
            derive_action_account(&context.settings, YIELD_ROUTE_WITHDRAW_ACTION_SEED).0
        );
        assert_eq!(
            setup.accounts.swap,
            derive_action_account(&context.settings, YIELD_ROUTE_SWAP_ACTION_SEED).0
        );
        assert_eq!(
            setup.accounts.deposit,
            derive_action_account(&context.settings, YIELD_ROUTE_DEPOSIT_ACTION_SEED).0
        );
    }

    #[test]
    fn builds_swap_action_with_loyal_hub_lane() {
        let context = context();
        let setup = create_swap_yield_route_action_with_swap_lanes(
            context,
            vec![Pubkey::new_unique(), Pubkey::new_unique()],
            vec![
                SwapLane::Jupiter,
                SwapLane::LoyalHub {
                    hub_authorizer: Pubkey::new_unique(),
                    max_fee_bps: 50,
                },
            ],
            YIELD_ROUTE_STANDALONE_ACTION_SEED,
        )
        .unwrap();

        assert_eq!(
            setup.account,
            derive_action_account(&context.settings, YIELD_ROUTE_STANDALONE_ACTION_SEED).0
        );
        assert_eq!(
            setup.instruction.program_id,
            SQUADS_SMART_ACCOUNT_PROGRAM_ID
        );
    }
}
