use solana_program::{
    account_info::{next_account_info, AccountInfo},
    entrypoint::ProgramResult,
    program::invoke_signed,
    program_error::ProgramError,
    pubkey::Pubkey,
    rent::Rent,
    sysvar::Sysvar,
};
use solana_system_interface::instruction as system_instruction;

use crate::{
    constants::HUB_CONFIG_SPACE,
    instruction::{parse_instruction, HubInstruction, SwapExactInArgs},
    state::{derive_config, derive_hub_authority, derive_inventory_account, HubConfig},
    token::{
        read_mint_decimals, require_matching_token_mint, require_token_account, transfer_checked,
        transfer_checked_signed,
    },
    validation::{
        require_admin, require_distinct_key, require_distinct_keys, require_distinct_pubkeys,
        require_fee_cap, require_key, require_signer, validate_fee_bps,
    },
};

pub fn process_instruction(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    match parse_instruction(data)? {
        HubInstruction::InitializeConfig(config) => {
            process_initialize_config(program_id, accounts, config)
        }
        HubInstruction::SwapExactIn(args) => process_swap_exact_in(program_id, accounts, args),
        HubInstruction::WithdrawInventory { amount } => {
            process_withdraw_inventory(program_id, accounts, amount)
        }
        HubInstruction::SetPaused { paused } => process_set_paused(program_id, accounts, paused),
        HubInstruction::SetMaxFee { max_fee_bps } => {
            process_set_max_fee(program_id, accounts, max_fee_bps)
        }
    }
}

fn process_initialize_config(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    config: HubConfig,
) -> ProgramResult {
    validate_fee_bps(config.max_fee_bps)?;

    let account_info_iter = &mut accounts.iter();
    let payer = next_account_info(account_info_iter)?;
    let config_account = next_account_info(account_info_iter)?;
    let system_program = next_account_info(account_info_iter)?;

    require_signer(payer)?;
    require_key(system_program, &solana_program::system_program::ID)?;
    require_key(config_account, &derive_config(program_id).0)?;
    if config_account.owner != &solana_program::system_program::ID
        || !config_account.data_is_empty()
    {
        return Err(ProgramError::AccountAlreadyInitialized);
    }

    let lamports = Rent::get()?.minimum_balance(HUB_CONFIG_SPACE);
    let (_, bump) = derive_config(program_id);
    invoke_signed(
        &system_instruction::create_account(
            payer.key,
            config_account.key,
            lamports,
            HUB_CONFIG_SPACE as u64,
            program_id,
        ),
        &[
            payer.clone(),
            config_account.clone(),
            system_program.clone(),
        ],
        &[&[crate::CONFIG_SEED, &[bump]]],
    )?;

    config.write_account(config_account)
}

fn process_set_max_fee(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    max_fee_bps: u16,
) -> ProgramResult {
    validate_fee_bps(max_fee_bps)?;

    let account_info_iter = &mut accounts.iter();
    let config_account = next_account_info(account_info_iter)?;
    let admin = next_account_info(account_info_iter)?;

    let mut config = HubConfig::read_account(program_id, config_account)?;
    require_admin(admin, &config)?;
    config.max_fee_bps = max_fee_bps;
    config.write_account(config_account)
}

fn process_set_paused(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    paused: bool,
) -> ProgramResult {
    let account_info_iter = &mut accounts.iter();
    let config_account = next_account_info(account_info_iter)?;
    let admin = next_account_info(account_info_iter)?;

    let mut config = HubConfig::read_account(program_id, config_account)?;
    require_admin(admin, &config)?;
    config.paused = paused;
    config.write_account(config_account)
}

fn process_swap_exact_in(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    args: SwapExactInArgs,
) -> ProgramResult {
    let SwapExactInArgs {
        amount_in,
        amount_out,
        min_out,
        max_fee_bps,
    } = args;
    if amount_in == 0 || amount_out == 0 {
        return Err(ProgramError::InvalidArgument);
    }
    if amount_out < min_out {
        return Err(ProgramError::InvalidArgument);
    }

    let account_info_iter = &mut accounts.iter();
    let config_account = next_account_info(account_info_iter)?;
    let user_vault = next_account_info(account_info_iter)?;
    let user_input = next_account_info(account_info_iter)?;
    let user_output = next_account_info(account_info_iter)?;
    let hub_input = next_account_info(account_info_iter)?;
    let hub_output = next_account_info(account_info_iter)?;
    let input_mint = next_account_info(account_info_iter)?;
    let output_mint = next_account_info(account_info_iter)?;
    let hub_authority = next_account_info(account_info_iter)?;
    let hub_authorizer = next_account_info(account_info_iter)?;
    let token_program = next_account_info(account_info_iter)?;

    let config = HubConfig::read_account(program_id, config_account)?;
    if config.paused || max_fee_bps > config.max_fee_bps {
        return Err(ProgramError::InvalidArgument);
    }
    require_signer(user_vault)?;
    require_signer(hub_authorizer)?;
    require_key(hub_authorizer, &config.hub_authorizer)?;
    require_key(token_program, &spl_token::id())?;
    require_key(hub_authority, &derive_hub_authority(program_id).0)?;
    config.require_allowed_mint(input_mint.key)?;
    config.require_allowed_mint(output_mint.key)?;
    require_distinct_pubkeys(input_mint.key, output_mint.key)?;
    require_distinct_keys(&[user_input, user_output, hub_input, hub_output])?;
    require_key(
        hub_input,
        &derive_inventory_account(program_id, input_mint.key),
    )?;
    require_key(
        hub_output,
        &derive_inventory_account(program_id, output_mint.key),
    )?;

    let input_decimals = read_mint_decimals(input_mint)?;
    let output_decimals = read_mint_decimals(output_mint)?;
    require_fee_cap(
        amount_in,
        amount_out,
        input_decimals,
        output_decimals,
        max_fee_bps,
    )?;
    require_token_account(user_input, input_mint.key, user_vault.key)?;
    require_token_account(user_output, output_mint.key, user_vault.key)?;
    require_token_account(hub_input, input_mint.key, hub_authority.key)?;
    require_token_account(hub_output, output_mint.key, hub_authority.key)?;

    transfer_checked(
        user_input,
        input_mint,
        hub_input,
        user_vault,
        token_program,
        amount_in,
        input_decimals,
    )?;
    transfer_checked_signed(
        program_id,
        hub_output,
        output_mint,
        user_output,
        hub_authority,
        token_program,
        amount_out,
        output_decimals,
    )
}

fn process_withdraw_inventory(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    amount: u64,
) -> ProgramResult {
    if amount == 0 {
        return Err(ProgramError::InvalidArgument);
    }

    let account_info_iter = &mut accounts.iter();
    let config_account = next_account_info(account_info_iter)?;
    let admin = next_account_info(account_info_iter)?;
    let hub_source = next_account_info(account_info_iter)?;
    let destination = next_account_info(account_info_iter)?;
    let mint = next_account_info(account_info_iter)?;
    let hub_authority = next_account_info(account_info_iter)?;
    let token_program = next_account_info(account_info_iter)?;

    let config = HubConfig::read_account(program_id, config_account)?;
    require_admin(admin, &config)?;
    require_key(token_program, &spl_token::id())?;
    require_key(hub_authority, &derive_hub_authority(program_id).0)?;
    require_distinct_key(hub_source, destination)?;
    require_key(hub_source, &derive_inventory_account(program_id, mint.key))?;
    config.require_allowed_mint(mint.key)?;
    require_token_account(hub_source, mint.key, hub_authority.key)?;
    require_matching_token_mint(destination, mint.key)?;

    let decimals = read_mint_decimals(mint)?;
    transfer_checked_signed(
        program_id,
        hub_source,
        mint,
        destination,
        hub_authority,
        token_program,
        amount,
        decimals,
    )
}
