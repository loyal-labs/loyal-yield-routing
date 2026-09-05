//! Two swaps on cloned deployed programs, using installed Go or explicitly
//! separate local candidate policy bindings. Not mainnet/signature/full-lifecycle
//! proof. All local overrides are retained; Go parity is checked separately.
use base64::{engine::general_purpose::STANDARD, Engine};
use litesvm::LiteSVM;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{
    account::Account, clock::Clock, message::VersionedMessage, pubkey::Pubkey,
    transaction::VersionedTransaction,
};
use std::{fs, io::Write, path::Path, str::FromStr};
#[path = "support/rwa_jupiter_candidate.rs"]
mod jupiter_candidate;

fn key(s: &str) -> Pubkey {
    Pubkey::from_str(s).unwrap()
}
fn sha(b: &[u8]) -> String {
    format!("{:x}", Sha256::digest(b))
}
fn bytes(v: &Value, field: &str) -> Vec<u8> {
    STANDARD.decode(v[field].as_str().unwrap()).unwrap()
}
fn amount(svm: &LiteSVM, a: Pubkey) -> u64 {
    u64::from_le_bytes(
        svm.get_account(&a).unwrap().data[64..72]
            .try_into()
            .unwrap(),
    )
}
fn capture(svm: &LiteSVM, addresses: &[Pubkey]) -> Value {
    Value::Array(addresses.iter().map(|a| match svm.get_account(a) {
        None => json!({"address":a.to_string(),"present":false}),
        Some(v) => json!({"address":a.to_string(),"present":true,"owner":v.owner.to_string(),"lamports":v.lamports,"dataBase64":STANDARD.encode(&v.data),"dataSha256":sha(&v.data)}),
    }).collect())
}

#[test]
#[ignore = "requires explicit public snapshot and zero-signature Go plan"]
fn ethena_go_swaps_execute_sequentially_under_deployed_policies() {
    execute_probe(false);
}

#[test]
#[ignore = "requires explicit public candidate snapshot; never installed-Go or mainnet proof"]
fn ethena_v2_candidate_swaps_execute_sequentially() {
    execute_probe(true);
}

fn execute_probe(candidate: bool) {
    let directory = std::env::var(if candidate {
        "PHASE3_JUPITER_CANDIDATE_PROBE_DIR"
    } else {
        "PHASE3_JUPITER_PROBE_DIR"
    })
    .expect("explicit snapshot directory");
    let directory = Path::new(&directory);
    let plan_bytes = fs::read(directory.join("plan.json")).unwrap();
    let snapshot_bytes = fs::read(directory.join("snapshot.json")).unwrap();
    let plan: Value = serde_json::from_slice(&plan_bytes).unwrap();
    let snapshot: Value = serde_json::from_slice(&snapshot_bytes).unwrap();
    assert_eq!(plan["schema"], "phase3-jupiter-controlled-probe/v1");
    assert_eq!(
        !plan["candidate"].is_null(),
        candidate,
        "candidate proof must not masquerade as installed Go execution"
    );
    if candidate {
        assert_eq!(plan["compiler"], "TYPESCRIPT_CANDIDATE_NOT_INSTALLED_GO");
    }
    assert_eq!(snapshot["schema"], "phase3-jupiter-svm-snapshot/v1");
    assert_eq!(plan["lane"], "Ethena/USDe/PYUSD");
    assert_eq!(plan["broadcast"], false);
    assert_eq!(snapshot["broadcast"], false);
    assert_eq!(
        snapshot["genesis"],
        "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
    );
    let steps = plan["steps"].as_array().unwrap();
    assert_eq!(steps.len(), 2);
    assert_eq!(steps[0]["action"], "SWAP_STABLE_TO_COLLATERAL_STEP");
    assert_eq!(steps[1]["action"], "SWAP_COLLATERAL_TO_DEBT_STEP");
    let mut svm = LiteSVM::new()
        .with_sigverify(false)
        .with_blockhash_check(false)
        .with_transaction_history(0);
    for p in snapshot["programs"].as_array().unwrap() {
        let code = fs::read(directory.join(p["file"].as_str().unwrap())).unwrap();
        assert_eq!(sha(&code), p["elfSha256"]);
        svm.add_program(key(p["program"].as_str().unwrap()), &code)
            .unwrap();
    }
    for a in snapshot["accounts"].as_array().unwrap() {
        if a["present"] != true || a["executable"] == true {
            continue;
        }
        let data = bytes(a, "dataBase64");
        assert_eq!(sha(&data), a["dataSha256"]);
        let address = key(a["address"].as_str().unwrap());
        if address == solana_sdk::sysvar::clock::ID {
            let clock: Clock = bincode::deserialize(&data).unwrap();
            let slot = snapshot["slot"].as_u64().unwrap();
            assert!(clock.slot >= slot && clock.slot <= slot + 1);
            svm.set_sysvar(&clock);
        } else {
            svm.set_account(
                address,
                Account {
                    lamports: a["lamports"].as_u64().unwrap(),
                    data,
                    owner: key(a["owner"].as_str().unwrap()),
                    executable: false,
                    rent_epoch: 0,
                },
            )
            .unwrap();
        }
    }
    for (address, hash) in plan["policies"].as_object().unwrap() {
        assert_eq!(
            sha(&svm.get_account(&key(address)).unwrap().data),
            hash.as_str().unwrap()
        );
    }
    let mut overrides = vec![];
    for (field, value, token) in [
        ("delegate", 1_000_000_000u64, false),
        (
            "inputCustody",
            steps[0]["amountRaw"].as_u64().unwrap(),
            true,
        ),
        ("collateralCustody", 0, true),
        ("debtCustody", 0, true),
    ] {
        let address = key(plan[field].as_str().unwrap());
        let mut a = svm.get_account(&address).unwrap();
        let before = if token {
            amount(&svm, address)
        } else {
            a.lamports
        };
        if token {
            a.data[64..72].copy_from_slice(&value.to_le_bytes());
        } else {
            a.lamports = value;
        }
        svm.set_account(address, a).unwrap();
        overrides.push(json!({"address":address.to_string(),"field":if token{"tokenAmount"}else{"lamports"},"before":before,"after":value,"reason":"local funded precondition only; policies, authorities, programs and pools unchanged"}));
    }
    let addresses: Vec<Pubkey> = plan["addresses"]
        .as_array()
        .unwrap()
        .iter()
        .map(|a| key(a.as_str().unwrap()))
        .collect();
    let mut creation = vec![];
    if candidate {
        use loyal_actions::{
            create_deployed_semantic_program_interaction_policy_instruction, derive_action_account,
        };
        use solana_sdk::{hash::Hash, message::Message, signature::Signature};
        let binding = &plan["candidate"];
        let artifact_bytes=fs::read("../../docs/evidence/backyard-rwa-go/phase3/jupiter-v2-repair-candidates-2026-09-04.json").unwrap();
        assert_eq!(sha(&artifact_bytes), binding["artifactSha256"]);
        let artifact: Value = serde_json::from_slice(&artifact_bytes).unwrap();
        let settings = key(binding["settings"].as_str().unwrap());
        assert_eq!(
            sha(&svm.get_account(&settings).unwrap().data),
            binding["settingsDataSha256"],
            "Settings changed between plan and snapshot"
        );
        let admin = key(binding["setupAdmin"].as_str().unwrap());
        let mut admin_account = svm.get_account(&admin).unwrap();
        let before = admin_account.lamports;
        admin_account.lamports = 1_000_000_000;
        svm.set_account(admin, admin_account).unwrap();
        overrides.push(json!({"address":admin.to_string(),"field":"lamports","before":before,"after":1_000_000_000u64,"reason":"local-only candidate PolicyCreate funding; no real signature"}));
        let seed_before = binding["seedBefore"]
            .as_str()
            .unwrap()
            .parse::<u64>()
            .unwrap();
        for (i, p) in binding["policies"].as_array().unwrap().iter().enumerate() {
            let group = &artifact["groups"][i];
            assert_eq!(group["edge"], p["edge"]);
            let seed = p["seed"].as_str().unwrap().parse::<u64>().unwrap();
            assert_eq!(seed, seed_before + i as u64 + 1);
            let policy = derive_action_account(&settings, seed).0;
            assert_eq!(policy.to_string(), p["policy"]);
            assert!(
                svm.get_account(&policy).is_none(),
                "candidate PDA must be absent from actual snapshot"
            );
            let ix = create_deployed_semantic_program_interaction_policy_instruction(
                settings,
                admin,
                key(plan["delegate"].as_str().unwrap()),
                seed,
                0,
                jupiter_candidate::constraints(group["replacementConstraints"].as_array().unwrap()),
            )
            .unwrap();
            let tx = VersionedTransaction {
                signatures: vec![Signature::default()],
                message: VersionedMessage::Legacy(Message::new_with_blockhash(
                    &[ix],
                    Some(&admin),
                    &Hash::new_from_array([42; 32]),
                )),
            };
            let wire = bincode::serialize(&tx).unwrap();
            assert!(wire.len() <= 1232);
            let before = capture(&svm, &[settings, admin, policy]);
            let meta = svm
                .send_transaction(tx)
                .expect("candidate PolicyCreate must execute on cloned real Settings");
            creation.push(json!({"policy":policy.to_string(),"seed":seed,"candidateOnly":true,"packetBytes":wire.len(),"wireSha256":sha(&wire),"before":before,"after":capture(&svm,&[settings,admin,policy]),"logs":meta.logs,"computeUnits":meta.compute_units_consumed}));
        }
        assert_eq!(creation.len(), 2);
    }
    let mut results: Vec<Value> = vec![];
    let mut pass = true;
    for step in steps {
        let wire = bytes(step, "wireBase64");
        assert!(wire.len() <= 1232 && wire[0] == 1 && wire[1..65].iter().all(|b| *b == 0));
        assert_eq!(sha(&wire), step["wireSha256"]);
        let tx: VersionedTransaction = bincode::deserialize(&wire).unwrap();
        let before = capture(&svm, &addresses);
        if let Some(previous) = results.last() {
            assert_eq!(previous["after"], before);
        }
        let source = key(step["source"].as_str().unwrap());
        let destination = key(step["destination"].as_str().unwrap());
        let balances_before = (amount(&svm, source), amount(&svm, destination));
        let mut negative_svm = svm.clone();
        let mut forbidden = tx.clone();
        let instructions = match &mut forbidden.message {
            VersionedMessage::Legacy(m) => &mut m.instructions,
            VersionedMessage::V0(m) => &mut m.instructions,
        };
        assert_eq!(instructions.len(), 1);
        let data = &mut instructions[0].data;
        let inner = bytes(step, "instructionDataBase64");
        let matches: Vec<usize> = data
            .windows(inner.len())
            .enumerate()
            .filter_map(|(i, v)| (v == inner).then_some(i))
            .collect();
        assert_eq!(matches.len(), 1);
        let offset = matches[0] + step["amountOffset"].as_u64().unwrap() as usize;
        data[offset..offset + 8]
            .copy_from_slice(&(step["policyMaximumInputRaw"].as_u64().unwrap() + 1).to_le_bytes());
        let negative = negative_svm
            .send_transaction(forbidden)
            .expect_err("installed amount guard bypassed");
        assert!(format!("{:?}", negative.err).starts_with("InstructionError(0, Custom("));
        assert!(!negative
            .meta
            .logs
            .iter()
            .any(|s| s == "Program JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4 invoke [2]"));
        assert_eq!(
            (
                amount(&negative_svm, source),
                amount(&negative_svm, destination)
            ),
            balances_before
        );
        let mut additional_negatives = vec![];
        if candidate {
            let mut mutations: Vec<(&str, usize, Vec<u8>)> = vec![
                (
                    "slippage_above_50_bps",
                    matches[0] + 25,
                    51u16.to_le_bytes().to_vec(),
                ),
                ("platform_fee_low_byte", matches[0] + 27, vec![1]),
                ("platform_fee_high_byte", matches[0] + 28, vec![1]),
                ("positive_slippage_fee_low_byte", matches[0] + 29, vec![1]),
                ("positive_slippage_fee_high_byte", matches[0] + 30, vec![1]),
            ];
            let account_count = step["headerRow"]["instruction"]["accounts"]
                .as_array()
                .unwrap()
                .len();
            let original_ix = match &tx.message {
                VersionedMessage::Legacy(m) => &m.instructions[0],
                VersionedMessage::V0(m) => &m.instructions[0],
            };
            let account_indexes = matches[0] - 2 - account_count;
            assert_eq!(
                original_ix.data[account_indexes - 1] as usize,
                account_count
            );
            // Change only the compiled inner destination reference (V2 slot 5)
            // to the existing source custody reference (slot 2). No alternate
            // program or mutable lookup mapping is needed for this falsifier.
            mutations.push((
                "destination_replaced_by_source",
                account_indexes + 5,
                vec![original_ix.data[account_indexes + 2]],
            ));
            for (label, offset, value) in mutations {
                let mut bad = tx.clone();
                let instructions = match &mut bad.message {
                    VersionedMessage::Legacy(m) => &mut m.instructions,
                    VersionedMessage::V0(m) => &mut m.instructions,
                };
                instructions[0].data[offset..offset + value.len()].copy_from_slice(&value);
                let mut rejected_svm = svm.clone();
                let rejection = rejected_svm
                    .send_transaction(bad)
                    .expect_err("candidate policy accepted forbidden economics or custody");
                assert!(format!("{:?}", rejection.err).starts_with("InstructionError(0, Custom("));
                assert!(
                    !rejection
                        .meta
                        .logs
                        .iter()
                        .any(|s| s
                            == "Program JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4 invoke [2]")
                );
                assert_eq!(
                    (
                        amount(&rejected_svm, source),
                        amount(&rejected_svm, destination)
                    ),
                    balances_before
                );
                additional_negatives.push(json!({"mutation":label,"error":format!("{:?}",rejection.err),"logs":rejection.meta.logs,"rejectedBeforeJupiterCPI":true,"custodyUnchanged":true}));
            }
        }
        let (error, meta) = match svm.send_transaction(tx) {
            Ok(m) => (None, m),
            Err(f) => (Some(format!("{:?}", f.err)), f.meta),
        };
        let balances_after = (amount(&svm, source), amount(&svm, destination));
        let economic_pass = error.is_none()
            && balances_before.0.checked_sub(balances_after.0) == step["amountRaw"].as_u64()
            && balances_after
                .1
                .checked_sub(balances_before.1)
                .is_some_and(|n| n >= step["minimumOutputRaw"].as_u64().unwrap());
        results.push(json!({"action":step["action"],"wireSha256":step["wireSha256"],"before":before,"after":capture(&svm,&addresses),"custodyBefore":balances_before,"custodyAfter":balances_after,"error":error,"logs":meta.logs,"computeUnits":meta.compute_units_consumed,"economicPass":economic_pass,"additionalNegatives":additional_negatives,"negative":{"error":format!("{:?}",negative.err),"logs":negative.meta.logs,"rejectedBeforeJupiterCPI":true,"custodyUnchanged":true}}));
        if !economic_pass {
            pass = false;
            break;
        }
    }
    let report = json!({"schema":if candidate {"phase3-jupiter-candidate-controlled-result/v1"} else {"phase3-jupiter-controlled-result/v1"},"broadcast":false,"signatureProof":false,"installedPolicyProof":!candidate,"goCompilerProof":false,"proofLevel":if candidate {"LOCAL_CANDIDATE_TWO_SWAP_EXECUTION_NOT_INSTALLED_GO_OR_FULL_R04"} else {"TWO_SWAP_PROGRAM_EXECUTION_NOT_FULL_R04_LIFECYCLE"},"slot":snapshot["slot"],"programs":snapshot["programs"],"planSha256":sha(&plan_bytes),"snapshotSha256":sha(&snapshot_bytes),"overrides":overrides,"candidateCreation":creation,"steps":results,"twoSwapsPassed":pass&&results.len()==2});
    let name =
        std::env::var("PHASE3_JUPITER_PROBE_RESULT").unwrap_or_else(|_| "result.json".into());
    assert!(!name.contains('/') && name.ends_with(".json"));
    fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(directory.join(name))
        .unwrap()
        .write_all(serde_json::to_string_pretty(&report).unwrap().as_bytes())
        .unwrap();
    assert!(
        pass && results.len() == 2,
        "Jupiter execution failed; inspect retained public result"
    );
}
