mod common;

use common::{
    assert_jupiter_usdc_pyusd_fixture_contract, decode_jupiter_swap_data, jupiter_build_url,
    jupiter_fixture_transaction, load_jupiter_usdc_pyusd_fixture, parse_fixture_amount,
    seed_jupiter_fixture_accounts, shell_single_quote, JupiterBuildFixture,
};
use solana_sdk::{signature::Keypair, signer::Signer};
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs,
    create_squads_program_interaction_jupiter_fixture_swap_policy_instruction,
    derive_squads_policy, execute_mock_jupiter_sol_to_usdc_swap_instruction,
    execute_squads_program_interaction_instruction, get_spl_token_amount,
    seed_mock_jupiter_spl_accounts, seed_spl_token_account, try_send_instructions, MockProgram,
    SquadsCompiledInstruction, LAMPORTS_PER_SOL, PYUSD_MINT, USDC_MINT,
};
use std::{env, process::Command};

const POLICY_SEED: u64 = 1;

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
