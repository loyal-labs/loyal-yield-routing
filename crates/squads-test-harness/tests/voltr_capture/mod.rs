use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use litesvm::LiteSVM;
use sha2::{Digest, Sha256};
use solana_sdk::{
    account::Account,
    clock::Clock,
    pubkey::Pubkey,
    signature::{Keypair, SeedDerivable},
    signer::Signer,
    transaction::VersionedTransaction,
};
use std::{path::PathBuf, str::FromStr};

pub const VOLTR: Pubkey = solana_sdk::pubkey!("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8");
pub const VAULT: Pubkey = solana_sdk::pubkey!("HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA");
pub const LIGHTHOUSE: Pubkey = solana_sdk::pubkey!("L2TExMFKdjpN9kozasaurPirfHy9P8sbXoAN1qA3S95");
pub const SETTINGS_SEED: u128 = 16;

pub fn path(relative: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("fixtures")
        .join(relative)
}

pub fn json(relative: &str) -> serde_json::Value {
    serde_json::from_slice(&std::fs::read(path(relative)).unwrap()).unwrap()
}

pub fn key(value: &serde_json::Value) -> Pubkey {
    Pubkey::from_str(value.as_str().unwrap()).unwrap()
}

pub fn pda(seed: &[u8]) -> Pubkey {
    Pubkey::find_program_address(&[seed, VAULT.as_ref()], &VOLTR).0
}

pub fn ata(owner: &Pubkey, mint: &Pubkey) -> Pubkey {
    spl_associated_token_account::get_associated_token_address(owner, mint)
}

pub fn owner() -> Keypair {
    Keypair::from_seed(&[7; 32]).unwrap()
}

fn program(svm: &mut LiteSVM, id: Pubkey, relative: &str) {
    let bytes = std::fs::read(path(relative)).unwrap();
    let provenance = json(&relative.replace(".so", ".json"));
    assert_eq!(
        format!("{:x}", Sha256::digest(&bytes)),
        provenance["sha256"].as_str().unwrap(),
        "{relative} differs from its recorded mainnet capture"
    );
    svm.add_program(id, &bytes).unwrap();
}

fn load_accounts(svm: &mut LiteSVM, relative: &str) {
    let captured = json(relative);
    for (address, value) in captured["addresses"]
        .as_array()
        .unwrap()
        .iter()
        .zip(captured["result"]["value"].as_array().unwrap())
    {
        if value.is_null() {
            continue;
        }
        let data = BASE64.decode(value["data"][0].as_str().unwrap()).unwrap();
        svm.set_account(
            key(address),
            Account {
                lamports: value["lamports"].as_u64().unwrap(),
                owner: key(&value["owner"]),
                data,
                executable: false,
                rent_epoch: 0,
            },
        )
        .unwrap();
    }
}

/// Captured mainnet Squads, Voltr and Lighthouse binaries over the captured
/// HXtk vault state and Earn MAX lookup tables, with signature verification on.
/// Settings seed 16 is created through the real Squads program for `owner()`.
pub fn captured_svm(slot: u64, unix_timestamp: i64) -> LiteSVM {
    let owner = owner();
    let mut svm = squads_test_harness::new_litesvm()
        .with_blockhash_check(false)
        .with_sigverify(true)
        .with_transaction_history(0);
    program(
        &mut svm,
        squads_test_harness::SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        "squads/squads_smart_account_mainnet.so",
    );
    program(&mut svm, VOLTR, "voltr/voltr.so");
    program(&mut svm, LIGHTHOUSE, "lighthouse/lighthouse.so");
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
        SETTINGS_SEED,
    );
    squads_test_harness::try_send_instructions(&mut svm, &[create], &owner, &[])
        .expect("create fixture Settings with captured Squads");
    load_accounts(&mut svm, "voltr/voltr-accounts.json");
    load_accounts(&mut svm, "voltr/lookup-tables.json");
    set_clock(&mut svm, slot, unix_timestamp);
    svm
}

pub fn set_clock(svm: &mut LiteSVM, slot: u64, unix_timestamp: i64) {
    let mut clock = svm.get_sysvar::<Clock>();
    clock.slot = slot;
    clock.unix_timestamp = unix_timestamp;
    svm.set_sysvar(&clock);
}

pub fn wire(fixture: &serde_json::Value) -> (Vec<u8>, VersionedTransaction) {
    let wire = BASE64
        .decode(fixture["wireBase64"].as_str().unwrap())
        .unwrap();
    assert!(wire.len() <= 1232, "packet is {} bytes", wire.len());
    let transaction: VersionedTransaction = bincode::deserialize(&wire).unwrap();
    assert_eq!(bincode::serialize(&transaction).unwrap(), wire);
    (wire, transaction)
}

pub fn resign(transaction: &mut VersionedTransaction) {
    let message = transaction.message.serialize();
    transaction.signatures[0] = owner().sign_message(&message);
}
