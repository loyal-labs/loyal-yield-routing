use solana_sdk::{pubkey::Pubkey, signature::Keypair, signer::Signer};
use squads_test_harness::{
    create_funded_squads_test_context_with_config_and_mock_programs,
    create_squads_yield_route_policy_instructions,
    execute_mock_jupiter_sol_to_usdc_swap_instruction,
    execute_squads_program_interaction_instruction, execute_squads_sync_transaction_instruction,
    execute_squads_yield_route_stable_swap_instruction, get_spl_token_amount,
    mock_jupiter_stable_reserve_token_account, mock_kamino_deposit_reserve_liquidity_data,
    mock_kamino_reserve_transaction, mock_kamino_withdraw_reserve_liquidity_data,
    seed_mock_jupiter_spl_accounts, seed_mock_jupiter_stable_reserve_spl_accounts,
    seed_mock_kamino_reserve_spl_accounts_with_mint, seed_spl_mint_if_missing, set_spl_mint_supply,
    set_spl_token_amount, try_send_instructions, FundedSquadsTestConfig,
    MockJupiterStableReserveTokenAccount, MockKaminoReserveTokenAccounts, MockProgram,
    SquadsYieldRoutePolicyWhitelist, KAMINO_PRIME_MARKET, KAMINO_PRIME_USDC_RESERVE,
    LAMPORTS_PER_SOL, USDC_DECIMALS, USDC_MINT,
};
use std::{
    collections::{BTreeMap, HashMap, HashSet},
    fs,
    path::{Path, PathBuf},
    str::FromStr,
};

const ANALYSIS_PATH: &str = "data/kamino-hourly-reserve-analysis.json";
const HISTORY_CACHE_PATH: &str = "data/kamino-hourly-reserve-history-cache.json";
const STARTING_VALUE_USD: f64 = 1_000.0;
const TVL_FLOOR_USD: f64 = 100_000.0;
const APY_CAP: f64 = 0.5;
const POOL_CHANGE_LAMPORTS: u64 = 5_000;
const SOL_PRICE_USD: f64 = 84.82;
const POOL_CHANGE_USD: f64 =
    (POOL_CHANGE_LAMPORTS as f64 / LAMPORTS_PER_SOL as f64) * SOL_PRICE_USD;
const JUPITER_RESERVE_RAW_AMOUNT: u64 = 1_000_000_000_000_000_000;

#[test]
#[ignore = "heavy optional historical replay; run with `bun run test:squads:e2e`"]
fn wallet_b_replays_fixed_start_kamino_hindsight_route() {
    let analysis = load_analysis();
    let history = load_history_cache();
    assert_eq!(analysis.assumptions.requested_start, "2026-03-01");
    assert_eq!(analysis.assumptions.requested_end, "2026-05-19");
    assert_eq!(analysis.assumptions.frequency, "hour");
    assert_eq!(
        analysis.assumptions.pool_change_lamports,
        POOL_CHANGE_LAMPORTS
    );

    let backtest = build_backtest(&history);
    let route = simulate_fixed_start_hindsight(&backtest, &analysis.jupiter_costs);
    assert!(route.path.len() > 1, "hindsight route should rebalance");
    assert_eq!(route.path[0].point.market_address, KAMINO_PRIME_MARKET);
    assert_eq!(
        route.path[0].point.reserve_address,
        KAMINO_PRIME_USDC_RESERVE
    );

    let mut context = create_funded_squads_test_context_with_config_and_mock_programs(
        FundedSquadsTestConfig {
            smart_account_seed: 1,
            vault_index: 0,
            wallet_airdrop_lamports: 5 * LAMPORTS_PER_SOL,
            vault_funding_lamports: 2 * LAMPORTS_PER_SOL,
        },
        &[MockProgram::Jupiter, MockProgram::KaminoLend],
    )
    .expect("create funded Squads test context");
    let Some(context) = context.as_mut() else {
        eprintln!("skipping historical Squads policy E2E; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let wallet_b = Keypair::new();
    context
        .svm
        .airdrop(&wallet_b.pubkey(), LAMPORTS_PER_SOL)
        .expect("airdrop wallet B");

    let route_reserve_indices = route
        .path
        .iter()
        .map(|step| step.point.reserve_index)
        .collect::<HashSet<_>>();
    let route_mint_addresses = route
        .path
        .iter()
        .map(|step| step.point.mint_address)
        .collect::<HashSet<_>>();
    let route_mints = route_mint_addresses.iter().copied().collect::<Vec<_>>();

    let vault_token_accounts = route_mint_addresses
        .iter()
        .map(|mint| (*mint, Keypair::new().pubkey()))
        .collect::<HashMap<_, _>>();

    seed_mock_jupiter_spl_accounts(
        &mut context.svm,
        JUPITER_RESERVE_RAW_AMOUNT,
        JUPITER_RESERVE_RAW_AMOUNT,
    );
    for mint in &route_mint_addresses {
        let decimals = route
            .path
            .iter()
            .find(|step| step.point.mint_address == *mint)
            .map(|step| step.point.decimals)
            .unwrap_or(6);
        seed_spl_mint_if_missing(
            &mut context.svm,
            *mint,
            None,
            decimals,
            JUPITER_RESERVE_RAW_AMOUNT,
        );
    }
    let jupiter_stable_reserves = route_mint_addresses
        .iter()
        .map(|mint| MockJupiterStableReserveTokenAccount {
            mint: *mint,
            reserve: mock_jupiter_stable_reserve_token_account(*mint),
        })
        .collect::<Vec<_>>();
    seed_mock_jupiter_stable_reserve_spl_accounts(
        &mut context.svm,
        &jupiter_stable_reserves,
        JUPITER_RESERVE_RAW_AMOUNT,
    );

    let mut reserve_accounts = HashMap::<usize, MockKaminoReserveTokenAccounts>::new();
    for reserve_index in route_reserve_indices {
        let reserve = &backtest.reserves[reserve_index];
        let vault_liquidity = vault_token_accounts[&reserve.mint_address];
        let accounts = seed_mock_kamino_reserve_spl_accounts_with_mint(
            &mut context.svm,
            reserve.reserve_address,
            reserve.market_address,
            reserve.mint_address,
            reserve.decimals,
            context.vault,
            vault_liquidity,
            Keypair::new().pubkey(),
            Keypair::new().pubkey(),
        );
        reserve_accounts.insert(reserve_index, accounts);
    }

    let route_reserve_accounts = reserve_accounts.values().copied().collect::<Vec<_>>();
    let route_policy_setup = create_squads_yield_route_policy_instructions(
        context,
        wallet_b.pubkey(),
        SquadsYieldRoutePolicyWhitelist {
            stable_mints: route_mints,
            kamino_reserves: route_reserve_accounts,
        },
    );
    let route_policies = route_policy_setup.policies;
    try_send_instructions(
        &mut context.svm,
        &route_policy_setup.instructions,
        &context.wallet,
        &[],
    )
    .expect("wallet A creates optimized-reserve withdraw, route-mint swap, and deposit policies");

    let first = &route.path[0].point;
    let mut current = first.clone();
    let mut amount_raw = raw_from_usd(STARTING_VALUE_USD, first);
    assert_eq!(first.mint_address, USDC_MINT);
    assert_eq!(first.decimals, USDC_DECIMALS);

    let wallet_a_before_setup = context.wallet_balance();
    let wallet_b_before_route = context
        .svm
        .get_account(&wallet_b.pubkey())
        .expect("wallet B account")
        .lamports;

    let wallet_a_sol_to_usdc_ix = execute_mock_jupiter_sol_to_usdc_swap_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        context.vault_index,
        context.vault,
        vault_token_accounts[&USDC_MINT],
        Keypair::new().pubkey(),
        amount_raw,
    );
    try_send_instructions(
        &mut context.svm,
        &[wallet_a_sol_to_usdc_ix],
        &context.wallet,
        &[],
    )
    .expect("wallet A swaps SOL to the starting USDC balance");

    let current_accounts = reserve_accounts[&current.reserve_index];
    let (deposit_instructions, deposit_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        current_accounts,
        mock_kamino_deposit_reserve_liquidity_data(amount_raw),
    );
    let deposit_ix = execute_squads_sync_transaction_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        context.vault_index,
        deposit_instructions,
        deposit_accounts,
    );
    try_send_instructions(&mut context.svm, &[deposit_ix], &context.wallet, &[])
        .expect("wallet A deposits into the starting USDC Prime reserve");
    assert_route_state(
        &context.svm,
        &reserve_accounts,
        current.reserve_index,
        amount_raw,
    );

    for next in route.path.iter().skip(1) {
        amount_raw = accrue_segment_raw(&backtest, &current, amount_raw, &next.timestamp);
        apply_mock_kamino_accrual(
            &mut context.svm,
            reserve_accounts[&current.reserve_index],
            amount_raw,
        );

        let from_at_switch = backtest
            .point_at(current.reserve_index, &next.timestamp)
            .unwrap_or(&current);
        let (transaction_instructions, next_amount_raw) = build_rebalance_transaction(
            context.vault,
            route_policies.withdraw,
            route_policies.swap,
            route_policies.deposit,
            wallet_b.pubkey(),
            context.vault_index,
            &vault_token_accounts,
            &reserve_accounts,
            &analysis.jupiter_costs,
            from_at_switch,
            &next.point,
            amount_raw,
        );
        try_send_instructions(&mut context.svm, &transaction_instructions, &wallet_b, &[])
            .unwrap_or_else(|error| {
                panic!(
                    "wallet B executes hindsight route at {}: {error:?}",
                    next.timestamp
                )
            });

        current = next.point.clone();
        amount_raw = next_amount_raw;
        assert_route_state(
            &context.svm,
            &reserve_accounts,
            current.reserve_index,
            amount_raw,
        );
    }

    amount_raw = accrue_segment_raw(&backtest, &current, amount_raw, &backtest.end_timestamp);
    apply_mock_kamino_accrual(
        &mut context.svm,
        reserve_accounts[&current.reserve_index],
        amount_raw,
    );

    let final_accounts = reserve_accounts[&current.reserve_index];
    let (withdraw_instructions, withdraw_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        final_accounts,
        mock_kamino_withdraw_reserve_liquidity_data(amount_raw),
    );
    let final_withdraw_ix = execute_squads_sync_transaction_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        context.vault_index,
        withdraw_instructions,
        withdraw_accounts,
    );
    try_send_instructions(&mut context.svm, &[final_withdraw_ix], &context.wallet, &[])
        .expect("wallet A withdraws from the final Kamino reserve");

    assert_eq!(
        get_spl_token_amount(&context.svm, final_accounts.vault_collateral),
        0
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_token_accounts[&current.mint_address]),
        amount_raw
    );

    let wallet_b_after_route = context
        .svm
        .get_account(&wallet_b.pubkey())
        .expect("wallet B account")
        .lamports;
    let route_transaction_count = (route.path.len() - 1) as u64;
    assert_eq!(
        wallet_b_before_route - wallet_b_after_route,
        route_transaction_count * POOL_CHANGE_LAMPORTS
    );

    let wallet_a_after_finish = context.wallet_balance();
    let wallet_a_transaction_count = 3;
    assert_eq!(
        wallet_a_before_setup - wallet_a_after_finish,
        wallet_a_transaction_count * POOL_CHANGE_LAMPORTS
    );

    let final_gross_value = usd_value(amount_raw, &current);
    let route_signature_fees_usd = route_transaction_count as f64 * POOL_CHANGE_USD;
    let final_net_value = final_gross_value - route_signature_fees_usd;
    assert!(
        final_net_value >= route.ending_value_usd - 0.25,
        "final net value {final_net_value} should track fixed-start hindsight {}",
        route.ending_value_usd
    );
}

include!("kamino_hindsight_e2e/support.rs");
