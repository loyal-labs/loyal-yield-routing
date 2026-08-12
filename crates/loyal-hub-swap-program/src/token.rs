#[cfg(not(kani))]
use pinocchio::instruction::{Seed, Signer};
use pinocchio::{
    account_info::AccountInfo, program_error::ProgramError, pubkey::Pubkey, ProgramResult,
};
#[cfg(not(kani))]
use pinocchio_tkn::common::TransferChecked;
#[cfg(not(kani))]
use pinocchio_tkn::state::Mint;
use pinocchio_tkn::state::TokenAccount;

#[cfg(not(kani))]
use crate::{constants::HUB_AUTHORITY_SEED, state::derive_hub_authority};

pub fn require_token_account_for_program(
    account: &AccountInfo,
    mint: &Pubkey,
    owner: &Pubkey,
    token_program_id: &Pubkey,
) -> ProgramResult {
    #[cfg(kani)]
    {
        if !pubkey_eq_kani(account.owner(), token_program_id)
            || !token_account_matches_kani(account, mint, owner)
        {
            return Err(ProgramError::InvalidAccountData);
        }
        return Ok(());
    }

    #[cfg(not(kani))]
    {
        let token = read_token_account_for_program(account, token_program_id)?;
        if token.mint() != mint || token.owner() != owner {
            return Err(ProgramError::InvalidAccountData);
        }
        Ok(())
    }
}

pub fn require_matching_token_mint_for_program(
    account: &AccountInfo,
    mint: &Pubkey,
    token_program_id: &Pubkey,
) -> ProgramResult {
    #[cfg(kani)]
    {
        if !pubkey_eq_kani(account.owner(), token_program_id)
            || !token_account_mint_matches_kani(account, mint)
        {
            return Err(ProgramError::InvalidAccountData);
        }
        return Ok(());
    }

    #[cfg(not(kani))]
    {
        let token = read_token_account_for_program(account, token_program_id)?;
        if token.mint() != mint {
            return Err(ProgramError::InvalidAccountData);
        }
        Ok(())
    }
}

pub fn require_supported_token_program(token_program: &AccountInfo) -> ProgramResult {
    if !is_supported_token_program_id(token_program.key()) {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

pub fn read_mint_decimals_for_program(
    mint: &AccountInfo,
    token_program_id: &Pubkey,
) -> Result<u8, ProgramError> {
    #[cfg(kani)]
    {
        if !is_supported_token_program_id(token_program_id)
            || !pubkey_eq_kani(mint.owner(), token_program_id)
        {
            return Err(ProgramError::InvalidAccountData);
        }
        let data = unsafe { mint.borrow_data_unchecked() };
        if data.len() < pinocchio_tkn::state::MINT_SIZE || data[45] != 1 {
            return Err(ProgramError::InvalidAccountData);
        }
        return Ok(data[44]);
    }

    #[cfg(not(kani))]
    {
        if !is_supported_token_program_id(token_program_id) || mint.owner() != token_program_id {
            return Err(ProgramError::InvalidAccountData);
        }
        Ok(Mint::from_account_info(mint)?.decimals())
    }
}

pub fn token_program_for_mint<'a>(
    mint: &AccountInfo,
    legacy_token_program: &'a AccountInfo,
    token_2022_program: Option<&'a AccountInfo>,
) -> Result<&'a AccountInfo, ProgramError> {
    if mint.owner() == legacy_token_program.key() {
        if legacy_token_program.key() != &pinocchio_tkn::TOKEN_PROGRAM_ID {
            return Err(ProgramError::InvalidArgument);
        }
        return Ok(legacy_token_program);
    }

    if mint.owner() == &pinocchio_tkn::TOKEN_2022_PROGRAM_ID {
        let token_2022_program = token_2022_program.ok_or(ProgramError::NotEnoughAccountKeys)?;
        if token_2022_program.key() != &pinocchio_tkn::TOKEN_2022_PROGRAM_ID {
            return Err(ProgramError::InvalidArgument);
        }
        return Ok(token_2022_program);
    }

    Err(ProgramError::InvalidAccountData)
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
#[allow(clippy::too_many_arguments)]
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
#[allow(clippy::too_many_arguments)]
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
    if !is_supported_token_program_id(token_program.key()) || mint.owner() != token_program.key() {
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
    if !is_supported_token_program_id(token_program.key())
        || !pubkey_eq_kani(mint.owner(), token_program.key())
    {
        return Err(ProgramError::InvalidArgument);
    }
    if read_mint_decimals_unchecked_kani(mint) != decimals {
        return Err(ProgramError::InvalidArgument);
    }

    if amount > 1_000_000
        || !source.is_writable()
        || !destination.is_writable()
        || !token_account_data_valid_kani(source)
        || !token_account_data_valid_kani(destination)
        || !token_amount_high_zero_kani(source)
        || !token_amount_high_zero_kani(destination)
    {
        return Err(ProgramError::InvalidArgument);
    }
    let source_amount = read_token_amount_bounded_kani(source);
    let destination_amount = read_token_amount_bounded_kani(destination);
    if source_amount < amount || destination_amount > 1_000_000 {
        return Err(ProgramError::InvalidArgument);
    }

    let new_source_amount = source_amount - amount;
    let new_destination_amount = destination_amount + amount;
    write_token_amount_bounded_kani(source, new_source_amount);
    write_token_amount_bounded_kani(destination, new_destination_amount);
    Ok(())
}

fn read_token_account_for_program<'a>(
    account: &'a AccountInfo,
    token_program_id: &Pubkey,
) -> Result<&'a TokenAccount, ProgramError> {
    if !is_supported_token_program_id(token_program_id) || account.owner() != token_program_id {
        return Err(ProgramError::InvalidAccountData);
    }
    TokenAccount::from_account_info(account)
}

fn is_supported_token_program_id(token_program_id: &Pubkey) -> bool {
    token_program_id == &pinocchio_tkn::TOKEN_PROGRAM_ID
        || token_program_id == &pinocchio_tkn::TOKEN_2022_PROGRAM_ID
}

#[cfg(kani)]
fn read_token_amount(account: &AccountInfo) -> Result<u64, ProgramError> {
    if !token_account_data_valid_kani(account) {
        return Err(ProgramError::InvalidAccountData);
    }
    Ok(read_token_amount_unchecked_kani(account))
}

#[cfg(kani)]
fn read_token_amount_unchecked_kani(account: &AccountInfo) -> u64 {
    let data = unsafe { account.borrow_data_unchecked() };
    u64::from_le_bytes([
        data[64], data[65], data[66], data[67], data[68], data[69], data[70], data[71],
    ])
}

#[cfg(kani)]
pub(crate) fn read_token_amount_bounded_kani(account: &AccountInfo) -> u64 {
    let data = unsafe { account.borrow_data_unchecked() };
    u32::from_le_bytes([data[64], data[65], data[66], data[67]]) as u64
}

#[cfg(kani)]
pub(crate) fn token_amount_high_zero_kani(account: &AccountInfo) -> bool {
    let data = unsafe { account.borrow_data_unchecked() };
    data[68] == 0 && data[69] == 0 && data[70] == 0 && data[71] == 0
}

#[cfg(kani)]
fn read_mint_decimals_unchecked_kani(mint: &AccountInfo) -> u8 {
    let data = unsafe { mint.borrow_data_unchecked() };
    data[44]
}

#[cfg(kani)]
fn write_token_amount(account: &AccountInfo, amount: u64) -> ProgramResult {
    if !pubkey_eq_kani(account.owner(), &pinocchio_tkn::TOKEN_PROGRAM_ID) {
        return Err(ProgramError::InvalidAccountData);
    }
    let data = unsafe { account.borrow_mut_data_unchecked() };
    if data.len() < pinocchio_tkn::state::TOKEN_ACCOUNT_SIZE {
        return Err(ProgramError::InvalidAccountData);
    }
    if data[108] != 1 {
        return Err(ProgramError::InvalidAccountData);
    }
    let bytes = amount.to_le_bytes();
    data[64] = bytes[0];
    data[65] = bytes[1];
    data[66] = bytes[2];
    data[67] = bytes[3];
    data[68] = bytes[4];
    data[69] = bytes[5];
    data[70] = bytes[6];
    data[71] = bytes[7];
    Ok(())
}

#[cfg(kani)]
fn write_token_amount_unchecked_kani(account: &AccountInfo, amount: u64) {
    let data = unsafe { account.borrow_mut_data_unchecked() };
    if data.len() < pinocchio_tkn::state::TOKEN_ACCOUNT_SIZE {
        return;
    }
    let bytes = amount.to_le_bytes();
    data[64] = bytes[0];
    data[65] = bytes[1];
    data[66] = bytes[2];
    data[67] = bytes[3];
    data[68] = bytes[4];
    data[69] = bytes[5];
    data[70] = bytes[6];
    data[71] = bytes[7];
}

#[cfg(kani)]
fn write_token_amount_bounded_kani(account: &AccountInfo, amount: u64) {
    let data = unsafe { account.borrow_mut_data_unchecked() };
    if data.len() < pinocchio_tkn::state::TOKEN_ACCOUNT_SIZE {
        return;
    }
    let bytes = (amount as u32).to_le_bytes();
    data[64] = bytes[0];
    data[65] = bytes[1];
    data[66] = bytes[2];
    data[67] = bytes[3];
    data[68] = 0;
    data[69] = 0;
    data[70] = 0;
    data[71] = 0;
}

#[cfg(kani)]
fn read_legacy_token_account_kani(account: &AccountInfo) -> Result<&[u8], ProgramError> {
    if !token_account_data_valid_kani(account) {
        return Err(ProgramError::InvalidAccountData);
    }
    Ok(unsafe { account.borrow_data_unchecked() })
}

#[cfg(kani)]
fn token_account_matches_kani(account: &AccountInfo, mint: &Pubkey, owner: &Pubkey) -> bool {
    if !token_account_data_valid_kani(account) {
        return false;
    }
    let data = unsafe { account.borrow_data_unchecked() };
    token_data_pubkey_eq_kani(data, 0, mint) && token_data_pubkey_eq_kani(data, 32, owner)
}

#[cfg(kani)]
fn token_account_mint_matches_kani(account: &AccountInfo, mint: &Pubkey) -> bool {
    if !token_account_data_valid_kani(account) {
        return false;
    }
    let data = unsafe { account.borrow_data_unchecked() };
    token_data_pubkey_eq_kani(data, 0, mint)
}

#[cfg(kani)]
fn token_account_data_valid_kani(account: &AccountInfo) -> bool {
    if !pubkey_eq_kani(account.owner(), &pinocchio_tkn::TOKEN_PROGRAM_ID) {
        return false;
    }
    let data = unsafe { account.borrow_data_unchecked() };
    data.len() >= pinocchio_tkn::state::TOKEN_ACCOUNT_SIZE && data[108] == 1
}

#[cfg(kani)]
fn token_data_pubkey_eq_kani(data: &[u8], offset: usize, expected: &Pubkey) -> bool {
    data[offset] == expected[0]
        && data[offset + 1] == expected[1]
        && data[offset + 2] == expected[2]
        && data[offset + 3] == expected[3]
        && data[offset + 4] == expected[4]
        && data[offset + 5] == expected[5]
}

#[cfg(kani)]
fn pubkey_eq_kani(left: &Pubkey, right: &Pubkey) -> bool {
    pubkey_domain_eq_kani(left, right)
}

#[cfg(kani)]
fn pubkey_domain_eq_kani(left: &Pubkey, right: &Pubkey) -> bool {
    left[0] == right[0]
        && left[1] == right[1]
        && left[2] == right[2]
        && left[3] == right[3]
        && left[4] == right[4]
        && left[5] == right[5]
}
