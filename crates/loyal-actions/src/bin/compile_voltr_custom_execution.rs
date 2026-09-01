use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use loyal_actions::{
    compile_squads_inner_instruction, execute_program_interaction_policy_instruction,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use std::{io::Read, str::FromStr};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Input {
    policy: String,
    delegated_signer: String,
    account_index: u8,
    constraint_indices: Vec<u8>,
    inner: Vec<WireInstruction>,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct WireInstruction {
    program_id: String,
    accounts: Vec<WireAccount>,
    data_base64: String,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct WireAccount {
    address: String,
    signer: bool,
    writable: bool,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Output {
    schema: &'static str,
    source_sha256: String,
    instruction: WireInstruction,
}

fn key(value: &str, label: &str) -> Result<Pubkey, String> {
    Pubkey::from_str(value).map_err(|error| format!("invalid {label}: {error}"))
}

fn instruction(value: WireInstruction) -> Result<Instruction, String> {
    Ok(Instruction {
        program_id: key(&value.program_id, "instruction program")?,
        accounts: value
            .accounts
            .into_iter()
            .map(|account| {
                let address = key(&account.address, "instruction account")?;
                Ok(match (account.writable, account.signer) {
                    (true, true) => AccountMeta::new(address, true),
                    (true, false) => AccountMeta::new(address, false),
                    (false, true) => AccountMeta::new_readonly(address, true),
                    (false, false) => AccountMeta::new_readonly(address, false),
                })
            })
            .collect::<Result<Vec<_>, String>>()?,
        data: BASE64
            .decode(value.data_base64)
            .map_err(|error| format!("invalid instruction data: {error}"))?,
    })
}

fn wire(value: Instruction) -> WireInstruction {
    WireInstruction {
        program_id: value.program_id.to_string(),
        accounts: value
            .accounts
            .into_iter()
            .map(|account| WireAccount {
                address: account.pubkey.to_string(),
                signer: account.is_signer,
                writable: account.is_writable,
            })
            .collect(),
        data_base64: BASE64.encode(value.data),
    }
}

fn run() -> Result<(), String> {
    let mut source = Vec::new();
    std::io::stdin()
        .read_to_end(&mut source)
        .map_err(|error| error.to_string())?;
    let input: Input = serde_json::from_slice(&source)
        .map_err(|error| format!("invalid execution input: {error}"))?;
    if input.inner.is_empty() || input.inner.len() != input.constraint_indices.len() {
        return Err(
            "inner instructions and constraint indices must be nonempty and aligned".into(),
        );
    }
    let mut transaction_accounts = Vec::new();
    let compiled = input
        .inner
        .into_iter()
        .map(|value| {
            instruction(value).map(|instruction| {
                compile_squads_inner_instruction(&mut transaction_accounts, instruction)
            })
        })
        .collect::<Result<Vec<_>, String>>()?;
    let output = execute_program_interaction_policy_instruction(
        key(&input.policy, "policy")?,
        key(&input.delegated_signer, "delegated signer")?,
        input.account_index,
        compiled,
        input.constraint_indices,
        transaction_accounts,
    );
    println!(
        "{}",
        serde_json::to_string(&Output {
            schema: "loyal-voltr-custom-execution/v2",
            source_sha256: format!("{:x}", Sha256::digest(&source)),
            instruction: wire(output),
        })
        .map_err(|error| error.to_string())?
    );
    Ok(())
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
