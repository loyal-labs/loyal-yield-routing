//! Locked official ALT builders and actual LiteSVM ALT program provenance.
//! Exported initial accounts contain no injected executable program or ALT state.
use base64::{engine::general_purpose::STANDARD, Engine};
use litesvm::LiteSVM;
use serde_json::{json, Map, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{
    address_lookup_table::{instruction, program},
    hash::Hash,
    instruction::Instruction,
    signature::{Keypair, SeedDerivable, Signer},
    sysvar::slot_hashes::SlotHashes,
    transaction::Transaction,
};

fn encode(ix: &Instruction) -> Value {
    json!({"program": ix.program_id.to_string(), "data": STANDARD.encode(&ix.data),
        "accounts": ix.accounts.iter().map(|a| json!({"address":a.pubkey.to_string(),"signer":a.is_signer,"writable":a.is_writable})).collect::<Vec<_>>()})
}
fn signed_packet(instructions: &[Instruction], manager: &Keypair, hash: Hash) -> String {
    STANDARD.encode(
        bincode::serialize(&Transaction::new_signed_with_payer(
            instructions,
            Some(&manager.pubkey()),
            &[manager],
            hash,
        ))
        .unwrap(),
    )
}
#[test]
fn official_alt_goldens_and_actual_program_fixture() {
    let manager = Keypair::from_seed(&[41; 32]).unwrap();
    let addresses = [
        solana_sdk::pubkey::Pubkey::new_from_array([51; 32]),
        solana_sdk::pubkey::Pubkey::new_from_array([52; 32]),
        solana_sdk::pubkey::Pubkey::new_from_array([53; 32]),
    ];
    let mut svm = LiteSVM::new();
    svm.warp_to_slot(1000);
    svm.set_sysvar(&SlotHashes::new(&[
        (999, Hash::new_from_array([44; 32])),
        (998, Hash::new_from_array([43; 32])),
    ]));
    svm.airdrop(&manager.pubkey(), 100_000_000).unwrap();
    let (create, table) = instruction::create_lookup_table(manager.pubkey(), manager.pubkey(), 999);
    let extend = instruction::extend_lookup_table(
        table,
        manager.pubkey(),
        Some(manager.pubkey()),
        addresses[..2].to_vec(),
    );
    let deactivate = instruction::deactivate_lookup_table(table, manager.pubkey());
    let close = instruction::close_lookup_table(table, manager.pubkey(), manager.pubkey());
    let program_account = svm
        .get_account(&program::id())
        .expect("actual ALT program installed by locked LiteSVM");
    assert!(program_account.executable);
    let mut initial = Map::new();
    for key in [manager.pubkey(), solana_sdk::sysvar::slot_hashes::id()] {
        let account = svm.get_account(&key).unwrap();
        assert!(!account.executable);
        initial.insert(key.to_string(),json!({"Address":key.to_string(),"Owner":account.owner.to_string(),"Lamports":account.lamports,"Data":STANDARD.encode(account.data),"Executable":false}));
    }
    assert!(svm.get_account(&table).is_none());
    let original_hash = svm.latest_blockhash();
    let signed_packets = json!({
        "create":signed_packet(&[create.clone(),extend.clone()],&manager,original_hash),
        "extend":signed_packet(&[instruction::extend_lookup_table(table,manager.pubkey(),Some(manager.pubkey()),addresses[2..].to_vec())],&manager,original_hash),
        "deactivate":signed_packet(&[deactivate.clone()],&manager,original_hash),
        "close":signed_packet(&[close.clone()],&manager,original_hash)
    });
    let transaction = Transaction::new_signed_with_payer(
        &[create.clone(), extend.clone()],
        Some(&manager.pubkey()),
        &[&manager],
        svm.latest_blockhash(),
    );
    svm.send_transaction(transaction)
        .expect("official actual ALT create+extend");
    let table_account = svm.get_account(&table).unwrap();
    assert_eq!(table_account.owner, program::id());
    assert_eq!(table_account.data.len(), 56 + 2 * 32);
    let fixture = json!({"provenance":{"sdk":"solana-address-lookup-table-interface 2.2.2 locked official instruction builders","bank":"LiteSVM locked actual ALT program, no mock ALT","program":program::id().to_string(),"program_owner":program_account.owner.to_string(),"program_data_sha256":format!("{:x}",Sha256::digest(&program_account.data)),"slot":1000},"manager":manager.pubkey().to_string(),"table":table.to_string(),"recent_slot":999,"addresses":addresses.iter().map(|a|a.to_string()).collect::<Vec<_>>(),"accounts":initial,"signed_packets":signed_packets,"instructions":{"create":encode(&create),"extend":encode(&extend),"deactivate":encode(&deactivate),"close":encode(&close)}});
    if let Ok(path) = std::env::var("WORKERS_V2_LOOKUP_FIXTURE_OUT") {
        std::fs::write(path, serde_json::to_vec_pretty(&fixture).unwrap()).unwrap();
    }
}
