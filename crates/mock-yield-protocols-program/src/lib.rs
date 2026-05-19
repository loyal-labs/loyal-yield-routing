#![allow(unexpected_cfgs)]

use solana_program::{
    account_info::{next_account_info, AccountInfo},
    entrypoint,
    entrypoint::ProgramResult,
    program::{invoke, invoke_signed},
    program_error::ProgramError,
    pubkey,
    pubkey::Pubkey,
};

pub const JUPITER_V6_PROGRAM_ID: Pubkey = pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4");
pub const USDC_MINT: Pubkey = pubkey!("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v");
pub const PYUSD_MINT: Pubkey = pubkey!("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo");
pub const WRAPPED_SOL_MINT: Pubkey = pubkey!("So11111111111111111111111111111111111111112");
pub const KAMINO_LEND_PROGRAM_ID: Pubkey = pubkey!("KvauGMspG5k6rtzrqqn7WNn3oZdyKqLKwK2XWQ8FLjd");
pub const KAMINO_MAIN_MARKET: Pubkey = pubkey!("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF");
pub const KAMINO_MAIN_USDC_RESERVE: Pubkey =
    pubkey!("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59");
pub const KAMINO_PRIME_MARKET: Pubkey = pubkey!("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA");
pub const KAMINO_PRIME_USDC_RESERVE: Pubkey =
    pubkey!("9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu");
pub const MOCK_JUPITER_SOL_TO_USDC: u8 = 1;
pub const MOCK_JUPITER_USDC_TO_PYUSD: u8 = 2;
pub const USDC_DECIMALS: u8 = 6;
pub const PYUSD_DECIMALS: u8 = 6;
pub const KAMINO_COLLATERAL_DECIMALS: u8 = 6;
pub const JUPITER_ROUTER_USDC_PYUSD_DISCRIMINATOR: [u8; 8] = [187, 100, 250, 204, 49, 196, 175, 20];
pub const KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [242, 35, 198, 137, 82, 225, 242, 182];
pub const KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [235, 52, 119, 152, 149, 197, 20, 7];
pub const JUPITER_SWAP_AUTHORITY_SEED: &[u8] = b"jupiter-swap-authority";
pub const KAMINO_RESERVE_LIQUIDITY_AUTHORITY_SEED: &[u8] = b"kamino-reserve-liquidity-authority";
pub const KAMINO_COLLATERAL_MINT_AUTHORITY_SEED: &[u8] = b"kamino-collateral-mint-authority";

entrypoint!(process_instruction);

enum JupiterInstruction {
    SolToUsdc { amount: u64 },
    UsdcToPyusd { in_amount: u64, out_amount: u64 },
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
    }
}

fn parse_jupiter_instruction(data: &[u8]) -> Result<JupiterInstruction, ProgramError> {
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

struct KaminoAccounts<'a, 'info> {
    vault: &'a AccountInfo<'info>,
    reserve: &'a AccountInfo<'info>,
    market: &'a AccountInfo<'info>,
    liquidity_mint: &'a AccountInfo<'info>,
    user_liquidity: &'a AccountInfo<'info>,
    user_collateral: &'a AccountInfo<'info>,
    reserve_liquidity_supply: &'a AccountInfo<'info>,
    collateral_mint: &'a AccountInfo<'info>,
    reserve_liquidity_authority: &'a AccountInfo<'info>,
    collateral_mint_authority: &'a AccountInfo<'info>,
    token_program: &'a AccountInfo<'info>,
}

fn process_kamino_deposit(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    amount: u64,
) -> ProgramResult {
    let kamino = parse_kamino_accounts(program_id, accounts)?;
    transfer_checked(
        kamino.user_liquidity,
        kamino.liquidity_mint,
        kamino.reserve_liquidity_supply,
        kamino.vault,
        kamino.token_program,
        amount,
        USDC_DECIMALS,
    )?;
    mint_to_checked_signed(
        program_id,
        kamino.collateral_mint,
        kamino.user_collateral,
        kamino.collateral_mint_authority,
        kamino.token_program,
        amount,
        KAMINO_COLLATERAL_DECIMALS,
        &[
            KAMINO_COLLATERAL_MINT_AUTHORITY_SEED,
            kamino.reserve.key.as_ref(),
        ],
    )
}

fn process_kamino_withdraw(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    amount: u64,
) -> ProgramResult {
    let kamino = parse_kamino_accounts(program_id, accounts)?;
    burn_checked(
        kamino.user_collateral,
        kamino.collateral_mint,
        kamino.vault,
        kamino.token_program,
        amount,
        KAMINO_COLLATERAL_DECIMALS,
    )?;
    transfer_checked_signed(
        program_id,
        kamino.reserve_liquidity_supply,
        kamino.liquidity_mint,
        kamino.user_liquidity,
        kamino.reserve_liquidity_authority,
        kamino.token_program,
        amount,
        USDC_DECIMALS,
        &[
            KAMINO_RESERVE_LIQUIDITY_AUTHORITY_SEED,
            kamino.reserve.key.as_ref(),
        ],
    )
}

fn parse_kamino_accounts<'a, 'info>(
    program_id: &Pubkey,
    accounts: &'a [AccountInfo<'info>],
) -> Result<KaminoAccounts<'a, 'info>, ProgramError> {
    let account_info_iter = &mut accounts.iter();
    let kamino = KaminoAccounts {
        vault: next_account_info(account_info_iter)?,
        reserve: next_account_info(account_info_iter)?,
        market: next_account_info(account_info_iter)?,
        liquidity_mint: next_account_info(account_info_iter)?,
        user_liquidity: next_account_info(account_info_iter)?,
        user_collateral: next_account_info(account_info_iter)?,
        reserve_liquidity_supply: next_account_info(account_info_iter)?,
        collateral_mint: next_account_info(account_info_iter)?,
        reserve_liquidity_authority: next_account_info(account_info_iter)?,
        collateral_mint_authority: next_account_info(account_info_iter)?,
        token_program: next_account_info(account_info_iter)?,
    };

    require_signer(kamino.vault)?;
    require_key(kamino.liquidity_mint, &USDC_MINT)?;
    require_key(kamino.token_program, &spl_token::id())?;

    let is_main_usdc =
        kamino.market.key == &KAMINO_MAIN_MARKET && kamino.reserve.key == &KAMINO_MAIN_USDC_RESERVE;
    let is_prime_usdc = kamino.market.key == &KAMINO_PRIME_MARKET
        && kamino.reserve.key == &KAMINO_PRIME_USDC_RESERVE;
    if !is_main_usdc && !is_prime_usdc {
        return Err(ProgramError::InvalidArgument);
    }

    let (reserve_liquidity_authority, _) = Pubkey::find_program_address(
        &[
            KAMINO_RESERVE_LIQUIDITY_AUTHORITY_SEED,
            kamino.reserve.key.as_ref(),
        ],
        program_id,
    );
    require_key(
        kamino.reserve_liquidity_authority,
        &reserve_liquidity_authority,
    )?;

    let (collateral_mint_authority, _) = Pubkey::find_program_address(
        &[
            KAMINO_COLLATERAL_MINT_AUTHORITY_SEED,
            kamino.reserve.key.as_ref(),
        ],
        program_id,
    );
    require_key(kamino.collateral_mint_authority, &collateral_mint_authority)?;

    Ok(kamino)
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
