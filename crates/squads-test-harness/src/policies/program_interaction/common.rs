use crate::types::*;
use crate::*;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

pub(super) fn create_squads_compact_program_interaction_policy_instruction(
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
pub(super) fn create_squads_program_interaction_policy_instruction(
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
