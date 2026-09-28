//! LiteSVM proof that a mint-scoped Daily spending limit on a
//! ProgramInteraction policy rejects packed outflow beyond the period budget,
//! blocks the second same-period execution, and resets once the period rolls.
//! The loaded Squads SBF does the enforcement; nothing here hand-encodes
//! policy-create Beet bytes (the create instruction comes from the production
//! loyal-actions compiler seam).
//! Both rejected executions return Squads custom error 6073,
//! `ProgramInteractionInsufficientTokenAllowance`.

use litesvm::LiteSVM;
use loyal_actions::{
    create_deployed_semantic_program_interaction_policy_with_daily_spending_limits,
    SemanticProgramInteractionConstraint, SemanticProgramInteractionDataConstraint,
};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    signature::{Keypair, Signer},
};
use squads_test_harness::{
    create_funded_squads_test_context, derive_squads_policy,
    execute_squads_program_interaction_instruction, get_spl_token_amount, seed_spl_mint_if_missing,
    seed_spl_token_account_if_missing, try_send_instructions, SquadsCompiledInstruction,
    LAMPORTS_PER_SOL,
};

// A fresh Settings account starts with policy_seed=0; PolicyCreate assigns
// the next sequential seed, so the first policy in this fixture is seed 1.
const POLICY_SEED: u64 = 1;
const PERIOD_BUDGET: u64 = 300;
const TRANSFER_AMOUNT: u64 = 200;
const VAULT_FUNDING: u64 = 1_000;
const DAY_SECONDS: i64 = 86_400;
const USDC_DECIMALS: u8 = 6;

// Transaction-account layout for the delegated execution:
// 0 source ATA, 1 mint, 2 destination ATA, 3 authority (vault), 4 token program.
const SOURCE_INDEX: usize = 0;
const MINT_INDEX: usize = 1;
const DESTINATION_INDEX: usize = 2;
const AUTHORITY_INDEX: usize = 3;
const TOKEN_PROGRAM_INDEX: usize = 4;

fn svm_timestamp(svm: &LiteSVM) -> i64 {
    let clock: solana_sdk::clock::Clock = svm.get_sysvar();
    clock.unix_timestamp
}

fn set_clock_unix_timestamp(svm: &mut LiteSVM, unix_timestamp: i64) {
    let mut clock: solana_sdk::clock::Clock = svm.get_sysvar();
    clock.slot += 1;
    clock.unix_timestamp = unix_timestamp;
    svm.set_sysvar(&clock);
}

fn transfer_checked(amount: u64) -> SquadsCompiledInstruction {
    let mut data = vec![12u8]; // spl_token TransferChecked
    data.extend_from_slice(&amount.to_le_bytes());
    data.push(USDC_DECIMALS);
    SquadsCompiledInstruction {
        program_id_index: TOKEN_PROGRAM_INDEX,
        accounts: vec![SOURCE_INDEX, MINT_INDEX, DESTINATION_INDEX, AUTHORITY_INDEX],
        data,
    }
}

fn delegated_execution(
    policy: Pubkey,
    signer: &Keypair,
    vault_index: u8,
    vault: Pubkey,
    mint: Pubkey,
    source: Pubkey,
    destination: Pubkey,
    amounts: &[u64],
) -> Instruction {
    let transaction_accounts = vec![
        AccountMeta::new(source, false),
        AccountMeta::new_readonly(mint, false),
        AccountMeta::new(destination, false),
        AccountMeta::new_readonly(vault, false),
        AccountMeta::new_readonly(spl_token::ID, false),
    ];
    execute_squads_program_interaction_instruction(
        policy,
        signer.pubkey(),
        vault_index,
        amounts
            .iter()
            .map(|amount| transfer_checked(*amount))
            .collect(),
        vec![0u8; amounts.len()],
        transaction_accounts,
    )
}

#[test]
fn daily_limit_rejects_packed_outflow_beyond_the_period_budget() {
    let mut context =
        create_funded_squads_test_context().expect("create funded Squads test context");
    let Some(context) = context.as_mut() else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let mint = Keypair::new().pubkey();
    let funder = context.wallet_pubkey();
    seed_spl_mint_if_missing(&mut context.svm, mint, Some(funder), USDC_DECIMALS, 0);
    let source = loyal_actions::derive_associated_token_account(context.vault, mint, spl_token::ID);
    let recipient = Keypair::new();
    context
        .svm
        .airdrop(&recipient.pubkey(), LAMPORTS_PER_SOL)
        .expect("airdrop recipient");
    let destination =
        loyal_actions::derive_associated_token_account(recipient.pubkey(), mint, spl_token::ID);
    seed_spl_token_account_if_missing(&mut context.svm, source, mint, context.vault, VAULT_FUNDING);
    seed_spl_token_account_if_missing(&mut context.svm, destination, mint, recipient.pubkey(), 0);

    let create_policy_ix =
        create_deployed_semantic_program_interaction_policy_with_daily_spending_limits(
            context.pool.settings,
            context.wallet_pubkey(),
            recipient.pubkey(),
            POLICY_SEED,
            context.vault_index,
            vec![SemanticProgramInteractionConstraint {
                program_id: spl_token::ID,
                account_pubkeys: Vec::new(),
                account_data: Vec::new(),
                data: vec![SemanticProgramInteractionDataConstraint::U8Equals {
                    offset: 0,
                    value: 12,
                }],
            }],
            &[(mint, PERIOD_BUDGET)],
        )
        .expect("compile the production deployed limited-policy payload");
    try_send_instructions(&mut context.svm, &[create_policy_ix], &context.wallet, &[])
        .expect("create the spending-limited ProgramInteraction policy");
    let (policy, _) = derive_squads_policy(&context.pool.settings, POLICY_SEED);
    let vault_index = context.vault_index;
    let vault = context.vault;
    let execution = |amounts: &[u64]| {
        delegated_execution(
            policy,
            &recipient,
            vault_index,
            vault,
            mint,
            source,
            destination,
            amounts,
        )
    };

    // (i) One delegated execution packing two 200 transfers under the 300
    // budget is rejected, and the atomic failure leaves both balances intact.
    let packed = execution(&[TRANSFER_AMOUNT, TRANSFER_AMOUNT]);
    let packed_error = try_send_instructions(&mut context.svm, &[packed], &recipient, &[])
        .expect_err("packed execution exceeding the period budget must be rejected");
    assert!(
        packed_error.contains("Custom(6073)"),
        "packed rejection must return Squads custom error 6073: {packed_error}"
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, source),
        VAULT_FUNDING,
        "rejected packed execution must not move outflow"
    );
    assert_eq!(get_spl_token_amount(&context.svm, destination), 0);

    // (ii) A single 200 transfer fits; the second 200 in the same period is
    // rejected because the budget is already drawn down.
    let first = execution(&[TRANSFER_AMOUNT]);
    try_send_instructions(&mut context.svm, &[first], &recipient, &[])
        .expect("first transfer fits inside the period budget");
    assert_eq!(
        get_spl_token_amount(&context.svm, source),
        VAULT_FUNDING - TRANSFER_AMOUNT
    );
    let second = execution(&[TRANSFER_AMOUNT]);
    let second_error = try_send_instructions(&mut context.svm, &[second], &recipient, &[])
        .expect_err("second same-period transfer must be rejected once the budget is drawn down");
    assert!(
        second_error.contains("Custom(6073)"),
        "same-period rejection must return Squads custom error 6073: {second_error}"
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, source),
        VAULT_FUNDING - TRANSFER_AMOUNT,
        "rejected same-period transfer must not move outflow"
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, destination),
        TRANSFER_AMOUNT
    );

    // (iii) After the 1-day period rolls, the budget resets and the same
    // transfer passes again.
    let now = svm_timestamp(&context.svm);
    set_clock_unix_timestamp(&mut context.svm, now + DAY_SECONDS + 1);
    let after_rollover = execution(&[TRANSFER_AMOUNT]);
    try_send_instructions(&mut context.svm, &[after_rollover], &recipient, &[])
        .expect("same transfer passes after the daily period rolls");
    assert_eq!(
        get_spl_token_amount(&context.svm, source),
        VAULT_FUNDING - (TRANSFER_AMOUNT * 2)
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, destination),
        TRANSFER_AMOUNT * 2
    );
}
