#![cfg(kani)]

#[kani::proof_for_contract(crate::validation::fee_cap_holds_no_overflow_kani)]
fn verify_fee_cap_holds_no_overflow_contract() {
    let input_normalized: u128 = kani::any();
    let output_normalized: u128 = kani::any();
    let retained_value_bps: u128 = kani::any();
    kani::assume(retained_value_bps <= loyal_hub_abi::MAX_FEE_BPS as u128);
    kani::assume(input_normalized <= crate::validation::normalized_fee_mul_max_kani());
    kani::assume(output_normalized < crate::validation::normalized_fee_mul_max_kani());
    kani::assume(
        output_normalized
            >= (input_normalized * retained_value_bps) / loyal_hub_abi::MAX_FEE_BPS as u128,
    );
    crate::validation::fee_cap_holds_no_overflow_kani(
        input_normalized,
        output_normalized,
        retained_value_bps,
    );
}

#[kani::proof_for_contract(crate::validation::require_fee_cap_accepts_kani)]
#[kani::stub_verified(crate::validation::fee_cap_holds_no_overflow_kani)]
fn verify_require_fee_cap_accepts_contract() {
    let amount_in: u64 = kani::any();
    let amount_out: u64 = kani::any();
    let input_decimals: u8 = kani::any();
    let output_decimals: u8 = kani::any();
    let max_fee_bps: u16 = kani::any();
    kani::assume(input_decimals == 6);
    kani::assume(output_decimals == 6);
    kani::assume(amount_in <= 1_000_000);
    kani::assume(amount_out <= 1_000_000);
    kani::assume(max_fee_bps as usize <= loyal_hub_abi::MAX_FEE_BPS);
    kani::assume(
        amount_out as u128
            >= ((amount_in as u128) * (loyal_hub_abi::MAX_FEE_BPS as u128 - max_fee_bps as u128))
                / loyal_hub_abi::MAX_FEE_BPS as u128,
    );
    crate::validation::require_fee_cap_accepts_kani(
        amount_in,
        amount_out,
        input_decimals,
        output_decimals,
        max_fee_bps,
    );
}
