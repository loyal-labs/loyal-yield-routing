#![allow(unexpected_cfgs)]

use solana_program::{
    account_info::{next_account_info, AccountInfo},
    entrypoint,
    entrypoint::ProgramResult,
    instruction::{AccountMeta, Instruction},
    program::{invoke, invoke_signed},
    program_error::ProgramError,
    pubkey,
    pubkey::Pubkey,
};
use spl_token::solana_program::program_pack::Pack;

pub const JUPITER_V6_PROGRAM_ID: Pubkey = pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4");
pub const JUPITER_BATCH_PROGRAM_ID: Pubkey = Pubkey::new_from_array([66; 32]);
pub const USDC_MINT: Pubkey = pubkey!("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v");
pub const PYUSD_MINT: Pubkey = pubkey!("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo");
pub const WRAPPED_SOL_MINT: Pubkey = pubkey!("So11111111111111111111111111111111111111112");
pub const KAMINO_LEND_PROGRAM_ID: Pubkey = pubkey!("KvauGMspG5k6rtzrqqn7WNn3oZdyKqLKwK2XWQ8FLjd");
pub const KAMINO_MAIN_MARKET: Pubkey = pubkey!("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF");
pub const KAMINO_MAIN_USDC_RESERVE: Pubkey =
    pubkey!("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59");
pub const KAMINO_MAIN_PYUSD_RESERVE: Pubkey =
    pubkey!("2gc9Dm1eB6UgVYFBUN9bWks6Kes9PbWSaPaa9DqyvEiN");
pub const KAMINO_PRIME_MARKET: Pubkey = pubkey!("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA");
pub const KAMINO_PRIME_USDC_RESERVE: Pubkey =
    pubkey!("9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu");
pub const MOCK_JUPITER_SOL_TO_USDC: u8 = 1;
pub const MOCK_JUPITER_USDC_TO_PYUSD: u8 = 2;
pub const MOCK_JUPITER_STABLE_EXACT_IN: [u8; 8] = [3, 0, 0, 0, 0, 0, 0, 0];
pub const MOCK_JUPITER_BATCH_EXACT_IN: [u8; 8] = [4, 0, 0, 0, 0, 0, 0, 0];
pub const USDC_DECIMALS: u8 = 6;
pub const PYUSD_DECIMALS: u8 = 6;
pub const KAMINO_COLLATERAL_DECIMALS: u8 = 6;
pub const JUPITER_ROUTER_USDC_PYUSD_DISCRIMINATOR: [u8; 8] = [187, 100, 250, 204, 49, 196, 175, 20];
pub const KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [242, 35, 198, 137, 82, 225, 242, 182];
pub const KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [235, 52, 119, 152, 149, 197, 20, 7];
pub const JUPITER_SWAP_AUTHORITY_SEED: &[u8] = b"jupiter-swap-authority";
pub const KAMINO_RESERVE_LIQUIDITY_AUTHORITY_SEED: &[u8] = b"kamino-reserve-liq-authority";
pub const KAMINO_COLLATERAL_MINT_AUTHORITY_SEED: &[u8] = b"kamino-collateral-mint-authority";
pub const KAMINO_LENDING_MARKET_AUTHORITY_SEED: &[u8] = b"kamino-lending-market-authority";
pub const KAMINO_RESERVE_STATE_LEN: usize = 128;

entrypoint!(process_instruction);

enum JupiterInstruction {
    SolToUsdc {
        amount: u64,
    },
    UsdcToPyusd {
        in_amount: u64,
        out_amount: u64,
    },
    StableExactIn {
        in_amount: u64,
        out_amount: u64,
        input_mint: Pubkey,
        output_mint: Pubkey,
    },
}

enum KaminoInstruction {
    Deposit { amount: u64 },
    Withdraw { amount: u64 },
}

pub fn process_instruction(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    if program_id == &JUPITER_V6_PROGRAM_ID {
        return process_jupiter(program_id, accounts, data);
    }

    if program_id == &KAMINO_LEND_PROGRAM_ID {
        return process_kamino(program_id, accounts, data);
    }

    if program_id == &JUPITER_BATCH_PROGRAM_ID {
        return process_jupiter_batch(accounts, data);
    }

    Err(ProgramError::IncorrectProgramId)
}

fn process_jupiter(program_id: &Pubkey, accounts: &[AccountInfo], data: &[u8]) -> ProgramResult {
    match parse_jupiter_instruction(data)? {
        JupiterInstruction::SolToUsdc { amount } => {
            process_jupiter_sol_to_usdc(program_id, accounts, amount)
        }
        JupiterInstruction::UsdcToPyusd {
            in_amount,
            out_amount,
        } => process_jupiter_usdc_to_pyusd(program_id, accounts, in_amount, out_amount),
        JupiterInstruction::StableExactIn {
            in_amount,
            out_amount,
            input_mint,
            output_mint,
        } => process_jupiter_stable_exact_in(
            program_id,
            accounts,
            in_amount,
            out_amount,
            input_mint,
            output_mint,
        ),
    }
}

fn parse_jupiter_instruction(data: &[u8]) -> Result<JupiterInstruction, ProgramError> {
    if data.len() == 90 && data[..8] == MOCK_JUPITER_STABLE_EXACT_IN {
        return Ok(JupiterInstruction::StableExactIn {
            in_amount: read_u64(&data[8..16])?,
            out_amount: read_u64(&data[16..24])?,
            input_mint: Pubkey::new_from_array(read_pubkey(&data[26..58])?),
            output_mint: Pubkey::new_from_array(read_pubkey(&data[58..90])?),
        });
    }

    if data.len() == 73 {
        let amount = read_u64(&data[1..9])?;
        let input_mint = Pubkey::new_from_array(read_pubkey(&data[9..41])?);
        let output_mint = Pubkey::new_from_array(read_pubkey(&data[41..73])?);

        if data[0] == MOCK_JUPITER_SOL_TO_USDC
            && input_mint == WRAPPED_SOL_MINT
            && output_mint == USDC_MINT
        {
            return Ok(JupiterInstruction::SolToUsdc { amount });
        }

        if data[0] == MOCK_JUPITER_USDC_TO_PYUSD
            && input_mint == USDC_MINT
            && output_mint == PYUSD_MINT
        {
            return Ok(JupiterInstruction::UsdcToPyusd {
                in_amount: amount,
                out_amount: amount,
            });
        }
    }

    if data.len() >= 24 && data[..8] == JUPITER_ROUTER_USDC_PYUSD_DISCRIMINATOR {
        return Ok(JupiterInstruction::UsdcToPyusd {
            in_amount: read_u64(&data[8..16])?,
            out_amount: read_u64(&data[16..24])?,
        });
    }

    Err(ProgramError::InvalidInstructionData)
}

struct BatchSwapRecord {
    direction: u8,
    in_amount: u64,
    out_amount: u64,
}

fn process_jupiter_batch(accounts: &[AccountInfo], data: &[u8]) -> ProgramResult {
    if data.len() < 9 || data[..8] != MOCK_JUPITER_BATCH_EXACT_IN {
        return Err(ProgramError::InvalidInstructionData);
    }
    let swap_count = data[8] as usize;
    let expected_data_len = 9 + swap_count * 17;
    if data.len() != expected_data_len {
        return Err(ProgramError::InvalidInstructionData);
    }
    if accounts.len() != 8 + swap_count * 2 {
        return Err(ProgramError::NotEnoughAccountKeys);
    }

    let batch_signer = &accounts[0];
    let jupiter_program = &accounts[1];
    let token_program = &accounts[2];
    let usdc_mint = &accounts[3];
    let pyusd_mint = &accounts[4];
    let jupiter_usdc_reserve = &accounts[5];
    let jupiter_pyusd_reserve = &accounts[6];
    let jupiter_authority = &accounts[7];

    require_signer(batch_signer)?;
    require_key(jupiter_program, &JUPITER_V6_PROGRAM_ID)?;
    require_key(token_program, &spl_token::id())?;
    require_key(usdc_mint, &USDC_MINT)?;
    require_key(pyusd_mint, &PYUSD_MINT)?;

    for index in 0..swap_count {
        let record_offset = 9 + index * 17;
        let record = BatchSwapRecord {
            direction: data[record_offset],
            in_amount: read_u64(&data[record_offset + 1..record_offset + 9])?,
            out_amount: read_u64(&data[record_offset + 9..record_offset + 17])?,
        };
        let user_usdc = &accounts[8 + index * 2];
        let user_pyusd = &accounts[9 + index * 2];

        let (
            vault_input,
            vault_output,
            input_mint,
            output_mint,
            jupiter_input_reserve,
            jupiter_output_reserve,
        ) = match record.direction {
            0 => (
                user_usdc,
                user_pyusd,
                usdc_mint,
                pyusd_mint,
                jupiter_usdc_reserve,
                jupiter_pyusd_reserve,
            ),
            1 => (
                user_pyusd,
                user_usdc,
                pyusd_mint,
                usdc_mint,
                jupiter_pyusd_reserve,
                jupiter_usdc_reserve,
            ),
            _ => return Err(ProgramError::InvalidInstructionData),
        };

        let mut jupiter_data = Vec::with_capacity(90);
        jupiter_data.extend_from_slice(&MOCK_JUPITER_STABLE_EXACT_IN);
        jupiter_data.extend_from_slice(&record.in_amount.to_le_bytes());
        jupiter_data.extend_from_slice(&record.out_amount.to_le_bytes());
        jupiter_data.extend_from_slice(&50u16.to_le_bytes());
        jupiter_data.extend_from_slice(input_mint.key.as_ref());
        jupiter_data.extend_from_slice(output_mint.key.as_ref());

        let jupiter_ix = Instruction {
            program_id: JUPITER_V6_PROGRAM_ID,
            accounts: vec![
                AccountMeta::new_readonly(*batch_signer.key, true),
                AccountMeta::new(*vault_input.key, false),
                AccountMeta::new(*vault_output.key, false),
                AccountMeta::new_readonly(*input_mint.key, false),
                AccountMeta::new_readonly(*output_mint.key, false),
                AccountMeta::new_readonly(*token_program.key, false),
                AccountMeta::new(*jupiter_input_reserve.key, false),
                AccountMeta::new(*jupiter_output_reserve.key, false),
                AccountMeta::new_readonly(*jupiter_authority.key, false),
            ],
            data: jupiter_data,
        };
        invoke(
            &jupiter_ix,
            &[
                batch_signer.clone(),
                vault_input.clone(),
                vault_output.clone(),
                input_mint.clone(),
                output_mint.clone(),
                token_program.clone(),
                jupiter_input_reserve.clone(),
                jupiter_output_reserve.clone(),
                jupiter_authority.clone(),
                jupiter_program.clone(),
            ],
        )?;
    }

    Ok(())
}

fn process_jupiter_sol_to_usdc(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    amount: u64,
) -> ProgramResult {
    let account_info_iter = &mut accounts.iter();
    let vault = next_account_info(account_info_iter)?;
    let vault_usdc = next_account_info(account_info_iter)?;
    let usdc_mint = next_account_info(account_info_iter)?;
    let jupiter_usdc_reserve = next_account_info(account_info_iter)?;
    let jupiter_authority = next_account_info(account_info_iter)?;
    let token_program = next_account_info(account_info_iter)?;

    require_signer(vault)?;
    require_key(usdc_mint, &USDC_MINT)?;
    require_key(token_program, &spl_token::id())?;

    transfer_checked_signed(
        program_id,
        jupiter_usdc_reserve,
        usdc_mint,
        vault_usdc,
        jupiter_authority,
        token_program,
        amount,
        USDC_DECIMALS,
        &[JUPITER_SWAP_AUTHORITY_SEED],
    )
}

fn process_jupiter_usdc_to_pyusd(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    in_amount: u64,
    out_amount: u64,
) -> ProgramResult {
    let account_info_iter = &mut accounts.iter();
    let vault = next_account_info(account_info_iter)?;
    let vault_usdc = next_account_info(account_info_iter)?;
    let vault_pyusd = next_account_info(account_info_iter)?;
    let usdc_mint = next_account_info(account_info_iter)?;
    let pyusd_mint = next_account_info(account_info_iter)?;
    let token_program = next_account_info(account_info_iter)?;
    let jupiter_usdc_reserve = next_account_info(account_info_iter)?;
    let jupiter_pyusd_reserve = next_account_info(account_info_iter)?;
    let jupiter_authority = next_account_info(account_info_iter)?;

    require_signer(vault)?;
    require_key(usdc_mint, &USDC_MINT)?;
    require_key(pyusd_mint, &PYUSD_MINT)?;
    require_key(token_program, &spl_token::id())?;

    transfer_checked(
        vault_usdc,
        usdc_mint,
        jupiter_usdc_reserve,
        vault,
        token_program,
        in_amount,
        USDC_DECIMALS,
    )?;
    transfer_checked_signed(
        program_id,
        jupiter_pyusd_reserve,
        pyusd_mint,
        vault_pyusd,
        jupiter_authority,
        token_program,
        out_amount,
        PYUSD_DECIMALS,
        &[JUPITER_SWAP_AUTHORITY_SEED],
    )
}

fn process_jupiter_stable_exact_in(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    in_amount: u64,
    out_amount: u64,
    input_mint: Pubkey,
    output_mint: Pubkey,
) -> ProgramResult {
    let account_info_iter = &mut accounts.iter();
    let vault = next_account_info(account_info_iter)?;
    let vault_input = next_account_info(account_info_iter)?;
    let vault_output = next_account_info(account_info_iter)?;
    let input_mint_account = next_account_info(account_info_iter)?;
    let output_mint_account = next_account_info(account_info_iter)?;
    let token_program = next_account_info(account_info_iter)?;
    let jupiter_input_reserve = next_account_info(account_info_iter)?;
    let jupiter_output_reserve = next_account_info(account_info_iter)?;
    let jupiter_authority = next_account_info(account_info_iter)?;

    require_signer(vault)?;
    require_key(input_mint_account, &input_mint)?;
    require_key(output_mint_account, &output_mint)?;
    require_key(token_program, &spl_token::id())?;

    let input_decimals =
        spl_token::state::Mint::unpack(&input_mint_account.data.borrow())?.decimals;
    let output_decimals =
        spl_token::state::Mint::unpack(&output_mint_account.data.borrow())?.decimals;

    transfer_checked(
        vault_input,
        input_mint_account,
        jupiter_input_reserve,
        vault,
        token_program,
        in_amount,
        input_decimals,
    )?;
    transfer_checked_signed(
        program_id,
        jupiter_output_reserve,
        output_mint_account,
        vault_output,
        jupiter_authority,
        token_program,
        out_amount,
        output_decimals,
        &[JUPITER_SWAP_AUTHORITY_SEED],
    )
}

fn process_kamino(program_id: &Pubkey, accounts: &[AccountInfo], data: &[u8]) -> ProgramResult {
    match parse_kamino_instruction(data)? {
        KaminoInstruction::Deposit { amount } => {
            process_kamino_deposit(program_id, accounts, amount)
        }
        KaminoInstruction::Withdraw { amount } => {
            process_kamino_withdraw(program_id, accounts, amount)
        }
    }
}

fn parse_kamino_instruction(data: &[u8]) -> Result<KaminoInstruction, ProgramError> {
    if data.len() != 16 {
        return Err(ProgramError::InvalidInstructionData);
    }

    let amount = read_u64(&data[8..16])?;
    if data[..8] == KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR {
        return Ok(KaminoInstruction::Deposit { amount });
    }
    if data[..8] == KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR {
        return Ok(KaminoInstruction::Withdraw { amount });
    }

    Err(ProgramError::InvalidInstructionData)
}

struct KaminoDepositAccounts<'a, 'info> {
    owner: &'a AccountInfo<'info>,
    reserve: &'a AccountInfo<'info>,
    lending_market: &'a AccountInfo<'info>,
    lending_market_authority: &'a AccountInfo<'info>,
    reserve_liquidity_mint: &'a AccountInfo<'info>,
    reserve_liquidity_supply: &'a AccountInfo<'info>,
    reserve_collateral_mint: &'a AccountInfo<'info>,
    user_source_liquidity: &'a AccountInfo<'info>,
    user_destination_collateral: &'a AccountInfo<'info>,
    collateral_token_program: &'a AccountInfo<'info>,
    liquidity_token_program: &'a AccountInfo<'info>,
    liquidity_decimals: u8,
}

struct KaminoRedeemAccounts<'a, 'info> {
    owner: &'a AccountInfo<'info>,
    lending_market: &'a AccountInfo<'info>,
    reserve: &'a AccountInfo<'info>,
    lending_market_authority: &'a AccountInfo<'info>,
    reserve_liquidity_mint: &'a AccountInfo<'info>,
    reserve_collateral_mint: &'a AccountInfo<'info>,
    reserve_liquidity_supply: &'a AccountInfo<'info>,
    user_source_collateral: &'a AccountInfo<'info>,
    user_destination_liquidity: &'a AccountInfo<'info>,
    collateral_token_program: &'a AccountInfo<'info>,
    liquidity_token_program: &'a AccountInfo<'info>,
    liquidity_decimals: u8,
}

struct KaminoReserveState {
    lending_market: Pubkey,
    liquidity_mint: Pubkey,
    collateral_mint: Pubkey,
    liquidity_supply: Pubkey,
}

fn process_kamino_deposit(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    amount: u64,
) -> ProgramResult {
    let kamino = parse_kamino_deposit_accounts(program_id, accounts)?;
    transfer_checked(
        kamino.user_source_liquidity,
        kamino.reserve_liquidity_mint,
        kamino.reserve_liquidity_supply,
        kamino.owner,
        kamino.liquidity_token_program,
        amount,
        kamino.liquidity_decimals,
    )?;
    mint_to_checked_signed(
        program_id,
        kamino.reserve_collateral_mint,
        kamino.user_destination_collateral,
        kamino.lending_market_authority,
        kamino.collateral_token_program,
        amount,
        KAMINO_COLLATERAL_DECIMALS,
        &[
            KAMINO_LENDING_MARKET_AUTHORITY_SEED,
            kamino.lending_market.key.as_ref(),
        ],
    )
}

fn process_kamino_withdraw(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    amount: u64,
) -> ProgramResult {
    let kamino = parse_kamino_redeem_accounts(program_id, accounts)?;
    burn_checked(
        kamino.user_source_collateral,
        kamino.reserve_collateral_mint,
        kamino.owner,
        kamino.collateral_token_program,
        amount,
        KAMINO_COLLATERAL_DECIMALS,
    )?;
    transfer_checked_signed(
        program_id,
        kamino.reserve_liquidity_supply,
        kamino.reserve_liquidity_mint,
        kamino.user_destination_liquidity,
        kamino.lending_market_authority,
        kamino.liquidity_token_program,
        amount,
        kamino.liquidity_decimals,
        &[
            KAMINO_LENDING_MARKET_AUTHORITY_SEED,
            kamino.lending_market.key.as_ref(),
        ],
    )
}

fn parse_kamino_deposit_accounts<'a, 'info>(
    program_id: &Pubkey,
    accounts: &'a [AccountInfo<'info>],
) -> Result<KaminoDepositAccounts<'a, 'info>, ProgramError> {
    let account_info_iter = &mut accounts.iter();
    let mut kamino = KaminoDepositAccounts {
        owner: next_account_info(account_info_iter)?,
        reserve: next_account_info(account_info_iter)?,
        lending_market: next_account_info(account_info_iter)?,
        lending_market_authority: next_account_info(account_info_iter)?,
        reserve_liquidity_mint: next_account_info(account_info_iter)?,
        reserve_liquidity_supply: next_account_info(account_info_iter)?,
        reserve_collateral_mint: next_account_info(account_info_iter)?,
        user_source_liquidity: next_account_info(account_info_iter)?,
        user_destination_collateral: next_account_info(account_info_iter)?,
        collateral_token_program: next_account_info(account_info_iter)?,
        liquidity_token_program: next_account_info(account_info_iter)?,
        liquidity_decimals: 0,
    };
    let _instruction_sysvar_account = next_account_info(account_info_iter)?;

    require_common_kamino_accounts(
        program_id,
        kamino.owner,
        kamino.lending_market,
        kamino.lending_market_authority,
    )?;
    require_key(kamino.collateral_token_program, &spl_token::id())?;
    require_key(kamino.liquidity_token_program, &spl_token::id())?;

    let reserve = read_kamino_reserve_state(kamino.reserve)?;
    require_key(kamino.lending_market, &reserve.lending_market)?;
    require_key(kamino.reserve_liquidity_mint, &reserve.liquidity_mint)?;
    require_key(kamino.reserve_collateral_mint, &reserve.collateral_mint)?;
    require_key(kamino.reserve_liquidity_supply, &reserve.liquidity_supply)?;

    kamino.liquidity_decimals =
        spl_token::state::Mint::unpack(&kamino.reserve_liquidity_mint.data.borrow())?.decimals;

    Ok(kamino)
}

fn parse_kamino_redeem_accounts<'a, 'info>(
    program_id: &Pubkey,
    accounts: &'a [AccountInfo<'info>],
) -> Result<KaminoRedeemAccounts<'a, 'info>, ProgramError> {
    let account_info_iter = &mut accounts.iter();
    let mut kamino = KaminoRedeemAccounts {
        owner: next_account_info(account_info_iter)?,
        lending_market: next_account_info(account_info_iter)?,
        reserve: next_account_info(account_info_iter)?,
        lending_market_authority: next_account_info(account_info_iter)?,
        reserve_liquidity_mint: next_account_info(account_info_iter)?,
        reserve_collateral_mint: next_account_info(account_info_iter)?,
        reserve_liquidity_supply: next_account_info(account_info_iter)?,
        user_source_collateral: next_account_info(account_info_iter)?,
        user_destination_liquidity: next_account_info(account_info_iter)?,
        collateral_token_program: next_account_info(account_info_iter)?,
        liquidity_token_program: next_account_info(account_info_iter)?,
        liquidity_decimals: 0,
    };
    let _instruction_sysvar_account = next_account_info(account_info_iter)?;

    require_common_kamino_accounts(
        program_id,
        kamino.owner,
        kamino.lending_market,
        kamino.lending_market_authority,
    )?;
    require_key(kamino.collateral_token_program, &spl_token::id())?;
    require_key(kamino.liquidity_token_program, &spl_token::id())?;

    let reserve = read_kamino_reserve_state(kamino.reserve)?;
    require_key(kamino.lending_market, &reserve.lending_market)?;
    require_key(kamino.reserve_liquidity_mint, &reserve.liquidity_mint)?;
    require_key(kamino.reserve_collateral_mint, &reserve.collateral_mint)?;
    require_key(kamino.reserve_liquidity_supply, &reserve.liquidity_supply)?;

    kamino.liquidity_decimals =
        spl_token::state::Mint::unpack(&kamino.reserve_liquidity_mint.data.borrow())?.decimals;

    Ok(kamino)
}

fn require_common_kamino_accounts(
    program_id: &Pubkey,
    owner: &AccountInfo,
    lending_market: &AccountInfo,
    lending_market_authority: &AccountInfo,
) -> ProgramResult {
    require_signer(owner)?;

    let (expected_lending_market_authority, _) = Pubkey::find_program_address(
        &[
            KAMINO_LENDING_MARKET_AUTHORITY_SEED,
            lending_market.key.as_ref(),
        ],
        program_id,
    );
    require_key(lending_market_authority, &expected_lending_market_authority)?;

    Ok(())
}

fn read_kamino_reserve_state(reserve: &AccountInfo) -> Result<KaminoReserveState, ProgramError> {
    let data = reserve.data.borrow();
    if data.len() < KAMINO_RESERVE_STATE_LEN {
        return Err(ProgramError::InvalidAccountData);
    }

    Ok(KaminoReserveState {
        lending_market: Pubkey::new_from_array(read_pubkey(&data[0..32])?),
        liquidity_mint: Pubkey::new_from_array(read_pubkey(&data[32..64])?),
        collateral_mint: Pubkey::new_from_array(read_pubkey(&data[64..96])?),
        liquidity_supply: Pubkey::new_from_array(read_pubkey(&data[96..128])?),
    })
}

fn transfer_checked<'info>(
    source: &AccountInfo<'info>,
    mint: &AccountInfo<'info>,
    destination: &AccountInfo<'info>,
    authority: &AccountInfo<'info>,
    token_program: &AccountInfo<'info>,
    amount: u64,
    decimals: u8,
) -> ProgramResult {
    let ix = spl_token::instruction::transfer_checked(
        token_program.key,
        source.key,
        mint.key,
        destination.key,
        authority.key,
        &[],
        amount,
        decimals,
    )?;
    invoke(
        &ix,
        &[
            source.clone(),
            mint.clone(),
            destination.clone(),
            authority.clone(),
            token_program.clone(),
        ],
    )
}

fn transfer_checked_signed<'info>(
    program_id: &Pubkey,
    source: &AccountInfo<'info>,
    mint: &AccountInfo<'info>,
    destination: &AccountInfo<'info>,
    authority: &AccountInfo<'info>,
    token_program: &AccountInfo<'info>,
    amount: u64,
    decimals: u8,
    seed_parts: &[&[u8]],
) -> ProgramResult {
    let ix = spl_token::instruction::transfer_checked(
        token_program.key,
        source.key,
        mint.key,
        destination.key,
        authority.key,
        &[],
        amount,
        decimals,
    )?;
    let account_infos = [
        source.clone(),
        mint.clone(),
        destination.clone(),
        authority.clone(),
        token_program.clone(),
    ];
    let (_, bump) = Pubkey::find_program_address(seed_parts, program_id);
    let bump_seed = [bump];
    match seed_parts {
        [seed] => invoke_signed(&ix, &account_infos, &[&[*seed, &bump_seed]]),
        [seed_a, seed_b] => invoke_signed(&ix, &account_infos, &[&[*seed_a, *seed_b, &bump_seed]]),
        _ => Err(ProgramError::InvalidSeeds),
    }
}

fn mint_to_checked_signed<'info>(
    program_id: &Pubkey,
    mint: &AccountInfo<'info>,
    destination: &AccountInfo<'info>,
    authority: &AccountInfo<'info>,
    token_program: &AccountInfo<'info>,
    amount: u64,
    decimals: u8,
    seed_parts: &[&[u8]],
) -> ProgramResult {
    let ix = spl_token::instruction::mint_to_checked(
        token_program.key,
        mint.key,
        destination.key,
        authority.key,
        &[],
        amount,
        decimals,
    )?;
    let account_infos = [
        mint.clone(),
        destination.clone(),
        authority.clone(),
        token_program.clone(),
    ];
    let (_, bump) = Pubkey::find_program_address(seed_parts, program_id);
    let bump_seed = [bump];
    match seed_parts {
        [seed] => invoke_signed(&ix, &account_infos, &[&[*seed, &bump_seed]]),
        [seed_a, seed_b] => invoke_signed(&ix, &account_infos, &[&[*seed_a, *seed_b, &bump_seed]]),
        _ => Err(ProgramError::InvalidSeeds),
    }
}

fn burn_checked<'info>(
    source: &AccountInfo<'info>,
    mint: &AccountInfo<'info>,
    authority: &AccountInfo<'info>,
    token_program: &AccountInfo<'info>,
    amount: u64,
    decimals: u8,
) -> ProgramResult {
    let ix = spl_token::instruction::burn_checked(
        token_program.key,
        source.key,
        mint.key,
        authority.key,
        &[],
        amount,
        decimals,
    )?;
    invoke(
        &ix,
        &[
            source.clone(),
            mint.clone(),
            authority.clone(),
            token_program.clone(),
        ],
    )
}

fn require_signer(account: &AccountInfo) -> ProgramResult {
    if !account.is_signer {
        return Err(ProgramError::MissingRequiredSignature);
    }
    Ok(())
}

fn require_key(account: &AccountInfo, expected: &Pubkey) -> ProgramResult {
    if account.key != expected {
        return Err(ProgramError::InvalidArgument);
    }
    Ok(())
}

fn read_u64(data: &[u8]) -> Result<u64, ProgramError> {
    Ok(u64::from_le_bytes(
        data.try_into()
            .map_err(|_| ProgramError::InvalidInstructionData)?,
    ))
}

fn read_pubkey(data: &[u8]) -> Result<[u8; 32], ProgramError> {
    data.try_into()
        .map_err(|_| ProgramError::InvalidInstructionData)
}
