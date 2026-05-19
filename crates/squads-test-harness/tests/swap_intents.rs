use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use litesvm::LiteSVM;
use serde::Deserialize;
use solana_sdk::{
    account::Account, instruction::AccountMeta, pubkey::Pubkey, signature::Keypair, signer::Signer,
};
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs,
    create_squads_program_interaction_jupiter_fixture_swap_policy_instruction,
    derive_squads_policy, execute_mock_jupiter_sol_to_usdc_swap_instruction,
    execute_squads_program_interaction_instruction, get_spl_token_amount,
    mock_jupiter_token_accounts, seed_mock_jupiter_spl_accounts, seed_spl_token_account,
    try_send_instructions, MockProgram, SquadsCompiledInstruction, JUPITER_V6_PROGRAM_ID,
    LAMPORTS_PER_SOL, PYUSD_MINT, USDC_MINT,
};
use std::{collections::HashMap, env, process::Command, str::FromStr};

const POLICY_SEED: u64 = 1;

const JUPITER_USDC_PYUSD_FIXTURE: &str = include_str!("../fixtures/jupiter/usdc-pyusd-build.json");

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterBuildFixture {
    input_mint: String,
    output_mint: String,
    in_amount: String,
    out_amount: String,
    other_amount_threshold: String,
    swap_mode: String,
    slippage_bps: u16,
    route_plan: Vec<JupiterRoutePlan>,
    compute_budget_instructions: Vec<JupiterInstruction>,
    setup_instructions: Vec<JupiterInstruction>,
    swap_instruction: JupiterInstruction,
    cleanup_instruction: Option<JupiterInstruction>,
    other_instructions: Vec<JupiterInstruction>,
    tip_instruction: Option<JupiterInstruction>,
    addresses_by_lookup_table_address: HashMap<String, Vec<String>>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterRoutePlan {
    percent: u16,
    bps: u16,
    swap_info: JupiterSwapInfo,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterSwapInfo {
    amm_key: String,
    label: String,
    input_mint: String,
    output_mint: String,
    in_amount: String,
    out_amount: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterInstruction {
    program_id: String,
    accounts: Vec<JupiterAccountMeta>,
    data: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterAccountMeta {
    pubkey: String,
    is_signer: bool,
    is_writable: bool,
}

fn load_jupiter_usdc_pyusd_fixture() -> JupiterBuildFixture {
    let fixture: JupiterBuildFixture =
        serde_json::from_str(JUPITER_USDC_PYUSD_FIXTURE).expect("parse Jupiter fixture");
    assert_jupiter_usdc_pyusd_fixture_contract(&fixture);
    fixture
}

fn assert_jupiter_usdc_pyusd_fixture_contract(fixture: &JupiterBuildFixture) {
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
    assert!(accounts.len() >= 5);
    assert!(accounts[0].is_signer);
    assert!(!accounts[0].is_writable);
    assert!(accounts[1].is_writable);
    assert!(accounts[2].is_writable);
    assert_eq!(accounts[3].pubkey, USDC_MINT.to_string());
    assert_eq!(accounts[4].pubkey, PYUSD_MINT.to_string());
    assert_eq!(accounts[5].pubkey, spl_token::id().to_string());
}

fn decode_jupiter_swap_data(fixture: &JupiterBuildFixture) -> Vec<u8> {
    BASE64_STANDARD
        .decode(&fixture.swap_instruction.data)
        .expect("decode Jupiter swap instruction data")
}

fn parse_fixture_amount(amount: &str) -> u64 {
    amount.parse().expect("parse Jupiter raw token amount")
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

fn jupiter_fixture_transaction(
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

fn seed_jupiter_fixture_accounts(
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

fn jupiter_build_url() -> String {
    format!(
        "https://api.jup.ag/swap/v2/build?inputMint={}&outputMint={}&amount=1000000&taker=11111111111111111111111111111111&maxAccounts=16&slippageBps=50",
        USDC_MINT, PYUSD_MINT
    )
}

fn shell_single_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\\''"))
}

#[test]
fn wallet_b_can_execute_allowed_usdc_to_pyusd_swap_intent() {
    let jupiter_fixture = load_jupiter_usdc_pyusd_fixture();
    let jupiter_swap_data = decode_jupiter_swap_data(&jupiter_fixture);
    let fixture_in_amount = parse_fixture_amount(&jupiter_fixture.in_amount);
    let fixture_out_amount = parse_fixture_amount(&jupiter_fixture.out_amount);

    let mut context = create_funded_squads_test_context_with_mock_programs(&[MockProgram::Jupiter])
        .expect("create funded Squads test context");
    let Some(context) = context.as_mut() else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };
    let wallet_b = Keypair::new();
    context
        .svm
        .airdrop(&wallet_b.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop wallet B");

    let vault_usdc = Keypair::new().pubkey();
    let vault_pyusd = Keypair::new().pubkey();
    let jupiter_sol_escrow = Keypair::new().pubkey();
    seed_mock_jupiter_spl_accounts(&mut context.svm, fixture_in_amount, fixture_out_amount);
    seed_spl_token_account(&mut context.svm, vault_usdc, USDC_MINT, context.vault, 0);
    seed_spl_token_account(&mut context.svm, vault_pyusd, PYUSD_MINT, context.vault, 0);

    let (policy, _) = derive_squads_policy(&context.pool.settings, POLICY_SEED);
    let create_policy_ix =
        create_squads_program_interaction_jupiter_fixture_swap_policy_instruction(
            context.pool.settings,
            context.wallet_pubkey(),
            wallet_b.pubkey(),
            POLICY_SEED,
            context.vault_index,
            context.vault,
            vault_usdc,
            vault_pyusd,
            &jupiter_swap_data,
        );
    try_send_instructions(&mut context.svm, &[create_policy_ix], &context.wallet, &[])
        .expect("wallet A creates Jupiter USDC/PYUSD swap policy for wallet B");

    let vault_starting_lamports = context.vault_balance();
    let wallet_a_sol_to_usdc_ix = execute_mock_jupiter_sol_to_usdc_swap_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        context.vault_index,
        context.vault,
        vault_usdc,
        jupiter_sol_escrow,
        fixture_in_amount,
    );
    try_send_instructions(
        &mut context.svm,
        &[wallet_a_sol_to_usdc_ix],
        &context.wallet,
        &[],
    )
    .expect("wallet A swaps vault SOL to USDC through the local Jupiter router");
    assert_eq!(
        context.vault_balance(),
        vault_starting_lamports - fixture_in_amount
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        fixture_in_amount
    );

    let (jupiter_transaction_accounts, jupiter_instruction_accounts, program_id_index) =
        jupiter_fixture_transaction(&jupiter_fixture, context.vault, vault_usdc, vault_pyusd);
    seed_jupiter_fixture_accounts(
        &mut context.svm,
        &jupiter_fixture,
        &jupiter_transaction_accounts,
    );

    let wallet_b_usdc_to_pyusd_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        vec![SquadsCompiledInstruction {
            program_id_index,
            accounts: jupiter_instruction_accounts,
            data: jupiter_swap_data,
        }],
        vec![0],
        jupiter_transaction_accounts,
    );
    try_send_instructions(
        &mut context.svm,
        &[wallet_b_usdc_to_pyusd_ix],
        &wallet_b,
        &[],
    )
    .expect("wallet B swaps the full vault USDC balance to PYUSD through policy");

    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_pyusd),
        fixture_out_amount
    );
}

#[test]
fn live_jupiter_usdc_pyusd_router_contract_matches_fixture_when_enabled() {
    if env::var("JUPITER_REFRESH_FIXTURES").ok().as_deref() != Some("1") {
        eprintln!("skipping live Jupiter fixture check; set JUPITER_REFRESH_FIXTURES=1");
        return;
    }

    let mut curl_command = String::from("curl -sSL");
    if let Ok(api_key) = env::var("JUPITER_API_KEY") {
        curl_command.push_str(" --header ");
        curl_command.push_str(&shell_single_quote(&format!("x-api-key: {api_key}")));
    }
    curl_command.push(' ');
    curl_command.push_str(&shell_single_quote(&jupiter_build_url()));

    let output = Command::new("/bin/zsh")
        .arg("-lc")
        .arg(curl_command)
        .output()
        .expect("fetch live Jupiter build response");
    if matches!(output.status.code(), Some(6 | 7)) {
        eprintln!(
            "skipping live Jupiter fixture check; curl could not reach api.jup.ag: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        return;
    }
    assert!(
        output.status.success(),
        "Jupiter build request failed: status={:?}, stderr={}, stdout={}",
        output.status,
        String::from_utf8_lossy(&output.stderr),
        String::from_utf8_lossy(&output.stdout)
    );

    let fixture: JupiterBuildFixture =
        serde_json::from_slice(&output.stdout).expect("parse live Jupiter build response");
    assert_jupiter_usdc_pyusd_fixture_contract(&fixture);
}
