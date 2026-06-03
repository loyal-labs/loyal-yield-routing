use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    sysvar,
};

pub use loyal_actions::{
    KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR, KAMINO_LEND_PROGRAM_ID,
    KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoReserveInstructionAccounts {
    pub reserve: Pubkey,
    pub market: Pubkey,
    pub lending_market_authority: Pubkey,
    pub liquidity_mint: Pubkey,
    pub reserve_liquidity_supply: Pubkey,
    pub collateral_mint: Pubkey,
    pub vault_liquidity: Pubkey,
    pub vault_collateral: Pubkey,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoDepositReserveLiquidityArgs {
    pub vault: Pubkey,
    pub accounts: KaminoReserveInstructionAccounts,
    pub liquidity_amount: u64,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoRedeemReserveCollateralArgs {
    pub vault: Pubkey,
    pub accounts: KaminoReserveInstructionAccounts,
    pub collateral_amount: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SquadsPolicyInstructionPayload {
    pub instructions: Vec<SquadsPolicyCompiledInstruction>,
    pub accounts: Vec<AccountMeta>,
}

impl SquadsPolicyInstructionPayload {
    pub fn into_parts(self) -> (Vec<SquadsPolicyCompiledInstruction>, Vec<AccountMeta>) {
        (self.instructions, self.accounts)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SquadsPolicyCompiledInstruction {
    pub program_id_index: usize,
    pub accounts: Vec<usize>,
    pub data: Vec<u8>,
}

pub fn kamino_deposit_reserve_liquidity_data(liquidity_amount: u64) -> Vec<u8> {
    kamino_amount_data(
        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
        liquidity_amount,
    )
}

pub fn kamino_redeem_reserve_collateral_data(collateral_amount: u64) -> Vec<u8> {
    // The existing policy layer calls this leg "redeem" while the shared
    // discriminator constant keeps its historical withdraw-oriented name.
    kamino_amount_data(
        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
        collateral_amount,
    )
}

pub fn kamino_deposit_reserve_liquidity_instruction(
    args: KaminoDepositReserveLiquidityArgs,
) -> Instruction {
    Instruction {
        program_id: KAMINO_LEND_PROGRAM_ID,
        accounts: kamino_deposit_reserve_liquidity_accounts(args.vault, args.accounts, true),
        data: kamino_deposit_reserve_liquidity_data(args.liquidity_amount),
    }
}

pub fn kamino_redeem_reserve_collateral_instruction(
    args: KaminoRedeemReserveCollateralArgs,
) -> Instruction {
    Instruction {
        program_id: KAMINO_LEND_PROGRAM_ID,
        accounts: kamino_redeem_reserve_collateral_accounts(args.vault, args.accounts, true),
        data: kamino_redeem_reserve_collateral_data(args.collateral_amount),
    }
}

pub fn kamino_deposit_reserve_liquidity_policy_payload(
    args: KaminoDepositReserveLiquidityArgs,
) -> SquadsPolicyInstructionPayload {
    let instruction = Instruction {
        program_id: KAMINO_LEND_PROGRAM_ID,
        accounts: kamino_deposit_reserve_liquidity_accounts(args.vault, args.accounts, false),
        data: kamino_deposit_reserve_liquidity_data(args.liquidity_amount),
    };
    compile_for_squads_policy(instruction)
}

pub fn kamino_redeem_reserve_collateral_policy_payload(
    args: KaminoRedeemReserveCollateralArgs,
) -> SquadsPolicyInstructionPayload {
    let instruction = Instruction {
        program_id: KAMINO_LEND_PROGRAM_ID,
        accounts: kamino_redeem_reserve_collateral_accounts(args.vault, args.accounts, false),
        data: kamino_redeem_reserve_collateral_data(args.collateral_amount),
    };
    compile_for_squads_policy(instruction)
}

fn kamino_amount_data(discriminator: [u8; 8], amount: u64) -> Vec<u8> {
    let mut data = Vec::with_capacity(16);
    data.extend_from_slice(&discriminator);
    data.extend_from_slice(&amount.to_le_bytes());
    data
}

fn kamino_deposit_reserve_liquidity_accounts(
    vault: Pubkey,
    accounts: KaminoReserveInstructionAccounts,
    vault_is_signer: bool,
) -> Vec<AccountMeta> {
    let token_program = spl_token::id();
    vec![
        AccountMeta::new(vault, vault_is_signer),
        AccountMeta::new(accounts.reserve, false),
        AccountMeta::new_readonly(accounts.market, false),
        AccountMeta::new_readonly(accounts.lending_market_authority, false),
        AccountMeta::new_readonly(accounts.liquidity_mint, false),
        AccountMeta::new(accounts.reserve_liquidity_supply, false),
        AccountMeta::new(accounts.collateral_mint, false),
        AccountMeta::new(accounts.vault_liquidity, false),
        AccountMeta::new(accounts.vault_collateral, false),
        AccountMeta::new_readonly(token_program, false),
        AccountMeta::new_readonly(token_program, false),
        AccountMeta::new_readonly(sysvar::instructions::id(), false),
    ]
}

fn kamino_redeem_reserve_collateral_accounts(
    vault: Pubkey,
    accounts: KaminoReserveInstructionAccounts,
    vault_is_signer: bool,
) -> Vec<AccountMeta> {
    let token_program = spl_token::id();
    vec![
        AccountMeta::new(vault, vault_is_signer),
        AccountMeta::new_readonly(accounts.market, false),
        AccountMeta::new(accounts.reserve, false),
        AccountMeta::new_readonly(accounts.lending_market_authority, false),
        AccountMeta::new_readonly(accounts.liquidity_mint, false),
        AccountMeta::new(accounts.collateral_mint, false),
        AccountMeta::new(accounts.reserve_liquidity_supply, false),
        AccountMeta::new(accounts.vault_collateral, false),
        AccountMeta::new(accounts.vault_liquidity, false),
        AccountMeta::new_readonly(token_program, false),
        AccountMeta::new_readonly(token_program, false),
        AccountMeta::new_readonly(sysvar::instructions::id(), false),
    ]
}

fn compile_for_squads_policy(instruction: Instruction) -> SquadsPolicyInstructionPayload {
    let mut transaction_accounts = Vec::new();
    let accounts = instruction
        .accounts
        .into_iter()
        .map(|account| push_or_update_account_meta(&mut transaction_accounts, account))
        .collect();
    let program_id_index = push_or_update_account_meta(
        &mut transaction_accounts,
        AccountMeta::new_readonly(instruction.program_id, false),
    );

    SquadsPolicyInstructionPayload {
        instructions: vec![SquadsPolicyCompiledInstruction {
            program_id_index,
            accounts,
            data: instruction.data,
        }],
        accounts: transaction_accounts,
    }
}

fn push_or_update_account_meta(accounts: &mut Vec<AccountMeta>, meta: AccountMeta) -> usize {
    if let Some(index) = accounts
        .iter()
        .position(|existing| existing.pubkey == meta.pubkey)
    {
        accounts[index].is_writable |= meta.is_writable;
        accounts[index].is_signer |= meta.is_signer;
        return index;
    }

    let index = accounts.len();
    accounts.push(meta);
    index
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key(seed: u8) -> Pubkey {
        Pubkey::new_from_array([seed; 32])
    }

    fn reserve_accounts() -> KaminoReserveInstructionAccounts {
        KaminoReserveInstructionAccounts {
            reserve: key(1),
            market: key(2),
            lending_market_authority: key(3),
            liquidity_mint: key(4),
            reserve_liquidity_supply: key(5),
            collateral_mint: key(6),
            vault_liquidity: key(7),
            vault_collateral: key(8),
        }
    }

    #[test]
    fn builds_deposit_reserve_liquidity_instruction() {
        let vault = key(42);
        let accounts = reserve_accounts();
        let instruction =
            kamino_deposit_reserve_liquidity_instruction(KaminoDepositReserveLiquidityArgs {
                vault,
                accounts,
                liquidity_amount: 1_000_000,
            });

        assert_eq!(instruction.program_id, KAMINO_LEND_PROGRAM_ID);
        assert_eq!(
            instruction.data,
            [
                KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR.as_slice(),
                1_000_000_u64.to_le_bytes().as_slice(),
            ]
            .concat()
        );
        assert_eq!(
            instruction.accounts,
            vec![
                AccountMeta::new(vault, true),
                AccountMeta::new(accounts.reserve, false),
                AccountMeta::new_readonly(accounts.market, false),
                AccountMeta::new_readonly(accounts.lending_market_authority, false),
                AccountMeta::new_readonly(accounts.liquidity_mint, false),
                AccountMeta::new(accounts.reserve_liquidity_supply, false),
                AccountMeta::new(accounts.collateral_mint, false),
                AccountMeta::new(accounts.vault_liquidity, false),
                AccountMeta::new(accounts.vault_collateral, false),
                AccountMeta::new_readonly(spl_token::id(), false),
                AccountMeta::new_readonly(spl_token::id(), false),
                AccountMeta::new_readonly(sysvar::instructions::id(), false),
            ]
        );
    }

    #[test]
    fn builds_redeem_reserve_collateral_instruction() {
        let vault = key(42);
        let accounts = reserve_accounts();
        let instruction =
            kamino_redeem_reserve_collateral_instruction(KaminoRedeemReserveCollateralArgs {
                vault,
                accounts,
                collateral_amount: 2_000_000,
            });

        assert_eq!(instruction.program_id, KAMINO_LEND_PROGRAM_ID);
        assert_eq!(
            instruction.data,
            [
                KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR.as_slice(),
                2_000_000_u64.to_le_bytes().as_slice(),
            ]
            .concat()
        );
        assert_eq!(
            instruction.accounts,
            vec![
                AccountMeta::new(vault, true),
                AccountMeta::new_readonly(accounts.market, false),
                AccountMeta::new(accounts.reserve, false),
                AccountMeta::new_readonly(accounts.lending_market_authority, false),
                AccountMeta::new_readonly(accounts.liquidity_mint, false),
                AccountMeta::new(accounts.collateral_mint, false),
                AccountMeta::new(accounts.reserve_liquidity_supply, false),
                AccountMeta::new(accounts.vault_collateral, false),
                AccountMeta::new(accounts.vault_liquidity, false),
                AccountMeta::new_readonly(spl_token::id(), false),
                AccountMeta::new_readonly(spl_token::id(), false),
                AccountMeta::new_readonly(sysvar::instructions::id(), false),
            ]
        );
    }

    #[test]
    fn compiles_policy_payload_with_policy_safe_vault_account() {
        let vault = key(42);
        let payload =
            kamino_deposit_reserve_liquidity_policy_payload(KaminoDepositReserveLiquidityArgs {
                vault,
                accounts: reserve_accounts(),
                liquidity_amount: 1_000_000,
            });

        assert_eq!(payload.instructions.len(), 1);
        assert_eq!(payload.accounts.len(), 12);
        assert_eq!(payload.accounts[0], AccountMeta::new(vault, false));
        assert_eq!(
            payload.accounts[11],
            AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false)
        );
        assert_eq!(payload.instructions[0].program_id_index, 11);
        assert_eq!(
            payload.instructions[0].accounts,
            vec![0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 9, 10]
        );
        assert_eq!(
            payload.instructions[0].data,
            kamino_deposit_reserve_liquidity_data(1_000_000)
        );
    }
}
