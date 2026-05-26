use loyal_actions::{
    create_swap_yield_route_action_with_swap_lanes, SwapLane, YieldRouteActionInstruction,
    YIELD_ROUTE_STANDALONE_ACTION_SEED,
};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
};
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs, create_squads_smart_account_instruction,
    derive_loyal_hub_authority, derive_loyal_hub_config, derive_squads_pool, derive_squads_vault,
    execute_mock_jupiter_sol_to_usdc_swap_instruction, execute_squads_sync_transaction_instruction,
    execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index,
    execute_squads_yield_route_stable_swap_instruction, get_spl_token_amount,
    initialize_loyal_hub_config_instruction, loyal_action_context, loyal_hub_swap_exact_in_data,
    loyal_hub_token_account, loyal_hub_withdraw_inventory_data,
    mock_jupiter_stable_reserve_token_account, seed_loyal_hub_inventory_spl_accounts,
    seed_mock_jupiter_spl_accounts, seed_mock_jupiter_stable_reserve_spl_accounts,
    seed_spl_mint_if_missing, seed_spl_token_account, set_loyal_hub_max_fee_instruction,
    set_loyal_hub_paused_instruction, try_send_instructions,
    withdraw_loyal_hub_inventory_instruction, MockJupiterStableReserveTokenAccount, MockProgram,
    SquadsCompiledInstruction, SquadsPool, LAMPORTS_PER_SOL, LOYAL_HUB_SWAP_PROGRAM_ID,
    PYUSD_DECIMALS, PYUSD_MINT, USDC_DECIMALS, USDC_MINT,
};

include!("loyal_hub_swap/support.rs");

#[test]
fn treasury_backed_simulation_covers_hub_jupiter_and_inventory_movement() {
    let Some(mut context) = create_funded_squads_test_context_with_mock_programs(&[
        MockProgram::LoyalHubSwap,
        MockProgram::Jupiter,
    ])
    .expect("create funded Squads test context") else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let wallet_b = Keypair::new();
    let wallet_c = Keypair::new();
    context
        .svm
        .airdrop(&wallet_b.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop wallet B");
    context
        .svm
        .airdrop(&wallet_c.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop wallet C");

    let route_input_amount = 3 * AMOUNT_IN;
    let hub_usdc_top_up = 500_000;
    let hub_pyusd_top_up = 2_000_000;
    let treasury_usdc_start = 10_000_000;
    let treasury_pyusd_start = 10_000_000;

    seed_spl_mint_if_missing(&mut context.svm, USDC_MINT, None, USDC_DECIMALS, 0);
    seed_spl_mint_if_missing(&mut context.svm, PYUSD_MINT, None, PYUSD_DECIMALS, 0);
    seed_loyal_hub_inventory_spl_accounts(&mut context.svm, &[USDC_MINT, PYUSD_MINT], 0);
    seed_mock_jupiter_spl_accounts(&mut context.svm, route_input_amount, route_input_amount);
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
        5 * AMOUNT_IN,
    );

    let vault_usdc = Keypair::new().pubkey();
    let vault_pyusd = Keypair::new().pubkey();
    let jupiter_sol_escrow = Keypair::new().pubkey();
    seed_spl_token_account(&mut context.svm, vault_usdc, USDC_MINT, context.vault, 0);
    seed_spl_token_account(&mut context.svm, vault_pyusd, PYUSD_MINT, context.vault, 0);

    let wallet_a_sol_to_usdc_ix = execute_mock_jupiter_sol_to_usdc_swap_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        context.vault_index,
        context.vault,
        vault_usdc,
        jupiter_sol_escrow,
        route_input_amount,
    );
    try_send_instructions(
        &mut context.svm,
        &[wallet_a_sol_to_usdc_ix],
        &context.wallet,
        &[],
    )
    .expect("wallet A swaps funded Squads vault SOL to USDC");
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        route_input_amount
    );

    let treasury = create_treasury_squads(
        &mut context,
        wallet_c.pubkey(),
        treasury_usdc_start,
        treasury_pyusd_start,
    );
    let init_hub_ix = treasury_initialize_hub_ix(&treasury, wallet_c.pubkey(), 50);
    try_send_instructions(&mut context.svm, &[init_hub_ix], &wallet_c, &[])
        .expect("Loyal Treasury initializes Loyal Hub config");

    let top_up_usdc_ix = treasury_top_up_hub_ix(
        &treasury,
        wallet_c.pubkey(),
        treasury.usdc,
        USDC_MINT,
        loyal_hub_token_account(USDC_MINT),
        hub_usdc_top_up,
        USDC_DECIMALS,
    );
    let top_up_pyusd_ix = treasury_top_up_hub_ix(
        &treasury,
        wallet_c.pubkey(),
        treasury.pyusd,
        PYUSD_MINT,
        loyal_hub_token_account(PYUSD_MINT),
        hub_pyusd_top_up,
        PYUSD_DECIMALS,
    );
    try_send_instructions(
        &mut context.svm,
        &[top_up_usdc_ix, top_up_pyusd_ix],
        &wallet_c,
        &[],
    )
    .expect("Loyal Treasury funds hot hub inventory");
    assert_eq!(
        get_spl_token_amount(&context.svm, loyal_hub_token_account(USDC_MINT)),
        hub_usdc_top_up
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, loyal_hub_token_account(PYUSD_MINT)),
        hub_pyusd_top_up
    );

    let swap_action = create_swap_yield_route_action_with_swap_lanes(
        loyal_action_context(&context, wallet_b.pubkey()),
        vec![USDC_MINT, PYUSD_MINT],
        vec![
            SwapLane::Jupiter,
            SwapLane::LoyalHub {
                hub_authorizer: wallet_c.pubkey(),
                max_fee_bps: 50,
            },
        ],
        YIELD_ROUTE_STANDALONE_ACTION_SEED,
    )
    .expect("build Jupiter and Loyal Hub swap action");
    try_send_instructions(
        &mut context.svm,
        &[swap_action.instruction.clone()],
        &context.wallet,
        &[],
    )
    .expect("wallet A creates policy allowing Jupiter and Loyal Hub lanes for wallet B");

    let full_hub_ix = execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
        swap_action.account,
        wallet_b.pubkey(),
        context.vault_index,
        context.vault,
        vault_usdc,
        vault_pyusd,
        USDC_MINT,
        PYUSD_MINT,
        wallet_c.pubkey(),
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        1,
    );
    try_send_instructions(&mut context.svm, &[full_hub_ix], &wallet_b, &[&wallet_c])
        .expect("wallet B executes full Loyal Hub fill authorized by wallet C");

    let half_hub_in = AMOUNT_IN / 2;
    let half_hub_out = 499_500;
    let half_jupiter_in = AMOUNT_IN - half_hub_in;
    let half_jupiter_out = half_jupiter_in;
    let half_hub_ix = execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
        swap_action.account,
        wallet_b.pubkey(),
        context.vault_index,
        context.vault,
        vault_usdc,
        vault_pyusd,
        USDC_MINT,
        PYUSD_MINT,
        wallet_c.pubkey(),
        half_hub_in,
        half_hub_out,
        half_hub_out,
        MAX_FEE_BPS,
        1,
    );
    let half_jupiter_ix = execute_squads_yield_route_stable_swap_instruction(
        swap_action.account,
        wallet_b.pubkey(),
        context.vault_index,
        context.vault,
        vault_usdc,
        vault_pyusd,
        USDC_MINT,
        PYUSD_MINT,
        half_jupiter_in,
        half_jupiter_out,
    );
    try_send_instructions(
        &mut context.svm,
        &[half_hub_ix, half_jupiter_ix],
        &wallet_b,
        &[&wallet_c],
    )
    .expect("wallet B executes half Loyal Hub fill and Jupiter residual");

    let jupiter_only_ix = execute_squads_yield_route_stable_swap_instruction(
        swap_action.account,
        wallet_b.pubkey(),
        context.vault_index,
        context.vault,
        vault_usdc,
        vault_pyusd,
        USDC_MINT,
        PYUSD_MINT,
        AMOUNT_IN,
        AMOUNT_IN,
    );
    try_send_instructions(&mut context.svm, &[jupiter_only_ix], &wallet_b, &[])
        .expect("wallet B executes Jupiter-only fallback fill");

    let expected_user_pyusd = HUB_OUT + half_hub_out + half_jupiter_out + AMOUNT_IN;
    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_pyusd),
        expected_user_pyusd
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, loyal_hub_token_account(USDC_MINT)),
        hub_usdc_top_up + AMOUNT_IN + half_hub_in
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, loyal_hub_token_account(PYUSD_MINT)),
        hub_pyusd_top_up - HUB_OUT - half_hub_out
    );

    let rebalance_withdraw_usdc = 1_200_000;
    let rebalance_top_up_pyusd = 300_000;
    let rebalance_ix = treasury_rebalance_hub_ix(
        &treasury,
        wallet_c.pubkey(),
        rebalance_withdraw_usdc,
        rebalance_top_up_pyusd,
    );
    try_send_instructions(&mut context.svm, &[rebalance_ix], &wallet_c, &[])
        .expect("Loyal Treasury rebalances hub inventory in both directions");

    let final_pyusd_withdraw = 100_000;
    let withdraw_pyusd_ix = treasury_withdraw_hub_ix(
        &treasury,
        wallet_c.pubkey(),
        loyal_hub_token_account(PYUSD_MINT),
        treasury.pyusd,
        PYUSD_MINT,
        final_pyusd_withdraw,
    );
    try_send_instructions(&mut context.svm, &[withdraw_pyusd_ix], &wallet_c, &[])
        .expect("Loyal Treasury withdraws hot PYUSD inventory");

    assert_eq!(
        get_spl_token_amount(&context.svm, treasury.usdc),
        treasury_usdc_start - hub_usdc_top_up + rebalance_withdraw_usdc
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, treasury.pyusd),
        treasury_pyusd_start - hub_pyusd_top_up - rebalance_top_up_pyusd + final_pyusd_withdraw
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, loyal_hub_token_account(USDC_MINT)),
        hub_usdc_top_up + AMOUNT_IN + half_hub_in - rebalance_withdraw_usdc
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, loyal_hub_token_account(PYUSD_MINT)),
        hub_pyusd_top_up - HUB_OUT - half_hub_out + rebalance_top_up_pyusd - final_pyusd_withdraw
    );
}

#[test]
fn loyal_hub_full_fill_swaps_atomically_through_squads_policy() {
    let Some(mut fixture) = setup_fixture(false) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let ix = hub_swap_ix(&fixture, AMOUNT_IN, HUB_OUT);
    try_send_instructions(
        &mut fixture.context.svm,
        &[ix],
        &fixture.wallet_b,
        &[&fixture.hub_authorizer],
    )
    .expect("wallet B swaps through Loyal Hub with separate hub authorizer");

    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc),
        0
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_pyusd),
        HUB_OUT
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, loyal_hub_token_account(USDC_MINT)),
        AMOUNT_IN * 3
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, loyal_hub_token_account(PYUSD_MINT)),
        (AMOUNT_IN * 2) - HUB_OUT
    );
}

#[test]
fn loyal_hub_rejects_missing_hub_authorizer_signature() {
    let Some(mut fixture) = setup_fixture(false) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let mut ix = hub_swap_ix(&fixture, AMOUNT_IN, HUB_OUT);
    for account in &mut ix.accounts {
        if account.pubkey == fixture.hub_authorizer.pubkey() {
            account.is_signer = false;
        }
    }

    let error = try_send_instructions(&mut fixture.context.svm, &[ix], &fixture.wallet_b, &[])
        .expect_err("hub authorizer must sign the outer transaction");
    assert!(error.contains("MissingRequiredSignature"), "{error}");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc),
        AMOUNT_IN
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_pyusd),
        0
    );
}

#[test]
fn loyal_hub_rejects_wrong_output_destination() {
    let Some(mut fixture) = setup_fixture(false) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };
    let wrong_output = Keypair::new().pubkey();
    seed_spl_token_account(
        &mut fixture.context.svm,
        wrong_output,
        USDC_MINT,
        fixture.context.vault,
        0,
    );

    let ix = execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
        fixture.swap_action.account,
        fixture.wallet_b.pubkey(),
        fixture.context.vault_index,
        fixture.context.vault,
        fixture.vault_usdc,
        wrong_output,
        USDC_MINT,
        PYUSD_MINT,
        fixture.hub_authorizer.pubkey(),
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        1,
    );
    let error = try_send_instructions(
        &mut fixture.context.svm,
        &[ix],
        &fixture.wallet_b,
        &[&fixture.hub_authorizer],
    )
    .expect_err("hub rejects output accounts with the wrong mint");
    assert!(error.contains("InvalidAccountData"), "{error}");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc),
        AMOUNT_IN
    );
}

#[test]
fn loyal_hub_rejects_non_canonical_inventory_accounts() {
    let Some(mut fixture) = setup_fixture(false) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let direct_user = Keypair::new();
    fixture
        .context
        .svm
        .airdrop(&direct_user.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop direct user");
    let direct_user_usdc = Keypair::new().pubkey();
    let direct_user_pyusd = Keypair::new().pubkey();
    let wrong_hub_usdc = Keypair::new().pubkey();
    seed_spl_token_account(
        &mut fixture.context.svm,
        direct_user_usdc,
        USDC_MINT,
        direct_user.pubkey(),
        AMOUNT_IN,
    );
    seed_spl_token_account(
        &mut fixture.context.svm,
        direct_user_pyusd,
        PYUSD_MINT,
        direct_user.pubkey(),
        0,
    );
    seed_spl_token_account(
        &mut fixture.context.svm,
        wrong_hub_usdc,
        USDC_MINT,
        derive_loyal_hub_authority(),
        0,
    );

    let ix = Instruction {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new_readonly(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(direct_user.pubkey(), true),
            AccountMeta::new(direct_user_usdc, false),
            AccountMeta::new(direct_user_pyusd, false),
            AccountMeta::new(wrong_hub_usdc, false),
            AccountMeta::new(loyal_hub_token_account(PYUSD_MINT), false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new_readonly(PYUSD_MINT, false),
            AccountMeta::new_readonly(derive_loyal_hub_authority(), false),
            AccountMeta::new_readonly(fixture.hub_authorizer.pubkey(), true),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
        data: loyal_hub_swap_exact_in_data(AMOUNT_IN, HUB_OUT, MIN_OUT, MAX_FEE_BPS),
    };
    let error = try_send_instructions(
        &mut fixture.context.svm,
        &[ix],
        &fixture.context.wallet,
        &[&direct_user, &fixture.hub_authorizer],
    )
    .expect_err("hub rejects non-canonical inventory account");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, direct_user_usdc),
        AMOUNT_IN
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, direct_user_pyusd),
        0
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, wrong_hub_usdc),
        0
    );
}

#[test]
fn loyal_hub_rejects_same_mint_swaps() {
    let Some(mut fixture) = setup_fixture(false) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };
    let second_usdc = Keypair::new().pubkey();
    seed_spl_token_account(
        &mut fixture.context.svm,
        second_usdc,
        USDC_MINT,
        fixture.context.vault,
        0,
    );

    let ix = execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
        fixture.swap_action.account,
        fixture.wallet_b.pubkey(),
        fixture.context.vault_index,
        fixture.context.vault,
        fixture.vault_usdc,
        second_usdc,
        USDC_MINT,
        USDC_MINT,
        fixture.hub_authorizer.pubkey(),
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        1,
    );
    let error = try_send_instructions(
        &mut fixture.context.svm,
        &[ix],
        &fixture.wallet_b,
        &[&fixture.hub_authorizer],
    )
    .expect_err("hub rejects same-mint swaps");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc),
        AMOUNT_IN
    );
    assert_eq!(get_spl_token_amount(&fixture.context.svm, second_usdc), 0);
}

#[test]
fn loyal_hub_rejects_duplicate_mutable_token_accounts() {
    let Some(mut fixture) = setup_fixture(false) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let ix = execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
        fixture.swap_action.account,
        fixture.wallet_b.pubkey(),
        fixture.context.vault_index,
        fixture.context.vault,
        fixture.vault_usdc,
        fixture.vault_usdc,
        USDC_MINT,
        PYUSD_MINT,
        fixture.hub_authorizer.pubkey(),
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        1,
    );
    let error = try_send_instructions(
        &mut fixture.context.svm,
        &[ix],
        &fixture.wallet_b,
        &[&fixture.hub_authorizer],
    )
    .expect_err("hub rejects duplicated mutable token accounts");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc),
        AMOUNT_IN
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_pyusd),
        0
    );
}

#[test]
fn loyal_hub_rejects_excessive_fee_and_paused_swaps() {
    let Some(mut fixture) = setup_fixture(false) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let excessive_fee_ix =
        execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
            fixture.swap_action.account,
            fixture.wallet_b.pubkey(),
            fixture.context.vault_index,
            fixture.context.vault,
            fixture.vault_usdc,
            fixture.vault_pyusd,
            USDC_MINT,
            PYUSD_MINT,
            fixture.hub_authorizer.pubkey(),
            AMOUNT_IN,
            900_000,
            900_000,
            MAX_FEE_BPS,
            1,
        );
    let error = try_send_instructions(
        &mut fixture.context.svm,
        &[excessive_fee_ix],
        &fixture.wallet_b,
        &[&fixture.hub_authorizer],
    )
    .expect_err("hub rejects output below the fee cap");
    assert!(error.contains("InvalidArgument"), "{error}");

    let pause_ix = set_loyal_hub_paused_instruction(fixture.context.wallet_pubkey(), true);
    try_send_instructions(
        &mut fixture.context.svm,
        &[pause_ix],
        &fixture.context.wallet,
        &[],
    )
    .expect("pause Loyal Hub");
    let paused_ix = hub_swap_ix(&fixture, AMOUNT_IN, HUB_OUT);
    let error = try_send_instructions(
        &mut fixture.context.svm,
        &[paused_ix],
        &fixture.wallet_b,
        &[&fixture.hub_authorizer],
    )
    .expect_err("hub rejects swaps while paused");
    assert!(error.contains("InvalidArgument"), "{error}");
}

#[test]
fn loyal_hub_admin_can_withdraw_hot_inventory() {
    let Some(mut fixture) = setup_fixture(false) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };
    let treasury_usdc = Keypair::new().pubkey();
    let treasury_owner = fixture.context.wallet_pubkey();
    seed_spl_token_account(
        &mut fixture.context.svm,
        treasury_usdc,
        USDC_MINT,
        treasury_owner,
        0,
    );

    let withdraw_ix = withdraw_loyal_hub_inventory_instruction(
        fixture.context.wallet_pubkey(),
        loyal_hub_token_account(USDC_MINT),
        treasury_usdc,
        USDC_MINT,
        250_000,
    );
    try_send_instructions(
        &mut fixture.context.svm,
        &[withdraw_ix],
        &fixture.context.wallet,
        &[],
    )
    .expect("admin withdraws hub inventory to treasury token account");

    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, treasury_usdc),
        250_000
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, loyal_hub_token_account(USDC_MINT)),
        (AMOUNT_IN * 2) - 250_000
    );
}

#[test]
fn loyal_hub_admin_can_update_max_fee() {
    let Some(mut fixture) = setup_fixture(false) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let set_fee_ix = set_loyal_hub_max_fee_instruction(fixture.context.wallet_pubkey(), 5);
    try_send_instructions(
        &mut fixture.context.svm,
        &[set_fee_ix],
        &fixture.context.wallet,
        &[],
    )
    .expect("admin lowers hub max fee");

    let ix = hub_swap_ix(&fixture, AMOUNT_IN, HUB_OUT);
    let error = try_send_instructions(
        &mut fixture.context.svm,
        &[ix],
        &fixture.wallet_b,
        &[&fixture.hub_authorizer],
    )
    .expect_err("hub rejects swaps above the updated max fee");
    assert!(error.contains("InvalidArgument"), "{error}");
}

#[test]
fn route_policy_allows_partial_hub_fill_then_jupiter_residual() {
    let Some(mut fixture) = setup_fixture(true) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let residual_in = 400_000;
    let residual_out = 400_000;
    seed_mock_jupiter_stable_reserve_spl_accounts(
        &mut fixture.context.svm,
        &[
            squads_test_harness::MockJupiterStableReserveTokenAccount {
                mint: USDC_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(USDC_MINT),
            },
            squads_test_harness::MockJupiterStableReserveTokenAccount {
                mint: PYUSD_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
            },
        ],
        AMOUNT_IN,
    );

    let hub_ix = hub_swap_ix(&fixture, AMOUNT_IN - residual_in, 599_400);
    let jupiter_ix = execute_squads_yield_route_stable_swap_instruction(
        fixture.swap_action.account,
        fixture.wallet_b.pubkey(),
        fixture.context.vault_index,
        fixture.context.vault,
        fixture.vault_usdc,
        fixture.vault_pyusd,
        USDC_MINT,
        PYUSD_MINT,
        residual_in,
        residual_out,
    );

    try_send_instructions(
        &mut fixture.context.svm,
        &[hub_ix, jupiter_ix],
        &fixture.wallet_b,
        &[&fixture.hub_authorizer],
    )
    .expect("route uses Loyal Hub first and Jupiter for residual liquidity");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc),
        0
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_pyusd),
        999_400
    );
}

#[test]
fn route_policy_still_allows_jupiter_only_fallback() {
    let Some(mut fixture) = setup_fixture(true) else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };
    seed_mock_jupiter_stable_reserve_spl_accounts(
        &mut fixture.context.svm,
        &[
            squads_test_harness::MockJupiterStableReserveTokenAccount {
                mint: USDC_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(USDC_MINT),
            },
            squads_test_harness::MockJupiterStableReserveTokenAccount {
                mint: PYUSD_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
            },
        ],
        AMOUNT_IN,
    );

    let jupiter_ix = execute_squads_yield_route_stable_swap_instruction(
        fixture.swap_action.account,
        fixture.wallet_b.pubkey(),
        fixture.context.vault_index,
        fixture.context.vault,
        fixture.vault_usdc,
        fixture.vault_pyusd,
        USDC_MINT,
        PYUSD_MINT,
        AMOUNT_IN,
        AMOUNT_IN,
    );
    try_send_instructions(
        &mut fixture.context.svm,
        &[jupiter_ix],
        &fixture.wallet_b,
        &[],
    )
    .expect("Jupiter fallback does not need Loyal Hub authorizer");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc),
        0
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_pyusd),
        AMOUNT_IN
    );
    assert_ne!(
        LOYAL_HUB_SWAP_PROGRAM_ID,
        squads_test_harness::JUPITER_V6_PROGRAM_ID
    );
}
