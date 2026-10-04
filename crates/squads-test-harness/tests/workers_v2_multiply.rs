#[path = "workers_v2_multiply/abi.rs"]
mod abi;
// Independent Squads/SPL execution receipt for the Go wallet-claim parser.
// This executes the checked-in real Squads SBF, not a policy mock.
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use loyal_actions::{compile_squads_inner_instruction, execute_sync_transaction_instruction};
use sha2::{Digest, Sha256};
use solana_sdk::{
    clock::Clock,
    compute_budget::ComputeBudgetInstruction,
    signature::{Keypair, Signer},
    transaction::Transaction,
};
use squads_test_harness::prelude::*;

#[test]
fn root_wallet_claim_pays_exact_request_and_rejects_delegate() {
    let mut context = create_funded_squads_test_context_with_config(FundedSquadsTestConfig {
        vault_index: 0,
        ..FundedSquadsTestConfig::default()
    }).expect("create funded Squads context").expect("required artifact: crates/squads-test-harness/fixtures/squads/squads_smart_account_program.so");
    seed_spl_mint_if_missing(&mut context.svm, USDC_MINT, None, USDC_DECIMALS, 0);
    let source = derive_associated_token_account(context.vault, USDC_MINT);
    let destination = derive_associated_token_account(context.wallet_pubkey(), USDC_MINT);
    seed_spl_token_account(
        &mut context.svm,
        source,
        USDC_MINT,
        context.vault,
        1_000_000,
    );
    let owner = context.wallet_pubkey();
    seed_spl_token_account(&mut context.svm, destination, USDC_MINT, owner, 125_000);
    let amount = 100_000;
    let inner = spl_token::instruction::transfer_checked(
        &spl_token::id(),
        &source,
        &USDC_MINT,
        &destination,
        &context.vault,
        &[],
        amount,
        USDC_DECIMALS,
    )
    .unwrap();
    let mut table = Vec::new();
    let compiled = compile_squads_inner_instruction(&mut table, inner);
    let delegate = Keypair::new();
    context
        .svm
        .airdrop(&delegate.pubkey(), LAMPORTS_PER_SOL / 10)
        .unwrap();
    let unauthorized = execute_sync_transaction_instruction(
        context.pool.settings,
        delegate.pubkey(),
        0,
        vec![compiled.clone()],
        table.clone(),
    );
    assert!(
        try_send_instructions(&mut context.svm, &[unauthorized], &delegate, &[]).is_err(),
        "a delegate cannot perform root-owned payout"
    );
    assert_eq!(get_spl_token_amount(&context.svm, source), 1_000_000);
    assert_eq!(get_spl_token_amount(&context.svm, destination), 125_000);
    let root_claim = execute_sync_transaction_instruction(
        context.pool.settings,
        owner,
        0,
        vec![compiled],
        table,
    );
    let mut clock = context.svm.get_sysvar::<Clock>();
    clock.slot = 1_000;
    context.svm.set_sysvar(&clock);
    let instructions = vec![
        ComputeBudgetInstruction::set_compute_unit_limit(400_000),
        ComputeBudgetInstruction::request_heap_frame(SQUADS_EXTENDED_HEAP_FRAME_BYTES),
        root_claim,
    ];
    let transaction = Transaction::new_signed_with_payer(
        &instructions,
        Some(&owner),
        &[&context.wallet],
        context.svm.latest_blockhash(),
    );
    let wire = bincode::serialize(&transaction).unwrap();
    let metadata = context
        .svm
        .send_transaction(transaction.clone())
        .expect("actual root wallet claim succeeds through Squads and SPL");
    assert_eq!(get_spl_token_amount(&context.svm, source), 900_000);
    assert_eq!(get_spl_token_amount(&context.svm, destination), 225_000);
    assert_eq!(metadata.signature, transaction.signatures[0]);
    if let Some(path) = std::env::var_os("WORKERS_V2_MULTIPLY_RECEIPT_FIXTURE_OUTPUT") {
        let fixture = serde_json::json!({
            "provenance": {"producer": "squads-test-harness workers_v2_multiply root_wallet_claim_pays_exact_request_and_rejects_delegate", "program": "crates/squads-test-harness/fixtures/squads/squads_smart_account_program.so", "programSha256": format!("{:x}", Sha256::digest(std::fs::read(&context.loaded_program_path).unwrap())), "proof": "real Squads SBF plus SPL Token in LiteSVM; unauthorized delegate failed; exact root payout executed"},
            "settings": context.pool.settings.to_string(), "rootAuthority": owner.to_string(), "vault": context.vault.to_string(), "vaultIndex": 0,
            "requestId": "workers-v2-svm-claim", "amountRaw": amount, "source": source.to_string(), "destination": destination.to_string(),
            "confirmedSlot": context.svm.get_sysvar::<Clock>().slot, "signature": metadata.signature.to_string(), "wireBase64": BASE64.encode(wire),
            "sourceBeforeRaw": 1_000_000, "sourceAfterRaw": 900_000, "destinationBeforeRaw": 125_000, "destinationAfterRaw": 225_000,
        });
        std::fs::write(path, serde_json::to_vec_pretty(&fixture).unwrap()).unwrap();
    }
}
