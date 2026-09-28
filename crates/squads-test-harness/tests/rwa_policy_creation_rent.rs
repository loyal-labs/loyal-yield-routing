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
#[path = "support/rwa_jupiter_candidate.rs"]
mod jupiter_candidate;

#[test]
#[ignore = "requires explicit deployed Squads SBF; measures offline replacement groups only"]
fn jupiter_v2_repair_groups_measure_real_creation_without_dropping_sibling_edges() {
    use serde_json::{json, Value};
    use solana_sdk::transaction::Transaction;
    let path = std::env::var("SQUADS_SMART_ACCOUNT_PROGRAM_SO").expect("explicit deployed binary");
    let program = std::fs::read(path).unwrap();
    assert_eq!(
        format!("{:x}", Sha256::digest(&program)),
        "1c95bd7be140589d2aec38a85d7ecfe70ec69639277f622c898f821ab1d636fa"
    );
    let candidate = std::fs::read(
        "../../docs/evidence/backyard-rwa-go/phase3/jupiter-v2-return-repair-candidates-2026-09-05.json",
    )
    .unwrap();
    let artifact: Value = serde_json::from_slice(&candidate).unwrap();
    assert_eq!(artifact["schema"], "phase3-jupiter-v2-repair-candidates/v1");
    assert_eq!(artifact["broadcast"], false);
    assert_eq!(artifact["installed"], false);
    let groups = artifact["groups"].as_array().unwrap();
    assert_eq!(groups.len(), 3);
    assert_eq!(
        groups
            .iter()
            .map(|g| g["edge"].as_str().unwrap())
            .collect::<Vec<_>>(),
        vec!["USDC->USDe", "USDe->PYUSD", "USDe->USDC"]
    );
    let mut context = create_funded_squads_test_context()
        .unwrap()
        .expect("deployed SBF required");
    let delegate = Pubkey::new_unique();
    let mut rows = vec![];
    for (i, group) in groups.iter().enumerate() {
        let specs = group["replacementConstraints"].as_array().unwrap();
        let originals = group["originalConstraints"].as_array().unwrap();
        assert_eq!(specs.len(), originals.len());
        for (n, spec) in specs.iter().enumerate() {
            if n != group["replacedConstraintIndex"].as_u64().unwrap() as usize {
                assert_eq!(spec, &originals[n]);
            }
        }
        let constraints = jupiter_candidate::constraints(specs);
        let seed = i as u64 + 1;
        let instruction = create_deployed_semantic_program_interaction_policy_instruction(
            context.pool.settings,
            context.wallet.pubkey(),
            delegate,
            seed,
            0,
            constraints,
        )
        .unwrap();
        let tx = Transaction::new_signed_with_payer(
            std::slice::from_ref(&instruction),
            Some(&context.wallet.pubkey()),
            &[&context.wallet],
            context.svm.latest_blockhash(),
        );
        tx.verify().unwrap();
        let wire = bincode::serialize(&tx).unwrap();
        assert!(
            wire.len() <= 1232,
            "replacement PolicyCreate packet does not fit"
        );
        let before = context.svm.get_balance(&context.wallet.pubkey()).unwrap();
        try_send_instructions(&mut context.svm, &[instruction], &context.wallet, &[])
            .expect("deployed Squads must accept exact candidate constraints");
        let policy = derive_action_account(&context.pool.settings, seed).0;
        let account = context.svm.get_account(&policy).unwrap();
        assert_eq!(account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        assert!(!account.executable);
        let rent = context
            .svm
            .minimum_balance_for_rent_exemption(account.data.len());
        assert_eq!(account.lamports, rent);
        rows.push(json!({"edge":group["edge"],"originalPolicy":group["originalPolicy"],"constraints":specs.len(),"packetBytes":wire.len(),"allocatedBytes":account.data.len(),"localRentLamports":rent,"localPayerDebitLamports":before-context.svm.get_balance(&context.wallet.pubkey()).unwrap(),"createdPolicyDataSha256":format!("{:x}",Sha256::digest(&account.data))}));
    }
    eprintln!(
        "PHASE3_JUPITER_V2_POLICY {}",
        json!({"broadcast":false,"installed":false,"candidateSha256":format!("{:x}",Sha256::digest(&candidate)),"proofLevel":"DEPLOYED_SQUADS_LOCAL_CREATION_WITH_EPHEMERAL_SETTINGS_NOT_SWAP_EXECUTION_OR_MAINNET_RENT","rows":rows})
    );
}

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
        let settings_before = context
            .svm
            .get_account(&context.pool.settings)
            .unwrap()
            .data;
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
        // Resolve whether rent can be paid in two separately governed debits,
        // without changing the exact policy or omitting any setup cost. This
        // cloned path is not live setup authority or production admission.
        let mut prefunded = context.svm.clone();
        let rent_target = prefunded.minimum_balance_for_rent_exemption(expected_bytes);
        let first_funding = rent_target / 2;
        let prefund_payer_before = prefunded.get_balance(&context.wallet.pubkey()).unwrap();
        let transfer = solana_system_interface::instruction::transfer(
            &context.wallet.pubkey(),
            &policy,
            first_funding,
        );
        try_send_instructions(&mut prefunded, &[transfer], &context.wallet, &[])
            .expect("local partial rent funding executes");
        let staged = prefunded.get_account(&policy).unwrap();
        assert_eq!(staged.owner, solana_sdk::system_program::ID);
        assert!(staged.data.is_empty());
        assert_eq!(staged.lamports, first_funding);
        let prefund_payer_middle = prefunded.get_balance(&context.wallet.pubkey()).unwrap();
        let prefunded_result = try_send_instructions(
            &mut prefunded,
            std::slice::from_ref(&instruction),
            &context.wallet,
            &[],
        );
        let prefund_payer_after = prefunded.get_balance(&context.wallet.pubkey()).unwrap();
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
        let prefunded_account = prefunded.get_account(&policy).unwrap();
        let accepted = prefunded_result.is_ok();
        if accepted {
            assert_eq!(
                prefunded_account, created,
                "prefunding must preserve exact policy bytes and rent"
            );
            assert!(prefund_payer_before - prefund_payer_middle > first_funding);
            assert!(prefund_payer_middle - prefund_payer_after > rent_target - first_funding);
            assert!(prefund_payer_before - prefund_payer_middle < rent_target);
            assert!(prefund_payer_middle - prefund_payer_after < rent_target);
        } else {
            assert_eq!(
                prefunded_account, staged,
                "rejection must not partially allocate or change policy authority"
            );
        }
        eprintln!(
            "PHASE3_PREFUNDED_POLICY {}",
            serde_json::json!({
                "broadcast": false, "installed": false, "accountCount": count,
                "allocatedBytes": expected_bytes, "accepted": accepted,
                "firstFundingLamports": first_funding, "localRentLamports": rent_target,
                "firstPayerDebitLamports": prefund_payer_before - prefund_payer_middle,
                "secondPayerDebitLamports": prefund_payer_middle - prefund_payer_after,
                "samePolicyBytesAndBalance": accepted && prefunded_account == created,
                "error": prefunded_result.err().map(|e| format!("{e:?}")),
                "proofLevel": "DEPLOYED_PROGRAM_LOCAL_RENT_STAGING_NOT_PRODUCTION_SETUP_ADMISSION_OR_MAINNET"
            })
        );
        eprintln!(
            "RWA PolicyCreate: accounts={count} bytes={} local_rent_lamports={rent} local_total_payer_debit={}",
            created.data.len(), payer_before - payer_after
        );
        let settings_after = context
            .svm
            .get_account(&context.pool.settings)
            .unwrap()
            .data;
        let mut expected_settings = settings_before.clone();
        assert_eq!(expected_settings.len(), 168);
        expected_settings[159..167].copy_from_slice(&seed.to_le_bytes());
        assert_eq!(settings_after, expected_settings, "PolicyCreate may only forward the existing Settings policySeed");
        eprintln!(
            "PHASE3_SETTINGS_CREATE_DELTA {}",
            serde_json::json!({
                "seed": seed,
                "beforeBytes": settings_before.len(), "afterBytes": settings_after.len(),
                "changedOffsets": settings_before.iter().zip(&settings_after).enumerate()
                    .filter_map(|(i,(a,b))| (a!=b).then_some(i)).collect::<Vec<_>>()
            })
        );
    }
}
