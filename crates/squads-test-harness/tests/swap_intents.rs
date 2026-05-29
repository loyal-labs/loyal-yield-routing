mod common;

use common::{
    assert_jupiter_usdc_pyusd_fixture_contract, decode_jupiter_swap_data, jupiter_build_url,
    jupiter_fixture_transaction, load_jupiter_usdc_pyusd_fixture, parse_fixture_amount,
    seed_jupiter_fixture_accounts, shell_single_quote, JupiterBuildFixture,
};
use loyal_actions::{
    create_swap_yield_route_action, JupiterSwapContract, SwapLane,
    YIELD_ROUTE_STANDALONE_ACTION_SEED,
};
use solana_sdk::{signature::Keypair, signer::Signer};
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs,
    execute_mock_jupiter_sol_to_usdc_swap_instruction,
    execute_squads_program_interaction_instruction, get_spl_token_amount, loyal_action_context,
    mock_jupiter_stable_reserve_token_account, mock_jupiter_swap_lane,
    seed_mock_jupiter_spl_accounts, seed_mock_jupiter_stable_reserve_spl_accounts,
    seed_spl_token_account, try_send_instructions, JupiterSwapExecution,
    MockJupiterStableReserveTokenAccount, MockProgram, RouteActionExt, SquadsCompiledInstruction,
    JUPITER_SWAP_DISCRIMINATOR, JUPITER_V6_PROGRAM_ID, LAMPORTS_PER_SOL, PYUSD_MINT, USDC_MINT,
};
use std::{env, process::Command};

#[test]
fn wallet_b_can_execute_allowed_usdc_to_pyusd_swap_intent() {
    let jupiter_fixture = load_jupiter_usdc_pyusd_fixture();
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
    let stable_reserves = vec![
        MockJupiterStableReserveTokenAccount {
            mint: USDC_MINT,
            reserve: mock_jupiter_stable_reserve_token_account(USDC_MINT),
        },
        MockJupiterStableReserveTokenAccount {
            mint: PYUSD_MINT,
            reserve: mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
        },
    ];
    seed_mock_jupiter_stable_reserve_spl_accounts(
        &mut context.svm,
        &stable_reserves,
        fixture_in_amount.max(fixture_out_amount),
    );
    seed_spl_token_account(&mut context.svm, vault_usdc, USDC_MINT, context.vault, 0);
    seed_spl_token_account(&mut context.svm, vault_pyusd, PYUSD_MINT, context.vault, 0);

    let swap_action_setup = create_swap_yield_route_action(
        loyal_action_context(context, wallet_b.pubkey()),
        vec![USDC_MINT, PYUSD_MINT],
        vec![mock_jupiter_swap_lane(true)],
        YIELD_ROUTE_STANDALONE_ACTION_SEED,
    )
    .expect("build swap action");
    try_send_instructions(
        &mut context.svm,
        &[swap_action_setup.instruction.clone()],
        &context.wallet,
        &[],
    )
    .expect("wallet A creates route-mint USDC/PYUSD swap policy for wallet B");

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

    let wallet_b_usdc_to_pyusd_ix = swap_action_setup
        .jupiter()
        .expect("swap action has Jupiter lane")
        .build(JupiterSwapExecution {
            signer: wallet_b.pubkey(),
            vault_index: context.vault_index,
            vault: context.vault,
            vault_input: vault_usdc,
            vault_output: vault_pyusd,
            input_mint: USDC_MINT,
            output_mint: PYUSD_MINT,
            in_amount: fixture_in_amount,
            out_amount: fixture_out_amount,
        });
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
fn wallet_b_can_execute_captured_jupiter_build_swap_instruction_through_policy() {
    let jupiter_fixture = load_jupiter_usdc_pyusd_fixture();
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
    seed_mock_jupiter_spl_accounts(&mut context.svm, fixture_in_amount, fixture_out_amount);
    seed_spl_token_account(
        &mut context.svm,
        vault_usdc,
        USDC_MINT,
        context.vault,
        fixture_in_amount,
    );
    seed_spl_token_account(&mut context.svm, vault_pyusd, PYUSD_MINT, context.vault, 0);

    let swap_action_setup = create_swap_yield_route_action(
        loyal_action_context(context, wallet_b.pubkey()),
        vec![USDC_MINT, PYUSD_MINT],
        vec![SwapLane::Jupiter(JupiterSwapContract {
            program_id: JUPITER_V6_PROGRAM_ID,
            exact_in_discriminator: JUPITER_SWAP_DISCRIMINATOR,
        })],
        YIELD_ROUTE_STANDALONE_ACTION_SEED,
    )
    .expect("build Jupiter swap action");
    try_send_instructions(
        &mut context.svm,
        &[swap_action_setup.instruction.clone()],
        &context.wallet,
        &[],
    )
    .expect("wallet A creates Jupiter policy for wallet B");

    let (transaction_accounts, instruction_accounts, program_id_index) =
        jupiter_fixture_transaction(&jupiter_fixture, context.vault, vault_usdc, vault_pyusd);
    seed_jupiter_fixture_accounts(&mut context.svm, &jupiter_fixture, &transaction_accounts);
    let swap_ix = execute_squads_program_interaction_instruction(
        swap_action_setup.account,
        wallet_b.pubkey(),
        context.vault_index,
        vec![SquadsCompiledInstruction {
            program_id_index,
            accounts: instruction_accounts,
            data: decode_jupiter_swap_data(&jupiter_fixture),
        }],
        vec![0],
        transaction_accounts,
    );

    try_send_instructions(&mut context.svm, &[swap_ix], &wallet_b, &[])
        .expect("captured Jupiter /build swap instruction executes through LiteSVM mock");

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
