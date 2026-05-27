mod common;

use common::{load_jupiter_usdc_pyusd_fixture, parse_fixture_amount};
use loyal_actions::{
    create_all_in_one_market_mint_yield_route_action, create_all_in_one_mint_yield_route_action,
    create_three_step_yield_route_actions, SwapLane, YieldRouteActionSeeds,
};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    signature::Keypair,
    signer::Signer,
};
use solana_system_interface::instruction as system_instruction;
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs,
    execute_mock_jupiter_sol_to_usdc_swap_instruction, execute_squads_sync_transaction_instruction,
    get_spl_token_amount, initialize_loyal_hub_config_instruction, loyal_action_context,
    loyal_hub_token_account, mock_jupiter_stable_reserve_token_account, mock_jupiter_swap_data,
    mock_jupiter_swap_lane, mock_jupiter_token_accounts,
    mock_kamino_deposit_reserve_liquidity_data, mock_kamino_reserve_transaction,
    mock_kamino_withdraw_reserve_liquidity_data, seed_loyal_hub_inventory_spl_accounts,
    seed_mock_jupiter_spl_accounts, seed_mock_jupiter_stable_reserve_spl_accounts,
    seed_mock_kamino_reserve_spl_accounts, seed_mock_kamino_reserve_spl_accounts_with_mint,
    seed_spl_token_account, try_send_instructions, try_send_instructions_with_heap_frame,
    yield_route_universe_from_mock_reserves, HubRouteExecution, HubSwapExecution,
    JupiterRouteExecution, JupiterSwapExecution, MockJupiterStableReserveTokenAccount, MockProgram,
    RouteActionExt, JUPITER_V6_PROGRAM_ID, KAMINO_MAIN_MARKET, KAMINO_MAIN_PYUSD_RESERVE,
    KAMINO_MAIN_USDC_RESERVE, KAMINO_PRIME_MARKET, KAMINO_PRIME_USDC_RESERVE, LAMPORTS_PER_SOL,
    MOCK_JUPITER_SOL_TO_USDC, PYUSD_DECIMALS, PYUSD_MINT, USDC_DECIMALS, USDC_MINT,
    WRAPPED_SOL_MINT,
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

    let route_action_setup = create_three_step_yield_route_actions(
        loyal_action_context(context, wallet_b.pubkey()),
        yield_route_universe_from_mock_reserves(
            vec![USDC_MINT, PYUSD_MINT],
            vec![
                main_usdc_reserve_accounts,
                prime_usdc_reserve_accounts,
                pyusd_reserve_accounts,
            ],
        ),
        vec![mock_jupiter_swap_lane(true)],
        YieldRouteActionSeeds::default(),
    )
    .expect("build route actions");
    let withdraw = route_action_setup.withdraw().expect("route has withdraw");
    let jupiter = route_action_setup
        .jupiter()
        .expect("route has Jupiter swap");
    let deposit = route_action_setup.deposit().expect("route has deposit");
    try_send_instructions(
        &mut context.svm,
        &route_action_setup.instructions,
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
    let main_withdraw_ix = withdraw.build(
        wallet_b.pubkey(),
        context.vault_index,
        main_withdraw_instructions,
        main_withdraw_accounts,
    );
    let (prime_deposit_instructions, prime_deposit_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        prime_usdc_reserve_accounts,
        usdc_deposit_data.clone(),
    );
    let prime_deposit_ix = deposit.build(
        wallet_b.pubkey(),
        context.vault_index,
        prime_deposit_instructions,
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
    let prime_withdraw_ix = withdraw.build(
        wallet_b.pubkey(),
        context.vault_index,
        prime_withdraw_instructions,
        prime_withdraw_accounts,
    );
    let usdc_to_pyusd_ix = jupiter.build(JupiterSwapExecution {
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
    let (pyusd_deposit_instructions, pyusd_deposit_accounts) =
        mock_kamino_reserve_transaction(context.vault, pyusd_reserve_accounts, pyusd_deposit_data);
    let pyusd_deposit_ix = deposit.build(
        wallet_b.pubkey(),
        context.vault_index,
        pyusd_deposit_instructions,
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
fn wallet_b_can_execute_reduced_all_in_one_yield_route_policy() {
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
    let vault_pyusd_collateral = Keypair::new().pubkey();
    let main_usdc_reserve_liquidity_supply = Keypair::new().pubkey();
    let pyusd_reserve_liquidity_supply = Keypair::new().pubkey();
    let jupiter_sol_escrow = Keypair::new().pubkey();

    seed_mock_jupiter_spl_accounts(&mut context.svm, fixture_in_amount, fixture_out_amount);
    seed_mock_jupiter_stable_reserve_spl_accounts(
        &mut context.svm,
        &[
            MockJupiterStableReserveTokenAccount {
                mint: USDC_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(USDC_MINT),
            },
            MockJupiterStableReserveTokenAccount {
                mint: PYUSD_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
            },
        ],
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

    let route_action_setup = create_all_in_one_market_mint_yield_route_action(
        loyal_action_context(context, wallet_b.pubkey()),
        yield_route_universe_from_mock_reserves(
            vec![USDC_MINT, PYUSD_MINT],
            vec![main_usdc_reserve_accounts, pyusd_reserve_accounts],
        ),
        vec![mock_jupiter_swap_lane(false)],
    )
    .expect("build all-in-one route action");
    let route_accounts = route_action_setup.accounts;
    let jupiter_route = route_action_setup
        .jupiter_route_action()
        .expect("route has one Jupiter execution");
    assert_eq!(
        route_action_setup
            .jupiter_route()
            .expect("route metadata")
            .instruction_constraint_indexes(),
        &[0, 1, 2]
    );
    assert_eq!(route_action_setup.instructions.len(), 1);
    assert_eq!(route_accounts.withdraw, route_accounts.swap);
    assert_eq!(route_accounts.swap, route_accounts.deposit);
    try_send_instructions_with_heap_frame(
        &mut context.svm,
        &route_action_setup.instructions,
        &context.wallet,
        &[],
    )
    .expect("wallet A creates one all-in-one yield route policy for Wallet B");

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
        mock_kamino_deposit_reserve_liquidity_data(fixture_in_amount),
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
        mock_kamino_withdraw_reserve_liquidity_data(fixture_in_amount),
    );
    let (pyusd_deposit_instructions, pyusd_deposit_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        pyusd_reserve_accounts,
        mock_kamino_deposit_reserve_liquidity_data(fixture_out_amount),
    );
    let route_ix = jupiter_route.build(JupiterRouteExecution {
        withdraw_instructions: main_withdraw_instructions,
        withdraw_accounts: main_withdraw_accounts,
        swap: JupiterSwapExecution {
            signer: wallet_b.pubkey(),
            vault_index: context.vault_index,
            vault: context.vault,
            vault_input: vault_usdc,
            vault_output: vault_pyusd,
            input_mint: USDC_MINT,
            output_mint: PYUSD_MINT,
            in_amount: fixture_in_amount,
            out_amount: fixture_out_amount,
        },
        deposit_instructions: pyusd_deposit_instructions,
        deposit_accounts: pyusd_deposit_accounts,
    });
    try_send_instructions_with_heap_frame(&mut context.svm, &[route_ix], &wallet_b, &[])
        .expect("wallet B runs withdraw, swap, and deposit through one policy call");

    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), 0);
    assert_eq!(get_spl_token_amount(&context.svm, vault_pyusd), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_main_usdc_collateral),
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
fn wallet_b_can_execute_same_mint_route_through_one_policy_call() {
    let amount = 1_000_000;

    let mut context =
        create_funded_squads_test_context_with_mock_programs(&[MockProgram::KaminoLend])
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
    let vault_main_usdc_collateral = Keypair::new().pubkey();
    let vault_prime_usdc_collateral = Keypair::new().pubkey();
    let main_usdc_reserve_liquidity_supply = Keypair::new().pubkey();
    let prime_usdc_reserve_liquidity_supply = Keypair::new().pubkey();

    seed_spl_token_account(
        &mut context.svm,
        vault_usdc,
        USDC_MINT,
        context.vault,
        amount,
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

    let route_action_setup = create_all_in_one_market_mint_yield_route_action(
        loyal_action_context(context, wallet_b.pubkey()),
        yield_route_universe_from_mock_reserves(
            vec![USDC_MINT],
            vec![main_usdc_reserve_accounts, prime_usdc_reserve_accounts],
        ),
        vec![mock_jupiter_swap_lane(false)],
    )
    .expect("build all-in-one same-mint route action");
    let same_mint_route = route_action_setup
        .same_mint_route_action()
        .expect("route has one same-mint execution");
    assert_eq!(
        route_action_setup
            .same_mint_route()
            .expect("route metadata")
            .instruction_constraint_indexes(),
        &[0, 2]
    );
    try_send_instructions_with_heap_frame(
        &mut context.svm,
        &route_action_setup.instructions,
        &context.wallet,
        &[],
    )
    .expect("wallet A creates one all-in-one route policy for Wallet B");

    let (main_deposit_instructions, main_deposit_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        main_usdc_reserve_accounts,
        mock_kamino_deposit_reserve_liquidity_data(amount),
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
        mock_kamino_withdraw_reserve_liquidity_data(amount),
    );
    let (prime_deposit_instructions, prime_deposit_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        prime_usdc_reserve_accounts,
        mock_kamino_deposit_reserve_liquidity_data(amount),
    );
    let route_ix = same_mint_route.build(
        wallet_b.pubkey(),
        context.vault_index,
        main_withdraw_instructions,
        main_withdraw_accounts,
        prime_deposit_instructions,
        prime_deposit_accounts,
    );
    try_send_instructions_with_heap_frame(&mut context.svm, &[route_ix], &wallet_b, &[])
        .expect("wallet B switches Main USDC to Prime USDC through one policy call");

    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_main_usdc_collateral),
        0
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_prime_usdc_collateral),
        amount
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, prime_usdc_reserve_liquidity_supply),
        amount
    );
}

#[test]
fn wallet_b_can_execute_all_in_one_policy_with_loyal_hub_swap_lane() {
    let amount_in = 1_000_000;
    let hub_out = 995_000;
    let max_fee_bps = 50;

    let mut context = create_funded_squads_test_context_with_mock_programs(&[
        MockProgram::Jupiter,
        MockProgram::KaminoLend,
        MockProgram::LoyalHubSwap,
    ])
    .expect("create funded Squads test context");
    let Some(context) = context.as_mut() else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let wallet_b = Keypair::new();
    let hub_authorizer = Keypair::new();
    context
        .svm
        .airdrop(&wallet_b.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop wallet B");
    context
        .svm
        .airdrop(&hub_authorizer.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop hub authorizer");

    let vault_usdc = Keypair::new().pubkey();
    let vault_pyusd = Keypair::new().pubkey();
    let vault_main_usdc_collateral = Keypair::new().pubkey();
    let vault_pyusd_collateral = Keypair::new().pubkey();
    let main_usdc_reserve_liquidity_supply = Keypair::new().pubkey();
    let pyusd_reserve_liquidity_supply = Keypair::new().pubkey();
    let jupiter_sol_escrow = Keypair::new().pubkey();

    seed_mock_jupiter_spl_accounts(&mut context.svm, amount_in, amount_in);
    seed_loyal_hub_inventory_spl_accounts(&mut context.svm, &[USDC_MINT, PYUSD_MINT], 0);
    let main_usdc_reserve_accounts = seed_mock_kamino_reserve_spl_accounts(
        &mut context.svm,
        KAMINO_MAIN_USDC_RESERVE,
        KAMINO_MAIN_MARKET,
        context.vault,
        vault_usdc,
        vault_main_usdc_collateral,
        main_usdc_reserve_liquidity_supply,
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
    seed_spl_token_account(
        &mut context.svm,
        loyal_hub_token_account(PYUSD_MINT),
        PYUSD_MINT,
        squads_test_harness::derive_loyal_hub_authority(),
        hub_out,
    );

    let init_hub_ix = initialize_loyal_hub_config_instruction(
        hub_authorizer.pubkey(),
        hub_authorizer.pubkey(),
        hub_authorizer.pubkey(),
        max_fee_bps,
        false,
        &[USDC_MINT, PYUSD_MINT],
    );
    try_send_instructions(&mut context.svm, &[init_hub_ix], &hub_authorizer, &[])
        .expect("hub authorizer initializes Loyal Hub config");

    let route_action_setup = create_all_in_one_mint_yield_route_action(
        loyal_action_context(context, wallet_b.pubkey()),
        yield_route_universe_from_mock_reserves(
            vec![USDC_MINT, PYUSD_MINT],
            vec![main_usdc_reserve_accounts, pyusd_reserve_accounts],
        ),
        vec![
            mock_jupiter_swap_lane(false),
            SwapLane::LoyalHub {
                hub_authorizer: hub_authorizer.pubkey(),
                max_fee_bps,
            },
        ],
    )
    .expect("build all-in-one route action with Loyal Hub lane");
    let hub_route = route_action_setup
        .loyal_hub_route_action()
        .expect("route has one Loyal Hub execution");
    assert_eq!(
        route_action_setup
            .loyal_hub_route()
            .expect("route metadata")
            .instruction_constraint_indexes(),
        &[0, 2, 3]
    );
    try_send_instructions_with_heap_frame(
        &mut context.svm,
        &route_action_setup.instructions,
        &context.wallet,
        &[],
    )
    .expect("wallet A creates one all-in-one policy with Jupiter and Loyal Hub lanes");

    let wallet_a_sol_to_usdc_ix = execute_mock_jupiter_sol_to_usdc_swap_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        context.vault_index,
        context.vault,
        vault_usdc,
        jupiter_sol_escrow,
        amount_in,
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
        mock_kamino_deposit_reserve_liquidity_data(amount_in),
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
        mock_kamino_withdraw_reserve_liquidity_data(amount_in),
    );
    let (pyusd_deposit_instructions, pyusd_deposit_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        pyusd_reserve_accounts,
        mock_kamino_deposit_reserve_liquidity_data(hub_out),
    );
    let route_ix = hub_route.build(HubRouteExecution {
        withdraw_instructions: main_withdraw_instructions,
        withdraw_accounts: main_withdraw_accounts,
        swap: HubSwapExecution {
            signer: wallet_b.pubkey(),
            vault_index: context.vault_index,
            vault: context.vault,
            vault_input: vault_usdc,
            vault_output: vault_pyusd,
            input_mint: USDC_MINT,
            output_mint: PYUSD_MINT,
            hub_authorizer: hub_authorizer.pubkey(),
            amount_in,
            amount_out: hub_out,
            min_out: hub_out,
            max_fee_bps,
            lane_id: 0,
        },
        deposit_instructions: pyusd_deposit_instructions,
        deposit_accounts: pyusd_deposit_accounts,
    });
    try_send_instructions_with_heap_frame(
        &mut context.svm,
        &[route_ix],
        &wallet_b,
        &[&hub_authorizer],
    )
    .expect("wallet B runs withdraw, Loyal Hub swap, and deposit through one policy call");

    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), 0);
    assert_eq!(get_spl_token_amount(&context.svm, vault_pyusd), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_main_usdc_collateral),
        0
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_pyusd_collateral),
        hub_out
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

    let route_action_setup = create_three_step_yield_route_actions(
        loyal_action_context(context, wallet_b.pubkey()),
        yield_route_universe_from_mock_reserves(
            vec![USDC_MINT, PYUSD_MINT],
            vec![
                main_usdc_reserve_accounts,
                prime_usdc_reserve_accounts,
                pyusd_reserve_accounts,
            ],
        ),
        vec![mock_jupiter_swap_lane(true)],
        YieldRouteActionSeeds::default(),
    )
    .expect("build route actions");
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
    packed_instructions.extend(route_action_setup.instructions);
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
        .get_account(&route_action_setup.accounts.withdraw)
        .is_some());
    assert!(context
        .svm
        .get_account(&route_action_setup.accounts.swap)
        .is_some());
    assert!(context
        .svm
        .get_account(&route_action_setup.accounts.deposit)
        .is_some());
}

#[test]
fn wallet_a_can_pack_vault_usdc_deposit_and_reduced_all_in_one_policy() {
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
    let vault_pyusd_collateral = Keypair::new().pubkey();
    let main_usdc_reserve_liquidity_supply = Keypair::new().pubkey();
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

    let route_action_setup = create_all_in_one_market_mint_yield_route_action(
        loyal_action_context(context, wallet_b.pubkey()),
        yield_route_universe_from_mock_reserves(
            vec![USDC_MINT, PYUSD_MINT],
            vec![main_usdc_reserve_accounts, pyusd_reserve_accounts],
        ),
        vec![mock_jupiter_swap_lane(false)],
    )
    .expect("build all-in-one route action");
    let route_accounts = route_action_setup.accounts;
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
    packed_instructions.extend(route_action_setup.instructions);
    try_send_instructions_with_heap_frame(
        &mut context.svm,
        &packed_instructions,
        &context.wallet,
        &[],
    )
    .expect(
        "wallet A deposits USDC into vault 0 and creates the all-in-one policy in one transaction",
    );

    assert_eq!(get_spl_token_amount(&context.svm, wallet_usdc), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        deposit_amount
    );
    assert_eq!(route_accounts.withdraw, route_accounts.swap);
    assert_eq!(route_accounts.swap, route_accounts.deposit);
    assert!(context.svm.get_account(&route_accounts.withdraw).is_some());
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
