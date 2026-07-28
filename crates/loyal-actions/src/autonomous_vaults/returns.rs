use crate::{
    create_unlimited_spl_spending_limit_policy_instruction, derive_action_account,
    derive_classic_associated_token_account, execute_spl_spending_limit_policy_instruction,
    USDC_MINT,
};
use solana_sdk::{instruction::Instruction, pubkey, pubkey::Pubkey};
use std::fmt;

use super::METEORA_LOYAL_MINT;

pub const MOTHER_TREASURY_VAULT: Pubkey = pubkey!("AQyyTwCKemeeMu8ZPZFxrXMbVwAYTSbBhi1w4PBrhvYE");
pub const LOYAL_RETURN_POLICY_SEED: u64 = 6;
pub const USDC_RETURN_POLICY_SEED: u64 = 7;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TreasuryReturnKind {
    Loyal,
    Usdc,
}

impl TreasuryReturnKind {
    pub fn label(self) -> &'static str {
        match self {
            Self::Loyal => "LOYAL",
            Self::Usdc => "USDC",
        }
    }

    pub fn mint(self) -> Pubkey {
        match self {
            Self::Loyal => METEORA_LOYAL_MINT,
            Self::Usdc => USDC_MINT,
        }
    }
}

#[derive(Clone, Debug)]
pub struct TreasuryReturnPolicyPlan {
    pub kind: TreasuryReturnKind,
    pub policy: Pubkey,
    pub policy_seed: u64,
    pub mint: Pubkey,
    pub source_token_account: Pubkey,
    pub destination_owner: Pubkey,
    pub destination_token_account: Pubkey,
    pub create_instruction: Instruction,
}

#[derive(Clone, Debug)]
pub struct AutonomousTreasuryReturnPolicies {
    pub loyal: TreasuryReturnPolicyPlan,
    pub usdc: TreasuryReturnPolicyPlan,
}

impl AutonomousTreasuryReturnPolicies {
    pub fn policy(self: &Self, kind: TreasuryReturnKind) -> &TreasuryReturnPolicyPlan {
        match kind {
            TreasuryReturnKind::Loyal => &self.loyal,
            TreasuryReturnKind::Usdc => &self.usdc,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum TreasuryReturnPolicyError {
    DuplicatePolicySeeds,
    MotherIsSourceVault,
}

impl fmt::Display for TreasuryReturnPolicyError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::DuplicatePolicySeeds => {
                formatter.write_str("treasury return policy seeds must be distinct")
            }
            Self::MotherIsSourceVault => {
                formatter.write_str("Mother destination must differ from the autonomous vault")
            }
        }
    }
}

impl std::error::Error for TreasuryReturnPolicyError {}

#[allow(clippy::too_many_arguments)]
pub fn create_return_to_mother_policies(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    vault: Pubkey,
    vault_index: u8,
    loyal_seed: u64,
    usdc_seed: u64,
) -> Result<AutonomousTreasuryReturnPolicies, TreasuryReturnPolicyError> {
    if loyal_seed == usdc_seed {
        return Err(TreasuryReturnPolicyError::DuplicatePolicySeeds);
    }
    if vault == MOTHER_TREASURY_VAULT {
        return Err(TreasuryReturnPolicyError::MotherIsSourceVault);
    }

    Ok(AutonomousTreasuryReturnPolicies {
        loyal: policy_plan(
            TreasuryReturnKind::Loyal,
            settings,
            authority,
            delegated_signer,
            vault,
            vault_index,
            loyal_seed,
        ),
        usdc: policy_plan(
            TreasuryReturnKind::Usdc,
            settings,
            authority,
            delegated_signer,
            vault,
            vault_index,
            usdc_seed,
        ),
    })
}

#[allow(clippy::too_many_arguments)]
fn policy_plan(
    kind: TreasuryReturnKind,
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    vault: Pubkey,
    vault_index: u8,
    policy_seed: u64,
) -> TreasuryReturnPolicyPlan {
    let mint = kind.mint();
    TreasuryReturnPolicyPlan {
        kind,
        policy: derive_action_account(&settings, policy_seed).0,
        policy_seed,
        mint,
        source_token_account: derive_classic_associated_token_account(vault, mint),
        destination_owner: MOTHER_TREASURY_VAULT,
        destination_token_account: derive_classic_associated_token_account(
            MOTHER_TREASURY_VAULT,
            mint,
        ),
        create_instruction: create_unlimited_spl_spending_limit_policy_instruction(
            settings,
            authority,
            delegated_signer,
            policy_seed,
            vault_index,
            mint,
            MOTHER_TREASURY_VAULT,
        ),
    }
}

pub fn return_to_mother_instruction(
    settings: Pubkey,
    delegated_signer: Pubkey,
    vault_index: u8,
    policy: &TreasuryReturnPolicyPlan,
    amount: u64,
) -> Instruction {
    execute_spl_spending_limit_policy_instruction(
        policy.policy,
        delegated_signer,
        settings,
        vault_index,
        policy.mint,
        policy.destination_owner,
        amount,
        6,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::derive_squads_vault;

    #[test]
    fn plans_two_distinct_canonical_return_policies() {
        let settings = Pubkey::new_unique();
        let authority = Pubkey::new_unique();
        let delegated = Pubkey::new_unique();
        let vault = derive_squads_vault(&settings, 0).0;
        let policies = create_return_to_mother_policies(
            settings,
            authority,
            delegated,
            vault,
            0,
            LOYAL_RETURN_POLICY_SEED,
            USDC_RETURN_POLICY_SEED,
        )
        .expect("return policies");

        assert_ne!(policies.loyal.policy, policies.usdc.policy);
        assert_ne!(
            policies.loyal.source_token_account,
            policies.usdc.source_token_account
        );
        assert_eq!(policies.loyal.destination_owner, MOTHER_TREASURY_VAULT);
        assert_eq!(policies.usdc.destination_owner, MOTHER_TREASURY_VAULT);
        assert_eq!(policies.loyal.create_instruction.accounts.len(), 6);
        assert_eq!(policies.usdc.create_instruction.accounts.len(), 6);

        let data = &policies.loyal.create_instruction.data;
        assert_eq!(data.len(), 167);
        assert_eq!(data[8], 1); // num_signers
        assert_eq!(&data[9..13], &1_u32.to_le_bytes()); // one settings action
        assert_eq!(data[13], 7); // PolicyCreate settings action
        assert_eq!(&data[14..22], &LOYAL_RETURN_POLICY_SEED.to_le_bytes());
        assert_eq!(data[22], 1); // SpendingLimit creation payload
        assert_eq!(&data[23..55], METEORA_LOYAL_MINT.as_ref());
        assert_eq!(data[55], 0); // source vault index
        assert_eq!(&data[56..64], &0_i64.to_le_bytes());
        assert_eq!(data[64], 0); // no spending expiration
        assert_eq!(data[65], 0); // OneTime
        assert_eq!(data[66], 0); // no unused accumulation
        assert_eq!(&data[67..75], &u64::MAX.to_le_bytes());
        assert_eq!(&data[75..83], &0_u64.to_le_bytes());
        assert_eq!(data[83], 0); // no exact-per-use enforcement
        assert_eq!(data[84], 0); // no supplied usage state
        assert_eq!(&data[85..89], &1_u32.to_le_bytes());
        assert_eq!(&data[89..121], MOTHER_TREASURY_VAULT.as_ref());
        assert_eq!(&data[121..125], &1_u32.to_le_bytes());
        assert_eq!(&data[125..157], delegated.as_ref());
        assert_eq!(data[157], 7); // full policy permissions
        assert_eq!(&data[158..160], &1_u16.to_le_bytes());
        assert_eq!(&data[160..164], &0_u32.to_le_bytes());
        assert_eq!(&data[164..], &[0, 0, 0]); // no start, expiration, memo
    }

    #[test]
    fn execution_uses_only_canonical_source_and_mother_token_accounts() {
        let settings = Pubkey::new_unique();
        let authority = Pubkey::new_unique();
        let delegated = Pubkey::new_unique();
        let vault = derive_squads_vault(&settings, 0).0;
        let policies = create_return_to_mother_policies(
            settings,
            authority,
            delegated,
            vault,
            0,
            LOYAL_RETURN_POLICY_SEED,
            USDC_RETURN_POLICY_SEED,
        )
        .expect("return policies");
        let instruction = return_to_mother_instruction(settings, delegated, 0, &policies.loyal, 1);

        assert_eq!(instruction.accounts[2].pubkey, delegated);
        assert!(instruction.accounts[2].is_signer);
        assert_eq!(instruction.accounts[3].pubkey, vault);
        assert_eq!(
            instruction.accounts[4].pubkey,
            policies.loyal.source_token_account
        );
        assert_eq!(
            instruction.accounts[5].pubkey,
            policies.loyal.destination_token_account
        );
        assert_eq!(instruction.accounts[6].pubkey, policies.loyal.mint);
        assert_eq!(instruction.accounts[7].pubkey, spl_token::id());
        assert_eq!(instruction.data.len(), 53);
        assert_eq!(instruction.data[8], 0); // source vault index
        assert_eq!(instruction.data[9], 1); // one policy signer
        assert_eq!(instruction.data[10], 1); // sync Policy payload
        assert_eq!(instruction.data[11], 2); // SpendingLimit execution payload
        assert_eq!(&instruction.data[12..20], &1_u64.to_le_bytes());
        assert_eq!(&instruction.data[20..52], MOTHER_TREASURY_VAULT.as_ref());
        assert_eq!(instruction.data[52], 6);
    }
}
