#![allow(dead_code, unused_imports)]

use loyal_actions::{
    create_swap_yield_route_action, SwapLane, YieldRouteActionInstruction,
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
    derive_loyal_hub_authority, derive_loyal_hub_config, derive_loyal_hub_lane_authority,
    derive_squads_pool, derive_squads_vault, execute_mock_jupiter_sol_to_usdc_swap_instruction,
    execute_squads_sync_transaction_instruction, get_spl_token_amount,
    initialize_loyal_hub_config_instruction,
    initialize_loyal_hub_config_instruction_with_rebalancer_and_lane_count, loyal_action_context,
    loyal_hub_lane_token_account, loyal_hub_rebalance_inventory_data, loyal_hub_swap_exact_in_data,
    loyal_hub_token_account, loyal_hub_withdraw_inventory_data,
    mock_jupiter_stable_reserve_token_account, mock_jupiter_swap_lane,
    rebalance_loyal_hub_inventory_instruction, seed_loyal_hub_inventory_spl_accounts,
    seed_loyal_hub_inventory_spl_accounts_for_lane, seed_mock_jupiter_spl_accounts,
    seed_mock_jupiter_stable_reserve_spl_accounts, seed_spl_mint_if_missing,
    seed_spl_token_account, set_loyal_hub_max_fee_instruction, set_loyal_hub_paused_instruction,
    try_send_instructions, withdraw_loyal_hub_inventory_instruction, HubSwapExecution,
    JupiterSwapExecution, LoyalHubRebalanceTransfer, MockJupiterStableReserveTokenAccount,
    MockProgram, RouteActionExt, SquadsCompiledInstruction, SquadsPool,
    DEFAULT_LOYAL_HUB_LANE_COUNT, LAMPORTS_PER_SOL, LOYAL_HUB_SWAP_PROGRAM_ID, PYUSD_DECIMALS,
    PYUSD_MINT, USDC_DECIMALS, USDC_MINT,
};

include!("loyal_hub_swap/support.rs");

struct DirectSwapAccounts {
    user: Keypair,
    user_input: Pubkey,
    user_output: Pubkey,
}

struct SwapBalances {
    user_input: u64,
    user_output: u64,
    hub_input: u64,
    hub_output: u64,
}

fn setup_or_skip() -> Option<HubSwapFixture> {
    let fixture = setup_fixture(false);
    if fixture.is_none() {
        eprintln!("skipping QED parity test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
    }
    fixture
}

fn seed_direct_swap_accounts(
    fixture: &mut HubSwapFixture,
    input_balance: u64,
) -> DirectSwapAccounts {
    let user = Keypair::new();
    fixture
        .context
        .svm
        .airdrop(&user.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop direct swap user");
    let user_input = Keypair::new().pubkey();
    let user_output = Keypair::new().pubkey();
    seed_spl_token_account(
        &mut fixture.context.svm,
        user_input,
        USDC_MINT,
        user.pubkey(),
        input_balance,
    );
    seed_spl_token_account(
        &mut fixture.context.svm,
        user_output,
        PYUSD_MINT,
        user.pubkey(),
        0,
    );
    DirectSwapAccounts {
        user,
        user_input,
        user_output,
    }
}

fn direct_hub_swap_ix(
    user: Pubkey,
    user_input: Pubkey,
    user_output: Pubkey,
    hub_input: Pubkey,
    hub_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    hub_authority: Pubkey,
    hub_authorizer: Pubkey,
    hub_authorizer_is_signer: bool,
    amount_in: u64,
    amount_out: u64,
    min_out: u64,
    max_fee_bps: u16,
    lane_id: u8,
) -> Instruction {
    Instruction {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new_readonly(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(user, true),
            AccountMeta::new(user_input, false),
            AccountMeta::new(user_output, false),
            AccountMeta::new(hub_input, false),
            AccountMeta::new(hub_output, false),
            AccountMeta::new_readonly(input_mint, false),
            AccountMeta::new_readonly(output_mint, false),
            AccountMeta::new_readonly(hub_authority, false),
            AccountMeta::new_readonly(hub_authorizer, hub_authorizer_is_signer),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
        data: loyal_hub_swap_exact_in_data(amount_in, amount_out, min_out, max_fee_bps, lane_id),
    }
}

fn valid_direct_hub_swap_ix(
    fixture: &HubSwapFixture,
    accounts: &DirectSwapAccounts,
    amount_in: u64,
    amount_out: u64,
    min_out: u64,
    max_fee_bps: u16,
    lane_id: u8,
) -> Instruction {
    direct_hub_swap_ix(
        accounts.user.pubkey(),
        accounts.user_input,
        accounts.user_output,
        loyal_hub_lane_token_account(USDC_MINT, lane_id),
        loyal_hub_lane_token_account(PYUSD_MINT, lane_id),
        USDC_MINT,
        PYUSD_MINT,
        derive_loyal_hub_lane_authority(lane_id),
        fixture.hub_authorizer.pubkey(),
        true,
        amount_in,
        amount_out,
        min_out,
        max_fee_bps,
        lane_id,
    )
}

fn send_direct_swap(
    fixture: &mut HubSwapFixture,
    ix: Instruction,
    accounts: &DirectSwapAccounts,
    hub_authorizer_should_sign: bool,
) -> Result<(), String> {
    let mut signers = vec![&accounts.user];
    if hub_authorizer_should_sign {
        signers.push(&fixture.hub_authorizer);
    }
    try_send_instructions(
        &mut fixture.context.svm,
        &[ix],
        &fixture.context.wallet,
        &signers,
    )
}

fn send_direct_swap_with_extra_signer(
    fixture: &mut HubSwapFixture,
    ix: Instruction,
    accounts: &DirectSwapAccounts,
    extra_signer: &Keypair,
) -> Result<(), String> {
    let signers = [&accounts.user, extra_signer];
    try_send_instructions(
        &mut fixture.context.svm,
        &[ix],
        &fixture.context.wallet,
        &signers,
    )
}

fn swap_balances(fixture: &HubSwapFixture, accounts: &DirectSwapAccounts) -> SwapBalances {
    swap_balances_for_lane(fixture, accounts, 0)
}

fn swap_balances_for_lane(
    fixture: &HubSwapFixture,
    accounts: &DirectSwapAccounts,
    lane_id: u8,
) -> SwapBalances {
    SwapBalances {
        user_input: get_spl_token_amount(&fixture.context.svm, accounts.user_input),
        user_output: get_spl_token_amount(&fixture.context.svm, accounts.user_output),
        hub_input: get_spl_token_amount(
            &fixture.context.svm,
            loyal_hub_lane_token_account(USDC_MINT, lane_id),
        ),
        hub_output: get_spl_token_amount(
            &fixture.context.svm,
            loyal_hub_lane_token_account(PYUSD_MINT, lane_id),
        ),
    }
}

fn assert_balances_unchanged(
    fixture: &HubSwapFixture,
    accounts: &DirectSwapAccounts,
    before: &SwapBalances,
) {
    let after = swap_balances(fixture, accounts);
    assert_eq!(after.user_input, before.user_input);
    assert_eq!(after.user_output, before.user_output);
    assert_eq!(after.hub_input, before.hub_input);
    assert_eq!(after.hub_output, before.hub_output);
}

fn assert_successful_swap_deltas(
    fixture: &HubSwapFixture,
    accounts: &DirectSwapAccounts,
    before: &SwapBalances,
    amount_in: u64,
    amount_out: u64,
) {
    assert_successful_swap_deltas_for_lane(fixture, accounts, before, amount_in, amount_out, 0);
}

fn assert_successful_swap_deltas_for_lane(
    fixture: &HubSwapFixture,
    accounts: &DirectSwapAccounts,
    before: &SwapBalances,
    amount_in: u64,
    amount_out: u64,
    lane_id: u8,
) {
    let after = swap_balances_for_lane(fixture, accounts, lane_id);
    assert_eq!(after.user_input, before.user_input - amount_in);
    assert_eq!(after.user_output, before.user_output + amount_out);
    assert_eq!(after.hub_input, before.hub_input + amount_in);
    assert_eq!(after.hub_output, before.hub_output - amount_out);
}

#[test]
fn qed_swap_valid_transition_matches_exact_token_deltas() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let ix = valid_direct_hub_swap_ix(
        &fixture,
        &accounts,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        0,
    );

    send_direct_swap(&mut fixture, ix, &accounts, true)
        .expect("valid QED swap transition succeeds");

    assert_successful_swap_deltas(&fixture, &accounts, &before, AMOUNT_IN, HUB_OUT);
}

#[test]
fn qed_swap_rejects_zero_amount_in() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let ix = valid_direct_hub_swap_ix(&fixture, &accounts, 0, HUB_OUT, MIN_OUT, MAX_FEE_BPS, 0);

    let error =
        send_direct_swap(&mut fixture, ix, &accounts, true).expect_err("zero amount_in rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_zero_amount_out() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let ix = valid_direct_hub_swap_ix(&fixture, &accounts, AMOUNT_IN, 0, 0, MAX_FEE_BPS, 0);

    let error =
        send_direct_swap(&mut fixture, ix, &accounts, true).expect_err("zero amount_out rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_output_below_min_out() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let ix = valid_direct_hub_swap_ix(
        &fixture,
        &accounts,
        AMOUNT_IN,
        MIN_OUT - 1,
        MIN_OUT,
        MAX_FEE_BPS,
        0,
    );

    let error = send_direct_swap(&mut fixture, ix, &accounts, true)
        .expect_err("amount_out below min_out rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_when_paused() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let pause_ix = set_loyal_hub_paused_instruction(fixture.context.wallet_pubkey(), true);
    try_send_instructions(
        &mut fixture.context.svm,
        &[pause_ix],
        &fixture.context.wallet,
        &[],
    )
    .expect("pause Loyal Hub");
    let ix = valid_direct_hub_swap_ix(
        &fixture,
        &accounts,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        0,
    );

    let error =
        send_direct_swap(&mut fixture, ix, &accounts, true).expect_err("paused config rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_requested_fee_above_config_max() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let ix = valid_direct_hub_swap_ix(&fixture, &accounts, AMOUNT_IN, HUB_OUT, MIN_OUT, 51, 0);

    let error = send_direct_swap(&mut fixture, ix, &accounts, true)
        .expect_err("requested max_fee_bps above config rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_out_of_range_lane() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let invalid_lane = DEFAULT_LOYAL_HUB_LANE_COUNT;
    let ix = direct_hub_swap_ix(
        accounts.user.pubkey(),
        accounts.user_input,
        accounts.user_output,
        loyal_hub_token_account(USDC_MINT),
        loyal_hub_token_account(PYUSD_MINT),
        USDC_MINT,
        PYUSD_MINT,
        derive_loyal_hub_authority(),
        fixture.hub_authorizer.pubkey(),
        true,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        invalid_lane,
    );

    let error =
        send_direct_swap(&mut fixture, ix, &accounts, true).expect_err("out-of-range lane rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_accepts_highest_configured_lane() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let high_lane = DEFAULT_LOYAL_HUB_LANE_COUNT - 1;
    seed_loyal_hub_inventory_spl_accounts_for_lane(
        &mut fixture.context.svm,
        &[USDC_MINT, PYUSD_MINT],
        AMOUNT_IN * 3,
        high_lane,
    );
    let before = swap_balances_for_lane(&fixture, &accounts, high_lane);

    let ix = direct_hub_swap_ix(
        accounts.user.pubkey(),
        accounts.user_input,
        accounts.user_output,
        loyal_hub_lane_token_account(USDC_MINT, high_lane),
        loyal_hub_lane_token_account(PYUSD_MINT, high_lane),
        USDC_MINT,
        PYUSD_MINT,
        derive_loyal_hub_lane_authority(high_lane),
        fixture.hub_authorizer.pubkey(),
        true,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        high_lane,
    );
    try_send_instructions(
        &mut fixture.context.svm,
        &[ix],
        &fixture.context.wallet,
        &[&accounts.user, &fixture.hub_authorizer],
    )
    .expect("valid lane_id at upper in-range boundary executes");

    assert_successful_swap_deltas_for_lane(
        &fixture, &accounts, &before, AMOUNT_IN, HUB_OUT, high_lane,
    );
}

#[test]
fn qed_swap_rejects_missing_hub_authorizer_signature() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let ix = direct_hub_swap_ix(
        accounts.user.pubkey(),
        accounts.user_input,
        accounts.user_output,
        loyal_hub_token_account(USDC_MINT),
        loyal_hub_token_account(PYUSD_MINT),
        USDC_MINT,
        PYUSD_MINT,
        derive_loyal_hub_authority(),
        fixture.hub_authorizer.pubkey(),
        false,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        0,
    );

    let error = send_direct_swap(&mut fixture, ix, &accounts, false)
        .expect_err("missing hub authorizer signature rejects");
    assert!(error.contains("MissingRequiredSignature"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_wrong_hub_authorizer() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let wrong_authorizer = Keypair::new();
    fixture
        .context
        .svm
        .airdrop(&wrong_authorizer.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop wrong hub authorizer");
    let before = swap_balances(&fixture, &accounts);
    let ix = direct_hub_swap_ix(
        accounts.user.pubkey(),
        accounts.user_input,
        accounts.user_output,
        loyal_hub_token_account(USDC_MINT),
        loyal_hub_token_account(PYUSD_MINT),
        USDC_MINT,
        PYUSD_MINT,
        derive_loyal_hub_authority(),
        wrong_authorizer.pubkey(),
        true,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        0,
    );

    let error = send_direct_swap_with_extra_signer(&mut fixture, ix, &accounts, &wrong_authorizer)
        .expect_err("wrong hub authorizer rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_same_input_and_output_mint() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let second_usdc = Keypair::new().pubkey();
    seed_spl_token_account(
        &mut fixture.context.svm,
        second_usdc,
        USDC_MINT,
        accounts.user.pubkey(),
        0,
    );
    let before = swap_balances(&fixture, &accounts);
    let ix = direct_hub_swap_ix(
        accounts.user.pubkey(),
        accounts.user_input,
        second_usdc,
        loyal_hub_token_account(USDC_MINT),
        loyal_hub_token_account(USDC_MINT),
        USDC_MINT,
        USDC_MINT,
        derive_loyal_hub_authority(),
        fixture.hub_authorizer.pubkey(),
        true,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        0,
    );

    let error =
        send_direct_swap(&mut fixture, ix, &accounts, true).expect_err("same mint swap rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
    assert_eq!(get_spl_token_amount(&fixture.context.svm, second_usdc), 0);
}

#[test]
fn qed_swap_rejects_non_canonical_inventory_account() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let wrong_hub_usdc = Keypair::new().pubkey();
    seed_spl_token_account(
        &mut fixture.context.svm,
        wrong_hub_usdc,
        USDC_MINT,
        derive_loyal_hub_authority(),
        0,
    );
    let before = swap_balances(&fixture, &accounts);
    let ix = direct_hub_swap_ix(
        accounts.user.pubkey(),
        accounts.user_input,
        accounts.user_output,
        wrong_hub_usdc,
        loyal_hub_token_account(PYUSD_MINT),
        USDC_MINT,
        PYUSD_MINT,
        derive_loyal_hub_authority(),
        fixture.hub_authorizer.pubkey(),
        true,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        0,
    );

    let error = send_direct_swap(&mut fixture, ix, &accounts, true)
        .expect_err("non-canonical hub inventory rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, wrong_hub_usdc),
        0
    );
}

#[test]
fn qed_swap_rejects_inventory_from_wrong_lane() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    seed_loyal_hub_inventory_spl_accounts_for_lane(
        &mut fixture.context.svm,
        &[USDC_MINT, PYUSD_MINT],
        AMOUNT_IN * 2,
        1,
    );
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let ix = direct_hub_swap_ix(
        accounts.user.pubkey(),
        accounts.user_input,
        accounts.user_output,
        loyal_hub_token_account(USDC_MINT),
        loyal_hub_lane_token_account(PYUSD_MINT, 1),
        USDC_MINT,
        PYUSD_MINT,
        derive_loyal_hub_lane_authority(1),
        fixture.hub_authorizer.pubkey(),
        true,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        1,
    );

    let error = send_direct_swap(&mut fixture, ix, &accounts, true)
        .expect_err("wrong lane inventory rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_duplicate_mutable_token_account() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let ix = direct_hub_swap_ix(
        accounts.user.pubkey(),
        accounts.user_input,
        accounts.user_input,
        loyal_hub_token_account(USDC_MINT),
        loyal_hub_token_account(PYUSD_MINT),
        USDC_MINT,
        PYUSD_MINT,
        derive_loyal_hub_authority(),
        fixture.hub_authorizer.pubkey(),
        true,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        0,
    );

    let error = send_direct_swap(&mut fixture, ix, &accounts, true)
        .expect_err("duplicate mutable token account rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_output_below_fee_cap() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let ix = valid_direct_hub_swap_ix(&fixture, &accounts, AMOUNT_IN, 900_000, 900_000, 10, 0);

    let error = send_direct_swap(&mut fixture, ix, &accounts, true)
        .expect_err("output below normalized fee cap rejects");
    assert!(error.contains("InvalidArgument"), "{error}");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_insufficient_user_input_balance() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN - 1);
    let before = swap_balances(&fixture, &accounts);
    let ix = valid_direct_hub_swap_ix(
        &fixture,
        &accounts,
        AMOUNT_IN,
        HUB_OUT,
        MIN_OUT,
        MAX_FEE_BPS,
        0,
    );

    send_direct_swap(&mut fixture, ix, &accounts, true)
        .expect_err("insufficient user input balance rejects");
    assert_balances_unchanged(&fixture, &accounts, &before);
}

#[test]
fn qed_swap_rejects_insufficient_hub_output_inventory() {
    let Some(mut fixture) = setup_or_skip() else {
        return;
    };
    let accounts = seed_direct_swap_accounts(&mut fixture, AMOUNT_IN);
    let before = swap_balances(&fixture, &accounts);
    let ix = valid_direct_hub_swap_ix(
        &fixture,
        &accounts,
        AMOUNT_IN,
        AMOUNT_IN * 3,
        0,
        MAX_FEE_BPS,
        0,
    );

    send_direct_swap(&mut fixture, ix, &accounts, true)
        .expect_err("insufficient hub output inventory rejects");
    assert_balances_unchanged(&fixture, &accounts, &before);
}
