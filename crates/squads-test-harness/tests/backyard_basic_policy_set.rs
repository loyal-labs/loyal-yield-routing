//! LiteSVM proof for the Backyard RWA basic policy boundary.
//!
//! The test uses the mainnet Settings account and the deployed Squads ELF from
//! the checked-in `voltr-repair` dump.  The protocol-positive legs are
//! deliberately reported as blocked when the Go message/snapshot handoff is
//! absent; this test never substitutes a synthetic protocol execution for
//! those production-shaped messages.

#![allow(clippy::too_many_arguments)]

use base64::{engine::general_purpose::STANDARD, Engine};
use litesvm::LiteSVM;
use loyal_actions::{
    decode_program_interaction_policy_account, SquadsAccountConstraintKindView,
    SquadsDataConstraintView, SquadsDataOperatorView, SquadsDataValueView,
};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{
    account::Account,
    instruction::{AccountMeta, Instruction},
    message::Message,
    pubkey::Pubkey,
    signature::Signature,
    transaction::Transaction,
};
use squads_test_harness::{
    derive_squads_policy, execute_squads_program_interaction_instruction,
    SquadsCompiledInstruction, SQUADS_SMART_ACCOUNT_PROGRAM_ID,
};
use std::{fs, path::PathBuf, str::FromStr};

const SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
const AUTHORITY: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const DELEGATE: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const VAULT: &str = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh";
const KLEND: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const JUPITER: &str = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4";
const TOKEN: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const ONRE_OBLIGATION: &str = "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei";
const MAPLE_OBLIGATION: &str = "Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT";
const ONYC_CUSTODY: &str = "AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3";
const USDC_CUSTODY: &str = "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe";

const POLICY_PDA: [&str; 4] = [
    "2Wn69xc4ntC2aTjQNi4nnfmTCAqHngWYVfLSyeRbkkKh",
    "BmWgjEMgfYpfAYJCUSUkBmQqRKVxodjioob1gtc8ekuA",
    "9Z9cCwWbh6ygM6zrw5peG6VABYtNufjkwqitE9pdd3aA",
    "Z9jqB9pWDf1L1yFKVzXU1XnX8eKLndFP37FUwZMfWyz",
];

fn key(value: &str) -> Pubkey {
    Pubkey::from_str(value).expect("valid base58 public key")
}

fn fixture_path(address: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("fixtures/voltr-repair")
        .join(format!("{address}.json"))
}

fn fixture(address: &str) -> Value {
    serde_json::from_slice(&fs::read(fixture_path(address)).expect("mainnet fixture exists"))
        .expect("mainnet fixture is JSON")
}

fn fixture_data(address: &str) -> Vec<u8> {
    STANDARD
        .decode(
            fixture(address)["dataBase64"]
                .as_str()
                .expect("fixture data"),
        )
        .expect("fixture data is base64")
}

fn program_elf(programdata: &str) -> Vec<u8> {
    let data = fixture_data(programdata);
    assert_eq!(&data[45..49], b"\x7fELF", "programdata contains an ELF");
    data[45..].to_vec()
}

fn set_system_account(svm: &mut LiteSVM, address: Pubkey) {
    svm.set_account(
        address,
        Account {
            lamports: 1_000_000,
            data: vec![],
            owner: solana_sdk::system_program::ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed system account");
}

fn set_obligation(svm: &mut LiteSVM, address: Pubkey, owner_field: Pubkey) {
    let mut data = vec![0u8; 128];
    data[64..96].copy_from_slice(owner_field.as_ref());
    svm.set_account(
        address,
        Account {
            lamports: 1_000_000,
            data,
            owner: key(KLEND),
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed obligation account");
}

fn build_base() -> LiteSVM {
    let mut svm = LiteSVM::new()
        .with_sigverify(false)
        .with_blockhash_check(false)
        .with_transaction_history(0);
    svm.add_program(
        SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        &program_elf("2g3u9qgz4adKQVN1TUoh7bbBKqaSsjXtz1yX2ptagW5T"),
    )
    .expect("load deployed Squads ELF");

    let settings = key(SETTINGS);
    let mut settings_data = fixture_data(SETTINGS);
    assert_eq!(settings_data[158], 1, "Settings fixture has a policy seed");
    settings_data[159..167].copy_from_slice(&140u64.to_le_bytes());
    let settings_fixture = fixture(SETTINGS);
    svm.set_account(
        settings,
        Account {
            lamports: settings_fixture["lamports"].as_u64().unwrap(),
            data: settings_data,
            owner: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("clone mainnet Settings");
    set_system_account(&mut svm, key(VAULT));
    svm.airdrop(&key(AUTHORITY), 1_000_000_000)
        .expect("fund authority");
    svm.airdrop(&key(DELEGATE), 1_000_000_000)
        .expect("fund delegate");
    svm
}

fn artifact() -> Value {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../docs/evidence/backyard-rwa-basic/policy-artifact-v1.json");
    serde_json::from_slice(&fs::read(path).expect("policy artifact exists"))
        .expect("policy artifact is JSON")
}

fn artifact_instruction(policy: &Value) -> Instruction {
    let accounts = policy["instruction"]["accounts"]
        .as_array()
        .expect("artifact accounts array")
        .iter()
        .map(|account| AccountMeta {
            pubkey: key(account["address"].as_str().unwrap()),
            is_signer: account["signer"].as_bool().unwrap(),
            is_writable: account["writable"].as_bool().unwrap(),
        })
        .collect();
    Instruction {
        program_id: key(policy["instruction"]["programId"].as_str().unwrap()),
        accounts,
        data: STANDARD
            .decode(policy["instruction"]["dataBase64"].as_str().unwrap())
            .expect("artifact instruction data is base64"),
    }
}

fn send(
    svm: &mut LiteSVM,
    instruction: Instruction,
    fee_payer: Pubkey,
) -> (Option<String>, u64, Vec<String>) {
    let message =
        Message::new_with_blockhash(&[instruction], Some(&fee_payer), &svm.latest_blockhash());
    let signatures = vec![Signature::default(); message.header.num_required_signatures as usize];
    match svm.send_transaction(Transaction {
        signatures,
        message,
    }) {
        Ok(meta) => (None, meta.compute_units_consumed, meta.logs),
        Err(meta) => (
            Some(format!("{:?}", meta.err)),
            meta.meta.compute_units_consumed,
            meta.meta.logs,
        ),
    }
}

fn sha256(data: &[u8]) -> String {
    format!("{:x}", Sha256::digest(data))
}

fn hex_bytes(data: &[u8]) -> String {
    data.iter().map(|byte| format!("{byte:02x}")).collect()
}

fn data_value_json(value: &SquadsDataValueView) -> Value {
    match value {
        SquadsDataValueView::U8(value) => json!({"kind":"U8","value":value}),
        SquadsDataValueView::U16Le(value) => json!({"kind":"U16Le","value":value}),
        SquadsDataValueView::U32Le(value) => json!({"kind":"U32Le","value":value}),
        SquadsDataValueView::U64Le(value) => json!({"kind":"U64Le","value":value}),
        SquadsDataValueView::U128Le(value) => json!({"kind":"U128Le","value":value.to_string()}),
        SquadsDataValueView::U8Slice(value) => json!({"kind":"U8Slice","value":hex_bytes(value)}),
    }
}

fn operator_json(value: SquadsDataOperatorView) -> &'static str {
    match value {
        SquadsDataOperatorView::Equals => "Equals",
        SquadsDataOperatorView::NotEquals => "NotEquals",
        SquadsDataOperatorView::GreaterThan => "GreaterThan",
        SquadsDataOperatorView::GreaterThanOrEqualTo => "GreaterThanOrEqualTo",
        SquadsDataOperatorView::LessThan => "LessThan",
        SquadsDataOperatorView::LessThanOrEqualTo => "LessThanOrEqualTo",
    }
}

fn data_constraint_json(value: &SquadsDataConstraintView) -> Value {
    json!({
        "offset": value.data_offset,
        "operator": operator_json(value.operator),
        "value": data_value_json(&value.data_value),
    })
}

fn constraint_json(value: &loyal_actions::SquadsInstructionConstraintView) -> Value {
    let account_constraints = value
        .account_constraints
        .iter()
        .map(|constraint| {
            let kind = match &constraint.kind {
                SquadsAccountConstraintKindView::Pubkey(values) => json!({
                    "kind": "Pubkey",
                    "values": values.iter().map(ToString::to_string).collect::<Vec<_>>(),
                }),
                SquadsAccountConstraintKindView::AccountData(values) => json!({
                    "kind": "AccountData",
                    "owner": constraint.owner.map(|value| value.to_string()),
                    "constraints": values.iter().map(data_constraint_json).collect::<Vec<_>>(),
                }),
            };
            json!({"accountIndex": constraint.account_index, "kind": kind})
        })
        .collect::<Vec<_>>();
    json!({
        "programId": value.program_id.to_string(),
        "accountConstraints": account_constraints,
        "dataConstraints": value.data_constraints.iter().map(data_constraint_json).collect::<Vec<_>>(),
    })
}

fn policy_projection(view: &loyal_actions::SquadsProgramInteractionPolicyAccountView) -> Value {
    json!({
        "settings": view.settings.to_string(),
        "seed": view.policy_seed,
        "policy": view.policy_account.to_string(),
        "delegatedSigner": view.delegated_signer.to_string(),
        "threshold": view.threshold,
        "vaultIndex": view.payload.vault_index,
        "pubkeyTable": view.payload.pubkey_table.iter().map(ToString::to_string).collect::<Vec<_>>(),
        "constraints": view.payload.constraints.iter().map(constraint_json).collect::<Vec<_>>(),
        "spendingLimits": view.payload.spending_limits.len(),
    })
}

fn filler(index: u8) -> Pubkey {
    Pubkey::new_from_array([index; 32])
}

fn seed_negative_accounts(svm: &mut LiteSVM) {
    for index in 1..=250u8 {
        set_system_account(svm, filler(index));
    }
    for address in [
        key(KLEND),
        key(JUPITER),
        key(TOKEN),
        key(ONYC_CUSTODY),
        key(USDC_CUSTODY),
        key("5LR9AdS7XwJjXQWkKNBXNibGNkFXqe7T2JXU2oBBwknV"),
        key("J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn"),
        key("DnBnX19kFyCP3Kdhkq7uEJ6juCYEaiS6jZMSXbfCXzct"),
        key("CYwM28WSoYp85HrQGuaVpWy2JhKH6JJah4m65DSWUNiN"),
    ] {
        set_system_account(svm, address);
    }
    set_obligation(svm, key(ONRE_OBLIGATION), key(VAULT));
    set_obligation(svm, key(MAPLE_OBLIGATION), key(VAULT));
    set_obligation(svm, filler(241), key(VAULT));
    set_obligation(svm, filler(242), filler(242));
}

fn lending_accounts(obligation: Pubkey, reserve: Pubkey, custody: Pubkey) -> Vec<AccountMeta> {
    [
        key(VAULT),
        obligation,
        filler(10),
        filler(11),
        reserve,
        filler(12),
        USDC_CUSTODY.parse().unwrap(),
        filler(13),
        USDC_CUSTODY.parse().unwrap(),
        custody,
    ]
    .into_iter()
    .map(|address| AccountMeta::new(address, false))
    .chain(std::iter::once(AccountMeta::new_readonly(
        key(KLEND),
        false,
    )))
    .collect()
}

fn swap_accounts(source: Pubkey, destination: Pubkey, authority: Pubkey) -> Vec<AccountMeta> {
    [
        filler(20),
        filler(21),
        authority,
        source,
        filler(22),
        filler(23),
        destination,
    ]
    .into_iter()
    .map(|address| AccountMeta::new(address, false))
    .chain(std::iter::once(AccountMeta::new_readonly(
        key(JUPITER),
        false,
    )))
    .collect()
}

fn execute_probe(
    svm: &mut LiteSVM,
    policy: Pubkey,
    constraint_index: u8,
    accounts: Vec<AccountMeta>,
    data: Vec<u8>,
) -> (Option<String>, u64, Vec<String>) {
    let account_count = accounts.len() - 1;
    let instruction = execute_squads_program_interaction_instruction(
        policy,
        key(DELEGATE),
        0,
        vec![SquadsCompiledInstruction {
            program_id_index: account_count,
            accounts: (0..account_count).collect(),
            data,
        }],
        vec![constraint_index],
        accounts,
    );
    send(svm, instruction, key(DELEGATE))
}

fn rejected_before_protocol(result: &(Option<String>, u64, Vec<String>), protocol: &str) -> bool {
    result.0.is_some()
        && !result
            .2
            .iter()
            .any(|log| log.contains(protocol) && log.contains("invoke"))
}

#[test]
fn backyard_basic_policy_set() {
    let artifact = artifact();
    assert_eq!(
        artifact["schema"],
        "loyal-backyard-rwa-basic-policy-artifact/v1"
    );
    let policies = artifact["policies"].as_array().unwrap();
    assert_eq!(policies.len(), 4);

    let mut svm = build_base();
    let mut installs = Vec::new();
    for (offset, policy) in policies.iter().enumerate() {
        let seed = policy["seed"].as_str().unwrap().parse::<u64>().unwrap();
        assert_eq!(seed, 141 + offset as u64);
        let expected_policy = key(POLICY_PDA[offset]);
        let (derived_policy, _) = derive_squads_policy(&key(SETTINGS), seed);
        assert_eq!(derived_policy, expected_policy);
        assert!(svm.get_account(&expected_policy).is_none());

        let instruction = artifact_instruction(policy);
        assert_eq!(sha256(&instruction.data), policy["dataSha256"]);
        let (err, units, logs) = send(&mut svm, instruction, key(AUTHORITY));
        assert!(
            err.is_none(),
            "PolicyCreate seed {seed} failed: {err:?} logs={logs:?}"
        );
        let account = svm
            .get_account(&expected_policy)
            .expect("PolicyCreate account exists");
        assert_eq!(account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        let view = decode_program_interaction_policy_account(&account.data)
            .expect("policy account decodes")
            .expect("policy account is ProgramInteraction");
        assert_eq!(view.settings, key(SETTINGS));
        assert_eq!(view.policy_seed, seed);
        assert_eq!(view.policy_account, expected_policy);
        assert_eq!(view.delegated_signer, key(DELEGATE));
        assert_eq!(view.threshold, 1);
        assert_eq!(view.payload.vault_index, 0);
        assert!(view.payload.pubkey_table.is_empty());
        assert!(view.payload.spending_limits.is_empty());
        assert_eq!(view.payload.constraints.len(), 2);
        installs.push(json!({
            "seed": seed.to_string(),
            "policy": expected_policy.to_string(),
            "instructionDataSha256": policy["dataSha256"],
            "accountDataSha256": sha256(&account.data),
            "accountDataBytes": account.data.len(),
            "payloadProjection": policy_projection(&view),
            "computeUnits": units,
            "proof": "decoded payload projection; account envelope adds Squads metadata",
        }));
    }
    assert_eq!(
        svm.get_account(&key(SETTINGS)).unwrap().data[159..167],
        144u64.to_le_bytes()
    );
    for (index, expected) in POLICY_PDA.iter().enumerate() {
        assert!(
            svm.get_account(&key(expected)).is_some(),
            "seed {} installed",
            141 + index
        );
    }

    seed_negative_accounts(&mut svm);
    let valid_reserve = filler(30);
    let valid_custody = key(ONYC_CUSTODY);
    let foreign_obligation = filler(242);
    let foreign_reserve = filler(243);
    let foreign_custody = filler(244);
    let negative_cases = [
        (
            "foreign obligation",
            key(POLICY_PDA[0]),
            0,
            lending_accounts(foreign_obligation, valid_reserve, valid_custody),
            vec![216, 224, 191, 27, 204, 151, 102, 175],
            KLEND,
        ),
        (
            "foreign reserve",
            key(POLICY_PDA[0]),
            0,
            lending_accounts(key(ONRE_OBLIGATION), foreign_reserve, valid_custody),
            vec![216, 224, 191, 27, 204, 151, 102, 175],
            KLEND,
        ),
        (
            "foreign custody",
            key(POLICY_PDA[0]),
            0,
            lending_accounts(key(ONRE_OBLIGATION), valid_reserve, foreign_custody),
            vec![216, 224, 191, 27, 204, 151, 102, 175],
            KLEND,
        ),
        (
            "wrong discriminator",
            key(POLICY_PDA[0]),
            0,
            lending_accounts(key(ONRE_OBLIGATION), valid_reserve, valid_custody),
            vec![0, 224, 191, 27, 204, 151, 102, 175],
            KLEND,
        ),
        (
            "forbidden swap pair ONyc -> PYUSD",
            key(POLICY_PDA[2]),
            0,
            swap_accounts(key(ONYC_CUSTODY), filler(245), key(VAULT)),
            vec![193, 32, 0],
            JUPITER,
        ),
        (
            "non-vault authority",
            key(POLICY_PDA[2]),
            0,
            swap_accounts(key(USDC_CUSTODY), key(ONYC_CUSTODY), key(AUTHORITY)),
            vec![193, 32, 0],
            JUPITER,
        ),
        (
            "Ethena lane not in catalog",
            key(POLICY_PDA[2]),
            0,
            swap_accounts(filler(246), filler(247), key(VAULT)),
            vec![193, 32, 0],
            JUPITER,
        ),
    ];
    let mut mutations = Vec::new();
    for (label, policy, constraint_index, accounts, data, protocol) in negative_cases {
        let result = execute_probe(&mut svm, policy, constraint_index, accounts, data);
        assert!(
            rejected_before_protocol(&result, protocol),
            "mutation {label} was not rejected by Squads first: {result:?}"
        );
        mutations.push(json!({
            "case": label,
            "result": "rejected",
            "computeUnits": result.1,
            "error": result.0,
            "protocolInvokeObserved": false,
        }));
    }

    let go_messages_path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../docs/evidence/backyard-rwa-basic/go-messages-v1.json");
    let positive_legs = [
        "Maple/syrupUSDC/USDC deposit",
        "Maple/syrupUSDC/USDC borrow",
        "Maple/syrupUSDC/USDC repay",
        "Maple/syrupUSDC/USDC withdraw",
        "OnRe/ONyc/USDC deposit",
        "OnRe/ONyc/USDC borrow",
        "OnRe/ONyc/USDC repay",
        "OnRe/ONyc/USDC withdraw",
        "stable -> RWA swap",
        "RWA -> stable swap",
    ];
    let positive_status = if go_messages_path.exists() {
        "BLOCKED: exact Go messages require the published protocol snapshot and executor wiring"
    } else {
        "BLOCKED: docs/evidence/backyard-rwa-basic/go-messages-v1.json is absent"
    };
    eprintln!("Backyard basic policy install proof: {} policies installed; {} mutations rejected before protocol invoke", installs.len(), mutations.len());
    eprintln!("Backyard positive legs: {positive_status}");

    let evidence = json!({
        "schema": "loyal-backyard-rwa-basic-policy-litesvm-proof/v1",
        "broadcast": false,
        "settings": SETTINGS,
        "settingsPolicySeedBefore": 140,
        "settingsPolicySeedAfter": 144,
        "install": installs,
        "positiveLegs": positive_legs.iter().map(|leg| json!({"leg":leg,"status":"BLOCKED","reason":positive_status})).collect::<Vec<_>>(),
        "negativeMutationMatrix": mutations,
    });
    let evidence_path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../docs/evidence/backyard-rwa-basic/policy-litesvm-proof-v1.json");
    fs::write(
        evidence_path,
        serde_json::to_vec_pretty(&evidence).expect("serialize LiteSVM proof"),
    )
    .expect("write LiteSVM proof evidence");
}
