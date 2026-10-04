//! Initial bank only. No financial poststate or successful receipt is injected.
//! Current Go executes its own signed policy packet in the existing LiteSVM driver.
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use borsh::BorshSerialize;
use loyal_actions::*;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{
    account::Account,
    pubkey::Pubkey,
    signature::{Keypair, SeedDerivable, Signer},
};
use squads_test_harness::prelude::*;
use std::{collections::BTreeMap, str::FromStr};

fn key(v: &Value, field: &str) -> Pubkey {
    Pubkey::from_str(v[field].as_str().expect(field)).unwrap()
}

// Narrow official Jupiter CPI IDL v1 mapping, reviewed 2026-10-03:
// https://raw.githubusercontent.com/jup-ag/jupiter-cpi/main/idl.json
// sharedAccountsRoute.id + Vec<RoutePlanStep>; first enum variants below.
// This independent Rust serialization proves the tested wire shape, not real
// Jupiter/TokenSwap execution (the local bank uses an explicit fixed-price model).
#[allow(dead_code)]
#[derive(BorshSerialize)]
enum FixtureSwap {
    Saber,
    SaberAddDecimalsDeposit,
    SaberAddDecimalsWithdraw,
    TokenSwap,
}
#[derive(BorshSerialize)]
struct FixtureStep {
    swap: FixtureSwap,
    percent: u8,
    input_index: u8,
    output_index: u8,
}
#[derive(BorshSerialize)]
struct FixtureSharedPrefix {
    id: u8,
    route_plan: Vec<FixtureStep>,
}

fn official_shared_route_prefix() -> Vec<u8> {
    let digest = Sha256::digest(b"global:shared_accounts_route");
    let mut data = digest[..8].to_vec();
    assert_eq!(data, [193, 32, 155, 51, 65, 214, 156, 129]);
    FixtureSharedPrefix {
        id: 0,
        route_plan: vec![FixtureStep {
            swap: FixtureSwap::TokenSwap,
            percent: 100,
            input_index: 0,
            output_index: 1,
        }],
    }
    .serialize(&mut data)
    .unwrap();
    assert_eq!(data.len(), 17);
    data
}

#[test]
fn export_current_go_multiply_initial_bank() {
    let Some(input_path) = std::env::var_os("WORKERS_V2_MULTIPLY_BANK_INPUT") else {
        return;
    };
    let output = std::env::var_os("WORKERS_V2_MULTIPLY_BANK_OUTPUT").expect("output path");
    let input: Value = serde_json::from_slice(&std::fs::read(input_path).unwrap()).unwrap();
    let seed = input["smartAccountSeed"].as_u64().unwrap();
    assert!(seed > 0);
    let mut svm = new_litesvm();
    let loaded_program_path = add_squads_program_from_env_or_sibling_checkout(&mut svm)
        .unwrap()
        .expect("required real Squads SBF");
    let wallet = Keypair::new();
    svm.airdrop(&wallet.pubkey(), 2_000_000_000).unwrap();
    // Initial source config uses its actual next-index contract. The generic
    // funded helper pins counter0 and therefore cannot create unique settings.
    seed_squads_program_config(
        &mut svm,
        wallet.pubkey(),
        squads_test_treasury(),
        u128::from(seed - 1),
    );
    send_instructions(
        &mut svm,
        &[create_squads_smart_account_instruction(
            wallet.pubkey(),
            wallet.pubkey(),
            u128::from(seed),
        )],
        &wallet,
    );
    let pool = derive_squads_pool(u128::from(seed));
    let vault = squads_test_harness::derive_squads_vault(&pool.settings, 0).0;
    send_instructions(
        &mut svm,
        &[solana_sdk::system_instruction::transfer(
            &wallet.pubkey(),
            &vault,
            1_000_000_000,
        )],
        &wallet,
    );
    let mut c = FundedSquadsTestContext {
        svm,
        wallet,
        pool,
        vault_index: 0,
        vault,
        wallet_airdrop_lamports: 2_000_000_000,
        vault_funding_lamports: 1_000_000_000,
        loaded_program_path,
    };
    assert_eq!(c.pool.settings, key(&input, "settings"));
    let mut delegate_seed = [0u8; 32];
    delegate_seed[..26].copy_from_slice(b"multiply-test-delegate-key");
    let delegate = Keypair::from_seed(&delegate_seed).unwrap();
    c.svm.airdrop(&delegate.pubkey(), 100_000_000).unwrap();
    let catalog = input["catalog"].as_array().unwrap();
    assert_eq!(catalog.len(), 7);
    let expected = [
        (
            EARN_MAX_ONRE_MARKET,
            EARN_MAX_ONYC_COLLATERAL_RESERVE,
            EARN_MAX_ONYC_MINT,
            EARN_MAX_ONYC_USDC_DEBT_RESERVE,
            EARN_MAX_USDC_MINT,
            spl_token::id(),
        ),
        (
            EARN_MAX_ONRE_MARKET,
            EARN_MAX_ONYC_COLLATERAL_RESERVE,
            EARN_MAX_ONYC_MINT,
            EARN_MAX_ONYC_USDS_DEBT_RESERVE,
            EARN_MAX_USDS_MINT,
            spl_token::id(),
        ),
        (
            EARN_MAX_FIGURE_MARKET,
            EARN_MAX_PRIME_COLLATERAL_RESERVE,
            EARN_MAX_PRIME_MINT,
            EARN_MAX_PRIME_USDC_DEBT_RESERVE,
            EARN_MAX_USDC_MINT,
            spl_token::id(),
        ),
        (
            EARN_MAX_FIGURE_MARKET,
            EARN_MAX_PRIME_COLLATERAL_RESERVE,
            EARN_MAX_PRIME_MINT,
            EARN_MAX_PRIME_PYUSD_DEBT_RESERVE,
            EARN_MAX_PYUSD_MINT,
            spl_token_2022::id(),
        ),
        (
            EARN_MAX_FIGURE_MARKET,
            EARN_MAX_PRIME_COLLATERAL_RESERVE,
            EARN_MAX_PRIME_MINT,
            EARN_MAX_PRIME_USDS_DEBT_RESERVE,
            EARN_MAX_USDS_MINT,
            spl_token::id(),
        ),
        (
            EARN_MAX_MAPLE_MARKET,
            EARN_MAX_SYRUP_COLLATERAL_RESERVE,
            EARN_MAX_SYRUP_USDC_MINT,
            EARN_MAX_SYRUP_USDC_DEBT_RESERVE,
            EARN_MAX_USDC_MINT,
            spl_token::id(),
        ),
        (
            EARN_MAX_MAPLE_MARKET,
            EARN_MAX_SYRUP_COLLATERAL_RESERVE,
            EARN_MAX_SYRUP_USDC_MINT,
            EARN_MAX_SYRUP_PYUSD_DEBT_RESERVE,
            EARN_MAX_PYUSD_MINT,
            spl_token_2022::id(),
        ),
    ];
    let lanes = catalog
        .iter()
        .zip(expected)
        .map(
            |(v, (market, reserve, collateral_mint, debt_reserve, debt_mint, token))| {
                let market = Pubkey::from_str(market).unwrap();
                let collateral_mint = Pubkey::from_str(collateral_mint).unwrap();
                let debt_mint = Pubkey::from_str(debt_mint).unwrap();
                assert_eq!(key(v, "Market"), market);
                assert_eq!(
                    key(v, "CollateralReserve"),
                    Pubkey::from_str(reserve).unwrap()
                );
                assert_eq!(
                    key(v, "DebtReserve"),
                    Pubkey::from_str(debt_reserve).unwrap()
                );
                assert_eq!(key(v, "CollateralMint"), collateral_mint);
                assert_eq!(key(v, "DebtMint"), debt_mint);
                assert_eq!(key(v, "DebtTokenProgram"), token);
                let obligation = loyal_actions::derive_kamino_obligation(
                    c.vault,
                    market,
                    1,
                    0,
                    collateral_mint,
                    debt_mint,
                );
                let collateral_custody = loyal_actions::derive_associated_token_account(
                    c.vault,
                    collateral_mint,
                    spl_token::id(),
                );
                let debt_custody =
                    loyal_actions::derive_associated_token_account(c.vault, debt_mint, token);
                assert_eq!(key(v, "Obligation"), obligation);
                assert_eq!(key(v, "CollateralCustody"), collateral_custody);
                assert_eq!(key(v, "DebtCustody"), debt_custody);
                EarnMaxPolicyLane {
                    market,
                    obligation,
                    collateral_reserve: key(v, "CollateralReserve"),
                    collateral_custody,
                    debt_reserve: key(v, "DebtReserve"),
                    debt_custody,
                    debt_token_program: token,
                }
            },
        )
        .collect::<Vec<_>>();
    let boundary = EarnMaxPolicyBoundary {
        vault: c.vault, klend_program: loyal_actions::KAMINO_LEND_PROGRAM_ID, jupiter_program: loyal_actions::JUPITER_V6_PROGRAM_ID,
        usdc_custody: lanes[0].debt_custody, pyusd_custody: lanes[3].debt_custody, usds_custody: lanes[1].debt_custody,
        onyc_custody: lanes[0].collateral_custody, prime_custody: lanes[2].collateral_custody, syrup_usdc_custody: lanes[5].collateral_custody,
        deposit_discriminator: klend_interface::discriminators::DEPOSIT_RESERVE_LIQUIDITY_AND_OBLIGATION_COLLATERAL_V2,
        withdraw_discriminator: klend_interface::discriminators::WITHDRAW_OBLIGATION_COLLATERAL_AND_REDEEM_RESERVE_COLLATERAL_V2,
        borrow_discriminator: klend_interface::discriminators::BORROW_OBLIGATION_LIQUIDITY_V2,
        repay_discriminator: klend_interface::discriminators::REPAY_OBLIGATION_LIQUIDITY_V2, lanes,
    };
    let wallet = c.wallet.pubkey();
    let mut owned = vec![c.pool.settings, c.vault, wallet, delegate.pubkey()];
    for (i, family) in [
        EarnMaxPolicyFamily::Collateral,
        EarnMaxPolicyFamily::Debt,
        EarnMaxPolicyFamily::Swap,
    ]
    .into_iter()
    .enumerate()
    {
        let policy_seed = i as u64 + 1;
        let creator = create_semantic_program_interaction_policy_instruction(
            c.pool.settings,
            wallet,
            delegate.pubkey(),
            policy_seed,
            0,
            earn_max_policy_constraints(&boundary, family).unwrap(),
        )
        .unwrap();
        try_send_instructions_with_heap_frame(&mut c.svm, &[creator], &c.wallet, &[])
            .expect("real Squads creates source canonical policy");
        owned.push(loyal_actions::derive_action_account(&c.pool.settings, policy_seed).0);
    }
    let mut accounts = BTreeMap::new();
    for (address, v) in input["accounts"].as_object().unwrap() {
        let address = Pubkey::from_str(address).unwrap();
        assert!(
            !owned.contains(&address),
            "cannot replace real Squads authority/policy accounts"
        );
        assert_eq!(v["Executable"].as_bool(), Some(false));
        let account = Account {
            lamports: v["Lamports"].as_u64().unwrap(),
            owner: key(v, "Owner"),
            data: BASE64.decode(v["Data"].as_str().unwrap()).unwrap(),
            executable: false,
            rent_epoch: 0,
        };
        c.svm.set_account(address, account.clone()).unwrap();
        accounts.insert(address.to_string(), json!({"Address":address.to_string(),"Owner":account.owner.to_string(),"Lamports":account.lamports,"Data":BASE64.encode(&account.data),"Executable":false}));
    }
    for address in owned {
        let a = c
            .svm
            .get_account(&address)
            .expect("actual authority/policy account");
        accounts.insert(address.to_string(), json!({"Address":address.to_string(),"Owner":a.owner.to_string(),"Lamports":a.lamports,"Data":BASE64.encode(&a.data),"Executable":false}));
    }
    assert_eq!(
        8 + std::mem::offset_of!(klend_interface::state::Reserve, config)
            + std::mem::offset_of!(
                klend_interface::state::ReserveConfig,
                liquidation_threshold_pct
            ),
        4873
    );
    std::fs::write(output, serde_json::to_vec(&json!({"settings":c.pool.settings.to_string(),"vault":c.vault.to_string(),"delegate":delegate.pubkey().to_string(),"accounts":accounts,"swapPrefix":BASE64.encode(official_shared_route_prefix()),"programSha256":format!("{:x}",Sha256::digest(std::fs::read(&c.loaded_program_path).unwrap()))})).unwrap()).unwrap();
}
