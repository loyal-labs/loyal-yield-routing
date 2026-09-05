//! Exact Go messages against captured deployed programs. This deliberately
//! disables signature/blockhash checks for zero-signature local wires; it is
//! program/state-transition proof, never signer or mainnet-canary proof.
use base64::{engine::general_purpose::STANDARD, Engine};
use litesvm::LiteSVM;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{
    account::Account, clock::Clock, message::VersionedMessage, pubkey::Pubkey,
    transaction::VersionedTransaction,
};
use std::{fs, path::Path, str::FromStr};

fn key(s: &str) -> Pubkey {
    Pubkey::from_str(s).unwrap()
}
fn sha(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}
fn bytes(v: &Value, field: &str) -> Vec<u8> {
    STANDARD.decode(v[field].as_str().unwrap()).unwrap()
}
fn capture(svm: &LiteSVM, addresses: &[Pubkey]) -> Value {
    Value::Array(addresses.iter().map(|a| match svm.get_account(a) {
        None => json!({"address":a.to_string(),"present":false}),
        Some(v) => json!({"address":a.to_string(),"present":true,"owner":v.owner.to_string(),"lamports":v.lamports,"dataSha256":sha(&v.data),"dataBase64":STANDARD.encode(&v.data)}),
    }).collect())
}
fn position(svm: &LiteSVM, obligation: Pubkey) -> (u64, u128) {
    let Some(a) = svm.get_account(&obligation) else {
        return (0, 0);
    };
    if a.lamports == 0 || a.data.is_empty() {
        return (0, 0);
    }
    // Official klend-interface Obligation layout: 8-byte discriminator,
    // 8 deposits of 136 bytes and 5 borrows of 200 bytes.
    assert_eq!(a.data.len(), 3344);
    let deposits = (0..8)
        .map(|i| u64::from_le_bytes(a.data[128 + i * 136..136 + i * 136].try_into().unwrap()))
        .sum();
    let debt = (0..5)
        .map(|i| u128::from_le_bytes(a.data[1296 + i * 200..1312 + i * 200].try_into().unwrap()))
        .sum();
    (deposits, debt)
}

fn token_amount(svm: &LiteSVM, address: Pubkey) -> u64 {
    let a = svm.get_account(&address).unwrap();
    assert!(a.data.len() >= 165);
    u64::from_le_bytes(a.data[64..72].try_into().unwrap())
}

#[test]
#[ignore = "requires explicit finalized public snapshot and Go-exported local probe plan"]
fn ethena_go_messages_execute_sequentially_under_deployed_policies() {
    let directory = std::env::var("PHASE3_KAMINO_PROBE_DIR").expect("explicit snapshot directory");
    let directory = Path::new(&directory);
    let plan: Value =
        serde_json::from_slice(&fs::read(directory.join("plan.json")).unwrap()).unwrap();
    let snapshot: Value =
        serde_json::from_slice(&fs::read(directory.join("snapshot.json")).unwrap()).unwrap();
    assert_eq!(plan["schema"], "phase3-kamino-controlled-probe/v1");
    assert_eq!(plan["lane"], "Ethena/USDe/PYUSD");
    assert_eq!(
        snapshot["genesis"],
        "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
    );
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
            // This public finalized batch returned Clock one slot ahead of its
            // RPC context. Preserve the actual Clock bytes rather than rewinding
            // the protocol clock to make the labels agree. Record both below.
            let context_slot = snapshot["slot"].as_u64().unwrap();
            assert!(clock.slot >= context_slot && clock.slot <= context_slot + 1);
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
    let obligation = key(plan["obligation"].as_str().unwrap());
    assert_eq!(
        position(&svm, obligation),
        (0, 0),
        "probe cannot inherit a live position"
    );
    let mut overrides = vec![];
    for (field, amount, token) in [
        ("delegate", 1_000_000_000u64, false),
        ("collateralCustody", 100_000_000, true),
        ("debtCustody", 2_000, true),
    ] {
        let address = key(plan[field].as_str().unwrap());
        let mut a = svm.get_account(&address).unwrap();
        let before = if token {
            u64::from_le_bytes(a.data[64..72].try_into().unwrap())
        } else {
            a.lamports
        };
        if token {
            assert!(a.data.len() >= 165);
            a.data[64..72].copy_from_slice(&amount.to_le_bytes())
        } else {
            a.lamports = amount
        }
        svm.set_account(address, a).unwrap();
        overrides.push(json!({"address":address.to_string(),"field":if token{"tokenAmount"}else{"lamports"},"before":before,"after":amount,"reason":"local-only funded precondition; no authority, policy or program change"}));
    }
    let mut protected = vec![];
    for step in plan["steps"].as_array().unwrap() {
        let wire = bytes(step, "wireBase64");
        assert_eq!(sha(&wire), step["wireSha256"]);
        assert!(wire.len() <= 1232);
        let tx: VersionedTransaction = bincode::deserialize(&wire).unwrap();
        for (i, a) in tx.message.static_account_keys().iter().enumerate() {
            if tx.message.is_maybe_writable(i, None) && !protected.contains(a) {
                protected.push(*a)
            }
        }
    }
    // Mutate only the amount in the already compiled Squads-inner deposit.
    // The unchanged positive below proves the same account/program setup works.
    // This tests the deployed policy, not just the Go construction guard.
    let mut negative_svm = svm.clone();
    let mut forbidden: VersionedTransaction =
        bincode::deserialize(&bytes(&plan["steps"][0], "wireBase64")).unwrap();
    let VersionedMessage::Legacy(ref mut message) = forbidden.message else {
        panic!("unexpected probe message version")
    };
    assert_eq!(message.instructions.len(), 4);
    let instruction = &mut message.instructions[3];
    let offset = instruction.data.len() - 8;
    assert_eq!(&instruction.data[offset..], &100_000_000u64.to_le_bytes());
    instruction.data[offset..].copy_from_slice(&1_000_000_000_001u64.to_le_bytes());
    let negative = negative_svm
        .send_transaction(forbidden)
        .expect_err("installed policy accepted over-limit deposit");
    assert!(format!("{:?}", negative.err).starts_with("InstructionError(3, Custom("));
    assert!(
        !negative
            .meta
            .logs
            .iter()
            .any(|s| s == "Program KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD invoke [2]"),
        "forbidden amount reached K-Lend instead of being rejected by Squads"
    );
    assert_eq!(position(&negative_svm, obligation), (0, 0));
    let negative_evidence = json!({"mutation":"deposit amount 100000000 -> 1000000000001; installed maximum 1000000000000",
        "error":format!("{:?}",negative.err),"logs":negative.meta.logs,"rejectedBeforeKaminoCPI":true});
    let mut results: Vec<Value> = vec![];
    let mut pass = true;
    let collateral_custody = key(plan["collateralCustody"].as_str().unwrap());
    let debt_custody = key(plan["debtCustody"].as_str().unwrap());
    for step in plan["steps"].as_array().unwrap() {
        let before = capture(&svm, &protected);
        if let Some(previous) = results.last() {
            assert_eq!(
                &previous["after"], &before,
                "sequential poststate was not carried forward"
            );
        }
        let balances_before = (
            token_amount(&svm, collateral_custody),
            token_amount(&svm, debt_custody),
        );
        let tx: VersionedTransaction = bincode::deserialize(&bytes(step, "wireBase64")).unwrap();
        let result = svm.send_transaction(tx);
        let (error, meta) = match result {
            Ok(m) => (None, m),
            Err(f) => (Some(format!("{:?}", f.err)), f.meta),
        };
        let after = capture(&svm, &protected);
        let (deposits, debt) = position(&svm, obligation);
        let balances_after = (
            token_amount(&svm, collateral_custody),
            token_amount(&svm, debt_custody),
        );
        results.push(json!({"leg":step["leg"],"error":error,"logs":meta.logs,"computeUnits":meta.compute_units_consumed,
          "before":before,"after":after,"custodyBefore":balances_before,"custodyAfter":balances_after,"depositedReceiptRaw":deposits,"borrowedSF":debt.to_string()}));
        if error.is_some() {
            pass = false;
            break;
        }
        match step["leg"].as_str().unwrap() {
            "deposit" => {
                assert!(deposits > 0 && debt == 0);
                assert_eq!(balances_before.0 - balances_after.0, 100_000_000);
                assert_eq!(balances_before.1, balances_after.1);
            }
            "borrow" => {
                assert!(deposits > 0 && debt > 0);
                assert_eq!(balances_after.1 - balances_before.1, 1_000);
                assert_eq!(balances_before.0, balances_after.0);
            }
            "repay" => {
                assert!(deposits > 0 && debt == 0);
                assert_eq!(balances_before.1 - balances_after.1, 1_000);
                assert_eq!(balances_before.0, balances_after.0);
            }
            "withdraw" => {
                assert_eq!((deposits, debt), (0, 0));
                assert_eq!(balances_after.0, 99_999_999);
                assert_eq!(balances_after.1, 2_000);
            }
            _ => panic!("unknown leg"),
        }
    }
    let report = json!({"schema":"phase3-kamino-controlled-result/v1","broadcast":false,"signatureProof":false,
      "proofLevel":"CONTROLLED_PROGRAM_EXECUTION_NOT_FULL_R04_LIFECYCLE","slot":snapshot["slot"],"clockSlot":svm.get_sysvar::<Clock>().slot,"programs":snapshot["programs"],
      "planSha256":sha(&fs::read(directory.join("plan.json")).unwrap()),"snapshotSha256":sha(&fs::read(directory.join("snapshot.json")).unwrap()),
      "overrides":overrides,"negative":negative_evidence,"steps":results,"fourKaminoLegsPassed":pass && results.len()==4});
    let result_name =
        std::env::var("PHASE3_KAMINO_PROBE_RESULT").unwrap_or_else(|_| "result.json".into());
    assert!(!result_name.contains('/') && result_name.ends_with(".json"));
    let mut output = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(directory.join(result_name))
        .unwrap();
    use std::io::Write;
    output
        .write_all(serde_json::to_string_pretty(&report).unwrap().as_bytes())
        .unwrap();
    eprintln!(
        "controlled Kamino probe: legs={} pass={pass}",
        results.len()
    );
    assert!(
        pass && results.len() == 4,
        "real-program boundary failed; inspect retained logs and exact pre/post accounts"
    );
}
