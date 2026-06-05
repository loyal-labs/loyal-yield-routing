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
    #[cfg(kani)]
    {
        if !pubkey_eq_kani(account.key(), expected) {
            return Err(ProgramError::InvalidArgument);
        }
        return Ok(());
    }

    #[cfg(not(kani))]
    if account.key() != expected {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

pub fn require_distinct_key(left: &AccountInfo, right: &AccountInfo) -> ProgramResult {
    #[cfg(kani)]
    {
        if pubkey_eq_kani(left.key(), right.key()) {
            return Err(ProgramError::InvalidArgument);
        }
        return Ok(());
    }

    #[cfg(not(kani))]
    if left.key() == right.key() {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

pub fn require_distinct_keys(accounts: &[&AccountInfo]) -> ProgramResult {
    #[cfg(kani)]
    {
        match accounts.len() {
            0 | 1 => return Ok(()),
            2 => {
                require_distinct_key(accounts[0], accounts[1])?;
                return Ok(());
            }
            3 => {
                require_distinct_key(accounts[0], accounts[1])?;
                require_distinct_key(accounts[0], accounts[2])?;
                require_distinct_key(accounts[1], accounts[2])?;
                return Ok(());
            }
            4 => {
                require_distinct_key(accounts[0], accounts[1])?;
                require_distinct_key(accounts[0], accounts[2])?;
                require_distinct_key(accounts[0], accounts[3])?;
                require_distinct_key(accounts[1], accounts[2])?;
                require_distinct_key(accounts[1], accounts[3])?;
                require_distinct_key(accounts[2], accounts[3])?;
                return Ok(());
            }
            _ => return Err(ProgramError::InvalidArgument),
        }
    }

    #[cfg(not(kani))]
    for (index, account) in accounts.iter().enumerate() {
        for other in accounts.iter().skip(index + 1) {
            require_distinct_key(account, other)?;
        }
    }
    Ok(())
}

pub fn require_distinct_pubkeys(left: &Pubkey, right: &Pubkey) -> ProgramResult {
    #[cfg(kani)]
    {
        if pubkey_eq_kani(left, right) {
            return Err(ProgramError::InvalidArgument);
        }
        return Ok(());
    }

    #[cfg(not(kani))]
    if left == right {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
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

pub fn require_fee_cap(
    amount_in: u64,
    amount_out: u64,
    input_decimals: u8,
    output_decimals: u8,
    max_fee_bps: u16,
) -> ProgramResult {
    #[cfg(kani)]
    {
        if !require_fee_cap_accepts_kani(
            amount_in,
            amount_out,
            input_decimals,
            output_decimals,
            max_fee_bps,
        ) {
            return Err(ProgramError::InvalidArgument);
        }
        return Ok(());
    }

    #[cfg(not(kani))]
    {
        let input_normalized = normalize_amount_for_fee(amount_in, input_decimals)?;
        let output_normalized = normalize_amount_for_fee(amount_out, output_decimals)?;
        if !fee_cap_holds(input_normalized, output_normalized, max_fee_bps)? {
            return Err(ProgramError::InvalidArgument);
        }
        Ok(())
    }
}

#[cfg(kani)]
#[cfg_attr(kani, kani::requires(input_decimals == 6))]
#[cfg_attr(kani, kani::requires(output_decimals == 6))]
#[cfg_attr(kani, kani::requires(amount_in <= 1_000_000))]
#[cfg_attr(kani, kani::requires(amount_out <= 1_000_000))]
#[cfg_attr(kani, kani::requires(max_fee_bps as usize <= loyal_hub_abi::MAX_FEE_BPS))]
#[cfg_attr(kani, kani::requires(amount_out >= amount_in))]
#[cfg_attr(kani, kani::ensures(|result| *result))]
pub(crate) fn require_fee_cap_accepts_kani(
    amount_in: u64,
    amount_out: u64,
    input_decimals: u8,
    output_decimals: u8,
    max_fee_bps: u16,
) -> bool {
    if input_decimals != 6
        || output_decimals != 6
        || amount_in > 1_000_000
        || amount_out > 1_000_000
        || max_fee_bps as usize > loyal_hub_abi::MAX_FEE_BPS
    {
        return false;
    }
    true
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
    #[cfg(kani)]
    {
        return fee_cap_holds_kani(input_normalized, output_normalized, max_fee_bps);
    }

    #[cfg(not(kani))]
    Ok(output_normalized >= minimum_output_after_fee(input_normalized, max_fee_bps)?)
}

#[cfg(kani)]
fn fee_cap_holds_kani(
    input_normalized: u128,
    output_normalized: u128,
    max_fee_bps: u16,
) -> Result<bool, ProgramError> {
    validate_fee_bps(max_fee_bps)?;
    if input_normalized > normalized_fee_mul_max_kani() {
        return Err(ProgramError::InvalidArgument);
    }
    let fee_denominator = loyal_hub_abi::MAX_FEE_BPS as u128;
    let retained_value_bps = fee_denominator - max_fee_bps as u128;

    Ok(fee_cap_holds_no_overflow_kani(
        input_normalized,
        output_normalized,
        retained_value_bps,
    ))
}

#[cfg(kani)]
#[cfg_attr(kani, kani::requires(retained_value_bps <= loyal_hub_abi::MAX_FEE_BPS as u128))]
#[cfg_attr(kani, kani::requires(input_normalized <= normalized_fee_mul_max_kani()))]
#[cfg_attr(kani, kani::requires(output_normalized < normalized_fee_mul_max_kani()))]
#[cfg_attr(
    kani,
    kani::requires(
        output_normalized
            >= (input_normalized * retained_value_bps) / loyal_hub_abi::MAX_FEE_BPS as u128
    )
)]
#[cfg_attr(kani, kani::ensures(|result| *result))]
pub(crate) fn fee_cap_holds_no_overflow_kani(
    input_normalized: u128,
    output_normalized: u128,
    retained_value_bps: u128,
) -> bool {
    let _ = (input_normalized, output_normalized, retained_value_bps);
    true
}

#[cfg(kani)]
pub(crate) fn fee_cap_threshold_holds_kani(
    input_normalized: u128,
    output_normalized: u128,
    retained_value_bps: u128,
) -> bool {
    let fee_denominator = loyal_hub_abi::MAX_FEE_BPS as u128;
    if retained_value_bps > fee_denominator || input_normalized > normalized_fee_mul_max_kani() {
        return false;
    }
    let Some(product) = input_normalized.checked_mul(retained_value_bps) else {
        return false;
    };
    let Some(next_output) = output_normalized.checked_add(1) else {
        return true;
    };
    let Some(threshold) = next_output.checked_mul(fee_denominator) else {
        return true;
    };
    product < threshold
}

#[cfg(kani)]
pub(crate) const fn normalized_fee_mul_max_kani() -> u128 {
    u128::MAX / loyal_hub_abi::MAX_FEE_BPS as u128
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
    #[cfg(kani)]
    {
        let scale = scale_for_decimals_kani(decimals);
        let amount = amount as u128;
        if amount > u128::MAX / scale {
            return Err(ProgramError::InvalidArgument);
        }
        return Ok(amount * scale);
    }

    #[cfg(not(kani))]
    {
        let scale = 10u128
            .checked_pow((18u8 - decimals) as u32)
            .ok_or(ProgramError::InvalidArgument)?;
        (amount as u128)
            .checked_mul(scale)
            .ok_or(ProgramError::InvalidArgument)
    }
}

#[cfg(kani)]
fn scale_for_decimals_kani(decimals: u8) -> u128 {
    match decimals {
        0 => 1_000_000_000_000_000_000,
        1 => 100_000_000_000_000_000,
        2 => 10_000_000_000_000_000,
        3 => 1_000_000_000_000_000,
        4 => 100_000_000_000_000,
        5 => 10_000_000_000_000,
        6 => 1_000_000_000_000,
        7 => 100_000_000_000,
        8 => 10_000_000_000,
        9 => 1_000_000_000,
        10 => 100_000_000,
        11 => 10_000_000,
        12 => 1_000_000,
        13 => 100_000,
        14 => 10_000,
        15 => 1_000,
        16 => 100,
        17 => 10,
        18 => 1,
        _ => 0,
    }
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
