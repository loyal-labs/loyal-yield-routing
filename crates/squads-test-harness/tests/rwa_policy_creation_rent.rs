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
#[ignore = "requires explicit deployed Squads SBF; measures offline replacement groups only"]
fn jupiter_v2_repair_groups_measure_real_creation_without_dropping_sibling_edges() {
    use serde_json::{json, Value};
    use solana_sdk::transaction::Transaction;
    use std::str::FromStr;
    let path = std::env::var("SQUADS_SMART_ACCOUNT_PROGRAM_SO").expect("explicit deployed binary");
    let program = std::fs::read(path).unwrap();
    assert_eq!(
        format!("{:x}", Sha256::digest(&program)),
        "1c95bd7be140589d2aec38a85d7ecfe70ec69639277f622c898f821ab1d636fa"
    );
    let candidate = std::fs::read(
        "../../docs/evidence/backyard-rwa-go/phase3/jupiter-v2-repair-candidates-2026-09-04.json",
    )
    .unwrap();
    let artifact: Value = serde_json::from_slice(&candidate).unwrap();
    assert_eq!(artifact["schema"], "phase3-jupiter-v2-repair-candidates/v1");
    assert_eq!(artifact["broadcast"], false);
    assert_eq!(artifact["installed"], false);
    let groups = artifact["groups"].as_array().unwrap();
    assert_eq!(groups.len(), 2);
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
        let constraints = specs
            .iter()
            .map(|c| SemanticProgramInteractionConstraint {
                program_id: Pubkey::from_str(c["programId"].as_str().unwrap()).unwrap(),
                account_pubkeys: c["accountPubkeys"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .map(|a| {
                        (
                            u8::try_from(a["index"].as_u64().unwrap()).unwrap(),
                            a["pubkeys"]
                                .as_array()
                                .unwrap()
                                .iter()
                                .map(|v| Pubkey::from_str(v.as_str().unwrap()).unwrap())
                                .collect(),
                        )
                    })
                    .collect(),
                account_data: vec![],
                data: c["data"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .map(|d| {
                        let offset = d["offset"].as_u64().unwrap();
                        match d["kind"].as_str().unwrap() {
                            "slice-equals" => {
                                let s = d["valueHex"].as_str().unwrap();
                                assert_eq!(s.len() % 2, 0);
                                SemanticProgramInteractionDataConstraint::SliceEquals {
                                    offset,
                                    value: (0..s.len())
                                        .step_by(2)
                                        .map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap())
                                        .collect(),
                                }
                            }
                            "u64-less-than-or-equal" => {
                                SemanticProgramInteractionDataConstraint::U64LessThanOrEqual {
                                    offset,
                                    value: d["value"].as_u64().unwrap(),
                                }
                            }
                            "u16-less-than-or-equal" => {
                                SemanticProgramInteractionDataConstraint::U16LessThanOrEqual {
                                    offset,
                                    value: u16::try_from(d["value"].as_u64().unwrap()).unwrap(),
                                }
                            }
                            "u8-equals" => SemanticProgramInteractionDataConstraint::U8Equals {
                                offset,
                                value: u8::try_from(d["value"].as_u64().unwrap()).unwrap(),
                            },
                            _ => panic!("candidate introduces unsupported constraint semantics"),
                        }
                    })
                    .collect(),
            })
            .collect();
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
