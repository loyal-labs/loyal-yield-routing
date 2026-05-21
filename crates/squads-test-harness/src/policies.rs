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

pub fn create_squads_spending_limit_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    source_account_index: u8,
    destination: Pubkey,
    max_per_period_lamports: u64,
    max_per_use_lamports: u64,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::SpendingLimit(
            SquadsSpendingLimitPolicyCreationPayload {
                mint: Pubkey::default(),
                source_account_index,
                time_constraints: SquadsTimeConstraints {
                    start: 0,
                    expiration: None,
                    period: SquadsPeriodV2::OneTime,
                    accumulate_unused: false,
                },
                quantity_constraints: SquadsQuantityConstraints {
                    max_per_period: max_per_period_lamports,
                    max_per_use: max_per_use_lamports,
                    enforce_exact_quantity: false,
                },
                usage_state: None,
                destinations: vec![destination],
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

pub fn create_squads_program_interaction_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    usdc_ledger: Pubkey,
    pyusd_ledger: Pubkey,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let jupiter_accounts = mock_jupiter_token_accounts();
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::LegacyProgramInteraction(
            SquadsProgramInteractionPolicyCreationPayloadLegacy {
                account_index,
                instructions_constraints: vec![SquadsInstructionConstraint {
                    program_id: JUPITER_V6_PROGRAM_ID,
                    account_constraints: vec![
                        SquadsAccountConstraint {
                            account_index: 0,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                            owner: None,
                        },
                        SquadsAccountConstraint {
                            account_index: 1,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                usdc_ledger,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 2,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                pyusd_ledger,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 3,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                USDC_MINT,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 4,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                PYUSD_MINT,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 5,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                spl_token::id(),
                            ]),
                            owner: None,
                        },
                        SquadsAccountConstraint {
                            account_index: 6,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                jupiter_accounts.usdc_reserve,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 7,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                jupiter_accounts.pyusd_reserve,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 8,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                jupiter_accounts.authority,
                            ]),
                            owner: None,
                        },
                    ],
                    data_constraints: vec![
                        SquadsDataConstraint {
                            data_offset: 0,
                            data_value: SquadsDataValue::U8(MOCK_JUPITER_USDC_TO_PYUSD),
                            operator: SquadsDataOperator::Equals,
                        },
                        SquadsDataConstraint {
                            data_offset: 9,
                            data_value: SquadsDataValue::U8Slice(USDC_MINT.to_bytes().to_vec()),
                            operator: SquadsDataOperator::Equals,
                        },
                        SquadsDataConstraint {
                            data_offset: 41,
                            data_value: SquadsDataValue::U8Slice(PYUSD_MINT.to_bytes().to_vec()),
                            operator: SquadsDataOperator::Equals,
                        },
                    ],
                }],
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

pub fn create_squads_program_interaction_jupiter_fixture_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    usdc_ledger: Pubkey,
    pyusd_ledger: Pubkey,
    swap_instruction_data: &[u8],
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![jupiter_fixture_swap_instruction_constraint(
            vault,
            usdc_ledger,
            pyusd_ledger,
            swap_instruction_data.to_vec(),
        )],
    )
}

fn jupiter_fixture_swap_instruction_constraint(
    vault: Pubkey,
    usdc_ledger: Pubkey,
    pyusd_ledger: Pubkey,
    swap_instruction_data: Vec<u8>,
) -> SquadsInstructionConstraint {
    let jupiter_accounts = mock_jupiter_token_accounts();

    SquadsInstructionConstraint {
        program_id: JUPITER_V6_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![usdc_ledger]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![pyusd_ledger]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![USDC_MINT]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![PYUSD_MINT]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    jupiter_accounts.usdc_reserve,
                ]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    jupiter_accounts.pyusd_reserve,
                ]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    jupiter_accounts.authority,
                ]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(swap_instruction_data),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

pub fn create_squads_program_interaction_allowed_jupiter_stable_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    vault_token_accounts: Vec<Pubkey>,
    stable_reserves: Vec<MockJupiterStableReserveTokenAccount>,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![jupiter_allowed_stable_swap_instruction_constraint(
            vault,
            vault_token_accounts,
            stable_reserves,
        )],
    )
}

pub fn create_squads_program_interaction_route_jupiter_stable_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
) -> Instruction {
    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![jupiter_route_stable_swap_instruction_constraint(
            vault,
            allowed_mints,
        )],
    )
}

pub fn create_squads_program_interaction_route_stable_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
) -> Instruction {
    assert!(
        !swap_lanes.is_empty(),
        "yield route swap policy needs at least one swap lane"
    );
    let mut constraints = Vec::with_capacity(swap_lanes.len());
    for lane in swap_lanes {
        match lane {
            SwapLane::Jupiter => constraints.push(
                jupiter_route_stable_swap_instruction_constraint(vault, allowed_mints.clone()),
            ),
            SwapLane::LoyalHub {
                hub_authorizer,
                max_fee_bps,
            } => constraints.push(loyal_hub_route_stable_swap_instruction_constraint(
                vault,
                allowed_mints.clone(),
                hub_authorizer,
                max_fee_bps,
            )),
        }
    }

    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        constraints,
    )
}

pub fn create_squads_program_interaction_mock_jupiter_stable_swap_policy_instruction(
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
        vec![SquadsInstructionConstraint {
            program_id: JUPITER_V6_PROGRAM_ID,
            account_constraints: vec![
                SquadsAccountConstraint {
                    account_index: 0,
                    account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                    owner: None,
                },
                SquadsAccountConstraint {
                    account_index: 1,
                    account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                    owner: Some(spl_token::id()),
                },
                SquadsAccountConstraint {
                    account_index: 2,
                    account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                    owner: Some(spl_token::id()),
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
                    account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                    owner: None,
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
                    account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                        derive_mock_jupiter_swap_authority(),
                    ]),
                    owner: None,
                },
            ],
            data_constraints: vec![SquadsDataConstraint {
                data_offset: 0,
                data_value: SquadsDataValue::U8(MOCK_JUPITER_STABLE_EXACT_IN),
                operator: SquadsDataOperator::Equals,
            }],
        }],
    )
}

fn jupiter_route_stable_swap_instruction_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: JUPITER_V6_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(allowed_mints.clone()),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::Pubkey(allowed_mints),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
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
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    derive_mock_jupiter_swap_authority(),
                ]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8(MOCK_JUPITER_STABLE_EXACT_IN),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

fn loyal_hub_route_stable_swap_instruction_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    derive_loyal_hub_config(),
                ]),
                owner: Some(LOYAL_HUB_SWAP_PROGRAM_ID),
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
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
                account_constraint: SquadsAccountConstraintType::Pubkey(allowed_mints.clone()),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::Pubkey(allowed_mints),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    derive_loyal_hub_authority(),
                ]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 9,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![hub_authorizer]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
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

fn jupiter_allowed_stable_swap_instruction_constraint(
    vault: Pubkey,
    vault_token_accounts: Vec<Pubkey>,
    stable_reserves: Vec<MockJupiterStableReserveTokenAccount>,
) -> SquadsInstructionConstraint {
    let mints = stable_reserves
        .iter()
        .map(|stable_reserve| stable_reserve.mint)
        .collect::<Vec<_>>();
    let reserve_token_accounts = stable_reserves
        .iter()
        .map(|stable_reserve| stable_reserve.reserve)
        .collect::<Vec<_>>();

    SquadsInstructionConstraint {
        program_id: JUPITER_V6_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    vault_token_accounts.clone(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(vault_token_accounts),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(mints.clone()),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::Pubkey(mints),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserve_token_accounts.clone(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::Pubkey(reserve_token_accounts),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    derive_mock_jupiter_swap_authority(),
                ]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8(MOCK_JUPITER_STABLE_EXACT_IN),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

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

fn kamino_route_market_mint_instruction_constraint(
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
                        .map(|reserve| reserve.vault_collateral)
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

fn create_squads_compact_program_interaction_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::ProgramInteraction(
            compile_squads_program_interaction_policy_creation_payload(account_index, constraints),
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

fn compile_squads_program_interaction_policy_creation_payload(
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
) -> SquadsProgramInteractionPolicyCreationPayload {
    let mut pubkey_table = Vec::new();
    let instructions_constraints = constraints
        .into_iter()
        .map(|constraint| compile_squads_instruction_constraint(constraint, &mut pubkey_table))
        .collect::<Vec<_>>();

    SquadsProgramInteractionPolicyCreationPayload {
        account_index,
        pubkey_table: pubkey_table.into(),
        instructions_constraints: instructions_constraints.into(),
        pre_hook: None,
        post_hook: None,
        spending_limits: Vec::<SquadsCompiledLimitedSpendingLimit>::new().into(),
    }
}

fn compile_squads_instruction_constraint(
    constraint: SquadsInstructionConstraint,
    pubkey_table: &mut Vec<Pubkey>,
) -> SquadsCompiledInstructionConstraint {
    SquadsCompiledInstructionConstraint {
        program_id_index: squads_pubkey_table_index(pubkey_table, constraint.program_id),
        account_constraints: constraint
            .account_constraints
            .into_iter()
            .map(|account_constraint| {
                compile_squads_account_constraint(account_constraint, pubkey_table)
            })
            .collect::<Vec<_>>()
            .into(),
        data_constraints: constraint.data_constraints.into(),
    }
}

fn compile_squads_account_constraint(
    constraint: SquadsAccountConstraint,
    pubkey_table: &mut Vec<Pubkey>,
) -> SquadsCompiledAccountConstraint {
    SquadsCompiledAccountConstraint {
        account_index: constraint.account_index,
        account_constraint: match constraint.account_constraint {
            SquadsAccountConstraintType::Pubkey(pubkeys) => {
                SquadsCompiledAccountConstraintType::Pubkey(
                    pubkeys
                        .into_iter()
                        .map(|pubkey| squads_pubkey_table_index(pubkey_table, pubkey))
                        .collect::<Vec<_>>()
                        .into(),
                )
            }
            SquadsAccountConstraintType::AccountData(data_constraints) => {
                SquadsCompiledAccountConstraintType::AccountData(data_constraints.into())
            }
        },
        owner_index: constraint
            .owner
            .map(|owner| squads_pubkey_table_index(pubkey_table, owner)),
    }
}

fn squads_pubkey_table_index(pubkey_table: &mut Vec<Pubkey>, pubkey: Pubkey) -> u8 {
    if let Some(index) = pubkey_table.iter().position(|existing| *existing == pubkey) {
        return index.try_into().expect("pubkey table index fits in u8");
    }

    assert!(
        pubkey_table.len() < 240,
        "Squads ProgramInteraction pubkey table supports up to 240 custom pubkeys"
    );
    let index = pubkey_table.len();
    pubkey_table.push(pubkey);
    index.try_into().expect("pubkey table index fits in u8")
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

fn create_squads_program_interaction_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::LegacyProgramInteraction(
            SquadsProgramInteractionPolicyCreationPayloadLegacy {
                account_index,
                instructions_constraints: constraints,
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

pub fn remove_squads_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    policy: Pubkey,
) -> Instruction {
    let action = SquadsSettingsAction::PolicyRemove { policy };

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
