mod common;

use common::{
    decode_jupiter_swap_data, jupiter_fixture_transaction, load_jupiter_usdc_pyusd_fixture,
    parse_fixture_amount, seed_jupiter_fixture_accounts,
};
use solana_sdk::{signature::Keypair, signer::Signer};
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs,
    create_squads_program_interaction_usdc_pyusd_kamino_route_policy_instruction,
    derive_squads_policy, execute_mock_jupiter_sol_to_usdc_swap_instruction,
    execute_squads_program_interaction_instruction, get_spl_token_amount,
    mock_kamino_deposit_reserve_liquidity_data, mock_kamino_reserve_transaction,
    mock_kamino_withdraw_reserve_liquidity_data, seed_mock_jupiter_spl_accounts,
    seed_mock_kamino_reserve_spl_accounts, seed_mock_kamino_reserve_spl_accounts_with_mint,
    try_send_instructions, MockProgram, SquadsCompiledInstruction, KAMINO_MAIN_MARKET,
    KAMINO_MAIN_PYUSD_RESERVE, KAMINO_MAIN_USDC_RESERVE, LAMPORTS_PER_SOL, PYUSD_DECIMALS,
    PYUSD_MINT,
};

const POLICY_SEED: u64 = 1;

#[test]
fn wallet_b_can_execute_single_policy_usdc_pyusd_kamino_route() {
    let jupiter_fixture = load_jupiter_usdc_pyusd_fixture();
    let jupiter_swap_data = decode_jupiter_swap_data(&jupiter_fixture);
    let fixture_in_amount = parse_fixture_amount(&jupiter_fixture.in_amount);
    let fixture_out_amount = parse_fixture_amount(&jupiter_fixture.out_amount);

    let mut context = create_funded_squads_test_context_with_mock_programs(&[
        MockProgram::Jupiter,
        MockProgram::KaminoLend,
    ])
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
    let vault_usdc_collateral = Keypair::new().pubkey();
    let vault_pyusd_collateral = Keypair::new().pubkey();
    let usdc_reserve_liquidity_supply = Keypair::new().pubkey();
    let pyusd_reserve_liquidity_supply = Keypair::new().pubkey();
    let jupiter_sol_escrow = Keypair::new().pubkey();

    seed_mock_jupiter_spl_accounts(&mut context.svm, fixture_in_amount, fixture_out_amount);
    let usdc_reserve_accounts = seed_mock_kamino_reserve_spl_accounts(
        &mut context.svm,
        KAMINO_MAIN_USDC_RESERVE,
        KAMINO_MAIN_MARKET,
        context.vault,
        vault_usdc,
        vault_usdc_collateral,
        usdc_reserve_liquidity_supply,
    );
    let pyusd_reserve_accounts = seed_mock_kamino_reserve_spl_accounts_with_mint(
        &mut context.svm,
        KAMINO_MAIN_PYUSD_RESERVE,
        KAMINO_MAIN_MARKET,
        PYUSD_MINT,
        PYUSD_DECIMALS,
        context.vault,
        vault_pyusd,
        vault_pyusd_collateral,
        pyusd_reserve_liquidity_supply,
    );

    let usdc_deposit_data = mock_kamino_deposit_reserve_liquidity_data(fixture_in_amount);
    let usdc_withdraw_data = mock_kamino_withdraw_reserve_liquidity_data(fixture_in_amount);
    let pyusd_deposit_data = mock_kamino_deposit_reserve_liquidity_data(fixture_out_amount);
    let pyusd_withdraw_data = mock_kamino_withdraw_reserve_liquidity_data(fixture_out_amount);

    let (policy, _) = derive_squads_policy(&context.pool.settings, POLICY_SEED);
    let create_policy_ix =
        create_squads_program_interaction_usdc_pyusd_kamino_route_policy_instruction(
            context.pool.settings,
            context.wallet_pubkey(),
            wallet_b.pubkey(),
            POLICY_SEED,
            context.vault_index,
            context.vault,
            vault_usdc,
            vault_pyusd,
            vault_usdc_collateral,
            usdc_reserve_liquidity_supply,
            vault_pyusd_collateral,
            pyusd_reserve_liquidity_supply,
            usdc_deposit_data.clone(),
            usdc_withdraw_data.clone(),
            jupiter_swap_data.clone(),
            pyusd_deposit_data.clone(),
            pyusd_withdraw_data.clone(),
        );
    try_send_instructions(&mut context.svm, &[create_policy_ix], &context.wallet, &[])
        .expect("wallet A creates one exact USDC/PYUSD Kamino route policy for wallet B");

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
    .expect("wallet A funds the vault USDC balance through the local Jupiter router");
    assert_eq!(
        context.vault_balance(),
        vault_starting_lamports - fixture_in_amount
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        fixture_in_amount
    );

    let (wrong_usdc_deposit_instructions, wrong_usdc_deposit_accounts) =
        mock_kamino_reserve_transaction(
            context.vault,
            usdc_reserve_accounts,
            mock_kamino_deposit_reserve_liquidity_data(fixture_in_amount - 1),
        );
    let wrong_usdc_deposit_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        wrong_usdc_deposit_instructions,
        vec![0],
        wrong_usdc_deposit_accounts,
    );
    let wrong_usdc_deposit_result =
        try_send_instructions(&mut context.svm, &[wrong_usdc_deposit_ix], &wallet_b, &[]);
    assert!(
        wrong_usdc_deposit_result.is_err(),
        "wallet B should not be able to change the exact USDC deposit amount"
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        fixture_in_amount
    );

    let (usdc_deposit_instructions, usdc_deposit_accounts) =
        mock_kamino_reserve_transaction(context.vault, usdc_reserve_accounts, usdc_deposit_data);
    let usdc_deposit_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        usdc_deposit_instructions,
        vec![0],
        usdc_deposit_accounts,
    );
    try_send_instructions(&mut context.svm, &[usdc_deposit_ix], &wallet_b, &[])
        .expect("wallet B deposits all vault USDC into Kamino Main Market USDC reserve");
    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc_collateral),
        fixture_in_amount
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, usdc_reserve_liquidity_supply),
        fixture_in_amount
    );

    let (usdc_withdraw_instructions, usdc_withdraw_accounts) =
        mock_kamino_reserve_transaction(context.vault, usdc_reserve_accounts, usdc_withdraw_data);
    let usdc_withdraw_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        usdc_withdraw_instructions,
        vec![1],
        usdc_withdraw_accounts,
    );
    try_send_instructions(&mut context.svm, &[usdc_withdraw_ix], &wallet_b, &[])
        .expect("wallet B withdraws all vault USDC from Kamino Main Market USDC reserve");
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        fixture_in_amount
    );
    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc_collateral), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, usdc_reserve_liquidity_supply),
        0
    );

    let (jupiter_transaction_accounts, jupiter_instruction_accounts, program_id_index) =
        jupiter_fixture_transaction(&jupiter_fixture, context.vault, vault_usdc, vault_pyusd);
    seed_jupiter_fixture_accounts(
        &mut context.svm,
        &jupiter_fixture,
        &jupiter_transaction_accounts,
    );
    let usdc_to_pyusd_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        vec![SquadsCompiledInstruction {
            program_id_index,
            accounts: jupiter_instruction_accounts,
            data: jupiter_swap_data,
        }],
        vec![2],
        jupiter_transaction_accounts,
    );
    try_send_instructions(&mut context.svm, &[usdc_to_pyusd_ix], &wallet_b, &[])
        .expect("wallet B swaps all vault USDC to PYUSD through the route policy");
    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_pyusd),
        fixture_out_amount
    );

    let (pyusd_deposit_instructions, pyusd_deposit_accounts) =
        mock_kamino_reserve_transaction(context.vault, pyusd_reserve_accounts, pyusd_deposit_data);
    let pyusd_deposit_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        pyusd_deposit_instructions,
        vec![3],
        pyusd_deposit_accounts,
    );
    try_send_instructions(&mut context.svm, &[pyusd_deposit_ix], &wallet_b, &[])
        .expect("wallet B deposits all vault PYUSD into Kamino Main Market PYUSD reserve");
    assert_eq!(get_spl_token_amount(&context.svm, vault_pyusd), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_pyusd_collateral),
        fixture_out_amount
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, pyusd_reserve_liquidity_supply),
        fixture_out_amount
    );

    let (pyusd_withdraw_instructions, pyusd_withdraw_accounts) =
        mock_kamino_reserve_transaction(context.vault, pyusd_reserve_accounts, pyusd_withdraw_data);
    let pyusd_withdraw_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        pyusd_withdraw_instructions,
        vec![4],
        pyusd_withdraw_accounts,
    );
    try_send_instructions(&mut context.svm, &[pyusd_withdraw_ix], &wallet_b, &[])
        .expect("wallet B withdraws all vault PYUSD from Kamino Main Market PYUSD reserve");
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_pyusd),
        fixture_out_amount
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_pyusd_collateral),
        0
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, pyusd_reserve_liquidity_supply),
        0
    );
}
