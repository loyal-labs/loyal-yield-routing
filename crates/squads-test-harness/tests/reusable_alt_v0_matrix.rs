use loyal_actions::{
    compile_squads_inner_instruction, compiler_lookup_eligible_addresses,
    create_all_in_one_market_mint_yield_route_action, derive_kamino_user_metadata,
    derive_kamino_vanilla_obligation, execute_program_interaction_policy_instruction,
    kamino_init_obligation_farm_instruction, KaminoInitObligationFarm, LookupTableAccountAccess,
    YieldRouteActionSetup, YieldRouteInstruction, YieldRouteInstructionPlan,
    YieldRouteLookupTableRequirements, KAMINO_COLLATERAL_FARM_MODE, KAMINO_FARMS_PROGRAM_ID,
    KAMINO_INIT_OBLIGATION_DISCRIMINATOR, KAMINO_LEND_PROGRAM_ID,
    KAMINO_REFRESH_OBLIGATION_DISCRIMINATOR, KAMINO_VANILLA_OBLIGATION_ID,
    KAMINO_VANILLA_OBLIGATION_TAG,
};
use serde::Serialize;
use solana_program_runtime::declare_process_instruction;
use solana_sdk::{
    account::Account,
    address_lookup_table::{
        program as address_lookup_table_program,
        state::{AddressLookupTable, LookupTableMeta},
        AddressLookupTableAccount,
    },
    compute_budget::ComputeBudgetInstruction,
    instruction::{AccountMeta, Instruction},
    message::{v0, VersionedMessage},
    packet::PACKET_DATA_SIZE,
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
    transaction::VersionedTransaction,
};
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs,
    execute_squads_sync_transaction_instruction, get_spl_token_amount, loyal_action_context,
    mock_kamino_deposit_reserve_liquidity_data, mock_kamino_reserve_route_part,
    mock_kamino_withdraw_reserve_liquidity_data, seed_empty_system_account_if_missing,
    seed_mock_kamino_reserve_spl_accounts, seed_spl_token_account,
    yield_route_universe_from_mock_reserves, MockProgram, RouteActionExt,
    SquadsCompiledInstruction, KAMINO_MAIN_MARKET, KAMINO_MAIN_USDC_RESERVE, KAMINO_PRIME_MARKET,
    KAMINO_PRIME_USDC_RESERVE, LAMPORTS_PER_SOL, USDC_MINT,
};
use std::{
    borrow::Cow,
    collections::{BTreeMap, BTreeSet},
};

const ROUTE_AMOUNT: u64 = 1_000_000;
const KAMINO_MAX_OBLIGATION_RESERVES: usize = 20;
const REUSABLE_ALT_HARD_CAPACITY: usize = 256;
const DEFAULT_PROVISIONER_SAFETY_MARGIN: usize = 16;
const EXPECTED_FIXTURE_NAMES: [&str; 13] = [
    "policy_setup_operations",
    "idle_vault_deposit_without_setup",
    "ordinary_same_mint_source_withdrawal_target_deposit",
    "full_withdrawal",
    "full_withdrawal_cleanup",
    "setup_only_policy_operations",
    "missing_destination_obligation_setup",
    "missing_destination_later_route_execution",
    "obligation_farm_user_initialization",
    "idle_vault_deposit_with_setup_obligation_phase",
    "idle_vault_deposit_with_setup_farm_phase",
    "idle_vault_deposit_with_setup_deposit_phase",
    "widest_supported_reserve_refresh_account_set",
];

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct FixtureCatalogMeasurement {
    name: &'static str,
    shared_typed_address_count: usize,
    vault_typed_address_count: usize,
    single_class_expansion: usize,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct ReusableAltCatalogSummary<'a> {
    schema_version: u8,
    fixture_count: usize,
    expected_fixture_count: usize,
    hard_capacity: usize,
    largest_atomic_expansion: usize,
    default_safety_margin: usize,
    allocation_high_water: usize,
    provisioner_cli_args: Vec<String>,
    fixtures: &'a [FixtureCatalogMeasurement],
}

// The real Kamino mock is used for balance-changing deposit/withdraw fixtures.
// Setup-only fixtures switch the same program id to this deterministic builtin
// so LiteSVM still exercises Squads CPI, account loading, and the exact Kamino
// instruction layouts without pretending to implement Kamino account creation.
declare_process_instruction!(NoopKaminoEntrypoint, 1, |_invoke_context| { Ok(()) });

fn route_instruction_plan(
    setup: &YieldRouteActionSetup,
    instructions: impl IntoIterator<Item = YieldRouteInstruction>,
) -> YieldRouteInstructionPlan {
    let mut plan =
        YieldRouteInstructionPlan::with_outer_context(setup.lookup_table_requirements().clone());
    for instruction in instructions {
        plan.push(instruction)
            .expect("builder-owned route requirements must be disjoint");
    }
    plan
}

fn policy_setup_instruction_plan(setup: &YieldRouteActionSetup) -> YieldRouteInstructionPlan {
    let mut plan =
        YieldRouteInstructionPlan::with_outer_context(setup.lookup_table_requirements().clone());
    plan.push_outer_instruction(ComputeBudgetInstruction::request_heap_frame(
        squads_test_harness::SQUADS_EXTENDED_HEAP_FRAME_BYTES,
    ));
    for instruction in &setup.instructions {
        plan.push_outer_instruction(instruction.clone());
    }
    plan
}

fn vault_token_cleanup_instruction(
    instruction: Instruction,
    vault_token_account: Pubkey,
) -> YieldRouteInstruction {
    let mut requirements = YieldRouteLookupTableRequirements::default();
    requirements.add_vault_token_account(vault_token_account);
    requirements.add_infrastructure(spl_token::id());
    YieldRouteInstruction::new(instruction, requirements)
}

#[test]
fn reusable_alt_v0_matrix_compiles_covers_and_executes_every_earn_shape() {
    let mut context =
        create_funded_squads_test_context_with_mock_programs(&[MockProgram::KaminoLend])
            .expect("create funded Squads test context")
            .expect("committed Squads SBF fixture should load");

    let delegated = Keypair::new();
    context
        .svm
        .airdrop(&delegated.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("fund delegated route signer");

    let vault_liquidity = Pubkey::new_unique();
    let vault_main_collateral = Pubkey::new_unique();
    let vault_prime_collateral = Pubkey::new_unique();
    let vault_cleanup_token = Pubkey::new_unique();
    let main_liquidity_supply = Pubkey::new_unique();
    let prime_liquidity_supply = Pubkey::new_unique();

    seed_spl_token_account(
        &mut context.svm,
        vault_liquidity,
        USDC_MINT,
        context.vault,
        ROUTE_AMOUNT,
    );
    seed_spl_token_account(
        &mut context.svm,
        vault_cleanup_token,
        USDC_MINT,
        context.vault,
        0,
    );
    let main = seed_mock_kamino_reserve_spl_accounts(
        &mut context.svm,
        KAMINO_MAIN_USDC_RESERVE,
        KAMINO_MAIN_MARKET,
        context.vault,
        vault_liquidity,
        vault_main_collateral,
        main_liquidity_supply,
    );
    let prime = seed_mock_kamino_reserve_spl_accounts(
        &mut context.svm,
        KAMINO_PRIME_USDC_RESERVE,
        KAMINO_PRIME_MARKET,
        context.vault,
        vault_liquidity,
        vault_prime_collateral,
        prime_liquidity_supply,
    );

    let route_setup = create_all_in_one_market_mint_yield_route_action(
        loyal_action_context(&context, delegated.pubkey()),
        yield_route_universe_from_mock_reserves(vec![USDC_MINT], vec![main, prime]),
        Vec::new(),
    )
    .expect("build production all-in-one same-mint action");
    let mut catalog_measurements = Vec::with_capacity(EXPECTED_FIXTURE_NAMES.len());
    // Policy/setup operations use the same v0 compiler and seeded ALT accounts.
    let policy_setup_plan = policy_setup_instruction_plan(&route_setup);
    catalog_measurements.push(prove_and_execute_v0(
        "policy_setup_operations",
        &mut context.svm,
        &context.wallet,
        &policy_setup_plan,
    ));

    // Idle-vault deposit without setup: the obligation-side setup is already
    // complete and only the authorized target deposit is submitted.
    let main_deposit = mock_kamino_reserve_route_part(
        context.vault,
        main,
        mock_kamino_deposit_reserve_liquidity_data(ROUTE_AMOUNT),
    );
    let idle_without_setup = route_setup
        .deposit()
        .expect("route has deposit action")
        .build_with_lookup_table_requirements(
            delegated.pubkey(),
            context.vault_index,
            main_deposit,
        );
    let idle_without_setup_plan = route_instruction_plan(&route_setup, [idle_without_setup]);
    catalog_measurements.push(prove_and_execute_v0(
        "idle_vault_deposit_without_setup",
        &mut context.svm,
        &delegated,
        &idle_without_setup_plan,
    ));
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_main_collateral),
        ROUTE_AMOUNT
    );

    // Ordinary same-mint source withdrawal plus target deposit runs through
    // the real Loyal Actions coalesced route and the real mock Kamino program.
    let main_withdraw = mock_kamino_reserve_route_part(
        context.vault,
        main,
        mock_kamino_withdraw_reserve_liquidity_data(ROUTE_AMOUNT),
    );
    let prime_deposit = mock_kamino_reserve_route_part(
        context.vault,
        prime,
        mock_kamino_deposit_reserve_liquidity_data(ROUTE_AMOUNT),
    );
    let ordinary_route = route_setup
        .same_mint_route_action()
        .expect("route has same-mint action")
        .build_with_lookup_table_requirements(
            delegated.pubkey(),
            context.vault_index,
            main_withdraw,
            prime_deposit,
        )
        .expect("same-mint mock route requirements must compose");
    let ordinary_route_plan = route_instruction_plan(&route_setup, [ordinary_route]);
    catalog_measurements.push(prove_and_execute_v0(
        "ordinary_same_mint_source_withdrawal_target_deposit",
        &mut context.svm,
        &delegated,
        &ordinary_route_plan,
    ));
    assert_eq!(get_spl_token_amount(&context.svm, vault_main_collateral), 0);
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_prime_collateral),
        ROUTE_AMOUNT
    );

    // Full withdrawal and cleanup of a separate empty vault-owned token account
    // are distinct runtime phases. K-Lend's reserve collateral supply is
    // protocol-owned and must never be treated as a vault-closeable account.
    let prime_withdraw = mock_kamino_reserve_route_part(
        context.vault,
        prime,
        mock_kamino_withdraw_reserve_liquidity_data(ROUTE_AMOUNT),
    );
    let full_withdrawal = route_setup
        .withdraw()
        .expect("route has withdraw action")
        .build_with_lookup_table_requirements(
            delegated.pubkey(),
            context.vault_index,
            prime_withdraw,
        );
    let full_withdrawal_plan = route_instruction_plan(&route_setup, [full_withdrawal]);
    catalog_measurements.push(prove_and_execute_v0(
        "full_withdrawal",
        &mut context.svm,
        &delegated,
        &full_withdrawal_plan,
    ));
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_prime_collateral),
        0
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_liquidity),
        ROUTE_AMOUNT
    );

    let close_collateral = spl_token::instruction::close_account(
        &spl_token::id(),
        &vault_cleanup_token,
        &context.vault,
        &context.vault,
        &[],
    )
    .expect("build SPL collateral-account cleanup");
    let (cleanup_compiled, cleanup_accounts) = compile_squads_instructions(&[close_collateral]);
    let cleanup = execute_squads_sync_transaction_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        context.vault_index,
        cleanup_compiled,
        cleanup_accounts,
    );
    let cleanup = vault_token_cleanup_instruction(cleanup, vault_cleanup_token);
    let cleanup_plan = route_instruction_plan(&route_setup, [cleanup]);
    catalog_measurements.push(prove_and_execute_v0(
        "full_withdrawal_cleanup",
        &mut context.svm,
        &context.wallet,
        &cleanup_plan,
    ));

    // From here onward a fresh SVM uses the exact production Kamino
    // instruction layouts through Squads where applicable, while a
    // deterministic builtin stands in for Kamino's account-creation/refresh
    // handlers. A fresh context is required because LiteSVM deliberately does
    // not replace an already-deployed SBF program-cache entry in place.
    let mut context = create_funded_squads_test_context_with_mock_programs(&[])
        .expect("create setup-only Squads context")
        .expect("committed Squads SBF fixture should load");
    context
        .svm
        .add_builtin(KAMINO_LEND_PROGRAM_ID, NoopKaminoEntrypoint::vm);
    // LiteSVM 0.7 registers custom builtins in the program cache but creates
    // their executable account under the BPF loader. With the current feature
    // set, native-loader ownership is what makes the runtime key that cache
    // entry by this custom program id (including for Squads CPI).
    let mut noop_program_account = context
        .svm
        .get_account(&KAMINO_LEND_PROGRAM_ID)
        .expect("custom Kamino builtin account exists");
    noop_program_account.owner = solana_sdk::native_loader::ID;
    context
        .svm
        .set_account(KAMINO_LEND_PROGRAM_ID, noop_program_account)
        .expect("register custom Kamino builtin as native-loader program");
    let delegated = Keypair::new();
    context
        .svm
        .airdrop(&delegated.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("fund setup-only delegated signer");

    let vault_liquidity = Pubkey::new_unique();
    let vault_main_collateral = Pubkey::new_unique();
    let vault_prime_collateral = Pubkey::new_unique();
    let main = seed_mock_kamino_reserve_spl_accounts(
        &mut context.svm,
        KAMINO_MAIN_USDC_RESERVE,
        KAMINO_MAIN_MARKET,
        context.vault,
        vault_liquidity,
        vault_main_collateral,
        Pubkey::new_unique(),
    );
    let prime = seed_mock_kamino_reserve_spl_accounts(
        &mut context.svm,
        KAMINO_PRIME_USDC_RESERVE,
        KAMINO_PRIME_MARKET,
        context.vault,
        vault_liquidity,
        vault_prime_collateral,
        Pubkey::new_unique(),
    );
    let route_setup = create_all_in_one_market_mint_yield_route_action(
        loyal_action_context(&context, delegated.pubkey()),
        yield_route_universe_from_mock_reserves(vec![USDC_MINT], vec![main, prime]),
        Vec::new(),
    )
    .expect("build setup-only all-in-one same-mint action");
    let policy = route_setup.accounts.withdraw;
    let setup_policy_plan = policy_setup_instruction_plan(&route_setup);
    catalog_measurements.push(prove_and_execute_v0(
        "setup_only_policy_operations",
        &mut context.svm,
        &context.wallet,
        &setup_policy_plan,
    ));

    let target_obligation = derive_kamino_vanilla_obligation(context.vault, prime.market);
    let target_metadata = derive_kamino_user_metadata(context.vault);
    seed_empty_system_account_if_missing(&mut context.svm, target_obligation);
    seed_empty_system_account_if_missing(&mut context.svm, target_metadata);

    // Missing destination obligation setup uses the production Loyal Actions
    // compiler and policy envelope, followed by a distinct later route tx.
    let init_obligation = kamino_init_obligation_instruction(context.vault, prime.market);
    let init_via_policy = init_obligation_policy_execution(
        policy,
        delegated.pubkey(),
        context.vault_index,
        init_obligation,
    );
    let init_obligation_plan = route_instruction_plan(&route_setup, [init_via_policy]);
    catalog_measurements.push(prove_and_execute_v0(
        "missing_destination_obligation_setup",
        &mut context.svm,
        &delegated,
        &init_obligation_plan,
    ));

    seed_empty_system_account_if_missing(&mut context.svm, vault_prime_collateral);
    let later_withdraw = mock_kamino_reserve_route_part(
        context.vault,
        main,
        mock_kamino_withdraw_reserve_liquidity_data(ROUTE_AMOUNT),
    );
    let later_deposit = mock_kamino_reserve_route_part(
        context.vault,
        prime,
        mock_kamino_deposit_reserve_liquidity_data(ROUTE_AMOUNT),
    );
    let later_route = route_setup
        .same_mint_route_action()
        .expect("route has later same-mint action")
        .build_with_lookup_table_requirements(
            delegated.pubkey(),
            context.vault_index,
            later_withdraw,
            later_deposit,
        )
        .expect("later route requirements must compose");
    let later_route_plan = route_instruction_plan(&route_setup, [later_route]);
    catalog_measurements.push(prove_and_execute_v0(
        "missing_destination_later_route_execution",
        &mut context.svm,
        &delegated,
        &later_route_plan,
    ));

    // Obligation farm-user initialization is the exact Loyal Actions Kamino
    // builder used by Earn, including the Farms program and derived farm PDA.
    let reserve_farm_state = Pubkey::new_unique();
    let farm_user_state = loyal_actions::derive_kamino_obligation_farm_user_state(
        reserve_farm_state,
        target_obligation,
    );
    seed_empty_system_account_if_missing(&mut context.svm, reserve_farm_state);
    seed_empty_system_account_if_missing(&mut context.svm, farm_user_state);
    seed_empty_system_account_if_missing(&mut context.svm, KAMINO_FARMS_PROGRAM_ID);
    let farm_init = kamino_farm_init_instruction(KaminoInitObligationFarm {
        payer: delegated.pubkey(),
        owner: context.vault,
        lending_market: prime.market,
        reserve: prime.reserve,
        reserve_farm_state,
    });
    assert_eq!(
        farm_init.instruction().data.last().copied(),
        Some(KAMINO_COLLATERAL_FARM_MODE)
    );
    let farm_init_plan = route_instruction_plan(&route_setup, [farm_init]);
    catalog_measurements.push(prove_and_execute_v0(
        "obligation_farm_user_initialization",
        &mut context.svm,
        &delegated,
        &farm_init_plan,
    ));

    // Idle-vault deposit with setup is intentionally phased like production:
    // obligation setup, optional farm setup, then a later deposit. Every phase
    // independently satisfies the compiler/packet/simulation contract.
    let idle_obligation = derive_kamino_vanilla_obligation(context.vault, main.market);
    let idle_setup_obligation = init_obligation_policy_execution(
        policy,
        delegated.pubkey(),
        context.vault_index,
        kamino_init_obligation_instruction(context.vault, main.market),
    );
    let idle_setup_obligation_plan = route_instruction_plan(&route_setup, [idle_setup_obligation]);
    catalog_measurements.push(prove_and_execute_v0(
        "idle_vault_deposit_with_setup_obligation_phase",
        &mut context.svm,
        &delegated,
        &idle_setup_obligation_plan,
    ));

    let idle_farm_state = Pubkey::new_unique();
    let idle_farm_user_state =
        loyal_actions::derive_kamino_obligation_farm_user_state(idle_farm_state, idle_obligation);
    seed_empty_system_account_if_missing(&mut context.svm, idle_obligation);
    seed_empty_system_account_if_missing(&mut context.svm, idle_farm_state);
    seed_empty_system_account_if_missing(&mut context.svm, idle_farm_user_state);
    let idle_farm_init = kamino_farm_init_instruction(KaminoInitObligationFarm {
        payer: delegated.pubkey(),
        owner: context.vault,
        lending_market: main.market,
        reserve: main.reserve,
        reserve_farm_state: idle_farm_state,
    });
    let idle_farm_init_plan = route_instruction_plan(&route_setup, [idle_farm_init]);
    catalog_measurements.push(prove_and_execute_v0(
        "idle_vault_deposit_with_setup_farm_phase",
        &mut context.svm,
        &delegated,
        &idle_farm_init_plan,
    ));

    let idle_deposit_inner = mock_kamino_reserve_route_part(
        context.vault,
        main,
        mock_kamino_deposit_reserve_liquidity_data(ROUTE_AMOUNT),
    );
    let idle_deposit_after_setup = route_setup
        .deposit()
        .expect("route has post-setup deposit action")
        .build_with_lookup_table_requirements(
            delegated.pubkey(),
            context.vault_index,
            idle_deposit_inner,
        );
    let idle_deposit_plan = route_instruction_plan(&route_setup, [idle_deposit_after_setup]);
    catalog_measurements.push(prove_and_execute_v0(
        "idle_vault_deposit_with_setup_deposit_phase",
        &mut context.svm,
        &delegated,
        &idle_deposit_plan,
    ));

    // Kamino's supported obligation width is twenty deposit/borrow reserve
    // entries. This uses the exact refresh discriminator and the production
    // writable remaining-account shape at that maximum.
    let refresh_obligation = Pubkey::new_unique();
    seed_empty_system_account_if_missing(&mut context.svm, refresh_obligation);
    let widest_reserves = (0..KAMINO_MAX_OBLIGATION_RESERVES)
        .map(|_| Pubkey::new_unique())
        .collect::<Vec<_>>();
    for reserve in &widest_reserves {
        seed_empty_system_account_if_missing(&mut context.svm, *reserve);
    }
    let widest_refresh =
        kamino_refresh_obligation_instruction(main.market, refresh_obligation, &widest_reserves);
    let widest_refresh_plan = route_instruction_plan(&route_setup, [widest_refresh]);
    catalog_measurements.push(prove_and_execute_v0(
        "widest_supported_reserve_refresh_account_set",
        &mut context.svm,
        &delegated,
        &widest_refresh_plan,
    ));
    assert_and_report_catalog_measurements(&catalog_measurements);
}

fn kamino_init_obligation_instruction(vault: Pubkey, market: Pubkey) -> YieldRouteInstruction {
    let obligation = derive_kamino_vanilla_obligation(vault, market);
    let metadata = derive_kamino_user_metadata(vault);
    let mut data = KAMINO_INIT_OBLIGATION_DISCRIMINATOR.to_vec();
    data.push(KAMINO_VANILLA_OBLIGATION_TAG);
    data.push(KAMINO_VANILLA_OBLIGATION_ID);
    let instruction = Instruction {
        program_id: KAMINO_LEND_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new_readonly(vault, true),
            AccountMeta::new(vault, true),
            AccountMeta::new(obligation, false),
            AccountMeta::new_readonly(market, false),
            AccountMeta::new_readonly(Pubkey::default(), false),
            AccountMeta::new_readonly(Pubkey::default(), false),
            AccountMeta::new_readonly(metadata, false),
            AccountMeta::new_readonly(solana_sdk::sysvar::rent::id(), false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data,
    };
    let mut requirements = YieldRouteLookupTableRequirements::default();
    requirements.add_vault_account(vault);
    requirements.add_obligation(obligation);
    requirements.add_metadata(metadata);
    requirements.add_shared_market(market);
    requirements.add_infrastructure_accounts([
        KAMINO_LEND_PROGRAM_ID,
        solana_sdk::sysvar::rent::id(),
        solana_sdk::system_program::ID,
        Pubkey::default(),
    ]);
    YieldRouteInstruction::new(instruction, requirements)
}

fn init_obligation_policy_execution(
    policy: Pubkey,
    delegated_signer: Pubkey,
    vault_index: u8,
    init_instruction: YieldRouteInstruction,
) -> YieldRouteInstruction {
    let (init_instruction, mut requirements) = init_instruction.into_parts();
    let mut transaction_accounts = Vec::new();
    let compiled = compile_squads_inner_instruction(&mut transaction_accounts, init_instruction);
    let instruction = execute_program_interaction_policy_instruction(
        policy,
        delegated_signer,
        vault_index,
        vec![compiled],
        vec![2],
        transaction_accounts,
    );
    requirements.add_policy(policy);
    YieldRouteInstruction::new(instruction, requirements)
}

fn kamino_farm_init_instruction(args: KaminoInitObligationFarm) -> YieldRouteInstruction {
    let instruction = kamino_init_obligation_farm_instruction(args);
    let obligation = derive_kamino_vanilla_obligation(args.owner, args.lending_market);
    let farm_user_state = loyal_actions::derive_kamino_obligation_farm_user_state(
        args.reserve_farm_state,
        obligation,
    );
    let mut requirements = YieldRouteLookupTableRequirements::default();
    requirements.add_vault_account(args.owner);
    requirements.add_obligation(obligation);
    requirements.add_shared_market(args.lending_market);
    requirements.add_shared_market_authority(
        loyal_actions::derive_kamino_lending_market_authority(args.lending_market),
    );
    requirements.add_shared_reserve(args.reserve);
    requirements.add_kamino_farm(args.reserve_farm_state, farm_user_state);
    requirements.add_infrastructure_accounts([
        KAMINO_LEND_PROGRAM_ID,
        KAMINO_FARMS_PROGRAM_ID,
        solana_sdk::sysvar::rent::id(),
        solana_sdk::system_program::ID,
    ]);
    YieldRouteInstruction::new(instruction, requirements)
}

fn kamino_refresh_obligation_instruction(
    market: Pubkey,
    obligation: Pubkey,
    reserves: &[Pubkey],
) -> YieldRouteInstruction {
    let mut accounts = vec![
        AccountMeta::new_readonly(market, false),
        AccountMeta::new(obligation, false),
    ];
    accounts.extend(
        reserves
            .iter()
            .copied()
            .map(|reserve| AccountMeta::new(reserve, false)),
    );
    let instruction = Instruction {
        program_id: KAMINO_LEND_PROGRAM_ID,
        accounts,
        data: KAMINO_REFRESH_OBLIGATION_DISCRIMINATOR.to_vec(),
    };
    let mut requirements = YieldRouteLookupTableRequirements::default();
    requirements.add_shared_market(market);
    requirements.add_obligation(obligation);
    for reserve in reserves {
        requirements.add_shared_reserve(*reserve);
    }
    requirements.add_infrastructure(KAMINO_LEND_PROGRAM_ID);
    YieldRouteInstruction::new(instruction, requirements)
}

fn compile_squads_instructions(
    instructions: &[Instruction],
) -> (Vec<SquadsCompiledInstruction>, Vec<AccountMeta>) {
    let mut transaction_accounts = Vec::new();
    let mut compiled = Vec::with_capacity(instructions.len());
    for instruction in instructions {
        let accounts = instruction
            .accounts
            .iter()
            .cloned()
            .map(|meta| push_or_update_meta(&mut transaction_accounts, meta))
            .collect();
        let program_id_index = push_or_update_meta(
            &mut transaction_accounts,
            AccountMeta::new_readonly(instruction.program_id, false),
        );
        compiled.push(SquadsCompiledInstruction {
            program_id_index,
            accounts,
            data: instruction.data.clone(),
        });
    }
    // Squads signs its vault PDA for the inner instructions. The PDA must not
    // become an outer transaction signer.
    for account in &mut transaction_accounts {
        account.is_signer = false;
    }
    (compiled, transaction_accounts)
}

fn push_or_update_meta(accounts: &mut Vec<AccountMeta>, meta: AccountMeta) -> usize {
    if let Some(index) = accounts
        .iter()
        .position(|existing| existing.pubkey == meta.pubkey)
    {
        accounts[index].is_signer |= meta.is_signer;
        accounts[index].is_writable |= meta.is_writable;
        index
    } else {
        let index = accounts.len();
        accounts.push(meta);
        index
    }
}

fn prove_and_execute_v0(
    name: &'static str,
    svm: &mut litesvm::LiteSVM,
    payer: &Keypair,
    plan: &YieldRouteInstructionPlan,
) -> FixtureCatalogMeasurement {
    let instructions = plan.instructions();
    let eligible = compiler_lookup_eligible_addresses(payer.pubkey(), instructions);
    assert!(!eligible.is_empty(), "{name}: fixture must exercise ALTs");

    let manifest = plan
        .manifest(payer.pubkey())
        .unwrap_or_else(|error| panic!("{name}: exact manifest failed: {error}"));
    manifest
        .validate_against_instructions(payer.pubkey(), instructions)
        .unwrap_or_else(|error| panic!("{name}: manifest/compiler drift: {error}"));

    let static_addresses = manifest
        .must_remain_static()
        .iter()
        .map(|requirement| requirement.address)
        .collect::<BTreeSet<_>>();
    let shared_addresses = manifest
        .shared_market()
        .iter()
        .map(|requirement| requirement.address)
        .collect::<BTreeSet<_>>();
    let vault_addresses = manifest
        .vault()
        .iter()
        .map(|requirement| requirement.address)
        .collect::<BTreeSet<_>>();
    let shared_typed_address_count = shared_addresses.len();
    let vault_typed_address_count = vault_addresses.len();
    let single_class_expansion = shared_typed_address_count.max(vault_typed_address_count);
    assert!(
        static_addresses.is_disjoint(&shared_addresses)
            && static_addresses.is_disjoint(&vault_addresses)
            && shared_addresses.is_disjoint(&vault_addresses),
        "{name}: manifest classes must be pairwise disjoint"
    );
    let compiler_universe = required_account_universe(payer.pubkey(), instructions);
    let manifest_universe = static_addresses
        .union(&shared_addresses)
        .copied()
        .collect::<BTreeSet<_>>()
        .union(&vault_addresses)
        .copied()
        .collect::<BTreeSet<_>>();
    assert_eq!(
        manifest_universe, compiler_universe,
        "{name}: manifest classes must exactly equal the compiler universe"
    );
    let lookup_eligible_manifest = shared_addresses
        .union(&vault_addresses)
        .copied()
        .collect::<BTreeSet<_>>();
    assert_eq!(
        lookup_eligible_manifest,
        eligible.into_iter().collect::<BTreeSet<_>>(),
        "{name}: shared plus vault classes must exactly equal compiler ALT eligibility"
    );

    let mut selected_tables = Vec::new();
    let shared_addresses = manifest
        .shared_market()
        .iter()
        .map(|requirement| requirement.address)
        .collect::<Vec<_>>();
    if shared_addresses.len() > 1 {
        // The production shared-market family is append-packed across
        // durable physical shards. Put every shared requirement in a distinct
        // table here to prove the worst measured contributing-shard topology:
        // all Earn shapes must still compile, fit, simulate, and execute even
        // when every shared account arrived in a different historical shard.
        selected_tables.extend(shared_addresses.into_iter().map(|address| {
            AddressLookupTableAccount {
                key: Pubkey::new_unique(),
                addresses: vec![address],
            }
        }));
    } else if !shared_addresses.is_empty() {
        selected_tables.push(AddressLookupTableAccount {
            key: Pubkey::new_unique(),
            addresses: shared_addresses,
        });
    }
    let vault_addresses = manifest
        .vault()
        .iter()
        .map(|requirement| requirement.address)
        .collect::<Vec<_>>();
    if !vault_addresses.is_empty() {
        selected_tables.push(AddressLookupTableAccount {
            key: Pubkey::new_unique(),
            addresses: vault_addresses,
        });
    }
    assert!(
        !selected_tables.is_empty(),
        "{name}: resolver must select at least one contributing table"
    );

    for table in &selected_tables {
        seed_lookup_table_account(svm, table, payer.pubkey());
    }
    svm.expire_blockhash();
    let message = v0::Message::try_compile(
        &payer.pubkey(),
        instructions,
        &selected_tables,
        svm.latest_blockhash(),
    )
    .unwrap_or_else(|error| panic!("{name}: production v0 compile failed: {error}"));

    let access = manifest
        .shared_market()
        .iter()
        .map(|requirement| (requirement.address, requirement.access))
        .chain(
            manifest
                .vault()
                .iter()
                .map(|requirement| (requirement.address, requirement.access)),
        )
        .collect::<BTreeMap<_, _>>();
    assert_eq!(
        message.address_table_lookups.len(),
        selected_tables.len(),
        "{name}: selected tables with zero compiler contribution must be omitted"
    );
    let mut contribution_summary = Vec::new();
    for table in &selected_tables {
        let lookup = message
            .address_table_lookups
            .iter()
            .find(|lookup| lookup.account_key == table.key)
            .unwrap_or_else(|| panic!("{name}: selected table {} contributed zero", table.key));
        let mut expected_writable = Vec::new();
        let mut expected_readonly = Vec::new();
        for (index, address) in table.addresses.iter().enumerate() {
            let index = u8::try_from(index).expect("fixture ALT index fits in u8");
            match access[address] {
                LookupTableAccountAccess::Writable => expected_writable.push(index),
                LookupTableAccountAccess::Readonly => expected_readonly.push(index),
            }
        }
        assert_eq!(
            lookup.writable_indexes, expected_writable,
            "{name}: exact writable lookup indexes"
        );
        assert_eq!(
            lookup.readonly_indexes, expected_readonly,
            "{name}: exact readonly lookup indexes"
        );
        assert!(
            !lookup.writable_indexes.is_empty() || !lookup.readonly_indexes.is_empty(),
            "{name}: no selected ALT may have zero contribution"
        );
        contribution_summary.push(format!(
            "table={} w{:?}/r{:?}",
            table.key, lookup.writable_indexes, lookup.readonly_indexes
        ));
    }

    let required = required_account_universe(payer.pubkey(), instructions);
    let loaded = loaded_account_universe(&message, &selected_tables);
    assert_eq!(
        loaded, required,
        "{name}: static plus loaded ALT keys must exactly cover every required address"
    );
    let static_manifest = manifest
        .must_remain_static()
        .iter()
        .map(|requirement| requirement.address)
        .collect::<BTreeSet<_>>();
    assert_eq!(
        message
            .account_keys
            .iter()
            .copied()
            .collect::<BTreeSet<_>>(),
        static_manifest,
        "{name}: only compiler-required static keys may remain static"
    );
    assert!(
        loaded.len() <= 256,
        "{name}: v0 account indexes must fit in u8"
    );

    let transaction =
        VersionedTransaction::try_new(VersionedMessage::V0(message.clone()), &[payer])
            .unwrap_or_else(|error| panic!("{name}: sign v0 fixture: {error}"));
    let serialized = bincode::serialize(&transaction)
        .unwrap_or_else(|error| panic!("{name}: serialize v0 transaction: {error}"));
    assert!(
        serialized.len() < PACKET_DATA_SIZE,
        "{name}: serialized packet {} must be below {} bytes",
        serialized.len(),
        PACKET_DATA_SIZE
    );

    let simulation = svm
        .simulate_transaction(transaction.clone())
        .unwrap_or_else(|error| panic!("{name}: LiteSVM simulation failed: {error:?}"));
    assert!(simulation.meta.compute_units_consumed > 0);
    svm.send_transaction(transaction)
        .unwrap_or_else(|error| panic!("{name}: LiteSVM execution failed: {error:?}"));

    eprintln!(
        "reusable_alt_v0_fixture name={name} shared_typed_addresses={shared_typed_address_count} vault_typed_addresses={vault_typed_address_count} single_class_expansion={single_class_expansion} static_keys={} alt_contribution={} unique_keys={} packet_bytes={} simulation=ok execution=ok coverage=exact",
        message.account_keys.len(),
        contribution_summary.join(","),
        loaded.len(),
        serialized.len(),
    );

    FixtureCatalogMeasurement {
        name,
        shared_typed_address_count,
        vault_typed_address_count,
        single_class_expansion,
    }
}

fn assert_and_report_catalog_measurements(measurements: &[FixtureCatalogMeasurement]) {
    assert_eq!(
        measurements.len(),
        EXPECTED_FIXTURE_NAMES.len(),
        "reusable ALT catalog must measure every supported fixture"
    );
    assert_eq!(
        measurements
            .iter()
            .map(|measurement| measurement.name)
            .collect::<Vec<_>>(),
        EXPECTED_FIXTURE_NAMES,
        "reusable ALT catalog fixture order and membership must remain explicit"
    );
    assert_eq!(
        measurements
            .iter()
            .map(|measurement| measurement.name)
            .collect::<BTreeSet<_>>()
            .len(),
        EXPECTED_FIXTURE_NAMES.len(),
        "reusable ALT catalog fixture names must be unique"
    );
    for measurement in measurements {
        assert_eq!(
            measurement.single_class_expansion,
            measurement
                .shared_typed_address_count
                .max(measurement.vault_typed_address_count),
            "{}: single-class expansion must be derived from typed manifest counts",
            measurement.name
        );
        assert!(
            measurement.single_class_expansion > 0
                && measurement.single_class_expansion < REUSABLE_ALT_HARD_CAPACITY,
            "{}: measured expansion must fit one physical ALT",
            measurement.name
        );
    }

    let largest_atomic_expansion = measurements
        .iter()
        .map(|measurement| measurement.single_class_expansion)
        .max()
        .expect("the explicit fixture catalog is non-empty");
    let allocation_high_water = REUSABLE_ALT_HARD_CAPACITY
        .checked_sub(largest_atomic_expansion)
        .and_then(|remaining| remaining.checked_sub(DEFAULT_PROVISIONER_SAFETY_MARGIN))
        .expect("measured expansion plus default safety margin must fit ALT capacity");
    assert!(
        allocation_high_water > 0
            && allocation_high_water + largest_atomic_expansion + DEFAULT_PROVISIONER_SAFETY_MARGIN
                == REUSABLE_ALT_HARD_CAPACITY,
        "catalog high-water must equal capacity minus measured expansion and safety margin"
    );

    let summary = ReusableAltCatalogSummary {
        schema_version: 1,
        fixture_count: measurements.len(),
        expected_fixture_count: EXPECTED_FIXTURE_NAMES.len(),
        hard_capacity: REUSABLE_ALT_HARD_CAPACITY,
        largest_atomic_expansion,
        default_safety_margin: DEFAULT_PROVISIONER_SAFETY_MARGIN,
        allocation_high_water,
        provisioner_cli_args: vec![
            "--largest-atomic-expansion".to_owned(),
            largest_atomic_expansion.to_string(),
            "--safety-margin".to_owned(),
            DEFAULT_PROVISIONER_SAFETY_MARGIN.to_string(),
        ],
        fixtures: measurements,
    };
    eprintln!(
        "reusable_alt_catalog_summary={}",
        serde_json::to_string(&summary).expect("catalog summary must serialize")
    );
}

fn seed_lookup_table_account(
    svm: &mut litesvm::LiteSVM,
    table: &AddressLookupTableAccount,
    authority: Pubkey,
) {
    assert!(table.addresses.len() <= 255);
    let mut meta = LookupTableMeta::new(authority);
    // These entries represent an already-warmed durable table. Setting the
    // current-slot prefix to the full length also keeps the fixture valid when
    // LiteSVM's clock begins at slot zero.
    meta.last_extended_slot_start_index = table.addresses.len() as u8;
    let data = AddressLookupTable {
        meta,
        addresses: Cow::Owned(table.addresses.clone()),
    }
    .serialize_for_tests()
    .expect("serialize on-chain ALT account data");
    let lamports = svm.minimum_balance_for_rent_exemption(data.len());
    svm.set_account(
        table.key,
        Account {
            lamports,
            data,
            owner: address_lookup_table_program::ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed on-chain-format ALT account");
}

fn required_account_universe(payer: Pubkey, instructions: &[Instruction]) -> BTreeSet<Pubkey> {
    let mut required = BTreeSet::from([payer]);
    for instruction in instructions {
        required.insert(instruction.program_id);
        required.extend(instruction.accounts.iter().map(|meta| meta.pubkey));
    }
    required
}

fn loaded_account_universe(
    message: &v0::Message,
    tables: &[AddressLookupTableAccount],
) -> BTreeSet<Pubkey> {
    let mut loaded = message
        .account_keys
        .iter()
        .copied()
        .collect::<BTreeSet<_>>();
    for lookup in &message.address_table_lookups {
        let table = tables
            .iter()
            .find(|table| table.key == lookup.account_key)
            .expect("compiled lookup references selected table");
        loaded.extend(
            lookup
                .writable_indexes
                .iter()
                .chain(&lookup.readonly_indexes)
                .map(|index| table.addresses[*index as usize]),
        );
    }
    loaded
}
