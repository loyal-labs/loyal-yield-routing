use crate::{
    AdaptorError, AdaptorResult, ReportTicket, ReportV1, StrategyConfig, CONFIG_LEN,
    REPORT_TICKET_LEN, REPORT_V1_LEN,
};
use solana_program::{
    account_info::{next_account_info, AccountInfo},
    clock::Clock,
    entrypoint::ProgramResult,
    hash::hashv,
    program::{invoke, invoke_signed, set_return_data},
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
pub const INITIALIZE_REPORT_TICKET_DISCRIMINATOR: [u8; 8] = [124, 41, 223, 13, 165, 246, 70, 62];
pub const ARM_REPORT_DISCRIMINATOR: [u8; 8] = [164, 175, 246, 41, 178, 140, 35, 3];
pub const INITIALIZE_DISCRIMINATOR: [u8; 8] = [175, 175, 109, 31, 13, 152, 155, 237];
pub const DEPOSIT_DISCRIMINATOR: [u8; 8] = [242, 35, 198, 137, 82, 225, 242, 182];
pub const WITHDRAW_DISCRIMINATOR: [u8; 8] = [183, 18, 70, 156, 148, 109, 161, 34];
const SQUADS_PREFIX: &[u8] = b"smart_account";
pub const REPORT_TICKET_SEED: &[u8] = b"report_ticket";
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
        INITIALIZE_REPORT_TICKET_DISCRIMINATOR if data.len() == 8 => {
            process_initialize_report_ticket(program_id, accounts)
        }
        ARM_REPORT_DISCRIMINATOR => process_arm_report(program_id, accounts, &data[8..]),
        // Voltr forwards the optional `additional_args` Borsh tag after the
        // adaptor discriminator. Initialize has no arguments, so the only
        // accepted wire is the discriminator followed by `Option::None`.
        INITIALIZE_DISCRIMINATOR if valid_initialize_wire(data) => initialize(program_id, accounts),
        DEPOSIT_DISCRIMINATOR => capital_path(program_id, accounts, &data[8..], true),
        WITHDRAW_DISCRIMINATOR => capital_path(program_id, accounts, &data[8..], false),
        _ => Err(AdaptorError::InvalidInstruction),
    }
    .map_err(ProgramError::from)
}

fn process_initialize_report_ticket(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
) -> AdaptorResult<()> {
    if accounts.len() != 4 {
        return Err(AdaptorError::InvalidAccountCount);
    }
    let payer = &accounts[0];
    let config_account = &accounts[1];
    let ticket_account = &accounts[2];
    let system_program = &accounts[3];
    if !payer.is_signer
        || !payer.is_writable
        || config_account.is_signer
        || config_account.is_writable
        || ticket_account.is_signer
        || !ticket_account.is_writable
        || ticket_account.owner != &solana_program::system_program::ID
        || !ticket_account.data_is_empty()
        || system_program.is_signer
        || system_program.is_writable
        || system_program.key != &solana_program::system_program::ID
    {
        return Err(AdaptorError::InvalidAccount);
    }
    load_config(program_id, config_account)?;
    let (expected_ticket, bump) = Pubkey::find_program_address(
        &[REPORT_TICKET_SEED, config_account.key.as_ref()],
        program_id,
    );
    if ticket_account.key != &expected_ticket {
        return Err(AdaptorError::InvalidTicket);
    }
    let rent = Rent::get().map_err(|_| AdaptorError::InvalidAccount)?;
    invoke_signed(
        &system_instruction::create_account(
            payer.key,
            ticket_account.key,
            rent.minimum_balance(REPORT_TICKET_LEN),
            REPORT_TICKET_LEN as u64,
            program_id,
        ),
        &[
            payer.clone(),
            ticket_account.clone(),
            system_program.clone(),
        ],
        &[&[REPORT_TICKET_SEED, config_account.key.as_ref(), &[bump]]],
    )
    .map_err(|_| AdaptorError::InvalidAccount)?;
    ReportTicket {
        bump,
        armed: false,
        config: *config_account.key,
        last_consumed_sequence: 0,
        active_sequence: 0,
        active_wire_sha256: [0; 32],
    }
    .encode(
        &mut ticket_account
            .try_borrow_mut_data()
            .map_err(|_| AdaptorError::InvalidTicket)?,
    )
}

fn process_arm_report(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> AdaptorResult<()> {
    const CAPITAL_TAIL_LEN: usize = 8 + 1 + 4 + REPORT_V1_LEN;
    if accounts.len() != 5 || data.len() != 1 + CAPITAL_TAIL_LEN {
        return Err(AdaptorError::InvalidInstruction);
    }
    if accounts[0].is_signer || accounts[0].is_writable {
        return Err(AdaptorError::InvalidConfig);
    }
    if !accounts[1].is_writable {
        return Err(AdaptorError::InvalidTicketWritable);
    }
    if accounts[1].is_signer
        || accounts[2].is_signer
        || accounts[2].is_writable
        || !accounts[3].is_signer
        || accounts[3].is_writable
        || accounts[4].is_signer
        || accounts[4].is_writable
    {
        return Err(AdaptorError::InvalidSquadsVault);
    }
    let config = load_config(program_id, &accounts[0])?;
    validate_squads_binding(&config, &accounts[2], &accounts[3], &accounts[4])?;
    let discriminator = match data[0] {
        0 => &DEPOSIT_DISCRIMINATOR,
        1 => &WITHDRAW_DISCRIMINATOR,
        _ => return Err(AdaptorError::InvalidInstruction),
    };
    let (amount, report) = parse_capital_wire(&data[1..])?;
    let clock = Clock::get().map_err(|_| AdaptorError::InvalidReport)?;
    validate_capital_fields(&config, amount, report, clock.slot)?;
    let mut ticket = load_ticket(program_id, &accounts[0], &accounts[1])?;
    validate_ticket_can_arm(&ticket, report, clock.slot, config.max_report_age_slots)?;
    ticket.armed = true;
    ticket.active_sequence = report.sequence;
    ticket.active_wire_sha256 = capital_wire_hash(discriminator, &data[1..]);
    ticket.encode(
        &mut accounts[1]
            .try_borrow_mut_data()
            .map_err(|_| AdaptorError::InvalidTicket)?,
    )
}

fn valid_initialize_wire(data: &[u8]) -> bool {
    data.len() == 9 && data[..8] == INITIALIZE_DISCRIMINATOR && data[8] == 0
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
    validate_capital_account_privileges(accounts)?;
    reject_duplicate_mutables(accounts)?;
    let (amount, report) = parse_capital_wire(data)?;
    let config = load_config(program_id, &accounts[1])?;
    validate_capital_authority(accounts[0].is_signer)?;
    validate_bindings(
        &config,
        &accounts[0],
        &accounts[5],
        &accounts[6],
        &accounts[2],
        &accounts[4],
        &accounts[7],
    )?;
    let clock = Clock::get().map_err(|_| AdaptorError::InvalidReport)?;
    validate_capital_fields(&config, amount, report, clock.slot)?;
    let discriminator = if is_deposit {
        &DEPOSIT_DISCRIMINATOR
    } else {
        &WITHDRAW_DISCRIMINATOR
    };
    let mut ticket = load_ticket(program_id, &accounts[1], &accounts[8])?;
    validate_ticket_for_capital(&ticket, report, capital_wire_hash(discriminator, data))?;
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
    consume_ticket(&mut ticket);
    ticket.encode(
        &mut accounts[8]
            .try_borrow_mut_data()
            .map_err(|_| AdaptorError::InvalidTicket)?,
    )?;
    set_return_data(&report.nav_after_raw.to_le_bytes());
    Ok(())
}

fn validate_capital_account_privileges(accounts: &[AccountInfo]) -> AdaptorResult<()> {
    if accounts.len() != 9 {
        return Err(AdaptorError::InvalidInstruction);
    }
    if accounts[1].is_signer || accounts[1].is_writable {
        return Err(AdaptorError::InvalidConfig);
    }
    if accounts[2].is_signer
        || !accounts[3].is_writable
        || accounts[3].is_signer
        || accounts[4].is_signer
        || accounts[4].is_writable
        || accounts[5].is_signer
        || accounts[5].is_writable
        || accounts[6].is_signer
        || accounts[6].is_writable
        || !accounts[7].is_writable
        || accounts[7].is_signer
        || accounts[8].is_signer
    {
        return Err(AdaptorError::InvalidAccount);
    }
    if !accounts[8].is_writable {
        return Err(AdaptorError::InvalidTicketWritable);
    }
    Ok(())
}

/// Voltr must forward its exact strategy authority as a signer. Squads
/// authorization is carried by the separately armed one-use ticket because
/// Voltr intentionally strips the Squads vault signer privilege from CPI.
fn validate_capital_authority(strategy_authority_signer: bool) -> AdaptorResult<()> {
    if !strategy_authority_signer {
        return Err(AdaptorError::InvalidAuthority);
    }
    Ok(())
}

fn capital_wire_hash(discriminator: &[u8; 8], capital_tail: &[u8]) -> [u8; 32] {
    hashv(&[discriminator, capital_tail]).to_bytes()
}

fn load_ticket(
    program_id: &Pubkey,
    config_account: &AccountInfo,
    ticket_account: &AccountInfo,
) -> AdaptorResult<ReportTicket> {
    if ticket_account.owner != program_id {
        return Err(AdaptorError::InvalidTicket);
    }
    let ticket = ReportTicket::decode(
        &ticket_account
            .try_borrow_data()
            .map_err(|_| AdaptorError::InvalidTicket)?,
    )?;
    let (expected_ticket, expected_bump) = Pubkey::find_program_address(
        &[REPORT_TICKET_SEED, config_account.key.as_ref()],
        program_id,
    );
    if ticket_account.key != &expected_ticket
        || ticket.config != *config_account.key
        || ticket.bump != expected_bump
        || (!ticket.armed && (ticket.active_sequence != 0 || ticket.active_wire_sha256 != [0; 32]))
        || (ticket.armed
            && (ticket.active_sequence == 0
                || ticket.active_sequence <= ticket.last_consumed_sequence
                || ticket.active_wire_sha256 == [0; 32]))
    {
        return Err(AdaptorError::InvalidTicket);
    }
    Ok(ticket)
}

fn validate_ticket_for_capital(
    ticket: &ReportTicket,
    report: ReportV1,
    wire_sha256: [u8; 32],
) -> AdaptorResult<()> {
    if !ticket.armed {
        return Err(AdaptorError::TicketNotArmed);
    }
    if ticket.active_sequence != report.sequence || ticket.active_wire_sha256 != wire_sha256 {
        return Err(AdaptorError::TicketMismatch);
    }
    if report.sequence <= ticket.last_consumed_sequence {
        return Err(AdaptorError::TicketReplay);
    }
    Ok(())
}

fn validate_ticket_can_arm(
    ticket: &ReportTicket,
    report: ReportV1,
    current_slot: u64,
    max_report_age_slots: u64,
) -> AdaptorResult<()> {
    if report.sequence <= ticket.last_consumed_sequence {
        return Err(AdaptorError::TicketReplay);
    }
    if ticket.armed {
        let active_expired = current_slot
            .checked_sub(ticket.active_sequence)
            .is_some_and(|age| age > max_report_age_slots);
        if !active_expired {
            return Err(AdaptorError::TicketAlreadyArmed);
        }
        if report.sequence <= ticket.active_sequence {
            return Err(AdaptorError::TicketReplay);
        }
    }
    Ok(())
}

fn consume_ticket(ticket: &mut ReportTicket) {
    ticket.last_consumed_sequence = ticket.active_sequence;
    ticket.armed = false;
    ticket.active_sequence = 0;
    ticket.active_wire_sha256 = [0; 32];
}

/// Decode the exact bytes Voltr forwards after selecting the adaptor
/// discriminator from its `Option<Vec<u8>>` instruction discriminator:
///
/// `amount: u64 || additional_args: Option<Vec<u8>>`.
///
/// The selected discriminator itself is forwarded unwrapped. The second
/// option remains Borsh encoded, so a report-bearing capital/NAV call must be
/// `Some`, must declare exactly 57 bytes, and must have no trailing bytes.
fn parse_capital_wire(data: &[u8]) -> AdaptorResult<(u64, ReportV1)> {
    const OPTION_TAG_OFFSET: usize = 8;
    const LENGTH_OFFSET: usize = OPTION_TAG_OFFSET + 1;
    const REPORT_OFFSET: usize = LENGTH_OFFSET + 4;
    if data.len() != REPORT_OFFSET + REPORT_V1_LEN
        || data[OPTION_TAG_OFFSET] != 1
        || u32::from_le_bytes(
            data[LENGTH_OFFSET..REPORT_OFFSET]
                .try_into()
                .map_err(|_| AdaptorError::InvalidInstruction)?,
        ) != REPORT_V1_LEN as u32
    {
        return Err(AdaptorError::InvalidInstruction);
    }
    let amount = u64::from_le_bytes(
        data[..8]
            .try_into()
            .map_err(|_| AdaptorError::InvalidInstruction)?,
    );
    Ok((amount, ReportV1::decode(&data[REPORT_OFFSET..])?))
}

fn validate_report_fields(
    config: &StrategyConfig,
    report: ReportV1,
    current_slot: u64,
) -> AdaptorResult<()> {
    // The report sequence is not caller-arbitrary: it must be the same nonzero
    // confirmed observation slot that the Squads-signed ArmReport authorizes.
    if report.sequence == 0 || report.sequence != report.observed_slot {
        return Err(AdaptorError::ReportSequence);
    }
    if report.observed_slot > current_slot
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

fn validate_capital_fields(
    config: &StrategyConfig,
    amount: u64,
    report: ReportV1,
    current_slot: u64,
) -> AdaptorResult<()> {
    if amount > config.max_report_nav_raw {
        return Err(AdaptorError::ReportCap);
    }
    validate_report_fields(config, report, current_slot)
}

fn validate_squads_binding(
    config: &StrategyConfig,
    settings: &AccountInfo,
    squads_vault: &AccountInfo,
    squads_program: &AccountInfo,
) -> AdaptorResult<()> {
    let expected_vault = Pubkey::find_program_address(
        &[
            SQUADS_PREFIX,
            config.squads_settings.as_ref(),
            SQUADS_PREFIX,
            &[config.squads_vault_index],
        ],
        &config.squads_program,
    )
    .0;
    if squads_program.key != &config.squads_program
        || !squads_program.executable
        || settings.key != &config.squads_settings
        || settings.owner != &config.squads_program
        || squads_vault.key != &config.squads_vault
        || config.squads_vault != expected_vault
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
    // v2 keeps the deployed layout stable, but these historical mutable fields
    // are now reserved and must remain exactly zero forever.
    if config.strategy != *account.key || !reserved_report_state_is_zero(&config) {
        return Err(AdaptorError::InvalidConfig);
    }
    Ok(config)
}
fn reserved_report_state_is_zero(config: &StrategyConfig) -> bool {
    config.last_sequence == 0
        && config.last_observed_slot == 0
        && config.last_nav_raw == 0
        && config.last_snapshot_digest == [0; 32]
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
            last_sequence: 0,
            last_observed_slot: 0,
            last_nav_raw: 0,
            last_snapshot_digest: [0; 32],
        }
    }

    fn report() -> ReportV1 {
        ReportV1 {
            sequence: 100,
            observed_slot: 100,
            nav_after_raw: 100,
            snapshot_digest: [2; 32],
        }
    }

    fn test_account(is_signer: bool, is_writable: bool) -> AccountInfo<'static> {
        AccountInfo::new(
            Box::leak(Box::new(Pubkey::new_unique())),
            is_signer,
            is_writable,
            Box::leak(Box::new(0)),
            Box::leak(Vec::new().into_boxed_slice()),
            Box::leak(Box::new(Pubkey::new_unique())),
            false,
            0,
        )
    }

    fn canonical_capital_accounts() -> Vec<AccountInfo<'static>> {
        vec![
            test_account(true, true),   // Voltr strategy authority
            test_account(false, false), // adaptor config
            test_account(false, true),  // Voltr requires vault asset mint writable
            test_account(false, true),  // strategy asset ATA
            test_account(false, false), // asset token program
            test_account(false, false), // Squads Settings
            test_account(false, false), // Squads vault
            test_account(false, true),  // Squads asset ATA
            test_account(false, true),  // report ticket
        ]
    }

    #[test]
    fn capital_privileges_accept_voltr_writable_mint_without_widening_other_roles() {
        assert_eq!(
            validate_capital_account_privileges(&canonical_capital_accounts()),
            Ok(())
        );

        let mut mint_signer = canonical_capital_accounts();
        mint_signer[2].is_signer = true;
        assert_eq!(
            validate_capital_account_privileges(&mint_signer),
            Err(AdaptorError::InvalidAccount)
        );

        let mut writable_config = canonical_capital_accounts();
        writable_config[1].is_writable = true;
        assert_eq!(
            validate_capital_account_privileges(&writable_config),
            Err(AdaptorError::InvalidConfig)
        );

        let mut writable_token_program = canonical_capital_accounts();
        writable_token_program[4].is_writable = true;
        assert_eq!(
            validate_capital_account_privileges(&writable_token_program),
            Err(AdaptorError::InvalidAccount)
        );

        let mut readonly_ticket = canonical_capital_accounts();
        readonly_ticket[8].is_writable = false;
        assert_eq!(
            validate_capital_account_privileges(&readonly_ticket),
            Err(AdaptorError::InvalidTicketWritable)
        );
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
    fn report_rules_bind_sequence_to_slot_and_reject_staleness_future_and_cap() {
        let config = config();
        assert_eq!(validate_report_fields(&config, report(), 100), Ok(()));

        let mut arbitrary_sequence = report();
        arbitrary_sequence.sequence = 99;
        assert_eq!(
            validate_report_fields(&config, arbitrary_sequence, 100),
            Err(AdaptorError::ReportSequence)
        );

        let mut zero_slot = report();
        zero_slot.sequence = 0;
        zero_slot.observed_slot = 0;
        assert_eq!(
            validate_report_fields(&config, zero_slot, 100),
            Err(AdaptorError::ReportSequence)
        );

        let mut stale = report();
        stale.sequence = 94;
        stale.observed_slot = 94;
        assert_eq!(
            validate_report_fields(&config, stale, 100),
            Err(AdaptorError::ReportSlot)
        );

        let mut future = report();
        future.sequence = 101;
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

        assert_eq!(
            validate_capital_fields(&config, 101, report(), 100),
            Err(AdaptorError::ReportCap)
        );
    }

    #[test]
    fn initialize_accepts_only_voltr_forwarded_none_tag() {
        let mut wire = INITIALIZE_DISCRIMINATOR.to_vec();
        assert!(!valid_initialize_wire(&wire));
        wire.push(0);
        assert!(valid_initialize_wire(&wire));
        wire[8] = 1;
        assert!(!valid_initialize_wire(&wire));
        wire.push(0);
        assert!(!valid_initialize_wire(&wire));
    }

    fn report_bytes(report: ReportV1) -> Vec<u8> {
        let mut bytes = vec![1];
        bytes.extend_from_slice(&report.sequence.to_le_bytes());
        bytes.extend_from_slice(&report.observed_slot.to_le_bytes());
        bytes.extend_from_slice(&report.nav_after_raw.to_le_bytes());
        bytes.extend_from_slice(&report.snapshot_digest);
        bytes
    }

    #[test]
    fn capital_parser_accepts_only_the_voltr_forwarded_some_envelope() {
        let expected = report();
        let mut wire = 1_000_000u64.to_le_bytes().to_vec();
        wire.push(1);
        wire.extend_from_slice(&(REPORT_V1_LEN as u32).to_le_bytes());
        wire.extend_from_slice(&report_bytes(expected));
        assert_eq!(wire.len(), 70);
        assert_eq!(parse_capital_wire(&wire), Ok((1_000_000, expected)));

        let mut none = wire.clone();
        none[8] = 0;
        assert_eq!(
            parse_capital_wire(&none),
            Err(AdaptorError::InvalidInstruction)
        );

        let mut wrong_length = wire.clone();
        wrong_length[9..13].copy_from_slice(&56u32.to_le_bytes());
        assert_eq!(
            parse_capital_wire(&wrong_length),
            Err(AdaptorError::InvalidInstruction)
        );

        let mut unwrapped = 1_000_000u64.to_le_bytes().to_vec();
        unwrapped.extend_from_slice(&report_bytes(expected));
        assert_eq!(
            parse_capital_wire(&unwrapped),
            Err(AdaptorError::InvalidInstruction)
        );

        let mut trailing = wire;
        trailing.push(0);
        assert_eq!(
            parse_capital_wire(&trailing),
            Err(AdaptorError::InvalidInstruction)
        );
    }

    #[test]
    fn capital_authority_errors_identify_the_missing_privilege() {
        assert_eq!(validate_capital_authority(true), Ok(()));
        assert_eq!(
            validate_capital_authority(false),
            Err(AdaptorError::InvalidAuthority)
        );
    }

    #[test]
    fn ticket_is_one_use_and_binds_the_exact_capital_wire() {
        let report = report();
        let mut tail = 1_000_000u64.to_le_bytes().to_vec();
        tail.push(1);
        tail.extend_from_slice(&(REPORT_V1_LEN as u32).to_le_bytes());
        tail.extend_from_slice(&report_bytes(report));
        let deposit_hash = capital_wire_hash(&DEPOSIT_DISCRIMINATOR, &tail);
        assert_ne!(
            deposit_hash,
            capital_wire_hash(&WITHDRAW_DISCRIMINATOR, &tail)
        );

        let mut ticket = ReportTicket {
            bump: 254,
            armed: true,
            config: Pubkey::new_unique(),
            last_consumed_sequence: 99,
            active_sequence: report.sequence,
            active_wire_sha256: deposit_hash,
        };
        assert_eq!(
            validate_ticket_for_capital(&ticket, report, deposit_hash),
            Ok(())
        );
        let mut different_tail = tail.clone();
        different_tail[0] ^= 1;
        assert_eq!(
            validate_ticket_for_capital(
                &ticket,
                report,
                capital_wire_hash(&DEPOSIT_DISCRIMINATOR, &different_tail),
            ),
            Err(AdaptorError::TicketMismatch)
        );

        consume_ticket(&mut ticket);
        assert!(!ticket.armed);
        assert_eq!(ticket.last_consumed_sequence, report.sequence);
        assert_eq!(ticket.active_sequence, 0);
        assert_eq!(ticket.active_wire_sha256, [0; 32]);
        assert_eq!(
            validate_ticket_for_capital(&ticket, report, deposit_hash),
            Err(AdaptorError::TicketNotArmed)
        );
    }

    #[test]
    fn ticket_rejects_a_non_monotonic_armed_sequence() {
        let report = report();
        let ticket = ReportTicket {
            bump: 254,
            armed: true,
            config: Pubkey::new_unique(),
            last_consumed_sequence: report.sequence,
            active_sequence: report.sequence,
            active_wire_sha256: [9; 32],
        };
        assert_eq!(
            validate_ticket_for_capital(&ticket, report, [9; 32]),
            Err(AdaptorError::TicketReplay)
        );
    }

    #[test]
    fn fresh_active_arm_cannot_be_overwritten() {
        let next = ReportV1 {
            sequence: 200,
            observed_slot: 200,
            ..report()
        };
        let ticket = ReportTicket {
            bump: 254,
            armed: true,
            config: Pubkey::new_unique(),
            last_consumed_sequence: 100,
            active_sequence: 190,
            active_wire_sha256: [9; 32],
        };
        assert_eq!(
            validate_ticket_can_arm(&ticket, next, 194, 5),
            Err(AdaptorError::TicketAlreadyArmed)
        );
    }

    #[test]
    fn expired_active_arm_can_be_replaced_by_a_newer_valid_report() {
        let next = ReportV1 {
            sequence: 200,
            observed_slot: 200,
            ..report()
        };
        let ticket = ReportTicket {
            bump: 254,
            armed: true,
            config: Pubkey::new_unique(),
            last_consumed_sequence: 100,
            active_sequence: 190,
            active_wire_sha256: [9; 32],
        };
        assert_eq!(validate_ticket_can_arm(&ticket, next, 200, 5), Ok(()));
        assert_eq!(
            validate_ticket_can_arm(
                &ticket,
                ReportV1 {
                    sequence: 190,
                    observed_slot: 190,
                    ..next
                },
                200,
                5
            ),
            Err(AdaptorError::TicketReplay)
        );
    }

    #[test]
    fn historical_report_state_is_zero_reserved() {
        let mut config = config();
        assert!(reserved_report_state_is_zero(&config));
        config.last_sequence = 1;
        assert!(!reserved_report_state_is_zero(&config));
    }
}
