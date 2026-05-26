const AMOUNT_IN: u64 = 1_000_000;
const HUB_OUT: u64 = 999_000;
const MIN_OUT: u64 = 998_000;
const MAX_FEE_BPS: u16 = 10;
const TREASURY_SEED: u128 = 2;

struct HubSwapFixture {
    context: squads_test_harness::FundedSquadsTestContext,
    wallet_b: Keypair,
    hub_authorizer: Keypair,
    swap_action: YieldRouteActionInstruction,
    vault_usdc: solana_sdk::pubkey::Pubkey,
    vault_pyusd: solana_sdk::pubkey::Pubkey,
}
struct TreasurySquads {
    pool: SquadsPool,
    vault_index: u8,
    vault: Pubkey,
    usdc: Pubkey,
    pyusd: Pubkey,
}

fn setup_fixture(with_jupiter: bool) -> Option<HubSwapFixture> {
    let mock_programs = if with_jupiter {
        vec![MockProgram::LoyalHubSwap, MockProgram::Jupiter]
    } else {
        vec![MockProgram::LoyalHubSwap]
    };
    let mut context = create_funded_squads_test_context_with_mock_programs(&mock_programs)
        .expect("create funded Squads test context")?;

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

    seed_spl_mint_if_missing(&mut context.svm, USDC_MINT, None, USDC_DECIMALS, 0);
    seed_spl_mint_if_missing(&mut context.svm, PYUSD_MINT, None, PYUSD_DECIMALS, 0);
    let vault_usdc = Keypair::new().pubkey();
    let vault_pyusd = Keypair::new().pubkey();
    seed_spl_token_account(
        &mut context.svm,
        vault_usdc,
        USDC_MINT,
        context.vault,
        AMOUNT_IN,
    );
    seed_spl_token_account(&mut context.svm, vault_pyusd, PYUSD_MINT, context.vault, 0);
    seed_loyal_hub_inventory_spl_accounts(
        &mut context.svm,
        &[USDC_MINT, PYUSD_MINT],
        AMOUNT_IN * 2,
    );

    let init_hub_ix = initialize_loyal_hub_config_instruction(
        context.wallet_pubkey(),
        context.wallet_pubkey(),
        hub_authorizer.pubkey(),
        50,
        false,
        &[USDC_MINT, PYUSD_MINT],
    );
    try_send_instructions(&mut context.svm, &[init_hub_ix], &context.wallet, &[])
        .expect("initialize Loyal Hub config");

    let swap_action = create_swap_yield_route_action(
        loyal_action_context(&context, wallet_b.pubkey()),
        vec![USDC_MINT, PYUSD_MINT],
        vec![
            mock_jupiter_swap_lane(true),
            SwapLane::LoyalHub {
                hub_authorizer: hub_authorizer.pubkey(),
                max_fee_bps: 50,
            },
        ],
        YIELD_ROUTE_STANDALONE_ACTION_SEED,
    )
    .expect("build LoyalHub/Jupiter swap action");
    try_send_instructions(
        &mut context.svm,
        &[swap_action.instruction.clone()],
        &context.wallet,
        &[],
    )
    .expect("create LoyalHub/Jupiter swap policy");

    Some(HubSwapFixture {
        context,
        wallet_b,
        hub_authorizer,
        swap_action,
        vault_usdc,
        vault_pyusd,
    })
}

fn hub_swap_ix(fixture: &HubSwapFixture, amount_in: u64, amount_out: u64) -> Instruction {
    fixture
        .swap_action
        .hub()
        .expect("swap action has Loyal Hub lane")
        .build(HubSwapExecution {
            signer: fixture.wallet_b.pubkey(),
            vault_index: fixture.context.vault_index,
            vault: fixture.context.vault,
            vault_input: fixture.vault_usdc,
            vault_output: fixture.vault_pyusd,
            input_mint: USDC_MINT,
            output_mint: PYUSD_MINT,
            hub_authorizer: fixture.hub_authorizer.pubkey(),
            amount_in,
            amount_out,
            min_out: MIN_OUT.min(amount_out),
            max_fee_bps: MAX_FEE_BPS,
        })
}

fn create_treasury_squads(
    context: &mut squads_test_harness::FundedSquadsTestContext,
    treasury_executor: Pubkey,
    usdc_amount: u64,
    pyusd_amount: u64,
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

    let usdc = Keypair::new().pubkey();
    let pyusd = Keypair::new().pubkey();
    seed_spl_token_account(&mut context.svm, usdc, USDC_MINT, vault, usdc_amount);
    seed_spl_token_account(&mut context.svm, pyusd, PYUSD_MINT, vault, pyusd_amount);

    TreasurySquads {
        pool,
        vault_index,
        vault,
        usdc,
        pyusd,
    }
}

fn treasury_initialize_hub_ix(
    treasury: &TreasurySquads,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
) -> Instruction {
    let init_ix = initialize_loyal_hub_config_instruction(
        treasury.vault,
        treasury.vault,
        hub_authorizer,
        max_fee_bps,
        false,
        &[USDC_MINT, PYUSD_MINT],
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

fn treasury_top_up_hub_ix(
    treasury: &TreasurySquads,
    signer: Pubkey,
    treasury_source: Pubkey,
    mint: Pubkey,
    hub_destination: Pubkey,
    amount: u64,
    decimals: u8,
) -> Instruction {
    let transfer_ix = spl_token::instruction::transfer_checked(
        &spl_token::id(),
        &treasury_source,
        &mint,
        &hub_destination,
        &treasury.vault,
        &[],
        amount,
        decimals,
    )
    .expect("build treasury top-up transfer_checked");

    execute_squads_sync_transaction_instruction(
        treasury.pool.settings,
        signer,
        treasury.vault_index,
        vec![SquadsCompiledInstruction {
            program_id_index: 4,
            accounts: vec![0, 1, 2, 3],
            data: transfer_ix.data,
        }],
        vec![
            AccountMeta::new(treasury_source, false),
            AccountMeta::new_readonly(mint, false),
            AccountMeta::new(hub_destination, false),
            AccountMeta::new_readonly(treasury.vault, false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
    )
}

fn treasury_withdraw_hub_ix(
    treasury: &TreasurySquads,
    signer: Pubkey,
    hub_source: Pubkey,
    treasury_destination: Pubkey,
    mint: Pubkey,
    amount: u64,
) -> Instruction {
    execute_squads_sync_transaction_instruction(
        treasury.pool.settings,
        signer,
        treasury.vault_index,
        vec![SquadsCompiledInstruction {
            program_id_index: 7,
            accounts: vec![0, 1, 2, 3, 4, 5, 6],
            data: loyal_hub_withdraw_inventory_data(amount),
        }],
        vec![
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(treasury.vault, false),
            AccountMeta::new(hub_source, false),
            AccountMeta::new(treasury_destination, false),
            AccountMeta::new_readonly(mint, false),
            AccountMeta::new_readonly(derive_loyal_hub_authority(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(LOYAL_HUB_SWAP_PROGRAM_ID, false),
        ],
    )
}

fn treasury_rebalance_hub_ix(
    treasury: &TreasurySquads,
    signer: Pubkey,
    withdraw_usdc: u64,
    top_up_pyusd: u64,
) -> Instruction {
    let withdraw_data = loyal_hub_withdraw_inventory_data(withdraw_usdc);
    let top_up_data = spl_token::instruction::transfer_checked(
        &spl_token::id(),
        &treasury.pyusd,
        &PYUSD_MINT,
        &loyal_hub_token_account(PYUSD_MINT),
        &treasury.vault,
        &[],
        top_up_pyusd,
        PYUSD_DECIMALS,
    )
    .expect("build treasury rebalance transfer_checked")
    .data;

    execute_squads_sync_transaction_instruction(
        treasury.pool.settings,
        signer,
        treasury.vault_index,
        vec![
            SquadsCompiledInstruction {
                program_id_index: 7,
                accounts: vec![0, 1, 2, 3, 4, 5, 6],
                data: withdraw_data,
            },
            SquadsCompiledInstruction {
                program_id_index: 6,
                accounts: vec![8, 9, 10, 1],
                data: top_up_data,
            },
        ],
        vec![
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(treasury.vault, false),
            AccountMeta::new(loyal_hub_token_account(USDC_MINT), false),
            AccountMeta::new(treasury.usdc, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new_readonly(derive_loyal_hub_authority(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(LOYAL_HUB_SWAP_PROGRAM_ID, false),
            AccountMeta::new(treasury.pyusd, false),
            AccountMeta::new_readonly(PYUSD_MINT, false),
            AccountMeta::new(loyal_hub_token_account(PYUSD_MINT), false),
        ],
    )
}
