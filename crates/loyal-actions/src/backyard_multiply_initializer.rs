//! Exact Multiply reentry boundary for the three admitted USDC lanes.
//! Account creation is separate from capital-management policies. The caller
//! supplies a freshly observed Settings seed; this module never signs or sends.

use crate::{
    create_deployed_semantic_program_interaction_policy_instruction, derive_kamino_obligation,
    derive_kamino_user_metadata, derive_squads_vault, SemanticProgramInteractionConstraint,
    SemanticProgramInteractionDataConstraint,
};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use std::{error::Error, str::FromStr};

pub const MULTIPLY_INIT_DATA: [u8; 10] = [251, 10, 231, 76, 27, 11, 159, 96, 1, 0];

/// A lane-specific initializer has no arbitrary market, owner, payer or seeds.
#[derive(Clone, Debug)]
pub struct MultiplyInitializer {
    pub lane: &'static str,
    pub instruction: Instruction,
}

pub fn backyard_multiply_initializers(
    settings: Pubkey,
) -> Result<Vec<MultiplyInitializer>, Box<dyn Error>> {
    let vault = derive_squads_vault(&settings, 0).0;
    let klend = Pubkey::from_str("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD")?;
    let debt = Pubkey::from_str(crate::EARN_MAX_USDC_MINT)?;
    let metadata = derive_kamino_user_metadata(vault);
    let mut result = Vec::new();
    for (lane, market, collateral) in [
        (
            "Prime/PRIME/USDC",
            "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
            crate::EARN_MAX_PRIME_MINT,
        ),
        (
            "Maple/syrupUSDC/USDC",
            "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
            crate::EARN_MAX_SYRUP_USDC_MINT,
        ),
        (
            "OnRe/ONyc/USDC",
            "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
            crate::EARN_MAX_ONYC_MINT,
        ),
    ] {
        let market = Pubkey::from_str(market)?;
        let collateral = Pubkey::from_str(collateral)?;
        let obligation = derive_kamino_obligation(vault, market, 1, 0, collateral, debt);
        result.push(MultiplyInitializer {
            lane,
            instruction: Instruction {
                program_id: klend,
                accounts: vec![
                    AccountMeta::new_readonly(vault, true),
                    AccountMeta::new(vault, true),
                    AccountMeta::new(obligation, false),
                    AccountMeta::new_readonly(market, false),
                    AccountMeta::new_readonly(collateral, false),
                    AccountMeta::new_readonly(debt, false),
                    AccountMeta::new_readonly(metadata, false),
                    AccountMeta::new_readonly(solana_sdk::sysvar::rent::ID, false),
                    AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
                ],
                data: MULTIPLY_INIT_DATA.to_vec(),
            },
        });
    }
    Ok(result)
}

/// Legacy SettingsAction packets cannot carry all three exact rules together
/// (1,597 bytes). Emit one narrow, independently installable policy per lane.
pub fn compile_backyard_multiply_initializer_policies(
    settings: Pubkey,
    authority: Pubkey,
    delegate: Pubkey,
    seed_base: u64,
) -> Result<Vec<Instruction>, Box<dyn Error>> {
    let mut result = Vec::new();
    for (offset, init) in backyard_multiply_initializers(settings)?
        .into_iter()
        .enumerate()
    {
        let seed = seed_base
            .checked_add(offset as u64)
            .ok_or("initializer seed overflow")?;
        let constraint = SemanticProgramInteractionConstraint {
            program_id: init.instruction.program_id,
            account_pubkeys: init
                .instruction
                .accounts
                .iter()
                .enumerate()
                .map(|(index, account)| (index as u8, vec![account.pubkey]))
                .collect(),
            account_data: Vec::new(),
            data: vec![SemanticProgramInteractionDataConstraint::SliceEquals {
                offset: 0,
                value: MULTIPLY_INIT_DATA.to_vec(),
            }],
        };
        result.push(
            create_deployed_semantic_program_interaction_policy_instruction(
                settings,
                authority,
                delegate,
                seed,
                0,
                vec![constraint],
            )?,
        );
    }
    Ok(result)
}
