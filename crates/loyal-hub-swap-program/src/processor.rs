use pinocchio::{
    account_info::AccountInfo,
    instruction::Seed,
    program_error::ProgramError,
    pubkey::Pubkey,
    sysvars::rent::Rent,
    sysvars::Sysvar,
    ProgramResult,
};
#[cfg(not(kani))]
use pinocchio::{
    instruction::{AccountMeta, Instruction, Signer},
    program::invoke_signed,
};
#[cfg(kani)]
use pinocchio::instruction::Signer;

use crate::{
    constants::{HUB_CONFIG_SPACE, SYSTEM_PROGRAM_ID},
    instruction::{
        parse_instruction, AllowedMint, HubInstruction, InventoryAccount, RebalanceInventoryArgs,
        SwapExactInArgs,
    },
    state::{derive_config, derive_hub_authority, derive_inventory_account, HubConfig},
    token::{
        read_mint_decimals, require_matching_token_mint, require_token_account, transfer_checked,
        transfer_checked_signed,
    },
    validation::{
        require_admin, require_distinct_key, require_distinct_keys, require_distinct_pubkeys,
        require_fee_cap, require_inventory_rebalancer, require_key, require_readonly,
        require_signer, validate_fee_bps,
    },
    SPL_TOKEN_ID,
};
#[cfg(not(kani))]
use crate::codec::write_bytes_at;

fn next_account_info<'a, I>(iter: &mut I) -> Result<&'a AccountInfo, ProgramError>
where
    I: Iterator<Item = &'a AccountInfo>,
{
    iter.next().ok_or(ProgramError::NotEnoughAccountKeys)
}

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
        HubInstruction::WithdrawInventory(args) => {
            process_withdraw_inventory(program_id, accounts, args)
        }
        HubInstruction::SetPaused { paused } => process_set_paused(program_id, accounts, paused),
        HubInstruction::SetMaxFee { max_fee_bps } => {
            process_set_max_fee(program_id, accounts, max_fee_bps)
        }
        HubInstruction::RebalanceInventory(args) => {
            process_rebalance_inventory(program_id, accounts, args)
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
    require_key(system_program, &SYSTEM_PROGRAM_ID)?;
    require_key(config_account, &derive_config(program_id).0)?;
    if config_account.owner() != &SYSTEM_PROGRAM_ID || !config_account.data_is_empty() {
        return Err(ProgramError::AccountAlreadyInitialized);
    }

    let lamports = Rent::get()?.minimum_balance(HUB_CONFIG_SPACE);
    let (_, bump) = derive_config(program_id);
    let bump_seed = [bump];
    let seeds = [Seed::from(crate::CONFIG_SEED), Seed::from(&bump_seed)];
    let signer = Signer::from(&seeds);
    invoke_create_account(
        payer,
        config_account,
        system_program,
        lamports,
        HUB_CONFIG_SPACE as u64,
        program_id,
        &[signer],
    )?;

    config.write_account(config_account)
}

fn invoke_create_account(
    payer: &AccountInfo,
    new_account: &AccountInfo,
    system_program: &AccountInfo,
    lamports: u64,
    space: u64,
    owner: &Pubkey,
    signers: &[Signer],
) -> ProgramResult {
    #[cfg(kani)]
    {
        let payer_lamports = payer.lamports();
        if payer_lamports < lamports {
            return Err(ProgramError::InsufficientFunds);
        }
        // SAFETY: this branch is a Kani-only model of system create-account.
        // The harness constructs distinct writable account objects, and no
        // lamport references are live while this model mutates their headers.
        unsafe {
            *payer.borrow_mut_lamports_unchecked() = payer_lamports - lamports;
            *new_account.borrow_mut_lamports_unchecked() =
                new_account.lamports().saturating_add(lamports);
            new_account.assign(owner);
        }
        new_account.resize(space as usize)?;
        let _ = system_program;
        let _ = signers;
        return Ok(());
    }

    #[cfg(not(kani))]
    {
        let account_metas = [
            AccountMeta::writable_signer(payer.key()),
            AccountMeta::writable_signer(new_account.key()),
        ];
        let mut data = [0u8; 52];
        write_bytes_at(&mut data, 4, &lamports.to_le_bytes())?;
        write_bytes_at(&mut data, 12, &space.to_le_bytes())?;
        write_bytes_at(&mut data, 20, owner.as_ref())?;
        let instruction = Instruction {
            program_id: system_program.key(),
            accounts: &account_metas,
            data: &data,
        };
        invoke_signed(&instruction, &[payer, new_account], signers)
    }
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
        lane_id,
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
    require_readonly(config_account)?;
    if config.paused || max_fee_bps > config.max_fee_bps {
        return Err(ProgramError::InvalidArgument);
    }
    config.require_lane(lane_id.0)?;
    require_signer(user_vault)?;
    require_signer(hub_authorizer)?;
    require_key(hub_authorizer, &config.hub_authorizer)?;
    require_key(token_program, &SPL_TOKEN_ID)?;
    require_key(
        hub_authority,
        &derive_hub_authority(program_id, lane_id.0).0,
    )?;
    let input_inventory = InventoryAccount {
        lane_id,
        mint: AllowedMint(*input_mint.key()),
    };
    let output_inventory = InventoryAccount {
        lane_id,
        mint: AllowedMint(*output_mint.key()),
    };
    config.require_allowed_mint(&input_inventory.mint.0)?;
    config.require_allowed_mint(&output_inventory.mint.0)?;
    require_distinct_pubkeys(input_mint.key(), output_mint.key())?;
    require_distinct_keys(&[user_input, user_output, hub_input, hub_output])?;
    require_key(
        hub_input,
        &derive_inventory_account(
            program_id,
            &input_inventory.mint.0,
            input_inventory.lane_id.0,
        ),
    )?;
    require_key(
        hub_output,
        &derive_inventory_account(
            program_id,
            &output_inventory.mint.0,
            output_inventory.lane_id.0,
        ),
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
    require_token_account(user_input, input_mint.key(), user_vault.key())?;
    require_token_account(user_output, output_mint.key(), user_vault.key())?;
    require_token_account(hub_input, input_mint.key(), hub_authority.key())?;
    require_token_account(hub_output, output_mint.key(), hub_authority.key())?;

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
        lane_id.0,
    )
}

fn process_withdraw_inventory(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    args: crate::instruction::WithdrawInventoryArgs,
) -> ProgramResult {
    let crate::instruction::WithdrawInventoryArgs { amount, lane_id } = args;
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
    config.require_lane(lane_id.0)?;
    require_key(token_program, &SPL_TOKEN_ID)?;
    require_key(
        hub_authority,
        &derive_hub_authority(program_id, lane_id.0).0,
    )?;
    require_distinct_key(hub_source, destination)?;
    let inventory_account = InventoryAccount {
        lane_id,
        mint: AllowedMint(*mint.key()),
    };
    require_key(
        hub_source,
        &derive_inventory_account(
            program_id,
            &inventory_account.mint.0,
            inventory_account.lane_id.0,
        ),
    )?;
    config.require_allowed_mint(&inventory_account.mint.0)?;
    require_token_account(hub_source, mint.key(), hub_authority.key())?;
    require_matching_token_mint(destination, mint.key())?;

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
        lane_id.0,
    )
}

fn process_rebalance_inventory(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    args: RebalanceInventoryArgs,
) -> ProgramResult {
    let account_info_iter = &mut accounts.iter();
    let config_account = next_account_info(account_info_iter)?;
    let inventory_rebalancer = next_account_info(account_info_iter)?;
    let token_program = next_account_info(account_info_iter)?;
    let mint = next_account_info(account_info_iter)?;

    let config = HubConfig::read_account(program_id, config_account)?;
    require_inventory_rebalancer(inventory_rebalancer, &config)?;
    require_key(token_program, &SPL_TOKEN_ID)?;
    let rebalance_mint = AllowedMint(*mint.key());
    config.require_allowed_mint(&rebalance_mint.0)?;
    let decimals = read_mint_decimals(mint)?;

    for transfer in args.transfers {
        if transfer.from_lane_id.0 == transfer.to_lane_id.0 {
            return Err(ProgramError::InvalidArgument);
        }
        config.require_lane(transfer.from_lane_id.0)?;
        config.require_lane(transfer.to_lane_id.0)?;

        let source_authority = next_account_info(account_info_iter)?;
        let source_inventory = next_account_info(account_info_iter)?;
        let destination_inventory = next_account_info(account_info_iter)?;

        let source_authority_key = derive_hub_authority(program_id, transfer.from_lane_id.0).0;
        let destination_authority_key = derive_hub_authority(program_id, transfer.to_lane_id.0).0;
        require_key(source_authority, &source_authority_key)?;
        require_key(
            source_inventory,
            &derive_inventory_account(program_id, &rebalance_mint.0, transfer.from_lane_id.0),
        )?;
        require_key(
            destination_inventory,
            &derive_inventory_account(program_id, &rebalance_mint.0, transfer.to_lane_id.0),
        )?;
        require_distinct_key(source_inventory, destination_inventory)?;
        require_token_account(source_inventory, mint.key(), &source_authority_key)?;
        require_token_account(
            destination_inventory,
            mint.key(),
            &destination_authority_key,
        )?;

        transfer_checked_signed(
            program_id,
            source_inventory,
            mint,
            destination_inventory,
            source_authority,
            token_program,
            transfer.amount,
            decimals,
            transfer.from_lane_id.0,
        )?;
    }

    Ok(())
}
