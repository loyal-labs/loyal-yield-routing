//! Two production Go swaps on cloned deployed programs; not mainnet/signature
//! proof and not a complete lending lifecycle. All local overrides are retained.
use base64::{engine::general_purpose::STANDARD, Engine};
use litesvm::LiteSVM;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{
    account::Account, clock::Clock, message::VersionedMessage, pubkey::Pubkey,
    transaction::VersionedTransaction,
};
use std::{fs, io::Write, path::Path, str::FromStr};

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
    let directory = std::env::var("PHASE3_JUPITER_PROBE_DIR").expect("explicit snapshot directory");
    let directory = Path::new(&directory);
    let plan_bytes = fs::read(directory.join("plan.json")).unwrap();
    let snapshot_bytes = fs::read(directory.join("snapshot.json")).unwrap();
    let plan: Value = serde_json::from_slice(&plan_bytes).unwrap();
    let snapshot: Value = serde_json::from_slice(&snapshot_bytes).unwrap();
    assert_eq!(plan["schema"], "phase3-jupiter-controlled-probe/v1");
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
        results.push(json!({"action":step["action"],"wireSha256":step["wireSha256"],"before":before,"after":capture(&svm,&addresses),"custodyBefore":balances_before,"custodyAfter":balances_after,"error":error,"logs":meta.logs,"computeUnits":meta.compute_units_consumed,"economicPass":economic_pass,"negative":{"error":format!("{:?}",negative.err),"logs":negative.meta.logs,"rejectedBeforeJupiterCPI":true}}));
        if !economic_pass {
            pass = false;
            break;
        }
    }
    let report = json!({"schema":"phase3-jupiter-controlled-result/v1","broadcast":false,"signatureProof":false,"proofLevel":"TWO_SWAP_PROGRAM_EXECUTION_NOT_FULL_R04_LIFECYCLE","slot":snapshot["slot"],"programs":snapshot["programs"],"planSha256":sha(&plan_bytes),"snapshotSha256":sha(&snapshot_bytes),"overrides":overrides,"steps":results,"twoSwapsPassed":pass&&results.len()==2});
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
