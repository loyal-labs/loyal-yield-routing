use crate::{AdaptorError, AdaptorResult, ReportV1, StrategyConfig, CONFIG_LEN, REPORT_V1_LEN};
use solana_program::{
    account_info::{next_account_info, AccountInfo},
    clock::Clock,
    entrypoint::ProgramResult,
    program::{invoke, set_return_data},
    program_error::ProgramError,
    program_pack::Pack,
    pubkey,
    pubkey::Pubkey,
    sysvar::{rent::Rent, Sysvar},
};
use solana_system_interface::instruction as system_instruction;
use spl_token::state::{Account as TokenAccount, AccountState, Mint};

pub const PROGRAM_ID: Pubkey = pubkey!("FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW");
pub const SQUADS_PROGRAM_ID: Pubkey = pubkey!("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
pub const VOLTR_PROGRAM_ID: Pubkey = pubkey!("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8");
const ASSOCIATED_TOKEN_PROGRAM_ID: Pubkey = pubkey!("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL");
pub const INITIALIZE_CONFIG_DISCRIMINATOR: [u8; 8] = [208, 127, 21, 1, 194, 190, 196, 70];
pub const INITIALIZE_DISCRIMINATOR: [u8; 8] = [175, 175, 109, 31, 13, 152, 155, 237];
pub const DEPOSIT_DISCRIMINATOR: [u8; 8] = [242, 35, 198, 137, 82, 225, 242, 182];
pub const WITHDRAW_DISCRIMINATOR: [u8; 8] = [183, 18, 70, 156, 148, 109, 161, 34];
const SQUADS_PREFIX: &[u8] = b"smart_account";
const SQUADS_SETTINGS_DISCRIMINATOR: [u8; 8] = [223, 179, 163, 190, 177, 224, 67, 173];

pub fn process_instruction(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    if program_id != &PROGRAM_ID {
        return Err(ProgramError::IncorrectProgramId);
    }
    let discriminator: [u8; 8] = data
        .get(..8)
        .ok_or(AdaptorError::InvalidInstruction)?
        .try_into()
        .map_err(|_| AdaptorError::InvalidInstruction)?;
    match discriminator {
        INITIALIZE_CONFIG_DISCRIMINATOR => initialize_config(program_id, accounts, &data[8..]),
        INITIALIZE_DISCRIMINATOR if data.len() == 8 => initialize(program_id, accounts),
        DEPOSIT_DISCRIMINATOR => capital_path(program_id, accounts, &data[8..], true),
        WITHDRAW_DISCRIMINATOR => capital_path(program_id, accounts, &data[8..], false),
        _ => Err(AdaptorError::InvalidInstruction),
    }
    .map_err(ProgramError::from)
}

fn initialize_config(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> AdaptorResult<()> {
    if data.len() != 17 || accounts.len() != 13 {
        return Err(AdaptorError::InvalidInstruction);
    }
    let mut it = accounts.iter();
    let payer = next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let config_account =
        next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let voltr_program =
        next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let voltr_vault = next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let strategy_auth =
        next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let squads_program =
        next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let settings = next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let settings_signer =
        next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let squads_vault = next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let asset_mint = next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let token_program =
        next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let squads_asset_ata =
        next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    let system_program =
        next_account_info(&mut it).map_err(|_| AdaptorError::InvalidAccountCount)?;
    if !payer.is_signer
        || !payer.is_writable
        || !config_account.is_signer
        || !config_account.is_writable
        || config_account.owner != &solana_program::system_program::ID
        || !config_account.data_is_empty()
        || voltr_program.key != &VOLTR_PROGRAM_ID
        || !voltr_program.executable
        || voltr_vault.owner != voltr_program.key
        || squads_program.key != &SQUADS_PROGRAM_ID
        || !squads_program.executable
        || token_program.key != &spl_token::id()
        || system_program.key != &solana_program::system_program::ID
    {
        return Err(AdaptorError::InvalidAccount);
    }
    let config = StrategyConfig {
        squads_vault_index: data[0],
        voltr_program: *voltr_program.key,
        voltr_vault: *voltr_vault.key,
        strategy: *config_account.key,
        vault_strategy_auth: *strategy_auth.key,
        squads_program: *squads_program.key,
        squads_settings: *settings.key,
        squads_settings_signer: *settings_signer.key,
        squads_vault: *squads_vault.key,
        asset_mint: *asset_mint.key,
        asset_token_program: *token_program.key,
        squads_asset_ata: *squads_asset_ata.key,
        max_report_nav_raw: u64::from_le_bytes(
            data[1..9]
                .try_into()
                .map_err(|_| AdaptorError::InvalidInstruction)?,
        ),
        max_report_age_slots: u64::from_le_bytes(
            data[9..17]
                .try_into()
                .map_err(|_| AdaptorError::InvalidInstruction)?,
        ),
        last_sequence: 0,
        last_observed_slot: 0,
        last_nav_raw: 0,
        last_snapshot_digest: [0; 32],
    };
    if config.max_report_nav_raw == 0 || config.max_report_age_slots == 0 {
        return Err(AdaptorError::InvalidConfig);
    }
    validate_bindings(
        &config,
        strategy_auth,
        settings,
        squads_vault,
        asset_mint,
        token_program,
        squads_asset_ata,
    )?;
    let rent = Rent::get().map_err(|_| AdaptorError::InvalidAccount)?;
    invoke(
        &system_instruction::create_account(
            payer.key,
            config_account.key,
            rent.minimum_balance(CONFIG_LEN),
            CONFIG_LEN as u64,
            program_id,
        ),
        &[
            payer.clone(),
            config_account.clone(),
            system_program.clone(),
        ],
    )
    .map_err(|_| AdaptorError::InvalidAccount)?;
    config.encode(
        &mut config_account
            .try_borrow_mut_data()
            .map_err(|_| AdaptorError::InvalidConfig)?,
    )
}

fn initialize(program_id: &Pubkey, accounts: &[AccountInfo]) -> AdaptorResult<()> {
    if accounts.len() != 10 {
        return Err(AdaptorError::InvalidAccountCount);
    }
    let config = load_config(program_id, &accounts[2])?;
    if !accounts[1].is_signer
        || accounts[3].key != &solana_program::system_program::ID
        || accounts[9].key != &config.squads_program
        || !accounts[9].executable
    {
        return Err(AdaptorError::InvalidAuthority);
    }
    validate_bindings(
        &config,
        &accounts[1],
        &accounts[4],
        &accounts[5],
        &accounts[6],
        &accounts[7],
        &accounts[8],
    )
}

fn capital_path(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
    is_deposit: bool,
) -> AdaptorResult<()> {
    if accounts.len() != 8 || data.len() != 8 + REPORT_V1_LEN {
        return Err(AdaptorError::InvalidInstruction);
    }
    reject_duplicate_mutables(accounts)?;
    let amount = u64::from_le_bytes(
        data[..8]
            .try_into()
            .map_err(|_| AdaptorError::InvalidInstruction)?,
    );
    let report = ReportV1::decode(&data[8..])?;
    let mut config = load_config(program_id, &accounts[1])?;
    if !accounts[1].is_writable {
        return Err(AdaptorError::InvalidAuthority);
    }
    validate_bindings(
        &config,
        &accounts[0],
        &accounts[5],
        &accounts[6],
        &accounts[2],
        &accounts[4],
        &accounts[7],
    )?;
    if !accounts[0].is_signer || !accounts[6].is_signer {
        return Err(AdaptorError::InvalidAuthority);
    }
    let strategy_asset = token(&accounts[3], &config.asset_token_program)?;
    let squads_asset = token(&accounts[7], &config.asset_token_program)?;
    let mint = Mint::unpack(
        &accounts[2]
            .try_borrow_data()
            .map_err(|_| AdaptorError::InvalidTokenAccount)?,
    )
    .map_err(|_| AdaptorError::InvalidTokenAccount)?;
    if accounts[3].key
        != &ata(
            &config.vault_strategy_auth,
            &config.asset_mint,
            &config.asset_token_program,
        )
        || strategy_asset.owner != config.vault_strategy_auth
        || strategy_asset.mint != config.asset_mint
        || squads_asset.owner != config.squads_vault
        || squads_asset.mint != config.asset_mint
        || !plain(&strategy_asset)
        || !plain(&squads_asset)
    {
        return Err(AdaptorError::InvalidTokenAccount);
    }
    if is_deposit && amount > 0 {
        if !accounts[3].is_writable || !accounts[7].is_writable {
            return Err(AdaptorError::InvalidAuthority);
        }
        invoke(
            &spl_token::instruction::transfer_checked(
                accounts[4].key,
                accounts[3].key,
                accounts[2].key,
                accounts[7].key,
                accounts[0].key,
                &[],
                amount,
                mint.decimals,
            )
            .map_err(|_| AdaptorError::InvalidTokenAccount)?,
            &[
                accounts[3].clone(),
                accounts[2].clone(),
                accounts[7].clone(),
                accounts[0].clone(),
                accounts[4].clone(),
            ],
        )
        .map_err(|_| AdaptorError::InvalidTokenAccount)?;
    }
    if !is_deposit && amount > 0 && strategy_asset.amount < amount {
        return Err(AdaptorError::InsufficientBridgeLiquidity);
    }
    accept_report(&mut config, report)?;
    config.encode(
        &mut accounts[1]
            .try_borrow_mut_data()
            .map_err(|_| AdaptorError::InvalidConfig)?,
    )?;
    set_return_data(&report.nav_after_raw.to_le_bytes());
    Ok(())
}

fn accept_report(config: &mut StrategyConfig, report: ReportV1) -> AdaptorResult<()> {
    let clock = Clock::get().map_err(|_| AdaptorError::InvalidReport)?;
    validate_report_fields(config, report, clock.slot)?;
    config.accept_report(report);
    Ok(())
}

fn validate_report_fields(
    config: &StrategyConfig,
    report: ReportV1,
    current_slot: u64,
) -> AdaptorResult<()> {
    if report.sequence
        != config
            .last_sequence
            .checked_add(1)
            .ok_or(AdaptorError::ReportSequence)?
    {
        return Err(AdaptorError::ReportSequence);
    }
    if report.observed_slot < config.last_observed_slot
        || report.observed_slot > current_slot
        || current_slot
            .checked_sub(report.observed_slot)
            .ok_or(AdaptorError::ReportSlot)?
            > config.max_report_age_slots
    {
        return Err(AdaptorError::ReportSlot);
    }
    if report.nav_after_raw > config.max_report_nav_raw {
        return Err(AdaptorError::ReportCap);
    }
    if report.snapshot_digest == [0; 32] {
        return Err(AdaptorError::InvalidReport);
    }
    Ok(())
}

fn validate_bindings(
    config: &StrategyConfig,
    strategy_auth: &AccountInfo,
    settings: &AccountInfo,
    squads_vault: &AccountInfo,
    mint: &AccountInfo,
    token_program: &AccountInfo,
    squads_asset_ata: &AccountInfo,
) -> AdaptorResult<()> {
    let strategy_authority = Pubkey::find_program_address(
        &[
            b"vault_strategy_auth",
            config.voltr_vault.as_ref(),
            config.strategy.as_ref(),
        ],
        &config.voltr_program,
    )
    .0;
    let vault = Pubkey::find_program_address(
        &[
            SQUADS_PREFIX,
            config.squads_settings.as_ref(),
            SQUADS_PREFIX,
            &[config.squads_vault_index],
        ],
        &config.squads_program,
    )
    .0;
    if strategy_auth.key != &config.vault_strategy_auth
        || config.vault_strategy_auth != strategy_authority
        || settings.key != &config.squads_settings
        || settings.owner != &config.squads_program
        || squads_vault.key != &config.squads_vault
        || config.squads_vault != vault
        || mint.key != &config.asset_mint
        || token_program.key != &config.asset_token_program
        || token_program.key != &spl_token::id()
        || squads_asset_ata.key != &config.squads_asset_ata
        || config.squads_asset_ata
            != ata(
                &config.squads_vault,
                &config.asset_mint,
                &config.asset_token_program,
            )
    {
        return Err(AdaptorError::InvalidSquadsVault);
    }
    let settings_data = settings
        .try_borrow_data()
        .map_err(|_| AdaptorError::InvalidSquadsVault)?;
    if !valid_settings_authority_graph(&settings_data, config.squads_settings_signer)? {
        return Err(AdaptorError::InvalidSquadsVault);
    }
    Ok(())
}

fn valid_settings_authority_graph(data: &[u8], expected_signer: Pubkey) -> AdaptorResult<bool> {
    if expected_signer == Pubkey::default()
        || data.len() < 126
        || data[..8] != SQUADS_SETTINGS_DISCRIMINATOR
        || data[24..56] != [0; 32]
        || u16::from_le_bytes(
            data[56..58]
                .try_into()
                .map_err(|_| AdaptorError::InvalidSquadsVault)?,
        ) != 1
        || u32::from_le_bytes(
            data[58..62]
                .try_into()
                .map_err(|_| AdaptorError::InvalidSquadsVault)?,
        ) != 0
    {
        return Ok(false);
    }
    let mut offset = 78;
    match data[offset] {
        0 => offset += 1,
        1 => offset += 33,
        _ => return Ok(false),
    }
    if data.len() < offset + 8 + 1 + 4 + 32 + 1 {
        return Ok(false);
    }
    offset += 8 + 1;
    let signer_count = u32::from_le_bytes(
        data[offset..offset + 4]
            .try_into()
            .map_err(|_| AdaptorError::InvalidSquadsVault)?,
    );
    offset += 4;
    if signer_count != 1
        || Pubkey::new_from_array(
            data[offset..offset + 32]
                .try_into()
                .map_err(|_| AdaptorError::InvalidSquadsVault)?,
        ) != expected_signer
        || data[offset + 32] != 7
    {
        return Ok(false);
    }
    Ok(true)
}
fn load_config(program_id: &Pubkey, account: &AccountInfo) -> AdaptorResult<StrategyConfig> {
    if account.owner != program_id {
        return Err(AdaptorError::InvalidConfig);
    }
    let config = StrategyConfig::decode(
        &account
            .try_borrow_data()
            .map_err(|_| AdaptorError::InvalidConfig)?,
    )?;
    if config.strategy != *account.key {
        return Err(AdaptorError::InvalidConfig);
    }
    Ok(config)
}
fn reject_duplicate_mutables(accounts: &[AccountInfo]) -> AdaptorResult<()> {
    for (index, account) in accounts.iter().enumerate() {
        if account.is_writable
            && accounts[..index]
                .iter()
                .any(|other| other.is_writable && other.key == account.key)
        {
            return Err(AdaptorError::DuplicateMutableAccount);
        }
    }
    Ok(())
}
fn ata(owner: &Pubkey, mint: &Pubkey, token_program: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), token_program.as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}
fn token(account: &AccountInfo, token_program: &Pubkey) -> AdaptorResult<TokenAccount> {
    if account.owner != token_program {
        return Err(AdaptorError::InvalidTokenAccount);
    }
    TokenAccount::unpack(
        &account
            .try_borrow_data()
            .map_err(|_| AdaptorError::InvalidTokenAccount)?,
    )
    .map_err(|_| AdaptorError::InvalidTokenAccount)
}
fn plain(account: &TokenAccount) -> bool {
    account.state == AccountState::Initialized
        && account.delegate.is_none()
        && account.close_authority.is_none()
        && account.is_native.is_none()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn config() -> StrategyConfig {
        StrategyConfig {
            squads_vault_index: 0,
            voltr_program: VOLTR_PROGRAM_ID,
            voltr_vault: Pubkey::new_unique(),
            strategy: Pubkey::new_unique(),
            vault_strategy_auth: Pubkey::new_unique(),
            squads_program: SQUADS_PROGRAM_ID,
            squads_settings: Pubkey::new_unique(),
            squads_settings_signer: Pubkey::new_unique(),
            squads_vault: Pubkey::new_unique(),
            asset_mint: Pubkey::new_unique(),
            asset_token_program: spl_token::id(),
            squads_asset_ata: Pubkey::new_unique(),
            max_report_nav_raw: 100,
            max_report_age_slots: 5,
            last_sequence: 7,
            last_observed_slot: 97,
            last_nav_raw: 0,
            last_snapshot_digest: [1; 32],
        }
    }

    fn report() -> ReportV1 {
        ReportV1 {
            sequence: 8,
            observed_slot: 100,
            nav_after_raw: 100,
            snapshot_digest: [2; 32],
        }
    }

    fn settings(expected_signer: Pubkey) -> Vec<u8> {
        let mut data = vec![0; 168];
        data[..8].copy_from_slice(&SQUADS_SETTINGS_DISCRIMINATOR);
        data[56..58].copy_from_slice(&1u16.to_le_bytes());
        data[58..62].copy_from_slice(&0u32.to_le_bytes());
        data[78] = 0;
        data[88..92].copy_from_slice(&1u32.to_le_bytes());
        data[92..124].copy_from_slice(expected_signer.as_ref());
        data[124] = 7;
        data
    }

    fn settings_with_archival_authority(expected_signer: Pubkey) -> Vec<u8> {
        let mut data = vec![0; 168];
        data[..8].copy_from_slice(&SQUADS_SETTINGS_DISCRIMINATOR);
        data[56..58].copy_from_slice(&1u16.to_le_bytes());
        data[78] = 1;
        data[79..111].copy_from_slice(Pubkey::new_unique().as_ref());
        data[120..124].copy_from_slice(&1u32.to_le_bytes());
        data[124..156].copy_from_slice(expected_signer.as_ref());
        data[156] = 7;
        data
    }

    #[test]
    fn settings_graph_is_exact_not_address_only() {
        let signer = Pubkey::new_unique();
        assert_eq!(
            valid_settings_authority_graph(&settings(signer), signer),
            Ok(true)
        );
        assert_eq!(
            valid_settings_authority_graph(&settings_with_archival_authority(signer), signer),
            Ok(true)
        );
        assert_eq!(
            valid_settings_authority_graph(&settings(signer), Pubkey::new_unique()),
            Ok(false)
        );
        assert_eq!(
            valid_settings_authority_graph(&settings(Pubkey::default()), Pubkey::default()),
            Ok(false)
        );
        let mut wrong_threshold = settings(signer);
        wrong_threshold[56..58].copy_from_slice(&2u16.to_le_bytes());
        assert_eq!(
            valid_settings_authority_graph(&wrong_threshold, signer),
            Ok(false)
        );
    }

    #[test]
    fn report_rules_reject_replay_staleness_future_and_cap() {
        let config = config();
        assert_eq!(validate_report_fields(&config, report(), 100), Ok(()));

        let mut replay = report();
        replay.sequence = 7;
        assert_eq!(
            validate_report_fields(&config, replay, 100),
            Err(AdaptorError::ReportSequence)
        );

        let mut stale = report();
        stale.observed_slot = 94;
        assert_eq!(
            validate_report_fields(&config, stale, 100),
            Err(AdaptorError::ReportSlot)
        );

        let mut future = report();
        future.observed_slot = 101;
        assert_eq!(
            validate_report_fields(&config, future, 100),
            Err(AdaptorError::ReportSlot)
        );

        let mut oversized = report();
        oversized.nav_after_raw = 101;
        assert_eq!(
            validate_report_fields(&config, oversized, 100),
            Err(AdaptorError::ReportCap)
        );
    }
}
