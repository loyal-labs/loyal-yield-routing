use pinocchio::{
    account_info::AccountInfo,
    instruction::{Seed, Signer},
    program_error::ProgramError,
    pubkey::Pubkey,
    ProgramResult,
};
use pinocchio_tkn::{
    common::TransferChecked,
    state::{Mint, TokenAccount},
};

use crate::{constants::HUB_AUTHORITY_SEED, state::derive_hub_authority};

pub fn require_token_account(
    account: &AccountInfo,
    mint: &Pubkey,
    owner: &Pubkey,
) -> ProgramResult {
    let token = read_legacy_token_account(account)?;
    if token.mint() != mint || token.owner() != owner {
        return Err(ProgramError::InvalidAccountData);
    }
    Ok(())
}

pub fn require_matching_token_mint(account: &AccountInfo, mint: &Pubkey) -> ProgramResult {
    let token = read_legacy_token_account(account)?;
    if token.mint() != mint {
        return Err(ProgramError::InvalidAccountData);
    }
    Ok(())
}

pub fn read_mint_decimals(mint: &AccountInfo) -> Result<u8, ProgramError> {
    if mint.owner() != &pinocchio_tkn::TOKEN_PROGRAM_ID {
        return Err(ProgramError::InvalidAccountData);
    }
    Ok(Mint::from_account_info(mint)?.decimals())
}

pub fn transfer_checked(
    source: &AccountInfo,
    mint: &AccountInfo,
    destination: &AccountInfo,
    authority: &AccountInfo,
    token_program: &AccountInfo,
    amount: u64,
    decimals: u8,
) -> ProgramResult {
    invoke_token_transfer_checked(
        source,
        mint,
        destination,
        authority,
        token_program,
        amount,
        decimals,
        &[],
    )
}

pub fn transfer_checked_signed(
    program_id: &Pubkey,
    source: &AccountInfo,
    mint: &AccountInfo,
    destination: &AccountInfo,
    authority: &AccountInfo,
    token_program: &AccountInfo,
    amount: u64,
    decimals: u8,
    lane_id: u8,
) -> ProgramResult {
    let (_, bump) = derive_hub_authority(program_id, lane_id);
    let lane_seed = [lane_id];
    let bump_seed = [bump];
    let seeds = [
        Seed::from(HUB_AUTHORITY_SEED),
        Seed::from(&lane_seed),
        Seed::from(&bump_seed),
    ];
    let signer = Signer::from(&seeds);
    invoke_token_transfer_checked(
        source,
        mint,
        destination,
        authority,
        token_program,
        amount,
        decimals,
        &[signer],
    )
}

fn invoke_token_transfer_checked(
    source: &AccountInfo,
    mint: &AccountInfo,
    destination: &AccountInfo,
    authority: &AccountInfo,
    token_program: &AccountInfo,
    amount: u64,
    decimals: u8,
    signers: &[Signer],
) -> ProgramResult {
    if token_program.key() != &pinocchio_tkn::TOKEN_PROGRAM_ID {
        return Err(ProgramError::InvalidArgument);
    }
    TransferChecked {
        source,
        mint,
        destination,
        authority,
        amount,
        decimals,
        program_id: Some(token_program.key()),
    }
    .invoke_signed(signers)
}

fn read_legacy_token_account(account: &AccountInfo) -> Result<&TokenAccount, ProgramError> {
    if account.owner() != &pinocchio_tkn::TOKEN_PROGRAM_ID {
        return Err(ProgramError::InvalidAccountData);
    }
    TokenAccount::from_account_info(account)
}
