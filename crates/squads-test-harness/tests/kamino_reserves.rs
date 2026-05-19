use solana_sdk::{signature::Keypair, signer::Signer};
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs,
    create_squads_program_interaction_kamino_usdc_reserve_policy_instruction, derive_squads_policy,
    execute_mock_jupiter_sol_to_usdc_swap_instruction,
    execute_squads_program_interaction_instruction, get_spl_token_amount,
    mock_kamino_deposit_reserve_liquidity_data, mock_kamino_reserve_transaction,
    mock_kamino_withdraw_reserve_liquidity_data, remove_squads_policy_instruction,
    seed_mock_jupiter_spl_accounts, seed_mock_kamino_reserve_spl_accounts, try_send_instructions,
    MockProgram, KAMINO_MAIN_MARKET, KAMINO_MAIN_USDC_RESERVE, KAMINO_PRIME_MARKET,
    KAMINO_PRIME_USDC_RESERVE, LAMPORTS_PER_SOL,
};

const POLICY_SEED: u64 = 1;
const SOL_TO_USDC_AMOUNT: u64 = 1_000_000;
const DEPOSIT_USDC_AMOUNT: u64 = 600_000;
const WITHDRAW_USDC_AMOUNT: u64 = 200_000;
const DENIED_DEPOSIT_USDC_AMOUNT: u64 = 100_000;

#[test]
fn wallet_b_can_only_deposit_and_withdraw_main_market_usdc_reserve() {
    let mut context = create_funded_squads_test_context_with_mock_programs(&[
        MockProgram::Jupiter,
        MockProgram::KaminoLend,
    ])
    .expect("create funded Squads test context");
    let Some(context) = context.as_mut() else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let wallet_b = Keypair::new();
    context
        .svm
        .airdrop(&wallet_b.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop wallet B");

    let vault_usdc = Keypair::new().pubkey();
    let vault_main_kamino_collateral = Keypair::new().pubkey();
    let main_reserve_liquidity_supply = Keypair::new().pubkey();
    let vault_prime_kamino_collateral = Keypair::new().pubkey();
    let prime_reserve_liquidity_supply = Keypair::new().pubkey();
    let jupiter_sol_escrow = Keypair::new().pubkey();

    seed_mock_jupiter_spl_accounts(&mut context.svm, SOL_TO_USDC_AMOUNT, SOL_TO_USDC_AMOUNT);
    let main_reserve_accounts = seed_mock_kamino_reserve_spl_accounts(
        &mut context.svm,
        KAMINO_MAIN_USDC_RESERVE,
        KAMINO_MAIN_MARKET,
        context.vault,
        vault_usdc,
        vault_main_kamino_collateral,
        main_reserve_liquidity_supply,
    );
    let prime_reserve_accounts = seed_mock_kamino_reserve_spl_accounts(
        &mut context.svm,
        KAMINO_PRIME_USDC_RESERVE,
        KAMINO_PRIME_MARKET,
        context.vault,
        vault_usdc,
        vault_prime_kamino_collateral,
        prime_reserve_liquidity_supply,
    );

    let wallet_a_sol_to_usdc_ix = execute_mock_jupiter_sol_to_usdc_swap_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        context.vault_index,
        context.vault,
        vault_usdc,
        jupiter_sol_escrow,
        SOL_TO_USDC_AMOUNT,
    );
    try_send_instructions(
        &mut context.svm,
        &[wallet_a_sol_to_usdc_ix],
        &context.wallet,
        &[],
    )
    .expect("wallet A swaps vault SOL to USDC through the shared local Jupiter router");
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        SOL_TO_USDC_AMOUNT
    );

    let (policy, _) = derive_squads_policy(&context.pool.settings, POLICY_SEED);
    let create_policy_ix = create_squads_program_interaction_kamino_usdc_reserve_policy_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        wallet_b.pubkey(),
        POLICY_SEED,
        context.vault_index,
        context.vault,
        vault_usdc,
        vault_main_kamino_collateral,
        main_reserve_liquidity_supply,
    );
    try_send_instructions(&mut context.svm, &[create_policy_ix], &context.wallet, &[])
        .expect("wallet A creates Kamino Main Market USDC reserve policy for wallet B");

    let (deposit_instructions, deposit_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        main_reserve_accounts,
        mock_kamino_deposit_reserve_liquidity_data(DEPOSIT_USDC_AMOUNT),
    );
    let main_deposit_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        deposit_instructions,
        vec![0],
        deposit_accounts,
    );
    try_send_instructions(&mut context.svm, &[main_deposit_ix], &wallet_b, &[])
        .expect("wallet B deposits vault USDC into Kamino Main Market USDC reserve");
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        SOL_TO_USDC_AMOUNT - DEPOSIT_USDC_AMOUNT
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_main_kamino_collateral),
        DEPOSIT_USDC_AMOUNT
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, main_reserve_liquidity_supply),
        DEPOSIT_USDC_AMOUNT
    );

    let (withdraw_instructions, withdraw_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        main_reserve_accounts,
        mock_kamino_withdraw_reserve_liquidity_data(WITHDRAW_USDC_AMOUNT),
    );
    let main_withdraw_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        withdraw_instructions,
        vec![1],
        withdraw_accounts,
    );
    try_send_instructions(&mut context.svm, &[main_withdraw_ix], &wallet_b, &[])
        .expect("wallet B withdraws vault USDC from Kamino Main Market USDC reserve");
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        SOL_TO_USDC_AMOUNT - DEPOSIT_USDC_AMOUNT + WITHDRAW_USDC_AMOUNT
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_main_kamino_collateral),
        DEPOSIT_USDC_AMOUNT - WITHDRAW_USDC_AMOUNT
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, main_reserve_liquidity_supply),
        DEPOSIT_USDC_AMOUNT - WITHDRAW_USDC_AMOUNT
    );

    let (prime_deposit_instructions, prime_deposit_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        prime_reserve_accounts,
        mock_kamino_deposit_reserve_liquidity_data(DENIED_DEPOSIT_USDC_AMOUNT),
    );
    let prime_deposit_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        prime_deposit_instructions,
        vec![0],
        prime_deposit_accounts,
    );
    let prime_deposit_result =
        try_send_instructions(&mut context.svm, &[prime_deposit_ix], &wallet_b, &[]);
    assert!(
        prime_deposit_result.is_err(),
        "wallet B should not be able to deposit into Kamino Prime Market USDC reserve"
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_prime_kamino_collateral),
        0
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        SOL_TO_USDC_AMOUNT - DEPOSIT_USDC_AMOUNT + WITHDRAW_USDC_AMOUNT
    );

    let remove_policy_ix =
        remove_squads_policy_instruction(context.pool.settings, context.wallet_pubkey(), policy);
    try_send_instructions(&mut context.svm, &[remove_policy_ix], &context.wallet, &[])
        .expect("wallet A removes Kamino USDC reserve policy");

    let (post_removal_instructions, post_removal_accounts) = mock_kamino_reserve_transaction(
        context.vault,
        main_reserve_accounts,
        mock_kamino_deposit_reserve_liquidity_data(DENIED_DEPOSIT_USDC_AMOUNT),
    );
    let post_removal_deposit_ix = execute_squads_program_interaction_instruction(
        policy,
        wallet_b.pubkey(),
        context.vault_index,
        post_removal_instructions,
        vec![0],
        post_removal_accounts,
    );
    let post_removal_result =
        try_send_instructions(&mut context.svm, &[post_removal_deposit_ix], &wallet_b, &[]);
    assert!(
        post_removal_result.is_err(),
        "wallet B should not be able to deposit into Kamino Main Market after policy removal"
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_usdc),
        SOL_TO_USDC_AMOUNT - DEPOSIT_USDC_AMOUNT + WITHDRAW_USDC_AMOUNT
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_main_kamino_collateral),
        DEPOSIT_USDC_AMOUNT - WITHDRAW_USDC_AMOUNT
    );
}
