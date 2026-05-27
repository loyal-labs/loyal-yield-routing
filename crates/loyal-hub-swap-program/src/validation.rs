use pinocchio::{
    account_info::AccountInfo, program_error::ProgramError, pubkey::Pubkey, ProgramResult,
};

use crate::state::HubConfig;

pub fn require_admin(admin: &AccountInfo, config: &HubConfig) -> ProgramResult {
    require_signer(admin)?;
    require_key(admin, &config.admin)
}

pub fn require_inventory_rebalancer(rebalancer: &AccountInfo, config: &HubConfig) -> ProgramResult {
    require_signer(rebalancer)?;
    require_key(rebalancer, &config.inventory_rebalancer)
}

pub fn require_signer(account: &AccountInfo) -> ProgramResult {
    if !account.is_signer() {
        return Err(ProgramError::MissingRequiredSignature);
    }
    Ok(())
}

pub fn require_readonly(account: &AccountInfo) -> ProgramResult {
    if account.is_writable() {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

pub fn require_key(account: &AccountInfo, expected: &Pubkey) -> ProgramResult {
    if account.key() != expected {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

pub fn require_distinct_key(left: &AccountInfo, right: &AccountInfo) -> ProgramResult {
    if left.key() == right.key() {
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
    let input_normalized = normalize_amount_for_fee(amount_in, input_decimals)?;
    let output_normalized = normalize_amount_for_fee(amount_out, output_decimals)?;
    if !fee_cap_holds(input_normalized, output_normalized, max_fee_bps)? {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

pub fn validate_fee_bps(fee_bps: u16) -> ProgramResult {
    if fee_bps > loyal_hub_abi::MAX_FEE_BPS as u16 {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

fn fee_cap_holds(
    input_normalized: u128,
    output_normalized: u128,
    max_fee_bps: u16,
) -> Result<bool, ProgramError> {
    Ok(output_normalized >= minimum_output_after_fee(input_normalized, max_fee_bps)?)
}

fn minimum_output_after_fee(
    input_normalized: u128,
    max_fee_bps: u16,
) -> Result<u128, ProgramError> {
    validate_fee_bps(max_fee_bps)?;
    let max_fee_bps = max_fee_bps as u128;
    let fee_denominator = loyal_hub_abi::MAX_FEE_BPS as u128;
    input_normalized
        .checked_mul(fee_denominator - max_fee_bps)
        .ok_or(ProgramError::InvalidArgument)
        .map(|value| value / fee_denominator)
}

fn normalize_amount_for_fee(amount: u64, decimals: u8) -> Result<u128, ProgramError> {
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalize_amount_rejects_decimals_above_eighteen() {
        assert_eq!(
            normalize_amount_for_fee(1, 19),
            Err(ProgramError::InvalidArgument)
        );
    }

    #[test]
    fn validate_fee_bps_rejects_values_above_ten_thousand() {
        assert_eq!(validate_fee_bps(10_001), Err(ProgramError::InvalidArgument));
    }

    #[test]
    fn fee_cap_rejects_normalized_multiplication_overflow() {
        let overflowing_input = (u128::MAX / loyal_hub_abi::MAX_FEE_BPS as u128) + 1;

        assert_eq!(
            minimum_output_after_fee(overflowing_input, 0),
            Err(ProgramError::InvalidArgument)
        );
    }

    #[test]
    fn fee_cap_accepts_exact_threshold_output() {
        let input_normalized = 1_000_000u128;
        let min_output = minimum_output_after_fee(input_normalized, 50).unwrap();

        assert!(fee_cap_holds(input_normalized, min_output, 50).unwrap());
    }

    #[test]
    fn fee_cap_rejects_output_below_threshold() {
        let input_normalized = 1_000_000u128;
        let min_output = minimum_output_after_fee(input_normalized, 50).unwrap();

        assert!(!fee_cap_holds(input_normalized, min_output - 1, 50).unwrap());
    }

    #[test]
    fn higher_output_cannot_break_passing_fee_cap() {
        let input_normalized = 1_000_000u128;
        let min_output = minimum_output_after_fee(input_normalized, 50).unwrap();

        for extra_output in 0..1_000u128 {
            assert!(fee_cap_holds(input_normalized, min_output + extra_output, 50).unwrap());
        }
    }

    #[test]
    fn require_fee_cap_uses_normalized_amounts_from_mint_decimals() {
        assert!(require_fee_cap(1_000_000, 995_000, 6, 6, 50).is_ok());
        assert_eq!(
            require_fee_cap(1_000_000, 994_999, 6, 6, 50),
            Err(ProgramError::InvalidArgument)
        );
    }
}
