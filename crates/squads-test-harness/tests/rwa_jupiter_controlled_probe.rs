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

fn lending_position(svm: &LiteSVM, address: Pubkey) -> (u64, u128) {
    let Some(a) = svm.get_account(&address) else {
        return (0, 0);
    };
    if a.lamports == 0 || a.data.is_empty() {
        return (0, 0);
    }
    // Deployed KLend Obligation layout, shared with the existing controlled probe.
    assert_eq!(a.data.len(), 3344);
    let receipts = (0..8)
        .map(|i| u64::from_le_bytes(a.data[128 + i * 136..136 + i * 136].try_into().unwrap()))
        .sum();
    let debt = (0..5)
        .map(|i| u128::from_le_bytes(a.data[1296 + i * 200..1312 + i * 200].try_into().unwrap()))
        .sum();
    (receipts, debt)
}

#[test]
#[ignore = "requires explicit public snapshot and zero-signature Go plan"]
fn ethena_go_swaps_execute_sequentially_under_deployed_policies() {
    execute_probe(false, false, false, false, false);
}

#[test]
#[ignore = "requires explicit public candidate snapshot; never installed-Go or mainnet proof"]
fn ethena_v2_candidate_swaps_execute_sequentially() {
    execute_probe(true, false, false, false, false);
}

#[test]
#[ignore = "requires explicit return snapshot; local custody preconditions are not a lending lifecycle"]
fn ethena_return_conversions_execute_with_exact_mixed_policy_bindings() {
    execute_probe(true, true, false, false, false);
}

#[test]
#[ignore = "explicit OnRe public snapshot; local candidate roundtrip, no runtime activation"]
fn onre_candidate_entry_and_return_execute_continuously() {
    execute_probe(true, false, true, false, false);
}

#[test]
#[ignore = "explicit OnRe local candidate swap/lending snapshot; no runtime activation"]
fn onre_candidate_lending_executes_between_entry_and_return() {
    execute_probe(true, false, true, true, false);
}

#[test]
#[ignore = "explicit OnRe leverage snapshot; local candidates, no live activation"]
fn onre_candidate_leverage_and_return_execute_continuously() {
    execute_probe(true, false, true, true, true);
}

// Rescale a captured V2 route for local executed poststate, not a live quote.
// Account metas, policy wrapper, discriminator, fees and slippage stay fixed.
fn resize_onre_swap(template: &Value, raw: u64) -> Value {
    let old_raw = template["amountRaw"].as_u64().unwrap();
    assert!(raw > 0 && raw <= 1_000_000_000_000 && old_raw > 0);
    let mut inner = bytes(template, "instructionDataBase64");
    let old_inner = inner.clone();
    let out = u64::from_le_bytes(inner[17..25].try_into().unwrap());
    let quoted = u64::try_from(u128::from(out) * u128::from(raw) / u128::from(old_raw)).unwrap();
    let minimum = u64::try_from(
        u128::from(template["minimumOutputRaw"].as_u64().unwrap()) * u128::from(raw)
            / u128::from(old_raw),
    )
    .unwrap();
    assert!(minimum > 0 && quoted >= minimum);
    inner[9..17].copy_from_slice(&raw.to_le_bytes());
    inner[17..25].copy_from_slice(&quoted.to_le_bytes());
    let mut tx: VersionedTransaction =
        bincode::deserialize(&bytes(template, "wireBase64")).unwrap();
    let ixs = match &mut tx.message {
        VersionedMessage::Legacy(m) => &mut m.instructions,
        VersionedMessage::V0(m) => &mut m.instructions,
    };
    assert_eq!(ixs.len(), 1);
    let positions: Vec<_> = ixs[0]
        .data
        .windows(old_inner.len())
        .enumerate()
        .filter_map(|(n, b)| (b == old_inner).then_some(n))
        .collect();
    assert_eq!(positions.len(), 1);
    ixs[0].data[positions[0]..positions[0] + inner.len()].copy_from_slice(&inner);
    let wire = bincode::serialize(&tx).unwrap();
    let mut resized = template.clone();
    resized["amountRaw"] = json!(raw);
    resized["minimumOutputRaw"] = json!(minimum);
    resized["instructionDataBase64"] = json!(STANDARD.encode(&inner));
    resized["wireBase64"] = json!(STANDARD.encode(&wire));
    resized["wireSha256"] = json!(sha(&wire));
    resized
}

fn onre_funding_swap(
    svm: &mut LiteSVM,
    plan: &Value,
    addresses: &[Pubkey],
    raw: u64,
) -> (Value, bool) {
    let step = resize_onre_swap(&plan["steps"][0], raw);
    let source = key(step["source"].as_str().unwrap());
    let destination = key(step["destination"].as_str().unwrap());
    let obligation = key(plan["onreLending"]["obligation"].as_str().unwrap());
    let position = lending_position(svm, obligation);
    let balances = (amount(svm, source), amount(svm, destination));
    let before = capture(svm, addresses);
    let wire = bytes(&step, "wireBase64");
    assert!(wire.len() <= 1232 && wire[0] == 1 && wire[1..65].iter().all(|b| *b == 0));
    let tx: VersionedTransaction = bincode::deserialize(&wire).unwrap();
    let mut forbidden = tx.clone();
    let ixs = match &mut forbidden.message {
        VersionedMessage::Legacy(m) => &mut m.instructions,
        VersionedMessage::V0(m) => &mut m.instructions,
    };
    let inner = bytes(&step, "instructionDataBase64");
    let positions: Vec<_> = ixs[0]
        .data
        .windows(inner.len())
        .enumerate()
        .filter_map(|(n, b)| (b == inner).then_some(n))
        .collect();
    assert_eq!(positions.len(), 1);
    ixs[0].data[positions[0] + 9..positions[0] + 17]
        .copy_from_slice(&1_000_000_000_001u64.to_le_bytes());
    let mut negative_svm = svm.clone();
    let negative = negative_svm
        .send_transaction(forbidden)
        .expect_err("funding amount constraint bypassed");
    assert!(format!("{:?}", negative.err).starts_with("InstructionError(0, Custom("));
    assert!(!negative
        .meta
        .logs
        .iter()
        .any(|l| l == "Program JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4 invoke [2]"));
    assert_eq!(
        (
            amount(&negative_svm, source),
            amount(&negative_svm, destination)
        ),
        balances
    );
    let (error, meta) = match svm.send_transaction(tx) {
        Ok(m) => (None, m),
        Err(f) => (Some(format!("{:?}", f.err)), f.meta),
    };
    let passed = error.is_none()
        && balances.0 - amount(svm, source) == raw
        && amount(svm, destination) - balances.1 >= step["minimumOutputRaw"].as_u64().unwrap()
        && lending_position(svm, obligation) == position;
    (
        json!({"leg":"funding-swap","executedRequest":step,"wireSha256":sha(&wire),"amountRaw":raw,
        "before":before,"after":capture(svm,addresses),"error":error,"pass":passed,"logs":meta.logs,"computeUnits":meta.compute_units_consumed,
        "negative":{"rejectedBeforeJupiterCPI":true,"custodyUnchanged":true,"error":format!("{:?}",negative.err),"logs":negative.meta.logs}}),
        passed,
    )
}

// Resize finite unsigned SDK templates from the executed obligation/custody.
// This changes transaction amounts, never account state, policy or authority.
fn onre_lending(
    svm: &mut LiteSVM,
    plan: &Value,
    addresses: &[Pubkey],
    leverage: bool,
) -> (Vec<Value>, bool) {
    let lending = &plan["onreLending"];
    assert_eq!(lending["schema"], "phase3-onre-linked-lending-plan/v1");
    assert_eq!(lending["broadcast"], false);
    assert_eq!(lending["signatureProof"], false);
    assert_eq!(lending["lane"], plan["lane"]);
    let obligation = key(lending["obligation"].as_str().unwrap());
    assert_eq!(lending_position(svm, obligation), (0, 0));
    let collateral = key(plan["collateralCustody"].as_str().unwrap());
    let debt_custody = key(plan["debtCustody"].as_str().unwrap());
    let mut results: Vec<Value> = vec![];
    let steps = lending["steps"].as_array().unwrap();
    assert_eq!(steps.len(), 4);
    let order = if leverage {
        vec![0, 1, 4, 2, 3]
    } else {
        vec![0, 1, 2, 3]
    };
    for i in order {
        let step = if i == 4 {
            &lending["redeposit"]
        } else {
            &steps[i]
        };
        assert_eq!(
            step["leg"],
            ["deposit", "borrow", "repay", "withdraw", "redeposit"][i]
        );
        if i == 4 {
            let borrowed = results.last().unwrap();
            assert_eq!(borrowed["leg"], "borrow");
            let cash_before = borrowed["before"]
                .as_array()
                .unwrap()
                .iter()
                .find(|a| a["address"] == debt_custody.to_string())
                .unwrap();
            let cash_before =
                u64::from_le_bytes(bytes(cash_before, "dataBase64")[64..72].try_into().unwrap());
            let (funding, passed) = onre_funding_swap(
                svm,
                plan,
                addresses,
                amount(svm, debt_custody) - cash_before,
            );
            assert_eq!(funding["before"], borrowed["after"]);
            results.push(funding);
            if !passed {
                return (results, false);
            }
        }
        let (receipts, debt_sf) = lending_position(svm, obligation);
        let raw = match i {
            0 | 4 => amount(svm, collateral),
            1 => 1000,
            2 => u64::try_from((debt_sf + (1u128 << 60) - 1) >> 60).unwrap(),
            _ => {
                assert_eq!(debt_sf, 0);
                receipts
            }
        };
        assert!(raw > 0 && raw <= 1_000_000_000_000);
        if i == 2 {
            assert!(
                raw <= amount(svm, debt_custody),
                "initial cash buffer cannot fund payoff"
            );
        }
        let template = bytes(step, "wireBase64");
        assert_eq!(sha(&template), step["wireSha256"]);
        assert!(
            template.len() <= 1232 && template[0] == 1 && template[1..65].iter().all(|b| *b == 0)
        );
        let mut tx: VersionedTransaction = bincode::deserialize(&template).unwrap();
        let VersionedMessage::Legacy(ref mut message) = tx.message else {
            panic!("unexpected lending encoding")
        };
        assert_eq!(message.instructions.len(), 4);
        let inner = bytes(step, "instructionDataBase64");
        assert_eq!(inner.len(), 16);
        let outer = &mut message.instructions[3].data;
        let matches: Vec<_> = outer
            .windows(inner.len())
            .enumerate()
            .filter_map(|(n, b)| (b == inner).then_some(n))
            .collect();
        assert_eq!(matches.len(), 1);
        let offset = matches[0] + 8;
        outer[offset..offset + 8].copy_from_slice(&raw.to_le_bytes());
        let wire = bincode::serialize(&tx).unwrap();
        let before = capture(svm, addresses);
        if let Some(previous) = results.last() {
            assert_eq!(previous["after"], before);
        }
        let mut forbidden = tx.clone();
        let VersionedMessage::Legacy(ref mut message) = forbidden.message else {
            unreachable!()
        };
        message.instructions[3].data[offset..offset + 8]
            .copy_from_slice(&1_000_000_000_001u64.to_le_bytes());
        let mut negative_svm = svm.clone();
        let negative = negative_svm
            .send_transaction(forbidden)
            .expect_err("lending policy amount boundary bypassed");
        assert!(format!("{:?}", negative.err).starts_with("InstructionError(3, Custom("));
        assert!(!negative
            .meta
            .logs
            .iter()
            .any(|l| l == "Program KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD invoke [2]"));
        assert_eq!(amount(&negative_svm, collateral), amount(svm, collateral));
        assert_eq!(
            amount(&negative_svm, debt_custody),
            amount(svm, debt_custody)
        );
        let (error, meta) = match svm.send_transaction(tx) {
            Ok(m) => (None, m),
            Err(f) => (Some(format!("{:?}", f.err)), f.meta),
        };
        let (receipts_after, debt_after) = lending_position(svm, obligation);
        let passed = error.is_none()
            && (receipts_after > 0) == (i != 3)
            && (debt_after > 0) == (i == 1 || i == 4)
            && (i != 4
                || (receipts_after > receipts
                    && debt_after == debt_sf
                    && amount(svm, collateral) == 0));
        results.push(json!({"leg":step["leg"],"templateSha256":step["wireSha256"],"amountRaw":raw,"wireBase64":STANDARD.encode(&wire),"wireSha256":sha(&wire),
            "before":before,"after":capture(svm,addresses),"receiptRaw":receipts_after,"debtSF":debt_after.to_string(),"error":error,"pass":passed,
            "logs":meta.logs,"computeUnits":meta.compute_units_consumed,"negative":{"error":format!("{:?}",negative.err),"rejectedBeforeKaminoCPI":true,"custodyUnchanged":true}}));
        if !passed {
            return (results, false);
        }
    }
    (results, true)
}

fn execute_probe(candidate: bool, returning: bool, onre: bool, linked_onre: bool, leverage: bool) {
    let directory = std::env::var(if onre {
        "PHASE3_ONRE_PROBE_DIR"
    } else if returning {
        "PHASE3_JUPITER_RETURN_PROBE_DIR"
    } else if candidate {
        "PHASE3_JUPITER_CANDIDATE_PROBE_DIR"
    } else {
        "PHASE3_JUPITER_PROBE_DIR"
    })
    .expect("explicit snapshot directory");
    let directory = Path::new(&directory);
    let plan_bytes = fs::read(directory.join("plan.json")).unwrap();
    let snapshot_bytes = fs::read(directory.join("snapshot.json")).unwrap();
    let plan: Value = serde_json::from_slice(&plan_bytes).unwrap();
    assert_eq!(!plan["onreLending"].is_null(), linked_onre);
    assert_eq!(plan["onreLending"]["redeposit"].is_object(), leverage);
    assert!(!leverage || linked_onre);
    assert!(!linked_onre || onre);
    let snapshot: Value = serde_json::from_slice(&snapshot_bytes).unwrap();
    assert_eq!(plan["schema"], "phase3-jupiter-controlled-probe/v1");
    assert_eq!(
        plan["profile"].as_str() == Some("RETURN_CONVERSIONS"),
        returning
    );
    assert_eq!(
        !plan["candidate"].is_null(),
        candidate,
        "candidate proof must not masquerade as installed Go execution"
    );
    if candidate {
        assert_eq!(plan["compiler"], "TYPESCRIPT_CANDIDATE_NOT_INSTALLED_GO");
    }
    assert_eq!(snapshot["schema"], "phase3-jupiter-svm-snapshot/v1");
    assert_eq!(
        plan["lane"],
        if onre {
            "OnRe/ONyc/USDC"
        } else {
            "Ethena/USDe/PYUSD"
        }
    );
    assert_eq!(plan["profile"].as_str() == Some("ONRE_ROUNDTRIP"), onre);
    if onre {
        assert!(candidate && !returning && plan["lendingPrelude"].is_null());
        assert_eq!(
            plan["inputCustody"],
            "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe"
        );
        assert_eq!(
            plan["collateralCustody"],
            "AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3"
        );
        assert_eq!(plan["debtCustody"], plan["inputCustody"]);
    }
    assert_eq!(plan["broadcast"], false);
    assert_eq!(snapshot["broadcast"], false);
    assert_eq!(
        snapshot["genesis"],
        "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
    );
    let steps = plan["steps"].as_array().unwrap();
    assert_eq!(steps.len(), 2);
    assert_eq!(
        steps[0]["action"],
        if returning {
            "SWAP_COLLATERAL_TO_STABLE_STEP"
        } else {
            "SWAP_STABLE_TO_COLLATERAL_STEP"
        }
    );
    assert_eq!(
        steps[1]["action"],
        if onre {
            "SWAP_COLLATERAL_TO_STABLE_STEP"
        } else if returning {
            "SWAP_DEBT_TO_USDC_STEP"
        } else {
            "SWAP_COLLATERAL_TO_DEBT_STEP"
        }
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
            if returning {
                0
            } else {
                steps[0]["amountRaw"].as_u64().unwrap() + if linked_onre { 1000 } else { 0 }
            },
            true,
        ),
        (
            "collateralCustody",
            if !plan["lendingPrelude"].is_null() {
                100_000_000
            } else if returning {
                steps[0]["amountRaw"].as_u64().unwrap()
            } else {
                0
            },
            true,
        ),
        (
            "debtCustody",
            if !plan["lendingPrelude"].is_null() {
                2_000
            } else if returning {
                steps[1]["amountRaw"].as_u64().unwrap()
            } else {
                0
            },
            true,
        ),
    ] {
        // USDC is both input and debt custody for OnRe. Never overwrite the
        // initial funding a second time, and never patch custody between legs.
        if onre && field == "debtCustody" {
            continue;
        }
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
        // Immutable deployed binaries are loaded and hash-checked above and
        // retained once in snapshot.programs. Repeating LiteSVM's program
        // account representation in every transition adds hundreds of MB;
        // none of these transaction graphs can write an executable account.
        .filter(|address| {
            !snapshot["accounts"]
                .as_array()
                .unwrap()
                .iter()
                .any(|a| a["address"] == address.to_string() && a["executable"] == true)
        })
        .collect();
    let mut creation = vec![];
    if candidate {
        use loyal_actions::{
            create_deployed_semantic_program_interaction_policy_instruction, derive_action_account,
        };
        use solana_sdk::{hash::Hash, message::Message, signature::Signature};
        let binding = &plan["candidate"];
        let artifact_bytes = if onre {
            fs::read(directory.join("candidate.json"))
        } else {
            fs::read(if returning { "../../docs/evidence/backyard-rwa-go/phase3/jupiter-v2-return-repair-candidates-2026-09-05.json" } else { "../../docs/evidence/backyard-rwa-go/phase3/jupiter-v2-repair-candidates-2026-09-04.json" })
        }.unwrap();
        assert_eq!(sha(&artifact_bytes), binding["artifactSha256"]);
        let artifact: Value = serde_json::from_slice(&artifact_bytes).unwrap();
        if onre {
            assert_eq!(artifact["broadcast"], false);
            assert_eq!(artifact["installed"], false);
            let catalog: Value = serde_json::from_slice(
                &fs::read("../../docs/evidence/backyard-rwa-go/policy-compiled-v1.json").unwrap(),
            )
            .unwrap();
            let groups = artifact["groups"].as_array().unwrap();
            assert_eq!(groups.len(), if linked_onre { 4 } else { 2 });
            for (i, group) in groups.iter().enumerate() {
                assert_eq!(
                    group["edge"],
                    ["USDC->ONyc", "ONyc->USDC", "OnRe/borrow", "OnRe/repay"][i]
                );
                let original = catalog["policies"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .find(|p| p["policy"] == group["originalPolicy"])
                    .unwrap();
                assert_eq!(group["originalConstraints"], original["constraints"]);
                let old = original["constraints"].as_array().unwrap();
                let new = group["replacementConstraints"].as_array().unwrap();
                assert_eq!(new.len(), old.len());
                let replaced = group["replacedConstraintIndex"].as_u64().unwrap() as usize;
                assert_eq!(replaced, 0);
                for (index, sibling) in old.iter().enumerate() {
                    if index != replaced {
                        assert_eq!(&new[index], sibling, "sibling authority changed");
                    }
                }
                if i >= 2 {
                    let mut expected = original["constraints"].clone();
                    let positions = if i == 2 { [12u64, 13] } else { [9, 10] };
                    for (n, index) in positions.iter().enumerate() {
                        let a = expected[0]["accountPubkeys"]
                            .as_array_mut()
                            .unwrap()
                            .iter_mut()
                            .find(|a| a["index"] == *index)
                            .unwrap();
                        assert_eq!(
                            a["pubkeys"],
                            json!(["KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"])
                        );
                        a["pubkeys"] = json!([if n == 0 {
                            "nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD"
                        } else {
                            "7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF"
                        }]);
                    }
                    assert_eq!(
                        group["replacementConstraints"], expected,
                        "repair changed more than the two farm positions"
                    );
                }
                assert_eq!(
                    sha(&svm
                        .get_account(&key(group["originalPolicy"].as_str().unwrap()))
                        .unwrap()
                        .data),
                    group["originalPolicyDataSha256"]
                );
            }
        }
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
            let group = artifact["groups"]
                .as_array()
                .unwrap()
                .iter()
                .find(|g| g["edge"] == p["edge"])
                .unwrap();
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
            let mut staged_svm = linked_onre.then(|| svm.clone());
            let staged_create = tx.clone();
            let meta = svm
                .send_transaction(tx)
                .expect("candidate PolicyCreate must execute on cloned real Settings");
            let mut staging = Value::Null;
            if let Some(ref mut staged) = staged_svm {
                let created = svm.get_account(&policy).unwrap();
                let rent = created.lamports;
                let first = rent / 2;
                assert!(first >= staged.minimum_balance_for_rent_exemption(0));
                let payer_before = staged.get_balance(&admin).unwrap();
                let transfer = solana_sdk::system_instruction::transfer(&admin, &policy, first);
                let funding = VersionedTransaction {
                    signatures: vec![Signature::default()],
                    message: VersionedMessage::Legacy(Message::new_with_blockhash(
                        &[transfer],
                        Some(&admin),
                        &Hash::new_from_array([42; 32]),
                    )),
                };
                let funding_wire = bincode::serialize(&funding).unwrap();
                assert!(funding_wire.len() <= 1232);
                staged
                    .send_transaction(funding)
                    .expect("exact OnRe prefunding must execute");
                let payer_middle = staged.get_balance(&admin).unwrap();
                let prefunded = staged.get_account(&policy).unwrap();
                assert_eq!(prefunded.lamports, first);
                assert!(prefunded.data.is_empty());
                assert_eq!(prefunded.owner, key("11111111111111111111111111111111"));
                staged
                    .send_transaction(staged_create)
                    .expect("exact OnRe candidate must create from staged rent");
                assert_eq!(staged.get_account(&policy).unwrap(), created);
                assert_eq!(staged.get_account(&settings), svm.get_account(&settings));
                let payer_after = staged.get_balance(&admin).unwrap();
                assert_eq!(payer_before - payer_middle, first + 5000);
                assert_eq!(payer_middle - payer_after, rent - first + 5000);
                staging = json!({"broadcast":false,"installed":false,"comparisonOnly":true,
                    "allocatedBytes":created.data.len(),"fundingPacketBytes":funding_wire.len(),
                    "fundingWireSha256":sha(&funding_wire),"firstFundingLamports":first,"localRentLamports":rent,
                    "firstPayerDebitLamports":payer_before-payer_middle,"secondPayerDebitLamports":payer_middle-payer_after,
                    "samePolicyBytesAndBalance":true,"sameSettings":true});
            }
            creation.push(json!({"policy":policy.to_string(),"seed":seed,"candidateOnly":true,"packetBytes":wire.len(),"wireSha256":sha(&wire),"before":before,"after":capture(&svm,&[settings,admin,policy]),"logs":meta.logs,"computeUnits":meta.compute_units_consumed,"stagedComparison":staging}));
        }
        assert_eq!(
            creation.len(),
            if linked_onre {
                4
            } else if returning {
                1
            } else {
                2
            }
        );
    }
    let mut lending_results: Vec<Value> = vec![];
    let mut deposit_rounding_probes: Vec<Value> = vec![];
    let mut borrow_fee_probes: Vec<Value> = vec![];
    let mut redeposit_probe = Value::Null;
    if !plan["lendingPrelude"].is_null() {
        assert!(returning);
        let lending = &plan["lendingPrelude"];
        assert_eq!(lending["schema"], "phase3-kamino-controlled-probe/v1");
        assert_eq!(lending["broadcast"], false);
        assert_eq!(lending["signatureProof"], false);
        assert_eq!(lending["lane"], plan["lane"]);
        for field in ["delegate", "collateralCustody", "debtCustody"] {
            assert_eq!(lending[field], plan[field]);
        }
        let obligation = key(lending["obligation"].as_str().unwrap());
        assert_eq!(
            lending_position(&svm, obligation),
            (0, 0),
            "cannot inherit a live position"
        );
        let lending_steps = lending["steps"].as_array().unwrap();
        assert_eq!(lending_steps.len(), 4);
        // Real deployed-program falsifiers for the worker's former exact
        // deposit assumption. Isolated clones never feed the linked lifecycle.
        for requested in [1_000_000u64, 99_999_999u64] {
            let mut probe = svm.clone();
            let mut tx: VersionedTransaction =
                bincode::deserialize(&bytes(&lending_steps[0], "wireBase64")).unwrap();
            let VersionedMessage::Legacy(ref mut message) = tx.message else {
                panic!("unexpected deposit encoding")
            };
            assert_eq!(message.instructions.len(), 4);
            let data = &mut message.instructions[3].data;
            let offset = data.len() - 8;
            assert_eq!(&data[offset..], &100_000_000u64.to_le_bytes());
            data[offset..].copy_from_slice(&requested.to_le_bytes());
            let wire = bincode::serialize(&tx).unwrap();
            let before = capture(&probe, &addresses);
            let source = key(plan["collateralCustody"].as_str().unwrap());
            let balance = amount(&probe, source);
            let meta = probe
                .send_transaction(tx)
                .expect("rounded deposit probe failed");
            let debit = balance - amount(&probe, source);
            let (receipts, debt) = lending_position(&probe, obligation);
            assert!(debit > 0 && debit <= requested && receipts > 0 && debt == 0);
            deposit_rounding_probes.push(json!({"requestedRaw":requested,"actualDebitRaw":debit,"receiptRaw":receipts,"wireBase64":STANDARD.encode(&wire),"wireSha256":sha(&wire),"before":before,"after":capture(&probe,&addresses),"logs":meta.logs,"computeUnits":meta.compute_units_consumed}));
        }
        for (i, step) in lending_steps.iter().enumerate() {
            assert_eq!(step["leg"], ["deposit", "borrow", "repay", "withdraw"][i]);
            if i == 2 {
                if let Ok(name) = std::env::var("PHASE3_REDEPOSIT_PLAN") {
                    assert_eq!(
                        std::path::Path::new(&name).file_name().unwrap(),
                        name.as_str()
                    );
                    let template: Value =
                        serde_json::from_slice(&std::fs::read(directory.join(name)).unwrap())
                            .unwrap();
                    assert_eq!(template["planSha256"], sha(&plan_bytes));
                    assert_eq!(template["broadcast"], false);
                    assert_eq!(template["signatureProof"], false);
                    let mut probe = svm.clone();
                    let collateral = key(plan["collateralCustody"].as_str().unwrap());
                    let debt_custody = key(plan["debtCustody"].as_str().unwrap());
                    let mut overrides = vec![];
                    for (address, raw) in [(collateral, 1_000_000u64), (debt_custody, 0u64)] {
                        let prior = amount(&probe, address);
                        let mut account = probe.get_account(&address).unwrap();
                        account.data[64..72].copy_from_slice(&raw.to_le_bytes());
                        probe.set_account(address, account).unwrap();
                        overrides.push(
                            json!({"address":address.to_string(),"beforeRaw":prior,"afterRaw":raw}),
                        );
                    }
                    let before = capture(&probe, &addresses);
                    let (receipt_before, debt_before) = lending_position(&probe, obligation);
                    let wire = bytes(&template, "wireBase64");
                    assert!(
                        wire.len() <= 1232 && wire[0] == 1 && wire[1..65].iter().all(|b| *b == 0)
                    );
                    assert_eq!(sha(&wire), template["wireSha256"]);
                    let tx: VersionedTransaction = bincode::deserialize(&wire).unwrap();
                    let meta = probe
                        .send_transaction(tx)
                        .expect("debt-bearing Go redeposit failed");
                    let (receipt_after, debt_after) = lending_position(&probe, obligation);
                    assert!(receipt_after > receipt_before && debt_before > 0);
                    assert_eq!(debt_after, debt_before);
                    let debit = 1_000_000 - amount(&probe, collateral);
                    redeposit_probe = json!({"wireBase64":STANDARD.encode(&wire),"wireSha256":sha(&wire),"overrides":overrides,"before":before,"after":capture(&probe,&addresses),"actualDebitRaw":debit,"receiptBefore":receipt_before,"receiptAfter":receipt_after,"debtBefore":debt_before.to_string(),"debtAfter":debt_after.to_string(),"logs":meta.logs,"computeUnits":meta.compute_units_consumed,"proofLevel":"ISOLATED_DEBT_BEARING_REDEPOSIT_WITH_LOCAL_CUSTODY_OVERRIDES_NOT_LINKED_FUNDING"});
                }
            }
            if i == 1 {
                // Isolated fee-config mutations answer the deployed-program
                // accounting question; they never feed the linked lifecycle.
                let request = &step["request"];
                let reserve = key(request["Accounts"][4]["Address"].as_str().unwrap());
                let supply = key(request["Accounts"][6]["Address"].as_str().unwrap());
                let receiver = key(request["Accounts"][7]["Address"].as_str().unwrap());
                let custody = key(request["Accounts"][8]["Address"].as_str().unwrap());
                assert_eq!(request["AmountRaw"], 1000);
                for (fee_sf, expected_fee) in [(1u64, 1u64), (1u64 << 52, 4u64)] {
                    let mut probe = svm.clone();
                    let mut account = probe.get_account(&reserve).unwrap();
                    // Official Reserve layout: config + ReserveFees.origination_fee_sf.
                    account.data[4856 + 40..4856 + 48].copy_from_slice(&fee_sf.to_le_bytes());
                    probe.set_account(reserve, account).unwrap();
                    let before = capture(&probe, &addresses);
                    let balances = (
                        amount(&probe, supply),
                        amount(&probe, custody),
                        amount(&probe, receiver),
                    );
                    let wire = bytes(step, "wireBase64");
                    let tx: VersionedTransaction = bincode::deserialize(&wire).unwrap();
                    let meta = probe.send_transaction(tx).expect("borrow fee probe failed");
                    let debit = balances.0 - amount(&probe, supply);
                    let received = amount(&probe, custody) - balances.1;
                    let fee = amount(&probe, receiver) - balances.2;
                    let (_, debt) = lending_position(&probe, obligation);
                    assert_eq!(
                        (debit, received, fee),
                        (1000 + expected_fee, 1000, expected_fee)
                    );
                    assert_eq!(debt, u128::from(1000 + expected_fee) << 60);
                    borrow_fee_probes.push(json!({"feeSF":fee_sf.to_string(),"overrideAccount":reserve.to_string(),"overrideOffset":4896,"actualDebitRaw":debit,"receivedRaw":received,"feeRaw":fee,"borrowedSF":debt.to_string(),"wireSha256":sha(&wire),"before":before,"after":capture(&probe,&addresses),"logs":meta.logs,"computeUnits":meta.compute_units_consumed}));
                }
            }
            let wire = bytes(step, "wireBase64");
            assert!(wire.len() <= 1232 && wire[0] == 1 && wire[1..65].iter().all(|b| *b == 0));
            assert_eq!(sha(&wire), step["wireSha256"]);
            let before = capture(&svm, &addresses);
            if let Some(previous) = lending_results.last() {
                assert_eq!(previous["after"], before);
            }
            let tx: VersionedTransaction = bincode::deserialize(&wire).unwrap();
            let meta = svm
                .send_transaction(tx)
                .expect("linked lending leg failed; no return custody overrides permitted");
            let (receipts, debt) = lending_position(&svm, obligation);
            assert_eq!(receipts > 0, i < 3);
            assert_eq!(debt > 0, i == 1);
            lending_results.push(json!({"leg":step["leg"],"wireSha256":step["wireSha256"],"before":before,"after":capture(&svm,&addresses),"error":null,"logs":meta.logs,"computeUnits":meta.compute_units_consumed,"depositedReceiptRaw":receipts,"borrowedSF":debt.to_string()}));
        }
        // Actual outputs, not fixture resets, must fund the complete returns.
        for step in steps {
            assert_eq!(
                amount(&svm, key(step["source"].as_str().unwrap())),
                step["amountRaw"].as_u64().unwrap()
            );
        }
    }
    let mut results: Vec<Value> = vec![];
    let mut pass = true;
    for (step_index, template_step) in steps.iter().enumerate() {
        let resized_step;
        let step = if linked_onre && step_index == 1 {
            let (legs, passed) = onre_lending(&mut svm, &plan, &addresses, leverage);
            assert_eq!(
                legs[0]["before"], results[0]["after"],
                "entry poststate replaced before lending"
            );
            lending_results = legs;
            if !passed {
                pass = false;
                break;
            }
            // Preserve the captured route and 50-bps slippage while sizing its
            // finite unsigned return to actual redeemed collateral. This is a
            // local quote rescaling, not a fresh live quote or Go planner claim.
            let raw = amount(&svm, key(plan["collateralCustody"].as_str().unwrap()));
            resized_step = resize_onre_swap(template_step, raw);
            &resized_step
        } else {
            template_step
        };
        let wire = bytes(step, "wireBase64");
        assert!(wire.len() <= 1232 && wire[0] == 1 && wire[1..65].iter().all(|b| *b == 0));
        assert_eq!(sha(&wire), step["wireSha256"]);
        let tx: VersionedTransaction = bincode::deserialize(&wire).unwrap();
        let before = capture(&svm, &addresses);
        if linked_onre && step_index == 1 {
            assert_eq!(lending_results.last().unwrap()["after"], before);
        } else if let Some(previous) = results.last() {
            assert_eq!(previous["after"], before);
        } else if let Some(previous) = lending_results.last() {
            assert_eq!(
                previous["after"], before,
                "lending poststate was replaced before return"
            );
        }
        let source = key(step["source"].as_str().unwrap());
        let destination = key(step["destination"].as_str().unwrap());
        let balances_before = (amount(&svm, source), amount(&svm, destination));
        if returning || (onre && step_index == 1) {
            assert_eq!(
                balances_before.0,
                step["amountRaw"].as_u64().unwrap(),
                "return must consume the complete controlled source custody"
            );
        }
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
            let v2 = !returning || step_index == 0;
            let indexes = &step["headerRow"]["header"]["indexes"];
            let mut mutations: Vec<(&str, usize, Vec<u8>)> = vec![
                (
                    "slippage_above_50_bps",
                    matches[0] + indexes["slippage"].as_u64().unwrap() as usize,
                    51u16.to_le_bytes().to_vec(),
                ),
                (
                    "platform_fee_low_byte",
                    matches[0] + indexes["platformFee"].as_u64().unwrap() as usize,
                    vec![1],
                ),
            ];
            if v2 {
                mutations.extend([
                    ("platform_fee_high_byte", matches[0] + 28, vec![1]),
                    ("positive_slippage_fee_low_byte", matches[0] + 29, vec![1]),
                    ("positive_slippage_fee_high_byte", matches[0] + 30, vec![1]),
                ]);
            }
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
            // Change only the compiled inner destination reference to the
            // existing source custody reference at the verified dialect slots. No alternate
            // program or mutable lookup mapping is needed for this falsifier.
            mutations.push((
                "destination_replaced_by_source",
                account_indexes + indexes["destination"].as_u64().unwrap() as usize,
                vec![
                    original_ix.data
                        [account_indexes + indexes["source"].as_u64().unwrap() as usize],
                ],
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
        results.push(json!({"action":step["action"],"wireSha256":step["wireSha256"],"executedRequest":if linked_onre {step.clone()}else{Value::Null},"before":before,"after":capture(&svm,&addresses),"custodyBefore":balances_before,"custodyAfter":balances_after,"error":error,"logs":meta.logs,"computeUnits":meta.compute_units_consumed,"economicPass":economic_pass,"additionalNegatives":additional_negatives,"negative":{"error":format!("{:?}",negative.err),"logs":negative.meta.logs,"rejectedBeforeJupiterCPI":true,"custodyUnchanged":true}}));
        if !economic_pass {
            pass = false;
            break;
        }
    }
    let return_custody_cleared = returning
        && pass
        && results.len() == 2
        && amount(&svm, key(plan["collateralCustody"].as_str().unwrap())) == 0
        && amount(&svm, key(plan["debtCustody"].as_str().unwrap())) == 0;
    let report = json!({"schema":if returning {"phase3-jupiter-return-controlled-result/v1"} else if candidate {"phase3-jupiter-candidate-controlled-result/v1"} else {"phase3-jupiter-controlled-result/v1"},"broadcast":false,"signatureProof":false,"installedPolicyProof":!candidate,"goCompilerProof":false,"proofLevel":if returning {"LOCAL_MIXED_CANDIDATE_AND_INSTALLED_RETURN_SWAPS_NOT_LENDING_BRIDGE_OR_SIGNATURE_PROOF"} else if candidate {"LOCAL_CANDIDATE_TWO_SWAP_EXECUTION_NOT_INSTALLED_GO_OR_FULL_R04"} else {"TWO_SWAP_PROGRAM_EXECUTION_NOT_FULL_R04_LIFECYCLE"},"slot":snapshot["slot"],"programs":snapshot["programs"],"planSha256":sha(&plan_bytes),"snapshotSha256":sha(&snapshot_bytes),"overrides":overrides,"candidateCreation":creation,"steps":results,"twoSwapsPassed":pass&&results.len()==2,"returnCustodyCleared":return_custody_cleared,"terminalUSDCRaw":amount(&svm,key(plan["inputCustody"].as_str().unwrap()))});
    let mut report = report;
    if onre {
        let flat = pass
            && results.len() == 2
            && amount(&svm, key(plan["collateralCustody"].as_str().unwrap())) == 0;
        report["schema"] = json!("phase3-onre-swap-roundtrip-result/v1");
        report["proofLevel"] =
            json!("LOCAL_ONRE_CANDIDATE_SWAP_ROUNDTRIP_NOT_LENDING_BRIDGE_SIGNER_OR_RUNTIME_PROOF");
        report["onreCollateralCleared"] = json!(flat);
    }
    report["stateAddresses"] = json!(addresses.iter().map(|a| a.to_string()).collect::<Vec<_>>());
    if !lending_results.is_empty() {
        report["lendingSteps"] = json!(lending_results);
        report["depositRoundingProbes"] = json!(deposit_rounding_probes);
        report["borrowFeeProbes"] = json!(borrow_fee_probes);
        report["redepositProbe"] = redeposit_probe;
        report["proofLevel"] =
            json!("LOCAL_LINKED_LENDING_AND_RETURN_NOT_BRIDGE_ENTRY_SIGNER_OR_MAINNET_PROOF");
    }
    if linked_onre {
        report["schema"] = json!("phase3-onre-lending-roundtrip-result/v1");
        report["proofLevel"]=json!("LOCAL_ONRE_SWAP_LENDING_RETURN_WITH_POSTSTATE_SIZING_NOT_BRIDGE_GO_SIGNER_RUNTIME_OR_MAINNET");
        report["initialCashBufferRaw"] = json!(1000);
    }
    if leverage {
        report["schema"] = json!("phase3-onre-leverage-roundtrip-result/v1");
        report["proofLevel"]=json!("LOCAL_ONRE_LINKED_BORROW_SWAP_REDEPOSIT_PAYOFF_RETURN_NOT_BRIDGE_GO_SIGNER_RUNTIME_OR_MAINNET");
    }
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
        pass && results.len() == 2 && (!onre || report["onreCollateralCleared"] == true),
        "Jupiter execution failed; inspect retained public result"
    );
}
