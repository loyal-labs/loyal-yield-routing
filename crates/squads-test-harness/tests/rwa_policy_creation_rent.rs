//! Measure the real Squads allocation for the legacy one-instruction RWA
//! borrow/repay policy shape. This is local setup-cost/ABI proof, not a live
//! installation or permission to change an existing policy's constraints.

use loyal_actions::{
    create_deployed_semantic_program_interaction_policy_instruction, derive_action_account,
    SemanticProgramInteractionConstraint, SemanticProgramInteractionDataConstraint,
    KAMINO_LEND_PROGRAM_ID,
};
use sha2::{Digest, Sha256};
use solana_sdk::{pubkey::Pubkey, signer::Signer};
use squads_test_harness::prelude::*;

#[test]
#[ignore = "requires explicitly supplied finalized mainnet Squads SBF; repository fixture differs"]
fn legacy_rwa_policy_creation_measures_allocated_bytes_and_payer_debit() {
    let program_path = std::env::var("SQUADS_SMART_ACCOUNT_PROGRAM_SO")
        .expect("supply the explicit deployed Squads binary, not the repository fallback");
    let bytes = std::fs::read(program_path).expect("read explicit deployed binary");
    assert_eq!(
        format!("{:x}", Sha256::digest(&bytes)),
        "1c95bd7be140589d2aec38a85d7ecfe70ec69639277f622c898f821ab1d636fa",
        "deployed program identity changed: refresh affected allocation proof"
    );
    let mut context = create_funded_squads_test_context()
        .expect("load Squads program")
        .expect("Squads SBF is required; absence is not a passing probe");
    let delegate = Pubkey::new_unique();
    // Every account constraint is one exact pubkey, as in the retained
    // forward Maple repair and the affected OnRe/Maple debt-farm vectors.
    // Legacy payloads store pubkeys directly; changing a value does not change
    // its byte length. Real creation, rather than a size formula, checks this.
    for (seed, count, expected_bytes, discriminator) in [
        (1, 15, 1400, [161, 128, 143, 245, 171, 199, 194, 6]),
        (2, 13, 1250, [116, 174, 213, 76, 180, 53, 210, 144]),
    ] {
        let instruction = create_deployed_semantic_program_interaction_policy_instruction(
            context.pool.settings,
            context.wallet.pubkey(),
            delegate,
            seed,
            0,
            vec![SemanticProgramInteractionConstraint {
                program_id: KAMINO_LEND_PROGRAM_ID,
                account_pubkeys: (0..count)
                    .map(|index| (index, vec![Pubkey::new_unique()]))
                    .collect(),
                account_data: vec![],
                data: vec![
                    SemanticProgramInteractionDataConstraint::SliceEquals {
                        offset: 0,
                        value: discriminator.to_vec(),
                    },
                    SemanticProgramInteractionDataConstraint::U64LessThanOrEqual {
                        offset: 8,
                        value: 1_000_000_000_000,
                    },
                ],
            }],
        )
        .expect("compile deployed policy ABI");
        let policy = derive_action_account(&context.pool.settings, seed).0;
        assert!(context.svm.get_account(&policy).is_none());
        let payer_before = context.svm.get_balance(&context.wallet.pubkey()).unwrap();
        try_send_instructions(&mut context.svm, &[instruction], &context.wallet, &[])
            .expect("real Squads PolicyCreate executes");
        let created = context.svm.get_account(&policy).expect("created policy");
        let payer_after = context.svm.get_balance(&context.wallet.pubkey()).unwrap();
        assert_eq!(created.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        assert!(!created.executable);
        assert_eq!(created.data.len(), expected_bytes);
        let rent = context
            .svm
            .minimum_balance_for_rent_exemption(created.data.len());
        assert_eq!(created.lamports, rent);
        // Fee is separate from account funding. Do not hard-code local rent
        // economics as mainnet rent; the live read-only inspector measures it.
        assert!(payer_before - payer_after > rent);
        eprintln!(
            "RWA PolicyCreate: accounts={count} bytes={} local_rent_lamports={rent} local_total_payer_debit={}",
            created.data.len(), payer_before - payer_after
        );
    }
}
