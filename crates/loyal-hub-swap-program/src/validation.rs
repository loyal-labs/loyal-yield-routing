use solana_program::{
    account_info::AccountInfo, entrypoint::ProgramResult, program_error::ProgramError,
    pubkey::Pubkey,
};

use crate::state::HubConfig;

pub fn require_admin(admin: &AccountInfo, config: &HubConfig) -> ProgramResult {
    require_signer(admin)?;
    require_key(admin, &config.admin)
}

pub fn require_signer(account: &AccountInfo) -> ProgramResult {
    if !account.is_signer {
        return Err(ProgramError::MissingRequiredSignature);
    }
    Ok(())
}

pub fn require_key(account: &AccountInfo, expected: &Pubkey) -> ProgramResult {
    if account.key != expected {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

pub fn require_distinct_key(left: &AccountInfo, right: &AccountInfo) -> ProgramResult {
    if left.key == right.key {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

pub fn require_distinct_keys(accounts: &[&AccountInfo]) -> ProgramResult {
    for (index, account) in accounts.iter().enumerate() {
        for other in accounts.iter().skip(index + 1) {
            require_distinct_key(account, other)?;
        }
    }
    Ok(())
}

pub fn require_distinct_pubkeys(left: &Pubkey, right: &Pubkey) -> ProgramResult {
    if left == right {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

pub fn require_fee_cap(
    amount_in: u64,
    amount_out: u64,
    input_decimals: u8,
    output_decimals: u8,
    max_fee_bps: u16,
) -> ProgramResult {
    validate_fee_bps(max_fee_bps)?;
    let input_normalized = normalize_amount(amount_in, input_decimals)?;
    let output_normalized = normalize_amount(amount_out, output_decimals)?;
    let min_output = input_normalized
        .checked_mul(10_000u128 - max_fee_bps as u128)
        .ok_or(ProgramError::InvalidArgument)?
        / 10_000u128;
    if output_normalized < min_output {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

pub fn validate_fee_bps(fee_bps: u16) -> ProgramResult {
    if fee_bps > 10_000 {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

fn normalize_amount(amount: u64, decimals: u8) -> Result<u128, ProgramError> {
    if decimals > 18 {
        return Err(ProgramError::InvalidArgument);
    }
    let scale = 10u128
        .checked_pow((18u8 - decimals) as u32)
        .ok_or(ProgramError::InvalidArgument)?;
    (amount as u128)
        .checked_mul(scale)
        .ok_or(ProgramError::InvalidArgument)
}
