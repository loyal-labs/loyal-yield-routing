use super::common::{
    create_squads_compact_program_interaction_policy_instruction,
    create_squads_program_interaction_policy_instruction,
};
use crate::types::*;
use crate::*;
use loyal_actions::SwapLane;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

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
            SwapLane::Jupiter(_) => constraints.push(
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

pub(super) fn loyal_hub_route_stable_swap_instruction_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: loyal_actions::hub_abi::swap_exact_in_accounts::CONFIG,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    derive_loyal_hub_config(),
                ]),
                owner: Some(LOYAL_HUB_SWAP_PROGRAM_ID),
            },
            SquadsAccountConstraint {
                account_index: loyal_actions::hub_abi::swap_exact_in_accounts::USER_VAULT,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: loyal_actions::hub_abi::swap_exact_in_accounts::HUB_INPUT,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: loyal_actions::hub_abi::swap_exact_in_accounts::HUB_OUTPUT,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: loyal_actions::hub_abi::swap_exact_in_accounts::INPUT_MINT,
                account_constraint: SquadsAccountConstraintType::Pubkey(allowed_mints.clone()),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: loyal_actions::hub_abi::swap_exact_in_accounts::OUTPUT_MINT,
                account_constraint: SquadsAccountConstraintType::Pubkey(allowed_mints),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: loyal_actions::hub_abi::swap_exact_in_accounts::HUB_AUTHORIZER,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![hub_authorizer]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: loyal_actions::hub_abi::swap_exact_in_accounts::TOKEN_PROGRAM,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![
            SquadsDataConstraint {
                data_offset: LOYAL_HUB_SWAP_TAG_OFFSET,
                data_value: SquadsDataValue::U8(LOYAL_HUB_SWAP_EXACT_IN),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: LOYAL_HUB_SWAP_MAX_FEE_BPS_OFFSET,
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
