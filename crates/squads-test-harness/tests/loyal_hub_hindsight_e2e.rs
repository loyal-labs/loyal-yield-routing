use loyal_actions::{create_three_step_yield_route_actions, SwapLane};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
};
use squads_test_harness::{
    create_funded_squads_test_context_with_config_and_mock_programs,
    create_squads_smart_account_instruction, derive_loyal_hub_authority, derive_loyal_hub_config,
    derive_mock_jupiter_swap_authority, derive_squads_pool, derive_squads_vault,
    execute_mock_jupiter_sol_to_usdc_swap_instruction, execute_squads_sync_transaction_instruction,
    get_spl_token_amount, initialize_loyal_hub_config_instruction, loyal_action_context,
    loyal_hub_token_account, loyal_hub_withdraw_inventory_data,
    mock_jupiter_stable_exact_in_swap_data, mock_jupiter_stable_reserve_token_account,
    mock_jupiter_swap_lane, mock_kamino_deposit_reserve_liquidity_data,
    mock_kamino_reserve_transaction, mock_kamino_withdraw_reserve_liquidity_data,
    seed_mock_jupiter_spl_accounts, seed_mock_jupiter_stable_reserve_spl_accounts,
    seed_mock_kamino_reserve_spl_accounts_with_mint, seed_spl_mint_if_missing,
    seed_spl_token_account, set_spl_mint_supply, set_spl_token_amount, try_send_instructions,
    yield_route_universe_from_mock_reserves, FundedSquadsTestConfig, HubAction, HubSwapExecution,
    KaminoAction, MockJupiterStableReserveTokenAccount, MockKaminoReserveTokenAccounts,
    MockProgram, RouteActionExt, SquadsCompiledInstruction, SquadsPool, JUPITER_V6_PROGRAM_ID,
    KAMINO_PRIME_MARKET, KAMINO_PRIME_USDC_RESERVE, LAMPORTS_PER_SOL, LOYAL_HUB_SWAP_PROGRAM_ID,
    USDC_DECIMALS, USDC_MINT,
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
const TREASURY_SEED: u128 = 2;
const HUB_MAX_FEE_BPS: u16 = 100;
const DISCOUNTED_FEE_SHARE_OF_JUPITER: f64 = 0.5;
const DISCOUNTED_FEE_CAP_BPS: f64 = 5.0;
const JUPITER_LIKE_FEE_CAP_BPS: f64 = HUB_MAX_FEE_BPS as f64;
const JUPITER_LIKE_MAX_APY_DRIFT: f64 = 0.003;
const JUPITER_LIKE_FEE_SHARES: [f64; 4] = [1.0, 1.1, 1.25, 1.35];
const SIX_HOUR_MEAN_WINDOW_HOURS: usize = 6;
const TREASURY_STARTING_VALUE_USD_PER_MINT: f64 = 10_000_000.0;
const HUB_TARGET_VALUE_USD_PER_MINT: f64 = 250_000.0;

#[test]
#[ignore = "heavy optional historical replay; run with `cargo test -p squads-test-harness --test loyal_hub_hindsight_e2e -- --ignored --nocapture`"]
fn loyal_hub_hindsight_estimates_user_apy_treasury_drag_and_fee_revenue() {
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
    let jupiter_route = simulate_fixed_start_hindsight(&backtest, &analysis.jupiter_costs);
    let zero_fee_model = HubPricing::ZeroFee;
    let discounted_fee_model = HubPricing::Discounted {
        share_of_jupiter: DISCOUNTED_FEE_SHARE_OF_JUPITER,
        cap_bps: DISCOUNTED_FEE_CAP_BPS,
    };
    let zero_fee_route =
        simulate_hub_hindsight(&backtest, &analysis.jupiter_costs, &zero_fee_model);
    let discounted_fee_route =
        simulate_hub_hindsight(&backtest, &analysis.jupiter_costs, &discounted_fee_model);

    assert!(
        zero_fee_route.ending_value_usd > jupiter_route.ending_value_usd,
        "zero-fee hub route should improve user net value over Jupiter-only route"
    );
    assert!(
        discounted_fee_route.ending_value_usd > jupiter_route.ending_value_usd,
        "discounted hub fees should still beat Jupiter-only route for the user"
    );

    let zero_fee_report = run_litesvm_hub_route(
        &backtest,
        &analysis.jupiter_costs,
        &zero_fee_route,
        &zero_fee_model,
    );
    let discounted_fee_report = run_litesvm_hub_route(
        &backtest,
        &analysis.jupiter_costs,
        &discounted_fee_route,
        &discounted_fee_model,
    );
    if zero_fee_report.skipped || discounted_fee_report.skipped {
        return;
    }

    let years = elapsed_years(
        &backtest
            .hourly_choices
            .first()
            .expect("hourly choices")
            .timestamp,
        &backtest.end_timestamp,
    );
    let jupiter_apy = annualized_apy(jupiter_route.ending_value_usd, years);
    let zero_fee_user_apy = annualized_apy(zero_fee_report.user_net_value_usd, years);
    let discounted_user_apy = annualized_apy(discounted_fee_report.user_net_value_usd, years);

    assert_close(
        zero_fee_report.user_net_value_usd,
        zero_fee_route.ending_value_usd,
        5.0,
        "zero-fee LiteSVM route should match the DP model after measured route fees",
    );
    assert_close(
        discounted_fee_report.user_net_value_usd,
        discounted_fee_route.ending_value_usd,
        5.0,
        "discounted-fee LiteSVM route should match the DP model after measured route fees",
    );
    assert!(
        zero_fee_report.treasury_rebalance_loss_usd > 0.0,
        "zero-fee hub fills should leave treasury with Jupiter rebalance drag"
    );
    assert!(
        discounted_fee_report.hub_fee_revenue_usd > 0.0,
        "discounted hub fee scenario should collect non-zero fees"
    );
    assert!(
        discounted_fee_report.hub_fee_revenue_usd
            < discounted_fee_report.equivalent_jupiter_user_loss_usd,
        "discounted hub fees should remain below the Jupiter swap drag users avoided"
    );
    assert!(
        discounted_fee_report.treasury_rebalance_loss_usd
            < zero_fee_report.treasury_rebalance_loss_usd,
        "discounted hub fees should offset some treasury rebalance loss"
    );

    println!(
        "\nLoyal Hub hindsight over {} -> {} on ${:.0} start",
        analysis.assumptions.requested_start,
        analysis.assumptions.requested_end,
        STARTING_VALUE_USD
    );
    println!(
        "Jupiter-only route: {} switches, ending ${:.4}, annualized APY {:.2}%",
        jupiter_route.path.len().saturating_sub(1),
        jupiter_route.ending_value_usd,
        jupiter_apy * 100.0
    );
    println!(
        "Zero-fee hub route: {} switches ({} cross-mint), user gross ${:.4}, user route fees ${:.4}, user net ${:.4}, APY {:.2}%, treasury rebalance loss ${:.4}, treasury tx fees ${:.4}",
        zero_fee_route.path.len().saturating_sub(1),
        zero_fee_report.cross_mint_rebalances,
        zero_fee_report.user_gross_value_usd,
        zero_fee_report.route_tx_fees_usd,
        zero_fee_report.user_net_value_usd,
        zero_fee_user_apy * 100.0,
        zero_fee_report.treasury_rebalance_loss_usd,
        zero_fee_report.treasury_rebalance_tx_fees_usd
    );
    println!(
        "Discounted-fee hub route: {} switches ({} cross-mint), user gross ${:.4}, user route fees ${:.4}, user net ${:.4}, APY {:.2}%, collected fees ${:.4}, treasury rebalance loss ${:.4}, treasury net after tx ${:.4}",
        discounted_fee_route.path.len().saturating_sub(1),
        discounted_fee_report.cross_mint_rebalances,
        discounted_fee_report.user_gross_value_usd,
        discounted_fee_report.route_tx_fees_usd,
        discounted_fee_report.user_net_value_usd,
        discounted_user_apy * 100.0,
        discounted_fee_report.hub_fee_revenue_usd,
        discounted_fee_report.treasury_rebalance_loss_usd,
        discounted_fee_report.treasury_net_after_fees_usd
    );
}

#[test]
#[ignore = "heavy optional historical replay; run with `cargo test -p squads-test-harness --test loyal_hub_hindsight_e2e -- --ignored --nocapture`"]
fn loyal_hub_hindsight_finds_max_jupiter_like_fee_revenue() {
    let analysis = load_analysis();
    let history = load_history_cache();
    let backtest = build_backtest(&history);
    let jupiter_route = simulate_fixed_start_hindsight(&backtest, &analysis.jupiter_costs);
    let years = elapsed_years(
        &backtest
            .hourly_choices
            .first()
            .expect("hourly choices")
            .timestamp,
        &backtest.end_timestamp,
    );
    let jupiter_apy = annualized_apy(jupiter_route.ending_value_usd, years);
    let target_min_apy = jupiter_apy - JUPITER_LIKE_MAX_APY_DRIFT;
    let (candidate, candidates) =
        find_max_jupiter_like_fee_model(&backtest, &analysis.jupiter_costs, years, target_min_apy);
    let report = run_litesvm_hub_route(
        &backtest,
        &analysis.jupiter_costs,
        &candidate.route,
        &candidate.pricing,
    );
    if report.skipped {
        return;
    }

    let user_apy = annualized_apy(report.user_net_value_usd, years);
    assert!(
        user_apy >= target_min_apy,
        "max Jupiter-like fee candidate should stay within {:.2}% APY of Jupiter: user {:.4}%, Jupiter {:.4}%",
        JUPITER_LIKE_MAX_APY_DRIFT * 100.0,
        user_apy * 100.0,
        jupiter_apy * 100.0
    );
    assert!(
        report.hub_fee_revenue_usd > 0.0,
        "Jupiter-like fee model should collect hub revenue"
    );
    assert!(
        report.hub_fee_revenue_usd <= report.equivalent_jupiter_user_loss_usd * 1.5,
        "Jupiter-like fees should stay in the same rough band as equivalent Jupiter user drag"
    );

    println!(
        "\nMax Jupiter-like Loyal Hub fee search over {} -> {} on ${:.0} start",
        analysis.assumptions.requested_start,
        analysis.assumptions.requested_end,
        STARTING_VALUE_USD
    );
    println!(
        "Jupiter baseline: ending ${:.4}, APY {:.2}%",
        jupiter_route.ending_value_usd,
        jupiter_apy * 100.0
    );
    println!("Modeled quick candidates:");
    for candidate in &candidates {
        println!(
            "  {:.0}% of Jupiter drag -> ending ${:.4}, APY {:.2}%",
            candidate.share_of_jupiter * 100.0,
            candidate.route.ending_value_usd,
            candidate.modeled_apy * 100.0
        );
    }
    println!(
        "Selected hub fee: {:.0}% of Jupiter drag, capped at {:.1} bps; DP APY {:.2}%",
        candidate.share_of_jupiter * 100.0,
        JUPITER_LIKE_FEE_CAP_BPS,
        candidate.modeled_apy * 100.0
    );
    println!(
        "LiteSVM replay: {} switches ({} cross-mint), user gross ${:.4}, user route fees ${:.4}, user net ${:.4}, APY {:.2}%, collected fees ${:.4}, equivalent Jupiter drag ${:.4}, treasury rebalance loss ${:.4}, treasury net after tx ${:.4}",
        candidate.route.path.len().saturating_sub(1),
        report.cross_mint_rebalances,
        report.user_gross_value_usd,
        report.route_tx_fees_usd,
        report.user_net_value_usd,
        user_apy * 100.0,
        report.hub_fee_revenue_usd,
        report.equivalent_jupiter_user_loss_usd,
        report.treasury_rebalance_loss_usd,
        report.treasury_net_after_fees_usd
    );
}

#[test]
#[ignore = "heavy optional historical replay; run with `cargo test -p squads-test-harness --test loyal_hub_hindsight_e2e -- --ignored --nocapture`"]
fn loyal_hub_hindsight_reports_200_percent_jupiter_drag_fee() {
    let analysis = load_analysis();
    let history = load_history_cache();
    let backtest = build_backtest(&history);
    let jupiter_route = simulate_fixed_start_hindsight(&backtest, &analysis.jupiter_costs);
    let years = elapsed_years(
        &backtest
            .hourly_choices
            .first()
            .expect("hourly choices")
            .timestamp,
        &backtest.end_timestamp,
    );
    let jupiter_apy = annualized_apy(jupiter_route.ending_value_usd, years);
    let pricing = HubPricing::Discounted {
        share_of_jupiter: 2.0,
        cap_bps: JUPITER_LIKE_FEE_CAP_BPS,
    };
    let route = simulate_hub_hindsight(&backtest, &analysis.jupiter_costs, &pricing);
    let modeled_apy = annualized_apy(route.ending_value_usd, years);
    let report = run_litesvm_hub_route(&backtest, &analysis.jupiter_costs, &route, &pricing);
    if report.skipped {
        return;
    }
    let user_apy = annualized_apy(report.user_net_value_usd, years);

    println!(
        "\nLoyal Hub 200% Jupiter-drag fee over {} -> {} on ${:.0} start",
        analysis.assumptions.requested_start,
        analysis.assumptions.requested_end,
        STARTING_VALUE_USD
    );
    println!(
        "Jupiter baseline: ending ${:.4}, APY {:.2}%",
        jupiter_route.ending_value_usd,
        jupiter_apy * 100.0
    );
    println!(
        "200% hub fee model: DP ending ${:.4}, DP APY {:.2}%",
        route.ending_value_usd,
        modeled_apy * 100.0
    );
    println!(
        "LiteSVM replay: {} switches ({} cross-mint), user gross ${:.4}, user route fees ${:.4}, user net ${:.4}, APY {:.2}%, collected fees ${:.4}, equivalent Jupiter drag ${:.4}, treasury rebalance loss ${:.4}, treasury net after tx ${:.4}",
        route.path.len().saturating_sub(1),
        report.cross_mint_rebalances,
        report.user_gross_value_usd,
        report.route_tx_fees_usd,
        report.user_net_value_usd,
        user_apy * 100.0,
        report.hub_fee_revenue_usd,
        report.equivalent_jupiter_user_loss_usd,
        report.treasury_rebalance_loss_usd,
        report.treasury_net_after_fees_usd
    );
}

#[test]
#[ignore = "heavy optional historical replay; run with `cargo test -p squads-test-harness --test loyal_hub_hindsight_e2e -- --ignored --nocapture`"]
fn loyal_hub_six_hour_mean_route_reports_135_and_200_percent_drag_fees() {
    let analysis = load_analysis();
    let history = load_history_cache();
    let backtest = build_backtest(&history);
    let years = elapsed_years(
        &backtest
            .hourly_choices
            .first()
            .expect("hourly choices")
            .timestamp,
        &backtest.end_timestamp,
    );

    let pricing_135 = HubPricing::Discounted {
        share_of_jupiter: 1.35,
        cap_bps: JUPITER_LIKE_FEE_CAP_BPS,
    };
    let pricing_200 = HubPricing::Discounted {
        share_of_jupiter: 2.0,
        cap_bps: JUPITER_LIKE_FEE_CAP_BPS,
    };
    let pricing_zero = HubPricing::ZeroFee;
    let pricing_30 = HubPricing::Discounted {
        share_of_jupiter: 0.3,
        cap_bps: JUPITER_LIKE_FEE_CAP_BPS,
    };
    let pricing_60 = HubPricing::Discounted {
        share_of_jupiter: 0.6,
        cap_bps: JUPITER_LIKE_FEE_CAP_BPS,
    };
    let pricing_100 = HubPricing::Discounted {
        share_of_jupiter: 1.0,
        cap_bps: JUPITER_LIKE_FEE_CAP_BPS,
    };
    let route_zero =
        simulate_trailing_six_hour_mean_route(&backtest, &analysis.jupiter_costs, &pricing_zero);
    let route_30 =
        simulate_trailing_six_hour_mean_route(&backtest, &analysis.jupiter_costs, &pricing_30);
    let route_60 =
        simulate_trailing_six_hour_mean_route(&backtest, &analysis.jupiter_costs, &pricing_60);
    let route_100 =
        simulate_trailing_six_hour_mean_route(&backtest, &analysis.jupiter_costs, &pricing_100);
    let route_135 =
        simulate_trailing_six_hour_mean_route(&backtest, &analysis.jupiter_costs, &pricing_135);
    let route_200 =
        simulate_trailing_six_hour_mean_route(&backtest, &analysis.jupiter_costs, &pricing_200);
    let report_zero = run_litesvm_hub_route(
        &backtest,
        &analysis.jupiter_costs,
        &route_zero,
        &pricing_zero,
    );
    let report_30 =
        run_litesvm_hub_route(&backtest, &analysis.jupiter_costs, &route_30, &pricing_30);
    let report_60 =
        run_litesvm_hub_route(&backtest, &analysis.jupiter_costs, &route_60, &pricing_60);
    let report_100 =
        run_litesvm_hub_route(&backtest, &analysis.jupiter_costs, &route_100, &pricing_100);
    let report_135 =
        run_litesvm_hub_route(&backtest, &analysis.jupiter_costs, &route_135, &pricing_135);
    let report_200 =
        run_litesvm_hub_route(&backtest, &analysis.jupiter_costs, &route_200, &pricing_200);
    if report_zero.skipped
        || report_30.skipped
        || report_60.skipped
        || report_100.skipped
        || report_135.skipped
        || report_200.skipped
    {
        return;
    }

    let user_apy_zero = annualized_apy(report_zero.user_net_value_usd, years);
    let user_apy_30 = annualized_apy(report_30.user_net_value_usd, years);
    let user_apy_60 = annualized_apy(report_60.user_net_value_usd, years);
    let user_apy_100 = annualized_apy(report_100.user_net_value_usd, years);
    let user_apy_135 = annualized_apy(report_135.user_net_value_usd, years);
    let user_apy_200 = annualized_apy(report_200.user_net_value_usd, years);

    println!(
        "\nTrailing 6h mean route, hourly decisions over {} -> {} on ${:.0} start",
        analysis.assumptions.requested_start,
        analysis.assumptions.requested_end,
        STARTING_VALUE_USD
    );
    println!(
        "0% route switches: {}, cross-mint fills: {}; 30% route switches: {}, cross-mint fills: {}; 60% route switches: {}, cross-mint fills: {}; 100% route switches: {}, cross-mint fills: {}; 135% route switches: {}, cross-mint fills: {}; 200% route switches: {}, cross-mint fills: {}",
        route_zero.path.len().saturating_sub(1),
        report_zero.cross_mint_rebalances,
        route_30.path.len().saturating_sub(1),
        report_30.cross_mint_rebalances,
        route_60.path.len().saturating_sub(1),
        report_60.cross_mint_rebalances,
        route_100.path.len().saturating_sub(1),
        report_100.cross_mint_rebalances,
        route_135.path.len().saturating_sub(1),
        report_135.cross_mint_rebalances,
        route_200.path.len().saturating_sub(1),
        report_200.cross_mint_rebalances
    );
    println!(
        "0% of Jupiter drag: user net ${:.4}, APY {:.2}%, collected fees ${:.4}, monthly per $1k ${:.4}, treasury net after tx ${:.4}",
        report_zero.user_net_value_usd,
        user_apy_zero * 100.0,
        report_zero.hub_fee_revenue_usd,
        monthly_fee_usd(report_zero.hub_fee_revenue_usd, years),
        report_zero.treasury_net_after_fees_usd
    );
    println!(
        "30% of Jupiter drag: user net ${:.4}, APY {:.2}%, collected fees ${:.4}, monthly per $1k ${:.4}, treasury net after tx ${:.4}",
        report_30.user_net_value_usd,
        user_apy_30 * 100.0,
        report_30.hub_fee_revenue_usd,
        monthly_fee_usd(report_30.hub_fee_revenue_usd, years),
        report_30.treasury_net_after_fees_usd
    );
    println!(
        "60% of Jupiter drag: user net ${:.4}, APY {:.2}%, collected fees ${:.4}, monthly per $1k ${:.4}, treasury net after tx ${:.4}",
        report_60.user_net_value_usd,
        user_apy_60 * 100.0,
        report_60.hub_fee_revenue_usd,
        monthly_fee_usd(report_60.hub_fee_revenue_usd, years),
        report_60.treasury_net_after_fees_usd
    );
    println!(
        "100% of Jupiter drag: user net ${:.4}, APY {:.2}%, collected fees ${:.4}, monthly per $1k ${:.4}, treasury net after tx ${:.4}",
        report_100.user_net_value_usd,
        user_apy_100 * 100.0,
        report_100.hub_fee_revenue_usd,
        monthly_fee_usd(report_100.hub_fee_revenue_usd, years),
        report_100.treasury_net_after_fees_usd
    );
    println!(
        "135% of Jupiter drag: user net ${:.4}, APY {:.2}%, collected fees ${:.4}, monthly per $1k ${:.4}, treasury net after tx ${:.4}",
        report_135.user_net_value_usd,
        user_apy_135 * 100.0,
        report_135.hub_fee_revenue_usd,
        monthly_fee_usd(report_135.hub_fee_revenue_usd, years),
        report_135.treasury_net_after_fees_usd
    );
    println!(
        "200% of Jupiter drag: user net ${:.4}, APY {:.2}%, collected fees ${:.4}, monthly per $1k ${:.4}, treasury net after tx ${:.4}",
        report_200.user_net_value_usd,
        user_apy_200 * 100.0,
        report_200.hub_fee_revenue_usd,
        monthly_fee_usd(report_200.hub_fee_revenue_usd, years),
        report_200.treasury_net_after_fees_usd
    );
}

#[test]
#[ignore = "heavy optional historical replay; run with `cargo test -p squads-test-harness --test loyal_hub_hindsight_e2e -- --ignored --nocapture`"]
fn loyal_hub_six_hour_mean_route_scans_positive_fee_drag() {
    let analysis = load_analysis();
    let history = load_history_cache();
    let backtest = build_backtest(&history);
    let years = elapsed_years(
        &backtest
            .hourly_choices
            .first()
            .expect("hourly choices")
            .timestamp,
        &backtest.end_timestamp,
    );
    let shares = [0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60];
    let mut rows = Vec::new();

    for share in shares {
        let pricing = HubPricing::Discounted {
            share_of_jupiter: share,
            cap_bps: JUPITER_LIKE_FEE_CAP_BPS,
        };
        let route =
            simulate_trailing_six_hour_mean_route(&backtest, &analysis.jupiter_costs, &pricing);
        let report = run_litesvm_hub_route(&backtest, &analysis.jupiter_costs, &route, &pricing);
        if report.skipped {
            return;
        }
        rows.push((
            share,
            route.path.len().saturating_sub(1),
            report.cross_mint_rebalances,
            report.user_net_value_usd,
            annualized_apy(report.user_net_value_usd, years),
            report.hub_fee_revenue_usd,
            monthly_fee_usd(report.hub_fee_revenue_usd, years),
            report.treasury_net_after_fees_usd,
        ));
    }

    let best_positive = rows
        .iter()
        .filter(|row| row.2 > 0 && row.7 > 0.0)
        .max_by(|a, b| a.0.total_cmp(&b.0));

    println!(
        "\nTrailing 6h mean route fee scan, hourly fee-aware decisions over {} -> {} on ${:.0} start",
        analysis.assumptions.requested_start,
        analysis.assumptions.requested_end,
        STARTING_VALUE_USD
    );
    println!(
        "share,switches,cross_mint,user_net,user_apy,fees,monthly_per_1k,treasury_net_after_tx"
    );
    for row in &rows {
        println!(
            "{:.0}%,{},{},${:.4},{:.2}%,${:.4},${:.4},${:.4}",
            row.0 * 100.0,
            row.1,
            row.2,
            row.3,
            row.4 * 100.0,
            row.5,
            row.6,
            row.7
        );
    }
    match best_positive {
        Some(row) => println!(
            "max_positive_share={:.0}%, fees=${:.4}, monthly_per_1k=${:.4}, treasury_net_after_tx=${:.4}, user_apy={:.2}%",
            row.0 * 100.0,
            row.5,
            row.6,
            row.7,
            row.4 * 100.0
        ),
        None => println!("max_positive_share=none"),
    }
}

include!("loyal_hub_hindsight_e2e/litesvm_support.rs");
include!("loyal_hub_hindsight_e2e/simulation_support.rs");
include!("loyal_hub_hindsight_e2e/data_support.rs");
