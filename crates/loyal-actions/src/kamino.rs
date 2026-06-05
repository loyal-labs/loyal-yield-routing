use crate::ids::{
    KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR, KAMINO_LEND_PROGRAM_ID,
    KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    sysvar,
};

pub const KAMINO_LENDING_PROGRAM_ID: Pubkey = KAMINO_LEND_PROGRAM_ID;
pub const KAMINO_RESERVE_AMOUNT_DATA_LEN: usize = 16;
pub const KAMINO_REDEEM_RESERVE_COLLATERAL_DISCRIMINATOR: [u8; 8] =
    KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoDepositReserveLiquidityAccounts {
    pub owner: Pubkey,
    pub reserve: Pubkey,
    pub lending_market: Pubkey,
    pub lending_market_authority: Pubkey,
    pub reserve_liquidity_mint: Pubkey,
    pub reserve_liquidity_supply: Pubkey,
    pub reserve_collateral_mint: Pubkey,
    pub user_source_liquidity: Pubkey,
    pub user_destination_collateral: Pubkey,
    pub liquidity_token_program: Pubkey,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoRedeemReserveCollateralAccounts {
    pub owner: Pubkey,
    pub lending_market: Pubkey,
    pub reserve: Pubkey,
    pub lending_market_authority: Pubkey,
    pub reserve_liquidity_mint: Pubkey,
    pub reserve_collateral_mint: Pubkey,
    pub reserve_liquidity_supply: Pubkey,
    pub user_source_collateral: Pubkey,
    pub user_destination_liquidity: Pubkey,
    pub liquidity_token_program: Pubkey,
}

pub type KaminoWithdrawReserveLiquidityAccounts = KaminoRedeemReserveCollateralAccounts;

pub fn kamino_deposit_reserve_liquidity_instruction(
    accounts: KaminoDepositReserveLiquidityAccounts,
    liquidity_amount: u64,
) -> Instruction {
    Instruction {
        program_id: KAMINO_LENDING_PROGRAM_ID,
        accounts: vec![
            signer(accounts.owner),
            writable(accounts.reserve),
            readonly(accounts.lending_market),
            readonly(accounts.lending_market_authority),
            readonly(accounts.reserve_liquidity_mint),
            writable(accounts.reserve_liquidity_supply),
            writable(accounts.reserve_collateral_mint),
            writable(accounts.user_source_liquidity),
            writable(accounts.user_destination_collateral),
            readonly(spl_token::ID),
            readonly(accounts.liquidity_token_program),
            readonly(sysvar::instructions::id()),
        ],
        data: kamino_deposit_reserve_liquidity_data(liquidity_amount),
    }
}

pub fn kamino_redeem_reserve_collateral_instruction(
    accounts: KaminoRedeemReserveCollateralAccounts,
    collateral_amount: u64,
) -> Instruction {
    Instruction {
        program_id: KAMINO_LENDING_PROGRAM_ID,
        accounts: vec![
            signer(accounts.owner),
            readonly(accounts.lending_market),
            writable(accounts.reserve),
            readonly(accounts.lending_market_authority),
            readonly(accounts.reserve_liquidity_mint),
            writable(accounts.reserve_collateral_mint),
            writable(accounts.reserve_liquidity_supply),
            writable(accounts.user_source_collateral),
            writable(accounts.user_destination_liquidity),
            readonly(spl_token::ID),
            readonly(accounts.liquidity_token_program),
            readonly(sysvar::instructions::id()),
        ],
        data: kamino_redeem_reserve_collateral_data(collateral_amount),
    }
}

pub fn kamino_withdraw_reserve_liquidity_instruction(
    accounts: KaminoWithdrawReserveLiquidityAccounts,
    collateral_amount: u64,
) -> Instruction {
    kamino_redeem_reserve_collateral_instruction(accounts, collateral_amount)
}

pub fn kamino_deposit_reserve_liquidity_data(liquidity_amount: u64) -> Vec<u8> {
    kamino_amount_data(
        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
        liquidity_amount,
    )
}

pub fn kamino_redeem_reserve_collateral_data(collateral_amount: u64) -> Vec<u8> {
    kamino_amount_data(
        KAMINO_REDEEM_RESERVE_COLLATERAL_DISCRIMINATOR,
        collateral_amount,
    )
}

pub fn kamino_withdraw_reserve_liquidity_data(collateral_amount: u64) -> Vec<u8> {
    kamino_redeem_reserve_collateral_data(collateral_amount)
}

fn kamino_amount_data(discriminator: [u8; 8], amount: u64) -> Vec<u8> {
    let mut data = Vec::with_capacity(KAMINO_RESERVE_AMOUNT_DATA_LEN);
    data.extend_from_slice(&discriminator);
    data.extend_from_slice(&amount.to_le_bytes());
    data
}

fn readonly(pubkey: Pubkey) -> AccountMeta {
    AccountMeta::new_readonly(pubkey, false)
}

fn writable(pubkey: Pubkey) -> AccountMeta {
    AccountMeta::new(pubkey, false)
}

fn signer(pubkey: Pubkey) -> AccountMeta {
    AccountMeta::new_readonly(pubkey, true)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deposit_reserve_liquidity_matches_klend_wire_layout() {
        let accounts = deposit_accounts();
        let amount = 1_234_567;

        let ix = kamino_deposit_reserve_liquidity_instruction(accounts, amount);

        assert_eq!(ix.program_id, KAMINO_LENDING_PROGRAM_ID);
        assert_eq!(
            KAMINO_LENDING_PROGRAM_ID.to_bytes(),
            klend_interface::KLEND_PROGRAM_ID.to_bytes()
        );
        assert_eq!(
            &ix.data[..8],
            klend_interface::discriminators::DEPOSIT_RESERVE_LIQUIDITY.as_ref()
        );
        assert_eq!(decoded_amount(&ix.data), amount);
        assert_eq!(ix.accounts.len(), 12);
        assert_meta(&ix.accounts[0], accounts.owner, true, false);
        assert_meta(&ix.accounts[1], accounts.reserve, false, true);
        assert_meta(&ix.accounts[2], accounts.lending_market, false, false);
        assert_meta(
            &ix.accounts[3],
            accounts.lending_market_authority,
            false,
            false,
        );
        assert_meta(
            &ix.accounts[4],
            accounts.reserve_liquidity_mint,
            false,
            false,
        );
        assert_meta(
            &ix.accounts[5],
            accounts.reserve_liquidity_supply,
            false,
            true,
        );
        assert_meta(
            &ix.accounts[6],
            accounts.reserve_collateral_mint,
            false,
            true,
        );
        assert_meta(&ix.accounts[7], accounts.user_source_liquidity, false, true);
        assert_meta(
            &ix.accounts[8],
            accounts.user_destination_collateral,
            false,
            true,
        );
        assert_meta(&ix.accounts[9], spl_token::ID, false, false);
        assert_meta(
            &ix.accounts[10],
            accounts.liquidity_token_program,
            false,
            false,
        );
        assert_meta(&ix.accounts[11], sysvar::instructions::id(), false, false);
    }

    #[test]
    fn redeem_reserve_collateral_matches_klend_wire_layout() {
        let accounts = redeem_accounts();
        let amount = 7_654_321;

        let ix = kamino_redeem_reserve_collateral_instruction(accounts, amount);

        assert_eq!(ix.program_id, KAMINO_LENDING_PROGRAM_ID);
        assert_eq!(
            &ix.data[..8],
            klend_interface::discriminators::REDEEM_RESERVE_COLLATERAL.as_ref()
        );
        assert_eq!(decoded_amount(&ix.data), amount);
        assert_eq!(ix.accounts.len(), 12);
        assert_meta(&ix.accounts[0], accounts.owner, true, false);
        assert_meta(&ix.accounts[1], accounts.lending_market, false, false);
        assert_meta(&ix.accounts[2], accounts.reserve, false, true);
        assert_meta(
            &ix.accounts[3],
            accounts.lending_market_authority,
            false,
            false,
        );
        assert_meta(
            &ix.accounts[4],
            accounts.reserve_liquidity_mint,
            false,
            false,
        );
        assert_meta(
            &ix.accounts[5],
            accounts.reserve_collateral_mint,
            false,
            true,
        );
        assert_meta(
            &ix.accounts[6],
            accounts.reserve_liquidity_supply,
            false,
            true,
        );
        assert_meta(
            &ix.accounts[7],
            accounts.user_source_collateral,
            false,
            true,
        );
        assert_meta(
            &ix.accounts[8],
            accounts.user_destination_liquidity,
            false,
            true,
        );
        assert_meta(&ix.accounts[9], spl_token::ID, false, false);
        assert_meta(
            &ix.accounts[10],
            accounts.liquidity_token_program,
            false,
            false,
        );
        assert_meta(&ix.accounts[11], sysvar::instructions::id(), false, false);
    }

    #[test]
    fn policy_discriminators_match_klend_interface() {
        assert_eq!(
            KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
            klend_interface::discriminators::DEPOSIT_RESERVE_LIQUIDITY
        );
        assert_eq!(
            KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
            klend_interface::discriminators::REDEEM_RESERVE_COLLATERAL
        );
        assert_eq!(
            KAMINO_REDEEM_RESERVE_COLLATERAL_DISCRIMINATOR,
            KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR
        );
    }

    #[test]
    fn withdraw_alias_uses_redeem_reserve_collateral_layout() {
        let accounts = redeem_accounts();
        let amount = 42;

        assert_eq!(
            kamino_withdraw_reserve_liquidity_instruction(accounts, amount),
            kamino_redeem_reserve_collateral_instruction(accounts, amount)
        );
        assert_eq!(
            kamino_withdraw_reserve_liquidity_data(amount),
            kamino_redeem_reserve_collateral_data(amount)
        );
    }

    fn deposit_accounts() -> KaminoDepositReserveLiquidityAccounts {
        KaminoDepositReserveLiquidityAccounts {
            owner: Pubkey::new_unique(),
            reserve: Pubkey::new_unique(),
            lending_market: Pubkey::new_unique(),
            lending_market_authority: Pubkey::new_unique(),
            reserve_liquidity_mint: Pubkey::new_unique(),
            reserve_liquidity_supply: Pubkey::new_unique(),
            reserve_collateral_mint: Pubkey::new_unique(),
            user_source_liquidity: Pubkey::new_unique(),
            user_destination_collateral: Pubkey::new_unique(),
            liquidity_token_program: Pubkey::new_unique(),
        }
    }

    fn redeem_accounts() -> KaminoRedeemReserveCollateralAccounts {
        KaminoRedeemReserveCollateralAccounts {
            owner: Pubkey::new_unique(),
            lending_market: Pubkey::new_unique(),
            reserve: Pubkey::new_unique(),
            lending_market_authority: Pubkey::new_unique(),
            reserve_liquidity_mint: Pubkey::new_unique(),
            reserve_collateral_mint: Pubkey::new_unique(),
            reserve_liquidity_supply: Pubkey::new_unique(),
            user_source_collateral: Pubkey::new_unique(),
            user_destination_liquidity: Pubkey::new_unique(),
            liquidity_token_program: Pubkey::new_unique(),
        }
    }

    fn assert_meta(meta: &AccountMeta, pubkey: Pubkey, is_signer: bool, is_writable: bool) {
        assert_eq!(meta.pubkey, pubkey);
        assert_eq!(meta.is_signer, is_signer);
        assert_eq!(meta.is_writable, is_writable);
    }

    fn decoded_amount(data: &[u8]) -> u64 {
        assert_eq!(data.len(), KAMINO_RESERVE_AMOUNT_DATA_LEN);
        u64::from_le_bytes(data[8..16].try_into().unwrap())
    }
}
