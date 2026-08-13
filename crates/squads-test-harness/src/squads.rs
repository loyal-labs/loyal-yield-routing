use borsh::BorshSerialize;
use litesvm::LiteSVM;
use solana_sdk::{
    account::Account,
    hash::hashv,
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use solana_system_interface::instruction as system_instruction;

use crate::*;

pub fn new_litesvm() -> LiteSVM {
    LiteSVM::new()
}

pub fn derive_squads_settings(seed: u128) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            SQUADS_SEED_SETTINGS,
            &seed.to_le_bytes(),
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

pub fn derive_squads_pool(seed: u128) -> SquadsPool {
    let (settings, settings_bump) = derive_squads_settings(seed);
    SquadsPool {
        seed,
        settings,
        settings_bump,
    }
}

pub fn derive_squads_vault(squads_settings: &Pubkey, vault_index: u8) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            squads_settings.as_ref(),
            SQUADS_SEED_SMART_ACCOUNT,
            &[vault_index],
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

pub fn derive_squads_policy(squads_settings: &Pubkey, policy_seed: u64) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            SQUADS_SEED_POLICY,
            squads_settings.as_ref(),
            &policy_seed.to_le_bytes(),
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

pub fn derive_squads_program_config() -> Pubkey {
    Pubkey::find_program_address(
        &[SQUADS_SEED_PREFIX, SQUADS_PROGRAM_CONFIG_SEED],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

pub fn anchor_instruction_discriminator(name: &str) -> [u8; 8] {
    let preimage = format!("global:{name}");
    let hash = hashv(&[preimage.as_bytes()]).to_bytes();
    hash[..8].try_into().unwrap()
}

pub fn squads_test_treasury() -> Pubkey {
    Pubkey::new_from_array(hash32(b"loyal-yield-routing-squads-treasury"))
}

pub fn serialize_squads_program_config(
    authority: Pubkey,
    treasury: Pubkey,
    smart_account_index: u128,
) -> Vec<u8> {
    let mut data = Vec::with_capacity(160);
    data.extend_from_slice(&SQUADS_PROGRAM_CONFIG_DISCRIMINATOR);
    smart_account_index.serialize(&mut data).unwrap();
    authority.serialize(&mut data).unwrap();
    0u64.serialize(&mut data).unwrap();
    treasury.serialize(&mut data).unwrap();
    [0u8; 64].serialize(&mut data).unwrap();
    data
}

pub fn seed_squads_program_config(
    svm: &mut LiteSVM,
    authority: Pubkey,
    treasury: Pubkey,
    smart_account_index: u128,
) -> Pubkey {
    let program_config = derive_squads_program_config();
    let data = serialize_squads_program_config(authority, treasury, smart_account_index);

    svm.set_account(
        program_config,
        Account {
            lamports: 1_000_000_000,
            data,
            owner: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed Squads program config account");

    program_config
}

pub fn serialize_squads_create_smart_account_args(verifier: Pubkey) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR);
    Option::<Pubkey>::None.serialize(&mut data).unwrap();
    1u16.serialize(&mut data).unwrap();
    1u32.serialize(&mut data).unwrap();
    verifier.serialize(&mut data).unwrap();
    SQUADS_FULL_PERMISSIONS_MASK.serialize(&mut data).unwrap();
    0u32.serialize(&mut data).unwrap();
    Option::<Pubkey>::None.serialize(&mut data).unwrap();
    Option::<String>::None.serialize(&mut data).unwrap();
    data
}

pub fn create_squads_smart_account_instruction(
    payer: Pubkey,
    verifier: Pubkey,
    seed: u128,
) -> Instruction {
    create_squads_smart_account_instruction_with_treasury(
        payer,
        verifier,
        seed,
        squads_test_treasury(),
    )
}

pub fn create_squads_smart_account_instruction_with_treasury(
    payer: Pubkey,
    verifier: Pubkey,
    seed: u128,
    treasury: Pubkey,
) -> Instruction {
    assert!(seed > 0, "Squads smart-account seed starts at 1");
    let program_config = derive_squads_program_config();
    let (settings, _) = derive_squads_settings(seed);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(program_config, false),
            AccountMeta::new(treasury, false),
            AccountMeta::new(payer, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new(settings, false),
        ],
        data: serialize_squads_create_smart_account_args(verifier),
    }
}

pub fn squads_system_transfer_payload(lamports: u64) -> Vec<u8> {
    let mut transfer_data = system_transfer_data(lamports);
    let mut payload = Vec::with_capacity(7 + transfer_data.len());
    payload.push(1);
    payload.push(2);
    payload.push(2);
    payload.push(0);
    payload.push(1);
    payload.extend_from_slice(&(transfer_data.len() as u16).to_le_bytes());
    payload.append(&mut transfer_data);
    payload
}

pub fn system_transfer_data(lamports: u64) -> Vec<u8> {
    system_instruction::transfer(&Pubkey::default(), &Pubkey::default(), lamports).data
}

#[derive(Debug)]
pub struct SquadsCompiledInstruction {
    pub program_id_index: usize,
    pub accounts: Vec<usize>,
    pub data: Vec<u8>,
}

pub fn squads_compiled_instruction_payload(instructions: &[SquadsCompiledInstruction]) -> Vec<u8> {
    let mut payload = Vec::new();
    payload.push(
        instructions
            .len()
            .try_into()
            .expect("Squads sync payload supports up to 255 instructions"),
    );

    for instruction in instructions {
        payload.push(
            instruction
                .program_id_index
                .try_into()
                .expect("program id index fits in u8"),
        );
        payload.push(
            instruction
                .accounts
                .len()
                .try_into()
                .expect("account index count fits in u8"),
        );
        for account in &instruction.accounts {
            payload.push(
                (*account)
                    .try_into()
                    .expect("account index fits in Squads u8 account index"),
            );
        }
        payload.extend_from_slice(&(instruction.data.len() as u16).to_le_bytes());
        payload.extend_from_slice(&instruction.data);
    }

    payload
}
