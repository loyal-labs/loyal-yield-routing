//! Voltr custom-adaptor policy builders.
//!
//! This keeps the one-shot repair policy on the same legacy
//! `ProgramInteraction` encoding used by the deployed Voltr policy compiler.
//! The policy constrains the immutable bridge identities, exact instruction
//! envelopes, and the reviewed ReportV1 NAV; the adaptor authenticates and
//! orders the remaining mutable ReportV1 fields.

use super::common::create_squads_program_interaction_policy_instruction;
use crate::types::*;
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};

const ARM_REPORT_PREFIX: usize = 9; // adaptor discriminator + operation
const ARM_REPORT_ENVELOPE_START: usize = 17;
const ARM_REPORT_ENVELOPE_END: usize = 23; // Option<ReportV1> + ReportV1 version
const ARM_REPORT_NAV_OFFSET: usize = 39;
const CAPITAL_ENVELOPE_START: usize = 16;
const CAPITAL_ENVELOPE_END: usize = 35; // adaptor instruction + ReportV1 version
const CAPITAL_NAV_OFFSET: usize = 51;

fn selected_account_constraints(
    instruction: &Instruction,
    indexes: &[usize],
) -> Vec<SquadsAccountConstraint> {
    indexes
        .iter()
        .map(|index| SquadsAccountConstraint {
            account_index: u8::try_from(*index).expect("Voltr account index fits in u8"),
            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                instruction.accounts[*index].pubkey,
            ]),
            owner: None,
        })
        .collect()
}

fn data_constraint(offset: u64, data_value: SquadsDataValue) -> SquadsDataConstraint {
    SquadsDataConstraint {
        data_offset: offset,
        data_value,
        operator: SquadsDataOperator::Equals,
    }
}

fn u64_le(data: &[u8], offset: usize) -> u64 {
    u64::from_le_bytes(
        data[offset..offset + std::mem::size_of::<u64>()]
            .try_into()
            .expect("Voltr report NAV field must be present"),
    )
}

/// Build the one-shot NAV-refresh repair policy used by `hxtk-reset`.
///
/// The passed instructions are the canonical arm_report and
/// deposit_strategy(0) templates. Building constraints from those templates
/// makes the account identities and immutable envelope bytes auditable without
/// duplicating the Squads wire serializer here.
pub fn create_squads_program_interaction_voltr_repair_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    arm_report: &Instruction,
    deposit_strategy: &Instruction,
) -> Instruction {
    assert!(arm_report.data.len() >= ARM_REPORT_ENVELOPE_END);
    assert!(deposit_strategy.data.len() >= CAPITAL_ENVELOPE_END);
    assert!(arm_report.data.len() >= ARM_REPORT_NAV_OFFSET + std::mem::size_of::<u64>());
    assert!(deposit_strategy.data.len() >= CAPITAL_NAV_OFFSET + std::mem::size_of::<u64>());

    let arm_report_nav = u64_le(&arm_report.data, ARM_REPORT_NAV_OFFSET);
    let capital_report_nav = u64_le(&deposit_strategy.data, CAPITAL_NAV_OFFSET);
    assert_eq!(
        arm_report_nav, capital_report_nav,
        "arm_report and deposit_strategy templates must carry the same NAV"
    );

    let constraints = vec![
        SquadsInstructionConstraint {
            program_id: arm_report.program_id,
            account_constraints: selected_account_constraints(arm_report, &[0, 1]),
            data_constraints: vec![
                data_constraint(
                    0,
                    SquadsDataValue::U8Slice(arm_report.data[..ARM_REPORT_PREFIX].to_vec()),
                ),
                data_constraint(9, SquadsDataValue::U64Le(0)),
                data_constraint(
                    ARM_REPORT_NAV_OFFSET as u64,
                    SquadsDataValue::U64Le(arm_report_nav),
                ),
                data_constraint(
                    ARM_REPORT_ENVELOPE_START as u64,
                    SquadsDataValue::U8Slice(
                        arm_report.data[ARM_REPORT_ENVELOPE_START..ARM_REPORT_ENVELOPE_END]
                            .to_vec(),
                    ),
                ),
            ],
        },
        SquadsInstructionConstraint {
            program_id: deposit_strategy.program_id,
            account_constraints: selected_account_constraints(
                deposit_strategy,
                &[0, 2, 3, 8, 11, 12, 13, 14, 15, 16, 17],
            ),
            data_constraints: vec![
                data_constraint(
                    0,
                    SquadsDataValue::U8Slice(deposit_strategy.data[..8].to_vec()),
                ),
                data_constraint(8, SquadsDataValue::U64Le(0)),
                data_constraint(
                    CAPITAL_NAV_OFFSET as u64,
                    SquadsDataValue::U64Le(capital_report_nav),
                ),
                data_constraint(
                    CAPITAL_ENVELOPE_START as u64,
                    SquadsDataValue::U8Slice(
                        deposit_strategy.data[CAPITAL_ENVELOPE_START..CAPITAL_ENVELOPE_END]
                            .to_vec(),
                    ),
                ),
            ],
        },
    ];

    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        constraints,
    )
}
