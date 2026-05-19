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

fn build_rebalance_transaction(
    vault: Pubkey,
    withdraw_policy: Pubkey,
    swap_policy: Pubkey,
    deposit_policy: Pubkey,
    signer: Pubkey,
    vault_index: u8,
    vault_token_accounts: &HashMap<Pubkey, Pubkey>,
    reserve_accounts: &HashMap<usize, MockKaminoReserveTokenAccounts>,
    jupiter_costs: &HashMap<String, JupiterCost>,
    from: &Choice,
    to: &Choice,
    in_amount_raw: u64,
) -> (Vec<solana_sdk::instruction::Instruction>, u64) {
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
        return (vec![withdraw_ix, deposit_ix], in_amount_raw);
    }

    let cost = jupiter_costs
        .get(&directed_pair_key(from, to))
        .expect("cross-mint Jupiter cost exists");
    assert!(
        cost.available,
        "cross-mint Jupiter route should be available"
    );
    let out_value_usd = usd_value(in_amount_raw, from) * (1.0 - cost.loss_fraction.unwrap_or(0.0));
    let out_amount_raw = raw_from_usd(out_value_usd, to);
    let swap_ix = execute_squads_yield_route_stable_swap_instruction(
        swap_policy,
        signer,
        vault_index,
        vault,
        vault_token_accounts[&from.mint_address],
        vault_token_accounts[&to.mint_address],
        from.mint_address,
        to.mint_address,
        in_amount_raw,
        out_amount_raw,
    );
    let (deposit_instructions, deposit_accounts) = mock_kamino_reserve_transaction(
        vault,
        to_accounts,
        mock_kamino_deposit_reserve_liquidity_data(out_amount_raw),
    );
    let deposit_ix = execute_squads_program_interaction_instruction(
        deposit_policy,
        signer,
        vault_index,
        deposit_instructions,
        vec![0],
        deposit_accounts,
    );

    (vec![withdraw_ix, swap_ix, deposit_ix], out_amount_raw)
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
        let point = backtest
            .point_at(current.reserve_index, previous_timestamp)
            .expect("current reserve has an hourly APY point while held");
        amount *= (point.supply_apy * elapsed_years).exp();
    }
    amount.round() as u64
}

fn simulate_fixed_start_hindsight(
    backtest: &Backtest,
    jupiter_costs: &HashMap<String, JupiterCost>,
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
                let Some(switch_cost) =
                    transition_cost(accrued_value, &state.point, candidate, jupiter_costs)
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

fn transition_cost(
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
