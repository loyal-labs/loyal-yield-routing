use crate::squads::{
    create_program_interaction_action_instruction, SquadsAccountConstraint,
    SquadsAccountConstraintType, SquadsDataConstraint, SquadsDataOperator, SquadsDataValue,
    SquadsInstructionConstraint,
};
use crate::{derive_action_account, LoyalActionError};
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};
use std::fmt;

const VOLTR_DEPOSIT: [u8; 8] = [246, 82, 57, 226, 131, 222, 253, 249];
const VOLTR_WITHDRAW: [u8; 8] = [31, 45, 162, 5, 193, 217, 134, 188];
pub const CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR: [u8; 8] = [242, 35, 198, 137, 82, 225, 242, 182];
pub const CUSTOM_ADAPTOR_WITHDRAW_DISCRIMINATOR: [u8; 8] = [183, 18, 70, 156, 148, 109, 161, 34];
pub const CUSTOM_ADAPTOR_ARM_REPORT_DISCRIMINATOR: [u8; 8] = [164, 175, 246, 41, 178, 140, 35, 3];
const SPL_TRANSFER_CHECKED: u8 = 12;
const DEPOSIT_BOUND_ACCOUNT_INDEXES: &[usize] = &[0, 2, 3, 8, 11, 12, 13, 14, 15, 16, 17];
const WITHDRAW_BOUND_ACCOUNT_INDEXES: &[usize] = &[0, 2, 5, 6, 9, 12, 13, 14, 15, 16, 17];

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct VoltrCustomPolicySeeds {
    pub allocation: u64,
    pub nav_refresh: u64,
    pub stage_withdrawal: u64,
    pub withdraw: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct VoltrCustomPolicyIdentity {
    pub settings: Pubkey,
    pub authority: Pubkey,
    pub delegated_signer: Pubkey,
    pub manager: Pubkey,
    pub squads_program: Pubkey,
    pub vault_index: u8,
    pub vault: Pubkey,
    pub strategy: Pubkey,
    pub voltr_program: Pubkey,
    pub adaptor_program: Pubkey,
    pub token_program: Pubkey,
    pub asset_mint: Pubkey,
    pub squads_asset_ata: Pubkey,
    pub strategy_asset_ata: Pubkey,
    pub report_ticket: Pubkey,
    pub max_amount_raw: u64,
    pub asset_decimals: u8,
    pub seeds: VoltrCustomPolicySeeds,
}

#[derive(Clone, Debug)]
pub struct VoltrCustomPolicyTemplates {
    pub allocation_arm: Instruction,
    pub allocation: Instruction,
    pub nav_refresh_arm: Instruction,
    pub nav_refresh: Instruction,
    pub stage_withdrawal: Instruction,
    pub withdraw_arm: Instruction,
    pub withdraw: Instruction,
}

#[derive(Clone, Debug)]
pub struct VoltrCustomPolicyPlan {
    pub policy: Pubkey,
    pub seed: u64,
    pub create_instruction: Instruction,
    pub constraint_index: u8,
    pub constraint_indices: Vec<u8>,
}

#[derive(Clone, Debug)]
pub struct VoltrCustomPolicies {
    pub allocation: VoltrCustomPolicyPlan,
    pub nav_refresh: VoltrCustomPolicyPlan,
    pub stage_withdrawal: VoltrCustomPolicyPlan,
    pub withdraw: VoltrCustomPolicyPlan,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum VoltrCustomPolicyError {
    DuplicateSeeds,
    InvalidLimit,
    InvalidInstruction {
        operation: &'static str,
        field: &'static str,
    },
    Squads(LoyalActionError),
}

impl fmt::Display for VoltrCustomPolicyError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::DuplicateSeeds => formatter
                .write_str("custom Voltr policy seeds must be distinct after packet-fit splitting"),
            Self::InvalidLimit => formatter.write_str("custom Voltr policy limit is invalid"),
            Self::InvalidInstruction { operation, field } => {
                write!(formatter, "invalid custom Voltr {operation} {field}")
            }
            Self::Squads(error) => write!(formatter, "Squads policy encoding failed: {error}"),
        }
    }
}

impl std::error::Error for VoltrCustomPolicyError {}

impl From<LoyalActionError> for VoltrCustomPolicyError {
    fn from(value: LoyalActionError) -> Self {
        Self::Squads(value)
    }
}

fn invalid(operation: &'static str, field: &'static str) -> VoltrCustomPolicyError {
    VoltrCustomPolicyError::InvalidInstruction { operation, field }
}

fn u64_at(data: &[u8], offset: usize) -> Option<u64> {
    Some(u64::from_le_bytes(
        data.get(offset..offset + 8)?.try_into().ok()?,
    ))
}

fn account_constraints(instruction: &Instruction) -> Vec<SquadsAccountConstraint> {
    instruction
        .accounts
        .iter()
        .enumerate()
        .map(|(index, account)| SquadsAccountConstraint {
            account_index: u8::try_from(index).expect("custom Voltr account count fits in u8"),
            account_constraint: SquadsAccountConstraintType::Pubkey(vec![account.pubkey]),
            owner: None,
        })
        .collect()
}

fn selected_account_constraints(
    instruction: &Instruction,
    indexes: &[usize],
) -> Vec<SquadsAccountConstraint> {
    indexes
        .iter()
        .map(|index| {
            let account = &instruction.accounts[*index];
            SquadsAccountConstraint {
                account_index: u8::try_from(*index).expect("custom Voltr account index fits in u8"),
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![account.pubkey]),
                owner: None,
            }
        })
        .collect()
}

fn validate_signer(
    operation: &'static str,
    instruction: &Instruction,
    index: usize,
    expected: Pubkey,
) -> Result<(), VoltrCustomPolicyError> {
    let account = instruction
        .accounts
        .get(index)
        .ok_or_else(|| invalid(operation, "account count"))?;
    if account.pubkey != expected
        || !account.is_signer
        || instruction
            .accounts
            .iter()
            .enumerate()
            .any(|(candidate, account)| account.is_signer && candidate != index)
    {
        return Err(invalid(operation, "signer set"));
    }
    Ok(())
}

fn validate_voltr_signers(
    operation: &'static str,
    instruction: &Instruction,
    manager: Pubkey,
) -> Result<(), VoltrCustomPolicyError> {
    for index in [0, 15] {
        let account = instruction
            .accounts
            .get(index)
            .ok_or_else(|| invalid(operation, "account count"))?;
        if account.pubkey != manager || !account.is_signer {
            return Err(invalid(operation, "signer set"));
        }
    }
    if instruction
        .accounts
        .iter()
        .enumerate()
        .any(|(index, account)| account.is_signer && index != 0 && index != 15)
    {
        return Err(invalid(operation, "signer set"));
    }
    Ok(())
}

fn validate_key(
    operation: &'static str,
    instruction: &Instruction,
    index: usize,
    expected: Pubkey,
    field: &'static str,
) -> Result<(), VoltrCustomPolicyError> {
    if instruction
        .accounts
        .get(index)
        .map(|account| account.pubkey)
        != Some(expected)
    {
        return Err(invalid(operation, field));
    }
    Ok(())
}

fn validate_envelope(
    operation: &'static str,
    instruction: &Instruction,
    outer: [u8; 8],
    inner: [u8; 8],
) -> Result<(), VoltrCustomPolicyError> {
    // The v2 adaptor consumes a report-bearing payload.  Only the report
    // version is static; the sequence, slot, NAV, and digest are deliberately
    // left unconstrained for the adaptor to authenticate and order.
    if instruction.data.len() != 91
        || instruction.data[..8] != outer
        || instruction.data[16] != 1
        || instruction.data[17..21] != 8u32.to_le_bytes()
        || instruction.data[21..29] != inner
        || instruction.data[29] != 1
        || instruction.data[30..34] != 57u32.to_le_bytes()
        || instruction.data[34] != 1
    {
        return Err(invalid(operation, "91-byte v2 adaptor envelope"));
    }
    Ok(())
}

fn voltr_constraint(
    instruction: &Instruction,
    outer: [u8; 8],
    inner: [u8; 8],
    amount_constraints: Vec<SquadsDataConstraint>,
    account_indexes: &[usize],
) -> SquadsInstructionConstraint {
    let mut envelope = Vec::with_capacity(14);
    envelope.push(1);
    envelope.extend_from_slice(&8u32.to_le_bytes());
    envelope.extend_from_slice(&inner);
    envelope.push(1);
    envelope.extend_from_slice(&57u32.to_le_bytes());
    envelope.push(1);
    let mut data_constraints = vec![SquadsDataConstraint {
        data_offset: 0,
        data_value: SquadsDataValue::U8Slice(outer.to_vec()),
        operator: SquadsDataOperator::Equals,
    }];
    data_constraints.extend(amount_constraints);
    data_constraints.push(SquadsDataConstraint {
        data_offset: 16,
        data_value: SquadsDataValue::U8Slice(envelope),
        operator: SquadsDataOperator::Equals,
    });
    SquadsInstructionConstraint {
        program_id: instruction.program_id,
        account_constraints: selected_account_constraints(instruction, account_indexes),
        data_constraints,
    }
}

fn validate_arm(
    operation: &'static str,
    instruction: &Instruction,
    capital: &Instruction,
    identity: &VoltrCustomPolicyIdentity,
    operation_tag: u8,
) -> Result<(), VoltrCustomPolicyError> {
    if instruction.program_id != identity.adaptor_program
        || instruction.accounts.len() != 5
        || instruction.data.len() != 79
        || instruction.data[..8] != CUSTOM_ADAPTOR_ARM_REPORT_DISCRIMINATOR
        || instruction.data[8] != operation_tag
        || instruction.data[9..17] != capital.data[8..16]
        || instruction.data[17..] != capital.data[29..]
    {
        return Err(invalid(operation, "ArmReport wire"));
    }
    validate_key(
        operation,
        instruction,
        0,
        identity.strategy,
        "ArmReport config",
    )?;
    validate_key(
        operation,
        instruction,
        1,
        identity.report_ticket,
        "ArmReport ticket",
    )?;
    validate_key(
        operation,
        instruction,
        2,
        identity.settings,
        "ArmReport Settings",
    )?;
    validate_key(
        operation,
        instruction,
        3,
        identity.manager,
        "ArmReport vault",
    )?;
    validate_key(
        operation,
        instruction,
        4,
        identity.squads_program,
        "ArmReport Squads program",
    )?;
    validate_signer(operation, instruction, 3, identity.manager)?;
    if instruction.accounts[0].is_writable
        || !instruction.accounts[1].is_writable
        || instruction.accounts[2].is_writable
        || instruction.accounts[3].is_writable
        || instruction.accounts[4].is_writable
    {
        return Err(invalid(operation, "ArmReport account roles"));
    }
    Ok(())
}

fn arm_constraint(
    instruction: &Instruction,
    amount_constraints: Vec<SquadsDataConstraint>,
) -> SquadsInstructionConstraint {
    let mut discriminator_and_operation = CUSTOM_ADAPTOR_ARM_REPORT_DISCRIMINATOR.to_vec();
    discriminator_and_operation.push(instruction.data[8]);
    let mut data_constraints = vec![SquadsDataConstraint {
        data_offset: 0,
        data_value: SquadsDataValue::U8Slice(discriminator_and_operation),
        operator: SquadsDataOperator::Equals,
    }];
    data_constraints.extend(amount_constraints);
    data_constraints.push(SquadsDataConstraint {
        data_offset: 17,
        data_value: SquadsDataValue::U8Slice(vec![1, 57, 0, 0, 0, 1]),
        operator: SquadsDataOperator::Equals,
    });
    SquadsInstructionConstraint {
        program_id: instruction.program_id,
        // The adaptor itself rederives and authenticates Settings/vault/program.
        // The policy only needs to pin the immutable config and its one-use PDA.
        account_constraints: selected_account_constraints(instruction, &[0, 1]),
        data_constraints,
    }
}

fn bounded_positive(max: u64) -> Vec<SquadsDataConstraint> {
    bounded_positive_at(8, max)
}

fn bounded_positive_at(offset: u64, max: u64) -> Vec<SquadsDataConstraint> {
    vec![
        SquadsDataConstraint {
            data_offset: offset,
            data_value: SquadsDataValue::U64Le(0),
            operator: SquadsDataOperator::GreaterThan,
        },
        SquadsDataConstraint {
            data_offset: offset,
            data_value: SquadsDataValue::U64Le(max),
            operator: SquadsDataOperator::LessThanOrEqualTo,
        },
    ]
}

fn policy_plan(
    identity: &VoltrCustomPolicyIdentity,
    seed: u64,
    constraints: Vec<SquadsInstructionConstraint>,
    constraint_index: u8,
) -> Result<VoltrCustomPolicyPlan, VoltrCustomPolicyError> {
    let (policy, _) = derive_action_account(&identity.settings, seed);
    let constraint_indices =
        (0..u8::try_from(constraints.len()).expect("constraint count fits u8")).collect();
    let create_instruction = create_program_interaction_action_instruction(
        identity.settings,
        identity.authority,
        identity.delegated_signer,
        seed,
        identity.vault_index,
        constraints,
    )?;
    Ok(VoltrCustomPolicyPlan {
        policy,
        seed,
        create_instruction,
        constraint_index,
        constraint_indices,
    })
}

pub fn create_voltr_custom_policies(
    identity: &VoltrCustomPolicyIdentity,
    templates: &VoltrCustomPolicyTemplates,
) -> Result<VoltrCustomPolicies, VoltrCustomPolicyError> {
    let seeds = [
        identity.seeds.allocation,
        identity.seeds.nav_refresh,
        identity.seeds.stage_withdrawal,
        identity.seeds.withdraw,
    ];
    if seeds
        .iter()
        .copied()
        .collect::<std::collections::BTreeSet<_>>()
        .len()
        != seeds.len()
    {
        return Err(VoltrCustomPolicyError::DuplicateSeeds);
    }
    if identity.max_amount_raw == 0 {
        return Err(VoltrCustomPolicyError::InvalidLimit);
    }

    let allocation = &templates.allocation;
    if allocation.program_id != identity.voltr_program || allocation.accounts.len() != 18 {
        return Err(invalid("allocation", "program or account count"));
    }
    validate_envelope(
        "allocation",
        allocation,
        VOLTR_DEPOSIT,
        CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR,
    )?;
    if u64_at(&allocation.data, 8) == Some(0) {
        return Err(invalid("allocation", "positive amount"));
    }
    validate_voltr_signers("allocation", allocation, identity.manager)?;
    validate_key("allocation", allocation, 2, identity.vault, "vault")?;
    validate_key("allocation", allocation, 3, identity.strategy, "strategy")?;
    validate_key(
        "allocation",
        allocation,
        8,
        identity.asset_mint,
        "asset mint",
    )?;
    validate_key(
        "allocation",
        allocation,
        12,
        identity.token_program,
        "token program",
    )?;
    validate_key(
        "allocation",
        allocation,
        13,
        identity.adaptor_program,
        "adaptor",
    )?;
    validate_key("allocation", allocation, 14, identity.settings, "Settings")?;
    validate_key(
        "allocation",
        allocation,
        15,
        identity.manager,
        "Squads signer",
    )?;
    validate_key(
        "allocation",
        allocation,
        16,
        identity.squads_asset_ata,
        "Squads asset ATA",
    )?;
    validate_key(
        "allocation",
        allocation,
        17,
        identity.report_ticket,
        "report ticket",
    )?;
    if allocation.accounts[3].is_writable
        || !allocation.accounts[16].is_writable
        || !allocation.accounts[17].is_writable
    {
        return Err(invalid("allocation", "bridge account roles"));
    }
    validate_arm(
        "allocation",
        &templates.allocation_arm,
        allocation,
        identity,
        0,
    )?;

    let refresh = &templates.nav_refresh;
    if refresh.program_id != identity.voltr_program || refresh.accounts.len() != 18 {
        return Err(invalid("NAV refresh", "program or account count"));
    }
    validate_envelope(
        "NAV refresh",
        refresh,
        VOLTR_DEPOSIT,
        CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR,
    )?;
    if u64_at(&refresh.data, 8) != Some(0) {
        return Err(invalid("NAV refresh", "zero amount"));
    }
    validate_voltr_signers("NAV refresh", refresh, identity.manager)?;
    validate_key("NAV refresh", refresh, 2, identity.vault, "vault")?;
    validate_key("NAV refresh", refresh, 3, identity.strategy, "strategy")?;
    validate_key(
        "NAV refresh",
        refresh,
        13,
        identity.adaptor_program,
        "adaptor",
    )?;
    validate_key("NAV refresh", refresh, 14, identity.settings, "Settings")?;
    validate_key(
        "NAV refresh",
        refresh,
        15,
        identity.manager,
        "Squads signer",
    )?;
    validate_key(
        "NAV refresh",
        refresh,
        16,
        identity.squads_asset_ata,
        "Squads asset ATA",
    )?;
    validate_key(
        "NAV refresh",
        refresh,
        17,
        identity.report_ticket,
        "report ticket",
    )?;
    if refresh.accounts[3].is_writable
        || !refresh.accounts[16].is_writable
        || !refresh.accounts[17].is_writable
    {
        return Err(invalid("NAV refresh", "bridge account roles"));
    }
    validate_arm(
        "NAV refresh",
        &templates.nav_refresh_arm,
        refresh,
        identity,
        0,
    )?;

    let stage = &templates.stage_withdrawal;
    if stage.program_id != identity.token_program
        || stage.accounts.len() != 4
        || stage.data.len() != 10
        || stage.data[0] != SPL_TRANSFER_CHECKED
        || stage.data[9] != identity.asset_decimals
        || u64_at(&stage.data, 1) == Some(0)
    {
        return Err(invalid("withdrawal staging", "instruction envelope"));
    }
    validate_key(
        "withdrawal staging",
        stage,
        0,
        identity.squads_asset_ata,
        "source",
    )?;
    validate_key(
        "withdrawal staging",
        stage,
        1,
        identity.asset_mint,
        "asset mint",
    )?;
    validate_key(
        "withdrawal staging",
        stage,
        2,
        identity.strategy_asset_ata,
        "destination",
    )?;
    validate_signer("withdrawal staging", stage, 3, identity.manager)?;

    let withdraw = &templates.withdraw;
    if withdraw.program_id != identity.voltr_program || withdraw.accounts.len() != 18 {
        return Err(invalid("withdraw", "program or account count"));
    }
    validate_envelope(
        "withdraw",
        withdraw,
        VOLTR_WITHDRAW,
        CUSTOM_ADAPTOR_WITHDRAW_DISCRIMINATOR,
    )?;
    if u64_at(&withdraw.data, 8) == Some(0) {
        return Err(invalid("withdraw", "positive amount"));
    }
    validate_voltr_signers("withdraw", withdraw, identity.manager)?;
    validate_key("withdraw", withdraw, 2, identity.vault, "vault")?;
    validate_key("withdraw", withdraw, 5, identity.strategy, "strategy")?;
    validate_key("withdraw", withdraw, 6, identity.adaptor_program, "adaptor")?;
    validate_key("withdraw", withdraw, 9, identity.asset_mint, "asset mint")?;
    validate_key(
        "withdraw",
        withdraw,
        12,
        identity.strategy_asset_ata,
        "strategy asset ATA",
    )?;
    validate_key(
        "withdraw",
        withdraw,
        13,
        identity.token_program,
        "token program",
    )?;
    validate_key("withdraw", withdraw, 14, identity.settings, "Settings")?;
    validate_key("withdraw", withdraw, 15, identity.manager, "Squads signer")?;
    validate_key(
        "withdraw",
        withdraw,
        16,
        identity.squads_asset_ata,
        "Squads asset ATA",
    )?;
    validate_key(
        "withdraw",
        withdraw,
        17,
        identity.report_ticket,
        "report ticket",
    )?;
    if withdraw.accounts[5].is_writable
        || !withdraw.accounts[16].is_writable
        || !withdraw.accounts[17].is_writable
    {
        return Err(invalid("withdraw", "bridge account roles"));
    }
    validate_arm("withdraw", &templates.withdraw_arm, withdraw, identity, 1)?;

    // Voltr validates the receipt/authority PDAs and the adaptor validates every
    // remaining account against its immutable config. The policy pins the
    // custody-critical bridge identities without duplicating that whole proof.
    let allocation_constraint = voltr_constraint(
        allocation,
        VOLTR_DEPOSIT,
        CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR,
        bounded_positive(identity.max_amount_raw),
        DEPOSIT_BOUND_ACCOUNT_INDEXES,
    );
    let refresh_constraint = voltr_constraint(
        refresh,
        VOLTR_DEPOSIT,
        CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR,
        vec![SquadsDataConstraint {
            data_offset: 8,
            data_value: SquadsDataValue::U64Le(0),
            operator: SquadsDataOperator::Equals,
        }],
        DEPOSIT_BOUND_ACCOUNT_INDEXES,
    );
    let stage_constraint = SquadsInstructionConstraint {
        program_id: stage.program_id,
        account_constraints: account_constraints(stage),
        data_constraints: vec![
            SquadsDataConstraint {
                data_offset: 0,
                data_value: SquadsDataValue::U8(SPL_TRANSFER_CHECKED),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: 1,
                data_value: SquadsDataValue::U64Le(0),
                operator: SquadsDataOperator::GreaterThan,
            },
            SquadsDataConstraint {
                data_offset: 1,
                data_value: SquadsDataValue::U64Le(identity.max_amount_raw),
                operator: SquadsDataOperator::LessThanOrEqualTo,
            },
            SquadsDataConstraint {
                data_offset: 9,
                data_value: SquadsDataValue::U8(identity.asset_decimals),
                operator: SquadsDataOperator::Equals,
            },
        ],
    };
    let withdraw_constraint = voltr_constraint(
        withdraw,
        VOLTR_WITHDRAW,
        CUSTOM_ADAPTOR_WITHDRAW_DISCRIMINATOR,
        bounded_positive(identity.max_amount_raw),
        WITHDRAW_BOUND_ACCOUNT_INDEXES,
    );

    Ok(VoltrCustomPolicies {
        allocation: policy_plan(
            identity,
            identity.seeds.allocation,
            vec![
                arm_constraint(
                    &templates.allocation_arm,
                    bounded_positive_at(9, identity.max_amount_raw),
                ),
                allocation_constraint,
            ],
            0,
        )?,
        nav_refresh: policy_plan(
            identity,
            identity.seeds.nav_refresh,
            vec![
                arm_constraint(
                    &templates.nav_refresh_arm,
                    vec![SquadsDataConstraint {
                        data_offset: 9,
                        data_value: SquadsDataValue::U64Le(0),
                        operator: SquadsDataOperator::Equals,
                    }],
                ),
                refresh_constraint,
            ],
            0,
        )?,
        stage_withdrawal: policy_plan(
            identity,
            identity.seeds.stage_withdrawal,
            vec![stage_constraint],
            0,
        )?,
        withdraw: policy_plan(
            identity,
            identity.seeds.withdraw,
            vec![
                arm_constraint(
                    &templates.withdraw_arm,
                    bounded_positive_at(9, identity.max_amount_raw),
                ),
                withdraw_constraint,
            ],
            0,
        )?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::backyard_policy_catalog::{signed_policy_create_packet_bytes, SOLANA_PACKET_BYTES};
    use solana_sdk::{
        hash::Hash,
        instruction::AccountMeta,
        signature::{Keypair, Signer},
    };

    fn key(value: u8) -> Pubkey {
        Pubkey::new_from_array([value; 32])
    }

    fn identity() -> VoltrCustomPolicyIdentity {
        VoltrCustomPolicyIdentity {
            settings: key(1),
            authority: key(2),
            delegated_signer: key(3),
            manager: key(4),
            squads_program: key(13),
            vault_index: 0,
            vault: key(5),
            strategy: key(6),
            voltr_program: key(7),
            adaptor_program: key(8),
            token_program: key(9),
            asset_mint: key(10),
            squads_asset_ata: key(11),
            strategy_asset_ata: key(12),
            report_ticket: key(14),
            max_amount_raw: 1_000_000,
            asset_decimals: 6,
            seeds: VoltrCustomPolicySeeds {
                allocation: 53,
                nav_refresh: 54,
                stage_withdrawal: 55,
                withdraw: 56,
            },
        }
    }

    fn data(outer: [u8; 8], amount: u64, inner: [u8; 8]) -> Vec<u8> {
        let mut value = Vec::from(outer);
        value.extend_from_slice(&amount.to_le_bytes());
        value.push(1);
        value.extend_from_slice(&8u32.to_le_bytes());
        value.extend_from_slice(&inner);
        value.push(1);
        value.extend_from_slice(&57u32.to_le_bytes());
        value.push(1);
        value.extend_from_slice(&[0; 56]);
        value
    }

    fn templates(identity: &VoltrCustomPolicyIdentity) -> VoltrCustomPolicyTemplates {
        let mut deposit_accounts = (0..18)
            .map(|index| AccountMeta::new_readonly(key(100 + index), false))
            .collect::<Vec<_>>();
        deposit_accounts[0] = AccountMeta::new_readonly(identity.manager, true);
        deposit_accounts[2].pubkey = identity.vault;
        deposit_accounts[3] = AccountMeta::new_readonly(identity.strategy, false);
        deposit_accounts[7].pubkey = key(20);
        deposit_accounts[8].pubkey = identity.asset_mint;
        deposit_accounts[11].pubkey = identity.strategy_asset_ata;
        deposit_accounts[12].pubkey = identity.token_program;
        deposit_accounts[13].pubkey = identity.adaptor_program;
        deposit_accounts[14].pubkey = identity.settings;
        deposit_accounts[15] = AccountMeta::new_readonly(identity.manager, true);
        deposit_accounts[16] = AccountMeta::new(identity.squads_asset_ata, false);
        deposit_accounts[17] = AccountMeta::new(identity.report_ticket, false);
        let allocation = Instruction {
            program_id: identity.voltr_program,
            accounts: deposit_accounts.clone(),
            data: data(VOLTR_DEPOSIT, 1_000, CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR),
        };
        let nav_refresh = Instruction {
            program_id: identity.voltr_program,
            accounts: deposit_accounts,
            data: data(VOLTR_DEPOSIT, 0, CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR),
        };

        let mut withdraw_accounts = (0..18)
            .map(|index| AccountMeta::new_readonly(key(140 + index), false))
            .collect::<Vec<_>>();
        withdraw_accounts[0] = AccountMeta::new_readonly(identity.manager, true);
        withdraw_accounts[2].pubkey = identity.vault;
        withdraw_accounts[5] = AccountMeta::new_readonly(identity.strategy, false);
        withdraw_accounts[6].pubkey = identity.adaptor_program;
        withdraw_accounts[8].pubkey = key(20);
        withdraw_accounts[9].pubkey = identity.asset_mint;
        withdraw_accounts[12].pubkey = identity.strategy_asset_ata;
        withdraw_accounts[13].pubkey = identity.token_program;
        withdraw_accounts[14].pubkey = identity.settings;
        withdraw_accounts[15] = AccountMeta::new_readonly(identity.manager, true);
        withdraw_accounts[16] = AccountMeta::new(identity.squads_asset_ata, false);
        withdraw_accounts[17] = AccountMeta::new(identity.report_ticket, false);
        let withdraw = Instruction {
            program_id: identity.voltr_program,
            accounts: withdraw_accounts,
            data: data(VOLTR_WITHDRAW, 1_000, CUSTOM_ADAPTOR_WITHDRAW_DISCRIMINATOR),
        };

        let mut stage_data = vec![SPL_TRANSFER_CHECKED];
        stage_data.extend_from_slice(&1_000u64.to_le_bytes());
        stage_data.push(identity.asset_decimals);
        let stage_withdrawal = Instruction {
            program_id: identity.token_program,
            accounts: vec![
                AccountMeta::new(identity.squads_asset_ata, false),
                AccountMeta::new_readonly(identity.asset_mint, false),
                AccountMeta::new(identity.strategy_asset_ata, false),
                AccountMeta::new_readonly(identity.manager, true),
            ],
            data: stage_data,
        };
        let arm = |capital: &Instruction, operation: u8| {
            let mut arm_data = CUSTOM_ADAPTOR_ARM_REPORT_DISCRIMINATOR.to_vec();
            arm_data.push(operation);
            arm_data.extend_from_slice(&capital.data[8..16]);
            arm_data.extend_from_slice(&capital.data[29..]);
            Instruction {
                program_id: identity.adaptor_program,
                accounts: vec![
                    AccountMeta::new_readonly(identity.strategy, false),
                    AccountMeta::new(identity.report_ticket, false),
                    AccountMeta::new_readonly(identity.settings, false),
                    AccountMeta::new_readonly(identity.manager, true),
                    AccountMeta::new_readonly(identity.squads_program, false),
                ],
                data: arm_data,
            }
        };
        VoltrCustomPolicyTemplates {
            allocation_arm: arm(&allocation, 0),
            nav_refresh_arm: arm(&nav_refresh, 0),
            withdraw_arm: arm(&withdraw, 1),
            allocation,
            nav_refresh,
            stage_withdrawal,
            withdraw,
        }
    }

    #[test]
    fn compiles_exact_four_policy_packet_boundary() {
        let identity = identity();
        let policies = create_voltr_custom_policies(&identity, &templates(&identity)).unwrap();
        assert_eq!(
            [
                policies.allocation.seed,
                policies.nav_refresh.seed,
                policies.stage_withdrawal.seed,
                policies.withdraw.seed
            ],
            [53, 54, 55, 56]
        );
        assert_eq!(
            [
                policies.allocation.constraint_index,
                policies.nav_refresh.constraint_index,
                policies.stage_withdrawal.constraint_index,
                policies.withdraw.constraint_index
            ],
            [0, 0, 0, 0]
        );
        assert_eq!(policies.allocation.constraint_indices, [0, 1]);
        assert_eq!(policies.nav_refresh.constraint_indices, [0, 1]);
        assert_eq!(policies.stage_withdrawal.constraint_indices, [0]);
        assert_eq!(policies.withdraw.constraint_indices, [0, 1]);
        assert_ne!(policies.allocation.policy, policies.nav_refresh.policy);
        assert_ne!(policies.stage_withdrawal.policy, policies.withdraw.policy);
    }

    #[test]
    fn four_split_bridge_policy_create_packets_fit() {
        let authority = Keypair::new();
        let mut identity = identity();
        identity.authority = authority.pubkey();
        let policies = create_voltr_custom_policies(&identity, &templates(&identity)).unwrap();
        let packets = [
            ("allocation", &policies.allocation.create_instruction),
            ("nav", &policies.nav_refresh.create_instruction),
            ("stage", &policies.stage_withdrawal.create_instruction),
            ("withdraw", &policies.withdraw.create_instruction),
        ]
        .map(|(name, create_instruction)| {
            (
                name,
                signed_policy_create_packet_bytes(
                    create_instruction,
                    &authority,
                    Hash::new_unique(),
                ),
            )
        });
        println!("backyard_bridge_policy_packets {packets:?} limit={SOLANA_PACKET_BYTES}");
        assert!(packets
            .iter()
            .all(|(_, create_bytes)| *create_bytes <= SOLANA_PACKET_BYTES));
    }

    #[test]
    fn rejects_trailing_adaptor_data() {
        let identity = identity();
        let mut templates = templates(&identity);
        templates.nav_refresh.data.push(0);
        assert!(matches!(
            create_voltr_custom_policies(&identity, &templates),
            Err(VoltrCustomPolicyError::InvalidInstruction {
                operation: "NAV refresh",
                field: "91-byte v2 adaptor envelope"
            })
        ));
    }

    #[test]
    fn rejects_redirected_staging_destination() {
        let identity = identity();
        let mut templates = templates(&identity);
        templates.stage_withdrawal.accounts[2].pubkey = key(99);
        assert!(matches!(
            create_voltr_custom_policies(&identity, &templates),
            Err(VoltrCustomPolicyError::InvalidInstruction {
                operation: "withdrawal staging",
                field: "destination"
            })
        ));
    }

    #[test]
    fn rejects_v2_bridge_config_writable_or_signer_mutation() {
        let identity = identity();
        let mut invalid_templates = templates(&identity);
        invalid_templates.allocation.accounts[3].is_writable = true;
        assert!(matches!(
            create_voltr_custom_policies(&identity, &invalid_templates),
            Err(VoltrCustomPolicyError::InvalidInstruction {
                operation: "allocation",
                field: "bridge account roles"
            })
        ));

        let mut invalid_templates = templates(&identity);
        invalid_templates.withdraw.accounts[15].pubkey = key(99);
        assert!(matches!(
            create_voltr_custom_policies(&identity, &invalid_templates),
            Err(VoltrCustomPolicyError::InvalidInstruction {
                operation: "withdraw",
                field: "signer set"
            })
        ));
    }

    #[test]
    fn squads_policy_bytes_cannot_encode_readonly_to_writable_expansion() {
        let identity = identity();
        let canonical = templates(&identity).allocation;
        assert!(!canonical.accounts[4].is_writable);

        let mut widened = canonical.clone();
        widened.accounts[4].is_writable = true;
        let indexes = (0..18).collect::<Vec<_>>();
        let canonical_constraint = voltr_constraint(
            &canonical,
            VOLTR_DEPOSIT,
            CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR,
            bounded_positive(identity.max_amount_raw),
            &indexes,
        );
        let widened_constraint = voltr_constraint(
            &widened,
            VOLTR_DEPOSIT,
            CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR,
            bounded_positive(identity.max_amount_raw),
            &indexes,
        );
        let canonical_policy = policy_plan(
            &identity,
            identity.seeds.allocation,
            vec![canonical_constraint],
            0,
        )
        .unwrap();
        let widened_policy = policy_plan(
            &identity,
            identity.seeds.allocation,
            vec![widened_constraint],
            0,
        )
        .unwrap();

        assert_eq!(
            canonical_policy.create_instruction.data, widened_policy.create_instruction.data,
            "ProgramInteraction constraints serialize pubkey/owner/data only, not writable roles"
        );
    }
}
