use anyhow::{bail, Context, Result};
use borsh::{BorshDeserialize, BorshSerialize};
use loyal_actions::SQUADS_SMART_ACCOUNT_PROGRAM_ID;
use solana_sdk::{
    hash::hashv,
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use solana_system_interface::program as system_program;

const CREATE_SMART_ACCOUNT_DISCRIMINATOR: [u8; 8] = [197, 102, 253, 231, 77, 84, 50, 17];
const FULL_PERMISSIONS_MASK: u8 = 7;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ProgramConfigAccount {
    pub smart_account_index: u128,
    pub authority: Pubkey,
    pub smart_account_creation_fee: u64,
    pub treasury: Pubkey,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SettingsAccount {
    pub seed: u128,
    pub settings_authority: Pubkey,
    pub threshold: u16,
    pub time_lock: u32,
    pub signers: Vec<SmartAccountSigner>,
    pub policy_seed: Option<u64>,
}

#[derive(Clone, Debug, PartialEq, Eq, BorshSerialize, BorshDeserialize)]
pub struct SmartAccountSigner {
    pub key: Pubkey,
    pub permissions: Permissions,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, BorshSerialize, BorshDeserialize)]
pub struct Permissions {
    pub mask: u8,
}

#[allow(dead_code)]
#[derive(BorshDeserialize)]
struct ProgramConfigWire {
    discriminator: [u8; 8],
    smart_account_index: u128,
    authority: Pubkey,
    smart_account_creation_fee: u64,
    treasury: Pubkey,
    reserved: [u8; 64],
}

#[allow(dead_code)]
#[derive(BorshDeserialize)]
struct SettingsWire {
    discriminator: [u8; 8],
    seed: u128,
    settings_authority: Pubkey,
    threshold: u16,
    time_lock: u32,
    transaction_index: u64,
    stale_transaction_index: u64,
    archival_authority: Option<Pubkey>,
    archivable_after: u64,
    bump: u8,
    signers: Vec<SmartAccountSigner>,
    account_utilization: u8,
    policy_seed: Option<u64>,
    reserved2: u8,
}

#[derive(BorshSerialize)]
struct CreateSmartAccountArgs {
    settings_authority: Option<Pubkey>,
    threshold: u16,
    signers: Vec<SmartAccountSigner>,
    time_lock: u32,
    rent_collector: Option<Pubkey>,
    memo: Option<String>,
}

pub fn derive_program_config() -> Pubkey {
    Pubkey::find_program_address(
        &[b"smart_account", b"program_config"],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

pub fn derive_settings(account_index: u128) -> Pubkey {
    Pubkey::find_program_address(
        &[b"smart_account", b"settings", &account_index.to_le_bytes()],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

pub fn derive_vault(settings: Pubkey, vault_index: u8) -> Pubkey {
    Pubkey::find_program_address(
        &[
            b"smart_account",
            settings.as_ref(),
            b"smart_account",
            &[vault_index],
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

pub fn create_smart_account_instruction(
    creator: Pubkey,
    treasury: Pubkey,
    settings: Pubkey,
) -> Result<Instruction> {
    let args = CreateSmartAccountArgs {
        settings_authority: None,
        threshold: 1,
        signers: vec![SmartAccountSigner {
            key: creator,
            permissions: Permissions {
                mask: FULL_PERMISSIONS_MASK,
            },
        }],
        time_lock: 0,
        rent_collector: None,
        memo: Some("Loyal autonomous treasury vault".to_owned()),
    };
    let mut data = CREATE_SMART_ACCOUNT_DISCRIMINATOR.to_vec();
    args.serialize(&mut data)
        .context("serialize createSmartAccount arguments")?;

    Ok(Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(derive_program_config(), false),
            AccountMeta::new(treasury, false),
            AccountMeta::new(creator, true),
            AccountMeta::new_readonly(system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new(settings, false),
        ],
        data,
    })
}

pub fn decode_program_config(data: &[u8]) -> Result<ProgramConfigAccount> {
    let wire = ProgramConfigWire::try_from_slice(data).context("decode Squads ProgramConfig")?;
    if wire.discriminator != account_discriminator("ProgramConfig") {
        bail!("Squads ProgramConfig discriminator mismatch");
    }
    Ok(ProgramConfigAccount {
        smart_account_index: wire.smart_account_index,
        authority: wire.authority,
        smart_account_creation_fee: wire.smart_account_creation_fee,
        treasury: wire.treasury,
    })
}

pub fn decode_settings(data: &[u8]) -> Result<SettingsAccount> {
    let wire = SettingsWire::try_from_slice(data).context("decode Squads Settings")?;
    if wire.discriminator != account_discriminator("Settings") {
        bail!("Squads Settings discriminator mismatch");
    }
    Ok(SettingsAccount {
        seed: wire.seed,
        settings_authority: wire.settings_authority,
        threshold: wire.threshold,
        time_lock: wire.time_lock,
        signers: wire.signers,
        policy_seed: wire.policy_seed,
    })
}

fn account_discriminator(name: &str) -> [u8; 8] {
    let preimage = format!("account:{name}");
    hashv(&[preimage.as_bytes()]).to_bytes()[..8]
        .try_into()
        .expect("Anchor discriminator is eight bytes")
}

pub fn verify_created_settings(
    account_owner: Pubkey,
    data: &[u8],
    expected_seed: u128,
    expected_signer: Pubkey,
) -> Result<SettingsAccount> {
    if account_owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        bail!("Settings account owner is not the Squads v5 program");
    }
    let settings = decode_settings(data)?;
    if settings.seed != expected_seed
        || settings.settings_authority != Pubkey::default()
        || settings.threshold != 1
        || settings.time_lock != 0
        || settings.signers
            != vec![SmartAccountSigner {
                key: expected_signer,
                permissions: Permissions {
                    mask: FULL_PERMISSIONS_MASK,
                },
            }]
    {
        bail!("created Settings account does not match the autonomous vault manifest");
    }
    Ok(settings)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn creation_instruction_uses_current_generated_layout() {
        let creator = Pubkey::new_unique();
        let treasury = Pubkey::new_unique();
        let settings = derive_settings(7);
        let instruction =
            create_smart_account_instruction(creator, treasury, settings).expect("instruction");

        assert_eq!(instruction.program_id, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        assert_eq!(&instruction.data[..8], &CREATE_SMART_ACCOUNT_DISCRIMINATOR);
        assert_eq!(instruction.accounts[0].pubkey, derive_program_config());
        assert_eq!(instruction.accounts[1].pubkey, treasury);
        assert_eq!(instruction.accounts[2].pubkey, creator);
        assert!(instruction.accounts[2].is_signer);
        assert_eq!(instruction.accounts[5].pubkey, settings);
    }

    #[test]
    fn vault_derivation_is_stable() {
        let settings = derive_settings(1);
        assert_ne!(derive_vault(settings, 0), derive_vault(settings, 1));
    }
}
