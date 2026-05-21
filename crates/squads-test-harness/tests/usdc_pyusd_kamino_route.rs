mod common;

use common::{load_jupiter_usdc_pyusd_fixture, parse_fixture_amount};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    signature::Keypair,
    signer::Signer,
};
use solana_system_interface::instruction as system_instruction;
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs,
    create_squads_yield_route_policy_instructions,
    execute_mock_jupiter_sol_to_usdc_swap_instruction,
    execute_squads_program_interaction_instruction, execute_squads_sync_transaction_instruction,
    execute_squads_yield_route_stable_swap_instruction, get_spl_token_amount,
    mock_jupiter_stable_reserve_token_account, mock_jupiter_swap_data, mock_jupiter_token_accounts,
    mock_kamino_deposit_reserve_liquidity_data, mock_kamino_reserve_transaction,
    mock_kamino_withdraw_reserve_liquidity_data, seed_mock_jupiter_spl_accounts,
    seed_mock_jupiter_stable_reserve_spl_accounts, seed_mock_kamino_reserve_spl_accounts,
    seed_mock_kamino_reserve_spl_accounts_with_mint, seed_spl_token_account, try_send_instructions,
    MockJupiterStableReserveTokenAccount, MockProgram, SquadsYieldRoutePolicyWhitelist,
    JUPITER_V6_PROGRAM_ID, KAMINO_MAIN_MARKET, KAMINO_MAIN_PYUSD_RESERVE, KAMINO_MAIN_USDC_RESERVE,
    KAMINO_PRIME_MARKET, KAMINO_PRIME_USDC_RESERVE, LAMPORTS_PER_SOL, MOCK_JUPITER_SOL_TO_USDC,
    PYUSD_DECIMALS, PYUSD_MINT, USDC_DECIMALS, USDC_MINT, WRAPPED_SOL_MINT,
};

#[test]
fn wallet_b_can_execute_bundled_kamino_yield_route_switches() {
    let jupiter_fixture = load_jupiter_usdc_pyusd_fixture();
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

    let route_policy_setup = create_squads_yield_route_policy_instructions(
        context,
        wallet_b.pubkey(),
        SquadsYieldRoutePolicyWhitelist {
            stable_mints: vec![USDC_MINT, PYUSD_MINT],
            kamino_reserves: vec![
                main_usdc_reserve_accounts,
                prime_usdc_reserve_accounts,
                pyusd_reserve_accounts,
            ],
        },
    );
    let route_policies = route_policy_setup.policies;
    try_send_instructions(
        &mut context.svm,
        &route_policy_setup.instructions,
        &context.wallet,
        &[],
    )
    .expect("wallet A creates route-sized withdraw, swap, and deposit policies for Wallet B");

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
        route_policies.withdraw,
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
        route_policies.deposit,
        wallet_b.pubkey(),
        context.vault_index,
        prime_deposit_instructions,
        vec![0],
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
        route_policies.withdraw,
        wallet_b.pubkey(),
        context.vault_index,
        prime_withdraw_instructions,
        vec![0],
        prime_withdraw_accounts,
    );
    let usdc_to_pyusd_ix = execute_squads_yield_route_stable_swap_instruction(
        route_policies.swap,
        wallet_b.pubkey(),
        context.vault_index,
        context.vault,
        vault_usdc,
        vault_pyusd,
        USDC_MINT,
        PYUSD_MINT,
        fixture_in_amount,
        fixture_out_amount,
    );
    let (pyusd_deposit_instructions, pyusd_deposit_accounts) =
        mock_kamino_reserve_transaction(context.vault, pyusd_reserve_accounts, pyusd_deposit_data);
    let pyusd_deposit_ix = execute_squads_program_interaction_instruction(
        route_policies.deposit,
        wallet_b.pubkey(),
        context.vault_index,
        pyusd_deposit_instructions,
        vec![0],
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

#[test]
fn wallet_a_can_pack_vault_usdc_deposit_and_three_yield_route_policies() {
    let deposit_amount = 1_000_000;
    let mut context = create_funded_squads_test_context_with_mock_programs(&[
        MockProgram::Jupiter,
        MockProgram::KaminoLend,
    ])
    .expect("create funded Squads test context");
    let Some(context) = context.as_mut() else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };
    let wallet_a = context.wallet_pubkey();

    let wallet_b = Keypair::new();
    context
        .svm
        .airdrop(&wallet_b.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop wallet B");

    let wallet_usdc = Keypair::new().pubkey();
    let vault_usdc = Keypair::new().pubkey();
    let vault_pyusd = Keypair::new().pubkey();
    let vault_main_usdc_collateral = Keypair::new().pubkey();
    let vault_prime_usdc_collateral = Keypair::new().pubkey();
    let vault_pyusd_collateral = Keypair::new().pubkey();
    let main_usdc_reserve_liquidity_supply = Keypair::new().pubkey();
    let prime_usdc_reserve_liquidity_supply = Keypair::new().pubkey();
    let pyusd_reserve_liquidity_supply = Keypair::new().pubkey();
    let jupiter_sol_escrow = Keypair::new().pubkey();

    seed_mock_jupiter_spl_accounts(&mut context.svm, deposit_amount, deposit_amount);
    seed_spl_token_account(&mut context.svm, wallet_usdc, USDC_MINT, wallet_a, 0);
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

    let wallet_a_sol_to_usdc_ix =
        wallet_mock_jupiter_sol_to_usdc_swap_instruction(wallet_a, wallet_usdc, deposit_amount);
    try_send_instructions(
        &mut context.svm,
        &[
            system_instruction::transfer(&wallet_a, &jupiter_sol_escrow, deposit_amount),
            wallet_a_sol_to_usdc_ix,
        ],
        &context.wallet,
        &[],
    )
    .expect("wallet A swaps SOL to wallet-owned USDC after smart-account creation");
    assert_eq!(
        get_spl_token_amount(&context.svm, wallet_usdc),
        deposit_amount
    );
    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), 0);

    let route_policy_setup = create_squads_yield_route_policy_instructions(
        context,
        wallet_b.pubkey(),
        SquadsYieldRoutePolicyWhitelist {
            stable_mints: vec![USDC_MINT, PYUSD_MINT],
            kamino_reserves: vec![
                main_usdc_reserve_accounts,
                prime_usdc_reserve_accounts,
                pyusd_reserve_accounts,
            ],
        },
    );
    let deposit_usdc_to_vault0_ix = spl_token::instruction::transfer_checked(
        &spl_token::id(),
        &wallet_usdc,
        &USDC_MINT,
        &vault_usdc,
        &wallet_a,
        &[],
        deposit_amount,
        USDC_DECIMALS,
    )
    .expect("build wallet A USDC deposit into vault 0");

    let mut packed_instructions = vec![deposit_usdc_to_vault0_ix];
    packed_instructions.extend(route_policy_setup.instructions);
    try_send_instructions(
        &mut context.svm,
        &packed_instructions,
        &context.wallet,
        &[],
    )
    .expect("wallet A deposits USDC into vault 0 and creates withdraw, swap, and deposit policies in one transaction");

    assert_eq!(get_spl_token_amount(&context.svm, wallet_usdc), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        deposit_amount
    );
    assert!(context
        .svm
        .get_account(&route_policy_setup.policies.withdraw)
        .is_some());
    assert!(context
        .svm
        .get_account(&route_policy_setup.policies.swap)
        .is_some());
    assert!(context
        .svm
        .get_account(&route_policy_setup.policies.deposit)
        .is_some());
}

fn wallet_mock_jupiter_sol_to_usdc_swap_instruction(
    wallet: solana_sdk::pubkey::Pubkey,
    wallet_usdc: solana_sdk::pubkey::Pubkey,
    amount: u64,
) -> Instruction {
    let jupiter_accounts = mock_jupiter_token_accounts();
    Instruction {
        program_id: JUPITER_V6_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new_readonly(wallet, true),
            AccountMeta::new(wallet_usdc, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new(jupiter_accounts.usdc_reserve, false),
            AccountMeta::new_readonly(jupiter_accounts.authority, false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
        data: mock_jupiter_swap_data(
            MOCK_JUPITER_SOL_TO_USDC,
            amount,
            WRAPPED_SOL_MINT,
            USDC_MINT,
        ),
    }
}
