use loyal_actions::{
    compile_squads_inner_instruction,
    create_deployed_semantic_program_interaction_policy_instruction as create_semantic_program_interaction_policy_instruction,
    execute_program_interaction_policy_instruction, execute_sync_transaction_instruction,
    SemanticProgramInteractionAccountDataConstraint, SemanticProgramInteractionConstraint,
    SemanticProgramInteractionDataConstraint,
};
use solana_sdk::{
    account::Account, instruction::AccountMeta, message::Message, pubkey::Pubkey, signer::Signer,
    transaction::Transaction,
};
use solana_system_interface::instruction as system_instruction;
use squads_test_harness::{
    create_funded_squads_test_context, get_spl_token_amount, seed_spl_mint_if_missing,
    seed_spl_token_account, try_send_instructions_with_heap_frame, USDC_DECIMALS, USDC_MINT,
};

#[test]
fn pinned_squads_accountdata_checks_generation_before_real_system_cpi() {
    let mut c = create_funded_squads_test_context()
        .unwrap()
        .expect("pinned Squads SBF fixture required");
    let wallet = c.wallet.pubkey();
    let receipt = Pubkey::new_unique();
    let receipt_owner = Pubkey::new_unique();
    let generation = vec![7u8; 104];
    c.svm
        .set_account(
            receipt,
            Account {
                lamports: 10_000_000,
                data: generation.clone(),
                owner: receipt_owner,
                executable: false,
                rent_epoch: 0,
            },
        )
        .unwrap();
    let mut inner = system_instruction::transfer(&c.vault, &wallet, 0);
    inner
        .accounts
        .push(AccountMeta::new_readonly(receipt, false));
    let spec = SemanticProgramInteractionConstraint {
        program_id: inner.program_id,
        account_pubkeys: vec![(0, vec![c.vault]), (1, vec![wallet])],
        account_data: vec![SemanticProgramInteractionAccountDataConstraint {
            account_index: 2,
            owner: Some(receipt_owner),
            data: vec![SemanticProgramInteractionDataConstraint::SliceEquals {
                offset: 0,
                value: generation.clone(),
            }],
        }],
        data: vec![SemanticProgramInteractionDataConstraint::SliceEquals {
            offset: 0,
            value: inner.data.clone(),
        }],
    };
    let create = create_semantic_program_interaction_policy_instruction(
        c.pool.settings,
        wallet,
        wallet,
        1,
        0,
        vec![spec.clone()],
    )
    .unwrap();
    let policy = loyal_actions::derive_action_account(&c.pool.settings, 1).0;
    try_send_instructions_with_heap_frame(&mut c.svm, &[create.clone()], &c.wallet, &[])
        .expect("create real policy");
    let mut accounts = vec![];
    let compiled = compile_squads_inner_instruction(&mut accounts, inner);
    let execute = execute_program_interaction_policy_instruction(
        policy,
        wallet,
        0,
        vec![compiled.clone()],
        vec![0],
        accounts.clone(),
    );
    try_send_instructions_with_heap_frame(&mut c.svm, &[execute.clone()], &c.wallet, &[])
        .expect("matching account bytes must execute");
    let mut changed = c.svm.get_account(&receipt).unwrap();
    changed.data[96] ^= 1;
    c.svm.set_account(receipt, changed).unwrap();
    let rejected =
        try_send_instructions_with_heap_frame(&mut c.svm, &[execute.clone()], &c.wallet, &[])
            .expect_err("reused receipt generation must reject");
    println!("GENERATION_MUTATION_REJECTED {rejected}");
    let direct = execute_sync_transaction_instruction(
        c.pool.settings,
        wallet,
        0,
        vec![compiled.clone()],
        accounts.clone(),
    );
    try_send_instructions_with_heap_frame(&mut c.svm, &[direct], &c.wallet, &[])
        .expect("ordinary owner sync does not check policy account data");
    let remove = loyal_actions::remove_policy_instruction(c.pool.settings, wallet, policy);
    let packet = Transaction::new_unsigned(Message::new(
        &[create.clone(), execute, remove.clone()],
        Some(&wallet),
    ));
    println!(
        "SYNTHETIC_GUARD_CREATE_EXECUTE_REMOVE_LEGACY_BYTES={}",
        bincode::serialize(&packet).unwrap().len()
    );
    try_send_instructions_with_heap_frame(&mut c.svm, &[remove], &c.wallet, &[])
        .expect("owner cancels guard");
    let stale = try_send_instructions_with_heap_frame(&mut c.svm, &[create], &c.wallet, &[])
        .expect_err("removed policy seed cannot be reused");
    println!("REMOVED_POLICY_SEED_REUSE_REJECTED {stale}");
    let mut restored = c.svm.get_account(&receipt).unwrap();
    restored.data = generation;
    c.svm.set_account(receipt, restored).unwrap();
    let create2 = create_semantic_program_interaction_policy_instruction(
        c.pool.settings,
        wallet,
        wallet,
        2,
        0,
        vec![spec],
    )
    .unwrap();
    let policy2 = loyal_actions::derive_action_account(&c.pool.settings, 2).0;
    let execute2 = execute_program_interaction_policy_instruction(
        policy2,
        wallet,
        0,
        vec![compiled],
        vec![0],
        accounts,
    );
    let remove2 = loyal_actions::remove_policy_instruction(c.pool.settings, wallet, policy2);
    try_send_instructions_with_heap_frame(
        &mut c.svm,
        &[create2, execute2, remove2],
        &c.wallet,
        &[],
    )
    .expect("create, enforce and remove transient guard in one real SBF transaction");
    assert!(c
        .svm
        .get_account(&policy2)
        .map(|a| a.lamports == 0)
        .unwrap_or(true));
    println!("EPHEMERAL_GUARD_CREATE_ENFORCE_REMOVE_PASSED");
}

fn close_recreate_floor(proceeds: u64) {
    let mut c = create_funded_squads_test_context()
        .unwrap()
        .expect("pinned Squads SBF fixture required");
    let wallet = c.wallet.pubkey();
    seed_spl_mint_if_missing(&mut c.svm, USDC_MINT, None, USDC_DECIMALS, 0);
    let authority_ata =
        spl_associated_token_account::get_associated_token_address(&c.vault, &USDC_MINT);
    let owner_ata = spl_associated_token_account::get_associated_token_address(&wallet, &USDC_MINT);
    let reserve = Pubkey::new_unique();
    seed_spl_token_account(&mut c.svm, authority_ata, USDC_MINT, c.vault, 100);
    seed_spl_token_account(&mut c.svm, owner_ata, USDC_MINT, wallet, 0);
    seed_spl_token_account(&mut c.svm, reserve, USDC_MINT, c.vault, 1000);
    let minimum = 50;
    let inner = vec![
        spl_token::instruction::transfer(
            &spl_token::id(),
            &authority_ata,
            &owner_ata,
            &c.vault,
            &[],
            100,
        )
        .unwrap(),
        spl_token::instruction::close_account(
            &spl_token::id(),
            &authority_ata,
            &wallet,
            &c.vault,
            &[],
        )
        .unwrap(),
        spl_associated_token_account::instruction::create_associated_token_account(
            &c.vault,
            &c.vault,
            &USDC_MINT,
            &spl_token::id(),
        ),
        spl_token::instruction::transfer(
            &spl_token::id(),
            &reserve,
            &authority_ata,
            &c.vault,
            &[],
            proceeds,
        )
        .unwrap(),
        spl_token::instruction::transfer(
            &spl_token::id(),
            &authority_ata,
            &owner_ata,
            &c.vault,
            &[],
            minimum,
        )
        .unwrap(),
    ];
    let mut accounts = vec![];
    let compiled = inner
        .into_iter()
        .map(|ix| compile_squads_inner_instruction(&mut accounts, ix))
        .collect();
    let execute =
        execute_sync_transaction_instruction(c.pool.settings, wallet, 0, compiled, accounts);
    let result = try_send_instructions_with_heap_frame(&mut c.svm, &[execute], &c.wallet, &[]);
    if proceeds < minimum {
        println!("MINIMUM_REJECTED {:?}", result.as_ref().err());
        result.expect_err("old USDC cannot satisfy new proceeds floor");
        assert_eq!(get_spl_token_amount(&c.svm, authority_ata), 100);
        assert_eq!(get_spl_token_amount(&c.svm, owner_ata), 0);
        assert_eq!(get_spl_token_amount(&c.svm, reserve), 1000);
    } else {
        result.expect("real SPL close/recreate and minimum transfer succeed");
        assert_eq!(
            get_spl_token_amount(&c.svm, authority_ata),
            proceeds - minimum
        );
        assert_eq!(get_spl_token_amount(&c.svm, owner_ata), 100 + minimum);
    }
}
#[test]
fn close_recreate_minimum_accepts_new_proceeds() {
    close_recreate_floor(60);
}
#[test]
fn close_recreate_minimum_rejects_old_cash_subsidy_and_rolls_back() {
    close_recreate_floor(40);
}

#[test]
fn settings_state_after_unrelated_owner_sync() {
    let mut c = create_funded_squads_test_context()
        .unwrap()
        .expect("pinned Squads SBF fixture required");
    let wallet = c.wallet.pubkey();
    let before = c.svm.get_account(&c.pool.settings).unwrap().data;
    let mut accounts = vec![];
    let compiled = compile_squads_inner_instruction(
        &mut accounts,
        system_instruction::transfer(&c.vault, &wallet, 0),
    );
    let execute =
        execute_sync_transaction_instruction(c.pool.settings, wallet, 0, vec![compiled], accounts);
    try_send_instructions_with_heap_frame(&mut c.svm, &[execute], &c.wallet, &[]).unwrap();
    let after = c.svm.get_account(&c.pool.settings).unwrap().data;
    let diffs: Vec<_> = before
        .iter()
        .zip(after.iter())
        .enumerate()
        .filter_map(|(i, (a, b))| (a != b).then_some((i, *a, *b)))
        .collect();
    println!(
        "SETTINGS_OWNER_SYNC_BYTE_DIFFS {diffs:?} length_before={} length_after={}",
        before.len(),
        after.len()
    );
    println!(
        "SETTINGS_TRANSACTION_INDEX_BEFORE={} AFTER={}",
        u64::from_le_bytes(before[62..70].try_into().unwrap()),
        u64::from_le_bytes(after[62..70].try_into().unwrap())
    );
}

#[test]
#[ignore = "requires explicit public program/account captures"]
fn actual_voltr_request_cancel_recreation_and_claim_floor() {
    use base64::Engine;
    use solana_sdk::{clock::Clock, instruction::Instruction};
    use std::str::FromStr;
    let capture = std::path::Path::new(
        "/Users/user/loyal/reports/vault-protection-2026-11-05/program-capture",
    );
    assert!(
        capture.join("voltr.so").exists(),
        "external public Voltr capture required"
    );
    let mut c = squads_test_harness::create_funded_squads_test_context_with_config(
        squads_test_harness::FundedSquadsTestConfig {
            vault_index: 2,
            ..Default::default()
        },
    )
    .unwrap()
    .unwrap();
    let voltr = Pubkey::from_str("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8").unwrap();
    c.svm
        .add_program(voltr, &std::fs::read(capture.join("voltr.so")).unwrap())
        .unwrap();
    let snapshot: serde_json::Value =
        serde_json::from_slice(&std::fs::read(capture.join("voltr-accounts.json")).unwrap())
            .unwrap();
    for (key, value) in snapshot["addresses"]
        .as_array()
        .unwrap()
        .iter()
        .zip(snapshot["result"]["value"].as_array().unwrap())
    {
        if value.is_null() {
            continue;
        }
        c.svm
            .set_account(
                Pubkey::from_str(key.as_str().unwrap()).unwrap(),
                Account {
                    lamports: value["lamports"].as_u64().unwrap(),
                    owner: Pubkey::from_str(value["owner"].as_str().unwrap()).unwrap(),
                    data: base64::engine::general_purpose::STANDARD
                        .decode(value["data"][0].as_str().unwrap())
                        .unwrap(),
                    executable: false,
                    rent_epoch: 0,
                },
            )
            .unwrap();
    }
    let owner = c.wallet.pubkey();
    let authority = c.vault;
    let vault = Pubkey::from_str("HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA").unwrap();
    let protocol = Pubkey::find_program_address(&[b"protocol"], &voltr).0;
    let pda = |seed: &[u8]| Pubkey::find_program_address(&[seed, vault.as_ref()], &voltr).0;
    let lp = pda(b"vault_lp_mint");
    let idle_auth = pda(b"vault_asset_idle_auth");
    let receipt = Pubkey::find_program_address(
        &[
            b"request_withdraw_vault_receipt",
            vault.as_ref(),
            authority.as_ref(),
        ],
        &voltr,
    )
    .0;
    let ata = |owner: &Pubkey, mint: &Pubkey| {
        spl_associated_token_account::get_associated_token_address(owner, mint)
    };
    let user_lp = ata(&authority, &lp);
    let escrow = ata(&receipt, &lp);
    let user_asset = ata(&authority, &USDC_MINT);
    let owner_asset = ata(&owner, &USDC_MINT);
    let idle = ata(&idle_auth, &USDC_MINT);
    seed_spl_token_account(&mut c.svm, user_lp, lp, authority, 100_000);
    seed_spl_token_account(&mut c.svm, escrow, lp, receipt, 0);
    seed_spl_token_account(&mut c.svm, user_asset, USDC_MINT, authority, 100);
    seed_spl_token_account(&mut c.svm, owner_asset, USDC_MINT, owner, 0);
    seed_spl_token_account(&mut c.svm, idle, USDC_MINT, idle_auth, 1_000_000_000);
    let mut clock = c.svm.get_sysvar::<Clock>();
    clock.unix_timestamp = 1_793_920_731;
    c.svm.set_sysvar(&clock);
    let mut data = vec![248, 225, 47, 22, 116, 144, 23, 143];
    data.extend_from_slice(&50_000u64.to_le_bytes());
    data.extend_from_slice(&[1, 0]);
    let request = Instruction {
        program_id: voltr,
        data,
        accounts: vec![
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new_readonly(protocol, false),
            AccountMeta::new_readonly(vault, false),
            AccountMeta::new_readonly(lp, false),
            AccountMeta::new(user_lp, false),
            AccountMeta::new(escrow, false),
            AccountMeta::new(receipt, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(solana_sdk::system_program::id(), false),
        ],
    };
    fn send_owner(
        c: &mut squads_test_harness::FundedSquadsTestContext,
        inner: Vec<Instruction>,
    ) -> Result<(), String> {
        let mut accounts = vec![];
        let compiled = inner
            .into_iter()
            .map(|ix| compile_squads_inner_instruction(&mut accounts, ix))
            .collect();
        let execute = execute_sync_transaction_instruction(
            c.pool.settings,
            c.wallet.pubkey(),
            2,
            compiled,
            accounts,
        );
        try_send_instructions_with_heap_frame(&mut c.svm, &[execute], &c.wallet, &[])
    }
    send_owner(&mut c, vec![request.clone()]).expect("actual Voltr request through Squads SBF");
    let first = c.svm.get_account(&receipt).unwrap().data;
    println!(
        "ACTUAL_VOLTR_RECEIPT_BYTES={} LP={}",
        first.len(),
        get_spl_token_amount(&c.svm, escrow)
    );
    let cancel = Instruction {
        program_id: voltr,
        data: vec![231, 54, 14, 6, 223, 124, 127, 238],
        accounts: vec![
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(protocol, false),
            AccountMeta::new(vault, false),
            AccountMeta::new(lp, false),
            AccountMeta::new(user_lp, false),
            AccountMeta::new(escrow, false),
            AccountMeta::new(receipt, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(solana_sdk::system_program::id(), false),
        ],
    };
    send_owner(&mut c, vec![cancel]).expect("actual Voltr cancellation");
    assert!(c
        .svm
        .get_account(&receipt)
        .map(|a| a.lamports == 0)
        .unwrap_or(true));
    send_owner(&mut c, vec![request]).expect("actual Voltr same-clock replacement request");
    let second = c.svm.get_account(&receipt).unwrap().data;
    println!(
        "ACTUAL_VOLTR_SAME_CLOCK_RECREATED_RECEIPT_IDENTICAL={} DIFF_OFFSETS={:?}",
        first == second,
        first
            .iter()
            .zip(second.iter())
            .enumerate()
            .filter_map(|(i, (a, b))| (a != b).then_some(i))
            .collect::<Vec<_>>()
    );
    clock.unix_timestamp = u64::from_le_bytes(second[96..104].try_into().unwrap()) as i64 + 1;
    c.svm.set_sysvar(&clock);
    let claim = Instruction {
        program_id: voltr,
        data: vec![135, 7, 237, 120, 149, 94, 95, 7],
        accounts: vec![
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(protocol, false),
            AccountMeta::new(vault, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new(lp, false),
            AccountMeta::new(escrow, false),
            AccountMeta::new(idle, false),
            AccountMeta::new(idle_auth, false),
            AccountMeta::new(user_asset, false),
            AccountMeta::new(receipt, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(solana_sdk::system_program::id(), false),
        ],
    };
    let inner = vec![
        spl_token::instruction::transfer(
            &spl_token::id(),
            &user_asset,
            &owner_asset,
            &authority,
            &[],
            100,
        )
        .unwrap(),
        spl_token::instruction::close_account(
            &spl_token::id(),
            &user_asset,
            &owner,
            &authority,
            &[],
        )
        .unwrap(),
        spl_associated_token_account::instruction::create_associated_token_account(
            &authority,
            &authority,
            &USDC_MINT,
            &spl_token::id(),
        ),
        claim,
        spl_token::instruction::transfer(
            &spl_token::id(),
            &user_asset,
            &owner_asset,
            &authority,
            &[],
            10,
        )
        .unwrap(),
    ];
    let mut accounts = vec![];
    let compiled = inner
        .into_iter()
        .map(|ix| compile_squads_inner_instruction(&mut accounts, ix))
        .collect();
    let owner_execute =
        execute_sync_transaction_instruction(c.pool.settings, owner, 2, compiled, accounts);
    let mut guard = system_instruction::transfer(&authority, &owner, 0);
    guard
        .accounts
        .push(AccountMeta::new_readonly(receipt, false));
    let create = create_semantic_program_interaction_policy_instruction(
        c.pool.settings,
        owner,
        owner,
        1,
        2,
        vec![SemanticProgramInteractionConstraint {
            program_id: guard.program_id,
            account_pubkeys: vec![],
            account_data: vec![SemanticProgramInteractionAccountDataConstraint {
                account_index: 2,
                owner: Some(voltr),
                data: vec![SemanticProgramInteractionDataConstraint::SliceEquals {
                    offset: 72,
                    value: second[72..104].to_vec(),
                }],
            }],
            data: vec![SemanticProgramInteractionDataConstraint::SliceEquals {
                offset: 0,
                value: guard.data.clone(),
            }],
        }],
    )
    .unwrap();
    let policy = loyal_actions::derive_action_account(&c.pool.settings, 1).0;
    let mut accounts = vec![];
    let compiled = compile_squads_inner_instruction(&mut accounts, guard);
    let guard_execute = execute_program_interaction_policy_instruction(
        policy,
        owner,
        2,
        vec![compiled],
        vec![0],
        accounts,
    );
    let remove = loyal_actions::remove_policy_instruction(c.pool.settings, owner, policy);
    use solana_sdk::{
        address_lookup_table::{
            program as alt_program,
            state::{AddressLookupTable, LookupTableMeta},
            AddressLookupTableAccount,
        },
        message::{v0, VersionedMessage},
        transaction::VersionedTransaction,
    };
    let operations = vec![create, guard_execute, owner_execute, remove];
    let addresses: std::collections::BTreeSet<_> = operations
        .iter()
        .flat_map(|ix| ix.accounts.iter().map(|a| a.pubkey))
        .filter(|key| *key != owner)
        .collect();
    let table = AddressLookupTableAccount {
        key: Pubkey::new_unique(),
        addresses: addresses.into_iter().collect(),
    };
    let mut meta = LookupTableMeta::new(owner);
    meta.last_extended_slot_start_index = table.addresses.len() as u8;
    let data = AddressLookupTable {
        meta,
        addresses: std::borrow::Cow::Owned(table.addresses.clone()),
    }
    .serialize_for_tests()
    .unwrap();
    c.svm
        .set_account(
            table.key,
            Account {
                lamports: c.svm.minimum_balance_for_rent_exemption(data.len()),
                data,
                owner: alt_program::ID,
                executable: false,
                rent_epoch: 0,
            },
        )
        .unwrap();
    c.svm.expire_blockhash();
    let message =
        v0::Message::try_compile(&owner, &operations, &[table], c.svm.latest_blockhash()).unwrap();
    let transaction =
        VersionedTransaction::try_new(VersionedMessage::V0(message), &[&c.wallet]).unwrap();
    println!(
        "ACTUAL_VOLTR_ATOMIC_GUARD_CLAIM_REMOVE_V0_BYTES={}",
        bincode::serialize(&transaction).unwrap().len()
    );
    let result = c
        .svm
        .send_transaction(transaction)
        .expect("actual Voltr guarded claim and floor with temporary policy creation/removal");
    println!(
        "ACTUAL_VOLTR_ATOMIC_GUARD_CLAIM_REMOVE_CU={}",
        result.compute_units_consumed
    );
    assert!(c
        .svm
        .get_account(&policy)
        .map(|a| a.lamports == 0)
        .unwrap_or(true));
    assert_eq!(get_spl_token_amount(&c.svm, owner_asset), 110);
    assert!(c
        .svm
        .get_account(&receipt)
        .map(|a| a.lamports == 0)
        .unwrap_or(true));
    println!(
        "ACTUAL_VOLTR_CLAIM_FLOOR_PASSED authority_asset={} owner_asset=110",
        get_spl_token_amount(&c.svm, user_asset)
    );
}

#[test]
#[ignore = "requires explicit public program/account captures and TS signed wire fixture"]
fn production_ts_signed_wire_executes_real_voltr_and_squads() {
    use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
    use solana_sdk::{
        clock::Clock,
        signature::{Keypair, SeedDerivable},
        transaction::VersionedTransaction,
    };
    use std::{path::Path, str::FromStr};
    let capture =
        Path::new("/Users/user/loyal/reports/vault-protection-2026-11-05/program-capture");
    let fixture: serde_json::Value = serde_json::from_slice(
        &std::fs::read(
            std::env::var("VOLTR_CLAIM_WIRE_FIXTURE")
                .unwrap_or_else(|_| "/private/tmp/vault-claim-guard/ts-claim-wire.json".to_owned()),
        )
        .unwrap(),
    )
    .unwrap();
    let key = |field: &str| Pubkey::from_str(fixture[field].as_str().unwrap()).unwrap();
    use sha2::{Digest, Sha256};
    for (name, expected) in [
        (
            "squads.so",
            "1c95bd7be140589d2aec38a85d7ecfe70ec69639277f622c898f821ab1d636fa",
        ),
        (
            "voltr.so",
            "bf1c1831b3d6350f4340badb942bd2e7bfaca4aa89276cb65e8480aa30d44c56",
        ),
    ] {
        assert_eq!(
            format!(
                "{:x}",
                Sha256::digest(std::fs::read(capture.join(name)).unwrap())
            ),
            expected,
            "public capture changed; requalify its deployed version"
        );
    }
    let owner = Keypair::from_seed(&[7; 32]).unwrap();
    assert_eq!(owner.pubkey(), key("owner"));
    let mut svm = squads_test_harness::new_litesvm()
        .with_blockhash_check(false)
        .with_sigverify(true);
    svm.add_program(
        squads_test_harness::SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        &std::fs::read(capture.join("squads.so")).unwrap(),
    )
    .unwrap();
    let voltr = Pubkey::from_str("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8").unwrap();
    svm.add_program(voltr, &std::fs::read(capture.join("voltr.so")).unwrap())
        .unwrap();
    svm.airdrop(&owner.pubkey(), 10_000_000_000).unwrap();
    squads_test_harness::seed_squads_program_config(
        &mut svm,
        owner.pubkey(),
        squads_test_harness::squads_test_treasury(),
        15,
    );
    let create = squads_test_harness::create_squads_smart_account_instruction(
        owner.pubkey(),
        owner.pubkey(),
        16,
    );
    squads_test_harness::try_send_instructions(&mut svm, &[create], &owner, &[])
        .expect("create fixture Settings with actual Squads SBF");
    assert_eq!(
        squads_test_harness::derive_squads_settings(16).0,
        key("settings")
    );
    let authority = key("authority");
    svm.airdrop(&authority, fixture["authorityLamports"].as_u64().unwrap())
        .unwrap();
    let globals: serde_json::Value =
        serde_json::from_slice(&std::fs::read(capture.join("voltr-accounts.json")).unwrap())
            .unwrap();
    for (address, value) in globals["addresses"]
        .as_array()
        .unwrap()
        .iter()
        .zip(globals["result"]["value"].as_array().unwrap())
    {
        if value.is_null() {
            continue;
        }
        svm.set_account(
            Pubkey::from_str(address.as_str().unwrap()).unwrap(),
            Account {
                lamports: value["lamports"].as_u64().unwrap(),
                owner: Pubkey::from_str(value["owner"].as_str().unwrap()).unwrap(),
                data: BASE64.decode(value["data"][0].as_str().unwrap()).unwrap(),
                executable: false,
                rent_epoch: 0,
            },
        )
        .unwrap();
    }
    let vault = Pubkey::from_str("HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA").unwrap();
    let lp = Pubkey::find_program_address(&[b"vault_lp_mint", vault.as_ref()], &voltr).0;
    let receipt = Pubkey::find_program_address(
        &[
            b"request_withdraw_vault_receipt",
            vault.as_ref(),
            authority.as_ref(),
        ],
        &voltr,
    )
    .0;
    let ata = |owner: &Pubkey, mint: &Pubkey| {
        spl_associated_token_account::get_associated_token_address(owner, mint)
    };
    let source = ata(&authority, &USDC_MINT);
    let destination = ata(&owner.pubkey(), &USDC_MINT);
    let escrow = ata(&receipt, &lp);
    let receipt_data = BASE64
        .decode(fixture["receiptBase64"].as_str().unwrap())
        .unwrap();
    svm.set_account(
        receipt,
        Account {
            lamports: svm.minimum_balance_for_rent_exemption(receipt_data.len()),
            data: receipt_data.clone(),
            owner: voltr,
            executable: false,
            rent_epoch: 0,
        },
    )
    .unwrap();
    seed_spl_token_account(&mut svm, source, USDC_MINT, authority, 500);
    seed_spl_token_account(&mut svm, escrow, lp, receipt, 100);
    let idle_auth =
        Pubkey::find_program_address(&[b"vault_asset_idle_auth", vault.as_ref()], &voltr).0;
    seed_spl_token_account(
        &mut svm,
        ata(&idle_auth, &USDC_MINT),
        USDC_MINT,
        idle_auth,
        1_000_000_000,
    );
    for table in fixture["lookupTables"].as_array().unwrap() {
        let data = BASE64
            .decode(table["dataBase64"].as_str().unwrap())
            .unwrap();
        svm.set_account(
            Pubkey::from_str(table["address"].as_str().unwrap()).unwrap(),
            Account {
                lamports: svm.minimum_balance_for_rent_exemption(data.len()),
                data,
                owner: solana_sdk::address_lookup_table::program::ID,
                executable: false,
                rent_epoch: 0,
            },
        )
        .unwrap();
    }
    let mut clock = svm.get_sysvar::<Clock>();
    clock.slot = 1_000_000_000;
    clock.unix_timestamp = 1_793_920_731;
    svm.set_sysvar(&clock);
    let wire = BASE64
        .decode(fixture["wireBase64"].as_str().unwrap())
        .unwrap();
    assert!(wire.len() <= 1232);
    let transaction: VersionedTransaction = bincode::deserialize(&wire).unwrap();
    assert_eq!(bincode::serialize(&transaction).unwrap(), wire);
    let policy = loyal_actions::derive_action_account(&key("settings"), 1).0;
    for offset in [72, 80, 96] {
        let mut changed = svm.get_account(&receipt).unwrap();
        changed.data[offset] ^= 1;
        svm.set_account(receipt, changed).unwrap();
        let failure = svm
            .simulate_transaction(transaction.clone())
            .expect_err("LP, ceiling and deadline mutations must fail the installed guard");
        assert!(format!("{:?}", failure.err).contains("6064"));
        let mut restored = svm.get_account(&receipt).unwrap();
        restored.data = receipt_data.clone();
        svm.set_account(receipt, restored).unwrap();
    }
    let mut wrong_owner = svm.get_account(&receipt).unwrap();
    wrong_owner.owner = solana_sdk::system_program::ID;
    svm.set_account(receipt, wrong_owner).unwrap();
    let failure = svm
        .simulate_transaction(transaction.clone())
        .expect_err("receipt owner must be Voltr");
    println!("WRONG_RECEIPT_OWNER_REJECTED {:?}", failure.err);
    assert!(failure
        .meta
        .logs
        .iter()
        .any(|line| line.contains("Instruction: ExecuteTransactionSyncV2")));
    assert!(!failure
        .meta
        .logs
        .iter()
        .any(|line| line.contains("Instruction: WithdrawVault")));
    let mut restored = svm.get_account(&receipt).unwrap();
    restored.owner = voltr;
    svm.set_account(receipt, restored).unwrap();
    let mut too_high = transaction.clone();
    let instructions = match &mut too_high.message {
        solana_sdk::message::VersionedMessage::V0(m) => &mut m.instructions,
        _ => panic!("production fixture must be v0"),
    };
    let owner_data = &mut instructions
        .iter_mut()
        .find(|ix| ix.data.len() > 11 && ix.data[8..11] == [2, 1, 0])
        .expect("owner sync transaction")
        .data;
    let at = owner_data.len() - 9;
    assert_eq!(
        u64::from_le_bytes(owner_data[at..at + 8].try_into().unwrap()),
        100
    );
    owner_data[at..at + 8].copy_from_slice(&101u64.to_le_bytes());
    let message = too_high.message.serialize();
    too_high.signatures[0] = owner.sign_message(&message);
    let failure = svm
        .send_transaction(too_high)
        .expect_err("actual Voltr payout cannot satisfy floor101");
    assert!(failure
        .meta
        .logs
        .iter()
        .any(|line| line.contains("insufficient funds")));
    assert_eq!(get_spl_token_amount(&svm, source), 500);
    assert_eq!(get_spl_token_amount(&svm, escrow), 100);
    assert!(svm.get_account(&destination).is_none());
    assert!(svm
        .get_account(&policy)
        .map(|a| a.lamports == 0)
        .unwrap_or(true));
    let result = svm.send_transaction(transaction).expect(
        "unchanged signed TS production wire executes actual programs and real captured ALT",
    );
    assert_eq!(get_spl_token_amount(&svm, destination), 600);
    assert_eq!(get_spl_token_amount(&svm, source), 0);
    assert!(svm
        .get_account(&receipt)
        .map(|a| a.lamports == 0)
        .unwrap_or(true));
    assert!(svm
        .get_account(&policy)
        .map(|a| a.lamports == 0)
        .unwrap_or(true));
    println!("EXACT_TS_PRODUCTION_WIRE_PASSED bytes={} CU={} signature_verified=true blockhash_check=false captured_real_ALT=true owner_USDC=600 policy_closed=true receipt_closed=true",wire.len(),result.compute_units_consumed);
}
