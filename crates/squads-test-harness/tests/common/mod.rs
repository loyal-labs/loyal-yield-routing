#![allow(dead_code)]

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use litesvm::LiteSVM;
use serde::Deserialize;
use solana_sdk::{account::Account, instruction::AccountMeta, pubkey::Pubkey};
use squads_test_harness::{
    mock_jupiter_token_accounts, JUPITER_V6_PROGRAM_ID, LAMPORTS_PER_SOL, PYUSD_MINT, USDC_MINT,
};
use std::{collections::HashMap, str::FromStr};

pub const JUPITER_USDC_PYUSD_FIXTURE: &str =
    include_str!("../../fixtures/jupiter/usdc-pyusd-build.json");

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct JupiterBuildFixture {
    pub input_mint: String,
    pub output_mint: String,
    pub in_amount: String,
    pub out_amount: String,
    pub other_amount_threshold: String,
    pub swap_mode: String,
    pub slippage_bps: u16,
    pub route_plan: Vec<JupiterRoutePlan>,
    pub compute_budget_instructions: Vec<JupiterInstruction>,
    pub setup_instructions: Vec<JupiterInstruction>,
    pub swap_instruction: JupiterInstruction,
    pub cleanup_instruction: Option<JupiterInstruction>,
    pub other_instructions: Vec<JupiterInstruction>,
    pub tip_instruction: Option<JupiterInstruction>,
    pub addresses_by_lookup_table_address: HashMap<String, Vec<String>>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct JupiterRoutePlan {
    pub percent: u16,
    pub bps: u16,
    pub swap_info: JupiterSwapInfo,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct JupiterSwapInfo {
    pub amm_key: String,
    pub label: String,
    pub input_mint: String,
    pub output_mint: String,
    pub in_amount: String,
    pub out_amount: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct JupiterInstruction {
    pub program_id: String,
    pub accounts: Vec<JupiterAccountMeta>,
    pub data: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct JupiterAccountMeta {
    pub pubkey: String,
    pub is_signer: bool,
    pub is_writable: bool,
}

pub fn load_jupiter_usdc_pyusd_fixture() -> JupiterBuildFixture {
    let fixture: JupiterBuildFixture =
        serde_json::from_str(JUPITER_USDC_PYUSD_FIXTURE).expect("parse Jupiter fixture");
    assert_jupiter_usdc_pyusd_fixture_contract(&fixture);
    fixture
}

pub fn assert_jupiter_usdc_pyusd_fixture_contract(fixture: &JupiterBuildFixture) {
    assert_eq!(fixture.input_mint, USDC_MINT.to_string());
    assert_eq!(fixture.output_mint, PYUSD_MINT.to_string());
    assert_eq!(fixture.swap_mode, "ExactIn");
    assert_eq!(fixture.slippage_bps, 50);
    assert_eq!(
        fixture.swap_instruction.program_id,
        JUPITER_V6_PROGRAM_ID.to_string()
    );
    assert!(!fixture.compute_budget_instructions.is_empty());
    assert!(!fixture.setup_instructions.is_empty());
    assert!(fixture.cleanup_instruction.is_none());
    assert!(fixture.other_instructions.is_empty());
    assert!(fixture.tip_instruction.is_none());
    assert!(!fixture.addresses_by_lookup_table_address.is_empty());
    assert!(!fixture.other_amount_threshold.is_empty());
    for (lookup_table, addresses) in &fixture.addresses_by_lookup_table_address {
        pubkey_from_fixture(lookup_table);
        assert!(!addresses.is_empty());
        for address in addresses {
            pubkey_from_fixture(address);
        }
    }

    let route = fixture.route_plan.first().expect("Jupiter route plan");
    assert_eq!(route.percent, 100);
    assert_eq!(route.bps, 10_000);
    assert_eq!(route.swap_info.input_mint, USDC_MINT.to_string());
    assert_eq!(route.swap_info.output_mint, PYUSD_MINT.to_string());
    assert_eq!(route.swap_info.in_amount, fixture.in_amount);
    assert_eq!(route.swap_info.out_amount, fixture.out_amount);
    assert!(!route.swap_info.amm_key.is_empty());
    assert!(!route.swap_info.label.is_empty());

    let accounts = &fixture.swap_instruction.accounts;
    assert!(accounts.len() >= 6);
    assert!(accounts[0].is_signer);
    assert!(!accounts[0].is_writable);
    assert!(accounts[1].is_writable);
    assert!(accounts[2].is_writable);
    assert_eq!(accounts[3].pubkey, USDC_MINT.to_string());
    assert_eq!(accounts[4].pubkey, PYUSD_MINT.to_string());
    assert_eq!(accounts[5].pubkey, spl_token::id().to_string());
}

pub fn decode_jupiter_swap_data(fixture: &JupiterBuildFixture) -> Vec<u8> {
    BASE64_STANDARD
        .decode(&fixture.swap_instruction.data)
        .expect("decode Jupiter swap instruction data")
}

pub fn parse_fixture_amount(amount: &str) -> u64 {
    amount.parse().expect("parse Jupiter raw token amount")
}

pub fn jupiter_fixture_transaction(
    fixture: &JupiterBuildFixture,
    vault: Pubkey,
    vault_usdc: Pubkey,
    vault_pyusd: Pubkey,
) -> (Vec<AccountMeta>, Vec<usize>, usize) {
    let jupiter_accounts = mock_jupiter_token_accounts();
    let mut transaction_accounts = vec![
        AccountMeta::new(vault, false),
        AccountMeta::new(vault_usdc, false),
        AccountMeta::new(vault_pyusd, false),
        AccountMeta::new_readonly(USDC_MINT, false),
        AccountMeta::new_readonly(PYUSD_MINT, false),
        AccountMeta::new_readonly(spl_token::id(), false),
        AccountMeta::new(jupiter_accounts.usdc_reserve, false),
        AccountMeta::new(jupiter_accounts.pyusd_reserve, false),
        AccountMeta::new_readonly(jupiter_accounts.authority, false),
    ];
    let mut instruction_accounts = vec![0, 1, 2, 3, 4, 5, 6, 7, 8];

    for fixture_meta in fixture.swap_instruction.accounts.iter().skip(6) {
        let pubkey = pubkey_from_fixture(&fixture_meta.pubkey);
        let index = push_or_update_meta(
            &mut transaction_accounts,
            pubkey,
            fixture_meta.is_writable,
            false,
        );
        instruction_accounts.push(index);
    }

    let program_id_index = push_or_update_meta(
        &mut transaction_accounts,
        JUPITER_V6_PROGRAM_ID,
        false,
        false,
    );

    (transaction_accounts, instruction_accounts, program_id_index)
}

pub fn seed_jupiter_fixture_accounts(
    svm: &mut LiteSVM,
    fixture: &JupiterBuildFixture,
    accounts: &[AccountMeta],
) {
    for account in accounts {
        seed_fixture_account_if_missing(svm, account.pubkey);
    }
    for (lookup_table, addresses) in &fixture.addresses_by_lookup_table_address {
        seed_fixture_account_if_missing(svm, pubkey_from_fixture(lookup_table));
        for address in addresses {
            seed_fixture_account_if_missing(svm, pubkey_from_fixture(address));
        }
    }
}

pub fn jupiter_build_url() -> String {
    format!(
        "https://api.jup.ag/swap/v2/build?inputMint={}&outputMint={}&amount=1000000&taker=11111111111111111111111111111111&maxAccounts=16&slippageBps=50",
        USDC_MINT, PYUSD_MINT
    )
}

pub fn shell_single_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\\''"))
}

fn pubkey_from_fixture(value: &str) -> Pubkey {
    Pubkey::from_str(value).expect("parse Jupiter account pubkey")
}

fn seed_fixture_account_if_missing(svm: &mut LiteSVM, pubkey: Pubkey) {
    if pubkey == solana_sdk::system_program::ID || svm.get_account(&pubkey).is_some() {
        return;
    }

    svm.set_account(
        pubkey,
        Account {
            lamports: LAMPORTS_PER_SOL,
            data: vec![],
            owner: solana_sdk::system_program::ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed Jupiter fixture account");
}

fn push_or_update_meta(
    metas: &mut Vec<AccountMeta>,
    pubkey: Pubkey,
    is_writable: bool,
    is_signer: bool,
) -> usize {
    if let Some(index) = metas.iter().position(|meta| meta.pubkey == pubkey) {
        metas[index].is_writable |= is_writable;
        metas[index].is_signer |= is_signer;
        return index;
    }

    let index = metas.len();
    metas.push(AccountMeta {
        pubkey,
        is_signer,
        is_writable,
    });
    index
}
