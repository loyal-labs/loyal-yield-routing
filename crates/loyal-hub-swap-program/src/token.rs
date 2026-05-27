use solana_program::{
    account_info::AccountInfo,
    entrypoint::ProgramResult,
    program::{invoke, invoke_signed},
    program_error::ProgramError,
    pubkey::Pubkey,
};
use spl_token::solana_program::program_pack::Pack;

use crate::{constants::HUB_AUTHORITY_SEED, state::derive_hub_authority};

pub fn require_token_account(
    account: &AccountInfo,
    mint: &Pubkey,
    owner: &Pubkey,
) -> ProgramResult {
    let token = spl_token::state::Account::unpack(&account.data.borrow())?;
    if account.owner != &spl_token::id() || token.mint != *mint || token.owner != *owner {
        return Err(ProgramError::InvalidAccountData);
    }
    Ok(())
}

pub fn require_matching_token_mint(account: &AccountInfo, mint: &Pubkey) -> ProgramResult {
    let token = spl_token::state::Account::unpack(&account.data.borrow())?;
    if account.owner != &spl_token::id() || token.mint != *mint {
        return Err(ProgramError::InvalidAccountData);
    }
    Ok(())
}

pub fn read_mint_decimals(mint: &AccountInfo) -> Result<u8, ProgramError> {
    if mint.owner != &spl_token::id() {
        return Err(ProgramError::InvalidAccountData);
    }
    Ok(spl_token::state::Mint::unpack(&mint.data.borrow())?.decimals)
}

pub fn transfer_checked<'info>(
    source: &AccountInfo<'info>,
    mint: &AccountInfo<'info>,
    destination: &AccountInfo<'info>,
    authority: &AccountInfo<'info>,
    token_program: &AccountInfo<'info>,
    amount: u64,
    decimals: u8,
) -> ProgramResult {
    let ix = spl_token::instruction::transfer_checked(
        token_program.key,
        source.key,
        mint.key,
        destination.key,
        authority.key,
        &[],
        amount,
        decimals,
    )?;
    invoke(
        &ix,
        &[
            source.clone(),
            mint.clone(),
            destination.clone(),
            authority.clone(),
            token_program.clone(),
        ],
    )
}

pub fn transfer_checked_signed<'info>(
    program_id: &Pubkey,
    source: &AccountInfo<'info>,
    mint: &AccountInfo<'info>,
    destination: &AccountInfo<'info>,
    authority: &AccountInfo<'info>,
    token_program: &AccountInfo<'info>,
    amount: u64,
    decimals: u8,
    lane_id: u8,
) -> ProgramResult {
    let ix = spl_token::instruction::transfer_checked(
        token_program.key,
        source.key,
        mint.key,
        destination.key,
        authority.key,
        &[],
        amount,
        decimals,
    )?;
    let account_infos = [
        source.clone(),
        mint.clone(),
        destination.clone(),
        authority.clone(),
        token_program.clone(),
    ];
    let (_, bump) = derive_hub_authority(program_id, lane_id);
    invoke_signed(
        &ix,
        &account_infos,
        &[&[HUB_AUTHORITY_SEED, &[lane_id], &[bump]]],
    )
}
