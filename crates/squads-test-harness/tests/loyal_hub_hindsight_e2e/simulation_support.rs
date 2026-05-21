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
