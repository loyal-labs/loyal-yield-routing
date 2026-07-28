use super::common::{
    create_squads_compact_program_interaction_policy_instruction,
    create_squads_program_interaction_policy_instruction,
};
use crate::types::*;
use crate::*;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

fn kamino_reserve_instruction_constraint(
    reserve: Pubkey,
    market: Pubkey,
    liquidity_mint: Pubkey,
    data_constraint_value: Vec<u8>,
    vault: Pubkey,
    vault_liquidity_token_account: Pubkey,
    vault_collateral_token_account: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> SquadsInstructionConstraint {
    let collateral_mint = mock_kamino_collateral_mint(reserve);
    let reserve_liquidity_authority = derive_mock_kamino_reserve_liquidity_authority(reserve);
    let collateral_mint_authority = derive_mock_kamino_collateral_mint_authority(reserve);

    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![reserve]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![market]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![liquidity_mint]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    vault_liquidity_token_account,
                ]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    vault_collateral_token_account,
                ]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    reserve_liquidity_supply,
                ]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![collateral_mint]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    reserve_liquidity_authority,
                ]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 9,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    collateral_mint_authority,
                ]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(data_constraint_value),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

pub fn create_squads_program_interaction_allowed_kamino_reserves_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![
            kamino_allowed_reserves_instruction_constraint(
                vault,
                &reserves,
                KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
            kamino_allowed_reserves_instruction_constraint(
                vault,
                &reserves,
                KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
        ],
    )
}

pub fn create_squads_program_interaction_route_kamino_withdraw_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> Instruction {
    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![kamino_route_reserves_instruction_constraint(
            vault,
            &reserves,
            KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
        )],
    )
}

pub fn create_squads_program_interaction_route_kamino_deposit_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> Instruction {
    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![kamino_route_reserves_instruction_constraint(
            vault,
            &reserves,
            KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
        )],
    )
}

pub fn create_squads_program_interaction_route_kamino_rebalance_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> Instruction {
    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![
            kamino_route_reserves_instruction_constraint(
                vault,
                &reserves,
                KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
            kamino_route_reserves_instruction_constraint(
                vault,
                &reserves,
                KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
        ],
    )
}

pub fn create_squads_program_interaction_route_kamino_market_mint_rebalance_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
) -> Instruction {
    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![
            kamino_route_market_mint_instruction_constraint(
                vault,
                markets.clone(),
                liquidity_mints.clone(),
                KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
            kamino_route_market_mint_instruction_constraint(
                vault,
                markets,
                liquidity_mints,
                KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
        ],
    )
}

pub fn create_squads_program_interaction_mock_kamino_reserves_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![
            mock_kamino_reserve_instruction_constraint(
                vault,
                KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
            mock_kamino_reserve_instruction_constraint(
                vault,
                KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
        ],
    )
}

fn kamino_route_reserves_instruction_constraint(
    vault: Pubkey,
    reserves: &[MockKaminoReserveTokenAccounts],
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves.iter().map(|reserve| reserve.reserve).collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves.iter().map(|reserve| reserve.market).collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(discriminator.to_vec()),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

pub(super) fn kamino_route_market_mint_instruction_constraint(
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(markets),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(liquidity_mints),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(discriminator.to_vec()),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

fn mock_kamino_reserve_instruction_constraint(
    vault: Pubkey,
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 9,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(discriminator.to_vec()),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

fn kamino_allowed_reserves_instruction_constraint(
    vault: Pubkey,
    reserves: &[MockKaminoReserveTokenAccounts],
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves.iter().map(|reserve| reserve.reserve).collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves.iter().map(|reserve| reserve.market).collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.liquidity_mint)
                        .collect(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.vault_liquidity)
                        .collect(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.reserve_collateral_supply)
                        .collect(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.reserve_liquidity_supply)
                        .collect(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.collateral_mint)
                        .collect(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.reserve_liquidity_authority)
                        .collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 9,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.collateral_mint_authority)
                        .collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(discriminator.to_vec()),
            operator: SquadsDataOperator::Equals,
        }],
    }
}
fn kamino_usdc_reserve_instruction_constraint(
    discriminator: [u8; 8],
    vault: Pubkey,
    vault_usdc_token_account: Pubkey,
    vault_collateral_token_account: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> SquadsInstructionConstraint {
    kamino_reserve_instruction_constraint(
        KAMINO_MAIN_USDC_RESERVE,
        KAMINO_MAIN_MARKET,
        USDC_MINT,
        discriminator.to_vec(),
        vault,
        vault_usdc_token_account,
        vault_collateral_token_account,
        reserve_liquidity_supply,
    )
}

pub fn create_squads_program_interaction_kamino_usdc_reserve_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    vault_usdc_token_account: Pubkey,
    vault_collateral_token_account: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::LegacyProgramInteraction(
            SquadsProgramInteractionPolicyCreationPayloadLegacy {
                account_index,
                instructions_constraints: vec![
                    kamino_usdc_reserve_instruction_constraint(
                        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
                        vault,
                        vault_usdc_token_account,
                        vault_collateral_token_account,
                        reserve_liquidity_supply,
                    ),
                    kamino_usdc_reserve_instruction_constraint(
                        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
                        vault,
                        vault_usdc_token_account,
                        vault_collateral_token_account,
                        reserve_liquidity_supply,
                    ),
                ],
                pre_hook: None,
                post_hook: None,
                spending_limits: vec![],
            },
        ),
        signers: vec![SquadsSmartAccountSigner {
            key: delegated_signer,
            permissions: SquadsPermissions {
                mask: SQUADS_FULL_PERMISSIONS_MASK,
            },
        }],
        threshold: 1,
        time_lock: 0,
        start_timestamp: None,
        expiration_args: None,
    };

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_squads_sync_settings_transaction_args(vec![action]),
    }
}

pub fn create_squads_program_interaction_main_to_prime_usdc_route_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    vault_usdc_token_account: Pubkey,
    vault_main_usdc_collateral_token_account: Pubkey,
    main_usdc_reserve_liquidity_supply: Pubkey,
    vault_prime_usdc_collateral_token_account: Pubkey,
    prime_usdc_reserve_liquidity_supply: Pubkey,
    main_usdc_withdraw_data: Vec<u8>,
    prime_usdc_deposit_data: Vec<u8>,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![
            kamino_reserve_instruction_constraint(
                KAMINO_MAIN_USDC_RESERVE,
                KAMINO_MAIN_MARKET,
                USDC_MINT,
                main_usdc_withdraw_data,
                vault,
                vault_usdc_token_account,
                vault_main_usdc_collateral_token_account,
                main_usdc_reserve_liquidity_supply,
            ),
            kamino_reserve_instruction_constraint(
                KAMINO_PRIME_USDC_RESERVE,
                KAMINO_PRIME_MARKET,
                USDC_MINT,
                prime_usdc_deposit_data,
                vault,
                vault_usdc_token_account,
                vault_prime_usdc_collateral_token_account,
                prime_usdc_reserve_liquidity_supply,
            ),
        ],
    )
}

pub fn create_squads_program_interaction_prime_usdc_to_pyusd_reserves_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    vault_usdc_token_account: Pubkey,
    vault_pyusd_token_account: Pubkey,
    vault_prime_usdc_collateral_token_account: Pubkey,
    prime_usdc_reserve_liquidity_supply: Pubkey,
    vault_pyusd_collateral_token_account: Pubkey,
    pyusd_reserve_liquidity_supply: Pubkey,
    prime_usdc_withdraw_data: Vec<u8>,
    pyusd_deposit_data: Vec<u8>,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![
            kamino_reserve_instruction_constraint(
                KAMINO_PRIME_USDC_RESERVE,
                KAMINO_PRIME_MARKET,
                USDC_MINT,
                prime_usdc_withdraw_data,
                vault,
                vault_usdc_token_account,
                vault_prime_usdc_collateral_token_account,
                prime_usdc_reserve_liquidity_supply,
            ),
            kamino_reserve_instruction_constraint(
                KAMINO_MAIN_PYUSD_RESERVE,
                KAMINO_MAIN_MARKET,
                PYUSD_MINT,
                pyusd_deposit_data,
                vault,
                vault_pyusd_token_account,
                vault_pyusd_collateral_token_account,
                pyusd_reserve_liquidity_supply,
            ),
        ],
    )
}
