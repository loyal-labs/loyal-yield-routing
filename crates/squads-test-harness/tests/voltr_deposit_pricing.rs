mod voltr_capture;

use litesvm::LiteSVM;
use solana_sdk::{
    account::Account,
    instruction::{AccountMeta, Instruction, InstructionError},
    message::VersionedMessage,
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
    transaction::{TransactionError, VersionedTransaction},
};
use squads_test_harness::{get_spl_token_amount, seed_spl_token_account, USDC_MINT};
use voltr_capture::{
    ata, captured_svm, json, key, pda, resign, set_clock, wire, LIGHTHOUSE, VAULT, VOLTR,
};

const DEPOSIT_DISCRIMINATOR: [u8; 8] = [126, 224, 21, 255, 228, 53, 117, 33];
// Lighthouse AssertionFailed; a multi assertion reports 0x1900 + failing index.
const ASSERTION_FAILED: u32 = 6001;
const MULTI_ASSERTION_FAILED: std::ops::Range<u32> = 0x1900..0x1940;
const VECTOR_UNIX_TS: i64 = 1_793_920_731;

fn direct_deposit(svm: &mut LiteSVM, user: &Keypair, amount: u64) -> u64 {
    let lp = pda(b"vault_lp_mint");
    let idle_auth = pda(b"vault_asset_idle_auth");
    let user_asset = ata(&user.pubkey(), &USDC_MINT);
    let user_lp = ata(&user.pubkey(), &lp);
    svm.airdrop(&user.pubkey(), 1_000_000_000).unwrap();
    seed_spl_token_account(svm, user_asset, USDC_MINT, user.pubkey(), amount);
    seed_spl_token_account(svm, user_lp, lp, user.pubkey(), 0);
    let mut data = DEPOSIT_DISCRIMINATOR.to_vec();
    data.extend_from_slice(&amount.to_le_bytes());
    let deposit = Instruction {
        program_id: VOLTR,
        data,
        accounts: vec![
            AccountMeta::new_readonly(user.pubkey(), true),
            AccountMeta::new_readonly(
                Pubkey::find_program_address(&[b"protocol"], &VOLTR).0,
                false,
            ),
            AccountMeta::new(VAULT, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new(lp, false),
            AccountMeta::new(user_asset, false),
            AccountMeta::new(ata(&idle_auth, &USDC_MINT), false),
            AccountMeta::new_readonly(idle_auth, false),
            AccountMeta::new(user_lp, false),
            AccountMeta::new_readonly(pda(b"vault_lp_mint_auth"), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(solana_sdk::system_program::id(), false),
        ],
    };
    squads_test_harness::try_send_instructions(svm, &[deposit], user, &[])
        .expect("direct Voltr deposit");
    get_spl_token_amount(svm, user_lp)
}

/// LP minted by the captured Voltr binary on fresh captured state; lane A's
/// quote math must reproduce these. VOLTR_LP_VECTORS_WRITE=1 rewrites them.
#[test]
fn captured_voltr_lp_vectors() {
    let vectors: Vec<serde_json::Value> = [1_000_000u64, 250_000_000]
        .into_iter()
        .map(|amount| {
            let mut svm = captured_svm(1, VECTOR_UNIX_TS);
            let minted = direct_deposit(&mut svm, &Keypair::new(), amount);
            serde_json::json!({
                "amountRaw": amount.to_string(),
                "unixTs": VECTOR_UNIX_TS.to_string(),
                "lpMinted": minted.to_string(),
            })
        })
        .collect();
    let document = serde_json::json!({
        "vault": VAULT.to_string(),
        "stateSlot": 465_888_518u64,
        "voltrSha256": json("voltr/voltr.json")["sha256"],
        "vectors": vectors,
    });
    let text = format!("{}\n", serde_json::to_string_pretty(&document).unwrap());
    let path = voltr_capture::path("voltr-deposit-lp-vectors.json");
    if std::env::var("VOLTR_LP_VECTORS_WRITE").is_ok() {
        std::fs::write(&path, &text).unwrap();
    }
    assert_eq!(std::fs::read_to_string(path).unwrap(), text);
    println!(
        "LP_VECTORS {}",
        serde_json::to_string(&document["vectors"]).unwrap()
    );
}

struct Deposit {
    svm: LiteSVM,
    fixture: serde_json::Value,
    lp: Pubkey,
    lp_ata: Pubkey,
    owner_usdc: Pubkey,
    slot: u64,
    unix_ts: i64,
}

fn quote_u64(fixture: &serde_json::Value, field: &str) -> u64 {
    fixture["quote"][field].as_str().unwrap().parse().unwrap()
}

fn deposit_setup() -> Deposit {
    let fixture = json("voltr/deposit-wire.json");
    let slot = quote_u64(&fixture, "certifiedSlot") + 1;
    let unix_ts = fixture["nowUnixTs"].as_str().unwrap().parse().unwrap();
    let mut svm = captured_svm(slot, unix_ts);
    let owner = voltr_capture::owner();
    let authority = key(&fixture["authority"]);
    let lp = pda(b"vault_lp_mint");
    let owner_usdc = ata(&owner.pubkey(), &USDC_MINT);
    seed_spl_token_account(
        &mut svm,
        owner_usdc,
        USDC_MINT,
        owner.pubkey(),
        1_000_000_000,
    );
    assert_eq!(owner.pubkey(), key(&fixture["owner"]));
    assert_eq!(
        squads_test_harness::derive_squads_settings(voltr_capture::SETTINGS_SEED).0,
        key(&fixture["settings"])
    );
    Deposit {
        svm,
        lp,
        lp_ata: ata(&authority, &lp),
        owner_usdc,
        fixture,
        slot,
        unix_ts,
    }
}

fn snapshot(d: &Deposit) -> (Vec<u8>, Vec<u8>, u64, Option<Account>) {
    (
        d.svm.get_account(&VAULT).unwrap().data,
        d.svm.get_account(&d.lp).unwrap().data,
        get_spl_token_amount(&d.svm, d.owner_usdc),
        d.svm.get_account(&d.lp_ata).filter(|a| a.lamports > 0),
    )
}

fn assertion_failure(d: &mut Deposit, transaction: &VersionedTransaction, case: &str) {
    let before = snapshot(d);
    let failure = d.svm.send_transaction(transaction.clone()).expect_err(case);
    let TransactionError::InstructionError(index, InstructionError::Custom(code)) = failure.err
    else {
        panic!("{case}: {:?}", failure.err);
    };
    let program = transaction.message.instructions()[index as usize]
        .program_id(transaction.message.static_account_keys());
    assert_eq!(*program, LIGHTHOUSE, "{case}");
    assert!(
        code == ASSERTION_FAILED || MULTI_ASSERTION_FAILED.contains(&code),
        "{case}: {code}"
    );
    assert!(before == snapshot(d), "{case}: state not rolled back");
    println!(
        "REJECTED {case} at instruction {index} code {code} CU={}",
        failure.meta.compute_units_consumed
    );
}

fn with_account(
    d: &mut Deposit,
    address: Pubkey,
    change: impl FnOnce(&mut Account),
    run: impl FnOnce(&mut Deposit),
) {
    let original = d.svm.get_account(&address);
    let mut changed = original.clone().unwrap_or_default();
    change(&mut changed);
    d.svm.set_account(address, changed).unwrap();
    run(d);
    match original {
        Some(account) => d.svm.set_account(address, account).unwrap(),
        None => d.svm.set_account(address, Account::default()).unwrap(),
    }
}

fn substitute(
    transaction: &VersionedTransaction,
    from: Pubkey,
    to: Pubkey,
) -> VersionedTransaction {
    let mut changed = transaction.clone();
    let VersionedMessage::V0(message) = &mut changed.message else {
        panic!("v0 wire")
    };
    let at = message
        .account_keys
        .iter()
        .position(|k| *k == from)
        .expect("static key");
    message.account_keys[at] = to;
    resign(&mut changed);
    changed
}

#[test]
fn production_ts_deposit_wire_enforces_certified_pricing() {
    let mut d = deposit_setup();
    let (bytes, transaction) = wire(&d.fixture);
    let (_, overmint) = wire(&d.fixture["overmint"]);
    let good = d
        .svm
        .simulate_transaction(transaction.clone())
        .expect("unchanged TS deposit wire");
    let expires_at = quote_u64(&d.fixture, "expiresAt") as i64;
    let not_after = quote_u64(&d.fixture, "notAfterSlot");
    let (slot, unix_ts) = (d.slot, d.unix_ts);

    set_clock(&mut d.svm, slot, expires_at + 1);
    assertion_failure(&mut d, &transaction, "stale timestamp");
    set_clock(&mut d.svm, not_after + 1, unix_ts);
    assertion_failure(&mut d, &transaction, "slot past notAfterSlot");
    set_clock(&mut d.svm, slot, unix_ts);

    for (case, offset) in [
        ("changed total_value", 168),
        ("changed issuance fee", 522),
        ("changed accumulator", 584),
    ] {
        with_account(
            &mut d,
            VAULT,
            |a| a.data[offset] ^= 1,
            |d| assertion_failure(d, &transaction, case),
        );
    }
    let lp = d.lp;
    with_account(
        &mut d,
        lp,
        |a| a.data[36] ^= 1,
        |d| assertion_failure(d, &transaction, "changed LP supply"),
    );

    let restore: Vec<_> = [VAULT, d.lp, ata(&pda(b"vault_asset_idle_auth"), &USDC_MINT)]
        .into_iter()
        .map(|address| (address, d.svm.get_account(&address).unwrap()))
        .collect();
    direct_deposit(&mut d.svm, &Keypair::new(), 1_000_000);
    assertion_failure(&mut d, &transaction, "interleaved deposit");
    for (address, account) in restore {
        d.svm.set_account(address, account).unwrap();
    }

    let lp_ata = d.lp_ata;
    let authority = key(&d.fixture["authority"]);
    seed_spl_token_account(&mut d.svm, lp_ata, d.lp, authority, 1);
    assertion_failure(&mut d, &transaction, "donated LP");
    d.svm.set_account(lp_ata, Account::default()).unwrap();

    let mut short = transaction.clone();
    let amount = quote_u64(&d.fixture, "amountRaw");
    let mut needle = DEPOSIT_DISCRIMINATOR.to_vec();
    needle.extend_from_slice(&amount.to_le_bytes());
    let VersionedMessage::V0(message) = &mut short.message else {
        panic!("v0 wire")
    };
    let data = message
        .instructions
        .iter_mut()
        .map(|ix| &mut ix.data)
        .find(|data| data.windows(16).any(|w| w == needle.as_slice()))
        .expect("owner sync carries the deposit");
    let at = data
        .windows(16)
        .position(|w| w == needle.as_slice())
        .unwrap()
        + 8;
    data[at..at + 8].copy_from_slice(&(amount - 1).to_le_bytes());
    resign(&mut short);
    assertion_failure(&mut d, &short, "deposit amount mutation");

    for (case, real) in [
        ("vault substitution", VAULT),
        ("LP mint substitution", d.lp),
    ] {
        let clone = Pubkey::new_unique();
        let account = d.svm.get_account(&real).unwrap();
        d.svm.set_account(clone, account).unwrap();
        let before = snapshot(&d);
        let failure = d
            .svm
            .send_transaction(substitute(&transaction, real, clone))
            .expect_err(case);
        assert!(before == snapshot(&d), "{case}: state not rolled back");
        println!("REJECTED {case} {:?}", failure.err);
    }

    assertion_failure(&mut d, &overmint, "overmint (lpMax below actual)");

    let supply_before = u64::from_le_bytes(
        d.svm.get_account(&d.lp).unwrap().data[36..44]
            .try_into()
            .unwrap(),
    );
    let result = d
        .svm
        .send_transaction(transaction)
        .expect("unchanged TS deposit wire lands");
    let minted = get_spl_token_amount(&d.svm, d.lp_ata);
    let vectors = json("voltr-deposit-lp-vectors.json");
    let vector = vectors["vectors"]
        .as_array()
        .unwrap()
        .iter()
        .find(|v| v["amountRaw"].as_str().unwrap() == amount.to_string())
        .unwrap();
    assert_eq!(minted.to_string(), vector["lpMinted"].as_str().unwrap());
    let supply_after = u64::from_le_bytes(
        d.svm.get_account(&d.lp).unwrap().data[36..44]
            .try_into()
            .unwrap(),
    );
    assert_eq!(supply_after - supply_before, minted);
    println!(
        "EXACT_TS_DEPOSIT_WIRE_PASSED bytes={} overmint_bytes={} CU={} simulated_CU={} lp_minted={minted} signature_verified=true",
        bytes.len(),
        bincode::serialize(&overmint).unwrap().len(),
        result.compute_units_consumed,
        good.meta.compute_units_consumed,
    );
}
