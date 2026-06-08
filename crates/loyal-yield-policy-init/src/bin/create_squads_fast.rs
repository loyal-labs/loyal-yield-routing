use std::{path::PathBuf, thread, time::Duration};

use borsh::BorshSerialize;
use loyal_actions::SQUADS_SMART_ACCOUNT_PROGRAM_ID;
use serde_json::json;
use solana_client::rpc_client::RpcClient;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    signature::{read_keypair_file, Signer},
    transaction::Transaction,
};

const SQUADS_SEED_PREFIX: &[u8] = b"smart_account";
const SQUADS_SEED_SETTINGS: &[u8] = b"settings";
const SQUADS_SEED_SMART_ACCOUNT: &[u8] = b"smart_account";
const SQUADS_PROGRAM_CONFIG_SEED: &[u8] = b"program_config";
const SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR: [u8; 8] = [197, 102, 253, 231, 77, 84, 50, 17];
const SQUADS_FULL_PERMISSIONS_MASK: u8 = 7;
const PROGRAM_CONFIG_SMART_ACCOUNT_INDEX_OFFSET: usize = 8;
const PROGRAM_CONFIG_TREASURY_OFFSET: usize = 64;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args = std::env::args().collect::<Vec<_>>();
    let rpc_url = args
        .get(1)
        .ok_or("usage: create_squads_fast <rpc-url> <keypair> [attempts]")?;
    let keypair = args
        .get(2)
        .map(PathBuf::from)
        .ok_or("usage: create_squads_fast <rpc-url> <keypair> [attempts]")?;
    let attempts = args
        .get(3)
        .map(|value| value.parse::<usize>())
        .transpose()?
        .unwrap_or(8);

    let rpc = RpcClient::new(rpc_url.clone());
    let payer = read_keypair_file(&keypair)
        .map_err(|err| format!("failed to read keypair {}: {err}", keypair.display()))?;
    let program_config = derive_program_config();

    let mut last_error = None;
    for attempt in 1..=attempts {
        let account = rpc.get_account(&program_config)?;
        let smart_account_index =
            read_u128(&account.data, PROGRAM_CONFIG_SMART_ACCOUNT_INDEX_OFFSET)?;
        let treasury = read_pubkey(&account.data, PROGRAM_CONFIG_TREASURY_OFFSET)?;
        let seed = smart_account_index
            .checked_add(1)
            .ok_or("smart account index overflow")?;
        let settings = derive_settings(seed);
        let vault = derive_vault(settings, 0);
        let instruction =
            create_smart_account_instruction(payer.pubkey(), &[payer.pubkey()], settings, treasury);
        let blockhash = rpc.get_latest_blockhash()?;
        let transaction = Transaction::new_signed_with_payer(
            &[instruction],
            Some(&payer.pubkey()),
            &[&payer],
            blockhash,
        );

        match rpc.send_and_confirm_transaction(&transaction) {
            Ok(signature) => {
                println!(
                    "{}",
                    serde_json::to_string_pretty(&json!({
                        "signature": signature.to_string(),
                        "attempt": attempt,
                        "settings": settings.to_string(),
                        "vault": vault.to_string(),
                        "smartAccountSeed": seed.to_string(),
                    }))?
                );
                return Ok(());
            }
            Err(error) => {
                last_error = Some(error.to_string());
                thread::sleep(Duration::from_millis(350));
            }
        }
    }

    Err(format!(
        "failed to create Squads smart account after {attempts} attempts: {}",
        last_error.unwrap_or_else(|| "unknown error".to_owned())
    )
    .into())
}

fn derive_program_config() -> Pubkey {
    Pubkey::find_program_address(
        &[SQUADS_SEED_PREFIX, SQUADS_PROGRAM_CONFIG_SEED],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

fn derive_settings(seed: u128) -> Pubkey {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            SQUADS_SEED_SETTINGS,
            &seed.to_le_bytes(),
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

fn derive_vault(settings: Pubkey, vault_index: u8) -> Pubkey {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            settings.as_ref(),
            SQUADS_SEED_SMART_ACCOUNT,
            &[vault_index],
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

fn create_smart_account_instruction(
    payer: Pubkey,
    signers: &[Pubkey],
    settings: Pubkey,
    treasury: Pubkey,
) -> Instruction {
    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(derive_program_config(), false),
            AccountMeta::new(treasury, false),
            AccountMeta::new(payer, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new(settings, false),
        ],
        data: serialize_create_smart_account_args(signers),
    }
}

fn serialize_create_smart_account_args(signers: &[Pubkey]) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR);
    Option::<Pubkey>::None.serialize(&mut data).unwrap();
    1u16.serialize(&mut data).unwrap();
    (signers.len() as u32).serialize(&mut data).unwrap();
    for signer in signers {
        signer.serialize(&mut data).unwrap();
        SQUADS_FULL_PERMISSIONS_MASK.serialize(&mut data).unwrap();
    }
    0u32.serialize(&mut data).unwrap();
    Option::<Pubkey>::None.serialize(&mut data).unwrap();
    Option::<String>::None.serialize(&mut data).unwrap();
    data
}

fn read_u128(data: &[u8], offset: usize) -> Result<u128, Box<dyn std::error::Error>> {
    let bytes = data
        .get(offset..offset + 16)
        .ok_or("program config missing smart account index")?;
    let mut value = [0u8; 16];
    value.copy_from_slice(bytes);
    Ok(u128::from_le_bytes(value))
}

fn read_pubkey(data: &[u8], offset: usize) -> Result<Pubkey, Box<dyn std::error::Error>> {
    let bytes = data
        .get(offset..offset + 32)
        .ok_or("program config missing treasury")?;
    let mut value = [0u8; 32];
    value.copy_from_slice(bytes);
    Ok(Pubkey::new_from_array(value))
}
