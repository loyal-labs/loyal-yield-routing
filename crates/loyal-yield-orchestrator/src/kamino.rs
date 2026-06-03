use std::str::FromStr;

use loyal_actions::{
    KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR, KAMINO_LEND_PROGRAM_ID,
    KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
};
use serde_json::Value;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    sysvar,
};

use crate::policy_execution::{compile_inner_instruction, CompiledInstructionSet};
use crate::OrchestratorError;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoReserveAccounts {
    pub reserve: Pubkey,
    pub lending_market: Pubkey,
    pub lending_market_authority: Pubkey,
    pub liquidity_mint: Pubkey,
    pub liquidity_supply: Pubkey,
    pub collateral_mint: Pubkey,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoVaultAccounts {
    pub owner: Pubkey,
    pub liquidity_token_account: Pubkey,
    pub collateral_token_account: Pubkey,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SameMintKaminoRouteAccounts {
    pub source_reserve: KaminoReserveAccounts,
    pub target_reserve: KaminoReserveAccounts,
    pub vault_owner: Pubkey,
    pub vault_liquidity_token_account: Pubkey,
    pub source_collateral_token_account: Pubkey,
    pub target_collateral_token_account: Pubkey,
}

impl KaminoReserveAccounts {
    pub fn from_metadata(metadata: &Value) -> Result<Self, OrchestratorError> {
        Ok(Self {
            reserve: parse_pubkey(metadata, "reserve")?,
            lending_market: parse_pubkey(metadata, "lending_market")?,
            lending_market_authority: parse_pubkey(metadata, "lending_market_authority")?,
            liquidity_mint: parse_pubkey(metadata, "liquidity_mint")?,
            liquidity_supply: parse_pubkey(metadata, "liquidity_supply")?,
            collateral_mint: parse_pubkey(metadata, "collateral_mint")?,
        })
    }
}

impl KaminoVaultAccounts {
    pub fn from_metadata(owner: Pubkey, metadata: &Value) -> Result<Self, OrchestratorError> {
        Ok(Self {
            owner,
            liquidity_token_account: parse_pubkey(metadata, "vault_liquidity_token_account")?,
            collateral_token_account: parse_pubkey(metadata, "vault_collateral_token_account")?,
        })
    }
}

impl SameMintKaminoRouteAccounts {
    pub fn validate(&self) -> Result<(), OrchestratorError> {
        if self.source_reserve.liquidity_mint != self.target_reserve.liquidity_mint {
            return Err(OrchestratorError::Execution(
                "same-mint Kamino route requires matching liquidity mints".to_owned(),
            ));
        }
        Ok(())
    }
}

pub fn kamino_deposit_reserve_liquidity_instruction(
    reserve: KaminoReserveAccounts,
    vault: KaminoVaultAccounts,
    amount: u64,
) -> Instruction {
    Instruction {
        program_id: KAMINO_LEND_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(vault.owner, false),
            AccountMeta::new(reserve.reserve, false),
            AccountMeta::new_readonly(reserve.lending_market, false),
            AccountMeta::new_readonly(reserve.lending_market_authority, false),
            AccountMeta::new_readonly(reserve.liquidity_mint, false),
            AccountMeta::new(reserve.liquidity_supply, false),
            AccountMeta::new(reserve.collateral_mint, false),
            AccountMeta::new(vault.liquidity_token_account, false),
            AccountMeta::new(vault.collateral_token_account, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(sysvar::instructions::id(), false),
        ],
        data: kamino_amount_data(KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR, amount),
    }
}

pub fn kamino_withdraw_reserve_liquidity_instruction(
    reserve: KaminoReserveAccounts,
    vault: KaminoVaultAccounts,
    amount: u64,
) -> Instruction {
    Instruction {
        program_id: KAMINO_LEND_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(vault.owner, false),
            AccountMeta::new_readonly(reserve.lending_market, false),
            AccountMeta::new(reserve.reserve, false),
            AccountMeta::new_readonly(reserve.lending_market_authority, false),
            AccountMeta::new_readonly(reserve.liquidity_mint, false),
            AccountMeta::new(reserve.collateral_mint, false),
            AccountMeta::new(reserve.liquidity_supply, false),
            AccountMeta::new(vault.collateral_token_account, false),
            AccountMeta::new(vault.liquidity_token_account, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(sysvar::instructions::id(), false),
        ],
        data: kamino_amount_data(KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR, amount),
    }
}

pub fn kamino_redeem_reserve_collateral_instruction(
    reserve: KaminoReserveAccounts,
    vault: KaminoVaultAccounts,
    amount: u64,
) -> Instruction {
    kamino_withdraw_reserve_liquidity_instruction(reserve, vault, amount)
}

pub fn same_mint_kamino_compiled_parts(
    accounts: SameMintKaminoRouteAccounts,
    amount: u64,
) -> Result<(CompiledInstructionSet, CompiledInstructionSet), OrchestratorError> {
    accounts.validate()?;
    let withdraw = kamino_redeem_reserve_collateral_instruction(
        accounts.source_reserve,
        KaminoVaultAccounts {
            owner: accounts.vault_owner,
            liquidity_token_account: accounts.vault_liquidity_token_account,
            collateral_token_account: accounts.source_collateral_token_account,
        },
        amount,
    );
    let deposit = kamino_deposit_reserve_liquidity_instruction(
        accounts.target_reserve,
        KaminoVaultAccounts {
            owner: accounts.vault_owner,
            liquidity_token_account: accounts.vault_liquidity_token_account,
            collateral_token_account: accounts.target_collateral_token_account,
        },
        amount,
    );

    Ok((
        compile_inner_instruction(withdraw),
        compile_inner_instruction(deposit),
    ))
}

fn kamino_amount_data(discriminator: [u8; 8], amount: u64) -> Vec<u8> {
    let mut data = Vec::with_capacity(16);
    data.extend_from_slice(&discriminator);
    data.extend_from_slice(&amount.to_le_bytes());
    data
}

fn parse_pubkey(metadata: &Value, key: &'static str) -> Result<Pubkey, OrchestratorError> {
    let value = metadata.get(key).and_then(Value::as_str).ok_or_else(|| {
        OrchestratorError::Execution(format!("missing Kamino metadata key {key}"))
    })?;
    Pubkey::from_str(value)
        .map_err(|error| OrchestratorError::Execution(format!("invalid pubkey for {key}: {error}")))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn reserve(liquidity_mint: Pubkey) -> KaminoReserveAccounts {
        KaminoReserveAccounts {
            reserve: Pubkey::new_unique(),
            lending_market: Pubkey::new_unique(),
            lending_market_authority: Pubkey::new_unique(),
            liquidity_mint,
            liquidity_supply: Pubkey::new_unique(),
            collateral_mint: Pubkey::new_unique(),
        }
    }

    #[test]
    fn kamino_deposit_instruction_matches_policy_account_order() {
        let liquidity_mint = Pubkey::new_unique();
        let reserve = reserve(liquidity_mint);
        let vault = KaminoVaultAccounts {
            owner: Pubkey::new_unique(),
            liquidity_token_account: Pubkey::new_unique(),
            collateral_token_account: Pubkey::new_unique(),
        };

        let ix = kamino_deposit_reserve_liquidity_instruction(reserve, vault, 42);

        assert_eq!(ix.program_id, KAMINO_LEND_PROGRAM_ID);
        assert_eq!(ix.accounts[0].pubkey, vault.owner);
        assert_eq!(ix.accounts[2].pubkey, reserve.lending_market);
        assert_eq!(ix.accounts[4].pubkey, liquidity_mint);
        assert_eq!(ix.accounts[8].pubkey, vault.collateral_token_account);
        assert_eq!(ix.accounts[10].pubkey, spl_token::id());
        assert_eq!(
            &ix.data[..8],
            &KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR
        );
        assert_eq!(u64::from_le_bytes(ix.data[8..16].try_into().unwrap()), 42);
    }

    #[test]
    fn kamino_withdraw_instruction_matches_policy_account_order() {
        let liquidity_mint = Pubkey::new_unique();
        let reserve = reserve(liquidity_mint);
        let vault = KaminoVaultAccounts {
            owner: Pubkey::new_unique(),
            liquidity_token_account: Pubkey::new_unique(),
            collateral_token_account: Pubkey::new_unique(),
        };

        let ix = kamino_withdraw_reserve_liquidity_instruction(reserve, vault, 88);

        assert_eq!(ix.accounts[0].pubkey, vault.owner);
        assert_eq!(ix.accounts[1].pubkey, reserve.lending_market);
        assert_eq!(ix.accounts[4].pubkey, liquidity_mint);
        assert_eq!(ix.accounts[8].pubkey, vault.liquidity_token_account);
        assert_eq!(ix.accounts[10].pubkey, spl_token::id());
        assert_eq!(
            &ix.data[..8],
            &KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR
        );
        assert_eq!(u64::from_le_bytes(ix.data[8..16].try_into().unwrap()), 88);
    }

    #[test]
    fn same_mint_compiled_parts_reject_mint_mismatch() {
        let err = same_mint_kamino_compiled_parts(
            SameMintKaminoRouteAccounts {
                source_reserve: reserve(Pubkey::new_unique()),
                target_reserve: reserve(Pubkey::new_unique()),
                vault_owner: Pubkey::new_unique(),
                vault_liquidity_token_account: Pubkey::new_unique(),
                source_collateral_token_account: Pubkey::new_unique(),
                target_collateral_token_account: Pubkey::new_unique(),
            },
            1,
        )
        .unwrap_err();

        assert!(err.to_string().contains("matching liquidity mints"));
    }

    #[test]
    fn same_mint_compiled_parts_match_squads_policy_indexes() {
        let mint = Pubkey::new_unique();
        let (withdraw, deposit) = same_mint_kamino_compiled_parts(
            SameMintKaminoRouteAccounts {
                source_reserve: reserve(mint),
                target_reserve: reserve(mint),
                vault_owner: Pubkey::new_unique(),
                vault_liquidity_token_account: Pubkey::new_unique(),
                source_collateral_token_account: Pubkey::new_unique(),
                target_collateral_token_account: Pubkey::new_unique(),
            },
            1,
        )
        .unwrap();

        assert_eq!(withdraw.accounts[10].pubkey, sysvar::instructions::id());
        assert_eq!(withdraw.accounts[11].pubkey, KAMINO_LEND_PROGRAM_ID);
        assert_eq!(withdraw.instructions[0].program_id_index, 11);
        assert_eq!(
            withdraw.instructions[0].accounts,
            vec![0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 9, 10]
        );
        assert_eq!(deposit.accounts[10].pubkey, sysvar::instructions::id());
        assert_eq!(deposit.accounts[11].pubkey, KAMINO_LEND_PROGRAM_ID);
        assert_eq!(deposit.instructions[0].program_id_index, 11);
        assert_eq!(
            deposit.instructions[0].accounts,
            vec![0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 9, 10]
        );
    }
}
