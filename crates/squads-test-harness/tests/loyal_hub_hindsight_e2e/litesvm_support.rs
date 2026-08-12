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
    let route_action_setup = create_three_step_yield_route_actions(
        loyal_action_context(context, wallet_b.pubkey()),
        yield_route_universe_from_mock_reserves(route_mints.clone(), route_reserve_accounts),
        vec![
            mock_jupiter_swap_lane(true),
            SwapLane::LoyalHub {
                hub_authorizer: treasury_executor.pubkey(),
                max_fee_bps: HUB_MAX_FEE_BPS,
            },
        ],
        loyal_actions::YieldRouteActionSeeds::default(),
    )
    .expect("build route actions with Jupiter and Loyal Hub swap lanes");
    let withdraw = route_action_setup.withdraw().expect("route has withdraw");
    let hub = route_action_setup.hub().expect("route has Loyal Hub swap");
    let deposit = route_action_setup.deposit().expect("route has deposit");
    try_send_instructions(
        &mut context.svm,
        &route_action_setup.instructions,
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
            withdraw,
            hub,
            deposit,
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
#[allow(clippy::too_many_arguments)]
fn build_hub_rebalance_transaction(
    vault: Pubkey,
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
    withdraw: KaminoAction,
    hub: HubAction,
    deposit: KaminoAction,
) -> HubTransition {
    let from_accounts = reserve_accounts[&from.reserve_index];
    let to_accounts = reserve_accounts[&to.reserve_index];
    let (withdraw_instructions, withdraw_accounts) = mock_kamino_reserve_transaction(
        vault,
        from_accounts,
        mock_kamino_withdraw_reserve_liquidity_data(in_amount_raw),
    );
    let withdraw_ix = withdraw.build(
        signer,
        vault_index,
        withdraw_instructions,
        withdraw_accounts,
    );

    if from.mint_address == to.mint_address {
        let (deposit_instructions, deposit_accounts) = mock_kamino_reserve_transaction(
            vault,
            to_accounts,
            mock_kamino_deposit_reserve_liquidity_data(in_amount_raw),
        );
        let deposit_ix = deposit.build(
            signer,
            vault_index,
            deposit_instructions,
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

    let swap_ix = hub.build(HubSwapExecution {
        signer,
        vault_index,
        vault,
        vault_input: vault_token_accounts[&from.mint_address],
        vault_output: vault_token_accounts[&to.mint_address],
        input_mint: from.mint_address,
        output_mint: to.mint_address,
        hub_authorizer,
        amount_in: in_amount_raw,
        amount_out: user_out_raw,
        min_out: user_out_raw,
        max_fee_bps: HUB_MAX_FEE_BPS,
        lane_id: 0,
    });
    let (deposit_instructions, deposit_accounts) = mock_kamino_reserve_transaction(
        vault,
        to_accounts,
        mock_kamino_deposit_reserve_liquidity_data(user_out_raw),
    );
    let deposit_ix = deposit.build(
        signer,
        vault_index,
        deposit_instructions,
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
                data: loyal_hub_withdraw_inventory_data(hub_input_amount, 0),
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
    set_spl_token_amount(svm, accounts.reserve_collateral_supply, amount_raw);
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
            get_spl_token_amount(svm, accounts.reserve_collateral_supply),
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
