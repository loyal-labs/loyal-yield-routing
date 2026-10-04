//! Initial fixed-price KLend model inventory. Official SDK creates the policy;
//! the connected Go packet later causes actual SPL/CPI asset movement.
use klend_interface::instructions::deposit::{
    deposit_reserve_liquidity_and_obligation_collateral_v2,
    DepositReserveLiquidityAndObligationCollateralV2Accounts,
};
use serde_json::{json, Value};
use solana_sdk::{
    account::Account,
    pubkey::Pubkey,
    signature::{Keypair, Signer},
};
use squads_test_harness::prelude::*;

pub fn initialize(
    c: &mut FundedSquadsTestContext,
    executor: &Keypair,
    vault_ata: Pubkey,
    amount: u64,
) -> (Vec<Pubkey>, Value) {
    let reserve = Pubkey::new_from_array([31; 32]);
    let market = Pubkey::new_from_array([32; 32]);
    let collateral_mint = Pubkey::new_from_array([33; 32]);
    let supply = Pubkey::new_from_array([34; 32]);
    let collateral_supply = Pubkey::new_from_array([35; 32]);
    let authority = loyal_actions::derive_kamino_lending_market_authority(market);
    let obligation = loyal_actions::derive_kamino_vanilla_obligation(c.vault, market);
    // Reviewed full Reserve/Obligation fixture layouts used by the Go decoder,
    // and by mock-yield-protocols-program's fixed-price v2 instruction validator.
    let mut reserve_data = vec![0u8; 8624];
    reserve_data[..8].copy_from_slice(&[43, 242, 204, 202, 26, 247, 59, 127]);
    reserve_data[8..16].copy_from_slice(&1u64.to_le_bytes());
    reserve_data[16..24].copy_from_slice(&1000u64.to_le_bytes());
    reserve_data[224..232].copy_from_slice(&1_000_000u64.to_le_bytes());
    reserve_data[2592..2600].copy_from_slice(&1_000_000u64.to_le_bytes());
    for (offset, key) in [
        (32, market),
        (128, USDC_MINT),
        (160, supply),
        (408, spl_token::id()),
        (2560, collateral_mint),
        (2600, collateral_supply),
    ] {
        reserve_data[offset..offset + 32].copy_from_slice(key.as_ref());
    }
    let mut obligation_data = vec![0u8; 3344];
    obligation_data[..8].copy_from_slice(&[168, 206, 141, 106, 88, 76, 172, 167]);
    obligation_data[16..24].copy_from_slice(&1000u64.to_le_bytes());
    obligation_data[32..64].copy_from_slice(market.as_ref());
    obligation_data[64..96].copy_from_slice(c.vault.as_ref());
    for (key, data, owner) in [
        (reserve, reserve_data, KAMINO_LEND_PROGRAM_ID),
        (obligation, obligation_data, KAMINO_LEND_PROGRAM_ID),
        (market, vec![0; 8], KAMINO_LEND_PROGRAM_ID),
        (authority, vec![], solana_sdk::system_program::ID),
    ] {
        c.svm
            .set_account(
                key,
                Account {
                    lamports: 100_000_000,
                    data,
                    owner,
                    executable: false,
                    rent_epoch: 0,
                },
            )
            .unwrap();
    }
    seed_spl_mint(
        &mut c.svm,
        collateral_mint,
        Some(authority),
        USDC_DECIMALS,
        1_000_000,
    );
    seed_spl_token_account(&mut c.svm, supply, USDC_MINT, authority, 1_000_000);
    seed_spl_token_account(
        &mut c.svm,
        collateral_supply,
        collateral_mint,
        authority,
        1_000_000,
    );
    // Exact current upstream v2 builder, matching loyal-klend-proxy deposit_ix.
    let deposit = deposit_reserve_liquidity_and_obligation_collateral_v2(
        DepositReserveLiquidityAndObligationCollateralV2Accounts {
            owner: c.vault,
            obligation,
            lending_market: market,
            lending_market_authority: authority,
            reserve,
            reserve_liquidity_mint: USDC_MINT,
            reserve_liquidity_supply: supply,
            reserve_collateral_mint: collateral_mint,
            reserve_destination_deposit_collateral: collateral_supply,
            user_source_liquidity: vault_ata,
            placeholder_user_destination_collateral: None,
            liquidity_token_program: spl_token::id(),
            obligation_farm_user_state: None,
            reserve_farm_state: None,
        },
        amount,
    );
    let seed = 2;
    let (policy, _) = derive_squads_policy(&c.pool.settings, seed);
    let creator = loyal_actions::create_exact_program_interaction_policy_instruction(
        c.pool.settings,
        c.wallet_pubkey(),
        executor.pubkey(),
        seed,
        1,
        &[deposit],
        &[vec![0, 1, 2, 4, 5, 6, 7, 8, 9, 11, 12]],
    )
    .unwrap();
    try_send_instructions(&mut c.svm, &[creator], &c.wallet, &[])
        .expect("real Squads creates exact official mock-topup policy");
    (
        vec![
            reserve,
            market,
            authority,
            obligation,
            collateral_mint,
            supply,
            collateral_supply,
            policy,
        ],
        json!({"scope":"fixed-price KLend mock SBF with real SPL token transfer/collateral mint and obligation update; no interest/oracle/mainnet correctness claim","reserve":reserve.to_string(),"market":market.to_string(),"obligation":obligation.to_string(),"routePolicy":policy.to_string(),"routePolicySeed":seed,"liquiditySupply":supply.to_string(),"collateralMint":collateral_mint.to_string(),"collateralSupply":collateral_supply.to_string()}),
    )
}
