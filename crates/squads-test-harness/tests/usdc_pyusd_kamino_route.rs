mod common;

use common::{
    decode_jupiter_swap_data, jupiter_fixture_transaction, load_jupiter_usdc_pyusd_fixture,
    parse_fixture_amount, seed_jupiter_fixture_accounts,
};
use solana_sdk::{signature::Keypair, signer::Signer};
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs,
    create_squads_program_interaction_jupiter_fixture_swap_policy_instruction,
    create_squads_program_interaction_main_to_prime_usdc_route_policy_instruction,
    create_squads_program_interaction_prime_usdc_to_pyusd_reserves_policy_instruction,
    derive_squads_policy, execute_mock_jupiter_sol_to_usdc_swap_instruction,
    execute_squads_program_interaction_instruction, execute_squads_sync_transaction_instruction,
    get_spl_token_amount, mock_kamino_deposit_reserve_liquidity_data,
    mock_kamino_reserve_transaction, mock_kamino_withdraw_reserve_liquidity_data,
    seed_mock_jupiter_spl_accounts, seed_mock_kamino_reserve_spl_accounts,
    seed_mock_kamino_reserve_spl_accounts_with_mint, try_send_instructions, MockProgram,
    SquadsCompiledInstruction, KAMINO_MAIN_MARKET, KAMINO_MAIN_PYUSD_RESERVE,
    KAMINO_MAIN_USDC_RESERVE, KAMINO_PRIME_MARKET, KAMINO_PRIME_USDC_RESERVE, LAMPORTS_PER_SOL,
    PYUSD_DECIMALS, PYUSD_MINT,
};

const POLICY_SEED: u64 = 1;
const SWAP_POLICY_SEED: u64 = 2;
const CROSS_MINT_RESERVE_POLICY_SEED: u64 = 3;

#[test]
fn wallet_b_can_execute_bundled_kamino_yield_route_switches() {
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
    let vault_main_usdc_collateral = Keypair::new().pubkey();
    let vault_prime_usdc_collateral = Keypair::new().pubkey();
    let vault_pyusd_collateral = Keypair::new().pubkey();
    let main_usdc_reserve_liquidity_supply = Keypair::new().pubkey();
    let prime_usdc_reserve_liquidity_supply = Keypair::new().pubkey();
    let pyusd_reserve_liquidity_supply = Keypair::new().pubkey();
    let jupiter_sol_escrow = Keypair::new().pubkey();

    seed_mock_jupiter_spl_accounts(&mut context.svm, fixture_in_amount, fixture_out_amount);
    let main_usdc_reserve_accounts = seed_mock_kamino_reserve_spl_accounts(
        &mut context.svm,
        KAMINO_MAIN_USDC_RESERVE,
        KAMINO_MAIN_MARKET,
        context.vault,
        vault_usdc,
        vault_main_usdc_collateral,
        main_usdc_reserve_liquidity_supply,
    );
    let prime_usdc_reserve_accounts = seed_mock_kamino_reserve_spl_accounts(
        &mut context.svm,
        KAMINO_PRIME_USDC_RESERVE,
        KAMINO_PRIME_MARKET,
        context.vault,
        vault_usdc,
        vault_prime_usdc_collateral,
        prime_usdc_reserve_liquidity_supply,
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

    let (same_mint_reserve_policy, _) = derive_squads_policy(&context.pool.settings, POLICY_SEED);
    let (swap_policy, _) = derive_squads_policy(&context.pool.settings, SWAP_POLICY_SEED);
    let (cross_mint_reserve_policy, _) =
        derive_squads_policy(&context.pool.settings, CROSS_MINT_RESERVE_POLICY_SEED);
    let create_same_mint_reserve_policy_ix =
        create_squads_program_interaction_main_to_prime_usdc_route_policy_instruction(
            context.pool.settings,
            context.wallet_pubkey(),
            wallet_b.pubkey(),
            POLICY_SEED,
            context.vault_index,
            context.vault,
            vault_usdc,
            vault_main_usdc_collateral,
            main_usdc_reserve_liquidity_supply,
            vault_prime_usdc_collateral,
            prime_usdc_reserve_liquidity_supply,
            usdc_withdraw_data.clone(),
            usdc_deposit_data.clone(),
        );
    let create_cross_mint_reserve_policy_ix =
        create_squads_program_interaction_prime_usdc_to_pyusd_reserves_policy_instruction(
            context.pool.settings,
            context.wallet_pubkey(),
            wallet_b.pubkey(),
            CROSS_MINT_RESERVE_POLICY_SEED,
            context.vault_index,
            context.vault,
            vault_usdc,
            vault_pyusd,
            vault_prime_usdc_collateral,
            prime_usdc_reserve_liquidity_supply,
            vault_pyusd_collateral,
            pyusd_reserve_liquidity_supply,
            usdc_withdraw_data.clone(),
            pyusd_deposit_data.clone(),
        );
    let create_swap_policy_ix =
        create_squads_program_interaction_jupiter_fixture_swap_policy_instruction(
            context.pool.settings,
            context.wallet_pubkey(),
            wallet_b.pubkey(),
            SWAP_POLICY_SEED,
            context.vault_index,
            context.vault,
            vault_usdc,
            vault_pyusd,
            &jupiter_swap_data,
        );
    try_send_instructions(
        &mut context.svm,
        &[
            create_same_mint_reserve_policy_ix,
            create_swap_policy_ix,
            create_cross_mint_reserve_policy_ix,
        ],
        &context.wallet,
        &[],
    )
    .expect("wallet A creates route-sized reserve policies and one swap policy for Wallet B");

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

    let (main_deposit_instructions, main_deposit_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        main_usdc_reserve_accounts,
        usdc_deposit_data.clone(),
    );
    let main_deposit_ix = execute_squads_sync_transaction_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        context.vault_index,
        main_deposit_instructions,
        main_deposit_accounts,
    );
    try_send_instructions(&mut context.svm, &[main_deposit_ix], &context.wallet, &[])
        .expect("wallet A deposits USDC into the starting Kamino reserve");

    let (main_withdraw_instructions, main_withdraw_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        main_usdc_reserve_accounts,
        usdc_withdraw_data.clone(),
    );
    let main_withdraw_ix = execute_squads_program_interaction_instruction(
        same_mint_reserve_policy,
        wallet_b.pubkey(),
        context.vault_index,
        main_withdraw_instructions,
        vec![0],
        main_withdraw_accounts,
    );
    let (prime_deposit_instructions, prime_deposit_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        prime_usdc_reserve_accounts,
        usdc_deposit_data.clone(),
    );
    let prime_deposit_ix = execute_squads_program_interaction_instruction(
        same_mint_reserve_policy,
        wallet_b.pubkey(),
        context.vault_index,
        prime_deposit_instructions,
        vec![1],
        prime_deposit_accounts,
    );
    try_send_instructions(
        &mut context.svm,
        &[main_withdraw_ix, prime_deposit_ix],
        &wallet_b,
        &[],
    )
    .expect("wallet B switches Main USDC to Prime USDC in one outer transaction");
    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_main_usdc_collateral),
        0
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_prime_usdc_collateral),
        fixture_in_amount
    );

    let (prime_withdraw_instructions, prime_withdraw_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        prime_usdc_reserve_accounts,
        usdc_withdraw_data,
    );
    let prime_withdraw_ix = execute_squads_program_interaction_instruction(
        cross_mint_reserve_policy,
        wallet_b.pubkey(),
        context.vault_index,
        prime_withdraw_instructions,
        vec![0],
        prime_withdraw_accounts,
    );
    let (jupiter_transaction_accounts, jupiter_instruction_accounts, program_id_index) =
        jupiter_fixture_transaction(&jupiter_fixture, context.vault, vault_usdc, vault_pyusd);
    seed_jupiter_fixture_accounts(
        &mut context.svm,
        &jupiter_fixture,
        &jupiter_transaction_accounts,
    );
    let usdc_to_pyusd_ix = execute_squads_program_interaction_instruction(
        swap_policy,
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
    let (pyusd_deposit_instructions, pyusd_deposit_accounts) =
        mock_kamino_reserve_transaction(context.vault, pyusd_reserve_accounts, pyusd_deposit_data);
    let pyusd_deposit_ix = execute_squads_program_interaction_instruction(
        cross_mint_reserve_policy,
        wallet_b.pubkey(),
        context.vault_index,
        pyusd_deposit_instructions,
        vec![1],
        pyusd_deposit_accounts,
    );
    try_send_instructions(
        &mut context.svm,
        &[prime_withdraw_ix, usdc_to_pyusd_ix, pyusd_deposit_ix],
        &wallet_b,
        &[],
    )
    .expect("wallet B switches Prime USDC to Main PYUSD through Jupiter in one outer transaction");
    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), 0);
    assert_eq!(get_spl_token_amount(&context.svm, vault_pyusd), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_prime_usdc_collateral),
        0
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_pyusd_collateral),
        fixture_out_amount
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, pyusd_reserve_liquidity_supply),
        fixture_out_amount
    );
}
