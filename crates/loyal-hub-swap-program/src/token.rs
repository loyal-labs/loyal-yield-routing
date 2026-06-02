#[cfg(not(kani))]
use pinocchio::instruction::{Seed, Signer};
use pinocchio::{
    account_info::AccountInfo, program_error::ProgramError, pubkey::Pubkey, ProgramResult,
};
#[cfg(not(kani))]
use pinocchio_tkn::common::TransferChecked;
use pinocchio_tkn::state::{Mint, TokenAccount};

#[cfg(not(kani))]
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

#[cfg(not(kani))]
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

#[cfg(kani)]
pub fn transfer_checked(
    source: &AccountInfo,
    mint: &AccountInfo,
    destination: &AccountInfo,
    authority: &AccountInfo,
    token_program: &AccountInfo,
    amount: u64,
    decimals: u8,
) -> ProgramResult {
    if !authority.is_signer() {
        return Err(ProgramError::MissingRequiredSignature);
    }
    invoke_token_transfer_checked(
        source,
        mint,
        destination,
        authority,
        token_program,
        amount,
        decimals,
    )
}

#[cfg(not(kani))]
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

#[cfg(kani)]
pub fn transfer_checked_signed(
    _program_id: &Pubkey,
    source: &AccountInfo,
    mint: &AccountInfo,
    destination: &AccountInfo,
    authority: &AccountInfo,
    token_program: &AccountInfo,
    amount: u64,
    decimals: u8,
    _lane_id: u8,
) -> ProgramResult {
    invoke_token_transfer_checked(
        source,
        mint,
        destination,
        authority,
        token_program,
        amount,
        decimals,
    )
}

#[cfg(not(kani))]
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

#[cfg(kani)]
fn invoke_token_transfer_checked(
    source: &AccountInfo,
    mint: &AccountInfo,
    destination: &AccountInfo,
    _authority: &AccountInfo,
    token_program: &AccountInfo,
    amount: u64,
    decimals: u8,
) -> ProgramResult {
    if token_program.key() != &pinocchio_tkn::TOKEN_PROGRAM_ID {
        return Err(ProgramError::InvalidArgument);
    }
    if read_mint_decimals(mint)? != decimals {
        return Err(ProgramError::InvalidArgument);
    }

    let source_amount = read_token_amount(source)?;
    let destination_amount = read_token_amount(destination)?;
    let new_source_amount = source_amount
        .checked_sub(amount)
        .ok_or(ProgramError::InvalidArgument)?;
    let new_destination_amount = destination_amount
        .checked_add(amount)
        .ok_or(ProgramError::InvalidArgument)?;

    write_token_amount(source, new_source_amount)?;
    write_token_amount(destination, new_destination_amount)
}

fn read_legacy_token_account(account: &AccountInfo) -> Result<&TokenAccount, ProgramError> {
    if account.owner() != &pinocchio_tkn::TOKEN_PROGRAM_ID {
        return Err(ProgramError::InvalidAccountData);
    }
    TokenAccount::from_account_info(account)
}

#[cfg(kani)]
fn read_token_amount(account: &AccountInfo) -> Result<u64, ProgramError> {
    let token = read_legacy_token_account(account)?;
    Ok(token.amount())
}

#[cfg(kani)]
fn write_token_amount(account: &AccountInfo, amount: u64) -> ProgramResult {
    if account.owner() != &pinocchio_tkn::TOKEN_PROGRAM_ID {
        return Err(ProgramError::InvalidAccountData);
    }
    let mut data = account.try_borrow_mut_data()?;
    if data.len() < pinocchio_tkn::state::TOKEN_ACCOUNT_SIZE {
        return Err(ProgramError::InvalidAccountData);
    }
    data[64..72].copy_from_slice(&amount.to_le_bytes());
    Ok(())
}
