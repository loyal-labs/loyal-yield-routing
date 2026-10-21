use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use loyal_actions::{
    compile_squads_inner_instruction, execute_program_interaction_policy_instruction,
};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use std::str::FromStr;

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Fixture {
    provenance: Provenance,
    request_json: String,
    expected: Expected,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Provenance {
    input_sha256: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Expected {
    source_sha256: String,
    instruction: WireInstruction,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Request {
    policy: String,
    delegated_signer: String,
    account_index: u8,
    constraint_indices: Vec<u8>,
    inner: Vec<WireInstruction>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct WireInstruction {
    program_id: String,
    accounts: Vec<WireAccount>,
    data_base64: String,
}

#[derive(Deserialize)]
struct WireAccount {
    address: String,
    signer: bool,
    writable: bool,
}

fn instruction(wire: &WireInstruction) -> Instruction {
    Instruction {
        program_id: Pubkey::from_str(&wire.program_id).unwrap(),
        accounts: wire
            .accounts
            .iter()
            .map(|account| AccountMeta {
                pubkey: Pubkey::from_str(&account.address).unwrap(),
                is_signer: account.signer,
                is_writable: account.writable,
            })
            .collect(),
        data: BASE64.decode(&wire.data_base64).unwrap(),
    }
}

#[test]
fn golden_squads_execute_envelope() {
    let fixture: Fixture =
        serde_json::from_str(include_str!("../fixtures/squads-execute-envelope-v1.json")).unwrap();
    let digest = format!("{:x}", Sha256::digest(fixture.request_json.as_bytes()));
    assert_eq!(digest, fixture.provenance.input_sha256);
    assert_eq!(digest, fixture.expected.source_sha256);

    let request: Request = serde_json::from_str(&fixture.request_json).unwrap();
    let mut transaction_accounts = Vec::new();
    let compiled = request
        .inner
        .iter()
        .map(|wire| compile_squads_inner_instruction(&mut transaction_accounts, instruction(wire)))
        .collect();
    let actual = execute_program_interaction_policy_instruction(
        Pubkey::from_str(&request.policy).unwrap(),
        Pubkey::from_str(&request.delegated_signer).unwrap(),
        request.account_index,
        compiled,
        request.constraint_indices,
        transaction_accounts,
    );
    assert_eq!(actual, instruction(&fixture.expected.instruction));
}
