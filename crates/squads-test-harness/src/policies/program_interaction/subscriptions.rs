use super::common::create_squads_compact_program_interaction_policy_instruction;
use crate::types::*;
use crate::*;
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};

pub fn create_squads_program_interaction_subscription_sweep_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    delegator: Pubkey,
    vault: Pubkey,
    delegator_usdc_ata: Pubkey,
    vault_usdc_ata: Pubkey,
    max_amount_per_period: u64,
) -> Instruction {
    create_squads_program_interaction_flexible_subscription_sweep_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        delegator,
        vault,
        delegator_usdc_ata,
        vault_usdc_ata,
        max_amount_per_period,
    )
}

pub fn create_squads_program_interaction_flexible_subscription_sweep_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    delegator: Pubkey,
    vault: Pubkey,
    delegator_usdc_ata: Pubkey,
    vault_usdc_ata: Pubkey,
    max_amount_per_period: u64,
) -> Instruction {
    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![subscription_sweep_constraint(
            delegator,
            vault,
            delegator_usdc_ata,
            vault_usdc_ata,
            max_amount_per_period,
        )],
    )
}

fn subscription_sweep_constraint(
    delegator: Pubkey,
    vault: Pubkey,
    delegator_usdc_ata: Pubkey,
    vault_usdc_ata: Pubkey,
    max_amount_per_period: u64,
) -> SquadsInstructionConstraint {
    let subscription_authority = derive_subscription_authority(delegator, USDC_MINT);
    let event_authority = derive_subscription_event_authority();

    SquadsInstructionConstraint {
        program_id: SUBSCRIPTIONS_PROGRAM_ID,
        account_constraints: vec![
            recurring_delegation_account_constraint(
                delegator,
                vault,
                subscription_authority,
                max_amount_per_period,
            ),
            pubkey_constraint(1, subscription_authority, Some(SUBSCRIPTIONS_PROGRAM_ID)),
            pubkey_constraint(2, delegator_usdc_ata, Some(spl_token::id())),
            pubkey_constraint(3, vault_usdc_ata, Some(spl_token::id())),
            pubkey_constraint(4, USDC_MINT, Some(spl_token::id())),
            pubkey_constraint(5, spl_token::id(), None),
            pubkey_constraint(6, vault, None),
            pubkey_constraint(7, event_authority, None),
            pubkey_constraint(8, SUBSCRIPTIONS_PROGRAM_ID, None),
        ],
        data_constraints: vec![
            SquadsDataConstraint {
                data_offset: 0,
                data_value: SquadsDataValue::U8(SUBSCRIPTIONS_TRANSFER_RECURRING),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: SUBSCRIPTION_TRANSFER_DELEGATOR_OFFSET,
                data_value: SquadsDataValue::U8Slice(delegator.to_bytes().to_vec()),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: SUBSCRIPTION_TRANSFER_MINT_OFFSET,
                data_value: SquadsDataValue::U8Slice(USDC_MINT.to_bytes().to_vec()),
                operator: SquadsDataOperator::Equals,
            },
        ],
    }
}

fn recurring_delegation_account_constraint(
    delegator: Pubkey,
    vault: Pubkey,
    subscription_authority: Pubkey,
    max_amount_per_period: u64,
) -> SquadsAccountConstraint {
    SquadsAccountConstraint {
        account_index: 0,
        account_constraint: SquadsAccountConstraintType::AccountData(vec![
            SquadsDataConstraint {
                data_offset: SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET,
                data_value: SquadsDataValue::U8(SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: SUBSCRIPTION_RECURRING_DELEGATION_DELEGATOR_OFFSET,
                data_value: SquadsDataValue::U8Slice(delegator.to_bytes().to_vec()),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: SUBSCRIPTION_RECURRING_DELEGATION_DELEGATEE_OFFSET,
                data_value: SquadsDataValue::U8Slice(vault.to_bytes().to_vec()),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: SUBSCRIPTION_RECURRING_DELEGATION_AUTHORITY_OFFSET,
                data_value: SquadsDataValue::U8Slice(subscription_authority.to_bytes().to_vec()),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: SUBSCRIPTION_RECURRING_DELEGATION_MINT_OFFSET,
                data_value: SquadsDataValue::U8Slice(USDC_MINT.to_bytes().to_vec()),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET,
                data_value: SquadsDataValue::U64Le(max_amount_per_period),
                operator: SquadsDataOperator::LessThanOrEqualTo,
            },
        ]),
        owner: Some(SUBSCRIPTIONS_PROGRAM_ID),
    }
}

fn pubkey_constraint(
    account_index: u8,
    pubkey: Pubkey,
    owner: Option<Pubkey>,
) -> SquadsAccountConstraint {
    SquadsAccountConstraint {
        account_index,
        account_constraint: SquadsAccountConstraintType::Pubkey(vec![pubkey]),
        owner,
    }
}
