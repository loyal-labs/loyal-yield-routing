use solana_sdk::{signature::Keypair, signer::Signer};
use squads_test_harness::{
    create_funded_squads_test_context, create_squads_spending_limit_policy_instruction,
    derive_squads_policy, execute_squads_spending_limit_withdrawal_instruction,
    remove_squads_policy_instruction, try_send_instructions, LAMPORTS_PER_SOL,
};

const POLICY_SEED: u64 = 1;
const POLICY_LIMIT_LAMPORTS: u64 = LAMPORTS_PER_SOL / 4;
const WITHDRAWAL_LAMPORTS: u64 = LAMPORTS_PER_SOL / 10;

#[test]
fn wallet_b_can_only_withdraw_within_wallet_a_spending_limit_policy() {
    let mut context =
        create_funded_squads_test_context().expect("create funded Squads test context");
    let Some(context) = context.as_mut() else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let wallet_b = Keypair::new();
    context
        .svm
        .airdrop(&wallet_b.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop wallet B");

    let (policy, _) = derive_squads_policy(&context.pool.settings, POLICY_SEED);

    let create_policy_ix = create_squads_spending_limit_policy_instruction(
        context.pool.settings,
        context.wallet_pubkey(),
        wallet_b.pubkey(),
        POLICY_SEED,
        context.vault_index,
        wallet_b.pubkey(),
        POLICY_LIMIT_LAMPORTS,
        POLICY_LIMIT_LAMPORTS,
    );
    try_send_instructions(&mut context.svm, &[create_policy_ix], &context.wallet, &[])
        .expect("wallet A creates spending limit policy for wallet B");

    let first_withdrawal_ix = execute_squads_spending_limit_withdrawal_instruction(
        policy,
        wallet_b.pubkey(),
        context.pool.settings,
        context.vault_index,
        wallet_b.pubkey(),
        WITHDRAWAL_LAMPORTS,
    );
    try_send_instructions(&mut context.svm, &[first_withdrawal_ix], &wallet_b, &[])
        .expect("wallet B withdraws 0.10 SOL");
    assert_eq!(
        context.vault_balance(),
        context.vault_funding_lamports - WITHDRAWAL_LAMPORTS
    );

    let excessive_withdrawal_ix = execute_squads_spending_limit_withdrawal_instruction(
        policy,
        wallet_b.pubkey(),
        context.pool.settings,
        context.vault_index,
        wallet_b.pubkey(),
        LAMPORTS_PER_SOL,
    );
    let excessive_withdrawal_result =
        try_send_instructions(&mut context.svm, &[excessive_withdrawal_ix], &wallet_b, &[]);
    assert!(
        excessive_withdrawal_result.is_err(),
        "wallet B should not be able to withdraw 1 SOL"
    );
    assert_eq!(
        context.vault_balance(),
        context.vault_funding_lamports - WITHDRAWAL_LAMPORTS
    );

    let second_withdrawal_ix = execute_squads_spending_limit_withdrawal_instruction(
        policy,
        wallet_b.pubkey(),
        context.pool.settings,
        context.vault_index,
        wallet_b.pubkey(),
        WITHDRAWAL_LAMPORTS,
    );
    try_send_instructions(&mut context.svm, &[second_withdrawal_ix], &wallet_b, &[])
        .expect("wallet B withdraws another 0.10 SOL");
    assert_eq!(
        context.vault_balance(),
        context.vault_funding_lamports - (WITHDRAWAL_LAMPORTS * 2)
    );

    let remove_policy_ix =
        remove_squads_policy_instruction(context.pool.settings, context.wallet_pubkey(), policy);
    try_send_instructions(&mut context.svm, &[remove_policy_ix], &context.wallet, &[])
        .expect("wallet A removes spending limit policy");

    let post_removal_withdrawal_ix = execute_squads_spending_limit_withdrawal_instruction(
        policy,
        wallet_b.pubkey(),
        context.pool.settings,
        context.vault_index,
        wallet_b.pubkey(),
        LAMPORTS_PER_SOL / 100,
    );
    let post_removal_result = try_send_instructions(
        &mut context.svm,
        &[post_removal_withdrawal_ix],
        &wallet_b,
        &[],
    );
    assert!(
        post_removal_result.is_err(),
        "wallet B should not be able to withdraw after wallet A removes the policy"
    );
    assert_eq!(
        context.vault_balance(),
        context.vault_funding_lamports - (WITHDRAWAL_LAMPORTS * 2)
    );
}
