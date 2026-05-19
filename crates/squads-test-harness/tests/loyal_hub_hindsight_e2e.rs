use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
};
use squads_test_harness::{
    create_funded_squads_test_context_with_config_and_mock_programs,
    create_squads_smart_account_instruction,
    create_squads_yield_route_policy_instructions_with_swap_lanes, derive_loyal_hub_authority,
    derive_loyal_hub_config, derive_mock_jupiter_swap_authority, derive_squads_pool,
    derive_squads_vault, execute_mock_jupiter_sol_to_usdc_swap_instruction,
    execute_squads_program_interaction_instruction, execute_squads_sync_transaction_instruction,
    execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index,
    get_spl_token_amount, initialize_loyal_hub_config_instruction, loyal_hub_token_account,
    loyal_hub_withdraw_inventory_data, mock_jupiter_stable_exact_in_swap_data,
    mock_jupiter_stable_reserve_token_account, mock_kamino_deposit_reserve_liquidity_data,
    mock_kamino_reserve_transaction, mock_kamino_withdraw_reserve_liquidity_data,
    seed_mock_jupiter_spl_accounts, seed_mock_jupiter_stable_reserve_spl_accounts,
    seed_mock_kamino_reserve_spl_accounts_with_mint, seed_spl_mint_if_missing,
    seed_spl_token_account, set_spl_mint_supply, set_spl_token_amount, try_send_instructions,
    FundedSquadsTestConfig, MockJupiterStableReserveTokenAccount, MockKaminoReserveTokenAccounts,
    MockProgram, SquadsCompiledInstruction, SquadsPool, SquadsYieldRoutePolicyWhitelist, SwapLane,
    JUPITER_V6_PROGRAM_ID, KAMINO_PRIME_MARKET, KAMINO_PRIME_USDC_RESERVE, LAMPORTS_PER_SOL,
    LOYAL_HUB_SWAP_PROGRAM_ID, USDC_DECIMALS, USDC_MINT,
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

fn run_litesvm_hub_route(
    backtest: &Backtest,
    jupiter_costs: &HashMap<String, JupiterCost>,
    route: &HindsightRoute,
    pricing: &HubPricing,
) -> HubRouteReport {
    assert!(route.path.len() > 1, "hindsight route should rebalance");
    assert_eq!(route.path[0].point.market_address, KAMINO_PRIME_MARKET);
    assert_eq!(
        route.path[0].point.reserve_address,
        KAMINO_PRIME_USDC_RESERVE
    );
    assert_eq!(route.path[0].point.mint_address, USDC_MINT);
    assert_eq!(route.path[0].point.decimals, USDC_DECIMALS);

    let mut context = create_funded_squads_test_context_with_config_and_mock_programs(
        FundedSquadsTestConfig {
            smart_account_seed: 1,
            vault_index: 0,
            wallet_airdrop_lamports: 5 * LAMPORTS_PER_SOL,
            vault_funding_lamports: 2 * LAMPORTS_PER_SOL,
        },
        &[
            MockProgram::Jupiter,
            MockProgram::KaminoLend,
            MockProgram::LoyalHubSwap,
        ],
    )
    .expect("create funded Squads test context");
    let Some(context) = context.as_mut() else {
        eprintln!("skipping historical Loyal Hub E2E; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return HubRouteReport::skipped();
    };

    let wallet_b = Keypair::new();
    let treasury_executor = Keypair::new();
    context
        .svm
        .airdrop(&wallet_b.pubkey(), LAMPORTS_PER_SOL)
        .expect("airdrop wallet B");
    context
        .svm
        .airdrop(&treasury_executor.pubkey(), LAMPORTS_PER_SOL)
        .expect("airdrop treasury executor");

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
    assert!(
        route_mint_addresses.len() <= 8,
        "Loyal Hub config helper supports up to 8 mints; route used {}",
        route_mint_addresses.len()
    );
    let route_mints = route_mint_addresses.iter().copied().collect::<Vec<_>>();
    let metadata_by_mint = route
        .path
        .iter()
        .map(|step| (step.point.mint_address, step.point.clone()))
        .collect::<HashMap<_, _>>();

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
        let point = &metadata_by_mint[mint];
        seed_spl_mint_if_missing(
            &mut context.svm,
            *mint,
            None,
            point.decimals,
            JUPITER_RESERVE_RAW_AMOUNT,
        );
        seed_spl_token_account(
            &mut context.svm,
            vault_token_accounts[mint],
            *mint,
            context.vault,
            0,
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

    let treasury = create_treasury_squads(
        context,
        treasury_executor.pubkey(),
        &route_mints,
        &metadata_by_mint,
    );
    seed_hub_inventory(context, &route_mints, &metadata_by_mint);
    let initial_treasury_value = treasury_token_value_usd(context, &treasury, &metadata_by_mint);
    let initial_hub_value = hub_inventory_value_usd(context, &route_mints, &metadata_by_mint);

    let init_hub_ix =
        treasury_initialize_hub_ix(&treasury, treasury_executor.pubkey(), &route_mints);
    try_send_instructions(&mut context.svm, &[init_hub_ix], &treasury_executor, &[])
        .expect("Loyal Treasury initializes Loyal Hub config");

    let route_reserve_accounts = reserve_accounts.values().copied().collect::<Vec<_>>();
    let route_policy_setup = create_squads_yield_route_policy_instructions_with_swap_lanes(
        context,
        wallet_b.pubkey(),
        SquadsYieldRoutePolicyWhitelist {
            stable_mints: route_mints.clone(),
            kamino_reserves: route_reserve_accounts,
        },
        vec![
            SwapLane::Jupiter,
            SwapLane::LoyalHub {
                hub_authorizer: treasury_executor.pubkey(),
                max_fee_bps: HUB_MAX_FEE_BPS,
            },
        ],
    );
    let route_policies = route_policy_setup.policies;
    try_send_instructions(
        &mut context.svm,
        &route_policy_setup.instructions,
        &context.wallet,
        &[],
    )
    .expect("wallet A creates route policies with Jupiter and Loyal Hub swap lanes");

    let first = &route.path[0].point;
    let mut current = first.clone();
    let mut amount_raw = raw_from_usd(STARTING_VALUE_USD, first);

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

    let wallet_b_before_route = context
        .svm
        .get_account(&wallet_b.pubkey())
        .expect("wallet B account")
        .lamports;
    let treasury_before_route_lamports = context
        .svm
        .get_account(&treasury_executor.pubkey())
        .expect("treasury executor account")
        .lamports;

    let mut hub_fee_revenue_usd = 0.0;
    let mut equivalent_jupiter_user_loss_usd = 0.0;
    let mut cross_mint_rebalances = 0_u64;

    for next in route.path.iter().skip(1) {
        amount_raw = accrue_segment_raw(backtest, &current, amount_raw, &next.timestamp);
        apply_mock_kamino_accrual(
            &mut context.svm,
            reserve_accounts[&current.reserve_index],
            amount_raw,
        );

        let from_at_switch = backtest
            .point_at(current.reserve_index, &next.timestamp)
            .unwrap_or(&current);
        let transition = build_hub_rebalance_transaction(
            context.vault,
            route_policies.withdraw,
            route_policies.swap,
            route_policies.deposit,
            wallet_b.pubkey(),
            treasury_executor.pubkey(),
            context.vault_index,
            &vault_token_accounts,
            &reserve_accounts,
            jupiter_costs,
            pricing,
            from_at_switch,
            &next.point,
            amount_raw,
        );
        try_send_instructions(
            &mut context.svm,
            &transition.route_instructions,
            &wallet_b,
            transition
                .needs_hub_authorizer
                .then_some(&treasury_executor)
                .into_iter()
                .collect::<Vec<_>>()
                .as_slice(),
        )
        .unwrap_or_else(|error| {
            panic!(
                "wallet B executes Loyal Hub hindsight route at {}: {error:?}",
                next.timestamp
            )
        });

        if let Some(treasury_rebalance_ix) = transition.treasury_rebalance_instruction {
            try_send_instructions(
                &mut context.svm,
                &[treasury_rebalance_ix],
                &treasury_executor,
                &[],
            )
            .unwrap_or_else(|error| {
                panic!(
                    "treasury rebalances Loyal Hub inventory at {}: {error:?}",
                    next.timestamp
                )
            });
            cross_mint_rebalances += 1;
            hub_fee_revenue_usd += transition.hub_fee_revenue_usd;
            equivalent_jupiter_user_loss_usd += transition.equivalent_jupiter_user_loss_usd;
        }

        current = next.point.clone();
        amount_raw = transition.next_amount_raw;
        assert_route_state(
            &context.svm,
            &reserve_accounts,
            current.reserve_index,
            amount_raw,
        );
        assert_close(
            hub_inventory_value_usd(context, &route_mints, &metadata_by_mint),
            initial_hub_value,
            0.02,
            "treasury rebalance should restore hub inventory target after each fill",
        );
    }

    amount_raw = accrue_segment_raw(backtest, &current, amount_raw, &backtest.end_timestamp);
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

    let final_gross_value = usd_value(amount_raw, &current);
    let wallet_b_after_route = context
        .svm
        .get_account(&wallet_b.pubkey())
        .expect("wallet B account")
        .lamports;
    let treasury_after_route_lamports = context
        .svm
        .get_account(&treasury_executor.pubkey())
        .expect("treasury executor account")
        .lamports;
    let route_tx_fees_usd = lamports_to_usd(wallet_b_before_route - wallet_b_after_route);
    let treasury_rebalance_tx_fees_usd =
        lamports_to_usd(treasury_before_route_lamports - treasury_after_route_lamports);
    let final_treasury_value = treasury_token_value_usd(context, &treasury, &metadata_by_mint);
    let treasury_rebalance_loss_usd = initial_treasury_value - final_treasury_value;

    HubRouteReport {
        skipped: false,
        user_gross_value_usd: final_gross_value,
        user_net_value_usd: final_gross_value - route_tx_fees_usd,
        route_tx_fees_usd,
        treasury_rebalance_loss_usd,
        treasury_rebalance_tx_fees_usd,
        treasury_net_after_fees_usd: hub_fee_revenue_usd
            - treasury_rebalance_loss_usd
            - treasury_rebalance_tx_fees_usd,
        hub_fee_revenue_usd,
        equivalent_jupiter_user_loss_usd,
        cross_mint_rebalances,
    }
}

fn build_hub_rebalance_transaction(
    vault: Pubkey,
    withdraw_policy: Pubkey,
    swap_policy: Pubkey,
    deposit_policy: Pubkey,
    signer: Pubkey,
    hub_authorizer: Pubkey,
    vault_index: u8,
    vault_token_accounts: &HashMap<Pubkey, Pubkey>,
    reserve_accounts: &HashMap<usize, MockKaminoReserveTokenAccounts>,
    jupiter_costs: &HashMap<String, JupiterCost>,
    pricing: &HubPricing,
    from: &Choice,
    to: &Choice,
    in_amount_raw: u64,
) -> HubTransition {
    let from_accounts = reserve_accounts[&from.reserve_index];
    let to_accounts = reserve_accounts[&to.reserve_index];
    let (withdraw_instructions, withdraw_accounts) = mock_kamino_reserve_transaction(
        vault,
        from_accounts,
        mock_kamino_withdraw_reserve_liquidity_data(in_amount_raw),
    );
    let withdraw_ix = execute_squads_program_interaction_instruction(
        withdraw_policy,
        signer,
        vault_index,
        withdraw_instructions,
        vec![0],
        withdraw_accounts,
    );

    if from.mint_address == to.mint_address {
        let (deposit_instructions, deposit_accounts) = mock_kamino_reserve_transaction(
            vault,
            to_accounts,
            mock_kamino_deposit_reserve_liquidity_data(in_amount_raw),
        );
        let deposit_ix = execute_squads_program_interaction_instruction(
            deposit_policy,
            signer,
            vault_index,
            deposit_instructions,
            vec![0],
            deposit_accounts,
        );
        return HubTransition {
            route_instructions: vec![withdraw_ix, deposit_ix],
            treasury_rebalance_instruction: None,
            next_amount_raw: in_amount_raw,
            needs_hub_authorizer: false,
            hub_fee_revenue_usd: 0.0,
            equivalent_jupiter_user_loss_usd: 0.0,
        };
    }

    let jupiter_cost = jupiter_costs
        .get(&directed_pair_key(from, to))
        .expect("cross-mint Jupiter cost exists");
    assert!(
        jupiter_cost.available,
        "cross-mint Jupiter route should be available for treasury rebalance"
    );
    let in_value_usd = usd_value(in_amount_raw, from);
    let jupiter_loss_fraction = jupiter_cost.loss_fraction.unwrap_or(0.0);
    let hub_fee_fraction = pricing.fee_fraction(jupiter_loss_fraction);
    let ideal_out_raw = raw_from_usd(in_value_usd, to);
    let user_out_value_usd = in_value_usd * (1.0 - hub_fee_fraction);
    let user_out_raw = raw_from_usd(user_out_value_usd, to);
    let jupiter_out_raw = raw_from_usd(in_value_usd * (1.0 - jupiter_loss_fraction), to);
    let hub_fee_revenue_usd = usd_value(ideal_out_raw.saturating_sub(user_out_raw), to);
    let equivalent_jupiter_user_loss_usd =
        usd_value(ideal_out_raw.saturating_sub(jupiter_out_raw), to);

    let swap_ix = execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
        swap_policy,
        signer,
        vault_index,
        vault,
        vault_token_accounts[&from.mint_address],
        vault_token_accounts[&to.mint_address],
        from.mint_address,
        to.mint_address,
        hub_authorizer,
        in_amount_raw,
        user_out_raw,
        user_out_raw,
        HUB_MAX_FEE_BPS,
        1,
    );
    let (deposit_instructions, deposit_accounts) = mock_kamino_reserve_transaction(
        vault,
        to_accounts,
        mock_kamino_deposit_reserve_liquidity_data(user_out_raw),
    );
    let deposit_ix = execute_squads_program_interaction_instruction(
        deposit_policy,
        signer,
        vault_index,
        deposit_instructions,
        vec![0],
        deposit_accounts,
    );

    HubTransition {
        route_instructions: vec![withdraw_ix, swap_ix, deposit_ix],
        treasury_rebalance_instruction: Some(treasury_rebalance_hub_through_jupiter_ix(
            hub_authorizer,
            from,
            to,
            in_amount_raw,
            jupiter_out_raw,
            user_out_raw,
        )),
        next_amount_raw: user_out_raw,
        needs_hub_authorizer: true,
        hub_fee_revenue_usd,
        equivalent_jupiter_user_loss_usd,
    }
}

fn create_treasury_squads(
    context: &mut squads_test_harness::FundedSquadsTestContext,
    treasury_executor: Pubkey,
    mints: &[Pubkey],
    metadata_by_mint: &HashMap<Pubkey, Choice>,
) -> TreasurySquads {
    let pool = derive_squads_pool(TREASURY_SEED);
    let create_ix = create_squads_smart_account_instruction(
        context.wallet_pubkey(),
        treasury_executor,
        TREASURY_SEED,
    );
    try_send_instructions(&mut context.svm, &[create_ix], &context.wallet, &[])
        .expect("wallet A creates Loyal Treasury Squads account");

    let vault_index = 0;
    let (vault, _) = derive_squads_vault(&pool.settings, vault_index);
    context
        .svm
        .airdrop(&vault, 10 * LAMPORTS_PER_SOL)
        .expect("airdrop Loyal Treasury vault");

    let mut token_accounts = HashMap::new();
    for mint in mints {
        let token_account = treasury_token_account_for_mint(*mint);
        let point = &metadata_by_mint[mint];
        seed_spl_token_account(
            &mut context.svm,
            token_account,
            *mint,
            vault,
            raw_from_usd(TREASURY_STARTING_VALUE_USD_PER_MINT, point),
        );
        token_accounts.insert(*mint, token_account);
    }

    TreasurySquads {
        pool,
        vault_index,
        vault,
        token_accounts,
    }
}

fn seed_hub_inventory(
    context: &mut squads_test_harness::FundedSquadsTestContext,
    mints: &[Pubkey],
    metadata_by_mint: &HashMap<Pubkey, Choice>,
) {
    let hub_authority = derive_loyal_hub_authority();
    squads_test_harness::seed_empty_system_account_if_missing(&mut context.svm, hub_authority);
    for mint in mints {
        let point = &metadata_by_mint[mint];
        seed_spl_token_account(
            &mut context.svm,
            loyal_hub_token_account(*mint),
            *mint,
            hub_authority,
            raw_from_usd(HUB_TARGET_VALUE_USD_PER_MINT, point),
        );
    }
}

fn treasury_initialize_hub_ix(
    treasury: &TreasurySquads,
    hub_authorizer: Pubkey,
    allowed_mints: &[Pubkey],
) -> Instruction {
    let init_ix = initialize_loyal_hub_config_instruction(
        treasury.vault,
        treasury.vault,
        hub_authorizer,
        HUB_MAX_FEE_BPS,
        false,
        allowed_mints,
    );
    execute_squads_sync_transaction_instruction(
        treasury.pool.settings,
        hub_authorizer,
        treasury.vault_index,
        vec![SquadsCompiledInstruction {
            program_id_index: 3,
            accounts: vec![0, 1, 2],
            data: init_ix.data,
        }],
        vec![
            AccountMeta::new(treasury.vault, false),
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(LOYAL_HUB_SWAP_PROGRAM_ID, false),
        ],
    )
}

fn treasury_rebalance_hub_through_jupiter_ix(
    treasury_signer: Pubkey,
    from: &Choice,
    to: &Choice,
    hub_input_amount: u64,
    jupiter_output_amount: u64,
    hub_output_top_up_amount: u64,
) -> Instruction {
    let treasury = derive_squads_pool(TREASURY_SEED);
    let (treasury_vault, _) = derive_squads_vault(&treasury.settings, 0);
    let treasury_input = treasury_token_account_for_mint(from.mint_address);
    let treasury_output = treasury_token_account_for_mint(to.mint_address);
    let hub_input = loyal_hub_token_account(from.mint_address);
    let hub_output = loyal_hub_token_account(to.mint_address);

    execute_squads_sync_transaction_instruction(
        treasury.settings,
        treasury_signer,
        0,
        vec![
            SquadsCompiledInstruction {
                program_id_index: 7,
                accounts: vec![0, 1, 2, 3, 4, 5, 6],
                data: loyal_hub_withdraw_inventory_data(hub_input_amount),
            },
            SquadsCompiledInstruction {
                program_id_index: 14,
                accounts: vec![1, 3, 8, 4, 9, 6, 10, 11, 12],
                data: mock_jupiter_stable_exact_in_swap_data(
                    hub_input_amount,
                    jupiter_output_amount,
                    from.mint_address,
                    to.mint_address,
                ),
            },
            SquadsCompiledInstruction {
                program_id_index: 6,
                accounts: vec![8, 9, 13, 1],
                data: spl_token::instruction::transfer_checked(
                    &spl_token::id(),
                    &treasury_output,
                    &to.mint_address,
                    &hub_output,
                    &treasury_vault,
                    &[],
                    hub_output_top_up_amount,
                    to.decimals,
                )
                .expect("build treasury top-up transfer_checked")
                .data,
            },
        ],
        vec![
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(treasury_vault, false),
            AccountMeta::new(hub_input, false),
            AccountMeta::new(treasury_input, false),
            AccountMeta::new_readonly(from.mint_address, false),
            AccountMeta::new_readonly(derive_loyal_hub_authority(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(LOYAL_HUB_SWAP_PROGRAM_ID, false),
            AccountMeta::new(treasury_output, false),
            AccountMeta::new_readonly(to.mint_address, false),
            AccountMeta::new(
                mock_jupiter_stable_reserve_token_account(from.mint_address),
                false,
            ),
            AccountMeta::new(
                mock_jupiter_stable_reserve_token_account(to.mint_address),
                false,
            ),
            AccountMeta::new_readonly(derive_mock_jupiter_swap_authority(), false),
            AccountMeta::new(hub_output, false),
            AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
        ],
    )
}

fn treasury_token_account_for_mint(mint: Pubkey) -> Pubkey {
    Pubkey::new_from_array(
        solana_sdk::hash::hashv(&[b"loyal-treasury-token", mint.as_ref()]).to_bytes(),
    )
}

fn apply_mock_kamino_accrual(
    svm: &mut litesvm::LiteSVM,
    accounts: MockKaminoReserveTokenAccounts,
    amount_raw: u64,
) {
    set_spl_token_amount(svm, accounts.vault_collateral, amount_raw);
    set_spl_mint_supply(svm, accounts.collateral_mint, amount_raw);
    if get_spl_token_amount(svm, accounts.reserve_liquidity_supply) < amount_raw {
        set_spl_token_amount(svm, accounts.reserve_liquidity_supply, amount_raw);
    }
}

fn assert_route_state(
    svm: &litesvm::LiteSVM,
    reserves: &HashMap<usize, MockKaminoReserveTokenAccounts>,
    current_reserve_index: usize,
    current_amount_raw: u64,
) {
    for (reserve_index, accounts) in reserves {
        let expected_collateral = if *reserve_index == current_reserve_index {
            current_amount_raw
        } else {
            0
        };
        assert_eq!(
            get_spl_token_amount(svm, accounts.vault_collateral),
            expected_collateral,
            "vault collateral mismatch for reserve {reserve_index}"
        );
        assert_eq!(
            get_spl_token_amount(svm, accounts.vault_liquidity),
            0,
            "vault liquidity should be fully deposited for reserve {reserve_index}"
        );
    }
}

fn simulate_fixed_start_hindsight(
    backtest: &Backtest,
    jupiter_costs: &HashMap<String, JupiterCost>,
) -> HindsightRoute {
    simulate_route(backtest, |value_usd, from, to| {
        transition_cost_jupiter(value_usd, from, to, jupiter_costs)
    })
}

fn simulate_hub_hindsight(
    backtest: &Backtest,
    jupiter_costs: &HashMap<String, JupiterCost>,
    pricing: &HubPricing,
) -> HindsightRoute {
    simulate_route(backtest, |value_usd, from, to| {
        transition_cost_hub(value_usd, from, to, jupiter_costs, pricing)
    })
}

fn simulate_trailing_six_hour_mean_route(
    backtest: &Backtest,
    jupiter_costs: &HashMap<String, JupiterCost>,
    pricing: &HubPricing,
) -> HindsightRoute {
    let first_hour = backtest
        .hourly_choices
        .first()
        .expect("at least one hourly choice");
    let start = first_hour
        .choices
        .iter()
        .find(|choice| {
            choice.market_address == KAMINO_PRIME_MARKET
                && choice.reserve_address == KAMINO_PRIME_USDC_RESERVE
                && choice.mint_address == USDC_MINT
        })
        .expect("Prime USDC is available at the first timestamp")
        .clone();

    let mut current = start.clone();
    let mut value_usd = STARTING_VALUE_USD;
    let mut path = vec![RouteStep {
        timestamp: first_hour.timestamp.clone(),
        point: start,
    }];

    for index in 1..backtest.hourly_choices.len() {
        let previous_timestamp = &backtest.hourly_choices[index - 1].timestamp;
        let hour = &backtest.hourly_choices[index];
        let elapsed = elapsed_years(previous_timestamp, &hour.timestamp);
        let Some(previous_point) = backtest.point_at(current.reserve_index, previous_timestamp)
        else {
            continue;
        };
        value_usd *= (previous_point.supply_apy * elapsed).exp();

        let Some(current_at_hour) = backtest
            .point_at(current.reserve_index, &hour.timestamp)
            .cloned()
        else {
            continue;
        };
        current = current_at_hour;

        let stay_mean = trailing_mean_apy(
            backtest,
            current.reserve_index,
            index,
            SIX_HOUR_MEAN_WINDOW_HOURS,
        )
        .unwrap_or(current.supply_apy);
        let mut best = Some((
            value_usd * (stay_mean * elapsed).exp(),
            value_usd,
            current.clone(),
        ));

        for candidate in &hour.choices {
            if !can_transition_with_hub_rebalance(&current, candidate, jupiter_costs) {
                continue;
            }
            let Some(mean_apy) = trailing_mean_apy(
                backtest,
                candidate.reserve_index,
                index,
                SIX_HOUR_MEAN_WINDOW_HOURS,
            ) else {
                continue;
            };
            let Some(transition_cost) =
                transition_cost_hub(value_usd, &current, candidate, jupiter_costs, pricing)
            else {
                continue;
            };
            let candidate_value = value_usd - transition_cost;
            if candidate_value <= 0.0 {
                continue;
            }
            let score = candidate_value * (mean_apy * elapsed).exp();
            if best
                .as_ref()
                .map(|(best_score, _, _): &(f64, f64, Choice)| score > *best_score)
                .unwrap_or(true)
            {
                best = Some((score, candidate_value, candidate.clone()));
            }
        }

        let Some((_, next_value, next)) = best else {
            continue;
        };
        if next.reserve_index != current.reserve_index {
            value_usd = next_value;
            path.push(RouteStep {
                timestamp: hour.timestamp.clone(),
                point: next.clone(),
            });
        }
        current = next;
    }

    let ending_value_usd = model_route_value(backtest, jupiter_costs, &path, pricing);
    HindsightRoute {
        path,
        ending_value_usd,
    }
}

fn can_transition_with_hub_rebalance(
    from: &Choice,
    to: &Choice,
    jupiter_costs: &HashMap<String, JupiterCost>,
) -> bool {
    if from.reserve_index == to.reserve_index || from.mint_address == to.mint_address {
        return true;
    }
    jupiter_costs
        .get(&directed_pair_key(from, to))
        .map(|cost| cost.available)
        .unwrap_or(false)
}

fn trailing_mean_apy(
    backtest: &Backtest,
    reserve_index: usize,
    end_index: usize,
    window_hours: usize,
) -> Option<f64> {
    if end_index + 1 < window_hours {
        return None;
    }
    let start_index = end_index + 1 - window_hours;
    let mut total = 0.0;
    for index in start_index..=end_index {
        let timestamp = &backtest.hourly_choices[index].timestamp;
        total += backtest.point_at(reserve_index, timestamp)?.supply_apy;
    }
    Some(total / window_hours as f64)
}

fn model_route_value(
    backtest: &Backtest,
    jupiter_costs: &HashMap<String, JupiterCost>,
    path: &[RouteStep],
    pricing: &HubPricing,
) -> f64 {
    let mut current = path.first().expect("route starts").point.clone();
    let mut value = STARTING_VALUE_USD;
    for next in path.iter().skip(1) {
        value = accrue_segment_value(backtest, &current, value, &next.timestamp);
        value -= transition_cost_hub(value, &current, &next.point, jupiter_costs, pricing)
            .expect("route transition is reachable");
        current = next.point.clone();
    }
    accrue_segment_value(backtest, &current, value, &backtest.end_timestamp)
}

fn accrue_segment_value(
    backtest: &Backtest,
    current: &Choice,
    mut value_usd: f64,
    end_timestamp: &str,
) -> f64 {
    let start_index = backtest.time_index[&current.timestamp];
    let end_index = backtest.time_index[end_timestamp];
    for index in (start_index + 1)..=end_index {
        let previous_timestamp = &backtest.hourly_choices[index - 1].timestamp;
        let timestamp = &backtest.hourly_choices[index].timestamp;
        let elapsed_years = elapsed_years(previous_timestamp, timestamp);
        let supply_apy = backtest
            .point_at(current.reserve_index, previous_timestamp)
            .map(|point| point.supply_apy)
            .unwrap_or(current.supply_apy);
        value_usd *= (supply_apy * elapsed_years).exp();
    }
    value_usd
}

fn find_max_jupiter_like_fee_model(
    backtest: &Backtest,
    jupiter_costs: &HashMap<String, JupiterCost>,
    years: f64,
    target_min_apy: f64,
) -> (JupiterLikeFeeCandidate, Vec<JupiterLikeFeeCandidate>) {
    let mut best = None;
    let mut candidates = Vec::new();
    for share_of_jupiter in JUPITER_LIKE_FEE_SHARES {
        let pricing = HubPricing::Discounted {
            share_of_jupiter,
            cap_bps: JUPITER_LIKE_FEE_CAP_BPS,
        };
        let route = simulate_hub_hindsight(backtest, jupiter_costs, &pricing);
        let modeled_apy = annualized_apy(route.ending_value_usd, years);
        if modeled_apy < target_min_apy {
            continue;
        }
        let candidate = JupiterLikeFeeCandidate {
            share_of_jupiter,
            modeled_apy,
            pricing,
            route,
        };
        candidates.push(candidate.clone());
        if best
            .as_ref()
            .map(|current: &JupiterLikeFeeCandidate| {
                candidate.share_of_jupiter > current.share_of_jupiter
            })
            .unwrap_or(true)
        {
            best = Some(candidate);
        }
    }

    (
        best.expect("at least one quick Jupiter-like fee candidate should satisfy the APY floor"),
        candidates,
    )
}

fn simulate_route<F>(backtest: &Backtest, transition_cost: F) -> HindsightRoute
where
    F: Fn(f64, &Choice, &Choice) -> Option<f64>,
{
    let first_hour = backtest
        .hourly_choices
        .first()
        .expect("at least one hourly choice");
    let start = first_hour
        .choices
        .iter()
        .find(|choice| {
            choice.market_address == KAMINO_PRIME_MARKET
                && choice.reserve_address == KAMINO_PRIME_USDC_RESERVE
                && choice.mint_address == USDC_MINT
        })
        .expect("Prime USDC is available at the first timestamp")
        .clone();

    let mut states = HashMap::from([(
        start.reserve_index,
        DynamicState {
            value_usd: STARTING_VALUE_USD,
            point: start.clone(),
            prev_key: start.reserve_index,
        },
    )]);
    let mut backpointers = Vec::<HashMap<usize, DynamicState>>::new();

    for index in 1..backtest.hourly_choices.len() {
        let previous_timestamp = &backtest.hourly_choices[index - 1].timestamp;
        let timestamp = &backtest.hourly_choices[index].timestamp;
        let elapsed_years = elapsed_years(previous_timestamp, timestamp);
        let previous_states = states.clone();
        let mut next_states = HashMap::new();

        for candidate in &backtest.hourly_choices[index].choices {
            let mut best = None;
            for (from_key, state) in &previous_states {
                let accrued_value =
                    state.value_usd * (state.point.supply_apy * elapsed_years).exp();
                let Some(switch_cost) = transition_cost(accrued_value, &state.point, candidate)
                else {
                    continue;
                };
                let value = accrued_value - switch_cost;
                if best
                    .as_ref()
                    .map(|current: &DynamicState| value > current.value_usd)
                    .unwrap_or(true)
                {
                    best = Some(DynamicState {
                        value_usd: value,
                        point: candidate.clone(),
                        prev_key: *from_key,
                    });
                }
            }
            if let Some(best) = best {
                next_states.insert(candidate.reserve_index, best);
            }
        }

        assert!(
            !next_states.is_empty(),
            "hindsight state should remain reachable"
        );
        states = next_states.clone();
        backpointers.push(next_states);
    }

    let (mut best_key, best_state) = states
        .iter()
        .max_by(|(_, a), (_, b)| a.value_usd.total_cmp(&b.value_usd))
        .map(|(key, state)| (*key, state.clone()))
        .expect("best final state");
    let ending_value_usd = best_state.value_usd;
    let mut path = Vec::new();

    for index in (0..backpointers.len()).rev() {
        let state = backpointers[index]
            .get(&best_key)
            .expect("backpointer for best key");
        if state.prev_key != best_key {
            path.push(RouteStep {
                timestamp: backtest.hourly_choices[index + 1].timestamp.clone(),
                point: state.point.clone(),
            });
        }
        best_key = state.prev_key;
    }

    path.push(RouteStep {
        timestamp: first_hour.timestamp.clone(),
        point: start,
    });
    path.reverse();

    HindsightRoute {
        path,
        ending_value_usd,
    }
}

fn transition_cost_jupiter(
    value_usd: f64,
    from: &Choice,
    to: &Choice,
    jupiter_costs: &HashMap<String, JupiterCost>,
) -> Option<f64> {
    if from.reserve_index == to.reserve_index {
        return Some(0.0);
    }
    if from.mint_address == to.mint_address {
        return Some(POOL_CHANGE_USD);
    }
    let quote_cost = jupiter_costs.get(&directed_pair_key(from, to))?;
    if !quote_cost.available {
        return None;
    }
    Some(value_usd * quote_cost.loss_fraction.unwrap_or(0.0) + POOL_CHANGE_USD)
}

fn transition_cost_hub(
    value_usd: f64,
    from: &Choice,
    to: &Choice,
    jupiter_costs: &HashMap<String, JupiterCost>,
    pricing: &HubPricing,
) -> Option<f64> {
    if from.reserve_index == to.reserve_index {
        return Some(0.0);
    }
    if from.mint_address == to.mint_address {
        return Some(POOL_CHANGE_USD);
    }
    let quote_cost = jupiter_costs.get(&directed_pair_key(from, to))?;
    if !quote_cost.available {
        return None;
    }
    let hub_fee = value_usd * pricing.fee_fraction(quote_cost.loss_fraction.unwrap_or(0.0));
    Some(hub_fee + 2.0 * POOL_CHANGE_USD)
}

fn accrue_segment_raw(
    backtest: &Backtest,
    current: &Choice,
    amount_raw: u64,
    end_timestamp: &str,
) -> u64 {
    let start_index = backtest.time_index[&current.timestamp];
    let end_index = backtest.time_index[end_timestamp];
    let mut amount = amount_raw as f64;
    for index in (start_index + 1)..=end_index {
        let previous_timestamp = &backtest.hourly_choices[index - 1].timestamp;
        let timestamp = &backtest.hourly_choices[index].timestamp;
        let elapsed_years = elapsed_years(previous_timestamp, timestamp);
        let supply_apy = backtest
            .point_at(current.reserve_index, previous_timestamp)
            .map(|point| point.supply_apy)
            .unwrap_or(current.supply_apy);
        amount *= (supply_apy * elapsed_years).exp();
    }
    amount.round() as u64
}

fn build_backtest(history: &HistoryCache) -> Backtest {
    let mut reserves = Vec::new();
    let mut by_timestamp = BTreeMap::<String, Vec<Choice>>::new();
    let mut point_lookup = HashMap::<(usize, String), Choice>::new();

    for reserve_history in &history.reserve_histories {
        let Some(latest) = reserve_history.history.history.last() else {
            continue;
        };
        if !is_stable_metric(&latest.metrics) {
            continue;
        }

        let reserve_index = reserves.len();
        for item in &reserve_history.history.history {
            let Some(point) = parse_choice(reserve_index, reserve_history, item) else {
                continue;
            };
            if !is_eligible_point(&point) {
                continue;
            }
            point_lookup.insert((reserve_index, point.timestamp.clone()), point.clone());
            by_timestamp
                .entry(point.timestamp.clone())
                .or_default()
                .push(point);
        }

        if by_timestamp.values().any(|choices| {
            choices
                .iter()
                .any(|choice| choice.reserve_index == reserve_index)
        }) {
            let latest_point = parse_choice(reserve_index, reserve_history, latest)
                .expect("stable latest point parses");
            reserves.push(ReserveMeta {
                market_address: latest_point.market_address,
                reserve_address: latest_point.reserve_address,
                mint_address: latest_point.mint_address,
                decimals: latest_point.decimals,
            });
        }
    }

    let mut hourly_choices = by_timestamp
        .into_iter()
        .map(|(timestamp, mut choices)| {
            choices.sort_by(|a, b| b.supply_apy.total_cmp(&a.supply_apy));
            HourlyChoices { timestamp, choices }
        })
        .collect::<Vec<_>>();
    hourly_choices.sort_by(|a, b| a.timestamp.cmp(&b.timestamp));
    let time_index = hourly_choices
        .iter()
        .enumerate()
        .map(|(index, hour)| (hour.timestamp.clone(), index))
        .collect::<HashMap<_, _>>();
    let end_timestamp = hourly_choices
        .last()
        .expect("hourly choices are present")
        .timestamp
        .clone();

    Backtest {
        reserves,
        hourly_choices,
        point_lookup,
        time_index,
        end_timestamp,
    }
}

fn parse_choice(
    reserve_index: usize,
    reserve_history: &ReserveHistory,
    item: &HistoryPoint,
) -> Option<Choice> {
    let metrics = &item.metrics;
    let market_address = pubkey_from_str(&reserve_history.market.lending_market);
    let reserve_address = pubkey_from_str(&reserve_history.reserve_address);
    let mint_address = pubkey_from_str(metrics.mint_address.as_ref()?);
    Some(Choice {
        reserve_index,
        timestamp: item.timestamp.clone(),
        market_address,
        reserve_address,
        mint_address,
        decimals: value_as_u8(metrics.decimals.as_ref()).unwrap_or(6),
        supply_apy: value_as_f64(metrics.supply_interest_apy.as_ref())?,
        deposit_tvl: value_as_f64(metrics.deposit_tvl.as_ref())?,
        asset_oracle_price_usd: value_as_f64(metrics.asset_oracle_price_usd.as_ref())
            .or_else(|| value_as_f64(metrics.asset_price_usd.as_ref()))?,
    })
}

fn is_stable_metric(metrics: &Metrics) -> bool {
    let symbol = metrics
        .symbol
        .as_deref()
        .unwrap_or_default()
        .to_ascii_uppercase()
        .chars()
        .filter(|ch| ch.is_ascii_alphanumeric())
        .collect::<String>();
    let price = value_as_f64(metrics.asset_oracle_price_usd.as_ref())
        .or_else(|| value_as_f64(metrics.asset_price_usd.as_ref()))
        .unwrap_or_default();
    stable_symbols().contains(symbol.as_str()) && (0.75..=1.35).contains(&price)
}

fn is_eligible_point(point: &Choice) -> bool {
    point.supply_apy.is_finite()
        && point.supply_apy >= 0.0
        && point.supply_apy < APY_CAP
        && point.deposit_tvl.is_finite()
        && point.deposit_tvl > TVL_FLOOR_USD
}

fn stable_symbols() -> HashSet<&'static str> {
    [
        "AUSD",
        "CASH",
        "EUSX",
        "FDUSD",
        "PYUSD",
        "SUSD",
        "SUSDE",
        "SYRUPUSDC",
        "USCC",
        "USDC",
        "USDCDEP",
        "USDE",
        "USD1",
        "USDG",
        "USDH",
        "USDS",
        "USDT",
        "USDY",
    ]
    .into_iter()
    .collect()
}

fn treasury_token_value_usd(
    context: &squads_test_harness::FundedSquadsTestContext,
    treasury: &TreasurySquads,
    metadata_by_mint: &HashMap<Pubkey, Choice>,
) -> f64 {
    treasury
        .token_accounts
        .iter()
        .map(|(mint, token_account)| {
            usd_value(
                get_spl_token_amount(&context.svm, *token_account),
                &metadata_by_mint[mint],
            )
        })
        .sum()
}

fn hub_inventory_value_usd(
    context: &squads_test_harness::FundedSquadsTestContext,
    mints: &[Pubkey],
    metadata_by_mint: &HashMap<Pubkey, Choice>,
) -> f64 {
    mints
        .iter()
        .map(|mint| {
            usd_value(
                get_spl_token_amount(&context.svm, loyal_hub_token_account(*mint)),
                &metadata_by_mint[mint],
            )
        })
        .sum()
}

fn raw_from_usd(value_usd: f64, point: &Choice) -> u64 {
    ((value_usd / point.asset_oracle_price_usd) * 10_f64.powi(point.decimals as i32)).round() as u64
}

fn usd_value(amount_raw: u64, point: &Choice) -> f64 {
    (amount_raw as f64 / 10_f64.powi(point.decimals as i32)) * point.asset_oracle_price_usd
}

fn directed_pair_key(from: &Choice, to: &Choice) -> String {
    format!("{}->{}", from.mint_address, to.mint_address)
}

fn elapsed_years(start: &str, end: &str) -> f64 {
    (timestamp_hours(end) - timestamp_hours(start)) as f64 / (365.0 * 24.0)
}

fn timestamp_hours(timestamp: &str) -> i64 {
    let year = timestamp[0..4].parse::<i32>().expect("timestamp year");
    let month = timestamp[5..7].parse::<u32>().expect("timestamp month");
    let day = timestamp[8..10].parse::<u32>().expect("timestamp day");
    let hour = timestamp[11..13].parse::<i64>().expect("timestamp hour");
    days_from_civil(year, month, day) * 24 + hour
}

fn days_from_civil(year: i32, month: u32, day: u32) -> i64 {
    let year = year - i32::from(month <= 2);
    let era = if year >= 0 { year } else { year - 399 } / 400;
    let year_of_era = year - era * 400;
    let month = month as i32;
    let day_of_year = (153 * (month + if month > 2 { -3 } else { 9 }) + 2) / 5 + day as i32 - 1;
    let day_of_era = year_of_era * 365 + year_of_era / 4 - year_of_era / 100 + day_of_year;
    (era * 146_097 + day_of_era - 719_468) as i64
}

fn annualized_apy(ending_value_usd: f64, years: f64) -> f64 {
    (ending_value_usd / STARTING_VALUE_USD).powf(1.0 / years) - 1.0
}

fn monthly_fee_usd(total_fee_usd: f64, years: f64) -> f64 {
    total_fee_usd / (years * 12.0)
}

fn lamports_to_usd(lamports: u64) -> f64 {
    (lamports as f64 / LAMPORTS_PER_SOL as f64) * SOL_PRICE_USD
}

fn assert_close(actual: f64, expected: f64, tolerance: f64, message: &str) {
    assert!(
        (actual - expected).abs() <= tolerance,
        "{message}: actual {actual}, expected {expected}, tolerance {tolerance}"
    );
}

fn load_analysis() -> Analysis {
    read_json(&repo_root_path(ANALYSIS_PATH))
}

fn load_history_cache() -> HistoryCache {
    read_json(&repo_root_path(HISTORY_CACHE_PATH))
}

fn repo_root_path(relative_path: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../..")
        .join(relative_path)
}

fn read_json<T: serde::de::DeserializeOwned>(path: &Path) -> T {
    let bytes = fs::read(path).unwrap_or_else(|error| {
        panic!(
            "failed to read {}; run `bun scripts/analyze-kamino-hourly-reserves.mjs --cache-only`: {error}",
            path.display()
        )
    });
    serde_json::from_slice(&bytes)
        .unwrap_or_else(|error| panic!("failed to parse {} as JSON: {error}", path.display()))
}

fn pubkey_from_str(value: &str) -> Pubkey {
    Pubkey::from_str(value).expect("parse pubkey")
}

fn value_as_u8(value: Option<&serde_json::Value>) -> Option<u8> {
    value_as_f64(value).map(|value| value as u8)
}

fn value_as_f64(value: Option<&serde_json::Value>) -> Option<f64> {
    match value? {
        serde_json::Value::Number(number) => number.as_f64(),
        serde_json::Value::String(string) => string.parse().ok(),
        _ => None,
    }
}

#[derive(Clone, Debug)]
struct Backtest {
    reserves: Vec<ReserveMeta>,
    hourly_choices: Vec<HourlyChoices>,
    point_lookup: HashMap<(usize, String), Choice>,
    time_index: HashMap<String, usize>,
    end_timestamp: String,
}

impl Backtest {
    fn point_at(&self, reserve_index: usize, timestamp: &str) -> Option<&Choice> {
        self.point_lookup
            .get(&(reserve_index, timestamp.to_owned()))
    }
}

#[derive(Clone, Debug)]
struct ReserveMeta {
    market_address: Pubkey,
    reserve_address: Pubkey,
    mint_address: Pubkey,
    decimals: u8,
}

#[derive(Clone, Debug)]
struct HourlyChoices {
    timestamp: String,
    choices: Vec<Choice>,
}

#[derive(Clone, Debug)]
struct Choice {
    reserve_index: usize,
    timestamp: String,
    market_address: Pubkey,
    reserve_address: Pubkey,
    mint_address: Pubkey,
    decimals: u8,
    supply_apy: f64,
    deposit_tvl: f64,
    asset_oracle_price_usd: f64,
}

#[derive(Clone, Debug)]
struct DynamicState {
    value_usd: f64,
    point: Choice,
    prev_key: usize,
}

#[derive(Clone, Debug)]
struct HindsightRoute {
    path: Vec<RouteStep>,
    ending_value_usd: f64,
}

#[derive(Clone, Debug)]
struct RouteStep {
    timestamp: String,
    point: Choice,
}

#[derive(Clone, Debug)]
enum HubPricing {
    ZeroFee,
    Discounted { share_of_jupiter: f64, cap_bps: f64 },
}

impl HubPricing {
    fn fee_fraction(&self, jupiter_loss_fraction: f64) -> f64 {
        match self {
            Self::ZeroFee => 0.0,
            Self::Discounted {
                share_of_jupiter,
                cap_bps,
            } => (jupiter_loss_fraction * share_of_jupiter).min(cap_bps / 10_000.0),
        }
    }
}

#[derive(Clone, Debug)]
struct HubTransition {
    route_instructions: Vec<Instruction>,
    treasury_rebalance_instruction: Option<Instruction>,
    next_amount_raw: u64,
    needs_hub_authorizer: bool,
    hub_fee_revenue_usd: f64,
    equivalent_jupiter_user_loss_usd: f64,
}

#[derive(Clone, Debug)]
struct JupiterLikeFeeCandidate {
    share_of_jupiter: f64,
    modeled_apy: f64,
    pricing: HubPricing,
    route: HindsightRoute,
}

#[derive(Clone, Debug)]
struct HubRouteReport {
    skipped: bool,
    user_gross_value_usd: f64,
    user_net_value_usd: f64,
    route_tx_fees_usd: f64,
    treasury_rebalance_loss_usd: f64,
    treasury_rebalance_tx_fees_usd: f64,
    treasury_net_after_fees_usd: f64,
    hub_fee_revenue_usd: f64,
    equivalent_jupiter_user_loss_usd: f64,
    cross_mint_rebalances: u64,
}

impl HubRouteReport {
    fn skipped() -> Self {
        Self {
            skipped: true,
            user_gross_value_usd: 0.0,
            user_net_value_usd: 0.0,
            route_tx_fees_usd: 0.0,
            treasury_rebalance_loss_usd: 0.0,
            treasury_rebalance_tx_fees_usd: 0.0,
            treasury_net_after_fees_usd: 0.0,
            hub_fee_revenue_usd: 0.0,
            equivalent_jupiter_user_loss_usd: 0.0,
            cross_mint_rebalances: 0,
        }
    }
}

#[derive(Clone, Debug)]
struct TreasurySquads {
    pool: SquadsPool,
    vault_index: u8,
    vault: Pubkey,
    token_accounts: HashMap<Pubkey, Pubkey>,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct Analysis {
    assumptions: AnalysisAssumptions,
    jupiter_costs: HashMap<String, JupiterCost>,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct AnalysisAssumptions {
    requested_start: String,
    requested_end: String,
    frequency: String,
    pool_change_lamports: u64,
}

#[derive(Clone, Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterCost {
    available: bool,
    loss_fraction: Option<f64>,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct HistoryCache {
    reserve_histories: Vec<ReserveHistory>,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct ReserveHistory {
    market: Market,
    reserve_address: String,
    history: ReserveMetricHistory,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct Market {
    lending_market: String,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct ReserveMetricHistory {
    history: Vec<HistoryPoint>,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct HistoryPoint {
    timestamp: String,
    metrics: Metrics,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct Metrics {
    #[serde(default)]
    symbol: Option<String>,
    #[serde(default)]
    decimals: Option<serde_json::Value>,
    #[serde(default)]
    #[serde(rename = "mintAddress")]
    mint_address: Option<String>,
    #[serde(default)]
    #[serde(rename = "supplyInterestAPY")]
    supply_interest_apy: Option<serde_json::Value>,
    #[serde(default)]
    #[serde(rename = "depositTvl")]
    deposit_tvl: Option<serde_json::Value>,
    #[serde(default)]
    #[serde(rename = "assetOraclePriceUSD")]
    asset_oracle_price_usd: Option<serde_json::Value>,
    #[serde(default)]
    #[serde(rename = "assetPriceUSD")]
    asset_price_usd: Option<serde_json::Value>,
}
